package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestSortGardenerRows_DefaultOrder verifies the run-centric default sort (epic
// ka-0000004446): running gardeners first (freshest activity first), then
// non-running by most-recently-completed (newest first; no-completed-run rows
// sink), and closed gardeners below all runnable ones regardless of run recency.
func TestSortGardenerRows_DefaultOrder(t *testing.T) {
	base := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	rows := []gardenerRowView{
		// closed but very recently completed — must still sink below runnable rows.
		{ID: "gd-closed", IsClosed: true, LastCompletedAt: base.Add(-1 * time.Minute), createdAt: base},
		// non-running, completed 3h ago.
		{ID: "gd-old-complete", LastCompletedAt: base.Add(-3 * time.Hour), createdAt: base.Add(-3 * time.Hour)},
		// running, active 30m ago (older live run).
		{ID: "gd-run-stale", IsRunning: true, lastActivityAt: base.Add(-30 * time.Minute), createdAt: base.Add(-5 * time.Hour)},
		// non-running, completed 10m ago (most recent completion).
		{ID: "gd-recent-complete", LastCompletedAt: base.Add(-10 * time.Minute), createdAt: base.Add(-2 * time.Hour)},
		// running, active 1m ago (freshest live run) -> should be first.
		{ID: "gd-run-fresh", IsRunning: true, lastActivityAt: base.Add(-1 * time.Minute), createdAt: base.Add(-4 * time.Hour)},
		// non-running with NO completed run -> sinks below completed runnable rows.
		{ID: "gd-never", createdAt: base.Add(-1 * time.Hour)},
	}
	sortGardenerRows(rows)

	gotOrder := make([]string, len(rows))
	for i, r := range rows {
		gotOrder[i] = r.ID
	}
	want := []string{
		"gd-run-fresh",       // running, freshest activity
		"gd-run-stale",       // running, older activity
		"gd-recent-complete", // non-running, most recent completion
		"gd-old-complete",    // non-running, older completion
		"gd-never",           // non-running, no completed run -> sinks
		"gd-closed",          // closed -> below all runnable, despite recent completion
	}
	for i := range want {
		if gotOrder[i] != want[i] {
			t.Fatalf("sort order[%d] = %q, want %q (full: %v)", i, gotOrder[i], want[i], gotOrder)
		}
	}
}

// TestHandleGardeners_NextFireRendered checks an enabled cron-bearing gardener
// surfaces a "fires in ..." chip on the index.
func TestHandleGardeners_NextFireRendered(t *testing.T) {
	f := newFake()
	cap := mkGardenerBead("gd-cap1", "delivery", map[string]any{"name": "delivery", "terminal_goal": "drain"})
	cap.Fields["cron"] = "*/5 * * * *" // every 5 min -> always a near next-fire
	cap.Fields["enabled"] = "true"
	f.triggerScheduleResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{cap}, Total: 1}

	mux := mkServerWithNow(t, f, gardenersNow)
	req := httptest.NewRequest("GET", "/gardeners", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	// The Next-fire column renders a soon-chip with the relative time ("in 5m").
	// The header ("Next fire") carries the label, so the cell itself is just the
	// relative value (ka-0000003582).
	if !strings.Contains(body, "cap-next-soon") || !strings.Contains(body, "in ") {
		t.Errorf("expected a next-fire chip on the cron-bearing gardener row, body=%s", body)
	}
	// Expand button + drawer host must be present per row.
	if !strings.Contains(body, "cap-expand") || !strings.Contains(body, `id="cap-drawer-gd-cap1"`) {
		t.Errorf("expand button / drawer host missing from gardener row")
	}
}

// TestHandleGardenerBeads_WalksTransitiveGraph seeds a small dependency graph and
// asserts the connected-beads view discovers beads out to the requested degree,
// grouped by type, with the gardener bead excluded from the listing.
func TestHandleGardenerBeads_WalksTransitiveGraph(t *testing.T) {
	f := newFake()
	created := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	// One invocation of the gardener.
	it := mkIteration("gd-it1", "gd-cap", "", 1, "closed", created)
	f.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{it}, Total: 1}

	// Graph: it1 --forward--> task1 --forward--> epic1 ; task1 <--reverse-- mr1
	f.deps = map[string][]beadsapi.Dependency{
		"gd-it1":   {{BeadID: "gd-it1", DependsOnID: "gd-task1", Type: "parent-child"}},
		"gd-task1": {{BeadID: "gd-task1", DependsOnID: "gd-epic1", Type: "parent-child"}},
	}
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-task1": {{BeadID: "gd-mr1", DependsOnID: "gd-task1", Type: "mr-for"}},
	}
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-it1":   it,
		"gd-task1": {ID: "gd-task1", Title: "the task", Type: "task", Status: "closed"},
		"gd-epic1": {ID: "gd-epic1", Title: "the epic", Type: "epic", Status: "open"},
		"gd-mr1":   {ID: "gd-mr1", Title: "the MR", Type: "mr", Status: "open"},
	}

	mux := mkServerWithNow(t, f, gardenersNow)
	// degree=2: from it1 (seed,0) -> task1,(1) -> epic1 + mr1 (2).
	req := httptest.NewRequest("GET", "/gardener/gd-cap/beads?degree=2", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, want := range []string{"the task", "the epic", "the MR", "gd-task1", "gd-epic1", "gd-mr1"} {
		if !strings.Contains(body, want) {
			t.Errorf("connected-beads body missing %q", want)
		}
	}
	// The gardener bead itself is the subject, never listed as a connection.
	if strings.Contains(body, `href="/gd-cap"`) {
		t.Errorf("gardener bead should be excluded from the connected listing")
	}
}

