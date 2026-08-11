package postgres

import (
	"context"
	"fmt"
	"strconv"
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

// Sources lists the registered sources. Path comes back empty: it is a local
// filesystem location, and a cache shared by workers that each resolve their own
// paths has no single true answer for it. store.Store documents it as advisory
// for exactly this reason.
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

// touchSites is touch for tool rows, whose parent is a site rather than an
// allele. Scoped to one tool: two tools' parents can share a position, and
// reading one says nothing about the other.
func (s *Store) touchSites(ctx context.Context, toolUID string, chrom []string, pos []int64) {
	now := hourOf(s.nowFn())
	_, _ = s.pool.Exec(ctx, `
		UPDATE cache_variant_source vs SET last_used = $4
		  FROM unnest($2::text[], $3::bigint[]) AS want(chrom, pos)
		 WHERE vs.assembly = $5 AND vs.source = $1 AND vs.last_used < $4
		   AND want.chrom = vs.chrom AND want.pos = vs.pos`,
		toolUID, chrom, pos, now, toolAssembly)
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
// would let the two disagree. Their parent is per site, with empty ref/alt —
// see the schema.
const toolAssembly = ""

func (s *Store) ToolProcessed(ctx context.Context, toolUID string, loci []model.Locus) (map[string]bool, error) {
	if len(loci) == 0 {
		return map[string]bool{}, nil
	}
	chrom, pos, ref, alt := columns(loci)
	rows, err := s.pool.Query(ctx, `
		SELECT vs.chrom, vs.pos, tp.ref, tp.alt
		  FROM cache_variant_source vs
		  JOIN cache_tool_processed tp ON tp.vs_id = vs.id
		  JOIN unnest($2::text[], $3::bigint[], $4::text[], $5::text[])
		       AS want(chrom, pos, ref, alt)
		    ON want.chrom = vs.chrom AND want.pos = vs.pos
		   AND want.ref = tp.ref AND want.alt = tp.alt
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
		c, p := sites(loci)
		s.touchSites(ctx, toolUID, c, p)
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

// ToolLines returns every cached line at the given loci's POSITIONS, whatever
// alleles those lines report. Retrieval by position is the contract (see
// store.Store and the schema): a line's own ref/alt may be absent or normalized,
// so matching on them would silently return nothing for a whole class of tools.
// The rebuilt file is re-matched on ref/alt by the tabix annotator downstream,
// so lines for a neighbouring allele cost a little I/O and change no answer.
func (s *Store) ToolLines(ctx context.Context, toolUID string, loci []model.Locus) ([]model.ToolLine, error) {
	if len(loci) == 0 {
		return nil, nil
	}
	// Deduped to distinct sites: several alleles at one position would otherwise
	// join the same lines once each and duplicate them in the rebuilt file.
	chrom, pos := sites(loci)
	rows, err := s.pool.Query(ctx, `
		SELECT vs.chrom, vs.pos, tl.line
		  FROM cache_variant_source vs
		  JOIN cache_tool_line tl ON tl.vs_id = vs.id
		  JOIN unnest($2::text[], $3::bigint[]) AS want(chrom, pos)
		    ON want.chrom = vs.chrom AND want.pos = vs.pos
		 WHERE vs.assembly = $4 AND vs.source = $1
		 ORDER BY vs.chrom, vs.pos, tl.ref, tl.alt, tl.ord`,
		toolUID, chrom, pos, toolAssembly)
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

	// Cleared first, so the header is replaced rather than merged. Upserting by
	// ord alone would leave the tail of a longer previous header in place, and
	// ToolHeader would hand back a splice of two runs.
	if len(header) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM cache_tool_header WHERE tool_uid=$1`, toolUID); err != nil {
			return err
		}
		for i, l := range header {
			if _, err := tx.Exec(ctx,
				`INSERT INTO cache_tool_header (tool_uid,ord,line) VALUES ($1,$2,$3)`,
				toolUID, i, l); err != nil {
				return err
			}
		}
	}

	now := hourOf(s.nowFn())
	// One parent per site, shared by this site's markers and lines. Memoized
	// because several alleles at a position resolve to the same row.
	ids := map[string]int64{}
	parent := func(chrom string, pos int64) (int64, error) {
		k := chrom + "\x00" + strconv.FormatInt(pos, 10)
		if id, ok := ids[k]; ok {
			return id, nil
		}
		var id int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO cache_variant_source (assembly,chrom,pos,ref,alt,source,last_used)
			VALUES ($1,$2,$3,'','',$4,$5)
			ON CONFLICT (assembly,chrom,pos,ref,alt,source)
			  DO UPDATE SET last_used = excluded.last_used
			RETURNING id`,
			toolAssembly, chrom, pos, toolUID, now).Scan(&id); err != nil {
			return 0, err
		}
		ids[k] = id
		return id, nil
	}

	// Markers carry the SUBMITTED alleles — the only place they are known to be
	// exact — and are written for every processed locus, including those the tool
	// had nothing to say about.
	for _, loc := range processed {
		id, err := parent(loc.Chrom, loc.Pos)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO cache_tool_processed (vs_id,ref,alt) VALUES ($1,$2,$3)
			ON CONFLICT (vs_id,ref,alt) DO NOTHING`,
			id, loc.Ref, loc.Alt); err != nil {
			return err
		}
	}

	// Lines carry the alleles the tool REPORTED, which is why they are keyed
	// separately from the markers above and may be empty.
	//
	// No delete-before-insert: the loci reaching here are novel (see
	// annotate.runToolCached), so a site cannot already hold lines for them, and
	// a blanket delete on this parent would take a sibling allele's output with
	// it. The upsert keeps a re-run idempotent without a per-locus index probe on
	// a write that runs at whole-genome scale.
	for loc, ls := range lines {
		id, err := parent(loc.Chrom, loc.Pos)
		if err != nil {
			return err
		}
		for i, line := range ls {
			if _, err := tx.Exec(ctx, `
				INSERT INTO cache_tool_line (vs_id,ref,alt,ord,line) VALUES ($1,$2,$3,$4,$5)
				ON CONFLICT (vs_id,ref,alt,ord) DO UPDATE SET line = excluded.line`,
				id, loc.Ref, loc.Alt, i, line); err != nil {
				return err
			}
		}
	}
	return tx.Commit(ctx)
}

// --- helpers ---

// sites reduces loci to their distinct positions, preserving order.
func sites(loci []model.Locus) (chrom []string, pos []int64) {
	type site struct {
		chrom string
		pos   int64
	}
	seen := make(map[site]bool, len(loci))
	for _, l := range loci {
		k := site{l.Chrom, l.Pos}
		if seen[k] {
			continue
		}
		seen[k] = true
		chrom = append(chrom, l.Chrom)
		pos = append(pos, l.Pos)
	}
	return chrom, pos
}

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
