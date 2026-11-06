package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

// escalationReviewURL renders the absolute, authenticated web-UI URL an operator
// clicks to review (approve/reject) an escalation. It is the SINGLE source of
// this URL shape (ka-0000005895): every escalation link the bridge emits — the
// creation card, the report button, the approved/rejected review buttons, and
// the operator DM (via b.escalationReviewURL wired into EscalationOperatorDM) —
// routes through here, so there is exactly one place to be right.
//
// Target: the bead deep link "$BEADS_WEB_URL/<id>", which resolves to the
// beads-viewer catch-all "GET /{id}" (handleBeadShow). That page renders the
// escalation card (type_escalation.html) WITH the inline approve/reject chips,
// so the click lands the operator directly on the control.
//
// It is deliberately NOT any of:
//   - "/escalations" — that list page is explicitly read-only ("approve/reject
//     lives in the bridge SPA"); it holds no control.
//   - "/ui/escalations.html#<id>" — the shape this replaces. That was a template
//     FILENAME served by the bridge's OWN /ui/ SPA, mistakenly fed the
//     beads-viewer base URL; it 404s on the viewer and has been dead since it
//     was introduced (epic ka-0000000887).
//   - any "#<id>" fragment — the viewer's escalation rows are keyed
//     "escalation-row-<id>", not "<id>", and the deep-link page needs no anchor.
//
// The regression guard (ka-0000001512) resolves this shape against the viewer's
// real *http.ServeMux so a filename can never again ship as a path.
//
// Returns "" when no absolute base URL is configured — Slack rejects relative
// URLs as invalid_blocks, and callers hide the button when this is empty.
func (b *Bot) escalationReviewURL(beadID string) string {
	if b == nil {
		return ""
	}
	return EscalationReviewURL(b.beadsWebURL, beadID)
}

// EscalationReviewURL is the pure, package-level form of escalationReviewURL. It
// is exported so the regression guard (ka-0000001512), which lives in the
// beads-viewer's package, can build the REAL emitted URL shape and resolve it
// against the viewer's real *http.ServeMux — closing the loop that no test ever
// closed before (a template filename shipped as a URL path and nothing caught
// it). See escalationReviewURL for the routing rationale.
//
// Returns "" when either input is empty — matching the callers' "hide the
// button" guard.
func EscalationReviewURL(beadsWebURL, beadID string) string {
	if beadsWebURL == "" || beadID == "" {
		return ""
	}
	return fmt.Sprintf("%s/%s", strings.TrimRight(beadsWebURL, "/"), beadID)
}

// slackBlockRenderError reports whether err is Slack refusing to render a
// message's blocks, and returns the human-readable detail Slack sent alongside
// the bare error code.
//
// This exists because slack-go's SlackErrorResponse.Error() returns ONLY
// `Err` — the bare code, e.g. "invalid_blocks" — while the actual diagnosis
// travels in `ResponseMetadata.Messages`, which typically names the offending
// block and field (e.g. "invalid_blocks_format: ... failed validation"). Every
// call site that formats the error with %w therefore logs a code with no way to
// tell WHICH block was bad, which is exactly why these failures went
// unexplained for so long (ka-0000000674, ka-0000006181).
func slackBlockRenderError(err error) (detail string, isBlockErr bool) {
	var resp slack.SlackErrorResponse
	if !errors.As(err, &resp) {
		return "", false
	}
	// Slack spells this family of rejection several ways (invalid_blocks,
	// invalid_blocks_format, block-specific variants); match on the prefix
	// rather than enumerating codes that Slack may extend.
	if !strings.HasPrefix(resp.Err, "invalid_blocks") {
		return "", false
	}
	detail = resp.Err
	if msgs := resp.ResponseMetadata.Messages; len(msgs) > 0 {
		detail += ": " + strings.Join(msgs, "; ")
	}
	return detail, true
}

// summarizeCredentials parses a credential_requirements JSON array and returns
// a compact Slack-friendly summary string like "AWS role, K8s context".
func summarizeCredentials(credsJSON string) string {
	var creds []struct {
		Type       string `json:"type"`
		Identifier string `json:"identifier"`
	}
	if err := json.Unmarshal([]byte(credsJSON), &creds); err != nil || len(creds) == 0 {
		return ""
	}
	var parts []string
	for _, c := range creds {
		label := c.Type
		if label == "" {
			label = c.Identifier
		}
		if label != "" {
			parts = append(parts, fmt.Sprintf("`%s`", label))
		}
	}
	return strings.Join(parts, ", ")
}

