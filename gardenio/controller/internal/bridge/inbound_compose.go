package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"

	"github.com/slack-go/slack/slackevents"
)

// Slice 4d of the Commons migration (design spec
// 2026-10-19-lands-commons-design.md, sections 2.3, 3.4 and 5.2): a mention,
// from Slack or from the Commons, is one conversation.Inbound{Compose}, and
// HandleCompose runs it.
//
//   - Slack: handleAppMention is the adapter. Every app_mention becomes
//     Inbound{Compose} carrying the event in Raw, and HandleCompose runs the
//     mention core (handleSlackMention) on it, unchanged:
//     TestInboundComposeCallsGolden pins its Slack calls.
//   - Commons: a message that names @gardenio or an agent of its land is
//     sent to POST /api/conversation/inbound as {"kind":"compose","message":
//     <id>}. The bridge reads the message itself, checks "spawn" against the
//     caller's land role (and "agent_control" for kill, stop, restart and
//     clear), and runs commonsCompose on the person's bearer:
//   - @<agent> nudges that agent of the land with the words, or wakes it in
//     its own thread when it has stopped;
//   - @gardenio <verb> (kill, stop, restart, clear) works on the thread's
//     agent;
//   - any other @gardenio goes to the thread's agent (a nudge, or a respawn
//     with resume) or, with none, spawns one, with --role and --model as in
//     Slack. A chat root (a plain message) mentioned at the top level or in
//     its thread is promoted to a wish head first.
//
// Only the inbound call composes: it alone carries the caller's land role.
// The SSE watchers leave a composing message to it (commonsComposes), and
// they and the sweep dispatch every message as a reply, so a guest's
// @gardenio never spawns through the back door. A caller whose role may not
// spawn (a guest) still has their mention in a thread delivered: the inbound
// call dispatches it as the reply it would have been before slice 4d.

// The bot's name in a Commons mention.
const commonsBotName = "gardenio"

// Promotion fields (section 2.3). A chat root that becomes a wish keeps its
// words where they were; the head names it, and it names the head.
// Replies are not re-pointed: kbeads lets only a message's writer move its
// thread_id (gd-74wnmpQgyT), so a promoted thread is every message whose
// thread_id is the head or the root it was promoted from.
const (
	fieldPromotedFrom = "promoted_from" // on the wish head: the chat root it was promoted from
	fieldPromotedTo   = "promoted_to"   // on the chat root: its wish head (best-effort)
)

// commonsSpawnSource is the spawn_source of a head the Commons minted.
const commonsSpawnSource = "commons-thread"

// mentionToken is an @name at the start of the text or after a space.
var mentionToken = regexp.MustCompile(`(^|\s)@([A-Za-z0-9][A-Za-z0-9._-]{0,127})`)

