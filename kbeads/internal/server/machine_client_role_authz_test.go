package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
)

// machine_client names the machine client whose secret the controller mounts
// into a role's pods (escalation routing Task 15, C5; gd-y4pcHPnmpe). Only an
// admin may set it: a machine token creating a role with it would choose which
// pod holds the coordinator's credential. Each denial has a positive control
// on the same fixture.

const machineClientRole = `{"model":"opus","machine_client":"gardenio-cluster-coordinator","privileged":true}`

func machineClientDeniedCallers() map[string]context.Context {
	return map[string]context.Context{
		"static": machineUserCtx(),
		"fleet":  fleetUserCtx(),
		"agent":  WithSystemOperator(withTestAgent(withAuthMethod(context.Background(), authMethodStatic), "gd-agent-1"), "slack:U0TESTER1"),
	}
}

func TestCreateRoleBead_MachineClient_MachineDenied(t *testing.T) {
	for name, ctx := range machineClientDeniedCallers() {
		t.Run(name, func(t *testing.T) {
			s, ms, _ := newTestServer()
			seedAuthzAgentBead(t, ms, "gd-agent-1", agentBeadFieldsJSON)
			_, err := s.createBead(ctx, createBeadInput{Title: "x", Type: "role", Fields: json.RawMessage(machineClientRole)})
			requireDenied(t, err)
			if _, err := s.createBead(ctx, createBeadInput{Title: "x", Type: "role", Fields: json.RawMessage(`{"model":"opus"}`)}); err != nil {
				t.Fatalf("create without machine_client denied: %v", err)
			}
		})
	}
	s, _, _ := newTestServer()
	if _, err := s.createBead(adminOIDCCtx(), createBeadInput{Title: "y", Type: "role", Fields: json.RawMessage(machineClientRole)}); err != nil {
		t.Fatalf("admin create denied: %v", err)
	}
}

// The field gate alone, on create: privileged (required beside machine_client)
// is gated too, so the end-to-end create above cannot isolate machine_client.
func TestAuthorizeRoleCreate_MachineClientGated(t *testing.T) {
	s, _, _ := newTestServer()
	for name, ctx := range machineClientDeniedCallers() {
		if d := s.authorizeAgentIdentityFields(ctx, "create", "role", json.RawMessage(`{"machine_client":"gardenio-cluster-coordinator"}`), nil); d == nil {
			t.Errorf("%s: role create naming a machine client passed the field gate", name)
		}
		if d := s.authorizeAgentIdentityFields(ctx, "create", "role", json.RawMessage(`{"machine_client":""}`), nil); d != nil {
			t.Errorf("%s: role create with an empty machine_client denied: %v", name, d)
		}
	}
	if d := s.authorizeAgentIdentityFields(adminOIDCCtx(), "create", "role", json.RawMessage(`{"machine_client":"gardenio-cluster-coordinator"}`), nil); d != nil {
		t.Errorf("admin denied: %v", d)
	}
}

func TestMergeBeadFields_MachineClientDeniedForMachine(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	seedConfigBead(t, ms, "gd-role1", "role", `{"model":"opus","privileged":true}`)
	requireDenied(t, srv.mergeBeadFields(machineUserCtx(), "gd-role1", map[string]any{"machine_client": "gardenio-cluster-coordinator"}))
	if err := srv.mergeBeadFields(machineUserCtx(), "gd-role1", map[string]any{"model": "sonnet"}); err != nil {
		t.Errorf("unrelated merge: %v", err)
	}
	if err := srv.mergeBeadFields(adminOIDCCtx(), "gd-role1", map[string]any{"machine_client": "gardenio-cluster-coordinator"}); err != nil {
		t.Errorf("admin merge: %v", err)
	}
}

// A role naming a machine client must be privileged, so the spawn gate
// escalates a non-human spawn of it.
func TestRolePrivilegeError_MachineClientNeedsPrivileged(t *testing.T) {
	msg := rolePrivilegeError(map[string]string{"machine_client": "gardenio-cluster-coordinator"})
	if !strings.Contains(msg, "machine_client") {
		t.Errorf("rolePrivilegeError without privileged = %q, want a refusal naming machine_client", msg)
	}
	if msg := rolePrivilegeError(map[string]string{"machine_client": "gardenio-cluster-coordinator", "privileged": "true"}); msg != "" {
		t.Errorf("rolePrivilegeError with privileged = %q, want none", msg)
	}
}
