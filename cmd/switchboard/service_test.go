package main

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/iMUngHee/switchboard/internal/paths"
	"github.com/iMUngHee/switchboard/internal/testenv"
)

// TestServiceInstallPrints: --print shows the definition install would write
// for this system, running this configuration, and installs nothing.
func TestServiceInstallPrints(t *testing.T) {
	sandbox(t)
	noRealService(t)
	s := testenv.NewStores(t)
	cfg := writeConfig(t, s.Pager, s.A, []string{s.Pager}, map[string]string{"PAGER_DB": s.B})
	exe := filepath.Join(t.TempDir(), "switchboard")
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	code, out, errb := cli(t, "service", "install", "--print", "--config", cfg, "--exe", exe)
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		if code != 1 {
			t.Fatalf("exit %d on %s", code, runtime.GOOS)
		}
		return
	}
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, errb)
	}
	stateDir, _ := paths.StateDir()
	want := []string{exe, "run", "--config", cfg}
	if runtime.GOOS == "windows" {
		u, err := user.Current()
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, "<UserId>"+u.Username+"</UserId>") // the task is this user's
	}
	if runtime.GOOS != "linux" { // systemd keeps the log in the journal
		want = append(want, "--log", filepath.Join(stateDir, "switchboard.log"))
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("the definition lacks %q:\n%s", w, out)
		}
	}
	if code, _, _ := cli(t, "service", "install", "--print", "--config", cfg, "--exe", filepath.Join(t.TempDir(), "missing")); code != 1 {
		t.Fatalf("a missing binary was accepted: exit %d", code)
	}
	if code, _, _ := cli(t, "service", "frobnicate", "--config", cfg); code != 2 {
		t.Fatalf("unknown subcommand: exit %d", code)
	}
}

// TestLogFileRotates: the daemon's log moves aside at its size limit, and
// only logFiles files are ever kept.
func TestLogFileRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "switchboard.log")
	l, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	line := []byte(strings.Repeat("x", 1023) + "\n")
	for range 4 * maxLogSize / len(line) {
		if _, err := l.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	files, _ := filepath.Glob(path + "*")
	if len(files) != logFiles {
		t.Fatalf("files %q, want %d", files, logFiles)
	}
	for _, f := range files {
		st, err := os.Stat(f)
		if err != nil || st.Size() > maxLogSize || st.Size()%int64(len(line)) != 0 {
			t.Fatalf("%s: %v, size %d: over the limit or cut mid-record", f, err, st.Size())
		}
	}
}

// noRealService makes any install or uninstall in this test fail it, so no
// test ever changes the machine's real services.
func noRealService(t *testing.T) {
	t.Helper()
	install, remove := installDefinition, uninstallService
	installDefinition = func(definition) error { t.Fatal("a real service was installed"); return nil }
	uninstallService = func() error { t.Fatal("a real service was removed"); return nil }
	t.Cleanup(func() { installDefinition, uninstallService = install, remove })
}

// TestServiceCommands: install, uninstall and status say what they did, with
// the service manager stubbed out.
func TestServiceCommands(t *testing.T) {
	sandbox(t)
	s := testenv.NewStores(t)
	cfg := writeConfig(t, s.Pager, s.A, []string{s.Pager}, map[string]string{"PAGER_DB": s.B})
	exe := filepath.Join(t.TempDir(), "switchboard")
	if err := os.WriteFile(exe, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	var installed, removed int
	install, remove := installDefinition, uninstallService
	t.Cleanup(func() { installDefinition, uninstallService = install, remove })
	installDefinition = func(d definition) error {
		installed++
		if d.exe != exe || len(d.text) == 0 {
			t.Errorf("installing %q, %d bytes", d.exe, len(d.text))
		}
		return nil
	}
	uninstallService = func() error { removed++; return nil }

	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" || runtime.GOOS == "windows" {
		if out := mustCLI(t, "service", "install", "--config", cfg, "--exe", exe); !strings.Contains(out, "installed and started: "+exe+" run --config "+cfg) {
			t.Fatalf("install: %q", out)
		}
	}
	if out := mustCLI(t, "service", "uninstall", "--config", cfg); !strings.Contains(out, "no switchboard holds any link") {
		t.Fatalf("uninstall: %q", out)
	}

	t.Run("uninstall without a configuration still removes the service", func(t *testing.T) {
		code, out, errb := cli(t, "service", "uninstall", "--config", filepath.Join(t.TempDir(), "gone.toml"))
		if code != 0 || !strings.Contains(out, "uninstalled") || !strings.Contains(errb, "without waiting") {
			t.Fatalf("exit %d\n%s\n%s", code, out, errb)
		}
	})
	t.Run("a failing service manager is an error", func(t *testing.T) {
		uninstallService = func() error { return errors.New("no user bus") }
		if code, _, errb := cli(t, "service", "uninstall", "--config", cfg); code != 1 || !strings.Contains(errb, "no user bus") {
			t.Fatalf("exit %d: %s", code, errb)
		}
	})
	if removed != 2 {
		t.Fatalf("removed %d times, want 2", removed)
	}
}

// TestStartedAs: the service runs this binary by the path it was started
// with when that is the same file, so a link that survives upgrades is kept.
func TestStartedAs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need a privilege on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "v1", "switchboard")
	os.MkdirAll(filepath.Dir(target), 0o755)
	if err := os.WriteFile(target, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	link := filepath.Join(bin, "switchboard")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if got := startedAs(link, target); got != link {
		t.Errorf("by its link: %q, want %q", got, link)
	}
	t.Setenv("PATH", bin)
	if got := startedAs("switchboard", target); got != link {
		t.Errorf("by name on PATH: %q, want %q", got, link)
	}
	other := filepath.Join(dir, "other")
	os.WriteFile(other, nil, 0o755)
	if got := startedAs(other, target); got != target {
		t.Errorf("by another file: %q, want the binary itself", got)
	}
}

// TestLogFileCountsAfterAFailedRotation: when the old log cannot be moved
// aside, the reopened file's real size is counted, so the next write tries
// again instead of letting the file grow unseen.
func TestLogFileCountsAfterAFailedRotation(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a read-only directory stops renames only for a user that is not root, and not on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "switchboard.log")
	l, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	l.Write([]byte("x\n"))
	os.Chmod(dir, 0o500) // renames in it now fail
	defer os.Chmod(dir, 0o700)
	l.rotate()
	l.Write([]byte("y\n"))
	if st, _ := os.Stat(path); l.size != st.Size() || l.size != 4 {
		t.Fatalf("counted %d, the file holds %d", l.size, st.Size())
	}
}
