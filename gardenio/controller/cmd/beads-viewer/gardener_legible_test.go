package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/scheduler"
	"gardenio/controller/internal/scheduler/conditions"
)

// TestDescribeStopEntry_AllKinds asserts every supported stop kind decodes to a
// human clause containing no raw JSON key, so the stop-spec render never leaks a
// field name. Each case names the kind and a substring the prose must carry.
func TestDescribeStopEntry_AllKinds(t *testing.T) {
	cases := []struct {
		name string
		e    scheduler.StopEntry
		want string
	}{
		{"max_runs", scheduler.StopEntry{Kind: scheduler.StopKindMaxRuns, Value: "40"}, "after 40 runs"},
		{"wall_clock", scheduler.StopEntry{Kind: scheduler.StopKindWallClock, Duration: "24h"}, "24h"},
		{"bead_query drain", scheduler.StopEntry{Kind: scheduler.StopKindBeadQuery, Op: "==", Value: "0", Parent: "gd-epic", Statuses: []string{"open"}, Types: []string{"task"}}, "no open task beads under gd-epic"},
		{"specific_bead_closed", scheduler.StopEntry{Kind: scheduler.StopKindSpecificBeadClosed, BeadID: "gd-target"}, "gd-target closes"},
		{"gate_present", scheduler.StopEntry{Kind: scheduler.StopKindGatePresent, Value: "release-cut"}, "release-cut"},
		{"gate_absent", scheduler.StopEntry{Kind: scheduler.StopKindGateAbsent, Value: "hold"}, "clears"},
		{"no_progress_for", scheduler.StopEntry{Kind: scheduler.StopKindNoProgressFor, Runs: 5}, "5 runs with no progress"},
		{"consecutive_failures", scheduler.StopEntry{Kind: scheduler.StopKindConsecutiveFailures, Value: "3"}, "3 consecutive failures"},
		{"mr_merged", scheduler.StopEntry{Kind: scheduler.StopKindMRMerged, Parent: "gd-ep"}, "every tracked MR under gd-ep is merged"},
		{"goal_satisfied", scheduler.StopEntry{Kind: scheduler.StopKindGoalSatisfied}, "terminal goal met"},
		{"manual reason", scheduler.StopEntry{Kind: scheduler.StopKindManual, ReasonField: "stop_reason"}, "stop_reason"},
		{"manual_force label", scheduler.StopEntry{Kind: scheduler.StopKindManualForce, Label: "kill"}, "kill"},
		{"unknown verbatim", scheduler.StopEntry{Kind: "future_kind"}, "future_kind"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := describeStopEntry(tc.e)
			if !strings.Contains(got, tc.want) {
				t.Errorf("describeStopEntry(%s) = %q, want substring %q", tc.name, got, tc.want)
			}
			if strings.Contains(got, "{") || strings.Contains(got, "\"kind\"") {
				t.Errorf("describeStopEntry(%s) leaked raw JSON: %q", tc.name, got)
			}
		})
	}
}

// TestDescribeExpression_Compositions asserts the condition humanizer recurses
// through all/any/not and describes daemon + field leaves without raw JSON.
func TestDescribeExpression_Compositions(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"bead_query gate", `{"type":"bead_query","op":">","value":"0","statuses":["open"],"types":["task"],"project":"gardenio"}`, "any open task beads in gardenio"},
		{"gate present", `{"type":"gate","op":"present","value":"go"}`, "gate"},
		{"ready_queue", `{"type":"ready_queue","op":">=","value":"1"}`, "ready in the queue"},
		{"active_agents role", `{"type":"active_agents","op":"==","value":"0","role":"crew"}`, "crew"},
		{"field ==", `{"field":"parent","op":"==","value":"gd-x"}`, "is"},
		{"all composition", `{"all":[{"type":"gate","op":"present","value":"a"},{"type":"bead_query","op":"==","value":"0"}]}`, " and "},
		{"any composition", `{"any":[{"type":"gate","op":"present","value":"a"},{"type":"gate","op":"present","value":"b"}]}`, " or "},
		{"not composition", `{"not":{"type":"gate","op":"present","value":"a"}}`, "not ("},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, err := conditions.ParseExpression(tc.raw)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got := describeExpression(e)
			if !strings.Contains(got, tc.want) {
				t.Errorf("describeExpression(%s) = %q, want substring %q", tc.name, got, tc.want)
			}
			if strings.Contains(got, "\"type\"") || strings.Contains(got, "{\"") {
				t.Errorf("describeExpression(%s) leaked raw JSON: %q", tc.name, got)
			}
		})
	}
}

