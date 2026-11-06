package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"gardenio/kbeads/internal/model"
)

// isProtectedDibsCurrent reports whether s is an automation-class operator
// whose ownership of a bead must NOT be overwritten by a human dibs flip
// (ka-0000007567). The maybeFlipDibs guard already prevents sentinel /
// automation operators from *claiming* dibs; this companion check makes the
// protection symmetric so that a bead owned by fleet:jira-bridge,
// system:scheduler, trigger:<id>, or schedule:<id> is not silently
// re-attributed to the first agent's inherited human operator. Aligns with
// isSentinelSpawner's spawner classification (scheduler:, trigger:,
// escalation-bridge, system:, fleet:).
func isProtectedDibsCurrent(s string) bool {
	if isSentinelOperator(s) {
		return true
	}
	return strings.HasPrefix(s, "trigger:") ||
		strings.HasPrefix(s, "schedule:") ||
		strings.HasPrefix(s, "scheduler:") ||
		strings.HasPrefix(s, "escalation-bridge")
}

// isMachineOperator reports whether s is a machine principal's operator
// ("machine:<clientId>" — the controller, bridge, viewer service accounts).
// A machine write is recorded with the machine as operator, but it is not an
// ownership signal: machine principals never take dibs (gd-Ndam8t6aMb).
// Without this the viewer's and controller's routine writes moved the
// owner's plans out of /plans Mine. A machine-held bead is deliberately NOT
// protected in isProtectedDibsCurrent, so the next human write flips it back.
func isMachineOperator(s string) bool {
	return strings.HasPrefix(s, machineSubjectPrefix)
}

// takesDibs reports whether a write resolved to operator s may take dibs:
// a non-empty operator that is neither a sentinel nor a machine principal.
func takesDibs(s string) bool {
	return s != "" && !isSentinelOperator(s) && !isMachineOperator(s)
}

// recordMutation persists one mutation_history row. Best-effort: errors are
// logged and swallowed (mutation history is observability, not transactional).
//
// Each call also emits an info log line carrying the R9 §4.4 attribution
// fields (operated_by, operated_by_source). Resolver invariant: every
// mutation goes through resolveOperatedBy exactly once — this ensures the
// per-write metric M2 increments 1:1 with mutation_history rows even when
// the eventual store.RecordMutation call fails.
func (s *BeadsServer) recordMutation(ctx context.Context, beadID, actor, field, oldVal, newVal, mutationType string) {
	value, valueID, ok := s.recordMutationNoDibs(ctx, beadID, actor, field, oldVal, newVal, mutationType)
	if !ok {
		return
	}
	s.maybeFlipDibs(ctx, beadID, actor, value, &valueID, field, mutationType)
}

// recordMutationNoDibs persists one mutation_history row WITHOUT running the
// dibs ownership-flip rule. It is the shared core of recordMutation; callers
// that record a mutation which is NOT an ownership signal (e.g. a dependency
// edge add/remove — see recordDependencyMutation) use this directly so an
// edge change never re-attributes the bead's operator. Returns the resolved
// operator value+id and ok=false when no operator was attributable (the row
// is skipped, matching recordMutation's fail-closed behavior).
func (s *BeadsServer) recordMutationNoDibs(ctx context.Context, beadID, actor, field, oldVal, newVal, mutationType string) (string, int64, bool) {
	value, valueID, source, ok := s.resolveAuditOperatorRequired(ctx)
	if !ok {
		// Best-effort observability: when no operator is attributable, skip
		// RecordMutation so migration 026's NOT VALID check on
		// mutation_history.operated_by_id never sees a NULL insert.
		slog.Warn("skipping RecordMutation: operator unresolvable",
			"bead_id", beadID, "field", field, "operated_by_source", source)
		return "", 0, false
	}
	mut := &model.MutationRow{
		BeadID:       beadID,
		Actor:        actor,
		Field:        field,
		OldValue:     oldVal,
		NewValue:     newVal,
		MutationType: mutationType,
		OperatedBy:   value,
		OperatedByID: &valueID,
	}
	if err := s.store.RecordMutation(ctx, mut); err != nil {
		slog.Warn("failed to record mutation history",
			"bead_id", beadID,
			"field", field,
			"error", err,
			"operated_by", value,
			"operated_by_source", source,
		)
		return "", 0, false
	}
	slog.Info("mutation",
		"bead_id", beadID,
		"actor", actor,
		"field", field,
		"mutation_type", mutationType,
		"operated_by", value,
		"operated_by_source", source,
	)
	return value, valueID, true
}

