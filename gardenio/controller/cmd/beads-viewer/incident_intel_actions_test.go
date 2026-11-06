package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
)

// Phase 3 — action descriptor + dispatcher tests. Bead: ka-0000005188.

// TestActionDescriptor_JSON confirms the descriptor wire shape: every field
// the dispatcher consumes is preserved across the JSON round-trip, and Kind
// is required (zero-value Kind errors out so a forgotten setter at the call
// site fails loud rather than emitting a malformed descriptor).
func TestActionDescriptor_JSON(t *testing.T) {
	t.Run("required Kind", func(t *testing.T) {
		_, err := ActionDescriptor{}.JSON()
		if err == nil {
			t.Fatal("expected error on empty Kind, got nil")
		}
	})

	t.Run("round-trips all fields", func(t *testing.T) {
		want := ActionDescriptor{
			ID:     "open-jira",
			Kind:   ActionExternalLink,
			Label:  "FSM-1",
			Target: "https://example/browse/FSM-1",
			Params: map[string]any{"k": "v", "n": float64(2)},
			Confirm: &ConfirmDescriptor{
				Title:   "Sure?",
				Body:    "Confirm body",
				Confirm: "Go",
				Danger:  true,
			},
			Hotkey:   "j",
			Requires: []string{"operator"},
			Danger:   true,
			Hint:     "Open ticket",
			SwapInto: "#evidence-1",
		}
		s, err := want.JSON()
		if err != nil {
			t.Fatalf("MustJSON: %v", err)
		}
		var got ActionDescriptor
		if err := json.Unmarshal([]byte(s), &got); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if got.ID != want.ID || got.Kind != want.Kind || got.Target != want.Target {
			t.Errorf("scalar fields lost: got %+v", got)
		}
		if got.Confirm == nil || got.Confirm.Body != want.Confirm.Body || !got.Confirm.Danger {
			t.Errorf("confirm lost: got %+v", got.Confirm)
		}
		if got.Hotkey != "j" || !got.Danger || got.Hint != want.Hint || got.SwapInto != want.SwapInto {
			t.Errorf("aux fields lost: got %+v", got)
		}
		if len(got.Requires) != 1 || got.Requires[0] != "operator" {
			t.Errorf("requires lost: %+v", got.Requires)
		}
	})

	t.Run("MustJSON returns empty on error", func(t *testing.T) {
		got := ActionDescriptor{}.MustJSON()
		if got != "" {
			t.Errorf("expected empty MustJSON for empty Kind, got %q", got)
		}
	})
}

// TestStandardActionLibrary_Helpers exercises every helper in the v1 action
// library. The descriptors they produce drive every actionable button across
// every Incident Intelligence widget — a regression here means widgets emit
// the wrong descriptor shape and the dispatcher silently misroutes.
func TestStandardActionLibrary_Helpers(t *testing.T) {
	t.Run("OpenJiraAction defaults base + escapes ticket", func(t *testing.T) {
		d := OpenJiraAction("", "FSM-12345")
		if d.Kind != ActionExternalLink || d.ID != "open-jira" {
			t.Errorf("kind/id: %+v", d)
		}
		if !strings.HasPrefix(d.Target, "https://acme.atlassian.net/browse/") {
			t.Errorf("default base missing: %s", d.Target)
		}
		if !strings.HasSuffix(d.Target, "FSM-12345") {
			t.Errorf("ticket missing: %s", d.Target)
		}
	})
	t.Run("OpenJiraAction custom base trims trailing slash", func(t *testing.T) {
		d := OpenJiraAction("https://acme.atlassian.net/", "X-1")
		if d.Target != "https://acme.atlassian.net/browse/X-1" {
			t.Errorf("trailing-slash base: %s", d.Target)
		}
	})
	t.Run("OpenMezmoAction encodes query", func(t *testing.T) {
		d := OpenMezmoAction("", `app:foo level:error`)
		u, err := url.Parse(d.Target)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if u.Host != "app.mezmo.com" {
			t.Errorf("default host: %s", u.Host)
		}
		if u.Query().Get("q") != "app:foo level:error" {
			t.Errorf("query lost: %q", u.Query().Get("q"))
		}
	})
	t.Run("OpenIDEAction includes line when >0", func(t *testing.T) {
		d := OpenIDEAction("foo/bar.go", 42)
		if d.Kind != ActionIDEDeeplink {
			t.Errorf("wrong kind: %s", d.Kind)
		}
		if d.Target != "foo/bar.go:42" {
			t.Errorf("target: %s", d.Target)
		}
		// Line=0 → no suffix; widgets often don't have a line.
		d2 := OpenIDEAction("foo/bar.go", 0)
		if d2.Target != "foo/bar.go" {
			t.Errorf("zero line should be suppressed: %s", d2.Target)
		}
	})
	t.Run("CopyToClipboardAction surfaces text via target", func(t *testing.T) {
		d := CopyToClipboardAction("Copy", "panic: x")
		if d.Kind != ActionClipboard || d.Target != "panic: x" {
			t.Errorf("clipboard: %+v", d)
		}
	})
	t.Run("DismissEvidenceAction is destructive + confirms", func(t *testing.T) {
		d := DismissEvidenceAction("ev-1", "#out")
		if d.Kind != ActionHtmxPost || d.SwapInto != "#out" {
			t.Errorf("htmx-post wiring: %+v", d)
		}
		if d.Confirm == nil || !d.Confirm.Danger {
			t.Errorf("confirm/danger missing: %+v", d.Confirm)
		}
		if d.Params["evidence_id"] != "ev-1" {
			t.Errorf("evidence_id missing: %+v", d.Params)
		}
	})
	t.Run("AckIncidentAction non-destructive but confirms", func(t *testing.T) {
		d := AckIncidentAction("gd-x", "#ack")
		if d.Kind != ActionHtmxPost {
			t.Errorf("kind: %s", d.Kind)
		}
		if d.Confirm == nil {
			t.Fatal("confirm required")
		}
		if d.Confirm.Danger {
			t.Errorf("ack should not be marked danger")
		}
	})
}

