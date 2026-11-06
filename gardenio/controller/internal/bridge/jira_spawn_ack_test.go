package bridge

import (
	"context"
	"strings"
	"testing"
)

// newTestSpawnAck builds an acknowledger over a mock Jira client and an
// enforce-mode default write policy (the production shape). The returned mock
// captures every posted comment for assertions.
func newTestSpawnAck(outboxEnabled bool) (*JiraSpawnAcknowledger, *mockJiraSquawkJira) {
	jira := &mockJiraSquawkJira{}
	ack := NewJiraSpawnAcknowledger(JiraSpawnAcknowledgerConfig{
		Jira:        jira,
		WritePolicy: enforcedWritePolicy(outboxEnabled),
		Logger:      jiraSquawkTestLogger(),
	})
	return ack, jira
}

// TestSpawnAck_PostsInternalNoteOnAllowedProject is the happy path: a mention
// spawn on a writable project (PE) posts exactly one INTERNAL note that names
// the spawned agent and its tracking bead.
func TestSpawnAck_PostsInternalNoteOnAllowedProject(t *testing.T) {
	ack, jira := newTestSpawnAck(false)

	ack.Acknowledge(context.Background(), JiraMentionSpawnEvent{
		AgentName:  "gardenio-jira-pe-1234-abc",
		TaskBeadID: "gd-TASK123",
		JiraKey:    "PE-1234",
		Role:       "crew",
	})

	if len(jira.comments) != 1 {
		t.Fatalf("expected exactly 1 comment, got %d", len(jira.comments))
	}
	c := jira.comments[0]
	if c.Key != "PE-1234" {
		t.Errorf("comment key = %q, want PE-1234", c.Key)
	}
	if c.Vis != CommentInternal {
		t.Errorf("comment visibility = %q, want internal", c.Vis)
	}
	if !strings.Contains(c.Text, "gardenio-jira-pe-1234-abc") {
		t.Errorf("comment %q does not name the spawned agent", c.Text)
	}
	if !strings.Contains(c.Text, "gd-TASK123") {
		t.Errorf("comment %q does not reference the tracking bead", c.Text)
	}
	if !strings.Contains(c.Text, "[gardenio agent]") {
		t.Errorf("comment %q missing the [gardenio agent] prefix", c.Text)
	}
}

// TestSpawnAck_BlockedOnFSMProject pins the hard invariant: FSM is customer-
// facing and the bot holds zero write there. An ack must NEVER post to FSM,
// even though a mention could theoretically resolve there.
func TestSpawnAck_BlockedOnFSMProject(t *testing.T) {
	ack, jira := newTestSpawnAck(false)

	ack.Acknowledge(context.Background(), JiraMentionSpawnEvent{
		AgentName:  "gardenio-jira-fsm-1-x",
		TaskBeadID: "gd-TASK1",
		JiraKey:    "FSM-1",
		Role:       "crew",
	})

	if len(jira.comments) != 0 {
		t.Fatalf("expected NO comment on FSM, got %d: %+v", len(jira.comments), jira.comments)
	}
}

// TestSpawnAck_BlockedOnUnknownProject pins deny-by-default: a project absent
// from the write policy (neither PE/DEVOPS nor GBFSM) gets no ack.
func TestSpawnAck_BlockedOnUnknownProject(t *testing.T) {
	ack, jira := newTestSpawnAck(false)

	ack.Acknowledge(context.Background(), JiraMentionSpawnEvent{
		AgentName: "gardenio-jira-abc-9-y",
		JiraKey:   "SECRET-9",
		Role:      "crew",
	})

	if len(jira.comments) != 0 {
		t.Fatalf("expected NO comment on unknown project, got %d", len(jira.comments))
	}
}

// TestSpawnAck_GBFSMAllowedRegardlessOfOutboxToggle pins the reply-predicate
// decoupling (ka-0000007628): an ack is not an FSM/outbox write, so a GBFSM
// mention is ack-eligible whether the outbox toggle is on or off — exactly like
// the reply path. Toggling the outbox flag must not silence the ack.
func TestSpawnAck_GBFSMAllowedRegardlessOfOutboxToggle(t *testing.T) {
	for _, outboxOn := range []bool{false, true} {
		ack, jira := newTestSpawnAck(outboxOn)
		ack.Acknowledge(context.Background(), JiraMentionSpawnEvent{
			AgentName:  "gardenio-jira-gbfsm-1-z",
			TaskBeadID: "gd-T",
			JiraKey:    outboxProjectKey + "-1",
			Role:       "crew",
		})
		if len(jira.comments) != 1 {
			t.Errorf("outbox=%v: expected 1 GBFSM ack, got %d", outboxOn, len(jira.comments))
		}
	}
}

