package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"

	"github.com/slack-go/slack"
)

// Slice 4a of the Commons migration (design spec
// 2026-10-19-lands-commons-design.md, sections 3.4 and 5.2): a card button or
// a modal submission, from Slack or from the Commons, becomes one
// conversation.Inbound{Action} or Inbound{Submit}, and one dispatcher runs
// HandleAction or HandleSubmit on it.
//
//   - Slack: handleBlockActions and handleViewSubmission are the adapter. A
//     Slack Inbound carries its interaction callback in Raw, and the handlers
//     under it still answer in Slack's own terms: the modals, the ephemerals by
//     response_url and the card edits Slack shapes. Their calls are pinned by
//     TestInboundActionCallsGolden.
//   - Commons: the Commons calls POST /api/conversation/inbound with the card
//     bead a button sat on (api_conversation.go). The bridge reads the card and
//     its subject itself, checks the action against the caller's land role
//     (conversation.Allows), and the handlers here do what needs the fleet: the
//     decision close and the nudge, the permission verdict, the kill or
//     restart. Each answer comes back as an ephemeral, which the Commons shows
//     the person alone. The decision watcher re-renders the Slack copy of a
//     decision card when its decision closes.
//
// Escalation Approve and Reject need nothing of the fleet: the Commons does
// them on the person's own bearer, as the bead page does.

// errSlackActionSkipped is a Slack block action no handler takes (a link
// button, a stale action id): the adapter goes on to the click's next action.
var errSlackActionSkipped = errors.New("bridge: slack block action not taken")

// slackBlockActionEvent is the Raw of a Slack Inbound{Action}.
type slackBlockActionEvent struct {
	Callback slack.InteractionCallback
	Action   *slack.BlockAction
}

// slackActionInbound normalises one block action of a Slack click.
func slackActionInbound(cb slack.InteractionCallback, a *slack.BlockAction) conversation.Inbound {
	ts := cb.Message.Timestamp
	if ts == "" {
		ts = cb.MessageTs
	}
	return conversation.Inbound{
		Kind:     conversation.InboundAction,
		Surface:  conversation.SurfaceSlack,
		Actor:    conversation.Principal{SlackUser: cb.User.ID, Name: cb.User.Name},
		ActionID: a.ActionID,
		Value:    a.Value,
		At:       conversation.Ref{Surface: conversation.SurfaceSlack, Channel: cb.Channel.ID, TS: ts},
		Trigger: conversation.Trigger{Surface: conversation.SurfaceSlack, TriggerID: cb.TriggerID,
			Channel: cb.Channel.ID, ThreadTS: cb.Container.ThreadTs},
		Raw: slackBlockActionEvent{Callback: cb, Action: a},
	}
}

// slackSubmitInbound normalises a Slack modal submission. ActionID is the
// view's callback_id.
func slackSubmitInbound(cb slack.InteractionCallback) conversation.Inbound {
	return conversation.Inbound{
		Kind:     conversation.InboundSubmit,
		Surface:  conversation.SurfaceSlack,
		Actor:    conversation.Principal{SlackUser: cb.User.ID, Name: cb.User.Name},
		ActionID: cb.View.CallbackID,
		Trigger:  conversation.Trigger{Surface: conversation.SurfaceSlack, TriggerID: cb.TriggerID},
		Raw:      cb,
	}
}

// handleBlockActions is the Slack adapter for card buttons. The first block
// action a handler takes ends the click, as it always did.
func (b *Bot) handleBlockActions(ctx context.Context, callback slack.InteractionCallback) {
	for _, action := range callback.ActionCallback.BlockActions {
		if action == nil || action.ActionID == "" {
			continue
		}
		_, err := b.inbound().Dispatch(ctx, slackActionInbound(callback, action))
		if errors.Is(err, errSlackActionSkipped) {
			continue
		}
		if err != nil {
			b.logger.Warn("inbound: slack block action failed", "action_id", action.ActionID, "error", err)
		}
		return
	}
}

// handleViewSubmission is the Slack adapter for modal submissions.
func (b *Bot) handleViewSubmission(ctx context.Context, callback slack.InteractionCallback) {
	if callback.View.CallbackID == "" {
		return
	}
	_, err := b.inbound().Dispatch(ctx, slackSubmitInbound(callback))
	if err != nil && !errors.Is(err, errSlackActionSkipped) {
		b.logger.Warn("inbound: slack view submission failed", "callback_id", callback.View.CallbackID, "error", err)
	}
}

