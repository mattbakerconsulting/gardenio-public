package main

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gardenio/controller/internal/gardenermode"
	"gardenio/controller/internal/scheduler"
)

// TestGardenerMode_InspectPrintsModeAndCadence covers the no-flag inspect path:
// a timed gardener reports `mode: timed  cron: <cron>` and exits 0 without any
// writes.
func TestGardenerMode_InspectPrintsModeAndCadence(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{
		"cron":     "7,37 * * * *",
		"timezone": "America/Los_Angeles",
	}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerModeCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "mode: timed") {
		t.Errorf("inspect output missing mode: timed\n%s", out)
	}
	if !strings.Contains(out, "cron: 7,37 * * * *") {
		t.Errorf("inspect output missing cron cadence\n%s", out)
	}
	// Inspect must not write.
	if ops := stub.opsOf("gd-CAP1"); len(ops) != 0 {
		t.Errorf("inspect wrote %v, want no writes", ops)
	}
}

// TestGardenerMode_InspectEventCadenceFromFallback verifies that an already
// event-mode gardener reports mode: event with the fallback watchdog cron as
// the effective cadence (the flat cron field is cleared in event mode).
func TestGardenerMode_InspectEventCadenceFromFallback(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	trig := makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{
		"cron":     "",
		"triggers": `[{"kind":"bead_event","events":["closed"]},{"kind":"cron","cron":"11 2,8,14,20 * * *"}]`,
	})
	trig["labels"] = []any{"trigger:multi", "role:gardener", "gardener-epic:gd-EPIC1"}
	stub.addBead(trig)
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerModeCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "mode: event") {
		t.Errorf("inspect output missing mode: event\n%s", out)
	}
	if !strings.Contains(out, "cron: 11 2,8,14,20 * * *") {
		t.Errorf("inspect output missing fallback cadence\n%s", out)
	}
}

// TestGardenerMode_FlipToEvent flips a timed gardener to event mode and asserts
// the ordered writes (fields → addlabel → rmlabel → comment) landed through
// the HTTP client, the cron field was cleared, the triggers[] array was
// written, and the printed cadence is the fallback cron.
func TestGardenerMode_FlipToEvent(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{
		"cron":     "7,37 * * * *",
		"timezone": "America/Los_Angeles",
	}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	oldOperated := operatedBy
	operatedBy = "slack:U0OPERATOR1"
	t.Cleanup(func() { operatedBy = oldOperated })

	if err := gardenerModeCmd.Flags().Set("event", "true"); err != nil {
		t.Fatalf("set event flag: %v", err)
	}

	cmd := gardenerModeCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	// Ordered writes are the double-fire guard.
	wantOrder := []string{"fields", "addlabel", "rmlabel", "comment"}
	got := stub.opsOf("gd-CAP1")
	if len(got) != len(wantOrder) {
		t.Fatalf("ops = %v, want %v", got, wantOrder)
	}
	for i := range wantOrder {
		if got[i] != wantOrder[i] {
			t.Fatalf("op %d = %s, want %s (order is the double-fire guard)", i, got[i], wantOrder[i])
		}
	}

	// The PATCH cleared cron, backed it up in timed_cron, and wrote triggers[].
	patch := lastFieldsPatch(t, stub, "gd-CAP1")
	if patch["cron"] != "" {
		t.Errorf("cron = %v, want cleared", patch["cron"])
	}
	if patch["timed_cron"] != "7,37 * * * *" {
		t.Errorf("timed_cron = %v, want backed-up 7,37 * * * *", patch["timed_cron"])
	}
	var entries []scheduler.TriggerEntry
	if err := json.Unmarshal([]byte(patch["triggers"].(string)), &entries); err != nil {
		t.Fatalf("triggers not valid JSON: %v", err)
	}
	if len(entries) != 2 || entries[0].Kind != "bead_event" || entries[1].Kind != "cron" {
		t.Fatalf("entries = %+v", entries)
	}
	if !reflect.DeepEqual(entries[0].Match, gardenermode.PlanActivityMatch("gd-EPIC1")) {
		t.Fatalf("subtree match = %+v", entries[0].Match)
	}

	// The printed cadence is the fallback watchdog cron.
	out := buf.String()
	if !strings.Contains(out, "event mode") || !strings.Contains(out, entries[1].Cron) {
		t.Errorf("output = %q, want event mode + fallback cron %q", out, entries[1].Cron)
	}
}

// TestGardenerMode_FlipWithFallbackCronOverride verifies --fallback-cron is
// threaded into the event-mode watchdog entry.
func TestGardenerMode_FlipWithFallbackCronOverride(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{
		"cron": "7,37 * * * *",
	}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	oldOperated := operatedBy
	operatedBy = "slack:U0OPERATOR1"
	t.Cleanup(func() { operatedBy = oldOperated })

	if err := gardenerModeCmd.Flags().Set("event", "true"); err != nil {
		t.Fatalf("set event flag: %v", err)
	}
	if err := gardenerModeCmd.Flags().Set("fallback-cron", "0 */4 * * *"); err != nil {
		t.Fatalf("set fallback-cron flag: %v", err)
	}

	cmd := gardenerModeCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	patch := lastFieldsPatch(t, stub, "gd-CAP1")
	var entries []scheduler.TriggerEntry
	if err := json.Unmarshal([]byte(patch["triggers"].(string)), &entries); err != nil {
		t.Fatalf("triggers not valid JSON: %v", err)
	}
	if entries[1].Cron != "0 */4 * * *" {
		t.Errorf("fallback cron = %q, want overridden 0 */4 * * *", entries[1].Cron)
	}
	if !strings.Contains(buf.String(), "0 */4 * * *") {
		t.Errorf("output missing overridden fallback cron\n%s", buf.String())
	}
}

