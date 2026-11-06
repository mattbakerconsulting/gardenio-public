package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"gardenio/kbeads/internal/model"
	"gardenio/kbeads/internal/store"
)

// This file implements ADR docs/adr/taints.md §4.2 row 2 / §5.2 (gd-Kebd0XJHZ2):
// assignment propagation. Two call sites trigger it, both funnelling into
// resolveAssignTaints:
//
//   - createBead, when a new type=agent bead is created already carrying a
//     task (fields.task_id) — the "agent bead created with a task" half of
//     the ADR row.
//   - updateBead, when an existing bead's `assignee` is set to a type=agent
//     bead — the "assignee set to an agent" half.
//
// Like resolveCreateTaints, the bead_taints marker propagates
// unconditionally: §4.2 row 2 puts assignment under D2 ("union, never
// automatic removal"), so the agent bead ALWAYS gets the task's taints. What
// §5.2 makes conditional is only the taint_grants row: "a human cannot grant
// an agent a taint the human does not hold." resolveAssignTaints checks the
// ASSIGNING principal (the caller of createBead/updateBead) against
// callerHoldsTaintGrant for each of the task's taints and withholds only the
// grant — never the marker — for any taint the assigner does not hold, or
// when no server-resolvable granter exists at all (a machine/static caller).
// An agent bead can therefore carry a marker with no matching grant of its
// own; that is expected here (not the read-gate invariant the create path
// preserves) because the marker's job on the assignment path is to keep the
// taint visible in the propagation chain — every bead this agent creates
// afterwards still inherits it via resolveCreateTaints — while the grant
// gates only this agent bead's own reads.
//
// gd-Kebd0XJHZ2's acceptance criteria reads as ambiguous between "reject the
// whole assignment" and "the assignment succeeds but the agent doesn't
// inherit the grant" when the assigner lacks the grant. This implementation
// picks the latter: an assignee change is not itself a taint operation, and
// an assigner who is simply missing a grant for ONE of a task's several
// taints should not be blocked from assigning the task at all — the agent
// just does not gain read access via that taint. See resolveAssignTaints for
// the per-taint grant skip.

// pendingTaintGrant is a resolved (taint, granter) pair waiting to be written
// as a taint_grants row for the agent bead being assigned a task, once that
// agent bead's id is known. See resolveAssignTaints, applyPendingAssignGrants.
type pendingTaintGrant struct {
	TaintID   int64
	GrantedBy int64
}

// assignGrantFallbackTTL bounds the stored expires_at for a taint_grants row
// written by automatic assignment propagation. It is deliberately generous
// (a year) because it is only a defense-in-depth backstop: the ADR's actual
// bound — "close of the agent bead" (§5.2, §10.4) — is enforced by
// revokeAssignTaintGrants at the close cascade (afterBeadClosed), not by this
// stored value. The explicit-grant endpoint's agentGrantExpiry cannot be
// reused here: at assignment time the agent bead is still open, and that
// helper's own doc comment says defaulting to "now" would immediately expire
// the grant it just wrote (see http_taint_grants.go).
const assignGrantFallbackTTL = 365 * 24 * time.Hour

// taskIDFromFields extracts fields.task_id from a bead's extensible fields
// blob, mirroring projectFromFields (write_authz.go). Used to detect the
// createBead half of ADR §4.2 row 2: "an agent bead created already carrying
// a task".
func taskIDFromFields(fields json.RawMessage) string {
	if len(fields) == 0 {
		return ""
	}
	var f struct {
		TaskID string `json:"task_id"`
	}
	if json.Unmarshal(fields, &f) == nil {
		return f.TaskID
	}
	return ""
}

