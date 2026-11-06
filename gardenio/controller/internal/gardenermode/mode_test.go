package gardenermode

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

type call struct{ op, a, b string }

type fakeWriter struct {
	calls  []call
	fields map[string]any
}

func (f *fakeWriter) UpdateBeadFieldsTyped(_ context.Context, id string, fl map[string]any) error {
	f.fields = fl
	f.calls = append(f.calls, call{"fields", id, ""})
	return nil
}
func (f *fakeWriter) AddLabel(_ context.Context, id, l string) error {
	f.calls = append(f.calls, call{"addlabel", id, l})
	return nil
}
func (f *fakeWriter) RemoveLabel(_ context.Context, id, l string) error {
	f.calls = append(f.calls, call{"rmlabel", id, l})
	return nil
}
func (f *fakeWriter) AddComment(_ context.Context, id, author, text string) error {
	f.calls = append(f.calls, call{"comment", author, text})
	return nil
}

func timedBead() *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     "gd-TRIGGER1",
		Labels: []string{"trigger:schedule", "gardener-epic:gd-EPIC1"},
		Fields: map[string]string{"cron": "7,37 * * * *", "timezone": "America/Los_Angeles"},
	}
}

func TestSet_TimedToEvent_OrderAndPayload(t *testing.T) {
	w := &fakeWriter{}
	res, err := Set(context.Background(), w, timedBead(), ModeEvent, Opts{Operator: "matt"})
	if err != nil || !res.Changed {
		t.Fatalf("err=%v changed=%v", err, res.Changed)
	}
	wantOrder := []string{"fields", "addlabel", "rmlabel", "comment"}
	for i, c := range w.calls {
		if c.op != wantOrder[i] {
			t.Fatalf("call %d = %s, want %s (order is the double-fire guard)", i, c.op, wantOrder[i])
		}
	}
	if w.fields["cron"] != "" || w.fields["timed_cron"] != "7,37 * * * *" {
		t.Fatalf("fields = %+v", w.fields)
	}
	var entries []scheduler.TriggerEntry
	if err := json.Unmarshal([]byte(w.fields["triggers"].(string)), &entries); err != nil {
		t.Fatalf("triggers not valid JSON: %v", err)
	}
	if len(entries) != 2 || entries[0].Kind != "bead_event" || entries[1].Kind != "cron" {
		t.Fatalf("entries = %+v", entries)
	}
	if !reflect.DeepEqual(entries[0].Match, PlanActivityMatch("gd-EPIC1")) {
		t.Fatalf("match = %+v", entries[0].Match)
	}
	if entries[0].Debounce != "10m" || entries[0].Throttle != "15m" {
		t.Fatalf("debounce/throttle = %+v", entries[0])
	}
}

func TestSet_EventToTimed_RestoresCron(t *testing.T) {
	b := timedBead()
	b.Labels = []string{"trigger:multi", "gardener-epic:gd-EPIC1"}
	b.Fields = map[string]string{"cron": "", "timed_cron": "9,39 * * * *", "triggers": "[…]"}
	w := &fakeWriter{}
	res, err := Set(context.Background(), w, b, ModeTimed, Opts{Operator: "matt"})
	if err != nil || !res.Changed {
		t.Fatalf("err=%v changed=%v", err, res.Changed)
	}
	if w.calls[0].op != "rmlabel" {
		t.Fatalf("timed flip must remove trigger:multi FIRST, got %v", w.calls[0])
	}
	if w.fields["cron"] != "9,39 * * * *" || w.fields["timed_cron"] != "" || w.fields["triggers"] != "" {
		t.Fatalf("fields = %+v", w.fields)
	}
	if res.Cron != "9,39 * * * *" {
		t.Fatalf("res.Cron = %q", res.Cron)
	}
}

func TestSet_Idempotent_NoWrites(t *testing.T) {
	w := &fakeWriter{}
	res, err := Set(context.Background(), w, timedBead(), ModeTimed, Opts{Operator: "matt"})
	if err != nil || res.Changed || len(w.calls) != 0 {
		t.Fatalf("err=%v res=%+v calls=%v", err, res, w.calls)
	}
}

func TestSet_AmbiguousLabels_ErrorNoWrites(t *testing.T) {
	for _, labels := range [][]string{
		{"trigger:schedule", "trigger:multi", "gardener-epic:gd-EPIC1"},
		{"gardener-epic:gd-EPIC1"},
	} {
		b := timedBead()
		b.Labels = labels
		w := &fakeWriter{}
		if _, err := Set(context.Background(), w, b, ModeEvent, Opts{Operator: "matt"}); err == nil {
			t.Fatalf("labels %v: want error", labels)
		}
		if len(w.calls) != 0 {
			t.Fatalf("labels %v: wrote %v", labels, w.calls)
		}
	}
}

