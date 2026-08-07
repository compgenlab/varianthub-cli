package faidx

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compgenlab/cghts/htsio/bgzf"
	"github.com/compgenlab/cghts/seqio"
)

const fastaBody = ">chr1 test sequence\n" +
	"ACGTACGTAC\nGTACGTACGT\nACGT\n" +
	">chr2\n" +
	"TTTTAAAACC\nGGGG\n"

// The indexes are only worth writing if a reader accepts them, so this asserts
// against cghts's own consumers rather than against our idea of the format.
func TestBuildPlainFasta(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ref.fa")
	if err := os.WriteFile(p, []byte(fastaBody), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := Build(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("indexed %d sequences, want 2", len(entries))
	}
	if entries[0].Name != "chr1" || entries[0].Length != 24 {
		t.Errorf("chr1: %+v (want name chr1, length 24)", entries[0])
	}
	if entries[1].Name != "chr2" || entries[1].Length != 14 {
		t.Errorf("chr2: %+v (want name chr2, length 14)", entries[1])
	}

	// The test that matters: a reader can use it, and returns the right bases.
	r, err := seqio.NewIndexedFastaReader(p)
	if err != nil {
		t.Fatalf("cghts rejected our .fai: %v", err)
	}
	defer r.Close()
	got, err := r.GetSequenceRange("chr1", 0, 4)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ToUpper(string(got)) != "ACGT" {
		t.Errorf("chr1:0-4 = %q, want ACGT", got)
	}
	// Across a line boundary, which is where a wrong LineWidth shows up.
	if got, err = r.GetSequenceRange("chr2", 8, 12); err != nil {
		t.Fatal(err)
	}
	if strings.ToUpper(string(got)) != "CCGG" {
		t.Errorf("chr2:8-12 = %q, want CCGG (line boundary)", got)
	}
}

// A BGZF FASTA needs a .gzi as well, and the .fai offsets stay uncompressed.
func TestBuildBGZF(t *testing.T) {
	bin := os.Getenv("VARHUB_BIN")
	if bin == "" {
		bin = "../../bin/varhub"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("no varhub binary at %s; build it to exercise bgzip", bin)
	}
	dir := t.TempDir()
	plain := filepath.Join(dir, "ref.fa")
	if err := os.WriteFile(plain, []byte(fastaBody), 0o644); err != nil {
		t.Fatal(err)
	}
	gz := filepath.Join(dir, "ref.fa.gz")
	out, err := os.Create(gz)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "bgzip", plain)
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		out.Close()
		t.Fatalf("varhub bgzip: %v", err)
	}
	out.Close()

	entries, err := Build(gz)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("indexed %d sequences, want 2", len(entries))
	}
	// Offsets are into the uncompressed stream, so they match the plain file.
	plainEntries, err := Build(plain)
	if err != nil {
		t.Fatal(err)
	}
	for i := range entries {
		if entries[i].Offset != plainEntries[i].Offset {
			t.Errorf("%s: bgzf offset %d != plain offset %d — .fai offsets must be "+
				"uncompressed", entries[i].Name, entries[i].Offset, plainEntries[i].Offset)
		}
	}
	if _, err := os.Stat(gz + ".gzi"); err != nil {
		t.Fatalf("no .gzi written: %v", err)
	}
	if _, err := bgzf.LoadGZIndex(gz + ".gzi"); err != nil {
		t.Fatalf("cghts rejected our .gzi: %v", err)
	}
}

// Plain gzip is refused rather than indexed: a .fai addresses uncompressed
// offsets, and nothing can seek to one in a plain gzip stream, so the index
// would be unusable rather than merely imperfect.
func TestBuildRefusesPlainGzip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ref.fa.gz")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	// compress/gzip writes no BGZF extra field.
	zw := gzip.NewWriter(f)
	if _, err := zw.Write([]byte(fastaBody)); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	f.Close()

	if _, err := Build(p); err == nil {
		t.Fatal("a plain-gzip FASTA was accepted; its index would be unusable")
	} else if !strings.Contains(err.Error(), "bgzip") {
		t.Errorf("error does not say what to do about it: %v", err)
	}
}

// oracle returns the samtools binary to compare against, or skips.
//
// Our .fai and .gzi are only correct if htslib's readers accept them, and the
// cheapest proof of that is producing byte-identical output to htslib's own
// writer. Skipped rather than failed when samtools is absent, so the package
// still builds and tests anywhere — but the comparison is the point, so the
// skip says so.
func oracle(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("samtools")
	if err != nil {
		t.Skip("samtools not installed; skipping the htslib comparison " +
			"(the other tests only prove cghts accepts our output, not that it matches htslib)")
	}
	return p
}

