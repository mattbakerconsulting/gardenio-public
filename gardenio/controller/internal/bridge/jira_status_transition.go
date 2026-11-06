// Jira status-transition-to-non-actionable handling (ka-0000005172).
//
// The JQL poller/trigger only PICK UP tickets while their status is actionable
// (an operator-configured allowlist — JIRA_STATUSES / a trigger's JQL). Once a
// ticket is in-flight (a task bead is open and, usually, an agent is working
// it), nothing watched for the ticket transitioning OUT of an actionable status:
// jira_sync.go syncs bead→Jira (claim/close/MR-link) but there was no
// Jira-status-change → reconsider-in-flight-work path. So if a human
// deprioritized or cancelled a ticket while an agent was mid-work, nothing
// signalled the agent to stop — it could still apply live infra changes or
// open/merge MRs for work no longer wanted (the DEVOPS-949 case, thread
// ka-0000002320).
//
// This file closes that gap. A jira:issue_updated webhook already arrives with a
// changelog on every field edit (the mention path reads the description item);
// here we read the STATUS item. When a ticket transitions INTO a non-actionable
// status and it has an OPEN task bead, we:
//
//   - stamp a durable jira_nonactionable flag + the new jira_status on the bead
//     (so downstream gating — e.g. before a live terraform apply or an MR merge —
//     can re-check, per the bead's proposal step 2), and post an internal note
//     to the bead recording the transition;
//   - if a live agent owns the bead: NUDGE it to re-evaluate before continuing.
//     We deliberately do NOT force the bead's status here — the owning agent may
//     be mid-irreversible-action and must decide (wind down / close / continue);
//     yanking work out from under it is exactly what the proposal warns against;
//   - if NO agent owns the bead (queued, "left open and waiting"): PAUSE it by
//     transitioning the bead to `blocked`, since no agent will otherwise
//     re-evaluate it. This is the acceptance's "paused rather than left open".
//
// Scope note on "Refine": the crew task prompt listed Refine among the
// non-actionable statuses, but gardenio's own DEVOPS pickup JQL classifies Refine
// as ACTIONABLE (status in (To Do, Open, Backlog, Refine, Ready, ...)). Pausing
// in-flight work on a move to Refine would therefore contradict the ingest
// filter, so Refine is intentionally EXCLUDED from the built-in default set. An
// operator who wants it can add it via JIRA_NONACTIONABLE_STATUSES.
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"gardenio/controller/internal/beadsapi"
)

// defaultJiraNonActionableStatuses is the conservative built-in set of Jira
// status names treated as non-actionable. Matching is case-insensitive and
// whitespace-trimmed (see normalizeJiraStatus). These are the unambiguously
// terminal/parked states from the bead's proposal (ka-0000005172): a ticket in
// any of them is not wanted RIGHT NOW, so an in-flight gardenio task for it should
// be reconsidered. "Refine" is deliberately absent — see the file header.
var defaultJiraNonActionableStatuses = []string{
	"Deprioritized",
	"Cancelled",
	"Canceled", // US spelling — some Jira workflows use it
	"Won't Do",
	"Won't Fix",
	"Abandoned",
	"Done",
	"Closed",
	"Resolved",
}

// jiraStatusReconsiderTotal counts reconsider actions taken when a ticket
// transitions into a non-actionable status, labelled by outcome:
//   - "paused"  — an unattended (no live assignee) task bead was moved to blocked
//   - "nudged"  — a live agent owning the bead was signalled to re-evaluate
//   - "flagged" — the bead was flagged but neither paused nor nudged (e.g. an
//     assigned bead whose nudge delivery failed, or a nudge-less config)
//
// Agents cannot read bridge pod logs (ka-0000006777), so this counter is the
// observable signal that the reconsider path fired. Registered against the
// controller's default registerer (promauto), exported on /metrics.
var jiraStatusReconsiderTotal = promauto.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_jira_status_reconsider_total",
		Help: "In-flight gardenio task beads reconsidered because their Jira ticket transitioned into a non-actionable status, by outcome (paused/nudged/flagged).",
	},
	[]string{"outcome"},
)

