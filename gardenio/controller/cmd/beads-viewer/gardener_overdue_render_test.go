package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// overdueGardenerBead builds an enabled, cron-driven gardener trigger bead whose
// last fire is far in the past relative to gardenerPageNow (2026-06-01T12:00Z),
// so the detail page must flag it OVERDUE (bug ka-0000006095 / A1). extraFields
// merges over the defaults for per-case tweaks (e.g. removing enabled).
func overdueGardenerBead(t *testing.T, spec map[string]any, extraFields map[string]string) *beadsapi.BeadDetail {
	t.Helper()
	raw, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("marshal gardener spec: %v", err)
	}
	fields := map[string]string{
		"role":            "gardener",
		"gardener":        string(raw),
		"enabled":         "true",
		"cron":            "*/30 * * * *",         // every 30m
		"last_outcome_at": "2026-05-30T12:00:00Z", // ~48h before "now"
		"last_run":        "2026-05-30T12:00:00Z",
	}
	for k, v := range extraFields {
		if v == "" {
			delete(fields, k)
			continue
		}
		fields[k] = v
	}
	return &beadsapi.BeadDetail{
		ID:        "gd-cap",
		Title:     "Overdue gardener",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
		Fields:    fields,
		UpdatedAt: time.Date(2026, 6, 1, 11, 0, 0, 0, time.UTC),
	}
}

// TestGardenerDetailGolden_Overdue pins the A1 overdue-detection render: an
// enabled cron gardener whose last fire is ~48h stale surfaces a loud "OVERDUE"
// badge, while a disabled/closed gardener (which cannot fire) never does.
func TestGardenerDetailGolden_Overdue(t *testing.T) {
	spec := map[string]any{
		"name":          "delivery",
		"terminal_goal": "drain the ready queue",
		"runs_spawned":  5,
	}

	cases := []struct {
		name        string
		extraFields map[string]string
		wantPresent []string
		wantAbsent  []string
	}{
		{
			name:        "enabled_stale_fire_is_overdue",
			extraFields: nil,
			wantPresent: []string{"OVERDUE", "expected every"},
		},
		{
			name:        "disabled_never_overdue",
			extraFields: map[string]string{"enabled": "false"},
			wantAbsent:  []string{"OVERDUE"},
		},
		{
			// Budget exhausted flags overdue even without a stale cron.
			name: "budget_exhausted_is_overdue",
			extraFields: map[string]string{
				"last_outcome_at": "2026-06-01T11:59:00Z", // fresh fire
				"last_run":        "2026-06-01T11:59:00Z",
			},
			// spec below overrides runs to exhaust the budget
			wantPresent: []string{"OVERDUE", "run budget exhausted"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := spec
			if tc.name == "budget_exhausted_is_overdue" {
				s = map[string]any{
					"name":          "delivery",
					"terminal_goal": "drain the ready queue",
					"runs_spawned":  10,
					"max_runs":      10,
				}
			}
			f := newFake()
			f.beads["gd-cap"] = overdueGardenerBead(t, s, tc.extraFields)
			f.liveAgentsResp = &beadsapi.ListBeadsResult{}

			body := renderGardenerGolden(t, f, "gd-cap")
			for _, want := range tc.wantPresent {
				if !strings.Contains(body, want) {
					t.Errorf("overdue gardener page missing %q", want)
				}
			}
			for _, bad := range tc.wantAbsent {
				if strings.Contains(body, bad) {
					t.Errorf("gardener page unexpectedly contains %q", bad)
				}
			}
		})
	}
}
