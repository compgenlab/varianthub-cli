package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/cganno/internal/config"
)

// testServer builds a Server over a builtins-only snapshot (no data files to
// download) and an open queue, so handlers can be exercised without real sources.
func testServer(t *testing.T) *Server {
	t.Helper()
	snap := &config.Snapshot{
		Name:     "2026-07",
		Assembly: "GRCh38",
		Defaults: []string{"auto_id"},
		Sources: []config.Source{{
			Name: "builtins", Version: "1", Type: "builtin",
			Annotations: []config.Annotation{
				{Builtin: "auto_id", Name: "auto_id", Description: "variant id"},
				{Builtin: "tstv", Name: "tstv"},
			},
		}},
	}
	snap.Normalize()

	q, err := OpenQueue(context.Background(), filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatalf("OpenQueue: %v", err)
	}
	t.Cleanup(func() { q.Close() })

	cfg := &config.Config{}
	cfg.Server.MasterKey = "test-key"
	return New(cfg, snap, nil, q, "test")
}

func TestHandleAnnotationsDiscovery(t *testing.T) {
	s := testServer(t)
	req := httptest.NewRequest("GET", "/ui/annotations", nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp annotationsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Snapshot != "2026-07" || len(resp.Sources) != 1 {
		t.Fatalf("unexpected discovery payload: %+v", resp)
	}
	src := resp.Sources[0]
	if src.Type != "builtin" || len(src.Annotations) != 2 {
		t.Fatalf("source = %+v", src)
	}
	var sawDefault bool
	for _, a := range src.Annotations {
		if a.Name == "auto_id" {
			sawDefault = a.Default
		}
	}
	if !sawDefault {
		t.Errorf("auto_id should be marked default")
	}
}

func TestV1RequiresToken(t *testing.T) {
	s := testServer(t)
	// No token → 401.
	req := httptest.NewRequest("GET", "/v1/annotations", nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-token status = %d, want 401", rec.Code)
	}
	// Valid token → 200.
	tok, _ := MintToken(s.cfg.Server.MasterKey, 0)
	req = httptest.NewRequest("GET", "/v1/annotations", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("with-token status = %d, want 200", rec.Code)
	}
}

func TestFormServedOpen(t *testing.T) {
	s := testServer(t)
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<form") {
		t.Fatalf("form page not served: status=%d", rec.Code)
	}
}

func TestSubmitValidatesLocusAndSelection(t *testing.T) {
	s := testServer(t)
	// Bad locus → 400.
	rec := postJSON(t, s, "/ui/submit", `{"locus":"not-a-locus"}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad locus status = %d, want 400", rec.Code)
	}
	// Unknown annotation → 400.
	rec = postJSON(t, s, "/ui/submit", `{"locus":"chr1:100:A:G","annotations":["nope"]}`)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("unknown annotation status = %d, want 400", rec.Code)
	}
	// Valid → 202 with a job_id.
	rec = postJSON(t, s, "/ui/submit", `{"locus":"chr1:100:A:G","annotations":["auto_id"]}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid submit status = %d, want 202", rec.Code)
	}
	var resp map[string]string
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["job_id"] == "" {
		t.Errorf("missing job_id in response")
	}
}

func TestSelectionFieldShapes(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{`"all"`, "all"},
		{`["a","b"]`, "a,b"},
		{`"a, b ,c"`, "a,b,c"},
		{`""`, ""},
	}
	for _, c := range cases {
		var sf selectionField
		if err := sf.UnmarshalJSON([]byte(c.in)); err != nil {
			t.Fatalf("UnmarshalJSON(%s): %v", c.in, err)
		}
		if got := sf.selection(); got != c.want {
			t.Errorf("selection(%s) = %q, want %q", c.in, got, c.want)
		}
	}
}

func postJSON(t *testing.T, s *Server, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, req)
	return rec
}
