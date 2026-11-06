package server

import (
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/events"
)

// verifier=true gives a role's pods the chart's read-only verifier SA and its
// token (P9.2, gd-6ujfeTZfNL): only an admin may set it, but it is not a
// privileged field, so a crew role carrying it does not escalate its spawns.
func TestVerifierRoleField_AdminOnlyNotPrivileged(t *testing.T) {
	s, _, _ := newTestServer()
	for name, ctx := range machineClientDeniedCallers() {
		if d := s.authorizeAgentIdentityFields(ctx, "create", "role", json.RawMessage(`{"verifier":true}`), nil); d == nil {
			t.Errorf("%s: role create with verifier=true passed the field gate", name)
		}
		if d := s.authorizeAgentIdentityFields(ctx, "create", "role", json.RawMessage(`{"verifier":false}`), nil); d != nil {
			t.Errorf("%s: role create with verifier=false denied: %v", name, d)
		}
	}
	if d := s.authorizeAgentIdentityFields(adminOIDCCtx(), "create", "role", json.RawMessage(`{"verifier":true}`), nil); d != nil {
		t.Errorf("admin denied: %v", d)
	}
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	seedConfigBead(t, ms, "gd-role1", "role", `{"model":"opus"}`)
	requireDenied(t, srv.mergeBeadFields(machineUserCtx(), "gd-role1", map[string]any{"verifier": "true"}))
	if err := srv.mergeBeadFields(adminOIDCCtx(), "gd-role1", map[string]any{"verifier": "true"}); err != nil {
		t.Errorf("admin merge: %v", err)
	}
	if msg := rolePrivilegeError(map[string]string{"verifier": "true"}); msg != "" {
		t.Errorf("rolePrivilegeError(verifier=true) = %q, want none", msg)
	}
}
