package annotate

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/compgenlab/cghts/bed"
	"github.com/compgenlab/cghts/gtf"
	"github.com/compgenlab/cghts/vcf"

	"github.com/compgenlab/cganno/internal/config"
	"github.com/compgenlab/cganno/internal/fetch"
)

// geneModel is the query surface an annotator needs from a GTF gene model, served
// either by the whole-file in-memory gtf.AnnotationSource or the tabix-backed,
// memory-bounded gtf.IndexedAnnotationSource.
type geneModel interface {
	FindGenes(ref string, start, end int) []*gtf.Gene
	FindGenicRegionForPos(ref string, pos int, strand bed.Strand, geneID string) gtf.GenicRegion
	RefNames() []string
}

// gtfField selects one GTF-derived value (key, one of config.GTFFields) and the
// INFO tag to write it under.
type gtfField struct {
	name string // output INFO key (the annotation's Name)
	key  string // GTF field, upper-cased (GENE/GENEID/STRAND/BIOTYPE/REGION/CODING/NONCODING)
}

// gtfAnnotator overlays gene annotations from a GTF onto VCF records, emitting
// only the selected fields under their configured names. It loads the GTF gene
// model once (in-memory, via the hts gtf package) and queries it per variant.
// One annotator serves every selected field of a single gtf source, so the GTF
// is parsed once regardless of how many fields are requested. Implements
// htsann.Annotator.
type gtfAnnotator struct {
	src      geneModel
	fields   []gtfField
	conv     *vcf.ContigConverter // cross-scheme contig matching (always on, like the other annotators)
	filename string
	closer   io.Closer // the indexed reader's tabix handle, if any
}

// openGeneModel opens a GTF source's queryable gene model. It prefers the indexed
// (tabix, memory-bounded) reader — ensuring the bgzip+tabix index exists (built
// once, cached under cache_dir) — and falls back to loading the whole GTF into
// memory (with a stderr warning) when no index is available. The returned closer
// is the indexed reader's tabix handle (nil for the in-memory model).
func openGeneModel(cfg *config.Config, src config.Source) (model geneModel, conv *vcf.ContigConverter, closer io.Closer, filename string, err error) {
	tags := src.GTFTags
	if indexed, _, ierr := fetch.EnsureIndexedGTF(cfg, src, false); ierr == nil {
		if isrc, ierr := gtf.NewIndexedAnnotationSource(indexed, tags); ierr == nil {
			return isrc, vcf.NewContigConverter(isrc.RefNames()), isrc, indexed, nil
		} else {
			warnUnindexedGTF(src, ierr)
		}
	} else {
		warnUnindexedGTF(src, ierr)
	}
	// Fallback: the whole-file in-memory model.
	raw := cfg.ResolveSourcePath(src)
	msrc, err := gtf.NewAnnotationSource(raw, tags)
	if err != nil {
		return nil, nil, nil, "", err
	}
	return msrc, vcf.NewContigConverter(msrc.RefNames()), nil, raw, nil
}

// newGTFAnnotator returns an annotator over a GTF source, emitting the selected
// fields under their configured names.
func newGTFAnnotator(cfg *config.Config, src config.Source, fields []gtfField) (*gtfAnnotator, error) {
	model, conv, closer, filename, err := openGeneModel(cfg, src)
	if err != nil {
		return nil, err
	}
	return &gtfAnnotator{src: model, fields: fields, conv: conv, closer: closer, filename: filename}, nil
}

func warnUnindexedGTF(src config.Source, err error) {
	fmt.Fprintf(os.Stderr,
		"cganno: GTF %s: no usable tabix index (%v) — loading the whole file into memory (high RAM); run `cganno download` to build the index\n",
		src.ID(), err)
}

func (a *gtfAnnotator) SetupHeader(h *vcf.VcfHeader) error {
	for _, f := range a.fields {
		h.AddInfo(&vcf.AnnotationDef{
			IsInfo: true, ID: f.name, Number: ".", Type: "String",
			Description: gtfFieldDesc(f.key), Source: a.filename,
		})
	}
	return nil
}

func (a *gtfAnnotator) Annotate(rec *vcf.VcfRecord) error {
	chrom, ok := a.conv.Resolve(rec.Chrom)
	if !ok {
		return nil // no contig in the GTF shares this record's identity
	}
	// Query position: the variant base for an SNV, the next base for a deletion
	// (mirrors the hts annotate base coordinate resolution).
	pos := rec.Pos
	if len(rec.Ref) != 1 {
		pos = rec.Pos + 1
	}
	pos0 := pos - 1

	genes := a.src.FindGenes(chrom, pos0, pos0+1)
	if len(genes) == 0 {
		return nil
	}
	for _, f := range a.fields {
		if v := gtfValue(a.src, chrom, pos0, genes, f.key); v != "" {
			rec.AddInfo(f.name, v)
		}
	}
	return nil
}

// Close releases the indexed reader's tabix handle (a no-op for the in-memory model).
func (a *gtfAnnotator) Close() error {
	if a.closer != nil {
		return a.closer.Close()
	}
	return nil
}

// gtfValue computes one field's comma-joined value across the overlapping genes.
// GENE/GENEID/STRAND/REGION yield one value per gene (parallel order); BIOTYPE
// fills "." for genes lacking one; CODING/NONCODING are the gene names filtered
// by coding status (empty → the field is omitted for this record). Variants are
// unstranded, so REGION is always a sense code.
func gtfValue(src geneModel, ref string, pos0 int, genes []*gtf.Gene, key string) string {
	var vals []string
	for _, g := range genes {
		switch key {
		case "GENE":
			vals = append(vals, g.GeneName)
		case "GENEID":
			vals = append(vals, g.GeneID)
		case "STRAND":
			vals = append(vals, string(g.Strand))
		case "BIOTYPE":
			if g.BioType != "" {
				vals = append(vals, g.BioType)
			} else {
				vals = append(vals, ".")
			}
		case "REGION":
			vals = append(vals, src.FindGenicRegionForPos(ref, pos0, bed.StrandNone, g.GeneID).Code)
		case "CODING":
			if g.IsCoding() {
				vals = append(vals, g.GeneName)
			}
		case "NONCODING":
			if !g.IsCoding() {
				vals = append(vals, g.GeneName)
			}
		}
	}
	return strings.Join(vals, ",")
}

func gtfFieldDesc(key string) string {
	switch key {
	case "GENE":
		return "Gene name"
	case "GENEID":
		return "Gene ID"
	case "STRAND":
		return "Gene strand"
	case "BIOTYPE":
		return "Gene biotype"
	case "REGION":
		return "Genic region"
	case "CODING":
		return "Coding gene name"
	case "NONCODING":
		return "Non-coding gene name"
	}
	return "GTF annotation"
}

// buildGTF constructs one grouped annotator for a gtf source over its selected
// annotations. A gtf source must resolve to a single file.
func buildGTF(cfg *config.Config, src config.Source, annos []config.Annotation, files []config.SourceFile) (*gtfAnnotator, error) {
	if len(files) != 1 {
		return nil, fmt.Errorf("gtf source %q must be a single file", src.ID())
	}
	fields := make([]gtfField, len(annos))
	for i, a := range annos {
		fields[i] = gtfField{name: a.Name, key: strings.ToUpper(a.FieldName())}
	}
	return newGTFAnnotator(cfg, src, fields)
}
