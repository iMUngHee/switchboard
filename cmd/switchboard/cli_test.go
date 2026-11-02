package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/pagercli"
	"github.com/iMUngHee/switchboard/internal/paths"
	"github.com/iMUngHee/switchboard/internal/state"
	"github.com/iMUngHee/switchboard/internal/testenv"
)

// sandbox points every per-user location at a temporary directory, so no
// command can read or write a real configuration or state. It returns the
// environment for a child switchboard process.
func sandbox(t *testing.T) []string {
	t.Helper()
	// Build what the tests need while HOME is still real, and keep the go
	// command on its real caches: a module cache under a temporary HOME is
	// read-only and would fail the test's cleanup.
	testenv.Pager(t)
	testenv.TestPager(t)
	goEnv, err := exec.Command("go", "env", "GOPATH", "GOCACHE", "GOMODCACHE").Output()
	if err != nil {
		t.Fatal(err)
	}
	g := strings.Split(strings.TrimSpace(string(goEnv)), "\n")
	home := t.TempDir()
	vars := map[string]string{
		"GOPATH": strings.TrimSpace(g[0]), "GOCACHE": strings.TrimSpace(g[1]), "GOMODCACHE": strings.TrimSpace(g[2]),
		"HOME": home, "USERPROFILE": home,
		"XDG_CONFIG_HOME": filepath.Join(home, "config"), "XDG_STATE_HOME": filepath.Join(home, "state"),
		"APPDATA": filepath.Join(home, "AppData", "Roaming"), "LOCALAPPDATA": filepath.Join(home, "AppData", "Local"),
		"SWITCHBOARD_CONFIG": "", "PAGER_WAKE": "off", "PAGER_CLIENT": "none",
	}
	env := os.Environ()
	for k, v := range vars {
		t.Setenv(k, v)
		env = append(env, k+"="+v)
	}
	return env
}

