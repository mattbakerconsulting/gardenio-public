package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// Workbench JSON actions (gd-jgVYg6kH5K): every write route the in-garden
// panels post to answers {"ok":…} JSON when asked, and today's HTML otherwise.

// jsonActionRoute reports whether pattern is one of the write routes that must
// answer JSON: the /b/{id}/* and /gardener/{id}/* POSTs, less the two markdown
// previews (they render a draft, they are not actions).
func jsonActionRoute(pattern string) bool {
	if !strings.HasPrefix(pattern, "POST /b/{id}/") && !strings.HasPrefix(pattern, "POST /gardener/{id}/") {
		return false
	}
	return !strings.HasSuffix(pattern, "/preview")
}

func actionPost(path string, form url.Values, asJSON bool) *http.Request {
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if asJSON {
		req.Header.Set("Accept", "application/json")
	}
	return req
}

func decodeAction(t *testing.T, w *httptest.ResponseRecorder) actionResponse {
	t.Helper()
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json; body=%s", ct, w.Body.String())
	}
	var res actionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &res); err != nil {
		t.Fatalf("body is not an action answer: %v\n%s", err, w.Body.String())
	}
	return res
}

// TestJSONAction_EveryWriteRouteAnswersJSON walks the real route table: a
// JSON caller of every write route gets an action answer, an HTML caller of the
// same request does not. No identity, so each one fails at its own gate or
// handler — the answer's shape is what is under test, not the action.
func TestJSONAction_EveryWriteRouteAnswersJSON(t *testing.T) {
	var routes []string
	for _, p := range registeredViewerRoutes(t).patterns {
		if jsonActionRoute(p) {
			routes = append(routes, p)
		}
	}
	if len(routes) < 30 {
		t.Fatalf("found only %d write routes; the walk is not seeing them: %v", len(routes), routes)
	}
	mux := mkServer(t, newFake())
	for _, p := range routes {
		path := strings.NewReplacer("{id}", "gd-nope", "{taint}", "t1").Replace(strings.TrimPrefix(p, "POST "))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, actionPost(path, nil, true))
		res := decodeAction(t, w)
		if res.OK {
			t.Errorf("%s: an anonymous post went through: %+v", p, res)
			continue
		}
		if res.Status != w.Code || res.Status < 400 || res.Error == "" {
			t.Errorf("%s: answer %+v with HTTP %d; want the status echoed and a message", p, res, w.Code)
		}

		w = httptest.NewRecorder()
		mux.ServeHTTP(w, actionPost(path, nil, false))
		if ct := w.Header().Get("Content-Type"); ct == "application/json" {
			t.Errorf("%s: an HTML caller got JSON: %s", p, w.Body.String())
		}
	}
}

func TestJSONAction_Translation(t *testing.T) {
	cases := []struct {
		name string
		h    http.HandlerFunc
		want actionResponse
		code int
	}{
		{"fragment", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<div>ok</div>")) },
			actionResponse{OK: true, Bead: "gd-x"}, 200},
		{"no content", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) },
			actionResponse{OK: true, Bead: "gd-x"}, 200},
		{"hx-redirect", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("HX-Redirect", "/gd-new")
			w.WriteHeader(http.StatusOK)
		}, actionResponse{OK: true, Bead: "gd-x", Redirect: "/gd-new"}, 200},
		{"see other", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/gardener/gd-x", http.StatusSeeOther)
		},
			actionResponse{OK: true, Bead: "gd-x", Redirect: "/gardener/gd-x"}, 200},
		{"plain error", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "No in-flight run to cancel.", http.StatusConflict)
		},
			actionResponse{Bead: "gd-x", Error: "No in-flight run to cancel.", Status: 409}, 409},
		{"html error", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<p role=alert>boom</p>"))
		}, actionResponse{Bead: "gd-x", Error: "Bad Gateway", Status: 502}, 502},
		{"already json", func(w http.ResponseWriter, r *http.Request) {
			respondAction(w, r, "gd-x", &actionError{Msg: "Decision is already resolved.", Status: http.StatusConflict})
		}, actionResponse{Bead: "gd-x", Error: "Decision is already resolved.", Status: 409}, 409},
	}
	for _, c := range cases {
		mux := http.NewServeMux()
		mux.Handle("POST /b/{id}/act", jsonAction(c.h))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, actionPost("/b/gd-x/act", nil, true))
		if w.Code != c.code {
			t.Errorf("%s: HTTP %d, want %d", c.name, w.Code, c.code)
		}
		if got := decodeAction(t, w); got != c.want {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

// An HTML caller (htmx, a form post, a browser Accept that names text/html) and
// a GET pass through jsonAction and respondAction untouched.
func TestJSONAction_HTMLCallerUntouched(t *testing.T) {
	h := jsonAction(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if respondAction(w, r, "gd-x", nil) {
			return
		}
		w.Header().Set("HX-Trigger", "saved")
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("<div>fragment</div>"))
	}))
	for _, accept := range []string{"", "*/*", "text/html,application/xhtml+xml,application/json;q=0.9"} {
		req := actionPost("/b/gd-x/act", nil, false)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusTeapot || w.Body.String() != "<div>fragment</div>" || w.Header().Get("HX-Trigger") != "saved" {
			t.Errorf("Accept %q: got %d %q, want the handler's own answer", accept, w.Code, w.Body.String())
		}
	}
	get := httptest.NewRequest(http.MethodGet, "/b/gd-x/act", nil)
	get.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, get)
	if w.Code != http.StatusTeapot {
		t.Errorf("GET with a JSON Accept was answered as an action: %d %s", w.Code, w.Body.String())
	}
}

