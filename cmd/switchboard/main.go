// Command switchboard carries pager mail between pager stores on different
// machines. It runs pager's relay contract — relay out, relay in, relay ack —
// across a link, decides when to run a round, and tells a person when only a
// person can fix what went wrong.
package main

import (
	"fmt"
	"os"
)

// version is set at release time with -ldflags "-X main.version=<tag>".
var version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	switch {
	case len(args) == 1 && (args[0] == "version" || args[0] == "--version"):
		fmt.Println("switchboard", version)
		return 0
	case len(args) >= 1 && args[0] == "spike":
		return spike(args[1:])
	}
	fmt.Fprintln(os.Stderr, "usage: switchboard version")
	return 2
}
