package bridge

// Tests for the Jira conversational-loop outcome metrics (GAP 1, epic
// ka-0000007401 / ka-0000006498). Two layers:
//
//   - Direct recorder tests prove the bounded-enum guard: a valid member
//     increments its labelled series, an out-of-enum value is DROPPED (no
//     increment, returns false) so cardinality stays bounded — the same
//     contract recordAssistantEvent holds.
//   - Handler-integration tests prove the increments actually fire from the
//     real reply and spawn-ack code paths (posted / blocked / error), so the
//     wiring can't silently rot.
//
// Counters are process-global (promauto/DefaultRegisterer). To read a delta
// without depending on test ordering, each test snapshots the specific labelled
// series with testutil.ToFloat64 before acting and asserts the increment,
// mirroring jira_mention_metrics_test.go.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/internal/beadsapi"
)

// --- Recorder enum guards -----------------------------------------------------

func TestRecordJiraFollowUpDelivery_EnumGuard(t *testing.T) {
	for _, path := range []string{jiraDeliveryWarm, jiraDeliveryCold, jiraDeliverySkipped} {
		before := testutil.ToFloat64(jiraFollowUpDeliveryTotal.WithLabelValues(path))
		if !recordJiraFollowUpDelivery(path) {
			t.Fatalf("recordJiraFollowUpDelivery(%q) = false, want true", path)
		}
		after := testutil.ToFloat64(jiraFollowUpDeliveryTotal.WithLabelValues(path))
		if after != before+1 {
			t.Errorf("path %q counter = %v, want %v", path, after, before+1)
		}
	}
	if recordJiraFollowUpDelivery("teleport") {
		t.Error("recordJiraFollowUpDelivery(invalid) = true, want false (dropped)")
	}
}

func TestRecordJiraMentionSpawn_EnumGuard(t *testing.T) {
	for _, outcome := range []string{jiraSpawnOK, jiraSpawnError} {
		before := testutil.ToFloat64(jiraMentionSpawnTotal.WithLabelValues(outcome))
		if !recordJiraMentionSpawn(outcome) {
			t.Fatalf("recordJiraMentionSpawn(%q) = false, want true", outcome)
		}
		after := testutil.ToFloat64(jiraMentionSpawnTotal.WithLabelValues(outcome))
		if after != before+1 {
			t.Errorf("outcome %q counter = %v, want %v", outcome, after, before+1)
		}
	}
	if recordJiraMentionSpawn("maybe") {
		t.Error("recordJiraMentionSpawn(invalid) = true, want false (dropped)")
	}
}

func TestRecordJiraReply_EnumGuardAndVisibilityNormalization(t *testing.T) {
	// Valid outcome + known visibility increments the exact (outcome,visibility)
	// series.
	before := testutil.ToFloat64(jiraReplyTotal.WithLabelValues(jiraReplyPosted, string(CommentPublic)))
	if !recordJiraReply(jiraReplyPosted, CommentPublic) {
		t.Fatal("recordJiraReply(posted, public) = false, want true")
	}
	after := testutil.ToFloat64(jiraReplyTotal.WithLabelValues(jiraReplyPosted, string(CommentPublic)))
	if after != before+1 {
		t.Errorf("posted/public counter = %v, want %v", after, before+1)
	}

	// An unknown visibility normalizes to the bounded "none" bucket rather than
	// minting a new label value.
	beforeNone := testutil.ToFloat64(jiraReplyTotal.WithLabelValues(jiraReplyBlocked, "none"))
	if !recordJiraReply(jiraReplyBlocked, CommentVisibility("weird")) {
		t.Fatal("recordJiraReply(blocked, weird) = false, want true")
	}
	afterNone := testutil.ToFloat64(jiraReplyTotal.WithLabelValues(jiraReplyBlocked, "none"))
	if afterNone != beforeNone+1 {
		t.Errorf("blocked/none counter = %v, want %v (unknown vis must fold to none)", afterNone, beforeNone+1)
	}

	// Out-of-enum outcome is dropped.
	if recordJiraReply("exploded", CommentInternal) {
		t.Error("recordJiraReply(invalid outcome) = true, want false (dropped)")
	}
}

