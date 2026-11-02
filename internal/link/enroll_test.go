package link

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/pagercli"
	"github.com/iMUngHee/switchboard/internal/testenv"
	"github.com/iMUngHee/switchboard/internal/transport"
)

// unpaired is two real pager stores that know nothing of each other: this
// store at a, reached locally, and a far one at b behind an exec endpoint.
type unpaired struct {
	pager, dir, a, b string
	local, far       transport.Endpoint
}

func newUnpaired(t *testing.T) unpaired {
	t.Helper()
	t.Setenv("PAGER_WAKE", "off")
	t.Setenv("PAGER_CLIENT", "none")
	u := unpaired{pager: testenv.Pager(t), dir: t.TempDir()}
	u.a, u.b = filepath.Join(u.dir, "a.db"), filepath.Join(u.dir, "b.db")
	u.local = transport.LocalEndpoint{Pager: u.pager, DB: u.a}
	u.far = transport.ExecEndpoint{Argv: []string{u.pager}, Env: map[string]string{"PAGER_DB": u.b}}
	return u
}

func (u unpaired) run(t *testing.T, db string, args ...string) string {
	t.Helper()
	cmd := exec.Command(u.pager, args...)
	cmd.Env = append(os.Environ(), "PAGER_DB="+db)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pager %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func (u unpaired) id(t *testing.T, db string) string {
	t.Helper()
	return strings.TrimSpace(u.run(t, db, "relay", "id"))
}

// peers is the store each peer name of db is enrolled as.
func (u unpaired) peers(t *testing.T, db string) map[string]string {
	t.Helper()
	list, err := pagercli.ParsePeerList([]byte(u.run(t, db, "peer", "ls")))
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]string{}
	for _, p := range list {
		m[p.Name] = p.Store
	}
	return m
}

func (u unpaired) enroll(t *testing.T) EnrollResult {
	t.Helper()
	return Enroll(t.Context(), pagercli.Runner{Timeout: time.Minute}, u.local, u.far, "vm", "laptop")
}

