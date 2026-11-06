package server

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
)

// Seal a plan's gardener from the owner's approval (gd-YaoqmhIx8x, owner
// decision gd-7h2bWUl0Uv option a).
//
// A plan written in a thread is approved by its owner on a decision (the
// steward's approve / change / drop card), and the thread then binds the plan's
// gardener with `gb lands plan answer`. The thread writes on its own agent
// token, so stampSpawnLineage never seals that trigger, and every fire of the
// privileged plan-gardener role 403s into an escalation until an admin reseals
// it by hand (gd-ShdToeVJdu, gd-zRJkf39CbP).
//
// The owner already stood behind the plan: they answered its decision in their
// own session. decisionResolverStamp recorded that server-side
// (resolver_subject, resolver_admin), and no client can write those keys. So
// when a non-admin caller creates or turns on a plan-gardener trigger, the
// server seals it to the admin who approved the plan, provided ALL hold:
//
//   - the caller is the agent that asked: it writes on its own per-agent token
//     (agentBeadIDFromContext), and that bead id is the approving decision's
//     requesting_agent_bead_id with requesting_agent_verified=true, which the
//     create guard stamps from the filer's credential
//     (decision_requesting_agent.go). Another agent in the project, or a
//     static-token caller naming the thread with X-Actor, cashes in nothing;
//   - the trigger spawns the plan-gardener role, in the plan's project, and is
//     pinned to the plan by its gardener-epic:<plan> label;
//   - the plan is an epic labelled plan with plan_state=building;
//   - a decision the plan is blocked by asked about this plan
//     (requesting_task, fixed at create like the question, see
//     decision_question_immutable.go, so an answer cannot be moved onto
//     another plan), is closed with chosen=approve, and was answered by an
//     OIDC/edge gardenio-admin session (resolver_admin);
//   - that answer closed no more than planApprovalSealWindow ago, so an old
//     approval cannot bless a gardener bound much later;
//   - no other open trigger for the plan already carries a valid seal, so one
//     approval buys one gardener lane.
//
// The seal is the same seal an admin's reseal writes, with the approver as its
// author and the trigger's content at that moment as its digest. A later change
// to that content voids it as usual. The thread's token decides nothing beyond
// who it is: every other input is a server-stamped decision field or the
// plan's own state, and what the approval can buy is the plan-gardener role in
// the plan's project.
//
// The viewer's plan answer reactor (gd-Dl88Htipuq) is the other caller: a
// machine principal holding the plan_scope capability, which no agent pod
// holds. It turns the gardener on as it applies the owner's approve, for
// whichever agent filed the decision, so for it the requester match is not
// asked; every other condition is.
//
// The plan's thread agent (role thread/dm, plan.thread_id == the agent's thread
// head) is not a second way in yet: an agent token may still rewrite its own
// agent bead's thread_id on main, so that binding is not credential-derived
// until the agent-bead link lock lands (planScopeBindingPatchDenial, PR #1267).
// The requester match covers the thread that filed the plan's decision, which
// is the thread that binds its gardener.
// The thread does write the trigger's prompt; it wrote the plan the owner
// approved too, and the gardener runs that plan.
//
// A missing or unreadable approval leaves the trigger unsealed, exactly as
// before, and its spawns escalate.

// planApprovalOption is the steward's approve answer (internal/steward
// PlanApprove in the controller).
const planApprovalOption = "approve"

// planGardenerSpawnRole is the role a plan's gardener spawns
// (internal/plangardener SpawnRole in the controller).
const planGardenerSpawnRole = "plan-gardener"

// planApprovalSealWindow is how long after the owner answers approve a
// non-admin bind may still be sealed from that answer.
const planApprovalSealWindow = 24 * time.Hour

