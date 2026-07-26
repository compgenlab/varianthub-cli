// Package fetch implements `cganno download`: fetching a snapshot's configured
// sources into the cache (one file at a time) and ensuring each is tabix-indexed
// (reuse a published .tbi/.csi, else build one via hts tabix.IndexWriter). A
// source with a `localpath` is used exactly and never downloaded; checksums are
// verified only when present. Multi-file sources (a {chrom} template or an
// explicit files list) are fetched one file at a time.
package fetch

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"github.com/compgenlab/cghts/htsio/tabix"
	"golang.org/x/sync/errgroup"

	"github.com/compgenlab/cganno/internal/checksum"
	"github.com/compgenlab/cganno/internal/config"
	"github.com/compgenlab/cganno/internal/software"
	"github.com/compgenlab/cganno/internal/tool"
)

// Result reports what happened for one source.
type Result struct {
	Source string // data_source_id
	Data   string // "downloaded" | "skipped" | "local" | "N downloaded, M skipped"
	Index  string // "reused" | "downloaded" | "built" | "present" | "linked" | aggregate, or "-"
}

// fileResult is the per-file outcome (data + index status).
type fileResult struct{ data, index string }

// logWriter receives `cganno download` progress lines (set to io.Discard for --quiet).
var logWriter io.Writer = os.Stderr

// SetLogWriter redirects fetch's progress logging (default os.Stderr).
func SetLogWriter(w io.Writer) { logWriter = w }

func logf(format string, a ...any) { fmt.Fprintf(logWriter, format+"\n", a...) }

// keepTemp, when set, leaves the per-source scratch directories (build recipe
// workdir, tool setup workdir) on disk instead of removing them — useful for
// debugging download/build/setup pipelines. Toggled by `download --keep-temp`.
var keepTemp bool

// SetKeepTemp controls whether fetch keeps its scratch directories (default false).
func SetKeepTemp(keep bool) { keepTemp = keep }

// cleanupTemp removes a scratch dir unless keepTemp is set, in which case it logs
// the retained path.
func cleanupTemp(dir, label string) {
	if keepTemp {
		logf("%s: kept temp dir %s", label, dir)
		return
	}
	os.RemoveAll(dir)
}

// algoOf returns the checksum algorithm of a "<algo>:..." spec ("" when absent).
func algoOf(spec string) string {
	if spec == "" {
		return ""
	}
	a, _, _ := strings.Cut(spec, ":")
	return a
}

// Snapshot downloads/indexes a snapshot's sources, fetching up to jobs files at once
// (jobs<1 ⇒ 1, sequential). Data sources are downloaded/built; type="tool" sources
// have their image acquired + one-time setup run (sequentially); builtins are skipped.
// If only != "" it restricts to that source name (or name:version). force re-does work.
func Snapshot(ctx context.Context, cfg *config.Config, snap *config.Snapshot, only string, force bool, jobs int) ([]Result, error) {
	if jobs < 1 {
		jobs = 1
	}
	type work struct {
		src     config.Source
		files   []config.SourceFile
		results []fileResult // one per file (distinct indices ⇒ safe concurrent writes)
	}
	var works []*work
	var builds, tools []config.Source
	matched := false
	for _, s := range snap.Sources {
		if s.IsBuiltinSource() {
			continue
		}
		if only != "" && s.Name != only && s.ID() != only {
			continue
		}
		matched = true
		if s.IsTool() { // acquire image + one-time setup, sequentially below
			tools = append(tools, s)
			continue
		}
		if s.Build != nil { // built from a recipe, run sequentially below
			builds = append(builds, s)
			continue
		}
		files := cfg.ResolveSourceFiles(s)
		works = append(works, &work{src: s, files: files, results: make([]fileResult, len(files))})
	}
	if only != "" && !matched {
		return nil, fmt.Errorf("no source %q in snapshot %q", only, snap.Name)
	}

	// gctx is the errgroup's context (cancelled once Wait returns); the original
	// ctx is kept for the build pass, which runs after Wait.
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(jobs)
	for _, w := range works {
		w := w
		for i := range w.files {
			i := i
			g.Go(func() error {
				ds, is, err := fetchFile(gctx, w.files[i], w.src.Format, w.src.ID(), force)
				if err != nil {
					return fmt.Errorf("%s: %w", w.src.ID(), err)
				}
				w.results[i] = fileResult{ds, is}
				return nil
			})
		}
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}

	results := make([]Result, 0, len(works)+len(builds)+len(tools))
	for _, w := range works {
		r := aggregate(w.src, w.results)
		// GTF sources are bgzip+tabix-indexed (cached, reused) so the gene model can
		// be queried by position instead of loaded whole into memory.
		if w.src.IsGTFSource() {
			_, status, err := EnsureIndexedGTF(cfg, w.src, force)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", w.src.ID(), err)
			}
			r.Index = status
		}
		results = append(results, r)
	}
	// Build sources (download inputs + run preprocessing) — sequential; heavy.
	for _, s := range builds {
		r, err := buildSource(ctx, cfg, s, force)
		if err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	// Tool sources: acquire the container image + run one-time setup — sequential.
	for _, s := range tools {
		r, err := setupToolSource(ctx, cfg, s, snap.Reference, force)
		if err != nil {
			return nil, err
		}
		results = append(results, r)
	}
	return results, nil
}

