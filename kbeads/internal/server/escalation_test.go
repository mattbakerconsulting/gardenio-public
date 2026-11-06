package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// TestEscalationTypeRegistered verifies that POST /v1/beads with type=escalation
// succeeds (i.e. the type config is in builtinConfigs).
func TestEscalationTypeRegistered(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "create IAM role",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "aws_iam",
			"operation_spec": map[string]any{"role_name": "deploy-bot", "policy": "AdministratorAccess"},
		},
	})
	requireStatus(t, rec, 201)

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	if resp["type"] != "escalation" {
		t.Fatalf("expected type=escalation, got %v", resp["type"])
	}
	if resp["id"] == "" {
		t.Fatal("expected non-empty id")
	}
}

// TestEscalationCreate_CloseRequestingTaskFieldAccepted is the schema-drift
// regression test for gd-91b7zMOR2z: `gb escalation create --requesting-task`
// always stamps close_requesting_task (true/false) on the escalation bead, so
// the type:escalation schema (config.go) MUST declare it or every
// --requesting-task create 400s "unknown field" on a real daemon — the exact
// failure class TestBuiltinEscalationReflagPingbackFieldsDeclared guards for
// the reflag/pingback fields, now extended to this create-time field.
func TestEscalationCreate_CloseRequestingTaskFieldAccepted(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "review PR",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type":        "github_pr_merge",
			"operation_spec":        map[string]any{},
			"requesting_task":       "gd-some-task",
			"close_requesting_task": false,
		},
	})
	requireStatus(t, rec, 201)
}

// TestEscalationTypeConfig verifies that GET /v1/configs/type:escalation returns
// the builtin config with kind=data.
func TestEscalationTypeConfig(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "GET", "/v1/configs/type:escalation", nil)
	requireStatus(t, rec, 200)

	var cfg struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	}
	decodeJSON(t, rec, &cfg)
	if cfg.Key != "type:escalation" {
		t.Fatalf("expected key=type:escalation, got %q", cfg.Key)
	}

	var tc map[string]any
	if err := json.Unmarshal(cfg.Value, &tc); err != nil {
		t.Fatalf("failed to decode type config value: %v", err)
	}
	if tc["kind"] != "data" {
		t.Fatalf("expected kind=data, got %v", tc["kind"])
	}
}

// TestListEscalations verifies GET /v1/escalations returns escalation beads.
func TestListEscalations(t *testing.T) {
	_, _, h := newTestServer()

	// Create an escalation bead.
	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "rotate secrets",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "k8s_secret",
			"operation_spec": map[string]any{"secret": "prod-db", "action": "rotate"},
		},
	})
	requireStatus(t, createRec, 201)

	rec := doJSON(t, h, "GET", "/v1/escalations", nil)
	requireStatus(t, rec, 200)

	var resp struct {
		Escalations []map[string]any `json:"escalations"`
	}
	decodeJSON(t, rec, &resp)
	if len(resp.Escalations) != 1 {
		t.Fatalf("expected 1 escalation, got %d", len(resp.Escalations))
	}
	esc := resp.Escalations[0]["escalation"].(map[string]any)
	if esc["operation_type"] != "k8s_secret" {
		t.Errorf("expected operation_type=k8s_secret, got %v", esc["operation_type"])
	}
}

// TestGetEscalation verifies GET /v1/escalations/{id} returns escalation details.
func TestGetEscalation(t *testing.T) {
	_, _, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "deploy override",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "helm_deploy",
			"operation_spec": map[string]any{"chart": "gardenio", "values": "image.tag=latest"},
		},
	})
	requireStatus(t, createRec, 201)

	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	rec := doJSON(t, h, "GET", "/v1/escalations/"+id, nil)
	requireStatus(t, rec, 200)

	var resp struct {
		Escalation map[string]any `json:"escalation"`
		Issue      map[string]any `json:"issue"`
	}
	decodeJSON(t, rec, &resp)
	if resp.Escalation["operation_type"] != "helm_deploy" {
		t.Errorf("expected operation_type=helm_deploy, got %v", resp.Escalation["operation_type"])
	}
	if resp.Issue["id"] != id {
		t.Errorf("expected issue.id=%s, got %v", id, resp.Issue["id"])
	}
}

// TestGetEscalation_NotFound verifies 404 for missing escalation.
func TestGetEscalation_NotFound(t *testing.T) {
	_, _, h := newTestServer()
	rec := doJSON(t, h, "GET", "/v1/escalations/gd-nonexistent", nil)
	requireStatus(t, rec, 404)
}

// TestApproveEscalation verifies POST /v1/escalations/{id}/approve sets
// approval fields and transitions to in_progress. approved_by is
// SERVER-DERIVED from the authenticated identity (taints ADR §6.3 correction
// 1) — a body-supplied approved_by is logged and ignored, never trusted
// (AUDIT-2026-04-18 §C3).
func TestApproveEscalation(t *testing.T) {
	_, ms, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "create role",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "aws_iam",
			"operation_spec": map[string]any{"role_name": "deploy-bot"},
		},
	})
	requireStatus(t, createRec, 201)

	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	// The body's approved_by ("gd-user-alice") is a spoofing attempt — it must
	// be ignored in favor of the authenticated OIDC caller (defaultTestSubject).
	rec := doJSON(t, h, "POST", "/v1/escalations/"+id+"/approve", map[string]any{
		"approved_by":     "gd-user-alice",
		"approval_method": "web_ui",
	})
	requireStatus(t, rec, 200)

	// Verify the bead has approval fields and is in_progress.
	bead := ms.beads[id]
	if bead == nil {
		t.Fatalf("bead %s not found in store", id)
	}
	if bead.Status != model.StatusInProgress {
		t.Errorf("expected status=in_progress, got %s", bead.Status)
	}

	var fields map[string]any
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		t.Fatalf("failed to decode fields: %v", err)
	}
	if fields["approved_by"] != defaultTestSubject {
		t.Errorf("expected approved_by=%s (server-derived identity, not the spoofed body value), got %v", defaultTestSubject, fields["approved_by"])
	}
	if fields["approval_method"] != "web_ui" {
		t.Errorf("expected approval_method=web_ui, got %v", fields["approval_method"])
	}
	if fields["approved_at"] == nil || fields["approved_at"] == "" {
		t.Error("expected approved_at to be set")
	}
}

