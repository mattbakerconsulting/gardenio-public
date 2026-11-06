package main

// Gardener "Connected beads" constellation (Slack thread 2026-06-04, task
// ka-0000007657).
//
// Matt Baker: "connected beads in the gardener view should totally be a
// constellation". The gardener detail page already walks the transitive
// dependency graph reachable from a gardener's invocations (gardener_beads.go)
// and renders it as type-grouped lists. This file turns that same graph into a
// constellation SVG — the gardener bead as a golden star at center, its
// invocations and connected beads fanned out on concentric rings by
// hop-degree.
//
// It reuses the existing constellationView / constellationNode / constellationEdge
// shapes (constellation.go) so the shared `constellation_node` template and the
// status-palette / glyph / MR-diamond helpers all apply unchanged. The layout
// differs from the bead-show constellation (which splits parents-up /
// children-down / similar-right): here every connected bead is laid out
// radially by its hop-distance from the gardener, because "degree from the
// gardener" is the salient axis for this view.

import (
	"math"
	"sort"

	"gardenio/controller/internal/beadsapi"
)

const (
	// gardenerConstellationWidth/Height define the SVG viewBox. Square-ish so
	// concentric rings render without distortion; scales to the container via
	// preserveAspectRatio.
	gardenerConstellationWidth  = 1000
	gardenerConstellationHeight = 1000
	// gardenerConstellationCenterRadius is the golden gardener disk radius.
	gardenerConstellationCenterRadius = 34.0
	// gardenerConstellationNodeRadius is the orbit-node disk radius. Matches the
	// bead-show constellation's orbit nodes for visual continuity.
	gardenerConstellationNodeRadius = 15.0
	// gardenerConstellationRingGap is the radial spacing between successive
	// degree rings. Ring N sits at centerRadius + N*ringGap.
	gardenerConstellationRingGap = 150.0
	// gardenerConstellationMaxNodes is the BASE drawing budget: how many connected
	// beads we draw in the SVG at the default depth (gardenerBeadsDefaultDegree)
	// and shallower. Beyond this the constellation gets unreadable and the grouped
	// lists below remain the complete record. We keep the lowest-degree (closest
	// to the gardener) nodes when over the budget. The default /maps/{id} view sits
	// at exactly this budget so its density stays beauty-tuned; deepening the
	// depth control past the default raises the budget (mapDrawBudget) so the
	// slider stops being inert on hub gardeners (ka-0000003533).
	gardenerConstellationMaxNodes = 120
	// gardenerConstellationDepthBudgetStep is how many extra draw slots each depth
	// step BEYOND the default unlocks. On a hub gardener whose degree-1 ring alone
	// fills the base budget, the depth control was provably inert: degree 2-5 all
	// redrew the identical 120 closest beads (degree3_grows == 0). Growing the
	// budget with depth lets a deliberate deepening genuinely surface the deeper
	// frontier the truncation banner says exists, while the default view is
	// untouched.
	gardenerConstellationDepthBudgetStep = 60
	// gardenerConstellationMaxNodesCeiling hard-caps the depth-scaled budget so a
	// pathological hub gardener at max depth cannot blow the SVG into an unreadable
	// wall of nodes. The radial degrades to the grouped lists past this; the BFS
	// walk cap (gardenerBeadsMaxNodes) is a separate, larger backstop.
	gardenerConstellationMaxNodesCeiling = 360
	// gardenerRingInnerSpread is the de-hairball boost (in user-units) added to
	// the inner rings, decaying as 1/degree (ka-0000004711): degree-1 gains the
	// full boost, degree-2 half, degree-3 a third, etc. Kept strictly below
	// gardenerConstellationRingGap so successive rings never reorder, and small
	// enough that the boosted degree-1 ring stays well inside the 1000×1000
	// viewBox.
	gardenerRingInnerSpread = 90.0
	// gardenerConstellationDenseThreshold is the rendered-node count at which the
	// radial starts reading as a hairball (ka-0000000447): at the 120-node cap a
	// busy gardener carries hundreds of edges, the bulk of them low-provenance
	// untyped mentions, and the outer rings blur. Below this count every edge
	// renders at its base opacity; at/above it the lowest-provenance edges fade
	// toward gardenerConstellationDenseMentionFloor so the structural dep / typed
	// edges stay legible against the dark field.
	gardenerConstellationDenseThreshold = 80
	// gardenerConstellationDenseMentionFloor is the multiplier applied to an
	// untyped-mention edge's base opacity once the radial is fully saturated (at
	// the node cap). It is a floor, not a cutoff — the faintest mentions stay
	// faintly visible so the graph never loses edges silently, it just demotes
	// the noisiest provenance class so hop-distance reads at the outer rings.
	gardenerConstellationDenseMentionFloor = 0.45
)

