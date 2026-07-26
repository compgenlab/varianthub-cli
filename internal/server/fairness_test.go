package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestWaitForReturnsOnDone(t *testing.T) {
	q := openTestQueue(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	q.StartWorkers(ctx, 1, func(_ context.Context, _ Job, _ []byte) ([]byte, int, error) {
		return []byte("[]"), 1, nil
	})
	id, _ := q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "ip", Body: []byte("x")})
	job, ok, err := q.WaitFor(ctx, id, 2*time.Second)
	if err != nil || !ok {
		t.Fatalf("WaitFor ok=%v err=%v", ok, err)
	}
	if job.Status != StatusDone {
		t.Errorf("status = %q, want done", job.Status)
	}
}

func TestWaitForTimeoutReturnsCurrent(t *testing.T) {
	q := openTestQueue(t) // no workers → job stays queued
	id, _ := q.Enqueue(context.Background(), NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "ip", Body: []byte("x")})
	start := time.Now()
	job, ok, err := q.WaitFor(context.Background(), id, 200*time.Millisecond)
	if err != nil || !ok {
		t.Fatalf("WaitFor ok=%v err=%v", ok, err)
	}
	if job.Status != StatusQueued {
		t.Errorf("status = %q, want queued (timed out)", job.Status)
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Errorf("returned too early (%s) — should have waited ~200ms", elapsed)
	}
}

func TestParseWaitCaps(t *testing.T) {
	s := testServer(t) // submit_wait unset → 10s cap
	mk := func(v string) *http.Request { return httptest.NewRequest("POST", "/ui/submit?wait="+v, nil) }
	cases := map[string]time.Duration{
		"5":   5 * time.Second,
		"3s":  3 * time.Second,
		"100": 10 * time.Second, // capped
		"":    0,
		"-4":  0,
	}
	for in, want := range cases {
		if got := s.parseWait(mk(in)); got != want {
			t.Errorf("parseWait(wait=%q) = %s, want %s", in, got, want)
		}
	}
	if got := s.parseWait(httptest.NewRequest("POST", "/ui/submit", nil)); got != 0 {
		t.Errorf("parseWait(no wait) = %s, want 0", got)
	}
}

func TestClientIPTrustedProxy(t *testing.T) {
	trusted := parseCIDRs([]string{"10.0.0.0/8"})
	// Peer is the trusted proxy → trust the rightmost X-Forwarded-For entry.
	r := httptest.NewRequest("POST", "/ui/submit", nil)
	r.RemoteAddr = "10.1.2.3:5555"
	r.Header.Set("X-Forwarded-For", "9.9.9.9, 203.0.113.7")
	if got := clientIP(r, trusted); got != "203.0.113.7" {
		t.Errorf("trusted proxy: clientIP = %q, want 203.0.113.7", got)
	}

	// Peer is NOT trusted → ignore the (possibly spoofed) header, use the peer.
	r2 := httptest.NewRequest("POST", "/ui/submit", nil)
	r2.RemoteAddr = "8.8.8.8:1234"
	r2.Header.Set("X-Forwarded-For", "1.1.1.1")
	if got := clientIP(r2, trusted); got != "8.8.8.8" {
		t.Errorf("untrusted peer: clientIP = %q, want 8.8.8.8", got)
	}
}

func TestIPLimiterBurstThenThrottle(t *testing.T) {
	l := newIPLimiter(60, 3) // 1/sec, burst 3
	for i := 0; i < 3; i++ {
		if !l.allow("1.2.3.4") {
			t.Fatalf("request %d should be allowed within burst", i)
		}
	}
	if l.allow("1.2.3.4") {
		t.Errorf("4th request should be throttled")
	}
	// A different IP has its own bucket.
	if !l.allow("5.6.7.8") {
		t.Errorf("distinct IP should be allowed")
	}
	// Disabled limiter allows everything.
	off := newIPLimiter(0, 0)
	for i := 0; i < 100; i++ {
		if !off.allow("x") {
			t.Fatalf("disabled limiter should allow all")
		}
	}
}

// monotonicNow returns a nowFn producing strictly-increasing unix seconds, so
// created_at ordering is deterministic in tests.
func monotonicNow() func() int64 {
	var n int64
	return func() int64 { n++; return n }
}

