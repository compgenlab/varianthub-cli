package sqlite

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-cli/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Init(context.Background()); err != nil {
		t.Fatalf("init: %v", err)
	}
	return s
}

// seed writes a small set of annotated loci into the cache.
func seed(t *testing.T, s *Store) context.Context {
	t.Helper()
	ctx := context.Background()
	loci := []model.Locus{
		{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"},  // BRCA1, Pathogenic, af 0.01
		{Chrom: "chr1", Pos: 250, Ref: "C", Alt: "T"},  // BRCA1, Benign,     af 0.40
		{Chrom: "chr2", Pos: 900, Ref: "G", Alt: "A"},  // TP53,  Pathogenic, af 0.20
		{Chrom: "chr1", Pos: 9000, Ref: "T", Alt: "C"}, // BRCA1, Pathogenic, af 0.50
	}
	type ann struct {
		gene, sig string
		af        float64
	}
	data := []ann{
		{"BRCA1", "Pathogenic", 0.01},
		{"BRCA1", "Benign", 0.40},
		{"TP53", "Pathogenic", 0.20},
		{"BRCA1", "Pathogenic", 0.50},
	}
	var rows []model.AnnRow
	for i, l := range loci {
		rows = append(rows,
			model.AnnRow{Locus: l, DataSource: "vep:1", Key: "gene", Value: model.Text(data[i].gene)},
			model.AnnRow{Locus: l, DataSource: "clinvar:1", Key: "clinvar_sig", Value: model.Text(data[i].sig)},
			model.AnnRow{Locus: l, DataSource: "gnomad:1", Key: "af", Value: model.Number(data[i].af)},
		)
	}
	if err := s.PutAnnotations(ctx, "GRCh38", rows); err != nil {
		t.Fatalf("put annotations: %v", err)
	}
	return ctx
}

func TestAnnotationsCacheGrouping(t *testing.T) {
	s := newTestStore(t)
	ctx := seed(t, s)
	l := model.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}
	miss := model.Locus{Chrom: "chrX", Pos: 1, Ref: "A", Alt: "T"}
	got, err := s.Annotations(ctx, "GRCh38", []model.Locus{l, miss})
	if err != nil {
		t.Fatalf("annotations: %v", err)
	}
	if len(got[l.Key()]) != 3 {
		t.Errorf("want 3 rows for %s, got %d", l.Key(), len(got[l.Key()]))
	}
	if _, ok := got[miss.Key()]; ok {
		t.Errorf("cache miss %s should be absent", miss.Key())
	}
}

// TestAnnotationsAssemblyScoped verifies that the same chrom:pos:ref:alt stored
// under two assemblies does not collide: each assembly reads back only its own
// value, and a query under a third assembly is a clean miss.
func TestAnnotationsAssemblyScoped(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	l := model.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}
	put := func(assembly, gene string) {
		row := model.AnnRow{Locus: l, DataSource: "vep:1", Key: "gene", Value: model.Text(gene)}
		if err := s.PutAnnotations(ctx, assembly, []model.AnnRow{row}); err != nil {
			t.Fatalf("put %s: %v", assembly, err)
		}
	}
	put("GRCh38", "BRCA1")
	put("GRCh37", "TP53")

	read := func(assembly string) string {
		got, err := s.Annotations(ctx, assembly, []model.Locus{l})
		if err != nil {
			t.Fatalf("read %s: %v", assembly, err)
		}
		rows := got[l.Key()]
		if len(rows) != 1 {
			t.Fatalf("%s: want 1 row, got %d", assembly, len(rows))
		}
		return rows[0].Value.Str
	}
	if g := read("GRCh38"); g != "BRCA1" {
		t.Errorf("GRCh38 gene = %q, want BRCA1", g)
	}
	if g := read("GRCh37"); g != "TP53" {
		t.Errorf("GRCh37 gene = %q, want TP53 (assemblies collided)", g)
	}
	if got, _ := s.Annotations(ctx, "NCBI36", []model.Locus{l}); len(got) != 0 {
		t.Errorf("unknown assembly should miss, got %v", got)
	}
}