// EscalationNotifier sends Slack notifications for escalation bead lifecycle events.
// Escalation approval happens in the authenticated web UI; Slack is notification-only.
type EscalationNotifier interface {
	NotifyNewEscalation(ctx context.Context, bead BeadEvent) error
	UpdateEscalationApproved(ctx context.Context, bead BeadEvent) error
	NotifyEscalationResult(ctx context.Context, bead BeadEvent) error
	DismissEscalation(ctx context.Context, beadID string) error
	// NotifyEscalationPingback re-surfaces a stalled escalation in Slack —
	// as a broadcast reply on the original card when it is still tracked,
	// or a fresh message otherwise (ka-0000004385).
	NotifyEscalationPingback(ctx context.Context, bead BeadEvent) error
	// NotifyEscalationQueueAlert posts a stale-escalation alert to the named
	// operator queue channel once an escalation has been stalled past the
	// queue threshold (default 48h).
	NotifyEscalationQueueAlert(ctx context.Context, bead BeadEvent) error
	// NotifyEscalationHolderOffline tells the operator, in the card's thread,
	// that the presence lane holding the escalation is offline and keeps it
	// (gd-t41kYFbLUt). It is never a DM: the item is not the owner's.
	NotifyEscalationHolderOffline(ctx context.Context, bead BeadEvent) error
}

// escalationPriorityEmoji returns a colored circle emoji for escalation priority.
// Escalations are inherently high-priority, so P0–1 → red, P2 → orange, P3+ → yellow.
func escalationPriorityEmoji(priority int) string {
	switch {
	case priority <= 1:
		return ":red_circle:"
	case priority == 2:
		return ":large_orange_circle:"
	default:
		return ":large_yellow_circle:"
	}
}

// sensitivityEmoji returns an emoji indicator for the escalation sensitivity level.
func sensitivityEmoji(level string) string {
	switch level {
	case "public":
		return ":large_green_circle:"
	case "internal":
		return ":large_yellow_circle:"
	case "sensitive":
		return ":large_orange_circle:"
	case "secret":
		return ":red_circle:"
	default:
		return ""
	}
}

// grantEscalationSummary names what a grant escalation grants and to whom,
// from its operation_spec, or "" for any other escalation.
func grantEscalationSummary(opType, rawSpec string) string {
	if strings.TrimSpace(strings.ToLower(opType)) != "grant" {
		return ""
	}
	spec := parseOperationSpec(rawSpec)
	kind, principal := stringField(spec, "principal_kind"), stringField(spec, "principal")
	if kind == "" || principal == "" {
		return ""
	}
	var taints []string
	if names, ok := spec["taint_names"].([]any); ok {
		for _, n := range names {
			if s, ok := n.(string); ok && s != "" {
				taints = append(taints, s)
			}
		}
	}
	what := "taint"
	if len(taints) > 0 {
		what = strings.Join(taints, ", ")
	}
	code := func(v string) string {
		return escapeSlackMrkdwn(strings.ReplaceAll(truncateRunes(v, 200), "`", "'"))
	}
	return fmt.Sprintf("*Grants* `%s` *to* `%s:%s`", code(what), code(kind), code(principal))
}

