package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync/atomic"
	"time"

	"github.com/iMUngHee/switchboard/internal/config"
	"github.com/iMUngHee/switchboard/internal/link"
	"github.com/iMUngHee/switchboard/internal/pagercli"
	"github.com/iMUngHee/switchboard/internal/paths"
	"github.com/iMUngHee/switchboard/internal/proc"
	"github.com/iMUngHee/switchboard/internal/round"
	"github.com/iMUngHee/switchboard/internal/state"
	"github.com/iMUngHee/switchboard/internal/transport"
)

const (
	// stageTimeout bounds one pager command on either side.
	stageTimeout = 2 * time.Minute
	// maxRounds bounds how many rounds run at once across all links.
	maxRounds = 4
)

// app is what every command starts from: the configuration it read, where,
// and the state directory.
type app struct {
	cfg      *config.Config
	cfgPath  string
	stateDir string
}

// load reads the configuration named by the --config value, or the default
// one, and says on stderr which file it read.
func load(flagValue string, stderr io.Writer) (*app, error) {
	path, source, err := paths.ConfigFile(flagValue)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(stderr, "config: %s (%s)\n", path, source)
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	dir, err := paths.StateDir()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &app{cfg: cfg, cfgPath: path, stateDir: dir}, nil
}

// links returns the named links, or every configured link, in order.
func (a *app) links(names []string) ([]string, error) {
	if len(names) == 0 {
		return slices.Sorted(maps.Keys(a.cfg.Links)), nil
	}
	for _, n := range names {
		if _, ok := a.cfg.Links[n]; !ok {
			return nil, fmt.Errorf("%s has no link %q", a.cfgPath, n)
		}
	}
	return names, nil
}

func (a *app) runner() pagercli.Runner { return pagercli.Runner{Timeout: stageTimeout} }

func (a *app) engine(log *slog.Logger, debug io.Writer) round.Engine {
	return round.Engine{Runner: a.runner(), Log: log, Debug: debug, Crash: testCrash()}
}

func (a *app) options(name string, e round.Engine, log *slog.Logger) link.Options {
	return link.Options{Name: name, Config: a.cfg.Links[name], Pager: a.cfg.Pager, StateDir: a.stateDir, Engine: e, Log: log}
}

func (a *app) local() transport.LocalEndpoint {
	return transport.LocalEndpoint{Pager: a.cfg.Pager.Path, DB: a.cfg.Pager.DB}
}

// key returns a link's identity key, which needs this store's id.
func (a *app) key(ctx context.Context, name string) (string, error) {
	id, err := link.LocalStoreID(ctx, a.runner(), a.cfg.Pager)
	if err != nil {
		return "", fmt.Errorf("this store: %w", err)
	}
	return state.Key(id, name), nil
}

// flags parses a command's flags wherever they stand among its arguments,
// so `status vm --json` works like `status --json vm`, and returns the
// others. When ok is false the command exits with code: 0 after -h, 2 after
// a usage mistake, which the flag package has already described.
func flags(fs *flag.FlagSet, args []string) (pos []string, code int, ok bool) {
	for {
		err := fs.Parse(args)
		switch {
		case errors.Is(err, flag.ErrHelp):
			return nil, 0, false
		case err != nil:
			return nil, 2, false
		}
		args = fs.Args()
		if len(args) == 0 {
			return pos, 0, true
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

func fail(stderr io.Writer, err error) int {
	fmt.Fprintln(stderr, "switchboard:", err)
	return 1
}

// The forced-end tests end switchboard at a named point, the way Task
// Scheduler ending the task would: SWITCHBOARD_TEST_KILL names it. "created"
// is right after the second process switchboard starts is created, before
// it is placed in its own job: the first stage of a round, once the link's
// lock is taken (the first is relay id, before it). "before-save" and
// "after-save" are either side of storing a round's results. Unset, as it
// always is outside those tests, it does nothing.
const testKillEnv = "SWITCHBOARD_TEST_KILL"

func testCrash() func(string) bool {
	point := os.Getenv(testKillEnv)
	if point == "" {
		return nil
	}
	return func(p string) bool {
		if p == point {
			die()
		}
		return false
	}
}

func installTestKill() {
	if os.Getenv(testKillEnv) == "created" {
		var n atomic.Int32
		proc.AfterCreate = func() {
			if n.Add(1) == 2 {
				die()
			}
		}
	}
}

// die ends the process at once, running nothing on the way out.
func die() { os.Exit(137) }
