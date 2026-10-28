package link

import (
	"time"

	"github.com/iMUngHee/switchboard/internal/round"
	"github.com/iMUngHee/switchboard/internal/state"
)

// Scheduling policy (plan S3).
const (
	MaxRerunsNow   = 2                // a cut or capped round is rerun at once this many times in a row
	DrainRounds    = 5                // back-to-back rounds before a draining direction yields
	DrainTime      = 30 * time.Second // or this long, whichever comes first
	RetryCap       = 5 * time.Minute  // the longest backoff after a pager failure
	OfflineCap     = 30 * time.Second // the longest wait while the far side is unreachable
	AuthProbe      = time.Minute      // how often a link waiting on credentials tries again
	OfflineGrace   = 10 * time.Minute // unreachable this long after the last contact: tell a person
	DegradedRuns   = 5                // the same failure this many times …
	DegradedWindow = 15 * time.Minute // … within this long: tell a person, keep retrying
	JitterFraction = 0.2              // backoff waits vary by up to ±20%
)

// direction is one direction of a link at run time: how to run it, its part
// of the stored state, and what the scheduler remembers between rounds.
type direction struct {
	name  string
	round round.Direction
	poll  time.Duration
	st    *state.Direction // the stored part; the link's mutex guards it
	limit int              // relay out --limit for the next round

	reruns     int         // cut or capped rounds rerun at once, in a row
	drained    int         // rounds run back to back while draining
	drainStart time.Time   // when the current drain began
	streak     string      // the code of the current streak of pager failures
	streakAt   []time.Time // when each failure of that streak happened, within DegradedWindow
	isDegraded bool        // a streak got long enough; it holds until a success or a resume
}

// after records outcome o, which ended at now, and returns how long to wait
// before the next round. jitter spreads a backoff; tests pass none.
func (d *direction) after(o round.Outcome, now time.Time, jitter func(time.Duration) time.Duration) time.Duration {
	st := d.st
	st.Class, st.Code = string(o.Class), o.Code
	st.PendingAck = o.Pending
	if o.Limit > 0 {
		d.limit = o.Limit
	}
	if o.Contact {
		st.LastContact = now
	}
	if o.Finals > 0 {
		st.LastSuccess = now
	}

	var wait time.Duration
	switch o.Class {
	case round.Idle, round.OK:
		st.ConsecutiveFailures, d.reruns = 0, 0
		d.endStreak()
		d.isDegraded = false
		wait = d.poll
		if o.Full && o.Finals > 0 && o.NotBefore.IsZero() {
			if d.drained == 0 {
				d.drainStart = now
			}
			d.drained++
			if d.drained < DrainRounds && now.Sub(d.drainStart) < DrainTime {
				wait = 0
			} else {
				d.drained = 0 // yield; the next poll starts a new drain
			}
		} else {
			d.drained = 0
		}
	case round.Rerun:
		d.drained = 0
		if d.reruns < MaxRerunsNow {
			d.reruns++
			wait = 0
			break
		}
		wait = d.fail(o, now, jitter)
	case round.Retry, round.Timeout:
		d.drained = 0
		wait = d.fail(o, now, jitter)
	case round.Offline:
		d.drained = 0
		d.fail(o, now, jitter)
		wait = min(jitter(backoff(d.poll, st.ConsecutiveFailures, OfflineCap)), OfflineCap)
	case round.Auth:
		d.drained = 0
		d.fail(o, now, jitter)
		wait = AuthProbe
	case round.NeedsHuman, round.Stopped:
		d.drained = 0
		return 0 // the link stops this direction; nothing is scheduled
	}

	at := now.Add(wait)
	if o.NotBefore.After(at) {
		at = o.NotBefore
	}
	if wait > d.poll || at.After(now.Add(wait)) {
		st.NotBefore = at // a restart honours backoff and the receiver's rate cap
	} else {
		st.NotBefore = time.Time{}
	}
	return at.Sub(now)
}

// fail counts a failure and returns the backoff for a pager failure:
// doubling from the poll interval up to RetryCap.
func (d *direction) fail(o round.Outcome, now time.Time, jitter func(time.Duration) time.Duration) time.Duration {
	st := d.st
	st.ConsecutiveFailures++
	d.reruns = 0
	// Only pager failures make a streak: an unreachable far side has its own
	// grace, and a key that ssh refuses is reported at once.
	switch {
	case o.Class != round.Retry:
		d.endStreak()
	default:
		if o.Code != d.streak {
			d.endStreak()
			d.streak = o.Code
		}
		d.streakAt = append(d.streakAt, now)
		for len(d.streakAt) > 0 && now.Sub(d.streakAt[0]) > DegradedWindow {
			d.streakAt = d.streakAt[1:]
		}
		if len(d.streakAt) >= DegradedRuns {
			d.isDegraded = true
		}
	}
	return min(jitter(backoff(d.poll, st.ConsecutiveFailures, RetryCap)), RetryCap)
}

// degraded reports whether the same pager failure repeated often enough,
// quickly enough, to tell a person while still retrying. Once it has, it
// holds until a round succeeds or a person resumes the link: a backoff grown
// to its cap fits fewer failures in the window, and a failure that changes
// its code has not gone away.
func (d *direction) degraded() bool { return d.isDegraded }

// endStreak forgets the current streak of pager failures, but not that the
// direction is degraded.
func (d *direction) endStreak() { d.streak, d.streakAt = "", nil }

// backoff doubles from poll for each failure after the first, up to limit.
// It never overflows, whatever the poll interval.
func backoff(poll time.Duration, failures int, limit time.Duration) time.Duration {
	w := min(poll, limit)
	for i := 1; i < failures && w < limit; i++ {
		w *= 2
	}
	return min(w, limit)
}

// firstWait is how long a direction waits when the link starts: none, unless
// the stored state says not before some time.
func (d *direction) firstWait(now time.Time) time.Duration {
	if d.st.NotBefore.After(now) {
		return d.st.NotBefore.Sub(now)
	}
	return 0
}
