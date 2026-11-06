package server

import (
	"fmt"
	"net/http"
	"strings"

	"gardenio/kbeads/internal/model"
)

// batchGetMaxIDs caps a single /v1/batch/beads or /v1/batch/dependencies
// request. Shared by both so the two batch surfaces stay in lockstep.
//
// Raised 50 -> 500 (ka-0000005142). The old 50 dated to the endpoint's
// introduction (393a5263e) and was an arbitrary starting value, not a measured
// constraint — the sibling children-by-status endpoint below already allowed
// 100. Measured against a prod-scale daemon, this cost is per-REQUEST, not
// per-row: GET /v1/beads at limit=50 / 500 / 1000 came back in 5.55s / 5.08s /
// 5.10s (interleaved, n=4 each) — returning 20x the rows was free, while each
// additional round-trip costs a flat ~20ms floor regardless of payload size.
// A cap of 50 therefore forced callers into pure overhead: the beads-viewer
// /plans index fetches 4522 plan children, which was 91 round-trips (~1.8s of
// floor) and is 10 at this cap.
//
// The underlying queries are safe at this width: queryGetBeadsByIDs and its
// label hydration both bind ids as a single `= ANY($1::text[])` array param
// (never an IN($1,...,$N) list), so they stay clear of lib/pq's 65535-parameter
// ceiling — the footgun documented in ka-0000002155. 500 rows measured ~1.4MB,
// which keeps a single response comfortably bounded.
//
// Rollout order matters: the server must accept the wider cap BEFORE any client
// ships a matching BatchGetBeadsMaxIDs, so a new client never outruns an old
// daemon during a rolling deploy.
const batchGetMaxIDs = 500

// batchGetDependenciesMaxIDs caps a single
// /v1/batch/dependencies request to keep the underlying IN(...) query bounded.
// Mirrors the cap on /v1/batch/beads.
const batchGetDependenciesMaxIDs = batchGetMaxIDs

// handleBatchGetBeads handles GET /v1/batch/beads?ids=ka-ABC,ka-DEF.
// Returns multiple beads in a single request, reducing HTTP round-trips.
func (s *BeadsServer) handleBatchGetBeads(w http.ResponseWriter, r *http.Request) {
	idsParam := r.URL.Query().Get("ids")
	if idsParam == "" {
		writeError(w, http.StatusBadRequest, "ids parameter is required")
		return
	}

	ids := strings.Split(idsParam, ",")
	if len(ids) > batchGetMaxIDs {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("maximum %d IDs per batch request", batchGetMaxIDs))
		return
	}

	// Trim whitespace from each ID.
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
	}

	// Bulk fetch using a single query.
	beads, err := s.store.GetBeadsByIDs(r.Context(), ids)
	if err != nil {
		writeServerError(r.Context(), w, "handleBatchGetBeads", err, "failed to fetch beads")
		return
	}

	// Enforce cross-project read scope (ka-0000008132): GetBeadsByIDs fetches
	// caller-supplied ids with no project predicate, so drop any bead the caller
	// is not authorized to read under BOTH gates (fleet-token project + OIDC
	// allowed_projects) — the same two-gate check handleGetBead applies. A
	// dropped bead reports "not found" below, matching the response an
	// out-of-scope id already gets from GET /v1/beads/{id} (403 there; the batch
	// endpoint has no per-id status, so the id simply falls through to the
	// not-found branch and never carries content). allowedOIDC is resolved once
	// here, not per row, so the batch does not re-query the caller's user bead.
	// taintGate is the third gate (ADR docs/adr/taints.md §5.1), resolved once;
	// its per-bead taint ids are batch-fetched only when the gate is not
	// exempt, so a zero-config registry never touches bead_taints here.
	allowedOIDC := oidcAllowedProjects(r.Context(), s.store)
	taintGate, err := s.resolveBeadTaintGateState(r.Context())
	if err != nil {
		writeServerError(r.Context(), w, "handleBatchGetBeads", err, "failed to resolve taint grants")
		return
	}
	var taintsByBead map[string][]int64
	if !taintGate.exempt {
		taintsByBead, err = s.store.GetTaintsForBeads(r.Context(), ids)
		if err != nil {
			writeServerError(r.Context(), w, "handleBatchGetBeads", err, "failed to fetch bead taints")
			return
		}
	}

	// Index results by ID for O(1) lookup.
	byID := make(map[string]*model.Bead, len(beads))
	readIDs := make([]string, 0, len(beads))
	for _, b := range beads {
		if !s.beadReadAuthorized(r.Context(), b, allowedOIDC, taintsByBead[b.ID], taintGate) {
			continue
		}
		byID[b.ID] = b
		readIDs = append(readIDs, b.ID)
	}
	if err := s.propagateReadTaintBeads(r.Context(), readIDs); err != nil {
		writeServerError(r.Context(), w, "handleBatchGetBeads", err, "failed to propagate read taint")
		return
	}

	type beadResult struct {
		ID    string      `json:"id"`
		Bead  *model.Bead `json:"bead,omitempty"`
		Error string      `json:"error,omitempty"`
	}

	results := make([]beadResult, len(ids))
	for i, id := range ids {
		results[i].ID = id
		if b, ok := byID[id]; ok {
			results[i].Bead = b
		} else {
			results[i].Error = "not found"
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"results": results,
	})
}

