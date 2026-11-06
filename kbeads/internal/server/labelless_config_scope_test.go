package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Label-less config beads (global claude-instructions, prerun, context) and the
// agent's own label-less type:project bead must stay readable by a
// project-scoped per-agent token (gd-4E1o2Xb8XX): `gb setup` / ResolveConfigBeads
// and `gb prime` auto_assign list them with the agent's own token, and the
// strict project predicate otherwise hides the 63 of 65 config beads that carry
// no project: label. Read-only, agent-token-only; config labelled for another
// project stays hidden, and so does another project's bead.

func seedConfigScopeBeads(ms *mockStore) {
	add := func(id, title string, typ model.BeadType, labels ...string) {
		ms.beads[id] = &model.Bead{ID: id, Title: title, Type: typ, Kind: "config", Status: model.StatusOpen, Fleet: "default", Labels: labels}
		ms.labels[id] = labels
	}
	add("gd-cfg-global", "claude-instructions:global", model.BeadType("config"))
	add("gd-cfg-role", "claude-instructions:role:crew", model.BeadType("config"), "role:crew")
	add("gd-cfg-prerun", "prerun:crew", model.BeadType("config"))
	add("gd-cfg-a", "claude-settings", model.BeadType("config"), "project:projA")
	add("gd-cfg-b", "claude-instructions:projB", model.BeadType("config"), "project:projB")
	add("gd-proj-a", "projA", model.BeadType("project"))
	add("gd-proj-a-prefixed", "Project: projA", model.BeadType("project"))
	add("gd-proj-b", "projB", model.BeadType("project"))
	add("gd-task-nolabel", "task", model.TypeTask)
}

func TestAgentToken_ListConfig_SeesLabelLessAndOwnProject_NotOtherProject(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedConfigScopeBeads(ms)

	for _, path := range []string{"/v1/beads?type=config", "/v1/beads?type=config,advice"} {
		got := listIDs(t, srv, agentProjARequest(http.MethodGet, path))
		for _, id := range []string{"gd-cfg-global", "gd-cfg-role", "gd-cfg-prerun", "gd-cfg-a"} {
			if !got[id] {
				t.Errorf("%s: project-A agent token must read %s, got %v", path, id, got)
			}
		}
		if got["gd-cfg-b"] {
			t.Errorf("%s: project-A agent token must NOT read project-B config, got %v", path, got)
		}
	}
}

func TestAgentToken_ListConfig_OtherTypesAndNonAgentStayStrict(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedConfigScopeBeads(ms)

	// Untyped and mixed-type lists stay strict.
	for _, path := range []string{"/v1/beads", "/v1/beads?type=config,task"} {
		got := listIDs(t, srv, agentProjARequest(http.MethodGet, path))
		if got["gd-cfg-global"] || got["gd-cfg-prerun"] || got["gd-task-nolabel"] || got["gd-cfg-b"] {
			t.Errorf("%s: label-less / other-project beads leaked to a project-A agent token: %v", path, got)
		}
	}

	// A project-bound token that is not a per-agent token keeps the strict predicate.
	got := listIDs(t, srv, tokenProjARequest(http.MethodGet, "/v1/beads?type=config"))
	if got["gd-cfg-global"] || got["gd-cfg-prerun"] || got["gd-cfg-b"] || !got["gd-cfg-a"] {
		t.Errorf("non-agent project token config list not strict: %v", got)
	}
}

func TestAgentToken_ListProject_SeesOnlyOwnProjectBead(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedConfigScopeBeads(ms)

	got := listIDs(t, srv, agentProjARequest(http.MethodGet, "/v1/beads?type=project"))
	if !got["gd-proj-a"] || !got["gd-proj-a-prefixed"] {
		t.Errorf("project-A agent token must read its own project bead, got %v", got)
	}
	if got["gd-proj-b"] {
		t.Errorf("project-A agent token must NOT read project-B's project bead, got %v", got)
	}

	// Asking for another project's title by name must not widen access.
	got = listIDs(t, srv, agentProjARequest(http.MethodGet, "/v1/beads?type=project&title=projB"))
	if got["gd-proj-b"] {
		t.Errorf("title filter widened project scope: %v", got)
	}
}

func TestAgentToken_GetConfigAndProject_ByID(t *testing.T) {
	srv, ms, _ := newTestServer()
	seedConfigScopeBeads(ms)

	cases := []struct {
		id   string
		want int
	}{
		{"gd-cfg-global", http.StatusOK},
		{"gd-cfg-role", http.StatusOK},
		{"gd-cfg-a", http.StatusOK},
		{"gd-cfg-b", http.StatusNotFound},
		{"gd-proj-a", http.StatusOK},
		{"gd-proj-a-prefixed", http.StatusOK},
		{"gd-proj-b", http.StatusNotFound},
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

func TestAgentToken_LabelLessConfig_NotWritable(t *testing.T) {
	_, ms, _ := newTestServer()
	seedConfigScopeBeads(ms)
	req := agentProjARequest(http.MethodPut, "/v1/beads/gd-cfg-global")
	// The exemption is read-only: the write gate keeps the strict predicate.
	if beadMatchesTokenProject(req.Context(), ms.labels["gd-cfg-global"]) {
		t.Error("write gate (beadMatchesTokenProject) must stay strict for label-less config")
	}
}

func TestScopeListFilter_AgentConfigAndProject(t *testing.T) {
	srv, _, _ := newTestServer()
	ctx := agentProjARequest(http.MethodGet, "/").Context()

	f := srv.scopeListFilter(ctx, model.BeadFilter{Type: []model.BeadType{"config"}})
	if len(f.Labels) != 0 || len(f.LabelsOrNoProject) != 1 {
		t.Errorf("config-only agent list: Labels=%v LabelsOrNoProject=%v", f.Labels, f.LabelsOrNoProject)
	}
	f = srv.scopeListFilter(ctx, model.BeadFilter{Type: []model.BeadType{"project"}})
	if len(f.Labels) != 0 || len(f.LabelsOrNoProject) != 1 || len(f.Titles) != 2 {
		t.Errorf("project-only agent list: Labels=%v LabelsOrNoProject=%v Titles=%v", f.Labels, f.LabelsOrNoProject, f.Titles)
	}
	// A title that is not this project's falls back to the strict predicate.
	f = srv.scopeListFilter(ctx, model.BeadFilter{Type: []model.BeadType{"project"}, Titles: []string{"projB"}})
	if len(f.LabelsOrNoProject) != 0 || len(f.Labels) != 1 {
		t.Errorf("foreign title: want strict Labels, got Labels=%v LabelsOrNoProject=%v", f.Labels, f.LabelsOrNoProject)
	}
}
