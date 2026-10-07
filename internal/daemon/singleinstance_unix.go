//go:build !windows

package daemon

import (
	"os"

	"github.com/blamely/blamely/internal/filelock"
)

// acquireInstanceLock takes an exclusive, non-blocking flock on path. The lock
// is held for the process lifetime through the returned *os.File and releases
// only when that file is closed (explicit Close on shutdown, or process exit —
// the kernel drops flocks automatically). ok=false means another daemon already
// holds the lock and this process should exit.
//
// The lock file is deliberately NOT removed on release: unlinking it would let a
// racing acquirer create-and-lock a fresh inode while the original still holds
// the old one, defeating the mutual exclusion. A stale zero-byte daemon.lock is
// harmless and gets re-locked next start.
func acquireInstanceLock(path string) (f *os.File, ok bool, err error) {
	return filelock.TryLock(path)
}