func TestSet_NoEpicLabel_ErrorOnEventFlip(t *testing.T) {
	b := timedBead()
	b.Labels = []string{"trigger:schedule"}
	w := &fakeWriter{}
	if _, err := Set(context.Background(), w, b, ModeEvent, Opts{Operator: "matt"}); err == nil ||
		!strings.Contains(err.Error(), "gardener-epic") {
		t.Fatalf("want gardener-epic error, got %v", err)
	}
}

func TestSet_NoOperator_Error(t *testing.T) {
	w := &fakeWriter{}
	if _, err := Set(context.Background(), w, timedBead(), ModeEvent, Opts{}); err == nil {
		t.Fatal("want operator-required error")
	}
}

// eventEntries decodes the triggers[] payload the last flip wrote.
func eventEntries(t *testing.T, w *fakeWriter) []scheduler.TriggerEntry {
	t.Helper()
	raw, ok := w.fields["triggers"].(string)
	if !ok {
		t.Fatalf("no triggers field written: %+v", w.fields)
	}
	var entries []scheduler.TriggerEntry
	if err := json.Unmarshal([]byte(raw), &entries); err != nil {
		t.Fatalf("triggers not valid JSON: %v", err)
	}
	return entries
}

func TestSet_TimedToEvent_NoFallbackCron(t *testing.T) {
	w := &fakeWriter{}
	res, err := Set(context.Background(), w, timedBead(), ModeEvent, Opts{Operator: "matt", NoFallbackCron: true})
	if err != nil || !res.Changed {
		t.Fatalf("err=%v changed=%v", err, res.Changed)
	}
	entries := eventEntries(t, w)
	if len(entries) != 1 || entries[0].Kind != "bead_event" {
		t.Fatalf("NoFallbackCron must yield ONLY the bead_event entry, got %+v", entries)
	}
	if res.Cron != "" {
		t.Fatalf("res.Cron = %q, want empty (no watchdog)", res.Cron)
	}
}

func TestSet_PlanGardener_DefaultsToNoFallbackCron(t *testing.T) {
	b := timedBead()
	b.Labels = append(b.Labels, planGardenerMarker)
	w := &fakeWriter{}
	if _, err := Set(context.Background(), w, b, ModeEvent, Opts{Operator: "matt"}); err != nil {
		t.Fatalf("err=%v", err)
	}
	entries := eventEntries(t, w)
	if len(entries) != 1 || entries[0].Kind != "bead_event" {
		t.Fatalf("a plan gardener must default to NO watchdog cron, got %+v", entries)
	}
}

func TestSet_PlanGardener_ExplicitFallbackCronOverridesDefault(t *testing.T) {
	b := timedBead()
	b.Labels = append(b.Labels, planGardenerMarker)
	w := &fakeWriter{}
	if _, err := Set(context.Background(), w, b, ModeEvent, Opts{Operator: "matt", FallbackCron: "0 */12 * * *"}); err != nil {
		t.Fatalf("err=%v", err)
	}
	entries := eventEntries(t, w)
	if len(entries) != 2 || entries[1].Kind != "cron" || entries[1].Cron != "0 */12 * * *" {
		t.Fatalf("explicit --fallback-cron must override the plan default, got %+v", entries)
	}
}

func TestSet_TimedToEvent_CarriesConditionsAsPreflight(t *testing.T) {
	b := timedBead()
	b.Fields["conditions"] = `[{"type":"bead_query","op":">","value":"0","parent":"gd-EPIC1","types":["task"],"statuses":["open"]}]`
	w := &fakeWriter{}
	if _, err := Set(context.Background(), w, b, ModeEvent, Opts{Operator: "matt"}); err != nil {
		t.Fatalf("err=%v", err)
	}
	entries := eventEntries(t, w)
	if len(entries) != 2 || entries[1].Kind != "cron" {
		t.Fatalf("want a fallback cron entry, got %+v", entries)
	}
	pf := entries[1].Preflight
	if pf == nil || pf.Type != "bead_query" || pf.Op != ">" || pf.Value != "0" || pf.Parent != "gd-EPIC1" {
		t.Fatalf("fallback cron must carry the conditions gate as preflight, got %+v", pf)
	}
}

