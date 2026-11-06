package main

// formula_dag.go — layered DAG layout for a formula's step graph
// (formula-legibility plan ka-0000007376, step S2 ka-0000005735).
//
// A formula's recipe is a DAG of steps ordered by depends_on. The radial
// constellation layout reads poorly for pipelines, so this is a purpose-built
// Sugiyama-lite layered layout: longest-path layering (topological stages →
// columns), a single barycenter pass for row ordering, and precomputed SVG
// coordinates so the template stays dumb — the same division of labor as
// constellation.go.
//
// Steps referenced in depends_on but not defined in the formula's own steps
// array (typically inherited via `extends`) render as dashed "external"
// phantom nodes rather than silently dropping their edges — otherwise an
// extends-based formula like PE-Ticket-to-Completion would show its first
// step as a root when it actually hangs off an inherited step.

import (
	"fmt"
	"sort"
	"strings"
)

const (
	formulaDAGNodeW  = 176.0
	formulaDAGNodeH  = 46.0
	formulaDAGColGap = 60.0
	formulaDAGRowGap = 18.0
	formulaDAGMargin = 14.0
	// formulaDAGVGap is the vertical stride between stacked nodes in the
	// top-to-bottom layout used for a pure linear chain (ka-0000001807).
	formulaDAGVGap = 34.0
	// formulaDAGWrapThreshold is the longest pure linear chain still drawn as a
	// single top-to-bottom column. Beyond it the chain WRAPS into balanced
	// columns (ka-0000001826): a single column of N nodes is 80·N−6 px tall, so
	// a 9-step chain (714px) already overruns the ~671px side-rail and the
	// step-list, driving the whole structure row taller than its content and
	// stranding a large content-left / whitespace-right void in the bottom-right
	// of the desktop recipe page. Wrapping keeps the graph short + wide so it
	// fills the reclaimed band instead. 8 keeps the two existing linear test
	// fixtures (6- and 8-step) single-column — this is purely additive.
	formulaDAGWrapThreshold = 8
	// formulaDAGWrapMaxRows caps how many nodes stack in one wrapped column
	// before a new column opens. Columns are then balanced (see wrapCols) so the
	// last column is never near-empty. 8 rows ≈ 634px, comfortably under the
	// side-rail height, so a wrapped chain reads as a compact block.
	formulaDAGWrapMaxRows = 8
	// formulaDAGMaxNodes caps layout size (steps + external refs). Beyond
	// this the view falls back to the flat step list — a recipe that large
	// is unreadable as a graph anyway.
	formulaDAGMaxNodes = 150
	// formulaDAGLabelMax is the character budget for the in-node title line
	// (~138px of text at 12px ≈ 20 chars); the full title lives in the
	// node's <title> tooltip.
	formulaDAGLabelMax = 20
	// formulaDAGIDMax clips the monospace step-id line (10px mono fits ~24
	// chars in the 138px text column).
	formulaDAGIDMax = 24
)

// DSL-shape glyphs. One glyph per node, by precedence: a gate outranks the
// composition markers, which outrank the plain task cog. ▤ covers both
// `children` and `expand`/`on_complete` (all three are composition — the step
// unfolds into more beads).
const (
	glyphTask     = "⚙"
	glyphGate     = "◆"
	glyphBranch   = "⑃"
	glyphLoop     = "↻"
	glyphChildren = "▤"
	glyphExternal = "⬡"
)

// formulaDAGNode is one laid-out step box. Coordinates are top-left of the
// node rect in the SVG user space; the text anchors are precomputed so the
// template never does arithmetic.
type formulaDAGNode struct {
	Step     formulaStep
	External bool // referenced in depends_on but not defined here (extends)
	Layer    int

	Glyph     string
	KindLabel string // "task" / "gate · decision" / "loop · region ∈ […]" / …
	Label     string // truncated title for the in-node text line
	IDLabel   string // truncated step id for the in-node mono line
	TitleAttr string // full tooltip: id — title, kind, deps, status
	IsCurrent bool   // matches formulaView.CurrentStep

	X, Y                   float64
	GlyphX, GlyphY         float64
	IDTextX, IDTextY       float64
	TitleTextX, TitleTextY float64
}

