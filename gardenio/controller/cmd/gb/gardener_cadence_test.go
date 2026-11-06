package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/controller/internal/scheduler"
)

// eventGardenerTrigger builds an already-event-mode gardener trigger: the
// trigger:multi label set, an empty flat cron, and a triggers[] array. Extra
// fields (conditions, timezone) merge over the defaults.
func eventGardenerTrigger(id, epicID, triggersJSON string, extra map[string]any) map[string]any {
	fields := map[string]any{"cron": "", "triggers": triggersJSON}
	for k, v := range extra {
		fields[k] = v
	}
	trig := makeGardenerTrigger(id, epicID, fields)
	trig["labels"] = []any{"trigger:multi", "role:gardener", "gardener-epic:" + epicID}
	return trig
}

// triggersFromLastPatch decodes the triggers[] array out of the most recent
// fields PATCH against the gardener.
func triggersFromLastPatch(t *testing.T, stub *gardenerStub, id string) []scheduler.TriggerEntry {
	t.Helper()
	patch := lastFieldsPatch(t, stub, id)
	raw, ok := patch["triggers"].(string)
	if !ok {
		t.Fatalf("patch has no triggers string: %+v", patch)
	}
	var entries []scheduler.TriggerEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatalf("triggers not valid JSON: %v", err)
	}
	return entries
}

// TestGardenerCadence_InspectPrintsModeAndCadence covers the no-flag inspect
// path: it reports mode + effective cadence and writes nothing.
func TestGardenerCadence_InspectPrintsModeAndCadence(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{
		"cron": "7,37 * * * *",
	}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	cmd := gardenerCadenceCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "mode: timed") || !strings.Contains(out, "cron: 7,37 * * * *") {
		t.Errorf("inspect output = %q, want mode + cadence", out)
	}
	if ops := stub.opsOf("gd-CAP1"); len(ops) != 0 {
		t.Errorf("inspect wrote %v, want no writes", ops)
	}
}

// TestGardenerCadence_ClearEventWatchdog is THE regression this command exists
// for (ka-0000005992): a gardener flipped to event mode BEFORE plan gardeners
// stopped getting a default watchdog still carries a legacy unconditional cron,
// and `gb gardener mode --event --no-fallback-cron` cannot strip it (Set
// early-returns Changed:false when already in the requested mode). --none must
// remove the cron entry while preserving the bead_event entry untouched.
func TestGardenerCadence_ClearEventWatchdog(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(eventGardenerTrigger("gd-CAP1", "gd-EPIC1",
		`[{"kind":"bead_event","label":"plan-activity","events":["created","updated","closed"],"debounce":"10m","throttle":"15m"},`+
			`{"kind":"cron","label":"fallback-watchdog","cron":"52 5,11,17,23 * * *","timezone":"America/Los_Angeles"}]`, nil))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	oldOperated := operatedBy
	operatedBy = "slack:U0OPERATOR1"
	t.Cleanup(func() { operatedBy = oldOperated })

	if err := gardenerCadenceCmd.Flags().Set("none", "true"); err != nil {
		t.Fatalf("set none flag: %v", err)
	}

	cmd := gardenerCadenceCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	entries := triggersFromLastPatch(t, stub, "gd-CAP1")
	if len(entries) != 1 {
		t.Fatalf("entries = %+v, want ONLY the bead_event entry", entries)
	}
	if entries[0].Kind != scheduler.TriggerKindBeadEvent {
		t.Fatalf("surviving entry = %+v, want the bead_event entry preserved", entries[0])
	}
	if entries[0].Debounce != "10m" || entries[0].Throttle != "15m" {
		t.Errorf("bead_event entry mutated: %+v — clearing the cron must not touch it", entries[0])
	}
	if out := buf.String(); !strings.Contains(out, "cleared") {
		t.Errorf("output = %q, want a cleared-cadence confirmation", out)
	}
}

