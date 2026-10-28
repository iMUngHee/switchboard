// Command hygiene fails when anything git would publish from this repository
// carries a private detail.
//
// It checks every path in the index and the working tree, every blob in the
// index and every tracked file in the working tree — text or binary — against
// two kinds of rule:
//
//   - generic rules, below: email addresses outside the reserved example
//     domains, real home directories, private-key headers and common token
//     formats. A placeholder (me@example.com, /home/me) is recognised from the
//     matched text alone, so a placeholder never hides a real match on the same
//     line;
//   - private rules, one Go regexp per line in .agents/hygiene-patterns, matched
//     case-insensitively. That file is untracked by design: the names a
//     maintainer must keep out of this repository must never be written into
//     it, not even as a pattern. It is looked up in the checkout being scanned
//     and in the main checkout, since a linked worktree has its own .agents/.
//
// Private rules also run over a copy of each file with line breaks — and the
// comment or quote markers that start a continued line — removed, so a name
// wrapped across two lines is still found.
//
// It also fails when anything under a .agents/ directory is tracked.
//
// With -history REV, it checks the commits REV would publish instead: author
// and committer names and emails against the private rules, and each commit
// message against every rule (the identity emails are public by design, so
// the generic email rule does not apply to them).
//
// A private name of two words should be written as `first\s*second`, so it is
// still found when a line break falls at the space.
//
// What it cannot see: text encoded as UTF-16, and a name deliberately broken up
// with invisible characters or URL encoding.
//
//	go run ./tools/hygiene [-require-private] [-history REV] [repo-dir]
//
// Exit status: 0 clean, 1 something found, 2 the scan could not run.
package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// rule is one thing that must not be published. A match that also matches ok
// in full is a placeholder.
type rule struct {
	name    string
	re      *regexp.Regexp
	ok      *regexp.Regexp
	private bool // also checked across line breaks, and in commit history
}

// lineJoin removes a line break together with the indentation and the comment
// or quote marker that may start the continued line.
var lineJoin = regexp.MustCompile(`[ \t]*\r?\n[ \t]*(//+|#+|;+|--|\*+|>+)?[ \t]*`)

