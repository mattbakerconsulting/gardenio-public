package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// setConfigAs PUTs /v1/configs/{key} with the given bearer token against a
// server that treats "primary-token" as the shared static token and
// "scoped-token" as a fleet token limited to the "task" bead type.
func setConfigAs(t *testing.T, key, token string) (int, *mockStore) {
	t.Helper()
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.userProvisioner = nil
	fc := NewFleetConfig("gardenio", nil)
	fc.RegisterTokenWithScopes("scoped-token", "gardenio", "", []string{"task"}, nil)
	srv.SetFleetAuth(fc)
	h := srv.NewHTTPHandler(nil, withTestToken(fc, "primary-token"))

	b, _ := json.Marshal(map[string]any{"value": json.RawMessage(`{"id":"x","command":"id","events":["*"]}`)})
	req := httptest.NewRequest(http.MethodPut, "/v1/configs/"+key, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, ms
}

func TestSetConfig_NonOIDCDeniedSensitiveKeys(t *testing.T) {
	denied := []string{
		"bus.handler.x",
		"bus.handler.nested.id",
		"Bus.Handler.x",
		"bridge_token:evil",
		"fleet_token:evil",
		"hook-execute-allowlist",
		"authz-denied-text",
		"security.token-revocation-slo",
		"type:user",
		"type:task",
		"type:agent",
	}
	for _, token := range []string{"primary-token", "scoped-token"} {
		for _, key := range denied {
			t.Run(token+"/"+key, func(t *testing.T) {
				code, ms := setConfigAs(t, key, token)
				if code != http.StatusForbidden {
					t.Fatalf("PUT %s as %s: code = %d, want 403", key, token, code)
				}
				if _, stored := ms.configs[key]; stored {
					t.Errorf("config %q was stored despite 403", key)
				}
			})
		}
	}
}

func TestSetConfig_StaticTokenAllowedKeys(t *testing.T) {
	// type:view is a plain custom type (what the beads-viewer e2e seed
	// registers; type:widget-instance is builtin, so the seed reuses it); the
	// others are ordinary non-authority config rows. Builtin and write_policy
	// type configs are refused by authorizeSetConfig, not this gate.
	for _, key := range []string{"type:view", "type:some-new-custom-type", "view:inbox", "claude-settings:e2e", "widget_type_clock"} {
		t.Run(key, func(t *testing.T) {
			code, ms := setConfigAs(t, key, "primary-token")
			if code != http.StatusOK {
				t.Fatalf("PUT %s as static token: code = %d, want 200", key, code)
			}
			if _, stored := ms.configs[key]; !stored {
				t.Errorf("config %q was not stored", key)
			}
		})
	}
}

// A fleet token whose type scope excludes configs is refused even for an
// otherwise-allowed key (checkTokenScopes).
func TestSetConfig_ScopedTokenDeniedAllowedKey(t *testing.T) {
	for _, key := range []string{"type:view", "view:inbox"} {
		code, _ := setConfigAs(t, key, "scoped-token")
		if code != http.StatusForbidden {
			t.Errorf("PUT %s as task-scoped token: code = %d, want 403", key, code)
		}
	}
}

func TestSetConfig_OIDCAdminUnaffected(t *testing.T) {
	for _, key := range []string{"bus.handler.x", "type:user", "authz-denied-text", "view:inbox"} {
		ms := newMockStore()
		srv := NewBeadsServer(ms, &events.NoopPublisher{})
		srv.userProvisioner = nil
		h := srv.NewHTTPHandler(&bearerSubjectVerifier{})
		rec := doJSONAs(t, h, http.MethodPut, "/v1/configs/"+key, map[string]any{"value": json.RawMessage(`{}`)}, "admin@example.com")
		if rec.Code != http.StatusOK {
			t.Errorf("OIDC admin PUT %s: code = %d, want 200; body %s", key, rec.Code, rec.Body.String())
		}
	}
}

// sensitiveRowMarker stands in for a bearer in the seeded rows; the read gate
// must never let it into a response below OIDC admin (gd-zdo4DIeGWR).
const sensitiveRowMarker = "SENSITIVE-ROW-MARKER"

var sensitiveReadKeys = []string{
	"bridge_token:land-other",
	"fleet_token:other",
	"bus.handler.other",
	"hook-execute-allowlist",
	"authz-denied-text",
	"security.token-revocation-slo",
}

func seedSensitiveConfigs(ms *mockStore) {
	for _, k := range append([]string{"view:inbox"}, sensitiveReadKeys...) {
		ms.configs[k] = &model.Config{Key: k, Value: json.RawMessage(`{"token":"` + sensitiveRowMarker + `"}`)}
	}
}

// Every caller the RoleViewer read routes admit, short of an OIDC admin, is
// refused a sensitive key or namespace, with the same answer for a missing
// key, and never sees a sensitive row.
func TestGetConfig_SensitiveKeysRefusedBelowOIDCAdmin(t *testing.T) {
	f := newScopedFixture(t)
	seedSensitiveConfigs(f.ms)
	callers := map[string]string{
		"static":         scopedAdminToken,
		"fleet":          tokFleetBridge,
		"land principal": tokLandClaimlayer,
		"OIDC operator":  tokOperator,
		"tenant member":  tokScopedWriter,
	}
	for name, tok := range callers {
		for _, key := range append([]string{"bridge_token:missing", "BRIDGE_TOKEN:land-other"}, sensitiveReadKeys...) {
			code, body := f.do(t, tok, http.MethodGet, "/v1/configs/"+key, nil)
			if code != http.StatusForbidden || strings.Contains(body, sensitiveRowMarker) {
				t.Errorf("%s GET %s: %d %.200s, want 403 without the row", name, key, code, body)
			}
		}
		for _, ns := range []string{"bridge_token", "fleet_token", "bus.handler", "bus.handler.", "Bridge_Token"} {
			code, body := f.do(t, tok, http.MethodGet, "/v1/configs?namespace="+url.QueryEscape(ns), nil)
			if code != http.StatusForbidden || strings.Contains(body, sensitiveRowMarker) {
				t.Errorf("%s list %s: %d %.200s, want 403 without rows", name, ns, code, body)
			}
		}
	}
	// An ordinary key stays readable below admin.
	if code, body := f.do(t, tokFleetBridge, http.MethodGet, "/v1/configs/view:inbox", nil); code != http.StatusOK {
		t.Errorf("fleet GET view:inbox: %d %.200s, want 200", code, body)
	}
}

func TestGetConfig_OIDCAdminReadsSensitiveKeys(t *testing.T) {
	f := newScopedFixture(t)
	seedSensitiveConfigs(f.ms)
	for _, key := range sensitiveReadKeys {
		if code, body := f.do(t, tokOwnerAdmin, http.MethodGet, "/v1/configs/"+key, nil); code != http.StatusOK || !strings.Contains(body, sensitiveRowMarker) {
			t.Errorf("OIDC admin GET %s: %d, want 200 with the row", key, code)
		}
	}
	if code, body := f.do(t, tokOwnerAdmin, http.MethodGet, "/v1/configs?namespace=bridge_token", nil); code != http.StatusOK || !strings.Contains(body, "bridge_token:land-other") {
		t.Errorf("OIDC admin list bridge_token: %d, want 200 with the row", code)
	}
}

// The store matches a namespace with LIKE, so "%" or "bridge%" reaches
// sensitive rows past the namespace gate; readableConfigs drops them.
func TestReadableConfigs_DropsSensitiveRowsBelowAdmin(t *testing.T) {
	var rows []*model.Config
	for _, k := range append([]string{"view:inbox", "type:task"}, sensitiveReadKeys...) {
		rows = append(rows, &model.Config{Key: k})
	}
	keys := func(cs []*model.Config) []string {
		var out []string
		for _, c := range cs {
			out = append(out, c.Key)
		}
		return out
	}
	static := withAuthMethod(context.Background(), authMethodStatic)
	if got := keys(readableConfigs(static, rows)); strings.Join(got, ",") != "view:inbox,type:task" {
		t.Errorf("static caller: got %v, want only view:inbox,type:task", got)
	}
	admin := WithRoles(withAuthMethod(context.Background(), authMethodOIDC), []string{RoleAdmin})
	if got := readableConfigs(admin, rows); len(got) != len(rows) {
		t.Errorf("OIDC admin: got %d rows, want %d", len(got), len(rows))
	}
}

func TestGRPCConfigReads_SensitiveKeysRefusedBelowOIDCAdmin(t *testing.T) {
	ms := newMockStore()
	seedSensitiveConfigs(ms)
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	for name, ctx := range map[string]context.Context{
		"static":      withAuthMethod(context.Background(), authMethodStatic),
		"OIDC viewer": WithRoles(withAuthMethod(context.Background(), authMethodOIDC), []string{RoleViewer, RoleOperator}),
		"land":        WithRoles(withAuthMethod(context.Background(), authMethodLand), landPrincipalRoles),
	} {
		for _, key := range sensitiveReadKeys {
			if _, err := srv.GetConfig(ctx, &beadsv1.GetConfigRequest{Key: key}); status.Code(err) != codes.PermissionDenied {
				t.Errorf("%s GetConfig %s: %v, want PermissionDenied", name, key, err)
			}
		}
		for _, ns := range []string{"bridge_token", "fleet_token", "bus.handler"} {
			if _, err := srv.ListConfigs(ctx, &beadsv1.ListConfigsRequest{Namespace: ns}); status.Code(err) != codes.PermissionDenied {
				t.Errorf("%s ListConfigs %s: %v, want PermissionDenied", name, ns, err)
			}
		}
	}
	admin := WithRoles(withAuthMethod(context.Background(), authMethodOIDC), []string{RoleAdmin})
	resp, err := srv.ListConfigs(admin, &beadsv1.ListConfigsRequest{Namespace: "bridge_token"})
	if err != nil || len(resp.GetConfigs()) != 1 {
		t.Errorf("OIDC admin ListConfigs bridge_token: %v, %d rows, want 1", err, len(resp.GetConfigs()))
	}
}
