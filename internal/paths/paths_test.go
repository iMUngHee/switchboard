package paths

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestConfigFilePrecedence: --config wins over SWITCHBOARD_CONFIG, which wins
// over the default, and a relative path comes back absolute.
func TestConfigFilePrecedence(t *testing.T) {
	root := t.TempDir()
	fromFlag := filepath.Join(root, "flag.toml")
	fromEnv := filepath.Join(root, "env.toml")
	t.Setenv("SWITCHBOARD_CONFIG", fromEnv)

	if p, src, err := ConfigFile(fromFlag); err != nil || p != fromFlag || src != SourceFlag {
		t.Errorf("flag and env: %q %q %v, want the flag's path from %q", p, src, err, SourceFlag)
	}
	if p, src, err := ConfigFile(""); err != nil || p != fromEnv || src != SourceEnv {
		t.Errorf("env only: %q %q %v, want the env path from %q", p, src, err, SourceEnv)
	}
	t.Chdir(root)
	if p, src, err := ConfigFile("rel.toml"); err != nil || p != filepath.Join(root, "rel.toml") || src != SourceFlag {
		t.Errorf("relative flag: %q %q %v, want it made absolute", p, src, err)
	}
	t.Setenv("SWITCHBOARD_CONFIG", "")
	if _, src, err := ConfigFile(""); err != nil || src != SourceDefault {
		t.Errorf("neither: source %q, err %v, want %q", src, err, SourceDefault)
	}
}

// TestDefaultLocations pins the per-system defaults and the environment that
// moves them. Only the current system's cases run.
func TestDefaultLocations(t *testing.T) {
	t.Setenv("SWITCHBOARD_CONFIG", "")
	root := t.TempDir()
	home := filepath.Join(root, "home")

	switch runtime.GOOS {
	case "windows":
		t.Run("AppData and LocalAppData", func(t *testing.T) {
			t.Setenv("APPDATA", filepath.Join(root, "Roaming"))
			t.Setenv("LOCALAPPDATA", filepath.Join(root, "Local"))
			wantPaths(t, filepath.Join(root, "Roaming", "switchboard", "config.toml"), filepath.Join(root, "Local", "switchboard"))
		})
		t.Run("unset AppData is an error", func(t *testing.T) {
			t.Setenv("APPDATA", "")
			t.Setenv("LOCALAPPDATA", "")
			if p, _, err := ConfigFile(""); err == nil {
				t.Errorf("ConfigFile = %q with %%AppData%% unset, want an error", p)
			}
			if p, err := StateDir(); err == nil {
				t.Errorf("StateDir = %q with %%LocalAppData%% unset, want an error", p)
			}
		})
	case "darwin":
		t.Run("XDG config, Application Support state", func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state")) // not used on macOS
			wantPaths(t, filepath.Join(root, "xdg", "switchboard", "config.toml"),
				filepath.Join(home, "Library", "Application Support", "switchboard"))
		})
		t.Run("home fallback", func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", "")
			wantPaths(t, filepath.Join(home, ".config", "switchboard", "config.toml"),
				filepath.Join(home, "Library", "Application Support", "switchboard"))
		})
	default:
		t.Run("XDG config and state", func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "xdg"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg-state"))
			wantPaths(t, filepath.Join(root, "xdg", "switchboard", "config.toml"), filepath.Join(root, "xdg-state", "switchboard"))
		})
		t.Run("home fallback", func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", "")
			t.Setenv("XDG_STATE_HOME", "")
			wantPaths(t, filepath.Join(home, ".config", "switchboard", "config.toml"), filepath.Join(home, ".local", "state", "switchboard"))
		})
	}
	if runtime.GOOS != "windows" {
		t.Run("relative XDG values are ignored", func(t *testing.T) {
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", "relative/config")
			t.Setenv("XDG_STATE_HOME", "relative/state")
			p, _, err := ConfigFile("")
			if want := filepath.Join(home, ".config", "switchboard", "config.toml"); err != nil || p != want {
				t.Errorf("ConfigFile = %q, %v; want %q", p, err, want)
			}
			if runtime.GOOS == "linux" {
				if p, err := StateDir(); err != nil || p != filepath.Join(home, ".local", "state", "switchboard") {
					t.Errorf("StateDir = %q, %v; want the home fallback", p, err)
				}
			}
		})
	}
}

func wantPaths(t *testing.T, config, state string) {
	t.Helper()
	if p, src, err := ConfigFile(""); err != nil || p != config || src != SourceDefault {
		t.Errorf("ConfigFile = %q %q %v; want %q %q", p, src, err, config, SourceDefault)
	}
	if p, err := StateDir(); err != nil || p != state {
		t.Errorf("StateDir = %q, %v; want %q", p, err, state)
	}
}
