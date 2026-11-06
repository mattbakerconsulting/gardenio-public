package main

// Unit tests for reconcileLegacyStatusFields — the probe's writeback of the
// legacy `status` / `exhausted_at` fields based on rate-limit state.
//
// Regression coverage for ka-0000005198: account beads stayed
// status="exhausted" indefinitely after Anthropic throttle resets, so
// buildProjectAccountOverride excluded healthy accounts as candidates.

import (
	"testing"
	"time"
)

func TestReconcileLegacyStatusFields_AllowedToThrottledSetsExhausted(t *testing.T) {
	now := time.Date(2026, 4, 29, 17, 0, 0, 0, time.UTC)
	current := map[string]string{
		"status":       "",
		"exhausted_at": "",
	}
	result := probeResult{Status: "rejected", Utilization7d: 0.42, CheckedAt: now}
	fields := map[string]string{}

	reconcileLegacyStatusFields(current, result, fields)

	if fields["status"] != "exhausted" {
		t.Errorf("status = %q, want %q", fields["status"], "exhausted")
	}
	if fields["exhausted_at"] != now.Format(time.RFC3339) {
		t.Errorf("exhausted_at = %q, want %q", fields["exhausted_at"], now.Format(time.RFC3339))
	}
}

func TestReconcileLegacyStatusFields_HighUtilSetsExhausted(t *testing.T) {
	now := time.Date(2026, 4, 29, 17, 0, 0, 0, time.UTC)
	current := map[string]string{
		"status":       "",
		"exhausted_at": "",
	}
	// Anthropic still serving (allowed_warning) but 7d utilization is at the
	// budget cap — treat as exhausted so failover steers traffic away.
	result := probeResult{Status: "allowed_warning", Utilization7d: 1.0, CheckedAt: now}
	fields := map[string]string{}

	reconcileLegacyStatusFields(current, result, fields)

	if fields["status"] != "exhausted" {
		t.Errorf("status = %q, want %q", fields["status"], "exhausted")
	}
	if fields["exhausted_at"] != now.Format(time.RFC3339) {
		t.Errorf("exhausted_at = %q, want %q", fields["exhausted_at"], now.Format(time.RFC3339))
	}
}

func TestReconcileLegacyStatusFields_ThrottledToAllowedClearsExhausted(t *testing.T) {
	current := map[string]string{
		"status":         "exhausted",
		"exhausted_at":   "2026-04-22T10:00:00Z",
		"cooldown_until": "2026-04-23T10:00:00Z",
	}
	result := probeResult{Status: "allowed", Utilization7d: 0.05, CheckedAt: time.Now().UTC()}
	fields := map[string]string{}

	reconcileLegacyStatusFields(current, result, fields)

	if v, ok := fields["status"]; !ok || v != "" {
		t.Errorf("status = %q (set=%v), want empty", v, ok)
	}
	if v, ok := fields["exhausted_at"]; !ok || v != "" {
		t.Errorf("exhausted_at = %q (set=%v), want empty", v, ok)
	}
	if v, ok := fields["cooldown_until"]; !ok || v != "" {
		t.Errorf("cooldown_until = %q (set=%v), want empty", v, ok)
	}
}

func TestReconcileLegacyStatusFields_AllowedWarningDoesNotClearExhausted(t *testing.T) {
	current := map[string]string{
		"status":       "exhausted",
		"exhausted_at": "2026-04-22T10:00:00Z",
	}
	// allowed_warning is high-utilization — could be rejected again immediately,
	// must not be treated as recovery (matches throttleRecovered semantics).
	result := probeResult{Status: "allowed_warning", Utilization7d: 0.98, CheckedAt: time.Now().UTC()}
	fields := map[string]string{}

	reconcileLegacyStatusFields(current, result, fields)

	if _, ok := fields["status"]; ok {
		t.Errorf("status should not be touched on allowed_warning; got %q", fields["status"])
	}
	if _, ok := fields["exhausted_at"]; ok {
		t.Errorf("exhausted_at should not be touched on allowed_warning; got %q", fields["exhausted_at"])
	}
}

func TestReconcileLegacyStatusFields_AlreadyExhaustedPreservesExhaustedAt(t *testing.T) {
	now := time.Date(2026, 4, 29, 17, 0, 0, 0, time.UTC)
	originalExhaustedAt := "2026-04-22T10:00:00Z"
	current := map[string]string{
		"status":       "exhausted",
		"exhausted_at": originalExhaustedAt,
	}
	result := probeResult{Status: "rejected", Utilization7d: 0.5, CheckedAt: now}
	fields := map[string]string{}

	reconcileLegacyStatusFields(current, result, fields)

	// status already exhausted — no-op (don't churn writes; don't overwrite
	// the original exhaustion timestamp).
	if _, ok := fields["status"]; ok {
		t.Errorf("status should not be re-written when already exhausted; got %q", fields["status"])
	}
	if _, ok := fields["exhausted_at"]; ok {
		t.Errorf("exhausted_at should not be overwritten when already set; got %q", fields["exhausted_at"])
	}
}

