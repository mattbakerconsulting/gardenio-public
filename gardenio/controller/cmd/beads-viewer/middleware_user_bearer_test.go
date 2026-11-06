package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestOperatorMiddleware_AttachesUserBearer verifies operatorMiddleware stamps
// the caller's OIDC access token onto the request context via WithUserBearer,
// so downstream daemon writes can authenticate as the user (ka-0000006379).
func TestOperatorMiddleware_AttachesUserBearer(t *testing.T) {
	var gotBearer, gotOperator string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotBearer = beadsapi.UserBearerFromContext(r.Context())
		gotOperator = beadsapi.OperatorFromContext(r.Context())
	})

	req := httptest.NewRequest(http.MethodPost, "/b/gd-1/owner", nil)
	req.Header.Set("X-Auth-Request-Email", "matthew.baker@acme.example")
	req.Header.Set("X-Auth-Request-Access-Token", "user-jwt-xyz")

	// operatorMiddleware now reads the VERIFIED identity from context, not the
	// raw header (ka-0000002979). In production it runs INSIDE the auth
	// middleware that populates ctxEmail; edgeAuth reproduces that seam by
	// promoting the trusted forwardauth-stamped header into the context.
	edgeAuth(operatorMiddleware(next)).ServeHTTP(httptest.NewRecorder(), req)

	if gotBearer != "user-jwt-xyz" {
		t.Errorf("user bearer not attached: got %q, want %q", gotBearer, "user-jwt-xyz")
	}
	if gotOperator != "oidc:matthew.baker@acme.example" {
		t.Errorf("operator not attached: got %q", gotOperator)
	}
}

// TestOperatorMiddleware_AttachesEdgeAuthOperator verifies operatorMiddleware
// stamps the verified human as the edge-auth envelope operator
// (beadsapi.WithEdgeAuthOperator) in canonical oidc: shape. This is what lets
// an AVA-routed write — which forwards NO bearer, so the write-bearer path
// can't authenticate it — sign an edge-auth envelope the daemon verifies to
// AuthIdentity{Type:"edge", Subject:oidc:<user>}, attributing the write to the
// human instead of the viewer's shared daemon token (decision ka-0000000960 →
// edge-auth-envelope). The stamp is inert until an HMAC key is wired
// (BEADS_VIEWER_EDGE_AUTH_HMAC_KEY → SetEdgeAuthHMACKey): beadsapi only emits
// the envelope headers when a key is set.
func TestOperatorMiddleware_AttachesEdgeAuthOperator(t *testing.T) {
	var gotEdgeOp string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotEdgeOp = beadsapi.EdgeAuthOperatorFromContext(r.Context())
	})

	req := httptest.NewRequest(http.MethodPost, "/b/gd-1/owner", nil)
	req.Header.Set("X-Auth-Request-Email", "Matthew.Baker@acme.example")

	edgeAuth(operatorMiddleware(next)).ServeHTTP(httptest.NewRecorder(), req)

	if gotEdgeOp != "oidc:matthew.baker@acme.example" {
		t.Errorf("edge-auth operator not attached in canonical shape: got %q, want %q",
			gotEdgeOp, "oidc:matthew.baker@acme.example")
	}
}

// TestOperatorMiddleware_NoEdgeAuthOperatorWhenUnauthenticated verifies that
// with no verified identity, operatorMiddleware stamps NO edge-auth operator —
// there is no anonymous write identity to assert.
func TestOperatorMiddleware_NoEdgeAuthOperatorWhenUnauthenticated(t *testing.T) {
	var gotEdgeOp string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotEdgeOp = beadsapi.EdgeAuthOperatorFromContext(r.Context())
	})

	req := httptest.NewRequest(http.MethodPost, "/b/gd-1/owner", nil)
	// No verified identity in context.

	operatorMiddleware(next).ServeHTTP(httptest.NewRecorder(), req)

	if gotEdgeOp != "" {
		t.Errorf("expected no edge-auth operator, got %q", gotEdgeOp)
	}
}

// TestOperatorMiddleware_NoBearerWhenAbsent verifies that when no OIDC bearer
// is present (e.g. an AVA-gated request that forwards no access token), the
// context carries no user bearer — so write-bearer enforcement, if enabled,
// fails such writes closed rather than silently using the static token.
func TestOperatorMiddleware_NoBearerWhenAbsent(t *testing.T) {
	var gotBearer string
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		gotBearer = beadsapi.UserBearerFromContext(r.Context())
	})

	req := httptest.NewRequest(http.MethodPost, "/b/gd-1/owner", nil)
	// No Authorization / X-Auth-Request-Access-Token header.

	operatorMiddleware(next).ServeHTTP(httptest.NewRecorder(), req)

	if gotBearer != "" {
		t.Errorf("expected no user bearer, got %q", gotBearer)
	}
}
