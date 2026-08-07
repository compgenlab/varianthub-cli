// Package fetch implements `varhub download`: fetching a snapshot's configured
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
	"github.com/compgenlab/varianthub-cli/internal/htsidx"
	"github.com/compgenlab/varianthub-cli/internal/objstore"
	"golang.org/x/sync/errgroup"

	"github.com/compgenlab/varianthub-cli/internal/checksum"
	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/software"
	"github.com/compgenlab/varianthub-cli/internal/tool"
)

// Result reports what happened for one source.
type Result struct {
	Source string // data_source_id
	Data   string // "downloaded" | "skipped" | "local" | "N downloaded, M skipped"
	Index  string // "reused" | "downloaded" | "built" | "present" | "linked" | aggregate, or "-"
	Freed  int64  // bytes reclaimed by pruning a converted GTF's original

	// Files is what the source now occupies in the cache, filled in by the
	// JSON output path. Consumers that need to record what a download produced
	// -- a server tracking provisioned sources, say -- would otherwise have to
	// scan the cache, which is not possible when it is a bucket.
	Files []FileInfo `json:"files,omitempty"`
}

// pruneRaw removes a converted GTF's original, logging rather than failing the
// download: the data is already usable, so a stuck original is untidy, not fatal.
func pruneRaw(cfg *config.Config, src config.Source) int64 {
	freed, err := PruneRawGTF(cfg, src)
	if err != nil {
		logf("%s: %v", src.ID(), err)
		return 0
	}
	if freed > 0 {
		logf("%s: removed the unprocessed download (%s reclaimed; --keep-raw to keep it)",
			src.ID(), HumanBytes(freed))
	}
	return freed
}

// HumanBytes formats a byte count for progress output.
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit && exp < 3; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// fileResult is the per-file outcome (data + index status).
type fileResult struct{ data, index string }

// logWriter receives `varhub download` progress lines (set to io.Discard for --quiet).
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

// ignoreStream provisions sources that declare `stream = true` anyway.
//
// The flag in a manifest is the publisher saying "you do not need a copy of
// this" — reasonable for a 24 GB file queried at a handful of loci. It is not a
// policy: an operator running whole genomes, or one who wants results pinned to
// bytes that cannot change underneath them, wants the copy. Toggled by
// `download --no-stream`.
var ignoreStream bool

