package server

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateFormulaFields_EmptyOK(t *testing.T) {
	if err := validateFormulaFields(nil); err != nil {
		t.Fatalf("nil fields should be accepted: %v", err)
	}
	if err := validateFormulaFields(json.RawMessage(`{}`)); err != nil {
		t.Fatalf("empty object should be accepted: %v", err)
	}
}

func TestValidateFormulaFields_LoopCountTooLarge(t *testing.T) {
	raw := json.RawMessage(`{"steps":[{"id":"s1","loop":{"var":"i","count":500}}]}`)
	err := validateFormulaFields(raw)
	if err == nil {
		t.Fatal("expected rejection for loop count 500")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Fatalf("error should mention count: %v", err)
	}
}

func TestValidateFormulaFields_LoopCountAcceptableAtCap(t *testing.T) {
	raw := json.RawMessage(`{"steps":[{"id":"s1","loop":{"var":"i","count":100}}]}`)
	if err := validateFormulaFields(raw); err != nil {
		t.Fatalf("count at cap should be accepted: %v", err)
	}
}

func TestValidateFormulaFields_RangeTooLarge(t *testing.T) {
	raw := json.RawMessage(`{"steps":[{"id":"s","loop":{"var":"i","range":"1..200"}}]}`)
	err := validateFormulaFields(raw)
	if err == nil {
		t.Fatal("expected rejection for range 1..200")
	}
}

func TestValidateFormulaFields_ValuesListTooLong(t *testing.T) {
	vals := make([]string, formulaMaxLoopCount+5)
	for i := range vals {
		vals[i] = "v"
	}
	payload, _ := json.Marshal(map[string]any{
		"steps": []map[string]any{
			{"id": "s", "loop": map[string]any{"var": "i", "values": vals}},
		},
	})
	if err := validateFormulaFields(payload); err == nil {
		t.Fatal("expected rejection for oversized values list")
	}
}

func TestValidateFormulaFields_DeepExpandNestingRejected(t *testing.T) {
	// Build children chain: each level has expand + child with expand …
	// 6-deep nested expansions exceeds cap (5).
	build := func(depth int) map[string]any {
		cur := map[string]any{"id": "leaf", "expand": "gd-leaf"}
		for i := depth - 1; i >= 0; i-- {
			cur = map[string]any{
				"id":       "s" + string(rune('0'+i)),
				"expand":   "gd-inner",
				"children": []map[string]any{cur},
			}
		}
		return cur
	}
	root := build(6)
	payload, _ := json.Marshal(map[string]any{
		"steps": []map[string]any{root},
	})
	if err := validateFormulaFields(payload); err == nil {
		t.Fatal("expected rejection for 6-deep expand nesting")
	}
}

func TestValidateFormulaFields_TotalStepsTooMany(t *testing.T) {
	steps := make([]map[string]any, formulaMaxTotalSteps+10)
	for i := range steps {
		steps[i] = map[string]any{"id": "s"}
	}
	payload, _ := json.Marshal(map[string]any{"steps": steps})
	if err := validateFormulaFields(payload); err == nil {
		t.Fatal("expected rejection for > max total steps")
	}
}

func TestValidateFormulaFields_ValidFormulaAccepted(t *testing.T) {
	raw := json.RawMessage(`{"steps":[
		{"id":"a","loop":{"var":"i","count":3}},
		{"id":"b","expand":"gd-x"},
		{"id":"c","children":[{"id":"c1"},{"id":"c2"}]}
	]}`)
	if err := validateFormulaFields(raw); err != nil {
		t.Fatalf("valid formula rejected: %v", err)
	}
}

func TestValidateScheduleFields_EmptyOK(t *testing.T) {
	if err := validateScheduleFields(nil); err != nil {
		t.Fatalf("nil fields should be accepted: %v", err)
	}
}

func TestValidateScheduleFields_MaxConcurrentCapped(t *testing.T) {
	raw := json.RawMessage(`{"cron":"*/5 * * * *","max_concurrent":50}`)
	err := validateScheduleFields(raw)
	if err == nil {
		t.Fatal("expected rejection for max_concurrent=50")
	}
	if !strings.Contains(err.Error(), "max_concurrent") {
		t.Fatalf("error should mention max_concurrent: %v", err)
	}
}

func TestValidateScheduleFields_MaxConcurrentAtCap(t *testing.T) {
	raw := json.RawMessage(`{"cron":"*/5 * * * *","max_concurrent":10}`)
	if err := validateScheduleFields(raw); err != nil {
		t.Fatalf("max_concurrent=10 at cap should be accepted: %v", err)
	}
}

func TestValidateScheduleFields_SecondsCronRejected(t *testing.T) {
	// 6-field cron is seconds-level — rejected.
	raw := json.RawMessage(`{"cron":"*/5 * * * * *"}`)
	err := validateScheduleFields(raw)
	if err == nil {
		t.Fatal("expected rejection for 6-field cron")
	}
}