// mapDrawBudget returns the SVG node-draw budget for a given selected depth. At
// or below the default depth (gardenerBeadsDefaultDegree) it is the base budget
// (gardenerConstellationMaxNodes), so the default /maps/{id} view keeps its
// beauty-tuned density. Each depth step beyond the default unlocks
// gardenerConstellationDepthBudgetStep extra slots, clamped to
// gardenerConstellationMaxNodesCeiling.
//
// This is the fix for the inert depth control (ka-0000003533): on a hub gardener
// whose degree-1 ring alone fills the base budget, every degree drew the same
// 120 closest beads (degree3_grows == 0). A depth-scaled budget lets a deliberate
// deepening spend real draw slots on the deeper frontier the truncation banner
// already discloses, so the slider does something instead of being a silent
// dead control.
func mapDrawBudget(degree int) int {
	budget := gardenerConstellationMaxNodes
	if degree > gardenerBeadsDefaultDegree {
		budget += (degree - gardenerBeadsDefaultDegree) * gardenerConstellationDepthBudgetStep
	}
	if budget > gardenerConstellationMaxNodesCeiling {
		budget = gardenerConstellationMaxNodesCeiling
	}
	if budget < gardenerConstellationMaxNodes {
		budget = gardenerConstellationMaxNodes
	}
	return budget
}