// A FASTA with several records, a short final line in each, and differing line
// widths between records — the shapes where a hand-written .fai goes wrong.
const oracleFasta = ">chr1 with a description\n" +
	"ACGTACGTACGTACGTACGT\nACGTACGTACGTACGTACGT\nACGTACGT\n" +
	">chr2\n" +
	"TTTTAAAACCCCGGGG\nTTTTAAAACCCCGGGG\nTTTT\n" +
	">chr3\n" +
	"NNNNNNNNNN\n"

func TestFaiMatchesHtslib(t *testing.T) {
	sam := oracle(t)

	// Separate directories: samtools writes its index beside the file, and so do we.
	ours, theirs := t.TempDir(), t.TempDir()
	for _, d := range []string{ours, theirs} {
		if err := os.WriteFile(filepath.Join(d, "ref.fa"), []byte(oracleFasta), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if out, err := exec.Command(sam, "faidx", filepath.Join(theirs, "ref.fa")).CombinedOutput(); err != nil {
		t.Fatalf("samtools faidx: %v\n%s", err, out)
	}
	if _, err := Build(filepath.Join(ours, "ref.fa")); err != nil {
		t.Fatal(err)
	}

	want, err := os.ReadFile(filepath.Join(theirs, "ref.fa.fai"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(ours, "ref.fa.fai"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf(".fai differs from htslib.\n--- ours ---\n%s\n--- samtools ---\n%s", got, want)
	}
}

// The same for a BGZF FASTA, where htslib writes both .fai and .gzi.
//
// Both index the *same* compressed file, so the block boundaries are fixed and
// the .gzi must agree exactly — this catches an off-by-one in the block walk
// that a "does it load" check would not.
// bigFasta is large enough to span several BGZF blocks, so the .gzi carries
// real entries. A small fixture only ever proves the count is zero.
func bigFasta() string {
	var b strings.Builder
	line := strings.Repeat("ACGT", 15) // 60 bases
	for c := 1; c <= 3; c++ {
		fmt.Fprintf(&b, ">chr%d some description\n", c)
		for i := 0; i < 1500; i++ { // ~90 KB per record
			b.WriteString(line)
			b.WriteByte('\n')
		}
		b.WriteString("ACGTACGT\n") // short final line
	}
	return b.String()
}

func TestFaiAndGziMatchHtslibForBGZF(t *testing.T) {
	sam := oracle(t)
	bin := os.Getenv("VARHUB_BIN")
	if bin == "" {
		bin = "../../bin/varhub"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("no varhub binary at %s", bin)
	}

	src := t.TempDir()
	plain := filepath.Join(src, "ref.fa")
	if err := os.WriteFile(plain, []byte(bigFasta()), 0o644); err != nil {
		t.Fatal(err)
	}
	gzBytes := filepath.Join(src, "ref.fa.gz")
	out, err := os.Create(gzBytes)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "bgzip", plain)
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		out.Close()
		t.Fatalf("varhub bgzip: %v", err)
	}
	out.Close()
	body, err := os.ReadFile(gzBytes)
	if err != nil {
		t.Fatal(err)
	}

	ours, theirs := t.TempDir(), t.TempDir()
	for _, d := range []string{ours, theirs} {
		if err := os.WriteFile(filepath.Join(d, "ref.fa.gz"), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if o, err := exec.Command(sam, "faidx", filepath.Join(theirs, "ref.fa.gz")).CombinedOutput(); err != nil {
		t.Fatalf("samtools faidx (bgzf): %v\n%s", err, o)
	}
	if _, err := Build(filepath.Join(ours, "ref.fa.gz")); err != nil {
		t.Fatal(err)
	}

	for _, ext := range []string{".fai", ".gzi"} {
		want, err := os.ReadFile(filepath.Join(theirs, "ref.fa.gz"+ext))
		if err != nil {
			t.Fatalf("samtools wrote no %s: %v", ext, err)
		}
		got, err := os.ReadFile(filepath.Join(ours, "ref.fa.gz"+ext))
		if err != nil {
			t.Fatalf("we wrote no %s: %v", ext, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from htslib (ours %d bytes, samtools %d bytes)\n"+
				"ours:     %x\nsamtools: %x", ext, len(got), len(want), got, want)
		}
	}
}

// A short line ends a record, so anything after one is ragged whatever its
// length. Checking pairwise lengths instead lets 8/3/8 through, and a .fai
// computed from it gives wrong coordinates rather than an error.
func TestBuildRejectsRaggedLines(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ragged.fa")
	if err := os.WriteFile(p, []byte(">chr1\nACGTACGT\nACG\nACGTACGT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(p); err == nil {
		t.Error("uneven line lengths were accepted")
	}
}
