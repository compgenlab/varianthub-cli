// Command varhub is the interactive CLI for variant annotation.
//
// Usage:
//
//	varhub [-home DIR] [-snapshot NAME] <command> [args]
//
// VARHUB_HOME (the -home flag, else $VARHUB_HOME, else the current directory)
// is the base directory holding config.toml; config values may reference it as
// $VARHUB_HOME (e.g. data_dir = "$VARHUB_HOME/data"). Sources and snapshot manifests
// live under annotations_dir (sources/, snapshots/). A source is a data file, a
// type="builtin" bundle, or a type="tool" external annotator.
//
// Config commands:
//
//	init                        scaffold config.toml + a starter snapshot
//	snapshot add <name> [--from] create a snapshot manifest (optionally copy another)
//	snapshot list               list snapshots
//	source add <snapshot> ...   add a source and reference it from a snapshot
//	annotation add <snapshot> .. add an annotation field to a source
//	download [--source N] [-j N] fetch the snapshot's sources (incl. tool images) + index
//	registry ...                pull pre-made snapshot/source configs from a catalog
//
// Annotation commands:
//
//	annotate [--format tab|vcf|json|text] <vcf|locus...>  annotate loci (default: tab)
//	annotate --format vcf -o <out.vcf> <in.vcf>  write a fully-annotated VCF
//	versions                    show the active snapshot
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	annotatepkg "github.com/compgenlab/varianthub-cli/internal/annotate"
	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/engine"
	"github.com/compgenlab/varianthub-cli/internal/model"
	"github.com/compgenlab/varianthub-cli/internal/service"
	"github.com/compgenlab/varianthub-cli/internal/store"
	"github.com/compgenlab/varianthub-cli/internal/store/postgres"
	"github.com/compgenlab/varianthub-cli/internal/store/sqlite"
	"github.com/compgenlab/varianthub-cli/internal/vcf"
)

// version is stamped at build time via -ldflags "-X main.version=…".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// resolveHome returns VARHUB_HOME: the --home flag, else $VARHUB_HOME, else the
// current directory; resolved to an absolute path. It exports the result back to
// the environment so $VARHUB_HOME references inside config.toml resolve.
func resolveHome(flagHome string) string {
	home := flagHome
	if home == "" {
		home = os.Getenv("VARHUB_HOME")
	}
	if home == "" {
		home = "."
	}
	if abs, err := filepath.Abs(home); err == nil {
		home = abs
	}
	os.Setenv("VARHUB_HOME", home)
	return home
}

func run(args []string) error {
	fs := flag.NewFlagSet("varhub", flag.ContinueOnError)
	home := fs.String("home", "", "varhub home dir (default: $VARHUB_HOME or CWD); holds config.toml")
	snapshotName := fs.String("snapshot", "", "snapshot to use (default: config default_snapshot)")
	fs.Usage = usage
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfgPathStr := filepath.Join(resolveHome(*home), "config.toml")
	cfgPath := &cfgPathStr
	rest := fs.Args()
	if len(rest) == 0 {
		usage()
		return fmt.Errorf("no command given")
	}
	cmd, cmdArgs := rest[0], rest[1:]
	ctx := context.Background()

	if cmd == "version" {
		fmt.Println("varhub", version)
		return nil
	}

	// Config-management commands operate on files, not the engine. `annotate`
	// also routes here because its -o (VCF-output) mode uses the streaming
	// pipeline rather than the cache engine.
	switch cmd {
	case "init":
		return cmdInit(*cfgPath, cmdArgs)
	case "snapshot":
		return cmdSnapshot(*cfgPath, cmdArgs)
	case "source":
		return cmdSource(*cfgPath, cmdArgs)
	case "annotation":
		return cmdAnnotation(*cfgPath, cmdArgs)
	case "annotate":
		return cmdAnnotate(ctx, *cfgPath, *snapshotName, cmdArgs)
	case "download":
		return cmdDownload(ctx, *cfgPath, *snapshotName, cmdArgs)
	case "registry":
		return cmdRegistry(ctx, *cfgPath, cmdArgs)
	case "configure", "edit":
		return cmdEdit(*cfgPath, cmdArgs)
	case "bgzip": // hidden: BGZF compress (mimics bgzip) for recipes/tool scripts
		return cmdBgzip(cmdArgs)
	case "faidx": // hidden: index a FASTA (mimics samtools faidx) for recipes/tool scripts
		return cmdFaidx(cmdArgs)
	case "tabix": // hidden: write a tabix index (mimics tabix) for recipes/tool scripts
		return cmdTabix(cmdArgs)
	case "vcf-merge": // combine same-order per-source VCFs (the annotate -t fan-out merge)
		return cmdVcfMerge(cmdArgs)
	}

	// `versions` needs the engine (config + snapshot + store + annotator).
	if cmd == "versions" {
		eng, closeFn, err := buildEngine(ctx, *cfgPath, *snapshotName)
		if err != nil {
			return err
		}
		defer closeFn()
		return cmdVersions(eng)
	}

	usage()
	return fmt.Errorf("unknown command %q", cmd)
}

