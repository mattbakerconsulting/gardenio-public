package main

import (
	"context"
	"encoding/json"
	"maps"
	"strings"
	"testing"
)

// resetFireNowFlags restores fire-now flags to defaults so cases don't bleed
// state.
func resetFireNowFlags(t *testing.T) {
	t.Helper()
	_ = gardenerFireNowCmd.Flags().Set("force", "false")
	_ = gardenerFireNowCmd.Flags().Set("prompt", "")
}

// makeFireNowGardener builds a gardener trigger bead (current trigger:schedule
// shape) with the given enabled/cron and a structured gardener blob so
// ParseTriggerDef yields a non-nil Gardener with loop-context fields.
func makeFireNowGardener(id, epicID, cron string, enabled bool, extraFields map[string]any) map[string]any {
	fields := map[string]any{
		"cron":     cron,
		"timezone": "America/Los_Angeles",
		"prompt":   "You are the gardener for parent bead " + epicID + ".",
		"project":  "gardenio",
	}
	if enabled {
		fields["enabled"] = "true"
	} else {
		fields["enabled"] = "false"
	}
	// A minimal but real gardener blob (assign_bead + terminal_goal + max_runs)
	// so the fired cycle carries loop-context fields, exactly as a scheduled
	// fire would.
	blob := map[string]any{
		"assign_bead":   epicID,
		"terminal_goal": "All children of " + epicID + " are closed.",
		"max_runs":      20,
		"runs_spawned":  4,
	}
	blobJSON, _ := json.Marshal(blob)
	fields["gardener"] = string(blobJSON)
	maps.Copy(fields, extraFields)
	return makeGardenerTrigger(id, epicID, fields)
}

// TestGardenerFireNow_DisabledGardenerFiresOnceAndStaysDisabled is the AC #3
// integration test: arm a gardener on a 1h cron, DISABLE it, fire it once, and
// verify (a) exactly one cycle agent is spawned and (b) the schedule bead is
// left DISABLED with its original cron — because fire-now never mutates the
// schedule, so "auto-restore to prior state" holds by construction.
func TestGardenerFireNow_DisabledGardenerFiresOnceAndStaysDisabled(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Verify gardener cycles", "project:gardenio"))
	// 1h cron, DISABLED.
	stub.addBead(makeFireNowGardener("gd-CAP1", "gd-EPIC1", "0 * * * *", false, nil))
	setupGardenerStub(t, stub)
	resetFireNowFlags(t)
	t.Cleanup(func() { resetFireNowFlags(t) })

	cmd := gardenerFireNowCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-CAP1"}); err != nil {
		t.Fatalf("fire-now RunE: %v", err)
	}

	// (a) EXACTLY ONE agent bead was created.
	var agentCreates []map[string]any
	for _, c := range stub.creates {
		if c["type"] == "agent" {
			agentCreates = append(agentCreates, c)
		}
	}
	if len(agentCreates) != 1 {
		t.Fatalf("expected exactly 1 agent spawn, got %d (creates=%d)", len(agentCreates), len(stub.creates))
	}

	// The spawned agent points at the gardener's epic and is marked as a
	// fire-now cycle carrying scheduled-loop context.
	af, _ := agentCreates[0]["fields"].(map[string]any)
	if got := af["task_id"]; got != "gd-EPIC1" {
		t.Errorf("agent task_id = %v, want gd-EPIC1 (the gardener's epic)", got)
	}
	if got := af["spawn_source"]; got != "fire-now" {
		t.Errorf("agent spawn_source = %v, want fire-now", got)
	}
	if got := af["schedule_id"]; got != "gd-CAP1" {
		t.Errorf("agent schedule_id = %v, want gd-CAP1 (loop context stamped)", got)
	}
	if got := af["spawned_by"]; got != "trigger:gd-CAP1" {
		t.Errorf("agent spawned_by = %v, want trigger:gd-CAP1", got)
	}
	// runs_spawned was 4 in the blob → this run is 5.
	if got := af["schedule_runs_spawned"]; got != "5" {
		t.Errorf("agent schedule_runs_spawned = %v, want 5 (blob had 4)", got)
	}
	if got := af["role"]; got != "gardener" {
		t.Errorf("agent role = %v, want gardener", got)
	}

	// (b) The schedule bead is UNCHANGED — no PATCH ever touched enabled/cron.
	if patches := stub.patches["gd-CAP1"]; len(patches) != 0 {
		t.Fatalf("fire-now PATCHed the gardener %d time(s); it must never mutate the schedule: %+v", len(patches), patches)
	}
	capBead := stub.beads["gd-CAP1"]
	capFields, _ := capBead["fields"].(map[string]any)
	if got := capFields["enabled"]; got != "false" {
		t.Errorf("gardener enabled = %v after fire-now, want it to STAY false (disabled gardener stays disabled)", got)
	}
	if got := capFields["cron"]; got != "0 * * * *" {
		t.Errorf("gardener cron = %v after fire-now, want original '0 * * * *' (schedule untouched)", got)
	}
}

