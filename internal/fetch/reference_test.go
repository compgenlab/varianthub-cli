package fetch

import (
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/cghts/seqio"
	"github.com/compgenlab/varianthub-cli/internal/config"
)

func referenceFasta() string {
	var b strings.Builder
	line := strings.Repeat("ACGT", 15)
	for _, c := range []string{"chr1", "chr2"} {
		b.WriteString(">" + c + " test\n")
		for i := 0; i < 40; i++ {
			b.WriteString(line)
			b.WriteByte('\n')
		}
		b.WriteString("ACGTACGT\n")
	}
	return b.String()
}

// refHome builds a config home with one reference source served over HTTP.
func refHome(t *testing.T, body []byte, name string) (*config.Config, config.Source) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(body)
	}))
	t.Cleanup(srv.Close)

	home := t.TempDir()
	write := func(rel, s string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("config.toml", "data_dir = \""+home+"/data\"\ncache_dir = \""+home+"/cache\"\n"+
		"annotations_dir = \""+home+"/annotations\"\n")
	write("annotations/sources/GRCh38/1/GRCh38-1.toml",
		"[[sources]]\ntype=\"reference\"\nname=\"GRCh38\"\nversion=\"1\"\nassembly=\"GRCh38\"\n"+
			"url=\""+srv.URL+"/"+name+"\"\n")
	write("annotations/snapshots/s.toml", "assembly=\"GRCh38\"\nsources=[\"GRCh38:1\"]\n")

	cfg, err := config.Load(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := cfg.LoadSnapshot("s")
	if err != nil {
		t.Fatal(err)
	}
	return cfg, snap.Sources[0]
}

// The references people publish are plain gzip, which faidx cannot index. A
// provisioned reference has to end up BGZF with a .fai beside it, or the failure
// lands inside a container as "Cannot index files compressed with gzip".
func TestFetchReferenceRecompressesAndIndexes(t *testing.T) {
	var gzBody strings.Builder
	zw := gzip.NewWriter(&gzBody) // plain gzip: no BGZF extra field
	zw.Write([]byte(referenceFasta()))
	zw.Close()

	cfg, src := refHome(t, []byte(gzBody.String()), "GRCh38.fa.gz")
	res, err := fetchReference(context.Background(), cfg, src, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Index != "built" {
		t.Errorf("Index = %q, want built", res.Index)
	}

	path := cfg.ResolveReferencePath(src, "GRCh38.fa.gz")
	if k, err := compressionOf(path); err != nil || k != compBGZF {
		t.Fatalf("provisioned file is not BGZF: kind=%v err=%v", k, err)
	}
	if _, err := os.Stat(path + ".fai"); err != nil {
		t.Errorf("no .fai: %v", err)
	}
	if _, err := os.Stat(path + ".gzi"); err != nil {
		t.Errorf("no .gzi (BGZF needs one to seek by uncompressed offset): %v", err)
	}

	// The point of indexing is random access, so read through it.
	r, err := seqio.NewIndexedFastaReader(path)
	if err != nil {
		t.Fatalf("the provisioned reference is not readable: %v", err)
	}
	defer r.Close()
	got, err := r.GetSequenceRange("chr2", 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ToUpper(string(got)) != "ACGT" {
		t.Errorf("chr2:0-4 = %q, want ACGT", got)
	}
}

// A second run must not re-download and re-index a reference that is already
// prepared — it is most of a gigabyte.
func TestFetchReferenceSkipsWhenPrepared(t *testing.T) {
	var gzBody strings.Builder
	zw := gzip.NewWriter(&gzBody)
	zw.Write([]byte(referenceFasta()))
	zw.Close()

	cfg, src := refHome(t, []byte(gzBody.String()), "GRCh38.fa.gz")
	if _, err := fetchReference(context.Background(), cfg, src, false); err != nil {
		t.Fatal(err)
	}
	res, err := fetchReference(context.Background(), cfg, src, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Data != "skipped" {
		t.Errorf("Data = %q on a second run, want skipped", res.Data)
	}
}

// An uncompressed FASTA is recompressed too: BGZF is what makes the .gzi
// meaningful, and a reference is large enough that leaving it uncompressed costs
// real disk.
func TestFetchReferenceCompressesPlainFasta(t *testing.T) {
	cfg, src := refHome(t, []byte(referenceFasta()), "GRCh38.fa")
	if _, err := fetchReference(context.Background(), cfg, src, false); err != nil {
		t.Fatal(err)
	}
	path := cfg.ResolveReferencePath(src, "GRCh38.fa.gz")
	if k, err := compressionOf(path); err != nil || k != compBGZF {
		t.Fatalf("plain FASTA was not recompressed: kind=%v err=%v path=%s", k, err, path)
	}
}

// A download job runs Snapshot(), not Source(), and the two used to disagree:
// the reference fork lived only in Source(), so a real download handed the FASTA
// to tabix and failed with "bgzf: FEXTRA flag not set" — a message that says
// nothing about references and sent the reader to the wrong code entirely.
func TestSnapshotProvisionsReferencesNotViaTabix(t *testing.T) {
	var gzBody strings.Builder
	zw := gzip.NewWriter(&gzBody)
	zw.Write([]byte(referenceFasta()))
	zw.Close()

	cfg, src := refHome(t, []byte(gzBody.String()), "GRCh38.fa.gz")
	snap := &config.Snapshot{Name: "s", Assembly: "GRCh38", Sources: []config.Source{src}}

	results, err := Snapshot(context.Background(), cfg, snap, "", false, true, 1)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Index != "built" {
		t.Errorf("Index = %q, want built (a reference is faidx-indexed, not tabix)",
			results[0].Index)
	}
	path := cfg.ResolveReferencePath(src, "GRCh38.fa.gz")
	if _, err := os.Stat(path + ".fai"); err != nil {
		t.Errorf("no .fai after a snapshot download: %v", err)
	}
}

// Provisioning and {ref} resolution must agree about where the file is.
//
// They resolve independently — fetch puts it somewhere, the snapshot loader
// looks somewhere — and a disagreement means provisioning reports success while
// {ref} points at nothing, which surfaces as a tool failing to open a file with
// no indication that it was ever downloaded.
func TestProvisionedReferenceIsWhereTheSnapshotLooks(t *testing.T) {
	var gzBody strings.Builder
	zw := gzip.NewWriter(&gzBody)
	zw.Write([]byte(referenceFasta()))
	zw.Close()

	cfg, src := refHome(t, []byte(gzBody.String()), "GRCh38.fa.gz")
	if _, err := fetchReference(context.Background(), cfg, src, false); err != nil {
		t.Fatal(err)
	}
	snap, err := cfg.LoadSnapshot("s")
	if err != nil {
		t.Fatal(err)
	}
	if snap.Reference == "" {
		t.Fatal("{ref} is empty after provisioning")
	}
	if _, err := os.Stat(snap.Reference); err != nil {
		t.Errorf("{ref} = %s, but nothing is there: %v", snap.Reference, err)
	}
	if _, err := os.Stat(snap.Reference + ".fai"); err != nil {
		t.Errorf("no .fai beside {ref}: %v", err)
	}
}

// A reference's durable copy is a real object in the store, and until it was
// inventoried the storage browser showed nothing and the metrics under-reported.
func TestInventoryCoversReferenceCopies(t *testing.T) {
	store := useStub(t)
	cfg := remoteCfg(t, "s3://bucket/prefix")
	ref := config.Source{
		Type: "reference", Name: "GRCh38", Version: "1",
		URL: "https://example.org/GRCh38.fa.gz",
	}

	// Nothing uploaded yet: nothing listed, so this is not "always reports".
	if got, _ := Inventory(context.Background(), cfg, ref); len(got) != 0 {
		t.Errorf("an unprovisioned reference listed %d files", len(got))
	}

	for _, k := range []string{
		"s3://bucket/prefix/GRCh38/1/GRCh38.fa.gz",
		"s3://bucket/prefix/GRCh38/1/GRCh38.fa.gz.fai",
		"s3://bucket/prefix/GRCh38/1/GRCh38.fa.gz.gzi",
	} {
		store.objects[k] = []byte("x")
	}
	got, err := Inventory(context.Background(), cfg, ref)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Errorf("listed %d files, want 3 (data, .fai, .gzi): %+v", len(got), got)
	}
	// Attribution keys on <name>/<version>/, so a path outside it is uploaded
	// but never tied to the source.
	for _, f := range got {
		if !strings.HasPrefix(f.Path, "GRCh38/1/") {
			t.Errorf("path %q is not under <name>/<version>/; it will not be attributed", f.Path)
		}
	}
}

// A tool's archived setup output is the largest single object this system
// writes — tens of gigabytes for VEP — and it appeared in no listing at all.
func TestInventoryCoversToolArchive(t *testing.T) {
	store := useStub(t)

	home := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("config.toml", "data_dir = \""+home+"/data\"\ncache_dir = \"s3://bucket/prefix\"\n"+
		"annotations_dir = \""+home+"/annotations\"\n")
	write("annotations/sources/vep/113/vep-113.toml",
		"[[sources]]\ntype=\"tool\"\nname=\"vep\"\nversion=\"113\"\n"+
			"  [[sources.setup]]\n  run=\"true\"\n  [[sources.steps]]\n  run=\"true\"\n")
	// tool_cache is a deployment fact, and arrives through the overlay.
	write("annotations/sources/vep/113/vep-113.locations.toml",
		"tool_cache = \"s3://bucket/prefix\"\n")
	write("annotations/snapshots/s.toml", "assembly=\"GRCh38\"\nsources=[\"vep:113\"]\n")

	cfg, err := config.Load(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := cfg.LoadSnapshot("s")
	if err != nil {
		t.Fatal(err)
	}
	src := snap.Sources[0]

	store.objects["s3://bucket/prefix/vep/113/tooldata.tar.gz"] = []byte("archive")
	got, err := Inventory(context.Background(), cfg, src)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, f := range got {
		if f.Path == "vep/113/tooldata.tar.gz" {
			found = true
		}
	}
	if !found {
		t.Errorf("the tool archive is not listed: %+v", got)
	}
}

// A prepared reference must still acquire its durable copy.
//
// The local file satisfies every later run, so returning early on the skip path
// meant the upload never got a second chance — a reference prepared before a
// storage location was configured could never be copied to it without deleting
// the local file first. The tool archive fell into exactly this trap.
func TestPreparedReferenceStillGetsItsDurableCopy(t *testing.T) {
	store := useStub(t)
	var gzBody strings.Builder
	zw := gzip.NewWriter(&gzBody)
	zw.Write([]byte(referenceFasta()))
	zw.Close()

	cfg, src := refHome(t, []byte(gzBody.String()), "GRCh38.fa.gz")
	// A remote cache, so a durable copy is expected.
	cfg.CacheDir = "s3://bucket/prefix"

	if _, err := fetchReference(context.Background(), cfg, src, false); err != nil {
		t.Fatal(err)
	}
	obj := "s3://bucket/prefix/GRCh38/1/GRCh38.fa.gz"
	if _, ok := store.objects[obj]; !ok {
		t.Fatalf("first run published no durable copy; have %v", keysOf(store.objects))
	}

	// Lose the copy, as a pruned bucket or a failed upload would.
	delete(store.objects, obj)

	// The local file is prepared, so this run takes the skip path.
	res, err := fetchReference(context.Background(), cfg, src, false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Data != "skipped" {
		t.Fatalf("expected the skip path, got Data=%q", res.Data)
	}
	if _, ok := store.objects[obj]; !ok {
		t.Error("the missing durable copy was not re-uploaded; it could only be " +
			"fixed by deleting the local file and re-fetching a gigabyte")
	}
}

func keysOf(m map[string][]byte) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// A download's JSON output attaches each source's inventory by looking results
// up by name:version, so Result.Source has to be that and nothing else.
//
// setupToolSource used to append " (tool)" for the text listing, so the lookup
// never matched: every tool reported an empty file list, and a 35 GB setup
// archive and a 1.5 GB image were invisible in the storage browser and missing
// from the metrics.
func TestToolResultSourceIsTheIdentity(t *testing.T) {
	home := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("config.toml", "data_dir = \""+home+"/data\"\ncache_dir = \""+home+"/cache\"\n"+
		"annotations_dir = \""+home+"/annotations\"\n")
	write("annotations/sources/vep/113/vep-113.toml",
		"[[sources]]\ntype=\"tool\"\nname=\"vep\"\nversion=\"113\"\n"+
			"  [[sources.setup]]\n  run=\"true\"\n  [[sources.steps]]\n  run=\"true\"\n")
	write("annotations/snapshots/s.toml", "assembly=\"GRCh38\"\nsources=[\"vep:113\"]\n")

	cfg, err := config.Load(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	snap, err := cfg.LoadSnapshot("s")
	if err != nil {
		t.Fatal(err)
	}
	res, err := setupToolSource(context.Background(), cfg, snap.Sources[0], "", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Source != "vep:113" {
		t.Errorf("Result.Source = %q, want vep:113 — the JSON output keys on this, "+
			"so anything else means the tool reports no files", res.Source)
	}
}
