package service

import (
	"context"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/model"
)

// toolSnapshot is a snapshot pinning one tool source whose annotation is
// selected — the shape that must always run the tool.
func toolSnapshot() (*config.Snapshot, []config.Annotation) {
	src := config.Source{
		Name: "vep", Version: "113", Type: "tool",
		// A software requirement nothing in a test environment satisfies, so an
		// attempt to run is observable as a specific failure rather than needing
		// a container runtime.
		Requires: []string{"definitely-not-installed-xyz"},
	}
	snap := &config.Snapshot{
		Name: "s", Assembly: "GRCh38",
		Sources:     []config.Source{src},
		Annotations: []config.Annotation{{Name: "VEP_Consequence", Source: "vep"}},
	}
	return snap, snap.Annotations
}

// A referenced tool runs even when there is no cache to put its output in.
//
// This is the bug this test exists for. The guard used to be
//
//	len(tools) > 0 && (toolStore != nil || skipToolCache)
//
// so a deployment with no cache configured — a nil store — silently skipped
// every tool source on the per-locus path. The file-based sources annotated
// normally and VEP's columns came back empty, which is indistinguishable in the
// output from VEP having nothing to say about the variant.
//
// "It ran" is asserted through the software preflight failing: reaching that
// error means the tool was launched, and a nil error would mean it was skipped.
func TestAReferencedToolRunsWithNoCacheStore(t *testing.T) {
	snap, selected := toolSnapshot()
	cfg := &config.Config{}
	cfg.SetBaseDir(t.TempDir())
	loci := []model.Locus{{Chrom: "chr12", Pos: 25245350, Ref: "C", Alt: "T"}}

	// st is nil: no cache configured, which is how varianthub-web materializes a
	// job's config today.
	_, cleanup, err := buildEngineForLoci(context.Background(), cfg, snap, nil, selected, loci, false)
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil {
		t.Fatal("no error: the tool source was skipped rather than run. " +
			"A nil store means no cache, not no tools.")
	}
	if !strings.Contains(err.Error(), "definitely-not-installed-xyz") {
		t.Fatalf("err = %v, want the software preflight for the tool — "+
			"the failure should come from trying to run it", err)
	}
}

// And with a bulk-VCF style bypass, which always worked.
func TestAReferencedToolRunsWhenBypassingTheCache(t *testing.T) {
	snap, selected := toolSnapshot()
	cfg := &config.Config{}
	cfg.SetBaseDir(t.TempDir())
	loci := []model.Locus{{Chrom: "chr12", Pos: 25245350, Ref: "C", Alt: "T"}}

	_, cleanup, err := buildEngineForLoci(context.Background(), cfg, snap, nil, selected, loci, true)
	if cleanup != nil {
		defer cleanup()
	}
	if err == nil || !strings.Contains(err.Error(), "definitely-not-installed-xyz") {
		t.Fatalf("err = %v, want the tool's software preflight", err)
	}
}

// A tool nobody selected still does not run. The selection-awareness is the
// reason an expensive tool is not launched for every annotation, and widening
// the guard must not have widened that too.
func TestAnUnreferencedToolDoesNotRun(t *testing.T) {
	snap, _ := toolSnapshot()
	// Select something else entirely.
	other := []config.Annotation{{Name: "REVEL", Source: "revel"}}

	cfg := &config.Config{}
	cfg.SetBaseDir(t.TempDir())
	loci := []model.Locus{{Chrom: "chr12", Pos: 25245350, Ref: "C", Alt: "T"}}

	_, cleanup, err := buildEngineForLoci(context.Background(), cfg, snap, nil, other, loci, false)
	if cleanup != nil {
		defer cleanup()
	}
	if err != nil && strings.Contains(err.Error(), "definitely-not-installed-xyz") {
		t.Fatal("an unselected tool source was launched")
	}
}
