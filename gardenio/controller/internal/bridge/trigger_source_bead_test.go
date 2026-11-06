package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

func TestBeadStateTriggerSource_Name(t *testing.T) {
	src := NewBeadStateTriggerSource(nil)
	if src.Name() != "bead" {
		t.Errorf("Name() = %q, want %q", src.Name(), "bead")
	}
}

func TestBeadStateTriggerSource_LoadTriggers(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: triggerBeads(
			triggerBead("t1", "Spawn on task created", "trigger:bead", map[string]string{
				"action":      "spawn_agent",
				"event_types": "created",
				"conditions":  `[{"field":"type","op":"==","value":"task"}]`,
				"role":        "crew",
				"project":     "gardenio",
				"prompt":      "Work on {{bead_title}}",
			}),
			triggerBead("t2", "Disabled trigger", "trigger:bead", map[string]string{
				"enabled": "false",
			}),
			triggerBead("t3", "No conditions", "trigger:bead", map[string]string{
				"action":      "notify",
				"event_types": "closed",
			}),
			// Different source — should be filtered out by label.
			triggerBead("t4", "JIRA trigger", "trigger:jira", map[string]string{
				"jql": "project = PE",
			}),
		),
	}

	src := NewBeadStateTriggerSource(slog.Default())
	triggers, err := src.LoadTriggers(context.Background(), daemon)
	if err != nil {
		t.Fatalf("LoadTriggers: %v", err)
	}

	if len(triggers) != 2 {
		t.Fatalf("expected 2 triggers (t1, t3), got %d", len(triggers))
	}

	// Verify t1.
	if triggers[0].ID != "t1" {
		t.Errorf("triggers[0].ID = %q", triggers[0].ID)
	}
	data := triggers[0].SourceData.(beadTriggerData)
	if len(data.EventTypes) != 1 || data.EventTypes[0] != "created" {
		t.Errorf("EventTypes = %v", data.EventTypes)
	}
	if len(data.Conditions) != 1 || data.Conditions[0].Field != "type" {
		t.Errorf("Conditions = %+v", data.Conditions)
	}

	// Verify t3 (no conditions, event_types=closed).
	if triggers[1].ID != "t3" {
		t.Errorf("triggers[1].ID = %q", triggers[1].ID)
	}
	data3 := triggers[1].SourceData.(beadTriggerData)
	if len(data3.EventTypes) != 1 || data3.EventTypes[0] != "closed" {
		t.Errorf("EventTypes = %v", data3.EventTypes)
	}
	if len(data3.Conditions) != 0 {
		t.Errorf("expected 0 conditions, got %d", len(data3.Conditions))
	}
}

func TestBeadStateTriggerSource_Evaluate(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	// Buffer some events.
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{
			Topic: "beads.bead.created",
			Bead: &BeadEvent{
				ID: "gd-1", Type: "task", Title: "My task",
				Status: "open", Labels: []string{"project:gardenio"},
				Fields: map[string]string{"agent_state": ""},
			},
		},
		{
			Topic: "beads.bead.created",
			Bead: &BeadEvent{
				ID: "gd-2", Type: "epic", Title: "My epic",
				Status: "open", Labels: []string{"project:gardenio"},
			},
		},
		{
			Topic: "beads.bead.updated",
			Bead: &BeadEvent{
				ID: "gd-3", Type: "agent", Title: "Agent bead",
				Status: "open", Fields: map[string]string{"agent_state": "failed"},
				Labels: []string{"project:gardenio"},
			},
		},
		{
			Topic: "beads.bead.closed",
			Bead: &BeadEvent{
				ID: "gd-4", Type: "task", Title: "Closed task",
				Status: "closed", ClosedBy: "user",
			},
		},
	}
	src.mu.Unlock()

	// Trigger: match created tasks only.
	trig := TriggerDef{
		ID: "t1", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"created"},
			Conditions: []beadCondition{
				{Field: "type", Op: "==", Value: "task"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match, got %d", len(matches))
	}
	if matches[0].Key != "gd-1:created" {
		t.Errorf("match key = %q", matches[0].Key)
	}
	if matches[0].Title != "My task" {
		t.Errorf("match title = %q", matches[0].Title)
	}
	if matches[0].TemplateVars["{{bead_type}}"] != "task" {
		t.Errorf("template var bead_type = %q", matches[0].TemplateVars["{{bead_type}}"])
	}
}

