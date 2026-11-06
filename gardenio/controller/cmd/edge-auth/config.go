package main

import (
	"slices"
	"strings"
)

const (
	oauth2ProxyRoleSourceProviderGroups = "provider-groups"
	oauth2ProxyRoleSourceEntraAppRoles  = "entra-app-roles"
)

// envFlag returns true when the env var is set to a recognised true value.
// Empty / unset / unrecognised values are treated as false so the flag is
// off by default and operators must opt in explicitly.
func envFlag(key string) bool {
	switch strings.ToLower(strings.TrimSpace(envOrDefault(key, ""))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// config holds the runtime configuration for the edge-auth service.
//
// Only the scaffold fields are consumed today. Fields prefixed with a
// "Future:" comment are placeholders for the follow-up tasks under epic
// ka-0000003384 and are wired to env vars now so the deployment manifest
// shape can stabilise alongside the chart work (W-B-12).
type config struct {
	// listenAddr is the bind address for the public HTTP server.
	listenAddr string

	// logLevel is one of "debug", "info", "warn", "error".
	logLevel string

	// Future (W-B-8): GitLab API base URL for PAT validation.
	gitlabBaseURL string

	// Future (W-B-8): name of the GitLab group whose members are authorised
	// to drive the CLI surface (e.g. "gardenio").
	gitlabAuthGroup string

	// patDisabled turns the GitLab-PAT auth path off entirely (ka-0000006087,
	// box auth profile §6). When true the GitLab validator is NOT built — the
	// shim makes no gitlab.com API calls at all, so a PAT minted from stale
	// org membership cannot authenticate even on a box with internet egress —
	// and the readiness gate stops requiring the validator, provided another
	// auth mode (cookie or AVA) is configured. PAT-bearing requests are then
	// rejected by the nil-validator guards already present on every path. The
	// air-gapped box uses this with cookieAuth on as the sole identity source:
	// the in-cluster Keycloak via oauth2-proxy. Defaults false → the PAT edge
	// behaves exactly as before.
	patDisabled bool

	// Future (W-B-9): HMAC key used to sign the X-Edge-Auth-Sig header that
	// upstream daemons verify before trusting X-Operator. Empty in the
	// scaffold; the wiring task asserts non-empty at startup.
	hmacKey string

	// hmacKeyV2 signs the v2 (role-bearing) envelope (EDGE_AUTH_HMAC_KEY_V2,
	// DAEMON_HMAC_KEY_V2 on the daemon). Split from hmacKey so a holder of
	// the v1 key alone cannot mint a v2 admin envelope (gd-dMof1PW1oJ).
	// Empty falls back to hmacKey until the second secret is wired.
	hmacKeyV2 string

	// Future (W-B-9): per-upstream Service URLs the proxy will dispatch to
	// based on path prefix (/kd -> kbeads, /mux -> coopmux, ...).
	upstreamKbeads  string
	upstreamCoopmux string

	// upstreamNats is the NATS WebSocket Service URL (e.g.
	// http://gardenio-nats:8443). It is dispatched on the /kd/nats/ prefix,
	// which MUST be registered before the /kd/ kbeads prefix so the
	// first-match router (matchUpstream) sends the realtime channel to NATS
	// instead of the daemon. Without it, `gb agent start --local` 502s: the
	// local nats-bridge dials wss://<host>/kd/nats, which falls through to the
	// kbeads daemon (no WS handler) and never gets its 101 (ka-0000006000).
	// Empty -> the NATS route is not registered and /kd/nats falls through to
	// kbeads as before.
	upstreamNats string

	// dryRun is the Stage 3 / Safe-deploy kill switch (ka-0000002544).
	// When true, the proxy still validates the bearer PAT + group membership
	// and emits the same audit log line, but it short-circuits the upstream
	// request and returns a synthetic 200 with X-Edge-Auth-Dry-Run: true so
	// CLI clients can smoke the auth path without risking real traffic.
	// Defaulted false; the chart flag (edgeAuth.dryRun) is off until the
	// rollout sequence explicitly opts in.
	dryRun bool

	// avaTrustSigner is the AWS Verified Access instance ARN whose signed
	// X-Amzn-Ava-User-Context JWTs we trust, e.g.
	// "arn:aws:ec2:us-east-1:234567890123:verified-access-instance/vai-…".
	// When empty (default) the AVA mode is OFF and the shim behaves exactly
	// as the PAT-only edge it has always been. When set, requests carrying
	// the AVA header are routed through ES384 JWT verification against the
	// pinned signer and the resulting email is stamped into the same HMAC
	// envelope (X-Operator / X-Auth-Audit-Id / X-Edge-Auth-Sig / X-Edge-
	// Auth-Ts) that PAT-mode produces — keeping the daemon-side validator
	// (kbeads internal/edgeauth/verify.go) trusting a single envelope shape
	// regardless of which edge the request crossed. Implements epic
	// ka-0000005862 / decision ka-0000001678.
	avaTrustSigner string

	// avaRegion is the AWS region used to build the AVA public-keys URL
	// (https://public-keys.prod.verified-access.<region>.amazonaws.com/<kid>).
	// Empty defaults to "us-east-1" inside the verifier. Only consulted when
	// avaTrustSigner is set; ignored otherwise.
	avaRegion string

	// entraIssuerURL enables the Microsoft Entra delegated-access-token path.
	// Bearer JWTs whose unverified iss exactly matches this value are claimed by
	// the Entra verifier and never fall through to GitLab PAT validation.
	entraIssuerURL string

	// entraAudience is the Gardenio API application client ID expected in the
	// access token aud claim. Entra v2 access tokens use the resource API's
	// client ID as the audience even when callers request an Application ID URI
	// scope.
	entraAudience string

	// entraTenantID is the immutable Acme tenant ID expected in tid. The
	// issuer also pins the tenant; checking both prevents an accidental
	// common/organizations issuer from widening the trust boundary later.
	entraTenantID string

	// entraRequiredScope is the exact delegated scope value required in the
	// token's space-delimited scp claim.
	entraRequiredScope string

	// entraRequiredRole is the minimum Gardenio app role required by the shared
	// role hierarchy. Production uses gardenio-viewer so viewer, operator, and
	// admin tokens all satisfy the edge authentication floor.
	entraRequiredRole string

	// entraAllowedClientIDs pins the v2 azp claim to explicitly authorized
	// public clients (PiID today). Empty is invalid when the Entra path is on.
	entraAllowedClientIDs []string

	// oauth2ProxyAuthURL is the in-cluster URL of the oauth2-proxy auth
	// subrequest endpoint (e.g. http://gardenio-forwardauth:4180/oauth2/auth)
	// used to validate the gb-login browser-session cookie (ka-0000000080).
	// Empty (default) = cookie mode OFF; the shim authenticates the PAT and
	// AVA edges only, exactly as before. When set, a proxy request that
	// carries the session cookie but no Bearer PAT / AVA header is validated
	// by forwarding its Cookie header to oauth2-proxy; on a 2xx the operator
	// email comes from oauth2-proxy's X-Auth-Request-Email RESPONSE header
	// (derived from the validated session, never a client-supplied value) and
	// is stamped into the same HMAC envelope the PAT / AVA paths produce.
	// This lets `gb login` (which stores the oauth2-proxy cookie) drive
	// gardenio.app/kd with no PAT, keeping a single cryptographic trust
	// boundary at the shim — the daemon is unchanged.
	oauth2ProxyAuthURL string

	// oauth2ProxyCookieName gates whether cookie auth is even attempted: the
	// shim only makes the oauth2-proxy subrequest when a cookie of this name
	// is present on the inbound request, so PAT / AVA callers never pay for an
	// extra network hop. Defaults to oauth2-proxy's gardenio session cookie.
	oauth2ProxyCookieName string

	// oauth2ProxyRoleSource identifies the authenticated claim contract behind
	// X-Auth-Request-Groups. The default, provider-groups, accepts only the
	// approved full Gardenio group paths. entra-app-roles is an explicit opt-in
	// for an oauth2-proxy configured to expose the Entra roles claim, where the
	// exact bare gardenio-* app-role values are authoritative.
	oauth2ProxyRoleSource string

	// allowedEmailDomains is an optional email-domain allowlist applied to the
	// resolved operator identity AFTER the primary group-membership gate, as
	// defense-in-depth (ka-0000000533). Empty (the default) means the gate is
	// OFF — group membership is the sole authority, matching the historical
	// behaviour of this shim. When non-empty, a resolved identity whose email
	// domain is not in the list is denied with deny:email-domain regardless of
	// auth mode (PAT / AVA / cookie). The web front door (oauth2-proxy) already
	// enforces an equivalent --email-domain allowlist; this brings the CLI/PAT
	// edge to parity when an operator opts in. Parsed from the comma-separated
	// EDGE_AUTH_ALLOWED_EMAIL_DOMAINS env var; case-insensitive.
	allowedEmailDomains []string
}

// emailDomainAllowed reports whether email's domain is in the allowlist.
// An empty allowlist means the gate is off → always allowed. A malformed
// email (no "@") is denied when the gate is on, since a verified identity
// must carry a usable domain to be matched.
func emailDomainAllowed(email string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	_, domain, ok := strings.Cut(email, "@")
	if !ok {
		return false
	}
	domain = strings.ToLower(domain)
	// Compare case-insensitively against each entry. parseCommaSeparated
	// already lower-cases entries that come from the env var, but lower-case
	// here too so the helper is correct for any caller-supplied allowlist.
	return slices.ContainsFunc(allowed, func(d string) bool {
		return strings.EqualFold(d, domain)
	})
}

// parseCommaSeparated splits a comma-separated env value into a trimmed,
// lower-cased, non-empty slice. Returns nil for an empty/unset value so the
// caller can treat "feature off" as a nil slice.
func parseCommaSeparated(v string) []string {
	var out []string
	for part := range strings.SplitSeq(v, ",") {
		part = strings.ToLower(strings.TrimSpace(part))
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseConfig() *config {
	return &config{
		listenAddr:            envOrDefault("LISTEN_ADDR", ":8096"),
		logLevel:              envOrDefault("LOG_LEVEL", "info"),
		gitlabBaseURL:         strings.TrimRight(envOrDefault("GITLAB_BASE_URL", "https://gitlab.com"), "/"),
		gitlabAuthGroup:       envOrDefault("EDGE_AUTH_GITLAB_GROUP", "gardenio"),
		patDisabled:           envFlag("EDGE_AUTH_PAT_DISABLED"),
		hmacKey:               envOrDefault("EDGE_AUTH_HMAC_KEY", ""),
		hmacKeyV2:             envOrDefault("EDGE_AUTH_HMAC_KEY_V2", ""),
		upstreamKbeads:        strings.TrimRight(envOrDefault("EDGE_AUTH_UPSTREAM_KBEADS", ""), "/"),
		upstreamCoopmux:       strings.TrimRight(envOrDefault("EDGE_AUTH_UPSTREAM_COOPMUX", ""), "/"),
		upstreamNats:          strings.TrimRight(envOrDefault("EDGE_AUTH_UPSTREAM_NATS", ""), "/"),
		dryRun:                envFlag("EDGE_AUTH_DRY_RUN"),
		avaTrustSigner:        strings.TrimSpace(envOrDefault("EDGE_AUTH_AVA_TRUST_SIGNER", "")),
		avaRegion:             strings.TrimSpace(envOrDefault("EDGE_AUTH_AVA_REGION", "")),
		entraIssuerURL:        strings.TrimSpace(envOrDefault("EDGE_AUTH_ENTRA_ISSUER_URL", "")),
		entraAudience:         strings.TrimSpace(envOrDefault("EDGE_AUTH_ENTRA_AUDIENCE", "")),
		entraTenantID:         strings.TrimSpace(envOrDefault("EDGE_AUTH_ENTRA_TENANT_ID", "")),
		entraRequiredScope:    strings.TrimSpace(envOrDefault("EDGE_AUTH_ENTRA_REQUIRED_SCOPE", "")),
		entraRequiredRole:     strings.TrimSpace(envOrDefault("EDGE_AUTH_ENTRA_REQUIRED_ROLE", "")),
		entraAllowedClientIDs: parseCommaSeparated(envOrDefault("EDGE_AUTH_ENTRA_ALLOWED_CLIENT_IDS", "")),

		oauth2ProxyAuthURL:    strings.TrimSpace(envOrDefault("EDGE_AUTH_OAUTH2_PROXY_AUTH_URL", "")),
		oauth2ProxyCookieName: envOrDefault("EDGE_AUTH_OAUTH2_PROXY_COOKIE_NAME", "_gardenio_oauth2"),
		oauth2ProxyRoleSource: strings.ToLower(strings.TrimSpace(envOrDefault(
			"EDGE_AUTH_OAUTH2_PROXY_ROLE_SOURCE",
			oauth2ProxyRoleSourceProviderGroups,
		))),
		allowedEmailDomains: parseCommaSeparated(envOrDefault("EDGE_AUTH_ALLOWED_EMAIL_DOMAINS", "")),
	}
}

// v2HMACKey is the key the v2 envelope is signed with: hmacKeyV2 when set,
// else the shared hmacKey.
func (c *config) v2HMACKey() string {
	if c.hmacKeyV2 != "" {
		return c.hmacKeyV2
	}
	return c.hmacKey
}
