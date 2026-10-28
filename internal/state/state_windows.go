//go:build windows

package state

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// lockFile locks the file's first byte with LockFileEx. The lock belongs to
// the handle, so a second Lock in this process conflicts too.
func lockFile(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrLocked
	}
	return err
}

// unlockFile unlocks explicitly: Windows also releases a lock when its handle
// closes, but only when it gets round to it.
func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}

// replace moves from over to. MOVEFILE_WRITE_THROUGH returns only once the
// move is on disk, which os.Rename does not ask for.
func replace(from, to string) error {
	f, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	t, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(f, t, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
