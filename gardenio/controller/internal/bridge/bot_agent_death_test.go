package bridge

import (
	"context"
	"strings"
	"testing"
	"time"

	gardeniometrics "gardenio/controller/internal/metrics"
)

// --- involuntaryDeathCause ---

func TestInvoluntaryDeathCause_ClassifiedFamilies(t *testing.T) {
	cases := []struct {
		name        string
		closeReason string
		wantSubstr  string
	}{
		{
			name:        "stop_gate_cooldown",
			closeReason: "stop_gate_cooldown_involuntary: claude exited mid-work without gb done",
			wantSubstr:  "cooldown",
		},
		{
			name:        "stop_gate_escape_hatch",
			closeReason: "stop_gate_escape_hatch_involuntary: claude exited mid-work without gb done",
			wantSubstr:  "escape-hatch",
		},
		{
			name:        "stop_gate_rate_limited",
			closeReason: "stop_gate_rate_limited_involuntary: claude exited mid-work without gb done",
			wantSubstr:  "rate-limited",
		},
		{
			name:        "context_cancelled",
			closeReason: "context_cancelled_involuntary: pod terminated mid-work before gb done",
			wantSubstr:  "SIGTERM",
		},
		{
			name:        "context_cancelled_unclaimed",
			closeReason: "context_cancelled_unclaimed: pod terminated with tool calls recorded but no claimed issue",
			wantSubstr:  "no claimed task",
		},
		{
			name:        "context_cancelled_claim_lookup_failed",
			closeReason: "context_cancelled_claim_lookup_failed: pod terminated and the claimed-task lookup could not be verified",
			wantSubstr:  "claimed-task lookup failed",
		},
		{
			name:        "other involuntary family",
			closeReason: "some_novel_kill_involuntary: brand new shape",
			wantSubstr:  "involuntary mid-work death",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := involuntaryDeathCause(tc.closeReason)
			if !strings.Contains(got, tc.wantSubstr) {
				t.Errorf("cause = %q, want substring %q", got, tc.wantSubstr)
			}
			// A one-line cause must never contain a newline.
			if strings.Contains(got, "\n") {
				t.Errorf("cause contains a newline (must be one line): %q", got)
			}
		})
	}
}

func TestInvoluntaryDeathCause_NonInvoluntaryFallsBack(t *testing.T) {
	// A close_reason that is not a classified involuntary death should surface
	// the raw reason rather than a blank cause.
	got := involuntaryDeathCause("idled_out: nothing to do")
	if !strings.Contains(got, "idled_out") {
		t.Errorf("expected raw reason to survive, got %q", got)
	}
}

func TestInvoluntaryDeathCause_EmptyReason(t *testing.T) {
	got := involuntaryDeathCause("")
	if got == "" {
		t.Fatalf("expected a non-empty fallback cause for empty close_reason")
	}
	if !strings.Contains(got, "unknown") {
		t.Errorf("expected 'unknown' fallback, got %q", got)
	}
}

// --- NotifyAgentDeath: Slack post + bead comment ---

func TestNotifyAgentDeath_PostsWarningAndAttachesComment(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"

	bead := BeadEvent{
		ID:       "gd-dead-1",
		Assignee: "gardenio-doomed-agent",
		Fields: map[string]string{
			"agent_state":  "done",
			"close_reason": "stop_gate_cooldown_involuntary: claude exited mid-work without gb done",
		},
	}

	if err := bot.NotifyAgentDeath(context.Background(), bead); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Slack: one warning post naming the agent + a cause.
	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 Slack call, got %d", len(got))
	}
	if got[0].Method != "chat.postMessage" {
		t.Errorf("expected chat.postMessage, got %s", got[0].Method)
	}
	if got[0].Channel != "C-test" {
		t.Errorf("expected channel C-test, got %s", got[0].Channel)
	}
	if !strings.Contains(got[0].Text, "gardenio-doomed-agent") {
		t.Errorf("expected warning to name the agent, got %q", got[0].Text)
	}
	if !strings.Contains(strings.ToLower(got[0].Text), "died") {
		t.Errorf("expected warning to say the agent died, got %q", got[0].Text)
	}

	// Bead: forensic breadcrumb attached to the dead bead.
	deadBeadComments := commentsForBead(daemon, "gd-dead-1")
	if len(deadBeadComments) != 1 {
		t.Fatalf("expected 1 comment on the dead bead, got %d", len(deadBeadComments))
	}
	if deadBeadComments[0].Author != deathReportAuthor {
		t.Errorf("expected comment author %q, got %q", deathReportAuthor, deadBeadComments[0].Author)
	}
	if !strings.Contains(deadBeadComments[0].Text, "cooldown") {
		t.Errorf("expected comment to carry the one-line cause, got %q", deadBeadComments[0].Text)
	}
}

