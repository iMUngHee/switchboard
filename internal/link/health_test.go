package link

import (
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/round"
	"github.com/iMUngHee/switchboard/internal/state"
)

// TestOfflineGraceFromLastContact: after an hour of empty polls, the first
// failure is reported only once 10 minutes have passed since the last
// contact, and recovery is reported when contact returns.
func TestOfflineGraceFromLastContact(t *testing.T) {
	c := newClock()
	l, rec := testLink(t, c)
	in := l.dirs[1] // inbound, polled every 30s

	// An hour of empty polls that reached the far side.
	for c.now().Before(l.awake.Add(time.Hour)) {
		w, _ := l.step(t.Context(), in)
		c.add(w)
	}
	lastContact := l.st.Inbound.LastContact
	if len(rec.all()) != 0 || lastContact.IsZero() {
		t.Fatalf("notes %+v, last contact %v", rec.all(), lastContact)
	}

	// Then the far side stops answering.
	l.runRound = always(offline)
	var first time.Time
	for c.now().Before(lastContact.Add(15 * time.Minute)) {
		w, _ := l.step(t.Context(), in)
		if notes := rec.all(); len(notes) > 0 && first.IsZero() {
			first = notes[0].at
		}
		c.add(w)
	}
	if first.IsZero() {
		t.Fatal("15 minutes offline and no one was told")
	}
	if since := first.Sub(lastContact); since < OfflineGrace || since > OfflineGrace+OfflineCap {
		t.Fatalf("told %v after the last contact, want within [%v, %v]", since, OfflineGrace, OfflineGrace+OfflineCap)
	}
	if notes := rec.all(); len(notes) != 1 || notes[0].health != Offline || notes[0].hint == "" {
		t.Fatalf("notes %+v, want one offline note with the hint", notes)
	}

	// Contact returns: recovery is reported once.
	l.runRound = always(idle)
	l.step(t.Context(), in)
	l.step(t.Context(), in)
	if notes := rec.all(); len(notes) != 2 || notes[1].health != Healthy {
		t.Fatalf("notes %+v, want a recovery", notes)
	}
}

// TestGraceStartsAtRestartOrWake: a last contact from before this process
// started does not cut the grace short.
func TestGraceStartsAtRestartOrWake(t *testing.T) {
	c := newClock()
	l, rec := testLink(t, c)
	l.st.Inbound.LastContact = c.now().Add(-2 * time.Hour) // stored by an earlier run
	l.runRound = always(offline)
	for c.now().Before(l.awake.Add(OfflineGrace - time.Minute)) {
		w, _ := l.step(t.Context(), l.dirs[1])
		c.add(w)
	}
	if notes := rec.all(); len(notes) != 0 {
		t.Fatalf("told %+v before the grace since the start had passed", notes)
	}
	for c.now().Before(l.awake.Add(OfflineGrace + time.Minute)) {
		w, _ := l.step(t.Context(), l.dirs[1])
		c.add(w)
	}
	if notes := rec.all(); len(notes) != 1 || notes[0].health != Offline {
		t.Fatalf("notes %+v", notes)
	}
}

