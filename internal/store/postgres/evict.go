package postgres

import (
	"context"
	"fmt"

	"github.com/compgenlab/varianthub-cli/internal/store"
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

// Evict trims the cache to budget, least recently used first.
//
// Both knobs, applied in that order: age first, because discarding what nobody
// has touched in months is cheap and may leave the count already under its cap,
// and a count sweep that runs anyway would then take entries that are merely
// old-ish rather than genuinely stale.
//
// Counted in parents rather than values: entries per (variant, source) is
// bounded by how many fields a source declares, so the parent count is a stable
// proxy for total size and needs no denormalized counter to be kept true by
// every writer.
//
// The count comes from the planner's estimate rather than count(*), which is a
// sequential scan of the whole table — a full scan after every run to decide
// whether to do anything is a poor trade for precision a cache budget does not
// need.
//
// Deleted in batches, each its own transaction: one statement removing millions
// of rows holds locks for its whole duration and writes a WAL record to match.
// A batch's row locks are brief and only touch what is going.
func (s *Store) Evict(ctx context.Context, budget store.Budget) (store.EvictionResult, error) {
	if budget.Unbounded() {
		return store.EvictionResult{}, nil
	}
	const batch int64 = 10_000

	// One sweeper at a time. Not fatal to lose the race — it means somebody
	// else is already doing it. Unlike SQLite, this cache is shared, so several
	// workers can finish a run at once and all reach for the same rows.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return store.EvictionResult{}, err
	}
	defer conn.Release()

	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(evictLockKey)).Scan(&got); err != nil {
		return store.EvictionResult{}, err
	}
	if !got {
		return store.EvictionResult{Skipped: true}, nil
	}
	defer conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, int64(evictLockKey)) //nolint:errcheck

	var res store.EvictionResult
	if budget.MaxAge > 0 {
		cutoff := store.HourBucket(s.nowFn() - int64(budget.MaxAge.Seconds()))
		for {
			tag, err := s.pool.Exec(ctx, `
				DELETE FROM cache_variant_source
				 WHERE id IN (
				   SELECT id FROM cache_variant_source
				    WHERE last_used < $1
				    LIMIT $2
				 )`, cutoff, batch)
			if err != nil {
				return res, fmt.Errorf("cache: evict by age: %w", err)
			}
			res.Removed += tag.RowsAffected()
			if tag.RowsAffected() < batch {
				break
			}
		}
	}
	if budget.MaxEntries <= 0 {
		return res, nil
	}

	before, err := s.approxCount(ctx)
	if err != nil {
		return res, err
	}
	if before <= budget.MaxEntries {
		return res, nil
	}

	target := before - budget.MaxEntries
	for removed := int64(0); removed < target; {
		n := batch
		if remaining := target - removed; remaining < n {
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
		if tag.RowsAffected() == 0 {
			break // nothing left to take; the estimate was high
		}
		removed += tag.RowsAffected()
		res.Removed += tag.RowsAffected()
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
