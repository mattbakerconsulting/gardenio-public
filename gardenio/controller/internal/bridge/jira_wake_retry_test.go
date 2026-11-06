package bridge

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/internal/beadsapi"
)

// wakeTransientDaemonErr is a daemon 503 — retryable by isTransientDaemonError,
// so it drives the bounded-retry path in retryJiraWake.
func wakeTransientDaemonErr() error {
	return &beadsapi.APIError{StatusCode: http.StatusServiceUnavailable, Message: "daemon restarting"}
}

// TestJiraAgentResolver_LiveNudge_RetriesTransientThenSucceeds proves a
// momentary daemon/coop blip while nudging a LIVE assigned agent on a follow-up
// is absorbed by the bounded wake retry: the first nudge attempt returns a
// transient 503, the retry succeeds, the agent is woken rather than the
// follow-up silently dropped (epic ka-0000007401 — Slack wake-durability
// parity, GAP 3a ka-0000002721).
func TestJiraAgentResolver_LiveNudge_RetriesTransientThenSucceeds(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-1234-abcd": {
				ID:     "agent-bead-1",
				Title:  "jira-pe-1234-abcd",
				Fields: map[string]string{"agent": "jira-pe-1234-abcd", "coop_url": "http://10.0.0.9:8080"},
			},
		},
		spawnID: "new-agent-bead",
	}

	// Nudge override injects one transient failure then succeeds.
	var nudgeCalls int
	nudge := func(_ context.Context, _, _ string) error {
		nudgeCalls++
		if nudgeCalls == 1 {
			return wakeTransientDaemonErr()
		}
		return nil
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Logger: jiraResolverTestLogger(),
		Nudge:  nudge,
	})

	retriesBefore := testutil.ToFloat64(jiraFollowUpWakeRetries.WithLabelValues(jiraWakeStageNudge))
	failuresBefore := testutil.ToFloat64(jiraFollowUpWakeFailures.WithLabelValues(jiraWakeStageNudge))

	err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "follow up")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if nudgeCalls != 2 {
		t.Errorf("expected 2 nudge calls (fail + retry), got %d", nudgeCalls)
	}
	// A live agent that was successfully nudged must NOT be respawned.
	if len(daemon.getSpawned()) != 0 {
		t.Errorf("expected no spawn when live agent is eventually nudged, got %d", len(daemon.getSpawned()))
	}
	if got := testutil.ToFloat64(jiraFollowUpWakeRetries.WithLabelValues(jiraWakeStageNudge)) - retriesBefore; got != 1 {
		t.Errorf("expected 1 nudge retry increment, got %v", got)
	}
	if got := testutil.ToFloat64(jiraFollowUpWakeFailures.WithLabelValues(jiraWakeStageNudge)) - failuresBefore; got != 0 {
		t.Errorf("expected 0 nudge failure increments, got %v", got)
	}
}

// TestJiraAgentResolver_LiveNudge_TerminalFailureCounted proves that when the
// live-agent nudge exhausts its retries (every attempt transient), the terminal
// failure is counted on the metric and surfaced to the caller — so a stalled
// conversational loop is visible in a scrape, not just a pod log the agent
// cannot read.
func TestJiraAgentResolver_LiveNudge_TerminalFailureCounted(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-1234-abcd": {
				ID:     "agent-bead-1",
				Title:  "jira-pe-1234-abcd",
				Fields: map[string]string{"agent": "jira-pe-1234-abcd", "coop_url": "http://10.0.0.9:8080"},
			},
		},
	}

	var nudgeCalls int
	nudge := func(_ context.Context, _, _ string) error {
		nudgeCalls++
		return wakeTransientDaemonErr()
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Logger: jiraResolverTestLogger(),
		Nudge:  nudge,
	})

	retriesBefore := testutil.ToFloat64(jiraFollowUpWakeRetries.WithLabelValues(jiraWakeStageNudge))
	failuresBefore := testutil.ToFloat64(jiraFollowUpWakeFailures.WithLabelValues(jiraWakeStageNudge))

	err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "follow up")
	if err == nil {
		t.Fatal("expected a terminal error when every nudge attempt fails transiently")
	}
	// threadWriteMaxAttempts attempts consumed (initial + retries).
	if nudgeCalls != threadWriteMaxAttempts {
		t.Errorf("expected %d nudge attempts, got %d", threadWriteMaxAttempts, nudgeCalls)
	}
	// retries = attempts - 1 (the last attempt is not a retry, it's the give-up).
	if got := testutil.ToFloat64(jiraFollowUpWakeRetries.WithLabelValues(jiraWakeStageNudge)) - retriesBefore; got != float64(threadWriteMaxAttempts-1) {
		t.Errorf("expected %d nudge retry increments, got %v", threadWriteMaxAttempts-1, got)
	}
	if got := testutil.ToFloat64(jiraFollowUpWakeFailures.WithLabelValues(jiraWakeStageNudge)) - failuresBefore; got != 1 {
		t.Errorf("expected 1 nudge terminal-failure increment, got %v", got)
	}
}

