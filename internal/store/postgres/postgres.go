package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/compgenlab/varianthub-cli/internal/model"
)

// Store is the annotation cache on Postgres.
type Store struct {
	pool *pgxpool.Pool
	// nowFn is injectable so tests can age entries without sleeping.
	nowFn func() int64
}

// Open connects to Postgres. The schema is created by Init.
func Open(ctx context.Context, dsn string) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("cache: connect: %w", err)
	}
	return &Store{pool: pool, nowFn: func() int64 { return time.Now().Unix() }}, nil
}

func (s *Store) Init(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, schema)
	return err
}

func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

// hourOf rounds a timestamp down to the hour.
//
// The whole point of the LRU is to know roughly what has been used lately, and
// rounding turns "one write per read" into "one write per variant per hour" —
// on a cache read by every annotation, that is the difference between a
// timestamp column and a vacuum problem.
func hourOf(sec int64) int64 { return sec - sec%3600 }

// --- data sources ---

func (s *Store) RegisterSources(ctx context.Context, sources []model.DataSource) error {
	if len(sources) == 0 {
		return nil
	}
	b := &pgx.Batch{}
	for _, d := range sources {
		b.Queue(`INSERT INTO cache_data_source (id,name,version) VALUES ($1,$2,$3)
		         ON CONFLICT (id) DO UPDATE SET name=excluded.name, version=excluded.version`,
			d.ID(), d.Name, d.Version)
	}
	return s.pool.SendBatch(ctx, b).Close()
}

