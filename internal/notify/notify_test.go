package notify

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/link"
)

// The test binary doubles as a notifier command: with NOTIFY_HELPER_OUT set
// it appends its arguments to that file, sleeps NOTIFY_HELPER_SLEEP_MS and
// exits NOTIFY_HELPER_EXIT.
func TestMain(m *testing.M) {
	if out := os.Getenv("NOTIFY_HELPER_OUT"); out != "" {
		f, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err == nil {
			json.NewEncoder(f).Encode(os.Args[1:])
			f.Close()
		}
		if ms, _ := strconv.Atoi(os.Getenv("NOTIFY_HELPER_SLEEP_MS")); ms > 0 {
			time.Sleep(time.Duration(ms) * time.Millisecond)
		}
		code, _ := strconv.Atoi(os.Getenv("NOTIFY_HELPER_EXIT"))
		os.Exit(code)
	}
	os.Exit(m.Run())
}

var _ link.Notifier = (*Notifier)(nil)

func helper(t *testing.T) (self, out string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	out = filepath.Join(t.TempDir(), "calls.jsonl")
	t.Setenv("NOTIFY_HELPER_OUT", out)
	return self, out
}

func calls(t *testing.T, out string) [][]string {
	t.Helper()
	f, err := os.Open(out)
	if err != nil {
		return nil
	}
	defer f.Close()
	var cs [][]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var c []string
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	return cs
}

// TestNotifyPayloadAndTransitions: every health a link reports becomes one
// notification, in order, carrying only the link, its state and the hint;
// a failing or hanging notifier is logged and does not stop later ones.
func TestNotifyPayloadAndTransitions(t *testing.T) {
	t.Run("the command gets the link, state and hint in place", func(t *testing.T) {
		self, out := helper(t)
		n := New([]string{self, "{link}", "state={state}", "{hint}", "{link}-{state}"}, nil)
		n.Notify("vm", link.Auth, "run `ssh vm.example.com` once")
		n.Close()
		want := []string{"vm", "state=auth", "run `ssh vm.example.com` once", "vm-auth"}
		if got := calls(t, out); len(got) != 1 || strings.Join(got[0], "|") != strings.Join(want, "|") {
			t.Fatalf("argv %q, want %q", got, want)
		}
	})

	t.Run("each transition is told once, in order", func(t *testing.T) {
		self, out := helper(t)
		n := New([]string{self, "{state}"}, nil)
		seq := []link.Health{link.Offline, link.Healthy, link.Degraded, link.Auth, link.NeedsHuman, link.Healthy}
		for _, h := range seq {
			n.Notify("vm", h, "hint")
		}
		n.Close()
		got := calls(t, out)
		if len(got) != len(seq) {
			t.Fatalf("%d calls, want %d: %q", len(got), len(seq), got)
		}
		for i, h := range seq {
			if got[i][0] != string(h) {
				t.Fatalf("call %d is %q, want %q", i, got[i][0], h)
			}
		}
	})

	t.Run("every health reads as a sentence; the hint only when there is something to do", func(t *testing.T) {
		for h, want := range map[link.Health]string{
			link.NeedsHuman: "switchboard resume vm",
			link.Auth:       "ssh needs a key loaded or a host key accepted; switchboard stopped trying until `switchboard resume vm`.",
			link.Offline:    "has not answered for 10 minutes",
			link.Degraded:   "still retrying",
			link.Healthy:    "works again",
		} {
			m := Message{Link: "vm", Health: h, Hint: "run `ssh vm.example.com` once"}
			if !strings.Contains(m.Body(), want) || m.Title() != "switchboard: vm" {
				t.Errorf("%s: %q / %q", h, m.Title(), m.Body())
			}
			if hasHint := strings.Contains(m.Body(), "ssh vm.example.com"); hasHint != (h != link.Healthy) {
				t.Errorf("%s: hint shown = %v", h, hasHint)
			}
		}
	})

	t.Run("a failing or hanging notifier is logged and does not stop the next", func(t *testing.T) {
		self, out := helper(t)
		var logs bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logs, nil))
		var mu sync.Mutex
		failNext, hangNext := true, false
		cmd := Command([]string{self, "{state}"})
		n := start(func(ctx context.Context, m Message) error {
			mu.Lock()
			switch {
			case failNext:
				failNext, hangNext = false, true
				os.Setenv("NOTIFY_HELPER_EXIT", "3")
			case hangNext:
				hangNext = false
				os.Setenv("NOTIFY_HELPER_EXIT", "0")
				os.Setenv("NOTIFY_HELPER_SLEEP_MS", "20000")
			default:
				os.Setenv("NOTIFY_HELPER_SLEEP_MS", "0")
			}
			mu.Unlock()
			return cmd(ctx, m)
		}, log)
		n.timeout = 500 * time.Millisecond
		t.Cleanup(func() { os.Unsetenv("NOTIFY_HELPER_EXIT"); os.Unsetenv("NOTIFY_HELPER_SLEEP_MS") })
		began := time.Now()
		n.Notify("vm", link.Offline, "")
		n.Notify("vm", link.Degraded, "")
		n.Notify("vm", link.Healthy, "")
		n.Close()
		if took := time.Since(began); took > 10*time.Second {
			t.Fatalf("a hanging notifier held the rest for %v", took)
		}
		if got := calls(t, out); len(got) != 3 || got[2][0] != "ok" {
			t.Fatalf("calls %q", got)
		}
		if l := logs.String(); !strings.Contains(l, "exited 3") || !strings.Contains(l, "did not finish in time") || !strings.Contains(l, "health=ok") {
			t.Fatalf("log:\n%s", l)
		}
	})

	t.Run("notify never blocks, and a full queue drops the oldest", func(t *testing.T) {
		release := make(chan struct{})
		var mu sync.Mutex
		var sent []link.Health
		n := start(func(_ context.Context, m Message) error {
			<-release
			mu.Lock()
			sent = append(sent, m.Health)
			mu.Unlock()
			return nil
		}, nil)
		begin := time.Now()
		for i := range queueSize * 3 {
			h := link.Offline
			if i == queueSize*3-1 {
				h = link.Healthy
			}
			n.Notify("vm", h, "")
		}
		if took := time.Since(begin); took > time.Second {
			t.Fatalf("Notify blocked for %v", took)
		}
		close(release)
		n.Close()
		if len(sent) > queueSize+1 || sent[len(sent)-1] != link.Healthy {
			t.Fatalf("sent %d, last %q: want at most %d, ending with the newest", len(sent), sent[len(sent)-1], queueSize+1)
		}
	})
}

// TestSystemNotifiers: each system's own notifier gets the text as arguments
// of their own, after anything it would read as an option or a script.
func TestSystemNotifiers(t *testing.T) {
	m := Message{Link: "vm", Health: link.Auth, Hint: `-e do shell script "id" --x`}
	mac := systemArgv("darwin", "", m)
	if len(mac) != 9 || mac[0] != "/usr/bin/osascript" || mac[7] != m.Title() || mac[8] != m.Body() ||
		strings.Contains(strings.Join(mac[:7], " "), "do shell script") {
		t.Fatalf("osascript argv %q", mac)
	}
	lin := systemArgv("linux", "/usr/bin/notify-send", m)
	if strings.Join(lin, "|") != strings.Join([]string{"/usr/bin/notify-send", "--app-name=switchboard", "--", m.Title(), m.Body()}, "|") {
		t.Fatalf("notify-send argv %q", lin)
	}
	if systemArgv("linux", "", m) != nil || systemArgv("windows", "", m) != nil {
		t.Fatal("a notifier was chosen where there is none")
	}
}
