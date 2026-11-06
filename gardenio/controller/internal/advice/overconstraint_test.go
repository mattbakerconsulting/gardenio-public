package advice

import (
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// ocBead builds an advice bead with the given id, title, and description for the
// over-constraint checks. Labels are irrelevant to these checks (they scan
// text, not targeting) so they are left empty.
func ocBead(id, title, description string) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID: id, Title: title, Type: "advice", Status: "open",
		Description: description,
	}
}

// findingsByCheck indexes findings by their check id for assertion.
func findingsByCheck(fs []OverConstraintFinding) map[string][]OverConstraintFinding {
	out := map[string][]OverConstraintFinding{}
	for _, f := range fs {
		out[f.Check] = append(out[f.Check], f)
	}
	return out
}

// findingsByID indexes findings by bead id.
func findingsByID(fs []OverConstraintFinding) map[string][]OverConstraintFinding {
	out := map[string][]OverConstraintFinding{}
	for _, f := range fs {
		out[f.ID] = append(out[f.ID], f)
	}
	return out
}

// TestLintOverConstraint_AllThreeViolationTypes exercises the three checks in
// one corpus and asserts each fires (and clean beads yield nothing). This is the
// ">= 3 different violation types" acceptance criterion.
func TestLintOverConstraint_AllThreeViolationTypes(t *testing.T) {
	// A reference corpus the redundancy check compares against — a chunk of
	// "injected workflow-context". The redundant bead below quotes a long run
	// of this verbatim (enough 6-word shingles to clear the min-shingle gate).
	ref := "When you finish work on this codebase you must run make build and make test and make lint " +
		"and then run helm lint against the chart before you open a merge request against the main branch. " +
		"Never push your commits directly to the main branch because that bypasses review and can break the fleet."

	beads := []*beadsapi.BeadDetail{
		// 1. rules-over-judgment framing.
		ocBead("gd-judgment",
			"Release policy",
			"Under no circumstances may you tag a release without a green pipeline. "+
				"There are no exceptions to this rule."),

		// 2. directive-density spike — stack of emphatic imperatives in a short bead.
		ocBead("gd-density",
			"Commit discipline",
			"You MUST run the tests. NEVER skip the linter. Do NOT force-push. "+
				"ALWAYS rebase. This is CRITICAL and IMPORTANT."),

		// 3. system-prompt redundancy — restates the reference corpus.
		ocBead("gd-redundant",
			"How to land a change",
			"When you finish work on this codebase you must run make build and make test and make lint "+
				"and then run helm lint against the chart before you open a merge request against the main branch. "+
				"Always follow that sequence exactly and keep the pipeline green before you ask for a review."),

		// Clean bead — normal actionable advice, no smell.
		ocBead("gd-clean",
			"Prefer gb mr link",
			"After opening a merge request, link it to the bead so the viewer can "+
				"track its pipeline and surface review comments to you."),
	}

	findings := LintOverConstraint(beads, ref, OverConstraintConfig{})
	byCheck := findingsByCheck(findings)
	byID := findingsByID(findings)

	if len(byCheck[OverConstraintRulesOverJudgment]) == 0 {
		t.Errorf("expected a rules-over-judgment finding, got none (all findings: %+v)", findings)
	}
	if len(byCheck[OverConstraintDirectiveDensity]) == 0 {
		t.Errorf("expected a directive-density finding, got none (all findings: %+v)", findings)
	}
	if len(byCheck[OverConstraintRedundancy]) == 0 {
		t.Errorf("expected a redundancy finding, got none (all findings: %+v)", findings)
	}

	// The clean bead must yield nothing.
	if len(byID["gd-clean"]) != 0 {
		t.Errorf("clean bead flagged: %+v", byID["gd-clean"])
	}

	// Every finding is WARN and carries the required fields (severity, id,
	// line, snippet, rule) from the acceptance criteria.
	for _, f := range findings {
		if f.Severity != LintWarn {
			t.Errorf("%s: severity = %q, want warn", f.ID, f.Severity)
		}
		if f.ID == "" || f.Rule == "" {
			t.Errorf("finding missing id/rule: %+v", f)
		}
	}
}

