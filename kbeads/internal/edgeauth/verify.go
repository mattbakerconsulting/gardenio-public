// Package edgeauth implements the daemon side of the Stage 3 / W-B-11
// trust handshake (ka-0000001678): verify the HMAC envelope stamped by
// gardenio-edge-auth (gardenio/controller/cmd/edge-auth/proxy.go) and
// surface a trust outcome the kbeads HTTP middleware can act on.
//
// The wire shape is the source-of-truth contract:
//
//	X-Operator:       <email>           // operator identity (was the bearer's owner)
//	X-Auth-Audit-Id:  <ulid>            // correlation id, also stamped by the edge
//	X-Edge-Auth-Ts:   <unix seconds>    // freshness anchor, ±MaxSkew window
//	X-Edge-Auth-Sig:  <base64 v1 hmac>
//
// Version 2 additionally carries X-Edge-Auth-Version, X-Edge-Auth-Mode, and
// X-Edge-Auth-Roles. X-Edge-Auth-Sig-V2 binds the HTTP method and every
// authorization-bearing field. The v1 signature remains on the wire only so
// older daemon pods can continue serving during a rolling deployment:
//
//	v2\noperator\nauthMode\nroles\nauditID\nMETHOD\npath\nts
//
// The HMAC pinning + freshness window defends against header forgery
// from inside the cluster: only a caller with the shared HMAC key can
// produce a valid envelope, and only within a small clock-skew window.
//
// Safe-deploy stance (ka-0000002544): the daemonHmacVerify.enforce chart
// flag defaults off. In burn-in (enforce=false) the helper computes the
// outcome so the daemon can log + count but reports Trusted=true so the
// pre-edge-auth attribution path is unchanged. Flipping enforce=true is
// gated on a clean ≥7d burn-in per env.
package edgeauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Header names recognised on inbound requests. Kept in sync with
// gardenio/controller/cmd/edge-auth/proxy.go.
const (
	HeaderOperator    = "X-Operator"
	HeaderAuditID     = "X-Auth-Audit-Id"
	HeaderSignature   = "X-Edge-Auth-Sig"
	HeaderSignatureV2 = "X-Edge-Auth-Sig-V2"
	HeaderTimestamp   = "X-Edge-Auth-Ts"
	HeaderVersion     = "X-Edge-Auth-Version"
	HeaderAuthMode    = "X-Edge-Auth-Mode"
	HeaderRoles       = "X-Edge-Auth-Roles"
)

// Outcome is the categorisation a daemon emits as a Prometheus label.
type Outcome string

const (
	OutcomeOK         Outcome = "ok"
	OutcomeNoOperator Outcome = "no_operator"
	OutcomeNoSig      Outcome = "no_sig"
	OutcomeStale      Outcome = "stale"
	OutcomeMismatch   Outcome = "mismatch"
	OutcomeNoKey      Outcome = "no_key"
	OutcomeMalformed  Outcome = "malformed"
)

// Config carries the daemon-side knobs. Built from env via ConfigFromEnv.
type Config struct {
	// HMACKey is the shared secret signed by edge-auth. Must match
	// EDGE_AUTH_HMAC_KEY on the edge-auth side. Empty key short-
	// circuits verification (Outcome: OutcomeNoKey).
	HMACKey []byte

	// HMACKeyV2 is the key v2 (role-bearing) envelopes are signed with
	// (DAEMON_HMAC_KEY_V2, EDGE_AUTH_HMAC_KEY_V2 on the edge). It is split
	// from HMACKey so a holder of the v1 key alone, such as beads-viewer,
	// which signs only v1 envelopes, cannot mint a v2 envelope naming any
	// operator with gardenio-admin (gd-dMof1PW1oJ). Empty falls back to
	// HMACKey, the pre-split behaviour, until the second secret is wired.
	HMACKeyV2 []byte

	// Enforce mirrors daemonHmacVerify.enforce. When false (default
	// safe-deploy stance) Verify computes the outcome so the daemon
	// can log + count, but reports Trusted=true so the existing
	// attribution path is unchanged. When true, only OutcomeOK is
	// trusted.
	Enforce bool

	// MaxSkew is the largest accepted clock drift between edge-auth
	// and the daemon. Defaults to 5 minutes when zero.
	MaxSkew time.Duration

	// Now is a clock injection seam for tests. Defaults to time.Now.
	Now func() time.Time
}

// ConfigFromEnv builds a Config from the standard env vars the chart
// sets on a daemon Deployment.
func ConfigFromEnv() Config {
	return Config{
		HMACKey:   []byte(os.Getenv("DAEMON_HMAC_KEY")),
		HMACKeyV2: []byte(os.Getenv("DAEMON_HMAC_KEY_V2")),
		Enforce:   envFlag("DAEMON_HMAC_VERIFY_ENFORCE"),
	}
}

// Enabled is true once a daemon has at least an HMAC key. Callers use
// this to decide whether to install the verify middleware at all.
func (c Config) Enabled() bool { return len(c.HMACKey) > 0 || len(c.HMACKeyV2) > 0 }

// keyFor returns the key an envelope of the given version must be signed
// with: HMACKeyV2 for v2 when set, else the shared HMACKey.
func (c Config) keyFor(version int) []byte {
	if version == 2 && len(c.HMACKeyV2) > 0 {
		return c.HMACKeyV2
	}
	return c.HMACKey
}

// Result is returned per request. Trusted indicates whether the daemon
// should honour X-Operator on this request.
type Result struct {
	Outcome    Outcome
	Trusted    bool
	Operator   string
	AuditID    string
	Version    int
	AuthMode   string
	Roles      []string
	VerifiedV2 bool
}

