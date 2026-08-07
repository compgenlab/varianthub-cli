// Package config loads the global config.toml plus per-snapshot directories
// (snapshots/<name>/), each holding one TOML file per source/tool. A snapshot is
// a named, version-pinned bundle of sources, tools, and annotation schema. The
// snapshot name is the version stamped on results.
package config

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/compgenlab/varianthub-cli/internal/objstore"

	"github.com/compgenlab/varianthub-cli/internal/checksum"
	"github.com/compgenlab/varianthub-cli/internal/model"
)

// decodeFragment decodes one snapshot fragment file with debug-friendly errors:
// a TOML syntax error is rendered with its line/column + a snippet (via
// toml.ParseError.ErrorWithUsage), and unrecognized keys (typos, wrong nesting)
// are reported instead of being silently ignored.
func decodeFragment(path string) (*Snapshot, error) {
	var snap Snapshot
	md, err := toml.DecodeFile(path, &snap)
	if err != nil {
		return nil, fmt.Errorf("parse %s:\n%w", path, enrichTOML(err))
	}
	if un := md.Undecoded(); len(un) > 0 {
		return nil, fmt.Errorf("%s: unrecognized key(s): %s\n"+
			"  (check spelling and nesting — e.g. annotations go under [[sources.annotations]], "+
			"not a top-level [[annotations]])", path, keyList(un))
	}
	return &snap, nil
}

// enrichTOML upgrades a BurntSushi parse error to its positioned form (file
// line:column + the offending line with a caret), far more useful than the terse
// message. Non-parse errors pass through unchanged.
func enrichTOML(err error) error {
	var pe toml.ParseError
	if errors.As(err, &pe) {
		return errors.New(pe.ErrorWithUsage())
	}
	return err
}

func keyList(keys []toml.Key) string {
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = "`" + k.String() + "`"
	}
	return strings.Join(parts, ", ")
}

// Config is the parsed global config.toml.
type Config struct {
	// Assembly is NOT global — it is per-snapshot (a snapshot is inherently
	// assembly-specific), set in the manifest. Reference FASTAs are keyed by
	// assembly in `References`, so a snapshot's reference is looked up from its
	// assembly (see ReferenceFor) rather than being pinned in the manifest.
	DataDir  string `toml:"data_dir"`
	CacheDir string `toml:"cache_dir"` // downloaded source files, cached by name/version
	// ToolDir holds container images and tool data. Always a filesystem path,
	// never an object store: a container binds {datadir}, and apptainer runs a
	// .sif from disk — neither can be handed a locator. Defaults to
	// <cache_dir>/… when the cache is local, and to <data_dir>/… when it is not.
	ToolDir         string               `toml:"tool_dir,omitempty"`
	TempDir         string               `toml:"temp_dir,omitempty"` // base for scratch dirs (tool workdirs, fan-out parts); default: $TMPDIR or /tmp
	DefaultSnapshot string               `toml:"default_snapshot"`
	AnnotationsDir  string               `toml:"annotations_dir"`      // root holding sources/ tools/ snapshots/ (default "annotations")
	RegistryURL     string               `toml:"registry_url"`         // single registry (legacy); HTTPS registry.toml URL or base
	Registries      []string             `toml:"registries,omitempty"` // multiple registries (HTTPS registry.toml URLs)
	Database        Database             `toml:"database"`
	References      map[string]Reference `toml:"references,omitempty"` // assembly -> reference FASTA

	dir string `toml:"-"` // directory holding config.toml, for resolving paths
}

// DefaultRegistry is the built-in registry used when none is configured. A
// registry is just a static HTTPS registry.toml (any host works).
const DefaultRegistry = "https://raw.githubusercontent.com/compgenlab/varianthub-public-data-registry/main/registry.toml"

// DefaultRegistryRepo is the GitHub repo (owner/name) that `registry submit`
// targets. Submission is GitHub-specific and only works against this repo.
const DefaultRegistryRepo = "compgenlab/varianthub-public-data-registry"

// RegistryLocations returns the effective registries to search, in order:
// the explicit `registries` list, else the legacy single `registry_url`, else
// the built-in default. Each is an HTTPS registry.toml URL (or a base).
func (c *Config) RegistryLocations() []string {
	if len(c.Registries) > 0 {
		return c.Registries
	}
	if c.RegistryURL != "" {
		return []string{c.RegistryURL}
	}
	return []string{DefaultRegistry}
}

// Database selects and locates the DB cache backend.
type Database struct {
	Backend string `toml:"backend"` // "sqlite" (default) or "postgres"
	Path    string `toml:"path"`    // file path (sqlite) or DSN (postgres)
}

// Reference pins the reference genome FASTA for one assembly (used by external
// tools and normalization). Configured per assembly under `[references.<assembly>]`.
type Reference struct {
	Fasta string `toml:"fasta"`
}

// ReferenceFor returns the reference FASTA configured for an assembly (from
// `[references.<assembly>]`), or "" if none. Config values are already
// $VARHUB_HOME-expanded at Load time.
func (c *Config) ReferenceFor(assembly string) string {
	return c.References[assembly].Fasta
}

// Snapshot is a version-pinned bundle assembled from a snapshots/<name>.toml
// manifest: the manifest references sources/tools (by name:version) that live under
// annotations_dir, and carries snapshot-scoped config (assembly, reference, defaults).
type Snapshot struct {
	Name string `toml:"-"` // the manifest's base filename (also the version stamp)
	// Sources serializes with `[[sources]]` — a single-item Snapshot is how one source
	// fragment is written (WriteFragment) and read (decodeFragment). On a loaded snapshot
	// it is populated by resolving the manifest's refs. A type="tool" source is just a
	// source here (see ToolSources / Source.AsTool).
	Sources []Source `toml:"sources,omitempty"`
	// Snapshot-scoped config. Title/Description/Assembly come from the manifest;
	// Reference is resolved from the global config by Assembly (ReferenceFor). Not
	// serialized on a source/tool fragment.
	Title       string   `toml:"-"` // human-readable name (the manifest `title`); falls back to Name
	Description string   `toml:"-"`
	Assembly    string   `toml:"-"`
	Reference   string   `toml:"-"` // resolved FASTA for Assembly (config [references.<assembly>])
	Defaults    []string `toml:"-"` // default annotation names (manifest `default_annotations`)
	// Annotations is the flat, derived list (one per nested annotation, Source set from
	// its parent). NOT serialized — rebuilt by normalize().
	Annotations []Annotation `toml:"-"`
}

// SnapshotConfig is the on-disk snapshots/<name>.toml manifest: which sources/tools a
// snapshot includes (by name:version) plus its snapshot-scoped settings.
type SnapshotConfig struct {
	Title       string   `toml:"title,omitempty"` // human-readable name (e.g. "July 2026 Clinical Panel")
	Description string   `toml:"description,omitempty"`
	Assembly    string   `toml:"assembly,omitempty"`
	Sources     []string `toml:"sources,omitempty"`             // "name:version" refs (incl. tool sources)
	Defaults    []string `toml:"default_annotations,omitempty"` // annotation names

	// Prefixes overrides a source's annotation prefix, keyed by the same
	// "name:version" ref the sources list uses:
	//
	//   [annotation_prefixes]
	//     "gencode:48" = "GENCODE_48_"
	//     "gencode:49" = "GENCODE_49_"
	//
	// A snapshot is where two versions of one source actually meet, so it is
	// where the disambiguation belongs — the source manifest cannot know what
	// else a snapshot pins, and the locations overlay is about where data sits
	// rather than what the output is called.
	//
	// The value "-" means no prefix, which an empty string cannot express: an
	// absent entry has to fall through to the manifest's own default.
	Prefixes map[string]string `toml:"annotation_prefixes,omitempty"`
}

// SourceByName finds a source by its name.
func (snap *Snapshot) SourceByName(name string) *Source { return snap.source(name) }

// DisplayTitle is the snapshot's human-readable name (Title), falling back to its
// slug (Name) when no title is set.
func (snap *Snapshot) DisplayTitle() string {
	if snap.Title != "" {
		return snap.Title
	}
	return snap.Name
}

// ToolSources returns the snapshot's type="tool" sources (in list order).
func (snap *Snapshot) ToolSources() []Source {
	var out []Source
	for _, s := range snap.Sources {
		if s.IsTool() {
			out = append(out, s)
		}
	}
	return out
}