func usage() {
	fmt.Fprint(os.Stderr, `VariantHub (varhub) — variant annotation CLI

usage: varhub [-home DIR] [-snapshot NAME] <command> [args]

VARHUB_HOME (-home flag, else $VARHUB_HOME, else CWD) is the base dir holding
config.toml; config values may reference it, e.g. data_dir = "$VARHUB_HOME/data".

config commands:
  init                         scaffold config.toml + a starter snapshot
  configure | edit             interactive editor: snapshots, sources, annotations
  snapshot add <name> [--from S]  create a snapshot (optionally copy from S)
  snapshot list                list snapshots
  source add [flags] [--snapshot S]  add a source (prompts when flags omitted)
  annotation add --source R [flags]  add an annotation to a source
  annotation list <snapshot>   list annotations + the default set
  download [--source N] [--force] [-j N]  fetch the snapshot's sources (incl. tool images) + index
  registry list|pull-snapshot <name>|add-source <name[:version]> [--snapshot S]
  registry submit <name[:version]> [--dry-run]  propose a source to the public registry

annotation commands:
  annotate [--all|-a name,...] [--format tab|vcf|json|text] [-o FILE] [-v] <vcf|locus...>
                               annotate loci (default format: tab; -o writes to a file; -v prints progress)
                               --no-cache ignores the cache DB entirely (compute fresh, persist nothing)
                               vcf output: --tool-cache-dir DIR caches/reuses tool output by input
                               --temp-dir DIR puts scratch files there (default: config temp_dir, else $TMPDIR)
  versions                     show the active snapshot
  version                      print the varhub version
`)
}

