package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// The routes an agent session needs on a runner token (gd-JmUt9B2iLb): each
// family admits the runner inside its project allowlist and refuses it
// outside, and the approval routes stay closed.

func TestRunnerSession_ConfigsReadOnly(t *testing.T) {
	ms, h, token := newRunnerTestServer(t)
	ms.configs["advice:x"] = &model.Config{Key: "advice:x", Value: json.RawMessage(`{"a":1}`)}
	ms.configs["bridge_token:laptop"] = &model.Config{Key: "bridge_token:laptop", Value: json.RawMessage(`"secret"`)}

	if rec := doRunnerBearer(h, http.MethodGet, "/v1/configs/advice:x", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("config read: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRunnerBearer(h, http.MethodGet, "/v1/configs?namespace=advice", token, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "advice:x") {
		t.Fatalf("config list: want 200 holding advice:x, got %d %s", rec.Code, rec.Body.String())
	}
	// A row that holds daemon authority stays an OIDC admin's to read.
	if rec := doRunnerBearer(h, http.MethodGet, "/v1/configs/bridge_token:laptop", token, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("sensitive config read: want 403, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRunnerBearer(h, http.MethodGet, "/v1/configs?namespace=bridge_token", token, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("sensitive config list: want 403, got %d %s", rec.Code, rec.Body.String())
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		rec := doRunnerBearer(h, method, "/v1/configs/advice:x", token, `{"value":{"a":2}}`)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), runnerRouteDeniedMsg) {
			t.Errorf("%s config: want 403 %q, got %d %s", method, runnerRouteDeniedMsg, rec.Code, rec.Body.String())
		}
	}
}

