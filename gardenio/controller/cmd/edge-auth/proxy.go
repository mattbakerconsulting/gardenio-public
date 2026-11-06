package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/edgeauth"
	"gardenio/controller/internal/oidcauth"
)

// Edge-auth header constants. Daemons (W-B-11) verify the signature and
// trust X-Operator only when X-Edge-Auth-Sig recomputes against the shared
// HMAC key and the timestamp is fresh.
const (
	headerOperator    = "X-Operator"
	headerAuditID     = "X-Auth-Audit-Id"
	headerSignature   = "X-Edge-Auth-Sig"
	headerSignatureV2 = "X-Edge-Auth-Sig-V2"
	headerTimestamp   = "X-Edge-Auth-Ts"
	headerVersion     = "X-Edge-Auth-Version"
	headerAuthMode    = "X-Edge-Auth-Mode"
	headerRoles       = "X-Edge-Auth-Roles"
	headerLegacyRoles = "X-Gardenio-Roles"

	// headerOperatedBy is the operator-attribution header the daemon stamps
	// onto downstream kbeads writes. On the edge path the daemon derives it
	// from the HMAC-verified operator (kbeads applyOperatedByHeader), so an
	// inbound client value is inert today — but it is a spoofable identity
	// header, so we strip it alongside the other identity headers below
	// (ka-0000002305) to keep the "daemon never sees them" invariant true for
	// any future downstream that reads it raw.
	headerOperatedBy = "X-Operated-By"

	// headerDryRun is stamped on synthetic 200 responses when the chart
	// flag edgeAuth.dryRun is true (ka-0000002544). CLI smoke tests assert
	// on this header to distinguish "validated but did not proxy" from a
	// real upstream success.
	headerDryRun = "X-Edge-Auth-Dry-Run"
)

// proxyUpstream binds one path-prefix to an in-cluster Service URL. The
// shim accepts requests at `<prefix>/...`, strips the prefix, and forwards
// to target. Upstreams expose themselves at root after the strip; the
// BASE_PATH retrofit (W-E-1..3) makes them generate prefixed links.
type proxyUpstream struct {
	name   string // audit-log label, e.g. "kbeads"
	prefix string // always slash-prefixed and slash-suffixed, e.g. "/kd/"
	target *url.URL
	proxy  *httputil.ReverseProxy
}

