package annotate

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/compgenlab/cghts/htsio/tabix"

	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/model"
	"github.com/compgenlab/varianthub-cli/internal/store"
	"github.com/compgenlab/varianthub-cli/internal/tool"
	ivcf "github.com/compgenlab/varianthub-cli/internal/vcf"
)

// runToolCached runs tool t over inPath, but uses st as a per-locus output cache:
// only loci the tool has not seen before are sent to it. outFile is then rebuilt
// from the cache so it covers ALL of inPath's loci, letting the downstream tabix
// annotator consume it exactly as if the tool had run over the whole input.
//
// p carries the run params (image/ref/datadir); p.Input/p.Output/p.Workdir are
// set here. workdir is a scratch dir private to this tool invocation.
//
// Output lines are mapped back to their input locus by the tool output's
// chrom/pos + ref/alt columns — this assumes the tool preserves the input
// alleles (true for VEP `--vcf` and the post-processing pipeline). To stay robust
// to allele normalization, lines are retrieved by position at rebuild time and
// the tabix annotator re-matches ref/alt.
func runToolCached(ctx context.Context, st store.Store, t config.Tool, assembly string, p tool.Params, inPath, outFile, workdir string) error {
	loci, err := ivcf.ReadFile(inPath)
	if err != nil {
		return err
	}
	lg := LoggerFrom(ctx)
	uid := toolUID(t, assembly)
	t0 := time.Now()
	done, err := st.ToolProcessed(ctx, uid, loci)
	if err != nil {
		return err
	}
	lg.Logf("tool %s: checked cache for %s loci [%s]", t.ID(), commaCount(len(loci)), took(t0))
	var novel []model.Locus
	novelKeys := make(map[string]bool)
	for _, l := range loci {
		k := l.Key()
		if !done[k] && !novelKeys[k] {
			novel = append(novel, l)
			novelKeys[k] = true
		}
	}

	if len(novel) == 0 {
		lg.Logf("tool %s: all %s loci already cached", t.ID(), commaCount(len(loci)))
	} else {
		lg.Logf("tool %s: %s of %s loci are novel — running the tool on those",
			t.ID(), commaCount(len(novel)), commaCount(len(loci)))
	}

	if len(novel) > 0 {
		reduced := filepath.Join(workdir, "novel.in")
		if err := writeToolInput(inPath, reduced, t.InputFormat, novel, novelKeys); err != nil {
			return err
		}
		raw := filepath.Join(workdir, t.OutputName())
		rp := p
		rp.Input, rp.Output, rp.Workdir = reduced, raw, workdir
		if err := tool.Run(ctx, t, rp); err != nil {
			return err
		}
		lg.Logf("tool %s: finished; parsing output", t.ID())
		t1 := time.Now()
		header, lines, err := parseToolOutput(raw, t)
		if err != nil {
			return fmt.Errorf("tool %s: parse output: %w", t.ID(), err)
		}
		lg.Logf("tool %s: parsed output for %s loci [%s]", t.ID(), commaCount(len(lines)), took(t1))
		// PutToolOutput writes every novel locus's lines + processed markers in ONE
		// transaction on the single-connection cache DB — a single-threaded phase.
		t2 := time.Now()
		if err := st.PutToolOutput(ctx, uid, header, lines, novel); err != nil {
			return err
		}
		lg.Logf("tool %s: wrote %s novel loci to the cache DB [%s]", t.ID(), commaCount(len(novel)), took(t2))
	}

	// Rebuild outFile from the cache for ALL input loci (not just the novel ones).
	header, err := st.ToolHeader(ctx, uid)
	if err != nil {
		return err
	}
	// ToolLines reads every input locus's cached lines back out (chunked, single
	// connection) — the other single-threaded cache phase.
	t3 := time.Now()
	cached, err := st.ToolLines(ctx, uid, loci)
	if err != nil {
		return err
	}
	lg.Logf("tool %s: loaded %s cached lines for %s loci [%s]",
		t.ID(), commaCount(len(cached)), commaCount(len(loci)), took(t3))
	t4 := time.Now()
	if err := writeRebuilt(outFile, header, cached, t.Format); err != nil {
		return err
	}
	lg.Logf("tool %s: rebuilt + indexed output [%s]", t.ID(), took(t4))
	return nil
}

