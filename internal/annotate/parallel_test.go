package annotate

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/compgenlab/cghts/htsio/tabix"
	"github.com/compgenlab/cghts/vcf"

	"github.com/compgenlab/varianthub-cli/internal/config"
)

// writeScoreTab writes a tiny indexed "chrom pos ref alt score" tabix file.
func writeScoreTab(t *testing.T, path string, lines []string) {
	t.Helper()
	w := tabix.NewWriter(path, tabix.NewWriterOpts().Columns(1, 2, 0).AutoIndex())
	for _, l := range lines {
		if err := w.Write(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// parallelSnapshot builds a snapshot with two tabix data sources plus builtins
// (tstv, vardist, dosage) — exercising INFO merge, the vardist stream, and a
// sample-derived FORMAT builtin.
func parallelSnapshot(t *testing.T, dir string) *config.Snapshot {
	t.Helper()
	a := filepath.Join(dir, "srcA.tab.gz")
	b := filepath.Join(dir, "srcB.tab.gz")
	writeScoreTab(t, a, []string{"chr1\t100\tA\tG\t0.10", "chr1\t200\tC\tT\t0.20"})
	writeScoreTab(t, b, []string{"chr1\t100\tA\tG\t0.90", "chr1\t200\tC\tT\t0.80"})
	return &config.Snapshot{
		Name: "r",
		Sources: []config.Source{
			{Name: "srcA", Format: "tab", RefCol: 3, AltCol: 4, LocalPath: a},
			{Name: "srcB", Format: "tab", RefCol: 3, AltCol: 4, LocalPath: b},
		},
		Annotations: []config.Annotation{
			{Name: "tstv", Source: "tstv", Builtin: "tstv"},
			{Name: "vardist", Source: "vardist", Builtin: "vardist"},
			{Name: "dosage", Source: "dosage", Builtin: "dosage"},
			{Name: "SCOREA", Source: "srcA", Field: "5", Type: "numeric"},
			{Name: "SCOREB", Source: "srcB", Field: "5", Type: "numeric"},
		},
	}
}

// TestVerboseFanOutLogging: under a verbose logger, a multi-source fan-out emits a
// job plan, one completion line per job, and a merge line; a single-pass run emits
// the running/total record count. Quiet (nil logger) emits nothing.
func TestVerboseFanOutLogging(t *testing.T) {
	dir := t.TempDir()
	snap := parallelSnapshot(t, dir)
	in := filepath.Join(dir, "in.vcf")
	if err := os.WriteFile(in, []byte(parallelInputVCF), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf strings.Builder
	ctx := WithLogger(context.Background(), NewLogger(&buf))
	out := filepath.Join(dir, "out.vcf")
	if err := AnnotateVCFSnapshot(ctx, &config.Config{}, snap, in, out, 4, false, ""); err != nil {
		t.Fatal(err)
	}
	log := buf.String()
	// 2 tab sources + a builtins job = 3 fan-out jobs.
	for _, want := range []string{"annotating 3 jobs", "job 1/3 complete", "job 3/3 complete", "merging 3 annotated parts"} {
		if !strings.Contains(log, want) {
			t.Errorf("fan-out log missing %q; got:\n%s", want, log)
		}
	}

	// Quiet: a nil logger in ctx must produce no output and still succeed.
	quiet := WithLogger(context.Background(), nil)
	if err := AnnotateVCFSnapshot(quiet, &config.Config{}, snap, in, filepath.Join(dir, "q.vcf"), 4, false, ""); err != nil {
		t.Fatal(err)
	}
}

const parallelInputVCF = "##fileformat=VCFv4.2\n" +
	"##FORMAT=<ID=GT,Number=1,Type=String,Description=\"Genotype\">\n" +
	"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\tFORMAT\tS1\n" +
	"chr1\t100\t.\tA\tG\t.\t.\t.\tGT\t0/1\n" +
	"chr1\t200\t.\tC\tT\t.\t.\t.\tGT\t1/1\n"

// TestAnnotateVCFParallelMatchesSequential: annotating with the fan-out (threads=4)
// yields the same per-record INFO/FORMAT values and header defs as the sequential
// single pass (threads=1), including the vardist stream and a sample FORMAT builtin.
func TestAnnotateVCFParallelMatchesSequential(t *testing.T) {
	dir := t.TempDir()
	snap := parallelSnapshot(t, dir)
	in := filepath.Join(dir, "in.vcf")
	if err := os.WriteFile(in, []byte(parallelInputVCF), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(threads int) string {
		out := filepath.Join(dir, fmt.Sprintf("out.t%d.vcf", threads))
		if err := AnnotateVCFSnapshot(context.Background(), &config.Config{}, snap, in, out, threads, false, ""); err != nil {
			t.Fatalf("threads=%d: %v", threads, err)
		}
		return out
	}
	seq := summarizeVCF(t, run(1))
	par := summarizeVCF(t, run(4))
	if seq != par {
		t.Errorf("parallel output differs from sequential:\n--- sequential ---\n%s\n--- parallel ---\n%s", seq, par)
	}
	// Sanity: the annotations actually appear (not a vacuous match).
	for _, want := range []string{"CG_TSTV=TS", "CG_VARDIST=100", "SCOREA=0.10", "SCOREB=0.90", "CG_DS="} {
		if !strings.Contains(seq, want) {
			t.Errorf("expected %q in annotated output, got:\n%s", want, seq)
		}
	}
}

// TestAnnotateVCFPerChromParallel: a per-chromosome (multi-file) source fans out to
// one job per chrom file; each file matches only its own records, and the merge
// unions them — matching the sequential multi-file pass.
func TestAnnotateVCFPerChromParallel(t *testing.T) {
	dir := t.TempDir()
	// Two per-chrom files, each holding only its chromosome's variant.
	writeScoreTab(t, filepath.Join(dir, "s.chr1.tab.gz"), []string{"chr1\t100\tA\tG\t0.10"})
	writeScoreTab(t, filepath.Join(dir, "s.chr2.tab.gz"), []string{"chr2\t100\tA\tG\t0.20"})
	snap := &config.Snapshot{
		Name: "r",
		Sources: []config.Source{{
			Name: "perchrom", Format: "tab", RefCol: 3, AltCol: 4,
			LocalPath: filepath.Join(dir, "s.{chrom}.tab.gz"),
			Chroms:    []string{"chr1", "chr2"},
		}},
		Annotations: []config.Annotation{{Name: "SC", Source: "perchrom", Field: "5", Type: "numeric"}},
	}
	in := filepath.Join(dir, "in.vcf")
	if err := os.WriteFile(in, []byte("##fileformat=VCFv4.2\n"+
		"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n"+
		"chr1\t100\t.\tA\tG\t.\t.\t.\n"+
		"chr2\t100\t.\tA\tG\t.\t.\t.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Two chrom files ⇒ the fan-out must produce 2 jobs for this one source.
	if jobs := splitAnnotationJobs(&config.Config{}, snap); len(jobs) != 2 {
		t.Fatalf("per-chrom source split into %d jobs, want 2", len(jobs))
	}

	run := func(threads int) string {
		out := filepath.Join(dir, fmt.Sprintf("out.t%d.vcf", threads))
		if err := AnnotateVCFSnapshot(context.Background(), &config.Config{}, snap, in, out, threads, false, ""); err != nil {
			t.Fatalf("threads=%d: %v", threads, err)
		}
		return out
	}
	seq, par := summarizeVCF(t, run(1)), summarizeVCF(t, run(4))
	if seq != par {
		t.Errorf("per-chrom parallel differs from sequential:\n--seq--\n%s\n--par--\n%s", seq, par)
	}
	// Each record got its own chromosome's score (not blank, not crossed).
	if !strings.Contains(par, "chr1:100:A:G\tINFO[SC=0.10]") || !strings.Contains(par, "chr2:100:A:G\tINFO[SC=0.20]") {
		t.Errorf("per-chrom values wrong:\n%s", par)
	}
}

// summarizeVCF renders a VCF's content order-independently: sorted header INFO+FORMAT
// def IDs, then per record the site plus its INFO (sorted key=value) and each sample's
// FORMAT (sorted key=value). Two VCFs with the same annotations compare equal
// regardless of INFO/def ordering.
func summarizeVCF(t *testing.T, path string) string {
	t.Helper()
	r, err := vcf.NewVcfFile(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer r.Close()
	h, err := r.Header()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	defs := append(append([]string{}, h.InfoIDs()...), h.FormatIDs()...)
	sort.Strings(defs)
	fmt.Fprintf(&b, "defs: %s\n", strings.Join(defs, ","))
	samples := h.Samples()
	for {
		rec, err := r.NextRecord()
		if err != nil {
			break
		}
		info := rec.Info().Keys()
		pairs := make([]string, 0, len(info))
		for _, k := range info {
			v, _ := rec.InfoValue(k)
			pairs = append(pairs, k+"="+v.String())
		}
		sort.Strings(pairs)
		fmt.Fprintf(&b, "%s:%d:%s:%s\tINFO[%s]", rec.Chrom, rec.Pos, rec.Ref, strings.Join(rec.Alt(), ","), strings.Join(pairs, ";"))
		for si := range samples {
			attr, err := rec.Sample(si)
			if err != nil {
				continue
			}
			var sp []string
			for _, k := range attr.Keys() {
				v, _ := attr.Get(k)
				sp = append(sp, k+"="+v.String())
			}
			sort.Strings(sp)
			fmt.Fprintf(&b, "\t%s[%s]", samples[si], strings.Join(sp, ";"))
		}
		b.WriteByte('\n')
	}
	return b.String()
}
