package objstore

import (
	"context"
	"fmt"

	"github.com/compgenlab/cghts/htsio/bbi"
	"github.com/compgenlab/cghts/htsio/tabix"
	"github.com/compgenlab/cghts/iosource"
)

// OpenBBILocator opens a bigWig/bigBed at a locator, returning nil for a local
// path so the caller lets the library open the file itself.
//
// BBI needs no sidecar — the index lives inside the file — so unlike tabix
// there is nothing to resolve alongside it.
func OpenBBILocator(ctx context.Context, locator string) (*bbi.Reader, error) {
	if !IsRemote(locator) {
		return nil, nil
	}
	src, err := iosource.Open(ctx, locator)
	if err != nil {
		return nil, err
	}
	r, err := bbi.NewReaderFromSource(src, bbi.WithReaderCloser(src))
	if err != nil {
		src.Close()
		return nil, fmt.Errorf("open %s: %w", locator, err)
	}
	return r, nil
}

// OpenTabixLocator opens a tabix-indexed file at a locator, returning nil for a
// local path so the caller lets the library open the file itself.
func OpenTabixLocator(ctx context.Context, locator string) (*tabix.Reader, error) {
	if !IsRemote(locator) {
		return nil, nil
	}
	return OpenTabix(ctx, locator)
}
