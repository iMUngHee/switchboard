package round

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/iMUngHee/switchboard/internal/transport"
)

// TestClassificationTable pins every row of the failure table (plan S3), each
// driven through a real round against testpager.
func TestClassificationTable(t *testing.T) {
	one := lines(rec(1))
	imported := lines(res(1, "imported", ""))
	for _, tc := range []struct {
		name               string
		src, dst           map[string]step
		missingSrc         bool // the source's launcher does not exist
		localMissing       bool // the source is a local pager that does not exist
		invalid            bool // the source is an ssh endpoint that fails validation
		in                 Input
		timeout            time.Duration
		cancelAfter        time.Duration // cancel the round's context, as a shutdown does
		class              Class
		code, stage        string
		destinationSkipped bool
	}{
		{name: "out 0, empty: idle",
			src: map[string]step{"relay out": {}}, class: Idle, destinationSkipped: true},

		{name: "pager exit 1: retry",
			src:   map[string]step{"relay out": {"exit": 1, "stderr": "pager: database is locked\n"}},
			class: Retry, code: "db-locked", stage: StageOut, destinationSkipped: true},
		{name: "pager exit 1 without a known cause: retry",
			src:   map[string]step{"relay out": {"exit": 1}},
			class: Retry, code: CodePager1, stage: StageOut},

		{name: "pager exit 2 from out: needs-human",
			src:   map[string]step{"relay out": {"exit": 2, "stderr": "pager: unknown peer \"vm\" (pager peer ls)\n"}},
			class: NeedsHuman, code: "unknown-peer", stage: StageOut, destinationSkipped: true},
		{name: "pager exit 2 from in: needs-human",
			src:   map[string]step{"relay out": {"stdout": one}},
			dst:   map[string]step{"relay in": {"exit": 2, "stderr": "pager: refused: wrong store\n"}},
			class: NeedsHuman, code: "refused", stage: StageIn},
		{name: "pager exit 2 from ack: needs-human",
			src:   map[string]step{"relay out": {"stdout": one}, "relay ack": {"exit": 2}},
			dst:   map[string]step{"relay in": {"stdout": imported}},
			class: NeedsHuman, code: CodePager2, stage: StageAck},

		{name: "pager exit 3 from in: rerun",
			src:   map[string]step{"relay out": {"stdout": one}},
			dst:   map[string]step{"relay in": {"exit": 3}},
			class: Rerun, code: CodeCut, stage: StageIn},
		{name: "pager exit 3 from ack: rerun",
			src:   map[string]step{"relay out": {"stdout": one}, "relay ack": {"exit": 3}},
			dst:   map[string]step{"relay in": {"stdout": imported}},
			class: Rerun, code: CodeCut, stage: StageAck},

		{name: "out exit 3: needs-human",
			src:   map[string]step{"relay out": {"exit": 3}},
			class: NeedsHuman, code: CodeUnexpected, stage: StageOut},
		{name: "a code pager never returns: needs-human",
			src:   map[string]step{"relay out": {"exit": 4}},
			class: NeedsHuman, code: CodeUnexpected, stage: StageOut},

		{name: "exit 127 on the far side: needs-human",
			src:   map[string]step{"relay out": {"exit": 127}},
			class: NeedsHuman, code: CodeNotRun, stage: StageOut},
		{name: "exit 126 on the far side: needs-human",
			src:   map[string]step{"relay out": {"stdout": one}},
			dst:   map[string]step{"relay in": {"exit": 126}},
			class: NeedsHuman, code: CodeNotRun, stage: StageIn},

		{name: "a gap deferral once: retry",
			src:   map[string]step{"relay out": {"stdout": lines(rec(2))}},
			dst:   map[string]step{"relay in": {"stdout": lines(res(2, "deferred", `,"reason":"gap: expected 1, got 2"`))}},
			class: Retry, code: "gap-1-2", stage: StageIn},
		{name: "the same gap deferral twice: needs-human",
			src:   map[string]step{"relay out": {"stdout": lines(rec(2))}},
			dst:   map[string]step{"relay in": {"stdout": lines(res(2, "deferred", `,"reason":"gap: expected 1, got 2"`))}},
			in:    Input{PrevCode: "gap-1-2"},
			class: NeedsHuman, code: CodeGapRepeated, stage: StageIn},
		{name: "a deferral for a reason this release does not know: retry",
			src:   map[string]step{"relay out": {"stdout": one}},
			dst:   map[string]step{"relay in": {"stdout": lines(res(1, "deferred", `,"reason":"maintenance"`))}},
			class: Retry, code: CodeDeferred, stage: StageIn},

		{name: "ssh 255 with an auth failure: auth",
			src:   map[string]step{"relay out": {"exit": 255, "stderr": "me@vm.example.com: Permission denied (publickey).\n"}},
			class: Auth, code: "auth", stage: StageOut},
		{name: "ssh 255 with a host key failure: auth",
			src:   map[string]step{"relay out": {"exit": 255, "stderr": "Host key verification failed.\n"}},
			class: Auth, code: "host-key", stage: StageOut},
		{name: "ssh 255 other: offline",
			src:   map[string]step{"relay out": {"exit": 255, "stderr": "Connection closed by 192.0.2.1 port 22\n"}},
			class: Offline, code: "conn-closed", stage: StageOut},
		{name: "ssh 255 unexplained: offline",
			src:   map[string]step{"relay out": {"exit": 255}},
			class: Offline, code: CodeSSH, stage: StageOut},
		{name: "a connect timeout: offline",
			src:   map[string]step{"relay out": {"exit": 255, "stderr": "ssh: connect to host vm.example.com port 22: Operation timed out\n"}},
			class: Offline, code: "conn-timeout", stage: StageOut},
		{name: "a spawn error: offline",
			missingSrc: true,
			class:      Offline, code: CodeSpawn, stage: StageOut},
		{name: "the local pager will not start: needs-human",
			localMissing: true,
			class:        NeedsHuman, code: CodeLocalPager, stage: StageOut},

		{name: "our deadline killed it: timeout",
			src:     map[string]step{"relay out": {"sleep_ms": 20000}},
			timeout: 500 * time.Millisecond,
			class:   Timeout, code: CodeDeadline, stage: StageOut},
		{name: "switchboard is stopping: stopped",
			src:         map[string]step{"relay out": {"sleep_ms": 20000}},
			cancelAfter: 300 * time.Millisecond,
			class:       Stopped, code: CodeStopped, stage: StageOut},
		{name: "an endpoint that fails validation: needs-human",
			invalid: true,
			class:   NeedsHuman, code: CodeInvalidEndpoint, stage: StageOut},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var src fakeSide
			switch {
			case tc.missingSrc:
				src = missing(t)
			case tc.localMissing:
				src = fakeSide{ep: transport.LocalEndpoint{Pager: filepath.Join(t.TempDir(), "pager")}}
			case tc.invalid:
				src = fakeSide{ep: transport.SSHEndpoint{SSH: filepath.Join(t.TempDir(), "ssh"), Host: "vm%h", Pager: "/usr/local/bin/pager"}}
			default:
				src = fake(t, tc.src)
			}
			dst := fake(t, tc.dst)
			h := newHarness(func(e *Engine) {
				if tc.timeout > 0 {
					e.Runner.Timeout = tc.timeout
				}
			})
			ctx := t.Context()
			if tc.cancelAfter > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				time.AfterFunc(tc.cancelAfter, cancel)
			}
			o := h.runCtx(ctx, src, dst, tc.in)
			expect(t, o, tc.class, tc.code, tc.stage)
			if tc.destinationSkipped && len(dst.calls(t)) > 0 {
				t.Errorf("the destination was contacted: %+v", dst.calls(t))
			}
		})
	}
}

