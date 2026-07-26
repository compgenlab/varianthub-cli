package annotator

import (
	"context"
	"errors"
	"testing"

	"github.com/compgenlab/varianthub-cli/internal/model"
)

type fakeSource struct {
	id   string
	rows []model.AnnRow
	err  error
}

func (f fakeSource) ID() string { return f.id }
func (f fakeSource) Annotate(_ context.Context, _ []model.Locus) ([]model.AnnRow, error) {
	return f.rows, f.err
}

func row(src, key, val string) model.AnnRow {
	return model.AnnRow{Locus: model.Locus{Chrom: "chr1", Pos: 1, Ref: "A", Alt: "G"}, DataSource: src, Key: key, Value: model.Text(val)}
}

func TestCompositeMerges(t *testing.T) {
	c := NewComposite([]SourceAnnotator{
		fakeSource{id: "a", rows: []model.AnnRow{row("a", "gene", "BRCA1")}},
		fakeSource{id: "b", rows: []model.AnnRow{row("b", "clinvar_sig", "Pathogenic")}},
	}, 0)
	rows, err := c.Annotate(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 merged rows, got %d", len(rows))
	}
}

func TestCompositeFailFast(t *testing.T) {
	boom := errors.New("source down")
	c := NewComposite([]SourceAnnotator{
		fakeSource{id: "a", rows: []model.AnnRow{row("a", "gene", "BRCA1")}},
		fakeSource{id: "b", err: boom},
	}, 0)
	rows, err := c.Annotate(context.Background(), nil)
	if err == nil {
		t.Fatal("want error from failing source, got nil")
	}
	if rows != nil {
		t.Errorf("want nil rows on fail-fast, got %d", len(rows))
	}
}
