// Package round runs one relay round in one direction of a link — relay out
// on the source, relay in on the destination, relay ack on the source — and
// reads what happened.
//
// A round has three independent results, combined in a fixed order:
//
//  1. Results first. Every complete result line that answers a record staged
//     this round is acked, whatever any exit code says: the destination
//     committed the import before it wrote the result.
//  2. The schedule second. A deferral for the receiver's rate cap sets a time
//     not to run before, and finals in the same round never clear it.
//  3. The failure class last. The worst stage decides.
//
// Results are stored through Engine.Save before relay ack runs. If switchboard
// dies after that write, the next round acks them first; if it dies before,
// relay out sends the same records again, the destination answers duplicate,
// and that ack closes them, so nothing is delivered twice.
package round

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/iMUngHee/switchboard/internal/pagercli"
	"github.com/iMUngHee/switchboard/internal/transport"
)

// Stage names, as logs and outcomes give them.
const (
	StageOut = "out"
	StageIn  = "in"
	StageAck = "ack"
)

const (
	DefaultLimit = 100  // relay out's own default
	MaxLimit     = 1000 // the most relay out accepts
	MinRateWait  = time.Minute
	MaxRateWait  = time.Hour
)

// Side is one end of a direction: where pager runs there, and the peer name
// that store gives the store at the other end.
type Side struct {
	Endpoint transport.Endpoint
	Peer     string
}

// Direction is one way across a link.
type Direction struct {
	Link string // the link's name, for logs
	Name string // "outbound" or "inbound", for logs
	From Side   // relay out and relay ack run here
	To   Side   // relay in runs here
}

// Input is what the caller remembers about the direction.
type Input struct {
	Pending  []string // result lines stored by an earlier round and not yet acked
	Limit    int      // relay out --limit; zero means DefaultLimit
	PrevCode string   // the direction's last diagnostic code
}

// Outcome is what a round found. Every field is a count, a time or a fixed
// string, except Pending, which holds result lines verbatim because relay ack
// needs them as pager wrote them.
type Outcome struct {
	Class     Class
	Code      string
	Stage     string // the stage that decided Class; "" when none failed
	Exit      int
	Records   int       // records relay out staged
	Finals    int       // final results answering a staged record
	Dropped   int       // result lines that answered no staged record
	Full      bool      // relay out returned a full batch, so more may wait
	Limit     int       // the --limit for the next round: halved after relay out passed the cap
	NotBefore time.Time // when set, do not run this direction again before it
	Pending   []string  // results still to ack; the caller keeps them
	Contact   bool      // a stage reached pager on the far side
	StateErr  error     // storing results before ack failed; ack ran from memory
}

// Engine runs rounds.
type Engine struct {
	Runner pagercli.Runner
	Log    *slog.Logger     // nil discards
	Now    func() time.Time // nil means time.Now
	// Save stores result lines before relay ack runs, and must return only
	// once they are on disk. nil stores nothing.
	Save func(pending []string) error
	// Debug, when set, receives each stage's stderr head. Only `once --debug`
	// sets it, to the terminal that asked; nothing else ever sees stderr.
	Debug io.Writer

	// crash, set by tests, ends Run at a named point as if the process had
	// died there.
	crash func(point string) bool
}

// Run runs one round in direction d.
func (e *Engine) Run(ctx context.Context, d Direction, in Input) Outcome {
	o := Outcome{Class: Idle, Limit: in.Limit, Pending: in.Pending}
	if o.Limit <= 0 {
		o.Limit = DefaultLimit
	}
	o.Limit = min(o.Limit, MaxLimit)

	// Results an earlier round stored come before anything new.
	if len(o.Pending) > 0 {
		if v := e.ack(ctx, d, o.Pending, &o); v.class != OK {
			o.decide(StageAck, v)
			return e.done(d, o)
		}
		o.Pending = nil
	}

	out, v := e.stage(ctx, d, d.From, StageOut, nil, &o,
		"relay", "out", "--peer", d.From.Peer, "--limit", strconv.Itoa(o.Limit))
	lines, partial := splitLines(out.Stdout)
	recs, ok := parseRecords(lines)
	switch {
	case !ok:
		// A banner, a prompt, another program: forward none of it.
		o.decide(StageOut, verdict{NeedsHuman, CodeNotPager, out.Result.ExitCode})
		return e.done(d, o)
	case out.Over && o.Limit > 1:
		v.code, o.Limit = CodeOutCap, max(o.Limit/2, 1)
	case out.Over:
		v = verdict{NeedsHuman, CodeRecordOverCap, v.exit}
	case v.class == OK && partial:
		// pager never ends a line without its LF: the stream was cut.
		v = verdict{Rerun, CodeCutOut, 0}
	}
	o.Records = len(recs)
	if len(recs) == 0 && v.class == OK {
		return e.done(d, o) // idle
	}
	o.decide(StageOut, v)
	if len(recs) == 0 {
		return e.done(d, o)
	}
	o.Full = v.class == OK && len(recs) == o.Limit

	// A failed or cut relay out still forwards its complete lines: relay in
	// judges each record on its own, in order.
	res, iv := e.stage(ctx, d, d.To, StageIn, joinLines(recs), &o, "relay", "in", "--peer", d.To.Peer)
	rlines, rpartial := splitLines(res.Stdout)
	c := correlate(recs, rlines)
	o.Finals, o.Dropped = len(c.finals), c.dropped
	switch {
	case !c.ok:
		iv = verdict{NeedsHuman, CodeNotPager, iv.exit}
	case iv.class == OK && rpartial:
		iv = verdict{Rerun, CodeCutIn, 0}
	}
	o.decide(StageIn, iv)
	e.schedule(&o, c.deferral, in.PrevCode)

	if len(c.finals) > 0 {
		if e.crashed("before-save") {
			return o
		}
		if e.Save != nil {
			if err := e.Save(c.finals); err != nil {
				o.StateErr = err
				e.logger().Warn("state write failed; acking from memory", "link", d.Link, "direction", d.Name, "code", "state-io", "err", err)
			}
		}
		o.Pending = c.finals
		if e.crashed("after-save") {
			return o
		}
		if av := e.ack(ctx, d, c.finals, &o); av.class == OK {
			o.Pending = nil
		} else {
			o.decide(StageAck, av)
		}
	}
	return e.done(d, o)
}

