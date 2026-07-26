package annotator

import (
	"context"
	"hash/fnv"

	"github.com/compgenlab/varianthub-cli/internal/model"
)

// Mock is a deterministic stand-in for the real annotation workflow. It derives
// plausible, reproducible annotations from each locus so datasets, the cache,
// annotate, and filter can be exercised end-to-end without external tools.
type Mock struct {
	byName map[string]model.DataSource
	first  model.DataSource
}

var _ Annotator = (*Mock)(nil)

// NewMock builds a Mock that tags rows with the configured (pinned) sources.
func NewMock(sources []model.DataSource) *Mock {
	m := &Mock{byName: make(map[string]model.DataSource)}
	for _, s := range sources {
		m.byName[s.Name] = s
		if m.first.Name == "" {
			m.first = s
		}
	}
	return m
}

var (
	genes        = []string{"BRCA1", "BRCA2", "TP53", "EGFR"}
	consequences = []string{"missense_variant", "synonymous_variant", "stop_gained", "intron_variant"}
	impacts      = []string{"MODERATE", "LOW", "HIGH", "MODIFIER"}
	clinSigs     = []string{"Pathogenic", "Likely_pathogenic", "Benign", "Uncertain_significance"}
	reviewStatus = []string{"criteria_provided,_single_submitter", "reviewed_by_expert_panel",
		"no_assertion_criteria_provided", "criteria_provided,_multiple_submitters"}
)

// Annotate returns deterministic canned rows for each locus.
func (m *Mock) Annotate(_ context.Context, loci []model.Locus) ([]model.AnnRow, error) {
	var out []model.AnnRow
	for _, l := range loci {
		h := hash(l.Key())
		gIdx := h % uint64(len(genes))
		cIdx := h % uint64(len(consequences))
		sIdx := (h / 7) % uint64(len(clinSigs))
		af := float64(h%1000) / 1000.0

		out = append(out,
			m.row("vep", l, "gene", model.Text(genes[gIdx])),
			m.row("vep", l, "consequence", model.Text(consequences[cIdx])),
			m.row("vep", l, "impact", model.Text(impacts[cIdx])),
			m.row("clinvar", l, "clinvar_sig", model.Text(clinSigs[sIdx])),
			m.row("clinvar", l, "clinvar_review_status", model.Text(reviewStatus[sIdx])),
			m.row("gnomad", l, "af", model.Number(af)),
		)
	}
	return out, nil
}

// row attaches an annotation to the named source if pinned, else the first one.
func (m *Mock) row(sourceName string, l model.Locus, key string, v model.Value) model.AnnRow {
	src, ok := m.byName[sourceName]
	if !ok {
		src = m.first
	}
	return model.AnnRow{Locus: l, DataSource: src.ID(), Key: key, Value: v}
}

func hash(s string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(s))
	return h.Sum64()
}
