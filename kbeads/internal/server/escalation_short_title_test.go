package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// The daemon authors the escalations handleEscalation and
// createGrantEscalation file, on behalf of an agent caller. Under
// BEADS_SHORT_TITLE_GATE=enforce a create with no short_title from that caller
// is refused, and handleEscalation then falls back to a bare 403, so every
// `gb spawn --escalate` would fail (gd-lYZUr0CqY0). These tests run each path
// under enforce and assert the 202 and a valid, non-empty short_title.

// requireEscalationShortTitle asserts rec is a 202 whose escalation bead
// carries want as its short_title, and that want passes ValidateBead.
func requireEscalationShortTitle(t *testing.T, ms *mockStore, code int, body []byte, want string) {
	t.Helper()
	if code != http.StatusAccepted {
		t.Fatalf("escalate under enforce: expected 202, got %d: %s", code, body)
	}
	var resp EscalationResponse
	if err := json.Unmarshal(body, &resp); err != nil || resp.EscalationID == "" {
		t.Fatalf("decoding escalation response %s: %v", body, err)
	}
	esc := ms.beads[resp.EscalationID]
	if esc == nil {
		t.Fatalf("escalation bead %s not found", resp.EscalationID)
	}
	if esc.ShortTitle != want {
		t.Errorf("short_title = %q; want %q", esc.ShortTitle, want)
	}
	if err := model.ValidateBead(esc); err != nil {
		t.Errorf("escalation bead fails ValidateBead: %v", err)
	}
}

// TestHandleEscalation_PrivilegedSpawn_EnforceGate drives the real
// `gb spawn --role infra --escalate` path end to end: a static caller is
// denied a privileged spawn and retries with X-Escalate.
func TestHandleEscalation_PrivilegedSpawn_EnforceGate(t *testing.T) {
	t.Setenv("BEADS_SHORT_TITLE_GATE", "enforce")
	const adminToken = "admin-esc-short-title"
	ts, _, ms := newEscalationE2EServer(t, adminToken, NewFleetConfig("esc-short-title-fleet", nil), &escOIDCMock{})
	makeRoleBead(ms, "gd-role-infra", "infra", true)

	resp := escRequest(t, ts, "POST", "/v1/beads", adminToken, map[string]any{
		"title":  "gardenio-infra-short-title-e2e",
		"type":   "agent",
		"labels": []string{"project:gardenio"},
		"fields": map[string]any{
			"agent":   "gardenio-infra-short-title-e2e",
			"mode":    "crew",
			"role":    "infra",
			"project": "gardenio",
		},
	}, map[string]string{"X-Operated-By": "oidc:admin@example.com", "X-Escalate": "true"})
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	requireEscalationShortTitle(t, ms, resp.StatusCode, body, "spawn infra agent")
}

// TestHandleEscalation_GenericDenial_EnforceGate covers a non-spawn denial
// (no SpawnRole) from an agent caller.
func TestHandleEscalation_GenericDenial_EnforceGate(t *testing.T) {
	t.Setenv("BEADS_SHORT_TITLE_GATE", "enforce")
	s, ms, _ := newTestServer()

	req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader([]byte(`{}`))).
		WithContext(shortTitleGateCtx(shortTitleGateCallers["fleet"].id))
	req.Header.Set("X-Escalate", "true")
	rec := httptest.NewRecorder()
	d := &AuthzDenial{Action: "create", BeadType: "user", Escalatable: true, Message: "admin only"}
	// A real create body: a create replay with operation_spec {} is refused
	// at filing (P7 Task 15).
	if !s.handleEscalation(rec, req, d, []byte(`{"title":"u","type":"user"}`), "") {
		t.Fatal("handleEscalation returned false for an escalatable X-Escalate denial")
	}
	requireEscalationShortTitle(t, ms, rec.Code, rec.Body.Bytes(), "create user denied")
}

// TestHandleEscalation_TaintGrant_EnforceGate covers the taint write-gate
// denial, which files a `grant` escalation through createGrantEscalation —
// the same creator `kd taint grant-request` uses. The taint names are long
// enough that the short_title must be clipped.
func TestHandleEscalation_TaintGrant_EnforceGate(t *testing.T) {
	t.Setenv("BEADS_SHORT_TITLE_GATE", "enforce")
	s, ms, _ := newTestServer()

	req := httptest.NewRequest(http.MethodPatch, "/v1/beads/gd-tainted", bytes.NewReader([]byte(`{}`))).
		WithContext(shortTitleGateCtx(AuthIdentity{Type: "fleet", Subject: "slack-bridge"}))
	req.Header.Set("X-Escalate", "true")
	rec := httptest.NewRecorder()
	d := &AuthzDenial{
		Action: "update", BeadType: "task", Escalatable: true, Message: "tainted",
		TaintNames:      []string{"acme-production", "acme-staging", "acme-billing"},
		TaintOwnerTeams: []string{"acme-owners"},
	}
	if !s.handleEscalation(rec, req, d, []byte(`{}`), "gd-tainted") {
		t.Fatal("handleEscalation returned false for an escalatable X-Escalate denial")
	}
	want := "grant acme-production, acme-staging…"
	if !strings.HasPrefix("grant "+strings.Join(d.TaintNames, ", "), strings.TrimSuffix(want, "…")) {
		t.Fatalf("test fixture: %q is not a clip of the taint list", want)
	}
	requireEscalationShortTitle(t, ms, rec.Code, rec.Body.Bytes(), want)
}
