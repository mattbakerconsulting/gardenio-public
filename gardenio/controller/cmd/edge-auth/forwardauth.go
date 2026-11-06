package main

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"gardenio/controller/internal/edgeauth"
	"gardenio/controller/internal/oidcauth"
)

// forwardauth.go — Traefik-`forwardAuth`-compatible endpoint for AVA-fronted
// hosts (epic ka-0000004294, ka-0000005862).
//
// The Coopmux Rust pod sits behind Traefik with a `forwardAuth` middleware
// that today points at `gardenio-forwardauth` (oauth2-proxy → Keycloak). To
// retire Keycloak (epic ka-0000004118) we need an equivalent endpoint that
// verifies the AWS Verified Access JWT and returns the same
// X-Auth-Request-{User,Email,Groups} headers that oauth2-proxy emits. This
// file is that endpoint.
//
// Important: this path does NOT replace the PAT/HMAC envelope produced by
// `handleProxy`. The HMAC envelope is consumed by the kbeads Go daemon,
// which speaks the edgeauth.Verify protocol. The X-Auth-Request-* headers
// are consumed by Traefik's `forwardAuth` middleware, which then injects
// them into the upstream request before passing it on. The Coopmux Rust
// pod expects oauth2-proxy-shaped headers; this endpoint produces them.
//
// Ships dark behind a `cfg.avaTrustSigner` check — same back-compat posture
// as the AVA arm of `handleProxy`. The Traefik IngressRoute middleware swap
// is a separate change.