// childrenStatusHistogramMaxParents bounds a single
// /v1/batch/children-by-status request. Sized to cover the project-view
// sequencer's per-page render (capped at 100 epics by
// projectListLimit in beads-viewer); see
// gardenio/plans/project-view-epic-sequencer.md AC-2.
const childrenStatusHistogramMaxParents = 100

// handleBatchChildrenByStatus handles
// GET /v1/batch/children-by-status?parents=ka-AAA,ka-BBB.
//
// For each parent ID, returns a histogram of its parent-child children's
// statuses, e.g.
//
//	{"histograms": {
//	   "ka-AAA": {"open": 3, "in_progress": 1, "closed": 7},
//	   "ka-BBB": {}
//	}}
//
// Parents that have no parent-child children still appear in the response
// with an empty status map so the caller can distinguish "asked but no
// children" from "did not ask". Unknown parent IDs are treated the same as
// "no children" — this endpoint does not validate that each parent exists.
//
// Used by the beads-viewer project-view sequencer to color epic pills in
// O(1) round trips per page render. Cap the parent set with
// childrenStatusHistogramMaxParents to keep the underlying GROUP BY bounded.
func (s *BeadsServer) handleBatchChildrenByStatus(w http.ResponseWriter, r *http.Request) {
	parentsParam := r.URL.Query().Get("parents")
	if parentsParam == "" {
		writeError(w, http.StatusBadRequest, "parents parameter is required")
		return
	}

	rawIDs := strings.Split(parentsParam, ",")
	parentIDs := make([]string, 0, len(rawIDs))
	for _, id := range rawIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		parentIDs = append(parentIDs, id)
	}
	if len(parentIDs) == 0 {
		writeError(w, http.StatusBadRequest, "parents parameter is required")
		return
	}
	if len(parentIDs) > childrenStatusHistogramMaxParents {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("maximum %d parent IDs per request", childrenStatusHistogramMaxParents))
		return
	}

	// Enforce cross-project read scope (ka-0000008132): a parent's child-status
	// histogram is scoped metadata (it leaks the parent's subtree shape — how
	// many children it has and their status split), and this endpoint took
	// caller-supplied parent ids with no project predicate. Resolve which
	// requested parents the caller may read under BOTH gates (fleet-token
	// project + OIDC allowed_projects) and return an empty histogram for any
	// parent outside scope, so the caller cannot enumerate an out-of-scope
	// parent's subtree. Same fix shape as the content-bearing batch siblings
	// handleBatchGetBeads / handleBatchGetDependencies; the authorized set is
	// computed once (single GetBeadsByIDs + one oidcAllowedProjects lookup),
	// not per row.
	authorized := make(map[string]bool, len(parentIDs))
	scopeBeads, err := s.store.GetBeadsByIDs(r.Context(), parentIDs)
	if err != nil {
		writeServerError(r.Context(), w, "handleBatchChildrenByStatus", err,
			"failed to fetch beads for scope check")
		return
	}
	allowedOIDC := oidcAllowedProjects(r.Context(), s.store)
	taintGate, err := s.resolveBeadTaintGateState(r.Context())
	if err != nil {
		writeServerError(r.Context(), w, "handleBatchChildrenByStatus", err, "failed to resolve taint grants")
		return
	}
	var taintsByBead map[string][]int64
	if !taintGate.exempt {
		taintsByBead, err = s.store.GetTaintsForBeads(r.Context(), parentIDs)
		if err != nil {
			writeServerError(r.Context(), w, "handleBatchChildrenByStatus", err, "failed to fetch bead taints")
			return
		}
	}
	for _, b := range scopeBeads {
		if s.beadReadAuthorized(r.Context(), b, allowedOIDC, taintsByBead[b.ID], taintGate) {
			authorized[b.ID] = true
		}
	}

	hist, err := s.store.GetChildrenStatusHistogram(r.Context(), parentIDs)
	if err != nil {
		writeServerError(r.Context(), w, "handleBatchChildrenByStatus", err,
			"failed to fetch children status histogram")
		return
	}

	// Echo every requested parent so the client can rely on a stable shape
	// (presence of the key) without having to reconcile the request set with
	// the response set. Empty map for parents with no children — and for
	// parents the caller is not authorized to read, so an out-of-scope id is
	// indistinguishable from a childless one.
	histograms := make(map[string]map[string]int, len(parentIDs))
	for _, id := range parentIDs {
		if bucket, ok := hist[id]; ok && bucket != nil && authorized[id] {
			histograms[id] = bucket
		} else {
			histograms[id] = map[string]int{}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"histograms": histograms,
	})
}

