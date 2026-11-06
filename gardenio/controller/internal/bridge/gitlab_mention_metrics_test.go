package bridge

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestRecordMentionStaleTaskPassthrough pins the observability half of the
// ka-0000001106 fix: the stale-task passthrough counter increments each time
// hasPendingMention deliberately forwards a re-mention past a stale open task.
// Agents cannot read pod logs (ka-0000006777), so this counter is the sole
// machine-readable signal that the permanent-mute path is no longer silently
// dropping follow-ups.
func TestRecordMentionStaleTaskPassthrough(t *testing.T) {
	before := testutil.ToFloat64(gitlabMentionStaleTaskPassthroughTotal)
	recordMentionStaleTaskPassthrough()
	recordMentionStaleTaskPassthrough()
	after := testutil.ToFloat64(gitlabMentionStaleTaskPassthroughTotal)
	if delta := after - before; delta != 2 {
		t.Errorf("stale-task passthrough counter delta = %v; want 2", delta)
	}
}
