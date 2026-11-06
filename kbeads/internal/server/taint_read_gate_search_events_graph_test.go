package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// jsonRequest builds an httptest.Request with a JSON-encoded body, matching
// the doJSON helper's encoding but usable when calling a handler method
// directly (bypassing the full mux) so the request context can carry a
// specific auth identity via asOIDC.
func jsonRequest(t *testing.T, method, path string, body any) *http.Request {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// Tests for gd-Jler8Q2wts: extending the taint read gate (ADR
// docs/adr/taints.md §4.3/§5.1) to the surfaces that don't route through
// ListBeads's shared filter — semantic search, events (by-bead and the
// global/feed listing), mutation_history, and graph/dependency-tree
// aggregations. Each surface gets a denied-for-an-ungranted-caller case and a
// granted-caller control proving the gate only narrows.

// --- semantic search --------------------------------------------------

// TestHandleSemanticSearch_TaintDenied_FilteredFromResults pins acceptance
// criterion #3: the raw vector-index hit for a tainted bead exists (an
// unrestricted/exempt caller's search surfaces it), but the served result for
// an ungranted caller does not include it — filtered post-query, since the
// vector index itself cannot express the bead_taints join.
func TestHandleSemanticSearch_TaintDenied_FilteredFromResults(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	s.SetEmbedder(&fakeEmbedder{vec: []float32{0.1, 0.2, 0.3}, name: "test-model", dims: 3})
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["gd-clean"] = &model.Bead{ID: "gd-clean", Kind: model.KindIssue, Type: model.TypeTask, Title: "clean hit"}
	ms.beads["gd-tainted"] = &model.Bead{ID: "gd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "tainted hit"}
	seedBeadTaints(ms, "gd-tainted", 1)
	ms.semanticResults = []*model.SemanticResult{
		{Bead: ms.beads["gd-clean"], Score: 0.91},
		{Bead: ms.beads["gd-tainted"], Score: 0.88},
	}

	// The raw vector-index hit exists: an exempt/unrestricted caller's query
	// (nil TaintGrantIDs, matching a taint-gate-exempt principal) sees both.
	raw, err := s.store.SemanticSearch(context.Background(), []float32{0.1, 0.2, 0.3}, model.BeadFilter{}, 10)
	if err != nil {
		t.Fatalf("raw SemanticSearch: %v", err)
	}
	foundRawHit := false
	for _, r := range raw {
		if r.Bead.ID == "gd-tainted" {
			foundRawHit = true
		}
	}
	if !foundRawHit {
		t.Fatalf("expected the raw vector-index hit for gd-tainted to exist, got %+v", raw)
	}

	// The served result for an ungranted caller must not include it.
	req := jsonRequest(t, http.MethodPost, "/v1/search/semantic", map[string]any{"query": "x"})
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleSemanticSearch(rec, req)

	requireStatus(t, rec, http.StatusOK)
	var resp SemanticSearchResponse
	decodeJSON(t, rec, &resp)
	ids := map[string]bool{}
	for _, r := range resp.Results {
		ids[r.Bead.ID] = true
	}
	if !ids["gd-clean"] {
		t.Errorf("expected gd-clean in served results, got %+v", resp.Results)
	}
	if ids["gd-tainted"] {
		t.Errorf("expected gd-tainted filtered out of served results (raw hit exists, served result must not), got %+v", resp.Results)
	}
}

// TestHandleSemanticSearch_GrantedCaller_Allowed is the control: a caller
// holding the grant sees the tainted bead in results, proving the gate only
// narrows.
func TestHandleSemanticSearch_GrantedCaller_Allowed(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	s.SetEmbedder(&fakeEmbedder{vec: []float32{0.1}, name: "m", dims: 1})
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["gd-tainted"] = &model.Bead{ID: "gd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "tainted hit"}
	seedBeadTaints(ms, "gd-tainted", 1)
	ms.semanticResults = []*model.SemanticResult{{Bead: ms.beads["gd-tainted"], Score: 0.9}}
	ms.operatorAliases = map[string]int64{"oidc\x00insider@example.com": 9}
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{TaintID: 1, PrincipalKind: model.PrincipalOperator, Principal: "9", GrantedBy: 1}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	req := jsonRequest(t, http.MethodPost, "/v1/search/semantic", map[string]any{"query": "x"})
	req = req.WithContext(asOIDC(req.Context(), "insider@example.com"))
	rec := httptest.NewRecorder()

	s.handleSemanticSearch(rec, req)

	requireStatus(t, rec, http.StatusOK)
	var resp SemanticSearchResponse
	decodeJSON(t, rec, &resp)
	if len(resp.Results) != 1 || resp.Results[0].Bead.ID != "gd-tainted" {
		t.Errorf("expected granted caller to see gd-tainted, got %+v", resp.Results)
	}
}

// TestGRPCSemanticSearch_TaintDenied_FilteredFromResults is the gRPC twin.
func TestGRPCSemanticSearch_TaintDenied_FilteredFromResults(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	s.SetEmbedder(&fakeEmbedder{vec: []float32{0.1}, name: "m", dims: 1})
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["gd-clean"] = &model.Bead{ID: "gd-clean", Kind: model.KindIssue, Type: model.TypeTask, Title: "clean"}
	ms.beads["gd-tainted"] = &model.Bead{ID: "gd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "tainted"}
	seedBeadTaints(ms, "gd-tainted", 1)
	ms.semanticResults = []*model.SemanticResult{
		{Bead: ms.beads["gd-clean"], Score: 0.9},
		{Bead: ms.beads["gd-tainted"], Score: 0.8},
	}

	ctx := asOIDC(context.Background(), "outsider@example.com")
	resp, err := s.SemanticSearch(ctx, &beadsv1.SemanticSearchRequest{Query: "x"})
	if err != nil {
		t.Fatalf("SemanticSearch: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range resp.GetResults() {
		ids[r.GetBead().GetId()] = true
	}
	if !ids["gd-clean"] {
		t.Errorf("expected gd-clean in results, got %+v", resp.GetResults())
	}
	if ids["gd-tainted"] {
		t.Errorf("expected gd-tainted filtered out, got %+v", resp.GetResults())
	}
}

// --- events: by-bead-id -------------------------------------------------

// TestHandleGetEvents_TaintDenied_EmptyResult pins acceptance criterion #1:
// an ungranted caller sees an empty events list (200, not 403/404 — §5.3's
// "silently absent from lists") for a tainted bead's events-by-bead endpoint.
func TestHandleGetEvents_TaintDenied_EmptyResult(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["gd-tainted"] = &model.Bead{ID: "gd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "gd-tainted", 1)
	ms.events = append(ms.events, &model.Event{ID: 1, BeadID: "gd-tainted", Topic: "bead.created"})

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/gd-tainted/events", nil)
	req.SetPathValue("id", "gd-tainted")
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleGetEvents(rec, req)

	requireStatus(t, rec, http.StatusOK)
	var out struct {
		Events []*model.Event `json:"events"`
	}
	decodeJSON(t, rec, &out)
	if len(out.Events) != 0 {
		t.Errorf("expected 0 events for an ungranted caller, got %+v", out.Events)
	}
}

// TestHandleGetEvents_GrantedCaller_Allowed is the control.
func TestHandleGetEvents_GrantedCaller_Allowed(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["gd-tainted"] = &model.Bead{ID: "gd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "gd-tainted", 1)
	ms.events = append(ms.events, &model.Event{ID: 1, BeadID: "gd-tainted", Topic: "bead.created"})
	ms.operatorAliases = map[string]int64{"oidc\x00insider@example.com": 9}
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{TaintID: 1, PrincipalKind: model.PrincipalOperator, Principal: "9", GrantedBy: 1}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/gd-tainted/events", nil)
	req.SetPathValue("id", "gd-tainted")
	req = req.WithContext(asOIDC(req.Context(), "insider@example.com"))
	rec := httptest.NewRecorder()

	s.handleGetEvents(rec, req)

	requireStatus(t, rec, http.StatusOK)
	var out struct {
		Events []*model.Event `json:"events"`
	}
	decodeJSON(t, rec, &out)
	if len(out.Events) != 1 {
		t.Errorf("expected 1 event for a granted caller, got %+v", out.Events)
	}
}

// TestGRPCGetEvents_TaintDenied_EmptyResult is the gRPC twin.
func TestGRPCGetEvents_TaintDenied_EmptyResult(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["gd-tainted"] = &model.Bead{ID: "gd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "gd-tainted", 1)
	ms.events = append(ms.events, &model.Event{ID: 1, BeadID: "gd-tainted", Topic: "bead.created"})

	ctx := asOIDC(context.Background(), "outsider@example.com")
	resp, err := s.GetEvents(ctx, &beadsv1.GetEventsRequest{BeadId: "gd-tainted"})
	if err != nil {
		t.Fatalf("GetEvents: %v", err)
	}
	if len(resp.GetEvents()) != 0 {
		t.Errorf("expected 0 events for an ungranted caller, got %+v", resp.GetEvents())
	}
}

// --- mutation_history -----------------------------------------------------

// TestHandleListMutations_TaintDenied_EmptyResult pins acceptance criterion
// #2: a mutation_history row for a tainted bead is invisible to an ungranted
// caller.
func TestHandleListMutations_TaintDenied_EmptyResult(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["gd-tainted"] = &model.Bead{ID: "gd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "gd-tainted", 1)
	if err := ms.RecordMutation(context.Background(), &model.MutationRow{
		BeadID: "gd-tainted", Field: "status", OldValue: "open", NewValue: "closed", MutationType: "field_change",
	}); err != nil {
		t.Fatalf("seed mutation: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/gd-tainted/mutations", nil)
	req.SetPathValue("id", "gd-tainted")
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleListMutations(rec, req)

	requireStatus(t, rec, http.StatusOK)
	var out struct {
		Mutations []*model.MutationRow `json:"mutations"`
	}
	decodeJSON(t, rec, &out)
	if len(out.Mutations) != 0 {
		t.Errorf("expected 0 mutations for an ungranted caller, got %+v", out.Mutations)
	}
}

// TestHandleListMutations_GrantedCaller_Allowed is the control.
func TestHandleListMutations_GrantedCaller_Allowed(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["gd-tainted"] = &model.Bead{ID: "gd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "t"}
	seedBeadTaints(ms, "gd-tainted", 1)
	if err := ms.RecordMutation(context.Background(), &model.MutationRow{
		BeadID: "gd-tainted", Field: "status", OldValue: "open", NewValue: "closed", MutationType: "field_change",
	}); err != nil {
		t.Fatalf("seed mutation: %v", err)
	}
	ms.operatorAliases = map[string]int64{"oidc\x00insider@example.com": 9}
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{TaintID: 1, PrincipalKind: model.PrincipalOperator, Principal: "9", GrantedBy: 1}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/beads/gd-tainted/mutations", nil)
	req.SetPathValue("id", "gd-tainted")
	req = req.WithContext(asOIDC(req.Context(), "insider@example.com"))
	rec := httptest.NewRecorder()

	s.handleListMutations(rec, req)

	requireStatus(t, rec, http.StatusOK)
	var out struct {
		Mutations []*model.MutationRow `json:"mutations"`
	}
	decodeJSON(t, rec, &out)
	if len(out.Mutations) != 1 {
		t.Errorf("expected 1 mutation for a granted caller, got %+v", out.Mutations)
	}
}

// --- events: global/feed listing ------------------------------------------

// TestScopeAuthz_GlobalEvents_ExcludesTaintedBead pins acceptance criterion
// #1's second half: a tainted bead's events are also invisible via the
// global/feed event listing (GET /v1/events), not just the by-bead endpoint.
func TestScopeAuthz_GlobalEvents_ExcludesTaintedBead(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["gd-clean"] = &model.Bead{ID: "gd-clean", Kind: model.KindIssue, Type: model.TypeTask, Title: "clean"}
	ms.beads["gd-tainted"] = &model.Bead{ID: "gd-tainted", Kind: model.KindIssue, Type: model.TypeTask, Title: "tainted"}
	seedBeadTaints(ms, "gd-tainted", 1)
	now := time.Now()
	ms.events = append(ms.events,
		&model.Event{ID: 1, BeadID: "gd-clean", Topic: "bead.created", CreatedAt: now},
		&model.Event{ID: 2, BeadID: "gd-tainted", Topic: "bead.created", CreatedAt: now},
	)

	req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleListGlobalEvents(rec, req)

	requireStatus(t, rec, http.StatusOK)
	var out struct {
		Events []*model.Event `json:"events"`
	}
	decodeJSON(t, rec, &out)
	ids := map[string]bool{}
	for _, e := range out.Events {
		ids[e.BeadID] = true
	}
	if !ids["gd-clean"] {
		t.Errorf("expected gd-clean's event in the global feed, got %+v", out.Events)
	}
	if ids["gd-tainted"] {
		t.Errorf("expected gd-tainted's event excluded from the global feed, got %+v", out.Events)
	}
}

// --- graph/dependency-tree aggregations -----------------------------------

// TestHandleGraph_TaintDenied_NeighbourRedacted pins acceptance criterion #4:
// a tainted bead pulled into the graph as a dependency neighbour (not by the
// base list filter — via the missing-dep-target batch hydration) is redacted
// for an ungranted caller the same way an out-of-project neighbour already
// is.
func TestHandleGraph_TaintDenied_NeighbourRedacted(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["gd-hub"] = &model.Bead{
		ID: "gd-hub", Title: "hub", Type: model.TypeTask, Kind: model.KindIssue,
		Status: model.StatusOpen,
	}
	// Closed, so it's excluded from the base status filter and only reachable
	// through the dependency-edge (missingDepIDs) hydration path.
	ms.beads["gd-tainted-neighbor"] = &model.Bead{
		ID: "gd-tainted-neighbor", Title: "tainted neighbor", Type: model.TypeTask, Kind: model.KindIssue,
		Status: model.StatusClosed,
	}
	seedBeadTaints(ms, "gd-tainted-neighbor", 1)
	if err := ms.AddDependency(context.Background(), &model.Dependency{
		BeadID: "gd-hub", DependsOnID: "gd-tainted-neighbor", Type: model.DepBlocks,
	}); err != nil {
		t.Fatalf("seed dependency: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/graph?include_deps=true", nil)
	req = req.WithContext(asOIDC(req.Context(), "outsider@example.com"))
	rec := httptest.NewRecorder()

	s.handleGraph(rec, req)

	resp := decodeGraph(t, rec)
	ids := graphNodeIDs(resp)
	if !ids["gd-hub"] {
		t.Errorf("expected gd-hub in the graph, got nodes %+v", ids)
	}
	if ids["gd-tainted-neighbor"] {
		t.Errorf("expected gd-tainted-neighbor redacted for an ungranted caller, got nodes %+v", ids)
	}
	for _, n := range resp.Nodes {
		if n.ID == "gd-tainted-neighbor" || n.Title == "tainted neighbor" {
			t.Errorf("tainted neighbor's id/title leaked into graph nodes: %+v", n)
		}
	}
}

// TestHandleGraph_GrantedCaller_NeighbourVisible is the control: a granted
// caller sees the same neighbour, proving the gate only narrows.
func TestHandleGraph_GrantedCaller_NeighbourVisible(t *testing.T) {
	s, ms := newTaintGrantsTestServer()
	seedTaint(ms, 1, "secret", "owner-team")
	ms.beads["gd-hub"] = &model.Bead{
		ID: "gd-hub", Title: "hub", Type: model.TypeTask, Kind: model.KindIssue,
		Status: model.StatusOpen,
	}
	ms.beads["gd-tainted-neighbor"] = &model.Bead{
		ID: "gd-tainted-neighbor", Title: "tainted neighbor", Type: model.TypeTask, Kind: model.KindIssue,
		Status: model.StatusClosed,
	}
	seedBeadTaints(ms, "gd-tainted-neighbor", 1)
	if err := ms.AddDependency(context.Background(), &model.Dependency{
		BeadID: "gd-hub", DependsOnID: "gd-tainted-neighbor", Type: model.DepBlocks,
	}); err != nil {
		t.Fatalf("seed dependency: %v", err)
	}
	ms.operatorAliases = map[string]int64{"oidc\x00insider@example.com": 9}
	if err := ms.GrantTaint(context.Background(), &model.TaintGrant{TaintID: 1, PrincipalKind: model.PrincipalOperator, Principal: "9", GrantedBy: 1}); err != nil {
		t.Fatalf("seed grant: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/graph?include_deps=true", nil)
	req = req.WithContext(asOIDC(req.Context(), "insider@example.com"))
	rec := httptest.NewRecorder()

	s.handleGraph(rec, req)

	ids := graphNodeIDs(decodeGraph(t, rec))
	if !ids["gd-tainted-neighbor"] {
		t.Errorf("expected granted caller to see gd-tainted-neighbor, got nodes %+v", ids)
	}
}
