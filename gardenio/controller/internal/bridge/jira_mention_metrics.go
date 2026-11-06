package bridge

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// gardenio_jira_mention_stale_task_passthrough_total counts Jira @gardenio
// re-mentions that the dedup gate deliberately let through because the only
// open jira-mention task on the ticket was STALE (older than
// jiraMentionDedupWindow) — the Jira twin of the GitLab permanent-mute bug
// (ka-0000001106, MR !5808) filed as ka-0000007091.
//
// Before the fix, hasPendingJiraMention returned true for ANY open mention
// task on the ticket, so a re-mention (@gardenio re-tagged on a ticket that
// already had an open mention task) made HandleMention return true from its
// dedup branch WITHOUT delivering to the agent, and the webhook then skipped
// its own follow-up loop because mentionHandled==true. The comment was
// delivered to neither path — silently swallowed. Because deliverJiraFollowUp
// is also the F3 respawn path, a re-mention on a ticket whose agent had
// STOPPED was permanently muted, exactly the GitLab failure mode.
//
// Bounding the suppression to a short window means a genuine re-mention
// minutes later is classified as a stale re-mention and routed to the webhook
// follow-up loop (nudge live / respawn dead) instead of being dropped. Agents
// cannot read pod logs (ka-0000006777), so this counter is the observable
// signal that a ticket HAD a stale mention task and the re-mention was
// correctly forwarded rather than muted. A steadily climbing count is expected
// on long-lived tickets (iterative conversations outlive their first agent);
// it should never sit at zero forever if @gardenio conversations are flowing.
//
// Registered globally against the controller's default registerer (promauto),
// exported on the controller /metrics endpoint (matching the GitLab twin).
var jiraMentionStaleTaskPassthroughTotal = promauto.NewCounter(
	prometheus.CounterOpts{
		Name: "gardenio_jira_mention_stale_task_passthrough_total",
		Help: "Jira @gardenio re-mentions routed to the follow-up loop (nudge/respawn) because the ticket's only open mention task was stale (older than the dedup window). Non-zero proves the ka-0000007091 permanent-mute path is no longer silently dropping re-mentions.",
	},
)

// recordJiraMentionStaleTaskPassthrough increments the stale-task passthrough
// counter. Called from classifyPendingJiraMention at the decision site where a
// stale open mention task is deliberately NOT treated as a fresh duplicate.
func recordJiraMentionStaleTaskPassthrough() {
	jiraMentionStaleTaskPassthroughTotal.Inc()
}
