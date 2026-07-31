package objstore

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/compgenlab/cghts/htsio/tabix"
)

// The read path end to end: a tabix query against an object returns exactly what
// the same file returns from disk.
func TestOpenTabixMatchesLocal(t *testing.T) {
	s, bucket := testStore(t)
	ctx := context.Background()

	dir := t.TempDir()
	local := filepath.Join(dir, "src.vcf.gz")
	w := tabix.NewWriter(local, tabix.NewWriterOpts().VCF().AutoIndex())
	w.WriteHeader("##fileformat=VCFv4.2")
	w.WriteHeader("#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO")
	for _, chrom := range []string{"chr1", "chr17"} {
		for pos := 1; pos <= 5000; pos++ {
			if err := w.Write(chrom + "\t" + itoa(pos*10) + "\t.\tA\tG\t.\t.\tDP=" + itoa(pos)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	prefix := uniqueKey(t, "tabix")
	dataRef := Ref{Bucket: bucket, Key: prefix + "/src.vcf.gz"}
	idxRef := Ref{Bucket: bucket, Key: prefix + "/src.vcf.gz.tbi"}
	t.Cleanup(func() { s.Remove(ctx, dataRef); s.Remove(ctx, idxRef) })
	if err := s.PutFile(ctx, dataRef, local, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.PutFile(ctx, idxRef, local+".tbi", ""); err != nil {
		t.Fatal(err)
	}

	lr, err := tabix.NewReader(local)
	if err != nil {
		t.Fatal(err)
	}
	defer lr.Close()

	rr, err := OpenTabix(ctx, dataRef.String())
	if err != nil {
		t.Fatalf("OpenTabix: %v", err)
	}
	defer rr.Close()

	for _, q := range []struct {
		ref        string
		start, end int
	}{
		{"chr1", 1000, 2000},
		{"chr17", 40000, 41000},
		{"chr1", 49990, 50010},
	} {
		want := query(t, lr, q.ref, q.start, q.end)
		got := query(t, rr, q.ref, q.start, q.end)
		if len(want) == 0 {
			t.Fatalf("%s:%d-%d: local returned nothing; the comparison proves nothing", q.ref, q.start, q.end)
		}
		if len(want) != len(got) {
			t.Fatalf("%s:%d-%d: got %d records, local gave %d", q.ref, q.start, q.end, len(got), len(want))
		}
		for i := range want {
			if want[i] != got[i] {
				t.Fatalf("%s:%d-%d record %d differs\n  s3: %s\n loc: %s",
					q.ref, q.start, q.end, i, got[i], want[i])
			}
		}
	}
}

// A missing index must fail with a message naming the problem, not with a
// confusing read error partway through the first query.
func TestOpenTabixWithoutIndex(t *testing.T) {
	s, bucket := testStore(t)
	ctx := context.Background()

	dir := t.TempDir()
	local := filepath.Join(dir, "noidx.vcf.gz")
	w := tabix.NewWriter(local, tabix.NewWriterOpts().VCF().AutoIndex())
	w.WriteHeader("#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO")
	if err := w.Write("chr1\t100\t.\tA\tG\t.\t.\t."); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	ref := Ref{Bucket: bucket, Key: uniqueKey(t, "noidx") + "/noidx.vcf.gz"}
	t.Cleanup(func() { s.Remove(ctx, ref) })
	if err := s.PutFile(ctx, ref, local, ""); err != nil { // data only, no sidecar
		t.Fatal(err)
	}

	_, err := OpenTabix(ctx, ref.String())
	if err == nil {
		t.Fatal("opening an unindexed object succeeded")
	}
	if !contains(err.Error(), "no index") {
		t.Errorf("error does not name the missing index: %v", err)
	}
}

func query(t *testing.T, r *tabix.Reader, ref string, start, end int) []string {
	t.Helper()
	seq, err := r.Query(ref, start, end)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rec, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, rec.Line)
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
