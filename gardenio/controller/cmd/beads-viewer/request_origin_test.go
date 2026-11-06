package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// originFixtureFake returns a fake daemon seeded with an agent bead, a task
// bead and a plan bead so the origin resolver has titles to find.
func originFixtureFake() *fakeDaemon {
	f := newFake()
	f.beads["gd-agent1"] = &beadsapi.BeadDetail{ID: "gd-agent1", Type: "agent", Title: "crew-gardenio-7", Status: "open"}
	f.beads["gd-task1"] = &beadsapi.BeadDetail{ID: "gd-task1", Type: "task", Title: "Rename gardener to gardener", Status: "in_progress"}
	f.beads["gd-plan1"] = &beadsapi.BeadDetail{ID: "gd-plan1", Type: "epic", Title: "Gardener rollout", Status: "open", Labels: []string{"plan"}}
	return f
}

func TestOriginRefs_DecisionVsEscalationAgentSpelling(t *testing.T) {
	dec := &beadsapi.BeadDetail{Type: "decision", CreatedBy: "crew-x", Fields: map[string]string{
		"requesting_agent_bead_id": "gd-agent1",
		"requested_by":             "crew-gardenio-7",
		"requesting_task":          "gd-task1",
		"requesting_plan":          "gd-plan1",
	}}
	agentID, agentName, taskID, planID := originRefs(dec)
	if agentID != "gd-agent1" || agentName != "crew-gardenio-7" || taskID != "gd-task1" || planID != "gd-plan1" {
		t.Errorf("decision refs = %q %q %q %q", agentID, agentName, taskID, planID)
	}

	// gb escalation create stamps the agent BEAD ID into requesting_agent.
	esc := &beadsapi.BeadDetail{Type: "escalation", Fields: map[string]string{
		"requesting_agent": "gd-agent1",
		"requesting_task":  "gd-task1",
	}}
	agentID, agentName, taskID, planID = originRefs(esc)
	if agentID != "gd-agent1" || agentName != "" || taskID != "gd-task1" || planID != "" {
		t.Errorf("escalation (bead id) refs = %q %q %q %q", agentID, agentName, taskID, planID)
	}

	// Legacy escalations carried a bare agent name; it must stay a name.
	legacy := &beadsapi.BeadDetail{Type: "escalation", Fields: map[string]string{"requesting_agent": "agent-7"}}
	agentID, agentName, _, _ = originRefs(legacy)
	if agentID != "" || agentName != "agent-7" {
		t.Errorf("legacy escalation refs = agentID %q agentName %q", agentID, agentName)
	}

	// A decision with no requested_by falls back to created_by.
	bare := &beadsapi.BeadDetail{Type: "decision", CreatedBy: "gardener-bot", Fields: map[string]string{}}
	if _, name, _, _ := originRefs(bare); name != "gardener-bot" {
		t.Errorf("bare decision agentName = %q, want created_by", name)
	}
}

func TestResolveRequestOrigins_OneBatchCallResolvesTitles(t *testing.T) {
	f := originFixtureFake()
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	beads := []*beadsapi.BeadDetail{
		{ID: "gd-dec1", Type: "decision", Fields: map[string]string{
			"requesting_agent_bead_id": "gd-agent1", "requested_by": "crew-gardenio-7",
			"requesting_task": "gd-task1", "requesting_plan": "gd-plan1",
		}},
		{ID: "gd-esc1", Type: "escalation", Fields: map[string]string{
			"requesting_agent": "gd-agent1", "requesting_task": "gd-missing",
		}},
		{ID: "gd-none", Type: "decision", Fields: map[string]string{}},
	}
	origins := srv.resolveRequestOrigins(context.Background(), beads)

	if f.batchGetBeadsCalls != 1 {
		t.Errorf("BatchGetBeads calls = %d, want exactly 1 for the whole list", f.batchGetBeadsCalls)
	}
	if f.getBeadCalls != 0 {
		t.Errorf("GetBead calls = %d, want 0 (no per-row N+1)", f.getBeadCalls)
	}

	d := origins["gd-dec1"]
	if d.Agent == nil || d.Agent.ID != "gd-agent1" || d.Agent.Label() != "crew-gardenio-7" {
		t.Errorf("decision agent = %+v", d.Agent)
	}
	if d.Task == nil || d.Task.Label() != "Rename gardener to gardener" {
		t.Errorf("decision task = %+v", d.Task)
	}
	if d.Plan == nil || d.Plan.Label() != "Gardener rollout" {
		t.Errorf("decision plan = %+v", d.Plan)
	}

	e := origins["gd-esc1"]
	if e.Agent == nil || e.Agent.Label() != "crew-gardenio-7" {
		t.Errorf("escalation agent should resolve the bead id to its name, got %+v", e.Agent)
	}
	// A batch miss keeps the id so the anchor still resolves to /{id}.
	if e.Task == nil || e.Task.ID != "gd-missing" || e.Task.Title != "" || e.Task.Label() != "gd-missing" {
		t.Errorf("missing task link = %+v, want id-only", e.Task)
	}
	if e.Plan != nil {
		t.Errorf("escalation plan = %+v, want nil", e.Plan)
	}

	if origins["gd-none"].Any() {
		t.Errorf("bead with no refs should have an empty origin, got %+v", origins["gd-none"])
	}
}

