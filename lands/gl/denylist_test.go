//go:build conformance || tripwire

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The contract's hygiene denylist (../contract/hygiene_denylist.txt), read the
// way the contract's own hygiene_test.go reads it, for the conformance suite's
// response check and the strings tripwire over the gl binary.

// denyRule is one line of the contract's hygiene_denylist.txt.
type denyRule struct {
	line string
	sub  string
	re   *regexp.Regexp
}

func loadDenylist(t *testing.T) []denyRule {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "contract", "hygiene_denylist.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var rules []denyRule
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if expr, ok := strings.CutPrefix(line, "re:"); ok {
			re, err := regexp.Compile(expr)
			if err != nil {
				t.Fatalf("denylist %q: %v", line, err)
			}
			rules = append(rules, denyRule{line: line, re: re})
			continue
		}
		rules = append(rules, denyRule{line: line, sub: strings.ToLower(line)})
	}
	if len(rules) == 0 {
		t.Fatal("the denylist has no rules")
	}
	return rules
}

// findings returns what in text breaks a rule. A regex match that is one of
// allowed (the viewing land's own ids: a land's ids are the one accepted
// leak, gd-EWSKPH7pa5 §9 question 1, until lands mint their own prefix) is
// not a finding.
func findings(rules []denyRule, text string, allowed map[string]bool) []string {
	var out []string
	lower := strings.ToLower(text)
	for _, r := range rules {
		if r.re == nil {
			if strings.Contains(lower, r.sub) {
				out = append(out, r.line)
			}
			continue
		}
		for _, m := range r.re.FindAllString(text, -1) {
			if !allowed[m] {
				out = append(out, r.line+" ("+m+")")
				break
			}
		}
	}
	return out
}
