// Package config reads switchboard's TOML configuration.
//
// It is strict on purpose: a key it does not know is an error, never something
// to skip, because a misspelt poll interval or transport would otherwise run
// quietly on its default. Every error names the file and the TOML key it is
// about.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"

	"github.com/iMUngHee/switchboard/internal/transport"
)

// Version is the configuration schema this release reads. New optional keys
// with defaults need no bump; a key never changes meaning within a version.
const Version = 1

// Poll intervals: the defaults, and the shortest each direction accepts.
const (
	DefaultPollOutbound = 5 * time.Second
	MinPollOutbound     = time.Second
	DefaultPollInbound  = 30 * time.Second
	MinPollInbound      = 5 * time.Second
)

// Config is a validated configuration.
type Config struct {
	Version int
	Pager   Pager
	Links   map[string]Link // key = this store's peer name for the far store
	Notify  Notify
}

// Pager says which pager binary this side runs, and which store. DB is empty
// unless set.
type Pager struct{ Path, DB string }

// Link is one far store and how to reach it. Exactly one of SSH and Exec is
// set.
type Link struct {
	Peer         string // this store's peer name for the far store, from the table key
	RemotePeer   string // the far store's peer name for this store
	PollOutbound time.Duration
	PollInbound  time.Duration
	Hint         string // what a person can do when the link needs one
	SSH          *SSH
	Exec         *Exec
}

// SSH reaches the far side with the ssh preset. Pager is the far side's pager.
type SSH struct {
	Host    string
	Pager   string
	Options []string // placed before the preset's own options
}

// Exec reaches the far side with any command that runs pager there; Argv is
// that command, pager included. Env is applied to the local launcher process.
type Exec struct {
	Argv []string
	Env  map[string]string
}

// Notify is where notifications go. An empty Command means the OS default.
type Notify struct{ Command []string }

// The document as decoded. Tables are structs, so DisallowUnknownFields can
// reject a key it does not know at any depth; values are left untyped and
// checked by build, so a value of the wrong type is reported under its TOML
// key rather than as the Go field it was being decoded into.
type file struct {
	Version any                  `toml:"version"`
	Pager   *pagerTable          `toml:"pager"`
	Link    map[string]linkTable `toml:"link"`
	Notify  *notifyTable         `toml:"notify"`
}

type pagerTable struct {
	Path any `toml:"path"`
	DB   any `toml:"db"`
}

type linkTable struct {
	RemotePeer   any        `toml:"remote_peer"`
	PollOutbound any        `toml:"poll_outbound"`
	PollInbound  any        `toml:"poll_inbound"`
	Hint         any        `toml:"hint"`
	SSH          *sshTable  `toml:"ssh"`
	Exec         *execTable `toml:"exec"`
}

type sshTable struct {
	Host    any `toml:"host"`
	Pager   any `toml:"pager"`
	Options any `toml:"options"`
}

type execTable struct {
	Argv any `toml:"argv"`
	Env  any `toml:"env"`
}

type notifyTable struct {
	Command any `toml:"command"`
}

// peerName is pager's rule for a peer name. Upper case is refused rather than
// folded: pager would fold it, and the config would then name a peer other
// than the one pager lists.
var peerName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// Load reads and validates the configuration file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data, path)
}

// Parse validates a configuration. path is used only in messages.
func Parse(data []byte, path string) (*Config, error) {
	// A generic decode first: it reports syntax errors on its own, and it lets
	// the version be judged before anything else. A config for a newer release
	// is bound to have keys this one does not know, and saying so would hide
	// the real cause.
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, decodeError(path, err)
	}
	if err := checkVersion(path, doc["version"]); err != nil {
		return nil, err
	}
	if err := checkTables(path, doc); err != nil {
		return nil, err
	}

	var raw file
	dec := toml.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return nil, decodeError(path, err)
	}
	return build(path, &raw)
}

func checkVersion(path string, v any) error {
	switch v := v.(type) {
	case nil:
		return fmt.Errorf("%s: version: is required; this release reads version %d", path, Version)
	case int64:
		if v != Version {
			return fmt.Errorf("%s: version: %d is not one this release reads (%d); the config may be for a newer release", path, v, Version)
		}
		return nil
	default:
		return fmt.Errorf("%s: version: must be an integer, not %s", path, kind(v))
	}
}

