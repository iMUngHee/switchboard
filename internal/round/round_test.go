package round

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestOutcomePrecedence: results are acked whatever an exit code says, a rate
// deferral's schedule survives finals in the same round, and the worst stage
// decides the class.
func TestOutcomePrecedence(t *testing.T) {
	two := lines(rec(1), rec(2))
	ok := map[string]step{"relay out": {"stdout": two}, "relay ack": {}}

	for _, tc := range []struct {
		name    string
		retryMS int64
		wait    time.Duration
	}{
		{"finals and a rate deferral keep not_before", 5_000, time.Minute}, // clamped up
		{"a rate deferral of ten minutes", 600_000, 10 * time.Minute},
		{"a rate deferral past an hour", 7_200_000, time.Hour}, // clamped down
		{"a rate deferral too large for a Duration", 10_000_000_000_000, time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := fake(t, ok)
			dst := fake(t, map[string]step{"relay in": {"stdout": lines(
				res(1, "imported", ""),
				res(2, "deferred", fmt.Sprintf(`,"reason":"rate: 60 imports from laptop in the last hour (limit 60)","retry_after_ms":%d`, tc.retryMS)),
			)}})
			o := newHarness().run(t, src, dst, Input{})
			expect(t, o, OK, "", "")
			if o.Finals != 1 || !o.NotBefore.Equal(testNow.Add(tc.wait)) {
				t.Fatalf("finals %d, not_before %v, want 1 and %v", o.Finals, o.NotBefore, testNow.Add(tc.wait))
			}
			if got := src.stdinOf(t, "relay ack"); got != lines(res(1, "imported", "")) {
				t.Fatalf("ack read %q", got)
			}
		})
	}

	for _, tc := range []struct {
		name  string
		exit  int
		class Class
		code  string
	}{
		{"finals and exit 2: acked, then needs-human", 2, NeedsHuman, CodePager2},
		{"finals and exit 3: acked, then rerun", 3, Rerun, CodeCut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := fake(t, ok)
			dst := fake(t, map[string]step{"relay in": {"stdout": lines(res(1, "imported", "")), "exit": tc.exit}})
			h := newHarness()
			o := h.run(t, src, dst, Input{})
			expect(t, o, tc.class, tc.code, StageIn)
			if got := src.stdinOf(t, "relay ack"); got != lines(res(1, "imported", "")) {
				t.Fatalf("ack read %q", got)
			}
			if o.Finals != 1 || len(o.Pending) != 0 || len(h.saves) != 1 {
				t.Fatalf("finals %d, pending %d, saves %d", o.Finals, len(o.Pending), len(h.saves))
			}
		})
	}
}

// TestTieRules: between equally bad stages the earlier one decides, except
// that a deferral's code wins, so the same gap can be seen twice in a row.
func TestTieRules(t *testing.T) {
	t.Run("the earlier stage keeps a tie", func(t *testing.T) {
		src := fake(t, map[string]step{"relay out": {"stdout": lines(rec(1)), "exit": 1}, "relay ack": {"exit": 1}})
		dst := fake(t, map[string]step{"relay in": {"stdout": lines(res(1, "imported", ""))}})
		expect(t, newHarness().run(t, src, dst, Input{}), Retry, CodePager1, StageOut)
	})
	gap := map[string]step{"relay in": {"stdout": lines(res(2, "deferred", `,"reason":"gap: expected 1, got 2"`))}}
	failedOut := map[string]step{"relay out": {"stdout": lines(rec(2)), "exit": 1}}
	t.Run("a gap's code wins a tie", func(t *testing.T) {
		expect(t, newHarness().run(t, fake(t, failedOut), fake(t, gap), Input{}), Retry, "gap-1-2", StageIn)
	})
	t.Run("but never a worse class", func(t *testing.T) {
		refusedOut := map[string]step{"relay out": {"stdout": lines(rec(2)), "exit": 2}}
		expect(t, newHarness().run(t, fake(t, refusedOut), fake(t, gap), Input{}), NeedsHuman, CodePager2, StageOut)
	})
	t.Run("so the same gap twice is still caught", func(t *testing.T) {
		expect(t, newHarness().run(t, fake(t, failedOut), fake(t, gap), Input{PrevCode: "gap-1-2"}), NeedsHuman, CodeGapRepeated, StageIn)
	})
}

