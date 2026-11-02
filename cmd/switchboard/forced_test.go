package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/paths"
	"github.com/iMUngHee/switchboard/internal/state"
	"github.com/iMUngHee/switchboard/internal/testenv"
)

// TestForcedEndDuringRound: switchboard ended at once, as Task Scheduler
// ending the task does, at three points:
//   - created: a stage's process exists but is not yet in its own job;
//   - before-save: relay in imported a message whose result is not stored;
//   - after-save: the result is stored but not acked.
//
// In each case no child is left and the lock is free, and the next run
// recovers: by resend before the save, by acking the stored result after it,
// with the destination holding the message exactly once.
func TestForcedEndDuringRound(t *testing.T) {
	env := sandbox(t)
	for _, point := range []string{"created", "before-save", "after-save"} {
		t.Run(point, func(t *testing.T) {
			if point == "created" && runtime.GOOS != "windows" {
				t.Skip("only Windows places a stage in its own job after creating it")
			}
			s := testenv.NewStores(t)
			pager := uniqueCopy(t, s.Pager) // so a leftover process can be found by name
			cfg := writeConfig(t, pager, s.A, []string{pager}, map[string]string{"PAGER_DB": s.B})
			body := "ended at " + point
			s.Run(t, s.A, "send", "kipa@vm", body, "--session", "s-a")

			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary(t), "run", "--config", cfg)
			cmd.Env = append(env, testKillEnv+"="+point)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 137 {
				t.Fatalf("switchboard did not end at %s: %v\n%s", point, err, out)
			}

			waitFor(t, "no child left", 5*time.Second, func() bool { return !running(t, pager) })
			stateDir, _ := paths.StateDir()
			idA := strings.TrimSpace(string(s.Run(t, s.A, "relay", "id")))
			key := state.Key(idA, "vm")
			lock, err := state.TryLock(stateDir, key)
			if err != nil {
				t.Fatalf("the lock is still held: %v", err)
			}
			lock.Close()
			saved, err := state.Load(stateDir, key)
			if err != nil {
				t.Fatal(err)
			}
			switch point {
			case "created":
				if count(t, s, s.B, body) != 0 {
					t.Fatal("the message moved before any stage ran")
				}
			case "before-save":
				if count(t, s, s.B, body) != 1 || len(saved.Outbound.PendingAck) != 0 {
					t.Fatalf("imported %d, stored %q: want imported and nothing stored", count(t, s, s.B, body), saved.Outbound.PendingAck)
				}
			case "after-save":
				if count(t, s, s.B, body) != 1 || len(saved.Outbound.PendingAck) != 1 {
					t.Fatalf("imported %d, stored %q: want imported and its result stored", count(t, s, s.B, body), saved.Outbound.PendingAck)
				}
			}
			if pendingOn(t, s, s.A) != 1 {
				t.Fatal("the message was acked before the end")
			}

			mustCLI(t, "once", "--config", cfg)
			if pendingOn(t, s, s.A) != 0 || count(t, s, s.B, body) != 1 {
				t.Fatalf("after recovery: pending %d, delivered %d times", pendingOn(t, s, s.A), count(t, s, s.B, body))
			}
			if saved, _ := state.Load(stateDir, key); len(saved.Outbound.PendingAck) != 0 {
				t.Fatalf("results still stored after recovery: %q", saved.Outbound.PendingAck)
			}
		})
	}
}

// uniqueCopy copies a binary under a name only this test uses, short enough
// that every process listing shows it whole (tasklist's table cuts names at
// 25 characters).
func uniqueCopy(t *testing.T, src string) string {
	t.Helper()
	name := fmt.Sprintf("sb%08x", uint32(time.Now().UnixNano())^uint32(os.Getpid())<<16)
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dst := filepath.Join(t.TempDir(), name)
	in, err := os.Open(src)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return dst
}

// running reports whether any process runs the binary at path.
func running(t *testing.T, path string) bool {
	t.Helper()
	if runtime.GOOS == "windows" {
		out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq "+filepath.Base(path), "/FO", "CSV", "/NH").Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Contains(strings.ToLower(string(out)), `"`+strings.ToLower(filepath.Base(path))+`"`)
	}
	err := exec.Command("pgrep", "-f", filepath.Base(path)).Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

// TestRunningSeesALiveProcess: the leftover check sees a process that is
// there, so its "nothing left" means something.
func TestRunningSeesALiveProcess(t *testing.T) {
	fake := uniqueCopy(t, testenv.TestPager(t))
	plan := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(plan, []byte(`{"relay out":{"sleep_ms":30000}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(fake, "relay", "out")
	cmd.Env = append(os.Environ(), "TESTPAGER_PLAN="+plan)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the process to show", 10*time.Second, func() bool { return running(t, fake) })
	cmd.Process.Kill()
	cmd.Wait()
	waitFor(t, "the process to go", 10*time.Second, func() bool { return !running(t, fake) })
}
