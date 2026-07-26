package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/compgenlab/cganno/internal/model"
	"github.com/compgenlab/cganno/internal/store/sqlite"
)

// safeAnnotator is a concurrency-safe annotator: it counts invocations atomically
// and returns one "gene" row per locus. Used to prove Engine.Annotate is safe to
// call from multiple goroutines (the basis for service-level locus chunking).
type safeAnnotator struct{ calls int64 }

func (s *safeAnnotator) Annotate(_ context.Context, loci []model.Locus) ([]model.AnnRow, error) {
	atomic.AddInt64(&s.calls, 1)
	rows := make([]model.AnnRow, 0, len(loci))
	for _, l := range loci {
		rows = append(rows, model.AnnRow{Locus: l, DataSource: "t:1", Key: "gene", Value: model.Text("BRCA1")})
	}
	return rows, nil
}

// TestAnnotateConcurrentDisjoint runs many Engine.Annotate calls over disjoint
// locus chunks concurrently (as the chunked REST path does) and asserts every
// locus is annotated with no data race. Run with -race.
func TestAnnotateConcurrentDisjoint(t *testing.T) {
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ann := &safeAnnotator{}
	e := New(st, ann, "2026-06", "GRCh38", []model.DataSource{{Name: "t", Version: "1"}})
	if err := e.Init(context.Background()); err != nil {
		t.Fatal(err)
	}

	const chunks, per = 12, 25
	var mu sync.Mutex
	merged := map[string][]model.AnnRow{}
	var wg sync.WaitGroup
	for c := 0; c < chunks; c++ {
		c := c
		wg.Add(1)
		go func() {
			defer wg.Done()
			loci := make([]model.Locus, per)
			for i := range loci {
				loci[i] = model.Locus{Chrom: "chr1", Pos: int64(c*per + i + 1), Ref: "A", Alt: "G"}
			}
			res, err := e.Annotate(context.Background(), loci)
			if err != nil {
				t.Errorf("chunk %d: %v", c, err)
				return
			}
			mu.Lock()
			for k, v := range res.ByLocus {
				merged[k] = v
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if len(merged) != chunks*per {
		t.Fatalf("merged %d loci, want %d", len(merged), chunks*per)
	}
	for c := 0; c < chunks; c++ {
		for i := 0; i < per; i++ {
			key := fmt.Sprintf("chr1:%d:A:G", c*per+i+1)
			rows, ok := merged[key]
			if !ok || len(rows) == 0 {
				t.Fatalf("locus %s missing from merged result", key)
			}
		}
	}
}
