package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// setupScheduleTestDaemon installs a test daemon whose GetBead returns the
// agent bead for agentID and the prior bead for priorID. Everything else 404s.
func setupScheduleTestDaemon(t *testing.T, agentBead, priorBead *mockBead) func() {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/beads/") && r.URL.RawQuery == "" {
			id := strings.TrimPrefix(r.URL.Path, "/v1/beads/")
			switch {
			case agentBead != nil && id == agentBead.ID:
				_ = json.NewEncoder(w).Encode(agentBead)
			case priorBead != nil && id == priorBead.ID:
				_ = json.NewEncoder(w).Encode(priorBead)
			default:
				http.Error(w, "not found", http.StatusNotFound)
			}
			return
		}
		http.Error(w, "unexpected request", http.StatusBadRequest)
	}))

	oldDaemon := daemon
	c, err := beadsapi.New(beadsapi.Config{HTTPAddr: srv.URL})
	if err != nil {
		t.Fatalf("failed to create test client: %v", err)
	}
	daemon = c

	return func() {
		daemon = oldDaemon
		srv.Close()
	}
}

func TestOutputScheduledLoopContext_NonScheduledAgent_Empty(t *testing.T) {
	// Agent bead with no schedule_id field — must emit nothing.
	agent := &mockBead{
		ID:     "gd-agent",
		Type:   "agent",
		Fields: map[string]string{},
	}
	cleanup := setupScheduleTestDaemon(t, agent, nil)
	defer cleanup()

	var buf bytes.Buffer
	outputScheduledLoopContext(&buf, "gd-agent")
	if buf.Len() != 0 {
		t.Errorf("expected empty output for non-scheduled agent, got: %q", buf.String())
	}
}

func TestOutputScheduledLoopContext_EmptyAgentID_Empty(t *testing.T) {
	// No agent ID resolved — must not call daemon, must emit nothing.
	var buf bytes.Buffer
	outputScheduledLoopContext(&buf, "")
	if buf.Len() != 0 {
		t.Errorf("expected empty output for empty agentID, got: %q", buf.String())
	}
}

func TestOutputScheduledLoopContext_FullBlock(t *testing.T) {
	// The prior run wrote BOTH an accomplishments self-report and a
	// forward-looking handoff. Only the handoff may be replayed — see
	// fetchPriorRunHandoff (ka-0000002280) for why.
	priorWrap, _ := json.Marshal(map[string]any{
		"accomplishments": "Fixed 2 panel queries. Great progress on the drift backlog.",
		"handoff_notes":   "Panel queries are fixed.\n1 remaining dashboard still fails to render.",
	})
	agent := &mockBead{
		ID:   "gd-agent",
		Type: "agent",
		Fields: map[string]string{
			"schedule_id":             "gd-sched-1",
			"schedule_title":          "Grafana dashboard cleanup",
			"schedule_cron":           "0 9 * * *",
			"schedule_runs_spawned":   "3",
			"schedule_runs_completed": "2",
			"schedule_max_runs":       "10",
			"schedule_terminal_goal":  "No open P1/P2 dashboard-drift bugs.",
			"schedule_prior_agent_id": "gd-prior",
		},
	}
	prior := &mockBead{
		ID:   "gd-prior",
		Type: "agent",
		Fields: map[string]string{
			"wrapup":       string(priorWrap),
			"close_reason": "goal_reached",
		},
	}
	cleanup := setupScheduleTestDaemon(t, agent, prior)
	defer cleanup()

	var buf bytes.Buffer
	outputScheduledLoopContext(&buf, "gd-agent")
	out := buf.String()

	checks := []string{
		"## Scheduled Loop Context",
		"run **3 of 10**",
		"Grafana dashboard cleanup",
		"gd-sched-1",
		"**Terminal goal:** No open P1/P2 dashboard-drift bugs.",
		"**Where the last run left off**",
		// The predecessor's bead id and how its run ENDED travel with the
		// claim, so the reader can check the record rather than only the
		// account of it.
		"gd-prior",
		"close_reason `goal_reached`",
		"> Panel queries are fixed.",
		"> 1 remaining dashboard still fails to render.",
		"**Runs remaining after this one:** 7",
		`gb stop --wrapup '{"goal_reached":true`,
		"`gb stop` **consumes one of your 10 runs.**",
		"`gb yield` **does NOT consume a run.**",
		"a run is an attempt to finish the goal",
	}
	for _, want := range checks {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n---\n%s", want, out)
		}
	}
}

