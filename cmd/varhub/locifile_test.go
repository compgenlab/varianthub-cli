package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "loci.txt")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestReadLociFile(t *testing.T) {
	got, err := readLociFile(writeTemp(t, `# a batch
chr17:7676154:C:T

  chr7:140753336:A:T
# trailing comment
chr1:1005000:C:T
`))
	if err != nil {
		t.Fatalf("readLociFile: %v", err)
	}
	want := []string{"chr17:7676154:C:T", "chr7:140753336:A:T", "chr1:1005000:C:T"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("locus %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// The whole reason this exists: a batch far past what argv can carry. 200k loci
// is ~4 MB of text, which as arguments would fail the exec rather than the
// parse — and with an error naming neither.
func TestReadLociFileHandlesABatchArgvCouldNot(t *testing.T) {
	var b strings.Builder
	const n = 200_000
	for i := 0; i < n; i++ {
		b.WriteString("chr1:")
		b.WriteString(strings.Repeat("1", 6))
		b.WriteString(":A:T\n")
	}
	if b.Len() < 2<<20 {
		t.Fatalf("fixture is only %d bytes; it needs to be past a plausible ARG_MAX", b.Len())
	}
	got, err := readLociFile(writeTemp(t, b.String()))
	if err != nil {
		t.Fatalf("readLociFile: %v", err)
	}
	if len(got) != n {
		t.Errorf("read %d loci, want %d", len(got), n)
	}
}

// An empty file is a mistake worth naming: the run would otherwise proceed to
// "no loci" from a source the user believed had some.
func TestReadLociFileRejectsAnEmptyFile(t *testing.T) {
	for _, body := range []string{"", "\n\n\n", "# only comments\n"} {
		if _, err := readLociFile(writeTemp(t, body)); err == nil {
			t.Errorf("an empty file (%q) was accepted", body)
		}
	}
}

func TestReadLociFileMissing(t *testing.T) {
	_, err := readLociFile(filepath.Join(t.TempDir(), "nope.txt"))
	if err == nil {
		t.Fatal("a missing file was accepted")
	}
	if !strings.Contains(err.Error(), "loci-file") {
		t.Errorf("error %q does not name the flag", err)
	}
}