func newProxyUpstream(name, prefix, rawURL string, logger *slog.Logger) (*proxyUpstream, error) {
	if name == "" {
		return nil, errors.New("upstream name is required")
	}
	if !strings.HasPrefix(prefix, "/") {
		return nil, fmt.Errorf("prefix must start with /: %q", prefix)
	}
	prefix = "/" + strings.Trim(prefix, "/") + "/"
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid upstream URL %q: %w", rawURL, err)
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, fmt.Errorf("upstream URL %q missing scheme or host", rawURL)
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			stripped := strings.TrimPrefix(pr.In.URL.Path, strings.TrimSuffix(prefix, "/"))
			if stripped == "" {
				stripped = "/"
			}
			pr.Out.URL.Path = stripped
			pr.Out.URL.RawPath = ""
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Warn("edge-auth: upstream proxy error",
				"upstream", name, "path", r.URL.Path, "error", err)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"upstream_unavailable"}`))
		},
	}
	return &proxyUpstream{name: name, prefix: prefix, target: target, proxy: rp}, nil
}

// buildUpstreams returns the configured upstreams in registration order.
// Upstreams without a URL are skipped silently; chart rollouts can land
// the deployment ahead of every upstream Service.
func buildUpstreams(cfg *config, logger *slog.Logger) ([]*proxyUpstream, error) {
	specs := []struct {
		name   string
		prefix string
		url    string
	}{
		// /kd/nats/ MUST precede /kd/ — matchUpstream is first-match-by-order,
		// so the realtime NATS WebSocket channel routes to the NATS service
		// rather than falling through to the kbeads daemon (ka-0000006000).
		{"nats", "/kd/nats/", cfg.upstreamNats},
		{"kbeads", "/kd/", cfg.upstreamKbeads},
		{"coopmux", "/mux/", cfg.upstreamCoopmux},
	}
	var ups []*proxyUpstream
	for _, s := range specs {
		if s.url == "" {
			continue
		}
		u, err := newProxyUpstream(s.name, s.prefix, s.url, logger)
		if err != nil {
			return nil, fmt.Errorf("upstream %s: %w", s.name, err)
		}
		ups = append(ups, u)
	}
	return ups, nil
}

// proxyEnabled is true once the shim has all the wiring it needs to act
// as a CLI gateway: at least one auth backend (GitLab PAT validator, AVA,
// Entra, or cookie verifier), an HMAC key, and at least one upstream.
//
// Either auth backend is sufficient — AVA-only deployments (epic
// ka-0000005862) leave s.validator nil but still need proxyEnabled to be
// true so /v1/... requests carrying X-Amzn-Ava-User-Context route to the
// AVA branch of handleProxy. Requiring BOTH backends would 503 every
// AVA-only request even when the verifier is correctly configured.
func (s *server) proxyEnabled() bool {
	haveAuth := s.validator != nil || s.avaVerifier != nil || s.entraVerifier != nil || s.cookieVerifier != nil
	return haveAuth && len(s.cfg.hmacKey) > 0 && len(s.upstreams) > 0
}

// handleProxy authenticates the caller, stamps trust headers, and forwards
// to the matching upstream. Two auth edges feed the same envelope:
//
//   - PAT mode (status quo). Bearer GitLab PAT, validated against the
//     configured group; identity comes from the GitLab /user endpoint. This
//     is the path the kd / coop CLIs have always used.
//
//   - AVA mode (epic ka-0000005862). X-Amzn-Ava-User-Context JWT stamped by
//     AWS Verified Access, verified against the pinned signer; identity
//     comes from the trust-provider claims AVA forwards. Opt-in via
//     EDGE_AUTH_AVA_TRUST_SIGNER; ships dark in this MR.
//
// Both paths produce the identical HMAC envelope (X-Operator /
// X-Auth-Audit-Id / X-Edge-Auth-Sig / X-Edge-Auth-Ts), so the daemon-side
// validator (kbeads internal/edgeauth/verify.go, ka-0000001678) is
// indifferent to which edge a request crossed. The auth_mode metadata
// surfaces the distinction in OCSF audit lines.
//
// Precedence: when the AVA header is present AND the AVA verifier is
// configured, AVA wins. We do NOT fall through to the PAT path on AVA
// failure — that would silently downgrade a tampered AVA request to a
// different auth flow (same reason oidcauth.authBranch rejects rather than
// falls through). When the AVA header is absent, the PAT path runs as it
// always has.
func (s *server) handleProxy(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	auditID := newAuditID(s.now())

	if !s.proxyEnabled() {
		rec := s.baseAuditRecord(r, auditEndpointProxy, "", auditID)
		s.failAudit(rec, auditDetailDenyProxyOff)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"proxy_not_configured"}`))
		return
	}

	up := s.matchUpstream(r.URL.Path)
	if up == nil {
		rec := s.baseAuditRecord(r, auditEndpointProxy, "", auditID)
		s.failAudit(rec, auditDetailDenyUnknownPrefix)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"unknown_upstream"}`))
		return
	}

	rec := s.baseAuditRecord(r, auditEndpointProxy, up.name, auditID)

	// Identity resolution precedence is deliberately non-downgradable:
	// AVA header > matching-issuer Entra Bearer > GitLab PAT > cookie.
	// Once a credential is claimed by a stronger structured-token verifier,
	// verification failure is terminal and never falls through to PAT/cookie.
	var (
		id            *edgeauth.Identity
		authMode      string
		verifiedRoles []string
	)
	cred := authorizationCredentialFromRequest(r)
	switch {
	case s.avaVerifier != nil && r.Header.Get(oidcauth.AVAHeaderName) != "":
		authMode = auditAuthModeAVA
		rec.Metadata.AuthMode = authMode
		avaID, err := s.avaVerifier.verify(r.Context(), r)
		if err != nil {
			s.logger.Debug("edge-auth: AVA verification failed", "upstream", up.name, "error", err)
			s.failAudit(rec, auditDetailDenyInvalidAVA)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_ava_token"}`))
			return
		}
		// AVA's Cedar policy already gated the request upstream; there is no
		// "not_in_group" disposition to enforce here. We trust the verified
		// email and stamp it into the same envelope PAT mode would.
		//
		// The public AVA path deliberately leaves verifiedRoles empty so it
		// signs a v1 envelope, preserving EXACTLY its pre-MR authority
		// (operator decision ka-0000001641, "additive": this MR adds v2 role
		// enforcement to the PiID/Entra and explicitly-configured Ziti
		// entra-app-roles paths ONLY; it does not change AVA). Downstream
		// (kbeads + coopmux) grants a verified v1 envelope its historical
		// authority, so AVA browser users keep working with enforce=true. Do
		// NOT map avaID.Groups to canonical roles here: that would upgrade AVA
		// to v2 and, because avaID.Groups is the POST-normalization role list
		// (oidcauth.RolesFromContext via ava.go), also reintroduce the
		// bare-name/suffix-escalation tension a role mapper would have to
		// solve. See ka-0000006523.
		id = &edgeauth.Identity{Email: avaID.Email, Name: avaID.Email}

	case s.cfg.entraIssuerURL != "" &&
		cred.scheme == "bearer" &&
		oidcauth.UnverifiedIssuer(cred.token) == s.cfg.entraIssuerURL:
		authMode = auditAuthModeEntra
		rec.Metadata.AuthMode = authMode
		if s.entraVerifier == nil {
			s.failAudit(rec, auditDetailDenyEntraUnavailable)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"entra_unavailable"}`))
			return
		}
		entraID, err := s.entraVerifier.verify(r.Context(), r)
		switch {
		case err == nil:
			id = &edgeauth.Identity{Email: entraID.Email, Name: entraID.Email}
			verifiedRoles = canonicalGardenioAppRoles(entraID.Roles)
		case errors.Is(err, errEntraForbidden):
			s.failAudit(rec, auditDetailDenyEntraForbidden)
			s.metrics.record403(up.name, auditDetailDenyEntraForbidden)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"entra_forbidden"}`))
			return
		case errors.Is(err, errEntraUnavailable):
			s.logger.Warn("edge-auth: Entra verifier unavailable", "upstream", up.name, "error", err)
			s.failAudit(rec, auditDetailDenyEntraUnavailable)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"entra_unavailable"}`))
			return
		default:
			s.failAudit(rec, auditDetailDenyInvalidEntra)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_entra_token"}`))
			return
		}

	case cred.token != "":
		authMode = auditAuthModePAT
		rec.Metadata.AuthMode = authMode
		pat := cred.token
		// AVA-only deployment posture (epic ka-0000005862): the chart
		// can wire the AVA verifier with no GitLab validator. If a
		// caller still presents a Bearer token to such a shim there is
		// no PAT validator to consult, so we 401 rather than nil-panic
		// on s.validator.Validate below.
		if s.validator == nil {
			s.failAudit(rec, auditDetailDenyInvalidPAT)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
			return
		}
		patID, err := s.validator.Validate(r.Context(), pat)
		switch {
		case err == nil:
			id = patID
			// PAT stays a v1 envelope: it carries no verified role claim, and
			// per operator decision ka-0000001641 the daemon restores pre-MR
			// authority for a cryptographically verified v1 envelope rather than
			// deriving a role here. (Do not map GitLab access_level.)
		case errors.Is(err, edgeauth.ErrInvalidToken),
			errors.Is(err, edgeauth.ErrEmptyToken):
			s.failAudit(rec, auditDetailDenyInvalidPAT)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
			return
		case errors.Is(err, edgeauth.ErrNotInGroup):
			// Validate returns the partial identity from /user on
			// ErrNotInGroup so the OCSF deny record attributes the request
			// to the actor.
			s.failAuditWithActor(rec, auditDetailDenyNotInGroup, patID)
			s.metrics.record403(up.name, auditDetailDenyNotInGroup)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":"not_in_group"}`))
			return
		default:
			s.logger.Warn("edge-auth: proxy auth failed", "upstream", up.name, "error", err)
			s.failAudit(rec, auditDetailDenyGitLabDown)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"gitlab_unavailable"}`))
			return
		}

	case s.cookieVerifier != nil && hasCookie(r, s.cfg.oauth2ProxyCookieName):
		// gb-login browser-session-cookie edge (ka-0000000080). No Bearer
		// PAT and no AVA header, but the request carries the oauth2-proxy
		// session cookie. Validate it against oauth2-proxy and stamp the
		// attested email into the same envelope — see cookie_auth.go.
		authMode = auditAuthModeCookie
		rec.Metadata.AuthMode = authMode
		cookieID, err := s.cookieVerifier.verify(r.Context(), r)
		switch {
		case err == nil:
			id = &edgeauth.Identity{Email: cookieID.Email, Name: cookieID.Email}
			verifiedRoles = canonicalCookieGardenioRoles(cookieID.Groups, s.cfg.oauth2ProxyRoleSource)
		case errors.Is(err, errCookieInvalid):
			s.failAudit(rec, auditDetailDenyInvalidCookie)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_session"}`))
			return
		default:
			// errCookieUpstream (or the can't-happen errCookieMissing): the
			// session may be valid but oauth2-proxy is unreachable. 502 so the
			// CLI can retry rather than treating it as a hard auth failure.
			s.logger.Warn("edge-auth: cookie verification upstream error", "upstream", up.name, "error", err)
			s.failAudit(rec, auditDetailDenyCookieUpstream)
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"error":"cookie_upstream_unavailable"}`))
			return
		}

	default:
		// No credential at all: no AVA header, no Bearer PAT, no session
		// cookie (or cookie mode off). Attribute the deny to the PAT edge for
		// backward-compatible audit dashboards.
		rec.Metadata.AuthMode = auditAuthModePAT
		s.failAudit(rec, auditDetailDenyMissingToken)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"missing_token"}`))
		return
	}

	// Defense-in-depth email-domain gate (ka-0000000533). Off unless the
	// operator sets EDGE_AUTH_ALLOWED_EMAIL_DOMAINS. Applies to every auth
	// mode — the identity is fully resolved by this point. Attribute the deny
	// to the actual operator email so audit dashboards can see who was bounced.
	if !emailDomainAllowed(id.Email, s.cfg.allowedEmailDomains) {
		s.failAuditWithActor(rec, auditDetailDenyEmailDomain, id)
		s.metrics.record403(up.name, auditDetailDenyEmailDomain)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"email_domain_not_allowed"}`))
		return
	}

	ts := strconv.FormatInt(s.now().Unix(), 10)
	upstreamPath := strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(up.prefix, "/"))
	if upstreamPath == "" {
		upstreamPath = "/"
	}

	// Stage 3 / Safe-deploy dry-run: identity is resolved, audit log fires,
	// but we never hit the upstream. The CLI gets a synthetic 200 stamped
	// with X-Edge-Auth-Dry-Run so smoke tests can verify the auth path
	// end-to-end without risking real traffic. Behaves identically on the
	// AVA path so AVA smoke tests work the same way.
	if s.cfg.dryRun {
		s.logger.Info("edge-auth: dry-run short-circuit",
			"upstream", up.name,
			"operator", id.Email,
			"auth_mode", authMode,
			"roles", verifiedRoles,
			"audit_id", auditID,
			"path", upstreamPath,
		)
		s.okAuditDryRun(rec, id)
		w.Header().Set(headerDryRun, "true")
		w.Header().Set(headerAuditID, auditID)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"dry_run"}`))
		return
	}

	// Remove the caller's raw PAT or Entra access token so the upstream daemon
	// sees only the HMAC envelope. The AVA header is also stripped — the
	// envelope remains the single source of identity.
	r.Header.Del("Authorization")
	r.Header.Del(oidcauth.AVAHeaderName)
	// Strip the browser session cookie and any inbound X-Auth-Request-*
	// headers so the daemon never sees them — the HMAC envelope is the single
	// source of identity. Leaving them in place would let a client smuggle a
	// forged X-Auth-Request-Email past the shim, and encourages downstreams
	// to grow a parallel (spoofable) trust path.
	r.Header.Del("Cookie")
	r.Header.Del("X-Auth-Request-User")
	r.Header.Del("X-Auth-Request-Email")
	r.Header.Del("X-Auth-Request-Groups")
	r.Header.Del("X-Auth-Request-Access-Token")
	// X-Operated-By is operator attribution the daemon derives from the
	// HMAC-verified operator, never from the caller. Strip any inbound value
	// so a future downstream that reads it raw cannot be confused (ka-0000002305).
	r.Header.Del(headerOperatedBy)
	// Strip every client-supplied identity-envelope field before stamping the
	// server-derived values below. This includes both the v2 roles header and
	// the spelling used by the original design draft.
	for _, header := range []string{
		headerOperator,
		headerAuditID,
		headerSignature,
		headerSignatureV2,
		headerTimestamp,
		headerVersion,
		headerAuthMode,
		headerRoles,
		headerLegacyRoles,
	} {
		r.Header.Del(header)
	}

	r.Header.Set(headerOperator, id.Email)
	r.Header.Set(headerAuditID, auditID)
	r.Header.Set(headerTimestamp, ts)
	// Preserve the v1 signature during the rolling transition so an older
	// daemon pod can continue serving reads while new pods come up. Updated
	// daemons never use this signature to authorize a mutation.
	r.Header.Set(
		headerSignature,
		signEdgeAuth([]byte(s.cfg.hmacKey), id.Email, auditID, upstreamPath, ts),
	)
	if len(verifiedRoles) > 0 {
		roles := strings.Join(verifiedRoles, ",")
		r.Header.Set(headerVersion, "2")
		r.Header.Set(headerAuthMode, authMode)
		r.Header.Set(headerRoles, roles)
		r.Header.Set(
			headerSignatureV2,
			signEdgeAuthV2(
				[]byte(s.cfg.v2HMACKey()),
				id.Email,
				authMode,
				roles,
				auditID,
				r.Method,
				upstreamPath,
				ts,
			),
		)
	} else {
		// Identities without a verified canonical Gardenio role keep their v1
		// envelope for read compatibility; downstream services deliberately
		// refuse it on mutation paths.
		r.Header.Del(headerSignatureV2)
	}

	s.logger.Info("edge-auth: proxying",
		"upstream", up.name,
		"operator", id.Email,
		"auth_mode", authMode,
		"roles", verifiedRoles,
		"audit_id", auditID,
		"path", upstreamPath,
	)
	s.okAudit(rec, id)

	// Clear the `application/json` Content-Type we set at the top of the
	// handler for the deny/error paths. httputil.ReverseProxy copies the
	// upstream's response headers with Header.Add (not Set), so leaving our
	// value in place produces a two-valued `Content-Type: application/json,
	// text/html` on every proxied response. Browsers honour the first value
	// and render an HTML upstream (the coopmux SPA) as plain text — the
	// "front door returns a bunch of JavaScript" symptom (ka-0000007632).
	// Deleting it lets the upstream's own Content-Type be the only one.
	w.Header().Del("Content-Type")

	up.proxy.ServeHTTP(w, r)
}

