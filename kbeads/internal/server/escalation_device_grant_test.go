package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

const testBrowserClient = "gardenio-traefik-forwardauth"

// azpVerifier is an OIDC verifier stub that reports a fixed authorized party,
// or an extraction error.
type azpVerifier struct {
	clientID string
	err      error
}

func (v *azpVerifier) Verify(context.Context, string) error { return nil }
func (v *azpVerifier) VerifyAndIdentify(context.Context, string) (string, error) {
	return "owner@example.com", nil
}
func (v *azpVerifier) ExtractRoles(context.Context, string) ([]string, error) {
	return []string{RoleAdmin}, nil
}
func (v *azpVerifier) ExtractClientID(context.Context, string) (string, error) {
	return v.clientID, v.err
}

// approveWith drives POST /v1/escalations/{id}/approve through the real HTTP
// auth chain with the given verifier and the approver allowlist. bearer is the
// Authorization token ("oidc-jwt" reaches the verifier; the primary token and
// a fleet token do not).
func approveWith(t *testing.T, verifier TokenVerifier, allow []string, bearer string) (*httptest.ResponseRecorder, *mockStore) {
	t.Helper()
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	srv.SetForwardAuthClients(allow)
	h := srv.NewHTTPHandler(verifier, withTestToken(nil, "primary-token"))
	fields, _ := json.Marshal(map[string]any{
		"operation_type": "test",
		"operation_spec": map[string]any{},
	})
	ms.beads["gd-esc-dg"] = &model.Bead{
		ID: "gd-esc-dg", Kind: model.KindData, Type: model.TypeEscalation,
		Title: "test", Status: model.StatusOpen, Fields: fields,
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/escalations/gd-esc-dg/approve", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, ms
}

func approveWithOIDC(t *testing.T, v *azpVerifier, allow ...string) (*httptest.ResponseRecorder, *mockStore) {
	t.Helper()
	return approveWith(t, v, allow, "oidc-jwt")
}

// requireNotApproved asserts the escalation stayed open and no approval
// fields were written.
func requireNotApproved(t *testing.T, ms *mockStore) {
	t.Helper()
	b := ms.beads["gd-esc-dg"]
	if b.Status != model.StatusOpen {
		t.Errorf("escalation must stay open after a refused approval, got %s", b.Status)
	}
	if strings.Contains(string(b.Fields), "approved_by") {
		t.Errorf("a refused approval must write no approval fields, got %s", b.Fields)
	}
}

// A `kd login` device-grant token is an OIDC human token with gardenio-admin,
// but escalation approval is refused for it (gd-bFFquoScHy): a device-code
// sign-in can be started by any agent pod and handed to the owner to confirm.
func TestApproveEscalation_DeviceGrantClientRefused(t *testing.T) {
	rec, ms := approveWithOIDC(t, &azpVerifier{clientID: DeviceGrantClientID}, testBrowserClient)
	requireStatus(t, rec, 403)
	if !strings.Contains(rec.Body.String(), "kd login") {
		t.Errorf("expected the refusal to name kd login, got %s", rec.Body.String())
	}
	requireNotApproved(t, ms)
}

// Fail closed: an azp that could not be extracted must not approve. Before the
// allowlist, verifiedClientID returned "" on error and the token approved.
func TestApproveEscalation_AzpExtractionErrorRefused(t *testing.T) {
	rec, ms := approveWithOIDC(t, &azpVerifier{clientID: DeviceGrantClientID, err: errors.New("parse azp claim")}, testBrowserClient)
	requireStatus(t, rec, 403)
	requireNotApproved(t, ms)
}

// A token with no azp at all is refused too.
func TestApproveEscalation_EmptyAzpRefused(t *testing.T) {
	rec, ms := approveWithOIDC(t, &azpVerifier{clientID: ""}, testBrowserClient)
	requireStatus(t, rec, 403)
	requireNotApproved(t, ms)
}

// A verifier that cannot extract a client at all (no ClientExtractor) yields
// no azp, so it cannot approve either.
type noClientVerifier struct{ azpVerifier }

func (noClientVerifier) ExtractClientID() {} // shadows the method set: not a ClientExtractor

func TestApproveEscalation_VerifierWithoutClientExtractorRefused(t *testing.T) {
	rec, ms := approveWith(t, &noClientVerifier{}, []string{testBrowserClient}, "oidc-jwt")
	requireStatus(t, rec, 403)
	requireNotApproved(t, ms)
}

// An azp that is neither the device client nor on the allowlist (a renamed or
// second device client) is refused: the gate is an allowlist, not a denylist.
func TestApproveEscalation_NonAllowlistedAzpRefused(t *testing.T) {
	for _, azp := range []string{"gardenio-cli-2", "some-other-client", "Gardenio-Traefik-Forwardauth"} {
		rec, ms := approveWithOIDC(t, &azpVerifier{clientID: azp}, testBrowserClient)
		if rec.Code != http.StatusForbidden {
			t.Errorf("azp %q: want 403, got %d %s", azp, rec.Code, rec.Body.String())
		}
		requireNotApproved(t, ms)
	}
}

// An empty (unconfigured) allowlist refuses every OIDC token.
func TestApproveEscalation_EmptyAllowlistRefusesAll(t *testing.T) {
	rec, ms := approveWithOIDC(t, &azpVerifier{clientID: testBrowserClient})
	requireStatus(t, rec, 403)
	requireNotApproved(t, ms)
}

// Positive control: the allowlisted browser SSO client still approves, and the
// escalation is actually approved.
func TestApproveEscalation_AllowlistedBrowserClientApproves(t *testing.T) {
	rec, ms := approveWithOIDC(t, &azpVerifier{clientID: testBrowserClient}, testBrowserClient)
	requireStatus(t, rec, 200)
	if !strings.Contains(string(ms.beads["gd-esc-dg"].Fields), "owner@example.com") {
		t.Errorf("approved_by should be the OIDC subject, fields=%s", ms.beads["gd-esc-dg"].Fields)
	}
}

// The static (primary) token has no OIDC identity and stays 403 even when its
// bearer is allowlisted-looking.
func TestApproveEscalation_StaticTokenRefused(t *testing.T) {
	rec, ms := approveWith(t, &azpVerifier{clientID: testBrowserClient}, []string{testBrowserClient}, "primary-token")
	requireStatus(t, rec, 403)
	requireNotApproved(t, ms)
}