// TestActionRegistry_RegisterLookup verifies the registry's read/write API,
// re-registration semantics, and the IDs() introspection used by the gallery.
func TestActionRegistry_RegisterLookup(t *testing.T) {
	reg := NewActionRegistry()

	// Lookup of unregistered id returns ok=false.
	if _, _, ok := reg.Lookup("nope"); ok {
		t.Error("expected ok=false for unregistered id")
	}

	calls := 0
	reg.Register("ack", func(w http.ResponseWriter, r *http.Request, p ActionParams) error {
		calls++
		return nil
	}, "operator")

	h, roles, ok := reg.Lookup("ack")
	if !ok {
		t.Fatal("expected handler registered")
	}
	if len(roles) != 1 || roles[0] != "operator" {
		t.Errorf("roles: %v", roles)
	}
	_ = h(httptest.NewRecorder(), httptest.NewRequest("POST", "/actions/ack", nil), nil)
	if calls != 1 {
		t.Errorf("handler not called")
	}

	// Re-registration replaces.
	reg.Register("ack", func(w http.ResponseWriter, r *http.Request, p ActionParams) error {
		calls += 10
		return nil
	})
	h2, _, _ := reg.Lookup("ack")
	_ = h2(httptest.NewRecorder(), httptest.NewRequest("POST", "/actions/ack", nil), nil)
	if calls != 11 {
		t.Errorf("re-register did not replace: %d", calls)
	}

	// IDs() returns every registered id.
	reg.Register("dismiss", func(w http.ResponseWriter, r *http.Request, p ActionParams) error { return nil })
	ids := reg.IDs()
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "ack" || ids[1] != "dismiss" {
		t.Errorf("IDs: %v", ids)
	}
}

// TestServer_HandleAction_JSONBody routes a JSON POST through the dispatcher
// end-to-end. The handler reads the params and the registry decodes the
// response — confirms the wire path widget authors will use.
func TestServer_HandleAction_JSONBody(t *testing.T) {
	mux := mkServer(t, newFake())
	body := bytes.NewBufferString(`{"evidence_id":"ev-42"}`)
	req := httptest.NewRequest("POST", "/actions/dismiss-evidence", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ev-42") {
		t.Errorf("handler did not see evidence_id: %s", w.Body.String())
	}
}

// TestServer_HandleAction_FormBody confirms form-encoded params (the htmx
// default if a widget chooses POSTed forms) decode to the same shape.
func TestServer_HandleAction_FormBody(t *testing.T) {
	mux := mkServer(t, newFake())
	form := url.Values{}
	form.Set("evidence_id", "ev-99")
	req := httptest.NewRequest("POST", "/actions/dismiss-evidence", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status: %d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ev-99") {
		t.Errorf("handler did not see form evidence_id: %s", w.Body.String())
	}
}

