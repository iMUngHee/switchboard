//go:build e2e

// Package e2e carries real mail both ways between two real pager stores
// through the round engine. CI runs it on every system with the far store
// behind an exec endpoint, and on Linux once more through ssh to a localhost
// sshd:
//
//	go test -tags e2e ./internal/e2e
//
// For the ssh run, SWITCHBOARD_E2E_SSH_PORT and SWITCHBOARD_E2E_SSH_KEY name
// the sshd and the client key, and SWITCHBOARD_E2E_FAR_DB the far store's
// database, which the sshd hands pager through SetEnv. Never point these at
// a real store: the test enrolls peers in it and sends mail.
package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/pagercli"
	"github.com/iMUngHee/switchboard/internal/round"
	"github.com/iMUngHee/switchboard/internal/testenv"
	"github.com/iMUngHee/switchboard/internal/transport"
)

func TestRoundTripBothWays(t *testing.T) {
	port, farDB := os.Getenv("SWITCHBOARD_E2E_SSH_PORT"), os.Getenv("SWITCHBOARD_E2E_FAR_DB")
	if (port == "") != (farDB == "") {
		t.Fatal("set both SWITCHBOARD_E2E_SSH_PORT and SWITCHBOARD_E2E_FAR_DB, or neither")
	}
	s := testenv.NewStoresAt(t, farDB)
	far := transport.Endpoint(s.Far)
	if port != "" {
		known := filepath.Join(t.TempDir(), "known_hosts")
		far = transport.SSHEndpoint{
			SSH:   "/usr/bin/ssh",
			Host:  "127.0.0.1",
			Pager: s.Pager,
			Options: []string{"-p", port, "-i", os.Getenv("SWITCHBOARD_E2E_SSH_KEY"),
				"-o", "StrictHostKeyChecking=accept-new", "-o", "UserKnownHostsFile=" + known},
		}
	}

	const toFar, toNear = "laptop to vm, once", "vm to laptop, once"
	s.Run(t, s.A, "send", "kipa@vm", toFar, "--session", "s-a")
	s.Run(t, s.B, "send", "sape@laptop", toNear, "--session", "s-b")
	if a, b := pending(t, s, s.A), pending(t, s, s.B); a != 1 || b != 1 {
		t.Fatalf("pending before: laptop %d, vm %d; want 1 each", a, b)
	}

	e := &round.Engine{Runner: pagercli.Runner{Timeout: time.Minute}}
	for _, d := range []round.Direction{
		{Link: "vm", Name: "outbound", From: round.Side{Endpoint: s.Local, Peer: "vm"}, To: round.Side{Endpoint: far, Peer: "laptop"}},
		{Link: "vm", Name: "inbound", From: round.Side{Endpoint: far, Peer: "laptop"}, To: round.Side{Endpoint: s.Local, Peer: "vm"}},
	} {
		o := e.Run(t.Context(), d, round.Input{})
		if o.Class != round.OK || o.Records != 1 || o.Finals != 1 || len(o.Pending) != 0 {
			t.Fatalf("%s: %+v", d.Name, o)
		}
		if _, isSSH := far.(transport.SSHEndpoint); isSSH && !o.Contact {
			t.Fatalf("%s: no stage reached the far pager", d.Name)
		}
	}

	if a, b := pending(t, s, s.A), pending(t, s, s.B); a != 0 || b != 0 {
		t.Fatalf("pending after: laptop %d, vm %d; want 0 each", a, b)
	}
	for _, c := range []struct{ db, body string }{{s.B, toFar}, {s.A, toNear}} {
		if n := strings.Count(string(s.Run(t, c.db, "export")), c.body); n != 1 {
			t.Fatalf("%s holds %q %d times, want once", filepath.Base(c.db), c.body, n)
		}
	}
}

func pending(t *testing.T, s *testenv.Stores, db string) int {
	t.Helper()
	peers, err := pagercli.ParsePeerList(s.Run(t, db, "peer", "ls"))
	if err != nil || len(peers) != 1 {
		t.Fatalf("peer ls: %v %+v", err, peers)
	}
	return peers[0].Pending
}
