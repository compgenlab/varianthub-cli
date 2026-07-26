package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/compgenlab/varianthub-cli/internal/checksum"
	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/fetch"
	"github.com/compgenlab/varianthub-cli/internal/prompt"
)

// --- top-level source helpers (v2 layout) ---------------------------------

// findLocalItem resolves a "name[:version]" ref to a top-level source file under
// annotations_dir (a source may be data, builtin, or type="tool"). Returns the file
// path, the decoded fragment, and ok=false when it doesn't exist.
func findLocalItem(cfg *config.Config, ref string) (path string, frag *config.Snapshot, ok bool, err error) {
	if n, v, e := cfg.ResolveSourceRef(ref); e == nil {
		p := cfg.SourceFile(n, v)
		f, err := config.ReadFragment(p)
		if err != nil {
			return "", nil, false, err
		}
		return p, f, true, nil
	}
	return "", nil, false, nil
}

// --- init -----------------------------------------------------------------

// cmdInit scaffolds a global config.toml plus a starter annotations tree
// (sources/, a snapshots/<name>.toml manifest).
func cmdInit(cfgPath string, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	force := fs.Bool("force", false, "overwrite an existing config.toml")
	snapName := fs.String("snapshot", "", "name of the starter snapshot (default: current YYYY-MM)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if _, err := os.Stat(cfgPath); err == nil && !*force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", cfgPath)
	}

	pr := prompt.New()
	annDir := pr.Ask("annotations directory (holds sources/, snapshots/)", "./annotations")
	// Snapshot name defaults to the current year-month; --snapshot overrides the prompt.
	snap := *snapName
	if snap == "" {
		snap = pr.Ask("starter snapshot name", time.Now().Format("2006-01"))
	}

	cfg := config.Config{
		DataDir:         "$VARHUB_HOME/data",
		CacheDir:        "$VARHUB_HOME/data/cache",
		DefaultSnapshot: snap,
		AnnotationsDir:  annDir,
		Database:        config.Database{Backend: "sqlite", Path: "$VARHUB_HOME/varhub.db"},
		// Reference FASTAs are keyed by assembly; the starter snapshot is GRCh38.
		References: map[string]config.Reference{"GRCh38": {Fasta: "$VARHUB_HOME/ref/GRCh38.fa"}},
	}
	if err := config.WriteTOML(cfgPath, cfg); err != nil {
		return fmt.Errorf("write %s: %w", cfgPath, err)
	}
	loaded, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	// Starter source: just the self-contained builtins bundle (no data to download).
	// Real sources/tools are added later via `varhub configure` or `varhub registry`.
	builtins := &config.Snapshot{Sources: []config.Source{{
		Name: "builtins", Version: "1", Type: "builtin",
		Annotations: []config.Annotation{
			{Builtin: "auto_id", Name: "auto_id"},
			{Builtin: "indel", Name: "indel"},
			{Builtin: "tstv", Name: "tstv"},
		},
	}}}
	if err := config.WriteFragment(loaded.SourceFile("builtins", "1"), builtins); err != nil {
		return err
	}
	// A snapshot manifest referencing the builtins. Defaults are left unset — add
	// real sources and choose their default annotations via `varhub configure`.
	manifest := &config.SnapshotConfig{
		Description: "starter snapshot", Assembly: "GRCh38",
		Sources: []string{"builtins:1"},
	}
	if err := config.WriteSnapshotConfig(loaded.SnapshotFile(snap), manifest); err != nil {
		return err
	}
	fmt.Printf("wrote %s\nwrote %s/{sources,snapshots}/… (starter snapshot %q)\n",
		cfgPath, annDir, snap)

	// Offer to jump straight into the editor to add/download sources and tools.
	// Only when stdin is a real terminal — the editor is a full-screen TUI, so a
	// piped/non-interactive `varhub init` must not try to launch it.
	if stdinIsTerminal() && pr.AskBool("configure annotation sources & tools now?", true) {
		return cmdEdit(cfgPath, nil)
	}
	fmt.Println("run `varhub configure` to add sources/tools, then `varhub download` to fetch them")
	return nil
}

