package main

import (
	"maps"
	"sort"
)

// maps_workflow.go — the pure workflow projection for the /maps card-graph
// (task ka-0000004778 / "C2"; design docs/gardener-chaining-design-2026-06-18.md
// §3.5, §4). Given the gardener cards and the normalized predecessor→heir
// chaining edges (maps_chains.go), it partitions the chained gardeners into
// WORKFLOWS — the connected components of the `chains_to` edge set — orders the
// cards within each workflow predecessor→heir, and computes the per-edge
// handoff state and per-card halo ignition.
//
// It is pure (no http, no daemon, no clock): the caller fetches cards + edges,
// this owns the shape. That keeps the connected-component grouping, the
// topological ordering and the handoff/halo state unit-testable in isolation.

// mapsChainArrow is one rendered predecessor→heir arrow inside a workflow rail.
// Lit is the handoff signal: the design lights the arrow at the moment the
// predecessor reaches its terminal goal (GoalState == "achieved"), at which the
// heir's halo ignites (see mapsWorkflowCard.HaloIgnited). Dim (Lit == false)
// while the predecessor is still mid-loop.
type mapsChainArrow struct {
	Pred string // predecessor gardener id
	Heir string // heir gardener id
	Lit  bool   // handoff has fired (predecessor goal achieved)
}

// mapsWorkflowCard is a gardener card as it appears inside a workflow rail. It
// embeds the same mapsCardView the standalone grid renders so the visual idiom
// is identical, and adds the chain-specific overlay state.
type mapsWorkflowCard struct {
	mapsCardView

	// HaloIgnited is true when this card's predecessor has handed off to it —
	// i.e. some incoming `chains_to` edge is Lit (the predecessor reached its
	// terminal goal). The workflow root (no predecessor) is never ignited by a
	// handoff; it lights its own way as the entry point.
	HaloIgnited bool

	// IsRoot marks the workflow entry card (no incoming chain edge). DanglingHeir
	// marks a card that is referenced as an heir but whose bead never
	// materialized as a gardener on the board (a "dangling chain" — the design's
	// health concern, §3.4). A dangling heir renders as a ghost node so the
	// broken link is visible rather than silently dropped.
	IsRoot       bool
	DanglingHeir bool

	// PendingHeir marks a PLANNED-but-unmaterialized successor (task
	// ka-0000005273 / "C3"): the predecessor DREW this heir via `chain.to` but
	// the engine has not spawned it yet. It renders as a distinct "pending" node
	// — an authored successor waiting to be born — NOT a broken/dangling chain.
	// PendingLabel captions it; PendingFanOut marks a fan-out declaration.
	PendingHeir   bool
	PendingLabel  string
	PendingFanOut bool
	// PhaseIndex is the card's 1-based position in the workflow's predecessor→heir
	// order; PhaseTotal is the workflow's card count. Together they back the
	// "phase 2/4" collapse label.
	PhaseIndex int
	PhaseTotal int
}

// mapsWorkflowView is one workflow: a connected component of chaining edges,
// rendered as a left→right rail of gardener cards joined by directed arrows.
type mapsWorkflowView struct {
	// ID is a stable identifier for the workflow (the lexicographically-smallest
	// member id), used as the DOM/CSS anchor and the default name seed.
	ID string
	// Name is the human label. v0 is emergent (no anchor bead, per design §7.4
	// open question — "lean emergent for v0"): we name the workflow after its
	// root gardener ("<root name> workflow"), falling back to the id.
	Name string
	// Cards are the workflow's gardeners in predecessor→heir (topological) order.
	Cards []mapsWorkflowCard
	// Arrows are the directed handoff edges between consecutive/linked cards,
	// in (Pred, Heir) order.
	Arrows []mapsChainArrow

	// GardenerCount is the number of real gardener cards in the workflow.
	GardenerCount int
	// Phase is the 1-based index of the current "live" phase — the first card
	// that has not yet handed off (its terminal goal is not achieved). Equals
	// GardenerCount once the whole chain has completed. Backs "phase 2/4".
	Phase int
	// Dangling is true when the workflow contains at least one dangling heir (a
	// chain.to whose heir never materialized) — surfaces the health pill.
	Dangling bool
	// Pending is true when the workflow contains at least one PLANNED heir (a
	// drawn-but-not-yet-spawned `chain.to` successor, task ka-0000005273 / "C3").
	// Surfaces the "pending" authoring pill — distinct from the dangling/broken
	// health pill.
	Pending bool
}