// TestHealthTransitions: credentials are reported at once; the same failure
// five times within 15 minutes is reported as degraded, once.
func TestHealthTransitions(t *testing.T) {
	t.Run("auth is reported at once", func(t *testing.T) {
		c := newClock()
		l, rec := testLink(t, c, auth)
		l.step(t.Context(), l.dirs[0])
		if notes := rec.all(); len(notes) != 1 || notes[0].health != Auth {
			t.Fatalf("notes %+v", notes)
		}
	})
	t.Run("the same failure five times is degraded", func(t *testing.T) {
		c := newClock()
		l, rec := testLink(t, c, repeat(8, retry)...)
		for i := range 8 {
			w, _ := l.step(t.Context(), l.dirs[0])
			c.add(w)
			if got := len(rec.all()); i < DegradedRuns-1 && got != 0 {
				t.Fatalf("told after %d failures", i+1)
			}
		}
		if notes := rec.all(); len(notes) != 1 || notes[0].health != Degraded {
			t.Fatalf("notes %+v", notes)
		}
	})
	t.Run("degraded holds while the failure goes on", func(t *testing.T) {
		c := newClock()
		l, rec := testLink(t, c)
		l.runRound = always(retry)
		for start := c.now(); c.now().Before(start.Add(time.Hour)); {
			w, _ := l.step(t.Context(), l.dirs[0])
			c.add(w)
		}
		if notes := rec.all(); len(notes) != 1 || notes[0].health != Degraded {
			t.Fatalf("notes over an hour of the same failure: %+v", notes)
		}
		l.runRound = always(idle)
		l.step(t.Context(), l.dirs[0])
		if notes := rec.all(); len(notes) != 2 || notes[1].health != Healthy {
			t.Fatalf("notes after it passed: %+v", notes)
		}
	})
	t.Run("a failure that changes its code is still degraded", func(t *testing.T) {
		c := newClock()
		other := retry
		other.Code = "pager-1"
		timeout := round.Outcome{Class: round.Timeout, Code: round.CodeDeadline}
		l, rec := testLink(t, c, append(repeat(5, retry), other, other, timeout)...)
		for range 8 {
			w, _ := l.step(t.Context(), l.dirs[0])
			c.add(w)
		}
		if notes := rec.all(); len(notes) != 1 || notes[0].health != Degraded {
			t.Fatalf("notes %+v, want only the degraded one", notes)
		}
	})
	t.Run("resume ends degraded", func(t *testing.T) {
		c := newClock()
		l, _ := testLink(t, c, repeat(5, retry)...)
		for range 5 {
			w, _ := l.step(t.Context(), l.dirs[0])
			c.add(w)
		}
		if !l.dirs[0].degraded() {
			t.Fatal("not degraded after five failures")
		}
		l.Resume()
		if l.dirs[0].degraded() {
			t.Fatal("still degraded after resume")
		}
	})
	t.Run("different failures are not one streak", func(t *testing.T) {
		c := newClock()
		var outcomes []round.Outcome
		for _, code := range []string{"db-locked", "pager-1", "db-locked", "pager-1", "db-locked", "pager-1"} {
			o := retry
			o.Code = code
			outcomes = append(outcomes, o)
		}
		l, rec := testLink(t, c, outcomes...)
		for range outcomes {
			w, _ := l.step(t.Context(), l.dirs[0])
			c.add(w)
		}
		if notes := rec.all(); len(notes) != 0 {
			t.Fatalf("notes %+v", notes)
		}
	})
	t.Run("failures spread over more than 15 minutes are not degraded", func(t *testing.T) {
		c := newClock()
		l, rec := testLink(t, c, repeat(5, retry)...)
		for range 5 {
			l.step(t.Context(), l.dirs[0])
			c.add(4 * time.Minute)
		}
		if notes := rec.all(); len(notes) != 0 {
			t.Fatalf("notes %+v", notes)
		}
	})
	t.Run("shutting down is not a recovery", func(t *testing.T) {
		c := newClock()
		l, rec := testLink(t, c, auth, round.Outcome{Class: round.Stopped, Pending: []string{"result"}})
		l.step(t.Context(), l.dirs[0])
		l.step(t.Context(), l.dirs[0])
		if notes := rec.all(); len(notes) != 1 || notes[0].health != Auth {
			t.Fatalf("notes %+v", notes)
		}
		saved, _ := state.Load(l.opt.StateDir, l.key)
		if saved.Outbound.Class != string(round.Auth) || len(saved.Outbound.PendingAck) != 1 {
			t.Fatalf("stored %+v", saved.Outbound)
		}
	})
	t.Run("repeated state write failures are degraded", func(t *testing.T) {
		c := newClock()
		l, rec := testLink(t, c)
		l.opt.StateDir = t.TempDir() + "/missing/dir" // every save fails
		for range stateFailures {
			l.step(t.Context(), l.dirs[0])
		}
		if notes := rec.all(); len(notes) != 1 || notes[0].health != Degraded {
			t.Fatalf("notes %+v", notes)
		}
	})
}
