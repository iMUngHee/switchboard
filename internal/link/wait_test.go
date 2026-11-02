package link

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/config"
	"github.com/iMUngHee/switchboard/internal/pagercli"
	"github.com/iMUngHee/switchboard/internal/proc"
	"github.com/iMUngHee/switchboard/internal/round"
	"github.com/iMUngHee/switchboard/internal/state"
	"github.com/iMUngHee/switchboard/internal/testenv"
	"github.com/iMUngHee/switchboard/internal/transport"
)

// waiter is a runWait that answers from a script and counts its calls. With
// block set it holds each wait until its context ends, and records that;
// with delay it takes that long to answer.
type waiter struct {
	mu        sync.Mutex
	answers   []waitAnswer
	block     bool
	delay     time.Duration
	calls     int
	cancelled int
	started   chan struct{}
}

func newWaiter(answers ...waitAnswer) *waiter {
	return &waiter{answers: answers, started: make(chan struct{}, 16)}
}

func (w *waiter) run(ctx context.Context, _ *direction) waitAnswer {
	w.mu.Lock()
	w.calls++
	block := w.block
	a := waitTimeout
	if len(w.answers) > 0 {
		a, w.answers = w.answers[0], w.answers[1:]
	}
	w.mu.Unlock()
	w.started <- struct{}{}
	if block {
		<-ctx.Done()
		w.mu.Lock()
		w.cancelled++
		w.mu.Unlock()
		return waitFailed
	}
	time.Sleep(w.delay)
	return a
}

func (w *waiter) count() (calls, cancelled int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls, w.cancelled
}

// waitLink is a test link whose inbound direction polls every 200ms and
// waits for mail through w, asking for 100ms waits.
func waitLink(t *testing.T, c *clock, w *waiter, outcomes ...round.Outcome) *Link {
	t.Helper()
	l, _ := testLink(t, c, outcomes...)
	l.runWait = w.run
	l.awaitEvery = 20 * time.Millisecond
	l.waitFor = 100 * time.Millisecond
	for _, d := range l.dirs {
		d.poll = 200 * time.Millisecond
	}
	return l
}

// timedPause runs one pause of the inbound direction and says how long it
// took.
func timedPause(t *testing.T, l *Link) time.Duration {
	t.Helper()
	start := time.Now()
	if !l.pause(t.Context(), 1, l.dirs[1], l.dirs[1].poll) {
		t.Fatal("the pause reported a cancelled context")
	}
	return time.Since(start)
}

