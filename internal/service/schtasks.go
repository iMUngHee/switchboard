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
	"regexp"
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

// TaskStatus reports whether the task is registered, asking Task Scheduler
// about that one task (taskQuery) rather than listing every task, which a
// standard user may be refused. Whether its process runs is not read: Task
// Scheduler's state names are localized, and the link locks say whether a
// switchboard holds the links.
func TaskStatus(name string) (Status, error) {
	path, args, err := taskQuery(name)
	if err != nil {
		return Status{}, err
	}
	var stderr bytes.Buffer
	cmd := exec.Command(path, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return Status{}, fmt.Errorf("Get-ScheduledTask %s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	registered, err := taskAnswer(string(out))
	return Status{Registered: registered}, err
}

// taskName is what a task name may be before it is written into a
// PowerShell script: no quote of any kind (PowerShell takes the typographic
// ones as quotes too), no $ or backtick, no wildcard, which -TaskName would
// match, and no \, which would name a folder.
var taskName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// taskQuery is the command TaskStatus runs: Windows PowerShell from System32,
// never one found on PATH, asking for the task of this name in the root
// folder, where schtasks /TN puts it. A task that is not there prints
// "absent", told by its error id, which unlike the message is not localized.
// Any other error, a refusal say, is rethrown, so PowerShell fails rather
// than answering for a task it could not read.
func taskQuery(name string) (path string, args []string, err error) {
	if !taskName.MatchString(name) {
		return "", nil, fmt.Errorf("task name %q must be letters, digits and _.- only", name)
	}
	root := os.Getenv("SystemRoot")
	if root == "" {
		return "", nil, errors.New("SystemRoot is not set")
	}
	script := `try { $null = Get-ScheduledTask -TaskName '` + name + `' -TaskPath '\' -ErrorAction Stop; 'registered' } ` +
		`catch { if ($_.FullyQualifiedErrorId -like 'CmdletizationQuery_NotFound*') { 'absent' } else { throw } }`
	return filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe"),
		[]string{"-NoProfile", "-NonInteractive", "-Command", script}, nil
}

// taskAnswer reads what taskQuery printed: "registered" or "absent", alone.
func taskAnswer(stdout string) (registered bool, err error) {
	switch strings.TrimSpace(stdout) {
	case "registered":
		return true, nil
	case "absent":
		return false, nil
	}
	return false, fmt.Errorf("Get-ScheduledTask answered %q, not registered or absent", stdout)
}
