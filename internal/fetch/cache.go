package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"

	"github.com/compgenlab/varianthub-cli/internal/checksum"
	"github.com/compgenlab/varianthub-cli/internal/objstore"
)

// The cache can be a local directory or an S3 prefix. A remote cache is served
// one file at a time, by one of two routes:
//
//   - Streamed, when nothing here has to read the file: the bytes go from the
//     upstream response straight into the object, hashed on the way past. This
//     is the common case, because most sources publish an index beside their
//     data.
//   - Staged, when something does: building a tabix index reads the whole file,
//     and a build recipe or a GTF conversion rewrites it. Those need a real
//     file, so one is written to scratch, worked on, uploaded and dropped.
//
// The distinction is worth keeping sharp, because scratch is the expensive
// resource here. Staging costs the size of the largest single file a source
// publishes — tens of gigabytes for dbSNP or CADD — and on an ephemeral disk
// that is a limit someone has to provision for. Streaming spends none of it, so
// the staging path should be reached only when the work genuinely requires it.

var (
	storeMu   sync.RWMutex
	storeOnce sync.Once
	storeVal  objstore.Store
	storeErr  error
)

// remoteStore returns the process-wide object-store client, built on first use.
//
// Lazy because a purely local install must never touch AWS configuration: an
// operator with no credentials and no bucket should not see a credential error
// from a command that was only ever going to write to /var/cache.
func remoteStore() (objstore.Store, error) {
	storeOnce.Do(func() {
		s, err := objstore.NewS3(context.Background())
		storeMu.Lock()
		defer storeMu.Unlock()
		if storeVal == nil { // a test may have installed one already
			storeVal, storeErr = s, err
		}
	})
	storeMu.RLock()
	defer storeMu.RUnlock()
	return storeVal, storeErr
}

// SetStore overrides the object-store client. For tests.
//
// Guarded because Snapshot fetches concurrently: without the lock, swapping the
// client races every worker that reads it.
func SetStore(s objstore.Store) {
	storeOnce.Do(func() {})
	storeMu.Lock()
	defer storeMu.Unlock()
	storeVal, storeErr = s, nil
}

// locatorExists reports whether a cache locator currently holds a file.
func locatorExists(ctx context.Context, loc string) (bool, error) {
	if !objstore.IsObject(loc) {
		return fileExists(loc), nil
	}
	store, err := remoteStore()
	if err != nil {
		return false, err
	}
	ref, err := objstore.Parse(loc)
	if err != nil {
		return false, err
	}
	_, ok, err := store.Stat(ctx, ref)
	return ok, err
}

// locatorSize returns the size of whatever is at a locator.
func locatorSize(ctx context.Context, loc string) (int64, bool, error) {
	if !objstore.IsObject(loc) {
		fi, err := os.Stat(loc)
		if err != nil {
			return 0, false, nil
		}
		return fi.Size(), true, nil
	}
	store, err := remoteStore()
	if err != nil {
		return 0, false, err
	}
	ref, err := objstore.Parse(loc)
	if err != nil {
		return 0, false, err
	}
	obj, ok, err := store.Stat(ctx, ref)
	return obj.Size, ok, err
}

// objectExists reports whether a locator holds an object, treating an error as
// absence. A cache lookup that cannot reach the store should fall through to
// building the thing, not fail the download — the store is an optimisation here,
// not the source of truth.
func objectExists(ctx context.Context, loc string) bool {
	ok, err := locatorExists(ctx, loc)
	return err == nil && ok
}