// commentsForBead filters the mock daemon's recorded comments down to a single
// bead, so a test can assert exactly what forensic breadcrumb landed where.
func commentsForBead(m *mockDaemon, beadID string) []commentCall {
	var out []commentCall
	for _, c := range m.getComments() {
		if c.BeadID == beadID {
			out = append(out, c)
		}
	}
	return out
}

func TestNotifyAgentDeath_UsesAgentFieldWhenAssigneeEmpty(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"

	bead := BeadEvent{
		ID: "gd-dead-2",
		Fields: map[string]string{
			"agent":        "gardenio-nameless",
			"close_reason": "context_cancelled_involuntary: pod terminated mid-work before gb done",
		},
	}

	if err := bot.NotifyAgentDeath(context.Background(), bead); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	got := getCalls(calls, mu)
	if len(got) != 1 {
		t.Fatalf("expected 1 Slack call, got %d", len(got))
	}
	if !strings.Contains(got[0].Text, "gardenio-nameless") {
		t.Errorf("expected warning to name the agent from the agent field, got %q", got[0].Text)
	}
}

// --- Death-storm volume gate ---

func TestAdmitDeathReport_BurstThenSuppress(t *testing.T) {
	bot := &Bot{} // gate uses only the mutex + counters, no Slack/daemon needed

	base := time.Unix(1_700_000_000, 0)
	// First deathStormBurst calls within the window are admitted.
	for i := 0; i < deathStormBurst; i++ {
		admit, rollup, _ := bot.admitDeathReport(base.Add(time.Duration(i)*time.Second), heldDeath{})
		if !admit {
			t.Fatalf("death %d within burst should be admitted", i)
		}
		if rollup != 0 {
			t.Fatalf("death %d should not trigger a rollup, got %d", i, rollup)
		}
	}
	// The next few in the same window are suppressed.
	for i := 0; i < 3; i++ {
		admit, rollup, _ := bot.admitDeathReport(base.Add(time.Duration(deathStormBurst+i)*time.Second), heldDeath{})
		if admit {
			t.Fatalf("death past the burst should be suppressed")
		}
		if rollup != 0 {
			t.Fatalf("suppressed death should not trigger a rollup mid-window, got %d", rollup)
		}
	}
}

func TestAdmitDeathReport_WindowRolloverFlushesSuppressed(t *testing.T) {
	bot := &Bot{}

	base := time.Unix(1_700_000_000, 0)
	// Spend the burst.
	for i := 0; i < deathStormBurst; i++ {
		bot.admitDeathReport(base, heldDeath{})
	}
	// Suppress three more in the same window.
	for i := 0; i < 3; i++ {
		bot.admitDeathReport(base, heldDeath{})
	}

	// A death after the window elapses admits again AND flushes the 3 suppressed.
	admit, rollup, _ := bot.admitDeathReport(base.Add(deathStormWindow + time.Second), heldDeath{})
	if !admit {
		t.Errorf("first death in a fresh window should be admitted")
	}
	if rollup != 3 {
		t.Errorf("expected rollup of 3 suppressed deaths, got %d", rollup)
	}

	// A subsequent death in the new window does not re-flush.
	_, rollup2, _ := bot.admitDeathReport(base.Add(deathStormWindow + 2*time.Second), heldDeath{})
	if rollup2 != 0 {
		t.Errorf("rollup should only fire once per window rollover, got %d", rollup2)
	}
}

func TestNotifyAgentDeath_StormSuppressesSlackButAlwaysAttachesComment(t *testing.T) {
	daemon := newMockDaemon()
	srv, calls, mu := capturingSlackServer(t)
	defer srv.Close()

	bot := newTestBot(daemon, srv)
	bot.channel = "C-test"

	// Fire deathStormBurst+extra involuntary deaths in a tight window.
	const extra = 4
	total := deathStormBurst + extra
	for i := 0; i < total; i++ {
		bead := BeadEvent{
			ID:       "gd-storm-" + string(rune('a'+i)),
			Assignee: "gardenio-storm-victim",
			Fields: map[string]string{
				"agent_state":  "done",
				"close_reason": "stop_gate_cooldown_involuntary: mid-work",
			},
		}
		if err := bot.NotifyAgentDeath(context.Background(), bead); err != nil {
			t.Fatalf("unexpected error on death %d: %v", i, err)
		}
	}

	// Slack: exactly deathStormBurst individual posts (the extra are suppressed;
	// their rollup only fires on the NEXT window rollover, not yet).
	got := getCalls(calls, mu)
	if len(got) != deathStormBurst {
		t.Fatalf("expected %d individual Slack posts during the storm, got %d", deathStormBurst, len(got))
	}

	// Bead comments: EVERY death gets a forensic breadcrumb, storm or not.
	commentCount := 0
	for i := 0; i < total; i++ {
		id := "gd-storm-" + string(rune('a'+i))
		commentCount += len(commentsForBead(daemon, id))
	}
	if commentCount != total {
		t.Errorf("expected %d forensic comments (one per death), got %d", total, commentCount)
	}
}

