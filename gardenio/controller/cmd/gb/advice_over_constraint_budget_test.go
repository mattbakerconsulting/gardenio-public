package main

// advice_over_constraint_budget_test.go is the durable CI guard for the
// QUALITATIVE over-constraint dimension (task ka-0000004072, P4 of the
// Opus/Fable-5 context-audit epic ka-0000006604). It is the direct analog of
// real_catalog_budget_test.go (matched-token budget) and
// prime_directive_density_budget_test.go (rendered-prime directive density):
// where those pin QUANTITATIVE budgets, this pins the count of OVER-CONSTRAINT
// smells in the committed advice corpus so a regression — a new bead that
// stacks shout-directives, frames a rule as a no-discretion absolute, or
// restates the injected workflow-context — turns CI red.
//
// It runs the exact production checks (advice.LintOverConstraint) over the same
// frozen catalog snapshot real_catalog_budget_test.go audits
// (testdata/advice_catalog_snapshot.json), with the redundancy check fed the
// REAL injected config text — the claude-instructions + nudge-prompts config
// beads bridge.EnsureConfigs seeds — so a bead that duplicates the system
// prompt is caught against the genuine reference, not a stand-in.
//
// Enforcement is a per-check frozen baseline plus a small headroom: the test
// fails if the count of any check's findings climbs past baseline + headroom.
// When an increase is deliberate (a legitimately emphatic new rule), re-measure
// from the BASELINE log line this test emits and bump the baseline in the same
// MR, with a note on why the extra imperative pressure is justified. The
// baseline is a ceiling to ratchet DOWN as the corpus is de-constrained (P3 of
// the epic), not a target to grow.

import (
	"context"
	"io"
	"log/slog"
	"sort"
	"strings"
	"testing"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/bridge"
)

// overConstraintBaseline pins the per-check finding count against the committed
// advice snapshot. headroom is the slack a check may grow before the guard trips
// — set to 1 so a single new borderline bead is a soft nudge (bump the baseline
// with justification) rather than an instant red, but a cluster of new
// over-constrained beads fails fast.
//
// Ratcheted DOWN 2026-07-25 (P3 remediation gd-PqV9LSSW4E): the de-constraint
// pass closed a dead-agent gardener prompt (ka-0000002635, a directive-density
// offender) and de-shouted an over-emphatic gardenio gardener rule, dropping
// directive-density 6→5 and rules-over-judgment 5→4 against the refreshed
// snapshot. Per this test's contract the baseline is a ceiling to ratchet DOWN
// as the corpus is de-constrained, so it is lowered to the new counts to lock in
// the win and re-close the phantom headroom a stale-high baseline leaves.
//
// Ratcheted to 0 across the board at the 2026-11-01 snapshot refresh
// (gd-FQqvaIw8SH): none of the 26 live advice beads trips a check.
var overConstraintBaseline = map[string]int{
	advice.OverConstraintRulesOverJudgment: 0,
	advice.OverConstraintDirectiveDensity:  0,
	advice.OverConstraintRedundancy:        0,
}

const overConstraintHeadroom = 1

// buildRealRefCorpus renders the canonical claude-instructions + nudge-prompts
// config beads bridge.EnsureConfigs seeds and concatenates their descriptions,
// reproducing the reference corpus the CLI's buildRefCorpus assembles from a
// live daemon — but from the in-memory production seed, so CI needs no daemon.
func buildRealRefCorpus(t *testing.T) string {
	t.Helper()
	setter := &densityConfigSetter{}
	if err := bridge.EnsureConfigs(context.Background(), setter, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("EnsureConfigs: %v", err)
	}
	want := map[string]bool{"claude-instructions": true, "nudge-prompts": true}
	var b strings.Builder
	for _, bead := range setter.beads {
		if bead == nil || !want[bead.Title] {
			continue
		}
		b.WriteString(bead.Description)
		b.WriteByte('\n')
	}
	return b.String()
}

// TestRealCatalogOverConstraintBudget audits the committed real-catalog snapshot
// through advice.LintOverConstraint — the exact checks `gb advice lint
// --over-constraint` runs — and fails CI if any check's finding count climbs
// past its frozen baseline + headroom. This is the durable regression guard P4
// installs: a re-leak of over-constrained framing into the advice corpus turns
// the frozen snapshot's finding count up and reds CI.
func TestRealCatalogOverConstraintBudget(t *testing.T) {
	if *updateAdviceSnapshot {
		t.Skip("snapshot regeneration handled by TestRealCatalogPerArchetypeBudget")
	}
	beads := loadAdviceSnapshot(t)
	ref := buildRealRefCorpus(t)
	if len(ref) < 2000 {
		t.Fatalf("reference corpus suspiciously small (%d bytes) — EnsureConfigs seed likely broke", len(ref))
	}

	findings := advice.LintOverConstraint(beads, ref, advice.OverConstraintConfig{})

	// Tally per check and log the worst offender per check so a maintainer can
	// re-baseline from the emitted numbers.
	counts := map[string]int{}
	worst := map[string]advice.OverConstraintFinding{}
	for _, f := range findings {
		counts[f.Check]++
		if cur, ok := worst[f.Check]; !ok || f.DirectiveCount > cur.DirectiveCount {
			worst[f.Check] = f
		}
	}

	for _, check := range sortedStringKeys(overConstraintBaseline) {
		got := counts[check]
		base := overConstraintBaseline[check]
		w := worst[check]
		t.Logf("BASELINE %s: findings=%d (baseline %d, cap %d); worst=%s %q line=%d count=%d density=%.2f",
			strings.TrimPrefix(check, "over-constraint:"), got, base, base+overConstraintHeadroom,
			w.ID, elideForLog(w.Snippet, 60), w.Line, w.DirectiveCount, w.DirectiveDensity)
	}

	// A check that appears in the findings but NOT in the baseline map is a new
	// dimension someone added without a baseline — fail loudly.
	for check, got := range counts {
		if _, ok := overConstraintBaseline[check]; !ok {
			t.Errorf("check %q produced %d findings but has no baseline entry — add it to overConstraintBaseline", check, got)
		}
	}

	for _, check := range sortedStringKeys(overConstraintBaseline) {
		got := counts[check]
		cap := overConstraintBaseline[check] + overConstraintHeadroom
		if got > cap {
			t.Errorf("%s: %d over-constraint finding(s) > cap %d (baseline %d + %d headroom) — a re-leak of over-constrained advice framing; audit with `gb advice lint --over-constraint`, de-constrain the offending bead(s), or bump the baseline in-MR with justification",
				strings.TrimPrefix(check, "over-constraint:"), got, cap,
				overConstraintBaseline[check], overConstraintHeadroom)
		}
	}
}

// sortedStringKeys returns the keys of a string-keyed int map sorted, for
// deterministic iteration/logging.
func sortedStringKeys(m map[string]int) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

// elideForLog trims a snippet for the BASELINE log line.
func elideForLog(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}
