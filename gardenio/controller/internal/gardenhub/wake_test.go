package gardenhub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// wakeFake records each revive PATCH and audit comment, with the operator the write's context carried. With answer set it
// plays the controller too: a restart request is answered on the bead with that result (cmd/controller farm_restart.go).
type wakeFake struct {
	*fakeBeads
	updates  []beadsapi.UpdateBeadRequest
	ids      []string
	ops      []string
	comments []string // "<author>: <text>"
	failUpd  error
	answer   string
}

func (f *wakeFake) UpdateBead(ctx context.Context, id string, req beadsapi.UpdateBeadRequest) error {
	f.fakeBeads.mu.Lock()
	defer f.fakeBeads.mu.Unlock()
	if f.failUpd != nil {
		return f.failUpd
	}
	f.ids = append(f.ids, id)
	f.updates = append(f.updates, req)
	f.ops = append(f.ops, beadsapi.OperatorFromContext(ctx))
	if stamp, ok := req.Fields[beadsapi.FieldRestartRequestedAt].(string); ok && f.answer != "" && f.beads[id] != nil {
		b := *f.beads[id] // a copy, so a reader holding the old bead never sees its fields change
		b.Fields = map[string]string{beadsapi.FieldRestartAnswered: stamp, beadsapi.FieldRestartResult: f.answer}
		for k, v := range f.beads[id].Fields {
			if _, set := b.Fields[k]; !set {
				b.Fields[k] = v
			}
		}
		f.beads[id] = &b
	}
	return nil
}

func (f *wakeFake) AddComment(_ context.Context, _, author, text string) error {
	f.fakeBeads.mu.Lock()
	defer f.fakeBeads.mu.Unlock()
	f.comments = append(f.comments, author+": "+text)
	return nil
}

func wakeFixture() *wakeFake {
	agent := func(id, status string, fields map[string]string) *beadsapi.BeadDetail {
		if fields["agent"] == "" {
			fields["agent"] = "gardenio-" + id
		}
		return &beadsapi.BeadDetail{ID: id, Title: id, Type: "agent", Status: status, Fields: fields}
	}
	return &wakeFake{fakeBeads: &fakeBeads{beads: map[string]*beadsapi.BeadDetail{
		"gd-down":   agent("gd-down", "in_progress", map[string]string{"agent_state": "failed", "task_id": "gd-task"}),
		"gd-sleep":  agent("gd-sleep", "open", map[string]string{"agent_state": "idle", "wrapup": `{"restart_when":"when gd-x merges"}`, "respawn_count": "2"}),
		"gd-parked": agent("gd-parked", "closed", map[string]string{"agent_state": "dormant", "successor": "same", "next_respawn_at": "2026-10-21T00:00:00Z"}),
		"gd-gone":   agent("gd-gone", "closed", map[string]string{"agent_state": "done"}),
		"gd-run":    agent("gd-run", "in_progress", map[string]string{"agent_state": "working", "last_progress_at": ago(10 * time.Minute)}),
		"gd-stale":  agent("gd-stale", "in_progress", map[string]string{"agent_state": "working", "last_progress_at": ago(2 * time.Hour), "task_id": "gd-task"}),
		"gd-asked": agent("gd-asked", "in_progress", map[string]string{"agent_state": "working", "last_progress_at": ago(2 * time.Hour),
			"restart_requested_at": time.Now().Add(-30 * time.Second).UTC().Format(time.RFC3339Nano)}),
		"gd-stop":     agent("gd-stop", "open", map[string]string{"agent_state": "stopping"}),
		"gd-hopeless": agent("gd-hopeless", "open", map[string]string{"agent_state": "failed", "goal_unachievable": "true"}),
		"gd-spent":    agent("gd-spent", "open", map[string]string{"agent_state": "failed", "respawn_count": "3", "max_respawns": "3"}),
		"gd-task":     {ID: "gd-task", Title: "A task", Type: "task", Status: "open"},
	}}}
}

// ago is the RFC3339 time d before now, as last_progress_at is written.
func ago(d time.Duration) string { return time.Now().Add(-d).UTC().Format(time.RFC3339) }

func wakeHandler(f *wakeFake, can bool) *Handler {
	h := &Handler{Hub: New(newFake(), "/v", nil), BasePath: "/v", Viewer: func(*http.Request) string { return "oidc:op@example.com" },
		CanWake: func(*http.Request) bool { return can }}
	if f != nil {
		h.Beads, h.Agents = f, f
	}
	return h
}

