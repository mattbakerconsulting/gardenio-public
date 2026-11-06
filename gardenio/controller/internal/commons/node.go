package commons

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"sync"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/kin"
)

// The node model (Commons v2 step 3, gd-qlk31NBV6o): GET /garden/c/n/{id} is
// one fragment for whatever the workbench is opened on. It takes any bead and
// answers with the node: its thread, and its children to two levels, each with
// its state, PR, open ask and job count. Its head and face are the bead view's
// (gardenhub bead.go), which the workbench draws above the tree's rails
// (web/commons2.js, gd-0GnBi7SRfK). A row with more below it
// carries the node URL that expands it (the same route, on that bead):
//
//	plan    a plan epic: its phases and steps (phase_order, step_order) and its
//	        gardener (the plan's gardener loop)
//	phase   a phase epic of a plan: its steps, and the plan's other phases
//	epic    any other epic: its tasks, and the beads its blocks edges hold up
//	thread  a wish head: the roots of the work bound to it (thread_id) and the
//	        agents on none of them, and its reply count
//	agent   an agent: the agents it spawned, and its jobs (jobs.go)
//	bead    anything else: its children and the agents on it
//
// Open under the node is the decisions, escalations and PRs the subtree's
// edges name (a decision or escalation blocks the task that asked it, a task
// produces its merge request): open ones first, done ones ticked.
//
// Every read is on the viewer's bearer and cut by land, as the routes' are; a
// bead in a land the viewer may not see is left out wherever it would show.
// The reads are batched by level: after the root, four rounds of batch reads
// (each round's reads at once), whatever the node's size.

// Nodes is the slice of the daemon client the node model reads children and
// edges through, on the request context. *beadsapi.Client satisfies it.
type Nodes interface {
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
	BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]beadsapi.Dependency, error)
}

const (
	// nodeChildrenMax and nodeGrandMax bound the first level and each first-
	// level row's own children; nodeRowsMax every row of the node together.
	nodeChildrenMax = 60
	nodeGrandMax    = 12
	nodeRowsMax     = 240
	// nodeOpenMax bounds the decisions, escalations and PRs under the node.
	nodeOpenMax = 30
	// nodeFaceMax is the rune cap on a steward node's face (its notes) on
	// screen; nodeFaceWholeMax on the whole of it the face carries for its Read
	// it all, the bead view's own cap on a description (gardenhub beadDescMax).
	nodeFaceMax      = 5000
	nodeFaceWholeMax = 20000
	// nodeJobsMax is the jobs an agent's node shows.
	nodeJobsMax = 20
	// nodeSpawnedMax bounds the agents an agent's node lists.
	nodeSpawnedMax = 60
	// GardenerEpicLabel prefixes the label a plan's gardener loop carries, and
	// PlanGardenerLabel marks a plan gardener (plangardener.GardenerEpicLabel
	// and MarkerLabel, which beads-viewer's tests pin: this package does not
	// import the scheduler side).
	GardenerEpicLabel = "gardener-epic:"
	PlanGardenerLabel = "plan-gardener"
)

// The kinds of node.
const (
	nodePlan       = "plan"
	nodePhase      = "phase"
	nodeEpic       = "epic"
	nodeThreadKind = "thread"
	nodeAgent      = "agent"
	nodeBead       = "bead"
	nodeSteward    = "steward" // a land's steward (stewardnode.go)
)

// nodeDoc is the node fragment's data.
type nodeDoc struct {
	Base string
	Kind string
	Land string
	Root nodeRow
	// Face is a steward node's notes (Markdown), FaceFrom the bead they were
	// read from (stewardnode.go); any other node's face is the bead view's.
	// FaceWhole is the whole of them when Face was cut at nodeFaceMax, opened
	// in place by the face's Read it all (web/cuts.js, gd-Fmq3fgqFTm).
	Face, FaceFrom, FaceWhole string
	Thread         *nodeThread
	Parent         *nodeRow     // what it hangs from: a phase's plan, a task's epic, an agent's bead
	Gardener       *nodeRow     // a plan's gardener loop (not the land's steward)
	Steward        *stewardHead // a steward node's own part (stewardnode.go)
	Children       []nodeRow
	More           int // first-level rows not sent
	Siblings       []nodeRow
	Dependents     []nodeRow
	Open           []nodeOpen
	Jobs           []Job
	JobCount       int
	// Live is a running agent's card face, which the node's head carries
	// (step 5): its state, spend, Watch live and Nudge.
	Live *AgentCard
	// Reason is an agent's reason, as Handler.Reason says it; nil when it has
	// none.
	Reason *nodeReason
	Names  beadNames
}

