// escalation_operator_dm.go DMs the addressed human operator the moment an
// escalation bead is created for them (operator ask, Matt Baker 2026-08-26:
// "time to start sending DMs to users when an escalation is created that's
// operated by them or whatever — very useful").
//
// WHY THIS IS NOT A DUPLICATE OF THE EXISTING CARD:
//
// NotifyNewEscalation (bot_escalations.go) already posts a rich escalation card,
// but it routes by the REQUESTING AGENT — the agent's bound Slack thread, its
// agent card, or the project channel (resolveChannel, bot_agents.go). The
// addressed operator is never consulted, so an escalation stamped
// operated_by=oidc:someone@example.com lands in whatever channel the requesting
// agent happens to live in, and reaches that human only if they were already
// watching it. Measured 2026-08-26: 171 escalations created in 7 days, and of
// the open ones 130 are addressed to a single operator while 6 are addressed to
// five other humans across the org — exactly the population that a
// channel-routed card serves worst.
//
// This adds the missing DELIVERY, not a second copy of the public record. Same
// reasoning as bot_cross_operator_notify.go and the standing rule against
// re-posting what the bridge already posts (advice ka-0000000409): the card
// stays the canonical, updatable artifact; the DM is a personal ping that
// carries the link back to it.
//
// SENSITIVITY. A DM is strictly more private than the channel post that already
// happens, so sensitive/secret escalations are still delivered — but their
// context body is withheld from the DM text, and the DM channel id is not
// stamped back onto the bead, mirroring the carve-out NotifyNewEscalation
// applies to slack_channel/slack_ts (a bearer-token agent can read bead fields).
package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// EscalationDMSender delivers the DM. *Bot satisfies it via SendDM.
type EscalationDMSender interface {
	SendDM(ctx context.Context, userID, text string) (channelID, ts string, err error)
}

// escalationDMContextLimit bounds how much of the escalation's context body is
// quoted into the DM. The full text lives on the bead and in the channel card;
// the DM only needs enough for the operator to know whether to open it now.
const escalationDMContextLimit = 600

// Every truncation in this file is RUNE-aware (truncateRunes, not the byte-based
// truncateText). A byte slice can split a multi-byte rune and emit invalid
// UTF-8, which Slack rejects — dropping the whole message. That is not
// hypothetical: it is the bug family that silently lost escalation
// notifications in the channel card (ka-0000000920, ka-0000006181), which is
// why NotifyNewEscalation truncates its context body by runes. Escalation
// context bodies in this repo routinely contain em-dashes and arrows, and a
// sweep over one realistic body found 160 of 1351 candidate cut lengths
// produced invalid UTF-8 under byte truncation.

// EscalationOperatorDMConfig configures the notifier.
type EscalationOperatorDMConfig struct {
	Daemon BeadClient
	Sender EscalationDMSender
	Logger *slog.Logger
	// Users resolves a bare provider handle in the CREATOR position back to a
	// human, so an operator who filed an escalation addressed to themselves is
	// not DMed about their own request. Optional.
	Users *UserDirectory
	// LinkedSlackIDs returns the Slack user ids with a confirmed account link
	// to the given email (SlackLinker.SlackIDsForEmail). Optional.
	LinkedSlackIDs func(email string) []string
	// LinkBead renders a Slack link for a bead id + title. Optional.
	LinkBead func(id, title string) string
	// ReviewURL renders the authenticated web-UI URL for an escalation id.
	// Optional; without it the DM carries only the bead link.
	ReviewURL func(id string) string
	// RecordDM records a sent DM (its Slack channel, ts and text) to the
	// operator at email as a card of the escalation, so the Commons shows the
	// copy in their DM channel with the requesting agent
	// (Bot.recordEscalationDM, escalation_cards.go). email is "" when the
	// operator resolved to a Slack id alone. Optional.
	RecordDM func(ctx context.Context, bead BeadEvent, email, channelID, ts, text string)
}

