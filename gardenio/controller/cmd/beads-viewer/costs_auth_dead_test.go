package main

import (
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// authDeadBead builds the account bead as the controller probe leaves it after
// a hard 403: probe_auth_status=rejected, while throttle_status / utilization /
// throttle_reset_at stay FROZEN at their last-good values because the 401/403
// path early-returns before refreshing them (ratelimit_probe.go). Mirrors the
// live matthewbaker1110 state on 2026-08-02.
func authDeadBead() *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:    "ka-0000006759",
		Title: "matthewbaker1110",
		Fields: map[string]string{
			"provider":                "anthropic",
			"status":                  "active",
			"throttle_status":         "allowed",
			"probe_auth_status":       "rejected",
			"utilization_5h":          "0.27",
			"utilization_7d":          "0.63",
			"overage_status":          "rejected",
			"overage_disabled_reason": "out_of_credits",
			"probe_checked_at":        time.Now().Add(-7 * 24 * time.Hour).UTC().Format(time.RFC3339),
		},
	}
}

// TestBuildCostAccountView_AuthDeadNotRenderedActive asserts the /costs card
// for an auth-dead credential does not present it as serviceable capacity.
// Before the fix the card read status "active" with throttle label "allowed"
// and 73% of the 5h window free, for an account 403ing every request.
func TestBuildCostAccountView_AuthDeadNotRenderedActive(t *testing.T) {
	v := buildCostAccountView(authDeadBead(), time.Now().UTC())

	if !v.IsAuthDead {
		t.Error("expected IsAuthDead to be set from probe_auth_status=rejected")
	}
	if v.ThrottleLabel != "auth-dead" {
		t.Errorf("ThrottleLabel = %q, want %q (frozen 'allowed' must not show)", v.ThrottleLabel, "auth-dead")
	}
	if v.IsActive {
		t.Error("auth-dead account must not render as active")
	}
	// IsThrottled drives both the card chip and inclusion in the Throttled
	// panel; an auth-dead account is never throttle_status=="rejected", so
	// without this it is silently absent from the panel.
	if !v.IsThrottled {
		t.Error("auth-dead account must count as throttled so the Throttled panel surfaces it")
	}
}

// TestBuildCostsThrottled_IncludesAuthDead asserts the Throttled panel — the
// operator's at-a-glance list of accounts that cannot serve traffic — includes
// the auth-dead account. It previously filtered on throttle_status alone, so
// the one account most needing attention was the only one omitted.
func TestBuildCostsThrottled_IncludesAuthDead(t *testing.T) {
	now := time.Now().UTC()
	accounts := []costAccountView{
		buildCostAccountView(authDeadBead(), now),
		buildCostAccountView(&beadsapi.BeadDetail{
			ID: "gd-healthy", Title: "gardenio",
			Fields: map[string]string{
				"status": "active", "throttle_status": "allowed",
				"probe_auth_status": "ok", "utilization_5h": "0.43",
			},
		}, now),
	}

	throttled := buildCostsThrottled(accounts)

	var found bool
	for _, r := range throttled {
		if r.Name == "matthewbaker1110" {
			found = true
		}
		if r.Name == "gardenio" {
			t.Error("healthy account must not appear in the Throttled panel")
		}
	}
	if !found {
		t.Errorf("auth-dead account missing from Throttled panel, got %d rows", len(throttled))
	}
}