// formulaDAGEdge is one depends_on edge, drawn as a horizontal cubic bezier
// from the source node's right-center to the target's left-center.
type formulaDAGEdge struct {
	From, To string // step ids
	Path     string // SVG path data, precomputed
}

// formulaDAGLegend is one glyph → meaning entry; only glyphs present in the
// graph are listed.
type formulaDAGLegend struct {
	Glyph string
	Label string
}

// formulaDAG is the template view model for the structure graph.
type formulaDAG struct {
	Nodes  []formulaDAGNode
	Edges  []formulaDAGEdge
	Legend []formulaDAGLegend

	Width  int
	Height int
	Layers int

	// Vertical is true when the graph is a single linear chain (one node per
	// stage) and is therefore laid out top-to-bottom instead of left-to-right.
	// A straight-line recipe drawn horizontally runs wider than the desktop
	// pane and forces needless horizontal scroll (ka-0000001807); stacked
	// vertically it fits at a glance and fills the vertical band the thin
	// horizontal strip otherwise left empty.
	Vertical bool

	// WrapCols is the number of columns a wrapped linear chain is laid out in
	// (>1 only when Vertical and the chain is longer than
	// formulaDAGWrapThreshold). A long chain drawn as one tall column overruns
	// the step-list + side-rail and strands a whitespace void in the bottom-
	// right of the desktop recipe page (ka-0000001826); wrapping it into a few
	// balanced columns keeps the graph short + wide so it fills that band. 0/1
	// mean the single-column vertical layout above.
	WrapCols int

	// ExternalCount is how many phantom nodes were synthesized for
	// depends_on refs not defined in this formula (inherited via extends).
	ExternalCount int
	// Cyclic is true when a depends_on cycle was broken to finish layering.
	// The daemon rejects cyclic formulas at creation, so this is a defensive
	// flag — surfaced as a banner rather than a crash if bad data slips in.
	Cyclic bool
	// HasEdges is false when no step declares depends_on — the steps are
	// unordered (all parallel roots), which the template calls out so a
	// single tall column doesn't read as a mistake.
	HasEdges bool
}

// stepGlyph picks the DSL-shape glyph for a step.
func stepGlyph(s formulaStep) string {
	switch {
	case s.GateType != "":
		return glyphGate
	case s.IsBranch:
		return glyphBranch
	case s.LoopDesc != "":
		return glyphLoop
	case s.ChildCount > 0 || s.Expand != "" || s.OnComplete:
		return glyphChildren
	default:
		return glyphTask
	}
}

// stepKindLabel renders the one-line shape descriptor shown in tooltips.
func stepKindLabel(s formulaStep) string {
	switch {
	case s.GateType != "":
		return "gate · " + s.GateType
	case s.IsBranch:
		if s.BranchDesc != "" {
			return "branch · " + s.BranchDesc
		}
		return "branch"
	case s.LoopDesc != "":
		return "loop · " + s.LoopDesc
	case s.ChildCount > 0:
		return fmt.Sprintf("children × %d", s.ChildCount)
	case s.Expand != "":
		return "expand → " + s.Expand
	case s.OnComplete:
		return "on_complete expansion"
	case s.Type != "" && s.Type != "task":
		return s.Type
	default:
		return "task"
	}
}

// wrapChainLayout returns the column count and rows-per-column for a pure
// linear chain of n nodes wrapped so no column exceeds formulaDAGWrapMaxRows.
// Columns are BALANCED: rowsPerCol = ceil(n/cols), so the last column is at
// most one row shorter than the rest (never a lone straggler beside a full
// column). Used only when n > formulaDAGWrapThreshold — see the WrapCols field.
func wrapChainLayout(n int) (cols, rowsPerCol int) {
	cols = max((n+formulaDAGWrapMaxRows-1)/formulaDAGWrapMaxRows, 1)
	rowsPerCol = (n + cols - 1) / cols
	return cols, rowsPerCol
}

