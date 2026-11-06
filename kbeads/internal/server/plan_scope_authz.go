package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Plan-scoped write rights (gd-Mk64Bko3XO, owner decision gd-7h2bWUl0Uv
// option a).
//
// A plan's own agents are often none of created_by / assignee / owner on the
// beads they drive: the owner minted the plan, or a finished crew filed the
// step. Their plan_state, step-body, phase_order and close writes bounced to
// the laptop as local escalations. This exception to the ownership gate lets
// an agent bound to plan epic P update ordinary fields on, and close, a bead
// whose parent chain reaches P. It is scoped six ways:
//
//   - identity: a per-agent token only (agentBeadIDFromContext). The binding
//     is derived from the credential and from beads the caller cannot write
//     through this rule; nothing in the request is read;
//   - binding: one of the three kinds in planScopeRule, checked against P.
//     Two of them read link fields off the caller's agent bead (thread_id,
//     trigger_id), which a per-agent token may not change once the agent
//     exists (planScopeBindingPatchDenial), so an agent cannot bind itself
//     to another plan by editing its own bead;
//   - membership: the bead is P or a descendant of P through parent-child
//     edges, and P is the NEAREST plan on that path (planScopePlans), so a
//     sibling or nested plan's agent gets nothing here;
//   - project: the token's project, P's project and the bead's project agree;
//   - type: only the work-item and narrative types in planScopeTypes. Triggers,
//     schedules, agents, roles, advice, escalations and decisions are never
//     admitted, so trigger enable, run budget, max_runs, spawn_lineage and
//     privileged role fields stay with the gates that already own them;
//   - action: update and close. Never delete.
//
// Every other gate still runs: authorizeWriteTitled before this, and after it the
// spawn-lineage seal, autonomous counters, the gardener-enable gate and the
// taint gate. Each admission is written to the bead's mutation history
// (recordPlanScopeAdmission) so the audit trail names the rule and the plan.

// planScopeRule names the binding that admitted a plan-scoped write.
type planScopeRule string

const (
	// planScopeRuleAssignee: the caller's agent name is P's assignee.
	planScopeRuleAssignee planScopeRule = "plan_assignee"
	// planScopeRuleThreadAgent: the caller is the thread (or DM) agent of P's
	// thread: P's fields.thread_id names the thread head the caller's agent
	// bead is bound to.
	planScopeRuleThreadAgent planScopeRule = "plan_thread_agent"
	// planScopeRulePlanGardener: the caller is an iteration of the gardener
	// trigger pinned to P with gardener-epic:<P>.
	planScopeRulePlanGardener planScopeRule = "plan_gardener"
	// planScopeRulePlanReactor: the caller is the beads-viewer's machine
	// principal with the plan_scope capability, applying the owner's answer to
	// P's decision (plan_answer_reactor.go in cmd/beads-viewer, gd-Dl88Htipuq).
	planScopeRulePlanReactor planScopeRule = "plan_reactor"
)

// planScopeAuditField is the mutation_history field of the admission row. Like
// the dep.* rows it names no bead column, so undo refuses it.
const planScopeAuditField = "authz.plan_scope"

// planScopeMaxNodes bounds the parent-chain walk. A plan is plan → phase →
// step (→ sub-step), and plan → bundle → doc; the bound is for corrupt or
// cyclic graphs, not real ones.
const planScopeMaxNodes = 32

// planScopeTypes are the bead types a plan's agents may write through this
// rule: the plan and its phases (epic), steps and check-ins (task, bug,
// feature, chore), and the narrative bundle and its docs.
var planScopeTypes = map[model.BeadType]bool{
	model.TypeEpic:    true,
	model.TypeTask:    true,
	model.TypeBug:     true,
	model.TypeFeature: true,
	model.TypeChore:   true,
	model.TypeBundle:  true,
	typeDoc:           true,
}

// planScopeAdmission is a plan-scoped write the ownership gate admitted.
type planScopeAdmission struct {
	Rule   planScopeRule
	PlanID string
}

// authorizePlanScope reports whether the caller is bound to the plan this
// bead belongs to and may therefore update or close it. A store error or a
// missing bead is a refusal: the ordinary ownership denial then stands.
func (s *BeadsServer) authorizePlanScope(ctx context.Context, action string, bead *model.Bead) (planScopeAdmission, bool) {
	if s == nil || s.store == nil || bead == nil {
		return planScopeAdmission{}, false
	}
	if action != "update" && action != "close" {
		return planScopeAdmission{}, false
	}
	if bead.Kind == model.KindConfig {
		return planScopeAdmission{}, false
	}
	if machineHasCapability(ctx, capPlanScope) {
		return s.authorizePlanReactor(ctx, action, bead)
	}
	if !planScopeTypes[bead.Type] {
		return planScopeAdmission{}, false
	}
	agentBeadID, ok := agentBeadIDFromContext(ctx)
	if !ok {
		return planScopeAdmission{}, false
	}
	agentName := AuthIdentityFromContext(ctx).Subject
	project := TokenProjectFromContext(ctx)
	if project == "" || agentName == "" || projectFromLabelsOrEmpty(bead.Labels) != project {
		return planScopeAdmission{}, false
	}
	agent, err := s.store.GetBead(ctx, agentBeadID)
	if err != nil || agent == nil || agent.Type != "agent" || projectFromFields(agent.Fields) != project {
		return planScopeAdmission{}, false
	}
	for _, plan := range s.planScopePlans(ctx, bead) {
		if projectFromLabelsOrEmpty(plan.Labels) != project {
			continue
		}
		if rule, ok := s.planScopeBinding(ctx, plan, agent, agentName); ok {
			return planScopeAdmission{Rule: rule, PlanID: plan.ID}, true
		}
	}
	return planScopeAdmission{}, false
}

// authorizePlanReactor is the plan-scope rule for the viewer's plan answer
// reactor (gd-Dl88Htipuq). The reactor applies an owner's answer to a plan
// decision server-side, so the owner's click is the only one: approve moves
// the plan to building and turns its gardener on, change and drop close the
// plan and its open steps. Those beads were filed by agents and people, so it
// needs the same reach a plan's own agents have, across projects, as a
// machine principal no agent pod holds:
//
//   - the plan's own beads (planScopeTypes, P or under P), update and close;
//   - the plan's gardener (a trigger or schedule pinned with
//     gardener-epic:<P>, P an open plan epic), update only: enabling it is
//     the start of the build. The enable still passes the gardener-enable gate
//     (P must be building), and the spawn seal comes only from the owner's
//     approval (stampSpawnLineageFromPlanApproval).
//
// Which answers count is the reactor's check, not this rule's: the rule only
// bounds what the reactor's credential can touch to plans.
func (s *BeadsServer) authorizePlanReactor(ctx context.Context, action string, bead *model.Bead) (planScopeAdmission, bool) {
	if isGardenerBead(bead.Type, bead.Labels, bead.Fields) {
		planID := gardenerEpicFromLabels(bead.Labels)
		if action != "update" || planID == "" {
			return planScopeAdmission{}, false
		}
		plan, err := s.store.GetBead(ctx, planID)
		if err != nil || plan == nil || plan.Type != model.TypeEpic || !hasLabel(plan.Labels, planMarkerLabel) ||
			plan.Status == model.StatusClosed || projectFromLabelsOrEmpty(plan.Labels) != projectFromLabelsOrEmpty(bead.Labels) {
			return planScopeAdmission{}, false
		}
		return planScopeAdmission{Rule: planScopeRulePlanReactor, PlanID: plan.ID}, true
	}
	if !planScopeTypes[bead.Type] {
		return planScopeAdmission{}, false
	}
	for _, plan := range s.planScopePlans(ctx, bead) {
		if projectFromLabelsOrEmpty(plan.Labels) == projectFromLabelsOrEmpty(bead.Labels) {
			return planScopeAdmission{Rule: planScopeRulePlanReactor, PlanID: plan.ID}, true
		}
	}
	return planScopeAdmission{}, false
}

// planScopeBinding reports how the caller's agent bead is bound to plan, if at
// all. The plan side of each kind is read from a bead under the ordinary
// ownership gate (the plan epic's assignee and thread_id, the gardener
// trigger); the agent side from link fields fixed when the agent was spawned
// (planScopeBindingKeys).
func (s *BeadsServer) planScopeBinding(ctx context.Context, plan, agent *model.Bead, agentName string) (planScopeRule, bool) {
	if plan.Assignee != "" && plan.Assignee == agentName {
		return planScopeRuleAssignee, true
	}
	if head := threadIDFromFields(plan.Fields); head != "" && agentHasRole(agent, "thread", "dm") && head == agentThreadHead(agent) {
		return planScopeRuleThreadAgent, true
	}
	if s.agentIsPlanGardener(ctx, plan, agent) {
		return planScopeRulePlanGardener, true
	}
	return "", false
}

// agentIsPlanGardener reports whether agent is an iteration of the gardener
// trigger bound to plan: the agent bead carries the plan-gardener role and
// names its trigger, and that trigger is an open gardener pinned to this plan
// by gardener-epic:<plan id> in the plan's project. The pin lives on the
// trigger; a retired (closed) gardener's iterations lose the binding.
func (s *BeadsServer) agentIsPlanGardener(ctx context.Context, plan, agent *model.Bead) bool {
	if !agentHasRole(agent, planGardenerRole) {
		return false
	}
	triggerID := strings.TrimSpace(fieldsStringValue(agent.Fields, triggerLineageField))
	if triggerID == "" {
		return false
	}
	trig, err := s.store.GetBead(ctx, triggerID)
	if err != nil || trig == nil || trig.Status == model.StatusClosed {
		return false
	}
	if !isGardenerBead(trig.Type, trig.Labels, trig.Fields) || !hasLabel(trig.Labels, gardenerEpicLabelPrefix+plan.ID) {
		return false
	}
	return projectFromLabelsOrEmpty(trig.Labels) == projectFromLabelsOrEmpty(plan.Labels)
}

// agentHasRole reports whether any element of the agent bead's role list is
// one of roles.
func agentHasRole(agent *model.Bead, roles ...string) bool {
	role, _, err := exactFieldString(agent.Fields, agentRoleField)
	if err != nil {
		return false
	}
	for _, r := range parseRoleList(role) {
		if slices.Contains(roles, strings.ToLower(r)) {
			return true
		}
	}
	return false
}

// agentThreadHead is the thread head an agent bead is bound to: thread_id, or
// its legacy alias slack_thread_bead_id.
func agentThreadHead(agent *model.Bead) string {
	if head := threadIDFromFields(agent.Fields); head != "" {
		return head
	}
	return strings.TrimSpace(fieldsStringValue(agent.Fields, "slack_thread_bead_id"))
}

// planGardenerRole is the spawn role of a plan's gardener iterations
// (plangardener.SpawnRole, planagent.SpawnRole).
const planGardenerRole = "plan-gardener"

// planScopePlans returns the plans bead belongs to: bead itself when it is a
// plan epic, otherwise the nearest plan epic on each parent-child path up from
// it. The walk stops at the first plan on a path, so a plan nested in another
// belongs to itself alone and an agent of the outer plan gains nothing inside
// it.
func (s *BeadsServer) planScopePlans(ctx context.Context, bead *model.Bead) []*model.Bead {
	var plans []*model.Bead
	seen := map[string]bool{bead.ID: true}
	queue := []*model.Bead{bead}
	for len(queue) > 0 && len(seen) <= planScopeMaxNodes {
		cur := queue[0]
		queue = queue[1:]
		if cur.Type == model.TypeEpic && hasLabel(cur.Labels, planMarkerLabel) {
			plans = append(plans, cur)
			continue
		}
		deps, err := s.store.GetDependencies(ctx, cur.ID)
		if err != nil {
			return nil
		}
		for _, d := range deps {
			if d == nil || !model.IsParentChildDepType(string(d.Type)) || seen[d.DependsOnID] {
				continue
			}
			seen[d.DependsOnID] = true
			parent, err := s.store.GetBead(ctx, d.DependsOnID)
			if err != nil || parent == nil {
				continue
			}
			queue = append(queue, parent)
		}
	}
	return plans
}

// recordPlanScopeAdmission writes the admission to the bead's mutation
// history: the rule and the plan, e.g. "plan_gardener plan=gd-P action=close".
// It records the ownership decision, so it is written before the write it
// admitted, whose own rows follow it. It does not flip dibs.
func (s *BeadsServer) recordPlanScopeAdmission(ctx context.Context, action string, bead *model.Bead, adm planScopeAdmission) {
	actor := actorFromContext(ctx)
	slog.Info("ownership: plan-scoped write admitted",
		"bead_id", bead.ID, "action", action, "rule", string(adm.Rule), "plan_id", adm.PlanID, "actor", actor)
	s.recordMutationNoDibs(ctx, bead.ID, actor, planScopeAuditField, "",
		string(adm.Rule)+" plan="+adm.PlanID+" action="+action, model.MutationTypeUpdate)
}

// planScopeBindingKeys are the agent-bead link fields the plan-scope rule
// and the dispatched-task rule (task_id, dispatched_task_authz.go) read. They are stamped when the agent is spawned (by the bridge, the pool,
// the trigger engine or `gb spawn`, all at create or with their own
// credentials). Agent beads are config-kind, outside the ownership gate, so
// without this lock any agent in the project could point its own bead at
// another plan's thread or gardener.
var planScopeBindingKeys = []string{"thread_id", "slack_thread_bead_id", triggerLineageField, "task_id"}

// planScopeBindingPatchDenial refuses a per-agent token changing a
// planScopeBindingKeys field on an existing agent bead through an update. An
// echo of the stored value is not a change. Other callers (operators, the
// bridge, machine principals, the static token) are not gated here.
func (s *BeadsServer) planScopeBindingPatchDenial(ctx context.Context, action string, bead *model.Bead, patch json.RawMessage) *AuthzDenial {
	if bead == nil || bead.Type != "agent" || len(patch) == 0 {
		return nil
	}
	var changed []string
	for _, k := range planScopeBindingKeys {
		val, present, err := exactFieldString(patch, k)
		if err != nil || (present && val != fieldsStringValue(bead.Fields, k)) {
			changed = append(changed, k)
		}
	}
	return s.planScopeBindingKeyDenial(ctx, action, changed)
}

// planScopeBindingKeyDenial is the field-mutation-endpoint form of
// planScopeBindingPatchDenial: only the keys are known there, so touching a
// binding key at all is refused for a per-agent token.
func (s *BeadsServer) planScopeBindingKeyDenial(ctx context.Context, action string, keys []string) *AuthzDenial {
	var gated []string
	for _, k := range keys {
		if slices.Contains(planScopeBindingKeys, k) {
			gated = append(gated, k)
		}
	}
	if len(gated) == 0 {
		return nil
	}
	if _, ok := agentBeadIDFromContext(ctx); !ok {
		return nil
	}
	d := &AuthzDenial{
		Action:   action,
		BeadType: "agent",
		Message: "insufficient permissions: " + action + " on type agent changing " + strings.Join(gated, ", ") +
			" from an agent token is refused; these link an agent to its thread, gardener and task and are fixed at spawn",
	}
	s.populateAdvisoryText(ctx, d, "An agent's thread, trigger and task links decide which plan and task it may write.")
	return d
}