// TestHandleGardenerBeads_DenseView verifies ?view=dense renders the info-dense
// status lanes (Slack thread ka-0000008021): the same connected beads bucketed
// by status, with the active-work lanes and the done lane both present.
func TestHandleGardenerBeads_DenseView(t *testing.T) {
	f := newFake()
	created := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	it := mkIteration("gd-it1", "gd-cap", "", 1, "closed", created)
	f.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{it}, Total: 1}

	// it1 -> task1 (in_progress) -> epic1 (open) ; task1 <- mr1 (closed/done)
	f.deps = map[string][]beadsapi.Dependency{
		"gd-it1":   {{BeadID: "gd-it1", DependsOnID: "gd-task1", Type: "parent-child"}},
		"gd-task1": {{BeadID: "gd-task1", DependsOnID: "gd-epic1", Type: "parent-child"}},
	}
	f.revDeps = map[string][]beadsapi.Dependency{
		"gd-task1": {{BeadID: "gd-mr1", DependsOnID: "gd-task1", Type: "mr-for"}},
	}
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-it1":   it,
		"gd-task1": {ID: "gd-task1", Title: "the task", Type: "task", Status: "in_progress"},
		"gd-epic1": {ID: "gd-epic1", Title: "the epic", Type: "epic", Status: "open"},
		"gd-mr1":   {ID: "gd-mr1", Title: "the MR", Type: "mr", Status: "closed"},
	}

	mux := mkServerWithNow(t, f, gardenersNow)
	req := httptest.NewRequest("GET", "/gardener/gd-cap/beads?degree=2&view=dense", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	// Dense container + lane buckets present.
	if !strings.Contains(body, "cap-beads-lanes") {
		t.Errorf("dense view should render the lanes container")
	}
	for _, want := range []string{`data-lane="in_progress"`, `data-lane="open"`, `data-lane="done"`} {
		if !strings.Contains(body, want) {
			t.Errorf("dense view missing lane %q", want)
		}
	}
	// All three connected beads still listed.
	for _, want := range []string{"the task", "the epic", "the MR"} {
		if !strings.Contains(body, want) {
			t.Errorf("dense view body missing %q", want)
		}
	}
	// The active view toggle reflects dense; the grouped constellation is absent.
	if !strings.Contains(body, `cap-beads-view active`) {
		t.Errorf("dense view should mark a view toggle active")
	}
	if strings.Contains(body, "cap-beads-groups") {
		t.Errorf("dense view should not render the grouped lists")
	}
}

