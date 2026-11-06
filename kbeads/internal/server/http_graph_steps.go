package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"gardenio/kbeads/internal/model"
)

// The named steps of handleGraph (gd-ubMxMUx6zQ). Each one is a stage of the
// pipeline: read args, list the in-window beads, batch-hydrate them, build
// edges, pull in neighbors by edge, build nodes and agent nodes, then prune.

// readGraphArgs decodes the graph args from a POST body or GET query. It
// returns false when a POST body failed to decode (the error is written).
func readGraphArgs(w http.ResponseWriter, r *http.Request) (model.GraphArgs, bool) {
	var args model.GraphArgs
	switch r.Method {
	case http.MethodPost:
		if r.Body != nil {
			if !decodeJSONBody(w, r, &args) {
				return args, false
			}
		}
	case http.MethodGet:
		args = graphArgsFromQuery(r)
	}
	return args, true
}

// applyGraphDefaults clamps the limit and fills the default status and
// excluded-type sets. Clamp the caller-supplied limit through the shared
// ListBeads choke point so this hand-built filter cannot drive the shared
// daemon to OOM — the graph path multiplies it (one ListBeads per status +
// per-bead batch hydrations), so an unbounded limit here is worse than a plain
// list (ka-0000007023; sibling class of the gRPC/HTTP clamps, ka-0000006889).
func applyGraphDefaults(args *model.GraphArgs) {
	args.Limit = clampListBeadsLimit(args.Limit, 500)
	if len(args.Status) == 0 {
		args.Status = []string{"open", "in_progress", "blocked", "deferred"}
	}
	if len(args.ExcludeTypes) == 0 {
		args.ExcludeTypes = []string{"message", "config", "advice", "role", "event"}
	}
}

// graphBaseFilter builds the unscoped base filter from the graph args.
func graphBaseFilter(args model.GraphArgs) model.BeadFilter {
	filter := model.BeadFilter{
		Limit: args.Limit,
		Fleet: args.Fleet,
	}
	if args.Assignee != "" {
		filter.Assignee = args.Assignee
	}
	if args.Priority != nil {
		filter.Priority = args.Priority
	}
	if args.PriorityMin != nil {
		filter.PriorityMin = args.PriorityMin
	}
	if args.PriorityMax != nil {
		filter.PriorityMax = args.PriorityMax
	}
	if len(args.Labels) > 0 {
		filter.Labels = args.Labels
	}
	if len(args.LabelsAny) > 0 {
		// Route the caller's any-of into LabelsAnyUser — its OWN AND-ed EXISTS
		// clause — NOT LabelsAny, which scopeListFilter reserves for the
		// OIDC allowed-projects scope labels. Folding a caller-controlled any-of
		// into LabelsAny would OR-merge it with the scope labels in the store's
		// single `= ANY(...)` clause, letting a restricted OIDC user widen past
		// their project scope (?labels_any=project:other). handleListBeads makes
		// exactly this split for the same reason (ka-0000003307).
		filter.LabelsAnyUser = args.LabelsAny
	}
	if args.ParentID != "" {
		filter.ParentID = args.ParentID
	}
	for _, t := range args.ExcludeTypes {
		filter.ExcludeTypes = append(filter.ExcludeTypes, model.BeadType(t))
	}
	return filter
}

// graphStatusFilters expands the base filter into one ListBeads filter per
// (status, type) pair, in the order the queries run. A closed status honours
// max_age_days.
func graphStatusFilters(filter model.BeadFilter, args model.GraphArgs) []model.BeadFilter {
	var out []model.BeadFilter
	for _, statusStr := range args.Status {
		f := filter
		f.Status = []model.Status{model.Status(statusStr)}
		if statusStr == "closed" && args.MaxAgeDays > 0 {
			cutoff := time.Now().AddDate(0, 0, -args.MaxAgeDays)
			f.UpdatedAfter = &cutoff
		}
		if args.Query != "" {
			f.Search = args.Query
		}
		if len(args.Types) == 0 {
			out = append(out, f)
			continue
		}
		for _, t := range args.Types {
			tf := f
			tf.Type = []model.BeadType{model.BeadType(t)}
			out = append(out, tf)
		}
	}
	return out
}

