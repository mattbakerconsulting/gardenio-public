package bridge

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// gd-H17IlFclcl: resolving a plan-gardener's decision respawned it with
// spawned_by=<prior agent bead id>, which dropped the respawn out of the
// spawned_by:trigger:<id> count the engine's single-live-run gate reads. The
// decision had been holding the trigger's preflight (blocked_by), so 23 s later
// the trigger fired a second session for the same plan, and both filed the same
// escalation. spawned_by must stay the prior bead (spawn authz admits the
// privileged resume only as a same-role continuation of it), so the respawn
// carries trigger_lease_id and the gates count that too.

// resolveGardenerDecision resolves a decision filed by a trigger-fired gardener
// and returns the decision-resume respawn the bridge minted.
func resolveGardenerDecision(t *testing.T) *beadsapi.BeadDetail {
	t.Helper()
	daemon := newMockDaemon()
	prior := &beadsapi.BeadDetail{
		ID:     "gd-alqw",
		Type:   "agent",
		Status: "closed",
		Fields: map[string]string{
			"agent":           "gardenio-plan-gardener-for-gd-plan",
			"agent_state":     "done",
			"project":         "gardenio",
			"role":            "plan-gardener",
			"task_id":         "gd-plan",
			"plan_id":         "gd-plan",
			"operated_by":     "oidc:owner@acme.example",
			"spawned_by":      "trigger:gd-trig",
			"trigger_id":      "gd-trig",
			"schedule_id":     "gd-trig",
			"lineage_root_id": "gd-root",
		},
	}
	daemon.beads["gardenio-plan-gardener-for-gd-plan"] = prior
	daemon.beads["gd-alqw"] = prior

	d := NewDecisions(DecisionsConfig{Daemon: daemon, Notifier: &mockNotifier{}, Logger: slog.Default()})
	d.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-dec",
		Type:     "decision",
		Assignee: "gardenio-plan-gardener-for-gd-plan",
		Labels:   []string{"project:gardenio"},
		Fields: map[string]string{
			"prompt":                   "Extend the gardener?",
			"chosen":                   "extend",
			"requesting_agent_bead_id": "gd-alqw",
		},
	}))

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			return b
		}
	}
	t.Fatal("expected a decision-resume respawn")
	return nil
}

func TestDecisions_ResumeOfTriggerGardenerKeepsTriggerLease(t *testing.T) {
	resp := resolveGardenerDecision(t)
	want := map[string]string{
		// The continuation edge spawn authz reads; never trigger:<id>.
		"spawned_by":       "gd-alqw",
		"trigger_lease_id": "gd-trig",
		"operated_by":      "oidc:owner@acme.example",
	}
	for k, v := range want {
		if got := resp.Fields[k]; got != v {
			t.Errorf("respawn %s = %q, want %q", k, got, v)
		}
	}
	// A resume is not a scheduled run: the iteration-owner keys that feed the
	// engine's run accounting must not be carried.
	for _, k := range []string{"trigger_id", "schedule_id"} {
		if got := resp.Fields[k]; got != "" {
			t.Errorf("respawn must not carry %s (run accounting), got %q", k, got)
		}
	}
}

// TestDecisions_ResumeOfTriggerGardenerHoldsScheduleFire is the incident: the
// decision closes, the bridge respawns the gardener, then the trigger's next
// window fires. Expect one session (the respawn), not two.
func TestDecisions_ResumeOfTriggerGardenerHoldsScheduleFire(t *testing.T) {
	resp := resolveGardenerDecision(t)
	// The real daemon opens the respawn; the decisions mock leaves status unset.
	resp.Status = "open"
	resp.CreatedAt = time.Now().UTC()
	resp.UpdatedAt = time.Now().UTC()

	daemon := &mockTriggerDaemon{
		beads: []*beadsapi.BeadDetail{
			{ID: "gd-trig", Type: "trigger", Status: "open", Title: "plan gardener"},
			resp,
		},
	}
	engine := NewTriggerEngine(TriggerEngineConfig{Daemon: daemon, Logger: slog.Default()})
	engine.RegisterSource(&mockTriggerSource{
		name: "mock",
		triggers: []TriggerDef{{
			ID: "gd-trig", Title: "plan gardener", Source: "mock",
			Action: TriggerActionSpawnAgent, Project: "gardenio", Role: "plan-gardener",
			Prompt: "garden the plan", MaxConcurrent: 3, LastChecked: pastLastChecked(),
			Gardener: &GardenerSpec{RunBudget: 64, MaxRuns: 64, RunsSpawned: 43, LastAgentID: "gd-alqw"},
		}},
		matches: map[string][]TriggerMatch{"gd-trig": {{Key: "run-44", Title: "window"}}},
	})

	engine.Evaluate(context.Background())

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	if n := len(daemon.spawnedNames); n != 0 {
		t.Fatalf("schedule fired %d session(s) beside the live decision-resume respawn; want 0", n)
	}
}

func TestStampResumeLineage(t *testing.T) {
	cases := []struct {
		name      string
		prior     map[string]string
		wantLease string
	}{
		{"trigger-fired gardener", map[string]string{"spawned_by": "trigger:gd-trig", "trigger_id": "gd-trig"}, "gd-trig"},
		// A gardener resumed once and resumed again keeps the lease.
		{"resume of a resume", map[string]string{"spawned_by": "gd-alqw", triggerLeaseField: "gd-trig"}, "gd-trig"},
		{"crew", map[string]string{"spawned_by": "gd-captain"}, ""},
		{"successor edge", map[string]string{"spawned_by": "successor:gd-old"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := map[string]string{}
			stampResumeLineage(got, &beadsapi.BeadDetail{ID: "gd-prior", Fields: tc.prior})
			if got["spawned_by"] != "gd-prior" {
				t.Errorf("spawned_by = %q, want the prior bead gd-prior", got["spawned_by"])
			}
			if got[triggerLeaseField] != tc.wantLease {
				t.Errorf("%s = %q, want %q", triggerLeaseField, got[triggerLeaseField], tc.wantLease)
			}
			for _, k := range []string{"trigger_id", "schedule_id"} {
				if v, ok := got[k]; ok {
					t.Errorf("respawn must not carry %s, got %q", k, v)
				}
			}
		})
	}
}