func TestBeadStateTriggerSource_EvaluateAgentStateFailed(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	src.mu.Lock()
	src.pending = []beadStateEvent{
		{
			Topic: "beads.bead.updated",
			Bead: &BeadEvent{
				ID: "gd-agent-1", Type: "agent", Title: "Agent bead",
				Status: "open", Fields: map[string]string{"agent_state": "failed"},
				Labels: []string{"project:gardenio"},
			},
		},
		{
			Topic: "beads.bead.updated",
			Bead: &BeadEvent{
				ID: "gd-agent-2", Type: "agent", Title: "Working agent",
				Status: "open", Fields: map[string]string{"agent_state": "working"},
				Labels: []string{"project:gardenio"},
			},
		},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "t-escalation", Source: "bead", Action: TriggerActionCreateBead, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"updated"},
			Conditions: []beadCondition{
				{Field: "type", Op: "==", Value: "agent"},
				{Field: "agent_state", Op: "==", Value: "failed"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match (failed agent), got %d", len(matches))
	}
	if matches[0].Key != "gd-agent-1:updated" {
		t.Errorf("match key = %q", matches[0].Key)
	}
}

func TestBeadStateTriggerSource_EvaluateAllEvents(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.created", Bead: &BeadEvent{ID: "gd-1", Type: "task", Status: "open"}},
		{Topic: "beads.bead.updated", Bead: &BeadEvent{ID: "gd-2", Type: "task", Status: "in_progress"}},
		{Topic: "beads.bead.closed", Bead: &BeadEvent{ID: "gd-3", Type: "task", Status: "closed"}},
	}
	src.mu.Unlock()

	// No event_types filter = match all.
	trig := TriggerDef{
		ID: "t-all", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 3 {
		t.Errorf("expected 3 matches (all events), got %d", len(matches))
	}
}

func TestBeadStateTriggerSource_LabelConditions(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	src.mu.Lock()
	src.pending = []beadStateEvent{
		{
			Topic: "beads.bead.created",
			Bead: &BeadEvent{
				ID: "gd-1", Type: "task",
				Labels: []string{"project:gardenio", "priority:high"},
			},
		},
		{
			Topic: "beads.bead.created",
			Bead: &BeadEvent{
				ID: "gd-2", Type: "task",
				Labels: []string{"project:other"},
			},
		},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "t-label", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"created"},
			Conditions: []beadCondition{
				{Field: "label:project:gardenio", Op: "present"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match (with project:gardenio label), got %d", len(matches))
	}
	if matches[0].Key != "gd-1:created" {
		t.Errorf("match key = %q", matches[0].Key)
	}
}

func TestBeadStateTriggerSource_LabelAbsent(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	src.mu.Lock()
	src.pending = []beadStateEvent{
		{
			Topic: "beads.bead.created",
			Bead: &BeadEvent{
				ID: "gd-1", Type: "task",
				Labels: []string{"project:gardenio"},
			},
		},
		{
			Topic: "beads.bead.created",
			Bead: &BeadEvent{
				ID: "gd-2", Type: "task",
				Labels: []string{"project:gardenio", "internal"},
			},
		},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "t-no-internal", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"created"},
			Conditions: []beadCondition{
				{Field: "label:internal", Op: "absent"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match (without internal label), got %d", len(matches))
	}
	if matches[0].Key != "gd-1:created" {
		t.Errorf("match key = %q", matches[0].Key)
	}
}

func TestBeadStateTriggerSource_InOperator(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.created", Bead: &BeadEvent{ID: "gd-1", Type: "task"}},
		{Topic: "beads.bead.created", Bead: &BeadEvent{ID: "gd-2", Type: "bug"}},
		{Topic: "beads.bead.created", Bead: &BeadEvent{ID: "gd-3", Type: "epic"}},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "t-in", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"created"},
			Conditions: []beadCondition{
				{Field: "type", Op: "in", Value: "task,bug"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 2 {
		t.Errorf("expected 2 matches (task, bug), got %d", len(matches))
	}
}

func TestBeadStateTriggerSource_NotInOperator(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.closed", Bead: &BeadEvent{ID: "gd-1", Type: "task", Status: "closed"}},
		{Topic: "beads.bead.closed", Bead: &BeadEvent{ID: "gd-2", Type: "agent", Status: "closed"}},
		{Topic: "beads.bead.closed", Bead: &BeadEvent{ID: "gd-3", Type: "config", Status: "closed"}},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "t-not-in", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"closed"},
			Conditions: []beadCondition{
				{Field: "type", Op: "not_in", Value: "agent,config"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Errorf("expected 1 match (task only), got %d", len(matches))
	}
}

func TestBeadStateTriggerSource_ContainsOperator(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.created", Bead: &BeadEvent{ID: "gd-1", Title: "Fix auth bug in login"}},
		{Topic: "beads.bead.created", Bead: &BeadEvent{ID: "gd-2", Title: "Add new feature"}},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "t-contains", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"created"},
			Conditions: []beadCondition{
				{Field: "title", Op: "contains", Value: "bug"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Errorf("expected 1 match (title contains 'bug'), got %d", len(matches))
	}
}

func TestBeadStateTriggerSource_DrainPending(t *testing.T) {
	src := NewBeadStateTriggerSource(nil)

	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.created", Bead: &BeadEvent{ID: "gd-1"}},
		{Topic: "beads.bead.created", Bead: &BeadEvent{ID: "gd-2"}},
	}
	src.mu.Unlock()

	if src.PendingCount() != 2 {
		t.Errorf("expected 2 pending, got %d", src.PendingCount())
	}

	src.DrainPending()

	if src.PendingCount() != 0 {
		t.Errorf("expected 0 pending after drain, got %d", src.PendingCount())
	}
}

// TestBeadStateTriggerSource_FreezeKeepsMidPassEvents: an event buffered
// after FreezePending is invisible to that pass and survives its
// DrainPending (gd-IQcMk16PkH).
func TestBeadStateTriggerSource_FreezeKeepsMidPassEvents(t *testing.T) {
	src := NewBeadStateTriggerSource(nil)
	trig := TriggerDef{
		ID:         "gd-bead-freeze",
		Title:      "freeze",
		SourceData: beadTriggerData{EventTypes: []string{"created"}},
	}

	src.InjectBeadEvent("beads.bead.created", &BeadEvent{ID: "gd-before", Type: "task"})
	src.FreezePending()
	src.InjectBeadEvent("beads.bead.created", &BeadEvent{ID: "gd-mid", Type: "task"})

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 || matches[0].TemplateVars["{{bead_id}}"] != "gd-before" {
		t.Fatalf("frozen pass matched %+v, want only gd-before", matches)
	}
	src.DrainPending()

	matches, err = src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 || matches[0].TemplateVars["{{bead_id}}"] != "gd-mid" {
		t.Fatalf("next pass matched %+v, want only gd-mid", matches)
	}
}

func TestBeadStateTriggerSource_CatchUpKeys(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: triggerBeads(
			&beadsapi.BeadDetail{
				ID: "gd-task-1", Type: "task", Status: "open",
				Labels: []string{"trigger:t-bead-1"},
				Fields: map[string]string{"bead_event_key": "gd-abc:created"},
			},
			&beadsapi.BeadDetail{
				ID: "gd-task-2", Type: "task", Status: "closed",
				Labels: []string{"trigger:t-bead-1"},
				Fields: map[string]string{"bead_event_key": "gd-def:updated"},
			},
		),
	}

	src := NewBeadStateTriggerSource(slog.Default())
	keys, err := src.CatchUpKeys(context.Background(), daemon, "t-bead-1")
	if err != nil {
		t.Fatalf("CatchUpKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
}

func TestBeadStateTriggerSource_EngineIntegration(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	src := NewBeadStateTriggerSource(slog.Default())
	engine.RegisterSource(src)

	// Simulate incoming SSE event.
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{
			Topic: "beads.bead.created",
			Bead: &BeadEvent{
				ID: "gd-new-task", Type: "task", Title: "New task from SSE",
				Status: "open", Labels: []string{"project:gardenio"},
			},
		},
	}
	src.mu.Unlock()

	// Set up trigger bead in daemon.
	daemon.mu.Lock()
	daemon.beads = triggerBeads(
		triggerBead("t-bead-1", "Auto-spawn on task create", "trigger:bead", map[string]string{
			"action":      "spawn_agent",
			"event_types": "created",
			"conditions":  `[{"field":"type","op":"==","value":"task"}]`,
			"role":        "crew",
			"project":     "gardenio",
			"prompt":      "Work on {{bead_title}}",
		}),
	)
	daemon.mu.Unlock()

	engine.EvaluateSource(context.Background(), "bead")

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.createdBeads) != 1 {
		t.Fatalf("expected 1 created bead, got %d", len(daemon.createdBeads))
	}
	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawned agent, got %d", len(daemon.spawnedNames))
	}
	if daemon.lastPrompt != "Work on New task from SSE" {
		t.Errorf("prompt = %q", daemon.lastPrompt)
	}
}

// TestBeadStateTriggerSource_ApplyFormulaWritesTriggeredByDeps drives a full
// SSE-event → BeadStateTriggerSource → engine → actionApplyFormula path and
// asserts that the molecule bead (created directly by the trigger after D6 /
// ka-0000006284 removed pending_formula) carries `triggered-by` deps to BOTH
// the trigger-definition bead AND the originating source bead. End-to-end
// form of the D4 lineage primitive (epic ka-0000007507).
func TestBeadStateTriggerSource_ApplyFormulaWritesTriggeredByDeps(t *testing.T) {
	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{testFormulaBead("gd-formula-incoming", "BEAD")},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	src := NewBeadStateTriggerSource(slog.Default())
	engine.RegisterSource(src)

	src.mu.Lock()
	src.pending = []beadStateEvent{
		{
			Topic: "beads.bead.created",
			Bead: &BeadEvent{
				ID: "gd-incoming-99", Type: "task", Title: "Incoming task",
				Status: "open", Labels: []string{"project:gardenio"},
			},
		},
	}
	src.mu.Unlock()

	daemon.mu.Lock()
	daemon.beads = append(daemon.beads, triggerBeads(
		triggerBead("t-formula-bead", "Apply formula on incoming", "trigger:bead", map[string]string{
			"action":       "apply_formula",
			"event_types":  "created",
			"conditions":   `[{"field":"type","op":"==","value":"task"}]`,
			"formula_id":   "gd-formula-incoming",
			"formula_vars": `{"BEAD":"{{bead_id}}"}`,
			"project":      "gardenio",
		}),
	)...)
	daemon.mu.Unlock()

	engine.EvaluateSource(context.Background(), "bead")

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.createdBeads) < 2 {
		t.Fatalf("expected molecule + step beads, got %d", len(daemon.createdBeads))
	}
	if daemon.createdBeads[0].Type != "molecule" {
		t.Fatalf("first created bead type = %q, want molecule", daemon.createdBeads[0].Type)
	}

	wantTargets := map[string]bool{
		"t-formula-bead": false, // trigger bead
		"gd-incoming-99": false, // source bead
	}
	for _, d := range daemon.addedDeps {
		if d.DepType != "triggered-by" {
			continue
		}
		if _, ok := wantTargets[d.DependsOnID]; !ok {
			t.Errorf("unexpected dep target %q (dep=%+v)", d.DependsOnID, d)
			continue
		}
		wantTargets[d.DependsOnID] = true
	}
	for target, seen := range wantTargets {
		if !seen {
			t.Errorf("missing triggered-by dep to %q (deps=%+v)", target, daemon.addedDeps)
		}
	}
}

// TestBeadStateTriggerSource_SpawnedTask_DependsOnTriggerAndSourceBead drives
// a full SSE-event → engine → actionSpawnAgent path and asserts that the
// spawned tracking task carries `triggered-by` deps to BOTH the trigger
// definition bead AND the originating source bead. Auto-link M2a
// (ka-0000007272, epic ka-0000000933).
func TestBeadStateTriggerSource_SpawnedTask_DependsOnTriggerAndSourceBead(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	src := NewBeadStateTriggerSource(slog.Default())
	engine.RegisterSource(src)

	src.mu.Lock()
	src.pending = []beadStateEvent{
		{
			Topic: "beads.bead.created",
			Bead: &BeadEvent{
				ID: "gd-source-m2a", Type: "task", Title: "Originating task",
				Status: "open", Labels: []string{"project:gardenio"},
			},
		},
	}
	src.mu.Unlock()

	daemon.mu.Lock()
	daemon.beads = triggerBeads(
		triggerBead("t-bead-m2a", "Auto-spawn on task create", "trigger:bead", map[string]string{
			"action":      "spawn_agent",
			"event_types": "created",
			"conditions":  `[{"field":"type","op":"==","value":"task"}]`,
			"role":        "crew",
			"project":     "gardenio",
			"prompt":      "Work on {{bead_title}}",
		}),
	)
	daemon.mu.Unlock()

	engine.EvaluateSource(context.Background(), "bead")

	daemon.mu.Lock()
	defer daemon.mu.Unlock()

	if len(daemon.spawnedNames) != 1 {
		t.Fatalf("expected 1 spawned agent, got %d", len(daemon.spawnedNames))
	}

	wantTargets := map[string]bool{
		"t-bead-m2a":    false, // trigger bead
		"gd-source-m2a": false, // source bead
	}
	for _, d := range daemon.addedDeps {
		if d.DepType != "triggered-by" {
			t.Errorf("unexpected dep type %q (dep=%+v)", d.DepType, d)
			continue
		}
		if _, ok := wantTargets[d.DependsOnID]; !ok {
			t.Errorf("unexpected dep target %q (dep=%+v)", d.DependsOnID, d)
			continue
		}
		wantTargets[d.DependsOnID] = true
	}
	for target, seen := range wantTargets {
		if !seen {
			t.Errorf("missing triggered-by dep to %q (deps=%+v)", target, daemon.addedDeps)
		}
	}
}

func TestBeadStateTriggerSource_EngineDedup(t *testing.T) {
	daemon := &mockTriggerDaemon{}
	engine := NewTriggerEngine(TriggerEngineConfig{
		Daemon: daemon,
		Logger: slog.Default(),
	})

	src := NewBeadStateTriggerSource(slog.Default())
	engine.RegisterSource(src)

	daemon.mu.Lock()
	daemon.beads = triggerBeads(
		triggerBead("t1", "Notify on close", "trigger:bead", map[string]string{
			"action":      "notify",
			"event_types": "closed",
		}),
	)
	daemon.mu.Unlock()

	var notifyCount int
	engine.notifier = func(_ context.Context, _, _ string, _ string) {
		notifyCount++
	}

	// First cycle: one event.
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.closed", Bead: &BeadEvent{ID: "gd-1", Title: "Done"}},
	}
	src.mu.Unlock()

	engine.EvaluateSource(context.Background(), "bead")
	if notifyCount != 1 {
		t.Errorf("expected 1 notification, got %d", notifyCount)
	}

	// Second cycle: same event should be deduped by engine.
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{Topic: "beads.bead.closed", Bead: &BeadEvent{ID: "gd-1", Title: "Done"}},
	}
	src.mu.Unlock()

	engine.EvaluateSource(context.Background(), "bead")
	if notifyCount != 1 {
		t.Errorf("expected still 1 notification (dedup), got %d", notifyCount)
	}
}

