package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"gardenio/kbeads/internal/model"
)

// handleGraph handles both GET /v1/graph and POST /v1/graph.
// Returns nodes and edges for 3D force-directed graph visualization.
// The steps live in http_graph_steps.go.
func (s *BeadsServer) handleGraph(w http.ResponseWriter, r *http.Request) {
	args, ok := readGraphArgs(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	applyGraphDefaults(&args)

	// Enforce cross-project read scope (ka-0000008132): this hand-built filter
	// went straight to store.ListBeads with no project predicate, so the graph
	// returned node Title/Status/Assignee/Labels (and, with include_body,
	// Description/Notes) across every project. Inject the fleet-token project:
	// label (AND) and the OIDC allowed-projects labels (OR) exactly as
	// handleListBeads does. allowedOIDC is also resolved once here so the
	// dep-target / connected-closed batch hydrations below can drop out-of-scope
	// beads pulled in by edge without re-querying the caller's user bead per row.
	filter := s.scopeListFilter(ctx, graphBaseFilter(args))
	allowedOIDC := oidcAllowedProjects(ctx, s.store)
	taintGate, err := s.resolveBeadTaintGateState(ctx)
	if err != nil {
		writeServerError(ctx, w, "handleGraph", err, "failed to resolve taint grants")
		return
	}

	// Multi-status: query each status separately and merge.
	beadMap, err := s.listGraphBeads(ctx, graphStatusFilters(filter, args))
	if err != nil {
		writeServerError(ctx, w, "handleGraph", err, "failed to list beads")
		return
	}
	beadMap = limitByRelevance(beadMap, args.Limit)

	beadIDs := make([]string, 0, len(beadMap))
	for id := range beadMap {
		beadIDs = append(beadIDs, id)
	}

	// Default include_text_references to true — see § 5b of
	// docs/design-bead-references.md. Callers must set the flag explicitly
	// to false to opt out (ka-0000002463).
	includeTextRefs := args.IncludeTextReferences == nil || *args.IncludeTextReferences

	batch := s.fetchGraphBatch(ctx, beadIDs, includeTextRefs)
	if batch.depErr != nil {
		writeServerError(ctx, w, "handleGraph", batch.depErr, "failed to get dependencies")
		return
	}
	if batch.countErr != nil {
		writeServerError(ctx, w, "handleGraph", batch.countErr, "failed to get dependency counts")
		return
	}

	es := newGraphEdgeSet()
	missingDepIDs := es.addDepEdges(batch, beadMap, args.IncludeDeps)
	if includeTextRefs && batch.refsErr == nil && len(batch.textRefs) > 0 {
		es.addTextRefEdges(batch.textRefs, beadMap)
	}
	edges := es.edges

	// Batch-fetch missing dep targets, then connected closed items.
	gate := newGraphReadGate(args.ExcludeTypes, allowedOIDC, taintGate)
	for _, b := range s.fetchReadableNeighbors(ctx, missingDepIDs, gate) {
		beadMap[b.ID] = b
	}
	if args.IncludeConnectedClosed != nil && *args.IncludeConnectedClosed {
		candidates := connectedClosedCandidates(edges, batch.depRecords, beadMap)
		for _, b := range s.fetchReadableNeighbors(ctx, candidates, gate) {
			if keepConnectedClosed(b, args.MaxAgeDays) {
				beadMap[b.ID] = b
			}
		}
	}

	parentOf := graphParentOf(batch, beadMap)
	promoteParentChildEdges(edges, parentOf)

	nodes := buildGraphNodes(beadMap, parentOf, batch, args.IncludeBody)
	if args.IncludeAgents {
		nodes, edges = s.addGraphAgents(ctx, beadMap, nodes, edges)
	}
	nodes, edges = pruneGraph(nodes, edges)
	if err := s.propagateReadTaintBeads(ctx, graphNodeBeadIDs(nodes)); err != nil {
		writeServerError(ctx, w, "handleGraph", err, "failed to propagate read taint")
		return
	}

	resp := &model.GraphResponse{
		Nodes: nodes,
		Edges: edges,
	}
	if batch.statsErr == nil && batch.graphStats != nil {
		resp.Stats = batch.graphStats
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleGetStats handles GET /v1/stats.
func (s *BeadsServer) handleGetStats(w http.ResponseWriter, r *http.Request) {
	stats, err := s.store.GetStats(r.Context())
	if err != nil {
		writeServerError(r.Context(), w, "handleGetStats", err, "failed to get stats")
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// handleGetReady handles GET /v1/ready.
// Returns beads that are open and have no unsatisfied blocking dependencies.
func (s *BeadsServer) handleGetReady(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	limit = clampListBeadsLimit(limit, 200) // cap at maxListBeadsLimit (ka-0000007023)

	// Enforce cross-project read scope (ka-0000008132): inject the fleet-token
	// project: label (AND) and the OIDC allowed-projects labels (OR) exactly as
	// handleListBeads does, so /v1/ready cannot return full out-of-scope beads.
	filter := s.scopeListFilter(r.Context(), model.BeadFilter{
		Status: []model.Status{model.StatusOpen},
		Sort:   "priority",
		Limit:  limit,
		Fleet:  r.URL.Query().Get("fleet"),
	})
	beads, _, err := s.store.ListBeads(r.Context(), filter)
	if err != nil {
		writeServerError(r.Context(), w, "handleGetReady", err, "failed to list beads")
		return
	}

	// Filter out beads that have unsatisfied blocking dependencies.
	var ready []*model.Bead
	for _, b := range beads {
		deps, err := s.store.GetDependencies(r.Context(), b.ID)
		if err != nil {
			writeServerError(r.Context(), w, "handleGetReady", err, "failed to get dependencies")
			return
		}
		blocked := false
		for _, d := range deps {
			if d.Type == model.DepBlocks {
				// Check if the blocking bead is still active. A blocker that
				// is terminal-for-progression ('closed' OR 'partial') no
				// longer blocks dependents (ka-0000007305 §2.3).
				blocker, err := s.store.GetBead(r.Context(), d.DependsOnID)
				if err != nil {
					continue
				}
				if blocker != nil && !blocker.Status.IsTerminalForProgression() {
					blocked = true
					break
				}
			}
		}
		if !blocked {
			ready = append(ready, b)
		}
	}

	if ready == nil {
		ready = []*model.Bead{}
	}
	if err := s.propagateReadTaintBeads(r.Context(), beadIDsOf(ready)); err != nil {
		writeServerError(r.Context(), w, "handleGetReady", err, "failed to propagate read taint")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"beads": ready,
		"total": len(ready),
	})
}

// beadRelevance returns a sort key for relevance-based ordering.
func beadRelevance(b *model.Bead) int {
	switch b.Status {
	case model.StatusInProgress:
		return 0
	case model.StatusBlocked:
		return 1
	case model.StatusOpen:
		return 2
	case model.StatusDeferred:
		return 3
	case model.StatusPartial:
		// Terminal-for-progression but flags descendant failures — surface it
		// just above clean closures so it stays visible (ka-0000007305 §2.3).
		return 4
	case model.StatusClosed:
		if !b.UpdatedAt.IsZero() && time.Since(b.UpdatedAt) < 7*24*time.Hour {
			return 5
		}
		return 6
	default:
		return 7
	}
}

// graphArgsFromQuery parses GraphArgs from URL query parameters (GET requests).
func graphArgsFromQuery(r *http.Request) model.GraphArgs {
	q := r.URL.Query()
	var args model.GraphArgs

	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			args.Limit = n
		}
	}
	if v := q.Get("assignee"); v != "" {
		args.Assignee = v
	}
	if v := q.Get("fleet"); v != "" {
		args.Fleet = v
	}
	if v := q.Get("query"); v != "" {
		args.Query = v
	}
	if v := q.Get("parent_id"); v != "" {
		args.ParentID = v
	}
	if v := q.Get("max_age_days"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			args.MaxAgeDays = n
		}
	}
	if v := q.Get("include_deps"); v == "true" || v == "1" {
		args.IncludeDeps = true
	}
	if v := q.Get("include_body"); v == "true" || v == "1" {
		args.IncludeBody = true
	}
	if v := q.Get("include_agents"); v == "true" || v == "1" {
		args.IncludeAgents = true
	}
	if v := q.Get("include_connected_closed"); v == "true" || v == "1" {
		t := true
		args.IncludeConnectedClosed = &t
	}
	// include_text_references defaults to true; only set the pointer when
	// the caller explicitly opts out (false/0) so the default-true semantic
	// is preserved when the param is absent.
	if v := q.Get("include_text_references"); v != "" {
		switch v {
		case "false", "0":
			f := false
			args.IncludeTextReferences = &f
		case "true", "1":
			t := true
			args.IncludeTextReferences = &t
		}
	}
	if vs := q["status"]; len(vs) > 0 {
		args.Status = vs
	}
	if vs := q["types"]; len(vs) > 0 {
		args.Types = vs
	}
	if vs := q["exclude_types"]; len(vs) > 0 {
		args.ExcludeTypes = vs
	}
	if vs := q["labels"]; len(vs) > 0 {
		args.Labels = vs
	}
	if vs := q["labels_any"]; len(vs) > 0 {
		args.LabelsAny = vs
	}

	return args
}

// enrichAgentMemory extracts memory_total and memory_avg_strength from an agent
// bead's JSONB fields and sets them on the graph node (ka-0000000124).
func enrichAgentMemory(node *model.GraphNode, fields json.RawMessage) {
	if len(fields) == 0 {
		return
	}
	var f map[string]any
	if err := json.Unmarshal(fields, &f); err != nil {
		return
	}
	if v, ok := f["memory_total"]; ok {
		switch n := v.(type) {
		case float64:
			node.MemoryTotal = int(n)
		case json.Number:
			if i, err := n.Int64(); err == nil {
				node.MemoryTotal = int(i)
			}
		}
	}
	if v, ok := f["memory_avg_strength"]; ok {
		switch n := v.(type) {
		case float64:
			node.MemoryAvgStrength = n
		case json.Number:
			if fv, err := n.Float64(); err == nil {
				node.MemoryAvgStrength = fv
			}
		}
	}
}
