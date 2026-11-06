package main

// Gardener transitive-beads view (thread request 2026-06-02, task ka-0000007414).
//
// GET /gardener/{id}/beads?degree=N
//
// "It might be nice if we could know all beads associated with a gardener — for
// instance, transitively to N degrees — and if we could see that."
//
// A gardener's most direct footprint is the set of agent iterations it spawns.
// But each iteration touches a wider graph: the task it worked, the epic that
// parents the task, MRs, reports, decisions, child beads it filed, and so on —
// reachable by walking dependency edges. This endpoint does a breadth-first
// walk of the dependency graph (BOTH forward `depends_on` and reverse
// `dependents` edges, undirected) starting from the gardener's invocations, out
// to `degree` hops, and renders every reachable bead grouped by type with its
// hop-distance.
//
// Seeds = the gardener bead itself (degree 0) + its invocation agent beads
// (also degree 0, they ARE the gardener's direct work). The walk then expands
// degree by degree using the batched dependency endpoint so a wide graph is a
// handful of round-trips, not one-per-node.
//
// This is rendered HTMX-style into the gardener detail page (a "Connected beads"
// section) AND is linkable standalone. Degree is clamped to a sane ceiling so a
// pathological graph cannot fan out unbounded.

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gardenio/controller/internal/beadsapi"
)

const (
	// gardenerBeadsDefaultDegree is the default transitive depth when no ?degree=
	// is supplied. One hop past the invocations captures the tasks/epics/MRs the
	// runs touched — the most useful "what did this gardener affect" view.
	gardenerBeadsDefaultDegree = 2
	// gardenerBeadsMaxDegree caps the walk. The graph fans out fast; beyond a few
	// hops "associated with the gardener" stops being meaningful and the fetch
	// cost climbs. Operators can dial up to this ceiling.
	gardenerBeadsMaxDegree = 5
	// gardenerBeadsMaxNodes is a hard backstop on total discovered beads so a
	// pathological hub bead (e.g. a mega-epic everything points at) cannot blow
	// up the page or the daemon. When hit, the walk stops and the view flags it.
	gardenerBeadsMaxNodes = 600
)

// gardenerBeadsHydrateBudget reserves a floor of wall-clock for the FINAL node
// hydration (BatchGetBeads of every discovered id), so the best-effort BFS
// expansion can never starve it (ka-0000006335). The expansion phase — and
// especially the per-bead mention fan-out — can, on a wide hop-2 frontier
// (hundreds of nodes), legitimately consume an entire tight request budget
// (the /maps page caps the whole walk at mapsHydrateTime). When that happened,
// the hydration call inherited an already-expired context and returned an empty
// map, collapsing EVERY node to type:"unknown"/status:"" — the grey "?" disks
// the per-gardener constellation rendered, with every facet chip stuck on a
// single dead value. Hydration is load-bearing (its absence makes the whole map
// illegible); the mention walk is decorative (it degrades to dep-only edges).
// So when the walk leaves less than this floor on the inbound deadline, run
// hydration on a detached context with a fresh budget instead.
const gardenerBeadsHydrateBudget = 1500 * time.Millisecond

// gardenerBeadNodeView is one discovered bead in the transitive graph.
type gardenerBeadNodeView struct {
	ID       string
	Title    string
	Type     string
	Status   string
	Degree   int    // hop distance from a seed (0 = gardener / its invocations)
	Href     string // -> bead page
	IsClosed bool
}

// gardenerGraphEdge is one discovered edge in the transitive walk, retained so
// the constellation renderer can draw the actual topology rather than
// re-deriving it. Source depends on / parents / mentions Target (forward
// direction). IsParentChild flags parent-child edges so the layout can prefer
// them when placing a node under its discovery parent.
//
// Origin records the edge's provenance so the renderer can style it to match
// the legend (ka-0000005664) rather than flattening every edge to "dep":
//
//	"dep"     — explicit deps-table row (RelType carries the verb).
//	"text"    — typed mention in prose (relationship_type set; RelType = verb).
//	"mention" — untyped mention in prose (relationship_type NULL).
//
// A dep-table row always beats a mention to the same directed (Source,Target)
// pair, and a typed mention beats an untyped one — the first edge added for a
// directed pair wins, and the walk adds dep edges before mention edges within
// each hop (see walkGardenerBeadGraph.addEdge).
type gardenerGraphEdge struct {
	Source        string // BeadID — depends on / child-of / mentions Target
	Target        string // DependsOnID — the thing Source points at
	RelType       string // relationship_type / typed-mention verb ("parent-child", "blocks", …)
	Origin        string // "dep" | "text" | "mention"
	IsParentChild bool
}