// commonsMentionTokens returns the names text @-mentions, lowercased, once
// each, in order.
func commonsMentionTokens(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range mentionToken.FindAllStringSubmatch(text, -1) {
		name := strings.ToLower(strings.TrimRight(m[2], "."))
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// stripMentions drops every @name in names (lowercased) from text.
func stripMentions(text string, names map[string]bool) string {
	out := mentionToken.ReplaceAllStringFunc(text, func(tok string) string {
		lead := ""
		if tok != "" && (tok[0] == ' ' || tok[0] == '\t' || tok[0] == '\n' || tok[0] == '\r') {
			lead = tok[:1]
		}
		name := strings.ToLower(strings.TrimRight(strings.TrimSpace(tok)[1:], "."))
		if names[name] {
			return lead
		}
		return tok
	})
	return strings.Join(strings.Fields(out), " ")
}

// commonsVerb is the in-thread verb a Commons mention carries ("kill",
// "stop", "restart", "clear"), or "": the words after @gardenio are the verb
// alone, as in Slack (parseThreadCommand, parseMentionCommand).
func commonsVerb(text string) (verb string, force bool) {
	toks := commonsMentionTokens(text)
	if !slices.Contains(toks, commonsBotName) {
		return "", false
	}
	rest := stripMentions(text, map[string]bool{commonsBotName: true})
	if cmd, f := parseThreadCommand(rest); cmd != "" {
		return cmd, f
	}
	if cmd, args := parseMentionCommand(rest); cmd == "clear" && strings.TrimSpace(args) == "" {
		return cmd, false
	}
	return "", false
}

// slackComposeInbound normalises a Slack app_mention. Actor names only the
// Slack user: the mention core resolves the account link itself, after its
// bot-message guard.
func slackComposeInbound(ev *slackevents.AppMentionEvent) conversation.Inbound {
	in := conversation.Inbound{
		Kind:    conversation.InboundCompose,
		Surface: conversation.SurfaceSlack,
		Actor:   conversation.Principal{SlackUser: ev.User},
		Text:    ev.Text,
		At:      conversation.Ref{Surface: conversation.SurfaceSlack, Channel: ev.Channel, TS: ev.TimeStamp},
		Trigger: conversation.Trigger{Surface: conversation.SurfaceSlack, Channel: ev.Channel, ThreadTS: ev.ThreadTimeStamp},
		Raw:     ev,
	}
	if ev.ThreadTimeStamp != "" {
		in.Roots = []conversation.Ref{{Surface: conversation.SurfaceSlack, Channel: ev.Channel, TS: ev.ThreadTimeStamp}}
	}
	return in
}

// handleAppMention is the Slack adapter for mentions: every app_mention is
// dispatched as Inbound{Compose}. The dispatcher is the panic boundary.
func (b *Bot) handleAppMention(ctx context.Context, ev *slackevents.AppMentionEvent) {
	if ev == nil {
		return
	}
	if _, err := b.inbound().Dispatch(ctx, slackComposeInbound(ev)); err != nil {
		b.logger.Warn("inbound: mention dispatch failed",
			"channel", ev.Channel, "thread_ts", ev.ThreadTimeStamp, "error", err)
	}
}

// HandleCompose runs a mention: Slack's through the mention core, the
// Commons' through commonsCompose.
func (b *Bot) HandleCompose(ctx context.Context, in conversation.Inbound) (conversation.Answer, error) {
	if in.Surface == conversation.SurfaceCommons {
		return b.commonsCompose(ctx, in)
	}
	ev, ok := in.Raw.(*slackevents.AppMentionEvent)
	if !ok || ev == nil {
		return conversation.Answer{}, fmt.Errorf("slack mention carries no event: %w", conversation.ErrNoHandler)
	}
	b.handleSlackMention(ctx, ev)
	return conversation.Answer{}, nil
}

// commonsMention is what a Commons message's mentions resolve to in its land.
type commonsMention struct {
	Bot    bool
	Agents []string // agent names of the land, as mentioned
	Text   string   // the words without the resolved mentions
}

func (m commonsMention) any() bool { return m.Bot || len(m.Agents) > 0 }

// resolveCommonsMention resolves text's @names in land: @gardenio is the bot,
// any other name an agent whose bead carries the land. A person's name, or an
// agent of another land, stays in the words.
func (b *Bot) resolveCommonsMention(ctx context.Context, land, text string) commonsMention {
	var m commonsMention
	resolved := map[string]bool{}
	for _, name := range commonsMentionTokens(text) {
		if name == commonsBotName {
			m.Bot, resolved[name] = true, true
			continue
		}
		if b.daemon == nil || land == "" {
			continue
		}
		bead, err := b.daemon.FindAgentBeadAnyStatus(ctx, name)
		if err != nil || bead == nil || agentLand(bead) != land {
			continue
		}
		m.Agents, resolved[name] = append(m.Agents, name), true
	}
	m.Text = stripMentions(text, resolved)
	return m
}

// agentLand is an agent bead's land: its one project label, else its project
// field.
func agentLand(bead *beadsapi.BeadDetail) string {
	if land, ok := onlyLand(bead.Labels); ok {
		return land
	}
	return strings.TrimSpace(bead.Fields["project"])
}

// agentInLand reports whether the agent named agent is of land. A Commons
// thread's head, and the slack_thread_channel/ts on it that name its Slack
// copy, are their writers': a land-A member may point a head at land B's
// Slack thread or agent-card thread. So whatever agent a thread leads to, by
// its binding, its card or the daemon, is held to the thread's land before it
// is nudged, woken, killed or restarted (gd-cVqdya4Zfo). Its newest bead must
// be of land, and so must its live bead, when it has one: names are not
// unique across lands, and the live bead is the one a nudge or a kill
// reaches. No land (a message that carries none) holds no agent.
func (b *Bot) agentInLand(ctx context.Context, agent, land string) bool {
	if land == "" || agent == "" || b.daemon == nil {
		return false
	}
	name := extractAgentName(agent)
	bead, err := b.daemon.FindAgentBeadAnyStatus(ctx, name)
	if err != nil || bead == nil || agentLand(bead) != land {
		return false
	}
	live, err := b.daemon.FindAgentBead(ctx, name)
	if errors.Is(err, beadsapi.ErrAgentNotFound) {
		return true // stopped: its newest bead is of land
	}
	return err == nil && live != nil && agentLand(live) == land
}

// slackChannelOfLand reports whether a channel bead of land binds the Slack
// channel. A Commons thread's Slack copy is named by fields its writers set
// (a head's slack_thread_channel/ts, a root's slack_channel/slack_ts, a
// reply's own), but a real copy is only ever made in a channel a channel bead
// of the land binds (the mirror), so a Slack thread anywhere else is not one:
// following it would read another land's Slack history, post there, or spawn
// in that channel's project (gd-cVqdya4Zfo). No land binds no channel.
func (b *Bot) slackChannelOfLand(ctx context.Context, slackChannel, land string) bool {
	if land == "" {
		return false
	}
	ref, ok := b.resolveChannelBead(ctx, slackChannel)
	return ok && ref.Land == land
}

// commonsComposes reports whether m may be a Commons mention, which only the
// inbound call may run (it alone carries the caller's land role): the SSE
// watchers leave it to that call, and to the sweep after it. A name the
// daemon could not look up counts as a mention: failing closed costs a reply
// the sweep's wait, where failing open would race the person's mention.
func (b *Bot) commonsComposes(ctx context.Context, m commonsMessage) bool {
	if m.Fields[conversation.BeadOrigin] != conversation.SurfaceCommons {
		return false
	}
	land := projectFromLabels(m.Labels)
	for _, name := range commonsMentionTokens(m.Fields[conversation.BeadText]) {
		if name == commonsBotName {
			return true
		}
		if b.daemon == nil || land == "" {
			continue
		}
		bead, err := b.daemon.FindAgentBeadAnyStatus(ctx, name)
		switch {
		case errors.Is(err, beadsapi.ErrAgentNotFound):
		case err != nil:
			b.logger.Warn("inbound: looking up a mentioned name failed; leaving the message to the inbound call",
				"message", m.ID, "name", name, "error", err)
			return true
		case bead != nil && agentLand(bead) == land:
			return true
		}
	}
	return false
}

// commonsCompose runs a Commons mention for the person in.Actor. The inbound
// endpoint has checked "spawn" (and "agent_control" for a verb) against their
// land role.
func (b *Bot) commonsCompose(ctx context.Context, in conversation.Inbound) (conversation.Answer, error) {
	if dm, ok := b.commonsDM(ctx, in.Channel); ok {
		return conversation.Answer{}, b.handleCommonsDM(ctx, in, dm)
	}
	mention := b.resolveCommonsMention(ctx, in.Land, in.Text)
	if !mention.any() {
		// Only people (or agents of other lands) are named: a reply is a reply.
		if in.Thread != "" {
			in.Kind = conversation.InboundReply
			return conversation.Answer{}, b.handleCommonsReply(ctx, in)
		}
		return conversation.Answer{}, inboundRefuse(http.StatusUnprocessableEntity, "That message names no agent.")
	}
	if msg := b.commonsSpawnRefusal(ctx, in.Land, mention.Text); msg != "" {
		return conversation.Answer{}, inboundRefuse(http.StatusUnprocessableEntity, "%s", msg)
	}
	s := &answerSay{logger: b.logger, agent: strings.Join(mention.Agents, ",")}
	userID := b.slackUserOf(ctx, in.Actor.ID)
	if !mention.Bot {
		for _, name := range mention.Agents {
			b.commonsNudgeNamed(ctx, in, name, mention.Text, userID, s)
		}
		return s.answer(), nil
	}
	head, err := b.commonsComposeHead(ctx, in)
	if err != nil {
		return conversation.Answer{}, err
	}
	at, ok := b.commonsThreadAt(ctx, head, in, s.say)
	if !ok {
		return conversation.Answer{}, inboundRefuse(http.StatusUnprocessableEntity, "%s", errSlackCopyNotOfLand)
	}
	if err := b.composeInThread(ctx, at, in, mention.Text, userID, s); err != nil {
		return conversation.Answer{}, err
	}
	return s.answer(), nil
}

// errSlackCopyNotOfLand is the refusal for a thread whose Slack copy is not in
// a channel of its land (slackChannelOfLand).
const errSlackCopyNotOfLand = "That thread's Slack copy belongs to another land."

// commonsThreadAt is the anchor of a Commons thread: its head, the head's
// Slack thread when it has one, and the person who asked. It reports false
// when the head names a Slack thread outside its land's channels
// (slackChannelOfLand): the head's slack_thread_channel/ts are its writer's.
func (b *Bot) commonsThreadAt(ctx context.Context, head *beadsapi.BeadDetail, in conversation.Inbound, say func(string)) (threadAt, bool) {
	at := threadAt{
		Head:      head.ID,
		Land:      in.Land,
		ChannelID: firstNonEmptyField(head.Fields, conversation.BeadChannelID),
		Actor:     in.Actor.ID,
		Say:       say,
	}
	if at.ChannelID == "" {
		at.ChannelID = in.Channel
	}
	if root, ok := slackThreadRoot(head.Fields); ok {
		if !b.slackChannelOfLand(ctx, root.Channel, in.Land) {
			b.logger.Warn("compose: a head names a Slack thread outside its land's channels; refusing it",
				"head", head.ID, "land", in.Land, "slack_channel", root.Channel, "slack_ts", root.TS)
			return threadAt{}, false
		}
		at.Channel, at.TS = root.Channel, root.TS
	}
	return at, true
}

// slackUserOf is the Slack user of principal (oidc:<email>), "" when the
// person has none: the spawn and respawn cores attribute a Slack thread's
// posts to it.
func (b *Bot) slackUserOf(ctx context.Context, principal string) string {
	email, ok := strings.CutPrefix(principal, "oidc:")
	if !ok || email == "" {
		return ""
	}
	if u := b.UserDirectory().LookupByEmail(ctx, email); u != nil {
		return u.SlackID
	}
	return ""
}

// composeInThread runs @gardenio in a thread: a verb on the thread's agent,
// else the words to the thread's agent (a nudge, or a respawn with resume when
// it has stopped), else a fresh agent for the thread. A fresh agent takes a
// slot of the person's spawn rate first (admitCommonsSpawn), whatever made
// the head: a member may create a bead that looks like one (gd-PTOUU1ufzq).
func (b *Bot) composeInThread(ctx context.Context, at threadAt, in conversation.Inbound, text, userID string, s *answerSay) error {
	if verb, force := commonsVerb(in.Text); verb != "" {
		b.commonsThreadVerb(ctx, at, verb, force, userID, s)
		return nil
	}
	agent := b.headAgent(ctx, at)
	if agent == "" {
		if err := b.admitCommonsSpawn(ctx, in, commonsRootID(in)); err != nil {
			return err
		}
	}
	b.toThreadAgent(ctx, at, agent, "Commons mention", text, in.Message, userID, s)
	return nil
}

// commonsRootID is the root of the Commons thread in is said in: its thread,
// or the message itself when it is top-level. A spawn's rate slot is keyed by
// it, so the slot taken before a root is promoted is the one its spawn uses.
func commonsRootID(in conversation.Inbound) string {
	if in.Thread != "" {
		return in.Thread
	}
	return in.Message
}

// toThreadAgent gives text to agent, the thread's agent (headAgent): a nudge
// labelled label when it runs, a respawn with the thread's conversation when
// it has stopped, a fresh agent for the thread when there is none. msgID is
// the message that carried the words.
func (b *Bot) toThreadAgent(ctx context.Context, at threadAt, agent, label, text, msgID, userID string, s *answerSay) {
	if agent == "" {
		ev := &slackevents.AppMentionEvent{User: userID, Channel: at.Channel, ThreadTimeStamp: at.TS, TimeStamp: at.TS, Text: text}
		b.handleThreadSpawnAt(ctx, at, ev, text)
		if !s.said() {
			s.say("Starting an agent for this thread.")
		}
		return
	}
	name := extractAgentName(agent)
	s.agent = name
	live, err := b.daemon.FindAgentBead(ctx, name)
	if err == nil && live != nil && agentLand(live) != at.Land {
		// headAgent held the thread's agent to its land; names are not unique
		// across lands, so the bead the words would reach is held to it too.
		s.say(fmt.Sprintf("%s is not this land's agent.", name))
		return
	}
	if err != nil || isAgentTerminal(live.Fields["agent_state"]) {
		b.respawnThreadAgentAt(ctx, at, name, text, userID)
		if !s.said() {
			s.say(fmt.Sprintf("Waking %s with this thread's conversation.", name))
		}
		return
	}
	words, rawModel := stripLeadingOverrideFlags(text)
	if rawModel != "" {
		s.say(fmt.Sprintf("%s is running, so it keeps its model; --model applies when an agent starts.", name))
	}
	if strings.TrimSpace(words) == "" {
		words = text
	}
	b.maybeInterruptFreeze(ctx, name)
	if err := b.nudgeAgentWords(ctx, name, label, words, msgID); err != nil {
		if errors.Is(err, ErrAgentNotNudgeable) {
			b.respawnThreadForGoneNudgeAt(ctx, at, name, words, userID)
			s.say(fmt.Sprintf("Waking %s with this thread's conversation.", name))
			return
		}
		b.logger.Warn("compose: nudge failed", "agent", name, "head", at.Head, "error", err)
		s.say(fmt.Sprintf("%s is busy; it will read this when it is free.", name))
		return
	}
	s.say(fmt.Sprintf("Told %s.", name))
}

// respawnThreadForGoneNudgeAt respawns a thread's agent whose nudge found its
// pod gone, as respawnThreadForGoneNudge does for a Slack thread.
func (b *Bot) respawnThreadForGoneNudgeAt(ctx context.Context, at threadAt, agent, text, userID string) {
	if at.slack() {
		b.respawnThreadForGoneNudge(ctx, at.Channel, at.TS, agent, text, userID)
		return
	}
	b.respawnThreadAgentAt(ctx, at, agent, text, userID)
}

// headAgent is the agent bound to the thread: the binding cache, then, for a
// Slack thread, its card agent or the daemon's agent of the Slack thread, then
// the newest agent bead whose thread_id is the head. In a Commons thread each
// is held to the thread's land (agentInLand): one of another land is passed
// over, never returned.
func (b *Bot) headAgent(ctx context.Context, at threadAt) string {
	inLand := func(agent string) bool {
		if agent == "" {
			return false
		}
		if b.agentInLand(ctx, agent, at.Land) {
			return true
		}
		b.logger.Warn("compose: the thread leads to another land's agent; not using it",
			"head", at.Head, "land", at.Land, "agent", agent, "slack_channel", at.Channel, "slack_ts", at.TS)
		return false
	}
	if agent, ok := b.threadAgentAt(at); ok && inLand(agent) {
		return agent
	}
	if at.slack() {
		if agent := b.getCardAgentByThread(at.Channel, at.TS); inLand(agent) {
			return agent
		}
		if agent := b.getAgentByThread(at.Channel, at.TS); inLand(agent) {
			return agent
		}
	}
	if b.daemon == nil || at.Head == "" {
		return ""
	}
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:  []string{"agent"},
		Fields: map[string]string{conversation.BeadThreadID: at.Head},
		Sort:   "-created_at",
		Limit:  1,
	})
	if err != nil || res == nil || len(res.Beads) == 0 || res.Beads[0] == nil {
		return ""
	}
	a := res.Beads[0]
	if at.Land == "" || agentLand(a) != at.Land {
		return ""
	}
	name := strings.TrimSpace(firstNonEmptyField(a.Fields, "agent"))
	if name == "" {
		name = strings.TrimSpace(a.Title)
	}
	if name != "" {
		b.bindThreadAgent(at, name)
	}
	return name
}

