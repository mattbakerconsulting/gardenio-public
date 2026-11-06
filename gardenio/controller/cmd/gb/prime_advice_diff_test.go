package main

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"
)

func TestDetectRoleMismatch_NoMismatch(t *testing.T) {
	labels := []string{"project:gardenio", "role:crew"}
	got := detectRoleMismatch(labels, "crew")
	if got != "" {
		t.Errorf("expected no mismatch, got %q", got)
	}
}

func TestDetectRoleMismatch_PluralMatch(t *testing.T) {
	// Agent has role "crew", bead has "role:crews" (plural) — should not mismatch
	// because Singularize("crews") = "crew".
	labels := []string{"role:crews"}
	got := detectRoleMismatch(labels, "crew")
	if got != "" {
		t.Errorf("expected no mismatch for plural form, got %q", got)
	}
}

func TestDetectRoleMismatch_DifferentRole(t *testing.T) {
	labels := []string{"project:gardenio", "role:lead"}
	got := detectRoleMismatch(labels, "crew")
	if got != "lead" {
		t.Errorf("expected mismatch role=lead, got %q", got)
	}
}

func TestDetectRoleMismatch_MultiRole(t *testing.T) {
	// Agent has "crew,thread" — both roles should be accepted.
	labels := []string{"role:thread"}
	got := detectRoleMismatch(labels, "crew,thread")
	if got != "" {
		t.Errorf("expected no mismatch for multi-role agent, got %q", got)
	}
}

func TestDetectRoleMismatch_NoRoleLabel(t *testing.T) {
	labels := []string{"project:gardenio"}
	got := detectRoleMismatch(labels, "crew")
	if got != "" {
		t.Errorf("expected no mismatch when bead has no role label, got %q", got)
	}
}

func TestParseRoles(t *testing.T) {
	roles := parseRoles("crew,thread")
	if !roles["crew"] {
		t.Error("expected crew in parsed roles")
	}
	if !roles["thread"] {
		t.Error("expected thread in parsed roles")
	}
	// Singularized forms should also be present.
	if !roles["crew"] { // "crew" is already singular
		t.Error("expected singular crew in parsed roles")
	}
}

func TestParseRoles_SingleRole(t *testing.T) {
	roles := parseRoles("crews")
	if !roles["crews"] {
		t.Error("expected crews in parsed roles")
	}
	if !roles["crew"] {
		t.Error("expected singular crew in parsed roles")
	}
}

// TestOutputAdviceDiffItems_BudgetTruncates ensures the role-diff render path
// (outputAdviceDiffItems) honors the shared abbreviated + title-only +
// "... N more" budget cascade. Without the budget the diff could emit full
// titles + descriptions for an arbitrarily large catalog and blow the 32 KB
// stress ceiling guarded by TestOutputAdvice_StressTotalPrimeUnder32KB.
func TestOutputAdviceDiffItems_BudgetTruncates(t *testing.T) {
	var items []advice.MatchedAdvice
	for i := range 50 {
		items = append(items, advice.MatchedAdvice{
			Scope: "global", ScopeTarget: "", ScopeHeader: "Global",
			Bead: &beadsapi.BeadDetail{
				ID:          fmt.Sprintf("gd-diff%03d", i),
				Title:       fmt.Sprintf("Diff rule %d", i),
				Description: fmt.Sprintf("Summary for diff rule %d.\nMore body.", i),
				Labels:      []string{"global"},
			},
		})
	}

	t.Setenv(primeAdviceBudgetEnv, "256")
	t.Setenv(primeAdviceTitleBudgetEnv, "256")

	var buf bytes.Buffer
	outputAdviceDiffItems(&buf, "gardenio/crews/diff-test", items)
	got := buf.String()

	if !strings.Contains(got, "budget exceeded") {
		t.Errorf("diff renderer: budget cap footer missing under tight budget:\n%s", got)
	}
	if !strings.Contains(got, "`gb advice list --for gardenio/crews/diff-test`") {
		t.Errorf("diff renderer: footer missing agent-id pointer:\n%s", got)
	}
	if c := strings.Count(got, "budget exceeded"); c != 1 {
		t.Errorf("diff renderer: expected exactly 1 budget-exceeded footer, got %d", c)
	}
}

// TestOutputAdviceDiffItems_StressUnder32KB stress-tests the role-diff render
// path mirroring TestOutputAdvice_StressTotalPrimeUnder32KB. With 200 matched
// diff items, the diff output alone must stay well under the 32 KB combined
// prime ceiling so role-mismatch diffs cannot blow the cap.
func TestOutputAdviceDiffItems_StressUnder32KB(t *testing.T) {
	var items []advice.MatchedAdvice
	for i := range 200 {
		items = append(items, advice.MatchedAdvice{
			Scope: "global", ScopeTarget: "", ScopeHeader: "Global",
			Bead: &beadsapi.BeadDetail{
				ID:          fmt.Sprintf("gd-diff%03d", i),
				Title:       fmt.Sprintf("Stress diff rule #%d — long-ish title to stress abbreviated render", i),
				Description: strings.Repeat(fmt.Sprintf("Summary line %d with realistic body length. ", i), 4),
				Labels:      []string{"global"},
			},
		})
	}

	t.Setenv(primeAdviceBudgetEnv, "")
	t.Setenv(primeAdviceTitleBudgetEnv, "")

	var buf bytes.Buffer
	outputAdviceDiffItems(&buf, "gardenio/crews/diff-stress", items)

	total := buf.Len()
	// Default budget is 4 KB abbreviated + 3 KB title-only + small footer.
	// 16 KB ceiling leaves plenty of headroom and guards against an unbounded
	// regression that drops budget enforcement on this path.
	if total >= 16384 {
		t.Errorf("diff stress: total=%d bytes exceeds 16KB cap with 200 items — diff budget not holding", total)
	}
	if !bytes.Contains(buf.Bytes(), []byte("budget exceeded")) {
		t.Errorf("diff stress: expected truncation footer at 200 items:\n%s", buf.String())
	}
}
