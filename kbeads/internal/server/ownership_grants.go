package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"

	"gardenio/kbeads/internal/model"
)

// The ownership grant table (design kbeads/docs/ownership-grants.md, plan
// gd-fRmqrpHIwk). authorizeOwnership admits a write to a non-config bead only
// when a row here admits it; the base rule is a row too. A role's write reach
// is rows on its role bead (ownership_grants), so a new role grant is config,
// not code (step 5, gd-8IcxHwgGF5).

// ownershipGrant is one row of the grant table: it admits Actions on beads of
// Types (carrying Label, changing only Fields) for Principal, when Condition
// holds. Rows only admit; no row denies, so their order never changes a verdict.
type ownershipGrant struct {
	ID         string         // stable, unique: "static-token", "machine:trigger-ops/ownership", "role:herder/0"
	Principal  grantPrincipal // who
	Actions    []string       // "update", "close", "delete", "hook-emit"; never empty
	Types      grantTypes     // explicit list, or All with an Except list
	Label      string         // optional: the bead must carry this label
	Fields     []string       // optional, update only: the patch changes nothing else
	Condition  string         // optional: a name in ownershipConditions
	Privileged bool           // optional raise; ownershipGrantPrivileged computes the floor
	Origin     string         // the bead that added it
}

// grantPrincipal names who a row admits. Kind is one of the grantKind*
// constants; MinRole is the realm role floor for oidc and edge-v2; Name is the
// gardenio-machine-* role for machine, and the role bead title for agent.
type grantPrincipal struct {
	Kind    string
	MinRole string
	Name    string
}

// grantTypes is the bead types a row reaches: an explicit list, or All minus
// an Except list.
type grantTypes struct {
	All    bool
	Only   []model.BeadType
	Except []model.BeadType // only with All
}

func (t grantTypes) matches(bt model.BeadType) bool {
	if t.All {
		return !slices.Contains(t.Except, bt)
	}
	return slices.Contains(t.Only, bt)
}

// The principal kinds. A caller resolves to the SET of kinds whose test it
// passes (resolveGrantCaller), from AuthIdentity and the auth method, never
// from a header.
const (
	grantKindAny             = "any"              // every caller
	grantKindNone            = "none"             // Subject == "" (no-auth, anonymous)
	grantKindOIDC            = "oidc"             // an OIDC bearer
	grantKindEdgeV2          = "edge-v2"          // a verified v2 or burn-in edge envelope (role-bearing)
	grantKindEdgeV1          = "edge-v1"          // a verified v1 edge envelope (no roles)
	grantKindFleet           = "fleet"            // any fleet token, per-agent ones included
	grantKindFleetBridge     = "fleet-bridge"     // a bridge fleet (isBridgeFleetIdentity)
	grantKindFleetPrivileged = "fleet-privileged" // a fleet in BEADS_PRIVILEGED_FLEETS
	grantKindAgent           = "agent"            // a per-agent token
	grantKindMachine         = "machine"          // a machine principal
	grantKindRunner          = "runner"           // a runner principal
)

// ownershipActions are every action that reaches authorizeOwnership.
var ownershipActions = []string{"update", "close", "delete", "hook-emit"}

// ownershipUpdateClose is the action set of the grants that never delete.
var ownershipUpdateClose = []string{"update", "close"}

// grantCaller is the request's caller, resolved once per evaluation.
type grantCaller struct {
	id           AuthIdentity
	kinds        map[string]bool
	roles        []string // verified realm roles (oidc, edge-v2)
	machineRoles []string // gardenio-machine-* roles
}

// resolveGrantCaller resolves the caller on ctx to its set of kinds.
func resolveGrantCaller(ctx context.Context) grantCaller {
	id := AuthIdentityFromContext(ctx)
	method := authMethodFromContext(ctx)
	k := map[string]bool{grantKindAny: true}
	if id.Subject == "" {
		k[grantKindNone] = true
	}
	switch id.Type {
	case "oidc":
		k[grantKindOIDC] = true
	case "edge":
		// edge_auth.go stamps a verified v2 (and a burn-in) envelope as
		// authMethodOIDC with its real roles, and a verified v1 as authMethodEdge.
		if roleBearing(method) {
			k[grantKindEdgeV2] = true
		} else if method == authMethodEdge {
			k[grantKindEdgeV1] = true
		}
	case "fleet":
		k[grantKindFleet] = true
		if isBridgeFleetIdentity(FleetFromContext(ctx), id.Subject) {
			k[grantKindFleetBridge] = true
		}
		if id.Privileged {
			k[grantKindFleetPrivileged] = true
		}
		if _, ok := agentBeadIDFromContext(ctx); ok {
			k[grantKindAgent] = true
		}
	case "machine":
		if method == authMethodMachine {
			k[grantKindMachine] = true
		}
	}
	if method == authMethodRunner {
		k[grantKindRunner] = true
	}
	return grantCaller{id: id, kinds: k, roles: RolesFromContext(ctx), machineRoles: machineRolesFromContext(ctx)}
}