// buildSource runs a source's [[sources.build]] recipe: download Inputs into
// {inputs}/, fetch Assets (URL, or co-located in the source's version dir) into the
// workdir, run the Run steps (which must write {output}), then move the result into
// the cache and tabix-index it. Skips when already built unless force.
func buildSource(ctx context.Context, cfg *config.Config, src config.Source, force bool) (Result, error) {
	srcDir := cfg.SourceDir(src.Name, src.Version) // co-located assets live here
	out := cfg.ResolveSourcePath(src)
	hasIdx := fileExists(out+".tbi") || fileExists(out+".csi")
	if fileExists(out) && hasIdx && !force {
		logf("%s: already built (cached) — use --force to rebuild", src.ID())
		return Result{Source: src.ID(), Data: "built (cached)", Index: "reused"}, nil
	}

	if err := software.Check(src.ID(), src.Requires); err != nil {
		return Result{}, err
	}

	logf("%s: building source", src.ID())
	work, err := os.MkdirTemp("", "cganno-build-")
	if err != nil {
		return Result{}, err
	}
	defer cleanupTemp(work, src.ID())
	inputs := filepath.Join(work, "inputs")
	if err := os.MkdirAll(inputs, 0o755); err != nil {
		return Result{}, err
	}

	logf("  downloading %d input file(s) → %s", len(src.Build.Inputs), inputs)
	for _, u := range src.Build.Inputs {
		logf("    ↓ %s", path.Base(u))
		if err := download(ctx, u, filepath.Join(inputs, path.Base(u)), ""); err != nil {
			return Result{}, fmt.Errorf("%s: input %s: %w", src.ID(), u, err)
		}
	}
	for _, a := range src.Build.Assets {
		dst := filepath.Join(work, path.Base(a))
		if isHTTP(a) {
			logf("    asset (download) %s", path.Base(a))
			if err := download(ctx, a, dst, ""); err != nil {
				return Result{}, fmt.Errorf("%s: asset %s: %w", src.ID(), a, err)
			}
		} else {
			ap := os.ExpandEnv(a) // env vars allowed in local asset paths
			if !filepath.IsAbs(ap) {
				ap = filepath.Join(srcDir, ap)
			}
			logf("    asset %s", ap)
			if err := copyFile(ap, dst); err != nil {
				return Result{}, fmt.Errorf("%s: asset %s: %w", src.ID(), a, err)
			}
			os.Chmod(dst, 0o755) // helper scripts are often executable
		}
	}

	built := filepath.Join(work, "output")
	repl := strings.NewReplacer("{workdir}", work, "{inputs}", inputs, "{output}", built, "{threads}", "1")
	logf("  running %d build step(s):", len(src.Build.Run))
	for i, step := range src.Build.Run {
		rendered := repl.Replace(step)
		logf("    + %s", rendered) // echo the step like a Makefile recipe
		if err := runShell(ctx, work, rendered); err != nil {
			return Result{}, fmt.Errorf("%s: build step %d: %w", src.ID(), i+1, err)
		}
	}
	if !fileExists(built) {
		return Result{}, fmt.Errorf("%s: build steps produced no {output} file", src.ID())
	}

	logf("  caching → %s", out)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return Result{}, err
	}
	if err := moveFile(built, out); err != nil {
		return Result{}, err
	}
	for _, ext := range []string{".tbi", ".csi"} { // an index the build produced
		if fileExists(built + ext) {
			moveFile(built+ext, out+ext)
		}
	}
	idx, err := ensureIndex(ctx, config.SourceFile{Path: out}, out, src.Format, false)
	if err != nil {
		return Result{}, fmt.Errorf("%s: index: %w", src.ID(), err)
	}
	logf("  index: %s", idx)
	r, err := tabix.NewReader(out)
	if err != nil {
		return Result{}, fmt.Errorf("%s: verify %s: %w", src.ID(), out, err)
	}
	r.Close()
	return Result{Source: src.ID(), Data: "built", Index: idx}, nil
}