func TestQueueListAndGC(t *testing.T) {
	q, err := OpenQueue(context.Background(), filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	q.nowFn = monotonicNow()
	ctx := context.Background()

	// Two terminal (done) jobs and one still-queued job.
	oldID, _ := q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "1.1.1.1", Body: []byte("a")})
	newID, _ := q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "2.2.2.2", Body: []byte("b")})
	queuedID, _ := q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "3.3.3.3", Body: []byte("c")})
	// Mark the first two finished at t=10 and t=100.
	if _, err := q.db.Exec(`UPDATE job SET status=?, finished_at=? WHERE id=?`, StatusDone, 10, oldID); err != nil {
		t.Fatal(err)
	}
	if _, err := q.db.Exec(`UPDATE job SET status=?, finished_at=? WHERE id=?`, StatusDone, 100, newID); err != nil {
		t.Fatal(err)
	}

	// List by status.
	done, err := q.List(ctx, JobFilter{Status: StatusDone}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(done) != 2 {
		t.Fatalf("List(done) = %d jobs, want 2", len(done))
	}
	if q, _ := q.List(ctx, JobFilter{Status: StatusQueued}, 50, 0); len(q) != 1 {
		t.Fatalf("List(queued) = %d, want 1", len(q))
	}

	// GC with cutoff=50 removes only the old done job; leaves the recent done and
	// the still-queued job untouched.
	n, err := q.DeleteOlderThan(ctx, 50)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("DeleteOlderThan removed %d, want 1", n)
	}
	if _, ok, _ := q.Get(ctx, oldID); ok {
		t.Errorf("old done job should be gone")
	}
	if _, ok, _ := q.Get(ctx, newID); !ok {
		t.Errorf("recent done job should remain")
	}
	if _, ok, _ := q.Get(ctx, queuedID); !ok {
		t.Errorf("queued job must never be GC'd")
	}
	// Input + result blobs of the GC'd job are gone too.
	if _, ok, _ := q.Result(ctx, oldID); ok {
		t.Errorf("GC'd job result blob should be gone")
	}
}

func TestFairClaimRoundRobin(t *testing.T) {
	q, err := OpenQueue(context.Background(), filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	q.nowFn = monotonicNow()
	q.SetMaxJobsPerIP(0) // unlimited running, so only fairness ordering is exercised
	ctx := context.Background()

	// IP A enqueues 3 jobs before IP B enqueues 3.
	for i := 0; i < 3; i++ {
		q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "10.0.0.1", Body: []byte("a")})
	}
	for i := 0; i < 3; i++ {
		q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "10.0.0.2", Body: []byte("b")})
	}

	// Claim (without completing) — jobs stay running, deprioritizing the busier IP.
	var seq []string
	for i := 0; i < 6; i++ {
		job, _, ok, err := q.claimNext(ctx)
		if err != nil || !ok {
			t.Fatalf("claim %d: ok=%v err=%v", i, ok, err)
		}
		seq = append(seq, job.ClientIP)
	}
	// Despite A being enqueued entirely first, fair scheduling must interleave.
	alt := 0
	for i := 1; i < len(seq); i++ {
		if seq[i] != seq[i-1] {
			alt++
		}
	}
	if alt < 4 {
		t.Errorf("expected round-robin interleaving across IPs, got sequence %v", seq)
	}
}

func TestFairClaimPerIPCap(t *testing.T) {
	q, err := OpenQueue(context.Background(), filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	q.nowFn = monotonicNow()
	q.SetMaxJobsPerIP(1)
	ctx := context.Background()

	// Two IPs, two jobs each; cap of 1 running per IP.
	q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "10.0.0.1", Body: []byte("a")})
	q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "10.0.0.1", Body: []byte("a")})
	q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "10.0.0.2", Body: []byte("b")})
	q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", ClientIP: "10.0.0.2", Body: []byte("b")})

	// First two claims: one per IP. Third: both IPs at cap → nothing claimable.
	if _, _, ok, _ := q.claimNext(ctx); !ok {
		t.Fatal("claim 1 should succeed")
	}
	if _, _, ok, _ := q.claimNext(ctx); !ok {
		t.Fatal("claim 2 should succeed")
	}
	if _, _, ok, _ := q.claimNext(ctx); ok {
		t.Errorf("claim 3 should be blocked — both IPs at their per-IP cap")
	}
}

