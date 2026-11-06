package bridge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/beadsapi"
)

// /runner invite|revoke|status (spec §4.2 steps 1 and 4). An invite is a
// single-use, 24-hour type=runner_invite bead holding only the hash of a code
// that is DM'd to the invitee; kbeads POST /v1/runners/register redeems it.
// type=runner_invite is admin-gated (min_role gardenio-admin), so the create
// runs on the invoking person's own linked bearer (withSlackActor in
// handleSlashCommand): an admin's invite succeeds, anyone else gets the
// daemon's 403 back as the ephemeral error.

var slackMentionRe = regexp.MustCompile(`^<@([A-Z0-9]+)(?:\|[^>]*)?>$`)

// inviteAlphabet drops the look-alikes 0/O and 1/I so a code read off a
// screen types back correctly.
const inviteAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// runnerInviteTTL is how long an invite code stays redeemable.
const runnerInviteTTL = 24 * time.Hour

// runnerStateRevoked is kbeads' fields.state on a revoked runner bead; the
// bead itself stays open.
const runnerStateRevoked = "revoked"

// newInviteCode returns a fresh "GRI-XXXX-XXXX" code. len(inviteAlphabet) is
// 32, which divides 256, so the byte-modulo pick is unbiased.
func newInviteCode() (string, error) {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString("GRI-")
	for i, c := range raw {
		if i == 4 {
			sb.WriteByte('-')
		}
		sb.WriteByte(inviteAlphabet[int(c)%len(inviteAlphabet)])
	}
	return sb.String(), nil
}

// inviteCodeHash is the code_hash kbeads matches on redeem: hex(sha256(code)).
func inviteCodeHash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

// parseRunnerInvite parses `invite <@U123|name> proj-a proj-b`. The invitee
// must be a Slack mention and at least one project is required: a runner's
// project allowlist is never empty (D8).
func parseRunnerInvite(text string) (string, []string, error) {
	fields := strings.Fields(text)
	if len(fields) < 2 || fields[0] != "invite" {
		return "", nil, fmt.Errorf("usage: /runner invite @person project [project...]")
	}
	m := slackMentionRe.FindStringSubmatch(fields[1])
	if m == nil {
		return "", nil, fmt.Errorf("the second word must be an @mention of the person to invite")
	}
	if len(fields) < 3 {
		return "", nil, fmt.Errorf("name at least one project: /runner invite @person project [project...]")
	}
	return m[1], fields[2:], nil
}

// /runner revoke is the owner's own off switch (spec D6). On a chart-default
// deploy the call reaches the daemon on the bridge's fleet token, which the
// daemon admits for any runner, so the bridge checks that the Slack caller is
// the runner's owner before it calls. An admin is not let through here: a
// Slack interaction never carries gardenio-admin (slackRoleCeiling), so an
// admin revokes someone else's runner with their own OIDC sign-in, which the
// daemon checks itself (callerMayRevokeRunner).

// handleRunnerCommand dispatches /runner invite, revoke and status.
func (b *Bot) handleRunnerCommand(ctx context.Context, cmd slack.SlashCommand) {
	sub := strings.Fields(cmd.Text)
	if len(sub) == 0 {
		b.ephemeral(ctx, cmd, "Usage: `/runner invite @person project...` · `/runner revoke <hostname>` · `/runner status`")
		return
	}
	switch sub[0] {
	case "invite":
		uid, projects, err := parseRunnerInvite(cmd.Text)
		if err != nil {
			b.runnerFail(ctx, cmd, err.Error())
			return
		}
		code, inviteID, err := b.createRunnerInvite(ctx, uid, projects)
		if err != nil {
			b.runnerFail(ctx, cmd, "could not create invite: "+err.Error())
			return
		}
		if err := b.dmRunnerInvite(ctx, uid, code, projects); err != nil {
			// Only the hash is stored, so an undelivered code can never be
			// used: close the invite rather than leave a dead one open.
			if cerr := b.daemon.CloseBead(ctx, inviteID, nil); cerr != nil {
				b.logger.Warn("could not close undelivered runner invite", "invite", inviteID, "error", cerr)
			}
			b.runnerFail(ctx, cmd, fmt.Sprintf("could not DM <@%s> the invite code (%v); the invite was withdrawn.", uid, err))
			return
		}
		b.ephemeral(ctx, cmd, b.themeFor(ctx, cmd.ChannelID).Semantic(SemOK)+fmt.Sprintf(" Invite sent to <@%s> for %s. It works once and expires in 24 hours.", uid, strings.Join(projects, ", ")))
	case "revoke":
		if len(sub) != 2 {
			b.ephemeral(ctx, cmd, "Usage: `/runner revoke <hostname>`")
			return
		}
		b.revokeRunnerByHostname(ctx, cmd, sub[1])
	case "status":
		b.runnerStatus(ctx, cmd)
	default:
		b.ephemeral(ctx, cmd, "Unknown subcommand. Try `invite`, `revoke`, or `status`.")
	}
}