func TestActionFailure(t *testing.T) {
	if err := actionFailure("", http.StatusOK); err != nil {
		t.Errorf("a clean render is a success, got %v", err)
	}
	for _, c := range []struct {
		msg    string
		status int
		want   actionError
	}{
		{"Title cannot be empty.", http.StatusBadRequest, actionError{"Title cannot be empty.", 400}},
		{"Stop spec must be valid JSON.", http.StatusOK, actionError{"Stop spec must be valid JSON.", 400}},
		{"", http.StatusBadGateway, actionError{"Bad Gateway", 502}},
	} {
		got, ok := actionFailure(c.msg, c.status).(*actionError)
		if !ok || *got != c.want {
			t.Errorf("actionFailure(%q, %d) = %+v, want %+v", c.msg, c.status, got, c.want)
		}
	}
}

// Decision resolve, both branches: the success, the 409 race and the empty form
// carry the same status, and JSON carries the banner's words.
func TestResolveDecision_HTMLAndJSON(t *testing.T) {
	resolve := func(asJSON bool, form url.Values, closed bool) (*httptest.ResponseRecorder, *fakeDaemon) {
		f := decisionFake()
		if closed {
			f.beads["gd-dec1"].Status = "closed"
			f.beads["gd-dec1"].Fields["chosen"] = "jwt"
		}
		f.resolveOnSuccess = func(fd *fakeDaemon, id string, _ beadsapi.ResolveDecisionRequest) { fd.beads[id].Status = "closed" }
		req := actionPost("/b/gd-dec1/decision/resolve", form, asJSON)
		req.Header.Set("X-Auth-Request-Email", "matt@example.com")
		w := httptest.NewRecorder()
		mkServer(t, f).ServeHTTP(w, req)
		return w, f
	}
	pick := url.Values{"option_id": {"jwt"}}

	w, f := resolve(true, pick, false)
	if res := decodeAction(t, w); w.Code != 200 || res != (actionResponse{OK: true, Bead: "gd-dec1"}) || f.resolveCalls != 1 {
		t.Errorf("JSON resolve: %d %+v (resolve calls %d)", w.Code, res, f.resolveCalls)
	}
	if f.resolveLastReq.RespondedBy != "matt@example.com" {
		t.Errorf("JSON resolve attributed to %q, want the session's identity", f.resolveLastReq.RespondedBy)
	}
	w, _ = resolve(false, pick, false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `<section class="decision-card"`) {
		t.Errorf("HTML resolve: %d, want the decision card fragment", w.Code)
	}

	w, f = resolve(true, pick, true)
	if res := decodeAction(t, w); w.Code != 409 || res.OK || res.Status != 409 || res.Error != "Decision is already resolved." || f.resolveCalls != 0 {
		t.Errorf("JSON race: %d %+v", w.Code, res)
	}
	w, _ = resolve(false, pick, true)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "already resolved") || !strings.Contains(w.Body.String(), "banner-resolved") {
		t.Errorf("HTML race: %d, want the re-rendered card with its banner", w.Code)
	}

	w, _ = resolve(true, url.Values{}, false)
	if res := decodeAction(t, w); w.Code != 400 || res.Error != "Pick an option or write a response." {
		t.Errorf("JSON empty form: %d %+v", w.Code, res)
	}
}

