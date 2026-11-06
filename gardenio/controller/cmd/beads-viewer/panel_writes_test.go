package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation"
)

// The server half of the in-garden bead panel's writes (Lands workbench-glass
// spec WB-2..WB-4, WB-12; task gd-PXHQecYeDU): the blocking-edge add/remove
// routes, close with a reason, and quick-spawn's model, JSON answers and land
// role gate.

func depsFake() *fakeDaemon {
	f := actionsFake()
	f.beads["gd-other"] = &beadsapi.BeadDetail{ID: "gd-other", Title: "Other", Type: "task", Status: "open", Labels: []string{"project:gardenio"}}
	f.deps = map[string][]beadsapi.Dependency{}
	return f
}

// reasonFake is actionsFake with a comment store, so a close with a reason can post its comment.
func reasonFake() *fakeDaemon {
	f := actionsFake()
	f.comments = map[string][]beadsapi.Comment{}
	return f
}

func authed(req *http.Request) *http.Request {
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	return req
}

// "Waits on" adds an edge from this bead to the other; "Holds up" the reverse.
// Both are blocks edges attributed to the operator.
func TestAddDep_WaitsAndHolds(t *testing.T) {
	for _, tc := range []struct{ dir, from, to string }{
		{"waits", "gd-act1", "gd-other"},
		{"holds", "gd-other", "gd-act1"},
	} {
		f := depsFake()
		mux := mkServer(t, f)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, authed(actionPost("/b/gd-act1/deps", url.Values{"other": {" gd-other "}, "dir": {tc.dir}}, true)))
		if res := decodeAction(t, w); res != (actionResponse{OK: true, Bead: "gd-act1"}) {
			t.Fatalf("%s: answer %+v (HTTP %d)", tc.dir, res, w.Code)
		}
		if f.addDepCalls != 1 || f.addDepLastBead != tc.from || f.addDepLastDep != tc.to || f.addDepLastType != "blocks" || f.addDepLastBy != "matt@example.com" {
			t.Errorf("%s: AddDependency(%q, %q, %q, %q), want (%q, %q, blocks, matt@example.com)", tc.dir, f.addDepLastBead, f.addDepLastDep, f.addDepLastType, f.addDepLastBy, tc.from, tc.to)
		}
	}
}

// A form that names no edge, or a bead the viewer cannot see, writes nothing
// and says why.
func TestAddDep_Refusals(t *testing.T) {
	for _, tc := range []struct {
		form url.Values
		code int
		msg  string
	}{
		{url.Values{"dir": {"waits"}}, http.StatusBadRequest, "Say which bead."},
		{url.Values{"other": {"gd-act1"}, "dir": {"waits"}}, http.StatusBadRequest, "A bead cannot wait on itself."},
		{url.Values{"other": {"gd-other"}, "dir": {"sideways"}}, http.StatusBadRequest, "Say whether this bead waits on it or holds it up."},
		{url.Values{"other": {"gd-gone"}, "dir": {"waits"}}, http.StatusNotFound, "There is no bead gd-gone, or you cannot see it."},
	} {
		f := depsFake()
		mux := mkServer(t, f)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, authed(actionPost("/b/gd-act1/deps", tc.form, true)))
		res := decodeAction(t, w)
		if res.OK || res.Status != tc.code || res.Error != tc.msg {
			t.Errorf("%v: answer %+v, want %d %q", tc.form, res, tc.code, tc.msg)
		}
		if f.addDepCalls != 0 {
			t.Errorf("%v: a refused form still wrote an edge", tc.form)
		}
	}
	w := httptest.NewRecorder()
	mkServer(t, depsFake()).ServeHTTP(w, actionPost("/b/gd-act1/deps", url.Values{"other": {"gd-other"}, "dir": {"waits"}}, true))
	if res := decodeAction(t, w); res.OK || res.Status != http.StatusUnauthorized {
		t.Errorf("anonymous add: %+v", res)
	}
}

