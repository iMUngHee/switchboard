// Package pagercli runs the pager subcommands switchboard needs on an
// endpoint and reads their output.
//
// What a stage wrote to stderr is kept only for classification (the first
// 64 KiB) and never leaves this process: pager's stderr can name recipients.
package pagercli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/iMUngHee/switchboard/internal/proc"
	"github.com/iMUngHee/switchboard/internal/transport"
)

const (
	// DefaultStdoutCap bounds one stage's stdout. A relay out batch of the
	// default 100 records fits with room to spare.
	DefaultStdoutCap = 64 << 20
	// StderrKept is how much stderr is kept for classification. The rest is
	// still read, so a chatty stage never blocks on a full pipe.
	StderrKept = 64 << 10
)

// Runner runs pager subcommands with a deadline and an output cap.
type Runner struct {
	Timeout   time.Duration // per invocation; zero means no deadline of its own
	StdoutCap int           // zero means DefaultStdoutCap
}

// Output is one finished invocation.
type Output struct {
	Stdout     []byte
	StderrHead []byte // classification only; never log or persist it
	Over       bool   // stdout passed the cap and the stage was stopped
	Result     proc.Result
}

// Run runs `pager <args>` on the endpoint with stdin (nil for none).
func (r Runner) Run(ctx context.Context, ep transport.Endpoint, stdin []byte, args ...string) (Output, error) {
	if err := ep.Validate(); err != nil {
		return Output{}, err
	}
	if err := transport.ValidateArgs(args); err != nil {
		return Output{}, err
	}
	limit := r.StdoutCap
	if limit <= 0 {
		limit = DefaultStdoutCap
	}
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	out := &capped{limit: limit, over: stop}
	errb := &head{limit: StderrKept}
	c := ep.Command(args...)
	spec := proc.Spec{Path: c.Path, Args: c.Args, Env: c.Env, Stdout: out, Stderr: errb}
	if stdin != nil {
		spec.Stdin = bytes.NewReader(stdin)
	}
	res := proc.Run(ctx, spec)
	return Output{Stdout: out.buf.Bytes(), StderrHead: errb.buf.Bytes(), Over: out.exceeded, Result: res}, nil
}

// capped keeps up to limit bytes and calls over once when more arrive.
type capped struct {
	buf      bytes.Buffer
	limit    int
	exceeded bool
	over     func()
}

func (c *capped) Write(p []byte) (int, error) {
	if c.exceeded {
		return len(p), nil
	}
	if room := c.limit - c.buf.Len(); len(p) > room {
		c.buf.Write(p[:max(room, 0)])
		c.exceeded = true
		c.over()
		return len(p), nil
	}
	return c.buf.Write(p)
}

// head keeps the first limit bytes and discards the rest.
type head struct {
	buf   bytes.Buffer
	limit int
}

func (h *head) Write(p []byte) (int, error) {
	if room := h.limit - h.buf.Len(); room > 0 {
		h.buf.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

var storeID = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ParseRelayID reads `pager relay id`: exactly one line holding a store id.
// Anything else — notably a remote shell printing a banner first — is an
// error, because the same noise would corrupt every relay stream.
func ParseRelayID(stdout []byte) (string, error) {
	text := strings.TrimRight(string(stdout), "\r\n")
	if strings.ContainsAny(text, "\r\n") {
		return "", fmt.Errorf("relay id printed %d lines, want one", strings.Count(text, "\n")+1)
	}
	if !storeID.MatchString(text) {
		return "", errors.New("relay id did not print a store id")
	}
	return text, nil
}

// Peer is one row of `pager peer ls`.
type Peer struct {
	Name    string
	Store   string
	Pending int
	Seq     int
}

// ParsePeerList reads the table `pager peer ls` prints: a header of NAME,
// STORE, PENDING and SEQ, then one row per peer; or a single "no peers" line.
func ParsePeerList(stdout []byte) ([]Peer, error) {
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(string(stdout), "\r\n", "\n"), "\n"), "\n")
	if len(lines) == 1 && strings.HasPrefix(lines[0], "no peers") {
		return nil, nil
	}
	head := strings.Fields(lines[0])
	if len(head) != 4 || head[0] != "NAME" || head[1] != "STORE" || head[2] != "PENDING" || (head[3] != "SEQ" && head[3] != "SENT") {
		return nil, fmt.Errorf("peer ls: unexpected header %q", lines[0])
	}
	var peers []Peer
	for _, l := range lines[1:] {
		f := strings.Fields(l)
		if len(f) != 4 || !storeID.MatchString(f[1]) {
			return nil, fmt.Errorf("peer ls: unexpected row %q", l)
		}
		pending, err1 := strconv.Atoi(f[2])
		seq, err2 := strconv.Atoi(f[3])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("peer ls: unexpected row %q", l)
		}
		peers = append(peers, Peer{Name: f[0], Store: f[1], Pending: pending, Seq: seq})
	}
	return peers, nil
}
