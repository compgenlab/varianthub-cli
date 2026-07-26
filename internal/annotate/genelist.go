package annotate

import (
	"fmt"
	"io"
	"strings"

	htsann "github.com/compgenlab/cghts/vcf/annotate"

	"github.com/compgenlab/cghts/vcf"

	"github.com/compgenlab/cganno/internal/config"
)

// geneListAnnotator flags a variant when the gene overlapping it (per a referenced
// GTF model) is a member of a named gene set. Each configured (flag) annotation is
// added to the record on a hit. Implements htsann.Annotator; built once per source
// via buildGeneList and closed by the caller (shares the GTF query surface with
// gtfAnnotator through openGeneModel).
type geneListAnnotator struct {
	model    geneModel
	conv     *vcf.ContigConverter
	closer   io.Closer
	filename string

	genes map[string]bool     // membership set, upper-cased
	useID bool                // match gene_id rather than gene_name
	anns  []config.Annotation // the flag annotations to add on a hit
}

// buildGeneList constructs the annotator for a type="genelist" source: it opens the
// referenced GTF model and resolves the gene membership set (inline + genes_file).
func buildGeneList(cfg *config.Config, src config.Source, anns []config.Annotation) (htsann.Annotator, error) {
	if src.GTFRef == nil {
		return nil, fmt.Errorf("genelist %q: unresolved gtf reference", src.ID())
	}
	set, err := cfg.GeneSet(src)
	if err != nil {
		return nil, err
	}
	model, conv, closer, filename, err := openGeneModel(cfg, *src.GTFRef)
	if err != nil {
		return nil, fmt.Errorf("genelist %q: open gtf %s: %w", src.ID(), src.GTFRef.ID(), err)
	}
	return &geneListAnnotator{
		model: model, conv: conv, closer: closer, filename: filename,
		genes: set, useID: strings.EqualFold(src.GeneField, "gene_id"), anns: anns,
	}, nil
}

func (a *geneListAnnotator) SetupHeader(h *vcf.VcfHeader) error {
	for _, an := range a.anns {
		desc := an.Description
		if desc == "" {
			desc = "Variant is in a listed gene"
		}
		h.AddInfo(&vcf.AnnotationDef{
			IsInfo: true, ID: an.Name, Number: "0", Type: "Flag",
			Description: desc, Source: a.filename,
		})
	}
	return nil
}

func (a *geneListAnnotator) Annotate(rec *vcf.VcfRecord) error {
	chrom, ok := a.conv.Resolve(rec.Chrom)
	if !ok {
		return nil // no contig in the GTF shares this record's identity
	}
	// Query position: the variant base for an SNV, the next base for a deletion
	// (mirrors gtfAnnotator / the hts base coordinate resolution).
	pos := rec.Pos
	if len(rec.Ref) != 1 {
		pos = rec.Pos + 1
	}
	pos0 := pos - 1

	genes := a.model.FindGenes(chrom, pos0, pos0+1)
	hit := false
	for _, g := range genes {
		key := g.GeneName
		if a.useID {
			key = g.GeneID
		}
		if a.genes[strings.ToUpper(key)] {
			hit = true
			break
		}
	}
	if !hit {
		return nil
	}
	for _, an := range a.anns {
		rec.AddInfoFlag(an.Name)
	}
	return nil
}

// Close releases the indexed reader's tabix handle (a no-op for the in-memory model).
func (a *geneListAnnotator) Close() error {
	if a.closer != nil {
		return a.closer.Close()
	}
	return nil
}
