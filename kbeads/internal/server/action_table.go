package server

import (
	"context"
	"maps"
	"strings"

	"gardenio/kbeads/internal/model"
)

// tier is a row of actionTable: the class of a scoped caller in one land.
// Boundary recommendation gd-uYNbyW4a9Z §6 step 3 (gd-QjqeHIEmWC). Today the
// rows are the land roles of an OIDC human (land_role.go) and the land
// principal, a per-land service token (land_principal.go, 10.11
// gd-x0kJF1wHk7); a narrower human (the lands-tenant tier, 10.1
// gd-g77WcV22SK) is a new row.
//
// To add a tier: a const here, a row in actionTable, a row in the reviewed
// decisions of TestActionTable_AllTypesAllTiers, and the place its caller is
// resolved to it (scopeFromContext, which fills scopeDecision.Tiers).
type tier string

const (
	tierOwner  tier = "owner"
	tierMember tier = "member"
	tierGuest  tier = "guest"
	// tierLand is a land principal: the token a per-land process holds.
	tierLand tier = "land"
)

// tierOperator is what GET /v1/me reports for an unrestricted caller. It is
// not a row: an unrestricted caller is never checked against actionTable.
const tierOperator tier = "operator"

// tierRunner is what GET /v1/me reports for each project in a runner bearer's
// allowlist. It is not a row either: a runner is admitted by route
// (runnerRouteGate) and bound to its allowlist, never checked against
// actionTable. It claims no land role for the owner, whose groups a runner
// bearer does not carry, so a guest owner is never reported as more.
const tierRunner tier = "runner"

const typeMessage model.BeadType = "message"

// typeAgent is the bead type an agent spawn creates; actionSpawn on it is the
// spawn right (spawn_authz.go).
const typeAgent model.BeadType = "agent"

// Actions that only one route produces, beside the write verbs (create,
// update, close, delete, comment).
const (
	// scopedActionReact is a reactions-only update of a message bead. Only
	// updateBead produces it, so every other write route (field endpoints,
	// labels, dependencies, comments, close, delete) still sees "update" or its
	// own verb and is denied on a message.
	scopedActionReact = "react"
	// actionApprove is approving or rejecting an escalation
	// (scopedApproverAllowed).
	actionApprove = "approve"
	// actionSpawn is spawning an agent into a land (authorizeAgentRole).
	actionSpawn = "spawn"
	// actionStewardDelegate is the land owner setting land_steward_approves
	// on its land's project bead (escalation-routing D8), through
	// POST /v1/lands/{land}/steward-delegation. Owner tier only.
	actionStewardDelegate = "steward_delegate"
	// actionReleaseDeploy is reporting where one of the land's releases runs,
	// through POST /v1/releases/{id}/deployments (the Lands contract's
	// reportDeployment, releaseDeployDenial). Owner tier only: a land's
	// releases are recorded from its tags, never written by a tenant, and a
	// member may not say what runs in production.
	actionReleaseDeploy = "release_deploy"
)

type typeActions map[model.BeadType]map[string]bool

// memberActions is what a land member may do in its land. A type absent from
// it is denied for every action, so a bead type added later is denied until
// someone decides it belongs here; TestActionTable_AllTypesAllTiers iterates
// every known type and fails for one with no decision.
//
// A decision is respond-only (update/close, which resolve and cancel route
// through, and comment). An escalation is create-only, plus approval. A
// message is create-only (the Commons creates it already closed), plus react:
// a patch that changes nothing but the caller's own entries in
// fields.reactions (scopedUpdateAction, scopedReactionDenial). A channel is
// join-only: a patch that changes nothing but fields.members, checked by
// scopedMembershipDenial (channel_membership.go). A notification is
// create-only: a mention of a person in the caller's own message
// (notification.go). A doc is edit-Holds-only, and only the land's
// steward-notes bead: a patch that changes nothing but the Holds section of
// fields.content (scopedHoldsDenial, steward_notes_authz.go).
var memberActions = typeActions{
	model.TypeTask:       {"create": true, "update": true, "close": true, "comment": true},
	model.TypeBug:        {"create": true, "update": true, "close": true, "comment": true},
	model.TypeFeature:    {"create": true, "update": true, "close": true, "comment": true},
	model.TypeDecision:   {"update": true, "close": true, "comment": true},
	model.TypeEscalation: {"create": true, actionApprove: true},
	typeMessage:          {"create": true, scopedActionReact: true},
	typeAgent:            {actionSpawn: true},
	typeChannel:          {scopedActionJoin: true},
	typeNotification:     {"create": true},
	typeDoc:              {scopedActionEditHolds: true},
}