// EscalationOperatorDM posts one DM to the addressed operator when an escalation
// is created for them.
type EscalationOperatorDM struct {
	daemon    BeadClient
	sender    EscalationDMSender
	logger    *slog.Logger
	link      func(id, title string) string
	reviewURL func(id string) string
	recordDM  func(ctx context.Context, bead BeadEvent, email, channelID, ts, text string)
	users     *UserDirectory
	index     *operatorSlackIndex
	linked    func(email string) []string

	// seen guarantees at most ONE DM per escalation for the dedup window, so an
	// SSE reconnect that redelivers beads.bead.created cannot re-ping a human.
	seen *seenSet
}

// NewEscalationOperatorDM builds the notifier. A nil Sender or Daemon disables
// it, so a slack-bridge running without a Slack app is unaffected.
func NewEscalationOperatorDM(cfg EscalationOperatorDMConfig) *EscalationOperatorDM {
	if cfg.Daemon == nil || cfg.Sender == nil {
		return nil
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &EscalationOperatorDM{
		daemon:    cfg.Daemon,
		sender:    cfg.Sender,
		logger:    logger,
		link:      cfg.LinkBead,
		reviewURL: cfg.ReviewURL,
		recordDM:  cfg.RecordDM,
		users:     cfg.Users,
		linked:    cfg.LinkedSlackIDs,
		index:     newOperatorSlackIndex(cfg.Daemon, logger),
		seen:      newSeenSet(seenTTL, seenMaxEntries),
	}
}

// Run sweeps the dedup set, mirroring the other bridge watchers.
func (e *EscalationOperatorDM) Run(ctx context.Context) {
	if e == nil {
		return
	}
	ticker := time.NewTicker(seenGCInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.seen.gc()
		}
	}
}

// EscalationCreated is called from Escalations.handleCreated after the channel
// card has been posted. Safe on a nil receiver.
func (e *EscalationOperatorDM) EscalationCreated(ctx context.Context, bead BeadEvent) {
	if e == nil || bead.ID == "" {
		return
	}

	// An escalation created already-approved is not an ask — it is a record of
	// a call somebody already made, and the approval path has its own notifier.
	if strings.TrimSpace(bead.Fields["approved_by"]) != "" {
		return
	}

	// The addressed operator lives on the bead ROW (operated_by /
	// operated_by_id / operated_by_email), not in fields and not on the SSE
	// payload — sseBeadData drops it. One GetBead is therefore required.
	detail, err := e.daemon.GetBead(ctx, bead.ID)
	if err != nil {
		e.logger.Warn("escalation operator DM: cannot read bead",
			"bead", bead.ID, "error", err)
		return
	}
	// The create event is a snapshot from before any same-request auto-approve
	// (spawn policy, auto-replay); the fresh read says whether it is still an ask.
	if escalationSettled(detail) {
		return
	}
	// Only an owner-audience escalation pings the owner personally
	// (gd-bmOiteQi36); desk and operator ones are in hand, and their channel
	// card is the whole delivery. Read off the fresh bead, so a re-route or the
	// forced-owner rule (credentials, secret) is honoured.
	if !beadsapi.EscalationForOwner(escalationAudienceFields(detail.Fields, bead.Fields)) {
		return
	}
	// #756's gate (above) said the owner's; held_by says which owner (D9). A
	// land's owners are reached in the Commons, not by a fleet DM.
	operatedBy, ok := escalationDMRecipient(detail)
	if !ok {
		e.logger.Info("escalation operator DM: held by a land's owners, not sent",
			"bead", bead.ID, "held_by", beadsapi.EscalationHeldBy(detail))
		return
	}
	if operatedBy == "" || !IsHumanOperator(operatedBy) {
		// No addressed human — e.g. escalations stamped fleet:bridge:jira.
		// Nobody to DM; the channel card is the whole delivery.
		return
	}
	e.deliver(ctx, bead, detail, operatedBy, bead.ID, "", true)
}

