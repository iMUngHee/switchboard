package link

import (
	"context"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/round"
	"github.com/iMUngHee/switchboard/internal/state"
)

var (
	idle    = round.Outcome{Class: round.Idle, Contact: true}
	full    = round.Outcome{Class: round.OK, Records: 100, Finals: 100, Full: true, Contact: true}
	retry   = round.Outcome{Class: round.Retry, Code: "db-locked", Stage: round.StageIn}
	rerun   = round.Outcome{Class: round.Rerun, Code: round.CodeCut, Stage: round.StageIn}
	offline = round.Outcome{Class: round.Offline, Code: "conn-refused", Stage: round.StageOut}
	auth    = round.Outcome{Class: round.Auth, Code: "auth", Stage: round.StageOut}
)

// waits runs the outbound direction (poll 5s) through the outcomes, moving
// the clock by each returned wait plus gap, and returns the waits.
func waits(t *testing.T, gap time.Duration, outcomes ...round.Outcome) []time.Duration {
	t.Helper()
	c := newClock()
	l, _ := testLink(t, c, outcomes...)
	var got []time.Duration
	for range outcomes {
		w, _ := l.step(t.Context(), l.dirs[0])
		got = append(got, w)
		c.add(w + gap)
	}
	return got
}

func s(n int) time.Duration { return time.Duration(n) * time.Second }

// TestSchedulerTiming pins when a direction runs again (plan S3), on a fake
// clock.
func TestSchedulerTiming(t *testing.T) {
	for _, tc := range []struct {
		name     string
		gap      time.Duration
		outcomes []round.Outcome
		want     []time.Duration
	}{
		{"an idle direction polls at its interval", 0, repeat(3, idle), []time.Duration{s(5), s(5), s(5)}},
		{"a full batch drains, then yields after five rounds", 0, repeat(7, full),
			[]time.Duration{0, 0, 0, 0, s(5), 0, 0}},
		{"a drain yields after 30 seconds", s(10), repeat(5, full),
			[]time.Duration{0, 0, 0, s(5), 0}},
		{"a full batch without finals does not drain", 0,
			[]round.Outcome{{Class: round.OK, Records: 100, Full: true}}, []time.Duration{s(5)}},
		{"pager failures back off, doubling to the cap", 0, repeat(9, retry),
			[]time.Duration{s(5), s(10), s(20), s(40), s(80), s(160), s(300), s(300), s(300)}},
		{"success resets the backoff", 0, []round.Outcome{retry, retry, idle, retry},
			[]time.Duration{s(5), s(10), s(5), s(5)}},
		{"a cut round reruns twice at once, then backs off", 0, repeat(4, rerun),
			[]time.Duration{0, 0, s(5), 0}},
		{"an unreachable far side retries, capped at 30 seconds", 0, repeat(6, offline),
			[]time.Duration{s(5), s(10), s(20), s(30), s(30), s(30)}},
		{"a link waiting on credentials probes every minute", 0, repeat(2, auth),
			[]time.Duration{time.Minute, time.Minute}},
		{"a timeout backs off like a pager failure", 0, repeat(3, round.Outcome{Class: round.Timeout, Code: round.CodeDeadline}),
			[]time.Duration{s(5), s(10), s(20)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := waits(t, tc.gap, tc.outcomes...); !slices.Equal(got, tc.want) {
				t.Fatalf("waits %v, want %v", got, tc.want)
			}
		})
	}

	t.Run("a rate deferral is honoured over draining", func(t *testing.T) {
		c := newClock()
		o := full
		o.NotBefore = c.now().Add(10 * time.Minute)
		l, _ := testLink(t, c, o)
		if w, _ := l.step(t.Context(), l.dirs[0]); w != 10*time.Minute {
			t.Fatalf("wait %v, want 10m", w)
		}
		if !l.st.Outbound.NotBefore.Equal(o.NotBefore) {
			t.Fatalf("stored not_before %v", l.st.Outbound.NotBefore)
		}
	})

	t.Run("a restart honours stored backoff", func(t *testing.T) {
		c := newClock()
		l, _ := testLink(t, c, repeat(6, retry)...)
		for range 6 {
			l.step(t.Context(), l.dirs[0])
		}
		saved, err := state.Load(l.opt.StateDir, l.key)
		if err != nil {
			t.Fatal(err)
		}
		c.add(time.Minute)
		d := &direction{poll: s(5), st: &saved.Outbound}
		if w := d.firstWait(c.now()); w != s(160)-time.Minute {
			t.Fatalf("first wait after a restart %v, want %v", w, s(160)-time.Minute)
		}
		if saved.Outbound.ConsecutiveFailures != 6 || saved.Outbound.Code != "db-locked" {
			t.Fatalf("stored %+v", saved.Outbound)
		}
	})

	t.Run("needs-human stops the link until resume", func(t *testing.T) {
		c := newClock()
		var runs atomic.Int32
		l, rec := testLink(t, c, round.Outcome{Class: round.NeedsHuman, Code: "refused", Stage: round.StageIn})
		inner := l.runRound
		l.runRound = func(ctx context.Context, e *round.Engine, d round.Direction, in round.Input) round.Outcome {
			runs.Add(1)
			return inner(ctx, e, d, in)
		}
		l.step(t.Context(), l.dirs[0])
		l.step(t.Context(), l.dirs[1]) // the other direction stops too
		if runs.Load() != 1 || !l.stopped() {
			t.Fatalf("runs %d, stopped %v", runs.Load(), l.stopped())
		}
		saved, _ := state.Load(l.opt.StateDir, l.key)
		if !saved.NeedsHuman {
			t.Fatal("needs-human was not stored")
		}
		l.Resume()
		if l.stopped() {
			t.Fatal("still stopped after resume")
		}
		l.step(t.Context(), l.dirs[0])
		if runs.Load() != 2 {
			t.Fatalf("runs after resume %d", runs.Load())
		}
		if got := rec.all(); len(got) != 2 || got[0].health != NeedsHuman || got[1].health != Healthy {
			t.Fatalf("notes %+v", got)
		}
	})
}

