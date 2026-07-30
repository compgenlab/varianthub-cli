package objstore

import (
	"context"
	"fmt"
	"sync"

	"github.com/compgenlab/cghts/htsio/bbi"
	"github.com/compgenlab/cghts/htsio/tabix"
)

var (
	sharedOnce sync.Once
	sharedS3   *S3
	sharedErr  error
)

// Shared returns the process-wide S3 client, built on first use.
//
// One client for the whole process because it is stateless and safe for
// concurrent use, and because building it touches the AWS credential chain —
// which a purely local install must never do. Nothing here runs until a remote
// locator actually turns up.
func Shared() (*S3, error) {
	sharedOnce.Do(func() {
		sharedS3, sharedErr = NewS3(context.Background())
	})
	if sharedErr != nil {
		return nil, fmt.Errorf("%w (set AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, or use an instance role; AWS_ENDPOINT_URL points at a non-AWS endpoint)", sharedErr)
	}
	return sharedS3, nil
}

// OpenBBI opens a bigWig/bigBed at a cache locator, local or remote.
//
// BBI needs no sidecar — the index lives inside the file — so unlike tabix
// there is nothing to resolve alongside it.
func OpenBBI(ctx context.Context, locator string) (*bbi.Reader, error) {
	if !IsRemote(locator) {
		return bbi.Open(locator)
	}
	store, err := Shared()
	if err != nil {
		return nil, err
	}
	ref, err := Parse(locator)
	if err != nil {
		return nil, err
	}
	src, err := store.Open(ctx, ref)
	if err != nil {
		return nil, err
	}
	r, err := bbi.NewReaderFromSource(src, bbi.WithReaderCloser(src))
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", locator, err)
	}
	return r, nil
}

// OpenTabixLocator opens a tabix-indexed file at a locator using the shared
// client, returning nil for a local path so the caller can let the underlying
// library open the file itself.
func OpenTabixLocator(ctx context.Context, locator string) (*tabix.Reader, error) {
	if !IsRemote(locator) {
		return nil, nil
	}
	store, err := Shared()
	if err != nil {
		return nil, err
	}
	return OpenTabix(ctx, store, locator)
}

// OpenBBILocator is OpenBBI for a locator that may be local, returning nil in
// that case so the caller lets the library open the file itself.
func OpenBBILocator(ctx context.Context, locator string) (*bbi.Reader, error) {
	if !IsRemote(locator) {
		return nil, nil
	}
	return OpenBBI(ctx, locator)
}
