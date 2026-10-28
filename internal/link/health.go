package link

import (
	"time"

	"github.com/iMUngHee/switchboard/internal/round"
)

// Health is what a person is told about a link.
type Health string

const (
	Healthy    Health = "ok" // also sent as the recovery from any other state
	Offline    Health = "offline"
	Degraded   Health = "degraded"
	Auth       Health = "auth"
	NeedsHuman Health = "needs-human"
)

var healthRank = map[Health]int{Healthy: 0, Offline: 1, Degraded: 2, Auth: 3, NeedsHuman: 4}

// Notifier tells a person about a link. Notify is called outside the link's
// lock but on its round's goroutine, so it must not block for long.
type Notifier interface {
	Notify(link string, h Health, hint string)
}

// health returns the link's health now, and whether it differs from what was
// last told. The caller holds l.mu.
func (l *Link) health(now time.Time) (Health, bool) {
	h := Healthy
	if l.st.NeedsHuman {
		h = NeedsHuman
	} else {
		for _, d := range l.dirs {
			if dh := l.dirHealth(d, now); healthRank[dh] > healthRank[h] {
				h = dh
			}
		}
		if l.saveFailures >= stateFailures && healthRank[Degraded] > healthRank[h] {
			h = Degraded
		}
	}
	if h == l.notified {
		return h, false
	}
	l.notified = h
	return h, true
}

func (l *Link) dirHealth(d *direction, now time.Time) Health {
	switch round.Class(d.st.Class) {
	case round.Auth:
		return Auth
	case round.NeedsHuman:
		return NeedsHuman
	case round.Offline:
		// Counted from the last contact, not the last delivery: a quiet link
		// that then fails still gets the whole grace period. Nor from before
		// this process started or the machine last woke, so neither a restart
		// nor a night's sleep reports the first failure after it at once.
		since := d.st.LastContact
		if since.Before(l.awake) {
			since = l.awake
		}
		if now.Sub(since) >= OfflineGrace {
			return Offline
		}
	}
	if d.degraded() {
		return Degraded
	}
	return Healthy
}