// gardenerBeadGroupView buckets discovered beads by type for a scannable render.
type gardenerBeadGroupView struct {
	Type  string
	Count int
	Nodes []gardenerBeadNodeView
}

// handleGardenerBeads renders the transitive "connected beads" view for one
// gardener. ?degree=N (clamped to [1, gardenerBeadsMaxDegree]) sets the walk
// depth; default gardenerBeadsDefaultDegree.
//
// ?view= selects the rendering of the SAME discovered node set (Slack thread
// ka-0000008021, Matt Baker: "show the closed beads and stuff underneath the
// gardener in a really info dense way so we can see what it's working on and
// what its done … that can be an optional thing to display"):
//
//	"groups" (default) — the established by-type grouped lists + constellation.
//	"dense"            — info-dense, one-line-per-bead lanes bucketed by STATUS
//	                     (In progress · Blocked · Open · Done), mirroring the
//	                     feed's lanes mode so "what it's working on / what it's
//	                     done" reads at a glance. Opt-in; never the default.
//
// Both views walk the graph once and toggle in place via HTMX, so flipping the
// view costs no extra daemon work beyond the re-render.
func (s *Server) handleGardenerBeads(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		http.NotFound(w, r)
		return
	}
	degree := clampGardenerBeadsDegree(r.URL.Query().Get("degree"))
	view := normalizeGardenerBeadsView(r.URL.Query().Get("view"))
	ctx := r.Context()

	// Seeds: the gardener's invocations (its direct work). We fold the gardener
	// bead id in too so its own direct dependencies are reachable.
	iters, _, err := s.gardenerIterations(ctx, id, s.now())
	if err != nil {
		s.logger.Error("gardener beads: iterations", "id", id, "error", err)
		s.renderPartial(w, r, "templates/partials/gardener_beads.html", "gardener_beads",
			map[string]any{"GardenerID": id, "Error": "Could not load this gardener's connected beads right now."})
		return
	}

	seeds := make([]string, 0, len(iters)+1)
	seeds = append(seeds, id)
	for _, it := range iters {
		seeds = append(seeds, it.ID)
	}

	nodes, edges, beads, truncated := s.walkGardenerBeadGraph(ctx, seeds, degree)

	// Group by type, drop the seed gardener bead from the listing (it is the
	// subject, not a "connected" bead), and sort groups by size then name.
	groups, total := groupGardenerBeadNodes(nodes, id)

	// Status lanes for the dense view (built from the same node set; cheap, so
	// always available regardless of the active view).
	lanes := gardenerBeadLanes(nodes, id)

	// Build the constellation: gardener at center, invocations + connected beads
	// fanned out on rings by hop-degree. Decorative — a nil view just hides the
	// SVG and leaves the grouped lists below as the load-bearing render.
	seedSet := make(map[string]struct{}, len(iters))
	for _, it := range iters {
		seedSet[it.ID] = struct{}{}
	}
	constellation := s.buildGardenerConstellation(id, nodes, edges, beads, seedSet, degree)

	data := map[string]any{
		"GardenerID":    id,
		"GardenerHref":  s.basePath + "/gardener/" + id,
		"Degree":        degree,
		"MaxDegree":     gardenerBeadsMaxDegree,
		"View":          view,
		"DenseView":     view == gardenerBeadsViewDense,
		"Groups":        groups,
		"Lanes":         lanes,
		"Total":         total,
		"Invocations":   len(iters),
		"Truncated":     truncated,
		"MaxNodes":      gardenerBeadsMaxNodes,
		"DegreeURLs":    s.gardenerBeadsDegreeURLs(id, degree, view),
		"ViewURLs":      s.gardenerBeadsViewURLs(id, degree, view),
		"Constellation": constellation,
	}
	s.renderPartial(w, r, "templates/partials/gardener_beads.html", "gardener_beads", data)
}