// Source is an annotation source. Type selects the kind:
//   - "" (default): a pinned, tabix-indexed data file (uses Format), possibly a
//     per-chromosome set (url/localpath {chrom} template — see Chroms) or a build recipe.
//   - "builtin": a container for built-in annotators (no data file).
//   - "tool": a dynamically-generated annotation from an external (often containerized)
//     annotator that runs per-query — see the tool-only fields below and AsTool.
type Source struct {
	Type    string `toml:"type,omitempty"` // "" = data file (uses Format) | "builtin" | "tool"
	Name    string `toml:"name,omitempty"`
	Version string `toml:"version,omitempty"`
	Title   string `toml:"title,omitempty"` // human-readable name (e.g. "ClinVar variant significance"); falls back to Name

	Description string `toml:"description,omitempty"` // one-line description of the source

	Assembly string `toml:"assembly,omitempty"` // genome assembly (e.g. GRCh38); verified against config
	Format   string `toml:"format,omitempty"`   // vcf | bed | tab

	// NonCommercial marks that this source's data/annotations carry commercial-use
	// restrictions (e.g. CADD). Informational only — nothing is blocked; it is
	// surfaced in `registry list`, at download, and in the server's annotation
	// discovery so users are aware.
	NonCommercial bool `toml:"non_commercial,omitempty"`

	// URL / URLIndex are the canonical reference locations (kept for provenance and
	// the registry). LocalPath / LocalPathIndex are this machine's actual files: if
	// LocalPath is set the file is used exactly and never downloaded.
	URL            string `toml:"url,omitempty"`
	URLIndex       string `toml:"url_index,omitempty"`
	LocalPath      string `toml:"localpath,omitempty"`       // exact local data file (abs, or rel to data_dir); env vars expanded
	LocalPathIndex string `toml:"localpath_index,omitempty"` // exact local index (else alongside data); env vars expanded

	// Integrity, as "<algo>:<hex-or-url>" (algo = md5|sha1|sha256). Optional —
	// verified only when present. A URL value is fetched and parsed (md5sum-style
	// manifest or a single hash). {chrom} is templated per file for multi-file sources.
	Checksum      string `toml:"checksum,omitempty"`
	ChecksumIndex string `toml:"checksum_index,omitempty"`

	// Chroms lists the chromosomes to expand a {chrom} template over (multi-file
	// sources, e.g. one file per chromosome). Ignored when url/localpath has no {chrom}.
	Chroms []string `toml:"chroms,omitempty"`
	// Alts lists the alternate bases to expand an {alt} template over — for per-alt
	// bigWig sets (AlphaMissense/CADD/REVEL: a/c/g/t.bw), where each file holds the
	// score for that substitution. Defaults to a,c,g,t. Ignored without {alt}.
	Alts []string `toml:"alts,omitempty"`
	// Files is an explicit list of files all queried + merged for one source (a
	// union split by something other than chromosome, e.g. coding + indels).
	Files []FileSpec `toml:"files,omitempty"`

	// tab-only, optional: 1-based REF/ALT columns for allele-aware matching (e.g.
	// REVEL, CADD). Omit (0) for position-only matching. Ignored for vcf/bed.
	RefCol int `toml:"ref_col,omitempty"`
	AltCol int `toml:"alt_col,omitempty"`

	// gtf-only, optional: required GTF tag(s) — only features carrying every
	// listed tag are used (e.g. "basic" for the GENCODE basic set). Ignored for
	// other formats.
	GTFTags []string `toml:"gtf_tags,omitempty"`

	// --- type="genelist" only: flag a variant when its gene (per the referenced
	// GTF model) is in the list. GTF names a gtf source in the same snapshot; the
	// gene set is Genes (inline) ∪ GenesFile (one symbol per line). GeneField picks
	// which GTF attribute to match. ---------------------------------------------
	GTF       string   `toml:"gtf,omitempty"`        // referenced GTF source: "name" or "name:version"
	Genes     []string `toml:"genes,omitempty"`      // inline gene symbols (or IDs)
	GenesFile string   `toml:"genes_file,omitempty"` // file of genes, one per line (blank/#comment lines skipped); relative to the source fragment dir
	GeneField string   `toml:"gene_field,omitempty"` // GTF attribute to match: "gene_name" (default) | "gene_id"

	GTFRef *Source `toml:"-"` // resolved referenced GTF source (set at snapshot load)

	// Build, when set, produces this source's data file from a download+preprocess
	// recipe instead of a ready-to-use url/localpath. Run once by `varhub download`
	// and cached. Mutually exclusive with url/localpath/files/chroms.
	Build *SourceBuild `toml:"build,omitempty"`

	// Requires lists external executables that must be on PATH for this source's
	// build recipe (or type="tool" steps) to run (e.g. "python3", "unzip"). Checked
	// by `varhub download`/`varhub annotate`. Irrelevant for plain (non-build) sources.
	Requires []string `toml:"requires,omitempty"`

	// --- type="tool" only: an external annotator run per-query (see AsTool) -------
	//
	// Image is the container (a docker://|oras://|shub:// ref that is pulled, or a
	// .sif URL that is downloaded). InputFormat is how query variants are written to
	// {input}: "vcf" (default) or a per-variant line template (placeholders {chrom}
	// {pos} {pos0} {ref} {alt} {end}). Output is consumed like a data source of Format
	// (vcf|tab). Setup runs once (image acquire time); Steps run per annotate.
	Image string `toml:"image,omitempty"`
	// See Tool.ImageChecksum — declared here because a tool is a [[sources]]
	// entry with type = "tool", and the fragment decoder rejects a key this
	// struct does not know.
	ImageChecksum string `toml:"image_checksum,omitempty"`

	// AnnotationPrefix declares the prefix this source's annotation names
	// already carry — VEP names every field VEP_Allele, VEP_Consequence, so it
	// declares "VEP_".
	//
	// Declaring it is what makes renaming possible: an override replaces this
	// prefix rather than stacking on top of it, so "VEP_113_" yields
	// VEP_113_Allele and not VEP_113_VEP_Allele. One line per manifest, versus
	// spelling out `field` on every annotation so the names could be written
	// bare.
	//
	// Overridden per deployment or per snapshot. That is what lets two versions
	// of one source share a snapshot: annotations are collected into a flat list
	// with no collision check, so identically named fields from two sources are
	// both written and one wins silently.
	AnnotationPrefix string `toml:"annotation_prefix,omitempty"`
	Engine           string `toml:"engine,omitempty"`       // container exec program; default "apptainer"
	InputFormat      string `toml:"input_format,omitempty"` // "vcf" (default) | per-variant line template
	Output           string `toml:"output,omitempty"`       // output filename the last step writes
	Setup            []Step `toml:"setup,omitempty"`        // one-time install into {datadir}
	Threads          int    `toml:"threads,omitempty"`      // per-run CPU count → {threads} (e.g. vep --fork)
	Steps            []Step `toml:"steps,omitempty"`        // per-run steps producing Output
	// Assets are helper files co-located with the fragment that Steps need (staged
	// into the step workdir, referenced as {workdir}/<name>). Tool sources use this;
	// build sources use build.assets.
	Assets []string `toml:"assets,omitempty"`

	// Annotations declared on this source (nested; their Source is this source's
	// Name, or the builtin name for a type="builtin" source).
	Annotations []Annotation `toml:"annotations,omitempty"`

	// Stream reads the source from its URL instead of caching a copy. For
	// something like gnomAD — tens of gigabytes, queried at a handful of loci —
	// downloading it to read 0.01% of it is the wrong trade. The index is
	// fetched whole and the data by range request.
	Stream bool `toml:"stream,omitempty"`

	// RequiresReference marks a source that cannot work without a reference
	// genome — VEP's --fasta, or a builtin computing trinucleotide context.
	//
	// Declared rather than inferred, and checked when a snapshot is assembled:
	// an unmet requirement is otherwise invisible until {ref} renders empty and
	// the tool fails on a mangled command line, which reads as a flag problem
	// and says nothing about a missing genome.
	RequiresReference bool `toml:"requires_reference,omitempty"`

	// locations is the overlay recording where this source's data was actually
	// provisioned. Attached at load time; not part of the manifest.
	locations *Locations
	// prefixOverride is the snapshot's annotation prefix for this source, set
	// while the snapshot is loaded. Not serialized: it belongs to the snapshot
	// that pinned this source, not to the source.
	prefixOverride string `toml:"-"`
}

// DisplayTitle is the source's human-readable name (Title), falling back to its
// slug (Name) when no title is set.
func (s Source) DisplayTitle() string {
	if s.Title != "" {
		return s.Title
	}
	return s.Name
}

// IsTool reports whether this source is an external per-query annotator.
func (s Source) IsTool() bool { return s.Type == "tool" }

// IsGeneList reports whether this source flags variants by gene membership.
func (s Source) IsGeneList() bool { return s.Type == "genelist" }

// GenesFilePath resolves a genelist's genes_file: env-expanded, absolute as-is,
// else relative to the source's fragment directory. Empty when no file is set.
func (c *Config) GenesFilePath(s Source) string {
	p := os.ExpandEnv(s.GenesFile)
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(filepath.Dir(c.SourceFile(s.Name, s.Version)), p)
}

// GeneSet resolves a genelist source's membership set: the inline Genes plus, when
// set, GenesFile (one symbol per line; blank lines and #-comments skipped; the
// first whitespace/comma-delimited field of each line is taken). Keys are
// upper-cased for case-insensitive matching.
func (c *Config) GeneSet(s Source) (map[string]bool, error) {
	set := map[string]bool{}
	add := func(g string) {
		if g = strings.TrimSpace(g); g != "" {
			set[strings.ToUpper(g)] = true
		}
	}
	for _, g := range s.Genes {
		add(g)
	}
	if path := c.GenesFilePath(s); path != "" {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("genelist %s: open genes_file: %w", s.ID(), err)
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			add(strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == '\t' || r == ' ' })[0])
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("genelist %s: read genes_file: %w", s.ID(), err)
		}
	}
	return set, nil
}

// resolveGeneLists wires each genelist source to its referenced GTF source (by
// name or name:version) and defaults its annotations to type="flag". Called before
// normalize so the flat annotation list carries the resolved type. The referenced
// GTF must be a gtf source present in the same snapshot.
func (snap *Snapshot) resolveGeneLists() error {
	for i := range snap.Sources {
		s := &snap.Sources[i]
		if !s.IsGeneList() {
			continue
		}
		if s.GTF == "" {
			return fmt.Errorf("genelist %q: needs gtf = \"name[:version]\" (a GTF source in this snapshot)", s.ID())
		}
		name := s.GTF
		if i := strings.IndexByte(name, ':'); i >= 0 {
			name = name[:i]
		}
		ref := snap.SourceByName(name)
		if ref == nil || !ref.IsGTFSource() {
			return fmt.Errorf("genelist %q: gtf %q is not a GTF source in this snapshot", s.ID(), s.GTF)
		}
		refCopy := *ref
		s.GTFRef = &refCopy
		for j := range s.Annotations {
			if s.Annotations[j].Type == "" {
				s.Annotations[j].Type = "flag"
			}
		}
	}
	return nil
}

