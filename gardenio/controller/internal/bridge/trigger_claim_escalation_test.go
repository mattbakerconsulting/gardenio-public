package bridge

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/scheduler"
)

// The engine claims the escalation a bead_event fire is about for the agent it
// spawned (gd-1yeWlbJUJi); the daemon's claim-for decides whether it may.

type claimingTriggerDaemon struct {
	*mockTriggerDaemon
	claims [][2]string // (escalation, agent) per ClaimEscalationFor call
	err    error
}

func (d *claimingTriggerDaemon) ClaimEscalationFor(_ context.Context, id, agentID string) (*beadsapi.BeadDetail, error) {
	d.claims = append(d.claims, [2]string{id, agentID})
	return nil, d.err
}

func TestActionSpawnAgent_ClaimsSourceEscalation(t *testing.T) {
	trig := TriggerDef{ID: "gd-trig", Title: "coordinator", Source: "multi", Action: TriggerActionSpawnAgent,
		Project: "gardenio", Role: "escalation-coordinator", Prompt: "run"}
	beadEvent := func(id string) TriggerMatch {
		return TriggerMatch{Key: "0:approved:updated:" + id,
			Fields:       map[string]string{"multi_entry_kind": scheduler.TriggerKindBeadEvent},
			TemplateVars: map[string]string{"{{event_bead_id}}": id}}
	}
	for _, tc := range []struct {
		name string
		m    TriggerMatch
		err  error
		want string // escalation claimed, "" for none
	}{
		{"multi bead_event on an escalation", beadEvent("gd-esc1"), nil, "gd-esc1"},
		{"bead source on an escalation", TriggerMatch{Key: "gd-esc1:updated", Fields: map[string]string{"source_bead_id": "gd-esc1"}}, nil, "gd-esc1"},
		{"claim refused: the spawn still stands", beadEvent("gd-esc1"), errors.New("403"), "gd-esc1"},
		{"bead_event on a task", beadEvent("gd-task1"), nil, ""},
		{"cron entry", TriggerMatch{Key: "2:backstop:2026-11-05T20:00", Fields: map[string]string{"multi_entry_kind": scheduler.TriggerKindCron}}, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &claimingTriggerDaemon{mockTriggerDaemon: &mockTriggerDaemon{beads: []*beadsapi.BeadDetail{
				{ID: "gd-esc1", Type: "escalation", Status: "open"},
				{ID: "gd-task1", Type: "task", Status: "open"},
			}}, err: tc.err}
			engine := NewTriggerEngine(TriggerEngineConfig{Daemon: d, Logger: slog.Default()})
			agentID, err := engine.actionSpawnAgent(context.Background(), trig, tc.m)
			if err != nil {
				t.Fatalf("spawn: %v", err)
			}
			switch {
			case tc.want == "" && len(d.claims) != 0:
				t.Fatalf("claims = %v, want none", d.claims)
			case tc.want != "" && (len(d.claims) != 1 || d.claims[0] != [2]string{tc.want, agentID}):
				t.Fatalf("claims = %v, want one claim of %s for %s", d.claims, tc.want, agentID)
			}
		})
	}
}
