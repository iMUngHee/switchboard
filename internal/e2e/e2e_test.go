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
// database, which the sshd hands pager through SetEnv. Never point these at
// a real store: the test enrolls peers in it and sends mail.
package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

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
		known := filepath.Join(t.TempDir(), "known_hosts")
		far = fmt.Sprintf("[link.vm.ssh]\nhost = '127.0.0.1'\npager = %s\noptions = ['-p', %s, '-i', %s, '-o', 'StrictHostKeyChecking=accept-new', '-o', %s]\n",
			lit(s.Pager), lit(port), lit(os.Getenv("SWITCHBOARD_E2E_SSH_KEY")), lit("UserKnownHostsFile="+known))
	}
	cfg := filepath.Join(t.TempDir(), "config.toml")
	text := fmt.Sprintf("version = 1\n\n[pager]\npath = %s\ndb = %s\n\n[link.vm]\nremote_peer = 'laptop'\n\n%s", lit(s.Pager), lit(s.A), far)
	if err := os.WriteFile(cfg, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
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
