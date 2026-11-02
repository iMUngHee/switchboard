package service

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Agent describes a launchd user agent that runs switchboard in the user's
// GUI session and keeps it running.
type Agent struct {
	Label      string // e.g. "io.github.imunghee.switchboard"
	Exe        string // absolute
	Args       []string
	StdoutPath string // absolute; normally a log file in the state dir
	StderrPath string // absolute
}

// agentLabel keeps a label to what can safely name the plist file.
var agentLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// AgentPlist renders the agent's property list. RunAtLoad starts it when the
// session does, KeepAlive restarts it whenever it exits, and ProcessType
// Background tells macOS it is a daemon, not something a person waits on.
func AgentPlist(a Agent) ([]byte, error) {
	if !agentLabel.MatchString(a.Label) {
		return nil, fmt.Errorf("agent label %q must be letters, digits and ._- only", a.Label)
	}
	for _, p := range []struct{ what, path string }{
		{"program", a.Exe}, {"stdout log", a.StdoutPath}, {"stderr log", a.StderrPath},
	} {
		if !path.IsAbs(p.path) {
			return nil, fmt.Errorf("agent %s %q is not an absolute path", p.what, p.path)
		}
	}

	var b strings.Builder
	str := func(indent, s string) error {
		e, err := plistString(s)
		if err != nil {
			return err
		}
		b.WriteString(indent + "<string>" + e + "</string>\n")
		return nil
	}
	key := func(k string) { b.WriteString("\t<key>" + k + "</key>\n") }

	b.WriteString(plistHead)
	key("Label")
	if err := str("\t", a.Label); err != nil {
		return nil, err
	}
	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, w := range append([]string{a.Exe}, a.Args...) {
		if err := str("\t\t", w); err != nil {
			return nil, err
		}
	}
	b.WriteString("\t</array>\n")
	key("RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("KeepAlive")
	b.WriteString("\t<true/>\n")
	key("ProcessType")
	b.WriteString("\t<string>Background</string>\n")
	key("StandardOutPath")
	if err := str("\t", a.StdoutPath); err != nil {
		return nil, err
	}
	key("StandardErrorPath")
	if err := str("\t", a.StderrPath); err != nil {
		return nil, err
	}
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String()), nil
}

const plistHead = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`

// plistString escapes s for a <string> element. What XML 1.0 cannot carry at
// all — NUL and most other control characters — is refused rather than
// replaced, so launchd reads back exactly s or nothing.
func plistString(s string) (string, error) {
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%q is not valid UTF-8", s)
	}
	for _, r := range s {
		if !(r == '\t' || r == '\n' || r == '\r' || r >= 0x20 && r <= 0xD7FF || r >= 0xE000 && r <= 0xFFFD || r >= 0x10000) {
			return "", fmt.Errorf("%q holds a character a property list cannot carry", s)
		}
	}
	var b strings.Builder
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return "", err
	}
	return b.String(), nil
}

// AgentPath is where a user agent's plist lives:
// ~/Library/LaunchAgents/<label>.plist.
func AgentPath(label string) (string, error) {
	if !agentLabel.MatchString(label) {
		return "", fmt.Errorf("agent label %q must be letters, digits and ._- only", label)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "LaunchAgents", label+".plist"), nil
}

// guiDomain is the launchd domain of the logged-in user's GUI session.
func guiDomain() string {
	return "gui/" + strconv.Itoa(os.Getuid())
}

// InstallAgent writes the plist and (re)loads it, which also starts it. A
// loaded copy is booted out first, so a changed plist takes effect now.
func InstallAgent(a Agent) error {
	plist, err := AgentPlist(a)
	if err != nil {
		return err
	}
	p, err := AgentPath(a.Label)
	if err != nil {
		return err
	}
	if err := writeConfig(p, plist); err != nil {
		return err
	}
	if err := bootout(a.Label); err != nil {
		return err
	}
	_, err = run("launchctl", "bootstrap", guiDomain(), p)
	return err
}

// UninstallAgent stops and unloads the agent and removes its plist. An agent
// that is not there is not an error.
func UninstallAgent(label string) error {
	p, err := AgentPath(label)
	if err != nil {
		return err
	}
	if err := bootout(label); err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// bootout unloads the agent, which stops it. One that is not loaded is fine:
// launchctl says "No such process" for it, or "Could not find" on older
// releases.
func bootout(label string) error {
	out, err := run("launchctl", "bootout", guiDomain()+"/"+label)
	if err != nil && !strings.Contains(out, "No such process") && !strings.Contains(out, "Could not find") {
		return err
	}
	return nil
}

// AgentStatus asks launchd about the agent. One launchd does not know is not
// registered, which is not an error.
func AgentStatus(label string) (Status, error) {
	if !agentLabel.MatchString(label) {
		return Status{}, fmt.Errorf("agent label %q must be letters, digits and ._- only", label)
	}
	out, err := run("launchctl", "print", guiDomain()+"/"+label)
	if err != nil {
		if strings.Contains(out, "Could not find service") {
			return Status{}, nil
		}
		return Status{}, err
	}
	s := Status{Registered: true}
	for _, line := range strings.Split(out, "\n") {
		// Only the service's own properties, one tab in: nested blocks such
		// as endpoints carry state lines of their own.
		if !strings.HasPrefix(line, "\t") || strings.HasPrefix(line, "\t\t") {
			continue
		}
		k, v, _ := strings.Cut(strings.TrimSpace(line), " = ")
		switch k {
		case "state":
			s.Running = v == "running"
		case "pid":
			s.PID, _ = strconv.Atoi(v)
		}
	}
	if !s.Running {
		s.PID = 0
	}
	return s, nil
}
