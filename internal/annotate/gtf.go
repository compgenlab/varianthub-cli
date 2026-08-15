package annotate

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/compgenlab/cghts/gtf"
	"github.com/compgenlab/cghts/vcf"
	htsann "github.com/compgenlab/cghts/vcf/annotate"

	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/fetch"
	"github.com/compgenlab/varianthub-cli/internal/objstore"
)

// openGeneModel opens a GTF source's queryable gene model. It prefers the indexed
// (tabix, memory-bounded) reader — ensuring the bgzip+tabix index exists (built
// once, cached under cache_dir) — and falls back to loading the whole GTF into
// memory (with a stderr warning) when no index is available. The returned closer
// is the indexed reader's tabix handle (nil for the in-memory model).
func openGeneModel(cfg *config.Config, src config.Source) (model htsann.GeneModel, conv *vcf.ContigConverter, closer io.Closer, filename string, err error) {
	tags := src.GTFTags
	if indexed, _, ierr := fetch.EnsureIndexedGTF(cfg, src, false); ierr == nil {
		if isrc, ierr := openIndexedGTF(indexed, tags); ierr == nil {
			return isrc, vcf.NewContigConverter(isrc.RefNames()), isrc, indexed, nil
		} else {
			warnUnindexedGTF(src, ierr)
		}
	} else {
		warnUnindexedGTF(src, ierr)
	}
	// Fallback: the whole-file in-memory model.
	raw := cfg.ResolveSourcePath(src)
	if objstore.IsRemote(raw) {
		// Reading a whole GTF into memory means streaming the entire object, so
		// the fallback that quietly costs RAM locally would quietly cost egress
		// and minutes here. An indexed copy is required rather than optional.
		return nil, nil, nil, "", fmt.Errorf(
			"GTF source %s has no usable index in the object store; "+
				"run `varhub download` to build one (the whole-file fallback is not used for remote sources)", raw)
	}
	msrc, err := gtf.NewAnnotationSource(raw, tags)
	if err != nil {
		return nil, nil, nil, "", err
	}
	return msrc, vcf.NewContigConverter(msrc.RefNames()), nil, raw, nil
}

func warnUnindexedGTF(src config.Source, err error) {
	fmt.Fprintf(os.Stderr,
		"varhub: GTF %s: no usable tabix index (%v) — loading the whole file into memory (high RAM); run `varhub download` to build the index\n",
		src.ID(), err)
}

// buildGTF constructs one grouped annotator for a gtf source over its selected
// annotations. A gtf source must resolve to a single file.
func buildGTF(cfg *config.Config, src config.Source, annos []config.Annotation, files []config.SourceFile) (htsann.Annotator, error) {
	if len(files) != 1 {
		return nil, fmt.Errorf("gtf source %q must be a single file", src.ID())
	}
	model, _, _, filename, err := openGeneModel(cfg, src)
	if err != nil {
		return nil, err
	}

	// Which of the GTF's derived fields this snapshot asked for, and what to
	// call each. The annotator can write seven; a manifest naming two should
	// produce two, under the names it gave them.
	fields := make([]string, 0, len(annos))
	names := htsann.FieldNames{}
	for _, a := range annos {
		key := strings.ToUpper(a.FieldName())
		fields = append(fields, key)
		names[key] = a.Name
	}

	return htsann.NewGtfAnnotator(htsann.GtfOptions{
		Filename: filename,
		// The model this service opened, rather than a path for the library to
		// open itself. Where a GTF lives, whether its index has been built yet,
		// and whether reading a remote one whole is acceptable are questions
		// about this deployment — see openGeneModel — and none of them are the
		// annotator's to answer.
		Source:       model,
		Fields:       fields,
		Names:        names,
		RequiredTags: src.GTFTags,
		AutoConvert:  true,
	})
}

// openIndexedGTF opens the indexed gene model at a locator, local or remote.
func openIndexedGTF(locator string, tags []string) (*gtf.IndexedAnnotationSource, error) {
	tr, err := objstore.OpenTabixLocator(context.Background(), locator)
	if err != nil {
		return nil, err
	}
	if tr == nil {
		return gtf.NewIndexedAnnotationSource(locator, tags)
	}
	return gtf.NewIndexedAnnotationSourceFrom(tr, tags), nil
}
