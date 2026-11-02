// Command switchboard carries pager mail between pager stores on different
// machines. It runs pager's relay contract — relay out, relay in, relay ack —
// across a link, decides when to run a round, and tells a person when only a
// person can fix what went wrong.
package main

import (
	"fmt"
	"io"
	"os"
)

// version is set at release time with -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	os.Exit(dispatch(os.Args[1:], os.Stdout, os.Stderr))
}

const usage = `usage: switchboard <command> [flags]

commands:
  run      [--config FILE]                 carry mail on every link until stopped
  once     [--config FILE] [--debug] [LINK…] run one round each way now
  check    [--config FILE] [LINK…]          verify both stores enrolled each other
  enroll   [--config FILE] [LINK…]          enroll both stores with each other where missing
  status   [--config FILE] [--json] [LINK…] show each link's state
  resume   [--config FILE] LINK             clear a stopped link and run it again
  service  install|uninstall|status [--config FILE] [--exe FILE] [--print]
                                            run switchboard at logon as this user
  version                                   print the version

Every command reports the configuration file it read.
`

func dispatch(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "run":
		return cmdRun(rest, stderr)
	case "once":
		return cmdOnce(rest, stdout, stderr)
	case "check":
		return cmdCheck(rest, stdout, stderr)
	case "enroll":
		return cmdEnroll(rest, stdout, stderr)
	case "status":
		return cmdStatus(rest, stdout, stderr)
	case "resume":
		return cmdResume(rest, stdout, stderr)
	case "service":
		return cmdService(rest, stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, "switchboard", version)
		return 0
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	case "spike":
		return spike(rest)
	}
	fmt.Fprintf(stderr, "switchboard: unknown command %q\n\n%s", cmd, usage)
	return 2
}