// jiraStatusReconsiderDedupWindow bounds how long a (bead, target-status)
// reconsider is suppressed after firing once, so a webhook redelivery (Jira
// retries, at-least-once delivery) does not repost the note or re-nudge. The
// from≠non-actionable → to=non-actionable edge already makes ordinary re-edits
// idempotent; this covers exact-duplicate redelivery of the same transition.
const jiraStatusReconsiderDedupWindow = 30 * time.Minute

// statusChange returns the status field's before/after names and true when this
// changelog contains a status transition. Jira identifies the field by fieldId
// "status" (Cloud) or field name "status" (Server) — the same dual-key match the
// sibling descriptionChange (jira_webhook.go) uses. FromString/ToString are the
// human-readable status NAMES (e.g. "In Progress" → "Done").
func (c *jiraChangelog) statusChange() (from, to string, ok bool) {
	if c == nil {
		return "", "", false
	}
	for _, item := range c.Items {
		if item.FieldID == "status" || item.Field == "status" {
			return item.FromString, item.ToString, true
		}
	}
	return "", "", false
}

// normalizeJiraStatus canonicalizes a Jira status name for set membership:
// trimmed and lower-cased. Jira status names are case-insensitive labels, so
// "Won't Do" and "won't do" are the same state.
func normalizeJiraStatus(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// nonActionableStatusSet builds the lookup set from the config's override list,
// falling back to defaultJiraNonActionableStatuses when unset. Keys are
// normalized. A single blank/whitespace entry does not produce a match (the
// empty string is never a real transition target).
func nonActionableStatusSet(configured []string) map[string]struct{} {
	list := configured
	if len(list) == 0 {
		list = defaultJiraNonActionableStatuses
	}
	set := make(map[string]struct{}, len(list))
	for _, s := range list {
		n := normalizeJiraStatus(s)
		if n == "" {
			continue
		}
		set[n] = struct{}{}
	}
	return set
}

// handleJiraStatusTransition reacts to a jira:issue_updated event that carries a
// STATUS transition into a non-actionable status, reconsidering any open task
// bead for the ticket. It is a no-op unless the changelog shows a status change
// whose target is non-actionable and whose source is NOT (the transition edge).
//
// The throttle is the webhook's shared nudgeThrottle; a namespaced key dedups
// exact webhook redelivery of the same (bead, target-status) transition.
func handleJiraStatusTransition(ctx context.Context, event jiraWebhookEvent, cfg JiraWebhookConfig, throttle *nudgeThrottle, logger *slog.Logger) {
	if event.Issue == nil || event.Issue.Key == "" {
		return
	}
	jiraKey := event.Issue.Key

	from, to, ok := event.Changelog.statusChange()
	if !ok {
		return // not a status transition
	}

	set := nonActionableStatusSet(cfg.NonActionableStatuses)
	_, toNonActionable := set[normalizeJiraStatus(to)]
	if !toNonActionable {
		return // still (or now) actionable — nothing to reconsider
	}
	if _, fromNonActionable := set[normalizeJiraStatus(from)]; fromNonActionable {
		// Already non-actionable before this edit; only act on the transition
		// edge, not on every subsequent edit while parked. Also makes a
		// non-actionable → non-actionable move (e.g. Done → Closed) a no-op.
		logger.Debug("jira-webhook: status transition between non-actionable states — ignoring",
			"jira_key", jiraKey, "from", from, "to", to)
		return
	}

	// Loop guard: never react to a transition the bot itself made. jira_sync
	// transitions tickets bead→Jira (claim → In Progress, MR-merge → Review), and
	// an MR-merge flow can legitimately drive a ticket to Done/Closed; reacting to
	// the bridge's own move would pause the very agent that just finished the work.
	editor := event.User
	if editor != nil {
		if cfg.BotAccountID != "" && editor.AccountID == cfg.BotAccountID {
			logger.Debug("jira-webhook: ignoring bot-authored status transition",
				"jira_key", jiraKey, "from", from, "to", to)
			return
		}
		if isBotAuthoredComment(editor.AccountID, cfg.BotAccountIDs) {
			logger.Debug("jira-webhook: ignoring bot-authored status transition (account set)",
				"jira_key", jiraKey, "from", from, "to", to)
			return
		}
	}

	beads, err := cfg.Daemon.ListTaskBeadsForJiraKey(ctx, jiraKey)
	if err != nil {
		logger.Error("jira-webhook: failed to list beads for status transition",
			"jira_key", jiraKey, "error", err)
		return
	}

	acted := false
	for _, bead := range beads {
		// Belt-and-suspenders: the server-side predicate already narrows by
		// jira_key, but re-check to match the comment path's guard exactly.
		if bead.Fields["jira_key"] != jiraKey {
			continue
		}
		// Only OPEN work is "in-flight". A closed bead is finished (the work
		// already landed or was abandoned); a blocked bead is already paused.
		if !jiraTaskBeadInFlight(bead.Status) {
			continue
		}
		// Respect the Jira-dark opt-out, mirroring jira_sync/completion-ack.
		if hasLabel(bead.Labels, jiraDarkLabel) {
			logger.Debug("jira-webhook: skipping dark bead for status transition",
				"bead", bead.ID, "jira_key", jiraKey)
			continue
		}
		// Dedup exact webhook redelivery of the same transition per bead.
		dedupKey := "jira-status-reconsider:" + bead.ID + ":" + normalizeJiraStatus(to)
		if throttle != nil && !throttle.Allow(dedupKey) {
			logger.Info("jira-webhook: status reconsider throttled (duplicate delivery)",
				"bead", bead.ID, "jira_key", jiraKey, "to", to)
			continue
		}
		acted = true
		reconsiderJiraTaskBead(ctx, cfg, bead, jiraKey, from, to, logger)
	}

	if !acted {
		logger.Debug("jira-webhook: no in-flight task bead to reconsider for status transition",
			"jira_key", jiraKey, "from", from, "to", to)
	}
}

// jiraTaskBeadInFlight reports whether a task bead status counts as in-flight
// work to reconsider: open or in_progress. Closed (finished) and blocked
// (already paused) are excluded.
func jiraTaskBeadInFlight(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "open", "in_progress":
		return true
	default:
		return false
	}
}