// fetchObject copies an object to a local file, writing through a temp file so
// an interrupted copy cannot leave something that later looks cached.
func fetchObject(ctx context.Context, loc, dest string) error {
	store, err := remoteStore()
	if err != nil {
		return err
	}
	ref, err := objstore.Parse(loc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".varhub-fetch-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if err := store.Download(ctx, ref, tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dest)
}

// verifyingReader hashes bytes as they are read and fails the final read when
// the digest disagrees.
//
// Failing the *read* is the whole point. The upload manager aborts a multipart
// upload when the body returns an error, so a mismatch means no object is ever
// completed; checking the digest after the upload returned would be checking an
// object readers can already see. A nil Verifier hashes nothing and always
// passes, which is how an unverified source streams.
type verifyingReader struct {
	r io.Reader
	v *checksum.Verifier
}

func (vr *verifyingReader) Read(p []byte) (int, error) {
	n, err := vr.r.Read(p)
	if n > 0 {
		_, _ = vr.v.Write(p[:n])
	}
	if errors.Is(err, io.EOF) {
		if cErr := vr.v.Check(); cErr != nil {
			// Returned instead of EOF, so the upload fails rather than completes.
			return n, cErr
		}
	}
	return n, err
}

// streamToObject downloads a URL straight into an object, with no local copy.
//
// For files nothing here needs to read: the bytes are hashed on the way past and
// otherwise pass through untouched. Anything that has to be opened locally —
// building an index, converting a format — belongs on the staging path instead.
func streamToObject(ctx context.Context, url, loc, sum string) error {
	v, err := checksum.New(sum)
	if err != nil {
		return err
	}
	store, err := remoteStore()
	if err != nil {
		return err
	}
	ref, err := objstore.Parse(loc)
	if err != nil {
		return err
	}
	resp, err := httpDo(ctx, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	if err := store.PutStream(ctx, ref, &verifyingReader{r: resp.Body, v: v}, sum); err != nil {
		return fmt.Errorf("stream %s: %w", url, err)
	}
	return nil
}

// verifyPair is indirected so tests can exercise the streaming decision without
// an object store that can serve a tabix pair by range request. Production
// always uses verifyRemotePair.
var verifyPair = verifyRemotePair

// verifyRemotePair opens an uploaded data/index pair and removes both if it does
// not work.
//
// The staging path proves the pair before publishing it, by opening the local
// copy. Streaming has no local copy, so the check moves after the upload — which
// means it has to clean up rather than simply decline to publish. Removing the
// data first is what makes that safe: a reader that finds data is entitled to
// assume an index, so data must be the first thing to go.
//
// Leaving a broken pair in place would be the worst outcome available. It looks
// cached, so the next run skips it, and the failure surfaces later as a source
// that returns nothing.
func verifyRemotePair(ctx context.Context, loc, idxExt string) error {
	r, err := objstore.OpenTabix(ctx, loc)
	if err == nil {
		r.Close()
		return nil
	}
	if rmErr := locatorRemove(ctx, loc); rmErr != nil {
		return fmt.Errorf("verify %s: %w (and the broken object could not be removed: %v)",
			loc, err, rmErr)
	}
	if rmErr := locatorRemove(ctx, loc+idxExt); rmErr != nil {
		return fmt.Errorf("verify %s: %w (and the broken index could not be removed: %v)",
			loc, err, rmErr)
	}
	return fmt.Errorf("verify %s: %w (nothing was published)", loc, err)
}

// putObject uploads a local file to a locator.
func putObject(ctx context.Context, src, loc string) error {
	store, err := remoteStore()
	if err != nil {
		return err
	}
	ref, err := objstore.Parse(loc)
	if err != nil {
		return err
	}
	return store.PutFile(ctx, ref, src, "")
}

// locatorRemove deletes whatever is at a locator, tolerating its absence.
func locatorRemove(ctx context.Context, loc string) error {
	if !objstore.IsObject(loc) {
		if err := os.Remove(loc); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	store, err := remoteStore()
	if err != nil {
		return err
	}
	ref, err := objstore.Parse(loc)
	if err != nil {
		return err
	}
	return store.Remove(ctx, ref)
}

// anyIndexAt reports whether a tabix sidecar accompanies a locator, and which.
func anyIndexAt(ctx context.Context, loc string) (string, bool, error) {
	for _, ext := range []string{".tbi", ".csi"} {
		ok, err := locatorExists(ctx, loc+ext)
		if err != nil {
			return "", false, err
		}
		if ok {
			return ext, true, nil
		}
	}
	return "", false, nil
}

// staging is a local working area for one artifact bound for a remote cache.
type staging struct {
	dir  string // temp dir, removed by close
	work string // the local path to build at
}

// newStaging creates a scratch area for the file that will land at final.
func newStaging(final string) (*staging, error) {
	dir, err := os.MkdirTemp("", "varhub-stage-")
	if err != nil {
		return nil, err
	}
	return &staging{dir: dir, work: filepath.Join(dir, objstore.Base(final))}, nil
}

func (s *staging) close() {
	if s == nil || s.dir == "" {
		return
	}
	if keepTemp {
		logf("kept staging dir %s", s.dir)
		return
	}
	os.RemoveAll(s.dir)
}

// publish uploads the staged file, and each present sidecar, to the cache.
//
// The data file goes last on purpose. A reader that finds the data object is
// entitled to assume the index is there too, so publishing in that order means
// an upload interrupted between the two leaves an incomplete-looking source
// that the next run redoes, rather than a complete-looking one with no index.
func (s *staging) publish(ctx context.Context, final, checksum string, sidecars ...string) error {
	store, err := remoteStore()
	if err != nil {
		return err
	}
	for _, ext := range sidecars {
		if ext == "" || !fileExists(s.work+ext) {
			continue
		}
		ref, err := objstore.Parse(final + ext)
		if err != nil {
			return err
		}
		if err := store.PutFile(ctx, ref, s.work+ext, ""); err != nil {
			return err
		}
	}
	ref, err := objstore.Parse(final)
	if err != nil {
		return err
	}
	return store.PutFile(ctx, ref, s.work, checksum)
}

// checkRemoteReady fails early, with an actionable message, when a remote cache
// is configured but unusable.
//
// Without this the first symptom is a credential error from deep inside a
// download loop, after the user has already waited for other sources.
func checkRemoteReady(ctx context.Context, cacheDir string) error {
	if !objstore.IsObject(cacheDir) {
		return nil
	}
	ref, err := objstore.Parse(cacheDir)
	if err != nil {
		return fmt.Errorf("cache_dir %q: %w", cacheDir, err)
	}
	store, err := remoteStore()
	if err != nil {
		return fmt.Errorf("cache_dir %s: %w (set AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY, or use an instance role; AWS_ENDPOINT_URL points at a non-AWS endpoint)", cacheDir, err)
	}
	// Check the bucket itself, not a key: a HEAD on a missing key is a 404
	// whether or not the bucket exists, so a typo in the bucket name would look
	// like an empty cache and only fail after the first download had run.
	if err := store.CheckBucket(ctx, ref.Bucket); err != nil {
		return fmt.Errorf("cache_dir %s is not usable: %w", cacheDir, err)
	}
	return nil
}
