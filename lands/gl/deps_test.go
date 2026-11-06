package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gl is what a stranger runs on their own machine: it may know the Lands
// contract and the shared sign-in, and nothing of what stands behind the
// land (gd-EWSKPH7pa5 section 3, Dependency guard).
var allowedRepoPackages = map[string]bool{
	"gardenio/lands/gl":               true,
	"gardenio/lands/gl.test":          true,
	"gardenio/lands/contract/landsv1": true,
	"gardenio/clicreds/oidclogin":     true,
}

func TestNoBackingStoreDependencies(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-test", "./...")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		n++
		pkg, _, _ := strings.Cut(strings.TrimSpace(line), " ") // drop a test variant's " [...]"
		if strings.HasPrefix(pkg, "gardenio/") && !allowedRepoPackages[pkg] {
			t.Errorf("depends on %s; gl may import only the contract binding and the shared sign-in", pkg)
		}
		if strings.Contains(pkg, "beads") || strings.Contains(pkg, "controller") || strings.Contains(pkg, "coop") {
			t.Errorf("depends on %s", pkg)
		}
	}
	if n == 0 {
		t.Fatal("go list -deps printed nothing")
	}
}

// The module paths are dotless, so the in-repo modules are found by replace;
// no other replace may appear.
func TestGoModReplacesOnlyInRepoModules(t *testing.T) {
	want := map[string]string{
		"gardenio/clicreds":       "../../gardenio/clicreds",
		"gardenio/lands/contract": "../contract",
	}
	f, err := os.Open("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		rest, ok := strings.CutPrefix(line, "replace")
		if !ok {
			continue
		}
		from, to, ok := strings.Cut(rest, "=>")
		if !ok {
			t.Errorf("go.mod: %q: a replace block; write one replace per line", line)
			continue
		}
		got[strings.TrimSpace(from)] = strings.TrimSpace(to)
	}
	if len(got) != len(want) {
		t.Errorf("replaces = %v, want exactly %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("replace %s => %q, want %q", k, got[k], v)
		}
	}
}

// Nothing from the repo is compiled into gl by go:embed.
func TestNoEmbed(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range files {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `"embed"`) || strings.Contains(string(b), "//go:embed") {
			t.Errorf("%s uses go:embed", p)
		}
	}
}