// stdinIsTerminal reports whether stdin is an interactive terminal (a character
// device), so init only launches the TUI for a real user, not a pipe/script.
func stdinIsTerminal() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// --- snapshot list/add ----------------------------------------------------

// cmdSnapshot handles `snapshot list` and `snapshot add`.
func cmdSnapshot(cfgPath string, args []string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: snapshot <list|add>")
	}
	switch args[0] {
	case "list":
		names, err := cfg.ListSnapshots()
		if err != nil {
			return err
		}
		if len(names) == 0 {
			fmt.Println("(no snapshots)")
			return nil
		}
		for _, n := range names {
			title := n
			if sc, err := config.ReadSnapshotConfig(cfg.SnapshotFile(n)); err == nil && sc.Title != "" {
				title = sc.Title
			}
			marker := ""
			if n == cfg.DefaultSnapshot {
				marker = "  (default)"
			}
			fmt.Printf("%-16s %s%s\n", n, title, marker)
		}
		return nil
	case "add":
		fs := flag.NewFlagSet("snapshot add", flag.ContinueOnError)
		from := fs.String("from", "", "copy the manifest of this snapshot as a starting point")
		var srcs, defs stringList
		fs.Var(&srcs, "source", "source ref name:version to include, incl. tool sources (repeatable/comma-separated)")
		fs.Var(&defs, "default", "annotation name applied by default (repeatable/comma-separated)")
		assembly := fs.String("assembly", "", "genome assembly (its reference FASTA comes from config [references.<assembly>])")
		title := fs.String("title", "", "human-readable name (e.g. \"July 2026 Clinical Panel\")")
		desc := fs.String("desc", "", "description")
		if len(args) < 2 {
			return fmt.Errorf("usage: snapshot add <name> [--title T --source a:1 --source vep:113 --default x --assembly A]")
		}
		name := args[1]
		if err := fs.Parse(args[2:]); err != nil {
			return err
		}
		file := cfg.SnapshotFile(name)
		if _, err := os.Stat(file); err == nil {
			return fmt.Errorf("snapshot %q already exists (%s)", name, file)
		}
		sc := &config.SnapshotConfig{
			Title: *title, Description: *desc, Assembly: *assembly,
			Sources: srcs, Defaults: defs,
		}
		if *from != "" {
			base, err := config.ReadSnapshotConfig(cfg.SnapshotFile(*from))
			if err != nil {
				return fmt.Errorf("copy from %q: %w", *from, err)
			}
			sc = base
			if *title != "" {
				sc.Title = *title
			}
			sc.Description = *desc
		}
		if err := config.WriteSnapshotConfig(file, sc); err != nil {
			return err
		}
		fmt.Printf("created snapshot %q (%s)\n", name, file)
		return nil
	default:
		return fmt.Errorf("unknown snapshot subcommand %q", args[0])
	}
}

// --- source add -----------------------------------------------------------