// depMutationField is the mutation_history.field prefix for dependency
// edge audit rows (Proposal C, ka-0000007756). The full field is
// "dep.<type>" (e.g. "dep.parent-child") so `kd audit bead` shows which
// kind of edge changed. The new/old value is the "<bead> -> <depends-on>"
// edge endpoint pair, and mutation_type is create (add) / delete (remove).
const depMutationField = "dep."

// recordDependencyMutation appends an audit row recording a dependency edge
// add or remove so `kd audit bead` can answer "why did this edge change"
// post-hoc (Proposal C, ka-0000007756). Dep edges were previously only
// visible in the live event stream (TopicDependencyAdded/Removed), never in
// the append-only mutation history.
//
// The row is attributed to the SOURCE bead (bead_id) — the dependent side —
// which is where an operator inspecting the child's history would look. It
// deliberately does NOT flip dibs: an edge change is not an ownership signal.
func (s *BeadsServer) recordDependencyMutation(ctx context.Context, beadID, dependsOnID, depType, actor, mutationType string) {
	edge := beadID + " -> " + dependsOnID
	field := depMutationField + depType
	var oldVal, newVal string
	switch mutationType {
	case model.MutationTypeDelete:
		oldVal = edge
	default:
		newVal = edge
	}
	s.recordMutationNoDibs(ctx, beadID, actor, field, oldVal, newVal, mutationType)
}

// maybeFlipDibs implements the dibs rule (R3 §4): on every mutation, if the
// resolved operator is a non-empty, non-sentinel, non-machine human and
// differs from the bead's current operated_by, flip beads.operated_by and
// append a synthetic field='operated_by' mutation_history row.
//
// Best-effort — store errors are logged and swallowed; dibs is observability
// + future-routing, not transactional. Skipped when:
//   - the feature flag resolves operator=” (dark-deploy stays inert);
//   - the operator is a sentinel (system:* / fleet:*) — appended to history
//     by the parent mutation but never takes dibs (R2 chain-stop);
//   - the operator is a machine principal (machine:*, gd-Ndam8t6aMb) — same
//     treatment as a sentinel: recorded on the parent mutation, no flip;
//   - the bead's *current* operator is a sentinel (ka-0000007567): an agent
//     touching a fleet:* / system:* / trigger:* / schedule:* -owned bead does
//     NOT take dibs away from the automation owner. Without this protection
//     any agent's first mutation against e.g. a jira-bridge-created task
//     flipped the operator to the agent's inherited human operator, erasing
//     the upstream automation attribution;
//   - the parent mutation is itself field='operated_by' — prevents recursion;
//   - the parent mutation is a delete (no live row to flip).
func (s *BeadsServer) maybeFlipDibs(ctx context.Context, beadID, actor, value string, valueID *int64, parentField, parentMutationType string) {
	if !takesDibs(value) {
		return
	}
	if parentField == "operated_by" {
		return
	}
	if parentMutationType == model.MutationTypeDelete {
		return
	}
	current, err := s.store.GetBeadOperator(ctx, beadID)
	if err != nil {
		slog.Warn("dibs: failed to read current operator",
			"bead_id", beadID,
			"error", err,
		)
		return
	}
	if current == value {
		return
	}
	if isProtectedDibsCurrent(current) {
		slog.Info("dibs: skipping flip — current operator is automation-class",
			"bead_id", beadID,
			"current", current,
			"would_be", value,
		)
		return
	}
	// Post-cutover (migration 017): UpdateBeadOperator writes operated_by_id
	// by FK. The resolver always populates valueID alongside the TEXT value
	// for non-empty/non-sentinel operators (M2 §4.4), but guard against the
	// rare resolver miss so dibs stays best-effort.
	if valueID == nil {
		slog.Warn("dibs: skipping flip — operator FK not resolved",
			"bead_id", beadID,
			"operator", value,
		)
		return
	}
	if err := s.store.UpdateBeadOperator(ctx, beadID, *valueID); err != nil {
		slog.Warn("dibs: failed to flip operator",
			"bead_id", beadID,
			"from", current,
			"to", value,
			"error", err,
		)
		return
	}
	flipMut := &model.MutationRow{
		BeadID:       beadID,
		Actor:        actor,
		Field:        "operated_by",
		OldValue:     current,
		NewValue:     value,
		MutationType: model.MutationTypeUpdate,
		OperatedBy:   value,
		OperatedByID: valueID,
	}
	if err := s.store.RecordMutation(ctx, flipMut); err != nil {
		slog.Warn("dibs: failed to record flip mutation",
			"bead_id", beadID,
			"error", err,
		)
		return
	}
	slog.Info("dibs flip",
		"bead_id", beadID,
		"actor", actor,
		"prev_operator", current,
		"new_operator", value,
		"trigger_field", parentField,
		"trigger_mutation_type", parentMutationType,
	)
}

