package bridge

import (
	"context"
	"log/slog"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// respawnForDecisionWithFields resolves a decision filed by an agent whose bead
// carries agentFields and returns the decision-resume bead it respawned.
func respawnForDecisionWithFields(t *testing.T, agentFields map[string]string) *beadsapi.BeadDetail {
	t.Helper()
	daemon := newMockDaemon()
	fields := map[string]string{
		"agent":       "gardenio-crew-route",
		"agent_state": "done",
		"project":     "gardenio",
		"task_id":     "gd-task-route",
		"role":        "crew",
	}
	for k, v := range agentFields {
		fields[k] = v
	}
	agent := &beadsapi.BeadDetail{ID: "bd-route-1", Type: "agent", Status: "closed", Fields: fields}
	daemon.beads["gardenio-crew-route"] = agent
	daemon.beads["bd-route-1"] = agent

	d := NewDecisions(DecisionsConfig{Daemon: daemon, Notifier: &mockNotifier{}, Logger: slog.Default()})
	d.handleClosed(context.Background(), marshalSSEBeadPayload(BeadEvent{
		ID:       "dec-route-1",
		Type:     "decision",
		Assignee: "gardenio-crew-route",
		Labels:   []string{"project:gardenio"},
		Fields: map[string]string{
			"prompt":                   "A or B?",
			"chosen":                   "A",
			"requesting_agent_bead_id": "bd-route-1",
		},
	}))

	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	for _, b := range daemon.beads {
		if b.Type == "agent" && b.Fields["spawn_source"] == "decision-resume" {
			return b
		}
	}
	t.Fatal("expected a decision-resume agent bead to be spawned")
	return nil
}

// TestRespawnAgentForDecision_CarriesSlackRouting is the gd-jOHtHdkdIt guard:
// each Slack routing field on the requesting agent lands on the respawned bead
// with the same value. schedule_slack_channel and the inherited-spawn marker
// were dropped before, so a scheduled agent fell back to channel_roles and an
// inherited spawn channel became an explicit pin.
func TestRespawnAgentForDecision_CarriesSlackRouting(t *testing.T) {
	all := map[string]string{
		"slack_spawn_channel":                            "C0SPAWN",
		beadsapi.FieldSlackSpawnChannelInheritedFromRole: "plan-gardener",
		"schedule_slack_channel":                         "C0SCHED",
		"slack_thread_channel":                           "C0THREAD",
		"slack_thread_ts":                                "1792008122.869589",
	}
	for _, k := range beadsapi.SlackRoutingFields {
		if _, ok := all[k]; !ok {
			t.Fatalf("test fixture is missing routing field %s", k)
		}
	}
	for k, want := range all {
		t.Run(k, func(t *testing.T) {
			spawned := respawnForDecisionWithFields(t, map[string]string{k: want})
			if got := spawned.Fields[k]; got != want {
				t.Errorf("respawned %s = %q, want %q", k, got, want)
			}
		})
	}
	t.Run("all", func(t *testing.T) {
		spawned := respawnForDecisionWithFields(t, all)
		for k, want := range all {
			if got := spawned.Fields[k]; got != want {
				t.Errorf("respawned %s = %q, want %q", k, got, want)
			}
		}
	})
}

// TestRespawnAgentForDecision_NoSlackRoutingGainsNone: an agent with no Slack
// fields respawns with none.
func TestRespawnAgentForDecision_NoSlackRoutingGainsNone(t *testing.T) {
	spawned := respawnForDecisionWithFields(t, nil)
	for k, v := range spawned.Fields {
		if strings.HasPrefix(k, "slack_") || strings.Contains(k, "slack_channel") {
			t.Errorf("respawned bead gained routing field %s=%q from an agent with none", k, v)
		}
	}
}

// TestCacheAgentBeadDetail_ScheduleChannelIsExplicitPin: a carried-over bead
// with an inherited spawn channel AND a schedule channel resolves to the
// schedule channel on the bead-detail cache path too, not channel_roles —
// matching NotifyAgentSpawn and NotifyAgentState.
func TestCacheAgentBeadDetail_ScheduleChannelIsExplicitPin(t *testing.T) {
	const agent = "gardenio-crew-sched"
	daemon := newMockDaemon()
	seedProjectWithRoleMap(daemon, "gardenio", "C-primary", map[string]string{"C-crew": "crew"})
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)
	bot.channel = "C-default"

	bot.cacheAgentBeadDetail(agent, &beadsapi.BeadDetail{ID: "bd-sched-1", Type: "agent", Fields: map[string]string{
		"project":             "gardenio",
		"role":                "crew",
		"slack_spawn_channel": "C-spawn",
		beadsapi.FieldSlackSpawnChannelInheritedFromRole: "plan-gardener",
		"schedule_slack_channel":                         "C-sched",
	}})
	if got := bot.resolveChannel(agent); got != "C-sched" {
		t.Errorf("resolveChannel = %q, want C-sched (schedule channel is an explicit pin)", got)
	}
}
