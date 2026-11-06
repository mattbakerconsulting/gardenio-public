package server

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// The shared restricted-ops check (Lands Track B spec §5.1, step 10.1
// gd-g77WcV22SK). Every operation a restricted agent may be refused calls
// restrictedOpDenial with the op it is about to perform. Two things restrict
// an agent, and the check reads both:
//
//   - its role's capability tiers: an agent whose role holds lands-tenant is a
//     tenant agent, held to landsTenantRules below (the §5.1 table as data);
//   - its taints: an agent carrying a taint whose effects include restrict
//     (docs/adr/taints.md §15.7), and holding no grant for it, is refused the
//     ops in restrictTaintOps. Every bead in a tenant land carries such a
//     taint (untrusted-input, taint_untrusted_input.go), so an agent that read
//     text a stranger wrote carries it too.
//
// A denial is an AuthzDenial with DeniedReason lands_tenant or
// taint_restrict, never escalatable, whose message says where to ask
// instead, so an agent can explain it in the Commons.
//
// The §5.1 rows the daemon does not rule on are enforced where the resource
// is, and are named here so the table reads whole:
//
//   - reading and writing beads of another project: the per-agent token is
//     bound to the agent's project (beadMatchesTokenProject, scopeListFilter);
//   - posting outside the land (Slack, email, any external send): the bridge
//     admits a per-agent token only into its own land's channels
//     (gardenio/controller/internal/bridge fenceTarget). There is no
//     agent-facing email path; internal/mailer is only the Lands provisioner's
//     and the viewer's;
//   - repositories: a tenant pod holds a Lands App installation token for its
//     land's one repo and nothing else (spec §5.3, the pod allowlist 10.3).

// restrictedOp is one operation the shared check rules on.
type restrictedOp string

const (
	// opSpawn is putting an agent into a role: an agent bead create, or an
	// update that sets its role (authorizeAgentRole).
	opSpawn restrictedOp = "spawn"
	// opEscalationFile is filing an escalation bead directly (createBead).
	opEscalationFile restrictedOp = "escalation_file"
	// opEscalationRetry is turning a denied write into an escalation or a
	// grant request (X-Escalate, handleEscalation).
	opEscalationRetry restrictedOp = "escalation_retry"
	// opEscalationApprove and opEscalationReject are answering one
	// (authorizeEscalationVerb).
	opEscalationApprove restrictedOp = "escalation_approve"
	opEscalationReject  restrictedOp = "escalation_reject"
	// opEscalationReplay is running an approved escalation's operation as the
	// agent that requested it (replayEscalation). A privileged spawn by an
	// agent only ever runs this way.
	opEscalationReplay restrictedOp = "escalation_replay"
	// opTypeWrite is writing a bead of a type, or with an action, that
	// landsTenantWrites does not list.
	opTypeWrite restrictedOp = "type_write"
	// opGatehouseRequest is filing a gatehouse_request, the gatehouse
	// steward's apply and demo tools (gatehouse_request.go). The type is
	// admin-gated; this op is the one way a tenant agent files one.
	opGatehouseRequest restrictedOp = "gatehouse_request"
)

// tenantRule is what a lands-tenant agent may do for one op.
type tenantRule int

const (
	// tenantNever refuses the op outright.
	tenantNever tenantRule = iota
	// tenantStewardSpawnsGardener admits the steward putting an agent into
	// exactly lands-gardener in its own project.
	tenantStewardSpawnsGardener
	// tenantOwnLandMerge admits a merge escalation (github_pr_merge,
	// gitlab_mr_merge) whose one project is the agent's own land. The
	// escalation policy routes it to the steward or the land owner
	// (escalation-routing Task 11).
	tenantOwnLandMerge
	// tenantStewardOwnLandMerge is tenantOwnLandMerge for the steward only;
	// the approve path then holds it to the land owner's delegation (D4/D8,
	// checkMayApprove).
	tenantStewardOwnLandMerge
	// tenantGatehouseSteward admits the steward of the gatehouse land
	// (land_gatehouse) filing in its own land, only while the gatehouse
	// carries land_gatehouse_tools=true (gatehouseStewardDenial).
	tenantGatehouseSteward
)

