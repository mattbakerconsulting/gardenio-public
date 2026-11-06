package main

import (
	"context"
	"math"
	"sort"

	"gardenio/controller/internal/beadsapi"
)

// buildInboxConstellation is the inbox-flavored sibling of buildConstellation
// (predicate G3.d). The inbox renders the intake at the center with its
// immediate descendants fanned around it; concentric category sectors and
// composite-urgency sort are OQ follow-up work.
//
// The two functions share the constellationView shape so the existing
// `bead_constellation` partial can render either one. Where buildConstellation
// also folds in similarity, typed mentions, and untyped mentions, this sibling
// is intentionally narrower:
//
//   - Center bead = the intake (or the synthetic orphan node when caller passes
//     a synthesized BeadDetail).
//   - Forward ring = parent-child deps the bead points at (rare for intake).
//   - Reverse ring = beads that depend on the intake — i.e. the descendant
//     subgraph's first level. This is the inbox's primary signal: "what work
//     hangs off this intake?"
//
// Errors degrade to "panel hidden". Like buildConstellation, this never blocks
// the rest of the page on hydration failures.
func (s *Server) buildInboxConstellation(
	ctx context.Context,
	bead *beadsapi.BeadDetail,
	deps []beadsapi.Dependency, // forward
	revDeps []beadsapi.Dependency, // reverse — children
) *constellationView {
	if bead == nil {
		return nil
	}
	width, height, cx, cy, radii := constellationLayout(1)
	view := &constellationView{
		Width:        width,
		Height:       height,
		BasePath:     s.basePath,
		TreeLevel:    1,
		MaxTreeLevel: constellationMaxTreeLevel,
	}
	cr := 42.0
	view.Center = constellationNode{
		ID:       bead.ID,
		Title:    bead.Title,
		X:        cx,
		Y:        cy,
		Radius:   cr,
		HaloR:    cr * 1.4,
		Status:   bead.Status,
		Type:     bead.Type,
		Role:     "center",
		IsCenter: true,
		Color:    fillForBead(bead),
		Stroke:   strokeForBead(bead),
		Glyph:    glyphForType(bead.Type),
		GlyphY:   cy + cr/3,
		IDLabelY: cy + cr*1.6,
		TopY:     cy - cr*1.2,
		Href:     s.basePath + "/" + bead.ID,
		Priority: bead.Priority,
		Opacity:  1.0,
	}

	r := radii[0]

	// --- Forward (parent-child) ring — usually empty for intakes ---
	forwardIDs := parentTargets(deps)
	if len(forwardIDs) > 0 {
		forwardIDs = capStrings(forwardIDs, constellationMaxForward)
		nodes := s.hydrateRingNodes(ctx, forwardIDs)
		placeRing(nodes, cx, cy, r, math.Pi*1.25, math.Pi*1.75, "forward")
		view.Forward = nodes
		view.Edges = append(view.Edges, ringEdges(view.Center, nodes, "forward")...)
		view.HasForward = len(nodes) > 0
	}

	// --- Reverse ring — direct descendants ---
	// Two storage shapes contribute children to this ring:
	//   1. Canonical: a child stores `type=parent-child` (or `child-of`)
	//      pointing at this bead — surfaces in `revDeps`, child id is `BeadID`.
	//   2. Inverse: this bead stores `type=parent-of` pointing at a child —
	//      surfaces in `deps`, child id is `DependsOnID`. Without case 2
	//      intakes whose parent→child edges were written on the parent
	//      render an empty constellation (ka-0000001707).
	reverseIDs := childTargets(deps, revDeps)
	if len(reverseIDs) > 0 {
		reverseIDs = capStrings(reverseIDs, constellationMaxReverse)
		nodes := s.hydrateRingNodes(ctx, reverseIDs)
		placeRing(nodes, cx, cy, r, math.Pi*0.25, math.Pi*0.75, "reverse")
		view.Reverse = nodes
		view.Edges = append(view.Edges, ringEdges(view.Center, nodes, "reverse")...)
		view.HasReverse = len(nodes) > 0
	}

	view.HasAnything = view.HasForward || view.HasReverse
	return view
}

// isParentChildAlias reports whether t is one of the structural verbs that
// model a child→parent dep stored on the child: the canonical
// `parent-child` and its inverse-direction synonym `child-of`.
func isParentChildAlias(t string) bool {
	return t == "parent-child" || t == "child-of"
}

// parentTargets returns the unique parent ids reachable via this bead's
// outgoing deps — rows where `bead_id == self` and the type names a
// child→parent relationship (`parent-child` / `child-of`).
func parentTargets(deps []beadsapi.Dependency) []string {
	seen := make(map[string]struct{}, len(deps))
	out := make([]string, 0, len(deps))
	for _, d := range deps {
		if !isParentChildAlias(d.Type) {
			continue
		}
		if d.DependsOnID == "" {
			continue
		}
		if _, dup := seen[d.DependsOnID]; dup {
			continue
		}
		seen[d.DependsOnID] = struct{}{}
		out = append(out, d.DependsOnID)
	}
	sort.Strings(out)
	return out
}

