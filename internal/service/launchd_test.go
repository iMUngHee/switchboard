package service

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// TestAgentPlistRefuses: what launchd would misread, or could not read back
// as written, is refused rather than rendered.
func TestAgentPlistRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Agent)
	}{
		{"empty label", func(a *Agent) { a.Label = "" }},
		{"label with a slash", func(a *Agent) { a.Label = "io.example/x" }},
		{"label that climbs out", func(a *Agent) { a.Label = "../x" }},
		{"relative program", func(a *Agent) { a.Exe = "bin/switchboard" }},
		{"no program", func(a *Agent) { a.Exe = "" }},
		{"relative stdout log", func(a *Agent) { a.StdoutPath = "switchboard.log" }},
		{"relative stderr log", func(a *Agent) { a.StderrPath = "switchboard.log" }},
		{"NUL in an argument", func(a *Agent) { a.Args = []string{"run\x00"} }},
		{"escape character in an argument", func(a *Agent) { a.Args = []string{"\x1b[31m"} }},
		{"invalid UTF-8 in an argument", func(a *Agent) { a.Args = []string{"\xff"} }},
		{"a noncharacter XML cannot carry", func(a *Agent) { a.Args = []string{"\ufffe"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := goldenAgent
			a.Args = append([]string(nil), goldenAgent.Args...)
			tc.mutate(&a)
			if b, err := AgentPlist(a); err == nil {
				t.Errorf("AgentPlist accepted it:\n%s", b)
			}
		})
	}
}

// TestAgentPlistReadsBack: macOS's own parser reads the plist back to exactly
// the values it was made from, awkward arguments included. plutil exists
// only on macOS.
func TestAgentPlistReadsBack(t *testing.T) {
	const plutil = "/usr/bin/plutil"
	if _, err := os.Stat(plutil); err != nil {
		t.Skip("plutil is macOS only")
	}
	a := goldenAgent
	a.Args = append(append([]string(nil), goldenAgent.Args...), "tab\there", "line\nbreak", "cr\rhere", "ünïcode ✓", `<&>'"`, "")
	plist, err := AgentPlist(a)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "agent.plist")
	if err := os.WriteFile(file, plist, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(plutil, "-convert", "json", "-o", "-", file).Output()
	if err != nil {
		t.Fatalf("plutil: %v\n%s", err, plist)
	}
	var got struct {
		Label             string
		ProgramArguments  []string
		RunAtLoad         bool
		KeepAlive         bool
		ProcessType       string
		StandardOutPath   string
		StandardErrorPath string
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode plutil's JSON: %v\n%s", err, out)
	}
	if want := append([]string{a.Exe}, a.Args...); !reflect.DeepEqual(got.ProgramArguments, want) {
		t.Errorf("ProgramArguments read back as\n %q\nwant\n %q", got.ProgramArguments, want)
	}
	if got.Label != a.Label || !got.RunAtLoad || !got.KeepAlive || got.ProcessType != "Background" ||
		got.StandardOutPath != a.StdoutPath || got.StandardErrorPath != a.StderrPath {
		t.Errorf("read back as %+v", got)
	}
}

// homeFor points the home directory at a temp dir on every system.
func homeFor(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

func wantFile(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("%s holds\n%s\nwant\n%s", path, got, want)
	}
	if runtime.GOOS == "windows" {
		return // no Unix modes there
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("%s mode = %v (%v), want 0644", path, fi.Mode().Perm(), err)
	}
}

const notLoaded = "Boot-out failed: 3: No such process\n"

