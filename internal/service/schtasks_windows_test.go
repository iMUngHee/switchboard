//go:build windows

package service

import (
	"syscall"
	"testing"
)

// TestQuoteMatchesTheRuntime: on Windows, the quoting agrees with the Go
// runtime's own, which CommandLineToArgvW reads back.
func TestQuoteMatchesTheRuntime(t *testing.T) {
	for _, in := range []string{"plain", `C:\dir\file.toml`, "two words", "", `say "hi"`, `C:\dir with\`, `a\\"b`, `x"y`, `a\b c`, "tab\there", "a\nb", `\\server\share\`} {
		if got, want := quoteWindowsArg(in), syscall.EscapeArg(in); got != want {
			t.Errorf("quoteWindowsArg(%q) = %q, syscall.EscapeArg = %q", in, got, want)
		}
	}
}
