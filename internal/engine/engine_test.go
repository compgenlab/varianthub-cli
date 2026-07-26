package engine

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/compgenlab/varianthub-cli/internal/model"
	"github.com/compgenlab/varianthub-cli/internal/store/sqlite"
)

// countingAnnotator records how many times it is invoked, to prove the
// DB-as-cache memoizes (a cached locus must not re-invoke the annotator).
type countingAnnotator struct{ calls int }

func (c *countingAnnotator) Annotate(_ context.Context, loci []model.Locus) ([]model.AnnRow, error) {
	c.calls++
	var rows []model.AnnRow
	for _, l := range loci {
		rows = append(rows, model.AnnRow{Locus: l, DataSource: "t:1", Key: "gene", Value: model.Text("BRCA1")})
	}
	return rows, nil
}

func newEngine(t *testing.T) (*Engine, *countingAnnotator) {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ann := &countingAnnotator{}
	e := New(st, ann, "2026-06", "GRCh38", []model.DataSource{{Name: "t", Version: "1"}})
	if err := e.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e, ann
}

func TestAnnotateCachesAndStampsSnapshot(t *testing.T) {
	e, ann := newEngine(t)
	ctx := context.Background()
	loci := []model.Locus{{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}}

	r1, err := e.Annotate(ctx, loci)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Novel != 1 || ann.calls != 1 {
		t.Fatalf("first annotate: novel=%d calls=%d, want 1/1", r1.Novel, ann.calls)
	}
	if r1.Version != "2026-06" {
		t.Errorf("version = %q, want snapshot name 2026-06", r1.Version)
	}

	r2, err := e.Annotate(ctx, loci)
	if err != nil {
		t.Fatal(err)
	}
	if r2.Novel != 0 || ann.calls != 1 {
		t.Errorf("second annotate (cache hit): novel=%d calls=%d, want 0/1", r2.Novel, ann.calls)
	}
}