// TestHandleGardenerBeads_DefaultViewIsGrouped confirms the dense lanes are
// strictly opt-in: with no ?view= the established grouped render is served.
func TestHandleGardenerBeads_DefaultViewIsGrouped(t *testing.T) {
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
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	if strings.Contains(body, "cap-beads-lanes") {
		t.Errorf("default view must not render the dense lanes")
	}
	if !strings.Contains(body, "cap-beads-groups") {
		t.Errorf("default view should render the grouped lists")
	}
}

// TestHandleGardenerBeads_GroupedListCollapsedByDefault pins the density fix from
// bug ka-0000007518: the by-type grouped LIST must be wrapped in a single
// disclosure that is COLLAPSED by default (no `open` attribute), while the
// constellation star map and the connected-beads summary line stay visible. The
// inline-expanded wall (~477 title rows / 2807 links) pushed the gardener page to
// 33k-39k px and scored CDQR Density 6/10 for three iterations; collapsing it
// restores signal-per-pixel while keeping the full list one click away.
func TestHandleGardenerBeads_GroupedListCollapsedByDefault(t *testing.T) {
	f := newFake()
	created := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)
	it := mkIteration("gd-it1", "gd-cap", "", 1, "closed", created)
	f.liveAgentsResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{it}, Total: 1}
	f.deps = map[string][]beadsapi.Dependency{
		"gd-it1":   {{BeadID: "gd-it1", DependsOnID: "gd-task1", Type: "parent-child"}},
		"gd-task1": {{BeadID: "gd-task1", DependsOnID: "gd-epic1", Type: "parent-child"}},
	}
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-it1":   it,
		"gd-task1": {ID: "gd-task1", Title: "the task", Type: "task", Status: "open"},
		"gd-epic1": {ID: "gd-epic1", Title: "the epic", Type: "epic", Status: "open"},
	}

	mux := mkServerWithNow(t, f, gardenersNow)
	req := httptest.NewRequest("GET", "/gardener/gd-cap/beads?degree=2", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()

	// The grouped list lives behind a single collapsed disclosure.
	discIdx := strings.Index(body, `<details class="cap-beads-disclosure"`)
	if discIdx < 0 {
		t.Fatal("grouped list is not wrapped in a cap-beads-disclosure — the " +
			"~477-row title wall renders inline and dominates the page again " +
			"(bug ka-0000007518)")
	}
	// The disclosure must NOT be open by default: the opening tag runs up to the
	// first '>' and must carry no `open` attribute.
	if end := strings.Index(body[discIdx:], ">"); end >= 0 {
		if openTag := body[discIdx : discIdx+end]; strings.Contains(openTag, " open") {
			t.Errorf("cap-beads-disclosure is open by default (%q) — the grouped "+
				"title wall must be collapsed until the operator reveals it "+
				"(bug ka-0000007518)", openTag)
		}
	}
	// The summary count and the constellation stay visible above the collapsed
	// wall as the at-a-glance signal.
	if !strings.Contains(body, "cap-beads-summary") {
		t.Errorf("connected-beads summary line missing — it must remain visible above the collapsed list")
	}
	if !strings.Contains(body, "cap-constellation") {
		t.Errorf("constellation missing — the star map must remain visible above the collapsed list")
	}
	// The full grouped list is still present in the DOM (revealed on demand), so
	// the record stays complete/accessible.
	if !strings.Contains(body, "cap-beads-groups") {
		t.Errorf("grouped lists absent from DOM — the disclosure must still contain the full record")
	}
	for _, want := range []string{"the task", "the epic"} {
		if !strings.Contains(body, want) {
			t.Errorf("connected-beads body missing %q inside the disclosure", want)
		}
	}
}