// TestPartialAckAndCorrelation: relay ack sees only final results answering a
// record staged this round; a failed ack keeps them, and the next round acks
// them before anything else.
func TestPartialAckAndCorrelation(t *testing.T) {
	recs := lines(rec(1), rec(2), rec(3))
	finals := []string{res(1, "imported", ""), res(2, "duplicate", ""), res(3, "rejected", `,"reason":"no such alias ghost"`)}
	stray := strings.Replace(res(9, "imported", ""), `"seq":9`, `"seq":19`, 1)  // answers no staged record
	otherLink := strings.Replace(res(1, "imported", ""), srcStore, dstStore, 1) // another link's store
	otherOrigin := strings.Replace(res(2, "imported", ""), `"origin_id":4002`, `"origin_id":4999`, 1)
	dst := fake(t, map[string]step{"relay in": {"stdout": lines(finals[0], stray, finals[1], otherLink, otherOrigin, finals[2])}})

	src := fake(t, map[string]step{"relay out": {"stdout": recs}, "relay ack": {"exit": 1}})
	h := newHarness()
	o := h.run(t, src, dst, Input{})
	expect(t, o, Retry, CodePager1, StageAck)
	if o.Records != 3 || o.Finals != 3 || o.Dropped != 3 {
		t.Fatalf("records %d finals %d dropped %d", o.Records, o.Finals, o.Dropped)
	}
	if got := src.stdinOf(t, "relay ack"); got != lines(finals...) {
		t.Fatalf("ack read\n%q\nwant\n%q", got, lines(finals...))
	}
	if len(h.saves) != 1 || strings.Join(h.saves[0], "\n") != strings.Join(finals, "\n") {
		t.Fatalf("saved %q before ack", h.saves)
	}
	if strings.Join(o.Pending, "\n") != strings.Join(finals, "\n") {
		t.Fatalf("pending after a failed ack: %q", o.Pending)
	}

	t.Run("the stored results are acked first", func(t *testing.T) {
		next := fake(t, map[string]step{"relay out": {}, "relay ack": {}})
		o2 := newHarness().run(t, next, fake(t, nil), Input{Pending: o.Pending})
		expect(t, o2, Idle, "", "")
		calls := next.calls(t)
		if len(calls) != 2 || calls[0].Args[1] != "ack" || calls[1].Args[1] != "out" {
			t.Fatalf("calls %+v, want ack then out", calls)
		}
		if calls[0].Stdin != lines(finals...) || len(o2.Pending) != 0 {
			t.Fatalf("ack read %q; pending %q", calls[0].Stdin, o2.Pending)
		}
	})

	t.Run("a stored ack that fails again runs nothing new", func(t *testing.T) {
		next := fake(t, map[string]step{"relay out": {"stdout": recs}, "relay ack": {"exit": 1}})
		o2 := newHarness().run(t, next, fake(t, nil), Input{Pending: o.Pending})
		expect(t, o2, Retry, CodePager1, StageAck)
		if next.ran(t, "relay out") || len(o2.Pending) != 3 {
			t.Fatalf("out ran = %v, pending %d", next.ran(t, "relay out"), len(o2.Pending))
		}
	})

	t.Run("no correlated finals: ack is skipped", func(t *testing.T) {
		src := fake(t, map[string]step{"relay out": {"stdout": recs}, "relay ack": {}})
		dst := fake(t, map[string]step{"relay in": {"stdout": lines(stray)}})
		o := newHarness().run(t, src, dst, Input{})
		if src.ran(t, "relay ack") || o.Finals != 0 || o.Dropped != 1 {
			t.Fatalf("ack ran = %v, finals %d, dropped %d", src.ran(t, "relay ack"), o.Finals, o.Dropped)
		}
	})
}

