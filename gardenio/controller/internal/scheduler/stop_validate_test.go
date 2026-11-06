package scheduler

import (
	"strings"
	"testing"
)

// TestValidateStopSpec_AcceptsEngineValidSpecs locks that a spec every entry of
// which the engine can evaluate passes validation. Each case is a real stop
// shape a gardener author would write.
func TestValidateStopSpec_AcceptsEngineValidSpecs(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"empty is unconstrained", ``},
		{"empty object", `{}`},
		{
			"full loop_end + hard_stop (home-beauty archetype)",
			`{"loop_end":{"any":[
				{"kind":"bead_query","value":"0","op":"==","parent":"gd-qipN5RP8Do"},
				{"kind":"max_runs"},
				{"kind":"no_progress_for","runs":3}
			]},"hard_stop":{"any":[
				{"kind":"consecutive_failures","value":"5"},
				{"kind":"wall_clock","duration":"720h"}
			]},"on_loop_end":"close"}`,
		},
		{
			"goal_satisfied + specific_bead_closed + gate",
			`{"loop_end":{"any":[
				{"kind":"goal_satisfied"},
				{"kind":"specific_bead_closed","bead_id":"gd-abc"},
				{"kind":"gate_present","value":"release-cut"}
			]}}`,
		},
		{
			"specific_mr_merged with an mr_url (--stop-on-mr-merged)",
			`{"loop_end":{"any":[
				{"kind":"specific_mr_merged","mr_url":"https://gitlab.com/acme/app/-/merge_requests/42"}
			]}}`,
		},
		{
			"run_end wall_clock (per-run timeout)",
			`{"run_end":{"any":[{"kind":"wall_clock","duration":"2h"}]},
			  "hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}`,
		},
		{
			"consecutive_failures value as JSON number (UnmarshalJSON canonicalizes)",
			`{"hard_stop":{"any":[{"kind":"consecutive_failures","value":5}]}}`,
		},
		{
			"bead_query with each recognized op",
			`{"loop_end":{"any":[
				{"kind":"bead_query","op":">=","value":"1"},
				{"kind":"bead_query","op":"!=","value":"0"}
			]}}`,
		},
		{
			"manual + manual_force need no fields (defaults apply)",
			`{"loop_end":{"any":[{"kind":"manual"},{"kind":"manual_force"}]}}`,
		},
		{
			"mr_merged park marker",
			`{"loop_end":{"any":[{"kind":"mr_merged","parent":"gd-epic"}]}}`,
		},
		{
			"on_loop_end notify_slack",
			`{"loop_end":{"any":[{"kind":"max_runs"}]},"on_loop_end":"notify_slack"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateStopSpec(tc.raw); err != nil {
				t.Errorf("ValidateStopSpec(%s) = %v, want nil", tc.raw, err)
			}
		})
	}
}

// TestValidateStopSpec_RejectsEngineDeadSpecs is the heart of I2.5: every case
// here is a spec that json.Valid ACCEPTS but the engine's stop evaluator would
// silently ignore (EvaluateGardenerStopEntry returns not-met). Validation must
// reject each with an actionable message so the UI never persists dead config.
func TestValidateStopSpec_RejectsEngineDeadSpecs(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantSubs []string // substrings the error message must contain
	}{
		{
			"typo'd kind (max_run) — the classic silent no-op",
			`{"loop_end":{"any":[{"kind":"max_run"}]}}`,
			[]string{"loop_end[0]", "unknown kind", "max_run"},
		},
		{
			"empty kind",
			`{"loop_end":{"any":[{"kind":""}]}}`,
			[]string{"loop_end[0]", "missing kind"},
		},
		{
			"wall_clock with unparseable duration",
			`{"hard_stop":{"any":[{"kind":"wall_clock","duration":"forever"}]}}`,
			[]string{"hard_stop[0]", "wall_clock", "does not parse"},
		},
		{
			"wall_clock with non-positive duration",
			`{"hard_stop":{"any":[{"kind":"wall_clock","duration":"0s"}]}}`,
			[]string{"hard_stop[0]", "must be positive"},
		},
		{
			"wall_clock missing duration",
			`{"hard_stop":{"any":[{"kind":"wall_clock"}]}}`,
			[]string{"hard_stop[0]", "wall_clock"},
		},
		{
			"consecutive_failures with non-numeric value",
			`{"hard_stop":{"any":[{"kind":"consecutive_failures","value":"lots"}]}}`,
			[]string{"hard_stop[0]", "consecutive_failures", "integer"},
		},
		{
			"consecutive_failures with non-positive value",
			`{"hard_stop":{"any":[{"kind":"consecutive_failures","value":"0"}]}}`,
			[]string{"hard_stop[0]", "positive integer"},
		},
		{
			"no_progress_for missing runs (runs<=0 is a no-op on gardeners)",
			`{"loop_end":{"any":[{"kind":"no_progress_for"}]}}`,
			[]string{"loop_end[0]", "no_progress_for", "runs>0"},
		},
		{
			"bead_query with unknown op",
			`{"loop_end":{"any":[{"kind":"bead_query","op":"=~","value":"0"}]}}`,
			[]string{"loop_end[0]", "bead_query op", "recognized comparison"},
		},
		{
			"bead_query with non-integer value",
			`{"loop_end":{"any":[{"kind":"bead_query","op":"==","value":"none"}]}}`,
			[]string{"loop_end[0]", "bead_query value", "integer threshold"},
		},
		{
			"specific_bead_closed missing bead_id",
			`{"loop_end":{"any":[{"kind":"specific_bead_closed"}]}}`,
			[]string{"loop_end[0]", "specific_bead_closed", "bead_id"},
		},
		{
			"specific_mr_merged missing mr_url",
			`{"loop_end":{"any":[{"kind":"specific_mr_merged"}]}}`,
			[]string{"loop_end[0]", "specific_mr_merged", "mr_url"},
		},
		{
			"gate_present missing value",
			`{"loop_end":{"any":[{"kind":"gate_present"}]}}`,
			[]string{"loop_end[0]", "gate_present", "gate title"},
		},
		{
			"unknown on_loop_end action",
			`{"loop_end":{"any":[{"kind":"max_runs"}]},"on_loop_end":"burn_it_down"}`,
			[]string{"on_loop_end", "burn_it_down", "recognized action"},
		},
		{
			"run_end slot is validated too",
			`{"run_end":{"any":[{"kind":"bogus"}]}}`,
			[]string{"run_end[0]", "unknown kind"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateStopSpec(tc.raw)
			if err == nil {
				t.Fatalf("ValidateStopSpec(%s) = nil, want an error", tc.raw)
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(err.Error(), sub) {
					t.Errorf("error %q does not contain %q", err.Error(), sub)
				}
			}
		})
	}
}

// TestValidateStopSpec_RejectsMalformedJSON confirms a structurally-broken blob
// surfaces the parse error (not a false pass).
func TestValidateStopSpec_RejectsMalformedJSON(t *testing.T) {
	if err := ValidateStopSpec(`{"loop_end":`); err == nil {
		t.Fatal("ValidateStopSpec of truncated JSON = nil, want a parse error")
	}
}

// TestValidateStopSpec_MirrorsEvaluatorKindSet is a guard against drift: every
// StopKind the evaluator has a real (non-default, non-fail-open-only) case for
// must be accepted by stopKindValid. If someone adds a StopKind constant and a
// matching evaluator case but forgets to teach the validator about it, the
// validator would reject a spec the engine can actually run. This test does not
// enumerate the evaluator by reflection (Go can't) but pins the known set so a
// new kind forces a conscious update here.
func TestValidateStopSpec_MirrorsEvaluatorKindSet(t *testing.T) {
	known := []string{
		StopKindMaxRuns, StopKindWallClock, StopKindBeadQuery,
		StopKindSpecificBeadClosed, StopKindGatePresent, StopKindGateAbsent,
		StopKindNoProgressFor, StopKindConsecutiveFailures, StopKindManual,
		StopKindManualForce, StopKindYieldedThenWokenThenIdle,
		StopKindNoActionableState, StopKindMRMerged, StopKindSpecificMRMerged,
		StopKindGoalSatisfied,
	}
	for _, k := range known {
		if !stopKindValid(k) {
			t.Errorf("stopKindValid(%q) = false, want true — evaluator has a case for it", k)
		}
	}
	if stopKindValid("definitely_not_a_kind") {
		t.Error("stopKindValid accepted a bogus kind")
	}
}
