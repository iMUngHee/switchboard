package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/iMUngHee/switchboard/internal/link"
	"github.com/iMUngHee/switchboard/internal/round"
	"github.com/iMUngHee/switchboard/internal/state"
)

var directionNames = [2]string{"outbound", "inbound"}

// cmdOnce runs one round each way on the named links, or on all of them, in
// this process. A link a running switchboard holds is asked to run instead.
func cmdOnce(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("once", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "configuration file")
	debug := fs.Bool("debug", false, "print each stage's stderr to this terminal")
	pos, code, ok := flags(fs, args)
	if !ok {
		return code
	}
	installTestKill()
	a, err := load(*cfgFlag, stderr)
	if err != nil {
		return fail(stderr, err)
	}
	names, err := a.links(pos)
	if err != nil {
		return fail(stderr, err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	var dbg io.Writer
	if *debug {
		dbg = stderr
	}
	e := a.engine(log, dbg)
	for _, n := range names {
		l, err := link.Open(ctx, a.options(n, e, log))
		if errors.Is(err, link.ErrDriven) {
			if err := a.request(ctx, n, state.KindOnce); err != nil {
				code = fail(stderr, err)
				continue
			}
			fmt.Fprintf(stdout, "%s: another switchboard process holds it; asked it to run within a second\n", n)
			continue
		}
		if err != nil {
			code = fail(stderr, err)
			continue
		}
		outcomes := l.Once(ctx)
		l.Close()
		for i, o := range outcomes {
			fmt.Fprintf(stdout, "%s %s: %s\n", n, directionNames[i], describe(n, o))
			if o.Class != round.Idle && o.Class != round.OK {
				code = 1
			}
		}
	}
	return code
}

func describe(name string, o round.Outcome) string {
	s := string(o.Class)
	if o.Code != "" {
		s += " (" + o.Code + ")"
	}
	if o.Records > 0 {
		s += fmt.Sprintf(", %d records, %d with a final result", o.Records, o.Finals)
	}
	if !o.NotBefore.IsZero() {
		s += ", next not before " + o.NotBefore.Local().Format(time.DateTime)
	}
	if o.Class == round.NeedsHuman {
		s += fmt.Sprintf("; fix the cause, then run `switchboard resume %s`", name)
	}
	return s
}

func (a *app) request(ctx context.Context, name, kind string) error {
	key, err := a.key(ctx, name)
	if err != nil {
		return err
	}
	return state.Request(a.stateDir, key, kind)
}

// cmdCheck verifies that both stores of each link enrolled each other.
func cmdCheck(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "configuration file")
	pos, code, ok := flags(fs, args)
	if !ok {
		return code
	}
	a, err := load(*cfgFlag, stderr)
	if err != nil {
		return fail(stderr, err)
	}
	names, err := a.links(pos)
	if err != nil {
		return fail(stderr, err)
	}
	for _, n := range names {
		cfg := a.cfg.Links[n]
		far, err := link.FarEndpoint(cfg, "")
		if err != nil {
			code = fail(stderr, fmt.Errorf("link %s: %w", n, err))
			continue
		}
		res := link.Check(context.Background(), a.runner(), a.local(), far, cfg.Peer, cfg.RemotePeer)
		if res.OK() {
			fmt.Fprintf(stdout, "%s: ok (this store %s, far store %s)\n", n, res.LocalStore, res.RemoteStore)
			continue
		}
		code = 1
		for _, p := range res.Problems {
			fmt.Fprintf(stdout, "%s: %s\n", n, p)
		}
	}
	return code
}

type directionStatus struct {
	Class       string     `json:"class"`
	Code        string     `json:"code,omitempty"`
	Pending     int        `json:"pending_ack"`
	Failures    int        `json:"consecutive_failures"`
	LastContact *time.Time `json:"last_contact,omitempty"`
	LastSuccess *time.Time `json:"last_success,omitempty"`
	NotBefore   *time.Time `json:"not_before,omitempty"`
}

type linkStatus struct {
	Name       string          `json:"name"`
	Key        string          `json:"key"`
	Daemon     bool            `json:"daemon"` // a running switchboard holds the link
	Stale      bool            `json:"stale"`  // no daemon, and nothing heard for two poll intervals
	NeedsHuman bool            `json:"needs_human"`
	Outbound   directionStatus `json:"outbound"`
	Inbound    directionStatus `json:"inbound"`
}

// cmdStatus reports each link's stored state and whether a daemon holds it.
func cmdStatus(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "configuration file")
	asJSON := fs.Bool("json", false, "print JSON")
	pos, code, ok := flags(fs, args)
	if !ok {
		return code
	}
	a, err := load(*cfgFlag, stderr)
	if err != nil {
		return fail(stderr, err)
	}
	names, err := a.links(pos)
	if err != nil {
		return fail(stderr, err)
	}
	id, err := link.LocalStoreID(context.Background(), a.runner(), a.cfg.Pager)
	if err != nil {
		return fail(stderr, fmt.Errorf("this store: %w", err))
	}
	now := time.Now()
	out := []linkStatus{}
	for _, n := range names {
		key := state.Key(id, n)
		st, err := state.Load(a.stateDir, key)
		if err != nil {
			return fail(stderr, err)
		}
		held, err := lockHeld(a.stateDir, key)
		if err != nil {
			return fail(stderr, err)
		}
		cfg := a.cfg.Links[n]
		heard := latest(st.Outbound.LastContact, st.Inbound.LastContact, st.Outbound.LastSuccess, st.Inbound.LastSuccess)
		out = append(out, linkStatus{
			Name: n, Key: key, Daemon: held, NeedsHuman: st.NeedsHuman,
			Stale:    !held && (heard.IsZero() || now.Sub(heard) > 2*max(cfg.PollOutbound, cfg.PollInbound)),
			Outbound: dirStatus(st.Outbound), Inbound: dirStatus(st.Inbound),
		})
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			Config   string       `json:"config"`
			StateDir string       `json:"state_dir"`
			Links    []linkStatus `json:"links"`
		}{a.cfgPath, a.stateDir, out}); err != nil {
			return fail(stderr, err)
		}
		return 0
	}
	for _, l := range out {
		head := "no daemon"
		if l.Daemon {
			head = "daemon running"
		}
		if l.Stale {
			head += ", stale"
		}
		if l.NeedsHuman {
			head += fmt.Sprintf(", stopped: fix the cause, then run `switchboard resume %s`", l.Name)
		}
		fmt.Fprintf(stdout, "%s: %s\n", l.Name, head)
		w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
		for i, d := range []directionStatus{l.Outbound, l.Inbound} {
			class := d.Class
			if class == "" {
				class = "never run"
			}
			if d.Code != "" {
				class += " (" + d.Code + ")"
			}
			fmt.Fprintf(w, "  %s\t%s\tpending %d\tcontact %s\tdelivered %s\tnext %s\n",
				directionNames[i], class, d.Pending, when(d.LastContact), when(d.LastSuccess), when(d.NotBefore))
		}
		w.Flush()
	}
	return 0
}

