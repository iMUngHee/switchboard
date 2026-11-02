// Package paths says where switchboard keeps its configuration and its state
// on each system. It only names the places; nothing here creates them.
package paths

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Sources of a config path, as ConfigFile reports them.
const (
	SourceFlag    = "flag"
	SourceEnv     = "env"
	SourceDefault = "default"
)

// ConfigFile returns the configuration file to read and where that choice came
// from: the --config value when given, then SWITCHBOARD_CONFIG, then the
// default for this system. A path given by flag or environment is made
// absolute, so a command that reports it names one file whatever directory it
// ran in.
//
// The default is $XDG_CONFIG_HOME/switchboard/config.toml, or
// ~/.config/switchboard/config.toml, on macOS and Linux, and
// %AppData%\switchboard\config.toml on Windows.
func ConfigFile(flagValue string) (path, source string, err error) {
	if flagValue != "" {
		path, err = filepath.Abs(flagValue)
		return path, SourceFlag, err
	}
	if v := os.Getenv("SWITCHBOARD_CONFIG"); v != "" {
		path, err = filepath.Abs(v)
		return path, SourceEnv, err
	}
	var dir string
	if runtime.GOOS == "windows" {
		dir, err = envDir("APPDATA")
	} else {
		dir, err = ConfigHome()
	}
	if err != nil {
		return "", "", err
	}
	return filepath.Join(dir, "switchboard", "config.toml"), SourceDefault, nil
}

// ConfigHome is the XDG configuration directory on macOS and Linux:
// $XDG_CONFIG_HOME, or ~/.config. The systemd user unit lives under it too.
func ConfigHome() (string, error) {
	return xdgDir("XDG_CONFIG_HOME", ".config")
}

// StateDir returns the directory for state files, locks and control
// requests: $XDG_STATE_HOME/switchboard or ~/.local/state/switchboard on
// Linux, ~/Library/Application Support/switchboard on macOS, and
// %LocalAppData%\switchboard on Windows. Callers create it, with mode 0700.
func StateDir() (string, error) {
	var dir string
	var err error
	switch runtime.GOOS {
	case "windows":
		dir, err = envDir("LOCALAPPDATA")
	case "darwin":
		var home string
		home, err = os.UserHomeDir()
		dir = filepath.Join(home, "Library", "Application Support")
	default:
		dir, err = xdgDir("XDG_STATE_HOME", filepath.Join(".local", "state"))
	}
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "switchboard"), nil
}

// xdgDir returns the XDG base directory named by env, or fallback under the
// home directory. A relative value is ignored, as the XDG specification asks:
// under a service manager it would be relative to wherever that started us.
func xdgDir(env, fallback string) (string, error) {
	if v := os.Getenv(env); v != "" && filepath.IsAbs(v) {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, fallback), nil
}

// envDir returns a Windows known folder from its environment variable. A
// relative value is refused for the reason xdgDir ignores one; there is no
// home fallback to use instead.
func envDir(env string) (string, error) {
	v := os.Getenv(env)
	if v == "" {
		return "", errors.New("%" + env + "% is not set")
	}
	if !filepath.IsAbs(v) {
		return "", fmt.Errorf("%%%s%% is not an absolute path: %q", env, v)
	}
	return v, nil
}
