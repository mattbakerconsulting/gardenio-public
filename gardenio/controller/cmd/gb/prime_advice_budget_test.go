package main

// prime_advice_budget_test.go exercises the hard byte-cap on the non-inline
// portion of the advice section (step 5 of epic ka-0000008080). See
// prime_advice.go:outputAdvice and adviceBudgetBytes.

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// renderAdviceBudget mirrors the budget-cap logic inside outputAdvice so tests
// can exercise it without standing up a daemon. Inline beads render first and
// bypass the budget; abbreviated beads (title + summary + pointer) accumulate
// into a buffer until adding the next would exceed `budget`. Beads that don't
// fit fall through to title-only rendering up to `titleBudget` so every advice
// surfaces at least as a title; the rest collapse to a "... N more" footer.
// Keep this in sync with outputAdvice.
func renderAdviceBudget(w *bytes.Buffer, agentID, header string, beads []*beadsapi.BeadDetail, budget, titleBudget int) {
	var regular, titles bytes.Buffer
	type overflowItem struct{ bead *beadsapi.BeadDetail }
	var overflow []overflowItem
	for _, b := range beads {
		if isInlineAdvice(b) {
			renderInlineAdviceItem(w, header, b)
			continue
		}
		var one bytes.Buffer
		renderAbbrevAdviceItem(&one, header, b)
		if regular.Len()+one.Len() > budget {
			overflow = append(overflow, overflowItem{bead: b})
			continue
		}
		_, _ = one.WriteTo(&regular)
	}
	_, _ = regular.WriteTo(w)
	truncated := 0
	for _, ov := range overflow {
		var one bytes.Buffer
		renderTitleOnlyAdviceItem(&one, header, ov.bead)
		if titles.Len()+one.Len() > titleBudget {
			truncated++
			continue
		}
		_, _ = one.WriteTo(&titles)
	}
	_, _ = titles.WriteTo(w)
	if truncated > 0 {
		fmt.Fprintf(w, "... %d more advice items not shown (budget exceeded). Run `gb advice list --for %s` to see all.\n\n",
			truncated, agentID)
	}
}

// TestAdviceBudgetBytes_DefaultAndOverride verifies the default byte budget
// and the GB_PRIME_ADVICE_BUDGET_BYTES env-var override.
func TestAdviceBudgetBytes_DefaultAndOverride(t *testing.T) {
	t.Run("default", func(t *testing.T) {
		t.Setenv(primeAdviceBudgetEnv, "")
		if got := adviceBudgetBytes(""); got != primeAdviceByteBudget {
			t.Errorf("adviceBudgetBytes() = %d, want default %d", got, primeAdviceByteBudget)
		}
	})
	t.Run("valid override", func(t *testing.T) {
		t.Setenv(primeAdviceBudgetEnv, "2048")
		if got := adviceBudgetBytes(""); got != 2048 {
			t.Errorf("adviceBudgetBytes() with override = %d, want 2048", got)
		}
	})
	t.Run("invalid override falls back to default", func(t *testing.T) {
		t.Setenv(primeAdviceBudgetEnv, "not-a-number")
		if got := adviceBudgetBytes(""); got != primeAdviceByteBudget {
			t.Errorf("adviceBudgetBytes() with garbage = %d, want default %d", got, primeAdviceByteBudget)
		}
	})
	t.Run("non-positive override falls back to default", func(t *testing.T) {
		t.Setenv(primeAdviceBudgetEnv, "0")
		if got := adviceBudgetBytes(""); got != primeAdviceByteBudget {
			t.Errorf("adviceBudgetBytes() with 0 = %d, want default %d", got, primeAdviceByteBudget)
		}
	})
}

// TestOutputAdvice_BudgetTruncatesAndFooters asserts the budget cap truncates
// abbreviated items once the running byte count would exceed the budget, that
// overflow items render as title-only lines up to the title budget, and that
// items past both budgets collapse to a single "... N more" footer.
func TestOutputAdvice_BudgetTruncatesAndFooters(t *testing.T) {
	// 50 abbreviated beads — abbreviated render ~80 bytes each, title-only
	// ~50 bytes each. With budget=256 (≈3 abbreviated) and titleBudget=256
	// (≈5 title-only), most beads should overflow into "... N more".
	var beads []*beadsapi.BeadDetail
	for i := range 50 {
		beads = append(beads, &beadsapi.BeadDetail{
			ID:          fmt.Sprintf("gd-abbrev%03d", i),
			Title:       fmt.Sprintf("Rule %d", i),
			Description: fmt.Sprintf("Summary line for rule %d.\nMore body.", i),
			Labels:      []string{"global"},
		})
	}

	var buf bytes.Buffer
	renderAdviceBudget(&buf, "gardenio/crews/test", "Global", beads, 256, 256)
	got := buf.String()

	if !strings.Contains(got, "more advice items not shown (budget exceeded)") {
		t.Errorf("budget cap footer missing:\n%s", got)
	}
	if !strings.Contains(got, "`gb advice list --for gardenio/crews/test`") {
		t.Errorf("budget cap footer missing agent-id pointer:\n%s", got)
	}
	if c := strings.Count(got, "budget exceeded"); c != 1 {
		t.Errorf("expected exactly 1 budget-exceeded footer, got %d:\n%s", c, got)
	}
	footerIdx := strings.Index(got, "... ")
	if footerIdx < 0 {
		t.Fatalf("footer not found in output:\n%s", got)
	}
	// Budgets are 256 abbrev + 256 titles = 512 bytes max before footer.
	if footerIdx > 512 {
		t.Errorf("content before footer exceeded combined budget: %d bytes > 512\n%s", footerIdx, got)
	}
}

