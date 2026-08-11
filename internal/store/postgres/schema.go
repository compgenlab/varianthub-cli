// Package postgres implements the annotation cache over Postgres, for
// deployments where several workers share one cache rather than each keeping a
// SQLite file of its own.
//
// The unit of caching is a (variant, source) pair: what one pinned source has to
// say about one variant. That is the unit the engine asks for, the unit that is
// wholly present or wholly absent, and therefore the unit eviction removes.
// Values hang off it.
//
// Why not one flat table with last_used on every row. Postgres updates are
// copy-on-write, so touching a variant's timestamp would rewrite one tuple per
// annotation field, on the hottest table, on every read — bloat and vacuum
// pressure scaling with read traffic, which is the wrong thing to scale with.
// One parent row per (variant, source) means one rewrite instead of N.
//
// Why a surrogate key. The natural key is six columns; repeated in the value
// table and its index it dominates storage at the tens of millions of rows this
// is sized for. An 8-byte id costs one join and saves that.
package postgres

// schema is applied by Init and is idempotent.
//
// source holds "name:version" for an annotation source and the tool UID for a
// tool's output — both are already versioned identifiers, which is what stops a
// re-provisioned source serving values computed from data it no longer has.
const schema = `
CREATE TABLE IF NOT EXISTS cache_data_source (
  id      TEXT PRIMARY KEY,
  name    TEXT NOT NULL,
  version TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS cache_variant_source (
  id        BIGSERIAL PRIMARY KEY,
  assembly  TEXT   NOT NULL,
  chrom     TEXT   NOT NULL,
  pos       BIGINT NOT NULL,
  ref       TEXT   NOT NULL,
  alt       TEXT   NOT NULL,
  source    TEXT   NOT NULL,
  -- Unix seconds, rounded down to the hour by the writer. LRU does not need
  -- second precision, and rounding turns a write per read into a write per
  -- variant per hour.
  last_used BIGINT NOT NULL,
  UNIQUE (assembly, chrom, pos, ref, alt, source)
);

-- Eviction reads this in order; without it the sweep is a sort of the whole
-- table every hour.
CREATE INDEX IF NOT EXISTS cache_variant_source_lru
  ON cache_variant_source (last_used);

CREATE TABLE IF NOT EXISTS cache_entry (
  vs_id      BIGINT NOT NULL REFERENCES cache_variant_source (id) ON DELETE CASCADE,
  key        TEXT   NOT NULL,
  value_text TEXT,
  value_num  DOUBLE PRECISION,
  PRIMARY KEY (vs_id, key)
);

-- A tool's raw output for one variant: several lines, ordered. Hangs off the
-- same parent, so eviction removes a tool's output and its "already run" marker
-- together. Splitting them would leave a variant marked processed with no
-- output — the tool would not be re-run and its annotations would silently
-- vanish, which is the failure this design exists to prevent.
CREATE TABLE IF NOT EXISTS cache_tool_line (
  vs_id BIGINT  NOT NULL REFERENCES cache_variant_source (id) ON DELETE CASCADE,
  ord   INTEGER NOT NULL,
  line  TEXT    NOT NULL,
  PRIMARY KEY (vs_id, ord)
);

-- Per tool, not per variant, so it is not evictable and not part of the budget.
CREATE TABLE IF NOT EXISTS cache_tool_header (
  tool_uid TEXT    NOT NULL,
  ord      INTEGER NOT NULL,
  line     TEXT    NOT NULL,
  PRIMARY KEY (tool_uid, ord)
);
`
