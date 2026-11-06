//go:build tripwire

package main

// The strings tripwire (Door D.11, task gd-oQdHmmwZFW; contract design
// gd-EWSKPH7pa5 §3 and §8 step 9): the printable strings of a release gl
// binary, run against the contract's hygiene denylist. It is advisory, not
// the guard. Gzipped descriptors and reflection hide strings from it, and
// code gl shares (the sign-in session's field names, the web approval link)
// carries tokens like "daemon" and "bead" that gl never says to a tenant, so
// a finding is a GitHub warning to read, never a failure. deps_test.go is the
// guard.
//
// What does fail: no binary, an unreadable denylist, or a scan that cannot
// be of gl (too few strings, or none of them gl's contract version). A
// tripwire that silently scans nothing is the false green this repo has
// shipped before (checks.yml, the internal-strings-audit note).
//
//	CGO_ENABLED=0 GOWORK=off go build -trimpath -o /tmp/gl . &&
//	GL_TRIPWIRE_BIN=/tmp/gl GOWORK=off go test -tags tripwire -run TestStringsTripwire -count=1 -v .

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"gardenio/lands/contract/landsv1"
)

// printableStrings is strings(1): runs of at least min printable ASCII bytes.
func printableStrings(b []byte, min int) []string {
	var out []string
	start := -1
	for i := 0; i <= len(b); i++ {
		if i < len(b) && (b[i] == '\t' || (b[i] >= 0x20 && b[i] < 0x7f)) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 && i-start >= min {
			out = append(out, string(b[start:i]))
		}
		start = -1
	}
	return out
}

func TestStringsTripwire(t *testing.T) {
	bin := os.Getenv("GL_TRIPWIRE_BIN")
	if bin == "" {
		t.Fatal("the tripwire build tag needs GL_TRIPWIRE_BIN, a gl binary built the way a release is")
	}
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	rules := loadDenylist(t)
	strs := printableStrings(raw, 4)
	if len(strs) < 1000 {
		t.Fatalf("%s holds %d strings; that is not a Go binary", bin, len(strs))
	}
	versionSeen := false
	hits := map[string][]string{}
	for _, s := range strs {
		if strings.Contains(s, landsv1.ContractVersion) {
			versionSeen = true
		}
		for _, f := range findings(rules, s, nil) {
			hits[f] = append(hits[f], s)
		}
	}
	if !versionSeen {
		t.Fatalf("no string in %s carries the contract version %s; it is not gl, or the scan is broken", bin, landsv1.ContractVersion)
	}

	rulesHit := make([]string, 0, len(hits))
	for r := range hits {
		rulesHit = append(rulesHit, r)
	}
	sort.Strings(rulesHit)
	for _, r := range rulesHit {
		ex := hits[r]
		sample := make([]string, 0, 3)
		for _, s := range ex[:min(3, len(ex))] {
			if len(s) > 80 {
				s = s[:80] + "..."
			}
			sample = append(sample, fmt.Sprintf("%q", s))
		}
		// A workflow command on stdout: GitHub shows it on the run's summary.
		fmt.Printf("::warning title=gl strings tripwire (advisory)::denylist %s: %d strings, e.g. %s\n",
			strings.ReplaceAll(r, "%", "%25"), len(ex), strings.ReplaceAll(strings.Join(sample, ", "), "%", "%25"))
	}
	t.Logf("tripwire: %d strings in %s, %d of %d denylist rules hit (advisory)", len(strs), bin, len(rulesHit), len(rules))
}
