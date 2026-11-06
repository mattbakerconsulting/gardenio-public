// Package obspii is the single source of truth for the operator-keyed metric
// label allow-list and the producer-side PII sanitiser, implementing the
// defense-in-depth guard described in
// gardenio/docs/research/obs-centralization-w2-pii.md §6 (the pii-gateway
// catch-all rule) at the metric producer/emitter boundary.
//
// Three layers protect operator-attributed and security telemetry from leaking
// personally-identifying values into Prometheus labels (which explode
// cardinality and persist in vmsingle for the full retention window):
//
//  1. The pii-gateway catalog (observability/pii-gateway/catalog.yaml) — the
//     downstream OTEL catch-all that hashes any `*.operator.email` / `user.email`
//     metric label before it reaches the common collector (rule
//     id=pii-direct.operator.email, class=pii-direct, action=hash, signals=[metrics]).
//  2. The vmagent metric_relabel backstop (helm vmagent configmap) — drops the
//     forbidden deny-list labels from every operator-keyed series before
//     remote_write to vmsingle.
//  3. This package — the producer-side allow-list guard + sanitiser. The CI
//     tests in allowlist_test.go fail if a forbidden label is ever introduced
//     onto an operator/security metric, and SanitizeOperatorLabelValue hashes
//     an accidentally-emitted email before it is rendered into the push payload.
//
// None of the three trusts the others to have done its job — that is the whole
// point of defense-in-depth (doc §1.2 / §1.3 "two-tier defense").
package obspii

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"regexp"
	"strings"
)

// AllowedOperatorLabels is the bounded allow-list of label names permitted on
// any operator-keyed or security metric series. Membership is positive-only:
// a label NOT in this set is rejected by ValidateMetricLabels (default-deny,
// mirroring the catalog's default_class:free-text posture in doc §6.1).
//
// The first 17 entries are the canonical set enumerated on the task bead
// (ka-0000007286). The final two — `model` and `escalated_by_role` — are
// bounded-cardinality, non-identifying labels that already appear on
// operator/security series merged from the sibling telemetry tasks
// (gardenio_operator_session_* carries `model`, ka-0000007818;
// gardenio_escalations_total carries `escalated_by_role`, ka-0000005541). They
// are admitted here because the guard's job is to reject PII / unbounded
// labels, not to reject every bounded label the bead author did not enumerate.
// New additions MUST be bounded-cardinality and carry no personal identifier.
var AllowedOperatorLabels = map[string]struct{}{
	"operator_id":     {}, // opaque operators(id) FK — never an email/slug
	"project":         {},
	"role":            {},
	"action":          {},
	"reason":          {},
	"bead_type":       {},
	"activity_type":   {},
	"surface":         {},
	"provider":        {},
	"status":          {},
	"revocation_type": {},
	"event":           {},
	"urgency":         {},
	"outcome":         {},
	"route_template":  {},
	"endpoint":        {},
	"status_code":     {},
	// Bounded, non-PII extensions present on in-tree operator/security series:
	"model":             {}, // ~5 model IDs (gardenio_operator_session_*)
	"escalated_by_role": {}, // role enum (gardenio_escalations_total)
}

// ForbiddenOperatorLabels is the explicit deny-list: label names that are known
// to carry (or directly encode) a personal identifier and MUST never appear on
// an operator-keyed metric. These are rejected even before the allow-list check
// so the failure message is specific. `session_id` is forbidden specifically on
// operator counters (it is legitimate on the per-session push metrics, which are
// not operator-keyed) — see doc §3.6 "metric data points are the worst place to
// leak PII because they explode cardinality".
var ForbiddenOperatorLabels = map[string]struct{}{
	"oidc_email":    {},
	"slack_user_id": {},
	"display":       {},
	"display_name":  {},
	"raw_url":       {},
	"url":           {},
	"email":         {},
	"viewer_email":  {},
	"session_id":    {}, // forbidden on operator-keyed series specifically
}