// --- watcher wiring: only involuntary deaths reach NotifyAgentDeath ---

func TestAgents_HandleClosed_InvoluntaryDeathNotified(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{Notifier: notif, Logger: testLogger()})

	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-inv-1",
		Type:     "agent",
		Assignee: "gardenio-inv-agent",
		Status:   "closed",
		Fields: map[string]string{
			"agent_state":  "done", // involuntary deaths close as done — invisible to crash path
			"close_reason": "stop_gate_cooldown_involuntary: claude exited mid-work without gb done",
		},
	})
	a.handleClosed(context.Background(), closedEvent)

	if deaths := notif.getDeaths(); len(deaths) != 1 {
		t.Fatalf("expected 1 death notification, got %d", len(deaths))
	}
	// It is NOT a crash (agent_state=done).
	if crashes := notif.getCrashes(); len(crashes) != 0 {
		t.Errorf("involuntary death must not fire the crash path, got %d crashes", len(crashes))
	}
}

// TestAgents_HandleClosed_ClaimLookupFailedDeathNotified pins gd-TYjMaxCMZR: a
// SIGTERM teardown whose claimed-task lookup failed carries no `_involuntary`
// marker, but it must still post the Slack death report.
func TestAgents_HandleClosed_ClaimLookupFailedDeathNotified(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{Notifier: notif, Logger: testLogger()})

	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-inv-lookup",
		Type:     "agent",
		Assignee: "gardenio-inv-agent",
		Status:   "closed",
		Fields: map[string]string{
			"agent_state":  "done",
			"close_reason": "context_cancelled_claim_lookup_failed: pod terminated and the claimed-task lookup could not be verified",
		},
	})
	a.handleClosed(context.Background(), closedEvent)

	if deaths := notif.getDeaths(); len(deaths) != 1 {
		t.Fatalf("expected 1 death notification, got %d", len(deaths))
	}
}

func TestAgents_HandleClosed_CleanCloseNotNotifiedAsDeath(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{Notifier: notif, Logger: testLogger()})

	closedEvent := marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-clean-1",
		Type:     "agent",
		Assignee: "gardenio-clean-agent",
		Status:   "closed",
		Fields: map[string]string{
			"agent_state":  "done",
			"close_reason": "", // clean gb stop
		},
	})
	a.handleClosed(context.Background(), closedEvent)

	if deaths := notif.getDeaths(); len(deaths) != 0 {
		t.Errorf("a clean close must not fire a death notification, got %d", len(deaths))
	}
}

func TestAgents_HandleClosed_DeathDedup(t *testing.T) {
	notif := &mockAgentNotifier{}
	a := NewAgents(AgentsConfig{Notifier: notif, Logger: testLogger()})

	ev := marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-dedup-1",
		Type:     "agent",
		Assignee: "gardenio-dedup-agent",
		Status:   "closed",
		Fields: map[string]string{
			"agent_state":  "done",
			"close_reason": "context_cancelled_unclaimed: no claimed issue",
		},
	})
	a.handleClosed(context.Background(), ev)
	a.handleClosed(context.Background(), ev) // redelivered

	if deaths := notif.getDeaths(); len(deaths) != 1 {
		t.Errorf("expected death to be reported once (dedup), got %d", len(deaths))
	}
}

// Ensure the Bot itself satisfies the interface method (compile-time + a live
// call through the interface).
func TestBot_ImplementsNotifyAgentDeath(t *testing.T) {
	var _ AgentNotifier = (*Bot)(nil)
	// Sanity: the shared classifier agrees the marker counts.
	if _, ok := gardeniometrics.InvoluntaryDeathFamily("stop_gate_cooldown_involuntary: x"); !ok {
		t.Fatal("expected the involuntary marker to classify as a death")
	}
}

// --- deathCause: controller-initiated restarts (ka-0000001482 D3) ---

