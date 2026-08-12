package fetch

import (
	"bufio"
	"compress/gzip"
	"context"
	"fmt"
	"io"

	"github.com/compgenlab/varianthub-cli/internal/config"
	"github.com/compgenlab/varianthub-cli/internal/objstore"
)

// OpenGTFStream opens a GTF source's whole file for a linear read, wherever it
// lives and however it is compressed.
//
// Goes through EnsureIndexedGTF rather than ResolveSourcePath because the raw
// download is not guaranteed to survive: `download` bgzips a GTF, indexes it, and
// then prunes the original to reclaim the duplicate copy. The indexed file is the
// one that is always there.
//
// The result is a decompressed stream, so the caller sees GTF text either way.
// Close it.
func OpenGTFStream(ctx context.Context, cfg *config.Config, src config.Source) (io.ReadCloser, error) {
	loc, _, err := EnsureIndexedGTF(cfg, src, false)
	if err != nil {
		return nil, err
	}
	if !objstore.IsObject(loc) {
		return openMaybeGz(loc)
	}

	store, err := remoteStore()
	if err != nil {
		return nil, err
	}
	ref, err := objstore.Parse(loc)
	if err != nil {
		return nil, err
	}

	// Piped rather than downloaded to a temp file: this is a whole-file read of
	// something that is gigabytes on disk, and the caller keeps only the gene
	// names. Spending the disk to hold a copy nobody reads twice would be the
	// staging path this package exists to avoid.
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(store.Download(ctx, ref, pw))
	}()
	gz, err := gzip.NewReader(bufio.NewReader(pr))
	if err != nil {
		pr.CloseWithError(err)
		return nil, fmt.Errorf("read %s: %w", loc, err)
	}
	return pipeCloser{Reader: gz, pr: pr}, nil
}

// pipeCloser closes the decompressor and then the pipe, so a caller that stops
// early (an error writing rows, say) unblocks the download goroutine instead of
// leaving it filling a pipe nobody reads.
type pipeCloser struct {
	*gzip.Reader
	pr *io.PipeReader
}

func (c pipeCloser) Close() error {
	err := c.Reader.Close()
	c.pr.CloseWithError(io.ErrClosedPipe)
	return err
}