// validateGeneListSource checks a type="genelist" container and its annotations.
func validateGeneListSource(s *Source) error {
	if s.GTFRef == nil {
		return fmt.Errorf("genelist %q: gtf %q not resolved to a GTF source in this snapshot", s.ID(), s.GTF)
	}
	if len(s.Genes) == 0 && s.GenesFile == "" {
		return fmt.Errorf("genelist %q: needs genes = [...] and/or genes_file", s.ID())
	}
	switch strings.ToLower(s.GeneField) {
	case "", "gene_name", "gene_id":
	default:
		return fmt.Errorf("genelist %q: gene_field %q must be gene_name or gene_id", s.ID(), s.GeneField)
	}
	if len(s.Annotations) == 0 {
		return fmt.Errorf("genelist %q: needs at least one annotation", s.ID())
	}
	for _, a := range s.Annotations {
		if a.Name == "" {
			return fmt.Errorf("genelist %q: an annotation needs a name", s.ID())
		}
		if a.Type != "flag" {
			return fmt.Errorf("genelist %q annotation %q: only type=\"flag\" is supported", s.ID(), a.Name)
		}
		if a.Builtin != "" {
			return fmt.Errorf("genelist %q annotation %q: `builtin` is not valid here", s.ID(), a.Name)
		}
	}
	return nil
}

// AsTool projects a type="tool" source onto the internal Tool execution view used
// by the tool runner/cache (internal/tool, annotate/toolcache). Tool is no longer a
// TOML type — it exists only as this projection.
func (s Source) AsTool() Tool {
	return Tool{
		Name: s.Name, Version: s.Version,
		Image: s.Image, Engine: s.Engine, Output: s.Output, Format: s.Format,
		InputFormat: s.InputFormat, RefCol: s.RefCol, AltCol: s.AltCol,
		Setup: s.Setup, Threads: s.Threads, Steps: s.Steps,
		Requires: s.Requires, Assets: s.Assets, Annotations: s.Annotations,
		ImageChecksum: s.ImageChecksum,
		// From the overlay, not the manifest: whether a machine that runs setup
		// is the machine that uses it is a deployment fact.
		ToolCache: s.locations.ToolCacheRoot(),
	}
}

// SourceBuild is a preprocessing recipe that produces a source's data file — for
// sources that need significant prep (e.g. REVEL: download many CSV zips, convert,
// merge, index). Because it lives in the fragment, such a source is self-contained
// and registry-shareable. `varhub download` fetches Inputs + Assets into a scratch
// workdir, runs the Run steps (which must write {output}), then caches + indexes
// the result. Step placeholders: {workdir} {inputs} {output} {threads}.
type SourceBuild struct {
	Inputs []string `toml:"inputs,omitempty"` // URLs fetched into {inputs}/ before the steps run
	Assets []string `toml:"assets,omitempty"` // helper files: a URL, or a path relative to the fragment dir
	Run    []string `toml:"run"`              // shell steps; the last must write {output} (and may index it)
	Output string   `toml:"output,omitempty"` // produced filename (default <name>.<format>.gz)
}

// BuildOutput is the produced data filename for a build source.
func (s Source) BuildOutput() string {
	if s.Build != nil && s.Build.Output != "" {
		return s.Build.Output
	}
	f := s.Format
	if f == "" {
		f = "tab"
	}
	return s.Name + "." + f + ".gz"
}

// ID is the data_source_id (name:version, a docker-style tag).
func (s Source) ID() string { return s.Name + ":" + s.Version }

// IsBuiltinSource reports whether this is a built-in-annotator container.
func (s Source) IsBuiltinSource() bool { return s.Type == "builtin" }

// IsReference reports whether this source is a reference genome.
//
// A reference is a source like any other — versioned, assembly-scoped, fetched
// and indexed by the same machinery, pinned by a snapshot so a run stays
// reproducible when a newer patch release appears. It differs in being read by
// path rather than queried by locus: it contributes no annotations of its own,
// and a tool step opens it through {ref}.
func (s Source) IsReference() bool { return s.Type == "reference" }

// IsGTFSource reports whether this is a GTF gene-annotation source. A GTF source
// is read into memory (no tabix index) and exposes a fixed vocabulary of derived
// fields (see GTFFields) that its annotations select via `field`.
func (s Source) IsGTFSource() bool { return s.Format == "gtf" }

// IsBBISource reports whether this is a UCSC BBI source (bigWig or bigBed). BBI
// files are self-indexed, so they are downloaded and queried in place (no tabix).
func (s Source) IsBBISource() bool { return s.Format == "bigwig" || s.Format == "bigbed" }

// IsMultiFile reports whether the source expands a {chrom} template into one file
// per chromosome (over Chroms).
func (s Source) IsMultiFile() bool {
	return strings.Contains(s.URL, "{chrom}") || strings.Contains(s.LocalPath, "{chrom}")
}

// IsPerAlt reports whether the source expands an {alt} template into one file per
// alternate base (over Alts) — a per-alt bigWig set.
func (s Source) IsPerAlt() bool {
	return strings.Contains(s.URL, "{alt}") || strings.Contains(s.LocalPath, "{alt}")
}

// altList is the alternate bases for an {alt} template (defaults to a,c,g,t).
func (s Source) altList() []string {
	if len(s.Alts) > 0 {
		return s.Alts
	}
	return []string{"a", "c", "g", "t"}
}

// FileSpec is one file of an explicit multi-file source (Source.Files). Each file
// carries its own url/index/checksum and optional local path.
type FileSpec struct {
	URL            string `toml:"url,omitempty"`
	URLIndex       string `toml:"url_index,omitempty"`
	LocalPath      string `toml:"localpath,omitempty"`
	LocalPathIndex string `toml:"localpath_index,omitempty"`
	Checksum       string `toml:"checksum,omitempty"`
	ChecksumIndex  string `toml:"checksum_index,omitempty"`
}

// SourceFile is one concrete file of a source (a single file, one chromosome of a
// {chrom} source, or one entry of an explicit Files list), with its resolved
// on-disk paths and canonical URLs.
type SourceFile struct {
	Chrom         string // "" for a single-file source
	Alt           string // "" unless this is one file of a per-alt {alt} set
	Path          string // resolved on-disk data path (localpath if set, else cache)
	IndexPath     string // resolved on-disk index path ("" = alongside Path)
	Local         bool   // true ⇒ use Path/IndexPath exactly, never download
	URL           string // canonical download URL (used only when !Local)
	URLIndex      string
	Checksum      string
	ChecksumIndex string
}

// ResolveSourceFiles returns the concrete file(s) for a source: one per entry of an
// explicit Files list, or one per chromosome for a {chrom} template, else a single
// file.
// ResolveSourceFiles resolves a source's files for *reading*, applying the
// location overlay when one is recorded.
//
// Provisioning wants the other question — where should this go? — and asks
// [Config.ResolveSourceTargets]. Keeping them apart is what lets `download
// --to` move a source: it writes by convention to the current cache_dir and
// records the result, rather than writing back to wherever the file used to be.
func (c *Config) ResolveSourceFiles(s Source) []SourceFile {
	if s.locations.Empty() {
		out := c.ResolveSourceTargets(s)
		if s.Stream {
			// Read in place: the url is the location, and there is no cached
			// copy to point at. Applied here and not in ResolveSourcePath
			// because that serves provisioning too — resolving a *target* to
			// the url makes `download --no-stream` write to a directory named
			// after it.
			for i := range out {
				out[i].Path = out[i].URL
				out[i].IndexPath = out[i].URLIndex
			}
		}
		return out
	}
	// A recorded root replaces cache_dir, and the usual layout applies under it,
	// so a multi-file source needs no per-file bookkeeping.
	out := c.resolveTargetsIn(s, s.locations.Root)
	for i := range out {
		fl, ok := s.locations.File(out[i].Chrom, out[i].Alt)
		if !ok {
			continue
		}
		// Path only. Local stays as the manifest declared it: it means "never
		// download", which an overlay entry does not, and conflating the two
		// would make download skip sources it should refresh.
		out[i].Path = fl.Path
		if fl.Index != "" {
			out[i].IndexPath = fl.Index
		}
	}
	return out
}

// ResolveSourceTargets resolves a source's files by the cache_dir convention,
// ignoring any overlay — where provisioning should put them.
func (c *Config) ResolveSourceTargets(s Source) []SourceFile {
	return c.resolveTargetsIn(s, "")
}

// resolveTargetsIn resolves a source's files under root, or under cache_dir when
// root is empty.
func (c *Config) resolveTargetsIn(s Source, root string) []SourceFile {
	if root != "" {
		c2 := *c
		c2.CacheDir = root
		c = &c2
		// A recorded location means the data was provisioned somewhere, so read
		// that copy even if the manifest prefers streaming. `stream` is the
		// publisher's suggestion; downloading it is the operator's decision, and
		// having downloaded it should not leave reads going to the origin.
		s.Stream = false
	}
	return c.resolveTargets(s)
}

