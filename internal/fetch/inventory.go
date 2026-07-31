package fetch

import (
	"context"
	"strings"

	"github.com/compgenlab/varianthub-cli/internal/config"
)

// FileInfo is one file a source occupies in the cache.
type FileInfo struct {
	// Path is relative to the cache root, so it reads the same whether the
	// cache is a directory or a bucket prefix — "gencode/48/gencode.gtf.gz"
	// either way.
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

// Inventory lists the files a source occupies in the cache.
//
// It asks the same locator resolution that put them there rather than scanning
// the cache, which is what lets it answer for an object store: there is nothing
// to walk in a bucket, and a caller that tried would have to reimplement the
// layout. A source with nothing downloaded returns nil, not an error.
func Inventory(ctx context.Context, cfg *config.Config, src config.Source) ([]FileInfo, error) {
	if src.IsBuiltinSource() || src.IsGeneList() {
		return nil, nil // computed from the variant; no files of its own
	}
	root := cfg.CacheDirAbs()

	var out []FileInfo
	add := func(loc string) error {
		size, ok, err := locatorSize(ctx, loc)
		if err != nil || !ok {
			return err
		}
		out = append(out, FileInfo{Path: relativeTo(root, loc), SizeBytes: size})
		return nil
	}

	// The data files, each with whichever index sits beside it.
	for _, f := range cfg.ResolveSourceFiles(src) {
		if f.Local {
			continue // a localpath source is used in place, not cached
		}
		if err := add(f.Path); err != nil {
			return nil, err
		}
		for _, ext := range []string{".tbi", ".csi"} {
			if err := add(f.Path + ext); err != nil {
				return nil, err
			}
		}
	}

	// A GTF is queried through its converted copy, which lives under a
	// different name than the download and so is not covered above.
	if src.IsGTFSource() {
		idx := cfg.ResolveGTFIndexPath(src)
		if err := add(idx); err != nil {
			return nil, err
		}
		for _, ext := range []string{".tbi", ".csi"} {
			if err := add(idx + ext); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// relativeTo trims the cache root from a locator, leaving the layout-relative
// path. Falls back to the full locator when it does not sit under the root,
// which happens for a localpath source.
func relativeTo(root, loc string) string {
	r := strings.TrimSuffix(root, "/")
	if rest, ok := strings.CutPrefix(loc, r+"/"); ok {
		return rest
	}
	return loc
}
