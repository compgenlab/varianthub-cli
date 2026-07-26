package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyTempDir(t *testing.T) {
	orig, had := os.LookupEnv("TMPDIR")
	t.Cleanup(func() {
		if had {
			os.Setenv("TMPDIR", orig)
		} else {
			os.Unsetenv("TMPDIR")
		}
	})

	// A non-existent nested dir is created and TMPDIR points at its absolute path.
	dir := filepath.Join(t.TempDir(), "scratch", "sub")
	if err := applyTempDir(dir); err != nil {
		t.Fatalf("applyTempDir: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("temp dir not created: %v", err)
	}
	abs, _ := filepath.Abs(dir)
	if got := os.Getenv("TMPDIR"); got != abs {
		t.Errorf("TMPDIR = %q, want %q", got, abs)
	}
	// os.MkdirTemp("", …) now lands under the chosen base.
	td, err := os.MkdirTemp("", "x-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(td)
	if !strings.HasPrefix(td, abs) {
		t.Errorf("MkdirTemp landed at %q, not under %q", td, abs)
	}

	// An empty dir is a no-op (leaves TMPDIR unchanged).
	prev := os.Getenv("TMPDIR")
	if err := applyTempDir(""); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TMPDIR") != prev {
		t.Errorf("empty dir changed TMPDIR to %q", os.Getenv("TMPDIR"))
	}
}

func TestFirstNonEmpty(t *testing.T) {
	if got := firstNonEmpty("", "", "b", "c"); got != "b" {
		t.Errorf("got %q, want b", got)
	}
	if got := firstNonEmpty("", ""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
