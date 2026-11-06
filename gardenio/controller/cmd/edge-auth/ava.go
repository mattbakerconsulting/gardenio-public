package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"gardenio/controller/internal/oidcauth"
)

// AVA-mode path of the gardenio-edge-auth shim.
//
// When the shim is configured with avaTrustSigner non-empty, requests carrying
// the X-Amzn-Ava-User-Context header are routed through this path instead of
// the GitLab PAT path. Both paths converge on the same HMAC envelope
// (X-Operator / X-Auth-Audit-Id / X-Edge-Auth-Sig / X-Edge-Auth-Ts) that
// kbeads daemon's internal/edgeauth/verify.go already trusts (ka-0000001678),
// so the daemon-side validator is unchanged and indifferent to which edge the
// request crossed.
//
// Design constraints (epic ka-0000005862):
//   - DO NOT re-implement AVA JWT verification. The verifier lives in
//     internal/oidcauth/ava.go and is exercised by the beads-viewer; we reach
//     it through the oidcauth.Handle middleware so any future signature /
//     claim hardening flows through both surfaces.
//   - The "not_in_group" notion does not apply: AVA's Cedar policy already
//     gated the upstream. A verified AVA token is trusted as an authenticated
//     user identity.
//   - On AVA verifier failure return 401 and DO NOT fall back to PAT — that
//     would silently downgrade a tampered AVA request to a different auth
//     flow (mirrors the same precedence the beads-viewer middleware uses).

// avaVerifier extracts an authenticated identity from an HTTP request's
// AVA user-context header. The shim consumes this through the small interface
// rather than the concrete oidcauth.Handle so unit tests can inject a stub
// without standing up a JWKS fixture; production wiring resolves to
// avaHandleVerifier below.
type avaVerifier interface {
	// verify inspects the AVA header on r and returns an identity on success.
	// The error is mapped to an HTTP status by the caller:
	//   errAVAMissing  -> caller routes to the PAT path (no AVA header present)
	//   errAVAInvalid  -> 401 (header present but signature / claims rejected)
	verify(ctx context.Context, r *http.Request) (avaIdentity, error)
}

// avaIdentity is the subset of an authenticated AVA token the shim needs to
// stamp the HMAC envelope. The daemon validator hashes Email into the
// signature. Groups come from the verified AVA JWT and are mapped to canonical
// Gardenio roles before a role-bearing v2 envelope is signed.
type avaIdentity struct {
	Email  string
	Groups []string
}

// Sentinel errors so the caller (handleProxy) can branch on disposition
// without string-matching.
var (
	errAVAMissing = errors.New("ava: header not present")
	errAVAInvalid = errors.New("ava: token invalid")
)

// avaHandleVerifier is the production verifier. It delegates JWT signature
// verification to oidcauth.Handle.Middleware so we share one implementation
// with the beads-viewer (ka-0000005862). The Handle is built once at server
// construction with cfg.IssuerURL="" (Keycloak disabled — the shim is not a
// Keycloak gate) and cfg.AVA populated from the chart values.
type avaHandleVerifier struct {
	h *oidcauth.Handle
}

// newAvaHandleVerifier returns a verifier when AVA mode is enabled in cfg
// (cfg.avaTrustSigner non-empty). Returns nil when AVA mode is OFF so the
// shim short-circuits to the PAT-only path with no overhead.
func newAvaHandleVerifier(cfg *config, logger *slog.Logger) avaVerifier {
	if cfg.avaTrustSigner == "" {
		return nil
	}
	// IssuerURL is empty here, so oidcauth.New does no Keycloak discovery and
	// starts no background goroutine — context.Background() is fine (nothing
	// observes its cancellation on this path).
	h := oidcauth.New(context.Background(), oidcauth.Config{
		// IssuerURL deliberately empty: the shim is not a Keycloak gate. With
		// IssuerURL empty oidcauth.New skips OIDC provider discovery entirely
		// and the AVA branch is the only auth path the Handle exposes.
		IssuerURL: "",
		AVA: oidcauth.AVAConfig{
			TrustSigner: cfg.avaTrustSigner,
			Region:      cfg.avaRegion,
		},
	}, logger)
	return &avaHandleVerifier{h: h}
}

// statusRecorder lets us observe whether the oidcauth middleware rejected the
// request (it writes 401 and bytes directly to the ResponseWriter) without
// surfacing those bytes to the real client. handleProxy uses this to detect
// AVA verification failure and emit its own JSON error shape consistent with
// the PAT path.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	// suppress drops the middleware's body bytes on a rejection so we can
	// substitute the JSON error shape the rest of handleProxy emits. The
	// middleware's plain-text "invalid AVA token\n" body would leak through
	// otherwise.
	suppress bool
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.wroteHeader = true
	if r.suppress {
		// Do not propagate the header — the caller will write its own.
		return
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	if r.suppress {
		// Pretend the write happened so the upstream handler doesn't error.
		return len(b), nil
	}
	return r.ResponseWriter.Write(b)
}

func (v *avaHandleVerifier) verify(_ context.Context, r *http.Request) (avaIdentity, error) {
	if r.Header.Get(oidcauth.AVAHeaderName) == "" {
		return avaIdentity{}, errAVAMissing
	}

	var (
		id  avaIdentity
		hit bool
	)
	// Inner handler runs only when the middleware's AVA verification passed.
	// It pulls the email + groups the middleware injected into the request
	// context and signals success via the closure-captured `hit` boolean.
	inner := http.HandlerFunc(func(_ http.ResponseWriter, ir *http.Request) {
		hit = true
		id.Email = oidcauth.EmailFromContext(ir.Context())
		id.Groups = oidcauth.RolesFromContext(ir.Context())
	})

	// We do not want the middleware's 401-and-bytes response to reach the
	// client — handleProxy will emit its own JSON error matching the PAT
	// path shape. The recorder swallows the middleware's body on rejection
	// while still surfacing the status code so we can branch.
	rec := &statusRecorder{ResponseWriter: discardResponseWriter{}, suppress: true}
	v.h.Middleware(inner).ServeHTTP(rec, r)

	if !hit {
		return avaIdentity{}, errAVAInvalid
	}
	if id.Email == "" {
		// Defence in depth: the middleware verified the signature but the
		// trust-provider claims did not carry an email. Treat as invalid;
		// stamping an empty X-Operator would let the daemon attribute the
		// request to an unauthenticated principal.
		return avaIdentity{}, errAVAInvalid
	}
	return id, nil
}

// discardResponseWriter is a /dev/null http.ResponseWriter the statusRecorder
// wraps when we know any bytes the middleware writes must be dropped. It
// implements just enough of the interface to satisfy ServeHTTP.
type discardResponseWriter struct{}

func (discardResponseWriter) Header() http.Header         { return http.Header{} }
func (discardResponseWriter) Write(b []byte) (int, error) { return len(b), nil }
func (discardResponseWriter) WriteHeader(int)             {}
