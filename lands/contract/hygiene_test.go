package contract

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const denylistPath = "hygiene_denylist.txt"

// hygieneTargets are the files a tenant's client is built from or shown.
// The scenario fixtures are tenant answers too. Tests, docs and the denylist
// itself are not scanned: they name the tokens on purpose.
func hygieneTargets(t *testing.T) []string {
	t.Helper()
	targets := []string{specPath, "landsv1/landsv1.gen.go", "ts/lands-v1.d.ts", "ts/fixtures.check.ts"}
	err := filepath.WalkDir(fixturesDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == invalidDir {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".json") {
			targets = append(targets, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	scenarios, err := filepath.Glob(filepath.Join(scenariosDir, "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	return append(targets, scenarios...)
}

type denyRule struct {
	text string
	re   *regexp.Regexp
}

func loadDenylist(t *testing.T) []denyRule {
	t.Helper()
	f, err := os.Open(denylistPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rules []denyRule
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		r := denyRule{text: line}
		if pat, ok := strings.CutPrefix(line, "re:"); ok {
			r.re = regexp.MustCompile(pat)
		} else {
			r.text = strings.ToLower(line)
		}
		rules = append(rules, r)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(rules) == 0 {
		t.Fatalf("%s has no rules", denylistPath)
	}
	return rules
}

func (r denyRule) match(line string) bool {
	if r.re != nil {
		return r.re.MatchString(line)
	}
	return strings.Contains(strings.ToLower(line), r.text)
}

func TestHygieneDenylist(t *testing.T) {
	rules := loadDenylist(t)
	for _, path := range hygieneTargets(t) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v (regenerate with `make generate-lands-contract`)", path, err)
			continue
		}
		for i, line := range strings.Split(string(raw), "\n") {
			for _, r := range rules {
				if r.match(line) {
					t.Errorf("%s:%d: denylisted %q: %s", path, i+1, r.text, strings.TrimSpace(line))
				}
			}
		}
	}
}

// The denylist must actually bite: each rule matches its own canary.
func TestHygieneDenylistBites(t *testing.T) {
	canaries := map[string]string{
		`\b(gd|ka)-[A-Za-z0-9]{10}\b`:                                            "see gd-EWSKPH7pa5",
		`/v1/(decisions|escalations|events|bus|search|accounts|plans|threads)\b`: "POST /v1/decisions/x/resolve",
	}
	for _, r := range loadDenylist(t) {
		canary := "x " + r.text + " x"
		if r.re != nil {
			c, ok := canaries[r.re.String()]
			if !ok {
				t.Errorf("regex rule %q has no canary in TestHygieneDenylistBites", r.re)
				continue
			}
			canary = c
		}
		if !r.match(canary) {
			t.Errorf("rule %q does not match its canary %q", r.text, canary)
		}
	}
	if (denyRule{re: regexp.MustCompile(`\b(gd|ka)-[A-Za-z0-9]{10}\b`)}).match("or-3kQ9vXw2Lp") {
		t.Error("a land's own prefix must not trip the fleet-id rule")
	}
}