// cmdSource handles `source add ...`, writing a top-level source file
// (sources/<name>/<version>/<name>-<version>.toml) and optionally adding it to a
// snapshot manifest via --snapshot.
func cmdSource(cfgPath string, args []string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	if len(args) < 1 || args[0] != "add" {
		return fmt.Errorf("usage: source add --name --version [--url|--localpath --format ... --snapshot S]")
	}
	fs := flag.NewFlagSet("source add", flag.ContinueOnError)
	var s config.Source
	snap := fs.String("snapshot", "", "also add this source to the named snapshot manifest")
	fs.StringVar(&s.Name, "name", "", "source name (slug)")
	fs.StringVar(&s.Version, "version", "", "source data version")
	fs.StringVar(&s.Title, "title", "", "human-readable name (e.g. \"ClinVar variant significance\")")
	fs.StringVar(&s.Description, "desc", "", "one-line description")
	fs.StringVar(&s.Assembly, "assembly", "", "genome assembly (default: the target snapshot's assembly)")
	fs.StringVar(&s.URL, "url", "", "canonical download URL")
	fs.StringVar(&s.URLIndex, "url-index", "", "URL of a prebuilt .tbi/.csi index (else guessed/built)")
	fs.StringVar(&s.LocalPath, "localpath", "", "exact local data file (used as-is, never downloaded)")
	fs.StringVar(&s.LocalPathIndex, "localpath-index", "", "exact local index file (else alongside the data)")
	fs.StringVar(&s.Format, "format", "vcf", "vcf|bed|tab|gtf")
	fs.StringVar(&s.Checksum, "checksum", "", "data checksum, <algo>:<hex-or-url> (optional)")
	fs.StringVar(&s.ChecksumIndex, "checksum-index", "", "index checksum, <algo>:<hex-or-url> (optional)")
	fs.IntVar(&s.RefCol, "ref-col", 0, "tab: 1-based REF column")
	fs.IntVar(&s.AltCol, "alt-col", 0, "tab: 1-based ALT column")
	var chroms stringList
	fs.Var(&chroms, "chroms", "chromosomes for a {chrom}-templated url (multi-file, repeatable/comma-separated)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	s.Chroms = chroms

	// Auto-tag the new source with the target snapshot's assembly (the snapshot is
	// the source of truth for assembly). Explicit --assembly wins; no snapshot leaves
	// it blank.
	if s.Assembly == "" && *snap != "" {
		if sc, err := config.ReadSnapshotConfig(cfg.SnapshotFile(*snap)); err == nil {
			s.Assembly = sc.Assembly
		}
	}

	if s.Name == "" || s.Version == "" {
		pr := prompt.New()
		if s.Name == "" {
			s.Name = pr.AskRequired("source name")
		}
		if s.Version == "" {
			s.Version = pr.AskRequired("version")
		}
		s.Format = pr.AskChoice("format", []string{"vcf", "bed", "tab", "gtf"}, s.Format)
		if s.URL == "" && s.LocalPath == "" {
			s.URL = pr.Ask("canonical download URL (blank if local)", "")
			s.LocalPath = pr.Ask("local data file (blank to download to cache)", "")
		}
		if s.Format == "tab" {
			s.RefCol = pr.AskInt("REF column (1-based, 0 = position-only)", s.RefCol)
			s.AltCol = pr.AskInt("ALT column (1-based, 0 = position-only)", s.AltCol)
		}
	}
	if s.Format != "tab" {
		s.RefCol, s.AltCol = 0, 0
	}
	if s.URL == "" && s.LocalPath == "" {
		return fmt.Errorf("source needs a --url or --localpath")
	}
	if err := checksum.ValidateSpec(s.Checksum); err != nil {
		return fmt.Errorf("checksum: %w", err)
	}
	if err := checksum.ValidateSpec(s.ChecksumIndex); err != nil {
		return fmt.Errorf("checksum-index: %w", err)
	}

	file := cfg.SourceFile(s.Name, s.Version)
	if _, err := os.Stat(file); err == nil {
		return fmt.Errorf("source %s already exists (%s)", s.ID(), file)
	}
	if err := config.WriteFragment(file, &config.Snapshot{Sources: []config.Source{s}}); err != nil {
		return err
	}
	fmt.Printf("added source %s → %s\n", s.ID(), file)
	if *snap != "" {
		if err := addRefToSnapshot(cfg, *snap, s.ID()); err != nil {
			return err
		}
		fmt.Printf("added %s to snapshot %q\n", s.ID(), *snap)
	}
	return nil
}

// addRefToSnapshot appends a "name:version" source ref (data, builtin, or tool) to a
// snapshot manifest's sources list.
func addRefToSnapshot(cfg *config.Config, snapName, ref string) error {
	file := cfg.SnapshotFile(snapName)
	sc, err := config.ReadSnapshotConfig(file)
	if err != nil {
		return fmt.Errorf("snapshot %q: %w", snapName, err)
	}
	for _, r := range sc.Sources {
		if r == ref {
			return nil // already present
		}
	}
	sc.Sources = append(sc.Sources, ref)
	return config.WriteSnapshotConfig(file, sc)
}

// --- annotation add/list --------------------------------------------------

// cmdAnnotation handles `annotation add ...` and `annotation list <snapshot>`.
func cmdAnnotation(cfgPath string, args []string) error {
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	if len(args) < 1 {
		return fmt.Errorf("usage: annotation <add|list> ...")
	}
	switch args[0] {
	case "add":
		return cmdAnnotationAdd(cfg, args[1:])
	case "list":
		if len(args) < 2 {
			return fmt.Errorf("usage: annotation list <snapshot>")
		}
		return cmdAnnotationList(cfg, args[1])
	default:
		return fmt.Errorf("unknown annotation subcommand %q (want add|list)", args[0])
	}
}

// cmdAnnotationAdd adds an annotation to a top-level source/tool file (selected by
// --source name[:version], or a builtin name). Defaults are set on the snapshot
// manifest, not here.
func cmdAnnotationAdd(cfg *config.Config, args []string) error {
	fs := flag.NewFlagSet("annotation add", flag.ContinueOnError)
	var a config.Annotation
	var source, values string
	fs.StringVar(&source, "source", "", "source/tool ref name[:version], or a builtin (tstv, auto_id, …)")
	fs.StringVar(&a.Name, "name", "", "annotation name (the new INFO tag added)")
	fs.StringVar(&a.Field, "field", "", "source INFO id / BED column / GTF field (defaults to name)")
	fs.StringVar(&a.Type, "type", "categorical", "categorical|text|numeric|flag")
	fs.StringVar(&a.Match, "match", "", "vcf: exact (default) | position")
	fs.StringVar(&a.Args, "args", "", "builtin args (tags: KEY:VALUE; copy_logratio: SOMATIC:GERMLINE)")
	fs.StringVar(&a.Description, "desc", "", "description")
	fs.StringVar(&values, "values", "", "comma-separated enum values")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if source == "" {
		return fmt.Errorf("--source is required (a source/tool ref, or a builtin name)")
	}
	if values != "" {
		a.Values = strings.Split(values, ",")
	}

	// Builtin annotation → nest under the "builtins" source.
	if config.IsBuiltin(source) {
		a.Builtin = source
		a.Name, a.Field, a.Type, a.Match = "", "", "", ""
		if (source == "tags" || source == "copy_logratio") && a.Args == "" {
			return fmt.Errorf("builtin %q needs --args", source)
		}
		n, v, err := cfg.ResolveSourceRef("builtins")
		if err != nil {
			return fmt.Errorf("no builtins source found (add one first): %w", err)
		}
		path := cfg.SourceFile(n, v)
		frag, err := config.ReadFragment(path)
		if err != nil {
			return err
		}
		if bs := builtinSourceOf(frag); bs != nil {
			bs.Annotations = append(bs.Annotations, a)
		} else {
			return fmt.Errorf("%s is not a type=\"builtin\" source", path)
		}
		if err := config.WriteFragment(path, frag); err != nil {
			return err
		}
		fmt.Printf("added builtin %q → %s\n", source, path)
		return nil
	}

	if a.Name == "" {
		return fmt.Errorf("--name is required")
	}
	if !config.ValidAnnotationType(a.Type) {
		return fmt.Errorf("unknown type %q (want %s)", a.Type, strings.Join(config.AnnotationTypes, "|"))
	}
	path, frag, ok, err := findLocalItem(cfg, source)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("no source %q found (add it first)", source)
	}
	name, _, _ := strings.Cut(source, ":")
	if s := frag.SourceByName(name); s != nil { // data or tool source
		s.Annotations = append(s.Annotations, a)
	} else if len(frag.Sources) == 1 {
		frag.Sources[0].Annotations = append(frag.Sources[0].Annotations, a)
	} else {
		return fmt.Errorf("source %q not found in %s", name, path)
	}
	if err := config.WriteFragment(path, frag); err != nil {
		return err
	}
	fmt.Printf("added annotation %q → %s\n", a.Name, path)
	return nil
}