// landsTenantRules is the "May" column of spec §5.1 for every op the daemon
// rules on. An op absent from it is refused, so a new op is closed to tenant
// agents until someone decides otherwise; TestLandsTenantRules_EveryOp pins
// the decisions.
var landsTenantRules = map[restrictedOp]tenantRule{
	opSpawn:             tenantStewardSpawnsGardener,
	opEscalationFile:    tenantOwnLandMerge,
	opEscalationRetry:   tenantNever,
	opEscalationApprove: tenantStewardOwnLandMerge,
	opEscalationReject:  tenantNever,
	opTypeWrite:         tenantNever,
	opGatehouseRequest:  tenantGatehouseSteward,
}

// restrictTaintOps are the ops a carried restrict taint refuses until the
// taint is lifted or granted (ADR §15.7). Replay is refused for every
// requester. Spawn, approve and a type write of a restrictTaintFleetTypes bead
// or a config key are refused only to an agent that is not a lands-tenant
// agent: for a tenant agent the
// tier rules them, and the steward always carries the taint, so the taint
// would close what the tier admits (gd-gaGdksntAG). Filing stays open, so a
// tainted agent may still ask (a human reads what it filed) and the merge desk
// can still move a tenant land's PRs after reading them; rejecting runs
// nothing and stays open too.
var restrictTaintOps = map[restrictedOp]bool{
	opEscalationReplay:  true,
	opSpawn:             true,
	opEscalationApprove: true,
	opTypeWrite:         true,
}

// landsTenantWrites is what a tenant agent may write in its own land, by type
// and action: the land's work and its own bookkeeping, nothing that
// configures agents, people or the fleet (§5.1 "any config bead, any user
// bead"). It is an allow-list, like actionTable: a type or action absent from
// it is refused, so a type added later, or one a bridge or the controller acts
// on (reaction-rule, widget-type, dashboard, schedule, ...), is closed to
// tenant agents until someone decides otherwise (gd-bRLiEzvMsy).
// TestLandsTenantWrites_EveryType pins the decisions.
//
// The rows are what the steward and lands-gardener write today. Each type's
// own gates still apply after this one: agent create is opSpawn, escalation
// create is opEscalationFile, doc is the steward-notes check, and a
// gatehouse_request is admin-gated but for opGatehouseRequest.
var landsTenantWrites = typeActions{
	model.TypeTask:     {"create": true, "update": true, "close": true, "comment": true},
	model.TypeBug:      {"create": true, "update": true, "close": true, "comment": true},
	model.TypeFeature:  {"create": true, "update": true, "close": true, "comment": true},
	model.TypeChore:    {"create": true, "update": true, "close": true, "comment": true},
	model.TypeEpic:     {"create": true, "update": true, "close": true, "comment": true},
	model.TypeDecision: {"create": true, "update": true, "close": true, "comment": true},
	// update is the filer's merge-spec amend; the filing fields stay frozen
	// for every caller (escalation_field_guard.go).
	model.TypeEscalation: {"create": true, "update": true, "close": true, "comment": true},
	// gb decision report, when a chosen option names an artifact.
	model.TypeReport: {"create": true, "update": true, "comment": true},
	model.TypeBundle: {"create": true, "update": true, "comment": true},
	typeMessage:      {"create": true, "update": true, "close": true, "comment": true},
	typeDoc:          {"create": true, "update": true, "comment": true},
	// Its own agent bead and session, and the gardeners the steward spawns
	// and stops.
	typeAgent:            {"create": true, "update": true, "close": true, "comment": true, "hook-emit": true},
	"session":            {"create": true, "update": true, "close": true},
	typeGatehouseRequest: {"create": true},
}

