package config

import (
	"strings"
	"testing"
)

// An annotation name is written into the output as a VCF INFO key, verbatim, so
// a name that is not a legal key is refused where it is declared.
//
// The alternative is a file that a strict parser rejects and a lenient one reads
// as something else, failing at the far end of somebody's pipeline in a tool
// nobody here runs — a very long way from the manifest that caused it.
func TestAnAnnotationNameMustBeAValidINFOKey(t *testing.T) {
	for _, tc := range []struct {
		name string
		ok   bool
		why  string
	}{
		{"clinvar_sig", true, "the ordinary case"},
		{"PANEL", true, "upper case is fine"},
		{"vep.consequence", true, "a dot is legal after the first character"},
		{"_private", true, "a leading underscore is legal"},
		{"revel1", true, "a digit is legal after the first character"},

		{"gnomAD-AF", false, "a hyphen is not a legal INFO character"},
		{"1000g_af", false, "a leading digit is not legal"},
		{".hidden", false, "a leading dot is not legal"},
		{"has space", false, "whitespace ends the key"},
		{"semi;colon", false, "a semicolon separates INFO fields"},
		{"equals=sign", false, "an equals separates key from value"},
		{"comma,sep", false, "a comma separates values within a field"},
		{"", false, "a name is required"},
	} {
		if got := ValidAnnotationName(tc.name); got != tc.ok {
			t.Errorf("ValidAnnotationName(%q) = %v, want %v — %s", tc.name, got, tc.ok, tc.why)
		}
	}
}

// The refusal names a legal alternative, because the fix is always a rename.
func TestTheRefusalSuggestsALegalName(t *testing.T) {
	err := checkAnnotationName("gnomAD-AF")
	if err == nil {
		t.Fatal("a hyphenated name was accepted")
	}
	if !strings.Contains(err.Error(), "gnomAD_AF") {
		t.Errorf("the error does not suggest a legal name: %v", err)
	}
}

// It is refused through snapshot validation, which is what a manifest actually
// goes through — not only by the helper.
func TestASnapshotWithAnIllegalAnnotationNameIsRefused(t *testing.T) {
	snap := &Snapshot{Sources: []Source{{
		Name: "gnomad", Version: "4.1", Format: "vcf", URL: "https://example.com/g.vcf.gz",
		Annotations: []Annotation{{Name: "gnomAD-AF", Field: "AF", Type: "numeric"}},
	}}}
	snap.normalize()
	err := snap.validate()
	if err == nil {
		t.Fatal("a snapshot declaring an illegal INFO key validated")
	}
	if !strings.Contains(err.Error(), "INFO key") {
		t.Errorf("the error does not say what is wrong: %v", err)
	}
}

// And a builtin's name is held to the same rule, though it takes a different
// validation path — it has no field to read, so it never reaches
// validateFileAnnotation.
func TestABuiltinWithAnIllegalNameIsRefused(t *testing.T) {
	snap := &Snapshot{Sources: []Source{{
		Name: "builtins", Version: "1", Type: "builtin",
		Annotations: []Annotation{{Name: "auto-id", Builtin: "auto_id"}},
	}}}
	snap.normalize()
	if err := snap.validate(); err == nil {
		t.Fatal("a builtin declaring an illegal INFO key validated")
	}
}

// The names the shipped examples use all pass, so the rule does not invalidate
// the manifests this repo tells people to copy.
func TestTheExampleAnnotationNamesAreLegal(t *testing.T) {
	for _, name := range []string{
		"clinvar_sig", "clinvar_pos", "gnomad_af", "gnomad_af_nfe", "dbsnp_id",
		"revel", "vep_consequence", "vep_impact", "vep_symbol",
		"auto_id", "indel", "tstv", "PANEL",
	} {
		if !ValidAnnotationName(name) {
			t.Errorf("the shipped example name %q is refused by the new rule", name)
		}
	}
}
