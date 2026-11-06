package obspii

import (
	"strings"
	"testing"
)

// TestOperatorKeyedMetricsRespectAllowlist is acceptance criterion (3): CI
// fails if a forbidden label is ever introduced on an operator/security metric.
// Every label set in the OperatorKeyedMetrics registry is validated against the
// allow-list (default-deny) and the explicit PII deny-list. A new emitter that
// adds, say, oidc_email or session_id to an operator counter — and records it
// here — turns this test red.
func TestOperatorKeyedMetricsRespectAllowlist(t *testing.T) {
	if len(OperatorKeyedMetrics) == 0 {
		t.Fatal("OperatorKeyedMetrics registry is empty — the guard would pass vacuously")
	}
	for metric, labels := range OperatorKeyedMetrics {
		if bad := ValidateMetricLabels(labels); len(bad) > 0 {
			t.Errorf("metric %q carries forbidden/unknown label(s) %v — not in the bounded allow-list", metric, bad)
		}
	}
}

// TestForbiddenLabelsRejected proves the guard actually catches the exact
// labels the bead calls out: oidc_email, slack_user_id, display, raw-url, and
// session_id on operator counters.
func TestForbiddenLabelsRejected(t *testing.T) {
	forbidden := []string{
		"oidc_email",
		"slack_user_id",
		"display",
		"display_name",
		"raw_url",
		"url",
		"email",
		"viewer_email",
		"session_id",
	}
	for _, l := range forbidden {
		t.Run(l, func(t *testing.T) {
			if !IsForbiddenLabel(l) {
				t.Errorf("%q should be on the deny-list", l)
			}
			// And a series carrying it must be rejected even alongside
			// otherwise-allowed labels.
			if bad := ValidateMetricLabels([]string{"operator_id", "project", l}); len(bad) == 0 {
				t.Errorf("a series with %q should be rejected by ValidateMetricLabels", l)
			}
		})
	}
}

// TestUnknownLabelRejectedDefaultDeny proves classification is default-deny: a
// label that is neither explicitly forbidden nor on the allow-list is still
// rejected (mirrors the catalog's default_class:free-text in doc §6.1).
func TestUnknownLabelRejectedDefaultDeny(t *testing.T) {
	bad := ValidateMetricLabels([]string{"operator_id", "totally_new_label"})
	if len(bad) != 1 || bad[0] != "totally_new_label" {
		t.Errorf("expected default-deny rejection of totally_new_label, got %v", bad)
	}
}

// TestAllowedLabelsPass confirms the full bead-enumerated allow-list is
// accepted as one series (no false positives).
func TestAllowedLabelsPass(t *testing.T) {
	allowed := []string{
		"operator_id", "project", "role", "action", "reason", "bead_type",
		"activity_type", "surface", "provider", "status", "revocation_type",
		"event", "urgency", "outcome", "route_template", "endpoint", "status_code",
	}
	if bad := ValidateMetricLabels(allowed); len(bad) > 0 {
		t.Errorf("the bead-enumerated allow-list should pass clean, got rejections %v", bad)
	}
}

// TestAccidentalEmailIsHashedBeforeVMSingle is acceptance criterion (3): an
// accidentally-emitted email is hashed before it would reach vmsingle. This
// exercises the producer-side catch-all that the gb session-push path calls on
// every operator label value.
func TestAccidentalEmailIsHashedBeforeVMSingle(t *testing.T) {
	key := []byte("test-redaction-key")
	email := "matthew.baker@acme.example"

	got := SanitizeOperatorLabelValue(key, email)

	if got == email {
		t.Fatalf("email leaked unhashed: %q", got)
	}
	if !IsHashed(got) {
		t.Errorf("sanitised value missing the h_ hashed marker: %q", got)
	}
	if strings.Contains(got, "@") || strings.Contains(got, "acme") {
		t.Errorf("hashed value still contains email fragments: %q", got)
	}
	if LooksLikeEmail(got) {
		t.Errorf("sanitised value still looks like an email: %q", got)
	}

	// Deterministic: same input → same hash (cardinality preserved).
	if again := SanitizeOperatorLabelValue(key, email); again != got {
		t.Errorf("hash is not deterministic: %q vs %q", got, again)
	}

	// Class prefix prevents cross-class collision.
	if Hash(key, "pii-direct", email) == Hash(key, "phi-direct", email) {
		t.Error("class prefix must prevent cross-class hash collision")
	}

	// Idempotent: hashing an already-hashed value is a no-op.
	if SanitizeOperatorLabelValue(key, got) != got {
		t.Error("SanitizeOperatorLabelValue should be idempotent on already-hashed values")
	}
}

// TestSanitizePassesThroughCleanValues confirms the sanitiser does not mangle
// legitimate non-email label values (operator_id FKs, project names, sentinels).
func TestSanitizePassesThroughCleanValues(t *testing.T) {
	key := []byte("k")
	for _, v := range []string{"", "0", "gardenio", "slack:U0OPERATOR1", "spawn", "web", "GET /u/{slug}"} {
		if got := SanitizeOperatorLabelValue(key, v); got != v {
			t.Errorf("clean value %q was modified to %q", v, got)
		}
	}
}

// TestSanitizeFailsClosedWithoutKey proves the sanitiser still redacts an email
// even when no REDACTION_KEY is configured (fail-closed, doc §4.4): the value
// is hashed (keyed by empty), never passed through as cleartext.
func TestSanitizeFailsClosedWithoutKey(t *testing.T) {
	email := "leak@example.com"
	got := SanitizeOperatorLabelValue(nil, email)
	if got == email || !IsHashed(got) {
		t.Errorf("email must be hashed even with no key, got %q", got)
	}
}
