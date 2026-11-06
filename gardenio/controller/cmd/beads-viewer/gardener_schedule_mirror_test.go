package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestIsScheduleMirrorGardenerBead exercises each of the three independent
// exclusion signals (schedule-mirror label, source field, static created_by +
// schedule: ref) and — critically — asserts that a real gardener carrying none
// of them is NOT flagged (bug ka-0000002811: no real gardener accidentally
// excluded).
func TestIsScheduleMirrorGardenerBead(t *testing.T) {
	cases := []struct {
		name string
		bead *beadsapi.BeadDetail
		want bool
	}{
		{"nil", nil, false},
		{
			"a real affected mirror (all three signals)",
			&beadsapi.BeadDetail{
				ID:        "ka-0000005169",
				Title:     "schedule-mirror: Coverage Gardener",
				Labels:    []string{"project:gardenio", "role:gardener", "schedule:ka-0000004416", "schedule-mirror"},
				Fields:    map[string]string{"role": "gardener", "source": "schedule-mirror"},
				CreatedBy: "static",
			},
			true,
		},
		{
			"schedule-mirror label alone",
			&beadsapi.BeadDetail{ID: "gd-x", Title: "Innocuous", Labels: []string{"schedule-mirror", "role:gardener"}, CreatedBy: "thread-123"},
			true,
		},
		{
			"source=schedule-mirror field alone",
			&beadsapi.BeadDetail{ID: "gd-x", Title: "Gardener: real-sounding", Labels: []string{"role:gardener"}, Fields: map[string]string{"source": "schedule-mirror"}, CreatedBy: "thread-123"},
			true,
		},
		{
			"static created_by + schedule: ref label (no schedule-mirror label/source)",
			&beadsapi.BeadDetail{ID: "gd-x", Title: "Gardener", Labels: []string{"role:gardener", "schedule:gd-foreign"}, CreatedBy: "static"},
			true,
		},
		{
			"static created_by WITHOUT a schedule: ref — not enough to flag",
			&beadsapi.BeadDetail{ID: "gd-x", Title: "Gardener", Labels: []string{"role:gardener"}, CreatedBy: "static"},
			false,
		},
		{
			"a real gardener — none of the signals",
			&beadsapi.BeadDetail{
				ID:        "gd-real",
				Title:     "Gardener: drive operated_by initials epic",
				Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
				CreatedBy: "thread-1779908146-389129",
			},
			false,
		},
		{
			"real gardener with a schedule: label but a thread author — not a mirror",
			&beadsapi.BeadDetail{
				ID:        "gd-real2",
				Title:     "Gardener: legacy schedule-backed",
				Labels:    []string{"role:gardener", "schedule:gd-own"},
				CreatedBy: "thread-1779908146-389129",
			},
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isScheduleMirrorGardenerBead(tc.bead); got != tc.want {
				t.Errorf("isScheduleMirrorGardenerBead(%q) = %v; want %v", tc.name, got, tc.want)
			}
		})
	}
}

// mkScheduleMirrorGardenerBead builds a gardener bead in the shape the gardenio-next
// config mirror dual-writes: a type=trigger + role:gardener bead with the
// schedule-mirror label, source=schedule-mirror field, static created_by, and a
// schedule:<foreign-id> reference, carrying an embedded engine tally with a big
// foreign run count.
func mkScheduleMirrorGardenerBead() *beadsapi.BeadDetail {
	spec := map[string]any{
		"name":           "Coverage Implementation+Monitoring Gardener",
		"terminal_goal":  "all phase-2 epics closed",
		"runs_spawned":   394,
		"runs_completed": 382,
	}
	raw, _ := json.Marshal(spec)
	return &beadsapi.BeadDetail{
		ID:        "ka-0000005169",
		Title:     "schedule-mirror: Coverage Implementation+Monitoring Gardener",
		Type:      "trigger",
		Status:    "open",
		Labels:    []string{"project:gardenio", "role:gardener", "schedule:ka-0000004416", "schedule-mirror"},
		Fields:    map[string]string{"role": "gardener", "source": "schedule-mirror", "gardener": string(raw)},
		CreatedBy: "static",
		UpdatedAt: time.Date(2026, 5, 26, 5, 38, 0, 0, time.UTC),
	}
}

// TestHandleGardeners_ExcludesScheduleMirror seeds a mirror bead alongside the
// real gardeners and asserts the index renders the real ones but NOT the mirror
// (bug ka-0000002811, acceptance criteria 1 + 3).
func TestHandleGardeners_ExcludesScheduleMirror(t *testing.T) {
	f := seedGardenersIndexFake(t)
	f.triggerScheduleResp.Beads = append(f.triggerScheduleResp.Beads, mkScheduleMirrorGardenerBead())
	f.triggerScheduleResp.Total = len(f.triggerScheduleResp.Beads)

	mux := mkServerWithNow(t, f, gardenersNow)
	req := httptest.NewRequest("GET", "/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// The two real gardeners still render.
	if !strings.Contains(body, "delivery") || !strings.Contains(body, "watchdog") {
		t.Errorf("real gardeners missing from index")
	}
	// The mirror must NOT render as a GARDENER ROW: its foreign name and its
	// /gardener/{id} life-thread link must be absent from the live roster.
	for _, banned := range []string{"Coverage Implementation+Monitoring Gardener", `href="/gardener/ka-0000005169"`} {
		if strings.Contains(body, banned) {
			t.Errorf("index leaked schedule-mirror as a gardener row: %q", banned)
		}
	}
	if !strings.Contains(body, `href="/gardener/gd-cap1"`) {
		t.Errorf("expected real gardener link present")
	}
	// But the mirror IS accounted for in the "N hidden — why" disclosure
	// (bug ka-0000007605): it links to the plain bead page and is labelled a
	// schedule-mirror artifact, so the All-chip-vs-population delta is explained.
	if !strings.Contains(body, "hidden from this roster") {
		t.Errorf("missing 'N hidden' affordance for the excluded mirror")
	}
	if !strings.Contains(body, `href="/ka-0000005169"`) {
		t.Errorf("hidden disclosure should link the mirror's plain bead page; body=%s", body)
	}
	if !strings.Contains(body, "schedule-mirror artifact") {
		t.Errorf("hidden disclosure should classify the mirror; body=%s", body)
	}
}

// TestHandleGardenerDetail_ScheduleMirrorReturns404 asserts a deep link to a
// mirror gardener 404s rather than rendering its foreign run_log as real
// (bug ka-0000002811 — detail surface).
func TestHandleGardenerDetail_ScheduleMirrorReturns404(t *testing.T) {
	f := newFake()
	m := mkScheduleMirrorGardenerBead()
	f.beads[m.ID] = m

	mux := mkServerWithNow(t, f, gardenersNow)
	req := httptest.NewRequest("GET", "/gardener/"+m.ID, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for schedule-mirror detail, got %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "Coverage Implementation+Monitoring Gardener") {
		t.Errorf("mirror detail leaked foreign gardener name")
	}
}

// TestHandleMap_ScheduleMirrorReturns404 asserts a deep-linked /maps/{id} for a
// mirror gardener 404s rather than rendering a foreign constellation
// (bug ka-0000002811 — map surface shares the gardener detail chokepoint).
func TestHandleMap_ScheduleMirrorReturns404(t *testing.T) {
	f := newFake()
	m := mkScheduleMirrorGardenerBead()
	f.beads[m.ID] = m

	mux := mkServerWithNow(t, f, gardenersNow)
	req := httptest.NewRequest("GET", "/maps/"+m.ID, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for schedule-mirror map, got %d body=%s", w.Code, w.Body.String())
	}
}