// TestToolOutputCache covers the external-tool output cache: processed markers
// (including no-output loci), header round-trip, and position-keyed line retrieval.
func TestToolOutputCache(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const uid = "vep:112"

	g := model.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}
	tt := model.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "T"}
	none := model.Locus{Chrom: "chr2", Pos: 5, Ref: "C", Alt: "G"} // processed, no output

	header := []string{"##source=vep", "#CHROM\tPOS\tREF\tALT\tCSQ"}
	lines := map[model.Locus][]string{
		g:  {"chr1\t100\tA\tG\tmissense"},
		tt: {"chr1\t100\tA\tT\tsynonymous"},
	}
	if err := s.PutToolOutput(ctx, uid, header, lines, []model.Locus{g, tt, none}); err != nil {
		t.Fatalf("put: %v", err)
	}

	// All three loci are now processed; an unseen locus is not.
	unseen := model.Locus{Chrom: "chr9", Pos: 1, Ref: "A", Alt: "C"}
	done, err := s.ToolProcessed(ctx, uid, []model.Locus{g, tt, none, unseen})
	if err != nil {
		t.Fatalf("processed: %v", err)
	}
	for _, l := range []model.Locus{g, tt, none} {
		if !done[l.Key()] {
			t.Errorf("%s should be processed", l.Key())
		}
	}
	if done[unseen.Key()] {
		t.Errorf("%s should NOT be processed", unseen.Key())
	}

	// Header round-trips.
	gotH, err := s.ToolHeader(ctx, uid)
	if err != nil || len(gotH) != 2 || gotH[0] != header[0] {
		t.Fatalf("header: %v %v", gotH, err)
	}

	// Lines are retrieved by position: querying either allele returns both lines
	// at chr1:100 (the tabix annotator re-matches ref/alt downstream).
	got, err := s.ToolLines(ctx, uid, []model.Locus{g})
	if err != nil {
		t.Fatalf("lines: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("want 2 lines at chr1:100, got %d: %v", len(got), got)
	}

	// A no-output locus contributes no lines.
	got2, err := s.ToolLines(ctx, uid, []model.Locus{none})
	if err != nil {
		t.Fatalf("lines none: %v", err)
	}
	if len(got2) != 0 {
		t.Errorf("want 0 lines at %s, got %d", none.Key(), len(got2))
	}
}

