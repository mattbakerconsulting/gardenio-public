package main

import (
	"context"
	"testing"
)

// Phantom-prevention guard (ka-0000008103): enabling a fire-shaped trigger that
// carries NO `trigger:*` source label used to leave a SILENT PHANTOM — the bead
// reads enabled=true on every dashboard while the engine (which loads triggers
// BY source label) never evaluates it. setTriggerEnabled now stamps the computed
// source label at the same enable chokepoint. These tests pin that behavior and
// its exemptions, mirroring gardener-autoblock-audit.py is_phantom().

// enableAndLabels enables beadID through the stub and returns the addlabel ops
// recorded for it plus the bead's resulting label set.
func enableAndLabels(t *testing.T, stub *gardenerStub, beadID string) ([]string, []string) {
	t.Helper()
	setupGardenerStub(t, stub)
	if err := setTriggerEnabled(context.Background(), beadID, true, triggerEnableOptions{}); err != nil {
		t.Fatalf("enable %s: %v", beadID, err)
	}
	var added []string
	stub.mu.Lock()
	for _, o := range stub.ops {
		if o.id == beadID && o.op == "addlabel" {
			added = append(added, o.label)
		}
	}
	labels := labelSlice(stub.beads[beadID]["labels"])
	stub.mu.Unlock()
	return added, labels
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}

func TestTriggerEnable_CronNoSourceLabel_AddsSchedule(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(map[string]any{
		"id":     "gd-PHAN1",
		"type":   "trigger",
		"status": "open",
		"labels": []any{"project:gardenio"}, // no trigger:* label
		"fields": map[string]any{"cron": "0 12 * * *", "enabled": "false"},
	})

	added, labels := enableAndLabels(t, stub, "gd-PHAN1")
	if !hasLabel(added, "trigger:schedule") {
		t.Fatalf("expected trigger:schedule to be added, got adds=%v", added)
	}
	if !hasLabel(labels, "trigger:schedule") {
		t.Fatalf("bead should carry trigger:schedule after enable, got %v", labels)
	}
}

func TestTriggerEnable_NonEmptyTriggersNoSourceLabel_AddsMulti(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(map[string]any{
		"id":     "gd-PHAN2",
		"type":   "trigger",
		"status": "open",
		"labels": []any{"project:gardenio"},
		"fields": map[string]any{
			// A non-empty triggers[] with no flat cron — the multi shape.
			"triggers": `[{"cron":"0 9 * * *"}]`,
			"enabled":  "false",
		},
	})

	added, labels := enableAndLabels(t, stub, "gd-PHAN2")
	if !hasLabel(added, "trigger:multi") {
		t.Fatalf("expected trigger:multi to be added, got adds=%v", added)
	}
	if hasLabel(added, "trigger:schedule") {
		t.Fatalf("a non-empty triggers[] must get trigger:multi, not trigger:schedule: %v", added)
	}
	if !hasLabel(labels, "trigger:multi") {
		t.Fatalf("bead should carry trigger:multi after enable, got %v", labels)
	}
}

func TestTriggerEnable_AlreadyHasSourceLabel_NoOp(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(map[string]any{
		"id":     "gd-OK1",
		"type":   "trigger",
		"status": "open",
		"labels": []any{"trigger:schedule", "project:gardenio"},
		"fields": map[string]any{"cron": "0 12 * * *", "enabled": "false"},
	})

	added, _ := enableAndLabels(t, stub, "gd-OK1")
	if len(added) != 0 {
		t.Fatalf("a trigger that already has a source label must get no addlabel, got %v", added)
	}
}

func TestTriggerEnable_E2EFixtureNoSourceLabel_Exempt(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(map[string]any{
		"id":     "gd-FIX1",
		"type":   "trigger",
		"status": "open",
		// isE2EFixture beads are seeded enabled without a source label BY DESIGN —
		// the engine deliberately never fires them, so we must not stamp one.
		"labels": []any{"fixture:e2e:demo", "project:gardenio"},
		"fields": map[string]any{"cron": "0 12 * * *", "enabled": "false"},
	})

	added, labels := enableAndLabels(t, stub, "gd-FIX1")
	if len(added) != 0 {
		t.Fatalf("an E2E fixture must stay exempt (no source label added), got %v", added)
	}
	for _, l := range labels {
		if len(l) >= len("trigger:") && l[:len("trigger:")] == "trigger:" {
			t.Fatalf("E2E fixture must not gain a trigger:* label, got %v", labels)
		}
	}
}

func TestTriggerEnable_NoFireShape_NoLabelAdded(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(map[string]any{
		"id":     "gd-NOFIRE",
		"type":   "trigger",
		"status": "open",
		"labels": []any{"project:gardenio"},
		// No cron, empty triggers[] — not a phantom; nothing to stamp.
		"fields": map[string]any{"enabled": "false"},
	})

	added, _ := enableAndLabels(t, stub, "gd-NOFIRE")
	if len(added) != 0 {
		t.Fatalf("a bead with no fire shape must not gain a source label, got %v", added)
	}
}

func TestTriggerDisable_NeverAddsSourceLabel(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(map[string]any{
		"id":     "gd-DIS1",
		"type":   "trigger",
		"status": "open",
		"labels": []any{"project:gardenio"},
		"fields": map[string]any{"cron": "0 12 * * *", "enabled": "true"},
	})
	setupGardenerStub(t, stub)

	if err := setTriggerEnabled(context.Background(), "gd-DIS1", false, triggerEnableOptions{}); err != nil {
		t.Fatalf("disable: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	for _, o := range stub.ops {
		if o.id == "gd-DIS1" && o.op == "addlabel" {
			t.Fatalf("disable must never add a source label, got %v", o.label)
		}
	}
}