// buildGardenerConstellation assembles a radial-tree constellationView from the
// gardener's transitive graph. Returns nil when there is nothing to draw (no
// connected beads), in which case the template falls back to the grouped lists.
//
//   - gardenerID is the subject; it becomes the golden center node.
//   - nodes/edges/beads come straight from walkGardenerBeadGraph.
//   - seedSet is the gardener's invocation ids (degree-0 agent beads) so they
//     can be styled distinctly from deeper connected beads.
//   - degree is the active walk depth (drives ring count + the figure caption).
func (s *Server) buildGardenerConstellation(
	gardenerID string,
	nodes []gardenerBeadNodeView,
	edges []gardenerGraphEdge,
	beads map[string]*beadsapi.BeadDetail,
	seedSet map[string]struct{},
	degree int,
) *constellationView {
	// Index nodes by id and bucket by degree (excluding the gardener itself,
	// which is the center, not an orbit node).
	byID := make(map[string]gardenerBeadNodeView, len(nodes))
	byDegree := make(map[int][]gardenerBeadNodeView)
	maxDegree := 0
	connected := 0
	for _, n := range nodes {
		byID[n.ID] = n
		if n.ID == gardenerID {
			continue
		}
		byDegree[n.Degree] = append(byDegree[n.Degree], n)
		if n.Degree > maxDegree {
			maxDegree = n.Degree
		}
		connected++
	}
	if connected == 0 {
		return nil // nothing to draw — fall back to the lists
	}

	// Depth-scaled draw budget (ka-0000003533): the default view stays at the
	// base budget; deepening the depth control unlocks more slots so the slider
	// reaches the deeper frontier instead of redrawing the identical inner rings.
	budget := mapDrawBudget(degree)

	cx := float64(gardenerConstellationWidth) / 2
	cy := float64(gardenerConstellationHeight) / 2

	view := &constellationView{
		Width:        gardenerConstellationWidth,
		Height:       gardenerConstellationHeight,
		BasePath:     s.basePath,
		TreeLevel:    degree,
		MaxTreeLevel: gardenerBeadsMaxDegree,
		Legend:       constellationLegendItems(),
	}

	// Center: the gardener bead, rendered as the golden star. Reuse whatever the
	// walk hydrated for it (type/status) but force the golden center styling via
	// IsCenter so the template paints the radial gradient.
	capNode := byID[gardenerID]
	capType := capNode.Type
	if capType == "" || capType == "unknown" {
		capType = "gardener"
	}
	view.Center = constellationNode{
		ID:       gardenerID,
		Title:    capNode.Title,
		X:        cx,
		Y:        cy,
		Radius:   gardenerConstellationCenterRadius,
		HaloR:    gardenerConstellationCenterRadius * 2.2,
		GlyphY:   cy + 6,
		IDLabelY: cy + gardenerConstellationCenterRadius + 22,
		TopY:     cy - gardenerConstellationCenterRadius - 10,
		Status:   capNode.Status,
		Type:     capType,
		Role:     "center",
		IsCenter: true,
		Glyph:    gardenerCenterGlyph(capType),
		Href:     s.basePath + "/gardener/" + gardenerID,
		Color:    statusFill(capNode.Status),
		Stroke:   statusStroke(capNode.Status),
		Opacity:  1.0,
	}

	// Assign an angle to every orbit node. We place each node on its degree
	// ring; to keep the tree readable we try to seat a node near the angle of
	// its discovery parent (the edge endpoint one degree closer to the center).
	// Where no parent angle is known we distribute evenly around the ring.
	parentOf := buildGardenerParentMap(gardenerID, byID, edges)
	angles := assignGardenerAngles(byDegree, parentOf, maxDegree)

	// Capacity guard: keep the lowest-degree nodes when over the SVG cap so the
	// most-relevant (closest to the gardener) beads always render. Degree 0 are
	// the gardener's invocations (its direct work) — they sit on an inner ring
	// just outside the center.
	rendered := make(map[string]struct{}, budget)
	placed := make(map[string]constellationNode, budget)
	count := 0
	// Track the deepest degree that actually wins a render slot. When the cap
	// fills on the inner rings before reaching the deeper walked rings, this
	// stays below maxDegree and the depth control is inert past it
	// (ka-0000002373). -1 means no orbit node placed yet.
	drawnMaxDegree := -1
	for d := 0; d <= maxDegree; d++ {
		ring := byDegree[d]
		// Order within a ring so that, when the SVG cap truncates mid-ring,
		// open/in_progress beads win render slots over closed/deferred ones
		// (ka-0000001741): active work is the point of the map, and dropping it
		// to draw a closed bead is the wrong trade. Status priority is the
		// primary key (statusOpenSortKey: 0 = open/in_progress/unknown,
		// 1 = closed/deferred); ID is the tiebreaker so layout stays
		// deterministic within a priority tier.
		sort.SliceStable(ring, func(i, j int) bool {
			ki, kj := statusOpenSortKey(ring[i].Status), statusOpenSortKey(ring[j].Status)
			if ki != kj {
				return ki < kj
			}
			return ring[i].ID < ring[j].ID
		})
		radius := gardenerRingRadius(d)
		for _, bn := range ring {
			if count >= budget {
				break
			}
			ang := angles[bn.ID]
			x := cx + radius*math.Cos(ang)
			y := cy + radius*math.Sin(ang)
			node := gardenerOrbitNode(bn, beads[bn.ID], x, y, s.basePath, seedSet)
			placed[bn.ID] = node
			rendered[bn.ID] = struct{}{}
			count++
			if d > drawnMaxDegree {
				drawnMaxDegree = d
			}
		}
	}
	// Depth-saturation signal (ka-0000002373): when the render budget filled
	// before the deepest walked ring got any slots, the degree pills past
	// drawnMaxDegree draw nothing new — the depth control is inert. Surface that
	// honestly via the view so the template can banner it rather than letting
	// the slider be a silent dead control on dense gardeners.
	view.WalkMaxDegree = maxDegree
	if drawnMaxDegree >= 0 {
		view.DrawnMaxDegree = drawnMaxDegree
	}
	view.DepthSaturated = count >= budget && drawnMaxDegree < maxDegree

	// Group placed nodes by ring index for the template. We reuse the
	// constellationView.Outer slice as the generic "all orbit nodes" bucket;
	// Forward/Reverse/Similar stay empty (this layout has a single radial axis).
	outer := make([]constellationNode, 0, len(placed))
	for _, n := range placed {
		outer = append(outer, n)
	}
	sort.SliceStable(outer, func(i, j int) bool { return outer[i].ID < outer[j].ID })
	view.Outer = outer

	// Edges: draw every discovered edge whose BOTH endpoints rendered. Color by
	// direction relative to the gardener (toward center = the bead this depends
	// on). We approximate "forward vs reverse" by hop-distance: an edge points
	// from the higher-degree (farther) node toward the lower-degree (closer)
	// node, mirroring the parent-child grain of the walk.
	for _, e := range edges {
		src, srcOK := endpointPoint(e.Source, gardenerID, view.Center, placed, rendered)
		tgt, tgtOK := endpointPoint(e.Target, gardenerID, view.Center, placed, rendered)
		if !srcOK || !tgtOK {
			continue
		}
		role := "forward"
		if degreeOf(e.Source, byID) < degreeOf(e.Target, byID) {
			role = "reverse"
		}
		// Style by the edge's TRUE provenance (ka-0000005664): dep-table rows,
		// typed mentions, and untyped mentions render distinctly so the legend
		// matches the wire colors instead of flattening everything to "dep". An
		// edge with no recorded origin (legacy / dep-only walk) falls back to
		// "dep", the historic default.
		origin := e.Origin
		if origin == "" {
			origin = "dep"
		}
		stroke, dash, opacity := edgeStyleFor(role, origin, e.RelType)
		// De-hairball at high density (ka-0000000447): a busy gardener pinned at
		// the node cap carries hundreds of edges, the bulk of them low-provenance
		// untyped mentions, and the outer rings blur into a hairball. Fade only
		// the noisiest provenance class (untyped mentions) as the rendered node
		// count climbs, so the structural dep / typed edges keep their full weight
		// and hop-distance stays legible. dep/typed/assignee/triggered edges are
		// untouched.
		opacity *= gardenerDenseEdgeFade(origin, count)
		view.Edges = append(view.Edges, constellationEdge{
			X1: src.X, Y1: src.Y, X2: tgt.X, Y2: tgt.Y,
			Role:             role,
			Origin:           origin,
			RelationshipType: e.RelType,
			SourceBead:       e.Source,
			TargetBead:       e.Target,
			TitleText:        edgeTitle(role, origin, e.RelType),
			Stroke:           stroke,
			Dash:             dash,
			Opacity:          opacity,
		})
	}

	// Faint backdrop rings, one per populated degree, so hop-distance reads.
	for d := 0; d <= maxDegree; d++ {
		if len(byDegree[d]) == 0 {
			continue
		}
		view.RingRadii = append(view.RingRadii, gardenerRingRadius(d))
	}

	view.HasOuter = len(view.Outer) > 0
	view.HasAnything = view.HasOuter
	return view
}

