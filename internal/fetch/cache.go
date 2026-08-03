package fetch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/compgenlab/varianthub-cli/internal/objstore"
)

// The cache can be a local directory or an S3 prefix. Everything varhub does to
// a source — verify a checksum, build a tabix index, open it to confirm it
// works — needs a real file, so a remote cache is served by staging one file at
// a time locally and uploading the result.
//
// That staging requirement is worth stating plainly, since "the server cannot
// store the annotation files" is what motivated remote caches in the first
// place: provisioning needs scratch for the largest single file, not for the
// whole snapshot.

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
