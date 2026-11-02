package service

import (
	"path/filepath"
	"slices"
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

// TestTaskStatusAsksPowerShell: the status check is one Get-ScheduledTask
// query for the named task in the root folder, run by Windows PowerShell
// from System32; a name that could change the script is refused before it
// is written in, and only a lone "registered" or "absent" is an answer.
func TestTaskStatusAsksPowerShell(t *testing.T) {
	t.Setenv("SystemRoot", `C:\Windows`)
	path, args, err := taskQuery("switchboard")
	if err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"-NoProfile", "-NonInteractive", "-Command",
		`try { $null = Get-ScheduledTask -TaskName 'switchboard' -TaskPath '\' -ErrorAction Stop; 'registered' } ` +
			`catch { if ($_.FullyQualifiedErrorId -like 'CmdletizationQuery_NotFound*') { 'absent' } else { throw } }`}
	if want := filepath.Join(`C:\Windows`, "System32", "WindowsPowerShell", "v1.0", "powershell.exe"); path != want || !slices.Equal(args, wantArgs) {
		t.Errorf("runs\n %q %q\nwant\n %q %q", path, args, want, wantArgs)
	}

	for _, name := range []string{"", "sw'x", "sw\u2019x", "sw`x", "$x", "sw*", "sw?", "sw[1]", `Folder\sw`, "sw x", "-sw", ".sw", "sw\n"} {
		if p, a, err := taskQuery(name); err == nil {
			t.Errorf("taskQuery(%q) = %q %q, not refused", name, p, a)
		}
	}
	t.Setenv("SystemRoot", "")
	if p, _, err := taskQuery("switchboard"); err == nil {
		t.Errorf("without SystemRoot it runs %q", p)
	}

	for _, tc := range []struct {
		out        string
		registered bool
		fail       bool
	}{
		{"registered\r\n", true, false},
		{"absent\r\n", false, false},
		{"registered", true, false},
		{"absent\n", false, false},
		{"", false, true},
		{"\r\n", false, true},
		{"Registered\r\n", false, true},
		{"absent\r\nregistered\r\n", false, true},
		{"WARNING: something\r\nabsent\r\n", false, true},
		{"not absent\r\n", false, true},
	} {
		registered, err := taskAnswer(tc.out)
		if registered != tc.registered || (err != nil) != tc.fail {
			t.Errorf("taskAnswer(%q) = %v, %v; want %v, failing %v", tc.out, registered, err, tc.registered, tc.fail)
		}
	}
}