// gardenerRingRadius returns the orbit radius for a given hop-degree. Degree 0
// (the gardener's invocations) sits on a tighter inner ring at half the gap so
// the gardener's own runs cluster close to the center; deeper degrees step out
// by a full ring gap each.
//
// De-hairball (ka-0000004711): the inner rings are where the ka-id labels
// overlap worst — a small ring at r≈184 has only ~1150px of circumference to
// seat what is often the bulk of the connected beads, so the closest (most
// important) labels collide into an unreadable smear. We push the inner rings
// outward by a DECAYING boost (gardenerRingInnerSpread/degree) so the crowded
// degree-1/2 rings gain real circumference (and angular spacing between nodes)
// while the deeper rings — which already approach the viewBox edge and would
// clip if shifted further — stay essentially where they were. The boost is
// strictly < ringGap so rings never reorder.
func gardenerRingRadius(degree int) float64 {
	if degree <= 0 {
		return gardenerConstellationCenterRadius + gardenerConstellationRingGap*0.5
	}
	boost := gardenerRingInnerSpread / float64(degree)
	return gardenerConstellationCenterRadius + float64(degree)*gardenerConstellationRingGap + boost
}

// gardenerDenseEdgeFade returns an opacity multiplier (in [floor, 1.0]) for an
// edge given the constellation's provenance and how many nodes were rendered.
// It de-hairballs dense gardeners (ka-0000000447) by fading ONLY untyped-mention
// edges — the noisiest, lowest-signal provenance class — as the rendered node
// count climbs from gardenerConstellationDenseThreshold up to the node cap.
//
//   - Non-mention origins (dep, typed text, assignee, triggered) always return
//     1.0: structural edges keep their full weight.
//   - Below the dense threshold every edge returns 1.0 (sparse maps stay as-is).
//   - From the threshold to the cap, untyped mentions fade linearly from 1.0 down
//     to gardenerConstellationDenseMentionFloor. It is a FLOOR, not a cutoff —
//     the faintest mentions remain faintly drawn so edges never vanish silently.
func gardenerDenseEdgeFade(origin string, renderedCount int) float64 {
	if origin != "mention" {
		return 1.0
	}
	if renderedCount <= gardenerConstellationDenseThreshold {
		return 1.0
	}
	span := float64(gardenerConstellationMaxNodes - gardenerConstellationDenseThreshold)
	if span <= 0 {
		return gardenerConstellationDenseMentionFloor
	}
	// t in (0,1]: 0 at the threshold, 1 at (or past) the node cap.
	t := float64(renderedCount-gardenerConstellationDenseThreshold) / span
	if t > 1 {
		t = 1
	}
	return 1.0 - t*(1.0-gardenerConstellationDenseMentionFloor)
}

