package fetch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Serializing tool provisioning between processes.
//
// A tool's image and data directory are shared state on a machine, and more than
// one process can provision the same tool at the same time: two workers on one
// host, or two pods mounting the same directory. Every step assumed a single
// writer. `apptainer pull` removes the .sif before writing a new one, so a
// concurrent job loses the image it is running from. Restoring cached setup data
// unpacked straight into the live data directory. Setup itself installs into that
// directory over hours.
//
// The sentinel does not cover this. It is written after the work finishes, so it
// answers "has this been done" and not "is someone doing it" — two processes that
// both find it absent both proceed, which is precisely the case that hurts.
//
// flock, rather than a lock file created with O_EXCL: the kernel drops it when
// the holder exits, so a process killed mid-setup leaves nothing to clean up. A
// hand-rolled lock file would survive its owner, and the recovery for that is
// guessing whether a lock is stale — on the one path where guessing wrong means
// two processes writing the same directory.

const lockRetryInterval = 2 * time.Second

// acquireProvisionLock takes an exclusive lock covering one tool's image and data
// directory, blocking until it is free or ctx ends. The returned function
// releases it and must be called.
//
// dir is the tool's data directory; the lock is its sibling, so it can be taken
// before that directory exists and survives it being replaced.
func acquireProvisionLock(ctx context.Context, dir string, logf func(string, ...any)) (func(), error) {
	lockPath := dir + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, fmt.Errorf("provision lock: %w", err)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("provision lock: %w", err)
	}

	waited := false
	for {
		ok, err := tryLockFile(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("provision lock: %w", err)
		}
		if ok {
			if waited {
				logf("another process finished; continuing")
			}
			return func() {
				unlockFile(f)
				f.Close()
				// The lock file itself is left behind deliberately. Removing it
				// races: another process may already hold a lock on this inode,
				// and a third would then create a new file and lock that
				// instead — two holders, each correctly locked, on different
				// inodes at the same path.
			}, nil
		}
		if !waited {
			// Said once. This can be a long wait — a VEP install is measured in
			// hours — and silence here looks like a hung job.
			logf("waiting for another process to finish provisioning this tool")
			waited = true
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(lockRetryInterval):
		}
	}
}
