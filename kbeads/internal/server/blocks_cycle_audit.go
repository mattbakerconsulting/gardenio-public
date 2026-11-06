package server

import (
	"context"
	"sort"

	"gardenio/kbeads/internal/model"
)

// BlocksCycle is one cycle found in the blocks graph: an ordered ring of bead
// IDs where each bead is blocked-by the next, and the last is blocked-by the
// first. Members is the canonical, sorted set of bead IDs in the cycle (a
// stable identity independent of traversal start), and Size is len(Members).
type BlocksCycle struct {
	Members []string `json:"members"`
	Size    int      `json:"size"`
}

// BlocksCycleAudit is the result of scanning the whole blocks graph for cycles.
type BlocksCycleAudit struct {
	EdgeCount  int           `json:"edge_count"`
	NodeCount  int           `json:"node_count"`
	CycleCount int           `json:"cycle_count"`
	Cycles     []BlocksCycle `json:"cycles"`
}

// findBlocksCycles reports every strongly-connected component of size > 1 (plus
// any self-loop) in the directed blocks graph described by edges, where an edge
// {BeadID, DependsOnID} means "BeadID is blocked-by DependsOnID". Each such SCC
// is a set of beads mutually reachable via blocks edges — i.e. a cycle (or a
// tangle of overlapping cycles) that pins every member as permanently
// undispatchable (ka-0000007984).
//
// It uses Tarjan's SCC algorithm, which is linear in |V|+|E| and finds every
// maximal strongly-connected component in one pass. Reporting SCCs (rather than
// enumerating simple cycles, which is exponential) gives one stable, bounded
// group per tangle — exactly the actionable unit for graph repair. The result
// is deterministic: members within a cycle are sorted, and cycles are ordered
// by their smallest member.
func findBlocksCycles(edges []model.DependencyEdge) []BlocksCycle {
	// Build adjacency: for a blocks edge (bead_id blocked-by depends_on_id),
	// the traversal direction that matches "reachable via blocks" is
	// bead_id -> depends_on_id (X can reach its blocker Y). A cycle in this
	// directed graph is exactly a blocks cycle.
	adj := make(map[string][]string)
	nodeSet := make(map[string]struct{})
	for _, e := range edges {
		adj[e.BeadID] = append(adj[e.BeadID], e.DependsOnID)
		nodeSet[e.BeadID] = struct{}{}
		nodeSet[e.DependsOnID] = struct{}{}
	}

	// Self-loops (bead_id == depends_on_id) are 1-cycles Tarjan reports as a
	// singleton SCC; track them explicitly so a singleton with a self-edge is
	// still surfaced.
	selfLoop := make(map[string]bool)
	for _, e := range edges {
		if e.BeadID == e.DependsOnID {
			selfLoop[e.BeadID] = true
		}
	}

	// Iterative Tarjan (explicit stack) so a deep/large graph cannot overflow
	// the goroutine stack.
	type frame struct {
		node    string
		childIx int
	}
	var (
		index    = 0
		indices  = make(map[string]int)
		lowlink  = make(map[string]int)
		onStack  = make(map[string]bool)
		tarStack []string
		out      []BlocksCycle
	)

	// Deterministic iteration order over roots.
	roots := make([]string, 0, len(nodeSet))
	for n := range nodeSet {
		roots = append(roots, n)
	}
	sort.Strings(roots)

	for _, root := range roots {
		if _, seen := indices[root]; seen {
			continue
		}
		callStack := []frame{{node: root}}
		indices[root] = index
		lowlink[root] = index
		index++
		tarStack = append(tarStack, root)
		onStack[root] = true

		for len(callStack) > 0 {
			f := &callStack[len(callStack)-1]
			children := adj[f.node]
			if f.childIx < len(children) {
				w := children[f.childIx]
				f.childIx++
				if _, seen := indices[w]; !seen {
					indices[w] = index
					lowlink[w] = index
					index++
					tarStack = append(tarStack, w)
					onStack[w] = true
					callStack = append(callStack, frame{node: w})
				} else if onStack[w] {
					if indices[w] < lowlink[f.node] {
						lowlink[f.node] = indices[w]
					}
				}
				continue
			}

			// Done exploring f.node's children: if it's an SCC root, pop the SCC.
			if lowlink[f.node] == indices[f.node] {
				var comp []string
				for {
					top := tarStack[len(tarStack)-1]
					tarStack = tarStack[:len(tarStack)-1]
					onStack[top] = false
					comp = append(comp, top)
					if top == f.node {
						break
					}
				}
				if len(comp) > 1 || selfLoop[f.node] {
					sort.Strings(comp)
					out = append(out, BlocksCycle{Members: comp, Size: len(comp)})
				}
			}

			// Pop the frame and propagate its lowlink to the parent.
			child := f.node
			callStack = callStack[:len(callStack)-1]
			if len(callStack) > 0 {
				parent := callStack[len(callStack)-1].node
				if lowlink[child] < lowlink[parent] {
					lowlink[parent] = lowlink[child]
				}
			}
		}
	}

	// Deterministic order: by first (smallest) member.
	sort.Slice(out, func(i, j int) bool {
		return out[i].Members[0] < out[j].Members[0]
	})
	return out
}

// AuditBlocksCycles scans the entire blocks graph and returns every cycle
// (strongly-connected tangle) present in it. It is the read-only counterpart to
// the write-time wouldCreateBlocksCycle guard: the guard stops NEW cycles, this
// surfaces cycles ALREADY in the live graph (ka-0000007984, ask #2).
func (s *BeadsServer) AuditBlocksCycles(ctx context.Context) (*BlocksCycleAudit, error) {
	edges, err := s.store.GetDependencyEdgesByType(ctx, model.DepBlocks)
	if err != nil {
		return nil, err
	}
	cycles := findBlocksCycles(edges)

	nodes := make(map[string]struct{})
	for _, e := range edges {
		nodes[e.BeadID] = struct{}{}
		nodes[e.DependsOnID] = struct{}{}
	}
	return &BlocksCycleAudit{
		EdgeCount:  len(edges),
		NodeCount:  len(nodes),
		CycleCount: len(cycles),
		Cycles:     cycles,
	}, nil
}