// TestOutputAdvice_OverflowRendersAsTitles asserts that when abbreviated
// budget is small but the title budget is generous, every bead surfaces at
// least as a title line — the explicit ask in the user feedback that prompted
// the two-tier render.
func TestOutputAdvice_OverflowRendersAsTitles(t *testing.T) {
	var beads []*beadsapi.BeadDetail
	for i := range 50 {
		beads = append(beads, &beadsapi.BeadDetail{
			ID:          fmt.Sprintf("gd-abbrev%03d", i),
			Title:       fmt.Sprintf("Rule %d", i),
			Description: fmt.Sprintf("Summary for rule %d.\nMore body.", i),
			Labels:      []string{"global"},
		})
	}

	var buf bytes.Buffer
	// Tiny abbreviated budget forces all 50 to overflow; large title budget
	// fits all 50 title-only lines.
	renderAdviceBudget(&buf, "gardenio/crews/test", "Global", beads, 10, 16384)
	got := buf.String()

	if strings.Contains(got, "budget exceeded") {
		t.Errorf("title-budget large enough; should not have truncated:\n%s", got)
	}
	for i := range 50 {
		want := fmt.Sprintf("`gb advice show gd-abbrev%03d`", i)
		if !strings.Contains(got, want) {
			t.Errorf("missing pointer for bead %d (%q):\n%s", i, want, got)
		}
	}
	// Overflow lines must be the compact title-only form (no "Summary for
	// rule N." text in the rendered body).
	if strings.Contains(got, "Summary for rule") {
		t.Errorf("overflow rendered as abbreviated form (with summary) instead of title-only:\n%s", got)
	}
}

// TestOutputAdvice_InlineBypassesBudget asserts inline:true beads always
// render in full regardless of the byte budget — they're the irreducible
// must-inline policy.
func TestOutputAdvice_InlineBypassesBudget(t *testing.T) {
	longBody := strings.Repeat("critical policy detail line that must be visible. ", 20)
	inline := &beadsapi.BeadDetail{
		ID:          "gd-inline-policy",
		Title:       "NEVER write to FSM JIRA project",
		Description: longBody,
		Labels:      []string{"global", "inline:true"},
	}
	var abbrevs []*beadsapi.BeadDetail
	for i := range 5 {
		abbrevs = append(abbrevs, &beadsapi.BeadDetail{
			ID:          fmt.Sprintf("gd-abbrev%03d", i),
			Title:       fmt.Sprintf("Lesser rule %d", i),
			Description: fmt.Sprintf("Body %d", i),
			Labels:      []string{"global"},
		})
	}

	beads := append([]*beadsapi.BeadDetail{inline}, abbrevs...)

	var buf bytes.Buffer
	// Both budgets zero so every non-inline bead is dropped.
	renderAdviceBudget(&buf, "gardenio/crews/test", "Global", beads, 0, 0)
	got := buf.String()

	if !strings.Contains(got, "NEVER write to FSM JIRA project") {
		t.Errorf("inline title dropped under budget=0:\n%s", got)
	}
	if !strings.Contains(got, "critical policy detail line") {
		t.Errorf("inline body truncated under budget=0:\n%s", got)
	}
	if !strings.Contains(got, "... 5 more advice items not shown") {
		t.Errorf("expected footer showing 5 truncated abbreviated items:\n%s", got)
	}
}

// TestOutputAdvice_SmallCatalogNoFooter asserts that when the entire catalog
// fits within the budget, no footer is appended.
func TestOutputAdvice_SmallCatalogNoFooter(t *testing.T) {
	beads := []*beadsapi.BeadDetail{
		{ID: "gd-a", Title: "Rule A", Description: "Summary A.", Labels: []string{"global"}},
		{ID: "gd-b", Title: "Rule B", Description: "Summary B.", Labels: []string{"global"}},
		{ID: "gd-c", Title: "Rule C", Description: "Summary C.", Labels: []string{"global"}},
	}

	var buf bytes.Buffer
	renderAdviceBudget(&buf, "gardenio/crews/test", "Global", beads, primeAdviceByteBudget, primeAdviceTitleByteBudget)
	got := buf.String()

	if strings.Contains(got, "budget exceeded") {
		t.Errorf("small catalog should not emit footer:\n%s", got)
	}
	if !strings.Contains(got, "`gb advice show gd-a`") {
		t.Errorf("small catalog missing first bead pointer:\n%s", got)
	}
	if !strings.Contains(got, "`gb advice show gd-c`") {
		t.Errorf("small catalog missing last bead pointer:\n%s", got)
	}
}

