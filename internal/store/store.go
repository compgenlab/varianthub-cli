// Package store defines the backend-agnostic persistence interface for the
// annotation cache. The shared contract is this interface, not any one physical
// schema — SQLite (default) and Postgres (later) may lay tables out differently
// behind it.
package store

import (
	"context"

	"github.com/compgenlab/varianthub-cli/internal/model"
)

// Store is the annotation cache: it memoizes computed annotations keyed by locus
// and data source.
type Store interface {
	// Init creates the schema if needed (idempotent).
	Init(ctx context.Context) error

	// RegisterSources upserts the pinned data sources.
	RegisterSources(ctx context.Context, sources []model.DataSource) error
	// Sources lists the registered data sources.
	Sources(ctx context.Context) ([]model.DataSource, error)

	// Annotations returns cached annotation rows grouped by locus key
	// (model.Locus.Key). Loci absent from the result are cache misses. Rows are
	// scoped to assembly, since a chrom:pos means different things across
	// assemblies (a snapshot pins exactly one assembly).
	Annotations(ctx context.Context, assembly string, loci []model.Locus) (map[string][]model.AnnRow, error)
	// PutAnnotations writes annotation rows into the cache (idempotent upsert),
	// scoped to assembly.
	PutAnnotations(ctx context.Context, assembly string, rows []model.AnnRow) error

	// --- External-tool output cache (keyed by opaque tool UID) ---
	//
	// These memoize an external tool's raw output so a tool (e.g. VEP) runs only
	// on loci it hasn't seen. Output lines are stored per locus but retrieved by
	// position, so the existing tabix annotator does the ref/alt matching when
	// the rebuilt output file is consumed. The toolUID is an opaque cache key
	// (name:version, plus the assembly folded in by the caller — see
	// annotate.toolUID) so the store need not know about assembly here.

	// ToolProcessed returns the subset of loci (by Locus.Key) already run through
	// the tool — including loci that produced no output line. Loci absent from the
	// result must still be sent to the tool.
	ToolProcessed(ctx context.Context, toolUID string, loci []model.Locus) (map[string]bool, error)
	// ToolHeader returns the tool's cached header/meta lines, in order.
	ToolHeader(ctx context.Context, toolUID string) ([]string, error)
	// ToolLines returns the tool's cached output lines covering the given loci'
	// positions (chrom+pos), for reassembling the output file.
	ToolLines(ctx context.Context, toolUID string, loci []model.Locus) ([]model.ToolLine, error)
	// PutToolOutput records, in one transaction: the tool's header (replacing any
	// prior), the output lines for each locus, and a processed marker for every
	// locus in `processed` (even those with no lines). Idempotent.
	PutToolOutput(ctx context.Context, toolUID string, header []string, lines map[model.Locus][]string, processed []model.Locus) error

	Close() error
}