func TestMatchEventType(t *testing.T) {
	tests := []struct {
		name       string
		eventTypes []string
		topic      string
		want       bool
	}{
		{"empty filter matches all", nil, "beads.bead.created", true},
		{"short name match", []string{"created"}, "beads.bead.created", true},
		{"full topic match", []string{"beads.bead.updated"}, "beads.bead.updated", true},
		{"no match", []string{"created"}, "beads.bead.closed", false},
		{"multiple types", []string{"created", "closed"}, "beads.bead.closed", true},
		{"multiple no match", []string{"created", "closed"}, "beads.bead.updated", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchEventType(tt.eventTypes, tt.topic); got != tt.want {
				t.Errorf("matchEventType(%v, %q) = %v, want %v", tt.eventTypes, tt.topic, got, tt.want)
			}
		})
	}
}

func TestMatchConditions(t *testing.T) {
	bead := &BeadEvent{
		ID: "gd-1", Type: "task", Title: "Fix bug", Status: "open",
		Assignee: "alice", Labels: []string{"project:gardenio", "priority:high"},
		Fields: map[string]string{"agent_state": "working", "source": "jira"},
	}

	tests := []struct {
		name       string
		conditions []beadCondition
		want       bool
	}{
		{"empty conditions match", nil, true},
		{"type equals", []beadCondition{{Field: "type", Op: "==", Value: "task"}}, true},
		{"type not equals", []beadCondition{{Field: "type", Op: "==", Value: "epic"}}, false},
		{"status !=", []beadCondition{{Field: "status", Op: "!=", Value: "closed"}}, true},
		{"field in", []beadCondition{{Field: "agent_state", Op: "in", Value: "working,failed"}}, true},
		{"field not in", []beadCondition{{Field: "agent_state", Op: "not_in", Value: "spawning,failed"}}, true},
		{"title contains", []beadCondition{{Field: "title", Op: "contains", Value: "bug"}}, true},
		{"field present", []beadCondition{{Field: "agent_state", Op: "present"}}, true},
		{"field absent", []beadCondition{{Field: "nonexistent", Op: "absent"}}, true},
		{"label present", []beadCondition{{Field: "label:project:gardenio", Op: "present"}}, true},
		{"label absent", []beadCondition{{Field: "label:internal", Op: "absent"}}, true},
		{"multiple conditions AND", []beadCondition{
			{Field: "type", Op: "==", Value: "task"},
			{Field: "status", Op: "==", Value: "open"},
		}, true},
		{"multiple conditions AND fail", []beadCondition{
			{Field: "type", Op: "==", Value: "task"},
			{Field: "status", Op: "==", Value: "closed"},
		}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchConditions(tt.conditions, bead); got != tt.want {
				t.Errorf("matchConditions() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetBeadField(t *testing.T) {
	bead := &BeadEvent{
		ID: "gd-1", Type: "task", Title: "Test", Status: "open",
		Assignee: "alice", CreatedBy: "bob", ClosedBy: "charlie",
		Fields: map[string]string{"custom_field": "custom_value"},
	}

	tests := []struct {
		field string
		want  string
	}{
		{"type", "task"},
		{"status", "open"},
		{"title", "Test"},
		{"assignee", "alice"},
		{"created_by", "bob"},
		{"closed_by", "charlie"},
		{"id", "gd-1"},
		{"custom_field", "custom_value"},
		{"nonexistent", ""},
	}

	for _, tt := range tests {
		t.Run(tt.field, func(t *testing.T) {
			if got := getBeadField(tt.field, bead); got != tt.want {
				t.Errorf("getBeadField(%q) = %q, want %q", tt.field, got, tt.want)
			}
		})
	}
}

func TestBeadEventToMatch(t *testing.T) {
	evt := beadStateEvent{
		Topic: "beads.bead.updated",
		Bead: &BeadEvent{
			ID: "gd-agent-1", Type: "agent", Title: "My Agent",
			Status: "open", Assignee: "bot",
			Fields: map[string]string{"agent_state": "failed"},
			Labels: []string{"project:gardenio"},
		},
	}

	m := beadEventToMatch(evt)

	if m.Key != "gd-agent-1:updated" {
		t.Errorf("Key = %q", m.Key)
	}
	if m.Title != "My Agent" {
		t.Errorf("Title = %q", m.Title)
	}
	if m.TemplateVars["{{bead_id}}"] != "gd-agent-1" {
		t.Errorf("bead_id = %q", m.TemplateVars["{{bead_id}}"])
	}
	if m.TemplateVars["{{bead_event}}"] != "updated" {
		t.Errorf("bead_event = %q", m.TemplateVars["{{bead_event}}"])
	}
	if m.TemplateVars["{{agent_state}}"] != "failed" {
		t.Errorf("agent_state template var = %q", m.TemplateVars["{{agent_state}}"])
	}
	if m.Fields["bead_event_key"] != "gd-agent-1:updated" {
		t.Errorf("bead_event_key = %q", m.Fields["bead_event_key"])
	}
	if m.Fields["source_bead_id"] != "gd-agent-1" {
		t.Errorf("source_bead_id = %q", m.Fields["source_bead_id"])
	}
}

// TestBeadEventToMatch_LineageLabel asserts that every match emits a
// `source-bead:<id>` label, the cross-cutting marker the D4 trigger-bead
// lineage primitive (epic ka-0000007507) will promote into a parent-child
// dep from the trigger-created task → the source bead. Without this label,
// downstream lineage promotion has nothing to bind to.
func TestBeadEventToMatch_LineageLabel(t *testing.T) {
	cases := []struct {
		name  string
		topic string
		id    string
	}{
		{"created", "beads.bead.created", "gd-src-c"},
		{"updated", "beads.bead.updated", "gd-src-u"},
		{"closed", "beads.bead.closed", "gd-src-d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := beadEventToMatch(beadStateEvent{
				Topic: tc.topic,
				Bead:  &BeadEvent{ID: tc.id, Type: "task", Title: "x"},
			})

			wantLabel := "source-bead:" + tc.id
			found := false
			for _, l := range m.Labels {
				if l == wantLabel {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("match.Labels missing %q: got %v", wantLabel, m.Labels)
			}
			if m.Fields["source_bead_id"] != tc.id {
				t.Errorf("Fields[source_bead_id] = %q, want %q", m.Fields["source_bead_id"], tc.id)
			}
		})
	}
}

// TestBeadStateTriggerSource_InjectBeadEvent_Buffers verifies that
// InjectBeadEvent appends a synthetic event to the pending buffer, preserving
// the supplied topic, and that the nil/empty guards drop bad input rather than
// buffering an unevaluatable event. This is the reconciler replay path
// (ka-0000004769) — bead mutations the SSE stream missed are re-injected so
// triggers can still fire on them.
func TestBeadStateTriggerSource_InjectBeadEvent_Buffers(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	if src.PendingCount() != 0 {
		t.Fatalf("expected empty buffer at start, got %d", src.PendingCount())
	}

	// Guard: nil bead is dropped.
	src.InjectBeadEvent("beads.bead.reconciled", nil)
	if src.PendingCount() != 0 {
		t.Errorf("nil bead must not be buffered, got %d pending", src.PendingCount())
	}

	// Guard: empty topic is dropped even with a valid bead.
	src.InjectBeadEvent("", &BeadEvent{ID: "gd-1", Type: "task"})
	if src.PendingCount() != 0 {
		t.Errorf("empty topic must not be buffered, got %d pending", src.PendingCount())
	}

	// Happy path: a valid (topic, bead) pair is buffered with the topic
	// preserved verbatim.
	src.InjectBeadEvent("beads.bead.reconciled", &BeadEvent{ID: "gd-99", Type: "task", Title: "Replayed"})
	if src.PendingCount() != 1 {
		t.Fatalf("expected 1 pending after inject, got %d", src.PendingCount())
	}
	src.mu.Lock()
	got := src.pending[0]
	src.mu.Unlock()
	if got.Topic != "beads.bead.reconciled" {
		t.Errorf("buffered topic = %q, want %q", got.Topic, "beads.bead.reconciled")
	}
	if got.Bead == nil || got.Bead.ID != "gd-99" {
		t.Errorf("buffered bead = %+v, want ID gd-99", got.Bead)
	}
}

// TestBeadStateTriggerSource_InjectBeadEvent_Created drives an injected
// created event through Evaluate and asserts the resulting TriggerMatch under a
// type-equals predicate. The reconciled-created topic is one the SSE handlers
// would also have produced, so the dedup key uses the historical leaf-only
// "created" form.
func TestBeadStateTriggerSource_InjectBeadEvent_Created(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	src.InjectBeadEvent("beads.bead.created", &BeadEvent{
		ID: "gd-inj-1", Type: "task", Title: "Injected task",
		Status: "open", Labels: []string{"project:gardenio"},
	})

	trig := TriggerDef{
		ID: "t-created", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"created"},
			Conditions: []beadCondition{{Field: "type", Op: "==", Value: "task"}},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match for injected created task, got %d", len(matches))
	}
	if matches[0].Key != "gd-inj-1:created" {
		t.Errorf("match key = %q, want gd-inj-1:created", matches[0].Key)
	}
	if matches[0].Title != "Injected task" {
		t.Errorf("match title = %q", matches[0].Title)
	}
}

// TestBeadStateTriggerSource_InjectBeadEvent_Closed drives an injected closed
// event through Evaluate under a label predicate and confirms it matches.
func TestBeadStateTriggerSource_InjectBeadEvent_Closed(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	src.InjectBeadEvent("beads.bead.closed", &BeadEvent{
		ID: "gd-inj-closed", Type: "task", Title: "Closed task",
		Status: "closed", ClosedBy: "bot", Labels: []string{"project:gardenio"},
	})

	trig := TriggerDef{
		ID: "t-closed", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"closed"},
			Conditions: []beadCondition{{Field: "label:project:gardenio", Op: "present"}},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match for injected closed task, got %d", len(matches))
	}
	if matches[0].Key != "gd-inj-closed:closed" {
		t.Errorf("match key = %q, want gd-inj-closed:closed", matches[0].Key)
	}
}

