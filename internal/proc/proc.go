// Package proc runs one stage of a relay round — a pager or transport process —
// and, when its time is up, stops the whole tree it started, not only the
// direct child. ssh in particular may leave a ProxyCommand behind, and a stage
// that outlives its deadline would hold a link's direction forever.
//
// On Unix every stage gets its own process group: a deadline sends SIGTERM to
// the group, then SIGKILL after a grace period. The polite signal matters:
// pager's `relay in` wakes the recipients of what it imported only when it
// exits normally.
//
// Windows has no polite signal Go can send, so a stage is started suspended,
// placed in its own job object, and resumed; a deadline terminates the job.
// ContainSelf additionally puts the calling process in a kill-on-close job, so
// that everything it starts dies with it even if it is killed outright.
//
// After a stage exits on its own, Unix leaves any descendant it started alone:
// ssh may have handed a connection to a persisted ControlMaster that must
// survive. On Windows, where ssh runs without ControlMaster, closing the
// stage's job ends any such descendant.
package proc

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"time"
)

// DefaultGrace is how long a stage gets between SIGTERM and SIGKILL.
const DefaultGrace = 3 * time.Second

// waitDelay bounds how long Wait keeps reading a pipe after the process exits,
// in case a descendant still holds it open.
const waitDelay = 2 * time.Second

// Spec describes one process to run.
type Spec struct {
	Path   string
	Args   []string // argv[1:]
	Env    []string // the full environment; nil inherits this process's
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	Grace  time.Duration // Unix only; zero means DefaultGrace
}

// Result is how a stage ended. Check TimedOut before ExitCode: a stage stopped
// at its deadline may still have exited with a code of its own (a Unix stage
// that handled SIGTERM), but one that had to be killed reports -1.
type Result struct {
	Started  bool
	ExitCode int  // -1 when it did not start, or was killed or ended by a signal
	TimedOut bool // the context ended first and the tree was stopped
	// Leftover means the stage exited but a descendant kept one of its pipes
	// open; Run stopped reading after waitDelay, so later output is lost.
	Leftover bool
	Err      error
	Duration time.Duration
}

// Run starts the stage, waits for it, and stops its whole tree if ctx ends
// first.
func Run(ctx context.Context, s Spec) Result {
	cmd := exec.Command(s.Path, s.Args...)
	cmd.Env, cmd.Dir = s.Env, s.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.Stdin, s.Stdout, s.Stderr
	cmd.WaitDelay = waitDelay
	grace := s.Grace
	if grace <= 0 {
		grace = DefaultGrace
	}

	start := time.Now()
	t := prepare(cmd)
	if err := t.start(); err != nil {
		return Result{ExitCode: -1, Err: err, Duration: time.Since(start)}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var err error
	timedOut := false
	select {
	case err = <-done:
	case <-ctx.Done():
		// A stage that finished at the same moment is not a timeout.
		select {
		case err = <-done:
		default:
			timedOut = true
			err = t.stop(done, grace)
		}
	}
	t.release()

	r := Result{Started: true, ExitCode: -1, TimedOut: timedOut, Duration: time.Since(start)}
	if cmd.ProcessState != nil && !t.forced() {
		r.ExitCode = cmd.ProcessState.ExitCode()
	}
	var exitErr *exec.ExitError
	switch {
	case err == nil, errors.As(err, &exitErr):
	case errors.Is(err, exec.ErrWaitDelay):
		r.Leftover = true
	default:
		r.Err = err
	}
	return r
}
