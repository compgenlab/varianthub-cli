package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// waitFor polls until cond() or the deadline, failing the test on timeout.
func waitFor(t *testing.T, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting for condition")
}

func openTestQueue(t *testing.T) *Queue {
	t.Helper()
	q, err := OpenQueue(context.Background(), filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatalf("OpenQueue: %v", err)
	}
	t.Cleanup(func() { q.Close() })
	return q
}

func TestEnqueueProcessDone(t *testing.T) {
	q := openTestQueue(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A runner that echoes the input body back as the "result" and reports 1 variant.
	var gotInput string
	q.StartWorkers(ctx, 2, func(_ context.Context, job Job, input []byte) ([]byte, int, error) {
		gotInput = string(input)
		return []byte(`["` + string(input) + `"]`), 1, nil
	})

	id, err := q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "2026-07", Selection: "clinvar_sig", ClientIP: "1.2.3.4", Body: []byte("chr1:100:A:G")})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}

	waitFor(t, 2*time.Second, func() bool {
		job, ok, _ := q.Get(ctx, id)
		return ok && job.Status == StatusDone
	})

	job, _, _ := q.Get(ctx, id)
	if job.NVariants != 1 {
		t.Errorf("n_variants = %d, want 1", job.NVariants)
	}
	if job.Selection != "clinvar_sig" {
		t.Errorf("selection = %q, want clinvar_sig", job.Selection)
	}
	if job.FinishedAt == 0 || job.StartedAt == 0 {
		t.Errorf("started_at/finished_at not set: %+v", job)
	}
	if gotInput != "chr1:100:A:G" {
		t.Errorf("runner saw input %q", gotInput)
	}
	result, ok, err := q.Result(ctx, id)
	if err != nil || !ok {
		t.Fatalf("Result: ok=%v err=%v", ok, err)
	}
	if string(result) != `["chr1:100:A:G"]` {
		t.Errorf("result = %s", result)
	}
}

func TestRunnerErrorMarksJobFailed(t *testing.T) {
	q := openTestQueue(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	q.StartWorkers(ctx, 1, func(_ context.Context, _ Job, _ []byte) ([]byte, int, error) {
		return nil, 0, context.DeadlineExceeded
	})
	id, _ := q.Enqueue(ctx, NewJob{Kind: KindLocus, Snapshot: "s", Body: []byte("bad")})

	waitFor(t, 2*time.Second, func() bool {
		job, ok, _ := q.Get(ctx, id)
		return ok && job.Status == StatusError
	})
	job, _, _ := q.Get(ctx, id)
	if job.Error == "" {
		t.Errorf("expected an error message on the failed job")
	}
}

func TestCrashRecoveryRequeuesRunning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "q.db")

	q1, err := OpenQueue(context.Background(), path)
	if err != nil {
		t.Fatalf("OpenQueue: %v", err)
	}
	// Enqueue then forcibly mark it running, simulating a crash mid-job.
	id, _ := q1.Enqueue(context.Background(), NewJob{Kind: KindLocus, Snapshot: "s", Body: []byte("x")})
	if _, err := q1.db.Exec(`UPDATE job SET status=? WHERE id=?`, StatusRunning, id); err != nil {
		t.Fatalf("force running: %v", err)
	}
	q1.Close()

	// Reopen: the running job must be reset to queued.
	q2, err := OpenQueue(context.Background(), path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer q2.Close()
	job, ok, _ := q2.Get(context.Background(), id)
	if !ok || job.Status != StatusQueued {
		t.Fatalf("after recovery status = %q (ok=%v), want queued", job.Status, ok)
	}
}
