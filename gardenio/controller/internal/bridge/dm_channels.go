package bridge

import (
	"context"
	"strings"
	"time"

	"gardenio/controller/internal/conversation"
)

// DMs in the Commons (design spec 2026-10-19-lands-commons-design.md, sections
// 2.4 and 7.2 step 8). A DM is a channel bead with channel_kind=dm between an
// agent and a person (or another agent) in the agent's land, made on first use
// (conversation.EnsureDM). Three things write into it:
//
//   - gb say --to and --ask (api_conversation_dm.go): the message bead, with
//     the Slack IM to a person as its delivery;
//   - the DM-first decision card (bot_decisions_dm.go): its DM copy's card
//     bead is in the DM channel between the requesting agent and the
//     addressee, beside the Slack IM copy it records;
//   - the escalation operator DM (escalation_cards.go), the same way.
//
// OWNER CONSTRAINT (Q8b gd-t4lzcEOEFT, native-push): Slack stays the primary
// delivery for decisions and DMs. Every Slack call here is the one the bridge
// made before; the DM channel is a second record of it. Handing delivery to the
// Commons waits for native push (Commons III step 4.11).

// dmPerson is a person a DM is with, resolved far enough to be a member of a
// DM channel (oidc:<email>) and to be sent the Slack IM.
type dmPerson struct {
	Email   string
	Name    string // display name, else the email's local part
	SlackID string // "" when the person has no Slack account the bridge knows
}

// principal is the person's DM member id.
func (p dmPerson) principal() string { return "oidc:" + p.Email }

// personBySlackID resolves a Slack user to the person a DM is with: the
// confirmed account link first, else the user directory. ok is false when
// neither gives an email.
func (b *Bot) personBySlackID(ctx context.Context, slackID string) (dmPerson, bool) {
	if slackID == "" {
		return dmPerson{}, false
	}
	p := dmPerson{SlackID: slackID}
	if l := b.slackLinker(); l != nil {
		if email, ok := l.Linked(slackID); ok {
			p.Email = normalizeEmail(email)
		}
	}
	if u := b.UserDirectory().LookupBySlackID(ctx, slackID); u != nil {
		if p.Email == "" {
			p.Email = normalizeEmail(u.Email)
		}
		p.Name = strings.TrimSpace(u.DisplayName)
	}
	return p.named(), p.Email != ""
}

// personByEmail resolves an email to the person a DM is with, filling the
// display name and Slack id from the user directory when it knows them.
func (b *Bot) personByEmail(ctx context.Context, email string) dmPerson {
	p := dmPerson{Email: normalizeEmail(strings.TrimPrefix(strings.TrimSpace(email), "oidc:"))}
	if u := b.UserDirectory().LookupByEmail(ctx, p.Email); u != nil {
		p.Name, p.SlackID = strings.TrimSpace(u.DisplayName), strings.TrimSpace(u.SlackID)
	}
	return p.named()
}

func (p dmPerson) named() dmPerson {
	if p.Name == "" {
		p.Name, _, _ = strings.Cut(p.Email, "@")
	}
	return p
}

// dmChannelTitle is a DM's display name: its two members' names, the agent
// first.
func dmChannelTitle(agent, other string) string {
	return agent + ", " + other
}

// bareAgent is an agent's name from its name, path or agent:<name> principal.
func bareAgent(agent string) string {
	return strings.TrimPrefix(extractAgentName(strings.TrimSpace(agent)), "agent:")
}

// dmChannelWith returns the DM channel of land between agent and the person,
// made on first use. ok is false when either is unknown or the daemon fails:
// the caller keeps the record it kept before DMs (a card bead keyed by the
// Slack IM alone).
func (b *Bot) dmChannelWith(ctx context.Context, land, agent string, p dmPerson) (conversation.ChannelRef, bool) {
	agent = bareAgent(agent)
	if b.daemon == nil || land == "" || agent == "" || p.Email == "" {
		return conversation.ChannelRef{}, false
	}
	ch, err := conversation.EnsureDM(ctx, b.daemon, land, dmChannelTitle(agent, p.Name), "agent:"+agent, p.principal())
	if err != nil {
		b.logger.Warn("dm: DM channel not found or made", "land", land, "agent", agent, "error", err)
		return conversation.ChannelRef{}, false
	}
	return ch, true
}

// recordDMCard records at, the Slack IM copy of subject's card the Bot posted
// itself (the DM-first decision card, the escalation operator DM), on the card
// bead of subject in the DM channel between author and the person, so the
// Commons shows it there. Without a DM channel the card bead is keyed by the
// Slack IM alone, as before DMs.
func (b *Bot) recordDMCard(ctx context.Context, land, subject string, author conversation.Principal, p dmPerson, at MessageRef, text string) {
	if b.daemon == nil || land == "" || subject == "" {
		return
	}
	ch := conversation.ChannelRef{Land: land, Slack: at.ChannelID}
	if dm, ok := b.dmChannelWith(ctx, land, author.ID, p); ok {
		ch.ID = dm.ID
	}
	ref := conversation.Ref{Surface: conversation.SurfaceSlack, Channel: at.ChannelID, TS: at.Timestamp}
	m := conversation.Message{SubjectID: subject, Text: text, Author: author}
	if b.Conversation().RecordCard(ctx, ch, nil, m, ref) == "" {
		b.logger.Warn("card: DM copy not recorded on a card bead", "subject", subject, "channel", at.ChannelID, "dm", ch.ID)
	}
}

// dmCloseInterval is how often the bridge closes the DMs that are done with.
const dmCloseInterval = 30 * time.Minute

// CloseDoneDMs closes the DMs that are done with (conversation.DMCloser,
// gd-ZUywEoAvCi) now and every dmCloseInterval until ctx is done, so the
// Commons stops listing and reading them.
func (b *Bot) CloseDoneDMs(ctx context.Context) {
	if b.daemon == nil {
		return
	}
	closer := &conversation.DMCloser{Store: b.daemon}
	ticker := time.NewTicker(dmCloseInterval)
	defer ticker.Stop()
	for {
		if n, err := closer.Sweep(ctx); err != nil {
			b.logger.Warn("dm: close sweep failed", "closed", n, "error", err)
		} else {
			b.logger.Info("dm: closed the DMs that are done with", "closed", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
