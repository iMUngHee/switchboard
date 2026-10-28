package link

import (
	"context"
	"fmt"

	"github.com/iMUngHee/switchboard/internal/diag"
	"github.com/iMUngHee/switchboard/internal/pagercli"
	"github.com/iMUngHee/switchboard/internal/transport"
)

// CheckResult is what Check found. Problems are sentences for a person; each
// names only store ids, peer names and fixed text.
type CheckResult struct {
	LocalStore, RemoteStore string
	Problems                []string
}

// OK reports whether the link is enrolled correctly both ways.
func (c CheckResult) OK() bool { return len(c.Problems) == 0 }

// Check verifies that both stores are enrolled with each other: the far
// store's id is the one this store enrolled under peer, and the far store
// enrolled this one under remotePeer. Run before trusting a link, it also
// catches a far side whose shell prints a banner, which would corrupt every
// relay stream.
func Check(ctx context.Context, r pagercli.Runner, local, far transport.Endpoint, peer, remotePeer string) CheckResult {
	var c CheckResult
	fail := func(format string, a ...any) { c.Problems = append(c.Problems, fmt.Sprintf(format, a...)) }

	localID, why := checkRelayID(ctx, r, local)
	if why != "" {
		fail("this store: %s", why)
		return c
	}
	c.LocalStore = localID
	enrolled, why := checkPeer(ctx, r, local, peer)
	switch {
	case why != "":
		fail("this store: %s", why)
	case enrolled == "":
		fail("this store has no peer %q: run `pager peer add %s --store <the far store's relay id>`", peer, peer)
	}

	farID, why := checkRelayID(ctx, r, far)
	if why != "" {
		fail("the far side: %s", why)
		return c
	}
	c.RemoteStore = farID
	if enrolled != "" && enrolled != farID {
		fail("peer %q is enrolled here as store %s, but the far side is store %s: the link reaches the wrong store, or the far store was recreated (`pager peer rebind %s --store %s`)", peer, enrolled, farID, peer, farID)
	}
	back, why := checkPeer(ctx, r, far, remotePeer)
	switch {
	case why != "":
		fail("the far side: %s", why)
	case back == "":
		fail("the far store has no peer %q: run `pager peer add %s --store %s` there", remotePeer, remotePeer, localID)
	case back != localID:
		fail("the far store enrolled %q as store %s, but this store is %s", remotePeer, back, localID)
	}
	return c
}

// checkRelayID runs relay id. why is a sentence when it did not give an id.
func checkRelayID(ctx context.Context, r pagercli.Runner, ep transport.Endpoint) (id, why string) {
	out, err := r.Run(ctx, ep, nil, "relay", "id")
	if why := stageProblem(ep, out, err); why != "" {
		return "", why
	}
	id, err = pagercli.ParseRelayID(out.Stdout)
	if err != nil {
		return "", "relay id printed something besides a store id; a login banner or shell profile writing to stdout would corrupt every relay stream"
	}
	return id, ""
}

// checkPeer returns the store id a store enrolled under name, or "" when it
// has no such peer.
func checkPeer(ctx context.Context, r pagercli.Runner, ep transport.Endpoint, name string) (store, why string) {
	out, err := r.Run(ctx, ep, nil, "peer", "ls")
	if why := stageProblem(ep, out, err); why != "" {
		return "", why
	}
	peers, err := pagercli.ParsePeerList(out.Stdout)
	if err != nil {
		return "", "peer ls printed something other than its table"
	}
	for _, p := range peers {
		if p.Name == name {
			return p.Store, ""
		}
	}
	return "", ""
}

// stageProblem describes a stage that did not run cleanly, in fixed words.
func stageProblem(ep transport.Endpoint, out pagercli.Output, err error) string {
	res := out.Result
	switch {
	case err != nil:
		return "the endpoint is not usable: " + err.Error()
	case !res.Started:
		return "could not start the command"
	case res.TimedOut:
		return "timed out"
	case res.ExitCode == 0:
		return ""
	case res.ExitCode == 255 && ep.Kind() != transport.Local:
		if code := diag.SSH(out.StderrHead); code != "" {
			return fmt.Sprintf("ssh failed (%s)", code)
		}
		return "ssh failed"
	case (res.ExitCode == 126 || res.ExitCode == 127) && ep.Kind() != transport.Local:
		return "could not execute pager there; check the pager path"
	}
	if code := diag.Pager(out.StderrHead); code != "" {
		return fmt.Sprintf("pager exited %d (%s)", res.ExitCode, code)
	}
	return fmt.Sprintf("pager exited %d", res.ExitCode)
}
