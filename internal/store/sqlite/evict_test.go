package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/compgenlab/varianthub-cli/internal/model"
	"github.com/compgenlab/varianthub-cli/internal/store"
)

const hour = 3600

// at pins the store's clock so entries can be aged without sleeping.
func at(s *Store, sec int64) { s.nowFn = func() int64 { return sec } }

func annRows(l model.Locus, keys ...string) []model.AnnRow {
	var out []model.AnnRow
	for _, k := range keys {
		out = append(out, model.AnnRow{Locus: l, DataSource: "src:1", Key: k, Value: model.Text("v")})
	}
	return out
}

// Eviction takes the stale and leaves the fresh, and a surviving unit keeps every
// value it had. A unit half-evicted is the failure the design exists to prevent:
// the engine treats any rows for a locus as a hit, so the remains of one would be
// served as a complete answer.
func TestEvictByAgeRemovesWholeUnits(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	old := model.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}
	recent := model.Locus{Chrom: "chr1", Pos: 200, Ref: "A", Alt: "G"}

	at(s, 100*hour)
	if err := s.PutAnnotations(ctx, "GRCh38", annRows(old, "A", "B", "C")); err != nil {
		t.Fatal(err)
	}
	at(s, 200*hour)
	if err := s.PutAnnotations(ctx, "GRCh38", annRows(recent, "A", "B", "C")); err != nil {
		t.Fatal(err)
	}

	// Now is 201h; keep anything used in the last 50h.
	at(s, 201*hour)
	res, err := s.Evict(ctx, store.Budget{MaxAge: 50 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 3 {
		t.Errorf("removed %d rows, want 3 (one unit of three values)", res.Removed)
	}

	got, err := s.Annotations(ctx, "GRCh38", []model.Locus{old, recent})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got[old.Key()]; ok {
		t.Error("the stale unit survived")
	}
	if n := len(got[recent.Key()]); n != 3 {
		t.Errorf("the fresh unit has %d values, want 3 — it was half-evicted, and the "+
			"engine would serve what is left as a complete answer", n)
	}
}

// Reading is what "recently used" means. Without a touch on read, the cache
// evicts precisely the entries it is busiest serving.
func TestReadingKeepsAnEntryAlive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	l := model.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}

	at(s, 100*hour)
	if err := s.PutAnnotations(ctx, "GRCh38", annRows(l, "A")); err != nil {
		t.Fatal(err)
	}
	// Read it much later, then trim with a window that would have caught the
	// original write.
	at(s, 200*hour)
	if _, err := s.Annotations(ctx, "GRCh38", []model.Locus{l}); err != nil {
		t.Fatal(err)
	}
	at(s, 201*hour)
	if _, err := s.Evict(ctx, store.Budget{MaxAge: 50 * time.Hour}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Annotations(ctx, "GRCh38", []model.Locus{l})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[l.Key()]) != 1 {
		t.Error("an entry read minutes ago was evicted as stale")
	}
}

// A tool's marker and its lines must go together. A marker outliving its output
// is the quiet failure: the tool is never re-run, and its annotations are simply
// absent from the rebuilt file.
func TestToolMarkerAndLinesAgeTogether(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	const uid = "vep:113|GRCh38"
	submitted := model.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}
	// A position-only tab tool reports no alleles, so its lines are filed under a
	// different locus than the marker — they can only stay in step by position.
	reported := model.Locus{Chrom: "chr1", Pos: 100}

	at(s, 100*hour)
	if err := s.PutToolOutput(ctx, uid, []string{"##h"},
		map[model.Locus][]string{reported: {"chr1\t100\tx"}},
		[]model.Locus{submitted}); err != nil {
		t.Fatal(err)
	}

	// Read at 200h: both the marker and the line must move forward.
	at(s, 200*hour)
	if _, err := s.ToolProcessed(ctx, uid, []model.Locus{submitted}); err != nil {
		t.Fatal(err)
	}
	at(s, 201*hour)
	if _, err := s.Evict(ctx, store.Budget{MaxAge: 50 * time.Hour}); err != nil {
		t.Fatal(err)
	}

	done, err := s.ToolProcessed(ctx, uid, []model.Locus{submitted})
	if err != nil {
		t.Fatal(err)
	}
	lines, err := s.ToolLines(ctx, uid, []model.Locus{submitted})
	if err != nil {
		t.Fatal(err)
	}
	if done[submitted.Key()] && len(lines) == 0 {
		t.Error("the processed marker outlived its output: the tool will not be re-run " +
			"and its annotations are gone")
	}
	if !done[submitted.Key()] || len(lines) != 1 {
		t.Errorf("marker=%v lines=%d, want both kept — they were touched together",
			done[submitted.Key()], len(lines))
	}
}

