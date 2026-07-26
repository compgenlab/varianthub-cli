// Package annotator defines the annotation workflow boundary. The real workflow
// (vcf-annotate / github.com/compgenlab/cghts, plus ANNOVAR/VEP and custom steps)
// will be implemented behind this interface later; for now Mock backs it with
// deterministic canned data so the rest of the system can be built and tested.
package annotator

import (
	"context"

	"github.com/compgenlab/varianthub-cli/internal/model"
)

// Annotator computes annotation rows for a set of loci. Implementations must be
// deterministic for a given pinned source set so the cache stays reproducible.
type Annotator interface {
	Annotate(ctx context.Context, loci []model.Locus) ([]model.AnnRow, error)
}