// Connected-beads view modes. "groups" is the default by-type render; "dense"
// is the opt-in info-dense status-lane render (Slack thread ka-0000008021).
const (
	gardenerBeadsViewGroups = "groups"
	gardenerBeadsViewDense  = "dense"
)

// normalizeGardenerBeadsView clamps ?view= to a known mode, defaulting to the
// established by-type grouped render so the dense lanes stay strictly opt-in.
func normalizeGardenerBeadsView(v string) string {
	if strings.TrimSpace(v) == gardenerBeadsViewDense {
		return gardenerBeadsViewDense
	}
	return gardenerBeadsViewGroups
}

// degreeURL is one degree-selector pill in the connected-beads header.
type degreeURL struct {
	Degree int
	URL    string
	Active bool
}

func (s *Server) gardenerBeadsDegreeURLs(id string, current int, view string) []degreeURL {
	out := make([]degreeURL, 0, gardenerBeadsMaxDegree)
	for d := 1; d <= gardenerBeadsMaxDegree; d++ {
		out = append(out, degreeURL{
			Degree: d,
			URL:    s.gardenerBeadsURL(id, d, view),
			Active: d == current,
		})
	}
	return out
}

// gardenerBeadsViewView is one view-toggle pill (Grouped vs Dense) in the
// connected-beads header. Both preserve the active degree so flipping the view
// does not silently reset the walk depth.
type gardenerBeadsViewView struct {
	View   string
	Label  string
	URL    string
	Active bool
}

func (s *Server) gardenerBeadsViewURLs(id string, degree int, current string) []gardenerBeadsViewView {
	specs := []struct{ view, label string }{
		{gardenerBeadsViewGroups, "Grouped"},
		{gardenerBeadsViewDense, "Dense"},
	}
	out := make([]gardenerBeadsViewView, 0, len(specs))
	for _, sp := range specs {
		out = append(out, gardenerBeadsViewView{
			View:   sp.view,
			Label:  sp.label,
			URL:    s.gardenerBeadsURL(id, degree, sp.view),
			Active: sp.view == current,
		})
	}
	return out
}

// gardenerBeadsURL builds the /gardener/{id}/beads fragment URL carrying both the
// degree and (when non-default) the view, so HTMX re-requests preserve both
// dimensions. The default "groups" view is omitted to keep canonical URLs clean.
func (s *Server) gardenerBeadsURL(id string, degree int, view string) string {
	u := s.basePath + "/gardener/" + id + "/beads?degree=" + strconv.Itoa(degree)
	if view == gardenerBeadsViewDense {
		u += "&view=" + gardenerBeadsViewDense
	}
	return u
}