func TestSetCadence_Event_SetAndClear(t *testing.T) {
	b := timedBead()
	b.Labels = []string{"trigger:multi", "gardener-epic:gd-EPIC1"}
	b.Fields = map[string]string{
		"cron":       "",
		"timezone":   "America/Los_Angeles",
		"conditions": `[{"type":"bead_query","op":">","value":"0","parent":"gd-EPIC1"}]`,
		"triggers":   `[{"kind":"bead_event","label":"plan-activity","events":["created"],"match":{"field":"id","op":"in_subtree_of","value":"gd-EPIC1"}}]`,
	}
	// Set a fallback cron on an event gardener that had none.
	w := &fakeWriter{}
	res, err := SetCadence(context.Background(), w, b, "0 */6 * * *", "", Opts{Operator: "matt"})
	if err != nil || res.Cron != "0 */6 * * *" {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	entries := eventEntries(t, w)
	if len(entries) != 2 || entries[0].Kind != "bead_event" || entries[1].Kind != "cron" || entries[1].Cron != "0 */6 * * *" {
		t.Fatalf("want bead_event + cron watchdog, got %+v", entries)
	}
	if entries[1].Preflight == nil || entries[1].Preflight.Type != "bead_query" {
		t.Fatalf("watchdog must carry the conditions preflight, got %+v", entries[1].Preflight)
	}

	// Clearing the cadence removes the watchdog entirely (purely event-driven).
	b.Fields["triggers"] = w.fields["triggers"].(string)
	w2 := &fakeWriter{}
	if _, err := SetCadence(context.Background(), w2, b, "", "", Opts{Operator: "matt"}); err != nil {
		t.Fatalf("err=%v", err)
	}
	cleared := eventEntries(t, w2)
	if len(cleared) != 1 || cleared[0].Kind != "bead_event" {
		t.Fatalf("clearing cadence must drop the watchdog, got %+v", cleared)
	}
}

func TestSetCadence_Timed_RequiresNonEmpty(t *testing.T) {
	w := &fakeWriter{}
	if _, err := SetCadence(context.Background(), w, timedBead(), "", "", Opts{Operator: "matt"}); err == nil {
		t.Fatal("a timed gardener must reject an empty cron")
	}
	if len(w.calls) != 0 {
		t.Fatalf("rejected cadence must write nothing, got %v", w.calls)
	}
}

func TestSetCadence_Timed_SetsCron(t *testing.T) {
	w := &fakeWriter{}
	res, err := SetCadence(context.Background(), w, timedBead(), "15 * * * *", "", Opts{Operator: "matt"})
	if err != nil || res.Cron != "15 * * * *" {
		t.Fatalf("err=%v res=%+v", err, res)
	}
	if w.fields["cron"] != "15 * * * *" {
		t.Fatalf("cron field = %+v", w.fields)
	}
}

func TestSetCadence_NoOperator_Error(t *testing.T) {
	w := &fakeWriter{}
	if _, err := SetCadence(context.Background(), w, timedBead(), "15 * * * *", "", Opts{}); err == nil {
		t.Fatal("want operator-required error")
	}
}

// A gardener stored before the actor leaf gets PlanActivityMatch on load, so
// per-occurrence keys never let it wake itself on its own writes
// (gd-wRullKPlWJ); a match someone changed by hand is left alone.
func TestUpgradeLegacyPlanActivity(t *testing.T) {
	legacy := `[{"kind":"bead_event","label":"plan-activity","events":["created","updated","closed"],"match":{"field":"id","op":"in_subtree_of","value":"gd-EPIC1"}},` +
		`{"kind":"bead_event","label":"custom","match":{"field":"id","op":"in_subtree_of","value":"gd-EPIC1"}}]`
	entries, err := scheduler.ParseTriggers(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !UpgradeLegacyPlanActivity(entries, "gd-EPIC1") {
		t.Fatal("a legacy plan-activity match must be upgraded")
	}
	if !reflect.DeepEqual(entries[0].Match, PlanActivityMatch("gd-EPIC1")) {
		t.Errorf("plan-activity match = %+v", entries[0].Match)
	}
	if entries[1].Match.Op != "in_subtree_of" {
		t.Errorf("a non-plan-activity entry must be left alone, got %+v", entries[1].Match)
	}
	if UpgradeLegacyPlanActivity(entries, "gd-EPIC1") {
		t.Error("a current match must not be rewritten again")
	}

	other, _ := scheduler.ParseTriggers(`[{"kind":"bead_event","label":"plan-activity","match":{"field":"id","op":"in_subtree_of","value":"gd-OTHER"}}]`)
	if UpgradeLegacyPlanActivity(other, "gd-EPIC1") {
		t.Error("a match on another epic is not the legacy shape")
	}
}
