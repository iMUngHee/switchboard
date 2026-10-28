// Package testenv builds the binaries switchboard's tests run against: the
// real pager at the version switchboard supports, and testpager, a fake pager
// for injecting faults. Each is built once per machine and reused.
package testenv

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// PagerVersion is the pager the tests prove switchboard against: the release
// that ships the relay contract and none of the additions planned since.
const PagerVersion = "ad6a9f4cd854ce15e64e641615de456b2007be81"

var (
	pagerOnce sync.Once
	pagerPath string
	pagerErr  error

	fakeOnce sync.Once
	fakePath string
	fakeErr  error
)

func exe(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

// Pager returns the path of a real pager binary. SWITCHBOARD_TEST_PAGER names
// a prebuilt one; otherwise PagerVersion is installed into a cache directory.
func Pager(t testing.TB) string {
	t.Helper()
	if p := os.Getenv("SWITCHBOARD_TEST_PAGER"); p != "" {
		return p
	}
	pagerOnce.Do(func() {
		// A per-user cache: a binary found in a shared temp directory could
		// have been put there by someone else.
		cache, err := os.UserCacheDir()
		if err != nil {
			pagerErr = err
			return
		}
		dir := filepath.Join(cache, "switchboard-test", "pager-"+PagerVersion[:12])
		pagerPath = filepath.Join(dir, exe("pager"))
		if _, err := os.Stat(pagerPath); err == nil {
			return
		}
		cmd := exec.Command("go", "install", "github.com/iMUngHee/pager/cmd/pager@"+PagerVersion)
		cmd.Env = append(os.Environ(), "GOBIN="+dir, "CGO_ENABLED=0", "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			pagerErr = &buildError{what: "pager " + PagerVersion[:12], err: err, out: out}
		}
	})
	if pagerErr != nil {
		t.Fatal(pagerErr)
	}
	return pagerPath
}

// TestPager returns the path of the fake pager built from internal/testpager.
func TestPager(t testing.TB) string {
	t.Helper()
	fakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "switchboard-testpager-")
		if err != nil {
			fakeErr = err
			return
		}
		fakePath = filepath.Join(dir, exe("testpager"))
		cmd := exec.Command("go", "build", "-o", fakePath, "github.com/iMUngHee/switchboard/internal/testpager")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			fakeErr = &buildError{what: "testpager", err: err, out: out}
		}
	})
	if fakeErr != nil {
		t.Fatal(fakeErr)
	}
	return fakePath
}

type buildError struct {
	what string
	err  error
	out  []byte
}

func (e *buildError) Error() string {
	return "build " + e.what + ": " + e.err.Error() + "\n" + string(e.out)
}
