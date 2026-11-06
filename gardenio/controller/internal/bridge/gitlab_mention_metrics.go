package bridge

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// gardenio_gitlab_mention_stale_task_passthrough_total counts GitLab @mention
// re-mentions that the dedup gate deliberately let through because the only
// open mention task on the discussion was STALE (older than mentionDedupWindow)
// — i.e. the previously-silent permanent-mute bug (ka-0000001106).
//
// Before the fix, hasPendingMention returned true for ANY open mention task on
// the discussion, so an orphaned open task (agent crashed without closing it)
// muted every subsequent @gardenio on that thread forever, with no log or
// counter. Agents cannot read pod logs (ka-0000006777), so this counter is the
// observable signal that a discussion HAD a stale task and the re-mention was
// correctly forwarded to spawnMentionAgent (nudge-or-respawn) instead of
// dropped. A steadily climbing count is expected (iterative review threads
// outlive their first agent); it should never sit at zero forever if mentions
// are flowing on long-lived threads.
//
// Registered globally against the controller's default registerer (promauto),
// exported on the controller /metrics endpoint.
var gitlabMentionStaleTaskPassthroughTotal = promauto.NewCounter(
	prometheus.CounterOpts{
		Name: "gardenio_gitlab_mention_stale_task_passthrough_total",
		Help: "GitLab @mention re-mentions forwarded to spawn/nudge because the discussion's only open mention task was stale (older than the dedup window). Non-zero proves the ka-0000001106 permanent-mute path is no longer silently dropping follow-ups.",
	},
)

// recordMentionStaleTaskPassthrough increments the stale-task passthrough
// counter. Called from hasPendingMention at the decision site where a stale
// open task is deliberately NOT treated as pending.
func recordMentionStaleTaskPassthrough() {
	gitlabMentionStaleTaskPassthroughTotal.Inc()
}