// buildMapsWorkflows partitions the board's gardeners into workflows by the
// connected components of the chaining edges, and returns (workflows,
// standaloneIDs): the set of gardener ids that belong to NO chain (rendered in
// the plain card grid). cardsByID must contain every real gardener card on the
// board; edges are the normalized predecessor→heir set (maps_chains.go).
//
// Edges whose endpoints are BOTH unknown to cardsByID are dropped (a chain
// between two non-board gardeners is not this board's concern). An edge with a
// known predecessor and an unknown heir is kept as a DANGLING edge so the
// broken link renders — that is the whole point of the dangling-chain health
// pill (design §3.4). An edge with an unknown predecessor but a known heir is
// likewise kept (the heir's `succeeds` provenance pointed at an off-board
// predecessor); the predecessor renders as a dangling ghost root.
//
// Pure and deterministic: components, card order within a component, and the
// returned standalone slice are all sorted, so the same inputs always render
// the same board.
func buildMapsWorkflows(cardsByID map[string]mapsCardView, edges []mapsChainEdge) (workflows []mapsWorkflowView, standalone []string) {
	// Keep only edges touching at least one known gardener card.
	known := func(id string) bool { _, ok := cardsByID[id]; return ok }

	var liveEdges []mapsChainEdge
	chained := make(map[string]struct{})
	for _, e := range edges {
		if !known(e.Pred) && !known(e.Heir) {
			continue
		}
		liveEdges = append(liveEdges, e)
		chained[e.Pred] = struct{}{}
		chained[e.Heir] = struct{}{}
	}

	if len(liveEdges) == 0 {
		// No chains at all → every card is standalone.
		standalone = sortedKeys(cardsByID)
		return nil, standalone
	}

	// Union-Find over every node that appears in a live edge (known or ghost).
	parent := make(map[string]string)
	find := func(x string) string {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	ensure := func(x string) {
		if _, ok := parent[x]; !ok {
			parent[x] = x
		}
	}
	union := func(a, b string) {
		ensure(a)
		ensure(b)
		ra, rb := find(a), find(b)
		if ra != rb {
			// Deterministic merge: smaller id becomes the root.
			if ra < rb {
				parent[rb] = ra
			} else {
				parent[ra] = rb
			}
		}
	}
	for _, e := range liveEdges {
		union(e.Pred, e.Heir)
	}

	// Group nodes + edges by component root.
	compNodes := make(map[string]map[string]struct{})
	compEdges := make(map[string][]mapsChainEdge)
	for _, e := range liveEdges {
		root := find(e.Pred)
		if compNodes[root] == nil {
			compNodes[root] = make(map[string]struct{})
		}
		compNodes[root][e.Pred] = struct{}{}
		compNodes[root][e.Heir] = struct{}{}
		compEdges[root] = append(compEdges[root], e)
	}

	roots := make([]string, 0, len(compNodes))
	for r := range compNodes {
		roots = append(roots, r)
	}
	sort.Strings(roots)

	for _, r := range roots {
		wf := buildOneWorkflow(cardsByID, compNodes[r], compEdges[r])
		if wf.GardenerCount == 0 {
			// A component made entirely of ghosts (both endpoints off-board);
			// nothing to render. Defensive — filtered above, but cheap to guard.
			continue
		}
		workflows = append(workflows, wf)
	}

	// Standalone = known cards that never appeared in a live edge.
	for id := range cardsByID {
		if _, ok := chained[id]; !ok {
			standalone = append(standalone, id)
		}
	}
	sort.Strings(standalone)
	return workflows, standalone
}

// buildOneWorkflow assembles a single workflow view from its node set + edges.
// Card order is a deterministic topological sort (predecessor before heir);
// cycles (which C1's cycle guard refuses at authoring time, but which we never
// trust the data to be free of) degrade to id order for the cyclic remainder so
// the rail still renders.
func buildOneWorkflow(cardsByID map[string]mapsCardView, nodes map[string]struct{}, edges []mapsChainEdge) mapsWorkflowView {
	// indegree + adjacency for topo sort over the component.
	indeg := make(map[string]int)
	adj := make(map[string][]string)
	for n := range nodes {
		indeg[n] = 0
	}
	// Dedup edges (component may carry the same edge twice if both endpoints
	// declared it).
	seenEdge := make(map[mapsChainEdge]struct{})
	var uniqEdges []mapsChainEdge
	for _, e := range edges {
		if _, ok := seenEdge[e]; ok {
			continue
		}
		seenEdge[e] = struct{}{}
		uniqEdges = append(uniqEdges, e)
		adj[e.Pred] = append(adj[e.Pred], e.Heir)
		indeg[e.Heir]++
	}

	order := topoOrder(nodes, indeg, adj)

	// Planned-heir lookup (task ka-0000005273 / "C3"): a heir id targeted by a
	// Planned edge is a drawn-but-not-yet-spawned successor. It renders as a
	// PENDING node — distinct from a broken dangling ghost — carrying the
	// authored phase/goal caption.
	type plannedInfo struct {
		label  string
		fanOut bool
	}
	planned := make(map[string]plannedInfo)
	for _, e := range uniqEdges {
		if e.Planned {
			planned[e.Heir] = plannedInfo{label: e.PlannedLabel, fanOut: e.PlannedFanOut}
		}
	}

	// Per-card incoming-handoff state. A card's halo ignites when SOME incoming
	// edge is lit (its predecessor reached goal).
	achieved := func(id string) bool {
		c, ok := cardsByID[id]
		return ok && c.GoalState == "achieved"
	}
	incomingLit := make(map[string]bool)
	for _, e := range uniqEdges {
		if achieved(e.Pred) {
			incomingLit[e.Heir] = true
		}
	}
	hasIncoming := make(map[string]bool)
	for _, e := range uniqEdges {
		hasIncoming[e.Heir] = true
	}

	wf := mapsWorkflowView{}
	idx := 0
	for _, id := range order {
		card, ok := cardsByID[id]
		if !ok {
			idx++
			if pi, isPlanned := planned[id]; isPlanned {
				// Planned heir — a successor drawn via chain.to that the engine has
				// not spawned yet. Render a PENDING node (not a broken ghost).
				label := pi.label
				if label == "" {
					label = "successor"
				}
				wf.Cards = append(wf.Cards, mapsWorkflowCard{
					mapsCardView:  mapsCardView{ID: id, Name: label},
					PendingHeir:   true,
					PendingLabel:  label,
					PendingFanOut: pi.fanOut,
					IsRoot:        !hasIncoming[id],
					PhaseIndex:    idx,
				})
				wf.Pending = true
				continue
			}
			// Dangling heir/predecessor — render a ghost node so the broken
			// link is visible.
			wf.Cards = append(wf.Cards, mapsWorkflowCard{
				mapsCardView: mapsCardView{ID: id, Name: id},
				DanglingHeir: true,
				IsRoot:       !hasIncoming[id],
				PhaseIndex:   idx,
			})
			wf.Dangling = true
			continue
		}
		idx++
		wf.GardenerCount++
		wf.Cards = append(wf.Cards, mapsWorkflowCard{
			mapsCardView: card,
			HaloIgnited:  incomingLit[id],
			IsRoot:       !hasIncoming[id],
			PhaseIndex:   idx,
		})
	}
	total := len(wf.Cards)
	for i := range wf.Cards {
		wf.Cards[i].PhaseTotal = total
	}

	// Arrows in topo (Pred-position) order.
	for _, e := range uniqEdges {
		wf.Arrows = append(wf.Arrows, mapsChainArrow{
			Pred: e.Pred,
			Heir: e.Heir,
			Lit:  achieved(e.Pred),
		})
	}
	sort.SliceStable(wf.Arrows, func(i, j int) bool {
		if wf.Arrows[i].Pred != wf.Arrows[j].Pred {
			return wf.Arrows[i].Pred < wf.Arrows[j].Pred
		}
		return wf.Arrows[i].Heir < wf.Arrows[j].Heir
	})

	// Identity: id = smallest member; name = root gardener's name + " workflow".
	wf.ID = smallestID(nodes)
	wf.Name = workflowName(wf)

	// Phase = 1-based index of the first card whose predecessor has NOT handed
	// off to it yet — i.e. the first card that is not ignited (after the root).
	// The root is "phase 1" until it hands off. Once every edge is lit the whole
	// chain is done and Phase == card count.
	wf.Phase = workflowPhase(wf)

	return wf
}

// topoOrder returns a deterministic topological ordering of the component
// (predecessor before heir). Ties broken by id. A cycle leaves its members
// unvisited by Kahn's algorithm; they are appended in id order so the rail
// still renders every node.
func topoOrder(nodes map[string]struct{}, indeg map[string]int, adj map[string][]string) []string {
	// Stable Kahn: maintain a sorted "ready" set.
	deg := make(map[string]int, len(indeg))
	maps.Copy(deg, indeg)
	var ready []string
	for n := range nodes {
		if deg[n] == 0 {
			ready = append(ready, n)
		}
	}
	sort.Strings(ready)
	var order []string
	visited := make(map[string]struct{})
	for len(ready) > 0 {
		n := ready[0]
		ready = ready[1:]
		if _, seen := visited[n]; seen {
			continue
		}
		visited[n] = struct{}{}
		order = append(order, n)
		heirs := append([]string(nil), adj[n]...)
		sort.Strings(heirs)
		for _, h := range heirs {
			deg[h]--
			if deg[h] == 0 {
				ready = append(ready, h)
				sort.Strings(ready)
			}
		}
	}
	// Append any cyclic remainder in id order.
	if len(order) < len(nodes) {
		var rest []string
		for n := range nodes {
			if _, seen := visited[n]; !seen {
				rest = append(rest, n)
			}
		}
		sort.Strings(rest)
		order = append(order, rest...)
	}
	return order
}

// workflowName seeds the emergent workflow label from its root card's name.
func workflowName(wf mapsWorkflowView) string {
	for _, c := range wf.Cards {
		if c.IsRoot && !c.DanglingHeir && c.Name != "" {
			return c.Name + " workflow"
		}
	}
	// Fallback: first real card, else the id.
	for _, c := range wf.Cards {
		if !c.DanglingHeir && c.Name != "" {
			return c.Name + " workflow"
		}
	}
	if wf.ID != "" {
		return wf.ID + " workflow"
	}
	return "workflow"
}

// workflowPhase computes the 1-based current phase: the position of the first
// card that has not yet been handed off to (not ignited) past the root, i.e.
// the live frontier of the baton. Once every non-root card is ignited the whole
// chain has completed and Phase == card count.
func workflowPhase(wf mapsWorkflowView) int {
	if len(wf.Cards) == 0 {
		return 0
	}
	for i, c := range wf.Cards {
		if c.IsRoot {
			continue
		}
		if !c.HaloIgnited {
			return i // 0-based index of first un-handed-off heir == 1-based phase of its predecessor
		}
	}
	return len(wf.Cards)
}

// smallestID returns the lexicographically-smallest id in the node set.
func smallestID(nodes map[string]struct{}) string {
	first := true
	var min string
	for n := range nodes {
		if first || n < min {
			min = n
			first = false
		}
	}
	return min
}

// sortedKeys returns the map keys sorted ascending.
func sortedKeys(m map[string]mapsCardView) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