func TestOutputScheduledLoopContext_UnlimitedMaxRuns(t *testing.T) {
	agent := &mockBead{
		ID:   "gd-agent",
		Type: "agent",
		Fields: map[string]string{
			"schedule_id":           "gd-sched-1",
			"schedule_title":        "Legacy cron",
			"schedule_runs_spawned": "42",
			"schedule_max_runs":     "0",
		},
	}
	cleanup := setupScheduleTestDaemon(t, agent, nil)
	defer cleanup()

	var buf bytes.Buffer
	outputScheduledLoopContext(&buf, "gd-agent")
	out := buf.String()

	if !strings.Contains(out, "run **42**") {
		t.Errorf("expected 'run **42**', got: %q", out)
	}
	if !strings.Contains(out, "`max_runs` is unlimited") {
		t.Errorf("expected unlimited note, got: %q", out)
	}
	// With no max, no "runs remaining" line.
	if strings.Contains(out, "Runs remaining after this one") {
		t.Errorf("unlimited schedule should NOT render 'Runs remaining', got: %q", out)
	}
	// Lifecycle block must still render with a generic budget label.
	if !strings.Contains(out, "consumes one of your run budget") {
		t.Errorf("expected generic budget label for unlimited, got: %q", out)
	}
}

func TestOutputScheduledLoopContext_NoPriorAgent_NoSummary(t *testing.T) {
	// First run of a schedule — no prior_agent_id means no prior-run summary.
	agent := &mockBead{
		ID:   "gd-agent",
		Type: "agent",
		Fields: map[string]string{
			"schedule_id":           "gd-sched-1",
			"schedule_title":        "First run",
			"schedule_runs_spawned": "1",
			"schedule_max_runs":     "5",
		},
	}
	cleanup := setupScheduleTestDaemon(t, agent, nil)
	defer cleanup()

	var buf bytes.Buffer
	outputScheduledLoopContext(&buf, "gd-agent")
	out := buf.String()

	if !strings.Contains(out, "run **1 of 5**") {
		t.Errorf("expected 'run **1 of 5**', got: %q", out)
	}
	if strings.Contains(out, "Where the last run left off") {
		t.Errorf("first run should NOT render prior-run summary, got: %q", out)
	}
}

func TestOutputScheduledLoopContext_PriorAgentMissingWrapup_NoSummary(t *testing.T) {
	// Prior agent exists but has no wrapup field — omit the summary block
	// rather than rendering an empty quote.
	agent := &mockBead{
		ID:   "gd-agent",
		Type: "agent",
		Fields: map[string]string{
			"schedule_id":             "gd-sched-1",
			"schedule_title":          "s",
			"schedule_runs_spawned":   "2",
			"schedule_max_runs":       "5",
			"schedule_prior_agent_id": "gd-prior",
		},
	}
	prior := &mockBead{
		ID:     "gd-prior",
		Type:   "agent",
		Fields: map[string]string{},
	}
	cleanup := setupScheduleTestDaemon(t, agent, prior)
	defer cleanup()

	var buf bytes.Buffer
	outputScheduledLoopContext(&buf, "gd-agent")
	if strings.Contains(buf.String(), "Where the last run left off") {
		t.Errorf("should NOT render the prior-run block when wrapup absent")
	}
}