// buildFormulaDAG lays out the step graph. Returns nil when there is nothing
// to draw or the graph is too large for a legible SVG (template falls back to
// the flat list).
func buildFormulaDAG(steps []formulaStep, current *formulaStep) *formulaDAG {
	if len(steps) == 0 {
		return nil
	}

	// Index real steps by id; first definition wins on (invalid) duplicates.
	idx := make(map[string]int, len(steps))
	for i, s := range steps {
		if _, dup := idx[s.ID]; !dup && s.ID != "" {
			idx[s.ID] = i
		}
	}

	// Collect external refs in first-reference order.
	var externals []string
	extSeen := map[string]bool{}
	for _, s := range steps {
		for _, dep := range s.DependsOn {
			if _, ok := idx[dep]; !ok && dep != "" && !extSeen[dep] {
				extSeen[dep] = true
				externals = append(externals, dep)
			}
		}
	}

	total := len(steps) + len(externals)
	if total > formulaDAGMaxNodes {
		return nil
	}

	dag := &formulaDAG{}

	// Longest-path layering via memoized DFS. External refs are roots at
	// layer 0. A cycle (rejected at creation, but defend anyway) is broken by
	// treating the back-edge's contribution as layer 0 and flagging Cyclic.
	layers := make(map[string]int, total)
	for _, ext := range externals {
		layers[ext] = 0
	}
	const visiting = -1
	var layerOf func(id string) int
	layerOf = func(id string) int {
		if l, ok := layers[id]; ok {
			if l == visiting {
				dag.Cyclic = true
				return 0
			}
			return l
		}
		i, ok := idx[id]
		if !ok {
			return 0
		}
		layers[id] = visiting
		max := 0
		for _, dep := range steps[i].DependsOn {
			if dep == "" {
				continue
			}
			if _, isExt := extSeen[dep]; isExt {
				if max < 1 {
					max = 1
				}
				continue
			}
			if l := layerOf(dep) + 1; l > max {
				max = l
			}
		}
		layers[id] = max
		return max
	}
	for _, s := range steps {
		if s.ID != "" {
			layerOf(s.ID)
		}
	}

	// Assemble nodes: real steps in definition order, then externals.
	nodes := make([]formulaDAGNode, 0, total)
	for i, s := range steps {
		if s.ID == "" || idx[s.ID] != i {
			continue // skip blank/duplicate ids
		}
		nodes = append(nodes, formulaDAGNode{
			Step:      s,
			Layer:     layers[s.ID],
			Glyph:     stepGlyph(s),
			KindLabel: stepKindLabel(s),
			IsCurrent: current != nil && current.ID == s.ID,
		})
	}
	for _, ext := range externals {
		nodes = append(nodes, formulaDAGNode{
			Step:      formulaStep{ID: ext, Title: ext},
			External:  true,
			Layer:     0,
			Glyph:     glyphExternal,
			KindLabel: "inherited step (extends) — not defined in this formula",
		})
	}

	// Group by layer, preserving assembly order, then one barycenter pass:
	// order each column L≥1 by the mean row index of its dependencies so
	// edges cross less. Earlier columns are already final when later ones
	// sort, so a single left-to-right sweep suffices for recipe-sized graphs.
	maxLayer := 0
	for _, n := range nodes {
		if n.Layer > maxLayer {
			maxLayer = n.Layer
		}
	}
	byLayer := make([][]int, maxLayer+1) // node indices per layer
	for i, n := range nodes {
		byLayer[n.Layer] = append(byLayer[n.Layer], i)
	}
	rowOf := make(map[string]int, total) // step id → row within its layer
	for row, ni := range byLayer[0] {
		rowOf[nodes[ni].Step.ID] = row
	}
	for l := 1; l <= maxLayer; l++ {
		col := byLayer[l]
		bary := make(map[int]float64, len(col))
		for _, ni := range col {
			sum, cnt := 0.0, 0
			for _, dep := range nodes[ni].Step.DependsOn {
				if r, ok := rowOf[dep]; ok {
					sum += float64(r)
					cnt++
				}
			}
			if cnt > 0 {
				bary[ni] = sum / float64(cnt)
			} else {
				bary[ni] = float64(len(col)) // dep-less strays sink to the bottom
			}
		}
		sort.SliceStable(col, func(a, b int) bool { return bary[col[a]] < bary[col[b]] })
		for row, ni := range col {
			rowOf[nodes[ni].Step.ID] = row
		}
	}

	maxRows := 0
	for _, col := range byLayer {
		if len(col) > maxRows {
			maxRows = len(col)
		}
	}

	// A pure linear chain — every stage holds exactly one node — reads better
	// stacked top-to-bottom: drawn left-to-right it runs wider than the desktop
	// main pane and forces horizontal scroll, and the thin strip leaves a large
	// empty vertical band (ka-0000001807). Stacked, it fits the pane width and
	// fills that band. Multi-node stages keep the horizontal layered layout,
	// where left→right dependency flow is the whole point. Every stage must hold
	// exactly one node — a cycle-broken graph can leave an empty stage, which
	// the layered layout tolerates but a single-per-stage stack cannot.
	dag.Vertical = len(byLayer) > 1
	for _, col := range byLayer {
		if len(col) != 1 {
			dag.Vertical = false
			break
		}
	}

	// The disambiguating shared-prefix strip (audit-style titles) needs every
	// node's title, so compute it once up front.
	titles := make([]string, 0, len(nodes))
	for _, n := range nodes {
		titles = append(titles, n.Step.Title)
	}
	sharedPrefix := stripSharedTitlePrefix(titles)

	if dag.Vertical {
		// A pure linear chain, one node per stage. Short chains stack in a
		// single top-to-bottom column (ka-0000001807). A LONG chain, drawn as
		// one column, runs taller than the step-list + side-rail beside it and
		// strands a whitespace void in the bottom-right of the desktop recipe
		// page — so past formulaDAGWrapThreshold it WRAPS into balanced columns
		// (ka-0000001826), snaking dependency order down the first column then
		// into the top of the next. WrapCols==1 is the original single column.
		cols, rowsPerCol := 1, len(byLayer)
		if len(byLayer) > formulaDAGWrapThreshold {
			cols, rowsPerCol = wrapChainLayout(len(byLayer))
		}
		dag.WrapCols = cols
		for l, col := range byLayer {
			ni := col[0]
			n := &nodes[ni]
			wc := l / rowsPerCol // which wrapped column this node falls in
			wr := l % rowsPerCol // row within that column
			n.X = formulaDAGMargin + float64(wc)*(formulaDAGNodeW+formulaDAGColGap)
			n.Y = formulaDAGMargin + float64(wr)*(formulaDAGNodeH+formulaDAGVGap)
			setNodeText(n, sharedPrefix)
		}
	} else {
		// Columns left→right, each column vertically centered against the
		// tallest one.
		fullH := float64(maxRows)*formulaDAGNodeH + float64(maxRows-1)*formulaDAGRowGap
		for l, col := range byLayer {
			colH := float64(len(col))*formulaDAGNodeH + float64(len(col)-1)*formulaDAGRowGap
			startY := formulaDAGMargin + (fullH-colH)/2
			x := formulaDAGMargin + float64(l)*(formulaDAGNodeW+formulaDAGColGap)
			for row, ni := range col {
				n := &nodes[ni]
				n.X = x
				n.Y = startY + float64(row)*(formulaDAGNodeH+formulaDAGRowGap)
				setNodeText(n, sharedPrefix)
			}
		}
	}

	// Edges. In the horizontal layout each runs dep right-center → step
	// left-center as a horizontal cubic bezier; in the vertical layout it runs
	// dep bottom-center → step top-center as a vertical one.
	pos := make(map[string]*formulaDAGNode, total)
	for i := range nodes {
		pos[nodes[i].Step.ID] = &nodes[i]
	}
	for _, s := range steps {
		to, ok := pos[s.ID]
		if !ok {
			continue
		}
		for _, dep := range s.DependsOn {
			from, ok := pos[dep]
			if !ok || dep == s.ID {
				continue
			}
			var path string
			if dag.Vertical && to.X > from.X {
				// Wrap edge (ka-0000001826): the chain snaked past the bottom of
				// one column into the top of the next, so `from` sits low-left and
				// `to` high-right. Connect right-center → left-center through the
				// inter-column gutter (the same horizontal S-curve the layered
				// layout uses). Going through the gutter keeps the curve inside
				// the viewBox — routing out of `from`'s bottom or into `to`'s top
				// would bow past the ±margin edge, since `to` is always a column's
				// top node at y == margin.
				x1 := from.X + formulaDAGNodeW
				y1 := from.Y + formulaDAGNodeH/2
				x2 := to.X
				y2 := to.Y + formulaDAGNodeH/2
				bend := (x2 - x1) / 2
				if bend < 20 {
					bend = 20
				}
				path = fmt.Sprintf("M %.1f %.1f C %.1f %.1f, %.1f %.1f, %.1f %.1f",
					x1, y1, x1+bend, y1, x2-bend, y2, x2, y2)
			} else if dag.Vertical {
				x1 := from.X + formulaDAGNodeW/2
				y1 := from.Y + formulaDAGNodeH
				x2 := to.X + formulaDAGNodeW/2
				y2 := to.Y
				bend := (y2 - y1) / 2
				if bend < 20 {
					bend = 20
				}
				path = fmt.Sprintf("M %.1f %.1f C %.1f %.1f, %.1f %.1f, %.1f %.1f",
					x1, y1, x1, y1+bend, x2, y2-bend, x2, y2)
			} else {
				x1 := from.X + formulaDAGNodeW
				y1 := from.Y + formulaDAGNodeH/2
				x2 := to.X
				y2 := to.Y + formulaDAGNodeH/2
				bend := (x2 - x1) / 2
				if bend < 20 {
					bend = 20
				}
				path = fmt.Sprintf("M %.1f %.1f C %.1f %.1f, %.1f %.1f, %.1f %.1f",
					x1, y1, x1+bend, y1, x2-bend, y2, x2, y2)
			}
			dag.Edges = append(dag.Edges, formulaDAGEdge{From: dep, To: s.ID, Path: path})
		}
	}

	dag.Nodes = nodes
	dag.Layers = maxLayer + 1
	dag.ExternalCount = len(externals)
	dag.HasEdges = len(dag.Edges) > 0
	if dag.Vertical {
		// Height is the tallest wrapped column (the first columns are full at
		// rowsPerCol rows; the last may be one shorter). A single column
		// (WrapCols==1) holds the whole chain.
		rowsPerCol := len(byLayer)
		if dag.WrapCols > 1 {
			_, rowsPerCol = wrapChainLayout(len(byLayer))
		}
		dag.Width = int(2*formulaDAGMargin + float64(dag.WrapCols)*formulaDAGNodeW + float64(dag.WrapCols-1)*formulaDAGColGap)
		dag.Height = int(2*formulaDAGMargin + float64(rowsPerCol)*formulaDAGNodeH + float64(rowsPerCol-1)*formulaDAGVGap)
	} else {
		fullH := float64(maxRows)*formulaDAGNodeH + float64(maxRows-1)*formulaDAGRowGap
		dag.Width = int(2*formulaDAGMargin + float64(dag.Layers)*formulaDAGNodeW + float64(dag.Layers-1)*formulaDAGColGap)
		dag.Height = int(2*formulaDAGMargin + fullH)
	}
	dag.Legend = formulaDAGLegendFor(nodes)
	return dag
}

