package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestBuildGardenerConstellation_RadialLayout checks the builder places the
// gardener at the center and lays connected beads out on rings keyed by
// hop-degree, with one orbit node per connected bead.
func TestBuildGardenerConstellation_RadialLayout(t *testing.T) {
	s := gardenerTestServer(t, newFakeDaemon())
	nodes := []gardenerBeadNodeView{
		{ID: "gd-cap", Type: "trigger", Status: "open", Degree: 0},
		{ID: "gd-it1", Type: "agent", Status: "closed", Degree: 0},
		{ID: "gd-task1", Title: "the task", Type: "task", Status: "in_progress", Degree: 1},
		{ID: "gd-epic1", Title: "the epic", Type: "epic", Status: "open", Degree: 2},
	}
	edges := []gardenerGraphEdge{
		{Source: "gd-it1", Target: "gd-task1", RelType: "parent-child", IsParentChild: true},
		{Source: "gd-task1", Target: "gd-epic1", RelType: "parent-child", IsParentChild: true},
	}
	beads := map[string]*beadsapi.BeadDetail{
		"gd-task1": {ID: "gd-task1", Type: "task", Status: "in_progress"},
		"gd-epic1": {ID: "gd-epic1", Type: "epic", Status: "open"},
	}
	seeds := map[string]struct{}{"gd-it1": {}}

	view := s.buildGardenerConstellation("gd-cap", nodes, edges, beads, seeds, 2)
	if view == nil {
		t.Fatal("expected a non-nil constellation view")
	}
	if view.Center.ID != "gd-cap" {
		t.Errorf("center should be the gardener, got %q", view.Center.ID)
	}
	if !view.Center.IsCenter {
		t.Error("center node should have IsCenter=true")
	}
	cx := float64(gardenerConstellationWidth) / 2
	cy := float64(gardenerConstellationHeight) / 2
	if view.Center.X != cx || view.Center.Y != cy {
		t.Errorf("center should sit at (%g,%g), got (%g,%g)", cx, cy, view.Center.X, view.Center.Y)
	}
	// Three orbit nodes: it1 (deg0 invocation), task1 (deg1), epic1 (deg2).
	if len(view.Outer) != 3 {
		t.Fatalf("expected 3 orbit nodes, got %d", len(view.Outer))
	}
	// RingRadii should have one entry per populated degree: deg 0 (invocation),
	// deg 1 (task), deg 2 (epic).
	if len(view.RingRadii) != 3 {
		t.Errorf("expected 3 ring radii, got %d (%v)", len(view.RingRadii), view.RingRadii)
	}
	// Edges between rendered endpoints should both survive.
	if len(view.Edges) != 2 {
		t.Errorf("expected 2 edges, got %d", len(view.Edges))
	}
	if !view.HasAnything {
		t.Error("HasAnything should be true with orbit nodes present")
	}

	// The deg-1 task should sit closer to the center than the deg-2 epic.
	dist := func(id string) float64 {
		for _, n := range view.Outer {
			if n.ID == id {
				dx := n.X - cx
				dy := n.Y - cy
				return dx*dx + dy*dy
			}
		}
		t.Fatalf("node %q not placed", id)
		return 0
	}
	if dist("gd-task1") >= dist("gd-epic1") {
		t.Error("deg-1 task should orbit closer to the gardener than the deg-2 epic")
	}

	// The invocation seed should get the golden agent styling.
	for _, n := range view.Outer {
		if n.ID == "gd-it1" {
			if n.Role != "invocation" {
				t.Errorf("invocation node role = %q, want invocation", n.Role)
			}
			if n.Color != "#f1c84a" {
				t.Errorf("invocation node should be golden, got %q", n.Color)
			}
		}
	}
}

// TestBuildGardenerConstellation_NilWhenNoConnections returns nil (lists-only
// fallback) when the gardener has no connected beads.
func TestBuildGardenerConstellation_NilWhenNoConnections(t *testing.T) {
	s := gardenerTestServer(t, newFakeDaemon())
	nodes := []gardenerBeadNodeView{{ID: "gd-cap", Type: "trigger", Degree: 0}}
	view := s.buildGardenerConstellation("gd-cap", nodes, nil, nil, nil, 1)
	if view != nil {
		t.Errorf("expected nil view when no connected beads, got %+v", view)
	}
}

