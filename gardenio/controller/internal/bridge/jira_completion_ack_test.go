package bridge

import (
	"context"
	"maps"
	"strings"
	"testing"
)

// newTestCompletionAck builds a completion acknowledger over a mock Jira client
// and an enforce-mode default write policy (the production shape). The returned
// mock captures every posted comment for assertions.
func newTestCompletionAck(outboxEnabled bool) (*JiraCompletionAcknowledger, *mockJiraSquawkJira) {
	jira := &mockJiraSquawkJira{}
	ack := NewJiraCompletionAcknowledger(JiraCompletionAcknowledgerConfig{
		Jira:        jira,
		WritePolicy: enforcedWritePolicy(outboxEnabled),
		Logger:      jiraSquawkTestLogger(),
	})
	return ack, jira
}

// mentionAgentClose builds a closed mention-agent BeadEvent SSE payload. The
// fields mirror what SpawnAgent stamps (jira_mentions.go): jira_key,
// spawn_source=jira-mention, role, plus the terminal agent_state.
func mentionAgentClose(id, agent, jiraKey, role, agentState string, extra map[string]string) []byte {
	fields := map[string]string{
		"agent":        agent,
		"jira_key":     jiraKey,
		"spawn_source": "jira-mention",
		"role":         role,
		"agent_state":  agentState,
	}
	maps.Copy(fields, extra)
	return marshalSSEBeadPayload(BeadEvent{
		ID:       id,
		Type:     "agent",
		Title:    agent,
		Assignee: agent,
		Status:   "closed",
		Fields:   fields,
	})
}

// TestCompletionAck_PostsDoneNote is the happy path: a mention agent that closes
// with agent_state=done on a writable project (PE) posts exactly one INTERNAL
// note naming the agent and the ✅ Done outcome.
func TestCompletionAck_PostsDoneNote(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	ack.handleClosed(context.Background(),
		mentionAgentClose("gd-AGENT1", "gardenio-jira-pe-1234-abc", "PE-1234", "crew", "done", nil))

	c := jira.getComments()
	if len(c) != 1 {
		t.Fatalf("expected exactly 1 comment, got %d", len(c))
	}
	if c[0].Key != "PE-1234" {
		t.Errorf("comment key = %q, want PE-1234", c[0].Key)
	}
	if c[0].Vis != CommentInternal {
		t.Errorf("comment visibility = %q, want internal", c[0].Vis)
	}
	if !strings.Contains(c[0].Text, "gardenio-jira-pe-1234-abc") {
		t.Errorf("comment %q does not name the agent", c[0].Text)
	}
	if !strings.Contains(c[0].Text, "✅") || !strings.Contains(c[0].Text, "Done") {
		t.Errorf("comment %q is not a Done note", c[0].Text)
	}
	if !strings.Contains(c[0].Text, "[gardenio agent]") {
		t.Errorf("comment %q missing the [gardenio agent] prefix", c[0].Text)
	}
}

// TestCompletionAck_PostsFailedNote pins that a crash (agent_state=failed) posts
// a ❌ Failed note, not a ✅ Done one.
func TestCompletionAck_PostsFailedNote(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	ack.handleClosed(context.Background(),
		mentionAgentClose("gd-AGENT2", "gardenio-jira-pe-2-x", "PE-2", "crew", "failed", nil))

	c := jira.getComments()
	if len(c) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(c))
	}
	if !strings.Contains(c[0].Text, "❌") || !strings.Contains(c[0].Text, "Failed") {
		t.Errorf("comment %q is not a Failed note", c[0].Text)
	}
}

// TestCompletionAck_PodPhaseFailedIsFailed pins that a pod_phase=failed close
// (agent_state empty) classifies as Failed — the Slack-parity crash signal.
func TestCompletionAck_PodPhaseFailedIsFailed(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	ack.handleClosed(context.Background(),
		mentionAgentClose("gd-AGENT3", "gardenio-jira-pe-3-x", "PE-3", "crew", "",
			map[string]string{"pod_phase": "failed"}))

	c := jira.getComments()
	if len(c) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(c))
	}
	if !strings.Contains(c[0].Text, "❌") {
		t.Errorf("comment %q should be a Failed note for pod_phase=failed", c[0].Text)
	}
}