// TestEnroll: enroll pairs two stores, finishes a half-done pairing, changes
// nothing when run again, and writes nothing where it would have to
// overwrite, guess, or could not read both sides.
func TestEnroll(t *testing.T) {
	paired := func(t *testing.T, u unpaired) {
		t.Helper()
		if a, b := u.peers(t, u.a), u.peers(t, u.b); len(a) != 1 || a["vm"] != u.id(t, u.b) || len(b) != 1 || b["laptop"] != u.id(t, u.a) {
			t.Fatalf("not paired: this store has %v, the far store has %v", a, b)
		}
	}

	t.Run("fresh stores", func(t *testing.T) {
		u := newUnpaired(t)
		e := u.enroll(t)
		if !e.OK() || e.Here != Added || e.There != Added {
			t.Fatalf("enroll: %+v", e)
		}
		if e.LocalStore != u.id(t, u.a) || e.RemoteStore != u.id(t, u.b) {
			t.Fatalf("stores %s and %s, want this and the far one", e.LocalStore, e.RemoteStore)
		}
		paired(t, u)
	})

	t.Run("half done here, or there", func(t *testing.T) {
		u := newUnpaired(t)
		u.run(t, u.a, "peer", "add", "vm", "--store", u.id(t, u.b))
		if e := u.enroll(t); !e.OK() || e.Here != Already || e.There != Added {
			t.Fatalf("enroll with this store done: %+v", e)
		}
		paired(t, u)

		v := newUnpaired(t)
		v.run(t, v.b, "peer", "add", "laptop", "--store", v.id(t, v.a))
		if e := v.enroll(t); !e.OK() || e.Here != Added || e.There != Already {
			t.Fatalf("enroll with the far store done: %+v", e)
		}
		paired(t, v)
	})

	t.Run("run again", func(t *testing.T) {
		u := newUnpaired(t)
		u.enroll(t)
		if e := u.enroll(t); !e.OK() || e.Here != Already || e.There != Already {
			t.Fatalf("second enroll: %+v", e)
		}
		paired(t, u)
	})

	t.Run("a name enrolled as another store is not overwritten", func(t *testing.T) {
		for _, side := range []string{"here", "there"} {
			u := newUnpaired(t)
			other := u.id(t, filepath.Join(u.dir, "other.db"))
			db, name, want := u.a, "vm", "`pager peer rebind vm --store "+u.id(t, u.b)+"`"
			if side == "there" {
				db, name, want = u.b, "laptop", "`pager peer rebind laptop --store "+u.id(t, u.a)+"` there"
			}
			u.run(t, db, "peer", "add", name, "--store", other)
			e := u.enroll(t)
			if e.OK() || e.Here != "" || e.There != "" || len(e.Problems) != 1 || !strings.Contains(e.Problems[0], want) {
				t.Fatalf("%s: enroll over a name bound to another store: %+v, want one problem naming %s", side, e, want)
			}
			a, b := u.peers(t, u.a), u.peers(t, u.b)
			if side == "here" && (len(a) != 1 || a["vm"] != other || len(b) != 0) ||
				side == "there" && (len(b) != 1 || b["laptop"] != other || len(a) != 0) {
				t.Fatalf("%s: enroll wrote something: this store has %v, the far store has %v", side, a, b)
			}
		}
	})

	t.Run("the same store at both ends", func(t *testing.T) {
		u := newUnpaired(t)
		u.far = transport.ExecEndpoint{Argv: []string{u.pager}, Env: map[string]string{"PAGER_DB": u.a}}
		e := u.enroll(t)
		if e.OK() || !strings.Contains(strings.Join(e.Problems, "\n"), "both ends of the link are store") {
			t.Fatalf("enroll to itself: %+v", e)
		}
		if p := u.peers(t, u.a); len(p) != 0 {
			t.Fatalf("enroll to itself wrote %v", p)
		}
	})

	// A far side that reads fine but whose peer add fails, or claims success
	// without enrolling: this store's half is done, the result says to run
	// enroll again, and the closing check catches the false success.
	fakeFar := func(t *testing.T, add map[string]any, relayID ...map[string]any) transport.Endpoint {
		t.Helper()
		id := map[string]any{"stdout": strings.Repeat("f", 32) + "\n", "skip_stdin": true}
		if len(relayID) > 0 {
			id = relayID[0]
		}
		plan := filepath.Join(t.TempDir(), "plan.json")
		raw, _ := json.Marshal(map[string]any{
			"relay id": id,
			"peer ls":  map[string]any{"stdout": "no peers\n", "skip_stdin": true},
			"peer add": add,
		})
		if err := os.WriteFile(plan, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return transport.ExecEndpoint{Argv: []string{testenv.TestPager(t)}, Env: map[string]string{"TESTPAGER_PLAN": plan}}
	}
	t.Run("the far side fails to add", func(t *testing.T) {
		u := newUnpaired(t)
		u.far = fakeFar(t, map[string]any{"exit": 1, "stderr": "refused\n", "skip_stdin": true})
		e := u.enroll(t)
		if e.OK() || e.Here != Added || e.There != "" || len(e.Problems) != 1 || !strings.Contains(e.Problems[0], "run `switchboard enroll` again") {
			t.Fatalf("enroll with a failing far add: %+v", e)
		}
		if p := u.peers(t, u.a); p["vm"] != strings.Repeat("f", 32) {
			t.Fatalf("this store's half is not done: %v", p)
		}
	})
	t.Run("this store fails to add", func(t *testing.T) {
		// This store is enrolled first, and its failure stops enroll before
		// the far store is touched.
		u := newUnpaired(t)
		u.local = fakeFar(t, map[string]any{"exit": 1, "stderr": "refused\n", "skip_stdin": true})
		e := u.enroll(t)
		if e.OK() || e.Here != "" || e.There != "" || len(e.Problems) != 1 ||
			!strings.HasPrefix(e.Problems[0], "this store: peer add: ") || !strings.Contains(e.Problems[0], "run `switchboard enroll` again") {
			t.Fatalf("enroll with a failing add here: %+v", e)
		}
		if p := u.peers(t, u.b); len(p) != 0 {
			t.Fatalf("the far store was written after this store failed: %v", p)
		}
	})
	t.Run("a far add that enrolls nothing is caught", func(t *testing.T) {
		u := newUnpaired(t)
		u.far = fakeFar(t, map[string]any{"skip_stdin": true})
		e := u.enroll(t)
		if e.OK() || e.There != Added || !strings.Contains(strings.Join(e.Problems, "\n"), `the far store has no peer "laptop"`) {
			t.Fatalf("enroll after a far add that did nothing: %+v", e)
		}
	})

	t.Run("a far side that gives no id", func(t *testing.T) {
		// Its peer ls works, so only stopping at relay id keeps enroll from
		// going on without the far store's id.
		u := newUnpaired(t)
		u.far = fakeFar(t, map[string]any{"skip_stdin": true}, map[string]any{"exit": 1, "skip_stdin": true})
		e := u.enroll(t)
		if e.OK() || e.Here != "" || len(e.Problems) != 1 || e.Problems[0] != "the far side: pager exited 1" {
			t.Fatalf("enroll with a far relay id that fails: %+v", e)
		}
		if p := u.peers(t, u.a); len(p) != 0 {
			t.Fatalf("enroll with no far id wrote %v here", p)
		}
	})

	t.Run("an unreachable far side", func(t *testing.T) {
		u := newUnpaired(t)
		u.far = transport.ExecEndpoint{Argv: []string{filepath.Join(u.dir, "no-pager-here")}}
		e := u.enroll(t)
		if e.OK() || e.Here != "" || !strings.HasPrefix(strings.Join(e.Problems, "\n"), "the far side: ") {
			t.Fatalf("enroll with no far side: %+v", e)
		}
		if p := u.peers(t, u.a); len(p) != 0 {
			t.Fatalf("enroll with no far side wrote %v here", p)
		}
	})
}