// reconsiderJiraTaskBead applies the reconsider action to one in-flight task
// bead: flag it, note the transition, and either nudge its live agent or pause
// the bead when unattended. See the file header for the rationale behind the
// nudge-vs-pause split.
func reconsiderJiraTaskBead(ctx context.Context, cfg JiraWebhookConfig, bead *beadsapi.BeadDetail, jiraKey, from, to string, logger *slog.Logger) {
	attended := strings.TrimSpace(bead.Assignee) != ""

	// Durable flag stamped on the bead so a later gate (before a live infra apply
	// or an MR merge — proposal step 2) can re-check that the ticket is still
	// wanted. jira_status is updated to the transition target to stay consistent
	// with the poller's mirror of the current status.
	fields := map[string]any{
		"jira_nonactionable":        "true",
		"jira_nonactionable_status": to,
		"jira_status":               to,
	}
	req := beadsapi.UpdateBeadRequest{Fields: fields}
	if !attended {
		// Unattended queued work: pause it so it stops "waiting" for an agent
		// that will never re-evaluate it. Reversible — an operator or a later
		// agent can unblock if the ticket is reactivated.
		blocked := "blocked"
		req.Status = &blocked
	}
	if err := retryJiraInboundWrite(ctx, "status_reconsider_flag", logger, func() error {
		return cfg.Daemon.UpdateBead(ctx, bead.ID, req)
	}); err != nil {
		logger.Error("jira-webhook: failed to flag/pause bead on non-actionable transition",
			"bead", bead.ID, "jira_key", jiraKey, "to", to, "error", err)
		// Still try to leave the note + nudge below so the signal is not lost.
	}

	note := buildJiraStatusReconsiderNote(jiraKey, from, to, attended)
	if err := retryJiraInboundWrite(ctx, "status_reconsider_note", logger, func() error {
		return cfg.Daemon.AddComment(ctx, bead.ID, "jira-bridge", note)
	}); err != nil {
		logger.Error("jira-webhook: failed to add status-reconsider note to bead",
			"bead", bead.ID, "jira_key", jiraKey, "error", err)
	}

	if !attended {
		logger.Info("jira-webhook: paused unattended task bead on non-actionable Jira transition",
			"bead", bead.ID, "jira_key", jiraKey, "from", from, "to", to)
		jiraStatusReconsiderTotal.WithLabelValues("paused").Inc()
		return
	}

	// Live agent owns the bead — signal it to re-evaluate before continuing. We
	// nudge (not respawn): the goal is to make a RUNNING agent reconsider, not to
	// resurrect a stopped one for work that may be unwanted.
	nudged := false
	if cfg.Nudge != nil {
		msg := buildJiraStatusReconsiderNudge(jiraKey, bead.ID, from, to)
		if err := cfg.Nudge(ctx, bead.Assignee, msg); err != nil {
			logger.Error("jira-webhook: failed to nudge agent on non-actionable transition",
				"bead", bead.ID, "agent", bead.Assignee, "jira_key", jiraKey, "error", err)
		} else {
			nudged = true
			logger.Info("jira-webhook: nudged agent to reconsider in-flight work on non-actionable Jira transition",
				"bead", bead.ID, "agent", bead.Assignee, "jira_key", jiraKey, "from", from, "to", to)
		}
	}
	if nudged {
		jiraStatusReconsiderTotal.WithLabelValues("nudged").Inc()
	} else {
		jiraStatusReconsiderTotal.WithLabelValues("flagged").Inc()
	}
}