// EscalationHandedUp is called from Escalations.handleUpdated when an
// unapproved escalation carries handed_up_at (escalation routing D5: a
// coordinator or steward gave it to a human). One DM per hand-up: the seen key
// carries handed_up_at, and operator_dm_at at or after it means this hand-up
// was already delivered (restart-safe). Safe on a nil receiver.
func (e *EscalationOperatorDM) EscalationHandedUp(ctx context.Context, bead BeadEvent) {
	if e == nil || bead.ID == "" {
		return
	}
	at := strings.TrimSpace(bead.Fields["handed_up_at"])
	if at == "" || strings.TrimSpace(bead.Fields["approved_by"]) != "" {
		return
	}
	detail, err := e.daemon.GetBead(ctx, bead.ID)
	if err != nil {
		e.logger.Warn("escalation hand-up DM: cannot read bead", "bead", bead.ID, "error", err)
		return
	}
	if escalationSettled(detail) {
		return
	}
	if strings.TrimSpace(detail.Fields["handed_up_at"]) != at {
		return // a newer hand-up has its own event
	}
	if dm := strings.TrimSpace(detail.Fields["operator_dm_at"]); dm != "" && dm >= at {
		return // RFC3339 UTC strings order as times
	}
	// The hand-up restamped audience from the new held_by (D11); the same
	// #756 gate as a create.
	if !beadsapi.EscalationForOwner(escalationAudienceFields(detail.Fields, bead.Fields)) {
		return
	}
	operatedBy, ok := escalationDMRecipient(detail)
	if !ok || operatedBy == "" || !IsHumanOperator(operatedBy) {
		return
	}
	by := strings.TrimSpace(detail.Fields["handed_up_by"])
	if by == "" {
		by = "a coordinator"
	}
	headline := escapeSlackMrkdwn(truncateRunes(by, 80)) + " handed this escalation up to you"
	if reason := strings.TrimSpace(detail.Fields["hand_up_reason"]); reason != "" {
		headline += ": " + escapeSlackMrkdwn(truncateRunes(reason, 300))
	} else {
		headline += "."
	}
	e.deliver(ctx, bead, detail, operatedBy, bead.ID+"#hu:"+at, headline, false)
}

