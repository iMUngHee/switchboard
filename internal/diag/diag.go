// Package diag turns what a stage wrote to stderr into a diagnostic code: one
// of a fixed set of short strings, safe to log, store and show to a person.
//
// A stage's stderr never leaves switchboard. pager's own can name recipients
// and quote what they were sent, and a remote shell's can say anything, so
// classifying it is its only use: nothing here returns, wraps or formats the
// text it is given.
package diag

import "strings"

// Codes ssh reports for its own failures (exit 255).
const (
	Auth           = "auth"     // no key or password the server accepts
	HostKey        = "host-key" // the host key is unknown or has changed
	ConnRefused    = "conn-refused"
	ConnTimeout    = "conn-timeout"
	NoRoute        = "no-route"
	NetUnreachable = "net-unreachable"
	DNS            = "dns"
	ConnClosed     = "conn-closed"
)

// Codes pager's relay commands report.
const (
	Schema      = "schema"       // the binary does not support this database's schema
	UnknownPeer = "unknown-peer" // --peer names no registered peer
	Refused     = "refused"      // the stream was refused: wrong store, link, version, a malformed line
	Usage       = "usage"
	DBLocked    = "db-locked"
)

type pattern struct{ code, text string }

// The first matching pattern wins, so the more specific ones come first.
// Matching is on lower-cased text.
var sshPatterns = []pattern{
	{HostKey, "host key verification failed"},
	{HostKey, "remote host identification has changed"},
	{Auth, "permission denied"},
	{Auth, "no more authentication methods"},
	{Auth, "too many authentication failures"},
	{Auth, "authentication failed"},
	{ConnRefused, "connection refused"},
	{ConnTimeout, "connection timed out"},
	{ConnTimeout, "operation timed out"},
	{NoRoute, "no route to host"},
	{NetUnreachable, "network is unreachable"},
	{DNS, "could not resolve hostname"},
	{DNS, "name or service not known"},
	{DNS, "nodename nor servname"},
	{ConnClosed, "connection closed by"},
	{ConnClosed, "connection reset by"},
}

var pagerPatterns = []pattern{
	{Schema, "database schema version"},
	{UnknownPeer, "unknown peer"},
	{Refused, "refused:"},
	{Usage, "--limit must be"},
	{Usage, "unknown relay subcommand"},
	{Usage, "usage:"},
	{DBLocked, "database is locked"},
}

// SSH classifies the stderr of a stage that exited 255, which is ssh's own
// failure rather than the remote command's. It returns "" when nothing
// matches.
func SSH(stderr []byte) string { return match(sshPatterns, stderr) }

// Pager classifies the stderr of a stage that exited as pager does. It
// returns "" when nothing matches.
func Pager(stderr []byte) string { return match(pagerPatterns, stderr) }

// IsAuth reports whether a code means credentials a person must fix: a key to
// load, or a host key to accept.
func IsAuth(code string) bool { return code == Auth || code == HostKey }

func match(ps []pattern, stderr []byte) string {
	s := strings.ToLower(string(stderr))
	for _, p := range ps {
		if strings.Contains(s, p.text) {
			return p.code
		}
	}
	return ""
}