// NotifyNewEscalation posts a Slack notification when a new escalation bead is
// created. The message contains a LINK to the authenticated web UI — not inline
// approve/deny buttons. Slack is the notification channel only; approval happens
// in the web UI.
func (b *Bot) NotifyNewEscalation(ctx context.Context, bead BeadEvent) error {
	opType := bead.Fields["operation_type"]
	// requesting_agent is the agent's bead id (gb escalation create), so it is
	// resolved to the agent's name: keyed by the id, the card lookups missed
	// the agent's card and ensureAgentCard posted a stray card for "gd-..."
	// with no card bead, whose thread the Commons could not find
	// (gd-rar4hYRbGC).
	agentName := b.escalationRequester(ctx, bead)
	agentDisplay := ""
	if agentName != "" {
		agentDisplay = b.agentDisplayName(agentName)
	}
	th := b.escalationTheme(ctx, bead)

	// Build the escalation URL pointing to the web UI.
	// Must be an absolute URL — Slack rejects relative URLs as invalid_blocks.
	escalationURL := b.escalationReviewURL(bead.ID)

	// Header with priority indicator.
	emoji := escalationPriorityEmoji(bead.Priority)
	headerText := fmt.Sprintf("%s %s *Escalation Request*", emoji, th.BeadType["escalation"])
	headerText += fmt.Sprintf("\n*%s*", b.beadTitleLink(bead.ID, bead.Title))

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", headerText, false, false),
			nil, nil,
		),
	}

	// A grant's principal is what the approver hands access to, so it leads
	// the card rather than sitting in the Web UI's spec (gd-URd8GBIXvL).
	if grant := grantEscalationSummary(opType, bead.Fields["operation_spec"]); grant != "" {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", grant, false, false),
			nil, nil,
		))
	}

	// Operation details.
	var detailParts []string
	if opType != "" {
		detailParts = append(detailParts, fmt.Sprintf("*Type:* `%s`", opType))
	}
	if execTarget := bead.Fields["execution_target"]; execTarget != "" {
		detailParts = append(detailParts, fmt.Sprintf("*Target:* `%s`", execTarget))
	}
	if sensitivity := bead.Fields["sensitivity"]; sensitivity != "" {
		sEmoji := sensitivityEmoji(sensitivity)
		detailParts = append(detailParts, fmt.Sprintf("*Sensitivity:* %s `%s`", sEmoji, sensitivity))
	}
	if where := escalationRunsWhere(ctx, b.daemon, bead.Fields); where != "" {
		detailParts = append(detailParts, fmt.Sprintf("*Where it will run:* %s", escapeSlackMrkdwn(truncateRunes(where, 160))))
	}
	if len(detailParts) > 0 {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", strings.Join(detailParts, " | "), false, false),
			nil, nil,
		))
	}

	// Credential requirements summary (V2) — show what access the executor needs.
	if creds := bead.Fields["credential_requirements"]; creds != "" {
		credSummary := summarizeCredentials(creds)
		if credSummary != "" {
			blocks = append(blocks, slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("%s *Credentials needed:* %s", th.Semantic(SemInfo), credSummary), false, false),
				nil, nil,
			))
		}
	}

	// Context field — human-readable background, its code made Slack-safe
	// (slackContextMrkdwn). Script content is NOT shown in Slack.
	if escCtx := bead.Fields["context"]; escCtx != "" {
		// Rune-aware truncation: a byte slice can split a multi-byte rune, emitting
		// invalid UTF-8 that Slack rejects with invalid_blocks — dropping the whole
		// escalation notification (ka-0000000920, same bug family as ka-0000006181).
		// Slack section text caps at 3000 chars, so a rune-based 2900 is the correct
		// reading of the limit anyway.
		escCtx = fitSlackContext(slackContextMrkdwn(escCtx), 2900, "...")
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", escCtx, false, false),
			nil, nil,
		))
	}

	// Note if execution_script is present but not shown in Slack.
	if bead.Fields["execution_script"] != "" {
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", th.Semantic(SemNote)+" _Execution script attached — review in Web UI_", false, false),
		))
	}

	// Context block with agent info.
	var ctxParts []string
	ctxParts = append(ctxParts, fmt.Sprintf("_%s_", b.beadTitleLink(bead.ID, bead.Title)))
	if agentDisplay != "" {
		ctxParts = append(ctxParts, fmt.Sprintf("Requested by: %s", agentDisplay))
	}
	blocks = append(blocks, slack.NewContextBlock("",
		slack.NewTextBlockObject("mrkdwn", strings.Join(ctxParts, " | "), false, false),
	))

	// Action button — link to web UI for authenticated approval.
	// Only add when we have an absolute URL; Slack rejects relative URLs.
	// A Codex re-login escalation also carries the Start device login button
	// (ADR 0007); it works without a web URL.
	loginBtn := codexDeviceLoginButton(bead)
	if escalationURL != "" || loginBtn != nil {
		blocks = append(blocks, slack.NewDividerBlock())
		var btns []slack.BlockElement
		if loginBtn != nil {
			loginBtn.Style = slack.StylePrimary
			btns = append(btns, loginBtn)
		}
		if escalationURL != "" {
			approveBtn := slack.NewButtonBlockElement("escalation_open_ui", bead.ID,
				slack.NewTextBlockObject("plain_text", "Review in Web UI", false, false))
			approveBtn.URL = escalationURL
			if loginBtn == nil {
				approveBtn.Style = slack.StylePrimary
			}
			btns = append(btns, approveBtn)
		}
		blocks = append(blocks, slack.NewActionBlock("", btns...))
	}

	fallbackText := fmt.Sprintf("Escalation request: %s (%s)", bead.Title, opType)

	// Resolve target channel — use requesting agent's thread if available.
	targetChannel := b.resolveCardChannel(ctx, agentName, "escalation")
	var threadTS string

	if agentName != "" {
		if slackChannel, slackTS := b.resolveAgentThread(ctx, agentName); slackChannel != "" && slackTS != "" {
			targetChannel = slackChannel
			threadTS = slackTS
		}
	}

	if threadTS == "" && b.agentThreadingEnabled() && agentName != "" {
		// Skip card creation for thread-bound agents to prevent stray top-level cards.
		b.mu.Lock()
		threadBound := b.isThreadBound(agentName)
		b.mu.Unlock()
		if !threadBound {
			if cardTS, err := b.ensureAgentCard(ctx, agentName, targetChannel); err == nil {
				threadTS = cardTS
			}
		}
	}

	// The card goes through the conversation Router (escalation_cards.go), which
	// also writes its card bead for the Commons. DEGRADE, NEVER DROP
	// (ka-0000000674): a block-rendering rejection used to lose the escalation
	// outright — measured over 7 days, 29 escalations were never notified, 13
	// of them still open — so postEscalationCard retries a refused card as its
	// plain-text fallback rather than returning.
	// The run and short title of the bead the escalation asks for, when it
	// bears arms (heraldry_cards.go), on the header line.
	blocks = b.withSubjectRun(blocks, b.cardSubjectBead(ctx, bead.Fields["requesting_task"]))
	posted, err := b.postEscalationCard(ctx, bead, agentName, targetChannel, threadTS, fallbackText, blocks)
	if err != nil {
		return fmt.Errorf("post escalation notification to Slack: %w", err)
	}
	channelID, ts := posted.ChannelID, posted.Timestamp

	// Track the message for later updates (approval, completion).
	ref := MessageRef{ChannelID: channelID, Timestamp: ts, Agent: agentName, ThreadTS: threadTS}
	b.mu.Lock()
	b.messages[bead.ID] = ref
	b.mu.Unlock()

	if b.state != nil {
		_ = b.state.SetDecisionMessage(bead.ID, ref)
	}

	// Stamp operator-visibility metadata onto the escalation bead so
	// bearer-token agents can read "was the operator notified, where, and how
	// long ago" via `kd show` / `gb escalation show` (ka-0000003667). The Slack
	// ref is otherwise trapped in b.messages (in-memory) + StateManager (behind
	// the OIDC web API), neither reachable by an agent. Best-effort, log-and-
	// continue: the notification already succeeded, so a stamp failure must
	// never gate it. notified_at means "the bridge posted the card", NOT "a
	// human read it". For sensitive/secret escalations, stamp only the
	// timestamp — the raw channel/ts would leak the operator channel to any
	// bearer-token agent that can read the bead.
	if ts != "" {
		visFields := map[string]string{
			"notified_at": time.Now().UTC().Format(time.RFC3339),
		}
		if sens := bead.Fields["sensitivity"]; sens != "sensitive" && sens != "secret" {
			visFields["slack_channel"] = channelID
			visFields["slack_ts"] = ts
			if threadTS != "" {
				visFields["slack_thread_ts"] = threadTS
			}
		}
		if err := b.daemon.UpdateBeadFields(ctx, bead.ID, visFields); err != nil {
			b.logger.Warn("failed to stamp operator-visibility fields on escalation",
				"bead", bead.ID, "error", err)
		}
	}

	b.logger.Info("posted escalation notification to Slack",
		"bead", bead.ID, "channel", channelID, "ts", ts, "operation_type", opType)
	return nil
}

