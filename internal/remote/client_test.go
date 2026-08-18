package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func testClient(t *testing.T, h http.Handler) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(Credentials{Server: srv.URL, Token: "tok-123"})
}

func TestSubmitSendsALocusListAndReturnsTheJobID(t *testing.T) {
	var gotPath, gotAuth string
	var body map[string]any
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		json.NewDecoder(r.Body).Decode(&body)
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"job_id": "j-1"})
	}))

	id, err := c.Submit(context.Background(), SubmitRequest{
		Variants: []string{"chr1:100:A:T", "chr2:200:G:C"},
		Snapshot: "gnomad-4.1", Annotations: "all",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if id != "j-1" {
		t.Errorf("job id = %q, want j-1", id)
	}
	if gotPath != "/api/v1/annotate" {
		t.Errorf("path = %q", gotPath)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if body["snapshot"] != "gnomad-4.1" || body["annotations"] != "all" {
		t.Errorf("body lost its terms: %v", body)
	}
	vs, _ := body["variants"].([]any)
	if len(vs) != 2 {
		t.Errorf("variants = %v, want 2", body["variants"])
	}
}

// A VCF goes as multipart, and the fields have to arrive before the file: the
// server reads the parts in order as they stream, so a field sent after the
// file is one it has already had to decide without.
func TestSubmitUploadsAVCFWithItsFieldsFirst(t *testing.T) {
	var order []string
	var fileBody, fileName string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Errorf("content type: %v", err)
			return
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				break
			}
			order = append(order, p.FormName())
			if p.FormName() == "vcf" {
				fileName = p.FileName()
				b, _ := io.ReadAll(p)
				fileBody = string(b)
			}
			p.Close()
		}
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(map[string]string{"job_id": "j-vcf"})
	}))

	dir := t.TempDir()
	path := filepath.Join(dir, "sample.vcf")
	if err := os.WriteFile(path, []byte("##fileformat=VCFv4.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	id, err := c.Submit(context.Background(), SubmitRequest{
		VCFPath: path, Snapshot: "s", Annotations: "all",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if id != "j-vcf" {
		t.Errorf("job id = %q", id)
	}
	if fileName != "sample.vcf" {
		t.Errorf("filename = %q, want sample.vcf", fileName)
	}
	if !strings.HasPrefix(fileBody, "##fileformat=") {
		t.Errorf("the file did not arrive: %q", fileBody)
	}
	if len(order) == 0 || order[len(order)-1] != "vcf" {
		t.Errorf("parts arrived as %v; the file must come last", order)
	}
}

// The claim the Fetch comment makes, checked rather than asserted: a 302 to
// another host is followed, and the VariantHub bearer token is NOT carried
// across. The presigned URL has its own authority in the query string, and
// forwarding our token would hand a storage provider a credential for this
// account.
func TestFetchFollowsARedirectWithoutForwardingTheToken(t *testing.T) {
	var storageAuth string
	var storageSeen bool
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		storageSeen = true
		storageAuth = r.Header.Get("Authorization")
		io.WriteString(w, "BGZF-ish result bytes")
	}))
	defer storage.Close()

	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, storage.URL+"/obj?X-Amz-Signature=abc", http.StatusFound)
	}))

	var buf bytes.Buffer
	n, err := c.Fetch(context.Background(), "j-1", "vcf", &buf)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if !storageSeen {
		t.Fatal("the redirect was not followed, so a presigned download would never arrive")
	}
	if storageAuth != "" {
		t.Errorf("the VariantHub token was forwarded to storage as %q; a presigned "+
			"URL carries its own authority and this hands a third party a "+
			"credential for the account", storageAuth)
	}
	if got := buf.String(); got != "BGZF-ish result bytes" || n != int64(len(got)) {
		t.Errorf("fetched %q (%d bytes)", got, n)
	}
}

func TestFetchAsksForTheFormat(t *testing.T) {
	var gotQuery string
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		io.WriteString(w, "x")
	}))
	if _, err := c.Fetch(context.Background(), "j-1", "tsv", io.Discard); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "format=tsv" {
		t.Errorf("query = %q, want format=tsv", gotQuery)
	}
}

// The server promises its error bodies are safe to show, and they are the only
// thing that explains a refusal — a bare status line sends somebody to the logs
// of a machine they may not have.
func TestAServerErrorIsReportedInItsOwnWords(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "snapshot \"nope\" is not published"})
	}))
	_, err := c.Submit(context.Background(), SubmitRequest{Variants: []string{"chr1:1:A:T"}})
	if err == nil {
		t.Fatal("a 400 was reported as success")
	}
	if !strings.Contains(err.Error(), "not published") {
		t.Errorf("the server's explanation was dropped: %v", err)
	}
}

// A rejected token is the one failure worth naming, because the fix is a
// command rather than a puzzle.
func TestARejectedTokenSaysWhatToDo(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "unknown token"})
	}))
	err := c.Ping(context.Background())
	if err == nil {
		t.Fatal("a 401 was reported as success")
	}
	if !strings.Contains(err.Error(), "varhub login") {
		t.Errorf("the error does not say how to fix it: %v", err)
	}
}

func TestStatusDecodesWhatTheCallerActsOn(t *testing.T) {
	c := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/jobs/j-1" {
			t.Errorf("path = %q", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"job_id": "j-1", "status": "error", "kind": "vcf",
			"n_variants": 42, "chunks_total": 26, "chunks_done": 25,
			"chunks_failed": 1, "error": "the reference was missing",
			"purged_at": 1700000000, "runner": "local",
		})
	}))
	j, err := c.Status(context.Background(), "j-1")
	if err != nil {
		t.Fatal(err)
	}
	if j.Status != "error" || j.NVariants != 42 || j.Failed != 1 {
		t.Errorf("decoded %+v", j)
	}
	if j.PurgedAt == 0 || j.Runner != "local" {
		t.Errorf("purged_at and runner did not decode: %+v", j)
	}
	if !j.Terminal() {
		t.Error("a failed job should be terminal")
	}
}
