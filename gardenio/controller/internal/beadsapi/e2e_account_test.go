package beadsapi

import "testing"

// TestIsE2EAccountBead exercises each of the three independent exclusion signals
// (title prefix, label prefix, e2e actor) against the ACTUAL naming conventions
// the E2E harnesses use, and — critically — pins that real accounts carrying an
// e2e-ish-looking name/actor are NOT excluded (ka-0000005167 acceptance criterion
// 4: over-filtering real spend is worse than the original bug).
func TestIsE2EAccountBead(t *testing.T) {
	cases := []struct {
		name string
		bead *BeadDetail
		want bool
	}{
		{"nil", nil, false},

		// ── Title-prefix signal (the only signal available at the PromQL layer) ──
		{
			"e2e-acct-primary (test-account-auth.sh — NO e2e: label, actor e2e-test)",
			&BeadDetail{Title: "e2e-acct-primary-abc123", Labels: []string{"project:e2e-test-abc123"}, CreatedBy: "e2e-test"},
			true,
		},
		{
			"e2e-account (test-usage-tracking.sh — only project:gardenio label)",
			&BeadDetail{Title: "e2e-account-TAG42", Labels: []string{"project:gardenio"}, CreatedBy: "e2e-test"},
			true,
		},
		{
			"e2e-cred-a (credential-rotation)",
			&BeadDetail{Title: "e2e-cred-a-cred-rot-1776033747", Labels: []string{"e2e:cred-rot-1776033747", "project:gardenio"}},
			true,
		},
		{
			"probe-acct-$$ (trigger-source-account-fires.toml — does NOT start with e2e-)",
			&BeadDetail{Title: "probe-acct-12345", Labels: []string{"e2e:claudeless", "e2e-tag:e2e-acct-12345-1700"}},
			true,
		},
		{
			"title prefix is case/space insensitive",
			&BeadDetail{Title: "  E2E-Drain-A-tag "},
			true,
		},
		{
			// Pairs with TestCostsRollupE2EAccountExclusion_MatchesRealAccounts: the
			// PromQL matcher carries (?i:…) so BOTH layers agree on an uppercase title.
			// Without that the producer would skip this bead while its retention-tail
			// series survived the rollup/dropdown filter (ka-0000005167).
			"uppercase PROBE-ACCT title",
			&BeadDetail{Title: "PROBE-ACCT-12345"},
			true,
		},

		// ── Label-prefix signal alone ──
		{
			"e2e: label alone, innocuous title",
			&BeadDetail{Title: "some-account", Labels: []string{"e2e:true", "project:gardenio"}},
			true,
		},
		{
			"e2e-tag: label alone",
			&BeadDetail{Title: "some-account", Labels: []string{"e2e-tag:claudeless-run"}},
			true,
		},

		// ── E2E actor signal alone ──
		{
			"run-shard oidc operated_by (e2e-shard)",
			&BeadDetail{Title: "some-account", OperatedBy: "oidc:e2e-shard-3@example.com"},
			true,
		},
		{
			"browser-e2e-spec created_by",
			&BeadDetail{Title: "some-account", CreatedBy: "gardenio-browser-e2e-spec-7vx"},
			true,
		},
		{
			"bare KD_ACTOR e2e-rwx",
			&BeadDetail{Title: "some-account", CreatedBy: "e2e-rwx"},
			true,
		},

		// ── No-false-positive guarantees (acceptance criterion 4) ──
		{
			"REAL account whose name legitimately begins with 'e2e' but not 'e2e-'",
			&BeadDetail{Title: "e2eflow-prod", Labels: []string{"project:gardenio"}, CreatedBy: "Matthew.Baker"},
			false,
		},
		{
			"REAL account 'e2e' with no trailing hyphen",
			&BeadDetail{Title: "e2e", Labels: []string{"project:gardenio"}},
			false,
		},
		{
			"REAL account authored by an agent whose NAME merely contains 'e2e-test' (this very agent)",
			&BeadDetail{Title: "gardenio3", Labels: []string{"project:gardenio"}, CreatedBy: "gardenio-e2e-test-accounts-c5n"},
			false,
		},
		{
			"REAL production account — none of the signals",
			&BeadDetail{Title: "gardenio3", Labels: []string{"account", "project:gardenio", "project:monorepo"}, CreatedBy: "Matthew.Baker", OperatedBy: "slack:U0OPERATOR1"},
			false,
		},
		{
			"REAL account with a project label that CONTAINS e2e but is not the e2e:/e2e-tag: namespace",
			&BeadDetail{Title: "prod-acct", Labels: []string{"project:my-e2e-lab"}, CreatedBy: "thread-123"},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsE2EAccountBead(tc.bead); got != tc.want {
				t.Errorf("IsE2EAccountBead(%q) = %v; want %v", tc.name, got, tc.want)
			}
		})
	}
}