// commonsThreadVerb runs kill, stop, restart or clear on the thread's agent,
// through the cores the Slack card buttons use. The agent is the thread's
// land's (headAgent), and so is the bead the verb acts on: the cores act with
// the bridge's own credentials, so the person's land role is all that stands
// between them and another land's agent.
func (b *Bot) commonsThreadVerb(ctx context.Context, at threadAt, verb string, force bool, userID string, s *answerSay) {
	agent := b.headAgent(ctx, at)
	if agent == "" {
		s.say("No agent works in this thread.")
		return
	}
	name := extractAgentName(agent)
	s.agent = name
	if !b.agentInLand(ctx, name, at.Land) {
		s.say(fmt.Sprintf("%s is not this land's agent.", name))
		return
	}
	if verb == "clear" {
		b.clearAgent(ctx, name, "", "", s.say)
		if !s.said() {
			s.say(fmt.Sprintf("Cleared %s's card.", name))
		}
		return
	}
	bead, err := b.daemon.FindAgentBead(ctx, name)
	if err != nil || bead == nil {
		s.say(fmt.Sprintf("%s is not running.", name))
		return
	}
	if agentLand(bead) != at.Land {
		s.say(fmt.Sprintf("%s is not this land's agent.", name))
		return
	}
	switch verb {
	case "kill", "stop":
		if force {
			go func() {
				if err := b.killAgentBead(context.WithoutCancel(ctx), name, bead.ID, true); err != nil {
					b.logger.Error("compose: force kill failed", "agent", name, "error", err)
				}
			}()
			s.say(fmt.Sprintf("Killing %s now.", name))
			return
		}
		b.killThreadAgent(ctx, name, bead.ID, "", "", s.say)
	case "restart":
		if at.slack() {
			b.restartThreadAgent(ctx, name, bead.ID, at.Channel, at.TS, userID, s.say)
			return
		}
		if !b.reopenDebouncer.Allow(name) {
			s.say(fmt.Sprintf("%s is already restarting.", name))
			return
		}
		s.say(fmt.Sprintf("Restarting %s…", name))
		bg := context.WithoutCancel(ctx)
		quiet := at
		quiet.Say = nil // the person has had their answer; the rest goes to the log
		go func() {
			if err := b.killAgentBead(bg, name, bead.ID, false); err != nil {
				b.logger.Error("compose: restart kill failed", "agent", name, "error", err)
				return
			}
			b.respawnThreadAgentAt(bg, quiet, name, "Restarted via @mention command", userID)
		}()
	}
}