// took formats elapsed time since t0 for a progress line (ms precision).
func took(t0 time.Time) string { return time.Since(t0).Round(time.Millisecond).String() }

// toolUID is the opaque cache key for a tool's output. It folds the assembly into
// the tool's name:version ID so a tool run under GRCh38 never serves cached lines
// to a GRCh37 query (positions mean different things across assemblies). The store
// treats this as an opaque TEXT key, so no schema change is needed to scope by
// assembly. A blank assembly (cache-only edge cases) degrades to the bare ID.
func toolUID(t config.Tool, assembly string) string {
	if assembly == "" {
		return t.ID()
	}
	return t.ID() + "|" + assembly
}

// isVCFInput reports whether a tool's input_format means "hand it a VCF" (the
// default) vs a per-variant line template.
func isVCFInput(inputFormat string) bool { return inputFormat == "" || inputFormat == "vcf" }

// writeToolInput writes the tool's {input} file for the novel loci in the tool's
// input_format: a VCF (default — preserves inPath's records/samples for the novel
// alleles) or one templated line per novel locus.
func writeToolInput(inPath, outPath, inputFormat string, novel []model.Locus, novelKeys map[string]bool) error {
	if isVCFInput(inputFormat) {
		return writeSubsetVCF(inPath, outPath, novelKeys)
	}
	return writeTemplatedVariants(outPath, novel, inputFormat)
}

// writeTemplatedVariants writes one line per locus, expanding a per-variant template
// (placeholders {chrom} {pos} {pos0} {ref} {alt} {end}). No header line is written.
func writeTemplatedVariants(outPath string, loci []model.Locus, tmpl string) error {
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	w := bufio.NewWriter(out)
	for _, l := range loci {
		fmt.Fprintln(w, expandVariantTemplate(tmpl, l))
	}
	return w.Flush()
}

// expandVariantTemplate substitutes a locus's fields into a per-variant template.
func expandVariantTemplate(tmpl string, l model.Locus) string {
	return strings.NewReplacer(
		"{chrom}", l.Chrom,
		"{pos}", strconv.FormatInt(l.Pos, 10),
		"{pos0}", strconv.FormatInt(l.Pos-1, 10),
		"{ref}", l.Ref,
		"{alt}", l.Alt,
		"{end}", strconv.FormatInt(l.Pos+int64(len(l.Ref))-1, 10),
	).Replace(tmpl)
}

// writeSubsetVCF copies inPath's header lines and only the data records carrying
// at least one novel allele into outPath (plain VCF). The tool re-annotates whole
// records; any already-cached allele in a kept record is simply re-stored
// idempotently.
func writeSubsetVCF(inPath, outPath string, novelKeys map[string]bool) error {
	in, err := openMaybeGz(inPath)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	defer out.Close()
	w := bufio.NewWriter(out)

	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") {
			fmt.Fprintln(w, line)
			continue
		}
		if recordHasNovel(line, novelKeys) {
			fmt.Fprintln(w, line)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return w.Flush()
}

// recordHasNovel reports whether a VCF data line carries any allele whose locus
// is in novelKeys. ALT/REF are uppercased to match internal/vcf.ReadFile.
func recordHasNovel(line string, novelKeys map[string]bool) bool {
	f := strings.Split(line, "\t")
	if len(f) < 5 {
		return false
	}
	pos, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return false
	}
	ref := strings.ToUpper(f[3])
	for _, alt := range strings.Split(f[4], ",") {
		alt = strings.ToUpper(strings.TrimSpace(alt))
		if alt == "" || alt == "." {
			continue
		}
		l := model.Locus{Chrom: f[0], Pos: pos, Ref: ref, Alt: alt}
		if novelKeys[l.Key()] {
			return true
		}
	}
	return false
}

