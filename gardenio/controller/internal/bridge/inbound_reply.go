package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"

	"github.com/slack-go/slack/slackevents"
)

// Slice 4c of the Commons migration (design spec
// 2026-10-19-lands-commons-design.md, sections 3.4 and 5.2): a human's reply
// in a thread, from Slack or from the Commons, becomes one
// conversation.Inbound{Reply}, and one dispatcher runs HandleReply on it.
//
//   - Slack: handleMessageEvent is the adapter. It keeps the subtype filter
//     (isUtteranceSubtype) and the bot-echo guard in front of the dispatcher,
//     and hands a plain thread reply over as slackReplyInbound. Its Message is
//     empty: HandleReply writes the conversation bead where the thread forward
//     always did, since writing one for every threaded reply would mint a wish
//     head for every ordinary channel thread.
//   - Commons: the Commons (and today's /threads composer) writes the human's
//     message bead itself and then calls POST /api/conversation/inbound
//     (api_conversation.go). ThreadMessageForward dispatches a composer bead
//     too once it has projected it into Slack, and a sweep picks up any
//     origin=commons message the inbound call never delivered. Each bead is
//     dispatched once: an in-process claim plus the bead's handled_at field.
//
// The dispatcher is the panic boundary for the handlers under it, counted on
// gardenio_bridge_handler_panic_total{handler="inbound_<kind>"}.

// Commons message bead field set once the bridge has dispatched it.
const commonsHandledAt = "handled_at"

const (
	// commonsSweepInterval is how often the safety-net sweep looks for
	// undelivered Commons messages.
	commonsSweepInterval = 2 * time.Minute
	// commonsSweepGrace leaves a fresh message to its own inbound call.
	commonsSweepGrace = time.Minute
	// commonsSweepWindow bounds how far back the sweep looks; an older reply
	// is stale enough that nudging an agent with it would surprise.
	commonsSweepWindow = 24 * time.Hour
	// inboundClaimTTL is how long a dispatched message id is remembered in
	// process, covering the race between the inbound call and the SSE close
	// event before handled_at lands.
	inboundClaimTTL = time.Hour
)

// inbound returns the Bot's Inbound dispatcher, built on first use.
func (b *Bot) inbound() *conversation.Dispatcher {
	b.conv.mu.Lock()
	defer b.conv.mu.Unlock()
	if b.conv.dispatcher == nil {
		d := conversation.NewDispatcher(func(in conversation.Inbound, v any, stack []byte) {
			handlerPanicTotal.WithLabelValues("inbound_" + string(in.Kind)).Inc()
			b.logger.Error("recovered panic in inbound handler",
				"kind", in.Kind, "surface", in.Surface, "message", in.Message,
				"panic", v, "stack", string(stack))
		})
		d.Handle(conversation.InboundCompose, b.HandleCompose)
		d.Handle(conversation.InboundReply, b.HandleReply)
		d.Handle(conversation.InboundAction, b.HandleAction)
		d.Handle(conversation.InboundSubmit, b.HandleSubmit)
		d.Handle(conversation.InboundReact, b.HandleReact)
		b.conv.dispatcher = d
	}
	return b.conv.dispatcher
}

// slackReplyInbound normalises a Slack thread reply. operator is the Slack
// user resolved through the account link (operatorFromSlackUser).
func slackReplyInbound(ev *slackevents.MessageEvent, operator string) conversation.Inbound {
	return conversation.Inbound{
		Kind:    conversation.InboundReply,
		Surface: conversation.SurfaceSlack,
		Actor:   conversation.Principal{ID: operator, SlackUser: ev.User},
		Text:    ev.Text,
		At:      conversation.Ref{Surface: conversation.SurfaceSlack, Channel: ev.Channel, TS: ev.TimeStamp},
		Roots:   []conversation.Ref{{Surface: conversation.SurfaceSlack, Channel: ev.Channel, TS: ev.ThreadTimeStamp}},
		Trigger: conversation.Trigger{Surface: conversation.SurfaceSlack, Channel: ev.Channel, ThreadTS: ev.ThreadTimeStamp},
	}
}