// walkGardenerBeadGraph does a breadth-first, UNDIRECTED walk of the dependency
// graph from the seed ids out to `degree` hops, returning every discovered bead
// as a node (with its minimum hop-distance) plus a truncated flag when the node
// cap was hit. "Undirected" = at each frontier we expand BOTH forward
// (depends_on) and reverse (dependents) edges, since a gardener's footprint
// reaches both the things its runs depend on (tasks, epics) and the things that
// depend on its runs (MRs, reports, child beads).
//
// It also returns the discovered edges and the hydrated BeadDetail map so the
// constellation renderer can draw the real topology (and reuse the per-bead
// type/status/MR-state) without a second walk.
func (s *Server) walkGardenerBeadGraph(ctx context.Context, seeds []string, degree int) (nodes []gardenerBeadNodeView, edges []gardenerGraphEdge, beads map[string]*beadsapi.BeadDetail, truncated bool) {
	// distByID records the minimum hop-distance at which each id was first seen.
	distByID := make(map[string]int, len(seeds))
	frontier := make([]string, 0, len(seeds))
	// seedSet is the gardener's OWN life-thread: the gardener bead id plus its
	// schedule_id/trigger_id-scoped invocation agents (gardenerIterations is the
	// authority that scopes them). It pins which agent beads legitimately belong
	// to THIS gardener so the node build below can reject FOREIGN agents the
	// undirected walk reaches through a shared epic/label — another gardener's
	// warm-up runs were being attributed to this gardener (bug ka-0000005604).
	seedSet := make(map[string]struct{}, len(seeds))
	for _, sid := range seeds {
		if sid == "" {
			continue
		}
		seedSet[sid] = struct{}{}
		if _, ok := distByID[sid]; !ok {
			distByID[sid] = 0
			frontier = append(frontier, sid)
		}
	}

	// edgeSeen dedups (source,target) pairs — a forward edge from one frontier
	// and its mirror reverse edge from the other describe the same connection.
	// addEdge is called dep-edges-first within each hop so a dep row wins over a
	// mention to the same directed pair (deps > text > untyped per the
	// constellation provenance contract).
	edgeSeen := make(map[string]struct{})
	addEdge := func(source, target, relType, origin string) {
		source = strings.TrimSpace(source)
		target = strings.TrimSpace(target)
		if source == "" || target == "" {
			return
		}
		key := source + "\x00" + target
		if _, ok := edgeSeen[key]; ok {
			return
		}
		edgeSeen[key] = struct{}{}
		edges = append(edges, gardenerGraphEdge{
			Source:        source,
			Target:        target,
			RelType:       relType,
			Origin:        origin,
			IsParentChild: relType == "parent-child",
		})
	}

	for hop := 1; hop <= degree && len(frontier) > 0; hop++ {
		// Expand the current frontier in both directions with two batched calls.
		fwd, ferr := s.daemon.BatchGetDependencies(ctx, frontier, "forward")
		if ferr != nil {
			s.logger.Error("gardener beads: batch forward deps", "error", ferr)
		}
		rev, rerr := s.daemon.BatchGetDependencies(ctx, frontier, "reverse")
		if rerr != nil {
			s.logger.Error("gardener beads: batch reverse deps", "error", rerr)
		}

		next := make([]string, 0)
		consider := func(neighborID string) {
			neighborID = strings.TrimSpace(neighborID)
			if neighborID == "" {
				return
			}
			if _, seen := distByID[neighborID]; seen {
				return
			}
			if len(distByID) >= gardenerBeadsMaxNodes {
				truncated = true
				return
			}
			distByID[neighborID] = hop
			next = append(next, neighborID)
		}
		// Dep-table edges first so a real dep beats a mention to the same pair.
		for _, dl := range fwd {
			for _, d := range dl {
				consider(d.DependsOnID)
				addEdge(d.BeadID, d.DependsOnID, d.Type, "dep")
			}
		}
		for _, dl := range rev {
			for _, d := range dl {
				consider(d.BeadID)
				addEdge(d.BeadID, d.DependsOnID, d.Type, "dep")
			}
		}

		// Mention edges (ka-0000005664): a gardener's footprint is wired together
		// as much by prose references (typed `parent(ka-X)` / `blocks: ka-X` and
		// untyped ka-id drops) as by deps-table rows. Following them both (a)
		// genuinely deepens the walk so ?degree=3 grows past the dep-only
		// saturation, and (b) yields real typed/untyped provenance so the edges
		// stop collapsing to a single "dep" class. Fanned out under a bounded
		// semaphore — the mention endpoints are per-bead, so a wide frontier
		// would otherwise be a long RTT chain.
		for _, m := range s.walkMentionEdges(ctx, frontier) {
			if m.forward {
				consider(m.target)
			} else {
				consider(m.source)
			}
			addEdge(m.source, m.target, m.relType, m.origin)
		}
		if truncated {
			break
		}
		frontier = next
	}

	// Resolve titles/types/status for every discovered id in one batch.
	ids := make([]string, 0, len(distByID))
	for bid := range distByID {
		ids = append(ids, bid)
	}
	// Hydration is load-bearing: without it every node renders as a grey "unknown"
	// disk and every facet chip collapses to a single dead value (ka-0000006335).
	// The best-effort BFS expansion above can exhaust a tight request budget (the
	// /maps page caps the whole walk at mapsHydrateTime), which would leave this
	// call with an already-cancelled context and return nothing. Guarantee a
	// hydration floor: only when the inbound deadline is set AND leaves less than
	// gardenerBeadsHydrateBudget, detach from it and hydrate on a fresh budget so
	// the node facets are populated even when the walk ran long. A context with no
	// deadline (the gardener-page caller) is left untouched — no new cap imposed.
	hydrateCtx := ctx
	hydrateCancel := func() {}
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < gardenerBeadsHydrateBudget {
		hydrateCtx, hydrateCancel = context.WithTimeout(context.WithoutCancel(ctx), gardenerBeadsHydrateBudget)
	}
	beads, err := s.daemon.BatchGetBeads(hydrateCtx, ids)
	hydrateCancel()
	if err != nil {
		s.logger.Error("gardener beads: batch get beads", "error", err)
		beads = map[string]*beadsapi.BeadDetail{}
	}

	// Session beads are hidden from every constellation surface by default
	// (ka-0000000532) — they are pure machine bookkeeping that adds noise to the
	// gardener map. Drop them from both the node set and the distance index so the
	// edge-prune below also strips any edge touching a session bead. Closed agent
	// beads are KEPT here: a gardener's invocations (its map seeds) are agent
	// beads, so dropping closed agents would erase the gardener's own history.
	//
	// Foreign-agent scope (bug ka-0000005604): the walk is UNDIRECTED, so an
	// agent bead belonging to a DIFFERENT gardener — e.g. another gardener's
	// warm-up runs that share this gardener's epic/label — is reachable in one or
	// two hops and was being listed in the connected-agents section as if it were
	// THIS gardener's own run. A gardener's legitimate agents are exactly its
	// life-thread seeds (schedule_id/trigger_id-scoped by gardenerIterations). So
	// drop any agent-type node that is NOT a seed: connected-agent attribution is
	// scoped to the gardener's own life-thread, while non-agent connected beads
	// (the tasks/epics/MRs the runs touched) still surface transitively.
	rendered := make(map[string]struct{}, len(distByID))
	nodes = make([]gardenerBeadNodeView, 0, len(distByID))
	for bid, dist := range distByID {
		if isSessionBead(beads[bid]) {
			continue
		}
		if _, isSeed := seedSet[bid]; !isSeed && isAgentBead(beads[bid]) {
			continue
		}
		rendered[bid] = struct{}{}
		n := gardenerBeadNodeView{
			ID:     bid,
			Degree: dist,
			Href:   s.basePath + "/" + bid,
			Type:   "unknown",
		}
		if b := beads[bid]; b != nil {
			n.Title = b.Title
			if b.Type != "" {
				n.Type = b.Type
			}
			n.Status = b.Status
			n.IsClosed = statusOpenSortKey(b.Status) != 0
		}
		nodes = append(nodes, n)
	}
	// Drop edges that point at a node beyond the walk frontier (e.g. a frontier
	// bead's forward dep we never expanded into) or at a filtered-out session
	// bead, so every edge endpoint is a real, rendered node in the graph.
	kept := edges[:0]
	for _, e := range edges {
		if _, ok := rendered[e.Source]; !ok {
			continue
		}
		if _, ok := rendered[e.Target]; !ok {
			continue
		}
		kept = append(kept, e)
	}
	edges = kept
	return nodes, edges, beads, truncated
}