// The bead toolbar actions (close here) and the inline editors (title here)
// answer JSON with the toolbar's/editor's own words.
func TestBeadActions_HTMLAndJSON(t *testing.T) {
	f := actionsFake()
	mux := mkServer(t, f)
	req := actionPost("/b/gd-act1/close", nil, true)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if res := decodeAction(t, w); res != (actionResponse{OK: true, Bead: "gd-act1"}) {
		t.Errorf("JSON close: %+v", res)
	}
	if f.beads["gd-act1"].Status != "closed" {
		t.Errorf("JSON close did not close: status %q", f.beads["gd-act1"].Status)
	}

	for _, asJSON := range []bool{false, true} {
		req = actionPost("/b/gd-act1/title", url.Values{"title": {"   "}}, asJSON)
		req.Header.Set("X-Auth-Request-Email", "matt@example.com")
		w = httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("empty title (json=%v): HTTP %d, want 400", asJSON, w.Code)
		}
		if asJSON {
			if res := decodeAction(t, w); res.Error != "Title cannot be empty." || res.Status != 400 {
				t.Errorf("JSON empty title: %+v", res)
			}
		} else if !strings.Contains(w.Body.String(), "Title cannot be empty.") {
			t.Errorf("HTML empty title lost its inline banner: %s", w.Body.String())
		}
	}
}

// A comment's validation message reaches a JSON caller (the chore tour's ping
// posts through answer.js).
func TestAddComment_JSONError(t *testing.T) {
	req := actionPost("/b/gd-act1/comments", url.Values{"text": {""}}, true)
	req.Header.Set("X-Auth-Request-Email", "matt@example.com")
	w := httptest.NewRecorder()
	mkServer(t, actionsFake()).ServeHTTP(w, req)
	if res := decodeAction(t, w); w.Code != 400 || res.Error != "Comment cannot be empty." {
		t.Errorf("JSON empty comment: %d %+v", w.Code, res)
	}
}

// Gardener POSTs: a plain-text http.Error and a 200-rendered inline form error
// both come back as failures; a success is ok.
func TestGardenerActions_HTMLAndJSON(t *testing.T) {
	f := seedGardenerOpsFake(t)
	mux := mkServer(t, f)

	req := opsReq("POST", "/gardener/gd-cap/disable", nil)
	req.Header.Set("Accept", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if res := decodeAction(t, w); res != (actionResponse{OK: true, Bead: "gd-cap"}) || f.updateFieldsLastVal["enabled"] != false {
		t.Errorf("JSON disable: %+v (enabled=%v)", res, f.updateFieldsLastVal["enabled"])
	}

	req = opsReq("POST", "/gardener/gd-cap/mode", url.Values{"mode": {"sometimes"}})
	req.Header.Set("Accept", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if res := decodeAction(t, w); w.Code != 400 || res.Error != "mode must be timed or event" {
		t.Errorf("JSON bad mode: %d %+v", w.Code, res)
	}

	bad := url.Values{"terminal_goal": {"x"}, "stop_json": {"{not json"}}
	req = opsReq("POST", "/gardener/gd-cap/config", bad)
	req.Header.Set("Accept", "application/json")
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if res := decodeAction(t, w); w.Code != 400 || res.OK || !strings.Contains(res.Error, "valid JSON") {
		t.Errorf("JSON bad stop spec: %d %+v", w.Code, res)
	}
	w = httptest.NewRecorder()
	mux.ServeHTTP(w, opsReq("POST", "/gardener/gd-cap/config", bad))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "valid JSON") {
		t.Errorf("HTML bad stop spec: %d, want the 200 form with its inline error", w.Code)
	}
	if f.updateFieldsCalls != 1 {
		t.Errorf("a rejected config reached the daemon: %d field updates", f.updateFieldsCalls)
	}
}

// Permission is the route's, in both branches: a viewer-role session is refused
// an operator-gated write with the same status, and nothing reaches the daemon.
func TestJSONAction_PermissionUnchanged(t *testing.T) {
	for _, asJSON := range []bool{false, true} {
		fake := escActionFake("open", `["approve:escalation"]`)
		srv := NewServer(fake, testLogger(), "", "http://test", "", nil)
		mux := http.NewServeMux()
		srv.RegisterRoutes(mux)
		req := actionPost("/b/gd-esc/escalation/route", url.Values{"audience": {"owner"}}, asJSON)
		ctx := oidcauth.ContextWithEmail(req.Context(), "approver@acme.example")
		ctx = oidcauth.ContextWithRoles(ctx, []string{oidcauth.RoleViewer})
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req.WithContext(ctx))
		if w.Code != http.StatusForbidden {
			t.Errorf("json=%v: HTTP %d, want 403", asJSON, w.Code)
		}
		if asJSON {
			if res := decodeAction(t, w); res.OK || res.Status != 403 || res.Error == "" {
				t.Errorf("JSON refusal: %+v", res)
			}
		}
		if len(fake.routeEscIDs) != 0 {
			t.Errorf("json=%v: a refused route reached the daemon", asJSON)
		}
	}
}