func TestValidateScheduleFields_StandardCronsAccepted(t *testing.T) {
	for _, cron := range []string{
		"* * * * *",
		"*/5 * * * *",
		"0 9 * * 1-5",
		"@hourly",
		"@daily",
		"@every 5m",
	} {
		raw, _ := json.Marshal(map[string]any{"cron": cron})
		if err := validateScheduleFields(raw); err != nil {
			t.Fatalf("cron %q rejected: %v", cron, err)
		}
	}
}

func TestValidateScheduleFields_SubMinuteEveryRejected(t *testing.T) {
	raw := json.RawMessage(`{"cron":"@every 30s"}`)
	err := validateScheduleFields(raw)
	if err == nil {
		t.Fatal("expected rejection for @every 30s")
	}
}

// ── validateTriggerFields (ka-0000000254) ─────────────────────────

func TestValidateTriggerFields_EmptyOK(t *testing.T) {
	if err := validateTriggerFields(nil); err != nil {
		t.Fatalf("nil fields should be accepted: %v", err)
	}
	if err := validateTriggerFields(json.RawMessage(`{}`)); err != nil {
		t.Fatalf("empty object should be accepted: %v", err)
	}
}

func TestValidateTriggerFields_SpawnAgentNoProjectRejected(t *testing.T) {
	raw := json.RawMessage(`{"action":"spawn_agent","role":"crew","prompt":"go"}`)
	err := validateTriggerFields(raw)
	if err == nil {
		t.Fatal("expected rejection for spawn_agent trigger without project")
	}
	if !strings.Contains(err.Error(), "project") {
		t.Fatalf("error should mention project: %v", err)
	}
}

func TestValidateTriggerFields_ApplyFormulaNoProjectRejected(t *testing.T) {
	raw := json.RawMessage(`{"action":"apply_formula","formula_id":"f1"}`)
	if err := validateTriggerFields(raw); err == nil {
		t.Fatal("expected rejection for apply_formula trigger without project")
	}
}

func TestValidateTriggerFields_SpawnAgentWithProjectAccepted(t *testing.T) {
	raw := json.RawMessage(`{"action":"spawn_agent","project":"gardenio","role":"crew"}`)
	if err := validateTriggerFields(raw); err != nil {
		t.Fatalf("spawn_agent with project should be accepted: %v", err)
	}
}

func TestValidateTriggerFields_BlankProjectRejected(t *testing.T) {
	// A whitespace-only project field is treated as absent.
	raw := json.RawMessage(`{"action":"spawn_agent","project":"   "}`)
	if err := validateTriggerFields(raw); err == nil {
		t.Fatal("expected rejection for spawn_agent trigger with blank project")
	}
}

func TestValidateTriggerFields_GardenerNoProjectRejected(t *testing.T) {
	// A gardener-only trigger still fires spawn_agent through the engine path.
	raw := json.RawMessage(`{"gardener":{"terminal_goal":"drain the lane"}}`)
	if err := validateTriggerFields(raw); err == nil {
		t.Fatal("expected rejection for gardener trigger without project")
	}
}

func TestValidateTriggerFields_MultiTriggerNoProjectRejected(t *testing.T) {
	raw := json.RawMessage(`{"action":"spawn_agent","triggers":[{"kind":"cron","cron":"0 * * * *"}]}`)
	if err := validateTriggerFields(raw); err == nil {
		t.Fatal("expected rejection for multi-entry spawn trigger without project")
	}
}

func TestValidateTriggerFields_NonSpawnActionsUnconstrained(t *testing.T) {
	// notify / create_bead / assert / wake_pr_owner never spawn a pod, so a missing project is fine.
	for _, action := range []string{"notify", "create_bead", "assert", "wake_pr_owner"} {
		raw := json.RawMessage(`{"action":"` + action + `"}`)
		if err := validateTriggerFields(raw); err != nil {
			t.Fatalf("action=%s without project should be accepted: %v", action, err)
		}
	}
}

func TestValidateTriggerFields_EmptyGardenerOrTriggersNotSpawn(t *testing.T) {
	// An empty/placeholder gardener or triggers value does not count as
	// spawn-capable (hasTriggerContent semantics), so no project is required.
	for _, raw := range []string{
		`{"gardener":null}`,
		`{"gardener":{}}`,
		`{"triggers":[]}`,
		`{"triggers":""}`,
	} {
		if err := validateTriggerFields(json.RawMessage(raw)); err != nil {
			t.Fatalf("non-spawn fields %s should be accepted: %v", raw, err)
		}
	}
}

func TestValidateTriggerFields_MalformedFieldsNotDoubleReported(t *testing.T) {
	// Malformed JSON is caught by ValidateTypeFields upstream; this guard
	// returns nil rather than double-reporting.
	if err := validateTriggerFields(json.RawMessage(`{not json`)); err != nil {
		t.Fatalf("malformed fields should not be reported here: %v", err)
	}
}