// nodeReason is why an agent is not simply working: the farm's reason kind
// and words, its name and its task's title, which the workbench's Wake or
// Restart asks with (web/wake.js).
type nodeReason struct {
	Kind, Text, Agent, Task string
}

// nodeRow is one bead of the tree.
type nodeRow struct {
	ID     string
	Type   string
	Title  string
	Href   string // its own node
	State  string // plan_state, agent_state, else status
	Closed bool
	// Blocked is the open beads it waits on (not its asks).
	Blocked []nodeRef
	PR      *nodePR
	// Ask is an open decision or escalation it waits on.
	Ask      *nodeRef
	Jobs     int
	Children []nodeRow
	// More is children not sent; Expand is the node URL that shows them.
	More   int
	Expand string
	// Flag marks a steward node's plan or step: stalled, parked.
	Flag string
	// Thread is the thread its conversation is in (itself for a thread), so a
	// stream message there marks the row unread in the workbench.
	Thread string
	// Wish is a steward node's wish row's conversation (stewardnode.go).
	Wish *nodeWish
}

// nodeRef names another bead from a row.
type nodeRef struct {
	ID, Type, Title, Href string
}

// nodePR is a row's merge request.
type nodePR struct {
	URL, State string
}

// nodeThread is where the node's conversation is: the place route sends a
// link there (place.go).
type nodeThread struct {
	ID, Href string
	Replies  int
}

// nodeOpen is one decision, escalation or PR under the node.
type nodeOpen struct {
	ID, Type, Title, URL, State string
	Done                        bool
}