// UpdateEscalationApproved updates the Slack message to show the escalation was
// approved. Replaces the "Review in Web UI" button with a green checkmark and
// approval details.
func (b *Bot) UpdateEscalationApproved(ctx context.Context, bead BeadEvent) error {
	ref, ok := b.lookupMessage(bead.ID)
	if !ok {
		b.logger.Debug("no Slack message found for approved escalation", "bead", bead.ID)
		return nil
	}

	// Prefer the human-readable email over the user bead ID for display.
	approverDisplay := bead.Fields["approved_by_email"]
	if approverDisplay == "" {
		approverDisplay = bead.Fields["approved_by"]
	}
	approvalMethod := bead.Fields["approval_method"]

	th := b.escalationTheme(ctx, bead)
	text := fmt.Sprintf("%s *Escalation Approved*\n*%s*", th.Semantic(SemOK), b.beadTitleLink(bead.ID, bead.Title))
	if line := escalationApprovalLine(approverDisplay, approvalMethod, bead.Fields["policy_rule"]); line != "" {
		text += "\n" + line
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", text, false, false),
			nil, nil,
		),
	}

	_, _, _, err := b.api.UpdateMessageContext(ctx, ref.ChannelID, ref.Timestamp,
		slack.MsgOptionText(fmt.Sprintf("Escalation approved: %s", bead.Title), false),
		slack.MsgOptionBlocks(blocks...),
	)
	if err != nil {
		b.logger.Error("failed to update escalation Slack message", "bead", bead.ID, "error", err)
		return fmt.Errorf("update escalation message: %w", err)
	}

	b.logger.Info("updated escalation notification (approved)",
		"bead", bead.ID, "approved_by", approverDisplay)
	return nil
}

