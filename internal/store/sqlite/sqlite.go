// Package sqlite implements store.Store on SQLite using a single EAV annotation
// table. It is the default (interactive) backend.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/compgenlab/varianthub-cli/internal/model"
	"github.com/compgenlab/varianthub-cli/internal/store"
)

// chunk caps how many loci go into one IN-list (4 bound vars each).
const chunk = 200

// Store is a SQLite-backed store.Store.
type Store struct {
	db *sql.DB
	// nowFn is injectable so tests can age entries without sleeping.
	nowFn func() int64
}

var (
	_ store.Store   = (*Store)(nil)
	_ store.Evictor = (*Store)(nil)
)

// stamp is the value written to last_used: the current hour.
func (s *Store) stamp() int64 { return store.HourBucket(s.nowFn()) }

// Open opens (creating if needed) a SQLite database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", path, err)
	}
	// Single connection keeps the embedded writer simple and avoids
	// "database is locked" churn for the interactive use-case.
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}
	return &Store{db: db, nowFn: func() int64 { return time.Now().Unix() }}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS data_source (
  id      TEXT PRIMARY KEY,
  name    TEXT NOT NULL,
  version TEXT NOT NULL,
  path    TEXT
);

-- last_used is Unix seconds rounded down to the hour (store.HourBucket). Every
-- row of a (variant, source) unit is stamped in a single statement — on write,
-- and again on read — so a unit's rows always carry the same value. That is what
-- lets eviction delete by cutoff without a GROUP BY and still never split a unit
-- in half, which would leave the engine serving a partial answer as a whole one.
CREATE TABLE IF NOT EXISTS annotation (
  assembly       TEXT    NOT NULL,
  chrom          TEXT    NOT NULL,
  pos            INTEGER NOT NULL,
  ref            TEXT    NOT NULL,
  alt            TEXT    NOT NULL,
  data_source_id TEXT    NOT NULL,
  key            TEXT    NOT NULL,
  value_text     TEXT,
  value_num      REAL,
  last_used      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (assembly, chrom, pos, ref, alt, data_source_id, key)
);

-- External-tool output cache (Phase 2): caches a tool's raw output keyed by
-- locus so the tool runs only on novel loci.
CREATE TABLE IF NOT EXISTS tool_header (
  tool_uid TEXT    NOT NULL,
  ord      INTEGER NOT NULL,
  line     TEXT    NOT NULL,
  PRIMARY KEY (tool_uid, ord)
);

-- A tool's marker and its lines carry last_used too, and are stamped together by
-- position — the granularity a tool's output is filed and retrieved at. Evicting
-- a marker while its lines survive (or the reverse) would leave a locus recorded
-- as processed with no output: the tool would not be re-run and its annotations
-- would disappear without a word.
CREATE TABLE IF NOT EXISTS tool_processed (
  tool_uid  TEXT    NOT NULL,
  chrom     TEXT    NOT NULL,
  pos       INTEGER NOT NULL,
  ref       TEXT    NOT NULL,
  alt       TEXT    NOT NULL,
  last_used INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (tool_uid, chrom, pos, ref, alt)
);

