package bridge

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Jira conversational-loop outcome metrics (epic ka-0000007401 — Slack parity,
// GAP 1 / ka-0000006498).
//
// The Slack conversational bridge instruments the whole loop end to end —
// gardenio_mention_delivery_total{warm|cold} (bot_mentions.go via
// assistant_event_metrics.go) forks warm-nudge from cold-respawn, and the
// spawn/reply legs are observable. The Jira conversational bridge shares the
// EXACT same shape — a fresh @gardenio mention spawns an agent (jira_mentions.go),
// a webhook follow-up forks warm-nudge (jira_agent_resolver.go resolveAndNudge
// live-agent branch) vs cold-respawn (its SpawnAgent branch), the agent replies
// via `gb jira reply` (jira_reply.go), and a spawn-ack note is posted
// (jira_spawn_ack.go) — but until now it emitted almost nothing on the SUCCESS
// side:
//
//   - jira_wake_retry.go already counts follow-up wake RETRIES + terminal
//     FAILURES ({stage=nudge|spawn}). Those are the error side only: there is
//     no counter for a SUCCESSFUL follow-up delivery, so the warm-hit ratio
//     (warm/(warm+cold)) — the Slack KPI for warm-idle keepalive — cannot be
//     computed for Jira, and a follow-up that the resolver deliberately SKIPS
//     (recent-spawn dedup) is invisible.
//   - jira_mentions.go spawns a fresh-mention agent with no success/failure
//     counter at all.
//   - jira_reply.go "just logs" the reply post outcome + visibility.
//   - jira_spawn_ack.go "just logs" the ack post outcome.
//
// These four families make each leg observable so the conversational loop's
// health is a scrape rather than a log-grep. Agents cannot read pod logs
// (ka-0000006777), so a counter is the only signal an agent (or a dashboard)
// has that, e.g., replies are landing (`posted` rising) versus being rejected
// by the write policy (`blocked` rising) or bouncing off Jira (`error` rising).
//
// All families are registered globally against the controller's default
// registerer (promauto), matching the other bridge metrics (jira_metrics.go,
// jira_mention_metrics.go, jira_inbound_comment_metrics.go). The jira-bridge
// serves /metrics from prometheus.DefaultGatherer, so these series are
// scrapeable; in the controller binary these paths never run, so the families
// stay empty. Every label is a HARD, bounded enum — the recorder functions drop
// (no increment) an out-of-enum value so a future caller cannot inflate
// cardinality, exactly as recordAssistantEvent does.

// --- Follow-up delivery outcome (warm/cold/skipped) ---------------------------

// jiraFollowUpDeliveryPath is the bounded enum for how the webhook follow-up
// path (resolveAndNudge) reached — or declined to reach — the agent.
const (
	// jiraDeliveryWarm: an alive agent bead was found (named agent or an active
	// agent bound to the jira_key) and a live nudge was delivered — the Slack
	// "warm" delivery path.
	jiraDeliveryWarm = "warm"
	// jiraDeliveryCold: no live agent — the resolver respawned a same-name agent
	// (session-continuity), the Slack "cold" delivery path.
	jiraDeliveryCold = "cold"
	// jiraDeliverySkipped: the recent-spawn dedup guard suppressed a respawn
	// (a spawn for this key already happened inside the window). Recorded so a
	// deliberately-dropped follow-up is not indistinguishable from one that
	// never arrived.
	jiraDeliverySkipped = "skipped"
)

func jiraFollowUpDeliveryPathValid(p string) bool {
	switch p {
	case jiraDeliveryWarm, jiraDeliveryCold, jiraDeliverySkipped:
		return true
	default:
		return false
	}
}

// gardenio_jira_followup_delivery_total counts Jira webhook follow-up deliveries
// by path — the Jira twin of gardenio_mention_delivery_total{warm|cold}. warm =
// live nudge (deliverNudge on an alive agent); cold = same-name respawn;
// skipped = recent-spawn dedup suppressed the respawn. warm/(warm+cold) is the
// Jira warm-hit ratio (the warm-idle keepalive KPI, epic ka-0000006987).
var jiraFollowUpDeliveryTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_jira_followup_delivery_total",
		Help: "Jira webhook follow-up deliveries by path (enum: warm=live nudge / cold=same-name respawn / skipped=recent-spawn dedup). warm/(warm+cold) is the Jira warm-hit ratio, the conversational twin of gardenio_mention_delivery_total (epic ka-0000006987).",
	},
	[]string{"path"},
)

// recordJiraFollowUpDelivery increments the follow-up delivery counter for one
// resolved follow-up. An out-of-enum path is DROPPED so cardinality stays
// bounded.
func recordJiraFollowUpDelivery(path string) bool {
	if !jiraFollowUpDeliveryPathValid(path) {
		return false
	}
	jiraFollowUpDeliveryTotal.WithLabelValues(path).Inc()
	return true
}

// --- Fresh-mention spawn outcome ---------------------------------------------