// TestSemaphoreBoundsRounds: links sharing a semaphore never run more rounds
// at once than it allows.
func TestSemaphoreBoundsRounds(t *testing.T) {
	sem := make(chan struct{}, 1)
	var now, peak atomic.Int32
	var links []*Link
	for range 2 {
		l, _ := testLink(t, newClock())
		l.opt.Sem = sem
		l.runRound = func(context.Context, *round.Engine, round.Direction, round.Input) round.Outcome {
			n := now.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			time.Sleep(20 * time.Millisecond)
			now.Add(-1)
			return idle
		}
		links = append(links, l)
	}
	var wg sync.WaitGroup
	for _, l := range links {
		for _, d := range l.dirs {
			wg.Go(func() { l.step(t.Context(), d) })
		}
	}
	wg.Wait()
	if peak.Load() != 1 {
		t.Fatalf("%d rounds ran at once, want 1", peak.Load())
	}
}

// TestJitterStaysInBounds: a spread wait stays within ±20%.
func TestJitterStaysInBounds(t *testing.T) {
	for range 1000 {
		if w := spread(10 * time.Second); w < 8*time.Second || w > 12*time.Second {
			t.Fatalf("spread 10s to %v", w)
		}
	}
}

// TestBackoffNeverOverflows: a poll interval near or past the caps still
// backs off to the cap, never to a negative or huge wait.
func TestBackoffNeverOverflows(t *testing.T) {
	for _, poll := range []time.Duration{time.Second, time.Hour, 3 * time.Hour} {
		d := &direction{poll: poll, st: &state.Direction{}}
		for n := range 100 {
			w := d.after(retry, newClock().now(), func(d time.Duration) time.Duration { return d })
			if w <= 0 || w > RetryCap {
				t.Fatalf("poll %v, failure %d: wait %v", poll, n+1, w)
			}
		}
		if got := backoff(poll, 1000, OfflineCap); got != min(poll, OfflineCap) && got != OfflineCap {
			t.Fatalf("offline backoff for poll %v: %v", poll, got)
		}
	}
}

