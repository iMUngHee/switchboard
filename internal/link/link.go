// Package link drives one configured link: both directions of rounds between
// this store and a far one, when to run each, the stored state, control
// requests, and what to tell a person.
//
// A link holds its identity lock for as long as it runs, so no second process
// drives it. Its two directions run concurrently; every change to the stored
// state happens under the link's mutex and is saved at once.
package link

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/iMUngHee/switchboard/internal/config"
	"github.com/iMUngHee/switchboard/internal/pagercli"
	"github.com/iMUngHee/switchboard/internal/round"
	"github.com/iMUngHee/switchboard/internal/state"
	"github.com/iMUngHee/switchboard/internal/transport"
)

const (
	// DefaultGrace is how long in-flight rounds may finish once shutdown
	// begins, so relay in still wakes its recipients.
	DefaultGrace = 10 * time.Second
	// controlEvery is how often a running link looks for control requests.
	controlEvery = time.Second
	// maxSleep bounds one timer, so a wait re-reads the wall clock after a
	// machine sleeps instead of trusting a timer that slept with it.
	maxSleep = 15 * time.Second
	// stateFailures is how many state writes in a row may fail before the
	// link counts as degraded.
	stateFailures = 3
)

// Options configures a link.
type Options struct {
	Name     string // this store's peer name for the far store
	Config   config.Link
	Pager    config.Pager
	StateDir string
	Engine   round.Engine  // a template: each direction gets a copy with its own Save
	Sem      chan struct{} // shared by every link to bound concurrent rounds; nil means no bound
	Notifier Notifier      // nil tells no one
	Log      *slog.Logger  // nil discards
	Grace    time.Duration // zero means DefaultGrace
	SSH      string        // the ssh client; found on PATH when empty
}

// Link is an open, locked link.
type Link struct {
	opt  Options
	key  string
	lock *state.Lock

	mu           sync.Mutex
	st           state.Link
	dirs         [2]*direction
	awake        time.Time // when this process started, or the machine last woke
	notified     Health
	noteSeq      uint64 // counts health changes, so they are told in order
	saveFailures int
	wake         [2]chan struct{}

	notifyMu sync.Mutex
	sentSeq  uint64

	// Seams for tests.
	now      func() time.Time
	jitter   func(time.Duration) time.Duration
	runRound func(ctx context.Context, e *round.Engine, d round.Direction, in round.Input) round.Outcome
}

// ErrDriven means another switchboard process already drives the link.
var ErrDriven = errors.New("another switchboard process already drives this link")

// Open finds this store's id, takes the link's identity lock and loads its
// state.
func Open(ctx context.Context, opt Options) (*Link, error) {
	if opt.Grace <= 0 {
		opt.Grace = DefaultGrace
	}
	if opt.Log == nil {
		opt.Log = slog.New(slog.DiscardHandler)
	}
	local := transport.LocalEndpoint{Pager: opt.Pager.Path, DB: opt.Pager.DB}
	far, err := FarEndpoint(opt.Config, opt.SSH)
	if err != nil {
		return nil, fmt.Errorf("link %s: %w", opt.Name, err)
	}
	id, err := relayID(ctx, opt.Engine.Runner, local)
	if err != nil {
		return nil, fmt.Errorf("link %s: this store: %w", opt.Name, err)
	}
	if err := os.MkdirAll(opt.StateDir, 0o700); err != nil {
		return nil, err
	}
	key := state.Key(id, opt.Name)
	lock, err := state.TryLock(opt.StateDir, key)
	if errors.Is(err, state.ErrLocked) {
		return nil, fmt.Errorf("link %s: %w", opt.Name, ErrDriven)
	}
	if err != nil {
		return nil, err
	}
	st, err := state.Load(opt.StateDir, key)
	if err != nil {
		lock.Close()
		return nil, err
	}
	st.LastLocalStore = id

	l := &Link{opt: opt, key: key, lock: lock, st: st, notified: Healthy,
		now: wallClock, jitter: spread, runRound: func(ctx context.Context, e *round.Engine, d round.Direction, in round.Input) round.Outcome {
			return e.Run(ctx, d, in)
		}}
	l.awake = l.now()
	cfg := opt.Config
	l.dirs[0] = &direction{name: "outbound", poll: cfg.PollOutbound, st: &l.st.Outbound,
		round: round.Direction{Link: opt.Name, Name: "outbound", From: round.Side{Endpoint: local, Peer: cfg.Peer}, To: round.Side{Endpoint: far, Peer: cfg.RemotePeer}}}
	l.dirs[1] = &direction{name: "inbound", poll: cfg.PollInbound, st: &l.st.Inbound,
		round: round.Direction{Link: opt.Name, Name: "inbound", From: round.Side{Endpoint: far, Peer: cfg.RemotePeer}, To: round.Side{Endpoint: local, Peer: cfg.Peer}}}
	for i := range l.wake {
		l.wake[i] = make(chan struct{}, 1)
	}
	return l, nil
}

