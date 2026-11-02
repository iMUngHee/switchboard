//go:build !windows

package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/paths"
	"github.com/iMUngHee/switchboard/internal/state"
	"github.com/iMUngHee/switchboard/internal/testenv"
)

// TestGracefulShutdownDuringRound: SIGTERM while relay ack hangs. switchboard
// stops starting rounds, gives the running one its grace, then stops it: no
// child is left, the results it was acking are on disk, the lock is free,
// and it exits 0.
func TestGracefulShutdownDuringRound(t *testing.T) {
	env := sandbox(t)
	const (
		localID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		farID   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		linkID  = "cccccccccccccccccccccccccccccccc"
	)
	record := fmt.Sprintf(`{"v":1,"kind":"msg","link":{"store":%q,"id":%q,"seq":1},"origin":{"store":%q,"id":7},"dst_store":%q,"to":"kipa","from":"sape","label":"sape","hop":0,"sent_at":"2026-10-28T00:00:00Z","body":"hello"}`, localID, linkID, localID, farID)
	result := fmt.Sprintf(`{"v":1,"kind":"result","store":%q,"max_v":1,"link":{"store":%q,"id":%q,"seq":1},"origin_id":7,"status":"imported"}`, farID, localID, linkID)

	dir := t.TempDir()
	plan := func(name string, steps map[string]any) string {
		path := filepath.Join(dir, name)
		raw, _ := json.Marshal(steps)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	localLog := filepath.Join(dir, "local.jsonl")
	localPlan := plan("local.json", map[string]any{
		"relay id":  map[string]any{"stdout": localID + "\n"},
		"relay out": map[string]any{"stdout": record + "\n", "stderr": "pager: to kipa: the launch codes\n"},
		"relay ack": map[string]any{"sleep_ms": 120000}, // hangs until stopped
	})
	farPlan := plan("far.json", map[string]any{
		"relay in":  map[string]any{"stdout": result + "\n"},
		"relay out": map[string]any{},
	})
	fake := testenv.TestPager(t)
	cfg := writeConfig(t, fake, filepath.Join(dir, "unused.db"), []string{fake}, map[string]string{"TESTPAGER_PLAN": farPlan})

	cmd := exec.Command(binary(t), "run", "--config", cfg, "--shutdown-grace", "1s")
	cmd.Env = append(env, "TESTPAGER_PLAN="+localPlan, "TESTPAGER_LOG="+localLog)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })

	var ackPID int
	waitFor(t, "relay ack to start", 30*time.Second, func() bool {
		ackPID = pidOf(t, localLog, "relay ack")
		return ackPID != 0
	})
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	signalled := time.Now()
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("switchboard exited with %v\n%s", err, stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatalf("switchboard did not stop\n%s", stderr.String())
	}
	// A 1s grace, then up to 3s for the stage to take SIGTERM: well under the
	// 10s default, so the flag is what was honoured.
	if took := time.Since(signalled); took > 8*time.Second {
		t.Fatalf("stopping took %v with a 1s grace", took)
	}
	if strings.Contains(stderr.String(), "launch codes") {
		t.Fatalf("a stage's stderr reached the daemon's log:\n%s", stderr.String())
	}

	if err := syscall.Kill(ackPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("relay ack (pid %d) outlived switchboard: %v", ackPID, err)
	}
	stateDir, _ := paths.StateDir()
	key := state.Key(localID, "vm")
	saved, err := state.Load(stateDir, key)
	if err != nil || len(saved.Outbound.PendingAck) != 1 || saved.Outbound.PendingAck[0] != result {
		t.Fatalf("stored results %q, %v", saved.Outbound.PendingAck, err)
	}
	lock, err := state.TryLock(stateDir, key)
	if err != nil {
		t.Fatalf("the lock is still held: %v", err)
	}
	lock.Close()
	if !strings.Contains(stderr.String(), "switchboard stopped") {
		t.Fatalf("no clean stop in the log:\n%s", stderr.String())
	}
}