// node serves GET /garden/c/n/{id}. A navigation gets the page with the
// workbench open on it (web/commons2.js), which fetches this fragment.
func (h *Handler) node(w http.ResponseWriter, r *http.Request) {
	if h.page(w, r) || !h.ready(w) {
		return
	}
	// ?embed=1 is the bead panel's read (gardenhub web/beadwork.js), which draws
	// its own lists when there is no node: no node is a 204 there, not an error
	// the browser logs on every bead the Commons does not hold.
	embed := r.URL.Query().Get("embed") == "1"
	if h.Nodes == nil {
		if embed {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, "the node model is not enabled", http.StatusServiceUnavailable)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" || len(id) > 200 {
		http.Error(w, "bad bead id", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	doc, err := h.readNode(ctx, h.scope(r), id)
	if err != nil && embed && errors.Is(err, errNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		h.fail(w, err)
		return
	}
	h.writeTagged(w, r, "node", doc)
}

// nodeRead is one node's reads: every bead read, cut by land, and the edges.
type nodeRead struct {
	h   *Handler
	sc  Scope
	got map[string]*beadsapi.BeadDetail
	fwd map[string][]beadsapi.Dependency
	rev map[string][]beadsapi.Dependency

	// The node being read, filled round by round.
	doc        nodeDoc
	id         string
	root, plan *beadsapi.BeadDetail // plan is a phase's
	// listed is the first level a query found; gardeners the plan's loops.
	listed, gardeners []*beadsapi.BeadDetail
	replies           int
	parent            string
	// first and second are the tree's levels, tree the root and both.
	first, second, tree  []string
	siblings, dependents []string
	grand                map[string][]string // each first-level row's children
}

// bead returns a bead read so far that the viewer may see.
func (n *nodeRead) bead(id string) *beadsapi.BeadDetail {
	if b := n.got[id]; b != nil && n.sc.Allows(landOf(b.Labels)) {
		return b
	}
	return nil
}

// fetch reads the ids not read yet.
func (n *nodeRead) fetch(ctx context.Context, ids []string) error {
	var want []string
	for _, id := range ids {
		if _, ok := n.got[id]; !ok && id != "" && !slices.Contains(want, id) {
			want = append(want, id)
		}
	}
	if len(want) == 0 {
		return nil
	}
	got, err := n.h.Nodes.BatchGetBeads(ctx, want)
	if err != nil {
		return readErr("beads", err)
	}
	for _, id := range want {
		n.got[id] = got[id] // nil marks one read and not returned
	}
	return nil
}

// edges reads the ids' edges in one direction, into dst.
func (n *nodeRead) edges(ctx context.Context, ids []string, dir string, dst map[string][]beadsapi.Dependency) error {
	if len(ids) == 0 {
		return nil
	}
	got, err := n.h.Nodes.BatchGetDependencies(ctx, ids, dir)
	if err != nil {
		return readErr("edges", err)
	}
	for _, id := range ids {
		dst[id] = got[id]
	}
	return nil
}

// below is the beads that hang from id: its parent-child children, then the
// agents assigned to it.
func (n *nodeRead) below(id string) []string {
	var kids, agents []string
	for _, d := range n.rev[id] {
		switch {
		case d.BeadID == "" || d.BeadID == id:
		case d.Type == "parent-child" && !slices.Contains(kids, d.BeadID):
			kids = append(kids, d.BeadID)
		case d.Type == "assigned" && !slices.Contains(agents, d.BeadID):
			agents = append(agents, d.BeadID)
		}
	}
	return append(kids, agents...)
}

// parallel runs fs at once and returns the first error.
func parallel(fs ...func() error) error {
	errs := make([]error, len(fs))
	var wg sync.WaitGroup
	for i, f := range fs {
		wg.Add(1)
		go func() { defer wg.Done(); errs[i] = f() }()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// readNode reads the node rooted at id: the root, then four rounds of batch
// reads (roundOne to roundThree), then the rows, the thread and the names.
func (h *Handler) readNode(ctx context.Context, sc Scope, id string) (nodeDoc, error) {
	b, err := h.Beads.GetBead(ctx, id)
	if err != nil {
		return nodeDoc{}, readErr("bead "+id, err)
	}
	if b == nil || !sc.Allows(landOf(b.Labels)) {
		return nodeDoc{}, errNotFound
	}
	if nodeKindOf(b) == nodeSteward && h.Stewards != nil {
		return h.readSteward(ctx, sc, landOf(b.Labels))
	}
	n := &nodeRead{h: h, sc: sc, id: id, root: b, got: map[string]*beadsapi.BeadDetail{b.ID: b},
		fwd: map[string][]beadsapi.Dependency{}, rev: map[string][]beadsapi.Dependency{}, grand: map[string][]string{},
		doc: nodeDoc{Base: h.BasePath, Land: landOf(b.Labels), Kind: nodeKindOf(b)}}
	for _, round := range []func(context.Context) error{n.roundOne, n.roundTwo, n.roundThree} {
		if err := round(ctx); err != nil {
			return nodeDoc{}, err
		}
	}
	n.rows()
	n.thread()
	c, agent := agentCardFrom(b, n.doc.Land)
	if agent && c.Live {
		n.doc.Live = &c
	}
	if agent && h.Reason != nil {
		if kind, text := h.Reason(b.ID); kind != "" {
			n.doc.Reason = &nodeReason{Kind: kind, Text: text, Agent: c.Name, Task: firstNonEmpty(c.Task, strings.TrimSpace(b.Fields["task_id"]))}
		}
	}
	n.doc.Names = beadNames{}
	for k, v := range n.got {
		if v != nil && sc.Allows(landOf(v.Labels)) {
			n.doc.Names[k] = rowTitle(v)
		}
	}
	return n.doc, nil
}

// roundOne reads the root's edges, and what a thread, an agent or a plan finds
// by query rather than by edge.
func (n *nodeRead) roundOne(ctx context.Context) error {
	id, b := n.id, n.root
	round := []func() error{
		func() error { return n.edges(ctx, []string{id}, "forward", n.fwd) },
		func() error { return n.edges(ctx, []string{id}, "reverse", n.rev) },
	}
	switch n.doc.Kind {
	case nodeThreadKind:
		round = append(round, func() error { return n.threadBeads(ctx) })
	case nodeAgent:
		if name := strings.TrimSpace(b.Fields["agent"]); name != "" {
			round = append(round, func() error { return n.spawned(ctx, name) })
		}
	case nodePlan:
		round = append(round, func() error {
			res, err := n.h.Beads.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
				Types: []string{"trigger", "schedule"}, Labels: []string{GardenerEpicLabel + id}, Limit: 10,
			})
			if err == nil && res != nil {
				n.gardeners = res.Beads
			}
			return nil // a plan with no gardener read still shows
		})
	}
	return parallel(round...)
}

// threadBeads lists the beads bound to the thread: its replies are counted,
// the rest are its first level.
func (n *nodeRead) threadBeads(ctx context.Context) error {
	res, err := n.h.Beads.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Labels: []string{"project:" + n.doc.Land}, Fields: map[string]string{fieldThreadID: n.id}, Limit: threadMax,
	})
	if err != nil {
		return readErr("thread "+n.id, err)
	}
	for _, c := range res.Beads {
		switch {
		case c == nil || c.ID == n.id:
		case c.Type == typeMessage:
			n.replies++
		default:
			n.listed = append(n.listed, c)
		}
	}
	return nil
}