// buildEngine loads the config + snapshot and builds the annotate engine.
func buildEngine(ctx context.Context, cfgPath, snapshotName string) (*engine.Engine, func(), error) {
	if err := config.MustExist(cfgPath); err != nil {
		return nil, nil, err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	snap, err := cfg.LoadSnapshot(snapshotName)
	if err != nil {
		return nil, nil, err
	}
	return buildEngineWith(ctx, cfg, snap)
}

// buildEngineWith builds the annotate engine over the store and the overlay (tabix
// source) annotator, for an already-loaded config + snapshot.
func buildEngineWith(ctx context.Context, cfg *config.Config, snap *config.Snapshot) (*engine.Engine, func(), error) {
	st, err := openStore(cfg)
	if err != nil {
		return nil, nil, err
	}
	eng, err := service.NewEngineOverStore(ctx, cfg, snap, st, nil)
	if err != nil {
		if st != nil {
			st.Close()
		}
		return nil, nil, err
	}
	return eng, func() {
		if st != nil {
			st.Close()
		}
	}, nil
}

// openStore opens the configured annotation-cache backend.
// openStore opens the configured cache store, or returns (nil, nil) when the cache is
// disabled (no [database]) — callers treat a nil store as "compute, don't persist".
func openStore(cfg *config.Config) (store.Store, error) {
	if !cfg.CacheEnabled() {
		return nil, nil
	}
	switch cfg.Database.Backend {
	case "sqlite":
		return sqlite.Open(cfg.DatabasePathAbs())
	case "postgres":
		// Shared between workers, which is the point: a SQLite file caches for
		// one machine, and a deployment running several gets no benefit from a
		// variant another worker already computed.
		return postgres.Open(context.Background(), cfg.Database.Path)
	default:
		return nil, fmt.Errorf("unsupported backend %q", cfg.Database.Backend)
	}
}

// cmdAnnotate runs either the VCF→annotated-VCF pipeline (with -o) or the
// cache/loci annotate path (printing values). Which annotations are applied is
// selected by --all / -a (else the snapshot's default-marked annotations).
func cmdAnnotate(ctx context.Context, cfgPath, snapshot string, args []string) error {
	fs := flag.NewFlagSet("annotate", flag.ContinueOnError)
	out := fs.String("o", "", "write output to this file (default: stdout)")
	format := fs.String("format", "tab", "output format: tab|vcf|json|text")
	all := fs.Bool("all", false, "apply all annotations (else the default-marked set)")
	threads := fs.Int("threads", 1, "vcf output: annotate this many sources in parallel (0 = all CPUs); each runs a full pass to a temp file, then merges")
	fs.IntVar(threads, "t", 1, "shorthand for --threads")
	keepTemp := fs.Bool("keep-temp", false, "vcf output: keep the per-source temp part files (for debugging the fan-out)")
	toolCacheDir := fs.String("tool-cache-dir", "", "vcf output: directory used as a per-input tool-output cache — reuse a saved output matching this input+tool+assembly (skip the tool), else run it and save the output there")
	tempDir := fs.String("temp-dir", "", "base directory for scratch files (tool workdirs, fan-out parts); default: config temp_dir, else $TMPDIR or /tmp")
	noCache := fs.Bool("no-cache", false, "ignore the annotation cache DB entirely: compute every locus fresh and persist nothing")
	verbose := fs.Bool("verbose", false, "print progress to stderr (phases, tool cache hits, variant counts)")
	fs.BoolVar(verbose, "v", false, "shorthand for --verbose")
	var keys stringList
	fs.Var(&keys, "annotation", "annotation name to apply (repeatable, comma-separated)")
	fs.Var(&keys, "a", "shorthand for --annotation")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *threads <= 0 {
		*threads = runtime.NumCPU()
	}
	rest := fs.Args()
	if *all && len(keys) > 0 {
		return fmt.Errorf("use --all or -a, not both")
	}

	// Under -v, carry a nil-safe progress logger through the pipeline via ctx.
	if *verbose {
		ctx = annotatepkg.WithLogger(ctx, annotatepkg.NewLogger(os.Stderr))
	}
	lg := annotatepkg.LoggerFrom(ctx)

	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	// Scratch dirs (tool workdirs, fan-out parts, synthesized input) go under this
	// base; the --temp-dir flag overrides the config's temp_dir. Applies to any
	// subprocess too (e.g. VEP inherits TMPDIR).
	if err := applyTempDir(firstNonEmpty(*tempDir, cfg.TempDir)); err != nil {
		return err
	}
	snap, err := cfg.LoadSnapshot(snapshot)
	if err != nil {
		return err
	}
	selected, err := snap.SelectAnnotations(keys, *all)
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		return fmt.Errorf("no annotations selected — mark some `default = true`, or pass --all / -a key[,key...]")
	}

	// vcf output uses the streaming pipeline (preserves samples for a VCF input;
	// synthesizes a sites-only VCF for bare loci). Other formats use the engine.
	if *format == "vcf" {
		sub := *snap
		sub.Annotations = selected
		sub.Sources = withSelectedTools(snap, selected)
		if err := service.RequireSources(cfg, snap, selected); err != nil {
			return err
		}
		// Bulk VCF: tools run over the whole input and annotate from their indexed
		// output directly — the per-locus tool cache is skipped, so no store is needed.
		inPath, cleanup, err := annotateInputVCF(rest)
		if err != nil {
			return err
		}
		defer cleanup()
		lg.Logf("annotating VCF %s → %s", inPath, vcfOutName(*out))
		return annotatepkg.AnnotateVCFSnapshot(ctx, cfg, &sub, inPath, *out, *threads, *keepTemp, *toolCacheDir)
	}

	if *format != "tab" && *format != "json" && *format != "text" {
		return fmt.Errorf("unknown --format %q (want tab|vcf|json|text)", *format)
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: annotate [--format tab|vcf|json|text] <vcf|locus...>  (locus = chrom:pos:ref:alt)")
	}
	// A single VCF-file input is a bulk request: referenced tools run over the whole
	// input directly (cache skipped), matching the --format vcf path. Bare loci are
	// individual queries and keep the per-locus tool cache.
	inputIsVCF := len(rest) == 1 && fileExists(rest[0])
	loci, err := readLoci(rest)
	if err != nil {
		return err
	}
	lg.Logf("annotating %s loci (%d annotation(s) selected)", annotatepkg.Count(len(loci)), len(selected))

	// --no-cache bypasses the cache DB: a nil store makes the engine (and the
	// per-locus tool cache) compute every locus fresh and persist nothing.
	var st store.Store
	if !*noCache {
		st, err = openStore(cfg)
		if err != nil {
			return err
		}
	}
	defer func() {
		if st != nil {
			st.Close()
		}
	}()

	// Annotate over the shared locus path (verifies sources, runs any referenced
	// tool sources, builds the engine, and annotates) — the same path the server uses.
	// --no-cache leaves st nil, so referenced tools must run directly over all loci
	// (the skip-tool-cache path) rather than through the per-locus cache.
	res, err := service.AnnotateLoci(ctx, cfg, snap, st, selected, loci, inputIsVCF || *noCache)
	if err != nil {
		return err
	}
	// Says what happened rather than a fixed sentence. "rest from cache" was
	// printed unconditionally, including when no cache existed to draw on — an
	// installation whose config declares no [database] has a nil store, and the
	// line still credited a cache for every locus it did not call novel. Reading
	// it as evidence of caching is the obvious mistake, and it was the wrong
	// trail to follow.
	switch {
	case st == nil:
		lg.Logf("done: %s loci annotated (%s computed; no cache configured)",
			annotatepkg.Count(len(loci)), annotatepkg.Count(res.Novel))
	default:
		lg.Logf("done: %s loci annotated (%s newly computed, %s from cache)",
			annotatepkg.Count(len(loci)), annotatepkg.Count(res.Novel),
			annotatepkg.Count(len(loci)-res.Novel))
	}

	w := io.Writer(os.Stdout)
	if *out != "" && *out != "-" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	return formatResults(w, *format, loci, selected, res)
}

