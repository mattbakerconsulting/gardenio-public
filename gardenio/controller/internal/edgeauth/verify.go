package edgeauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Daemon-side HMAC verification helpers for the X-Operator trust
// headers stamped by edge-auth (cmd/edge-auth/proxy.go). Daemons in
// front of the CLI (kbeads, coopmux, ...) consume these helpers to gate
// whether they trust the X-Operator header when computing operated_by
// attribution.
//
// The Stage 3 / Safe-deploy rollout (ka-0000002544) introduces the
// daemonHmacVerify.enforce chart flag, defaulted off. Burn-in mode
// (enforce=false) logs mismatches but still trusts X-Operator (the
// pre-edge-auth attribution path is unchanged). When enforce=true is
// flipped per-env after a 7d clean burn-in, daemons drop X-Operator
// when the HMAC envelope is missing or invalid.

// VerifyHeader* names the request headers daemons inspect.
const (
	VerifyHeaderOperator  = "X-Operator"
	VerifyHeaderAuditID   = "X-Auth-Audit-Id"
	VerifyHeaderSignature = "X-Edge-Auth-Sig"
	VerifyHeaderTimestamp = "X-Edge-Auth-Ts"
)

// VerifyOutcome is the categorisation a daemon emits as a Prometheus
// counter label. The Safe-deploy alert pages on any non-ok in `enforce`
// mode and warns on every `mismatch` in either mode.
type VerifyOutcome string

const (
	OutcomeOK         VerifyOutcome = "ok"
	OutcomeNoOperator VerifyOutcome = "no_operator"
	OutcomeNoSig      VerifyOutcome = "no_sig"
	OutcomeStale      VerifyOutcome = "stale"
	OutcomeMismatch   VerifyOutcome = "mismatch"
	OutcomeNoKey      VerifyOutcome = "no_key"
)

// VerifyConfig is the daemon-side knob set. Daemons construct one from
// env at startup; the chart wires DAEMON_HMAC_KEY +
// DAEMON_HMAC_VERIFY_ENFORCE.
type VerifyConfig struct {
	// HMACKey is the shared secret signed by edge-auth. Must match
	// EDGE_AUTH_HMAC_KEY on the edge-auth side. Empty key short-
	// circuits verification (Outcome: OutcomeNoKey).
	HMACKey []byte

	// Enforce reflects daemonHmacVerify.enforce. When false (default
	// safe-deploy stance) Verify computes the outcome so the daemon
	// can log + count, but always returns Trusted=true. When true,
	// only OutcomeOK results in Trusted=true.
	Enforce bool

	// MaxSkew is the largest accepted clock drift between edge-auth
	// and the daemon. Defaults to 5 minutes when zero.
	MaxSkew time.Duration

	// Now is a clock injection seam for tests. Defaults to time.Now.
	Now func() time.Time
}

// VerifyConfigFromEnv builds a VerifyConfig from the standard env vars
// the chart sets on a daemon Deployment.
func VerifyConfigFromEnv() VerifyConfig {
	return VerifyConfig{
		HMACKey: []byte(os.Getenv("DAEMON_HMAC_KEY")),
		Enforce: envFlag("DAEMON_HMAC_VERIFY_ENFORCE"),
	}
}

// VerifyResult is returned per request. Trusted indicates whether the
// daemon should honour X-Operator on this request.
type VerifyResult struct {
	Outcome  VerifyOutcome
	Trusted  bool
	Operator string
	AuditID  string
}

// Verify inspects the edge-auth headers on r and returns the trust
// outcome. Daemons should call this once per request before consulting
// X-Operator for operated_by attribution. Callers MUST emit
// `daemon_hmac_verify_total{outcome=...,enforce=...}` from the result
// and surface non-ok outcomes to the operator log.
func (c VerifyConfig) Verify(r *http.Request) VerifyResult {
	op := r.Header.Get(VerifyHeaderOperator)
	auditID := r.Header.Get(VerifyHeaderAuditID)
	sig := r.Header.Get(VerifyHeaderSignature)
	ts := r.Header.Get(VerifyHeaderTimestamp)

	outcome := c.classify(op, auditID, sig, ts, r.URL.Path)

	res := VerifyResult{Outcome: outcome, Operator: op, AuditID: auditID}
	switch outcome {
	case OutcomeOK:
		res.Trusted = true
	case OutcomeNoOperator:
		// Nothing to trust in either mode.
		res.Trusted = false
	default:
		// Burn-in mode (enforce=false): hand back the asserted operator
		// so callers can fall through to the pre-edge-auth attribution
		// path. Enforce mode flips this only after a clean per-env soak.
		res.Trusted = !c.Enforce
	}
	return res
}

func (c VerifyConfig) classify(op, auditID, sig, ts, path string) VerifyOutcome {
	if op == "" {
		return OutcomeNoOperator
	}
	if len(c.HMACKey) == 0 {
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
	expect := canonicalHMAC(c.HMACKey, op, auditID, path, ts)
	if !hmac.Equal(got, expect) {
		return OutcomeMismatch
	}
	return OutcomeOK
}

// canonicalHMAC matches the edge-auth proxy.signEdgeAuth message shape
// (operator\nauditID\npath\nts) so the daemon and the edge sign
// identical bytes.
func canonicalHMAC(key []byte, operator, auditID, path, ts string) []byte {
	msg := operator + "\n" + auditID + "\n" + path + "\n" + ts
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(msg))
	return m.Sum(nil)
}

// envFlag matches the cmd/edge-auth helper of the same name so the
// daemon and the edge share parsing semantics for boolean env vars.
func envFlag(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