// SetIgnoreStream controls whether `stream = true` sources are downloaded
// anyway (default false).
func SetIgnoreStream(ignore bool) { ignoreStream = ignore }

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
func Snapshot(ctx context.Context, cfg *config.Config, snap *config.Snapshot, only string, force, keepRaw bool, jobs int) ([]Result, error) {
	if jobs < 1 {
		jobs = 1
	}
	if err := checkRemoteReady(ctx, cfg.CacheDirAbs()); err != nil {
		return nil, err
	}
	remoteCache := objstore.IsObject(cfg.CacheDirAbs())
	type work struct {
		src     config.Source
		files   []config.SourceFile
		results []fileResult // one per file (distinct indices ⇒ safe concurrent writes)
	}
	var works []*work
	var builds, tools, remoteGTFs, streamed []config.Source
	matched := false
	for _, s := range snap.Sources {
		if s.IsBuiltinSource() {
			continue
		}
		if s.Stream && !ignoreStream {
			// Read in place; there is nothing to fetch. Reported so the run
			// still accounts for every source in the snapshot.
			streamed = append(streamed, s)
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
		if remoteCache && s.IsGTFSource() {
			// Download, convert and upload as one staged unit — see fetchGTFRemote.
			remoteGTFs = append(remoteGTFs, s)
			continue
		}
		files := cfg.ResolveSourceTargets(s)
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

	results := make([]Result, 0, len(works)+len(builds)+len(tools)+len(remoteGTFs)+len(streamed))
	for _, s := range streamed {
		results = append(results, Result{Source: s.ID(), Data: "streamed", Index: "remote"})
	}
	for _, s := range remoteGTFs {
		r, err := fetchGTFRemote(ctx, cfg, s, force, keepRaw)
		if err != nil {
			return nil, err
		}
		results = append(results, r)
	}
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
			if !keepRaw {
				r.Freed = pruneRaw(cfg, w.src)
			}
		}
		if err := recordLocations(ctx, cfg, w.src); err != nil {
			return nil, fmt.Errorf("%s: recording locations: %w", w.src.ID(), err)
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
	outPresent, err := locatorExists(ctx, out)
	if err != nil {
		return Result{}, err
	}
	_, hasIdx, err := anyIndexAt(ctx, out)
	if err != nil {
		return Result{}, err
	}
	if outPresent && hasIdx && !force {
		logf("%s: already built (cached) — use --force to rebuild", src.ID())
		return Result{Source: src.ID(), Data: "built (cached)", Index: "reused"}, nil
	}

	if err := software.Check(src.ID(), src.Requires); err != nil {
		return Result{}, err
	}
	if src.Build != nil {
		// Before any input is fetched: a REVEL build downloads 175 files before
		// its first step would have discovered a missing converter.
		if err := checkAssets(cfg, src.ID(), src.Name, src.Version, src.Build.Assets); err != nil {
			return Result{}, err
		}
	}

	logf("%s: building source", src.ID())
	work, err := os.MkdirTemp("", "varhub-build-")
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

	// A remote cache is published from the build workdir: the recipe output is
	// already local and already temporary, so it is indexed and verified where
	// it stands and only the verified pair is uploaded.
	if objstore.IsObject(out) {
		idx, err := ensureIndex(ctx, config.SourceFile{Path: built}, built, src.Format, force)
		if err != nil {
			return Result{}, fmt.Errorf("%s: index: %w", src.ID(), err)
		}
		r, err := tabix.NewReader(built)
		if err != nil {
			return Result{}, fmt.Errorf("%s: verify %s: %w", src.ID(), built, err)
		}
		r.Close()
		st := &staging{work: built} // dir left empty: cleanupTemp owns the workdir
		if err := st.publish(ctx, out, "", ".tbi", ".csi"); err != nil {
			return Result{}, err
		}
		logf("  index: %s, uploaded", idx)
		return Result{Source: src.ID(), Data: "built", Index: idx}, nil
	}

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
	// force matters here: the data file was just replaced, so a sidecar left by
	// the previous build describes bytes that no longer exist.
	idx, err := ensureIndex(ctx, config.SourceFile{Path: out}, out, src.Format, force)
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
	// Before the image is pulled: a missing 2 KB script should not cost a 1.5 GB
	// download to discover.
	if err := checkAssets(cfg, t.ID(), src.Name, src.Version, t.Assets); err != nil {
		return res, err
	}

	// Acquire the image.
	var img string
	if t.Image != "" {
		img = cfg.ResolveToolImage(t)
		if err := os.MkdirAll(filepath.Dir(img), 0o755); err != nil {
			return res, err
		}
		// Where the built image is kept durably, when the cache is an object
		// store. A SIF is a single immutable blob, so unlike the tool's data it
		// belongs there without reservation — and a worker that starts with an
		// empty disk then fetches one file instead of re-pulling and
		// re-converting a multi-gigabyte OCI image.
		obj := cfg.ToolImageObject(t)

		switch {
		case fileExists(img) && !force:
			logf("%s: image cached", t.ID())
			res.Data = "skipped"
		case obj != "" && !force && objectExists(ctx, obj):
			// Pinned bytes, not just a faster pull: `apptainer pull` of a
			// mutable tag can produce a different image later, and every worker
			// running the same annotation should run the same one.
			logf("%s: fetching cached image", t.ID())
			if err := fetchObject(ctx, obj, img); err != nil {
				return res, fmt.Errorf("%s image: %w", t.ID(), err)
			}
			res.Data = "fetched"
		case t.ImageIsRef():
			logf("%s: pulling image %s", t.ID(), t.Image)
			os.Remove(img) // pull fails if the target exists
			if err := tool.PullImage(ctx, t, img); err != nil {
				return res, err
			}
			if obj != "" {
				logf("%s: caching image", t.ID())
				if err := putObject(ctx, img, obj); err != nil {
					// The image is built and usable; failing the whole download
					// because the copy did not upload would throw away work
					// that succeeded.
					logf("%s: could not cache image: %v", t.ID(), err)
				}
			}
			res.Data = "pulled"
		default:
			logf("%s: downloading image %s", t.ID(), t.Image)
			// A prebuilt image may live in an object store as readily as on a
			// web server; both are just places a file comes from.
			if objstore.IsObject(t.Image) {
				if err := fetchObject(ctx, t.Image, img); err != nil {
					return res, fmt.Errorf("%s image: %w", t.ID(), err)
				}
				if err := checksum.Verify(img, t.ImageChecksum); err != nil {
					os.Remove(img)
					return res, fmt.Errorf("%s image: %w", t.ID(), err)
				}
			} else if err := download(ctx, t.Image, img, t.ImageChecksum); err != nil {
				return res, fmt.Errorf("%s image: %w", t.ID(), err)
			}
			if obj != "" {
				if err := putObject(ctx, img, obj); err != nil {
					logf("%s: could not cache image: %v", t.ID(), err)
				}
			}
			res.Data = "downloaded"
		}
	}

	// Run setup once (sentinel-gated).
	if len(t.Setup) > 0 {
		datadir := cfg.ResolveToolData(t)
		sentinel := filepath.Join(datadir, ".varhub-setup-done")
		switch {
		case fileExists(sentinel) && !force:
			res.Index = "setup: skipped"
			// The archive is separate state from the setup, so a run that finds
			// the work already done still checks whether the copy exists.
			//
			// Publishing used to happen only in the branch that ran setup, which
			// made a failed upload unrecoverable: the sentinel says "done", so
			// every later run skips, and the only way back is --force and the
			// whole install again. That is days for VEP, to retry a step that
			// takes minutes and had failed for a passing reason — a full disk,
			// an expired credential, a bucket not yet created.
			//
			// Keyed on the object being absent rather than on a flag, so the
			// restored branch needs no special case: data that came from the
			// archive is data whose archive exists.
			if obj := toolDataObject(cfg, t); obj != "" && !objectExists(ctx, obj) {
				logf("%s: setup is done but not archived; uploading", t.ID())
				publishToolData(ctx, cfg, t, datadir, logf)
				res.Index = "setup: skipped, archived"
			}
		// A machine with no data of its own, when someone has already done this
		// work and published it. Unpacking a tarball is minutes where running
		// setup can be hours.
		case !force && t.ToolCache != "" && restoreToolData(ctx, cfg, t, datadir, logf):
			if err := os.WriteFile(sentinel, nil, 0o644); err != nil {
				return res, err
			}
			res.Index = "setup: restored"
		default:
			if err := os.MkdirAll(datadir, 0o755); err != nil {
				return res, err
			}
			wd, err := os.MkdirTemp("", "varhub-setup-")
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
			if t.ToolCache != "" {
				publishToolData(ctx, cfg, t, datadir, logf)
			}
			res.Index = "setup: done"
		}
	}
	return res, nil
}

// Source downloads a source's file(s) sequentially and ensures each is
// tabix-indexed. A builtin source is a no-op. (Snapshot fetches across sources
// concurrently; this is the single-source, in-order path.)
func Source(ctx context.Context, cfg *config.Config, src config.Source, force, keepRaw bool) (Result, error) {
	if src.IsBuiltinSource() {
		return Result{Source: src.ID(), Data: "-", Index: "-"}, nil
	}
	if src.Stream && !ignoreStream {
		return Result{Source: src.ID(), Data: "streamed", Index: "remote"}, nil
	}
	if err := checkRemoteReady(ctx, cfg.CacheDirAbs()); err != nil {
		return Result{}, err
	}
	if src.IsReference() {
		// Indexed by sequence offset rather than by coordinate, and always
		// local: a tool step binds the FASTA's directory into a container.
		return fetchReference(ctx, cfg, src, force)
	}
	if objstore.IsObject(cfg.CacheDirAbs()) && src.IsGTFSource() {
		return fetchGTFRemote(ctx, cfg, src, force, keepRaw)
	}
	files := cfg.ResolveSourceTargets(src)
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
		if !keepRaw {
			r.Freed = pruneRaw(cfg, src)
		}
	}
	if err := recordLocations(ctx, cfg, src); err != nil {
		return Result{}, fmt.Errorf("%s: recording locations: %w", src.ID(), err)
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

	if objstore.IsObject(f.Path) {
		return fetchFileRemote(ctx, f, format, label, selfIndexed, force)
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
	if err := htsidx.WriteIndex(opts, target); err != nil {
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
	if src.Stream && !src.HasLocations() {
		// Nothing is expected on disk. Whether the origin is reachable is a
		// question for the annotate open, which reports it precisely; probing
		// every streamed source here would add a network round trip to every
		// run just to produce a worse error. A recorded location means a copy
		// was downloaded anyway, and that copy is checked like any other.
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
	// Which check to run follows the *files*, not the cache directory. A source
	// can resolve somewhere other than cache_dir — an overlay names its own root,
	// which is how one job reads sources from several places — so deciding from
	// cache_dir alone tests an s3:// locator with os.Stat and calls a perfectly
	// good source missing. That reads as "sources not downloaded" naming an
	// object that is plainly there.
	if objstore.IsObject(cfg.CacheDirAbs()) || resolvesToObject(cfg, src) {
		return missingRemote(context.Background(), cfg, src)
	}
	var missing []string
	for _, f := range cfg.ResolveSourceFiles(src) {
		if src.IsGTFSource() {
			// A GTF is queried through the bgzip+tabix copy under cache_dir, and
			// that copy is sufficient on its own — `download` prunes the original
			// once it has been converted. So check the usable file first: testing
			// the raw up front reports a perfectly good source as missing.
			if strings.HasSuffix(f.Path, ".gz") && fileExists(f.Path) &&
				(fileExists(f.Path+".tbi") || fileExists(f.Path+".csi")) {
				continue // shipped pre-indexed: the raw IS the queried file
			}
			idx := cfg.ResolveGTFIndexPath(src)
			if fileExists(idx) && (fileExists(idx+".tbi") || fileExists(idx+".csi")) {
				continue
			}
			// Nothing usable. Name whichever piece is actually absent.
			if !fileExists(f.Path) {
				missing = append(missing, f.Path)
			} else {
				missing = append(missing, idx+" (GTF index)")
			}
			continue
		}
		if !fileExists(f.Path) {
			missing = append(missing, f.Path)
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
// building it once under cache_dir and reusing it on later calls (so `varhub
// download` produces it and annotation is O(1) memory per query). If the source's
// raw file is already a bgzipped GTF with a sidecar .tbi/.csi, it is used directly.
// status is "pre-indexed" | "reused" | "built". force rebuilds the cached index.
func EnsureIndexedGTF(cfg *config.Config, src config.Source, force bool) (path, status string, err error) {
	raw := cfg.ResolveSourcePath(src)
	if objstore.IsObject(raw) {
		return ensureIndexedGTFRemote(context.Background(), cfg, src, raw)
	}
	// A .gz raw with a sidecar tabix index is already usable directly.
	if strings.HasSuffix(raw, ".gz") && fileExists(raw) &&
		(fileExists(raw+".tbi") || fileExists(raw+".csi")) {
		return raw, "pre-indexed", nil
	}
	idx := cfg.ResolveGTFIndexPath(src)
	if idx == raw {
		return "", "", fmt.Errorf("GTF index path %s collides with the source file", idx)
	}
	if !force && fileExists(idx) && (fileExists(idx+".tbi") || fileExists(idx+".csi")) {
		return idx, "reused", nil
	}
	// The raw is needed only to *build* the index. Checking it here rather than
	// up front is what lets `download` prune the original after a successful
	// conversion: the reuse path above then keeps working without it.
	if !fileExists(raw) {
		return "", "", fmt.Errorf("GTF source file %s not found (run `varhub download`)", raw)
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
// PruneRawGTF deletes a GTF's downloaded original once it has been converted to
// the bgzipped, tabix-indexed copy that queries actually use.
//
// GENCODE and friends ship plain gzip, which tabix cannot seek into, so the
// conversion writes a second file — larger than the original, because BGZF
// resets the compressor every block to make random access possible. Keeping both
// doubles the footprint for a file nothing reads again except a re-index, and
// `--force` re-downloads anyway.
//
// Refuses to act unless the derived file *and* its index are both present, and
// never touches a raw that is itself the queried file (a source that shipped
// pre-indexed). Returns the bytes freed; 0 when there was nothing to prune.
func PruneRawGTF(cfg *config.Config, src config.Source) (freed int64, err error) {
	raw := cfg.ResolveSourcePath(src)
	idx := cfg.ResolveGTFIndexPath(src)
	if raw == idx {
		return 0, nil
	}
	if objstore.IsObject(raw) {
		return pruneRawGTFRemote(context.Background(), raw, idx)
	}
	// A pre-indexed raw is what queries read. Deleting it would remove the data.
	if fileExists(raw+".tbi") || fileExists(raw+".csi") {
		return 0, nil
	}
	// Only prune when a usable replacement exists, or this deletes the only copy.
	if !fileExists(idx) || !(fileExists(idx+".tbi") || fileExists(idx+".csi")) {
		return 0, nil
	}
	fi, statErr := os.Stat(raw)
	if statErr != nil {
		return 0, nil // already gone
	}
	if err := os.Remove(raw); err != nil {
		return 0, fmt.Errorf("prune %s: %w", raw, err)
	}
	return fi.Size(), nil
}

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

// httpExists reports whether a URL serves something, without fetching it.
//
// Used to find out whether an upstream publishes an index beside its data. A
// HEAD rather than a GET because the answer decides which path provisioning
// takes, and asking must not cost a download.
func httpExists(ctx context.Context, url string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// upstreamIndex finds the index an upstream publishes for a file: the explicit
// url_index, or a sibling beside the data URL.
//
// Whether one exists decides whether provisioning needs scratch at all. An
// index that is published is just another file to copy; one that has to be
// built has to be built *from* the data, which means the data on a disk.
func upstreamIndex(ctx context.Context, f config.SourceFile) (url, ext, sum string) {
	if f.URLIndex != "" {
		ext = ".tbi"
		if strings.HasSuffix(f.URLIndex, ".csi") {
			ext = ".csi"
		}
		return f.URLIndex, ext, f.ChecksumIndex
	}
	for _, e := range []string{".tbi", ".csi"} {
		if httpExists(ctx, f.URL+e) {
			return f.URL + e, e, ""
		}
	}
	return "", "", ""
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

// fetchFileRemote provisions one file into an object-store cache.
//
// Everything happens on a local staging copy first — checksum, index, and the
// tabix open that proves the pair actually works — because none of those can be
// done against a bucket, and because an object that failed verification must
// never be uploaded at all. Only a verified pair is published.
func fetchFileRemote(ctx context.Context, f config.SourceFile, format, label string,
	selfIndexed, force bool) (data, index string, err error) {

	base := objstore.Base(f.Path)

	// Complete means the data *and* its index are both present. Checking them
	// together matters: an interrupted run can leave data with no index, and
	// treating that as cached would provision a source that cannot be queried.
	if !force {
		ok, err := locatorExists(ctx, f.Path)
		if err != nil {
			return "", "", err
		}
		if ok {
			if selfIndexed {
				logf("%s: cached %s", label, base)
				return "skipped", "none", nil
			}
			if _, hasIdx, err := anyIndexAt(ctx, f.Path); err != nil {
				return "", "", err
			} else if hasIdx {
				logf("%s: cached %s", label, base)
				return "skipped", "reused", nil
			}
			logf("%s: %s has no index in the cache; rebuilding it", label, base)
		}
	}

	if a := algoOf(f.Checksum); a != "" {
		logf("%s: downloading %s (verifying %s)", label, base, a)
	} else {
		logf("%s: downloading %s", label, base)
	}
	sum, err := resolveChecksum(ctx, f.Checksum, base)
	if err != nil {
		return "", "", err
	}

	// Scratch is for work, not for transit. Nothing here reads the data unless
	// an index has to be built from it, and building is the only reason a
	// multi-gigabyte file needs to exist on a disk on the way past — the digest
	// can be taken from the stream. A source with no index at all, or one whose
	// index the upstream already publishes, streams straight into the store.
	idxURL, idxExt, idxSum := "", "", ""
	if !selfIndexed {
		idxURL, idxExt, idxSum = upstreamIndex(ctx, f)
	}
	if selfIndexed || idxURL != "" {
		// Index first, so an interrupted run never leaves data that looks
		// complete with no index beside it — the same order publish() uses.
		if idxURL != "" {
			s, cErr := resolveChecksum(ctx, idxSum, base+idxExt)
			if cErr != nil {
				return "", "", cErr
			}
			if err := streamToObject(ctx, idxURL, f.Path+idxExt, s); err != nil {
				return "", "", fmt.Errorf("stream index: %w", err)
			}
		}
		if err := streamToObject(ctx, f.URL, f.Path, sum); err != nil {
			return "", "", err
		}
		if idxURL != "" {
			// Prove the pair works before it becomes the copy everyone reads —
			// the same check the staging path makes, made against the objects by
			// range request rather than against a local copy. Cheap: opening a
			// tabix file reads its header and index, not its contents.
			if err := verifyPair(ctx, f.Path, idxExt); err != nil {
				return "", "", err
			}
			logf("%s: index %s (downloaded), streamed", label, base)
			return "downloaded", "downloaded", nil
		}
		logf("%s: streamed %s", label, base)
		return "downloaded", "none", nil
	}

	// An index has to be built, which needs the data on a disk.
	st, err := newStaging(f.Path)
	if err != nil {
		return "", "", err
	}
	defer st.close()

	if err := download(ctx, f.URL, st.work, sum); err != nil {
		return "", "", err
	}

	index, err = ensureIndex(ctx, f, st.work, format, true)
	if err != nil {
		return "", "", err
	}
	// Prove the pair works before it becomes the copy everyone reads.
	r, err := tabix.NewReader(st.work)
	if err != nil {
		return "", "", fmt.Errorf("verify index %s: %w", base, err)
	}
	r.Close()

	if err := st.publish(ctx, f.Path, f.Checksum, ".tbi", ".csi"); err != nil {
		return "", "", err
	}
	logf("%s: index %s (%s), uploaded", label, base, index)
	return "downloaded", index, nil
}

// fetchGTFRemote provisions a GTF source into an object-store cache as one unit.
//
// GTF is handled apart from the generic path because its useful artifact is not
// what was downloaded: varhub bgzips and tabix-indexes the original, and prunes
// the original afterwards unless --keep-raw. Running that through the generic
// path would upload a multi-gigabyte original only to delete it again, so the
// original is staged, converted locally, and only the converted pair is
// published — which is what the local cache ends up holding anyway.
func fetchGTFRemote(ctx context.Context, cfg *config.Config, src config.Source,
	force, keepRaw bool) (Result, error) {

	label := src.ID()
	files := cfg.ResolveSourceTargets(src)
	if len(files) != 1 {
		return Result{}, fmt.Errorf("%s: a GTF source must resolve to one file, got %d", label, len(files))
	}
	f := files[0]
	idx := cfg.ResolveGTFIndexPath(src)
	if idx == f.Path {
		return Result{}, fmt.Errorf("%s: GTF index path %s collides with the source file", label, idx)
	}

	if !force {
		ok, err := locatorExists(ctx, idx)
		if err != nil {
			return Result{}, err
		}
		if _, hasIdx, err2 := anyIndexAt(ctx, idx); err == nil && err2 == nil && ok && hasIdx {
			logf("%s: cached %s", label, objstore.Base(idx))
			return Result{Source: label, Data: "skipped", Index: "reused"}, nil
		}
	}

	st, err := newStaging(f.Path)
	if err != nil {
		return Result{}, err
	}
	defer st.close()

	sum, err := resolveChecksum(ctx, f.Checksum, objstore.Base(f.Path))
	if err != nil {
		return Result{}, err
	}
	logf("%s: downloading %s", label, objstore.Base(f.Path))
	if err := download(ctx, f.URL, st.work, sum); err != nil {
		return Result{}, err
	}

	// Convert beside the original, then publish under the index locator.
	built := filepath.Join(st.dir, objstore.Base(idx))
	if built == st.work {
		return Result{}, fmt.Errorf("%s: converted GTF would overwrite the original", label)
	}
	logf("%s: bgzip + tabix indexing", label)
	if err := buildGTFIndex(st.work, built); err != nil {
		return Result{}, fmt.Errorf("index GTF %s: %w", objstore.Base(f.Path), err)
	}

	conv := &staging{dir: st.dir, work: built}
	if err := conv.publish(ctx, idx, "", ".tbi", ".csi"); err != nil {
		return Result{}, err
	}
	res := Result{Source: label, Data: "downloaded", Index: "built"}

	if keepRaw {
		if err := st.publish(ctx, f.Path, f.Checksum); err != nil {
			return Result{}, err
		}
	} else {
		// Nothing to reclaim remotely — the original was never uploaded — but
		// report the bytes not spent, so the output matches the local case.
		if fi, err := os.Stat(st.work); err == nil {
			res.Freed = fi.Size()
		}
		// A previous --keep-raw run may have left one in the bucket.
		if err := locatorRemove(ctx, f.Path); err != nil {
			logf("%s: %v", label, err)
		}
	}
	logf("%s: uploaded %s", label, objstore.Base(idx))
	if err := recordLocations(ctx, cfg, src); err != nil {
		return Result{}, fmt.Errorf("%s: recording locations: %w", label, err)
	}
	return res, nil
}

// pruneRawGTFRemote is PruneRawGTF for an object-store cache. It applies the
// same guard: never remove the original unless a usable converted copy is
// actually in place, or the prune deletes the only data there is.
func pruneRawGTFRemote(ctx context.Context, raw, idx string) (int64, error) {
	if _, hasRawIdx, err := anyIndexAt(ctx, raw); err != nil {
		return 0, err
	} else if hasRawIdx {
		return 0, nil // the original is what queries read
	}
	ok, err := locatorExists(ctx, idx)
	if err != nil || !ok {
		return 0, err
	}
	if _, hasIdx, err := anyIndexAt(ctx, idx); err != nil || !hasIdx {
		return 0, err
	}
	size, present, err := locatorSize(ctx, raw)
	if err != nil || !present {
		return 0, err
	}
	if err := locatorRemove(ctx, raw); err != nil {
		return 0, fmt.Errorf("prune %s: %w", raw, err)
	}
	return size, nil
}

// missingRemote is Missing for an object-store cache.
//
// It mirrors the local rules rather than restating them differently: a GTF is
// satisfied by the converted copy alone, a BBI needs no sidecar, and everything
// else needs data plus an index. A locator that cannot be reached is reported
// as missing with the reason attached, because "run `varhub download`" is the
// wrong advice when the real problem is a bad endpoint or expired credentials.
// resolvesToObject reports whether a source's files live in an object store,
// whatever the cache directory happens to be.
func resolvesToObject(cfg *config.Config, src config.Source) bool {
	for _, f := range cfg.ResolveSourceFiles(src) {
		if objstore.IsObject(f.Path) {
			return true
		}
	}
	return false
}

func missingRemote(ctx context.Context, cfg *config.Config, src config.Source) []string {
	var missing []string
	note := func(loc string, err error) { missing = append(missing, fmt.Sprintf("%s (%v)", loc, err)) }

	for _, f := range cfg.ResolveSourceFiles(src) {
		if src.IsGTFSource() {
			idx := cfg.ResolveGTFIndexPath(src)
			ok, err := locatorExists(ctx, idx)
			if err != nil {
				note(idx, err)
				continue
			}
			if ok {
				if _, hasIdx, err := anyIndexAt(ctx, idx); err != nil {
					note(idx, err)
					continue
				} else if hasIdx {
					continue
				}
			}
			// Fall back to a pre-indexed original, the same as the local rule.
			if _, hasRawIdx, err := anyIndexAt(ctx, f.Path); err == nil && hasRawIdx {
				if ok, err := locatorExists(ctx, f.Path); err == nil && ok {
					continue
				}
			}
			missing = append(missing, idx+" (GTF index)")
			continue
		}

		ok, err := locatorExists(ctx, f.Path)
		if err != nil {
			note(f.Path, err)
			continue
		}
		if !ok {
			missing = append(missing, f.Path)
			continue
		}
		if src.IsBBISource() {
			continue
		}
		_, hasIdx, err := anyIndexAt(ctx, f.Path)
		if err != nil {
			note(f.Path, err)
			continue
		}
		if !hasIdx {
			missing = append(missing, f.Path+" (index)")
		}
	}
	return missing
}

// ensureIndexedGTFRemote resolves the queryable GTF locator in an object store.
//
// Unlike the local case this never builds anything: converting a GTF means
// streaming it through bgzip and tabix, which needs a local staging copy, so it
// belongs to `varhub download` (see fetchGTFRemote) and not to a lazy path taken
// during annotation. Here we only report which locator is usable.
func ensureIndexedGTFRemote(ctx context.Context, cfg *config.Config, src config.Source, raw string) (string, string, error) {
	// A .gz original that ships its own index is queried directly, matching the
	// local rule.
	if strings.HasSuffix(raw, ".gz") {
		if ok, err := locatorExists(ctx, raw); err == nil && ok {
			if _, hasIdx, err := anyIndexAt(ctx, raw); err == nil && hasIdx {
				return raw, "pre-indexed", nil
			}
		}
	}
	idx := cfg.ResolveGTFIndexPath(src)
	if idx == raw {
		return "", "", fmt.Errorf("GTF index path %s collides with the source file", idx)
	}
	ok, err := locatorExists(ctx, idx)
	if err != nil {
		return "", "", err
	}
	if ok {
		if _, hasIdx, err := anyIndexAt(ctx, idx); err != nil {
			return "", "", err
		} else if hasIdx {
			return idx, "reused", nil
		}
	}
	return "", "", fmt.Errorf("no indexed GTF at %s (run `varhub download`)", idx)
}

// recordLocations writes the overlay for a source that was just provisioned,
// so a later run reads it from where it actually went rather than from wherever
// the current cache_dir happens to point.
//
// Written per source, beside its manifest: two concurrent `download --source`
// runs cannot collide, and deleting a source removes its record with it.
func recordLocations(ctx context.Context, cfg *config.Config, src config.Source) error {
	if src.IsBuiltinSource() || src.IsGeneList() {
		return nil // nothing is provisioned, so there is nothing to record
	}
	// Record the root, not each file: the layout under it is the convention
	// varhub already applies, so restating it per file would be duplication that
	// can drift. Per-file entries stay available for irregular cases.
	loc := &config.Locations{Root: cfg.CacheDirAbs()}
	anyLocal := false
	for _, f := range cfg.ResolveSourceTargets(src) {
		if f.Local {
			anyLocal = true // used in place; the manifest already says where
		}
	}
	if anyLocal {
		loc.Root = ""
	}
	if src.IsGTFSource() && loc.Root != "" {
		// A GTF is queried through the converted copy, whose name differs from
		// the download, so the root alone does not locate it.
		loc.GTFIndex = cfg.GTFIndexTarget(src)
	}

	saved := cfg.NewLocations(src.Name, src.Version, loc)
	if loc.Root == "" && len(loc.Files) == 0 && loc.GTFIndex == "" {
		// Nothing recorded: drop any stale overlay rather than leaving it to
		// point at files this run did not produce.
		return saved.Delete()
	}
	return saved.Save()
}