// TestApproveEscalation_BodyApprovedByOptional verifies approved_by is no
// longer a required body field (taints ADR §6.3 correction 1: it is derived
// from the authenticated identity, mirroring how decision resolve/cancel
// never hard-require a body-supplied responded_by/canceled_by).
func TestApproveEscalation_BodyApprovedByOptional(t *testing.T) {
	_, ms, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "test",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "test",
			"operation_spec": map[string]any{},
		},
	})
	requireStatus(t, createRec, 201)
	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	rec := doJSON(t, h, "POST", "/v1/escalations/"+id+"/approve", map[string]any{
		"approval_method": "web_ui",
	})
	requireStatus(t, rec, 200)

	var fields map[string]any
	if err := json.Unmarshal(ms.beads[id].Fields, &fields); err != nil {
		t.Fatalf("failed to decode fields: %v", err)
	}
	if fields["approved_by"] != defaultTestSubject {
		t.Errorf("expected approved_by=%s (derived from auth identity), got %v", defaultTestSubject, fields["approved_by"])
	}
}

// TestApproveEscalation_RequiresOIDC verifies the taints ADR §6.3 correction
// 2: a caller with no OIDC-derived human identity — here a fully anonymous
// caller, same as a bare static/fleet token with no X-Operated-By-threaded
// human — is refused outright with 403, closing the gap named by decision
// gd-CxfzKo5VOJ ("any static-token agent can approve any escalation").
func TestApproveEscalation_RequiresOIDC(t *testing.T) {
	_, ms, h := newTestServerNoAuth()

	fields, _ := json.Marshal(map[string]any{
		"operation_type": "test",
		"operation_spec": map[string]any{},
	})
	ms.beads["gd-esc-noauth"] = &model.Bead{
		ID: "gd-esc-noauth", Kind: model.KindData, Type: model.TypeEscalation,
		Title: "test", Status: model.StatusOpen, Fields: fields,
	}

	rec := doJSON(t, h, "POST", "/v1/escalations/gd-esc-noauth/approve", map[string]any{
		"approved_by": "gd-user-alice",
	})
	requireStatus(t, rec, 403)

	// The escalation must be untouched — no approval fields, still open.
	bead := ms.beads["gd-esc-noauth"]
	if bead.Status != model.StatusOpen {
		t.Errorf("expected escalation to remain open after refused approval, got %s", bead.Status)
	}
	var got map[string]any
	if err := json.Unmarshal(bead.Fields, &got); err != nil {
		t.Fatalf("failed to decode fields: %v", err)
	}
	if _, ok := got["approved_by"]; ok {
		t.Errorf("expected no approved_by field to be recorded, got %v", got["approved_by"])
	}
}

// TestApproveEscalation_StaticTokenWithOperatedByHuman pins gd-ZjiSd9Kzio
// round 3's removal of correction 2's X-Operated-By relay path: a
// static-primary-token caller that threads a human via X-Operated-By in
// "oidc:<email>" form is now refused, the same as any other non-OIDC caller.
// Prior to round 3 this was accepted — the theory was that any forgery was
// "bounded by requireRole", but handleApproveEscalation is
// requireRole(RoleOperator, …), which short-circuits with NO check at all
// for a non-OIDC caller (RoleOperator < RoleAdmin), so the header led
// straight into replayEscalation's withMachineWrite total bypass: any
// holder of the shared primary static token — i.e. any agent pod — could
// self-approve (and fully execute) its own privileged escalation by naming
// an arbitrary "oidc:<email>" in the header. See escalationApprover's doc
// comment for the follow-up needed to restore a relay path for
// beads-viewer specifically. Drives the same real AuthMiddleware path as
// doStaticTokenWithOperator's decision-resolve regression test
// (ka-0000001759) to prove the refusal happens at this layer, not just in
// a synthetic unit context.
func TestApproveEscalation_StaticTokenWithOperatedByHuman(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	h := srv.NewHTTPHandler(nil, withTestToken(nil, "primary-token"))

	fields, _ := json.Marshal(map[string]any{
		"operation_type": "test",
		"operation_spec": map[string]any{},
	})
	ms.beads["gd-esc-viewer"] = &model.Bead{
		ID: "gd-esc-viewer", Kind: model.KindData, Type: model.TypeEscalation,
		Title: "test", Status: model.StatusOpen, Fields: fields,
	}

	const operator = "oidc:matt@example.com"
	rec := doStaticTokenWithOperator(t, h, "POST", "/v1/escalations/gd-esc-viewer/approve",
		operator, map[string]any{"approved_by": "gd-user-alice"})
	requireStatus(t, rec, 403)

	// The escalation must be untouched — no approval fields, still open, no
	// replay executed against the (once-forgeable) approver identity.
	bead := ms.beads["gd-esc-viewer"]
	if bead.Status != model.StatusOpen {
		t.Errorf("expected escalation to remain open after refused approval, got %s", bead.Status)
	}
	var got map[string]any
	if err := json.Unmarshal(bead.Fields, &got); err != nil {
		t.Fatalf("failed to decode fields: %v", err)
	}
	if _, ok := got["approved_by"]; ok {
		t.Errorf("expected no approved_by field to be recorded, got %v", got["approved_by"])
	}
}

// TestApproveEscalation_NonAdminOIDCApprover_Refused pins the other
// gd-ZjiSd9Kzio round 3 refusal-test ask: a genuinely OIDC-authenticated
// caller who does not hold RoleOperator (the route's own min_role,
// requireRole(RoleOperator, handleApproveEscalation)) is refused by
// requireRole itself, before escalationApprover ever runs — this pins that
// behavior at the same real-handler layer as the static-token test above,
// guarding against a future registration change accidentally widening the
// route to viewers.
func TestApproveEscalation_NonAdminOIDCApprover_Refused(t *testing.T) {
	verifier := &stubOIDCVerifier{
		email: "viewer@example.com",
		roles: []string{RoleViewer},
	}
	h, ms := newSpawnAuthzTestServer(verifier)

	fields, _ := json.Marshal(map[string]any{
		"operation_type": "test",
		"operation_spec": map[string]any{},
	})
	ms.beads["gd-esc-viewer2"] = &model.Bead{
		ID: "gd-esc-viewer2", Kind: model.KindData, Type: model.TypeEscalation,
		Title: "test", Status: model.StatusOpen, Fields: fields,
	}

	rec := doOIDC(t, h, "POST", "/v1/escalations/gd-esc-viewer2/approve",
		map[string]any{"approved_by": "gd-user-alice"})
	requireStatus(t, rec, 403)
}

