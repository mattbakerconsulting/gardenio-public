package server

import (
	"context"
	"log/slog"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Seal the gardener of a plan the home garden's owner asked for in a thread
// (programme step P8.7, owner ruling 3 on gd-7qUpu15BBK).
//
// A plan the owner asks for in their own thread starts building with a
// "change or drop" notice instead of an approve card (gb lands plan write,
// internal/steward WritePlan). The thread agent then binds the plan's gardener
// on its own token, so stampSpawnLineage never seals it, and with no approve
// answer stampSpawnLineageFromPlanApproval has nothing to seal from. The ask
// itself is the owner standing behind the plan, so the server seals such a
// trigger to the home garden's owner when ALL hold:
//
//   - the caller is a thread (or DM) agent on its own per-agent token, and its
//     agent bead names the thread head H it is bound to (thread_id, which a
//     per-agent token may not change, planScopeBindingPatchDenial);
//   - H is a thread head (labelled thread) that the home garden's owner
//     opened: its created_by, server-stamped from the opener's identity, is
//     the owner, so a thread an agent or another person opened buys nothing;
//   - the trigger is open, enabled, spawns the plan-gardener role in the
//     caller's project, and is pinned to a plan by its gardener-epic:<plan>
//     label;
//   - the plan is an epic labelled plan in that project with
//     plan_state=building, its thread_id is H, and it was created no more
//     than planApprovalSealWindow ago, so an old ask cannot bless a gardener
//     bound much later;
//   - the project is the home garden (homeGardenOperator): tenant lands keep
//     the steward's approve card;
//   - no other open trigger for the plan carries a valid seal, so one ask buys
//     one gardener lane.
//
// What the thread can buy is the plan-gardener role in the home garden, for a
// building plan the home steward would seal anyway
// (spawn_lineage_home_steward.go); this seals it at bind instead of after the
// first refused fire.

// stampSpawnLineageFromThreadAsk seals bead to the home garden's owner when
// the conditions in the file header hold. It must run on the final merged
// fields.
func (s *BeadsServer) stampSpawnLineageFromThreadAsk(ctx context.Context, bead *model.Bead) bool {
	if !isAutonomousBeadType(bead.Type) || isOIDCAdminSession(ctx) || validSpawnSeal(bead) != nil {
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
	callerID, head := s.threadAskCaller(ctx, project)
	if head == "" {
		return false
	}
	owner, err := homeGardenOperator(ctx, s.store, project)
	if err != nil || owner == "" || !s.threadAskPlan(ctx, planID, project, head) {
		return false
	}
	thread, err := s.store.GetBead(ctx, head)
	if err != nil || thread == nil || !hasLabel(thread.Labels, threadHeadLabel) || threadOpener(thread) != owner {
		return false
	}
	if s.planHasSealedGardener(ctx, planID, bead.ID) {
		return false
	}
	if !s.sealSpawnLineageAs(bead, owner) {
		return false
	}
	slog.Info("spawn lineage: plan gardener sealed from the owner's ask in a thread",
		"trigger", bead.ID, "plan", planID, "thread", head, "author", owner, "agent", callerID)
	return true
}

// threadAskCaller returns the caller's agent bead id and the thread head it is
// bound to, when it is a thread (or DM) agent of project on its own per-agent
// token; head is "" otherwise.
func (s *BeadsServer) threadAskCaller(ctx context.Context, project string) (callerID, head string) {
	callerID, ok := agentBeadIDFromContext(ctx)
	if !ok {
		return "", ""
	}
	agent, err := s.store.GetBead(ctx, callerID)
	if err != nil || agent == nil || agent.Type != model.BeadType("agent") ||
		!agentHasRole(agent, "thread", "dm") || projectFromFields(agent.Fields) != project {
		return "", ""
	}
	return callerID, agentThreadHead(agent)
}

// threadAskPlan reports whether planID is a building plan of project bound to
// thread head, created within planApprovalSealWindow.
func (s *BeadsServer) threadAskPlan(ctx context.Context, planID, project, head string) bool {
	plan, err := s.store.GetBead(ctx, planID)
	if err != nil || plan == nil || plan.Type != model.TypeEpic || !hasLabel(plan.Labels, "plan") ||
		projectFromLabelsOrEmpty(plan.Labels) != project || planStateFromFields(plan.Fields) != planStateBuilding ||
		threadIDFromFields(plan.Fields) != head {
		return false
	}
	now := s.lineageNow()
	return !plan.CreatedAt.After(now.Add(spawnLineageClockSkew)) && now.Sub(plan.CreatedAt) <= planApprovalSealWindow
}

// threadOpener is the person who opened a thread head, as "oidc:<email>", or
// "" when its created_by names no person (an agent or a machine opened it).
func threadOpener(head *model.Bead) string {
	who := strings.TrimPrefix(strings.TrimSpace(head.CreatedBy), "oidc:")
	if !strings.Contains(who, "@") {
		return ""
	}
	return canonicalizeHumanOperator("oidc:" + who)
}