func TestNormalizeGardenerBeadsView(t *testing.T) {
	cases := map[string]string{
		"":        gardenerBeadsViewGroups,
		"groups":  gardenerBeadsViewGroups,
		"garbage": gardenerBeadsViewGroups,
		"dense":   gardenerBeadsViewDense,
		" dense ": gardenerBeadsViewDense,
		"DENSE":   gardenerBeadsViewGroups, // case-sensitive: only exact "dense" opts in
	}
	for in, want := range cases {
		if got := normalizeGardenerBeadsView(in); got != want {
			t.Errorf("normalizeGardenerBeadsView(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGardenerBeadLanes_BucketsByStatus verifies the dense-view status bucketing:
// each canonical status lands in its lane, closed/non-canonical statuses go to
// "done", empty/unknown open work falls to "open", and the subject gardener bead
// is dropped. Lanes are always returned in the fixed working-first order.
func TestGardenerBeadLanes_BucketsByStatus(t *testing.T) {
	nodes := []gardenerBeadNodeView{
		{ID: "gd-cap", Status: "in_progress"}, // subject — dropped
		{ID: "gd-a", Status: "in_progress"},
		{ID: "gd-b", Status: "blocked"},
		{ID: "gd-c", Status: "open"},
		{ID: "gd-d", Status: "closed", IsClosed: true},
		{ID: "gd-e", Status: "done", IsClosed: true},
		{ID: "gd-f", Status: "", IsClosed: false}, // unknown-open -> open lane
	}
	lanes := gardenerBeadLanes(nodes, "gd-cap")

	// Fixed order: in_progress, blocked, open, done.
	wantOrder := []string{"in_progress", "blocked", "open", "done"}
	if len(lanes) != len(wantOrder) {
		t.Fatalf("got %d lanes, want %d", len(lanes), len(wantOrder))
	}
	byLane := map[string]gardenerBeadLaneView{}
	for i, l := range lanes {
		if l.Lane != wantOrder[i] {
			t.Errorf("lane[%d] = %q, want %q", i, l.Lane, wantOrder[i])
		}
		byLane[l.Lane] = l
	}
	wantCounts := map[string]int{"in_progress": 1, "blocked": 1, "open": 2, "done": 2}
	for lane, want := range wantCounts {
		if got := byLane[lane].Count; got != want {
			t.Errorf("lane %q count = %d, want %d", lane, got, want)
		}
	}
	// Subject gardener bead never appears in any lane.
	for _, l := range lanes {
		for _, n := range l.Nodes {
			if n.ID == "gd-cap" {
				t.Errorf("subject gardener bead leaked into lane %q", l.Lane)
			}
		}
	}
}

func TestClampGardenerBeadsDegree(t *testing.T) {
	cases := map[string]int{
		"":    gardenerBeadsDefaultDegree,
		"x":   gardenerBeadsDefaultDegree,
		"0":   1,
		"-3":  1,
		"3":   3,
		"999": gardenerBeadsMaxDegree,
	}
	for in, want := range cases {
		if got := clampGardenerBeadsDegree(in); got != want {
			t.Errorf("clampGardenerBeadsDegree(%q) = %d, want %d", in, got, want)
		}
	}
}

// TestWalkGardenerBeadGraph_NodeCapTruncates ensures the hard node cap stops a
// runaway fan-out and flags truncation.
func TestWalkGardenerBeadGraph_RespectsDegree(t *testing.T) {
	f := newFake()
	s := gardenerTestServer(t, f)
	// Chain a -> b -> c -> d (forward only).
	f.deps = map[string][]beadsapi.Dependency{
		"gd-a": {{BeadID: "gd-a", DependsOnID: "gd-b"}},
		"gd-b": {{BeadID: "gd-b", DependsOnID: "gd-c"}},
		"gd-c": {{BeadID: "gd-c", DependsOnID: "gd-d"}},
	}
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-a": {ID: "gd-a", Type: "agent"},
		"gd-b": {ID: "gd-b", Type: "task"},
		"gd-c": {ID: "gd-c", Type: "task"},
		"gd-d": {ID: "gd-d", Type: "epic"},
	}
	// degree=1 from seed a reaches only b.
	nodes, _, _, _ := s.walkGardenerBeadGraph(t.Context(), []string{"gd-a"}, 1)
	ids := map[string]int{}
	for _, n := range nodes {
		ids[n.ID] = n.Degree
	}
	if _, ok := ids["gd-b"]; !ok {
		t.Errorf("degree-1 walk should reach gd-b")
	}
	if _, ok := ids["gd-c"]; ok {
		t.Errorf("degree-1 walk must NOT reach gd-c")
	}
}

// TestWalkGardenerBeadGraph_FollowsMentionEdges proves the walk follows prose
// mention edges, not just deps-table rows (ka-0000005664). Both root causes are
// covered: (1) a mention-only neighbor deepens the walk past the dep-only
// frontier, so ?degree=N keeps growing where deps saturate; (2) the discovered
// edges carry their TRUE provenance origin (dep / text / mention) rather than
// flattening every edge to "dep".
func TestWalkGardenerBeadGraph_FollowsMentionEdges(t *testing.T) {
	f := newFake()
	s := gardenerTestServer(t, f)
	parent := "parent-child"

	// Topology from seed gd-a:
	//   gd-a --dep--> gd-b           (deps-table row)
	//   gd-a --typed mention--> gd-c (relationship_type set)
	//   gd-a --untyped mention--> gd-d
	// Only deps would reach gd-b; the mention edges are what surface gd-c/gd-d.
	f.deps = map[string][]beadsapi.Dependency{
		"gd-a": {{BeadID: "gd-a", DependsOnID: "gd-b", Type: "parent-child"}},
	}
	f.mentions = map[string][]beadsapi.BeadReferenceSummary{
		"gd-a": {
			{ID: "gd-c", SourceField: "description", RelationshipType: &parent},
			{ID: "gd-d", SourceField: "description"}, // untyped
		},
	}
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-a": {ID: "gd-a", Type: "agent"},
		"gd-b": {ID: "gd-b", Type: "task"},
		"gd-c": {ID: "gd-c", Type: "task"},
		"gd-d": {ID: "gd-d", Type: "task"},
	}

	nodes, edges, _, _ := s.walkGardenerBeadGraph(t.Context(), []string{"gd-a"}, 1)
	got := map[string]bool{}
	for _, n := range nodes {
		got[n.ID] = true
	}
	for _, want := range []string{"gd-b", "gd-c", "gd-d"} {
		if !got[want] {
			t.Errorf("walk should reach %q via dep+mention edges; nodes=%v", want, got)
		}
	}

	// Edge origins must NOT all be "dep" — that flattening is the bug.
	origins := map[string]string{} // target -> origin
	for _, e := range edges {
		origins[e.Target] = e.Origin
	}
	if origins["gd-b"] != "dep" {
		t.Errorf("gd-b edge origin = %q, want dep", origins["gd-b"])
	}
	if origins["gd-c"] != "text" {
		t.Errorf("gd-c edge origin = %q, want text (typed mention)", origins["gd-c"])
	}
	if origins["gd-d"] != "mention" {
		t.Errorf("gd-d edge origin = %q, want mention (untyped)", origins["gd-d"])
	}
}

// TestWalkGardenerBeadGraph_HydratesUnderExpiredBudget pins ka-0000006335: the
// load-bearing node hydration must populate type/status even when the inbound
// context budget is already exhausted by the best-effort BFS expansion. Before
// the fix the hydration BatchGetBeads inherited the cancelled context, returned
// an empty map, and every node collapsed to type:"unknown" — the grey "?" disks
// the per-gardener map rendered, with every facet chip stuck on one dead value.
func TestWalkGardenerBeadGraph_HydratesUnderExpiredBudget(t *testing.T) {
	f := newFake()
	f.batchGetBeadsHonorCtx = true // surface a cancelled ctx as an error, like the live client
	s := gardenerTestServer(t, f)
	f.deps = map[string][]beadsapi.Dependency{
		"gd-a": {{BeadID: "gd-a", DependsOnID: "gd-b", Type: "parent-child"}},
	}
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-a": {ID: "gd-a", Type: "agent", Status: "closed"},
		"gd-b": {ID: "gd-b", Type: "task", Status: "open"},
	}

	// Simulate the /maps path: the BFS expansion consumed the whole request
	// budget, so the context handed to the walk is already past its deadline.
	ctx, cancel := context.WithDeadline(context.Background(), s.now().Add(-time.Second))
	defer cancel()

	nodes, _, beads, _ := s.walkGardenerBeadGraph(ctx, []string{"gd-a"}, 1)

	// Hydration must have run on a detached budget and populated the map.
	if beads["gd-b"] == nil {
		t.Fatalf("hydration returned no bead for gd-b under an expired budget (the ka-0000006335 regression)")
	}
	for _, n := range nodes {
		if n.ID != "gd-b" {
			continue
		}
		if n.Type != "task" {
			t.Errorf("gd-b node Type = %q, want task (hydration must survive an expired budget)", n.Type)
		}
		if n.Status != "open" {
			t.Errorf("gd-b node Status = %q, want open", n.Status)
		}
		if n.Type == "unknown" || n.Type == "" {
			t.Errorf("gd-b collapsed to %q — the grey '?' regression", n.Type)
		}
	}
}

// TestWalkGardenerBeadGraph_DepBeatsMention verifies a deps-table row wins over a
// mention to the same directed pair: the edge keeps origin "dep", not "text"/
// "mention" (deps > text > untyped precedence, ka-0000005664).
func TestWalkGardenerBeadGraph_DepBeatsMention(t *testing.T) {
	f := newFake()
	s := gardenerTestServer(t, f)
	parent := "parent-child"
	// gd-a both depends on AND typed-mentions gd-b — the dep row must win.
	f.deps = map[string][]beadsapi.Dependency{
		"gd-a": {{BeadID: "gd-a", DependsOnID: "gd-b", Type: "parent-child"}},
	}
	f.mentions = map[string][]beadsapi.BeadReferenceSummary{
		"gd-a": {{ID: "gd-b", SourceField: "description", RelationshipType: &parent}},
	}
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-a": {ID: "gd-a", Type: "agent"},
		"gd-b": {ID: "gd-b", Type: "task"},
	}
	_, edges, _, _ := s.walkGardenerBeadGraph(t.Context(), []string{"gd-a"}, 1)
	var aToB int
	for _, e := range edges {
		if e.Source == "gd-a" && e.Target == "gd-b" {
			aToB++
			if e.Origin != "dep" {
				t.Errorf("gd-a→gd-b origin = %q, want dep (dep beats mention)", e.Origin)
			}
		}
	}
	if aToB != 1 {
		t.Errorf("expected exactly one gd-a→gd-b edge, got %d", aToB)
	}
}

