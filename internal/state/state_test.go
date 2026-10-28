package state

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

const storeID = "0123456789abcdef0123456789abcdef"

// pending are result lines as relay in writes them, kept verbatim.
var pending = []string{
	`{"v":1,"kind":"result","store":"fedcba9876543210fedcba9876543210","max_v":1,"link":{"store":"0123456789abcdef0123456789abcdef","id":"a1","seq":7},"origin_id":41,"status":"imported"}`,
	`{"v":1,"kind":"result","store":"fedcba9876543210fedcba9876543210","max_v":1,"link":{"store":"0123456789abcdef0123456789abcdef","id":"a1","seq":8},"origin_id":42,"status":"rejected","reason":"no such alias <x>; live names here: kipa"}`,
}

func at(min int) time.Time { return time.Date(2026, 10, 28, 12, min, 0, 0, time.UTC) }

func mustLock(t *testing.T, dir, key string) *Lock {
	t.Helper()
	l, err := TryLock(dir, key)
	if err != nil {
		t.Fatalf("TryLock: %v", err)
	}
	return l
}

func mustSave(t *testing.T, dir, key string, l Link) {
	t.Helper()
	lock := mustLock(t, dir, key)
	defer lock.Close()
	if err := Save(dir, key, l); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func mustLoad(t *testing.T, dir, key string) Link {
	t.Helper()
	l, err := Load(dir, key)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return l
}

// TestPendingAckSurvivesRestart: results saved for ack come back verbatim from
// a fresh Load, and a crash between writing the temp file and renaming it
// over leaves the previous state whole, with the temp file never read as
// state.
func TestPendingAckSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	key := Key(storeID, "vm")

	if got := mustLoad(t, dir, key); !reflect.DeepEqual(got, Link{}) {
		t.Fatalf("Load before any Save = %+v, want the zero Link", got)
	}

	before := Link{
		Outbound: Direction{
			Class: "rerun", Code: "cut",
			LastSuccess: at(1), LastContact: at(2), NotBefore: at(3),
			ConsecutiveFailures: 2,
			PendingAck:          pending,
		},
		Inbound:         Direction{Class: "idle", LastContact: at(2)},
		LastLocalStore:  storeID,
		LastRemoteStore: "fedcba9876543210fedcba9876543210",
		NeedsHuman:      true,
	}
	mustSave(t, dir, key, before)
	if got := mustLoad(t, dir, key); !reflect.DeepEqual(got, before) {
		t.Fatalf("after a restart:\n got %+v\nwant %+v", got, before)
	}
	data, err := os.ReadFile(filepath.Join(dir, key+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var onDisk struct{ Version int }
	if err := json.Unmarshal(data, &onDisk); err != nil || onDisk.Version != 1 {
		t.Errorf("state file version = %d (%v), want 1:\n%s", onDisk.Version, err, data)
	}

	// The next state, cut off by a crash after its temp file is synced but
	// before the rename.
	after := before
	after.Outbound.PendingAck = nil
	after.NeedsHuman = false
	crash := errors.New("crash")
	lock := mustLock(t, dir, key)
	if err := save(dir, key, after, func() error { return crash }); !errors.Is(err, crash) {
		t.Fatalf("save with a crash before the rename: %v", err)
	}
	lock.Close()
	if got := mustLoad(t, dir, key); !reflect.DeepEqual(got, before) {
		t.Fatalf("after the crash:\n got %+v\nwant the state before it, pending results included: %+v", got, before)
	}
	// The temp file is there, and was not read as state.
	if temps, _ := filepath.Glob(filepath.Join(dir, key+".json.tmp-*")); len(temps) != 1 {
		t.Fatalf("temp files left by the crash: %q, want one", temps)
	}

	// The leftover temp file does not get in the way of the next Save.
	mustSave(t, dir, key, after)
	if got := mustLoad(t, dir, key); !reflect.DeepEqual(got, after) {
		t.Errorf("after the next Save:\n got %+v\nwant %+v", got, after)
	}
}

// TestControlWithoutDaemon: with no daemon, resume takes the link's lock,
// re-reads the state there, clears needs-human and backoff and saves. While
// another holder has the lock it gets ErrLocked instead and leaves a request
// for that holder. Pending results survive both paths.
func TestControlWithoutDaemon(t *testing.T) {
	dir := t.TempDir()
	key := Key(storeID, "vm")
	stuck := Link{
		Outbound: Direction{
			Class: "needs-human", Code: "refused",
			LastContact: at(5), NotBefore: at(50), ConsecutiveFailures: 4,
			PendingAck: pending,
		},
		Inbound:    Direction{Class: "retry", NotBefore: at(40), ConsecutiveFailures: 1},
		NeedsHuman: true,
	}
	resumed := stuck
	resumed.NeedsHuman = false
	resumed.Outbound.NotBefore, resumed.Outbound.ConsecutiveFailures = time.Time{}, 0
	resumed.Inbound.NotBefore, resumed.Inbound.ConsecutiveFailures = time.Time{}, 0

	t.Run("no holder: resume applies it", func(t *testing.T) {
		mustSave(t, dir, key, stuck)
		queued, err := Resume(dir, key)
		if err != nil || queued {
			t.Fatalf("Resume = queued %v, %v; want it applied in place", queued, err)
		}
		if got := mustLoad(t, dir, key); !reflect.DeepEqual(got, resumed) {
			t.Errorf("after resume:\n got %+v\nwant %+v", got, resumed)
		}
		// It let the lock go and left no request behind.
		mustLock(t, dir, key).Close()
		if kinds, err := Take(dir, key); err != nil || len(kinds) != 0 {
			t.Errorf("Take after an applied resume = %q, %v; want nothing", kinds, err)
		}
	})

	t.Run("held: resume leaves a request", func(t *testing.T) {
		mustSave(t, dir, key, stuck)
		holder := mustLock(t, dir, key) // a second handle, standing in for a running daemon
		defer holder.Close()

		if _, err := TryLock(dir, key); !errors.Is(err, ErrLocked) {
			t.Fatalf("TryLock while held: %v, want ErrLocked", err)
		}
		queued, err := Resume(dir, key)
		if err != nil || !queued {
			t.Fatalf("Resume while held = queued %v, %v; want queued", queued, err)
		}
		if got := mustLoad(t, dir, key); !reflect.DeepEqual(got, stuck) {
			t.Errorf("resume changed the state under the holder:\n got %+v\nwant %+v", got, stuck)
		}

		// The holder takes the request once, and applies it as a daemon would.
		kinds, err := Take(dir, key)
		if err != nil || !reflect.DeepEqual(kinds, []string{KindResume}) {
			t.Fatalf("Take = %q, %v; want [resume]", kinds, err)
		}
		if again, _ := Take(dir, key); len(again) != 0 {
			t.Errorf("a second Take = %q, want nothing", again)
		}
		l := mustLoad(t, dir, key)
		l.Resume()
		if err := Save(dir, key, l); err != nil {
			t.Fatal(err)
		}
		if got := mustLoad(t, dir, key); !reflect.DeepEqual(got, resumed) {
			t.Errorf("after the holder applied it:\n got %+v\nwant %+v", got, resumed)
		}
	})

	t.Run("requests queue and coalesce", func(t *testing.T) {
		for _, kind := range []string{KindOnce, KindResume, KindOnce} {
			if err := Request(dir, key, kind); err != nil {
				t.Fatalf("Request(%s): %v", kind, err)
			}
		}
		if kinds, err := Take(dir, key); err != nil || !reflect.DeepEqual(kinds, []string{KindResume, KindOnce}) {
			t.Errorf("Take = %q, %v; want [resume once]", kinds, err)
		}
		if err := Request(dir, key, "stop"); err == nil {
			t.Error("Request accepted an unknown kind")
		}
	})
}

// TestLockHolderProcess is a helper, not a test: TestLockIsOneProcessAndDiesWithIt
// runs this binary as a child that takes the lock, says so, and waits.
func TestLockHolderProcess(t *testing.T) {
	dir := os.Getenv("STATE_TEST_LOCK_DIR")
	if dir == "" {
		return
	}
	if _, err := TryLock(dir, os.Getenv("STATE_TEST_LOCK_KEY")); err != nil {
		os.Stdout.WriteString("lock failed: " + err.Error() + "\n")
		os.Exit(1)
	}
	os.Stdout.WriteString("holding\n")
	bufio.NewReader(os.Stdin).ReadString('\n') // until killed, or the parent goes
	os.Exit(0)
}

// TestLockIsOneProcessAndDiesWithIt: the lock is an OS lock — a second process
// cannot take it — and the OS frees it when its holder is killed, with no
// Close run.
func TestLockIsOneProcessAndDiesWithIt(t *testing.T) {
	dir := t.TempDir()
	key := Key(storeID, "vm")
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockHolderProcess$")
	cmd.Env = append(os.Environ(), "STATE_TEST_LOCK_DIR="+dir, "STATE_TEST_LOCK_KEY="+key)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	if line, err := bufio.NewReader(stdout).ReadString('\n'); strings.TrimSpace(line) != "holding" {
		t.Fatalf("child said %q (%v), want holding", line, err)
	}

	if _, err := TryLock(dir, key); !errors.Is(err, ErrLocked) {
		t.Fatalf("TryLock while another process holds it: %v, want ErrLocked", err)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	cmd.Wait()
	// Windows frees a dead process's locks soon after, not at once.
	deadline := time.Now().Add(5 * time.Second)
	for {
		l, err := TryLock(dir, key)
		if err == nil {
			l.Close()
			return
		}
		if !errors.Is(err, ErrLocked) || time.Now().After(deadline) {
			t.Fatalf("TryLock after the holder died: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestKeysCannotLeaveTheStateDir: a key is a file name, so one that could
// reach outside the state directory is refused everywhere.
func TestKeysCannotLeaveTheStateDir(t *testing.T) {
	dir := t.TempDir()
	for _, key := range []string{"", "../x", "a/b", `a\b`, ".hidden"} {
		if _, err := Load(dir, key); err == nil {
			t.Errorf("Load(%q) accepted it", key)
		}
		if err := Save(dir, key, Link{}); err == nil {
			t.Errorf("Save(%q) accepted it", key)
		}
		if l, err := TryLock(dir, key); err == nil {
			l.Close()
			t.Errorf("TryLock(%q) accepted it", key)
		}
		if err := Request(dir, key, KindOnce); err == nil {
			t.Errorf("Request(%q) accepted it", key)
		}
	}
	if got := Key(storeID, "vm"); got != storeID+".vm" {
		t.Errorf("Key = %q", got)
	}
}
