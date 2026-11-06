package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// gd-32dXqWb95Y: the type:user write_policy must not be the only thing
// standing between a signed-in non-admin and another person's slack_id /
// email_aliases, and the shared static token must not be able to weaken it.

const weakUserTypeConfig = `{"kind":"config","write_policy":{},"fields":[{"name":"slack_id","type":"string"},{"name":"email_aliases","type":"json"},{"name":"status_message","type":"string"}]}`

// (1) denyUserIdentityFields requires the admin role for OIDC callers.
func TestDenyUserIdentityFields_OIDCNonAdminDenied(t *testing.T) {
	s, _, _ := newTestServer()
	for _, role := range []string{RoleViewer, RoleOperator} {
		d := s.denyUserIdentityFields(oidcCtx("mallory@example.com", []string{role}), "update", []string{"slack_id"})
		if d == nil {
			t.Fatalf("role %s: want denial for identity field, got nil", role)
		}
		requireDenied(t, d)
	}
	if d := s.denyUserIdentityFields(oidcCtx("admin@example.com", []string{RoleAdmin}), "update", []string{"slack_id"}); d != nil {
		t.Errorf("admin denied: %v", d)
	}
	if d := s.denyUserIdentityFields(oidcCtx("mallory@example.com", []string{RoleViewer}), "update", nil); d != nil {
		t.Errorf("no identity field changed, want nil, got %v", d)
	}
}

// (1)+(3) end to end: even with a stored type:user policy that lacks min_role,
// a signed-in non-admin cannot set another person's slack_id.
func TestUserIdentity_WeakStoredPolicy_OIDCNonAdminDenied(t *testing.T) {
	for _, role := range []string{RoleViewer, RoleOperator} {
		_, ms, h := newTaintTestServer(map[string][]string{"mallory@example.com": {role}})
		ms.configs["type:user"] = &model.Config{Key: "type:user", Value: json.RawMessage(weakUserTypeConfig)}
		seedIdentityUserBead(t, ms, "gd-user1", userBeadFieldsJSON)
		before := string(ms.beads["gd-user1"].Fields)

		c := fieldWriteCase{name: "PATCH slack_id", method: http.MethodPatch, path: "/v1/beads/gd-user1",
			body: map[string]any{"fields": map[string]any{"slack_id": "UMALLORY"}}}
		rec := fieldWriteRequest(t, h, c, "mallory@example.com", "")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%s: code = %d, want 403; body %s", role, rec.Code, rec.Body.String())
		}
		if after := string(ms.beads["gd-user1"].Fields); after != before {
			t.Errorf("%s: fields changed despite 403:\n before %s\n after  %s", role, before, after)
		}
	}
}