// createRunnerInvite writes the type=runner_invite bead for slackUserID and
// returns the plaintext code (never stored) and the invite bead's id.
func (b *Bot) createRunnerInvite(ctx context.Context, slackUserID string, projects []string) (code, inviteID string, err error) {
	resolve := b.operatorFromSlackUser
	if b.operatorResolver != nil {
		resolve = b.operatorResolver
	}
	// The invitee becomes the runner's owner, so it must be a real person:
	// operatorFromSlackUser falls back to slack:U…/fleet: sentinels.
	invitee := resolve(ctx, slackUserID)
	if !strings.HasPrefix(invitee, "oidc:") {
		return "", "", fmt.Errorf("could not resolve <@%s> to a gardenio sign-in; they need to have signed in once", slackUserID)
	}
	code, err = newInviteCode()
	if err != nil {
		return "", "", err
	}
	fields, err := json.Marshal(map[string]any{
		"code_hash":  inviteCodeHash(code),
		"invitee":    invitee,
		"projects":   projects,
		"expires_at": time.Now().Add(runnerInviteTTL).UTC().Format(time.RFC3339),
	})
	if err != nil {
		return "", "", err
	}
	labels := make([]string, 0, len(projects))
	for _, p := range projects {
		labels = append(labels, "project:"+p)
	}
	inviteID, err = b.daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
		Title:      "Runner invite for " + invitee,
		ShortTitle: "runner invite",
		Type:       "runner_invite",
		Kind:       "data",
		Labels:     labels,
		Fields:     json.RawMessage(fields),
	})
	if err != nil {
		return "", "", err
	}
	return code, inviteID, nil
}

// dmRunnerInvite DMs the invitee the code and the register command.
func (b *Bot) dmRunnerInvite(ctx context.Context, slackUserID, code string, projects []string) error {
	daemon := b.runnerPublicURL
	if daemon == "" {
		daemon = "<ask your operator for the runner URL>"
	}
	text := fmt.Sprintf("You have been invited to run a gardenio local agent for projects: %s.\nOn your laptop:\n```gb runner register --invite %s --daemon %s```\nThe code works once and expires in 24 hours.",
		strings.Join(projects, ", "), code, daemon)
	if _, _, err := b.api.PostMessageContext(ctx, slackUserID, slack.MsgOptionText(text, false)); err != nil {
		b.logger.Error("runner invite DM failed", "user", slackUserID, "error", err)
		return err
	}
	return nil
}

// listRunners returns the open type=runner beads, sorted by hostname.
func (b *Bot) listRunners(ctx context.Context) ([]*beadsapi.BeadDetail, error) {
	res, err := b.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"runner"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		return nil, err
	}
	runners := res.Beads
	sort.Slice(runners, func(i, j int) bool {
		return runners[i].Fields["hostname"] < runners[j].Fields["hostname"]
	})
	return runners, nil
}

