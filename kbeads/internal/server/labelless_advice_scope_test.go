package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Label-less advice (Global working rules, role-only advice) must stay
// readable by a project-scoped per-agent token (gd-sDjQZ4kgW9): `gb prime`
// lists advice with the agent's own token, and the strict project predicate
// otherwise hides every advice bead that has no project: label. The exemption
// is read-only, advice-only and agent-token-only; a bead labelled for another
// project stays hidden.

// agentProjARequest is a request authenticated as a per-agent fleet token
// (fleet "agent-<bead id>") scoped to projA.
func agentProjARequest(method, path string) *http.Request {
	req := httptest.NewRequest(method, path, nil)
	ctx := WithTokenProject(req.Context(), "projA")
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "fleet", Subject: "crew-a"})
	ctx = WithFleet(ctx, "agent-gd-a1")
	ctx = withAuthMethod(ctx, authMethodStatic)
	return req.WithContext(ctx)
}

func seedAdviceScopeBeads(ms *mockStore) {
	add := func(id string, typ model.BeadType, labels ...string) {
		ms.beads[id] = &model.Bead{ID: id, Title: id, Type: typ, Kind: "data", Status: model.StatusOpen, Fleet: "default", Labels: labels}
		ms.labels[id] = labels
	}
	add("gd-adv-global", model.BeadType("advice"))
	add("gd-adv-role", model.BeadType("advice"), "role:crew")
	add("gd-adv-a", model.BeadType("advice"), "project:projA", "role:crew")
	add("gd-adv-b", model.BeadType("advice"), "project:projB", "role:crew")
	add("gd-task-nolabel", model.TypeTask)
	add("gd-task-a", model.TypeTask, "project:projA")
}

func listIDs(t *testing.T, srv *BeadsServer, req *http.Request) map[string]bool {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleListBeads(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list: status %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Beads []struct {
			ID string `json:"id"`
		} `json:"beads"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string]bool{}
	for _, b := range resp.Beads {
		got[b.ID] = true
	}
	return got
}

func TestAgentToken_ListAdvice_SeesLabelLessAndOwnProject_NotOtherProject(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedAdviceScopeBeads(ms)

	got := listIDs(t, srv, agentProjARequest(http.MethodGet, "/v1/beads?type=advice"))
	for _, id := range []string{"gd-adv-global", "gd-adv-role", "gd-adv-a"} {
		if !got[id] {
			t.Errorf("project-A agent token must read %s, got %v", id, got)
		}
	}
	if got["gd-adv-b"] {
		t.Errorf("project-A agent token must NOT read project-B advice, got %v", got)
	}
}

func TestAgentToken_ListNonAdvice_StaysStrictlyProjectScoped(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedAdviceScopeBeads(ms)

	// Only type=advice is exempt: a label-less task, and an untyped list,
	// stay hidden.
	for _, path := range []string{"/v1/beads?type=task", "/v1/beads", "/v1/beads?type=advice,task"} {
		got := listIDs(t, srv, agentProjARequest(http.MethodGet, path))
		if got["gd-task-nolabel"] || got["gd-adv-global"] || got["gd-adv-role"] || got["gd-adv-b"] {
			t.Errorf("%s: label-less / other-project beads leaked to a project-A agent token: %v", path, got)
		}
		if path == "/v1/beads?type=task" && !got["gd-task-a"] {
			t.Errorf("%s: own-project task must stay visible, got %v", path, got)
		}
	}
}

func TestNonAgentProjectToken_ListAdvice_StaysStrict(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedAdviceScopeBeads(ms)

	// A project-bound token that is NOT a per-agent token (tokenProjARequest:
	// fleet "default") keeps the strict predicate.
	got := listIDs(t, srv, tokenProjARequest(http.MethodGet, "/v1/beads?type=advice"))
	if got["gd-adv-global"] || got["gd-adv-role"] || got["gd-adv-b"] {
		t.Errorf("non-agent project token must not gain label-less advice: %v", got)
	}
	if !got["gd-adv-a"] {
		t.Errorf("own-project advice must stay visible: %v", got)
	}
}

func TestAgentToken_GetAdvice_ByID(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedAdviceScopeBeads(ms)

	cases := []struct {
		id   string
		want int
	}{
		{"gd-adv-global", http.StatusOK},
		{"gd-adv-role", http.StatusOK},
		{"gd-adv-a", http.StatusOK},
		{"gd-adv-b", http.StatusNotFound},
		{"gd-task-nolabel", http.StatusNotFound},
	}
	for _, tc := range cases {
		req := agentProjARequest(http.MethodGet, "/v1/beads/"+tc.id)
		req.SetPathValue("id", tc.id)
		rec := httptest.NewRecorder()
		srv.handleGetBead(rec, req)
		if rec.Code != tc.want {
			t.Errorf("GET %s: status %d, want %d (%s)", tc.id, rec.Code, tc.want, rec.Body.String())
		}
	}
}

func TestAgentToken_LabelLessAdvice_SubresourceReadable_NotWritable(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedAdviceScopeBeads(ms)

	// Sub-resource reads (comments) go through getBeadFiltered.
	req := agentProjARequest(http.MethodGet, "/v1/beads/gd-adv-global/comments")
	req.SetPathValue("id", "gd-adv-global")
	rec := httptest.NewRecorder()
	srv.handleGetComments(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("comments on label-less advice: status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}

	// The exemption is read-only: the write gate keeps the strict predicate.
	if beadMatchesTokenProject(req.Context(), ms.labels["gd-adv-global"]) {
		t.Error("write gate (beadMatchesTokenProject) must stay strict for label-less advice")
	}
}

func TestScopeListFilter_AgentAdvice_UsesLabelsOrNoProject(t *testing.T) {
	srv, _, _ := newTestServer()
	ctx := agentProjARequest(http.MethodGet, "/").Context()

	f := srv.scopeListFilter(ctx, model.BeadFilter{Type: []model.BeadType{"advice"}})
	if len(f.Labels) != 0 || len(f.LabelsOrNoProject) != 1 || f.LabelsOrNoProject[0] != "project:projA" {
		t.Errorf("advice-only agent list: Labels=%v LabelsOrNoProject=%v", f.Labels, f.LabelsOrNoProject)
	}
	for _, types := range [][]model.BeadType{nil, {"task"}, {"advice", "task"}} {
		f = srv.scopeListFilter(ctx, model.BeadFilter{Type: types})
		if len(f.LabelsOrNoProject) != 0 || len(f.Labels) != 1 || f.Labels[0] != "project:projA" {
			t.Errorf("types=%v: want strict Labels, got Labels=%v LabelsOrNoProject=%v", types, f.Labels, f.LabelsOrNoProject)
		}
	}
}