// gardenerCenterGlyph picks the glyph for the gardener center disk. A gardener is
// driven by a type=trigger bead (role=gardener), an agent bead, or a synthetic
// "gardener" type when the walk could not hydrate it — all render as a star so
// the center reads as the gardener regardless of the underlying bead type.
func gardenerCenterGlyph(beadType string) string {
	switch beadType {
	case "gardener", "agent", "trigger", "schedule", "", "unknown":
		return "★"
	}
	return typeGlyph(beadType)
}

// gardenerOrbitNode builds one orbit constellationNode for a connected bead,
// reusing the shared status palette / glyph / MR-diamond styling. Invocation
// (seed) agent beads get the golden agent styling so the gardener's own runs
// read distinctly from the wider connected graph.
func gardenerOrbitNode(
	bn gardenerBeadNodeView,
	bd *beadsapi.BeadDetail,
	x, y float64,
	basePath string,
	seedSet map[string]struct{},
) constellationNode {
	rad := gardenerConstellationNodeRadius
	n := constellationNode{
		ID:       bn.ID,
		Title:    bn.Title,
		X:        x,
		Y:        y,
		Radius:   rad,
		GlyphY:   y + 4,
		IDLabelY: y + rad + 14,
		Status:   bn.Status,
		Type:     bn.Type,
		Role:     "outer",
		Degree:   bn.Degree,
		Href:     basePath + "/" + bn.ID,
		Color:    statusFill(bn.Status),
		Stroke:   statusStroke(bn.Status),
		Glyph:    typeGlyph(bn.Type),
		Opacity:  1.0,
	}
	if bn.Type == "" || bn.Type == "unknown" {
		n.Glyph = "?"
		if bd == nil {
			n.Color = "#cfcfcf"
			n.Stroke = "#888"
		}
	}
	if _, isSeed := seedSet[bn.ID]; isSeed {
		// Gardener invocation — golden disk echoing the center, like the
		// bead-show assignee node.
		n.Role = "invocation"
		n.Color = "#f1c84a"
		n.Stroke = "#b9851b"
		n.Glyph = typeGlyph("agent")
	}
	if bd != nil {
		applyMRState(&n, bd.Type, bd.Fields)
	}
	return n
}

