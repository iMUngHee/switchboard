// Package service installs switchboard as a per-user background service:
// a launchd agent on macOS, a systemd user unit on Linux, a Task Scheduler
// task on Windows. Each runs as the user whose pager store and hooks it
// serves; a system-wide service would run under another account and relay
// the wrong store.
//
// Each kind has a pure renderer (AgentPlist, UnitFile, TaskXML) and functions
// that install, uninstall and report on it by running the OS's own tool,
// always from a fixed system directory and never from PATH.
package service

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Status is what the service manager says about the service.
type Status struct {
	Registered bool // the manager knows it
	Running    bool
	PID        int // 0 when it is not running
}

// toolDirs are the only places launchctl, systemctl and loginctl are taken
// from. PATH is never consulted: under a service manager it is minimal, and
// anywhere else something earlier on it could stand in for the real tool.
var toolDirs = []string{"/usr/bin", "/bin"}

// run runs an OS tool by name and returns its combined output, which it also
// returns with an error so callers can tell "not loaded" from a failure.
// Tests replace it, so no test touches the real launchd or systemd.
var run = func(name string, args ...string) (string, error) {
	path, err := findTool(name, toolDirs...)
	if err != nil {
		return "", err
	}
	out, err := exec.Command(path, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// findTool returns the first of dirs that holds a regular file called name.
func findTool(name string, dirs ...string) (string, error) {
	for _, d := range dirs {
		p := filepath.Join(d, name)
		if fi, err := os.Stat(p); err == nil && fi.Mode().IsRegular() {
			return p, nil
		}
	}
	return "", fmt.Errorf("%s is not in %s", name, strings.Join(dirs, " or "))
}

// writeConfig writes a service definition with mode 0644 whatever the umask
// or an older file's mode: launchd ignores a plist that group or others can
// write.
func writeConfig(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return err
	}
	return os.Chmod(path, 0o644)
}