// commonsNudgeNamed gives the words to a named agent of the land: a nudge
// when it runs, else a respawn in its own thread, else a note that it has
// stopped.
func (b *Bot) commonsNudgeNamed(ctx context.Context, in conversation.Inbound, name, text, userID string, s *answerSay) {
	words := strings.TrimSpace(text)
	if words == "" {
		words = strings.TrimSpace(in.Text)
	}
	live, err := b.daemon.FindAgentBead(ctx, name)
	if err == nil && live != nil && !isAgentTerminal(live.Fields["agent_state"]) {
		b.maybeInterruptFreeze(ctx, name)
		if err := b.nudgeAgentWords(ctx, name, "Commons mention", words, in.Message); err == nil {
			s.say(fmt.Sprintf("Told %s.", name))
			return
		} else if !errors.Is(err, ErrAgentNotNudgeable) {
			s.say(fmt.Sprintf("%s is busy; it will read this when it is free.", name))
			return
		}
	}
	prior, perr := b.daemon.FindAgentBeadAnyStatus(ctx, name)
	headID := ""
	if perr == nil && prior != nil {
		headID = strings.TrimSpace(firstNonEmptyField(prior.Fields, conversation.BeadThreadID, "slack_thread_bead_id"))
	}
	if headID == "" {
		s.say(fmt.Sprintf("%s has stopped, and works in no thread to wake it in.", name))
		return
	}
	head, herr := b.daemon.GetBead(ctx, headID)
	if herr != nil || head == nil || head.ID == "" {
		s.say(fmt.Sprintf("%s has stopped, and its thread is gone.", name))
		return
	}
	// As commonsComposeHead: a done wish is not woken, nor a head of another land.
	if hl, ok := onlyLand(head.Labels); !ok || hl != in.Land {
		s.say(fmt.Sprintf("%s has stopped, and its thread is not this land's.", name))
		return
	}
	if isWishHeadBead(head) && head.Status == "closed" {
		s.say(fmt.Sprintf("%s has stopped, and its wish is done. Start a new thread to ask again.", name))
		return
	}
	at, ok := b.commonsThreadAt(ctx, head, in, s.say)
	if !ok {
		s.say(fmt.Sprintf("%s has stopped, and its thread's Slack copy is not this land's.", name))
		return
	}
	b.respawnThreadAgentAt(ctx, at, name, words, userID)
	if !s.said() {
		s.say(fmt.Sprintf("Waking %s in its thread.", name))
	}
}

