package fetch

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/objstore"
)

// stubStore is an in-memory object store. It covers the staging and publish
// logic without an endpoint; the S3 implementation itself is exercised against
// a real gateway in internal/objstore.
type stubStore struct {
	mu      sync.Mutex
	objects map[string][]byte
	meta    map[string]string
	puts    int
	failPut bool
}

func newStubStore() *stubStore {
	return &stubStore{objects: map[string][]byte{}, meta: map[string]string{}}
}

func (s *stubStore) Stat(_ context.Context, ref objstore.Ref) (objstore.Object, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.objects[ref.String()]
	if !ok {
		return objstore.Object{}, false, nil
	}
	return objstore.Object{Size: int64(len(b)), Checksum: s.meta[ref.String()]}, true, nil
}

func (s *stubStore) PutFile(_ context.Context, ref objstore.Ref, localPath, checksum string) error {
	b, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failPut {
		return os.ErrPermission
	}
	s.puts++
	s.objects[ref.String()] = b
	if checksum != "" {
		s.meta[ref.String()] = checksum
	}
	return nil
}

func (s *stubStore) CheckBucket(_ context.Context, _ string) error { return nil }

// Download serves an object back, so the image cache path can be exercised
// without an endpoint.
func (s *stubStore) Download(ctx context.Context, ref objstore.Ref, w io.Writer) error {
	s.mu.Lock()
	body, ok := s.objects[ref.String()]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("no such object: %s", ref)
	}
	_, err := w.Write(body)
	return err
}

func (s *stubStore) Remove(_ context.Context, ref objstore.Ref) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.objects, ref.String())
	return nil
}

func (s *stubStore) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.objects))
	for k := range s.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// useStub installs a fresh stub store for the duration of a test.
func useStub(t *testing.T) *stubStore {
	t.Helper()
	s := newStubStore()
	prev := storeVal
	SetStore(s)
	t.Cleanup(func() { SetStore(prev) })
	return s
}

// countingServer serves a directory and records how many requests it saw, so a
// test can assert that a cached source was not fetched again.
type countingServer struct {
	*httptest.Server
	mu sync.Mutex
	n  int
}

func newCountingServer(dir string) *countingServer {
	cs := &countingServer{}
	fs := http.FileServer(http.Dir(dir))
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		cs.n++
		cs.mu.Unlock()
		fs.ServeHTTP(w, r)
	}))
	return cs
}

func (cs *countingServer) hits() int {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return cs.n
}

// remoteCfg builds a config rooted in a temp dir. The base matters: without it
// relative paths — the annotations tree a location overlay is written into —
// resolve against the package directory, and the test writes into the repo.
func remoteCfg(t *testing.T, cache string) *config.Config {
	t.Helper()
	c := &config.Config{CacheDir: cache, DataDir: t.TempDir(), AnnotationsDir: "annotations"}
	c.SetBaseDir(t.TempDir())
	return c
}

func TestRemoteCacheIsRecognised(t *testing.T) {
	cfg := remoteCfg(t, "s3://bucket/prefix")
	if got := cfg.CacheDirAbs(); got != "s3://bucket/prefix" {
		t.Fatalf("CacheDirAbs mangled the locator: %q", got)
	}
	src := config.Source{Name: "clinvar", Version: "2026-01", URL: "https://x/clinvar.vcf.gz"}
	want := "s3://bucket/prefix/clinvar/2026-01/clinvar.vcf.gz"
	if got := cfg.ResolveSourcePath(src); got != want {
		t.Errorf("ResolveSourcePath = %q, want %q", got, want)
	}
}

// The whole point of staging: only a verified data+index pair is published.
func TestRemoteFetchPublishesVerifiedPair(t *testing.T) {
	store := useStub(t)
	srvDir := t.TempDir()
	writeVCFGz(t, filepath.Join(srvDir, "src.vcf.gz"), false) // no published index
	ts := httptest.NewServer(http.FileServer(http.Dir(srvDir)))
	defer ts.Close()

	cfg := remoteCfg(t, "s3://bucket/prefix")
	src := config.Source{Name: "src", Version: "1", Format: "vcf", URL: ts.URL + "/src.vcf.gz"}
	if _, err := Source(context.Background(), cfg, src, false, false); err != nil {
		t.Fatalf("Source: %v", err)
	}

	got := store.keys()
	want := []string{
		"s3://bucket/prefix/src/1/src.vcf.gz",
		"s3://bucket/prefix/src/1/src.vcf.gz.tbi",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("published %v, want %v", got, want)
	}
	// The uploaded data must be exactly what the server served.
	orig, _ := os.ReadFile(filepath.Join(srvDir, "src.vcf.gz"))
	if string(store.objects["s3://bucket/prefix/src/1/src.vcf.gz"]) != string(orig) {
		t.Error("uploaded bytes differ from the downloaded file")
	}
}

// A complete source must not be fetched or uploaded again.
func TestRemoteFetchSkipsWhenComplete(t *testing.T) {
	store := useStub(t)
	srvDir := t.TempDir()
	writeVCFGz(t, filepath.Join(srvDir, "src.vcf.gz"), false)
	cs := newCountingServer(srvDir)
	defer cs.Close()

	cfg := remoteCfg(t, "s3://bucket/prefix")
	src := config.Source{Name: "src", Version: "1", Format: "vcf", URL: cs.URL + "/src.vcf.gz"}
	ctx := context.Background()
	if _, err := Source(ctx, cfg, src, false, false); err != nil {
		t.Fatal(err)
	}
	firstHits, firstPuts := cs.hits(), store.puts
	if firstHits == 0 || firstPuts == 0 {
		t.Fatalf("first run did nothing (hits=%d puts=%d)", firstHits, firstPuts)
	}

	if _, err := Source(ctx, cfg, src, false, false); err != nil {
		t.Fatal(err)
	}
	if cs.hits() != firstHits {
		t.Errorf("re-run made %d extra HTTP requests; want none", cs.hits()-firstHits)
	}
	if store.puts != firstPuts {
		t.Errorf("re-run made %d extra uploads; want none", store.puts-firstPuts)
	}
}