// TestShutdownStartsNoRound: once the link is stopping, a direction waiting
// for the semaphore, or about to run, starts nothing.
func TestShutdownStartsNoRound(t *testing.T) {
	l, _ := testLink(t, newClock())
	var runs atomic.Int32
	l.runRound = func(context.Context, *round.Engine, round.Direction, round.Input) round.Outcome {
		runs.Add(1)
		return idle
	}
	stopping, stop := context.WithCancel(t.Context())
	stop()
	if _, o := l.turn(stopping, t.Context(), l.dirs[0]); o.Class != round.Stopped || runs.Load() != 0 {
		t.Fatalf("outcome %s, runs %d", o.Class, runs.Load())
	}

	l.opt.Sem = make(chan struct{}, 1)
	l.opt.Sem <- struct{}{} // another link holds the only slot
	waiting, stop2 := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, stop2)
	if _, o := l.turn(waiting, t.Context(), l.dirs[0]); o.Class != round.Stopped || runs.Load() != 0 {
		t.Fatalf("outcome %s, runs %d after waiting for the semaphore", o.Class, runs.Load())
	}
}

// TestSleepUsesTheWallClock: a wait re-reads the wall clock, and a jump far
// past where a timer should have fired counts as the machine waking.
func TestSleepUsesTheWallClock(t *testing.T) {
	l, _ := testLink(t, newClock())
	base := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	l.now = func() time.Time {
		// sleep reads the clock for its target, the time left and when the
		// timer should fire; the machine sleeps through that timer.
		if calls.Add(1) > 3 {
			return base.Add(8 * time.Hour)
		}
		return base
	}
	if !l.sleep(t.Context(), 0, 20*time.Millisecond) {
		t.Fatal("sleep reported a cancelled context")
	}
	if !l.awake.Equal(base.Add(8 * time.Hour)) {
		t.Fatalf("awake %v, want the time after the jump", l.awake)
	}
	if s := wallClock().String(); strings.Contains(s, "m=") {
		t.Fatalf("the link's clock keeps a monotonic reading: %s", s)
	}
}

// TestNotificationsKeepTheirOrder: a health change told late does not
// overwrite a later one already told.
func TestNotificationsKeepTheirOrder(t *testing.T) {
	c := newClock()
	l, rec := testLink(t, c)
	l.tell(2, Healthy)
	l.tell(1, Offline)
	if notes := rec.all(); len(notes) != 1 || notes[0].health != Healthy {
		t.Fatalf("notes %+v", notes)
	}

	// Told from many goroutines at once, the last word is the latest change.
	l2, rec2 := testLink(t, c)
	var wg sync.WaitGroup
	for seq := uint64(1); seq <= 100; seq++ {
		h := Offline
		if seq == 100 {
			h = Healthy
		}
		wg.Go(func() { l2.tell(seq, h) })
	}
	wg.Wait()
	if notes := rec2.all(); len(notes) == 0 || notes[len(notes)-1].health != Healthy {
		t.Fatalf("the last note is not the latest change: %+v", notes)
	}
}

// TestSleepIgnoresAClockSetBack: a wall clock set back an hour does not
// stretch a short wait.
func TestSleepIgnoresAClockSetBack(t *testing.T) {
	l, _ := testLink(t, newClock())
	base := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	l.now = func() time.Time {
		if calls.Add(1) > 1 {
			return base.Add(-time.Hour)
		}
		return base
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if !l.sleep(ctx, 0, 50*time.Millisecond) {
		t.Fatal("a 50ms wait outlasted 5s after the clock was set back")
	}
}