// matches reports whether the caller passes the row's principal test. An
// agent row with a Name is only ever built from the caller's own role beads
// (roleOwnershipGrants), so the name holds by construction.
func (c grantCaller) matches(p grantPrincipal) bool {
	if !c.kinds[p.Kind] {
		return false
	}
	if p.MinRole != "" && !hasMinimumRole(c.roles, p.MinRole) {
		return false
	}
	if p.Kind == grantKindMachine && p.Name != "" {
		return slices.Contains(c.machineRoles, p.Name)
	}
	return true
}

// matchesBead reports whether the row's bead side (action, type, label,
// fields) covers this write. authorizeOwnership never sees the patch, so a row
// with a field list does not match here; those rows are evaluated on the
// update path (machineMayStampFields, gardenerMayIdleOwnTrigger,
// homeStewardMayRearm) and are in the table for the golden.
func (g *ownershipGrant) matchesBead(action string, b *model.Bead) bool {
	if len(g.Fields) > 0 || !slices.Contains(g.Actions, action) || !g.Types.matches(b.Type) {
		return false
	}
	return g.Label == "" || slices.Contains(b.Labels, g.Label)
}

// ownershipCondition is the dynamic part of a row. Eval reads the store, never
// writes, and fails closed: an error is "does not hold" and is counted
// (gardenio_authz_ownership_condition_errors_total{condition}). A condition
// whose admission leaves a record (the legacy fall-through counter, the
// plan-scope history row) returns it as onAdmit; authorizeOwnership runs it
// once that row has admitted the write.
//
// CallerBound marks a condition that ties the bead to the caller's own
// identity, plan, land or escalation; only those may narrow a role row. Cheap
// marks one that reads no store, so the evaluator tries its rows first.
type ownershipCondition struct {
	CallerBound bool
	Cheap       bool
	Eval        func(ctx context.Context, s *BeadsServer, c grantCaller, action string, b *model.Bead) (held bool, onAdmit func(), err error)
}

// planScopeHeld adapts a plan-scope check to a condition result: an admission
// writes its authz.plan_scope history row (recordPlanScopeAdmission).
func planScopeHeld(ctx context.Context, s *BeadsServer, action string, b *model.Bead, adm planScopeAdmission, ok bool) (bool, func(), error) {
	if !ok {
		return false, nil, nil
	}
	return true, func() { s.recordPlanScopeAdmission(ctx, action, b, adm) }, nil
}

