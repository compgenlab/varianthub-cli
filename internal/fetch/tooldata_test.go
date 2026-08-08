package fetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/objstore"
)

// A tool's data dir survives the round trip: contents, layout, and the exec bit
// a setup step may have set on something it installed.
func TestToolDataRoundTrip(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "homo_sapiens", "115_GRCh38"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]os.FileMode{
		"homo_sapiens/115_GRCh38/1.cache": 0o644,
		"bin/run.sh":                      0o755,
	}
	for name, mode := range files {
		p := filepath.Join(src, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("content of "+name), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("1.cache", filepath.Join(src, "homo_sapiens", "latest.cache")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := writeTarGz(&buf, src); err != nil {
		t.Fatalf("writeTarGz: %v", err)
	}
	arc := filepath.Join(t.TempDir(), "a.tar.gz")
	if err := os.WriteFile(arc, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "restored")
	if err := extractTarGz(arc, dest); err != nil {
		t.Fatalf("extractTarGz: %v", err)
	}
	for name, mode := range files {
		p := filepath.Join(dest, name)
		body, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if string(body) != "content of "+name {
			t.Errorf("%s = %q", name, body)
		}
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		// The exec bit matters: a restored setup whose scripts lost it fails at
		// run time with a permission error that says nothing about the cause.
		if fi.Mode().Perm()&0o100 != mode&0o100 {
			t.Errorf("%s mode = %04o, want the exec bit to match %04o", name, fi.Mode().Perm(), mode)
		}
	}
	// A symlink stays a symlink rather than being inlined.
	if fi, err := os.Lstat(filepath.Join(dest, "homo_sapiens", "latest.cache")); err != nil {
		t.Fatal(err)
	} else if fi.Mode()&os.ModeSymlink == 0 {
		t.Error("symlink was not preserved")
	}
}

// The archive arrives over the network from a shared bucket. A "../.." entry is
// the oldest trick there is, and unpacking one would write outside the tool's
// directory.
func TestExtractRefusesEscapes(t *testing.T) {
	for _, name := range []string{"../escaped.txt", "sub/../../escaped.txt", "/abs/escaped.txt"} {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		tw := tar.NewWriter(gz)
		body := []byte("nope")
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		tw.Write(body)
		tw.Close()
		gz.Close()

		arc := filepath.Join(t.TempDir(), "evil.tar.gz")
		if err := os.WriteFile(arc, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		dest := filepath.Join(t.TempDir(), "dest")
		err := extractTarGz(arc, dest)
		if name == "/abs/escaped.txt" {
			// An absolute path is cleaned into the destination rather than
			// escaping it, which is safe; just confirm nothing landed outside.
			if _, sErr := os.Stat("/abs/escaped.txt"); sErr == nil {
				t.Fatal("an absolute entry wrote outside the destination")
			}
			continue
		}
		if err == nil {
			t.Errorf("extract accepted %q", name)
		} else if !strings.Contains(err.Error(), "escapes") {
			t.Errorf("extract(%q) = %v; want an escape error", name, err)
		}
	}
}

func TestSafeJoin(t *testing.T) {
	dest := "/tmp/dest"
	for _, ok := range []string{"a.txt", "sub/a.txt", "./a.txt"} {
		if _, err := safeJoin(dest, ok); err != nil {
			t.Errorf("safeJoin(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{"../a.txt", "sub/../../a.txt"} {
		if _, err := safeJoin(dest, bad); err == nil {
			t.Errorf("safeJoin(%q) accepted", bad)
		}
	}
	// A sibling directory sharing a prefix must not pass as "under" it.
	if _, err := safeJoin("/tmp/dest", "../destevil/a.txt"); err == nil {
		t.Error("safeJoin accepted a prefix-sharing sibling")
	}
}

// A failed restore must leave an existing data directory alone.
//
// It used to remove datadir on a bad unpack. That is safe only when this process
// is the sole reader — and the case the lock exists for is exactly the one where
// it isn't: a shared directory where deleting it takes out an install another
// process is using. The failure here should cost this attempt and nothing else.
func TestFailedRestoreLeavesExistingDataIntact(t *testing.T) {
	s := useStub(t)
	base := t.TempDir()
	cfg := &config.Config{DataDir: base, CacheDir: base}
	cfg.SetBaseDir(base)

	tool := config.Tool{Name: "vep", Version: "115", ToolCache: "s3://bucket/tools"}
	obj := toolDataObject(cfg, tool)
	if obj == "" {
		t.Fatal("tool has no archive object")
	}
	// Not a gzip stream, so extraction fails partway through the restore.
	ref, err := objstore.Parse(obj)
	if err != nil {
		t.Fatal(err)
	}
	s.objects[ref.String()] = []byte("this is not a tarball")

	// A complete install already on disk, as a second worker would have left it.
	datadir := filepath.Join(base, "installed")
	if err := os.MkdirAll(filepath.Join(datadir, "homo_sapiens"), 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(datadir, "homo_sapiens", "1.cache")
	if err := os.WriteFile(keep, []byte("hours of work"), 0o644); err != nil {
		t.Fatal(err)
	}

	if restoreToolData(context.Background(), cfg, tool, datadir, func(string, ...any) {}) {
		t.Fatal("restore reported success on a corrupt archive")
	}

	body, err := os.ReadFile(keep)
	if err != nil {
		t.Fatalf("the existing install was destroyed by a failed restore: %v", err)
	}
	if string(body) != "hours of work" {
		t.Errorf("existing file = %q, want it untouched", body)
	}
	// And no staging directory left lying around.
	if _, err := os.Stat(datadir + ".restoring"); !os.IsNotExist(err) {
		t.Errorf("staging directory survived the failure: %v", err)
	}
}

// The live directory is never partially populated: it goes from absent to
// complete. A reader that catches it mid-restore would otherwise find some of
// the tool's data and fail deep inside the tool.
func TestSuccessfulRestoreSwapsInAtomically(t *testing.T) {
	s := useStub(t)
	base := t.TempDir()
	cfg := &config.Config{DataDir: base, CacheDir: base}
	cfg.SetBaseDir(base)

	tool := config.Tool{Name: "vep", Version: "115", ToolCache: "s3://bucket/tools"}

	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "homo_sapiens"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "homo_sapiens", "1.cache"), []byte("restored"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writeTarGz(&buf, src); err != nil {
		t.Fatal(err)
	}
	ref, err := objstore.Parse(toolDataObject(cfg, tool))
	if err != nil {
		t.Fatal(err)
	}
	s.objects[ref.String()] = buf.Bytes()

	datadir := filepath.Join(base, "installed")
	if !restoreToolData(context.Background(), cfg, tool, datadir, func(string, ...any) {}) {
		t.Fatal("restore failed")
	}
	body, err := os.ReadFile(filepath.Join(datadir, "homo_sapiens", "1.cache"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "restored" {
		t.Errorf("content = %q", body)
	}
	if _, err := os.Stat(datadir + ".restoring"); !os.IsNotExist(err) {
		t.Errorf("staging directory survived a successful restore: %v", err)
	}
}
