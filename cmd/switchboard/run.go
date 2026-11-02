package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/iMUngHee/switchboard/internal/link"
	"github.com/iMUngHee/switchboard/internal/notify"
	"github.com/iMUngHee/switchboard/internal/proc"
)

// cmdRun carries mail on every configured link until SIGINT or SIGTERM. It
// then starts no new round, gives running ones the shutdown grace, saves the
// state, releases the locks and exits 0.
func cmdRun(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfgFlag := fs.String("config", "", "configuration file")
	grace := fs.Duration("shutdown-grace", link.DefaultGrace, "how long running rounds may finish once stopping; 0 stops them at once")
	logPath := fs.String("log", "", "write the log to this file, moved aside at 10 MiB with 3 files kept, instead of stderr")
	pos, code, ok := flags(fs, args)
	if !ok {
		return code
	}
	if len(pos) > 0 || *grace < 0 {
		fmt.Fprintln(stderr, "usage: switchboard run [--config FILE] [--shutdown-grace DURATION] [--log FILE]")
		return 2
	}
	// On Windows the daemon puts itself in a job first, so no stage it
	// starts can outlive it, however it ends.
	if err := proc.ContainSelf(); err != nil {
		return fail(stderr, err)
	}
	installTestKill()
	a, err := load(*cfgFlag, stderr)
	if err != nil {
		return fail(stderr, err)
	}
	names, _ := a.links(nil)
	if len(names) == 0 {
		return fail(stderr, fmt.Errorf("%s configures no links", a.cfgPath))
	}

	var out io.Writer = stderr
	if *logPath != "" {
		lf, err := openLog(*logPath)
		if err != nil {
			return fail(stderr, err)
		}
		defer lf.Close()
		out = lf
	}
	log := slog.New(slog.NewTextHandler(out, nil))
	signals, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	ctx, cancel := context.WithCancel(signals)
	defer cancel()

	sem := make(chan struct{}, maxRounds)
	notifier := notify.New(a.cfg.Notify.Command, log)
	defer notifier.Close() // after every link has stopped: it sends what is queued
	e := a.engine(log, nil)
	g := max(*grace, time.Nanosecond) // the link reads zero as its default
	var wg sync.WaitGroup
	for _, n := range names {
		opt := a.options(n, e, log)
		opt.Sem, opt.Grace, opt.Notifier = sem, g, notifier
		l, err := link.Open(ctx, opt)
		switch {
		case errors.Is(err, link.ErrDriven):
			// A once or status may hold it for a moment, or a once against
			// an unreachable far side for minutes: wait, the others run.
			log.Warn("link held by another switchboard process; waiting for it", "link", n)
			wg.Go(func() {
				if l := waitOpen(ctx, opt); l != nil {
					defer l.Close()
					log.Info("link open", "link", n)
					l.Run(ctx)
				}
			})
		case err != nil:
			cancel()
			wg.Wait()
			if signals.Err() != nil {
				return 0 // stopped while starting
			}
			return fail(stderr, err)
		default:
			log.Info("link open", "link", n)
			wg.Go(func() {
				defer l.Close()
				l.Run(ctx)
			})
		}
	}
	log.Info("switchboard running", "version", version, "config", a.cfgPath, "links", len(names))
	wg.Wait()
	log.Info("switchboard stopped")
	return 0
}

// waitOpen opens a link whose lock another process holds, once that process
// lets go, retrying every second at first and every 30 seconds at most. It
// never gives up: a daemon that let a link go would run on without it, or,
// with no other link, exit as if stopped, and no service manager restarts a
// clean exit. Other errors while waiting (a pager being upgraded, say) are
// logged once each and retried too. It returns nil only when ctx ends.
func waitOpen(ctx context.Context, opt link.Options) *link.Link {
	wait, last := time.Second, ""
	for {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil
		}
		l, err := link.Open(ctx, opt)
		switch {
		case err == nil:
			return l
		case ctx.Err() != nil:
			return nil // stopping: not the link's fault
		case !errors.Is(err, link.ErrDriven) && err.Error() != last:
			last = err.Error()
			opt.Log.Warn("link cannot open yet; retrying", "link", opt.Name, "err", err)
		}
		wait = min(2*wait, 30*time.Second)
	}
}
