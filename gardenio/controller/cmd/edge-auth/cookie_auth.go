package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Cookie-mode path of the gardenio-edge-auth shim (ka-0000000080).
//
// When the shim is configured with oauth2ProxyAuthURL non-empty, a proxy
// request that carries the gb-login browser-session cookie but no Bearer PAT
// and no AVA header is authenticated here. The shim makes a subrequest to
// oauth2-proxy's /oauth2/auth endpoint, forwarding ONLY the inbound Cookie
// header; oauth2-proxy validates its own session cookie and, on success,
// returns 202 with the authenticated identity in X-Auth-Request-* RESPONSE
// headers. The operator email comes from that response — derived by
// oauth2-proxy from the validated session, never from a client-supplied
// header — and is stamped into the same HMAC envelope (X-Operator /
// X-Auth-Audit-Id / X-Edge-Auth-Sig / X-Edge-Auth-Ts) the PAT and AVA paths
// produce. The daemon (kbeads internal/edgeauth/verify.go, ka-0000001678) is
// therefore unchanged and trusts a single envelope shape regardless of edge.
//
// Why validate at the shim rather than teach the daemon to trust a
// forwardauth-injected X-Auth-Request-Email header directly (the originally
// scoped ka-0000007238): a plain header on the daemon's in-cluster port is
// spoofable by any pod that can reach :8080, which would break the single
// cryptographic trust boundary the 2026-04-22 daemon security audit relies
// on. Keeping cookie validation here preserves that boundary — the email is
// only ever trusted after oauth2-proxy attests it and the shim re-signs it.
//
// Design constraints (mirrors the AVA path in ava.go):
//   - oauth2-proxy is the authority for the session; we DO NOT parse or
//     decrypt the cookie ourselves.
//   - group authorization is already enforced by oauth2-proxy's own
//     --allowed-group config at session-creation time, exactly as AVA's
//     Cedar policy gates the AVA edge — a valid session implies membership.
//   - on any verification failure we return an error and DO NOT fall back to
//     another auth flow; a present-but-rejected cookie is a hard deny.

// cookieVerifier authenticates a request's gb-login browser-session cookie
// and returns the attested identity. The shim consumes this through the
// small interface (rather than the concrete oauth2ProxyVerifier) so unit
// tests can inject a stub without standing up an oauth2-proxy.
type cookieVerifier interface {
	// verify inspects the session cookie on r and returns an identity on
	// success. The error is mapped to an HTTP status by the caller:
	//   errCookieMissing  -> caller routes onward (no session cookie present)
	//   errCookieInvalid  -> 401 (cookie present but oauth2-proxy rejected it)
	//   errCookieUpstream -> 502 (oauth2-proxy unreachable / unexpected status)
	verify(ctx context.Context, r *http.Request) (cookieIdentity, error)
}

// cookieIdentity is the subset of an authenticated session the shim needs to
// stamp the HMAC envelope. Groups come from oauth2-proxy's authenticated
// response headers and are mapped to canonical Gardenio roles before a
// role-bearing v2 envelope is signed.
type cookieIdentity struct {
	Email  string
	Groups []string
}

// Sentinel errors so the caller (handleProxy) can branch on disposition
// without string-matching.
var (
	errCookieMissing  = errors.New("cookie: session cookie not present")
	errCookieInvalid  = errors.New("cookie: session rejected by oauth2-proxy")
	errCookieUpstream = errors.New("cookie: oauth2-proxy auth endpoint unavailable")
)

// oauth2ProxyVerifier is the production cookieVerifier. It delegates session
// validation to oauth2-proxy's /oauth2/auth subrequest endpoint.
type oauth2ProxyVerifier struct {
	authURL    string
	cookieName string
	client     *http.Client
	logger     *slog.Logger
}

