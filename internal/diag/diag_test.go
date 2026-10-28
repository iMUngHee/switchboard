package diag

import "testing"

func TestSSHCodes(t *testing.T) {
	for in, want := range map[string]string{
		"me@vm.example.com: Permission denied (publickey).\r\n":                 Auth,
		"Received disconnect: Too many authentication failures":                 Auth,
		"Host key verification failed.\n":                                       HostKey,
		"@ WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED! @":                  HostKey,
		"ssh: connect to host vm.example.com port 22: Connection refused":       ConnRefused,
		"ssh: connect to host vm.example.com port 22: Operation timed out":      ConnTimeout,
		"ssh: connect to host 192.0.2.1 port 22: No route to host":              NoRoute,
		"ssh: connect to host 192.0.2.1 port 22: Network is unreachable":        NetUnreachable,
		"ssh: Could not resolve hostname vm.example.com: nodename nor servname": DNS,
		"ssh: Could not resolve hostname vm: Name or service not known":         DNS,
		"Connection closed by 192.0.2.1 port 22":                                ConnClosed,
		"kex_exchange_identification: read: Connection reset by peer":           ConnClosed,
		"something nobody expected":                                             "",
		"":                                                                      "",
	} {
		if got := SSH([]byte(in)); got != want {
			t.Errorf("SSH(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPagerCodes(t *testing.T) {
	for in, want := range map[string]string{
		"pager: database schema version 9 is not in the range this binary supports (0..4)": Schema,
		`pager: unknown peer "vm" (pager peer ls)`:                                         UnknownPeer,
		"pager: refused: wrong store: the record is for store 00, and this is 11":          Refused,
		"pager: --limit must be between 1 and 1000 (got 0)":                                Usage,
		"pager: database is locked":                                                        DBLocked,
		"pager: read records: unexpected EOF":                                              "",
	} {
		if got := Pager([]byte(in)); got != want {
			t.Errorf("Pager(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPatternsAreSeparate: pager text that happens to quote an ssh error, or
// the reverse, is not read with the other's patterns.
func TestPatternsAreSeparate(t *testing.T) {
	if got := Pager([]byte("Permission denied (publickey)")); got != "" {
		t.Errorf("pager patterns matched ssh text: %q", got)
	}
	if got := SSH([]byte("refused: wrong store")); got != "" {
		t.Errorf("ssh patterns matched pager text: %q", got)
	}
}
