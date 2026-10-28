// Package transport says how to run pager on each side of a link.
//
// An Endpoint turns "pager <args>" into a process to start: the local pager
// directly, a remote one through ssh, or one reached by any other command
// (docker exec, wsl.exe, …). Nothing here runs a shell on this machine; what
// reaches the far side is an argv, or, for ssh, one command string built only
// from characters no shell treats specially.
package transport

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Kind tells the round engine which failures can belong to the transport
// rather than to pager.
type Kind int

const (
	Local Kind = iota // pager on this machine; every failure is pager's
	SSH               // exit 255 is ssh's own
	Exec              // the launcher's own codes are unknown in general
)

func (k Kind) String() string {
	switch k {
	case Local:
		return "local"
	case SSH:
		return "ssh"
	default:
		return "exec"
	}
}

// Command is a process to start: Path plus Args (argv[1:]) and Env (the full
// environment; nil inherits this process's).
type Command struct {
	Path string
	Args []string
	Env  []string
}

// Endpoint runs pager with the given arguments on one side of a link.
// Validate reports whether the endpoint can be used as configured; callers
// check it before every Command.
type Endpoint interface {
	Kind() Kind
	Validate() error
	Command(pagerArgs ...string) Command
}

// LocalEndpoint runs the pager on this machine. DB, when set, is passed as
// PAGER_DB; when empty, an inherited PAGER_DB is removed, so a terminal that
// exported one and a service that did not relay the same store.
type LocalEndpoint struct {
	Pager string // absolute path
	DB    string
}

func (l LocalEndpoint) Kind() Kind { return Local }

func (l LocalEndpoint) Validate() error {
	if !filepath.IsAbs(l.Pager) {
		return fmt.Errorf("local pager %q is not an absolute path", l.Pager)
	}
	return nil
}

func (l LocalEndpoint) Command(args ...string) Command {
	return Command{Path: l.Pager, Args: args, Env: withPagerDB(os.Environ(), l.DB)}
}

// ExecEndpoint runs pager through a launcher whose argv ends with the pager
// to run there, e.g. ["docker", "exec", "-i", "box", "/usr/local/bin/pager"].
// Env entries override this process's environment for the launcher.
type ExecEndpoint struct {
	Argv []string
	Env  map[string]string
}

func (e ExecEndpoint) Kind() Kind { return Exec }

func (e ExecEndpoint) Validate() error {
	if len(e.Argv) == 0 || e.Argv[0] == "" {
		return fmt.Errorf("exec argv is empty")
	}
	return nil
}

func (e ExecEndpoint) Command(args ...string) Command {
	env := os.Environ()
	for k, v := range e.Env {
		env = setEnv(env, k, v)
	}
	return Command{Path: e.Argv[0], Args: append(append([]string{}, e.Argv[1:]...), args...), Env: env}
}

// SSHEndpoint runs a remote pager through ssh.
type SSHEndpoint struct {
	SSH     string   // absolute path of the ssh client
	Host    string   // destination or ssh_config alias
	Pager   string   // absolute path of the remote pager
	Options []string // user options, placed before the preset's
	Windows bool     // the client is Windows OpenSSH, which has no ControlMaster
}

func (s SSHEndpoint) Kind() Kind { return SSH }

func (s SSHEndpoint) Validate() error { return ValidateSSH(s) }

// presetOptions keep a relay round non-interactive and bounded. ssh takes the
// first value it sees for an option, so the user's options come first and win.
var presetOptions = []string{
	"-T",
	"-o", "BatchMode=yes",
	"-o", "ConnectTimeout=10",
	"-o", "ServerAliveInterval=15",
	"-o", "ServerAliveCountMax=3",
}

// windowsOptions: Windows OpenSSH has no ControlMaster, and a ControlMaster
// line in the user's ssh_config makes it fail instead of falling back.
var windowsOptions = []string{"-o", "ControlMaster=no", "-o", "ControlPath=none"}

func (s SSHEndpoint) Command(args ...string) Command {
	argv := append([]string{}, s.Options...)
	argv = append(argv, presetOptions...)
	if s.Windows {
		argv = append(argv, windowsOptions...)
	}
	// "--" ends ssh's options, so the destination can never be read as one.
	argv = append(argv, "--", s.Host, remoteCommand(s.Pager, args))
	return Command{Path: s.SSH, Args: argv, Env: os.Environ()}
}

// remoteCommand joins the remote pager and its arguments into the one string
// ssh hands to the remote shell. ValidateSSH guarantees the path, and every
// argument pager is given here is fixed text, a validated peer name or a
// number, so no part of it needs quoting.
func remoteCommand(pager string, args []string) string {
	return strings.Join(append([]string{pager}, args...), " ")
}

var (
	safeRemotePath = regexp.MustCompile(`^/[A-Za-z0-9/._~+-]*$`)
	safeArg        = regexp.MustCompile(`^[A-Za-z0-9._=-]+$`)
	// safeHost admits host names, ssh_config aliases, user@host and bracketed
	// IPv6 literals, each part starting with a letter or digit. Nothing else:
	// a '%' or a shell character in the host can reach a local shell through
	// a ProxyCommand's %h on older OpenSSH (CVE-2023-51385), and a leading
	// '-' can reach the ProxyCommand as an option.
	safeHost = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*@)?([A-Za-z0-9][A-Za-z0-9._-]*|\[[0-9A-Fa-f:.]+\])$`)
)

// ValidateSSH refuses what the ssh preset cannot carry safely.
func ValidateSSH(s SSHEndpoint) error {
	if !filepath.IsAbs(s.SSH) {
		return fmt.Errorf("ssh client %q is not an absolute path", s.SSH)
	}
	if err := ValidateHost(s.Host); err != nil {
		return err
	}
	return ValidateRemotePager(s.Pager)
}

// ValidateHost refuses an ssh destination outside host-name characters.
// Configuration checks use it too, so a host is refused when it is read.
func ValidateHost(h string) error {
	if !safeHost.MatchString(h) {
		return fmt.Errorf("%q is not a host: use letters, digits and ._-, starting with a letter or digit, an optional user@, or a [bracketed] IPv6 address", h)
	}
	return nil
}

// ValidateRemotePager refuses a far-side pager path the remote command line
// could not carry unquoted.
func ValidateRemotePager(p string) error {
	if !safeRemotePath.MatchString(p) {
		return fmt.Errorf("%q must be an absolute path of A-Z, a-z, 0-9 and /._~+- only", p)
	}
	return nil
}

// ValidateArgs refuses a pager argument outside the safe set: the ssh command
// string must carry it as written, and some launchers pass their arguments to
// a shell. Every caller passes only fixed words, peer names and numbers.
func ValidateArgs(args []string) error {
	for _, a := range args {
		if !safeArg.MatchString(a) {
			return fmt.Errorf("pager argument %q is outside the safe set", a)
		}
	}
	return nil
}

// withPagerDB sets PAGER_DB to db, or removes it when db is empty.
func withPagerDB(env []string, db string) []string {
	if db == "" {
		return unsetEnv(env, "PAGER_DB")
	}
	return setEnv(env, "PAGER_DB", db)
}

func setEnv(env []string, k, v string) []string {
	return append(unsetEnv(env, k), k+"="+v)
}

func unsetEnv(env []string, k string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if name, _, _ := strings.Cut(kv, "="); envKeyEqual(name, k) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
