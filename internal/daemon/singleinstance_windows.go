//go:build windows

package daemon

import (
	"os"

	"github.com/blamely/blamely/internal/filelock"
)

// acquireInstanceLock takes an exclusive, fail-immediately lock on the first
// byte of path via LockFileEx. The lock is held for the process lifetime through
// the returned *os.File (Windows releases the lock when the handle closes — on
// shutdown or process exit). ok=false means another daemon already holds it and
// this process should exit.
//
// Like the Unix variant, the lock file is left on disk: deleting it would let a
// racing acquirer lock a new handle while the original still holds the old one.
func acquireInstanceLock(path string) (f *os.File, ok bool, err error) {
	return filelock.TryLock(path)
}