// TestBuildGardenerConstellation_MRDiamond flips a connected merge-request into
// an MR-diamond node with a state-tinted overlay.
func TestBuildGardenerConstellation_MRDiamond(t *testing.T) {
	s := gardenerTestServer(t, newFakeDaemon())
	nodes := []gardenerBeadNodeView{
		{ID: "gd-cap", Type: "trigger", Degree: 0},
		{ID: "gd-mr1", Title: "the MR", Type: "merge-request", Status: "open", Degree: 1},
	}
	beads := map[string]*beadsapi.BeadDetail{
		"gd-mr1": {ID: "gd-mr1", Type: "merge-request", Status: "open", Fields: map[string]string{"mr_state": "merged"}},
	}
	view := s.buildGardenerConstellation("gd-cap", nodes, nil, beads, nil, 1)
	if view == nil {
		t.Fatal("expected non-nil view")
	}
	var mr *constellationNode
	for i := range view.Outer {
		if view.Outer[i].ID == "gd-mr1" {
			mr = &view.Outer[i]
		}
	}
	if mr == nil {
		t.Fatal("MR node not placed")
	}
	if !mr.IsMR {
		t.Error("merge-request node should have IsMR=true")
	}
	if mr.MRState != "merged" || mr.MROverlay == "" {
		t.Errorf("MR node should carry merged state + overlay, got state=%q overlay=%q", mr.MRState, mr.MROverlay)
	}
}

// TestBuildGardenerConstellation_CapsNodes keeps only the lowest-degree nodes
// when the connected graph exceeds the SVG node cap.
func TestBuildGardenerConstellation_CapsNodes(t *testing.T) {
	s := gardenerTestServer(t, newFakeDaemon())
	const degree = 3
	budget := mapDrawBudget(degree)
	nodes := []gardenerBeadNodeView{{ID: "gd-cap", Type: "trigger", Degree: 0}}
	// One close node + far more distant nodes than the depth-scaled budget.
	nodes = append(nodes, gardenerBeadNodeView{ID: "gd-close", Type: "task", Degree: 1})
	for i := range budget + 50 {
		nodes = append(nodes, gardenerBeadNodeView{
			ID:     "gd-far-" + string(rune('a'+i%26)) + itoaPad(i),
			Type:   "task",
			Degree: 3,
		})
	}
	view := s.buildGardenerConstellation("gd-cap", nodes, nil, nil, nil, degree)
	if view == nil {
		t.Fatal("expected non-nil view")
	}
	if len(view.Outer) > budget {
		t.Errorf("orbit nodes %d exceed budget %d", len(view.Outer), budget)
	}
	// The single deg-1 node must always survive the cap (lowest degree first).
	found := false
	for _, n := range view.Outer {
		if n.ID == "gd-close" {
			found = true
		}
	}
	if !found {
		t.Error("the closest (deg-1) node should always render within the cap")
	}
}

// TestBuildGardenerConstellation_PrioritizesOpenAtCap verifies that when a single
// ring overflows the SVG cap, open/in_progress beads win render slots over
// closed ones (ka-0000001741): the map exists to surface active work, so a
// truncating ring must not drop live beads to draw closed ones.
func TestBuildGardenerConstellation_PrioritizesOpenAtCap(t *testing.T) {
	s := gardenerTestServer(t, newFakeDaemon())
	nodes := []gardenerBeadNodeView{{ID: "gd-cap", Type: "trigger", Degree: 0}}
	// Fill the cap with closed beads whose IDs sort BEFORE the open ones, so
	// pure ID-order would crowd the open beads out entirely. All on one ring.
	for i := range gardenerConstellationMaxNodes {
		nodes = append(nodes, gardenerBeadNodeView{
			ID:       "gd-aclosed-" + itoaPad(i),
			Type:     "task",
			Status:   "closed",
			IsClosed: true,
			Degree:   1,
		})
	}
	// A handful of open / in_progress beads with later-sorting IDs.
	openIDs := []string{"gd-zopen-0", "gd-zopen-1", "gd-zprog-2"}
	statuses := []string{"open", "open", "in_progress"}
	for i, id := range openIDs {
		nodes = append(nodes, gardenerBeadNodeView{
			ID: id, Type: "task", Status: statuses[i], Degree: 1,
		})
	}
	view := s.buildGardenerConstellation("gd-cap", nodes, nil, nil, nil, 1)
	if view == nil {
		t.Fatal("expected non-nil view")
	}
	if len(view.Outer) > gardenerConstellationMaxNodes {
		t.Fatalf("orbit nodes %d exceed cap %d", len(view.Outer), gardenerConstellationMaxNodes)
	}
	drawn := make(map[string]bool, len(view.Outer))
	for _, n := range view.Outer {
		drawn[n.ID] = true
	}
	for _, id := range openIDs {
		if !drawn[id] {
			t.Errorf("open/in_progress bead %s should win a render slot over closed beads at the cap", id)
		}
	}
}

