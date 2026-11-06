package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Own-actor audit reads (gd-GJzCfUmbQC, operator decision gd-brxrFiZ1Z3:
// own_actor). Audit beads carry no project label, so a project-scoped
// per-agent token could not read any of them, not even the ones recording its
// own 403s. The exemption admits only type=audit whose fields.actor is the
// caller's own identity; a foreign actor's audit bead, another project's
// audit bead and every other label-less type stay hidden.

func seedOwnActorAuditBeads(ms *mockStore) {
	add := func(id string, typ model.BeadType, actor string, labels ...string) {
		var fields json.RawMessage
		if actor != "" {
			fields, _ = json.Marshal(map[string]string{"actor": actor, "action": "update", "outcome": "deny"})
		}
		ms.beads[id] = &model.Bead{ID: id, Title: id, Type: typ, Kind: "data", Status: model.StatusOpen, Fleet: "default", Labels: labels, Fields: fields}
		ms.labels[id] = labels
	}
	// agentProjARequest authenticates as subject "crew-a".
	add("gd-aud-own", "audit", "crew-a", "auto:authz-audit")
	add("gd-aud-foreign", "audit", "crew-b", "auto:authz-audit")
	add("gd-aud-own-projb", "audit", "crew-a", "auto:authz-audit", "project:projB")
	add("gd-task-nolabel", model.TypeTask, "crew-a")
	add("gd-task-a", model.TypeTask, "", "project:projA")
}

func listTotal(t *testing.T, srv *BeadsServer, req *http.Request) int {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.handleListBeads(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("count: status %d body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp.Total
}

func TestAgentToken_ListAudit_SeesOwnActorOnly(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedOwnActorAuditBeads(ms)

	got := listIDs(t, srv, agentProjARequest(http.MethodGet, "/v1/beads?type=audit"))
	if !got["gd-aud-own"] {
		t.Errorf("agent must read its own audit bead, got %v", got)
	}
	if got["gd-aud-foreign"] || got["gd-aud-own-projb"] || got["gd-task-nolabel"] {
		t.Errorf("foreign / other-project / non-audit beads leaked: %v", got)
	}

	// Asking for another actor's entries falls back to the strict predicate.
	got = listIDs(t, srv, agentProjARequest(http.MethodGet, "/v1/beads?type=audit&field=actor=crew-b"))
	if len(got) != 0 {
		t.Errorf("actor=crew-b must return nothing to crew-a, got %v", got)
	}
	got = listIDs(t, srv, agentProjARequest(http.MethodGet, "/v1/beads?type=audit&field=actor=crew-a"))
	if !got["gd-aud-own"] || len(got) != 1 {
		t.Errorf("actor=crew-a must return exactly its own bead, got %v", got)
	}
}

func TestAgentToken_CountAudit_CountsOwnActorOnly(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedOwnActorAuditBeads(ms)

	if n := listTotal(t, srv, agentProjARequest(http.MethodGet, "/v1/beads?type=audit&count_only=true")); n != 1 {
		t.Errorf("audit count for crew-a = %d, want 1 (own bead only)", n)
	}
	if n := listTotal(t, srv, agentProjARequest(http.MethodGet, "/v1/beads?type=audit&field=actor=crew-b&count_only=true")); n != 0 {
		t.Errorf("foreign audit count = %d, want 0", n)
	}
}

func TestAgentToken_ListNonAudit_StaysStrict(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedOwnActorAuditBeads(ms)

	// Only an audit-only list is exempt: untyped and mixed-type lists keep the
	// strict project predicate, so no label-less bead shows up.
	for _, path := range []string{"/v1/beads", "/v1/beads?type=task", "/v1/beads?type=audit,task"} {
		got := listIDs(t, srv, agentProjARequest(http.MethodGet, path))
		if got["gd-aud-own"] || got["gd-aud-foreign"] || got["gd-task-nolabel"] {
			t.Errorf("%s: label-less beads leaked: %v", path, got)
		}
	}
}

func TestNonAgentProjectToken_ListAudit_StaysStrict(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedOwnActorAuditBeads(ms)

	// A project-bound token that is not a per-agent token gains nothing.
	got := listIDs(t, srv, tokenProjARequest(http.MethodGet, "/v1/beads?type=audit"))
	if len(got) != 0 {
		t.Errorf("non-agent project token must not read label-less audit beads: %v", got)
	}
}

func TestAgentToken_GetAudit_ByID(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedOwnActorAuditBeads(ms)

	cases := []struct {
		id   string
		want int
	}{
		{"gd-aud-own", http.StatusOK},
		{"gd-aud-foreign", http.StatusNotFound},
		{"gd-aud-own-projb", http.StatusNotFound},
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

	// The exemption is read-only: the write gate stays strict.
	if beadMatchesTokenProject(agentProjARequest(http.MethodGet, "/").Context(), ms.labels["gd-aud-own"]) {
		t.Error("write gate (beadMatchesTokenProject) must stay strict for an own audit bead")
	}
}

func TestScopeListFilter_AgentAudit_PinsOwnActor(t *testing.T) {
	srv, _, _ := newTestServer()
	ctx := agentProjARequest(http.MethodGet, "/").Context()

	asked := map[string]string{"outcome": "deny"}
	f := srv.scopeListFilter(ctx, model.BeadFilter{Type: []model.BeadType{"audit"}, Fields: asked})
	if f.Fields["actor"] != "crew-a" || f.Fields["outcome"] != "deny" {
		t.Errorf("audit-only agent list must pin actor=crew-a and keep outcome, got %v", f.Fields)
	}
	if len(f.Labels) != 0 || len(f.LabelsOrNoProject) != 1 || f.LabelsOrNoProject[0] != "project:projA" {
		t.Errorf("audit-only agent list: Labels=%v LabelsOrNoProject=%v", f.Labels, f.LabelsOrNoProject)
	}
	if _, ok := asked["actor"]; ok {
		t.Error("scopeListFilter must not write the caller's Fields map")
	}

	f = srv.scopeListFilter(ctx, model.BeadFilter{Type: []model.BeadType{"audit"}, Fields: map[string]string{"actor": "crew-b"}})
	if len(f.LabelsOrNoProject) != 0 || len(f.Labels) != 1 || f.Labels[0] != "project:projA" {
		t.Errorf("foreign-actor audit list must stay strict, got Labels=%v LabelsOrNoProject=%v", f.Labels, f.LabelsOrNoProject)
	}
}