func TestReconcileLegacyStatusFields_AllowedNoExhaustionIsNoop(t *testing.T) {
	current := map[string]string{
		"status":       "",
		"exhausted_at": "",
	}
	result := probeResult{Status: "allowed", Utilization7d: 0.05, CheckedAt: time.Now().UTC()}
	fields := map[string]string{}

	reconcileLegacyStatusFields(current, result, fields)

	if len(fields) != 0 {
		t.Errorf("expected no field writes when account is healthy and clean; got %v", fields)
	}
}

// ka-0000003350: a SUBSCRIPTION account (the whole gardenio fleet; billing_type
// unset defaults to subscription) whose rate-limit windows are OK
// (status="allowed", util well under 1.0) but which carries
// overage_disabled_reason="out_of_credits" MUST have its exhausted flag cleared
// on recovery. For a subscription account out_of_credits is a benign permanent
// property — the fleet primary matthewbaker1110 carries it while serving 200s.
// Holding it here on a transient 429 stranded the one healthy account and
// starved the fleet (the incident this fix closes). Subscription oscillation is
// prevented by the re-mark debounce, not by a permanent freeze.
func TestReconcileLegacyStatusFields_SubscriptionOutOfCreditsClearsExhausted(t *testing.T) {
	current := map[string]string{
		"status":         "exhausted",
		"exhausted_at":   "2026-07-06T05:00:22Z",
		"cooldown_until": "2026-07-06T06:00:22Z",
		// billing_type intentionally unset — defaults to subscription.
	}
	// matthewbaker1110's real probe shape: throttle allowed, low utilization,
	// but overage rejected because out of credits.
	result := probeResult{
		Status:        "allowed",
		Utilization5h: 0.06,
		Utilization7d: 0.28,
		OverageStatus: "rejected",
		OverageReason: "out_of_credits",
		CheckedAt:     time.Now().UTC(),
	}
	fields := map[string]string{}

	reconcileLegacyStatusFields(current, result, fields)

	if v, ok := fields["status"]; !ok || v != "" {
		t.Errorf("status = %q (set=%v), want cleared (subscription out_of_credits is benign)", v, ok)
	}
	if v, ok := fields["exhausted_at"]; !ok || v != "" {
		t.Errorf("exhausted_at = %q (set=%v), want cleared", v, ok)
	}
	if v, ok := fields["cooldown_until"]; !ok || v != "" {
		t.Errorf("cooldown_until = %q (set=%v), want cleared", v, ok)
	}
}

// ka-0000003350: a METERED account (explicit billing_type=metered) whose
// rate-limit windows are OK but which is OUT OF BILLING CREDITS
// (overage_status="rejected", reason="out_of_credits") must NOT have its
// exhausted flag cleared. On a genuine pay-as-you-go account real inference 429s
// on billing, so clearing here flips it back to active seconds before the next
// 429 re-exhausts it — the active↔exhausted oscillation the reactive hold exists
// to prevent (ka-0000008164). The billing-type gate keeps today's hold for these.
func TestReconcileLegacyStatusFields_MeteredOutOfCreditsDoesNotClearExhausted(t *testing.T) {
	current := map[string]string{
		"status":         "exhausted",
		"exhausted_at":   "2026-07-06T05:00:22Z",
		"cooldown_until": "2026-07-06T06:00:22Z",
		"billing_type":   "metered",
	}
	result := probeResult{
		Status:        "allowed",
		Utilization5h: 0.06,
		Utilization7d: 0.28,
		OverageStatus: "rejected",
		OverageReason: "out_of_credits",
		CheckedAt:     time.Now().UTC(),
	}
	fields := map[string]string{}

	reconcileLegacyStatusFields(current, result, fields)

	if _, ok := fields["status"]; ok {
		t.Errorf("status must not be cleared while metered account is out of credits; got %q", fields["status"])
	}
	if _, ok := fields["exhausted_at"]; ok {
		t.Errorf("exhausted_at must not be cleared while metered account is out of credits; got %q", fields["exhausted_at"])
	}
	if _, ok := fields["cooldown_until"]; ok {
		t.Errorf("cooldown_until must not be cleared while metered account is out of credits; got %q", fields["cooldown_until"])
	}
}

