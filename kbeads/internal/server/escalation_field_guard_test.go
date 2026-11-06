package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"gardenio/kbeads/internal/model"
)

// PATCH cannot write a server-only field or change a frozen one, not even as
// a gardenio-admin (bearerSubjectVerifier grants RoleAdmin). Positive control:
// an ordinary field (notified_at, stamped by the bridge) still patches
// (gd-tZYAufHCdo).
func TestEscalationFields_PatchGuard(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedEscalation(t, ms, "gd-f1", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}, "execution_target": "server"})
	for _, f := range []map[string]any{
		{"approved_by": "x"}, {"executor": "x"}, {"result_status": "success"}, {"result": map[string]any{"ok": true}},
		{"replay_in_progress": true}, {"rejected_by": "x"}, {"execution_dispatch": "local"},
		{"operation_spec": map[string]any{"m": 2}}, {"execution_target": "local"}, {"Execution_Target": "local"},
		{"target_runner": "gd-r1"}, {"execution_script": "rm -rf /"}, {"approval_comment": "looks fine"},
	} {
		rec := doBearerJSON(t, h, "PATCH", "/v1/beads/gd-f1", "owner@example.com", map[string]any{"fields": f}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("PATCH %v: status %d, want 400; %s", f, rec.Code, rec.Body.String())
		}
	}
	rec := doBearerJSON(t, h, "PATCH", "/v1/beads/gd-f1", "owner@example.com", map[string]any{"fields": map[string]any{"notified_at": "2026-10-11T00:00:00Z"}}, nil)
	requireStatus(t, rec, http.StatusOK)
}

// The field-edit endpoints go straight to the store and skip the PATCH
// guards (spec §1.5), so they refuse every server-only and frozen escalation
// field themselves (gd-tZYAufHCdo). Positive control: the same edit on a task.
func TestEscalationFields_SubFieldEndpointsGuarded(t *testing.T) {
	_, ms, h := newClassedTestServer(t)
	seedEscalation(t, ms, "gd-f2", "", map[string]any{"operation_type": "k8s_apply", "operation_spec": map[string]any{"m": 1}, "result": map[string]any{"m": 1}})
	for _, field := range []string{"operation_spec", "result"} {
		rec := doBearerJSON(t, h, "POST", "/v1/beads/gd-f2/fields/"+field+"/edit", "owner@example.com",
			map[string]any{"op": "set", "key": "m", "value": "2"}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("edit %s: %d, want 400; %s", field, rec.Code, rec.Body.String())
		}
	}
	rec := doBearerJSON(t, h, "POST", "/v1/beads/gd-f2/increment", "owner@example.com",
		map[string]any{"replay_in_progress": 1}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("increment replay_in_progress: %d, want 400; %s", rec.Code, rec.Body.String())
	}
	raw, _ := json.Marshal(map[string]any{"meta": map[string]any{"m": "1"}})
	if err := ms.CreateBead(context.Background(), &model.Bead{
		ID: "gd-t1", Title: "t", Type: "task", Kind: model.KindIssue,
		Status: model.StatusOpen, Labels: []string{"project:gardenio"}, Fields: raw,
	}); err != nil {
		t.Fatal(err)
	}
	ms.labels["gd-t1"] = []string{"project:gardenio"}
	rec = doBearerJSON(t, h, "POST", "/v1/beads/gd-t1/fields/meta/edit", "owner@example.com",
		map[string]any{"op": "set", "key": "m", "value": "2"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("positive control: edit on a task: %d, want 200; %s", rec.Code, rec.Body.String())
	}
}

// Create refuses client-supplied server-only fields, admins included
// (requesting_agent stays allowed: the create guard stamps or accepts it).
// Frozen fields are set here.
func TestEscalationFields_CreateRefusesServerOnly(t *testing.T) {
	_, _, h := newClassedTestServer(t)
	base := func(extra map[string]any) map[string]any {
		f := map[string]any{"operation_type": "manual_operator_action", "operation_spec": map[string]any{"x": 1}}
		for k, v := range extra {
			f[k] = v
		}
		return map[string]any{"title": "e", "type": "escalation", "kind": "data", "labels": []string{"project:gardenio"}, "fields": f}
	}
	for _, k := range []string{"approved_by", "executor", "execution_dispatch", "result_status", "rejected_by"} {
		if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", base(map[string]any{k: "me"}), nil); rec.Code != http.StatusBadRequest {
			t.Errorf("create with %s: %d, want 400; %s", k, rec.Code, rec.Body.String())
		}
	}
	if rec := doBearerJSON(t, h, "POST", "/v1/beads", "owner@example.com", base(map[string]any{"requesting_agent": "crew-x", "sensitivity": "internal"}), nil); rec.Code != http.StatusCreated {
		t.Fatalf("positive control: create with requesting_agent + frozen field: %d, want 201; %s", rec.Code, rec.Body.String())
	}
}

// The daemon's own filing (handleEscalation) and machine writes may carry
// server-only fields at create.
func TestEscalationFields_CreateDaemonExempt(t *testing.T) {
	b := &model.Bead{Type: model.TypeEscalation, Fields: json.RawMessage(`{"policy_rule":"r","executor":"x"}`)}
	if err := checkEscalationCreateFields(context.Background(), b); err == nil {
		t.Fatal("client create with executor: want refusal")
	}
	for name, ctx := range map[string]context.Context{
		"server-stamped": withServerStampedEscalation(context.Background()),
		"machine":        withMachineWrite(context.Background()),
	} {
		if err := checkEscalationCreateFields(ctx, b); err != nil {
			t.Errorf("%s create: %v, want nil", name, err)
		}
	}
}