// TestServer_HandleAction_UnknownReturns404 — unregistered id MUST 404, not
// silently 200. Widgets that emit a typo'd id would otherwise look fine in
// dev and break only when the back end runs in production.
func TestServer_HandleAction_UnknownReturns404(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("POST", "/actions/no-such-id", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", w.Code)
	}
}

// TestServer_HandleAction_BadJSON returns 400 instead of crashing the handler
// — malformed bodies are user-input boundaries.
func TestServer_HandleAction_BadJSON(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("POST", "/actions/dismiss-evidence", strings.NewReader(`{not json`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestServer_HandleAction_AckEmitsAcknowledgedPill — the demo ack handler
// returns an `acknowledged` pill that htmx swaps into the page. This wire
// shape is what Phase 8 widgets will mirror.
func TestServer_HandleAction_AckEmitsAcknowledgedPill(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("POST", "/actions/ack-incident", strings.NewReader(`{"incident_id":"gd-x"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status: %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `data-confidence="high"`) || !strings.Contains(body, "acknowledged") {
		t.Errorf("ack response missing acknowledged pill: %s", body)
	}
}

// TestServer_HandleAction_NilRegistryReturns503 — exercising the nil-safe
// guard when a future test wires a Server without actions.
func TestServer_HandleAction_NilRegistryReturns503(t *testing.T) {
	srv := &Server{}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/actions/anything", nil)
	srv.handleAction(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503 on nil registry, got %d", w.Code)
	}
}

// TestServer_HandleAction_HandlerErrorReturns500 confirms a handler returning
// a non-nil error becomes a 500 with body. Dispatcher must never panic; the
// handler is the layer that surfaces user-facing errors.
func TestServer_HandleAction_HandlerErrorReturns500(t *testing.T) {
	srv := &Server{
		actions: NewActionRegistry(),
		logger:  testLogger(),
	}
	srv.actions.Register("boom", func(w http.ResponseWriter, r *http.Request, p ActionParams) error {
		return errors.New("synthetic")
	})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /actions/{id}", srv.handleAction)

	req := httptest.NewRequest("POST", "/actions/boom", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expected 500, got %d", w.Code)
	}
}

// TestStaticAsset_ServesIncidentIntelActionsJS confirms the Phase 3 client
// dispatcher is reachable via the same /assets/static/ route as the rest of
// the bundle. layout.html links it after incident_intel.js — a missing
// asset breaks every actionable button across every widget at runtime.
func TestStaticAsset_ServesIncidentIntelActionsJS(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/assets/static/incident_intel_actions.js", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"dispatchAction",
		"parseActionDescriptor",
		"ide-deeplink",
		"htmx-post",
		"clipboard",
		"ii:ide-pref",
		"ii:action-dispatched",
		"ii:action-cancelled",
		"ii:action-error",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("incident_intel_actions.js missing %q", want)
		}
	}
}

// TestLayout_LinksIncidentIntelActionsJS — the global layout MUST include
// the Phase 3 dispatcher script after the Phase 1 bundle, otherwise widgets
// rendered on any page won't bind.
func TestLayout_LinksIncidentIntelActionsJS(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for /, got %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "/assets/static/incident_intel_actions.js") {
		t.Errorf("layout missing incident_intel_actions.js script tag")
	}
}

// TestStandardActions_RegisteredOnConstruction — the Server constructor must
// install every v1 action handler. Forgotten handlers would yield 404 in
// production with no dev signal.
func TestStandardActions_RegisteredOnConstruction(t *testing.T) {
	srv := mkRawServer(t, newFake())
	if srv.actions == nil {
		t.Fatal("actions registry not installed")
	}
	for _, want := range []string{"dismiss-evidence", "ack-incident"} {
		if _, _, ok := srv.actions.Lookup(want); !ok {
			t.Errorf("expected standard action %q registered", want)
		}
	}
}

// mkRawServer constructs a *Server (not the http.Mux wrapper) so tests can
// poke at internals like s.actions directly. The mkServer helper used by
// every other test wires routes through RegisterRoutes; this helper exists
// so we can introspect the constructed instance without re-implementing
// route registration.
func mkRawServer(t *testing.T, daemon beadAPI) *Server {
	t.Helper()
	return NewServer(daemon, testLogger(), "", "", "", nil)
}

// testLogger returns a slog.Logger that drops to io.Discard — handlers
// expect a non-nil logger but the tests here don't assert on log output.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Compile-time assertion the test file actually exercises ActionParams.
var _ = func() bool { var _ ActionParams = ActionParams{}; return true }
