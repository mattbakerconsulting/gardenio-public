package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/edgeauth"
)

// server is the edge-auth HTTP surface. Beyond health probes and the
// /auth/check self-test from W-B-8, W-B-9 adds the proxy surface: per-
// upstream path-prefix handlers that authenticate the Bearer PAT, stamp
// X-Operator + HMAC trust headers, and reverse-proxy to in-cluster
// Services. Daemons (W-B-11) verify the HMAC before trusting X-Operator.
type server struct {
	cfg       *config
	version   string
	logger    *slog.Logger
	validator edgeauth.Validator
	// avaVerifier is the AVA-mode path. nil when cfg.avaTrustSigner is empty
	// (AVA mode OFF; the shim behaves as a PAT-only edge — back-compat for
	// every environment that has not yet been migrated behind AWS Verified
	// Access). When non-nil, requests carrying X-Amzn-Ava-User-Context are
	// authenticated against it instead of GitLab.
	avaVerifier avaVerifier
	// entraVerifier is the PiID delegated-token path. nil when no Entra issuer
	// is configured. A matching-issuer Bearer is fully verified here and never
	// downgraded to the GitLab PAT path after a verification failure.
	entraVerifier entraVerifier
	// cookieVerifier is the gb-login browser-session-cookie path
	// (ka-0000000080). nil when cfg.oauth2ProxyAuthURL is empty (cookie mode
	// OFF; the shim authenticates PAT + AVA only). When non-nil, a proxy
	// request carrying the session cookie but no Bearer / AVA header is
	// validated against oauth2-proxy and stamped into the same HMAC envelope.
	cookieVerifier cookieVerifier
	upstreams      []*proxyUpstream
	now            func() time.Time
	audit          *auditSink
	// metrics owns the Prometheus instruments (gardenio_forwardauth_403_total)
	// and their dedicated registry, which main() mounts on /metrics.
	metrics *edgeAuthMetrics
}

func newServer(cfg *config, version string, logger *slog.Logger) *server {
	s := &server{cfg: cfg, version: version, logger: logger, now: time.Now}
	// Prometheus instruments on a dedicated registry; main() serves it on
	// /metrics. Built unconditionally so every 403 site can record without a
	// nil check beyond record403's own nil-receiver guard.
	s.metrics = newEdgeAuthMetrics()
	// Audit sink defaults to stderr; tests inject a buffer via s.audit.
	// Closure keeps the timestamp aligned with s.now overrides used in
	// fixed-clock tests.
	s.audit = newAuditSink(nil, func() time.Time { return s.now() })
	// Build the GitLab PAT validator when its config is present. The
	// service still starts (and serves /healthz) without it so chart
	// rollouts can land the deployment before the GitLab base URL or
	// group are populated — /auth/check and the proxy surface will
	// report 503 in that case.
	if cfg.patDisabled {
		// Box auth profile (ka-0000006087 §6): PAT path off. Leave s.validator
		// nil so no gitlab.com calls are ever made and PAT-bearing requests are
		// rejected by the nil-validator guards on every path. The readiness gate
		// below tolerates this as long as cookie (or AVA) mode is configured.
		logger.Info("edge-auth: GitLab-PAT path disabled (EDGE_AUTH_PAT_DISABLED); PAT-bearing requests will be rejected")
	} else if cfg.gitlabBaseURL != "" && cfg.gitlabAuthGroup != "" {
		v, err := edgeauth.NewGitLabValidator(edgeauth.GitLabConfig{
			BaseURL: cfg.gitlabBaseURL,
			Group:   cfg.gitlabAuthGroup,
		})
		if err != nil {
			logger.Error("edge-auth: failed to build GitLab validator", "error", err)
		} else {
			s.validator = v
		}
	}
	ups, err := buildUpstreams(cfg, logger)
	if err != nil {
		logger.Error("edge-auth: failed to build upstream proxy table", "error", err)
	} else {
		s.upstreams = ups
	}
	// AVA-mode wiring (ka-0000005862). Returns nil when avaTrustSigner is
	// empty so the PAT path stays the only auth surface — this MR ships
	// dark, no production env flips avaTrustSigner on (A2 is a separate MR).
	s.avaVerifier = newAvaHandleVerifier(cfg, logger)
	if s.avaVerifier != nil {
		logger.Info("edge-auth: AVA-mode path enabled",
			"trust_signer", cfg.avaTrustSigner,
			"region", cfg.avaRegion,
		)
	}
	var entraErr error
	s.entraVerifier, entraErr = newEntraHandleVerifier(cfg, logger)
	if entraErr != nil {
		logger.Error("edge-auth: failed to configure Entra delegated-token path", "error", entraErr)
	} else if s.entraVerifier != nil {
		logger.Info("edge-auth: Entra delegated-token path enabled",
			"issuer", cfg.entraIssuerURL,
			"audience", cfg.entraAudience,
			"tenant_id", cfg.entraTenantID,
			"required_scope", cfg.entraRequiredScope,
			"required_role", cfg.entraRequiredRole,
			"allowed_clients", len(cfg.entraAllowedClientIDs),
		)
	}
	// Cookie-mode wiring (ka-0000000080). Returns nil when oauth2ProxyAuthURL
	// is empty so the PAT/AVA paths stay the only auth surfaces — ships dark
	// until the chart points it at the in-cluster oauth2-proxy.
	s.cookieVerifier = newOauth2ProxyVerifier(cfg, logger)
	if s.cookieVerifier != nil {
		logger.Info("edge-auth: cookie-mode path enabled",
			"oauth2_proxy_auth_url", cfg.oauth2ProxyAuthURL,
			"cookie_name", cfg.oauth2ProxyCookieName,
			"role_source", cfg.oauth2ProxyRoleSource,
		)
	}
	return s
}

