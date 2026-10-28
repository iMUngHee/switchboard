//go:build !windows

package main

import "errors"

// consoleWindow has no meaning outside Windows.
func consoleWindow() (uintptr, error) { return 0, errors.New("consoles are a Windows notion") }