// TestCountPhrase covers the quantifier mapping for bead-query counts.
func TestCountPhrase(t *testing.T) {
	cases := []struct{ op, v, want string }{
		{"==", "0", "no"},
		{"==", "3", "exactly 3"},
		{">", "0", "any"},
		{">=", "1", "any"},
		{">=", "3", "at least 3"},
		{"<", "5", "fewer than 5"},
		{"<=", "2", "at most 2"},
		{"!=", "0", "any"},
	}
	for _, tc := range cases {
		if got := countPhrase(tc.op, tc.v); got != tc.want {
			t.Errorf("countPhrase(%q,%q) = %q, want %q", tc.op, tc.v, got, tc.want)
		}
	}
}

// TestDecodeGardenerWakes_Cron asserts a cron gardener surfaces a clock cadence
// wake decoded off the header projection (no raw cron in the summary).
func TestDecodeGardenerWakes_Cron(t *testing.T) {
	b := &beadsapi.BeadDetail{
		ID:     "gd-cap",
		Type:   "trigger",
		Fields: map[string]string{"cron": "0 * * * *", "role": "gardener"},
	}
	v := &gardenerView{Cron: "0 * * * *", Enabled: true, NextFireRelative: "in 12m", NextFire: "2026-08-11 17:00 UTC"}
	wakes := decodeGardenerWakes(b, v)
	if len(wakes) == 0 {
		t.Fatal("expected a cron wake")
	}
	if wakes[0].Kind != "cron" {
		t.Errorf("kind = %q, want cron", wakes[0].Kind)
	}
	if !strings.Contains(wakes[0].Summary, "fires in 12m") {
		t.Errorf("summary = %q, want next-fire phrasing", wakes[0].Summary)
	}
	// The raw cron expression belongs in the tooltip, not the visible summary.
	if strings.Contains(wakes[0].Summary, "* * *") {
		t.Errorf("summary leaked raw cron: %q", wakes[0].Summary)
	}
	if !strings.Contains(wakes[0].Detail, "0 * * * *") {
		t.Errorf("detail should carry the raw cron: %q", wakes[0].Detail)
	}
}

// TestDecodeGardenerWakes_ConditionsGate asserts a bare `conditions` pre-flight
// gate decodes into an "only when …" wake.
func TestDecodeGardenerWakes_ConditionsGate(t *testing.T) {
	b := &beadsapi.BeadDetail{
		ID:   "gd-cap",
		Type: "trigger",
		Fields: map[string]string{
			"role":       "gardener",
			"conditions": `[{"type":"bead_query","op":">","value":"0","statuses":["open"],"types":["task"],"parent":"gd-epic"}]`,
		},
	}
	v := &gardenerView{Enabled: true}
	wakes := decodeGardenerWakes(b, v)
	if len(wakes) != 1 || wakes[0].Kind != "gate" {
		t.Fatalf("expected one gate wake, got %+v", wakes)
	}
	if !strings.Contains(wakes[0].Summary, "only when any open task beads under gd-epic") {
		t.Errorf("gate summary = %q", wakes[0].Summary)
	}
}

// TestDecodeGardenerWakes_TriggersArray asserts a multi-source triggers array
// decodes bead_event / decision_resolved entries (and skips the cron entry,
// which the header projection already surfaces).
func TestDecodeGardenerWakes_TriggersArray(t *testing.T) {
	entries := []scheduler.TriggerEntry{
		{Kind: scheduler.TriggerKindCron, Cron: "0 * * * *"},
		{Kind: scheduler.TriggerKindBeadEvent, Events: []string{"closed"}},
		{Kind: scheduler.TriggerKindDecisionResolved},
	}
	raw, _ := json.Marshal(entries)
	b := &beadsapi.BeadDetail{
		ID:     "gd-cap",
		Type:   "trigger",
		Labels: []string{"trigger:multi"},
		Fields: map[string]string{"role": "gardener", "triggers": string(raw)},
	}
	v := &gardenerView{Cron: "0 * * * *", Enabled: true, NextFireRelative: "in 5m"}
	wakes := decodeGardenerWakes(b, v)

	var kinds []string
	for _, w := range wakes {
		kinds = append(kinds, w.Kind)
	}
	// Exactly one cron (from the header projection), plus event + decision from
	// the triggers array — the cron ENTRY inside triggers[] must not double.
	cron := 0
	for _, k := range kinds {
		if k == "cron" {
			cron++
		}
	}
	if cron != 1 {
		t.Errorf("expected exactly one cron wake (no double from triggers[]), kinds=%v", kinds)
	}
	if !slices.Contains(kinds, "event") || !slices.Contains(kinds, "decision") {
		t.Errorf("expected event + decision wakes, kinds=%v", kinds)
	}
}

