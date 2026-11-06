package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"gardenio/controller/internal/edgeauth"
)

// OCSF Access Activity status_id values.
//
// Reference: OCSF v1.1 Access Activity (uid 3006), status_id enum.
//
//	1 = Success, 2 = Failure, 99 = Other (unused here).
const (
	ocsfStatusSuccess = 1
	ocsfStatusFailure = 2
)

// ocsfClassAccessActivity is the OCSF event class for authn / authz
// outcomes plus access-control proxying. Daemon-side and Mezmo dashboards
// filter on this string to find edge-auth audit lines.
const ocsfClassAccessActivity = "Access Activity: Access"

// status_detail strings emitted by the shim. The set is fixed so Mezmo
// dashboards can pivot on it; new entries should be added here first.
const (
	auditDetailOK               = "ok"
	auditDetailDenyInvalidPAT   = "deny:invalid-pat"
	auditDetailDenyMissingToken = "deny:missing-token"
	auditDetailDenyNotInGroup   = "deny:not-in-gardenio"
	// auditDetailDenyEmailDomain is emitted when the resolved operator
	// identity passed the group gate but its email domain is not in the
	// optional EDGE_AUTH_ALLOWED_EMAIL_DOMAINS allowlist (ka-0000000533).
	// Off by default (empty allowlist); only fires when an operator opts in.
	auditDetailDenyEmailDomain   = "deny:email-domain"
	auditDetailDenyUnknownPrefix = "deny:unknown-upstream"
	auditDetailDenyValidatorOff  = "deny:validator-not-configured"
	auditDetailDenyProxyOff      = "deny:proxy-not-configured"
	auditDetailDenyGitLabDown    = "deny:gitlab-unavailable"
	// auditDetailDenyInvalidAVA is emitted when an X-Amzn-Ava-User-Context
	// header was present but the AVA verifier rejected the JWT (bad
	// signature, expired, signer-mismatch, missing email claim, …). Distinct
	// from invalid-pat so dashboards can split AVA-edge failures from PAT-
	// edge failures.
	auditDetailDenyInvalidAVA = "deny:invalid-ava"
	// auditDetailDenyMissingAVAHeader is the forwardauth-endpoint analogue
	// of deny:missing-token. Distinct from invalid-ava so dashboards can
	// distinguish a probe hitting the forwardauth endpoint with no AVA
	// header from a tampered JWT.
	auditDetailDenyMissingAVAHeader = "deny:missing-ava-header"
	// auditDetailDenyForwardauthOff is emitted by handleForwardauth when
	// the chart did not configure avaTrustSigner. Distinct status_detail
	// so dashboards spot a Traefik IngressRoute pointing at an unprepared
	// shim before users notice 404s.
	auditDetailDenyForwardauthOff = "deny:forwardauth-not-configured"
	// auditDetailDenyVerifierUnavailable is emitted when the AVA verifier
	// returns an error that is neither "missing header" nor "invalid
	// token" — typically a transient JWKS-fetch failure. Maps to 502 to
	// signal "retryable" semantics to the caller.
	auditDetailDenyVerifierUnavailable = "deny:verifier-unavailable"
	// auditDetailDenyInvalidCookie is emitted when a gb-login browser-session
	// cookie was present but oauth2-proxy rejected it (expired / forged /
	// signed-out session). Distinct from invalid-pat / invalid-ava so
	// dashboards can split cookie-edge failures from the other two edges.
	auditDetailDenyInvalidCookie = "deny:invalid-cookie"
	// auditDetailDenyCookieUpstream is emitted when the oauth2-proxy
	// /oauth2/auth subrequest could not be reached or returned an
	// unexpected status. Maps to 502 (retryable), mirroring
	// deny:verifier-unavailable on the AVA edge.
	auditDetailDenyCookieUpstream = "deny:cookie-upstream-unavailable"
	// Entra delegated-token outcomes stay distinct from PAT and AVA so audit
	// dashboards can separate bad JWTs, authorization failures, and transient
	// discovery/JWKS outages.
	auditDetailDenyInvalidEntra        = "deny:invalid-entra"
	auditDetailDenyEntraForbidden      = "deny:entra-forbidden"
	auditDetailDenyEntraUnavailable    = "deny:entra-unavailable"
	auditDetailDenyEntraForwardauthOff = "deny:entra-forwardauth-not-configured"
)