// TestBuildGardenerConstellation_DepthSaturated flags the depth control as inert
// when the render budget fills on the inner rings before the deeper walked ring
// gets any slots (ka-0000002373). A dense degree-1/2 ring should consume every
// render slot, leaving the degree-3 ring undrawn — DepthSaturated must be true
// and DrawnMaxDegree must report the deepest degree that actually drew.
func TestBuildGardenerConstellation_DepthSaturated(t *testing.T) {
	s := gardenerTestServer(t, newFakeDaemon())
	const degree = 3
	budget := mapDrawBudget(degree)
	nodes := []gardenerBeadNodeView{{ID: "gd-cap", Type: "trigger", Degree: 0}}
	// Pack more than the whole (depth-scaled) render budget onto degree 1 so the
	// budget fills there; then add genuine degree-3 beads that exist in the walk
	// but can never win a slot. Even the larger degree-3 budget saturates here.
	for i := range budget + 20 {
		nodes = append(nodes, gardenerBeadNodeView{
			ID:     "gd-near-" + string(rune('a'+i%26)) + itoaPad(i),
			Type:   "task",
			Degree: 1,
		})
	}
	for i := range 10 {
		nodes = append(nodes, gardenerBeadNodeView{
			ID:     "gd-deep-" + string(rune('a'+i%26)) + itoaPad(i),
			Type:   "task",
			Degree: 3,
		})
	}
	view := s.buildGardenerConstellation("gd-cap", nodes, nil, nil, nil, degree)
	if view == nil {
		t.Fatal("expected non-nil view")
	}
	if view.WalkMaxDegree != 3 {
		t.Errorf("WalkMaxDegree = %d, want 3 (deepest walked ring)", view.WalkMaxDegree)
	}
	if view.DrawnMaxDegree != 1 {
		t.Errorf("DrawnMaxDegree = %d, want 1 (budget filled on the degree-1 ring)", view.DrawnMaxDegree)
	}
	if !view.DepthSaturated {
		t.Error("DepthSaturated should be true: deeper walked beads exist but never drew")
	}
}

// TestMapDrawBudget asserts the depth-scaled draw budget (ka-0000003533): the
// default depth and shallower keep the base budget (so the default /maps/{id}
// view's density is untouched), each deeper step unlocks a fixed step of slots,
// and the budget clamps at the ceiling.
func TestMapDrawBudget(t *testing.T) {
	// At/below the default depth: exactly the base budget.
	for d := 1; d <= gardenerBeadsDefaultDegree; d++ {
		if got := mapDrawBudget(d); got != gardenerConstellationMaxNodes {
			t.Errorf("mapDrawBudget(%d) = %d, want base %d", d, got, gardenerConstellationMaxNodes)
		}
	}
	// One step past the default unlocks exactly one step of extra slots.
	wantOneStep := gardenerConstellationMaxNodes + gardenerConstellationDepthBudgetStep
	if got := mapDrawBudget(gardenerBeadsDefaultDegree + 1); got != wantOneStep {
		t.Errorf("mapDrawBudget(default+1) = %d, want %d", got, wantOneStep)
	}
	// The budget is monotonic in depth and clamps at the ceiling.
	prev := 0
	for d := 1; d <= gardenerBeadsMaxDegree; d++ {
		got := mapDrawBudget(d)
		if got < prev {
			t.Errorf("budget not monotonic: mapDrawBudget(%d)=%d < prev %d", d, got, prev)
		}
		if got > gardenerConstellationMaxNodesCeiling {
			t.Errorf("mapDrawBudget(%d) = %d exceeds ceiling %d", d, got, gardenerConstellationMaxNodesCeiling)
		}
		prev = got
	}
}