// withSelectedTools returns the snapshot's data/builtin sources plus only the tool
// sources referenced by the selected annotations (so unused tools aren't run).
func withSelectedTools(snap *config.Snapshot, selected []config.Annotation) []config.Source {
	var out []config.Source
	for _, s := range snap.Sources {
		if !s.IsTool() {
			out = append(out, s)
		}
	}
	return append(out, service.ReferencedTools(snap, selected)...)
}

// readLoci parses loci from CLI args: a single existing file is read as a VCF,
// otherwise each arg is a chrom:pos:ref:alt locus.
func readLoci(rest []string) ([]model.Locus, error) {
	if len(rest) == 1 && fileExists(rest[0]) {
		return vcf.ReadFile(rest[0])
	}
	var loci []model.Locus
	for _, a := range rest {
		l, err := parseLocus(a)
		if err != nil {
			return nil, err
		}
		loci = append(loci, l)
	}
	return loci, nil
}

// annotateInputVCF returns a VCF path to stream through the pipeline: the input VCF
// as-is (samples preserved) when a single VCF file is given, else a temp sites-only
// VCF synthesized from the locus args. The cleanup removes any temp file.
func annotateInputVCF(rest []string) (string, func(), error) {
	noop := func() {}
	if len(rest) == 0 {
		return "", noop, fmt.Errorf("usage: annotate --format vcf <vcf|locus...>")
	}
	if len(rest) == 1 && fileExists(rest[0]) {
		return rest[0], noop, nil
	}
	loci, err := readLoci(rest)
	if err != nil {
		return "", noop, err
	}
	tmp, err := os.CreateTemp("", "varhub-in-*.vcf")
	if err != nil {
		return "", noop, err
	}
	tmp.Close()
	if err := vcf.WriteLoci(tmp.Name(), loci); err != nil {
		os.Remove(tmp.Name())
		return "", noop, err
	}
	return tmp.Name(), func() { os.Remove(tmp.Name()) }, nil
}

