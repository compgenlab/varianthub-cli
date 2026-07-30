package objstore

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// testStore connects to the S3-compatible endpoint named by the environment,
// skipping when there is none.
//
// Integration coverage runs against versitygw (or real S3) via AWS_ENDPOINT_URL.
// VARHUB_TEST_S3_BUCKET names a bucket the test may write into; without it these
// stay skipped rather than inventing a bucket on someone's account.
func testStore(t *testing.T) (*S3, string) {
	t.Helper()
	bucket := os.Getenv("VARHUB_TEST_S3_BUCKET")
	if bucket == "" {
		t.Skip("set VARHUB_TEST_S3_BUCKET (and AWS_ENDPOINT_URL for a local gateway) to run S3 integration tests")
	}
	s, err := NewS3(context.Background())
	if err != nil {
		t.Fatalf("NewS3: %v", err)
	}
	return s, bucket
}

// uniqueKey keeps parallel or repeated runs from colliding in a shared bucket.
func uniqueKey(t *testing.T, name string) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("varhub-test/%s-%x", name, b)
}

func TestS3RoundTrip(t *testing.T) {
	s, bucket := testStore(t)
	ctx := context.Background()
	ref := Ref{Bucket: bucket, Key: uniqueKey(t, "roundtrip")}
	t.Cleanup(func() { s.Remove(ctx, ref) })

	if _, ok, err := s.Stat(ctx, ref); err != nil || ok {
		t.Fatalf("Stat before upload = %v, %v; want not found", ok, err)
	}

	body := bytes.Repeat([]byte("varhub"), 1000)
	local := filepath.Join(t.TempDir(), "data.bin")
	if err := os.WriteFile(local, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.PutFile(ctx, ref, local, "md5:deadbeef"); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	obj, ok, err := s.Stat(ctx, ref)
	if err != nil || !ok {
		t.Fatalf("Stat after upload = %v, %v", ok, err)
	}
	if obj.Size != int64(len(body)) {
		t.Errorf("size = %d, want %d", obj.Size, len(body))
	}
	// The declared checksum rides along as metadata so a later run can tell
	// "already uploaded" from "upstream changed" without downloading.
	if obj.Checksum != "md5:deadbeef" {
		t.Errorf("checksum metadata = %q", obj.Checksum)
	}

	var got bytes.Buffer
	if err := s.Download(ctx, ref, &got); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !bytes.Equal(got.Bytes(), body) {
		t.Error("downloaded bytes differ from what was uploaded")
	}
}

// A file larger than the multipart threshold must arrive byte-identical, since
// that is the path every real source takes.
func TestS3MultipartRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("multipart upload moves a lot of bytes")
	}
	s, bucket := testStore(t)
	ctx := context.Background()
	ref := Ref{Bucket: bucket, Key: uniqueKey(t, "multipart")}
	t.Cleanup(func() { s.Remove(ctx, ref) })

	// Just over one part, so the upload genuinely splits.
	body := make([]byte, partSize+(1<<20))
	if _, err := rand.Read(body); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(local, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.PutFile(ctx, ref, local, ""); err != nil {
		t.Fatalf("PutFile: %v", err)
	}

	obj, ok, err := s.Stat(ctx, ref)
	if err != nil || !ok {
		t.Fatalf("Stat = %v, %v", ok, err)
	}
	if obj.Size != int64(len(body)) {
		t.Fatalf("size = %d, want %d", obj.Size, len(body))
	}
	var got bytes.Buffer
	if err := s.Download(ctx, ref, &got); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if !bytes.Equal(got.Bytes(), body) {
		t.Error("multipart upload did not round-trip byte-identically")
	}
}

func TestS3RemoveIsIdempotent(t *testing.T) {
	s, bucket := testStore(t)
	ctx := context.Background()
	ref := Ref{Bucket: bucket, Key: uniqueKey(t, "gone")}
	if err := s.Remove(ctx, ref); err != nil {
		t.Errorf("removing a missing object should be a no-op, got %v", err)
	}
}

// A failed upload must leave nothing a later run could mistake for a complete
// object. Pointing at a bucket that does not exist is the cheapest way to fail
// an upload without racing a real one.
func TestS3FailedUploadLeavesNothing(t *testing.T) {
	s, bucket := testStore(t)
	ctx := context.Background()
	local := filepath.Join(t.TempDir(), "x.bin")
	if err := os.WriteFile(local, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := Ref{Bucket: bucket + "-does-not-exist", Key: "varhub-test/nope"}
	if err := s.PutFile(ctx, bad, local, ""); err == nil {
		t.Fatal("upload to a missing bucket succeeded")
	}
	if _, ok, _ := s.Stat(ctx, bad); ok {
		t.Error("an object exists after a failed upload")
	}
}

// A wrong bucket must be distinguishable from an empty one — a HEAD on a key
// answers 404 either way, which is why CheckBucket exists.
func TestS3CheckBucket(t *testing.T) {
	s, bucket := testStore(t)
	ctx := context.Background()
	if err := s.CheckBucket(ctx, bucket); err != nil {
		t.Errorf("CheckBucket on an existing bucket: %v", err)
	}
	if err := s.CheckBucket(ctx, bucket+"-does-not-exist"); err == nil {
		t.Error("CheckBucket accepted a bucket that does not exist")
	}
	// The distinction the check is for: a missing key in a real bucket is not
	// an error, so Stat alone could never have caught a bad bucket name.
	if _, ok, err := s.Stat(ctx, Ref{Bucket: bucket, Key: "varhub-test/definitely-absent"}); err != nil || ok {
		t.Errorf("Stat on a missing key = %v, %v; want not-found with no error", ok, err)
	}
}
