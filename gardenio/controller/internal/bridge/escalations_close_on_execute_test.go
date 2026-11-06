package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestEscalations_HandleClosed_RespawnsWithoutCloseOnExecute pins the shape of
// gd-XmSjwrrhIc (gd-MgicCcY2rj): a crew filed a window_mint escalation with
// --requesting-task but no --close-on-execute and parked; the operator closed
// it partial. The follow-through respawn must not depend on the close-on-execute
// opt-in, which only decides whether the daemon closes the task.
func TestEscalations_HandleClosed_RespawnsWithoutCloseOnExecute(t *testing.T) {
	for _, status := range []string{"success", "partial"} {
		client := newMockEscalationBeadClient()
		client.beads["gd-agent-req"] = &beadsapi.BeadDetail{
			ID:     "gd-agent-req",
			Type:   "agent",
			Status: "closed",
			Fields: map[string]string{
				"agent":          "pw6-crew",
				"agent_state":    "done",
				"stop_requested": "true",
				"project":        "gardenio",
				"task_id":        "gd-step",
				"role":           "crew",
			},
		}
		esc := NewEscalations(EscalationsConfig{Daemon: client, Logger: slog.Default()})

		data, _ := json.Marshal(map[string]any{"bead": BeadEvent{
			ID:   "gd-esc-mint",
			Type: "escalation",
			Fields: map[string]string{
				"operation_type":        "window_mint",
				"result_status":         status,
				"result":                `{"output":"minted; screenshots left to the crew"}`,
				"requesting_agent":      "gd-agent-req",
				"requesting_task":       "gd-step",
				"close_requesting_task": "false",
			},
		}})
		esc.handleClosed(context.Background(), data)

		client.mu.Lock()
		n := len(client.spawnedAgents)
		var task string
		if n > 0 {
			task = client.spawnedAgentFields[0]["task_id"]
		}
		client.mu.Unlock()
		if n != 1 {
			t.Fatalf("%s: spawned %d agents, want 1 follow-through crew", status, n)
		}
		if task != "gd-step" {
			t.Fatalf("%s: follow-through task_id = %q, want gd-step", status, task)
		}
	}
}