// ownerActions is memberActions plus the owner-only acts the daemon checks.
var ownerActions = withActions(memberActions, typeActions{
	model.TypeProject: {actionStewardDelegate: true},
	model.TypeRelease: {actionReleaseDeploy: true},
})

// withActions returns a deep copy of base with extra's actions added, so a
// tier's row can extend another's without mutating it.
func withActions(base, extra typeActions) typeActions {
	out := make(typeActions, len(base)+len(extra))
	for typ, acts := range base {
		out[typ] = maps.Clone(acts)
	}
	for typ, acts := range extra {
		if out[typ] == nil {
			out[typ] = map[string]bool{}
		}
		maps.Copy(out[typ], acts)
	}
	return out
}

// actionTable is actionTable[tier][type][action]: what a scoped caller of that
// tier may do to a bead of that type in the land the tier is for. Lands
// Commons spec §6.4 (gd-9aCcG1zCCY). The project gate (scopedProjectDenial)
// runs first, so a row only ever applies inside one of the caller's lands.
var actionTable = map[tier]typeActions{
	// An owner has no bead write a member lacks. Its daemon-checked
	// owner-only acts are steward_delegate on its land's project bead and
	// release_deploy on its land's releases; the others (channel_manage,
	// member_manage) are the controller's (conversation.Allows).
	tierOwner:  ownerActions,
	tierMember: memberActions,
	// A guest is an invited bystander (Q8c, gd-fPXoeHDQS3): it says things and
	// reacts, mentions people in what it says, and leaves a channel
	// (scopedMembershipDenial narrows join to leaving). Tasks, bugs, features, answering decisions, filing and
	// approving escalations and spawning are closed to it.
	tierGuest: {
		typeMessage:      {"create": true, scopedActionReact: true},
		typeChannel:      {scopedActionJoin: true},
		typeNotification: {"create": true},
	},
	// A land principal is a member less the acts that need a human: spawning
	// spends the land's purse (Q8c gives it to the member, gd-fPXoeHDQS3),
	// and approving an escalation or answering a decision is a human's call
	// (escalationApprover and decisionResolverAllowed refuse it as well). It
	// files a decision, which a member does not: the steward asks the land's
	// owner to approve, change or drop a plan (Track B design §4, steward
	// item 2; gd-mh9ANzCHIE), and a human answers it. A per-land process
	// that acts for a user forwards that user's bearer; an agent forwards its
	// per-agent token (land_door_agent.go).
	tierLand: {
		model.TypeTask:       {"create": true, "update": true, "close": true, "comment": true},
		model.TypeBug:        {"create": true, "update": true, "close": true, "comment": true},
		model.TypeFeature:    {"create": true, "update": true, "close": true, "comment": true},
		model.TypeDecision:   {"create": true, "comment": true},
		model.TypeEscalation: {"create": true},
		typeMessage:          {"create": true, scopedActionReact: true},
	},
}

// tierAllows is the one lookup every scoped write check makes. An unknown
// tier, type or action is denied.
func tierAllows(t tier, beadType model.BeadType, action string) bool {
	return actionTable[t][beadType][action]
}

// anyTierAllows reports whether some row of actionTable permits the action.
// createBead uses it (scopedTypeDenial) ahead of type resolution, so a type no
// tier may write is refused the same way whatever the caller's lands.
func anyTierAllows(beadType model.BeadType, action string) bool {
	for t := range actionTable {
		if tierAllows(t, beadType, action) {
			return true
		}
	}
	return false
}

// scopedTierDenial denies a scoped principal's action on a bead unless
// actionTable permits it for the caller's tier in every land the bead's
// project: labels name. It returns nil for every caller that is not a scoped
// principal. It does not check that those lands are the caller's: run
// scopedProjectDenial first, as scopedWriteDenial does.
func scopedTierDenial(ctx context.Context, action string, beadType model.BeadType, labels []string) *AuthzDenial {
	if !isScopedPrincipal(ctx) {
		return nil
	}
	scope := scopeFromContext(ctx)
	for _, l := range labels {
		p, ok := strings.CutPrefix(l, "project:")
		if !ok || p == "" {
			continue
		}
		if t := scope.Tiers[p]; !tierAllows(t, beadType, action) {
			return &AuthzDenial{
				Action:        action,
				BeadType:      string(beadType),
				RequiredScope: p,
				Message:       string(t) + " in land " + p + " may not " + action + " beads of type " + string(beadType),
			}
		}
	}
	return nil
}
