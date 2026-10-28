//go:build windows

package proc

import (
	"golang.org/x/sys/windows"
)

// alive reports whether pid is a running process.
func alive(pid int) bool {
	h, err := windows.OpenProcess(windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, 0)
	return err == nil && ev == uint32(windows.WAIT_TIMEOUT)
}

// trapTerm does nothing on Windows: there is no SIGTERM to trap, and the
// "term" helper is used only by a Unix test.
func trapTerm() {}
