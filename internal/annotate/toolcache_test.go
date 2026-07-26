package annotate

import (
	"testing"

	"github.com/compgenlab/varianthub-cli/internal/model"
)

// TestExpandVariantTemplate covers the input_format per-variant template expansion.
func TestExpandVariantTemplate(t *testing.T) {
	l := model.Locus{Chrom: "chr1", Pos: 100, Ref: "AC", Alt: "G"}
	if got := expandVariantTemplate("{chrom}_{pos}:{ref}>{alt}", l); got != "chr1_100:AC>G" {
		t.Errorf("template = %q, want chr1_100:AC>G", got)
	}
	// pos0 = pos-1 (0-based); end = pos + len(ref) - 1.
	if got := expandVariantTemplate("{chrom}\t{pos0}\t{end}", l); got != "chr1\t99\t101" {
		t.Errorf("template = %q, want chr1\\t99\\t101", got)
	}
}

// TestIsVCFInput: empty/"vcf" mean VCF input; anything else is a template.
func TestIsVCFInput(t *testing.T) {
	for _, f := range []string{"", "vcf"} {
		if !isVCFInput(f) {
			t.Errorf("isVCFInput(%q) = false, want true", f)
		}
	}
	if isVCFInput("{chrom}\t{pos}") {
		t.Error("a template should not be VCF input")
	}
}
