package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Cross-project read-authz regression for GET/POST /v1/graph (handleGraph).
//
// handleGraph routes the caller-controlled labels_any parameter into the
// filter. It MUST land in filter.LabelsAnyUser (its own AND-ed EXISTS clause),
// NOT filter.LabelsAny — the latter is reserved for the OIDC allowed-projects
// scope labels that scopeListFilter appends. Folding a caller any-of into
// LabelsAny OR-merges it with the scope labels in the store's single
// `= ANY(...)` clause, so a restricted OIDC user could name a foreign project
// (?labels_any=project:projB) and have its beads unioned back into the result,
// widening past their scope. handleListBeads makes exactly this split for the
// same reason (ka-0000003307). This is the graph member of the ka-0000008132
// read-authz family, whose no-scope-at-all leak was patched earlier but whose
// any-of collision was not.

// seedGraphScopeBeads seeds one open bead in each of projA and projB.
func seedGraphScopeBeads(ms *mockStore) {
	ms.beads["gd-graph-a"] = &model.Bead{
		ID: "gd-graph-a", Title: "projA task", Type: model.TypeTask,
		Kind: model.KindIssue, Status: model.StatusOpen, Fleet: "default",
	}
	ms.labels["gd-graph-a"] = []string{"project:projA"}

	ms.beads["gd-graph-b"] = &model.Bead{
		ID: "gd-graph-b", Title: "projB SECRET task", Type: model.TypeTask,
		Kind: model.KindIssue, Status: model.StatusOpen, Fleet: "default",
	}
	ms.labels["gd-graph-b"] = []string{"project:projB"}
}

func decodeGraph(t *testing.T, rec *httptest.ResponseRecorder) model.GraphResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("graph = %d: %s", rec.Code, rec.Body.String())
	}
	var resp model.GraphResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode graph: %v", err)
	}
	return resp
}

func graphNodeIDs(resp model.GraphResponse) map[string]bool {
	ids := make(map[string]bool, len(resp.Nodes))
	for _, n := range resp.Nodes {
		ids[n.ID] = true
	}
	return ids
}

// A restricted OIDC user (allowed_projects=projA) naming a foreign project in
// labels_any must NOT have projB beads unioned back into the graph.
func TestScopeAuthz_Graph_LabelsAnyCannotWidenOIDCScope(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedGraphScopeBeads(ms)
	makeUserBead(ms, "gd-user-graph", "restricted@example.com", "projA", "crew")

	req := httptest.NewRequest(http.MethodGet, "/v1/graph?labels_any=project:projB", nil)
	ctx := oidcCtx("restricted@example.com", []string{RoleOperator})
	ctx = WithFleet(ctx, "default")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handleGraph(rec, req)

	ids := graphNodeIDs(decodeGraph(t, rec))
	if ids["gd-graph-b"] {
		t.Errorf("cross-project leak: restricted projA user surfaced projB bead gd-graph-b via labels_any=project:projB")
	}
}

// The same via the POST/JSON body path (both decode into GraphArgs.LabelsAny).
func TestScopeAuthz_Graph_LabelsAnyCannotWidenOIDCScope_POST(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedGraphScopeBeads(ms)
	makeUserBead(ms, "gd-user-graph2", "restricted2@example.com", "projA", "crew")

	body, _ := json.Marshal(map[string]any{"labels_any": []string{"project:projB"}})
	req := httptest.NewRequest(http.MethodPost, "/v1/graph", bytes.NewReader(body))
	ctx := oidcCtx("restricted2@example.com", []string{RoleOperator})
	ctx = WithFleet(ctx, "default")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handleGraph(rec, req)

	ids := graphNodeIDs(decodeGraph(t, rec))
	if ids["gd-graph-b"] {
		t.Errorf("cross-project leak (POST): restricted projA user surfaced projB bead gd-graph-b via labels_any=project:projB")
	}
}

// The gate must not over-restrict: an in-scope labels_any still narrows within
// the caller's project, and the in-scope bead remains visible.
func TestScopeAuthz_Graph_LabelsAnyStillNarrowsWithinScope(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedGraphScopeBeads(ms)
	makeUserBead(ms, "gd-user-graph3", "restricted3@example.com", "projA", "crew")

	req := httptest.NewRequest(http.MethodGet, "/v1/graph?labels_any=project:projA", nil)
	ctx := oidcCtx("restricted3@example.com", []string{RoleOperator})
	ctx = WithFleet(ctx, "default")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handleGraph(rec, req)

	ids := graphNodeIDs(decodeGraph(t, rec))
	if !ids["gd-graph-a"] {
		t.Errorf("in-scope projA bead gd-graph-a missing from graph when labels_any=project:projA")
	}
	if ids["gd-graph-b"] {
		t.Errorf("cross-project leak: projB bead gd-graph-b surfaced for restricted projA user")
	}
}

// An unrestricted caller (static/primary token, no OIDC restriction) keeps
// full cross-project graph visibility — labels_any then behaves as a plain
// any-of filter with no scope labels appended.
func TestScopeAuthz_Graph_UnrestrictedSeesAll(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedGraphScopeBeads(ms)

	req := httptest.NewRequest(http.MethodGet, "/v1/graph", nil)
	ctx := withAuthMethod(req.Context(), authMethodStatic)
	ctx = WithFleet(ctx, "default")
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handleGraph(rec, req)

	ids := graphNodeIDs(decodeGraph(t, rec))
	if !ids["gd-graph-a"] || !ids["gd-graph-b"] {
		t.Errorf("unrestricted caller should see both projA and projB beads, got %v", ids)
	}
}
