package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/compgenlab/varianthub-cli/internal/model"
	"github.com/compgenlab/varianthub-cli/internal/store"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("VARHUB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("VARHUB_TEST_DATABASE_URL not set; skipping Postgres cache tests")
	}
	ctx := context.Background()
	s, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	// Its own schema per test, so tests do not see each other's rows.
	schemaName := fmt.Sprintf("t_%d", time.Now().UnixNano())
	if _, err := s.pool.Exec(ctx, `CREATE SCHEMA `+schemaName); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `SET search_path TO `+schemaName); err != nil {
		t.Fatal(err)
	}
	s.pool.Close()

	s2, err := Open(ctx, dsn+"&search_path="+schemaName)
	if err != nil {
		t.Fatal(err)
	}
	if err := s2.Init(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		s2.pool.Exec(context.Background(), `DROP SCHEMA `+schemaName+` CASCADE`)
		s2.Close()
	})
	return s2
}

func locus(pos int64) model.Locus {
	return model.Locus{Chrom: "chr1", Pos: pos, Ref: "A", Alt: "G"}
}

// A round trip through the cache, and the shape that makes it safe: values hang
// off a (variant, source) parent, so they are wholly there or wholly gone.
func TestAnnotationsRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	rows := []model.AnnRow{
		{Locus: locus(100), DataSource: "clinvar:2026-01", Key: "SIG", Value: model.Text("benign")},
		{Locus: locus(100), DataSource: "clinvar:2026-01", Key: "AF", Value: model.Number(0.25)},
	}
	if err := s.PutAnnotations(ctx, "GRCh38", rows); err != nil {
		t.Fatal(err)
	}
	got, err := s.Annotations(ctx, "GRCh38", []model.Locus{locus(100)})
	if err != nil {
		t.Fatal(err)
	}
	if len(got[locus(100).Key()]) != 2 {
		t.Fatalf("got %d rows, want 2", len(got[locus(100).Key()]))
	}
	// A different assembly is a different cache: chrom:pos means something else.
	other, err := s.Annotations(ctx, "GRCh37", []model.Locus{locus(100)})
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 0 {
		t.Error("an assembly's cache leaked into another's")
	}
}