// Key is the link's identity key.
func (l *Link) Key() string { return l.key }

// Close releases the identity lock.
func (l *Link) Close() error { return l.lock.Close() }

// Run drives the link until ctx ends. Rounds already running then get
// Options.Grace to finish before they are stopped.
func (l *Link) Run(ctx context.Context) {
	roundCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	stop := context.AfterFunc(ctx, func() { time.AfterFunc(l.opt.Grace, cancel) })
	defer stop()

	var wg sync.WaitGroup
	for i, d := range l.dirs {
		wg.Go(func() { l.loop(ctx, roundCtx, i, d) })
	}
	l.control(ctx)
	wg.Wait()
}

// Once runs one round in each direction now, in this process, and returns
// their outcomes: what `switchboard once` does when no daemon holds the link.
// It must not be called while Run runs.
func (l *Link) Once(ctx context.Context) [2]round.Outcome {
	var out [2]round.Outcome
	var wg sync.WaitGroup
	for i, d := range l.dirs {
		wg.Go(func() { _, out[i] = l.step(ctx, d) })
	}
	wg.Wait()
	return out
}

func (l *Link) loop(ctx, roundCtx context.Context, i int, d *direction) {
	l.mu.Lock()
	wait := d.firstWait(l.now())
	l.mu.Unlock()
	for l.sleep(ctx, i, wait) {
		wait, _ = l.turn(ctx, roundCtx, d)
		if ctx.Err() != nil {
			return
		}
	}
}

// sleep waits for wait to pass on the wall clock, for a wake-up, or for ctx.
// A stopped link sleeps until woken. It reports false when ctx ended.
//
// The wait ends at whichever comes first of two deadlines. One is on the wall
// clock, read without its monotonic reading: that reading stops while the
// machine sleeps, and a wait measured by it alone would ignore the night. The
// other is monotonic, so a wall clock set back cannot stretch the wait.
func (l *Link) sleep(ctx context.Context, i int, wait time.Duration) bool {
	target := l.now().Add(wait)
	deadline := time.Now().Add(wait) // monotonic: a clock set back cannot stretch it
	for {
		rem := min(target.Sub(l.now()), time.Until(deadline))
		if l.stopped() {
			rem = maxSleep
		} else if rem <= 0 {
			return true
		}
		step := min(rem, maxSleep)
		expect := l.now().Add(step)
		t := time.NewTimer(step)
		select {
		case <-t.C:
			// A timer that fired far later than set slept with the machine.
			if now := l.now(); now.Sub(expect) > time.Minute {
				l.mu.Lock()
				l.awake = now
				l.mu.Unlock()
			}
		case <-l.wake[i]:
			t.Stop()
			return true
		case <-ctx.Done():
			t.Stop()
			return false
		}
	}
}

// step runs one round in direction d, records it, and returns how long to
// wait before the next. The second result is the outcome.
func (l *Link) step(ctx context.Context, d *direction) (time.Duration, round.Outcome) {
	return l.turn(ctx, ctx, d)
}

// turn is step for a running link: once stop ends no round starts, but one
// already running keeps roundCtx.
func (l *Link) turn(stop, roundCtx context.Context, d *direction) (time.Duration, round.Outcome) {
	if l.stopped() {
		return 0, round.Outcome{Class: round.NeedsHuman}
	}
	if stop.Err() != nil {
		return 0, round.Outcome{Class: round.Stopped}
	}
	if l.opt.Sem != nil {
		select {
		case l.opt.Sem <- struct{}{}:
			defer func() { <-l.opt.Sem }()
		case <-stop.Done():
			return 0, round.Outcome{Class: round.Stopped}
		}
		if stop.Err() != nil { // both were ready, and select chose at random
			return 0, round.Outcome{Class: round.Stopped}
		}
	}
	ctx := roundCtx
	l.mu.Lock()
	in := round.Input{Pending: d.st.PendingAck, Limit: d.limit, PrevCode: d.st.Code}
	l.mu.Unlock()

	e := l.opt.Engine
	e.Save = func(p []string) error {
		l.mu.Lock()
		defer l.mu.Unlock()
		d.st.PendingAck = p
		return l.save()
	}
	o := l.runRound(ctx, &e, d.round, in)

	l.mu.Lock()
	if o.Class == round.Stopped {
		// Shutting down says nothing about the link: keep what it last was,
		// and only the results still to ack.
		d.st.PendingAck = o.Pending
		l.save()
		l.mu.Unlock()
		return 0, o
	}
	now := l.now()
	wait := d.after(o, now, l.jitter)
	if o.Class == round.NeedsHuman {
		l.st.NeedsHuman = true
	}
	l.save()
	h, changed := l.health(now)
	var seq uint64
	if changed {
		l.noteSeq++
		seq = l.noteSeq
	}
	l.mu.Unlock()

	if changed {
		l.opt.Log.Info("link health", "link", l.opt.Name, "health", string(h), "direction", d.name, "class", string(o.Class), "code", o.Code)
		l.tell(seq, h)
	}
	return wait, o
}

