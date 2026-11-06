package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
)

// Cross-project read-authz tests for the by-id / batch / aggregation read
// endpoints gated in ka-0000008132 (sibling of ka-0000003827 / MR !7235, which
// gated the per-bead SUB-RESOURCE handlers). Each surface is checked for a
// cross-project DENIAL under both transports of project scope — an OIDC user
// restricted to projA, and a project-bound fleet token bound to projA — plus a
// same-project / unrestricted ALLOW so the gate does not over-restrict.

// scopeTestBeads seeds one projA bead and one projB bead of the given type.
func scopeTestBeads(ms *mockStore, typ model.BeadType) (aID, bID string) {
	aID, bID = "gd-scope-a", "gd-scope-b"
	ms.beads[aID] = &model.Bead{ID: aID, Title: "A", Type: typ, Kind: "issue", Status: model.StatusOpen, Fleet: "default"}
	ms.beads[bID] = &model.Bead{ID: bID, Title: "B", Type: typ, Kind: "issue", Status: model.StatusOpen, Fleet: "default"}
	ms.labels[aID] = []string{"project:projA"}
	ms.labels[bID] = []string{"project:projB"}
	return aID, bID
}

// oidcProjARequest builds a request authenticated as an OIDC user restricted to
// projA (via a seeded user bead). srv/ms must be the pair under test.
func oidcProjARequest(srv *BeadsServer, ms *mockStore, method, path string) *http.Request {
	makeUserBead(ms, "gd-user-scope", "restricted@example.com", "projA", "crew")
	req := httptest.NewRequest(method, path, nil)
	ctx := oidcCtx("restricted@example.com", []string{RoleOperator})
	ctx = WithFleet(ctx, "default")
	return req.WithContext(ctx)
}

// tokenProjARequest builds a request scoped by a project-bound fleet token to
// projA.
func tokenProjARequest(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	ctx := WithTokenProject(req.Context(), "projA")
	ctx = withAuthMethod(ctx, authMethodStatic)
	ctx = WithFleet(ctx, "default")
	return req.WithContext(ctx)
}

// --- #1 handleGetDecision (GET /v1/decisions/{id}) ---

