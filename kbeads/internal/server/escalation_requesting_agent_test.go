package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// TestEscalationCreate_RequestingAgentServerStamped verifies gd-8eu41KlMr2: a
// static-token agent (identified via X-Actor over the primary token, the way
// agent pods authenticate) cannot forge fields.requesting_agent by naming a
// different agent in the create body. Escalation replay attributes taint
// propagation to this field (WithTaintAttributionAgent in replayEscalation,
// PR #47 / ADR §4.2 row 7), so an un-stamped requesting_agent would let a
// tainted filer attribute a replayed write to a clean agent, bypassing the
// taint boundary once a human approves.
func TestEscalationCreate_RequestingAgentServerStamped(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)
	putAgentBead(ms, "gd-tainted-agent", "gd-tainted-agent", model.StatusInProgress, time.Now())

	body, _ := json.Marshal(map[string]any{
		"title": "grant deploy IAM role",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "manual_intervention",
			"operation_spec": map[string]any{"action": "grant role"},
			// The filer (gd-tainted-agent, via X-Actor below) forges an
			// untainted agent as the requester.
			"requesting_agent": "gd-untainted-agent",
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer primary-token")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Actor", "gd-tainted-agent")
	req.Header.Set("X-Operated-By", "slack:U0OPERATOR")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	fields, _ := resp["fields"].(map[string]any)
	if fields["requesting_agent"] != "gd-tainted-agent" {
		t.Errorf("requesting_agent = %v, want server-stamped gd-tainted-agent (the authenticated filer) — a forged value would let replay attribute taint propagation to the wrong agent", fields["requesting_agent"])
	}
}

// TestEscalationCreate_RequestingAgentLeftAloneForOIDC verifies the guard is
// scoped to static-token agent callers only: an OIDC human filing an
// escalation on an agent's behalf (e.g. via `kd create --escalate` run by a
// person) has no agent-bead identity to stamp from, so requesting_agent is
// left as supplied — matching handleEscalation's authz-denial path, which
// likewise only ever stamps identity.Subject.
func TestEscalationCreate_RequestingAgentLeftAloneForOIDC(t *testing.T) {
	_, _, h := newTestServerAuth()

	rec := doJSONAs(t, h, "POST", "/v1/beads", map[string]any{
		"title": "grant deploy IAM role",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type":   "manual_intervention",
			"operation_spec":   map[string]any{"action": "grant role"},
			"requesting_agent": "gd-some-agent",
		},
	}, "alice@example.com")
	requireStatus(t, rec, 201)

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	fields, _ := resp["fields"].(map[string]any)
	if fields["requesting_agent"] != "gd-some-agent" {
		t.Errorf("requesting_agent = %v, want gd-some-agent unchanged for an OIDC caller", fields["requesting_agent"])
	}
}

// TestEscalationUpdate_RequestingAgentImmutable verifies gd-8eu41KlMr2 (PR #63,
// second send-back): re-stamping a PATCH to the caller's own identity (the
// first fix) still let ANY static-token agent take over attribution from the
// original filer just by touching requesting_agent on a bead it didn't file —
// the re-stamp made the PATCH succeed with a different, merely-also-server-
// authenticated value. Escalation replay reads requesting_agent at approval
// time (WithTaintAttributionAgent in replayEscalation), so the field must be
// IMMUTABLE after create: any PATCH that would change it is rejected (400),
// for every caller, including the original filer.
func TestEscalationUpdate_RequestingAgentImmutable(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)
	putAgentBead(ms, "gd-tainted-agent", "gd-tainted-agent", model.StatusInProgress, time.Now())

	createBody, _ := json.Marshal(map[string]any{
		"title": "grant deploy IAM role",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "manual_intervention",
			"operation_spec": map[string]any{"action": "grant role"},
		},
	})
	createReq := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", "Bearer primary-token")
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("X-Actor", "gd-tainted-agent")
	createReq.Header.Set("X-Operated-By", "slack:U0OPERATOR")
	createRec := httptest.NewRecorder()
	h.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d; body: %s", createRec.Code, createRec.Body.String())
	}
	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	// Same filer now tries to PATCH requesting_agent to a different,
	// untainted agent before approval — the same forgery the create guard
	// blocks, attempted one write later. This must be rejected outright, not
	// re-stamped, or the field would still be movable by whichever
	// static-token agent PATCHes last.
	patchBody, _ := json.Marshal(map[string]any{
		"fields": map[string]any{
			"requesting_agent": "gd-untainted-agent",
		},
	})
	patchReq := httptest.NewRequest(http.MethodPatch, "/v1/beads/"+id, bytes.NewReader(patchBody))
	patchReq.Header.Set("Authorization", "Bearer primary-token")
	patchReq.Header.Set("Content-Type", "application/json")
	patchReq.Header.Set("X-Actor", "gd-tainted-agent")
	patchReq.Header.Set("X-Operated-By", "slack:U0OPERATOR")
	patchRec := httptest.NewRecorder()
	h.ServeHTTP(patchRec, patchReq)
	if patchRec.Code != http.StatusBadRequest {
		t.Fatalf("patch: expected 400, got %d; body: %s", patchRec.Code, patchRec.Body.String())
	}

	// The bead itself must be untouched — requesting_agent still the original
	// filer, not blanked out or partially applied.
	getReq := httptest.NewRequest(http.MethodGet, "/v1/beads/"+id, nil)
	getReq.Header.Set("Authorization", "Bearer primary-token")
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, getReq)
	var resp map[string]any
	decodeJSON(t, getRec, &resp)
	fields, _ := resp["fields"].(map[string]any)
	if fields["requesting_agent"] != "gd-tainted-agent" {
		t.Errorf("requesting_agent = %v, want unchanged gd-tainted-agent (the original filer) after a rejected PATCH", fields["requesting_agent"])
	}
}