// runShell runs one templated build step via bash in dir (strict mode).
func runShell(ctx context.Context, dir, script string) error {
	cmd := exec.CommandContext(ctx, "bash", "-c", "set -euo pipefail\n"+script)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

func isHTTP(s string) bool {
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

// moveFile renames src→dst, falling back to copy+remove across filesystems.
func moveFile(src, dst string) error {
	if os.Rename(src, dst) == nil {
		return nil
	}
	if err := copyFile(src, dst); err != nil {
		return err
	}
	os.Remove(src)
	return nil
}

// aggregate summarizes a source's per-file results into one Result.
func aggregate(src config.Source, results []fileResult) Result {
	if len(results) == 1 {
		return Result{Source: src.ID(), Data: results[0].data, Index: results[0].index}
	}
	dl, skip := 0, 0
	idx := map[string]int{}
	for _, r := range results {
		if r.data == "downloaded" {
			dl++
		} else {
			skip++
		}
		idx[r.index]++
	}
	return Result{Source: src.ID(), Data: summarizeData(dl, skip, len(results)), Index: summarizeIndex(idx, len(results))}
}

// setupToolSource acquires a type="tool" source's container image (pull a registry
// ref, or download a .sif URL) and runs its one-time setup (install data into the
// tool's data dir), keyed by name/version. Setup is skipped when its sentinel exists,
// unless force. `ref` is the snapshot's reference FASTA. Called by Snapshot.
func setupToolSource(ctx context.Context, cfg *config.Config, src config.Source, ref string, force bool) (Result, error) {
	t := src.AsTool() // execution view
	res := Result{Source: t.ID() + " (tool)", Data: "-", Index: "-"}

	if err := software.Check(t.ID(), t.RequiredSoftware()); err != nil {
		return res, err
	}

	// Acquire the image.
	var img string
	if t.Image != "" {
		img = cfg.ResolveToolImage(t)
		if err := os.MkdirAll(filepath.Dir(img), 0o755); err != nil {
			return res, err
		}
		switch {
		case fileExists(img) && !force:
			logf("%s: image cached", t.ID())
			res.Data = "skipped"
		case t.ImageIsRef():
			logf("%s: pulling image %s", t.ID(), t.Image)
			os.Remove(img) // pull fails if the target exists
			if err := tool.PullImage(ctx, t, img); err != nil {
				return res, err
			}
			res.Data = "pulled"
		default:
			logf("%s: downloading image %s", t.ID(), t.Image)
			if err := download(ctx, t.Image, img, ""); err != nil {
				return res, fmt.Errorf("%s image: %w", t.ID(), err)
			}
			res.Data = "downloaded"
		}
	}

	// Run setup once (sentinel-gated).
	if len(t.Setup) > 0 {
		datadir := cfg.ResolveToolData(t)
		sentinel := filepath.Join(datadir, ".cganno-setup-done")
		if fileExists(sentinel) && !force {
			res.Index = "setup: skipped"
		} else {
			if err := os.MkdirAll(datadir, 0o755); err != nil {
				return res, err
			}
			wd, err := os.MkdirTemp("", "cganno-setup-")
			if err != nil {
				return res, err
			}
			logf("%s: running setup (one-time)", t.ID())
			err = tool.Setup(ctx, t, tool.Params{Image: img, Datadir: datadir, Ref: ref, Workdir: wd, AssetDir: cfg.SourceDir(src.Name, src.Version)})
			cleanupTemp(wd, t.ID()+" setup")
			if err != nil {
				return res, err
			}
			if err := os.WriteFile(sentinel, nil, 0o644); err != nil {
				return res, err
			}
			res.Index = "setup: done"
		}
	}
	return res, nil
}

// Source downloads a source's file(s) sequentially and ensures each is
// tabix-indexed. A builtin source is a no-op. (Snapshot fetches across sources
// concurrently; this is the single-source, in-order path.)
func Source(ctx context.Context, cfg *config.Config, src config.Source, force bool) (Result, error) {
	if src.IsBuiltinSource() {
		return Result{Source: src.ID(), Data: "-", Index: "-"}, nil
	}
	files := cfg.ResolveSourceFiles(src)
	results := make([]fileResult, len(files))
	for i, f := range files {
		ds, is, err := fetchFile(ctx, f, src.Format, src.ID(), force)
		if err != nil {
			return Result{}, err
		}
		results[i] = fileResult{ds, is}
	}
	r := aggregate(src, results)
	if src.IsGTFSource() {
		_, status, err := EnsureIndexedGTF(cfg, src, force)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", src.ID(), err)
		}
		r.Index = status
	}
	return r, nil
}

// fetchFile downloads one concrete source file (verifying its checksum when
// present) and ensures its tabix index, then confirms it opens. A local file
// (f.Local) is used exactly: it is never downloaded, and its index must be present
// alongside or pointed at by localpath_index.
func fetchFile(ctx context.Context, f config.SourceFile, format, label string, force bool) (data, index string, err error) {
	base := path.Base(f.Path)
	// A GTF source is read into memory; a BBI source (bigwig/bigbed) is self-indexed.
	// Neither is a tabix file, so skip indexing and the tabix-open verification.
	selfIndexed := format == "gtf" || format == "bigwig" || format == "bigbed"
	if f.Local {
		if !fileExists(f.Path) {
			return "", "", fmt.Errorf("localpath not found: %s", f.Path)
		}
		logf("%s: using local %s", label, f.Path)
		if selfIndexed {
			return "local", "none", nil
		}
		idx, err := ensureLocalIndex(f)
		if err != nil {
			return "", "", err
		}
		r, err := tabix.NewReader(f.Path)
		if err != nil {
			return "", "", fmt.Errorf("verify index %s: %w", f.Path, err)
		}
		r.Close()
		return "local", idx, nil
	}

	target := f.Path
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", "", err
	}
	if fileExists(target) && !force {
		logf("%s: cached %s", label, base)
		data = "skipped"
	} else {
		// A checksum is optional; when present it is fetched/verified.
		if a := algoOf(f.Checksum); a != "" {
			logf("%s: downloading %s (verifying %s)", label, base, a)
		} else {
			logf("%s: downloading %s", label, base)
		}
		sum, err := resolveChecksum(ctx, f.Checksum, base)
		if err != nil {
			return "", "", err
		}
		if err := download(ctx, f.URL, target, sum); err != nil {
			return "", "", err
		}
		data = "downloaded"
	}
	if selfIndexed {
		return data, "none", nil
	}
	index, err = ensureIndex(ctx, f, target, format, force)
	if err != nil {
		return "", "", err
	}
	logf("%s: index %s (%s)", label, base, index)
	r, err := tabix.NewReader(target)
	if err != nil {
		return "", "", fmt.Errorf("verify index %s: %w", target, err)
	}
	r.Close()
	return data, index, nil
}