func TestOutputScheduledLoopContext_FinalRun(t *testing.T) {
	agent := &mockBead{
		ID:   "gd-agent",
		Type: "agent",
		Fields: map[string]string{
			"schedule_id":           "gd-sched-1",
			"schedule_title":        "s",
			"schedule_runs_spawned": "5",
			"schedule_max_runs":     "5",
		},
	}
	cleanup := setupScheduleTestDaemon(t, agent, nil)
	defer cleanup()

	var buf bytes.Buffer
	outputScheduledLoopContext(&buf, "gd-agent")
	out := buf.String()
	if !strings.Contains(out, "Runs remaining after this one:** 0") {
		t.Errorf("final run should state 0 remaining, got: %q", out)
	}
	if !strings.Contains(out, "This is the final run") {
		t.Errorf("final run should note 'final run', got: %q", out)
	}
}

// TestOutputScheduledLoopContext_NeverReplaysAccomplishments pins the A3
// behavior change (ka-0000002280) as a property, not an incidental of the
// current preference order.
//
// This block used to inject `accomplishments` and ONLY accomplishments, which
// made it the single place in the system where a wrap-up field re-entered a
// later agent's context — and the field it chose was the one that is purely an
// agent's assessment of its own achievement, with no record behind it. Across a
// long-running scheduled loop that is the exact channel by which an unchecked
// self-report accretes into a premise (operator direction, 2026-08-16).
//
// A run that wrote ONLY accomplishments must therefore render nothing here.
func TestOutputScheduledLoopContext_NeverReplaysAccomplishments(t *testing.T) {
	priorWrap, _ := json.Marshal(map[string]any{
		"accomplishments": "Shipped the whole thing. Enormous progress. Best run yet.",
	})
	agent := &mockBead{
		ID:   "gd-agent",
		Type: "agent",
		Fields: map[string]string{
			"schedule_id":             "gd-sched-1",
			"schedule_prior_agent_id": "gd-prior",
		},
	}
	prior := &mockBead{
		ID:     "gd-prior",
		Type:   "agent",
		Fields: map[string]string{"wrapup": string(priorWrap)},
	}
	cleanup := setupScheduleTestDaemon(t, agent, prior)
	defer cleanup()

	var buf bytes.Buffer
	outputScheduledLoopContext(&buf, "gd-agent")
	out := buf.String()

	if strings.Contains(out, "Where the last run left off") {
		t.Errorf("must not render a prior-run block from accomplishments alone\n---\n%s", out)
	}
	for _, leaked := range []string{"Enormous progress", "Best run yet", "Shipped the whole thing"} {
		if strings.Contains(out, leaked) {
			t.Errorf("accomplishments text %q leaked into a successor's context\n---\n%s", leaked, out)
		}
	}
}

// TestOutputScheduledLoopContext_FallsBackToBlockers: a run that hit a wall and
// recorded it, but wrote no handoff, still passes the wall forward — the
// successor should not re-derive the same blocker from scratch.
func TestOutputScheduledLoopContext_FallsBackToBlockers(t *testing.T) {
	priorWrap, _ := json.Marshal(map[string]any{
		"accomplishments": "Investigated the failure.",
		"blockers":        "Terraform state lock held by another apply.",
	})
	agent := &mockBead{
		ID:   "gd-agent",
		Type: "agent",
		Fields: map[string]string{
			"schedule_id":             "gd-sched-1",
			"schedule_prior_agent_id": "gd-prior",
		},
	}
	prior := &mockBead{
		ID:     "gd-prior",
		Type:   "agent",
		Fields: map[string]string{"wrapup": string(priorWrap)},
	}
	cleanup := setupScheduleTestDaemon(t, agent, prior)
	defer cleanup()

	var buf bytes.Buffer
	outputScheduledLoopContext(&buf, "gd-agent")
	out := buf.String()

	if !strings.Contains(out, "> Terraform state lock held by another apply.") {
		t.Errorf("expected the blocker to carry forward\n---\n%s", out)
	}
	if strings.Contains(out, "Investigated the failure") {
		t.Errorf("accomplishments must not be rendered even as a fallback\n---\n%s", out)
	}
}