func (c *Config) resolveTargets(s Source) []SourceFile {
	if s.IsGeneList() {
		return nil // a genelist has no data file of its own; it reads the referenced GTF
	}
	if s.Build != nil {
		// A build source resolves to its single produced (cached) file.
		return []SourceFile{{Path: c.ResolveSourcePath(s)}}
	}
	mk := func(sub Source, chrom, alt string) SourceFile {
		return SourceFile{
			Chrom:         chrom,
			Alt:           alt,
			Path:          c.ResolveSourcePath(sub),
			IndexPath:     c.resolveLocalIndex(sub),
			Local:         sub.LocalPath != "",
			URL:           sub.URL,
			URLIndex:      sub.URLIndex,
			Checksum:      sub.Checksum,
			ChecksumIndex: sub.ChecksumIndex,
		}
	}
	if s.IsPerAlt() {
		// One file per alternate base ({alt} → a/c/g/t). No index (BBI self-indexed).
		alts := s.altList()
		out := make([]SourceFile, 0, len(alts))
		for _, a := range alts {
			s2 := s
			rep := func(v string) string { return strings.ReplaceAll(v, "{alt}", a) }
			s2.URL, s2.LocalPath = rep(s.URL), rep(s.LocalPath)
			out = append(out, mk(s2, "", a))
		}
		return out
	}
	if len(s.Files) > 0 {
		out := make([]SourceFile, 0, len(s.Files))
		for _, f := range s.Files {
			sub := s // inherit format etc.; carry this file's locations
			sub.Files, sub.Chroms = nil, nil
			sub.URL, sub.LocalPath = f.URL, f.LocalPath
			sub.URLIndex, sub.LocalPathIndex = f.URLIndex, f.LocalPathIndex
			sub.Checksum, sub.ChecksumIndex = f.Checksum, f.ChecksumIndex
			out = append(out, mk(sub, "", ""))
		}
		return out
	}
	chroms := []string{""}
	if s.IsMultiFile() {
		chroms = s.Chroms
	}
	out := make([]SourceFile, 0, len(chroms))
	for _, ch := range chroms {
		s2 := s
		if ch != "" {
			rep := func(v string) string { return strings.ReplaceAll(v, "{chrom}", ch) }
			s2.URL, s2.LocalPath = rep(s.URL), rep(s.LocalPath)
			s2.URLIndex, s2.LocalPathIndex = rep(s.URLIndex), rep(s.LocalPathIndex)
			s2.Checksum, s2.ChecksumIndex = rep(s.Checksum), rep(s.ChecksumIndex)
		}
		out = append(out, mk(s2, ch, ""))
	}
	return out
}

// DataSource projects a Source onto the storage type.
func (s Source) DataSource() model.DataSource {
	return model.DataSource{Name: s.Name, Version: s.Version, Path: s.LocalPath}
}

// Tool is the internal execution view of a type="tool" Source (built by
// Source.AsTool) — an external (often containerized) annotator, e.g. VEP/ANNOVAR.
// It transforms the query variants ({input}) into an annotated output file (tab/vcf)
// via an ordered list of steps, then that output is consumed as a normal source.
// NOTE: Tool is no longer decoded from or written to TOML — a tool is a Source with
// type="tool"; these fields mirror that source's fields.
type Tool struct {
	Name    string `toml:"name"`
	Version string `toml:"version"`
	// Image is the container: a registry ref (docker://, oras://, shub://) that
	// is pulled and converted, or a prebuilt .sif to download — over http(s), or
	// from an object store as s3://bucket/key. Cached by name/version.
	//
	// A docker:// ref may name a digest (repo@sha256:…) instead of a tag, which
	// is the only way to pin what gets pulled: a tag can be re-pushed. The
	// converted SIF's bytes are not reproducible either way — apptainer inserts
	// its own configuration per pull — so ImageChecksum applies to a prebuilt
	// image only.
	Image string `toml:"image,omitempty"`
	// ImageChecksum verifies a downloaded prebuilt image, as "<algo>:<hex>".
	//
	// Only meaningful for a .sif that is fetched whole. Converting a docker://
	// ref produces different bytes on every pull, so a checksum there would fail
	// for everyone including the person who wrote it.
	ImageChecksum string `toml:"image_checksum,omitempty"`
	// ToolCache is where this tool's installed data is archived so another
	// machine can unpack it rather than repeating the install — an object-store
	// locator. Empty means it is not archived at all.
	//
	// Set from the source's locations overlay, not the manifest: whether the
	// machine that runs setup is the machine that later uses it depends on how
	// something is deployed, and a manifest shared through a registry would be
	// asserting it for every consumer.
	//
	// One field rather than a flag plus a destination, so there is no way to be
	// switched on with nowhere to write.
	ToolCache   string `toml:"-"`
	Engine      string `toml:"engine,omitempty"`       // container exec program; default "apptainer"
	Output      string `toml:"output,omitempty"`       // output filename the last step writes (default name.<format>.gz)
	Format      string `toml:"format,omitempty"`       // vcf | tab — how the output is consumed
	InputFormat string `toml:"input_format,omitempty"` // "vcf" (default) | per-variant line template
	RefCol      int    `toml:"ref_col,omitempty"`      // tab output: 1-based REF column
	AltCol      int    `toml:"alt_col,omitempty"`      // tab output: 1-based ALT column

	// Setup runs once after the image is acquired (`varhub download`) to install the
	// tool's data into its data dir ({datadir}, bound into container steps).
	Setup   []Step `toml:"setup,omitempty"`
	Threads int    `toml:"threads,omitempty"` // per-run CPU count → {threads} (e.g. vep --fork)
	Steps   []Step `toml:"steps"`

	// Requires lists external executables that must be on PATH for this tool to
	// run (e.g. "python3", "bgzip"). Checked by `varhub download` and `varhub
	// annotate` before any step runs. The container engine is checked
	// automatically when the tool uses a container — see RequiredSoftware.
	Requires []string `toml:"requires,omitempty"`

	// Assets are helper files co-located with the fragment (a filename relative to
	// the snapshot/fragment dir) that steps need — e.g. post-processing scripts.
	// They are staged into the step workdir before every run, so a step references
	// one as `{workdir}/<name>` (works in host and container steps). Declaring them
	// also lets the registry bundle them with the tool. (Mirrors SourceBuild.Assets.)
	Assets []string `toml:"assets,omitempty"`

	// Annotations declared on this tool's output (nested; their Source is this
	// tool's Name).
	Annotations []Annotation `toml:"annotations,omitempty"`
}

// ImageIsRef reports whether Image is a registry ref to pull (vs a .sif URL to
// download).
func (t Tool) ImageIsRef() bool {
	return strings.HasPrefix(t.Image, "docker://") ||
		strings.HasPrefix(t.Image, "oras://") ||
		strings.HasPrefix(t.Image, "shub://")
}

// Step is one stage of a tool pipeline: a templated shell command, optionally run
// inside the tool's container image.
type Step struct {
	Name      string `toml:"name,omitempty"`
	Run       string `toml:"run"`
	Container bool   `toml:"container,omitempty"`
}

// ID is the tool's data_source_id (name:version, a docker-style tag).
func (t Tool) ID() string { return t.Name + ":" + t.Version }

// OutputName is the tool's output filename (defaults to name.<format>.gz).
func (t Tool) OutputName() string {
	if t.Output != "" {
		return t.Output
	}
	f := t.Format
	if f == "" {
		f = "vcf"
	}
	return t.Name + "." + f + ".gz"
}

// ContainerEngine is the container exec program (default apptainer).
func (t Tool) ContainerEngine() string {
	if t.Engine != "" {
		return t.Engine
	}
	return "apptainer"
}

// usesContainer reports whether the tool acquires/runs a container image (an
// image to pull/exec, or any container step/setup step).
func (t Tool) usesContainer() bool {
	if t.Image != "" {
		return true
	}
	for _, s := range t.Steps {
		if s.Container {
			return true
		}
	}
	for _, s := range t.Setup {
		if s.Container {
			return true
		}
	}
	return false
}

