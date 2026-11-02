package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/iMUngHee/switchboard/internal/service"
)

const (
	serviceName = "switchboard"                    // systemd unit and Windows task
	agentLabel  = "io.github.imunghee.switchboard" // launchd
	description = "switchboard: carries pager mail between pager stores"
	// uninstallWait is how long uninstall waits for the stopped daemon to
	// release its links.
	uninstallWait = 15 * time.Second
)

// cmdService installs, removes and reports the per-user service that runs
// `switchboard run` with this configuration: a launchd agent on macOS, a
// systemd user unit on Linux, a Task Scheduler task on Windows.
func cmdService(args []string, stdout, stderr io.Writer) int {
	usage := "usage: switchboard service install|uninstall|status [--config FILE] [--exe FILE] [--print]"
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	sub := args[0]
	fs := flag.NewFlagSet("service "+sub, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "configuration file the service runs with")
	exeFlag := fs.String("exe", "", "the switchboard to run; this one when empty")
	printOnly := fs.Bool("print", false, "print what install would write, and change nothing")
	pos, code, ok := flags(fs, args[1:])
	if !ok {
		return code
	}
	if len(pos) > 0 || (sub != "install" && sub != "uninstall" && sub != "status") {
		fmt.Fprintln(stderr, usage)
		return 2
	}
	a, err := load(*cfgFlag, stderr)
	if err != nil && sub == "uninstall" {
		// The service goes even when its configuration has; only the wait for
		// the links, which needs it, is skipped.
		fmt.Fprintf(stderr, "switchboard: %v; removing the service without waiting for its links\n", err)
		if err := uninstallService(); err != nil {
			return fail(stderr, err)
		}
		fmt.Fprintln(stdout, "uninstalled")
		return 0
	}
	if err != nil {
		return fail(stderr, err)
	}
	switch sub {
	case "install":
		return a.serviceInstall(*exeFlag, *printOnly, stdout, stderr)
	case "uninstall":
		return a.serviceUninstall(stdout, stderr)
	default:
		return a.serviceStatus(stdout, stderr)
	}
}

// definition is the service install writes, for this system.
type definition struct {
	exe     string // what the service runs
	text    []byte // what goes on disk, for --print
	install func() error
}

func (a *app) definition(exe string) (definition, error) {
	exe, err := daemonExe(exe)
	if err != nil {
		return definition{}, err
	}
	args := []string{"run", "--config", a.cfgPath}
	logPath := filepath.Join(a.stateDir, "switchboard.log")
	switch runtime.GOOS {
	case "darwin":
		ag := service.Agent{Label: agentLabel, Exe: exe, Args: append(args, "--log", logPath),
			// What launchd itself catches: a crash before the log is open.
			StdoutPath: filepath.Join(a.stateDir, "launchd.log"), StderrPath: filepath.Join(a.stateDir, "launchd.log")}
		text, err := service.AgentPlist(ag)
		return definition{exe, text, func() error { return service.InstallAgent(ag) }}, err
	case "linux":
		// The journal keeps the log.
		u := service.Unit{Name: serviceName, Description: description, Exe: exe, Args: args}
		text, err := service.UnitFile(u)
		return definition{exe, text, func() error { return service.InstallUnit(u) }}, err
	case "windows":
		// The task is this user's, at this user's logon.
		u, err := user.Current()
		if err != nil {
			return definition{}, err
		}
		t := service.Task{Name: serviceName, Description: description, UserID: u.Username, Exe: exe, Args: append(args, "--log", logPath), WorkDir: a.stateDir}
		text, err := service.TaskXML(t)
		return definition{exe, text, func() error { return service.InstallTask(t) }}, err
	}
	return definition{}, fmt.Errorf("switchboard cannot install a service on %s", runtime.GOOS)
}

// daemonExe is the binary the service runs: the one asked for, or this one.
// On Windows it is the console-less switchboardw.exe beside it, so no
// console window opens at logon.
func daemonExe(exe string) (string, error) {
	if exe == "" {
		self, err := invokedAs()
		if err != nil {
			return "", err
		}
		exe = self
		if runtime.GOOS == "windows" {
			exe = filepath.Join(filepath.Dir(self), "switchboardw.exe")
		}
	}
	exe, err := filepath.Abs(exe)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(exe); err != nil {
		return "", fmt.Errorf("the service would run %s, which is missing: %w", exe, err)
	}
	return exe, nil
}

// invokedAs is this binary by the path it was started with, when that is the
// same file: a stable link such as ~/.local/bin/switchboard keeps working
// after an upgrade replaces what it points to, where the resolved path
// (which os.Executable gives on Linux) would name a removed version.
func invokedAs() (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	return startedAs(os.Args[0], self), nil
}

// startedAs is arg0, found on PATH when it is a bare name, if it is the same
// file as self; otherwise self.
func startedAs(arg0, self string) string {
	if !strings.ContainsRune(arg0, filepath.Separator) && !strings.ContainsRune(arg0, '/') {
		if found, err := exec.LookPath(arg0); err == nil {
			arg0 = found
		}
	}
	if abs, err := filepath.Abs(arg0); err == nil {
		a, errA := os.Stat(abs)
		b, errB := os.Stat(self)
		if errA == nil && errB == nil && os.SameFile(a, b) {
			return abs
		}
	}
	return self
}