// spawned lists the agents the agent named name spawned.
func (n *nodeRead) spawned(ctx context.Context, name string) error {
	res, err := n.h.Beads.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types: []string{"agent"}, Fields: map[string]string{"spawned_by": name}, Sort: "created_at", Limit: nodeSpawnedMax,
	})
	if err != nil {
		return readErr("agents of "+n.id, err)
	}
	n.listed = res.Beads
	return nil
}

// levelOne is the first level: the root's children (a thread's and an
// agent's are listed). It returns the beads the root's edges name that show
// beside them: what it waits on and makes, what it holds up, its parent.
func (n *nodeRead) levelOne() []string {
	for _, c := range n.listed {
		if c != nil {
			n.got[c.ID] = c
			n.first = append(n.first, c.ID)
		}
	}
	n.first = append(n.first, n.below(n.id)...)
	var also []string
	for _, d := range n.fwd[n.id] {
		switch {
		case (d.Type == "parent-child" || d.Type == "assigned") && n.parent == "":
			n.parent = d.DependsOnID // an agent's is the bead it is on
		case d.Type == "blocks" || d.Type == "produces":
			also = append(also, d.DependsOnID)
		}
	}
	if n.doc.Kind == nodeEpic || n.doc.Kind == nodePlan {
		for _, d := range n.rev[n.id] {
			if d.Type == "blocks" && d.BeadID != "" {
				n.dependents = append(n.dependents, d.BeadID)
			}
		}
	}
	return append(append(also, n.dependents...), n.parent)
}

