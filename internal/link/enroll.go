package link

import (
	"context"
	"fmt"

	"github.com/iMUngHee/switchboard/internal/pagercli"
	"github.com/iMUngHee/switchboard/internal/transport"
)

// What Enroll did on one side.
const (
	Added   = "added"   // it enrolled the other store there
	Already = "already" // the other store was enrolled there before
)

// EnrollResult is what Enroll did. Here and There say, for this store and the
// far store, Added, Already, or "" when Enroll stopped before that side.
// Problems are, as in CheckResult, sentences for a person.
type EnrollResult struct {
	LocalStore, RemoteStore string
	Here, There             string
	Problems                []string
}

// OK reports whether the link ended up enrolled correctly both ways.
func (e EnrollResult) OK() bool { return len(e.Problems) == 0 }

// Enroll pairs a link's two stores: each enrolls the other under the name the
// configuration gives it, unless it already has. It writes nothing when either
// side cannot be read, when both ends are the same store, or when a name is
// enrolled as another store. That last is a person's call (pager peer rebind)
// and is never overwritten: enroll cannot tell a recreated store from a link
// that reaches the wrong machine. This store is enrolled first, so a far side
// that fails leaves the near half done, and running Enroll again finishes it.
// It ends with Check.
func Enroll(ctx context.Context, r pagercli.Runner, local, far transport.Endpoint, peer, remotePeer string) EnrollResult {
	var e EnrollResult
	fail := func(format string, a ...any) { e.Problems = append(e.Problems, fmt.Sprintf(format, a...)) }

	// Read everything first.
	localID, why := checkRelayID(ctx, r, local)
	if why != "" {
		fail("this store: %s", why)
		return e
	}
	farID, why := checkRelayID(ctx, r, far)
	if why != "" {
		fail("the far side: %s", why)
		return e
	}
	e.LocalStore, e.RemoteStore = localID, farID
	if localID == farID {
		fail("both ends of the link are store %s: the far side reaches this store's own database", localID)
		return e
	}
	here, why := checkPeer(ctx, r, local, peer)
	if why != "" {
		fail("this store: %s", why)
		return e
	}
	there, why := checkPeer(ctx, r, far, remotePeer)
	if why != "" {
		fail("the far side: %s", why)
		return e
	}

	// Then decide, before the first write.
	if here != "" && here != farID {
		fail("this store already enrolled %q as store %s, but the far side is store %s: enroll does not change it; if the far store was recreated, run `pager peer rebind %s --store %s`", peer, here, farID, peer, farID)
	}
	if there != "" && there != localID {
		fail("the far store already enrolled %q as store %s, but this store is %s: enroll does not change it; if this store was recreated, run `pager peer rebind %s --store %s` there", remotePeer, there, localID, remotePeer, localID)
	}
	if !e.OK() {
		return e
	}

	// Then write what is missing, this store first.
	e.Here, e.There = Already, Already
	if here == "" {
		if why := addPeer(ctx, r, local, peer, farID); why != "" {
			e.Here, e.There = "", ""
			fail("this store: peer add: %s; nothing was changed on the far side: fix that, then run `switchboard enroll` again", why)
			return e
		}
		e.Here = Added
	}
	if there == "" {
		if why := addPeer(ctx, r, far, remotePeer, localID); why != "" {
			e.There = ""
			fail("the far side: peer add: %s; run `switchboard enroll` again to finish", why)
			return e
		}
		e.There = Added
	}

	c := Check(ctx, r, local, far, peer, remotePeer)
	e.Problems = append(e.Problems, c.Problems...)
	return e
}

// addPeer runs peer add on ep. why is a sentence when it did not succeed.
func addPeer(ctx context.Context, r pagercli.Runner, ep transport.Endpoint, name, store string) (why string) {
	out, err := r.Run(ctx, ep, nil, "peer", "add", name, "--store", store)
	return stageProblem(ep, out, err)
}