// listGraphBeads runs every status filter and merges the results by ID.
func (s *BeadsServer) listGraphBeads(ctx context.Context, filters []model.BeadFilter) (map[string]*model.Bead, error) {
	beadMap := make(map[string]*model.Bead)
	for _, f := range filters {
		beads, _, err := s.store.ListBeads(ctx, f)
		if err != nil {
			return nil, err
		}
		for _, b := range beads {
			beadMap[b.ID] = b
		}
	}
	return beadMap, nil
}

// limitByRelevance keeps the limit most relevant beads (then by priority)
// when the merged set is over the limit.
func limitByRelevance(beadMap map[string]*model.Bead, limit int) map[string]*model.Bead {
	if len(beadMap) <= limit {
		return beadMap
	}
	sorted := make([]*model.Bead, 0, len(beadMap))
	for _, b := range beadMap {
		sorted = append(sorted, b)
	}
	sort.Slice(sorted, func(i, j int) bool {
		ri, rj := beadRelevance(sorted[i]), beadRelevance(sorted[j])
		if ri != rj {
			return ri < rj
		}
		return sorted[i].Priority < sorted[j].Priority
	})
	out := make(map[string]*model.Bead, limit)
	for i := 0; i < limit && i < len(sorted); i++ {
		out[sorted[i].ID] = sorted[i]
	}
	return out
}

// graphBatch holds the per-bead batch hydrations of the in-window bead set.
type graphBatch struct {
	depRecords    map[string][]*model.Dependency
	revDepRecords map[string][]*model.Dependency
	depCounts     map[string]*model.DependencyCounts
	labelsMap     map[string][]string
	blockedMap    map[string][]string
	graphStats    *model.GraphStats
	textRefs      []*model.BeadReference

	depErr, revDepErr, countErr, statsErr, refsErr error
}

// fetchGraphBatch runs the batch queries concurrently. Label and blocked-by
// failures degrade to empty maps; the caller decides which other errors fail
// the request.
func (s *BeadsServer) fetchGraphBatch(ctx context.Context, beadIDs []string, includeTextRefs bool) *graphBatch {
	b := &graphBatch{}
	var wg sync.WaitGroup

	wg.Add(6)
	if includeTextRefs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			b.textRefs, b.refsErr = s.store.GetTextReferencesForBeads(ctx, beadIDs)
		}()
	}
	go func() {
		defer wg.Done()
		b.depRecords, b.depErr = s.store.GetDependenciesForBeads(ctx, beadIDs)
	}()
	go func() {
		defer wg.Done()
		b.revDepRecords, b.revDepErr = s.store.GetReverseDependenciesForBeads(ctx, beadIDs)
	}()
	go func() {
		defer wg.Done()
		b.depCounts, b.countErr = s.store.GetDependencyCounts(ctx, beadIDs)
	}()
	go func() {
		defer wg.Done()
		var err error
		b.labelsMap, err = s.store.GetLabelsForBeads(ctx, beadIDs)
		if err != nil {
			b.labelsMap = make(map[string][]string)
		}
	}()
	go func() {
		defer wg.Done()
		var err error
		b.blockedMap, err = s.store.GetBlockedByForBeads(ctx, beadIDs)
		if err != nil {
			b.blockedMap = make(map[string][]string)
		}
	}()
	go func() {
		defer wg.Done()
		// GetStats counts every bead in the store. A scoped principal gets no
		// stats rather than another project's totals (gd-IyeaKcmI82).
		if !scopeFromContext(ctx).Unrestricted {
			return
		}
		b.graphStats, b.statsErr = s.store.GetStats(ctx)
	}()

	wg.Wait()
	return b
}

// graphEdgeSet accumulates edges, dropping any whose key was already seen.
type graphEdgeSet struct {
	edges []*model.GraphEdge
	seen  map[string]bool
}

func newGraphEdgeSet() *graphEdgeSet {
	return &graphEdgeSet{edges: make([]*model.GraphEdge, 0), seen: make(map[string]bool)}
}

// add appends e under key and reports whether it was new.
func (es *graphEdgeSet) add(key string, e *model.GraphEdge) bool {
	if es.seen[key] {
		return false
	}
	es.seen[key] = true
	es.edges = append(es.edges, e)
	return true
}