// NotifyEscalationResult posts a Slack notification when an escalation execution
// completes (success, failure, or partial). If the original notification message
// exists, updates it; otherwise posts a new message.
// For sensitive/secret results, only the redacted_summary is shown in Slack
// with a "View Full Report" button linking to the authenticated web UI.
func (b *Bot) NotifyEscalationResult(ctx context.Context, bead BeadEvent) error {
	resultStatus := bead.Fields["result_status"]
	executor := bead.Fields["executor"]
	sensitivity := bead.Fields["sensitivity"]

	th := b.escalationTheme(ctx, bead)
	var emoji string
	switch resultStatus {
	case "success":
		emoji = th.Semantic(SemOK)
	case "failure":
		emoji = th.Semantic(SemFail)
	case "partial":
		emoji = th.Semantic(SemWarn)
	default:
		emoji = th.Semantic(SemInfo)
	}

	text := fmt.Sprintf("%s *%s*\n*%s*", emoji, escalationOutcomeLabel(resultStatus), b.beadTitleLink(bead.ID, bead.Title))

	var detailParts []string
	if executor != "" {
		detailParts = append(detailParts, "Run by "+b.agentDisplayName(executor))
	}
	if sensitivity != "" {
		detailParts = append(detailParts, fmt.Sprintf("Sensitivity: %s %s", sensitivityEmoji(sensitivity), sensitivity))
	}
	if len(detailParts) > 0 {
		text += "\n" + strings.Join(detailParts, " | ")
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", text, false, false),
			nil, nil,
		),
	}

	// What the run produced, for an escalation filed as public or internal.
	// Unlabelled ones show nothing: a result is free text from the executor,
	// and only the filer's label says it is fit for the channel.
	if excerpt := escalationResultExcerpt(bead.Fields); excerpt != "" {
		blocks = append(blocks, slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", excerpt, false, false),
			nil, nil,
		))
	}

	// For sensitive/secret results, show only redacted_summary and a web UI link.
	if sensitivity == "sensitive" || sensitivity == "secret" {
		redacted := bead.Fields["redacted_summary"]
		if redacted != "" {
			blocks = append(blocks, slack.NewSectionBlock(
				slack.NewTextBlockObject("mrkdwn",
					fmt.Sprintf("%s *Summary:* %s", th.Semantic(SemNote), redacted), false, false),
				nil, nil,
			))
		}
		blocks = append(blocks, slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn",
				fmt.Sprintf("_Full results are %s-level — view via authenticated Web UI only_", sensitivity), false, false),
		))

		if reportURL := b.escalationReviewURL(bead.ID); reportURL != "" {
			viewBtn := slack.NewButtonBlockElement("escalation_view_report", bead.ID,
				slack.NewTextBlockObject("plain_text", "View Full Report", false, false))
			viewBtn.URL = reportURL
			viewBtn.Style = slack.StylePrimary
			blocks = append(blocks, slack.NewActionBlock("", viewBtn))
		}
	}

	ref, ok := b.lookupMessage(bead.ID)
	if ok {
		// Update existing message.
		_, _, _, err := b.api.UpdateMessageContext(ctx, ref.ChannelID, ref.Timestamp,
			slack.MsgOptionText(escalationResultFallback(resultStatus, bead.Title), false),
			slack.MsgOptionBlocks(blocks...),
		)
		if err != nil {
			b.logger.Warn("failed to update escalation result, posting new message",
				"bead", bead.ID, "error", err)
			return b.postEscalationResultFallback(ctx, ref, bead, blocks)
		}

		b.logger.Info("updated escalation notification (result)",
			"bead", bead.ID, "result_status", resultStatus)
		return nil
	}

	// No tracked message — post to the requesting agent's channel.
	b.logger.Debug("no tracked escalation message, posting result as new message", "bead", bead.ID)
	agentName := b.escalationRequester(ctx, bead)
	targetChannel := b.resolveCardChannel(ctx, agentName, "escalation result")

	msgOpts := []slack.MsgOption{
		slack.MsgOptionText(escalationResultFallback(resultStatus, bead.Title), false),
		slack.MsgOptionBlocks(blocks...),
	}
	// Into the requester's thread, where its request card went, so the
	// result lands beside the work it unblocks.
	if agentName != "" {
		if ch, ts := b.resolveAgentThread(ctx, agentName); ch != "" && ts != "" {
			targetChannel = ch
			msgOpts = append(msgOpts, slack.MsgOptionTS(ts))
		}
	}

	_, _, err := b.api.PostMessageContext(ctx, targetChannel, msgOpts...)
	if err != nil {
		return fmt.Errorf("post escalation result to Slack: %w", err)
	}

	b.logger.Info("posted escalation result to Slack",
		"bead", bead.ID, "channel", targetChannel, "result_status", resultStatus)
	return nil
}

// escalationOutcomeLabel names an escalation's result_status in words for the
// result card's header.
func escalationOutcomeLabel(status string) string {
	switch status {
	case "success":
		return "Done"
	case "partial":
		return "Partly done"
	case "failure":
		return "Failed"
	default:
		return "Finished"
	}
}

// escalationResultFallback is the result card's notification text. It leads
// with the outcome in words, not the raw result_status, which read "Escalation
// success: Escalation: create on type agent denied" for a denied spawn
// (gd-Sv85FM7JFd).
func escalationResultFallback(status, title string) string {
	return fmt.Sprintf("%s: %s", escalationOutcomeLabel(status), title)
}

// escalationApprovalLine says who approved an escalation and how, in words:
// "Approved by alex@x.example in the web UI", "Approved automatically by
// policy floor:x". approval_method is the daemon's enum (init.go); an unknown
// value adds nothing rather than leaking the raw word.
func escalationApprovalLine(approver, method, policyRule string) string {
	how := ""
	switch method {
	case "web_ui":
		how = "in the web UI"
	case "api_token":
		how = "with an API token"
	case "cli":
		how = "from the command line"
	case "commons":
		how = "in the Commons"
	case "coordinator":
		how = "by the coordinator"
	case "policy":
		if policyRule != "" {
			return fmt.Sprintf("Approved automatically by policy `%s`", policyRule)
		}
		return "Approved automatically by policy"
	}
	switch {
	case approver != "" && how != "":
		return fmt.Sprintf("Approved by %s %s", approver, how)
	case approver != "":
		return "Approved by " + approver
	case how != "":
		return "Approved " + how
	}
	return ""
}