// RequiredSoftware is the tool's declared Requires plus its container engine when
// the tool uses a container, so callers needn't list the engine explicitly. The
// returned slice is deduplicated; blank entries are dropped.
func (t Tool) RequiredSoftware() []string {
	names := append([]string(nil), t.Requires...)
	if t.usesContainer() {
		names = append(names, t.ContainerEngine())
	}
	seen := map[string]bool{}
	var out []string
	for _, n := range names {
		if n == "" || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// AsSource projects a tool onto a Source (its output becomes a local source file
// at path; used as-is, never downloaded).
func (t Tool) AsSource(path string) Source {
	return Source{Name: t.Name, Version: t.Version, Format: t.Format, LocalPath: path, RefCol: t.RefCol, AltCol: t.AltCol}
}

// Annotation declares one annotation field in the snapshot's schema. It lives
// nested under its source/tool, so it carries no `source` on disk; Source is
// populated from the parent by normalize().
type Annotation struct {
	Source string `toml:"-"` // derived: owning source/tool name, or builtin name
	// Builtin names the built-in annotator for an annotation under a type="builtin"
	// source (auto_id, tstv, …). Empty for file/tool annotations.
	Builtin string `toml:"builtin,omitempty"`
	// Name is the NEW annotation key written to the output (the INFO tag added to
	// each record / the value's key in the cache).
	Name string `toml:"name,omitempty"`
	// Field is what to read from the SOURCE: an INFO id (vcf) / column (bed,tab) /
	// "@ID" to copy the source ID. Defaults to Name. Ignored when Type is "flag".
	Field string `toml:"field,omitempty"`
	// Type is how the value is interpreted/stored: categorical (a string from a
	// limited set, optionally declared in Values), text (a free-form string),
	// numeric (a float, stored in value_num), or flag (presence only, no value).
	Type        string   `toml:"type,omitempty"`   // categorical | text | numeric | flag
	Match       string   `toml:"match,omitempty"`  // vcf only: "exact" (default) | "position"
	Unique      bool     `toml:"unique,omitempty"` // vcf only: de-duplicate multiple matches
	Values      []string `toml:"values,omitempty"` // declared enum (categorical)
	Description string   `toml:"description,omitempty"`
	Default     bool     `toml:"-"` // derived: name is in the snapshot's default_annotations
	// Args: parameters for a builtin (e.g. tags = "KEY:VALUE",
	// copy_logratio = "SOMATIC:GERMLINE[:st:gt]").
	Args string `toml:"args,omitempty"`
}

// BuiltinNames are the recognized built-in annotators (in prompt order).
var BuiltinNames = []string{
	"auto_id", "indel", "tstv", "vardist",
	"dosage", "vaf", "minor_strand", "fisher_sb",
	"tags", "copy_logratio",
}

var builtinSources = func() map[string]bool {
	m := make(map[string]bool, len(BuiltinNames))
	for _, n := range BuiltinNames {
		m[n] = true
	}
	return m
}()

// IsBuiltin reports whether name is a recognized built-in annotator.
func IsBuiltin(name string) bool { return builtinSources[name] }

// GTFFields are the derived fields a GTF source publishes; an annotation on a
// format="gtf" source selects one of these via `field` (case-insensitive). They
// map to the hts gtf gene model: gene name/ID/strand/biotype, the genic-region
// code, and the gene name split by coding vs non-coding.
var GTFFields = []string{"GENE", "GENEID", "STRAND", "BIOTYPE", "REGION", "CODING", "NONCODING"}

// IsGTFField reports whether name (case-insensitive) is a recognized GTF field.
func IsGTFField(name string) bool {
	u := strings.ToUpper(name)
	for _, f := range GTFFields {
		if f == u {
			return true
		}
	}
	return false
}

// AnnotationTypes are the recognized annotation value types (in prompt order). An
// empty type is allowed and defaults to categorical.
var AnnotationTypes = []string{"categorical", "text", "numeric", "flag"}

// ValidAnnotationType reports whether t is a recognized annotation type ("" = the
// categorical default, allowed).
func ValidAnnotationType(t string) bool {
	if t == "" {
		return true
	}
	for _, x := range AnnotationTypes {
		if x == t {
			return true
		}
	}
	return false
}

// IsFlag reports whether this is a presence-flag annotation.
func (a Annotation) IsFlag() bool { return a.Type == "flag" }

// FieldName is the source field to read, defaulting to Name.
func (a Annotation) FieldName() string {
	if a.Field != "" {
		return a.Field
	}
	return a.Name
}

// IsNumeric reports whether values for this annotation are numeric.
func (a Annotation) IsNumeric() bool { return a.Type == "numeric" }

// normalize rebuilds the flat Annotations list from the nested source/tool
// annotations, populating each one's Source. Idempotent.
func (snap *Snapshot) normalize() {
	def := map[string]bool{}
	for _, n := range snap.Defaults {
		def[n] = true
	}
	snap.Annotations = nil
	add := func(a Annotation, source string) {
		a.Source = source
		a.Default = def[a.Name] // derived from the snapshot manifest, not the fragment
		snap.Annotations = append(snap.Annotations, a)
	}
	for si := range snap.Sources {
		s := &snap.Sources[si]
		// Re-prefix this source's output names once, in place, so every reader —
		// the flat list below, the overlay annotators, the cache keys — sees the
		// same name. A name that differs between two readers is a bug nobody
		// would find quickly.
		s.applyAnnotationPrefix()
		for ai := range s.Annotations {
			a := &s.Annotations[ai]
			if s.IsBuiltinSource() {
				// A builtin's output name defaults to the builtin name when omitted
				// (a blank key is never useful). Done in place so the overlay
				// BuiltinSource — which reads the source's own annotations — and the
				// flat list below both see the resolved name.
				if a.Name == "" {
					a.Name = a.Builtin
				}
				add(*a, a.Builtin)
			} else {
				add(*a, s.Name) // data and tool sources alike
			}
		}
	}
}

// applyAnnotationPrefix rewrites this source's output names to the effective
// prefix, leaving what is read from the source alone.
//
// A manifest declares the prefix its names already carry — VEP names every
// field VEP_Allele, VEP_Consequence — so swapping it for VEP_113_ is a
// substitution rather than a prepend. Declaring it costs the manifest one line;
// the alternative was spelling out `field` on all forty-odd annotations so the
// names could be written bare.
//
// Field is pinned first, because FieldName() falls back to Name lazily: rename
// Name without doing that and the annotator starts looking for VEP_113_Allele in
// a file that contains VEP_Allele, which fails as "no values" rather than as
// anything to do with naming.
func (s *Source) applyAnnotationPrefix() {
	declared, effective := s.AnnotationPrefix, s.EffectiveAnnotationPrefix()
	if declared == effective {
		return // nothing overrode it
	}
	for i := range s.Annotations {
		a := &s.Annotations[i]
		if a.Name == "" {
			continue
		}
		if a.Field == "" {
			a.Field = a.Name // what the source actually writes, before renaming
		}
		switch {
		case declared != "" && strings.HasPrefix(a.Name, declared):
			a.Name = effective + strings.TrimPrefix(a.Name, declared)
		case effective != "" && !strings.HasPrefix(a.Name, effective):
			// The manifest declared no prefix, so there is nothing to replace
			// and the new one is simply added.
			a.Name = effective + a.Name
		}
	}
}

// EffectiveAnnotationPrefix is the prefix actually applied, most specific first:
//
//	the snapshot that pinned this source  — "in this bundle, call it GENCODE_48_"
//	the deployment's overlay              — "this catalog always calls it that"
//	the manifest's own default            — what the source author suggested
//
// Each level knows something the one below it cannot: a manifest travels through
// a registry and cannot know what else is installed; an overlay is one catalog's
// standing answer; a snapshot is where two versions of a source actually meet.
//
// "-" means deliberately no prefix at any level, which an empty string cannot
// express — unset has to fall through rather than clear.
func (s Source) EffectiveAnnotationPrefix() string {
	for _, p := range []string{s.prefixOverride, s.locations.AnnotationPrefixOverride()} {
		if p == "-" {
			return ""
		}
		if p != "" {
			return p
		}
	}
	return s.AnnotationPrefix
}

// Normalize rebuilds the derived flat annotation list (exported for callers that
// decode a snapshot/fragment directly, e.g. the registry).
func (snap *Snapshot) Normalize() { snap.normalize() }

// SelectAnnotations resolves which annotations to apply: all when all is true; the
// named keys when keys is non-empty (error on an unknown key); otherwise the
// default set (annotations with Default = true).
func (snap *Snapshot) SelectAnnotations(keys []string, all bool) ([]Annotation, error) {
	if all {
		return snap.Annotations, nil
	}
	if len(keys) > 0 {
		byKey := map[string]Annotation{}
		for _, a := range snap.Annotations {
			byKey[a.Name] = a
		}
		out := make([]Annotation, 0, len(keys))
		for _, k := range keys {
			a, ok := byKey[k]
			if !ok {
				return nil, fmt.Errorf("unknown annotation %q", k)
			}
			out = append(out, a)
		}
		return out, nil
	}
	var def []Annotation
	for _, a := range snap.Annotations {
		if a.Default {
			def = append(def, a)
		}
	}
	return def, nil
}

// DropSource removes the named source (and its nested annotations), returning the
// number of annotations removed. Re-normalizes the flat list.
func (snap *Snapshot) DropSource(name string) int {
	before := len(snap.Annotations)
	kept := snap.Sources[:0]
	for _, s := range snap.Sources {
		if s.Name != name {
			kept = append(kept, s)
		}
	}
	snap.Sources = kept
	snap.normalize()
	return before - len(snap.Annotations)
}

// Load reads and validates the global config.toml. Values may reference
// $VARHUB_HOME / ${VARHUB_HOME}, which is expanded to the VARHUB_HOME env var
// (or "." when unset) before decoding. Other $NAME sequences are left intact.
func Load(path string) (*Config, error) {
	var c Config
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	home := os.Getenv("VARHUB_HOME")
	if home == "" {
		home = "."
	}
	expand := func(name string) string {
		if name == "VARHUB_HOME" {
			return home
		}
		return "${" + name + "}" // leave other vars intact
	}
	if _, err := toml.Decode(os.Expand(string(raw), expand), &c); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	if abs, err := filepath.Abs(path); err == nil {
		c.dir = filepath.Dir(abs)
	}
	// The cache is optional: an absent [database] leaves the backend empty (disabled),
	// so no varhub.db is created. Only a configured backend defaults its path.
	if c.Database.Backend == "sqlite" && c.Database.Path == "" {
		c.Database.Path = "varhub.db"
	}
	if c.AnnotationsDir == "" {
		c.AnnotationsDir = "annotations"
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// CacheEnabled reports whether the annotation cache DB is configured (a [database]
// backend is set). When false, annotate computes every locus and persists nothing.
func (c *Config) CacheEnabled() bool {
	return c.Database.Backend != "" && c.Database.Backend != "none"
}

func (c *Config) validate() error {
	// assembly is snapshot-scoped now (global is only a fallback), so not required here.
	switch c.Database.Backend {
	case "", "none", "sqlite", "postgres":
	default:
		return fmt.Errorf("config: unsupported database backend %q (want sqlite|postgres, or omit to disable)", c.Database.Backend)
	}
	return nil
}

// annotationsPath resolves annotations_dir relative to config.toml.
func (c *Config) annotationsPath() string {
	d := c.AnnotationsDir
	if d == "" {
		d = "annotations"
	}
	if filepath.IsAbs(d) || c.dir == "" {
		return d
	}
	return filepath.Join(c.dir, d)
}

// SourcesPath / SnapshotsPath are the sources/, snapshots/ dirs under annotations_dir.
// Tool sources live under sources/ too (type="tool") — there is no separate tools/ dir.
func (c *Config) SourcesPath() string   { return filepath.Join(c.annotationsPath(), "sources") }
func (c *Config) SnapshotsPath() string { return filepath.Join(c.annotationsPath(), "snapshots") }

// SourceDir is a source's version directory (where its assets co-locate); SourceFile
// is the <name>-<version>.toml inside it. Mirrors the registry layout. Tool sources
// (type="tool") live here too.
func (c *Config) SourceDir(name, version string) string {
	return filepath.Join(c.SourcesPath(), name, version)
}
func (c *Config) SourceFile(name, version string) string {
	return filepath.Join(c.SourceDir(name, version), name+"-"+version+".toml")
}

// SnapshotFile is a snapshot manifest path (snapshots/<name>.toml).
func (c *Config) SnapshotFile(name string) string {
	return filepath.Join(c.SnapshotsPath(), name+".toml")
}

// ResolveSourceRef resolves a "name[:version]" ref to (name, version) against the
// local sources/ tree (bare name = the sole version). Tool sources resolve here too.
func (c *Config) ResolveSourceRef(ref string) (string, string, error) {
	return resolveRef(c.SourcesPath(), ref)
}

// ReadSnapshotConfig / WriteSnapshotConfig load and save a snapshots/<name>.toml
// manifest (the source/tool refs + snapshot-scoped settings).
func ReadSnapshotConfig(path string) (*SnapshotConfig, error) {
	var sc SnapshotConfig
	if _, err := toml.DecodeFile(path, &sc); err != nil {
		return nil, err
	}
	return &sc, nil
}
func WriteSnapshotConfig(path string, sc *SnapshotConfig) error {
	return WriteTOML(path, sc)
}

// ListSnapshots returns the names of all snapshot manifests (snapshots/*.toml).
func (c *Config) ListSnapshots() ([]string, error) {
	entries, err := os.ReadDir(c.SnapshotsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".toml") {
			names = append(names, strings.TrimSuffix(e.Name(), ".toml"))
		}
	}
	sort.Strings(names)
	return names, nil
}

// ListSources returns every available "name:version" ref found on disk under
// sources/ (one per version dir), sorted — including tool sources. A missing tree
// yields no refs (not an error).
func (c *Config) ListSources() ([]string, error) { return listRefs(c.SourcesPath()) }

// listRefs walks base/<name>/<version>/ and returns "name:version" for each.
func listRefs(base string) ([]string, error) {
	names, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var refs []string
	for _, n := range names {
		if !n.IsDir() {
			continue
		}
		versions, err := os.ReadDir(filepath.Join(base, n.Name()))
		if err != nil {
			return nil, err
		}
		for _, v := range versions {
			if v.IsDir() {
				refs = append(refs, n.Name()+":"+v.Name())
			}
		}
	}
	sort.Strings(refs)
	return refs, nil
}

// resolveRef resolves a "name" or "name:version" reference (under base =
// SourcesPath/ToolsPath) to (name, version). A bare name picks the sole version dir,
// or errors if several exist (pin one).
func resolveRef(base, ref string) (name, version string, err error) {
	name, version, hasV := strings.Cut(ref, ":")
	if hasV && version != "" {
		return name, version, nil
	}
	dir := filepath.Join(base, name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", "", fmt.Errorf("%q not found (%s)", name, dir)
	}
	var versions []string
	for _, e := range entries {
		if e.IsDir() {
			versions = append(versions, e.Name())
		}
	}
	switch {
	case len(versions) == 0:
		return "", "", fmt.Errorf("%q has no versions under %s", name, dir)
	case len(versions) > 1:
		sort.Strings(versions)
		return "", "", fmt.Errorf("%q has multiple versions (%s) — pin one as %s:<version>",
			name, strings.Join(versions, ", "), name)
	}
	return name, versions[0], nil
}

// LoadSnapshot reads a snapshot manifest (snapshots/<name>.toml; default if empty),
// resolves its source/tool references from annotations_dir, and validates the result.
// Apply order follows the manifest's `sources` list (tool sources included).
func (c *Config) LoadSnapshot(name string) (*Snapshot, error) {
	if name == "" {
		name = c.DefaultSnapshot
	}
	if name == "" {
		return nil, fmt.Errorf("config: no snapshot given and default_snapshot is unset")
	}
	manifest := c.SnapshotFile(name)
	var mc SnapshotConfig
	md, err := toml.DecodeFile(manifest, &mc)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("snapshot %q not found (%s)", name, manifest)
		}
		return nil, fmt.Errorf("parse %s:\n%w", manifest, enrichTOML(err))
	}
	if un := md.Undecoded(); len(un) > 0 {
		return nil, fmt.Errorf("%s: unrecognized key(s): %s", manifest, keyList(un))
	}

	snap := &Snapshot{
		Name: name, Title: mc.Title, Description: mc.Description, Defaults: mc.Defaults,
		Assembly: mc.Assembly,
	}
	// The reference FASTA comes from the reference source this snapshot pins,
	// resolved after the sources are loaded below. Configuration is consulted
	// only as a fallback, for a deployment that placed a FASTA on the host by
	// other means.
	//
	// Pinned rather than global because a reference has versions — GRCh38.p13
	// and p14 are different genomes — and a snapshot exists to make a run
	// reproducible. A global lookup would silently change what an old snapshot
	// resolves to the moment a newer patch was registered.

	for _, ref := range mc.Sources {
		n, v, err := resolveRef(c.SourcesPath(), ref)
		if err != nil {
			return nil, fmt.Errorf("snapshot %q: source %w", name, err)
		}
		frag, err := decodeFragment(c.SourceFile(n, v))
		if err != nil {
			return nil, err
		}
		// Attach the source's overlay, if it has one. Without this every reader
		// of s.locations sees nil and resolution silently falls back to the
		// cache_dir convention — which is correct for a source with no overlay
		// and wrong for every source with one.
		loc, err := c.LoadLocations(n, v)
		if err != nil {
			return nil, fmt.Errorf("snapshot %q: source %s:%s: %w", name, n, v, err)
		}
		for i := range frag.Sources {
			frag.Sources[i].locations = loc
			// The snapshot's override, matched on the ref it was pinned by.
			if p, ok := mc.Prefixes[ref]; ok {
				frag.Sources[i].prefixOverride = p
			} else if p, ok := mc.Prefixes[n+":"+v]; ok {
				// The ref may have been written without a version ("gencode"),
				// so a keyed override still has to find it.
				frag.Sources[i].prefixOverride = p
			}
		}
		snap.Sources = append(snap.Sources, frag.Sources...)
	}

	if err := snap.resolveGeneLists(); err != nil {
		return nil, fmt.Errorf("snapshot %q: %w", name, err)
	}
	snap.normalize()
	if err := snap.validate(); err != nil {
		return nil, fmt.Errorf("snapshot %q: %w", name, err)
	}
	if err := c.verifyAssembly(snap); err != nil {
		return nil, fmt.Errorf("snapshot %q: %w", name, err)
	}
	if err := c.resolveReference(snap); err != nil {
		return nil, fmt.Errorf("snapshot %q: %w", name, err)
	}
	return snap, nil
}

// resolveReference sets snap.Reference from the reference source the snapshot
// pins, and checks that anything needing one has it.
//
// At most one, because {ref} has to have a single answer: two reference sources
// would make "the genome for this run" ambiguous, and annotating against the
// wrong one produces plausible values at coordinates that mean something else —
// the failure that is invisible in the output.
func (c *Config) resolveReference(snap *Snapshot) error {
	var pinned *Source
	for i := range snap.Sources {
		if !snap.Sources[i].IsReference() {
			continue
		}
		if pinned != nil {
			return fmt.Errorf("pins two reference genomes (%s and %s); a snapshot may pin one",
				pinned.ID(), snap.Sources[i].ID())
		}
		pinned = &snap.Sources[i]
	}

	if pinned != nil {
		files := c.ResolveSourceFiles(*pinned)
		if len(files) == 0 {
			return fmt.Errorf("reference %s resolves to no file", pinned.ID())
		}
		snap.Reference = files[0].Path
	} else {
		// A deployment that placed a FASTA on the host by other means.
		snap.Reference = c.ReferenceFor(snap.Assembly)
	}

	// Checked here rather than at run time so the failure lands on whoever
	// assembled the snapshot. Otherwise {ref} renders empty and the tool fails
	// on a mangled command line — "--fasta --fork 4" reads as a flag problem
	// and says nothing about a missing genome.
	if snap.Reference == "" {
		for _, src := range snap.Sources {
			if src.RequiresReference {
				return fmt.Errorf("source %s requires a reference genome, but none is pinned "+
					"(add a type=\"reference\" source for %s)", src.ID(), snap.Assembly)
			}
		}
	}
	return nil
}

// verifyAssembly rejects any source whose declared assembly differs from the
// snapshot's. Untagged sources (assembly = "") are allowed — they can't be checked.
// When the snapshot has no assembly at all, there's nothing to verify.
func (c *Config) verifyAssembly(snap *Snapshot) error {
	if snap.Assembly == "" {
		return nil
	}
	for _, s := range snap.Sources {
		if s.Assembly != "" && s.Assembly != snap.Assembly {
			return fmt.Errorf("source %q is assembly %q but snapshot assembly is %q",
				s.ID(), s.Assembly, snap.Assembly)
		}
	}
	return nil
}

func (snap *Snapshot) validate() error {
	if len(snap.Sources) == 0 {
		return fmt.Errorf("a snapshot needs at least one source")
	}
	seen := map[string]bool{}
	for i := range snap.Sources {
		s := &snap.Sources[i]
		if s.IsBuiltinSource() {
			if err := validateBuiltinSource(s); err != nil {
				return err
			}
			continue
		}
		if s.IsReference() {
			// A tool step binds the FASTA's directory into its container, so
			// this is the one source that must exist as a local file at run
			// time however the rest are read. Streaming it would render {ref}
			// as an s3:// locator that no tool can open.
			if s.Stream {
				return fmt.Errorf("reference %s cannot be streamed: a tool opens it by path",
					s.ID())
			}
			if len(s.Annotations) > 0 {
				return fmt.Errorf("reference %s declares annotations; a reference is read by "+
					"path and contributes none", s.ID())
			}
		}
		if s.Name == "" || s.Version == "" {
			return fmt.Errorf("each source needs a name and version")
		}
		if seen[s.ID()] {
			return fmt.Errorf("duplicate source %q", s.ID())
		}
		if s.Stream {
			if s.LocalPath != "" {
				return fmt.Errorf("source %q: `stream` and `localpath` contradict — one reads from the url, the other from disk", s.ID())
			}
			if s.Build != nil {
				return fmt.Errorf("source %q: `stream` cannot combine with `build`, which produces a local file", s.ID())
			}
			// Every file has to name where to read it from. A source may state
			// one url at the top — templated by {chrom} or {alt} — or give each
			// file its own in [[sources.files]]; CADD does the latter, with a
			// SNV table and an indel table that share nothing but their columns.
			if len(s.Files) > 0 {
				for i, f := range s.Files {
					if f.URL == "" && f.LocalPath == "" {
						return fmt.Errorf("source %q: `stream` needs a `url` on every [[sources.files]] entry (entry %d has none)", s.ID(), i+1)
					}
				}
			} else if s.URL == "" {
				return fmt.Errorf("source %q: `stream` reads from `url`, so the source needs one (or a url on each [[sources.files]] entry)", s.ID())
			}
		}
		seen[s.ID()] = true
		if s.IsTool() {
			if err := validateToolSource(s); err != nil {
				return err
			}
			continue
		}
		if s.IsGeneList() {
			if err := validateGeneListSource(s); err != nil {
				return err
			}
			continue
		}
		switch s.Format {
		case "", "vcf", "bed", "tab", "gtf", "bigwig", "bigbed":
		default:
			return fmt.Errorf("source %q: unsupported format %q (want vcf|bed|tab|gtf|bigwig|bigbed)", s.ID(), s.Format)
		}
		if s.Build != nil {
			// `url` is allowed alongside build as a provenance reference; localpath/
			// files/chroms describe an already-built file and conflict with a recipe.
			if s.LocalPath != "" || len(s.Files) > 0 || len(s.Chroms) > 0 {
				return fmt.Errorf("source %q: `build` cannot combine with localpath/files/chroms", s.ID())
			}
			if len(s.Build.Run) == 0 {
				return fmt.Errorf("source %q: build needs at least one `run` step", s.ID())
			}
		} else if s.URL == "" && s.LocalPath == "" && len(s.Files) == 0 {
			return fmt.Errorf("source %q needs a url, localpath, files, or build", s.ID())
		}
		if err := checksum.ValidateSpec(s.Checksum); err != nil {
			return fmt.Errorf("source %q checksum: %w", s.ID(), err)
		}
		if err := checksum.ValidateSpec(s.ChecksumIndex); err != nil {
			return fmt.Errorf("source %q checksum_index: %w", s.ID(), err)
		}
		if s.IsMultiFile() && len(s.Chroms) == 0 {
			return fmt.Errorf("source %q uses {chrom} but has no chroms list", s.ID())
		}
		if s.IsPerAlt() {
			if !s.IsBBISource() {
				return fmt.Errorf("source %q: {alt} templating is only for bigwig/bigbed sources", s.ID())
			}
			if s.IsMultiFile() || len(s.Files) > 0 {
				return fmt.Errorf("source %q: use an {alt} template, `files`, or a {chrom} template — not a combination", s.ID())
			}
		}
		if len(s.Files) > 0 && (s.URL != "" || s.LocalPath != "" || s.IsMultiFile()) {
			return fmt.Errorf("source %q: use `files`, a `url`/`localpath`, or a {chrom} template — not a combination", s.ID())
		}
		for _, a := range s.Annotations {
			if a.Builtin != "" {
				return fmt.Errorf("source %q annotation %q: `builtin` is only valid under a type=\"builtin\" source", s.ID(), a.Name)
			}
			if err := validateFileAnnotation(a); err != nil {
				return fmt.Errorf("source %q: %w", s.ID(), err)
			}
			if s.IsGTFSource() && !IsGTFField(a.FieldName()) {
				return fmt.Errorf("source %q annotation %q: field %q is not a GTF field (want %s)",
					s.ID(), a.Name, a.FieldName(), strings.Join(GTFFields, "|"))
			}
		}
	}
	// Every default_annotations name must match a declared annotation.
	names := map[string]bool{}
	for _, a := range snap.Annotations {
		names[a.Name] = true
	}
	for _, d := range snap.Defaults {
		if !names[d] {
			return fmt.Errorf("default_annotations references unknown annotation %q", d)
		}
	}
	return nil
}

// validateToolSource checks a type="tool" source: it needs run steps, a vcf|tab
// output format, a valid input_format (vcf or a {chrom}/{pos} template), and its
// annotations read the tool's output like any file source.
func validateToolSource(s *Source) error {
	if len(s.Steps) == 0 {
		return fmt.Errorf("tool source %q has no steps", s.ID())
	}
	switch s.Format {
	case "", "vcf", "tab":
	default:
		return fmt.Errorf("tool source %q: unsupported output format %q (want vcf|tab)", s.ID(), s.Format)
	}
	if s.InputFormat != "" && s.InputFormat != "vcf" {
		if !strings.Contains(s.InputFormat, "{chrom}") || !strings.Contains(s.InputFormat, "{pos}") {
			return fmt.Errorf("tool source %q: input_format template must contain {chrom} and {pos}", s.ID())
		}
	}
	for _, a := range s.Annotations {
		if a.Builtin != "" {
			return fmt.Errorf("tool source %q annotation %q: `builtin` is only valid under a type=\"builtin\" source", s.ID(), a.Name)
		}
		if err := validateFileAnnotation(a); err != nil {
			return fmt.Errorf("tool source %q: %w", s.ID(), err)
		}
	}
	return nil
}

// validateBuiltinSource checks a type="builtin" container and its annotations.
func validateBuiltinSource(s *Source) error {
	if s.URL != "" || s.LocalPath != "" || len(s.Files) > 0 || s.Format != "" {
		return fmt.Errorf("builtin source must not set url/localpath/files/format")
	}
	if len(s.Annotations) == 0 {
		return fmt.Errorf("builtin source has no annotations")
	}
	for _, a := range s.Annotations {
		if !IsBuiltin(a.Builtin) {
			return fmt.Errorf("builtin %q is not recognized (want %s)", a.Builtin, strings.Join(BuiltinNames, "|"))
		}
		if (a.Builtin == "tags" || a.Builtin == "copy_logratio") && a.Args == "" {
			return fmt.Errorf("builtin %q needs args", a.Builtin)
		}
	}
	return nil
}

// validateFileAnnotation checks a file/tool annotation (key + type).
func validateFileAnnotation(a Annotation) error {
	if a.Name == "" {
		return fmt.Errorf("annotation needs a key")
	}
	if !ValidAnnotationType(a.Type) {
		return fmt.Errorf("annotation %q: unknown type %q (want %s)", a.Name, a.Type, strings.Join(AnnotationTypes, "|"))
	}
	return nil
}

// source finds a non-builtin source by name (data or tool).
func (snap *Snapshot) source(name string) *Source {
	for i := range snap.Sources {
		if snap.Sources[i].IsBuiltinSource() {
			continue
		}
		if snap.Sources[i].Name == name {
			return &snap.Sources[i]
		}
	}
	return nil
}

// DataSources projects the snapshot's data sources (not builtins) onto the
// storage type.
func (snap *Snapshot) DataSources() []model.DataSource {
	var out []model.DataSource
	for _, s := range snap.Sources {
		if s.IsBuiltinSource() || s.IsTool() {
			continue // builtins + tool sources have no static data file to register
		}
		out = append(out, s.DataSource())
	}
	return out
}

// SetBaseDir sets the directory relative paths resolve against — what Load()
// infers from the config file's own location.
//
// Exported because a Config built in code, rather than loaded from disk, has an
// empty base and silently resolves every relative path against the process
// working directory. That is not a hypothetical: a test built one, and the
// download it ran wrote a source's files and its location overlay into the
// package directory, where they were committed.
func (c *Config) SetBaseDir(dir string) { c.dir = dir }

// resolveDir resolves a configured directory relative to the config file (unless
// it is absolute).
func (c *Config) resolveDir(d string) string {
	if d == "" || filepath.IsAbs(d) || c.dir == "" {
		return d
	}
	return filepath.Join(c.dir, d)
}

// DataDirAbs is data_dir resolved relative to the config file.
func (c *Config) DataDirAbs() string { return c.resolveDir(c.DataDir) }

// DatabasePathAbs is database.path resolved relative to VARHUB_HOME (the config
// dir) for sqlite; an absolute path or a postgres DSN is returned unchanged.
func (c *Config) DatabasePathAbs() string {
	if c.Database.Backend != "sqlite" {
		return c.Database.Path
	}
	return c.resolveDir(c.Database.Path)
}

// CacheDirAbs is the source-file cache location resolved relative to the config
// file. Defaults to "<data_dir>/cache" (or "cache") when cache_dir is unset.
//
// An s3:// cache_dir is returned verbatim: it is a locator, not a path, so
// resolving it against the config dir would be meaningless and filepath.Clean
// would eat the scheme's second slash.
func (c *Config) CacheDirAbs() string {
	cd := c.CacheDir
	if objstore.IsObject(cd) {
		return cd
	}
	if cd == "" {
		if c.DataDir != "" {
			cd = filepath.Join(c.DataDir, "cache")
		} else {
			cd = "cache"
		}
	}
	return c.resolveDir(cd)
}

// ResolveSourcePath returns the on-disk path to a source's data file. A LocalPath
// (absolute, or relative to data_dir) wins — the file is used exactly. Otherwise
// the file is cached under cache_dir keyed by name/version. Environment variables
// in a localpath (`$VAR` / `${VAR}`, incl. $VARHUB_HOME) are expanded here at
// resolve time, so the raw value stays in the fragment file.
func (c *Config) ResolveSourcePath(s Source) string {
	if s.LocalPath != "" {
		return c.resolveLocal(s.LocalPath)
	}

	if s.Build != nil {
		return objstore.Join(c.CacheDirAbs(), s.Name, s.Version, s.BuildOutput())
	}
	base := path.Base(s.URL)
	if base == "" || base == "." || base == "/" {
		base = s.Name + ".gz"
	}
	return objstore.Join(c.CacheDirAbs(), s.Name, s.Version, base)
}

// ResolveGTFIndexPath is the canonical on-disk location for a GTF source's
// bgzipped + tabix-indexed form, under cache_dir keyed by name/version. varhub
// builds it once (at download, or lazily on first annotate) so the gene model can
// be queried by position instead of loaded whole into memory.
func (c *Config) ResolveGTFIndexPath(s Source) string {
	if s.locations != nil && s.locations.GTFIndex != "" {
		return s.locations.GTFIndex
	}
	return c.GTFIndexTarget(s)
}

// GTFIndexTarget is where a GTF's converted copy should be built, by the
// cache_dir convention and ignoring any overlay.
func (c *Config) GTFIndexTarget(s Source) string {
	return objstore.Join(c.CacheDirAbs(), s.Name, s.Version, s.Name+".gtf.gz")
}

// resolveLocalIndex returns the resolved on-disk index path when LocalPathIndex is
// set, else "" (meaning the index is expected alongside the data file).
func (c *Config) resolveLocalIndex(s Source) string {
	if s.LocalPathIndex == "" {
		return ""
	}
	return c.resolveLocal(s.LocalPathIndex)
}

// resolveLocal expands environment variables in a local path and joins a relative
// result under data_dir (an absolute path is used as-is).
func (c *Config) resolveLocal(p string) string {
	p = os.ExpandEnv(p)
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.DataDirAbs(), p)
}

