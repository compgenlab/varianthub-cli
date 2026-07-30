package fetch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/cghts/htsio/tabix"
	"github.com/compgenlab/varianthub-cli/internal/bbitest"

	"github.com/compgenlab/varianthub-cli/internal/config"
)

// sha256Spec returns the "sha256:<hex>" checksum spec for a file on disk.
func sha256Spec(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// writeVCFGz writes a bgzipped VCF, optionally with a .tbi (AutoIndex).
func writeVCFGz(t *testing.T, path string, indexed bool) {
	t.Helper()
	opts := tabix.NewWriterOpts().VCF()
	if indexed {
		opts = opts.AutoIndex()
	}
	w := tabix.NewWriter(path, opts)
	w.WriteHeader("##fileformat=VCFv4.2")
	w.WriteHeader("#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO")
	if err := w.Write("chr1\t100\t.\tA\tG\t.\t.\tCLNSIG=Pathogenic"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSourceReusePublishedIndexAndBuild(t *testing.T) {
	srvDir := t.TempDir()
	writeVCFGz(t, filepath.Join(srvDir, "clinvar.vcf.gz"), true) // ships .tbi
	writeVCFGz(t, filepath.Join(srvDir, "gnomad.vcf.gz"), false) // no .tbi

	ts := httptest.NewServer(http.FileServer(http.Dir(srvDir)))
	defer ts.Close()

	cfg := &config.Config{DataDir: t.TempDir()}
	ctx := context.Background()

	r1, err := Source(ctx, cfg, config.Source{
		Name: "clinvar", Version: "1", Format: "vcf",
		URL:      ts.URL + "/clinvar.vcf.gz",
		Checksum: sha256Spec(t, filepath.Join(srvDir, "clinvar.vcf.gz")),
	}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Data != "downloaded" || r1.Index != "downloaded" {
		t.Errorf("clinvar: got data=%s index=%s, want downloaded/downloaded", r1.Data, r1.Index)
	}

	r2, err := Source(ctx, cfg, config.Source{
		Name: "gnomad", Version: "1", Format: "vcf",
		URL:      ts.URL + "/gnomad.vcf.gz",
		Checksum: sha256Spec(t, filepath.Join(srvDir, "gnomad.vcf.gz")),
	}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Index != "built" {
		t.Errorf("gnomad: got index=%s, want built", r2.Index)
	}

	// Re-sync without force: data skipped, index reused.
	r3, err := Source(ctx, cfg, config.Source{
		Name: "clinvar", Version: "1", Format: "vcf",
		URL: ts.URL + "/clinvar.vcf.gz",
	}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if r3.Data != "skipped" || r3.Index != "reused" {
		t.Errorf("re-sync: got data=%s index=%s, want skipped/reused", r3.Data, r3.Index)
	}
}

func TestSourceGTFNoIndex(t *testing.T) {
	srvDir := t.TempDir()
	gtfBody := "chr1\tt\texon\t101\t200\t.\t+\t.\tgene_id \"G\"; gene_name \"G\"; transcript_id \"T\";\n"
	if err := os.WriteFile(filepath.Join(srvDir, "genes.gtf"), []byte(gtfBody), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.FileServer(http.Dir(srvDir)))
	defer ts.Close()

	cfg := &config.Config{DataDir: t.TempDir()}
	src := config.Source{Name: "gencode", Version: "38", Format: "gtf", URL: ts.URL + "/genes.gtf"}

	r, err := Source(context.Background(), cfg, src, false, true)
	if err != nil {
		t.Fatalf("gtf source download: %v", err)
	}
	// Downloaded, then bgzip+tabix-indexed (built once, under cache_dir).
	if r.Data != "downloaded" || r.Index != "built" {
		t.Errorf("gtf: got data=%s index=%s, want downloaded/built", r.Data, r.Index)
	}
	// The built index + its .tbi exist, so the source is complete.
	if m := Missing(cfg, src); m != nil {
		t.Errorf("gtf source should not be Missing after indexing, got %v", m)
	}
	idx := cfg.ResolveGTFIndexPath(src)
	if !fileExists(idx) || !fileExists(idx+".tbi") {
		t.Errorf("expected %s + .tbi to exist", idx)
	}
}

func TestSourceChecksumVerify(t *testing.T) {
	srvDir := t.TempDir()
	file := filepath.Join(srvDir, "clinvar.vcf.gz")
	writeVCFGz(t, file, true)
	ts := httptest.NewServer(http.FileServer(http.Dir(srvDir)))
	defer ts.Close()

	cfg := &config.Config{DataDir: t.TempDir()}
	ctx := context.Background()

	// Correct checksum → download succeeds.
	good, err := Source(ctx, cfg, config.Source{
		Name: "clinvar", Version: "1", Format: "vcf",
		URL: ts.URL + "/clinvar.vcf.gz", Checksum: sha256Spec(t, file),
	}, false, true)
	if err != nil {
		t.Fatalf("correct checksum: %v", err)
	}
	if good.Data != "downloaded" {
		t.Errorf("data=%s, want downloaded", good.Data)
	}

	// Wrong checksum → error, and no file (or .tmp) left at the target.
	bad := config.Source{
		Name: "gnomad", Version: "1", Format: "vcf",
		URL: ts.URL + "/clinvar.vcf.gz", Checksum: "sha256:" + strings.Repeat("0", 64),
	}
	if _, err := Source(ctx, cfg, bad, false, true); err == nil {
		t.Fatal("expected a checksum mismatch error")
	}
	target := cfg.ResolveSourcePath(bad)
	if fileExists(target) {
		t.Errorf("failed download left a file at %s", target)
	}
	if fileExists(target + ".tmp") {
		t.Errorf("failed download left a .tmp at %s", target)
	}
}

func TestSourceExplicitIndexURL(t *testing.T) {
	srvDir := t.TempDir()
	vcf := filepath.Join(srvDir, "clinvar.vcf.gz")
	writeVCFGz(t, vcf, true) // writes clinvar.vcf.gz.tbi alongside
	// Move the index to a non-guessable name so only index_url can locate it
	// (URL+".tbi" will 404, and falling back to "built" would fail the assertion).
	custom := filepath.Join(srvDir, "custom.tbi")
	if err := os.Rename(vcf+".tbi", custom); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.FileServer(http.Dir(srvDir)))
	defer ts.Close()

	cfg := &config.Config{DataDir: t.TempDir()}
	r, err := Source(context.Background(), cfg, config.Source{
		Name: "clinvar", Version: "1", Format: "vcf",
		URL:           ts.URL + "/clinvar.vcf.gz",
		Checksum:      sha256Spec(t, vcf),
		URLIndex:      ts.URL + "/custom.tbi",
		ChecksumIndex: sha256Spec(t, custom),
	}, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.Index != "downloaded" {
		t.Errorf("index=%s, want downloaded (via index_url, not built)", r.Index)
	}
}

// TestToolsSetupOnce: a tool source's setup runs once (sentinel-gated) via
// fetch.Snapshot, and --force re-runs it.
func TestToolsSetupOnce(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir()}
	rel := &config.Snapshot{Sources: []config.Source{{
		Type: "tool", Name: "vep", Version: "112",
		Setup: []config.Step{{Run: "touch {datadir}/installed"}},
	}}}
	ctx := context.Background()

	res, err := Snapshot(ctx, cfg, rel, "", false, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	datadir := cfg.ResolveToolData(rel.Sources[0].AsTool())
	if !fileExists(filepath.Join(datadir, "installed")) {
		t.Fatal("setup did not run (marker missing)")
	}
	if res[0].Index != "setup: done" {
		t.Errorf("first: status=%q, want 'setup: done'", res[0].Index)
	}
	// Second run: sentinel present → skipped.
	res, _ = Snapshot(ctx, cfg, rel, "", false, true, 1)
	if res[0].Index != "setup: skipped" {
		t.Errorf("second: status=%q, want 'setup: skipped'", res[0].Index)
	}
	// --force: re-runs.
	res, _ = Snapshot(ctx, cfg, rel, "", true, true, 1)
	if res[0].Index != "setup: done" {
		t.Errorf("force: status=%q, want 'setup: done'", res[0].Index)
	}
}

// TestToolsOnlyFilter: the `only` arg restricts download to one tool source (by name
// or name:version); an unknown name errors.
func TestToolsOnlyFilter(t *testing.T) {
	cfg := &config.Config{DataDir: t.TempDir()}
	rel := &config.Snapshot{Name: "2026-06", Sources: []config.Source{
		{Type: "tool", Name: "vep", Version: "112", Setup: []config.Step{{Run: "touch {datadir}/installed"}}},
		{Type: "tool", Name: "cadd", Version: "1.7", Setup: []config.Step{{Run: "touch {datadir}/installed"}}},
	}}
	ctx := context.Background()

	res, err := Snapshot(ctx, cfg, rel, "vep", false, true, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Source != "vep:112 (tool)" {
		t.Fatalf("only=vep ran %+v, want just vep:112", res)
	}
	if fileExists(filepath.Join(cfg.ResolveToolData(rel.Sources[1].AsTool()), "installed")) {
		t.Error("cadd setup ran despite --source vep")
	}
	// name:version form also matches.
	if res, err = Snapshot(ctx, cfg, rel, "cadd:1.7", false, true, 1); err != nil || len(res) != 1 {
		t.Fatalf("only=cadd:1.7 → res=%+v err=%v", res, err)
	}
	// Unknown name errors.
	if _, err := Snapshot(ctx, cfg, rel, "nope", false, true, 1); err == nil {
		t.Error("expected error for unknown source")
	}
}

// TestCacheKeyedDedup: a source with no explicit path is cached by name/version,
// so a second snapshot referencing the same source reuses the cached file.
func TestCacheKeyedDedup(t *testing.T) {
	srvDir := t.TempDir()
	writeVCFGz(t, filepath.Join(srvDir, "clinvar.vcf.gz"), true)
	ts := httptest.NewServer(http.FileServer(http.Dir(srvDir)))
	defer ts.Close()

	cfg := &config.Config{DataDir: t.TempDir()}
	src := config.Source{Name: "clinvar", Version: "2026-01", Format: "vcf", URL: ts.URL + "/clinvar.vcf.gz",
		Checksum: sha256Spec(t, filepath.Join(srvDir, "clinvar.vcf.gz"))}
	ctx := context.Background()

	r1, err := Source(ctx, cfg, src, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Data != "downloaded" {
		t.Errorf("first: data=%s, want downloaded", r1.Data)
	}
	// The cached file lives under cache_dir keyed by name/version.
	want := filepath.Join(cfg.CacheDirAbs(), "clinvar", "2026-01", "clinvar.vcf.gz")
	if !fileExists(want) {
		t.Errorf("expected cached file at %s", want)
	}
	// Second reference (e.g. another snapshot) reuses the cache — no re-download.
	r2, err := Source(ctx, cfg, src, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Data != "skipped" {
		t.Errorf("second: data=%s, want skipped (cache hit)", r2.Data)
	}
}

// TestBuildSource exercises the [[sources.build]] runner: download an input, copy
// a co-located asset into the workdir, run steps that produce {output}(+index),
// and cache + verify the result.
func TestBuildSource(t *testing.T) {
	// A prebuilt indexed tab file the build "produces" by copying.
	dir := t.TempDir()
	pre := filepath.Join(dir, "pre.tab.gz")
	w := tabix.NewWriter(pre, tabix.NewWriterOpts().Columns(1, 2, 0).AutoIndex())
	if err := w.Write("chr1\t100\tA\tG\t0.9"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	// An "input" file served over HTTP.
	srv := t.TempDir()
	if err := os.WriteFile(filepath.Join(srv, "seg.csv"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.FileServer(http.Dir(srv)))
	defer ts.Close()

	cfg := &config.Config{DataDir: t.TempDir(), AnnotationsDir: t.TempDir()}
	src := config.Source{Name: "revel", Version: "1.3", Format: "tab", Build: &config.SourceBuild{
		Output: "merged.tab.gz",
		Inputs: []string{ts.URL + "/seg.csv"},
		Assets: []string{"helper.sh"},
		Run: []string{
			"test -f {workdir}/helper.sh",     // asset present
			"test -f {inputs}/seg.csv",        // input downloaded
			"cp " + pre + " {output}",         // produce the data file
			"cp " + pre + ".tbi {output}.tbi", // ship our index
		},
	}}
	// A co-located asset in the source's version dir.
	srcDir := cfg.SourceDir(src.Name, src.Version)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "helper.sh"), []byte("#!/bin/bash\n:\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	res, err := buildSource(context.Background(), cfg, src, false)
	if err != nil {
		t.Fatalf("buildSource: %v", err)
	}
	if res.Data != "built" {
		t.Errorf("data = %q, want built", res.Data)
	}
	out := cfg.ResolveSourcePath(src)
	if !fileExists(out) || !fileExists(out+".tbi") {
		t.Fatalf("cached output/index missing at %s", out)
	}
	r, err := tabix.NewReader(out)
	if err != nil {
		t.Fatalf("built file not a valid tabix: %v", err)
	}
	r.Close()

	// Second call without force → cached.
	res2, _ := buildSource(context.Background(), cfg, src, false)
	if res2.Data != "built (cached)" {
		t.Errorf("second build = %q, want 'built (cached)'", res2.Data)
	}
}

// TestSnapshotBuildSource runs a build source through Snapshot (not buildSource
// directly) — guarding the bug where the errgroup's cancelled context leaked into
// the post-Wait build pass ("context canceled").
func TestSnapshotBuildSource(t *testing.T) {
	t.Setenv("VARHUB_HOME", "")
	base := t.TempDir()

	// prebuilt indexed tab the build copies as {output}
	pre := filepath.Join(base, "pre.tab.gz")
	w := tabix.NewWriter(pre, tabix.NewWriterOpts().Columns(1, 2, 0).AutoIndex())
	if err := w.Write("chr1\t100\tA\tG\t0.5"); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	srv := t.TempDir()
	os.WriteFile(filepath.Join(srv, "seg.csv"), []byte("x"), 0o644)
	ts := httptest.NewServer(http.FileServer(http.Dir(srv)))
	defer ts.Close()

	cfgPath := filepath.Join(base, "config.toml")
	os.WriteFile(cfgPath, []byte("assembly=\"GRCh38\"\ndata_dir=\"data\"\nannotations_dir=\".\"\ndefault_snapshot=\"s\"\n"), 0o644)
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// v2 layout: a top-level source (+ co-located asset) referenced by a manifest.
	srcDir := cfg.SourceDir("revel", "1.3")
	os.MkdirAll(srcDir, 0o755)
	os.WriteFile(filepath.Join(srcDir, "helper.sh"), []byte("#!/bin/bash\n:\n"), 0o755)
	frag := "[[sources]]\nname=\"revel\"\nversion=\"1.3\"\nformat=\"tab\"\n" +
		"  [sources.build]\n  output=\"out.tab.gz\"\n" +
		"  inputs=[\"" + ts.URL + "/seg.csv\"]\n  assets=[\"helper.sh\"]\n" +
		"  run=[\"test -f {workdir}/helper.sh\", \"cp " + pre + " {output}\", \"cp " + pre + ".tbi {output}.tbi\"]\n" +
		"  [[sources.annotations]]\n  name=\"revel\"\n  field=\"5\"\n  type=\"numeric\"\n"
	os.WriteFile(cfg.SourceFile("revel", "1.3"), []byte(frag), 0o644)
	os.MkdirAll(cfg.SnapshotsPath(), 0o755)
	os.WriteFile(cfg.SnapshotFile("s"), []byte("assembly=\"GRCh38\"\nsources=[\"revel:1.3\"]\n"), 0o644)

	snap, err := cfg.LoadSnapshot("s")
	if err != nil {
		t.Fatal(err)
	}
	results, err := Snapshot(context.Background(), cfg, snap, "", false, true, 4)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(results) != 1 || results[0].Data != "built" {
		t.Fatalf("results = %+v, want one 'built'", results)
	}
}

// TestSourceBigWigNoIndex: a bigWig is downloaded and used as-is — self-indexed,
// so no tabix index is built or expected (mirrors TestSourceGTFNoIndex).
func TestSourceBigWigNoIndex(t *testing.T) {
	srvDir := t.TempDir()
	bw := filepath.Join(srvDir, "scores.bw")
	if err := bbitest.WriteBigWig(bw, "chr1", []bbitest.WigItem{{Start: 99, End: 100, Val: 1.5}}); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.FileServer(http.Dir(srvDir)))
	defer ts.Close()

	cfg := &config.Config{DataDir: t.TempDir()}
	src := config.Source{Name: "revel", Version: "1.3", Format: "bigwig", URL: ts.URL + "/scores.bw"}

	r, err := Source(context.Background(), cfg, src, false, true)
	if err != nil {
		t.Fatalf("bigwig source download: %v", err)
	}
	if r.Data != "downloaded" || r.Index != "none" {
		t.Errorf("bigwig: got data=%s index=%s, want downloaded/none", r.Data, r.Index)
	}
	if m := Missing(cfg, src); m != nil {
		t.Errorf("bigwig source should not be Missing, got %v", m)
	}
}

// TestEnsureIndexedGTF: an unsorted, plain GTF is sorted + bgzipped + tabix-indexed
// under cache_dir, and a second call reuses it (no rebuild).
func TestEnsureIndexedGTF(t *testing.T) {
	dir := t.TempDir()
	// Deliberately out of coordinate order — the tabix writer sorts it.
	raw := filepath.Join(dir, "genes.gtf")
	body := "chr1\tt\texon\t5000\t6000\t.\t-\t.\tgene_id \"B\";\n" +
		"chr1\tt\tgene\t1000\t2000\t.\t+\t.\tgene_id \"A\";\n" +
		"chr1\tt\texon\t1000\t2000\t.\t+\t.\tgene_id \"A\";\n"
	if err := os.WriteFile(raw, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{CacheDir: t.TempDir()}
	src := config.Source{Name: "gencode", Version: "44", Format: "gtf", LocalPath: raw}

	idx, status, err := EnsureIndexedGTF(cfg, src, false)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if status != "built" {
		t.Errorf("first call status = %q, want built", status)
	}
	if idx != cfg.ResolveGTFIndexPath(src) {
		t.Errorf("idx = %q, want %q", idx, cfg.ResolveGTFIndexPath(src))
	}
	if !fileExists(idx) || !fileExists(idx+".tbi") {
		t.Fatalf("index or .tbi missing at %s", idx)
	}
	// The output must be a valid tabix file (sorted); opening it must succeed.
	r, err := tabix.NewReader(idx)
	if err != nil {
		t.Fatalf("indexed output not readable: %v", err)
	}
	r.Close()

	// Second call reuses the cached index.
	_, status2, err := EnsureIndexedGTF(cfg, src, false)
	if err != nil {
		t.Fatal(err)
	}
	if status2 != "reused" {
		t.Errorf("second call status = %q, want reused", status2)
	}
}

// TestPruneRawGTF: after conversion the original is removable, and the source
// keeps working without it. Before the check order was fixed, EnsureIndexedGTF
// demanded the raw even when a perfectly good derived index was on disk.
func TestPruneRawGTF(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "genes.gtf")
	body := "chr1\tt\tgene\t1000\t2000\t.\t+\t.\tgene_id \"A\";\n"
	if err := os.WriteFile(raw, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{CacheDir: t.TempDir()}
	src := config.Source{Name: "gencode", Version: "44", Format: "gtf", LocalPath: raw}

	if _, status, err := EnsureIndexedGTF(cfg, src, false); err != nil || status != "built" {
		t.Fatalf("build: %q %v", status, err)
	}

	freed, err := PruneRawGTF(cfg, src)
	if err != nil {
		t.Fatalf("PruneRawGTF: %v", err)
	}
	if freed != int64(len(body)) {
		t.Errorf("freed = %d, want %d", freed, len(body))
	}
	if fileExists(raw) {
		t.Error("the original is still present")
	}

	// The point of the reorder: the source still resolves from the derived index.
	idx, status, err := EnsureIndexedGTF(cfg, src, false)
	if err != nil {
		t.Fatalf("after prune: %v", err)
	}
	if status != "reused" {
		t.Errorf("after prune status = %q, want reused", status)
	}
	if r, rerr := tabix.NewReader(idx); rerr != nil {
		t.Errorf("indexed output unreadable after prune: %v", rerr)
	} else {
		r.Close()
	}

	// Pruning twice is a no-op, not an error.
	if freed, err := PruneRawGTF(cfg, src); err != nil || freed != 0 {
		t.Errorf("second prune = %d, %v; want 0 and no error", freed, err)
	}
}

// A source that shipped pre-indexed IS the queried file. Pruning it would delete
// the data.
func TestPruneRawGTFSkipsPreIndexed(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "genes.gtf.gz")
	for _, f := range []string{raw, raw + ".tbi"} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{CacheDir: t.TempDir()}
	src := config.Source{Name: "g", Version: "1", Format: "gtf", LocalPath: raw}

	if _, status, _ := EnsureIndexedGTF(cfg, src, false); status != "pre-indexed" {
		t.Fatalf("status = %q, want pre-indexed", status)
	}
	if freed, err := PruneRawGTF(cfg, src); err != nil || freed != 0 {
		t.Errorf("prune = %d, %v; want a no-op", freed, err)
	}
	if !fileExists(raw) {
		t.Fatal("pruned a pre-indexed source's only copy")
	}
}

// Without a usable replacement, pruning must do nothing — otherwise a failed or
// partial conversion would take the only copy with it.
func TestPruneRawGTFRequiresReplacement(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "genes.gtf")
	if err := os.WriteFile(raw, []byte("chr1\tt\tgene\t1\t2\t.\t+\t.\tgene_id \"A\";\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{CacheDir: t.TempDir()}
	src := config.Source{Name: "g", Version: "1", Format: "gtf", LocalPath: raw}

	// No index built yet.
	if freed, err := PruneRawGTF(cfg, src); err != nil || freed != 0 {
		t.Errorf("prune with no index = %d, %v; want a no-op", freed, err)
	}
	if !fileExists(raw) {
		t.Fatal("pruned the only copy")
	}

	// Derived file present but its .tbi missing: still not usable.
	idx := cfg.ResolveGTFIndexPath(src)
	if err := os.MkdirAll(filepath.Dir(idx), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idx, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if freed, err := PruneRawGTF(cfg, src); err != nil || freed != 0 {
		t.Errorf("prune with an unindexed replacement = %d, %v; want a no-op", freed, err)
	}
	if !fileExists(raw) {
		t.Fatal("pruned with no usable replacement")
	}
}

// A converted GTF is usable from its derived index alone. Missing() checked the
// raw first, so a pruned source was reported "not downloaded" despite being
// queryable — annotation refused to run on data that was right there.
func TestMissingAcceptsPrunedGTF(t *testing.T) {
	dir := t.TempDir()
	raw := filepath.Join(dir, "genes.gtf")
	if err := os.WriteFile(raw, []byte("chr1\tt\tgene\t1000\t2000\t.\t+\t.\tgene_id \"A\";\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{CacheDir: t.TempDir()}
	src := config.Source{Name: "gencode", Version: "44", Format: "gtf", LocalPath: raw}

	if m := Missing(cfg, src); len(m) == 0 {
		t.Fatal("an unindexed GTF should be reported missing")
	}
	if _, _, err := EnsureIndexedGTF(cfg, src, false); err != nil {
		t.Fatal(err)
	}
	if m := Missing(cfg, src); len(m) != 0 {
		t.Fatalf("indexed GTF reported missing: %v", m)
	}
	if _, err := PruneRawGTF(cfg, src); err != nil {
		t.Fatal(err)
	}
	if m := Missing(cfg, src); len(m) != 0 {
		t.Fatalf("pruned GTF reported missing: %v", m)
	}

	// With both gone it really is missing, and the message names the raw.
	os.Remove(cfg.ResolveGTFIndexPath(src))
	os.Remove(cfg.ResolveGTFIndexPath(src) + ".tbi")
	m := Missing(cfg, src)
	if len(m) != 1 || !strings.Contains(m[0], "genes.gtf") {
		t.Errorf("missing = %v, want the raw path", m)
	}
}
