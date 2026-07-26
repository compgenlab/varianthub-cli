package vcf

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-cli/internal/model"
)

// TestWriteLoci: WriteLoci emits a sites-only VCF (sorted) that ReadFile parses
// back to the same loci.
func TestWriteLoci(t *testing.T) {
	loci := []model.Locus{
		{Chrom: "chr12", Pos: 25245350, Ref: "C", Alt: "G"},
		{Chrom: "chr1", Pos: 200, Ref: "C", Alt: "T"},
		{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"},
	}
	path := filepath.Join(t.TempDir(), "loci.vcf")
	if err := WriteLoci(path, loci); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Sorted by chrom then pos: chr1:100, chr1:200, chr12:25245350.
	want := []model.Locus{
		{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"},
		{Chrom: "chr1", Pos: 200, Ref: "C", Alt: "T"},
		{Chrom: "chr12", Pos: 25245350, Ref: "C", Alt: "G"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d loci, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("loci[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

func TestReadStripsAndSplits(t *testing.T) {
	// Includes GT/FORMAT/INFO (must be ignored), a multi-allelic site, a
	// header/meta block, lowercase alleles, and a '.' ALT.
	in := `##fileformat=VCFv4.2
##INFO=<ID=AC,Number=A,Type=Integer,Description="x">
#CHROM	POS	ID	REF	ALT	QUAL	FILTER	INFO	FORMAT	S1	S2
chr1	100	.	a	g	.	PASS	AC=2	GT	0/1	1/1
chr1	200	.	C	T,A	.	PASS	AC=1	GT	1/2	0/0
chr2	300	.	G	.	.	PASS	.	GT	0/0	0/0
`
	loci, err := Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(loci))
	for i, l := range loci {
		got[i] = l.Key()
	}
	want := []string{"chr1:100:A:G", "chr1:200:C:T", "chr1:200:C:A"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("locus %d = %q, want %q (REF/ALT uppercased, multi-allelic split, '.' ALT dropped)", i, got[i], want[i])
		}
	}
}

func TestReadBadPos(t *testing.T) {
	_, err := Read(strings.NewReader("chr1\tNOPE\t.\tA\tG\n"))
	if err == nil {
		t.Fatal("expected error for non-numeric POS")
	}
}