// TestBuildGardenerConstellation_DepthGrowsBudget is the core regression for the
// inert depth slider (ka-0000003533): a hub gardener whose degree-1 ring alone
// overfills the BASE budget must draw strictly MORE nodes at a deeper degree
// than at the default, because the deeper budget unlocks slots for the degree-2
// frontier that the default view had to drop. This is exactly the `degree3_grows`
// signal the maps-beauty audit measures.
func TestBuildGardenerConstellation_DepthGrowsBudget(t *testing.T) {
	s := gardenerTestServer(t, newFakeDaemon())
	nodes := []gardenerBeadNodeView{{ID: "gd-cap", Type: "trigger", Degree: 0}}
	// Degree-1 ring overfills the base budget on its own.
	for i := range gardenerConstellationMaxNodes + 30 {
		nodes = append(nodes, gardenerBeadNodeView{
			ID:     "gd-d1-" + string(rune('a'+i%26)) + itoaPad(i),
			Type:   "task",
			Degree: 1,
		})
	}
	// A genuine degree-2 frontier that only deeper budgets can reach.
	for i := range 80 {
		nodes = append(nodes, gardenerBeadNodeView{
			ID:     "gd-d2-" + string(rune('a'+i%26)) + itoaPad(i),
			Type:   "task",
			Degree: 2,
		})
	}
	base := s.buildGardenerConstellation("gd-cap", nodes, nil, nil, nil, gardenerBeadsDefaultDegree)
	deep := s.buildGardenerConstellation("gd-cap", nodes, nil, nil, nil, gardenerBeadsDefaultDegree+1)
	if base == nil || deep == nil {
		t.Fatal("expected non-nil views")
	}
	if len(base.Outer) != gardenerConstellationMaxNodes {
		t.Errorf("default view drew %d nodes, want the base budget %d", len(base.Outer), gardenerConstellationMaxNodes)
	}
	if len(deep.Outer) <= len(base.Outer) {
		t.Errorf("deepening must draw MORE nodes (degree3_grows): base=%d deep=%d", len(base.Outer), len(deep.Outer))
	}
}

// TestBuildGardenerConstellation_NotSaturatedWhenAllDrawn keeps DepthSaturated
// false when every walked degree fits inside the render budget — the depth
// control genuinely reaches the deepest ring, so no inert-slider banner.
func TestBuildGardenerConstellation_NotSaturatedWhenAllDrawn(t *testing.T) {
	s := gardenerTestServer(t, newFakeDaemon())
	nodes := []gardenerBeadNodeView{
		{ID: "gd-cap", Type: "trigger", Degree: 0},
		{ID: "gd-d1", Type: "task", Degree: 1},
		{ID: "gd-d2", Type: "task", Degree: 2},
		{ID: "gd-d3", Type: "task", Degree: 3},
	}
	view := s.buildGardenerConstellation("gd-cap", nodes, nil, nil, nil, 3)
	if view == nil {
		t.Fatal("expected non-nil view")
	}
	if view.DepthSaturated {
		t.Error("DepthSaturated should be false when the whole walk fits under the cap")
	}
	if view.DrawnMaxDegree != 3 || view.WalkMaxDegree != 3 {
		t.Errorf("drawn/walk max degree = %d/%d, want 3/3", view.DrawnMaxDegree, view.WalkMaxDegree)
	}
}

// TestGardenerDenseEdgeFade asserts the de-hairball fade (ka-0000000447) demotes
// only untyped-mention edges and only once the radial is dense — structural
// edges and sparse maps are untouched, and the fade floors out rather than
// dropping edges to zero.
func TestGardenerDenseEdgeFade(t *testing.T) {
	// Sparse: nothing fades, regardless of origin.
	for _, origin := range []string{"mention", "dep", "text", "assignee"} {
		if got := gardenerDenseEdgeFade(origin, gardenerConstellationDenseThreshold); got != 1.0 {
			t.Errorf("at/under threshold origin %q fade = %g, want 1.0", origin, got)
		}
	}
	// Dense: only untyped mentions fade; structural edges stay at full weight.
	for _, origin := range []string{"dep", "text", "assignee", ""} {
		if got := gardenerDenseEdgeFade(origin, gardenerConstellationMaxNodes); got != 1.0 {
			t.Errorf("dense structural origin %q fade = %g, want 1.0", origin, got)
		}
	}
	// A mention fades monotonically as density climbs, floored (never zero).
	atThreshold := gardenerDenseEdgeFade("mention", gardenerConstellationDenseThreshold+1)
	atCap := gardenerDenseEdgeFade("mention", gardenerConstellationMaxNodes)
	if !(atCap < atThreshold && atThreshold < 1.0) {
		t.Errorf("mention fade should drop as it densifies: threshold+1=%g, cap=%g", atThreshold, atCap)
	}
	if atCap < gardenerConstellationDenseMentionFloor-1e-9 {
		t.Errorf("mention fade at cap = %g, must not dip below floor %g", atCap, gardenerConstellationDenseMentionFloor)
	}
	// Past the cap clamps at the floor, not below.
	if got := gardenerDenseEdgeFade("mention", gardenerConstellationMaxNodes+50); got < gardenerConstellationDenseMentionFloor-1e-9 {
		t.Errorf("over-cap mention fade = %g, must clamp at floor %g", got, gardenerConstellationDenseMentionFloor)
	}
}

