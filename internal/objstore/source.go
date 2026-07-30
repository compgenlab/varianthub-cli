package objstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/compgenlab/cghts/htsio/tabix"
	"github.com/compgenlab/cghts/iosource"
)

// Source is the read half: an iosource.ByteSource backed by S3 range requests,
// so an indexed file can be queried where it sits.
//
// This is built on the SDK's GetObject with a Range rather than signing raw
// HTTP requests over iosource.HTTPRange. Both would work — HTTPRange takes a
// custom client, and SigV4 could be applied by a RoundTripper — but going
// through the SDK means credentials, retries, redirects and the choice between
// virtual-host and path-style addressing are all handled by code that already
// gets them right, instead of being reimplemented against an endpoint we do not
// control.
type Source struct {
	client *s3.Client
	ref    Ref

	mu   sync.Mutex
	size int64 // -1 until learned
}

var _ iosource.ByteSource = (*Source)(nil)

// Open returns a byte source for an object.
func (s *S3) Open(ctx context.Context, ref Ref) (*Source, error) {
	src := &Source{client: s.client, ref: ref, size: -1}
	// Learn the size up front: it is needed to bound the section reader that
	// BGZF decompression seeks within, and it doubles as an existence check with
	// a clearer error than a failed read halfway through a query.
	if _, err := src.Size(); err != nil {
		return nil, err
	}
	return src, nil
}

// Size reports the object's length.
func (s *Source) Size() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.size >= 0 {
		return s.size, nil
	}
	out, err := s.client.HeadObject(context.Background(), &s3.HeadObjectInput{
		Bucket: aws.String(s.ref.Bucket),
		Key:    aws.String(s.ref.Key),
	})
	if err != nil {
		return 0, fmt.Errorf("head %s: %w", s.ref, err)
	}
	if out.ContentLength == nil {
		return 0, fmt.Errorf("head %s: no content length", s.ref)
	}
	s.size = *out.ContentLength
	return s.size, nil
}

// ReadAt implements io.ReaderAt with a ranged GET.
//
// Safe for concurrent use, as ByteSource requires: each call is an independent
// request holding no reader state.
func (s *Source) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 {
		return 0, fmt.Errorf("read %s: negative offset %d", s.ref, off)
	}
	end := off + int64(len(p)) - 1

	out, err := s.client.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(s.ref.Bucket),
		Key:    aws.String(s.ref.Key),
		Range:  aws.String(fmt.Sprintf("bytes=%d-%d", off, end)),
	})
	if err != nil {
		// Reading at or past the end is io.EOF, not a failure: callers probe
		// past the end legitimately, and bgzf does it at the last block.
		if isRangeUnsatisfiable(err) {
			return 0, io.EOF
		}
		return 0, fmt.Errorf("read %s at %d: %w", s.ref, off, err)
	}
	defer out.Body.Close()

	n, err := io.ReadFull(out.Body, p)
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		// Short read at the end of the object: io.ReaderAt requires a non-nil
		// error when fewer than len(p) bytes are returned.
		return n, io.EOF
	}
	return n, err
}

// Close releases the source. There is no persistent connection to release —
// each read is its own request — so this exists to satisfy ByteSource.
func (s *Source) Close() error { return nil }

// isRangeUnsatisfiable reports the 416 an over-long or past-the-end range gets.
func isRangeUnsatisfiable(err error) bool {
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) && re.HTTPStatusCode() == 416 {
		return true
	}
	return strings.Contains(err.Error(), "InvalidRange")
}

// OpenReader returns a whole object as a stream. Used for index sidecars, which
// are read start to finish and are small enough not to warrant ranging.
func (s *S3) OpenReader(ctx context.Context, ref Ref) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(ref.Bucket),
		Key:    aws.String(ref.Key),
	})
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", ref, err)
	}
	return out.Body, nil
}

// Sibling returns an iosource.SiblingOpener for objects in a store, so index
// resolution works the same way over S3 as it does over files and HTTP.
func (s *S3) Sibling(ctx context.Context) iosource.SiblingOpener {
	return func(locator, suffix string) (io.ReadCloser, error) {
		ref, err := Parse(locator + suffix)
		if err != nil {
			return nil, err
		}
		return s.OpenReader(ctx, ref)
	}
}

// OpenTabix opens a tabix-indexed file at a cache locator, whether that is a
// local path or an object in a store.
//
// The index suffix is resolved by trying .tbi then .csi, matching what the
// path-based reader does, so a source indexed either way works remotely without
// the caller having to know which.
func OpenTabix(ctx context.Context, store *S3, locator string) (*tabix.Reader, error) {
	if !IsRemote(locator) {
		return tabix.NewReader(locator)
	}
	ref, err := Parse(locator)
	if err != nil {
		return nil, err
	}
	src, err := store.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	rs, err := iosource.ReadSeeker(src)
	if err != nil {
		return nil, err
	}
	idx, _, err := iosource.ResolveSibling(locator, []string{".tbi", ".csi"}, store.Sibling(ctx))
	if err != nil {
		return nil, fmt.Errorf("no index (.tbi or .csi) for %s: %w", locator, err)
	}
	defer idx.Close()

	r, err := tabix.NewReaderFromSource(rs, idx, tabix.WithCloser(src))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", locator, err)
	}
	return r, nil
}