// writeConfig writes a configuration with one link, vm, whose far side is
// reached by running farArgv with farEnv. Strings are TOML literal strings,
// so Windows paths need no escaping. Notifications go to testpager, so no
// test ever shows a real desktop notification.
func writeConfig(t *testing.T, pager, db string, farArgv []string, farEnv map[string]string) string {
	t.Helper()
	lit := func(s string) string { return "'" + s + "'" }
	var argv, env []string
	for _, a := range farArgv {
		argv = append(argv, lit(a))
	}
	for k, v := range farEnv {
		env = append(env, k+" = "+lit(v))
	}
	text := fmt.Sprintf("version = 1\n\n[pager]\npath = %s\ndb = %s\n\n[notify]\ncommand = [%s, 'notify', '{link}', '{state}']\n\n[link.vm]\nremote_peer = 'laptop'\npoll_outbound = '1s'\npoll_inbound = '5s'\n\n[link.vm.exec]\nargv = [%s]\nenv = { %s }\n",
		lit(pager), lit(db), lit(testenv.TestPager(t)), strings.Join(argv, ", "), strings.Join(env, ", "))
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// cli runs one switchboard command in this process.
func cli(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = dispatch(args, &out, &errb)
	return code, out.String(), errb.String()
}

func mustCLI(t *testing.T, args ...string) string {
	t.Helper()
	code, out, errb := cli(t, args...)
	if code != 0 {
		t.Fatalf("switchboard %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, out, errb)
	}
	return out
}

func pendingOn(t *testing.T, s *testenv.Stores, db string) int {
	t.Helper()
	peers, err := pagercli.ParsePeerList(s.Run(t, db, "peer", "ls"))
	if err != nil || len(peers) != 1 {
		t.Fatalf("peer ls: %v %+v", err, peers)
	}
	return peers[0].Pending
}

func count(t *testing.T, s *testenv.Stores, db, body string) int {
	t.Helper()
	return strings.Count(string(s.Run(t, db, "export")), body)
}

type statusOut struct {
	Config string       `json:"config"`
	Links  []linkStatus `json:"links"`
}

func status(t *testing.T, cfg string) statusOut {
	t.Helper()
	var st statusOut
	if err := json.Unmarshal([]byte(mustCLI(t, "status", "--config", cfg, "--json")), &st); err != nil || len(st.Links) != 1 {
		t.Fatalf("status --json: %v %+v", err, st)
	}
	return st
}

// TestCLIEndToEnd: check, once, status and resume against two real stores,
// as a person would run them.
func TestCLIEndToEnd(t *testing.T) {
	sandbox(t)
	s := testenv.NewStores(t)
	cfg := writeConfig(t, s.Pager, s.A, []string{s.Pager}, map[string]string{"PAGER_DB": s.B})

	if out := mustCLI(t, "check", "--config", cfg); !strings.HasPrefix(out, "vm: ok (this store ") {
		t.Fatalf("check: %q", out)
	}
	// check stores nothing, so no store has been seen yet.
	if st := status(t, cfg); st.Config != cfg || st.Links[0].Daemon || st.Links[0].Outbound.Class != "" || !st.Links[0].Stale ||
		st.Links[0].LocalStore != "" || st.Links[0].RemoteStore != "" {
		t.Fatalf("status before any round: %+v", st)
	}
	if out := mustCLI(t, "status", "--config", cfg); !strings.Contains(out, "vm: no daemon, stale (far store not seen yet)\n") {
		t.Fatalf("status before any round: %q", out)
	}
	idA := strings.TrimSpace(string(s.Run(t, s.A, "relay", "id")))
	idB := strings.TrimSpace(string(s.Run(t, s.B, "relay", "id")))

	s.Run(t, s.A, "send", "kipa@vm", "from the laptop", "--session", "s-a")
	s.Run(t, s.B, "send", "sape@laptop", "from the vm", "--session", "s-b")
	code, out, errb := cli(t, "once", "--config", cfg)
	if code != 0 || !strings.Contains(out, "vm outbound: ok, 1 records, 1 with a final result") || !strings.Contains(out, "vm inbound: ok, 1 records") {
		t.Fatalf("once: exit %d\n%s\n%s", code, out, errb)
	}
	if !strings.HasPrefix(errb, "config: "+cfg+" (flag)\n") {
		t.Fatalf("once did not say which configuration it read: %q", errb)
	}
	if count(t, s, s.B, "from the laptop") != 1 || count(t, s, s.A, "from the vm") != 1 || pendingOn(t, s, s.A) != 0 || pendingOn(t, s, s.B) != 0 {
		t.Fatal("the mail did not arrive exactly once each way")
	}

	st := status(t, cfg).Links[0]
	if st.Outbound.Class != "ok" || st.Inbound.Class != "ok" || st.Outbound.LastSuccess == nil || st.Outbound.Pending != 0 || st.Daemon || st.Stale {
		t.Fatalf("status after once: %+v", st)
	}
	if st.LocalStore != idA || st.RemoteStore != idB {
		t.Fatalf("status after once names stores %q and %q, want %q and %q", st.LocalStore, st.RemoteStore, idA, idB)
	}
	var later statusOut
	raw := mustCLI(t, "status", "vm", "--json", "--config", cfg)
	if err := json.Unmarshal([]byte(raw), &later); err != nil || len(later.Links) != 1 {
		t.Fatalf("flags after the link: %v", err)
	}
	if !strings.Contains(raw, `"local_store": "`+idA+`"`) || !strings.Contains(raw, `"remote_store": "`+idB+`"`) {
		t.Fatalf("status --json does not name the stores local_store and remote_store:\n%s", raw)
	}
	if out := mustCLI(t, "status", "--config", cfg); !strings.Contains(out, "vm: no daemon (this store "+idA+", far store "+idB+")\n") || !strings.Contains(out, "outbound") {
		t.Fatalf("status: %q", out)
	}
	if out := mustCLI(t, "resume", "--config", cfg, "vm"); !strings.Contains(out, "vm: cleared") {
		t.Fatalf("resume: %q", out)
	}
	if out := mustCLI(t, "once", "--config", cfg); !strings.Contains(out, "vm outbound: idle") || !strings.Contains(out, "vm inbound: idle") {
		t.Fatalf("a second once: %q", out)
	}

	t.Run("a link a daemon holds is asked instead", func(t *testing.T) {
		dir, err := paths.StateDir()
		if err != nil {
			t.Fatal(err)
		}
		lock, err := state.TryLock(dir, st.Key)
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if out := mustCLI(t, "once", "--config", cfg); !strings.Contains(out, "another switchboard process holds it") {
			t.Fatalf("once: %q", out)
		}
		if out := mustCLI(t, "resume", "--config", cfg, "vm"); !strings.Contains(out, "asked it to resume") {
			t.Fatalf("resume: %q", out)
		}
		if kinds, _ := state.Take(dir, st.Key); len(kinds) != 2 {
			t.Fatalf("requests %q, want resume and once", kinds)
		}
		if !status(t, cfg).Links[0].Daemon {
			t.Fatal("status does not see the daemon")
		}
	})

	t.Run("stage stderr shows only with --debug", func(t *testing.T) {
		plan := filepath.Join(t.TempDir(), "plan.json")
		if err := os.WriteFile(plan, []byte(`{"relay out":{"stderr":"pager: to kipa: the launch codes\n"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		noisy := writeConfig(t, s.Pager, s.A, []string{testenv.TestPager(t)}, map[string]string{"TESTPAGER_PLAN": plan})
		_, out, errb := cli(t, "once", "--config", noisy)
		if strings.Contains(out+errb, "launch codes") {
			t.Fatalf("stderr shown without --debug:\n%s%s", out, errb)
		}
		if _, out, errb := cli(t, "once", "--debug", "--config", noisy); !strings.Contains(errb, "launch codes") {
			t.Fatalf("--debug did not show it:\n%s%s", out, errb)
		}
	})

	t.Run("a failed round exits 1", func(t *testing.T) {
		broken := writeConfig(t, s.Pager, s.A, []string{filepath.Join(t.TempDir(), "no-such-launcher")}, nil)
		if code, out, _ := cli(t, "once", "--config", broken); code != 1 || !strings.Contains(out, "offline (spawn)") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})

	t.Run("mistakes are refused", func(t *testing.T) {
		if code, _, errb := cli(t, "once", "--config", cfg, "pi"); code != 1 || !strings.Contains(errb, `no link "pi"`) {
			t.Fatalf("unknown link: %d %q", code, errb)
		}
		if code, _, _ := cli(t, "resume", "--config", cfg); code != 2 {
			t.Fatalf("resume without a link: %d", code)
		}
		if code, _, _ := cli(t, "frobnicate"); code != 2 {
			t.Fatalf("unknown command: %d", code)
		}
		if code, _, _ := cli(t, "status", "-h"); code != 0 {
			t.Fatalf("status -h: %d", code)
		}
		if code, _, _ := cli(t, "run", "extra"); code != 2 {
			t.Fatalf("run with an argument: %d", code)
		}
		if code, _, errb := cli(t, "once", "--config", filepath.Join(t.TempDir(), "missing.toml")); code != 1 || !strings.Contains(errb, "missing.toml") {
			t.Fatalf("missing config: %d %q", code, errb)
		}
	})
}

// TestCLIEnroll: enroll pairs two fresh stores over the link and says so per
// side, says "already" when run again, and refuses, with exit 1, a name
// enrolled as another store.
func TestCLIEnroll(t *testing.T) {
	sandbox(t)
	t.Setenv("PAGER_WAKE", "off")
	t.Setenv("PAGER_CLIENT", "none")
	pager := testenv.Pager(t)
	run := func(db string, args ...string) string {
		t.Helper()
		cmd := exec.Command(pager, args...)
		cmd.Env = append(os.Environ(), "PAGER_DB="+db)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("pager %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.db"), filepath.Join(dir, "b.db")
	idA, idB := run(a, "relay", "id"), run(b, "relay", "id")
	cfg := writeConfig(t, pager, a, []string{pager}, map[string]string{"PAGER_DB": b})

	want := "vm: this store enrolled the far store as \"vm\" (store " + idB + ")\n" +
		"vm: the far store enrolled this store as \"laptop\" (store " + idA + ")\n" +
		"vm: ok (this store " + idA + ", far store " + idB + ")\n"
	if out := mustCLI(t, "enroll", "--config", cfg); out != want {
		t.Fatalf("enroll on fresh stores:\n%s\nwant:\n%s", out, want)
	}
	if out := mustCLI(t, "check", "--config", cfg); !strings.HasPrefix(out, "vm: ok (this store ") {
		t.Fatalf("check after enroll: %q", out)
	}
	want = "vm: this store already enrolled the far store as \"vm\"\n" +
		"vm: the far store already enrolled this store as \"laptop\"\n" +
		"vm: ok (this store " + idA + ", far store " + idB + ")\n"
	if out := mustCLI(t, "enroll", "vm", "--config", cfg); out != want {
		t.Fatalf("enroll again:\n%s\nwant:\n%s", out, want)
	}

	c, d := filepath.Join(dir, "c.db"), filepath.Join(dir, "d.db")
	run(c, "peer", "add", "vm", "--store", idA) // vm is some other store here
	cfg = writeConfig(t, pager, c, []string{pager}, map[string]string{"PAGER_DB": d})
	code, out, _ := cli(t, "enroll", "--config", cfg)
	if code != 1 || !strings.Contains(out, "`pager peer rebind vm --store "+run(d, "relay", "id")+"`") || strings.Contains(out, "enrolled this store") {
		t.Fatalf("enroll over a name bound to another store: exit %d\n%s", code, out)
	}
}

// TestStatusShowsWaiting: with a switchboard holding the link, status says per
// direction whether it waits on relay wait or polls a side without it, in
// text and in --json; with none, or with the link stopped, it says neither.
func TestStatusShowsWaiting(t *testing.T) {
	sandbox(t)
	s := testenv.NewStores(t)
	cfg := writeConfig(t, s.Pager, s.A, []string{s.Pager}, map[string]string{"PAGER_DB": s.B})
	dir, err := paths.StateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	key := state.Key(strings.TrimSpace(string(s.Run(t, s.A, "relay", "id"))), "vm")
	since := time.Date(2026, 11, 2, 10, 37, 6, 0, time.UTC)
	until := since.Add(time.Hour)
	stored := state.Link{
		Outbound: state.Direction{Class: "idle", WaitingSince: since},
		Inbound:  state.Direction{Class: "idle", NoWaitUntil: until},
	}
	save := func(l state.Link) {
		t.Helper()
		if err := state.Save(dir, key, l); err != nil {
			t.Fatal(err)
		}
	}
	// lines returns the outbound and inbound lines of the text status.
	lines := func() (out, in string) {
		t.Helper()
		for _, line := range strings.Split(mustCLI(t, "status", "--config", cfg), "\n") {
			switch f := strings.Fields(line); {
			case len(f) > 0 && f[0] == "outbound":
				out = line
			case len(f) > 0 && f[0] == "inbound":
				in = line
			}
		}
		return out, in
	}
	quiet := func(when string) {
		t.Helper()
		out, in := lines()
		for _, line := range []string{out, in} {
			if !strings.HasSuffix(line, "  -") || strings.Contains(line, "waiting since") || strings.Contains(line, "polling") {
				t.Errorf("%s: %q, want the wait column to read -", when, line)
			}
		}
		raw := mustCLI(t, "status", "--config", cfg, "--json")
		if strings.Contains(raw, "waiting_since") || strings.Contains(raw, "no_wait_until") {
			t.Errorf("%s: status --json shows a wait:\n%s", when, raw)
		}
	}

	save(stored)
	quiet("no daemon")

	lock, err := state.TryLock(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	out, in := lines()
	if want := "waiting since " + since.Local().Format(time.DateTime); !strings.HasSuffix(out, want) {
		t.Errorf("outbound: %q, want it to end %q", out, want)
	}
	if want := "polling: no relay wait there, asks again " + until.Local().Format(time.DateTime); !strings.HasSuffix(in, want) {
		t.Errorf("inbound: %q, want it to end %q", in, want)
	}
	raw := mustCLI(t, "status", "--config", cfg, "--json")
	var st statusOut
	if err := json.Unmarshal([]byte(raw), &st); err != nil || len(st.Links) != 1 {
		t.Fatalf("status --json: %v\n%s", err, raw)
	}
	o, i := st.Links[0].Outbound, st.Links[0].Inbound
	if o.WaitingSince == nil || !o.WaitingSince.Equal(since) || o.NoWaitUntil != nil ||
		i.NoWaitUntil == nil || !i.NoWaitUntil.Equal(until) || i.WaitingSince != nil {
		t.Errorf("status --json with a daemon: outbound %+v, inbound %+v", o, i)
	}
	if !strings.Contains(raw, `"waiting_since": "`) || !strings.Contains(raw, `"no_wait_until": "`) {
		t.Errorf("status --json does not name them waiting_since and no_wait_until:\n%s", raw)
	}

	// A stopped link neither waits nor polls, whatever its state still says.
	stored.NeedsHuman = true
	save(stored)
	quiet("stopped, with a daemon")
}

var (
	buildOnce sync.Once
	binPath   string
	buildErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if binPath != "" {
		os.RemoveAll(filepath.Dir(binPath))
	}
	os.Exit(code)
}

// binary builds switchboard once for the tests that need it as a process.
func binary(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "switchboard-cli-")
		if err != nil {
			buildErr = err
			return
		}
		binPath = filepath.Join(dir, "switchboard")
		if runtime.GOOS == "windows" {
			binPath += ".exe"
		}
		if out, err := exec.Command("go", "build", "-o", binPath, ".").CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("build switchboard: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return binPath
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(within); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}
