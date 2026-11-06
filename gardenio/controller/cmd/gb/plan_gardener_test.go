package main

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenermode"
	"gardenio/controller/internal/plangardener"
)

// resetPlanGardenerFlags restores the plan-gardener flag globals between tests
// (cobra flag vars are package globals, so a prior test's values leak).
func resetPlanGardenerFlags(t *testing.T) {
	t.Helper()
	flags := []struct{ name, val string }{
		{"cron", ""},
		{"timed", "false"},
		{"fallback-cron", ""},
		{"tz", ""},
		{"project", ""},
		{"max-runs", "8"},
		{"loop-wall-clock", "168h"},
		{"title", ""},
		{"slack-channel", ""},
		{"prompt", ""},
	}
	for _, f := range flags {
		_ = planGardenerCmd.Flags().Set(f.name, f.val)
	}
}

// makePlanEpic is makeEpic wearing the /plans marker label.
func makePlanEpic(id, title string, extraLabels ...string) map[string]any {
	labels := append([]string{"plan"}, extraLabels...)
	return makeEpic(id, title, labels...)
}

func runPlanGardenerCmd(t *testing.T, args ...string) error {
	t.Helper()
	cmd := planGardenerCmd
	cmd.SetContext(context.Background())
	return cmd.RunE(cmd, args)
}

// TestPlanGardener_BindsPlanGardenerSpawnRole locks the plan-agent contract (epic
// ka-0000002022 Step 1; unified by the ka-0000000081 merge): the trigger bead is
// the canonical firing gardener shape (type=trigger / trigger:multi /
// action=spawn_agent / role:gardener + gardener-epic labels), but its SPAWN role
// field is the unified plan-gardener role — so every iteration the engine fires
// resolves as the plan's bound agent in beads-viewer (whose resolver keys on
// role:plan-gardener).
func TestPlanGardener_BindsPlanGardenerSpawnRole(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makePlanEpic("gd-PLAN1", "Ship the widget", "project:gardenio"))
	setupGardenerStub(t, stub)
	resetPlanGardenerFlags(t)
	t.Cleanup(func() { resetPlanGardenerFlags(t) })

	oldOperated := operatedBy
	operatedBy = ""
	t.Cleanup(func() { operatedBy = oldOperated })

	if err := runPlanGardenerCmd(t, "gd-PLAN1"); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if len(stub.creates) != 1 {
		t.Fatalf("expected 1 CreateBead call, got %d", len(stub.creates))
	}
	c := stub.creates[0]
	if got := c["type"]; got != "trigger" {
		t.Errorf("created bead type = %v, want trigger", got)
	}
	fields, _ := c["fields"].(map[string]any)
	// The plan-agent seam: spawn role is plan-gardener, NOT gardener.
	if got := fields["role"]; got != "plan-gardener" {
		t.Errorf("fields.role = %v, want plan-gardener (iterations must resolve as the plan's bound agent)", got)
	}
	if got := fields["action"]; got != "spawn_agent" {
		t.Errorf("fields.action = %v, want spawn_agent", got)
	}
	if got, ok := fields["enabled"].(bool); !ok || !got {
		t.Errorf("fields.enabled = %#v, want JSON bool true", fields["enabled"])
	}
	// Event mode by default (gd-gsvlS6YDj3): no timed cron field, and a
	// triggers[] whose watchdog is the staggered daily cron seeded from the plan
	// epic id. Serialized iterations.
	if _, ok := fields["cron"]; ok {
		t.Errorf("fields.cron = %v, want none (a default bind is event mode)", fields["cron"])
	}
	if want := gardenermode.StaggeredCron("gd-PLAN1", 24*time.Hour); !strings.Contains(fmt.Sprint(fields["triggers"]), `"cron":"`+want+`"`) {
		t.Errorf("fields.triggers = %v, want a fallback watchdog on the staggered daily cron %q", fields["triggers"], want)
	}
	if got, ok := fields["max_concurrent"].(float64); !ok || got != 1 {
		t.Errorf("fields.max_concurrent = %#v, want JSON number 1", fields["max_concurrent"])
	}

	// The trigger bead itself stays a gardener via labels (ka-0000005048):
	// role:gardener + gardener-epic pin + the plan-gardener marker.
	labels, _ := c["labels"].([]any)
	labelSet := map[string]bool{}
	for _, l := range labels {
		if s, ok := l.(string); ok {
			labelSet[s] = true
		}
	}
	for _, want := range []string{"trigger:multi", "role:gardener", "gardener-epic:gd-PLAN1", "plan-gardener", "project:gardenio"} {
		if !labelSet[want] {
			t.Errorf("labels missing %q (have %v)", want, labelSet)
		}
	}

	// The blob carries the full self-block (advice ka-0000000974) plus the
	// C4-carried one_spawn_at_a_time and the ~8-run session budget.
	blob := gardenerBlobFromCreate(t, c)
	if blob == nil {
		t.Fatal("gardener blob absent — the structured loop spec must always be stamped")
	}
	assertGardenerSelfBlock(t, blob, "gd-PLAN1")
	if got, ok := blob["run_budget"].(float64); !ok || got != 8 {
		t.Errorf("gardener.run_budget = %#v, want 8 (steering-session budget)", blob["run_budget"])
	}
	if got, _ := blob["one_spawn_at_a_time"].(bool); !got {
		t.Errorf("gardener.one_spawn_at_a_time = %#v, want true (a plan never has two agents racing)", blob["one_spawn_at_a_time"])
	}

	// The playbook prompt names the plan and teaches a steering path the gardener
	// can actually execute: `gb plan add-step` requires an oidc:<email> operator
	// and rejects every agent identity, so the prompt teaches kd create + kd dep
	// add instead (gd-01nlBEQlLV / bug gd-0BLDvtYoQw).
	prompt, _ := fields["prompt"].(string)
	if !strings.Contains(prompt, "gd-PLAN1") {
		t.Error("prompt does not name the plan epic")
	}
	if !strings.Contains(prompt, "kd dep add <step-id> gd-PLAN1 --type parent-child") {
		t.Error("prompt does not teach the agent-callable steering path (kd create + kd dep add)")
	}
}