// ack runs relay ack with the given result lines.
func (e *Engine) ack(ctx context.Context, d Direction, results []string, o *Outcome) verdict {
	var b bytes.Buffer
	for _, r := range results {
		b.WriteString(r)
		b.WriteByte('\n')
	}
	_, v := e.stage(ctx, d, d.From, StageAck, b.Bytes(), o, "relay", "ack", "--peer", d.From.Peer)
	return v
}

// stage runs one pager command on side s and classifies how it ended.
func (e *Engine) stage(ctx context.Context, d Direction, s Side, stage string, stdin []byte, o *Outcome, args ...string) (pagercli.Output, verdict) {
	kind := s.Endpoint.Kind()
	out, err := e.Runner.Run(ctx, s.Endpoint, stdin, args...)
	var v verdict
	switch {
	case err != nil:
		v = verdict{NeedsHuman, CodeInvalidEndpoint, -1}
	case out.Over:
		// The Runner stopped the stage itself; the caller says what that means.
		v = verdict{Rerun, CodeInCap, out.Result.ExitCode}
	default:
		v = classify(ctx, stage, kind, out)
	}
	if reached(kind, out) {
		o.Contact = true
	}
	if e.Debug != nil && len(out.StderrHead) > 0 {
		fmt.Fprintf(e.Debug, "--- %s %s %s: stderr\n%s\n", d.Link, d.Name, stage, out.StderrHead)
	}
	level := slog.LevelDebug
	if v.class != OK {
		level = slog.LevelInfo
	}
	e.logger().Log(ctx, level, "stage", "link", d.Link, "direction", d.Name, "stage", stage,
		"exit", out.Result.ExitCode, "class", string(v.class), "code", v.code, "duration", out.Result.Duration)
	return out, v
}

// decide records v as the round's class when it is worse than what the round
// has so far. On a tie the earlier stage stays.
func (o *Outcome) decide(stage string, v verdict) { o.set(stage, v, worse(v.class, o.Class)) }

// decideOrTie is decide, except that v also wins a tie. A deferral's code
// must reach the state even beside an equally bad stage, or the same gap
// could never be seen twice in a row.
func (o *Outcome) decideOrTie(stage string, v verdict) { o.set(stage, v, !worse(o.Class, v.class)) }

func (o *Outcome) set(stage string, v verdict, take bool) {
	if take {
		o.Class, o.Code, o.Exit = v.class, v.code, v.exit
		o.Stage = ""
		if v.class != OK {
			o.Stage = stage
		}
	}
}

// schedule applies a deferral: the rate cap sets NotBefore; a gap backs off,
// and the same gap twice in a row means the receiver was rolled back.
func (e *Engine) schedule(o *Outcome, df deferral, prevCode string) {
	switch df.kind {
	case deferRate:
		// Clamp the milliseconds first: a huge value would overflow a Duration.
		ms := min(max(df.retryAfterMS, MinRateWait.Milliseconds()), MaxRateWait.Milliseconds())
		o.NotBefore = e.now().Add(time.Duration(ms) * time.Millisecond)
	case deferGap:
		code := fmt.Sprintf("gap-%d-%d", df.expected, df.got)
		if prevCode == code {
			o.decide(StageIn, verdict{NeedsHuman, CodeGapRepeated, 0})
		} else {
			o.decideOrTie(StageIn, verdict{Retry, code, 0})
		}
	case deferOther:
		o.decideOrTie(StageIn, verdict{Retry, CodeDeferred, 0})
	}
}