// TestApproveEscalation_FourEyesBlocksSameOperator verifies taints ADR §6.3
// correction 3: when the escalation's policy demands four-eyes
// (requires_four_eyes), an approver that resolves — AFTER alias resolution —
// to the same operator as the escalation's recorded requester is refused,
// even though the raw identity strings differ ("slack:U123" vs the OIDC
// approver's bare email) — the two are linked to one operator row.
func TestApproveEscalation_FourEyesBlocksSameOperator(t *testing.T) {
	_, ms, h := newTestServer()

	// Link the requester's slack identity and the approver's OIDC identity to
	// the same operator row, as operator_aliases would after a real link.
	ms.operatorAliases = map[string]int64{
		"slack\x00U123":                 1,
		"oidc\x00" + defaultTestSubject: 1,
	}

	fields, _ := json.Marshal(map[string]any{
		"operation_type":     "test",
		"operation_spec":     map[string]any{},
		"requires_four_eyes": true,
	})
	ms.beads["gd-esc-4eyes-same"] = &model.Bead{
		ID: "gd-esc-4eyes-same", Kind: model.KindData, Type: model.TypeEscalation,
		Title: "test", Status: model.StatusOpen, Fields: fields, OperatedBy: "slack:U123",
	}

	rec := doJSON(t, h, "POST", "/v1/escalations/gd-esc-4eyes-same/approve", map[string]any{})
	requireStatus(t, rec, 403)

	bead := ms.beads["gd-esc-4eyes-same"]
	if bead.Status != model.StatusOpen {
		t.Errorf("expected escalation to remain open after four-eyes refusal, got %s", bead.Status)
	}
}

// TestApproveEscalation_FourEyesAllowsDifferentOperator verifies the
// four-eyes check only blocks a genuine requester==approver match: a
// requester linked to a DIFFERENT operator than the approver is let through.
func TestApproveEscalation_FourEyesAllowsDifferentOperator(t *testing.T) {
	_, ms, h := newTestServer()

	ms.operatorAliases = map[string]int64{
		"slack\x00U123":                 1,
		"oidc\x00" + defaultTestSubject: 2,
	}

	fields, _ := json.Marshal(map[string]any{
		"operation_type":     "test",
		"operation_spec":     map[string]any{},
		"requires_four_eyes": true,
	})
	ms.beads["gd-esc-4eyes-diff"] = &model.Bead{
		ID: "gd-esc-4eyes-diff", Kind: model.KindData, Type: model.TypeEscalation,
		Title: "test", Status: model.StatusOpen, Fields: fields, OperatedBy: "slack:U123",
	}

	rec := doJSON(t, h, "POST", "/v1/escalations/gd-esc-4eyes-diff/approve", map[string]any{})
	requireStatus(t, rec, 200)
}

// TestApproveEscalation_FourEyesNotEnforcedWithoutFlag verifies the
// four-eyes check is opt-in per escalation: without requires_four_eyes set,
// a requester==approver match is unaffected (no behavior change for existing
// escalations that never declared the policy).
func TestApproveEscalation_FourEyesNotEnforcedWithoutFlag(t *testing.T) {
	_, ms, h := newTestServer()

	fields, _ := json.Marshal(map[string]any{
		"operation_type": "test",
		"operation_spec": map[string]any{},
	})
	ms.beads["gd-esc-4eyes-off"] = &model.Bead{
		ID: "gd-esc-4eyes-off", Kind: model.KindData, Type: model.TypeEscalation,
		Title: "test", Status: model.StatusOpen, Fields: fields, OperatedBy: defaultTestSubject,
	}

	rec := doJSON(t, h, "POST", "/v1/escalations/gd-esc-4eyes-off/approve", map[string]any{})
	requireStatus(t, rec, 200)
}

// TestRejectEscalation verifies POST /v1/escalations/{id}/reject closes the bead.
func TestRejectEscalation(t *testing.T) {
	_, ms, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "risky deploy",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "helm_deploy",
			"operation_spec": map[string]any{"release": "prod"},
		},
	})
	requireStatus(t, createRec, 201)
	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	rec := doJSON(t, h, "POST", "/v1/escalations/"+id+"/reject", map[string]any{
		"rejected_by": "gd-user-bob",
		"reason":      "too risky for Friday deploy",
	})
	requireStatus(t, rec, 200)

	bead := ms.beads[id]
	if bead == nil {
		t.Fatalf("bead %s not found in store", id)
	}
	if bead.Status != model.StatusClosed {
		t.Errorf("expected status=closed, got %s", bead.Status)
	}

	var fields map[string]any
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		t.Fatalf("failed to decode fields: %v", err)
	}
	if fields["result_status"] != "failure" {
		t.Errorf("expected result_status=failure, got %v", fields["result_status"])
	}
}

// TestCaptureEscalationResult verifies POST /v1/escalations/{id}/result
// records result and closes the bead.
func TestCaptureEscalationResult(t *testing.T) {
	_, ms, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "create IAM role",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "aws_iam",
			"operation_spec": map[string]any{"role_name": "deploy-bot"},
		},
	})
	requireStatus(t, createRec, 201)
	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	rec := doJSON(t, h, "POST", "/v1/escalations/"+id+"/result", map[string]any{
		"executor":         "gd-agent-local",
		"execution_target": "local",
		"result":           map[string]any{"role_arn": "arn:aws:iam::123:role/deploy-bot"},
		"result_status":    "success",
	})
	requireStatus(t, rec, 200)

	bead := ms.beads[id]
	if bead == nil {
		t.Fatalf("bead %s not found in store", id)
	}
	if bead.Status != model.StatusClosed {
		t.Errorf("expected status=closed, got %s", bead.Status)
	}

	var fields map[string]any
	if err := json.Unmarshal(bead.Fields, &fields); err != nil {
		t.Fatalf("failed to decode fields: %v", err)
	}
	if fields["result_status"] != "success" {
		t.Errorf("expected result_status=success, got %v", fields["result_status"])
	}
	// P7 Task 10: executor is the authenticated caller, never the body, and
	// execution_target is frozen at filing.
	if fields["executor"] != "test@example.com" {
		t.Errorf("expected executor=test@example.com (server-stamped), got %v", fields["executor"])
	}
	if fields["execution_target"] != nil {
		t.Errorf("expected body execution_target ignored, got %v", fields["execution_target"])
	}
}