// parseToolOutput reads a tool's (bgzipped) output file into its header/meta lines
// and a per-locus map of data lines.
func parseToolOutput(path string, t config.Tool) ([]string, map[model.Locus][]string, error) {
	in, err := openMaybeGz(path)
	if err != nil {
		return nil, nil, err
	}
	defer in.Close()

	var header []string
	lines := make(map[model.Locus][]string)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			header = append(header, line)
			continue
		}
		l, ok := lineLocus(line, t)
		if !ok {
			continue
		}
		lines[l] = append(lines[l], line)
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	return header, lines, nil
}

// lineLocus extracts the locus a tool output line sits on. VCF output uses the
// standard REF/ALT columns (4/5); tab output uses the tool's configured
// ref_col/alt_col (0 = position-only, leaving REF/ALT empty). A comma-separated
// ALT keeps only the first allele.
func lineLocus(line string, t config.Tool) (model.Locus, bool) {
	f := strings.Split(line, "\t")
	if len(f) < 2 {
		return model.Locus{}, false
	}
	pos, err := strconv.ParseInt(f[1], 10, 64)
	if err != nil {
		return model.Locus{}, false
	}
	refCol, altCol := 4, 5
	if t.Format == "tab" {
		refCol, altCol = t.RefCol, t.AltCol
	}
	l := model.Locus{Chrom: f[0], Pos: pos}
	if refCol > 0 && refCol <= len(f) {
		l.Ref = strings.ToUpper(f[refCol-1])
	}
	if altCol > 0 && altCol <= len(f) {
		alt := f[altCol-1]
		if i := strings.IndexByte(alt, ','); i >= 0 {
			alt = alt[:i]
		}
		l.Alt = strings.ToUpper(alt)
	}
	return l, true
}

// writeRebuilt assembles outFile (bgzipped + tabix-indexed) from the cached header
// and data lines. The tabix Writer sorts the data lines by reference order of
// appearance then position, so the index is valid regardless of input order.
func writeRebuilt(outFile string, header []string, lines []model.ToolLine, format string) error {
	opts, err := writerPreset(format)
	if err != nil {
		return err
	}
	w := tabix.NewWriter(outFile, opts.AutoIndex())
	for _, h := range header {
		w.WriteHeader(h)
	}
	for _, l := range lines {
		if err := w.Write(l.Line); err != nil {
			w.Close()
			return err
		}
	}
	return w.Close()
}

// writerPreset mirrors tool.preset but is used for the rebuilt (cached) output.
func writerPreset(format string) (*tabix.WriterOpts, error) {
	switch format {
	case "vcf", "":
		return tabix.NewWriterOpts().VCF(), nil
	case "tab":
		return tabix.NewWriterOpts().Columns(1, 2, 0).Meta('#'), nil
	default:
		return nil, fmt.Errorf("cannot rebuild tool output of format %q (want vcf|tab)", format)
	}
}

// openMaybeGz opens path, transparently decompressing .gz/.bgz (bgzf is a series
// of gzip members, which compress/gzip reads in multistream mode).
func openMaybeGz(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".gz") || strings.HasSuffix(path, ".bgz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, err
		}
		return multiCloser{Reader: gz, closers: []io.Closer{gz, f}}, nil
	}
	return multiCloser{Reader: f, closers: []io.Closer{f}}, nil
}

// multiCloser closes a stack of closers (innermost first) on Close.
type multiCloser struct {
	io.Reader
	closers []io.Closer
}

func (m multiCloser) Close() error {
	var err error
	for i := len(m.closers) - 1; i >= 0; i-- {
		if e := m.closers[i].Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}