// ownershipConditions is the closed registry of condition names.
var ownershipConditions = map[string]ownershipCondition{
	// 11: created_by, assignee or owner equals the subject.
	"caller_owns_bead": {CallerBound: true, Cheap: true, Eval: func(_ context.Context, _ *BeadsServer, c grantCaller, _ string, b *model.Bead) (bool, func(), error) {
		a := c.id.Subject
		return a != "" && (b.CreatedBy == a || b.Assignee == a || b.Owner == a), nil, nil
	}},
	// 10: all three ownership fields empty. Each admission is counted
	// (ka-0000000671) so the row can go once the rate is known to be zero.
	"bead_unowned": {Cheap: true, Eval: func(_ context.Context, _ *BeadsServer, _ grantCaller, action string, b *model.Bead) (bool, func(), error) {
		if b.CreatedBy != "" || b.Assignee != "" || b.Owner != "" {
			return false, nil, nil
		}
		return true, func() { recordAuthzLegacyFallthrough(action, string(b.Type)) }, nil
	}},
	// 12: created_by == "<name>-bridge" for the subject bridge:<name>.
	"bridge_created_legacy": {CallerBound: true, Cheap: true, Eval: func(_ context.Context, _ *BeadsServer, c grantCaller, _ string, b *model.Bead) (bool, func(), error) {
		return b.CreatedBy != "" && bridgeSubjectMatchesLegacyCreatedBy(c.id.Subject, b.CreatedBy), nil, nil
	}},
	// 8: decisionResolverAllowed.
	"decision_caller_may_answer": {Cheap: true, Eval: func(ctx context.Context, _ *BeadsServer, _ grantCaller, _ string, _ *model.Bead) (bool, func(), error) {
		return decisionCallerMayAnswer(ctx), nil, nil
	}},
	// 13: operated_by, through the operators alias table.
	"caller_operates_bead": {CallerBound: true, Eval: func(ctx context.Context, s *BeadsServer, c grantCaller, _ string, b *model.Bead) (bool, func(), error) {
		return b.OperatedBy != "" && s.callerIsBeadOperator(ctx, c.id, b), nil, nil
	}},
	// 5: assigneeIsGoneAgent.
	"assignee_gone_agent": {Eval: func(ctx context.Context, s *BeadsServer, _ grantCaller, _ string, b *model.Bead) (bool, func(), error) {
		return s.assigneeIsGoneAgent(ctx, b.Assignee), nil, nil
	}},
	// 5: assigneeWasReapedAgent, the gone-agent reclaim kept across the reap.
	"assignee_reaped_agent": {Eval: func(ctx context.Context, s *BeadsServer, _ grantCaller, _ string, b *model.Bead) (bool, func(), error) {
		return s.assigneeWasReapedAgent(ctx, b), nil, nil
	}},
	// 16: doc steward-notes:<land> with project:<land>, caller is the land's steward.
	"steward_notes_of_callers_land": {CallerBound: true, Eval: func(ctx context.Context, s *BeadsServer, _ grantCaller, action string, b *model.Bead) (bool, func(), error) {
		return stewardOwnsNotes(ctx, s.store, action, b), nil, nil
	}},
	// 17: the assignee, thread agent or gardener binding (plan_scope_authz.go).
	"in_callers_plan": {CallerBound: true, Eval: func(ctx context.Context, s *BeadsServer, _ grantCaller, action string, b *model.Bead) (bool, func(), error) {
		adm, ok := s.authorizePlanScope(ctx, action, b)
		return planScopeHeld(ctx, s, action, b, adm, ok)
	}},
	// 17: the reactor: beads in or under any plan.
	"plan_reactor_scope": {Eval: func(ctx context.Context, s *BeadsServer, _ grantCaller, action string, b *model.Bead) (bool, func(), error) {
		if isGardenerBead(b.Type, b.Labels, b.Fields) {
			return false, nil, nil
		}
		adm, ok := s.authorizePlanReactor(ctx, action, b)
		return planScopeHeld(ctx, s, action, b, adm, ok)
	}},
	// 17: the reactor: the plan's gardener, plan open, same project.
	"plan_reactor_gardener": {Eval: func(ctx context.Context, s *BeadsServer, _ grantCaller, action string, b *model.Bead) (bool, func(), error) {
		if !isGardenerBead(b.Type, b.Labels, b.Fields) {
			return false, nil, nil
		}
		adm, ok := s.authorizePlanReactor(ctx, action, b)
		return planScopeHeld(ctx, s, action, b, adm, ok)
	}},
	// 18: escalationCloseGrant. Its close_authority row is written by
	// authorizeWriteForBead once every gate has passed.
	"named_by_approved_close_escalation": {CallerBound: true, Eval: func(ctx context.Context, s *BeadsServer, _ grantCaller, action string, b *model.Bead) (bool, func(), error) {
		return s.escalationCloseGrant(ctx, action, b) != "", nil, nil
	}},
	// 19: the claimed, approved escalation that lists the bead in
	// approved_targets (escalation_executor_grant.go).
	"listed_by_claimed_approved_escalation": {CallerBound: true, Eval: func(ctx context.Context, s *BeadsServer, _ grantCaller, action string, b *model.Bead) (bool, func(), error) {
		escID, err := s.escalationExecutorGrant(ctx, b)
		if err != nil || escID == "" {
			return false, nil, err
		}
		return true, func() {
			slog.Info("write by the executor of an approved escalation that lists the bead",
				"bead", b.ID, "action", action, "escalation", escID, "executor", actorFromContext(ctx))
		}, nil
	}},
	// 23: the home steward executing its project's approved advice escalation
	// that lists the bead in approved_targets (escalation_steward_advice.go).
	"home_steward_executes_approved_advice": {CallerBound: true, Eval: func(ctx context.Context, s *BeadsServer, _ grantCaller, action string, b *model.Bead) (bool, func(), error) {
		escID, err := s.stewardAdviceEscalation(ctx, action, b.Labels, b.ID)
		if err != nil || escID == "" {
			return false, nil, err
		}
		return true, func() {
			slog.Info("advice write by the home steward executing an approved advice escalation",
				"bead", b.ID, "action", action, "escalation", escID, "steward", actorFromContext(ctx))
		}, nil
	}},
	// 20: the caller is an iteration of this gardener trigger (gardener_self_idle.go).
	"callers_own_gardener_trigger": {CallerBound: true, Eval: func(ctx context.Context, s *BeadsServer, _ grantCaller, _ string, b *model.Bead) (bool, func(), error) {
		return s.callerRunsGardenerTrigger(ctx, b), nil, nil
	}},
	// 22: the home steward on an engine-closed, sealed plan gardener of a
	// building plan in its land (spawn_lineage_rearm.go).
	"home_steward_rearms_plan_gardener": {CallerBound: true, Eval: func(ctx context.Context, s *BeadsServer, _ grantCaller, _ string, b *model.Bead) (bool, func(), error) {
		return s.homeStewardRearmsPlanGardener(ctx, b), nil, nil
	}},
	// 21: the caller's agent bead names the bead (or its work-item parent) in task_id.
	"caller_dispatched_onto_bead": {CallerBound: true, Eval: func(ctx context.Context, s *BeadsServer, _ grantCaller, _ string, b *model.Bead) (bool, func(), error) {
		held, err := s.callerDispatchedOnto(ctx, b)
		return held, nil, err
	}},
	// 22: an open, unassigned bead a live single-claim agent was dispatched onto.
	// Its row carries a field list, so it is evaluated on the update path with
	// the patch (machineMayClaimDispatchedTask), never here.
	"dispatched_agent_claims_unassigned": {Eval: func(context.Context, *BeadsServer, grantCaller, string, *model.Bead) (bool, func(), error) {
		return false, nil, nil
	}},
}

