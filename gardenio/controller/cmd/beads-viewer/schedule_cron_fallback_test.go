package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestCronFromTriggers covers the low-level parse: pull cron+timezone from the
// first kind:cron entry, ignore non-cron entries, and return empty on
// empty/unparseable/cron-less input.
func TestCronFromTriggers(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		wantCron string
		wantTZ   string
	}{
		{"empty", "", "", ""},
		{"blank", "   ", "", ""},
		{"unparseable", "{not json", "", ""},
		{"not an array", `{"kind":"cron"}`, "", ""},
		{
			"single cron entry",
			`[{"kind":"cron","cron":"*/20 * * * *","timezone":"America/Los_Angeles"}]`,
			"*/20 * * * *", "America/Los_Angeles",
		},
		{
			"cron entry without timezone",
			`[{"kind":"cron","cron":"7,37 * * * *"}]`,
			"7,37 * * * *", "",
		},
		{
			// Event-mode shape: a bead_event entry first, then a watchdog cron.
			// cronFromTriggers returns the cron entry it finds; the CALLERS are
			// responsible for not invoking this on an event-mode gardener (they
			// gate on trigger:multi / source first). This asserts the raw
			// behavior, not the policy.
			"bead_event then cron",
			`[{"kind":"bead_event"},{"kind":"cron","cron":"15 * * * *","timezone":"UTC"}]`,
			"15 * * * *", "UTC",
		},
		{
			"no cron entry",
			`[{"kind":"bead_event"},{"kind":"decision_resolved"}]`,
			"", "",
		},
		{
			"first cron wins",
			`[{"kind":"cron","cron":"1 * * * *","timezone":"UTC"},{"kind":"cron","cron":"2 * * * *","timezone":"America/New_York"}]`,
			"1 * * * *", "UTC",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cron, tz := cronFromTriggers(tc.raw)
			if cron != tc.wantCron || tz != tc.wantTZ {
				t.Errorf("cronFromTriggers(%q) = (%q,%q), want (%q,%q)",
					tc.raw, cron, tz, tc.wantCron, tc.wantTZ)
			}
		})
	}
}

// TestScheduleCronTimezone covers the precedence rule: legacy top-level fields
// win when present; the triggers array fills in only what the legacy fields
// leave empty (cron and timezone fall back independently).
func TestScheduleCronTimezone(t *testing.T) {
	mk := func(cron, tz, triggers string) *beadsapi.BeadDetail {
		return &beadsapi.BeadDetail{
			ID:     "gd-sched",
			Type:   "schedule",
			Fields: map[string]string{"cron": cron, "timezone": tz, "triggers": triggers},
		}
	}
	triggers := `[{"kind":"cron","cron":"*/20 * * * *","timezone":"America/Los_Angeles"}]`

	t.Run("nil bead", func(t *testing.T) {
		if c, z := scheduleCronTimezone(nil); c != "" || z != "" {
			t.Errorf("nil bead = (%q,%q), want empty", c, z)
		}
	})
	t.Run("legacy fields win when both set", func(t *testing.T) {
		c, z := scheduleCronTimezone(mk("0 9 * * *", "UTC", triggers))
		if c != "0 9 * * *" || z != "UTC" {
			t.Errorf("= (%q,%q), want legacy (0 9 * * *,UTC)", c, z)
		}
	})
	t.Run("both legacy empty falls back to triggers", func(t *testing.T) {
		c, z := scheduleCronTimezone(mk("", "", triggers))
		if c != "*/20 * * * *" || z != "America/Los_Angeles" {
			t.Errorf("= (%q,%q), want triggers fallback", c, z)
		}
	})
	t.Run("legacy cron set but timezone cleared falls back for tz only", func(t *testing.T) {
		c, z := scheduleCronTimezone(mk("7,37 * * * *", "", triggers))
		if c != "7,37 * * * *" || z != "America/Los_Angeles" {
			t.Errorf("= (%q,%q), want (7,37 * * * *, America/Los_Angeles)", c, z)
		}
	})
	t.Run("both empty and no triggers yields empty", func(t *testing.T) {
		c, z := scheduleCronTimezone(mk("", "", ""))
		if c != "" || z != "" {
			t.Errorf("= (%q,%q), want empty", c, z)
		}
	})
}