// (3) the builtin min_role is a floor a stored type:user policy cannot lower.
func TestResolveTypeConfig_UserMinRoleFloor(t *testing.T) {
	for name, stored := range map[string]string{
		"empty write_policy": weakUserTypeConfig,
		"viewer min_role":    `{"kind":"config","write_policy":{"min_role":"gardenio-viewer"},"fields":[]}`,
		"unknown min_role":   `{"kind":"config","write_policy":{"min_role":"bogus"},"fields":[]}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, ms, _ := newTestServer()
			ms.configs["type:user"] = &model.Config{Key: "type:user", Value: json.RawMessage(stored)}
			tc, err := s.resolveTypeConfig(context.Background(), "user")
			if err != nil || tc == nil || tc.WritePolicy == nil {
				t.Fatalf("resolveTypeConfig: %v %v", tc, err)
			}
			if tc.WritePolicy.MinRole != RoleAdmin {
				t.Errorf("min_role = %q, want %q", tc.WritePolicy.MinRole, RoleAdmin)
			}
		})
	}
}

func TestResolveTypeConfig_UserMinRoleFloor_CanBeRaisedOnly(t *testing.T) {
	if !roleAtLeast(RoleAdmin, RoleAdmin) || roleAtLeast(RoleOperator, RoleAdmin) || roleAtLeast("", RoleAdmin) {
		t.Fatal("roleAtLeast ranking wrong")
	}
	// A non-floor builtin type keeps honoring a deliberate stored override.
	s, ms, _ := newTestServer()
	ms.configs["type:decision"] = &model.Config{Key: "type:decision",
		Value: json.RawMessage(`{"kind":"data","write_policy":{"min_role":"gardenio-viewer"},"fields":[]}`)}
	tc, err := s.resolveTypeConfig(context.Background(), "decision")
	if err != nil || tc == nil || tc.WritePolicy == nil || tc.WritePolicy.MinRole != "gardenio-viewer" {
		t.Fatalf("non-floor type override not honored: %+v %v", tc, err)
	}
}

// (2) the shared static token cannot rewrite type configs that carry policy.
func TestSetConfig_StaticTokenTypeConfigs(t *testing.T) {
	put := func(h http.Handler, key, value string) int {
		c := fieldWriteCase{method: http.MethodPut, path: "/v1/configs/" + key, body: map[string]any{"value": json.RawMessage(value)}}
		return fieldWriteRequest(t, h, c, "primary-token", "slack:U0TESTER1").Code
	}
	newH := func() (*mockStore, http.Handler) {
		ms := newMockStore()
		srv := NewBeadsServer(ms, &events.NoopPublisher{})
		srv.userProvisioner = nil
		return ms, srv.NewHTTPHandler(nil, withTestToken(nil, "primary-token"))
	}

	t.Run("builtin type:user denied", func(t *testing.T) {
		ms, h := newH()
		if code := put(h, "type:user", weakUserTypeConfig); code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", code)
		}
		if _, ok := ms.configs["type:user"]; ok {
			t.Error("type:user was stored despite 403")
		}
	})
	t.Run("other builtin type denied", func(t *testing.T) {
		_, h := newH()
		if code := put(h, "type:decision", `{"kind":"data","fields":[]}`); code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", code)
		}
	})
	t.Run("custom type carrying write_policy denied", func(t *testing.T) {
		_, h := newH()
		if code := put(h, "type:e2e-policy", `{"kind":"data","write_policy":{},"fields":[]}`); code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", code)
		}
	})
	t.Run("custom type already stored with write_policy denied", func(t *testing.T) {
		ms, h := newH()
		ms.configs["type:locked"] = &model.Config{Key: "type:locked", Value: json.RawMessage(`{"kind":"data","write_policy":{"min_role":"gardenio-admin"},"fields":[]}`)}
		if code := put(h, "type:locked", `{"kind":"data","fields":[]}`); code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", code)
		}
	})
	t.Run("plain custom type still registers", func(t *testing.T) {
		ms, h := newH()
		if code := put(h, "type:e2e-custom", `{"kind":"data","fields":[{"name":"x","type":"string"}]}`); code != http.StatusOK {
			t.Fatalf("code = %d, want 200", code)
		}
		if _, ok := ms.configs["type:e2e-custom"]; !ok {
			t.Error("custom type not stored")
		}
	})
	t.Run("non-type config still writable", func(t *testing.T) {
		_, h := newH()
		if code := put(h, "view:inbox", `{"filter":{}}`); code != http.StatusOK {
			t.Fatalf("code = %d, want 200", code)
		}
	})
}

func TestSetConfig_OIDCAdminMayWriteTypeUser(t *testing.T) {
	_, ms, h := newTaintTestServer(map[string][]string{"admin@example.com": {RoleAdmin}})
	c := fieldWriteCase{method: http.MethodPut, path: "/v1/configs/type:user", body: map[string]any{"value": json.RawMessage(weakUserTypeConfig)}}
	if rec := fieldWriteRequest(t, h, c, "admin@example.com", ""); rec.Code != http.StatusOK {
		t.Fatalf("admin code = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if _, ok := ms.configs["type:user"]; !ok {
		t.Error("admin write not stored")
	}
}

// gRPC SetConfig shares the gate.
func TestGRPCSetConfig_StaticTokenTypeUserDenied(t *testing.T) {
	s, ms, _ := newTestServer()
	_, err := s.SetConfig(machineUserCtx(), &beadsv1.SetConfigRequest{Key: "type:user", Value: []byte(weakUserTypeConfig)})
	requireDenied(t, err)
	if _, ok := ms.configs["type:user"]; ok {
		t.Error("type:user stored despite denial")
	}
	// Escalation replay (withMachineWrite) keeps its authority.
	if _, err := s.SetConfig(withMachineWrite(machineUserCtx()), &beadsv1.SetConfigRequest{Key: "type:user", Value: []byte(weakUserTypeConfig)}); err != nil {
		t.Fatalf("replay denied: %v", err)
	}
}
