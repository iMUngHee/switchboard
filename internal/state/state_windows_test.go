//go:build windows

package state

import (
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// TestSaveAndLoadWaitOutABriefHolder: on Windows a save and a read of the
// same state file meet as sharing violations; each side tries again for a
// moment instead of failing (status reading while the daemon saves).
func TestSaveAndLoadWaitOutABriefHolder(t *testing.T) {
	dir := t.TempDir()
	key := strings.Repeat("a", 32) + ".vm"
	if err := Save(dir, key, Link{LastRemoteStore: "first"}); err != nil {
		t.Fatal(err)
	}
	path := statePath(dir, key)

	t.Run("a save over a file a reader has open", func(t *testing.T) {
		// Opened just before the move, so the move is sure to meet it. Go
		// opens without FILE_SHARE_DELETE, so a move over it fails until the
		// reader closes it.
		held := false
		err := save(dir, key, Link{LastRemoteStore: "second"}, func() error {
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			held = true
			time.AfterFunc(150*time.Millisecond, func() { f.Close() })
			return nil
		})
		if err != nil || !held {
			t.Fatalf("save while a reader held the file: %v (held %v)", err, held)
		}
		if l, err := Load(dir, key); err != nil || l.LastRemoteStore != "second" {
			t.Fatalf("load: %+v %v", l, err)
		}
	})

	t.Run("a read while the file is held exclusively", func(t *testing.T) {
		p, err := windows.UTF16PtrFromString(path)
		if err != nil {
			t.Fatal(err)
		}
		h, err := windows.CreateFile(p, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
		if err != nil {
			t.Fatal(err)
		}
		time.AfterFunc(150*time.Millisecond, func() { windows.CloseHandle(h) })
		if l, err := Load(dir, key); err != nil || l.LastRemoteStore != "second" {
			t.Fatalf("load while the file was held: %+v %v", l, err)
		}
	})
}