// TestCaptureEscalationResult_MissingStatus verifies 400 when result_status is missing.
func TestCaptureEscalationResult_MissingStatus(t *testing.T) {
	_, _, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "test",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "test",
			"operation_spec": map[string]any{},
		},
	})
	requireStatus(t, createRec, 201)
	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	rec := doJSON(t, h, "POST", "/v1/escalations/"+id+"/result", map[string]any{
		"executor": "gd-agent",
		"result":   map[string]any{"output": "done"},
	})
	requireStatus(t, rec, 400)
}

// TestCaptureEscalationResult_InvalidStatus verifies 400 for invalid result_status.
func TestCaptureEscalationResult_InvalidStatus(t *testing.T) {
	_, _, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "test",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "test",
			"operation_spec": map[string]any{},
		},
	})
	requireStatus(t, createRec, 201)
	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	rec := doJSON(t, h, "POST", "/v1/escalations/"+id+"/result", map[string]any{
		"result_status": "unknown",
	})
	requireStatus(t, rec, 400)
}

// TestGenericCloseEscalationDiverted verifies the resolve-surface guard
// (ka-0000005516): a generic POST /v1/beads/{id}/close on an OPEN escalation is
// refused with 409 and an actionable redirect to the approve/reject/result
// verbs, and — crucially — the escalation stays OPEN (a plain close would skip
// operation_spec replay and wrongly release close-on-escalation wrappers).
func TestGenericCloseEscalationDiverted(t *testing.T) {
	_, ms, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "rotate secret",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "create",
			"operation_spec": map[string]any{"title": "replayed side effect"},
		},
	})
	requireStatus(t, createRec, 201)
	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	rec := doJSON(t, h, "POST", "/v1/beads/"+id+"/close", map[string]any{})
	requireStatus(t, rec, 409)
	if body := rec.Body.String(); !strings.Contains(body, "escalation approve|reject|close") {
		t.Errorf("409 body should point at the resolve verbs; got: %s", body)
	}
	if got := ms.beads[id].Status; got != model.StatusOpen {
		t.Errorf("escalation must stay open after a diverted generic close, got status=%s", got)
	}
}

// TestGenericCloseEscalationClosedIsIdempotent verifies the guard does NOT
// break idempotent close: once an escalation is legitimately resolved (here via
// reject, which closes it), a subsequent generic close returns 200 rather than
// the 409 divert — the guard only diverts OPEN/in_progress escalations.
func TestGenericCloseEscalationClosedIsIdempotent(t *testing.T) {
	_, _, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "risky deploy",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "helm_deploy",
			"operation_spec": map[string]any{"release": "prod"},
		},
	})
	requireStatus(t, createRec, 201)
	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	// Resolve it through the dedicated reject verb (closes the escalation).
	rejectRec := doJSON(t, h, "POST", "/v1/escalations/"+id+"/reject", map[string]any{
		"rejected_by": "gd-user-bob",
		"reason":      "not needed",
	})
	requireStatus(t, rejectRec, 200)

	// A generic close of the now-closed escalation is idempotent, not diverted.
	rec := doJSON(t, h, "POST", "/v1/beads/"+id+"/close", map[string]any{})
	requireStatus(t, rec, 200)
}

// escalationDivertFixture wires a test server over a capture publisher so a test
// can assert both the HTTP status AND that NO closed event was emitted on the
// diverted PATCH path (ka-0000006558, defect 2).
func escalationDivertFixture() (*mockStore, *capturePublisher, http.Handler) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &capturePublisher{})
	s.userProvisioner = nil
	s.SetForwardAuthClients([]string{testBrowserClient})
	inner := s.NewHTTPHandler(&bearerSubjectVerifier{})
	return ms, s.publisher.(*capturePublisher), defaultBearerMiddleware(inner)
}

// TestPatchCloseEscalationDiverted verifies the guard parity fix: a
// `PATCH /v1/beads/{id}` that sets status=closed on an OPEN escalation is
// refused with 409 and the escalation stays open — matching the two generic
// close paths (gRPC CloseBead, HTTP handleCloseBead). Before the fix this PATCH
// terminated the escalation invisibly: no operation_spec replay, no
// result_status, and (because updateBead only publishes TopicBeadUpdated) no
// closed event at all (ka-0000006558, defect 2).
func TestPatchCloseEscalationDiverted(t *testing.T) {
	ms, cap, h := escalationDivertFixture()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "rotate secret",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "credential_rotation",
			"operation_spec": map[string]any{"secret": "prod-db"},
		},
	})
	requireStatus(t, createRec, 201)
	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	rec := doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{"status": "closed"})
	requireStatus(t, rec, 409)
	if body := rec.Body.String(); !strings.Contains(body, "escalation approve|reject|close") {
		t.Errorf("409 body should point at the resolve verbs; got: %s", body)
	}
	if got := ms.beads[id].Status; got != model.StatusOpen {
		t.Errorf("escalation must stay open after a diverted PATCH close, got status=%s", got)
	}

	// The divert must fire BEFORE any mutation/publish — no closed event, and
	// (because the refusal short-circuits before store.UpdateBead) no update
	// event for the refused write either.
	cap.mu.Lock()
	defer cap.mu.Unlock()
	for _, topic := range cap.topics {
		if topic == events.TopicBeadClosed {
			t.Errorf("a diverted PATCH close must NOT publish %s", events.TopicBeadClosed)
		}
	}
}

