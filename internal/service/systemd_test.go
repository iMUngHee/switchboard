package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// execStartWords reads an ExecStart value back the way systemd.service(5)
// describes it: %-specifiers expanded over the whole line ("%%" is "%"),
// then words split on whitespace outside double quotes, with \\ and \"
// undone inside them, then "$$" in each word read as "$". It accepts only
// what execWord writes, so a word that would mean something else to systemd
// — an unescaped specifier or variable — is an error here.
func execStartWords(line string) ([]string, error) {
	var spec strings.Builder
	for i := 0; i < len(line); i++ {
		if line[i] == '%' {
			if i+1 >= len(line) || line[i+1] != '%' {
				return nil, fmt.Errorf("specifier at %d", i)
			}
			i++
		}
		spec.WriteByte(line[i])
	}
	s := spec.String()
	var words []string
	for len(s) > 0 {
		if s[0] == ' ' {
			s = s[1:]
			continue
		}
		if s[0] != '"' {
			return nil, fmt.Errorf("unquoted word at %q", s)
		}
		var w strings.Builder
		i := 1
		for ; i < len(s) && s[i] != '"'; i++ {
			if s[i] == '\\' {
				i++
			}
			w.WriteByte(s[i])
		}
		if i == len(s) {
			return nil, errors.New("unterminated quote")
		}
		s = s[i+1:]
		word := w.String()
		var v strings.Builder
		for j := 0; j < len(word); j++ {
			if word[j] == '$' {
				if j+1 >= len(word) || word[j+1] != '$' {
					return nil, fmt.Errorf("variable in %q", word)
				}
				j++
			}
			v.WriteByte(word[j])
		}
		words = append(words, v.String())
	}
	return words, nil
}