// TestWaitReplacesTheIdlePoll pins when a direction waits for mail instead
// of sleeping, and what each answer leads to (plan relay-wait-trigger).
func TestWaitReplacesTheIdlePoll(t *testing.T) {
	carried := round.Outcome{Class: round.OK, Records: 3, Finals: 3, Contact: true}
	in := func(l *Link, o round.Outcome) {
		l.runRound = always(o)
		l.step(t.Context(), l.dirs[1])
	}

	t.Run("after a round that left nothing to carry", func(t *testing.T) {
		for name, o := range map[string]round.Outcome{"idle": idle, "carried everything": carried} {
			w := newWaiter(waitReady)
			l := waitLink(t, newClock(), w)
			in(l, o)
			if took := timedPause(t, l); took >= l.dirs[1].poll {
				t.Errorf("%s: the pause took %v, a poll sleep; want an answer from relay wait", name, took)
			}
			if calls, _ := w.count(); calls != 1 {
				t.Errorf("%s: relay wait ran %d times, want 1", name, calls)
			}
		}
	})

	t.Run("timeout runs a round too", func(t *testing.T) {
		w := newWaiter(waitTimeout)
		w.delay = 60 * time.Millisecond // past half the 100ms asked for
		l := waitLink(t, newClock(), w)
		in(l, idle)
		if took := timedPause(t, l); took >= w.delay+l.dirs[1].poll/2 {
			t.Errorf("the pause took %v after a timeout answer", took)
		}
	})

	t.Run("a timeout long before the time asked for polls once", func(t *testing.T) {
		w := newWaiter(waitTimeout)
		l := waitLink(t, newClock(), w)
		in(l, idle)
		if took := timedPause(t, l); took < l.dirs[1].poll {
			t.Errorf("an instant timeout ran a round after %v, not a poll sleep", took)
		}
	})

	t.Run("a timeout across a machine's sleep is an answer", func(t *testing.T) {
		// The monotonic clock stopped while the machine slept, so only the
		// wall clock shows the wait lasted.
		l := waitLink(t, newClock(), newWaiter())
		in(l, idle)
		base := l.now()
		var slept atomic.Bool
		l.now = func() time.Time {
			if slept.Load() {
				return base.Add(10 * time.Minute)
			}
			return base
		}
		l.runWait = func(context.Context, *direction) waitAnswer { slept.Store(true); return waitTimeout }
		if took := timedPause(t, l); took >= l.dirs[1].poll {
			t.Errorf("a timeout after the machine slept was taken as early: the pause took %v", took)
		}
	})

	t.Run("not after a deferral, a backoff, a drain or a stop", func(t *testing.T) {
		for name, o := range map[string]round.Outcome{
			"rate deferral": {Class: round.OK, Records: 2, NotBefore: newClock().now().Add(time.Hour), Contact: true},
			// Not an outcome the engine gives today, where a deferral always
			// leaves a record unanswered: a time not to run before is enough.
			"NotBefore alone":      {Class: round.OK, Records: 2, Finals: 2, NotBefore: newClock().now().Add(time.Hour), Contact: true},
			"records not final":    {Class: round.OK, Records: 3, Finals: 2, Contact: true},
			"results still to ack": {Class: round.OK, Records: 1, Finals: 1, Pending: []string{"{}"}, Contact: true},
			"full batch":           full,
			"retry":                retry,
			"rerun":                rerun,
			"offline":              offline,
			"auth":                 auth,
			"needs a person":       {Class: round.NeedsHuman, Code: "refused", Stage: round.StageIn},
		} {
			l := waitLink(t, newClock(), newWaiter())
			in(l, o)
			if l.mayWait(l.dirs[1]) {
				t.Errorf("%s: the next pause would wait for mail", name)
			}
		}
	})

	t.Run("a stopped link does not wait", func(t *testing.T) {
		l := waitLink(t, newClock(), newWaiter())
		in(l, idle)
		l.mu.Lock()
		l.st.NeedsHuman = true
		l.mu.Unlock()
		if l.mayWait(l.dirs[1]) {
			t.Error("a stopped link would wait for mail")
		}
	})

	t.Run("ready, then nothing found: one poll before the next wait", func(t *testing.T) {
		w := newWaiter(waitReady, waitReady)
		l := waitLink(t, newClock(), w)
		in(l, idle)
		timedPause(t, l) // ready
		in(l, idle)      // yet relay out had nothing
		if l.mayWait(l.dirs[1]) {
			t.Fatal("wait and relay out disagree, yet the next pause would wait again")
		}
		in(l, idle)
		if !l.mayWait(l.dirs[1]) {
			t.Fatal("after one poll the direction does not wait again")
		}
		// After a ready that found mail, waiting resumes at once.
		timedPause(t, l)
		in(l, carried)
		if !l.mayWait(l.dirs[1]) {
			t.Fatal("after a ready that found mail, the next pause does not wait")
		}
	})

	t.Run("missing: poll until WaitRetry, or a resume", func(t *testing.T) {
		c := newClock()
		w := newWaiter(waitMissing)
		l := waitLink(t, c, w)
		in(l, idle)
		if took := timedPause(t, l); took < l.dirs[1].poll {
			t.Errorf("after exit 2 the pause took %v, want a poll sleep", took)
		}
		in(l, idle)
		if l.mayWait(l.dirs[1]) {
			t.Fatal("a side without relay wait is asked again at once")
		}
		c.add(WaitRetry - time.Second)
		if l.mayWait(l.dirs[1]) {
			t.Fatal("asked again before WaitRetry")
		}
		c.add(time.Second)
		if !l.mayWait(l.dirs[1]) {
			t.Fatal("not asked again after WaitRetry")
		}

		w2 := newWaiter(waitMissing)
		l2 := waitLink(t, newClock(), w2)
		in(l2, idle)
		timedPause(t, l2)
		l2.Resume()
		in(l2, idle)
		if !l2.mayWait(l2.dirs[1]) {
			t.Fatal("a resume does not clear the missing mark")
		}
	})

	t.Run("a failure polls once, then waits again", func(t *testing.T) {
		w := newWaiter(waitFailed)
		l := waitLink(t, newClock(), w)
		in(l, idle)
		if took := timedPause(t, l); took < l.dirs[1].poll {
			t.Errorf("after a failed wait the pause took %v, want a poll sleep", took)
		}
		in(l, idle)
		if !l.mayWait(l.dirs[1]) {
			t.Fatal("one failed wait stopped the direction waiting")
		}
	})

	t.Run("asks the From side", func(t *testing.T) {
		log := filepath.Join(t.TempDir(), "log.jsonl")
		l := waitLink(t, newClock(), newWaiter())
		l.waitFor = 0 // what a running link asks for
		l.dirs[1].round.From = round.Side{Endpoint: testPagerPlan(t, log, map[string]any{"stdout": "ready\n", "skip_stdin": true}), Peer: "laptop"}
		if a := l.relayWait(t.Context(), l.dirs[1]); a != waitReady {
			t.Fatalf("answer %v, want ready", a)
		}
		want := []string{"relay", "wait", "--peer", "laptop", "--timeout", "5m0s"}
		if got := loggedArgs(t, log); len(got) != 1 || !slices.Equal(got[0], want) {
			t.Fatalf("ran %q, want %q once", got, want)
		}
	})
}