// TestInstallAgent: install writes the plist with mode 0644, boots out any
// loaded copy — one that is not loaded is fine — and bootstraps it, running
// launchctl by name through run, never anything else.
func TestInstallAgent(t *testing.T) {
	home := homeFor(t)
	plistPath := filepath.Join(home, "Library", "LaunchAgents", goldenAgent.Label+".plist")
	want, _ := AgentPlist(goldenAgent)
	bootout := "launchctl bootout " + guiDomain() + "/" + goldenAgent.Label
	bootstrap := "launchctl bootstrap " + guiDomain() + " " + plistPath

	t.Run("fresh", func(t *testing.T) {
		calls := stubRun(t, func(cmd string) (string, error) {
			if cmd == bootout {
				return notLoaded, errors.New("exit status 3")
			}
			return "", nil
		})
		if err := InstallAgent(goldenAgent); err != nil {
			t.Fatalf("InstallAgent: %v", err)
		}
		wantCalls(t, calls, bootout, bootstrap)
		wantFile(t, plistPath, want)
	})

	t.Run("over a loaded agent with a looser mode", func(t *testing.T) {
		if err := os.WriteFile(plistPath, []byte("old"), 0o666); err != nil {
			t.Fatal(err)
		}
		os.Chmod(plistPath, 0o666)
		calls := stubRun(t, nil)
		if err := InstallAgent(goldenAgent); err != nil {
			t.Fatalf("InstallAgent: %v", err)
		}
		wantCalls(t, calls, bootout, bootstrap)
		wantFile(t, plistPath, want)
	})

	t.Run("a bootout that fails otherwise stops it", func(t *testing.T) {
		calls := stubRun(t, func(cmd string) (string, error) {
			return "Boot-out failed: 5: Input/output error\n", errors.New("exit status 5")
		})
		if err := InstallAgent(goldenAgent); err == nil {
			t.Fatal("InstallAgent went on past a failed bootout")
		}
		wantCalls(t, calls, bootout)
	})
}

// TestUninstallAgent: uninstall boots the agent out and removes its plist,
// and running it again, with nothing left, is not an error.
func TestUninstallAgent(t *testing.T) {
	home := homeFor(t)
	plistPath := filepath.Join(home, "Library", "LaunchAgents", goldenAgent.Label+".plist")
	bootout := "launchctl bootout " + guiDomain() + "/" + goldenAgent.Label
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(plistPath, []byte("plist"), 0o644); err != nil {
		t.Fatal(err)
	}

	calls := stubRun(t, nil)
	if err := UninstallAgent(goldenAgent.Label); err != nil {
		t.Fatalf("UninstallAgent: %v", err)
	}
	wantCalls(t, calls, bootout)
	if _, err := os.Stat(plistPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist still there: %v", err)
	}

	calls = stubRun(t, func(string) (string, error) { return notLoaded, errors.New("exit status 3") })
	if err := UninstallAgent(goldenAgent.Label); err != nil {
		t.Errorf("UninstallAgent with nothing installed: %v", err)
	}
	wantCalls(t, calls, bootout)
}

// TestAgentStatus reads launchctl print: the service's own state and pid,
// not those of the blocks nested in it, and "Could not find service" as not
// registered.
func TestAgentStatus(t *testing.T) {
	printCmd := "launchctl print " + guiDomain() + "/" + goldenAgent.Label
	for _, tc := range []struct {
		name string
		out  string
		err  error
		want Status
		fail bool
	}{
		{"running", "gui/501/io.github.imunghee.switchboard = {\n\tactive count = 1\n\tpath = /Users/me/Library/LaunchAgents/io.github.imunghee.switchboard.plist\n\tstate = running\n\n\tprogram = /Users/me/bin/switchboard\n\tendpoints = {\n\t\t\"x\" = {\n\t\t\tstate = active\n\t\t\tpid = 7\n\t\t}\n\t}\n\tpid = 4242\n}\n", nil, Status{Registered: true, Running: true, PID: 4242}, false},
		{"loaded, not running", "gui/501/io.github.imunghee.switchboard = {\n\tstate = not running\n\tlast exit code = 1\n\tendpoints = {\n\t\tstate = running\n\t}\n}\n", nil, Status{Registered: true}, false},
		{"not registered", "Bad request.\nCould not find service \"io.github.imunghee.switchboard\" in domain for user gui: 501\n", errors.New("exit status 113"), Status{}, false},
		{"launchctl fails", "Bad request.\n", errors.New("exit status 1"), Status{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := stubRun(t, func(string) (string, error) { return tc.out, tc.err })
			s, err := AgentStatus(goldenAgent.Label)
			if (err != nil) != tc.fail || s != tc.want {
				t.Errorf("AgentStatus = %+v, %v; want %+v, failing %v", s, err, tc.want, tc.fail)
			}
			wantCalls(t, calls, printCmd)
		})
	}
}