// TestGardenerMode_BothFlagsError rejects --event --timed as a usage error and
// writes nothing.
func TestGardenerMode_BothFlagsError(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{"cron": "7,37 * * * *"}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	if err := gardenerModeCmd.Flags().Set("event", "true"); err != nil {
		t.Fatalf("set event flag: %v", err)
	}
	if err := gardenerModeCmd.Flags().Set("timed", "true"); err != nil {
		t.Fatalf("set timed flag: %v", err)
	}

	cmd := gardenerModeCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err == nil {
		t.Fatal("want mutually-exclusive error, got nil")
	}
	if ops := stub.opsOf("gd-CAP1"); len(ops) != 0 {
		t.Errorf("both-flags path wrote %v, want no writes", ops)
	}
}

// TestGardenerMode_FlipRequiresOperator errors when no operator identity is
// available to attribute the audit comment.
func TestGardenerMode_FlipRequiresOperator(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{"cron": "7,37 * * * *"}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	oldOperated := operatedBy
	operatedBy = ""
	t.Cleanup(func() { operatedBy = oldOperated })

	if err := gardenerModeCmd.Flags().Set("event", "true"); err != nil {
		t.Fatalf("set event flag: %v", err)
	}

	cmd := gardenerModeCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err == nil {
		t.Fatal("want operator-required error, got nil")
	}
	if ops := stub.opsOf("gd-CAP1"); len(ops) != 0 {
		t.Errorf("operator-missing path wrote %v, want no writes", ops)
	}
}

// TestGardenerMode_IdempotentNoWrites: flipping a timed gardener --timed is a
// no-op that reports the current cadence and writes nothing.
func TestGardenerMode_IdempotentNoWrites(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{"cron": "7,37 * * * *"}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	oldOperated := operatedBy
	operatedBy = "slack:U0OPERATOR1"
	t.Cleanup(func() { operatedBy = oldOperated })

	if err := gardenerModeCmd.Flags().Set("timed", "true"); err != nil {
		t.Fatalf("set timed flag: %v", err)
	}

	cmd := gardenerModeCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if ops := stub.opsOf("gd-CAP1"); len(ops) != 0 {
		t.Errorf("idempotent flip wrote %v, want no writes", ops)
	}
	out := buf.String()
	if !strings.Contains(out, "already in timed mode") || !strings.Contains(out, "7,37 * * * *") {
		t.Errorf("output = %q, want already-in-timed with cadence", out)
	}
}

// lastFieldsPatch returns the most recent PATCH fields body for a bead.
func lastFieldsPatch(t *testing.T, stub *gardenerStub, id string) map[string]any {
	t.Helper()
	stub.mu.Lock()
	defer stub.mu.Unlock()
	patches := stub.patches[id]
	if len(patches) == 0 {
		t.Fatalf("no PATCH recorded for %s", id)
	}
	last := patches[len(patches)-1]
	fields, ok := last["fields"].(map[string]any)
	if !ok {
		t.Fatalf("PATCH body for %s has no fields object: %+v", id, last)
	}
	return fields
}

// TestGardenerMode_EventRefreshAddsStewardNudge: --event on a gardener the
// 11-01 flip left in event mode (plan-activity + watchdog) adds the
// steward-nudge entry, keeps the watchdog, and changes no label
// (gd-bo2RUmtpHb).
func TestGardenerMode_EventRefreshAddsStewardNudge(t *testing.T) {
	old, _ := json.Marshal(gardenermode.EventTriggers("gd-EPIC1", "", "5 7 * * *", "America/Los_Angeles", nil))
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	trig := makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{
		"project":  "gardenio",
		"timezone": "America/Los_Angeles",
		"triggers": string(old),
	})
	trig["labels"] = []any{"trigger:multi", "role:gardener", "plan-gardener", "gardener-epic:gd-EPIC1"}
	stub.addBead(trig)
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	oldOperated := operatedBy
	operatedBy = "slack:U0OPERATOR1"
	t.Cleanup(func() { operatedBy = oldOperated })

	if err := gardenerModeCmd.Flags().Set("event", "true"); err != nil {
		t.Fatalf("set event flag: %v", err)
	}
	cmd := gardenerModeCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)
	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	if got := stub.opsOf("gd-CAP1"); len(got) != 2 || got[0] != "fields" || got[1] != "comment" {
		t.Fatalf("ops = %v, want [fields comment]", got)
	}
	patch := lastFieldsPatch(t, stub, "gd-CAP1")
	want, _ := json.Marshal(gardenermode.EventTriggers("gd-EPIC1", "gardenio", "5 7 * * *", "America/Los_Angeles", nil))
	if patch["triggers"] != string(want) {
		t.Fatalf("triggers = %v\nwant       %s", patch["triggers"], want)
	}
	if out := buf.String(); !strings.Contains(out, "triggers refreshed") || !strings.Contains(out, "5 7 * * *") {
		t.Errorf("output = %q", out)
	}
}