// resolveAssignTaints computes the taint set and grant set an agent bead
// should inherit from being assigned taskBeadID, per docs/adr/taints.md §4.2
// row 2 / §5.2. Every taint currently on the task is ALWAYS returned as a
// pending bead_taints marker (D2: union, never automatic removal — the
// assigner's own grants play no part in whether the marker propagates). The
// matching taint_grants row is returned only for a taint the ASSIGNING
// principal (the caller in ctx — the createBead/updateBead caller, never
// client-supplied) itself holds a grant for (callerHoldsTaintGrant); a taint
// the assigner lacks a grant for still taints the agent bead, it just grants
// nothing alongside it.
//
// Returns a nil grant slice (taints still populated) when the caller cannot
// be resolved to a server-derived operator id (granterOperatorID) — ADR §11
// invariant #1 forbids a client-supplied granted_by, so with no resolvable
// granter (e.g. a machine/static caller) there is no principal to attribute
// a grant to, but the marker still propagates.
func (s *BeadsServer) resolveAssignTaints(ctx context.Context, taskBeadID string) ([]pendingBeadTaint, []pendingTaintGrant, error) {
	taskTaints, err := s.store.GetBeadTaints(ctx, taskBeadID)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving task bead taints: %w", err)
	}
	if len(taskTaints) == 0 {
		return nil, nil, nil
	}

	granterID, hasGranter := s.granterOperatorID(ctx)

	var pendingTaints []pendingBeadTaint
	var pendingGrants []pendingTaintGrant
	for _, bt := range taskTaints {
		pendingTaints = append(pendingTaints, pendingBeadTaint{
			TaintID:      bt.TaintID,
			Source:       model.TaintSourceInheritAssign,
			SourceBeadID: taskBeadID,
		})
		if !hasGranter {
			continue
		}
		taint, err := s.store.GetTaint(ctx, bt.TaintName)
		if err != nil {
			return nil, nil, fmt.Errorf("looking up taint %q: %w", bt.TaintName, err)
		}
		holds, err := s.callerHoldsTaintGrant(ctx, taint)
		if err != nil {
			return nil, nil, fmt.Errorf("checking assigner's grant for taint %q: %w", bt.TaintName, err)
		}
		if !holds {
			continue
		}
		pendingGrants = append(pendingGrants, pendingTaintGrant{
			TaintID:   bt.TaintID,
			GrantedBy: granterID,
		})
	}
	return pendingTaints, pendingGrants, nil
}

// applyPendingAssignGrants writes the taint_grants rows resolveAssignTaints
// computed for agentBeadID, expiring assignGrantFallbackTTL from now (see its
// doc comment for why that bound — not the agent bead's actual close — is
// stored). Callers should invoke this alongside applyPendingBeadTaints,
// through the same transactional store handle, so the marker and any grant
// resolveAssignTaints paired with it land atomically. `pending` is commonly
// shorter than the marker list applyPendingBeadTaints just wrote — an agent
// bead carrying a marker with no grant (assigner lacked one, or none was
// resolvable) is the expected shape, not a bug; see resolveAssignTaints.
func applyPendingAssignGrants(ctx context.Context, tx store.Store, agentBeadID string, pending []pendingTaintGrant, now time.Time) error {
	if len(pending) == 0 {
		return nil
	}
	expiresAt := now.Add(assignGrantFallbackTTL)
	for _, p := range pending {
		if err := tx.GrantTaint(ctx, &model.TaintGrant{
			TaintID:       p.TaintID,
			PrincipalKind: model.PrincipalAgent,
			Principal:     agentBeadID,
			GrantedBy:     p.GrantedBy,
			ExpiresAt:     &expiresAt,
		}); err != nil {
			return fmt.Errorf("granting taint %d to agent %s: %w", p.TaintID, agentBeadID, err)
		}
	}
	return nil
}

// revokeAssignTaintGrants is the real enforcement of §5.2's "expires_at is
// the agent bead's close": called from afterBeadClosed when a type=agent
// bead closes, it revokes every agent-principal taint_grants row the agent
// bead holds, for every taint currently on the bead — not scoped to
// assignment-derived grants specifically, because §5.2 makes every
// agent-principal grant derived (inherit:create or inherit:assign; an agent
// bead is never the target of an explicit `kd taint grant`), so "all of this
// agent's grants" and "all of this agent's assignment-derived grants" are
// the same set in practice. It walks the agent's OWN current bead_taints
// (the same set resolveCreateTaints/resolveAssignTaints wrote onto it)
// rather than tracking provenance per grant — RevokeTaint is idempotent, so
// revoking a taint the agent was never actually granted (e.g. one it carries
// only as a marker, per resolveAssignTaints's no-grant case) is a harmless
// no-op. Best-effort: a failure here must never block the bead close itself.
func (s *BeadsServer) revokeAssignTaintGrants(ctx context.Context, closedBead *model.Bead) {
	taints, err := s.store.GetBeadTaints(ctx, closedBead.ID)
	if err != nil {
		slog.Warn("auto-close: failed to list bead taints for grant revoke",
			"bead", closedBead.ID, "err", err.Error(), "feature", "gd-Kebd0XJHZ2")
		return
	}
	for _, bt := range taints {
		if _, err := s.store.RevokeTaint(ctx, bt.TaintID, model.PrincipalAgent, closedBead.ID); err != nil {
			slog.Warn("auto-close: failed to revoke taint grant on agent close",
				"bead", closedBead.ID, "taint_id", bt.TaintID, "err", err.Error(), "feature", "gd-Kebd0XJHZ2")
		}
	}
	s.invalidateAgentGrantSet(closedBead.ID)
}