// TestCompletionAck_DoneWinsPodPhaseFailedRace pins the Slack-parity guard: a
// clean agent_state=done suppresses a racing pod_phase=failed (pod teardown can
// sync failed after a clean close), so the note stays ✅ Done.
func TestCompletionAck_DoneWinsPodPhaseFailedRace(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	ack.handleClosed(context.Background(),
		mentionAgentClose("gd-AGENT4", "gardenio-jira-pe-4-x", "PE-4", "crew", "done",
			map[string]string{"pod_phase": "failed"}))

	c := jira.getComments()
	if len(c) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(c))
	}
	if !strings.Contains(c[0].Text, "✅") {
		t.Errorf("comment %q should stay Done when agent_state=done wins the race", c[0].Text)
	}
}

// TestCompletionAck_OrphanReaperIsNotFailed pins that a benign orphan reaper
// cleanup (close_reason orphan:, pod_phase failed) is NOT a crash — it defers to
// terminalCardState, which returns done for an empty agent_state + orphan reason.
func TestCompletionAck_OrphanReaperIsNotFailed(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	ack.handleClosed(context.Background(),
		mentionAgentClose("gd-AGENT5", "gardenio-jira-pe-5-x", "PE-5", "crew", "",
			map[string]string{"pod_phase": "failed", "close_reason": "orphan: source task closed"}))

	c := jira.getComments()
	if len(c) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(c))
	}
	if strings.Contains(c[0].Text, "❌") {
		t.Errorf("comment %q must not be a Failed note for a benign orphan reaper cleanup", c[0].Text)
	}
}

// TestCompletionAck_EchoesWrapUpTLDR pins that a wrap-up tldr is echoed into the
// note so the ticket carries the outcome summary, not just a glyph.
func TestCompletionAck_EchoesWrapUpTLDR(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	ack.handleClosed(context.Background(),
		mentionAgentClose("gd-AGENT6", "gardenio-jira-pe-6-x", "PE-6", "crew", "done",
			map[string]string{"wrapup": `{"tldr":"Fixed the upload race; MR !123 merged"}`}))

	c := jira.getComments()
	if len(c) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(c))
	}
	if !strings.Contains(c[0].Text, "Fixed the upload race; MR !123 merged") {
		t.Errorf("comment %q did not echo the wrap-up tldr", c[0].Text)
	}
}

// TestCompletionAck_FillerTLDRNotEchoed pins that a filler-sentinel tldr ("none")
// adds no echo line — the same negative-sentinel filter the Slack card uses.
func TestCompletionAck_FillerTLDRNotEchoed(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	ack.handleClosed(context.Background(),
		mentionAgentClose("gd-AGENT7", "gardenio-jira-pe-7-x", "PE-7", "crew", "done",
			map[string]string{"wrapup": `{"tldr":"none"}`}))

	c := jira.getComments()
	if len(c) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(c))
	}
	// Body is exactly the Done line (no tldr echo); it must not contain "none".
	if strings.Contains(strings.ToLower(c[0].Text), "none") {
		t.Errorf("comment %q echoed a filler tldr", c[0].Text)
	}
}

// TestCompletionAck_BlockedOnFSMProject pins the hard invariant: FSM is customer-
// facing and the bot holds zero write there — no completion note, ever.
func TestCompletionAck_BlockedOnFSMProject(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	ack.handleClosed(context.Background(),
		mentionAgentClose("gd-AGENT8", "gardenio-jira-fsm-1-x", "FSM-1", "crew", "done", nil))

	if len(jira.getComments()) != 0 {
		t.Fatalf("expected NO note on FSM, got %d", len(jira.getComments()))
	}
}

// TestCompletionAck_BlockedOnUnknownProject pins deny-by-default: a project absent
// from the write policy gets no note.
func TestCompletionAck_BlockedOnUnknownProject(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	ack.handleClosed(context.Background(),
		mentionAgentClose("gd-AGENT9", "gardenio-jira-secret-9-x", "SECRET-9", "crew", "done", nil))

	if len(jira.getComments()) != 0 {
		t.Fatalf("expected NO note on unknown project, got %d", len(jira.getComments()))
	}
}