// escalationResultExcerptRunes caps the result excerpt on the result card.
const escalationResultExcerptRunes = 300

// escalationResultExcerpt is a short quote of what an escalation's run
// produced, for its result card. The output otherwise reaches only the
// requesting agent (gd-Sv85FM7JFd). The redacted summary wins when there is
// one; the raw result is quoted only for an escalation filed as public or
// internal. Sensitive and secret results keep their own summary block.
func escalationResultExcerpt(fields map[string]string) string {
	sensitivity := fields["sensitivity"]
	if sensitivity != "public" && sensitivity != "internal" {
		return ""
	}
	text := strings.TrimSpace(fields["redacted_summary"])
	if text == "" {
		text = strings.TrimSpace(fields["result"])
	}
	if text == "" {
		return ""
	}
	text = truncateRunes(strings.Join(strings.Fields(text), " "), escalationResultExcerptRunes)
	return "> " + escapeSlackMrkdwn(text)
}

// postEscalationResultFallback posts the result as a new thread reply when the
// original message cannot be updated.
func (b *Bot) postEscalationResultFallback(ctx context.Context, ref MessageRef, bead BeadEvent, blocks []slack.Block) error {
	msgOpts := []slack.MsgOption{
		slack.MsgOptionText(escalationResultFallback(bead.Fields["result_status"], bead.Title), false),
		slack.MsgOptionBlocks(blocks...),
	}
	threadTS := ref.ThreadTS
	if threadTS == "" {
		threadTS = ref.Timestamp
	}
	if threadTS != "" {
		msgOpts = append(msgOpts, slack.MsgOptionTS(threadTS))
	}
	_, _, err := b.api.PostMessageContext(ctx, ref.ChannelID, msgOpts...)
	if err != nil {
		return fmt.Errorf("post escalation result fallback: %w", err)
	}
	return nil
}

// escalationStallAge returns a human-readable stall age for an escalation,
// anchored on notified_at (when the bridge posted the original card). Returns
// "" when the field is absent (pre-stamping escalations) or unparseable —
// BeadEvent does not carry created_at, so there is no better fallback here.
func escalationStallAge(fields map[string]string) string {
	if v := fields["notified_at"]; v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return formatAge(t)
		}
	}
	return ""
}

// NotifyEscalationPingback re-surfaces a stalled escalation in Slack
// (ka-0000004385). When the original escalation card is still tracked, the
// pingback posts as a broadcast thread reply (reply_broadcast) so it re-appears
// in the channel without duplicating the full card. When no message ref exists
// (bridge restarted and state lost), it posts a fresh compact card to the
// requesting agent's channel.
func (b *Bot) NotifyEscalationPingback(ctx context.Context, bead BeadEvent) error {
	count := bead.Fields["pingback_count"]
	source := bead.Fields["pingback_source"]
	reason := bead.Fields["pingback_reason"]

	age := escalationStallAge(bead.Fields)

	th := b.escalationTheme(ctx, bead)
	text := fmt.Sprintf("%s *Escalation Pingback* — still awaiting operator action\n*%s*",
		th.Semantic(SemWarn), b.beadTitleLink(bead.ID, bead.Title))

	var detailParts []string
	detailParts = append(detailParts, fmt.Sprintf("Priority: `P%d`", bead.Priority))
	if age != "" {
		detailParts = append(detailParts, fmt.Sprintf("Posted: %s", age))
	}
	if count != "" {
		detailParts = append(detailParts, fmt.Sprintf("Pingback: #%s", count))
	}
	if source != "" {
		detailParts = append(detailParts, fmt.Sprintf("Source: `%s`", source))
	}
	text += "\n" + strings.Join(detailParts, " | ")
	if reason != "" {
		text += fmt.Sprintf("\n_%s_", reason)
	}

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", text, false, false),
			nil, nil,
		),
	}

	// Review button — same web UI target as the original card.
	if escalationURL := b.escalationReviewURL(bead.ID); escalationURL != "" {
		reviewBtn := slack.NewButtonBlockElement("escalation_open_ui", bead.ID,
			slack.NewTextBlockObject("plain_text", "Review in Web UI", false, false))
		reviewBtn.URL = escalationURL
		reviewBtn.Style = slack.StylePrimary
		blocks = append(blocks, slack.NewActionBlock("", reviewBtn))
	}

	fallbackText := fmt.Sprintf("Escalation pingback: %s", bead.Title)
	msgOpts := []slack.MsgOption{
		slack.MsgOptionText(fallbackText, false),
		slack.MsgOptionBlocks(blocks...),
	}

	if ref, ok := b.lookupMessage(bead.ID); ok {
		// Reply in the original card's thread and broadcast to the channel so
		// the pingback re-surfaces without a duplicate card.
		threadTS := ref.Timestamp
		if ref.ThreadTS != "" {
			threadTS = ref.ThreadTS
		}
		msgOpts = append(msgOpts, slack.MsgOptionTS(threadTS), slack.MsgOptionBroadcast())
		if _, _, err := b.api.PostMessageContext(ctx, ref.ChannelID, msgOpts...); err != nil {
			return fmt.Errorf("post escalation pingback to Slack: %w", err)
		}
		b.logger.Info("posted escalation pingback (thread broadcast)",
			"bead", bead.ID, "channel", ref.ChannelID, "count", count, "source", source)
		return nil
	}

	// No tracked message — post fresh to the requesting agent's channel.
	agentName := b.escalationRequester(ctx, bead)
	targetChannel := b.resolveCardChannel(ctx, agentName, "escalation pingback")
	channelID, ts, err := b.api.PostMessageContext(ctx, targetChannel, msgOpts...)
	if err != nil {
		return fmt.Errorf("post escalation pingback to Slack: %w", err)
	}

	// Track the fresh message so subsequent pingbacks thread onto it.
	ref := MessageRef{ChannelID: channelID, Timestamp: ts, Agent: agentName}
	b.mu.Lock()
	b.messages[bead.ID] = ref
	b.mu.Unlock()
	if b.state != nil {
		_ = b.state.SetDecisionMessage(bead.ID, ref)
	}

	b.logger.Info("posted escalation pingback (fresh card)",
		"bead", bead.ID, "channel", channelID, "count", count, "source", source)
	return nil
}