// addDepEdge adds a deps-table edge and reports whether it was new.
func (es *graphEdgeSet) addDepEdge(dep *model.Dependency, source string) bool {
	return es.add(fmt.Sprintf("%s->%s:%s", source, dep.DependsOnID, dep.Type), &model.GraphEdge{
		Source:   source,
		Target:   dep.DependsOnID,
		Type:     string(dep.Type),
		DepType:  string(dep.Type),
		Metadata: map[string]string{"source": "dep"},
	})
}

// addDepEdges adds forward and (when they loaded) reverse dep edges. With
// includeDeps it returns the off-window endpoints of each new edge: the
// target of a forward dep, the dependent of a reverse dep.
func (es *graphEdgeSet) addDepEdges(batch *graphBatch, beadMap map[string]*model.Bead, includeDeps bool) map[string]bool {
	missing := make(map[string]bool)
	note := func(id string) {
		if !includeDeps {
			return
		}
		if _, exists := beadMap[id]; !exists {
			missing[id] = true
		}
	}
	for beadID, deps := range batch.depRecords {
		for _, dep := range deps {
			if es.addDepEdge(dep, beadID) {
				note(dep.DependsOnID)
			}
		}
	}
	if batch.revDepErr != nil || batch.revDepRecords == nil {
		return missing
	}
	for _, deps := range batch.revDepRecords {
		for _, dep := range deps {
			if es.addDepEdge(dep, dep.BeadID) {
				note(dep.BeadID)
			}
		}
	}
	return missing
}

// addTextRefEdges builds edges from text references (ka-id mentions parsed
// out of bead text). Both source and target must be in the in-window bead
// set — dangling targets are dropped at both the SQL layer and here. See
// docs/design-bead-references.md § 5b.
//
// Typed mentions (epic ka-0000003840): rows with relationship_type set emit
// one edge per (src, tgt, relationship_type) triple with Type and DepType set
// to the relationship verb (e.g. "blocks", "parent-child"), matching
// deps-edge shape so renderers achieve Decision-4 parity. Rows with NULL
// relationship_type collapse to a single "mentions" edge with source_fields
// joined in metadata (legacy behavior).
//
// All text-derived edges carry metadata["source"]="text" so consumers can
// distinguish from deps-table edges (metadata["source"]="dep").
func (es *graphEdgeSet) addTextRefEdges(textRefs []*model.BeadReference, beadMap map[string]*model.Bead) {
	type mentionKey struct{ src, tgt, relType string }
	mentionFields := make(map[mentionKey][]string)
	mentionOrder := make([]mentionKey, 0)
	for _, ref := range textRefs {
		if _, srcOk := beadMap[ref.SourceBeadID]; !srcOk {
			continue
		}
		if _, tgtOk := beadMap[ref.TargetBeadID]; !tgtOk {
			continue
		}
		relType := ""
		if ref.RelationshipType != nil {
			relType = *ref.RelationshipType
		}
		k := mentionKey{ref.SourceBeadID, ref.TargetBeadID, relType}
		if _, seen := mentionFields[k]; !seen {
			mentionOrder = append(mentionOrder, k)
		}
		mentionFields[k] = append(mentionFields[k], ref.SourceField)
	}
	for _, k := range mentionOrder {
		fields := mentionFields[k]
		sort.Strings(fields)
		edgeType := "mentions"
		if k.relType != "" {
			edgeType = k.relType
		}
		es.add(fmt.Sprintf("%s->%s:text:%s", k.src, k.tgt, edgeType), &model.GraphEdge{
			Source:  k.src,
			Target:  k.tgt,
			Type:    edgeType,
			DepType: edgeType,
			Metadata: map[string]string{
				"source":       "text",
				"source_field": strings.Join(fields, ","),
			},
		})
	}
}

// graphReadGate carries what the by-edge hydrations need to re-check read
// access on beads that the scoped base filter did not select.
type graphReadGate struct {
	excludeTypes map[model.BeadType]bool
	allowedOIDC  []string
	taintGate    beadTaintGateState
}

