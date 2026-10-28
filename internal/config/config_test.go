package config

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"
)

// localPager and localDB are absolute on the system running the test; the far
// side's pager path is POSIX everywhere.
var localPager, localDB = func() (string, string) {
	if runtime.GOOS == "windows" {
		return `C:\pager\pager.exe`, `C:\pager\msg.db`
	}
	return "/usr/local/bin/pager", "/home/me/.pager/msg.db"
}()

// head is the part of a valid document most cases share.
var head = "version = 1\n[pager]\npath = '" + localPager + "'\n"

// vmLink is a valid ssh link; vmLinkWith adds lines to its link table, and
// sshWith to its ssh table.
const vmLink = `
[link.vm]
remote_peer = "laptop"
[link.vm.ssh]
host = "vm.example.com"
pager = "/home/me/.local/bin/pager"
`

func vmLinkWith(lines string) string {
	return "\n[link.vm]\nremote_peer = \"laptop\"\n" + lines + "\n[link.vm.ssh]\nhost = \"vm.example.com\"\npager = \"/home/me/.local/bin/pager\"\n"
}

func sshWith(lines string) string {
	return "\n[link.vm]\nremote_peer = \"laptop\"\n[link.vm.ssh]\n" + lines + "\n"
}

func execWith(lines string) string {
	return "\n[link.box]\nremote_peer = \"laptop\"\n[link.box.exec]\n" + lines + "\n"
}