// ensureLocalIndex makes a local source's index findable by hts (alongside the
// data file as <data>.tbi/.csi). When localpath_index points elsewhere, it is
// symlinked (best-effort copy fallback) into place. Errors if no index is found.
func ensureLocalIndex(f config.SourceFile) (string, error) {
	if fileExists(f.Path+".tbi") || fileExists(f.Path+".csi") {
		return "present", nil
	}
	if f.IndexPath != "" {
		if !fileExists(f.IndexPath) {
			return "", fmt.Errorf("localpath_index not found: %s", f.IndexPath)
		}
		ext := ".tbi"
		if strings.HasSuffix(f.IndexPath, ".csi") {
			ext = ".csi"
		}
		link := f.Path + ext
		if err := os.Symlink(f.IndexPath, link); err != nil {
			if cerr := copyFile(f.IndexPath, link); cerr != nil {
				return "", fmt.Errorf("link local index %s -> %s: %v", f.IndexPath, link, err)
			}
		}
		return "linked", nil
	}
	return "", fmt.Errorf("no index for local source %s (expected %s.tbi/.csi or localpath_index)", f.Path, f.Path)
}

// ensureIndex reuses an existing/published index or builds one in place. An explicit
// url_index is downloaded directly (checksum-verified when present); otherwise we
// guess one alongside the data url, falling back to building from a format preset.
func ensureIndex(ctx context.Context, f config.SourceFile, target, format string, force bool) (string, error) {
	if !force {
		if fileExists(target+".tbi") || fileExists(target+".csi") {
			return "reused", nil
		}
	}
	if f.URLIndex != "" {
		ext := ".tbi"
		if strings.HasSuffix(f.URLIndex, ".csi") {
			ext = ".csi"
		}
		sum, err := resolveChecksum(ctx, f.ChecksumIndex, path.Base(target)+ext)
		if err != nil {
			return "", err
		}
		if err := download(ctx, f.URLIndex, target+ext, sum); err != nil {
			return "", fmt.Errorf("download index: %w", err)
		}
		return "downloaded", nil
	}
	for _, ext := range []string{".tbi", ".csi"} {
		if err := download(ctx, f.URL+ext, target+ext, ""); err == nil {
			return "downloaded", nil
		}
	}
	opts, err := presetFor(format)
	if err != nil {
		return "", err
	}
	if err := tabix.NewIndexWriter(opts).WriteIndex(target); err != nil {
		return "", fmt.Errorf("build index: %w", err)
	}
	return "built", nil
}

