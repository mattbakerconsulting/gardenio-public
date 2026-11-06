package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// TestHandleResolveDecision_DerivesRespondedByFromAuth covers ka-0000000687:
// the server must derive responded_by from the authenticated identity and
// ignore any client-supplied responded_by in the body. A client that forges
// responded_by to impersonate another user must not succeed (AUDIT §C3).
func TestHandleResolveDecision_DerivesRespondedByFromAuth(t *testing.T) {
	_, ms, h := newTestServerAuth()

	fields, _ := json.Marshal(map[string]any{
		"prompt":  "Deploy?",
		"options": json.RawMessage(`[{"id":"y","label":"Yes"}]`),
	})
	ms.beads["gd-d-auth"] = &model.Bead{
		ID: "gd-d-auth", Kind: model.KindData, Type: model.TypeDecision,
		Title: "Deploy?", Status: model.StatusOpen, Fields: fields,
	}

	rec := doJSONAs(t, h, "POST", "/v1/decisions/gd-d-auth/resolve", map[string]any{
		"selected_option": "y",
		"responded_by":    "spoofed-admin@example.com",
	}, "alice@example.com")
	requireStatus(t, rec, 200)

	var result struct {
		Decision map[string]any `json:"decision"`
		Issue    *model.Bead    `json:"issue"`
	}
	decodeJSON(t, rec, &result)

	if got := result.Decision["responded_by"]; got != "alice@example.com" {
		t.Fatalf("expected responded_by=%q derived from auth, got %v",
			"alice@example.com", got)
	}
	if got := result.Issue.ClosedBy; got != "alice@example.com" {
		t.Fatalf("expected ClosedBy=%q derived from auth, got %q",
			"alice@example.com", got)
	}

	// Event audit attribution also reflects the authenticated identity,
	// not the forged body value.
	if n := len(ms.events); n == 0 {
		t.Fatalf("expected at least one event, got 0")
	}
	last := ms.events[len(ms.events)-1]
	if last.Actor != "alice@example.com" {
		t.Fatalf("expected event actor=%q, got %q",
			"alice@example.com", last.Actor)
	}
}

// TestHandleCancelDecision_DerivesCanceledByFromAuth — companion to the
// resolve test. Cancel attribution also derives from context, not body.
func TestHandleCancelDecision_DerivesCanceledByFromAuth(t *testing.T) {
	_, ms, h := newTestServerAuth()

	fields, _ := json.Marshal(map[string]any{"prompt": "Deploy?"})
	ms.beads["gd-d-cancel-auth"] = &model.Bead{
		ID: "gd-d-cancel-auth", Kind: model.KindData, Type: model.TypeDecision,
		Title: "Deploy?", Status: model.StatusOpen, Fields: fields,
	}

	rec := doJSONAs(t, h, "POST", "/v1/decisions/gd-d-cancel-auth/cancel", map[string]any{
		"reason":      "obsolete",
		"canceled_by": "spoofed-admin@example.com",
	}, "bob@example.com")
	requireStatus(t, rec, 200)

	var result struct {
		Decision map[string]any `json:"decision"`
		Issue    *model.Bead    `json:"issue"`
	}
	decodeJSON(t, rec, &result)

	if got := result.Issue.ClosedBy; got != "bob@example.com" {
		t.Fatalf("expected ClosedBy=%q derived from auth, got %q",
			"bob@example.com", got)
	}
}

