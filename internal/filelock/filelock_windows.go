//go:build windows

package filelock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// TryLock takes an exclusive, fail-immediately lock on the first byte of path via
// LockFileEx, creating the file if needed. The lock lives as long as the returned
// *os.File is open: Windows releases it when the handle closes, on Close or
// process exit. ok=false means another holder has it.
//
// Like the Unix variant, the lock file is left on disk: deleting it would let a
// racing acquirer lock a new handle while the original still holds the old one.
func TryLock(path string) (f *os.File, ok bool, err error) {
	f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, ol,
	)
	if err != nil {
		_ = f.Close()
		// A lock already held by another process surfaces as ERROR_LOCK_VIOLATION.
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}