// ResolveToolImage returns the cached on-disk path for a tool's container image
// (keyed by name/version), or "" if the tool has no image. A registry ref pulls to
// <name>.sif; a .sif URL keeps its basename.
func (c *Config) ResolveToolImage(t Tool) string {
	if t.Image == "" {
		return ""
	}
	base := t.Name + ".sif"
	if !t.ImageIsRef() {
		if b := path.Base(t.Image); b != "" && b != "." && b != "/" {
			base = b
		}
	}
	return filepath.Join(c.toolBase(), "images", t.Name, t.Version, base)
}

// toolBase is the local directory holding tool images and tool data.
//
// Tools cannot live in an object store. A container binds its data dir and
// apptainer runs a .sif from a real path, so composing either from an s3://
// cache produced "s3:/bucket/..." — filepath.Join collapses the scheme — which
// is neither a locator nor a path, and failed far from the cause.
func (c *Config) toolBase() string {
	if c.ToolDir != "" {
		return c.resolveDir(c.ToolDir)
	}
	if cd := c.CacheDirAbs(); !objstore.IsRemote(cd) {
		return cd
	}
	// The cache is remote, so fall back to local persistent storage. data_dir is
	// what a deployment already sets aside for exactly this.
	if c.DataDir != "" {
		return c.DataDirAbs()
	}
	return "tools-local"
}

