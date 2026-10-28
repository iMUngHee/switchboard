package round

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/pagercli"
	"github.com/iMUngHee/switchboard/internal/testenv"
	"github.com/iMUngHee/switchboard/internal/transport"
)

// Placeholder store and link ids for records testpager prints.
const (
	srcStore = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dstStore = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	linkID   = "cccccccccccccccccccccccccccccccc"
)

var testNow = time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)

// rec is the record relay out would print for seq on the test link.
func rec(seq int64) string {
	return fmt.Sprintf(`{"v":1,"kind":"msg","link":{"store":%q,"id":%q,"seq":%d},"origin":{"store":%q,"id":%d},"dst_store":%q,"to":"kipa","from":"sape","label":"sape","hop":0,"sent_at":"2026-10-28T00:00:00Z","body":"hello %d"}`,
		srcStore, linkID, seq, srcStore, 4000+seq, dstStore, seq)
}

// res is the result relay in would print for seq. extra is appended inside
// the object, e.g. `,"reason":"…"`.
func res(seq int64, status, extra string) string {
	return fmt.Sprintf(`{"v":1,"kind":"result","store":%q,"max_v":1,"link":{"store":%q,"id":%q,"seq":%d},"origin_id":%d,"status":%q%s}`,
		dstStore, srcStore, linkID, seq, 4000+seq, status, extra)
}

// lines joins complete lines, each with its LF.
func lines(ls ...string) string {
	if len(ls) == 0 {
		return ""
	}
	return strings.Join(ls, "\n") + "\n"
}

type step = map[string]any

// fakeSide is a testpager endpoint and the record of what it was asked.
type fakeSide struct {
	ep  transport.Endpoint
	log string
}

func fake(t *testing.T, plan map[string]step) fakeSide {
	t.Helper()
	dir := t.TempDir()
	planPath := filepath.Join(dir, "plan.json")
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(planPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "calls.jsonl")
	return fakeSide{
		ep:  transport.ExecEndpoint{Argv: []string{testenv.TestPager(t)}, Env: map[string]string{"TESTPAGER_PLAN": planPath, "TESTPAGER_LOG": log}},
		log: log,
	}
}

// missing is an endpoint whose launcher does not exist.
func missing(t *testing.T) fakeSide {
	return fakeSide{ep: transport.ExecEndpoint{Argv: []string{filepath.Join(t.TempDir(), "no-such-launcher")}}}
}

type call struct {
	Args  []string `json:"args"`
	Stdin string   `json:"stdin"`
}

// calls returns what testpager was asked, in order.
func (f fakeSide) calls(t *testing.T) []call {
	t.Helper()
	if f.log == "" {
		return nil
	}
	file, err := os.Open(f.log)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var cs []call
	sc := bufio.NewScanner(file)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var c call
		if err := json.Unmarshal(sc.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		cs = append(cs, c)
	}
	return cs
}

// stdinOf returns what the named subcommand ("relay ack", …) read, or fails
// when it ran other than once.
func (f fakeSide) stdinOf(t *testing.T, sub string) string {
	t.Helper()
	var got []string
	for _, c := range f.calls(t) {
		if strings.Join(c.Args[:min(2, len(c.Args))], " ") == sub {
			got = append(got, c.Stdin)
		}
	}
	if len(got) != 1 {
		t.Fatalf("%s ran %d times, want once", sub, len(got))
	}
	return got[0]
}

func (f fakeSide) ran(t *testing.T, sub string) bool {
	t.Helper()
	for _, c := range f.calls(t) {
		if strings.Join(c.Args[:min(2, len(c.Args))], " ") == sub {
			return true
		}
	}
	return false
}

// harness runs rounds against two sides and keeps everything a round leaves
// behind: its log, and what it saved.
type harness struct {
	e     *Engine
	logs  bytes.Buffer
	saves [][]string
}

func newHarness(opts ...func(*Engine)) *harness {
	h := &harness{}
	h.e = &Engine{
		Runner: pagercli.Runner{Timeout: 30 * time.Second},
		Log:    slog.New(slog.NewTextHandler(&h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:    func() time.Time { return testNow },
		Save: func(p []string) error {
			h.saves = append(h.saves, append([]string(nil), p...))
			return nil
		},
	}
	for _, o := range opts {
		o(h.e)
	}
	return h
}

func (h *harness) run(t *testing.T, src, dst fakeSide, in Input) Outcome {
	t.Helper()
	return h.runCtx(t.Context(), src, dst, in)
}

func (h *harness) runCtx(ctx context.Context, src, dst fakeSide, in Input) Outcome {
	d := Direction{Link: "vm", Name: "outbound", From: Side{Endpoint: src.ep, Peer: "vm"}, To: Side{Endpoint: dst.ep, Peer: "laptop"}}
	return h.e.Run(ctx, d, in)
}

func expect(t *testing.T, o Outcome, class Class, code, stage string) {
	t.Helper()
	if o.Class != class || o.Code != code || o.Stage != stage {
		t.Fatalf("outcome %s/%s at %q, want %s/%s at %q (%+v)", o.Class, o.Code, o.Stage, class, code, stage, o)
	}
}