// TestSignalDeathIsRetry: a stage killed by a signal switchboard did not send
// (a service manager stopping everything at once) is retried, not left for a
// person. On Windows os.Kill is TerminateProcess with exit 1, also a retry.
func TestSignalDeathIsRetry(t *testing.T) {
	src := fake(t, map[string]step{"relay out": {"kill_self": true}})
	o := newHarness().run(t, src, fake(t, nil), Input{})
	code := CodeSignal
	if runtime.GOOS == "windows" {
		code = CodePager1
	}
	expect(t, o, Retry, code, StageOut)
}

// TestContact: only a stage that reached pager on the far side and got a
// pager exit status counts as contact.
func TestContact(t *testing.T) {
	for name, tc := range map[string]struct {
		src  fakeSide
		want bool
	}{
		"exit 0":   {fake(t, map[string]step{"relay out": {}}), true},
		"exit 2":   {fake(t, map[string]step{"relay out": {"exit": 2}}), true},
		"exit 255": {fake(t, map[string]step{"relay out": {"exit": 255}}), false},
		"exit 127": {fake(t, map[string]step{"relay out": {"exit": 127}}), false},
		"spawn":    {missing(t), false},
	} {
		if got := newHarness().run(t, tc.src, fake(t, nil), Input{}).Contact; got != tc.want {
			t.Errorf("%s: contact %v, want %v", name, got, tc.want)
		}
	}
}
