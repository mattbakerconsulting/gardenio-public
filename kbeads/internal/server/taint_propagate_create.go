package server

import (
	"context"
	"encoding/json"
	"fmt"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// pendingBeadTaint is a resolved taint id/source/provenance waiting to be
// written as a bead_taints row once the new bead exists (inside the same
// create transaction). See resolveCreateTaints.
type pendingBeadTaint struct {
	TaintID      int64
	Source       string
	SourceBeadID string
}

// resolveCreateTaints computes the taint set a new bead must carry, per
// docs/adr/taints.md §4.2 row 1 (createBead): the caller agent's current
// taints (source inherit:create) unioned with the taints on the bead named
// by parentBeadID, when set (source inherit:parent) — the atomic
// parent_bead_id create-time wiring, i.e. the ADR's "when a --parent-task
// dep is set" case — unioned with the intake taints declared by the new
// bead's project, when set (source inherit:project-intake, §10.3; see
// resolveProjectIntakeTaints). labels/fields are the new bead's own
// create-time values (createBeadInput.Labels/Fields), passed directly since
// no *model.Bead exists yet at this point in createBead.
//
// Resolution happens here, between applyAutoInherit and authorizeWriteTitled per
// §4.2. Persistence happens later, inside the same transaction as the bead
// insert (applyPendingBeadTaints), so a caller can never observe the new
// bead before it carries its taints — the read gate's "nothing leaks by
// default" guarantee would otherwise have a window between the bead commit
// and the taint-row commit.
//
// The union with taints on a triggered-by/derived-from dep target (also
// named in §4.2 row 1) is not reachable from createBead today:
// createBeadInput has no field for wiring such a dep at create time (only
// ParentBeadID, the parent-child case) — every existing triggered-by writer
// (formula_apply.go, maybeAddTriggeredByOnClaim) adds the edge in a separate
// AddDependency call after the bead already exists, which is a write to an
// existing bead, not a createBead event. Land that source alongside whatever
// future task adds create-time triggered-by/derived-from wiring.
//
// X-No-Auto-Inherit is never consulted: per §4.2 there is no opt-out for
// taint propagation.
func (s *BeadsServer) resolveCreateTaints(ctx context.Context, parentBeadID string, labels []string, fields json.RawMessage) ([]pendingBeadTaint, error) {
	var pending []pendingBeadTaint

	agentTaints, err := s.taintContextOf(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving caller taint context: %w", err)
	}
	if len(agentTaints) > 0 {
		agentBeadID, _ := resolveTaintAgentBeadID(ctx)
		for taintID := range agentTaints {
			pending = append(pending, pendingBeadTaint{
				TaintID:      taintID,
				Source:       model.TaintSourceInheritCreate,
				SourceBeadID: agentBeadID,
			})
		}
	}

	if parentBeadID != "" {
		parentTaintIDs, err := s.store.GetAllTaintsForBead(ctx, parentBeadID)
		if err != nil {
			return nil, fmt.Errorf("resolving parent bead taints: %w", err)
		}
		for _, taintID := range parentTaintIDs {
			pending = append(pending, pendingBeadTaint{
				TaintID:      taintID,
				Source:       model.TaintSourceInheritParent,
				SourceBeadID: parentBeadID,
			})
		}
	}

	projectName := projectFromLabels(labels)
	if projectName == "" || projectName == "unknown" {
		projectName = projectFromFields(fields)
	}
	projectTaints, err := s.resolveProjectIntakeTaints(ctx, projectName)
	if err != nil {
		return nil, err
	}
	pending = append(pending, projectTaints...)

	return pending, nil
}

// applyPendingBeadTaints writes the taints resolveCreateTaints computed as
// bead_taints rows for beadID, attributed to appliedBy (the server-derived
// operator, never client-supplied). Callers MUST invoke this through the
// transactional store handle bead creation runs in, so the insert commits
// atomically with the bead itself. ApplyTaint's own ON CONFLICT DO NOTHING
// makes a duplicate taint id across sources (e.g. both the caller agent and
// the parent carry the same taint) a harmless no-op on the second write.
//
// This calls the SAME store method an explicit `kd taint apply` does. An
// earlier revision of this task added a second method, AddBeadTaint, with
// identical semantics -- written in parallel with ApplyTaint on another branch,
// against an ADR that did not say which one owned the write. They were
// collapsed onto ApplyTaint when the branches met.
//
// The `inserted` result is deliberately discarded here. Its purpose elsewhere
// is to stop a no-op re-apply writing a duplicate taint_audit row, and
// propagation writes NO audit row at all: taint_audit records grant, revoke,
// explicit apply and scrub-* only. inherit:* rows already carry their own
// provenance (source, source_bead_id, applied_by, applied_at), and auditing
// every bead created by a tainted agent would be the highest-volume write in
// the system for no signal (docs/adr/taints.md).
func applyPendingBeadTaints(ctx context.Context, tx store.Store, beadID string, pending []pendingBeadTaint, appliedBy string) error {
	for _, p := range pending {
		if _, err := tx.ApplyTaint(ctx, &model.BeadTaint{
			BeadID:       beadID,
			TaintID:      p.TaintID,
			Source:       p.Source,
			SourceBeadID: p.SourceBeadID,
			AppliedBy:    appliedBy,
		}); err != nil {
			return fmt.Errorf("applying taint %d (%s) to %s: %w", p.TaintID, p.Source, beadID, err)
		}
	}
	return nil
}
