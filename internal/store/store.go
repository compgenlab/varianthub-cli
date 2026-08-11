// Package store defines the backend-agnostic persistence interface for the
// annotation cache. The shared contract is this interface, not any one physical
// schema — SQLite (default) and Postgres (later) may lay tables out differently
// behind it.
package store

import (
	"context"
	"time"

	"github.com/compgenlab/varianthub-cli/internal/model"
)

// HourBucket rounds a Unix timestamp down to the hour.
//
// Both backends stamp last_used through this. An LRU only needs to know roughly
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
// Two knobs because the two backends can answer two different questions
// cheaply. Age is universal: both index last_used, so discarding what is older
// than a cutoff costs only what it deletes. A count is not — it needs to know
// how big the cache is before deciding to act, and only Postgres keeps an
// estimate (pg_class.reltuples) that answers that without scanning. SQLite would
// have to COUNT(*) a table with hundreds of millions of rows to learn the same
// thing, after every run, usually to conclude there was nothing to do.
//
// Config validation rejects MaxEntries against a backend that cannot serve it,
// so an unsupported setting is an error at startup rather than a limit that
// silently never applies.
type Budget struct {
	// MaxAge discards entries unused for longer than this. Both backends.
	MaxAge time.Duration
	// MaxEntries caps cached (variant, source) units. Postgres only.
	MaxEntries int64
}

// Unbounded reports whether nothing would be evicted.
func (b Budget) Unbounded() bool { return b.MaxAge <= 0 && b.MaxEntries <= 0 }

// EvictionResult reports what a sweep did.
type EvictionResult struct {
	// Removed counts (variant, source) units for Postgres and rows for SQLite —
	// each backend's natural unit. It is progress reporting, not a quantity
	// anything decides on.
	Removed int64
	// Skipped means another sweeper held the lock and this one stood down.
	Skipped bool
}

// Evictor is a store that can be trimmed. Both backends implement it; the
// interface is separate from Store because trimming is a property of a cache
// with a budget, not of the annotation contract.
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
