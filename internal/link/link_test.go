package link

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/config"
	"github.com/iMUngHee/switchboard/internal/pagercli"
	"github.com/iMUngHee/switchboard/internal/round"
	"github.com/iMUngHee/switchboard/internal/state"
	"github.com/iMUngHee/switchboard/internal/testenv"
	"github.com/iMUngHee/switchboard/internal/transport"
)

// TestOpenLocksTheLinkAndOnceCarriesMail: Open keys the link by this store's
// id and holds its lock against a second Open; Once carries real mail both
// ways.
func TestOpenLocksTheLinkAndOnceCarriesMail(t *testing.T) {
	s := testenv.NewStores(t)
	opt := Options{
		Name:     "vm",
		Pager:    config.Pager{Path: s.Pager, DB: s.A},
		StateDir: t.TempDir(),
		Engine:   round.Engine{Runner: pagercli.Runner{Timeout: time.Minute}},
		Config: config.Link{Peer: "vm", RemotePeer: "laptop", PollOutbound: 5 * time.Second, PollInbound: 30 * time.Second,
			Exec: &config.Exec{Argv: []string{s.Pager}, Env: map[string]string{"PAGER_DB": s.B}}},
	}
	l, err := Open(t.Context(), opt)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	idA := strings.TrimSpace(string(s.Run(t, s.A, "relay", "id")))
	if l.Key() != state.Key(idA, "vm") {
		t.Fatalf("key %q, want %q", l.Key(), state.Key(idA, "vm"))
	}
	if _, err := Open(t.Context(), opt); !errors.Is(err, ErrDriven) {
		t.Fatalf("a second Open: %v, want ErrDriven", err)
	}

	s.Run(t, s.A, "send", "kipa@vm", "over the link", "--session", "s-a")
	s.Run(t, s.B, "send", "sape@laptop", "and back", "--session", "s-b")
	for i, o := range l.Once(t.Context()) {
		if o.Class != round.OK || o.Finals != 1 {
			t.Fatalf("direction %d: %+v", i, o)
		}
	}
	saved, err := state.Load(opt.StateDir, l.Key())
	if err != nil || saved.Outbound.LastSuccess.IsZero() || saved.Inbound.LastSuccess.IsZero() || saved.LastLocalStore != idA {
		t.Fatalf("stored %+v %v", saved, err)
	}
	if !strings.Contains(string(s.Run(t, s.B, "export")), "over the link") || !strings.Contains(string(s.Run(t, s.A, "export")), "and back") {
		t.Fatal("the mail did not arrive")
	}
}

// TestRunResumesOnRequest: a stopped link waits for a resume request, then
// runs again.
func TestRunResumesOnRequest(t *testing.T) {
	c := newClock()
	l, _ := testLink(t, c, round.Outcome{Class: round.NeedsHuman, Code: "refused", Stage: round.StageIn})
	l.now = time.Now
	var runs atomic.Int32
	inner := l.runRound
	l.runRound = func(ctx context.Context, e *round.Engine, d round.Direction, in round.Input) round.Outcome {
		runs.Add(1)
		return inner(ctx, e, d, in)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()

	waitFor(t, "the link to stop", func() bool { return l.stopped() })
	before := runs.Load()
	time.Sleep(1500 * time.Millisecond)
	if runs.Load() != before {
		t.Fatal("a stopped link ran a round")
	}
	if err := state.Request(l.opt.StateDir, l.key, state.KindResume); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the resume", func() bool { return !l.stopped() && runs.Load() > before })
	cancel()
	<-done
}

// TestShutdownGivesRoundsTheirGrace: when Run's context ends, a running round
// keeps its own context for the grace period and is then stopped.
func TestShutdownGivesRoundsTheirGrace(t *testing.T) {
	l, _ := testLink(t, newClock())
	l.now = time.Now
	l.opt.Grace = 300 * time.Millisecond
	started := make(chan struct{}, 2)
	var stoppedAfter atomic.Int64
	var shutdown atomic.Int64
	l.runRound = func(ctx context.Context, _ *round.Engine, _ round.Direction, _ round.Input) round.Outcome {
		started <- struct{}{}
		<-ctx.Done()
		stoppedAfter.Store(time.Now().UnixNano() - shutdown.Load())
		return round.Outcome{Class: round.Stopped}
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	<-started
	shutdown.Store(time.Now().UnixNano())
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return")
	}
	if got := time.Duration(stoppedAfter.Load()); got < l.opt.Grace {
		t.Fatalf("the round was stopped %v after shutdown, before its %v grace", got, l.opt.Grace)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestRunWakesOnOnceRequest: a once request runs both directions at once,
// without waiting out a long poll.
func TestRunWakesOnOnceRequest(t *testing.T) {
	l, _ := testLink(t, newClock())
	l.now = wallClock
	for _, d := range l.dirs {
		d.poll = time.Hour
	}
	var runs atomic.Int32
	l.runRound = func(context.Context, *round.Engine, round.Direction, round.Input) round.Outcome {
		runs.Add(1)
		return idle
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	waitFor(t, "the first rounds", func() bool { return runs.Load() == 2 })
	if err := state.Request(l.opt.StateDir, l.key, state.KindOnce); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the requested rounds", func() bool { return runs.Load() == 4 })
	cancel()
	<-done
}

// TestFarEndpoint: a configured link becomes the endpoint its table names.
func TestFarEndpoint(t *testing.T) {
	ssh := filepath.Join(t.TempDir(), "ssh")
	ep, err := FarEndpoint(config.Link{SSH: &config.SSH{Host: "vm.example.com", Pager: "/home/me/.local/bin/pager", Options: []string{"-p", "2222"}}}, ssh)
	if err != nil {
		t.Fatal(err)
	}
	want := transport.SSHEndpoint{SSH: ssh, Host: "vm.example.com", Pager: "/home/me/.local/bin/pager", Options: []string{"-p", "2222"}, Windows: runtime.GOOS == "windows"}
	if got, ok := ep.(transport.SSHEndpoint); !ok || got.SSH != want.SSH || got.Host != want.Host || got.Pager != want.Pager || got.Windows != want.Windows || !slices.Equal(got.Options, want.Options) {
		t.Fatalf("endpoint %+v, want %+v", ep, want)
	}
	if _, err := FarEndpoint(config.Link{SSH: &config.SSH{Host: "vm%h", Pager: "/p"}}, ssh); err == nil {
		t.Fatal("an unsafe host was accepted")
	}
	ep, err = FarEndpoint(config.Link{Exec: &config.Exec{Argv: []string{"docker", "exec", "-i", "box", "pager"}}}, "")
	if _, ok := ep.(transport.ExecEndpoint); err != nil || !ok {
		t.Fatalf("exec endpoint %+v %v", ep, err)
	}
}
