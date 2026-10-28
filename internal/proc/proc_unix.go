//go:build !windows

package proc

import (
	"os/exec"
	"syscall"
	"time"
)

// tree is one stage and the process group it leads.
type tree struct{ cmd *exec.Cmd }

func prepare(cmd *exec.Cmd) *tree {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &tree{cmd: cmd}
}

func (t *tree) start() error { return t.cmd.Start() }

// stop sends SIGTERM to the group, waits up to grace for the stage to exit,
// then SIGKILLs the group. Whatever is left of the group is killed either way,
// so no descendant outlives the stage.
func (t *tree) stop(done <-chan error, grace time.Duration) error {
	pgid := t.cmd.Process.Pid
	signalGroup(pgid, syscall.SIGTERM)
	var err error
	select {
	case err = <-done:
	case <-time.After(grace):
		signalGroup(pgid, syscall.SIGKILL)
		err = <-done
	}
	signalGroup(pgid, syscall.SIGKILL)
	return err
}

func (t *tree) release() {}

// forced is false on Unix: a stage killed by a signal already reports -1.
func (t *tree) forced() bool { return false }

// signalGroup signals every process in the group. An error means the group is
// already gone, which is the outcome wanted.
func signalGroup(pgid int, sig syscall.Signal) { _ = syscall.Kill(-pgid, sig) }

// ContainSelf is a no-op on Unix: each stage's process group is stopped on
// its own deadline, and a service manager stops the daemon's.
func ContainSelf() error { return nil }
