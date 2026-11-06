package server

import (
	"context"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// Sensitivity alone does not stamp requires_four_eyes (operator send-back
// gd-GG17BiBxR0): agent-filed escalations carry operated_by = the owner, so a
// default stamp would lock the only human out of approve, reject and result
// (gd-TXbrvA9E6F). The requester of a secret escalation can still reject it,
// on the page (P10.B).
func TestEscalationPolicy_SensitivityDoesNotStampFourEyes(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	for _, sens := range []string{"sensitive", "secret"} {
		rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{
			"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}, "sensitivity": sens,
		}), nil)
		requireStatus(t, rec, http.StatusCreated)
		var b map[string]any
		decodeJSON(t, rec, &b)
		id := b["id"].(string)
		if f := escFields(t, ms, id); f["requires_four_eyes"] != nil {
			t.Fatalf("%s: requires_four_eyes = %v, want unset", sens, f["requires_four_eyes"])
		}
		if rec := doBearerJSON(t, h, "POST", "/v1/escalations/"+id+"/reject", "owner@example.com", map[string]any{"reason": "no"}, nil); rec.Code != http.StatusOK {
			t.Errorf("%s: reject by the single human requester: %d, want 200 (%s)", sens, rec.Code, rec.Body.String())
		}
	}
}

// Replay-unfit types may be filed only as a manual request (no executor).
func TestEscalationPolicy_ReplayUnfitCannotNameExecutor(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedRunnerAndAgent(ms, "gd-la3", "laptop")
	seedPrivilegedRole(t, ms, "infra")
	for name, extra := range map[string]map[string]any{
		"local":           {"execution_target": "local", "target_runner": "gd-la3"},
		"service_account": {"execution_target": "service_account", "spawn_role": "infra", "execution_script": "terraform apply"},
		"runner only":     {"target_runner": "gd-la3"},
		"owner only":      {"target_owner": "owner@example.com"},
	} {
		fields := map[string]any{"operation_type": "terraform_apply", "operation_spec": map[string]any{"dir": "x"}}
		for k, v := range extra {
			fields[k] = v
		}
		if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(fields), nil); rec.Code != http.StatusBadRequest {
			t.Errorf("terraform_apply with an executor (%s): %d, want 400", name, rec.Code)
		}
	}
	if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", escCreate(map[string]any{
		"operation_type": "terraform_apply", "operation_spec": map[string]any{"dir": "x"},
	}), nil); rec.Code != http.StatusCreated {
		t.Fatalf("positive control: terraform_apply as a manual request: %d", rec.Code)
	}
}

// Four-eyes applies to reject and result, not only approve.
func TestFourEyes_RejectAndResult(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient}) // the operator rejects on the page
	for _, verb := range []string{"reject", "result"} {
		id := "gd-4e-" + verb
		seedEscalation(t, ms, id, "owner@example.com", map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}, "requires_four_eyes": true})
		body := map[string]any{"reason": "no", "result_status": "success"}
		if rec := doBearerJSON(t, h, "POST", "/v1/escalations/"+id+"/"+verb, "owner@example.com", body, nil); rec.Code != http.StatusForbidden {
			t.Errorf("%s by the requester: %d, want 403", verb, rec.Code)
		}
		if b, _ := ms.GetBead(context.Background(), id); b.Status != model.StatusOpen {
			t.Errorf("%s by the requester closed the escalation: %s", verb, b.Status)
		}
		if rec := doBearerJSON(t, h, "POST", "/v1/escalations/"+id+"/"+verb, "second@example.com", body, nil); rec.Code != http.StatusOK {
			t.Errorf("positive control: %s by a second operator: %d, want 200 (%s)", verb, rec.Code, rec.Body.String())
		}
	}
}

// A PATCH may not give a replay-unfit escalation an executor (the D8 filing
// freeze); one that leaves the target alone still goes through.
func TestEscalationPolicy_ReplayUnfitPatchCannotNameExecutor(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedRunnerAndAgent(ms, "gd-la4", "laptop")
	seedEscalation(t, ms, "gd-ru-patch", "owner@example.com", map[string]any{"operation_type": "terraform_apply", "operation_spec": map[string]any{"dir": "x"}})
	for name, patch := range map[string]map[string]any{
		"local":       {"execution_target": "local", "target_runner": "gd-la4"},
		"runner only": {"target_runner": "gd-la4"},
	} {
		if rec := doBearerJSON(t, h, "PATCH", "/v1/beads/gd-ru-patch", "owner@example.com", map[string]any{"fields": patch}, nil); rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH %s onto terraform_apply: %d, want 400 (%s)", name, rec.Code, rec.Body.String())
		}
	}
	if f := escFields(t, ms, "gd-ru-patch"); f["execution_target"] != nil || f["target_runner"] != nil {
		t.Fatalf("refused PATCH still wrote a target: %v", f)
	}
	if rec := doBearerJSON(t, h, "PATCH", "/v1/beads/gd-ru-patch", "owner@example.com", map[string]any{"fields": map[string]any{"context": "run by hand"}}, nil); rec.Code != http.StatusOK {
		t.Fatalf("positive control: unrelated PATCH: %d (%s)", rec.Code, rec.Body.String())
	}
}

// An approve-time target override is refused for a replay-unfit type, and a
// plain approve of one that somehow carries a target dispatches nothing.
func TestEscalationPolicy_ReplayUnfitApproveDispatchesNone(t *testing.T) {
	s, ms, h := newClassedTestServer(t)
	s.SetForwardAuthClients([]string{testBrowserClient})
	seedRunnerAndAgent(ms, "gd-la5", "laptop")
	seedEscalation(t, ms, "gd-ru-ovr", "", map[string]any{"operation_type": "terraform_apply", "operation_spec": map[string]any{"dir": "x"}, "target_runner": "gd-la5"})
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-ru-ovr/approve", "owner@example.com", map[string]any{"execution_target": "local"}, nil), http.StatusBadRequest)
	if f := escFields(t, ms, "gd-ru-ovr"); f["execution_dispatch"] != nil || f["approved_by"] != nil {
		t.Fatalf("refused override still approved: %v", f)
	}
	seedEscalation(t, ms, "gd-ru-legacy2", "", map[string]any{"operation_type": "terraform_apply", "operation_spec": map[string]any{"dir": "x"}, "execution_target": "local", "target_runner": "gd-la5"})
	requireStatus(t, doBearerJSON(t, h, "POST", "/v1/escalations/gd-ru-legacy2/approve", "owner@example.com", map[string]any{}, nil), http.StatusOK)
	if f := escFields(t, ms, "gd-ru-legacy2"); f["execution_dispatch"] != dispatchNone {
		t.Fatalf("execution_dispatch = %v, want %q", f["execution_dispatch"], dispatchNone)
	}
}
