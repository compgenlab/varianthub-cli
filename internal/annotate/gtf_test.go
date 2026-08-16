package annotate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/cghts/vcf"

	"github.com/compgenlab/varianthub-cli/internal/config"
)

// gtfFixture: GeneA (+, coding) over [100,200) and GeneB (-, non-coding lncRNA)
// over [150,250) overlap at [150,200).
const gtfFixture = `chr1	t	exon	101	200	.	+	.	gene_id "GeneA"; gene_name "GeneA"; transcript_id "TA"; gene_type "protein_coding";
chr1	t	CDS	101	200	.	+	0	gene_id "GeneA"; gene_name "GeneA"; transcript_id "TA"; gene_type "protein_coding";
chr1	t	exon	151	250	.	-	.	gene_id "GeneB"; gene_name "GeneB"; transcript_id "TB"; gene_type "lncRNA";
`

// TestSourceAnnotatorsGTF covers the cache/locus path (annotator/overlay), which
// builds annotators via SourceAnnotators rather than the streaming pipeline. A gtf
// source must yield ONE grouped annotator that writes every selected field — the
// regression for `varhub annotate <locus>` failing with "unsupported format gtf".
func TestSourceAnnotatorsGTF(t *testing.T) {
	dir := t.TempDir()
	gtfPath := filepath.Join(dir, "genes.gtf")
	if err := os.WriteFile(gtfPath, []byte(gtfFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{CacheDir: t.TempDir()}
	src := config.Source{Name: "gencode", Version: "38", Format: "gtf", LocalPath: gtfPath}
	anns := []config.Annotation{
		{Name: "gene", Source: "gencode", Field: "GENE", Type: "text"},
		{Name: "region", Source: "gencode", Field: "REGION", Type: "categorical"},
	}
	got, err := SourceAnnotators(cfg, src, anns, []config.SourceFile{{Path: gtfPath}})
	if err != nil {
		t.Fatalf("SourceAnnotators: %v", err)
	}
	if len(got) != 1 { // grouped: the GTF is parsed once for all fields
		t.Fatalf("got %d annotators, want 1 (grouped)", len(got))
	}
	defer got[0].Close()

	rec := vcf.NewRecord("chr1", 120, "A", []string{"G"}) // inside GeneA only
	if err := got[0].Annotate(rec); err != nil {
		t.Fatal(err)
	}
	if v, ok := rec.InfoValue("gene"); !ok || v.String() != "GeneA" {
		t.Errorf("gene = %q (ok=%v), want GeneA", v.String(), ok)
	}
	if _, ok := rec.InfoValue("region"); !ok {
		t.Error("region not written")
	}
}

// refseqBiotypeFixture mirrors a coordinate-sorted RefSeq GTF for one gene: the
// gene_biotype lives only on the "gene" feature line, and a transcript row for the
// same gene_id sorts ahead of it (as CECR2 does after cgkit tab-sort). The gene
// must still resolve BIOTYPE=protein_coding rather than the empty biotype of the
// row that happened to seed it first. Regression for cghts v0.6.3.
const refseqBiotypeFixture = `chr1	BestRefSeq	transcript	101	800	.	+	.	gene_id "GeneR"; transcript_id "NM_1"; gene "GeneR"; transcript_biotype "mRNA";
chr1	BestRefSeq	exon	101	200	.	+	.	gene_id "GeneR"; transcript_id "NM_1"; gene "GeneR"; transcript_biotype "mRNA";
chr1	BestRefSeq	CDS	101	200	.	+	0	gene_id "GeneR"; transcript_id "NM_1"; gene "GeneR"; transcript_biotype "mRNA";
chr1	BestRefSeq,Gnomon	gene	101	800	.	+	.	gene_id "GeneR"; gene "GeneR"; gene_biotype "protein_coding";
`

func TestSourceAnnotatorsGTFRefSeqBiotype(t *testing.T) {
	dir := t.TempDir()
	gtfPath := filepath.Join(dir, "refseq.gtf")
	if err := os.WriteFile(gtfPath, []byte(refseqBiotypeFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{CacheDir: t.TempDir()}
	src := config.Source{Name: "refseq", Version: "2024_08", Format: "gtf", LocalPath: gtfPath}
	anns := []config.Annotation{{Name: "biotype", Source: "refseq", Field: "BIOTYPE", Type: "categorical"}}
	got, err := SourceAnnotators(cfg, src, anns, []config.SourceFile{{Path: gtfPath}})
	if err != nil {
		t.Fatalf("SourceAnnotators: %v", err)
	}
	defer got[0].Close()

	rec := vcf.NewRecord("chr1", 150, "A", []string{"G"}) // inside GeneR
	if err := got[0].Annotate(rec); err != nil {
		t.Fatal(err)
	}
	if v, ok := rec.InfoValue("biotype"); !ok || v.String() != "protein_coding" {
		t.Errorf("biotype = %q (ok=%v), want protein_coding", v.String(), ok)
	}
}

func TestBuildPipelineGTF(t *testing.T) {
	dir := t.TempDir()
	gtfPath := filepath.Join(dir, "genes.gtf")
	if err := os.WriteFile(gtfPath, []byte(gtfFixture), 0o644); err != nil {
		t.Fatal(err)
	}

	snap := &config.Snapshot{
		Name:    "2026-06",
		Sources: []config.Source{{Name: "gencode", Version: "38", Format: "gtf", LocalPath: gtfPath}},
		Annotations: []config.Annotation{
			{Name: "gene", Source: "gencode", Field: "GENE", Type: "text"},
			{Name: "region", Source: "gencode", Field: "REGION", Type: "categorical"},
			{Name: "coding", Source: "gencode", Field: "CODING", Type: "text"},
		},
	}

	cfg := &config.Config{CacheDir: t.TempDir()}
	p, err := BuildPipeline(cfg, snap, func(s config.Source) []config.SourceFile {
		return []config.SourceFile{{Path: s.LocalPath}}
	})
	if err != nil {
		t.Fatal(err)
	}
	// Three GTF fields → ONE grouped annotator (the GTF is parsed once).
	if p.Len() != 1 {
		t.Fatalf("pipeline length = %d, want 1 (fields should share one annotator)", p.Len())
	}

	in := filepath.Join(dir, "in.vcf")
	if err := os.WriteFile(in, []byte(
		"##fileformat=VCFv4.2\n"+
			"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n"+
			"chr1\t120\t.\tA\tG\t.\t.\t.\n"+ // GeneA only
			"chr1\t170\t.\tA\tG\t.\t.\t.\n"+ // GeneA + GeneB
			"chr1\t9000\t.\tA\tG\t.\t.\t.\n", // intergenic
	), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.vcf")
	if err := AnnotateVCF(context.Background(), p, in, out, ""); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}

	recs := map[string]string{}
	for _, ln := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(ln, "chr1\t") {
			f := strings.SplitN(ln, "\t", 3)
			recs[f[1]] = ln
		}
	}

	// Record at 120: GeneA only.
	for _, want := range []string{"gene=GeneA", "region=coding_exon", "coding=GeneA"} {
		if !strings.Contains(recs["120"], want) {
			t.Errorf("rec@120 missing %q: %s", want, recs["120"])
		}
	}
	// Record at 170: both genes, comma-joined in start order; coding only GeneA.
	for _, want := range []string{"gene=GeneA,GeneB", "region=coding_exon,nc_exon", "coding=GeneA"} {
		if !strings.Contains(recs["170"], want) {
			t.Errorf("rec@170 missing %q: %s", want, recs["170"])
		}
	}
	// Record at 9000: intergenic — no GTF fields.
	if strings.Contains(recs["9000"], "gene=") || strings.Contains(recs["9000"], "region=") {
		t.Errorf("rec@9000 should have no GTF fields: %s", recs["9000"])
	}

	// The selected-out NONCODING/GENEID/etc. must not appear.
	if strings.Contains(string(data), "GTF_") || strings.Contains(string(data), "noncoding=") {
		t.Errorf("unexpected unselected fields in output:\n%s", data)
	}
}

func TestGTFHeaderDefs(t *testing.T) {
	dir := t.TempDir()
	gtfPath := filepath.Join(dir, "g.gtf")
	if err := os.WriteFile(gtfPath, []byte(gtfFixture), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{CacheDir: t.TempDir()}
	a, err := buildGTF(
		cfg,
		config.Source{Name: "g", Version: "1", Format: "gtf", LocalPath: gtfPath},
		[]config.Annotation{{Name: "gene", Field: "GENE"}, {Name: "rgn", Field: "region"}},
		[]config.SourceFile{{Path: gtfPath}},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// Both fields are declared, under the names the manifest gave them — and a
	// lower-case `field` still resolves, so "region" selects REGION.
	//
	// Asserted through the header the annotator writes rather than through its
	// internals: the annotator lives in cghts now, and what matters here is what
	// this service's configuration produces, not how the library stores it.
	h := vcf.NewVcfHeader()
	if err := a.SetupHeader(h); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"gene", "rgn"} {
		if _, ok := h.InfoDef(want); !ok {
			t.Errorf("%q was configured but not declared; header has %v", want, h.InfoIDs())
		}
	}
	// And nothing the manifest did not ask for.
	for _, unwanted := range []string{"GTF_GENE", "GTF_REGION", "GTF_STRAND", "GTF_GENEID"} {
		if _, ok := h.InfoDef(unwanted); ok {
			t.Errorf("%q was declared though no annotation selected it", unwanted)
		}
	}
}
