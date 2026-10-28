package service

import (
	"strings"
	"testing"
	"unicode/utf16"
)

func decodeUTF16LE(t *testing.T, b []byte) string {
	t.Helper()
	if len(b) < 2 || b[0] != 0xFF || b[1] != 0xFE || len(b)%2 != 0 {
		t.Fatalf("not UTF-16LE with a BOM: % x", b[:min(len(b), 8)])
	}
	u := make([]uint16, 0, len(b)/2-1)
	for i := 2; i < len(b); i += 2 {
		u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(u))
}

func TestTaskXML(t *testing.T) {
	raw, err := TaskXML(Task{
		Name:        "switchboard",
		Description: "relay <pager> & more",
		UserID:      `EXAMPLE\me`,
		Exe:         `C:\Program Files\switchboard\switchboardw.exe`,
		Args:        []string{"run", "--config", `C:\Users\me\AppData\Roaming\switchboard\config.toml`, `with "quotes"`},
		WorkDir:     `C:\Users\me`,
	})
	if err != nil {
		t.Fatal(err)
	}
	x := decodeUTF16LE(t, raw)
	for _, want := range []string{
		`<?xml version="1.0" encoding="UTF-16"?>`,
		"<LogonTrigger>\n      <Enabled>true</Enabled>\n      <UserId>EXAMPLE\\me</UserId>",
		"<Principal id=\"Author\">\n      <UserId>EXAMPLE\\me</UserId>",
		"<LogonType>InteractiveToken</LogonType>",
		"<RunLevel>LeastPrivilege</RunLevel>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>", // the default would stop it after 72h
		"<MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>",
		"<DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>",
		"<StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>",
		"<Interval>PT1M</Interval>",
		"<Description>relay &lt;pager&gt; &amp; more</Description>",
		`<Command>C:\Program Files\switchboard\switchboardw.exe</Command>`,
		`<Arguments>run --config C:\Users\me\AppData\Roaming\switchboard\config.toml &#34;with \&#34;quotes\&#34;&#34;</Arguments>`,
		`<WorkingDirectory>C:\Users\me</WorkingDirectory>`,
	} {
		if !strings.Contains(x, want) {
			t.Errorf("task XML lacks %s\n%s", want, x)
		}
	}
}

func TestTaskXMLNeedsAUser(t *testing.T) {
	if _, err := TaskXML(Task{Name: "switchboard", Exe: `C:\sb\switchboardw.exe`}); err == nil {
		t.Fatal("a task without a user would fire on any user's logon")
	}
}

func TestQuoteWindowsArg(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:            `plain`,
		`C:\dir\file.toml`: `C:\dir\file.toml`,
		`two words`:        `"two words"`,
		``:                 `""`,
		`say "hi"`:         `"say \"hi\""`,
		`C:\dir with\`:     `"C:\dir with\\"`, // a trailing backslash would escape the closing quote
		`a\\"b`:            `a\\\\\"b`,        // backslashes before a quote are doubled, plus one
		`x"y`:              `x\"y`,            // a quote alone does not make it quoted
		`a\b c`:            `"a\b c"`,         // backslashes elsewhere are literal
		"tab\there":        "\"tab\there\"",
	} {
		if got := quoteWindowsArg(in); got != want {
			t.Errorf("quoteWindowsArg(%q) = %q, want %q", in, got, want)
		}
	}
}