// dispatchInbound runs a Slack Inbound and sends its answer back to Slack.
func (b *Bot) dispatchInbound(ctx context.Context, in conversation.Inbound) {
	a, err := b.inbound().Dispatch(ctx, in)
	if err != nil {
		b.logger.Warn("inbound: dispatch failed",
			"kind", in.Kind, "channel", in.Trigger.Channel, "thread_ts", in.Trigger.ThreadTS, "error", err)
	}
	b.answerSlack(ctx, in, a)
}

// answerSlack sends a handler's ephemerals to the Slack user who acted.
func (b *Bot) answerSlack(ctx context.Context, in conversation.Inbound, a conversation.Answer) {
	for _, m := range a.Ephemeral {
		if err := b.conversationSlack().Ephemeral(ctx, in.Trigger, in.Actor, m); err != nil {
			b.logger.Warn("inbound: ephemeral answer failed",
				"channel", in.Trigger.Channel, "user", in.Actor.SlackUser, "error", err)
		}
	}
}

// HandleReply is the reply core. A Slack reply resolves the decision whose
// thread it is in, then goes to the agent bound to the thread: forwarded
// (conversation bead + nudge, or a resume hint when the agent has exited) to a
// thread-spawned agent, a mention hint in an agent card thread, or recorded
// alone in an orphan wish thread. A Commons reply already has its bead, and
// gets the outcome a mention would: a live agent is nudged, a gone one is
// respawned with session resume, an agent-less thread gets a fresh one.
func (b *Bot) HandleReply(ctx context.Context, in conversation.Inbound) (conversation.Answer, error) {
	if in.Surface == conversation.SurfaceCommons {
		return conversation.Answer{}, b.handleCommonsReply(ctx, in)
	}
	answer := b.resolveDecisionByReply(ctx, in)
	root, ok := in.Root(conversation.SurfaceSlack)
	if !ok {
		return answer, conversation.ErrNoThread
	}
	// Forward to the bound agent on every non-mention reply when the thread
	// was spawned by an @mention. Previously this was gated on --listen and
	// otherwise fell through to hintMentionRequired, which was throttled to
	// once per 10 minutes per thread — after the first hint every subsequent
	// reply was silently dropped (ka-0000006280). The user already opted into
	// the conversation by spawning the agent in this thread; expecting them to
	// re-@mention every turn is a footgun. Nudge volume is bounded by the 30s
	// threadNudgeInterval throttle inside handleThreadForward.
	//
	// Agent-card threads (getAgentByThread fallback) keep the
	// hintMentionRequired behavior — those are bot-initiated card threads, not
	// threads the user explicitly opened by @mentioning.
	if agent := b.getThreadSpawnedAgent(root.Channel, root.TS); agent != "" {
		b.handleThreadForward(ctx, in, agent)
	} else if agent := b.getAgentByThread(root.Channel, root.TS); agent != "" {
		b.hintMentionRequired(ctx, root.Channel, root.TS, agent)
	} else if headID := b.findThreadHeadByThreadLabel(ctx, root.Channel, root.TS); headID != "" {
		// Orphan wish thread: a thread (wish) HEAD exists for this (channel,
		// thread_ts) — so it renders in the beads-viewer /threads index — but
		// NO agent is bound anymore (state binding lost + card gone + the
		// daemon-metadata resolver skipped a done/failed agent, or the agent
		// bead was pruned). Without this branch the reply was silently dropped
		// from /threads (ka-0000001743). Record the human's words as a
		// conversation bubble bound to the existing head so the turn is
		// preserved. Gated on head existence so ordinary channel threads (no
		// wish head) are untouched — recordThreadConversationMessage would
		// otherwise mint a head for every random threaded reply. No agent to
		// nudge; the wish's own reopen/close gesture governs lifecycle.
		b.recordThreadConversationMessage(ctx, in, "", "")
	}
	return answer, nil
}

