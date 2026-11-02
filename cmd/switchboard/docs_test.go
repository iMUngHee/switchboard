package main

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/iMUngHee/switchboard/internal/config"
)

var docFiles = []string{"../../README.md", "../../docs/configuration.md", "../../docs/scenarios.md"}

var (
	fenced     = regexp.MustCompile("(?s)```([a-z]*)\n(.*?)```")
	inlineCode = regexp.MustCompile("`([^`\n]+)`")
	invocation = regexp.MustCompile(`\bswitchboard ([a-z]+)((?: [a-z]+)?)`)
	longFlag   = regexp.MustCompile(`(?:^|\s)--([a-z][a-z-]*)`)
	usageFlag  = regexp.MustCompile(`(?m)^\s+-([a-z][a-z-]*)`)
	// localPath is a [pager] path or db written as an absolute path, on
	// Unix or Windows; a relative one is left for Parse to refuse.
	localPath = regexp.MustCompile(`(?m)^(path|db) = (?:"/[^"]*"|'/[^']*'|'[A-Za-z]:\\[^']*')`)
	snakeKey  = regexp.MustCompile("`([a-z]+(?:_[a-z]+)+)`")
	tomlKey   = regexp.MustCompile(`(?m)^\s*([a-z][a-z_]*)\s*=`)
)

// TestDocsMatchCLI: every command and flag the documentation shows exists,
// and every configuration it shows is one switchboard accepts.
func TestDocsMatchCLI(t *testing.T) {
	sandbox(t)
	flagsOf := map[string]map[string]bool{}
	known := func(cmd string) map[string]bool {
		if f, ok := flagsOf[cmd]; ok {
			return f
		}
		code, _, help := cli(t, append(strings.Fields(cmd), "-h")...)
		if code != 0 {
			t.Fatalf("switchboard %s -h: exit %d\n%s", cmd, code, help)
		}
		f := map[string]bool{}
		for _, m := range usageFlag.FindAllStringSubmatch(help, -1) {
			f[m[1]] = true
		}
		flagsOf[cmd] = f
		return f
	}
	commands := map[string]bool{"run": true, "once": true, "check": true, "status": true, "resume": true, "service": true, "version": true}
	services := map[string]bool{"install": true, "uninstall": true, "status": true}

	var invocations, configs int
	keys := map[string]bool{} // every key a configuration in the docs uses
	var prose []struct{ doc, key string }
	for _, doc := range docFiles {
		raw, err := os.ReadFile(doc)
		if err != nil {
			t.Fatal(err)
		}
		// A Windows checkout may have turned every LF into CRLF.
		text := strings.ReplaceAll(string(raw), "\r\n", "\n")
		var code []string
		for _, m := range fenced.FindAllStringSubmatch(text, -1) {
			if m[1] == "toml" {
				configs++
				checkConfig(t, doc, m[2])
				for _, k := range tomlKey.FindAllStringSubmatch(m[2], -1) {
					keys[k[1]] = true
				}
				continue
			}
			code = append(code, strings.Split(m[2], "\n")...)
		}
		outside := fenced.ReplaceAllString(text, "")
		for _, m := range inlineCode.FindAllStringSubmatch(outside, -1) {
			code = append(code, m[1])
		}
		for _, m := range snakeKey.FindAllStringSubmatch(outside, -1) {
			prose = append(prose, struct{ doc, key string }{doc, m[1]})
		}
		for _, line := range code {
			m := invocation.FindStringSubmatchIndex(line)
			if m == nil {
				continue
			}
			invocations++
			cmd, sub := line[m[2]:m[3]], strings.TrimSpace(line[m[4]:m[5]])
			if !commands[cmd] {
				t.Errorf("%s: `%s` names no command", doc, line)
				continue
			}
			name := cmd
			if cmd == "service" {
				if !services[sub] {
					t.Errorf("%s: `%s` names no service subcommand", doc, line)
					continue
				}
				name += " " + sub
			}
			if cmd == "version" {
				continue
			}
			flags := known(name) // -h proves the command exists, flags or not
			for _, f := range longFlag.FindAllStringSubmatch(line[m[1]:], -1) {
				if !flags[f[1]] {
					t.Errorf("%s: `%s`: switchboard %s has no --%s", doc, line, name, f[1])
				}
			}
		}
	}
	// A key named in prose, such as `poll_inbound`, is one the documented
	// configurations, which Parse accepted, actually use.
	for _, p := range prose {
		if !keys[p.key] {
			t.Errorf("%s names `%s`, which no configuration in the docs uses", p.doc, p.key)
		}
	}
	if invocations < 10 || configs < 4 || len(prose) < 3 {
		t.Fatalf("found %d invocations, %d configurations and %d keys in prose: the documentation was not read", invocations, configs, len(prose))
	}
}

// checkConfig parses a configuration the documentation shows. Its local paths
// are written for one system, so they are made absolute on this one first.
func checkConfig(t *testing.T, doc, text string) {
	t.Helper()
	abs := "/usr/local/bin/pager"
	if runtime.GOOS == "windows" {
		abs = `C:\pager\pager.exe`
	}
	text = localPath.ReplaceAllString(text, "$1 = '"+abs+"'")
	if _, err := config.Parse([]byte(text), filepath.Base(doc)); err != nil {
		t.Errorf("%s shows a configuration switchboard refuses: %v\n%s", doc, err, text)
	}
}