// allExceptDecision is the type set of the grants that never reach a decision
// (gd-Du6wHclx4g).
var allExceptDecision = grantTypes{All: true, Except: []model.BeadType{model.TypeDecision}}

// planScopeTypeList is planScopeTypes as a sorted list.
func planScopeTypeList() []model.BeadType {
	out := make([]model.BeadType, 0, len(planScopeTypes))
	for t := range planScopeTypes {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

// ownershipGrantRows are the code rows: every branch of authorizeOwnership
// that is not a machine grant or a role bead field. The branch numbers are the
// inventory's (kbeads/docs/ownership-grants.md).
func ownershipGrantRows() []ownershipGrant {
	all := grantTypes{All: true}
	escalation := grantTypes{Only: []model.BeadType{model.TypeEscalation}}
	decision := grantTypes{Only: []model.BeadType{model.TypeDecision}}
	rows := []ownershipGrant{
		// 2: admin.
		{ID: "admin-oidc", Principal: grantPrincipal{Kind: grantKindOIDC, MinRole: RoleAdmin}, Actions: ownershipActions, Types: all, Origin: "B6"},
		{ID: "admin-edge", Principal: grantPrincipal{Kind: grantKindEdgeV2, MinRole: RoleAdmin}, Actions: ownershipActions, Types: all, Origin: "B6"},
		// 3: the shared static token, retired (gd-ke6PC7bRto): no row.
		// 6: a privileged service fleet.
		{ID: "fleet-privileged", Principal: grantPrincipal{Kind: grantKindFleetPrivileged}, Actions: ownershipActions, Types: all, Origin: "ka-0000001946"},
		// 7: the DM carve-out. Admits delete today (known widening, finding 5).
		{ID: "fleet-dm", Principal: grantPrincipal{Kind: grantKindFleet}, Actions: ownershipActions, Types: grantTypes{Only: []model.BeadType{"dm"}}, Origin: "ka-0000005087"},
		// 8: the escalation queue, one row per kind (finding 6).
		{ID: "queue-escalation-fleet", Principal: grantPrincipal{Kind: grantKindFleet}, Actions: ownershipActions, Types: escalation, Origin: "ka-0000002032"},
		{ID: "queue-escalation-machine", Principal: grantPrincipal{Kind: grantKindMachine}, Actions: ownershipActions, Types: escalation, Origin: "ka-0000002032"},
		{ID: "queue-escalation-edge-v1", Principal: grantPrincipal{Kind: grantKindEdgeV1}, Actions: ownershipActions, Types: escalation, Origin: "ka-0000002032"},
		{ID: "queue-escalation-runner", Principal: grantPrincipal{Kind: grantKindRunner}, Actions: ownershipActions, Types: escalation, Origin: "ka-0000002032"},
		{ID: "queue-escalation-oidc", Principal: grantPrincipal{Kind: grantKindOIDC, MinRole: RoleOperator}, Actions: ownershipActions, Types: escalation, Origin: "ka-0000002032"},
		{ID: "queue-escalation-edge-v2", Principal: grantPrincipal{Kind: grantKindEdgeV2, MinRole: RoleOperator}, Actions: ownershipActions, Types: escalation, Origin: "ka-0000002032"},
		// 8: the decision queue, narrowed to the resolvers.
		{ID: "queue-decision-oidc", Principal: grantPrincipal{Kind: grantKindOIDC, MinRole: RoleOperator}, Actions: ownershipActions, Types: decision, Condition: "decision_caller_may_answer", Origin: "ka-0000000798"},
		{ID: "queue-decision-edge-v2", Principal: grantPrincipal{Kind: grantKindEdgeV2, MinRole: RoleOperator}, Actions: ownershipActions, Types: decision, Condition: "decision_caller_may_answer", Origin: "ka-0000000798"},
		{ID: "queue-decision-edge-v1", Principal: grantPrincipal{Kind: grantKindEdgeV1}, Actions: ownershipActions, Types: decision, Condition: "decision_caller_may_answer", Origin: "ka-0000000798"},
		{ID: "queue-decision-machine", Principal: grantPrincipal{Kind: grantKindMachine}, Actions: ownershipActions, Types: decision, Condition: "decision_caller_may_answer", Origin: "ka-0000000798"},
		{ID: "queue-decision-fleet-bridge", Principal: grantPrincipal{Kind: grantKindFleetBridge}, Actions: ownershipActions, Types: decision, Condition: "decision_caller_may_answer", Origin: "gd-Du6wHclx4g"},
		// 9: no identity; the outer middleware decides.
		{ID: "anonymous", Principal: grantPrincipal{Kind: grantKindNone}, Actions: ownershipActions, Types: all, Origin: "B6"},
		// 10: a legacy bead with no ownership fields.
		{ID: "legacy-unowned", Principal: grantPrincipal{Kind: grantKindAny}, Actions: ownershipActions, Types: all, Condition: "bead_unowned", Origin: "ka-0000000671"},
		// 11: the base rule.
		{ID: "owner", Principal: grantPrincipal{Kind: grantKindAny}, Actions: ownershipActions, Types: all, Condition: "caller_owns_bead", Origin: "B6"},
		// 12: a bridge's own beads across the fleet-name rename.
		{ID: "bridge-legacy-creator", Principal: grantPrincipal{Kind: grantKindFleet}, Actions: ownershipActions, Types: all, Condition: "bridge_created_legacy", Origin: "ka-0000007614"},
		// 13: the operator the bead was filed for.
		{ID: "operator-of-bead", Principal: grantPrincipal{Kind: grantKindAny}, Actions: ownershipActions, Types: all, Condition: "caller_operates_bead", Origin: "ka-0000007637"},
		// 16: a land's steward on its notes doc.
		{ID: "steward-notes", Principal: grantPrincipal{Kind: grantKindAgent}, Actions: []string{"update"}, Types: grantTypes{Only: []model.BeadType{typeDoc}}, Condition: "steward_notes_of_callers_land", Origin: "gd-mN1TLhGpGe"},
		// 17: a plan's own agents.
		{ID: "plan-scope", Principal: grantPrincipal{Kind: grantKindAgent}, Actions: ownershipUpdateClose, Types: grantTypes{Only: planScopeTypeList()}, Condition: "in_callers_plan", Origin: "gd-Mk64Bko3XO"},
		// 18: the executor of an approved close escalation.
		{ID: "escalation-close", Principal: grantPrincipal{Kind: grantKindAgent}, Actions: []string{"close"}, Types: allExceptDecision, Condition: "named_by_approved_close_escalation", Origin: "gd-Ti5JGqdhpd"},
		// 20: a plan's gardener idling its own trigger until a bead closes;
		// checked on the update path (gardenerMayIdleOwnTrigger).
		{ID: gardenerSelfIdleGrant, Principal: grantPrincipal{Kind: grantKindAgent}, Actions: []string{"update"}, Types: grantTypes{Only: []model.BeadType{"schedule", "trigger"}}, Fields: gardenerSelfIdleFields, Condition: "callers_own_gardener_trigger", Origin: "gd-V68T7v5JPP"},
		// 22: the home steward re-arming a plan's sealed gardener; checked on
		// the update path (homeStewardMayRearm).
		{ID: stewardRearmGrant, Principal: grantPrincipal{Kind: grantKindAgent}, Actions: []string{"update"}, Types: grantTypes{Only: []model.BeadType{"schedule", "trigger"}}, Fields: stewardRearmFields, Condition: "home_steward_rearms_plan_gardener", Origin: "gd-miuZqFACwL"},
		// 23: the home steward executing its project's approved advice escalation.
		{ID: stewardAdviceGrant, Principal: grantPrincipal{Kind: grantKindAgent}, Actions: ownershipUpdateClose, Types: grantTypes{Only: []model.BeadType{model.TypeAdvice}}, Condition: "home_steward_executes_approved_advice", Origin: "gd-kEis8bURm5"},
		// 21: a crew on the task it was dispatched onto.
		{ID: "dispatched-task", Principal: grantPrincipal{Kind: grantKindAgent}, Actions: ownershipUpdateClose, Types: grantTypes{Only: dispatchedTaskTypes}, Condition: "caller_dispatched_onto_bead", Origin: "gd-GrJRP7kLBY"},
	}
	return rows
}

// machineGrantRows derives the machine rows from machineCapabilityGrants, which
// stays their one source (branches 4a, 4b, 5, 17 and FieldOwnership).
func machineGrantRows() []ownershipGrant {
	roles := make([]string, 0, len(machineCapabilityGrants))
	for role := range machineCapabilityGrants {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	var rows []ownershipGrant
	for _, role := range roles {
		g := machineCapabilityGrants[role]
		short := "machine:" + strings.TrimPrefix(role, "gardenio-machine-")
		p := grantPrincipal{Kind: grantKindMachine, Name: role}
		if len(g.Ownership) > 0 {
			rows = append(rows, ownershipGrant{ID: short + "/ownership", Principal: p, Actions: ownershipActions,
				Types: grantTypes{Only: sortedTypes(g.Ownership)}, Origin: "gd-Hc34EENbO9"})
		}
		for i, l := range g.LabeledOwnership {
			rows = append(rows, ownershipGrant{ID: short + "/labeled/" + strconv.Itoa(i), Principal: p, Actions: l.Actions,
				Types: grantTypes{Only: []model.BeadType{l.BeadType}}, Label: l.Label, Origin: "gd-Hc34EENbO9"})
		}
		if len(g.ReclaimOwnership) > 0 {
			rows = append(rows, ownershipGrant{ID: short + "/reclaim", Principal: p, Actions: []string{"update"},
				Types: grantTypes{Only: sortedTypes(g.ReclaimOwnership)}, Condition: "assignee_gone_agent", Origin: "gd-RZJk5gspFv"},
				ownershipGrant{ID: short + "/reclaim-reaped", Principal: p, Actions: []string{"update"},
					Types: grantTypes{Only: sortedTypes(g.ReclaimOwnership)}, Condition: "assignee_reaped_agent", Origin: "gd-8Irb2vDFf7"})
		}
		if len(g.DispatchClaim) > 0 {
			rows = append(rows, ownershipGrant{ID: short + "/dispatch-claim", Principal: p, Actions: []string{"update"},
				Types: grantTypes{Only: sortedTypes(g.DispatchClaim)}, Fields: []string{"assignee", "status"},
				Condition: "dispatched_agent_claims_unassigned", Origin: "gd-GrJRP7kLBY"})
		}
		for i, f := range g.FieldOwnership {
			rows = append(rows, ownershipGrant{ID: short + "/fields/" + strconv.Itoa(i), Principal: p, Actions: []string{"update"},
				Types: grantTypes{Only: []model.BeadType{f.BeadType}}, Fields: f.Fields, Origin: "gd-Hk70sS1XuS"})
		}
		if g.EscalationExecutor {
			rows = append(rows, ownershipGrant{ID: short + "/approved-targets", Principal: p, Actions: ownershipUpdateClose,
				Types: grantTypes{Only: escalationExecutorTypeList()}, Condition: "listed_by_claimed_approved_escalation", Origin: "gd-sZNUPjzygT"},
				ownershipGrant{ID: short + "/approved-close", Principal: p, Actions: []string{"close"},
					Types: grantTypes{Only: escalationCloseOnlyTypeList()}, Condition: "listed_by_claimed_approved_escalation", Origin: "gd-dNYrSP9KkW"})
		}
		if g.PlanScope {
			rows = append(rows,
				ownershipGrant{ID: short + "/plan-scope", Principal: p, Actions: ownershipUpdateClose,
					Types: grantTypes{Only: planScopeTypeList()}, Condition: "plan_reactor_scope", Origin: "gd-Dl88Htipuq"},
				ownershipGrant{ID: short + "/plan-gardener", Principal: p, Actions: []string{"update"},
					Types: grantTypes{Only: []model.BeadType{"schedule", "trigger"}}, Condition: "plan_reactor_gardener", Origin: "gd-Dl88Htipuq"})
		}
	}
	return rows
}

// escalationExecutorTypeList is escalationExecutorTypes as a sorted list.
func escalationExecutorTypeList() []model.BeadType { return sortedTypeSet(escalationExecutorTypes) }

// escalationCloseOnlyTypeList is escalationCloseOnlyTypes as a sorted list.
func escalationCloseOnlyTypeList() []model.BeadType { return sortedTypeSet(escalationCloseOnlyTypes) }

func sortedTypeSet(set map[model.BeadType]bool) []model.BeadType {
	out := make([]model.BeadType, 0, len(set))
	for t := range set {
		out = append(out, t)
	}
	slices.Sort(out)
	return out
}

func sortedTypes(in []model.BeadType) []model.BeadType {
	out := slices.Clone(in)
	slices.Sort(out)
	return out
}

// roleOwnershipGrantsField is the type:role bead field that holds the role's
// rows of the grant table: a JSON list, each entry a row without its principal
// (the role) or its id (role:<title>/<index>):
//
//	[{"types":["task"],"label":"thread","actions":["update","close"]}]
//
// "types":["*"] is every type, and then "except" must name decision. Setting it
// is gated like the other authority grants (agent_identity_authz.go). Runbook:
// kbeads/docs/ownership-grants.md, "Give a role write reach".
const roleOwnershipGrantsField = "ownership_grants"

// roleGrantAllTypes in a role row's types means every type.
const roleGrantAllTypes model.BeadType = "*"

// roleGrantActions are the only actions a role row may name: no role deletes
// beads other agents filed, and hook-emit reaches only config-kind beads.
var roleGrantActions = []string{"update", "close"}

// maxRoleGrantRows bounds one role's rows, and so the admit metric's grant label.
const maxRoleGrantRows = 32

// roleGrantRow is one entry of a role bead's ownership_grants.
type roleGrantRow struct {
	Types      []model.BeadType `json:"types"`
	Except     []model.BeadType `json:"except,omitempty"`
	Label      string           `json:"label,omitempty"`
	Actions    []string         `json:"actions"`
	Condition  string           `json:"condition,omitempty"`
	Privileged bool             `json:"privileged,omitempty"`
}

// parseRoleOwnershipGrants decodes an ownership_grants value (the text form
// parseFieldsToStringMap gives). Empty or null is no rows. An unknown key, an
// entry with no types or actions, a decision type (a "*" row must list decision
// in except), delete or any action outside roleGrantActions, or a condition that
// is unknown or not caller-bound makes the whole value invalid: the write-time
// check (validateRoleFieldsImpl) refuses it, and a stored invalid value grants
// nothing.
func parseRoleOwnershipGrants(raw string) ([]roleGrantRow, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var rows []roleGrantRow
	if err := dec.Decode(&rows); err != nil {
		return nil, fmt.Errorf("must be a JSON list of {types, except, label, actions, condition, privileged}: %w", err)
	}
	if dec.More() {
		return nil, errors.New("must be a single JSON list")
	}
	if len(rows) > maxRoleGrantRows {
		return nil, fmt.Errorf("holds %d rows, at most %d", len(rows), maxRoleGrantRows)
	}
	for i, r := range rows {
		if _, err := r.grantTypes(); err != nil {
			return nil, fmt.Errorf("entry %d: %w", i, err)
		}
		if len(r.Actions) == 0 {
			return nil, fmt.Errorf("entry %d names no actions", i)
		}
		for _, a := range r.Actions {
			if !slices.Contains(roleGrantActions, a) {
				return nil, fmt.Errorf("entry %d action %q must be one of %v", i, a, roleGrantActions)
			}
		}
		if r.Label != strings.TrimSpace(r.Label) {
			return nil, fmt.Errorf("entry %d label %q has surrounding spaces", i, r.Label)
		}
		if r.Condition != "" {
			cond, known := ownershipConditions[r.Condition]
			if !known {
				return nil, fmt.Errorf("entry %d names unknown condition %q", i, r.Condition)
			}
			if !cond.CallerBound {
				return nil, fmt.Errorf("entry %d condition %q holds for a bead whoever asks, so no role row may use it", i, r.Condition)
			}
		}
	}
	return rows, nil
}

// grantTypes is the row's type set, or an error naming what is wrong with it.
func (r roleGrantRow) grantTypes() (grantTypes, error) {
	if len(r.Types) == 0 {
		return grantTypes{}, errors.New("names no types")
	}
	if slices.Contains(r.Types, roleGrantAllTypes) {
		if len(r.Types) != 1 {
			return grantTypes{}, errors.New(`types "*" must stand alone`)
		}
		if !slices.Contains(r.Except, model.TypeDecision) {
			return grantTypes{}, errors.New(`types ["*"] must list decision in except: no role row may reach a decision`)
		}
		for _, e := range r.Except {
			if strings.TrimSpace(string(e)) == "" || e == roleGrantAllTypes {
				return grantTypes{}, fmt.Errorf("except names an invalid type %q", e)
			}
		}
		return grantTypes{All: true, Except: sortedTypes(r.Except)}, nil
	}
	if len(r.Except) > 0 {
		return grantTypes{}, errors.New(`except needs types ["*"]`)
	}
	for _, t := range r.Types {
		switch {
		case strings.TrimSpace(string(t)) == "":
			return grantTypes{}, errors.New("names an empty type")
		case t == model.TypeDecision:
			return grantTypes{}, errors.New("names type decision, which no role row may reach")
		}
	}
	return grantTypes{Only: sortedTypes(r.Types)}, nil
}

// roleOwnershipGrants reads a role bead's rows from ownership_grants. An
// unset value or an empty list is no row; an invalid value is an error and
// grants nothing.
func roleOwnershipGrants(rb *model.Bead) ([]ownershipGrant, error) {
	fields := parseFieldsToStringMap(rb.Fields)
	parsed, err := parseRoleOwnershipGrants(fields[roleOwnershipGrantsField])
	if err != nil {
		return nil, err
	}
	p := grantPrincipal{Kind: grantKindAgent, Name: rb.Title}
	rows := make([]ownershipGrant, 0, len(parsed))
	for i, r := range parsed {
		types, _ := r.grantTypes()
		rows = append(rows, ownershipGrant{ID: "role:" + rb.Title + "/" + strconv.Itoa(i), Principal: p,
			Actions: r.Actions, Types: types, Label: r.Label, Condition: r.Condition, Privileged: r.Privileged,
			Origin: rb.ID})
	}
	return rows, nil
}

// ownershipGrantPrivileged is the computed privilege floor of a row (owner
// choice 2 on gd-BAXfAAEbBS): its writer raised it, it reaches All types, it
// deletes, or it has no label, fields or condition and so reaches every bead
// of its types. A writer can raise a row but never lower it below this.
func ownershipGrantPrivileged(g *ownershipGrant) bool {
	return g.Privileged || g.Types.All || slices.Contains(g.Actions, "delete") ||
		(g.Label == "" && len(g.Fields) == 0 && g.Condition == "")
}

// roleGrantsPrivileged reports whether a role bead holds a privileged row, or
// a grant value that does not parse (counted as privileged, as an unparseable
// capability tier set is).
func roleGrantsPrivileged(rb *model.Bead) bool {
	rows, err := roleOwnershipGrants(rb)
	if err != nil {
		return true
	}
	return slices.ContainsFunc(rows, func(g ownershipGrant) bool { return ownershipGrantPrivileged(&g) })
}

// ownershipGrantTable is every code row and every machine-derived row.
func ownershipGrantTable() []ownershipGrant {
	return append(ownershipGrantRows(), machineGrantRows()...)
}

// ownershipTable is built once: the code rows are constants and
// machineCapabilityGrants is never written at run time.
var ownershipTable = ownershipGrantTable()

// ownershipAdmission is the row that admitted a write, and the record its
// condition leaves (nil for most rows).
type ownershipAdmission struct {
	grant   string
	onAdmit func()
}

// evaluateOwnershipGrants returns the row that admits action on bead for the
// caller on ctx, or ok=false when none does. Rows are tried cheapest first:
// rows with no condition, then rows whose condition reads no store, then the
// caller's role rows, then the rows whose condition reads the store, so a
// write the base rule admits costs no store read. The order changes which row
// is credited, never the verdict. Config-kind beads stay outside the table.
func (s *BeadsServer) evaluateOwnershipGrants(ctx context.Context, action string, bead *model.Bead) (adm ownershipAdmission, ok bool) {
	if bead == nil || bead.Kind == model.KindConfig {
		return ownershipAdmission{}, false
	}
	c := resolveGrantCaller(ctx)
	try := func(g *ownershipGrant) bool {
		if !g.matchesBead(action, bead) || !c.matches(g.Principal) {
			return false
		}
		if g.Condition == "" {
			adm = ownershipAdmission{grant: g.ID}
			return true
		}
		cond, known := ownershipConditions[g.Condition]
		if !known {
			return false
		}
		held, onAdmit, err := cond.Eval(ctx, s, c, action, bead)
		if err != nil {
			recordOwnershipConditionError(g.Condition)
			return false
		}
		if held {
			adm = ownershipAdmission{grant: g.ID, onAdmit: onAdmit}
		}
		return held
	}
	for _, pass := range []func(g *ownershipGrant) bool{
		func(g *ownershipGrant) bool { return g.Condition == "" },
		func(g *ownershipGrant) bool { return g.Condition != "" && ownershipConditions[g.Condition].Cheap },
	} {
		for i := range ownershipTable {
			if g := &ownershipTable[i]; pass(g) && try(g) {
				return adm, true
			}
		}
	}
	if c.kinds[grantKindAgent] {
		for _, rb := range s.callerOpenRoleBeads(ctx) {
			rows, err := roleOwnershipGrants(rb)
			if err != nil {
				continue
			}
			for i := range rows {
				if try(&rows[i]) {
					return adm, true
				}
			}
		}
	}
	for i := range ownershipTable {
		if g := &ownershipTable[i]; g.Condition != "" && !ownershipConditions[g.Condition].Cheap && try(g) {
			return adm, true
		}
	}
	return ownershipAdmission{}, false
}

// AllowingGrant names one grant row that would admit a denied write for some
// other caller (AuthzDenial.AllowingGrants). It discloses the table's shape,
// which the golden publishes anyway, and a condition's name, never its inputs.
type AllowingGrant struct {
	Grant     string `json:"grant"`
	Principal string `json:"principal"`
	Condition string `json:"condition,omitempty"`
}

// maxAllowingGrants bounds the rows a denial lists.
const maxAllowingGrants = 5

// ownershipAllowingGrants lists the rows whose bead side (action, type, label)
// covers a denied write and that name a role (a machine role or a role bead)
// or a type list narrower than all; the others match every bead and say
// nothing about this one. Role rows come from every open role bead; a store
// error leaves them out. Sorted by id, at most maxAllowingGrants.
func (s *BeadsServer) ownershipAllowingGrants(ctx context.Context, action string, bead *model.Bead) []AllowingGrant {
	var out []AllowingGrant
	add := func(g *ownershipGrant) {
		if !g.matchesBead(action, bead) || (g.Principal.Name == "" && g.Types.All) {
			return
		}
		principal := g.Principal.Kind
		if g.Principal.Name != "" {
			principal += ":" + g.Principal.Name
		}
		out = append(out, AllowingGrant{Grant: g.ID, Principal: principal, Condition: g.Condition})
	}
	for i := range ownershipTable {
		add(&ownershipTable[i])
	}
	if s != nil && s.store != nil {
		roles, _, err := s.store.ListBeads(ctx, model.BeadFilter{Type: []model.BeadType{"role"}})
		if err == nil {
			for _, rb := range roles {
				if rb.Status == model.StatusClosed {
					continue
				}
				rows, err := roleOwnershipGrants(rb)
				if err != nil {
					continue
				}
				for i := range rows {
					add(&rows[i])
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Grant < out[j].Grant })
	if len(out) > maxAllowingGrants {
		out = out[:maxAllowingGrants]
	}
	return out
}

// describeAllowingGrants renders the rows for the denial message, e.g.
// "machine gardenio-machine-trigger-ops (row machine:trigger-ops/ownership),
// agent role herder (row role:herder/0)".
func describeAllowingGrants(grants []AllowingGrant) string {
	parts := make([]string, 0, len(grants))
	for _, g := range grants {
		kind, name, _ := strings.Cut(g.Principal, ":")
		who := kind
		switch {
		case kind == grantKindAgent && name != "":
			who = "agent role " + name
		case name != "":
			who = kind + " " + name
		}
		if g.Condition != "" {
			who += " when " + g.Condition
		}
		parts = append(parts, who+" (row "+g.Grant+")")
	}
	return strings.Join(parts, ", ")
}