// TestOutputAdvice_BudgetWithEnvOverride exercises the end-to-end path via
// outputAdvice against a stubbed daemon, asserting the
// GB_PRIME_ADVICE_BUDGET_BYTES env var lowers the cap and triggers the
// footer.
func TestOutputAdvice_BudgetWithEnvOverride(t *testing.T) {
	var corpus []budgetBead
	for i := range 30 {
		corpus = append(corpus, budgetBead{
			ID:          fmt.Sprintf("gd-env%03d", i),
			Title:       fmt.Sprintf("Global rule env%03d", i),
			Description: fmt.Sprintf("One-line summary for env%03d.\nmore body", i),
			Type:        "advice", Kind: "data", Status: "open",
			Labels: []string{"global"},
			Fields: map[string]string{"confidence": "0.7"},
		})
	}
	cleanup := startBudgetDaemon(t, corpus, nil)
	defer cleanup()

	// Constrain BOTH budgets to force the footer; otherwise overflow items
	// fall through to the title-only tier and all 30 items still render.
	t.Setenv(primeAdviceBudgetEnv, "256")
	t.Setenv(primeAdviceTitleBudgetEnv, "256")

	var buf bytes.Buffer
	outputAdvice(&buf, "gardenio/crews/env-override-test", "crew")
	got := buf.String()

	if !strings.Contains(got, "budget exceeded") {
		t.Errorf("env-override budget did not trigger footer:\n%s", got)
	}
	if !strings.Contains(got, "`gb advice list --for gardenio/crews/env-override-test`") {
		t.Errorf("footer missing agent pointer:\n%s", got)
	}
}

// TestOutputAdvice_StressTotalPrimeUnder100KB stress-tests the byte budget:
// with 200 advice beads matching an agent, the rendered prime output must
// stay within the combined ~100 KB cap (60 KB abbreviated + 20 KB title-only
// + other prime sections + slack). The advice budgets were bumped to
// ~20k tokens per ka-0000006677.
func TestOutputAdvice_StressTotalPrimeUnder100KB(t *testing.T) {
	var corpus []budgetBead
	for i := range 200 {
		corpus = append(corpus, budgetBead{
			ID:          fmt.Sprintf("gd-stress%03d", i),
			Title:       fmt.Sprintf("Stress rule #%d — long-ish title to stress abbreviated render", i),
			Description: strings.Repeat(fmt.Sprintf("Summary line %d with realistic body length. ", i), 4),
			Type:        "advice", Kind: "data", Status: "open",
			Labels: []string{"global"},
			Fields: map[string]string{"confidence": "0.7"},
		})
	}
	cleanup := startBudgetDaemon(t, corpus, budgetConfigBeads())
	defer cleanup()

	oldFor, oldNoAdvice, oldSize := primeForAgent, primeNoAdvice, primeSizeOnly
	primeForAgent = "gardenio/crews/stress-test"
	primeNoAdvice = false
	primeSizeOnly = false
	defer func() {
		primeForAgent, primeNoAdvice, primeSizeOnly = oldFor, oldNoAdvice, oldSize
	}()

	t.Setenv("BOAT_ROLE", "crew")
	t.Setenv("BOAT_PROJECT", "gardenio")
	t.Setenv("KD_ACTOR", "")
	t.Setenv("KD_AGENT_ID", "")
	t.Setenv("BOAT_TASK_ID", "skip-auto-assign")

	var stdout, stderr bytes.Buffer
	if err := runPrimeTo(&stdout, &stderr); err != nil {
		t.Fatalf("runPrimeTo: %v", err)
	}

	total := stdout.Len()
	// 60 KB abbrev + 20 KB title-only + ~3 KB other prime + slack ≈ 85 KB.
	// 100 KB hard ceiling guards against unbounded growth at the new 20k-token
	// advice budget (ka-0000006677).
	if total >= 102400 {
		t.Errorf("stress test: prime total=%d bytes exceeds 100KB combined cap with 200 advice beads — hard ceiling not holding", total)
	}
	// Spot-check coverage at the volume extremes — beads 0 and 199 should
	// both surface (either abbreviated or title-only) under the bumped
	// budgets that accommodate the entire synthetic corpus.
	for _, i := range []int{0, 199} {
		want := fmt.Sprintf("`gb advice show gd-stress%03d`", i)
		if !bytes.Contains(stdout.Bytes(), []byte(want)) {
			t.Errorf("stress test: bead %d missing pointer %q in prime output", i, want)
		}
	}
	_ = stderr
}