// deliver resolves operatedBy's Slack id and sends one DM, deduped by seenKey,
// then stamps operator_dm_at. checkSelf skips the DM when the creator is that
// same human (a create, not a hand-up). headline "" is the create wording.
func (e *EscalationOperatorDM) deliver(ctx context.Context, bead BeadEvent, detail *beadsapi.BeadDetail, operatedBy, seenKey, headline string, checkSelf bool) {
	op := e.index.Resolve(ctx, operatedBy, detail.OperatedByID, detail.OperatedByEmail)
	if op.SlackID == "" {
		op.SlackID = e.fallbackSlackID(ctx, bead.ID, op)
	}
	if op.SlackID == "" {
		// The headline owner-notification path just went silent for this
		// escalation: count it (escalationOperatorDMUnresolvedTotal) and log at
		// ERROR so it is not buried among WARNs (gd-So39r4Fo4c).
		escalationOperatorDMUnresolvedTotal.Inc()
		e.logger.Error("escalation operator DM: no slack id for addressed operator",
			"bead", bead.ID, "operated_by", operatedBy)
		return
	}

	// A human who filed an escalation addressed to themselves already knows.
	// sameOperator compares generously and its false-"same" direction is
	// silence, which is the safe way to be wrong here.
	if checkSelf {
		if creator := escalationCreator(bead, detail.CreatedBy); creator != "" &&
			sameOperator(ctx, op, creator, e.users) {
			return
		}
	}

	// CLAIM before sending so a concurrent redelivery cannot double-DM, and
	// hand the claim back if the send fails — a transient Slack error must not
	// turn into a permanently undelivered ask.
	if e.seen.markSeen(seenKey) {
		return
	}

	title := detail.Title
	if title == "" {
		title = bead.Title
	}
	sensitivity := strings.TrimSpace(bead.Fields["sensitivity"])
	body := ""
	if !escalationDMWithholdsBody(sensitivity) {
		body = strings.TrimSpace(bead.Fields["context"])
	}
	reviewURL := ""
	if e.reviewURL != nil {
		reviewURL = e.reviewURL(bead.ID)
	}

	text := buildEscalationOperatorDM(escalationOperatorDMMessage{
		BeadLink:        e.beadLink(bead.ID, title),
		Mention:         op.Mention(),
		OperationType:   strings.TrimSpace(bead.Fields["operation_type"]),
		ExecutionTarget: strings.TrimSpace(bead.Fields["execution_target"]),
		Target:          escalationRunsWhere(ctx, e.daemon, detail.Fields),
		Sensitivity:     sensitivity,
		RequestingAgent: extractAgentName(strings.TrimSpace(bead.Fields["requesting_agent"])),
		Context:         body,
		ReviewURL:       reviewURL,
		Headline:        headline,
	})

	dmChannel, dmTS, err := e.sender.SendDM(ctx, op.SlackID, text)
	if err != nil {
		e.seen.release(seenKey)
		e.logger.Error("escalation operator DM failed, claim released for retry",
			"bead", bead.ID, "slack_id", op.SlackID, "error", err)
		return
	}

	// Stamp delivery so an agent reading the bead can tell the addressed human
	// was pinged personally, not merely posted at (same intent as notified_at,
	// ka-0000003667). The channel id is withheld for sensitive/secret, matching
	// the slack_channel carve-out in NotifyNewEscalation.
	//
	// BOTH KEYS MUST STAY DECLARED in the daemon's type:escalation schema
	// (kbeads internal/server/config.go) — it sets allow_additional_fields=false
	// and the update path grandfathers only keys ALREADY stored on the bead, so
	// an undeclared key is a hard 400. Verified against the live daemon while
	// building this: the stamp 400'd while the DM itself succeeded, making the
	// failure invisible apart from the Warn below and rendering the `gb
	// escalation show` readback permanently blank. That is the third instance of
	// this class here (ka-0000004605, ka-0000004864), so
	// TestBuiltinEscalationReflagPingbackFieldsDeclared lists both keys.
	//
	// Deliberately log-and-continue rather than release the claim: the DM — the
	// thing the operator actually asked for — has already landed, so a stamp
	// failure must not re-send it. One release cuts every component on a single
	// tag, so the schema row and this writer ship together.
	fields := map[string]string{
		"operator_dm_at": time.Now().UTC().Format(time.RFC3339),
	}
	if !escalationDMWithholdsBody(sensitivity) {
		fields["operator_dm_channel"] = dmChannel
	}
	if err := e.daemon.UpdateBeadFields(ctx, bead.ID, fields); err != nil {
		e.logger.Warn("escalation operator DM: failed to stamp delivery fields",
			"bead", bead.ID, "error", err)
	}
	if e.recordDM != nil {
		e.recordDM(ctx, bead, op.Email, dmChannel, dmTS, text)
	}

	e.logger.Info("escalation operator DM sent",
		"bead", bead.ID, "operated_by", operatedBy, "slack_id", op.SlackID,
		"operation_type", bead.Fields["operation_type"])
}

// escalationSettled reports whether a freshly read escalation is no longer an
// open ask: approved (by a person or a policy) or closed. Such an escalation
// gets no DM, however recently it was created (gd-Q86qORzVrx).
func escalationSettled(detail *beadsapi.BeadDetail) bool {
	return strings.EqualFold(strings.TrimSpace(detail.Status), "closed") ||
		strings.TrimSpace(detail.Fields["approved_by"]) != ""
}

// escalationDMWithholdsBody reports whether the escalation's context body and
// DM channel id must be kept out of, respectively, the DM text and the bead
// fields. Mirrors the sensitivity carve-out in NotifyNewEscalation.
func escalationDMWithholdsBody(sensitivity string) bool {
	switch strings.ToLower(strings.TrimSpace(sensitivity)) {
	case "sensitive", "secret":
		return true
	}
	return false
}