// buildGardenerParentMap maps each non-center node to its "discovery parent" —
// the adjacent node exactly one hop closer to the gardener. Used to seat a node
// near its parent's angle so the radial tree reads as a tree, not a scatter.
func buildGardenerParentMap(
	gardenerID string,
	byID map[string]gardenerBeadNodeView,
	edges []gardenerGraphEdge,
) map[string]string {
	parent := make(map[string]string)
	consider := func(child, par string) {
		if child == "" || par == "" || child == gardenerID {
			return
		}
		cd, cok := byID[child]
		pd, pok := byID[par]
		if !cok || !pok {
			return
		}
		// Parent must be exactly one ring closer to the gardener.
		if pd.Degree != cd.Degree-1 {
			return
		}
		if _, set := parent[child]; set {
			return
		}
		parent[child] = par
	}
	for _, e := range edges {
		// Either endpoint may be the parent depending on edge direction; the
		// degree check inside consider keeps only the closer-to-gardener one.
		consider(e.Source, e.Target)
		consider(e.Target, e.Source)
	}
	return parent
}

// assignGardenerAngles computes a stable angle (radians) for every non-center
// node. Each ring gets an EVEN spread around the full circle so nodes never
// crowd into an overlapping wedge — the "star map" look Matt asked for. To keep
// the tree legible (parent and child roughly aligned, fewer edge crossings),
// nodes within a ring are ORDERED by their discovery parent's angle before the
// even spacing is applied; nodes with no known parent sort last by ID.
func assignGardenerAngles(
	byDegree map[int][]gardenerBeadNodeView,
	parentOf map[string]string,
	maxDegree int,
) map[string]float64 {
	angles := make(map[string]float64)

	for d := 0; d <= maxDegree; d++ {
		ring := append([]gardenerBeadNodeView(nil), byDegree[d]...)
		if len(ring) == 0 {
			continue
		}
		// Order the ring by parent angle (so a child lands near its parent),
		// tie-broken by ID for determinism. The innermost rings (0/1) usually
		// have no parent angle yet, so they fall back to pure ID order.
		sort.SliceStable(ring, func(i, j int) bool {
			ai, aok := angles[parentOf[ring[i].ID]]
			aj, bok := angles[parentOf[ring[j].ID]]
			switch {
			case aok && bok && ai != aj:
				return ai < aj
			case aok != bok:
				return aok // parented nodes first
			default:
				return ring[i].ID < ring[j].ID
			}
		})
		n := len(ring)
		for i, bn := range ring {
			angles[bn.ID] = -math.Pi/2 + 2*math.Pi*float64(i)/float64(n)
		}
	}
	return angles
}

// endpointPoint resolves an edge endpoint id to its rendered (X,Y). The gardener
// id resolves to the center; any other id must be in the placed map. Returns
// ok=false when the endpoint was not rendered (over the cap / not in graph).
func endpointPoint(
	id, gardenerID string,
	center constellationNode,
	placed map[string]constellationNode,
	rendered map[string]struct{},
) (constellationNode, bool) {
	if id == gardenerID {
		return center, true
	}
	if _, ok := rendered[id]; !ok {
		return constellationNode{}, false
	}
	n, ok := placed[id]
	return n, ok
}

// degreeOf returns the hop-distance of a node id (0 for the gardener / unknown).
func degreeOf(id string, byID map[string]gardenerBeadNodeView) int {
	if n, ok := byID[id]; ok {
		return n.Degree
	}
	return 0
}