// setNodeText fills a node's text-anchor coordinates and clipped label strings
// once its X/Y are placed. Shared by both layouts so the in-node text geometry
// stays identical whichever way the graph flows.
func setNodeText(n *formulaDAGNode, sharedPrefix string) {
	n.GlyphX = n.X + 16
	n.GlyphY = n.Y + 29
	n.IDTextX = n.X + 32
	n.IDTextY = n.Y + 18
	n.TitleTextX = n.X + 32
	n.TitleTextY = n.Y + 34
	n.Label = formulaNodeLabel(n.Step.Title, n.Step.ID, sharedPrefix)
	n.IDLabel = clipEnd(n.Step.ID, formulaDAGIDMax)
	n.TitleAttr = formulaNodeTooltip(*n)
}

// formulaNodeLabel is the in-node title line: the step title clipped at a
// word boundary, falling back to the id when the title is empty. Unbound recipe
// placeholders are normalized to the readable <var> angle form so the graph
// never shows a raw {{var}} (or an ugly mid-placeholder truncation) — a no-op
// once a molecule's titles carry substituted values (ka-0000003537).
//
// sharedPrefix (from stripSharedTitlePrefix) is dropped from the front of the
// title before clipping so audit-style recipes — whose every step is titled
// "<formula> (<iter>): <the real step>" — read as DISTINCT labels instead of a
// column of identical "<formula>…" truncations (ka-0000001807). The full,
// unstripped title still lives in the node's <title> tooltip.
func formulaNodeLabel(title, id, sharedPrefix string) string {
	s := normalizeFormulaPlaceholders(strings.TrimSpace(title))
	if s == "" {
		s = id
	} else if sharedPrefix != "" && strings.HasPrefix(s, sharedPrefix) {
		if stripped := strings.TrimSpace(s[len(sharedPrefix):]); stripped != "" {
			s = stripped
		}
	}
	if len(s) <= formulaDAGLabelMax {
		return s
	}
	return truncate(s, formulaDAGLabelMax) + "…"
}