// builtinSourceOf returns the (first) type="builtin" source in a fragment.
func builtinSourceOf(frag *config.Snapshot) *config.Source {
	for i := range frag.Sources {
		if frag.Sources[i].IsBuiltinSource() {
			return &frag.Sources[i]
		}
	}
	return nil
}

// cmdAnnotationList lists a snapshot's annotations (marking the default set).
func cmdAnnotationList(cfg *config.Config, snapName string) error {
	snap, err := cfg.LoadSnapshot(snapName)
	if err != nil {
		return err
	}
	for _, a := range snap.Annotations {
		marker := " "
		if a.Default {
			marker = "*"
		}
		name := a.Name
		if name == "" {
			name = "(" + a.Source + ")"
		}
		fmt.Printf("%s %s\n", marker, name)
	}
	def, _ := snap.SelectAnnotations(nil, false)
	keys := make([]string, 0, len(def))
	for _, a := range def {
		if a.Name != "" {
			keys = append(keys, a.Name)
		}
	}
	fmt.Printf("\n* = default. `annotate` with no flag applies: %s\n", defaultSummary(keys))
	return nil
}

func defaultSummary(keys []string) string {
	if len(keys) == 0 {
		return "(none — pass --all or -a key[,key...])"
	}
	return strings.Join(keys, ", ")
}

