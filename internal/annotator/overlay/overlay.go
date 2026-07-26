// Package overlay is the cache-path annotator for file-based sources. For each
// locus it synthesizes a record, runs the source's hts annotators over it (built
// by annotate.AnnotatorFor — the single config→hts mapping), and reads the
// resulting INFO values back as typed rows.
package overlay

import (
	"context"
	"fmt"
	"strconv"

	"github.com/compgenlab/cghts/vcf"

	"github.com/compgenlab/varianthub-cli/internal/annotate"
	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/model"
)

// Source annotates loci from a tabix-indexed reference file (or per-chrom files).
type Source struct {
	cfg   *config.Config
	src   config.Source
	files []config.SourceFile
	anns  []config.Annotation // annotations that read from this source
}

// NewSource builds an overlay annotator for one source over its resolved file(s),
// keeping the annotation keys that name this source. cfg lets GTF sources resolve
// their bgzip+tabix index (see annotate.SourceAnnotators).
func NewSource(cfg *config.Config, src config.Source, files []config.SourceFile, anns []config.Annotation) *Source {
	var mine []config.Annotation
	for _, a := range anns {
		if a.Source == src.Name {
			mine = append(mine, a)
		}
	}
	return &Source{cfg: cfg, src: src, files: files, anns: mine}
}

// ID is the data_source_id rows are tagged with.
func (s *Source) ID() string { return s.src.ID() }

// Annotate runs the source's annotators over a synthesized record per locus and
// reads the resulting INFO values back as typed rows.
func (s *Source) Annotate(_ context.Context, loci []model.Locus) ([]model.AnnRow, error) {
	if len(s.anns) == 0 {
		return nil, nil
	}
	anns, err := annotate.SourceAnnotators(s.cfg, s.src, s.anns, s.files)
	if err != nil {
		return nil, fmt.Errorf("open source %s: %w", s.ID(), err)
	}
	defer func() {
		for _, a := range anns {
			a.Close()
		}
	}()

	var out []model.AnnRow
	for _, l := range loci {
		rec := vcf.NewRecord(l.Chrom, int(l.Pos), l.Ref, []string{l.Alt})
		for _, ann := range anns {
			if err := ann.Annotate(rec); err != nil {
				return nil, fmt.Errorf("annotate %s %s: %w", s.ID(), l.Key(), err)
			}
		}
		for _, a := range s.anns {
			if row, ok := extract(l, s.ID(), a, rec); ok {
				out = append(out, row)
			}
		}
	}
	return out, nil
}

// extract reads one annotation's value back from the annotated record.
func extract(l model.Locus, sourceID string, a config.Annotation, rec *vcf.VcfRecord) (model.AnnRow, bool) {
	av, ok := rec.InfoValue(a.Name)
	if !ok {
		return model.AnnRow{}, false
	}
	row := model.AnnRow{Locus: l, DataSource: sourceID, Key: a.Name}
	if a.IsFlag() {
		// A present flag renders as the tag name (a no-match is absent → blank),
		// rather than "true"/"false" — so a TSV cell reads e.g. "CLINVAR_MATCH".
		row.Value = model.Text(a.Name)
		return row, true
	}
	if av.IsMissing() {
		return row, false
	}
	s := av.String()
	if s == "" {
		return row, false
	}
	if a.IsNumeric() {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return row, false
		}
		row.Value = model.Number(v)
	} else {
		row.Value = model.Text(s)
	}
	return row, true
}