// TestWalkGardenerBeadGraph_DropsSessionBeads pins ka-0000000532: session beads
// are filtered out of the gardener map (and the edges touching them are pruned),
// while closed AGENT beads — a gardener's own invocations — stay visible.
//
// The gardener's own closed invocation (gd-agent) is passed as a SEED, exactly as
// production does (gardenerIterations scopes the seed set by schedule_id/
// trigger_id). This keeps the "own closed agent stays visible" assertion honest
// after the foreign-agent scope fix (ka-0000005604), which drops only NON-seed
// agent beads.
func TestWalkGardenerBeadGraph_DropsSessionBeads(t *testing.T) {
	f := newFake()
	s := gardenerTestServer(t, f)
	// gd-a (gardener) deps on a session bead (dropped) and a task (kept). gd-agent
	// is the gardener's own invocation, seeded alongside the gardener.
	f.deps = map[string][]beadsapi.Dependency{
		"gd-a": {
			{BeadID: "gd-a", DependsOnID: "gd-sess", Type: "parent-child"},
			{BeadID: "gd-a", DependsOnID: "gd-task", Type: "parent-child"},
		},
	}
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-a":     {ID: "gd-a", Type: "agent", Status: "closed"},
		"gd-agent": {ID: "gd-agent", Type: "agent", Status: "closed"},
		"gd-sess":  {ID: "gd-sess", Type: "session", Status: "closed"},
		"gd-task":  {ID: "gd-task", Type: "task", Status: "open"},
	}
	nodes, edges, _, _ := s.walkGardenerBeadGraph(t.Context(), []string{"gd-a", "gd-agent"}, 1)
	got := map[string]bool{}
	for _, n := range nodes {
		got[n.ID] = true
	}
	if got["gd-sess"] {
		t.Errorf("session bead gd-sess should be dropped from the gardener map; nodes=%v", got)
	}
	if !got["gd-agent"] {
		t.Errorf("closed agent invocation gd-agent should stay visible; nodes=%v", got)
	}
	if !got["gd-task"] {
		t.Errorf("task gd-task should stay visible; nodes=%v", got)
	}
	// No edge may point at the dropped session bead.
	for _, e := range edges {
		if e.Source == "gd-sess" || e.Target == "gd-sess" {
			t.Errorf("edge %s→%s touches dropped session bead gd-sess", e.Source, e.Target)
		}
	}
}

