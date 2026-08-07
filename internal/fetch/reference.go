package fetch

import (
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/compgenlab/cghts/htsio/bgzf"
	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/faidx"
	"github.com/compgenlab/varianthub-cli/internal/objstore"
)

// Provisioning a reference genome.
//
// A reference is fetched like any other source and then indexed differently:
// tabix indexes records by coordinate, a FASTA is indexed by sequence offset.
// So the download path is shared and only the indexing step differs.
//
// Two things are specific to it. The file must end up BGZF, because htslib can
// build a .fai over an uncompressed or BGZF file and nothing else — and the
// references people actually publish are plain gzip, which is neither. And it
// must end up on a local disk, because a tool step binds its directory into a
// container; a reference is the one source that cannot be read from a bucket.

// fetchReference provisions a reference source: download, recompress to BGZF if
// needed, and build .fai/.gzi.
func fetchReference(ctx context.Context, cfg *config.Config, src config.Source,
	force bool) (Result, error) {

	res := Result{Source: src.ID(), Data: "-", Index: "-"}
	files := cfg.ResolveSourceTargets(src)
	if len(files) != 1 {
		return res, fmt.Errorf("%s: a reference has exactly one file, found %d",
			src.ID(), len(files))
	}
	// Always local, whatever the deployment's storage is: a tool step binds the
	// FASTA's directory into a container. The storage location a job names
	// governs the durable copy below, not this one — there is no choice about
	// where the working copy lives.
	local := cfg.ResolveReferencePath(src, objstore.Base(files[0].Path))
	f := files[0]
	f.Path = local

	prepared, indexed := f.Path, false
	if !force {
		// Already prepared: the BGZF file and its .fai are both there.
		if p, ok := preparedReference(f.Path); ok {
			res.Data, res.Index = "skipped", "reused"
			logf("%s: cached %s", src.ID(), filepath.Base(p))
			return res, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return res, err
	}
	sum, err := resolveChecksum(ctx, f.Checksum, filepath.Base(f.Path))
	if err != nil {
		return res, err
	}
	logf("%s: downloading %s", src.ID(), filepath.Base(f.Path))
	if err := download(ctx, f.URL, f.Path, sum); err != nil {
		return res, err
	}
	res.Data = "downloaded"

	if prepared, err = ensureBGZF(f.Path); err != nil {
		return res, fmt.Errorf("%s: %w", src.ID(), err)
	}
	logf("%s: indexing %s", src.ID(), filepath.Base(prepared))
	entries, err := faidx.Build(prepared)
	if err != nil {
		return res, fmt.Errorf("%s: %w", src.ID(), err)
	}
	indexed = true
	res.Index = "built"
	logf("%s: indexed %d sequence(s)", src.ID(), len(entries))
	_ = indexed

	// A durable copy, so another machine unpacks what this one fetched,
	// recompressed and indexed rather than repeating most of a gigabyte and a
	// bgzip pass. Best effort: the reference is already usable here, and failing
	// the download over the copy would discard work that went right.
	if dst := cfg.CacheDirAbs(); objstore.IsObject(dst) {
		if err := publishReference(ctx, prepared, dst, src); err != nil {
			logf("%s: durable copy failed: %v", src.ID(), err)
		} else {
			logf("%s: durable copy kept", src.ID())
		}
	}
	return res, nil
}

// publishReference uploads a prepared reference and its indexes.
//
// The indexes go too: rebuilding them is cheap but not free, and a copy that
// restores to something a tool still has to index is only half a copy.
func publishReference(ctx context.Context, local, cacheDir string, src config.Source) error {
	// Under <name>/<version>/ like every other file a source owns, so the
	// storage browser attributes it and the metrics count it.
	base := objstore.Join(cacheDir, src.Name, src.Version)
	for _, ext := range []string{"", ".fai", ".gzi"} {
		if !fileExists(local + ext) {
			continue // .gzi exists only for BGZF
		}
		if err := putObject(ctx, local+ext, objstore.Join(base, filepath.Base(local)+ext)); err != nil {
			return fmt.Errorf("%s: %w", filepath.Base(local+ext), err)
		}
	}
	return nil
}

// preparedReference reports the usable BGZF file for a reference, if it and its
// .fai are already present.
func preparedReference(path string) (string, bool) {
	for _, p := range []string{path, bgzfName(path)} {
		if fileExists(p) && fileExists(p+".fai") {
			return p, true
		}
	}
	return "", false
}

// bgzfName is where a recompressed copy of path lands.
func bgzfName(path string) string {
	if strings.HasSuffix(path, ".gz") {
		return path // recompressed in place, under the same name
	}
	return path + ".gz"
}

// ensureBGZF returns a BGZF version of path, recompressing when it is plain
// gzip or uncompressed.
//
// htslib indexes an uncompressed or BGZF FASTA and nothing else, and the
// references people publish — Ensembl's and GENCODE's alike — are plain gzip.
// Left alone, the failure surfaces inside a container as "Cannot index files
// compressed with gzip, please use bgzip", a long way from the fetch that chose
// the file.
func ensureBGZF(path string) (string, error) {
	kind, err := compressionOf(path)
	if err != nil {
		return "", err
	}
	if kind == compBGZF {
		return path, nil
	}

	dest := bgzfName(path)
	tmp := dest + ".bgzf.tmp"
	in, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer in.Close()

	var src io.Reader = in
	if kind == compGzip {
		zr, err := gzip.NewReader(in)
		if err != nil {
			return "", err
		}
		defer zr.Close()
		src = zr
	}

	logf("recompressing %s to BGZF (faidx cannot index plain gzip)", filepath.Base(path))
	w, err := bgzf.NewBGZipFile(tmp)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(w, src); err != nil {
		w.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("recompress %s: %w", filepath.Base(path), err)
	}
	if err := w.Close(); err != nil {
		os.Remove(tmp)
		return "", err
	}
	in.Close()
	// The original is redundant once recompressed, and a reference is large
	// enough that keeping both is a real cost rather than a tidy safety net.
	if dest == path {
		if err := os.Remove(path); err != nil {
			os.Remove(tmp)
			return "", err
		}
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return dest, nil
}

type compression int

const (
	compPlain compression = iota
	compGzip
	compBGZF
)

// compressionOf reports how a file is compressed. BGZF is gzip carrying a "BC"
// extra subfield; plain gzip has no extra field at all.
func compressionOf(path string) (compression, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	hdr := make([]byte, 18)
	n, err := io.ReadFull(f, hdr)
	if err != nil && n < 2 {
		return 0, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	if hdr[0] != 0x1f || hdr[1] != 0x8b {
		return compPlain, nil
	}
	if n >= 14 && hdr[3]&0x04 != 0 && hdr[12] == 'B' && hdr[13] == 'C' {
		return compBGZF, nil
	}
	return compGzip, nil
}