func TestRemoveDep(t *testing.T) {
	f := depsFake()
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authed(actionPost("/b/gd-act1/deps/remove", url.Values{"other": {"gd-other"}, "dir": {"holds"}}, true)))
	if res := decodeAction(t, w); !res.OK {
		t.Fatalf("remove: %+v", res)
	}
	if f.removeDepCalls != 1 || f.removeDepLast != [3]string{"gd-other", "gd-act1", "blocks"} {
		t.Errorf("RemoveDependency %d calls, last %v", f.removeDepCalls, f.removeDepLast)
	}
	f.removeDepErr = &beadsapi.APIError{StatusCode: http.StatusNotFound, Message: "dependency not found"}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, authed(actionPost("/b/gd-act1/deps/remove", url.Values{"other": {"gd-other"}, "dir": {"waits"}}, true)))
	if res := decodeAction(t, w); res.OK || res.Status != http.StatusNotFound || !strings.Contains(res.Error, "dependency not found") {
		t.Errorf("daemon refusal: %+v", res)
	}
}

// A close with a reason records it as the operator's comment first, the way
// kd close --reason does; a close without one posts nothing.
func TestCloseBeadAction_WithReason(t *testing.T) {
	f := reasonFake()
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authed(actionPost("/b/gd-act1/close", url.Values{"reason": {"  Superseded by gd-other.  "}}, true)))
	if res := decodeAction(t, w); !res.OK {
		t.Fatalf("close: %+v", res)
	}
	if f.addCommentCalls != 1 || f.addCommentLastID != "gd-act1" || f.addCommentLastText != "Superseded by gd-other." || f.addCommentLastAuth != "matt@example.com" {
		t.Errorf("reason comment: %d calls, %q by %q on %q", f.addCommentCalls, f.addCommentLastText, f.addCommentLastAuth, f.addCommentLastID)
	}
	if f.closeCalls != 1 || f.beads["gd-act1"].Status != "closed" {
		t.Errorf("close: %d calls, status %q", f.closeCalls, f.beads["gd-act1"].Status)
	}

	f = reasonFake()
	f.addCommentErr = &beadsapi.APIError{StatusCode: http.StatusBadGateway, Message: "down"}
	w = httptest.NewRecorder()
	mkServer(t, f).ServeHTTP(w, authed(actionPost("/b/gd-act1/close", url.Values{"reason": {"why"}}, true)))
	if res := decodeAction(t, w); res.OK || f.closeCalls != 0 {
		t.Errorf("a reason that did not post must not close: %+v, %d closes", res, f.closeCalls)
	}

	f = reasonFake()
	w = httptest.NewRecorder()
	mkServer(t, f).ServeHTTP(w, authed(actionPost("/b/gd-act1/close", nil, true)))
	if res := decodeAction(t, w); !res.OK || f.addCommentCalls != 0 || f.closeCalls != 1 {
		t.Errorf("plain close: %+v, %d comments, %d closes", res, f.addCommentCalls, f.closeCalls)
	}
}