// NotifyEscalationHolderOffline replies in the escalation card's thread (no
// broadcast) that its holder is offline, or posts to the operator queue
// channel when the card is not tracked.
func (b *Bot) NotifyEscalationHolderOffline(ctx context.Context, bead BeadEvent) error {
	lane := beadsapi.LaneLabel(bead.Fields["held_by"])
	since := "it has not been seen yet"
	if seen := strings.TrimSpace(bead.Fields["held_by_last_seen_at"]); seen != "" {
		if at, err := time.Parse(time.RFC3339, seen); err == nil {
			since = "last seen " + at.UTC().Format("Jan 2 15:04Z")
		}
	}
	text := fmt.Sprintf("%s *%s* is offline (%s) and holds this escalation until it is back; it is not the owner's to act on.\n*%s*",
		b.escalationTheme(ctx, bead).Semantic(SemWarn), lane, since, b.beadTitleLink(bead.ID, bead.Title))
	opts := []slack.MsgOption{slack.MsgOptionText(text, false)}
	if ref, ok := b.lookupMessage(bead.ID); ok {
		threadTS := ref.Timestamp
		if ref.ThreadTS != "" {
			threadTS = ref.ThreadTS
		}
		_, _, err := b.api.PostMessageContext(ctx, ref.ChannelID, append(opts, slack.MsgOptionTS(threadTS))...)
		return err
	}
	target := b.escalationQueueChannel
	if target == "" {
		target = b.channel
	}
	if target == "" {
		return nil
	}
	_, _, err := b.api.PostMessageContext(ctx, target, opts...)
	return err
}

// NotifyEscalationQueueAlert posts a stale-escalation alert to the operator
// queue channel (SLACK_ESCALATION_QUEUE_CHANNEL) once an escalation has been
// stalled past the queue threshold. Falls back to the default channel when no
// queue channel is configured, so a genuinely-stuck P0 never goes silent.
func (b *Bot) NotifyEscalationQueueAlert(ctx context.Context, bead BeadEvent) error {
	targetChannel := b.escalationQueueChannel
	if targetChannel == "" {
		targetChannel = b.channel
	}
	if targetChannel == "" {
		b.logger.Warn("no queue or default channel for escalation queue alert", "bead", bead.ID)
		return nil
	}

	age := escalationStallAge(bead.Fields)

	text := b.themeFor(ctx, targetChannel).BeadType["escalation"] + " *Stale Escalation — operator action needed*"
	if b.escalationQueueMention != "" {
		text += fmt.Sprintf(" <%s>", b.escalationQueueMention)
	}
	text += fmt.Sprintf("\n*%s*", b.beadTitleLink(bead.ID, bead.Title))

	var detailParts []string
	detailParts = append(detailParts, fmt.Sprintf("Priority: `P%d`", bead.Priority))
	if age != "" {
		detailParts = append(detailParts, fmt.Sprintf("Posted: %s", age))
	}
	if opType := bead.Fields["operation_type"]; opType != "" {
		detailParts = append(detailParts, fmt.Sprintf("Type: `%s`", opType))
	}
	if task := bead.Fields["requesting_task"]; task != "" {
		detailParts = append(detailParts, fmt.Sprintf("Blocks: %s", b.beadLink(task, fmt.Sprintf("`%s`", task))))
	}
	text += "\n" + strings.Join(detailParts, " | ")
	text += "\n_This escalation has been awaiting operator action past the stall threshold._"

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", text, false, false),
			nil, nil,
		),
	}
	if escalationURL := b.escalationReviewURL(bead.ID); escalationURL != "" {
		reviewBtn := slack.NewButtonBlockElement("escalation_open_ui", bead.ID,
			slack.NewTextBlockObject("plain_text", "Review in Web UI", false, false))
		reviewBtn.URL = escalationURL
		reviewBtn.Style = slack.StyleDanger
		blocks = append(blocks, slack.NewActionBlock("", reviewBtn))
	}

	fallbackText := fmt.Sprintf("Stale escalation: %s", bead.Title)
	_, _, err := b.api.PostMessageContext(ctx, targetChannel,
		slack.MsgOptionText(fallbackText, false),
		slack.MsgOptionBlocks(blocks...),
	)
	if err != nil {
		return fmt.Errorf("post escalation queue alert to Slack: %w", err)
	}

	b.logger.Info("posted escalation queue alert",
		"bead", bead.ID, "channel", targetChannel)
	return nil
}

