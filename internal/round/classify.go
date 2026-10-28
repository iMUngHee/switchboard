package round

import (
	"context"

	"github.com/iMUngHee/switchboard/internal/diag"
	"github.com/iMUngHee/switchboard/internal/pagercli"
	"github.com/iMUngHee/switchboard/internal/transport"
)

// Class is what a round means for its direction of a link.
type Class string

// The classes, from best to worst. When a round's stages disagree, the worst
// one decides (see worse).
const (
	Idle       Class = "idle"        // nothing to carry
	OK         Class = "ok"          // carried, or deferred by the receiver's rate cap
	Rerun      Class = "rerun"       // the stream was cut or capped; run again now
	Retry      Class = "retry"       // pager failed for a reason that passes; back off
	Timeout    Class = "timeout"     // a stage hit its deadline and was stopped
	Offline    Class = "offline"     // the far side could not be reached
	Auth       Class = "auth"        // ssh needs a key loaded or a host key accepted
	Stopped    Class = "stopped"     // switchboard itself is shutting down
	NeedsHuman Class = "needs-human" // stop the direction and tell a person
)

var rank = map[Class]int{Idle: 0, OK: 1, Rerun: 2, Retry: 3, Timeout: 4, Offline: 5, Auth: 6, Stopped: 7, NeedsHuman: 8}

func worse(a, b Class) bool { return rank[a] > rank[b] }

// Diagnostic codes the round engine sets itself; the others come from diag.
// Like those, every one is a fixed string: nothing a child printed.
const (
	CodeStopped         = "stopped"
	CodeDeadline        = "deadline"
	CodeSpawn           = "spawn"            // the launcher (ssh, docker, …) could not be started
	CodeLocalPager      = "local-pager"      // the local pager could not be started
	CodeIO              = "io"               // reading or writing a stage's pipes failed
	CodeSignal          = "signal"           // killed by a signal switchboard did not send
	CodePager1          = "pager-1"          // pager exit 1 without a known cause
	CodePager2          = "pager-2"          // pager exit 2 without a known cause
	CodeCut             = "cut"              // pager exit 3: its input ended inside a line
	CodeNotRun          = "pager-not-run"    // exit 126/127 on the far side
	CodeSSH             = "ssh"              // ssh exit 255 without a known cause
	CodeUnexpected      = "unexpected-exit"  // an exit status pager never uses
	CodeNotPager        = "not-pager-output" // stdout held something other than relay lines
	CodeCutOut          = "cut-out"          // relay out exited 0 but its output ended mid-line
	CodeCutIn           = "cut-in"           // relay in exited 0 but its output ended mid-line
	CodeOutCap          = "out-cap"          // relay out passed the stdout cap
	CodeInCap           = "in-cap"           // relay in passed the stdout cap
	CodeRecordOverCap   = "record-over-cap"  // a single record does not fit under the cap
	CodeGapRepeated     = "receiver-rolled-back"
	CodeDeferred        = "deferred" // deferred for a reason this release does not know
	CodeInvalidEndpoint = "invalid-endpoint"
)

// verdict is one stage's reading.
type verdict struct {
	class Class
	code  string
	exit  int
}

// classify reads how one stage ended. ctx is the round's own context: when it
// has ended, switchboard is stopping, whatever the stage reports.
func classify(ctx context.Context, stage string, kind transport.Kind, out pagercli.Output) verdict {
	r := out.Result
	switch {
	case ctx.Err() != nil:
		return verdict{Stopped, CodeStopped, r.ExitCode}
	case r.TimedOut:
		return verdict{Timeout, CodeDeadline, r.ExitCode}
	case !r.Started && kind == transport.Local:
		return verdict{NeedsHuman, CodeLocalPager, -1}
	case !r.Started:
		return verdict{Offline, CodeSpawn, -1}
	case r.Err != nil && kind == transport.Local:
		return verdict{Retry, CodeIO, r.ExitCode}
	case r.Err != nil:
		return verdict{Offline, CodeIO, r.ExitCode}
	case r.ExitCode < 0:
		// A service manager stopping everything at once, or the OOM killer:
		// nothing a person must fix, and not worth surviving a restart.
		return verdict{Retry, CodeSignal, -1}
	}

	remote := kind != transport.Local
	switch code := r.ExitCode; {
	case code == 0:
		return verdict{OK, "", 0}
	case code == 1:
		return verdict{Retry, or(diag.Pager(out.StderrHead), CodePager1), 1}
	case code == 2:
		return verdict{NeedsHuman, or(diag.Pager(out.StderrHead), CodePager2), 2}
	case code == 3 && stage != StageOut:
		return verdict{Rerun, CodeCut, 3}
	case (code == 126 || code == 127) && remote:
		return verdict{NeedsHuman, CodeNotRun, code}
	case code == 255 && remote:
		c := diag.SSH(out.StderrHead)
		if diag.IsAuth(c) {
			return verdict{Auth, c, 255}
		}
		return verdict{Offline, or(c, CodeSSH), 255}
	default:
		// relay out never reads input, so it never exits 3; anything else
		// pager never returns. Something other than pager answered.
		return verdict{NeedsHuman, CodeUnexpected, code}
	}
}

// reached reports whether a stage got to pager on the far side and pager
// answered as pager does.
func reached(kind transport.Kind, out pagercli.Output) bool {
	r := out.Result
	return kind != transport.Local && r.Started && !r.TimedOut && r.Err == nil && r.ExitCode >= 0 && r.ExitCode <= 3
}

func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
