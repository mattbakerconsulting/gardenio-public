package main

// grafana_proxy.go — the AVA-authenticated reverse proxy that fronts the
// in-cluster Grafana under the viewer's own /grafana subpath (Option C,
// decision ka-0000004242 "viewer_proxy"; epic ka-0000000746).
//
// WHY THIS EXISTS. On the new prod stack there is no Traefik unified-host edge
// (the migration removed it): every browsable surface sits behind AWS Verified
// Access → an internal NLB → straight to the pod, and each pod verifies the AVA
// token itself. beads-viewer does that natively (oidcauth.authBranch injects the
// authenticated email into the request context). Grafana is a third-party app
// that cannot verify an AVA JWT; on new prod nothing hands it an identity
// header, so it was reachable only by port-forward. Rather than re-introduce the
// Traefik + edge-auth forwardAuth layer we just tore out (Option A) or give
// Grafana its own cross-origin AVA endpoint that breaks the in-app dashboard
// embeds (Option B), we extend the ALREADY-AVA-authenticated viewer origin to
// reverse-proxy Grafana too — same host, same session, same-origin /grafana.
//
// HOW IT AUTHENTICATES TO GRAFANA. Grafana runs with its header auth-proxy on
// (GF_AUTH_PROXY_ENABLED). On every forwarded request this proxy stamps the
// authenticated viewer email into the auth-proxy header (GRAFANA_AUTH_PROXY_HEADER,
// default X-WEBAUTH-USER); Grafana trusts that header as the signed-in user and
// shows no second login. This mirrors what the legacy Traefik forwardAuth
// middleware did, and what cmd/edge-auth/proxy.go does for kbeads/coopmux — the
// identity header is the single source of truth and any client-supplied copy is
// stripped before forwarding so a browser can never smuggle a forged identity
// past this hop.
//
// SUBPATH. Grafana is configured with serve_from_sub_path=true and
// root_url=https://<viewer-host>/grafana, so it OWNS the /grafana prefix
// end-to-end: it emits its own asset/API links already carrying /grafana and
// expects inbound requests to keep the prefix. We therefore forward the request
// path VERBATIM (no strip) — the opposite of the edge-auth shim, whose upstreams
// serve at root after a strip. Keeping the prefix is what makes the SPA's
// <base href>=/grafana/ resolve /grafana/public/build/*.js against the pod
// instead of 404-ing (the class of bug ka-0000001440).
//
// DEGRADED MODE. When no upstream is configured (local dev / tests, or an env
// that has not opted in) the handler returns 503 so the misconfiguration is
// visible rather than surfacing as a blank Grafana. Wiring is opt-in from main
// via SetGrafanaProxy, driven by GRAFANA_UPSTREAM_URL.

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"

	"gardenio/controller/internal/oidcauth"
)

// defaultGrafanaAuthProxyHeader is the header the proxy stamps the identity
// into when GRAFANA_AUTH_PROXY_HEADER is unset. X-WEBAUTH-USER is Grafana's
// canonical auth-proxy header name. The chart passes grafana.auth.proxy.headerName
// (default X-Auth-Request-Email) as GRAFANA_AUTH_PROXY_HEADER whenever
// auth.proxy is on, so the stamp always matches GF_AUTH_PROXY_HEADER_NAME; a
// mismatch makes Grafana ignore the stamp and fall back to anonymous or its
// login form (gd-lHJot24kJu).
const defaultGrafanaAuthProxyHeader = "X-WEBAUTH-USER"

// grafanaProxyStripHeaders are inbound headers a client might send to
// impersonate a user or to reach Grafana's auth-proxy directly. We delete every
// one (plus the configured auth-proxy header, see handleGrafanaProxy) before
// forwarding so the ONLY identity Grafana sees is the one this proxy stamps
// from the AVA-verified session. Grafana's own GF_AUTH_PROXY_WHITELIST is a
// second line of defence (it only trusts the viewer pod's source IP), but
// stripping here means a forged header never even leaves the viewer.
var grafanaProxyStripHeaders = []string{
	defaultGrafanaAuthProxyHeader,
	"X-Auth-Request-Email",
	"X-Auth-Request-User",
	"X-Auth-Request-Groups",
	"X-Grafana-User",
	"X-WEBAUTH-GROUPS",
}

// SetGrafanaAuthProxyHeader sets the header the /grafana proxy stamps the
// verified email into. It must equal Grafana's GF_AUTH_PROXY_HEADER_NAME. A
// blank name keeps defaultGrafanaAuthProxyHeader. Called once from main.
func (s *Server) SetGrafanaAuthProxyHeader(name string) {
	s.grafanaAuthHeader = strings.TrimSpace(name)
}

