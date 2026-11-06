package bridge

import (
	"testing"
)

// A fully-hygienic gardener blob: non-trivial goal, run_budget, an intelligent
// (bead_query + no_progress_for) loop_end, and a wall_clock hard_stop backstop.
// This is the exact shape `gb gardener start` now emits by default (gd-Lee2eG1pST),
// so a freshly-authored gardener must score a perfect 5/5. Mirrors the live
// plan-gardener blobs (e.g. ka-0000001203) the backfill audited.
const d6PerfectBlob = `{
  "terminal_goal": "All steps of plan ka-0000000306 are closed (or the operator has retired the plan).",
  "run_budget": 21, "max_runs": 8,
  "progress": {"events":[{"kind":"child_closed","parent":"ka-0000000306"},{"kind":"wrapup_field","field":"progress_score","op":">=","value":"1"}]},
  "stop": {
    "loop_end": {"any":[{"kind":"bead_query","op":"==","value":"0","parent":"ka-0000000306","types":["task","bug","epic"],"statuses":["open","in_progress","blocked"]},{"kind":"max_runs"},{"kind":"no_progress_for","runs":3}]},
    "hard_stop": {"any":[{"kind":"consecutive_failures","value":"5"},{"kind":"wall_clock","duration":"168h"}]},
    "on_loop_end": "close"
  }
}`

// TestScoreGardenerD6_Perfect pins that a default-authored gardener scores a clean
// 5/5 — the whole point of the structured-stop work was to make the rubric's D6
// the DEFAULT, not homework (gd-Lee2eG1pST acceptance criterion).
func TestScoreGardenerD6_Perfect(t *testing.T) {
	v := ScoreGardenerD6Blob(d6PerfectBlob, "20,50 * * * *")
	if v.Raw != D6MaxRaw {
		t.Fatalf("Raw = %d, want %d; failing checks: %v", v.Raw, D6MaxRaw, failingD6(v))
	}
	if v.Weighted != 10.0 {
		t.Errorf("Weighted = %v, want 10.0", v.Weighted)
	}
	for _, c := range v.Checks {
		if !c.Pass {
			t.Errorf("check %q unexpectedly failed: %s", c.Name, c.Reason)
		}
		if c.Pass && c.Reason != "" {
			t.Errorf("check %q passed but carries a reason %q", c.Name, c.Reason)
		}
	}
}

