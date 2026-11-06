package bridge

import (
	"context"
	"errors"
	"strings"

	"github.com/slack-go/slack"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// edgeAuthModeSlack is the envelope auth_mode the bridge signs for a person it
// resolved from a Slack user. The daemon only logs the mode; the roles carry
// the authority.
const edgeAuthModeSlack = "slack"

// PersonRoleSource reports the canonical gardenio-* roles the identity
// provider the daemon trusts (Keycloak) grants a person. It is the ONLY source
// of roles the bridge signs: roles are never taken from config, a user bead,
// or Slack. An unknown or disabled person yields no roles and no error; any
// failure to ask yields an error (callers fail closed).
type PersonRoleSource interface {
	GardenioRoles(ctx context.Context, email string) ([]string, error)
}

// SetPersonRoleSource turns on person-bound Slack writes (gd-7CDM9gdog0): once
// set, a Slack interaction is performed AS the person the Slack user maps to,
// with that person's Keycloak roles, via a signed v2 edge-auth envelope; a user
// who maps to nobody, or to nobody holding a gardenio role, is refused. Unset
// (the default) keeps the legacy v1 slack:U… envelope. Requires the edge-auth
// HMAC key on the daemon client, or every such write fails closed.
func (b *Bot) SetPersonRoleSource(src PersonRoleSource) {
	b.mu.Lock()
	b.personRoles = src
	b.mu.Unlock()
}

func (b *Bot) personRoleSource() PersonRoleSource {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.personRoles
}

// SetSlackLinker turns on account-linked Slack writes (gd-OZJ6H2S3xx): every
// human-initiated daemon write carries the linked person's own Keycloak
// access token as the bearer, an unlinked user is shown the link prompt and
// no write is sent. Takes precedence over SetPersonRoleSource.
func (b *Bot) SetSlackLinker(l *SlackLinker) {
	b.mu.Lock()
	b.slackLink = l
	b.mu.Unlock()
	if l != nil {
		l.SetOnConfirm(b.postLinkConfirm)
		if b.api != nil {
			l.SetEmailResolver(&slackAPIEmailResolver{api: b.api})
		}
	}
}

func (b *Bot) slackLinker() *SlackLinker {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.slackLink
}

// actsAsPerson reports whether Slack-initiated writes are person-bound (link
// or PersonRoleSource) rather than the legacy slack:U… envelope.
func (b *Bot) actsAsPerson() bool {
	return b.slackLinker() != nil || b.personRoleSource() != nil
}

// refusalNeedsLink is the refusal withSlackActor returns for an unlinked
// user; postSlackRefusal renders it as the link prompt with a button instead
// of plain text.
const refusalNeedsLink slackPersonRefusal = "\x00needs-link"

// slackLinkPromptText is the fallback text of the link prompt.
const slackLinkPromptText = "Link your gardenio account to act from Slack. Press the button, sign in once, then retry."

// withLinkedSlackActor is the account-linking branch of withSlackActor: the
// person's own access token becomes the daemon bearer. kbeads authorizes
// and attributes the write from that token (azp gardenio-slack-link, capped at
// operator by the daemon's client role ceiling); the operator stamp is a
// display hint only.
func (b *Bot) withLinkedSlackActor(ctx context.Context, l *SlackLinker, slackUserID string) (context.Context, slackPersonRefusal, bool) {
	tok, email, err := l.AccessToken(ctx, slackUserID)
	switch {
	case errors.Is(err, ErrSlackNotLinked):
		b.logger.Info("slack person refused: not linked", "user", slackUserID)
		return ctx, refusalNeedsLink, false
	case err != nil:
		b.logger.Error("slack person refused: could not mint the person's token (failing closed)", "user", slackUserID, "error", err)
		return ctx, "I couldn't get your gardenio sign-in right now, so I did not act. Try again in a moment.", false
	}
	ctx = beadsapi.WithOperator(ctx, "oidc:"+email)
	return beadsapi.WithUserBearer(ctx, tok), "", true
}

// postSlackRefusal shows a refusal to the user: the link prompt (with the
// single-use button bound to this Slack user) for refusalNeedsLink, plain
// text otherwise. Ephemeral in the channel when there is one, else a DM.
func (b *Bot) postSlackRefusal(ctx context.Context, channelID, userID, teamID, threadTS string, refusal slackPersonRefusal) {
	if b.api == nil || userID == "" {
		return
	}
	var opts []slack.MsgOption
	if refusal == refusalNeedsLink {
		l := b.slackLinker()
		if l == nil {
			return
		}
		startURL, err := l.StartURL(userID, teamID)
		if err != nil {
			b.logger.Error("could not create slack link state", "user", userID, "error", err)
			return
		}
		opts = []slack.MsgOption{slack.MsgOptionText(slackLinkPromptText, false), slack.MsgOptionBlocks(slackLinkPromptBlocks(startURL)...)}
	} else {
		opts = []slack.MsgOption{slack.MsgOptionText(string(refusal), false)}
	}
	if channelID != "" {
		if threadTS != "" {
			opts = append(opts, slack.MsgOptionTS(threadTS))
		}
		if _, err := b.api.PostEphemeral(channelID, userID, opts...); err != nil {
			b.logger.Warn("could not post slack refusal", "user", userID, "error", err)
		}
		return
	}
	if _, _, err := b.api.PostMessage(userID, opts...); err != nil {
		b.logger.Warn("could not DM slack refusal", "user", userID, "error", err)
	}
}

// slackLinkPromptBlocks builds the link prompt: one line and a button that
// opens the bridge's /slack/link/start for this user's state.
func slackLinkPromptBlocks(startURL string) []slack.Block {
	text := slack.NewTextBlockObject(slack.MarkdownType,
		"*Link your gardenio account* to act from Slack. You'll sign in once in the browser; the link expires in 10 minutes and works once.", false, false)
	btn := slack.NewButtonBlockElement("slack_link_start", "link", slack.NewTextBlockObject(slack.PlainTextType, "Link account", false, false))
	btn.URL = startURL
	btn.Style = slack.StylePrimary
	return []slack.Block{
		slack.NewSectionBlock(text, nil, nil),
		slack.NewActionBlock("slack_link_actions", btn),
	}
}

// Action ids of the link confirm prompt. Their presses are handled before
// withSlackActor: the person pressing is, by definition, not linked yet.
const (
	slackLinkConfirmActionID = "slack_link_confirm"
	slackLinkCancelActionID  = "slack_link_cancel"
)

// slackLinkConfirmBlocks builds the "Link this Slack account to <email>?"
// prompt. confirmID rides in both buttons' value.
func slackLinkConfirmBlocks(email, warning, confirmID string) []slack.Block {
	text := "*Link this Slack account to " + email + "?*\nSlack actions (approving escalations, answering decisions) will then run as that gardenio account. " +
		"Press Cancel if you did not just sign in as " + email + "; the question expires in 10 minutes."
	if warning != "" {
		text += "\nNote: " + warning
	}
	confirm := slack.NewButtonBlockElement(slackLinkConfirmActionID, confirmID, slack.NewTextBlockObject(slack.PlainTextType, "Confirm", false, false))
	confirm.Style = slack.StylePrimary
	cancel := slack.NewButtonBlockElement(slackLinkCancelActionID, confirmID, slack.NewTextBlockObject(slack.PlainTextType, "Cancel", false, false))
	cancel.Style = slack.StyleDanger
	return []slack.Block{
		slack.NewSectionBlock(slack.NewTextBlockObject(slack.MarkdownType, text, false, false), nil, nil),
		slack.NewActionBlock("slack_link_confirm_actions", confirm, cancel),
	}
}

// postLinkConfirm DMs the Slack user from the link state the confirm prompt.
// A DM, not an ephemeral: the state records no channel, and the question must
// reach exactly that user wherever they pressed the link button.
func (b *Bot) postLinkConfirm(slackUserID, confirmID, email, warning string) error {
	if b.api == nil {
		return errors.New("no slack api")
	}
	_, _, err := b.api.PostMessage(slackUserID,
		slack.MsgOptionText("Link this Slack account to "+email+"? Confirm or Cancel.", false),
		slack.MsgOptionBlocks(slackLinkConfirmBlocks(email, warning, confirmID)...))
	return err
}

// isSlackLinkConfirmAction reports whether a block_actions callback is a press
// on the link confirm prompt, and returns the action.
func isSlackLinkConfirmAction(callback slack.InteractionCallback) (*slack.BlockAction, bool) {
	if callback.Type != slack.InteractionTypeBlockActions || callback.ActionCallback.BlockActions == nil {
		return nil, false
	}
	for _, a := range callback.ActionCallback.BlockActions {
		if a != nil && (a.ActionID == slackLinkConfirmActionID || a.ActionID == slackLinkCancelActionID) {
			return a, true
		}
	}
	return nil, false
}

// handleSlackLinkConfirm applies a Confirm / Cancel press. The linker checks
// the presser is the Slack user the link flow was started for, so a forwarded
// start URL cannot be confirmed by (or on behalf of) anyone else.
func (b *Bot) handleSlackLinkConfirm(ctx context.Context, action *slack.BlockAction, callback slack.InteractionCallback) {
	l := b.slackLinker()
	if l == nil {
		return
	}
	user := callback.User.ID
	var text string
	var err error
	if action.ActionID == slackLinkConfirmActionID {
		var email string
		if email, err = l.ConfirmLink(ctx, action.Value, user); err == nil {
			text = "Linked as *" + email + "*. Slack actions now run as you. Say `@gardenio unlink` to undo."
		}
	} else {
		var email string
		if email, err = l.CancelLink(ctx, action.Value, user); err == nil {
			text = "Cancelled. Nothing was linked to " + email + "."
		}
	}
	if err != nil {
		var refused *LinkRefusedError
		switch {
		case errors.As(err, &refused):
			text = "Not linked: " + refused.Reason
		case errors.Is(err, ErrLinkConfirmNotYours):
			// Leave the owner's prompt alone; tell only the presser.
			b.postInteractionEphemeral(ctx, callback, callback.Channel.ID, "This link question belongs to someone else, so I did nothing.")
			return
		case errors.Is(err, ErrLinkConfirmUnknown):
			text = "This link question expired or was already answered. Nothing was linked. Press the Link button again if you still want to."
		default:
			b.logger.Error("slack link confirm failed", "user", user, "error", err)
			text = "Could not complete the link. Nothing was linked."
		}
	}
	b.replaceLinkConfirm(ctx, callback, text)
}

// replaceLinkConfirm swaps the confirm prompt for the outcome (dropping the
// buttons), falling back to a DM when the response_url is unusable.
func (b *Bot) replaceLinkConfirm(ctx context.Context, callback slack.InteractionCallback, text string) {
	if callback.ResponseURL != "" {
		msg := &slack.WebhookMessage{Text: text, ReplaceOriginal: true}
		err := slack.PostWebhookContext(ctx, callback.ResponseURL, msg)
		if err == nil {
			return
		}
		b.logger.Warn("link confirm response_url post failed, falling back to a DM", "user", callback.User.ID, "error", err)
	}
	if b.api == nil || callback.User.ID == "" {
		return
	}
	if _, _, err := b.api.PostMessage(callback.User.ID, slack.MsgOptionText(text, false)); err != nil {
		b.logger.Warn("could not DM link outcome", "user", callback.User.ID, "error", err)
	}
}

// slackRoleCeiling is the highest role a Slack interaction may carry
// (decision gd-8PT2Qr2gEo). Approving an escalation and answering a decision
// only need gardenio-operator, and the Slack-to-person binding is a user-bead
// field, so a Slack button press never carries gardenio-admin even when the
// person holds it in Keycloak: admin-only writes need the browser or CLI OIDC
// session. Revisit once the binding is hardened (gd-uAvleAZUvV, PR #246).
const slackRoleCeiling = oidcauth.RoleOperator

// capSlackRoles returns min(roles, slackRoleCeiling): a person holding the
// ceiling or anything above it signs exactly the ceiling role, anyone below it
// keeps their own (highest) role. Roles outside the canonical hierarchy are
// dropped, so an empty result means the person holds no usable role.
func capSlackRoles(roles []string) []string {
	switch {
	case oidcauth.HasMinimumRole(roles, slackRoleCeiling):
		return []string{slackRoleCeiling}
	case oidcauth.HasMinimumRole(roles, oidcauth.RoleViewer):
		return []string{oidcauth.RoleViewer}
	}
	return nil
}

// slackPersonRefusal explains, in Slack, why a Slack user was not allowed to
// act. Its text is user-facing.
type slackPersonRefusal string

// resolveSlackPerson maps a Slack user id to the person (user bead, by its
// slack_id alias) and that person's Keycloak roles. Exactly one of the returns
// is meaningful: a non-empty refusal means the user must not act.
func (b *Bot) resolveSlackPerson(ctx context.Context, src PersonRoleSource, slackUserID string) (beadsapi.EdgeAuthIdentity, slackPersonRefusal) {
	// Two open user beads with one slack_id make the person depend on daemon
	// list order (the directory keeps the last), so act as nobody (fail closed).
	if slackUserID != "" && b.UserDirectory().CountBySlackID(ctx, slackUserID) > 1 {
		b.logger.Error("slack person refused: slack_id is carried by more than one open user bead", "user", slackUserID)
		return beadsapi.EdgeAuthIdentity{}, ":lock: Your Slack id is linked to more than one Gardenio person, so I can't tell who you are and did not act. Ask an admin to fix the duplicate."
	}
	var email string
	if u := b.UserDirectory().LookupBySlackID(ctx, slackUserID); u != nil {
		email = strings.ToLower(strings.TrimSpace(u.Email))
	}
	if email == "" {
		b.logger.Info("slack person refused: no person mapped to slack user", "user", slackUserID)
		return beadsapi.EdgeAuthIdentity{}, ":lock: Your Slack account isn't linked to a Gardenio person, so I can't act for you. Ask an admin to add your Slack id to your user."
	}
	roles, err := src.GardenioRoles(ctx, email)
	if err != nil {
		b.logger.Error("slack person refused: role lookup failed (failing closed)", "user", slackUserID, "error", err)
		return beadsapi.EdgeAuthIdentity{}, ":warning: I couldn't verify your Gardenio roles right now, so I did not act. Try again in a moment."
	}
	roles = capSlackRoles(roles)
	if len(roles) == 0 {
		b.logger.Info("slack person refused: no gardenio role in keycloak", "user", slackUserID, "operator", "oidc:"+email)
		return beadsapi.EdgeAuthIdentity{}, ":lock: You don't hold a Gardenio role, so I can't act for you."
	}
	return beadsapi.EdgeAuthIdentity{
		Operator: "oidc:" + email,
		AuthMode: edgeAuthModeSlack,
		Roles:    roles,
	}, ""
}

// withSlackActor stamps ctx so every daemon call made for slackUserID is done
// as that Slack user. With a PersonRoleSource it is the mapped person with
// their real roles (v2 envelope); without one it is the legacy v1 slack:U…
// envelope. When the user must be refused it returns ok=false and the text to
// show them.
func (b *Bot) withSlackActor(ctx context.Context, slackUserID string) (context.Context, slackPersonRefusal, bool) {
	ctx = beadsapi.WithOperator(ctx, b.operatorFromSlackUser(ctx, slackUserID))
	if l := b.slackLinker(); l != nil {
		return b.withLinkedSlackActor(ctx, l, slackUserID)
	}
	src := b.personRoleSource()
	if src == nil {
		return beadsapi.WithEdgeAuthOperator(ctx, "slack:"+slackUserID), "", true
	}
	id, refusal := b.resolveSlackPerson(ctx, src, slackUserID)
	if refusal != "" {
		return ctx, refusal, false
	}
	// Attribute to the canonical person, not the Slack-local guess, so Slack,
	// browser and CLI writes land on the same operator.
	ctx = beadsapi.WithOperator(ctx, id.Operator)
	return beadsapi.WithEdgeAuthIdentity(ctx, id), "", true
}

// refuseSlackUser makes a refusal visible to the user: ephemeral in the
// channel when there is one, otherwise a DM (modal submissions carry no
// channel or response_url).
func (b *Bot) refuseSlackUser(ctx context.Context, callback slack.InteractionCallback, text slackPersonRefusal) {
	if text == refusalNeedsLink {
		b.postSlackRefusal(ctx, callback.Channel.ID, callback.User.ID, callback.Team.ID, "", text)
		return
	}
	if callback.ResponseURL != "" || callback.Channel.ID != "" {
		b.postInteractionEphemeral(ctx, callback, callback.Channel.ID, string(text))
		return
	}
	if b.api != nil && callback.User.ID != "" {
		if _, _, err := b.api.PostMessage(callback.User.ID, slack.MsgOptionText(string(text), false)); err != nil {
			b.logger.Warn("could not DM slack person refusal", "user", callback.User.ID, "error", err)
		}
	}
}
