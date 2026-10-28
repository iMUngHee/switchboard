package service

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// Task describes a Task Scheduler task that runs a console-less binary for
// one user from their logon, and keeps it running.
type Task struct {
	Name        string // e.g. "switchboard"
	Description string
	UserID      string // DOMAIN\user; InstallTask fills in the current user
	Exe         string // absolute path, normally switchboardw.exe
	Args        []string
	WorkDir     string
}

// TaskXML renders the task definition schtasks /Create /XML takes: UTF-16LE
// with a byte-order mark. The settings that matter for a daemon:
//
//   - ExecutionTimeLimit PT0S: without it Task Scheduler stops the task after
//     72 hours;
//   - restart on failure, every minute, many times;
//   - IgnoreNew, so a second trigger never starts a second copy;
//   - no battery conditions, so a laptop on battery keeps relaying;
//   - the user named in both the trigger and the principal: a logon trigger
//     without one fires for any user, which takes an administrator.
func TaskXML(t Task) ([]byte, error) {
	if t.UserID == "" {
		return nil, errors.New("task has no user")
	}
	var args strings.Builder
	for i, a := range t.Args {
		if i > 0 {
			args.WriteByte(' ')
		}
		args.WriteString(quoteWindowsArg(a))
	}
	var esc bytes.Buffer
	for _, v := range []*string{&t.Description, &t.UserID, &t.Exe, &t.WorkDir} {
		esc.Reset()
		if err := xml.EscapeText(&esc, []byte(*v)); err != nil {
			return nil, err
		}
		*v = esc.String()
	}
	esc.Reset()
	if err := xml.EscapeText(&esc, []byte(args.String())); err != nil {
		return nil, err
	}
	body := fmt.Sprintf(taskTemplate, t.Description, t.UserID, t.UserID, t.Exe, esc.String(), t.WorkDir)
	return utf16LE(body), nil
}

const taskTemplate = `<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo>
    <Description>%s</Description>
  </RegistrationInfo>
  <Triggers>
    <LogonTrigger>
      <Enabled>true</Enabled>
      <UserId>%s</UserId>
    </LogonTrigger>
  </Triggers>
  <Principals>
    <Principal id="Author">
      <UserId>%s</UserId>
      <LogonType>InteractiveToken</LogonType>
      <RunLevel>LeastPrivilege</RunLevel>
    </Principal>
  </Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <AllowHardTerminate>true</AllowHardTerminate>
    <StartWhenAvailable>true</StartWhenAvailable>
    <RunOnlyIfNetworkAvailable>false</RunOnlyIfNetworkAvailable>
    <IdleSettings>
      <StopOnIdleEnd>false</StopOnIdleEnd>
      <RestartOnIdle>false</RestartOnIdle>
    </IdleSettings>
    <AllowStartOnDemand>true</AllowStartOnDemand>
    <Enabled>true</Enabled>
    <Hidden>false</Hidden>
    <RunOnlyIfIdle>false</RunOnlyIfIdle>
    <WakeToRun>false</WakeToRun>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <Priority>7</Priority>
    <RestartOnFailure>
      <Interval>PT1M</Interval>
      <Count>999</Count>
    </RestartOnFailure>
  </Settings>
  <Actions Context="Author">
    <Exec>
      <Command>%s</Command>
      <Arguments>%s</Arguments>
      <WorkingDirectory>%s</WorkingDirectory>
    </Exec>
  </Actions>
</Task>
`

// quoteWindowsArg escapes one argument the way Go's syscall.EscapeArg does,
// so CommandLineToArgvW reads it back unchanged. Only an empty argument or
// one with a space or tab is quoted. Backslashes are literal unless they
// precede a quote: a run of them is doubled before a quote, plus one to
// escape it, and doubled before a closing quote.
func quoteWindowsArg(a string) string {
	if a == "" {
		return `""`
	}
	quote := strings.ContainsAny(a, " \t")
	var b strings.Builder
	if quote {
		b.WriteByte('"')
	}
	slashes := 0
	for i := 0; i < len(a); i++ {
		switch a[i] {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
		default:
			slashes = 0
		}
		b.WriteByte(a[i])
	}
	if quote {
		b.WriteString(strings.Repeat(`\`, slashes))
		b.WriteByte('"')
	}
	return b.String()
}

func utf16LE(s string) []byte {
	u := utf16.Encode([]rune(s))
	out := make([]byte, 2, 2+2*len(u))
	out[0], out[1] = 0xFF, 0xFE
	for _, c := range u {
		out = append(out, byte(c), byte(c>>8))
	}
	return out
}

// schtasks runs the Windows Task Scheduler CLI from System32, never one found
// first on PATH.
func schtasks(args ...string) (string, error) {
	root := os.Getenv("SystemRoot")
	if root == "" {
		return "", fmt.Errorf("SystemRoot is not set")
	}
	out, err := exec.Command(filepath.Join(root, "System32", "schtasks.exe"), args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("schtasks %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// InstallTask registers (or replaces) the task, for the current user unless
// t names one, and starts it now.
func InstallTask(t Task) error {
	if t.UserID == "" {
		u, err := user.Current()
		if err != nil {
			return err
		}
		t.UserID = u.Username
	}
	def, err := TaskXML(t)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp("", "switchboard-task-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "task.xml")
	if err := os.WriteFile(path, def, 0o600); err != nil {
		return err
	}
	if _, err := schtasks("/Create", "/TN", t.Name, "/XML", path, "/F"); err != nil {
		return err
	}
	_, err = schtasks("/Run", "/TN", t.Name)
	return err
}

// UninstallTask stops the task if it runs, then removes it.
func UninstallTask(name string) error {
	_, _ = schtasks("/End", "/TN", name)
	_, err := schtasks("/Delete", "/TN", name, "/F")
	return err
}