// TestCompletionAck_GBFSMAllowedRegardlessOfOutboxToggle pins the reply-predicate
// decoupling (ka-0000007628): a completion note is not an FSM/outbox write, so a
// GBFSM mention agent's terminal note posts whether the outbox toggle is on or
// off — exactly like the spawn-ack and reply paths.
func TestCompletionAck_GBFSMAllowedRegardlessOfOutboxToggle(t *testing.T) {
	for _, outboxOn := range []bool{false, true} {
		ack, jira := newTestCompletionAck(outboxOn)
		ack.handleClosed(context.Background(),
			mentionAgentClose("gd-AGENTG", "gardenio-jira-gbfsm-1-z", outboxProjectKey+"-1", "crew", "done", nil))
		if len(jira.getComments()) != 1 {
			t.Errorf("outbox=%v: expected 1 GBFSM note, got %d", outboxOn, len(jira.getComments()))
		}
	}
}

// TestCompletionAck_BlockedForRoleWithoutComment ensures the note honors the same
// per-role allowlist as every other comment path: fsm-triage may write labels but
// NOT comments, so it posts no note.
func TestCompletionAck_BlockedForRoleWithoutComment(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	ack.handleClosed(context.Background(),
		mentionAgentClose("gd-AGENTT", "gardenio-jira-pe-t", "PE-8", "fsm-triage", "done", nil))

	if len(jira.getComments()) != 0 {
		t.Fatalf("expected NO note for fsm-triage (no comment grant), got %d", len(jira.getComments()))
	}
}

// TestCompletionAck_BlockedForUnknownRole pins fail-closed role handling.
func TestCompletionAck_BlockedForUnknownRole(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	ack.handleClosed(context.Background(),
		mentionAgentClose("gd-AGENTU", "gardenio-jira-pe-u", "PE-9", "some-unshipped-role", "done", nil))

	if len(jira.getComments()) != 0 {
		t.Fatalf("expected NO note for unknown role, got %d", len(jira.getComments()))
	}
}

// TestCompletionAck_NilStoreFailsClosed pins that a nil write policy denies every
// note — an unwired policy can never open an ungated write surface.
func TestCompletionAck_NilStoreFailsClosed(t *testing.T) {
	jira := &mockJiraSquawkJira{}
	ack := NewJiraCompletionAcknowledger(JiraCompletionAcknowledgerConfig{
		Jira:        jira,
		WritePolicy: nil,
		Logger:      jiraSquawkTestLogger(),
	})

	ack.handleClosed(context.Background(),
		mentionAgentClose("gd-AGENTN", "gardenio-jira-pe-n", "PE-10", "crew", "done", nil))

	if len(jira.getComments()) != 0 {
		t.Fatalf("expected NO note with nil write policy, got %d", len(jira.getComments()))
	}
}

// TestCompletionAck_IgnoresNonMentionAgent pins the scope: an agent bead WITHOUT
// spawn_source=jira-mention (a plain crew agent that happens to carry a jira_key)
// is not ours — no note.
func TestCompletionAck_IgnoresNonMentionAgent(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	payload := marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-PLAIN",
		Type:     "agent",
		Title:    "gardenio-crew-plain",
		Assignee: "gardenio-crew-plain",
		Status:   "closed",
		Fields: map[string]string{
			"agent":       "gardenio-crew-plain",
			"jira_key":    "PE-11",
			"role":        "crew",
			"agent_state": "done",
			// no spawn_source
		},
	})
	ack.handleClosed(context.Background(), payload)

	if len(jira.getComments()) != 0 {
		t.Fatalf("expected NO note for a non-mention agent, got %d", len(jira.getComments()))
	}
}

// TestCompletionAck_IgnoresTaskBead pins that a closed TASK bead (not type=agent)
// is ignored — jira_sync owns the task-bead close note.
func TestCompletionAck_IgnoresTaskBead(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	payload := marshalSSEBeadPayload(BeadEvent{
		ID:     "gd-TASK",
		Type:   "task",
		Title:  "[PE-12] Fix bug",
		Status: "closed",
		Fields: map[string]string{
			"jira_key":     "PE-12",
			"spawn_source": "jira-mention", // even with the marker, type=task is not ours
			"role":         "crew",
		},
	})
	ack.handleClosed(context.Background(), payload)

	if len(jira.getComments()) != 0 {
		t.Fatalf("expected NO note for a task bead, got %d", len(jira.getComments()))
	}
}

