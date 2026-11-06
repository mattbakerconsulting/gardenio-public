package server

import (
	"context"
	"fmt"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// dependencyTypeDerivedFrom is not (yet) a model.DependencyType constant —
// only "triggered-by" (model.DepTriggeredBy) is a real edge type in this
// codebase today; "derived-from" is named in docs/adr/taints.md §4.2 row 1
// alongside it for a future writer. DependencyType is a plain string, so
// resolveDependencyCreateTaints matches on both without waiting on that.
const dependencyTypeDerivedFrom model.DependencyType = "derived-from"

// resolveWriteTaints computes the taint set a write to an EXISTING bead must
// union onto the target, per docs/adr/taints.md §4.2 row 4: any write to a
// bead by a tainted agent (update, comment, label, dep, close) taints the
// target bead with the writer's current taints, source inherit:write.
//
// The writer is resolveTaintAgentBeadID(ctx) — the calling static-token
// agent, or the escalation-replay attribution override. An untainted writer
// (including every OIDC human, the primary static token, and fleet tokens —
// principals hold grants, not taints) resolves to nil: zero rows are
// written, matching resolveCreateTaints' zero-caller-taint case.
func (s *BeadsServer) resolveWriteTaints(ctx context.Context) ([]pendingBeadTaint, error) {
	agentTaints, err := s.taintContextOf(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolving caller taint context: %w", err)
	}
	if len(agentTaints) == 0 {
		return nil, nil
	}
	agentBeadID, _ := resolveTaintAgentBeadID(ctx)
	pending := make([]pendingBeadTaint, 0, len(agentTaints))
	for taintID := range agentTaints {
		pending = append(pending, pendingBeadTaint{
			TaintID:      taintID,
			Source:       model.TaintSourceInheritWrite,
			SourceBeadID: agentBeadID,
		})
	}
	return pending, nil
}

// propagateWriteTaint unions the writing agent's current taints onto beadID
// after a write has already committed — update, comment, add/remove label,
// add dependency, close — per docs/adr/taints.md §4.2 row 4. appliedBy is the
// server-derived actor (never client-supplied) recorded on each new
// bead_taints row. A no-op (untainted writer) returns nil without a store
// call.
//
// This is deliberately NOT part of the write's own transaction — none of
// update/comment/label/dep/close run inside one today — but ApplyTaint's own
// ON CONFLICT DO NOTHING makes the call idempotent, and the underlying
// mutation has already durably committed by the time this runs: an error
// here is still returned to the caller as a hard failure (mirroring the
// existing reconcileLabels-after-UpdateBead precedent in updateBead), not
// swallowed as best-effort.
func (s *BeadsServer) propagateWriteTaint(ctx context.Context, beadID, appliedBy string) error {
	pending, err := s.resolveWriteTaints(ctx)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	return applyPendingBeadTaints(ctx, s.store, beadID, pending, appliedBy)
}

// addDependencyWithTaintPropagation inserts dep and, when it is of type
// triggered-by or derived-from, unions the taints currently on the
// dependency's TARGET bead (dep.DependsOnID) onto the DEPENDENT bead
// (dep.BeadID) in the SAME transaction, source inherit:create,
// source_bead_id = dep.DependsOnID.
//
// This is the triggered-by/derived-from half of docs/adr/taints.md §4.2 row 1
// that createBead itself cannot see (see taint_propagate_create.go): every
// real writer — the HTTP/gRPC AddDependency endpoints, formula pour's
// molecule triggered-by wiring, maybeAddTriggeredByOnClaim — adds such an
// edge via a separate post-create call like this one, after the dependent
// bead already exists. No opt-out; ApplyTaint's ON CONFLICT DO NOTHING makes
// re-adding an edge whose target's taints already landed a harmless no-op.
//
// It does NOT also apply the caller's own inherit:write taint (ADR row 4)
// onto the dependent bead — that union is independent and only applies to
// writer-attributed calls (a human/agent hitting the AddDependency API), not
// system-internal callers like formula pour or the triggered-by-on-claim
// auto-link, which run under a machine/OIDC identity that carries no taints
// of its own anyway. Callers that need it call propagateWriteTaint
// separately after this succeeds.
func (s *BeadsServer) addDependencyWithTaintPropagation(ctx context.Context, dep *model.Dependency) error {
	appliedBy := actorFromContext(ctx)
	return s.store.RunInTransaction(ctx, func(tx store.Store) error {
		if err := tx.AddDependency(ctx, dep); err != nil {
			return err
		}
		if dep.Type != model.DepTriggeredBy && dep.Type != dependencyTypeDerivedFrom {
			return nil
		}
		// GetAllTaintsForBead, not GetTaintsForBead: this is INHERITANCE, and
		// inheritance carries every taint regardless of effects. This PR
		// narrowed GetTaintsForBead to `'gate' = ANY(t.effects)` for the
		// gating decision, which is right there and wrong here -- a taint
		// whose effects are e.g. ['warn'] would propagate when a bead is
		// created (taint_propagate_create.go:64 already uses the unfiltered
		// call for the identical TaintSourceInheritCreate union) but be
		// silently dropped across a triggered-by/derived-from edge. Two
		// paths, one semantic; they must read the same rows.
		targetTaintIDs, err := tx.GetAllTaintsForBead(ctx, dep.DependsOnID)
		if err != nil {
			return fmt.Errorf("resolving dependency target taints: %w", err)
		}
		if len(targetTaintIDs) == 0 {
			return nil
		}
		pending := make([]pendingBeadTaint, 0, len(targetTaintIDs))
		for _, taintID := range targetTaintIDs {
			pending = append(pending, pendingBeadTaint{
				TaintID:      taintID,
				Source:       model.TaintSourceInheritCreate,
				SourceBeadID: dep.DependsOnID,
			})
		}
		return applyPendingBeadTaints(ctx, tx, dep.BeadID, pending, appliedBy)
	})
}
