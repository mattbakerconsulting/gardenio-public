package main

import (
	"context"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// Constellation cap: how many similar / forward-deps / reverse-deps to draw.
// Beyond these the SVG gets crowded and the daemon gets hammered with hydrate
// calls. The bead description still lists everything below the SVG.
const (
	constellationMaxSimilar  = 8
	constellationMaxForward  = 6
	constellationMaxReverse  = 6
	constellationHydrateTime = 600 * time.Millisecond
	// constellationMaxTreeLevel is the upper bound on the tree-level slider.
	// Each extra level pulls another layer of forward/reverse deps and makes
	// the SVG larger; 3 is enough to cover "children of children" without
	// blowing up daemon load or the on-page SVG size.
	constellationMaxTreeLevel = 3
	// constellationFanoutPerParent caps how many transitive deps we draw per
	// L1 parent at each deeper level. Keeps the level-2/3 fanout bounded
	// even when an upstream bead has dozens of children.
	constellationFanoutPerParent = 3
	// constellationHydrateMultiplier sets how many candidate dep targets we
	// hydrate per visible slot. Hidden orbit beads — session beads + closed
	// agents — are filtered out post-hydration (ka-0000005724, ka-0000000532);
	// over-hydrating lets a hidden reverse-dep make room for a more interesting
	// visible sibling instead of leaving an empty slot. Hydration stays bounded —
	// one batch round-trip with timeout=600ms; missing beads degrade silently to
	// "?" nodes.
	constellationHydrateMultiplier = 3
)

// ageOpacityFloor is the minimum disk fill-opacity applied to the oldest node
// in the constellation. Newest = 1.0; oldest = floor; intermediates interpolate
// linearly across log-age. 0.30 keeps old nodes legible while still pushing the
// eye toward fresh activity.
const ageOpacityFloor = 0.30

// constellationView is the template view model for the bead-show SVG.
//
// Coordinates are in a 1000×500 viewBox so the rendered <svg> can scale to any
// container width via preserveAspectRatio. The center bead sits at (500, 250);
// similar beads orbit on the right semicircle; forward dependencies (this bead
// depends on) hang above the center; reverse dependencies (depend on this) hang
// below.
type constellationView struct {
	Center   constellationNode
	Similar  []constellationNode
	Forward  []constellationNode // this bead depends on these (parents)
	Reverse  []constellationNode // these beads depend on this (children)
	Outer    []constellationNode // transitive deps at level 2..TreeLevel
	Assignee constellationNode   // synthetic agent node when bead.Assignee != ""
	// Predecessor is the DISPLAY-ONLY succession node (task ka-0000007852):
	// when the center bead is an agent carrying a prior_agent_id ∨
	// schedule_prior_agent_id back-pointer, its immediate predecessor renders
	// as a distinct-glyph node tied by a synthesized succession edge. This is
	// NOT a deps-table edge — it never enters the forward/reverse walk, the
	// blocked-by graph, or the reaper — so a live head never reads as blocked
	// on its closed predecessor. Zero-value (HasPredecessor false) renders
	// nothing.
	Predecessor    constellationNode
	HasPredecessor bool
	Edges          []constellationEdge
	Legend         []constellationLegend
	Width          int
	Height         int
	BasePath       string
	// TreeLevel is the active tree-depth slider value (1..constellationMaxTreeLevel).
	// 1 = just direct deps; 2 = one extra ring of grandparents/grandchildren; etc.
	TreeLevel    int
	MaxTreeLevel int
	HasSimilar   bool
	HasForward   bool
	HasReverse   bool
	HasOuter     bool
	HasAssignee  bool
	HasAnything  bool
	// RingRadii holds the radii of the faint concentric degree rings drawn
	// behind the orbit nodes. Only populated by the gardener constellation
	// (buildGardenerConstellation), where the radial axis is hop-distance; the
	// bead-show / inbox builders leave it nil and their templates never read it.
	RingRadii []float64
	// DrawnMaxDegree / WalkMaxDegree expose the depth-saturation state of the
	// gardener constellation (ka-0000002373). DrawnMaxDegree is the deepest
	// hop-degree that actually won a render slot; WalkMaxDegree is the deepest
	// degree present in the walked graph. They differ when the 120-node render
	// budget fills on the inner rings before the deeper walked rings get any
	// slots. Only set by buildGardenerConstellation; other builders leave them 0.
	DrawnMaxDegree int
	WalkMaxDegree  int
	// DepthSaturated is true when the render budget filled before reaching the
	// deepest walked ring (DrawnMaxDegree < WalkMaxDegree after the cap was
	// hit). In that state the depth/degree control is INERT past DrawnMaxDegree:
	// deeper connected beads exist in the walk but never draw, so bumping the
	// degree pill changes nothing. The template surfaces this as an explicit
	// banner instead of letting the slider be a silent dead control.
	DepthSaturated bool
	// ShowClosedAgents reflects the user's "show closed agents" toggle
	// (ka-0000005724). When false (the default), closed agent beads are filtered
	// out of the orbit nodes; the toggle drives the figcaption checkbox state and
	// the URL param round-trip in JS. Session beads are hidden unconditionally
	// (ka-0000000532) and this toggle does NOT bring them back.
	ShowClosedAgents bool
}

type constellationNode struct {
	ID       string
	Title    string // unlinkified plain text — SVG can't host HTML
	X        float64
	Y        float64
	Radius   float64
	HaloR    float64 // halo radius (only used by center)
	GlyphY   float64 // baseline y so glyph reads vertically centered
	IDLabelY float64 // y for the small ID label below the disk
	TopY     float64 // y above the disk (used by center for the small ID line)
	Status   string  // open, in_progress, closed, deferred, ""
	Type     string  // task, epic, formula, decision, agent, advice, ...
	Score    string  // similarity score, e.g. "0.812" — empty for deps
	Role     string  // "center" | "similar" | "forward" | "reverse"
	Color    string  // CSS fill from status palette
	Stroke   string  // CSS stroke color
	IsCenter bool
	Glyph    string // single-char type marker rendered inside the disk
	Href     string // /{id}
	Priority int
	// Opacity in [0,1] used to fade older nodes toward transparency. Newest
	// node in the constellation = 1.0; oldest = ageOpacityFloor (default 0.30).
	// Applied as fill-opacity on the orbit disks. The center node stays at 1.0
	// (it is the focus); edges keep their existing constellationEdge.Opacity.
	Opacity float64
	// IsMR is true when Type == "merge-request" — flips the node from a
	// circle to a diamond and enables the ↗ corner overlay (ka-0000004604).
	IsMR bool
	// MRState is the merge_request state (`opened`, `merged`, `closed`).
	// Empty when the bead is not an MR or the field is missing — the
	// template suppresses the overlay glyph in that case.
	MRState string
	// MROverlay is the overlay-badge fill tinted by MRState (purple for
	// merged, blue for opened, gray for closed-unmerged).
	MROverlay string
	// IsExternal is true when an MR bead is a read-only upstream-MR mirror —
	// a projection of a human-owned MR on the REAL monorepo (E-PROV FW-5,
	// ka-0000003146, field external_mirror=true). The template adds the
	// `constellation-node-external` class and a dashed diamond stroke so the
	// node reads as "external / not-ours" at a glance, distinct from dark MR
	// diamonds. Only ever set on IsMR nodes.
	IsExternal bool
	// Emphasis / Ghost are Maps Phase 4 insight-overlay flags
	// (maps_overlay.go). Emphasis = the active lens (heat / needs-attention)
	// pulls this node to the foreground with a heavier ring; Ghost = the
	// timeline lens marks this node as not-yet-born at the scrub cutoff (a
	// hollow future placeholder). Both default false so every other
	// constellation (bead-show / inbox / the plain gardener map) renders
	// exactly as before — the maps template is the only reader.
	Emphasis bool
	Ghost    bool
	// Degree is the hop-distance from the gardener on the radial maps layout
	// (buildGardenerConstellation): 0 = the gardener's own invocations, 1 =
	// direct deps, etc. The bead-show / inbox builders leave it at 0 (their
	// axis is parent/child/similar, not hop-distance) and never read it.
	Degree int
	// MutedLabel hides the small id caption below the disk by default,
	// revealing it on hover/focus. It was set only by the old maps page's
	// de-hairball pass (decorateMapConstellation), removed with the rest of
	// /maps in ka-0000000247; no current builder sets it, so the shared
	// `constellation_node` template — used by bead-show and the gardener page —
	// always renders the label. Retained as a template-facing hook for a future
	// re-introduction of label muting (e.g. maps2).
	MutedLabel bool
}

type constellationEdge struct {
	X1, Y1, X2, Y2 float64
	// Role describes the *direction* of the edge in the layout:
	//   "similar" | "forward" | "reverse"
	Role string
	// Origin describes the *provenance* of the edge:
	//   "dep"     — explicit deps-table row
	//   "text"    — typed mention (relationship_type set)
	//   "mention" — untyped mention (relationship_type NULL)
	//   ""        — similarity edge (no provenance distinction)
	Origin string
	// RelationshipType is the typed-mention verb (e.g. "blocks",
	// "parent-child") for Origin == "text" / "dep". Empty for untyped
	// mentions and similarity edges.
	RelationshipType string
	// SourceBead/TargetBead are the bead IDs at the two ends of the edge.
	// They flow into `data-source-bead` / `data-target-bead` SVG attrs so
	// the (future) edge-click handler in E2 can navigate.
	SourceBead string
	TargetBead string
	// TitleText is the SVG <title> tooltip ("blocks (text)" etc.). Empty
	// renders no <title> element.
	TitleText string
	Stroke    string
	Dash      string // empty for solid, "4 3" etc. for dashed
	Opacity   float64
}

// taggedDep is the internal "edge spec" the constellation uses when folding
// deps-table rows together with text-extracted mentions. Each tuple of
// (BeadID, DependsOnID) is represented at most once; Origin records whether
// it came from an explicit deps row, a typed mention in prose, or an untyped
// mention. The first one we see for a given target wins (deps > typed > untyped).
type taggedDep struct {
	BeadID      string
	DependsOnID string
	// Type is the dep / relationship-type verb. Empty for untyped mentions.
	Type string
	// Origin is "dep" | "text" | "mention".
	Origin string
}

type constellationLegend struct {
	Label string
	Color string
	// Shape: "node" (filled circle), "edge" (stroked line), or "mr"
	// (diamond glyph with optional state-tinted overlay — ka-0000004604).
	Shape   string
	Stroke  string
	Dash    string
	Overlay string // overlay-badge fill for Shape == "mr"
	OffsetY int
}

// buildConstellation hydrates dependency beads (best-effort, capped, parallel)
// and assembles the geometry for the SVG. Errors anywhere downgrade to "panel
// hidden" — the layout is decorative, never load-bearing.
//
// `mentions` and `mentionedBy` carry text-extracted references from the bead's
// description / notes / comments. Edges are emitted with three distinct
// `Origin` values (see constellationEdge):
//
//   - "dep"     — explicit deps-table row, solid line.
//   - "text"    — typed mention (relationship_type set), same direction
//     color as deps but with a subtle 6 2 dash to mark the
//     text-derived provenance.
//   - "mention" — untyped mention (NULL relationship_type), neutral gray
//     #7a7a7a, opacity 0.55, dashed 3 4.
//
// Per-target dedup: a deps-row beats a typed mention beats an untyped
// mention. Each target appears once in each direction.
//
// See `gardenio/docs/beads-viewer-constellation.md` for the rendered legend.
func (s *Server) buildConstellation(
	ctx context.Context,
	bead *beadsapi.BeadDetail,
	deps []beadsapi.Dependency, // forward: this depends on X
	revDeps []beadsapi.Dependency, // reverse: X depends on this
	similar *beadsapi.FindSimilarResponse,
	mentions []beadsapi.BeadReferenceSummary, // forward: this mentions X
	mentionedBy []beadsapi.BeadReferenceSummary, // reverse: X mentions this
	treeLevel int,
	showClosedAgents bool, // when false, drop closed agent/session nodes (ka-0000005724)
) *constellationView {
	if bead == nil {
		return nil
	}
	treeLevel = clampTreeLevel(treeLevel)
	forwardDeps := tagExistingDeps(deps)
	forwardDeps = mergeTypedMentionsAsDepEdges(forwardDeps, bead.ID, mentions, true /*forward*/)
	forwardDeps = appendUntypedMentionEdges(forwardDeps, bead.ID, mentions, true /*forward*/)
	reverseDeps := tagExistingDeps(revDeps)
	reverseDeps = mergeTypedMentionsAsDepEdges(reverseDeps, bead.ID, mentionedBy, false /*reverse*/)
	reverseDeps = appendUntypedMentionEdges(reverseDeps, bead.ID, mentionedBy, false /*reverse*/)

	width, height, cx, cy, levelRadii := constellationLayout(treeLevel)
	view := &constellationView{
		Width:            width,
		Height:           height,
		BasePath:         s.basePath,
		TreeLevel:        treeLevel,
		MaxTreeLevel:     constellationMaxTreeLevel,
		ShowClosedAgents: showClosedAgents,
	}
	_ = levelRadii // used below per-level

	// --- Center bead ---
	cr := 42.0
	view.Center = constellationNode{
		ID:       bead.ID,
		Title:    bead.Title,
		X:        cx,
		Y:        cy,
		Radius:   cr,
		HaloR:    cr * 2.2,
		GlyphY:   cy + 6,
		IDLabelY: cy + cr + 22, // for the title label below
		TopY:     cy - cr - 12, // for the small bead ID above
		Status:   bead.Status,
		Type:     bead.Type,
		Role:     "center",
		Color:    statusFill(bead.Status),
		Stroke:   "#d4a017", // golden ring
		IsCenter: true,
		Glyph:    typeGlyph(bead.Type),
		Href:     s.basePath + "/" + bead.ID,
		Priority: bead.Priority,
	}
	applyMRState(&view.Center, bead.Type, bead.Fields)

	// --- Similar beads on the right semicircle (radial cloud) ---
	var similarResults []*beadsapi.SemanticSearchResult
	if similar != nil {
		similarResults = similar.Results
		// Session beads are ALWAYS dropped (ka-0000000532), even when the user
		// opts into show_closed_agents — that toggle is scoped to closed AGENT
		// beads, not session bookkeeping.
		filtered := make([]*beadsapi.SemanticSearchResult, 0, len(similarResults))
		for _, r := range similarResults {
			if r == nil || r.Bead == nil {
				filtered = append(filtered, r)
				continue
			}
			if r.Bead.Type == "session" {
				continue
			}
			if !showClosedAgents && isHiddenOrbitBeadFields(r.Bead.Type, r.Bead.Status) {
				continue
			}
			filtered = append(filtered, r)
		}
		similarResults = filtered
		if len(similarResults) > constellationMaxSimilar {
			similarResults = similarResults[:constellationMaxSimilar]
		}
		results := similarResults
		n := len(results)
		for i, r := range results {
			if r == nil || r.Bead == nil {
				continue
			}
			// Higher score → closer to center. Score is in [0,1].
			score := r.Score
			if score < 0 {
				score = 0
			}
			if score > 1 {
				score = 1
			}
			rMin, rMax := 150.0, 380.0
			radius := rMax - (rMax-rMin)*score
			// Spread evenly across the right arc, -65° → +65° measured from x-axis.
			arcStart := -65.0
			arcEnd := 65.0
			frac := 0.5
			if n > 1 {
				frac = float64(i) / float64(n-1)
			}
			angle := (arcStart + (arcEnd-arcStart)*frac) * math.Pi / 180.0
			x := cx + radius*math.Cos(angle)
			y := cy + radius*math.Sin(angle)
			rad := 18.0
			node := constellationNode{
				ID:       r.Bead.ID,
				Title:    r.Bead.Title,
				X:        x,
				Y:        y,
				Radius:   rad,
				GlyphY:   y + 4,
				IDLabelY: y + rad + 14,
				Status:   r.Bead.Status,
				Type:     r.Bead.Type,
				Role:     "similar",
				Color:    statusFill(r.Bead.Status),
				Stroke:   statusStroke(r.Bead.Status),
				Glyph:    typeGlyph(r.Bead.Type),
				Score:    fmt.Sprintf("%.2f", r.Score),
				Href:     s.basePath + "/" + r.Bead.ID,
				Priority: r.Bead.Priority,
			}
			applyMRState(&node, r.Bead.Type, r.Bead.Fields)
			view.Similar = append(view.Similar, node)
			view.Edges = append(view.Edges, constellationEdge{
				X1: view.Center.X, Y1: view.Center.Y, X2: x, Y2: y,
				Role:    "similar",
				Stroke:  "#9b8650",
				Dash:    "3 4",
				Opacity: 0.45 + 0.45*score,
			})
		}
	}

	// --- Hydrate dep targets in parallel for richer node rendering. ---
	// We over-fetch by constellationHydrateMultiplier so that hidden orbit
	// targets (session beads + closed agents) can be filtered out post-hydration
	// without leaving empty visible slots (ka-0000005724). Session beads are
	// always dropped (ka-0000000532); closed agents are dropped unless the user
	// opted into ?show_closed_agents=1, in which case the hydration cap collapses
	// back to the display cap (only the always-on session filter still runs).
	forwardCap, reverseCap := constellationMaxForward, constellationMaxReverse
	if !showClosedAgents {
		forwardCap *= constellationHydrateMultiplier
		reverseCap *= constellationHydrateMultiplier
	}
	forwardIDs, forwardOriginByID := taggedTargets(forwardDeps, true, forwardCap)
	reverseIDs, reverseOriginByID := taggedTargets(reverseDeps, false, reverseCap)
	hydrated := s.hydrateBeads(ctx, append(append([]string{}, forwardIDs...), reverseIDs...))
	if showClosedAgents {
		// Closed agents are allowed back in, but session beads never are.
		forwardIDs, forwardOriginByID = filterSessionBeads(forwardIDs, forwardOriginByID, hydrated, constellationMaxForward)
		reverseIDs, reverseOriginByID = filterSessionBeads(reverseIDs, reverseOriginByID, hydrated, constellationMaxReverse)
	} else {
		forwardIDs, forwardOriginByID = filterHiddenOrbitBeads(forwardIDs, forwardOriginByID, hydrated, constellationMaxForward)
		reverseIDs, reverseOriginByID = filterHiddenOrbitBeads(reverseIDs, reverseOriginByID, hydrated, constellationMaxReverse)
	}

	l1Radius := levelRadii[0]
	// --- Forward dependencies (parents) along the top arc. ---
	forwardAngles := make(map[string]float64, len(forwardIDs))
	for i, id := range forwardIDs {
		angle := arcAngle(i, len(forwardIDs), -150, -30)
		x, y := polarPosition(cx, cy, l1Radius, angle)
		forwardAngles[id] = angle
		bd := hydrated[id]
		node := depNode(id, bd, x, y, "forward", s.basePath)
		view.Forward = append(view.Forward, node)
		view.Edges = append(view.Edges, mentionEdge(
			view.Center.X, view.Center.Y, x, y, "forward",
			bead.ID, id, forwardOriginByID[id],
		))
	}

	// --- Reverse dependencies (children) along the left arc. ---
	reverseAngles := make(map[string]float64, len(reverseIDs))
	for i, id := range reverseIDs {
		angle := arcAngle(i, len(reverseIDs), 150, 210)
		x, y := polarPosition(cx, cy, l1Radius, angle)
		reverseAngles[id] = angle
		bd := hydrated[id]
		node := depNode(id, bd, x, y, "reverse", s.basePath)
		view.Reverse = append(view.Reverse, node)
		view.Edges = append(view.Edges, mentionEdge(
			view.Center.X, view.Center.Y, x, y, "reverse",
			id, bead.ID, reverseOriginByID[id],
		))
	}

	// --- Outer rings (level 2..TreeLevel) ---
	// Each deeper level fans the transitive deps of every parent at the
	// previous level around its own radial direction. Edges hop level→level
	// (grandparent → parent → center) so the tree topology reads visually.
	if treeLevel >= 2 && len(levelRadii) >= 2 {
		s.expandOuterLevels(ctx, view, hydrated, forwardIDs, forwardAngles, reverseIDs, reverseAngles, levelRadii, treeLevel, cx, cy, showClosedAgents)
	}

	// --- Assignee linkage (ka-0000003339): synthetic agent node + edge ---
	// Rendered directly below the center at a tighter radius than the L1 dep
	// arc so it sits in the empty area between center and the children/parents
	// rings. Reuses constellation_node, age opacity stays at 1.0 (assignment
	// is bead-scoped, not a deps-graph entity, so it doesn't participate in
	// the recency curve).
	if bead.Assignee != "" {
		ax, ay := assigneePosition(cx, cy, l1Radius)
		href := s.assigneeNodeHref(ctx, bead.Assignee)
		view.Assignee = assigneeNode(bead.Assignee, ax, ay, href)
		view.Edges = append(view.Edges, assigneeEdge(view.Center.X, view.Center.Y, ax, ay, bead.ID, bead.Assignee))
		view.HasAssignee = true
	}

	// --- Succession linkage (task ka-0000007852): display-only predecessor ---
	// When the center is an agent with a succession back-pointer, synthesize a
	// single node + edge to its immediate predecessor from the field-walk
	// (prior_agent_id ∨ schedule_prior_agent_id). Rendered ABOVE the center at a
	// tighter radius than the L1 arc — a distinct region from the assignee node
	// below — with its own glyph/legend. Kept entirely out of the deps graph.
	if bead.Type == "agent" {
		if predID := lineagePriorID(bead.Fields); predID != "" {
			pred := s.hydrateBeads(ctx, []string{predID})[predID]
			px, py := predecessorPosition(cx, cy, l1Radius)
			view.Predecessor = predecessorNode(predID, pred, px, py, s.basePath)
			view.Edges = append(view.Edges, predecessorEdge(view.Center.X, view.Center.Y, px, py, bead.ID, predID))
			view.HasPredecessor = true
		}
	}

	view.HasSimilar = len(view.Similar) > 0
	view.HasForward = len(view.Forward) > 0
	view.HasReverse = len(view.Reverse) > 0
	view.HasOuter = len(view.Outer) > 0
	view.HasAnything = view.HasSimilar || view.HasForward || view.HasReverse || view.HasOuter || view.HasAssignee || view.HasPredecessor
	view.Legend = constellationLegendItems()
	applyAgeOpacity(view, bead, hydrated, similar)
	return view
}

// applyAgeOpacity computes the (oldest, newest) recency window across every
// orbit node in the view (using UpdatedAt, falling back to CreatedAt) and
// assigns Opacity to each via computeAgeOpacity. The center node always
// renders at 1.0 — it is the focus, and the golden halo would look broken
// behind a faded disk.
//
// Nodes without any usable timestamp keep Opacity = 1.0 (set explicitly so
// the template never emits an empty fill-opacity attribute).
func applyAgeOpacity(
	view *constellationView,
	center *beadsapi.BeadDetail,
	hydrated map[string]*beadsapi.BeadDetail,
	similar *beadsapi.FindSimilarResponse,
) {
	view.Center.Opacity = 1.0

	// Gather (id, timestamp) for every orbit node so we can find the range
	// in one pass without ranging the slices three more times below.
	type stamped struct {
		idx     int
		role    string
		ts      time.Time
		hasTime bool
	}
	stamps := make([]stamped, 0, len(view.Forward)+len(view.Reverse)+len(view.Similar))

	addFromBead := func(idx int, role string, bd *beadsapi.BeadDetail) {
		s := stamped{idx: idx, role: role}
		if bd != nil {
			s.ts, s.hasTime = pickAgeTimestamp(bd.UpdatedAt, bd.CreatedAt)
		}
		stamps = append(stamps, s)
	}

	for i, n := range view.Forward {
		addFromBead(i, "forward", hydrated[n.ID])
	}
	for i, n := range view.Reverse {
		addFromBead(i, "reverse", hydrated[n.ID])
	}
	for i, n := range view.Outer {
		addFromBead(i, "outer", hydrated[n.ID])
	}
	// Similar beads don't go through hydrateBeads — their timestamps live on
	// FindSimilarResponse.Results[i].Bead. Walk the matching index.
	if similar != nil {
		results := similar.Results
		if len(results) > constellationMaxSimilar {
			results = results[:constellationMaxSimilar]
		}
		// Map the ranged result index → node index in view.Similar (some
		// results are skipped above when r==nil or r.Bead==nil).
		simIdx := 0
		for _, r := range results {
			if r == nil || r.Bead == nil {
				continue
			}
			if simIdx >= len(view.Similar) {
				break
			}
			s := stamped{idx: simIdx, role: "similar"}
			s.ts, s.hasTime = pickAgeTimestamp(r.Bead.UpdatedAt, r.Bead.CreatedAt)
			stamps = append(stamps, s)
			simIdx++
		}
	}

	// Find the (oldest, newest) window across orbit nodes only — the center
	// is excluded so a recently-touched center doesn't compress the curve
	// against a much older orbit.
	var oldest, newest time.Time
	have := false
	for _, s := range stamps {
		if !s.hasTime {
			continue
		}
		if !have {
			oldest, newest = s.ts, s.ts
			have = true
			continue
		}
		if s.ts.Before(oldest) {
			oldest = s.ts
		}
		if s.ts.After(newest) {
			newest = s.ts
		}
	}
	// Fall back to the center bead's own timestamp if every orbit node was
	// missing one — keeps the helper's range non-degenerate.
	if !have && center != nil {
		ts, ok := pickAgeTimestamp(center.UpdatedAt, center.CreatedAt)
		if ok {
			oldest, newest, have = ts, ts, true
		}
	}

	// Default everything to 1.0 first so the template attribute is always set.
	for i := range view.Forward {
		view.Forward[i].Opacity = 1.0
	}
	for i := range view.Reverse {
		view.Reverse[i].Opacity = 1.0
	}
	for i := range view.Similar {
		view.Similar[i].Opacity = 1.0
	}
	for i := range view.Outer {
		view.Outer[i].Opacity = 1.0
	}
	if !have {
		return
	}

	for _, s := range stamps {
		if !s.hasTime {
			continue
		}
		op := computeAgeOpacity(s.ts, oldest, newest, ageOpacityFloor)
		switch s.role {
		case "forward":
			view.Forward[s.idx].Opacity = op
		case "reverse":
			view.Reverse[s.idx].Opacity = op
		case "similar":
			view.Similar[s.idx].Opacity = op
		case "outer":
			view.Outer[s.idx].Opacity = op
		}
	}
}

// pickAgeTimestamp returns updated (preferred) else created, with a flag for
// "we have something usable." A zero time.Time counts as missing.
func pickAgeTimestamp(updated, created time.Time) (time.Time, bool) {
	if !updated.IsZero() {
		return updated, true
	}
	if !created.IsZero() {
		return created, true
	}
	return time.Time{}, false
}

// computeAgeOpacity maps a timestamp to a fill-opacity in [floor, 1.0] using
// linear interpolation against log-age:
//
//	ageMs    = max(1, newest - ts)            // milliseconds since newest
//	maxAgeMs = max(1, newest - oldest)
//	t        = log(ageMs) / log(maxAgeMs)     // 0 at newest, 1 at oldest
//	op       = 1 - t * (1 - floor)
//
// The newest node returns 1.0; the oldest returns the floor; intermediates
// fall on the curve. A degenerate range (oldest == newest, or ts past newest,
// or ts before oldest) clamps to 1.0 / floor respectively.
func computeAgeOpacity(ts, oldest, newest time.Time, floor float64) float64 {
	if floor < 0 {
		floor = 0
	}
	if floor > 1 {
		floor = 1
	}
	if ts.IsZero() || newest.IsZero() {
		return 1.0
	}
	if !newest.After(oldest) {
		// No spread — every node is "newest."
		return 1.0
	}
	if !ts.Before(newest) {
		return 1.0
	}
	if !ts.After(oldest) {
		return floor
	}
	ageMs := float64(newest.Sub(ts).Milliseconds())
	maxAgeMs := float64(newest.Sub(oldest).Milliseconds())
	if ageMs < 1 {
		ageMs = 1
	}
	if maxAgeMs < 1 {
		maxAgeMs = 1
	}
	t := math.Log(ageMs) / math.Log(maxAgeMs)
	if t < 0 {
		t = 0
	}
	if t > 1 {
		t = 1
	}
	return 1.0 - t*(1.0-floor)
}

// arcAngle returns the radian angle for item `i` of `n` evenly spread across
// arcStartDeg..arcEndDeg. Single-item lists land at the arc midpoint.
func arcAngle(i, n int, arcStartDeg, arcEndDeg float64) float64 {
	frac := 0.5
	if n > 1 {
		frac = float64(i) / float64(n-1)
	}
	return (arcStartDeg + (arcEndDeg-arcStartDeg)*frac) * math.Pi / 180.0
}

// polarPosition converts a polar (radius, angle-radians) coordinate around
// (cx, cy) into (x, y).
func polarPosition(cx, cy, radius, angleRad float64) (float64, float64) {
	return cx + radius*math.Cos(angleRad), cy + radius*math.Sin(angleRad)
}

// parseShowClosedAgentsParam reads the `?show_closed_agents=...` URL
// parameter. Closed agent/session beads are hidden from the constellation
// by default (ka-0000005724) — the parameter is opt-in. Truthy values
// (`1`, `true`, `yes`, `on`) re-enable rendering of closed agent and
// session orbit nodes; everything else (including the empty string)
// keeps them hidden.
func parseShowClosedAgentsParam(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// constellationDefaultTreeLevel is the tree-depth the constellation renders at
// when the `?tree=N` URL parameter is absent or unparseable. Level 3 shows the
// full transitive hierarchy by default (ka-0000004042) so the dependency graph
// is visible without the user having to drag the slider.
const constellationDefaultTreeLevel = 3

// parseTreeLevelParam reads the `?tree=N` URL parameter (any non-int / out-of-range
// value falls back to constellationDefaultTreeLevel) and clamps it to the
// supported slider range.
func parseTreeLevelParam(raw string) int {
	if raw == "" {
		return constellationDefaultTreeLevel
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return constellationDefaultTreeLevel
	}
	return clampTreeLevel(n)
}

// clampTreeLevel snaps an arbitrary slider value into [1, constellationMaxTreeLevel].
func clampTreeLevel(level int) int {
	if level < 1 {
		return 1
	}
	if level > constellationMaxTreeLevel {
		return constellationMaxTreeLevel
	}
	return level
}

// constellationLayout returns viewBox dimensions, the center coordinate, and
// per-level radii for the tree-level slider. Level 1 keeps the original
// 1000×500 / center=(500,250) / radius=240 geometry. Deeper levels grow the
// viewBox so outer rings stay on-canvas.
func constellationLayout(treeLevel int) (width, height int, cx, cy float64, radii []float64) {
	switch clampTreeLevel(treeLevel) {
	case 3:
		return 1400, 1100, 700, 550, []float64{240, 380, 500}
	case 2:
		return 1200, 800, 600, 400, []float64{240, 380}
	default:
		return 1000, 500, 500, 250, []float64{240}
	}
}

// expandOuterLevels grows the constellation outward by recursively pulling
// each parent's own forward/reverse deps and rendering them on the next-level
// arc, fanned around the parent's radial direction. Mutates `view` in place,
// extends `hydrated` with newly fetched targets so applyAgeOpacity sees them,
// and emits hop-by-hop edges (level n+1 → level n) so the tree topology reads.
//
// Best-effort: each fetch shares a single bounded context, and any failure
// downgrades to "fewer outer nodes" rather than an error.
func (s *Server) expandOuterLevels(
	ctx context.Context,
	view *constellationView,
	hydrated map[string]*beadsapi.BeadDetail,
	l1ForwardIDs []string,
	forwardAngles map[string]float64,
	l1ReverseIDs []string,
	reverseAngles map[string]float64,
	levelRadii []float64,
	treeLevel int,
	cx, cy float64,
	showClosedAgents bool,
) {
	depCtx, cancel := context.WithTimeout(ctx, constellationHydrateTime)
	defer cancel()

	// Track which ids we've already drawn (any role) to keep the tree acyclic
	// and avoid duplicate nodes when a grandparent re-points at a sibling.
	seen := map[string]struct{}{view.Center.ID: {}}
	for _, n := range view.Forward {
		seen[n.ID] = struct{}{}
	}
	for _, n := range view.Reverse {
		seen[n.ID] = struct{}{}
	}

	// `frontier` maps parent-id → its angle (radians) on the previous ring,
	// in the relevant direction. Each level reduces the frontier to the
	// new outer-level ids actually drawn so the next level can fan around them.
	//
	// The dep fetch is batched once per outer level: the daemon's
	// /v1/batch/dependencies endpoint collapses what used to be N per-parent
	// round-trips into one (ka-0000008009). A hard fetch failure downgrades
	// the entire level to "no expansion" — same semantics as the per-parent
	// `continue` path the loop used to take.
	expandLevel := func(level int, role string, frontier map[string]float64, direction string, targetField func(beadsapi.Dependency) string) map[string]float64 {
		if level >= len(levelRadii) {
			return nil
		}
		radius := levelRadii[level]
		next := make(map[string]float64)
		// Iterate parents in stable order — Go map iteration is randomized
		// and we want deterministic layout.
		parents := make([]string, 0, len(frontier))
		for id := range frontier {
			parents = append(parents, id)
		}
		sort.Strings(parents)
		if len(parents) == 0 {
			return next
		}
		// One batched daemon round-trip for the whole frontier. The shape
		// is map[parentID][]Dependency; parents with no edges land as nil,
		// which the per-parent loop below treats the same as "no rows".
		depsByParent, err := s.daemon.BatchGetDependencies(depCtx, parents, direction)
		if err != nil {
			s.logger.Debug("constellation transitive batch fetch", "role", role, "level", level, "parents", len(parents), "error", err)
			return next
		}

		// Agent/session nodes are connective hubs, not leaves: a task links to
		// an agent, and that agent also links to the OTHER tasks it touched —
		// but those sibling tasks hang off the OPPOSITE dep direction (the
		// agent depends-on its tasks; tasks are reached by a reverse-walk
		// while a forward frontier follows forward edges, and vice-versa).
		// A single-direction walk therefore dead-ends at every agent and the
		// siblings never surface (T1→A1→T2/T3 invisible; ka-0000006339). For
		// agent-like parents only, also fetch the opposite direction so edges
		// propagate THROUGH the hub. Scoped to agent/session to avoid the
		// general dep-graph hairball prior commits fought (ka-0000000447).
		oppositeDir := "reverse"
		oppositeField := func(d beadsapi.Dependency) string { return d.BeadID }
		if direction == "reverse" {
			oppositeDir = "forward"
			oppositeField = func(d beadsapi.Dependency) string { return d.DependsOnID }
		}
		hubParents := make([]string, 0, len(parents))
		for _, id := range parents {
			if isAgentLike(hydrated[id]) {
				hubParents = append(hubParents, id)
			}
		}
		var oppositeByParent map[string][]beadsapi.Dependency
		if len(hubParents) > 0 {
			oppositeByParent, err = s.daemon.BatchGetDependencies(depCtx, hubParents, oppositeDir)
			if err != nil {
				s.logger.Debug("constellation hub passthrough batch fetch", "role", role, "level", level, "hubs", len(hubParents), "error", err)
				oppositeByParent = nil
			}
		}

		for _, parentID := range parents {
			parentAngle := frontier[parentID]
			// Candidate edge sources for this parent: its own forward/reverse
			// rows, plus — for agent-like hubs — the opposite-direction rows so
			// sibling work reached through the agent is followed too.
			type candSource struct {
				rows    []beadsapi.Dependency
				extract func(beadsapi.Dependency) string
			}
			sources := []candSource{{depsByParent[parentID], targetField}}
			if opp := oppositeByParent[parentID]; len(opp) > 0 {
				sources = append(sources, candSource{opp, oppositeField})
			}
			// Pick at most fanout rows that haven't already been drawn.
			// When the closed-agent filter is active, over-fetch by the
			// hydration multiplier so closed agent/session children can be
			// dropped post-hydration without leaving empty slots
			// (ka-0000005724).
			fanout := constellationFanoutPerParent
			if !showClosedAgents {
				fanout *= constellationHydrateMultiplier
			}
			picked := make([]string, 0, fanout)
			pickedSeen := make(map[string]struct{}, fanout)
		collect:
			for _, src := range sources {
				for _, d := range src.rows {
					// Skip triggered-by provenance edges in the L2+ traversal.
					// triggered-by means "this work was caused by that incoming
					// bead" and is not part of the parent/child decomposition the
					// constellation tree is showing — following it would pull
					// unrelated lineage into the visual hierarchy (auto-link
					// epic ka-0000000933, M1).
					if d.Type == "triggered-by" {
						continue
					}
					targetID := src.extract(d)
					if targetID == "" || targetID == parentID {
						continue
					}
					if _, dupe := seen[targetID]; dupe {
						continue
					}
					if _, dupe := pickedSeen[targetID]; dupe {
						continue
					}
					picked = append(picked, targetID)
					pickedSeen[targetID] = struct{}{}
					if len(picked) >= fanout {
						break collect
					}
				}
			}
			if len(picked) == 0 {
				continue
			}
			// Hydrate the picked ids so the rendered nodes carry status / type.
			fresh := s.hydrateBeads(depCtx, picked)
			for k, v := range fresh {
				hydrated[k] = v
			}
			{
				// Session beads are always dropped (ka-0000000532); closed agents
				// are dropped only when the user has not opted into
				// ?show_closed_agents=1.
				drop := isSessionBead
				if !showClosedAgents {
					drop = isHiddenOrbitBead
				}
				kept := picked[:0]
				for _, id := range picked {
					if drop(hydrated[id]) {
						continue
					}
					kept = append(kept, id)
					if len(kept) >= constellationFanoutPerParent {
						break
					}
				}
				picked = kept
				if len(picked) == 0 {
					continue
				}
			}
			// Fan picked ids around parentAngle: ±15° per extra level, evenly spread.
			fanDeg := 18.0 + 6.0*float64(level-1) // small fan; widens slightly with depth
			fanRad := fanDeg * math.Pi / 180.0
			parentX, parentY := polarPosition(cx, cy, levelRadii[level-1], parentAngle)
			n := len(picked)
			for i, targetID := range picked {
				frac := 0.5
				if n > 1 {
					frac = float64(i) / float64(n-1)
				}
				offset := -fanRad + 2*fanRad*frac
				angle := parentAngle + offset
				x, y := polarPosition(cx, cy, radius, angle)
				bd := hydrated[targetID]
				outer := depNode(targetID, bd, x, y, "outer-"+role, s.basePath)
				outer.Radius = 14.0
				outer.GlyphY = y + 4
				outer.IDLabelY = y + outer.Radius + 14
				view.Outer = append(view.Outer, outer)
				// Edge from parent (previous-level node) → grandparent/grandchild.
				stroke, dash, opacity := edgeStyle(role, "dep")
				view.Edges = append(view.Edges, constellationEdge{
					X1: parentX, Y1: parentY, X2: x, Y2: y,
					Role:       role,
					Origin:     "dep",
					SourceBead: parentID, TargetBead: targetID,
					Stroke: stroke, Dash: dash, Opacity: opacity * 0.85,
				})
				seen[targetID] = struct{}{}
				next[targetID] = angle
			}
		}
		return next
	}

	// Forward (parents-of-parents): fetch *forward* deps of each parent.
	frontierFwd := make(map[string]float64, len(l1ForwardIDs))
	for _, id := range l1ForwardIDs {
		frontierFwd[id] = forwardAngles[id]
	}
	// Reverse (children-of-children): fetch *reverse* deps of each child.
	frontierRev := make(map[string]float64, len(l1ReverseIDs))
	for _, id := range l1ReverseIDs {
		frontierRev[id] = reverseAngles[id]
	}

	for level := 1; level < treeLevel && level < len(levelRadii); level++ {
		frontierFwd = expandLevel(level, "forward", frontierFwd, "forward", func(d beadsapi.Dependency) string { return d.DependsOnID })
		frontierRev = expandLevel(level, "reverse", frontierRev, "reverse", func(d beadsapi.Dependency) string { return d.BeadID })
	}
}

// tagExistingDeps wraps each deps-table Dependency row as a taggedDep with
// Origin = "dep" — the strongest provenance, deps-table rows are first-class
// edits that beat any text-derived mention to the same target.
func tagExistingDeps(rows []beadsapi.Dependency) []taggedDep {
	if len(rows) == 0 {
		return nil
	}
	out := make([]taggedDep, 0, len(rows))
	for _, d := range rows {
		out = append(out, taggedDep{BeadID: d.BeadID, DependsOnID: d.DependsOnID, Type: d.Type, Origin: "dep"})
	}
	return out
}

// mergeTypedMentionsAsDepEdges appends typed-mention rows (RelationshipType
// != nil) to `existing` as synthetic taggedDep edges with Origin = "text".
// Targets already represented in `existing` (e.g. as a real deps-table row,
// or as an earlier typed mention) are dropped — first edge wins, which keeps
// deps > text precedence per the buildConstellation contract.
//
// `forward` selects which slot of the synthetic dep to populate: forward
// (this bead mentions X) puts X in DependsOnID; reverse (X mentions this
// bead) puts X in BeadID and self in DependsOnID.
func mergeTypedMentionsAsDepEdges(existing []taggedDep, selfID string, refs []beadsapi.BeadReferenceSummary, forward bool) []taggedDep {
	if len(refs) == 0 {
		return existing
	}
	seen := taggedSeenSet(existing, forward)
	out := existing
	for _, r := range refs {
		if r.RelationshipType == nil {
			continue
		}
		if r.ID == "" || r.ID == selfID {
			continue
		}
		if _, dupe := seen[r.ID]; dupe {
			continue
		}
		seen[r.ID] = struct{}{}
		if forward {
			out = append(out, taggedDep{BeadID: selfID, DependsOnID: r.ID, Type: *r.RelationshipType, Origin: "text"})
		} else {
			out = append(out, taggedDep{BeadID: r.ID, DependsOnID: selfID, Type: *r.RelationshipType, Origin: "text"})
		}
	}
	return out
}

// appendUntypedMentionEdges appends untyped-mention rows (RelationshipType ==
// nil) to `existing` as taggedDep edges with Origin = "mention". These render
// as neutral gray dashed arcs in the constellation — distinct from typed
// mentions and from deps. Dedup against existing entries follows the same
// rule as the typed merge: first edge wins.
func appendUntypedMentionEdges(existing []taggedDep, selfID string, refs []beadsapi.BeadReferenceSummary, forward bool) []taggedDep {
	if len(refs) == 0 {
		return existing
	}
	seen := taggedSeenSet(existing, forward)
	out := existing
	for _, r := range refs {
		if r.RelationshipType != nil {
			continue
		}
		if r.ID == "" || r.ID == selfID {
			continue
		}
		if _, dupe := seen[r.ID]; dupe {
			continue
		}
		seen[r.ID] = struct{}{}
		if forward {
			out = append(out, taggedDep{BeadID: selfID, DependsOnID: r.ID, Origin: "mention"})
		} else {
			out = append(out, taggedDep{BeadID: r.ID, DependsOnID: selfID, Origin: "mention"})
		}
	}
	return out
}

func taggedSeenSet(rows []taggedDep, forward bool) map[string]struct{} {
	seen := make(map[string]struct{}, len(rows))
	for _, d := range rows {
		var id string
		if forward {
			id = d.DependsOnID
		} else {
			id = d.BeadID
		}
		if id == "" {
			continue
		}
		seen[id] = struct{}{}
	}
	return seen
}

// taggedTargets pulls the "other" bead ID from each taggedDep row, stable-sorts
// them, and caps to limit. Returns the ID list and a parallel map from each
// surviving target ID back to its winning taggedDep so callers can read its
// Origin / Type when building the edge.
func taggedTargets(rows []taggedDep, forward bool, limit int) ([]string, map[string]taggedDep) {
	winning := make(map[string]taggedDep, len(rows))
	ids := make([]string, 0, len(rows))
	for _, d := range rows {
		var id string
		if forward {
			id = d.DependsOnID
		} else {
			id = d.BeadID
		}
		if id == "" {
			continue
		}
		if _, ok := winning[id]; ok {
			continue
		}
		winning[id] = d
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if len(ids) > limit {
		// Keep the lookup map aligned with the truncated slice.
		dropped := ids[limit:]
		ids = ids[:limit]
		for _, id := range dropped {
			delete(winning, id)
		}
	}
	return ids, winning
}

// filterHiddenOrbitBeads drops hidden orbit beads (all session beads + closed
// agent beads, see isHiddenOrbitBead) from a hydrated (ids, originByID) pair and
// truncates the survivors to displayCap. Used after hydration so the next-best
// visible targets backfill the slots when the over-fetched candidates include
// hidden beads (ka-0000005724, ka-0000000532). Targets missing from `hydrated`
// (404, timeout) are kept — we don't know their type/status, and rendering them
// as "?" nodes is preferable to silently dropping them.
func filterHiddenOrbitBeads(ids []string, originByID map[string]taggedDep, hydrated map[string]*beadsapi.BeadDetail, displayCap int) ([]string, map[string]taggedDep) {
	return filterOrbitBy(ids, originByID, hydrated, displayCap, isHiddenOrbitBead)
}

// filterSessionBeads is the session-only variant of filterHiddenOrbitBeads: it
// drops session beads but keeps closed agents. Used on the bead-show surface
// when the user opted into ?show_closed_agents=1 — closed agents come back, but
// session bookkeeping stays hidden (ka-0000000532).
func filterSessionBeads(ids []string, originByID map[string]taggedDep, hydrated map[string]*beadsapi.BeadDetail, displayCap int) ([]string, map[string]taggedDep) {
	return filterOrbitBy(ids, originByID, hydrated, displayCap, isSessionBead)
}

// filterOrbitBy drops every id whose hydrated bead matches `drop` from an
// (ids, originByID) pair and truncates the survivors to displayCap. Targets
// missing from `hydrated` (404, timeout) are kept — we don't know their
// type/status, and rendering them as "?" nodes is preferable to silently
// dropping them.
//
// When the survivors overflow displayCap, open/in_progress beads win render
// slots over closed/deferred ones (ka-0000002450, matching the
// gardener-constellation cap behavior in commit 4abb58f8): the map exists to
// surface active work, so a truncating orbit must not drop live beads to draw
// closed ones. Status priority (statusOpenSortKey: 0 = open/in_progress/unknown,
// 1 = closed/deferred) is the primary key; the incoming ID order (taggedTargets
// sorts by ID) is preserved within a tier via a stable sort, so layout stays
// deterministic. Beads missing from `hydrated` carry an empty status, which
// statusOpenSortKey treats as priority 0 — consistent with keeping them visible.
func filterOrbitBy(ids []string, originByID map[string]taggedDep, hydrated map[string]*beadsapi.BeadDetail, displayCap int, drop func(*beadsapi.BeadDetail) bool) ([]string, map[string]taggedDep) {
	kept := make([]string, 0, len(ids))
	for _, id := range ids {
		if drop(hydrated[id]) {
			continue
		}
		kept = append(kept, id)
	}
	// Prioritize open/in_progress survivors before truncating to displayCap.
	sort.SliceStable(kept, func(i, j int) bool {
		return orbitStatusSortKey(hydrated[kept[i]]) < orbitStatusSortKey(hydrated[kept[j]])
	})
	if len(kept) > displayCap {
		kept = kept[:displayCap]
	}
	keptOrigin := make(map[string]taggedDep, len(kept))
	for _, id := range kept {
		if td, ok := originByID[id]; ok {
			keptOrigin[id] = td
		}
	}
	return kept, keptOrigin
}

// orbitStatusSortKey returns the open-first cap-ordering key for a (possibly
// nil) hydrated bead. A missing bead has unknown status, which sorts as
// open/in_progress (priority 0) so we keep it visible rather than dropping it
// to draw a known-closed bead.
func orbitStatusSortKey(bd *beadsapi.BeadDetail) int {
	if bd == nil {
		return 0
	}
	return statusOpenSortKey(bd.Status)
}

// mentionEdge constructs a constellationEdge for a given (source, target,
// origin) tuple. role is the layout direction ("forward" or "reverse") and
// drives the base stroke color for "dep" / "text" edges. Untyped "mention"
// edges use a neutral gray independent of role.
func mentionEdge(x1, y1, x2, y2 float64, role, source, target string, td taggedDep) constellationEdge {
	stroke, dash, opacity := edgeStyleFor(role, td.Origin, td.Type)
	verb := td.Type
	return constellationEdge{
		X1: x1, Y1: y1, X2: x2, Y2: y2,
		Role:             role,
		Origin:           td.Origin,
		RelationshipType: verb,
		SourceBead:       source,
		TargetBead:       target,
		TitleText:        edgeTitle(role, td.Origin, verb),
		Stroke:           stroke,
		Dash:             dash,
		Opacity:          opacity,
	}
}

// edgeStyle returns (stroke, dash, opacity) for an edge given its layout role
// and provenance origin. Per the typed-mentions plan (ka-0000004349):
//
//   - role + "dep"      → solid direction-color edge (existing styling).
//   - role + "text"     → direction-color edge with subtle 6 2 dash to mark
//     text-derived provenance.
//   - role + "mention"  → neutral #7a7a7a dashed 3 4 at 0.55 opacity.
//   - "assignee"        → golden #b9851b solid edge tying center to the
//     synthetic agent node (ka-0000003339).
func edgeStyle(role, origin string) (stroke, dash string, opacity float64) {
	return edgeStyleFor(role, origin, "")
}

// edgeStyleFor extends edgeStyle with the dep-type verb so distinct relationship
// types can get distinct visual treatment. Currently the only verb that
// overrides the base style is `triggered-by` (auto-link epic ka-0000000933, M1):
// triggered-by edges render in a violet hue (#8a5cf6) with a dot-dash pattern
// so users can tell at a glance that the edge is provenance lineage ("this work
// was caused by that incoming bead") rather than epic decomposition
// (parent-child) or scheduling (blocks). Empty verb / unknown verb falls back
// to the role/origin-driven base style.
func edgeStyleFor(role, origin, verb string) (stroke, dash string, opacity float64) {
	if verb == "triggered-by" && (origin == "dep" || origin == "text" || origin == "") {
		if origin == "text" {
			dash = "6 2"
		} else {
			dash = "4 2 1 2"
		}
		return "#8a5cf6", dash, 0.85
	}
	directionColor := "#3b6fa0"
	if role == "reverse" {
		directionColor = "#5d8a3a"
	}
	switch origin {
	case "mention":
		return "#7a7a7a", "3 4", 0.55
	case "text":
		return directionColor, "6 2", 0.85
	case "assignee":
		return "#b9851b", "", 0.85
	case "succession":
		// Display-only succession edge (task ka-0000007852): teal, long-dashed,
		// distinct from the violet triggered-by provenance and the golden
		// assignee edge. Never a deps edge.
		return "#0f9b8e", "5 3", 0.9
	default: // "dep" and any unknown origin
		return directionColor, "", 0.85
	}
}

// edgeTitle returns a short SVG <title> tooltip for an edge. Dep edges show
// just the verb ("blocks"); typed mentions append "(text)" so users can see
// the provenance distinction without reading the dash; untyped mentions
// render as "mentions (text)".
func edgeTitle(role, origin, verb string) string {
	switch origin {
	case "mention":
		return "mentions (text)"
	case "text":
		if verb != "" {
			return verb + " (text)"
		}
		return "mention (text)"
	case "dep":
		if verb != "" {
			return verb
		}
		if role == "reverse" {
			return "depended on by"
		}
		return "depends on"
	case "assignee":
		return "assigned to"
	}
	return ""
}

// assigneePosition returns the (x, y) for the synthetic assignee node:
// directly below the center at ~55% of the L1 dep radius. That keeps it
// inside the L1 ring (so it doesn't fight parents/children/similar arcs)
// while remaining clearly outside the center disk.
func assigneePosition(cx, cy, l1Radius float64) (float64, float64) {
	return cx, cy + l1Radius*0.55
}

// assigneeNodeHref resolves the assignee string to a useful destination
// URL. The assignee can be:
//
//   - an agent name (e.g. "thread-123…") — resolve to its agent bead and
//     link at /{ka-id} so the disk lands on the agent's bead-show page;
//   - a ka-XXX bead id — link straight through at /{ka-id};
//   - a human / OIDC subject / anything else — fall back to a search for
//     beads assigned to that name.
//
// Previously the href was always basePath+"/"+assignee, which 404'd for
// raw agent names (they aren't bead ids), producing a dead page on click
// (ka-0000002108). Resolution mirrors renderAgentLink's name→bead lookup.
func (s *Server) assigneeNodeHref(ctx context.Context, assignee string) string {
	if assignee == "" {
		return ""
	}
	// ka-XXX ids route straight through to bead-show.
	if strings.HasPrefix(assignee, "gd-") || strings.HasPrefix(assignee, "ka-") {
		return s.basePath + "/" + url.PathEscape(assignee)
	}
	// Agent-shaped names resolve to their most-recent agent bead.
	if looksLikeAgentName(assignee) && s.daemon != nil {
		if bead, err := s.daemon.FindMostRecentAgentBead(ctx, assignee); err == nil && bead != nil {
			return s.basePath + "/" + url.PathEscape(bead.ID)
		}
	}
	// Anything else (humans, OIDC subjects, unresolved agents): a search
	// for that assignee is a useful page rather than a 404.
	return s.basePath + "/search?q=" + url.QueryEscape(assignee)
}

// assigneeNode builds a constellationNode for the synthetic agent disk.
// href is resolved by assigneeNodeHref so the disk links to a useful page
// (the agent's bead, a kd bead, or a search) rather than the raw assignee
// name, which 404'd for agent names (ka-0000002108).
func assigneeNode(assignee string, x, y float64, href string) constellationNode {
	rad := 16.0
	return constellationNode{
		ID:       assignee,
		Title:    "assignee: " + assignee,
		X:        x,
		Y:        y,
		Radius:   rad,
		GlyphY:   y + 4,
		IDLabelY: y + rad + 14,
		Status:   "",
		Type:     "agent",
		Role:     "assignee",
		Color:    "#f1c84a", // golden disk to echo the center ring
		Stroke:   "#b9851b",
		Glyph:    typeGlyph("agent"),
		Href:     href,
		Opacity:  1.0,
	}
}

// predecessorPosition returns the (x, y) for the display-only succession node:
// directly ABOVE the center at ~55% of the L1 dep radius. This mirrors the
// assignee node's placement below the center, keeping the succession region
// visually distinct and inside the L1 ring so it doesn't fight the dep arcs.
func predecessorPosition(cx, cy, l1Radius float64) (float64, float64) {
	return cx, cy - l1Radius*0.55
}

// predecessorNode builds the display-only succession node for an agent's
// immediate predecessor. Uses the predecessor's real status color when it
// hydrated; degrades to a neutral ID-only disk otherwise (the bead may have
// been reaped). Role "succession" keeps it out of the dep-role styling.
func predecessorNode(id string, bd *beadsapi.BeadDetail, x, y float64, basePath string) constellationNode {
	rad := 16.0
	n := constellationNode{
		ID:       id,
		X:        x,
		Y:        y,
		Radius:   rad,
		GlyphY:   y + 4,
		IDLabelY: y + rad + 14,
		Role:     "succession",
		Type:     "agent",
		Glyph:    typeGlyph("agent"),
		Href:     basePath + "/" + id,
		Opacity:  1.0,
	}
	if bd != nil {
		n.Title = "predecessor: " + bd.Title
		n.Status = bd.Status
		n.Color = statusFill(bd.Status)
		n.Stroke = statusStroke(bd.Status)
	} else {
		n.Title = "predecessor: " + id
		n.Color = "#cfcfcf"
		n.Stroke = "#888"
	}
	return n
}

// predecessorEdge builds the synthesized, DISPLAY-ONLY succession edge tying
// the center agent to its predecessor. Origin "succession" gives it a distinct
// hue + dash and a legend entry, so an operator reads it as provenance lineage
// ("this run succeeded that one") that is deliberately NOT part of the
// dependency graph.
func predecessorEdge(x1, y1, x2, y2 float64, beadID, predID string) constellationEdge {
	stroke, dash, opacity := edgeStyle("succession", "succession")
	return constellationEdge{
		X1: x1, Y1: y1, X2: x2, Y2: y2,
		Role:       "succession",
		Origin:     "succession",
		SourceBead: beadID,
		TargetBead: predID,
		TitleText:  "succeeds",
		Stroke:     stroke,
		Dash:       dash,
		Opacity:    opacity,
	}
}

// assigneeEdge builds the constellationEdge tying the center bead to its
// assignee node. SourceBead = the bead, TargetBead = the assignee string —
// keeps the data-source-bead / data-target-bead attribute contract aligned
// with the rest of the graph for any future click-handler.
func assigneeEdge(x1, y1, x2, y2 float64, beadID, assignee string) constellationEdge {
	stroke, dash, opacity := edgeStyle("assignee", "assignee")
	return constellationEdge{
		X1: x1, Y1: y1, X2: x2, Y2: y2,
		Role:       "assignee",
		Origin:     "assignee",
		SourceBead: beadID,
		TargetBead: assignee,
		TitleText:  edgeTitle("assignee", "assignee", ""),
		Stroke:     stroke,
		Dash:       dash,
		Opacity:    opacity,
	}
}

// hydrateBeads fetches BeadDetail for the supplied IDs in one round-trip via
// the daemon's batch endpoint (sharding above BatchGetBeadsMaxIDs is handled
// by the client). Returns a map keyed by ID; missing entries (404, timeout,
// etc.) are simply absent — callers fall back to ID-only rendering.
func (s *Server) hydrateBeads(ctx context.Context, ids []string) map[string]*beadsapi.BeadDetail {
	if len(ids) == 0 {
		return map[string]*beadsapi.BeadDetail{}
	}
	hydrateCtx, cancel := context.WithTimeout(ctx, constellationHydrateTime)
	defer cancel()
	out, err := s.daemon.BatchGetBeads(hydrateCtx, ids)
	if err != nil {
		s.logger.Debug("hydrate batch", "ids", len(ids), "error", err)
		return map[string]*beadsapi.BeadDetail{}
	}
	return out
}

func depNode(id string, bd *beadsapi.BeadDetail, x, y float64, role, basePath string) constellationNode {
	rad := 16.0
	n := constellationNode{
		ID:       id,
		X:        x,
		Y:        y,
		Radius:   rad,
		GlyphY:   y + 4,
		IDLabelY: y + rad + 14,
		Role:     role,
		Href:     basePath + "/" + id,
	}
	if bd != nil {
		n.Title = bd.Title
		n.Status = bd.Status
		n.Type = bd.Type
		n.Color = statusFill(bd.Status)
		n.Stroke = statusStroke(bd.Status)
		n.Glyph = typeGlyph(bd.Type)
		n.Priority = bd.Priority
		applyMRState(&n, bd.Type, bd.Fields)
	} else {
		n.Color = "#cfcfcf"
		n.Stroke = "#888"
		n.Glyph = "?"
	}
	return n
}

// isHiddenOrbitBead returns true when bd is an orbit node that should be hidden
// from the constellation by default. Two families qualify:
//
//   - ANY session bead, regardless of status (ka-0000000532). Session beads are
//     pure machine bookkeeping — they add noise without telling a human anything
//     useful about the work graph, so they are hidden everywhere constellations
//     render.
//   - CLOSED agent beads — the long tail of despawned crew/gardener invocations
//     (ka-0000005724). Open agent beads stay visible (a live gardener's
//     invocations are the seeds of its map).
//
// Nil-safe: returns false for missing hydrate results so unhydrated targets stay
// visible (status/type unknown ≠ "hidden orbit bead").
func isHiddenOrbitBead(bd *beadsapi.BeadDetail) bool {
	if bd == nil {
		return false
	}
	return isHiddenOrbitBeadFields(bd.Type, bd.Status)
}

// isHiddenOrbitBeadFields is the field-level form used when the caller already
// has type/status (e.g. SemanticSearchResult.Bead) and would rather not allocate
// a wrapper.
func isHiddenOrbitBeadFields(beadType, status string) bool {
	if beadType == "session" {
		return true
	}
	return beadType == "agent" && status == "closed"
}

// isAgentLike reports whether bd is an agent or session bead, regardless of
// status. Unlike isHiddenOrbitBead (which hides the stale tail), this marks a
// node as a connective *hub* in the dependency graph: an agent sits between a
// task and the OTHER tasks that agent also touched. The constellation walk
// must pass THROUGH such a node rather than treat it as a leaf, or edges
// dead-end at every agent and sibling tasks never surface (ka-0000006339).
// Nil-safe: an unhydrated target (type unknown) returns false.
func isAgentLike(bd *beadsapi.BeadDetail) bool {
	if bd == nil {
		return false
	}
	return bd.Type == "agent" || bd.Type == "session"
}

// isSessionBead reports whether bd is a session bead (any status). Session beads
// are hidden by default across every constellation surface (ka-0000000532). The
// gardener map and inbox panel use THIS predicate rather than isHiddenOrbitBead:
// they legitimately seed on / surround agent beads (a gardener's invocations are
// its map seeds), so closed agents must stay visible there — only the noisy
// session bookkeeping is dropped. Nil-safe: missing hydrate results are kept.
func isSessionBead(bd *beadsapi.BeadDetail) bool {
	return bd != nil && bd.Type == "session"
}

// isAgentBead reports whether bd is an agent (iteration) bead. The gardener map's
// connected-beads walk uses this to scope agent attribution to the gardener's
// OWN life-thread (bug ka-0000005604): the undirected walk reaches foreign
// gardeners' agents through a shared epic/label, and only the gardener's own
// seeds (its schedule_id/trigger_id-scoped invocations) should be listed as its
// connected agents. Nil-safe: an unhydrated bead is treated as not-an-agent so a
// daemon miss never drops a node it cannot positively classify as foreign.
func isAgentBead(bd *beadsapi.BeadDetail) bool {
	return bd != nil && bd.Type == "agent"
}

// statusFill returns the disk fill color for a bead status. Uses the same
// palette as the .status-pill chips in layout.html so the SVG reads
// consistently with the rest of the page.
func statusFill(status string) string {
	switch status {
	case "open":
		return "#a8d4ab" // soft green
	case "in_progress":
		return "#9bc7f0" // soft blue
	case "deferred":
		return "#f3c889" // soft amber
	case "partial":
		return "#e0b3a0" // soft terracotta — terminal but flags descendant failures
	case "closed":
		return "#cfcfcf" // soft gray
	default:
		return "#dcdcdc"
	}
}

func statusStroke(status string) string {
	switch status {
	case "open":
		return "#3a8a3f"
	case "in_progress":
		return "#1d6bbd"
	case "deferred":
		return "#a36a14"
	case "partial":
		return "#b5673f" // terracotta stroke to match the partial fill
	case "closed":
		return "#777"
	default:
		return "#888"
	}
}

// mrOverlayColor returns the corner-badge fill for an MR's state. Purple for
// merged, blue for opened, gray for closed-unmerged; empty string when the
// state is unknown so the template suppresses the badge entirely.
// Mirrors the palette used by inbox MR chips for visual continuity
// (ka-0000004604).
func mrOverlayColor(state string) string {
	switch state {
	case "merged":
		return "#8a5cf6" // purple
	case "opened":
		return "#1d6bbd" // blue
	case "closed":
		return "#777777" // gray
	}
	return ""
}

// applyMRState flips a node into MR-diamond mode when the bead is a
// type=merge-request, plumbing the state-tinted overlay fill so the
// template can render the corner badge.
func applyMRState(n *constellationNode, beadType string, fields map[string]string) {
	if beadType != "merge-request" {
		return
	}
	n.IsMR = true
	state := mrFieldString(fields, "mr_state", "state")
	n.MRState = state
	n.MROverlay = mrOverlayColor(state)
	// Read-only upstream-MR mirror (E-PROV FW-5, ka-0000003146): the bridge /
	// `gb mr link --upstream` stamps external_mirror=true (alongside the
	// upstream:true / external-mirror:true labels). Render it as external so it
	// reads as "not-ours, pulled FROM upstream" — honoring the read-only-mirror
	// invariant ka-0000003234.
	if fields["external_mirror"] == "true" || fields["upstream"] == "true" {
		n.IsExternal = true
	}
}

// DiamondPath returns the SVG `d` attribute for the MR diamond glyph,
// expressed in the node-local frame (rendered with transform="translate(X,Y)").
// Same bounding-box as the equivalent circle so force-layout collision math
// stays unchanged.
func (n constellationNode) DiamondPath() string {
	return fmt.Sprintf("M0,-%.1f L %.1f,0 L 0,%.1f L -%.1f,0 Z", n.Radius, n.Radius, n.Radius, n.Radius)
}

// OverlayCX returns the x-coordinate (relative to the node centroid) of the
// MR state overlay badge — sits at the NE corner of the diamond.
func (n constellationNode) OverlayCX() float64 { return n.X + n.Radius*0.55 }

// OverlayCY returns the y-coordinate of the MR state overlay badge.
func (n constellationNode) OverlayCY() float64 { return n.Y - n.Radius*0.55 }

// OverlayR returns the radius of the MR state overlay badge. Scales with the
// node so it stays visually balanced across center/orbit/outer sizes.
func (n constellationNode) OverlayR() float64 {
	r := n.Radius * 0.42
	if r < 6 {
		r = 6
	}
	return r
}

// mapOverlayEmphasisStrokeWidth is the disk stroke-width for a node an overlay
// lens EMPHASIZES, vs. the constellation's default 1.6 — a heavier ring pulls
// the eye to the salient beads without a color change. Relocated here from the
// retired maps_overlay.go (ka-0000000247): OverlayStrokeWidth on the kept
// constellation node is its only remaining consumer.
const mapOverlayEmphasisStrokeWidth = 3.2

// OverlayStrokeWidth returns the disk/diamond stroke-width for the node. An
// Emphasis node (heat / needs-attention lens) gets a heavier ring so it reads
// as salient without a color change; everything else keeps the constellation's
// default 1.6. Pure function of the Emphasis flag, so constellations that never
// set Emphasis render identically to before.
func (n constellationNode) OverlayStrokeWidth() float64 {
	if n.Emphasis {
		return mapOverlayEmphasisStrokeWidth
	}
	return 1.6
}

// EmphasisHaloR returns the radius of the golden emphasis halo drawn behind an
// Emphasis node (Maps Phase 4 heat / needs-attention lens). Slightly larger
// than the disk so the ring reads as a glow, not a second border.
func (n constellationNode) EmphasisHaloR() float64 {
	r := n.Radius + 5.0
	if r < 12 {
		r = 12
	}
	return r
}

// typeGlyph returns a single-char marker for a bead type so the disks read at
// a glance. ASCII / common Unicode only — must render in any browser without
// loading custom fonts.
func typeGlyph(t string) string {
	switch t {
	case "epic":
		return "E"
	case "task":
		return "T"
	case "feature":
		return "F"
	case "bug":
		return "B"
	case "chore":
		return "C"
	case "formula":
		return "✦"
	case "molecule":
		return "M"
	case "decision":
		return "?"
	case "agent":
		return "A"
	case "advice":
		return "i"
	case "schedule":
		return "⏱"
	case "merge-request":
		return "↗"
	}
	if t == "" {
		return "•"
	}
	return strings.ToUpper(t[:1])
}

func constellationLegendItems() []constellationLegend {
	items := []constellationLegend{
		{Label: "open", Color: statusFill("open"), Shape: "node", Stroke: statusStroke("open")},
		{Label: "in progress", Color: statusFill("in_progress"), Shape: "node", Stroke: statusStroke("in_progress")},
		{Label: "deferred", Color: statusFill("deferred"), Shape: "node", Stroke: statusStroke("deferred")},
		{Label: "closed", Color: statusFill("closed"), Shape: "node", Stroke: statusStroke("closed")},
		// ka-0000004604: MR nodes render as a diamond with a corner badge
		// tinted by mr_state so they read as distinct at a glance from
		// task/bug/decision circles.
		{Label: "MR (merged)", Color: statusFill("closed"), Shape: "mr", Stroke: statusStroke("closed"), Overlay: mrOverlayColor("merged")},
		{Label: "MR (opened)", Color: statusFill("open"), Shape: "mr", Stroke: statusStroke("open"), Overlay: mrOverlayColor("opened")},
		{Label: "MR (closed)", Color: statusFill("closed"), Shape: "mr", Stroke: statusStroke("closed"), Overlay: mrOverlayColor("closed")},
		{Label: "dep (parent)", Shape: "edge", Stroke: "#3b6fa0"},
		{Label: "dep (child)", Shape: "edge", Stroke: "#5d8a3a"},
		{Label: "typed mention (text)", Shape: "edge", Stroke: "#3b6fa0", Dash: "6 2"},
		{Label: "triggered-by", Shape: "edge", Stroke: "#8a5cf6", Dash: "4 2 1 2"},
		{Label: "untyped mention", Shape: "edge", Stroke: "#7a7a7a", Dash: "3 4"},
		{Label: "assignee", Shape: "edge", Stroke: "#b9851b"},
		{Label: "succession (display-only)", Shape: "edge", Stroke: "#0f9b8e", Dash: "5 3"},
		{Label: "semantically similar", Shape: "edge", Stroke: "#9b8650", Dash: "3 4"},
	}
	for i := range items {
		items[i].OffsetY = i * 14
	}
	return items
}

// constellationTitleAttr returns a short hover-tooltip string for a node.
// Goldmark / template HTML escaping is done by the template engine.
func (n constellationNode) TitleAttr() string {
	parts := []string{n.ID}
	if n.Title != "" {
		parts = append(parts, n.Title)
	}
	if n.Status != "" {
		parts = append(parts, n.Status)
	}
	if n.Type != "" {
		parts = append(parts, n.Type)
	}
	if n.Score != "" {
		parts = append(parts, "score "+n.Score)
	}
	return strings.Join(parts, " · ")
}

// LabelText returns the visible label rendered next to a node — kept short so
// it reads at the SVG's natural size. The center node prints its title; orbit
// nodes print just the ID (titles are in the hover tooltip).
func (n constellationNode) LabelText() string {
	if n.IsCenter {
		t := n.Title
		if len(t) > 60 {
			t = t[:58] + "…"
		}
		return t
	}
	return n.ID
}