// TestBeadStateTriggerSource_InjectBeadEvent_StateChange drives an injected
// updated (state-change) event carrying a Changes map and asserts the
// changed_to predicate fires, with the per-transition fingerprint folded into
// the dedup key and the {{changed_status}} template var surfaced.
func TestBeadStateTriggerSource_InjectBeadEvent_StateChange(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	src.InjectBeadEvent("beads.bead.updated", &BeadEvent{
		ID: "gd-inj-upd", Type: "task", Title: "Transitioned",
		Status:  "in_progress",
		Changes: map[string]any{"status": "in_progress"},
		Labels:  []string{"project:gardenio"},
	})

	trig := TriggerDef{
		ID: "t-state", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"updated"},
			Conditions: []beadCondition{{Field: "status", Op: "changed_to", Value: "in_progress"}},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match for injected state-change, got %d", len(matches))
	}
	if !strings.Contains(matches[0].Key, "status=in_progress") {
		t.Errorf("match.Key = %q does not embed the transition fingerprint", matches[0].Key)
	}
	if v := matches[0].TemplateVars["{{changed_status}}"]; v != "in_progress" {
		t.Errorf("TemplateVars[{{changed_status}}] = %q, want in_progress", v)
	}
}

// TestBeadStateTriggerSource_InjectBeadEvent_NonMatchingDropped confirms that
// injected events failing the trigger's event-type filter or condition
// predicate are dropped (produce no match), so the reconciler replay path does
// not over-fire.
func TestBeadStateTriggerSource_InjectBeadEvent_NonMatchingDropped(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	// Wrong event type for the trigger (closed, but trigger wants created).
	src.InjectBeadEvent("beads.bead.closed", &BeadEvent{
		ID: "gd-wrong-evt", Type: "task", Title: "Closed not created",
		Status: "closed",
	})
	// Right event type but failing condition (type=epic, trigger wants task).
	src.InjectBeadEvent("beads.bead.created", &BeadEvent{
		ID: "gd-wrong-type", Type: "epic", Title: "Epic not task",
		Status: "open",
	})

	trig := TriggerDef{
		ID: "t-created-task", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"created"},
			Conditions: []beadCondition{{Field: "type", Op: "==", Value: "task"}},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches (both injected events non-matching), got %d: %+v",
			len(matches), matches)
	}
}

// TestBeadStateTriggerSource_Evaluate_NonBeadSourceData verifies the early
// return when a trigger's SourceData is not beadTriggerData (e.g. a misrouted
// trigger). Evaluate must yield no matches and no error rather than panic.
func TestBeadStateTriggerSource_Evaluate_NonBeadSourceData(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())
	src.InjectBeadEvent("beads.bead.created", &BeadEvent{ID: "gd-1", Type: "task"})

	trig := TriggerDef{ID: "t-bad", Source: "bead", SourceData: "not-bead-data"}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if matches != nil {
		t.Errorf("expected nil matches for non-bead SourceData, got %+v", matches)
	}
}

// TestBeadStateTriggerSource_Evaluate_InSubgraphOf exercises the daemon-backed
// in_subgraph_of operator through Evaluate with a SubgraphResolver wired via
// SetSubgraphResolver. The candidate bead (the event's id, the default field)
// is in the root's connected subgraph, so the trigger fires; a sibling bead
// outside the subgraph is dropped.
func TestBeadStateTriggerSource_Evaluate_InSubgraphOf(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	// Graph: gd-root --parent-child--> gd-child. gd-outsider is unconnected.
	client := &fakeSubgraphClient{
		fwd: map[string][]beadsapi.Dependency{
			"gd-root": {dep("gd-root", "gd-child", "parent-child")},
		},
	}
	src.SetSubgraphResolver(NewSubgraphResolver(client, SubgraphResolverConfig{MaxDepth: 4}))

	src.InjectBeadEvent("beads.bead.updated", &BeadEvent{
		ID: "gd-child", Type: "task", Title: "In subgraph", Status: "open",
	})
	src.InjectBeadEvent("beads.bead.updated", &BeadEvent{
		ID: "gd-outsider", Type: "task", Title: "Out of subgraph", Status: "open",
	})

	trig := TriggerDef{
		ID: "t-subgraph", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"updated"},
			Conditions: []beadCondition{{Field: "id", Op: "in_subgraph_of", Value: "gd-root"}},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match (gd-child in subgraph), got %d: %+v", len(matches), matches)
	}
	if matches[0].Fields["source_bead_id"] != "gd-child" {
		t.Errorf("match source_bead_id = %q, want gd-child", matches[0].Fields["source_bead_id"])
	}
}

// TestBeadStateTriggerSource_Evaluate_NotInSubgraphOf exercises the inverse
// not_in_subgraph_of operator: only the bead OUTSIDE the root's subgraph fires.
func TestBeadStateTriggerSource_Evaluate_NotInSubgraphOf(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	client := &fakeSubgraphClient{
		fwd: map[string][]beadsapi.Dependency{
			"gd-root": {dep("gd-root", "gd-child", "parent-child")},
		},
	}
	src.SetSubgraphResolver(NewSubgraphResolver(client, SubgraphResolverConfig{MaxDepth: 4}))

	src.InjectBeadEvent("beads.bead.updated", &BeadEvent{ID: "gd-child", Type: "task", Status: "open"})
	src.InjectBeadEvent("beads.bead.updated", &BeadEvent{ID: "gd-outsider", Type: "task", Status: "open"})

	trig := TriggerDef{
		ID: "t-not-subgraph", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"updated"},
			Conditions: []beadCondition{{Field: "id", Op: "not_in_subgraph_of", Value: "gd-root"}},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match (gd-outsider not in subgraph), got %d: %+v", len(matches), matches)
	}
	if matches[0].Fields["source_bead_id"] != "gd-outsider" {
		t.Errorf("match source_bead_id = %q, want gd-outsider", matches[0].Fields["source_bead_id"])
	}
}

// TestBeadStateTriggerSource_Evaluate_SubgraphNoResolver confirms that an
// in_subgraph_of condition with no SubgraphResolver configured does not fire
// (treated as no-match) rather than firing on every event.
func TestBeadStateTriggerSource_Evaluate_SubgraphNoResolver(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	src.InjectBeadEvent("beads.bead.updated", &BeadEvent{ID: "gd-1", Type: "task", Status: "open"})

	trig := TriggerDef{
		ID: "t-no-resolver", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"updated"},
			Conditions: []beadCondition{{Field: "id", Op: "in_subgraph_of", Value: "gd-root"}},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches with no resolver configured, got %d", len(matches))
	}
}

// TestBeadStateTriggerSource_Evaluate_SubgraphEmptyRoot confirms that an
// in_subgraph_of condition with an empty Value (no root bead id) does not fire,
// even with a resolver wired.
func TestBeadStateTriggerSource_Evaluate_SubgraphEmptyRoot(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())
	src.SetSubgraphResolver(NewSubgraphResolver(&fakeSubgraphClient{}, SubgraphResolverConfig{}))

	src.InjectBeadEvent("beads.bead.updated", &BeadEvent{ID: "gd-1", Type: "task", Status: "open"})

	trig := TriggerDef{
		ID: "t-empty-root", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"updated"},
			Conditions: []beadCondition{{Field: "id", Op: "in_subgraph_of", Value: ""}},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches for empty subgraph root, got %d", len(matches))
	}
}

// TestBeadStateTriggerSource_Evaluate_SubgraphResolverError confirms that a
// resolver error (e.g. daemon RPC failure) yields no-match — the trigger does
// not fire on a failed lookup. fakeSubgraphClient.fwdErr makes the closure
// expansion fail for the root.
func TestBeadStateTriggerSource_Evaluate_SubgraphResolverError(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())
	src.SetSubgraphResolver(NewSubgraphResolver(
		&fakeSubgraphClient{fwdErr: errSubgraphTest},
		SubgraphResolverConfig{MaxDepth: 4},
	))

	src.InjectBeadEvent("beads.bead.updated", &BeadEvent{ID: "gd-child", Type: "task", Status: "open"})

	trig := TriggerDef{
		ID: "t-resolver-err", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"updated"},
			Conditions: []beadCondition{{Field: "id", Op: "in_subgraph_of", Value: "gd-root"}},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches on resolver error, got %d", len(matches))
	}
}