// PostStaleEscalationDigest posts ONE consolidated message to the operator queue
// channel (SLACK_ESCALATION_QUEUE_CHANNEL) listing every stale open P0/P1
// escalation the digest loop found due this sweep (ka-0000006001). Unlike the
// per-item pingback/queue-alert, this is a single scannable snapshot the
// operator can triage at a glance. Falls back to the default channel so a
// genuinely-stuck backlog never goes silent, and is a no-op with no entries.
func (b *Bot) PostStaleEscalationDigest(ctx context.Context, entries []StaleEscalationDigestEntry, overflow int) error {
	if len(entries) == 0 {
		return nil
	}
	targetChannel := b.escalationQueueChannel
	if targetChannel == "" {
		targetChannel = b.channel
	}
	if targetChannel == "" {
		b.logger.Warn("no queue or default channel for escalation staleness digest")
		return nil
	}

	header := fmt.Sprintf("%s *Stale Escalations Digest* — %d awaiting operator action",
		b.themeFor(ctx, targetChannel).BeadType["escalation"], len(entries))
	if b.escalationQueueMention != "" {
		header += fmt.Sprintf(" <%s>", b.escalationQueueMention)
	}

	var lines []string
	for _, e := range entries {
		line := fmt.Sprintf("• `P%d` %s — stalled %s",
			e.Priority, b.beadTitleLink(e.ID, e.Title), formatStallDuration(e.Age))
		if e.OperationType != "" {
			line += fmt.Sprintf(" · `%s`", e.OperationType)
		}
		if e.RequestingTask != "" {
			line += fmt.Sprintf(" · blocks %s", b.beadLink(e.RequestingTask, fmt.Sprintf("`%s`", e.RequestingTask)))
		}
		lines = append(lines, line)
	}

	body := header + "\n" + strings.Join(lines, "\n")
	if overflow > 0 {
		body += fmt.Sprintf("\n_…and %d more not shown — see the escalations queue._", overflow)
	}
	body += "\n_Approval happens in the web UI, not in Slack._"

	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", body, false, false),
			nil, nil,
		),
	}

	fallbackText := fmt.Sprintf("Stale escalations digest: %d awaiting operator action", len(entries))
	_, _, err := b.api.PostMessageContext(ctx, targetChannel,
		slack.MsgOptionText(fallbackText, false),
		slack.MsgOptionBlocks(blocks...),
	)
	if err != nil {
		return fmt.Errorf("post escalation staleness digest to Slack: %w", err)
	}

	b.logger.Info("posted escalation staleness digest",
		"channel", targetChannel, "entries", len(entries), "overflow", overflow)
	return nil
}

// Compile-time assertion: *Bot satisfies the digest loop's notifier.
var _ StaleDigestNotifier = (*Bot)(nil)

// DismissEscalation removes the Slack message for a rejected/expired escalation.
func (b *Bot) DismissEscalation(ctx context.Context, beadID string) error {
	ref, ok := b.lookupMessage(beadID)
	if !ok {
		b.logger.Debug("no Slack message found for dismissed escalation", "bead", beadID)
		return nil
	}

	resultStatus := "rejected"
	text := fmt.Sprintf("%s *Escalation Rejected*\n%s", b.themeFor(ctx, ref.ChannelID).Semantic(SemDenied), b.beadLink(beadID, fmt.Sprintf("`%s`", beadID)))
	blocks := []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", text, false, false),
			nil, nil,
		),
	}

	_, _, _, err := b.api.UpdateMessageContext(ctx, ref.ChannelID, ref.Timestamp,
		slack.MsgOptionText(fmt.Sprintf("Escalation %s: %s", resultStatus, beadID), false),
		slack.MsgOptionBlocks(blocks...),
	)
	if err != nil {
		b.logger.Warn("failed to update dismissed escalation, deleting instead",
			"bead", beadID, "error", err)
		_, _, _ = b.api.DeleteMessageContext(ctx, ref.ChannelID, ref.Timestamp)
	}

	b.mu.Lock()
	delete(b.messages, beadID)
	b.mu.Unlock()

	if b.state != nil {
		_ = b.state.RemoveDecisionMessage(beadID)
	}

	b.logger.Info("dismissed escalation from Slack", "bead", beadID)
	return nil
}

// Compile-time assertion: ensures Bot satisfies EscalationNotifier at build time.
var _ EscalationNotifier = (*Bot)(nil)