func TestRunnerSession_MeIsTheOwner(t *testing.T) {
	_, h, token := newRunnerTestServer(t)
	rec := doRunnerBearer(h, http.MethodGet, "/v1/me", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/me: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var me meJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.OperatorID != "owner@example.com" || me.Source != whoamiSourceRunner {
		t.Fatalf("GET /v1/me: want the owner via the runner, got %+v", me)
	}
	if me.Unrestricted || !slices.Equal(me.Lands, []meLand{{"pet", tierRunner}}) {
		t.Fatalf("GET /v1/me: want restricted to the allowlist at tierRunner, got %+v", me)
	}
}

// Each allowlisted project is listed once, sorted, at tierRunner: the runner
// carries no groups claim, so it never reports a land role the owner (who
// may be a guest there) does not hold.
func TestRunnerSession_MeListsTheAllowlist(t *testing.T) {
	ms, h, token := newRunnerTestServer(t)
	ms.beadsMu.Lock()
	ms.beads["gd-r1"].Fields = []byte(`{"owner":"oidc:owner@example.com","projects":["pet","alpha","pet"],"state":"online"}`)
	ms.beadsMu.Unlock()
	rec := doRunnerBearer(h, http.MethodGet, "/v1/me", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/me: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var me meJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &me); err != nil {
		t.Fatal(err)
	}
	if me.Unrestricted || !slices.Equal(me.Lands, []meLand{{"alpha", tierRunner}, {"pet", tierRunner}}) {
		t.Fatalf("GET /v1/me: want alpha and pet at tierRunner, got %+v", me)
	}
}

func TestRunnerSession_AgentsByNameInAllowlist(t *testing.T) {
	ms, h, token := newRunnerTestServer(t)
	ms.beadsMu.Lock()
	ms.beads["gd-a1"].Fields = []byte(`{"runtime_id":"gd-r1","agent":"local-laptop"}`)
	ms.beads["gd-a3"] = &model.Bead{ID: "gd-a3", Type: "agent", Title: "fleet-crew", Status: model.StatusOpen,
		Labels: []string{"project:fleet-internal"}, Fields: []byte(`{"agent":"fleet-crew"}`)}
	ms.labels["gd-a3"] = []string{"project:fleet-internal"}
	ms.beadsMu.Unlock()

	if rec := doRunnerBearer(h, http.MethodGet, "/v1/agents-by-name/local-laptop", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("agent in the allowlist: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRunnerBearer(h, http.MethodGet, "/v1/agents-by-name/fleet-crew", token, ""); rec.Code != http.StatusForbidden {
		t.Fatalf("agent in another project: want 403, got %d %s", rec.Code, rec.Body.String())
	}
}

// An escalation or decision is created on POST /v1/beads: in the allowlist it
// is the owner's, outside it the runner is refused.
func TestRunnerSession_EscalationAndDecisionCreate(t *testing.T) {
	_, h, token := newRunnerTestServer(t)
	for _, c := range []struct{ typ, fields string }{
		{"escalation", `{"operation_type":"runner_session_test","operation_spec":{"ask":"own"},"context":"c"}`},
		{"decision", `{"prompt":"which?","options":[{"id":"a","label":"A"},{"id":"b","label":"B"}]}`},
	} {
		body := `{"title":"t","type":"` + c.typ + `","labels":["project:pet"],"fields":` + c.fields + `}`
		rec := doRunnerBearer(h, http.MethodPost, "/v1/beads", token, body)
		if rec.Code != http.StatusCreated {
			t.Fatalf("%s create in the allowlist: want 201, got %d %s", c.typ, rec.Code, rec.Body.String())
		}
		var created model.Bead
		if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
			t.Fatal(err)
		}
		if created.OperatedBy != "oidc:owner@example.com" {
			t.Errorf("%s operated_by: want the runner's owner, got %q", c.typ, created.OperatedBy)
		}
		body = strings.Replace(body, "project:pet", "project:fleet-internal", 1)
		if rec := doRunnerBearer(h, http.MethodPost, "/v1/beads", token, body); rec.Code != http.StatusForbidden {
			t.Errorf("%s create in another project: want 403, got %d %s", c.typ, rec.Code, rec.Body.String())
		}
	}
}

// A decision is read on the bead routes, under the read scope; the decision
// routes stay closed.
func TestRunnerSession_DecisionReadInAllowlist(t *testing.T) {
	ms, h, token := newRunnerTestServer(t)
	ms.beadsMu.Lock()
	for id, project := range map[string]string{"gd-d1": "pet", "gd-d2": "fleet-internal"} {
		ms.beads[id] = &model.Bead{ID: id, Type: model.TypeDecision, Title: "d", Status: model.StatusOpen,
			Labels: []string{"project:" + project}, Fields: []byte(`{"prompt":"p"}`)}
		ms.labels[id] = []string{"project:" + project}
	}
	ms.beadsMu.Unlock()
	if rec := doRunnerBearer(h, http.MethodGet, "/v1/beads/gd-d1", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("decision in the allowlist: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRunnerBearer(h, http.MethodGet, "/v1/beads/gd-d2", token, ""); rec.Code != http.StatusNotFound && rec.Code != http.StatusForbidden {
		t.Fatalf("decision in another project: want hidden, got %d %s", rec.Code, rec.Body.String())
	}
	for _, path := range []string{"/v1/decisions/gd-d1", "/v1/decisions"} {
		if rec := doRunnerBearer(h, http.MethodGet, path, token, ""); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), runnerRouteDeniedMsg) {
			t.Errorf("GET %s: want 403 %q, got %d %s", path, runnerRouteDeniedMsg, rec.Code, rec.Body.String())
		}
	}
}

