package main

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
)

// TestAgentBucketMatchesGardenhub pins the copies the farm's gardeners[] uses
// (internal/gardenhub AgentBucket, IsPoolShell and GardenerStaleAfter, design:
// gardenio/docs/homestead-agents-as-people.md section 2.2) to agentStateBucket
// and isUnengagedPrewarmedAgent here. If either copy drifts, this goes red.
func TestAgentBucketMatchesGardenhub(t *testing.T) {
	states := []string{
		"spawning", "working", "yielding", "failed", "rate_limited", "budget_exceeded",
		"idle", "assigning", "queued", "prewarmed", "done", "stopping", "stop_requested", "expired",
		agentStateDormant, "", "no-such-state",
	}
	for s := range agentStateAlive {
		states = append(states, s)
	}
	for s := range agentStateStuck {
		states = append(states, s)
	}
	for _, s := range states {
		if got, want := gardenhub.AgentBucket(s), agentStateBucket(s); got != want {
			t.Errorf("agent_state %q: gardenhub.AgentBucket = %q, agentStateBucket = %q", s, got, want)
		}
	}
	// The farm's stale reason (gd-TvJosTKfAt) uses the /agents threshold.
	if gardenhub.GardenerStaleAfter != agentStaleThreshold {
		t.Errorf("gardenhub.GardenerStaleAfter = %v, agentStaleThreshold = %v", gardenhub.GardenerStaleAfter, agentStaleThreshold)
	}

	agent := func(title, state string, fields map[string]string) *beadsapi.BeadDetail {
		b := &beadsapi.BeadDetail{ID: "gd-pin", Type: "agent", Status: "open", Title: title, Fields: map[string]string{"agent_state": state}}
		for k, v := range fields {
			b.Fields[k] = v
		}
		return b
	}
	cases := map[string]*beadsapi.BeadDetail{
		"nil":                        nil,
		"prewarmed":                  agent("prewarmed-a", agentStatePrewarmed, map[string]string{"agent": "prewarmed-a"}),
		"prewarmed, any name":        agent("crew-x", agentStatePrewarmed, nil),
		"queued shell":               agent("prewarmed-b", agentStateQueued, map[string]string{"agent": "prewarmed-b"}),
		"idle shell, title only":     agent("prewarmed-c", "idle", nil),
		"assigned shell with a task": agent("prewarmed-d", "idle", map[string]string{"agent": "prewarmed-d", "task_id": "gd-task"}),
		"assigned shell with thread": agent("prewarmed-e", agentStateQueued, map[string]string{"agent": "prewarmed-e", "slack_thread_ts": "1.2"}),
		"working pool name":          agent("prewarmed-f", "working", map[string]string{"agent": "prewarmed-f"}),
		"idle crew":                  agent("crew-y", "idle", map[string]string{"agent": "crew-y"}),
		"queued crew":                agent("crew-z", agentStateQueued, nil),
	}
	for name, b := range cases {
		if got, want := gardenhub.IsPoolShell(b), isUnengagedPrewarmedAgent(b); got != want {
			t.Errorf("%s: gardenhub.IsPoolShell = %v, isUnengagedPrewarmedAgent = %v", name, got, want)
		}
	}
}