// mentionEdge is one prose-reference edge discovered while expanding a frontier.
// forward=true means the frontier bead mentions target (the frontier IS source);
// forward=false means source mentions the frontier bead (the frontier IS target).
type gardenerMentionEdge struct {
	source  string
	target  string
	relType string // typed-mention verb; empty for untyped
	origin  string // "text" (typed) | "mention" (untyped)
	forward bool
}

// gardenerMentionFanout bounds the concurrent per-bead mention lookups so a wide
// frontier doesn't turn into a long serial RTT chain (mirrors the inbox orphan
// rail's bounded fan-out, ka-0000005664).
const gardenerMentionFanout = 16

// gardenerMentionLimit caps the references pulled per bead per direction. A bead
// with thousands of mentions (a mega-epic everyone references) would otherwise
// swamp the walk; the node cap is the ultimate backstop, but this keeps any one
// hub from dominating a single hop.
const gardenerMentionLimit = 50

// walkMentionEdges fetches the typed + untyped prose references emitted by and
// pointing at every bead in `frontier`, fanned out under a bounded semaphore,
// and returns them as mentionEdges. Forward (GetMentions) = references this bead
// emits; reverse (GetMentionedBy) = beads that reference this one. Typed
// mentions (relationship_type set) carry origin "text"; untyped carry "mention".
// A nil daemon or a per-bead fetch error degrades to "no mention edges for that
// bead" — the dep-table spine still renders.
func (s *Server) walkMentionEdges(ctx context.Context, frontier []string) []gardenerMentionEdge {
	if s.daemon == nil || len(frontier) == 0 {
		return nil
	}
	type result struct {
		fwd []beadsapi.BeadReferenceSummary
		rev []beadsapi.BeadReferenceSummary
	}
	results := make([]result, len(frontier))
	sem := make(chan struct{}, gardenerMentionFanout)
	var wg sync.WaitGroup
	for i, id := range frontier {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, id string) {
			defer wg.Done()
			defer func() { <-sem }()
			if fwd, err := s.daemon.GetMentions(ctx, id, gardenerMentionLimit); err == nil {
				results[i].fwd = fwd
			}
			if rev, err := s.daemon.GetMentionedBy(ctx, id, gardenerMentionLimit); err == nil {
				results[i].rev = rev
			}
		}(i, id)
	}
	wg.Wait()

	out := make([]gardenerMentionEdge, 0)
	for i, id := range frontier {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		for _, r := range results[i].fwd {
			tgt := strings.TrimSpace(r.ID)
			if tgt == "" || tgt == id {
				continue
			}
			out = append(out, gardenerMentionEdge{source: id, target: tgt, relType: mentionRelType(r), origin: mentionOrigin(r), forward: true})
		}
		for _, r := range results[i].rev {
			src := strings.TrimSpace(r.ID)
			if src == "" || src == id {
				continue
			}
			out = append(out, gardenerMentionEdge{source: src, target: id, relType: mentionRelType(r), origin: mentionOrigin(r), forward: false})
		}
	}
	return out
}

