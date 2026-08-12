package main

import (
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-cli/internal/config"
)

func genesSnapshot() *config.Snapshot {
	return &config.Snapshot{
		Name: "test",
		Sources: []config.Source{
			{Name: "clinvar", Version: "2025", Format: "vcf"},
			{Name: "gencode", Version: "47", Format: "gtf"},
			{Name: "gencode", Version: "48", Format: "gtf"},
			// A gene list is named after its GTF often enough that asking for one
			// by the wrong name is a plausible mistake; it has no genes of its own
			// to list.
			{Name: "cancer_genes", Version: "1", Type: "genelist"},
		},
	}
}

func TestGTFSourceByRef(t *testing.T) {
	snap := genesSnapshot()

	// A bare name resolves; ties go to the first in snapshot order, which is the
	// same rule the engine uses to resolve a genelist's `gtf`.
	got, err := gtfSourceByRef(snap, "gencode")
	if err != nil {
		t.Fatalf("gencode: %v", err)
	}
	if got.Version != "47" {
		t.Errorf("gencode resolved to version %q, want the first in snapshot order", got.Version)
	}

	got, err = gtfSourceByRef(snap, "gencode:48")
	if err != nil {
		t.Fatalf("gencode:48: %v", err)
	}
	if got.Version != "48" {
		t.Errorf("gencode:48 resolved to version %q", got.Version)
	}
}

// Asking a VCF for its genes is a mistake in the caller, and an empty list would
// look like a GTF with no genes in it — which is what a gene-list builder would
// then report as "none of your genes exist".
func TestGTFSourceByRefRefusesANonGTFSource(t *testing.T) {
	snap := genesSnapshot()
	for _, ref := range []string{"clinvar", "cancer_genes"} {
		_, err := gtfSourceByRef(snap, ref)
		if err == nil {
			t.Fatalf("%s was accepted as a GTF source", ref)
		}
		if !strings.Contains(err.Error(), "not a GTF source") {
			t.Errorf("%s: error %q does not say why", ref, err)
		}
	}
}

func TestGTFSourceByRefUnknown(t *testing.T) {
	_, err := gtfSourceByRef(genesSnapshot(), "refseq")
	if err == nil || !strings.Contains(err.Error(), "refseq") {
		t.Errorf("error %v does not name the source that was asked for", err)
	}
	// A version that does not exist is as much a miss as a name that does not.
	if _, err := gtfSourceByRef(genesSnapshot(), "gencode:99"); err == nil {
		t.Error("gencode:99 resolved to a source")
	}
}