// handleForwardauth verifies an AVA JWT and emits oauth2-proxy-shaped
// X-Auth-Request-{User,Email,Groups} headers so Traefik can replace the
// existing `gardenio-forwardauth` (oauth2-proxy → Keycloak) middleware with
// this endpoint for AVA-fronted hosts. The Coopmux Rust pod is unchanged —
// it still reads X-Auth-Request-{Email,User,Groups} from the request.
//
// Disposition map (each line is an audit-emitted, JSON-bodied response):
//
//	avaVerifier == nil                 → 404 forwardauth_not_configured
//	missing X-Amzn-Ava-User-Context    → 401 missing_ava_token
//	verifier returns errAVAMissing     → 401 missing_ava_token (defence-in-depth)
//	verifier returns errAVAInvalid     → 401 invalid_ava_token
//	verifier returns other error       → 502 verifier_unavailable
//	verified but Email == ""           → 401 invalid_ava_token (DiD)
//	success                            → 202 + X-Auth-Request-{User,Email[,Groups]} + X-Auth-Audit-Id
//
// On success we write the X-Auth-Request-* headers BEFORE calling
// WriteHeader — Go's http.ResponseWriter flushes the header block on the
// first WriteHeader call, and Traefik's `forwardAuth` middleware reads
// trusted headers off the auth response. 202 (not 200) matches the
// oauth2-proxy convention; Traefik treats 2xx as "allow" regardless but
// keeping the same status keeps Traefik logs and dashboards equivalent
// after the middleware swap.
func (s *server) handleForwardauth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	auditID := newAuditID(s.now())
	rec := s.baseAuditRecord(r, auditEndpointForwardauth, "", auditID)

	// CLI path (ka-0000004118): a request carrying a GitLab PAT bearer is
	// unambiguously a programmatic client (kd/coop), not a browser. Validate it
	// with the same edgeauth.Validator the daemon shim (handleProxy) uses and
	// emit the identical X-Auth-Request-* success headers, so the AVA host
	// serves CLIs natively without the browser OIDC flow. Takes precedence over
	// the AVA-JWT path. Skipped when no validator is configured (PAT mode off).
	if pat := extractBearerToken(r); pat != "" && s.validator != nil {
		rec.Metadata.AuthMode = auditAuthModePAT
		s.forwardauthPAT(w, r, rec, auditID, pat)
		return
	}

	rec.Metadata.AuthMode = auditAuthModeAVA

	if s.avaVerifier == nil {
		// Dark-ship guard: when the chart did not configure
		// avaTrustSigner the endpoint reports "not configured" rather
		// than 401, so a misrouted Traefik middleware shows up loudly
		// in Traefik logs instead of silently failing every request.
		s.failAudit(rec, auditDetailDenyForwardauthOff)
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"forwardauth_not_configured"}`))
		return
	}

	if r.Header.Get(oidcauth.AVAHeaderName) == "" {
		s.failAudit(rec, auditDetailDenyMissingAVAHeader)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"missing_ava_token"}`))
		return
	}

	id, err := s.avaVerifier.verify(r.Context(), r)
	switch {
	case err == nil:
		// fall through to success path below
	case errors.Is(err, errAVAMissing):
		// Defence in depth: we already checked the header above. If the
		// verifier still reports missing it means the implementation
		// reads a different header field; we treat that as missing-token
		// rather than silently dropping the request.
		s.failAudit(rec, auditDetailDenyMissingAVAHeader)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"missing_ava_token"}`))
		return
	case errors.Is(err, errAVAInvalid):
		s.failAudit(rec, auditDetailDenyInvalidAVA)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_ava_token"}`))
		return
	default:
		s.logger.Warn("edge-auth: forwardauth verifier unavailable", "error", err)
		s.failAudit(rec, auditDetailDenyVerifierUnavailable)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"verifier_unavailable"}`))
		return
	}

	if id.Email == "" {
		// Defence in depth: the verifier accepted the signature but
		// produced no email. Stamping X-Auth-Request-Email="" would let
		// Traefik forward an unauthenticated principal to coopmux.
		s.failAudit(rec, auditDetailDenyInvalidAVA)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_ava_token"}`))
		return
	}

	// Headers must be set BEFORE WriteHeader — once WriteHeader writes
	// the status line the header block is flushed and subsequent
	// .Set() calls are ignored.
	w.Header().Set("X-Auth-Request-User", id.Email)
	w.Header().Set("X-Auth-Request-Email", id.Email)
	// Match oauth2-proxy: emit X-Auth-Request-Groups only when there is
	// at least one non-empty group. An empty header would be ambiguous
	// — coopmux cannot tell "no groups" from "header omitted" — and
	// oauth2-proxy chose the latter.
	groups := joinNonEmptyGroups(id.Groups)
	if groups != "" {
		w.Header().Set("X-Auth-Request-Groups", groups)
	}
	w.Header().Set(headerAuditID, auditID)
	s.stampForwardAuthProvenance(w, id.Email, groups, auditID)

	// Mirror Email into Name so the audit OCSF schema validator's
	// "success implies at least one of email/name" rule holds even
	// when the trust provider stops emitting a display name.
	s.okAudit(rec, &edgeauth.Identity{Email: id.Email, Name: id.Email})
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// forwardauthPAT authenticates a programmatic client by validating a GitLab PAT
// (the same edgeauth.Validator the daemon shim handleProxy uses) and, on
// success, emits the oauth2-proxy-shaped X-Auth-Request-* headers + 202 —
// identical to the AVA-JWT success path so coopmux sees the same principal. The
// verified group membership (cfg.gitlabAuthGroup) is emitted as the single
// group claim so coopmux's group-based authz matches the browser path. Error
// mapping mirrors handleProxy.
func (s *server) forwardauthPAT(w http.ResponseWriter, r *http.Request, rec auditRecord, auditID, pat string) {
	id, err := s.validator.Validate(r.Context(), pat)
	switch {
	case err == nil:
		// fall through to success path below
	case errors.Is(err, edgeauth.ErrInvalidToken), errors.Is(err, edgeauth.ErrEmptyToken):
		s.failAudit(rec, auditDetailDenyInvalidPAT)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
		return
	case errors.Is(err, edgeauth.ErrNotInGroup):
		// Validate returns the partial /user identity on ErrNotInGroup so the
		// deny record attributes the request to the actor.
		s.failAuditWithActor(rec, auditDetailDenyNotInGroup, id)
		s.metrics.record403(auditEndpointForwardauth, auditDetailDenyNotInGroup)
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"not_in_group"}`))
		return
	default:
		s.logger.Warn("edge-auth: forwardauth PAT validation failed", "error", err)
		s.failAudit(rec, auditDetailDenyGitLabDown)
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`{"error":"gitlab_unavailable"}`))
		return
	}

	if id.Email == "" {
		// Defence in depth: a verified PAT with no email would let Traefik
		// forward an unattributable principal to coopmux.
		s.failAudit(rec, auditDetailDenyInvalidPAT)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"invalid_token"}`))
		return
	}

	// Headers must be set BEFORE WriteHeader (see handleForwardauth).
	w.Header().Set("X-Auth-Request-User", id.Email)
	w.Header().Set("X-Auth-Request-Email", id.Email)
	groups := joinNonEmptyGroups([]string{s.cfg.gitlabAuthGroup})
	if groups != "" {
		w.Header().Set("X-Auth-Request-Groups", groups)
	}
	w.Header().Set(headerAuditID, auditID)
	s.stampForwardAuthProvenance(w, id.Email, groups, auditID)

	s.okAudit(rec, id)
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// stampForwardAuthProvenance signs the forwardauth identity assertion and
// stamps the X-Edge-Auth-Fwd-Sig / X-Edge-Auth-Fwd-Ts provenance envelope on
// the auth response, so an upstream that lists those headers in
// authResponseHeaders can prove the X-Auth-Request-* identity arrived via this
// endpoint rather than being forged by a caller that bypassed Traefik (task
// ka-0000004151). email/groups/auditID MUST be the exact header values just
// stamped (groups "" when no groups header was emitted).
//
// Dark-ship: with no HMAC key configured (edgeAuth.dryRun, or an env that has
// not provisioned the shared secret) no envelope is emitted — identical posture
// to the daemon shim's handleProxy, and the upstream verifier's burn-in mode
// tolerates its absence. Must be called BEFORE WriteHeader (like the identity
// headers): once the status is written the header block is flushed.
func (s *server) stampForwardAuthProvenance(w http.ResponseWriter, email, groups, auditID string) {
	if len(s.cfg.hmacKey) == 0 {
		return
	}
	ts := strconv.FormatInt(s.now().Unix(), 10)
	sig := edgeauth.SignForwardAuth([]byte(s.cfg.hmacKey), email, groups, auditID, ts)
	w.Header().Set(edgeauth.ForwardAuthHeaderSig, sig)
	w.Header().Set(edgeauth.ForwardAuthHeaderTs, ts)
}

// joinNonEmptyGroups returns a comma-separated list of the non-empty
// elements of groups, matching the format oauth2-proxy uses for
// X-Auth-Request-Groups. Returns "" when every element is empty so the
// caller can decide whether to emit the header at all.
func joinNonEmptyGroups(groups []string) string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		if g = strings.TrimSpace(g); g != "" {
			out = append(out, g)
		}
	}
	return strings.Join(out, ",")
}
