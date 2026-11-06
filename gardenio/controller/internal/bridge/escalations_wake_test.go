package bridge

import (
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"gardenio/controller/internal/agentwake"
	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/podmanager"
)

// TestEscalations_HandleClosed_RespawnThawsFrozenRequester: the escalation
// follow-through now shares agentwake.Wake with the decision resume
// (gd-T7aFA3tFoW), so a requester frozen at agent_state=stopping is thawed:
// its open bead is closed (or SpawnAgent's name dedup returns it and nothing
// happens) and the same-name spawn is pinned to runsc.
func TestEscalations_HandleClosed_RespawnThawsFrozenRequester(t *testing.T) {
	client := newMockEscalationBeadClient()
	client.beads["gd-agent-req"] = &beadsapi.BeadDetail{
		ID:     "gd-agent-req",
		Type:   "agent",
		Status: "open",
		Fields: map[string]string{
			"agent":         "test-agent",
			"agent_state":   "stopping",
			"runtime_class": "runsc",
			"project":       "gardenio",
			"task_id":       "gd-task-1",
			"role":          "crew",
		},
	}
	esc := NewEscalations(EscalationsConfig{Daemon: client, Logger: slog.Default()})
	data, _ := json.Marshal(map[string]any{"bead": BeadEvent{
		ID:   "gd-esc-frozen",
		Type: "escalation",
		Fields: map[string]string{
			"operation_type":   "github_pr_merge",
			"result_status":    "success",
			"requesting_agent": "gd-agent-req",
			"requesting_task":  "gd-task-1",
		},
	}})

	esc.handleClosed(context.Background(), data)

	client.mu.Lock()
	defer client.mu.Unlock()
	if _, ok := client.closedBeads["gd-agent-req"]; !ok {
		t.Errorf("frozen requester bead not closed before the thaw spawn; closed=%v", client.closedBeads)
	}
	if len(client.spawnedAgents) != 1 || client.spawnedAgents[0] != "test-agent" {
		t.Fatalf("spawned %v, want the same-name thaw spawn", client.spawnedAgents)
	}
	if rc := client.spawnedAgentFields[0]["runtime_class"]; rc != agentwake.RuntimeClassRunsc {
		t.Errorf("runtime_class = %q, want runsc so the snapshot thaws", rc)
	}
}

func TestAgentwakeRuntimeClassMatchesPodmanager(t *testing.T) {
	if agentwake.RuntimeClassRunsc != podmanager.RuntimeClassRunsc {
		t.Errorf("agentwake.RuntimeClassRunsc = %q, podmanager.RuntimeClassRunsc = %q",
			agentwake.RuntimeClassRunsc, podmanager.RuntimeClassRunsc)
	}
}