// TestLintOverConstraint_RulesOverJudgment checks phrase detection, the reported
// line/snippet, and that multiple distinct phrases each yield a finding (ka-0000004072).
func TestLintOverConstraint_RulesOverJudgment(t *testing.T) {
	tests := []struct {
		name    string
		desc    string
		wantHit bool
		wantN   int // expected number of rules-over-judgment findings
	}{
		{"under-no-circumstances", "Under no circumstances edit prod.", true, 1},
		{"no-exceptions", "Always add the label. No exceptions.", true, 1},
		{"without-exception", "Every MR needs a proof block, without exception.", true, 1},
		{"strictly-forbidden", "Direct pushes to main are strictly forbidden.", true, 1},
		{"never-ever", "Never, ever commit a secret.", true, 1},
		{"follow-exactly", "Here is the runbook; follow it exactly.", true, 1},
		{"two-distinct-phrases", "Under no circumstances skip CI. No exceptions.", true, 2},
		{"clean", "Prefer a feature branch and open an MR when the change is ready.", false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := ocBead("gd-x", "t", tt.desc)
			// Pass "" ref so only the judgment check is exercised here; keep the
			// directive-density check from firing by using low-directive text.
			findings := LintOverConstraint([]*beadsapi.BeadDetail{b}, "", OverConstraintConfig{})
			var judg []OverConstraintFinding
			for _, f := range findings {
				if f.Check == OverConstraintRulesOverJudgment {
					judg = append(judg, f)
				}
			}
			if tt.wantHit && len(judg) != tt.wantN {
				t.Errorf("%q: got %d rules-over-judgment findings, want %d (%+v)", tt.desc, len(judg), tt.wantN, judg)
			}
			if !tt.wantHit && len(judg) != 0 {
				t.Errorf("%q: unexpected rules-over-judgment findings %+v", tt.desc, judg)
			}
		})
	}
}

// TestLintOverConstraint_DirectiveDensity checks both spike modes: the absolute
// high-count trigger and the density+min-count trigger, plus that a long,
// low-density bead with a couple of directives is NOT flagged.
func TestLintOverConstraint_DirectiveDensity(t *testing.T) {
	cfg := DefaultOverConstraintConfig()

	// High absolute count in a longer bead: 9 markers, well over HighCount(8).
	highCount := ocBead("gd-highcount", "big",
		"MUST MUST MUST NEVER NEVER NEVER CRITICAL IMPORTANT REQUIRED "+
			strings.Repeat("padding words here that are not directives at all. ", 40))
	// Short, dense bead: 5 markers in ~30 tokens → density well over 6/1k and
	// count over the min of 4.
	dense := ocBead("gd-dense", "small",
		"MUST do this. NEVER that. Do NOT this. ALWAYS the other. CRITICAL note.")
	// Long bead, only 2 directives → low density, under min count → NOT flagged.
	sparse := ocBead("gd-sparse", "sparse",
		"You MUST read the runbook first. "+
			strings.Repeat("This is ordinary prose with no shouting whatsoever. ", 60)+
			"Do NOT forget to close the bead.")

	findings := LintOverConstraint([]*beadsapi.BeadDetail{highCount, dense, sparse}, "", cfg)
	byID := findingsByID(findings)

	for _, id := range []string{"gd-highcount", "gd-dense"} {
		var found bool
		for _, f := range byID[id] {
			if f.Check == OverConstraintDirectiveDensity {
				found = true
				if f.DirectiveCount == 0 {
					t.Errorf("%s: directive-density finding missing DirectiveCount", id)
				}
			}
		}
		if !found {
			t.Errorf("%s: expected a directive-density finding, got %+v", id, byID[id])
		}
	}

	for _, f := range byID["gd-sparse"] {
		if f.Check == OverConstraintDirectiveDensity {
			t.Errorf("gd-sparse: unexpectedly flagged for directive density: %+v", f)
		}
	}
}