// Wire values of AuthzDenial.DeniedReason for this check.
const deniedReasonTaintRestrict = "taint_restrict"

// restrictedOpRequest is the op and what it acts on.
type restrictedOpRequest struct {
	Op restrictedOp
	// Action is the audited verb ("create", "update", "approve", ...).
	Action string
	// BeadType is the type the op writes (agent, escalation, ...).
	BeadType model.BeadType
	// Role and Project are the spawn target (opSpawn); Project is also the
	// land a gatehouse_request is filed in (opGatehouseRequest).
	Role, Project string
	// Escalation is the escalation filed or answered (opEscalationFile,
	// opEscalationApprove).
	Escalation *model.Bead
	// Agent is the agent the op runs as when that is not the caller
	// (opEscalationReplay: the requesting agent).
	Agent string
}

// restrictedOpDenial is the shared check. It returns nil when the op may run.
// Every lookup failure denies.
func (s *BeadsServer) restrictedOpDenial(ctx context.Context, req restrictedOpRequest) *AuthzDenial {
	if req.Op != opEscalationReplay {
		if d := landsTenantOpDenial(ctx, s.store, req); d != nil {
			return d
		}
	}
	return s.restrictTaintOpDenial(ctx, req)
}

// restrictTaintOpDenial is the taint half of the shared check, for a gate
// that applies the tenant half itself (the spawn and write gates). An agent
// whose role holds the tier is exempt from every op but replay
// (restrictTaintOps); a ByLand caller keeps its taint, since not every gate
// that calls this applies the tier first (gd-PtbbUxKWds).
func (s *BeadsServer) restrictTaintOpDenial(ctx context.Context, req restrictedOpRequest) *AuthzDenial {
	if !restrictTaintOps[req.Op] || (req.Op == opTypeWrite && !restrictTaintFleetTypes[req.BeadType]) {
		return nil
	}
	d := s.restrictTaintDenial(ctx, req)
	if d == nil || req.Op == opEscalationReplay {
		return d
	}
	caller, err := landsTenantCallerFrom(ctx, s.store)
	if err != nil {
		return landsTenantLookupDenial(req.Action, req.BeadType, err)
	}
	if caller != nil && !caller.Home && !caller.ByLand {
		return nil
	}
	return d
}

// landsTenantOpDenial applies landsTenantRules to the caller. nil for a caller
// that is not a tenant agent. It needs only the store, so the package-level
// spawn gate (authorizeAgentRole) calls it too.
func landsTenantOpDenial(ctx context.Context, st store.Store, req restrictedOpRequest) *AuthzDenial {
	caller, err := landsTenantCallerFrom(ctx, st)
	if err != nil {
		return landsTenantLookupDenial(req.Action, req.BeadType, err)
	}
	if caller == nil || caller.Home {
		return nil
	}
	switch rule := landsTenantRules[req.Op]; rule {
	case tenantStewardSpawnsGardener:
		return landsTenantSpawnTargetDenial(ctx, st, caller, req.Action, req.Role, req.Project)
	case tenantGatehouseSteward:
		return gatehouseStewardDenial(ctx, st, caller, req)
	case tenantOwnLandMerge, tenantStewardOwnLandMerge:
		if rule == tenantStewardOwnLandMerge && !caller.isSteward() {
			break
		}
		if ownLandMerge(caller, req.Escalation) {
			return nil
		}
		if rule == tenantOwnLandMerge {
			return landsTenantDenial(req.Action, req.BeadType, "escalation "+req.Action+" denied: "+caller.holder()+
				" holds the lands-tenant tier and files only a merge escalation (github_pr_merge, gitlab_mr_merge) about its own land "+strconvQuote(caller.Project))
		}
	case tenantNever:
		if req.Op == opTypeWrite {
			return landsTenantDenial(req.Action, req.BeadType, req.Action+" denied: "+caller.holder()+
				" holds the lands-tenant tier and may not "+req.Action+" beads of type "+string(req.BeadType)+": a tenant agent writes only its land's work")
		}
	}
	return landsTenantDenial(req.Action, req.BeadType, "escalation "+req.Action+" denied: "+caller.holder()+" holds the lands-tenant tier")
}