// TestEscalationUpdate_RequestingAgentImmutable_OIDCAlsoRejected verifies the
// immutability guard applies to every caller, not just static-token agents:
// an OIDC human editing an escalation cannot move requesting_agent either,
// since the only legitimate value is the one the create-path guard stamped
// from the original filer's identity.
func TestEscalationUpdate_RequestingAgentImmutable_OIDCAlsoRejected(t *testing.T) {
	_, _, h := newTestServerAuth()

	createRec := doJSONAs(t, h, "POST", "/v1/beads", map[string]any{
		"title": "grant deploy IAM role",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type":   "manual_intervention",
			"operation_spec":   map[string]any{"action": "grant role"},
			"requesting_agent": "gd-some-agent",
		},
	}, "alice@example.com")
	requireStatus(t, createRec, 201)
	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	patchRec := doJSONAs(t, h, "PATCH", "/v1/beads/"+id, map[string]any{
		"fields": map[string]any{
			"requesting_agent": "gd-some-other-agent",
		},
	}, "alice@example.com")
	requireStatus(t, patchRec, 400)

	getRec := doJSONAs(t, h, "GET", "/v1/beads/"+id, nil, "alice@example.com")
	requireStatus(t, getRec, 200)
	var resp map[string]any
	decodeJSON(t, getRec, &resp)
	fields, _ := resp["fields"].(map[string]any)
	if fields["requesting_agent"] != "gd-some-agent" {
		t.Errorf("requesting_agent = %v, want unchanged gd-some-agent after a rejected PATCH", fields["requesting_agent"])
	}
}

// TestEscalationUpdate_RequestingAgentUnrelatedFieldPatchAllowed verifies the
// "B patches an unrelated field, A stays" case the coordinator called out: a
// DIFFERENT static-token agent PATCHing a field other than requesting_agent
// on an escalation it did not file must succeed, and requesting_agent must
// stay attributed to the original filer — the immutability guard only blocks
// patches that actually touch requesting_agent.
func TestEscalationUpdate_RequestingAgentUnrelatedFieldPatchAllowed(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)
	putAgentBead(ms, "gd-agent-a", "gd-agent-a", model.StatusInProgress, time.Now())
	putAgentBead(ms, "gd-agent-b", "gd-agent-b", model.StatusInProgress, time.Now())

	createBody, _ := json.Marshal(map[string]any{
		"title": "grant deploy IAM role",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "manual_intervention",
			"operation_spec": map[string]any{"action": "grant role"},
		},
	})
	createReq := httptest.NewRequest(http.MethodPost, "/v1/beads", bytes.NewReader(createBody))
	createReq.Header.Set("Authorization", "Bearer primary-token")
	createReq.Header.Set("Content-Type", "application/json")
	createReq.Header.Set("X-Actor", "gd-agent-a")
	createReq.Header.Set("X-Operated-By", "slack:U0OPERATOR")
	createRec := httptest.NewRecorder()
	h.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d; body: %s", createRec.Code, createRec.Body.String())
	}
	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	// A different agent, B, PATCHes an unrelated field on A's escalation.
	patchBody, _ := json.Marshal(map[string]any{
		"fields": map[string]any{
			"context": "aws_iam",
		},
	})
	patchReq := httptest.NewRequest(http.MethodPatch, "/v1/beads/"+id, bytes.NewReader(patchBody))
	patchReq.Header.Set("Authorization", "Bearer primary-token")
	patchReq.Header.Set("Content-Type", "application/json")
	patchReq.Header.Set("X-Actor", "gd-agent-b")
	patchReq.Header.Set("X-Operated-By", "slack:U0OPERATOR")
	patchRec := httptest.NewRecorder()
	h.ServeHTTP(patchRec, patchReq)
	if patchRec.Code != http.StatusOK {
		t.Fatalf("patch: expected 200, got %d; body: %s", patchRec.Code, patchRec.Body.String())
	}

	var resp map[string]any
	decodeJSON(t, patchRec, &resp)
	fields, _ := resp["fields"].(map[string]any)
	if fields["requesting_agent"] != "gd-agent-a" {
		t.Errorf("requesting_agent = %v, want unchanged gd-agent-a (the original filer) after B's unrelated-field PATCH", fields["requesting_agent"])
	}
	if fields["context"] != "aws_iam" {
		t.Errorf("context = %v, want aws_iam applied from B's PATCH", fields["context"])
	}
}