// roundTwo reads the first level's beads and their children's edges, and a
// phase's plan for its other phases; then it settles the first level.
func (n *nodeRead) roundTwo(ctx context.Context) error {
	also := n.levelOne()
	revIDs := slices.Clone(n.first)
	if n.parent != "" {
		revIDs = append(revIDs, n.parent)
	}
	var listedIDs []string
	if n.doc.Kind == nodeThreadKind {
		listedIDs = slices.Clone(n.first)
	}
	if err := parallel(
		func() error { return n.fetch(ctx, append(slices.Clone(n.first), also...)) },
		func() error { return n.edges(ctx, revIDs, "reverse", n.rev) },
		func() error { return n.edges(ctx, listedIDs, "forward", n.fwd) },
	); err != nil {
		return err
	}
	if len(listedIDs) > 0 {
		// the thread's own level is the roots of the work it made and its
		// agents: a bound bead with a parent other than the thread (a plan's
		// step) shows under it, and a crew on a bound bead under that bead
		n.first = slices.DeleteFunc(n.first, func(c string) bool {
			return slices.ContainsFunc(n.fwd[c], func(d beadsapi.Dependency) bool {
				return (d.Type == "parent-child" && d.DependsOnID != n.id) || (d.Type == "assigned" && slices.Contains(listedIDs, d.DependsOnID))
			})
		})
	}
	if p := n.bead(n.parent); p != nil && nodeKindOf(p) == nodePlan && slices.Contains(idList(p.Fields["phase_order"]), n.id) {
		n.doc.Kind, n.plan = nodePhase, p
		n.siblings = idList(p.Fields["phase_order"])
	}
	order := idList(n.root.Fields["step_order"])
	if n.doc.Kind == nodePlan {
		order = append(idList(n.root.Fields["phase_order"]), order...)
	}
	n.first = n.ordered(n.first, order)
	if n.doc.Kind == nodePlan { // a plan's narrative is its face, not a step
		n.first = slices.DeleteFunc(n.first, func(c string) bool { bb := n.bead(c); return bb != nil && bb.Type == "bundle" })
	}
	if len(n.first) > nodeChildrenMax {
		n.doc.More, n.first = len(n.first)-nodeChildrenMax, n.first[:nodeChildrenMax]
	}
	for _, c := range n.first {
		g := n.below(c)
		n.grand[c] = g
		n.second = append(n.second, g[:min(len(g), nodeGrandMax)]...)
	}
	if len(n.first)+len(n.second) > nodeRowsMax {
		n.second = n.second[:max(0, nodeRowsMax-len(n.first))]
	}
	return nil
}

// roundThree reads the second level's beads, whether each has more below it,
// and the forward edges of the whole tree (what each row waits on and makes);
// then, in the last round, the beads those edges name that are not in it.
func (n *nodeRead) roundThree(ctx context.Context) error {
	n.tree = append(append([]string{n.id}, n.first...), n.second...)
	if err := parallel(
		func() error { return n.fetch(ctx, append(slices.Clone(n.second), n.siblings...)) },
		func() error { return n.edges(ctx, n.second, "reverse", n.rev) },
		func() error { return n.edges(ctx, n.tree[1:], "forward", n.fwd) },
	); err != nil {
		return err
	}
	var named []string
	for _, t := range n.tree {
		for _, d := range n.fwd[t] {
			if d.Type == "blocks" || d.Type == "produces" {
				named = append(named, d.DependsOnID)
			}
		}
	}
	return n.fetch(ctx, named)
}

// rows fills the doc's rows from the beads read.
func (n *nodeRead) rows() {
	jobs := map[string]int{}
	for _, t := range n.tree {
		if bb := n.bead(t); bb != nil && bb.Type == "agent" {
			jobs[t] = n.h.Jobs.Count(t)
		}
	}
	doc := &n.doc
	doc.Root = n.row(n.id, jobs)
	for _, c := range n.first {
		if row := n.childRow(c, jobs); row.ID != "" {
			doc.Children = append(doc.Children, row)
		}
	}
	for _, s := range n.siblings {
		if row := n.row(s, jobs); s != n.id && row.ID != "" {
			doc.Siblings = append(doc.Siblings, row)
		}
	}
	for _, d := range n.dependents {
		if row := n.row(d, jobs); row.ID != "" {
			doc.Dependents = append(doc.Dependents, row)
		}
	}
	if p := n.row(n.parent, jobs); p.ID != "" {
		doc.Parent = &p
	}
	if g := gardenerOf(n.gardeners, n.id); g != nil && n.sc.Allows(landOf(g.Labels)) {
		n.got[g.ID] = g
		s := n.row(g.ID, jobs)
		doc.Gardener = &s
	}
	doc.Open = n.open(n.tree)
}