func TestResolveRequestOrigins_BatchErrorDegradesToIDs(t *testing.T) {
	f := originFixtureFake()
	f.batchGetBeadsErr = errors.New("daemon down")
	srv := NewServer(f, slog.Default(), "", "", "", nil)
	o := srv.resolveRequestOrigin(context.Background(), &beadsapi.BeadDetail{ID: "gd-dec1", Type: "decision", Fields: map[string]string{
		"requesting_agent_bead_id": "gd-agent1", "requested_by": "crew-gardenio-7", "requesting_task": "gd-task1",
	}})
	if o.Agent == nil || o.Agent.Label() != "crew-gardenio-7" {
		t.Errorf("agent should fall back to the stamped name, got %+v", o.Agent)
	}
	if o.Task == nil || o.Task.Label() != "gd-task1" {
		t.Errorf("task should fall back to the id, got %+v", o.Task)
	}
	if !o.WithoutAgent().Any() || o.WithoutAgent().Agent != nil {
		t.Errorf("WithoutAgent = %+v", o.WithoutAgent())
	}
}

func TestHandleDecisions_RendersOriginAndDetails(t *testing.T) {
	f := originFixtureFake()
	f.decisionResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID: "gd-dec1", Title: "Pick a state backend", Type: "decision", Status: "open",
			Labels: []string{"project:gardenio"}, CreatedAt: time.Now().Add(-time.Hour), CreatedBy: "crew-gardenio-7",
			Fields: map[string]string{
				"prompt":                   "Choose a TF state backend.",
				"context":                  "I tried the S3 bucket but the **lock table** does not exist yet.",
				"requested_by":             "crew-gardenio-7",
				"requesting_agent_bead_id": "gd-agent1",
				"requesting_task":          "gd-task1",
				"requesting_plan":          "gd-plan1",
			},
		}},
		Total: 1,
	}
	mux := mkServer(t, f)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/decisions", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		// agent by name, linked to its bead
		`requested by <a href="/gd-agent1"`, `>crew-gardenio-7</a>`,
		// task by title, plan by title
		`for <a href="/gd-task1">Rename gardener to gardener</a>`,
		`plan <a href="/plans/gd-plan1">Gardener rollout</a>`,
		// inline details disclosure, open by default, carries the agent's
		// context block as markdown (gd-PNtilz1bgs)
		`<details class="bead-row-details" open>`, `class="decision-ctx-block"`, "<strong>lock table</strong>",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("decisions page missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, "requested by crew-gardenio-7</span>") {
		t.Errorf("agent should be linked, not plain text")
	}
}

