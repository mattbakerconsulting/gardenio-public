package bridge

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/oidcauth"
)

// Who may answer a permission prompt from Slack (gd-eemeNhUt5I). An Allow
// runs a tool in the agent's land, so the clicker must be an operator or a
// member of that land (conversation.ActionPermissionAnswer), the same rule
// the Commons applies, and the answer records who they are: the Slack user id
// they clicked as and the gardenio person that id is linked to, never the
// Slack username, which its owner can change at will.

// permissionAnswerer is who answered a permission prompt.
type permissionAnswerer struct {
	SlackID   string // the clicker's Slack user id
	Principal string // the person it is linked to, oidc:<email>
}

// LandAccessSource reports what the identity provider says of a person's
// standing in lands: their effective realm roles and the paths of the realm
// groups they are in, the same groups kbeads reads land roles from
// (conversation.LandRolesFromGroups). An unknown or disabled person has
// neither, and no error; a failure to ask is an error (callers fail closed).
type LandAccessSource interface {
	LandAccess(ctx context.Context, email string) (roles, groups []string, err error)
}

// landAccessSource is the Keycloak client that can answer LandAccess: the
// person-role source when it can, else the mention-gate checker. nil when
// the bridge has neither.
func (b *Bot) landAccessSource() LandAccessSource {
	if src, ok := b.personRoleSource().(LandAccessSource); ok {
		return src
	}
	if b.keycloakChecker != nil {
		return b.keycloakChecker
	}
	return nil
}

// permissionAnswerer resolves the Slack user slackUserID to a person and
// checks they may answer a permission request in land. A non-empty refusal
// means they may not, and says why.
func (b *Bot) permissionAnswerer(ctx context.Context, slackUserID, land string) (permissionAnswerer, slackPersonRefusal) {
	if slackUserID == "" {
		return permissionAnswerer{}, "I can't tell who clicked, so I did not answer this permission request."
	}
	email, refusal := b.permissionAnswererEmail(ctx, slackUserID)
	if refusal != "" {
		return permissionAnswerer{}, refusal
	}
	who := permissionAnswerer{SlackID: slackUserID, Principal: "oidc:" + email}

	role, refusal := b.permissionLandRole(ctx, email, land)
	if refusal != "" {
		return permissionAnswerer{}, refusal
	}
	if !conversation.Allows(role, conversation.ActionPermissionAnswer) {
		b.logger.Info("permission answer refused: not an operator or member of the land",
			"user", slackUserID, "principal", who.Principal, "land", land, "role", string(role))
		if land == "" {
			return permissionAnswerer{}, "Only an operator can answer this permission request."
		}
		return permissionAnswerer{}, slackPersonRefusal(fmt.Sprintf(
			"Only an operator or a member of land %s can answer this permission request.", land))
	}
	return who, ""
}

// permissionAnswererEmail is the email of the person slackUserID is linked
// to: the linked account's (withLinkedSlackActor stamped it on ctx), else the
// one open user bead carrying slackUserID as its slack_id. Two such beads are
// ambiguous and refused, as resolveSlackPerson does.
func (b *Bot) permissionAnswererEmail(ctx context.Context, slackUserID string) (string, slackPersonRefusal) {
	if b.slackLinker() != nil {
		if email, ok := strings.CutPrefix(beadsapi.OperatorFromContext(ctx), "oidc:"); ok && email != "" {
			return strings.ToLower(email), ""
		}
	}
	dir := b.UserDirectory()
	if dir.CountBySlackID(ctx, slackUserID) > 1 {
		b.logger.Error("permission answer refused: slack_id is carried by more than one open user bead", "user", slackUserID)
		return "", "Your Slack id is linked to more than one Gardenio person, so I can't tell who you are and did not answer. Ask an admin to fix the duplicate."
	}
	if u := dir.LookupBySlackID(ctx, slackUserID); u != nil {
		if email := strings.ToLower(strings.TrimSpace(u.Email)); email != "" {
			return email, ""
		}
	}
	b.logger.Info("permission answer refused: no person mapped to slack user", "user", slackUserID)
	return "", "Your Slack account isn't linked to a Gardenio person, so I can't answer this permission request for you. Ask an admin to add your Slack id to your user."
}

// permissionLandRole is the person's role in land, read as kbeads reads a
// token (isScopedPrincipal, scopeFromContext): gardenio-admin is the operator
// everywhere; a gardenio-project-member who is not an admin is confined to the
// lands their groups name; any other holder of gardenio-operator is the
// operator. With no identity provider to ask, a person mapped by slack_id
// stands for the operator in an operator land, as every Slack write did before,
// and for nobody in a tenant land, whose membership the bridge cannot see.
func (b *Bot) permissionLandRole(ctx context.Context, email, land string) (conversation.LandRole, slackPersonRefusal) {
	src := b.landAccessSource()
	if src == nil {
		kind, err := b.tenantKind(ctx, land)
		switch {
		case err != nil:
			b.logger.Error("permission answer refused: reading the land failed (failing closed)", "land", land, "error", err)
			return conversation.LandRoleNone, "I couldn't check this land right now, so I did not answer. Try again in a moment."
		case kind != "":
			b.logger.Info("permission answer refused: tenant land membership is unknowable without Keycloak", "land", land)
			return conversation.LandRoleNone, slackPersonRefusal(fmt.Sprintf(
				"I can't check who belongs to land %s, so permission requests there can't be answered from Slack.", land))
		}
		return conversation.LandRoleOperator, ""
	}
	roles, groups, err := src.LandAccess(ctx, email)
	if err != nil {
		b.logger.Error("permission answer refused: land access lookup failed (failing closed)", "principal", "oidc:"+email, "error", err)
		return conversation.LandRoleNone, "I couldn't verify your Gardenio roles right now, so I did not answer. Try again in a moment."
	}
	return landRoleFromToken(roles, groups, land), ""
}

// landRoleFromToken is a person's role in land from the realm roles and
// groups of their verified token, read as kbeads reads it (isScopedPrincipal,
// scopeFromContext): gardenio-admin is the operator everywhere; a
// gardenio-project-member who is not an admin is confined to the lands their
// groups name; any other holder of gardenio-operator is the operator.
func landRoleFromToken(roles, groups []string, land string) conversation.LandRole {
	switch {
	case oidcauth.HasMinimumRole(roles, oidcauth.RoleAdmin):
		return conversation.LandRoleOperator
	case slices.Contains(roles, oidcauth.RoleProjectMember):
		if land == "" {
			return conversation.LandRoleNone
		}
		return conversation.LandRolesFromGroups(groups)[land]
	case oidcauth.HasMinimumRole(roles, oidcauth.RoleOperator):
		return conversation.LandRoleOperator
	}
	return conversation.LandRoleNone
}