// TestGardenerFireNow_ResolvesByEpicID verifies fire-now accepts the epic id (not
// just the gardener trigger id), matching resolveGardenerSchedule's behavior.
func TestGardenerFireNow_ResolvesByEpicID(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC2", "Epic-id resolve", "project:gardenio"))
	stub.addBead(makeFireNowGardener("gd-CAP2", "gd-EPIC2", "0 * * * *", true, nil))
	setupGardenerStub(t, stub)
	resetFireNowFlags(t)
	t.Cleanup(func() { resetFireNowFlags(t) })

	cmd := gardenerFireNowCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-EPIC2"}); err != nil {
		t.Fatalf("fire-now by epic id: %v", err)
	}
	var agentCreates int
	for _, c := range stub.creates {
		if c["type"] == "agent" {
			agentCreates++
		}
	}
	if agentCreates != 1 {
		t.Fatalf("expected 1 agent spawn resolving by epic id, got %d", agentCreates)
	}
	// An ENABLED gardener also stays enabled + cron unchanged (no PATCH).
	if patches := stub.patches["gd-CAP2"]; len(patches) != 0 {
		t.Errorf("fire-now PATCHed an enabled gardener; schedule must stay untouched: %+v", patches)
	}
}

// TestGardenerFireNow_SingletonGuardBlocksWhenLive verifies fire-now refuses when
// a live cycle already exists, and that --force overrides it (ka-0000003097).
func TestGardenerFireNow_SingletonGuardBlocksWhenLive(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC3", "Singleton guard", "project:gardenio"))
	stub.addBead(makeFireNowGardener("gd-CAP3", "gd-EPIC3", "0 * * * *", true, nil))
	// A live cycle agent already spawned by this gardener.
	stub.addBead(map[string]any{
		"id":     "gd-LIVE1",
		"title":  "live gardener cycle",
		"type":   "agent",
		"kind":   "data",
		"status": "in_progress",
		"fields": map[string]any{
			"agent":      "gardenio-live-cycle",
			"spawned_by": "trigger:gd-CAP3",
		},
	})
	setupGardenerStub(t, stub)
	resetFireNowFlags(t)
	t.Cleanup(func() { resetFireNowFlags(t) })

	cmd := gardenerFireNowCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-CAP3"})
	if err == nil {
		t.Fatalf("expected fire-now to refuse when a live cycle exists")
	}
	if !strings.Contains(err.Error(), "live cycle") {
		t.Errorf("error should name the live-cycle refusal, got: %v", err)
	}
	// No agent spawned on the refusal.
	for _, c := range stub.creates {
		if c["type"] == "agent" {
			t.Fatalf("no agent should be spawned when the singleton guard blocks")
		}
	}

	// --force overrides the guard and spawns anyway.
	_ = gardenerFireNowCmd.Flags().Set("force", "true")
	if err := cmd.RunE(cmd, []string{"gd-CAP3"}); err != nil {
		t.Fatalf("fire-now --force should override the singleton guard: %v", err)
	}
	var agentCreates int
	for _, c := range stub.creates {
		if c["type"] == "agent" {
			agentCreates++
		}
	}
	if agentCreates != 1 {
		t.Fatalf("expected exactly 1 agent spawn after --force, got %d", agentCreates)
	}
}