// childRow is a first-level row with its own children, as far as the tree
// went; one with more below it expands.
func (n *nodeRead) childRow(c string, jobs map[string]int) nodeRow {
	row := n.row(c, jobs)
	if row.ID == "" {
		return row
	}
	g := n.grand[c]
	for i, gc := range g {
		if i == nodeGrandMax || !slices.Contains(n.tree, gc) {
			row.More = len(g) - i
			break
		}
		gr := n.row(gc, jobs)
		if gr.ID == "" || (gr.Type == "bundle" && nodeKindOf(n.got[c]) == nodePlan) {
			continue // a plan's narrative is its face, not a step
		}
		if below := n.below(gc); len(below) > 0 {
			gr.More, gr.Expand = len(below), n.h.nodeHref(gc)
		}
		row.Children = append(row.Children, gr)
		if gr.Type == "agent" {
			row.Jobs += gr.Jobs
		}
	}
	if row.More > 0 {
		row.Expand = n.h.nodeHref(c)
	}
	return row
}

// thread fills the doc's thread, a thread head's own or the one the bead is
// in, and an agent's jobs.
func (n *nodeRead) thread() {
	doc, b := &n.doc, n.root
	switch doc.Kind {
	case nodeThreadKind:
		doc.Thread = &nodeThread{ID: n.id, Href: n.h.placeHref(n.id), Replies: n.replies}
	case nodeAgent:
		doc.Jobs, doc.JobCount = n.h.Jobs.Of(n.id)
		doc.Jobs = doc.Jobs[:min(len(doc.Jobs), nodeJobsMax)]
	}
	if head := strings.TrimSpace(b.Fields[fieldThreadID]); doc.Thread == nil && head != "" {
		doc.Thread = &nodeThread{ID: head, Href: n.h.placeHref(head)}
	}
}

// row is the bead as the tree shows it, with what its forward edges name; the
// zero row for a bead the viewer may not see.
func (n *nodeRead) row(id string, jobs map[string]int) nodeRow {
	b := n.bead(id)
	if b == nil {
		return nodeRow{}
	}
	r := nodeRow{ID: b.ID, Type: b.Type, Title: rowTitle(b), Href: n.h.nodeHref(b.ID), Closed: b.Status == "closed", Jobs: jobs[b.ID]}
	r.State = firstNonEmpty(b.Fields["plan_state"], b.Fields["agent_state"], b.Status)
	r.Thread = strings.TrimSpace(b.Fields[fieldThreadID])
	if kin.IsWishHead(b) {
		r.Thread = b.ID
	}
	if b.Type == "trigger" || b.Type == "schedule" {
		r.State = loopState(b)
	}
	for _, d := range n.fwd[id] {
		o := n.bead(d.DependsOnID)
		if o == nil {
			continue
		}
		switch {
		case d.Type == "produces" && o.Type == "merge-request" && r.PR == nil:
			r.PR = &nodePR{URL: httpURL(o.Fields["mr_url"]), State: firstNonEmpty(o.Fields["mr_state"], o.Status)}
		case d.Type != "blocks" || o.Status == "closed":
		case o.Type == "decision" || o.Type == "escalation":
			if r.Ask == nil {
				r.Ask = &nodeRef{ID: o.ID, Type: o.Type, Title: rowTitle(o), Href: n.h.nodeHref(o.ID)}
			}
		default:
			r.Blocked = append(r.Blocked, nodeRef{ID: o.ID, Type: o.Type, Title: rowTitle(o), Href: n.h.nodeHref(o.ID)})
		}
	}
	if r.PR == nil && b.Type == "merge-request" {
		r.PR = &nodePR{URL: httpURL(b.Fields["mr_url"]), State: firstNonEmpty(b.Fields["mr_state"], b.Status)}
	}
	return r
}