// HandleAction runs a card button.
func (b *Bot) HandleAction(ctx context.Context, in conversation.Inbound) (conversation.Answer, error) {
	if in.Surface == conversation.SurfaceCommons {
		return b.handleCommonsCard(ctx, in)
	}
	ev, ok := in.Raw.(slackBlockActionEvent)
	if !ok || ev.Action == nil {
		return conversation.Answer{}, fmt.Errorf("slack action %q carries no interaction: %w", in.ActionID, conversation.ErrNoHandler)
	}
	if !b.slackBlockAction(ctx, ev.Callback, ev.Action) {
		return conversation.Answer{}, errSlackActionSkipped
	}
	return conversation.Answer{}, nil
}

// HandleSubmit runs a modal (Slack) or lightbox (Commons) submission.
func (b *Bot) HandleSubmit(ctx context.Context, in conversation.Inbound) (conversation.Answer, error) {
	if in.Surface == conversation.SurfaceCommons {
		return b.handleCommonsCard(ctx, in)
	}
	cb, ok := in.Raw.(slack.InteractionCallback)
	if !ok {
		return conversation.Answer{}, fmt.Errorf("slack submission %q carries no interaction: %w", in.ActionID, conversation.ErrNoHandler)
	}
	if !b.slackViewSubmission(ctx, cb) {
		return conversation.Answer{}, errSlackActionSkipped
	}
	return conversation.Answer{}, nil
}

// InboundRefusal is a Commons action the bridge declined, with the status and
// the words the person sees.
type InboundRefusal struct {
	Status  int
	Message string
}

func (e *InboundRefusal) Error() string { return e.Message }

func inboundRefuse(status int, format string, args ...any) error {
	return &InboundRefusal{Status: status, Message: fmt.Sprintf(format, args...)}
}

// commonsCard is the card a Commons button or lightbox acted on, read as the
// bridge: the card bead, its one land and channel, and its subject.
type commonsCard struct {
	ID      string
	Land    string
	Channel string
	Card    *beadsapi.BeadDetail
	Subject *beadsapi.BeadDetail
}

// readCommonsCard reads card bead id and its subject. A bead that is not a
// card, a card or a subject that carries no land or two, and a subject from
// another land than its card's are refused (rule 1 of section 6.2).
func (b *Bot) readCommonsCard(ctx context.Context, id string) (commonsCard, error) {
	if b.daemon == nil {
		return commonsCard{}, inboundRefuse(http.StatusServiceUnavailable, "The bridge has no beads daemon.")
	}
	card, err := b.daemon.GetBead(ctx, id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return commonsCard{}, inboundRefuse(http.StatusNotFound, "That card is gone.")
		}
		return commonsCard{}, fmt.Errorf("read card %s: %w", id, err)
	}
	if card == nil || card.Type != "message" || card.Fields[conversation.BeadKind] != string(conversation.KindCard) ||
		strings.TrimSpace(card.Fields[conversation.BeadSubjectID]) == "" {
		return commonsCard{}, inboundRefuse(http.StatusUnprocessableEntity, "That is not a card.")
	}
	land, ok := onlyLand(card.Labels)
	if !ok {
		return commonsCard{}, inboundRefuse(http.StatusUnprocessableEntity, "That card belongs to no single land.")
	}
	subjectID := strings.TrimSpace(card.Fields[conversation.BeadSubjectID])
	subject, err := b.daemon.GetBead(ctx, subjectID)
	if err != nil || subject == nil {
		var apiErr *beadsapi.APIError
		if err == nil || (errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound) {
			return commonsCard{}, inboundRefuse(http.StatusNotFound, "The bead this card shows is gone.")
		}
		return commonsCard{}, fmt.Errorf("read subject %s: %w", subjectID, err)
	}
	// The subject must be in the card's land and in no other: a subject with
	// no land, or two, could be acted on from a land it is not in.
	if sl, ok := onlyLand(subject.Labels); !ok || sl != land {
		return commonsCard{}, inboundRefuse(http.StatusUnprocessableEntity, "That card shows a bead from another land.")
	}
	return commonsCard{ID: card.ID, Land: land, Channel: card.Fields[conversation.BeadChannelID], Card: card, Subject: subject}, nil
}

