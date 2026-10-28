package pagercli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/testenv"
	"github.com/iMUngHee/switchboard/internal/transport"
)

func TestParseRelayID(t *testing.T) {
	id := strings.Repeat("ab", 16)
	for in, ok := range map[string]bool{
		id + "\n":                       true,
		id + "\r\n":                     true,
		id:                              true,
		"Welcome to the host\n" + id:    false, // a remote banner
		id + "\n" + id + "\n":           false,
		strings.ToUpper(id) + "\n":      false,
		strings.Repeat("ab", 15) + "\n": false,
		"":                              false,
	} {
		got, err := ParseRelayID([]byte(in))
		if ok != (err == nil) || (ok && got != id) {
			t.Errorf("ParseRelayID(%q) = %q, %v", in, got, err)
		}
	}
}

func TestParsePeerList(t *testing.T) {
	a, b := strings.Repeat("a", 32), strings.Repeat("b", 32)
	peers, err := ParsePeerList([]byte("NAME    STORE                             PENDING  SEQ\nvm      " + a + "  1        7\nbox     " + b + "  0        0\n"))
	if err != nil || len(peers) != 2 || peers[0] != (Peer{"vm", a, 1, 7}) || peers[1] != (Peer{"box", b, 0, 0}) {
		t.Fatalf("table: %+v, %v", peers, err)
	}
	if peers, err := ParsePeerList([]byte("no peers — register one with: pager peer add <name> --store <id>\n")); err != nil || peers != nil {
		t.Fatalf("no peers: %+v, %v", peers, err)
	}
	if _, err := ParsePeerList([]byte("NAME STORE PENDING SENT\nvm " + a + " 0 3\n")); err != nil {
		t.Fatalf("an older pager's SENT header: %v", err)
	}
	for _, bad := range []string{"banner\nNAME STORE PENDING SEQ\n", "NAME STORE PENDING SEQ\nvm notanid 0 0\n", "NAME STORE PENDING SEQ\nvm " + a + " x 0\n"} {
		if _, err := ParsePeerList([]byte(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// pair makes two stores on this machine — "laptop" (a) and "vm" (b) — enrolled
// with each other, with a session named sape on a and kipa on b. b is reached
// through an exec endpoint, the same way a test stands in for a far side.
type pair struct {
	pager string
	a, b  string // database paths
	local transport.LocalEndpoint
	far   transport.ExecEndpoint
	run   Runner
}

func newPair(t *testing.T) *pair {
	t.Helper()
	// Nothing these stores do may poke a real session on this machine.
	t.Setenv("PAGER_WAKE", "off")
	t.Setenv("PAGER_CLIENT", "none")
	pager := testenv.Pager(t)
	dir := t.TempDir()
	p := &pair{pager: pager, a: filepath.Join(dir, "a.db"), b: filepath.Join(dir, "b.db"), run: Runner{Timeout: time.Minute}}
	p.local = transport.LocalEndpoint{Pager: pager, DB: p.a}
	p.far = transport.ExecEndpoint{Argv: []string{pager}, Env: map[string]string{"PAGER_DB": p.b}}
	p.direct(t, p.a, "attach", "--session", "s-a", "--tool", "claude", "--root", dir)
	p.must(t, p.local, nil, "alias", "sape", "--session", "s-a")
	p.direct(t, p.b, "attach", "--session", "s-b", "--tool", "claude", "--root", dir)
	p.must(t, p.far, nil, "alias", "kipa", "--session", "s-b")
	idA := p.id(t, p.local)
	idB := p.id(t, p.far)
	p.must(t, p.local, nil, "peer", "add", "vm", "--store", idB)
	p.must(t, p.far, nil, "peer", "add", "laptop", "--store", idA)
	return p
}

// direct runs pager on one store without the Runner, for the steps whose
// arguments (a path, message text) the Runner deliberately refuses.
func (p *pair) direct(t *testing.T, db string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(p.pager, args...)
	cmd.Env = append(os.Environ(), "PAGER_DB="+db)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pager %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}

func (p *pair) must(t *testing.T, ep transport.Endpoint, stdin []byte, args ...string) []byte {
	t.Helper()
	out, err := p.run.Run(t.Context(), ep, stdin, args...)
	if err != nil || out.Result.ExitCode != 0 || out.Result.Err != nil {
		t.Fatalf("pager %s: %+v %v\nstderr: %s", strings.Join(args, " "), out.Result, err, out.StderrHead)
	}
	return out.Stdout
}

func (p *pair) id(t *testing.T, ep transport.Endpoint) string {
	t.Helper()
	id, err := ParseRelayID(p.must(t, ep, nil, "relay", "id"))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestAgainstRealPager runs the subcommands switchboard depends on against the
// pager version it supports, through both endpoint kinds a test can reach.
func TestAgainstRealPager(t *testing.T) {
	p := newPair(t)

	peers, err := ParsePeerList(p.must(t, p.local, nil, "peer", "ls"))
	if err != nil || len(peers) != 1 || peers[0].Name != "vm" || peers[0].Store != p.id(t, p.far) || peers[0].Pending != 0 {
		t.Fatalf("peer ls on a: %+v %v", peers, err)
	}

	p.direct(t, p.a, "send", "kipa@vm", "hello across", "--session", "s-a")
	peers, _ = ParsePeerList(p.must(t, p.local, nil, "peer", "ls"))
	if peers[0].Pending != 1 {
		t.Fatalf("pending after send: %+v", peers)
	}

	records := p.must(t, p.local, nil, "relay", "out", "--peer", "vm", "--limit", "100")
	results := p.must(t, p.far, records, "relay", "in", "--peer", "laptop")
	var r struct{ Status string }
	if err := json.Unmarshal([]byte(strings.SplitN(string(results), "\n", 2)[0]), &r); err != nil || r.Status != "imported" {
		t.Fatalf("relay in results %q (%v)", results, err)
	}
	p.must(t, p.local, results, "relay", "ack", "--peer", "vm")

	peers, _ = ParsePeerList(p.must(t, p.local, nil, "peer", "ls"))
	if peers[0].Pending != 0 {
		t.Fatalf("pending after ack: %+v", peers)
	}
	if export := p.must(t, p.far, nil, "export"); !strings.Contains(string(export), "hello across") {
		t.Fatalf("the far store does not hold the message:\n%s", export)
	}
}

func plan(t *testing.T, steps map[string]map[string]any) transport.ExecEndpoint {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.json")
	raw, _ := json.Marshal(steps)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return transport.ExecEndpoint{Argv: []string{testenv.TestPager(t)}, Env: map[string]string{"TESTPAGER_PLAN": path}}
}

// TestStdoutCapStopsTheStage: a stage that writes past the cap is stopped and
// marked, rather than buffered without limit.
func TestStdoutCapStopsTheStage(t *testing.T) {
	ep := plan(t, map[string]map[string]any{"relay out": {"stdout_bytes": 8 << 20, "sleep_ms": 0}})
	out, err := Runner{StdoutCap: 1 << 20, Timeout: time.Minute}.Run(t.Context(), ep, nil, "relay", "out", "--peer", "vm")
	if err != nil || !out.Over || len(out.Stdout) != 1<<20 {
		t.Fatalf("over=%v len=%d err=%v result=%+v", out.Over, len(out.Stdout), err, out.Result)
	}
}

// TestChattyStderrNeverBlocks: a stage that writes a lot of stderr still
// finishes; only the head is kept.
func TestChattyStderrNeverBlocks(t *testing.T) {
	ep := plan(t, map[string]map[string]any{"relay in": {"stderr_bytes": 4 << 20, "stdout": "{}\n"}})
	out, err := Runner{Timeout: time.Minute}.Run(t.Context(), ep, []byte("{}\n"), "relay", "in", "--peer", "laptop")
	if err != nil || out.Result.ExitCode != 0 || out.Result.TimedOut || len(out.StderrHead) != StderrKept || string(out.Stdout) != "{}\n" {
		t.Fatalf("result %+v head=%d stdout=%q err=%v", out.Result, len(out.StderrHead), out.Stdout, err)
	}
}

func TestRunRefusesUnsafeInput(t *testing.T) {
	// Programs that do not exist: should a check regress, nothing real starts.
	missing := "/nonexistent/"
	if runtime.GOOS == "windows" {
		missing = `C:\nonexistent\`
	}
	ssh := transport.SSHEndpoint{SSH: missing + "ssh", Host: "vm.example.com", Pager: "/usr/local/bin/pager"}
	exe := transport.ExecEndpoint{Argv: []string{missing + "wsl.exe", "/usr/local/bin/pager"}}
	for name, ep := range map[string]transport.Endpoint{"ssh": ssh, "exec": exe} {
		// The endpoint itself is valid, so only the argument can be refused.
		if err := ep.Validate(); err != nil {
			t.Fatalf("%s: the endpoint is refused before the argument is looked at: %v", name, err)
		}
		if _, err := (Runner{}).Run(t.Context(), ep, nil, "relay", "out", "--peer", "vm;id"); err == nil {
			t.Errorf("%s: an unsafe argument got through", name)
		}
	}
	bad := ssh
	bad.Pager = "/opt/x;id"
	if _, err := (Runner{}).Run(t.Context(), bad, nil, "relay", "id"); err == nil {
		t.Error("an endpoint that fails validation was run")
	}
}