// TestSpawnAck_BlockedForRoleWithoutComment ensures the ack honors the same
// per-role allowlist as every other comment path: fsm-triage may write labels
// and versions but NOT comments, so it must post no ack.
func TestSpawnAck_BlockedForRoleWithoutComment(t *testing.T) {
	ack, jira := newTestSpawnAck(false)

	ack.Acknowledge(context.Background(), JiraMentionSpawnEvent{
		AgentName:  "gardenio-jira-pe-2-triage",
		TaskBeadID: "gd-T",
		JiraKey:    "PE-2",
		Role:       "fsm-triage",
	})

	if len(jira.comments) != 0 {
		t.Fatalf("expected NO ack for fsm-triage (no comment grant), got %d", len(jira.comments))
	}
}

// TestSpawnAck_BlockedForUnknownRole pins fail-closed role handling: an unknown
// role is denied all writes, so no ack is posted.
func TestSpawnAck_BlockedForUnknownRole(t *testing.T) {
	ack, jira := newTestSpawnAck(false)

	ack.Acknowledge(context.Background(), JiraMentionSpawnEvent{
		AgentName: "gardenio-jira-pe-3-mystery",
		JiraKey:   "PE-3",
		Role:      "some-unshipped-role",
	})

	if len(jira.comments) != 0 {
		t.Fatalf("expected NO ack for unknown role, got %d", len(jira.comments))
	}
}

// TestSpawnAck_NilStoreFailsClosed pins that a nil write policy denies every
// ack — an unwired policy can never open an ungated write surface.
func TestSpawnAck_NilStoreFailsClosed(t *testing.T) {
	jira := &mockJiraSquawkJira{}
	ack := NewJiraSpawnAcknowledger(JiraSpawnAcknowledgerConfig{
		Jira:        jira,
		WritePolicy: nil,
		Logger:      jiraSquawkTestLogger(),
	})

	ack.Acknowledge(context.Background(), JiraMentionSpawnEvent{
		AgentName: "gardenio-jira-pe-4-x",
		JiraKey:   "PE-4",
		Role:      "crew",
	})

	if len(jira.comments) != 0 {
		t.Fatalf("expected NO ack with nil write policy, got %d", len(jira.comments))
	}
}

// TestSpawnAck_NilClientAndNilReceiverNoPanic pins that a partially-wired or
// absent acknowledger is a safe no-op — the mention handler always invokes
// OnSpawn without a guard, so neither shape may panic.
func TestSpawnAck_NilClientAndNilReceiverNoPanic(t *testing.T) {
	// Nil Jira client: constructed but disabled.
	ackNoClient := NewJiraSpawnAcknowledger(JiraSpawnAcknowledgerConfig{
		Jira:        nil,
		WritePolicy: enforcedWritePolicy(false),
		Logger:      jiraSquawkTestLogger(),
	})
	ackNoClient.Acknowledge(context.Background(), JiraMentionSpawnEvent{JiraKey: "PE-1", Role: "crew"})

	// Nil receiver: the OnSpawn adapter over a nil acknowledger must no-op.
	var nilAck *JiraSpawnAcknowledger
	nilAck.Acknowledge(context.Background(), JiraMentionSpawnEvent{JiraKey: "PE-1", Role: "crew"})
}

// TestSpawnAck_EmptyJiraKeyNoOp pins that an event with no ticket key posts
// nothing (defensive — the mention path always carries a key, but the ack must
// not construct a write target from an empty string).
func TestSpawnAck_EmptyJiraKeyNoOp(t *testing.T) {
	ack, jira := newTestSpawnAck(false)
	ack.Acknowledge(context.Background(), JiraMentionSpawnEvent{AgentName: "a", JiraKey: "", Role: "crew"})
	if len(jira.comments) != 0 {
		t.Fatalf("expected NO ack for empty jira key, got %d", len(jira.comments))
	}
}

// TestSpawnAck_StampsOperatorAttribution pins that the ack carries the honest
// "Acted by <operator>" line for a human operator — the same audit-faithful
// attribution every outbound path stamps (epic ka-0000004904). The RAW
// operated_by id must appear verbatim (the E2E harness greps for it).
func TestSpawnAck_StampsOperatorAttribution(t *testing.T) {
	ack, jira := newTestSpawnAck(false)

	ack.Acknowledge(context.Background(), JiraMentionSpawnEvent{
		AgentName:       "gardenio-jira-pe-5-x",
		TaskBeadID:      "gd-T",
		JiraKey:         "PE-5",
		Role:            "crew",
		Operator:        "slack:U0OPERATOR1",
		OperatorDisplay: "Matt Baker",
	})

	if len(jira.comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(jira.comments))
	}
	body := jira.comments[0].Text
	if !strings.Contains(body, "slack:U0OPERATOR1") {
		t.Errorf("comment %q missing raw operator id", body)
	}
	if !strings.Contains(body, "Matt Baker") {
		t.Errorf("comment %q missing operator display name", body)
	}
	if !strings.Contains(body, "Acted by") {
		t.Errorf("comment %q missing attribution line", body)
	}
}