// TestConfigRejectsUnknownKeysAndVersions: an unknown key anywhere, a missing
// or unsupported version, and every semantic rule each fail with an error
// that names the file and the TOML key.
func TestConfigRejectsUnknownKeysAndVersions(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		want []string // each must appear in the error
	}{
		// Unknown keys, at every depth.
		{"unknown top-level key", "bogus = 1\n" + head + vmLink, []string{"cfg.toml: bogus: unknown key"}},
		{"unknown key in pager", head + "paht = \"/x\"\n" + vmLink, []string{"cfg.toml: pager.paht: unknown key"}},
		{"unknown key in a link", head + vmLinkWith(`pol_outbound = "5s"`), []string{"cfg.toml: link.vm.pol_outbound: unknown key"}},
		{"unknown key in ssh", head + sshWith("host = \"vm.example.com\"\npager = \"/p\"\nhots = \"x\""), []string{"cfg.toml: link.vm.ssh.hots: unknown key"}},
		{"unknown key in exec", head + execWith("argv = [\"/bin/pager\"]\nargs = []"), []string{"cfg.toml: link.box.exec.args: unknown key"}},
		{"unknown key in notify", head + vmLink + "[notify]\ncmd = [\"x\"]\n", []string{"cfg.toml: notify.cmd: unknown key"}},
		{"every unknown key is reported", "a = 1\nb = 2\n" + head + vmLink, []string{"cfg.toml: a: unknown key", "cfg.toml: b: unknown key"}},

		// Version.
		{"missing version", "[pager]\npath = \"/usr/local/bin/pager\"\n" + vmLink, []string{"cfg.toml: version: is required"}},
		{"version 2", strings.Replace(head, "version = 1", "version = 2", 1) + vmLink, []string{"cfg.toml: version: 2 is not one this release reads", "may be for a newer release"}},
		{"version 0", strings.Replace(head, "version = 1", "version = 0", 1) + vmLink, []string{"cfg.toml: version: 0 is not one this release reads"}},
		// A newer config's new keys must not hide the version.
		{"version 2 with keys this release lacks", "newer = true\n" + strings.Replace(head, "version = 1", "version = 2", 1) + vmLink, []string{"cfg.toml: version: 2 is not one this release reads"}},
		{"version as a string", strings.Replace(head, "version = 1", `version = "1"`, 1) + vmLink, []string{"cfg.toml: version: must be an integer, not a string"}},
		{"version as a float", strings.Replace(head, "version = 1", "version = 1.0", 1) + vmLink, []string{"cfg.toml: version: must be an integer, not a float"}},

		// Tables that are not tables.
		{"pager not a table", "version = 1\npager = \"/usr/local/bin/pager\"\n" + vmLink, []string{"cfg.toml: pager: must be a table, not a string"}},
		{"link as an array of tables", head + "[[link]]\nremote_peer = \"x\"\n", []string{"cfg.toml: link: must be a table, not an array"}},
		{"ssh not a table", head + "[link.vm]\nremote_peer = \"laptop\"\nssh = \"vm.example.com\"\n", []string{"cfg.toml: link.vm.ssh: must be a table, not a string"}},
		{"notify not a table", "notify = 1\n" + head + vmLink, []string{"cfg.toml: notify: must be a table, not an integer"}},

		// pager.
		{"pager.path missing", "version = 1\n[pager]\n" + vmLink, []string{"cfg.toml: pager.path: is required"}},
		{"no pager table", "version = 1\n" + vmLink, []string{"cfg.toml: pager.path: is required"}},
		{"pager.path relative", "version = 1\n[pager]\npath = \"bin/pager\"\n" + vmLink, []string{"cfg.toml: pager.path: must be an absolute path"}},
		{"pager.path not a string", "version = 1\n[pager]\npath = 1\n" + vmLink, []string{"cfg.toml: pager.path: must be a string, not an integer"}},
		{"pager.db relative", head + "db = \"msg.db\"\n" + vmLink, []string{"cfg.toml: pager.db: must be an absolute path"}},

		// Link names and remote_peer: pager's peer-name rule, upper case refused.
		{"upper-case link name", head + strings.ReplaceAll(vmLink, "link.vm", "link.VM"), []string{`cfg.toml: link.VM: "VM" is not a peer name`}},
		{"link name starting with '-'", head + strings.ReplaceAll(vmLink, "link.vm", "link.-vm"), []string{`cfg.toml: link.-vm: "-vm" is not a peer name`}},
		{"link name with '_'", head + strings.ReplaceAll(vmLink, "link.vm", "link.v_m"), []string{`cfg.toml: link.v_m: "v_m" is not a peer name`}},
		{"link name of 33 characters", head + strings.ReplaceAll(vmLink, "link.vm", "link."+strings.Repeat("a", 33)), []string{"cfg.toml: link." + strings.Repeat("a", 33) + ": "}},
		{"remote_peer missing", head + "[link.vm]\n[link.vm.ssh]\nhost = \"vm.example.com\"\npager = \"/p\"\n", []string{"cfg.toml: link.vm.remote_peer: is required"}},
		{"upper-case remote_peer", head + strings.Replace(vmLink, `"laptop"`, `"Laptop"`, 1), []string{`cfg.toml: link.vm.remote_peer: "Laptop" is not a peer name`}},
		{"remote_peer starting with '-'", head + strings.Replace(vmLink, `"laptop"`, `"-laptop"`, 1), []string{`cfg.toml: link.vm.remote_peer: "-laptop" is not a peer name`}},

		// Exactly one transport.
		{"both ssh and exec", head + vmLink + "[link.vm.exec]\nargv = [\"/bin/pager\"]\n", []string{"cfg.toml: link.vm: has both an ssh and an exec table"}},
		{"neither ssh nor exec", head + "[link.vm]\nremote_peer = \"laptop\"\n", []string{"cfg.toml: link.vm: needs an ssh or an exec table"}},

		// ssh.
		{"ssh.host missing", head + sshWith(`pager = "/p"`), []string{"cfg.toml: link.vm.ssh.host: is required"}},
		{"ssh.host empty", head + sshWith("host = \"\"\npager = \"/p\""), []string{`cfg.toml: link.vm.ssh.host: "" is not a host`}},
		{"ssh.host starting with '-'", head + sshWith("host = \"-oProxyCommand=x\"\npager = \"/p\""), []string{`cfg.toml: link.vm.ssh.host: "-oProxyCommand=x" is not a host`}},
		{"ssh.host with a ProxyCommand token", head + sshWith("host = \"vm%h\"\npager = \"/p\""), []string{`cfg.toml: link.vm.ssh.host: "vm%h" is not a host`}},
		{"ssh.host with shell syntax", head + sshWith("host = \"vm;id\"\npager = \"/p\""), []string{`cfg.toml: link.vm.ssh.host: "vm;id" is not a host`}},
		{"ssh.host with a space", head + sshWith("host = \"vm example\"\npager = \"/p\""), []string{`cfg.toml: link.vm.ssh.host: "vm example" is not a host`}},
		{"ssh.pager missing", head + sshWith(`host = "vm.example.com"`), []string{"cfg.toml: link.vm.ssh.pager: is required"}},
		{"ssh.pager relative", head + sshWith("host = \"vm.example.com\"\npager = \"bin/pager\""), []string{`cfg.toml: link.vm.ssh.pager: "bin/pager" must be an absolute path`}},
		{"ssh.pager with a space", head + sshWith("host = \"vm.example.com\"\npager = \"/opt/my pager\""), []string{`cfg.toml: link.vm.ssh.pager: "/opt/my pager" must be`}},
		{"ssh.pager with shell syntax", head + sshWith("host = \"vm.example.com\"\npager = \"/bin/pager;id\""), []string{`cfg.toml: link.vm.ssh.pager: "/bin/pager;id" must be`}},
		{"ssh.options not an array", head + sshWith("host = \"vm.example.com\"\npager = \"/p\"\noptions = \"-v\""), []string{"cfg.toml: link.vm.ssh.options: must be an array of strings, not a string"}},
		{"ssh.options element not a string", head + sshWith("host = \"vm.example.com\"\npager = \"/p\"\noptions = [\"-v\", 1]"), []string{"cfg.toml: link.vm.ssh.options[1]: must be a string, not an integer"}},

		// exec.
		{"exec.argv missing", head + execWith(`env = { A = "b" }`), []string{"cfg.toml: link.box.exec.argv: is required"}},
		{"exec.argv empty", head + execWith("argv = []"), []string{"cfg.toml: link.box.exec.argv: must name a program"}},
		{"exec.argv[0] empty", head + execWith(`argv = ["", "relay"]`), []string{"cfg.toml: link.box.exec.argv: must name a program"}},
		{"exec.argv not an array", head + execWith(`argv = "/bin/pager"`), []string{"cfg.toml: link.box.exec.argv: must be an array of strings, not a string"}},
		{"exec.env not a table", head + execWith("argv = [\"/bin/pager\"]\nenv = \"A=b\""), []string{"cfg.toml: link.box.exec.env: must be a table of strings, not a string"}},
		{"exec.env value not a string", head + execWith("argv = [\"/bin/pager\"]\nenv = { A = 1 }"), []string{"cfg.toml: link.box.exec.env.A: must be a string, not an integer"}},
		{"exec.env name with '='", head + execWith("argv = [\"/bin/pager\"]\nenv = { \"A=B\" = \"c\" }"), []string{`cfg.toml: link.box.exec.env: "A=B" is not an environment variable name`}},

		// Durations: Go strings, at or above their minimums.
		{"poll_outbound below its minimum", head + vmLinkWith(`poll_outbound = "500ms"`), []string{"cfg.toml: link.vm.poll_outbound: 500ms is shorter than the minimum, 1s"}},
		{"poll_inbound below its minimum", head + vmLinkWith(`poll_inbound = "4s"`), []string{"cfg.toml: link.vm.poll_inbound: 4s is shorter than the minimum, 5s"}},
		{"poll_outbound without a unit", head + vmLinkWith(`poll_outbound = "5"`), []string{`cfg.toml: link.vm.poll_outbound: "5" is not a duration such as "5s"`}},
		{"poll_inbound negative", head + vmLinkWith(`poll_inbound = "-30s"`), []string{"cfg.toml: link.vm.poll_inbound: -30s is shorter than the minimum, 5s"}},
		{"poll_outbound as an integer", head + vmLinkWith(`poll_outbound = 5`), []string{"cfg.toml: link.vm.poll_outbound: must be a string, not an integer"}},
		{"hint not a string", head + vmLinkWith(`hint = ["x"]`), []string{"cfg.toml: link.vm.hint: must be a string, not an array"}},

		// notify.
		{"notify.command empty", head + vmLink + "[notify]\ncommand = []\n", []string{"cfg.toml: notify.command: must name a program"}},
		{"notify.command[0] empty", head + vmLink + "[notify]\ncommand = [\"\", \"{link}\"]\n", []string{"cfg.toml: notify.command: must name a program"}},
		{"notify.command not an array", head + vmLink + "[notify]\ncommand = \"notify-send\"\n", []string{"cfg.toml: notify.command: must be an array of strings, not a string"}},

		// Several problems at once are all reported.
		{"every problem is reported", head + sshWith("host = \"-x\"\npager = \"rel\""), []string{"cfg.toml: link.vm.ssh.host:", "cfg.toml: link.vm.ssh.pager:"}},

		// A syntax error names the file and the position.
		{"syntax error", "version = 1\n[pager\n", []string{"cfg.toml:2:"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tc.doc), "cfg.toml")
			if err == nil {
				t.Fatalf("Parse accepted it: %+v\n%s", cfg, tc.doc)
			}
			msg := err.Error()
			for _, want := range tc.want {
				if !strings.Contains(msg, want) {
					t.Errorf("error does not contain %q:\n%s", want, msg)
				}
			}
			for _, line := range strings.Split(msg, "\n") {
				if !strings.HasPrefix(line, "cfg.toml") {
					t.Errorf("error line does not name the file: %q", line)
				}
			}
		})
	}
}

