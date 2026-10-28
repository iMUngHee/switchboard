package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures are assembled at run time, so this file never contains the text
// it plants — the tool scans its own repository too.
var (
	realEmail = "alice" + "@" + "corp-mail" + ".io"
	realHome  = "/" + "home" + "/" + "alice"
	keyHeader = "-----BEGIN " + "RSA PRIVATE KEY-----"
	marker    = "Hyg" + "1ene" + "Marker" // matched by the private pattern below, in any case
	pattern   = "hyg1ene" + "marker"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=test", "-c", "user.email=test@example.com"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// repo creates a git repository with the given files staged.
func repo(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, dir, "add", "-f", name)
	}
	return dir
}

func writePatterns(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".agents", "hygiene-patterns"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runIn(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(args, &out, &errb)
	return code, out.String() + errb.String()
}

func TestCleanRepoPasses(t *testing.T) {
	dir := repo(t, map[string]string{
		"README.md": "mail me@example.com or you@corp.test, see /home/me and C:\\Users\\me\n",
	})
	if code, out := runIn(t, dir); code != 0 || !strings.Contains(out, "hygiene: clean") {
		t.Fatalf("clean repo: exit %d\n%s", code, out)
	}
}

func TestPlaceholderDoesNotHideARealMatch(t *testing.T) {
	for name, line := range map[string]string{
		"email": "me@example.com or " + realEmail,
		"home":  "/home/me and " + realHome + "/src",
	} {
		t.Run(name, func(t *testing.T) {
			dir := repo(t, map[string]string{"notes.txt": line + "\n"})
			if code, out := runIn(t, dir); code != 1 {
				t.Fatalf("exit %d, want 1\n%s", code, out)
			}
		})
	}
}

func TestPlaceholderInThePathDoesNotHideTheContent(t *testing.T) {
	dir := repo(t, map[string]string{"docs/home/me/n.md": "see " + realHome + "\n"})
	if code, out := runIn(t, dir); code != 1 || !strings.Contains(out, "docs/home/me/n.md:1") {
		t.Fatalf("exit %d, want 1 naming the file\n%s", code, out)
	}
}

func TestEveryGenericRuleFires(t *testing.T) {
	for name, body := range map[string]string{
		"email":   "contact " + realEmail,
		"home":    "path " + realHome + "/x",
		"winhome": "path C:\\Users\\" + "alice\\x",
		"key":     keyHeader,
		"token":   "token ghp_" + strings.Repeat("a", 36),
	} {
		t.Run(name, func(t *testing.T) {
			dir := repo(t, map[string]string{"f.txt": body + "\n"})
			if code, out := runIn(t, dir); code != 1 {
				t.Fatalf("exit %d, want 1\n%s", code, out)
			}
		})
	}
}

func TestPrivatePatternsIgnoreCaseAndCRLF(t *testing.T) {
	dir := repo(t, map[string]string{"f.txt": "a line naming " + strings.ToUpper(marker) + "\n"})
	writePatterns(t, dir, "# private\r\n"+pattern+"\r\n")
	if code, out := runIn(t, dir); code != 1 || !strings.Contains(out, "f.txt:1") {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
}

func TestAPrivateNameInAPathIsFound(t *testing.T) {
	dir := repo(t, map[string]string{"notes-" + marker + ".txt": "nothing here\n"})
	writePatterns(t, dir, pattern+"\n")
	if code, out := runIn(t, dir); code != 1 || !strings.Contains(out, "(in the path)") {
		t.Fatalf("exit %d, want 1 for the path\n%s", code, out)
	}
}

func TestBinaryFilesAreScanned(t *testing.T) {
	dir := repo(t, map[string]string{"blob.bin": "\x00\x01\x02" + realEmail + "\x00"})
	if code, out := runIn(t, dir); code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
}

func TestALeakOnlyInTheIndexIsFound(t *testing.T) {
	dir := repo(t, map[string]string{"gone.txt": "contact " + realEmail + "\n"})
	if err := os.Remove(filepath.Join(dir, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	if code, out := runIn(t, dir); code != 1 || !strings.Contains(out, "(index)") {
		t.Fatalf("exit %d, want 1 from the index\n%s", code, out)
	}
}

func TestTrackedAgentsFails(t *testing.T) {
	dir := repo(t, map[string]string{".agents/plan.md": "plans\n"})
	if code, out := runIn(t, dir); code != 1 || !strings.Contains(out, "tracked under .agents/") {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
}

func TestTheScanFailsWhenItCannotRun(t *testing.T) {
	t.Run("invalid pattern", func(t *testing.T) {
		dir := repo(t, map[string]string{"f.txt": "x\n"})
		writePatterns(t, dir, "bad(\n")
		if code, out := runIn(t, dir); code != 2 {
			t.Fatalf("exit %d, want 2\n%s", code, out)
		}
	})
	t.Run("not a repository", func(t *testing.T) {
		if code, out := runIn(t, t.TempDir()); code != 2 {
			t.Fatalf("exit %d, want 2\n%s", code, out)
		}
	})
	t.Run("missing directory", func(t *testing.T) {
		if code, out := runIn(t, filepath.Join(t.TempDir(), "nope")); code != 2 {
			t.Fatalf("exit %d, want 2\n%s", code, out)
		}
	})
}

func TestRequirePrivate(t *testing.T) {
	dir := repo(t, map[string]string{"f.txt": "x\n"})
	if code, out := runIn(t, "-require-private", dir); code != 2 {
		t.Fatalf("without a pattern file: exit %d, want 2\n%s", code, out)
	}
	if code, out := runIn(t, dir); code != 0 || !strings.Contains(out, "private names were not checked") {
		t.Fatalf("without the flag: exit %d, want 0 with a note\n%s", code, out)
	}
	writePatterns(t, dir, pattern+"\n")
	if code, out := runIn(t, "-require-private", dir); code != 0 {
		t.Fatalf("with a pattern file: exit %d, want 0\n%s", code, out)
	}
}

// A linked worktree has its own .agents/; the patterns kept in the main
// checkout must still apply.
func TestAWorktreeUsesTheMainCheckoutsPatterns(t *testing.T) {
	main := repo(t, map[string]string{"README.md": "hello\n"})
	gitIn(t, main, "commit", "-q", "-m", "start")
	writePatterns(t, main, pattern+"\n")
	wt := filepath.Join(t.TempDir(), "wt")
	gitIn(t, main, "worktree", "add", "-q", wt)
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte(marker+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, wt, "add", "f.txt")
	if code, out := runIn(t, "-require-private", wt); code != 1 || !strings.Contains(out, "f.txt:1") {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
}

func TestAPatternFileWithABOMStillWorks(t *testing.T) {
	dir := repo(t, map[string]string{"f.txt": marker + "\n"})
	writePatterns(t, dir, "\ufeff"+pattern+"\n")
	if code, out := runIn(t, dir); code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
}

func TestAPrivateNameWrappedAcrossLinesIsFound(t *testing.T) {
	half := len(marker) / 2
	for name, body := range map[string]string{
		"plain":   marker[:half] + "\n" + marker[half:] + "\n",
		"comment": "// " + marker[:half] + "\n// " + marker[half:] + "\n",
		"quote":   "> " + marker[:half] + "\n> " + marker[half:] + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			dir := repo(t, map[string]string{"f.txt": body})
			writePatterns(t, dir, pattern+"\n")
			if code, out := runIn(t, dir); code != 1 || !strings.Contains(out, "across a line break") {
				t.Fatalf("exit %d, want 1 across a line break\n%s", code, out)
			}
		})
	}
}

func TestHomeDirectoriesIgnoreCase(t *testing.T) {
	dir := repo(t, map[string]string{"f.txt": "path /" + "users/alice/x\n"})
	if code, out := runIn(t, dir); code != 1 {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
}

func TestANestedAgentsDirectoryIsCaught(t *testing.T) {
	dir := repo(t, map[string]string{"sub/.agents/notes.md": "notes\n"})
	if code, out := runIn(t, dir); code != 1 || !strings.Contains(out, "tracked under .agents/") {
		t.Fatalf("exit %d, want 1\n%s", code, out)
	}
}

func TestHistoryChecksMessagesAndAuthors(t *testing.T) {
	commit := func(t *testing.T, dir, name, msg string) {
		t.Helper()
		gitIn(t, dir, "-c", "user.name="+name, "commit", "-q", "--allow-empty", "-m", msg)
	}
	t.Run("clean", func(t *testing.T) {
		dir := repo(t, map[string]string{"f.txt": "x\n"})
		writePatterns(t, dir, pattern+"\n")
		commit(t, dir, "someone", "Start the repository")
		if code, out := runIn(t, "-history", "HEAD", dir); code != 0 {
			t.Fatalf("exit %d, want 0\n%s", code, out)
		}
	})
	t.Run("message", func(t *testing.T) {
		dir := repo(t, map[string]string{"f.txt": "x\n"})
		writePatterns(t, dir, pattern+"\n")
		commit(t, dir, "someone", "Fix the "+marker+" relay")
		if code, out := runIn(t, "-history", "HEAD", dir); code != 1 || !strings.Contains(out, "commit ") {
			t.Fatalf("exit %d, want 1 naming the commit\n%s", code, out)
		}
	})
	t.Run("generic rule in the message", func(t *testing.T) {
		dir := repo(t, map[string]string{"f.txt": "x\n"})
		commit(t, dir, "someone", "Read "+realHome+"/src")
		if code, out := runIn(t, "-history", "HEAD", dir); code != 1 || !strings.Contains(out, "home directory") {
			t.Fatalf("exit %d, want 1 for the path\n%s", code, out)
		}
	})
	t.Run("author", func(t *testing.T) {
		dir := repo(t, map[string]string{"f.txt": "x\n"})
		writePatterns(t, dir, pattern+"\n")
		commit(t, dir, marker, "Start")
		if code, out := runIn(t, "-history", "HEAD", dir); code != 1 {
			t.Fatalf("exit %d, want 1\n%s", code, out)
		}
	})
}
