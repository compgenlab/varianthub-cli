package htsidx

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/cghts/htsio/bgzf"
	"github.com/compgenlab/cghts/htsio/tabix"
)

// writeBGZF writes lines to a bgzip file verbatim — deliberately not through
// tabix.Writer, which sorts internally and so could never produce the unsorted
// input these tests are about.
func writeBGZF(t *testing.T, dir, name string, lines ...string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	w, err := bgzf.NewBGZipFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if _, err := w.Write([]byte(l + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return p
}

func vcfOpts() *tabix.WriterOpts { return tabix.NewWriterOpts().VCF() }

func hasIndex(p string) bool {
	for _, ext := range []string{".tbi", ".csi"} {
		if _, err := os.Stat(p + ext); err == nil {
			return true
		}
	}
	return false
}

// Equal start positions are legal: a multiallelic site split across lines is
// the normal shape of a VCF, not a sorting error.
func TestEqualPositionsAreSorted(t *testing.T) {
	dir := t.TempDir()
	p := writeBGZF(t, dir, "ok.vcf.gz",
		"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO",
		"chr1\t100\t.\tA\tG\t.\t.\t.",
		"chr1\t100\t.\tA\tT\t.\t.\t.", // same position, second ALT
		"chr1\t200\t.\tC\tT\t.\t.\t.",
		"chr2\t50\t.\tG\tA\t.\t.\t.",
	)
	if err := WriteIndex(vcfOpts(), p); err != nil {
		t.Fatalf("WriteIndex on sorted input: %v", err)
	}
	if !hasIndex(p) {
		t.Error("no index written for valid input")
	}
}

func TestDescendingPositionsRejected(t *testing.T) {
	dir := t.TempDir()
	p := writeBGZF(t, dir, "desc.vcf.gz",
		"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO",
		"chr1\t500\t.\tA\tG\t.\t.\t.",
		"chr1\t100\t.\tC\tT\t.\t.\t.", // goes backwards
	)
	err := WriteIndex(vcfOpts(), p)
	if err == nil {
		t.Fatal("WriteIndex accepted descending positions")
	}
	var u *tabix.UnsortedError
	if !errors.As(err, &u) {
		t.Fatalf("error does not unwrap to *tabix.UnsortedError: %v", err)
	}
	if u.Revisit {
		t.Error("Revisit set for a descending-position error")
	}
	if !strings.Contains(err.Error(), "positions must ascend") {
		t.Errorf("missing the ascending-position hint: %v", err)
	}
	if hasIndex(p) {
		t.Error("an index survived a failed build")
	}
}

func TestInterleavedContigsRejected(t *testing.T) {
	dir := t.TempDir()
	p := writeBGZF(t, dir, "weave.vcf.gz",
		"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO",
		"chr1\t100\t.\tA\tG\t.\t.\t.",
		"chr2\t100\t.\tA\tG\t.\t.\t.",
		"chr1\t200\t.\tA\tG\t.\t.\t.", // chr1 resumes after chr2
	)
	err := WriteIndex(vcfOpts(), p)
	if err == nil {
		t.Fatal("WriteIndex accepted interleaved contigs")
	}
	var u *tabix.UnsortedError
	if !errors.As(err, &u) {
		t.Fatalf("error does not unwrap to *tabix.UnsortedError: %v", err)
	}
	if !u.Revisit {
		t.Error("Revisit not set for a contig-revisit error")
	}
	if !strings.Contains(err.Error(), "contiguous block") {
		t.Errorf("missing the contiguous-block hint: %v", err)
	}
}

// The reason this package exists: a failed rebuild must not leave the previous
// index in place. A stale index is structurally valid, so every presence check
// in varhub passes and queries then resolve to the wrong offsets.
func TestStaleIndexRemovedOnFailure(t *testing.T) {
	dir := t.TempDir()
	p := writeBGZF(t, dir, "stale.vcf.gz",
		"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO",
		"chr1\t100\t.\tA\tG\t.\t.\t.",
		"chr1\t200\t.\tC\tT\t.\t.\t.",
	)
	if err := WriteIndex(vcfOpts(), p); err != nil {
		t.Fatalf("initial index: %v", err)
	}
	if !hasIndex(p) {
		t.Fatal("initial index missing")
	}

	// Same path, now holding unsorted content — the shape of a source whose
	// upstream published a new, unsorted release.
	bad := writeBGZF(t, dir, "stale.vcf.gz",
		"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO",
		"chr1\t500\t.\tA\tG\t.\t.\t.",
		"chr1\t100\t.\tC\tT\t.\t.\t.",
	)
	err := WriteIndex(vcfOpts(), bad)
	if err == nil {
		t.Fatal("rebuild over unsorted content succeeded")
	}
	if hasIndex(bad) {
		t.Error("the stale index survived — a later run would reuse it and read wrong offsets")
	}
	if !strings.Contains(err.Error(), "was removed") {
		t.Errorf("removal not reported to the user: %v", err)
	}
}

// A non-sorting failure must pass through unchanged rather than being dressed
// up with irrelevant sorting advice.
func TestUnrelatedErrorPassesThrough(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.vcf.gz")
	err := WriteIndex(vcfOpts(), missing)
	if err == nil {
		t.Fatal("indexing a missing file succeeded")
	}
	if strings.Contains(err.Error(), "Sort before indexing") {
		t.Errorf("sorting advice attached to an unrelated error: %v", err)
	}
}