// TestHandleResolveDecision_NoAuthFallbackToBody covers ka-0000006099: when
// the daemon runs in no-auth mode (dev/e2e), AuthIdentity.Subject is empty
// and there is no spoofing surface to protect — the handler must fall back
// to the body's responded_by so beads-viewer can forward the oauth2-proxy
// X-Auth-Request-Email email through to the daemon for audit attribution.
// In production auth modes the auth-derived value always wins (see the
// _DerivesRespondedByFromAuth test above).
func TestHandleResolveDecision_NoAuthFallbackToBody(t *testing.T) {
	_, ms, h := newTestServerNoAuth()

	fields, _ := json.Marshal(map[string]any{
		"prompt":  "Deploy?",
		"options": json.RawMessage(`[{"id":"y","label":"Yes"}]`),
	})
	ms.beads["gd-d-noauth"] = &model.Bead{
		ID: "gd-d-noauth", Kind: model.KindData, Type: model.TypeDecision,
		Title: "Deploy?", Status: model.StatusOpen, Fields: fields,
	}

	rec := doJSON(t, h, "POST", "/v1/decisions/gd-d-noauth/resolve", map[string]any{
		"selected_option": "y",
		"responded_by":    "e2e-t9@example.test",
	})
	requireStatus(t, rec, 200)

	var result struct {
		Decision map[string]any `json:"decision"`
		Issue    *model.Bead    `json:"issue"`
	}
	decodeJSON(t, rec, &result)

	if got := result.Decision["responded_by"]; got != "e2e-t9@example.test" {
		t.Fatalf("expected responded_by=%q (body fallback in no-auth mode), got %v",
			"e2e-t9@example.test", got)
	}
}

// doStaticTokenWithOperator drives the FULL production HTTP path that
// beads-viewer uses: authenticate with the primary static service token and
// carry the human operator in the X-Operated-By header (stamped by the
// viewer's operatorMiddleware). The request goes through the real
// AuthMiddleware interceptor so AuthIdentity lands as {Type:"static",
// Subject:"static"} and X-Operated-By is sanitized into context — exactly the
// runtime shape that produced responded_by="static" before ka-0000001759.
func doStaticTokenWithOperator(t *testing.T, h http.Handler, method, path, operator string, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(method, path, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer primary-token")
	if operator != "" {
		req.Header.Set("X-Operated-By", operator)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestHandleResolveDecision_StaticTokenRelayRefused pins gd-Du6wHclx4g. Until
// then a static-token caller relaying a human in X-Operated-By resolved a
// decision and the record named that human (the ka-0000001759 viewer path).
// The shared static token is also what every agent pod holds, so the relay
// header cannot be trusted from it: the verb is refused and the decision stays
// open. The viewer now writes as the signed-in user (OIDC bearer / edge
// envelope), which keeps the attribution via the identity, not the header.
func TestHandleResolveDecision_StaticTokenRelayRefused(t *testing.T) {
	ms := newMockStore()
	srv := NewBeadsServer(ms, &events.NoopPublisher{})
	h := srv.NewHTTPHandler(nil, withTestToken(nil, "primary-token"))

	fields, _ := json.Marshal(map[string]any{
		"prompt":  "Apply prod litellm IRSA?",
		"options": json.RawMessage(`[{"id":"apply","label":"Apply"}]`),
	})
	for _, tc := range []struct {
		name, path string
		body       map[string]any
	}{
		{"resolve", "/v1/decisions/gd-static-relay/resolve", map[string]any{"selected_option": "apply"}},
		{"cancel", "/v1/decisions/gd-static-relay/cancel", map[string]any{"reason": "superseded"}},
	} {
		for _, operator := range []string{"oidc:matthew.baker@acme.example", ""} {
			t.Run(tc.name+"/operator="+operator, func(t *testing.T) {
				ms.beads["gd-static-relay"] = &model.Bead{
					ID: "gd-static-relay", Kind: model.KindData, Type: model.TypeDecision,
					Title: "Apply prod litellm IRSA?", Status: model.StatusOpen, Fields: fields,
				}
				rec := doStaticTokenWithOperator(t, h, "POST", tc.path, operator, tc.body)
				requireStatus(t, rec, http.StatusForbidden)
				if got := ms.beads["gd-static-relay"].Status; got != model.StatusOpen {
					t.Fatalf("refused %s closed the decision: %s", tc.name, got)
				}
				if got := decodeFields(t, ms.beads["gd-static-relay"].Fields)["responded_by"]; got != nil {
					t.Fatalf("refused %s stamped responded_by=%v", tc.name, got)
				}
			})
		}
	}
}
