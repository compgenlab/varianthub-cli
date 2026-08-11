package sqlite

import (
	"context"
	"fmt"

	"github.com/compgenlab/varianthub-cli/internal/store"
)

// Evict trims the cache to budget.
//
// Age only. store.Budget documents why a count is Postgres-only: deciding
// whether to act on a count means knowing the size first, and SQLite keeps no
// estimate — it would have to COUNT(*) a table of hundreds of millions of rows
// after every run, nearly always to conclude there was nothing to do. A cutoff
// costs a range scan over the LRU index and touches only what it removes. Config
// validation rejects MaxEntries against this backend, so an unusable setting
// fails at startup rather than quietly never applying.
//
// Whole units go together for free here, without the parent row Postgres needs:
// every row of a (variant, source) is stamped by one statement, so they all
// carry the same value and no cutoff can fall between them. A tool's marker and
// its lines are stamped by position for the same reason.
func (s *Store) Evict(ctx context.Context, budget store.Budget) (store.EvictionResult, error) {
	if budget.MaxAge <= 0 {
		return store.EvictionResult{}, nil
	}
	cutoff := store.HourBucket(s.nowFn() - int64(budget.MaxAge.Seconds()))

	var res store.EvictionResult
	// Ordered so a crash between statements cannot leave a marker outliving its
	// lines: lines go first, and a marker whose lines are gone only means the tool
	// runs again. The reverse would be silently missing annotations.
	for _, table := range []string{"tool_line", "tool_processed", "annotation"} {
		out, err := s.db.ExecContext(ctx,
			`DELETE FROM `+table+` WHERE last_used < ?`, cutoff)
		if err != nil {
			return res, fmt.Errorf("cache: evict %s: %w", table, err)
		}
		n, err := out.RowsAffected()
		if err != nil {
			return res, err
		}
		res.Removed += n
	}
	return res, nil
}

// Clear removes everything cached, for someone who wants to start over.
//
// The rows go, the file does not shrink: SQLite keeps freed pages for reuse
// rather than returning them to the filesystem. VACUUM would reclaim the space
// but rewrites the whole database, which on a cache measured in tens of
// gigabytes is a long stall for a directory that is safe to simply delete. Left
// to the operator, who can do either.
func (s *Store) Clear(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	for _, table := range []string{"annotation", "tool_line", "tool_processed", "tool_header"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table); err != nil {
			return fmt.Errorf("cache: clear %s: %w", table, err)
		}
	}
	return tx.Commit()
}