// buildJiraStatusReconsiderNote builds the internal note posted to the task bead
// when its Jira ticket transitions to a non-actionable status. It records the
// transition and tells whoever picks the bead up to re-evaluate before any
// irreversible action.
func buildJiraStatusReconsiderNote(jiraKey, from, to string, attended bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ Jira ticket %s transitioned to non-actionable status **%s**", jiraKey, to)
	if strings.TrimSpace(from) != "" {
		fmt.Fprintf(&b, " (was %s)", from)
	}
	b.WriteString(".\n\nThis gardenio task may no longer be wanted. Re-evaluate before continuing — do NOT apply live infrastructure changes or open/merge MRs without re-confirming on the ticket that the work is still active. If the ticket is abandoned, wind down and close this bead.")
	if !attended {
		b.WriteString("\n\nNo agent was working this task, so it has been paused (status → blocked). Unblock it only if the ticket is reactivated.")
	}
	return b.String()
}

// buildJiraStatusReconsiderNudge builds the message delivered into a live
// agent's session when its ticket goes non-actionable mid-work.
func buildJiraStatusReconsiderNudge(jiraKey, beadID, from, to string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Jira ticket %s just moved to non-actionable status %q", jiraKey, to)
	if strings.TrimSpace(from) != "" {
		fmt.Fprintf(&b, " (was %q)", from)
	}
	b.WriteString(". The work you are doing may no longer be wanted.\n")
	b.WriteString("PAUSE and re-evaluate before continuing — especially before any irreversible action (live terraform apply, opening or merging an MR). ")
	fmt.Fprintf(&b, "Re-read the current ticket status with `gb jira show %s`. ", jiraKey)
	b.WriteString("If the ticket is abandoned, stop, wind down cleanly, and close this task.\n")
	fmt.Fprintf(&b, "Bead: %s", beadID)
	return b.String()
}