// stripSharedTitlePrefix returns the longest delimiter-aligned prefix common to
// every (non-empty, normalized) step title. Recipes assembled from an audit
// formula prefix every step title with the formula name — e.g.
// "Maps beauty (<ITERATION>): capture" — so the disambiguating tail truncates
// away and all nodes render the same clipped string (ka-0000001807). Removing
// the shared head restores distinct labels. Returns "" (a no-op) when the
// titles don't share a prefix (ordinary recipes), when fewer than two titles
// carry a label, when the shared head is too short to be a real prefix, or when
// stripping it would blank any label.
func stripSharedTitlePrefix(titles []string) string {
	norm := make([]string, 0, len(titles))
	for _, t := range titles {
		if t = normalizeFormulaPlaceholders(strings.TrimSpace(t)); t != "" {
			norm = append(norm, t)
		}
	}
	if len(norm) < 2 {
		return ""
	}
	// Longest common prefix, rune-aligned so a multibyte glyph never splits.
	lcp := []rune(norm[0])
	for _, t := range norm[1:] {
		rt := []rune(t)
		if len(rt) < len(lcp) {
			lcp = lcp[:len(rt)]
		}
		for i := range lcp {
			if lcp[i] != rt[i] {
				lcp = lcp[:i]
				break
			}
		}
		if len(lcp) == 0 {
			return ""
		}
	}
	// Back up to the last word/delimiter boundary so the cut never lands
	// mid-word (": ", ") ", " - ", " — ", or whitespace).
	cut := -1
	for i, r := range lcp {
		switch r {
		case ':', ')', '-', '—', ' ', '\t':
			cut = i
		}
	}
	// A 1–3 char shared head ("(" or a lone space) is noise, not a prefix.
	if cut+1 < 4 {
		return ""
	}
	prefix := string(lcp[:cut+1])
	for _, t := range norm {
		if strings.TrimSpace(strings.TrimPrefix(t, prefix)) == "" {
			return "" // stripping would blank a label — leave titles intact
		}
	}
	return prefix
}