// queryPlan returns the EXPLAIN QUERY PLAN detail text for q (bound to args).
func queryPlan(t *testing.T, s *Store, q string, args ...any) string {
	t.Helper()
	rows, err := s.db.QueryContext(context.Background(), "EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var sb strings.Builder
	for rows.Next() {
		var id, parent, notused int
		var detail string
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			t.Fatalf("scan plan: %v", err)
		}
		sb.WriteString(detail)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// TestChunkQueriesUseIndex is the regression guard for the full-scan bug: the
// chunked lookup queries must SEARCH via an index, never SCAN the whole table.
// The bug was factoring the scoping column (assembly / tool_uid) OUTSIDE the OR
// group, which made every disjunct non-indexable and forced a per-chunk full scan
// — invisible on a small table, fatal once the cache held millions of rows.
func TestChunkQueriesUseIndex(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	// Seed enough rows that a full scan would be a clearly-wrong plan choice.
	var rows []model.AnnRow
	lines := map[model.Locus][]string{}
	var processed []model.Locus
	for i := 0; i < 2000; i++ {
		l := model.Locus{Chrom: "chr1", Pos: int64(i + 1), Ref: "A", Alt: "G"}
		rows = append(rows, model.AnnRow{Locus: l, DataSource: "vep:1", Key: "gene", Value: model.Text("G")})
		lines[l] = []string{"chr1\t1\tA\tG\tx"}
		processed = append(processed, l)
	}
	if err := s.PutAnnotations(ctx, "GRCh38", rows); err != nil {
		t.Fatal(err)
	}
	if err := s.PutToolOutput(ctx, "vep:1", nil, lines, processed); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, query string
		args        []any
	}{
		{
			"annotation",
			`SELECT chrom,pos,ref,alt,data_source_id,key,value_text,value_num FROM annotation ` +
				`WHERE (assembly=? AND chrom=? AND pos=? AND ref=? AND alt=?) OR (assembly=? AND chrom=? AND pos=? AND ref=? AND alt=?)`,
			[]any{"GRCh38", "chr1", 1, "A", "G", "GRCh38", "chr1", 2, "A", "G"},
		},
		{
			"tool_processed",
			`SELECT chrom,pos,ref,alt FROM tool_processed ` +
				`WHERE (tool_uid=? AND chrom=? AND pos=? AND ref=? AND alt=?) OR (tool_uid=? AND chrom=? AND pos=? AND ref=? AND alt=?)`,
			[]any{"vep:1", "chr1", 1, "A", "G", "vep:1", "chr1", 2, "A", "G"},
		},
		{
			"tool_line",
			`SELECT chrom,pos,line FROM tool_line ` +
				`WHERE (tool_uid=? AND chrom=? AND pos=?) OR (tool_uid=? AND chrom=? AND pos=?)`,
			[]any{"vep:1", "chr1", 1, "vep:1", "chr1", 2},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := queryPlan(t, s, c.query, c.args...)
			if !strings.Contains(plan, "SEARCH") {
				t.Errorf("plan should SEARCH via an index, got:\n%s", plan)
			}
			if strings.Contains(plan, "SCAN "+c.name) {
				t.Errorf("plan full-scans %s (the bug), got:\n%s", c.name, plan)
			}
			// The decisive check: the index seek must be constrained by chrom (and
			// pos), narrowing to the site — not just the scoping column. EXPLAIN lists
			// only the columns the index actually resolves in the parenthetical, so
			// "chrom=?" appearing there means the seek uses it.
			if !strings.Contains(plan, "chrom=?") {
				t.Errorf("index seek must narrow by chrom/pos, not just the scoping column; got:\n%s", plan)
			}
		})
	}

	// Prove the OLD shape (scoping column factored outside the OR) narrows by ONLY
	// the scoping column: it seeks tool_uid=? then walks EVERY row for that uid per
	// chunk (O(rows_for_uid) × chunks) — the real, non-hypothetical bug.
	oldForm := `SELECT chrom,pos,line FROM tool_line WHERE tool_uid=? AND ((chrom=? AND pos=?) OR (chrom=? AND pos=?))`
	plan := queryPlan(t, s, oldForm, "vep:1", "chr1", 1, "chr1", 2)
	if strings.Contains(plan, "chrom=?") {
		t.Errorf("expected the old shape's index seek to use tool_uid only (not chrom), got:\n%s", plan)
	}
}

// TestPutToolOutputIdempotent verifies the upsert (no DELETE pass): re-writing the
// same locus replaces its line at each ord rather than erroring on the PK or
// leaving duplicate rows.
func TestPutToolOutputIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const uid = "vep:1"
	l := model.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}

	if err := s.PutToolOutput(ctx, uid, nil, map[model.Locus][]string{l: {"chr1\t100\tA\tG\told"}}, []model.Locus{l}); err != nil {
		t.Fatalf("first put: %v", err)
	}
	// Re-write the same locus/ord with a new value — must not error, must replace.
	if err := s.PutToolOutput(ctx, uid, nil, map[model.Locus][]string{l: {"chr1\t100\tA\tG\tnew"}}, []model.Locus{l}); err != nil {
		t.Fatalf("second put: %v", err)
	}
	got, err := s.ToolLines(ctx, uid, []model.Locus{l})
	if err != nil {
		t.Fatalf("lines: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want exactly 1 line (replaced, not duplicated), got %d: %v", len(got), got)
	}
	if !strings.HasSuffix(got[0].Line, "new") {
		t.Errorf("line not replaced: %q", got[0].Line)
	}
}