// TestConfigParsesAValidFile: a valid file comes back with its values, the
// link name in Peer, and the defaults filled in.
func TestConfigParsesAValidFile(t *testing.T) {
	doc := `version = 1

[pager]
path = '` + localPager + `'
db = '` + localDB + `'

[link.vm]
remote_peer = "laptop"

[link.vm.ssh]
host = "vm.example.com"
pager = "/home/me/.local/bin/pager"
options = ["-o", "IdentitiesOnly=yes"]

[link.box]
remote_peer = "laptop-2"
poll_outbound = "1s"
poll_inbound = "1m"
hint = "start the box container"

[link.box.exec]
argv = ["docker", "exec", "-i", "box", "/usr/local/bin/pager"]
env = { PAGER_DB = "/data/msg.db" }

[notify]
command = ["/usr/local/bin/my-notifier", "{link}", "{state}", "{hint}"]
`
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := &Config{
		Version: 1,
		Pager:   Pager{Path: localPager, DB: localDB},
		Links: map[string]Link{
			"vm": {
				Peer: "vm", RemotePeer: "laptop",
				PollOutbound: 5 * time.Second, PollInbound: 30 * time.Second,
				Hint: "run `ssh vm.example.com` once",
				SSH:  &SSH{Host: "vm.example.com", Pager: "/home/me/.local/bin/pager", Options: []string{"-o", "IdentitiesOnly=yes"}},
			},
			"box": {
				Peer: "box", RemotePeer: "laptop-2",
				PollOutbound: time.Second, PollInbound: time.Minute,
				Hint: "start the box container",
				Exec: &Exec{
					Argv: []string{"docker", "exec", "-i", "box", "/usr/local/bin/pager"},
					Env:  map[string]string{"PAGER_DB": "/data/msg.db"},
				},
			},
		},
		Notify: Notify{Command: []string{"/usr/local/bin/my-notifier", "{link}", "{state}", "{hint}"}},
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Errorf("Load =\n%+v\nwant\n%+v", cfg, want)
	}

	if _, err := Load(filepath.Join(t.TempDir(), "missing.toml")); err == nil || !strings.Contains(err.Error(), "missing.toml") {
		t.Errorf("Load of a missing file: %v, want an error naming it", err)
	}
}