// handleCommonsReply gives a Commons reply its agent outcome. In a thread with
// a Slack copy (from the message itself, else from its head) that is
// SpawnOrRespawnForViewerReply's when the reply may spawn (in.Spawns: the
// inbound call checked the sender's land role); otherwise, as in a Commons
// thread with no Slack copy, a live agent is nudged with the words and a
// stopped one, or none, is left be, since waking it is a spawn. In a Commons
// thread with no Slack copy the binding is the head's (slice 4d). A message in
// a DM channel, a reply or not, goes to the agents in the DM (dm_replies.go).
func (b *Bot) handleCommonsReply(ctx context.Context, in conversation.Inbound) error {
	if dm, ok := b.commonsDM(ctx, in.Channel); ok {
		return b.handleCommonsDM(ctx, in, dm)
	}
	// A reply to the herder's "fulfilled" note contests it (gd-EfbganTatT).
	b.reopenHerderClosedWish(ctx, b.commonsReplyHead(ctx, in.Thread, in.Land))
	root, ok := in.Root(conversation.SurfaceSlack)
	if ok && !b.slackThreadInLand(ctx, root, in.Thread, in.Land) {
		b.logger.Warn("commons reply: the message names a Slack thread of another land; not following it",
			"message", in.Message, "land", in.Land, "slack_channel", root.Channel, "slack_ts", root.TS)
		ok = false
	}
	if !ok {
		root, ok = b.slackRootOfHead(ctx, in.Thread, in.Land)
	}
	if ok && !in.Spawns {
		b.commonsReplyToSlackThread(ctx, root, in)
		return nil
	}
	if ok {
		if agent := b.getAgentByThread(root.Channel, root.TS); agent != "" && !b.agentInLand(ctx, agent, in.Land) {
			b.logger.Warn("commons reply: the Slack thread's agent is another land's; not following it",
				"message", in.Message, "land", in.Land, "slack_channel", root.Channel, "slack_ts", root.TS, "agent", agent)
			return nil
		}
		// SpawnOrRespawnForViewerReply spawns and respawns with the words' flags.
		if msg := b.commonsSpawnRefusal(ctx, in.Land, in.Text); msg != "" {
			return inboundRefuse(http.StatusUnprocessableEntity, "%s", msg)
		}
		// With no agent bound it starts a fresh one, which takes a slot of
		// the person's spawn rate as a mention's does (gd-PTOUU1ufzq).
		if b.getAgentByThread(root.Channel, root.TS) == "" {
			if err := b.admitCommonsSpawn(ctx, in, commonsRootID(in)); err != nil {
				return err
			}
		}
		b.SpawnOrRespawnForViewerReply(ctx, root.Channel, root.TS, in.Text, in.Message,
			strings.TrimPrefix(in.Actor.ID, "oidc:"))
		return nil
	}
	head := b.commonsReplyHead(ctx, in.Thread, in.Land)
	if head == "" {
		return fmt.Errorf("commons reply %s: thread %q is no wish: %w", in.Message, in.Thread, conversation.ErrNoThread)
	}
	agent := b.headAgent(ctx, threadAt{Head: head, Land: in.Land})
	if agent == "" {
		return nil // a wish no agent works yet: the words are in the thread
	}
	name := extractAgentName(agent)
	if !b.agentLive(ctx, name, in.Land) {
		b.logger.Info("commons reply: the thread's agent has stopped; a mention wakes it",
			"head", head, "agent", name)
		return nil
	}
	if b.state != nil {
		_ = b.state.TouchThreadAgentByHead(head)
	}
	if err := b.nudgeAgentWords(ctx, name, "Commons reply", in.Text, in.Message); err != nil {
		b.logger.Warn("commons reply: nudge failed", "head", head, "agent", name, "error", err)
	}
	return nil
}