// onlyLand is the land of a bead carrying exactly one project: label.
func onlyLand(labels []string) (string, bool) {
	land, n := "", 0
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok {
			land, n = v, n+1
		}
	}
	return land, n == 1 && land != ""
}

// CommonsCardAction is the action-table entry a Commons card button (kind
// action) or lightbox (kind submit) takes on a subject of subjectType, the
// verb the land-role check is asked about. ok is false for one the bridge
// does not take.
func CommonsCardAction(kind conversation.InboundKind, subjectType, actionID string) (conversation.Action, bool) {
	switch string(kind) + " " + subjectType + " " + actionID {
	case "action decision resolve", "submit decision other":
		return conversation.ActionDecisionRespond, true
	case "action decision dismiss":
		return conversation.ActionDecisionDismiss, true
	case "submit decision promote":
		return conversation.ActionDecisionPromote, true
	case "action permission allow", "action permission deny", "action permission answered":
		return conversation.ActionPermissionAnswer, true
	case "action agent clear", "action agent kill", "action agent restart":
		return conversation.ActionAgentControl, true
	}
	return "", false
}

// handleCommonsCard runs a Commons card button or lightbox submission. The
// inbound endpoint has already checked the action against the caller's land
// role; in.Message is the card bead.
func (b *Bot) handleCommonsCard(ctx context.Context, in conversation.Inbound) (conversation.Answer, error) {
	card, err := b.readCommonsCard(ctx, in.Message)
	if err != nil {
		return conversation.Answer{}, err
	}
	if _, ok := CommonsCardAction(in.Kind, card.Subject.Type, in.ActionID); !ok {
		return conversation.Answer{}, inboundRefuse(http.StatusUnprocessableEntity, "This card has no %q.", in.ActionID)
	}
	who := strings.TrimPrefix(in.Actor.ID, "oidc:")
	subject := card.Subject
	switch subject.Type + " " + in.ActionID {
	case "decision resolve":
		return b.commonsResolveDecision(ctx, subject, in.Value, who)
	case "decision other":
		return b.commonsAnswerDecision(ctx, subject, in.Values["text"], who)
	case "decision dismiss":
		return b.commonsDismissDecision(ctx, subject, in.Values["reason"], who)
	case "decision promote":
		return b.commonsPromoteDecision(ctx, subject, who)
	case "permission allow", "permission deny":
		return b.commonsAnswerPermission(ctx, card, in.ActionID, in.Actor.ID)
	case "permission answered":
		return b.commonsPermissionAnswered(ctx, card, in.Values["behavior"], in.Values["said"], in.Actor.ID)
	default: // agent clear, kill, restart
		return b.commonsAgentControl(ctx, subject, in.ActionID)
	}
}

func sayAnswer(text string) conversation.Answer {
	return conversation.Answer{Ephemeral: []conversation.Message{{Kind: conversation.KindText, Text: text}}}
}

// openDecision refuses a decision that is answered already.
func openDecision(dec *beadsapi.BeadDetail) error {
	if dec.Status == "closed" {
		return inboundRefuse(http.StatusConflict, "This decision is answered already.")
	}
	return nil
}

// closeDecisionFromCommons closes dec with fields and nudges its agent.
func (b *Bot) closeDecisionFromCommons(ctx context.Context, dec *beadsapi.BeadDetail, fields map[string]string) error {
	if err := b.daemon.CloseBead(ctx, dec.ID, fields); err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusConflict) {
			return inboundRefuse(apiErr.StatusCode, "The beads daemon refused that: %s", strings.TrimSpace(apiErr.Message))
		}
		return fmt.Errorf("close decision %s: %w", dec.ID, err)
	}
	// Best-effort and slow (retries take up to 14s): never hold the answer.
	go b.nudgeDecisionAgent(dec.ID)
	return nil
}

