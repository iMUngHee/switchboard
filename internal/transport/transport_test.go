package transport

import (
	"runtime"
	"slices"
	"strings"
	"testing"
)

// localBin is an absolute path to a program on this machine, in this
// system's form. Paths on the far side of ssh stay POSIX everywhere.
func localBin(name string) string {
	if runtime.GOOS == "windows" {
		return `C:\bin\` + name + ".exe"
	}
	return "/usr/bin/" + name
}

func TestSSHArgv(t *testing.T) {
	base := SSHEndpoint{
		SSH:     localBin("ssh"),
		Host:    "vm.example.com",
		Pager:   "/home/me/.local/bin/pager",
		Options: []string{"-o", "ServerAliveInterval=5", "-i", "/home/me/.ssh/relay"},
	}

	t.Run("layout", func(t *testing.T) {
		c := base.Command("relay", "out", "--peer", "laptop", "--limit", "100")
		if c.Path != base.SSH {
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

	t.Run("ssh -G gets the round's options and no remote command", func(t *testing.T) {
		c := base.ConfigCommand()
		want := []string{
			"-o", "ServerAliveInterval=5", "-i", "/home/me/.ssh/relay",
			"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10",
			"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
			"-G", "--", "vm.example.com",
		}
		if c.Path != base.SSH || !slices.Equal(c.Args, want) {
			t.Fatalf("ssh -G\n got %q %q\nwant %q", c.Path, c.Args, want)
		}
		w := base
		w.Windows = true
		if joined := strings.Join(w.ConfigCommand().Args, " "); !strings.Contains(joined, "-o ControlMaster=no -o ControlPath=none -G -- vm.example.com") {
			t.Fatalf("windows ssh -G lacks the ControlMaster override the rounds get: %q", joined)
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
			"user@-host":         func(s *SSHEndpoint) { s.Host = "me@-vm" },
			"-user@host":         func(s *SSHEndpoint) { s.Host = "-me@vm" },
			"empty user":         func(s *SSHEndpoint) { s.Host = "@vm" },
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

// TestControlPath reads the control socket from ssh -G's output: the first
// controlpath line, as ssh printed it, and none means there is none.
func TestControlPath(t *testing.T) {
	for _, tc := range []struct{ name, config, want string }{
		{"expanded", "user me\nhostname vm.example.com\ncontrolmaster auto\ncontrolpath /home/me/.ssh/cm/d66e11c2\nbatchmode yes\n", "/home/me/.ssh/cm/d66e11c2"},
		{"crlf", "hostname vm.example.com\r\ncontrolpath /tmp/cm/x\r\n", "/tmp/cm/x"},
		{"a key in other case", "ControlPath /tmp/cm/x\n", "/tmp/cm/x"},
		{"a path with a space", "controlpath /Users/me/Library/Application Support/cm/x\n", "/Users/me/Library/Application Support/cm/x"},
		{"none", "controlmaster auto\ncontrolpath none\n", ""},
		{"absent", "user me\ncontrolmaster auto\ncontrolpersist 600\n", ""},
		{"another key that starts the same", "controlpathx /tmp/no\n", ""},
		{"empty", "", ""},
	} {
		if got := ControlPath([]byte(tc.config)); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestJumps reads the ProxyJump from ssh -G's output, a hop at a time, each
// with the hops before it; one hop outside the safe set drops them all.
func TestJumps(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		want         []Jump
	}{
		{"one alias", "hostname vm.example.com\nproxyjump jump\n", []Jump{{Host: "jump"}}},
		{"user and port", "proxyjump me@jump.example.com:2222\n", []Jump{{User: "me", Host: "jump.example.com", Port: "2222"}}},
		{"two hops", "proxyjump me@a.example.com:2200,b.example.com\n", []Jump{
			{User: "me", Host: "a.example.com", Port: "2200"},
			{Host: "b.example.com", Via: "me@a.example.com:2200"},
		}},
		{"three hops", "proxyjump a,b,c\n", []Jump{{Host: "a"}, {Host: "b", Via: "a"}, {Host: "c", Via: "a,b"}}},
		{"a uri", "proxyjump ssh://me@a.example.com:2200\n", []Jump{{User: "me", Host: "a.example.com", Port: "2200"}}},
		{"ipv6, kept bare", "proxyjump me@[2001:db8::1]:22\n", []Jump{{User: "me", Host: "2001:db8::1", Port: "22"}}},
		{"a numeric hop as ssh -G prints it", "proxyjump [192.0.2.10]\n", []Jump{{Host: "192.0.2.10"}}},
		{"an ipv6 hop before another", "proxyjump me@[2001:db8::5]:2222,b\n", []Jump{
			{User: "me", Host: "2001:db8::5", Port: "2222"},
			{Host: "b", Via: "me@[2001:db8::5]:2222"},
		}},
		{"a bracket of other characters", "proxyjump [g::1]\n", nil},
		{"something after the bracket", "proxyjump [::1]x\n", nil},
		{"crlf", "proxyjump jump\r\n", []Jump{{Host: "jump"}}},
		{"none", "proxyjump none\n", nil},
		{"absent", "hostname vm.example.com\ncontrolpath /tmp/cm/x\n", nil},
		{"an option for a host", "proxyjump -oProxyCommand=evil\n", nil},
		{"a shell character", "proxyjump a;id\n", nil},
		{"two @", "proxyjump a@b@c\n", nil},
		{"a user with a quote", "proxyjump m'e@jump\n", nil},
		{"a port that is not one", "proxyjump jump:22x\n", nil},
		{"an unclosed bracket", "proxyjump [2001:db8::1\n", nil},
		{"one bad hop of two", "proxyjump good.example.com,-bad\n", nil},
		{"an empty hop", "proxyjump a,,b\n", nil},
	} {
		if got := Jumps([]byte(tc.config)); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// TestJumpConfigCommand: a hop's ssh -G gets what ssh gives its own command
// for the hop, -F from the round's options, -l, -p and the hops before it
// with -J, and nothing else of the round's.
func TestJumpConfigCommand(t *testing.T) {
	s := SSHEndpoint{SSH: localBin("ssh"), Host: "vm.example.com", Pager: "/home/me/.local/bin/pager"}
	for _, tc := range []struct {
		name    string
		options []string
		jump    Jump
		want    []string
	}{
		{"a bare hop", nil, Jump{Host: "jump"}, []string{"-G", "--", "jump"}},
		{"everything", []string{"-o", "ServerAliveInterval=5", "-F", "/home/me/.ssh/relay_config", "-i", "/home/me/.ssh/relay"},
			Jump{User: "me", Host: "b.example.com", Port: "2222", Via: "a.example.com"},
			[]string{"-F", "/home/me/.ssh/relay_config", "-l", "me", "-p", "2222", "-J", "a.example.com", "-G", "--", "b.example.com"}},
		{"-F joined, the last one wins", []string{"-F/etc/a", "-F", "/etc/b"}, Jump{Host: "jump"}, []string{"-F", "/etc/b", "-G", "--", "jump"}},
		{"-F as another option's value", []string{"-o", "-F", "-i", "/home/me/.ssh/relay"}, Jump{Host: "jump"}, []string{"-G", "--", "jump"}},
		{"-F with nothing after it", []string{"-F"}, Jump{Host: "jump"}, []string{"-G", "--", "jump"}},
		{"-F clustered after a flag", []string{"-qF", "/etc/c"}, Jump{Host: "jump"}, []string{"-F", "/etc/c", "-G", "--", "jump"}},
		{"-F clustered and joined", []string{"-4F/etc/d"}, Jump{Host: "jump"}, []string{"-F", "/etc/d", "-G", "--", "jump"}},
		{"-F as a clustered option's value", []string{"-vo", "-F"}, Jump{Host: "jump"}, []string{"-G", "--", "jump"}},
		{"an option with its value joined", []string{"-oUserKnownHostsFile=/dev/null", "-oForwardAgent=no"}, Jump{Host: "jump"}, []string{"-G", "--", "jump"}},
		{"an ipv6 hop, bare", nil, Jump{Host: "2001:db8::5", Port: "2222"}, []string{"-p", "2222", "-G", "--", "2001:db8::5"}},
	} {
		s.Options = tc.options
		c := s.JumpConfigCommand(tc.jump)
		if c.Path != s.SSH || !slices.Equal(c.Args, tc.want) {
			t.Errorf("%s:\n got %q %q\nwant %q", tc.name, c.Path, c.Args, tc.want)
		}
	}
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
	c := LocalEndpoint{Pager: localBin("pager"), DB: "/data/msg.db"}.Command("relay", "id")
	if got := envOf(c, "PAGER_DB"); !slices.Equal(got, []string{"/data/msg.db"}) {
		t.Fatalf("PAGER_DB = %q, want only the configured one", got)
	}
	c = LocalEndpoint{Pager: localBin("pager")}.Command("relay", "id")
	if got := envOf(c, "PAGER_DB"); len(got) != 0 {
		t.Fatalf("an inherited PAGER_DB survived: %q", got)
	}
	if c.Path != localBin("pager") || !slices.Equal(c.Args, []string{"relay", "id"}) {
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
		"unsafe ssh pager":     SSHEndpoint{SSH: localBin("ssh"), Host: "vm", Pager: "/opt/x;id"},
	} {
		if err := ep.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	for name, ep := range map[string]Endpoint{
		"local": LocalEndpoint{Pager: localBin("pager")},
		"exec":  ExecEndpoint{Argv: []string{"docker", "exec", "-i", "box", "/usr/local/bin/pager"}},
		"ssh":   SSHEndpoint{SSH: localBin("ssh"), Host: "vm", Pager: "/usr/local/bin/pager"},
	} {
		if err := ep.Validate(); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}
}