// ToolImageObject is where a built image is kept durably, or "" when the cache
// is a local directory and the local copy is already the durable one.
//
// A SIF is the one part of a tool that belongs in an object store without
// reservation: a single immutable blob, written once and read sequentially.
// The tool's *data* is the opposite — millions of small files opened by
// coordinate — which is why only this has a remote home.
func (c *Config) ToolImageObject(t Tool) string {
	if t.Image == "" {
		return ""
	}
	cd := c.CacheDirAbs()
	if !objstore.IsRemote(cd) {
		return ""
	}
	return objstore.Join(cd, "images", t.Name, t.Version, filepath.Base(c.ResolveToolImage(t)))
}

// ResolveToolData returns the tool's persistent data dir (<name>/<version>, matching
// the image cache layout), where setup installs the tool's data; bound into container
// steps as {datadir}. Uses name/version rather than the ":" ID so the version never
// appears as a ":" in a filesystem path.
func (c *Config) ResolveToolData(t Tool) string {
	return filepath.Join(c.toolBase(), "tools", t.Name, t.Version)
}

// MustExist returns a helpful error if the config file is missing.
func MustExist(path string) error {
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("config file %s not found (run `varhub init`)", path)
	}
	return nil
}

// ReadFragment decodes a single snapshot fragment file (one or more sources/tools)
// and populates the derived Annotations; no validation (used for editing).
func ReadFragment(path string) (*Snapshot, error) {
	snap, err := decodeFragment(path)
	if err != nil {
		return nil, err
	}
	snap.normalize()
	return snap, nil
}