// Eviction removes whole (variant, source) units. A unit half-removed is the
// failure this design exists to prevent: the engine treats any rows as a hit, so
// surviving values would be served as a complete answer.
func TestEvictionRemovesWholeUnits(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	// Three variants, two values each, aged apart.
	for i, pos := range []int64{100, 200, 300} {
		s.nowFn = func() int64 { return int64(i+1) * 7200 }
		rows := []model.AnnRow{
			{Locus: locus(pos), DataSource: "src:1", Key: "A", Value: model.Text("x")},
			{Locus: locus(pos), DataSource: "src:1", Key: "B", Value: model.Text("y")},
		}
		if err := s.PutAnnotations(ctx, "GRCh38", rows); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.pool.Exec(ctx, `ANALYZE cache_variant_source`); err != nil {
		t.Fatal(err)
	}

	res, err := s.Evict(ctx, store.Budget{MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 2 {
		t.Fatalf("removed %d parents, want 2 (3 cached, budget 1)", res.Removed)
	}

	// The survivor is whole, and the evicted are entirely gone.
	got, err := s.Annotations(ctx, "GRCh38", []model.Locus{locus(100), locus(200), locus(300)})
	if err != nil {
		t.Fatal(err)
	}
	for k, rows := range got {
		if len(rows) != 2 {
			t.Errorf("%s has %d values, want 2 — a unit was half-evicted, which the "+
				"engine would serve as a complete answer", k, len(rows))
		}
	}
	if len(got) != 1 {
		t.Errorf("%d variants survived, want 1", len(got))
	}
	// Least recently used went first.
	if _, ok := got[locus(300).Key()]; !ok {
		t.Error("the most recently used variant was evicted")
	}
}

// A tool's "already run" marker and its output lines are one unit. Splitting
// them would leave a variant marked processed with no output — the tool would
// not be re-run, and its annotations would vanish without a word.
func TestToolMarkerAndLinesAreEvictedTogether(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	loci := []model.Locus{locus(100)}
	if err := s.PutToolOutput(ctx, "vep:113:GRCh38", []string{"##header"},
		map[model.Locus][]string{locus(100): {"line-1", "line-2"}}, loci); err != nil {
		t.Fatal(err)
	}
	processed, err := s.ToolProcessed(ctx, "vep:113:GRCh38", loci)
	if err != nil {
		t.Fatal(err)
	}
	if !processed[locus(100).Key()] {
		t.Fatal("the locus was not recorded as processed")
	}

	if _, err := s.pool.Exec(ctx, `ANALYZE cache_variant_source`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM cache_variant_source`); err != nil {
		t.Fatal(err)
	}

	// Marker gone, and so are the lines — no orphans claiming work was done.
	processed, err = s.ToolProcessed(ctx, "vep:113:GRCh38", loci)
	if err != nil {
		t.Fatal(err)
	}
	if processed[locus(100).Key()] {
		t.Error("the processed marker survived its output")
	}
	lines, err := s.ToolLines(ctx, "vep:113:GRCh38", loci)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 0 {
		t.Errorf("%d output lines survived the parent", len(lines))
	}
}

// A tool's output lines are filed under the locus the LINE reports, which is not
// always the locus that was submitted: a tab-format tool with no ref_col/alt_col
// reports no alleles at all (annotate.lineLocus leaves them empty), and an allele
// may come back normalized. Storing or fetching those lines by allele loses them
// outright — the tool would be marked as having run, and its annotations would be
// missing from the rebuilt file with nothing to say so.
func TestToolLinesSurviveAnAlleleTheToolDidNotReport(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const uid = "phylop:1|GRCh38"
	submitted := model.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}
	// What lineLocus yields for a position-only tab tool: same site, no alleles.
	reported := model.Locus{Chrom: "chr1", Pos: 100}

	if err := s.PutToolOutput(ctx, uid, []string{"#chrom\tpos\tscore"},
		map[model.Locus][]string{reported: {"chr1\t100\t2.5"}},
		[]model.Locus{submitted}); err != nil {
		t.Fatal(err)
	}

	lines, err := s.ToolLines(ctx, uid, []model.Locus{submitted})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 {
		t.Fatalf("got %d cached lines, want 1 — a line the tool reported without "+
			"alleles was not retrievable by the locus that was submitted", len(lines))
	}
	if lines[0].Line != "chr1\t100\t2.5" {
		t.Errorf("got line %q", lines[0].Line)
	}

	// The marker still carries the submitted alleles, so a sibling allele at the
	// same site is still novel and will still be sent to the tool.
	done, err := s.ToolProcessed(ctx, uid, []model.Locus{
		submitted, {Chrom: "chr1", Pos: 100, Ref: "A", Alt: "T"}})
	if err != nil {
		t.Fatal(err)
	}
	if !done[submitted.Key()] {
		t.Error("the submitted allele was not marked processed")
	}
	if done[model.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "T"}.Key()] {
		t.Error("an allele that was never submitted came back as already processed")
	}
}

// Two alleles at one site keep their own output. They share a parent row, so a
// write for one must not disturb the other's lines.
func TestSiblingAllelesAtOneSiteKeepTheirOwnLines(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const uid = "vep:113|GRCh38"
	ag := model.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "G"}
	at := model.Locus{Chrom: "chr1", Pos: 100, Ref: "A", Alt: "T"}

	for _, l := range []model.Locus{ag, at} {
		if err := s.PutToolOutput(ctx, uid, nil,
			map[model.Locus][]string{l: {"chr1\t100\t" + l.Alt}},
			[]model.Locus{l}); err != nil {
			t.Fatal(err)
		}
	}
	lines, err := s.ToolLines(ctx, uid, []model.Locus{ag, at})
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 — one allele's write clobbered the other's", len(lines))
	}
	// Asking about a single allele still returns the whole site: the tabix
	// annotator re-matches ref/alt downstream, and duplicates would be worse.
	one, err := s.ToolLines(ctx, uid, []model.Locus{ag})
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 2 {
		t.Errorf("got %d lines for one allele, want the site's 2", len(one))
	}
}

// A header is replaced, not merged. Upserting line-by-line leaves the tail of a
// longer previous header behind, and the caller gets a splice of two runs.
func TestToolHeaderIsReplacedNotMerged(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	const uid = "vep:113|GRCh38"
	loci := []model.Locus{locus(100)}
	if err := s.PutToolOutput(ctx, uid, []string{"##a", "##b", "##c"}, nil, loci); err != nil {
		t.Fatal(err)
	}
	if err := s.PutToolOutput(ctx, uid, []string{"##only"}, nil, loci); err != nil {
		t.Fatal(err)
	}
	got, err := s.ToolHeader(ctx, uid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "##only" {
		t.Errorf("header is %q, want [##only] — the previous run's tail survived", got)
	}
}

// Two sweepers must not both evict. The second finds the lock held and does
// nothing, rather than taking another slice out of the cache.
func TestOnlyOneSweeperEvicts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	var got bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, int64(evictLockKey)).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Fatal("could not take the lock to simulate a peer")
	}
	defer conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, int64(evictLockKey))

	res, err := s.Evict(ctx, store.Budget{MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped {
		t.Error("a second sweeper evicted while another held the lock")
	}
}