func postWake(t *testing.T, h *Handler, id string, hdr map[string]string) (int, wakeResult) {
	t.Helper()
	mux := http.NewServeMux()
	h.Register(mux)
	mux.HandleFunc("POST /garden/gardener/{id}/wake", h.Wake) // beads-viewer registers it behind its operator gate
	req := httptest.NewRequest("POST", "/garden/gardener/"+id+"/wake", nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	var res wakeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("POST %s: body %q is not JSON: %v", id, rec.Body.String(), err)
	}
	return rec.Code, res
}

// The garden's Wake / Restart sends the one revive `gb wake` and the wake-scanner send (beadsapi.ReviveAgentFields), on
// the operator's own identity, with an audit comment that names them. A closed, parked bead is reopened in the same
// PATCH; an open one keeps its status.
func TestWakeSendsTheSharedRevive(t *testing.T) {
	for _, tc := range []struct {
		id      string
		respawn int
		reopen  bool
	}{
		{"gd-down", 1, false},
		{"gd-sleep", 3, false},
		{"gd-parked", 1, true},
	} {
		f := wakeFixture()
		code, res := postWake(t, wakeHandler(f, true), tc.id, nil)
		if code != http.StatusOK || !res.OK {
			t.Fatalf("%s: %d %+v, want 200 ok", tc.id, code, res)
		}
		if res.Agent != "gardenio-"+tc.id || res.Respawn != tc.respawn || res.Max != beadsapi.DefaultMaxRespawns {
			t.Errorf("%s: answer %+v", tc.id, res)
		}
		if len(f.updates) != 1 || f.ids[0] != tc.id {
			t.Fatalf("%s: writes %v %+v, want one revive of %s", tc.id, f.ids, f.updates, tc.id)
		}
		up := f.updates[0]
		if want := beadsapi.ReviveAgentFields(tc.respawn, beadsapi.WakeReasonManual); !reflect.DeepEqual(up.Fields, want) {
			t.Errorf("%s: fields %v, want the shared revive %v", tc.id, up.Fields, want)
		}
		if (up.Status != nil) != tc.reopen || (tc.reopen && *up.Status != "open") {
			t.Errorf("%s: status %v, want reopen=%v", tc.id, up.Status, tc.reopen)
		}
		if up.Title != nil || up.Assignee != nil || up.Labels != nil {
			t.Errorf("%s: the revive touched more than status and fields: %+v", tc.id, up)
		}
		if f.ops[0] != "oidc:op@example.com" {
			t.Errorf("%s: the write carried operator %q, want the viewer's", tc.id, f.ops[0])
		}
		if len(f.comments) != 1 || !strings.HasPrefix(f.comments[0], "oidc:op@example.com: Restarted from the garden by oidc:op@example.com") {
			t.Errorf("%s: audit comment %q", tc.id, f.comments)
		}
	}
}

// The revive has one shape on every path: what it deletes, what it bumps, the reason it stamps.
func TestReviveAgentFields(t *testing.T) {
	got := beadsapi.ReviveAgentFields(4, beadsapi.WakeReasonManual)
	want := map[string]any{"next_respawn_at": nil, "agent_state": nil, "stop_requested": "", "spawn_task_claim": nil, "respawn_count": "4", "wake_reason": "manual"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReviveAgentFields = %v, want %v", got, want)
	}
}

