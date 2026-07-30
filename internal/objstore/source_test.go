package objstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/compgenlab/cghts/htsio/tabix"
)

// TestSourceReadAtContract pins the io.ReaderAt contract, which the BGZF layer
// depends on and which is easy to get subtly wrong over HTTP: a short read at
// the end must come back with io.EOF rather than a nil error.
func TestSourceReadAtContract(t *testing.T) {
	s, bucket := testStore(t)
	ctx := context.Background()
	ref := Ref{Bucket: bucket, Key: uniqueKey(t, "readat")}
	t.Cleanup(func() { s.Remove(ctx, ref) })

	body := make([]byte, 10000)
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(t.TempDir(), "d.bin")
	if err := os.WriteFile(local, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.PutFile(ctx, ref, local, ""); err != nil {
		t.Fatal(err)
	}

	src, err := s.Open(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()

	if got, err := src.Size(); err != nil || got != int64(len(body)) {
		t.Fatalf("Size = %d, %v; want %d", got, err, len(body))
	}

	// A read wholly inside the object.
	p := make([]byte, 100)
	n, err := src.ReadAt(p, 500)
	if err != nil || n != 100 {
		t.Fatalf("mid read = %d, %v", n, err)
	}
	if !bytes.Equal(p, body[500:600]) {
		t.Error("mid read returned the wrong bytes")
	}

	// A read that runs off the end must return what exists plus io.EOF.
	p = make([]byte, 100)
	n, err = src.ReadAt(p, int64(len(body)-40))
	if n != 40 {
		t.Errorf("tail read returned %d bytes, want 40", n)
	}
	if err != io.EOF {
		t.Errorf("tail read err = %v, want io.EOF", err)
	}
	if !bytes.Equal(p[:40], body[len(body)-40:]) {
		t.Error("tail read returned the wrong bytes")
	}

	// A read starting past the end is io.EOF, not a failure.
	if n, err := src.ReadAt(make([]byte, 10), int64(len(body))+100); n != 0 || err != io.EOF {
		t.Errorf("past-end read = %d, %v; want 0, io.EOF", n, err)
	}

	// Zero-length reads are a no-op.
	if n, err := src.ReadAt(nil, 0); n != 0 || err != nil {
		t.Errorf("empty read = %d, %v", n, err)
	}
}

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

	rr, err := OpenTabix(ctx, s, dataRef.String())
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

	_, err := OpenTabix(ctx, s, ref.String())
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

// countingHTTP records what actually went over the wire.
type countingHTTP struct {
	inner   *http.Client
	mu      sync.Mutex
	body    int64
	ranged  int
	fullGET int
}

func (c *countingHTTP) Do(req *http.Request) (*http.Response, error) {
	isGet := req.Method == http.MethodGet
	hasRange := req.Header.Get("Range") != ""
	resp, err := c.inner.Do(req)
	if err != nil {
		return resp, err
	}
	c.mu.Lock()
	if isGet {
		if hasRange {
			c.ranged++
		} else {
			c.fullGET++
		}
	}
	c.mu.Unlock()
	resp.Body = &countingBody{ReadCloser: resp.Body, c: c}
	return resp, nil
}

type countingBody struct {
	io.ReadCloser
	c *countingHTTP
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.c.mu.Lock()
	b.c.body += int64(n)
	b.c.mu.Unlock()
	return n, err
}

// The claim correctness cannot make: a query must fetch byte ranges, not the
// object. A silent fallback to a whole-object GET returns the right records and
// would pass every other test here while defeating the entire point.
func TestQueryIssuesRangedGetsOnly(t *testing.T) {
	if os.Getenv("VARHUB_TEST_S3_BUCKET") == "" {
		t.Skip("set VARHUB_TEST_S3_BUCKET (and AWS_ENDPOINT_URL) to run S3 integration tests")
	}
	ctx := context.Background()

	counter := &countingHTTP{inner: &http.Client{}}
	s, err := NewS3(ctx, WithHTTPClient(counter))
	if err != nil {
		t.Fatal(err)
	}
	bucket := os.Getenv("VARHUB_TEST_S3_BUCKET")

	// Large enough that a full GET is unmistakable next to one narrow query.
	dir := t.TempDir()
	local := filepath.Join(dir, "big.vcf.gz")
	w := tabix.NewWriter(local, tabix.NewWriterOpts().VCF().AutoIndex())
	w.WriteHeader("##fileformat=VCFv4.2")
	w.WriteHeader("#CHROM\tPOS\tID\tREF\tALT\tQUAL\tFILTER\tINFO")
	for _, chrom := range []string{"chr1", "chr2"} {
		for pos := 1; pos <= 200000; pos++ {
			if err := w.Write(chrom + "\t" + itoa(pos*10) + "\t.\tA\tG\t.\t.\tDP=" + itoa(pos) + ";PAD=xxxxxxxxxxxxxxxxxxxx"); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(local)
	if err != nil {
		t.Fatal(err)
	}

	prefix := uniqueKey(t, "ranged")
	dataRef := Ref{Bucket: bucket, Key: prefix + "/big.vcf.gz"}
	idxRef := Ref{Bucket: bucket, Key: prefix + "/big.vcf.gz.tbi"}
	t.Cleanup(func() { s.Remove(ctx, dataRef); s.Remove(ctx, idxRef) })
	if err := s.PutFile(ctx, dataRef, local, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.PutFile(ctx, idxRef, local+".tbi", ""); err != nil {
		t.Fatal(err)
	}

	r, err := OpenTabix(ctx, s, dataRef.String())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	// Reset after open: fetching the index whole is expected and fine.
	counter.mu.Lock()
	counter.body, counter.ranged, counter.fullGET = 0, 0, 0
	counter.mu.Unlock()

	got := query(t, r, "chr2", 1000000, 1001000)
	if len(got) == 0 {
		t.Fatal("query returned nothing; the measurement would be meaningless")
	}

	counter.mu.Lock()
	body, ranged, full := counter.body, counter.ranged, counter.fullGET
	counter.mu.Unlock()

	if full > 0 {
		t.Errorf("%d whole-object GET(s) during a region query; reads must be ranged", full)
	}
	if ranged == 0 {
		t.Error("no ranged GETs issued")
	}
	if limit := fi.Size() / 10; body > limit {
		t.Errorf("query transferred %d bytes of a %d-byte object (>%d)", body, fi.Size(), limit)
	}
	t.Logf("%d records via %d ranged GETs, %d bytes of %d (%.2f%%)",
		len(got), ranged, body, fi.Size(), 100*float64(body)/float64(fi.Size()))
}