// TestJiraAgentResolver_LiveNudge_DeterministicErrorNotRetried proves a
// non-transient (deterministic 4xx) nudge error is NOT retried — retrying a
// deterministic rejection only burns the synchronous webhook budget.
func TestJiraAgentResolver_LiveNudge_DeterministicErrorNotRetried(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-1234-abcd": {
				ID:     "agent-bead-1",
				Title:  "jira-pe-1234-abcd",
				Fields: map[string]string{"agent": "jira-pe-1234-abcd", "coop_url": "http://10.0.0.9:8080"},
			},
		},
	}

	var nudgeCalls int
	nudge := func(_ context.Context, _, _ string) error {
		nudgeCalls++
		return &beadsapi.APIError{StatusCode: http.StatusBadRequest, Message: "bad request"}
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Logger: jiraResolverTestLogger(),
		Nudge:  nudge,
	})

	retriesBefore := testutil.ToFloat64(jiraFollowUpWakeRetries.WithLabelValues(jiraWakeStageNudge))
	failuresBefore := testutil.ToFloat64(jiraFollowUpWakeFailures.WithLabelValues(jiraWakeStageNudge))

	err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "follow up")
	if err == nil {
		t.Fatal("expected the deterministic error to surface")
	}
	if nudgeCalls != 1 {
		t.Errorf("expected 1 nudge attempt (no retry on 4xx), got %d", nudgeCalls)
	}
	if got := testutil.ToFloat64(jiraFollowUpWakeRetries.WithLabelValues(jiraWakeStageNudge)) - retriesBefore; got != 0 {
		t.Errorf("expected 0 retry increments on deterministic error, got %v", got)
	}
	if got := testutil.ToFloat64(jiraFollowUpWakeFailures.WithLabelValues(jiraWakeStageNudge)) - failuresBefore; got != 1 {
		t.Errorf("expected 1 nudge terminal-failure increment, got %v", got)
	}
}