// TestPatchNonCloseEscalationAllowed verifies the guard is narrow: a PATCH that
// does NOT transition an open escalation into closed (here a reflag/pingback
// field update) is untouched — the divert only fires on the status→closed
// transition, mirroring escalationBlocksGenericClose (ka-0000006558).
func TestPatchNonCloseEscalationAllowed(t *testing.T) {
	_, _, h := escalationDivertFixture()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "risky deploy",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "deploy",
			"operation_spec": map[string]any{"release": "prod"},
		},
	})
	requireStatus(t, createRec, 201)
	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	// A non-status field patch must succeed — the guard is scoped to the
	// status→closed transition, not any write to an open escalation.
	rec := doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{
		"fields": map[string]any{"pingback_count": "1"},
	})
	requireStatus(t, rec, 200)
}

// TestPatchClosedEscalationIdempotent verifies a status=closed PATCH of an
// ALREADY-closed escalation is not diverted — the guard only diverts
// open/in_progress escalations, so a racing cleanup re-write stays harmless
// (mirrors TestGenericCloseEscalationClosedIsIdempotent for the PATCH path).
func TestPatchClosedEscalationIdempotent(t *testing.T) {
	_, _, h := escalationDivertFixture()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "risky deploy",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "deploy",
			"operation_spec": map[string]any{"release": "prod"},
		},
	})
	requireStatus(t, createRec, 201)
	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	// Resolve through the dedicated reject verb (closes the escalation).
	rejectRec := doJSON(t, h, "POST", "/v1/escalations/"+id+"/reject", map[string]any{
		"rejected_by": "gd-user-bob",
		"reason":      "not needed",
	})
	requireStatus(t, rejectRec, 200)

	// A status=closed PATCH of the now-closed escalation is idempotent, not
	// diverted (the guard short-circuits an already-closed bead well before the
	// divert check via dropTransientClosedFields + hasAnyChange).
	rec := doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{"status": "closed"})
	requireStatus(t, rec, 200)
}

// TestEscalationV2Fields verifies that V2 fields (execution_script,
// credential_requirements, expected_outputs, sensitivity) are accepted and
// returned via the escalation API.
func TestEscalationV2Fields(t *testing.T) {
	_, _, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "rotate prod DB creds",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type":   "credential_rotation",
			"operation_spec":   map[string]any{"target": "prod-primary"},
			"execution_script": "#!/bin/bash\nset -euo pipefail\nNEW_PASS=$(openssl rand -base64 32)\necho $NEW_PASS",
			"credential_requirements": []map[string]any{
				{"type": "aws_role", "identifier": "arn:aws:iam::123:role/rds-admin", "purpose": "Modify RDS password", "scope": "rds:ModifyDBInstance"},
			},
			"data_requirements": []map[string]any{
				{"name": "DB_INSTANCE_ID", "type": "env_var", "description": "RDS instance ID", "default": "prod-primary"},
			},
			"expected_outputs": []map[string]any{
				{"name": "new_password", "description": "Rotated DB password", "sensitivity": "secret", "format": "string"},
				{"name": "rds_status", "description": "Final RDS status", "sensitivity": "public", "format": "string"},
			},
			"sensitivity": "secret",
		},
	})
	requireStatus(t, createRec, 201)

	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	// Fetch via escalation API and verify V2 fields are present.
	rec := doJSON(t, h, "GET", "/v1/escalations/"+id, nil)
	requireStatus(t, rec, 200)

	var resp struct {
		Escalation map[string]any `json:"escalation"`
	}
	decodeJSON(t, rec, &resp)
	esc := resp.Escalation

	if esc["execution_script"] == nil || esc["execution_script"] == "" {
		t.Error("expected execution_script to be present")
	}
	if esc["sensitivity"] != "secret" {
		t.Errorf("expected sensitivity=secret, got %v", esc["sensitivity"])
	}
	if esc["credential_requirements"] == nil {
		t.Error("expected credential_requirements to be present")
	}
	if esc["data_requirements"] == nil {
		t.Error("expected data_requirements to be present")
	}
	if esc["expected_outputs"] == nil {
		t.Error("expected expected_outputs to be present")
	}
}

// TestApproveEscalation_InvalidMethod verifies 400 for invalid approval_method.
// The body's approval_method is ignored, not validated: the daemon derives
// it from the identity (P7 Task 15, spec D12).
func TestApproveEscalation_BodyMethodIgnored(t *testing.T) {
	_, _, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "test",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "test",
			"operation_spec": map[string]any{},
		},
	})
	requireStatus(t, createRec, 201)
	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	rec := doJSON(t, h, "POST", "/v1/escalations/"+id+"/approve", map[string]any{
		"approved_by":     "gd-user1",
		"approval_method": "magic_wand",
	})
	requireStatus(t, rec, 200)
	var resp struct {
		Issue struct {
			Fields map[string]any `json:"fields"`
		} `json:"issue"`
	}
	decodeJSON(t, rec, &resp)
	if m := resp.Issue.Fields["approval_method"]; m != "web_ui" {
		t.Errorf("approval_method = %v, want web_ui", m)
	}
}

// --- Auto-replay tests ---