// ready reports whether the service is ready to handle traffic. The PAT
// validation surface needs a configured validator; without one,
// /auth/check and the proxy surface will 503 but /healthz still reports
// up so we can land deployments before secrets land. The proxy surface
// has additional requirements (HMAC key, upstreams) that the per-route
// handler enforces — they do not gate /readyz so chart rollouts can land
// the deployment before HMAC secrets exist.
func (s *server) ready() (bool, string) {
	// A configured Entra route must be usable before the pod is ready even
	// when PAT/AVA/cookie auth could serve other routes. Otherwise Traefik can
	// send delegated-token traffic to a pod whose OIDC discovery is still
	// fail-closed. A construction error also remains NotReady instead of
	// silently downgrading matching-issuer tokens onto PAT authentication.
	if s.cfg.entraIssuerURL != "" {
		if s.entraVerifier == nil {
			return false, "Entra verifier not configured"
		}
		if !s.entraVerifier.ready() {
			return false, "Entra OIDC provider not ready"
		}
	}
	if s.validator != nil {
		return true, ""
	}
	if s.avaVerifier != nil || s.cookieVerifier != nil {
		return true, ""
	}
	if s.entraVerifier != nil {
		return true, ""
	}
	// Box auth profile (ka-0000006087 §6): with the PAT path explicitly
	// disabled, the GitLab validator is intentionally nil. The edge is still
	// ready as long as another identity source is wired — cookie auth (the
	// in-box Keycloak via oauth2-proxy) or AVA. Without any auth mode the shim
	// would admit no one, so keep reporting NotReady to surface the misconfig.
	if s.cfg.patDisabled {
		return false, "PAT disabled but no cookie/AVA/Entra auth mode configured"
	}
	return false, "gitlab validator not configured"
}