// TestPlanGardener_SlackChannelRoutesIterations verifies --slack-channel lands
// in the gardener blob (the engine copies spec.SlackChannel onto each spawned
// iteration as schedule_slack_channel, which routes its agent card).
func TestPlanGardener_SlackChannelRoutesIterations(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makePlanEpic("gd-PLAN2", "Routed plan", "project:gardenio"))
	setupGardenerStub(t, stub)
	resetPlanGardenerFlags(t)
	t.Cleanup(func() { resetPlanGardenerFlags(t) })

	if err := planGardenerCmd.Flags().Set("slack-channel", "#planner"); err != nil {
		t.Fatal(err)
	}
	if err := runPlanGardenerCmd(t, "gd-PLAN2"); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	blob := gardenerBlobFromCreate(t, stub.creates[0])
	if got, _ := blob["slack_channel"].(string); got != "#planner" {
		t.Errorf("gardener.slack_channel = %q, want #planner", got)
	}
}

// TestPlanGardener_AdoptsExistingGardener locks the one-gardener-per-plan
// invariant: an enabled gardener already pinned to the plan is adopted
// (reported, exit 0) rather than duplicated. This is also the backfill path —
// re-running the bind across existing plans must be a no-op.
func TestPlanGardener_AdoptsExistingGardener(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makePlanEpic("gd-PLAN3", "Already gardenered", "project:gardenio"))
	stub.addBead(map[string]any{
		"id":     "gd-CAP1",
		"title":  "Plan gardener for gd-PLAN3",
		"type":   "trigger",
		"kind":   "data",
		"status": "open",
		"labels": []string{"trigger:schedule", "role:gardener", "gardener-epic:gd-PLAN3"},
		"fields": map[string]any{
			"enabled":  "true",
			"role":     "plan-assistant",
			"gardener": `{"assign_bead":"gd-PLAN3"}`,
		},
	})
	setupGardenerStub(t, stub)
	resetPlanGardenerFlags(t)
	t.Cleanup(func() { resetPlanGardenerFlags(t) })

	if err := runPlanGardenerCmd(t, "gd-PLAN3"); err != nil {
		t.Fatalf("adopt should succeed (exit 0), got: %v", err)
	}
	if len(stub.creates) != 0 {
		t.Fatalf("adopt must not create a duplicate gardener; got %d creates", len(stub.creates))
	}
}