// commonsReplyToSlackThread nudges the live agent of the Slack thread root
// with a Commons reply that may not spawn: one the watchers or the sweep
// dispatched, which have no caller whose land role could let it, or a
// guest's (landrole.go denies them "spawn"). No agent, or a stopped one, is
// left be, and a failed nudge is not followed by a respawn: the words are in
// the thread either way, and a mention wakes the agent. An agent of another
// land than the reply's is left be too (agentInLand): the Slack thread is
// named by fields the root's or the reply's writer sets.
func (b *Bot) commonsReplyToSlackThread(ctx context.Context, root conversation.Ref, in conversation.Inbound) {
	agent := b.getAgentByThread(root.Channel, root.TS)
	if agent == "" {
		return
	}
	if !b.agentInLand(ctx, agent, in.Land) {
		b.logger.Warn("commons reply: the Slack thread's agent is another land's; not nudging it",
			"message", in.Message, "land", in.Land, "slack_channel", root.Channel, "slack_ts", root.TS, "agent", agent)
		return
	}
	name := extractAgentName(agent)
	if !b.agentLive(ctx, name, in.Land) {
		b.logger.Info("commons reply: the Slack thread's agent has stopped; a mention wakes it",
			"slack_channel", root.Channel, "slack_ts", root.TS, "agent", name)
		return
	}
	if err := b.nudgeAgentWords(ctx, name, "Commons reply", in.Text, in.Message); err != nil {
		b.logger.Warn("commons reply: nudge failed",
			"slack_channel", root.Channel, "slack_ts", root.TS, "agent", name, "error", err)
	}
}

// agentLive reports whether the agent named name has a bead of land that is
// not terminal. Names are not unique across lands, so the live bead a nudge
// reaches is held to the land itself.
func (b *Bot) agentLive(ctx context.Context, name, land string) bool {
	live, err := b.daemon.FindAgentBead(ctx, name)
	return err == nil && live != nil && land != "" && agentLand(live) == land &&
		!isAgentTerminal(live.Fields["agent_state"])
}

// commonsReplyHead is the wish head of a Commons reply's thread in land: the
// root itself when it is a head of land, else the head it was promoted to; ""
// for a chat root that is no wish, and for a thread of another land. A reply's
// thread_id is its writer's to set, and this path checks no land role, so a
// head of another land would let a guest of one land talk to another land's
// agent.
func (b *Bot) commonsReplyHead(ctx context.Context, rootID, land string) string {
	if rootID == "" || land == "" || b.daemon == nil {
		return ""
	}
	root, err := b.daemon.GetBead(ctx, rootID)
	if err != nil || root == nil || root.ID == "" {
		return ""
	}
	if rl, ok := onlyLand(root.Labels); !ok || rl != land {
		return ""
	}
	if isWishHeadBead(root) {
		return root.ID
	}
	if root.Type == "message" {
		if head := b.promotedHead(ctx, root, land); head != nil {
			return head.ID
		}
	}
	return ""
}

// slackRootOfHead returns the Slack thread a wish head of land is bound to;
// false for a head of another land, for the reason commonsReplyHead gives,
// for a Slack thread outside the land's channels (the head's
// slack_thread_channel/ts are its writer's: slackChannelOfLand), and for a
// message of no land.
func (b *Bot) slackRootOfHead(ctx context.Context, headID, land string) (conversation.Ref, bool) {
	if headID == "" || land == "" || b.daemon == nil {
		return conversation.Ref{}, false
	}
	head, err := b.daemon.GetBead(ctx, headID)
	if err != nil || head == nil {
		return conversation.Ref{}, false
	}
	if hl, ok := onlyLand(head.Labels); !ok || hl != land {
		return conversation.Ref{}, false
	}
	root, ok := slackThreadRoot(head.Fields)
	if !ok || !b.slackChannelOfLand(ctx, root.Channel, land) {
		return conversation.Ref{}, false
	}
	return root, true
}