// The panel's quick-spawn: a JSON caller gets the agent bead and name; a model
// alias becomes the agent's model override; an unknown model is refused.
func TestQuickSpawn_JSONAndModel(t *testing.T) {
	f := newFake()
	f.createResp = "gd-newagent"
	f.beads["gd-act1"] = &beadsapi.BeadDetail{ID: "gd-act1", Title: "Fix it", Type: "task", Status: "open"}
	mux := mkServer(t, f)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, authed(actionPost("/quick-spawn", url.Values{"project": {"gardenio"}, "role": {"crew"}, "model": {"Opus"}, "prompt": {"Look into it"}, "task_id": {"gd-act1"}}, true)))
	res := decodeAction(t, w)
	if !res.OK || res.Bead != "gd-newagent" || !strings.HasPrefix(res.Agent, "gardenio-look-into-it-") {
		t.Fatalf("answer %+v", res)
	}
	var fields map[string]string
	if err := json.Unmarshal(f.createLastReq.Fields, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["model"] != "claude-opus-5-5" || fields["task_id"] != "gd-act1" {
		t.Errorf("fields = %v, want model claude-opus-5-5 and task gd-act1", fields)
	}

	f.createCalls = 0
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, authed(actionPost("/quick-spawn", url.Values{"project": {"gardenio"}, "prompt": {"Look"}}, true)))
	if res := decodeAction(t, w); !res.OK {
		t.Fatalf("no model: %+v", res)
	}
	var plain map[string]string
	if err := json.Unmarshal(f.createLastReq.Fields, &plain); err != nil || f.createCalls != 1 {
		t.Fatalf("no model: %d creates, %v", f.createCalls, err)
	}
	if m, ok := plain["model"]; ok {
		t.Errorf("no model chosen, yet the agent pins %q", m)
	}

	f.createCalls = 0
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, authed(actionPost("/quick-spawn", url.Values{"project": {"gardenio"}, "model": {"gpt-9"}, "prompt": {"Look"}}, true)))
	if res := decodeAction(t, w); res.OK || res.Status != http.StatusBadRequest || !strings.Contains(res.Error, "Model must be one of") || !strings.Contains(res.Error, "opus") {
		t.Errorf("unknown model: %+v", res)
	}
	if f.createCalls != 0 {
		t.Error("an unknown model still spawned")
	}

	w = httptest.NewRecorder()
	mux.ServeHTTP(w, authed(actionPost("/quick-spawn", url.Values{"project": {"gardenio"}}, true)))
	if res := decodeAction(t, w); res.OK || res.Status != http.StatusBadRequest || !strings.HasPrefix(res.Error, "Prompt is required") {
		t.Errorf("no prompt: %+v", res)
	}
}

// Spawn rights (Q8c): owner and members of the land may start agents, guests
// and outsiders may not.
func TestQuickSpawn_LandRoleGate(t *testing.T) {
	for _, tc := range []struct {
		role conversation.LandRole
		ok   bool
	}{
		{conversation.LandRoleOwner, true},
		{conversation.LandRoleMember, true},
		{conversation.LandRoleGuest, false},
		{conversation.LandRoleNone, false},
	} {
		f := newFake()
		f.createResp = "gd-newagent"
		mux := mkServer(t, f)
		req := authed(actionPost("/quick-spawn", url.Values{"project": {"claimlayer"}, "prompt": {"Look"}}, true))
		sc := viewerScope{Projects: []string{"claimlayer"}, Roles: map[string]conversation.LandRole{"claimlayer": tc.role}}
		req = req.WithContext(withViewerScope(req.Context(), sc))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		res := decodeAction(t, w)
		if res.OK != tc.ok {
			t.Errorf("%q: answer %+v, want ok=%v", tc.role, res, tc.ok)
		}
		if !tc.ok && (res.Status != http.StatusForbidden || f.createCalls != 0) {
			t.Errorf("%q: refused with %d after %d creates; want 403 and none", tc.role, res.Status, f.createCalls)
		}
	}
}

// The inline form's JSON is the viewer's spawn preferences, the prompt rendered
// against the task, and the model choices.
func TestQuickSpawnInline_JSON(t *testing.T) {
	f := newFake()
	f.beads["gd-aaa"] = &beadsapi.BeadDetail{ID: "gd-aaa", Title: "Noisy alert", Type: "task"}
	f.listResp = &beadsapi.ListBeadsResult{Total: 1, Beads: []*beadsapi.BeadDetail{{ID: "gd-user", Type: "user", Kind: "config", Fields: map[string]string{
		"email":       "matt@example.com",
		"preferences": `{"default_spawn_role":"gardener","default_spawn_prompt_template":"Fix {{title}} ({{id}})"}`,
	}}}}
	mux := mkServer(t, f)
	req := authed(httptest.NewRequest(http.MethodGet, "/quick-spawn-inline?task=gd-aaa&project=kbeads&format=json", nil))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("HTTP %d %q: %s", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	var got quickSpawnForm
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Project != "kbeads" || got.TaskID != "gd-aaa" || got.Role != "gardener" || got.Prompt != "Fix Noisy alert (gd-aaa)" {
		t.Errorf("form = %+v", got)
	}
	if strings.Join(got.Roles, ",") != "crew,gardener,thread" || len(got.Models) == 0 || got.Models[0] != "fable" {
		t.Errorf("choices = roles %v, models %v", got.Roles, got.Models)
	}
}
