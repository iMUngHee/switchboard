//go:build windows

package state

import (
	"errors"
	"os"
	"time"

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

// On Windows a save and a read of the same state file can meet: a read that
// opens the file while a save moves the new one over it fails with a sharing
// violation, and a move over a file someone has open fails until it is
// closed. Both are brief (status reads, the daemon saves), so each side tries
// again, shareTries times sharePause apart (about half a second), before
// reporting the error.
const (
	shareTries = 20
	sharePause = 25 * time.Millisecond
)

// shared reports whether err is one of those brief conflicts.
func shared(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// readFile reads a state file, again while a save is moving over it.
func readFile(path string) ([]byte, error) {
	for i := 1; ; i++ {
		data, err := os.ReadFile(path)
		if err == nil || !shared(err) || i == shareTries {
			return data, err
		}
		time.Sleep(sharePause)
	}
}

// replace moves from over to, again while a reader has it open.
// MOVEFILE_WRITE_THROUGH returns only once the move is on disk, which
// os.Rename does not ask for.
func replace(from, to string) error {
	f, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	t, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	for i := 1; ; i++ {
		err := windows.MoveFileEx(f, t, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
		if err == nil || !shared(err) || i == shareTries {
			return err
		}
		time.Sleep(sharePause)
	}
}