// ReadConfigFile decodes config.toml WITHOUT expanding $VARHUB_HOME, so the raw
// values (e.g. "$VARHUB_HOME/data") round-trip when edited and rewritten. Use Load
// for a resolved, validated config to run against.
func ReadConfigFile(path string) (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	return &c, nil
}

// WriteTOML encodes v to path (overwriting), reflowing arrays with long elements
// (e.g. full URLs) onto one element per line. Comments are not preserved.
func WriteTOML(path string, v any) error {
	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(v); err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(reflowLongArrays(b.String())), 0o644)
}

// MarshalSnapshot encodes a snapshot fragment to TOML, dropping zero-valued
// tab-only columns (ref_col/alt_col) that BurntSushi's omitempty doesn't omit (so
// vcf/bed stubs aren't littered with `ref_col = 0`), and reflowing arrays with long
// elements (e.g. full URLs) onto one element per line for readability.
func MarshalSnapshot(snap *Snapshot) (string, error) {
	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(snap); err != nil {
		return "", err
	}
	lines := strings.Split(b.String(), "\n")
	kept := lines[:0]
	for _, ln := range lines {
		if t := strings.TrimSpace(ln); t == "ref_col = 0" || t == "alt_col = 0" {
			continue
		}
		kept = append(kept, ln)
	}
	return reflowLongArrays(strings.Join(kept, "\n")), nil
}

// WriteFragment writes a snapshot fragment file (see MarshalSnapshot for the
// formatting rules).
func WriteFragment(path string, snap *Snapshot) error {
	s, err := MarshalSnapshot(snap)
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, []byte(s), 0o644)
}

// arrayElemWrapWidth is the element length (including quotes) above which an inline
// TOML array is reflowed to one element per line. Chosen to keep short tokens
// (chrom names, "python3") inline while wrapping long values like full URLs and
// shell-command `run` steps.
const arrayElemWrapWidth = 40

// reflowLongArrays rewrites single-line TOML array assignments (`key = [a, b, …]`)
// whose elements are long onto multiple lines — one element per line, trailing
// comma — leaving short arrays inline. BurntSushi always emits arrays on one line,
// so this is a post-encode pass over the rendered text.
func reflowLongArrays(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, ln := range lines {
		if expanded, ok := expandArrayLine(ln); ok {
			out = append(out, expanded...)
		} else {
			out = append(out, ln)
		}
	}
	return strings.Join(out, "\n")
}

var inlineArrayRe = regexp.MustCompile(`^(\s*)([A-Za-z0-9_-]+)\s*=\s*\[(.*)\]\s*$`)

// expandArrayLine reflows one `key = [ … ]` line to multi-line form when any
// element is longer than arrayElemWrapWidth. Returns (nil, false) for non-array
// lines, empty arrays, and arrays of only short elements (left untouched).
func expandArrayLine(ln string) ([]string, bool) {
	m := inlineArrayRe.FindStringSubmatch(ln)
	if m == nil {
		return nil, false
	}
	indent, key, inner := m[1], m[2], m[3]
	elems := splitTOMLArray(inner)
	if len(elems) == 0 {
		return nil, false
	}
	long := false
	for _, e := range elems {
		if len(e) > arrayElemWrapWidth {
			long = true
			break
		}
	}
	if !long {
		return nil, false
	}
	out := make([]string, 0, len(elems)+2)
	out = append(out, indent+key+" = [")
	for _, e := range elems {
		out = append(out, indent+"  "+e+",")
	}
	out = append(out, indent+"]")
	return out, true
}

// splitTOMLArray splits an inline array's inner text into element strings on
// top-level commas, respecting double-quoted strings (and their backslash escapes)
// and nested brackets. A trailing comma yields no empty element.
func splitTOMLArray(inner string) []string {
	var elems []string
	var cur strings.Builder
	inStr, esc, depth := false, false, 0
	flush := func() {
		if e := strings.TrimSpace(cur.String()); e != "" {
			elems = append(elems, e)
		}
		cur.Reset()
	}
	for _, r := range inner {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case r == '\\' && inStr:
			cur.WriteRune(r)
			esc = true
		case r == '"':
			inStr = !inStr
			cur.WriteRune(r)
		case r == '[' && !inStr:
			depth++
			cur.WriteRune(r)
		case r == ']' && !inStr:
			depth--
			cur.WriteRune(r)
		case r == ',' && !inStr && depth == 0:
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return elems
}