// TestBeadStateTriggerSource_Evaluate_NotInSubgraphOf_EmptyCandidate covers the
// edge case where the candidate field resolves to empty: not_in_subgraph_of
// treats a bead with no candidate id as "outside" (fires), while in_subgraph_of
// treats it as "inside-unknown" (does not fire). Here we use a custom field
// that is absent from the bead so the candidate is "".
func TestBeadStateTriggerSource_Evaluate_NotInSubgraphOf_EmptyCandidate(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())
	src.SetSubgraphResolver(NewSubgraphResolver(&fakeSubgraphClient{}, SubgraphResolverConfig{}))

	src.InjectBeadEvent("beads.bead.updated", &BeadEvent{ID: "gd-1", Type: "task", Status: "open"})

	trig := TriggerDef{
		ID: "t-empty-candidate", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"updated"},
			Conditions: []beadCondition{
				{Field: "nonexistent_field", Op: "not_in_subgraph_of", Value: "gd-root"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match (empty candidate => not in subgraph), got %d", len(matches))
	}
}

// errSubgraphTest is a sentinel error used to drive the resolver-error branch
// of matchSubgraphCondition.
var errSubgraphTest = errSubgraph("subgraph lookup failed")

type errSubgraph string

func (e errSubgraph) Error() string { return string(e) }

// triggerBead creates a BeadDetail for testing trigger source loading.
//
// Defaults `last_checked` to 1 hour ago so the engine's runtime-bootstrap
// guard (which suppresses first-evaluation firing for brand-new triggers)
// does not kick in. Tests that specifically want to exercise the bootstrap
// path should override `last_checked` to "" or omit it via a custom builder.
func triggerBead(id, title, label string, fields map[string]string) *beadsapi.BeadDetail {
	if _, ok := fields["last_checked"]; !ok {
		if fields == nil {
			fields = map[string]string{}
		}
		fields["last_checked"] = time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339)
	}
	return &beadsapi.BeadDetail{
		ID:     id,
		Title:  title,
		Type:   "trigger",
		Status: "open",
		Labels: []string{label},
		Fields: fields,
	}
}

// triggerBeads converts a variadic list of BeadDetails to a slice (convenience).
func triggerBeads(beads ...*beadsapi.BeadDetail) []*beadsapi.BeadDetail {
	return beads
}

// TestMatchOneCondition_FieldChanged covers the diff-aware "field_changed"
// operator. The operator reads BeadEvent.Changes (populated only on
// BeadUpdated events) and matches when the named field appears in the map.
func TestMatchOneCondition_FieldChanged(t *testing.T) {
	tests := []struct {
		name    string
		changes map[string]any
		field   string
		want    bool
	}{
		{
			name:    "field present in changes",
			changes: map[string]any{"status": "in_progress"},
			field:   "status",
			want:    true,
		},
		{
			name:    "different field changed",
			changes: map[string]any{"title": "new title"},
			field:   "status",
			want:    false,
		},
		{
			name:    "comment-add: no changes payload at all",
			changes: nil,
			field:   "status",
			want:    false,
		},
		{
			name:    "empty changes map",
			changes: map[string]any{},
			field:   "status",
			want:    false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bead := &BeadEvent{ID: "gd-1", Status: "in_progress", Changes: tt.changes}
			c := beadCondition{Field: tt.field, Op: "field_changed"}
			if got := matchOneCondition(c, bead); got != tt.want {
				t.Errorf("matchOneCondition(field_changed %q, changes=%v) = %v, want %v",
					tt.field, tt.changes, got, tt.want)
			}
		})
	}
}

// TestMatchOneCondition_ChangedTo covers the diff-aware "changed_to" operator.
// It must match only when the named field is in Changes AND the new value
// matches the condition value — covering the acceptance criterion that
// status:changed_to:in_progress fires on real transitions but not on
// unrelated BeadUpdated events.
func TestMatchOneCondition_ChangedTo(t *testing.T) {
	tests := []struct {
		name    string
		changes map[string]any
		field   string
		value   string
		want    bool
	}{
		{
			name:    "status transition matches",
			changes: map[string]any{"status": "in_progress"},
			field:   "status", value: "in_progress",
			want: true,
		},
		{
			name:    "status changed to different value",
			changes: map[string]any{"status": "closed"},
			field:   "status", value: "in_progress",
			want: false,
		},
		{
			name:    "field absent from changes (comment-add)",
			changes: map[string]any{"comment_count": float64(3)},
			field:   "status", value: "in_progress",
			want: false,
		},
		{
			name:    "nil Changes (e.g. BeadCreated payload)",
			changes: nil,
			field:   "status", value: "in_progress",
			want: false,
		},
		{
			name:    "numeric new value stringified for compare",
			changes: map[string]any{"priority": float64(2)},
			field:   "priority", value: "2",
			want: true,
		},
		{
			name:    "bool new value stringified for compare",
			changes: map[string]any{"enabled": true},
			field:   "enabled", value: "true",
			want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bead := &BeadEvent{ID: "gd-1", Changes: tt.changes}
			c := beadCondition{Field: tt.field, Op: "changed_to", Value: tt.value}
			if got := matchOneCondition(c, bead); got != tt.want {
				t.Errorf("matchOneCondition(changed_to %q=%q, changes=%v) = %v, want %v",
					tt.field, tt.value, tt.changes, got, tt.want)
			}
		})
	}
}

// TestMatchOneCondition_IsInvoluntaryDeath covers the involuntary-death
// classifier operator (ka-0000002929). The op keys off the close_reason field
// and delegates to gardeniometrics.InvoluntaryDeathFamily — the SAME predicate
// backing gardenio_agent_involuntary_deaths_total — so it fires on EXACTLY the
// population the metric counts. The close_reason strings below are the verbatim
// prod shapes pinned by metrics.TestInvoluntaryDeathFamily; the discriminator is
// the `_involuntary` marker, NOT the pod exit code (always 143).
func TestMatchOneCondition_IsInvoluntaryDeath(t *testing.T) {
	tests := []struct {
		name        string
		field       string // condition Field; "" defaults to close_reason
		wantFamily  string // condition Value: narrow to one family, "" = any
		closeReason string
		want        bool
	}{
		{
			name:        "stop_gate_cooldown involuntary (canonical ka-0000004720 shape) fires",
			closeReason: "stop_gate_cooldown_involuntary: claude exited mid-work without gb done; reopened ka-0000004720",
			want:        true,
		},
		{
			name:        "context_cancelled involuntary (SIGTERM mid-work) fires",
			closeReason: "context_cancelled_involuntary: pod terminated mid-work before gb done",
			want:        true,
		},
		{
			name:        "unclaimed mid-turn kill counted without the marker fires",
			closeReason: "context_cancelled_unclaimed: pod terminated with tool calls but no claimed issue",
			want:        true,
		},
		{
			name:        "claim-lookup-failed mid-turn kill counted without the marker fires",
			closeReason: "context_cancelled_claim_lookup_failed: pod terminated and the claimed-task lookup could not be verified",
			want:        true,
		},
		{
			name:        "novel involuntary shape (family=other) still fires",
			closeReason: "some_new_path_involuntary: died mid-work",
			want:        true,
		},
		{
			name:        "clean stop-gate idle-out (no _involuntary marker) does NOT fire",
			closeReason: "stop_gate_cooldown: claude exited without gb done",
			want:        false,
		},
		{
			name:        "graceful zero-work teardown does NOT fire",
			closeReason: "context_cancelled",
			want:        false,
		},
		{
			name:        "empty close_reason does NOT fire",
			closeReason: "",
			want:        false,
		},
		{
			name:        "value narrows to a matching family",
			wantFamily:  "stop_gate_cooldown",
			closeReason: "stop_gate_cooldown_involuntary: reopened gd-x",
			want:        true,
		},
		{
			name:        "value narrows away a non-matching family",
			wantFamily:  "context_cancelled",
			closeReason: "stop_gate_cooldown_involuntary: reopened gd-x",
			want:        false,
		},
		{
			name:        "explicit close_reason field name works",
			field:       "close_reason",
			closeReason: "stop_gate_escape_hatch_involuntary: claude exited mid-work",
			want:        true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bead := &BeadEvent{ID: "gd-1", Fields: map[string]string{"close_reason": tt.closeReason}}
			c := beadCondition{Field: tt.field, Op: "is_involuntary_death", Value: tt.wantFamily}
			if got := matchOneCondition(c, bead); got != tt.want {
				t.Errorf("matchOneCondition(is_involuntary_death field=%q value=%q, close_reason=%q) = %v, want %v",
					tt.field, tt.wantFamily, tt.closeReason, got, tt.want)
			}
		})
	}
}