// TestPlanGardener_RequiresPlanLabel verifies the verb is plan-scoped: a plain
// epic without the /plans marker label is refused with a pointer to `gb
// gardener start`, keeping "the plan's one gardener" a meaningful invariant.
func TestPlanGardener_RequiresPlanLabel(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC9", "Not a plan", "project:gardenio"))
	setupGardenerStub(t, stub)
	resetPlanGardenerFlags(t)
	t.Cleanup(func() { resetPlanGardenerFlags(t) })

	err := runPlanGardenerCmd(t, "gd-EPIC9")
	if err == nil {
		t.Fatal("expected an error for an epic without the plan label")
	}
	if !strings.Contains(err.Error(), "'plan' label") {
		t.Errorf("error should explain the missing plan label, got: %v", err)
	}
	if len(stub.creates) != 0 {
		t.Fatalf("no gardener must be created for a non-plan epic; got %d creates", len(stub.creates))
	}
}

// TestPlanGardener_BornPausedWhenDraft verifies the approve→build gate on the
// CLI path (task ka-0000002863 / ka-0000005350): a plan that entered the
// authoring lifecycle (plan_state ∈ {draft, ready}) binds a gardener born
// PAUSED (enabled=false) so the scheduler cannot fire an unapproved plan.
func TestPlanGardener_BornPausedWhenDraft(t *testing.T) {
	for _, st := range []string{"draft", "ready"} {
		t.Run(st, func(t *testing.T) {
			stub := newGardenerStub()
			epic := makePlanEpic("gd-PLANG", "Gated plan", "project:gardenio")
			epic["fields"] = map[string]any{"plan_state": st}
			stub.addBead(epic)
			setupGardenerStub(t, stub)
			resetPlanGardenerFlags(t)
			t.Cleanup(func() { resetPlanGardenerFlags(t) })

			if err := runPlanGardenerCmd(t, "gd-PLANG"); err != nil {
				t.Fatalf("RunE: %v", err)
			}
			fields, _ := stub.creates[0]["fields"].(map[string]any)
			if got, ok := fields["enabled"].(bool); !ok || got {
				t.Errorf("plan_state=%s: fields.enabled = %#v, want JSON bool false (born paused)", st, fields["enabled"])
			}
		})
	}
}

// TestPlanGardener_BornEnabledWhenUnset verifies a hand-marked / legacy plan
// with no plan_state binds a gardener born ENABLED (ungated) — the historical
// behaviour the backfill and the browser hand-mark path both rely on.
func TestPlanGardener_BornEnabledWhenUnset(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makePlanEpic("gd-PLANU", "Legacy plan", "project:gardenio"))
	setupGardenerStub(t, stub)
	resetPlanGardenerFlags(t)
	t.Cleanup(func() { resetPlanGardenerFlags(t) })

	if err := runPlanGardenerCmd(t, "gd-PLANU"); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	fields, _ := stub.creates[0]["fields"].(map[string]any)
	if got, ok := fields["enabled"].(bool); !ok || !got {
		t.Errorf("unset plan_state: fields.enabled = %#v, want JSON bool true (ungated)", fields["enabled"])
	}
}

