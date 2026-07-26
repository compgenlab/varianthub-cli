package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/compgenlab/cghts/htsio/tabix"
)

const tabixVCF = "##fileformat=VCFv4.2\n" +
	"#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO\n" +
	"chr1\t100\t.\tA\tG\t.\t.\t.\n" +
	"chr1\t200\t.\tC\tT\t.\t.\t.\n" +
	"chr2\t50\t.\tG\tA\t.\t.\t.\n"

// TestBgzipAndTabix: `varhub bgzip` compresses a file to a BGZF file, and `varhub tabix`
// indexes it; the result opens as a valid tabix-indexed file.
func TestBgzipAndTabix(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.vcf")
	if err := os.WriteFile(in, []byte(tabixVCF), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.vcf.gz")

	if err := cmdBgzip([]string{"-o", out, in}); err != nil {
		t.Fatalf("bgzip: %v", err)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("bgzf output missing: %v", err)
	}
	// No index yet (plain compress).
	if _, err := os.Stat(out + ".tbi"); err == nil {
		t.Error("plain bgzip should not have written an index")
	}
	if err := cmdTabix([]string{"-p", "vcf", out}); err != nil {
		t.Fatalf("tabix: %v", err)
	}
	if _, err := os.Stat(out + ".tbi"); err != nil {
		t.Errorf("tabix index missing: %v", err)
	}
	r, err := tabix.NewReader(out)
	if err != nil {
		t.Fatalf("open indexed file: %v", err)
	}
	r.Close()
}

// TestBgzipMergedIndex: `varhub bgzip -o FILE -p vcf` writes the BGZF file AND its
// tabix index in one step; explicit columns work too.
func TestBgzipMergedIndex(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in.vcf")
	if err := os.WriteFile(in, []byte(tabixVCF), 0o644); err != nil {
		t.Fatal(err)
	}

	// preset form
	out := filepath.Join(dir, "preset.vcf.gz")
	if err := cmdBgzip([]string{"-o", out, "-p", "vcf", in}); err != nil {
		t.Fatalf("bgzip -p vcf: %v", err)
	}
	if _, err := os.Stat(out + ".tbi"); err != nil {
		t.Errorf("merged preset index missing: %v", err)
	}

	// explicit-columns form (chrom=1, pos=2, end=2, skip the header/comment)
	out2 := filepath.Join(dir, "cols.vcf.gz")
	if err := cmdBgzip([]string{"-o", out2, "-s", "1", "-b", "2", "-e", "2", "-c", "#", in}); err != nil {
		t.Fatalf("bgzip explicit cols: %v", err)
	}
	if r, err := tabix.NewReader(out2); err != nil {
		t.Errorf("open cols-indexed file: %v", err)
	} else {
		r.Close()
	}

	// index requested without -o is an error (can't index a stream).
	if err := cmdBgzip([]string{"-p", "vcf", in}); err == nil {
		t.Error("expected error: index without -o")
	}
}
