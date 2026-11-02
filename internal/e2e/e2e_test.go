//go:build e2e

// Package e2e carries real mail both ways between two real pager stores
// with the switchboard command itself. CI runs it on every system with the
// far store behind an exec endpoint, and on Linux once more through the ssh
// preset to a localhost sshd:
//
//	go test -tags e2e ./internal/e2e
//
// For the ssh run, SWITCHBOARD_E2E_SSH_PORT and SWITCHBOARD_E2E_SSH_KEY name
// the sshd and the client key, and SWITCHBOARD_E2E_FAR_DB the far store's
// database, which the far side must give pager: the Linux job's sshd does it
// with SetEnv. SWITCHBOARD_E2E_FAR_PAGER, when set, is the far pager as the
// far shell names it; the Windows job points it at a Git Bash wrapper that
// sets the database and runs pager.exe. Never point these at a real store:
// the test enrolls peers in it and sends mail.
package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/pagercli"
	"github.com/iMUngHee/switchboard/internal/testenv"
)

func TestRoundTripBothWays(t *testing.T) {
	port, farDB := os.Getenv("SWITCHBOARD_E2E_SSH_PORT"), os.Getenv("SWITCHBOARD_E2E_FAR_DB")
	if (port == "") != (farDB == "") {
		t.Fatal("set both SWITCHBOARD_E2E_SSH_PORT and SWITCHBOARD_E2E_FAR_DB, or neither")
	}
	bin := build(t)
	s := testenv.NewStoresAt(t, farDB)

	lit := func(v string) string { return "'" + v + "'" }
	far := fmt.Sprintf("[link.vm.exec]\nargv = [%s]\nenv = { PAGER_DB = %s }\n", lit(s.Pager), lit(s.B))
	if port != "" {
		farPager := s.Pager
		if p := os.Getenv("SWITCHBOARD_E2E_FAR_PAGER"); p != "" {
			farPager = p
		}
		known := filepath.Join(t.TempDir(), "known_hosts")
		far = fmt.Sprintf("[link.vm.ssh]\nhost = '127.0.0.1'\npager = %s\noptions = ['-p', %s, '-i', %s, '-o', 'StrictHostKeyChecking=accept-new', '-o', %s]\n",
			lit(farPager), lit(port), lit(os.Getenv("SWITCHBOARD_E2E_SSH_KEY")), lit("UserKnownHostsFile="+known))
	}
	// config writes a configuration for the link, with linkKeys added to it.
	config := func(linkKeys string) string {
		cfg := filepath.Join(t.TempDir(), "config.toml")
		text := fmt.Sprintf("version = 1\n\n[pager]\npath = %s\ndb = %s\n\n[link.vm]\nremote_peer = 'laptop'\n%s\n%s", lit(s.Pager), lit(s.A), linkKeys, far)
		if err := os.WriteFile(cfg, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	cfg := config("")
	env := sandbox(t)
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("switchboard %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return string(out)
	}

	if out := run("check", "--config", cfg); !strings.Contains(out, "vm: ok") {
		t.Fatalf("check:\n%s", out)
	}
	const toFar, toNear = "laptop to vm, once", "vm to laptop, once"
	s.Run(t, s.A, "send", "kipa@vm", toFar, "--session", "s-a")
	s.Run(t, s.B, "send", "sape@laptop", toNear, "--session", "s-b")
	if a, b := pending(t, s, s.A), pending(t, s, s.B); a != 1 || b != 1 {
		t.Fatalf("pending before: laptop %d, vm %d; want 1 each", a, b)
	}

	out := run("once", "--config", cfg)
	for _, want := range []string{"vm outbound: ok, 1 records, 1 with a final result", "vm inbound: ok, 1 records, 1 with a final result"} {
		if !strings.Contains(out, want) {
			t.Fatalf("once lacks %q:\n%s", want, out)
		}
	}
	if a, b := pending(t, s, s.A), pending(t, s, s.B); a != 0 || b != 0 {
		t.Fatalf("pending after: laptop %d, vm %d; want 0 each", a, b)
	}
	for _, c := range []struct{ db, body string }{{s.B, toFar}, {s.A, toNear}} {
		if n := strings.Count(string(s.Run(t, c.db, "export")), c.body); n != 1 {
			t.Fatalf("%s holds %q %d times, want once", filepath.Base(c.db), c.body, n)
		}
	}

	// Then the daemon, with polls far longer than the test: mail crosses
	// both ways because each side's relay wait says it is waiting.
	t.Run("without polling", func(t *testing.T) {
		cfg := config("poll_outbound = '10m'\npoll_inbound = '10m'\n")
		logf, err := os.Create(filepath.Join(t.TempDir(), "run.log"))
		if err != nil {
			t.Fatal(err)
		}
		defer logf.Close()
		daemon := exec.Command(bin, "run", "--config", cfg)
		daemon.Env, daemon.Stdout, daemon.Stderr = env, logf, logf
		if err := daemon.Start(); err != nil {
			t.Fatal(err)
		}
		exited := make(chan error, 1)
		go func() { exited <- daemon.Wait() }()
		var once sync.Once
		halt := func() { once.Do(func() { stop(t, daemon, exited) }) }
		defer halt()
		fail := func(format string, a ...any) {
			t.Helper()
			halt()
			b, _ := os.ReadFile(logf.Name())
			t.Fatalf(format+"\nrun log:\n%s", append(a, b)...)
		}

		// Idle: the daemon's first rounds found nothing, so both directions wait.
		idle := func() bool {
			cmd := exec.Command(bin, "status", "--config", cfg, "--json")
			cmd.Env = env
			out, err := cmd.Output()
			var st struct {
				Links []struct {
					Daemon            bool
					Outbound, Inbound struct{ Class string }
				}
			}
			return err == nil && json.Unmarshal(out, &st) == nil && len(st.Links) == 1 && st.Links[0].Daemon &&
				st.Links[0].Outbound.Class == "idle" && st.Links[0].Inbound.Class == "idle"
		}
		if !eventually(30*time.Second, idle) {
			fail("the daemon never went idle")
		}

		const toFar, toNear = "laptop to vm, waited for", "vm to laptop, waited for"
		sent := time.Now()
		s.Run(t, s.A, "send", "kipa@vm", toFar, "--session", "s-a")
		s.Run(t, s.B, "send", "sape@laptop", toNear, "--session", "s-b")
		arrived := func() bool {
			return strings.Contains(string(s.Run(t, s.B, "export")), toFar) &&
				strings.Contains(string(s.Run(t, s.A, "export")), toNear)
		}
		if !eventually(10*time.Second, arrived) {
			fail("mail did not cross within 10s with 10m polls")
		}
		t.Logf("both crossed %v after sending", time.Since(sent).Round(100*time.Millisecond))
	})
}

// eventually reports whether cond held within d, checking every 100ms.
func eventually(d time.Duration, cond func() bool) bool {
	for deadline := time.Now().Add(d); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return false
}

// stop ends a switchboard run: an interrupt where there is one, so it ends
// its waits itself; on Windows the process is killed, and the job it put
// itself in ends what it started.
func stop(t *testing.T, daemon *exec.Cmd, exited chan error) {
	t.Helper()
	select {
	case <-exited:
		return
	default:
	}
	if runtime.GOOS == "windows" {
		_ = daemon.Process.Kill()
	} else {
		_ = daemon.Process.Signal(os.Interrupt)
	}
	select {
	case <-exited:
	case <-time.After(30 * time.Second):
		_ = daemon.Process.Kill()
		<-exited
		t.Error("switchboard run did not stop within 30s of an interrupt")
	}
}

// build compiles switchboard for this test.
func build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "switchboard")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	if out, err := exec.Command("go", "build", "-o", bin, "github.com/iMUngHee/switchboard/cmd/switchboard").CombinedOutput(); err != nil {
		t.Fatalf("build switchboard: %v\n%s", err, out)
	}
	return bin
}

// sandbox is the environment for switchboard: every per-user location in a
// temporary directory, so it reads no real configuration and keeps its
// state there.
func sandbox(t *testing.T) []string {
	t.Helper()
	home := t.TempDir()
	return append(os.Environ(),
		"HOME="+home, "USERPROFILE="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, "config"), "XDG_STATE_HOME="+filepath.Join(home, "state"),
		"APPDATA="+filepath.Join(home, "AppData", "Roaming"), "LOCALAPPDATA="+filepath.Join(home, "AppData", "Local"),
		"SWITCHBOARD_CONFIG=")
}

func pending(t *testing.T, s *testenv.Stores, db string) int {
	t.Helper()
	peers, err := pagercli.ParsePeerList(s.Run(t, db, "peer", "ls"))
	if err != nil || len(peers) != 1 {
		t.Fatalf("peer ls: %v %+v", err, peers)
	}
	return peers[0].Pending
}