// TestWalkGardenerBeadGraph_ScopesAgentsToOwnLifeThread pins bug ka-0000005604:
// the undirected connected-beads walk reaches ANOTHER gardener's agent beads
// through a shared epic, and those foreign warm-up runs were being attributed to
// this gardener in the connected-agents section. The fix scopes agent attribution
// to the gardener's OWN life-thread (its schedule_id/trigger_id-scoped seeds), so
// only the gardener's own invocation agents appear — while the shared epic and
// other non-agent connected beads still surface transitively, and the edge
// touching the dropped foreign agent is pruned.
func TestWalkGardenerBeadGraph_ScopesAgentsToOwnLifeThread(t *testing.T) {
	f := newFake()
	s := gardenerTestServer(t, f)
	// Topology mirrors the reported case (ka-0000004561 vs ka-0000000242):
	//   gd-cap (this gardener) --parent--> gd-epic (shared epic)
	//   gd-own  (this gardener's own invocation) seeded directly
	//   gd-foreign (ANOTHER gardener's warm-up agent) --parent--> gd-epic
	// The undirected walk reaches gd-foreign via the shared epic in 2 hops; it
	// must NOT be listed as this gardener's connected agent.
	f.deps = map[string][]beadsapi.Dependency{
		"gd-cap":  {{BeadID: "gd-cap", DependsOnID: "gd-epic", Type: "parent-child"}},
		"gd-own":  {{BeadID: "gd-own", DependsOnID: "gd-epic", Type: "parent-child"}},
		"gd-epic": {{BeadID: "gd-foreign", DependsOnID: "gd-epic", Type: "parent-child"}},
	}
	f.beads = map[string]*beadsapi.BeadDetail{
		"gd-cap":     {ID: "gd-cap", Type: "trigger", Status: "open"},
		"gd-own":     {ID: "gd-own", Type: "agent", Status: "closed", Fields: map[string]string{"schedule_id": "gd-cap"}},
		"gd-epic":    {ID: "gd-epic", Type: "epic", Status: "open"},
		"gd-foreign": {ID: "gd-foreign", Type: "agent", Status: "closed", Fields: map[string]string{"schedule_id": "ka-0000000242"}},
	}
	// Seeds = the gardener bead + its OWN scoped invocation (what gardenerIterations
	// returns). gd-foreign is NOT a seed because it belongs to another gardener.
	nodes, edges, _, _ := s.walkGardenerBeadGraph(t.Context(), []string{"gd-cap", "gd-own"}, 2)
	got := map[string]bool{}
	for _, n := range nodes {
		got[n.ID] = true
	}
	if got["gd-foreign"] {
		t.Errorf("foreign gardener's agent gd-foreign must NOT be attributed to this gardener; nodes=%v", got)
	}
	if !got["gd-own"] {
		t.Errorf("gardener's own invocation gd-own should stay visible; nodes=%v", got)
	}
	if !got["gd-epic"] {
		t.Errorf("shared epic gd-epic should still surface transitively; nodes=%v", got)
	}
	// No edge may touch the dropped foreign agent.
	for _, e := range edges {
		if e.Source == "gd-foreign" || e.Target == "gd-foreign" {
			t.Errorf("edge %s→%s touches dropped foreign agent gd-foreign", e.Source, e.Target)
		}
	}

	// The connected-agents section is built from the type-grouped nodes: the
	// "agent" group must contain ONLY the gardener's own invocation, never the
	// foreign warm-up agent.
	groups, _ := groupGardenerBeadNodes(nodes, "gd-cap")
	for _, g := range groups {
		if g.Type != "agent" {
			continue
		}
		for _, n := range g.Nodes {
			if n.ID == "gd-foreign" {
				t.Errorf("agent group leaked foreign agent gd-foreign: %+v", g.Nodes)
			}
		}
	}
}