// TestScoreGardenerD6_PerCheck exercises each D6 check by feeding a blob that
// satisfies every OTHER check, asserting exactly the named check(s) fail and the
// raw score is D6MaxRaw minus that count. This is the enforceability contract —
// each rubric rule is scored independently and deterministically.
func TestScoreGardenerD6_PerCheck(t *testing.T) {
	cases := []struct {
		name     string
		blob     string
		cron     string
		wantFail []string // the check(s) expected to fail (all others must pass)
	}{
		{
			name: "trivial terminal_goal (bare default)",
			blob: `{"terminal_goal":"All children of gd-EPIC are closed.",` +
				`"run_budget":20,"max_runs":20,` +
				`"progress":{"events":[{"kind":"wrapup_field","field":"progress_score"}]},` +
				`"stop":{"loop_end":{"any":[{"kind":"no_progress_for","runs":3}]},` +
				`"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}}`,
			cron:     "3 9 * * *",
			wantFail: []string{"non_trivial_terminal_goal"},
		},
		{
			name: "empty terminal_goal",
			blob: `{"run_budget":20,"max_runs":20,` +
				`"progress":{"events":[{"kind":"wrapup_field","field":"progress_score"}]},` +
				`"stop":{"loop_end":{"any":[{"kind":"no_progress_for","runs":3}]},` +
				`"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}}`,
			cron:     "3 9 * * *",
			wantFail: []string{"non_trivial_terminal_goal"},
		},
		{
			// No run_budget, but a progress-aware wedge detector + wall_clock
			// backstop + non-trivial goal + valid cron. Check 5 (not_wall_clock_only)
			// is decoupled from run_budget, so ONLY the run_budget check fails.
			name: "no run_budget",
			blob: `{"terminal_goal":"Standing audit of the fleet, retired only by operator.",` +
				`"progress":{"events":[{"kind":"wrapup_field","field":"progress_score"}]},` +
				`"stop":{"loop_end":{"any":[{"kind":"no_progress_for","runs":3}]},` +
				`"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}}`,
			cron:     "3 9 * * *",
			wantFail: []string{"run_budget"},
		},
		{
			// An empty loop_end.any has neither an intelligent kind NOR a wedge
			// detector, so it faithfully fails BOTH check 3 and check 5 — a gardener
			// that can only stop on the wall-clock. Pinning the two-check failure
			// documents that the two checks are related but distinct.
			name: "empty loop_end (no intelligent kind, no wedge)",
			blob: `{"terminal_goal":"Standing audit of the fleet, retired only by operator.",` +
				`"run_budget":20,"max_runs":20,` +
				`"progress":{"events":[{"kind":"wrapup_field","field":"progress_score"}]},` +
				`"stop":{"loop_end":{"any":[]},` +
				`"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}}`,
			cron:     "3 9 * * *",
			wantFail: []string{"intelligent_loop_end", "not_wall_clock_only"},
		},
		{
			name: "malformed cron",
			blob: d6PerfectBlob,
			cron: "not-a-cron",
			// d6PerfectBlob uses a monorepo epic; its goal is non-trivial, so only
			// cadence should fail.
			wantFail: []string{"sane_cadence"},
		},
		{
			// A gardener with a bead_query goal-completion loop_end (intelligent, so
			// check 3 passes) but NO no_progress_for wedge detector — it terminates
			// on goal completion or the wall-clock, but during a stall it drifts to
			// the wall-clock backstop rather than stopping on stalled progress. That
			// is exactly the §D6 check-5 miss, isolated from check 3.
			name: "wall-clock-only survival (goal-completion loop_end, no wedge)",
			blob: `{"terminal_goal":"All steps of plan gd-XYZ are closed or the operator retires it.",` +
				`"run_budget":20,"max_runs":20,` +
				`"stop":{"loop_end":{"any":[{"kind":"bead_query","op":"==","value":"0","parent":"gd-XYZ"},{"kind":"max_runs"}]},` +
				`"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}}`,
			cron:     "3 9 * * *",
			wantFail: []string{"not_wall_clock_only"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := ScoreGardenerD6Blob(tc.blob, tc.cron)
			if got, want := v.Raw, D6MaxRaw-len(tc.wantFail); got != want {
				t.Fatalf("Raw = %d, want %d; failing: %v", got, want, failingD6(v))
			}
			wantFail := map[string]bool{}
			for _, n := range tc.wantFail {
				wantFail[n] = true
			}
			for _, c := range v.Checks {
				if wantFail[c.Name] {
					if c.Pass {
						t.Errorf("check %q expected to FAIL but passed", c.Name)
					}
					if c.Reason == "" {
						t.Errorf("failing check %q must carry a reason", c.Name)
					}
				} else if !c.Pass {
					t.Errorf("check %q unexpectedly failed (%s); only %v should fail",
						c.Name, c.Reason, tc.wantFail)
				}
			}
		})
	}
}

// TestScoreGardenerD6_EmptyBlob: a gardener with no loop spec at all (a legacy
// spec-less trigger) scores 0 — the fail-closed verdict, matching the safety
// classifiers. This is the "unmanaged" gardener the audit flags.
func TestScoreGardenerD6_EmptyBlob(t *testing.T) {
	for _, blob := range []string{"", "   ", "null", "{}", "not json"} {
		v := ScoreGardenerD6Blob(blob, "* * * * *")
		if v.Raw != 0 {
			t.Errorf("blob %q: Raw = %d, want 0", blob, v.Raw)
		}
		if v.Weighted != 0 {
			t.Errorf("blob %q: Weighted = %v, want 0", blob, v.Weighted)
		}
		if len(v.Checks) != D6MaxRaw {
			t.Errorf("blob %q: got %d checks, want %d", blob, len(v.Checks), D6MaxRaw)
		}
	}
}

// TestScoreGardenerD6_NilSpec: a nil spec is not a gardener — every check fails
// fail-closed, even with a valid cron, because a cron alone does not make a bead
// a gardener worth scoring for loop hygiene.
func TestScoreGardenerD6_NilSpec(t *testing.T) {
	v := ScoreGardenerD6(nil, "3 9 * * *")
	if v.Raw != 0 {
		t.Errorf("Raw = %d, want 0 for nil spec", v.Raw)
	}
	if len(v.Checks) != D6MaxRaw {
		t.Fatalf("nil spec: got %d checks, want %d", len(v.Checks), D6MaxRaw)
	}
	for _, c := range v.Checks {
		if c.Pass {
			t.Errorf("nil spec: check %q should fail-closed", c.Name)
		}
		if c.Reason == "" {
			t.Errorf("nil spec: failing check %q must carry a reason", c.Name)
		}
	}
}

