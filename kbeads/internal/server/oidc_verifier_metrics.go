package server

import (
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
)

// beadsOIDCVerifierHealthy is the persistent-warning signal bug
// gd-qZs1IbMhzQ asks for: today a failed OIDC verifier init at startup
// (cmd/kd/serve.go) only logs an ERROR line and otherwise silently runs the
// pod with OIDC auth permanently disabled — no other signal exists. 1 means
// the daemon has an initialized, currently-installed OIDC verifier; 0 means
// OIDC auth is unavailable (init never succeeded, or hasn't succeeded yet —
// see the retry loop in cmd/kd/serve.go). Every "OIDC-only" rule (escalation
// approver derivation, taint scrub-approve, the OIDC spawn permission path)
// is unreachable while this reads 0.
var beadsOIDCVerifierHealthy = prometheus.NewGauge(prometheus.GaugeOpts{
	Name: "beads_oidc_verifier_healthy",
	Help: "1 when the daemon's OIDC verifier is initialized and installed, 0 when OIDC auth is unavailable because discovery failed or has not yet succeeded.",
})

// beadsOIDCVerifierInitErrors is an info-style gauge carrying the reason for
// the most recent OIDC verifier init/retry failure, labelled by a bounded
// classification (not the raw error string, to keep cardinality fixed
// regardless of what a dial/TLS error happens to say) plus the configured
// issuer count. Exactly one series is ever set at a time — RecordOIDCVerifierHealth
// resets the vec before (re-)setting it — so it always reflects the latest
// attempt, and the series disappears entirely once init succeeds.
var beadsOIDCVerifierInitErrors = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "beads_oidc_verifier_init_error",
		Help: "1 for the bounded reason classification of the most recent OIDC verifier init/retry failure; no series once initialization has succeeded.",
	},
	[]string{"reason", "issuer_count"},
)

// classifyOIDCInitError maps an OIDC discovery error onto a small, bounded
// reason enum by matching the stable substrings the underlying net/tls/http
// errors produce (the same message-shape-matching approach as
// isDBAuthError/isQueryCanceledError in db_auth_metrics.go), so the label
// cardinality stays fixed no matter what a dial error's remote address or a
// TLS error's certificate CN happens to be.
func classifyOIDCInitError(err error) string {
	if err == nil {
		return "none"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "deadline exceeded"):
		return "timeout"
	case strings.Contains(msg, "connection refused"):
		return "connection_refused"
	case strings.Contains(msg, "no such host"):
		return "dns"
	case strings.Contains(msg, "certificate"), strings.Contains(msg, "x509"), strings.Contains(msg, "tls"):
		return "tls"
	default:
		return "other"
	}
}

// RecordOIDCVerifierHealth flips beadsOIDCVerifierHealthy and, on failure,
// records the bounded-reason error series. Called from cmd/kd/serve.go at
// the OIDC verifier init site and from each retry attempt.
func RecordOIDCVerifierHealth(issuerCount int, err error) {
	beadsOIDCVerifierInitErrors.Reset()
	if err != nil {
		beadsOIDCVerifierHealthy.Set(0)
		beadsOIDCVerifierInitErrors.WithLabelValues(classifyOIDCInitError(err), strconv.Itoa(issuerCount)).Set(1)
		return
	}
	beadsOIDCVerifierHealthy.Set(1)
}

// registerOIDCVerifierMetrics registers the OIDC verifier health collectors
// on reg. Called unconditionally from MetricsHandler — the gauge reads 0 with
// no error series when OIDC is not configured at all, which is a fine,
// unambiguous default (no verifier was ever expected to be healthy).
func registerOIDCVerifierMetrics(reg prometheus.Registerer) {
	reg.MustRegister(beadsOIDCVerifierHealthy, beadsOIDCVerifierInitErrors)
}
