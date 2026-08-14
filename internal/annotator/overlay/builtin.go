package overlay

import (
	"context"
	"fmt"
	"strings"

	"github.com/compgenlab/cghts/vcf"

	"github.com/compgenlab/varianthub-cli/internal/annotate"
	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/model"
)

// BuiltinSource is the cache/locus-path annotator for the variant-only builtins
// (auto_id, indel, tstv, tags). For each locus it synthesizes a record, runs the
// hts builtin annotators, and reads back one value per annotation — the same
// annotators the `-o` VCF pipeline uses, so both paths agree. Sample-derived
// builtins (dosage/vaf/minor_strand/fisher_sb/copy_logratio) and vardist (needs
// neighboring variants) can't run on a bare locus and are skipped.
type BuiltinSource struct {
	src  config.Source
	anns []config.Annotation // the variant-only builtin annotations of src
}

// NewBuiltinSource builds a locus-path annotator over a source's variant-only
// builtins. src must be a type="builtin" source; its own annotations carry the
// builtin name and output name.
func NewBuiltinSource(src config.Source) *BuiltinSource {
	var mine []config.Annotation
	for _, a := range src.Annotations {
		if annotate.VariantOnlyBuiltin(a.Builtin) {
			mine = append(mine, a)
		}
	}
	return &BuiltinSource{src: src, anns: mine}
}

// ID is the data_source_id rows are tagged with.
func (s *BuiltinSource) ID() string { return s.src.ID() }

// Annotate runs the variant-only builtins over a synthesized record per locus and
// reads back one value per annotation.
func (s *BuiltinSource) Annotate(_ context.Context, loci []model.Locus) ([]model.AnnRow, error) {
	if len(s.anns) == 0 {
		return nil, nil
	}
	anns := make([]htsAnn, 0, len(s.anns))
	for _, a := range s.anns {
		ann, err := annotate.BuiltinAnnotator(a)
		if err != nil {
			return nil, fmt.Errorf("builtin %s: %w", a.Builtin, err)
		}
		anns = append(anns, htsAnn{a: a, ann: ann})
	}

	var out []model.AnnRow
	for _, l := range loci {
		rec := vcf.NewRecord(l.Chrom, int(l.Pos), l.Ref, []string{l.Alt})
		for _, h := range anns {
			if err := h.ann.Annotate(rec); err != nil {
				return nil, fmt.Errorf("builtin %s %s: %w", h.a.Builtin, l.Key(), err)
			}
		}
		for _, h := range anns {
			if row, ok := extractBuiltin(l, s.ID(), h.a, rec); ok {
				out = append(out, row)
			}
		}
	}
	return out, nil
}

// htsAnn pairs a builtin annotation with its hts annotator.
type htsAnn struct {
	a   config.Annotation
	ann interface{ Annotate(*vcf.VcfRecord) error }
}

// extractBuiltin reads one value for a builtin annotation off the annotated
// record. auto_id yields the synthesized ID; tstv the TS/TV call (blank for
// indels/multiallelics); indel the insertion/deletion class (blank for SNVs);
// tags the constant tag's value (or its name, for a flag).
func extractBuiltin(l model.Locus, sourceID string, a config.Annotation, rec *vcf.VcfRecord) (model.AnnRow, bool) {
	row := model.AnnRow{Locus: l, DataSource: sourceID, Key: a.Name}
	switch a.Builtin {
	case "auto_id":
		if id := rec.ID(); id != "" {
			row.Value = model.Text(id)
			return row, true
		}
	case "tstv":
		// Read under the manifest's name, which is what the annotator now writes
		// there. This used to look for CG_TSTV and file the value under a.Name —
		// a translation that existed only on this path, so `name` meant one
		// thing here and another in a VCF. The annotator is told the name now,
		// and this simply reads it.
		if v, ok := rec.InfoValue(a.Name); ok && !v.IsMissing() && v.String() != "" {
			row.Value = model.Text(v.String())
			return row, true
		}
	case "indel":
		// Still a derivation rather than a read: the annotator writes five
		// fields describing the change, and this path reports one categorical
		// value. The names follow the manifest, so the fields it inspects are
		// the ones the annotator was told to write.
		if _, ok := rec.InfoValue(a.Name + "_INSERT"); ok {
			row.Value = model.Text("insertion")
			return row, true
		}
		if _, ok := rec.InfoValue(a.Name + "_DELETE"); ok {
			row.Value = model.Text("deletion")
			return row, true
		}
	case "tags":
		key := a.Args
		if i := strings.IndexByte(a.Args, ':'); i >= 0 {
			key = a.Args[:i]
		}
		if v, ok := rec.InfoValue(key); ok {
			// A valued tag (PANEL=v1) renders its value; a bare flag renders its
			// name (matching the file-flag convention in extract()).
			if s := v.String(); s != "" {
				row.Value = model.Text(s)
			} else {
				row.Value = model.Text(key)
			}
			return row, true
		}
	}
	return row, false
}
