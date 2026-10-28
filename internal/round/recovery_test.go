package round

import (
	"errors"
	"strings"
	"testing"

	"github.com/iMUngHee/switchboard/internal/pagercli"
	"github.com/iMUngHee/switchboard/internal/testenv"
)

// TestRecoveryBoundary: a round that dies before its results are stored is
// recovered by resend, one that dies after is recovered by acking the stored
// results first, and a failing store still acks from memory. Every case runs
// against two real pager stores, and the message arrives exactly once.
func TestRecoveryBoundary(t *testing.T) {
	const body = "recover me exactly once"

	setup := func(t *testing.T) (*testenv.Stores, Direction) {
		t.Helper()
		s := testenv.NewStores(t)
		s.Run(t, s.A, "send", "kipa@vm", body, "--session", "s-a")
		d := Direction{Link: "vm", Name: "outbound",
			From: Side{Endpoint: s.Local, Peer: "vm"},
			To:   Side{Endpoint: s.Far, Peer: "laptop"}}
		return s, d
	}
	pending := func(t *testing.T, s *testenv.Stores) int {
		t.Helper()
		peers, err := pagercli.ParsePeerList(s.Run(t, s.A, "peer", "ls"))
		if err != nil || len(peers) != 1 {
			t.Fatalf("peer ls: %v %+v", err, peers)
		}
		return peers[0].Pending
	}
	deliveredOnce := func(t *testing.T, s *testenv.Stores) {
		t.Helper()
		if n := strings.Count(string(s.Run(t, s.B, "export")), body); n != 1 {
			t.Fatalf("the far store holds the message %d times, want 1", n)
		}
	}

	t.Run("killed before the state write: recovered by resend", func(t *testing.T) {
		s, d := setup(t)
		h := newHarness()
		h.e.crash = func(point string) bool { return point == "before-save" }
		h.e.Run(t.Context(), d, Input{})
		if len(h.saves) != 0 || pending(t, s) != 1 {
			t.Fatalf("saves %d, pending %d: the crash came too late", len(h.saves), pending(t, s))
		}
		deliveredOnce(t, s) // relay in committed it before the crash

		o := newHarness().e.Run(t.Context(), d, Input{})
		if o.Class != OK || o.Finals != 1 || pending(t, s) != 0 {
			t.Fatalf("after the resend: %+v, pending %d", o, pending(t, s))
		}
		deliveredOnce(t, s) // answered duplicate, not imported again
	})

	t.Run("killed after the state write: the stored results are acked first", func(t *testing.T) {
		s, d := setup(t)
		h := newHarness()
		h.e.crash = func(point string) bool { return point == "after-save" }
		h.e.Run(t.Context(), d, Input{})
		if len(h.saves) != 1 || len(h.saves[0]) != 1 || pending(t, s) != 1 {
			t.Fatalf("saves %q, pending %d", h.saves, pending(t, s))
		}

		next := newHarness()
		o := next.e.Run(t.Context(), d, Input{Pending: h.saves[0]})
		if o.Class != Idle || len(o.Pending) != 0 || pending(t, s) != 0 {
			t.Fatalf("after restart: %+v, pending %d", o, pending(t, s))
		}
		log := next.logs.String()
		if ack, out := strings.Index(log, "stage=ack"), strings.Index(log, "stage=out"); ack < 0 || out < 0 || ack > out {
			t.Fatalf("want ack before out:\n%s", log)
		}
		deliveredOnce(t, s)
	})

	t.Run("a failing state write still acks from memory", func(t *testing.T) {
		s, d := setup(t)
		h := newHarness(func(e *Engine) { e.Save = func([]string) error { return errors.New("read-only file system") } })
		o := h.e.Run(t.Context(), d, Input{})
		if o.Class != OK || o.StateErr == nil || pending(t, s) != 0 {
			t.Fatalf("%+v, pending %d", o, pending(t, s))
		}
		deliveredOnce(t, s)
	})
}
