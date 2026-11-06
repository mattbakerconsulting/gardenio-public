package bridge

import (
	"testing"
)

// TestClassifyGardenerBlob_TwoAxes is the load-bearing regression for bug
// ka-0000000849: `gb gardener status` and gardener-autoblock-audit.py answered
// "is this gardener bounded?" with two DIFFERENT properties and both called it
// "bounded". ClassifyGardenerBlob now computes BOTH axes off one decoded blob so
// the two tools speak one vocabulary. Each case pins the (has_backstop,
// self_block) pair for a canonical shape.
func TestClassifyGardenerBlob_TwoAxes(t *testing.T) {
	cases := []struct {
		name         string
		blob         string
		wantBackstop bool
		wantSelf     SelfBlockLevel
	}{
		{
			// THE bug's divergence: ka-0000003772 (gardener-tree-hug). run_budget +
			// no_progress_for + progress spec => self-block ok, but its only
			// hard_stop is consecutive_failures (no wall_clock) => NO backstop.
			// Same bead, opposite one-word verdicts — now both render.
			name: "divergent tree-hug: self-block ok, no backstop",
			blob: `{"run_budget":50,"max_runs":50,` +
				`"progress":{"events":[{"kind":"wrapup_field","field":"progress_score"}]},` +
				`"stop":{"loop_end":{"any":[{"kind":"max_runs"},{"kind":"no_progress_for","runs":3}]},` +
				`"hard_stop":{"any":[{"kind":"consecutive_failures","value":"5"}]}}}`,
			wantBackstop: false,
			wantSelf:     SelfBlockOK,
		},
		{
			// A fully-safe gardener: yes on BOTH axes.
			name: "fully safe: backstop + progress-aware self-block",
			blob: `{"run_budget":20,"max_runs":20,` +
				`"progress":{"events":[{"kind":"wrapup_field","field":"progress_score"}]},` +
				`"stop":{"loop_end":{"any":[{"kind":"no_progress_for","runs":3}]},` +
				`"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}}`,
			wantBackstop: true,
			wantSelf:     SelfBlockOK,
		},
		{
			// Backstop present but no wedge detector: the audit's "weak".
			name: "backstop but weak self-block (no progress-aware wedge)",
			blob: `{"run_budget":20,` +
				`"stop":{"loop_end":{"any":[{"kind":"bead_query","op":"==","value":"0","parent":"gd-E"}]},` +
				`"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}}`,
			wantBackstop: true,
			wantSelf:     SelfBlockWeak,
		},
		{
			// no_progress_for kind but NO progress spec => not progress-aware, so
			// weak (it still has run_budget + an intelligent loop_end).
			name: "no_progress_for without progress spec is weak",
			blob: `{"run_budget":20,` +
				`"stop":{"loop_end":{"any":[{"kind":"no_progress_for","runs":3}]},` +
				`"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}}`,
			wantBackstop: true,
			wantSelf:     SelfBlockWeak,
		},
		{
			// Neither axis: no run_budget, no intelligent loop_end. The audit's
			// "offender". Backstop also fails (no run_budget).
			name: "offender: no run_budget, no intelligent loop_end",
			blob: `{"stop":{"loop_end":{"any":[]},` +
				`"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}}`,
			wantBackstop: false,
			wantSelf:     SelfBlockNone,
		},
		{
			// wall_clock over the 720h ceiling is not a real backstop; the
			// self-block is still ok (run_budget + no_progress_for + progress).
			name: "wall_clock over ceiling: no backstop, self-block ok",
			blob: `{"run_budget":20,` +
				`"progress":{"events":[{"kind":"wrapup_field","field":"x"}]},` +
				`"stop":{"loop_end":{"any":[{"kind":"no_progress_for","runs":3}]},` +
				`"hard_stop":{"any":[{"kind":"wall_clock","duration":"100000h"}]}}}`,
			wantBackstop: false,
			wantSelf:     SelfBlockOK,
		},
		{
			name:         "empty blob: no backstop, self-block none",
			blob:         "",
			wantBackstop: false,
			wantSelf:     SelfBlockNone,
		},
		{
			name:         "unparseable blob: no backstop, self-block none",
			blob:         `{not json`,
			wantBackstop: false,
			wantSelf:     SelfBlockNone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyGardenerBlob(tc.blob)
			if got.HasBackstop != tc.wantBackstop {
				t.Errorf("HasBackstop = %v, want %v", got.HasBackstop, tc.wantBackstop)
			}
			if got.SelfBlock != tc.wantSelf {
				t.Errorf("SelfBlock = %q, want %q", got.SelfBlock, tc.wantSelf)
			}
		})
	}
}

// TestClassifyGardenerBlob_BackstopMatchesGuard is the parity invariant with the
// engine: the backstop axis of ClassifyGardenerBlob MUST equal
// ValidateGardenerBlob==nil for every input, because a surveyor reading
// ClassifyGardenerBlob's has_backstop must get the exact answer the engine
// enforces at fire time (that parity is the whole point of not forking the
// logic) (ka-0000000849).
func TestClassifyGardenerBlob_BackstopMatchesGuard(t *testing.T) {
	blobs := []string{
		`{"run_budget":20,"stop":{"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}}`,
		`{"stop":{"hard_stop":{"any":[{"kind":"wall_clock","duration":"168h"}]}}}`,
		`{"run_budget":20}`,
		`{"run_budget":20,"stop":{"hard_stop":{"any":[{"kind":"wall_clock","duration":"100000h"}]}}}`,
		`{"run_budget":20,"stop":{"hard_stop":{"any":[{"kind":"wall_clock","duration":"720h"}]}}}`,
		"",
		`{bad`,
	}
	for _, blob := range blobs {
		want := ValidateGardenerBlob(blob) == nil
		got := ClassifyGardenerBlob(blob).HasBackstop
		if got != want {
			t.Errorf("has_backstop parity broken for %q: classify=%v guard=%v", blob, got, want)
		}
	}
}

// TestIntelligentLoopEndKinds_Doc pins the exact set of intelligent loop_end
// kinds so the Go classifier and the Python audit
// (INTELLIGENT_LOOP_END_KINDS in gardener-autoblock-audit.py) can never silently
// drift on what "intelligent" means — the drift that produced bug ka-0000000849.
// The Python side pins the same four in gardener-autoblock-audit_test.sh.
func TestIntelligentLoopEndKinds_Doc(t *testing.T) {
	want := map[string]bool{
		"no_progress_for":      true,
		"bead_query":           true,
		"specific_bead_closed": true,
		"max_runs":             true,
	}
	if len(IntelligentLoopEndKinds) != len(want) {
		t.Fatalf("IntelligentLoopEndKinds size = %d, want %d: %v",
			len(IntelligentLoopEndKinds), len(want), IntelligentLoopEndKinds)
	}
	for k := range want {
		if !IntelligentLoopEndKinds[k] {
			t.Errorf("IntelligentLoopEndKinds missing %q", k)
		}
	}
}

// TestClassifyGardenerSafety_NilSpec confirms a nil spec (not a gardener) is a
// clean no-op on both axes rather than a panic.
func TestClassifyGardenerSafety_NilSpec(t *testing.T) {
	got := ClassifyGardenerSafety(nil)
	if got.HasBackstop || got.SelfBlock != SelfBlockNone {
		t.Errorf("nil spec must be no-backstop/self-block-none, got %+v", got)
	}
}