// Missing returns a source's resolved files (data and/or index) that are not present
// on disk. A fully-present source returns nil. Builtin sources have no files.
func Missing(cfg *config.Config, src config.Source) []string {
	if src.IsBuiltinSource() {
		return nil
	}
	if src.IsGeneList() {
		// A genelist has no data file of its own: it needs the referenced GTF
		// present (indexed) and its genes_file, if any.
		var missing []string
		if src.GTFRef != nil {
			missing = append(missing, Missing(cfg, *src.GTFRef)...)
		}
		if p := cfg.GenesFilePath(src); p != "" && !fileExists(p) {
			missing = append(missing, p+" (genes_file)")
		}
		return missing
	}
	var missing []string
	for _, f := range cfg.ResolveSourceFiles(src) {
		if !fileExists(f.Path) {
			missing = append(missing, f.Path)
			continue
		}
		if src.IsGTFSource() {
			// A GTF is queried via a bgzip+tabix index built under cache_dir; report
			// it missing until `cganno download` has produced it (unless the raw file
			// is already a bgzipped GTF with a sidecar index).
			if strings.HasSuffix(f.Path, ".gz") && (fileExists(f.Path+".tbi") || fileExists(f.Path+".csi")) {
				continue
			}
			idx := cfg.ResolveGTFIndexPath(src)
			if !fileExists(idx) || !(fileExists(idx+".tbi") || fileExists(idx+".csi")) {
				missing = append(missing, idx+" (GTF index)")
			}
			continue
		}
		if src.IsBBISource() {
			continue // BBI self-indexed; no sidecar index expected
		}
		hasIndex := fileExists(f.Path+".tbi") || fileExists(f.Path+".csi") ||
			(f.IndexPath != "" && fileExists(f.IndexPath))
		if !hasIndex {
			missing = append(missing, f.Path+" (index)")
		}
	}
	return missing
}