// maybeRebindAgentOnDecisionAnswer implements the privileged dibs rebind on
// decision-answer (R3 §5.6.2, R2 §5.2 step 3): when a human answers a
// decision created by an agent, the agent bead's operated_by is rebound to
// the responder so the agent inherits the responder on its next prompt
// cycle. This is a side-effect of decision-answer, NOT the dibs rule firing
// on the agent bead directly — there is no "incoming" mutation against the
// agent bead, only a propagation step keyed off the decision close.
//
// Best-effort: errors are logged and swallowed; rebind is a propagation
// side-effect, not transactional. Skipped when:
//   - the feature flag short-circuits resolveOperatedBy to "" (dark
//     deploy stays inert);
//   - the responder is sentinel-class (system:* / fleet:*) — chain-stop
//     applies symmetrically to the responder side (R3 §3, R2 §5.2) — or a
//     machine principal (machine:*), which never takes dibs;
//   - the decision bead has no requesting_agent_bead_id field (e.g.
//     externally-created decision with no agent owner);
//   - the agent bead is missing or already bound to the responder.
//
// The mutation_history row written here mirrors the maybeFlipDibs shape
// (field='operated_by', mutation_type='update') so it surfaces as a
// 'dibs-flip' source_kind in bead_operator_history. The accompanying
// "decision-answer rebind" slog line carries the decision_bead_id so
// audit consumers can correlate the rebind to the answering decision.
func (s *BeadsServer) maybeRebindAgentOnDecisionAnswer(ctx context.Context, decision *model.Bead, actor string) {
	if s == nil || s.store == nil || decision == nil {
		return
	}
	responder, _ := s.resolveOperatedByPure(ctx)
	if !takesDibs(responder) {
		return
	}
	// Resolve the FK alongside the TEXT value so the synthetic flipMut
	// row below carries operated_by_id during the dual-write window. This
	// path doesn't fire M2 — resolveOperatedByPure already skipped it, and
	// resolveOperator also doesn't, so the count of M2 increments stays
	// 1:1 with mutation rows the resolver actually attributed.
	var responderID *int64
	if id, ok := s.resolveOperator(ctx, responder); ok {
		responderID = &id
	}
	if len(decision.Fields) == 0 {
		return
	}
	var fields map[string]any
	if err := json.Unmarshal(decision.Fields, &fields); err != nil {
		return
	}
	rawAgent, _ := fields["requesting_agent_bead_id"].(string)
	agentID := strings.TrimSpace(rawAgent)
	if agentID == "" {
		return
	}

	current, err := s.store.GetBeadOperator(ctx, agentID)
	if err != nil {
		slog.Warn("decision-answer rebind: failed to read agent operator",
			"agent_bead_id", agentID,
			"decision_bead_id", decision.ID,
			"error", err,
		)
		return
	}
	if current == responder {
		return
	}
	if isProtectedDibsCurrent(current) {
		slog.Info("decision-answer rebind: skipping — current operator is automation-class",
			"agent_bead_id", agentID,
			"decision_bead_id", decision.ID,
			"current", current,
			"would_be", responder,
		)
		return
	}

	// Push: stamp Fields["operated_by"] on the agent bead so the pod's
	// next gb prime / kd invocation reads the rebound operator (R2 §5.2
	// step 3). The server-side resolver's agent-bead fallback also keys
	// off this field for static-token writes whose RPC ctx didn't carry
	// X-Operated-By.
	if err := s.mergeBeadFields(ctx, agentID, map[string]any{
		"operated_by": responder,
	}); err != nil {
		slog.Warn("decision-answer rebind: failed to merge agent operated_by field",
			"agent_bead_id", agentID,
			"decision_bead_id", decision.ID,
			"error", err,
		)
		return
	}

	// Pull: flip beads.operated_by_id so the viewer chip and dibs queries
	// see the rebound operator immediately. Post-cutover (migration 017),
	// UpdateBeadOperator takes the FK id; skip when the resolver couldn't
	// produce one (best-effort, matches maybeFlipDibs).
	if responderID == nil {
		slog.Warn("decision-answer rebind: skipping flip — responder FK not resolved",
			"agent_bead_id", agentID,
			"decision_bead_id", decision.ID,
			"responder", responder,
		)
		return
	}
	if err := s.store.UpdateBeadOperator(ctx, agentID, *responderID); err != nil {
		slog.Warn("decision-answer rebind: failed to update agent operator",
			"agent_bead_id", agentID,
			"decision_bead_id", decision.ID,
			"from", current,
			"to", responder,
			"error", err,
		)
		return
	}

	flipMut := &model.MutationRow{
		BeadID:       agentID,
		Actor:        actor,
		Field:        "operated_by",
		OldValue:     current,
		NewValue:     responder,
		MutationType: model.MutationTypeUpdate,
		OperatedBy:   responder,
		OperatedByID: responderID,
	}
	if err := s.store.RecordMutation(ctx, flipMut); err != nil {
		slog.Warn("decision-answer rebind: failed to record flip mutation",
			"agent_bead_id", agentID,
			"decision_bead_id", decision.ID,
			"error", err,
		)
		return
	}
	slog.Info("decision-answer rebind",
		"agent_bead_id", agentID,
		"decision_bead_id", decision.ID,
		"actor", actor,
		"prev_operator", current,
		"new_operator", responder,
	)
}

