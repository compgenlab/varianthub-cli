package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// checksumMeta records the checksum a source declared, so a later run can tell
// "already uploaded" from "uploaded, but the upstream file has since changed"
// without downloading anything.
const checksumMeta = "varhub-checksum"

// partSize is the multipart chunk size. The registry has sources in the tens of
// gigabytes (dbSNP is ~24 GB), and S3 caps a multipart upload at 10,000 parts,
// so 5 MiB parts would top out around 50 GB with a great many round trips.
// 64 MiB keeps the part count in the low hundreds for those sources.
const partSize = 64 * 1024 * 1024

// Object is what a Stat found.
type Object struct {
	Size     int64
	Checksum string // the source's declared checksum at upload time, if any
}

// Store is the subset of object-store behaviour fetch depends on. It is an
// interface so tests can exercise the staging and publish logic without an
// endpoint; the S3 implementation is covered separately against a real gateway.
type Store interface {
	Stat(ctx context.Context, ref Ref) (Object, bool, error)
	PutFile(ctx context.Context, ref Ref, localPath, checksum string) error
	Remove(ctx context.Context, ref Ref) error
	CheckBucket(ctx context.Context, bucket string) error
}

// S3 talks to an S3-compatible endpoint.
type S3 struct {
	client   *s3.Client
	uploader *manager.Uploader
}

// NewS3 builds a client from the ambient AWS configuration.
//
// Credentials come from the standard chain — environment, shared config, then
// the instance/container role — so a deployed pod can use an assumed role
// instead of static keys, which is the whole reason not to read
// AWS_ACCESS_KEY_ID directly here.
//
// AWS_ENDPOINT_URL (or AWS_ENDPOINT_URL_S3) points at a non-AWS, S3-compatible
// target. Setting it also forces path-style addressing: virtual-host style
// requires wildcard DNS for the bucket, which a local gateway does not have.
// Option configures the client.
type Option func(*s3.Options)

// WithHTTPClient overrides the HTTP client. Used by tests that need to observe
// what actually went over the wire — specifically, that indexed reads issue
// ranged GETs rather than pulling whole objects.
func WithHTTPClient(c aws.HTTPClient) Option {
	return func(o *s3.Options) { o.HTTPClient = c }
}

func NewS3(ctx context.Context, opts ...Option) (*S3, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	if cfg.Region == "" {
		// S3 requires a region to sign with. Any value works against a
		// compatible gateway, and us-east-1 is the conventional default.
		cfg.Region = "us-east-1"
	}

	endpoint := firstEnv("AWS_ENDPOINT_URL_S3", "AWS_ENDPOINT_URL")
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if endpoint != "" {
			o.BaseEndpoint = aws.String(endpoint)
			o.UsePathStyle = true
		}
		for _, opt := range opts {
			opt(o)
		}
	})

	up := manager.NewUploader(client, func(u *manager.Uploader) {
		u.PartSize = partSize
		// Abort the multipart upload if any part fails. Leaving the parts would
		// bill for them and, worse, a resumed run could complete an upload from
		// a half-written mixture. This is the default; it is set explicitly
		// because the acceptance criterion depends on it.
		u.LeavePartsOnError = false
	})
	return &S3{client: client, uploader: up}, nil
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

// CheckBucket confirms the bucket exists and the caller can reach it.
//
// A HEAD on a key cannot do this job: S3 answers 404 for a missing key whether
// or not the bucket exists, so a wrong bucket name would look like "nothing
// uploaded yet" and only surface after the first download had already run.
func (s *S3) CheckBucket(ctx context.Context, bucket string) error {
	_, err := s.client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
	if err != nil {
		return fmt.Errorf("bucket %q: %w", bucket, err)
	}
	return nil
}

// Stat reports whether an object exists, and what varhub recorded about it.
func (s *S3) Stat(ctx context.Context, ref Ref) (Object, bool, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(ref.Bucket),
		Key:    aws.String(ref.Key),
	})
	if err != nil {
		if isNotFound(err) {
			return Object{}, false, nil
		}
		return Object{}, false, fmt.Errorf("head %s: %w", ref, err)
	}
	o := Object{Checksum: out.Metadata[checksumMeta]}
	if out.ContentLength != nil {
		o.Size = *out.ContentLength
	}
	return o, true, nil
}

// PutFile uploads localPath to ref, using a multipart upload when the file is
// large enough to need one.
//
// checksum, when non-empty, is stored as object metadata rather than verified
// here: verification already happened against the staged local copy before this
// is called, and an object that failed verification must never be uploaded at
// all.
func (s *S3) PutFile(ctx context.Context, ref Ref, localPath, checksum string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()

	in := &s3.PutObjectInput{
		Bucket: aws.String(ref.Bucket),
		Key:    aws.String(ref.Key),
		Body:   f,
	}
	if checksum != "" {
		in.Metadata = map[string]string{checksumMeta: checksum}
	}
	if _, err := s.uploader.Upload(ctx, in); err != nil {
		var mu manager.MultiUploadFailure
		if errors.As(err, &mu) {
			// The manager already aborted; say so, because "upload failed" on a
			// 24 GB object otherwise leaves the operator wondering whether they
			// are now paying for orphaned parts.
			return fmt.Errorf("upload %s: %w (multipart upload %s was aborted; no partial object remains)",
				ref, err, mu.UploadID())
		}
		return fmt.Errorf("upload %s: %w", ref, err)
	}
	return nil
}

// Remove deletes an object. A missing object is not an error, matching
// os.Remove's role in the local cache where a prune of something already gone
// is a no-op.
func (s *S3) Remove(ctx context.Context, ref Ref) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(ref.Bucket),
		Key:    aws.String(ref.Key),
	})
	if err != nil && !isNotFound(err) {
		return fmt.Errorf("delete %s: %w", ref, err)
	}
	return nil
}

// isNotFound distinguishes "the object is not there" from a real failure.
//
// S3 reports a missing object on HEAD as a bare 404 with no parsed error shape,
// so the typed checks alone are not enough — hence the status-code fallback.
func isNotFound(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) && re.HTTPStatusCode() == 404 {
		return true
	}
	var api smithy.APIError
	if errors.As(err, &api) {
		switch api.ErrorCode() {
		case "NotFound", "NoSuchKey", "404":
			return true
		}
	}
	return false
}

// Download fetches an object to a local file. It exists for verification —
// reading a provisioned object back to compare it against a local build — not
// for the annotate read path, which will use range requests.
func (s *S3) Download(ctx context.Context, ref Ref, w io.Writer) error {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(ref.Bucket),
		Key:    aws.String(ref.Key),
	})
	if err != nil {
		return fmt.Errorf("get %s: %w", ref, err)
	}
	defer out.Body.Close()
	_, err = io.Copy(w, out.Body)
	return err
}