func TestApproveEscalation_ReplayCreate(t *testing.T) {
	_, ms, h := newTestServer()

	// Create an escalation that stores a create operation.
	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Escalation: create on type agent denied",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "create",
			"operation_spec": map[string]any{
				"title":  "Deploy Bot",
				"type":   "agent",
				"labels": []string{"project:monorepo"},
				"fields": map[string]any{
					"agent":   "deploy-bot",
					"role":    "crew",
					"project": "monorepo",
				},
			},
			"requesting_agent": "alice@example.com",
			"context":          "insufficient permissions",
		},
	})
	requireStatus(t, createRec, 201)

	var created map[string]any
	decodeJSON(t, createRec, &created)
	escID := created["id"].(string)

	beadCountBefore := len(ms.beads)

	// Approve → should auto-replay the create.
	rec := doJSON(t, h, "POST", "/v1/escalations/"+escID+"/approve", map[string]any{
		"approved_by": "operator@example.com",
	})
	requireStatus(t, rec, 200)

	var resp map[string]any
	decodeJSON(t, rec, &resp)

	// Verify replay result is in the response.
	replay, ok := resp["replay"].(map[string]any)
	if !ok {
		t.Fatal("expected replay field in response")
	}
	if replay["status"] != "success" {
		t.Errorf("expected replay status=success, got %v", replay["status"])
	}
	if replay["bead_id"] == nil || replay["bead_id"] == "" {
		t.Error("expected replay bead_id to be set")
	}

	// Verify new beads were created: the replayed agent bead + audit bead.
	newBeads := len(ms.beads) - beadCountBefore
	if newBeads < 1 {
		t.Errorf("expected at least 1 new bead after replay, got %d", newBeads)
	}

	// Verify the escalation bead is closed.
	escalation := ms.beads[escID]
	if escalation.Status != model.StatusClosed {
		t.Errorf("expected escalation status=closed, got %s", escalation.Status)
	}

	// Verify result fields on escalation.
	var fields map[string]any
	if err := json.Unmarshal(escalation.Fields, &fields); err != nil {
		t.Fatalf("failed to decode escalation fields: %v", err)
	}
	if fields["result_status"] != "success" {
		t.Errorf("expected result_status=success, got %v", fields["result_status"])
	}
	if fields["executor"] != "auto-replay" {
		t.Errorf("expected executor=auto-replay, got %v", fields["executor"])
	}
}

// TestReplayEscalation_ConcurrentCallsReplayOnlyOnce pins the double-replica
// idempotency guard from the Phase 2 2-replica spike (gd-XxrObXvc8r):
// replayEscalation is called twice for the same escalation, standing in for
// a double-click, client retry, or two approve requests landing on two
// different replicas. Only the first call may execute the operation; the
// second must observe the lost claim race and report "skipped" without
// creating a second bead.
func TestReplayEscalation_ConcurrentCallsReplayOnlyOnce(t *testing.T) {
	s, ms, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Escalation: create on type agent denied",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "create",
			"operation_spec": map[string]any{
				"title":  "Deploy Bot",
				"type":   "agent",
				"labels": []string{"project:monorepo"},
				"fields": map[string]any{
					"agent":   "deploy-bot",
					"role":    "crew",
					"project": "monorepo",
				},
			},
			"requesting_agent": "alice@example.com",
			"context":          "insufficient permissions",
		},
	})
	requireStatus(t, createRec, 201)

	var created map[string]any
	decodeJSON(t, createRec, &created)
	escID := created["id"].(string)
	escalation := ms.beads[escID]

	beadCountBefore := len(ms.beads)

	first := s.replayEscalation(context.Background(), escalation, "operator@example.com", nil)
	if first == nil || first["status"] != "success" {
		t.Fatalf("first replay: expected status=success, got %v", first)
	}
	if len(ms.beads) == beadCountBefore {
		t.Fatal("expected the first replay to create at least one new bead")
	}
	beadCountAfterFirst := len(ms.beads)

	second := s.replayEscalation(context.Background(), escalation, "operator@example.com", nil)
	if second == nil || second["status"] != "skipped" {
		t.Fatalf("second (concurrent) replay: expected status=skipped, got %v", second)
	}
	if second["replayed"] != false {
		t.Errorf("second replay: expected replayed=false, got %v", second["replayed"])
	}

	if got := len(ms.beads); got != beadCountAfterFirst {
		t.Errorf("second replay created beads (count %d -> %d), want no change — the claim should have blocked it", beadCountAfterFirst, got)
	}
}

// TestApproveEscalation_ReplayCreate_StampsSpawnProvenance verifies that a
// replayed agent create is stamped with spawned_by (the escalation id),
// spawn_approval_mode and spawn_approved_by, derived server-side from the
// approval rather than trusted from the originally-denied create body
// (design doc gd-sGW5Y1B8sg §5, gd-h7fgTgcuyn).
func TestApproveEscalation_ReplayCreate_StampsSpawnProvenance(t *testing.T) {
	_, ms, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Escalation: create on type agent denied",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "create",
			"operation_spec": map[string]any{
				"title":  "Deploy Bot",
				"type":   "agent",
				"labels": []string{"project:monorepo"},
				"fields": map[string]any{
					"agent":      "deploy-bot",
					"role":       "k8s-live",
					"project":    "monorepo",
					"spawned_by": "should-be-overwritten",
				},
			},
			"requesting_agent": "alice@example.com",
			"context":          "insufficient permissions",
		},
	})
	requireStatus(t, createRec, 201)

	var created map[string]any
	decodeJSON(t, createRec, &created)
	escID := created["id"].(string)

	rec := doJSON(t, h, "POST", "/v1/escalations/"+escID+"/approve", map[string]any{
		"approved_by": "oidc:operator@example.com",
	})
	requireStatus(t, rec, 200)

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	replay := resp["replay"].(map[string]any)
	beadID, _ := replay["bead_id"].(string)
	if beadID == "" {
		t.Fatal("expected replay bead_id to be set")
	}

	spawned := ms.beads[beadID]
	if spawned == nil {
		t.Fatalf("replayed bead %s not found", beadID)
	}
	var spawnedFields map[string]any
	if err := json.Unmarshal(spawned.Fields, &spawnedFields); err != nil {
		t.Fatalf("failed to decode spawned bead fields: %v", err)
	}
	if spawnedFields["spawned_by"] != escID {
		t.Errorf("spawned_by = %v, want escalation id %q", spawnedFields["spawned_by"], escID)
	}
	if spawnedFields["spawn_approval_mode"] != "human" {
		t.Errorf("spawn_approval_mode = %v, want %q", spawnedFields["spawn_approval_mode"], "human")
	}
	// gd-731gBeGgl7: the approver is derived from the authenticated identity
	// (defaultTestSubject here); the body's "oidc:operator@example.com" is
	// logged and ignored, so it must NOT appear in the stamp.
	if spawnedFields["spawn_approved_by"] != defaultTestSubject {
		t.Errorf("spawn_approved_by = %v, want identity-derived %q (body value must be ignored)", spawnedFields["spawn_approved_by"], defaultTestSubject)
	}
}