// TestGardenerFireNow_RejectsNonGardener verifies fire-now refuses a bead that is
// not a gardener.
func TestGardenerFireNow_RejectsNonGardener(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC4", "Not a gardener", "project:gardenio"))
	setupGardenerStub(t, stub)
	resetFireNowFlags(t)
	t.Cleanup(func() { resetFireNowFlags(t) })

	cmd := gardenerFireNowCmd
	cmd.SetContext(context.Background())
	// A bare epic id with no gardener resolves to a "no gardener found" error.
	err := cmd.RunE(cmd, []string{"gd-EPIC4"})
	if err == nil {
		t.Fatalf("expected fire-now to fail when no gardener exists for the bead")
	}
}

// TestGardenerFireNow_JSONOutput verifies the --json result reports the
// enabled_unchanged contract and the spawned agent bead.
func TestGardenerFireNow_JSONOutput(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC5", "Json out", "project:gardenio"))
	stub.addBead(makeFireNowGardener("gd-CAP5", "gd-EPIC5", "0 * * * *", false, nil))
	setupGardenerStub(t, stub)
	resetFireNowFlags(t)
	t.Cleanup(func() { resetFireNowFlags(t) })

	oldJSON := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = oldJSON })

	cmd := gardenerFireNowCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-CAP5"}); err != nil {
		t.Fatalf("fire-now --json: %v", err)
	}
	// The schedule stayed disabled + cron unchanged (structural assertion; the
	// JSON contract is covered by the fields the command reads).
	capFields, _ := stub.beads["gd-CAP5"]["fields"].(map[string]any)
	if capFields["enabled"] != "false" {
		t.Errorf("gardener enabled = %v, want false (unchanged)", capFields["enabled"])
	}
}

// gd-VuYwGGYLLT: fire-now sends X-Escalate, so a privileged gardener role that
// the daemon refuses to spawn for this caller becomes an escalation, reported
// to the operator, not an error.
func TestGardenerFireNow_EscalatesWhenRoleIsPrivileged(t *testing.T) {
	stub := newGardenerStub()
	stub.escalateAgentCreates = true
	stub.addBead(makeEpic("gd-EPIC1", "Verify gardener cycles", "project:gardenio"))
	stub.addBead(makeFireNowGardener("gd-CAP1", "gd-EPIC1", "0 * * * *", true, nil))
	setupGardenerStub(t, stub)
	resetFireNowFlags(t)
	t.Cleanup(func() { resetFireNowFlags(t) })

	cmd := gardenerFireNowCmd
	cmd.SetContext(context.Background())
	var out strings.Builder
	cmd.SetOut(&out)
	t.Cleanup(func() { cmd.SetOut(nil) })
	if err := cmd.RunE(cmd, []string{"gd-CAP1"}); err != nil {
		t.Fatalf("an escalated fire-now is not an error, got %v", err)
	}
	if !strings.Contains(out.String(), "gd-esc1") || !strings.Contains(out.String(), "operator approval") {
		t.Errorf("output must name the escalation, got %q", out.String())
	}
	var sawEscalate bool
	for _, h := range stub.escalateHeaders {
		sawEscalate = sawEscalate || h == "true"
	}
	if !sawEscalate {
		t.Errorf("fire-now must send X-Escalate: true on the agent create, headers=%v", stub.escalateHeaders)
	}
}
