// Package service holds the shared locus-annotation orchestration used by both
// the CLI (`cganno annotate`) and the REST server (`cganno server`): building the
// overlay/composite engine over a store, running any referenced external tool
// sources, and annotating a set of loci. Keeping it here (rather than in package
// main) lets the server reuse the exact same code path as the CLI.
package service

import (
	"context"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sync/errgroup"

	"github.com/compgenlab/cganno/internal/annotate"
	"github.com/compgenlab/cganno/internal/annotator"
	"github.com/compgenlab/cganno/internal/annotator/overlay"
	"github.com/compgenlab/cganno/internal/config"
	"github.com/compgenlab/cganno/internal/engine"
	"github.com/compgenlab/cganno/internal/fetch"
	"github.com/compgenlab/cganno/internal/model"
	"github.com/compgenlab/cganno/internal/store"
)

// NewEngineOverStore builds the annotate engine over an already-open store: the
// overlay annotator is one tabix source per pinned (non-builtin) source, plus
// `extra` sources (e.g. tool outputs projected via Tool.AsSource, used on the
// cache/locus path) read from their LocalPath. A nil store means compute-only
// (no cache persistence).
func NewEngineOverStore(ctx context.Context, cfg *config.Config, snap *config.Snapshot, st store.Store, extra []config.Source) (*engine.Engine, error) {
	var srcs []annotator.SourceAnnotator
	for _, s := range snap.Sources {
		if s.IsTool() {
			continue // tool sources have no static file; their output arrives via `extra`
		}
		if s.IsBuiltinSource() {
			// The variant-only builtins (auto_id/indel/tstv/tags) compute from the
			// locus alone, so they run on this path too; sample-derived builtins and
			// vardist are skipped (NewBuiltinSource filters them).
			srcs = append(srcs, overlay.NewBuiltinSource(s))
			continue
		}
		srcs = append(srcs, overlay.NewSource(cfg, s, cfg.ResolveSourceFiles(s), snap.Annotations))
	}
	for _, s := range extra {
		srcs = append(srcs, overlay.NewSource(cfg, s, []config.SourceFile{{Path: s.LocalPath}}, snap.Annotations))
	}
	ann := annotator.NewComposite(srcs, 0)

	eng := engine.New(st, ann, snap.Name, snap.Assembly, snap.DataSources())
	if err := eng.Init(ctx); err != nil {
		return nil, err
	}
	return eng, nil
}