// jiraSpawnOutcome is the bounded enum for the terminal outcome of a
// fresh-@gardenio-mention spawn in jira_mentions.go.
const (
	jiraSpawnOK    = "ok"    // SpawnAgent returned an agent bead id
	jiraSpawnError = "error" // SpawnAgent returned an error (mention dropped)
)

func jiraSpawnOutcomeValid(o string) bool {
	switch o {
	case jiraSpawnOK, jiraSpawnError:
		return true
	default:
		return false
	}
}

// gardenio_jira_mention_spawn_total counts fresh-mention agent spawns by outcome
// (enum: ok/error). error rising means @gardenio mentions are being received but
// the agent never boots — the conversation dies before it starts, with the
// webhook already 200'd. This is the front-half twin of the follow-up
// wake-failure counter (jira_wake_retry.go), which only covers RESPAWNS.
var jiraMentionSpawnTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_jira_mention_spawn_total",
		Help: "Fresh @gardenio-mention agent spawns by outcome (enum: ok/error). error rising = mentions arriving but agents not booting (conversation dies at the front half). Complements gardenio_jira_followup_wake_failures_total, which covers respawns only.",
	},
	[]string{"outcome"},
)

// recordJiraMentionSpawn increments the fresh-mention spawn counter. An
// out-of-enum outcome is DROPPED so cardinality stays bounded.
func recordJiraMentionSpawn(outcome string) bool {
	if !jiraSpawnOutcomeValid(outcome) {
		return false
	}
	jiraMentionSpawnTotal.WithLabelValues(outcome).Inc()
	return true
}

// --- Agent reply post outcome (with visibility) ------------------------------

// jiraReplyOutcome is the bounded enum for the terminal outcome of a
// `gb jira reply` post through /api/jira/reply (jira_reply.go).
const (
	// jiraReplyPosted: the comment was posted to Jira (or an idempotent
	// dedup-collapse reported success without re-posting — both are a
	// user-visible "reply landed").
	jiraReplyPosted = "posted"
	// jiraReplyBlocked: the write policy / bound-ticket gate rejected the reply
	// before any Jira call (fail-closed 4xx from resolveReplyTarget).
	jiraReplyBlocked = "blocked"
	// jiraReplyError: the reply passed the gate but the Jira AddComment call
	// failed (transient or non-retryable — a 5xx/502 or 422 to the agent).
	jiraReplyError = "error"
)

func jiraReplyOutcomeValid(o string) bool {
	switch o {
	case jiraReplyPosted, jiraReplyBlocked, jiraReplyError:
		return true
	default:
		return false
	}
}

// jiraReplyVisibilityLabel bounds the visibility label to the two known comment
// visibilities plus a "none" bucket for outcomes recorded before a visibility
// is resolved (a malformed request rejected by the gate). Any other value
// collapses to "none" so cardinality stays bounded.
func jiraReplyVisibilityLabel(vis CommentVisibility) string {
	switch vis {
	case CommentInternal, CommentPublic:
		return string(vis)
	default:
		return "none"
	}
}

// gardenio_jira_reply_total counts agent `gb jira reply` posts by outcome and
// visibility (enum outcome: posted/blocked/error; visibility: internal/public/
// none). posted{visibility=public} rising is customer-facing reply volume;
// blocked rising means the write policy is refusing replies (misconfig or a
// non-replyable project); error rising means Jira is bouncing the post.
var jiraReplyTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_jira_reply_total",
		Help: "Agent `gb jira reply` posts by outcome and visibility (enum outcome: posted/blocked/error; visibility: internal/public/none). posted = reply landed on the ticket; blocked = write-policy/bound-ticket rejection; error = Jira post failed.",
	},
	[]string{"outcome", "visibility"},
)

// recordJiraReply increments the reply counter. An out-of-enum outcome is
// DROPPED; the visibility is normalized to the bounded label set.
func recordJiraReply(outcome string, vis CommentVisibility) bool {
	if !jiraReplyOutcomeValid(outcome) {
		return false
	}
	jiraReplyTotal.WithLabelValues(outcome, jiraReplyVisibilityLabel(vis)).Inc()
	return true
}

// --- Spawn-ack post outcome --------------------------------------------------

// jiraAckOutcome is the bounded enum for the terminal outcome of a spawn-ack
// note post (jira_spawn_ack.go).
const (
	jiraAckPosted  = "posted"  // the "👋 On it" internal note was posted
	jiraAckBlocked = "blocked" // the write policy / non-replyable project gate refused it
	jiraAckError   = "error"   // the gate passed but the Jira AddComment call failed
	jiraAckQuiet   = "quiet"   // a --quiet mention suppressed the note before any gate (design-doc F-E)
)

func jiraAckOutcomeValid(o string) bool {
	switch o {
	case jiraAckPosted, jiraAckBlocked, jiraAckError, jiraAckQuiet:
		return true
	default:
		return false
	}
}