// stampSpawnLineageFromPlanApproval seals bead to the admin who approved the
// plan it gardens, when the conditions in the file header hold. It is a no-op
// for an OIDC admin session (stampSpawnLineage seals those) and for a bead that
// already carries a valid seal. It must run on the final merged fields.
func (s *BeadsServer) stampSpawnLineageFromPlanApproval(ctx context.Context, bead *model.Bead) bool {
	if !isAutonomousBeadType(bead.Type) || isOIDCAdminSession(ctx) || validSpawnSeal(bead) != nil {
		return false
	}
	caller, ok := agentBeadIDFromContext(ctx)
	reactor := !ok && machineHasCapability(ctx, capPlanScope)
	if !ok && !reactor {
		return false
	}
	if bead.Status == model.StatusClosed || !effectiveGardenerEnabled(bead.Type, bead.Fields) {
		return false
	}
	if triggerSpawnRole(bead) != planGardenerSpawnRole {
		return false
	}
	planID := gardenerEpicFromLabels(bead.Labels)
	project := projectFromLabelsOrEmpty(bead.Labels)
	if planID == "" || project == "" || strings.TrimSpace(fieldsStringValue(bead.Fields, "project")) != project {
		return false
	}
	plan, err := s.store.GetBead(ctx, planID)
	if err != nil || plan == nil || plan.Type != model.TypeEpic || !hasLabel(plan.Labels, "plan") ||
		projectFromLabelsOrEmpty(plan.Labels) != project || planStateFromFields(plan.Fields) != planStateBuilding {
		return false
	}
	author := s.planApprovalAuthor(ctx, planID, caller, reactor)
	if author == "" {
		return false
	}
	if s.planHasSealedGardener(ctx, planID, bead.ID) {
		return false
	}
	if !s.sealSpawnLineageAs(bead, author) {
		return false
	}
	slog.Info("spawn lineage: plan gardener sealed from the plan's approval",
		"trigger", bead.ID, "plan", planID, "author", author, "agent", caller, "plan_reactor", reactor)
	return true
}

// planApprovalAuthor returns "oidc:<email>" of the gardenio-admin who most
// recently answered approve, within planApprovalSealWindow, on a decision that
// gates planID and that agent bead callerAgent filed on its own credential, or
// "" when there is none. anyRequester (the plan answer reactor) drops the
// requester match.
func (s *BeadsServer) planApprovalAuthor(ctx context.Context, planID, callerAgent string, anyRequester bool) string {
	deps, err := s.store.GetDependencies(ctx, planID)
	if err != nil {
		return ""
	}
	var ids []string
	for _, d := range deps {
		if d != nil && d.Type == model.DepBlocks && d.DependsOnID != "" {
			ids = append(ids, d.DependsOnID)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	decisions, err := s.store.GetBeadsByIDs(ctx, ids)
	if err != nil {
		return ""
	}
	now := s.lineageNow()
	var author string
	var newest time.Time
	for _, d := range decisions {
		if d == nil || d.Type != model.TypeDecision || d.Status != model.StatusClosed || d.ClosedAt == nil {
			continue
		}
		answered := *d.ClosedAt
		if answered.After(now.Add(spawnLineageClockSkew)) || now.Sub(answered) > planApprovalSealWindow {
			continue
		}
		filer := parseFieldsToStringMap(d.Fields)
		if !anyRequester && (filer[decisionRequestingAgentVerifiedField] != "true" || filer[decisionRequestingAgentField] != callerAgent) {
			continue
		}
		f := fieldsAsMap(d.Fields)
		if task, _ := f["requesting_task"].(string); strings.TrimSpace(task) != planID {
			continue
		}
		if chosen, _ := f["chosen"].(string); strings.TrimSpace(chosen) != planApprovalOption {
			continue
		}
		if isAdmin, _ := f[decisionResolverAdminField].(bool); !isAdmin {
			continue
		}
		subject, _ := f[decisionResolverSubjectField].(string)
		subject = strings.TrimPrefix(strings.TrimSpace(subject), "oidc:")
		if !strings.Contains(subject, "@") {
			continue
		}
		if author == "" || answered.After(newest) {
			author, newest = canonicalizeHumanOperator("oidc:"+subject), answered
		}
	}
	return author
}

// planHasSealedGardener reports whether an open trigger/schedule other than
// selfID is pinned to planID and validly sealed. A failed lookup reports true,
// so the caller does not seal.
func (s *BeadsServer) planHasSealedGardener(ctx context.Context, planID, selfID string) bool {
	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{"trigger", "schedule"},
		Labels: []string{gardenerEpicLabelPrefix + planID},
		Limit:  100,
	})
	if err != nil {
		return true
	}
	for _, b := range beads {
		if b != nil && b.ID != selfID && b.Status != model.StatusClosed && validSpawnSeal(b) != nil {
			return true
		}
	}
	return false
}