// mentionOrigin maps a reference to its constellation edge origin: a typed
// mention (relationship_type set) is "text"; an untyped one is "mention".
func mentionOrigin(r beadsapi.BeadReferenceSummary) string {
	if r.RelationshipType != nil && strings.TrimSpace(*r.RelationshipType) != "" {
		return "text"
	}
	return "mention"
}

// mentionRelType returns the typed-mention verb, or "" for an untyped mention.
func mentionRelType(r beadsapi.BeadReferenceSummary) string {
	if r.RelationshipType == nil {
		return ""
	}
	return strings.TrimSpace(*r.RelationshipType)
}

// groupGardenerBeadNodes buckets nodes by type (excluding the gardener bead
// itself, which is the subject), returning groups sorted by size desc then type
// name, with each group's nodes sorted by degree then ID. total is the count of
// listed (non-gardener) beads.
func groupGardenerBeadNodes(nodes []gardenerBeadNodeView, gardenerID string) (groups []gardenerBeadGroupView, total int) {
	byType := make(map[string][]gardenerBeadNodeView)
	for _, n := range nodes {
		if n.ID == gardenerID {
			continue // the subject, not a connection
		}
		byType[n.Type] = append(byType[n.Type], n)
		total++
	}
	groups = make([]gardenerBeadGroupView, 0, len(byType))
	for typ, ns := range byType {
		sort.SliceStable(ns, func(i, j int) bool {
			if ns[i].Degree != ns[j].Degree {
				return ns[i].Degree < ns[j].Degree
			}
			return ns[i].ID < ns[j].ID
		})
		groups = append(groups, gardenerBeadGroupView{Type: typ, Count: len(ns), Nodes: ns})
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if groups[i].Count != groups[j].Count {
			return groups[i].Count > groups[j].Count
		}
		return groups[i].Type < groups[j].Type
	})
	return groups, total
}

