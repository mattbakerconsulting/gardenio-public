package bridge

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestWarnUnbackedGardenerProse_WarnsOnce(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	e := NewTriggerEngine(TriggerEngineConfig{Logger: logger})

	trig := TriggerDef{
		ID:     "ka-0000003150",
		Title:  "soup-to-nuts (unbacked)",
		Prompt: "Run budget: at most 24 runs.\nTerminal goal: All children closed.",
		// Gardener == nil and HasConditions == false → unbacked.
	}

	e.warnUnbackedGardenerProse(trig)
	first := strings.Count(buf.String(), "unbacked gardener prose")
	if first != 1 {
		t.Fatalf("expected exactly 1 warning, got %d:\n%s", first, buf.String())
	}

	// Second evaluation of the same trigger must NOT re-warn.
	e.warnUnbackedGardenerProse(trig)
	if got := strings.Count(buf.String(), "unbacked gardener prose"); got != 1 {
		t.Fatalf("expected warn-once, got %d warnings", got)
	}

	// A properly-backed trigger never warns.
	buf.Reset()
	e.warnUnbackedGardenerProse(TriggerDef{
		ID:            "ka-0000004474",
		Prompt:        "Run budget: at most 24 runs.",
		HasConditions: true,
	})
	if buf.Len() != 0 {
		t.Fatalf("backed trigger should not warn, got:\n%s", buf.String())
	}
}

func TestGardenerGateProse(t *testing.T) {
	tests := []struct {
		name   string
		prompt string
		want   []string
	}{
		{"empty", "", nil},
		{"none", "Drive the epic to completion and close children.", nil},
		{"underscore run_budget", "Honor the run_budget of 24.", []string{"run_budget"}},
		{"prose run budget", "Run budget: at most 24 runs.", []string{"run budget"}},
		{"underscore terminal_goal", "terminal_goal: all closed", []string{"terminal_goal"}},
		{"prose terminal goal", "Terminal goal: All children of gd-X are closed.", []string{"terminal goal"}},
		{"max_runs", "max_runs is 24", []string{"max_runs"}},
		{"case insensitive", "RUN BUDGET and TERMINAL GOAL", []string{"run budget", "terminal goal"}},
		{
			"the bug's trigger prompt (ka-0000003150 shape)",
			"Run budget: at most 24 runs\nTerminal goal: All children of ka-0000001579 are closed",
			[]string{"run budget", "terminal goal"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GardenerGateProse(tt.prompt)
			if !equalStrSlice(got, tt.want) {
				t.Fatalf("GardenerGateProse(%q) = %v, want %v", tt.prompt, got, tt.want)
			}
		})
	}
}

func TestUnbackedGardenerProse(t *testing.T) {
	const budgetPrompt = "Run budget: at most 24 runs. Terminal goal: all children closed."

	tests := []struct {
		name         string
		prompt       string
		hasGardener  bool
		hasGate      bool
		wantMarkers  bool // expect at least one marker reported
		wantUnbacked bool
	}{
		{
			name:         "budget prose, no gardener, no gate — UNBACKED (the bug)",
			prompt:       budgetPrompt,
			hasGardener:  false,
			hasGate:      false,
			wantMarkers:  true,
			wantUnbacked: true,
		},
		{
			name:         "budget prose backed by gardener block — OK",
			prompt:       budgetPrompt,
			hasGardener:  true,
			hasGate:      false,
			wantMarkers:  true,
			wantUnbacked: false,
		},
		{
			name:         "budget prose backed by conditions pre-flight — OK (gb gardener start shape)",
			prompt:       budgetPrompt,
			hasGardener:  false,
			hasGate:      true,
			wantMarkers:  true,
			wantUnbacked: false,
		},
		{
			name:         "no budget prose, no backing — not a gardener, OK",
			prompt:       "Triage the ready queue for gardenio.",
			hasGardener:  false,
			hasGate:      false,
			wantMarkers:  false,
			wantUnbacked: false,
		},
		{
			name:         "empty prompt — OK",
			prompt:       "",
			hasGardener:  false,
			hasGate:      false,
			wantMarkers:  false,
			wantUnbacked: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			markers, unbacked := UnbackedGardenerProse(tt.prompt, tt.hasGardener, tt.hasGate)
			if unbacked != tt.wantUnbacked {
				t.Fatalf("unbacked = %v, want %v (markers=%v)", unbacked, tt.wantUnbacked, markers)
			}
			if (len(markers) > 0) != tt.wantMarkers {
				t.Fatalf("markers = %v, wantMarkers=%v", markers, tt.wantMarkers)
			}
		})
	}
}