func TestOpsEndpoints(t *testing.T) {
	s := testServer(t)

	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz status = %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/version", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/version status = %d", rec.Code)
	}

	// /v1/jobs requires a token.
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/jobs", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/v1/jobs without token = %d, want 401", rec.Code)
	}

	tok, _ := MintToken(s.cfg.Server.MasterKey, 0)
	req := httptest.NewRequest("GET", "/v1/jobs", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("/v1/jobs with token = %d, want 200", rec.Code)
	}
}

func TestListScopedBySession(t *testing.T) {
	q, err := OpenQueue(context.Background(), filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	q.nowFn = monotonicNow()
	ctx := context.Background()

	// Two sessions submit jobs.
	q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", Session: "alice", Label: "chr1:1:A:G", Body: []byte("x")})
	q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", Session: "alice", Label: "chr2:2:C:T", Body: []byte("y")})
	q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", Session: "bob", Label: "chr3:3:G:A", Body: []byte("z")})

	alice, err := q.List(ctx, JobFilter{Session: "alice"}, 50, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(alice) != 2 {
		t.Fatalf("alice sees %d jobs, want 2 (only her own)", len(alice))
	}
	for _, j := range alice {
		if j.Session != "alice" {
			t.Errorf("alice's list leaked session %q", j.Session)
		}
		if j.Label == "" {
			t.Errorf("job %s missing label for history display", j.ID)
		}
	}
	if all, _ := q.List(ctx, JobFilter{}, 50, 0); len(all) != 3 {
		t.Errorf("unscoped list = %d, want 3 (admin sees all)", len(all))
	}
}

func TestUIListScopedByCookie(t *testing.T) {
	s := testServer(t)
	// Two jobs on session "s1", one on "s2".
	s.queue.Enqueue(context.Background(), NewJob{Kind: KindLocus, Snapshot: s.snap.Name, Session: "s1", Body: []byte("a")})
	s.queue.Enqueue(context.Background(), NewJob{Kind: KindLocus, Snapshot: s.snap.Name, Session: "s1", Body: []byte("b")})
	s.queue.Enqueue(context.Background(), NewJob{Kind: KindLocus, Snapshot: s.snap.Name, Session: "s2", Body: []byte("c")})

	req := httptest.NewRequest("GET", "/ui/jobs", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: "s1"})
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Jobs   []Job `json:"jobs"`
		Scoped bool  `json:"scoped"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Scoped {
		t.Errorf("unauthenticated /ui/jobs should be scoped")
	}
	if len(resp.Jobs) != 2 {
		t.Errorf("session s1 sees %d jobs, want 2 (not s2's)", len(resp.Jobs))
	}
}

func TestRequireTokenFalseOpensV1(t *testing.T) {
	s := testServer(t)
	no := false
	s.cfg.Server.RequireToken = &no

	// /v1 is reachable with no Authorization header.
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/jobs", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/v1/jobs with require_token=false = %d, want 200 (open)", rec.Code)
	}
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/annotations", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/v1/annotations with require_token=false = %d, want 200", rec.Code)
	}

	// Default (nil) still requires a token.
	s.cfg.Server.RequireToken = nil
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/jobs", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("/v1/jobs default = %d, want 401", rec.Code)
	}
}

func TestUIDisabled(t *testing.T) {
	s := testServer(t)
	no := false
	s.cfg.Server.UIEnabled = &no

	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("form page with ui_enabled=false = %d, want 404", rec.Code)
	}
	// /healthz still works with the UI off.
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("/healthz with ui off = %d, want 200", rec.Code)
	}
}

func TestThrottle429(t *testing.T) {
	s := testServer(t)
	s.limiter = newIPLimiter(60, 1) // burst 1

	post := func() int {
		req := httptest.NewRequest("POST", "/ui/submit", nil)
		req.RemoteAddr = "9.9.9.9:1000"
		rec := httptest.NewRecorder()
		s.throttle(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)
		return rec.Code
	}
	if code := post(); code != http.StatusOK {
		t.Fatalf("first request = %d, want 200", code)
	}
	if code := post(); code != http.StatusTooManyRequests {
		t.Errorf("second request = %d, want 429", code)
	}
}