// recordCreate records mutation_history rows for a bead's birth.
//
// Two rows are written when the resolver yields a non-empty operator:
//
//  1. field='*' mutation_type='create' — the bead's birth marker.
//  2. field='operated_by' mutation_type='create' — paired audit row that
//     bead_operator_history (R4 §4 Option C) classifies as
//     source_kind='create'.
//
// The dibs flip path (maybeFlipDibs) only writes a field='operated_by' row
// when the resolved operator differs from the bead's current operated_by.
// At create time the row was just inserted with operated_by==value so dibs
// short-circuits, leaving the bead_operator_history view blind to the
// bead's first owner. This explicit paired write fills that gap without
// re-firing the M2 per-write metric (one resolver call covers the create
// pair, mirroring the synthetic dibs-flip row in maybeFlipDibs).
//
// Sentinels (system:*, fleet:*) are recorded — the view's CASE orders
// mutation_type='create' above 'sentinel-attempt' so they surface as
// 'create' rows, and audit-time visibility of sentinel-created beads is
// desirable (R3 §5.5).
func (s *BeadsServer) recordCreate(ctx context.Context, beadID, actor string) {
	value, valueID, source, ok := s.resolveAuditOperatorRequired(ctx)
	if !ok {
		// Best-effort observability: skip when no operator is attributable
		// so the migration-026 NOT VALID check on
		// mutation_history.operated_by_id never sees a NULL insert.
		slog.Warn("skipping recordCreate: operator unresolvable",
			"bead_id", beadID, "operated_by_source", source)
		return
	}

	starMut := &model.MutationRow{
		BeadID:       beadID,
		Actor:        actor,
		Field:        "*",
		OldValue:     "",
		NewValue:     "<created>",
		MutationType: model.MutationTypeCreate,
		OperatedBy:   value,
		OperatedByID: &valueID,
	}
	if err := s.store.RecordMutation(ctx, starMut); err != nil {
		slog.Warn("failed to record mutation history",
			"bead_id", beadID,
			"field", "*",
			"error", err,
			"operated_by", value,
			"operated_by_source", source,
		)
		return
	}
	slog.Info("mutation",
		"bead_id", beadID,
		"actor", actor,
		"field", "*",
		"mutation_type", model.MutationTypeCreate,
		"operated_by", value,
		"operated_by_source", source,
	)

	if value == "" {
		return
	}
	opMut := &model.MutationRow{
		BeadID:       beadID,
		Actor:        actor,
		Field:        "operated_by",
		OldValue:     "",
		NewValue:     value,
		MutationType: model.MutationTypeCreate,
		OperatedBy:   value,
		OperatedByID: &valueID,
	}
	if err := s.store.RecordMutation(ctx, opMut); err != nil {
		slog.Warn("failed to record operated_by create row",
			"bead_id", beadID,
			"error", err,
			"operated_by", value,
			"operated_by_source", source,
		)
		return
	}
	slog.Info("mutation",
		"bead_id", beadID,
		"actor", actor,
		"field", "operated_by",
		"mutation_type", model.MutationTypeCreate,
		"operated_by", value,
		"operated_by_source", source,
	)
}

