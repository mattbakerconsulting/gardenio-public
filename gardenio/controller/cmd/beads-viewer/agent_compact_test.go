package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// TestHandleAgentCompact_FirstClickRequiresConfirmation: an unconfirmed POST
// returns the inline confirmation fragment and makes NO upstream call. The
// audit line records result=confirm_required.
func TestHandleAgentCompact_FirstClickRequiresConfirmation(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{}
	mux, logs := captureLogServer(t, f, cm)

	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/compact", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.nudgeCalls != 0 {
		t.Errorf("expected no upstream call before confirm, got %d", cm.nudgeCalls)
	}
	if !strings.Contains(w.Body.String(), "agent-compact-confirm") {
		t.Errorf("expected confirm fragment, got %q", w.Body.String())
	}
	if !strings.Contains(logs.String(), "confirm_required") {
		t.Errorf("expected confirm_required audit line, got %q", logs.String())
	}
}

// TestHandleAgentCompact_ConfirmedSendsCompact: a confirmed POST forwards the
// "/compact" slash command through the nudge path (types + Enter) and logs
// result=ok.
func TestHandleAgentCompact_ConfirmedSendsCompact(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{}
	mux, logs := captureLogServer(t, f, cm)

	form := url.Values{"confirmed": {"1"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/compact",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.nudgeCalls != 1 {
		t.Fatalf("expected 1 Nudge call, got %d", cm.nudgeCalls)
	}
	if cm.nudgeLastID != "sess-xyz" {
		t.Errorf("Nudge sessionID = %q, want sess-xyz", cm.nudgeLastID)
	}
	if cm.nudgeLastText != "/compact" {
		t.Errorf("Nudge message = %q, want /compact", cm.nudgeLastText)
	}
	if !strings.Contains(logs.String(), `"result":"ok"`) {
		t.Errorf("expected ok audit line, got %q", logs.String())
	}
}

// TestHandleAgentCompactCancel_NoUpstream: a cancel POST makes no upstream
// call, clears the confirm slot, and audits result=ux_canceled.
func TestHandleAgentCompactCancel_NoUpstream(t *testing.T) {
	f := newFake()
	f.beads["gd-agt"] = mkAgentBead()
	cm := &fakeScreenClient{}
	mux, logs := captureLogServer(t, f, cm)

	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/compact/cancel", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.nudgeCalls != 0 {
		t.Errorf("expected no upstream call, got %d", cm.nudgeCalls)
	}
	if !strings.Contains(logs.String(), "ux_canceled") {
		t.Errorf("expected ux_canceled audit line, got %q", logs.String())
	}
}

// TestHandleAgentCompact_NoSession: an agent bead with no live session returns
// 409 Conflict (shared agentWriteContext guard) and makes no upstream call.
func TestHandleAgentCompact_NoSession(t *testing.T) {
	f := newFake()
	b := mkAgentBead()
	b.Fields["last_session_id"] = ""
	f.beads["gd-agt"] = b
	cm := &fakeScreenClient{}
	mux, _ := captureLogServer(t, f, cm)

	form := url.Values{"confirmed": {"1"}}
	req := httptest.NewRequest(http.MethodPost, "/agents/gd-agt/compact",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d body=%q", w.Code, w.Body.String())
	}
	if cm.nudgeCalls != 0 {
		t.Errorf("expected no upstream call, got %d", cm.nudgeCalls)
	}
}