// Verify inspects the edge-auth headers on r and returns the trust
// outcome. Callers MUST emit a `edge_auth_verify_total{outcome,enforce}`
// metric from the result and log non-ok outcomes.
func (c Config) Verify(r *http.Request) Result {
	op := r.Header.Get(HeaderOperator)
	auditID := r.Header.Get(HeaderAuditID)
	sig := r.Header.Get(HeaderSignature)
	ts := r.Header.Get(HeaderTimestamp)
	versionHeader := r.Header.Get(HeaderVersion)
	authMode := r.Header.Get(HeaderAuthMode)
	rolesHeader := r.Header.Get(HeaderRoles)

	version := 1
	var roles []string
	if versionHeader != "" {
		if versionHeader != "2" {
			return c.result(OutcomeMalformed, op, auditID, version, "", nil)
		}
		version = 2
		sig = r.Header.Get(HeaderSignatureV2)
		var ok bool
		roles, ok = parseCanonicalRoles(rolesHeader)
		if !ok || !validAuthMode(authMode) {
			return c.result(OutcomeMalformed, op, auditID, version, authMode, nil)
		}
	}

	outcome := c.classify(
		op,
		auditID,
		sig,
		ts,
		r.Method,
		r.URL.Path,
		version,
		authMode,
		rolesHeader,
	)

	res := c.result(outcome, op, auditID, version, authMode, roles)
	res.VerifiedV2 = outcome == OutcomeOK && version == 2
	if !res.VerifiedV2 {
		// Never let client-supplied v2 role data escape verification. Burn-in
		// may preserve read compatibility, but it cannot grant write roles.
		res.Roles = nil
		res.AuthMode = ""
	}
	return res
}

func (c Config) result(
	outcome Outcome,
	op, auditID string,
	version int,
	authMode string,
	roles []string,
) Result {
	res := Result{
		Outcome:  outcome,
		Operator: op,
		AuditID:  auditID,
		Version:  version,
		AuthMode: authMode,
		Roles:    roles,
	}
	switch outcome {
	case OutcomeOK:
		res.Trusted = true
	case OutcomeNoOperator:
		// Nothing to trust either way.
		res.Trusted = false
	default:
		// Burn-in mode (Enforce=false): trust the asserted operator so
		// callers fall through to the pre-edge-auth attribution path.
		// Enforce mode flips this only after a clean per-env soak.
		res.Trusted = !c.Enforce
	}
	return res
}

func (c Config) classify(
	op, auditID, sig, ts, method, path string,
	version int,
	authMode, roles string,
) Outcome {
	if op == "" {
		return OutcomeNoOperator
	}
	key := c.keyFor(version)
	if len(key) == 0 {
		return OutcomeNoKey
	}
	if sig == "" || ts == "" {
		return OutcomeNoSig
	}

	tsUnix, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64)
	if err != nil {
		return OutcomeStale
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
		return OutcomeStale
	}

	got, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return OutcomeMismatch
	}
	var expect []byte
	if version == 2 {
		expect = canonicalHMACV2(
			key,
			op,
			authMode,
			roles,
			auditID,
			method,
			path,
			ts,
		)
	} else {
		expect = canonicalHMAC(key, op, auditID, path, ts)
	}
	if !hmac.Equal(got, expect) {
		return OutcomeMismatch
	}
	return OutcomeOK
}

// canonicalHMAC matches gardenio-edge-auth's proxy.signEdgeAuth message
// shape (operator\nauditID\npath\nts) so the daemon and the edge sign
// identical bytes.
func canonicalHMAC(key []byte, operator, auditID, path, ts string) []byte {
	msg := operator + "\n" + auditID + "\n" + path + "\n" + ts
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(msg))
	return m.Sum(nil)
}

func canonicalHMACV2(
	key []byte,
	operator, authMode, roles, auditID, method, path, ts string,
) []byte {
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
	return m.Sum(nil)
}

func parseCanonicalRoles(raw string) ([]string, bool) {
	if raw == "" {
		return nil, false
	}
	allowed := map[string]bool{
		"gardenio-viewer":   true,
		"gardenio-operator": true,
		"gardenio-admin":    true,
	}
	parts := strings.Split(raw, ",")
	roles := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))
	for _, role := range parts {
		if role == "" || strings.TrimSpace(role) != role || !allowed[role] || seen[role] {
			return nil, false
		}
		seen[role] = true
		roles = append(roles, role)
	}
	sorted := append([]string(nil), roles...)
	sort.Strings(sorted)
	if strings.Join(sorted, ",") != raw {
		return nil, false
	}
	return roles, true
}

func validAuthMode(mode string) bool {
	switch mode {
	case "entra", "ava", "pat", "cookie", "slack":
		return true
	default:
		return false
	}
}

func envFlag(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// LogResult emits a structured log line per Verify call. Daemons
// hand a request-scoped logger so the audit id correlates.
func LogResult(logger *slog.Logger, r Result, enforce bool) {
	if logger == nil {
		return
	}
	if r.Outcome == OutcomeOK {
		logger.Debug("edge-auth verify ok",
			"operator", r.Operator,
			"audit_id", r.AuditID,
			"version", r.Version,
			"auth_mode", r.AuthMode,
			"roles", r.Roles,
			"enforce", enforce,
		)
		return
	}
	logger.Warn("edge-auth verify failed",
		"outcome", string(r.Outcome),
		"operator", r.Operator,
		"audit_id", r.AuditID,
		"trusted", r.Trusted,
		"enforce", enforce,
	)
}
