package edgeauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// forwardauth_provenance.go — the forwardauth identity-provenance envelope
// (task ka-0000004151).
//
// PROBLEM. A surface served behind the edge-auth forwardauth middleware
// (cmd/edge-auth/forwardauth.go -> /forwardauth/verify) reads its caller
// identity from the X-Auth-Request-{Email,Groups} headers Traefik injects from
// the auth response. That is trustworthy on the BROWSER path only because
// Traefik's forwardAuth OVERWRITES those headers with the verified values,
// discarding anything the client sent. It is NOT trustworthy on a path that
// does not transit Traefik: an in-cluster caller that reaches the pod's Service
// directly can forge X-Auth-Request-Email and name itself as any identity — for
// the scif approval portal that defeats four-eyes from inside the cluster.
//
// This is the SAME class of gap the daemon shim closed with the X-Operator HMAC
// envelope (verify.go), but the forwardauth path never produced an envelope:
// handleForwardauth emits only X-Auth-Request-* + X-Auth-Audit-Id. This file
// adds a purpose-built provenance envelope for that path — edge-auth signs the
// identity it just verified, and the upstream verifies the signature before
// trusting the identity headers.
//
// WHY NOT REUSE the daemon envelope (VerifyConfig / signEdgeAuth). That envelope
// binds the UPSTREAM request path into the HMAC, which the daemon shim knows
// because it is a reverse proxy. The forwardauth endpoint is NOT a proxy: it
// sees Traefik's auth-probe path, not the eventual upstream path, and the scif
// route additionally rides a StripPrefix so the path the pod sees differs again.
// Path-binding here would be fragile and wrong. This envelope instead binds the
// IDENTITY ASSERTION itself (email, groups, audit-id, ts) — "edge-auth vouched
// for THIS identity at THIS time" — which is exactly what the upstream needs to
// know and is stable across StripPrefix. The distinct "fwdauth-v1" message
// prefix domain-separates it from the daemon envelope even though both ride an
// X-Edge-Auth-* header, so a signature from one scheme can never verify in the
// other.
//
// BURN-IN. Mirrors the daemonHmacVerify.enforce rollout (ka-0000002544): with
// Enforce=false the verifier computes the outcome so the caller can log + count,
// but still trusts the identity (the pre-envelope behaviour, unchanged). Flip
// Enforce=true per-env only after edge-auth is signing and a clean burn-in shows
// no unexpected non-ok outcomes; then a request whose identity did not arrive
// with a valid envelope is refused.

// ForwardAuthHeader* names the request/response headers involved in the
// forwardauth provenance envelope. The identity headers are the oauth2-proxy
// shape edge-auth already emits; the Sig/Ts pair is new to this envelope and is
// deliberately distinct from the daemon envelope's X-Edge-Auth-Sig/Ts so the
// two schemes never share a header value.
const (
	ForwardAuthHeaderEmail   = "X-Auth-Request-Email"
	ForwardAuthHeaderGroups  = "X-Auth-Request-Groups"
	ForwardAuthHeaderAuditID = "X-Auth-Audit-Id"
	ForwardAuthHeaderSig     = "X-Edge-Auth-Fwd-Sig"
	ForwardAuthHeaderTs      = "X-Edge-Auth-Fwd-Ts"
)

// forwardAuthCanonicalVersion prefixes every signed message so this envelope is
// domain-separated from the daemon envelope (verify.go canonicalHMAC) and from
// any future scheme. A signature is only ever valid for the exact version it
// was minted under.
const forwardAuthCanonicalVersion = "fwdauth-v1"

// canonicalForwardAuth builds the byte string signed by both the edge (sign)
// and the upstream (verify). Every field is joined verbatim with newline
// separators — the caller must pass the SAME strings it stamped into / read
// from the headers (no trimming), or the HMAC will not reconstruct.
func canonicalForwardAuth(email, groups, auditID, ts string) string {
	return strings.Join([]string{
		forwardAuthCanonicalVersion,
		email,
		groups,
		auditID,
		ts,
	}, "\n")
}

// hmacForwardAuth returns the raw HMAC-SHA256 tag for the canonical message.
func hmacForwardAuth(key []byte, email, groups, auditID, ts string) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(canonicalForwardAuth(email, groups, auditID, ts)))
	return m.Sum(nil)
}

// SignForwardAuth computes the base64 provenance signature edge-auth stamps as
// X-Edge-Auth-Fwd-Sig on the forwardauth success response. email/groups/auditID
// MUST be the exact values stamped into X-Auth-Request-Email /
// X-Auth-Request-Groups / X-Auth-Audit-Id (groups is "" when no groups header is
// emitted), and ts the exact X-Edge-Auth-Fwd-Ts value.
func SignForwardAuth(key []byte, email, groups, auditID, ts string) string {
	return base64.StdEncoding.EncodeToString(hmacForwardAuth(key, email, groups, auditID, ts))
}

// ForwardAuthOutcome categorises a verification result — suitable as a metric
// label or a structured-log field.
type ForwardAuthOutcome string