// TestBuildCostsHottestUtilization_ExcludesAuthDead asserts the Hottest panel
// drops auth-dead accounts. That panel is the leading indicator of an imminent
// throttle, and an auth-dead account's utilization is frozen at its last good
// probe — here a week old. Left in, a stale 27% can outrank a genuinely hot
// account and misdirect attention.
func TestBuildCostsHottestUtilization_ExcludesAuthDead(t *testing.T) {
	now := time.Now().UTC()
	accounts := []costAccountView{
		buildCostAccountView(authDeadBead(), now),
		buildCostAccountView(&beadsapi.BeadDetail{
			ID: "gd-hot", Title: "gardenio",
			Fields: map[string]string{
				"status": "active", "throttle_status": "allowed",
				"probe_auth_status": "ok", "utilization_5h": "0.43",
			},
		}, now),
	}

	hottest := buildCostsHottestUtilization(accounts, 5)

	for _, r := range hottest {
		if r.Name == "matthewbaker1110" {
			t.Error("auth-dead account must not appear in Hottest — its utilization is frozen/stale")
		}
	}
	if len(hottest) != 1 || hottest[0].Name != "gardenio" {
		t.Errorf("expected only the live account in Hottest, got %+v", hottest)
	}
}

// TestBuildCostAccountView_HealthyUnaffected guards against over-reach: an
// account with a clean probe must keep its real throttle label and active
// status, and must not be condemned as auth-dead.
func TestBuildCostAccountView_HealthyUnaffected(t *testing.T) {
	v := buildCostAccountView(&beadsapi.BeadDetail{
		ID: "ka-0000006686", Title: "gardenio",
		Fields: map[string]string{
			"provider": "anthropic", "status": "active",
			"throttle_status": "allowed", "probe_auth_status": "ok",
			"utilization_5h": "0.43", "utilization_7d": "0.05",
			"probe_checked_at": time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339),
		},
	}, time.Now().UTC())

	if v.IsAuthDead {
		t.Error("healthy account must not be flagged auth-dead")
	}
	if v.ThrottleLabel != "allowed" {
		t.Errorf("ThrottleLabel = %q, want %q", v.ThrottleLabel, "allowed")
	}
	if !v.IsActive {
		t.Error("healthy account should remain active")
	}
	if v.IsThrottled {
		t.Error("healthy account should not be marked throttled")
	}
}

// TestBuildCostAccountView_GenuineThrottleStillWorks asserts the pre-existing
// throttle path is untouched — a real throttle_status=="rejected" account keeps
// its own label rather than being relabelled auth-dead.
func TestBuildCostAccountView_GenuineThrottleStillWorks(t *testing.T) {
	v := buildCostAccountView(&beadsapi.BeadDetail{
		ID: "gd-p6dbwciQ2i", Title: "fimbaz",
		Fields: map[string]string{
			"provider": "anthropic", "status": "exhausted",
			"throttle_status": "rejected", "probe_auth_status": "ok",
			"utilization_7d": "1.0",
		},
	}, time.Now().UTC())

	if v.IsAuthDead {
		t.Error("a throttled-but-authorized account is not auth-dead")
	}
	if v.ThrottleLabel != "rejected" {
		t.Errorf("ThrottleLabel = %q, want %q", v.ThrottleLabel, "rejected")
	}
	if !v.IsThrottled {
		t.Error("throttle_status=rejected must still mark the account throttled")
	}
}

// TestBuildCostAccountView_BlankStatusIsActive asserts an account bead whose
// "status" field a spend-sync tick cleared to "" still renders as active — per
// the controller's own usable-status check (account_spread.go selectAccount:
// `acct.Status != "" && acct.Status != "active"` excludes), "" is active by
// default, not absent. Before this fix the fleet summary's ActiveCount undercounted
// every account whose status field was blank rather than the literal string
// "active", even though it was actively serving traffic (observed on the live
// "fimbaz" account 2026-09-22: real spend tracked, status field blank).
func TestBuildCostAccountView_BlankStatusIsActive(t *testing.T) {
	v := buildCostAccountView(&beadsapi.BeadDetail{
		ID: "gd-2bZJkQ8VZR", Title: "fimbaz",
		Fields: map[string]string{
			"provider": "anthropic", "status": "",
			"throttle_status": "allowed", "probe_auth_status": "ok",
		},
	}, time.Now().UTC())

	if !v.IsActive {
		t.Error("blank status field must render as active, matching the controller's usable-status check")
	}
}