// auditAuthMode distinguishes which edge an authenticated request crossed.
// "pat" is the GitLab-PAT path (status quo); "ava" is the AWS Verified Access
// path introduced under epic ka-0000005862. Empty for failure records that
// could not be attributed to either path (e.g. validator misconfigured before
// any auth decision was made).
const (
	auditAuthModePAT = "pat"
	auditAuthModeAVA = "ava"
	// auditAuthModeCookie is the gb-login browser-session-cookie path
	// (ka-0000000080): the shim validates the oauth2-proxy session cookie via
	// an /oauth2/auth subrequest and stamps the same HMAC envelope the PAT /
	// AVA edges produce, so the daemon trusts one envelope shape regardless of
	// which edge authenticated the caller.
	auditAuthModeCookie = "cookie"
	auditAuthModeEntra  = "entra"
)

// auditEndpointKind is the metadata.endpoint value distinguishing the
// /auth/check self-test from the reverse-proxy surface.
const (
	auditEndpointAuthCheck = "auth_check"
	auditEndpointProxy     = "proxy"
	// auditEndpointForwardauth is the Traefik forwardAuth-compatible
	// endpoint (handleForwardauth). Distinct from "proxy" so OCSF
	// dashboards can split coopmux/AVA flow from kbeads CLI flow even
	// though both ultimately validate via avaVerifier. No upstream
	// service is associated with this endpoint — the shim only verifies
	// the JWT and stamps response headers; Traefik forwards on success.
	auditEndpointForwardauth      = "forwardauth"
	auditEndpointEntraForwardauth = "entra_forwardauth"
)

// auditRecord is the OCSF Access Activity JSON the shim emits per
// authn outcome and per proxied request. See
// gardenio/plans/stage3-ava-cli-eb-rollout.md §"Audit log schema (OCSF
// Access Activity)". One JSON object per line so CloudWatch / Mezmo
// agents can ingest it as a structured log stream.
type auditRecord struct {
	Class        string         `json:"class"`
	Time         string         `json:"time"`
	Actor        *auditActor    `json:"actor"`
	SrcEndpoint  *auditEndpoint `json:"src_endpoint,omitempty"`
	HTTPRequest  auditRequest   `json:"http_request"`
	Metadata     auditMetadata  `json:"metadata"`
	StatusID     int            `json:"status_id"`
	StatusDetail string         `json:"status_detail"`
}

type auditActor struct {
	User auditUser `json:"user"`
}

type auditUser struct {
	EmailAddr string `json:"email_addr,omitempty"`
	Name      string `json:"name,omitempty"`
}

type auditEndpoint struct {
	IP string `json:"ip,omitempty"`
}

type auditRequest struct {
	URL    auditURL `json:"url"`
	Method string   `json:"http_method"`
}

type auditURL struct {
	Path string `json:"path"`
}

type auditMetadata struct {
	AuditID         string `json:"audit_id"`
	UpstreamService string `json:"upstream_service,omitempty"`
	ShimVersion     string `json:"shim_version"`
	Endpoint        string `json:"endpoint,omitempty"`
	DryRun          bool   `json:"dry_run,omitempty"`
	// AuthMode is "pat" or "ava" depending on which edge authenticated the
	// request. Surfaced as a metadata field (rather than reshuffling the
	// top-level record) so existing OCSF consumers parse unchanged — they
	// just gain a new optional pivot. Empty for failure records that did
	// not reach an auth decision.
	AuthMode string `json:"auth_mode,omitempty"`
}