// ownLandMerge reports whether esc is a merge escalation whose one project
// label is the caller's land.
func ownLandMerge(caller *landsTenantCaller, esc *model.Bead) bool {
	if esc == nil || caller.Project == "" {
		return false
	}
	if !stewardDelegableMerges[normalizeOperationType(parseSpawnEscalationFields(esc).OperationType)] {
		return false
	}
	projects := projectLabelValues(esc.Labels)
	return len(projects) == 1 && projects[0] == caller.Project
}

// landsTenantTypeDenial is the opTypeWrite check for the write gate. It looks
// the caller up only for a write landsTenantWrites does not list, so an
// ordinary write costs nothing.
func landsTenantTypeDenial(ctx context.Context, st store.Store, action string, beadType model.BeadType) *AuthzDenial {
	if landsTenantWrites[beadType][action] {
		return nil
	}
	if _, ok := agentBeadIDFromContext(ctx); !ok {
		return nil
	}
	return landsTenantOpDenial(ctx, st, restrictedOpRequest{Op: opTypeWrite, Action: action, BeadType: beadType})
}

// restrictTaintDenial refuses req.Agent (or, when empty, the calling agent)
// the op while it carries a restrict taint it holds no grant for (ADR
// docs/adr/taints.md §15.3: a grant is a toleration). Only a grant to the
// agent itself counts, never its spawn-chain root's (§15.7). An agent
// carrying no taint costs one cached taint-context read.
func (s *BeadsServer) restrictTaintDenial(ctx context.Context, req restrictedOpRequest) *AuthzDenial {
	agent := req.Agent
	if agent == "" {
		agent, _ = resolveTaintAgentBeadID(ctx)
	} else {
		ctx = WithTaintAttributionAgent(ctx, agent)
	}
	if agent == "" {
		return nil
	}
	held, err := s.taintContextOf(ctx)
	if err != nil {
		return restrictTaintLookupDenial(req, err)
	}
	if len(held) == 0 {
		return nil
	}
	all, err := s.store.ListTaints(ctx)
	if err != nil {
		return restrictTaintLookupDenial(req, err)
	}
	var restricting []int64
	names := map[int64]string{}
	for _, t := range all {
		if held[t.ID] && model.HasEffect(t.Effects, model.EffectRestrict) {
			restricting = append(restricting, t.ID)
			names[t.ID] = t.Name
		}
	}
	if len(restricting) == 0 {
		return nil
	}
	grants, err := s.agentOwnGrants(ctx, agent)
	if err != nil {
		return restrictTaintLookupDenial(req, err)
	}
	var carried []string
	for _, id := range restricting {
		if !slices.Contains(grants, id) {
			carried = append(carried, names[id])
		}
	}
	if len(carried) == 0 {
		return nil
	}
	sort.Strings(carried)
	return &AuthzDenial{
		Action:       req.Action,
		BeadType:     string(req.BeadType),
		DeniedReason: deniedReasonTaintRestrict,
		TaintNames:   carried,
		Message: fmt.Sprintf("%s denied: agent %s carries the restrict taint %s (it read untrusted input); "+
			"a reviewer lifts the taint from the agent bead, or the taint's owner grants it to the agent",
			req.Op, agent, strings.Join(carried, ", ")),
	}
}

func restrictTaintLookupDenial(req restrictedOpRequest, err error) *AuthzDenial {
	return &AuthzDenial{
		Action:   req.Action,
		BeadType: string(req.BeadType),
		Message:  "denied: could not verify the agent's restrict taints (" + err.Error() + "); retry",
	}
}