// matchUpstream returns the upstream whose prefix matches path. The match
// is exact on the slash-terminated prefix; "/kdsomething" does not match
// the "/kd/" prefix.
func (s *server) matchUpstream(path string) *proxyUpstream {
	for _, up := range s.upstreams {
		trimmed := strings.TrimSuffix(up.prefix, "/")
		if path == trimmed || strings.HasPrefix(path, up.prefix) {
			return up
		}
	}
	return nil
}

// signEdgeAuth computes the HMAC-SHA256 of (operator, audit-id, upstream
// path, ts) with newline separators, base64-encoded. Daemons (W-B-11)
// recompute and compare in constant time before trusting X-Operator.
func signEdgeAuth(key []byte, operator, auditID, path, ts string) string {
	msg := operator + "\n" + auditID + "\n" + path + "\n" + ts
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// signEdgeAuthV2 binds every authorization-bearing field to the HMAC. Roles
// must already be filtered, deduplicated, sorted, and comma-joined by the
// identity-provider-specific canonical role mapper.
func signEdgeAuthV2(
	key []byte,
	operator, authMode, roles, auditID, method, path, ts string,
) string {
	msg := strings.Join([]string{
		"v2",
		operator,
		authMode,
		roles,
		auditID,
		strings.ToUpper(method),
		path,
		ts,
	}, "\n")
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(msg))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// canonicalGardenioAppRoles accepts only the bare app-role values emitted by
// the Gardenio Entra application. This is the ONLY role source that upgrades a
// request to a role-bearing v2 envelope (the direct PiID/Entra bearer path and
// the explicitly-configured Ziti oauth2-proxy entra-app-roles cookie source).
// A verified provider-group claim is authentic, but an unrelated group whose
// leaf happens to be "gardenio-admin" is not authorized to grant a Gardenio
// role, so group-path claims are NOT accepted here.
func canonicalGardenioAppRoles(roles []string) []string {
	return canonicalGardenioRoles(roles, func(value string) (string, bool) {
		switch value {
		case oidcauth.RoleViewer, oidcauth.RoleOperator, oidcauth.RoleAdmin:
			return value, true
		default:
			return "", false
		}
	})
}

// canonicalCookieGardenioRoles maps the authenticated oauth2-proxy response
// according to the explicitly configured provider contract.
//
// Only an explicitly-configured entra-app-roles source produces v2 roles. The
// DEFAULT (empty / provider-groups — the Helm chart default) yields NO roles,
// so a default cookie deployment signs a v1 envelope and keeps EXACTLY its
// pre-MR authority (operator decision ka-0000001641, "additive": generic
// provider-group cookies are an existing path this MR does not change). An
// unknown source also grants no role (fail closed).
func canonicalCookieGardenioRoles(groups []string, roleSource string) []string {
	switch roleSource {
	case oauth2ProxyRoleSourceEntraAppRoles:
		return canonicalGardenioAppRoles(groups)
	default:
		// "" / provider-groups (default) and any unknown source: no v2 roles.
		return nil
	}
}

// canonicalGardenioRoles filters provider-specific role inputs, removes
// duplicates, and sorts the wire value so Go and Rust verifiers sign identical
// bytes.
func canonicalGardenioRoles(
	values []string,
	resolve func(string) (string, bool),
) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		role, ok := resolve(strings.TrimSpace(value))
		if ok && !seen[role] {
			seen[role] = true
			out = append(out, role)
		}
	}
	sort.Strings(out)
	return out
}

// newAuditID returns a 26-char ULID-shaped correlation id: 48-bit ms
// timestamp + 80 random bits, encoded with Crockford-style base32 (RFC
// 4648 alphabet, no padding). The timestamp prefix keeps IDs roughly
// sortable in audit logs without pulling in a ULID dependency.
func newAuditID(now time.Time) string {
	var b [16]byte
	ms := uint64(now.UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	_, _ = rand.Read(b[6:])
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:])
}
