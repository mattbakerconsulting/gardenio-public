package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/gardenhub"
)

func mkAgentBead() *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:     "gd-agt",
		Type:   "agent",
		Status: "in_progress",
		Fields: map[string]string{"last_session_id": "sess-xyz"},
	}
}

// captureLogServer wraps mkAgentServer with a buffered slog handler so
// audit-log assertions can inspect the structured fields. Phase 3 leans on
// the audit lines to distinguish ux_canceled from ok.
func captureLogServer(t *testing.T, daemon beadAPI, cm screenClient) (*http.ServeMux, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := NewServer(daemon, logger, "", "", "", cm)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return mux, &buf
}

// -- /agents/{id}/keys --------------------------------------------------------

func TestHandleAgentKeys_Success(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{}
	mux, _ := captureLogServer(t, f, cm)

	form := url.Values{"keys": {"Up,Tab,Enter"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/keys",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.keysCalls != 1 {
		t.Fatalf("expected 1 SendKeys call, got %d", cm.keysCalls)
	}
	if got, want := cm.keysLastKeys, []string{"Up", "Tab", "Enter"}; len(got) != len(want) || got[0] != want[0] || got[2] != want[2] {
		t.Errorf("keys = %v, want %v", got, want)
	}
}

// TestHandleAgentKeys_DestructiveRequiresConfirmation: clicking Ctrl-C in the
// toolbar (no confirmed=1) returns the inline confirmation fragment and
// makes NO upstream call. The audit line records result=confirm_required.
func TestHandleAgentKeys_DestructiveRequiresConfirmation(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{}
	mux, logs := captureLogServer(t, f, cm)

	form := url.Values{"keys": {"Ctrl-C"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/keys",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 fragment, got %d", w.Code)
	}
	if cm.keysCalls != 0 {
		t.Errorf("expected 0 upstream SendKeys calls (confirm not satisfied), got %d", cm.keysCalls)
	}
	body := w.Body.String()
	if !strings.Contains(body, `id="agent-keys-confirm"`) {
		t.Errorf("expected confirm fragment, got: %s", body)
	}
	if !strings.Contains(body, "Confirm send") {
		t.Errorf("expected Confirm button, got: %s", body)
	}
	if !strings.Contains(logs.String(), `"result":"confirm_required"`) {
		t.Errorf("expected confirm_required audit line, logs=%s", logs.String())
	}
}

// TestHandleAgentKeys_DestructiveConfirmed forwards the keys upstream when
// the operator's second POST carries confirmed=1.
func TestHandleAgentKeys_DestructiveConfirmed(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{}
	mux, logs := captureLogServer(t, f, cm)

	form := url.Values{"keys": {"Ctrl-D"}, "confirmed": {"1"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/keys",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if cm.keysCalls != 1 {
		t.Fatalf("expected 1 SendKeys call, got %d", cm.keysCalls)
	}
	if !strings.Contains(logs.String(), `"result":"ok"`) {
		t.Errorf("expected result:ok audit line, logs=%s", logs.String())
	}
	if !strings.Contains(logs.String(), `"keys":"Ctrl-D"`) {
		t.Errorf("expected keys field in audit, logs=%s", logs.String())
	}
}

// TestHandleAgentKeysCancel emits a ux_canceled audit line and makes no
// upstream call.
func TestHandleAgentKeysCancel(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{}
	mux, logs := captureLogServer(t, f, cm)

	form := url.Values{"keys": {"Ctrl-C"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/keys/cancel",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if cm.keysCalls != 0 {
		t.Errorf("expected 0 SendKeys calls on cancel, got %d", cm.keysCalls)
	}
	if !strings.Contains(logs.String(), `"result":"ux_canceled"`) {
		t.Fatalf("expected ux_canceled audit line, logs=%s", logs.String())
	}
}

// TestHandleAgentKeys_EmptyKeys rejects an empty CSV — the toolbar would
// otherwise fire on a blank custom-key submission.
func TestHandleAgentKeys_EmptyKeys(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{}
	mux := mkAgentServer(t, f, cm)

	form := url.Values{"keys": {""}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/keys",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if cm.keysCalls != 0 {
		t.Errorf("expected no upstream call, got %d", cm.keysCalls)
	}
}

// TestHandleAgentKeys_NoSession returns 409 when last_session_id is empty.
func TestHandleAgentKeys_NoSession(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = &beadsapi.BeadDetail{
		ID: "gd-agt", Type: "agent", Status: "in_progress",
		Fields: map[string]string{},
	}
	cm := &fakeScreenClient{}
	mux := mkAgentServer(t, f, cm)

	form := url.Values{"keys": {"Tab"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/keys",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

func TestHandleAgentKeys_UpstreamSessionNotFound(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{keysErr: coopmuxapi.ErrSessionNotFound}
	mux := mkAgentServer(t, f, cm)

	form := url.Values{"keys": {"Tab"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/keys",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// -- /agents/{id}/nudge -------------------------------------------------------

// glassNudgeServer is mkAgentServer with the garden's Glass wired the way main
// wires it, but with a recording nudge: /agents/{id}/nudge goes the Glass's way.
func glassNudgeServer(t *testing.T, daemon *fakeDaemon, cm screenClient) (*http.ServeMux, *bytes.Buffer, *[]string) {
	t.Helper()
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	srv := NewServer(daemon, logger, "", "", "", cm)
	var nudged []string
	srv.SetGardenHandler(&gardenhub.Handler{Beads: daemon, Glass: &gardenhub.Glass{
		InScope: func(*http.Request, string) bool { return true },
		Who: func(*http.Request) coopmuxapi.Identity {
			return coopmuxapi.Identity{Operator: "op@example.com", AuthMode: "cookie", Roles: []string{"gardenio-operator"}}
		},
		Nudge: func(_ context.Context, agent, message string) gardenhub.NudgeOutcome {
			nudged = append(nudged, agent+": "+message)
			return gardenhub.NudgeDelivered
		},
		Logger: logger,
	}})
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)
	return mux, &buf, &nudged
}

func postNudge(mux http.Handler, message string) *httptest.ResponseRecorder {
	form := url.Values{"message": {message}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/nudge", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	return w
}

// /agents/{id}/nudge is re-pointed at the Glass's nudge (Lands 6.5): the
// bridge's NudgeAgent by agent name, not coopmux's PTY nudge.
func TestHandleAgentNudge_GoesTheGlassWay(t *testing.T) {
	f := newFake()
	b := mkAgentBead()
	b.Title = "crew-agt"
	f.beads["gd-agt"] = b
	cm := &fakeScreenClient{}
	mux, logs, nudged := glassNudgeServer(t, f, cm)

	w := postNudge(mux, "catch up and continue")
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%q", w.Code, w.Body.String())
	}
	if len(*nudged) != 1 || (*nudged)[0] != "crew-agt: catch up and continue" {
		t.Fatalf("nudged %q", *nudged)
	}
	if cm.nudgeCalls != 0 {
		t.Errorf("coopmux's PTY nudge was used: %d calls", cm.nudgeCalls)
	}
	if !strings.Contains(logs.String(), `"subverb":"nudge"`) || !strings.Contains(logs.String(), `"surface":"glass"`) {
		t.Errorf("expected the subverb:nudge audit line, logs=%s", logs.String())
	}
}

func TestHandleAgentNudge_EmptyMessage(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{}
	mux, _, nudged := glassNudgeServer(t, f, cm)

	if w := postNudge(mux, "   "); w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if len(*nudged) != 0 || cm.nudgeCalls != 0 {
		t.Errorf("expected no nudge, got %q / %d", *nudged, cm.nudgeCalls)
	}
}

func TestHandleAgentNudge_NoGlass(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{}
	if w := postNudge(mkAgentServer(t, f, cm), "hi"); w.Code != http.StatusServiceUnavailable || cm.nudgeCalls != 0 {
		t.Fatalf("expected 503 and no nudge, got %d / %d", w.Code, cm.nudgeCalls)
	}
}

func TestNudgeOutcomeOf(t *testing.T) {
	for _, c := range []struct {
		err  error
		want gardenhub.NudgeOutcome
	}{
		{nil, gardenhub.NudgeDelivered},
		{fmt.Errorf("x: %w", bridge.ErrAgentNotNudgeable), gardenhub.NudgeStopped},
		{fmt.Errorf("find agent bead: %w", beadsapi.ErrAgentNotFound), gardenhub.NudgeGone},
		{errors.New("find agent bead: 500"), gardenhub.NudgeFailed},
	} {
		if got := nudgeOutcomeOf(c.err); got != c.want {
			t.Errorf("%v: %s, want %s", c.err, got, c.want)
		}
	}
}

// -- /agents/{id}/respond -----------------------------------------------------

func TestHandleAgentRespond_Accept(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{}
	mux, logs := captureLogServer(t, f, cm)

	form := url.Values{"accept": {"yes"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/respond",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.respondCalls != 1 {
		t.Fatalf("expected 1 Respond call, got %d", cm.respondCalls)
	}
	if cm.respondLastReq.Accept == nil || *cm.respondLastReq.Accept != true {
		t.Errorf("Accept = %v, want &true", cm.respondLastReq.Accept)
	}
	if cm.respondLastReq.Text != nil || cm.respondLastReq.Option != nil {
		t.Errorf("expected only Accept to be set, got Text=%v Option=%v",
			cm.respondLastReq.Text, cm.respondLastReq.Option)
	}
	if !strings.Contains(logs.String(), `"subverb":"respond"`) {
		t.Errorf("expected subverb:respond audit, logs=%s", logs.String())
	}
}

func TestHandleAgentRespond_Reject(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{}
	mux := mkAgentServer(t, f, cm)

	form := url.Values{"accept": {"no"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/respond",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if cm.respondLastReq.Accept == nil || *cm.respondLastReq.Accept != false {
		t.Errorf("Accept = %v, want &false", cm.respondLastReq.Accept)
	}
}

func TestHandleAgentRespond_Text(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{}
	mux := mkAgentServer(t, f, cm)

	form := url.Values{"text": {"please continue with plan A"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/respond",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if cm.respondLastReq.Text == nil || *cm.respondLastReq.Text != "please continue with plan A" {
		t.Errorf("Text = %v", cm.respondLastReq.Text)
	}
}

func TestHandleAgentRespond_Option(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{}
	mux := mkAgentServer(t, f, cm)

	form := url.Values{"option": {"3"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/respond",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", w.Code)
	}
	if cm.respondLastReq.Option == nil || *cm.respondLastReq.Option != 3 {
		t.Errorf("Option = %v, want &3", cm.respondLastReq.Option)
	}
}

func TestHandleAgentRespond_RequiresAtLeastOneField(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{}
	mux := mkAgentServer(t, f, cm)

	form := url.Values{}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/respond",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	if cm.respondCalls != 0 {
		t.Errorf("expected no upstream call, got %d", cm.respondCalls)
	}
}

func TestHandleAgentRespond_BadAcceptValue(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{}
	mux := mkAgentServer(t, f, cm)

	form := url.Values{"accept": {"maybe"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/respond",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// -- Coopmux disabled paths --------------------------------------------------

func TestHandleAgentKeys_CoopmuxDisabled(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{disabled: true}
	mux := mkAgentServer(t, f, cm)

	form := url.Values{"keys": {"Tab"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/keys",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}

// -- helpers ------------------------------------------------------------------

func TestParseAgentKeysCSV(t *testing.T) {
	cases := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{"Esc", []string{"Esc"}, false},
		{"Esc,Tab,Enter", []string{"Esc", "Tab", "Enter"}, false},
		{" Esc , Tab ,, Enter ", []string{"Esc", "Tab", "Enter"}, false},
		{"", nil, true},
		{",,,", nil, true},
		{strings.Repeat("a", maxAgentKeyLen+1), nil, true},
	}
	for _, c := range cases {
		got, err := parseAgentKeysCSV(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseAgentKeysCSV(%q) expected error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseAgentKeysCSV(%q) error = %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("parseAgentKeysCSV(%q) len = %d, want %d", c.in, len(got), len(c.want))
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("parseAgentKeysCSV(%q)[%d] = %q, want %q", c.in, i, got[i], c.want[i])
			}
		}
	}
}

func TestIsDestructiveKey(t *testing.T) {
	cases := []struct {
		in   []string
		want bool
	}{
		{[]string{"Esc"}, true}, // Claude Code reads Esc as interrupt
		{[]string{"Tab", "Enter"}, false},
		{[]string{"Ctrl-C"}, true},
		{[]string{"ctrl-c"}, true},
		{[]string{"CTRL-D"}, true},
		{[]string{"Ctrl-\\"}, false}, // coop cannot type it, so it is not offered (gd-HCcFZAc9Rj)
		{[]string{"Ctrl-Z"}, true},
		{[]string{"Tab", "Ctrl-C"}, true}, // any destructive poisons batch
		{[]string{"Ctrl-A"}, false},       // not in destructive set
	}
	for _, c := range cases {
		if got := isDestructiveKey(c.in); got != c.want {
			t.Errorf("isDestructiveKey(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestAtoiSigned(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"0", 0, false},
		{"3", 3, false},
		{"-7", -7, false},
		{"+12", 12, false},
		{"", 0, true},
		{"-", 0, true},
		{"3a", 0, true},
		{"a3", 0, true},
	}
	for _, c := range cases {
		got, err := atoiSigned(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("atoiSigned(%q) expected error, got %d", c.in, got)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("atoiSigned(%q) = (%d, %v), want %d", c.in, got, err, c.want)
		}
	}
}

func TestBeadShowRefererID(t *testing.T) {
	cases := []struct {
		basePath, referer, want string
	}{
		// The agent page is the catch-all /<id> bead-show route, so its URL
		// (and thus the palette's Referer) is /<id> — never /agents/{id}.
		{"", "https://example.com/gd-xfshRmkdC5", "gd-xfshRmkdC5"},
		{"", "/gd-xfshRmkdC5", "gd-xfshRmkdC5"},
		{"", "/gd-xfshRmkdC5#agent-keys", "gd-xfshRmkdC5"},
		{"", "/gd-xfshRmkdC5?tab=x", "gd-xfshRmkdC5"},
		{"/v", "https://example.com/v/gd-xfshRmkdC5", "gd-xfshRmkdC5"},
		{"/v/", "/v/gd-xfshRmkdC5", "gd-xfshRmkdC5"},
		// Nested routes have an interior '/' and are rejected — only the
		// single-segment bead-show page qualifies.
		{"", "/agents/gd-xfshRmkdC5", ""},
		{"", "/agents/gd-xfshRmkdC5/prime", ""},
		{"", "/plans/gd-xfshRmkdC5", ""},
		{"", "/u/some-slug", ""},
		{"", "/p/gardenio", ""},
		// Single-segment literal routes whose leaf isn't a bead id are rejected.
		{"", "/agents", ""},
		{"", "/active", ""},
		{"", "/not-a-bead", ""},
		{"", "", ""},
	}
	for _, c := range cases {
		if got := beadShowRefererID(c.basePath, c.referer); got != c.want {
			t.Errorf("beadShowRefererID(%q, %q) = %q, want %q", c.basePath, c.referer, got, c.want)
		}
	}
}

// TestHandleCmdkSuggest_AgentDetailActions verifies the palette surfaces the
// agent-action rows when invoked from an agent bead page — the catch-all
// /<id> route — and anchors them to /<id>#agent-… where those sections live.
func TestHandleCmdkSuggest_AgentDetailActions(t *testing.T) {
	f := newFake()
	f.beads["gd-xfshRmkdC5"] = &beadsapi.BeadDetail{ID: "gd-xfshRmkdC5", Type: "agent"}
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	req := httptest.NewRequest(http.MethodGet, "/cmdk/suggest", nil)
	req.Header.Set("Referer", "/gd-xfshRmkdC5")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{"send-keys", "nudge", "respond", "open-transcript", "catch-up"} {
		if !strings.Contains(body, want) {
			t.Errorf("expected %q in body, got: %s", want, body)
		}
	}
	if !strings.Contains(body, `href="/gd-xfshRmkdC5#agent-keys"`) {
		t.Errorf("expected anchored href, got: %s", body)
	}
}

// TestHandleCmdkSuggest_NoActionsFromNonAgentBead: actions are NOT surfaced
// when the referring /<id> page is a non-agent bead (task/bug/epic). The
// verb rows (send-keys, nudge, respond, …) would anchor to sections that
// only exist on type_agent.html.
func TestHandleCmdkSuggest_NoActionsFromNonAgentBead(t *testing.T) {
	f := newFake()
	f.beads["gd-xfshRmkdC5"] = &beadsapi.BeadDetail{ID: "gd-xfshRmkdC5", Type: "task"}
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	req := httptest.NewRequest(http.MethodGet, "/cmdk/suggest?q=", nil)
	req.Header.Set("Referer", "/gd-xfshRmkdC5")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "send-keys") || strings.Contains(body, "open-transcript") {
		t.Errorf("did not expect action rows on a task-bead referer: %s", body)
	}
}

// TestHandleCmdkSuggest_NoActionsFromOtherPages: actions are NOT surfaced
// when the referer is unrelated.
func TestHandleCmdkSuggest_NoActionsFromOtherPages(t *testing.T) {
	f := newFake()
	mux := mkAgentServer(t, f, &fakeScreenClient{})

	req := httptest.NewRequest(http.MethodGet, "/cmdk/suggest?q=", nil)
	req.Header.Set("Referer", "/active")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if strings.Contains(body, "send-keys") || strings.Contains(body, "open-transcript") {
		t.Errorf("did not expect action rows on /active referer: %s", body)
	}
}

// Sanity: SendKeys interface error returned for an empty-keys pass-through
// shouldn't happen at handler level (we error earlier), but keep the
// coopmuxapi noop guarantee tested.
func TestCoopmuxAPI_SendKeysEmpty(t *testing.T) {
	c := coopmuxapi.New(coopmuxapi.Config{})
	if err := c.SendKeys(context.Background(), "x", nil); !errors.Is(err, coopmuxapi.ErrDisabled) {
		t.Errorf("expected ErrDisabled for empty client, got %v", err)
	}
}