// TestBuildGardenerConstellation_DenseFadesMentionEdges drives the fade through
// the real builder: a gardener packed past the dense threshold with a mix of dep
// and untyped-mention edges should emit faded mention edges while dep edges keep
// their base opacity (ka-0000000447).
func TestBuildGardenerConstellation_DenseFadesMentionEdges(t *testing.T) {
	s := gardenerTestServer(t, newFakeDaemon())
	nodes := []gardenerBeadNodeView{{ID: "gd-cap", Type: "trigger", Degree: 0}}
	var edges []gardenerGraphEdge
	// Pack the degree-1 ring well past the dense threshold so count saturates,
	// and wire a dep edge and a mention edge from the gardener to two of them.
	for i := range gardenerConstellationDenseThreshold + 30 {
		id := "gd-near-" + string(rune('a'+i%26)) + itoaPad(i)
		nodes = append(nodes, gardenerBeadNodeView{ID: id, Type: "task", Degree: 1})
	}
	depTarget := nodes[1].ID
	mentionTarget := nodes[2].ID
	edges = append(edges,
		gardenerGraphEdge{Source: "gd-cap", Target: depTarget, RelType: "blocks", Origin: "dep"},
		gardenerGraphEdge{Source: "gd-cap", Target: mentionTarget, Origin: "mention"},
	)
	view := s.buildGardenerConstellation("gd-cap", nodes, edges, nil, nil, 1)
	if view == nil {
		t.Fatal("expected non-nil view")
	}
	var depOp, mentionOp float64
	for _, e := range view.Edges {
		switch e.Origin {
		case "dep":
			depOp = e.Opacity
		case "mention":
			mentionOp = e.Opacity
		}
	}
	if depOp == 0 || mentionOp == 0 {
		t.Fatalf("expected both a dep and a mention edge to render, got dep=%g mention=%g", depOp, mentionOp)
	}
	// The dep edge keeps its full base opacity; the mention edge is faded below
	// its own unfaded base, proving the density multiplier hit only the mention.
	_, _, depBaseOp := edgeStyleFor("forward", "dep", "blocks")
	if depOp != depBaseOp {
		t.Errorf("dep edge opacity %g should equal its unfaded base %g (structural edges never fade)", depOp, depBaseOp)
	}
	_, _, mentionBaseOp := edgeStyleFor("forward", "mention", "")
	if mentionOp >= mentionBaseOp {
		t.Errorf("dense mention edge opacity %g should be faded below its base %g", mentionOp, mentionBaseOp)
	}
}

// TestHandleGardenerBeads_RendersConstellationSVG exercises the full handler and
// asserts the connected-beads fragment now includes the constellation SVG.
func TestHandleGardenerBeads_RendersConstellationSVG(t *testing.T) {
	f := newFake()
	created := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	it := mkIteration("gd-it1", "gd-cap", "", 1, "closed", created)
	f.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{it}, Total: 1}
	f.deps = map[string][]beadsapi.Dependency{
		"gd-it1": {{BeadID: "gd-it1", DependsOnID: "gd-task1", Type: "parent-child"}},
	}
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-it1":   it,
		"gd-task1": {ID: "gd-task1", Title: "the task", Type: "task", Status: "open"},
	}
	mux := mkServerWithNow(t, f, gardenersNow)
	req := httptest.NewRequest("GET", "/gardener/gd-cap/beads?degree=2", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"cap-constellation", "constellation-svg", "Gardener constellation"} {
		if !strings.Contains(body, want) {
			t.Errorf("connected-beads body missing constellation marker %q", want)
		}
	}
}

// itoaPad renders i as a fixed-width string so generated test ids stay unique
// without importing strconv solely for the loop.
func itoaPad(i int) string {
	const digits = "0123456789"
	if i == 0 {
		return "000"
	}
	buf := []byte{'0', '0', '0'}
	p := len(buf) - 1
	for i > 0 && p >= 0 {
		buf[p] = digits[i%10]
		i /= 10
		p--
	}
	return string(buf)
}