// TestDeathCause_ControllerReasonWinsOverGenericGuess is the regression test for
// the misattribution: on 2026-07-27 the controller's own account-rotation sweep
// deleted 5 working pods, and the report told the operator it was a "node drain,
// rollout, or Spot reclaim" — all three false on an all-on-demand cluster with no
// rollout and no drain events. When the controller recorded WHY it deleted the
// pod, that reason must win.
func TestDeathCause_ControllerReasonWinsOverGenericGuess(t *testing.T) {
	got := deathCause(
		"context_cancelled_involuntary: pod terminated mid-work before gb done",
		"controller restarted the pod off exhausted account shannon onto matthewbaker-acme",
		nil,
	)
	for _, want := range []string{"controller restarted", "shannon"} {
		if !strings.Contains(got, want) {
			t.Errorf("cause %q does not mention %q", got, want)
		}
	}
	// The misleading guess must be gone entirely.
	for _, unwanted := range []string{"Spot", "node drain", "rollout"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("cause %q still blames %q for a controller-initiated delete", got, unwanted)
		}
	}
	// The mid-turn framing is still composed in, so the operator learns both that
	// the controller did it AND that a turn was interrupted.
	if !strings.Contains(got, "mid-turn") {
		t.Errorf("cause %q dropped the mid-turn/orphaned-task framing", got)
	}
}

// TestDeathCause_FallsBackWithoutControllerReason verifies the generic
// classification is untouched when the controller did NOT delete the pod — a real
// node drain or Spot reclaim must still read as such.
func TestDeathCause_FallsBackWithoutControllerReason(t *testing.T) {
	reason := "context_cancelled_involuntary: pod terminated mid-work before gb done"
	if got, want := deathCause(reason, "", nil), involuntaryDeathCause(reason); got != want {
		t.Errorf("deathCause with no controller reason = %q, want the generic cause %q", got, want)
	}
	// Whitespace-only is treated as absent, not as a reason.
	if got, want := deathCause(reason, "   ", nil), involuntaryDeathCause(reason); got != want {
		t.Errorf("blank controller reason must fall back, got %q want %q", got, want)
	}
}

// TestDeathCause_CleanExitWithControllerReason verifies a controller restart of an
// agent that had already stopped cleanly reports just the restart reason, with no
// spurious "was mid-turn" claim attached.
func TestDeathCause_CleanExitWithControllerReason(t *testing.T) {
	got := deathCause("idle_ttl_expired", "controller restarted the pod off exhausted account shannon", nil)
	if strings.Contains(got, "mid-turn") {
		t.Errorf("a clean exit must not be described as mid-turn, got %q", got)
	}
	if !strings.Contains(got, "controller restarted") {
		t.Errorf("cause %q lost the controller restart reason", got)
	}
}

// --- deathCause: credential-failure evidence beats the generic guess (ka-0000002985 DEFECT 1) ---

// TestDeathCause_AuthFieldsOverrideContextCancelled is the regression test for
// the whole incident: an agent closed context_cancelled_involuntary while its
// fields carried error_category=unauthorized / authentication_failed and a
// never-authenticated token shape. The honest cause is the credential failure,
// not a phantom Spot reclaim on an all-on-demand cluster.
func TestDeathCause_AuthFieldsOverrideContextCancelled(t *testing.T) {
	fields := map[string]string{
		"error_category":      "unauthorized",
		"error_detail":        "authentication_failed",
		"total_output_tokens": "0",
		"total_request_count": "15",
		"total_cost_usd":      "0",
	}
	got := deathCause(
		"context_cancelled_involuntary: pod terminated mid-work before gb done",
		"",
		fields,
	)
	for _, want := range []string{"credential", "unauthorized"} {
		if !strings.Contains(strings.ToLower(got), want) {
			t.Errorf("cause %q does not name the credential failure (missing %q)", got, want)
		}
	}
	// The false infra attribution must be gone.
	for _, unwanted := range []string{"Spot", "node drain", "rollout"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("cause %q still asserts %q for a credential failure", got, unwanted)
		}
	}
	if strings.Contains(got, "\n") {
		t.Errorf("cause contains a newline (must be one line): %q", got)
	}
}

// TestDeathCause_ControllerReasonBeatsAuthFields verifies precedence: a recorded
// controller action is ground truth and wins even when the auth signature is also
// present.
func TestDeathCause_ControllerReasonBeatsAuthFields(t *testing.T) {
	fields := map[string]string{"error_category": "unauthorized"}
	got := deathCause(
		"context_cancelled_involuntary: pod terminated mid-work before gb done",
		"controller restarted the pod off exhausted account shannon",
		fields,
	)
	if !strings.Contains(got, "controller restarted") {
		t.Errorf("controller reason must win over auth fields, got %q", got)
	}
}

// TestDeathCause_NoAuthSignatureStaysHonest verifies a generic context_cancelled
// death with NO credential evidence no longer asserts Spot/node-drain as fact.
func TestDeathCause_NoAuthSignatureStaysHonest(t *testing.T) {
	got := deathCause(
		"context_cancelled_involuntary: pod terminated mid-work before gb done",
		"",
		map[string]string{},
	)
	for _, unwanted := range []string{"Spot", "rollout, or Spot"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("honest cause must not assert %q, got %q", unwanted, got)
		}
	}
	if !strings.Contains(got, "not determined") {
		t.Errorf("honest cause should say the interruption source was not determined, got %q", got)
	}
}
