package link

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/pagercli"
	"github.com/iMUngHee/switchboard/internal/testenv"
	"github.com/iMUngHee/switchboard/internal/transport"
)

// TestCheckCatchesBannerAndMismatch: check passes a correctly enrolled pair
// of real stores, and names each way one can be wrong.
func TestCheckCatchesBannerAndMismatch(t *testing.T) {
	r := pagercli.Runner{Timeout: time.Minute}
	s := testenv.NewStores(t)
	idA := strings.TrimSpace(string(s.Run(t, s.A, "relay", "id")))
	idB := strings.TrimSpace(string(s.Run(t, s.B, "relay", "id")))

	t.Run("a correct pair passes", func(t *testing.T) {
		c := Check(t.Context(), r, s.Local, s.Far, "vm", "laptop")
		if !c.OK() || c.LocalStore != idA || c.RemoteStore != idB {
			t.Fatalf("%+v", c)
		}
	})

	t.Run("a banner on the far side", func(t *testing.T) {
		far := testpager(t, map[string]any{"relay id": map[string]any{"stdout": "Welcome to vm.example.com\n" + idB + "\n"}})
		expectProblem(t, Check(t.Context(), r, s.Local, far, "vm", "laptop"), "login banner")
	})

	t.Run("the far side is another store", func(t *testing.T) {
		other := filepath.Join(t.TempDir(), "c.db")
		s.Run(t, other, "relay", "id") // creates it
		far := transport.ExecEndpoint{Argv: []string{s.Pager}, Env: map[string]string{"PAGER_DB": other}}
		expectProblem(t, Check(t.Context(), r, s.Local, far, "vm", "laptop"), "enrolled here as store "+idB)
	})

	t.Run("the far store never enrolled this one", func(t *testing.T) {
		expectProblem(t, Check(t.Context(), r, s.Local, s.Far, "vm", "desktop"), `the far store has no peer "desktop"`)
	})

	t.Run("this store never enrolled the far one", func(t *testing.T) {
		expectProblem(t, Check(t.Context(), r, s.Local, s.Far, "pi", "laptop"), `this store has no peer "pi"`)
	})

	t.Run("the far side cannot run pager", func(t *testing.T) {
		far := testpager(t, map[string]any{"relay id": map[string]any{"exit": 127}})
		expectProblem(t, Check(t.Context(), r, s.Local, far, "vm", "laptop"), "could not execute pager there")
	})
}

func expectProblem(t *testing.T, c CheckResult, want string) {
	t.Helper()
	for _, p := range c.Problems {
		if strings.Contains(p, want) {
			return
		}
	}
	t.Fatalf("no problem mentions %q: %q", want, c.Problems)
}

func testpager(t *testing.T, plan map[string]any) transport.ExecEndpoint {
	t.Helper()
	path := filepath.Join(t.TempDir(), "plan.json")
	raw, _ := json.Marshal(plan)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return transport.ExecEndpoint{Argv: []string{testenv.TestPager(t)}, Env: map[string]string{"TESTPAGER_PLAN": path}}
}