// slackThreadInLand reports whether a Commons reply's own Slack thread
// (its slack_thread_channel/ts, which its writer sets) is a thread of land:
// it is in a Slack channel a channel bead of land binds (slackChannelOfLand:
// every field naming it, and a head's slack-thread label, is a writer's), and
// the Slack thread's wish head is of land, or, with no head, the reply's
// Commons root (of land) is that Slack thread's copy. A message of no land
// (the /threads composer's thread-say, which predates lands) is held to
// none, so it follows no Slack thread: it is relayed to Slack, and reaches no
// agent from here.
func (b *Bot) slackThreadInLand(ctx context.Context, root conversation.Ref, threadID, land string) bool {
	if land == "" || b.daemon == nil || !b.slackChannelOfLand(ctx, root.Channel, land) {
		return false
	}
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{threadBeadLabel(root.Channel, root.TS)},
		Limit:  1,
	})
	if err == nil && res != nil && len(res.Beads) > 0 && res.Beads[0] != nil {
		hl, ok := onlyLand(res.Beads[0].Labels)
		return ok && hl == land
	}
	if threadID == "" {
		return false
	}
	tr, err := b.daemon.GetBead(ctx, threadID)
	if err != nil || tr == nil {
		return false
	}
	if tl, ok := onlyLand(tr.Labels); !ok || tl != land {
		return false
	}
	if tr.Fields["slack_channel"] == root.Channel && tr.Fields["slack_ts"] == root.TS {
		return true
	}
	r, ok := slackThreadRoot(tr.Fields)
	return ok && r.Channel == root.Channel && r.TS == root.TS
}

// slackThreadRoot reads a bead's slack_thread_channel/slack_thread_ts binding.
func slackThreadRoot(fields map[string]string) (conversation.Ref, bool) {
	channel, ts := fields["slack_thread_channel"], fields["slack_thread_ts"]
	if !isValidThreadBinding(channel, ts) {
		return conversation.Ref{}, false
	}
	return conversation.Ref{Surface: conversation.SurfaceSlack, Channel: channel, TS: ts}, true
}

// commonsMessage is a message bead the Commons (or the /threads composer)
// wrote, read from an SSE event or the daemon.
type commonsMessage struct {
	ID        string
	Type      string
	Title     string
	CreatedBy string
	CreatedAt time.Time
	Labels    []string
	Fields    map[string]string
}

func commonsMessageFromEvent(ev *BeadEvent) commonsMessage {
	return commonsMessage{ID: ev.ID, Type: ev.Type, Title: ev.Title, CreatedBy: ev.CreatedBy, Labels: ev.Labels, Fields: ev.Fields}
}

func commonsMessageFromDetail(d *beadsapi.BeadDetail) commonsMessage {
	return commonsMessage{ID: d.ID, Type: d.Type, Title: d.Title, CreatedBy: d.CreatedBy, CreatedAt: d.CreatedAt, Labels: d.Labels, Fields: d.Fields}
}

// fromCommons reports whether m is a human's words written by the Commons:
// a message with origin=commons, or a /threads composer thread-say message.
func (m commonsMessage) fromCommons() bool {
	return m.Type == "message" &&
		(m.Fields[conversation.BeadOrigin] == conversation.SurfaceCommons || hasLabel(m.Labels, ThreadSayLabel))
}

// author is the principal who wrote m: its author field, else its creator.
func (m commonsMessage) author() string {
	who := m.Fields[conversation.BeadAuthor]
	if who == "" {
		who = m.CreatedBy
	}
	return asPrincipal(who)
}

// commonsRepliable reports whether m has someone to be a reply to: it is in a
// thread, or said in a DM, whose agents hear every message.
func (b *Bot) commonsRepliable(ctx context.Context, m commonsMessage) bool {
	if m.Fields[conversation.BeadThreadID] != "" {
		return true
	}
	_, dm := b.commonsDM(ctx, m.Fields[conversation.BeadChannelID])
	return dm
}

// asPrincipal reads a bare email as the oidc:<email> principal it names.
func asPrincipal(who string) string {
	if who == "" || strings.Contains(who, ":") || !strings.Contains(who, "@") {
		return who
	}
	return "oidc:" + who
}

// errCommonsNotHuman refuses a Commons message no human is shown to have
// written (gd-vnVsKJo4zw).
var errCommonsNotHuman = errors.New("commons message was not written by its author")

// writtenByAuthor reports whether a human wrote m as its author: its creator
// is an OIDC principal, and its author field, when set, names that creator.
// created_by is stamped by kbeads from the writer's credential, and since
// #670 a static-token caller cannot take an email or oidc: subject through
// X-Actor, so only a person's own bearer yields one. The author field is free
// text any writer may set. This is the check POST /api/conversation/inbound
// makes against its caller, made against the writer for the paths that have
// no caller: the composer forward and the sweep.
func (m commonsMessage) writtenByAuthor() bool {
	creator := asPrincipal(m.CreatedBy)
	email, ok := strings.CutPrefix(creator, "oidc:")
	if !ok || !strings.Contains(email, "@") {
		return false
	}
	return strings.EqualFold(m.author(), creator)
}