// checkTables makes sure every key that must hold a table does, so the
// strict decode after it can fail only on unknown keys.
func checkTables(path string, doc map[string]any) error {
	var errs []error
	table := func(key string, v any) map[string]any {
		if v == nil {
			return nil
		}
		m, ok := v.(map[string]any)
		if !ok {
			errs = append(errs, fmt.Errorf("%s: %s: must be a table, not %s", path, key, kind(v)))
		}
		return m
	}
	table("pager", doc["pager"])
	table("notify", doc["notify"])
	links := table("link", doc["link"])
	for _, name := range slices.Sorted(maps.Keys(links)) {
		if l := table("link."+name, links[name]); l != nil {
			table("link."+name+".ssh", l["ssh"])
			table("link."+name+".exec", l["exec"])
		}
	}
	return errors.Join(errs...)
}

// decodeError names the keys go-toml could not place, or the position of a
// syntax error.
func decodeError(path string, err error) error {
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		errs := make([]error, len(strict.Errors))
		for i, e := range strict.Errors {
			row, _ := e.Position()
			errs[i] = fmt.Errorf("%s: %s: unknown key (line %d)", path, strings.Join(e.Key(), "."), row)
		}
		return errors.Join(errs...)
	}
	var de *toml.DecodeError
	if errors.As(err, &de) {
		row, col := de.Position()
		return fmt.Errorf("%s:%d:%d: %s", path, row, col, strings.TrimPrefix(de.Error(), "toml: "))
	}
	return fmt.Errorf("%s: %w", path, err)
}

// checker collects every problem in a document, so one run reports them all.
type checker struct {
	path string
	errs []error
}

func (c *checker) fail(key, format string, args ...any) {
	c.errs = append(c.errs, fmt.Errorf("%s: %s: %s", c.path, key, fmt.Sprintf(format, args...)))
}

// str returns v as a string. ok is false when v is absent or not a string;
// the second is recorded.
func (c *checker) str(key string, v any) (s string, ok bool) {
	if v == nil {
		return "", false
	}
	if s, ok = v.(string); !ok {
		c.fail(key, "must be a string, not %s", kind(v))
	}
	return s, ok
}

// requiredStr is str for a key that must be present.
func (c *checker) requiredStr(key string, v any) (string, bool) {
	if v == nil {
		c.fail(key, "is required")
		return "", false
	}
	return c.str(key, v)
}

// strs returns v as an array of strings; ok is as for str. A present but
// empty array is non-nil.
func (c *checker) strs(key string, v any) ([]string, bool) {
	if v == nil {
		return nil, false
	}
	arr, ok := v.([]any)
	if !ok {
		c.fail(key, "must be an array of strings, not %s", kind(v))
		return nil, false
	}
	out := make([]string, len(arr))
	for i, e := range arr {
		s, ok := e.(string)
		if !ok {
			c.fail(fmt.Sprintf("%s[%d]", key, i), "must be a string, not %s", kind(e))
			return nil, false
		}
		out[i] = s
	}
	return out, true
}

// duration parses a Go duration string such as "5s", falling back to def
// when the key is absent.
func (c *checker) duration(key string, v any, def, minimum time.Duration) time.Duration {
	s, ok := c.str(key, v)
	if !ok {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		c.fail(key, "%q is not a duration such as %q", s, def.String())
		return def
	}
	if d < minimum {
		c.fail(key, "%s is shorter than the minimum, %s", d, minimum)
		return def
	}
	return d
}

func build(path string, raw *file) (*Config, error) {
	c := &checker{path: path}
	cfg := &Config{Version: Version, Links: map[string]Link{}}

	var p pagerTable
	if raw.Pager != nil {
		p = *raw.Pager
	}
	if s, ok := c.requiredStr("pager.path", p.Path); ok {
		if !filepath.IsAbs(s) {
			c.fail("pager.path", "must be an absolute path, not %q", s)
		}
		cfg.Pager.Path = s
	}
	// A relative store path would be relative to wherever a service manager
	// started the daemon.
	if s, ok := c.str("pager.db", p.DB); ok {
		if !filepath.IsAbs(s) {
			c.fail("pager.db", "must be an absolute path, not %q", s)
		}
		cfg.Pager.DB = s
	}

	for _, name := range slices.Sorted(maps.Keys(raw.Link)) {
		cfg.Links[name] = c.link(name, raw.Link[name])
	}

	if raw.Notify != nil {
		if cmd, ok := c.strs("notify.command", raw.Notify.Command); ok {
			if len(cmd) == 0 || cmd[0] == "" {
				c.fail("notify.command", "must name a program")
			}
			cfg.Notify.Command = cmd
		}
	}

	if len(c.errs) > 0 {
		return nil, errors.Join(c.errs...)
	}
	return cfg, nil
}