// ka-0000008164: org_spend_cap_reached is a benign steady-state on healthy
// subscription accounts (ka-0000004929) — the fleet primary permanently reports
// overage_status="rejected" with that reason while serving 200s. It must still
// clear exhausted normally, exactly like an account with no overage rejection,
// or the fix would freeze the healthy primary (the fleet-starvation regression).
func TestReconcileLegacyStatusFields_OrgSpendCapClearsExhausted(t *testing.T) {
	current := map[string]string{
		"status":         "exhausted",
		"exhausted_at":   "2026-07-06T04:00:00Z",
		"cooldown_until": "2026-07-06T05:00:00Z",
	}
	result := probeResult{
		Status:        "allowed",
		Utilization7d: 0.05,
		OverageStatus: "rejected",
		OverageReason: "org_spend_cap_reached",
		CheckedAt:     time.Now().UTC(),
	}
	fields := map[string]string{}

	reconcileLegacyStatusFields(current, result, fields)

	if v, ok := fields["status"]; !ok || v != "" {
		t.Errorf("status = %q (set=%v), want cleared to empty (org_spend_cap_reached is benign)", v, ok)
	}
	if v, ok := fields["exhausted_at"]; !ok || v != "" {
		t.Errorf("exhausted_at = %q (set=%v), want cleared", v, ok)
	}
	if v, ok := fields["cooldown_until"]; !ok || v != "" {
		t.Errorf("cooldown_until = %q (set=%v), want cleared", v, ok)
	}
}

// ka-0000008164: the normal rate-limit reset path is unaffected — an account
// with no overage rejection at all clears exhausted on recovery as before.
func TestReconcileLegacyStatusFields_NormalResetUnaffectedByGuard(t *testing.T) {
	current := map[string]string{
		"status":       "exhausted",
		"exhausted_at": "2026-07-06T04:00:00Z",
	}
	// Overage allowed — the account fully recovered.
	result := probeResult{
		Status:        "allowed",
		Utilization7d: 0.05,
		OverageStatus: "allowed",
		OverageReason: "",
		CheckedAt:     time.Now().UTC(),
	}
	fields := map[string]string{}

	reconcileLegacyStatusFields(current, result, fields)

	if v, ok := fields["status"]; !ok || v != "" {
		t.Errorf("status = %q (set=%v), want cleared on normal reset", v, ok)
	}
	if v, ok := fields["exhausted_at"]; !ok || v != "" {
		t.Errorf("exhausted_at = %q (set=%v), want cleared on normal reset", v, ok)
	}
}

func TestOverageOutOfCredits(t *testing.T) {
	cases := []struct {
		name          string
		billingType   string
		overageStatus string
		overageReason string
		want          bool
	}{
		// Billing-type gate (ka-0000003350): the hold fires ONLY for metered.
		{"metered out of credits", "metered", "rejected", "out_of_credits", true},
		{"subscription out of credits is benign", "subscription", "rejected", "out_of_credits", false},
		// Same-shape header dimensions, holding billing_type=metered fixed.
		{"metered org spend cap is benign", "metered", "rejected", "org_spend_cap_reached", false},
		{"metered overage allowed", "metered", "allowed", "", false},
		{"metered empty overage status", "metered", "", "", false},
		// Reason without a rejected status is not a billing block.
		{"metered out_of_credits reason but allowed status", "metered", "allowed", "out_of_credits", false},
		{"metered rejected but unknown reason", "metered", "rejected", "some_new_reason", false},
		{"metered rejected but empty reason", "metered", "rejected", "", false},
		// Subscription never holds regardless of header shape.
		{"subscription org spend cap is benign", "subscription", "rejected", "org_spend_cap_reached", false},
		{"subscription overage allowed", "subscription", "allowed", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := overageOutOfCredits(c.billingType, c.overageStatus, c.overageReason); got != c.want {
				t.Errorf("overageOutOfCredits(%q, %q, %q) = %v, want %v",
					c.billingType, c.overageStatus, c.overageReason, got, c.want)
			}
		})
	}
}

// ka-0000003350: billing_type defaults to subscription when the field is empty
// or unset — the load-bearing default that keeps the entire pre-existing fleet
// (whose account beads carry no billing_type) out of the reactive hold.
func TestBillingType_DefaultsToSubscription(t *testing.T) {
	cases := []struct {
		name   string
		fields map[string]string
		want   string
	}{
		{"nil fields", nil, "subscription"},
		{"empty map", map[string]string{}, "subscription"},
		{"empty string value", map[string]string{"billing_type": ""}, "subscription"},
		{"explicit subscription", map[string]string{"billing_type": "subscription"}, "subscription"},
		{"explicit metered", map[string]string{"billing_type": "metered"}, "metered"},
		{"other fields present, no billing_type", map[string]string{"status": "active"}, "subscription"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := billingType(c.fields); got != c.want {
				t.Errorf("billingType(%v) = %q, want %q", c.fields, got, c.want)
			}
		})
	}
}
