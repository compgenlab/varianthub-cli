package main

import (
	"flag"
	"fmt"

	annotatepkg "github.com/compgenlab/varianthub-cli/internal/annotate"
)

// cmdVcfMerge combines same-order per-source VCFs into one. It is the merge half of
// the `annotate -t` fan-out, exposed as a subcommand so a hand-run per-source
// annotation workflow (one `annotate -a …` pass per source, e.g. across an HPC
// cluster) can be recombined the same way:
//
//	varhub vcf-merge -o out.vcf.gz part.A.vcf.gz part.B.vcf.gz …
//
// The parts must hold identical sites in identical order (only their INFO/FORMAT
// columns differ). This is a same-order column combine, NOT a bcftools-style site
// merge.
func cmdVcfMerge(args []string) error {
	fs := flag.NewFlagSet("vcf-merge", flag.ContinueOnError)
	out := fs.String("o", "", "write merged output here (default: stdout; .gz/.bgz ⇒ BGZF)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	parts := fs.Args()
	if len(parts) < 2 {
		return fmt.Errorf("vcf-merge: need at least two part files")
	}
	return annotatepkg.MergeVCFParts(parts, *out)
}