// TestJiraAgentResolver_Respawn_RetriesTransientThenSucceeds proves a momentary
// daemon blip during the same-name respawn of a STOPPED agent is absorbed by
// the bounded wake retry: the first SpawnAgent attempt returns a transient 503,
// the retry succeeds, the conversation is resumed rather than silently dying.
func TestJiraAgentResolver_Respawn_RetriesTransientThenSucceeds(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-1234-abcd": {
				ID:     "agent-bead-1",
				Title:  "jira-pe-1234-abcd",
				Fields: map[string]string{"agent": "jira-pe-1234-abcd", "stop_requested": "true"}, // stopping → dead
			},
		},
		spawnID:      "new-agent-bead",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
		// First SpawnAgent attempt fails transiently; the retry succeeds.
		spawnErrs: []error{wakeTransientDaemonErr()},
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	retriesBefore := testutil.ToFloat64(jiraFollowUpWakeRetries.WithLabelValues(jiraWakeStageSpawn))
	failuresBefore := testutil.ToFloat64(jiraFollowUpWakeFailures.WithLabelValues(jiraWakeStageSpawn))

	err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "follow up")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Two SpawnAgent attempts (transient failure + successful retry); exactly
	// one recorded spawn (the failed attempt produced no bead).
	if got := daemon.getSpawnAttempts(); got != 2 {
		t.Errorf("expected 2 SpawnAgent attempts (fail + retry), got %d", got)
	}
	if got := len(daemon.getSpawned()); got != 1 {
		t.Fatalf("expected 1 recorded spawn after retry, got %d", got)
	}
	if got := testutil.ToFloat64(jiraFollowUpWakeRetries.WithLabelValues(jiraWakeStageSpawn)) - retriesBefore; got != 1 {
		t.Errorf("expected 1 spawn retry increment, got %v", got)
	}
	if got := testutil.ToFloat64(jiraFollowUpWakeFailures.WithLabelValues(jiraWakeStageSpawn)) - failuresBefore; got != 0 {
		t.Errorf("expected 0 spawn failure increments, got %v", got)
	}
}

// TestJiraAgentResolver_Respawn_TerminalFailureCounted proves that when the
// respawn exhausts its retries, the terminal failure is counted on the
// spawn-stage metric and surfaced to the caller.
func TestJiraAgentResolver_Respawn_TerminalFailureCounted(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-1234-abcd": {
				ID:     "agent-bead-1",
				Title:  "jira-pe-1234-abcd",
				Fields: map[string]string{"agent": "jira-pe-1234-abcd", "stop_requested": "true"}, // dead
			},
		},
		spawnID:      "new-agent-bead",
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
		// Every SpawnAgent attempt fails transiently.
		spawnErrs: []error{
			wakeTransientDaemonErr(), wakeTransientDaemonErr(), wakeTransientDaemonErr(),
		},
	}

	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon,
		Nudge:  subscriberNudge(daemon.findAnyAgent),
		Logger: jiraResolverTestLogger(),
	})

	retriesBefore := testutil.ToFloat64(jiraFollowUpWakeRetries.WithLabelValues(jiraWakeStageSpawn))
	failuresBefore := testutil.ToFloat64(jiraFollowUpWakeFailures.WithLabelValues(jiraWakeStageSpawn))

	err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-1234-abcd", "PE-1234", "gardenio", "task-1", "follow up")
	if err == nil {
		t.Fatal("expected a terminal error when every spawn attempt fails transiently")
	}
	if got := daemon.getSpawnAttempts(); got != threadWriteMaxAttempts {
		t.Errorf("expected %d SpawnAgent attempts, got %d", threadWriteMaxAttempts, got)
	}
	if got := len(daemon.getSpawned()); got != 0 {
		t.Errorf("expected 0 recorded spawns (all attempts failed), got %d", got)
	}
	if got := testutil.ToFloat64(jiraFollowUpWakeRetries.WithLabelValues(jiraWakeStageSpawn)) - retriesBefore; got != float64(threadWriteMaxAttempts-1) {
		t.Errorf("expected %d spawn retry increments, got %v", threadWriteMaxAttempts-1, got)
	}
	if got := testutil.ToFloat64(jiraFollowUpWakeFailures.WithLabelValues(jiraWakeStageSpawn)) - failuresBefore; got != 1 {
		t.Errorf("expected 1 spawn terminal-failure increment, got %v", got)
	}
}

// TestRetryJiraWake_RespectsContextCancellation proves the retry loop bails out
// promptly on context cancellation between attempts rather than sleeping the
// full backoff — the webhook handler's request context governs the wake.
func TestRetryJiraWake_RespectsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the first inter-attempt sleep

	var calls int
	err := retryJiraWake(ctx, jiraWakeStageNudge, jiraResolverTestLogger(), func() error {
		calls++
		return wakeTransientDaemonErr()
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	// One attempt ran; the cancelled context stopped the loop before a second.
	if calls != 1 {
		t.Errorf("expected 1 attempt before cancellation, got %d", calls)
	}
}