// gardenio_jira_spawn_ack_total counts spawn-ack ("On it") note posts by outcome
// (enum: posted/blocked/error). It makes the FRONT half of the reaction
// lifecycle observable: posted rising tracks conversation starts; error rising
// means the ticket goes silent after a mention even though an agent booted.
var jiraSpawnAckTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_jira_spawn_ack_total",
		Help: "Jira spawn-ack (\"On it\") note posts by outcome (enum: posted/blocked/error/quiet). posted = the ticket acknowledged the mention; blocked = write-policy/non-replyable refusal; error = Jira post failed (ticket stays silent despite a booted agent); quiet = a --quiet mention deliberately suppressed the note.",
	},
	[]string{"outcome"},
)

// recordJiraSpawnAck increments the spawn-ack counter. An out-of-enum outcome
// is DROPPED so cardinality stays bounded.
func recordJiraSpawnAck(outcome string) bool {
	if !jiraAckOutcomeValid(outcome) {
		return false
	}
	jiraSpawnAckTotal.WithLabelValues(outcome).Inc()
	return true
}

// --- Startup pre-initialization ----------------------------------------------

// PreInitJiraConversationMetrics materializes every bounded label combination of
// the Jira conversational-loop CounterVecs at 0 so each labelled series exists
// on the very first scrape — before any mention, reply, ack, wake, or comment
// mirror has fired.
//
// Why this is needed (ka-0000004443): a Prometheus CounterVec exports NO time
// series for a label set until that set is first observed with WithLabelValues.
// Every counter in this family carries a label (outcome / path / stage /
// visibility / reason), so on a healthy but quiet Jira loop — which is the
// steady state, replies/acks/wake-failures/DLQ-drops are all rare or
// error-only — the series never come into existence and 17/27 dashboard panels
// render "No data" instead of a graphable flat 0. That is indistinguishable, at
// the panel, from a broken emitter, and it is exactly why the plain (labelless)
// Counter/Gauge siblings in this family (dlq_enqueued / dlq_replayed /
// dlq_depth) DO show up: they register at 0 immediately, a CounterVec does not.
//
// Pre-touching every bounded member with .WithLabelValues(...) (without
// incrementing) forces the series into existence at 0, the standard Prometheus
// idiom for exactly this problem. The enum sets touched here are the SAME
// bounded sets the record* guards enforce, so this cannot inflate cardinality:
// no path can ever emit a label value not already materialized here.
//
// This is called only from the jira-bridge binary's startup, matching the
// family's design contract — in the controller binary these emit paths never
// run, so the series intentionally stay absent there.
func PreInitJiraConversationMetrics() {
	// Fresh-mention spawn outcome.
	for _, outcome := range []string{jiraSpawnOK, jiraSpawnError} {
		jiraMentionSpawnTotal.WithLabelValues(outcome)
	}

	// Spawn-ack outcome.
	for _, outcome := range []string{jiraAckPosted, jiraAckBlocked, jiraAckError, jiraAckQuiet} {
		jiraSpawnAckTotal.WithLabelValues(outcome)
	}

	// Reply outcome × visibility. Every (outcome, visibility) the recorder can
	// emit: posted/blocked/error × internal/public/none.
	for _, outcome := range []string{jiraReplyPosted, jiraReplyBlocked, jiraReplyError} {
		for _, vis := range []string{string(CommentInternal), string(CommentPublic), "none"} {
			jiraReplyTotal.WithLabelValues(outcome, vis)
		}
	}

	// Follow-up delivery path.
	for _, path := range []string{jiraDeliveryWarm, jiraDeliveryCold, jiraDeliverySkipped} {
		jiraFollowUpDeliveryTotal.WithLabelValues(path)
	}

	// Follow-up wake retries + terminal failures, by stage.
	for _, stage := range []string{jiraWakeStageNudge, jiraWakeStageSpawn} {
		jiraFollowUpWakeRetries.WithLabelValues(stage)
		jiraFollowUpWakeFailures.WithLabelValues(stage)
	}

	// Inbound comment-mirror write retries + terminal failures, by stage. The
	// two stages are the literal labels the emit sites use (jira_webhook.go,
	// jira_inbound_comment_retry.go): the AddComment write and the reply-anchor
	// set_fields write.
	for _, stage := range []string{"add_comment", "set_fields"} {
		jiraInboundCommentWriteRetries.WithLabelValues(stage)
		jiraInboundCommentWriteFailures.WithLabelValues(stage)
	}

	// Inbound comment-mirror DLQ drops, by reason — the literal reasons the DLQ
	// emits (jira_inbound_comment_dlq.go / jira_webhook.go): a bounded-queue
	// overflow, a spent replay budget, a deterministic non-retryable failure
	// never queued, and a missing state manager.
	for _, reason := range []string{"overflow", "exhausted", "nontransient", "no_state"} {
		jiraInboundCommentDLQDropped.WithLabelValues(reason)
	}
}