// childTargets returns the unique child ids reachable from this bead via
// either dep storage shape (see callsite comment in buildInboxConstellation).
func childTargets(deps, revDeps []beadsapi.Dependency) []string {
	seen := make(map[string]struct{}, len(deps)+len(revDeps))
	out := make([]string, 0)

	// Canonical: child stored a parent-pointing dep — `revDeps` rows where
	// depends_on_id == self, type names child→parent.
	for _, d := range revDeps {
		if !isParentChildAlias(d.Type) {
			continue
		}
		if d.BeadID == "" {
			continue
		}
		if _, dup := seen[d.BeadID]; dup {
			continue
		}
		seen[d.BeadID] = struct{}{}
		out = append(out, d.BeadID)
	}

	// Inverse: this bead stored a child-pointing dep — `deps` rows where
	// bead_id == self, type=parent-of (DependsOnID is the child).
	for _, d := range deps {
		if d.Type != "parent-of" {
			continue
		}
		if d.DependsOnID == "" {
			continue
		}
		if _, dup := seen[d.DependsOnID]; dup {
			continue
		}
		seen[d.DependsOnID] = struct{}{}
		out = append(out, d.DependsOnID)
	}

	sort.Strings(out)
	return out
}

func capStrings(in []string, n int) []string {
	if len(in) <= n {
		return in
	}
	return in[:n]
}

// hydrateRingNodes fetches each id's BeadDetail (best-effort, bounded). Misses
// degrade to placeholder "?" nodes so the ring shape stays stable. Session beads
// are dropped (ka-0000000532) — they are hidden from every constellation surface
// by default. Unhydratable ids (404/timeout, type unknown) are kept as "?" nodes.
func (s *Server) hydrateRingNodes(ctx context.Context, ids []string) []constellationNode {
	out := make([]constellationNode, 0, len(ids))
	for _, id := range ids {
		b, err := s.daemon.GetBead(ctx, id)
		if isSessionBead(b) {
			continue
		}
		node := constellationNode{
			ID:       id,
			Radius:   24,
			Href:     s.basePath + "/" + id,
			Opacity:  1.0,
			GlyphY:   0, // filled in by placeRing post-position
			IDLabelY: 0,
		}
		if err == nil && b != nil {
			node.Title = b.Title
			node.Status = b.Status
			node.Type = b.Type
			node.Color = fillForBead(b)
			node.Stroke = strokeForBead(b)
			node.Glyph = glyphForType(b.Type)
			node.Priority = b.Priority
		} else {
			node.Glyph = "?"
		}
		out = append(out, node)
	}
	return out
}

// placeRing arranges nodes evenly across [angleStart, angleEnd] around the
// center (cx, cy) at radius r. Updates each node's X/Y/GlyphY/IDLabelY and Role.
func placeRing(nodes []constellationNode, cx, cy, r, angleStart, angleEnd float64, role string) {
	n := len(nodes)
	if n == 0 {
		return
	}
	span := angleEnd - angleStart
	step := span
	if n > 1 {
		step = span / float64(n-1)
	}
	for i := range nodes {
		var theta float64
		if n == 1 {
			theta = (angleStart + angleEnd) / 2
		} else {
			theta = angleStart + step*float64(i)
		}
		nodes[i].X = cx + r*math.Cos(theta)
		nodes[i].Y = cy + r*math.Sin(theta)
		nodes[i].GlyphY = nodes[i].Y + nodes[i].Radius/3
		nodes[i].IDLabelY = nodes[i].Y + nodes[i].Radius*1.6
		nodes[i].Role = role
	}
}

// ringEdges emits one edge per ring node from the center outward.
func ringEdges(center constellationNode, ring []constellationNode, role string) []constellationEdge {
	out := make([]constellationEdge, 0, len(ring))
	for _, n := range ring {
		out = append(out, constellationEdge{
			X1:         center.X,
			Y1:         center.Y,
			X2:         n.X,
			Y2:         n.Y,
			Role:       role,
			Origin:     "dep",
			SourceBead: center.ID,
			TargetBead: n.ID,
			Stroke:     "#888",
			Opacity:    0.6,
		})
	}
	return out
}

// fillForBead, strokeForBead, glyphForType: minimal palettes that share the
// status-driven coloring rules buildConstellation uses without leaking its
// internals. The real palettes live alongside buildConstellation; the inbox
// can converge on them when subtask 3 (layout) lands.
func fillForBead(b *beadsapi.BeadDetail) string {
	if b == nil {
		return "#ccc"
	}
	switch b.Status {
	case "in_progress":
		return "#3a86ff"
	case "open":
		return "#fb8500"
	case "closed":
		return "#999"
	case "deferred":
		return "#9b5de5"
	default:
		return "#ccc"
	}
}

func strokeForBead(b *beadsapi.BeadDetail) string {
	if b == nil {
		return "#666"
	}
	return "#222"
}

func glyphForType(t string) string {
	switch t {
	case "epic":
		return "E"
	case "task":
		return "T"
	case "bug":
		return "B"
	case "decision":
		return "D"
	case "agent":
		return "A"
	case "advice":
		return "★"
	case "rich-report":
		return "R"
	case "config":
		return "C"
	default:
		return "•"
	}
}