func TestScopeAuthz_GetDecision_DeniesCrossProjectOIDC(t *testing.T) {
	srv, ms, _ := newTestServer()
	_, bID := scopeTestBeads(ms, model.TypeDecision)

	req := oidcProjARequest(srv, ms, http.MethodGet, "/v1/decisions/"+bID)
	req.SetPathValue("id", bID)
	rec := httptest.NewRecorder()
	srv.handleGetDecision(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing) for cross-project OIDC decision read, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestScopeAuthz_GetDecision_DeniesCrossProjectToken(t *testing.T) {
	srv, ms, _ := newTestServer()
	_, bID := scopeTestBeads(ms, model.TypeDecision)

	req := tokenProjARequest(http.MethodGet, "/v1/decisions/"+bID)
	req.SetPathValue("id", bID)
	rec := httptest.NewRecorder()
	srv.handleGetDecision(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing) for cross-project token decision read, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestScopeAuthz_GetDecision_AllowsInScope(t *testing.T) {
	srv, ms, _ := newTestServer()
	aID, _ := scopeTestBeads(ms, model.TypeDecision)

	req := oidcProjARequest(srv, ms, http.MethodGet, "/v1/decisions/"+aID)
	req.SetPathValue("id", aID)
	rec := httptest.NewRecorder()
	srv.handleGetDecision(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for in-scope decision read, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- #2 handleGetEscalation (GET /v1/escalations/{id}) ---

func TestScopeAuthz_GetEscalation_DeniesCrossProjectOIDC(t *testing.T) {
	srv, ms, _ := newTestServer()
	_, bID := scopeTestBeads(ms, model.TypeEscalation)

	req := oidcProjARequest(srv, ms, http.MethodGet, "/v1/escalations/"+bID)
	req.SetPathValue("id", bID)
	rec := httptest.NewRecorder()
	srv.handleGetEscalation(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing) for cross-project OIDC escalation read, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestScopeAuthz_GetEscalation_DeniesCrossProjectToken(t *testing.T) {
	srv, ms, _ := newTestServer()
	_, bID := scopeTestBeads(ms, model.TypeEscalation)

	req := tokenProjARequest(http.MethodGet, "/v1/escalations/"+bID)
	req.SetPathValue("id", bID)
	rec := httptest.NewRecorder()
	srv.handleGetEscalation(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 (as missing) for cross-project token escalation read, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestScopeAuthz_GetEscalation_AllowsInScope(t *testing.T) {
	srv, ms, _ := newTestServer()
	aID, _ := scopeTestBeads(ms, model.TypeEscalation)

	req := tokenProjARequest(http.MethodGet, "/v1/escalations/"+aID)
	req.SetPathValue("id", aID)
	rec := httptest.NewRecorder()
	srv.handleGetEscalation(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for in-scope escalation read, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- #8 handleListDecisions / #9 handleListEscalations ---

func decodeIssueList(t *testing.T, rec *httptest.ResponseRecorder, key string) []string {
	t.Helper()
	var resp map[string][]struct {
		Issue *model.Bead `json:"issue"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode %s: %v", key, err)
	}
	var ids []string
	for _, row := range resp[key] {
		if row.Issue != nil {
			ids = append(ids, row.Issue.ID)
		}
	}
	return ids
}

func TestScopeAuthz_ListDecisions_FiltersCrossProjectOIDC(t *testing.T) {
	srv, ms, _ := newTestServer()
	aID, bID := scopeTestBeads(ms, model.TypeDecision)

	req := oidcProjARequest(srv, ms, http.MethodGet, "/v1/decisions")
	rec := httptest.NewRecorder()
	srv.handleListDecisions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	ids := decodeIssueList(t, rec, "decisions")
	for _, id := range ids {
		if id == bID {
			t.Fatalf("cross-project decision %s leaked into list: %v", bID, ids)
		}
	}
	if len(ids) != 1 || ids[0] != aID {
		t.Fatalf("expected only in-scope %s, got %v", aID, ids)
	}
}

func TestScopeAuthz_ListDecisions_FiltersCrossProjectToken(t *testing.T) {
	srv, ms, _ := newTestServer()
	aID, bID := scopeTestBeads(ms, model.TypeDecision)

	req := tokenProjARequest(http.MethodGet, "/v1/decisions")
	rec := httptest.NewRecorder()
	srv.handleListDecisions(rec, req)

	ids := decodeIssueList(t, rec, "decisions")
	for _, id := range ids {
		if id == bID {
			t.Fatalf("cross-project decision %s leaked into token list: %v", bID, ids)
		}
	}
	if len(ids) != 1 || ids[0] != aID {
		t.Fatalf("expected only in-scope %s, got %v", aID, ids)
	}
}

func TestScopeAuthz_ListEscalations_FiltersCrossProjectOIDC(t *testing.T) {
	srv, ms, _ := newTestServer()
	aID, bID := scopeTestBeads(ms, model.TypeEscalation)

	req := oidcProjARequest(srv, ms, http.MethodGet, "/v1/escalations")
	rec := httptest.NewRecorder()
	srv.handleListEscalations(rec, req)

	ids := decodeIssueList(t, rec, "escalations")
	for _, id := range ids {
		if id == bID {
			t.Fatalf("cross-project escalation %s leaked into list: %v", bID, ids)
		}
	}
	if len(ids) != 1 || ids[0] != aID {
		t.Fatalf("expected only in-scope %s, got %v", aID, ids)
	}
}

// --- #3 handleBatchGetBeads (GET /v1/batch/beads?ids=) ---

func TestScopeAuthz_BatchGetBeads_DropsCrossProjectOIDC(t *testing.T) {
	srv, ms, _ := newTestServer()
	aID, bID := scopeTestBeads(ms, "task")

	req := oidcProjARequest(srv, ms, http.MethodGet, "/v1/batch/beads?ids="+aID+","+bID)
	rec := httptest.NewRecorder()
	srv.handleBatchGetBeads(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Results []struct {
			ID    string      `json:"id"`
			Bead  *model.Bead `json:"bead"`
			Error string      `json:"error"`
		} `json:"results"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, r := range resp.Results {
		if r.ID == bID && r.Bead != nil {
			t.Fatalf("cross-project bead %s leaked full content in batch: %+v", bID, r.Bead)
		}
		if r.ID == aID && r.Bead == nil {
			t.Fatalf("in-scope bead %s should be returned, got error %q", aID, r.Error)
		}
	}
}

func TestScopeAuthz_BatchGetBeads_DropsCrossProjectToken(t *testing.T) {
	srv, ms, _ := newTestServer()
	aID, bID := scopeTestBeads(ms, "task")

	req := tokenProjARequest(http.MethodGet, "/v1/batch/beads?ids="+aID+","+bID)
	rec := httptest.NewRecorder()
	srv.handleBatchGetBeads(rec, req)

	var resp struct {
		Results []struct {
			ID   string      `json:"id"`
			Bead *model.Bead `json:"bead"`
		} `json:"results"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	for _, r := range resp.Results {
		if r.ID == bID && r.Bead != nil {
			t.Fatalf("cross-project bead %s leaked in token batch", bID)
		}
	}
}

// --- #4 handleBatchGetDependencies (GET /v1/batch/dependencies?ids=) ---

func TestScopeAuthz_BatchGetDependencies_DropsCrossProjectOIDC(t *testing.T) {
	srv, ms, _ := newTestServer()
	aID, bID := scopeTestBeads(ms, "task")
	// Both beads have an outgoing dep edge.
	ms.deps[aID] = []*model.Dependency{{BeadID: aID, DependsOnID: "gd-x", Type: model.DepBlocks}}
	ms.deps[bID] = []*model.Dependency{{BeadID: bID, DependsOnID: "gd-y", Type: model.DepBlocks}}

	req := oidcProjARequest(srv, ms, http.MethodGet, "/v1/batch/dependencies?ids="+aID+","+bID)
	rec := httptest.NewRecorder()
	srv.handleBatchGetDependencies(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Results []struct {
			ID   string              `json:"id"`
			Deps []*model.Dependency `json:"deps"`
		} `json:"results"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, r := range resp.Results {
		if r.ID == bID && len(r.Deps) > 0 {
			t.Fatalf("cross-project bead %s leaked dep edges: %+v", bID, r.Deps)
		}
		if r.ID == aID && len(r.Deps) == 0 {
			t.Fatalf("in-scope bead %s should return its dep edges", aID)
		}
	}
}

// --- children-by-status histogram (GET /v1/batch/children-by-status + gRPC
// GetChildrenStatusHistogram) — last deferred member of ka-0000008132. A
// parent's child-status histogram is scoped metadata (subtree shape), so an
// out-of-scope parent must come back with an empty histogram, never counts. ---

// scopeHistogramBeads seeds a projA parent and a projB parent, each with one
// open parent-child child, so a leak shows up as a non-empty histogram bucket.
func scopeHistogramBeads(ms *mockStore) (aParent, bParent string) {
	aParent, bParent = scopeTestBeads(ms, "epic")
	ms.beads["gd-scope-child-a"] = &model.Bead{ID: "gd-scope-child-a", Type: "task", Kind: "issue", Status: model.StatusOpen, Fleet: "default"}
	ms.beads["gd-scope-child-b"] = &model.Bead{ID: "gd-scope-child-b", Type: "task", Kind: "issue", Status: model.StatusOpen, Fleet: "default"}
	ms.deps["gd-scope-child-a"] = []*model.Dependency{{BeadID: "gd-scope-child-a", DependsOnID: aParent, Type: model.DepParentChild}}
	ms.deps["gd-scope-child-b"] = []*model.Dependency{{BeadID: "gd-scope-child-b", DependsOnID: bParent, Type: model.DepParentChild}}
	return aParent, bParent
}

func TestScopeAuthz_ChildrenByStatus_DropsCrossProjectOIDC(t *testing.T) {
	srv, ms, _ := newTestServer()
	aParent, bParent := scopeHistogramBeads(ms)

	req := oidcProjARequest(srv, ms, http.MethodGet, "/v1/batch/children-by-status?parents="+aParent+","+bParent)
	rec := httptest.NewRecorder()
	srv.handleBatchChildrenByStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Histograms map[string]map[string]int `json:"histograms"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Histograms[bParent]) != 0 {
		t.Fatalf("cross-project parent %s leaked child-status histogram: %v", bParent, resp.Histograms[bParent])
	}
	if resp.Histograms[aParent]["open"] != 1 {
		t.Fatalf("in-scope parent %s should report its child histogram, got %v", aParent, resp.Histograms[aParent])
	}
}

func TestScopeAuthz_ChildrenByStatus_DropsCrossProjectToken(t *testing.T) {
	srv, ms, _ := newTestServer()
	aParent, bParent := scopeHistogramBeads(ms)

	req := tokenProjARequest(http.MethodGet, "/v1/batch/children-by-status?parents="+aParent+","+bParent)
	rec := httptest.NewRecorder()
	srv.handleBatchChildrenByStatus(rec, req)

	var resp struct {
		Histograms map[string]map[string]int `json:"histograms"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	if len(resp.Histograms[bParent]) != 0 {
		t.Fatalf("cross-project parent %s leaked child-status histogram to token: %v", bParent, resp.Histograms[bParent])
	}
	if resp.Histograms[aParent]["open"] != 1 {
		t.Fatalf("in-scope parent %s should report its child histogram, got %v", aParent, resp.Histograms[aParent])
	}
}

func TestScopeAuthz_ChildrenByStatusGRPC_DropsCrossProjectOIDC(t *testing.T) {
	srv, ms, _ := newTestServer()
	aParent, bParent := scopeHistogramBeads(ms)

	makeUserBead(ms, "gd-user-scope", "restricted@example.com", "projA", "crew")
	ctx := oidcCtx("restricted@example.com", []string{RoleOperator})
	ctx = WithFleet(ctx, "default")
	resp, err := srv.GetChildrenStatusHistogram(ctx, &beadsv1.GetChildrenStatusHistogramRequest{
		ParentIds: []string{aParent, bParent},
	})
	if err != nil {
		t.Fatalf("GetChildrenStatusHistogram: %v", err)
	}
	if b := resp.Histograms[bParent]; b != nil && len(b.StatusCounts) != 0 {
		t.Fatalf("cross-project parent %s leaked histogram over gRPC: %v", bParent, b.StatusCounts)
	}
	if a := resp.Histograms[aParent]; a == nil || a.StatusCounts["open"] != 1 {
		t.Fatalf("in-scope parent %s should report its histogram over gRPC, got %v", aParent, a)
	}
}

func TestScopeAuthz_ChildrenByStatusGRPC_DropsCrossProjectToken(t *testing.T) {
	srv, ms, _ := newTestServer()
	aParent, bParent := scopeHistogramBeads(ms)

	ctx := WithTokenProject(context.Background(), "projA")
	ctx = withAuthMethod(ctx, authMethodStatic)
	ctx = WithFleet(ctx, "default")
	resp, err := srv.GetChildrenStatusHistogram(ctx, &beadsv1.GetChildrenStatusHistogramRequest{
		ParentIds: []string{aParent, bParent},
	})
	if err != nil {
		t.Fatalf("GetChildrenStatusHistogram: %v", err)
	}
	if b := resp.Histograms[bParent]; b != nil && len(b.StatusCounts) != 0 {
		t.Fatalf("cross-project parent %s leaked histogram to token over gRPC: %v", bParent, b.StatusCounts)
	}
	if a := resp.Histograms[aParent]; a == nil || a.StatusCounts["open"] != 1 {
		t.Fatalf("in-scope parent %s should report its histogram over gRPC, got %v", aParent, a)
	}
}

// --- #5 handleGraph (GET /v1/graph) ---

func TestScopeAuthz_Graph_FiltersCrossProjectOIDC(t *testing.T) {
	srv, ms, _ := newTestServer()
	aID, bID := scopeTestBeads(ms, "task")

	req := oidcProjARequest(srv, ms, http.MethodGet, "/v1/graph?include_agents=false")
	rec := httptest.NewRecorder()
	srv.handleGraph(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	// The base ListBeads filter must carry the OIDC project scope so the store
	// never returns projB. Assert on the last filter as well as the payload.
	if !containsLabel(ms.lastListFilter.LabelsAny, "project:projA") {
		t.Fatalf("graph base filter missing OIDC project scope: %+v", ms.lastListFilter.LabelsAny)
	}
	var resp model.GraphResponse
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	for _, n := range resp.Nodes {
		if n.ID == bID {
			t.Fatalf("cross-project bead %s leaked as graph node", bID)
		}
	}
	_ = aID
}

func TestScopeAuthz_Graph_InjectsTokenProject(t *testing.T) {
	srv, ms, _ := newTestServer()
	scopeTestBeads(ms, "task")

	req := tokenProjARequest(http.MethodGet, "/v1/graph?include_agents=false")
	rec := httptest.NewRecorder()
	srv.handleGraph(rec, req)

	if !containsLabel(ms.lastListFilter.Labels, "project:projA") {
		t.Fatalf("graph base filter missing token project scope: %+v", ms.lastListFilter.Labels)
	}
}

// --- #6 handleGetReady ---

func TestScopeAuthz_Ready_InjectsScopeOIDC(t *testing.T) {
	srv, ms, _ := newTestServer()
	aID, bID := scopeTestBeads(ms, "task")

	req := oidcProjARequest(srv, ms, http.MethodGet, "/v1/ready")
	rec := httptest.NewRecorder()
	srv.handleGetReady(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var resp struct {
		Beads []*model.Bead `json:"beads"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&resp)
	for _, b := range resp.Beads {
		if b.ID == bID {
			t.Fatalf("cross-project bead %s leaked into /v1/ready", bID)
		}
	}
	_ = aID
}

// --- #10 handleFindSimilar neighbor scope ---

func TestScopeAuthz_FindSimilar_InjectsNeighborScope(t *testing.T) {
	srv, ms, _ := newTestServer()
	aID, _ := scopeTestBeads(ms, "task")
	ms.embeddings = map[string]mockEmbedding{aID: {vec: []float32{0.1, 0.2}, model: "test"}}

	req := oidcProjARequest(srv, ms, http.MethodGet, "/v1/beads/"+aID+"/similar")
	req.SetPathValue("id", aID)
	rec := httptest.NewRecorder()
	srv.handleFindSimilar(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for in-scope source, got %d: %s", rec.Code, rec.Body.String())
	}
	// The neighbor SemanticSearch filter must carry the OIDC project scope.
	if !containsLabel(ms.lastSemanticFilter.LabelsAny, "project:projA") {
		t.Fatalf("find-similar neighbor filter missing OIDC scope: %+v", ms.lastSemanticFilter.LabelsAny)
	}
}

// --- #11 handleSemanticSearch: token project injection ---

func TestScopeAuthz_SemanticSearch_InjectsTokenProject(t *testing.T) {
	srv, ms, _ := newTestServer()
	srv.SetEmbedder(&fakeEmbedder{vec: []float32{0.1, 0.2}, name: "test-model", dims: 2})
	scopeTestBeads(ms, "task")

	body, _ := json.Marshal(SemanticSearchRequest{Query: "x"})
	req := httptest.NewRequest(http.MethodPost, "/v1/search/semantic", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx := WithTokenProject(req.Context(), "projA")
	ctx = withAuthMethod(ctx, authMethodStatic)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handleSemanticSearch(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !containsLabel(ms.lastSemanticFilter.Labels, "project:projA") {
		t.Fatalf("semantic search filter missing token project scope: %+v", ms.lastSemanticFilter.Labels)
	}
}

// --- static-token unrestricted allow: the gate must not touch a primary token ---

func TestScopeAuthz_StaticTokenUnrestricted(t *testing.T) {
	srv, ms, _ := newTestServer()
	_, bID := scopeTestBeads(ms, model.TypeEscalation)

	req := httptest.NewRequest(http.MethodGet, "/v1/escalations/"+bID, nil)
	req.SetPathValue("id", bID)
	ctx := withAuthMethod(req.Context(), authMethodStatic)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handleGetEscalation(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("static/primary token should read any project, got %d: %s", rec.Code, rec.Body.String())
	}
}