func TestBeadUnbackedGardenerProse(t *testing.T) {
	// Mirrors trigger ka-0000003150: budget/goal prose, no gardener, no conditions.
	bug := &beadsapi.BeadDetail{
		ID: "ka-0000003150",
		Fields: map[string]string{
			"prompt": "Run budget: at most 24 runs.\nTerminal goal: All children closed.",
		},
	}
	markers, unbacked := BeadUnbackedGardenerProse(bug)
	if !unbacked {
		t.Fatalf("bug-shape bead should be flagged unbacked; markers=%v", markers)
	}

	// Sibling ka-0000004474: same prose but a real gardener block backs it.
	backed := &beadsapi.BeadDetail{
		ID: "ka-0000004474",
		Fields: map[string]string{
			"prompt":   "Run budget: at most 24 runs.",
			"gardener": `{"run_budget":24,"terminal_goal":"all closed"}`,
		},
	}
	if _, unbacked := BeadUnbackedGardenerProse(backed); unbacked {
		t.Fatal("gardener-backed bead should NOT be flagged unbacked")
	}

	// gb gardener start shape: prose folded into prompt, backed by a conditions gate.
	gated := &beadsapi.BeadDetail{
		ID: "gd-gated",
		Fields: map[string]string{
			"prompt":     "Run budget: at most 20 runs before you wrap up.",
			"conditions": `[{"type":"bead_query","op":">","value":"0","parent":"gd-epic"}]`,
		},
	}
	if _, unbacked := BeadUnbackedGardenerProse(gated); unbacked {
		t.Fatal("conditions-gated bead should NOT be flagged unbacked")
	}

	if _, unbacked := BeadUnbackedGardenerProse(nil); unbacked {
		t.Fatal("nil bead should not be flagged")
	}
}

func TestFieldsUnbackedGardenerProse(t *testing.T) {
	// CLI-side fields map: budget prose, no gardener/conditions → unbacked.
	fields := map[string]any{
		"prompt": "Terminal goal: drain the epic. Run budget 24.",
		"action": "spawn_agent",
	}
	if _, unbacked := FieldsUnbackedGardenerProse(fields); !unbacked {
		t.Fatal("CLI fields with budget prose and no backing should be flagged")
	}

	fields["conditions"] = `[{"type":"bead_query"}]`
	if _, unbacked := FieldsUnbackedGardenerProse(fields); unbacked {
		t.Fatal("CLI fields with a conditions gate should NOT be flagged")
	}
}

func TestGardenerGateWarning(t *testing.T) {
	w := GardenerGateWarning("ka-0000003150", []string{"run budget", "terminal goal"})
	for _, want := range []string{"ka-0000003150", "run budget/terminal goal", "gb gardener start", "ka-0000000974"} {
		if !strings.Contains(w, want) {
			t.Fatalf("warning missing %q:\n%s", want, w)
		}
	}
	// Empty trigger ID renders a placeholder rather than a blank.
	if !strings.Contains(GardenerGateWarning("", []string{"run budget"}), "<new>") {
		t.Fatal("empty trigger ID should render <new> placeholder")
	}
}

func equalStrSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