// --- download -------------------------------------------------------------

// cmdDownload downloads a snapshot's sources and ensures each is tabix-indexed.
func cmdDownload(ctx context.Context, cfgPath, snapshot string, args []string) error {
	fs := flag.NewFlagSet("download", flag.ContinueOnError)
	source := fs.String("source", "", "only this source (name or name:version), incl. tool sources")
	force := fs.Bool("force", false, "re-download and re-index")
	jobs := fs.Int("jobs", 1, "number of files to download at once")
	fs.IntVar(jobs, "j", 1, "number of files to download at once (shorthand)")
	quiet := fs.Bool("quiet", false, "suppress per-step progress logs")
	keepTemp := fs.Bool("keep-temp", false, "keep per-source scratch dirs (build/tool setup) for debugging")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *quiet {
		fetch.SetLogWriter(io.Discard)
	}
	fetch.SetKeepTemp(*keepTemp)
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return err
	}
	if err := applyTempDir(cfg.TempDir); err != nil {
		return err
	}
	snap, err := cfg.LoadSnapshot(snapshot)
	if err != nil {
		return err
	}

	// Non-commercial sources: inform the user (nothing is blocked) so they don't
	// unknowingly obtain data whose terms restrict commercial use.
	for _, s := range snap.Sources {
		if s.NonCommercial {
			fmt.Fprintf(os.Stderr, "note: %s has commercial-use restrictions (non-commercial)\n", s.ID())
		}
	}

	// fetch.Snapshot handles data sources (download/build) + tool sources (image
	// acquire + one-time setup) in one pass.
	results, err := fetch.Snapshot(ctx, cfg, snap, *source, *force, *jobs)
	if err != nil {
		return err
	}
	for _, r := range results {
		fmt.Println(r.Describe())
	}
	fmt.Printf("downloaded %d item(s) for snapshot %s (cache: %s)\n", len(results), snap.Name, cfg.CacheDirAbs())
	return nil
}

func loadConfig(cfgPath string) (*config.Config, error) {
	if err := config.MustExist(cfgPath); err != nil {
		return nil, err
	}
	return config.Load(cfgPath)
}
