package overlay

import (
	"context"
	"testing"

	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/model"
)

// TestBuiltinSource: the variant-only builtins (auto_id/indel/tstv/tags) compute a
// value per locus off the synthesized record; sample-derived builtins and vardist
// are filtered out (they can't run on a bare locus).
func TestBuiltinSource(t *testing.T) {
	src := config.Source{
		Name: "builtins", Version: "1", Type: "builtin",
		Annotations: []config.Annotation{
			{Builtin: "auto_id", Name: "auto_id"},
			{Builtin: "indel", Name: "indel"},
			{Builtin: "tstv", Name: "tstv"},
			{Builtin: "tags", Name: "PANEL", Args: "PANEL:v1"},
			{Builtin: "dosage", Name: "dosage"},   // sample-derived → skipped
			{Builtin: "vardist", Name: "vardist"}, // needs neighbors → skipped
		},
	}
	s := NewBuiltinSource(src)
	if got := len(s.anns); got != 4 {
		t.Fatalf("variant-only builtins kept = %d, want 4 (auto_id/indel/tstv/tags)", got)
	}
	ctx := context.Background()

	valFor := func(rows []model.AnnRow, key string) (string, bool) {
		for _, r := range rows {
			if r.Key == key {
				return r.Value.String(), true
			}
		}
		return "", false
	}

	// A transition SNV: tstv=TS, no indel value, PANEL constant, auto_id set.
	rows, err := s.Annotate(ctx, []model.Locus{{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := valFor(rows, "tstv"); !ok || v != "TS" {
		t.Errorf("tstv = %q,%v, want TS", v, ok)
	}
	if _, ok := valFor(rows, "indel"); ok {
		t.Errorf("indel should be blank for an SNV")
	}
	if v, ok := valFor(rows, "PANEL"); !ok || v != "v1" {
		t.Errorf("PANEL = %q,%v, want v1", v, ok)
	}
	if v, ok := valFor(rows, "auto_id"); !ok || v == "" {
		t.Errorf("auto_id = %q,%v, want a synthesized id", v, ok)
	}

	// A transversion SNV.
	rows, _ = s.Annotate(ctx, []model.Locus{{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "C"}})
	if v, ok := valFor(rows, "tstv"); !ok || v != "TV" {
		t.Errorf("tstv = %q,%v, want TV", v, ok)
	}

	// An insertion: indel=insertion, tstv blank (indels skip TS/TV).
	rows, _ = s.Annotate(ctx, []model.Locus{{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "AT"}})
	if v, ok := valFor(rows, "indel"); !ok || v != "insertion" {
		t.Errorf("indel = %q,%v, want insertion", v, ok)
	}
	if _, ok := valFor(rows, "tstv"); ok {
		t.Errorf("tstv should be blank for an indel")
	}

	// A deletion.
	rows, _ = s.Annotate(ctx, []model.Locus{{Chrom: "chr1", Pos: 100, Ref: "AT", Alt: "A"}})
	if v, ok := valFor(rows, "indel"); !ok || v != "deletion" {
		t.Errorf("indel = %q,%v, want deletion", v, ok)
	}
}