func (s *server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/auth/check", s.handleAuthCheck)
	// Traefik forwardAuth-compatible endpoint (ka-0000004294). Mounted
	// unconditionally so a misrouted IngressRoute surfaces as a 404 +
	// audit line rather than silently fanning out to handleProxy. The
	// handler short-circuits to 404 forwardauth_not_configured when
	// s.avaVerifier is nil.
	mux.HandleFunc("/forwardauth/verify", s.handleForwardauth)
	// Dedicated Entra bearer-token ForwardAuth endpoint for the private /beads
	// direct-bearer route. It enforces scp/azp/tid/role at the edge while the
	// viewer independently re-verifies the original Authorization token.
	mux.HandleFunc("/forwardauth/entra", s.handleEntraForwardauth)
	for _, up := range s.upstreams {
		// Register both the slash-terminated form and the prefix-without-trailing-slash
		// (i.e. "/kd/" and "/kd") so a CLI hitting the root of the upstream resolves.
		mux.HandleFunc(up.prefix, s.handleProxy)
		mux.HandleFunc(strings.TrimSuffix(up.prefix, "/"), s.handleProxy)
	}
}

func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"status":"ok","version":%q}`, s.version)
}

func (s *server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if ok, reason := s.ready(); !ok {
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"status":"not_ready","reason":%q}`, reason)
		return
	}
	fmt.Fprint(w, `{"status":"ok"}`)
}

// handleAuthCheck validates the Authorization: Bearer PAT and returns the
// resolved operator identity. CLI clients can call this to verify their
// token before issuing a real request. W-B-9 wraps the same validator into
// a reverse-proxy middleware; the check endpoint stays as a self-test.
// Every outcome (success + failure) emits an OCSF Access Activity record
// via s.audit for downstream CloudWatch / Mezmo ingestion.
func (s *server) handleAuthCheck(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	auditID := newAuditID(s.now())
	rec := s.baseAuditRecord(r, auditEndpointAuthCheck, "", auditID)

	if s.validator == nil {
		s.failAudit(rec, auditDetailDenyValidatorOff)
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprint(w, `{"error":"validator_not_configured"}`)
		return
	}

	pat := extractBearerToken(r)
	if pat == "" {
		s.failAudit(rec, auditDetailDenyMissingToken)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"missing_token"}`)
		return
	}

	id, err := s.validator.Validate(r.Context(), pat)
	switch {
	case err == nil:
		s.okAudit(rec, id)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status":   "ok",
			"user_id":  id.UserID,
			"username": id.Username,
			"email":    id.Email,
			"name":     id.Name,
		})
	case errors.Is(err, edgeauth.ErrInvalidToken):
		s.failAudit(rec, auditDetailDenyInvalidPAT)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"invalid_token"}`)
	case errors.Is(err, edgeauth.ErrNotInGroup):
		// Validate returns the partial identity from /user on ErrNotInGroup
		// so the OCSF deny record attributes the request to the actor.
		s.failAuditWithActor(rec, auditDetailDenyNotInGroup, id)
		s.metrics.record403(auditEndpointAuthCheck, auditDetailDenyNotInGroup)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"error":"not_in_group"}`)
	case errors.Is(err, edgeauth.ErrEmptyToken):
		s.failAudit(rec, auditDetailDenyMissingToken)
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":"missing_token"}`)
	default:
		s.logger.Warn("edge-auth: gitlab validation failed", "error", err)
		s.failAudit(rec, auditDetailDenyGitLabDown)
		w.WriteHeader(http.StatusBadGateway)
		fmt.Fprint(w, `{"error":"gitlab_unavailable"}`)
	}
}

// extractBearerToken pulls the PAT from a standard Authorization header.
// Both "Bearer <pat>" and "Token <pat>" (some GitLab CLI flows) are
// accepted; case-insensitive scheme matching matches RFC 7235.
type authorizationCredential struct {
	scheme string
	token  string
}

func authorizationCredentialFromRequest(r *http.Request) authorizationCredential {
	h := r.Header.Get("Authorization")
	if h == "" {
		return authorizationCredential{}
	}
	scheme, token, ok := strings.Cut(h, " ")
	if !ok {
		return authorizationCredential{}
	}
	scheme = strings.ToLower(scheme)
	switch scheme {
	case "bearer", "token":
		return authorizationCredential{scheme: scheme, token: strings.TrimSpace(token)}
	}
	return authorizationCredential{}
}

func extractBearerToken(r *http.Request) string {
	return authorizationCredentialFromRequest(r).token
}
