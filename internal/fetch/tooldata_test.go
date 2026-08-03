package fetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