func (s *Store) Sources(ctx context.Context) ([]model.DataSource, error) {
	rows, err := s.pool.Query(ctx, `SELECT name, version FROM cache_data_source ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.DataSource
	for rows.Next() {
		var d model.DataSource
		if err := rows.Scan(&d.Name, &d.Version); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// --- annotation values ---

// Annotations returns cached rows grouped by locus key.
//
// A locus appears in the result only for the sources actually cached for it, so
// a caller comparing against what it asked for can tell a partial hit from a
// whole one. The engine's own hit test is coarser than that today; this does not
// make it worse, and it makes the finer test possible.
func (s *Store) Annotations(ctx context.Context, assembly string, loci []model.Locus) (map[string][]model.AnnRow, error) {
	if len(loci) == 0 {
		return map[string][]model.AnnRow{}, nil
	}
	chrom, pos, ref, alt := columns(loci)

	rows, err := s.pool.Query(ctx, `
		SELECT vs.chrom, vs.pos, vs.ref, vs.alt, vs.source, e.key, e.value_text, e.value_num
		  FROM cache_variant_source vs
		  JOIN cache_entry e ON e.vs_id = vs.id
		  JOIN unnest($2::text[], $3::bigint[], $4::text[], $5::text[])
		       AS want(chrom, pos, ref, alt)
		    ON want.chrom = vs.chrom AND want.pos = vs.pos
		   AND want.ref = vs.ref AND want.alt = vs.alt
		 WHERE vs.assembly = $1`,
		assembly, chrom, pos, ref, alt)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string][]model.AnnRow{}
	for rows.Next() {
		var r model.AnnRow
		var text *string
		var num *float64
		if err := rows.Scan(&r.Locus.Chrom, &r.Locus.Pos, &r.Locus.Ref, &r.Locus.Alt,
			&r.DataSource, &r.Key, &text, &num); err != nil {
			return nil, err
		}
		r.Value = valueOf(text, num)
		out[r.Locus.Key()] = append(out[r.Locus.Key()], r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > 0 {
		s.touch(ctx, assembly, chrom, pos, ref, alt)
	}
	return out, nil
}

// touch marks these variants as recently used, at hour granularity.
//
// Best effort and deliberately not fatal: failing an annotation because an LRU
// timestamp could not be written would trade a correct answer for a bookkeeping
// detail. The cost of missing one is that an entry looks staler than it is.
func (s *Store) touch(ctx context.Context, assembly string, chrom []string, pos []int64, ref, alt []string) {
	now := hourOf(s.nowFn())
	_, _ = s.pool.Exec(ctx, `
		UPDATE cache_variant_source vs SET last_used = $6
		  FROM unnest($2::text[], $3::bigint[], $4::text[], $5::text[])
		       AS want(chrom, pos, ref, alt)
		 WHERE vs.assembly = $1 AND vs.last_used < $6
		   AND want.chrom = vs.chrom AND want.pos = vs.pos
		   AND want.ref = vs.ref AND want.alt = vs.alt`,
		assembly, chrom, pos, ref, alt, now)
}

// PutAnnotations writes rows, creating the (variant, source) parents they hang
// off.
//
// One transaction, which is what makes this safe against a concurrent eviction:
// inserting an entry takes a foreign-key share lock on its parent, so a sweep
// deleting that parent either waits or is waited for. Neither side has to know
// about the other, and no table lock is involved.
func (s *Store) PutAnnotations(ctx context.Context, assembly string, rows []model.AnnRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	now := hourOf(s.nowFn())
	ids := map[string]int64{}
	for _, r := range rows {
		k := r.Locus.Key() + "\x00" + r.DataSource
		id, ok := ids[k]
		if !ok {
			if err := tx.QueryRow(ctx, `
				INSERT INTO cache_variant_source (assembly,chrom,pos,ref,alt,source,last_used)
				VALUES ($1,$2,$3,$4,$5,$6,$7)
				ON CONFLICT (assembly,chrom,pos,ref,alt,source)
				  DO UPDATE SET last_used = excluded.last_used
				RETURNING id`,
				assembly, r.Locus.Chrom, r.Locus.Pos, r.Locus.Ref, r.Locus.Alt,
				r.DataSource, now).Scan(&id); err != nil {
				return err
			}
			ids[k] = id
		}
		text, num := split(r.Value)
		if _, err := tx.Exec(ctx, `
			INSERT INTO cache_entry (vs_id,key,value_text,value_num) VALUES ($1,$2,$3,$4)
			ON CONFLICT (vs_id,key) DO UPDATE
			  SET value_text = excluded.value_text, value_num = excluded.value_num`,
			id, r.Key, text, num); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// --- tool output ---

// Tool rows use the tool UID as their source and an empty assembly: the UID
// already folds the assembly in (see annotate.toolUID), and repeating it here
// would let the two disagree.
const toolAssembly = ""

func (s *Store) ToolProcessed(ctx context.Context, toolUID string, loci []model.Locus) (map[string]bool, error) {
	if len(loci) == 0 {
		return map[string]bool{}, nil
	}
	chrom, pos, ref, alt := columns(loci)
	rows, err := s.pool.Query(ctx, `
		SELECT vs.chrom, vs.pos, vs.ref, vs.alt
		  FROM cache_variant_source vs
		  JOIN unnest($2::text[], $3::bigint[], $4::text[], $5::text[])
		       AS want(chrom, pos, ref, alt)
		    ON want.chrom = vs.chrom AND want.pos = vs.pos
		   AND want.ref = vs.ref AND want.alt = vs.alt
		 WHERE vs.assembly = $6 AND vs.source = $1`,
		toolUID, chrom, pos, ref, alt, toolAssembly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var l model.Locus
		if err := rows.Scan(&l.Chrom, &l.Pos, &l.Ref, &l.Alt); err != nil {
			return nil, err
		}
		out[l.Key()] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) > 0 {
		s.touch(ctx, toolAssembly, chrom, pos, ref, alt)
	}
	return out, nil
}

func (s *Store) ToolHeader(ctx context.Context, toolUID string) ([]string, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT line FROM cache_tool_header WHERE tool_uid=$1 ORDER BY ord`, toolUID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) ToolLines(ctx context.Context, toolUID string, loci []model.Locus) ([]model.ToolLine, error) {
	if len(loci) == 0 {
		return nil, nil
	}
	chrom, pos, ref, alt := columns(loci)
	rows, err := s.pool.Query(ctx, `
		SELECT vs.chrom, vs.pos, tl.line
		  FROM cache_variant_source vs
		  JOIN cache_tool_line tl ON tl.vs_id = vs.id
		  JOIN unnest($2::text[], $3::bigint[], $4::text[], $5::text[])
		       AS want(chrom, pos, ref, alt)
		    ON want.chrom = vs.chrom AND want.pos = vs.pos
		   AND want.ref = vs.ref AND want.alt = vs.alt
		 WHERE vs.assembly = $6 AND vs.source = $1
		 ORDER BY vs.chrom, vs.pos, tl.ord`,
		toolUID, chrom, pos, ref, alt, toolAssembly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.ToolLine
	for rows.Next() {
		var t model.ToolLine
		if err := rows.Scan(&t.Chrom, &t.Pos, &t.Line); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PutToolOutput records a tool's run over some loci.
//
// processed and lines are written together, in one transaction, because they
// mean one thing: a variant marked processed with no lines is a variant the tool
// will not be run on again and has no output for, which is an annotation that
// silently disappears. A locus in processed with no line is legitimate — the
// tool had nothing to say — and that is exactly why the marker cannot be
// inferred from the lines.
func (s *Store) PutToolOutput(ctx context.Context, toolUID string, header []string,
	lines map[model.Locus][]string, processed []model.Locus) error {

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after Commit

	for i, l := range header {
		if _, err := tx.Exec(ctx, `
			INSERT INTO cache_tool_header (tool_uid,ord,line) VALUES ($1,$2,$3)
			ON CONFLICT (tool_uid,ord) DO UPDATE SET line = excluded.line`,
			toolUID, i, l); err != nil {
			return err
		}
	}

	now := hourOf(s.nowFn())
	for _, loc := range processed {
		var id int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO cache_variant_source (assembly,chrom,pos,ref,alt,source,last_used)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT (assembly,chrom,pos,ref,alt,source)
			  DO UPDATE SET last_used = excluded.last_used
			RETURNING id`,
			toolAssembly, loc.Chrom, loc.Pos, loc.Ref, loc.Alt, toolUID, now).Scan(&id); err != nil {
			return err
		}
		// Replaced rather than merged: a re-run's output is the whole answer for
		// that locus, and leaving earlier lines beside it would duplicate records
		// in the rebuilt file.
		if _, err := tx.Exec(ctx, `DELETE FROM cache_tool_line WHERE vs_id=$1`, id); err != nil {
			return err
		}
		for i, line := range lines[loc] {
			if _, err := tx.Exec(ctx,
				`INSERT INTO cache_tool_line (vs_id,ord,line) VALUES ($1,$2,$3)`,
				id, i, line); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// --- helpers ---

func columns(loci []model.Locus) (chrom []string, pos []int64, ref, alt []string) {
	chrom = make([]string, len(loci))
	pos = make([]int64, len(loci))
	ref = make([]string, len(loci))
	alt = make([]string, len(loci))
	for i, l := range loci {
		chrom[i], pos[i], ref[i], alt[i] = l.Chrom, l.Pos, l.Ref, l.Alt
	}
	return
}

func split(v model.Value) (*string, *float64) {
	if v.IsNum {
		n := v.Num
		return nil, &n
	}
	t := v.Str
	return &t, nil
}

func valueOf(text *string, num *float64) model.Value {
	if num != nil {
		return model.Value{IsNum: true, Num: *num}
	}
	if text != nil {
		return model.Value{Str: *text}
	}
	return model.Value{}
}
