package server

import (
	"context"
	"log/slog"

	"gardenio/kbeads/internal/model"
)

// wouldCloseDependencyCycle reports whether inserting an edge
// (beadID depends-on dependsOnID) of type edgeType would close a cycle in the
// subgraph of edges of that single type. It is the shared walk behind both the
// parent-child cycle guard (ka-0000006899) and the blocks cycle guard
// (ka-0000007984): the deps convention is identical for both edge kinds —
// bead_id is the dependent (child / blocked) side and depends_on_id is the
// dependency (parent / blocker) side — so cycle detection is the same graph
// walk with a different type filter.
//
// Direction: the new edge makes beadID depend on dependsOnID. A cycle forms iff
// beadID is ALREADY (transitively) a dependency of dependsOnID — i.e. walking
// the dependency chain UP from dependsOnID (following each node's depends_on_id
// edges of edgeType) eventually reaches beadID. The walk carries a visited set
// so it terminates even if the stored graph already contains a cycle, and a
// read error fails OPEN (the insert is allowed) so a transient store failure
// never blocks a dep-add. A beadID == dependsOnID self-edge is the trivial
// 1-cycle and is always reported.
func (s *BeadsServer) wouldCloseDependencyCycle(ctx context.Context, beadID, dependsOnID string, edgeType model.DependencyType) bool {
	if s == nil || s.store == nil {
		return false
	}
	if beadID == "" || dependsOnID == "" {
		return false
	}
	if beadID == dependsOnID {
		return true
	}

	target := beadID // reaching it from the dependency side = cycle
	visited := map[string]bool{}
	frontier := []string{dependsOnID}
	for len(frontier) > 0 {
		node := frontier[0]
		frontier = frontier[1:]
		if visited[node] {
			continue
		}
		visited[node] = true

		deps, err := s.store.GetDependencies(ctx, node)
		if err != nil {
			// Fail-open: a transient read error must not block the insert.
			return false
		}
		for _, d := range deps {
			if d.Type != edgeType {
				continue
			}
			next := d.DependsOnID // node's dependency (deps row bead_id=node)
			if next == target {
				return true
			}
			if !visited[next] {
				frontier = append(frontier, next)
			}
		}
	}
	return false
}

// wouldCreateBlocksCycle reports whether inserting dep — a blocks edge whose
// bead_id is the BLOCKED side and depends_on_id is the BLOCKER side — would
// close a cycle in the blocks graph.
//
// Background (ka-0000007984): a cycle in the blocks graph (e.g. the mutual pair
// A blocked-by B, B blocked-by A) makes every bead in and downstream of the
// cycle permanently undispatchable — no dispatcher lane can ever pick them up
// because each is forever blocked by an open blocker, and nothing surfaces the
// condition (each bead individually looks like an ordinary, legitimately
// blocked bead). A concrete 2-cycle pinned six gardenio beads for 12 days. The
// write is the cheapest place to catch this, so the cycle-forming edge is
// rejected at insert time.
//
// Only model.DepBlocks rows are considered; the walk shares
// wouldCloseDependencyCycle with the parent-child guard.
func (s *BeadsServer) wouldCreateBlocksCycle(ctx context.Context, dep *model.Dependency) bool {
	if dep == nil || dep.Type != model.DepBlocks {
		return false
	}
	return s.wouldCloseDependencyCycle(ctx, dep.BeadID, dep.DependsOnID, model.DepBlocks)
}

// rejectBlocksCycleLog emits the standard structured warning when a blocks
// cycle is rejected, shared by the gRPC and HTTP add-dependency paths so the
// log shape stays identical on both surfaces.
func rejectBlocksCycleLog(dep *model.Dependency) {
	slog.Warn("rejecting blocks dep that would create a cycle",
		"bead_id", dep.BeadID,
		"depends_on_id", dep.DependsOnID,
		"created_by", dep.CreatedBy,
		"bead", "ka-0000007984",
	)
}
