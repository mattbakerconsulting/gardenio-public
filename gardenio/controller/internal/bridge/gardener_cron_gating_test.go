package bridge

import (
	"strings"
	"testing"
)

// A trigger:multi gardener whose cron entry carries the exact open-children
// bead_query preflight — the engine's fail-closed cron gate. This is the shape
// of the real gardener (ka-0000001347) the meta-scorer wrongly graded C for "no
// preflight gate" by reading the inert top-level conditions field (ka-0000006215).
const multiGatedTriggers = `[
  {"kind":"cron","cron":"0 * * * *","timezone":"America/Los_Angeles",
   "preflight":{"type":"bead_query","op":">","value":"0","parent":"ka-0000003645","statuses":["open","in_progress","blocked"]}},
  {"kind":"bead_event","events":["closed"],"label":"drain-watch"}
]`

// A trigger:multi gardener whose cron entry has NO per-entry preflight — a
// genuinely ungated hourly cron. This is the ONLY multi shape in which the "fires
// unconditionally into empty gb stop calls" finding is legitimate.
const multiUngatedTriggers = `[
  {"kind":"cron","cron":"0 * * * *","timezone":"America/Los_Angeles"},
  {"kind":"bead_event","events":["closed"]}
]`

// A trigger:multi gardener with no cron entry at all — pure event-wake. There is
// no cron floor to gate, so it is inherently gated and NOT a tuning gap.
const multiEventOnlyTriggers = `[
  {"kind":"bead_event","events":["closed","mail_sent"]},
  {"kind":"decision_resolved"}
]`

// TestCronGating_MultiGated is the core regression for ka-0000006215: a
// trigger:multi gardener with a per-entry cron preflight must read GATED even when
// its top-level conditions field is the vestigial empty legacy value the scorer
// used to read.
func TestCronGating_MultiGated(t *testing.T) {
	v := ScoreGardenerCronGating(multiGatedTriggers, "", "[]")
	if v.Shape != "multi" {
		t.Fatalf("Shape = %q, want multi", v.Shape)
	}
	if !v.HasCron {
		t.Fatal("HasCron = false, want true (there is a cron entry)")
	}
	if !v.Gated {
		t.Fatalf("Gated = false, want true; reason: %s", v.Reason)
	}
	if v.GatedCronEntries != 1 || v.TotalCronEntries != 1 {
		t.Errorf("gated/total = %d/%d, want 1/1", v.GatedCronEntries, v.TotalCronEntries)
	}
}

// TestCronGating_MultiGated_IgnoresInertConditions pins the exact bug premise:
// even a NON-empty legacy top-level conditions field must not change the verdict
// for a multi gardener — the multi shape reads only triggers[].preflight.
func TestCronGating_MultiGated_IgnoresInertConditions(t *testing.T) {
	// A bogus/absent legacy conditions value must be irrelevant when triggers[]
	// is present — the multi source never reads it.
	for _, conds := range []string{"", "[]", `[{"type":"gate","value":"nonsense"}]`} {
		v := ScoreGardenerCronGating(multiGatedTriggers, "0 * * * *", conds)
		if v.Shape != "multi" || !v.Gated {
			t.Errorf("conditions=%q: Shape=%q Gated=%v, want multi/true (per-entry preflight governs)", conds, v.Shape, v.Gated)
		}
	}
}

// TestCronGating_MultiUngated: a multi gardener whose cron entry lacks a preflight
// is genuinely ungated — this is when the tuning finding is real.
func TestCronGating_MultiUngated(t *testing.T) {
	v := ScoreGardenerCronGating(multiUngatedTriggers, "", "")
	if v.Shape != "multi" {
		t.Fatalf("Shape = %q, want multi", v.Shape)
	}
	if !v.HasCron {
		t.Fatal("HasCron = false, want true")
	}
	if v.Gated {
		t.Fatalf("Gated = true, want false (no per-entry preflight); reason: %s", v.Reason)
	}
	if v.GatedCronEntries != 0 || v.TotalCronEntries != 1 {
		t.Errorf("gated/total = %d/%d, want 0/1", v.GatedCronEntries, v.TotalCronEntries)
	}
}

// TestCronGating_MultiEventOnly: a pure event-wake multi gardener has no cron
// floor to gate, so it is Gated (inherently) and not a tuning gap.
func TestCronGating_MultiEventOnly(t *testing.T) {
	v := ScoreGardenerCronGating(multiEventOnlyTriggers, "", "")
	if v.Shape != "multi" {
		t.Fatalf("Shape = %q, want multi", v.Shape)
	}
	if v.HasCron {
		t.Fatal("HasCron = true, want false (no cron entry)")
	}
	if !v.Gated {
		t.Fatalf("Gated = false, want true (no cron floor to gate); reason: %s", v.Reason)
	}
	if v.TotalCronEntries != 0 {
		t.Errorf("TotalCronEntries = %d, want 0", v.TotalCronEntries)
	}
}