// TestGardenerCadence_SetEventWatchdogCarriesConditions verifies that installing
// a watchdog cron carries the bead's top-level `conditions` gate onto the entry
// as a per-entry preflight. MultiTriggerSource never reads the bead's top-level
// conditions (only ScheduleTriggerSource does), so without this the watchdog
// would fire on a drained plan (ka-0000008237).
func TestGardenerCadence_SetEventWatchdogCarriesConditions(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(eventGardenerTrigger("gd-CAP1", "gd-EPIC1",
		`[{"kind":"bead_event","label":"plan-activity","events":["closed"]}]`,
		map[string]any{
			"conditions": `[{"type":"bead_query","op":">","value":"0","parent":"gd-EPIC1","statuses":["open","in_progress","blocked"]}]`,
		}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	oldOperated := operatedBy
	operatedBy = "slack:U0OPERATOR1"
	t.Cleanup(func() { operatedBy = oldOperated })

	if err := gardenerCadenceCmd.Flags().Set("cron", "0 */12 * * *"); err != nil {
		t.Fatalf("set cron flag: %v", err)
	}

	cmd := gardenerCadenceCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}

	entries := triggersFromLastPatch(t, stub, "gd-CAP1")
	var cronEntry *scheduler.TriggerEntry
	for i := range entries {
		if entries[i].Kind == scheduler.TriggerKindCron {
			cronEntry = &entries[i]
		}
	}
	if cronEntry == nil {
		t.Fatalf("no cron entry written: %+v", entries)
	}
	if cronEntry.Cron != "0 */12 * * *" {
		t.Errorf("cron = %q, want the flag value", cronEntry.Cron)
	}
	if cronEntry.Preflight == nil {
		t.Error("watchdog has no preflight — the conditions gate was dropped (ka-0000008237)")
	}
}

// TestGardenerCadence_SetTimedCron verifies the timed-mode path writes the flat
// cron field rather than a triggers[] entry.
func TestGardenerCadence_SetTimedCron(t *testing.T) {
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

	if err := gardenerCadenceCmd.Flags().Set("cron", "*/15 * * * *"); err != nil {
		t.Fatalf("set cron flag: %v", err)
	}

	cmd := gardenerCadenceCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	patch := lastFieldsPatch(t, stub, "gd-CAP1")
	if patch["cron"] != "*/15 * * * *" {
		t.Errorf("cron = %v, want the flag value on the flat field", patch["cron"])
	}
	if _, ok := patch["triggers"]; ok {
		t.Errorf("timed cadence wrote triggers[] — mode confusion: %+v", patch)
	}
}

// TestGardenerCadence_ClearRejectedInTimedMode: a timed gardener with no cron
// never fires, so --none must be refused with ZERO writes rather than silently
// bricking the gardener.
func TestGardenerCadence_ClearRejectedInTimedMode(t *testing.T) {
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

	if err := gardenerCadenceCmd.Flags().Set("none", "true"); err != nil {
		t.Fatalf("set none flag: %v", err)
	}

	cmd := gardenerCadenceCmd
	cmd.SetContext(context.Background())
	cmd.SetOut(&strings.Builder{})

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err == nil {
		t.Fatal("clearing a timed gardener's cron must be refused")
	}
	if ops := stub.opsOf("gd-CAP1"); len(ops) != 0 {
		t.Errorf("refused clear still wrote %v, want zero writes", ops)
	}
}

// TestGardenerCadence_InvalidCronRejectedBeforeWrite: a typo must fail against
// the SAME parser the trigger engine uses at fire time, BEFORE any write, so a
// dead expression that silently never fires is never persisted.
func TestGardenerCadence_InvalidCronRejectedBeforeWrite(t *testing.T) {
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

	if err := gardenerCadenceCmd.Flags().Set("cron", "every 5 minutes plz"); err != nil {
		t.Fatalf("set cron flag: %v", err)
	}

	cmd := gardenerCadenceCmd
	cmd.SetContext(context.Background())
	cmd.SetOut(&strings.Builder{})

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err == nil {
		t.Fatal("invalid cron must be rejected")
	}
	if ops := stub.opsOf("gd-CAP1"); len(ops) != 0 {
		t.Errorf("invalid cron still wrote %v, want validation BEFORE any write", ops)
	}
}

// TestGardenerCadence_NoneAndCronMutuallyExclusive guards the flag contract
// before any bead is even resolved.
func TestGardenerCadence_NoneAndCronMutuallyExclusive(t *testing.T) {
	stub := newGardenerStub()
	stub.addBead(makeEpic("gd-EPIC1", "Slack ratings v2"))
	stub.addBead(makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{"cron": "7,37 * * * *"}))
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })

	if err := gardenerCadenceCmd.Flags().Set("none", "true"); err != nil {
		t.Fatalf("set none flag: %v", err)
	}
	if err := gardenerCadenceCmd.Flags().Set("cron", "*/15 * * * *"); err != nil {
		t.Fatalf("set cron flag: %v", err)
	}

	cmd := gardenerCadenceCmd
	cmd.SetContext(context.Background())
	cmd.SetOut(&strings.Builder{})

	if err := cmd.RunE(cmd, []string{"gd-EPIC1"}); err == nil {
		t.Fatal("--none and --cron together must be a usage error")
	}
}
