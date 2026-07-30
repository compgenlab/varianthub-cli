package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/compgenlab/cghts/htsio/bgzf"
	"github.com/compgenlab/cghts/htsio/tabix"
	"github.com/compgenlab/varianthub-cli/internal/htsidx"
)

// cmdBgzip and cmdTabix are hidden subcommands that mimic the `bgzip` and `tabix`
// programs using the hts library, so source-build recipes and tool post-processing
// scripts can `varhub bgzip` / `varhub tabix` without those binaries installed.

// tabixFlags holds the tabix index configuration shared by `tabix` and by `bgzip`'s
// optional auto-index.
type tabixFlags struct {
	preset                string
	seq, begin, end, skip int
	comment               string
	zero                  bool
}

// register adds the tabix flags (short + long names) to fs.
func (tf *tabixFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&tf.preset, "p", "", "preset: vcf | bed | gff (aka gtf)")
	fs.StringVar(&tf.preset, "preset", "", "")
	fs.IntVar(&tf.seq, "s", 0, "sequence (chrom) column, 1-based")
	fs.IntVar(&tf.seq, "seq", 0, "")
	fs.IntVar(&tf.begin, "b", 0, "begin/start column, 1-based")
	fs.IntVar(&tf.begin, "begin", 0, "")
	fs.IntVar(&tf.end, "e", 0, "end column, 1-based (0 = same as begin)")
	fs.IntVar(&tf.end, "end", 0, "")
	fs.IntVar(&tf.skip, "S", 0, "skip the first N lines")
	fs.IntVar(&tf.skip, "skip", 0, "")
	fs.StringVar(&tf.comment, "c", "", "comment/meta char: skip lines starting with it (default #)")
	fs.StringVar(&tf.comment, "comment", "", "")
	fs.BoolVar(&tf.zero, "0", false, "coordinates are 0-based")
	fs.BoolVar(&tf.zero, "zero", false, "")
}

// set reports whether any index configuration was requested.
func (tf *tabixFlags) set() bool { return tf.preset != "" || tf.seq > 0 || tf.begin > 0 }

// opts builds hts tabix WriterOpts from a preset or explicit columns.
func (tf *tabixFlags) opts() (*tabix.WriterOpts, error) {
	if tf.preset != "" {
		switch strings.ToLower(tf.preset) {
		case "vcf":
			return tabix.NewWriterOpts().VCF(), nil
		case "bed":
			return tabix.NewWriterOpts().BED(), nil
		case "gff", "gtf":
			return tabix.NewWriterOpts().GFF(), nil
		default:
			return nil, fmt.Errorf("unknown preset %q (want vcf|bed|gff)", tf.preset)
		}
	}
	if tf.seq <= 0 || tf.begin <= 0 {
		return nil, fmt.Errorf("need -p PRESET, or at least -s (seq) and -b (begin) columns")
	}
	o := tabix.NewWriterOpts().Columns(tf.seq, tf.begin, tf.end)
	if tf.comment != "" {
		o = o.Meta(tf.comment[0])
	}
	if tf.skip > 0 {
		o = o.Skip(tf.skip)
	}
	if tf.zero {
		o = o.ZeroBased()
	}
	return o, nil
}

// cmdBgzip BGZF-compresses a file argument (or stdin) to -o FILE (or stdout). When a
// tabix preset/columns are also given (which requires -o), it writes the index too —
// `varhub bgzip -o out.vcf.gz -p vcf < in.vcf` produces out.vcf.gz + out.vcf.gz.tbi.
func cmdBgzip(args []string) error {
	fs := flag.NewFlagSet("bgzip", flag.ContinueOnError)
	out := fs.String("o", "", "write to this file (default stdout); required to also write an index")
	fs.StringVar(out, "output", "", "")
	tf := &tabixFlags{}
	tf.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}

	index := tf.set()
	if index && *out == "" {
		return fmt.Errorf("bgzip: writing an index needs -o FILE (cannot index a stream to stdout)")
	}

	in := io.Reader(os.Stdin)
	if rest := fs.Args(); len(rest) > 0 && rest[0] != "-" {
		f, err := os.Open(rest[0])
		if err != nil {
			return err
		}
		defer f.Close()
		in = f
	}

	var w *bgzf.Writer
	if *out != "" {
		var err error
		if w, err = bgzf.NewBGZipFile(*out); err != nil {
			return err
		}
	} else {
		w = bgzf.NewWriter(os.Stdout)
	}
	if _, err := io.Copy(w, in); err != nil {
		w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}

	if index {
		opts, err := tf.opts()
		if err != nil {
			return fmt.Errorf("bgzip: %w", err)
		}
		if err := htsidx.WriteIndex(opts, *out); err != nil {
			return fmt.Errorf("bgzip: index %s: %w", *out, err)
		}
	}
	return nil
}

// cmdTabix writes a tabix index (<file>.tbi) for an existing BGZF-compressed file.
func cmdTabix(args []string) error {
	fs := flag.NewFlagSet("tabix", flag.ContinueOnError)
	tf := &tabixFlags{}
	tf.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	rest := fs.Args()
	if len(rest) != 1 {
		return fmt.Errorf("usage: tabix [-p vcf|bed|gff | -s SEQ -b BEGIN -e END -S SKIP -c CHAR -0] <file.gz>")
	}
	opts, err := tf.opts()
	if err != nil {
		return fmt.Errorf("tabix: %w", err)
	}
	if err := htsidx.WriteIndex(opts, rest[0]); err != nil {
		return fmt.Errorf("tabix: %s: %w", rest[0], err)
	}
	return nil
}
