package fetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	// gets counts completed downloads, so a test can assert that a transfer did
	// not happen — the restore path is only correct if it stays lazy.
	gets    int
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

// PutStream mirrors the real store's contract in the way that matters here: the
// body is read to completion, and a read error means nothing is stored. That is
// how an abort-on-mismatch upload behaves, and asserting it needs the stub to
// leave no object behind rather than a partial one.
func (s *stubStore) PutStream(_ context.Context, ref objstore.Ref, r io.Reader, checksum string) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err // the multipart upload would have been aborted; store nothing
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
	if ok {
		s.gets++
	}
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

// Scratch is for work, not for transit.
//
// A source whose index the upstream already publishes needs nothing read
// locally: the digest can be taken from the stream. Staging it anyway means
// scratch the size of the largest single file a source publishes — tens of
// gigabytes for dbSNP or CADD — held only so the bytes can be hashed on the way
// past. This asserts the file never touches a disk, which is the whole point.
func TestPublishedIndexStreamsWithoutStaging(t *testing.T) {
	s := newStubStore()
	SetStore(s)
	t.Cleanup(func() { SetStore(nil) })

	data := []byte("chr1\t1\tA\tT\n")
	idx := []byte("fake index bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".tbi"):
			w.Write(idx)
		default:
			w.Write(data)
		}
	}))
	t.Cleanup(srv.Close)

	// Any staging directory this creates would appear under TMPDIR.
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)

	f := config.SourceFile{
		URL:      srv.URL + "/d.tsv.gz",
		URLIndex: srv.URL + "/d.tsv.gz.tbi",
		Path:     "s3://bucket/d.tsv.gz",
		Checksum: "sha256:" + hexSHA256(data),
	}
	// The pair check needs a store that serves range requests, which a stub does
	// not; it is covered separately below. What matters here is the decision to
	// stream and what lands in the store.
	verifyPair = func(context.Context, string, string) error { return nil }
	t.Cleanup(func() { verifyPair = verifyRemotePair })

	_, _, err := fetchFileRemote(context.Background(), f, "tab", "src", false, true)
	if err != nil {
		t.Fatalf("fetchFileRemote: %v", err)
	}

	if got := s.objects["s3://bucket/d.tsv.gz"]; !bytes.Equal(got, data) {
		t.Errorf("data object = %q, want %q", got, data)
	}
	if got := s.objects["s3://bucket/d.tsv.gz.tbi"]; !bytes.Equal(got, idx) {
		t.Errorf("index object = %q, want %q", got, idx)
	}

	entries, _ := os.ReadDir(tmp)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "varhub-stage-") {
			t.Errorf("staged to %s; a published index needs no local copy", e.Name())
		}
	}
}

// A digest that disagrees must leave nothing readable.
//
// With staging, a bad checksum simply means the upload never starts. Streaming
// has no such moment, so verification has to fail a *read* — the upload manager
// aborts on a body error, and an object that was never completed is one no
// reader can see. Checking after the upload returned would be checking
// something already visible.
func TestStreamedChecksumMismatchPublishesNothing(t *testing.T) {
	s := newStubStore()
	SetStore(s)
	t.Cleanup(func() { SetStore(nil) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("the bytes actually served"))
	}))
	t.Cleanup(srv.Close)

	err := streamToObject(context.Background(), srv.URL+"/d.gz", "s3://bucket/d.gz",
		"sha256:"+hexSHA256([]byte("what the manifest promised")))
	if err == nil {
		t.Fatal("a mismatched digest was accepted")
	}
	if !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("error does not name the mismatch: %v", err)
	}
	if _, ok := s.objects["s3://bucket/d.gz"]; ok {
		t.Error("an object was published despite the digest disagreeing")
	}
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// A source can live somewhere other than cache_dir, and the presence check has
// to follow it.
//
// An overlay names a source's own root — that is how one job reads sources from
// several places — so a job whose cache_dir is a local path can still hold a
// source in a bucket. Choosing the check from cache_dir alone ran os.Stat on an
// "s3://…" string, which is never a file, so a fully downloaded source was
// reported as missing: "sources not downloaded — run `varhub download`:
// REVEL:1.3 (missing s3://varhub-dev/REVEL/1.3/REVEL.tab.gz)", naming an object
// that was plainly there.
func TestMissingFollowsTheSourceNotTheCacheDir(t *testing.T) {
	store := useStub(t)

	home := t.TempDir()
	cfgPath := filepath.Join(home, "config.toml")
	// A LOCAL cache_dir: the case that misrouted the check.
	if err := os.WriteFile(cfgPath, []byte(
		"data_dir = \""+home+"/data\"\ncache_dir = \""+home+"/cache\"\n"+
			"annotations_dir = \""+home+"/annotations\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.SourceDir("REVEL", "1.3"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.SourceFile("REVEL", "1.3"), []byte(
		"[[sources]]\nname=\"REVEL\"\nversion=\"1.3\"\nformat=\"tab\"\n"+
			"url=\"https://example.org/REVEL.tab.gz\"\n"+
			"  [[sources.annotations]]\n  name=\"REVEL\"\n  field=\"5\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The overlay puts this source in a bucket, while the cache stays local.
	if err := os.WriteFile(cfg.LocationsPath("REVEL", "1.3"),
		[]byte("root = \"s3://bucket\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(cfg.SnapshotsPath(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.SnapshotFile("s"),
		[]byte("assembly=\"GRCh38\"\nsources=[\"REVEL:1.3\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := cfg.LoadSnapshot("s")
	if err != nil {
		t.Fatal(err)
	}
	src := snap.Sources[0]

	// Nothing uploaded yet: still reported missing, so this is not just "never
	// reports anything".
	if m := Missing(cfg, src); len(m) == 0 {
		t.Error("an empty bucket reported nothing missing")
	}
	store.objects["s3://bucket/REVEL/1.3/REVEL.tab.gz"] = []byte("data")
	store.objects["s3://bucket/REVEL/1.3/REVEL.tab.gz.tbi"] = []byte("idx")

	if m := Missing(cfg, src); len(m) != 0 {
		t.Errorf("a downloaded source was reported missing: %v", m)
	}
}
