package proc

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The test binary doubles as the processes the tests start, selected by
// PROC_TEST_HELPER.
func TestMain(m *testing.M) {
	switch os.Getenv("PROC_TEST_HELPER") {
	case "":
		os.Exit(m.Run())
	case "tree":
		// Start a grandchild that outlives us unless something stops it, report
		// its pid, then wait to be stopped.
		gc := exec.Command(os.Args[0])
		gc.Env = append(os.Environ(), "PROC_TEST_HELPER=sleep")
		if err := gc.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		fmt.Println(gc.Process.Pid)
		time.Sleep(time.Minute)
		os.Exit(0)
	case "sleep":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "leaver":
		// Exit at once, leaving a grandchild that holds our stdout open.
		gc := exec.Command(os.Args[0])
		gc.Env = append(os.Environ(), "PROC_TEST_HELPER=sleep")
		gc.Stdout = os.Stdout
		if err := gc.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(3)
		}
		fmt.Println(gc.Process.Pid)
		os.Exit(0)
	case "exit7":
		os.Exit(7)
	case "echo":
		_, _ = io.Copy(os.Stdout, os.Stdin)
		os.Exit(0)
	case "term":
		trapTerm() // prints "stopped politely" and exits 0 on SIGTERM; Unix only
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(4)
}

func helper(name string) Spec {
	return Spec{Path: os.Args[0], Env: append(os.Environ(), "PROC_TEST_HELPER="+name)}
}

func TestExitCodeAndStreams(t *testing.T) {
	s := helper("exit7")
	r := Run(t.Context(), s)
	if !r.Started || r.ExitCode != 7 || r.TimedOut || r.Err != nil {
		t.Fatalf("exit7: %+v", r)
	}

	var out bytes.Buffer
	s = helper("echo")
	s.Stdin, s.Stdout = strings.NewReader("line one\nline two\n"), &out
	if r := Run(t.Context(), s); r.ExitCode != 0 || r.Err != nil {
		t.Fatalf("echo: %+v", r)
	}
	if out.String() != "line one\nline two\n" {
		t.Fatalf("echo wrote %q", out.String())
	}
}

func TestStartFailure(t *testing.T) {
	r := Run(t.Context(), Spec{Path: "/nonexistent/stage-binary"})
	if r.Started || r.Err == nil || r.ExitCode != -1 {
		t.Fatalf("missing binary: %+v", r)
	}
}

// TestDeadlineKillsTree: a stage past its deadline is stopped together with a
// grandchild it started, which would otherwise keep running.
func TestDeadlineKillsTree(t *testing.T) {
	pr, pw := io.Pipe()
	s := helper("tree")
	s.Stdout = pw
	s.Grace = 500 * time.Millisecond

	pids := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(pr)
		if sc.Scan() {
			pid, _ := strconv.Atoi(strings.TrimSpace(sc.Text()))
			pids <- pid
		}
		_, _ = io.Copy(io.Discard, pr)
	}()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	results := make(chan Result, 1)
	go func() { results <- Run(ctx, s); pw.Close() }()

	var grandchild int
	select {
	case grandchild = <-pids:
	case <-time.After(30 * time.Second):
		t.Fatal("the stage never reported its grandchild")
	}
	if !alive(grandchild) {
		t.Fatalf("grandchild %d is not running before the deadline", grandchild)
	}
	cancel()

	var r Result
	select {
	case r = <-results:
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after the deadline")
	}
	if !r.TimedOut || !r.Started || r.ExitCode != -1 {
		t.Fatalf("result %+v, want a started, timed-out stage reporting -1", r)
	}
	deadline := time.Now().Add(10 * time.Second)
	for alive(grandchild) {
		if time.Now().After(deadline) {
			t.Fatalf("grandchild %d still runs after the stage was stopped", grandchild)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestLeftoverIsNotAFailure: a stage that exits 0 while a descendant still
// holds its stdout is a success with Leftover set, not an error, and Run
// returns soon after waitDelay instead of waiting for the descendant.
func TestLeftoverIsNotAFailure(t *testing.T) {
	var out bytes.Buffer
	s := helper("leaver")
	s.Stdout = &out
	r := Run(t.Context(), s)
	pid, _ := strconv.Atoi(strings.TrimSpace(strings.SplitN(out.String(), "\n", 2)[0]))
	t.Cleanup(func() {
		if pid > 0 {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Kill()
			}
		}
	})
	if r.ExitCode != 0 || r.TimedOut || r.Err != nil || !r.Leftover {
		t.Fatalf("result %+v, want exit 0, no error, Leftover", r)
	}
	if r.Duration > waitDelay+5*time.Second {
		t.Fatalf("took %v; Run waited on the descendant", r.Duration)
	}
}