// Both ends of an edge a runner adds or removes are in its allowlist.
func TestRunnerSession_DependenciesBothEndsAllowlisted(t *testing.T) {
	ms, h, token := newRunnerTestServer(t)
	ms.beadsMu.Lock()
	ms.beads["gd-t1"].CreatedBy = "oidc:owner@example.com"
	ms.beads["gd-t5"] = &model.Bead{ID: "gd-t5", Type: "task", Kind: model.KindIssue, Title: "also pet", Status: model.StatusOpen,
		Labels: []string{"project:pet"}, CreatedBy: "oidc:owner@example.com"}
	ms.labels["gd-t5"] = []string{"project:pet"}
	ms.beadsMu.Unlock()

	add := func(from, to string) *httptest.ResponseRecorder {
		return doRunnerBearer(h, http.MethodPost, "/v1/beads/"+from+"/dependencies", token, `{"depends_on_id":"`+to+`","type":"blocks"}`)
	}
	if rec := add("gd-t1", "gd-t5"); rec.Code != http.StatusCreated {
		t.Fatalf("link inside the allowlist: want 201, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := add("gd-t1", "gd-t2"); rec.Code != http.StatusNotFound {
		t.Fatalf("link to another project: want 404, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := add("gd-t2", "gd-t1"); rec.Code != http.StatusNotFound {
		t.Fatalf("link from another project: want 404, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRunnerBearer(h, http.MethodDelete, "/v1/beads/gd-t1/dependencies?depends_on_id=gd-t5&type=blocks", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("unlink inside the allowlist: want 200, got %d %s", rec.Code, rec.Body.String())
	}

	// Existing cross-project edges, in both directions, survive the runner's
	// unlink: a foreign target unlinks as a missing one, a foreign source is 404.
	ms.deps["gd-t1"] = append(ms.deps["gd-t1"], &model.Dependency{BeadID: "gd-t1", DependsOnID: "gd-t2", Type: model.DependencyType("blocks")})
	ms.deps["gd-t2"] = append(ms.deps["gd-t2"], &model.Dependency{BeadID: "gd-t2", DependsOnID: "gd-t1", Type: model.DependencyType("blocks")})
	rec := doRunnerBearer(h, http.MethodDelete, "/v1/beads/gd-t1/dependencies?depends_on_id=gd-t2&type=blocks", token, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"removed":false`) {
		t.Fatalf("unlink to another project: want removed:false, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRunnerBearer(h, http.MethodDelete, "/v1/beads/gd-t2/dependencies?depends_on_id=gd-t1&type=blocks", token, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unlink from another project: want 404, got %d %s", rec.Code, rec.Body.String())
	}
	for from, to := range map[string]string{"gd-t1": "gd-t2", "gd-t2": "gd-t1"} {
		deps := ms.deps[from]
		if len(deps) != 1 || deps[0].DependsOnID != to {
			t.Errorf("edge %s -> %s: want it kept, deps now %+v", from, to, deps)
		}
	}
}

// Semantic search is bound to the allowlist: the store is asked only for
// beads labelled with one of the runner's projects (LabelsAny, ANDed with the
// caller's own labels), so asking for another project's label finds nothing.
func TestRunnerSession_SemanticSearchInAllowlist(t *testing.T) {
	s, ms, h, _ := newRunnerTestServerBare(t)
	token := runnerTestToken(t, ms)
	s.SetEmbedder(&fakeEmbedder{vec: []float32{1}, name: "m", dims: 1})
	ms.semanticResults = []*model.SemanticResult{}
	for _, body := range []string{`{"query":"x"}`, `{"query":"x","labels":["project:fleet-internal"]}`} {
		rec := doRunnerBearer(h, http.MethodPost, "/v1/search/semantic", token, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("semantic search %s: want 200, got %d %s", body, rec.Code, rec.Body.String())
		}
		if got := ms.lastSemanticFilter.LabelsAny; !slices.Equal(got, []string{"project:pet"}) {
			t.Errorf("semantic search %s: want LabelsAny [project:pet], got %q", body, got)
		}
	}
}

// Approve, reject and resolve are never a runner's, whatever else it may call.
func TestRunnerSession_NoApprovals(t *testing.T) {
	ms, h, token := newRunnerTestServer(t)
	ms.beadsMu.Lock()
	ms.beads["gd-d1"] = &model.Bead{ID: "gd-d1", Type: model.TypeDecision, Title: "d", Status: model.StatusOpen,
		Labels: []string{"project:pet"}, Fields: []byte(`{"prompt":"p"}`)}
	ms.labels["gd-d1"] = []string{"project:pet"}
	ms.beadsMu.Unlock()
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/v1/escalations/gd-e1/approve"},
		{http.MethodPost, "/v1/escalations/gd-e1/reject"},
		{http.MethodPost, "/v1/escalations/gd-e1/can-approve"},
		{http.MethodPost, "/v1/escalations/gd-e1/audience"},
		{http.MethodPost, "/v1/decisions/gd-d1/resolve"},
		{http.MethodPost, "/v1/decisions/gd-d1/cancel"},
	} {
		rec := doRunnerBearer(h, c.method, c.path, token, `{"selected_option":"a"}`)
		if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), runnerRouteDeniedMsg) {
			t.Errorf("%s %s: want 403 %q, got %d %s", c.method, c.path, runnerRouteDeniedMsg, rec.Code, rec.Body.String())
		}
	}
}

// The rest of a session's calls (gd-pJlQEIdlRD): the escalation list and the
// batch read hold only beads in the allowlist.
func TestRunnerSession_EscalationListAndBatchInAllowlist(t *testing.T) {
	ms, h, token := newRunnerTestServer(t)
	ms.beadsMu.Lock()
	ms.beads["gd-e2"] = &model.Bead{ID: "gd-e2", Type: model.TypeEscalation, Title: "foreign", Status: model.StatusOpen,
		Labels: []string{"project:fleet-internal"}, Fields: []byte(`{"operation_type":"x","operation_spec":{}}`)}
	ms.labels["gd-e2"] = []string{"project:fleet-internal"}
	ms.beadsMu.Unlock()

	rec := doRunnerBearer(h, http.MethodGet, "/v1/escalations", token, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"gd-e1"`) || strings.Contains(rec.Body.String(), `"gd-e2"`) {
		t.Fatalf("escalation list: want 200 holding gd-e1 and not gd-e2, got %d %s", rec.Code, rec.Body.String())
	}

	rec = doRunnerBearer(h, http.MethodGet, "/v1/batch/beads?ids=gd-t1,gd-t2", token, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("batch read: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Results []struct {
			ID    string      `json:"id"`
			Bead  *model.Bead `json:"bead"`
			Error string      `json:"error"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	for _, r := range got.Results {
		found[r.ID] = r.Bead != nil
	}
	if !found["gd-t1"] || found["gd-t2"] {
		t.Fatalf("batch read: want gd-t1 returned and gd-t2 not, got %s", rec.Body.String())
	}
}

// Labels go on beads the write gate admits; a project: label is never added
// outside the allowlist, nor removed.
func TestRunnerSession_LabelsInAllowlist(t *testing.T) {
	ms, h, token := newRunnerTestServer(t)
	ms.beadsMu.Lock()
	ms.beads["gd-t1"].CreatedBy = "oidc:owner@example.com"
	ms.beadsMu.Unlock()

	if rec := doRunnerBearer(h, http.MethodPost, "/v1/beads/gd-t1/labels", token, `{"label":"needs-review"}`); rec.Code != http.StatusCreated {
		t.Fatalf("label in the allowlist: want 201, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRunnerBearer(h, http.MethodDelete, "/v1/beads/gd-t1/labels/needs-review", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("unlabel in the allowlist: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRunnerBearer(h, http.MethodPost, "/v1/beads/gd-t2/labels", token, `{"label":"needs-review"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("label in another project: want 404, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := doRunnerBearer(h, http.MethodDelete, "/v1/beads/gd-t2/labels/project:fleet-internal", token, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unlabel in another project: want 404, got %d %s", rec.Code, rec.Body.String())
	}
	rec := doRunnerBearer(h, http.MethodPost, "/v1/beads/gd-t1/labels", token, `{"label":"project:fleet-internal"}`)
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "not allowlisted") {
		t.Fatalf("project label outside the allowlist: want 403, got %d %s", rec.Code, rec.Body.String())
	}
	rec = doRunnerBearer(h, http.MethodDelete, "/v1/beads/gd-t1/labels/project:pet", token, "")
	if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "may not remove a project label") {
		t.Fatalf("project label removed: want 403, got %d %s", rec.Code, rec.Body.String())
	}
	if labels := ms.labels["gd-t1"]; !slices.Contains(labels, "project:pet") || slices.Contains(labels, "project:fleet-internal") {
		t.Fatalf("gd-t1 labels changed project: %q", labels)
	}
}

// A runner satisfies the gates of its own agents only; any other id answers
// as a missing agent.
func TestRunnerSession_GateSatisfyOwnAgentsOnly(t *testing.T) {
	_, h, token := newRunnerTestServer(t)
	if rec := doRunnerBearer(h, http.MethodPost, "/v1/agents/gd-a1/gates/decision/satisfy", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("own agent's gate: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	for _, id := range []string{"gd-a2", "gd-nope", "gd-t1"} {
		if rec := doRunnerBearer(h, http.MethodPost, "/v1/agents/"+id+"/gates/decision/satisfy", token, ""); rec.Code != http.StatusNotFound {
			t.Errorf("gate of %s: want 404, got %d %s", id, rec.Code, rec.Body.String())
		}
	}
}