// Refusals write nothing and say why: a pod still up, an agent stopping or stopped for good, a goal recorded unreachable,
// a spent budget, a bead that is no agent or not there, a viewer who may not, a request from another page.
func TestWakeRefuses(t *testing.T) {
	for _, tc := range []struct {
		id, want string
		code     int
		can      bool
		hdr      map[string]string
	}{
		{"gd-run", "still running (working) and has made progress within the hour", http.StatusConflict, true, nil},
		{"gd-asked", "already being restarted", http.StatusConflict, true, nil},
		{"gd-stale", "Only a gardenio admin", http.StatusForbidden, false, nil},
		{"gd-stop", "is stopping", http.StatusConflict, true, nil},
		{"gd-gone", "stopped for good", http.StatusConflict, true, nil},
		{"gd-hopeless", "goal cannot be reached", http.StatusConflict, true, nil},
		{"gd-spent", "used its 3 restarts. `gb wake --force gardenio-gd-spent`", http.StatusConflict, true, nil},
		{"gd-task", "is not a gardener", http.StatusBadRequest, true, nil},
		{"gd-nope", "There is no gardener gd-nope", http.StatusNotFound, true, nil},
		{"gd-down", "Only a gardenio admin", http.StatusForbidden, false, nil},
		{"gd-down", "from the garden page itself", http.StatusForbidden, true, map[string]string{"Sec-Fetch-Site": "cross-site"}},
	} {
		f := wakeFixture()
		code, res := postWake(t, wakeHandler(f, tc.can), tc.id, tc.hdr)
		if code != tc.code || res.OK || !strings.Contains(res.Error, tc.want) {
			t.Errorf("%s: %d %+v, want %d %q", tc.id, code, res, tc.code, tc.want)
		}
		if len(f.updates) != 0 || len(f.comments) != 0 {
			t.Errorf("%s: a refusal wrote %v %v", tc.id, f.updates, f.comments)
		}
	}
	if code, res := postWake(t, wakeHandler(nil, true), "gd-down", nil); code != http.StatusServiceUnavailable || res.Error == "" {
		t.Errorf("no daemon: %d %+v, want 503", code, res)
	}
	h := wakeHandler(wakeFixture(), true)
	h.Viewer = nil
	if code, _ := postWake(t, h, "gd-down", nil); code != http.StatusUnauthorized {
		t.Errorf("no viewer: %d, want 401", code)
	}
	f := wakeFixture()
	f.failUpd = &beadsapi.APIError{StatusCode: http.StatusForbidden}
	if code, res := postWake(t, wakeHandler(f, true), "gd-down", nil); code != http.StatusForbidden || !strings.Contains(res.Error, "did not let you") {
		t.Errorf("daemon refused: %d %+v, want 403", code, res)
	}
	f.failUpd = errors.New("down")
	if code, _ := postWake(t, wakeHandler(f, true), "gd-down", nil); code != http.StatusBadGateway {
		t.Errorf("daemon down: %d, want 502", code)
	}
}

// A stale gardener, its pod still up, is not revived but restarted in place (gd-LUmZlr6CKM): the route writes the restart
// request on the agent bead, on the operator's identity, and answers with what the controller wrote back there: 200 when
// coop took it, 502 with the controller's reason when not, 202 when no answer came in time. Each leaves an audit comment.
func TestWakeRestartsAStaleGardenerInPlace(t *testing.T) {
	defer func(p, w time.Duration) { restartPoll, restartWait = p, w }(restartPoll, restartWait)
	restartPoll, restartWait = 5*time.Millisecond, 200*time.Millisecond
	for _, tc := range []struct {
		name, answer, err, comment string
		code                       int
		pending                    bool
	}{
		{"taken", beadsapi.RestartResultOK, "", "Restarted in place from the garden by oidc:op@example.com", http.StatusOK, false},
		{"refused", "Its pod is gone, so there is no session to restart.", "gardenio-gd-stale was not restarted. Its pod is gone",
			"did not go through: Its pod is gone", http.StatusBadGateway, false},
		{"unanswered", "", "", "it had not answered within", http.StatusAccepted, true},
	} {
		f := wakeFixture()
		f.answer = tc.answer
		before := time.Now()
		code, res := postWake(t, wakeHandler(f, true), "gd-stale", nil)
		if code != tc.code || res.OK != (tc.err == "") || !res.InPlace || res.Pending != tc.pending || !strings.HasPrefix(res.Error, tc.err) {
			t.Errorf("%s: %d %+v, want %d in place, pending=%v, error %q", tc.name, code, res, tc.code, tc.pending, tc.err)
		}
		if len(f.updates) != 1 || f.ids[0] != "gd-stale" || f.ops[0] != "oidc:op@example.com" {
			t.Fatalf("%s: writes %v %+v by %v, want one request on gd-stale by the viewer", tc.name, f.ids, f.updates, f.ops)
		}
		up := f.updates[0]
		if up.Status != nil || len(up.Fields) != 2 || up.Fields[beadsapi.FieldRestartRequestedBy] != "oidc:op@example.com" {
			t.Errorf("%s: the request %+v, want only the two request fields", tc.name, up)
		}
		if at, err := time.Parse(time.RFC3339Nano, up.Fields[beadsapi.FieldRestartRequestedAt].(string)); err != nil || at.Before(before.Add(-time.Second)) {
			t.Errorf("%s: requested_at %v (%v), want now", tc.name, up.Fields[beadsapi.FieldRestartRequestedAt], err)
		}
		if _, revived := up.Fields[beadsapi.FieldAgentState]; revived {
			t.Errorf("%s: a running gardener's state was cleared: %+v", tc.name, up.Fields)
		}
		if len(f.comments) != 1 || !strings.Contains(f.comments[0], tc.comment) || !strings.HasPrefix(f.comments[0], "oidc:op@example.com: ") {
			t.Errorf("%s: audit comment %q, want one containing %q", tc.name, f.comments, tc.comment)
		}
	}
}

