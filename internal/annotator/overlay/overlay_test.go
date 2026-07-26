package overlay

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/compgenlab/cghts/htsio/tabix"

	"github.com/compgenlab/varianthub-cli/internal/bbitest"
	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/model"
)

func writeIndexedVCF(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "clinvar.vcf.gz")
	w := tabix.NewWriter(path, tabix.NewWriterOpts().VCF().AutoIndex())
	w.WriteHeader("##fileformat=VCFv4.2")
	w.WriteHeader("#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO")
	for _, line := range []string{
		"chr1\t100\t.\tA\tG\t.\t.\tCLNSIG=Pathogenic;AF=0.01",
		"chr1\t100\t.\tA\tT\t.\t.\tCLNSIG=Benign;AF=0.2",
		"chr1\t250\t.\tC\tT\t.\t.\tCLNSIG=Likely_pathogenic;AF=0.5",
	} {
		if err := w.Write(line); err != nil {
			t.Fatalf("write vcf line: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close vcf: %v", err)
	}
	return path
}

func writeIndexedBED(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "genes.bed.gz")
	w := tabix.NewWriter(path, tabix.NewWriterOpts().BED().AutoIndex())
	for _, line := range []string{
		"chr1\t50\t500\tBRCA1",
		"chr1\t800\t900\tTP53",
	} {
		if err := w.Write(line); err != nil {
			t.Fatalf("write bed line: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close bed: %v", err)
	}
	return path
}

// rowsByKey flattens annotation rows into key→value for assertions.
func rowsByKey(rows []model.AnnRow) map[string]model.Value {
	m := map[string]model.Value{}
	for _, r := range rows {
		m[r.Key] = r.Value
	}
	return m
}

