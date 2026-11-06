package main

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestHandleAgents_RendersUptimeAndExtras verifies the /agents view renders
// per-agent pod uptime ("up Nh"), spawned_by attribution, a task-title link,
// and terminal / Slack deep-link chips when the corresponding fields and
// config are present. Covers gd-10aoohHzaj.
func TestHandleAgents_RendersUptimeAndExtras(t *testing.T) {
	now := time.Now()
	f := newFake()
	f.listResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{
			{
				ID:        "gd-ag-up",
				Title:     "crew-alpha-up",
				Type:      "agent",
				Status:    "in_progress",
				Labels:    []string{"project:gardenio"},
				CreatedAt: now.Add(-5 * time.Hour),
				UpdatedAt: now.Add(-2 * time.Minute),
				Notes:     "pod_name: agent-hq-12",
				Fields: map[string]string{
					"agent":                "crew-alpha-up",
					"role":                 "crew",
					"agent_state":          "working",
					"task_id":              "gd-task-up",
					"task_title":           "Refactor agents page",
					"spawned_by":           "gardener-7",
					"slack_thread_channel": "C0AHE7C0E1G",
					"slack_thread_ts":      "1777831245.785019",
					"project":              "gardenio",
				},
			},
		},
		Total: 1,
	}
	cm := &fakeScreenClient{}
	srv := NewServer(f, slog.Default(), "", "", "", cm)
	srv.coopmuxPublicURL = "https://gardenio.example/mux"
	srv.slackWorkspaceDomain = "acme"
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	req := httptest.NewRequest("GET", "/agents", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		// Uptime chip ("up Xh") in list view; LongRunning (>4h) → warn class.
		"up 5h",
		"agent-uptime-warn",
		// SpawnedBy attribution.
		"by gardener-7",
		// Active label uses "active Nm".
		"active 2m",
		// Task title is now a link to the task page.
		`href="/gd-task-up"`,
		"Refactor agents page",
		// Terminal + Slack deep-links rendered as chips.
		`href="https://gardenio.example/mux#agent-hq-12"`,
		`href="https://acme.slack.com/archives/C0AHE7C0E1G/p1777831245785019?thread_ts=1777831245.785019&amp;cid=C0AHE7C0E1G"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/agents missing %q\nbody snippet: %s", want, snippetN(body, 1200))
		}
	}
}