// auditSink writes one OCSF JSON record per line to its underlying writer.
// Writes are serialised under a mutex so concurrent handlers do not
// interleave records.
type auditSink struct {
	mu  sync.Mutex
	w   io.Writer
	now func() time.Time
}

func newAuditSink(w io.Writer, now func() time.Time) *auditSink {
	if w == nil {
		w = os.Stderr
	}
	if now == nil {
		now = time.Now
	}
	return &auditSink{w: w, now: now}
}

func (a *auditSink) emit(rec auditRecord) {
	if a == nil {
		return
	}
	if rec.Time == "" {
		rec.Time = a.now().UTC().Format(time.RFC3339Nano)
	}
	if rec.Class == "" {
		rec.Class = ocsfClassAccessActivity
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	_, _ = a.w.Write(b)
	_, _ = a.w.Write([]byte{'\n'})
}

// baseAuditRecord returns a partially-populated record ready for one of
// the helpers below to flesh out with a status_id + status_detail. The
// upstream argument is the audit-log label of the matched proxyUpstream
// (e.g. "kbeads") or "" for the /auth/check self-test.
func (s *server) baseAuditRecord(r *http.Request, endpoint, upstream, auditID string) auditRecord {
	rec := auditRecord{
		HTTPRequest: auditRequest{
			URL:    auditURL{Path: r.URL.Path},
			Method: r.Method,
		},
		Metadata: auditMetadata{
			AuditID:         auditID,
			UpstreamService: upstream,
			ShimVersion:     s.version,
			Endpoint:        endpoint,
		},
	}
	if ip := remoteAddrIP(r); ip != "" {
		rec.SrcEndpoint = &auditEndpoint{IP: ip}
	}
	return rec
}

// okAudit emits a success record. The actor is populated from the
// validated GitLab identity.
func (s *server) okAudit(rec auditRecord, id *edgeauth.Identity) {
	if id != nil {
		rec.Actor = &auditActor{User: auditUser{EmailAddr: id.Email, Name: id.Name}}
	}
	rec.StatusID = ocsfStatusSuccess
	rec.StatusDetail = auditDetailOK
	s.audit.emit(rec)
}

// okAuditDryRun emits a success record flagged as dry-run. Used when the
// chart-level edgeAuth.dryRun flag short-circuits the upstream call.
func (s *server) okAuditDryRun(rec auditRecord, id *edgeauth.Identity) {
	if id != nil {
		rec.Actor = &auditActor{User: auditUser{EmailAddr: id.Email, Name: id.Name}}
	}
	rec.Metadata.DryRun = true
	rec.StatusID = ocsfStatusSuccess
	rec.StatusDetail = auditDetailOK
	s.audit.emit(rec)
}

// failAudit emits a failure record without an actor. Used when the PAT
// could not be resolved (missing, invalid, validator down, upstream down).
// Mirrors the AVA OCSF deny semantic where actor=null on PAT-invalid.
func (s *server) failAudit(rec auditRecord, detail string) {
	rec.StatusID = ocsfStatusFailure
	rec.StatusDetail = detail
	s.audit.emit(rec)
}

// failAuditWithActor emits a failure record carrying the resolved actor.
// Used for deny:not-in-gardenio where the PAT validated but group
// membership failed, so we know who tried.
func (s *server) failAuditWithActor(rec auditRecord, detail string, id *edgeauth.Identity) {
	if id != nil {
		rec.Actor = &auditActor{User: auditUser{EmailAddr: id.Email, Name: id.Name}}
	}
	rec.StatusID = ocsfStatusFailure
	rec.StatusDetail = detail
	s.audit.emit(rec)
}

// remoteAddrIP extracts the client IP from a request's RemoteAddr. The Go
// stdlib stores it as "host:port"; we drop the port for the OCSF
// src_endpoint.ip field.
func remoteAddrIP(r *http.Request) string {
	if r == nil || r.RemoteAddr == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