// TestApproveEscalation_ReplayCreate_AutoApprovalMode verifies that a
// system:-prefixed approver (the policy evaluator sentinel) is stamped as
// spawn_approval_mode=auto rather than human.
func TestApproveEscalation_ReplayCreate_AutoApprovalMode(t *testing.T) {
	_, ms, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Escalation: create on type agent denied",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "create",
			"operation_spec": map[string]any{
				"title":  "Deploy Bot",
				"type":   "agent",
				"labels": []string{"project:monorepo"},
				"fields": map[string]any{
					"agent":   "deploy-bot",
					"role":    "k8s-live",
					"project": "monorepo",
				},
			},
			"requesting_agent": "alice@example.com",
			"context":          "insufficient permissions",
		},
	})
	requireStatus(t, createRec, 201)

	var created map[string]any
	decodeJSON(t, createRec, &created)
	escID := created["id"].(string)

	// gd-731gBeGgl7: over HTTP the approver is identity-derived, so a human
	// caller cannot claim a system: approver through the body. The body
	// value is ignored and the stamp records a HUMAN approval by the caller.
	rec := doJSON(t, h, "POST", "/v1/escalations/"+escID+"/approve", map[string]any{
		"approved_by": "system:spawn-policy",
	})
	requireStatus(t, rec, 200)

	var resp map[string]any
	decodeJSON(t, rec, &resp)
	replay := resp["replay"].(map[string]any)
	beadID, _ := replay["bead_id"].(string)

	spawned := ms.beads[beadID]
	var spawnedFields map[string]any
	if err := json.Unmarshal(spawned.Fields, &spawnedFields); err != nil {
		t.Fatalf("failed to decode spawned bead fields: %v", err)
	}
	if spawnedFields["spawn_approval_mode"] != "human" {
		t.Errorf("HTTP approve: spawn_approval_mode = %v, want %q (body-claimed system actor must be ignored)", spawnedFields["spawn_approval_mode"], "human")
	}
	if spawnedFields["spawn_approved_by"] != defaultTestSubject {
		t.Errorf("HTTP approve: spawn_approved_by = %v, want %q", spawnedFields["spawn_approved_by"], defaultTestSubject)
	}
}

// The "auto" approval mode is reserved for a server-side policy evaluator
// (taints ADR §6.2, system:<name> actor) that calls the replay directly; it
// is not reachable through the HTTP approve handler. Exercise the internal
// path so the stamp contract for auto approvals stays covered.
func TestReplayEscalation_SystemApprover_StampsAutoMode(t *testing.T) {
	s, ms, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Escalation: create on type agent denied",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "create",
			"operation_spec": map[string]any{
				"title":  "Deploy Bot",
				"type":   "agent",
				"labels": []string{"project:monorepo"},
				"fields": map[string]any{
					"agent":   "deploy-bot",
					"role":    "k8s-live",
					"project": "monorepo",
				},
			},
			"requesting_agent": "alice@example.com",
			"context":          "insufficient permissions",
		},
	})
	requireStatus(t, createRec, 201)
	var created map[string]any
	decodeJSON(t, createRec, &created)
	escID := created["id"].(string)

	ctx := WithSystemOperator(context.Background(), "spawn-policy")
	result := s.replayEscalation(ctx, ms.beads[escID], "system:spawn-policy", nil)
	beadID, _ := result["bead_id"].(string)
	if beadID == "" {
		t.Fatalf("replay did not create a bead: %v", result)
	}
	var spawnedFields map[string]any
	if err := json.Unmarshal(ms.beads[beadID].Fields, &spawnedFields); err != nil {
		t.Fatalf("failed to decode spawned bead fields: %v", err)
	}
	if spawnedFields["spawn_approval_mode"] != "auto" {
		t.Errorf("spawn_approval_mode = %v, want %q", spawnedFields["spawn_approval_mode"], "auto")
	}
	if spawnedFields["spawn_approved_by"] != "system:spawn-policy" {
		t.Errorf("spawn_approved_by = %v, want %q", spawnedFields["spawn_approved_by"], "system:spawn-policy")
	}
}

func TestApproveEscalation_ReplayUpdate(t *testing.T) {
	_, ms, h := newTestServer()

	// Create the target bead first.
	targetRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Existing task",
		"type":  "task",
	})
	requireStatus(t, targetRec, 201)
	var target map[string]any
	decodeJSON(t, targetRec, &target)
	targetID := target["id"].(string)

	// Create an escalation for an update operation.
	escRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Escalation: update denied",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "update",
			"target_bead_id": targetID,
			"operation_spec": map[string]any{
				"title": "Updated title via replay",
			},
			"requesting_agent": "bob@example.com",
		},
	})
	requireStatus(t, escRec, 201)
	var escCreated map[string]any
	decodeJSON(t, escRec, &escCreated)
	escID := escCreated["id"].(string)

	// Approve → should auto-replay the update.
	rec := doJSON(t, h, "POST", "/v1/escalations/"+escID+"/approve", map[string]any{
		"approved_by": "operator@example.com",
	})
	requireStatus(t, rec, 200)

	// Verify the target bead was updated.
	targetBead := ms.beads[targetID]
	if targetBead.Title != "Updated title via replay" {
		t.Errorf("expected title='Updated title via replay', got %q", targetBead.Title)
	}

	// Verify escalation is closed.
	escalation := ms.beads[escID]
	if escalation.Status != model.StatusClosed {
		t.Errorf("expected escalation status=closed, got %s", escalation.Status)
	}
}

func TestApproveEscalation_ReplayClose(t *testing.T) {
	_, ms, h := newTestServer()

	// Create the target bead first.
	targetRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Task to close",
		"type":  "task",
	})
	requireStatus(t, targetRec, 201)
	var target map[string]any
	decodeJSON(t, targetRec, &target)
	targetID := target["id"].(string)

	// Create an escalation for a close operation.
	escRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Escalation: close denied",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "close",
			"target_bead_id": targetID,
			"operation_spec": map[string]any{
				"closed_by": "alice@example.com",
			},
			"requesting_agent": "alice@example.com",
		},
	})
	requireStatus(t, escRec, 201)
	var escCreated map[string]any
	decodeJSON(t, escRec, &escCreated)
	escID := escCreated["id"].(string)

	// Approve → should auto-replay the close.
	rec := doJSON(t, h, "POST", "/v1/escalations/"+escID+"/approve", map[string]any{
		"approved_by": "operator@example.com",
	})
	requireStatus(t, rec, 200)

	// Verify the target bead was closed.
	targetBead := ms.beads[targetID]
	if targetBead.Status != model.StatusClosed {
		t.Errorf("expected target status=closed, got %s", targetBead.Status)
	}

	// Verify escalation is closed.
	escalation := ms.beads[escID]
	if escalation.Status != model.StatusClosed {
		t.Errorf("expected escalation status=closed, got %s", escalation.Status)
	}
}

