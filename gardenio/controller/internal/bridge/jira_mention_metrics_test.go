package bridge

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"gardenio/controller/internal/beadsapi"
)

// TestRecordJiraMentionStaleTaskPassthrough pins the observability half of the
// ka-0000007091 fix: the stale-task passthrough counter increments each time
// classifyPendingJiraMention deliberately forwards a re-mention past a stale
// open mention task. Agents cannot read pod logs (ka-0000006777), so this
// counter is the sole machine-readable signal that the Jira permanent-mute
// path is no longer silently dropping re-mentions.
func TestRecordJiraMentionStaleTaskPassthrough(t *testing.T) {
	before := testutil.ToFloat64(jiraMentionStaleTaskPassthroughTotal)
	recordJiraMentionStaleTaskPassthrough()
	recordJiraMentionStaleTaskPassthrough()
	after := testutil.ToFloat64(jiraMentionStaleTaskPassthroughTotal)
	if delta := after - before; delta != 2 {
		t.Errorf("jira stale-task passthrough counter delta = %v; want 2", delta)
	}
}

// TestJiraHandleMention_StaleReMentionIncrementsCounter wires the mention
// handler to the counter end-to-end: a stale-only re-mention must both return
// false (route to the webhook follow-up loop) AND bump the passthrough counter,
// so the previously-silent forward is observable.
func TestJiraHandleMention_StaleReMentionIncrementsCounter(t *testing.T) {
	daemon := &mockJiraMentionDaemon{
		createID: "task-bead-1",
		spawnID:  "agent-bead-1",
		beads: []*beadsapi.BeadDetail{
			{
				ID:        "stale-task",
				Status:    "open",
				CreatedAt: time.Now().Add(-30 * time.Minute),
				Fields:    map[string]string{"jira_key": "PE-1234", "spawn_source": "jira-mention"},
			},
		},
	}
	h := NewJiraMentionHandler(JiraMentionHandlerConfig{
		Users:   allowJiraMentionUsers{},
		Daemon:  daemon,
		Project: "gardenio",
		Logger:  jiraMentionTestLogger(),
	})

	before := testutil.ToFloat64(jiraMentionStaleTaskPassthroughTotal)
	handled := h.HandleMention(context.Background(),
		makeJiraMentionEvent("PE-1234", "Fix login bug", "@gardenio re-ping", "User"))
	after := testutil.ToFloat64(jiraMentionStaleTaskPassthroughTotal)

	if handled {
		t.Fatal("expected stale re-mention to return false")
	}
	if delta := after - before; delta != 1 {
		t.Errorf("passthrough counter delta = %v; want 1", delta)
	}
}