// commonsComposeHead is the wish head a Commons mention works under: the
// thread's root when it is a head already (or was promoted to one), else the
// chat root promoted now. A top-level mention is its own root. Promoting
// takes a slot of the person's spawn rate (admitCommonsSpawn) first, so a
// refusal mints no head; a head found takes its slot in composeInThread, when
// it has no agent.
func (b *Bot) commonsComposeHead(ctx context.Context, in conversation.Inbound) (*beadsapi.BeadDetail, error) {
	rootID := commonsRootID(in)
	root, err := b.daemon.GetBead(ctx, rootID)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return nil, inboundRefuse(http.StatusNotFound, "That thread is gone.")
		}
		return nil, fmt.Errorf("read thread root %s: %w", rootID, err)
	}
	if root == nil || root.ID == "" {
		return nil, inboundRefuse(http.StatusNotFound, "That thread is gone.")
	}
	if land, ok := onlyLand(root.Labels); !ok || land != in.Land {
		return nil, inboundRefuse(http.StatusUnprocessableEntity, "That thread belongs to no single land.")
	}
	if isWishHeadBead(root) {
		if root.Status == "closed" {
			return nil, inboundRefuse(http.StatusConflict, "This wish is done. Start a new thread to ask again.")
		}
		return root, nil
	}
	if root.Type != "message" {
		return nil, inboundRefuse(http.StatusUnprocessableEntity, "That is not a thread.")
	}
	if head := b.promotedHead(ctx, root, in.Land); head != nil {
		return head, nil
	}
	if err := b.admitCommonsSpawn(ctx, in, root.ID); err != nil {
		return nil, err
	}
	return b.promoteChatRoot(ctx, root, in)
}