func TestRecordJiraSpawnAck_EnumGuard(t *testing.T) {
	for _, outcome := range []string{jiraAckPosted, jiraAckBlocked, jiraAckError, jiraAckQuiet} {
		before := testutil.ToFloat64(jiraSpawnAckTotal.WithLabelValues(outcome))
		if !recordJiraSpawnAck(outcome) {
			t.Fatalf("recordJiraSpawnAck(%q) = false, want true", outcome)
		}
		after := testutil.ToFloat64(jiraSpawnAckTotal.WithLabelValues(outcome))
		if after != before+1 {
			t.Errorf("outcome %q counter = %v, want %v", outcome, after, before+1)
		}
	}
	if recordJiraSpawnAck("pending") {
		t.Error("recordJiraSpawnAck(invalid) = true, want false (dropped)")
	}
}

// --- Startup pre-initialization -----------------------------------------------

// TestPreInitJiraConversationMetrics_MaterializesEverySeriesAtZero proves that
// PreInitJiraConversationMetrics forces every bounded labelled series of the
// conversational-loop CounterVecs into the default gatherer at 0, so a
// quiet-but-healthy jira-bridge exports a graphable flat line on the first
// scrape instead of "No data" (ka-0000004443). It gathers the process-global
// registry and asserts each (metric,label-set) is present.
func TestPreInitJiraConversationMetrics_MaterializesEverySeriesAtZero(t *testing.T) {
	PreInitJiraConversationMetrics()

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	got := map[string]bool{}
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			var sb strings.Builder
			sb.WriteString(mf.GetName())
			sb.WriteString("{")
			for i, lp := range m.GetLabel() {
				if i > 0 {
					sb.WriteString(",")
				}
				sb.WriteString(lp.GetName())
				sb.WriteString("=")
				sb.WriteString(lp.GetValue())
			}
			sb.WriteString("}")
			got[sb.String()] = true
		}
	}

	// One representative series per family — the ones a quiet loop would never
	// otherwise emit, which are exactly the panels the bug reported dark.
	wantSeries := []string{
		"gardenio_jira_mention_spawn_total{outcome=error}",
		"gardenio_jira_spawn_ack_total{outcome=error}",
		"gardenio_jira_reply_total{outcome=blocked,visibility=public}",
		"gardenio_jira_followup_delivery_total{path=skipped}",
		"gardenio_jira_followup_wake_retries_total{stage=spawn}",
		"gardenio_jira_followup_wake_failures_total{stage=nudge}",
		"gardenio_jira_inbound_comment_write_retries_total{stage=set_fields}",
		"gardenio_jira_inbound_comment_dlq_dropped_total{reason=exhausted}",
	}
	for _, s := range wantSeries {
		if !got[s] {
			t.Errorf("PreInit must materialize %s at 0; series not present after pre-init", s)
		}
	}
}

// --- Handler integration ------------------------------------------------------

// TestJiraReplyMetric_PostedOnBoundTicket proves a successful reply through the
// real handler bumps gardenio_jira_reply_total{outcome=posted,visibility=internal}.
func TestJiraReplyMetric_PostedOnBoundTicket(t *testing.T) {
	daemon := replyAgentDaemon("crew-metric-1", "PE-42", "crew")
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	before := testutil.ToFloat64(jiraReplyTotal.WithLabelValues(jiraReplyPosted, string(CommentInternal)))
	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-metric-1", Text: "done"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}
	after := testutil.ToFloat64(jiraReplyTotal.WithLabelValues(jiraReplyPosted, string(CommentInternal)))
	if after != before+1 {
		t.Errorf("posted/internal counter = %v, want %v", after, before+1)
	}
}

// TestJiraReplyMetric_BlockedOnUnboundTicket proves a gate rejection (agent not
// bound to the requested ticket) bumps the blocked series, not posted.
func TestJiraReplyMetric_BlockedOnUnboundTicket(t *testing.T) {
	// Agent bound to PE-42 but the request names a different key → fail-closed
	// 403 out of resolveReplyTarget.
	daemon := replyAgentDaemon("crew-metric-2", "PE-42", "crew")
	jira := &mockJiraSquawkJira{}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	before := testutil.ToFloat64(jiraReplyTotal.WithLabelValues(jiraReplyBlocked, string(CommentInternal)))
	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-metric-2", Text: "hi", JiraKey: "PE-999"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
	after := testutil.ToFloat64(jiraReplyTotal.WithLabelValues(jiraReplyBlocked, string(CommentInternal)))
	if after != before+1 {
		t.Errorf("blocked/internal counter = %v, want %v", after, before+1)
	}
}