// TestCutAndCaps: a cut exit-0 stream is not idle, caps stop a stage without
// losing what fit, and a chatty stderr never blocks a stage.
func TestCutAndCaps(t *testing.T) {
	t.Run("relay out ending mid-line is a cut", func(t *testing.T) {
		src := fake(t, map[string]step{"relay out": {"stdout": rec(1) + "\n" + rec(2)[:40]}, "relay ack": {}})
		dst := fake(t, map[string]step{"relay in": {"stdout": lines(res(1, "imported", ""))}})
		o := newHarness().run(t, src, dst, Input{})
		expect(t, o, Rerun, CodeCutOut, StageOut)
		if got := dst.stdinOf(t, "relay in"); got != lines(rec(1)) {
			t.Fatalf("in read %q, want only the complete line", got)
		}
		if o.Finals != 1 || !src.ran(t, "relay ack") {
			t.Fatalf("finals %d, acked %v", o.Finals, src.ran(t, "relay ack"))
		}
	})

	t.Run("relay in ending mid-line is a cut; the complete results are acked", func(t *testing.T) {
		src := fake(t, map[string]step{"relay out": {"stdout": lines(rec(1), rec(2))}, "relay ack": {}})
		dst := fake(t, map[string]step{"relay in": {"stdout": res(1, "imported", "") + "\n" + res(2, "imported", "")[:40]}})
		o := newHarness().run(t, src, dst, Input{})
		expect(t, o, Rerun, CodeCutIn, StageIn)
		if got := src.stdinOf(t, "relay ack"); got != lines(res(1, "imported", "")) {
			t.Fatalf("ack read %q, want only the complete result", got)
		}
	})

	t.Run("byte-order marks and CRLF are read; marks alone are empty", func(t *testing.T) {
		src := fake(t, map[string]step{"relay out": {"stdout": "\uFEFF\uFEFF" + rec(1) + "\r\n"}, "relay ack": {}})
		dst := fake(t, map[string]step{"relay in": {"stdout": lines(res(1, "imported", ""))}})
		o := newHarness().run(t, src, dst, Input{})
		expect(t, o, OK, "", "")
		if got := dst.stdinOf(t, "relay in"); got != lines(rec(1)) {
			t.Fatalf("in read %q", got)
		}
		empty := fake(t, map[string]step{"relay out": {"stdout": "\uFEFF\uFEFF"}})
		expect(t, newHarness().run(t, empty, fake(t, nil), Input{}), Idle, "", "")
	})

	t.Run("relay out over the cap halves the limit and forwards what fit", func(t *testing.T) {
		two := lines(rec(1), rec(2))
		src := fake(t, map[string]step{"relay out": {"stdout": two, "stdout_bytes": 8 << 10}, "relay ack": {}})
		dst := fake(t, map[string]step{"relay in": {"stdout": lines(res(1, "imported", ""), res(2, "imported", ""))}})
		h := newHarness(func(e *Engine) { e.Runner.StdoutCap = len(two) + 100 })
		o := h.run(t, src, dst, Input{Limit: 100})
		expect(t, o, Rerun, CodeOutCap, StageOut)
		if o.Limit != 50 || o.Full {
			t.Fatalf("limit %d full %v, want 50 and false", o.Limit, o.Full)
		}
		if got := dst.stdinOf(t, "relay in"); got != two || o.Finals != 2 {
			t.Fatalf("in read %q; finals %d", got, o.Finals)
		}
	})

	t.Run("one record over the cap needs a person", func(t *testing.T) {
		src := fake(t, map[string]step{"relay out": {"stdout_bytes": 8 << 10}})
		h := newHarness(func(e *Engine) { e.Runner.StdoutCap = 1 << 10 })
		expect(t, h.run(t, src, fake(t, nil), Input{Limit: 1}), NeedsHuman, CodeRecordOverCap, StageOut)
	})

	t.Run("relay in over the cap acks the results that fit", func(t *testing.T) {
		two := lines(rec(1), rec(2))
		results := lines(res(1, "imported", ""), res(2, "imported", ""))
		src := fake(t, map[string]step{"relay out": {"stdout": two}, "relay ack": {}})
		dst := fake(t, map[string]step{"relay in": {"stdout": results, "stdout_bytes": 8 << 10}})
		h := newHarness(func(e *Engine) { e.Runner.StdoutCap = max(len(two), len(results)) + 100 })
		o := h.run(t, src, dst, Input{})
		expect(t, o, Rerun, CodeInCap, StageIn)
		if got := src.stdinOf(t, "relay ack"); got != results || len(o.Pending) != 0 {
			t.Fatalf("ack read %q; pending %d", got, len(o.Pending))
		}
	})

	t.Run("a megabyte of stderr never blocks a stage", func(t *testing.T) {
		mb := 1 << 20
		src := fake(t, map[string]step{"relay out": {"stdout": lines(rec(1)), "stderr_bytes": mb}, "relay ack": {"stderr_bytes": mb}})
		dst := fake(t, map[string]step{"relay in": {"stdout": lines(res(1, "imported", "")), "stderr_bytes": mb}})
		h := newHarness(func(e *Engine) { e.Runner.Timeout = 20 * time.Second })
		o := h.run(t, src, dst, Input{})
		expect(t, o, OK, "", "")
		if o.Finals != 1 {
			t.Fatalf("finals %d", o.Finals)
		}
	})

	t.Run("a full batch says so", func(t *testing.T) {
		src := fake(t, map[string]step{"relay out": {"stdout": lines(rec(1), rec(2))}, "relay ack": {}})
		dst := fake(t, map[string]step{"relay in": {"stdout": lines(res(1, "imported", ""), res(2, "imported", ""))}})
		if o := newHarness().run(t, src, dst, Input{Limit: 2}); !o.Full {
			t.Fatalf("a batch of exactly --limit records is not full: %+v", o)
		}
	})

	t.Run("lines from relay in that are not results still ack the results", func(t *testing.T) {
		src := fake(t, map[string]step{"relay out": {"stdout": lines(rec(1))}, "relay ack": {}})
		dst := fake(t, map[string]step{"relay in": {"stdout": "Welcome to vm\n" + lines(res(1, "imported", ""))}})
		expect(t, newHarness().run(t, src, dst, Input{}), NeedsHuman, CodeNotPager, StageIn)
		if got := src.stdinOf(t, "relay ack"); got != lines(res(1, "imported", "")) {
			t.Fatalf("ack read %q", got)
		}
	})

	t.Run("lines that are not records forward nothing", func(t *testing.T) {
		src := fake(t, map[string]step{"relay out": {"stdout": "Welcome to vm\n" + lines(rec(1))}})
		dst := fake(t, nil)
		expect(t, newHarness().run(t, src, dst, Input{}), NeedsHuman, CodeNotPager, StageOut)
		if len(dst.calls(t)) > 0 {
			t.Fatal("a banner was forwarded to relay in")
		}
	})
}

