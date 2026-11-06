package bridge

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestE2E_JiraMention_MentionTaskHasDepToJiraMirror drives the full
// Jira webhook → handleJiraCommentWebhook → JiraMentionHandler.HandleMention →
// CreateBead + SpawnAgent + writeMirrorTriggeredBy path and asserts that
// the spawned mention task carries a `triggered-by` dep to the Jira-mirror
// bead labeled `jira:<key>`.
//
// Plan gd-T5vMf08jDN §4.1 (epic ka-0000000933 / M2c).
//
// Distinct from TestJiraHandleMention_WritesTriggeredByToMirrorBead
// (M2c unit test): that test calls HandleMention directly with a
// pre-built event. This e2e variant POSTs through the real
// JiraWebhookHandler, exercising HMAC-skip-via-DevMode, JSON decode,
// event routing, and the mention path together.
func TestE2E_JiraMention_MentionTaskHasDepToJiraMirror(t *testing.T) {
	const (
		jiraKey  = "PE-7777"
		mirrorID = "gd-jira-mirror-pe7777"
		taskID   = "gd-mention-task-1"
		agentID  = "gd-mention-agent-1"
	)

	// Webhook daemon (handles AddComment / nudge listing).
	whDaemon := newMockJiraWebhookDaemon()

	// Mention daemon: M2c writeMirrorTriggeredBy path needs the mirror bead
	// to be returned for `jira:<key>` label queries, plus a spawn-role
	// trigger bead for resolveJiraSpawnRole.
	mentionDaemon := &mockJiraMentionDaemon{
		createID:     taskID,
		spawnID:      agentID,
		triggerBeads: jiraTestSpawnTriggerBeads("gardenio"),
		mirrorBeads: []*beadsapi.BeadDetail{
			{ID: mirrorID, Labels: []string{"source:jira", "jira:" + jiraKey}},
		},
	}
	mentionHandler := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  mentionDaemon,
		Project: "gardenio",
		Logger:  slog.Default(),
	})

	handler := JiraWebhookHandler(JiraWebhookConfig{
		Daemon:         whDaemon,
		DevMode:        true, // skip HMAC for this e2e wire test
		MentionHandler: mentionHandler,
		Logger:         slog.Default(),
	})

	body := makeJiraWebhookPayload(
		"comment_created",
		jiraKey,
		"@gardenio please investigate this",
		"Jane Doe",
	)
	req := httptest.NewRequest(http.MethodPost, "/webhook", bytes.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("webhook POST returned %d, want 200 (body=%s)", w.Code, w.Body.String())
	}

	// Mention should have been handled: task created, agent spawned.
	if len(mentionDaemon.created) != 1 {
		t.Fatalf("expected 1 task bead created by mention handler, got %d", len(mentionDaemon.created))
	}
	if len(mentionDaemon.spawned) != 1 {
		t.Fatalf("expected 1 agent spawned by mention handler, got %d", len(mentionDaemon.spawned))
	}

	// Assert: tracking task → triggered-by → mirror bead.
	if len(mentionDaemon.addDepCalls) != 1 {
		t.Fatalf("expected exactly 1 AddDependency call (triggered-by → mirror), got %d: %+v",
			len(mentionDaemon.addDepCalls), mentionDaemon.addDepCalls)
	}
	dep := mentionDaemon.addDepCalls[0]
	if dep.BeadID != taskID {
		t.Errorf("triggered-by dep BeadID = %q, want %q (the spawned mention task)",
			dep.BeadID, taskID)
	}
	if dep.DependsOnID != mirrorID {
		t.Errorf("triggered-by dep DependsOnID = %q, want %q (the Jira mirror)",
			dep.DependsOnID, mirrorID)
	}
	if dep.DepType != "triggered-by" {
		t.Errorf("dep type = %q, want triggered-by", dep.DepType)
	}
	if dep.CreatedBy != "jira-bridge" {
		t.Errorf("dep CreatedBy = %q, want jira-bridge", dep.CreatedBy)
	}

	// Read-only mirror contract (advice ka-0000003234): the mention path must
	// NOT mutate the mirror bead via the webhook daemon (UpdateBeadFields is
	// only invoked for matched task beads with jira_key, which mirrorID is
	// not — so the webhook daemon should hold no record for it).
	if got := whDaemon.getBead(mirrorID); got != nil {
		t.Errorf("mention path must not write to Jira mirror bead (got %+v)", got)
	}
}
