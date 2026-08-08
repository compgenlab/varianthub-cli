//go:build !unix

package fetch

import "os"

// No inter-process lock off Unix.
//
// The lock exists for a shared data directory reached by several processes at
// once — two workers on a host, or two pods on one volume — which is a
// deployment shape that is Linux in every case here: the tools being provisioned
// run under apptainer or docker. Rather than reach for a portable lock file,
// which would outlive a killed process and leave someone deciding whether it is
// stale, this platform provisions unserialized, exactly as it did before.
//
// Provisioning stays correct for a single process, which is what a Windows build
// of this is.
func tryLockFile(*os.File) (bool, error) { return true, nil }

func unlockFile(*os.File) {}
