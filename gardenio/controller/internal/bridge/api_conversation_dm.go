package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
)

// gb say --to and --ask (design spec 2026-10-19-lands-commons-design.md,
// sections 2.4, 4.1 and 7.2 step 8). A say to a person or an agent is a
// message bead in the DM channel between the speaking agent and them, in the
// agent's land, made on first use (dm_channels.go). The bead has kind=text, or
// kind=ask for --ask, and `to` naming the target.
//
// Slack stays the primary delivery (owner Q8b gd-t4lzcEOEFT): a person with a
// Slack account gets the Slack IM gb dm send / gb dm ask always sent, with the
// same face (buildDMText, buildQuestionDMText), and its ref is stamped on the
// bead. An agent is nudged. The reply to an ask is captured from either side
// (dm_replies.go): a reply in the Slack IM's thread, or a message in the
// Commons DM channel.

// dmPeer is who a say --to goes to: an agent, or a person.
type dmPeer struct {
	agent  string // the agent's name; "" for a person
	person dmPerson
}

func (p dmPeer) principal() string {
	if p.agent != "" {
		return "agent:" + p.agent
	}
	return p.person.principal()
}

func (p dmPeer) name() string {
	if p.agent != "" {
		return p.agent
	}
	return p.person.Name
}

// sayTo serves a say with a `to`: a DM from the speaking agent sp.
func (a *ConversationAgentAPI) sayTo(ctx context.Context, w http.ResponseWriter, req sayRequest, files []conversation.File, sp speaker) {
	switch {
	case req.Thread != "" || req.Channel != "" || req.ThreadTS != "":
		writeInboundError(w, http.StatusBadRequest, "to names its own channel (the DM); drop thread and channel")
		return
	case len(files) > 0:
		writeInboundError(w, http.StatusBadRequest, "a DM carries no files yet; share them with gb say --file in a thread or channel")
		return
	case strings.TrimSpace(req.Text) == "":
		writeInboundError(w, http.StatusBadRequest, "text is required")
		return
	case a.bot.daemon == nil:
		writeInboundError(w, http.StatusServiceUnavailable, "bridge has no daemon to hold the DM")
		return
	case sp.land == "":
		writeInboundError(w, http.StatusUnprocessableEntity, fmt.Sprintf("agent %s has no land; a DM lives in one", sp.agent))
		return
	}
	peer, err := a.dmPeer(ctx, req.To, sp)
	if err != nil {
		writeSayError(w, err)
		return
	}
	dm, err := conversation.EnsureDM(ctx, a.bot.daemon, sp.land, dmChannelTitle(sp.agent, peer.name()), "agent:"+sp.agent, peer.principal())
	if err != nil {
		a.logger.Error("say --to: DM channel not found or made", "agent", sp.agent, "to", peer.principal(), "error", err)
		writeInboundError(w, http.StatusBadGateway, "failed to open the DM: "+err.Error())
		return
	}

	kind := conversation.KindText
	if req.Ask {
		kind = conversation.KindAsk
	}
	m := conversation.Message{
		Kind:   kind,
		Text:   req.Text,
		Author: agentPrincipal(sp.agent),
		Origin: "cli",
		Fields: map[string]string{conversation.BeadTo: peer.principal()},
	}
	d, err := a.bot.Conversation().Post(ctx, dm, m)
	if err != nil {
		// Degrade, never drop: the Slack IM still goes, with no bead to
		// capture a reply on.
		a.logger.Warn("say --to: DM message bead write failed", "agent", sp.agent, "dm", dm.ID, "error", err)
	}
	resp := sayResponse{OK: true, ID: d.BeadID, DM: dm.ID}

	if peer.agent != "" {
		a.nudgeDMPeer(ctx, sp.agent, peer.agent, d.BeadID, req)
	} else if peer.person.SlackID != "" {
		channel, ts, err := a.bot.SendDM(ctx, peer.person.SlackID, a.dmSlackText(ctx, sp.agent, peer.person.SlackID, req))
		if err != nil {
			a.logger.Error("say --to: Slack IM failed", "agent", sp.agent, "to", peer.principal(), "bead", d.BeadID, "error", err)
			msg := "failed to send the Slack IM: " + err.Error()
			if d.BeadID != "" {
				msg += " (the DM is in the Commons as " + d.BeadID + ")"
			}
			writeInboundError(w, http.StatusBadGateway, msg)
			return
		}
		resp.Channel, resp.Timestamp = channel, ts
		if d.BeadID != "" {
			stamp := map[string]string{beadSlackChannel: channel, beadSlackTS: ts}
			if err := a.bot.daemon.UpdateBeadFields(ctx, d.BeadID, stamp); err != nil {
				// An ask whose Slack ref is lost is answered in the Commons only.
				a.logger.Warn("say --to: Slack IM ref not stamped on the DM bead", "bead", d.BeadID, "error", err)
			}
		}
	}
	if d.BeadID != "" {
		a.stampLineage(ctx, d.BeadID, sp.agent, req.AgentBead, req.TriggerBead)
	}
	writeSayJSON(w, resp)
}