// TestDecodeGardenerStops_Precedence asserts stops are ordered hard_stop >
// loop_end > run_end and each carries its slot label.
func TestDecodeGardenerStops_Precedence(t *testing.T) {
	spec := &bridge.GardenerSpec{Stop: json.RawMessage(`{
		"run_end":  {"any":[{"kind":"wall_clock","duration":"1h"}]},
		"loop_end": {"any":[{"kind":"bead_query","op":"==","value":"0","parent":"gd-ep","statuses":["open"]}]},
		"hard_stop":{"any":[{"kind":"consecutive_failures","value":"3"}]}
	}`)}
	stops := decodeGardenerStops(spec)
	if len(stops) != 3 {
		t.Fatalf("expected 3 stops, got %d: %+v", len(stops), stops)
	}
	if stops[0].Slot != scheduler.StopSlotHardStop {
		t.Errorf("first stop slot = %q, want hard_stop", stops[0].Slot)
	}
	if stops[1].Slot != scheduler.StopSlotLoopEnd {
		t.Errorf("second stop slot = %q, want loop_end", stops[1].Slot)
	}
	if stops[2].Slot != scheduler.StopSlotRunEnd {
		t.Errorf("third stop slot = %q, want run_end", stops[2].Slot)
	}
	if stops[0].SlotLabel == "" {
		t.Error("stop slot label should be set")
	}
}

// TestSummarizeStops_PrefersLoopEnd asserts the compact one-liner leads with the
// loop-ending (goal) condition.
func TestSummarizeStops_PrefersLoopEnd(t *testing.T) {
	stops := []gardenerStop{
		{Slot: scheduler.StopSlotHardStop, Summary: "after 3 consecutive failures"},
		{Slot: scheduler.StopSlotLoopEnd, Summary: "when no open task beads under gd-ep"},
	}
	got := summarizeStops(stops)
	if !strings.HasPrefix(got, "when no open task beads under gd-ep") {
		t.Errorf("summary = %q, want loop_end lead", got)
	}
	if !strings.Contains(got, "+1 more") {
		t.Errorf("summary = %q, want +N more suffix", got)
	}
}

// TestSummarizeChasing covers the crew-count + goal composition.
func TestSummarizeChasing(t *testing.T) {
	cases := []struct {
		name      string
		live      int
		goal      string
		closed    bool
		wantParts []string
	}{
		{"one crew", 1, "drain epic", false, []string{"1 crew live", "drain epic"}},
		{"many crews", 3, "", false, []string{"3 crews live"}},
		{"idle", 0, "watch MRs", false, []string{"idle", "watch MRs"}},
		{"closed", 2, "x", true, []string{"done"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizeChasing(tc.live, tc.goal, tc.closed)
			for _, p := range tc.wantParts {
				if !strings.Contains(got, p) {
					t.Errorf("summarizeChasing(%d,%q,%v) = %q, want substring %q", tc.live, tc.goal, tc.closed, got, p)
				}
			}
		})
	}
}

// TestBuildGardenerLegible_Integration decodes a realistic gardener end-to-end and
// asserts the summaries are populated and JSON-free.
func TestBuildGardenerLegible_Integration(t *testing.T) {
	b := &beadsapi.BeadDetail{
		ID:     "gd-cap",
		Type:   "trigger",
		Fields: map[string]string{"cron": "*/30 * * * *", "role": "gardener"},
	}
	spec := &bridge.GardenerSpec{
		TerminalGoal: "drive the release epic to done",
		Stop: json.RawMessage(`{
			"loop_end":{"any":[{"kind":"bead_query","op":"==","value":"0","parent":"gd-rel","statuses":["open"]}]},
			"on_loop_end":"notify_slack"
		}`),
	}
	v := &gardenerView{Cron: "*/30 * * * *", Enabled: true, NextFireRelative: "in 8m", TerminalGoal: spec.TerminalGoal}
	l := buildGardenerLegible(b, spec, v)

	if !l.HasWakes() {
		t.Error("expected wakes")
	}
	if l.WakesSummary == "" || strings.Contains(l.WakesSummary, "*") {
		t.Errorf("wakes summary bad: %q", l.WakesSummary)
	}
	if !l.HasStops() {
		t.Error("expected stops")
	}
	if !strings.Contains(l.StopsSummary, "no open beads under gd-rel") {
		t.Errorf("stops summary = %q", l.StopsSummary)
	}
	if l.OnLoopEnd != "notify Slack on loop end" {
		t.Errorf("on_loop_end = %q", l.OnLoopEnd)
	}
	if l.ChasingGoal == "" {
		t.Error("expected a chasing goal")
	}
}