// commonsResolveDecision answers dec with its option number value, the
// Commons twin of an option button and its confirm modal.
func (b *Bot) commonsResolveDecision(ctx context.Context, dec *beadsapi.BeadDetail, value, who string) (conversation.Answer, error) {
	if err := openDecision(dec); err != nil {
		return conversation.Answer{}, err
	}
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || n < 1 || n > len(parseDecisionOptionRows(dec.Fields["options"])) {
		return conversation.Answer{}, inboundRefuse(http.StatusBadRequest, "That option is not on this decision.")
	}
	chosenID, chosenDisplay := b.resolveOptionChoice(ctx, dec.ID, strconv.Itoa(n))
	fields := map[string]string{"chosen": chosenID, "rationale": fmt.Sprintf("Chosen by %s via the Commons", who)}
	if at := b.lookupArtifactType(ctx, dec.ID, chosenID); at != "" {
		fields["required_artifact"] = at
		fields["artifact_status"] = "pending"
	}
	if err := b.closeDecisionFromCommons(ctx, dec, fields); err != nil {
		return conversation.Answer{}, err
	}
	b.logger.Info("decision resolved via the Commons", "bead", dec.ID, "chosen", chosenID, "user", who)
	return sayAnswer(fmt.Sprintf("Recorded your choice: %s.", chosenDisplay)), nil
}

// commonsAnswerDecision answers dec in the person's own words (the Other
// lightbox). Words that name an option resolve to it, as in Slack.
func (b *Bot) commonsAnswerDecision(ctx context.Context, dec *beadsapi.BeadDetail, text, who string) (conversation.Answer, error) {
	if err := openDecision(dec); err != nil {
		return conversation.Answer{}, err
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return conversation.Answer{}, inboundRefuse(http.StatusBadRequest, "Write an answer first.")
	}
	fields := decisionResolutionFieldsFor(dec.Fields["options"], text)
	fields["rationale"] = fmt.Sprintf("Custom response by %s via the Commons", who)
	if err := b.closeDecisionFromCommons(ctx, dec, fields); err != nil {
		return conversation.Answer{}, err
	}
	b.logger.Info("decision resolved via the Commons (own words)", "bead", dec.ID, "user", who)
	return sayAnswer("Recorded your answer."), nil
}

// commonsDismissDecision dismisses dec, with the viewer's reason, when it
// gave one, after the rationale's who and where.
func (b *Bot) commonsDismissDecision(ctx context.Context, dec *beadsapi.BeadDetail, reason, who string) (conversation.Answer, error) {
	if err := openDecision(dec); err != nil {
		return conversation.Answer{}, err
	}
	rationale := fmt.Sprintf("Dismissed by %s via the Commons", who)
	if reason = strings.TrimSpace(reason); reason != "" {
		rationale += ": " + reason
	}
	fields := map[string]string{"chosen": "dismissed", "rationale": rationale}
	if err := b.daemon.CloseBead(ctx, dec.ID, fields); err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusConflict) {
			return conversation.Answer{}, inboundRefuse(apiErr.StatusCode, "The beads daemon refused that: %s", strings.TrimSpace(apiErr.Message))
		}
		return conversation.Answer{}, fmt.Errorf("dismiss decision %s: %w", dec.ID, err)
	}
	b.logger.Info("decision dismissed via the Commons", "bead", dec.ID, "user", who)
	return sayAnswer("Dismissed."), nil
}

// commonsPromoteDecision hands dec back to its agent to file a structured
// escalation (the Escalate lightbox, Slack's "Escalate (Agent)").
func (b *Bot) commonsPromoteDecision(ctx context.Context, dec *beadsapi.BeadDetail, who string) (conversation.Answer, error) {
	if err := openDecision(dec); err != nil {
		return conversation.Answer{}, err
	}
	agent, ok := b.returnDecisionToAgent(ctx, dec, who+" via the Commons")
	if !ok {
		return conversation.Answer{}, inboundRefuse(http.StatusUnprocessableEntity, "This decision names no agent to hand it back to.")
	}
	b.logger.Info("decision returned to agent for escalation via the Commons", "decision", dec.ID, "agent", agent, "user", who)
	return sayAnswer(fmt.Sprintf("Handed back to %s to file an escalation.", agent)), nil
}