// inbound normalises m into an Inbound{Reply}, whatever it names: only the
// inbound call's compose, which alone carries the caller's land role, makes a
// mention of it (dispatchCommonsCompose). It spawns only when it is the
// /threads composer's (a thread-say of no origin, which keeps
// ka-0000005407's spawn); a Commons reply has leave to spawn from the inbound
// call alone (dispatchCommonsAs). Its files ride in the text as
// links, one per line, so the agent it reaches can open them, and a Commons
// attachment with the gb command that fetches its bytes.
func (m commonsMessage) inbound() conversation.Inbound {
	text := m.Fields[conversation.BeadText]
	if text == "" {
		text = m.Title
	}
	// An agent reads a message's files, and a farm snapshot's view, as links
	// under the words.
	if files := withSnapLink(decodeFileRefs(m.Fields[conversation.BeadFiles]), m.Fields[conversation.BeadSnap]); len(files) > 0 {
		text = strings.TrimSpace(text + "\n" + fileLinks(files))
		if hint := fetchHint(m.ID, files); hint != "" {
			text += "\n" + hint
		}
	}
	in := conversation.Inbound{
		Kind:    conversation.InboundReply,
		Surface: conversation.SurfaceCommons,
		Land:    projectFromLabels(m.Labels),
		Channel: m.Fields[conversation.BeadChannelID],
		Thread:  m.Fields[conversation.BeadThreadID],
		Message: m.ID,
		Actor:   conversation.Principal{ID: m.author()},
		Text:    text,
		Trigger: conversation.Trigger{Surface: conversation.SurfaceCommons},
	}
	in.Spawns = m.Fields[conversation.BeadOrigin] != conversation.SurfaceCommons
	if root, ok := slackThreadRoot(m.Fields); ok {
		in.Roots = []conversation.Ref{root}
	}
	return in
}

// dispatchCommonsMessage dispatches m once, as a reply, and stamps handled_at
// on it. It reports false, with no error, when m was already dispatched (by
// the inbound call, the composer forward or the sweep, whichever came first).
// A failed dispatch is stamped too: it fails the same way every time, and the
// sweep must not retry it forever. A message its author did not write is
// refused with errCommonsNotHuman and stamped, and reaches no agent:
// dispatching it would act for the person it names. The reply may not spawn:
// it only reaches a live agent (handleCommonsReply).
func (b *Bot) dispatchCommonsMessage(ctx context.Context, m commonsMessage) (conversation.Answer, bool, error) {
	return b.dispatchCommonsAs(ctx, m, conversation.InboundReply, false)
}

// dispatchCommonsCompose dispatches m once as a mention. Only the inbound
// call's compose may, once it has checked the caller's land role.
func (b *Bot) dispatchCommonsCompose(ctx context.Context, m commonsMessage) (conversation.Answer, bool, error) {
	return b.dispatchCommonsAs(ctx, m, conversation.InboundCompose, true)
}

// dispatchCommonsAs dispatches m once as kind. spawns gives it leave to start
// or wake an agent (Inbound.Spawns), which only the inbound call may, once it
// has checked the caller's land role.
func (b *Bot) dispatchCommonsAs(ctx context.Context, m commonsMessage, kind conversation.InboundKind, spawns bool) (conversation.Answer, bool, error) {
	if m.Fields[commonsHandledAt] != "" || !b.claimInbound(m.ID) {
		return conversation.Answer{}, false, nil
	}
	var a conversation.Answer
	var err error
	if m.writtenByAuthor() {
		in := m.inbound()
		in.Kind = kind
		in.Spawns = in.Spawns || spawns
		a, err = b.inbound().Dispatch(ctx, in)
		if err != nil {
			b.logger.Warn("inbound: commons message dispatch failed", "message", m.ID, "error", err)
		}
	} else {
		err = errCommonsNotHuman
		b.logger.Warn("inbound: refusing a commons message its author did not write",
			"message", m.ID, "created_by", m.CreatedBy, "author", m.author())
	}
	if b.daemon != nil {
		stamp := map[string]string{commonsHandledAt: time.Now().UTC().Format(time.RFC3339)}
		if serr := b.daemon.UpdateBeadFields(ctx, m.ID, stamp); serr != nil {
			// Without handled_at only the claim stops the sweep dispatching
			// it again, so hold it for as long as the sweep looks back
			// (gd-2IEp8qx7bq).
			b.holdInbound(m.ID, commonsSweepWindow)
			b.logger.Warn("inbound: stamping handled_at failed", "message", m.ID, "error", serr)
		}
	}
	return a, true, err
}