// TestCronGating_MultiPartiallyGated: a multi gardener with two cron entries where
// only one carries a preflight is NOT fully gated — the ungated entry still fires
// unconditionally, so the verdict is ungated and the count surfaces the split.
func TestCronGating_MultiPartiallyGated(t *testing.T) {
	const partial = `[
      {"kind":"cron","cron":"0 * * * *","preflight":{"type":"bead_query","op":">","value":"0","parent":"gd-X"}},
      {"kind":"cron","cron":"30 * * * *"}
    ]`
	v := ScoreGardenerCronGating(partial, "", "")
	if v.Gated {
		t.Fatalf("Gated = true, want false (one cron entry ungated); reason: %s", v.Reason)
	}
	if v.GatedCronEntries != 1 || v.TotalCronEntries != 2 {
		t.Errorf("gated/total = %d/%d, want 1/2", v.GatedCronEntries, v.TotalCronEntries)
	}
}

// TestCronGating_LegacyScheduleGated: a legacy trigger:schedule gardener (flat
// cron + top-level conditions, empty triggers[]) reads gating from the
// conditions field — the ONE place that field is authoritative.
func TestCronGating_LegacyScheduleGated(t *testing.T) {
	conds := `[{"type":"bead_query","op":">","value":"0","statuses":["open"]}]`
	v := ScoreGardenerCronGating("", "0 * * * *", conds)
	if v.Shape != "schedule" {
		t.Fatalf("Shape = %q, want schedule", v.Shape)
	}
	if !v.HasCron || !v.Gated {
		t.Fatalf("HasCron=%v Gated=%v, want true/true; reason: %s", v.HasCron, v.Gated, v.Reason)
	}
	if v.GatedCronEntries != 1 || v.TotalCronEntries != 1 {
		t.Errorf("gated/total = %d/%d, want 1/1", v.GatedCronEntries, v.TotalCronEntries)
	}
}

// TestCronGating_LegacyScheduleUngated: a legacy schedule gardener with a cron but
// no conditions preflight is ungated — the real tuning finding for the legacy
// shape.
func TestCronGating_LegacyScheduleUngated(t *testing.T) {
	for _, conds := range []string{"", "[]"} {
		v := ScoreGardenerCronGating("", "0 * * * *", conds)
		if v.Shape != "schedule" {
			t.Errorf("conditions=%q: Shape = %q, want schedule", conds, v.Shape)
		}
		if !v.HasCron {
			t.Errorf("conditions=%q: HasCron = false, want true", conds)
		}
		if v.Gated {
			t.Errorf("conditions=%q: Gated = true, want false; reason: %s", conds, v.Reason)
		}
	}
}

// TestCronGating_NoCronNoTriggers: a bead with neither triggers[] nor a cron
// field has no scheduled wake to gate — shape "none", Gated true.
func TestCronGating_NoCronNoTriggers(t *testing.T) {
	v := ScoreGardenerCronGating("", "", "")
	if v.Shape != "none" {
		t.Fatalf("Shape = %q, want none", v.Shape)
	}
	if v.HasCron {
		t.Fatal("HasCron = true, want false")
	}
	if !v.Gated {
		t.Fatalf("Gated = false, want true; reason: %s", v.Reason)
	}
}

// TestCronGating_MalformedTriggersFallsBackToLegacy: if the triggers[] JSON does
// not parse, the verdict must fall back to the legacy flat fields rather than
// crashing or silently claiming multi.
func TestCronGating_MalformedTriggersFallsBackToLegacy(t *testing.T) {
	v := ScoreGardenerCronGating("{not valid json", "0 * * * *",
		`[{"type":"bead_query","op":">","value":"0"}]`)
	if v.Shape != "schedule" {
		t.Fatalf("Shape = %q, want schedule (fallback on unparseable triggers[])", v.Shape)
	}
	if !v.Gated {
		t.Errorf("Gated = false, want true (legacy conditions present); reason: %s", v.Reason)
	}
}

// TestCronGating_Reason_NamesTheField guards the load-bearing docstring intent:
// the ungated-multi reason must steer the reader to triggers[].preflight and away
// from the top-level conditions field — the whole point of the fix.
func TestCronGating_Reason_NamesTheField(t *testing.T) {
	v := ScoreGardenerCronGating(multiUngatedTriggers, "", "")
	if want := "triggers[].preflight"; !strings.Contains(v.Reason, want) {
		t.Errorf("ungated-multi reason %q does not mention %q", v.Reason, want)
	}
}