// A cache written by an earlier varhub has the tables but not the column. Init
// has to add it, or every query afterwards fails with "no such column".
func TestOpeningACacheFromBeforeTheLRUColumn(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")

	// The pre-LRU schema, as that version wrote it.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`
		CREATE TABLE annotation (
		  assembly TEXT NOT NULL, chrom TEXT NOT NULL, pos INTEGER NOT NULL,
		  ref TEXT NOT NULL, alt TEXT NOT NULL, data_source_id TEXT NOT NULL,
		  key TEXT NOT NULL, value_text TEXT, value_num REAL,
		  PRIMARY KEY (assembly, chrom, pos, ref, alt, data_source_id, key));
		CREATE TABLE tool_processed (
		  tool_uid TEXT NOT NULL, chrom TEXT NOT NULL, pos INTEGER NOT NULL,
		  ref TEXT NOT NULL, alt TEXT NOT NULL,
		  PRIMARY KEY (tool_uid, chrom, pos, ref, alt));
		CREATE TABLE tool_line (
		  tool_uid TEXT NOT NULL, chrom TEXT NOT NULL, pos INTEGER NOT NULL,
		  ref TEXT NOT NULL, alt TEXT NOT NULL, ord INTEGER NOT NULL, line TEXT NOT NULL,
		  PRIMARY KEY (tool_uid, chrom, pos, ref, alt, ord));
		INSERT INTO annotation VALUES ('GRCh38','chr1',100,'A','G','src:1','K','v',NULL);`)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Init(ctx); err != nil {
		t.Fatalf("init against a pre-LRU cache: %v", err)
	}

	// Checked before any read, because reading is a touch: a migrated row carries
	// 0 until something uses it, making it the oldest thing in the cache. That is
	// the right answer for an entry whose last use was never recorded.
	var stamp int64
	if err := s.db.QueryRowContext(ctx, `SELECT last_used FROM annotation`).Scan(&stamp); err != nil {
		t.Fatal(err)
	}
	if stamp != 0 {
		t.Errorf("migrated row carries last_used=%d, want 0", stamp)
	}

	l := model.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}
	at(s, 100*hour)
	got, err := s.Annotations(ctx, "GRCh38", []model.Locus{l})
	if err != nil {
		t.Fatalf("reading a migrated cache: %v", err)
	}
	if len(got[l.Key()]) != 1 {
		t.Error("the existing entry did not survive the migration")
	}
	// That read moved it to 100h, so a window ending after it still discards it.
	at(s, 200*hour)
	if _, err := s.Evict(ctx, store.Budget{MaxAge: time.Hour}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Annotations(ctx, "GRCh38", []model.Locus{l}); len(got) != 0 {
		t.Error("a stale entry in a migrated cache was not evicted")
	}
}

// An unbounded budget is not an instruction to empty the cache.
func TestUnboundedBudgetEvictsNothing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	l := model.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}
	at(s, 100*hour)
	if err := s.PutAnnotations(ctx, "GRCh38", annRows(l, "A")); err != nil {
		t.Fatal(err)
	}
	at(s, 10_000*hour)
	res, err := s.Evict(ctx, store.Budget{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 0 {
		t.Errorf("removed %d rows with no budget set", res.Removed)
	}
}
