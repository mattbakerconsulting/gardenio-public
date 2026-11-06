package server

import (
	"context"
	"log/slog"
	"time"

	"gardenio/kbeads/internal/model"
)

// epicOnChildSide reports whether dep is a parent-child edge whose bead_id
// (the child side) resolves to a type=epic bead while its depends_on_id
// (the parent side) resolves to a NON-epic bead — i.e. the unambiguous
// "reversed" shape behind ka-0000006880.
//
// Background: the ParentSubtreeID recursive CTE
// (internal/store/postgres/queries.go) seeds the subtree from
// `SELECT bead_id FROM deps WHERE depends_on_id = <root>` — it expects
// children on the bead_id side and the parent on the depends_on_id side.
// A row stored as bead_id=epic, depends_on_id=child is invisible to that
// CTE: the subtree count comes back 0 and every gardener `bead_query`
// loop_end ("open children == 0") fires immediately, so the gardener is
// DOA. These rows are typically produced by a
// `kd dep add <epic> <child> -t parent-child` loop with the arguments
// swapped. An epic should essentially never be the bead_id (child) side
// of a parent-child edge.
//
// The check is best-effort: epic→epic edges (sub-epics) and any row
// whose endpoints cannot be resolved are reported as not-reversed, and a
// lookup error is non-fatal (logged-free, swallowed) so callers never
// block on a transient read failure. Only model.DepParentChild rows are
// considered.
func (s *BeadsServer) epicOnChildSide(ctx context.Context, dep *model.Dependency) bool {
	if s == nil || s.store == nil || dep == nil {
		return false
	}
	if dep.Type != model.DepParentChild {
		return false
	}
	if dep.BeadID == "" || dep.DependsOnID == "" || dep.BeadID == dep.DependsOnID {
		return false
	}

	childSide, err := s.store.GetBead(ctx, dep.BeadID)
	if err != nil || childSide == nil {
		// Fail-open: a missing/unreadable endpoint should not block the
		// dep insert (the FK constraint rejects a truly absent bead).
		return false
	}
	if childSide.Type != model.TypeEpic {
		return false
	}

	parentSide, err := s.store.GetBead(ctx, dep.DependsOnID)
	if err != nil || parentSide == nil {
		return false
	}
	// epic→epic is a legitimate sub-epic edge; only flag epic(child) →
	// non-epic(parent), the unambiguous reversed shape.
	return parentSide.Type != model.TypeEpic
}

// closedParentSide reports the PARENT bead of a canonical parent-child edge
// (bead_id = child, depends_on_id = parent) when that parent is already
// CLOSED, and nil otherwise. It is the detector behind the closed-parent
// guard (ka-0000005756).
//
// Background: a parent-child edge onto an already-closed epic is accepted
// silently today. The new child is status=open, so it enters `gb ready` as a
// claimable row whose epic died — sometimes months earlier — and no RDY-* gate
// scores it (RDY-3 only asks "has a parent", never "is that parent alive").
// Worse, the auto-close cascade (auto_close.go) explicitly SKIPS already-closed
// parents, so the edge never triggers re-evaluation: the child cannot be
// drained by the mechanism that drains every other child and stays claimable
// indefinitely.
//
// The check reuses the parent read the caller's cycle/canonicalize guards
// already perform conceptually — a single GetBead on the depends_on_id side —
// at the exact point the insert decision is made, so it costs no extra
// architecture. It runs AFTER canonicalizeParentChildDep so the orientation is
// final: dep.DependsOnID is the true parent.
//
// Scope: only model.StatusClosed is rejected — the measured, drain-invisible
// case (auto_close.go skips `== StatusClosed`). deferred/blocked parents are
// legitimately re-openable and are NOT flagged. The check is best-effort: a
// missing/unreadable parent, a non-parent-child edge, or a self-edge returns
// nil (fail-open) so a transient store failure never blocks a dep insert.
func (s *BeadsServer) closedParentSide(ctx context.Context, dep *model.Dependency) *model.Bead {
	if s == nil || s.store == nil || dep == nil {
		return nil
	}
	if dep.Type != model.DepParentChild {
		return nil
	}
	if dep.BeadID == "" || dep.DependsOnID == "" || dep.BeadID == dep.DependsOnID {
		return nil
	}
	parentSide, err := s.store.GetBead(ctx, dep.DependsOnID)
	if err != nil || parentSide == nil {
		// Fail-open: a missing/unreadable parent should not block the insert
		// (the FK constraint rejects a truly absent bead).
		return nil
	}
	if parentSide.Status == model.StatusClosed {
		return parentSide
	}
	return nil
}

