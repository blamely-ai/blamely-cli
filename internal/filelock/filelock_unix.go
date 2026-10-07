//go:build !windows

package filelock

import (
	"os"
	"syscall"
)

// TryLock takes an exclusive, non-blocking flock on path, creating the file if
// needed. The lock lives as long as the returned *os.File is open: Close releases
// it, and the kernel drops it when the process exits. ok=false means another
// holder has it.
//
// The lock file is deliberately NOT removed on release: unlinking it would let a
// racing acquirer create-and-lock a fresh inode while the original still holds
// the old one, defeating the mutual exclusion. A leftover zero-byte file is
// harmless and gets re-locked next time.
func TryLock(path string) (f *os.File, ok bool, err error) {
	f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil // held by another process
		}
		return nil, false, err
	}
	return f, true, nil
}
