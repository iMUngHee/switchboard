package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iMUngHee/switchboard/internal/transport"
)

// Stores is two real pager stores on this machine, enrolled with each other:
// "laptop" (A, holding session s-a named sape) and "vm" (B, holding session
// s-b named kipa). A peer name is what each store calls the other: A knows B
// as vm, B knows A as laptop. B stands in for a far side and is reached
// through an exec endpoint.
type Stores struct {
	Pager string
	A, B  string // database paths
	Local transport.LocalEndpoint
	Far   transport.ExecEndpoint
}

// NewStores makes the two stores in a temporary directory. Nothing they do
// may wake a real session on this machine, so wakes are off for the test.
func NewStores(t *testing.T) *Stores {
	t.Helper()
	t.Setenv("PAGER_WAKE", "off")
	t.Setenv("PAGER_CLIENT", "none")
	dir := t.TempDir()
	s := &Stores{Pager: Pager(t), A: filepath.Join(dir, "a.db"), B: filepath.Join(dir, "b.db")}
	s.Local = transport.LocalEndpoint{Pager: s.Pager, DB: s.A}
	s.Far = transport.ExecEndpoint{Argv: []string{s.Pager}, Env: map[string]string{"PAGER_DB": s.B}}
	s.Run(t, s.A, "attach", "--session", "s-a", "--tool", "claude", "--root", dir)
	s.Run(t, s.A, "alias", "sape", "--session", "s-a")
	s.Run(t, s.B, "attach", "--session", "s-b", "--tool", "claude", "--root", dir)
	s.Run(t, s.B, "alias", "kipa", "--session", "s-b")
	idA := strings.TrimSpace(string(s.Run(t, s.A, "relay", "id")))
	idB := strings.TrimSpace(string(s.Run(t, s.B, "relay", "id")))
	s.Run(t, s.A, "peer", "add", "vm", "--store", idB)
	s.Run(t, s.B, "peer", "add", "laptop", "--store", idA)
	return s
}

// Run runs pager on one store directly and returns its combined output,
// failing the test if pager fails.
func (s *Stores) Run(t *testing.T, db string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(s.Pager, args...)
	cmd.Env = append(os.Environ(), "PAGER_DB="+db)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pager %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return out
}