// TestBuildScheduleView_CronFallsBackToTriggers verifies the schedule detail
// page (/b/{id} type=schedule) surfaces cron + next-fire from the triggers
// array once the legacy cron field is cleared — the render surface Step B of
// ka-0000007640 depends on.
func TestBuildScheduleView_CronFallsBackToTriggers(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	bead := &beadsapi.BeadDetail{
		ID:   "gd-sched",
		Type: "schedule",
		Fields: map[string]string{
			"enabled":  "true",
			"triggers": `[{"kind":"cron","cron":"0 23 * * *","timezone":"UTC"}]`,
		},
	}
	v := buildScheduleView(bead, now)
	if v == nil {
		t.Fatal("expected non-nil scheduleView")
	}
	if v.Cron != "0 23 * * *" {
		t.Errorf("Cron = %q, want fallback 0 23 * * *", v.Cron)
	}
	if v.Timezone != "UTC" {
		t.Errorf("Timezone = %q, want fallback UTC", v.Timezone)
	}
	if v.NextFire == "" || v.NextFireError != "" {
		t.Errorf("NextFire = %q, NextFireError = %q; want computed next-fire, no error", v.NextFire, v.NextFireError)
	}
}

// TestBuildScheduleView_LegacyCronWinsOverTriggers is the no-regression control:
// a bead that still carries both surfaces the legacy field, not the triggers
// entry.
func TestBuildScheduleView_LegacyCronWinsOverTriggers(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	bead := &beadsapi.BeadDetail{
		ID:   "gd-sched",
		Type: "schedule",
		Fields: map[string]string{
			"enabled":  "true",
			"cron":     "0 9 * * *",
			"timezone": "America/New_York",
			"triggers": `[{"kind":"cron","cron":"0 23 * * *","timezone":"UTC"}]`,
		},
	}
	v := buildScheduleView(bead, now)
	if v == nil {
		t.Fatal("expected non-nil scheduleView")
	}
	if v.Cron != "0 9 * * *" || v.Timezone != "America/New_York" {
		t.Errorf("Cron/TZ = %q/%q, want legacy (0 9 * * *, America/New_York)", v.Cron, v.Timezone)
	}
}

// TestGardenerViewFromBead_CronFallsBackToTriggers verifies the gardener DETAIL
// header falls back to the triggers cron when the legacy cron is cleared, and
// computes next-fire like a timed gardener.
func TestGardenerViewFromBead_CronFallsBackToTriggers(t *testing.T) {
	d := newFakeDaemon()
	s := gardenerTestServer(t, d) // clock pinned to 2026-06-01 12:00 UTC
	b := &beadsapi.BeadDetail{
		ID:     "gd-cap",
		Title:  "Migrated gardener",
		Type:   "schedule",
		Status: "open",
		Labels: []string{"role:gardener", "project:gardenio"},
		Fields: map[string]string{
			"role":     "gardener",
			"enabled":  "true",
			"triggers": `[{"kind":"cron","cron":"0 23 * * *","timezone":"UTC"}]`,
		},
	}
	v, err := s.gardenerViewFromBead(context.Background(), b)
	if err != nil {
		t.Fatalf("gardenerViewFromBead: %v", err)
	}
	if v.Cron != "0 23 * * *" {
		t.Errorf("Cron = %q, want fallback 0 23 * * *", v.Cron)
	}
	if v.Timezone != "UTC" {
		t.Errorf("Timezone = %q, want fallback UTC", v.Timezone)
	}
	if v.NextFire == "" || v.NextFireError != "" {
		t.Errorf("NextFire = %q / err = %q; want computed next-fire", v.NextFire, v.NextFireError)
	}
}