func dirStatus(d state.Direction) directionStatus {
	opt := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		return &t
	}
	return directionStatus{Class: d.Class, Code: d.Code, Pending: len(d.PendingAck), Failures: d.ConsecutiveFailures,
		LastContact: opt(d.LastContact), LastSuccess: opt(d.LastSuccess), NotBefore: opt(d.NotBefore)}
}

func when(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format(time.DateTime)
}

func latest(ts ...time.Time) time.Time {
	var m time.Time
	for _, t := range ts {
		if t.After(m) {
			m = t
		}
	}
	return m
}

// lockHeld reports whether a process holds the link's lock. It takes the lock
// for a moment to find out.
func lockHeld(dir, key string) (bool, error) {
	l, err := state.TryLock(dir, key)
	if errors.Is(err, state.ErrLocked) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, l.Close()
}

// cmdResume clears a stopped link's needs-human mark and backoff. A running
// switchboard holding it is asked to, and does within a second.
func cmdResume(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("resume", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "configuration file")
	pos, code, ok := flags(fs, args)
	if !ok {
		return code
	}
	if len(pos) != 1 {
		fmt.Fprintln(stderr, "usage: switchboard resume [--config FILE] LINK")
		return 2
	}
	a, err := load(*cfgFlag, stderr)
	if err != nil {
		return fail(stderr, err)
	}
	name := pos[0]
	if _, err := a.links([]string{name}); err != nil {
		return fail(stderr, err)
	}
	key, err := a.key(context.Background(), name)
	if err != nil {
		return fail(stderr, err)
	}
	queued, err := state.Resume(a.stateDir, key)
	if err != nil {
		return fail(stderr, err)
	}
	if queued {
		fmt.Fprintf(stdout, "%s: another switchboard process holds it; asked it to resume within a second\n", name)
	} else {
		fmt.Fprintf(stdout, "%s: cleared; the next run or once carries it again\n", name)
	}
	return 0
}
