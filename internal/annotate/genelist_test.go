package annotate

import (
	"testing"

	"github.com/compgenlab/cghts/bed"
	"github.com/compgenlab/cghts/gtf"
	"github.com/compgenlab/cghts/vcf"

	"github.com/compgenlab/cganno/internal/config"
)

// fakeModel is a geneModel that returns preset genes for chr1 regardless of range.
type fakeModel struct{ genes []*gtf.Gene }

func (m fakeModel) FindGenes(ref string, _, _ int) []*gtf.Gene {
	if ref == "chr1" {
		return m.genes
	}
	return nil
}
func (m fakeModel) FindGenicRegionForPos(string, int, bed.Strand, string) gtf.GenicRegion {
	return gtf.GenicRegion{}
}
func (m fakeModel) RefNames() []string { return []string{"chr1"} }

func newTestGeneList(genes map[string]bool, model geneModel) *geneListAnnotator {
	return &geneListAnnotator{
		model: model,
		conv:  vcf.NewContigConverter([]string{"chr1"}),
		genes: genes,
		anns:  []config.Annotation{{Name: "germline_cancer_gene", Type: "flag"}},
	}
}

func TestGeneListFlagsInListGene(t *testing.T) {
	model := fakeModel{genes: []*gtf.Gene{{GeneName: "BRCA1", GeneID: "ENSG1"}}}
	a := newTestGeneList(map[string]bool{"BRCA1": true, "TP53": true}, model)

	rec := vcf.NewRecord("chr1", 100, "A", []string{"G"})
	if err := a.Annotate(rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.InfoValue("germline_cancer_gene"); !ok {
		t.Errorf("variant in BRCA1 should be flagged, but the INFO flag is absent")
	}
}

func TestGeneListNoFlagWhenNotInList(t *testing.T) {
	model := fakeModel{genes: []*gtf.Gene{{GeneName: "EGFR", GeneID: "ENSG2"}}}
	a := newTestGeneList(map[string]bool{"BRCA1": true}, model)

	rec := vcf.NewRecord("chr1", 100, "A", []string{"G"})
	if err := a.Annotate(rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.InfoValue("germline_cancer_gene"); ok {
		t.Errorf("variant in EGFR (not listed) should not be flagged")
	}
}

func TestGeneListMatchesGeneID(t *testing.T) {
	model := fakeModel{genes: []*gtf.Gene{{GeneName: "BRCA1", GeneID: "ENSG00000012048"}}}
	a := newTestGeneList(map[string]bool{"ENSG00000012048": true}, model)
	a.useID = true

	rec := vcf.NewRecord("chr1", 100, "A", []string{"G"})
	if err := a.Annotate(rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.InfoValue("germline_cancer_gene"); !ok {
		t.Errorf("gene_id match should flag the variant")
	}
}

func TestGeneListNoGeneOverlap(t *testing.T) {
	a := newTestGeneList(map[string]bool{"BRCA1": true}, fakeModel{genes: nil})
	rec := vcf.NewRecord("chr1", 100, "A", []string{"G"})
	if err := a.Annotate(rec); err != nil {
		t.Fatal(err)
	}
	if _, ok := rec.InfoValue("germline_cancer_gene"); ok {
		t.Errorf("no overlapping gene → no flag")
	}
}