// isWishHeadBead reports whether b is a wish head: a work bead labelled
// thread (C3).
func isWishHeadBead(b *beadsapi.BeadDetail) bool {
	return b.Type != "message" && b.Type != "channel" && hasLabel(b.Labels, threadKindLabel)
}

// promotedHead is the head a chat root of land was promoted to already, by
// its promoted_to, else by a head's promoted_from. Like the Commons'
// promotedHeadOf (commons/model.go), a head counts only in the root's land and
// only when it names the root back: promoted_to is a field the root's writer
// may set, so it alone would let a land-A message point at a land-B wish and
// drive that land's agent with land A's role.
func (b *Bot) promotedHead(ctx context.Context, root *beadsapi.BeadDetail, land string) *beadsapi.BeadDetail {
	if land == "" {
		return nil
	}
	if to := strings.TrimSpace(root.Fields[fieldPromotedTo]); to != "" {
		if head, err := b.daemon.GetBead(ctx, to); err == nil && head != nil && isPromotedHeadOf(head, root, land) {
			return head
		}
	}
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{"project:" + land, threadKindLabel},
		Fields: map[string]string{fieldPromotedFrom: root.ID},
		Limit:  1,
	})
	if err == nil && res != nil && len(res.Beads) > 0 && res.Beads[0] != nil && isPromotedHeadOf(res.Beads[0], root, land) {
		return res.Beads[0]
	}
	return nil
}

