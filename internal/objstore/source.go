package objstore

import (
	"context"
	"fmt"

	"github.com/compgenlab/cghts/htsio/tabix"
	"github.com/compgenlab/cghts/iosource"
	_ "github.com/compgenlab/cghts/iosource/s3" // registers the s3:// scheme
)

// The read side lives in cghts: iosource.Open dispatches on the locator's
// scheme, and the blank import above registers s3://. What remains here is the
// mapping from varhub's cache locators to that, plus the sidecar resolution a
// tabix source needs.
//
// Provisioning — multipart upload, checksum metadata, abort-on-failure — stays
// in this package, because writing is varhub's concern and not something a
// read-oriented I/O library should carry.

// OpenTabix opens a tabix-indexed file at a cache locator, local or remote.
//
// The index suffix is resolved by trying .tbi then .csi, matching the
// path-based reader, so a source indexed either way works remotely without the
// caller needing to know which.
func OpenTabix(ctx context.Context, locator string) (*tabix.Reader, error) {
	if !IsRemote(locator) {
		return tabix.NewReader(locator)
	}
	src, err := iosource.Open(ctx, locator)
	if err != nil {
		return nil, err
	}
	rs, err := iosource.ReadSeeker(src)
	if err != nil {
		src.Close()
		return nil, err
	}
	idx, _, err := iosource.ResolveSibling(locator, []string{".tbi", ".csi"}, iosource.Sibling(ctx))
	if err != nil {
		src.Close()
		return nil, fmt.Errorf("no index (.tbi or .csi) for %s: %w", locator, err)
	}
	defer idx.Close()

	r, err := tabix.NewReaderFromSource(rs, idx, tabix.WithCloser(src))
	if err != nil {
		src.Close()
		return nil, fmt.Errorf("open %s: %w", locator, err)
	}
	return r, nil
}
