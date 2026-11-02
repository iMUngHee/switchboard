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

// TestTaskStatusOfAnAbsentTask runs the real query, read-only: a task no one
// registered is absent rather than an error, and so is ScheduledDefrag,
// which Windows keeps in a subfolder, not the root folder the query asks.
func TestTaskStatusOfAnAbsentTask(t *testing.T) {
	for _, name := range []string{"switchboard-test-no-such-task", "ScheduledDefrag"} {
		if st, err := TaskStatus(name); err != nil || st != (Status{}) {
			t.Errorf("TaskStatus(%q) = %+v, %v; want not registered", name, st, err)
		}
	}
}