CREATE TABLE IF NOT EXISTS tool_line (
  tool_uid  TEXT    NOT NULL,
  chrom     TEXT    NOT NULL,
  pos       INTEGER NOT NULL,
  ref       TEXT    NOT NULL,
  alt       TEXT    NOT NULL,
  ord       INTEGER NOT NULL,
  line      TEXT    NOT NULL,
  last_used INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (tool_uid, chrom, pos, ref, alt, ord)
);
CREATE INDEX IF NOT EXISTS tool_line_pos ON tool_line(tool_uid, chrom, pos);
`

// addedColumns are columns introduced after the first release. A varhub.db
// created by an earlier version has the tables already, so CREATE TABLE IF NOT
// EXISTS above skips them and the new column never appears — every subsequent
// query then fails with "no such column". Added here instead, ignoring the
// duplicate-column error that means a current database.
var addedColumns = []string{
	`ALTER TABLE annotation ADD COLUMN last_used INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE tool_processed ADD COLUMN last_used INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE tool_line ADD COLUMN last_used INTEGER NOT NULL DEFAULT 0`,
}

// lruIndexes are created after addedColumns, not with the tables: on a database
// from an earlier version the column does not exist until the ALTER has run, and
// an index over a missing column is an error rather than a skip.
const lruIndexes = `
CREATE INDEX IF NOT EXISTS annotation_lru     ON annotation(last_used);
CREATE INDEX IF NOT EXISTS tool_processed_lru ON tool_processed(last_used);
CREATE INDEX IF NOT EXISTS tool_line_lru      ON tool_line(last_used);
`

// Init creates the schema: tables, then any columns added since they were first
// written, then the indexes that depend on those columns.
func (s *Store) Init(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	for _, stmt := range addedColumns {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil && !isDuplicateColumn(err) {
			return fmt.Errorf("init schema: %s: %w", stmt, err)
		}
	}
	if _, err := s.db.ExecContext(ctx, lruIndexes); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	return nil
}

// isDuplicateColumn reports whether err is SQLite refusing to add a column that
// is already there — the expected outcome on an up-to-date database.
//
// Matched on the message because modernc.org/sqlite reports it as a generic
// error rather than a distinguishable code. A miss here is not silent: the ALTER
// fails, Init returns it, and the cache refuses to open.
func isDuplicateColumn(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "duplicate column")
}

// RegisterSources upserts the pinned data sources.
func (s *Store) RegisterSources(ctx context.Context, sources []model.DataSource) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO data_source(id,name,version,path) VALUES(?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, version=excluded.version, path=excluded.path`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, src := range sources {
		if _, err := stmt.ExecContext(ctx, src.ID(), src.Name, src.Version, src.Path); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Sources lists the registered data sources.
func (s *Store) Sources(ctx context.Context) ([]model.DataSource, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name,version,path FROM data_source ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.DataSource
	for rows.Next() {
		var d model.DataSource
		var path sql.NullString
		if err := rows.Scan(&d.Name, &d.Version, &path); err != nil {
			return nil, err
		}
		d.Path = path.String
		out = append(out, d)
	}
	return out, rows.Err()
}

// Annotations returns cached rows grouped by locus key, scoped to assembly.
func (s *Store) Annotations(ctx context.Context, assembly string, loci []model.Locus) (map[string][]model.AnnRow, error) {
	out := make(map[string][]model.AnnRow)
	for start := 0; start < len(loci); start += chunk {
		end := start + chunk
		if end > len(loci) {
			end = len(loci)
		}
		batch := loci[start:end]
		var preds []string
		var args []any
		// The scoping column (assembly) is repeated INSIDE each OR term so every
		// disjunct is a full prefix of the PK index (assembly,chrom,pos,ref,alt,…);
		// factored outside the OR, SQLite can't use the index and full-scans the
		// table per chunk — fatal once the cache is large.
		for _, l := range batch {
			preds = append(preds, "(assembly=? AND chrom=? AND pos=? AND ref=? AND alt=?)")
			args = append(args, assembly, l.Chrom, l.Pos, l.Ref, l.Alt)
		}
		where := strings.Join(preds, " OR ")
		q := `SELECT chrom,pos,ref,alt,data_source_id,key,value_text,value_num
		      FROM annotation WHERE ` + where
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		if err := scanAnnRows(rows, out); err != nil {
			return nil, err
		}
		// Reading is what "recently used" means, so a hit has to move the entry
		// forward or the cache evicts exactly what it is serving. One statement per
		// chunk, over the same index prefix the read used, and only when the hour
		// has actually turned — a re-read within the hour writes nothing.
		//
		// Best effort: failing an annotation because a bookkeeping timestamp could
		// not be written would trade a correct answer for a housekeeping detail.
		// The cost of losing one is that an entry looks staler than it is.
		if len(batch) > 0 {
			now := s.stamp()
			_, _ = s.db.ExecContext(ctx,
				`UPDATE annotation SET last_used=? WHERE last_used<? AND (`+where+`)`,
				append([]any{now, now}, args...)...)
		}
	}
	return out, nil
}

func scanAnnRows(rows *sql.Rows, out map[string][]model.AnnRow) error {
	defer rows.Close()
	for rows.Next() {
		var r model.AnnRow
		var vt sql.NullString
		var vn sql.NullFloat64
		if err := rows.Scan(&r.Locus.Chrom, &r.Locus.Pos, &r.Locus.Ref, &r.Locus.Alt,
			&r.DataSource, &r.Key, &vt, &vn); err != nil {
			return err
		}
		if vn.Valid {
			r.Value = model.Number(vn.Float64)
		} else {
			r.Value = model.Text(vt.String)
		}
		k := r.Locus.Key()
		out[k] = append(out[k], r)
	}
	return rows.Err()
}

// PutAnnotations upserts annotation rows, scoped to assembly.
func (s *Store) PutAnnotations(ctx context.Context, assembly string, rows []model.AnnRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO annotation(assembly,chrom,pos,ref,alt,data_source_id,key,value_text,value_num,last_used)
		 VALUES(?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(assembly,chrom,pos,ref,alt,data_source_id,key)
		 DO UPDATE SET value_text=excluded.value_text, value_num=excluded.value_num,
		               last_used=excluded.last_used`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	// One value for the whole call, so every row of a unit written here shares a
	// timestamp — the property eviction relies on.
	now := s.stamp()
	for _, r := range rows {
		var vt any
		var vn any
		if r.Value.IsNum {
			vn = r.Value.Num
		} else {
			vt = r.Value.Str
		}
		if _, err := stmt.ExecContext(ctx, assembly, r.Locus.Chrom, r.Locus.Pos, r.Locus.Ref, r.Locus.Alt,
			r.DataSource, r.Key, vt, vn, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ToolProcessed returns the subset of loci already run through toolUID.
func (s *Store) ToolProcessed(ctx context.Context, toolUID string, loci []model.Locus) (map[string]bool, error) {
	out := make(map[string]bool)
	for start := 0; start < len(loci); start += chunk {
		end := start + chunk
		if end > len(loci) {
			end = len(loci)
		}
		preds := make([]string, 0, end-start)
		var args []any
		// tool_uid repeated inside each OR term so every disjunct is a prefix of the
		// PK index (tool_uid,chrom,pos,ref,alt); see Annotations for why.
		for _, l := range loci[start:end] {
			preds = append(preds, "(tool_uid=? AND chrom=? AND pos=? AND ref=? AND alt=?)")
			args = append(args, toolUID, l.Chrom, l.Pos, l.Ref, l.Alt)
		}
		where := strings.Join(preds, " OR ")
		q := `SELECT chrom,pos,ref,alt FROM tool_processed WHERE ` + where
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var l model.Locus
			if err := rows.Scan(&l.Chrom, &l.Pos, &l.Ref, &l.Alt); err != nil {
				rows.Close()
				return nil, err
			}
			out[l.Key()] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
		// After the rows are closed, never before: the pool is capped at one
		// connection, so a write issued while a result set is still open waits for a
		// connection that only closing the result set can release.
		s.touchTool(ctx, toolUID, loci[start:end])
	}
	return out, nil
}

// touchTool moves a tool's cached work at these positions forward.
//
// By position, matching how the two tables are keyed against each other: markers
// carry the submitted alleles, lines the ones the tool reported, and those need
// not agree. Stamping both by position keeps them at the same value, so a cutoff
// takes them together or leaves them together — never a marker without its lines.
//
// Best effort, like the annotation touch.
func (s *Store) touchTool(ctx context.Context, toolUID string, loci []model.Locus) {
	if len(loci) == 0 {
		return
	}
	seen := make(map[string]bool, len(loci))
	preds := make([]string, 0, len(loci))
	args := []any{}
	for _, l := range loci {
		k := l.Chrom + "\x00" + strconv.FormatInt(l.Pos, 10)
		if seen[k] {
			continue
		}
		seen[k] = true
		preds = append(preds, "(tool_uid=? AND chrom=? AND pos=?)")
		args = append(args, toolUID, l.Chrom, l.Pos)
	}
	where := strings.Join(preds, " OR ")
	now := s.stamp()
	for _, table := range []string{"tool_processed", "tool_line"} {
		_, _ = s.db.ExecContext(ctx,
			`UPDATE `+table+` SET last_used=? WHERE last_used<? AND (`+where+`)`,
			append([]any{now, now}, args...)...)
	}
}

// ToolHeader returns toolUID's cached header lines in order.
func (s *Store) ToolHeader(ctx context.Context, toolUID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT line FROM tool_header WHERE tool_uid=? ORDER BY ord`, toolUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	return out, rows.Err()
}

// ToolLines returns cached output lines covering the given loci's positions.
func (s *Store) ToolLines(ctx context.Context, toolUID string, loci []model.Locus) ([]model.ToolLine, error) {
	// Dedup to distinct positions — output lines are retrieved by site, then the
	// tabix annotator re-matches ref/alt.
	type site struct {
		chrom string
		pos   int64
	}
	seen := make(map[site]bool)
	var sites []site
	for _, l := range loci {
		k := site{l.Chrom, l.Pos}
		if !seen[k] {
			seen[k] = true
			sites = append(sites, k)
		}
	}
	var out []model.ToolLine
	for start := 0; start < len(sites); start += chunk {
		end := start + chunk
		if end > len(sites) {
			end = len(sites)
		}
		preds := make([]string, 0, end-start)
		var args []any
		// tool_uid repeated inside each OR term so every disjunct is a prefix of the
		// tool_line_pos index (tool_uid,chrom,pos); see Annotations for why.
		for _, st := range sites[start:end] {
			preds = append(preds, "(tool_uid=? AND chrom=? AND pos=?)")
			args = append(args, toolUID, st.chrom, st.pos)
		}
		q := `SELECT chrom,pos,line FROM tool_line WHERE ` +
			strings.Join(preds, " OR ")
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var tl model.ToolLine
			if err := rows.Scan(&tl.Chrom, &tl.Pos, &tl.Line); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, tl)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// PutToolOutput stores toolUID's header, per-locus output lines, and processed
// markers in one transaction.
func (s *Store) PutToolOutput(ctx context.Context, toolUID string, header []string, lines map[model.Locus][]string, processed []model.Locus) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if len(header) > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM tool_header WHERE tool_uid=?`, toolUID); err != nil {
			return err
		}
		hstmt, err := tx.PrepareContext(ctx, `INSERT INTO tool_header(tool_uid,ord,line) VALUES(?,?,?)`)
		if err != nil {
			return err
		}
		for i, line := range header {
			if _, err := hstmt.ExecContext(ctx, toolUID, i, line); err != nil {
				hstmt.Close()
				return err
			}
		}
		hstmt.Close()
	}

	// No per-locus DELETE-before-insert: tool_line and tool_processed are committed
	// together in this one transaction, so a "novel" locus (the only kind passed
	// here — see runToolCached) cannot already have tool_line rows. The upsert keeps
	// this idempotent (replace an existing line at the same ord) without the extra
	// per-locus index probe that dominated the write at whole-genome scale.
	now := s.stamp()
	insLine, err := tx.PrepareContext(ctx,
		`INSERT INTO tool_line(tool_uid,chrom,pos,ref,alt,ord,line,last_used) VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(tool_uid,chrom,pos,ref,alt,ord) DO UPDATE SET line=excluded.line,
		     last_used=excluded.last_used`)
	if err != nil {
		return err
	}
	defer insLine.Close()
	for l, ls := range lines {
		for i, line := range ls {
			if _, err := insLine.ExecContext(ctx, toolUID, l.Chrom, l.Pos, l.Ref, l.Alt, i, line, now); err != nil {
				return err
			}
		}
	}

	markStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO tool_processed(tool_uid,chrom,pos,ref,alt,last_used) VALUES(?,?,?,?,?,?)
		 ON CONFLICT(tool_uid,chrom,pos,ref,alt) DO UPDATE SET last_used=excluded.last_used`)
	if err != nil {
		return err
	}
	defer markStmt.Close()
	for _, l := range processed {
		if _, err := markStmt.ExecContext(ctx, toolUID, l.Chrom, l.Pos, l.Ref, l.Alt, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }
