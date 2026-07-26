// Package sqlite implements store.Store on SQLite using a single EAV annotation
// table. It is the default (interactive) backend.
package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/compgenlab/varianthub-cli/internal/model"
	"github.com/compgenlab/varianthub-cli/internal/store"
)

// chunk caps how many loci go into one IN-list (4 bound vars each).
const chunk = 200

// Store is a SQLite-backed store.Store.
type Store struct {
	db *sql.DB
}

var _ store.Store = (*Store)(nil)

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
	return &Store{db: db}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS data_source (
  id      TEXT PRIMARY KEY,
  name    TEXT NOT NULL,
  version TEXT NOT NULL,
  path    TEXT
);

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

CREATE TABLE IF NOT EXISTS tool_processed (
  tool_uid TEXT    NOT NULL,
  chrom    TEXT    NOT NULL,
  pos      INTEGER NOT NULL,
  ref      TEXT    NOT NULL,
  alt      TEXT    NOT NULL,
  PRIMARY KEY (tool_uid, chrom, pos, ref, alt)
);

CREATE TABLE IF NOT EXISTS tool_line (
  tool_uid TEXT    NOT NULL,
  chrom    TEXT    NOT NULL,
  pos      INTEGER NOT NULL,
  ref      TEXT    NOT NULL,
  alt      TEXT    NOT NULL,
  ord      INTEGER NOT NULL,
  line     TEXT    NOT NULL,
  PRIMARY KEY (tool_uid, chrom, pos, ref, alt, ord)
);
CREATE INDEX IF NOT EXISTS tool_line_pos ON tool_line(tool_uid, chrom, pos);
`

// Init creates the schema.
func (s *Store) Init(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("init schema: %w", err)
	}
	return nil
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
		q := `SELECT chrom,pos,ref,alt,data_source_id,key,value_text,value_num
		      FROM annotation WHERE ` + strings.Join(preds, " OR ")
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		if err := scanAnnRows(rows, out); err != nil {
			return nil, err
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
		`INSERT INTO annotation(assembly,chrom,pos,ref,alt,data_source_id,key,value_text,value_num)
		 VALUES(?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(assembly,chrom,pos,ref,alt,data_source_id,key)
		 DO UPDATE SET value_text=excluded.value_text, value_num=excluded.value_num`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		var vt any
		var vn any
		if r.Value.IsNum {
			vn = r.Value.Num
		} else {
			vt = r.Value.Str
		}
		if _, err := stmt.ExecContext(ctx, assembly, r.Locus.Chrom, r.Locus.Pos, r.Locus.Ref, r.Locus.Alt,
			r.DataSource, r.Key, vt, vn); err != nil {
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
		q := `SELECT chrom,pos,ref,alt FROM tool_processed WHERE ` +
			strings.Join(preds, " OR ")
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
	}
	return out, nil
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
	insLine, err := tx.PrepareContext(ctx,
		`INSERT INTO tool_line(tool_uid,chrom,pos,ref,alt,ord,line) VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(tool_uid,chrom,pos,ref,alt,ord) DO UPDATE SET line=excluded.line`)
	if err != nil {
		return err
	}
	defer insLine.Close()
	for l, ls := range lines {
		for i, line := range ls {
			if _, err := insLine.ExecContext(ctx, toolUID, l.Chrom, l.Pos, l.Ref, l.Alt, i, line); err != nil {
				return err
			}
		}
	}

	markStmt, err := tx.PrepareContext(ctx,
		`INSERT INTO tool_processed(tool_uid,chrom,pos,ref,alt) VALUES(?,?,?,?,?)
		 ON CONFLICT(tool_uid,chrom,pos,ref,alt) DO NOTHING`)
	if err != nil {
		return err
	}
	defer markStmt.Close()
	for _, l := range processed {
		if _, err := markStmt.ExecContext(ctx, toolUID, l.Chrom, l.Pos, l.Ref, l.Alt); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }
