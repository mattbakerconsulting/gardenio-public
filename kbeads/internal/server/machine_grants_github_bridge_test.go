package server

import (
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// gardenio-machine-github-bridge (gd-tCMXlS9fXW, decision gd-aZrncmZbkD
// "tight"): the bridge updates and closes merge-request beads others filed,
// and stamps only its listed fields on the work beads a PR belongs to. It never
// rewrites or closes a work bead, and nothing reaches another type.
func TestAuthorizeOwnership_Machine_GitHubBridge(t *testing.T) {
	srv, _, _ := testCtx(t)
	bridge := machineCtx("gardenio-github-bridge", "gardenio-machine-github-bridge")
	foreign := func(id string, typ model.BeadType) *model.Bead {
		return &model.Bead{ID: id, Type: typ, Kind: model.KindIssue, CreatedBy: "agent-gd-crew1", Fields: json.RawMessage(`{}`)}
	}

	for _, action := range []string{"update", "close"} {
		if d := srv.authorizeOwnership(bridge, action, foreign("gd-mr1", mergeRequestType)); d != nil {
			t.Errorf("bridge must %s a merge-request bead it did not file, got %+v", action, d)
		}
	}
	for _, c := range []struct {
		action string
		bead   *model.Bead
	}{
		{"update", foreign("gd-t1", model.TypeTask)},
		{"close", foreign("gd-t1", model.TypeTask)},
		{"update", foreign("gd-e1", model.TypeEpic)},
		{"update", foreign("gd-doc1", "doc")},
		{"update", foreign("gd-r1", model.TypeRelease)},
	} {
		d := srv.authorizeOwnership(bridge, c.action, c.bead)
		if d == nil {
			t.Errorf("bridge must be ownership-denied %s on a foreign %s", c.action, c.bead.Type)
			continue
		}
		if d.CallerSubject != "machine:gardenio-github-bridge" {
			t.Errorf("%s %s denial CallerSubject=%q", c.action, c.bead.Type, d.CallerSubject)
		}
	}

	stamp := &updateBeadInput{Fields: json.RawMessage(`{"mr_merge_state":"conflicting","mr_conflict_kind":"code"}`)}
	for _, typ := range []model.BeadType{model.TypeTask, model.TypeBug, model.TypeFeature, model.TypeChore, model.TypeEpic} {
		if !machineMayStampFields(bridge, foreign("gd-w1", typ), stamp) {
			t.Errorf("bridge must stamp its conflict fields on a %s", typ)
		}
	}
	renamed, closed := "Renamed", string(model.StatusClosed)
	for name, in := range map[string]*updateBeadInput{
		"another field": {Fields: json.RawMessage(`{"mr_merge_state":"clean","awaits_release":true}`)},
		"a column":      {Title: &renamed, Fields: json.RawMessage(`{"mr_merge_state":"clean"}`)},
		"a status":      {Status: &closed, Fields: json.RawMessage(`{"mr_merge_state":"clean"}`)},
	} {
		if machineMayStampFields(bridge, foreign("gd-t1", model.TypeTask), in) {
			t.Errorf("%s must not pass the bridge's field grant", name)
		}
	}
	if machineMayStampFields(bridge, foreign("gd-doc1", "doc"), stamp) {
		t.Error("the bridge's field grant must not extend to docs")
	}

	// It writes no admin-gated type.
	for _, c := range []struct{ action, typ string }{
		{"create", "advice"}, {"update", "account"}, {"create", "config"}, {"create", "permission"},
	} {
		if d := srv.authorizeWrite(bridge, c.action, model.BeadType(c.typ), nil); d == nil {
			t.Errorf("github-bridge must not be granted %s/%s", c.action, c.typ)
		}
	}

	// Another machine client gets none of it.
	canary := machineCtx("gardenio-authz-canary", "gardenio-operator")
	if d := srv.authorizeOwnership(canary, "close", foreign("gd-mr1", mergeRequestType)); d == nil {
		t.Error("canary must be ownership-denied on a foreign merge-request bead")
	}
	if machineMayStampFields(canary, foreign("gd-t1", model.TypeTask), stamp) {
		t.Error("canary must not stamp the bridge's fields")
	}
}

// The same grant end to end: the bridge's conflict stamp on an agent's task
// and its close of a merge-request bead `gb mr link` filed are admitted; a
// rewrite of the task and a principal without the role are refused.
func TestMachineGrants_GitHubBridge_HTTP(t *testing.T) {
	h := newMachineHarness(t, []string{"gardenio-github-bridge", "gardenio-authz-canary"})
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	srv.SetFleetAuth(withTestToken(h.fc, machineStaticToken))
	handler := srv.NewHTTPHandler(h.verifier, h.fc)
	seed := func(id string, typ model.BeadType) {
		ms.beads[id] = &model.Bead{
			ID: id, Title: "work " + id, Type: typ, Kind: model.KindIssue, Status: model.StatusOpen,
			CreatedBy: "agent-gd-crew1", OperatedBy: "oidc:owner@example.com",
			Labels: []string{"project:gardenio"}, Fields: json.RawMessage(`{}`),
		}
	}
	seed("gd-task1", model.TypeTask)
	seed("gd-mr1", mergeRequestType)

	bridge := h.saToken(t, "gardenio-github-bridge", "gardenio-machine-github-bridge")
	canary := h.saToken(t, "gardenio-authz-canary", "gardenio-operator")
	stamp := map[string]any{"fields": map[string]any{"mr_merge_state": "conflicting", "mr_url": "https://github.com/o/r/pull/7"}}

	if rec := machineDo(t, handler, canary, http.MethodPatch, "/v1/beads/gd-task1", stamp); rec.Code != http.StatusForbidden {
		t.Fatalf("canary stamp: status=%d body=%s; want 403", rec.Code, rec.Body.String())
	}
	if rec := machineDo(t, handler, bridge, http.MethodPatch, "/v1/beads/gd-task1", stamp); rec.Code != http.StatusOK {
		t.Fatalf("bridge stamp: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := machineDo(t, handler, bridge, http.MethodPatch, "/v1/beads/gd-task1", map[string]any{"title": "Renamed"}); rec.Code != http.StatusForbidden {
		t.Fatalf("bridge rename of a task: status=%d body=%s; want 403", rec.Code, rec.Body.String())
	}
	if rec := machineDo(t, handler, bridge, http.MethodPost, "/v1/beads/gd-task1/close", map[string]any{}); rec.Code != http.StatusForbidden {
		t.Fatalf("bridge close of a task: status=%d body=%s; want 403", rec.Code, rec.Body.String())
	}

	if rec := machineDo(t, handler, canary, http.MethodPost, "/v1/beads/gd-mr1/close", map[string]any{}); rec.Code != http.StatusForbidden {
		t.Fatalf("canary MR close: status=%d body=%s; want 403", rec.Code, rec.Body.String())
	}
	if rec := machineDo(t, handler, bridge, http.MethodPost, "/v1/beads/gd-mr1/close", map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("bridge MR close: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := ms.beads["gd-mr1"].Status; got != model.StatusClosed {
		t.Errorf("MR bead status = %q, want closed", got)
	}
}
