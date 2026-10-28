//go:build !windows

package proc

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// alive reports whether pid is a running process. A zombie — exited, not yet
// reaped by whoever inherited it — counts as gone.
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func trapTerm() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM)
	go func() {
		<-ch
		fmt.Println("stopped politely")
		os.Exit(0)
	}()
}

// TestDeadlineStopsPolitelyFirst: a stage that handles SIGTERM finishes on its
// own terms within the grace period, which is what lets pager's relay in still
// wake its recipients.
func TestDeadlineStopsPolitelyFirst(t *testing.T) {
	var out bytes.Buffer
	s := helper("term")
	s.Stdout = &out
	s.Grace = 5 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel()
	r := Run(ctx, s)
	if !r.TimedOut || r.ExitCode != 0 {
		t.Fatalf("result %+v, want a timed-out stage that exited 0", r)
	}
	if !strings.Contains(out.String(), "stopped politely") {
		t.Fatalf("the stage did not see SIGTERM first: %q", out.String())
	}
	if r.Duration > 4*time.Second {
		t.Fatalf("took %v; SIGKILL should not have been needed", r.Duration)
	}
}