func genericRules() []rule {
	return []rule{
		{
			name: "email address",
			re:   regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`),
			ok:   regexp.MustCompile(`^[^@]+@([A-Za-z0-9-]+\.)*(example\.(com|org|net)|[A-Za-z0-9-]+\.(example|test|invalid|localhost))$`),
		},
		{
			name: "home directory",
			re:   regexp.MustCompile(`(?i)/(users|home)/[a-z0-9._-]+`),
			ok:   regexp.MustCompile(`(?i)^/(users|home)/(me|you|user|runner|example)$`),
		},
		{
			name: "Windows home directory",
			re:   regexp.MustCompile(`(?i)[a-z]:\\+users\\+[a-z0-9._-]+`),
			ok:   regexp.MustCompile(`(?i)^[a-z]:\\+users\\+(me|you|user|runneradmin|example)$`),
		},
		{
			name: "private key",
			re:   regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
		},
		{
			name: "access token",
			re:   regexp.MustCompile(`(gh[pousr]_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{22,}|xox[baprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{16})`),
		},
	}
}

// privateRules reads one case-insensitive regexp per line; blank lines and
// lines starting with '#' are skipped, and a trailing CR is dropped.
func privateRules(path string) ([]rule, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var rules []rule
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		text := sc.Text()
		if n == 1 {
			text = strings.TrimPrefix(text, "\ufeff") // a BOM would hide the first pattern
		}
		line := strings.TrimSpace(strings.TrimSuffix(text, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		re, err := regexp.Compile("(?i)" + line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, n, err)
		}
		rules = append(rules, rule{name: "private pattern " + strconv.Quote(line), re: re, private: true})
	}
	return rules, sc.Err()
}

type finding struct {
	where string // "path", "path:line" or "path:line (index)"
	rule  string
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("hygiene", flag.ContinueOnError)
	fs.SetOutput(stderr)
	requirePrivate := fs.Bool("require-private", false, "fail when no .agents/hygiene-patterns is found")
	history := fs.String("history", "", "check the commits of this revision range instead of the files")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 1 {
		fmt.Fprintln(stderr, "usage: hygiene [-require-private] [repo-dir]")
		return 2
	}
	dir := "."
	if fs.NArg() == 1 {
		dir = fs.Arg(0)
	}

	var (
		findings     []finding
		privateFiles []string
		err          error
	)
	if *history != "" {
		findings, privateFiles, err = scanHistory(dir, *history)
	} else {
		findings, privateFiles, err = scan(dir)
	}
	if err != nil {
		fmt.Fprintln(stderr, "hygiene:", err)
		return 2
	}
	if len(privateFiles) == 0 {
		if *requirePrivate {
			fmt.Fprintln(stderr, "hygiene: no .agents/hygiene-patterns found, and -require-private is set")
			return 2
		}
		fmt.Fprintln(stderr, "hygiene: note: no .agents/hygiene-patterns found; private names were not checked")
	}
	if len(findings) > 0 {
		for _, f := range findings {
			fmt.Fprintf(stderr, "hygiene: %s: %s\n", f.where, f.rule)
		}
		return 1
	}
	fmt.Fprintln(stdout, "hygiene: clean")
	return 0
}

// load resolves the checkout and the rules for it: the generic ones plus the
// private patterns found in the checkout or in the main checkout.
func load(dir string) (root string, rules []rule, used []string, err error) {
	top, err := git(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", nil, nil, err
	}
	root = strings.TrimSpace(string(top))
	out, err := git(root, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", nil, nil, err
	}
	common := strings.TrimSpace(string(out))
	if !filepath.IsAbs(common) {
		common = filepath.Join(root, common)
	}
	mainRoot := filepath.Dir(common)

	rules = genericRules()
	seen := map[string]bool{}
	for _, d := range []string{root, mainRoot} {
		p := filepath.Join(d, ".agents", "hygiene-patterns")
		abs, _ := filepath.Abs(p)
		if seen[abs] {
			continue
		}
		seen[abs] = true
		pr, err := privateRules(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", nil, nil, err
		}
		rules = append(rules, pr...)
		used = append(used, abs)
	}
	return root, rules, used, nil
}

// scan returns what it found in the files and the private pattern files used.
func scan(dir string) ([]finding, []string, error) {
	root, rules, used, err := load(dir)
	if err != nil {
		return nil, nil, err
	}

	entries, err := indexEntries(root)
	if err != nil {
		return nil, nil, err
	}
	blobs, err := readBlobs(root, entries)
	if err != nil {
		return nil, nil, err
	}

	var out []finding
	add := func(where, name string) { out = append(out, finding{where, name}) }
	for _, e := range entries {
		if slicesContains(strings.Split(e.path, "/"), ".agents") {
			add(e.path, "tracked under .agents/")
		}
		for _, r := range rules {
			if hit(r, []byte(e.path)) {
				add(e.path, r.name+" (in the path)")
			}
		}
		if blob, ok := blobs[e.sha]; ok {
			check(rules, e.path, " (index)", blob, add)
		}
		wt, err := readWorktree(root, e.path)
		if err != nil {
			return nil, nil, err
		}
		check(rules, e.path, "", wt, add)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].where != out[j].where {
			return out[i].where < out[j].where
		}
		return out[i].rule < out[j].rule
	})
	return dedupe(out), used, nil
}

// check reports every rule's matches in b, and a private rule that matches
// only once the lines are joined.
func check(rules []rule, path, suffix string, b []byte, add func(where, name string)) {
	var joined []byte
	for _, r := range rules {
		lines := hits(r, b)
		for _, line := range lines {
			add(path+":"+strconv.Itoa(line)+suffix, r.name)
		}
		if !r.private {
			continue
		}
		if joined == nil {
			joined = lineJoin.ReplaceAll(b, nil)
		}
		if len(hits(r, joined)) > len(lines) {
			add(path+suffix, r.name+" (across a line break)")
		}
	}
}

// scanHistory checks the commits of rev — messages, author and committer —
// against the private rules.
func scanHistory(dir, rev string) ([]finding, []string, error) {
	root, rules, used, err := load(dir)
	if err != nil {
		return nil, nil, err
	}
	out, err := git(root, "log", "-z", "--format=%H%n%an%n%ae%n%cn%n%ce%n%B", rev, "--")
	if err != nil {
		return nil, nil, err
	}
	var found []finding
	for _, rec := range bytes.Split(out, []byte{0}) {
		if len(bytes.TrimSpace(rec)) == 0 {
			continue
		}
		// sha, author name, author email, committer name, committer email, message.
		parts := bytes.SplitN(bytes.TrimLeft(rec, "\n"), []byte("\n"), 6)
		for len(parts) < 6 {
			parts = append(parts, nil)
		}
		short := string(parts[0])
		if len(short) > 12 {
			short = short[:12]
		}
		identity := bytes.Join(parts[1:5], []byte("\n"))
		message := parts[5]
		for _, r := range rules {
			bad := hit(r, message)
			if r.private {
				bad = bad || hit(r, identity) || hit(r, lineJoin.ReplaceAll(message, nil))
			}
			if bad {
				found = append(found, finding{"commit " + short, r.name})
			}
		}
	}
	return found, used, nil
}

func hit(r rule, b []byte) bool { return len(hits(r, b)) > 0 }

func slicesContains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// hits returns the 1-based line of every match that is not a placeholder.
func hits(r rule, b []byte) []int {
	var lines []int
	for _, loc := range r.re.FindAllIndex(b, -1) {
		if r.ok != nil && r.ok.Match(b[loc[0]:loc[1]]) {
			continue
		}
		lines = append(lines, bytes.Count(b[:loc[0]], []byte("\n"))+1)
	}
	return lines
}

func dedupe(fs []finding) []finding {
	var out []finding
	for i, f := range fs {
		if i > 0 && f == fs[i-1] {
			continue
		}
		out = append(out, f)
	}
	return out
}

type entry struct{ mode, sha, path string }

// indexEntries lists the index, staged additions included. Submodules are
// skipped: their content is another repository's.
func indexEntries(root string) ([]entry, error) {
	raw, err := git(root, "ls-files", "-s", "-z")
	if err != nil {
		return nil, err
	}
	var out []entry
	for _, rec := range bytes.Split(raw, []byte{0}) {
		if len(rec) == 0 {
			continue
		}
		meta, path, ok := bytes.Cut(rec, []byte{'\t'})
		if !ok {
			return nil, fmt.Errorf("unexpected ls-files record %q", rec)
		}
		fields := strings.Fields(string(meta))
		if len(fields) != 3 {
			return nil, fmt.Errorf("unexpected ls-files record %q", rec)
		}
		if fields[0] == "160000" {
			continue
		}
		out = append(out, entry{mode: fields[0], sha: fields[1], path: string(path)})
	}
	return out, nil
}

// readBlobs reads every index blob through one `git cat-file --batch`.
func readBlobs(root string, entries []entry) (map[string][]byte, error) {
	if len(entries) == 0 {
		return map[string][]byte{}, nil
	}
	var in bytes.Buffer
	for _, e := range entries {
		in.WriteString(e.sha + "\n")
	}
	cmd := exec.Command("git", "-C", root, "cat-file", "--batch")
	cmd.Stdin = &in
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	raw, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file: %v: %s", err, strings.TrimSpace(stderr.String()))
	}
	blobs := map[string][]byte{}
	r := bufio.NewReader(bytes.NewReader(raw))
	for range entries {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("git cat-file: short output: %w", err)
		}
		f := strings.Fields(header)
		if len(f) != 3 {
			return nil, fmt.Errorf("git cat-file: unexpected header %q", header)
		}
		size, err := strconv.Atoi(f[2])
		if err != nil {
			return nil, fmt.Errorf("git cat-file: unexpected header %q", header)
		}
		body := make([]byte, size)
		if _, err := io.ReadFull(r, body); err != nil {
			return nil, fmt.Errorf("git cat-file: short blob: %w", err)
		}
		if _, err := r.ReadByte(); err != nil { // the LF after each object
			return nil, fmt.Errorf("git cat-file: short blob: %w", err)
		}
		blobs[f[0]] = body
	}
	return blobs, nil
}

// readWorktree returns the working tree's content for a tracked path: the file,
// a symlink's target, or nothing when it was deleted.
func readWorktree(root, path string) ([]byte, error) {
	p := filepath.Join(root, filepath.FromSlash(path))
	fi, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	switch {
	case fi.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(p)
		return []byte(target), err
	case fi.IsDir():
		return nil, nil
	default:
		return os.ReadFile(p)
	}
}

func git(dir string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