// TestPlanGardener_SharedBuilderParity is the load-bearing refactor assertion
// (task ka-0000002863): `gb plan gardener` and a direct plangardener.Bind call
// against the same daemon + operator emit BYTE-IDENTICAL gardener trigger beads.
// This is what guarantees the CLI and the beads-viewer auto-bind never drift —
// there is exactly ONE gardener-spec builder. The prompt (rendered from the
// shared template) is asserted equal too; the only fields excluded from the
// comparison are those the CLI resolves before delegating (project inheritance
// is identical here) — none, so the whole field map must match.
func TestPlanGardener_SharedBuilderParity(t *testing.T) {
	oldOperated := operatedBy
	operatedBy = "slack:UOP"
	t.Cleanup(func() { operatedBy = oldOperated })

	// Path A — the CLI verb through the HTTP stub.
	stubA := newGardenerStub()
	stubA.addBead(makePlanEpic("gd-PAR1", "Parity plan", "project:gardenio"))
	setupGardenerStub(t, stubA)
	resetPlanGardenerFlags(t)
	t.Cleanup(func() { resetPlanGardenerFlags(t) })
	if err := runPlanGardenerCmd(t, "gd-PAR1"); err != nil {
		t.Fatalf("gb plan gardener: %v", err)
	}
	cliFields, _ := stubA.creates[0]["fields"].(map[string]any)
	cliLabels := toStringSlice(stubA.creates[0]["labels"])

	// Path B — a direct shared-core Bind against a second, identical stub. The
	// daemon client and operator are the same globals the CLI used, so the
	// owner resolution round-trip resolves identically.
	stubB := newGardenerStub()
	setupGardenerStub(t, stubB) // rebinds `daemon` to stubB's server
	planBead := &beadsapi.BeadDetail{
		ID: "gd-PAR1", Title: "Parity plan", Type: "epic", Kind: "issue",
		Status: "open", Labels: []string{"plan", "project:gardenio"}, Fields: map[string]string{},
	}
	if _, err := plangardener.Bind(context.Background(), daemon, planBead, plangardener.BindSpec{
		Project:   "gardenio",
		Operator:  operatedBy,
		CreatedBy: actor,
	}); err != nil {
		t.Fatalf("plangardener.Bind: %v", err)
	}
	if len(stubB.creates) != 1 {
		t.Fatalf("direct Bind should create exactly 1 gardener, got %d", len(stubB.creates))
	}
	coreFields, _ := stubB.creates[0]["fields"].(map[string]any)
	coreLabels := toStringSlice(stubB.creates[0]["labels"])

	// Labels identical.
	if !reflect.DeepEqual(cliLabels, coreLabels) {
		t.Errorf("labels differ:\n CLI:  %v\n core: %v", cliLabels, coreLabels)
	}
	// Every field the two builders emit must match byte-for-byte after JSON
	// round-trip (the gardener blob + prompt + conditions are all strings).
	cliJSON, _ := json.Marshal(cliFields)
	coreJSON, _ := json.Marshal(coreFields)
	if string(cliJSON) != string(coreJSON) {
		t.Errorf("gardener trigger fields differ between gb plan gardener and plangardener.Bind:\n CLI:  %s\n core: %s", cliJSON, coreJSON)
	}
}

// toStringSlice normalizes a captured CreateBead labels field (a []any after
// JSON round-trip through the stub) to a []string for comparison.
func toStringSlice(v any) []string {
	raw, _ := v.([]any)
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// TestPlanGardener_TimedFlagOptsIntoTimedMode: --timed binds the timed shape on
// the staggered 30m cron, and --cron binds it on the given cron. Both keep
// today's trigger:schedule gardener for a caller who asks for the clock.
func TestPlanGardener_TimedFlagOptsIntoTimedMode(t *testing.T) {
	for _, tc := range []struct {
		flag, val, cron string
	}{
		{"timed", "true", gardenermode.StaggeredCron("gd-PLANT", 30*time.Minute)},
		{"cron", "0 9 * * *", "0 9 * * *"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			stub := newGardenerStub()
			stub.addBead(makePlanEpic("gd-PLANT", "Timed plan", "project:gardenio"))
			setupGardenerStub(t, stub)
			resetPlanGardenerFlags(t)
			t.Cleanup(func() { resetPlanGardenerFlags(t) })
			_ = planGardenerCmd.Flags().Set(tc.flag, tc.val)

			if err := runPlanGardenerCmd(t, "gd-PLANT"); err != nil {
				t.Fatalf("RunE: %v", err)
			}
			fields, _ := stub.creates[0]["fields"].(map[string]any)
			if fields["cron"] != tc.cron {
				t.Errorf("fields.cron = %v, want %q", fields["cron"], tc.cron)
			}
			if _, ok := fields["triggers"]; ok {
				t.Errorf("a timed bind carries no triggers[], got %v", fields["triggers"])
			}
			labels := toStringSlice(stub.creates[0]["labels"])
			if !slices.Contains(labels, "trigger:schedule") || slices.Contains(labels, "trigger:multi") {
				t.Errorf("labels = %v, want trigger:schedule and no trigger:multi", labels)
			}
		})
	}
}