// ReferencedTools returns the snapshot's tool sources whose output is read by a
// selected annotation (so unused tools — e.g. VEP — aren't run).
func ReferencedTools(snap *config.Snapshot, anns []config.Annotation) []config.Source {
	need := map[string]bool{}
	for _, a := range anns {
		need[a.Source] = true
	}
	var out []config.Source
	for _, s := range snap.ToolSources() {
		if need[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// RequireSources errors if any file-based source referenced by anns isn't fully
// present on disk (tool sources are generated at run time, so skipped).
func RequireSources(cfg *config.Config, snap *config.Snapshot, anns []config.Annotation) error {
	seen := map[string]bool{}
	var problems []string
	for _, a := range anns {
		src := snap.SourceByName(a.Source)
		if src == nil || src.IsTool() || seen[src.ID()] {
			continue
		}
		seen[src.ID()] = true
		if m := fetch.Missing(cfg, *src); len(m) > 0 {
			problems = append(problems, fmt.Sprintf("%s (missing %s)", src.ID(), m[0]))
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("sources not downloaded — run `cganno download`:\n  %s", strings.Join(problems, "\n  "))
	}
	return nil
}

// AnnotateLoci annotates loci over a snapshot, returning the engine result. It
// mirrors the CLI's tab/json/text path: it verifies the file sources are present,
// runs any external tool sources referenced by the selected annotations into a
// temp workdir (cleaned up before returning), builds the overlay/composite engine
// over the store, and annotates. The engine always annotates every source (so the
// cache stays complete); `selected` only governs which tools are launched.
//
// A nil store computes without persisting (tool sources then require a non-nil
// store for their output cache and are skipped when st is nil).
//
// skipToolCache selects how referenced tools run: false (individual-locus queries)
// uses the per-locus tool cache (runToolCached — a tool runs only on novel loci);
// true (bulk VCF inputs — a file or an uploaded VCF) bypasses the cache and runs
// each tool once over all loci, annotating from its indexed output directly. The
// engine's annotation-value cache (st) is used either way.
func AnnotateLoci(ctx context.Context, cfg *config.Config, snap *config.Snapshot, st store.Store, selected []config.Annotation, loci []model.Locus, skipToolCache bool) (engine.AnnotateResult, error) {
	eng, cleanup, err := buildEngineForLoci(ctx, cfg, snap, st, selected, loci, skipToolCache)
	if err != nil {
		return engine.AnnotateResult{}, err
	}
	defer cleanup()
	return eng.Annotate(ctx, loci)
}

// buildEngineForLoci performs the once-per-request setup shared by the single-pass
// and chunked paths: it verifies file sources are present, runs any referenced tool
// sources ONCE over all loci (VEP/ANNOVAR batch — never per chunk), and builds the
// overlay/composite engine over the store. The returned cleanup removes the tool
// workdir and must run only after every eng.Annotate call has completed (the engine
// overlays the tools' output files from that workdir).
func buildEngineForLoci(ctx context.Context, cfg *config.Config, snap *config.Snapshot, st store.Store, selected []config.Annotation, loci []model.Locus, skipToolCache bool) (*engine.Engine, func(), error) {
	cleanup := func() {}
	// Every file source must be present; the engine annotates all of them.
	if err := RequireSources(cfg, snap, snap.Annotations); err != nil {
		return nil, cleanup, err
	}

	// Run any tool sources (VEP/ANNOVAR) referenced by the selected annotations over
	// the requested loci, projecting each tool's output as a source the engine
	// overlays. Selection-aware so an expensive tool isn't launched unless asked for.
	// The tool store is nil when bypassing the cache (bulk VCF) so RunToolsForLoci
	// runs the tool directly over all loci; the cached path needs a non-nil store.
	toolStore := st
	if skipToolCache {
		toolStore = nil
	}
	var toolSrcs []config.Source
	if tools := ReferencedTools(snap, selected); len(tools) > 0 && (toolStore != nil || skipToolCache) {
		if toolStore != nil {
			if err := toolStore.Init(ctx); err != nil {
				return nil, cleanup, err
			}
		}
		workdir, err := os.MkdirTemp("", "cganno-loci-tools-")
		if err != nil {
			return nil, cleanup, err
		}
		cleanup = func() { os.RemoveAll(workdir) }
		toolSrcs, err = annotate.RunToolsForLoci(ctx, cfg, tools, toolStore, loci, workdir, snap.Reference, snap.Assembly)
		if err != nil {
			cleanup()
			return nil, func() {}, err
		}
	}

	eng, err := NewEngineOverStore(ctx, cfg, snap, st, toolSrcs)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return eng, cleanup, nil
}

// AnnotateLociChunked annotates loci like AnnotateLoci, but splits a large locus set
// into contiguous chunks of ≤ chunkSize variants and annotates them concurrently
// (up to `threads` at once), so a single dominant source's per-locus lookups
// parallelize across cores instead of running as one goroutine over the whole input.
// Tools still run once over all loci (in buildEngineForLoci). A chunkSize ≤ 0, or a
// locus count within one chunk, runs a single pass. Results merge into one map;
// engine.BuildVariants restores input order, so chunk completion order is irrelevant.
func AnnotateLociChunked(ctx context.Context, cfg *config.Config, snap *config.Snapshot, st store.Store, selected []config.Annotation, loci []model.Locus, skipToolCache bool, chunkSize, threads int) (engine.AnnotateResult, error) {
	eng, cleanup, err := buildEngineForLoci(ctx, cfg, snap, st, selected, loci, skipToolCache)
	if err != nil {
		return engine.AnnotateResult{}, err
	}
	defer cleanup()

	// Warm GTF indexes ONCE before fanning out: EnsureIndexedGTF builds the tabix
	// index on first use with no locking, so concurrent chunks would otherwise race
	// on the same output. If a source has no buildable index, its overlay annotator
	// re-parses the whole GTF into memory on every call — catastrophic under chunking
	// — so fall back to a single pass in that case.
	canChunk := chunkSize > 0 && len(loci) > chunkSize
	if canChunk {
		for _, src := range snap.Sources {
			if !src.IsGTFSource() {
				continue
			}
			if _, _, err := fetch.EnsureIndexedGTF(cfg, src, false); err != nil {
				fmt.Fprintf(os.Stderr, "cganno: GTF source %s has no index (%v); annotating in a single pass — run `cganno download` to enable chunking\n", src.ID(), err)
				canChunk = false
				break
			}
		}
	}
	if !canChunk {
		return eng.Annotate(ctx, loci)
	}

	// Partition into contiguous index ranges and annotate concurrently.
	var chunks [][]model.Locus
	for i := 0; i < len(loci); i += chunkSize {
		end := i + chunkSize
		if end > len(loci) {
			end = len(loci)
		}
		chunks = append(chunks, loci[i:end])
	}

	results := make([]engine.AnnotateResult, len(chunks))
	g, gctx := errgroup.WithContext(ctx)
	if threads > 0 {
		g.SetLimit(threads)
	}
	for i, chunk := range chunks {
		i, chunk := i, chunk
		g.Go(func() error {
			res, err := eng.Annotate(gctx, chunk)
			if err != nil {
				return err
			}
			results[i] = res
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return engine.AnnotateResult{}, err
	}

	merged := engine.AnnotateResult{ByLocus: make(map[string][]model.AnnRow, len(loci)), Version: eng.Version()}
	for _, res := range results {
		for k, rows := range res.ByLocus {
			merged.ByLocus[k] = rows
		}
		merged.Novel += res.Novel
	}
	return merged, nil
}
