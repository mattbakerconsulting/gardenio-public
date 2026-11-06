package plantranscript

import (
	"encoding/json"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func decodeFields(t *testing.T, raw json.RawMessage) map[string]string {
	t.Helper()
	out := map[string]string{}
	if len(raw) == 0 {
		return out
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode fields: %v", err)
	}
	return out
}

// TestTurnBead proves the base turn-bead write contract: type=message/data, the
// plan_id binding, the turn_role + turn_id fields, the literal text, the
// from:<author> label + source_agent field, and the epic mention in the
// Description (so kbeads emits a durable mention edge).
func TestTurnBead(t *testing.T) {
	req := TurnBead("gd-EPIC", "gardenio-asst-1", TurnRoleAssistant, "the reply", "sess-9:1:abc")
	if req.Type != "message" || req.Kind != "data" {
		t.Errorf("Type/Kind = %q/%q, want message/data", req.Type, req.Kind)
	}
	if req.CreatedBy != "gardenio-asst-1" {
		t.Errorf("CreatedBy = %q, want the author", req.CreatedBy)
	}
	if req.Description != "Plan transcript turn for gd-EPIC" {
		t.Errorf("Description = %q, want the epic mention", req.Description)
	}
	wantLabels := map[string]bool{"plan-transcript": true, "from:gardenio-asst-1": true}
	for _, l := range req.Labels {
		delete(wantLabels, l)
	}
	if len(wantLabels) != 0 {
		t.Errorf("missing labels %v; got %v", wantLabels, req.Labels)
	}
	f := decodeFields(t, req.Fields)
	if f[PlanIDField] != "gd-EPIC" {
		t.Errorf("plan_id = %q, want gd-EPIC", f[PlanIDField])
	}
	if f[TurnRoleField] != TurnRoleAssistant {
		t.Errorf("turn_role = %q, want assistant", f[TurnRoleField])
	}
	if f[TurnIDField] != "sess-9:1:abc" {
		t.Errorf("turn_id = %q, want sess-9:1:abc", f[TurnIDField])
	}
	if f["text"] != "the reply" || f["source_agent"] != "gardenio-asst-1" {
		t.Errorf("text/source_agent = %q/%q", f["text"], f["source_agent"])
	}
}

// TestTurnBead_TitleTruncation proves a >500-char turn truncates its Title (the
// full text is still preserved in fields.text).
func TestTurnBead_TitleTruncation(t *testing.T) {
	long := make([]byte, 600)
	for i := range long {
		long[i] = 'x'
	}
	req := TurnBead("gd-EPIC", "a", TurnRoleOperator, string(long), "")
	if len(req.Title) != 500 {
		t.Errorf("Title len = %d, want 500 (497 + ...)", len(req.Title))
	}
	if f := decodeFields(t, req.Fields); len(f["text"]) != 600 {
		t.Errorf("fields.text len = %d, want the full 600 bytes", len(f["text"]))
	}
}

// TestTurnBead_EmptyTurnIDOmitsField proves an empty turnID omits the turn_id
// field entirely (legacy/test shape).
func TestTurnBead_EmptyTurnIDOmitsField(t *testing.T) {
	req := TurnBead("gd-EPIC", "a", TurnRoleAssistant, "hi", "")
	if _, ok := decodeFields(t, req.Fields)[TurnIDField]; ok {
		t.Error("empty turnID must omit the turn_id field")
	}
}

// TestToolTurnBead proves the tool-turn shape: turn_role=tool, the structured
// tool fields, and the "<tool>: <args>" fallback text. Empty args/result fields
// are omitted so a legacy/plain tool turn round-trips.
func TestToolTurnBead(t *testing.T) {
	req := ToolTurnBead("gd-EPIC", "a", "Bash", "run the tests", "exit 0", "turn-1")
	f := decodeFields(t, req.Fields)
	if f[TurnRoleField] != TurnRoleTool {
		t.Errorf("turn_role = %q, want tool", f[TurnRoleField])
	}
	if f[ToolNameField] != "Bash" || f[ToolArgsSummaryField] != "run the tests" || f[ToolResultSummaryField] != "exit 0" {
		t.Errorf("tool fields = %q/%q/%q", f[ToolNameField], f[ToolArgsSummaryField], f[ToolResultSummaryField])
	}
	if f["text"] != "Bash: run the tests" {
		t.Errorf("fallback text = %q, want 'Bash: run the tests'", f["text"])
	}

	// No args/result → bare tool name, omitted optional fields.
	bare := ToolTurnBead("gd-EPIC", "a", "Read", "", "", "turn-2")
	bf := decodeFields(t, bare.Fields)
	if bf["text"] != "Read" {
		t.Errorf("bare tool text = %q, want 'Read'", bf["text"])
	}
	if _, ok := bf[ToolArgsSummaryField]; ok {
		t.Error("empty args summary must be omitted")
	}
	if _, ok := bf[ToolResultSummaryField]; ok {
		t.Error("empty result summary must be omitted")
	}
}

// TestIdempotencyKey proves the shared dedup-key namespace.
func TestIdempotencyKey(t *testing.T) {
	if got := IdempotencyKey("gd-EPIC", "sess-9:1:abc"); got != "plan-turn:gd-EPIC:sess-9:1:abc" {
		t.Errorf("IdempotencyKey = %q", got)
	}
}

// TestIsPlanAssistantBead proves the identity predicate: role:plan-gardener label
// AND a plan_id/task_id binding to the epic. A same-name agent without the role
// label, or bound to a different epic, is rejected.
func TestIsPlanAssistantBead(t *testing.T) {
	mk := func(labels []string, fields map[string]string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{ID: "gd-a", Type: "agent", Labels: labels, Fields: fields}
	}
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		epic string
		want bool
	}{
		{"nil", nil, "gd-E", false},
		{"role + plan_id match", mk([]string{PlanGardenerRoleLabel}, map[string]string{PlanIDField: "gd-E"}), "gd-E", true},
		{"role + task_id match", mk([]string{PlanGardenerRoleLabel}, map[string]string{TaskIDField: "gd-E"}), "gd-E", true},
		{"role but wrong epic", mk([]string{PlanGardenerRoleLabel}, map[string]string{PlanIDField: "gd-OTHER"}), "gd-E", false},
		{"binding but no role label", mk([]string{"role:crew"}, map[string]string{PlanIDField: "gd-E"}), "gd-E", false},
	}
	for _, c := range cases {
		if got := IsPlanAssistantBead(c.bead, c.epic); got != c.want {
			t.Errorf("%s: IsPlanAssistantBead = %v, want %v", c.name, got, c.want)
		}
	}
}