// handleBatchGetDependencies handles
// GET /v1/batch/dependencies?ids=ka-AAA,ka-BBB&direction=forward|reverse.
//
// For each requested ID, returns the list of dependency edges in the
// requested direction:
//
//   - direction=forward: rows where bead_id = <id> (this bead depends on X).
//     Mirrors GET /v1/beads/{id}/dependencies.
//   - direction=reverse: rows where depends_on_id = <id> (X depends on this
//     bead). Mirrors GET /v1/beads/{id}/dependents.
//
// Response shape:
//
//	{"results": [
//	   {"id": "ka-AAA", "deps": [Dependency, ...]},
//	   {"id": "ka-BBB", "deps": []}
//	]}
//
// Every requested ID appears in results — IDs with no edges land with an
// empty (non-nil) deps array so callers can rely on a stable shape. The
// per-request cap matches /v1/batch/beads.
//
// Used by the beads-viewer constellation expandLevel batched fetch to
// collapse N per-parent GetDependencies round-trips into one (see
// constellation.go expandLevel, ka-0000008009).
func (s *BeadsServer) handleBatchGetDependencies(w http.ResponseWriter, r *http.Request) {
	idsParam := r.URL.Query().Get("ids")
	if idsParam == "" {
		writeError(w, http.StatusBadRequest, "ids parameter is required")
		return
	}

	directionParam := strings.TrimSpace(r.URL.Query().Get("direction"))
	if directionParam == "" {
		directionParam = "forward"
	}
	switch directionParam {
	case "forward", "reverse":
	default:
		writeError(w, http.StatusBadRequest, "direction must be 'forward' or 'reverse'")
		return
	}

	rawIDs := strings.Split(idsParam, ",")
	ids := make([]string, 0, len(rawIDs))
	for _, id := range rawIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		writeError(w, http.StatusBadRequest, "ids parameter is required")
		return
	}
	if len(ids) > batchGetDependenciesMaxIDs {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("maximum %d IDs per batch request", batchGetDependenciesMaxIDs))
		return
	}

	// Enforce cross-project read scope (ka-0000008132): the dep edges of a bead
	// are scoped content (they leak the bead's graph neighborhood), and this
	// endpoint took caller-supplied ids with no project predicate — inconsistent
	// with the now-guarded single GET /v1/beads/{id}/dependencies. Resolve which
	// requested ids the caller may read under BOTH gates (fleet-token project +
	// OIDC allowed_projects) and return an empty edge list for any id outside
	// scope, so the caller cannot enumerate an out-of-scope bead's edges. The
	// authorized-id set is computed once (single GetBeadsByIDs + one
	// oidcAllowedProjects lookup), not per row.
	authorized := make(map[string]bool, len(ids))
	scopeBeads, err := s.store.GetBeadsByIDs(r.Context(), ids)
	if err != nil {
		writeServerError(r.Context(), w, "handleBatchGetDependencies", err,
			"failed to fetch beads for scope check")
		return
	}
	allowedOIDC := oidcAllowedProjects(r.Context(), s.store)
	taintGate, err := s.resolveBeadTaintGateState(r.Context())
	if err != nil {
		writeServerError(r.Context(), w, "handleBatchGetDependencies", err, "failed to resolve taint grants")
		return
	}
	var taintsByBead map[string][]int64
	if !taintGate.exempt {
		taintsByBead, err = s.store.GetTaintsForBeads(r.Context(), ids)
		if err != nil {
			writeServerError(r.Context(), w, "handleBatchGetDependencies", err, "failed to fetch bead taints")
			return
		}
	}
	for _, b := range scopeBeads {
		if s.beadReadAuthorized(r.Context(), b, allowedOIDC, taintsByBead[b.ID], taintGate) {
			authorized[b.ID] = true
		}
	}

	var byID map[string][]*model.Dependency
	if directionParam == "reverse" {
		byID, err = s.store.GetReverseDependenciesForBeads(r.Context(), ids)
	} else {
		byID, err = s.store.GetDependenciesForBeads(r.Context(), ids)
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleBatchGetDependencies", err,
			"failed to fetch dependencies")
		return
	}

	type depResult struct {
		ID   string              `json:"id"`
		Deps []*model.Dependency `json:"deps"`
	}
	results := make([]depResult, len(ids))
	for i, id := range ids {
		results[i].ID = id
		if deps := byID[id]; deps != nil && authorized[id] {
			results[i].Deps = deps
		} else {
			results[i].Deps = []*model.Dependency{}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"results": results,
	})
}