func TestHandleDecisions_NoOriginKeepsPlainRequestedBy(t *testing.T) {
	f := newFake()
	f.decisionResp = &beadsapi.ListBeadsResult{
		Beads: []*beadsapi.BeadDetail{{
			ID: "gd-dec2", Title: "Defer the cutover", Type: "decision", Status: "open",
			CreatedAt: time.Now(), Fields: map[string]string{"requested_by": "alice", "prompt": "short"},
		}},
		Total: 1,
	}
	mux := mkServer(t, f)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/decisions", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "requested by alice") {
		t.Errorf("plain requested-by missing\nbody=%s", body)
	}
	if strings.Contains(body, `<details class="bead-row-details">`) {
		t.Errorf("no details disclosure expected when prompt fits and there is no context")
	}
	if f.batchGetBeadsCalls != 0 {
		t.Errorf("BatchGetBeads calls = %d, want 0 when no row carries an origin ref", f.batchGetBeadsCalls)
	}
}

func TestHandleEscalations_RendersOriginByNameAndTitle(t *testing.T) {
	f := originFixtureFake()
	long := strings.Repeat("The controller SA lacks DeleteVolume. ", 12)
	f.escalationsResp = []beadsapi.EscalationDetail{
		{Issue: makeEscBead("gd-esc1", "Grant ec2:DeleteVolume", "aws_iam", "internal", "open", time.Now().Add(-time.Hour), map[string]string{
			"requesting_agent": "gd-agent1", // bead id, as gb escalation create stamps it
			"requesting_task":  "gd-task1",
			"requesting_plan":  "gd-plan1",
			"context":          long,
		})},
	}
	srv := NewServer(f, slog.Default(), "", "http://test", "", nil)
	rec := httptest.NewRecorder()
	srv.handleEscalations(rec, httptest.NewRequest(http.MethodGet, "/escalations", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`agent <a href="/gd-agent1"`, `>crew-gardenio-7</a>`,
		`for <a href="/gd-task1">Rename gardener to gardener</a>`,
		`plan <a href="/plans/gd-plan1">Gardener rollout</a>`,
		`<details class="bead-row-details">`, "Why this needs a human",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("escalations page missing %q\nbody=%s", want, body)
		}
	}
	if strings.Contains(body, ">gd-agent1</a>") {
		t.Errorf("escalation row should show the agent NAME as the link text, not the bare bead id\nbody=%s", body)
	}
}

func TestBeadShow_EscalationCardLinksOriginByName(t *testing.T) {
	f := originFixtureFake()
	f.beads["gd-esc"] = &beadsapi.BeadDetail{
		ID: "gd-esc", Type: "escalation", Kind: "data", Title: "Merge MR", Status: "open",
		Fields: map[string]string{
			"operation_type":   "gitlab_mr_merge",
			"requesting_agent": "gd-agent1",
			"requesting_task":  "gd-task1",
			"requesting_plan":  "gd-plan1",
		},
	}
	srv := NewServer(f, slog.Default(), "", "http://test", "", nil)
	rec := httptest.NewRecorder()
	edgeAuthMux(srv).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/gd-esc", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"escalation-origin",
		`<a href="/gd-agent1" class="agent-link" data-agent="crew-gardenio-7">crew-gardenio-7</a>`,
		`<a href="/gd-task1">Rename gardener to gardener</a>`,
		`<a href="/plans/gd-plan1">Gardener rollout</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("escalation card missing %q\nbody=%s", want, body)
		}
	}
}

func TestBeadShow_DecisionCardLinksOriginAndDetailPage(t *testing.T) {
	f := originFixtureFake()
	f.beads["gd-dec"] = &beadsapi.BeadDetail{
		ID: "gd-dec", Type: "decision", Title: "Pick a backend", Status: "open", CreatedBy: "crew-gardenio-7",
		Fields: map[string]string{
			"prompt":                   "Which backend?",
			"requested_by":             "crew-gardenio-7",
			"requesting_agent_bead_id": "gd-agent1",
			"requesting_task":          "gd-task1",
			"options":                  `[{"id":"a","short":"A","artifact_type":"plan"}]`,
		},
	}
	srv := NewServer(f, slog.Default(), "", "http://test", "", nil)
	rec := httptest.NewRecorder()
	edgeAuthMux(srv).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/gd-dec", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`Requested by <strong><a href="/gd-agent1"`, `>crew-gardenio-7</a></strong>`,
		`for <a href="/gd-task1">Rename gardener to gardener</a>`,
		`<a href="/decisions/gd-dec">decision page &rsaquo;</a>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("decision card missing %q\nbody=%s", want, body)
		}
	}
}