// resolveChecksum turns a "<algo>:<hex-or-url>" spec into a concrete "<algo>:<hex>".
// A URL value is fetched and parsed (md5sum-style manifest matched by filename, or a
// single hash). Empty spec → empty (no verification).
func resolveChecksum(ctx context.Context, spec, dataName string) (string, error) {
	if spec == "" {
		return "", nil
	}
	algo, value, isURL, err := checksum.Parse(spec)
	if err != nil {
		return "", err
	}
	if !isURL {
		return spec, nil
	}
	body, err := httpGet(ctx, value)
	if err != nil {
		return "", fmt.Errorf("fetch checksum %s: %w", value, err)
	}
	hexv, err := extractHash(string(body), dataName)
	if err != nil {
		return "", fmt.Errorf("checksum %s: %w", value, err)
	}
	return algo + ":" + hexv, nil
}

// extractHash pulls a hex digest from a checksum file: the line whose filename
// matches dataName, else the sole hash when the file has exactly one entry.
func extractHash(body, dataName string) (string, error) {
	var lone string
	n := 0
	for _, ln := range strings.Split(body, "\n") {
		flds := strings.Fields(ln)
		if len(flds) == 0 {
			continue
		}
		if len(flds) >= 2 && path.Base(strings.TrimPrefix(flds[1], "*")) == dataName {
			return flds[0], nil
		}
		n++
		lone = flds[0]
	}
	switch {
	case n == 1:
		return lone, nil
	case n == 0:
		return "", fmt.Errorf("empty checksum file")
	default:
		return "", fmt.Errorf("no entry for %q in checksum manifest", dataName)
	}
}

// presetFor maps a source format to a tabix column preset for index building.
func presetFor(format string) (*tabix.WriterOpts, error) {
	switch format {
	case "vcf", "":
		return tabix.NewWriterOpts().VCF(), nil
	case "bed":
		return tabix.NewWriterOpts().BED(), nil
	case "tab":
		return tabix.NewWriterOpts().Columns(1, 2, 0).Meta('#'), nil
	case "gtf", "gff":
		return tabix.NewWriterOpts().GFF(), nil
	default:
		return nil, fmt.Errorf("cannot build index for format %q (want vcf|bed|tab|gtf)", format)
	}
}

// EnsureIndexedGTF returns a bgzipped + tabix(GFF)-indexed path for a GTF source,
// building it once under cache_dir and reusing it on later calls (so `cganno
// download` produces it and annotation is O(1) memory per query). If the source's
// raw file is already a bgzipped GTF with a sidecar .tbi/.csi, it is used directly.
// status is "pre-indexed" | "reused" | "built". force rebuilds the cached index.
func EnsureIndexedGTF(cfg *config.Config, src config.Source, force bool) (path, status string, err error) {
	raw := cfg.ResolveSourcePath(src)
	if !fileExists(raw) {
		return "", "", fmt.Errorf("GTF source file %s not found (run `cganno download`)", raw)
	}
	// A .gz raw with a sidecar tabix index is already usable directly.
	if strings.HasSuffix(raw, ".gz") && (fileExists(raw+".tbi") || fileExists(raw+".csi")) {
		return raw, "pre-indexed", nil
	}
	idx := cfg.ResolveGTFIndexPath(src)
	if idx == raw {
		return "", "", fmt.Errorf("GTF index path %s collides with the source file", idx)
	}
	if !force && fileExists(idx) && (fileExists(idx+".tbi") || fileExists(idx+".csi")) {
		return idx, "reused", nil
	}
	if err := buildGTFIndex(raw, idx); err != nil {
		return "", "", fmt.Errorf("index GTF %s: %w", raw, err)
	}
	return idx, "built", nil
}