// commonsAnswerPermission sends Allow or Deny for the permission request the
// card shows, records who answered (their verified principal, never a name
// they chose), and edits the request's Slack prompt to the answer.
func (b *Bot) commonsAnswerPermission(ctx context.Context, card commonsCard, behavior, principal string) (conversation.Answer, error) {
	req := card.Subject
	if req.Status == "closed" {
		return conversation.Answer{}, inboundRefuse(http.StatusConflict, "This permission request was already answered.")
	}
	requestID, agent := strings.TrimSpace(req.Fields["request_id"]), strings.TrimSpace(req.Fields["agent"])
	if requestID == "" || agent == "" {
		return conversation.Answer{}, inboundRefuse(http.StatusUnprocessableEntity, "This permission request names no agent to answer.")
	}
	who := permissionAnswerer{Principal: principal}
	email := strings.TrimPrefix(principal, "oidc:")
	by := "*" + email + "*"
	if u := b.UserDirectory().LookupByEmail(ctx, email); u != nil && u.SlackID != "" {
		who.SlackID, by = u.SlackID, "<@"+u.SlackID+">"
	}
	verdictErr := b.deliverPermissionVerdict(ctx, req, requestID, agent, behavior, who)
	if at, ok := conversation.CardRef(conversation.SurfaceSlack, card.Card); ok && b.api != nil {
		if _, _, _, err := b.api.UpdateMessageContext(ctx, at.Channel, at.TS,
			slack.MsgOptionBlocks(permissionResultBlocks(behavior, requestID, by, verdictErr)...)); err != nil {
			b.logger.Warn("permission: updating the Slack prompt failed", "bead", req.ID, "error", err)
		}
	}
	word := "Allowed"
	if behavior == "deny" {
		word = "Denied"
	}
	if verdictErr != nil {
		return sayAnswer(fmt.Sprintf("%s, but the answer did not reach %s: %v", word, agent, verdictErr)), nil
	}
	return sayAnswer(word + "."), nil
}

// commonsPermissionAnswered closes the permission request the card shows as
// answered on the Glass (Lands workbench-glass spec 3.3): the person answered
// Claude Code's dialog itself, so no verdict goes to the agent. behavior is
// allow, deny or "" (an option that says neither), said the option's label.
// It records who answered (their verified principal) and where, and edits the
// request's Slack prompt to the answer.
func (b *Bot) commonsPermissionAnswered(ctx context.Context, card commonsCard, behavior, said, principal string) (conversation.Answer, error) {
	req := card.Subject
	if req.Status == "closed" {
		return conversation.Answer{}, inboundRefuse(http.StatusConflict, "This permission request was already answered.")
	}
	if behavior != "" && behavior != "allow" && behavior != "deny" {
		return conversation.Answer{}, inboundRefuse(http.StatusBadRequest, "An answer is allow, deny or neither.")
	}
	who := permissionAnswerer{Principal: principal}
	email := strings.TrimPrefix(principal, "oidc:")
	by := "*" + email + "*"
	if u := b.UserDirectory().LookupByEmail(ctx, email); u != nil && u.SlackID != "" {
		who.SlackID, by = u.SlackID, "<@"+u.SlackID+">"
	}
	said = truncateRunes(strings.TrimSpace(said), 200)
	fields := map[string]string{"behavior": behavior, "answered_by": who.Principal, "answered_by_slack_id": who.SlackID,
		"answered_on": "glass", "answer_label": said}
	for k, v := range fields {
		if v == "" {
			delete(fields, k)
		}
	}
	if err := b.daemon.CloseBead(beadsapi.WithMachinePrincipal(ctx, b.machine), req.ID, fields); err != nil {
		return conversation.Answer{}, fmt.Errorf("close permission %s: %w", req.ID, err)
	}
	if at, ok := conversation.CardRef(conversation.SurfaceSlack, card.Card); ok && b.api != nil {
		if _, _, _, err := b.api.UpdateMessageContext(ctx, at.Channel, at.TS,
			slack.MsgOptionBlocks(permissionGlassBlocks(b.themeFor(ctx, at.Channel), behavior, strings.TrimSpace(req.Fields["request_id"]), said, by)...)); err != nil {
			b.logger.Warn("permission: updating the Slack prompt failed", "bead", req.ID, "error", err)
		}
	}
	b.logger.Info("permission answered on the Glass", "bead", req.ID, "behavior", behavior, "said", said, "principal", principal)
	return sayAnswer("Recorded."), nil
}

