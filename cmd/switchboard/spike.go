package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/iMUngHee/switchboard/internal/proc"
	"github.com/iMUngHee/switchboard/internal/service"
)

// spike is a hidden probe of how this machine runs switchboard as a service:
// it installs a task that heartbeats into a log and keeps starting a console
// child through internal/proc, which records whether it got a console window.
// It exists to prove the Windows service path before the daemon is built on
// it, and is not part of switchboard's documented interface.
func spike(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: switchboard spike install|uninstall|run|child")
		return 2
	}
	fs := flag.NewFlagSet("spike "+args[0], flag.ContinueOnError)
	name := fs.String("name", "switchboard-spike", "task name")
	exe := fs.String("exe", "", "console-less binary the task runs (switchboardw.exe)")
	child := fs.String("child", "", "console binary started as the child (switchboard.exe)")
	logPath := fs.String("log", "", "log file")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	var err error
	switch args[0] {
	case "install":
		err = spikeInstall(*name, *exe, *child, *logPath)
	case "uninstall":
		err = spikeUninstall(*name)
	case "run":
		err = spikeRun(*child, *logPath)
	case "child":
		line := "console "
		if h, cerr := consoleWindow(); cerr != nil {
			line += "unknown: " + cerr.Error()
		} else {
			line += fmt.Sprint(h)
		}
		err = appendLine(*logPath, line)
	default:
		err = fmt.Errorf("unknown spike command %q", args[0])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "switchboard spike:", err)
		return 1
	}
	return 0
}

func spikeInstall(name, exe, child, logPath string) error {
	if runtime.GOOS != "windows" {
		return errors.New("the service spike is Windows only")
	}
	for _, p := range []string{exe, child, logPath} {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("%q is not an absolute path", p)
		}
	}
	return service.InstallTask(service.Task{
		Name:        name,
		Description: "switchboard service probe",
		Exe:         exe,
		Args:        []string{"spike", "run", "--child", child, "--log", logPath},
		WorkDir:     filepath.Dir(logPath),
	})
}

func spikeUninstall(name string) error {
	if runtime.GOOS != "windows" {
		return errors.New("the service spike is Windows only")
	}
	return service.UninstallTask(name)
}

// spikeRun heartbeats every second and starts the console child every five.
func spikeRun(child, logPath string) error {
	if err := proc.ContainSelf(); err != nil {
		return err
	}
	for i := 0; ; i++ {
		if err := appendLine(logPath, fmt.Sprintf("heartbeat %d", time.Now().Unix())); err != nil {
			return err
		}
		if i%5 == 0 {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			r := proc.Run(ctx, proc.Spec{Path: child, Args: []string{"spike", "child", "--log", logPath}})
			cancel()
			if r.Err != nil || r.ExitCode != 0 {
				_ = appendLine(logPath, fmt.Sprintf("child failed: exit %d err %v", r.ExitCode, r.Err))
			}
		}
		time.Sleep(time.Second)
	}
}

func appendLine(path, line string) error {
	if path == "" {
		return errors.New("no --log given")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintln(f, line)
	return err
}
