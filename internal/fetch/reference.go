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
			// The durable copy is separate state from the local one, so a run
			// that finds the work already done still checks whether the copy
			// exists. Returning here unconditionally is how a prepared reference
			// could never acquire one: the local file satisfies every later run,
			// so the upload never gets a second chance — the same trap the tool
			// archive fell into.
			publishReferenceIfMissing(ctx, cfg, p, src)
			return res, nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
		return res, err
	}

	// Before going to the origin, try the durable copy this or another machine
	// already prepared. It is the finished article — BGZF and indexed — so
	// restoring skips a decompress, a bgzip pass and an index build as well as
	// most of a gigabyte from someone else's server.
	//
	// Skipped under --force, which exists to say "ignore what is already there".
	if !force {
		if p, ok := restoreDurableReference(ctx, cfg, src, f.Path); ok {
			res.Data, res.Index = "restored", "restored"
			logf("%s: restored %s from the durable copy", src.ID(), filepath.Base(p))
			return res, nil
		}
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
	publishReferenceIfMissing(ctx, cfg, prepared, src)
	return res, nil
}

// publishReferenceIfMissing uploads the durable copy when the store does not
// already have it.
//
// Keyed on the object being absent rather than on a flag, so a run that
// prepared the file and a run that found it prepared behave the same. Best
// effort: the reference is usable locally either way, and failing the download
// over the copy would discard work that went right.
func publishReferenceIfMissing(ctx context.Context, cfg *config.Config, local string,
	src config.Source) {

	dst := cfg.CacheDirAbs()
	if !objstore.IsObject(dst) {
		return
	}
	target := objstore.Join(dst, src.Name, src.Version, filepath.Base(local))
	if objectExists(ctx, target) {
		return
	}
	logf("%s: uploading the durable copy", src.ID())
	if err := publishReference(ctx, local, dst, src); err != nil {
		logf("%s: durable copy failed: %v", src.ID(), err)
		return
	}
	logf("%s: durable copy kept", src.ID())
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

// EnsureReference makes a pinned reference usable on this machine, restoring it
// from the durable copy when the local working copy is absent.
//
// This is what the durable copy exists for. Provisioning writes the FASTA
// locally and uploads a prepared copy, but {ref} is always a local path — a tool
// step binds its directory into a container, which cannot read an object store.
// So a worker that did not run the download has nothing to bind, and until this
// existed nothing put it there: the upload had no reader, and a second replica
// failed with a --fasta pointing at a file that was never created.
//
// Restoring beats re-downloading by more than the transfer. The origin publishes
// plain gzip, so provisioning also recompresses to BGZF and indexes it; the
// durable copy is the finished article, so this skips a decompress, a bgzip pass
// and an index build as well as most of a gigabyte from someone else's server.
//
// Called where a tool is about to run, so a snapshot that pins a reference no
// selected annotation needs never transfers it.
func EnsureReference(ctx context.Context, cfg *config.Config, snap *config.Snapshot) error {
	if snap.Reference == "" {
		return nil // pins none; RequiresMissingReference reports that separately
	}
	var src *config.Source
	for i := range snap.Sources {
		if snap.Sources[i].IsReference() {
			src = &snap.Sources[i]
			break
		}
	}
	if src == nil {
		// A reference from deployment config rather than a pinned source. There
		// is no source to name a durable copy under, so the path is whatever the
		// deployment put there.
		if _, ok := preparedReference(snap.Reference); !ok {
			return fmt.Errorf("the configured reference %s is missing or unindexed",
				snap.Reference)
		}
		return nil
	}
	if _, ok := preparedReference(snap.Reference); ok {
		return nil
	}

	dst := cfg.CacheDirAbs()
	if !objstore.IsObject(dst) {
		// Nothing to restore from: the durable copy is what an object store
		// holds, and a filesystem cache_dir is the working copy itself.
		return fmt.Errorf("reference %s is not present at %s; provision it with "+
			"`varhub download %s`", src.ID(), snap.Reference, src.ID())
	}

	base := objstore.Join(dst, src.Name, src.Version)
	name := filepath.Base(snap.Reference)
	if !objectExists(ctx, objstore.Join(base, name)) {
		return fmt.Errorf("reference %s is not present at %s and no durable copy "+
			"exists at %s; provision it with `varhub download %s`",
			src.ID(), snap.Reference, objstore.Join(base, name), src.ID())
	}

	logf("%s: restoring the durable copy", src.ID())
	for _, ext := range []string{"", ".fai", ".gzi"} {
		loc := objstore.Join(base, name+ext)
		if ext != "" && !objectExists(ctx, loc) {
			// .gzi exists only for BGZF. A missing .fai is fatal below rather
			// than here, so the message names the index rather than the fetch.
			continue
		}
		if err := fetchObject(ctx, loc, snap.Reference+ext); err != nil {
			return fmt.Errorf("restore %s: %w", filepath.Base(loc), err)
		}
	}
	// Verify what was restored is what a tool needs, rather than trusting that
	// the objects were complete: a durable copy uploaded before indexing would
	// otherwise leave {ref} pointing at a FASTA no tool can random-access.
	if _, ok := preparedReference(snap.Reference); !ok {
		return fmt.Errorf("restored %s but it is still missing its .fai index; "+
			"re-provision it with `varhub download --force %s`", src.ID(), src.ID())
	}
	logf("%s: restored %s", src.ID(), name)
	return nil
}

// restoreDurableReference copies a prepared reference back from the object store,
// reporting whether it produced a usable one.
//
// Best effort by design: every failure here falls through to the origin
// download, which is slower but always works. A missing or half-uploaded durable
// copy should cost time, not the provisioning run.
func restoreDurableReference(ctx context.Context, cfg *config.Config, src config.Source,
	local string) (string, bool) {

	dst := cfg.CacheDirAbs()
	if !objstore.IsObject(dst) {
		return "", false
	}
	// The durable copy is the recompressed name, which is what provisioning
	// uploaded — not the origin's, which may be plain gzip.
	name := filepath.Base(bgzfName(local))
	base := objstore.Join(dst, src.Name, src.Version)
	if !objectExists(ctx, objstore.Join(base, name)) {
		return "", false
	}
	target := filepath.Join(filepath.Dir(local), name)
	for _, ext := range []string{"", ".fai", ".gzi"} {
		loc := objstore.Join(base, name+ext)
		if ext != "" && !objectExists(ctx, loc) {
			continue
		}
		if err := fetchObject(ctx, loc, target+ext); err != nil {
			logf("%s: durable copy unusable (%v); falling back to the origin", src.ID(), err)
			return "", false
		}
	}
	// Only a copy that a tool can actually random-access counts. A durable copy
	// uploaded before its index would otherwise be reported as a success and
	// fail later inside a container.
	p, ok := preparedReference(target)
	if !ok {
		logf("%s: durable copy has no .fai; falling back to the origin", src.ID())
		return "", false
	}
	return p, true
}
