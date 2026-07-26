package annotator

import (
	"context"
	"fmt"
	"runtime"

	"golang.org/x/sync/errgroup"

	"github.com/compgenlab/varianthub-cli/internal/model"
)

// SourceAnnotator annotates loci from a single source. The overlay package's
// Source satisfies this structurally.
type SourceAnnotator interface {
	ID() string
	Annotate(ctx context.Context, loci []model.Locus) ([]model.AnnRow, error)
}

// Composite fans a locus batch out across per-source annotators concurrently
// and concatenates the results. Because rows are EAV (locus, source, key,
// value), merging never collides — it is just a slice append.
//
// Fail-fast: the first source error cancels the rest and fails the whole call,
// so every successful result reflects the complete pinned source set.
type Composite struct {
	sources []SourceAnnotator
	limit   int // max concurrent sources (<=0 ⇒ NumCPU)
}

var _ Annotator = (*Composite)(nil)

// NewComposite builds a Composite. A limit <= 0 defaults to GOMAXPROCS.
func NewComposite(sources []SourceAnnotator, limit int) *Composite {
	if limit <= 0 {
		limit = runtime.GOMAXPROCS(0)
	}
	return &Composite{sources: sources, limit: limit}
}

// Annotate runs every source over the same loci and merges the rows.
func (c *Composite) Annotate(ctx context.Context, loci []model.Locus) ([]model.AnnRow, error) {
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(c.limit)

	results := make([][]model.AnnRow, len(c.sources))
	for i, s := range c.sources {
		i, s := i, s
		g.Go(func() error {
			rows, err := s.Annotate(ctx, loci)
			if err != nil {
				return fmt.Errorf("source %s: %w", s.ID(), err)
			}
			results[i] = rows
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	var out []model.AnnRow
	for _, rows := range results {
		out = append(out, rows...)
	}
	return out, nil
}