// TestExecStartQuoting: every word comes out as written, a path with a space
// and words holding %h, $HOME, a quote and a backslash included, and what a
// unit file cannot carry is refused.
func TestExecStartQuoting(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/home/me/bin/switch board", `"/home/me/bin/switch board"`},
		{"%h", `"%%h"`},
		{"$HOME", `"$$HOME"`},
		{"${HOME}", `"$${HOME}"`},
		{`say "hi"`, `"say \"hi\""`},
		{`C:\dir\`, `"C:\\dir\\"`},
		{"tab\there", "\"tab\there\""},
		{"", `""`},
		{"ünïcode ✓", `"ünïcode ✓"`},
		{";x", `";x"`},
		{"-", `"-"`},
	} {
		got, err := execWord(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("execWord(%q) = %s, %v; want %s", tc.in, got, err, tc.want)
			continue
		}
		if back, err := execStartWords(got); err != nil || !reflect.DeepEqual(back, []string{tc.in}) {
			t.Errorf("%s reads back as %q, %v; want %q", got, back, err, tc.in)
		}
	}
	for _, bad := range []string{"line\nbreak", "cr\rhere", "nul\x00", "\x1b[31m", "del\x7f", ";", "\xff"} {
		if got, err := execWord(bad); err == nil {
			t.Errorf("execWord(%q) = %s, want it refused", bad, got)
		}
	}

	// The whole ExecStart line of a unit reads back to Exe and Args.
	unit, err := UnitFile(goldenUnit)
	if err != nil {
		t.Fatal(err)
	}
	var line string
	for _, l := range strings.Split(string(unit), "\n") {
		if v, ok := strings.CutPrefix(l, "ExecStart="); ok {
			line = v
		}
	}
	words, err := execStartWords(line)
	if want := append([]string{goldenUnit.Exe}, goldenUnit.Args...); err != nil || !reflect.DeepEqual(words, want) {
		t.Errorf("ExecStart=%s reads back as %q, %v; want %q", line, words, err, want)
	}
}

// TestUnitFileRefuses: a unit name that could name a file elsewhere, a
// relative program, and a description systemd would read differently are
// refused.
func TestUnitFileRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Unit)
	}{
		{"empty name", func(u *Unit) { u.Name = "" }},
		{"name with a slash", func(u *Unit) { u.Name = "a/b" }},
		{"name that climbs out", func(u *Unit) { u.Name = "../x" }},
		{"hidden name", func(u *Unit) { u.Name = ".x" }},
		{"relative program", func(u *Unit) { u.Exe = "bin/switchboard" }},
		{"no program", func(u *Unit) { u.Exe = "" }},
		{"program path with a quote", func(u *Unit) { u.Exe = `/opt/a"b/switchboard` }},
		{"program path with an apostrophe", func(u *Unit) { u.Exe = "/opt/it's/switchboard" }},
		{"program path with a backslash", func(u *Unit) { u.Exe = `/opt/a\b/switchboard` }},
		{"program path with a $", func(u *Unit) { u.Exe = "/opt/$HOME/switchboard" }},
		{"two-line description", func(u *Unit) { u.Description = "one\nExecStartPre=/bin/false" }},
		{"description ending in a backslash", func(u *Unit) { u.Description = `trailing \` }},
		{"argument with a line break", func(u *Unit) { u.Args = []string{"run\nExecStartPre=/bin/false"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := goldenUnit
			u.Args = append([]string(nil), goldenUnit.Args...)
			tc.mutate(&u)
			if b, err := UnitFile(u); err == nil {
				t.Errorf("UnitFile accepted it:\n%s", b)
			}
		})
	}
	u := goldenUnit
	u.Description = "50% of %n"
	b, err := UnitFile(u)
	if err != nil || !strings.Contains(string(b), "\nDescription=50%% of %%n\n") {
		t.Errorf("a description with %% signs:\n%s\n%v", b, err)
	}
}

// TestUnitPath: the unit goes under $XDG_CONFIG_HOME, or ~/.config when that
// is unset or relative.
func TestUnitPath(t *testing.T) {
	home := homeFor(t)
	xdg := filepath.Join(home, "xdg")
	for _, tc := range []struct{ xdg, want string }{
		{xdg, filepath.Join(xdg, "systemd", "user", "switchboard.service")},
		{"", filepath.Join(home, ".config", "systemd", "user", "switchboard.service")},
		{filepath.Join("relative", "xdg"), filepath.Join(home, ".config", "systemd", "user", "switchboard.service")},
	} {
		t.Setenv("XDG_CONFIG_HOME", tc.xdg)
		if p, err := UnitPath("switchboard"); err != nil || p != tc.want {
			t.Errorf("XDG_CONFIG_HOME=%q: %q, %v; want %q", tc.xdg, p, err, tc.want)
		}
	}
	if p, err := UnitPath("../x"); err == nil {
		t.Errorf("UnitPath(../x) = %q", p)
	}
}

// TestInstallUnit: install writes the unit with mode 0644, reloads, enables
// and restarts it, so a reinstall with changed arguments takes effect now;
// the first failure stops it.
func TestInstallUnit(t *testing.T) {
	home := homeFor(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	unitPath := filepath.Join(home, ".config", "systemd", "user", "switchboard.service")
	want, _ := UnitFile(goldenUnit)

	calls := stubRun(t, nil)
	if err := InstallUnit(goldenUnit); err != nil {
		t.Fatalf("InstallUnit: %v", err)
	}
	wantCalls(t, calls,
		"systemctl --user daemon-reload",
		"systemctl --user enable switchboard.service",
		"systemctl --user restart switchboard.service")
	wantFile(t, unitPath, want)

	calls = stubRun(t, func(cmd string) (string, error) {
		return "Failed to connect to bus: No medium found\n", errors.New("exit status 1")
	})
	if err := InstallUnit(goldenUnit); err == nil {
		t.Fatal("InstallUnit went on past a failed daemon-reload")
	}
	wantCalls(t, calls, "systemctl --user daemon-reload")
}

// TestUninstallUnit: uninstall disables and stops the unit, removes the file
// and reloads; a unit that is already gone is not an error, but a systemctl
// that cannot be reached is, and then the file stays.
func TestUninstallUnit(t *testing.T) {
	home := homeFor(t)
	t.Setenv("XDG_CONFIG_HOME", "")
	unitPath := filepath.Join(home, ".config", "systemd", "user", "switchboard.service")
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(unitPath, []byte("unit"), 0o644); err != nil {
		t.Fatal(err)
	}
	disable := "systemctl --user disable --now switchboard.service"
	reload := "systemctl --user daemon-reload"

	calls := stubRun(t, func(cmd string) (string, error) {
		if cmd == disable {
			return "Failed to connect to bus: No medium found\n", errors.New("exit status 1")
		}
		return "", nil
	})
	if err := UninstallUnit("switchboard"); err == nil {
		t.Error("UninstallUnit went on past an unreachable systemd")
	}
	wantCalls(t, calls, disable)
	if _, err := os.Stat(unitPath); err != nil {
		t.Errorf("unit file removed anyway: %v", err)
	}

	calls = stubRun(t, nil)
	if err := UninstallUnit("switchboard"); err != nil {
		t.Fatalf("UninstallUnit: %v", err)
	}
	wantCalls(t, calls, disable, reload)
	if _, err := os.Stat(unitPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unit file still there: %v", err)
	}

	calls = stubRun(t, func(cmd string) (string, error) {
		if cmd == disable {
			return "Failed to disable unit: Unit file switchboard.service does not exist.\n", errors.New("exit status 1")
		}
		return "", nil
	})
	if err := UninstallUnit("switchboard"); err != nil {
		t.Errorf("UninstallUnit with nothing installed: %v", err)
	}
	wantCalls(t, calls, disable, reload)
}

// TestUnitStatus reads systemctl show: loaded or not, active or not, and the
// main PID only while it runs.
func TestUnitStatus(t *testing.T) {
	show := "systemctl --user show switchboard.service -p LoadState -p ActiveState -p MainPID"
	for _, tc := range []struct {
		name string
		out  string
		err  error
		want Status
		fail bool
	}{
		{"running", "MainPID=4242\nLoadState=loaded\nActiveState=active\n", nil, Status{Registered: true, Running: true, PID: 4242}, false},
		{"stopped", "MainPID=0\nLoadState=loaded\nActiveState=inactive\n", nil, Status{Registered: true}, false},
		{"failed, with a stale PID", "MainPID=99\nLoadState=loaded\nActiveState=failed\n", nil, Status{Registered: true}, false},
		{"not installed", "MainPID=0\nLoadState=not-found\nActiveState=inactive\n", nil, Status{}, false},
		{"systemctl fails", "Failed to connect to bus: No medium found\n", errors.New("exit status 1"), Status{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := stubRun(t, func(string) (string, error) { return tc.out, tc.err })
			s, err := UnitStatus("switchboard")
			if (err != nil) != tc.fail || s != tc.want {
				t.Errorf("UnitStatus = %+v, %v; want %+v, failing %v", s, err, tc.want, tc.fail)
			}
			wantCalls(t, calls, show)
		})
	}
}

// TestLingerHint: a hint only when loginctl says lingering is off; nothing
// when it is on or loginctl cannot be asked.
func TestLingerHint(t *testing.T) {
	u, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	ask := "loginctl show-user " + u.Username + " -p Linger"
	for _, tc := range []struct {
		name, out string
		err       error
		hint      bool
	}{
		{"off", "Linger=no\n", nil, true},
		{"on", "Linger=yes\n", nil, false},
		{"no loginctl", "", errors.New("loginctl is not in /usr/bin or /bin"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := stubRun(t, func(string) (string, error) { return tc.out, tc.err })
			h := LingerHint()
			if tc.hint != (h != "") || tc.hint && !strings.Contains(h, "loginctl enable-linger") {
				t.Errorf("LingerHint = %q, want a hint: %v", h, tc.hint)
			}
			wantCalls(t, calls, ask)
		})
	}
}

// TestUnitVerifies has systemd itself read a unit whose program path holds a
// space and a %, the characters systemd allows there that need care: verify
// fails if the path does not come back as the file it names. It needs
// systemd-analyze, so it runs on Linux only; the golden test pins the same
// unit everywhere.
func TestUnitVerifies(t *testing.T) {
	analyze, err := findTool("systemd-analyze", toolDirs...)
	if err != nil {
		t.Skip("systemd-analyze is not installed")
	}
	dir := t.TempDir()
	exeDir := filepath.Join(dir, "odd dir 100%")
	if err := os.MkdirAll(exeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(exeDir, "switch board")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	u := Unit{Name: "switchboard-verify", Description: "verify 100%", Exe: exe, Args: []string{"run", "%h", "$HOME", `"q"`, `\`}}
	unit, err := UnitFile(u)
	if err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(dir, u.Name+".service")
	if err := os.WriteFile(unitPath, unit, 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(analyze, "verify", "--user", unitPath)
	if os.Getenv("XDG_RUNTIME_DIR") == "" {
		cmd.Env = append(os.Environ(), "XDG_RUNTIME_DIR="+t.TempDir())
	}
	// verify exits 1 for a program it cannot find or would not run; its
	// output may also carry warnings that have nothing to do with the unit,
	// such as having no system bus to talk to.
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(string(out), "not executable") {
		t.Errorf("systemd-analyze verify: %v\n%s\n%s", err, out, unit)
	}
}