// newOauth2ProxyVerifier returns a verifier when cookie mode is enabled in
// cfg (oauth2ProxyAuthURL non-empty). Returns nil when cookie mode is OFF so
// the shim short-circuits to the PAT/AVA-only paths with no overhead.
func newOauth2ProxyVerifier(cfg *config, logger *slog.Logger) cookieVerifier {
	if cfg.oauth2ProxyAuthURL == "" {
		return nil
	}
	return &oauth2ProxyVerifier{
		authURL:    cfg.oauth2ProxyAuthURL,
		cookieName: cfg.oauth2ProxyCookieName,
		// A short timeout keeps a slow/wedged oauth2-proxy from stalling
		// the proxy request; the caller maps the resulting error to a 502.
		client: &http.Client{
			Timeout: 5 * time.Second,
			// Never follow redirects: an unauthenticated /oauth2/auth replies
			// 401, but a misconfigured oauth2-proxy could 302 to the OIDC
			// start page; treating that as "not a 2xx" is the correct deny.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		logger: logger,
	}
}

func (v *oauth2ProxyVerifier) verify(ctx context.Context, r *http.Request) (cookieIdentity, error) {
	// Cheap gate: only attempt validation when the session cookie is present,
	// so PAT/AVA callers (and anonymous probes) never trigger a network hop.
	if !hasCookie(r, v.cookieName) {
		return cookieIdentity{}, errCookieMissing
	}

	sub, err := http.NewRequestWithContext(ctx, http.MethodGet, v.authURL, nil)
	if err != nil {
		return cookieIdentity{}, fmt.Errorf("%w: build request: %v", errCookieUpstream, err)
	}
	// Forward ONLY the cookies. We deliberately do not copy Authorization or
	// any inbound X-Auth-Request-* headers so the client cannot influence the
	// identity oauth2-proxy attests — the response headers are the sole
	// source of truth.
	if c := r.Header.Get("Cookie"); c != "" {
		sub.Header.Set("Cookie", c)
	}

	resp, err := v.client.Do(sub)
	if err != nil {
		return cookieIdentity{}, fmt.Errorf("%w: %v", errCookieUpstream, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		// Valid session (oauth2-proxy /oauth2/auth returns 202 Accepted).
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return cookieIdentity{}, errCookieInvalid
	default:
		return cookieIdentity{}, fmt.Errorf("%w: status %d", errCookieUpstream, resp.StatusCode)
	}

	email := strings.TrimSpace(resp.Header.Get("X-Auth-Request-Email"))
	if email == "" {
		// Defence in depth: a 2xx with no email means the session carried no
		// identity we can attribute. Fail closed rather than stamp an empty
		// X-Operator that the daemon would attribute to an unauthenticated
		// principal (same guard the AVA path applies).
		return cookieIdentity{}, errCookieInvalid
	}

	return cookieIdentity{
		Email:  email,
		Groups: splitHeaderList(resp.Header.Get("X-Auth-Request-Groups")),
	}, nil
}

// hasCookie reports whether the request carries either the base cookie or one
// of oauth2-proxy's numbered chunks (<name>_0, <name>_1, ...). This is only a
// cheap routing gate: the complete Cookie header is still forwarded to
// /oauth2/auth, which reconstructs and validates the session.
func hasCookie(r *http.Request, name string) bool {
	if name == "" {
		return false
	}
	for _, cookie := range r.Cookies() {
		if cookie.Name == name {
			return true
		}
		suffix, ok := strings.CutPrefix(cookie.Name, name+"_")
		if !ok || suffix == "" {
			continue
		}
		numbered := true
		for _, ch := range suffix {
			if ch < '0' || ch > '9' {
				numbered = false
				break
			}
		}
		if numbered {
			return true
		}
	}
	return false
}

// splitHeaderList splits a comma-separated header value (e.g. the
// X-Auth-Request-Groups oauth2-proxy emits) into trimmed, non-empty parts.
func splitHeaderList(v string) []string {
	if v == "" {
		return nil
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