// revokeRunnerByHostname revokes the live runner whose hostname (or bead id)
// is target. Already-revoked runner beads stay open, so they are skipped: a
// laptop re-registered after a revoke matches only its live runner.
func (b *Bot) revokeRunnerByHostname(ctx context.Context, cmd slack.SlashCommand, target string) {
	runners, err := b.listRunners(ctx)
	if err != nil {
		b.runnerFail(ctx, cmd, "could not list runners: "+err.Error())
		return
	}
	var matches []*beadsapi.BeadDetail
	for _, r := range runners {
		if r.Fields["state"] == runnerStateRevoked {
			continue
		}
		if r.Fields["hostname"] == target || r.ID == target {
			matches = append(matches, r)
		}
	}
	switch len(matches) {
	case 0:
		b.runnerFail(ctx, cmd, fmt.Sprintf("No live runner named `%s`. `/runner status` lists them.", target))
		return
	case 1:
	default:
		ids := make([]string, 0, len(matches))
		for _, r := range matches {
			ids = append(ids, "`"+r.ID+"`")
		}
		b.runnerFail(ctx, cmd, fmt.Sprintf("%d runners are named `%s`: %s. Run `/runner revoke <id>` with one of them.", len(matches), target, strings.Join(ids, ", ")))
		return
	}
	r := matches[0]
	if caller := b.runnerCaller(ctx, cmd.UserID); !ownsRunner(caller, r) {
		b.logger.Info("runner revoke refused: caller does not own the runner", "user", cmd.UserID, "caller", caller, "runner", r.ID)
		b.runnerFail(ctx, cmd, fmt.Sprintf("Only the owner of %s (%s) can revoke it from Slack. An admin can revoke it with their own sign-in.", runnerHostname(r), runnerOwner(r)))
		return
	}
	tokens, agents, err := b.daemon.RevokeRunner(ctx, r.ID)
	if err != nil {
		b.runnerFail(ctx, cmd, fmt.Sprintf("could not revoke %s (%s): %v", runnerHostname(r), r.ID, err))
		return
	}
	b.ephemeral(ctx, cmd, fmt.Sprintf("Revoked %s (%s): %d tokens, %d agents told to stop.", runnerHostname(r), r.ID, tokens, agents))
}

// runnerCaller is the person behind a /runner, /kill or /kill-thread
// command, as an "oidc:<email>" operator id: the one handleSlashCommand
// stamped on ctx (the linked account's own email when account linking is on),
// else resolved from the Slack user. An unresolved user yields a slack:/fleet:
// sentinel, which owns no runner and no agent.
func (b *Bot) runnerCaller(ctx context.Context, slackUserID string) string {
	if op := beadsapi.OperatorFromContext(ctx); op != "" {
		return op
	}
	resolve := b.operatorFromSlackUser
	if b.operatorResolver != nil {
		resolve = b.operatorResolver
	}
	return resolve(ctx, slackUserID)
}

// ownsRunner reports whether caller (an operator id) is r's owner. Only a
// resolved person (oidc:) can own one.
func ownsRunner(caller string, r *beadsapi.BeadDetail) bool {
	return strings.HasPrefix(caller, "oidc:") && strings.EqualFold(caller, runnerOwner(r))
}

// runnerOwner is the runner's owner ("oidc:<email>", the invitee).
func runnerOwner(r *beadsapi.BeadDetail) string {
	if o := r.Fields["owner"]; o != "" {
		return o
	}
	return r.Owner
}

// runnerStatusMaxCards caps the runner cards in one /runner status reply.
// Slack rejects a message over 50 blocks, and posts nothing: each card is
// three blocks, a divider sits between cards and one overflow line follows
// the last, so 12 cards is 3*12 + 11 + 1 = 48 blocks and 13 would be 52.
const runnerStatusMaxCards = 12

// runnerStatus replies with one runner card per registered runner (spec §9).
func (b *Bot) runnerStatus(ctx context.Context, cmd slack.SlashCommand) {
	runners, err := b.listRunners(ctx)
	if err != nil {
		b.runnerFail(ctx, cmd, "could not list runners: "+err.Error())
		return
	}
	if len(runners) == 0 {
		b.ephemeral(ctx, cmd, "No runners registered. Invite one with `/runner invite @person project...`.")
		return
	}
	agents, err := b.activeLocalAgentsByRunner(ctx)
	if err != nil {
		// The cards still say who owns what; only the agent column is lost.
		b.logger.Warn("runner status: could not list laptop agents", "error", err)
	}
	blocks := buildRunnerStatusBlocks(b.themeFor(ctx, cmd.ChannelID), runners, agents)
	text := fmt.Sprintf("%d runners", len(runners))
	if _, err := b.api.PostEphemeralContext(ctx, cmd.ChannelID, cmd.UserID,
		slack.MsgOptionText(text, false), slack.MsgOptionBlocks(blocks...)); err != nil {
		b.logger.Warn("slash command ephemeral reply failed", "command", cmd.Command, "user", cmd.UserID, "error", err)
	}
}