// permissionGlassBlocks is a permission prompt's face once it was answered on
// the Glass: the option's label, by whom (Slack mrkdwn), its glyph from the
// prompt channel's theme.
func permissionGlassBlocks(th Theme, behavior, requestID, said, by string) []slack.Block {
	emoji, label := th.Semantic(SemOK), "Answered"
	switch behavior {
	case "allow":
		label = "Allowed"
	case "deny":
		emoji, label = th.Semantic(SemDenied), "Denied"
	}
	text := fmt.Sprintf("%s *%s* on the Glass by %s", emoji, label, by)
	if said != "" { // the dialog's own words, which can carry the tool's input: never mrkdwn
		text += " — " + escapeSlackMrkdwn(sanitizeCodeFence(said))
	}
	return []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject("mrkdwn", text, false, false), nil, nil),
		slack.NewContextBlock("", slack.NewTextBlockObject("mrkdwn",
			fmt.Sprintf("Permission answered in the terminal — request `%s`", requestID), false, false)),
	}
}

// commonsAgentControl clears, kills or restarts the agent the card shows,
// through the same cores as the Slack card's buttons. The cores find an agent
// by name, and the bridge's token reaches every land, so the control is held
// to the card's own agent bead: a name that resolves to another bead (a
// same-named agent in another land) is refused, and the kill or restart
// itself checks the bead again.
func (b *Bot) commonsAgentControl(ctx context.Context, agentBead *beadsapi.BeadDetail, verb string) (conversation.Answer, error) {
	if agentBead.Type != "agent" {
		return conversation.Answer{}, inboundRefuse(http.StatusUnprocessableEntity, "This card shows no agent.")
	}
	name := strings.TrimSpace(agentBead.Fields["agent"])
	if name == "" {
		name = strings.TrimSpace(agentBead.Title)
	}
	if name == "" {
		return conversation.Answer{}, inboundRefuse(http.StatusUnprocessableEntity, "This card names no agent.")
	}
	if err := b.agentIsCardBead(ctx, name, agentBead, verb); err != nil {
		return conversation.Answer{}, err
	}
	s := &answerSay{logger: b.logger, agent: name}
	switch verb {
	case "clear":
		b.clearAgent(ctx, name, "", "", s.say)
		if len(s.msgs) == 0 {
			s.say(fmt.Sprintf("Cleared %s's Slack card.", name))
		}
	case "kill":
		b.killThreadAgent(ctx, name, agentBead.ID, "", "", s.say)
	case "restart":
		root, ok := slackThreadRoot(agentBead.Fields)
		if !ok {
			return conversation.Answer{}, inboundRefuse(http.StatusUnprocessableEntity,
				"%s is bound to no thread, so it cannot be restarted from its card.", name)
		}
		b.restartThreadAgent(ctx, name, agentBead.ID, root.Channel, root.TS, "", s.say)
	}
	return s.answer(), nil
}

// agentIsCardBead refuses an agent control when name, as the bridge resolves
// it, is not the card's agent bead. A kill or a restart needs the bead live; a
// clear (the card of an agent that may be gone) is refused only when name is
// now some other bead.
func (b *Bot) agentIsCardBead(ctx context.Context, name string, agentBead *beadsapi.BeadDetail, verb string) error {
	find := b.daemon.FindAgentBead
	if verb == "clear" {
		find = b.daemon.FindAgentBeadAnyStatus
	}
	live, err := find(ctx, extractAgentName(name))
	switch {
	case errors.Is(err, beadsapi.ErrAgentNotFound) && verb == "clear":
		return nil
	case errors.Is(err, beadsapi.ErrAgentNotFound):
		return inboundRefuse(http.StatusConflict, "%s is not running.", name)
	case err != nil:
		return fmt.Errorf("find agent %s: %w", name, err)
	case live == nil || live.ID != agentBead.ID:
		return inboundRefuse(http.StatusConflict, "%s is another agent now; this card's agent is gone.", name)
	}
	return nil
}

// answerSay collects what an agent control says while its click is answered.
// What it says later, from the goroutine a kill or a restart runs in, goes to
// the log: the person sees the card change instead.
type answerSay struct {
	mu     sync.Mutex
	done   bool
	msgs   []string
	logger *slog.Logger
	agent  string
}

func (a *answerSay) say(text string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.done {
		a.msgs = append(a.msgs, text)
		return
	}
	a.logger.Info("agent control from the Commons", "agent", a.agent, "outcome", text)
}

// said reports whether anything has been said for the answer yet.
func (a *answerSay) said() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.msgs) > 0
}

func (a *answerSay) answer() conversation.Answer {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.done = true
	var out conversation.Answer
	for _, m := range a.msgs {
		out.Ephemeral = append(out.Ephemeral, conversation.Message{Kind: conversation.KindText, Text: m})
	}
	return out
}