// tell notifies a health change unless a later one was told first: the two
// directions decide under the lock but notify outside it, and the last word
// a person gets must be the current one.
func (l *Link) tell(seq uint64, h Health) {
	l.notifyMu.Lock()
	defer l.notifyMu.Unlock()
	if seq <= l.sentSeq {
		return
	}
	l.sentSeq = seq
	if l.opt.Notifier != nil {
		l.opt.Notifier.Notify(l.opt.Name, h, l.opt.Config.Hint)
	}
}

// control picks up resume and once requests until ctx ends.
func (l *Link) control(ctx context.Context) {
	t := time.NewTicker(controlEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		kinds, err := state.Take(l.opt.StateDir, l.key)
		if err != nil {
			l.opt.Log.Warn("control requests", "link", l.opt.Name, "code", "state-io", "err", err)
		}
		for _, k := range kinds {
			if k == state.KindResume {
				l.Resume()
			}
			l.wakeAll()
		}
	}
}

// Resume clears the link's needs-human mark and backoff and runs both
// directions at once.
func (l *Link) Resume() {
	l.mu.Lock()
	l.st.Resume()
	for _, d := range l.dirs {
		d.reruns, d.drained = 0, 0
		d.endStreak()
		d.isDegraded = false
	}
	l.save()
	l.mu.Unlock()
	l.opt.Log.Info("link resumed", "link", l.opt.Name)
	l.wakeAll()
}

func (l *Link) wakeAll() {
	for _, w := range l.wake {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}

func (l *Link) stopped() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.st.NeedsHuman
}

// save writes the state. The caller holds l.mu.
func (l *Link) save() error {
	err := state.Save(l.opt.StateDir, l.key, l.st)
	if err != nil {
		l.saveFailures++
		l.opt.Log.Warn("state write failed", "link", l.opt.Name, "code", "state-io", "err", err)
	} else {
		l.saveFailures = 0
	}
	return err
}

// FarEndpoint says how to reach the far side of a configured link.
func FarEndpoint(cfg config.Link, sshPath string) (transport.Endpoint, error) {
	var ep transport.Endpoint
	switch {
	case cfg.SSH != nil:
		if sshPath == "" {
			p, err := exec.LookPath("ssh")
			if err != nil {
				return nil, fmt.Errorf("no ssh client on PATH: %w", err)
			}
			if sshPath, err = filepath.Abs(p); err != nil {
				return nil, err
			}
		}
		ep = transport.SSHEndpoint{SSH: sshPath, Host: cfg.SSH.Host, Pager: cfg.SSH.Pager, Options: cfg.SSH.Options, Windows: runtime.GOOS == "windows"}
	case cfg.Exec != nil:
		ep = transport.ExecEndpoint{Argv: cfg.Exec.Argv, Env: cfg.Exec.Env}
	default:
		return nil, errors.New("the link has neither ssh nor exec")
	}
	return ep, ep.Validate()
}

func relayID(ctx context.Context, r pagercli.Runner, ep transport.Endpoint) (string, error) {
	out, err := r.Run(ctx, ep, nil, "relay", "id")
	if err != nil {
		return "", err
	}
	if res := out.Result; !res.Started || res.TimedOut || res.ExitCode != 0 {
		return "", fmt.Errorf("relay id did not run cleanly (exit %d)", res.ExitCode)
	}
	return pagercli.ParseRelayID(out.Stdout)
}

// wallClock is time.Now without its monotonic reading (see sleep).
func wallClock() time.Time { return time.Now().Round(0) }

// spread varies d by up to ±JitterFraction, so links that failed together do
// not retry together.
func spread(d time.Duration) time.Duration {
	f := 1 + JitterFraction*(2*rand.Float64()-1)
	return time.Duration(float64(d) * f)
}