// TestDiagnosticsCarryNoChildOutput: what a stage writes to stderr never
// reaches the log, the outcome or the stored state, on success or failure.
func TestDiagnosticsCarryNoChildOutput(t *testing.T) {
	const marker = "kipa@vm said: the launch codes are 0000"
	stderr := "pager: delivered to kipa: " + marker + "\n"
	for _, tc := range []struct {
		name     string
		src, dst map[string]step
	}{
		{"success", map[string]step{"relay out": {"stdout": lines(rec(1)), "stderr": stderr}, "relay ack": {"stderr": stderr}},
			map[string]step{"relay in": {"stdout": lines(res(1, "imported", "")), "stderr": stderr}}},
		{"out exit 255", map[string]step{"relay out": {"exit": 255, "stderr": stderr}}, nil},
		{"out exit 1", map[string]step{"relay out": {"exit": 1, "stderr": stderr}}, nil},
		{"in exit 2", map[string]step{"relay out": {"stdout": lines(rec(1))}},
			map[string]step{"relay in": {"exit": 2, "stderr": stderr}}},
		{"ack exit 1", map[string]step{"relay out": {"stdout": lines(rec(1))}, "relay ack": {"exit": 1, "stderr": stderr}},
			map[string]step{"relay in": {"stdout": lines(res(1, "imported", "")), "stderr": stderr}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var debug bytes.Buffer
			h := newHarness(func(e *Engine) { e.Save = nil })
			var saved []string
			h.e.Save = func(p []string) error { saved = append(saved, p...); return nil }
			o := h.run(t, fake(t, tc.src), fake(t, tc.dst), Input{})
			for where, text := range map[string]string{
				"log":     h.logs.String(),
				"outcome": fmt.Sprintf("%+v", o),
				"state":   strings.Join(saved, "\n"),
			} {
				if strings.Contains(text, "launch codes") || strings.Contains(text, "kipa@vm") {
					t.Errorf("child stderr reached the %s:\n%s", where, text)
				}
			}

			// The same round with --debug shows it, so the check above is
			// not passing on a stderr that was never written.
			h2 := newHarness(func(e *Engine) { e.Debug = &debug })
			h2.run(t, fake(t, tc.src), fake(t, tc.dst), Input{})
			if !strings.Contains(debug.String(), marker) {
				t.Fatalf("debug output lacks the stderr:\n%s", debug.String())
			}
		})
	}
}

// TestStateWriteFailureStillAcks: a failing write-ahead save is reported, and
// ack runs from memory.
func TestStateWriteFailureStillAcks(t *testing.T) {
	src := fake(t, map[string]step{"relay out": {"stdout": lines(rec(1))}, "relay ack": {}})
	dst := fake(t, map[string]step{"relay in": {"stdout": lines(res(1, "imported", ""))}})
	h := newHarness(func(e *Engine) { e.Save = func([]string) error { return errors.New("disk full") } })
	o := h.run(t, src, dst, Input{})
	expect(t, o, OK, "", "")
	if o.StateErr == nil || !src.ran(t, "relay ack") || len(o.Pending) != 0 {
		t.Fatalf("state err %v, acked %v, pending %d", o.StateErr, src.ran(t, "relay ack"), len(o.Pending))
	}
	if !strings.Contains(h.logs.String(), "code=state-io") {
		t.Fatalf("the failed write was not logged:\n%s", h.logs.String())
	}
}