func newGraphReadGate(excludeTypes []string, allowedOIDC []string, taintGate beadTaintGateState) graphReadGate {
	set := make(map[model.BeadType]bool, len(excludeTypes))
	for _, t := range excludeTypes {
		set[model.BeadType(t)] = true
	}
	return graphReadGate{excludeTypes: set, allowedOIDC: allowedOIDC, taintGate: taintGate}
}

// fetchReadableNeighbors loads the beads in idSet and returns, in store
// order, those that are not an excluded type and pass the read gates. A
// store or taint error yields nil: neighbors are best-effort.
//
// Scope gate (ka-0000008132): these beads are pulled in by edge, not by the
// scoped base filter, so re-check all three read gates (project x2, taint)
// before surfacing an out-of-scope neighbor as a graph node.
func (s *BeadsServer) fetchReadableNeighbors(ctx context.Context, idSet map[string]bool, gate graphReadGate) []*model.Bead {
	if len(idSet) == 0 {
		return nil
	}
	ids := make([]string, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	beads, err := s.store.GetBeadsByIDs(ctx, ids)
	if err != nil {
		return nil
	}
	taintsByBead, taintErr := map[string][]int64(nil), error(nil)
	if !gate.taintGate.exempt {
		taintsByBead, taintErr = s.store.GetTaintsForBeads(ctx, ids)
	}
	if taintErr != nil {
		return nil
	}
	out := make([]*model.Bead, 0, len(beads))
	for _, b := range beads {
		if gate.excludeTypes[b.Type] {
			continue
		}
		if !s.beadReadAuthorized(ctx, b, gate.allowedOIDC, taintsByBead[b.ID], gate.taintGate) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// connectedClosedCandidates returns every edge endpoint and forward-dep
// target that is not already in the bead set.
func connectedClosedCandidates(edges []*model.GraphEdge, depRecords map[string][]*model.Dependency, beadMap map[string]*model.Bead) map[string]bool {
	ids := make(map[string]bool)
	addIfMissing := func(id string) {
		if _, exists := beadMap[id]; !exists {
			ids[id] = true
		}
	}
	for _, e := range edges {
		addIfMissing(e.Source)
		addIfMissing(e.Target)
	}
	for _, deps := range depRecords {
		for _, dep := range deps {
			addIfMissing(dep.DependsOnID)
		}
	}
	return ids
}

// keepConnectedClosed reports whether a connected candidate joins the graph:
// it must be closed and, with maxAgeDays, updated inside the window.
func keepConnectedClosed(b *model.Bead, maxAgeDays int) bool {
	if b.Status != model.StatusClosed {
		return false
	}
	if maxAgeDays > 0 {
		cutoff := time.Now().AddDate(0, 0, -maxAgeDays)
		if b.UpdatedAt.Before(cutoff) {
			return false
		}
	}
	return true
}

// graphParentOf maps each bead to its parent: explicit parent-child deps
// first, then a "blocks" dep onto an in-window epic for beads with none.
func graphParentOf(batch *graphBatch, beadMap map[string]*model.Bead) map[string]string {
	parentOf := make(map[string]string)
	for beadID, deps := range batch.depRecords {
		for _, dep := range deps {
			if dep.Type == model.DepParentChild {
				parentOf[beadID] = dep.DependsOnID
			}
		}
	}
	for _, deps := range batch.revDepRecords {
		for _, dep := range deps {
			if dep.Type == model.DepParentChild {
				parentOf[dep.BeadID] = dep.DependsOnID
			}
		}
	}
	for beadID, deps := range batch.depRecords {
		if _, hasParent := parentOf[beadID]; hasParent {
			continue
		}
		if epic := blockedEpic(deps, beadMap); epic != "" {
			parentOf[beadID] = epic
		}
	}
	return parentOf
}

// blockedEpic returns the first "blocks" target that is an in-window epic.
func blockedEpic(deps []*model.Dependency, beadMap map[string]*model.Bead) string {
	for _, dep := range deps {
		if dep.Type != model.DepBlocks {
			continue
		}
		if target, ok := beadMap[dep.DependsOnID]; ok && target.Type == model.TypeEpic {
			return dep.DependsOnID
		}
	}
	return ""
}

// promoteParentChildEdges retypes "blocks" edges to "parent-child" where
// parentOf says the target is the source's parent.
func promoteParentChildEdges(edges []*model.GraphEdge, parentOf map[string]string) {
	for i, e := range edges {
		if e.Type == string(model.DepBlocks) && parentOf[e.Source] == e.Target {
			edges[i].Type = string(model.DepParentChild)
			edges[i].DepType = string(model.DepParentChild)
		}
	}
}

// buildGraphNodes turns the bead set into graph nodes.
func buildGraphNodes(beadMap map[string]*model.Bead, parentOf map[string]string, batch *graphBatch, includeBody bool) []model.GraphNode {
	nodes := make([]model.GraphNode, 0, len(beadMap))
	for _, bead := range beadMap {
		node := model.GraphNode{
			ID:        bead.ID,
			Title:     bead.Title,
			Status:    string(bead.Status),
			Priority:  bead.Priority,
			IssueType: string(bead.Type),
			ParentID:  parentOf[bead.ID],
			Assignee:  bead.Assignee,
			Labels:    batch.labelsMap[bead.ID],
			CreatedAt: bead.CreatedAt.Format("2006-01-02T15:04:05Z"),
			UpdatedAt: bead.UpdatedAt.Format("2006-01-02T15:04:05Z"),
			BlockedBy: batch.blockedMap[bead.ID],
		}
		if counts, ok := batch.depCounts[bead.ID]; ok {
			node.DepCount = counts.DependencyCount
			node.DepByCount = counts.DependentCount
		}
		if includeBody {
			node.Description = bead.Description
			node.Notes = bead.Notes
		}
		nodes = append(nodes, node)
	}
	return nodes
}

// rosterAgentNodes builds agent nodes from the live presence roster.
func (s *BeadsServer) rosterAgentNodes() map[string]*model.GraphNode {
	agentNodes := make(map[string]*model.GraphNode)
	if s.Presence == nil {
		return agentNodes
	}
	for _, e := range s.Presence.Roster(0) {
		if e.Reaped {
			continue
		}
		status := "idle"
		if e.IdleSecs < 30 {
			status = "active"
		}
		agentNodes[e.Actor] = &model.GraphNode{
			ID:        "agent:" + e.Actor,
			Title:     e.Actor,
			IssueType: "agent",
			Status:    status,
		}
	}
	return agentNodes
}

// assignAgents adds a fallback agent node for every in-progress assignee not
// on the roster, then returns edges with an assigned_to edge per in-progress
// bead appended.
func assignAgents(agentNodes map[string]*model.GraphNode, beadMap map[string]*model.Bead, edges []*model.GraphEdge) []*model.GraphEdge {
	for _, bead := range beadMap {
		if bead.Assignee == "" || bead.Status != model.StatusInProgress {
			continue
		}
		if _, exists := agentNodes[bead.Assignee]; !exists {
			agentNodes[bead.Assignee] = &model.GraphNode{
				ID:        "agent:" + bead.Assignee,
				Title:     bead.Assignee,
				IssueType: "agent",
			}
		}
	}
	for _, bead := range beadMap {
		if bead.Assignee == "" || bead.Status != model.StatusInProgress {
			continue
		}
		if _, exists := agentNodes[bead.Assignee]; exists {
			edges = append(edges, &model.GraphEdge{
				Source:  "agent:" + bead.Assignee,
				Target:  bead.ID,
				Type:    "assigned_to",
				DepType: "assigned_to",
			})
		}
	}
	return edges
}

// enrichAgentNodes adds memory stats from agent beads (ka-0000000124). Agent
// beads (type=agent) have a "agent" field matching the actor name, and may
// have memory_total / memory_avg_strength fields.
func (s *BeadsServer) enrichAgentNodes(ctx context.Context, agentNodes map[string]*model.GraphNode) {
	for name, node := range agentNodes {
		agentBeads, _, _ := s.store.ListBeads(ctx, model.BeadFilter{
			Type:   []model.BeadType{"agent"},
			Fields: map[string]string{"agent": name},
			Limit:  1,
		})
		if len(agentBeads) > 0 {
			enrichAgentMemory(node, agentBeads[0].Fields)
		}
	}
}

// agentNodesWithEdges returns the agent nodes that source at least one
// assigned_to or requested_by edge.
func agentNodesWithEdges(agentNodes map[string]*model.GraphNode, edges []*model.GraphEdge) []model.GraphNode {
	agentsWithEdges := make(map[string]bool)
	for _, e := range edges {
		if e.Type == "assigned_to" || e.Type == "requested_by" {
			agentsWithEdges[e.Source] = true
		}
	}
	var out []model.GraphNode
	for _, node := range agentNodes {
		if agentsWithEdges[node.ID] {
			out = append(out, *node)
		}
	}
	return out
}

// addGraphAgents adds agent nodes with assignment edges.
func (s *BeadsServer) addGraphAgents(ctx context.Context, beadMap map[string]*model.Bead, nodes []model.GraphNode, edges []*model.GraphEdge) ([]model.GraphNode, []*model.GraphEdge) {
	agentNodes := s.rosterAgentNodes()
	edges = assignAgents(agentNodes, beadMap, edges)
	s.enrichAgentNodes(ctx, agentNodes)
	return append(nodes, agentNodesWithEdges(agentNodes, edges)...), edges
}

// isOpenActiveGraphStatus reports whether a node status counts as live work
// (bead statuses plus the agent active/idle statuses).
func isOpenActiveGraphStatus(status string) bool {
	switch status {
	case "open", "in_progress", "blocked", "deferred", "active", "idle":
		return true
	}
	return false
}

// terminalNodesNearActive returns the terminal nodes that neighbor live
// work. 'closed' and 'partial' are both terminal-for-progression
// (ka-0000007305 §2.3); a terminal node is only kept when it neighbors active
// work.
func terminalNodesNearActive(nodes []model.GraphNode, edges []*model.GraphEdge) map[string]bool {
	openActiveSet := make(map[string]bool)
	for _, n := range nodes {
		if isOpenActiveGraphStatus(n.Status) {
			openActiveSet[n.ID] = true
		}
	}
	neighbors := make(map[string][]string)
	for _, e := range edges {
		neighbors[e.Source] = append(neighbors[e.Source], e.Target)
		neighbors[e.Target] = append(neighbors[e.Target], e.Source)
	}
	closedConnected := make(map[string]bool)
	for _, n := range nodes {
		if !model.Status(n.Status).IsTerminalForProgression() {
			continue
		}
		for _, neighborID := range neighbors[n.ID] {
			if openActiveSet[neighborID] {
				closedConnected[n.ID] = true
				break
			}
		}
	}
	return closedConnected
}

// keepGraphNode reports whether a node survives pruning: gates and decisions
// need an edge, agents need an edge or a live status, and terminal nodes must
// neighbor live work.
func keepGraphNode(n model.GraphNode, connectedIDs, closedConnected map[string]bool) bool {
	switch n.IssueType {
	case "gate", "decision":
		if !connectedIDs[n.ID] {
			return false
		}
	case "agent":
		if !connectedIDs[n.ID] && n.Status != "active" && n.Status != "idle" {
			return false
		}
	}
	return !model.Status(n.Status).IsTerminalForProgression() || closedConnected[n.ID]
}

// pruneGraph drops disconnected nodes, then the edges pointing to them.
func pruneGraph(nodes []model.GraphNode, edges []*model.GraphEdge) ([]model.GraphNode, []*model.GraphEdge) {
	connectedIDs := make(map[string]bool)
	for _, e := range edges {
		connectedIDs[e.Source] = true
		connectedIDs[e.Target] = true
	}
	closedConnected := terminalNodesNearActive(nodes, edges)

	filteredNodes := make([]model.GraphNode, 0, len(nodes))
	for _, n := range nodes {
		if keepGraphNode(n, connectedIDs, closedConnected) {
			filteredNodes = append(filteredNodes, n)
		}
	}

	nodeIDSet := make(map[string]bool, len(filteredNodes))
	for _, n := range filteredNodes {
		nodeIDSet[n.ID] = true
	}
	filteredEdges := make([]*model.GraphEdge, 0, len(edges))
	for _, e := range edges {
		if nodeIDSet[e.Source] && nodeIDSet[e.Target] {
			filteredEdges = append(filteredEdges, e)
		}
	}
	return filteredNodes, filteredEdges
}
