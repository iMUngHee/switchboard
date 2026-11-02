//go:build smoke

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/paths"
	"github.com/iMUngHee/switchboard/internal/testenv"
)

// TestServiceSmoke installs switchboard as this user's real service, sees it
// carry mail and hold its link, and uninstalls it, leaving nothing behind.
// It changes the machine's service registrations, so it runs only in CI:
//
//	SWITCHBOARD_SERVICE_SMOKE=1 go test -tags smoke -run TestServiceSmoke ./cmd/switchboard
func TestServiceSmoke(t *testing.T) {
	if os.Getenv("SWITCHBOARD_SERVICE_SMOKE") != "1" {
		t.Skip("installs a real service; set SWITCHBOARD_SERVICE_SMOKE=1 to run it")
	}
	if runtime.GOOS == "linux" && exec.Command("systemctl", "--user", "show-environment").Run() != nil {
		t.Skip("no user systemd answers here")
	}

	dir := t.TempDir()
	bin := filepath.Join(dir, "switchboard"+exeSuffix())
	build := func(out string, ldflags ...string) {
		args := append([]string{"build", "-o", out}, ldflags...)
		if out, err := exec.Command("go", append(args, ".")...).CombinedOutput(); err != nil {
			t.Fatalf("build: %v\n%s", err, out)
		}
	}
	build(bin)
	if runtime.GOOS == "windows" {
		build(filepath.Join(dir, "switchboardw.exe"), "-ldflags=-H=windowsgui")
	}

	s := testenv.NewStores(t)
	cfg := writeConfig(t, s.Pager, s.A, []string{s.Pager}, map[string]string{"PAGER_DB": s.B})
	sb := func(args ...string) (string, error) {
		out, err := exec.Command(bin, args...).CombinedOutput()
		return string(out), err
	}
	t.Cleanup(func() { sb("service", "uninstall", "--config", cfg) })

	if out, err := sb("service", "install", "--config", cfg); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	waitFor(t, "the service to hold its link", 60*time.Second, func() bool {
		out, _ := sb("service", "status", "--config", cfg)
		return strings.Contains(out, "vm: held by a running switchboard")
	})
	if out, _ := sb("service", "status", "--config", cfg); !strings.Contains(out, "service: installed") {
		t.Fatalf("status:\n%s", out)
	}

	s.Run(t, s.A, "send", "kipa@vm", "carried by the service", "--session", "s-a")
	waitFor(t, "the service to carry the mail", 60*time.Second, func() bool {
		return strings.Contains(string(s.Run(t, s.B, "export")), "carried by the service")
	})
	if runtime.GOOS != "linux" { // systemd keeps the log in the journal
		stateDir, _ := paths.StateDir()
		log, err := os.ReadFile(filepath.Join(stateDir, "switchboard.log"))
		if err != nil || !strings.Contains(string(log), "switchboard running") {
			t.Fatalf("the service's log: %v\n%s", err, log)
		}
	}

	if out, err := sb("service", "uninstall", "--config", cfg); err != nil || !strings.Contains(out, "no switchboard holds any link") {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if out, _ := sb("service", "status", "--config", cfg); !strings.Contains(out, "service: not installed") || !strings.Contains(out, "vm: no switchboard holds it") {
		t.Fatalf("status after uninstall:\n%s", out)
	}
	if runtime.GOOS == "windows" {
		waitFor(t, "the daemon to be gone", 15*time.Second, func() bool { return !running(t, filepath.Join(dir, "switchboardw.exe")) })
	}
}

func exeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}