// formatResults renders engine results in the chosen format: tab (default; a
// #-commented header + chrom/pos/ref/alt then a column per selected annotation),
// json (per-variant objects), or text (a human report).
func formatResults(w io.Writer, format string, loci []model.Locus, selected []config.Annotation, res engine.AnnotateResult) error {
	names := make([]string, 0, len(selected))
	for _, a := range selected {
		if a.Name != "" { // skip any unnamed annotation (no column to render)
			names = append(names, a.Name)
		}
	}
	valOf := func(l model.Locus, name string) (model.AnnRow, bool) {
		for _, r := range res.ByLocus[l.Key()] {
			if r.Key == name {
				return r, true
			}
		}
		return model.AnnRow{}, false
	}
	switch format {
	case "tab":
		bw := bufio.NewWriter(w)
		fmt.Fprintln(bw, "#"+strings.Join(append([]string{"chrom", "pos", "ref", "alt"}, names...), "\t"))
		for _, l := range loci {
			cols := []string{l.Chrom, strconv.FormatInt(l.Pos, 10), l.Ref, l.Alt}
			for _, n := range names {
				r, ok := valOf(l, n)
				if ok {
					cols = append(cols, r.Value.String())
				} else {
					cols = append(cols, "")
				}
			}
			fmt.Fprintln(bw, strings.Join(cols, "\t"))
		}
		return bw.Flush()
	case "json":
		// Shared schema with the REST server (engine.BuildVariants): an array of
		// per-variant objects, every selected annotation a key (null when absent).
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(engine.BuildVariants(loci, names, res))
	case "text":
		bw := bufio.NewWriter(w)
		for _, l := range loci {
			fmt.Fprintf(bw, "%s\n", l.Key())
			for _, n := range names {
				if r, ok := valOf(l, n); ok {
					fmt.Fprintf(bw, "  %-24s %-24s = %s\n", r.DataSource, r.Key, r.Value.String())
				}
			}
		}
		fmt.Fprintf(bw, "\nsnapshot %s  (%d newly annotated)\n", res.Version, res.Novel)
		return bw.Flush()
	}
	return fmt.Errorf("unknown format %q", format)
}

// stringList is a repeatable, comma-splittable flag.Value.
type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*s = append(*s, p)
		}
	}
	return nil
}

func cmdVersions(eng *engine.Engine) error {
	fmt.Printf("snapshot: %s\n", eng.Version())
	return nil
}

func parseLocus(s string) (model.Locus, error) { return vcf.ParseLocus(s) }

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// applyTempDir points the process's temp base at dir (creating it if needed) by
// setting TMPDIR, so every os.MkdirTemp("", …) scratch dir — and any subprocess
// that honors TMPDIR — lands there instead of the default /tmp. An empty dir leaves
// the default in place.
func applyTempDir(dir string) error {
	if dir == "" {
		return nil
	}
	abs, err := filepath.Abs(os.ExpandEnv(dir))
	if err != nil {
		abs = os.ExpandEnv(dir)
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return fmt.Errorf("temp dir %s: %w", abs, err)
	}
	return os.Setenv("TMPDIR", abs)
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// vcfOutName renders a VCF output destination for -v logs (stdout shown as "(stdout)").
func vcfOutName(out string) string {
	if out == "" || out == "-" {
		return "(stdout)"
	}
	return out
}