// escalationCreator picks the identity that filed the escalation, preferring the
// server-resolved actor over the created_by string. Mechanical creators (agents,
// fleet sentinels) are returned as-is; sameOperator will not match them to a
// human, which is the intended outcome.
func escalationCreator(bead BeadEvent, detailCreatedBy string) string {
	for _, v := range []string{bead.Actor, bead.CreatedBy, detailCreatedBy} {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func (e *EscalationOperatorDM) beadLink(id, title string) string {
	if e.link != nil {
		if s := e.link(id, title); s != "" {
			return s
		}
	}
	if title == "" {
		return id
	}
	return fmt.Sprintf("%s — %s", id, escapeSlackMrkdwn(title))
}

type escalationOperatorDMMessage struct {
	// Headline replaces "a new escalation is addressed to you." (a hand-up's
	// wording); "" keeps it. Already Slack-escaped by the caller.
	Headline        string
	BeadLink        string
	Mention         string
	OperationType   string
	ExecutionTarget string
	// Target is where the escalation will run once approved
	// (escalationRunsWhere); "" omits the line.
	Target          string
	Sensitivity     string
	RequestingAgent string
	Context         string
	ReviewURL       string
}

// buildEscalationOperatorDM renders the DM. Kept pure so the exact wording is
// asserted by tests rather than discovered in production.
func buildEscalationOperatorDM(m escalationOperatorDMMessage) string {
	var b strings.Builder
	headline := m.Headline
	if headline == "" {
		headline = "a new escalation is addressed to you."
	}
	fmt.Fprintf(&b, ":rotating_light: %s — %s\n", m.Mention, headline)
	fmt.Fprintf(&b, "*%s*\n", m.BeadLink)

	var meta []string
	if m.OperationType != "" {
		meta = append(meta, fmt.Sprintf("*Type:* `%s`", escapeSlackMrkdwn(truncateRunes(m.OperationType, 120))))
	}
	if m.ExecutionTarget != "" {
		meta = append(meta, fmt.Sprintf("*Target:* `%s`", escapeSlackMrkdwn(truncateRunes(m.ExecutionTarget, 60))))
	}
	if m.Sensitivity != "" {
		meta = append(meta, fmt.Sprintf("*Sensitivity:* %s `%s`",
			sensitivityEmoji(m.Sensitivity), escapeSlackMrkdwn(truncateRunes(m.Sensitivity, 40))))
	}
	if m.RequestingAgent != "" {
		meta = append(meta, fmt.Sprintf("*Requested by:* `%s`", escapeSlackMrkdwn(truncateRunes(m.RequestingAgent, 80))))
	}
	if len(meta) > 0 {
		b.WriteString(strings.Join(meta, " | ") + "\n")
	}
	if m.Target != "" {
		fmt.Fprintf(&b, "*Where it will run:* %s\n", escapeSlackMrkdwn(truncateRunes(m.Target, 160)))
	}

	if m.Context != "" {
		fmt.Fprintf(&b, "\n%s\n", quoteSlackLines(truncateRunes(m.Context, escalationDMContextLimit)))
	}

	if m.ReviewURL != "" {
		fmt.Fprintf(&b, "\n<%s|Review in Web UI> — approval happens there, not in Slack.", m.ReviewURL)
	} else {
		b.WriteString("\n_Approval happens in the web UI, not in Slack._")
	}
	return b.String()
}

// escalationRunsWhere says where an escalation will run once approved, for
// the approver (spec §9 request card): for execution_target=local, the runner
// it names, as "<hostname> (<owner>)" read from the runner bead (or the owner
// whose runner the daemon will pick, for target_owner alone); otherwise the
// cluster, with the role it spawns. "" when the escalation names no target.
func escalationRunsWhere(ctx context.Context, daemon interface {
	GetBead(ctx context.Context, beadID string) (*beadsapi.BeadDetail, error)
}, fields map[string]string,
) string {
	if strings.TrimSpace(fields["execute_by"]) == beadsapi.LaneRequester {
		return "the requesting agent"
	}
	switch target := strings.TrimSpace(fields["execution_target"]); target {
	case "":
		return ""
	case "local":
		owner := strings.TrimSpace(fields["target_owner"])
		runnerID := strings.TrimSpace(fields["target_runner"])
		if runnerID == "" {
			if owner == "" {
				return "a local runner (none named)"
			}
			return "a runner of " + strings.TrimPrefix(owner, "oidc:")
		}
		host := runnerID
		if daemon != nil {
			if r, err := daemon.GetBead(ctx, runnerID); err == nil && r != nil {
				if h := runnerHostname(r); h != "" {
					host = h
				}
				if o := runnerOwner(r); o != "" {
					owner = o
				}
			}
		}
		if owner == "" {
			return host
		}
		return fmt.Sprintf("%s (%s)", host, strings.TrimPrefix(owner, "oidc:"))
	default:
		if role := strings.TrimSpace(fields["spawn_role"]); role != "" {
			return "cluster, role " + role
		}
		return "cluster"
	}
}

// quoteSlackLines renders text as a Slack blockquote so a multi-line context
// body reads as quoted material rather than as the message's own prose.
func quoteSlackLines(s string) string {
	lines := strings.Split(escapeSlackMrkdwn(s), "\n")
	for i, ln := range lines {
		lines[i] = "> " + ln
	}
	return strings.Join(lines, "\n")
}

// escalationOperatorDMUnresolvedTotal counts escalations addressed to a human
// whose Slack id could not be resolved by ANY source, so no DM was sent. Any
// increase means an operator missed their personal ping (gd-So39r4Fo4c).
var escalationOperatorDMUnresolvedTotal = promauto.NewCounter(prometheus.CounterOpts{
	Name: "escalation_operator_dm_unresolved_total",
	Help: "Escalations addressed to a human operator that got no DM because no Slack id resolved from the operators alias feed, the Slack account links, or the user beads.",
})

// escalationOperatorDMSkippedTotal counts owner-audience escalations that got
// no fleet DM because held_by addresses a land's owners (reached through the
// Commons, Lands) or, for a stale stamp, nobody. In-hand escalations never
// get here: #756's audience gate returns first.
var escalationOperatorDMSkippedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "escalation_operator_dm_skipped_total",
	Help: "Owner-audience escalation DMs not sent because held_by addresses no fleet operator, by lane kind.",
}, []string{"lane"})