func TestOverlayVCF(t *testing.T) {
	dir := t.TempDir()
	path := writeIndexedVCF(t, dir)
	src := config.Source{Name: "clinvar", Version: "2026-01", Format: "vcf", LocalPath: path}
	anns := []config.Annotation{
		{Name: "clinvar_sig", Source: "clinvar", Field: "CLNSIG", Type: "categorical"},
		{Name: "af", Source: "clinvar", Field: "AF", Type: "numeric"},
	}
	s := NewSource(&config.Config{}, src, []config.SourceFile{{Path: path}}, anns)
	ctx := context.Background()

	t.Run("exact match A>G", func(t *testing.T) {
		rows, err := s.Annotate(ctx, []model.Locus{{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}})
		if err != nil {
			t.Fatal(err)
		}
		m := rowsByKey(rows)
		if got := m["clinvar_sig"].Str; got != "Pathogenic" {
			t.Errorf("clinvar_sig = %q, want Pathogenic", got)
		}
		if v := m["af"]; !v.IsNum || v.Num != 0.01 {
			t.Errorf("af = %+v, want numeric 0.01", v)
		}
	})

	t.Run("alt discriminates A>T", func(t *testing.T) {
		rows, err := s.Annotate(ctx, []model.Locus{{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "T"}})
		if err != nil {
			t.Fatal(err)
		}
		if got := rowsByKey(rows)["clinvar_sig"].Str; got != "Benign" {
			t.Errorf("clinvar_sig = %q, want Benign (the A>T record)", got)
		}
	})

	t.Run("no match → no rows", func(t *testing.T) {
		rows, err := s.Annotate(ctx, []model.Locus{{Chrom: "chr1", Pos: 999, Ref: "A", Alt: "G"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 0 {
			t.Errorf("want 0 rows for non-existent locus, got %d", len(rows))
		}
	})
}

// writeChromVCF writes a one-record bgzipped+indexed VCF for a single chromosome.
func writeChromVCF(t *testing.T, dir, chrom, sig string) string {
	t.Helper()
	path := filepath.Join(dir, chrom+".vcf.gz")
	w := tabix.NewWriter(path, tabix.NewWriterOpts().VCF().AutoIndex())
	w.WriteHeader("##fileformat=VCFv4.2")
	w.WriteHeader("#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO")
	if err := w.Write(chrom + "\t100\t.\tA\tG\t.\t.\tCLNSIG=" + sig); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestOverlayMultiFile: a per-chrom source routes each locus to its chromosome's
// file, and a chromosome with no file yields nothing.
func TestOverlayMultiFile(t *testing.T) {
	dir := t.TempDir()
	files := []config.SourceFile{
		{Chrom: "chr1", Path: writeChromVCF(t, dir, "chr1", "Pathogenic")},
		{Chrom: "chr2", Path: writeChromVCF(t, dir, "chr2", "Benign")},
	}
	src := config.Source{Name: "split", Version: "1", Format: "vcf", URL: "https://x/{chrom}.vcf.gz", Chroms: []string{"chr1", "chr2"}}
	anns := []config.Annotation{{Name: "clinvar_sig", Source: "split", Field: "CLNSIG", Type: "categorical"}}
	s := NewSource(&config.Config{}, src, files, anns)
	ctx := context.Background()

	for _, tc := range []struct{ chrom, want string }{{"chr1", "Pathogenic"}, {"chr2", "Benign"}} {
		rows, err := s.Annotate(ctx, []model.Locus{{Chrom: tc.chrom, Pos: 100, Ref: "A", Alt: "G"}})
		if err != nil {
			t.Fatalf("%s: %v", tc.chrom, err)
		}
		if got := rowsByKey(rows)["clinvar_sig"].Str; got != tc.want {
			t.Errorf("%s clinvar_sig = %q, want %q", tc.chrom, got, tc.want)
		}
	}
	// A chromosome with no file annotates nothing (no error).
	rows, err := s.Annotate(ctx, []model.Locus{{Chrom: "chr3", Pos: 100, Ref: "A", Alt: "G"}})
	if err != nil {
		t.Fatalf("chr3: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("chr3 (no file) returned %d rows, want 0", len(rows))
	}
}

// writeVCFRecord writes a one-record bgzipped+indexed VCF.
func writeVCFRecord(t *testing.T, dir, name, record string) string {
	t.Helper()
	path := filepath.Join(dir, name+".vcf.gz")
	w := tabix.NewWriter(path, tabix.NewWriterOpts().VCF().AutoIndex())
	w.WriteHeader("##fileformat=VCFv4.2")
	w.WriteHeader("#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO")
	if err := w.Write(record); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestOverlayUnionFiles: two files for one source at the same chrom/pos, split by
// variant type (ref/alt). Both files are queried; ref/alt picks the match.
func TestOverlayUnionFiles(t *testing.T) {
	dir := t.TempDir()
	files := []config.SourceFile{
		{Path: writeVCFRecord(t, dir, "coding", "chr1\t100\t.\tA\tG\t.\t.\tCLNSIG=Coding")},
		{Path: writeVCFRecord(t, dir, "indels", "chr1\t100\t.\tAT\tA\t.\t.\tCLNSIG=Indel")},
	}
	src := config.Source{Name: "split", Version: "1", Format: "vcf"}
	anns := []config.Annotation{{Name: "clinvar_sig", Source: "split", Field: "CLNSIG", Type: "categorical"}}
	s := NewSource(&config.Config{}, src, files, anns)
	ctx := context.Background()

	for _, tc := range []struct{ ref, alt, want string }{{"A", "G", "Coding"}, {"AT", "A", "Indel"}} {
		rows, err := s.Annotate(ctx, []model.Locus{{Chrom: "chr1", Pos: 100, Ref: tc.ref, Alt: tc.alt}})
		if err != nil {
			t.Fatalf("%s>%s: %v", tc.ref, tc.alt, err)
		}
		if got := rowsByKey(rows)["clinvar_sig"].Str; got != tc.want {
			t.Errorf("%s>%s clinvar_sig = %q, want %q", tc.ref, tc.alt, got, tc.want)
		}
	}
}

func writeIndexedTab(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "revel.txt.gz")
	// columns: 1=chrom 2=pos 3=ref 4=alt 5=score
	w := tabix.NewWriter(path, tabix.NewWriterOpts().Columns(1, 2, 0).AutoIndex())
	for _, line := range []string{
		"chr1\t100\tA\tG\t0.95",
		"chr1\t100\tA\tT\t0.10",
	} {
		if err := w.Write(line); err != nil {
			t.Fatalf("write tab: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close tab: %v", err)
	}
	return path
}

func TestOverlayTabAlleleMatch(t *testing.T) {
	dir := t.TempDir()
	path := writeIndexedTab(t, dir)
	src := config.Source{Name: "revel", Version: "1", Format: "tab", LocalPath: path, RefCol: 3, AltCol: 4}
	anns := []config.Annotation{{Name: "revel", Source: "revel", Field: "5", Type: "numeric"}}
	s := NewSource(&config.Config{}, src, []config.SourceFile{{Path: path}}, anns)
	ctx := context.Background()

	for _, tc := range []struct {
		alt  string
		want float64
	}{{"G", 0.95}, {"T", 0.10}} {
		rows, err := s.Annotate(ctx, []model.Locus{{Chrom: "chr1", Pos: 100, Ref: "A", Alt: tc.alt}})
		if err != nil {
			t.Fatal(err)
		}
		if v := rowsByKey(rows)["revel"]; !v.IsNum || v.Num != tc.want {
			t.Errorf("A>%s revel = %+v, want numeric %v", tc.alt, v, tc.want)
		}
	}
}

func TestOverlayVCFKnobs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "clinvar.vcf.gz")
	w := tabix.NewWriter(path, tabix.NewWriterOpts().VCF().AutoIndex())
	w.WriteHeader("##fileformat=VCFv4.2")
	w.WriteHeader("#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO")
	for _, line := range []string{
		"chr1\t100\trs1\tA\tG\t.\t.\tCLNSIG=Pathogenic",
		"chr1\t100\trs2\tA\tT\t.\t.\tCLNSIG=Benign",
	} {
		if err := w.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	src := config.Source{Name: "clinvar", Version: "1", Format: "vcf", LocalPath: path}
	anns := []config.Annotation{
		{Name: "clinvar_match", Source: "clinvar", Type: "flag"},                       // exact-match flag
		{Name: "clinvar_pos", Source: "clinvar", Type: "flag", Match: "position"},      // position flag
		{Name: "dbsnp_id", Source: "clinvar", Field: "@ID", Unique: true},              // copy source ID
		{Name: "clinvar_sig", Source: "clinvar", Field: "CLNSIG", Type: "categorical"}, // exact value
	}
	s := NewSource(&config.Config{}, src, []config.SourceFile{{Path: path}}, anns)

	// Exact A>G: all four fire.
	m := rowsByKey(mustAnnotate(t, s, model.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}))
	// A present flag renders as its tag name (not "true").
	if m["clinvar_match"].Str != "clinvar_match" || m["clinvar_pos"].Str != "clinvar_pos" {
		t.Errorf("flags: match=%q pos=%q, want the tag names", m["clinvar_match"].Str, m["clinvar_pos"].Str)
	}
	if m["dbsnp_id"].Str != "rs1" {
		t.Errorf("dbsnp_id = %q, want rs1", m["dbsnp_id"].Str)
	}
	if m["clinvar_sig"].Str != "Pathogenic" {
		t.Errorf("clinvar_sig = %q, want Pathogenic", m["clinvar_sig"].Str)
	}

	// A>C (no exact allele in file): exact ones absent, position flag still present.
	m2 := rowsByKey(mustAnnotate(t, s, model.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "C"}))
	if _, ok := m2["clinvar_match"]; ok {
		t.Error("clinvar_match should be absent for a non-matching allele (exact)")
	}
	if _, ok := m2["clinvar_sig"]; ok {
		t.Error("clinvar_sig should be absent for a non-matching allele (exact)")
	}
	if m2["clinvar_pos"].Str != "clinvar_pos" {
		t.Errorf("clinvar_pos = %q, want the tag name (position match)", m2["clinvar_pos"].Str)
	}
}

func mustAnnotate(t *testing.T, s *Source, l model.Locus) []model.AnnRow {
	t.Helper()
	rows, err := s.Annotate(context.Background(), []model.Locus{l})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestOverlayBED(t *testing.T) {
	dir := t.TempDir()
	path := writeIndexedBED(t, dir)
	src := config.Source{Name: "gencode", Version: "45", Format: "bed", LocalPath: path}
	anns := []config.Annotation{{Name: "gene", Source: "gencode", Field: "name", Type: "categorical"}}
	s := NewSource(&config.Config{}, src, []config.SourceFile{{Path: path}}, anns)
	ctx := context.Background()

	rows, err := s.Annotate(ctx, []model.Locus{{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowsByKey(rows)["gene"].Str; got != "BRCA1" {
		t.Errorf("gene = %q, want BRCA1 (overlap)", got)
	}

	rows, err = s.Annotate(ctx, []model.Locus{{Chrom: "chr1", Pos: 600, Ref: "A", Alt: "G"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Errorf("want no gene for non-overlapping locus, got %d rows", len(rows))
	}
}

// TestOverlayAutoConvertChromNaming proves chrom-naming auto-conversion: the source
// uses UCSC "chr1" while the query locus uses Ensembl "1" — AutoConvert (built from
// the source file's ref names) should still match.
func TestOverlayAutoConvertChromNaming(t *testing.T) {
	dir := t.TempDir()
	path := writeIndexedVCF(t, dir) // source contigs are "chr1"
	src := config.Source{Name: "clinvar", Version: "1", Format: "vcf", LocalPath: path}
	anns := []config.Annotation{{Name: "clinvar_sig", Source: "clinvar", Field: "CLNSIG", Type: "categorical"}}
	s := NewSource(&config.Config{}, src, []config.SourceFile{{Path: path}}, anns)

	rows, err := s.Annotate(context.Background(), []model.Locus{{Chrom: "1", Pos: 100, Ref: "A", Alt: "G"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowsByKey(rows)["clinvar_sig"].Str; got != "Pathogenic" {
		t.Errorf("Ensembl locus 1:100 against UCSC source chr1: clinvar_sig = %q, want Pathogenic", got)
	}
}

// TestOverlayBigWig: a single bigWig source yields the base value at the position.
func TestOverlayBigWig(t *testing.T) {
	dir := t.TempDir()
	bw := filepath.Join(dir, "cons.bw")
	if err := bbitest.WriteBigWig(bw, "chr1", []bbitest.WigItem{
		{Start: 99, End: 100, Val: 0.42},
		{Start: 199, End: 200, Val: 9.0},
	}); err != nil {
		t.Fatal(err)
	}
	src := config.Source{Name: "cons", Version: "1", Format: "bigwig", LocalPath: bw}
	anns := []config.Annotation{{Name: "cons_score", Source: "cons", Type: "numeric"}}
	s := NewSource(&config.Config{}, src, []config.SourceFile{{Path: bw}}, anns)
	ctx := context.Background()

	rows, err := s.Annotate(ctx, []model.Locus{{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowsByKey(rows)["cons_score"].Num; got != 0.42 {
		t.Errorf("cons_score = %v, want 0.42", got)
	}
	if rows, _ = s.Annotate(ctx, []model.Locus{{Chrom: "chr1", Pos: 150, Ref: "A", Alt: "G"}}); len(rows) != 0 {
		t.Errorf("gap should yield no value, got %d rows", len(rows))
	}
}

// TestOverlayBigBed: a single bigBed source yields a column from the overlap.
func TestOverlayBigBed(t *testing.T) {
	dir := t.TempDir()
	bb := filepath.Join(dir, "clinvar.bb")
	if err := bbitest.WriteBigBed(bb, "chr1", []bbitest.BedItem{{Start: 100, End: 200, Rest: "Pathogenic\t5"}}); err != nil {
		t.Fatal(err)
	}
	src := config.Source{Name: "clinvar", Version: "1", Format: "bigbed", LocalPath: bb}
	anns := []config.Annotation{{Name: "sig", Source: "clinvar", Field: "name", Type: "categorical"}}
	s := NewSource(&config.Config{}, src, []config.SourceFile{{Path: bb}}, anns)

	rows, err := s.Annotate(context.Background(), []model.Locus{{Chrom: "chr1", Pos: 150, Ref: "A", Alt: "G"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowsByKey(rows)["sig"].Str; got != "Pathogenic" {
		t.Errorf("sig = %q, want Pathogenic", got)
	}
}

// TestOverlayPerAltBigWig: a per-alt bigWig set ({alt} → a/c/g/t) routes each
// variant to the file for its alt base (the allele-specific score).
func TestOverlayPerAltBigWig(t *testing.T) {
	dir := t.TempDir()
	vals := map[string]float32{"a": 0.1, "c": 0.2, "g": 0.3, "t": 0.4}
	var files []config.SourceFile
	for _, alt := range []string{"a", "c", "g", "t"} {
		p := filepath.Join(dir, alt+".bw")
		if err := bbitest.WriteBigWig(p, "chr1", []bbitest.WigItem{{Start: 149, End: 150, Val: vals[alt]}}); err != nil {
			t.Fatal(err)
		}
		files = append(files, config.SourceFile{Path: p, Alt: alt})
	}
	src := config.Source{Name: "am", Version: "1", Format: "bigwig", LocalPath: filepath.Join(dir, "{alt}.bw")}
	anns := []config.Annotation{{Name: "am_score", Source: "am", Type: "numeric"}}
	s := NewSource(&config.Config{}, src, files, anns)
	ctx := context.Background()

	// alt G → g.bw = 0.3
	rows, err := s.Annotate(ctx, []model.Locus{{Chrom: "chr1", Pos: 150, Ref: "A", Alt: "G"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rowsByKey(rows)["am_score"].Num; got != 0.3 {
		t.Errorf("alt G am_score = %v, want 0.3", got)
	}
	// alt T → t.bw = 0.4
	rows, _ = s.Annotate(ctx, []model.Locus{{Chrom: "chr1", Pos: 150, Ref: "A", Alt: "T"}})
	if got := rowsByKey(rows)["am_score"].Num; got != 0.4 {
		t.Errorf("alt T am_score = %v, want 0.4", got)
	}
	// an indel alt (multi-base) matches no per-alt file → no value
	if rows, _ = s.Annotate(ctx, []model.Locus{{Chrom: "chr1", Pos: 150, Ref: "A", Alt: "AT"}}); len(rows) != 0 {
		t.Errorf("indel should get no per-alt value, got %d rows", len(rows))
	}
}
