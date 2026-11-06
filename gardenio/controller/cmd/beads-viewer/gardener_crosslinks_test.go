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

// crosslinksNow pins the clock so the row builders produce deterministic
// uptime/ago strings.
func crosslinksNow() time.Time { return time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) }

// TestAgentRowFromBeadCtx_ScheduleID covers cross-link (b): the agent
// view-model surfaces b.Fields["schedule_id"] (the gardener trigger bead ID)
// on ScheduleID so the agent card can render a gardener link. Gardener-spawned
// agents carry it; one-off agents leave it empty.
func TestAgentRowFromBeadCtx_ScheduleID(t *testing.T) {
	s := gardenerTestServer(t, newFakeDaemon())
	now := crosslinksNow()

	tests := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{
			name:   "gardener-spawned agent carries schedule_id",
			fields: map[string]string{"agent": "gd-r1", "role": "crew", "schedule_id": "gd-cap"},
			want:   "gd-cap",
		},
		{
			name:   "one-off agent has no schedule_id",
			fields: map[string]string{"agent": "gd-solo", "role": "crew"},
			want:   "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b := &beadsapi.BeadDetail{
				ID:     tc.fields["agent"],
				Type:   "agent",
				Status: "open",
				Fields: tc.fields,
			}
			row := s.agentRowFromBeadCtx(context.Background(), b, "gardenio", now)
			if row.ScheduleID != tc.want {
				t.Errorf("ScheduleID = %q, want %q", row.ScheduleID, tc.want)
			}
		})
	}
}

// TestAgentCard_GardenerLink covers cross-link (b) end-to-end through the
// /agents render: an active agent with schedule_id gets a gardener deep-link;
// an agent without one renders no extra link.
func TestAgentCard_GardenerLink(t *testing.T) {
	f := newFake()
	capAgent := mkIteration("gd-r1", "gd-cap", "", 1, "in_progress", crosslinksNow().Add(-time.Hour))
	soloAgent := &beadsapi.BeadDetail{
		ID:        "gd-solo",
		Title:     "gd-solo",
		Type:      "agent",
		Status:    "in_progress",
		Fields:    map[string]string{"agent": "gd-solo", "role": "crew", "project": "gardenio"},
		CreatedAt: crosslinksNow().Add(-time.Hour),
		UpdatedAt: crosslinksNow().Add(-time.Hour),
	}
	capAgent.Labels = []string{"project:gardenio"}
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{capAgent, soloAgent},
		Total: 2,
	}
	mux := mkServerWithNow(t, f, crosslinksNow)

	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	if !strings.Contains(body, `href="/gardener/gd-cap"`) {
		t.Errorf("agent card with schedule_id missing gardener link\nbody=%s", body)
	}
	// The solo agent must not invent a gardener link of its own.
	if strings.Contains(body, `href="/gardener/gd-solo"`) {
		t.Errorf("agent without schedule_id rendered an unexpected gardener link")
	}
}

// TestTriggerRow_GardenerLink covers cross-link (a): a role:gardener trigger row
// links the role chip to /gardener/{id} (the row's own bead ID), while a
// non-gardener row keeps a plain role chip with no gardener link.
func TestTriggerRow_GardenerLink(t *testing.T) {
	now := crosslinksNow()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-cap",
				Title:     "Delivery gardener",
				Type:      "trigger",
				Status:    "open",
				Labels:    []string{"trigger:schedule", "role:gardener", "project:gardenio"},
				UpdatedAt: now.Add(-3 * time.Minute),
				Fields: map[string]string{
					"cron":    "0 9 * * *",
					"enabled": "true",
					"project": "gardenio",
					"role":    "gardener",
				},
			},
			{
				ID:        "gd-crew",
				Title:     "Crew schedule",
				Type:      "trigger",
				Status:    "open",
				Labels:    []string{"trigger:schedule", "project:gardenio"},
				UpdatedAt: now.Add(-5 * time.Minute),
				Fields: map[string]string{
					"cron":    "0 8 * * *",
					"enabled": "true",
					"project": "gardenio",
					"role":    "crew",
				},
			},
		},
		Total: 2,
	}
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/triggers", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// (a) gardener row links its role chip to /gardener/{ownID}.
	if !strings.Contains(body, `href="/gardener/gd-cap"`) {
		t.Errorf("role:gardener trigger row missing /gardener link\nbody=%s", body)
	}
	// Non-gardener row must not gain a gardener link.
	if strings.Contains(body, `href="/gardener/gd-crew"`) {
		t.Errorf("non-gardener trigger row rendered an unexpected gardener link")
	}
}