// grafanaAuthProxyHeader returns the configured auth-proxy header name, or
// defaultGrafanaAuthProxyHeader when none is set.
func (s *Server) grafanaAuthProxyHeader() string {
	if s.grafanaAuthHeader == "" {
		return defaultGrafanaAuthProxyHeader
	}
	return s.grafanaAuthHeader
}

// SetGrafanaProxy wires the upstream in-cluster Grafana base URL used by the
// /grafana reverse proxy. An empty/blank upstream leaves the handler in
// degraded mode (503). Called once from main; idempotent (last write wins).
//
// The upstream is the ROOT Grafana Service URL (e.g. http://gardenio-grafana:3000
// — no /grafana suffix): Grafana serves the subpath itself when
// serve_from_sub_path is on, so we forward the client's /grafana-prefixed path
// unchanged to the Service root. A trailing slash on the configured base is
// trimmed so path joining stays predictable.
func (s *Server) SetGrafanaProxy(upstreamBaseURL string, logger *slog.Logger) {
	base := strings.TrimRight(strings.TrimSpace(upstreamBaseURL), "/")
	if base == "" {
		s.grafanaProxy = nil
		s.grafanaUpstream = ""
		return
	}
	target, err := url.Parse(base)
	if err != nil || target.Scheme == "" || target.Host == "" {
		// A malformed GRAFANA_UPSTREAM_URL is an operator misconfiguration; log
		// it and stay in degraded mode rather than panicking a request later.
		logger.Error("grafana proxy disabled — invalid GRAFANA_UPSTREAM_URL", "value", upstreamBaseURL, "error", err)
		s.grafanaProxy = nil
		s.grafanaUpstream = ""
		return
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// Point at the Grafana Service, preserving the FULL inbound path
			// (Grafana owns the /grafana prefix via serve_from_sub_path). SetURL
			// joins target's base path with pr.Out.URL.Path; target has no path,
			// so the request path — /grafana/... — is forwarded verbatim.
			pr.SetURL(target)
			pr.Out.Host = target.Host
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			logger.Warn("grafana proxy upstream error", "path", r.URL.Path, "error", err)
			http.Error(w, "grafana upstream unavailable", http.StatusBadGateway)
		},
	}
	s.grafanaProxy = rp
	s.grafanaUpstream = base
}

// handleGrafanaProxy forwards a request to the in-cluster Grafana, stamping the
// AVA-authenticated viewer identity into Grafana's auth-proxy header. It is
// mounted (server.go) at /grafana and /grafana/{path...} so every Grafana asset,
// API, and Live WebSocket route under the subpath reaches the pod through the
// already-authenticated viewer origin.
//
// Identity: oidcauth.EmailFromContext reads the AVA/Keycloak-verified email that
// the auth middleware injected into the request context (the dev-bypass path
// promotes the trusted forwardauth-stamped header into that same slot). If it is
// empty the request never authenticated — we refuse rather than hand Grafana an
// anonymous or spoofable identity.
func (s *Server) handleGrafanaProxy(w http.ResponseWriter, r *http.Request) {
	if s.grafanaProxy == nil {
		// No upstream wired (local dev / tests / env not opted in). Be explicit
		// so the misconfiguration is visible rather than a blank Grafana.
		http.Error(w, "grafana proxy unavailable", http.StatusServiceUnavailable)
		return
	}

	// Resolve the identity from the VERIFIED request context, NOT from
	// oidcauth.EmailFromRequest — that helper reads the X-Auth-Request-Email
	// header first, and on the new-prod AVA edge that header is client-supplied
	// (AVA stamps its identity in X-Amzn-Ava-User-Context and injects the
	// verified email into the request context; it does NOT strip a forged
	// X-Auth-Request-Email). Trusting the header would let a browser send
	// `X-Auth-Request-Email: someone-else` and log into Grafana as that user.
	// EmailFromContext returns only what the auth middleware verified: the AVA
	// authBranch and the Keycloak verifyingMiddleware both populate ctxEmail
	// from a validated credential, so it is the single trustworthy source.
	email := strings.TrimSpace(oidcauth.EmailFromContext(r.Context()))
	if email == "" {
		// Auth middleware admitted the request but attached no verified
		// identity (e.g. a header-only spoof attempt, or the AVA-only chain
		// passing an AVA-headerless request through). Fail closed rather than
		// proxy an unauthenticated or spoofable user into Grafana.
		http.Error(w, "unauthenticated", http.StatusUnauthorized)
		return
	}

	// Strip any client-supplied identity/auth-proxy headers, THEN stamp the
	// verified email. Order matters: a client could otherwise send both a forged
	// auth-proxy header and rely on us not overwriting it. Del-then-Set guarantees
	// the outbound header carries exactly one value — ours.
	header := s.grafanaAuthProxyHeader()
	for _, h := range grafanaProxyStripHeaders {
		r.Header.Del(h)
	}
	r.Header.Del(header)
	r.Header.Set(header, email)

	s.grafanaProxy.ServeHTTP(w, r)
}