// escalationDMRecipient is the operated_by identity to DM for an owner-audience
// escalation (the caller has applied #756's gate), from held_by via
// conversation.EscalationAddressees; ok=false means held_by addresses no fleet
// operator. The operator lane, and an unstamped escalation, DM operated_by
// exactly as #756 does (Global Constraint 7).
func escalationDMRecipient(detail *beadsapi.BeadDetail) (string, bool) {
	held := beadsapi.EscalationHeldBy(detail)
	for _, a := range conversation.EscalationAddressees(held, projectFromLabels(detail.Labels)) {
		if a.Lane == beadsapi.LaneOperator {
			return strings.TrimSpace(detail.OperatedBy), true
		}
	}
	kind, _, _ := strings.Cut(held, ":")
	escalationOperatorDMSkippedTotal.WithLabelValues(kind).Inc()
	return "", false
}

// fallbackSlackID resolves the addressed operator's Slack id when the
// operators alias feed has no row for them. That feed is keyed by the email the
// operators resync last saw in Slack, so a person whose gardenio sign-in email
// (oidc:<email>) differs from their alias row is missing from it even though
// they are linked (gd-So39r4Fo4c). In order:
//
//  1. the confirmed Slack account link for that email;
//  2. the type=user beads for that email (email -> slack_id).
//
// Each source must name exactly ONE distinct Slack id; ambiguity yields "" —
// DMing the wrong human is worse than the (now loud) miss.
func (e *EscalationOperatorDM) fallbackSlackID(ctx context.Context, beadID string, op resolvedOperator) string {
	if op.Email == "" {
		return ""
	}
	if e.linked != nil {
		if ids := e.linked(op.Email); len(ids) == 1 && slackUserIDShape.MatchString(ids[0]) {
			e.logger.Info("escalation operator DM: resolved via slack account link",
				"bead", beadID, "operated_by", op.Raw)
			return ids[0]
		}
	}
	if e.users != nil {
		ids := map[string]bool{}
		for _, u := range e.users.All(ctx) {
			if normalizeEmail(u.Email) == op.Email && slackUserIDShape.MatchString(strings.TrimSpace(u.SlackID)) {
				ids[strings.TrimSpace(u.SlackID)] = true
			}
		}
		if id, ok := soleSlackID(ids); ok {
			e.logger.Info("escalation operator DM: resolved via user bead",
				"bead", beadID, "operated_by", op.Raw)
			return id
		}
	}
	return ""
}
