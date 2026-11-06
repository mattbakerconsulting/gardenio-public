package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// decisionDetailFake seeds one open decision with a full context block: a
// requesting agent (with a Slack thread), a requesting task, an owning plan,
// a produces-lineage MR, and prior reflag bookkeeping.
func decisionDetailFake() *fakeDaemon {
	return &fakeDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-dec1": {
				ID:        "gd-dec1",
				Title:     "Pick auth approach",
				Type:      "decision",
				Status:    "open",
				Priority:  2,
				CreatedBy: "gardener-1",
				Labels:    []string{"project:gardenio"},
				CreatedAt: time.Now().Add(-3 * time.Hour),
				UpdatedAt: time.Now().Add(-30 * time.Minute),
				Fields: map[string]string{
					"prompt":                   "Pick auth approach for the new service.",
					"requested_by":             "worker-agent",
					"requesting_agent":         "worker-agent",
					"requesting_agent_bead_id": "gd-agent1",
					"requesting_task":          "gd-task1",
					"requesting_plan":          "gd-plan1",
					"context":                  "Tried JWT locally; sessions need a shared store we don't have yet.",
					"options":                  `[{"id":"jwt","short":"Use JWT","label":"Stateless JWT","artifact_type":"plan"},{"id":"session","short":"Use sessions","label":"Server-side sessions","artifact_type":"plan"}]`,
					"reflag_count":             "1",
					"reflagged_at":             time.Now().Add(-1 * time.Hour).UTC().Format(time.RFC3339),
					"reflagged_by":             "gardener-1",
					"reflag_reason":            "user pinged again",
				},
			},
			"gd-agent1": {
				ID:    "gd-agent1",
				Title: "worker-agent",
				Type:  "agent",
				Fields: map[string]string{
					"slack_thread_channel": "C123",
					"slack_thread_ts":      "1785000000.001",
				},
			},
			"gd-task1": {ID: "gd-task1", Title: "Ship the auth service", Type: "task"},
			"gd-plan1": {ID: "gd-plan1", Title: "Auth service plan", Type: "epic"},
			"gd-mr1": {
				ID:    "gd-mr1",
				Title: "feat: auth service skeleton",
				Type:  "merge-request",
				Fields: map[string]string{
					"mr_url":   "https://gitlab.com/x/y/-/merge_requests/1",
					"mr_state": "opened",
				},
			},
		},
		deps: map[string][]beadsapi.Dependency{
			// agent --produces--> task --produces--> MR (two-hop lineage).
			"gd-agent1": {{BeadID: "gd-agent1", DependsOnID: "gd-taskP", Type: "produces"}},
			"gd-taskP":  {{BeadID: "gd-taskP", DependsOnID: "gd-mr1", Type: "produces"}},
		},
		revDeps: map[string][]beadsapi.Dependency{
			// task1 is blocked by the decision (decision→work blocks edge points
			// task→decision, so the task shows up as a reverse dep of type blocks).
			"gd-dec1": {{BeadID: "gd-task1", DependsOnID: "gd-dec1", Type: "blocks"}},
		},
	}
}

func TestDecisionDetail_RendersContextBlock(t *testing.T) {
	f := decisionDetailFake()
	// The intermediate produces task must exist for MRBeadsForAgent's second hop.
	f.beads["gd-taskP"] = &beadsapi.BeadDetail{ID: "gd-taskP", Title: "produce", Type: "task"}
	// slackWorkspaceDomain must be set for the Slack deep-link to render.
	srv := NewServer(f, testLogger(), "", "", "", nil)
	srv.slackWorkspaceDomain = "acme"
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	handler := edgeAuth(mux)

	req := httptest.NewRequest("GET", "/decisions/gd-dec1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d\nbody: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{
		"Pick auth approach",                           // page title / header
		`id="decision-context"`,                        // context sidebar present
		`class="decision-ctx-block"`,                   // context block, above the options (gd-PNtilz1bgs)
		"sessions need a shared store",                 // context text rendered
		"gd-task1",                                     // blocks-task link
		`/plans/gd-plan1`,                              // owning-plan link
		"https://gitlab.com/x/y/-/merge_requests/1",    // related MR
		"acme.slack.com/archives/C123",                 // slack deep-link
		"Reflagged",                                    // reflag history
		"user pinged again",                            // reflag reason
		`id="decision-reflag-form"`,                    // reflag affordance (authed)
		`class="chip chip-type decision-resolve-chip"`, // resolve chips reused
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in decision-detail render", want)
		}
	}
}

func TestDecisionDetail_ReflagFormHiddenWhenAnonymous(t *testing.T) {
	f := decisionDetailFake()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/decisions/gd-dec1", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if body := w.Body.String(); strings.Contains(body, `id="decision-reflag-form"`) {
		t.Error("reflag form should be hidden for anonymous viewer")
	}
}

func TestDecisionDetail_ResolvedDecisionHidesActions(t *testing.T) {
	f := decisionDetailFake()
	dec := f.beads["gd-dec1"]
	dec.Status = "closed"
	dec.Fields["chosen"] = "jwt"
	dec.Fields["responded_by"] = "matt@example.com"
	dec.Fields["responded_at"] = "2026-08-01T10:00:00Z"

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/decisions/gd-dec1", nil)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "banner-resolved") {
		t.Error("expected resolved banner on closed decision")
	}
	if strings.Contains(body, `id="decision-reflag-form"`) {
		t.Error("reflag form should be hidden on a resolved decision")
	}
	if strings.Contains(body, `class="chip chip-type decision-resolve-chip"`) {
		t.Error("resolve chips should be hidden on a resolved decision")
	}
}

func TestDecisionDetail_NonDecisionRedirectsToBeadShow(t *testing.T) {
	f := newFake() // gd-aaa is a task, not a decision
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/decisions/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/gd-aaa" {
		t.Errorf("redirect Location = %q, want /gd-aaa", loc)
	}
}

func TestDecisionDetail_NotFound(t *testing.T) {
	f := newFake()
	mux := mkServer(t, f)

	req := httptest.NewRequest("GET", "/decisions/gd-nope", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}