const (
	// FwdOutcomeOK: a valid, in-skew signature over the presented identity.
	FwdOutcomeOK ForwardAuthOutcome = "ok"
	// FwdOutcomeNoIdentity: no X-Auth-Request-Email at all — there is nothing to
	// vouch for, so this is never trusted in either mode (the caller 401s on the
	// missing identity independently).
	FwdOutcomeNoIdentity ForwardAuthOutcome = "no_identity"
	// FwdOutcomeNoKey: no HMAC key configured on the verifier — verification
	// cannot run. Burn-in trusts; enforce refuses (fail closed on misconfig).
	FwdOutcomeNoKey ForwardAuthOutcome = "no_key"
	// FwdOutcomeNoSig: identity present but no envelope (no sig/ts). This is the
	// forged-in-cluster shape AND the pre-cutover shape (edge not yet signing),
	// which is exactly why burn-in exists.
	FwdOutcomeNoSig ForwardAuthOutcome = "no_sig"
	// FwdOutcomeStale: the timestamp is unparseable or outside MaxSkew.
	FwdOutcomeStale ForwardAuthOutcome = "stale"
	// FwdOutcomeMismatch: a signature was present but did not recompute — a
	// tampered identity or a wrong key.
	FwdOutcomeMismatch ForwardAuthOutcome = "mismatch"
)

// ForwardAuthVerifyConfig is the upstream-side knob set. Construct one from env
// at startup (the chart wires the shared edge-auth HMAC key + an enforce flag).
type ForwardAuthVerifyConfig struct {
	// HMACKey is the shared secret edge-auth signs with (EDGE_AUTH_HMAC_KEY).
	// Empty short-circuits verification to FwdOutcomeNoKey.
	HMACKey []byte

	// Enforce reflects the per-env burn-in flag. False (default): compute the
	// outcome for logging/metrics but always trust a PRESENT identity. True:
	// only FwdOutcomeOK is trusted; every other non-ok outcome for a present
	// identity is refused.
	Enforce bool

	// MaxSkew is the largest accepted clock drift between edge-auth and the
	// upstream. Defaults to 5 minutes when zero (matches the daemon envelope).
	MaxSkew time.Duration

	// Now is a clock injection seam for tests. Defaults to time.Now.
	Now func() time.Time
}

// ForwardAuthVerifyResult is returned per request. Trusted indicates whether the
// upstream should honour the X-Auth-Request-* identity on this request.
type ForwardAuthVerifyResult struct {
	Outcome ForwardAuthOutcome
	Trusted bool
	Email   string
	AuditID string
}

// Verify inspects the forwardauth provenance headers on r and returns the trust
// outcome. Call it once, after establishing that an identity is present, before
// acting on the identity. RAW (untrimmed) header values feed the HMAC so they
// match exactly what edge-auth signed.
func (c ForwardAuthVerifyConfig) Verify(r *http.Request) ForwardAuthVerifyResult {
	email := r.Header.Get(ForwardAuthHeaderEmail)
	groups := r.Header.Get(ForwardAuthHeaderGroups)
	auditID := r.Header.Get(ForwardAuthHeaderAuditID)
	sig := r.Header.Get(ForwardAuthHeaderSig)
	ts := r.Header.Get(ForwardAuthHeaderTs)

	outcome := c.classify(email, groups, auditID, sig, ts)

	res := ForwardAuthVerifyResult{Outcome: outcome, Email: email, AuditID: auditID}
	switch outcome {
	case FwdOutcomeOK:
		res.Trusted = true
	case FwdOutcomeNoIdentity:
		// Nothing to trust in either mode; the caller refuses the missing
		// identity on its own.
		res.Trusted = false
	default:
		// Burn-in (enforce=false) trusts; enforce refuses.
		res.Trusted = !c.Enforce
	}
	return res
}

func (c ForwardAuthVerifyConfig) classify(email, groups, auditID, sig, ts string) ForwardAuthOutcome {
	if strings.TrimSpace(email) == "" {
		return FwdOutcomeNoIdentity
	}
	if len(c.HMACKey) == 0 {
		return FwdOutcomeNoKey
	}
	if sig == "" || ts == "" {
		return FwdOutcomeNoSig
	}

	tsUnix, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64)
	if err != nil {
		return FwdOutcomeStale
	}
	skew := c.MaxSkew
	if skew == 0 {
		skew = 5 * time.Minute
	}
	now := c.Now
	if now == nil {
		now = time.Now
	}
	delta := now().Sub(time.Unix(tsUnix, 0))
	if delta < 0 {
		delta = -delta
	}
	if delta > skew {
		return FwdOutcomeStale
	}

	got, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return FwdOutcomeMismatch
	}
	expect := hmacForwardAuth(c.HMACKey, email, groups, auditID, ts)
	if !hmac.Equal(got, expect) {
		return FwdOutcomeMismatch
	}
	return FwdOutcomeOK
}

// LogForwardAuthOutcome emits a structured log line per Verify call. An ok
// result logs at debug; every non-ok logs at warn so a burn-in soak surfaces
// the outcomes an operator must clear before flipping Enforce on.
func LogForwardAuthOutcome(logger *slog.Logger, r ForwardAuthVerifyResult, enforce bool) {
	if logger == nil {
		return
	}
	if r.Outcome == FwdOutcomeOK {
		logger.Debug("forwardauth provenance ok",
			"email", r.Email,
			"audit_id", r.AuditID,
			"enforce", enforce,
		)
		return
	}
	logger.Warn("forwardauth provenance not verified",
		"outcome", string(r.Outcome),
		"email", r.Email,
		"audit_id", r.AuditID,
		"trusted", r.Trusted,
		"enforce", enforce,
	)
}
