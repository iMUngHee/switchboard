// Package testenv builds the binaries switchboard's tests run against: the
// real pager at the version switchboard supports, and testpager, a fake pager
// for injecting faults. Each is built once per machine and reused.
package testenv

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

// PagerVersion is the pager the tests prove switchboard against: the commit
// that adds relay wait to the relay contract. switchboard still works with a
// pager from before it, by polling; the link tests cover that with testpager.
const PagerVersion = "65515fe8c830d16be89d1c80f34518032ebccbe8"

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
// It is kept in the per-user cache under a hash of its source, so every test
// process reuses one build until the source changes.
func TestPager(t testing.TB) string {
	t.Helper()
	fakeOnce.Do(func() {
		_, here, _, _ := runtime.Caller(0)
		src, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "testpager", "main.go"))
		if err != nil {
			fakeErr = err
			return
		}
		cache, err := os.UserCacheDir()
		if err != nil {
			fakeErr = err
			return
		}
		sum := sha256.Sum256(src)
		dir := filepath.Join(cache, "switchboard-test", "testpager-"+hex.EncodeToString(sum[:6]))
		fakePath = filepath.Join(dir, exe("testpager"))
		if _, err := os.Stat(fakePath); err == nil {
			return
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			fakeErr = err
			return
		}
		// Built under a name of its own, then renamed: test processes for
		// several packages may build it at once.
		tmp := filepath.Join(dir, fmt.Sprintf("build-%d-%s", os.Getpid(), exe("testpager")))
		cmd := exec.Command("go", "build", "-o", tmp, "github.com/iMUngHee/switchboard/internal/testpager")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			fakeErr = &buildError{what: "testpager", err: err, out: out}
			return
		}
		if err := os.Rename(tmp, fakePath); err != nil {
			if _, statErr := os.Stat(fakePath); statErr != nil {
				fakeErr = err
			}
			os.Remove(tmp)
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
