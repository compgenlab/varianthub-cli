//go:build unix

package fetch

import (
	"errors"
	"os"
	"syscall"
)

// tryLockFile takes an exclusive flock without blocking, reporting whether it
// got one. A refusal is not an error: it means someone else holds it.
func tryLockFile(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return false, nil
	default:
		return false, err
	}
}

func unlockFile(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