// TestCompletionAck_NoJiraKeyNoOp pins that a mention agent with no jira_key
// binding posts nothing (defensive — the mention path always stamps a key).
func TestCompletionAck_NoJiraKeyNoOp(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	payload := marshalSSEBeadPayload(BeadEvent{
		ID:     "gd-NOKEY",
		Type:   "agent",
		Title:  "gardenio-jira-x",
		Status: "closed",
		Fields: map[string]string{
			"agent":        "gardenio-jira-x",
			"spawn_source": "jira-mention",
			"role":         "crew",
			"agent_state":  "done",
			// no jira_key
		},
	})
	ack.handleClosed(context.Background(), payload)

	if len(jira.getComments()) != 0 {
		t.Fatalf("expected NO note with no jira_key, got %d", len(jira.getComments()))
	}
}

// TestCompletionAck_DarkBeadIgnored pins that a dark-marked mention agent never
// flows a note upstream, mirroring every other outbound path.
func TestCompletionAck_DarkBeadIgnored(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	payload := marshalSSEBeadPayload(BeadEvent{
		ID:       "gd-DARK",
		Type:     "agent",
		Title:    "gardenio-jira-pe-d",
		Assignee: "gardenio-jira-pe-d",
		Status:   "closed",
		Labels:   []string{jiraDarkLabel},
		Fields: map[string]string{
			"agent":        "gardenio-jira-pe-d",
			"jira_key":     "PE-13",
			"spawn_source": "jira-mention",
			"role":         "crew",
			"agent_state":  "done",
		},
	})
	ack.handleClosed(context.Background(), payload)

	if len(jira.getComments()) != 0 {
		t.Fatalf("expected NO note for a dark-marked bead, got %d", len(jira.getComments()))
	}
}

// TestCompletionAck_DedupsRedeliveredClose pins that a redelivered terminal close
// (SSE reconnect) posts the note only once.
func TestCompletionAck_DedupsRedeliveredClose(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	payload := mentionAgentClose("gd-DEDUP", "gardenio-jira-pe-dd", "PE-14", "crew", "done", nil)
	ack.handleClosed(context.Background(), payload)
	ack.handleClosed(context.Background(), payload) // redelivered

	if len(jira.getComments()) != 1 {
		t.Fatalf("expected exactly 1 note after a redelivered close, got %d", len(jira.getComments()))
	}
}

// TestCompletionAck_StampsOperatorAttribution pins the honest "Acted by
// <operator>" line for a human operator — the raw operated_by must appear
// verbatim (the E2E harness greps for it).
func TestCompletionAck_StampsOperatorAttribution(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	ack.handleClosed(context.Background(),
		mentionAgentClose("gd-ATTR", "gardenio-jira-pe-a", "PE-15", "crew", "done",
			map[string]string{"operated_by": "slack:U0OPERATOR1"}))

	c := jira.getComments()
	if len(c) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(c))
	}
	if !strings.Contains(c[0].Text, "Acted by") || !strings.Contains(c[0].Text, "slack:U0OPERATOR1") {
		t.Errorf("comment %q missing operator attribution", c[0].Text)
	}
}

// TestCompletionAck_NoAttributionForNonHumanOperator pins that a non-human
// operator (a fleet: sentinel) earns NO attribution line — never a fake actor.
// The note still posts (attribution is additive).
func TestCompletionAck_NoAttributionForNonHumanOperator(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	ack.handleClosed(context.Background(),
		mentionAgentClose("gd-ATTR2", "gardenio-jira-pe-a2", "PE-16", "crew", "done",
			map[string]string{"operated_by": "fleet:jira-bridge"}))

	c := jira.getComments()
	if len(c) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(c))
	}
	if strings.Contains(c[0].Text, "Acted by") {
		t.Errorf("comment %q should carry NO attribution for a non-human operator", c[0].Text)
	}
}

// TestCompletionAck_PostFailureDoesNotPanic pins that a Jira post error is
// swallowed (logged, not propagated) — the agent has already finished.
func TestCompletionAck_PostFailureDoesNotPanic(t *testing.T) {
	jira := &mockJiraSquawkJira{err: context.DeadlineExceeded}
	ack := NewJiraCompletionAcknowledger(JiraCompletionAcknowledgerConfig{
		Jira:        jira,
		WritePolicy: enforcedWritePolicy(false),
		Logger:      jiraSquawkTestLogger(),
	})
	// Must not panic despite the post error.
	ack.handleClosed(context.Background(),
		mentionAgentClose("gd-ERR", "gardenio-jira-pe-e", "PE-17", "crew", "done", nil))
}