func (e *Engine) done(d Direction, o Outcome) Outcome {
	level := slog.LevelDebug
	if o.Class != Idle && o.Class != OK || o.Records > 0 {
		level = slog.LevelInfo
	}
	e.logger().Log(context.Background(), level, "round", "link", d.Link, "direction", d.Name,
		"class", string(o.Class), "code", o.Code, "stage", o.Stage, "exit", o.Exit,
		"records", o.Records, "finals", o.Finals, "dropped", o.Dropped, "pending", len(o.Pending))
	return o
}

func (e *Engine) crashed(point string) bool { return e.crash != nil && e.crash(point) }

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) logger() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.New(slog.DiscardHandler)
}

// splitLines returns the complete, non-blank lines of a stream, without their
// line endings, after up to four leading byte-order marks. partial reports a
// non-blank last line that lacks its LF.
func splitLines(b []byte) (lines [][]byte, partial bool) {
	for range 4 {
		b, _ = bytes.CutPrefix(b, []byte("\uFEFF"))
	}
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			return lines, len(bytes.TrimSpace(b)) > 0
		}
		line := bytes.TrimSuffix(b[:i], []byte("\r"))
		if len(bytes.TrimSpace(line)) > 0 {
			lines = append(lines, line)
		}
		b = b[i+1:]
	}
	return lines, false
}

// key identifies a record, and the result that answers it.
type key struct {
	store, link string
	seq, origin int64
}

type record struct {
	raw []byte
	key key
}

// parseRecords reads relay out's lines. ok is false when a line is not a
// record, which means something other than pager wrote to the stream.
func parseRecords(lines [][]byte) (recs []record, ok bool) {
	for _, l := range lines {
		var r struct {
			Kind string `json:"kind"`
			Link struct {
				Store string `json:"store"`
				ID    string `json:"id"`
				Seq   int64  `json:"seq"`
			} `json:"link"`
			Origin struct {
				ID int64 `json:"id"`
			} `json:"origin"`
		}
		if json.Unmarshal(l, &r) != nil || r.Kind == "" || r.Link.ID == "" || r.Link.Seq < 1 {
			return nil, false
		}
		recs = append(recs, record{raw: l, key: key{r.Link.Store, r.Link.ID, r.Link.Seq, r.Origin.ID}})
	}
	return recs, true
}

func joinLines(recs []record) []byte {
	var b bytes.Buffer
	for _, r := range recs {
		b.Write(r.raw)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

type deferKind int

const (
	deferNone deferKind = iota
	deferRate
	deferGap
	deferOther
)

type deferral struct {
	kind          deferKind
	expected, got int64
	retryAfterMS  int64
}

type correlation struct {
	finals   []string // final results answering a staged record, verbatim
	dropped  int      // results that answered nothing staged
	deferral deferral // the first deferral, which stops the run
	ok       bool     // every line was a result
}

// correlate keeps the results that answer a record staged this round. A
// result for anything else — an earlier round, another link — is dropped:
// relay ack must see only what this round can vouch for.
func correlate(recs []record, lines [][]byte) correlation {
	staged := make(map[key]bool, len(recs))
	for _, r := range recs {
		staged[r.key] = true
	}
	c := correlation{ok: true}
	for _, l := range lines {
		var r struct {
			Kind string `json:"kind"`
			Link struct {
				Store string `json:"store"`
				ID    string `json:"id"`
				Seq   int64  `json:"seq"`
			} `json:"link"`
			OriginID     int64  `json:"origin_id"`
			Status       string `json:"status"`
			Reason       string `json:"reason"`
			RetryAfterMS int64  `json:"retry_after_ms"`
		}
		if json.Unmarshal(l, &r) != nil || r.Kind != "result" {
			c.ok = false
			continue
		}
		if !staged[key{r.Link.Store, r.Link.ID, r.Link.Seq, r.OriginID}] {
			c.dropped++
			continue
		}
		switch r.Status {
		case "imported", "duplicate", "rejected":
			c.finals = append(c.finals, string(l))
		case "deferred":
			if c.deferral.kind == deferNone {
				c.deferral = parseDeferral(r.Reason, r.RetryAfterMS)
			}
		}
	}
	return c
}

func parseDeferral(reason string, retryAfterMS int64) deferral {
	var exp, got int64
	switch {
	case strings.HasPrefix(reason, "rate:"):
		return deferral{kind: deferRate, retryAfterMS: retryAfterMS}
	case strings.HasPrefix(reason, "gap:"):
		if _, err := fmt.Sscanf(reason, "gap: expected %d, got %d", &exp, &got); err == nil {
			return deferral{kind: deferGap, expected: exp, got: got}
		}
	}
	return deferral{kind: deferOther}
}
