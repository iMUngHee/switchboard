// Command notices writes the license texts of everything linked into
// switchboard besides its own code: the Go runtime and standard library, and
// every module the binary depends on. Release archives carry its output as
// THIRD_PARTY_NOTICES. It fails when a license cannot be found, so a release
// never ships without one.
//
//	go run ./tools/notices [-pkg ./cmd/switchboard] > THIRD_PARTY_NOTICES
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
	"sort"
	"strings"
)

// licenseNames are the file names a license is looked for under, in order.
var licenseNames = []string{"LICENSE", "LICENSE.txt", "LICENSE.md", "COPYING"}

type component struct {
	name, version, dir string
}

func main() {
	pkg := flag.String("pkg", "./cmd/switchboard", "the package whose dependencies are listed")
	flag.Parse()
	if err := write(os.Stdout, *pkg); err != nil {
		fmt.Fprintln(os.Stderr, "notices:", err)
		os.Exit(1)
	}
}

func write(w io.Writer, pkg string) error {
	comps, err := components(pkg)
	if err != nil {
		return err
	}
	return render(w, comps)
}

// render writes each component's license, or fails at the first one that
// has none.
func render(w io.Writer, comps []component) error {
	fmt.Fprintln(w, "switchboard includes the following software. Each is listed with its license.")
	for _, c := range comps {
		text, err := license(c.dir)
		if err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
		fmt.Fprintf(w, "\n================================================================\n%s %s\n================================================================\n\n%s\n",
			c.name, c.version, bytes.TrimSpace(text))
	}
	return nil
}

// components lists Go itself, then every module pkg links, sorted, without
// the module pkg belongs to.
func components(pkg string) ([]component, error) {
	goroot, err := output("go", "env", "GOROOT")
	if err != nil {
		return nil, err
	}
	goversion, err := output("go", "env", "GOVERSION")
	if err != nil {
		return nil, err
	}
	main, err := output("go", "list", "-m")
	if err != nil {
		return nil, err
	}
	deps, err := output("go", "list", "-deps", "-f", "{{with .Module}}{{.Path}}\t{{.Version}}\t{{.Dir}}{{end}}", pkg)
	if err != nil {
		return nil, err
	}
	seen := map[string]component{}
	sc := bufio.NewScanner(strings.NewReader(deps))
	for sc.Scan() {
		f := strings.Split(sc.Text(), "\t")
		if len(f) != 3 || f[0] == main {
			continue
		}
		seen[f[0]] = component{f[0], f[1], f[2]}
	}
	var mods []component
	for _, c := range seen {
		mods = append(mods, c)
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].name < mods[j].name })
	return append([]component{{"Go (runtime and standard library)", goversion, goroot}}, mods...), nil
}

// license reads the license file in dir. A Go installed by some package
// managers keeps it one directory above GOROOT.
func license(dir string) ([]byte, error) {
	for _, d := range []string{dir, filepath.Dir(dir)} {
		for _, n := range licenseNames {
			if b, err := os.ReadFile(filepath.Join(d, n)); err == nil {
				return b, nil
			}
		}
	}
	return nil, errors.New("no license file in " + dir)
}

func output(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return "", fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, ee.Stderr)
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
