package transport

import (
	"slices"
	"strings"
	"testing"
)

func TestSSHArgv(t *testing.T) {
	base := SSHEndpoint{
		SSH:     "/usr/bin/ssh",
		Host:    "vm.example.com",
		Pager:   "/home/me/.local/bin/pager",
		Options: []string{"-o", "ServerAliveInterval=5", "-i", "/home/me/.ssh/relay"},
	}

	t.Run("layout", func(t *testing.T) {
		c := base.Command("relay", "out", "--peer", "laptop", "--limit", "100")
		if c.Path != "/usr/bin/ssh" {
			t.Fatalf("path %q", c.Path)
		}
		want := []string{
			"-o", "ServerAliveInterval=5", "-i", "/home/me/.ssh/relay", // the user's first: ssh keeps the first value
			"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
			"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
			"--", "vm.example.com",
			"/home/me/.local/bin/pager relay out --peer laptop --limit 100",
		}
		if !slices.Equal(c.Args, want) {
			t.Fatalf("argv\n got %q\nwant %q", c.Args, want)
		}
	})

	t.Run("windows has no ControlMaster", func(t *testing.T) {
		w := base
		w.Windows = true
		c := w.Command("relay", "id")
		joined := strings.Join(c.Args, " ")
		if !strings.Contains(joined, "-o ControlMaster=no -o ControlPath=none -- vm.example.com") {
			t.Fatalf("windows argv lacks the ControlMaster override before the destination: %q", c.Args)
		}
		if strings.Contains(strings.Join(base.Command("relay", "id").Args, " "), "ControlMaster") {
			t.Fatal("a non-Windows client must keep the user's ControlMaster setting")
		}
	})

	t.Run("validation", func(t *testing.T) {
		for name, mut := range map[string]func(*SSHEndpoint){
			"relative ssh":       func(s *SSHEndpoint) { s.SSH = "ssh" },
			"empty host":         func(s *SSHEndpoint) { s.Host = "" },
			"leading-dash host":  func(s *SSHEndpoint) { s.Host = "-oProxyCommand=evil" },
			"host with a space":  func(s *SSHEndpoint) { s.Host = "vm example" },
			"host with %":        func(s *SSHEndpoint) { s.Host = "vm%h" },
			"host with $":        func(s *SSHEndpoint) { s.Host = "$(id).example.com" },
			"host with ;":        func(s *SSHEndpoint) { s.Host = "vm;id" },
			"two @":              func(s *SSHEndpoint) { s.Host = "a@b@vm" },
			"relative pager":     func(s *SSHEndpoint) { s.Pager = "bin/pager" },
			"pager with a space": func(s *SSHEndpoint) { s.Pager = "/opt/my tools/pager" },
			"pager with a quote": func(s *SSHEndpoint) { s.Pager = "/opt/x';rm -rf ~;'" },
			"pager with $":       func(s *SSHEndpoint) { s.Pager = "/opt/$HOME/pager" },
		} {
			s := base
			mut(&s)
			if err := ValidateSSH(s); err == nil {
				t.Errorf("%s: accepted %+v", name, s)
			}
		}
		for _, host := range []string{"vm.example.com", "vm", "me@vm.example.com", "my_alias", "[2001:db8::1]", "me@[2001:db8::1]"} {
			s := base
			s.Host = host
			if err := ValidateSSH(s); err != nil {
				t.Errorf("host %q was refused: %v", host, err)
			}
		}
		if err := ValidateArgs([]string{"relay", "out", "--peer", "laptop", "--limit", "100"}); err != nil {
			t.Errorf("valid args refused: %v", err)
		}
		for _, bad := range []string{"a b", "x;y", "$(id)", "`id`", "", "'"} {
			if ValidateArgs([]string{bad}) == nil {
				t.Errorf("argument %q accepted", bad)
			}
		}
	})
}

func TestLocalEndpointOwnsPagerDB(t *testing.T) {
	t.Setenv("PAGER_DB", "/tmp/inherited.db")
	envOf := func(c Command, key string) []string {
		var vals []string
		for _, kv := range c.Env {
			if k, v, _ := strings.Cut(kv, "="); envKeyEqual(k, key) {
				vals = append(vals, v)
			}
		}
		return vals
	}
	c := LocalEndpoint{Pager: "/usr/local/bin/pager", DB: "/data/msg.db"}.Command("relay", "id")
	if got := envOf(c, "PAGER_DB"); !slices.Equal(got, []string{"/data/msg.db"}) {
		t.Fatalf("PAGER_DB = %q, want only the configured one", got)
	}
	c = LocalEndpoint{Pager: "/usr/local/bin/pager"}.Command("relay", "id")
	if got := envOf(c, "PAGER_DB"); len(got) != 0 {
		t.Fatalf("an inherited PAGER_DB survived: %q", got)
	}
	if c.Path != "/usr/local/bin/pager" || !slices.Equal(c.Args, []string{"relay", "id"}) {
		t.Fatalf("command %+v", c)
	}
}

func TestExecEndpoint(t *testing.T) {
	t.Setenv("PAGER_DB", "/tmp/inherited.db")
	e := ExecEndpoint{
		Argv: []string{"docker", "exec", "-i", "box", "/usr/local/bin/pager"},
		Env:  map[string]string{"PAGER_DB": "/data/far.db"},
	}
	c := e.Command("relay", "in", "--peer", "laptop")
	if c.Path != "docker" {
		t.Fatalf("path %q", c.Path)
	}
	if want := []string{"exec", "-i", "box", "/usr/local/bin/pager", "relay", "in", "--peer", "laptop"}; !slices.Equal(c.Args, want) {
		t.Fatalf("args %q, want %q", c.Args, want)
	}
	n := 0
	for _, kv := range c.Env {
		if k, v, _ := strings.Cut(kv, "="); envKeyEqual(k, "PAGER_DB") {
			n++
			if v != "/data/far.db" {
				t.Fatalf("PAGER_DB = %q", v)
			}
		}
	}
	if n != 1 {
		t.Fatalf("PAGER_DB appears %d times", n)
	}
	if &e.Argv[0] == &c.Args[0] {
		t.Fatal("the command shares the endpoint's argv")
	}
}

func TestEveryEndpointValidates(t *testing.T) {
	for name, ep := range map[string]Endpoint{
		"relative local pager": LocalEndpoint{Pager: "pager"},
		"empty exec argv":      ExecEndpoint{},
		"unsafe ssh pager":     SSHEndpoint{SSH: "/usr/bin/ssh", Host: "vm", Pager: "/opt/x;id"},
	} {
		if err := ep.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, ep := range map[string]Endpoint{
		"local": LocalEndpoint{Pager: "/usr/local/bin/pager"},
		"exec":  ExecEndpoint{Argv: []string{"docker", "exec", "-i", "box", "/usr/local/bin/pager"}},
		"ssh":   SSHEndpoint{SSH: "/usr/bin/ssh", Host: "vm", Pager: "/usr/local/bin/pager"},
	} {
		if err := ep.Validate(); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}