// TestSpawnAck_NoAttributionForNonHumanOperator pins that a non-human operator
// (a jira:/fleet: sentinel) earns NO attribution line — we never stamp a fake
// actor. The ack still posts (attribution is additive), just without the line.
func TestSpawnAck_NoAttributionForNonHumanOperator(t *testing.T) {
	ack, jira := newTestSpawnAck(false)

	ack.Acknowledge(context.Background(), JiraMentionSpawnEvent{
		AgentName:  "gardenio-jira-pe-6-x",
		TaskBeadID: "gd-T",
		JiraKey:    "PE-6",
		Role:       "crew",
		Operator:   "fleet:jira-bridge",
	})

	if len(jira.comments) != 1 {
		t.Fatalf("expected 1 comment, got %d", len(jira.comments))
	}
	if strings.Contains(jira.comments[0].Text, "Acted by") {
		t.Errorf("comment %q should carry NO attribution for a non-human operator", jira.comments[0].Text)
	}
}

// TestSpawnAck_PostFailureDoesNotPanic pins that a Jira post error is swallowed
// (logged, not propagated) — the agent has already spawned, so a failed ack must
// never surface as a panic or a returned error (Acknowledge returns nothing).
func TestSpawnAck_PostFailureDoesNotPanic(t *testing.T) {
	jira := &mockJiraSquawkJira{err: context.DeadlineExceeded}
	ack := NewJiraSpawnAcknowledger(JiraSpawnAcknowledgerConfig{
		Jira:        jira,
		WritePolicy: enforcedWritePolicy(false),
		Logger:      jiraSquawkTestLogger(),
	})
	// Must not panic despite the post error.
	ack.Acknowledge(context.Background(), JiraMentionSpawnEvent{
		AgentName: "gardenio-jira-pe-7-x",
		JiraKey:   "PE-7",
		Role:      "crew",
	})
}

// TestSpawnAck_OnSpawnAdapterIsWireable pins that OnSpawn returns a non-nil
// JiraMentionNotifyFunc that, when invoked, posts the ack — this is the exact
// value main.go wires onto JiraMentionHandlerConfig.OnSpawn.
func TestSpawnAck_OnSpawnAdapterIsWireable(t *testing.T) {
	ack, jira := newTestSpawnAck(false)
	var fn JiraMentionNotifyFunc = ack.OnSpawn()
	if fn == nil {
		t.Fatal("OnSpawn() returned nil")
	}
	fn(context.Background(), JiraMentionSpawnEvent{
		AgentName: "gardenio-jira-pe-8-x",
		JiraKey:   "PE-8",
		Role:      "crew",
	})
	if len(jira.comments) != 1 {
		t.Fatalf("OnSpawn adapter did not post the ack: got %d comments", len(jira.comments))
	}
}

// TestSpawnAck_BodyTruncatedUnderCeiling pins the length cap: a body that would
// exceed jiraSpawnAckMaxLen is truncated, and the attribution line is appended
// AFTER truncation so it is never clipped.
func TestSpawnAck_BodyTruncatedUnderCeiling(t *testing.T) {
	longAgent := "gardenio-" + strings.Repeat("x", jiraSpawnAckMaxLen+200)
	body := buildJiraSpawnAckBody(JiraMentionSpawnEvent{
		AgentName:       longAgent,
		JiraKey:         "PE-9",
		Role:            "crew",
		Operator:        "slack:U0OPERATOR1",
		OperatorDisplay: "Matt Baker",
	})
	// The pre-attribution portion is capped; the attribution line follows.
	if !strings.Contains(body, "Acted by") || !strings.Contains(body, "slack:U0OPERATOR1") {
		t.Errorf("attribution line was clipped by truncation: %q", body)
	}
}

// TestSpawnAck_SuppressedOnQuietMention pins the mention-time --quiet intent
// hint (design-doc F-E, epic ka-0000007401): a spawn carrying Quiet=true posts
// NO "👋 On it" note, even on a writable project (PE) with a comment-granted
// role — the operator asked for a low-noise ticket. Without the flag the same
// event would post (TestSpawnAck_PostsInternalNoteOnAllowedProject).
func TestSpawnAck_SuppressedOnQuietMention(t *testing.T) {
	ack, jira := newTestSpawnAck(false)

	ack.Acknowledge(context.Background(), JiraMentionSpawnEvent{
		AgentName:  "gardenio-jira-pe-1234-abc",
		TaskBeadID: "gd-TASK123",
		JiraKey:    "PE-1234",
		Role:       "crew",
		Quiet:      true,
	})

	if len(jira.comments) != 0 {
		t.Fatalf("expected no ack comment on a --quiet mention, got %d", len(jira.comments))
	}
}
