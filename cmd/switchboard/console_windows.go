//go:build windows

package main

import "golang.org/x/sys/windows"

var procGetConsoleWindow = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow")

// consoleWindow returns this process's console window handle; 0 means it has
// none, which is what a console child of the console-less daemon must see.
// A failed lookup is an error, never 0, so it cannot pass for "no window".
func consoleWindow() (uintptr, error) {
	if err := procGetConsoleWindow.Find(); err != nil {
		return 0, err
	}
	h, _, _ := procGetConsoleWindow.Call()
	return h, nil
}