// Which running gardeners are restarted in place: a stale one with no request on its way; one whose request is answered or
// has lapsed may be asked again.
func TestWakeCheckInPlace(t *testing.T) {
	now := time.Now()
	at := func(d time.Duration) string { return now.Add(-d).UTC().Format(time.RFC3339Nano) }
	for _, tc := range []struct {
		name    string
		fields  map[string]string
		inPlace bool
	}{
		{"fresh", map[string]string{"last_progress_at": ago(59 * time.Minute)}, false},
		{"stale", map[string]string{"last_progress_at": ago(61 * time.Minute)}, true},
		{"asked", map[string]string{"last_progress_at": ago(2 * time.Hour), "restart_requested_at": at(time.Minute)}, false},
		{"answered", map[string]string{"last_progress_at": ago(2 * time.Hour), "restart_requested_at": at(time.Minute), "restart_answered": at(time.Minute)}, true},
		{"lapsed", map[string]string{"last_progress_at": ago(2 * time.Hour), "restart_requested_at": at(3 * time.Minute)}, true},
	} {
		tc.fields["agent_state"], tc.fields["agent"] = "yielding", "gardenio-x"
		b := &beadsapi.BeadDetail{ID: "gd-x", Type: "agent", Status: "in_progress", Fields: tc.fields}
		if _, _, _, msg, inPlace := wakeCheck(b, now); inPlace != tc.inPlace || (msg == "") != tc.inPlace {
			t.Errorf("%s: inPlace=%v %q, want inPlace=%v", tc.name, inPlace, msg, tc.inPlace)
		}
	}
}