// closedAtString renders a bead's closed_at as an RFC3339 string for log
// context, or "" when unset (a closed bead predating the closed_at stamp, or a
// partial). Best-effort diagnostic only.
func closedAtString(b *model.Bead) string {
	if b == nil || b.ClosedAt == nil {
		return ""
	}
	return b.ClosedAt.UTC().Format(time.RFC3339)
}

// canonicalizeParentChildDep enforces the canonical direction of a
// parent-child dependency row (bead_id = CHILD, depends_on_id = PARENT)
// for the EXPLICIT dependency-add paths (HTTP handleAddDependency and the
// gRPC AddDependency), where the caller supplies both endpoints and may
// have swapped them.
//
// When epicOnChildSide reports the reversed shape, the two endpoints are
// swapped in place and the fix is logged loudly. Swapping (rather than
// rejecting) lets the call still succeed with the correct orientation,
// self-healing the caller's mistake. Returns true when a swap was made.
func (s *BeadsServer) canonicalizeParentChildDep(ctx context.Context, dep *model.Dependency) bool {
	if !s.epicOnChildSide(ctx, dep) {
		return false
	}
	slog.Warn("canonicalizing reversed parent-child dep (epic on child side)",
		"reversed_bead_id", dep.BeadID,
		"reversed_depends_on_id", dep.DependsOnID,
		"epic", dep.BeadID,
		"child", dep.DependsOnID,
		"created_by", dep.CreatedBy,
		"bead", "ka-0000006880",
	)
	dep.BeadID, dep.DependsOnID = dep.DependsOnID, dep.BeadID
	return true
}

// wouldCreateParentChildCycle reports whether inserting dep — a parent-child
// edge whose bead_id is the CHILD side and depends_on_id is the PARENT side —
// would close a cycle in the parent-child graph.
//
// Background (ka-0000006899): the ParentSubtreeID recursive CTE
// (internal/store/postgres/queries.go) counts an epic's transitive
// descendants. A parent-child cycle (e.g. an epic that lists one of its own
// children as its parent) makes the open epic a descendant of itself, so a
// gardener whose dormancy preflight is `bead_query > 0` over its subtree never
// reaches 0 even after every real child closes — the loop stays awake
// forever. The closed-root short-circuit (ka-0000008051) does not help when
// the root epic is legitimately still open. Rejecting the cycle-forming edge
// at insert time is the documented mitigation.
//
// Direction: the new edge makes dep.BeadID a child of dep.DependsOnID. A
// cycle forms iff dep.BeadID is ALREADY an ancestor of dep.DependsOnID — i.e.
// walking UP the existing parent-child edges from dep.DependsOnID eventually
// reaches dep.BeadID. The walk (shared with the blocks cycle guard via
// wouldCloseDependencyCycle) carries a visited set so it terminates even if the
// stored graph already contains a cycle, and a read error fails open (the
// insert is allowed) so a transient store failure never blocks dep-add. Only
// model.DepParentChild rows are considered; a bead_id == depends_on_id
// self-edge is the trivial 1-cycle and is always reported.
func (s *BeadsServer) wouldCreateParentChildCycle(ctx context.Context, dep *model.Dependency) bool {
	if dep == nil || dep.Type != model.DepParentChild {
		return false
	}
	return s.wouldCloseDependencyCycle(ctx, dep.BeadID, dep.DependsOnID, model.DepParentChild)
}