// clipEnd hard-clips s to max runes with an ellipsis (ids have no word
// boundaries worth preserving).
func clipEnd(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}

// formulaNodeTooltip builds the full-detail hover text for a node.
func formulaNodeTooltip(n formulaDAGNode) string {
	var b strings.Builder
	b.WriteString(n.Step.ID)
	if n.Step.Title != "" && n.Step.Title != n.Step.ID {
		b.WriteString(" — ")
		b.WriteString(normalizeFormulaPlaceholders(n.Step.Title))
	}
	b.WriteString("\n")
	b.WriteString(n.KindLabel)
	if len(n.Step.DependsOn) > 0 {
		b.WriteString("\nafter: ")
		b.WriteString(strings.Join(n.Step.DependsOn, ", "))
	}
	if n.Step.WaitsFor != "" {
		b.WriteString("\nwaits_for: ")
		b.WriteString(n.Step.WaitsFor)
	}
	if n.Step.Status != "" {
		b.WriteString("\nstatus: ")
		b.WriteString(n.Step.Status)
	}
	return b.String()
}

// formulaDAGLegendFor lists only the glyphs that actually appear.
func formulaDAGLegendFor(nodes []formulaDAGNode) []formulaDAGLegend {
	defs := []formulaDAGLegend{
		{glyphTask, "task"},
		{glyphGate, "gate (decision / sync / manual)"},
		{glyphBranch, "branch"},
		{glyphLoop, "loop"},
		{glyphChildren, "children / expand"},
		{glyphExternal, "inherited via extends"},
	}
	present := map[string]bool{}
	for _, n := range nodes {
		present[n.Glyph] = true
	}
	out := make([]formulaDAGLegend, 0, len(defs))
	for _, d := range defs {
		if present[d.Glyph] {
			out = append(out, d)
		}
	}
	return out
}
