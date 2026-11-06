package server

import "github.com/prometheus/client_golang/prometheus"

// edgeAuthVerifyTotal counts edge-auth HMAC verify outcomes (ka-0000001678 /
// W-B-11). The `outcome` label tracks the categorisation returned by
// edgeauth.Verify (ok / no_operator / no_sig / stale / mismatch / no_key) so
// the operator can see during the burn-in window whether the shim and the
// daemon agree on the wire shape before flipping daemonHmacVerify.enforce to
// true. The `enforce` label distinguishes burn-in from enforce mode so the
// operator can confirm a clean ≥7d soak per env before flipping.
//
// Exposed via /metrics on the internal-only metrics port.
var edgeAuthVerifyTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		Name: "gardenio_beads_edge_auth_verify_total",
		Help: "Number of edge-auth HMAC verify attempts on the daemon, labelled by outcome (ok/no_operator/no_sig/stale/mismatch/no_key) and enforce mode (true/false). Burn-in counts non-ok outcomes without rejecting; enforce mode rejects.",
	},
	[]string{"outcome", "enforce"},
)