// The page tells the card whether to offer Wake / Restart (data-wake), only for a viewer who may; the ETag tells the two
// pages apart, and wake.js is inlined before garden.js's card needs it is shown. The workbench's page does the same.
func TestWakePageFlag(t *testing.T) {
	serve := func(path string, can bool) *httptest.ResponseRecorder {
		h := wakeHandler(wakeFixture(), can)
		rec := httptest.NewRecorder()
		h.ServePage(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}
	yes, no := serve("/garden", true), serve("/garden", false)
	if !strings.Contains(yes.Body.String(), ` data-wake="1"`) {
		t.Error("an admin's page lacks data-wake")
	}
	if strings.Contains(no.Body.String(), ` data-wake="1"`) {
		t.Error("a page for a viewer who may not restart carries data-wake")
	}
	if yes.Header().Get("ETag") == no.Header().Get("ETag") {
		t.Error("the page with and without Wake share an ETag")
	}
	if !strings.Contains(inlined(t, yes.Body.String()), "var WAKE = ") {
		t.Error("wake.js is not inlined")
	}
	// The workbench's page (node.html) names page-wake.js (wake.js) for its agent head, to an admin only, and loads it
	// only when a head has a reason (web/commons2.js wake; gd-7DyHdnx8fC).
	yes, no = serve("/garden/c/n/gd-down", true), serve("/garden/c/n/gd-down", false)
	wakeJs := ` data-wake-js="/v` + assetsPath + pageAssets.byName["page-wake.js"].file + `"`
	if b := yes.Body.String(); !strings.Contains(b, ` data-wake="1"`) || !strings.Contains(b, wakeJs) || !strings.Contains(b, `class="wb2-page"`) {
		t.Error("an admin's workbench page lacks data-wake and page-wake.js")
	}
	if b := no.Body.String(); strings.Contains(b, ` data-wake="1"`) || strings.Contains(b, pageAssets.byName["page-wake.js"].file) {
		t.Error("a workbench page for a viewer who may not restart offers Wake")
	}
	if strings.Contains(yes.Body.String(), `src="/v`+assetsPath+pageAssets.byName["page-wake.js"].file) {
		t.Error("the workbench page loads page-wake.js cold")
	}
	if yes.Header().Get("ETag") == no.Header().Get("ETag") {
		t.Error("the workbench page with and without Wake share an ETag")
	}
	if !strings.Contains(pageAssets.byName["page-wake.js"].src, "var WAKE = ") {
		t.Error("page-wake.js is not wake.js")
	}
}

// wake.js's pure half runs under node: which gardeners are offered which verb, the question, the card's lines at each step
// (hidden for a working, parked or starting gardener), and what the viewer is told.
func TestWakeLogicUnderNode(t *testing.T) {
	script := `
var K = require(process.argv[1]), assert = require("assert");
function g(kind, o) { return Object.assign({ id: "gd-a", agent: "gardenio-a", reason: kind ? { kind: kind, text: "x" } : null }, o || {}); }
assert.deepStrictEqual(["sleeping", "down", "stale", "parked", "starting", "purse_empty", ""].map(function (k) { return K.verb(g(k)); }),
  ["Wake", "Restart", "Restart", "", "", "", ""], "sleeping wakes, down and stale restart, nothing else");
assert.strictEqual(K.ask(g("down", { task: "gd-t", task_title: "Fix the fence" })), "Restart gardenio-a? It will resume its task Fix the fence.");
assert.strictEqual(K.ask(g("sleeping", { task: "gd-t" })), "Wake gardenio-a? It will resume its task gd-t.");
assert.strictEqual(K.ask(g("down", { agent: "" })), "Restart gd-a? It will resume where it left off.");
assert.strictEqual(K.ask(g("stale", { task: "gd-t", task_title: "Fix the fence" })),
  "Restart gardenio-a? Its session is interrupted and starts again in the same pod, resuming its task Fix the fence.", "a running session is told it is cut");
assert.strictEqual(K.ask(g("stale")), "Restart gardenio-a? Its session is interrupted and starts again in the same pod, resuming where it left off.");
// the card: hidden without a verb; a button; the question with Yes and Cancel; the wait; the answer
["parked", "starting", ""].forEach(function (k) { assert.deepStrictEqual(K.lines(g(k)), [], "no action for " + (k || "working")); });
assert.deepStrictEqual(K.lines(g("down")), [["wk", [{ text: "Restart…", cls: "btn gwake", id: "gd-a", btn: 1 }]]]);
assert.deepStrictEqual(K.lines(g("stale")), [["wk", [{ text: "Restart…", cls: "btn gwake", id: "gd-a", btn: 1 }]]]);
assert.deepStrictEqual(K.lines(g("sleeping"), { step: "ask" }), [["wk", "Wake gardenio-a? It will resume where it left off."],
  ["wk", [{ text: "Wake", cls: "btn gwake-yes", id: "gd-a", btn: 1 }, { text: "Cancel", cls: "btn gwake-no", id: "gd-a", btn: 1 }]]]);
assert.deepStrictEqual(K.lines(g("down"), { step: "busy" }), [["wk", "Restarting gardenio-a…"]]);
assert.deepStrictEqual(K.lines(g("down"), { ok: true, text: "Restarted." }), [["wk", "Restarted."]], "done: no second button");
assert.deepStrictEqual(K.lines(g("down"), { ok: false, text: "No." }), [["wk err", "No."], ["wk", [{ text: "Restart…", cls: "btn gwake", id: "gd-a", btn: 1 }]]], "refused: say why, offer again");
assert.deepStrictEqual(K.outcome(200, { ok: true }, g("sleeping")), { ok: true, text: "Woken. It shows as starting on the next update." });
assert.deepStrictEqual(K.outcome(200, { ok: true }, g("down")), { ok: true, text: "Restarted. It shows as starting on the next update." });
assert.deepStrictEqual(K.outcome(200, { ok: true, in_place: true }, g("stale")), { ok: true, text: "Restarted. Its session started again in the same pod." });
assert.deepStrictEqual(K.outcome(202, { ok: true, in_place: true, pending: true }, g("stale")),
  { ok: true, text: "Asked. The controller has not confirmed the restart yet; the card shows it once it is back." });
assert.deepStrictEqual(K.outcome(502, { ok: false, error: "gardenio-a was not restarted. Its pod is gone." }, g("stale")),
  { ok: false, text: "gardenio-a was not restarted. Its pod is gone." });
assert.deepStrictEqual(K.outcome(409, { ok: false, error: "gardenio-a is stopping." }, g("down")), { ok: false, text: "gardenio-a is stopping." });
assert.deepStrictEqual(K.outcome(403, null, g("down")), { ok: false, text: "You are not allowed to restart gardeners." }, "the operator gate answers plain text");
assert.deepStrictEqual(K.outcome(500, null, g("down")), { ok: false, text: "That did not go through (500)." });
assert.strictEqual(K.on(), false, "off the page: never offered");
`
	runNodeLogic(t, "wake.js", script)
}