// TestLintOverConstraint_Redundancy checks the shingle-overlap check: a bead
// that quotes the reference corpus is flagged, a bead that shares only stock
// short phrases is not, and passing an empty ref skips the check entirely.
func TestLintOverConstraint_Redundancy(t *testing.T) {
	ref := "The gardenio controller is the single source of truth for agent credentials. " +
		"Account beads define credential accounts with a provider an env key and an api key. " +
		"The controller watches account bead events and syncs them to the coopmux credential broker " +
		"which stores the api keys and distributes them to sessions as profiles with pool load balancing."

	redundant := ocBead("gd-redundant", "creds",
		"The gardenio controller is the single source of truth for agent credentials. "+
			"Account beads define credential accounts with a provider an env key and an api key. "+
			"The controller watches account bead events and syncs them to the coopmux credential broker "+
			"which then hands the keys to each running session at startup time.")
	original := ocBead("gd-original", "creds-note",
		"Rotate the shared pool key every quarter and page the on call operator whenever a session "+
			"stalls on an expired token in the middle of a long running database migration job. "+
			"Prefer scheduling those migrations during a low traffic window and snapshot the volume first "+
			"so that a failed run can be rolled back quickly without any data loss for the tenant.")

	// With a ref corpus, the redundant bead is flagged; the original is not.
	findings := LintOverConstraint([]*beadsapi.BeadDetail{redundant, original}, ref, OverConstraintConfig{})
	byID := findingsByID(findings)

	var redFound bool
	for _, f := range byID["gd-redundant"] {
		if f.Check == OverConstraintRedundancy {
			redFound = true
		}
	}
	if !redFound {
		t.Errorf("gd-redundant: expected a redundancy finding, got %+v", byID["gd-redundant"])
	}
	for _, f := range byID["gd-original"] {
		if f.Check == OverConstraintRedundancy {
			t.Errorf("gd-original: unexpectedly flagged for redundancy: %+v", f)
		}
	}

	// With an empty ref corpus, the redundancy check is skipped entirely.
	skipFindings := LintOverConstraint([]*beadsapi.BeadDetail{redundant}, "", OverConstraintConfig{})
	for _, f := range skipFindings {
		if f.Check == OverConstraintRedundancy {
			t.Errorf("empty ref corpus should skip redundancy check, got %+v", f)
		}
	}
}

// TestLintOverConstraint_SkipsUndeliverable verifies closed / expired / fixture
// beads are never scanned — their framing can't reach an agent, mirroring the
// targeting lint's skip set.
func TestLintOverConstraint_SkipsUndeliverable(t *testing.T) {
	shouty := "You MUST. NEVER. Do NOT. ALWAYS. CRITICAL. IMPORTANT. REQUIRED. Under no circumstances."

	closed := ocBead("gd-closed", "t", shouty)
	closed.Status = "closed"
	fixture := ocBead("gd-fixture", "t", shouty)
	fixture.Labels = []string{"fixture:e2e"}
	expired := ocBead("gd-expired", "t", shouty)
	expired.Fields = map[string]string{"expires_at": "2000-01-01T00:00:00Z"}

	findings := LintOverConstraint([]*beadsapi.BeadDetail{closed, fixture, expired, nil}, "", OverConstraintConfig{})
	if len(findings) != 0 {
		t.Errorf("undeliverable beads should yield no findings, got %+v", findings)
	}
}

// TestCountDirectiveMarkers pins the marker-counting definition shared by the
// CLI and the budget test.
func TestCountDirectiveMarkers(t *testing.T) {
	got := CountDirectiveMarkers("MUST NEVER Do NOT do NOT DO NOT ALWAYS CRITICAL Prohibited IMPORTANT REQUIRED WARNING must never")
	// MUST(1) NEVER(1) "Do NOT"/"do NOT"/"DO NOT"(3) ALWAYS(1) CRITICAL(1)
	// Prohibited(1) IMPORTANT(1) REQUIRED(1) WARNING(1) = 11. Lowercase
	// must/never don't count (word-boundary uppercase only).
	if got != 11 {
		t.Errorf("CountDirectiveMarkers = %d, want 11", got)
	}
}

// TestLintOverConstraint_ZeroConfigUsesDefaults verifies passing the zero-value
// config is equivalent to DefaultOverConstraintConfig (the CLI relies on this; ka-0000004072).
func TestLintOverConstraint_ZeroConfigUsesDefaults(t *testing.T) {
	dense := ocBead("gd-dense", "small",
		"MUST do this. NEVER that. Do NOT this. ALWAYS the other. CRITICAL note.")
	zero := LintOverConstraint([]*beadsapi.BeadDetail{dense}, "", OverConstraintConfig{})
	explicit := LintOverConstraint([]*beadsapi.BeadDetail{dense}, "", DefaultOverConstraintConfig())
	if len(zero) != len(explicit) {
		t.Errorf("zero-config produced %d findings, default-config %d — should match", len(zero), len(explicit))
	}
}