// buildGTFIndex streams a GTF (plain / gzip / bgzip) into a sorted, bgzipped,
// GFF-tabix-indexed file. The tabix writer sorts by coordinate with a bounded
// memory footprint (spilling to temp BGZF), so even a whole GENCODE GTF indexes in
// one pass without loading it all into memory. On any failure the partial output +
// index are removed so a later run rebuilds rather than treating them as cached.
func buildGTFIndex(raw, idx string) (err error) {
	if err := os.MkdirAll(filepath.Dir(idx), 0o755); err != nil {
		return err
	}
	in, err := openMaybeGz(raw)
	if err != nil {
		return err
	}
	defer in.Close()

	w := tabix.NewWriter(idx, tabix.NewWriterOpts().GFF().AutoIndex())
	cleanup := func() { os.Remove(idx); os.Remove(idx + ".tbi"); os.Remove(idx + ".csi") }

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if len(line) == 0 || line[0] == '#' {
			continue // GTF comment/meta lines are not indexed data
		}
		if err := w.Write(line); err != nil {
			w.Close()
			cleanup()
			return err
		}
	}
	if err := sc.Err(); err != nil {
		w.Close()
		cleanup()
		return err
	}
	if err := w.Close(); err != nil {
		cleanup()
		return err
	}
	return nil
}

// openMaybeGz opens path, transparently decompressing a .gz/.bgz suffix (BGZF is a
// series of gzip members, read in multistream mode by compress/gzip).
func openMaybeGz(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".gz") || strings.HasSuffix(path, ".bgz") {
		gz, err := gzip.NewReader(bufio.NewReader(f))
		if err != nil {
			f.Close()
			return nil, err
		}
		return gzCloser{gz, f}, nil
	}
	return f, nil
}

// gzCloser closes the gzip reader then the underlying file.
type gzCloser struct {
	*gzip.Reader
	f *os.File
}

func (c gzCloser) Close() error {
	err := c.Reader.Close()
	if e := c.f.Close(); e != nil && err == nil {
		err = e
	}
	return err
}

// download streams url to dest atomically (via a .tmp + rename). When sum is a
// non-empty "<algo>:<hex>" spec, the content is hashed while streaming and verified
// before the rename — a mismatch removes the tmp and fails.
func download(ctx context.Context, url, dest, sum string) error {
	v, err := checksum.New(sum)
	if err != nil {
		return err
	}
	resp, err := httpDo(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}

	tmp := dest + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	var w io.Writer = f
	if v != nil {
		w = io.MultiWriter(f, v)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("download %s: %w", url, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := v.Check(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("%s: %w", url, err)
	}
	return os.Rename(tmp, dest)
}

// copyFile copies src to dst (used as a symlink fallback for local indexes).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

func httpGet(ctx context.Context, url string) ([]byte, error) {
	resp, err := httpDo(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

func httpDo(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	return http.DefaultClient.Do(req)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func summarizeData(dl, skip, n int) string {
	if n == 1 {
		if dl == 1 {
			return "downloaded"
		}
		return "skipped"
	}
	return fmt.Sprintf("%d downloaded, %d skipped (%d files)", dl, skip, n)
}

func summarizeIndex(m map[string]int, n int) string {
	if n == 1 {
		for k := range m {
			return k
		}
		return "-"
	}
	var parts []string
	for _, k := range []string{"reused", "downloaded", "built", "present", "linked"} {
		if m[k] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", m[k], k))
		}
	}
	return strings.Join(parts, ", ")
}

// Describe renders a one-line summary of a result.
func (r Result) Describe() string {
	return fmt.Sprintf("%-24s data:%-28s index:%s", r.Source, r.Data, r.Index)
}
