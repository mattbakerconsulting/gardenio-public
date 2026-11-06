package server

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"gardenio/kbeads/internal/model"
)

// machineUserCtx returns a context for a non-OIDC caller (the shared static
// token every agent pod holds, or a fleet token) with a human operator header,
// so createBead/updateBead get past the audit-operator requirement and reach
// the authorization checks under test.
func machineUserCtx() context.Context {
	return WithSystemOperator(withAuthMethod(
		WithAuthIdentity(context.Background(), AuthIdentity{Type: "static", Subject: "static"}),
		authMethodStatic), "slack:U0TESTER1")
}

func fleetUserCtx() context.Context {
	return WithSystemOperator(withAuthMethod(
		WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "slack-bridge"}),
		authMethodStatic), "slack:U0TESTER1")
}

func seedIdentityUserBead(t *testing.T, ms *mockStore, id, fields string) {
	t.Helper()
	ms.beads[id] = &model.Bead{
		ID: id, Title: "owner", Type: "user", Kind: model.KindConfig,
		Status: model.StatusOpen, Fields: json.RawMessage(fields),
	}
}

func requireDenied(t *testing.T, err error) *AuthzDenial {
	t.Helper()
	var d *AuthzDenial
	if !errors.As(err, &d) {
		t.Fatalf("want *AuthzDenial, got %T: %v", err, err)
	}
	if d.RequiredRole != RoleAdmin {
		t.Errorf("RequiredRole=%q, want %q", d.RequiredRole, RoleAdmin)
	}
	return d
}

// A static or fleet token must not write identity fields on a user bead
// (gd-uAvleAZUvV): update path.
func TestUpdateBead_UserIdentityFields_MachineDenied(t *testing.T) {
	for name, ctx := range map[string]context.Context{"static": machineUserCtx(), "fleet": fleetUserCtx()} {
		for _, key := range []string{"email", "email_aliases", "slack_id", "slack_user_id", "gitlab_username",
			"github_username", "gitlab_user_id", "github_user_id", "jira_user_id", "jira_user", "allowed_roles", "allowed_projects",
			"capabilities", "auth_methods"} {
			t.Run(name+"/"+key, func(t *testing.T) {
				s, ms, _ := newTestServer()
				seedIdentityUserBead(t, ms, "gd-user1", `{"email":"owner@example.com","status_message":"hi"}`)
				_, err := s.updateBead(ctx, "gd-user1", updateBeadInput{
					Fields: json.RawMessage(`{"` + key + `":"attacker@example.com"}`),
				})
				requireDenied(t, err)
				if got := string(ms.beads["gd-user1"].Fields); got != `{"email":"owner@example.com","status_message":"hi"}` {
					t.Errorf("fields mutated despite denial: %s", got)
				}
			})
		}
	}
}

// The viewer's self-service writes (status_message, preferences, ...) are
// non-identity fields and keep working on the machine allow-list.
func TestUpdateBead_UserNonIdentityFields_MachineAllowed(t *testing.T) {
	for _, patch := range []string{
		`{"status_message":"heads down"}`,
		`{"preferences":{"homepage":{"hide":["inbox"]}}}`,
		`{"profile_prefs":{"theme":"dark"}}`,
		`{"display_name":"Matt","pronouns":"they/them","team":"core","slack_channel_id":"C1"}`,
	} {
		t.Run(patch, func(t *testing.T) {
			s, ms, _ := newTestServer()
			seedIdentityUserBead(t, ms, "gd-user1", `{"email":"owner@example.com"}`)
			if _, err := s.updateBead(machineUserCtx(), "gd-user1", updateBeadInput{Fields: json.RawMessage(patch)}); err != nil {
				t.Fatalf("non-identity write denied: %v", err)
			}
		})
	}
}

// A read-modify-write client echoing the unchanged identity values back
// alongside a status_message edit is not "touching" them.
func TestUpdateBead_UserEchoedIdentityFields_MachineAllowed(t *testing.T) {
	s, ms, _ := newTestServer()
	seedIdentityUserBead(t, ms, "gd-user1", `{"email":"owner@example.com","slack_id":"U1"}`)
	patch := json.RawMessage(`{"email":"owner@example.com","slack_id":"U1","status_message":"x"}`)
	if _, err := s.updateBead(machineUserCtx(), "gd-user1", updateBeadInput{Fields: patch}); err != nil {
		t.Fatalf("echoed unchanged identity fields denied: %v", err)
	}
}

// Create path: seeding identity fields onto a new user bead is denied; a
// user bead with no identity fields is still creatable.
func TestCreateBead_UserIdentityFields_MachineDenied(t *testing.T) {
	s, _, _ := newTestServer()
	_, err := s.createBead(machineUserCtx(), createBeadInput{
		Title: "mallory", Type: "user", Fields: json.RawMessage(`{"email":"owner@example.com","slack_id":"U0C37URBQVD"}`),
	})
	requireDenied(t, err)

	if _, err := s.createBead(machineUserCtx(), createBeadInput{
		Title: "mallory", Type: "user", Fields: json.RawMessage(`{"display_name":"Mallory","email":""}`),
	}); err != nil {
		t.Fatalf("user bead without identity values denied: %v", err)
	}
}

// An OIDC gardenio-admin may write identity fields; an operator-approved
// escalation replay (withMachineWrite) is not gated either.
func TestUpdateBead_UserIdentityFields_AdminAndReplayAllowed(t *testing.T) {
	s, ms, _ := newTestServer()
	seedIdentityUserBead(t, ms, "gd-user1", `{"email":"owner@example.com"}`)
	admin := WithRoles(oidcCtx("admin@example.com", nil), []string{RoleAdmin})
	if _, err := s.updateBead(admin, "gd-user1", updateBeadInput{Fields: json.RawMessage(`{"slack_id":"U1"}`)}); err != nil {
		t.Fatalf("admin identity write denied: %v", err)
	}
	if d := s.authorizeUserIdentityFields(withMachineWrite(machineUserCtx()), "update", "user",
		json.RawMessage(`{"slack_id":"U2"}`), ms.beads["gd-user1"].Fields); d != nil {
		t.Fatalf("escalation replay denied: %v", d)
	}
}

// The gate is scoped to type:user.
func TestAuthorizeUserIdentityFields_OtherTypesUnaffected(t *testing.T) {
	s, _, _ := newTestServer()
	if d := s.authorizeUserIdentityFields(machineUserCtx(), "update", "agent", json.RawMessage(`{"email":"a@b"}`), nil); d != nil {
		t.Fatalf("non-user type gated: %v", d)
	}
}