// OperatorKeyedMetrics is the canonical registry of operator-attributed and
// security metric series and the exact label set each is allowed to carry. It
// is the machine-checked contract behind acceptance criterion (3): the CI test
// validates every declared label set against the allow/deny lists, so adding a
// forbidden label to any of these metrics — and recording it here — fails CI;
// and adding a label WITHOUT recording it here drifts the registry, which the
// live-collection test (bridge package) catches for the metrics it can
// introspect in-process.
//
// Keep this in sync with the emitters:
//   - gardenio_operator_activity_total          — bridge + kbeads (ka-0000006613)
//   - gardenio_operator_session_duration_seconds — gb session push (ka-0000007818)
//   - gardenio_operator_cost_usd_total          — controller session-close reconciler
//     (cmd/controller/reconcile_session_spend.go, ka-0000000999). operator_id is the
//     opaque operators(id) FK, never an email — so this producer needs no PII hashing;
//     the SanitizeOperatorLabelValue catch-all guards the gb-push emitters above.
//   - gardenio_decisions_lifecycle_total        — kbeads (ka-0000005541)
//   - gardenio_escalations_total                — kbeads (ka-0000005541)
//   - gardenio_user_request_latency_seconds     — beads-viewer (ka-0000006752)
var OperatorKeyedMetrics = map[string][]string{
	"gardenio_operator_activity_total":           {"operator_id", "project", "activity_type", "surface"},
	"gardenio_operator_session_duration_seconds": {"operator_id", "project", "model"},
	"gardenio_operator_cost_usd_total":           {"operator_id", "project", "model"},
	"gardenio_decisions_lifecycle_total":         {"event", "urgency"},
	"gardenio_escalations_total":                 {"escalated_by_role", "outcome"},
	"gardenio_user_request_latency_seconds":      {"surface", "endpoint", "status_code"},
}

// emailPattern matches an RFC-5322-ish email shape. Deliberately broad: the
// sanitiser's job is to catch an accidentally-emitted address, so a false
// positive (hashing a non-email that looks like one) is strictly safer than a
// false negative (leaking a real one).
var emailPattern = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)

// LooksLikeEmail reports whether v contains an email-shaped substring.
func LooksLikeEmail(v string) bool {
	return emailPattern.MatchString(v)
}

// hashPrefix is the marker prepended to hashed values so downstream consumers
// (and the audit pipeline, doc §3.1) can tell at a glance the value was hashed
// rather than a raw value that happens to look like a token.
const hashPrefix = "h_"

// Hash implements the doc §3.1 `hash` action: HMAC-SHA256 keyed by `key`, over
// `class || ":" || value`, base64url-encoded, first 16 bytes (96 bits), with an
// "h_" prefix. The class prefix means the same string mapped under two classes
// never collides. A nil/empty key still produces a stable (keyed-by-empty) hash
// so the sanitiser fails closed rather than emitting cleartext when the
// REDACTION_KEY is unset.
func Hash(key []byte, class, value string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(class))
	mac.Write([]byte(":"))
	mac.Write([]byte(value))
	sum := mac.Sum(nil)
	enc := base64.RawURLEncoding.EncodeToString(sum[:16])
	return hashPrefix + enc
}

// IsHashed reports whether v carries the "h_" hashed-value marker.
func IsHashed(v string) bool {
	return strings.HasPrefix(v, hashPrefix)
}

// SanitizeOperatorLabelValue is the producer-side defense-in-depth catch-all
// that mirrors the catalog's pii-direct.operator.email rule (action=hash) at
// the emitter. If value contains an email shape it is replaced by its
// class-prefixed HMAC hash (doc §2.2: pii-direct default action is `hash`,
// preserving cardinality for "how many distinct operators" without keeping the
// cleartext). Already-hashed and non-email values pass through unchanged.
//
// This is the function that makes acceptance criterion (3) — "an accidentally
// emitted email is hashed before vmsingle" — true at the gb session-push
// boundary, complementing the downstream gateway rule and the vmagent relabel.
func SanitizeOperatorLabelValue(key []byte, value string) string {
	if value == "" || IsHashed(value) {
		return value
	}
	if LooksLikeEmail(value) {
		return Hash(key, "pii-direct", value)
	}
	return value
}