// buildRunnerStatusBlocks renders the /runner status reply: up to
// runnerStatusMaxCards runner cards separated by dividers, then an overflow
// line naming how many runners were left out.
func buildRunnerStatusBlocks(th Theme, runners []*beadsapi.BeadDetail, agents map[string][]string) []slack.Block {
	var blocks []slack.Block
	for i, r := range runners {
		if i == runnerStatusMaxCards {
			blocks = append(blocks, slack.NewContextBlock("",
				slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("_...and %d more_", len(runners)-i), false, false)))
			break
		}
		if i > 0 {
			blocks = append(blocks, slack.NewDividerBlock())
		}
		info := runnerInfo(r)
		info.Hostname = runnerHostname(r)
		info.Owner = runnerOwner(r)
		lastSeen, _ := time.Parse(time.RFC3339, r.Fields["last_seen_at"])
		blocks = append(blocks, buildRunnerCardBlocks(th, info, lastSeen, runnerProjects(r), agents[r.ID])...)
	}
	return blocks
}

// buildRunnerCardBlocks renders one runner: its hostname as the title, then
// owner, state with last heartbeat, tags, projects and its live laptop
// agents. lastSeen is zero when the runner has never sent a heartbeat.
func buildRunnerCardBlocks(th Theme, r RunnerInfo, lastSeen time.Time, projects, agents []string) []slack.Block {
	state := r.State
	switch {
	case state == "":
		state = "unknown"
	case state == "online" && (lastSeen.IsZero() || time.Since(lastSeen) > runnerOnlineWindow):
		// Placement treats a silent runner as offline (runnerOnline); say so.
		state = "offline"
	}
	switch {
	case lastSeen.IsZero():
	case time.Since(lastSeen) < time.Minute:
		state += " (last seen just now)"
	default:
		state += fmt.Sprintf(" (last seen %s)", formatAge(lastSeen))
	}
	owner := "unknown"
	if r.Owner != "" {
		owner = formatOwnerMention(r.Owner)
	}
	orNone := func(xs []string) string {
		if len(xs) == 0 {
			return "none"
		}
		return escapeSlackMrkdwn(strings.Join(xs, ", "))
	}
	fields := []*slack.TextBlockObject{
		slack.NewTextBlockObject("mrkdwn", "*Owner*\n"+owner, false, false),
		slack.NewTextBlockObject("mrkdwn", "*State*\n"+state, false, false),
		slack.NewTextBlockObject("mrkdwn", "*Tags*\n"+orNone(r.Tags), false, false),
		slack.NewTextBlockObject("mrkdwn", "*Projects*\n"+orNone(projects), false, false),
		slack.NewTextBlockObject("mrkdwn", "*Active agents*\n"+orNone(agents), false, false),
	}
	return []slack.Block{
		slack.NewSectionBlock(
			slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("%s *%s*", th.Semantic(SemLocal), escapeSlackMrkdwn(r.Hostname)), false, false),
			nil, nil),
		slack.NewSectionBlock(nil, fields, nil),
		slack.NewContextBlock("",
			slack.NewTextBlockObject("mrkdwn", fmt.Sprintf("`%s` \u00b7 `/runner revoke %s`", r.ID, escapeSlackMrkdwn(r.Hostname)), false, false)),
	}
}

// runnerHostname is the runner's hostname, falling back to the bead title.
func runnerHostname(r *beadsapi.BeadDetail) string {
	if h := r.Fields["hostname"]; h != "" {
		return h
	}
	return r.Title
}

// runnerProjects decodes fields.projects, a JSON list flattened to a string
// by the client's fields map.
func runnerProjects(r *beadsapi.BeadDetail) []string {
	raw := r.Fields["projects"]
	var projects []string
	if err := json.Unmarshal([]byte(raw), &projects); err != nil && raw != "" {
		return []string{raw}
	}
	return projects
}

// runnerFail replies with a failure line, glyph from the channel's theme.
func (b *Bot) runnerFail(ctx context.Context, cmd slack.SlashCommand, text string) {
	b.ephemeral(ctx, cmd, b.themeFor(ctx, cmd.ChannelID).Semantic(SemFail)+" "+text)
}

// ephemeral replies to a slash command with a message only its invoker sees.
func (b *Bot) ephemeral(ctx context.Context, cmd slack.SlashCommand, text string) {
	if _, err := b.api.PostEphemeralContext(ctx, cmd.ChannelID, cmd.UserID, slack.MsgOptionText(text, false)); err != nil {
		b.logger.Warn("slash command ephemeral reply failed", "command", cmd.Command, "user", cmd.UserID, "error", err)
	}
}
