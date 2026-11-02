package service

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite testdata/*.golden from the current renderers")

// The inputs the golden files are made from: one per system, with example
// paths and arguments that hold a space and a quote.
var (
	goldenAgent = Agent{
		Label:      "io.github.imunghee.switchboard",
		Exe:        "/Users/me/Applications/Switch Board/switchboard",
		Args:       []string{"run", "--config", "/Users/me/Library/Application Support/switchboard/config.toml", `--note=say "hi" & <bye>`},
		StdoutPath: "/Users/me/Library/Application Support/switchboard/switchboard.log",
		StderrPath: "/Users/me/Library/Application Support/switchboard/switchboard.log",
	}
	goldenUnit = Unit{
		Name:        "switchboard",
		Description: "switchboard: carries pager mail (100% polling)",
		Exe:         "/home/me/.local/bin/switch board",
		Args:        []string{"run", "--config", "/home/me/.config/switchboard/config.toml", `--note=say "hi" \ $HOME %h`},
	}
	goldenTask = Task{
		Name:        "switchboard",
		Description: "switchboard: carries pager mail",
		UserID:      `EXAMPLE\me`,
		Exe:         `C:\Program Files\switchboard\switchboardw.exe`,
		Args:        []string{"run", "--config", `C:\Users\me\AppData\Roaming\switchboard\config.toml`, `--note=say "hi"`},
		WorkDir:     `C:\Users\me`,
	}
)

// TestServiceGolden pins the three service definitions byte for byte against
// testdata/*.golden; `go test ./internal/service -run TestServiceGolden
// -update` rewrites them. The task XML is compared decoded from UTF-16.
func TestServiceGolden(t *testing.T) {
	plist, err := AgentPlist(goldenAgent)
	if err != nil {
		t.Fatalf("AgentPlist: %v", err)
	}
	unit, err := UnitFile(goldenUnit)
	if err != nil {
		t.Fatalf("UnitFile: %v", err)
	}
	task, err := TaskXML(goldenTask)
	if err != nil {
		t.Fatalf("TaskXML: %v", err)
	}
	for _, g := range []struct {
		file string
		got  []byte
	}{
		{"launchd.plist.golden", plist},
		{"systemd.service.golden", unit},
		{"schtasks.xml.golden", []byte(decodeUTF16LE(t, task))},
	} {
		t.Run(g.file, func(t *testing.T) {
			path := filepath.Join("testdata", g.file)
			if *update {
				if err := os.WriteFile(path, g.got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			// A Windows checkout may have turned the golden file's line ends
			// into CRLF; what the renderer produces is compared as it is.
			want = bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))
			if !bytes.Equal(g.got, want) {
				t.Errorf("%s differs from the renderer's output (run with -update if the change is intended)\n--- got\n%s\n--- want\n%s", path, g.got, want)
			}
		})
	}
}

// stubRun replaces run for one test. It records each command as one line,
// "systemctl --user daemon-reload", and answers with reply, or with no output
// and no error when reply is nil.
func stubRun(t *testing.T, reply func(cmd string) (string, error)) *[]string {
	t.Helper()
	var calls []string
	orig := run
	run = func(name string, args ...string) (string, error) {
		cmd := strings.Join(append([]string{name}, args...), " ")
		calls = append(calls, cmd)
		if reply == nil {
			return "", nil
		}
		return reply(cmd)
	}
	t.Cleanup(func() { run = orig })
	return &calls
}

func wantCalls(t *testing.T, got *[]string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(*got, want) {
		t.Errorf("commands run:\n %q\nwant\n %q", *got, want)
	}
}

// TestToolsComeOnlyFromTheirDirs: an OS tool is taken from the fixed system
// directories and nowhere else — not from PATH, not as a directory.
func TestToolsComeOnlyFromTheirDirs(t *testing.T) {
	if want := []string{"/usr/bin", "/bin"}; !reflect.DeepEqual(toolDirs, want) {
		t.Errorf("toolDirs = %q, want %q", toolDirs, want)
	}
	first, second, onPath := t.TempDir(), t.TempDir(), t.TempDir()
	for _, p := range []string{filepath.Join(second, "systemctl"), filepath.Join(onPath, "systemctl"), filepath.Join(onPath, "launchctl")} {
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(first, "loginctl"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", onPath)

	if p, err := findTool("systemctl", first, second); err != nil || p != filepath.Join(second, "systemctl") {
		t.Errorf("systemctl = %q, %v; want the one in the second dir", p, err)
	}
	if p, err := findTool("launchctl", first, second); err == nil {
		t.Errorf("launchctl = %q, found although it is only on PATH", p)
	}
	if p, err := findTool("loginctl", first, second); err == nil {
		t.Errorf("loginctl = %q, a directory taken for the tool", p)
	}
}