func (c *checker) link(name string, t linkTable) Link {
	key := "link." + name
	l := Link{Peer: name}
	if !peerName.MatchString(name) {
		c.fail(key, "%q is not a peer name: 1-32 of a-z, 0-9 and '-', not starting with '-'", name)
	}
	if s, ok := c.requiredStr(key+".remote_peer", t.RemotePeer); ok {
		if !peerName.MatchString(s) {
			c.fail(key+".remote_peer", "%q is not a peer name: 1-32 of a-z, 0-9 and '-', not starting with '-'", s)
		}
		l.RemotePeer = s
	}
	l.PollOutbound = c.duration(key+".poll_outbound", t.PollOutbound, DefaultPollOutbound, MinPollOutbound)
	l.PollInbound = c.duration(key+".poll_inbound", t.PollInbound, DefaultPollInbound, MinPollInbound)
	l.Hint, _ = c.str(key+".hint", t.Hint)

	switch {
	case t.SSH != nil && t.Exec != nil:
		c.fail(key, "has both an ssh and an exec table; give exactly one")
	case t.SSH == nil && t.Exec == nil:
		c.fail(key, "needs an ssh or an exec table")
	case t.SSH != nil:
		l.SSH = c.ssh(key+".ssh", t.SSH)
		if l.Hint == "" && l.SSH.Host != "" {
			l.Hint = "run `ssh " + l.SSH.Host + "` once"
		}
	default:
		l.Exec = c.exec(key+".exec", t.Exec)
	}
	return l
}

func (c *checker) ssh(key string, t *sshTable) *SSH {
	s := &SSH{}
	// The transport's own checks, so what loads is what the ssh preset runs:
	// the pager path goes into the remote command line unquoted.
	if h, ok := c.requiredStr(key+".host", t.Host); ok {
		if err := transport.ValidateHost(h); err != nil {
			c.fail(key+".host", "%v", err)
		}
		s.Host = h
	}
	if p, ok := c.requiredStr(key+".pager", t.Pager); ok {
		if err := transport.ValidateRemotePager(p); err != nil {
			c.fail(key+".pager", "%v", err)
		}
		s.Pager = p
	}
	s.Options, _ = c.strs(key+".options", t.Options)
	return s
}

func (c *checker) exec(key string, t *execTable) *Exec {
	e := &Exec{}
	if t.Argv == nil {
		c.fail(key+".argv", "is required")
	} else if argv, ok := c.strs(key+".argv", t.Argv); ok {
		if len(argv) == 0 || argv[0] == "" {
			c.fail(key+".argv", "must name a program")
		}
		e.Argv = argv
	}
	switch env := t.Env.(type) {
	case nil:
	case map[string]any:
		e.Env = make(map[string]string, len(env))
		for _, name := range slices.Sorted(maps.Keys(env)) {
			v, ok := env[name].(string)
			switch {
			case !ok:
				c.fail(key+".env."+name, "must be a string, not %s", kind(env[name]))
			case name == "" || strings.Contains(name, "="):
				c.fail(key+".env", "%q is not an environment variable name", name)
			default:
				e.Env[name] = v
			}
		}
	default:
		c.fail(key+".env", "must be a table of strings, not %s", kind(env))
	}
	return e
}

// kind names a decoded TOML value's type for a message.
func kind(v any) string {
	switch v.(type) {
	case string:
		return "a string"
	case int64:
		return "an integer"
	case float64:
		return "a float"
	case bool:
		return "a boolean"
	case []any:
		return "an array"
	case map[string]any:
		return "a table"
	default:
		return "a date or time"
	}
}
