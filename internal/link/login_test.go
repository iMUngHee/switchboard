package link

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/config"
	"github.com/iMUngHee/switchboard/internal/pagercli"
	"github.com/iMUngHee/switchboard/internal/round"
	"github.com/iMUngHee/switchboard/internal/testenv"
	"github.com/iMUngHee/switchboard/internal/transport"
)

var hostKey = round.Outcome{Class: round.Auth, Code: "host-key", Stage: round.StageOut}

// socketFile is a file standing in for a master's socket; a second call with
// the same name makes a new file, as a new master does.
func socketFile(t *testing.T, dir, name string) os.FileInfo {
	t.Helper()
	p := filepath.Join(dir, name)
	os.Remove(p)
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// TestStoppedOverLogin: only a stop that ssh's credentials alone caused is
// one a login can end.
func TestStoppedOverLogin(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stopped bool
		out, in round.Class
		want    bool
	}{
		{"running", false, round.Auth, round.Idle, false},
		{"stopped over auth outbound", true, round.Auth, round.Idle, true},
		{"stopped over auth inbound", true, round.OK, round.Auth, true},
		{"stopped over auth both ways", true, round.Auth, round.Auth, true},
		{"auth one way, needs-human the other", true, round.Auth, round.NeedsHuman, false},
		{"needs-human", true, round.NeedsHuman, round.Idle, false},
		{"stopped, no cause recorded", true, round.Idle, round.Idle, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, _ := testLink(t, newClock())
			l.st.NeedsHuman = tc.stopped
			l.st.Outbound.Class, l.st.Inbound.Class = string(tc.out), string(tc.in)
			if got := l.stoppedOverLogin(); got != tc.want {
				t.Fatalf("stoppedOverLogin = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestResumeOnLogin: a master resumes a link stopped over credentials once;
// stopped again while the same master listens, the link waits for a new one.
func TestResumeOnLogin(t *testing.T) {
	c := newClock()
	l, rec := testLink(t, c)
	dir := t.TempDir()
	var refused atomic.Bool
	refused.Store(true)
	l.runRound = func(context.Context, *round.Engine, round.Direction, round.Input) round.Outcome {
		if refused.Load() {
			return auth
		}
		return idle
	}

	l.step(t.Context(), l.dirs[1])
	if !l.stopped() {
		t.Fatal("the auth failure did not stop the link")
	}
	first := socketFile(t, dir, "m")
	l.resumeOnLogin(first)
	if l.stopped() {
		t.Fatal("a master did not resume the link")
	}

	// The link's ssh does not share that master: stopped again, and the
	// same master does not resume it a second time.
	l.step(t.Context(), l.dirs[1])
	if !l.stopped() {
		t.Fatal("the second auth failure did not stop the link")
	}
	again, err := os.Stat(filepath.Join(dir, "m"))
	if err != nil {
		t.Fatal(err)
	}
	l.resumeOnLogin(again)
	if !l.stopped() {
		t.Fatal("the same master resumed the link twice")
	}

	// A new login: a new socket at the same path.
	time.Sleep(10 * time.Millisecond) // a later modification time, on file systems that keep coarse ones
	l.resumeOnLogin(socketFile(t, dir, "m"))
	if l.stopped() {
		t.Fatal("a new master did not resume the link")
	}
	refused.Store(false)
	l.step(t.Context(), l.dirs[1])
	if l.stopped() {
		t.Fatal("the link stopped after a round that worked")
	}
	notes := rec.all()
	if len(notes) != 3 || notes[0].health != Auth || notes[1].health != Auth || notes[2].health != Healthy {
		t.Fatalf("notes %+v, want auth, auth, ok", notes)
	}

	t.Run("a master does not resume a link that is not waiting on a login", func(t *testing.T) {
		for _, o := range []round.Outcome{{Class: round.NeedsHuman, Code: "refused", Stage: round.StageIn}, idle} {
			l, _ := testLink(t, newClock(), o)
			l.step(t.Context(), l.dirs[0])
			wasStopped := l.stopped()
			l.resumeOnLogin(socketFile(t, t.TempDir(), "m"))
			if l.stopped() != wasStopped || l.resumedFor != nil {
				t.Fatalf("after %s: stopped %v, was %v, resumedFor %v", o.Class, l.stopped(), wasStopped, l.resumedFor)
			}
		}
	})
}

// TestSameMaster: one socket file is one master; a new file, or one made
// again in the same place (a reused inode, with a later time), is another.
func TestSameMaster(t *testing.T) {
	dir := t.TempDir()
	a := socketFile(t, dir, "a")
	again, err := os.Stat(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if !sameMaster(a, again) {
		t.Fatal("one file read twice is not the same master")
	}
	later := a.ModTime().Add(time.Second)
	if err := os.Chtimes(filepath.Join(dir, "a"), later, later); err != nil {
		t.Fatal(err)
	}
	touched, err := os.Stat(filepath.Join(dir, "a"))
	if err != nil {
		t.Fatal(err)
	}
	if sameMaster(a, touched) {
		t.Fatal("the same inode with a later time is the same master")
	}
	b := socketFile(t, dir, "b")
	if err := os.Chtimes(filepath.Join(dir, "b"), later, later); err != nil {
		t.Fatal(err)
	}
	b, err = os.Stat(filepath.Join(dir, "b"))
	if err != nil {
		t.Fatal(err)
	}
	if sameMaster(touched, b) {
		t.Fatal("two files with one time are the same master")
	}
}

// TestRunWatchesForALogin: a running link looks for a master only while
// stopped over credentials, host keys included. It asks for the ControlPath
// once per stop, resumes when a master listens there, does not resume a
// second time for the same master, and does for a new one.
func TestRunWatchesForALogin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows OpenSSH has no ControlMaster, so no link looks for one")
	}
	l, rec := testLink(t, newClock())
	l.now = time.Now
	l.loginEvery = 10 * time.Millisecond
	sock := filepath.Join(shortDir(t), "s")
	var loggedIn atomic.Bool
	var runs atomic.Int32
	l.runRound = func(context.Context, *round.Engine, round.Direction, round.Input) round.Outcome {
		runs.Add(1)
		if loggedIn.Load() {
			return idle
		}
		return hostKey
	}
	var asked atomic.Int32
	var unbounded atomic.Bool
	l.controlPath = func(ctx context.Context) string {
		asked.Add(1)
		if d, ok := ctx.Deadline(); !ok || time.Until(d) > masterCheck {
			unbounded.Store(true)
		}
		return sock
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()

	waitFor(t, "the link to stop", l.stopped)
	before := runs.Load()
	time.Sleep(150 * time.Millisecond)
	if !l.stopped() || runs.Load() != before {
		t.Fatalf("with no master: stopped %v, %d more rounds", l.stopped(), runs.Load()-before)
	}
	if n := asked.Load(); n != 1 {
		t.Fatalf("ssh -G asked %d times in one stop, want once", n)
	}

	loggedIn.Store(true)
	ln := listenAt(t, sock)
	waitFor(t, "both directions to work", func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return !l.st.NeedsHuman && l.st.Outbound.Class == string(round.Idle) && l.st.Inbound.Class == string(round.Idle)
	})
	waitFor(t, "the link told it works", func() bool {
		notes := rec.all()
		return len(notes) > 0 && notes[len(notes)-1].health == Healthy
	})
	if notes := rec.all(); len(notes) != 2 || notes[0].health != Auth {
		t.Fatalf("notes %+v, want auth, then ok", notes)
	}
	time.Sleep(100 * time.Millisecond)
	if n := asked.Load(); n != 1 {
		t.Fatalf("ssh -G asked %d times by a link that works", n)
	}

	// Stopped again while the same master listens: the link's ssh is not
	// sharing it, and the stop asks ssh -G again.
	loggedIn.Store(false)
	l.wakeAll()
	waitFor(t, "the link to stop again", l.stopped)
	waitFor(t, "ssh -G asked for the new stop", func() bool { return asked.Load() == 2 })
	time.Sleep(150 * time.Millisecond)
	if !l.stopped() {
		t.Fatal("the same master resumed the link twice")
	}

	// A new login: a new master on the same path.
	ln.Close()
	loggedIn.Store(true)
	listenAt(t, sock)
	waitFor(t, "a new master to resume the link", func() bool { return !l.stopped() })
	if unbounded.Load() {
		t.Fatalf("ssh -G was asked without a deadline of %v at most", masterCheck)
	}
}

// shortDir is a temporary directory short enough for a socket in it:
// sun_path holds about a hundred bytes, and macOS's TMPDIR alone takes half.
func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "sbm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func listenAt(t *testing.T, path string) net.Listener {
	t.Helper()
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

// TestSSHControlPath: the ControlPath is what ssh -G prints, run with the
// round's options and nothing else; an ssh -G that fails or hangs names none.
func TestSSHControlPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows OpenSSH has no ControlMaster, so no link asks for one")
	}
	dir := shortDir(t)
	argsFile := filepath.Join(dir, "args")
	// fakeSSH records its arguments, then prints config as ssh -G would.
	fakeSSH := func(t *testing.T, config string, exit int, hang bool) transport.SSHEndpoint {
		t.Helper()
		cfg := filepath.Join(dir, "config")
		if err := os.WriteFile(cfg, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > '" + argsFile + "'\n"
		if hang {
			script += "sleep 30\n"
		}
		script += "cat '" + cfg + "'\nexit " + strconv.Itoa(exit) + "\n"
		p := filepath.Join(dir, "ssh")
		if err := os.WriteFile(p, []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
		return transport.SSHEndpoint{SSH: p, Host: "vm.example.com", Pager: "/usr/local/bin/pager", Options: []string{"-F", "/dev/null"}}
	}
	config := "user me\nhostname vm.example.com\ncontrolmaster auto\ncontrolpath /tmp/cm/x\nbatchmode yes\n"

	t.Run("names the path ssh -G prints", func(t *testing.T) {
		if got := sshControlPath(t.Context(), fakeSSH(t, config, 0, false)); got != "/tmp/cm/x" {
			t.Fatalf("path %q", got)
		}
		data, err := os.ReadFile(argsFile)
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
		want := []string{"-F", "/dev/null", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-G", "--", "vm.example.com"}
		if !slices.Equal(got, want) {
			t.Fatalf("ssh ran with %q, want %q", got, want)
		}
	})

	t.Run("names none", func(t *testing.T) {
		for _, tc := range []struct {
			name, config string
			exit         int
		}{
			{"no control path", "user me\nhostname vm.example.com\n", 0},
			{"control path none", "controlpath none\n", 0},
			{"ssh -G fails", config, 1},
		} {
			if got := sshControlPath(t.Context(), fakeSSH(t, tc.config, tc.exit, false)); got != "" {
				t.Errorf("%s: path %q", tc.name, got)
			}
		}
	})

	t.Run("a hanging ssh -G names none, at its deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
		defer cancel()
		start := time.Now()
		if got := sshControlPath(ctx, fakeSSH(t, config, 0, true)); got != "" {
			t.Fatalf("path %q", got)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Fatalf("took %v", d)
		}
	})
}

// TestListening: a socket counts while a process listens on it, and not
// once it is left behind.
func TestListening(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows OpenSSH has no ControlMaster, so no link looks for one")
	}
	p := filepath.Join(shortDir(t), "s")
	for _, path := range []string{"", p} {
		if m, ok := listening(t.Context(), path); ok {
			t.Fatalf("%q: found %v with nothing there", path, m)
		}
	}
	ln := listenAt(t, p)
	m, ok := listening(t.Context(), p)
	if want, err := os.Stat(p); !ok || err != nil || !os.SameFile(m, want) {
		t.Fatalf("found %v (%v), want the socket at %s", m, ok, p)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("the left-behind socket is gone: %v", err)
	}
	if m, ok := listening(t.Context(), p); ok {
		t.Fatalf("a left-behind socket counts: %v", m)
	}
}

// TestControlPathWithThisSSH: the ssh on PATH names the socket for the host,
// %C expanded, and a master listening there is found. Nothing is reached:
// ssh -G reads configuration, here none, for a host that cannot resolve.
func TestControlPathWithThisSSH(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows OpenSSH has no ControlMaster, so no link looks for one")
	}
	sshPath, err := exec.LookPath("ssh")
	if err != nil {
		t.Skip("no ssh on PATH")
	}
	dir := shortDir(t)
	s := transport.SSHEndpoint{SSH: sshPath, Host: "vm.example.invalid", Pager: "/usr/local/bin/pager",
		Options: []string{"-F", "/dev/null", "-o", "ControlPath=" + dir + "/%C"}}
	p := sshControlPath(t.Context(), s)
	if filepath.Dir(p) != dir || strings.Contains(p, "%") {
		t.Fatalf("ssh -G names %q, want %%C expanded in %s", p, dir)
	}
	if m, ok := listening(t.Context(), p); ok {
		t.Fatalf("found a master before one listens: %v", m)
	}
	listenAt(t, p)
	m, ok := listening(t.Context(), p)
	if want, err := os.Stat(p); !ok || err != nil || !os.SameFile(m, want) {
		t.Fatalf("found %v (%v), want the socket at %s", m, ok, p)
	}
}

// TestOpenWatchesSSHLinksOnly: a link reached with ssh looks for a master,
// except on Windows; one reached with any other command does not.
func TestOpenWatchesSSHLinksOnly(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	plan := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(plan, []byte(`{"relay id":{"stdout":"`+id+`\n","skip_stdin":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TESTPAGER_PLAN", plan)
	pager := testenv.TestPager(t)
	sshPath, err := filepath.Abs(pager) // never run: Open builds the far side, it does not reach it
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		cfg  config.Link
		want bool
	}{
		{"ssh", config.Link{SSH: &config.SSH{Host: "vm.example.com", Pager: "/usr/local/bin/pager"}}, runtime.GOOS != "windows"},
		{"exec", config.Link{Exec: &config.Exec{Argv: []string{pager}}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Peer, tc.cfg.RemotePeer = "vm", "laptop"
			tc.cfg.PollOutbound, tc.cfg.PollInbound = 5*time.Second, 30*time.Second
			l, err := Open(t.Context(), Options{
				Name: "vm", Pager: config.Pager{Path: pager}, StateDir: t.TempDir(), SSH: sshPath,
				Engine: round.Engine{Runner: pagercli.Runner{Timeout: time.Minute}}, Config: tc.cfg,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()
			if got := l.controlPath != nil; got != tc.want {
				t.Fatalf("looks for a master: %v, want %v", got, tc.want)
			}
		})
	}
}