// isPromotedHeadOf reports whether head is root's wish head in land: a wish
// head of that one land that names root as its promoted_from.
func isPromotedHeadOf(head, root *beadsapi.BeadDetail, land string) bool {
	if !isWishHeadBead(head) || strings.TrimSpace(head.Fields[fieldPromotedFrom]) != root.ID {
		return false
	}
	hl, ok := onlyLand(head.Labels)
	return ok && hl == land
}

// promoteChatRoot makes a chat root a wish (section 2.3): a head with the
// root's words, in its channel and land, that names the root
// (promoted_from). A root with a Slack copy (mirrored either way) makes the
// head that Slack thread's head too, so a mention there finds the same one.
// The root is stamped promoted_to, best-effort: only its writer may change a
// person's message's words, and promoted_to is not one of them, but a stamp
// that fails leaves the head findable by promoted_from.
func (b *Bot) promoteChatRoot(ctx context.Context, root *beadsapi.BeadDetail, in conversation.Inbound) (*beadsapi.BeadDetail, error) {
	text := strings.TrimSpace(firstNonEmptyField(root.Fields, conversation.BeadText))
	if text == "" {
		text = strings.TrimSpace(root.Title)
	}
	land := in.Land
	labels := []string{"source:commons", threadKindLabel, "project:" + land}
	fields := map[string]string{
		"spawn_source":             commonsSpawnSource,
		fieldPromotedFrom:          root.ID,
		"operated_by":              in.Actor.ID,
		conversation.BeadChannelID: firstNonEmptyField(root.Fields, conversation.BeadChannelID),
	}
	if text != "" {
		fields["slack_root_text"] = truncateText(text, 4000)
	}
	idem := "commons-promote:" + root.ID
	if c, ts := root.Fields["slack_channel"], root.Fields["slack_ts"]; isValidThreadBinding(c, ts) {
		if !b.slackChannelOfLand(ctx, c, land) {
			// The root's Slack fields are its writer's: a copy outside the
			// land's channels is no copy, and a head bound to it would steer
			// this land's spawns into that Slack thread.
			b.logger.Warn("compose: a chat root names a Slack thread outside its land's channels",
				"root", root.ID, "land", land, "slack_channel", c, "slack_ts", ts)
			return nil, inboundRefuse(http.StatusUnprocessableEntity, "%s", errSlackCopyNotOfLand)
		}
		label := threadBeadLabel(c, ts)
		if res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{Labels: []string{label}, Limit: 1}); err == nil && res != nil && len(res.Beads) > 0 && res.Beads[0] != nil {
			// The Slack thread has a head already (a mention there came first):
			// the Commons thread is that wish, when it is this land's. The
			// root's Slack fields are its writer's, so a head of another land
			// (or of none) is refused, not adopted.
			head := res.Beads[0]
			if hl, ok := onlyLand(head.Labels); !ok || hl != land {
				b.logger.Warn("compose: a chat root names a Slack thread whose wish is another land's",
					"root", root.ID, "head", head.ID, "land", land)
				return nil, inboundRefuse(http.StatusUnprocessableEntity, "That thread's wish belongs to another land.")
			}
			b.stampPromotion(ctx, root, head)
			return head, nil
		}
		labels = append(labels, label)
		fields["slack_thread_channel"], fields["slack_thread_ts"] = c, ts
		idem = label
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("promote chat root %s: %w", root.ID, err)
	}
	title := "Commons thread"
	if text != "" {
		title = truncateText("Commons thread: "+text, 120)
	}
	id, err := b.daemon.CreateBead(wishHeadCtx(ctx), beadsapi.CreateBeadRequest{
		Title:          title,
		Type:           "task",
		Kind:           "issue",
		Priority:       threadBeadPriority,
		Assignee:       threadBeadAssignee,
		Labels:         labels,
		Fields:         json.RawMessage(raw),
		IdempotencyKey: idem,
	})
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusForbidden || apiErr.StatusCode == http.StatusConflict) {
			return nil, inboundRefuse(apiErr.StatusCode, "The beads daemon refused that: %s", strings.TrimSpace(apiErr.Message))
		}
		return nil, fmt.Errorf("promote chat root %s: %w", root.ID, err)
	}
	head, err := b.daemon.GetBead(ctx, id)
	if err != nil || head == nil {
		head = &beadsapi.BeadDetail{ID: id, Type: "task", Status: "open", Labels: labels, Fields: fields}
	}
	if !slices.Contains(head.Labels, threadKindLabel) {
		// The create returned a bead that already held the key and is no
		// wish head, so the bridge did not write it: refuse it rather than
		// stamp it (gd-3HAfTiByJK; kbeads reserves the key from tenants).
		b.logger.Warn("compose: a chat root's head key is held by a bead that is not a wish head",
			"root", root.ID, "bead", head.ID, "land", land, "key", idem)
		return nil, inboundRefuse(http.StatusConflict, "That thread's wish is held by a bead that is not a wish.")
	}
	b.stampPromotion(ctx, root, head)
	b.logger.Info("compose: promoted a chat root to a wish", "root", root.ID, "head", id, "land", land)
	return head, nil
}

// stampPromotion writes promoted_to on the root and promoted_from on the head
// where each is unset. Best-effort.
func (b *Bot) stampPromotion(ctx context.Context, root, head *beadsapi.BeadDetail) {
	if root.Fields[fieldPromotedTo] == "" {
		if err := b.daemon.UpdateBeadFields(wishHeadCtx(ctx), root.ID, map[string]string{fieldPromotedTo: head.ID}); err != nil {
			b.logger.Info("compose: stamping promoted_to on the chat root failed; the head names it",
				"root", root.ID, "head", head.ID, "error", err)
		}
	}
	if head.Fields[fieldPromotedFrom] == "" {
		if err := b.daemon.UpdateBeadFields(wishHeadCtx(ctx), head.ID, map[string]string{fieldPromotedFrom: root.ID}); err != nil {
			b.logger.Warn("compose: stamping promoted_from on the head failed", "root", root.ID, "head", head.ID, "error", err)
		}
	}
}