// TestPlanGardener_FallbackCronWithTimedRefused: --fallback-cron is an
// event-mode knob; combining it with --timed is a usage error before any write.
func TestPlanGardener_FallbackCronWithTimedRefused(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makePlanEpic("gd-PLANX", "Bad flags", "project:gardenio"))
	setupGardenerStub(t, stub)
	resetPlanGardenerFlags(t)
	t.Cleanup(func() { resetPlanGardenerFlags(t) })
	_ = planGardenerCmd.Flags().Set("timed", "true")
	_ = planGardenerCmd.Flags().Set("fallback-cron", "0 4 * * *")

	if err := runPlanGardenerCmd(t, "gd-PLANX"); err == nil || !strings.Contains(err.Error(), "--fallback-cron") {
		t.Fatalf("want a --fallback-cron usage error, got %v", err)
	}
	if len(stub.creates) != 0 {
		t.Fatalf("no gardener must be created; got %d creates", len(stub.creates))
	}
}

// TestPlanGardener_NoReadInstructionsGate locks the ka-0000006130 decision:
// `gb plan gardener` is DELIBERATELY EXEMPT from the
// --i-read-the-gardener-instructions gate that guards `gb gardener start`
// (ka-0000007415). Unlike a hand-rolled `gb gardener start`, a plan-gardener has
// no author-supplied shape — plangardener.Bind builds the whole bead and
// structurally enforces the five non-negotiable rules before persist — so there
// is nothing for the echoed guide to correct. Binding must therefore SUCCEED
// with no such flag registered and no flag value threaded in.
//
// This is a guard test: if someone later adds the gate to this command (a
// required bool flag defaulting false), a bind with no flag set would start
// erroring and this test would go red, forcing a re-litigation of the decision.
func TestPlanGardener_NoReadInstructionsGate(t *testing.T) {
	// The flag must NOT exist on this command (it is gb-gardener-start-only).
	if f := planGardenerCmd.Flags().Lookup("i-read-the-gardener-instructions"); f != nil {
		t.Fatalf("gb plan gardener must NOT carry the --i-read-the-gardener-instructions gate (ka-0000006130: plangardener.Bind already enforces the shape); found flag %q", f.Name)
	}

	// And a plain bind — no acknowledgement flag anywhere — must succeed.
	stub := newGardenerStub()
	stub.addBead(makePlanEpic("gd-PLANNG", "Ungated bind", "project:gardenio"))
	setupGardenerStub(t, stub)
	resetPlanGardenerFlags(t)
	t.Cleanup(func() { resetPlanGardenerFlags(t) })

	if err := runPlanGardenerCmd(t, "gd-PLANNG"); err != nil {
		t.Fatalf("gb plan gardener must bind without any read-instructions gate, got: %v", err)
	}
	if len(stub.creates) != 1 {
		t.Fatalf("exactly one plan gardener must be created, got %d", len(stub.creates))
	}
}

// TestPlanGardener_RejectsNonEpic verifies a task/bug id is refused outright.
func TestPlanGardener_RejectsNonEpic(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(map[string]any{
		"id": "gd-TASK1", "title": "a task", "type": "task", "kind": "issue",
		"status": "open", "fields": map[string]any{}, "labels": []string{"plan"},
	})
	setupGardenerStub(t, stub)
	resetPlanGardenerFlags(t)
	t.Cleanup(func() { resetPlanGardenerFlags(t) })

	err := runPlanGardenerCmd(t, "gd-TASK1")
	if err == nil {
		t.Fatal("expected an error for a non-epic bead")
	}
	if !strings.Contains(err.Error(), "not an epic") {
		t.Errorf("error should explain the type mismatch, got: %v", err)
	}
}