// recordDelete records a single delete-mutation row marking the bead's removal.
func (s *BeadsServer) recordDelete(ctx context.Context, beadID, actor string) {
	s.recordMutation(ctx, beadID, actor, "*", "<existed>", "", model.MutationTypeDelete)
}

// recordRestore records a single restore-mutation row marking that a
// soft-deleted bead was undeleted.
func (s *BeadsServer) recordRestore(ctx context.Context, beadID, actor string) {
	s.recordMutation(ctx, beadID, actor, "*", "<deleted>", "<restored>", model.MutationTypeRestore)
}

// recordUpdates records one row per (field, old→new) pair in changes.
// oldValues is the snapshot taken BEFORE the update was applied. Fields whose
// old value is missing from oldValues record an empty old_value. The "fields"
// row stores only the keys that changed (fieldsHistoryValues), and is skipped
// when none did.
func (s *BeadsServer) recordUpdates(ctx context.Context, beadID, actor string, changes, oldValues map[string]any) {
	for field, newVal := range changes {
		oldStr, newStr, ok := updateHistoryValues(field, oldValues[field], newVal)
		if !ok {
			continue
		}
		s.recordMutation(ctx, beadID, actor, field, oldStr, newStr, model.MutationTypeUpdate)
	}
}

// recordUpdatesNoDibs is recordUpdates for a patch that is not an ownership
// signal (a reaction on someone else's message), so it never flips dibs.
func (s *BeadsServer) recordUpdatesNoDibs(ctx context.Context, beadID, actor string, changes, oldValues map[string]any) {
	for field, newVal := range changes {
		oldStr, newStr, ok := updateHistoryValues(field, oldValues[field], newVal)
		if !ok {
			continue
		}
		s.recordMutationNoDibs(ctx, beadID, actor, field, oldStr, newStr, model.MutationTypeUpdate)
	}
}

// updateHistoryValues renders one changed column's old_value and new_value.
// ok is false for a "fields" change in which no key changed.
func updateHistoryValues(field string, oldVal, newVal any) (string, string, bool) {
	if field == "fields" {
		return fieldsHistoryValues(oldVal, newVal)
	}
	return fmtValue(oldVal), fmtValue(newVal), true
}

func fmtValue(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}
