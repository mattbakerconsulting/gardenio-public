package contract

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The contract module must never reach the backing store's code: it is what
// gl and the web client build against (gd-EWSKPH7pa5 section 3, Dependency
// guard). It sits outside go.work so a workspace member cannot slip an import
// in; these tests make the same promise hold if someone adds it anyway.
func TestNoBackingStoreDependencies(t *testing.T) {
	cmd := exec.Command("go", "list", "-deps", "-test", "./...")
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps: %v\n%s", err, out)
	}
	n := 0
	for _, pkg := range strings.Fields(string(out)) {
		n++
		if strings.HasPrefix(pkg, "gardenio/") && !strings.HasPrefix(pkg, "gardenio/lands/contract") {
			t.Errorf("depends on %s; the contract may import nothing else from this repo", pkg)
		}
		if strings.Contains(pkg, "beads") {
			t.Errorf("depends on %s", pkg)
		}
	}
	if n == 0 {
		t.Fatal("go list -deps printed nothing")
	}
}

func TestGoModHasNoReplace(t *testing.T) {
	f, err := os.Open("go.mod")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if strings.HasPrefix(strings.TrimSpace(sc.Text()), "replace") {
			t.Errorf("go.mod: %q; the contract module takes no replace directives", sc.Text())
		}
	}
}

func TestNoEmbed(t *testing.T) {
	err := filepath.WalkDir(".", func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == "node_modules" {
			return filepath.SkipDir
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		raw, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "//go:embed") {
				continue
			}
			// The one embed: the denylist the map scans live answers with,
			// a file of this module (denylist.go).
			if p == "denylist.go" && strings.TrimSpace(line) == "//go:embed hygiene_denylist.txt" {
				continue
			}
			t.Errorf("%s: %s: go:embed is not allowed in the contract module", p, strings.TrimSpace(line))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