// dmPeer resolves a say's `to`: agent:<name>; oidc:<email> or an email; a
// Slack user id; a GitLab or GitHub username from the user directory; else an
// agent's name. A person needs an email, which is their DM member id. An
// agent must be in the speaker's land (section 6.2 rule 4).
func (a *ConversationAgentAPI) dmPeer(ctx context.Context, to string, sp speaker) (dmPeer, error) {
	to = strings.TrimSpace(to)
	if name, ok := strings.CutPrefix(to, "agent:"); ok {
		return a.agentPeer(ctx, name, sp, true)
	}
	b := a.bot
	if strings.HasPrefix(to, "oidc:") || strings.Contains(to, "@") {
		p := b.personByEmail(ctx, to)
		if p.Email == "" || !strings.Contains(p.Email, "@") {
			return dmPeer{}, sayErrorf(http.StatusBadRequest, "%q is not an email", to)
		}
		return dmPeer{person: p}, nil
	}
	if slackUserIDShape.MatchString(to) {
		if p, ok := b.personBySlackID(ctx, to); ok {
			return dmPeer{person: p}, nil
		}
		return dmPeer{}, sayErrorf(http.StatusUnprocessableEntity,
			"Slack user %s has no email the bridge knows; a DM needs one (gb dm send reaches them in Slack alone)", to)
	}
	dir := b.UserDirectory()
	for _, u := range []*UserIdentity{dir.LookupByGitLabUsername(ctx, to), dir.LookupByGitHubUsername(ctx, to)} {
		if u == nil {
			continue
		}
		if u.Email == "" {
			return dmPeer{}, sayErrorf(http.StatusUnprocessableEntity,
				"%s has no email in the user directory; a DM needs one (gb dm send reaches them in Slack alone)", to)
		}
		return dmPeer{person: b.personByEmail(ctx, u.Email)}, nil
	}
	return a.agentPeer(ctx, to, sp, false)
}

// agentPeer resolves an agent target by name. named is true when the say
// said agent:<name>, so a miss names only agents.
func (a *ConversationAgentAPI) agentPeer(ctx context.Context, name string, sp speaker, named bool) (dmPeer, error) {
	name = bareAgent(name)
	if name == "" {
		return dmPeer{}, sayErrorf(http.StatusBadRequest, "to is required")
	}
	if name == sp.agent {
		return dmPeer{}, sayErrorf(http.StatusBadRequest, "a DM is with someone else")
	}
	bead, err := a.bot.daemon.FindAgentBead(ctx, name)
	if errors.Is(err, beadsapi.ErrAgentNotFound) || (err == nil && bead == nil) {
		if named {
			return dmPeer{}, sayErrorf(http.StatusNotFound, "no agent named %s", name)
		}
		return dmPeer{}, sayErrorf(http.StatusNotFound,
			"no person or agent named %s: pass an email, a Slack user id, a GitLab or GitHub username, or agent:<name>", name)
	}
	if err != nil {
		return dmPeer{}, fmt.Errorf("reading agent %s: %w", name, err)
	}
	land := projectFromLabels(bead.Labels)
	if land == "" {
		land = strings.TrimSpace(bead.Fields["project"])
	}
	if land != sp.land {
		return dmPeer{}, sayErrorf(http.StatusForbidden,
			"agent %s is not in land %q; an agent DMs only agents of its own land", name, sp.land)
	}
	return dmPeer{agent: name}, nil
}

// dmSlackText is the Slack IM face of a say to a person: gb dm send's for a
// message, gb dm ask's for an ask, so a person sees what they always saw.
func (a *ConversationAgentAPI) dmSlackText(ctx context.Context, agent, slackID string, req sayRequest) string {
	ev := &BeadEvent{Fields: map[string]string{"body": req.Text, "source_agent": agent}}
	if !req.Ask {
		return buildDMText(ev)
	}
	if req.Task != "" {
		ev.Fields["task_context"] = req.Task
	}
	return buildQuestionDMText(a.bot.agentTheme(ctx, agent), ev, a.bot.vibeModeForSlackUser(ctx, slackID))
}

// nudgeDMPeer tells the agent a DM went to about it. Best-effort: the DM is
// in its channel either way.
func (a *ConversationAgentAPI) nudgeDMPeer(ctx context.Context, from, to, beadID string, req sayRequest) {
	what := "DM"
	if req.Ask {
		what = "Question"
	}
	msg := fmt.Sprintf("%s from agent %s", what, from)
	if beadID != "" {
		msg += fmt.Sprintf(" (bead %s)", beadID)
	}
	msg += fmt.Sprintf(": %s\nAnswer with: gb say --to agent:%s \"...\"", truncateText(req.Text, 500), from)
	if err := NudgeAgent(ctx, a.bot.daemon, a.logger, to, msg); err != nil {
		a.logger.Warn("say --to: nudging the agent failed", "from", from, "to", to, "error", err)
	}
}
