// Package store defines the persistence interface for varhub's annotation
// cache. The contract is this interface rather than any one physical schema, so
// a backend is free to lay its tables out as it likes behind it.
//
// SQLite is the only backend. A Postgres one existed for the web deployment and
// was removed once that cache moved up a layer: varianthub-web looks a variant
// up before it invokes varhub at all, which skips the process, the source reads
// and any tool container — none of which a cache down here can avoid — and keeps
// a database credential out of a config file varhub executes recipes beside.
package store

import (
	"context"
	"time"

	"github.com/compgenlab/varianthub-cli/internal/model"
)

// HourBucket rounds a Unix timestamp down to the hour.
//
// last_used is stamped through this. An LRU only needs to know roughly
// what has been used lately, and rounding turns "one write per read" into "one
// write per variant per hour" — on a table read by every annotation, that is the
// difference between a timestamp column and a write-amplification problem.
//
// It also does the load-bearing work for eviction: every row of a (variant,
// source) unit is stamped in one statement, so a unit's rows always share one
// value and a cutoff can never fall through the middle of one.
func HourBucket(sec int64) int64 { return sec - sec%3600 }

// Budget bounds a cache. The zero value means unbounded.
//
// Age, and only age. Trimming by count would need to know the cache's size
// before deciding to act, and SQLite keeps no estimate — it would have to
// COUNT(*) a table of hundreds of millions of rows after every run, almost
// always to conclude there was nothing to do. A cutoff costs a range scan over
// the LRU index and touches only what it removes.
type Budget struct {
	// MaxAge discards entries unused for longer than this.
	MaxAge time.Duration
}

// Unbounded reports whether nothing would be evicted.
func (b Budget) Unbounded() bool { return b.MaxAge <= 0 }

// EvictionResult reports what a sweep did.
type EvictionResult struct {
	// Removed counts rows. It is progress reporting, not a quantity anything
	// decides on.
	Removed int64
	// Skipped means another sweeper held the cache and this one stood down.
	Skipped bool
}

// Evictor is a store that can be trimmed. Separate from Store because trimming
// is a property of a cache with a budget, not of the annotation contract.
type Evictor interface {
	// Evict trims the cache to budget, removing least-recently-used first.
	// Whole (variant, source) units go at once: a unit half-removed would be
	// served as a complete answer by the engine.
	Evict(ctx context.Context, budget Budget) (EvictionResult, error)
	// Clear removes everything cached.
	Clear(ctx context.Context) error
}

// Store is the annotation cache: it memoizes computed annotations keyed by locus
// and data source.
type Store interface {
	// Init creates the schema if needed (idempotent).
	Init(ctx context.Context) error

	// RegisterSources upserts the pinned data sources. A source is identified by
	// model.DataSource.ID(), which the store treats as opaque: "name:version" as
	// the CLI composes it, or any other identifier that changes when the data
	// behind it does.
	RegisterSources(ctx context.Context, sources []model.DataSource) error
	// Sources lists the registered data sources. Path is advisory and may come
	// back empty — it is a local filesystem location, which a cache shared by
	// several workers cannot answer for.
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
	//
	// By position, and deliberately not by allele: a line is filed under the
	// locus the line itself reports, and a tool may normalize an allele or (with
	// ref_col/alt_col unset) report none at all. Matching on ref/alt would return
	// nothing for those tools. Lines belonging to another allele at the same site
	// may therefore come back; the tabix annotator re-matches ref/alt when the
	// rebuilt file is read, so they change no answer.
	ToolLines(ctx context.Context, toolUID string, loci []model.Locus) ([]model.ToolLine, error)
	// PutToolOutput records, in one transaction: the tool's header (replacing any
	// prior), the output lines for each locus, and a processed marker for every
	// locus in `processed` (even those with no lines). Idempotent.
	PutToolOutput(ctx context.Context, toolUID string, header []string, lines map[model.Locus][]string, processed []model.Locus) error

	Close() error
}
