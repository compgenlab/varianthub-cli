package postgres

import (
	"context"
	"fmt"
)

// evictLockKey identifies the eviction advisory lock.
//
// An advisory lock rather than a table lock, deliberately. Locking the cache
// tables would stall every annotation for the length of the sweep, which on a
// large cache is an outage on the hot path for a piece of housekeeping. This
// only stops two sweepers running at once, which is the actual requirement — and
// Postgres releases it if the holder's session dies, so a killed worker leaves
// nothing to clear.
const evictLockKey = 0x7661_7268_7562_0001 // "varhub" + 1

// EvictionResult reports what a sweep did.
type EvictionResult struct {
	Before  int64 // variant-source rows before
	Removed int64 // variant-source rows removed
	Skipped bool  // another sweeper held the lock
}

// Evict trims the cache to maxEntries (variant,source) rows, removing the
// least recently used first.
//
// Counted in parents rather than values: entries per (variant, source) is
// bounded by how many fields a source declares, so the parent count is a stable
// proxy for total size and needs no denormalized counter to be kept true by
// every writer.
//
// The count comes from the planner's estimate rather than count(*), which is a
// sequential scan of the whole table — an hourly full scan of tens of millions
// of rows to decide whether to do anything is a poor trade for precision a cache
// budget does not need.
//
// Deleted in batches, each its own transaction: one statement removing millions
// of rows holds locks for its whole duration and writes a WAL record to match.
// A batch's row locks are brief and only touch what is going.
func (s *Store) Evict(ctx context.Context, maxEntries, batch int64) (EvictionResult, error) {
	if maxEntries <= 0 {
		return EvictionResult{}, fmt.Errorf("cache: maxEntries must be positive, got %d", maxEntries)
	}
	if batch <= 0 {
		batch = 10_000
	}

	// One sweeper at a time. Not fatal to lose the race — it means somebody
	// else is already doing it.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return EvictionResult{}, err
	}
	defer conn.Release()

	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(evictLockKey)).Scan(&got); err != nil {
		return EvictionResult{}, err
	}
	if !got {
		return EvictionResult{Skipped: true}, nil
	}
	defer conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, int64(evictLockKey)) //nolint:errcheck

	before, err := s.approxCount(ctx)
	if err != nil {
		return EvictionResult{}, err
	}
	res := EvictionResult{Before: before}
	if before <= maxEntries {
		return res, nil
	}

	target := before - maxEntries
	for res.Removed < target {
		n := batch
		if remaining := target - res.Removed; remaining < n {
			n = remaining
		}
		tag, err := s.pool.Exec(ctx, `
			DELETE FROM cache_variant_source
			 WHERE id IN (
			   SELECT id FROM cache_variant_source
			    ORDER BY last_used ASC
			    LIMIT $1
			 )`, n)
		if err != nil {
			return res, err
		}
		removed := tag.RowsAffected()
		if removed == 0 {
			break // nothing left to take; the estimate was high
		}
		res.Removed += removed
	}
	return res, nil
}

// approxCount is the planner's row estimate for the parent table.
//
// Maintained by autovacuum and read from the catalog in constant time. Wrong by
// a few percent between analyses, which for "is the cache over budget" is
// indistinguishable from right — and the alternative is a sequential scan every
// hour to learn the same thing.
//
// -1 means the table has never been analysed; treated as zero so a brand-new
// cache is not evicted on the strength of a missing statistic.
func (s *Store) approxCount(ctx context.Context) (int64, error) {
	var est float64
	err := s.pool.QueryRow(ctx,
		`SELECT reltuples FROM pg_class WHERE relname = 'cache_variant_source'`).Scan(&est)
	if err != nil {
		return 0, err
	}
	if est < 0 {
		return 0, nil
	}
	return int64(est), nil
}

// Clear removes everything cached, for an administrator who wants to start over.
//
// TRUNCATE rather than DELETE: the point is to reclaim the space, and DELETE
// leaves it to be vacuumed. CASCADE follows the foreign keys to the values.
func (s *Store) Clear(ctx context.Context) error {
	_, err := s.pool.Exec(ctx,
		`TRUNCATE cache_variant_source, cache_tool_header RESTART IDENTITY CASCADE`)
	return err
}