// open is the decisions, escalations and PRs the tree's edges name: open ones
// first, then done ones, each group as the edges came.
func (n *nodeRead) open(tree []string) []nodeOpen {
	var open, done []nodeOpen
	seen := map[string]bool{}
	for _, t := range tree {
		for _, d := range n.fwd[t] {
			o := n.bead(d.DependsOnID)
			if o == nil || seen[o.ID] {
				continue
			}
			var x nodeOpen
			switch {
			case d.Type == "blocks" && (o.Type == "decision" || o.Type == "escalation"):
				x = nodeOpen{ID: o.ID, Type: o.Type, Title: rowTitle(o), State: o.Status, Done: o.Status == "closed"}
			case d.Type == "produces" && o.Type == "merge-request":
				st := firstNonEmpty(o.Fields["mr_state"], o.Status)
				x = nodeOpen{ID: o.ID, Type: "pr", Title: rowTitle(o), URL: httpURL(o.Fields["mr_url"]), State: st,
					Done: o.Status == "closed" || st == "merged" || st == "closed"}
			default:
				continue
			}
			seen[o.ID] = true
			if x.Done {
				done = append(done, x)
			} else {
				open = append(open, x)
			}
		}
	}
	out := append(open, done...)
	if len(out) > nodeOpenMax {
		out = out[:nodeOpenMax]
	}
	return out
}

// ordered sorts ids by order first, then as they came; it drops repeats.
func (n *nodeRead) ordered(ids, order []string) []string {
	rank := map[string]int{}
	for i, id := range order {
		if _, dup := rank[id]; !dup {
			rank[id] = i
		}
	}
	var out []string
	for _, id := range ids {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, iok := rank[out[i]]
		rj, jok := rank[out[j]]
		if iok != jok {
			return iok
		}
		return iok && ri < rj
	})
	return out
}

// nodeKindOf is the kind of node a bead roots, as far as the bead alone says:
// a phase reads as an epic until its plan is read.
func nodeKindOf(b *beadsapi.BeadDetail) string {
	switch {
	case b.Type == "agent":
		return nodeAgent
	case stewardNodeLand(b) != "":
		return nodeSteward
	case kin.IsWishHead(b):
		return nodeThreadKind
	case b.Type == "epic" && hasLabel(b.Labels, "plan"):
		return nodePlan
	case b.Type == "epic":
		return nodeEpic
	}
	return nodeBead
}

// gardenerOf is the plan's gardener loop among the loops listed: open before
// closed, then the plan-gardener marker, then the one updated last (gardenhub
// planGardenerBetter).
func gardenerOf(loops []*beadsapi.BeadDetail, plan string) *beadsapi.BeadDetail {
	var best *beadsapi.BeadDetail
	for _, g := range loops {
		if g == nil || !hasLabel(g.Labels, GardenerEpicLabel+plan) {
			continue
		}
		if best == nil {
			best = g
			continue
		}
		if ao, bo := g.Status != "closed", best.Status != "closed"; ao != bo {
			if ao {
				best = g
			}
			continue
		}
		if am, bm := hasLabel(g.Labels, PlanGardenerLabel), hasLabel(best.Labels, PlanGardenerLabel); am != bm {
			if am {
				best = g
			}
			continue
		}
		if g.UpdatedAt.After(best.UpdatedAt) {
			best = g
		}
	}
	return best
}

// loopState is a gardener loop's state: retired (closed), paused (enabled
// false) or running.
func loopState(g *beadsapi.BeadDetail) string {
	switch {
	case g.Status == "closed":
		return "retired"
	case strings.TrimSpace(g.Fields["enabled"]) == "false":
		return "paused"
	}
	return "running"
}

// rowTitle is the name a row shows: the short title, else the title.
func rowTitle(b *beadsapi.BeadDetail) string {
	return clipRunes(firstNonEmpty(b.ShortTitle, b.Title, b.ID), nodeTitleMax)
}

// idList is a JSON array of ids (phase_order, step_order), nil when it is not
// one.
func idList(raw string) []string {
	var ids []string
	if json.Unmarshal([]byte(raw), &ids) != nil {
		return nil
	}
	return slices.DeleteFunc(ids, func(s string) bool { return strings.TrimSpace(s) == "" })
}

// httpURL is s when it is an http(s) link, else "".
func httpURL(s string) string {
	s = strings.TrimSpace(s)
	if u, err := url.Parse(s); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return s
}

func (h *Handler) nodeHref(id string) string {
	return h.BasePath + "/garden/c/n/" + url.PathEscape(id)
}

func (h *Handler) placeHref(id string) string {
	return h.BasePath + "/garden/c/bead/" + url.PathEscape(id)
}