// TestCompletionAck_NilClientAndNilReceiverNoPanic pins that a partially-wired or
// absent acknowledger is a safe no-op — RegisterHandlers wires handleClosed onto
// the shared stream, so neither shape may panic.
func TestCompletionAck_NilClientAndNilReceiverNoPanic(t *testing.T) {
	// Nil Jira client: constructed but disabled.
	ackNoClient := NewJiraCompletionAcknowledger(JiraCompletionAcknowledgerConfig{
		Jira:        nil,
		WritePolicy: enforcedWritePolicy(false),
		Logger:      jiraSquawkTestLogger(),
	})
	ackNoClient.handleClosed(context.Background(),
		mentionAgentClose("gd-NC", "gardenio-jira-pe-nc", "PE-18", "crew", "done", nil))

	// Nil receiver: handleClosed on a nil acknowledger must no-op.
	var nilAck *JiraCompletionAcknowledger
	nilAck.handleClosed(context.Background(),
		mentionAgentClose("gd-NR", "gardenio-jira-pe-nr", "PE-19", "crew", "done", nil))
}

// TestCompletionAck_BodyTruncatedUnderCeiling pins the length cap: a body that
// would exceed jiraCompletionAckMaxLen is truncated, and the attribution line is
// appended AFTER truncation so it is never clipped.
func TestCompletionAck_BodyTruncatedUnderCeiling(t *testing.T) {
	longTLDR := strings.Repeat("x", jiraCompletionAckMaxLen+200)
	fields := map[string]string{
		"agent":        "gardenio-jira-pe-long",
		"agent_state":  "done",
		"operated_by":  "slack:U0OPERATOR1",
		"wrapup":       `{"tldr":"` + longTLDR + `"}`,
		"spawn_source": "jira-mention",
	}
	body := buildJiraCompletionBody("gardenio-jira-pe-long", fields)
	if !strings.Contains(body, "Acted by") || !strings.Contains(body, "slack:U0OPERATOR1") {
		t.Errorf("attribution line was clipped by truncation: %q", body)
	}
}

// TestClassifyJiraCompletion is a table-drive over the terminal-outcome
// classifier, pinning each Slack-parity verdict.
func TestClassifyJiraCompletion(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{"clean done", map[string]string{"agent_state": "done"}, "done"},
		{"explicit failed", map[string]string{"agent_state": "failed"}, "failed"},
		{"pod failed", map[string]string{"pod_phase": "failed"}, "failed"},
		{"done wins race", map[string]string{"agent_state": "done", "pod_phase": "failed"}, "done"},
		{"rate limited", map[string]string{"agent_state": "rate_limited"}, "rate_limited"},
		{"orphan reaper benign", map[string]string{"close_reason": "orphan: source task closed"}, "done"},
		{"abnormal reaped", map[string]string{"close_reason": "podless_leak: pod gone"}, "reaped"},
		{"empty is done", map[string]string{}, "done"},
	}
	for _, tc := range cases {
		if got := classifyJiraCompletion(tc.fields); got != tc.want {
			t.Errorf("%s: classifyJiraCompletion = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestCompletionAck_SuppressedOnQuietTicket pins the mention-time --quiet intent
// hint (design-doc F-E, epic ka-0000007401): a mention agent that closes carrying
// jira_quiet=true posts NO terminal ✅/❌ note, even on a writable project (PE)
// with a comment-granted role. Without the field the same close posts a Done note
// (TestCompletionAck_PostsDoneNote).
func TestCompletionAck_SuppressedOnQuietTicket(t *testing.T) {
	ack, jira := newTestCompletionAck(false)

	ack.handleClosed(context.Background(),
		mentionAgentClose("gd-AGENTQ", "gardenio-jira-pe-1234-abc", "PE-1234", "crew", "done",
			map[string]string{"jira_quiet": "true"}))

	if len(jira.getComments()) != 0 {
		t.Fatalf("expected no completion note on a --quiet ticket, got %d", len(jira.getComments()))
	}
}
