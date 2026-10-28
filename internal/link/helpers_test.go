package link

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/config"
	"github.com/iMUngHee/switchboard/internal/round"
	"github.com/iMUngHee/switchboard/internal/state"
)

// clock is a fake wall clock the tests move by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// note is one notification, stamped with the fake time it was sent.
type note struct {
	at     time.Time
	health Health
	hint   string
}

type recorder struct {
	mu    sync.Mutex
	clock *clock
	notes []note
}

func (r *recorder) Notify(link string, h Health, hint string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notes = append(r.notes, note{r.clock.now(), h, hint})
}

func (r *recorder) all() []note {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]note(nil), r.notes...)
}

// testLink is a link with no pager behind it: its rounds come from outcomes,
// in order, and its clock is c.
func testLink(t *testing.T, c *clock, outcomes ...round.Outcome) (*Link, *recorder) {
	t.Helper()
	dir := t.TempDir()
	key := state.Key(strings.Repeat("a", 32), "vm")
	lock, err := state.TryLock(dir, key)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	rec := &recorder{clock: c}
	l := &Link{
		opt: Options{Name: "vm", StateDir: dir, Notifier: rec, Log: slog.New(slog.DiscardHandler),
			Config: config.Link{Peer: "vm", RemotePeer: "laptop", Hint: "run `ssh vm.example.com` once"}},
		key: key, lock: lock, notified: Healthy,
		now:    c.now,
		jitter: func(d time.Duration) time.Duration { return d },
	}
	l.awake = c.now()
	l.dirs[0] = &direction{name: "outbound", poll: 5 * time.Second, st: &l.st.Outbound}
	l.dirs[1] = &direction{name: "inbound", poll: 30 * time.Second, st: &l.st.Inbound}
	for i := range l.wake {
		l.wake[i] = make(chan struct{}, 1)
	}
	var mu sync.Mutex
	l.runRound = func(context.Context, *round.Engine, round.Direction, round.Input) round.Outcome {
		mu.Lock()
		defer mu.Unlock()
		if len(outcomes) == 0 {
			return round.Outcome{Class: round.Idle, Contact: true}
		}
		o := outcomes[0]
		outcomes = outcomes[1:]
		return o
	}
	return l, rec
}

// always is a round runner that always ends with o.
func always(o round.Outcome) func(context.Context, *round.Engine, round.Direction, round.Input) round.Outcome {
	return func(context.Context, *round.Engine, round.Direction, round.Input) round.Outcome { return o }
}

func repeat(n int, o round.Outcome) []round.Outcome {
	out := make([]round.Outcome, n)
	for i := range out {
		out[i] = o
	}
	return out
}
