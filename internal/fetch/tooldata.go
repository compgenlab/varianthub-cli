package fetch

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/objstore"
)

// Publishing and restoring a tool's setup output.
//
// A tool's setup can take hours — VEP's INSTALL.pl fetches a species cache — and
// produces a directory, not a file. That directory has to be on a filesystem at
// run time, because the tool opens files inside it by path, so an object store
// can hold an archive of it but never serve it directly.
//
// So: tar it once, keep the tarball where every machine can reach it, and unpack
// it on any machine that has none. Nothing here knows what tool it is handling;
// a source opts in with cache_setup and gets it.

// toolDataObject is where a tool's archived data lives, or "" when it is not
// archived at all.
//
// The destination is named explicitly rather than inherited from cache_dir, so
// a deployment can archive tool data to one bucket while its source files live
// in another — and so "archive this" and "archive it here" cannot disagree.
func toolDataObject(cfg *config.Config, t config.Tool) string {
	root := t.ToolCache
	if root == "" || !objstore.IsObject(root) {
		// A filesystem destination needs no archive: the directory is already
		// where another machine reading that path would look.
		return ""
	}
	// Under <name>/<version>/ like every other file a source owns. It used to
	// live at tooldata/<name>/<version>.tar.gz, which no listing recognised: the
	// storage browser attributes objects by that prefix, so tens of gigabytes of
	// VEP cache were invisible in the file list and missing from the metrics.
	return objstore.Join(root, t.Name, t.Version, "tooldata.tar.gz")
}

// publishToolData archives a tool's data dir and uploads it.
//
// Best effort by design: setup has already succeeded and the tool is usable on
// this machine. Failing the download because the copy did not upload would throw
// away hours of work that went right.
func publishToolData(ctx context.Context, cfg *config.Config, t config.Tool, datadir string, logf func(string, ...any)) {
	obj := toolDataObject(cfg, t)
	if obj == "" {
		return
	}
	logf("%s: archiving setup data", t.ID())
	tmp, err := os.CreateTemp("", "varhub-tooldata-*.tar.gz")
	if err != nil {
		logf("%s: could not archive setup data: %v", t.ID(), err)
		return
	}
	defer os.Remove(tmp.Name())

	if err := writeTarGz(tmp, datadir); err != nil {
		tmp.Close()
		logf("%s: could not archive setup data: %v", t.ID(), err)
		return
	}
	if err := tmp.Close(); err != nil {
		logf("%s: could not archive setup data: %v", t.ID(), err)
		return
	}
	if err := putObject(ctx, tmp.Name(), obj); err != nil {
		logf("%s: could not cache setup data: %v", t.ID(), err)
		return
	}
	logf("%s: setup data cached", t.ID())
}

// restoreToolData unpacks a tool's archived data, reporting whether it did.
//
// A failure here is not fatal either: the caller falls back to running setup,
// which is slower but always correct.
func restoreToolData(ctx context.Context, cfg *config.Config, t config.Tool, datadir string,
	logf func(string, ...any)) bool {

	obj := toolDataObject(cfg, t)
	if obj == "" || !objectExists(ctx, obj) {
		return false
	}
	logf("%s: restoring cached setup data", t.ID())
	tmp, err := os.CreateTemp("", "varhub-tooldata-*.tar.gz")
	if err != nil {
		return false
	}
	defer os.Remove(tmp.Name())
	tmp.Close()

	if err := fetchObject(ctx, obj, tmp.Name()); err != nil {
		logf("%s: could not fetch cached setup data: %v", t.ID(), err)
		return false
	}

	// Unpack beside the data directory and move it into place, rather than
	// unpacking into it.
	//
	// A half-unpacked directory is worse than none: the tool would find some of
	// its data and fail somewhere deep inside itself. Cleaning that up used to
	// mean removing datadir on error — which is safe only if this process is the
	// only one that can see it. It isn't: a deployment sharing the directory
	// between workers could have one delete the cache another was reading, and a
	// failure here would take out a working install rather than just this
	// attempt. Now a failure discards only the staging copy, and the live
	// directory is never in a partial state at all — it goes from absent to
	// complete in one rename.
	//
	// Callers hold the provisioning lock, so nothing else is staging or reading
	// through the swap.
	staging := datadir + ".restoring"
	_ = os.RemoveAll(staging) // an earlier attempt that died before cleaning up
	if err := extractTarGz(tmp.Name(), staging); err != nil {
		logf("%s: could not unpack cached setup data: %v", t.ID(), err)
		_ = os.RemoveAll(staging)
		return false
	}
	// Rename onto an existing directory fails, so clear whatever a previous
	// interrupted run left. Reached only when the sentinel is absent, meaning
	// nothing here is known-good.
	if err := os.RemoveAll(datadir); err != nil {
		logf("%s: could not clear the tool data directory: %v", t.ID(), err)
		_ = os.RemoveAll(staging)
		return false
	}
	if err := os.Rename(staging, datadir); err != nil {
		logf("%s: could not install cached setup data: %v", t.ID(), err)
		_ = os.RemoveAll(staging)
		return false
	}
	return true
}

// writeTarGz archives a directory tree.
//
// Regular files and directories only. A setup that produced a device node or a
// socket is not something to reproduce elsewhere, and a symlink is preserved as
// a symlink rather than followed — following one would silently inline whatever
// it points at, possibly from outside the tree.
func writeTarGz(w io.Writer, root string) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)

	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		var link string
		if fi.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(p); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// extractTarGz unpacks an archive under dest.
//
// Every entry's path is checked to stay under dest. The archive is one this
// deployment wrote, but it arrives over the network from a shared bucket, and a
// "../.." in a tar is the oldest trick there is.
func extractTarGz(path, dest string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()

	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target, err := safeJoin(dest, hdr.Name)
		if err != nil {
			return err
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)&os.ModePerm|0o700); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// The link target is checked too: a symlink pointing outside the
			// tree would let a later entry write through it.
			if _, err := safeJoin(dest, filepath.Join(filepath.Dir(hdr.Name), hdr.Linkname)); err != nil {
				return fmt.Errorf("symlink %s escapes the archive", hdr.Name)
			}
			_ = os.Remove(target)
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY,
				os.FileMode(hdr.Mode)&os.ModePerm|0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, tr); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		}
	}
}

// safeJoin resolves name under dest, refusing anything that escapes it.
func safeJoin(dest, name string) (string, error) {
	clean := filepath.Clean(filepath.Join(dest, filepath.FromSlash(name)))
	if clean != dest && !strings.HasPrefix(clean, dest+string(os.PathSeparator)) {
		return "", fmt.Errorf("archive entry %q escapes the destination", name)
	}
	return clean, nil
}