func TestApproveEscalation_NoReplayWithoutSpec(t *testing.T) {
	_, ms, h := newTestServer()

	// Create an escalation with a non-bead operation (e.g. aws_iam).
	// Has operation_spec (required) but it's not a create/update/close type.
	escRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "Manual escalation",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type": "aws_iam",
			"operation_spec": map[string]any{"role_name": "deploy-bot"},
		},
	})
	requireStatus(t, escRec, 201)
	var escCreated map[string]any
	decodeJSON(t, escRec, &escCreated)
	escID := escCreated["id"].(string)

	// Approve → should NOT attempt replay.
	rec := doJSON(t, h, "POST", "/v1/escalations/"+escID+"/approve", map[string]any{
		"approved_by": "operator@example.com",
	})
	requireStatus(t, rec, 200)

	var resp map[string]any
	decodeJSON(t, rec, &resp)

	// Verify no replay field in response.
	if resp["replay"] != nil {
		t.Errorf("expected no replay field for manual escalation, got %v", resp["replay"])
	}

	// Escalation should be in_progress (not auto-closed since no replay).
	escalation := ms.beads[escID]
	if escalation.Status != model.StatusInProgress {
		t.Errorf("expected escalation status=in_progress, got %s", escalation.Status)
	}
}

// TestEscalationRequestingPlanPatch reproduces ka-0000000658: `gb escalation
// create --requesting-task <plan-anchored task>` resolves the nearest
// plan-labeled epic ancestor and PATCHes it onto the escalation as
// requesting_plan (cmd/gb/escalation.go, feat ka-0000006860) so the plan the
// escalation blocked is pinned even if the step is later reparented. The field
// was never declared on type:escalation, so that PATCH 400'd "unknown field"
// and every plan-anchored escalation silently lost its plan pin.
func TestEscalationRequestingPlanPatch(t *testing.T) {
	_, _, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "grant deploy IAM role",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type":  "manual_intervention",
			"operation_spec":  map[string]any{"action": "grant role"},
			"requesting_task": "gd-step1",
		},
	})
	requireStatus(t, createRec, 201)

	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	// The post-create plan pin the gb writer issues.
	patchRec := doJSON(t, h, "PATCH", "/v1/beads/"+id, map[string]any{
		"fields": map[string]any{"requesting_plan": "gd-plan1"},
	})
	requireStatus(t, patchRec, 200)

	getRec := doJSON(t, h, "GET", "/v1/beads/"+id, nil)
	requireStatus(t, getRec, 200)
	var got struct {
		Fields map[string]any `json:"fields"`
	}
	decodeJSON(t, getRec, &got)
	if got.Fields["requesting_plan"] != "gd-plan1" {
		t.Errorf("requesting_plan = %v, want gd-plan1", got.Fields["requesting_plan"])
	}
	if got.Fields["requesting_task"] != "gd-step1" {
		t.Errorf("requesting_task = %v, want gd-step1 (plan pin must not clobber it)", got.Fields["requesting_task"])
	}

	// The pin must also survive the escalation API projection — that envelope,
	// not the raw bead, is what /v1/escalations consumers (beads-viewer) read.
	escRec := doJSON(t, h, "GET", "/v1/escalations/"+id, nil)
	requireStatus(t, escRec, 200)
	var escResp struct {
		Escalation map[string]any `json:"escalation"`
	}
	decodeJSON(t, escRec, &escResp)
	if escResp.Escalation["requesting_plan"] != "gd-plan1" {
		t.Errorf("escalation envelope requesting_plan = %v, want gd-plan1", escResp.Escalation["requesting_plan"])
	}
}

// TestEscalationThreadIDDeclared reproduces ka-0000000226: an escalation raised
// inside a Slack thread carries the wish-pointer field thread_id, which the
// beads-viewer /threads timeline resolves membership by
// (UNION(fields.thread_id==head, parent-child closure) —
// cmd/beads-viewer/threads.go, ka-0000003279). The server transitively copies
// thread_id onto derivative beads via a direct store write that bypasses
// ValidateTypeFields (maybeCopyThreadID, auto_parent.go), so some live
// escalations carried it — but an explicit create that supplies thread_id in
// the request hit ValidateTypeFields and 400'd "unknown field" because
// type:escalation never declared the field, dropping the escalation out of its
// thread. Declaring it makes the explicit-create path 201 and gives the
// transitive-copy path a properly-declared home.
func TestEscalationThreadIDDeclared(t *testing.T) {
	_, _, h := newTestServer()

	createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
		"title": "grant deploy IAM role",
		"type":  "escalation",
		"kind":  "data",
		"fields": map[string]any{
			"operation_type":  "manual_intervention",
			"operation_spec":  map[string]any{"action": "grant role"},
			"requesting_task": "gd-step1",
			"thread_id":       "gd-thread1",
		},
	})
	requireStatus(t, createRec, 201)

	var created map[string]any
	decodeJSON(t, createRec, &created)
	id := created["id"].(string)

	getRec := doJSON(t, h, "GET", "/v1/beads/"+id, nil)
	requireStatus(t, getRec, 200)
	var got struct {
		Fields map[string]any `json:"fields"`
	}
	decodeJSON(t, getRec, &got)
	if got.Fields["thread_id"] != "gd-thread1" {
		t.Errorf("thread_id = %v, want gd-thread1 (escalation dropped out of its Slack thread)", got.Fields["thread_id"])
	}
	if got.Fields["requesting_task"] != "gd-step1" {
		t.Errorf("requesting_task = %v, want gd-step1 (thread_id must not clobber it)", got.Fields["requesting_task"])
	}
}
