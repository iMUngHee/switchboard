package service

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/iMUngHee/switchboard/internal/paths"
)

// Unit describes a systemd user unit that runs switchboard and restarts it
// when it fails.
type Unit struct {
	Name        string // without ".service", e.g. "switchboard"
	Description string
	Exe         string // absolute
	Args        []string
}

// unitName keeps a unit name to what can safely name the unit file.
var unitName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// UnitFile renders the unit. KillMode=mixed sends the stop signal to
// switchboard alone, so its own shutdown can let in-flight relay stages
// finish (relay in still wakes its recipients) before it stops them; the
// default would signal every process in the unit at once. TimeoutStopSec
// leaves room for that shutdown's 10s drain before systemd kills what
// remains.
func UnitFile(u Unit) ([]byte, error) {
	if !unitName.MatchString(u.Name) {
		return nil, fmt.Errorf("unit name %q must be letters, digits and _.- only", u.Name)
	}
	if !path.IsAbs(u.Exe) {
		return nil, fmt.Errorf("unit program %q is not an absolute path", u.Exe)
	}
	desc, err := unitDescription(u.Description)
	if err != nil {
		return nil, err
	}
	// systemd runs a program path only when it holds no quote, backslash or
	// control character, and it expands no $VARIABLE in the path, so doubling
	// a $ there would change it. Such a path cannot be written down at all.
	if strings.ContainsAny(u.Exe, `"'\$`) {
		return nil, fmt.Errorf(`unit program %q: systemd cannot run a path holding a quote, a backslash or "$"`, u.Exe)
	}
	words := make([]string, 0, 1+len(u.Args))
	for _, w := range append([]string{u.Exe}, u.Args...) {
		q, err := execWord(w)
		if err != nil {
			return nil, err
		}
		words = append(words, q)
	}
	return fmt.Appendf(nil, unitTemplate, desc, strings.Join(words, " ")), nil
}

const unitTemplate = `# Written by switchboard service install; reinstalling replaces it.
[Unit]
Description=%s

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
RestartSec=10s
KillMode=mixed
TimeoutStopSec=30s

[Install]
WantedBy=default.target
`

// execWord quotes one ExecStart word so systemd hands the program exactly w.
// Inside double quotes systemd undoes C escapes, so \ and " are escaped; it
// expands %-specifiers across the whole line and $VARIABLES in each argument,
// even quoted ones, so % and $ are doubled. A unit file is read line by line,
// so a line break or other control character cannot be carried and is
// refused, as is a word that is a lone ";", which systemd treats as a
// command separator.
func execWord(w string) (string, error) {
	if !utf8.ValidString(w) {
		return "", fmt.Errorf("ExecStart word %q is not valid UTF-8", w)
	}
	if w == ";" {
		return "", errors.New(`ExecStart word ";" would separate commands`)
	}
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(w); i++ {
		switch c := w[i]; {
		case c < 0x20 && c != '\t' || c == 0x7f:
			return "", fmt.Errorf("ExecStart word %q holds a line break or control character", w)
		case c == '\\' || c == '"':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '%':
			b.WriteString("%%")
		case c == '$':
			b.WriteString("$$")
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String(), nil
}

// unitDescription makes s safe for Description=: one line, % doubled, since
// systemd expands specifiers there too, and no trailing backslash, which
// would join the next line onto it.
func unitDescription(s string) (string, error) {
	if !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return "", fmt.Errorf("unit description %q must be one line of text", s)
	}
	if strings.HasSuffix(s, `\`) {
		return "", fmt.Errorf("unit description %q must not end with a backslash", s)
	}
	return strings.ReplaceAll(s, "%", "%%"), nil
}

// UnitPath is where a user unit lives:
// $XDG_CONFIG_HOME/systemd/user/<name>.service, or under ~/.config.
func UnitPath(name string) (string, error) {
	if !unitName.MatchString(name) {
		return "", fmt.Errorf("unit name %q must be letters, digits and _.- only", name)
	}
	dir, err := paths.ConfigHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "systemd", "user", name+".service"), nil
}

// InstallUnit writes the unit, enables it and (re)starts it. Restarting
// rather than starting is what makes a reinstall with changed arguments take
// effect at once; enable --now would leave the old process running.
func InstallUnit(u Unit) error {
	unit, err := UnitFile(u)
	if err != nil {
		return err
	}
	p, err := UnitPath(u.Name)
	if err != nil {
		return err
	}
	if err := writeConfig(p, unit); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"--user", "daemon-reload"},
		{"--user", "enable", u.Name + ".service"},
		{"--user", "restart", u.Name + ".service"},
	} {
		if _, err := run("systemctl", args...); err != nil {
			return err
		}
	}
	return nil
}

// UninstallUnit stops and disables the unit, removes its file and reloads.
// A unit that is not there is not an error.
func UninstallUnit(name string) error {
	p, err := UnitPath(name)
	if err != nil {
		return err
	}
	out, err := run("systemctl", "--user", "disable", "--now", name+".service")
	if err != nil && !strings.Contains(out, "does not exist") && !strings.Contains(out, "not loaded") {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	_, err = run("systemctl", "--user", "daemon-reload")
	return err
}

// UnitStatus asks systemd about the unit. A unit systemd cannot find is not
// registered, which is not an error.
func UnitStatus(name string) (Status, error) {
	if !unitName.MatchString(name) {
		return Status{}, fmt.Errorf("unit name %q must be letters, digits and _.- only", name)
	}
	out, err := run("systemctl", "--user", "show", name+".service", "-p", "LoadState", "-p", "ActiveState", "-p", "MainPID")
	if err != nil {
		return Status{}, err
	}
	prop := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			prop[k] = v
		}
	}
	s := Status{
		Registered: prop["LoadState"] != "" && prop["LoadState"] != "not-found",
		Running:    prop["ActiveState"] == "active" || prop["ActiveState"] == "reloading",
	}
	if s.Running {
		s.PID, _ = strconv.Atoi(prop["MainPID"])
	}
	return s, nil
}

// LingerHint says what to run so the unit keeps running while the user is
// logged out, or "" when lingering is already on or loginctl cannot tell. It
// only reads; enabling it is left to the person.
func LingerHint() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	out, err := run("loginctl", "show-user", u.Username, "-p", "Linger")
	if err != nil || strings.TrimSpace(out) != "Linger=no" {
		return ""
	}
	return "Run `loginctl enable-linger` so switchboard keeps running while you are logged out."
}
