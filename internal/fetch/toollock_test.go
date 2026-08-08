package fetch

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func quietf(string, ...any) {}

// The whole point: the second holder waits for the first. flock is keyed to an
// open file description rather than to a process, so two OpenFile calls conflict
// even from one process — which is what lets this be tested without spawning a
// second binary.
func TestProvisionLockIsExclusive(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tools", "vep", "115")

	first, err := acquireProvisionLock(context.Background(), dir, quietf)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	var got atomic.Bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		release, err := acquireProvisionLock(context.Background(), dir, quietf)
		if err != nil {
			t.Errorf("second acquire: %v", err)
			return
		}
		got.Store(true)
		release()
	}()

	// Longer than one retry interval, so a lock that is not actually exclusive
	// has had every chance to be taken.
	time.Sleep(lockRetryInterval + 500*time.Millisecond)
	if got.Load() {
		t.Fatal("second caller took the lock while the first still held it")
	}

	first()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("second caller never acquired the lock after it was released")
	}
	if !got.Load() {
		t.Fatal("second caller finished without acquiring")
	}
}

// A blocked waiter is still cancellable. Without this a job that cannot get the
// lock ignores its deadline and the worker's timeout does nothing.
func TestProvisionLockHonoursContext(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tools", "vep", "115")

	release, err := acquireProvisionLock(context.Background(), dir, quietf)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	if _, err := acquireProvisionLock(ctx, dir, quietf); err == nil {
		t.Fatal("acquired a lock that was held")
	} else if !isDeadline(err) {
		t.Fatalf("err = %v, want a context error", err)
	}
	if el := time.Since(start); el > 5*time.Second {
		t.Fatalf("waited %v before honouring the context", el)
	}
}

func isDeadline(err error) bool {
	return err == context.DeadlineExceeded ||
		(err != nil && (err.Error() == context.DeadlineExceeded.Error() ||
			err.Error() == "provision lock: "+context.DeadlineExceeded.Error()))
}

// The lock is taken before the directory it guards exists — the first process to
// provision a tool creates that directory, so a lock inside it could not have
// protected its creation.
func TestProvisionLockWorksBeforeTheDirExists(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tools", "vep", "115")
	release, err := acquireProvisionLock(context.Background(), dir, quietf)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer release()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("acquiring the lock created the data directory: %v", err)
	}
}

// Serial acquisitions must each succeed. A release that did not actually unlock
// would pass the exclusivity test above and deadlock every later caller.
func TestProvisionLockIsReusable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tools", "vep", "115")
	for i := 0; i < 3; i++ {
		release, err := acquireProvisionLock(context.Background(), dir, quietf)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		release()
	}
}

// Concurrent callers see one another's completed work rather than interleaving.
// Each increments a counter under the lock; without exclusion the reads and
// writes overlap and the total comes up short.
func TestProvisionLockSerializesWriters(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tools", "vep", "115")
	counter := filepath.Join(t.TempDir(), "n")
	if err := os.WriteFile(counter, []byte("0"), 0o644); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := acquireProvisionLock(context.Background(), dir, quietf)
			if err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			defer release()

			b, err := os.ReadFile(counter)
			if err != nil {
				t.Errorf("read: %v", err)
				return
			}
			// A read, a pause, then a write: the window a non-exclusive lock
			// leaves open, made wide enough to lose.
			time.Sleep(2 * time.Millisecond)
			v := 0
			for _, c := range b {
				v = v*10 + int(c-'0')
			}
			if err := os.WriteFile(counter, []byte(itoa(v+1)), 0o644); err != nil {
				t.Errorf("write: %v", err)
			}
		}()
	}
	wg.Wait()

	b, err := os.ReadFile(counter)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != itoa(n) {
		t.Fatalf("counter = %s, want %d — updates were lost, so the lock did not serialize", b, n)
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}
