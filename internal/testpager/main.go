// Command testpager stands in for pager in switchboard's tests. It does what a
// JSON plan says for each subcommand — print this, exit with that, hang, spew
// stderr, leave a child behind — so the round engine can be driven through
// failures a real pager rarely produces on demand.
//
// TESTPAGER_PLAN names the plan file: an object keyed by the first two
// arguments ("relay out", "relay in", "peer ls", …). TESTPAGER_LOG, when set,
// receives one JSON line per invocation with its arguments and stdin.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// step is what one invocation does, in this order: read stdin (unless
// skip_stdin), sleep, write stderr, write stdout, maybe leave a child holding
// stdout, then exit.
type step struct {
	Stdout      string `json:"stdout"`
	StdoutBytes int    `json:"stdout_bytes"` // then this many 'x' bytes
	Stderr      string `json:"stderr"`
	StderrBytes int    `json:"stderr_bytes"` // then this many 'e' bytes
	Exit        int    `json:"exit"`
	SleepMS     int    `json:"sleep_ms"`
	SkipStdin   bool   `json:"skip_stdin"`
	LeaveChild  bool   `json:"leave_child"`
}

func main() {
	if os.Getenv("TESTPAGER_CHILD") == "1" {
		time.Sleep(time.Minute)
		return
	}
	key := strings.Join(firstN(os.Args[1:], 2), " ")
	plan := map[string]step{}
	if p := os.Getenv("TESTPAGER_PLAN"); p != "" {
		raw, err := os.ReadFile(p)
		if err != nil {
			fail("read plan: %v", err)
		}
		if err := json.Unmarshal(raw, &plan); err != nil {
			fail("parse plan: %v", err)
		}
	}
	s, ok := plan[key]
	if !ok {
		fail("no plan for %q", key)
	}

	var stdin []byte
	if !s.SkipStdin {
		stdin, _ = io.ReadAll(os.Stdin)
	}
	if log := os.Getenv("TESTPAGER_LOG"); log != "" {
		f, err := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			_ = json.NewEncoder(f).Encode(map[string]any{"args": os.Args[1:], "stdin": string(stdin)})
			f.Close()
		}
	}
	if s.SleepMS > 0 {
		time.Sleep(time.Duration(s.SleepMS) * time.Millisecond)
	}
	fmt.Fprint(os.Stderr, s.Stderr)
	writeN(os.Stderr, 'e', s.StderrBytes)
	fmt.Fprint(os.Stdout, s.Stdout)
	writeN(os.Stdout, 'x', s.StdoutBytes)
	if s.LeaveChild {
		self, err := os.Executable()
		if err != nil {
			fail("find myself: %v", err)
		}
		c := exec.Command(self)
		c.Env = append(os.Environ(), "TESTPAGER_CHILD=1")
		c.Stdout = os.Stdout
		_ = c.Start()
	}
	os.Exit(s.Exit)
}

func firstN(s []string, n int) []string {
	if len(s) < n {
		return s
	}
	return s[:n]
}

func writeN(w io.Writer, b byte, n int) {
	buf := []byte(strings.Repeat(string(b), 64<<10))
	for n > 0 {
		k := min(n, len(buf))
		if _, err := w.Write(buf[:k]); err != nil {
			return
		}
		n -= k
	}
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "testpager: "+format+"\n", a...)
	os.Exit(99)
}