// installDefinition installs a definition; the tests replace it, so that no
// test can install a real service even if --print stopped working.
var installDefinition = func(d definition) error { return d.install() }

func (a *app) serviceInstall(exe string, printOnly bool, stdout, stderr io.Writer) int {
	def, err := a.definition(exe)
	if err != nil {
		return fail(stderr, err)
	}
	if printOnly {
		stdout.Write(printable(def.text))
		return 0
	}
	if err := installDefinition(def); err != nil {
		return fail(stderr, err)
	}
	fmt.Fprintf(stdout, "installed and started: %s run --config %s\n", def.exe, a.cfgPath)
	switch runtime.GOOS {
	case "linux":
		if hint := service.LingerHint(); hint != "" {
			fmt.Fprintln(stdout, hint)
		}
	case "windows":
		if len(a.cfg.Notify.Command) == 0 {
			fmt.Fprintf(stdout, "Windows has no notifier switchboard uses by itself: a link that needs you is written to %s; set [notify] command to be told another way.\n",
				filepath.Join(a.stateDir, "switchboard.log"))
		}
	}
	return 0
}

// printable is the definition as text. The Windows task is UTF-16, which a
// terminal shows as noise.
func printable(text []byte) []byte {
	if len(text) < 2 || text[0] != 0xFF || text[1] != 0xFE {
		return text
	}
	out := make([]byte, 0, len(text)/2)
	for i := 2; i+1 < len(text); i += 2 {
		if text[i+1] == 0 {
			out = append(out, text[i])
		} else {
			out = append(out, '?')
		}
	}
	return out
}

// uninstall stops and removes this system's service.
func uninstall() error {
	switch runtime.GOOS {
	case "darwin":
		return service.UninstallAgent(agentLabel)
	case "linux":
		return service.UninstallUnit(serviceName)
	case "windows":
		return service.UninstallTask(serviceName)
	}
	return fmt.Errorf("switchboard has no service on %s", runtime.GOOS)
}

// uninstallService removes the service; the tests replace it, as they do
// installDefinition.
var uninstallService = uninstall

func (a *app) serviceUninstall(stdout, stderr io.Writer) int {
	if err := uninstallService(); err != nil {
		return fail(stderr, err)
	}
	// The service manager stopped the daemon; wait for it to let go.
	held, err := a.heldLinks(context.Background(), uninstallWait)
	if err != nil {
		return fail(stderr, err)
	}
	if len(held) > 0 {
		for _, n := range held {
			fmt.Fprintf(stdout, "%s: still held by a switchboard process after %v\n", n, uninstallWait)
		}
		return 1
	}
	fmt.Fprintln(stdout, "uninstalled; no switchboard holds any link")
	return 0
}

func (a *app) serviceStatus(stdout, stderr io.Writer) int {
	var st service.Status
	var err error
	switch runtime.GOOS {
	case "darwin":
		st, err = service.AgentStatus(agentLabel)
	case "linux":
		st, err = service.UnitStatus(serviceName)
	case "windows":
		st, err = service.TaskStatus(serviceName)
	default:
		err = fmt.Errorf("switchboard has no service on %s", runtime.GOOS)
	}
	if err != nil {
		return fail(stderr, err)
	}
	switch {
	case !st.Registered:
		fmt.Fprintln(stdout, "service: not installed")
	case st.Running && st.PID > 0:
		fmt.Fprintf(stdout, "service: installed, running (pid %d)\n", st.PID)
	case st.Running:
		fmt.Fprintln(stdout, "service: installed, running")
	case runtime.GOOS == "windows":
		fmt.Fprintln(stdout, "service: installed")
	default:
		fmt.Fprintln(stdout, "service: installed, not running")
	}
	held, err := a.heldLinks(context.Background(), 0)
	if err != nil {
		return fail(stderr, err)
	}
	names, _ := a.links(nil)
	for _, n := range names {
		state := "no switchboard holds it"
		for _, h := range held {
			if h == n {
				state = "held by a running switchboard"
			}
		}
		fmt.Fprintf(stdout, "%s: %s\n", n, state)
	}
	return 0
}

// heldLinks returns the links whose lock a process holds, waiting up to
// wait for them to be released.
func (a *app) heldLinks(ctx context.Context, wait time.Duration) ([]string, error) {
	names, _ := a.links(nil)
	keys := map[string]string{}
	for _, n := range names {
		k, err := a.key(ctx, n)
		if err != nil {
			return nil, err
		}
		keys[n] = k
	}
	deadline := time.Now().Add(wait)
	for {
		var held []string
		for _, n := range names {
			h, err := lockHeld(a.stateDir, keys[n])
			if err != nil {
				return nil, err
			}
			if h {
				held = append(held, n)
			}
		}
		if len(held) == 0 || !time.Now().Before(deadline) {
			return held, nil
		}
		time.Sleep(250 * time.Millisecond)
	}
}