// TestGardenerViewFromBead_EventModeDoesNotSurfaceWatchdogAsNextFire is the
// event-mode control: a trigger:multi gardener with a bead_event entry + a
// watchdog cron must surface the cron as cadence but NOT compute a next-fire
// clock countdown (existing behavior at gardener.go, must not regress).
func TestGardenerViewFromBead_EventModeDoesNotSurfaceWatchdogAsNextFire(t *testing.T) {
	d := newFakeDaemon()
	s := gardenerTestServer(t, d)
	b := &beadsapi.BeadDetail{
		ID:     "gd-cap-evt",
		Title:  "Event-mode gardener",
		Type:   "trigger",
		Status: "open",
		Labels: []string{"trigger:multi", "role:gardener", "project:gardenio"},
		Fields: map[string]string{
			"role":     "gardener",
			"enabled":  "true",
			"triggers": `[{"kind":"bead_event"},{"kind":"cron","cron":"15 * * * *","timezone":"UTC"}]`,
		},
	}
	v, err := s.gardenerViewFromBead(context.Background(), b)
	if err != nil {
		t.Fatalf("gardenerViewFromBead: %v", err)
	}
	if v.Cron != "15 * * * *" {
		t.Errorf("Cron = %q, want watchdog cadence 15 * * * *", v.Cron)
	}
	if v.NextFire != "" || v.NextFireRelative != "" {
		t.Errorf("NextFire = %q / rel = %q; event-mode must NOT show a clock countdown", v.NextFire, v.NextFireRelative)
	}
}

// TestGardenerRowFromBead_CronFallsBackToTriggers verifies the /gardeners INDEX
// row computes next-fire from the triggers cron when the legacy field is empty.
func TestGardenerRowFromBead_CronFallsBackToTriggers(t *testing.T) {
	d := newFakeDaemon()
	s := gardenerTestServer(t, d)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	b := &beadsapi.BeadDetail{
		ID:     "gd-cap-row",
		Title:  "Migrated gardener",
		Type:   "schedule",
		Status: "open",
		Labels: []string{"role:gardener", "project:gardenio"},
		Fields: map[string]string{
			"role":     "gardener",
			"enabled":  "true",
			"triggers": `[{"kind":"cron","cron":"0 23 * * *","timezone":"UTC"}]`,
		},
	}
	row := s.gardenerRowFromBead(context.Background(), b, now)
	if row.NextFire == "" || row.NextFireError != "" {
		t.Errorf("NextFire = %q / err = %q; want computed next-fire from triggers cron", row.NextFire, row.NextFireError)
	}
	if row.nextFireAt.IsZero() {
		t.Error("nextFireAt is zero; want a parsed sort key from the triggers cron")
	}
}

// TestGardenerRowFromBead_EventModeNoNextFire is the index-row event-mode
// control: a trigger:multi gardener's watchdog cron must not drive a next-fire.
func TestGardenerRowFromBead_EventModeNoNextFire(t *testing.T) {
	d := newFakeDaemon()
	s := gardenerTestServer(t, d)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	b := &beadsapi.BeadDetail{
		ID:     "gd-cap-evt-row",
		Title:  "Event-mode gardener",
		Type:   "trigger",
		Status: "open",
		Labels: []string{"trigger:multi", "role:gardener", "project:gardenio"},
		Fields: map[string]string{
			"role":     "gardener",
			"enabled":  "true",
			"triggers": `[{"kind":"bead_event"},{"kind":"cron","cron":"15 * * * *","timezone":"UTC"}]`,
		},
	}
	row := s.gardenerRowFromBead(context.Background(), b, now)
	if row.NextFire != "" || !row.nextFireAt.IsZero() {
		t.Errorf("NextFire = %q / nextFireAt zero=%v; event-mode watchdog must not drive next-fire",
			row.NextFire, row.nextFireAt.IsZero())
	}
}

// TestHandleTriggers_CronFallsBackToTriggers exercises the /triggers page (the
// target of the /schedules redirect, and the acceptance surface of
// ka-0000007640): a type=schedule gardener whose legacy cron field is cleared
// but which carries a kind:cron triggers entry still renders its cadence +
// next-fire in the row.
func TestHandleTriggers_CronFallsBackToTriggers(t *testing.T) {
	now := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-migrated",
				Title:     "Migrated gardener",
				Type:      "schedule",
				Status:    "open",
				Labels:    []string{"project:gardenio"},
				UpdatedAt: now.Add(-3 * time.Minute),
				Fields: map[string]string{
					// legacy cron+timezone CLEARED (Step B state); cadence lives
					// only in the triggers array now.
					"enabled":  "true",
					"project":  "gardenio",
					"triggers": `[{"kind":"cron","cron":"0 23 * * *","timezone":"UTC"}]`,
				},
			},
		},
		Total: 1,
	}
	mux := mkServerWithNow(t, f, func() time.Time { return now })

	req := httptest.NewRequest("GET", "/triggers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "0 23 * * *") {
		t.Errorf("triggers page missing cron cadence from triggers array\nbody=%s", body)
	}
}