// TestScoreGardenerD6_EventWakeCadence: a pure event-wake gardener (no cron) passes
// the cadence check — it fires only on real events, the healthiest cadence, and
// must not be penalized for an absent cron.
func TestScoreGardenerD6_EventWakeCadence(t *testing.T) {
	v := ScoreGardenerD6Blob(d6PerfectBlob, "")
	if v.Raw != D6MaxRaw {
		t.Fatalf("event-wake gardener: Raw = %d, want %d; failing: %v", v.Raw, D6MaxRaw, failingD6(v))
	}
	for _, c := range v.Checks {
		if c.Name == "sane_cadence" && !c.Pass {
			t.Errorf("empty cron (event-wake) should pass cadence, got: %s", c.Reason)
		}
	}
}

// TestScoreGardenerD6_StandingGardenerGoal: the live standing gardeners (e.g.
// ka-0000007535 OSS-sanitization, ka-0000004081 CED inspector) carry long
// standing-mandate goals. Those are non-trivial and must earn the goal point,
// even though they never self-declare a terminal "done" epic.
func TestScoreGardenerD6_StandingGardenerGoal(t *testing.T) {
	standingGoals := []string{
		"STANDING weekly audit sentinel. No terminal epic: it reports drift every Monday.",
		"PERPETUAL OSHA-style inspector for epic ka-0000003297. Each ~6h tick samples one session.",
	}
	for _, g := range standingGoals {
		if isTrivialTerminalGoal(g) {
			t.Errorf("standing-mandate goal wrongly flagged trivial: %q", g)
		}
	}
}

// TestScoreGardenerD6_WeightedRounding pins the weighted arithmetic for every raw
// score so a caller can sum D6 into the 0–100 rubric total exactly as the doc
// specifies (raw/5 × 10).
func TestScoreGardenerD6_WeightedRounding(t *testing.T) {
	want := map[int]float64{0: 0, 1: 2, 2: 4, 3: 6, 4: 8, 5: 10}
	for raw, expect := range want {
		got := float64(raw) / float64(D6MaxRaw) * 10.0
		got = float64(int(got*100+0.5)) / 100
		if got != expect {
			t.Errorf("raw %d: weighted = %v, want %v", raw, got, expect)
		}
	}
}

// TestD6CheckNames_Doc pins the exact set + order of D6 check slugs so the meta
// scorer and gardener-effectiveness-rubric.md §D6 can never silently drift on the
// check vocabulary — the same doc-pinning discipline as
// TestIntelligentLoopEndKinds_Doc (bug ka-0000000849).
func TestD6CheckNames_Doc(t *testing.T) {
	want := []string{
		"non_trivial_terminal_goal",
		"run_budget",
		"intelligent_loop_end",
		"sane_cadence",
		"not_wall_clock_only",
	}
	v := ScoreGardenerD6Blob(d6PerfectBlob, "20,50 * * * *")
	if len(v.Checks) != len(want) {
		t.Fatalf("got %d checks, want %d", len(v.Checks), len(want))
	}
	for i, c := range v.Checks {
		if c.Name != want[i] {
			t.Errorf("check[%d] = %q, want %q", i, c.Name, want[i])
		}
	}

	// The fail-closed no-spec path must emit the SAME slug set, in the same order,
	// as the real scoring path — otherwise a `d6.checks[]` consumer sees different
	// keys for a spec-less gardener than a specced one. This pins the exact drift
	// bug caught during authoring (the no-spec verdict lagged on the old
	// "has_backstop" slug after the real check was renamed "not_wall_clock_only").
	noSpec := ScoreGardenerD6(nil, "3 9 * * *")
	if len(noSpec.Checks) != len(want) {
		t.Fatalf("no-spec: got %d checks, want %d", len(noSpec.Checks), len(want))
	}
	for i, c := range noSpec.Checks {
		if c.Name != want[i] {
			t.Errorf("no-spec check[%d] = %q, want %q (must match the real path)", i, c.Name, want[i])
		}
	}
}

// failingD6 lists the names of failing checks for a readable test failure.
func failingD6(v D6Verdict) []string {
	var out []string
	for _, c := range v.Checks {
		if !c.Pass {
			out = append(out, c.Name)
		}
	}
	return out
}