// pidOf returns the pid testpager logged for the first invocation of sub, or
// 0 when there is none yet.
func pidOf(t *testing.T, log, sub string) int {
	t.Helper()
	f, err := os.Open(log)
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c struct {
			Args []string `json:"args"`
			PID  int      `json:"pid"`
		}
		if json.Unmarshal(sc.Bytes(), &c) == nil && len(c.Args) >= 2 && c.Args[0]+" "+c.Args[1] == sub {
			return c.PID
		}
	}
	return 0
}

// TestRunWaitsForAHeldLink: a link another process holds is waited for, not
// given up on, and runs once it is free.
func TestRunWaitsForAHeldLink(t *testing.T) {
	env := sandbox(t)
	s := testenv.NewStores(t)
	cfg := writeConfig(t, s.Pager, s.A, []string{s.Pager}, map[string]string{"PAGER_DB": s.B})
	stateDir, _ := paths.StateDir()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	idA := strings.TrimSpace(string(s.Run(t, s.A, "relay", "id")))
	lock, err := state.TryLock(stateDir, state.Key(idA, "vm"))
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binary(t), "run", "--config", cfg)
	cmd.Env = env
	var stderr syncBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	waitFor(t, "the daemon to wait for the link", 20*time.Second, func() bool { return strings.Contains(stderr.String(), "waiting for it") })
	time.Sleep(1500 * time.Millisecond)
	if strings.Contains(stderr.String(), "link open") {
		t.Fatal("the link opened while another process held it")
	}
	lock.Close()
	waitFor(t, "the link to open", 20*time.Second, func() bool { return strings.Contains(stderr.String(), "link open") })
	cmd.Process.Signal(syscall.SIGTERM)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("exit %v\n%s", err, stderr.String())
	}
}

// syncBuffer is a strings.Builder safe for a process writing while the test
// reads.
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestRunKeepsWaitingThroughErrors: while it waits for a held link, an error
// opening it (pager failing for a moment) is retried, never a reason to exit.
func TestRunKeepsWaitingThroughErrors(t *testing.T) {
	env := sandbox(t)
	const localID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dir := t.TempDir()
	localPlan := filepath.Join(dir, "local.json")
	setPlan := func(text string) {
		t.Helper()
		if err := os.WriteFile(localPlan, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	good := `{"relay id":{"stdout":"` + localID + `\n"},"relay out":{}}`
	setPlan(good)
	farPlan := filepath.Join(dir, "far.json")
	if err := os.WriteFile(farPlan, []byte(`{"relay out":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := testenv.TestPager(t)
	cfg := writeConfig(t, fake, filepath.Join(dir, "unused.db"), []string{fake}, map[string]string{"TESTPAGER_PLAN": farPlan})
	stateDir, _ := paths.StateDir()
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	lock, err := state.TryLock(stateDir, state.Key(localID, "vm"))
	if err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binary(t), "run", "--config", cfg)
	cmd.Env = append(env, "TESTPAGER_PLAN="+localPlan)
	var stderr syncBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	waitFor(t, "the daemon to wait for the link", 20*time.Second, func() bool { return strings.Contains(stderr.String(), "waiting for it") })
	setPlan(`{"relay id":{"exit":1}}`) // pager fails while the link is held
	waitFor(t, "the failure to be logged", 20*time.Second, func() bool { return strings.Contains(stderr.String(), "cannot open yet") })
	select {
	case err := <-exited:
		t.Fatalf("switchboard exited (%v) while waiting for a link\n%s", err, stderr.String())
	case <-time.After(2 * time.Second):
	}
	setPlan(good)
	lock.Close()
	waitFor(t, "the link to open", 60*time.Second, func() bool { return strings.Contains(stderr.String(), "link open") })
	cmd.Process.Signal(syscall.SIGTERM)
	if err := <-exited; err != nil {
		t.Fatalf("exit %v\n%s", err, stderr.String())
	}
}