// gardenerBeadLaneView is one status lane in the dense connected-beads view. The
// four lanes mirror the feed's lanes mode (feed.go): in_progress · blocked ·
// open · done, ordered so "what it's working on" reads left-to-right before
// "what it's done". Lane keys match the feed's data-lane CSS so the status-dot
// colours stay consistent across surfaces.
type gardenerBeadLaneView struct {
	Lane  string // status bucket key: "in_progress" | "blocked" | "open" | "done"
	Label string
	Count int
	Nodes []gardenerBeadNodeView
}

// gardenerBeadLaneOrder fixes the lane order + labels for the dense view. Working
// lanes first (in_progress, blocked, open), then the done lane — matching the
// "what it's working on … and what its done" reading order from the request.
var gardenerBeadLaneOrder = []struct{ key, label string }{
	{"in_progress", "In progress"},
	{"blocked", "Blocked"},
	{"open", "Open"},
	{"done", "Done"},
}

// gardenerBeadLaneFor maps a bead status to its dense-view lane key, mirroring
// the feed's status→lane assignment. Closed/terminal statuses land in "done";
// "blocked" gets its own lane; everything else open-ish (open, in_progress, or
// an empty/unknown status) falls back to the nearest lifecycle lane. Using the
// shared statusOpenSortKey keeps the open-vs-done split identical to the
// IsClosed flag the node already carries.
func gardenerBeadLaneFor(n gardenerBeadNodeView) string {
	switch n.Status {
	case "in_progress":
		return "in_progress"
	case "blocked":
		return "blocked"
	case "open":
		return "open"
	}
	if n.IsClosed {
		return "done"
	}
	// Empty / unknown status with no closed flag: treat as open work so it is
	// never silently dropped from the dense lanes.
	return "open"
}

// gardenerBeadLanes buckets the discovered nodes into the four status lanes for
// the dense view. The subject gardener bead is dropped (it is not a connection),
// matching groupGardenerBeadNodes. Lanes are always returned in gardenerBeadLaneOrder
// even when empty so the dense grid renders a stable four-column shape; the
// template hides empty lanes. Within a lane, nodes sort by hop-distance then ID,
// the same ordering the grouped view uses.
func gardenerBeadLanes(nodes []gardenerBeadNodeView, gardenerID string) []gardenerBeadLaneView {
	byLane := make(map[string][]gardenerBeadNodeView, len(gardenerBeadLaneOrder))
	for _, n := range nodes {
		if n.ID == gardenerID {
			continue // the subject, not a connection
		}
		lane := gardenerBeadLaneFor(n)
		byLane[lane] = append(byLane[lane], n)
	}
	out := make([]gardenerBeadLaneView, 0, len(gardenerBeadLaneOrder))
	for _, spec := range gardenerBeadLaneOrder {
		ns := byLane[spec.key]
		sort.SliceStable(ns, func(i, j int) bool {
			if ns[i].Degree != ns[j].Degree {
				return ns[i].Degree < ns[j].Degree
			}
			return ns[i].ID < ns[j].ID
		})
		out = append(out, gardenerBeadLaneView{
			Lane:  spec.key,
			Label: spec.label,
			Count: len(ns),
			Nodes: ns,
		})
	}
	return out
}

// clampGardenerBeadsDegree parses ?degree= and clamps to [1, gardenerBeadsMaxDegree],
// defaulting to gardenerBeadsDefaultDegree on empty/garbage input.
func clampGardenerBeadsDegree(v string) int {
	v = strings.TrimSpace(v)
	if v == "" {
		return gardenerBeadsDefaultDegree
	}
	d, err := strconv.Atoi(v)
	if err != nil {
		return gardenerBeadsDefaultDegree
	}
	if d < 1 {
		return 1
	}
	if d > gardenerBeadsMaxDegree {
		return gardenerBeadsMaxDegree
	}
	return d
}