// claimInbound reserves a Commons message id for one dispatch in this process.
func (b *Bot) claimInbound(id string) bool {
	b.conv.mu.Lock()
	defer b.conv.mu.Unlock()
	now := time.Now()
	if until, ok := b.conv.claims[id]; ok && now.Before(until) {
		return false
	}
	if b.conv.claims == nil {
		b.conv.claims = map[string]time.Time{}
	}
	if len(b.conv.claims) >= 1024 {
		for k, until := range b.conv.claims {
			if !now.Before(until) {
				delete(b.conv.claims, k)
			}
		}
	}
	b.conv.claims[id] = now.Add(inboundClaimTTL)
	return true
}

// holdInbound keeps a claimed Commons message id claimed for d from now.
func (b *Bot) holdInbound(id string, d time.Duration) {
	b.conv.mu.Lock()
	defer b.conv.mu.Unlock()
	if b.conv.claims == nil {
		b.conv.claims = map[string]time.Time{}
	}
	b.conv.claims[id] = time.Now().Add(d)
}

// startCommonsInboundSweep runs sweepCommonsInbound every commonsSweepInterval
// until ctx is done.
func (b *Bot) startCommonsInboundSweep(ctx context.Context) {
	if b.daemon == nil {
		return
	}
	ticker := time.NewTicker(commonsSweepInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			b.sweepCommonsInbound(ctx, time.Now())
		}
	}
}

// sweepCommonsInbound dispatches origin=commons message beads written in the
// last commonsSweepWindow that were never handled: the safety net for an
// inbound call lost while the bridge was down, as the thread-reply DLQ is for
// Slack replies. A mention the inbound call never ran goes as a reply too:
// the sweep has no caller whose land role could let it spawn. It returns how
// many it dispatched.
func (b *Bot) sweepCommonsInbound(ctx context.Context, now time.Time) int {
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:        []string{"message"},
		Statuses:     []string{"closed"},
		Fields:       map[string]string{conversation.BeadOrigin: conversation.SurfaceCommons},
		CreatedAfter: now.Add(-commonsSweepWindow),
		Limit:        200,
	})
	if err != nil || res == nil {
		if err != nil {
			b.logger.Warn("inbound: commons sweep list failed", "error", err)
		}
		return 0
	}
	n := 0
	dms := map[string]bool{}
	inDM := func(channel string) bool {
		is, ok := dms[channel]
		if !ok {
			_, is = b.commonsDM(ctx, channel)
			dms[channel] = is
		}
		return is
	}
	for _, d := range res.Beads {
		// A top-level message is said in a channel, not to an agent: only a
		// reply has a thread to hand to (CommonsMirror posts it to Slack).
		// A DM is said to the agents in it, reply or not.
		if d == nil || d.Fields[commonsHandledAt] != "" ||
			(d.Fields[conversation.BeadThreadID] == "" && !inDM(d.Fields[conversation.BeadChannelID])) {
			continue
		}
		if !d.CreatedAt.IsZero() && now.Sub(d.CreatedAt) < commonsSweepGrace {
			continue
		}
		if _, dispatched, err := b.dispatchCommonsMessage(ctx, commonsMessageFromDetail(d)); dispatched && !errors.Is(err, errCommonsNotHuman) {
			b.logger.Info("inbound: sweep dispatched an undelivered commons message", "message", d.ID)
			n++
		}
	}
	return n
}