// TestBeadStateTriggerSource_InvoluntaryDeathFiresOnAgentClose wires the source
// end-to-end for the ka-0000002929 acceptance scenario: an agent bead closing
// with an involuntary close_reason fires the trigger (and surfaces a
// {{death_family}} template var), while a cleanly-stopped sibling does not — so
// a forensics gardener keyed on `is_involuntary_death` spawns on exactly the
// deaths the metric counter records, off close_reason, no new instrumentation.
func TestBeadStateTriggerSource_InvoluntaryDeathFiresOnAgentClose(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	src.mu.Lock()
	src.pending = []beadStateEvent{
		{
			Topic: "beads.bead.closed",
			Bead: &BeadEvent{
				ID: "gd-dead", Type: "agent", Title: "crew that died",
				Status: "closed",
				Fields: map[string]string{
					"close_reason": "stop_gate_cooldown_involuntary: claude exited mid-work; reopened gd-orphan",
					"project":      "gardenio",
					"role":         "crew",
				},
			},
		},
		{
			Topic: "beads.bead.closed",
			Bead: &BeadEvent{
				ID: "gd-clean", Type: "agent", Title: "crew that stopped cleanly",
				Status: "closed",
				Fields: map[string]string{
					"close_reason": "done: gb stop after work complete",
					"project":      "gardenio",
					"role":         "crew",
				},
			},
		},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "trig-forensics",
		SourceData: beadTriggerData{
			EventTypes: []string{"closed"},
			Conditions: []beadCondition{
				{Field: "type", Op: "==", Value: "agent"},
				{Field: "close_reason", Op: "is_involuntary_death"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 involuntary-death match, got %d (%+v)", len(matches), matches)
	}
	got := matches[0]
	if got.Fields["source_bead_id"] != "gd-dead" {
		t.Errorf("match source_bead_id = %q, want gd-dead", got.Fields["source_bead_id"])
	}
	// The classified family is surfaced so the forensics prompt can name the
	// death shape without re-parsing close_reason.
	if fam := got.TemplateVars["{{death_family}}"]; fam != "stop_gate_cooldown" {
		t.Errorf("{{death_family}} = %q, want stop_gate_cooldown", fam)
	}
}

// TestBeadStateTriggerSource_ChangedToFiresOncePerTransition wires the trigger
// source end-to-end for the acceptance scenario in gd-2WWNEo4bIR:
//
//		condition: {field:status, op:changed_to, value:in_progress}
//
//	  (a) BeadUpdated carrying a status→in_progress change fires the trigger.
//	  (b) BeadUpdated for a comment-add (no status in Changes) does NOT fire.
//	  (c) A subsequent BeadUpdated with a different status transition fires
//	      again — the per-transition dedup fingerprint prevents an open→
//	      in_progress→closed sequence from collapsing on a single key.
func TestBeadStateTriggerSource_ChangedToFiresOncePerTransition(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	// Buffer three updated events for the same bead:
	//   1. status: open -> in_progress  (should fire)
	//   2. comment_count: 1 -> 2        (no status change; must NOT fire)
	//   3. status: in_progress -> closed (different transition; the
	//      changed_to:in_progress trigger should still not fire for this)
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{
			Topic: "beads.bead.updated",
			Bead: &BeadEvent{
				ID: "gd-1", Type: "task", Title: "T",
				Status:  "in_progress",
				Changes: map[string]any{"status": "in_progress"},
			},
		},
		{
			Topic: "beads.bead.updated",
			Bead: &BeadEvent{
				ID: "gd-1", Type: "task", Title: "T",
				Status:  "in_progress",
				Changes: map[string]any{"comment_count": float64(2)},
			},
		},
		{
			Topic: "beads.bead.updated",
			Bead: &BeadEvent{
				ID: "gd-1", Type: "task", Title: "T",
				Status:  "closed",
				Changes: map[string]any{"status": "closed"},
			},
		},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "trig-1",
		SourceData: beadTriggerData{
			EventTypes: []string{"updated"},
			Conditions: []beadCondition{
				{Field: "status", Op: "changed_to", Value: "in_progress"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	// Acceptance criterion: only the open→in_progress event should match.
	if len(matches) != 1 {
		t.Fatalf("expected exactly 1 match for status:changed_to:in_progress, got %d (%+v)",
			len(matches), matches)
	}
	got := matches[0]
	if got.Fields["source_bead_id"] != "gd-1" {
		t.Errorf("match source_bead_id = %q, want gd-1", got.Fields["source_bead_id"])
	}
	// Dedup key must include the transition fingerprint so that other
	// status transitions on the same bead can fire later without colliding.
	if !strings.Contains(got.Key, "status=in_progress") {
		t.Errorf("match.Key = %q does not embed the transition fingerprint", got.Key)
	}
	// Template var exposes the new value so the gardener prompt can reference
	// it without a separate bead-fetch.
	if v := got.TemplateVars["{{changed_status}}"]; v != "in_progress" {
		t.Errorf("TemplateVars[{{changed_status}}] = %q, want in_progress", v)
	}
}

// TestBeadStateTriggerSource_FieldChangedSuppressesCommentAdd asserts the
// second acceptance criterion: a BeadUpdated event whose Changes map does
// not carry the trigger's field of interest must not fire the trigger.
func TestBeadStateTriggerSource_FieldChangedSuppressesCommentAdd(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	src.mu.Lock()
	src.pending = []beadStateEvent{
		{
			Topic: "beads.bead.updated",
			Bead: &BeadEvent{
				ID: "gd-1", Type: "task", Status: "open",
				// Comment-add carries no field rotation, only metadata.
				Changes: map[string]any{"comment_count": float64(1)},
			},
		},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "trig-1",
		SourceData: beadTriggerData{
			EventTypes: []string{"updated"},
			Conditions: []beadCondition{
				{Field: "status", Op: "field_changed"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 matches for field_changed:status on a comment-add event, got %d: %+v",
			len(matches), matches)
	}
}

// TestChangesFingerprintDistinct verifies that distinct field transitions
// produce distinct dedup keys. This is what lets open→in_progress and
// in_progress→closed both fire (each is a different transition); without it
// the engine's per-trigger dedup would drop the second event because the
// first updated event for the bead already saturated the "gd-1:updated" key.
func TestChangesFingerprintDistinct(t *testing.T) {
	a := changesFingerprint(map[string]any{"status": "in_progress"})
	b := changesFingerprint(map[string]any{"status": "closed"})
	if a == "" || b == "" {
		t.Fatalf("fingerprints unexpectedly empty: a=%q b=%q", a, b)
	}
	if a == b {
		t.Errorf("expected distinct fingerprints, got both = %q", a)
	}
	// Determinism: same input → same output.
	if got := changesFingerprint(map[string]any{"status": "in_progress"}); got != a {
		t.Errorf("fingerprint non-deterministic: %q != %q", got, a)
	}
	// Multi-key ordering is stable.
	x := changesFingerprint(map[string]any{"a": "1", "b": "2"})
	y := changesFingerprint(map[string]any{"b": "2", "a": "1"})
	if x != y {
		t.Errorf("fingerprint not order-stable: %q vs %q", x, y)
	}
	// Empty map fingerprint is empty.
	if got := changesFingerprint(nil); got != "" {
		t.Errorf("nil Changes fingerprint = %q, want empty string", got)
	}
}

// TestStringifyChangeValue covers the JSON-decoded coercion table that
// changed_to relies on for comparing condition Value strings to non-string
// new values in the Changes payload.
func TestStringifyChangeValue(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"string", "in_progress", "in_progress"},
		{"nil-as-clear", nil, ""},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"int float64", float64(2), "2"},
		{"non-int float64", 1.5, "1.5"},
		{"json.Number", json.Number("42"), "42"},
		{"unknown type", []int{1, 2}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stringifyChangeValue(tt.in); got != tt.want {
				t.Errorf("stringifyChangeValue(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestBeadStateTriggerSource_DecisionAnswerMatch verifies that a trigger:bead
// watcher subscribed to beads.bead.closed for type=decision can match on the
// `chosen` field that GAP-4 (ka-0000005470) propagates through the SSE
// pipeline. Without DecisionAnswer mirroring in ParseBeadEvent, the condition
// {field:"chosen",op:"==",value:"yes"} would have no field to match against.
func TestBeadStateTriggerSource_DecisionAnswerMatch(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	// Simulate what ParseBeadEvent produces for a resolved decision bead:
	// Fields mirrors the DecisionAnswer payload so condition DSL just works.
	src.mu.Lock()
	src.pending = []beadStateEvent{
		{
			Topic: "beads.bead.closed",
			Bead: &BeadEvent{
				ID: "gd-dec-1", Type: "decision", Title: "Ship it?",
				Status: "closed", ClosedBy: "alice@example.com",
				Fields: map[string]string{
					"chosen":        "yes",
					"response_text": "Looks good",
					"responded_by":  "alice@example.com",
					"responded_at":  "2026-05-27T10:00:00Z",
				},
				DecisionAnswer: &DecisionAnswer{
					Chosen:       "yes",
					ResponseText: "Looks good",
					RespondedBy:  "alice@example.com",
					RespondedAt:  "2026-05-27T10:00:00Z",
				},
			},
		},
		{
			Topic: "beads.bead.closed",
			Bead: &BeadEvent{
				ID: "gd-dec-2", Type: "decision", Title: "Other decision",
				Status: "closed", ClosedBy: "bob@example.com",
				Fields: map[string]string{
					"chosen":       "no",
					"responded_by": "bob@example.com",
				},
				DecisionAnswer: &DecisionAnswer{
					Chosen:      "no",
					RespondedBy: "bob@example.com",
				},
			},
		},
		{
			Topic: "beads.bead.closed",
			Bead: &BeadEvent{
				ID: "gd-task-1", Type: "task", Title: "A closed task",
				Status: "closed", ClosedBy: "alice@example.com",
			},
		},
	}
	src.mu.Unlock()

	// Trigger: wake when a decision is resolved with chosen=="yes".
	trig := TriggerDef{
		ID: "t-decision-yes", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"closed"},
			Conditions: []beadCondition{
				{Field: "type", Op: "==", Value: "decision"},
				{Field: "chosen", Op: "==", Value: "yes"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("expected 1 match (decision chosen=yes), got %d", len(matches))
	}
	if matches[0].Key != "gd-dec-1:closed" {
		t.Errorf("match key = %q, want %q", matches[0].Key, "gd-dec-1:closed")
	}
	if got := matches[0].TemplateVars["{{chosen}}"]; got != "yes" {
		t.Errorf("template var {{chosen}} = %q, want %q", got, "yes")
	}
	if got := matches[0].TemplateVars["{{responded_by}}"]; got != "alice@example.com" {
		t.Errorf("template var {{responded_by}} = %q, want %q", got, "alice@example.com")
	}
}

// TestBeadStateTriggerSource_DecisionAnswer_InOperator verifies the `in`
// operator works against the chosen field for multi-option triggers.
func TestBeadStateTriggerSource_DecisionAnswer_InOperator(t *testing.T) {
	src := NewBeadStateTriggerSource(slog.Default())

	src.mu.Lock()
	src.pending = []beadStateEvent{
		{
			Topic: "beads.bead.closed",
			Bead: &BeadEvent{
				ID: "gd-dec-1", Type: "decision", Status: "closed",
				Fields: map[string]string{"chosen": "approve"},
			},
		},
		{
			Topic: "beads.bead.closed",
			Bead: &BeadEvent{
				ID: "gd-dec-2", Type: "decision", Status: "closed",
				Fields: map[string]string{"chosen": "merge"},
			},
		},
		{
			Topic: "beads.bead.closed",
			Bead: &BeadEvent{
				ID: "gd-dec-3", Type: "decision", Status: "closed",
				Fields: map[string]string{"chosen": "reject"},
			},
		},
	}
	src.mu.Unlock()

	trig := TriggerDef{
		ID: "t-decision-go", Source: "bead", Action: TriggerActionNotify, MaxConcurrent: 10,
		SourceData: beadTriggerData{
			EventTypes: []string{"closed"},
			Conditions: []beadCondition{
				{Field: "type", Op: "==", Value: "decision"},
				{Field: "chosen", Op: "in", Value: "approve,merge,ship"},
			},
		},
	}

	matches, err := src.Evaluate(context.Background(), trig)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(matches) != 2 {
		t.Fatalf("expected 2 matches (approve, merge), got %d", len(matches))
	}
}

// TestParseBeadEvent_DecisionAnswerMirrored verifies that ParseBeadEvent
// mirrors the wrapper's decision_answer into the BeadEvent.Fields map so
// the generic condition DSL (which keys off Fields) sees the answer.
func TestParseBeadEvent_DecisionAnswerMirrored(t *testing.T) {
	payload := []byte(`{
		"bead": {
			"id": "gd-dec-1",
			"type": "decision",
			"title": "Ship?",
			"status": "closed",
			"fields": {"prompt": "Ship it?", "responded_at": "2026-05-27T10:00:00Z"}
		},
		"closed_by": "alice@example.com",
		"decision_answer": {
			"chosen": "yes",
			"response_text": "Looks good",
			"responded_by": "alice@example.com",
			"responded_at": "2026-05-27T10:00:00Z"
		}
	}`)

	bead := ParseBeadEvent(payload)
	if bead == nil {
		t.Fatal("ParseBeadEvent returned nil for valid decision-closed payload")
	}
	if bead.Type != "decision" || bead.ID != "gd-dec-1" {
		t.Errorf("bead identity wrong: type=%q id=%q", bead.Type, bead.ID)
	}
	if bead.ClosedBy != "alice@example.com" {
		t.Errorf("ClosedBy = %q, want %q", bead.ClosedBy, "alice@example.com")
	}
	if bead.DecisionAnswer == nil {
		t.Fatal("DecisionAnswer not parsed from wrapper")
	}
	if bead.DecisionAnswer.Chosen != "yes" {
		t.Errorf("DecisionAnswer.Chosen = %q, want %q", bead.DecisionAnswer.Chosen, "yes")
	}

	// Fields mirror — this is the path the trigger condition DSL takes.
	wantFields := map[string]string{
		"chosen":        "yes",
		"response_text": "Looks good",
		"responded_by":  "alice@example.com",
		"responded_at":  "2026-05-27T10:00:00Z",
	}
	for k, want := range wantFields {
		if got := bead.Fields[k]; got != want {
			t.Errorf("Fields[%q] = %q, want %q", k, got, want)
		}
	}
}

// TestParseBeadEvent_NoDecisionAnswerForRegularClose verifies that a plain
// non-decision close (no decision_answer wrapper field) does not synthesize
// spurious Fields entries.
func TestParseBeadEvent_NoDecisionAnswerForRegularClose(t *testing.T) {
	payload := []byte(`{
		"bead": {
			"id": "gd-task-1",
			"type": "task",
			"title": "A task",
			"status": "closed"
		},
		"closed_by": "bot"
	}`)

	bead := ParseBeadEvent(payload)
	if bead == nil {
		t.Fatal("ParseBeadEvent returned nil for valid task-closed payload")
	}
	if bead.DecisionAnswer != nil {
		t.Errorf("DecisionAnswer should be nil for non-decision close, got %+v", bead.DecisionAnswer)
	}
	if _, ok := bead.Fields["chosen"]; ok {
		t.Errorf("Fields[chosen] should not be set for non-decision close")
	}
}

// TestMatchTimeRelativeConditions covers the older_than / newer_than freshness
// operators — the scriptable-trigger JSON-DSL stopgap (decision ka-0000001680 /
// F4a gd-qTfm3EWqfv). The clock is pinned via timeNow so the boundary is exact.
func TestMatchTimeRelativeConditions(t *testing.T) {
	fixedNow := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	orig := timeNow
	timeNow = func() time.Time { return fixedNow }
	defer func() { timeNow = orig }()

	// last_checked 3h before "now": stale relative to 2h, fresh relative to 4h.
	threeHoursAgo := fixedNow.Add(-3 * time.Hour).Format(time.RFC3339)
	// last_checked 30m before "now".
	thirtyMinAgo := fixedNow.Add(-30 * time.Minute).Format(time.RFC3339)

	bead := &BeadEvent{
		ID: "gd-cap", Type: "trigger", Status: "open",
		Fields: map[string]string{
			"last_checked": threeHoursAgo,
			"recent":       thirtyMinAgo,
			"human_ts":     fixedNow.Add(-3 * time.Hour).Format("2006-01-02 15:04:05"),
			"garbage":      "not-a-timestamp",
		},
	}

	tests := []struct {
		name string
		cond beadCondition
		want bool
	}{
		// older_than == STALE
		{"older_than: stale field is older than 2h",
			beadCondition{Field: "last_checked", Op: "older_than", Value: "2h"}, true},
		{"older_than: field is NOT older than 4h",
			beadCondition{Field: "last_checked", Op: "older_than", Value: "4h"}, false},
		{"older_than: fresh field is not older than 2h",
			beadCondition{Field: "recent", Op: "older_than", Value: "2h"}, false},
		// newer_than == FRESH (inverse of older_than)
		{"newer_than: fresh field is newer than 2h",
			beadCondition{Field: "recent", Op: "newer_than", Value: "2h"}, true},
		{"newer_than: stale field is NOT newer than 2h",
			beadCondition{Field: "last_checked", Op: "newer_than", Value: "2h"}, false},
		{"newer_than: stale field IS newer than 4h",
			beadCondition{Field: "last_checked", Op: "newer_than", Value: "4h"}, true},
		// Boundary: age == duration. older_than uses >= so it fires; newer_than uses < so it does not.
		{"older_than: boundary age==dur fires (>=)",
			beadCondition{Field: "last_checked", Op: "older_than", Value: "3h"}, true},
		{"newer_than: boundary age==dur does not fire (<)",
			beadCondition{Field: "last_checked", Op: "newer_than", Value: "3h"}, false},
		// human-readable timestamp form is tolerated
		{"older_than: human-form timestamp parses",
			beadCondition{Field: "human_ts", Op: "older_than", Value: "2h"}, true},
		// Fail-closed cases — all must be false.
		{"older_than: unparseable timestamp -> false",
			beadCondition{Field: "garbage", Op: "older_than", Value: "2h"}, false},
		{"newer_than: unparseable timestamp -> false",
			beadCondition{Field: "garbage", Op: "newer_than", Value: "2h"}, false},
		{"older_than: missing field -> false",
			beadCondition{Field: "nonexistent", Op: "older_than", Value: "2h"}, false},
		{"older_than: bad duration -> false",
			beadCondition{Field: "last_checked", Op: "older_than", Value: "notaduration"}, false},
		{"older_than: empty duration -> false",
			beadCondition{Field: "last_checked", Op: "older_than", Value: ""}, false},
		{"older_than: negative duration -> false",
			beadCondition{Field: "last_checked", Op: "older_than", Value: "-2h"}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchOneCondition(tt.cond, bead); got != tt.want {
				t.Errorf("matchOneCondition(%+v) = %v, want %v", tt.cond, got, tt.want)
			}
		})
	}
}

// TestMatchTimeRelativeConditions_AndComposition confirms the new operators
// compose with the existing AND-only conjunction — the exact fleet-liveness
// shape (status==open AND enabled==true AND last_checked older_than 2h).
func TestMatchTimeRelativeConditions_AndComposition(t *testing.T) {
	fixedNow := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	orig := timeNow
	timeNow = func() time.Time { return fixedNow }
	defer func() { timeNow = orig }()

	staleBead := &BeadEvent{
		ID: "gd-dark", Type: "trigger", Status: "open",
		Fields: map[string]string{
			"enabled":      "true",
			"last_checked": fixedNow.Add(-3 * time.Hour).Format(time.RFC3339),
		},
	}
	freshBead := &BeadEvent{
		ID: "gd-live", Type: "trigger", Status: "open",
		Fields: map[string]string{
			"enabled":      "true",
			"last_checked": fixedNow.Add(-10 * time.Minute).Format(time.RFC3339),
		},
	}
	conds := []beadCondition{
		{Field: "status", Op: "==", Value: "open"},
		{Field: "enabled", Op: "==", Value: "true"},
		{Field: "last_checked", Op: "older_than", Value: "2h"},
	}
	if !matchConditions(conds, staleBead) {
		t.Error("stale gardener should match the liveness predicate (all three AND clauses true)")
	}
	if matchConditions(conds, freshBead) {
		t.Error("fresh gardener should NOT match (last_checked is recent)")
	}
}

// TestBeadStateTriggerSource_CooldownEventRace guards the bead source's
// cooldown-bypass (ka-0000001376). A bead event that arrives while the
// trigger's last_checked is inside the 30s cooldown floor must still be
// consumed THIS cycle rather than cooldown-skipped-then-drained-and-lost — the
// exact silent event loss that flaked the trigger-action-fidelity E2E when a
// live staging trigger-engine stamped last_checked while racing the probe's
// EvaluateSource. The fix mirrors MultiTriggerSource.EventReady (bug
// ka-0000005988); this is its bead-source twin.
func TestBeadStateTriggerSource_CooldownEventRace(t *testing.T) {
	newEngine := func(src *BeadStateTriggerSource) (*TriggerEngine, *int) {
		daemon := &mockTriggerDaemon{}
		engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
		engine.RegisterSource(src)
		fired := 0
		engine.notifier = func(_ context.Context, _, _ string, _ string) { fired++ }
		return engine, &fired
	}

	// midCooldownTrigger: last_checked 1 minute ago with a 20-minute cooldown →
	// squarely inside the cooldown window. Matches created tasks only.
	midCooldownTrigger := func() TriggerDef {
		return TriggerDef{
			ID:              "gd-bead-cooldown-race",
			Title:           "bead cooldown race",
			Source:          "bead",
			Action:          TriggerActionNotify,
			MaxConcurrent:   5,
			CooldownMinutes: 20,
			LastChecked:     time.Now().Add(-1 * time.Minute),
			SourceData: beadTriggerData{
				EventTypes: []string{"created"},
				Conditions: []beadCondition{{Field: "type", Op: "==", Value: "task"}},
			},
		}
	}

	t.Run("matching event mid-cooldown is consumed, not drained", func(t *testing.T) {
		src := NewBeadStateTriggerSource(slog.Default())
		engine, fired := newEngine(src)

		src.InjectBeadEvent("beads.bead.created", &BeadEvent{
			ID: "gd-src-1", Type: "task", Title: "mid-cooldown task",
			Status: "open", Labels: []string{"project:gardenio"},
		})

		trig := midCooldownTrigger()
		if time.Since(trig.LastChecked) >= effectiveCooldown(trig) {
			t.Fatalf("test setup: trigger is not in cooldown window")
		}

		// EventReady must see the matching pending event so the cooldown gate
		// bypasses.
		if !src.EventReady(context.Background(), trig) {
			t.Fatalf("EventReady = false; a matching pending event must report ready")
		}

		engine.evalOne(context.Background(), src, trig)

		if *fired != 1 {
			t.Errorf("expected the mid-cooldown event to fire exactly once, fired %d "+
				"(without the cooldown/event-race fix the event is cooldown-skipped and dropped)", *fired)
		}
		if !engine.SeenKeys(trig.ID)["gd-src-1:created"] {
			t.Errorf("expected the consumed event to be recorded in the seen set, got %v",
				engine.SeenKeys(trig.ID))
		}
	})

	t.Run("no pending event mid-cooldown still honors cooldown", func(t *testing.T) {
		src := NewBeadStateTriggerSource(slog.Default())
		engine, fired := newEngine(src)

		trig := midCooldownTrigger()
		if src.EventReady(context.Background(), trig) {
			t.Fatalf("EventReady = true with no pending event")
		}

		engine.evalOne(context.Background(), src, trig)

		if *fired != 0 {
			t.Errorf("with no pending event, cooldown must suppress re-eval, fired %d", *fired)
		}
	})

	t.Run("non-matching event mid-cooldown does not bypass cooldown", func(t *testing.T) {
		src := NewBeadStateTriggerSource(slog.Default())
		engine, fired := newEngine(src)

		// An epic-created event does NOT match the "type == task" condition, so
		// it must neither report ready nor fire.
		src.InjectBeadEvent("beads.bead.created", &BeadEvent{
			ID: "gd-epic-1", Type: "epic", Title: "not a task",
			Status: "open", Labels: []string{"project:gardenio"},
		})

		trig := midCooldownTrigger()
		if src.EventReady(context.Background(), trig) {
			t.Fatalf("EventReady = true for a non-matching pending event")
		}

		engine.evalOne(context.Background(), src, trig)

		if *fired != 0 {
			t.Errorf("a non-matching mid-cooldown event must not fire, fired %d", *fired)
		}
	})
}

// TestConditionsReferenceLabel covers the predicate that decides whether the
// event_types/label-condition over-constraint guardrail applies (ka-0000002382).
func TestConditionsReferenceLabel(t *testing.T) {
	tests := []struct {
		name       string
		conditions []beadCondition
		want       bool
	}{
		{"no conditions", nil, false},
		{
			name:       "plain field condition, no label",
			conditions: []beadCondition{{Field: "type", Op: "==", Value: "epic"}},
			want:       false,
		},
		{
			name:       "label present condition",
			conditions: []beadCondition{{Field: "label:fsm-verify-passed", Op: "present"}},
			want:       true,
		},
		{
			name:       "label absent condition",
			conditions: []beadCondition{{Field: "label:fsm-published", Op: "absent"}},
			want:       true,
		},
		{
			name: "label mixed with plain fields (the MR-D shape)",
			conditions: []beadCondition{
				{Field: "label:fsm-verify-passed", Op: "present"},
				{Field: "label:fsm-published", Op: "absent"},
				{Field: "type", Op: "==", Value: "epic"},
			},
			want: true,
		},
		{
			name:       "CEL expression referencing labels is excluded",
			conditions: []beadCondition{{Expr: `bead.labels.exists(l, l == "x")`}},
			want:       false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := conditionsReferenceLabel(tt.conditions); got != tt.want {
				t.Errorf("conditionsReferenceLabel(%+v) = %v, want %v", tt.conditions, got, tt.want)
			}
		})
	}
}

// TestLoadTriggers_LabelConditionEventTypeGapWarn is the regression guard for
// the MR-D publish-fix dark-trigger stall (ka-0000002382): a trigger gated on a
// `label:` condition but with event_types restricted to `updated` can never be
// woken by the arming label-add (the daemon delivers a label change ONLY as the
// beads.label.added edge topic), so LoadTriggers must WARN loudly. Triggers
// that admit label.added, or leave event_types empty (match-all), stay quiet.
func TestLoadTriggers_LabelConditionEventTypeGapWarn(t *testing.T) {
	const labelConds = `[{"field":"label:fsm-verify-passed","op":"present"},{"field":"label:fsm-published","op":"absent"},{"field":"type","op":"==","value":"epic"}]`

	tests := []struct {
		name     string
		fields   map[string]string
		wantWarn bool
	}{
		{
			name: "over-constrained: label condition with event_types=updated (the bug)",
			fields: map[string]string{
				"action":      "apply_formula",
				"event_types": "updated",
				"conditions":  labelConds,
			},
			wantWarn: true,
		},
		{
			name: "fixed: label.added added to event_types",
			fields: map[string]string{
				"action":      "apply_formula",
				"event_types": "updated,label.added",
				"conditions":  labelConds,
			},
			wantWarn: false,
		},
		{
			name: "match-all: empty event_types admits label.added",
			fields: map[string]string{
				"action":     "apply_formula",
				"conditions": labelConds,
			},
			wantWarn: false,
		},
		{
			name: "no label condition: plain field gate on updated is fine",
			fields: map[string]string{
				"action":      "notify",
				"event_types": "updated",
				"conditions":  `[{"field":"type","op":"==","value":"epic"}]`,
			},
			wantWarn: false,
		},
		{
			name: "full topic form beads.label.added also covers the gap",
			fields: map[string]string{
				"action":      "apply_formula",
				"event_types": "closed,beads.label.added",
				"conditions":  labelConds,
			},
			wantWarn: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			src := NewBeadStateTriggerSource(captureLogger(&buf))
			daemon := &mockTriggerDaemon{
				beads: triggerBeads(triggerBead("tg", "gap trigger", "trigger:bead", tt.fields)),
			}
			if _, err := src.LoadTriggers(context.Background(), daemon); err != nil {
				t.Fatalf("LoadTriggers: %v", err)
			}
			gotWarn := strings.Contains(buf.String(), "can never be woken by its event_types")
			if gotWarn != tt.wantWarn {
				t.Errorf("warn emitted = %v, want %v (log: %s)", gotWarn, tt.wantWarn, buf.String())
			}
		})
	}
}

// TestLoadTriggers_LabelConditionGapWarnDedup verifies the WARN is one-shot per
// (trigger, event_types): a steady reload loop must not re-log it every cycle.
func TestLoadTriggers_LabelConditionGapWarnDedup(t *testing.T) {
	var buf bytes.Buffer
	src := NewBeadStateTriggerSource(captureLogger(&buf))
	daemon := &mockTriggerDaemon{
		beads: triggerBeads(triggerBead("tg", "gap trigger", "trigger:bead", map[string]string{
			"action":      "apply_formula",
			"event_types": "updated",
			"conditions":  `[{"field":"label:fsm-verify-passed","op":"present"}]`,
		})),
	}

	for i := range 3 {
		if _, err := src.LoadTriggers(context.Background(), daemon); err != nil {
			t.Fatalf("LoadTriggers pass %d: %v", i, err)
		}
	}

	if n := strings.Count(buf.String(), "can never be woken by its event_types"); n != 1 {
		t.Errorf("expected exactly 1 gap WARN across 3 reloads, got %d (log: %s)", n, buf.String())
	}
}