// Data present with no index is the shape an interrupted run leaves behind. It
// must not read as complete.
func TestRemoteFetchRebuildsWhenIndexMissing(t *testing.T) {
	store := useStub(t)
	srvDir := t.TempDir()
	writeVCFGz(t, filepath.Join(srvDir, "src.vcf.gz"), false)
	ts := httptest.NewServer(http.FileServer(http.Dir(srvDir)))
	defer ts.Close()

	cfg := remoteCfg(t, "s3://bucket/prefix")
	src := config.Source{Name: "src", Version: "1", Format: "vcf", URL: ts.URL + "/src.vcf.gz"}
	ctx := context.Background()
	if _, err := Source(ctx, cfg, src, false, false); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	delete(store.objects, "s3://bucket/prefix/src/1/src.vcf.gz.tbi")
	store.mu.Unlock()

	if _, err := Source(ctx, cfg, src, false, false); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.objects["s3://bucket/prefix/src/1/src.vcf.gz.tbi"]; !ok {
		t.Error("a data object with no index was treated as complete; the index was not rebuilt")
	}
}

// Nothing may be uploaded when the download fails verification — a bad object
// in the bucket is worse than no object, because the next run would skip it.
func TestRemoteFetchUploadsNothingOnChecksumMismatch(t *testing.T) {
	store := useStub(t)
	srvDir := t.TempDir()
	writeVCFGz(t, filepath.Join(srvDir, "src.vcf.gz"), false)
	ts := httptest.NewServer(http.FileServer(http.Dir(srvDir)))
	defer ts.Close()

	cfg := remoteCfg(t, "s3://bucket/prefix")
	src := config.Source{
		Name: "src", Version: "1", Format: "vcf",
		URL:      ts.URL + "/src.vcf.gz",
		Checksum: "sha256:" + strings.Repeat("00", 32), // deliberately wrong
	}
	if _, err := Source(context.Background(), cfg, src, false, false); err == nil {
		t.Fatal("a checksum mismatch did not fail the fetch")
	}
	if keys := store.keys(); len(keys) != 0 {
		t.Errorf("objects published despite a failed checksum: %v", keys)
	}
}

// A GTF is provisioned as its converted, indexed form. The original is not
// uploaded — it would only be deleted again.
func TestRemoteGTFPublishesConvertedOnly(t *testing.T) {
	store := useStub(t)
	srvDir := t.TempDir()
	gtf := "chr1\ttest\tgene\t50\t500\t.\t+\t.\tgene_id \"G1\"; gene_name \"AAA\";\n"
	if err := os.WriteFile(filepath.Join(srvDir, "genes.gtf"), []byte(gtf), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.FileServer(http.Dir(srvDir)))
	defer ts.Close()

	cfg := remoteCfg(t, "s3://bucket/prefix")
	src := config.Source{Name: "genes", Version: "1", Format: "gtf", URL: ts.URL + "/genes.gtf"}
	if _, err := Source(context.Background(), cfg, src, false, false); err != nil {
		t.Fatalf("Source: %v", err)
	}

	got := store.keys()
	want := []string{
		"s3://bucket/prefix/genes/1/genes.gtf.gz",
		"s3://bucket/prefix/genes/1/genes.gtf.gz.tbi",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("published %v, want just the converted pair %v", got, want)
	}
}

// --keep-raw additionally publishes the original.
func TestRemoteGTFKeepRawPublishesOriginal(t *testing.T) {
	store := useStub(t)
	srvDir := t.TempDir()
	gtf := "chr1\ttest\tgene\t50\t500\t.\t+\t.\tgene_id \"G1\"; gene_name \"AAA\";\n"
	if err := os.WriteFile(filepath.Join(srvDir, "genes.gtf"), []byte(gtf), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.FileServer(http.Dir(srvDir)))
	defer ts.Close()

	cfg := remoteCfg(t, "s3://bucket/prefix")
	src := config.Source{Name: "genes", Version: "1", Format: "gtf", URL: ts.URL + "/genes.gtf"}
	if _, err := Source(context.Background(), cfg, src, false, true); err != nil {
		t.Fatalf("Source: %v", err)
	}
	if _, ok := store.objects["s3://bucket/prefix/genes/1/genes.gtf"]; !ok {
		t.Errorf("--keep-raw did not publish the original; got %v", store.keys())
	}
}

// Missing() must consult the object store rather than the filesystem.
func TestMissingChecksTheObjectStore(t *testing.T) {
	store := useStub(t)
	cfg := remoteCfg(t, "s3://bucket/prefix")
	src := config.Source{Name: "src", Version: "1", Format: "vcf", URL: "https://x/src.vcf.gz"}

	if m := Missing(cfg, src); len(m) == 0 {
		t.Error("an empty bucket reported nothing missing")
	}
	store.objects["s3://bucket/prefix/src/1/src.vcf.gz"] = []byte("data")
	if m := Missing(cfg, src); len(m) != 1 || !strings.Contains(m[0], "index") {
		t.Errorf("data without an index should report the index missing, got %v", m)
	}
	store.objects["s3://bucket/prefix/src/1/src.vcf.gz.tbi"] = []byte("idx")
	if m := Missing(cfg, src); len(m) != 0 {
		t.Errorf("a complete source still reported missing: %v", m)
	}
}