// TestJiraReplyMetric_ErrorWhenJiraPostFails proves a Jira post failure (gate
// passed, AddComment errored) bumps the error series.
func TestJiraReplyMetric_ErrorWhenJiraPostFails(t *testing.T) {
	daemon := replyAgentDaemon("crew-metric-3", "PE-42", "crew")
	jira := &mockJiraSquawkJira{err: context.DeadlineExceeded}
	api := newJiraReplyAPI(daemon, jira, enforcedWritePolicy(false))

	before := testutil.ToFloat64(jiraReplyTotal.WithLabelValues(jiraReplyError, string(CommentInternal)))
	rec := postJiraReply(t, api, jiraReplyRequest{Agent: "crew-metric-3", Text: "will fail"})
	if rec.Code == http.StatusOK {
		t.Fatalf("status = %d, want a non-200 error", rec.Code)
	}
	after := testutil.ToFloat64(jiraReplyTotal.WithLabelValues(jiraReplyError, string(CommentInternal)))
	if after != before+1 {
		t.Errorf("error/internal counter = %v, want %v", after, before+1)
	}
}

// TestJiraSpawnAckMetric_PostedAndBlocked proves the ack path bumps posted on a
// writable project and blocked on the customer-facing FSM project (zero write).
func TestJiraSpawnAckMetric_PostedAndBlocked(t *testing.T) {
	ack, _ := newTestSpawnAck(false)

	beforePosted := testutil.ToFloat64(jiraSpawnAckTotal.WithLabelValues(jiraAckPosted))
	ack.Acknowledge(context.Background(), JiraMentionSpawnEvent{
		AgentName:  "gardenio-jira-pe-1-x",
		TaskBeadID: "gd-T1",
		JiraKey:    "PE-1",
		Role:       "crew",
	})
	afterPosted := testutil.ToFloat64(jiraSpawnAckTotal.WithLabelValues(jiraAckPosted))
	if afterPosted != beforePosted+1 {
		t.Errorf("ack posted counter = %v, want %v", afterPosted, beforePosted+1)
	}

	beforeBlocked := testutil.ToFloat64(jiraSpawnAckTotal.WithLabelValues(jiraAckBlocked))
	ack.Acknowledge(context.Background(), JiraMentionSpawnEvent{
		AgentName:  "gardenio-jira-fsm-1-x",
		TaskBeadID: "gd-T2",
		JiraKey:    "FSM-1",
		Role:       "crew",
	})
	afterBlocked := testutil.ToFloat64(jiraSpawnAckTotal.WithLabelValues(jiraAckBlocked))
	if afterBlocked != beforeBlocked+1 {
		t.Errorf("ack blocked counter = %v, want %v", afterBlocked, beforeBlocked+1)
	}
}

// TestJiraSpawnAckMetric_ErrorWhenPostFails proves a Jira post failure on the
// ack path bumps the error series.
func TestJiraSpawnAckMetric_ErrorWhenPostFails(t *testing.T) {
	jira := &mockJiraSquawkJira{err: context.DeadlineExceeded}
	ack := NewJiraSpawnAcknowledger(JiraSpawnAcknowledgerConfig{
		Jira:        jira,
		WritePolicy: enforcedWritePolicy(false),
		Logger:      jiraSquawkTestLogger(),
	})

	before := testutil.ToFloat64(jiraSpawnAckTotal.WithLabelValues(jiraAckError))
	ack.Acknowledge(context.Background(), JiraMentionSpawnEvent{
		AgentName:  "gardenio-jira-pe-2-x",
		TaskBeadID: "gd-T3",
		JiraKey:    "PE-2",
		Role:       "crew",
	})
	after := testutil.ToFloat64(jiraSpawnAckTotal.WithLabelValues(jiraAckError))
	if after != before+1 {
		t.Errorf("ack error counter = %v, want %v", after, before+1)
	}
}

// --- Follow-up delivery path (warm/cold/skipped) ------------------------------