// TestReadWait: only a clean exit with one known line is an answer.
func TestReadWait(t *testing.T) {
	ran := func(exit int, stdout string) pagercli.Output {
		return pagercli.Output{Stdout: []byte(stdout), Result: proc.Result{Started: true, ExitCode: exit}}
	}
	for _, tc := range []struct {
		name string
		out  pagercli.Output
		want waitAnswer
	}{
		{"ready", ran(0, "ready\n"), waitReady},
		{"ready with CRLF", ran(0, "ready\r\n"), waitReady},
		{"timeout", ran(0, "timeout\n"), waitTimeout},
		{"exit 2", ran(2, ""), waitMissing},
		{"exit 1", ran(1, ""), waitFailed},
		{"exit 255 from ssh", ran(255, ""), waitFailed},
		{"no line", ran(0, ""), waitFailed},
		{"two lines", ran(0, "ready\nready\n"), waitFailed},
		{"an unknown word", ran(0, "maybe\n"), waitFailed},
		{"stopped at its deadline", pagercli.Output{Result: proc.Result{Started: true, TimedOut: true, ExitCode: -1}}, waitFailed},
		// A wait that handled the stop and exited 0 did not end on its own.
		{"stopped at its deadline, exit 0", pagercli.Output{Stdout: []byte("ready\n"), Result: proc.Result{Started: true, TimedOut: true}}, waitFailed},
		{"never started", pagercli.Output{Result: proc.Result{ExitCode: -1}}, waitFailed},
		{"over the cap", pagercli.Output{Stdout: []byte("ready\n"), Over: true, Result: proc.Result{Started: true}}, waitFailed},
	} {
		if got := readWait(tc.out, nil); got != tc.want {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestWaitYieldsToControl: a wait for mail ends for a wake-up, a shutdown, a
// stop and a machine that slept, and is always reaped first.
func TestWaitYieldsToControl(t *testing.T) {
	blocking := func() *waiter { w := newWaiter(); w.block = true; return w }

	t.Run("resume ends the wait and a round runs", func(t *testing.T) {
		w := blocking()
		l := waitLink(t, newClock(), w)
		l.runRound = always(idle)
		l.step(t.Context(), l.dirs[1])
		done := make(chan bool, 1)
		go func() { done <- l.pause(t.Context(), 1, l.dirs[1], l.dirs[1].poll) }()
		<-w.started
		l.Resume()
		select {
		case more := <-done:
			if !more {
				t.Fatal("the pause reported a cancelled context")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a resume did not end the wait")
		}
		if _, cancelled := w.count(); cancelled != 1 {
			t.Fatalf("the wait was not ended and reaped: cancelled %d", cancelled)
		}
	})

	t.Run("shutdown ends the wait and runs no round", func(t *testing.T) {
		w := blocking()
		l := waitLink(t, newClock(), w)
		// Rounds get this long to finish at shutdown; a wait gets none of it.
		l.opt.Grace = 3 * time.Second
		var runs atomic.Int32
		l.runRound = func(context.Context, *round.Engine, round.Direction, round.Input) round.Outcome {
			runs.Add(1)
			return idle
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() { l.Run(ctx); close(done) }()
		<-w.started
		<-w.started // both directions wait
		before := runs.Load()
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("Run did not return within 1s of shutdown: the waits held on into the rounds' grace")
		}
		if runs.Load() != before {
			t.Fatalf("%d rounds ran after shutdown", runs.Load()-before)
		}
		if _, cancelled := w.count(); cancelled != 2 {
			t.Fatalf("waits ended: %d, want both", cancelled)
		}
	})

	t.Run("a stopped link ends the wait", func(t *testing.T) {
		w := blocking()
		l := waitLink(t, newClock(), w)
		l.runRound = always(idle)
		l.step(t.Context(), l.dirs[1])
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan bool, 1)
		go func() { done <- l.pause(ctx, 1, l.dirs[1], l.dirs[1].poll) }()
		<-w.started
		l.mu.Lock()
		l.st.NeedsHuman = true
		l.mu.Unlock()
		waitFor(t, "the wait to end", func() bool { _, c := w.count(); return c == 1 })
		select {
		case <-done:
			t.Fatal("a stopped link went on to a round without a wake-up")
		case <-time.After(300 * time.Millisecond):
		}
		cancel()
		<-done
	})

	t.Run("a machine that slept ends the wait and runs a round", func(t *testing.T) {
		w := blocking()
		l := waitLink(t, newClock(), w)
		l.runRound = always(idle)
		l.step(t.Context(), l.dirs[1])
		base := l.now()
		var slept atomic.Bool
		l.now = func() time.Time {
			if slept.Load() {
				return base.Add(8 * time.Hour)
			}
			return base
		}
		done := make(chan bool, 1)
		go func() { done <- l.pause(t.Context(), 1, l.dirs[1], l.dirs[1].poll) }()
		<-w.started
		slept.Store(true)
		select {
		case more := <-done:
			if !more {
				t.Fatal("the pause reported a cancelled context")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the wait outlived the machine's sleep")
		}
		l.mu.Lock()
		awake := l.awake
		l.mu.Unlock()
		if !awake.Equal(base.Add(8 * time.Hour)) {
			t.Fatalf("awake %v, want the time after the sleep", awake)
		}
		if _, cancelled := w.count(); cancelled != 1 {
			t.Fatal("the wait was not ended and reaped")
		}
	})
}

// TestWaitFallsBackWithoutRelayWait: against a pager that has no relay wait,
// rounds still run at the poll interval, and wait is asked once per
// direction within WaitRetry.
func TestWaitFallsBackWithoutRelayWait(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log.jsonl")
	ep := testPagerPlan(t, log, map[string]any{"exit": 2, "stderr": "unknown relay subcommand \"wait\"\n", "skip_stdin": true})
	l, _ := testLink(t, newClock())
	l.now = wallClock
	l.runWait = l.relayWait
	l.awaitEvery = 20 * time.Millisecond
	for _, d := range l.dirs {
		d.poll = 200 * time.Millisecond
		d.round.From = round.Side{Endpoint: ep, Peer: "laptop"}
	}
	var runs atomic.Int32
	l.runRound = func(context.Context, *round.Engine, round.Direction, round.Input) round.Outcome {
		runs.Add(1)
		return idle
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	l.Run(ctx)
	if n := len(loggedArgs(t, log)); n != 2 {
		t.Fatalf("relay wait ran %d times, want once per direction", n)
	}
	// Two directions polling every 200ms for 3s, less two failed waits.
	if n := runs.Load(); n < 16 {
		t.Fatalf("only %d rounds ran: polling did not carry on", n)
	}
}

// savedInbound reads the inbound direction as the state file has it.
func savedInbound(t *testing.T, l *Link) state.Direction {
	t.Helper()
	st, err := state.Load(l.opt.StateDir, l.key)
	if err != nil {
		t.Fatal(err)
	}
	return st.Inbound
}

// TestStateRecordsTheWait: the state file says since when a direction has
// waited on relay wait, and until when it polls a side without one, so status
// can show both (plan status-shows-wait).
func TestStateRecordsTheWait(t *testing.T) {
	idleRound := func(l *Link) {
		l.runRound = always(idle)
		l.step(t.Context(), l.dirs[1])
	}
	pauseIn := func(l *Link) chan bool {
		done := make(chan bool, 1)
		go func() { done <- l.pause(t.Context(), 1, l.dirs[1], l.dirs[1].poll) }()
		return done
	}

	t.Run("a wait is saved as it begins, and the next round clears it", func(t *testing.T) {
		c := newClock()
		w := newWaiter()
		w.block = true
		l := waitLink(t, c, w)
		idleRound(l)
		began := c.now()
		done := pauseIn(l)
		<-w.started
		if got := savedInbound(t, l).WaitingSince; !got.Equal(began) {
			t.Fatalf("saved WaitingSince %v during the wait, want %v", got, began)
		}
		c.add(time.Minute)
		l.wakeAll()
		if !<-done {
			t.Fatal("the pause reported a cancelled context")
		}
		idleRound(l)
		if got := savedInbound(t, l).WaitingSince; !got.IsZero() {
			t.Fatalf("saved WaitingSince %v after the next round, want none", got)
		}
	})

	// Both sleep a poll before the next round; through that sleep the saved
	// state must not say the direction waits.
	for _, tc := range []struct {
		name   string
		answer waitAnswer
	}{{"a failed wait is cleared before its poll sleep", waitFailed}, {"exit 2 saves NoWaitUntil before its poll sleep", waitMissing}} {
		t.Run(tc.name, func(t *testing.T) {
			c := newClock()
			w := newWaiter(tc.answer)
			l := waitLink(t, c, w)
			l.dirs[1].poll = time.Minute // the sleep lasts until the wake below
			idleRound(l)
			done := pauseIn(l)
			<-w.started
			var got state.Direction
			waitFor(t, "the wait to be cleared", func() bool { got = savedInbound(t, l); return got.WaitingSince.IsZero() })
			select {
			case <-done:
				t.Fatal("the pause ended before the poll sleep")
			default:
			}
			want := time.Time{}
			if tc.answer == waitMissing {
				want = c.now().Add(WaitRetry)
			}
			if !got.NoWaitUntil.Equal(want) {
				t.Fatalf("saved NoWaitUntil %v, want %v", got.NoWaitUntil, want)
			}
			l.wakeAll()
			<-done
			if tc.answer != waitMissing {
				return
			}
			// The scheduler polls until that saved time.
			idleRound(l)
			c.add(WaitRetry - time.Second)
			if l.mayWait(l.dirs[1]) {
				t.Fatal("asked for relay wait again before NoWaitUntil")
			}
			c.add(time.Second)
			if !l.mayWait(l.dirs[1]) {
				t.Fatal("not asked again at NoWaitUntil")
			}
			// Asking again ends the mark, so status stops showing it.
			w.mu.Lock()
			w.block = true
			w.mu.Unlock()
			done = pauseIn(l)
			<-w.started
			if got := savedInbound(t, l); !got.NoWaitUntil.IsZero() || !got.WaitingSince.Equal(c.now()) {
				t.Fatalf("asked again: saved %+v, want waiting since %v and no NoWaitUntil", got, c.now())
			}
			l.wakeAll()
			<-done
		})
	}

	t.Run("the scheduler reads the stored NoWaitUntil, and resume clears it", func(t *testing.T) {
		c := newClock()
		l := waitLink(t, c, newWaiter())
		idleRound(l)
		l.mu.Lock()
		l.st.Inbound.NoWaitUntil = c.now().Add(time.Hour)
		l.mu.Unlock()
		if l.mayWait(l.dirs[1]) {
			t.Fatal("a stored NoWaitUntil did not hold relay wait off")
		}
		l.Resume()
		if got := savedInbound(t, l).NoWaitUntil; !got.IsZero() {
			t.Fatalf("saved NoWaitUntil %v after a resume, want none", got)
		}
		idleRound(l)
		if !l.mayWait(l.dirs[1]) {
			t.Fatal("after a resume the direction does not ask for relay wait")
		}
	})

	t.Run("Open clears both from a stored state", func(t *testing.T) {
		const id = "0123456789abcdef0123456789abcdef"
		dir := t.TempDir()
		plan := filepath.Join(t.TempDir(), "plan.json")
		if err := os.WriteFile(plan, []byte(`{"relay id":{"stdout":"`+id+`\n","skip_stdin":true}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("TESTPAGER_PLAN", plan)
		at := newClock().now()
		left := state.Direction{Class: "idle", LastContact: at, WaitingSince: at, NoWaitUntil: at.Add(WaitRetry)}
		key := state.Key(id, "vm")
		lock, err := state.TryLock(dir, key)
		if err != nil {
			t.Fatal(err)
		}
		err = state.Save(dir, key, state.Link{Outbound: left, Inbound: left})
		lock.Close()
		if err != nil {
			t.Fatal(err)
		}

		pager := testenv.TestPager(t)
		l, err := Open(t.Context(), Options{
			Name: "vm", Pager: config.Pager{Path: pager}, StateDir: dir,
			Engine: round.Engine{Runner: pagercli.Runner{Timeout: time.Minute}},
			Config: config.Link{Peer: "vm", RemotePeer: "laptop", PollOutbound: 5 * time.Second, PollInbound: 30 * time.Second,
				Exec: &config.Exec{Argv: []string{pager}}},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		l.mu.Lock()
		st := l.st
		l.mu.Unlock()
		// status reads the file, so it must say the same as memory at once.
		saved, err := state.Load(dir, key)
		if err != nil {
			t.Fatal(err)
		}
		for name, d := range map[string]state.Direction{
			"outbound": st.Outbound, "inbound": st.Inbound,
			"saved outbound": saved.Outbound, "saved inbound": saved.Inbound,
		} {
			if !d.WaitingSince.IsZero() || !d.NoWaitUntil.IsZero() {
				t.Errorf("%s after Open: waiting since %v, no wait until %v; want both cleared", name, d.WaitingSince, d.NoWaitUntil)
			}
			if d.Class != "idle" || !d.LastContact.Equal(at) {
				t.Errorf("%s after Open lost the rest of its state: %+v", name, d)
			}
		}
	})
}

// testPagerPlan is an exec endpoint running testpager with step as its
// answer to relay wait, logging each invocation to log.
func testPagerPlan(t *testing.T, log string, step map[string]any) transport.Endpoint {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.json")
	raw, _ := json.Marshal(map[string]any{"relay wait": step})
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return transport.ExecEndpoint{Argv: []string{testenv.TestPager(t)}, Env: map[string]string{"TESTPAGER_PLAN": path, "TESTPAGER_LOG": log}}
}

func loggedArgs(t *testing.T, log string) [][]string {
	t.Helper()
	f, err := os.Open(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var line struct{ Args []string }
		if err := json.Unmarshal(sc.Bytes(), &line); err != nil {
			t.Fatal(err)
		}
		out = append(out, line.Args)
	}
	return out
}
