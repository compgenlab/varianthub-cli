// Package annotate builds an hts annotation pipeline from a snapshot and streams
// an input VCF to a fully-annotated output VCF. Unlike the cache path (which is
// sample-blind and stores loci→values), this is a local file transformation:
// it preserves the input's samples so the sample-derived tools (dosage/VAF/…)
// can run.
package annotate

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/compgenlab/cghts/vcf"
	htsann "github.com/compgenlab/cghts/vcf/annotate"

	"github.com/compgenlab/cganno/internal/config"
	"github.com/compgenlab/cganno/internal/model"
	"github.com/compgenlab/cganno/internal/software"
	"github.com/compgenlab/cganno/internal/store"
	"github.com/compgenlab/cganno/internal/tool"
	ivcf "github.com/compgenlab/cganno/internal/vcf"
)

// AnnotateVCFSnapshot annotates inPath → outPath for a whole snapshot. This is the
// bulk-VCF path: it runs any external tools (VEP/ANNOVAR) over the WHOLE input —
// each producing an indexed output file — and annotates directly from those files,
// then runs the hts pipeline over all sources + builtins. It deliberately does NOT
// use the per-locus tool cache: for a whole VCF that round-trip (write every line
// to SQLite, read it all back to rebuild an index) is pure overhead, since the
// tool's own output is already indexed (tool.Run/ensureIndex). The per-locus cache
// is reserved for individual-locus queries (see RunToolsForLoci).
//
// When toolCacheDir is non-empty it is used as a file-based, per-input tool-output
// cache: a matching prior output is reused (skipping the tool) and fresh output is
// saved there for later runs (see runTools / lookupToolCache / storeToolCache).
func AnnotateVCFSnapshot(ctx context.Context, cfg *config.Config, snap *config.Snapshot, inPath, outPath string, threads int, keepTemp bool, toolCacheDir string) error {
	lg := LoggerFrom(ctx)

	// The "effective" snapshot is the one the pipeline runs over: with tool sources
	// resolved to their generated output files. With no tools it is the snapshot itself.
	eff := snap
	toolSources := snap.ToolSources()
	if len(toolSources) > 0 {
		workdir, err := os.MkdirTemp("", "cganno-tools-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(workdir)

		lg.Logf("resolving %d external tool(s) for the input (bulk VCF; per-locus cache skipped)…", len(toolSources))
		// Pass a nil store so runTools takes the direct branch: run the tool over the
		// whole input and use its indexed output as-is (no per-locus cache/rebuild).
		toolSrcs, err := runTools(ctx, cfg, toolSources, nil, inPath, workdir, snap.Reference, snap.Assembly, toolCacheDir)
		if err != nil {
			return err
		}
		lg.Logf("tool(s) ready")

		// Replace the tool-type sources with their generated output sources, so the
		// pipeline reads the produced files (by name) rather than the tool fragments.
		aug := *snap
		aug.Sources = append(nonToolSources(snap.Sources), toolSrcs...)
		eff = &aug
	}

	// threads > 1 fans out: one annotation pass per source into a temp part VCF (up
	// to `threads` concurrently), then merges the parts. threads <= 1 keeps the
	// single sequential pass.
	if threads > 1 {
		return annotateVCFFanOut(ctx, cfg, eff, inPath, outPath, threads, keepTemp)
	}
	lg.Logf("annotating %d annotation(s) in a single pass (-t 1; pass -t N to parallelize across sources)", len(eff.Annotations))
	p, err := BuildPipeline(cfg, eff, cfg.ResolveSourceFiles)
	if err != nil {
		return err
	}
	return AnnotateVCF(ctx, p, inPath, outPath, "")
}

// nonToolSources returns a copy of srcs with type="tool" sources removed.
func nonToolSources(srcs []config.Source) []config.Source {
	out := make([]config.Source, 0, len(srcs))
	for _, s := range srcs {
		if !s.IsTool() {
			out = append(out, s)
		}
	}
	return out
}

// runTools runs each tool over inPath, returning each tool's indexed output
// projected as a local Source (via Tool.AsSource). When st != nil the per-locus
// tool cache is used (runToolCached) so each tool runs only on loci it hasn't seen;
// when nil the tool runs over the whole input. `ref` is the snapshot's reference
// FASTA and `assembly` scopes the tool output cache. Each tool's assets are staged
// from its own version dir. Shared by AnnotateVCFSnapshot (the -o path) and
// RunToolsForLoci (the cache/locus path).
//
// toolCacheDir (bulk-VCF path only; "" disables it) is a directory used as a
// file-based, per-input tool-output cache: before running a tool, a previously
// saved output whose recorded input matches inPath (same path+size+mtime), tool,
// and assembly is reused as-is; otherwise the tool runs and its output is saved
// there for next time. Reuse needs neither the tool nor its container engine.
func runTools(ctx context.Context, cfg *config.Config, toolSources []config.Source, st store.Store, inPath, workdir, ref, assembly, toolCacheDir string) ([]config.Source, error) {
	lg := LoggerFrom(ctx)
	var out []config.Source
	for _, src := range toolSources {
		t := src.AsTool() // execution view of the type="tool" source

		// Reuse a cached output for this exact input before any software check, so a
		// cache hit needs neither the tool nor its container engine.
		if st == nil && toolCacheDir != "" {
			if cached, ok, err := lookupToolCache(toolCacheDir, t, assembly, inPath); err != nil {
				return nil, err
			} else if ok {
				lg.Logf("tool %s: reusing cached output for this input (%s)", t.ID(), cached)
				out = append(out, t.AsSource(cached))
				continue
			}
		}

		if err := software.Check(t.ID(), t.RequiredSoftware()); err != nil {
			return nil, err
		}
		outFile := filepath.Join(workdir, t.OutputName())
		p := tool.Params{
			Image: cfg.ResolveToolImage(t), Ref: ref,
			Datadir: cfg.ResolveToolData(t), AssetDir: cfg.SourceDir(src.Name, src.Version),
		}
		if st != nil {
			toolWork, err := os.MkdirTemp(workdir, "tool-")
			if err != nil {
				return nil, err
			}
			if err := runToolCached(ctx, st, t, assembly, p, inPath, outFile, toolWork); err != nil {
				return nil, err
			}
		} else {
			lg.Logf("tool %s: running over the full input (no cache)", t.ID())
			input := inPath // VCF tools take the input VCF as-is (samples preserved)
			if !isVCFInput(t.InputFormat) {
				loci, err := ivcf.ReadFile(inPath)
				if err != nil {
					return nil, err
				}
				input = filepath.Join(workdir, "tool.in")
				if err := writeTemplatedVariants(input, loci, t.InputFormat); err != nil {
					return nil, err
				}
			}
			p.Input, p.Output, p.Workdir = input, outFile, workdir
			if err := tool.Run(ctx, t, p); err != nil {
				return nil, err
			}
			if toolCacheDir != "" {
				if err := storeToolCache(toolCacheDir, src, assembly, inPath, outFile); err != nil {
					return nil, err
				}
				lg.Logf("tool %s: saved output to cache dir %s", t.ID(), toolCacheDir)
			}
		}
		out = append(out, t.AsSource(outFile))
	}
	return out, nil
}

// RunToolsForLoci runs the given tools over loci (materialized as a temp VCF) and
// returns each tool's indexed output as a local Source — the entry point for the
// cache/locus path, which overlays those sources onto the engine. st (the cache
// store) drives the per-locus tool cache, so a tool runs only on novel loci.
func RunToolsForLoci(ctx context.Context, cfg *config.Config, toolSources []config.Source, st store.Store, loci []model.Locus, workdir, ref, assembly string) ([]config.Source, error) {
	if len(toolSources) == 0 {
		return nil, nil
	}
	lociVCF := filepath.Join(workdir, "loci.vcf")
	if err := ivcf.WriteLoci(lociVCF, loci); err != nil {
		return nil, err
	}
	// The locus path uses the per-locus SQLite cache (st), not the file cache.
	return runTools(ctx, cfg, toolSources, st, lociVCF, workdir, ref, assembly, "")
}

// BuildPipeline assembles the hts pipeline for a snapshot: one source annotator
// per declared annotation, plus the enabled self-contained builtins. resolve maps
// a source to its concrete file(s) (one, or one per chromosome for multi-file).
//
// GTF sources are the exception: all selected fields of one gtf source share a
// single annotator (so the GTF gene model is parsed once, not per field). Those
// grouped annotators are added after the per-annotation ones, in first-seen
// source order — fine since GTF output is independent of other annotators.
func BuildPipeline(cfg *config.Config, snap *config.Snapshot, resolve func(config.Source) []config.SourceFile) (*htsann.Pipeline, error) {
	p := htsann.NewPipeline()

	// Bucket each data source's annotations so the source is annotated by a single
	// grouped annotator (one reader, one query per record) via SourceAnnotators,
	// instead of one reader per annotation. GTF keeps its long-standing deferral to
	// the end; every other source emits at its first appearance, interleaved with
	// builtins, so INFO/header ordering is unchanged for the common case where a
	// source's annotations are contiguous.
	srcGroups := map[string][]config.Annotation{}
	gtfGroups := map[string][]config.Annotation{}
	var gtfOrder []string
	for _, a := range snap.Annotations {
		if config.IsBuiltin(a.Source) {
			continue
		}
		src := snap.SourceByName(a.Source)
		if src == nil {
			return nil, fmt.Errorf("annotation %q: unknown source %q", a.Name, a.Source)
		}
		if src.IsGTFSource() {
			if _, seen := gtfGroups[a.Source]; !seen {
				gtfOrder = append(gtfOrder, a.Source)
			}
			gtfGroups[a.Source] = append(gtfGroups[a.Source], a)
			continue
		}
		srcGroups[a.Source] = append(srcGroups[a.Source], a)
	}

	emitted := map[string]bool{}
	for _, a := range snap.Annotations {
		if config.IsBuiltin(a.Source) {
			if a.Source == "vardist" {
				p.AddStream(htsann.NewVariantDistance()) // streaming (look-ahead)
				continue
			}
			ann, err := builtinAnnotator(a)
			if err != nil {
				return nil, err
			}
			p.Add(ann)
			continue
		}
		src := snap.SourceByName(a.Source)
		if src.IsGTFSource() || emitted[a.Source] {
			continue // GTF handled below; other sources emitted once, at first appearance
		}
		emitted[a.Source] = true
		anns, err := SourceAnnotators(cfg, *src, srcGroups[a.Source], resolve(*src))
		if err != nil {
			return nil, err
		}
		for _, ann := range anns {
			p.Add(ann)
		}
	}

	for _, name := range gtfOrder {
		src := snap.SourceByName(name)
		anns, err := SourceAnnotators(cfg, *src, gtfGroups[name], resolve(*src))
		if err != nil {
			return nil, err
		}
		for _, ann := range anns {
			p.Add(ann)
		}
	}
	return p, nil
}

// AnnotatorFor builds the hts annotator for one (source, annotation) pair over the
// source's resolved file(s). A single-file source yields one annotator; a multi-file
// source (per-chrom, or an explicit Files union) yields a wrapper that runs every
// file's annotator per record — a record whose contig isn't in a given file is a
// no-op (hts), so the file matching by position+ref/alt provides the value.
func AnnotatorFor(src config.Source, a config.Annotation, files []config.SourceFile) (htsann.Annotator, error) {
	if src.IsPerAlt() {
		// A per-alt file set (bigWig a/c/g/t): route each record to the file matching
		// its alt base, so the value is allele-specific.
		d := &altDispatch{anns: map[string]htsann.Annotator{}}
		for _, f := range files {
			ann, err := buildSingle(src, a, f.Path)
			if err != nil {
				d.Close()
				return nil, err
			}
			d.order = append(d.order, ann)
			d.anns[strings.ToLower(f.Alt)] = ann
		}
		return d, nil
	}
	if len(files) == 1 {
		return buildSingle(src, a, files[0].Path)
	}
	m := &multiFile{anns: make([]htsann.Annotator, 0, len(files))}
	for _, f := range files {
		ann, err := buildSingle(src, a, f.Path)
		if err != nil {
			m.Close()
			return nil, err
		}
		m.anns = append(m.anns, ann)
	}
	return m, nil
}

// SourceAnnotators builds the hts annotators for one source over the given
// annotations and resolved file(s). Most sources produce one annotator per
// annotation (via AnnotatorFor); a gtf source produces a single grouped annotator
// for all of them, so the gene model is parsed once (mirrors BuildPipeline's GTF
// handling). The caller owns Close() on each returned annotator. This is the
// shared entry point for the cache/locus path (see annotator/overlay).
func SourceAnnotators(cfg *config.Config, src config.Source, anns []config.Annotation, files []config.SourceFile) ([]htsann.Annotator, error) {
	if src.IsGeneList() {
		ann, err := buildGeneList(cfg, src, anns)
		if err != nil {
			return nil, err
		}
		return []htsann.Annotator{ann}, nil
	}
	if src.IsGTFSource() {
		ann, err := buildGTF(cfg, src, anns, files)
		if err != nil {
			return nil, err
		}
		return []htsann.Annotator{ann}, nil
	}
	// Tabix-backed sources (vcf/tab/bed) collapse all their annotations into one
	// grouped annotator: a single reader and a single query per record for the whole
	// source, instead of one reader/query per annotation.
	if isGroupable(src) {
		ann, err := buildSourceGroup(src, anns, files)
		if err != nil {
			return nil, err
		}
		return []htsann.Annotator{ann}, nil
	}
	// bigwig/bigbed (incl. per-alt sets): one annotator per annotation.
	out := make([]htsann.Annotator, 0, len(anns))
	for _, a := range anns {
		ann, err := AnnotatorFor(src, a, files)
		if err != nil {
			for _, x := range out {
				x.Close()
			}
			return nil, err
		}
		out = append(out, ann)
	}
	return out, nil
}

// isGroupable reports whether a source's annotations can share one grouped
// annotator (one reader, one query per record): the tabix-backed formats, excluding
// per-alt file sets (bigWig a/c/g/t), which route by alt allele instead.
func isGroupable(src config.Source) bool {
	if src.IsPerAlt() {
		return false
	}
	switch src.Format {
	case "vcf", "", "bed", "tab":
		return true
	}
	return false
}

// buildSourceGroup builds a single grouped annotator for a tabix-backed source over
// its resolved file(s); a multi-file source is wrapped in multiFile (one grouped
// annotator per file), mirroring AnnotatorFor's multi-file handling.
func buildSourceGroup(src config.Source, anns []config.Annotation, files []config.SourceFile) (htsann.Annotator, error) {
	if len(files) == 1 {
		return buildGroupedSingle(src, anns, files[0].Path)
	}
	m := &multiFile{anns: make([]htsann.Annotator, 0, len(files))}
	for _, f := range files {
		ann, err := buildGroupedSingle(src, anns, f.Path)
		if err != nil {
			m.Close()
			return nil, err
		}
		m.anns = append(m.anns, ann)
	}
	return m, nil
}

// buildGroupedSingle maps a source's annotations onto one hts grouped annotator for a
// single file. The per-field mapping matches buildSingle; only the shape differs (N
// fields on one reader instead of one annotator per field).
func buildGroupedSingle(src config.Source, anns []config.Annotation, path string) (htsann.Annotator, error) {
	switch src.Format {
	case "vcf", "":
		fields := make([]htsann.VcfFieldOptions, 0, len(anns))
		for _, a := range anns {
			field := a.FieldName() // INFO id, or "@ID" to copy the source ID
			if a.IsFlag() {
				field = "" // presence flag
			}
			fields = append(fields, htsann.VcfFieldOptions{
				Name:   a.Name,
				Field:  field,
				Exact:  a.Match != "position", // default exact; "position" = position-only
				Unique: a.Unique,
			})
		}
		return htsann.NewVcfAnnotationGroup(htsann.VcfGroupOptions{
			Filename: path, AutoConvert: true, Fields: fields,
		})
	case "bed":
		fields := make([]htsann.TabixFieldOptions, 0, len(anns))
		for _, a := range anns {
			col, colName := bedColumn(a.FieldName())
			fields = append(fields, htsann.TabixFieldOptions{
				Name: a.Name, Col: col, ColName: colName, IsNumber: a.IsNumeric(),
			})
		}
		return htsann.NewTabixAnnotationGroup(htsann.TabixGroupOptions{
			Filename: path, AutoConvert: true, Fields: fields,
		})
	case "tab":
		fields := make([]htsann.TabixFieldOptions, 0, len(anns))
		for _, a := range anns {
			col, colName := tabColumn(a.FieldName())
			fields = append(fields, htsann.TabixFieldOptions{
				Name: a.Name, Col: col, ColName: colName, IsNumber: a.IsNumeric(),
			})
		}
		return htsann.NewTabixAnnotationGroup(htsann.TabixGroupOptions{
			Filename: path, AutoConvert: true,
			AltCol: src.AltCol, RefCol: src.RefCol, Fields: fields,
		})
	default:
		return nil, fmt.Errorf("source %q: cannot group format %q", src.ID(), src.Format)
	}
}

// multiFile runs every file's annotator for a multi-file source. The shared INFO
// header is declared once (via the first); overlapping files are not deduplicated
// (a later file's match overwrites an earlier one).
type multiFile struct{ anns []htsann.Annotator }

func (m *multiFile) SetupHeader(h *vcf.VcfHeader) error {
	if len(m.anns) == 0 {
		return nil
	}
	return m.anns[0].SetupHeader(h)
}

func (m *multiFile) Annotate(rec *vcf.VcfRecord) error {
	for _, ann := range m.anns {
		if err := ann.Annotate(rec); err != nil {
			return err
		}
	}
	return nil
}

func (m *multiFile) Close() error {
	var err error
	for _, ann := range m.anns {
		if e := ann.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// altDispatch routes a record to the annotator for its alt base (a per-alt bigWig
// set: a/c/g/t). A record whose alt isn't a single base with a file (e.g. an indel)
// gets no value. The shared header is declared once (via the first file).
type altDispatch struct {
	anns  map[string]htsann.Annotator // lowercased alt base -> annotator
	order []htsann.Annotator          // stable order for header/close
}

func (d *altDispatch) SetupHeader(h *vcf.VcfHeader) error {
	if len(d.order) == 0 {
		return nil
	}
	return d.order[0].SetupHeader(h)
}

func (d *altDispatch) Annotate(rec *vcf.VcfRecord) error {
	alts := rec.Alt()
	if len(alts) == 0 {
		return nil
	}
	ann, ok := d.anns[strings.ToLower(alts[0])]
	if !ok {
		return nil // no file for this alt (multi-base/indel) → no value
	}
	return ann.Annotate(rec)
}

func (d *altDispatch) Close() error {
	var err error
	for _, ann := range d.order {
		if e := ann.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

// buildSingle maps cganno's config knobs onto hts annotate options for one file.
// AutoConvert is on for every annotator: hts builds a contig converter from the
// source file's own ref names, so input/source chrom naming (Ensembl "1" / UCSC
// "chr1" / NCBI "NC_000001.11") is matched automatically.
func buildSingle(src config.Source, a config.Annotation, path string) (htsann.Annotator, error) {
	switch src.Format {
	case "vcf", "":
		field := a.FieldName() // INFO id, or "@ID" to copy the source ID
		if a.IsFlag() {
			field = "" // presence flag
		}
		return htsann.NewVcfAnnotation(htsann.VcfOptions{
			Name:        a.Name,
			Field:       field,
			Filename:    path,
			Exact:       a.Match != "position", // default exact; "position" = position-only
			Unique:      a.Unique,
			AutoConvert: true,
		})
	case "bed":
		opts := htsann.TabixOptions{Name: a.Name, Filename: path, IsNumber: a.IsNumeric(), AutoConvert: true}
		setBedColumn(&opts, a.FieldName())
		return htsann.NewTabixAnnotator(opts)
	case "tab":
		opts := htsann.TabixOptions{
			Name: a.Name, Filename: path, IsNumber: a.IsNumeric(),
			AltCol: src.AltCol, RefCol: src.RefCol, AutoConvert: true,
		}
		setTabColumn(&opts, a.FieldName())
		return htsann.NewTabixAnnotator(opts)
	case "bigwig":
		// One numeric value per base (no ref/alt); allele specificity, when needed,
		// comes from a per-alt {alt} file set routed by altDispatch.
		return htsann.NewBigWigAnnotator(htsann.BigWigOptions{
			Name: a.Name, Filename: path, AutoConvert: true,
		})
	case "bigbed":
		col, err := bigBedColumn(a.FieldName())
		if err != nil {
			return nil, fmt.Errorf("source %q: %w", src.ID(), err)
		}
		return htsann.NewBigBedAnnotator(htsann.BigBedOptions{
			Name: a.Name, Filename: path, Col: col, IsNumber: a.IsNumeric(), AutoConvert: true,
		})
	default:
		return nil, fmt.Errorf("source %q: unsupported format %q", src.ID(), src.Format)
	}
}

// bigBedColumn resolves a bigBed annotation field to a 1-based BED column: a
// number, or "name" (4) / "score" (5). autoSql column names are not yet supported.
func bigBedColumn(field string) (int, error) {
	switch strings.ToLower(field) {
	case "name", "":
		return 4, nil
	case "score":
		return 5, nil
	}
	if n, err := strconv.Atoi(field); err == nil {
		return n, nil
	}
	return 0, fmt.Errorf("bigbed field %q: use a 1-based column number, \"name\", or \"score\"", field)
}

// setTabColumn sets the value column for a tab source (1-based integer or a
// header column name).
func setTabColumn(o *htsann.TabixOptions, field string) {
	if n, err := strconv.Atoi(field); err == nil {
		o.Col = n
	} else {
		o.ColName = field
	}
}

// bedColumn / tabColumn resolve a field to a (col, colName) pair for a grouped
// annotator, reusing the single-annotator column logic (setBedColumn/setTabColumn).
func bedColumn(field string) (int, string) {
	var o htsann.TabixOptions
	setBedColumn(&o, field)
	return o.Col, o.ColName
}

func tabColumn(field string) (int, string) {
	var o htsann.TabixOptions
	setTabColumn(&o, field)
	return o.Col, o.ColName
}

// builtinAnnotator builds the hts annotator for a builtin-sourced annotation
// (except vardist, a stream wrapper handled in BuildPipeline). Parameterized
// builtins read a.Args.
// VariantOnlyBuiltin reports whether a builtin computes from chrom/pos/ref/alt
// alone, so it can run on the cache/locus path (not just the `-o` VCF pipeline).
// The rest can't: dosage/vaf/minor_strand/fisher_sb/copy_logratio read per-sample
// FORMAT fields, and vardist needs the neighboring variants in the stream.
func VariantOnlyBuiltin(name string) bool {
	switch name {
	case "auto_id", "indel", "tstv", "tags":
		return true
	}
	return false
}

// BuiltinAnnotator builds the hts annotator for a builtin annotation, keyed by
// its builtin name. Exported for the cache/locus overlay path (the streaming
// pipeline reaches the same annotators via BuildPipeline).
func BuiltinAnnotator(a config.Annotation) (htsann.Annotator, error) {
	if a.Source == "" {
		a.Source = a.Builtin
	}
	return builtinAnnotator(a)
}

func builtinAnnotator(a config.Annotation) (htsann.Annotator, error) {
	switch a.Source {
	case "auto_id":
		return htsann.NewAutoID(), nil
	case "indel":
		return htsann.NewIndel(), nil
	case "tstv":
		return htsann.NewTsTv(), nil
	case "dosage":
		return htsann.NewDosage(), nil
	case "vaf":
		return htsann.NewVAF(), nil
	case "minor_strand":
		return htsann.NewMinorStrand(), nil
	case "fisher_sb":
		return htsann.NewFisherSB(), nil
	case "tags":
		if i := strings.IndexByte(a.Args, ':'); i >= 0 {
			return htsann.NewConstantTag(a.Args[:i], a.Args[i+1:]), nil
		}
		return htsann.NewConstantFlag(a.Args), nil
	case "copy_logratio":
		return parseCopyLogRatio(a.Args)
	default:
		return nil, fmt.Errorf("unknown builtin %q", a.Source)
	}
}

// progressEvery is how many records between running-count progress lines under -v.
const progressEvery = 100_000

// AnnotateVCF streams inPath through the pipeline to outPath ("" or "-" =
// stdout). The input's header and samples are preserved. When ctx carries a
// verbose Logger, it emits a running record count (prefixed by label, blank for a
// single pass) and a final total.
func AnnotateVCF(ctx context.Context, p *htsann.Pipeline, inPath, outPath, label string) error {
	lg := LoggerFrom(ctx)
	tag := ""
	if label != "" {
		tag = "[" + label + "] "
	}

	reader, err := vcf.NewVcfFile(inPath)
	if err != nil {
		return fmt.Errorf("open %s: %w", inPath, err)
	}
	defer reader.Close()

	header, err := reader.Header()
	if err != nil {
		return err
	}
	if err := p.SetupHeaders(header); err != nil {
		return err
	}

	var writer *vcf.VcfWriter
	var closeFile func() error
	if outPath == "" || outPath == "-" {
		writer = vcf.NewVcfWriter(os.Stdout)
	} else {
		w, err := vcf.OpenVcfWriter(outPath)
		if err != nil {
			return err
		}
		writer, closeFile = w, w.Close
	}
	if err := writer.WriteHeader(header); err != nil {
		return err
	}

	n := 0
	next := p.Build(reader.NextRecord)
	for {
		rec, err := next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := writer.WriteRecord(rec); err != nil {
			return err
		}
		n++
		if n%progressEvery == 0 {
			lg.Logf("%sannotated %s records…", tag, commaCount(n))
		}
	}
	lg.Logf("%sdone: %s records annotated", tag, commaCount(n))
	if err := p.Close(); err != nil {
		return err
	}
	if closeFile != nil {
		return closeFile()
	}
	return writer.Close()
}

func setBedColumn(o *htsann.TabixOptions, field string) {
	switch strings.ToLower(field) {
	case "name", "":
		o.Col = 4
	case "score":
		o.Col = 5
	default:
		if n, err := strconv.Atoi(field); err == nil {
			o.Col = n
		} else {
			o.ColName = field
		}
	}
}

// parseCopyLogRatio parses SOMATIC:GERMLINE[:somatic-total:germline-total].
func parseCopyLogRatio(arg string) (*htsann.CopyNumberLogRatio, error) {
	spl := strings.Split(arg, ":")
	switch len(spl) {
	case 2:
		return htsann.NewCopyLogRatio(spl[0], spl[1], -1, -1), nil
	case 4:
		st, err := strconv.ParseInt(spl[2], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("copy_logratio somatic-total: %w", err)
		}
		gt, err := strconv.ParseInt(spl[3], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("copy_logratio germline-total: %w", err)
		}
		return htsann.NewCopyLogRatio(spl[0], spl[1], st, gt), nil
	default:
		return nil, fmt.Errorf("copy_logratio %q: want SOMATIC:GERMLINE[:st:gt]", arg)
	}
}