// spawnTrigger is a minimal enabled spawn_agent trigger so the resolver's
// cold-respawn path resolves a role and spawns.
func spawnTrigger() []*beadsapi.BeadDetail {
	return []*beadsapi.BeadDetail{
		{Fields: map[string]string{
			"action": "spawn_agent", "project": "gardenio",
			"role": "gardenio-crew", "enabled": "true",
		}},
	}
}

// TestJiraFollowUpDeliveryMetric_Warm proves nudging a live agent through the
// resolver bumps gardenio_jira_followup_delivery_total{path=warm}.
func TestJiraFollowUpDeliveryMetric_Warm(t *testing.T) {
	coopServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer coopServer.Close()

	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-77-abcd": {
				ID:     "agent-bead-warm",
				Title:  "jira-pe-77-abcd",
				Fields: map[string]string{"agent": "jira-pe-77-abcd", "coop_url": coopServer.URL},
			},
		},
		spawnID: "unused",
	}
	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon, Nudge: subscriberNudge(daemon.findAnyAgent), Logger: jiraSquawkTestLogger(),
	})

	before := testutil.ToFloat64(jiraFollowUpDeliveryTotal.WithLabelValues(jiraDeliveryWarm))
	if err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-77-abcd", "PE-77", "gardenio", "task-1", "follow up"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	after := testutil.ToFloat64(jiraFollowUpDeliveryTotal.WithLabelValues(jiraDeliveryWarm))
	if after != before+1 {
		t.Errorf("warm delivery counter = %v, want %v", after, before+1)
	}
}

// TestJiraFollowUpDeliveryMetric_Cold proves respawning a dead agent bumps the
// cold series.
func TestJiraFollowUpDeliveryMetric_Cold(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		agentBeads: map[string]*beadsapi.BeadDetail{
			"jira-pe-78-abcd": {
				ID:     "agent-bead-cold",
				Title:  "jira-pe-78-abcd",
				Fields: map[string]string{"agent": "jira-pe-78-abcd", "stop_requested": "true"}, // stopping → dead
			},
		},
		spawnID:      "new-agent-bead",
		triggerBeads: spawnTrigger(),
	}
	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon, Nudge: subscriberNudge(daemon.findAnyAgent), Logger: jiraSquawkTestLogger(),
	})

	before := testutil.ToFloat64(jiraFollowUpDeliveryTotal.WithLabelValues(jiraDeliveryCold))
	if err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-78-abcd", "PE-78", "gardenio", "task-1", "follow up"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	after := testutil.ToFloat64(jiraFollowUpDeliveryTotal.WithLabelValues(jiraDeliveryCold))
	if after != before+1 {
		t.Errorf("cold delivery counter = %v, want %v", after, before+1)
	}
}

// TestJiraFollowUpDeliveryMetric_Skipped proves the recent-spawn dedup guard
// bumps the skipped series (a deliberately-dropped follow-up is observable, not
// silent).
func TestJiraFollowUpDeliveryMetric_Skipped(t *testing.T) {
	daemon := &mockJiraResolverDaemon{
		agentBeads:   map[string]*beadsapi.BeadDetail{},
		spawnID:      "agent-1",
		triggerBeads: spawnTrigger(),
	}
	resolver := NewJiraAgentResolver(JiraAgentResolverConfig{
		Daemon: daemon, Nudge: subscriberNudge(daemon.findAnyAgent), Logger: jiraSquawkTestLogger(),
	})

	// First call spawns (cold), arming the recent-spawn guard.
	if err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-79-abcd", "PE-79", "gardenio", "task-1", "first"); err != nil {
		t.Fatalf("unexpected error on first call: %v", err)
	}

	before := testutil.ToFloat64(jiraFollowUpDeliveryTotal.WithLabelValues(jiraDeliverySkipped))
	// Second call within the cooldown is suppressed → skipped.
	if err := resolver.ResolveAndNudge(context.Background(),
		"jira-pe-79-abcd", "PE-79", "gardenio", "task-1", "second"); err != nil {
		t.Fatalf("unexpected error on second call: %v", err)
	}
	after := testutil.ToFloat64(jiraFollowUpDeliveryTotal.WithLabelValues(jiraDeliverySkipped))
	if after != before+1 {
		t.Errorf("skipped delivery counter = %v, want %v", after, before+1)
	}
}
