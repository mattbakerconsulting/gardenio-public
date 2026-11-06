package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// acct is a terse builder for a project-pool AccountCacheEntry in tests.
func acct(litellmKey, status, throttle string, labels ...string) config.AccountCacheEntry {
	return config.AccountCacheEntry{
		LiteLLMKey:     litellmKey,
		Status:         status,
		ThrottleStatus: throttle,
		Labels:         labels,
	}
}

// exclusiveAcct is acct plus an exclusive_to_role reservation, for testing the
// data-driven account-exclusivity partition (ka-0000005223).
func exclusiveAcct(litellmKey, status, exclusiveToRole string, labels ...string) config.AccountCacheEntry {
	e := acct(litellmKey, status, "", labels...)
	e.ExclusiveToRole = exclusiveToRole
	return e
}

// TestSelectSpreadAccount_DistributesAcrossHealthyAccounts verifies that, given
// equal load, selection is deterministic (alphabetical) and that as load
// accrues on the chosen account the next pick rotates to the next least-loaded.
func TestSelectSpreadAccount_DistributesAcrossHealthyAccounts(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-a": acct("sk-a", "active", "", "project:gardenio"),
			"acct-b": acct("sk-b", "active", "", "project:gardenio"),
			"acct-c": acct("sk-c", "", "", "project:gardenio"), // empty status == active
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio"}

	// Simulate spawning 6 pods, threading live counts forward as the spawn
	// path would (optimistic bump). Expect an even 2/2/2 spread.
	live := map[string]int{}
	got := map[string]int{}
	for i := 0; i < 6; i++ {
		name := selectAccount(cfg, "gardenio", spec, live)
		if name == "" {
			t.Fatalf("iteration %d: expected an account, got empty", i)
		}
		live[name]++
		got[name]++
	}
	for _, name := range []string{"acct-a", "acct-b", "acct-c"} {
		if got[name] != 2 {
			t.Errorf("expected even spread (2 each), got %v", got)
			break
		}
	}
}

// TestSelectSpreadAccount_PicksLeastLoaded verifies the core least-loaded rule
// with a pre-existing skewed load and the alphabetical tie-break.
func TestSelectSpreadAccount_PicksLeastLoaded(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-a": acct("sk-a", "active", "", "project:gardenio"),
			"acct-b": acct("sk-b", "active", "", "project:gardenio"),
			"acct-c": acct("sk-c", "active", "", "project:gardenio"),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio"}

	tests := []struct {
		name string
		live map[string]int
		want string
	}{
		{"all empty → alphabetical first", map[string]int{}, "acct-a"},
		{"a loaded → pick b", map[string]int{"acct-a": 3}, "acct-b"},
		{"a,b loaded → pick c", map[string]int{"acct-a": 2, "acct-b": 2}, "acct-c"},
		{"tie at 1 → alphabetical", map[string]int{"acct-a": 1, "acct-b": 1, "acct-c": 1}, "acct-a"},
		{"c least loaded", map[string]int{"acct-a": 5, "acct-b": 4, "acct-c": 1}, "acct-c"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := selectAccount(cfg, "gardenio", spec, tc.live); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSelectSpreadAccount_SkipsRejectedAndExhausted verifies that rejected
// (live-throttled) and exhausted/cooldown accounts are excluded from the pool.
func TestSelectSpreadAccount_SkipsRejectedAndExhausted(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-rejected":  acct("sk-r", "active", "rejected", "project:gardenio"),
			"acct-exhausted": acct("sk-e", "exhausted", "", "project:gardenio"),
			"acct-cooldown":  acct("sk-cd", "cooldown", "", "project:gardenio"),
			"acct-healthy":   acct("sk-h", "active", "", "project:gardenio"),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio"}
	// Even if the healthy account is the most loaded, it is the only eligible
	// candidate.
	if got := selectAccount(cfg, "gardenio", spec, map[string]int{"acct-healthy": 99}); got != "acct-healthy" {
		t.Fatalf("expected acct-healthy (only eligible), got %q", got)
	}
}

// TestSelectSpreadAccount_SkipsProbeAuthRejected is the probe-gate regression
// for ka-0000002085: an account that reads pool-healthy on the rate-limit axis
// (Status active, throttle allowed, no overage) but whose read-only probe got a
// 401/403 (probe_auth_status="rejected" — org disabled Claude Code access, the
// matthewbaker1110 hard error) must NEVER be a spread/switch target, even when
// it is the least-loaded account. Pool status is not sufficient.
func TestSelectSpreadAccount_SkipsProbeAuthRejected(t *testing.T) {
	authDead := acct("sk-dead", "active", "allowed", "project:gardenio")
	authDead.ProbeAuthStatus = "rejected"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-authdead": authDead,
			"acct-healthy":  acct("sk-h", "active", "allowed", "project:gardenio"),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio"}
	// authdead is idle (0 pods) and would win least-loaded; healthy is loaded.
	// The gate must still steer to the healthy account.
	if got := selectAccount(cfg, "gardenio", spec, map[string]int{"acct-healthy": 99, "acct-authdead": 0}); got != "acct-healthy" {
		t.Fatalf("probe-auth-rejected account must be excluded; expected acct-healthy, got %q", got)
	}
	// With ONLY an auth-dead account, spread returns "" so the caller falls back
	// rather than baking a dead credential.
	cfgOnlyDead := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{"acct-authdead": authDead},
	}
	if got := selectAccount(cfgOnlyDead, "gardenio", spec, nil); got != "" {
		t.Fatalf("expected \"\" when the only account is probe-auth-rejected, got %q", got)
	}
}

// TestSelectSpreadAccount_SkipsKeylessButKeepsSubscriptionOverage verifies that
// a keyless LiteLLM-proxied account (401) is excluded from the spread pool, but
// accounts whose ONLY flag is a subscription-overage reason
// (org_spend_cap_reached or out_of_credits) stay eligible. Per ka-0000004929,
// out_of_credits on a Max OAuth token only means pay-as-you-go overage billing
// is off — the subscription quota still bills fine (proven: HTTP 200 from the
// box egress with that exact header set). Treating it as a hard block was
// starving the GIB box of any usable account, so agents spawned with no
// ANTHROPIC_* env and died "Not logged in".
func TestSelectSpreadAccount_SkipsKeylessButKeepsSubscriptionOverage(t *testing.T) {
	const litellm = "https://litellm.app.devops.acme.example"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			// Keyless LiteLLM account — would 401 (this was acme-shared). Excluded.
			"acct-keyless": {BaseURL: litellm, LiteLLMKey: "", Status: "", ThrottleStatus: "allowed", Labels: []string{"project:gardenio"}},
			// out_of_credits (subscription overage off) — usable; MUST stay in pool.
			"acct-no-credits": {BaseURL: litellm, LiteLLMKey: "sk-x", Status: "active", ThrottleStatus: "allowed", OverageDisabledReason: "out_of_credits", Labels: []string{"project:gardenio"}},
			// Spend-cap only — usable for us; MUST stay in the pool (this was gardenio/2/3).
			"acct-spend-cap": {BaseURL: litellm, LiteLLMKey: "sk-y", Status: "active", ThrottleStatus: "allowed", OverageDisabledReason: "org_spend_cap_reached", Labels: []string{"project:gardenio"}},
			// A clean healthy account.
			"acct-healthy": {BaseURL: litellm, LiteLLMKey: "sk-h", Status: "active", ThrottleStatus: "allowed", OverageDisabledReason: "", Labels: []string{"project:gardenio"}},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio"}

	// Only the keyless account is excluded. Load every usable account heavily
	// except acct-no-credits → spread must pick it, proving an out_of_credits
	// subscription account stays in rotation.
	if got := selectAccount(cfg, "gardenio", spec, map[string]int{"acct-healthy": 50, "acct-spend-cap": 50, "acct-no-credits": 1}); got != "acct-no-credits" {
		t.Fatalf("expected acct-no-credits (usable, least-loaded), got %q", got)
	}
	// The keyless account is never picked even when least-loaded.
	if got := selectAccount(cfg, "gardenio", spec, map[string]int{"acct-healthy": 50, "acct-spend-cap": 50, "acct-no-credits": 50, "acct-keyless": 0}); got == "acct-keyless" {
		t.Fatalf("keyless account must never be selected, got %q", got)
	}

	// With only a keyless account, spread returns "" so the caller falls back.
	// An out_of_credits account, by contrast, would be usable.
	cfgNoneUsable := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-keyless": {BaseURL: litellm, LiteLLMKey: "", Status: "", Labels: []string{"project:gardenio"}},
		},
	}
	if got := selectAccount(cfgNoneUsable, "gardenio", spec, nil); got != "" {
		t.Fatalf("expected \"\" when no usable account exists, got %q", got)
	}
}

// TestOverageReasonBlocks pins the operator corrections (ka-0000003769 +
// ka-0000004929) and the 2026-08-28 fleet outage that generalized them: the
// overage-disabled-reason header describes pay-as-you-go overage billing, which
// is irrelevant to a subscription (Max OAuth) account. For subscription billing
// NO reason blocks — including reasons we have never seen before, which is the
// whole point: the previous allowlist blocked on the unknown value
// org_level_disabled and emptied every project pool. Metered accounts keep the
// conservative behavior, since there the overage path IS the billing path.
func TestOverageReasonBlocks(t *testing.T) {
	subscriptionCases := map[string]bool{
		"":                         false,
		"org_spend_cap_reached":    false,
		"out_of_credits":           false,
		"org_level_disabled":       false,
		"org_level_disabled_until": false,
		"some_future_reason":       false,
	}
	for reason, want := range subscriptionCases {
		// Explicit subscription and the empty default must agree.
		for _, billing := range []string{billingTypeSubscription, ""} {
			if got := overageReasonBlocks(billing, reason); got != want {
				t.Errorf("overageReasonBlocks(%q, %q) = %v, want %v", billing, reason, got, want)
			}
		}
	}

	meteredCases := map[string]bool{
		"":                      false,
		"out_of_credits":        true,
		"org_spend_cap_reached": true,
		"org_level_disabled":    true,
		"some_future_reason":    true,
	}
	for reason, want := range meteredCases {
		if got := overageReasonBlocks(billingTypeMetered, reason); got != want {
			t.Errorf("overageReasonBlocks(metered, %q) = %v, want %v", reason, got, want)
		}
	}
}

// TestSelectSpreadAccount_OrgLevelDisabledStaysInPool reproduces the 2026-08-28
// fleet-wide spawn outage. Five accounts were genuinely 7-day exhausted; the two
// that still held capacity (fimbaz, matthewbaker1110 — throttle_status=allowed,
// utilization_5h=0, probe_auth_status=ok) carried the newly-observed
// overage_disabled_reason values org_level_disabled / org_level_disabled_until.
// The old string allowlist treated those as blocking, so selectAccount returned
// "" and applyAccountConfig SpawnBlocked every agent in every project. Because
// the reason string does not change on a rate-limit reset, the block would never
// have self-healed.
func TestSelectSpreadAccount_OrgLevelDisabledStaysInPool(t *testing.T) {
	const litellm = "https://litellm.app.devops.acme.example"
	labels := []string{"project:gardenio"}
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio":  {BaseURL: litellm, LiteLLMKey: "sk-1", Status: "exhausted", ThrottleStatus: "rejected", OverageDisabledReason: "org_spend_cap_reached", Labels: labels},
			"gardenio2": {BaseURL: litellm, LiteLLMKey: "sk-2", Status: "exhausted", ThrottleStatus: "rejected", OverageDisabledReason: "org_spend_cap_reached", Labels: labels},
			"fimbaz":    {BaseURL: litellm, LiteLLMKey: "sk-3", Status: "", ThrottleStatus: "allowed", ProbeAuthStatus: "ok", OverageDisabledReason: "org_level_disabled", Labels: labels},
			"mb1110":    {BaseURL: litellm, LiteLLMKey: "sk-4", Status: "", ThrottleStatus: "allowed", ProbeAuthStatus: "ok", OverageDisabledReason: "org_level_disabled_until", Labels: labels},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio"}

	got := selectAccount(cfg, "gardenio", spec, map[string]int{"fimbaz": 5, "mb1110": 0})
	if got != "mb1110" {
		t.Fatalf("expected the least-loaded org_level_disabled account (mb1110), got %q — the pool must not be emptied by an org-level overage switch", got)
	}
	if got := selectAccount(cfg, "gardenio", spec, map[string]int{"fimbaz": 0, "mb1110": 5}); got != "fimbaz" {
		t.Fatalf("expected fimbaz when it is least-loaded, got %q", got)
	}
	// The genuinely exhausted accounts stay excluded.
	for _, name := range []string{"gardenio", "gardenio2"} {
		if isHealthyAuthorized(cfg.AccountCache[name]) {
			t.Errorf("%s is status=exhausted/throttle-rejected and must stay excluded", name)
		}
	}
	// A metered account with a disabled overage reason is still excluded.
	metered := config.AccountCacheEntry{BaseURL: litellm, LiteLLMKey: "sk-5", Status: "", ThrottleStatus: "allowed", BillingType: "metered", OverageDisabledReason: "out_of_credits", Labels: labels}
	if isHealthyAuthorized(metered) {
		t.Error("a metered out_of_credits account must stay excluded — there the overage path IS the billing path")
	}
}

// TestSelectSpreadAccount_WarningIsDeprioritizedNotExcluded verifies the
// weighted-rotation behavior (operator report 2026-06-29, gd-B9hsh88IFB): an
// allowed_warning account stays in the spread pool as real, de-prioritized
// capacity rather than being excluded whenever any fully-healthy account
// exists. A fully-healthy account wins at low load, but once it carries
// warningSpreadPenalty more pods than an idle warning account, new pods spread
// onto the warning account instead of re-saturating the healthy one.
func TestSelectSpreadAccount_WarningIsDeprioritizedNotExcluded(t *testing.T) {
	spec := &podmanager.AgentPodSpec{Project: "gardenio"}

	cfgMixed := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-healthy": acct("sk-h", "active", "", "project:gardenio"),
			"acct-warn":    acct("sk-w", "active", "allowed_warning", "project:gardenio"),
		},
	}

	// At low load the fully-healthy account is preferred even though both are
	// idle (warning carries the penalty).
	if got := selectAccount(cfgMixed, "gardenio", spec, map[string]int{}); got != "acct-healthy" {
		t.Errorf("idle pool: expected fully-healthy acct-healthy, got %q", got)
	}

	// Just below the penalty threshold: healthy still wins.
	if got := selectAccount(cfgMixed, "gardenio", spec, map[string]int{"acct-healthy": warningSpreadPenalty - 1}); got != "acct-healthy" {
		t.Errorf("healthy at penalty-1 pods: expected acct-healthy to still win, got %q", got)
	}

	// Once the healthy account carries penalty+1 pods (and the warning account
	// is idle), the de-prioritized warning account is the least-loaded pick —
	// this is the operator's "use matthewbaker1110 too" case.
	if got := selectAccount(cfgMixed, "gardenio", spec, map[string]int{"acct-healthy": warningSpreadPenalty + 1}); got != "acct-warn" {
		t.Errorf("healthy heavily loaded, warning idle: expected acct-warn to enter rotation, got %q", got)
	}

	// Only warning accounts → least-loaded warning account (penalty applies
	// equally, so it cancels out and the raw live count decides).
	cfgWarnOnly := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-w1": acct("sk-w1", "active", "allowed_warning", "project:gardenio"),
			"acct-w2": acct("sk-w2", "active", "allowed_warning", "project:gardenio"),
		},
	}
	if got := selectAccount(cfgWarnOnly, "gardenio", spec, map[string]int{"acct-w1": 3}); got != "acct-w2" {
		t.Errorf("warning-only pool: expected least-loaded acct-w2, got %q", got)
	}
}

// TestSelectSpreadAccount_RolePinWins verifies a role-pinned account is returned
// directly and is NOT spread.
func TestSelectSpreadAccount_RolePinWins(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-a": acct("sk-a", "active", "", "project:gardenio"),
			"acct-b": acct("sk-b", "active", "", "project:gardenio"),
			"pinned": acct("sk-pin", "active", "", "project:gardenio"),
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"special": {Account: "pinned"},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "special"}
	if got := selectAccount(cfg, "gardenio", spec, map[string]int{"pinned": 100}); got != "pinned" {
		t.Fatalf("role pin must win even when heavily loaded, got %q", got)
	}
}

// TestSelectSpreadAccount_ExclusiveOwnerRoleNotSpread verifies a role that owns
// an exclusive account (exclusive_to_role) is never spread — selectAccount
// returns "" so the caller falls through to the role-pin path. This is the
// data-driven replacement for the old fsm-triage hardcode (ka-0000005223).
func TestSelectSpreadAccount_ExclusiveOwnerRoleNotSpread(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-a":          acct("sk-a", "active", "", "project:gardenio"),
			"litellm-bedrock": exclusiveAcct("sk-bedrock", "active", "fsm-triage", "project:gardenio"),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "fsm-triage"}
	if got := selectAccount(cfg, "gardenio", spec, map[string]int{}); got != "" {
		t.Fatalf("exclusive-owner role must not be spread, got %q", got)
	}
}

// TestSelectSpreadAccount_NeverSpreadsOntoExclusiveAccount verifies an exclusive
// account is excluded from a non-owner agent's pool even when it is
// project-labeled and the only lightly-loaded option.
func TestSelectSpreadAccount_NeverSpreadsOntoExclusiveAccount(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"litellm-bedrock": exclusiveAcct("sk-bedrock", "active", "fsm-triage", "project:gardenio"),
			"acct-a":          acct("sk-a", "active", "", "project:gardenio"),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew"}
	if got := selectAccount(cfg, "gardenio", spec, map[string]int{"acct-a": 50}); got != "acct-a" {
		t.Fatalf("exclusive account must never be a non-owner spread candidate, got %q", got)
	}
}

// TestSelectSpreadAccount_ExclusivePoolUnaffectedForOtherRoles is a focused
// check that a NEW exclusive account (no controller change) automatically
// partitions: a role unrelated to the exclusivity spreads only over the
// non-exclusive pool, and the exclusive account is invisible to it.
func TestSelectSpreadAccount_ExclusivePoolUnaffectedForOtherRoles(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-a":      acct("sk-a", "active", "", "project:gardenio"),
			"acct-b":      acct("sk-b", "active", "", "project:gardenio"),
			"vip-account": exclusiveAcct("sk-vip", "active", "vip-role", "project:gardenio"),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew"}
	// Even when the exclusive account is unloaded and the others are heavily
	// loaded, a non-owner never selects it.
	for i := 0; i < 5; i++ {
		if got := selectAccount(cfg, "gardenio", spec, map[string]int{"acct-a": 99, "acct-b": 99}); got == "vip-account" {
			t.Fatalf("non-owner role must never spread onto exclusive vip-account, got %q", got)
		}
	}
}

// TestSelectSpreadAccount_HaikuProbeOpusDefaultEligible locks in the gd-xz4KrTJSs7
// unwind of the old divergent-probe guard (formerly gd-pymvKs6XUp). The guard
// excluded any account whose probe_model differed from its default Model unless
// it carried a spread-divergent-probe-ok opt-in label. But that shape — a cheap
// haiku probe over an opus default on the SAME Claude Max OAuth subscription — is
// the universal shape of every gardenio Opus account: haiku and opus share one
// entitlement/rate-limit bucket, so a green haiku probe genuinely proves the opus
// default is healthy. The fail-closed guard therefore excluded the WHOLE pool
// whenever the opt-in label was missing, collapsing spread to a single cached
// account (the gd-JB5txU69bq outage). The real fable footgun it was built for —
// a genuinely un-entitled NAMED default model labeled into a project pool — is a
// label-hygiene problem fixed by not labeling such accounts (gd-AmiXcco6lc
// de-labeled fable directly). This test asserts the haiku/opus shape is now an
// ordinary eligible candidate with no opt-in label required.
func TestSelectSpreadAccount_HaikuProbeOpusDefaultEligible(t *testing.T) {
	spec := &podmanager.AgentPodSpec{Project: "gardenio"}

	// The universal gardenio shape: an opus default model with no opt-in label.
	// Pre-unwind, the haiku-probe/opus-default divergence excluded this unless
	// labeled; post-unwind it is selected purely on label + health.
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-haiku-probe": {
				LiteLLMKey: "sk-hp", Status: "active", ThrottleStatus: "",
				Model:  "claude-opus-4-8-oauth",
				Labels: []string{"project:gardenio"},
			},
		},
	}
	if got := selectAccount(cfg, "gardenio", spec, map[string]int{}); got != "acct-haiku-probe" {
		t.Fatalf("haiku-probe/opus-default account must be eligible post-unwind, got %q", got)
	}

	// A whole pool of this shape spreads evenly — no collapse to a single
	// account, which was the regression the guard caused in production.
	cfgPool := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-a": {
				LiteLLMKey: "sk-a", Status: "active", ThrottleStatus: "",
				Model:  "claude-opus-4-8-oauth",
				Labels: []string{"project:gardenio"},
			},
			"acct-b": {
				LiteLLMKey: "sk-b", Status: "active", ThrottleStatus: "",
				Model:  "claude-opus-4-8-oauth",
				Labels: []string{"project:gardenio"},
			},
		},
	}
	live := map[string]int{}
	got := map[string]int{}
	for i := 0; i < 4; i++ {
		name := selectAccount(cfgPool, "gardenio", spec, live)
		if name == "" {
			t.Fatalf("iteration %d: expected an account, got empty (pool collapsed)", i)
		}
		live[name]++
		got[name]++
	}
	if got["acct-a"] != 2 || got["acct-b"] != 2 {
		t.Fatalf("expected even 2/2 spread across the haiku-probe pool, got %v", got)
	}
}

// TestSelectSpreadAccount_FallbackWhenPoolEmpty verifies "" is returned (clean
// fall-through to the legacy path) when the project has no healthy labeled
// account.
func TestSelectSpreadAccount_FallbackWhenPoolEmpty(t *testing.T) {
	spec := &podmanager.AgentPodSpec{Project: "gardenio"}

	tests := []struct {
		name string
		cfg  *config.Config
	}{
		{"no accounts at all", &config.Config{AccountCache: map[string]config.AccountCacheEntry{}}},
		{"accounts labeled for other projects", &config.Config{AccountCache: map[string]config.AccountCacheEntry{
			"other": acct("sk-o", "active", "", "project:monorepo"),
		}}},
		{"all project accounts unhealthy", &config.Config{AccountCache: map[string]config.AccountCacheEntry{
			"ex":  acct("sk-e", "exhausted", "", "project:gardenio"),
			"rej": acct("sk-r", "active", "rejected", "project:gardenio"),
		}}},
		{"empty project", &config.Config{AccountCache: map[string]config.AccountCacheEntry{
			"acct-a": acct("sk-a", "active", "", "project:gardenio"),
		}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			project := "gardenio"
			if tc.name == "empty project" {
				project = ""
				spec.Project = ""
			} else {
				spec.Project = "gardenio"
			}
			if got := selectAccount(tc.cfg, project, spec, map[string]int{}); got != "" {
				t.Errorf("expected empty (fall-through), got %q", got)
			}
		})
	}
}

// TestSelectSpreadAccount_PrimaryMarkerDoesNotPin is the load-bearing
// abolish-primary Phase 4 invariant (ka-0000005978): a "primary account" marker
// can no longer pin spawns. selectAccount ranks over cfg.AccountCache,
// whose AccountCacheEntry carries no primary field at all, so even if one
// account in a project pool is the operator's designated primary, new pods still
// spread evenly across the whole healthy pool. This is what demotes the primary
// concept from a privileged home to (at most) an empty-pool tiebreaker resolved
// elsewhere (buildProjectAccountOverride). If a future change ever routed the
// marker back into the spread path, this test fails.
func TestSelectSpreadAccount_PrimaryMarkerDoesNotPin(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			// Even if "acct-a" were the marked primary, the cache entry cannot
			// express it — proving the spread path is structurally primary-blind.
			"acct-a": acct("sk-a", "active", "", "project:gardenio"),
			"acct-b": acct("sk-b", "active", "", "project:gardenio"),
			"acct-c": acct("sk-c", "active", "", "project:gardenio"),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio"}

	live := map[string]int{}
	got := map[string]int{}
	for i := 0; i < 9; i++ {
		name := selectAccount(cfg, "gardenio", spec, live)
		if name == "" {
			t.Fatalf("iteration %d: expected an account, got empty", i)
		}
		live[name]++
		got[name]++
	}
	// Even 3/3/3 spread — no account (marked or otherwise) is preferred.
	for _, name := range []string{"acct-a", "acct-b", "acct-c"} {
		if got[name] != 3 {
			t.Errorf("primary marker must not pin: expected even 3/3/3 spread, got %v", got)
			break
		}
	}
}

// TestApplyAccountConfig_SpreadWinsOverPrimaryHome asserts the full-path Phase 4
// behavior (ka-0000005978): when a project's spread pool is healthy,
// applyAccountConfig resolves via selectAccount and never consults the
// primary/last-resort home in ProjectCache[project].Account. Here ProjectCache
// names "primary-home" (the demoted buildProjectAccountOverride pick), but a
// healthy spread pool means the resolved account comes from the pool instead.
func TestApplyAccountConfig_SpreadWinsOverPrimaryHome(t *testing.T) {
	cfg := &config.Config{
		// ProjectCache.Account is the empty-pool last-resort home (what
		// buildProjectAccountOverride would compute, primary marker included).
		ProjectCache: map[string]config.ProjectCacheEntry{"p": {Account: "primary-home"}},
		AccountCache: map[string]config.AccountCacheEntry{
			"primary-home": poolAcct("active", "", "project:p"),
			"pool-a":       poolAcct("active", "", "project:p"),
			"pool-b":       poolAcct("active", "", "project:p"),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "p", Role: "crew", Env: map[string]string{}}
	applyAccountConfig(cfg, spec)
	if spec.SpawnBlocked {
		t.Fatalf("healthy pool must not block")
	}
	// selectAccount picks alphabetically-first at zero load among the three
	// pool members — "pool-a" — proving the primary home did NOT win.
	if spec.ResolvedAccount != "pool-a" {
		t.Errorf("spread should win over the primary home: got ResolvedAccount=%q, want pool-a", spec.ResolvedAccount)
	}
}

// TestApplyAccountConfig_EmptyPoolBlocksSpawn is the Phase 2 replacement
// (ka-0000004094, account-routing.md §7) for the old
// EmptyPoolFallsThroughToPrimaryHome guard. Phase 2 deleted the
// ProjectCache[project].Account fall-through: when every labeled pool account is
// exhausted/rejected the spread pool is empty and there is no last-resort home to
// fall through to. Instead, because the project DOES have credentialed account
// beads, applyAccountConfig sets SpawnBlocked with an explicit reason (the loud
// no-credential WARN of ka-0000007343) rather than baking a credential-less pod
// that would produce synthetic turns. The reconciler re-attempts once an account
// recovers. A healthy-but-unlabeled account ("home") is NOT a candidate for any
// selection path and cannot rescue the project.
func TestApplyAccountConfig_EmptyPoolBlocksSpawn(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			// "home" is healthy but is NOT labeled for the project, so no selection
			// path (spread has no unlabeled candidates; no role pin) reaches it.
			"home":   poolAcct("active", "", ""),
			"cooked": poolAcct("exhausted", "rejected", "project:p"),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "p", Role: "crew", Env: map[string]string{}}
	applyAccountConfig(cfg, spec)
	if !spec.SpawnBlocked {
		t.Fatalf("empty spread pool with credentialed-but-gated accounts must block the spawn")
	}
	if spec.SpawnBlockReason == "" {
		t.Error("expected a non-empty SpawnBlockReason")
	}
	if spec.ResolvedAccount != "" {
		t.Errorf("must not record a ResolvedAccount for a blocked spawn, got %q", spec.ResolvedAccount)
	}
}

// TestApplyAccountConfig_PinsBoatAccountForBothResolutionTypes locks in the
// single-resolver contract that lets gb init drop its own FNV resolver
// (ka-0000002572): the controller's applyAccountConfig is now the ONLY place an
// account is resolved, and it must pin spec.Env["BOAT_ACCOUNT"] to the resolved
// account for BOTH resolution types gb init used to compute for itself —
//
//	(1) the project-pool spread pick (selectAccount), and
//	(2) the role-pinned account (resolveRoleAccount),
//
// because gb init no longer resolves anything: it consumes BOAT_ACCOUNT
// verbatim from the pod env the controller baked. If either path failed to pin
// BOAT_ACCOUNT, coop/coopmux registration would report an empty or wrong
// account — the exact divergence the two-resolver split caused before !4740.
func TestApplyAccountConfig_PinsBoatAccountForBothResolutionTypes(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			// Project pool for the spread path. Names sort before the role-pin
			// target so the crew spread deterministically lands on "acct-a".
			"acct-a": poolAcct("active", "", "project:gardenio"),
			"acct-b": poolAcct("active", "", "project:gardenio"),
			// Account pinned by a role (must win over the spread pool for its role).
			"zz-role-pinned": poolAcct("active", "", "project:gardenio"),
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"pinned-role": {Account: "zz-role-pinned"},
		},
	}

	// (1) Project-spread type: no role pin → the least-loaded / alphabetically
	// first pool account is resolved AND pinned into BOAT_ACCOUNT.
	spreadSpec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew", Env: map[string]string{}}
	applyAccountConfig(cfg, spreadSpec)
	if spreadSpec.ResolvedAccount != "acct-a" {
		t.Fatalf("spread pick: ResolvedAccount = %q, want acct-a", spreadSpec.ResolvedAccount)
	}
	if got := spreadSpec.Env["BOAT_ACCOUNT"]; got != "acct-a" {
		t.Errorf("spread pick: BOAT_ACCOUNT = %q, want acct-a (gb init consumes this pin)", got)
	}

	// (2) Role-pin type: the role's pinned account wins over the pool AND is the
	// value pinned into BOAT_ACCOUNT.
	pinSpec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "pinned-role", Env: map[string]string{}}
	applyAccountConfig(cfg, pinSpec)
	if pinSpec.ResolvedAccount != "zz-role-pinned" {
		t.Fatalf("role pin: ResolvedAccount = %q, want zz-role-pinned", pinSpec.ResolvedAccount)
	}
	if got := pinSpec.Env["BOAT_ACCOUNT"]; got != "zz-role-pinned" {
		t.Errorf("role pin: BOAT_ACCOUNT = %q, want zz-role-pinned (gb init consumes this pin)", got)
	}
}

// TestSelectSpreadAccount_MultiProjectLabels verifies an account labeled for
// multiple projects is a candidate for each.
func TestSelectSpreadAccount_MultiProjectLabels(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"shared": acct("sk-s", "active", "", "project:gardenio", "project:monorepo"),
		},
	}
	for _, project := range []string{"gardenio", "monorepo"} {
		spec := &podmanager.AgentPodSpec{Project: project}
		if got := selectAccount(cfg, project, spec, map[string]int{}); got != "shared" {
			t.Errorf("project %q: expected shared, got %q", project, got)
		}
	}
}

// TestLiteLLMKeyFromHeader round-trips litellmHeaderFor and handles the
// multi-line (with x-litellm-tags) and malformed cases.
func TestLiteLLMKeyFromHeader(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   string
	}{
		{"round-trip", litellmHeaderFor("sk-abc"), "sk-abc"},
		{"with tags line", "x-litellm-api-key: Bearer sk-xyz\nx-litellm-tags: agent:foo", "sk-xyz"},
		{"tags line first", "x-litellm-tags: agent:foo\nx-litellm-api-key: Bearer sk-zzz", "sk-zzz"},
		{"no api-key line", "x-litellm-tags: agent:foo", ""},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := liteLLMKeyFromHeader(tc.header); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCountLivePodsByAccount verifies pods are bucketed by the account whose
// LiteLLM key they carry, and that terminal / unrecognized-key pods are skipped.
func TestCountLivePodsByAccount(t *testing.T) {
	accounts := map[string]config.AccountCacheEntry{
		"acct-a": {LiteLLMKey: "sk-a"},
		"acct-b": {LiteLLMKey: "sk-b"},
	}
	pods := []corev1.Pod{
		agentPodWithHeader("a1", litellmHeaderFor("sk-a")),
		agentPodWithHeader("a2", litellmHeaderFor("sk-a")),
		agentPodWithHeader("b1", litellmHeaderFor("sk-b")),
		agentPodWithHeader("unknown", litellmHeaderFor("sk-unmapped")),
		agentPodWithHeader("noheader", ""),
	}
	// A terminal pod carrying sk-a must not be counted.
	terminal := agentPodWithHeader("a-dead", litellmHeaderFor("sk-a"))
	terminal.Status.Phase = corev1.PodSucceeded
	pods = append(pods, terminal)

	counts := countLivePodsByAccount(pods, accounts)
	if counts["acct-a"] != 2 {
		t.Errorf("acct-a: got %d, want 2", counts["acct-a"])
	}
	if counts["acct-b"] != 1 {
		t.Errorf("acct-b: got %d, want 1", counts["acct-b"])
	}
	if len(counts) != 2 {
		t.Errorf("unexpected extra buckets: %v", counts)
	}
}

// TestRefreshAccountLiveCounts verifies the end-to-end refresh wires the pod
// list through to the cfg snapshot.
func TestRefreshAccountLiveCounts(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-a": {LiteLLMKey: "sk-a"},
			"acct-b": {LiteLLMKey: "sk-b"},
		},
	}
	pm := &mockPodManager{pods: []corev1.Pod{
		agentPodWithHeader("a1", litellmHeaderFor("sk-a")),
		agentPodWithHeader("b1", litellmHeaderFor("sk-b")),
		agentPodWithHeader("b2", litellmHeaderFor("sk-b")),
	}}
	refreshAccountLiveCounts(context.Background(), slog.Default(), pm, "gardenio", cfg)

	got := cfg.SnapshotAccountLiveCounts()
	if got["acct-a"] != 1 || got["acct-b"] != 2 {
		t.Fatalf("unexpected snapshot: %v", got)
	}
}

// TestConfigAccountLiveCounts_BumpAndSnapshot exercises the optimistic bump and
// the snapshot-copy semantics (mutating the copy must not affect the cfg).
func TestConfigAccountLiveCounts_BumpAndSnapshot(t *testing.T) {
	cfg := &config.Config{}
	cfg.BumpAccountLiveCount("acct-a")
	cfg.BumpAccountLiveCount("acct-a")
	cfg.BumpAccountLiveCount("acct-b")
	cfg.BumpAccountLiveCount("") // no-op

	snap := cfg.SnapshotAccountLiveCounts()
	if snap["acct-a"] != 2 || snap["acct-b"] != 1 {
		t.Fatalf("unexpected counts after bump: %v", snap)
	}
	if _, ok := snap[""]; ok {
		t.Errorf("empty account must not be bumped")
	}
	// Mutating the snapshot must not affect the live counts.
	snap["acct-a"] = 999
	if again := cfg.SnapshotAccountLiveCounts(); again["acct-a"] != 2 {
		t.Errorf("snapshot must be a copy; cfg mutated to %v", again)
	}
}

// TestCountCredentialedProjectAccounts is the observability guard for the gasbox
// outage (ka-0000007343): a project with credentialed, labeled account beads that
// resolves to no account must be detectable so applyAccountConfig can WARN
// instead of silently spawning credential-less pods.
func TestCountCredentialedProjectAccounts(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			// Two gasbox accounts with a LiteLLM key — credentialed.
			"gasbox-a": {LiteLLMKey: "sk-a", Labels: []string{"project:gasbox"}},
			"gasbox-b": {Credential: "secret:tok", Labels: []string{"project:gasbox"}},
			// A gasbox account with NO credential — not counted.
			"gasbox-keyless": {Labels: []string{"project:gasbox"}},
			// Another project — not counted for gasbox.
			"other": {LiteLLMKey: "sk-o", Labels: []string{"project:gardenio"}},
		},
	}
	if got := countCredentialedProjectAccounts(cfg, "gasbox", ""); got != 2 {
		t.Errorf("gasbox: want 2 credentialed accounts, got %d", got)
	}
	if got := countCredentialedProjectAccounts(cfg, "gardenio", ""); got != 1 {
		t.Errorf("gardenio: want 1, got %d", got)
	}
	if got := countCredentialedProjectAccounts(cfg, "nonexistent", ""); got != 0 {
		t.Errorf("nonexistent: want 0, got %d", got)
	}
	if got := countCredentialedProjectAccounts(cfg, "", ""); got != 0 {
		t.Errorf("empty project: want 0, got %d", got)
	}
}

// --- Role account_whitelist enforcement on the spread path (ka-0000003061) ---
//
// gb-init's resolveProjectAccount whitelist filter became a runtime no-op once
// the controller pins BOAT_ACCOUNT (MR !4740): init early-returns and never
// runs its intersection. These tests pin the controller-side enforcement that
// replaces it — restrict the spread pool to the role's whitelist, fail-closed.

// TestSelectSpreadAccount_WhitelistRestrictsPool verifies a role whitelist
// narrows the spread pool to its accounts: even when a non-whitelisted account
// is least-loaded, only a whitelisted account is picked.
func TestSelectSpreadAccount_WhitelistRestrictsPool(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-a": acct("sk-a", "active", "", "project:gardenio"),
			"acct-b": acct("sk-b", "active", "", "project:gardenio"),
			"acct-c": acct("sk-c", "active", "", "project:gardenio"),
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"scoped": {AccountWhitelist: []string{"acct-b", "acct-c"}},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "scoped"}
	// acct-a is least-loaded (0) but not whitelisted; acct-b is least-loaded
	// among the whitelisted pair, so it wins.
	got := selectAccount(cfg, "gardenio", spec, map[string]int{"acct-b": 1, "acct-c": 5})
	if got != "acct-b" {
		t.Fatalf("whitelist must restrict pool to {acct-b,acct-c}, least-loaded acct-b; got %q", got)
	}
}

// TestSelectSpreadAccount_WhitelistFailClosed verifies a whitelist with no
// overlap with the project pool yields no spread candidate (""), rather than
// silently spreading onto a forbidden account.
func TestSelectSpreadAccount_WhitelistFailClosed(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-a": acct("sk-a", "active", "", "project:gardenio"),
			"acct-b": acct("sk-b", "active", "", "project:gardenio"),
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"scoped": {AccountWhitelist: []string{"not-in-pool"}},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "scoped"}
	if got := selectAccount(cfg, "gardenio", spec, map[string]int{}); got != "" {
		t.Fatalf("whitelist with no pool overlap must yield no candidate (fail-closed), got %q", got)
	}
}

// TestSelectSpreadAccount_EmptyWhitelistNoRestriction verifies a role with an
// empty/absent whitelist spreads across the full project pool as before.
func TestSelectSpreadAccount_EmptyWhitelistNoRestriction(t *testing.T) {
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-a": acct("sk-a", "active", "", "project:gardenio"),
			"acct-b": acct("sk-b", "active", "", "project:gardenio"),
		},
		RoleCache: map[string]config.RoleCacheEntry{
			"unscoped": {AccountWhitelist: nil},
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "unscoped"}
	// Deterministic alphabetical tie-break at equal load → acct-a.
	if got := selectAccount(cfg, "gardenio", spec, map[string]int{}); got != "acct-a" {
		t.Fatalf("empty whitelist must impose no restriction, got %q", got)
	}
}

// TestResolveRoleAccountWhitelist_MultiRoleUnion verifies a comma-separated
// multi-role string unions the whitelists of its roles (an agent may use any
// account allowed by any of its roles).
func TestResolveRoleAccountWhitelist_MultiRoleUnion(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"r1": {AccountWhitelist: []string{"acct-a", "acct-b"}},
			"r2": {AccountWhitelist: []string{"acct-b", "acct-c"}},
		},
	}
	got := resolveRoleAccountWhitelist(cfg, "r1,r2")
	want := map[string]bool{"acct-a": true, "acct-b": true, "acct-c": true}
	if len(got) != len(want) {
		t.Fatalf("union want %d distinct accounts, got %v", len(want), got)
	}
	for _, a := range got {
		if !want[a] {
			t.Fatalf("unexpected account %q in union %v", a, got)
		}
	}
}

// TestAccountAllowedByRoleWhitelist covers the fail-closed gate logic directly:
// empty whitelist allows anything; a non-empty whitelist allows only listed
// accounts.
func TestAccountAllowedByRoleWhitelist(t *testing.T) {
	cfg := &config.Config{
		RoleCache: map[string]config.RoleCacheEntry{
			"scoped":   {AccountWhitelist: []string{"acct-b"}},
			"unscoped": {},
		},
	}
	cases := []struct {
		role, account string
		want          bool
	}{
		{"scoped", "acct-b", true},     // listed
		{"scoped", "acct-a", false},    // not listed → fail-closed
		{"unscoped", "anything", true}, // empty whitelist → no restriction
		{"", "anything", true},         // no role → no restriction
		{"unknown-role", "x", true},    // role not in cache → no restriction
	}
	for _, c := range cases {
		if got := accountAllowedByRoleWhitelist(cfg, c.role, c.account); got != c.want {
			t.Errorf("accountAllowedByRoleWhitelist(role=%q, account=%q) = %v, want %v",
				c.role, c.account, got, c.want)
		}
	}
}

// --- Capacity+reset bonus + spawn_weight (ka-0000001522) ---
//
// Operator ask (2026-07-02): "weight accounts so that when they are closer to
// resetting with unused capacity they get prioritized when possible" plus an
// immediate manual lever for matthewbaker1110. These tests pin the two new
// ranking terms: the capped capacityResetBonus auto-heuristic and the uncapped
// spawn_weight operator bias.

// utilAcct is acct plus the probe-written utilization/reset fields feeding
// capacityResetBonus.
func utilAcct(litellmKey, util5h, reset5h string, labels ...string) config.AccountCacheEntry {
	e := acct(litellmKey, "active", "", labels...)
	e.Utilization5h = util5h
	e.ThrottleReset5hAt = reset5h
	e.RepresentativeClaim = "five_hour"
	return e
}

// TestCapacityResetBonus covers the bonus math directly: unused capacity earns
// the baseline, reset proximity ramps it toward the cap, stale/absent data
// earns nothing, and the result never exceeds maxCapacityResetBonus.
func TestCapacityResetBonus(t *testing.T) {
	now := time.Date(2026, 7, 2, 0, 0, 0, 0, time.UTC)
	rfc := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }

	tests := []struct {
		name string
		acct config.AccountCacheEntry
		want float64
	}{
		{"no probe data → 0", acct("sk", "active", "", "project:gardenio"), 0},
		{"unparsable utilization → 0", utilAcct("sk", "n/a", rfc(time.Hour)), 0},
		{"NaN utilization → 0", utilAcct("sk", "NaN", rfc(time.Hour)), 0},
		{"Inf utilization → 0", utilAcct("sk", "-Inf", rfc(time.Hour)), 0},
		// Far from reset (beyond horizon): baseline only, scaled by unused.
		{"idle, reset far away → baseline", utilAcct("sk", "0.0000", rfc(48*time.Hour)), capacityBaselineWeight},
		{"half-used, reset far away", utilAcct("sk", "0.5000", rfc(48*time.Hour)), 0.5 * capacityBaselineWeight},
		{"fully used → 0 regardless of reset", utilAcct("sk", "1.0000", rfc(time.Minute)), 0},
		// No reset timestamp at all: still the baseline capacity term.
		{"idle, no reset time → baseline", utilAcct("sk", "0.0000", ""), capacityBaselineWeight},
		// At the reset instant-ish (1 min out): urgency ≈ 1 → ≈ cap for an idle account.
		{"idle, resets in 1min → ≈cap", utilAcct("sk", "0.0000", rfc(time.Minute)), 0}, // want checked via range below
		// Stale reset (in the past): utilization no longer describes the live
		// window — no urgency, baseline only.
		{"idle, stale reset → baseline", utilAcct("sk", "0.0000", rfc(-time.Hour)), capacityBaselineWeight},
		// Out-of-range utilization clamps instead of going negative/over.
		{"utilization >1 clamps to 0 bonus", utilAcct("sk", "3.7", rfc(time.Minute)), 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := capacityResetBonus(tc.acct, now)
			if tc.name == "idle, resets in 1min → ≈cap" {
				if got < maxCapacityResetBonus*0.95 || got > maxCapacityResetBonus {
					t.Fatalf("want ≈%v (within 5%%), got %v", maxCapacityResetBonus, got)
				}
				return
			}
			if diff := got - tc.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}

	// Cap invariant: no input exceeds maxCapacityResetBonus.
	for _, u := range []string{"0.0", "0.05", "0.5", "0.99"} {
		for _, d := range []time.Duration{-time.Hour, time.Second, time.Minute, time.Hour, 3 * time.Hour} {
			if got := capacityResetBonus(utilAcct("sk", u, rfc(d)), now); got > maxCapacityResetBonus {
				t.Fatalf("bonus %v exceeds cap %v (util=%s, reset in %v)", got, maxCapacityResetBonus, u, d)
			}
		}
	}

	// seven_day binding window reads the 7d fields, not the 5h ones.
	sevenDay := acct("sk", "active", "", "project:gardenio")
	sevenDay.RepresentativeClaim = "seven_day"
	sevenDay.Utilization7d = "0.0000"
	sevenDay.ThrottleReset7dAt = rfc(48 * time.Hour)
	sevenDay.Utilization5h = "1.0000" // would yield 0 if wrongly read
	if got := capacityResetBonus(sevenDay, now); got != capacityBaselineWeight {
		t.Errorf("seven_day claim must read 7d fields: got %v, want %v", got, capacityBaselineWeight)
	}
}

// TestSelectSpreadAccount_IdleNearResetPreferred is the operator scenario end
// to end: pool of equally-loaded healthy accounts where one (matthewbaker1110)
// is nearly idle in a 5h window that resets soon — it must win the spread even
// against the alphabetical tie-break, and keep winning until it has absorbed
// the (capped) bonus worth of extra pods.
func TestSelectSpreadAccount_IdleNearResetPreferred(t *testing.T) {
	now := time.Date(2026, 7, 2, 1, 40, 0, 0, time.UTC)
	reset := "2026-07-02T02:10:00Z" // 30 min out — inside the urgency horizon
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio":         utilAcct("sk-g", "0.4300", reset, "project:gardenio"),
			"gardenio2":        utilAcct("sk-g2", "0.4100", reset, "project:gardenio"),
			"matthewbaker1110": utilAcct("sk-mb", "0.0500", reset, "project:gardenio"),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio"}

	// Idle-capacity account wins at equal live load despite sorting last
	// alphabetically.
	if got := selectAccountAt(cfg, "gardenio", spec, map[string]int{"gardenio": 2, "gardenio2": 2, "matthewbaker1110": 2}, now); got != "matthewbaker1110" {
		t.Fatalf("idle near-reset account must win at equal load, got %q", got)
	}

	// BIAS, not dogpile: once the favored account carries more than the capped
	// bonus worth of extra pods, the spread takes back over.
	extra := int(maxCapacityResetBonus) + 1
	if got := selectAccountAt(cfg, "gardenio", spec, map[string]int{"gardenio": 2, "gardenio2": 2, "matthewbaker1110": 2 + extra}, now); got == "matthewbaker1110" {
		t.Fatalf("capped bonus must not concentrate all pods: favored account still picked at +%d pods", extra)
	}
}

// TestSelectSpreadAccount_SpawnWeightManualBias pins the operator lever: a
// positive spawn_weight pulls new pods onto the account (even past the auto
// bonus cap), a negative one repels them, and garbage values are ignored.
func TestSelectSpreadAccount_SpawnWeightManualBias(t *testing.T) {
	spec := &podmanager.AgentPodSpec{Project: "gardenio"}
	weighted := acct("sk-w", "active", "", "project:gardenio")
	weighted.SpawnWeight = "10"
	plain := acct("sk-p", "active", "", "project:gardenio")
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-plain":    plain,
			"acct-weighted": weighted,
		},
	}

	// The weighted account absorbs up to spawn_weight extra pods before the
	// plain account is picked again — the "lean hard on matthewbaker1110" ask.
	live := map[string]int{}
	for i := 0; i < 10; i++ {
		got := selectAccount(cfg, "gardenio", spec, live)
		if got != "acct-weighted" {
			t.Fatalf("spawn %d: want acct-weighted while weight headroom remains, got %q (live=%v)", i, got, live)
		}
		live[got]++
	}
	// Headroom exhausted (10 pods ≈ weight 10): tie at effective load 0 breaks
	// alphabetically to acct-plain.
	if got := selectAccount(cfg, "gardenio", spec, live); got != "acct-plain" {
		t.Fatalf("after absorbing weight, spread must resume: got %q (live=%v)", got, live)
	}

	// Negative weight repels: alphabetically-first account with weight -2 loses
	// the idle tie.
	repelled := acct("sk-r", "active", "", "project:gardenio")
	repelled.SpawnWeight = "-2"
	cfgNeg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-a": repelled,
			"acct-b": acct("sk-b", "active", "", "project:gardenio"),
		},
	}
	if got := selectAccount(cfgNeg, "gardenio", spec, map[string]int{}); got != "acct-b" {
		t.Fatalf("negative spawn_weight must repel, got %q", got)
	}

	// Garbage weights are ignored, not treated as 0-with-error side effects.
	// ParseFloat accepts "NaN"/"Inf" WITHOUT error: NaN would make the sort
	// comparator inconsistent and Inf is bump-proof (live-count increments
	// never rebalance it), so non-finite values must be rejected too.
	for _, w := range []string{"heavy", "NaN", "nan", "Inf", "+Inf", "-inf"} {
		garbage := acct("sk-g", "active", "", "project:gardenio")
		garbage.SpawnWeight = w
		cfgGarbage := &config.Config{
			AccountCache: map[string]config.AccountCacheEntry{
				"acct-garbage": garbage,
				"acct-z":       acct("sk-z", "active", "", "project:gardenio"),
			},
		}
		if got := selectAccount(cfgGarbage, "gardenio", spec, map[string]int{}); got != "acct-garbage" {
			t.Fatalf("spawn_weight=%q must fall back to plain ranking (alphabetical tie), got %q", w, got)
		}
	}
}

// TestSelectSpreadAccount_SpawnWeightNeverOverridesHealthGating verifies the
// manual lever biases ranking only: a rejected / exhausted account stays
// excluded no matter how large its spawn_weight.
func TestSelectSpreadAccount_SpawnWeightNeverOverridesHealthGating(t *testing.T) {
	spec := &podmanager.AgentPodSpec{Project: "gardenio"}
	rejected := acct("sk-r", "active", "rejected", "project:gardenio")
	rejected.SpawnWeight = "1000"
	exhausted := acct("sk-e", "exhausted", "", "project:gardenio")
	exhausted.SpawnWeight = "1000"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-rejected":  rejected,
			"acct-exhausted": exhausted,
			"acct-healthy":   acct("sk-h", "active", "", "project:gardenio"),
		},
	}
	if got := selectAccount(cfg, "gardenio", spec, map[string]int{"acct-healthy": 99}); got != "acct-healthy" {
		t.Fatalf("spawn_weight must never resurrect an unhealthy account, got %q", got)
	}
}

// TestSelectSpreadAccount_BonusCannotBeatWarningPenalty pins the tier
// invariant: maxCapacityResetBonus < warningSpreadPenalty, so the auto bonus
// can never promote an equally-loaded allowed_warning account over a
// fully-healthy one. (An account near its limit with high "unused capacity"
// numbers would be probe-inconsistent anyway — belt and braces.)
func TestSelectSpreadAccount_BonusCannotBeatWarningPenalty(t *testing.T) {
	if maxCapacityResetBonus >= warningSpreadPenalty {
		t.Fatalf("maxCapacityResetBonus (%v) must stay below warningSpreadPenalty (%v)", maxCapacityResetBonus, float64(warningSpreadPenalty))
	}
	now := time.Date(2026, 7, 2, 1, 40, 0, 0, time.UTC)
	warn := utilAcct("sk-w", "0.0000", "2026-07-02T01:41:00Z", "project:gardenio")
	warn.ThrottleStatus = "allowed_warning"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-healthy": acct("sk-h", "active", "", "project:gardenio"),
			"acct-warn":    warn,
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio"}
	if got := selectAccountAt(cfg, "gardenio", spec, map[string]int{}, now); got != "acct-healthy" {
		t.Fatalf("max bonus on a warning account must not beat an idle healthy one, got %q", got)
	}
}

// --- Headroom guardrail + long-lived-role awareness (ka-0000001331) ---
//
// Operator ask (2026-07-14): "prefer the agent with lowest utilization and
// highest time until reset ... nearly-exhausted accounts trip up long-running
// agents e.g. workflow runners." Two behaviors are pinned below: (1) the
// reset-proximity urgency term of capacityResetBonus is suppressed once the
// binding window is heavily utilized, and (2) long-lived roles prefer absolute
// account headroom and steer away from near-limit accounts.

// TestCapacityResetBonus_HighUtilGuardrail pins the guardrail: below the
// highUtilGuardrail threshold, an account near its reset earns the ramped
// use-it-or-lose-it bonus for its (small) unused capacity; ABOVE the threshold
// the same reset proximity no longer amplifies the bonus — it collapses to the
// always-on baseline for the remaining headroom. This is the exact shannon
// failure: a near-limit account must not earn an INCREASING bonus as its reset
// approaches.
func TestCapacityResetBonus_HighUtilGuardrail(t *testing.T) {
	now := time.Date(2026, 7, 14, 4, 4, 0, 0, time.UTC)
	rfc := func(d time.Duration) string { return now.Add(d).Format(time.RFC3339) }

	// Just BELOW the guardrail, 1 min from reset: urgency ≈ 1, so the bonus is
	// close to the ramped max scaled by the (small) unused fraction.
	below := utilAcct("sk-b", "0.6900", rfc(time.Minute))
	gotBelow := capacityResetBonus(below, now)
	unusedBelow := 1 - 0.69
	if wantMin := unusedBelow * (maxCapacityResetBonus * 0.95); gotBelow < wantMin {
		t.Fatalf("below guardrail, near reset: want ramped bonus ≥%v, got %v", wantMin, gotBelow)
	}

	// Just ABOVE the guardrail, same 1 min from reset: urgency is suppressed, so
	// the bonus collapses to baseline × unused — strictly LESS than what the same
	// proximity earned just below the threshold.
	above := utilAcct("sk-a", "0.7100", rfc(time.Minute))
	gotAbove := capacityResetBonus(above, now)
	unusedAbove := 1 - 0.71
	if want := unusedAbove * capacityBaselineWeight; gotAbove < want-1e-9 || gotAbove > want+1e-9 {
		t.Fatalf("above guardrail: bonus must collapse to baseline×unused (%v), got %v", want, gotAbove)
	}
	if gotAbove >= gotBelow {
		t.Fatalf("guardrail must make a near-limit account earn LESS reset bonus (%v) than a below-limit one (%v)", gotAbove, gotBelow)
	}

	// The guardrail only suppresses the proximity RAMP, not the baseline: a
	// high-util account far from reset earns the same baseline×unused it always
	// did (no regression for the steady-state term that separates accounts
	// day-to-day).
	far := utilAcct("sk-f", "0.9000", rfc(48*time.Hour))
	if got, want := capacityResetBonus(far, now), (1-0.90)*capacityBaselineWeight; got < want-1e-9 || got > want+1e-9 {
		t.Fatalf("high-util far-from-reset: baseline term must be unchanged, want %v got %v", want, got)
	}
}

// TestSelectSpreadAccount_GuardrailStopsPullingOntoNearLimit is the shannon
// scenario end to end: a near-limit account (util 0.76) approaching its reset
// must NOT out-rank a lower-util account at equal live load. Before the
// guardrail, shannon's reset-proximity bonus climbed as it neared the wall and
// pulled MORE load onto it; the guardrail collapses that bonus so the
// lower-util account wins.
func TestSelectSpreadAccount_GuardrailStopsPullingOntoNearLimit(t *testing.T) {
	now := time.Date(2026, 7, 14, 4, 4, 0, 0, time.UTC)
	soon := now.Add(20 * time.Minute).Format(time.RFC3339) // inside the urgency horizon
	far := now.Add(30 * time.Hour).Format(time.RFC3339)
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			// Near its limit AND close to reset — the shape that used to win.
			"shannon": utilAcct("sk-sh", "0.7600", soon, "project:gardenio"),
			// More headroom, reset far off.
			"gardenio2": utilAcct("sk-g2", "0.5000", far, "project:gardenio"),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio"} // ordinary short-lived role
	if got := selectAccountAt(cfg, "gardenio", spec, map[string]int{"shannon": 2, "gardenio2": 2}, now); got != "gardenio2" {
		t.Fatalf("guardrail: near-limit account approaching reset must not win at equal load, got %q", got)
	}
}

// TestRoleIsLongLived pins the role classification, including the
// comma-separated primary-role convention (matches modeForRole).
func TestRoleIsLongLived(t *testing.T) {
	cases := map[string]bool{
		"gardener":      true,
		"plan-gardener": true,
		"gardener,crew": true, // primary role is authoritative
		" gardener ":    true, // trimmed
		"crew":          false,
		"thread":        false,
		"crew,gardener": false, // primary is crew — not long-lived
		"reviewer":      false,
		"":              false,
	}
	for role, want := range cases {
		if got := roleIsLongLived(role); got != want {
			t.Errorf("roleIsLongLived(%q) = %v, want %v", role, got, want)
		}
	}
}

// TestSelectSpreadAccount_LongLivedPrefersHeadroom verifies a long-lived role
// (gardener) is routed to the account with the most ABSOLUTE binding-window
// headroom even when a lower-headroom account carries FEWER live pods — the
// absolute-headroom bias for hours-long agents. A short-lived role in the same
// pool follows plain least-loaded instead.
func TestSelectSpreadAccount_LongLivedPrefersHeadroom(t *testing.T) {
	now := time.Date(2026, 7, 14, 4, 4, 0, 0, time.UTC)
	far := now.Add(30 * time.Hour).Format(time.RFC3339) // no reset-proximity bonus in play
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			// Lots of headroom but a couple more live pods.
			"roomy": utilAcct("sk-r", "0.1000", far, "project:gardenio"),
			// Nearly full but momentarily idle.
			"tight": utilAcct("sk-t", "0.6500", far, "project:gardenio"),
		},
	}
	// Live load favors "tight" (idle) by 1 pod. For a SHORT-lived role that is
	// enough to win despite the small baseline headroom bonus.
	live := map[string]int{"roomy": 1, "tight": 0}
	shortSpec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew"}
	if got := selectAccountAt(cfg, "gardenio", shortSpec, live, now); got != "tight" {
		t.Fatalf("short-lived role should follow least-loaded (tight), got %q", got)
	}
	// A LONG-lived role weights absolute headroom heavily enough to overcome the
	// live-load gap and pick the roomy account.
	longSpec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "gardener"}
	if got := selectAccountAt(cfg, "gardenio", longSpec, live, now); got != "roomy" {
		t.Fatalf("long-lived role must prefer absolute headroom (roomy), got %q", got)
	}
}

// TestSelectSpreadAccount_LongLivedAvoidsNearLimit verifies the near-limit
// penalty: a long-lived role never lands on an account past
// longLivedUtilCeiling when any account with real headroom exists, even if the
// near-limit account is completely idle (0 live pods).
func TestSelectSpreadAccount_LongLivedAvoidsNearLimit(t *testing.T) {
	now := time.Date(2026, 7, 14, 4, 4, 0, 0, time.UTC)
	far := now.Add(30 * time.Hour).Format(time.RFC3339)
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			// Idle by live count, but past the ceiling — a mid-run wall risk.
			"nearlimit": utilAcct("sk-n", "0.9000", far, "project:gardenio"),
			// Loaded by live count, but comfortable headroom.
			"headroom": utilAcct("sk-h", "0.3000", far, "project:gardenio"),
		},
	}
	longSpec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "plan-gardener"}
	// Even with headroom carrying 3 more live pods, the ceiling penalty keeps the
	// long-lived role off the near-limit account.
	if got := selectAccountAt(cfg, "gardenio", longSpec, map[string]int{"nearlimit": 0, "headroom": 3}, now); got != "headroom" {
		t.Fatalf("long-lived role must avoid a near-limit account, got %q", got)
	}
	// Degenerate pool — EVERY account past the ceiling: the penalty is a soft
	// guard, so the role still gets the least-bad (most-headroom) account rather
	// than a stranded empty pick.
	allTight := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"tighter": utilAcct("sk-a", "0.9500", far, "project:gardenio"),
			"tight":   utilAcct("sk-b", "0.8500", far, "project:gardenio"),
		},
	}
	if got := selectAccountAt(allTight, "gardenio", longSpec, map[string]int{}, now); got != "tight" {
		t.Fatalf("all-near-limit pool must still resolve to the most-headroom account, got %q", got)
	}
}

// TestSelectSpreadAccount_ShortLivedRoleUnaffected is a regression guard: the
// long-lived headroom bias must not perturb ordinary roles. With no long-lived
// role in play, ranking is identical to before this change (least-loaded +
// existing bonuses).
func TestSelectSpreadAccount_ShortLivedRoleUnaffected(t *testing.T) {
	now := time.Date(2026, 7, 14, 4, 4, 0, 0, time.UTC)
	far := now.Add(30 * time.Hour).Format(time.RFC3339)
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-a": utilAcct("sk-a", "0.9000", far, "project:gardenio"), // near limit
			"acct-b": utilAcct("sk-b", "0.1000", far, "project:gardenio"), // lots of headroom
		},
	}
	// For a short-lived role the near-limit penalty and headroom weight are OFF,
	// so a 5-pod live-load gap on the headroom account still routes the crew
	// agent onto the (idle) near-limit account — plain least-loaded with only the
	// small baseline capacity bonus in play.
	shortSpec := &podmanager.AgentPodSpec{Project: "gardenio", Role: "crew"}
	if got := selectAccountAt(cfg, "gardenio", shortSpec, map[string]int{"acct-a": 0, "acct-b": 5}, now); got != "acct-a" {
		t.Fatalf("short-lived role must be unaffected by the long-lived headroom bias, got %q", got)
	}
}

// --- Served-model spread gate (ka-0000002790) ---
//
// The glm-bedrock incident: a target/undeployed-model account (valid LiteLLM
// key, project label, but a model LiteLLM does not serve) entered the cognition
// spread pool as lowest-load and was baked as the agent's OWN brain, crash-
// looping every pod on its first inference turn. These tests pin the gate that
// excludes such an account, plus its fail-open safety rails.

const gateLiteLLM = "http://gardenio-litellm:4000"

// servedAcct builds a project-pool account with an explicit model + base_url so
// the served-model gate (which keys on model ∈ served-set AND base_url == our
// litellm) can be exercised. Mirrors the terse acct builder.
func servedAcct(litellmKey, model string, labels ...string) config.AccountCacheEntry {
	e := acct(litellmKey, "active", "", labels...)
	e.Model = model
	e.BaseURL = gateLiteLLM
	return e
}

// TestSelectSpreadAccount_ServedModelGate_ExcludesUnserved is the core glm-
// bedrock regression: an account whose baked model is NOT in LiteLLM's served
// set must be excluded from the spread pool even when it is the least-loaded
// (idle) candidate, so it can never be baked as cognition.
func TestSelectSpreadAccount_ServedModelGate_ExcludesUnserved(t *testing.T) {
	cfg := &config.Config{
		SpreadServedModelGateEnabled: true,
		LiteLLMURL:                   gateLiteLLM,
		// Served set omits bedrock-glm-5 (the target model, not yet deployed).
		ServedModels: map[string]bool{"claude-opus-4-8-oauth": true},
		AccountCache: map[string]config.AccountCacheEntry{
			"glm-bedrock": servedAcct("sk-glm", "bedrock-glm-5", "project:glm-lab"),
			"shannon":     servedAcct("sk-op", "claude-opus-4-8-oauth", "project:glm-lab"),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "glm-lab"}

	// glm-bedrock is idle (0 pods) and would win least-loaded; the served-model
	// gate must still steer to the served account even when it is heavily loaded.
	if got := selectAccount(cfg, "glm-lab", spec, map[string]int{"shannon": 99, "glm-bedrock": 0}); got != "shannon" {
		t.Fatalf("unserved-model account must be excluded; expected shannon, got %q", got)
	}

	// With ONLY the unserved account, spread returns "" so the caller falls back
	// rather than baking a model LiteLLM cannot serve.
	cfgOnlyUnserved := &config.Config{
		SpreadServedModelGateEnabled: true,
		LiteLLMURL:                   gateLiteLLM,
		ServedModels:                 map[string]bool{"claude-opus-4-8-oauth": true},
		AccountCache: map[string]config.AccountCacheEntry{
			"glm-bedrock": servedAcct("sk-glm", "bedrock-glm-5", "project:glm-lab"),
		},
	}
	if got := selectAccount(cfgOnlyUnserved, "glm-lab", spec, nil); got != "" {
		t.Fatalf("expected \"\" when the only account names an unserved model, got %q", got)
	}
}

// TestSelectSpreadAccount_ServedModelGate_KeepsServed verifies an account whose
// model IS served stays a normal spread candidate (the gate is not a blanket
// exclusion of model-bearing accounts).
func TestSelectSpreadAccount_ServedModelGate_KeepsServed(t *testing.T) {
	cfg := &config.Config{
		SpreadServedModelGateEnabled: true,
		LiteLLMURL:                   gateLiteLLM,
		ServedModels:                 map[string]bool{"claude-opus-4-8-oauth": true},
		AccountCache: map[string]config.AccountCacheEntry{
			"shannon":   servedAcct("sk-a", "claude-opus-4-8-oauth", "project:gardenio"),
			"gardenio2": servedAcct("sk-b", "claude-opus-4-8-oauth", "project:gardenio"),
		},
	}
	spec := &podmanager.AgentPodSpec{Project: "gardenio"}
	// Both served → least-loaded/alphabetical selection proceeds normally.
	if got := selectAccount(cfg, "gardenio", spec, map[string]int{"shannon": 5, "gardenio2": 1}); got != "gardenio2" {
		t.Fatalf("served account must remain a candidate; expected gardenio2, got %q", got)
	}
}

// TestSelectSpreadAccount_ServedModelGate_FailOpen pins every fail-open rail:
// the gate must NOT exclude an unserved-model account when (a) the kill switch
// is off, (b) the served set is unknown (nil), (c) the account routes through a
// DIFFERENT litellm than the one the served set describes.
func TestSelectSpreadAccount_ServedModelGate_FailOpen(t *testing.T) {
	spec := &podmanager.AgentPodSpec{Project: "glm-lab"}
	unserved := servedAcct("sk-glm", "bedrock-glm-5", "project:glm-lab")

	// (a) Kill switch OFF: even a known served set that omits the model does not
	// gate. The unserved (idle) account wins least-loaded as before the fix.
	cfgOff := &config.Config{
		SpreadServedModelGateEnabled: false,
		LiteLLMURL:                   gateLiteLLM,
		ServedModels:                 map[string]bool{"claude-opus-4-8-oauth": true},
		AccountCache:                 map[string]config.AccountCacheEntry{"glm-bedrock": unserved},
	}
	if got := selectAccount(cfgOff, "glm-lab", spec, nil); got != "glm-bedrock" {
		t.Errorf("kill switch off: expected no gating (glm-bedrock), got %q", got)
	}

	// (b) Served set UNKNOWN (nil): no data → no gating.
	cfgNil := &config.Config{
		SpreadServedModelGateEnabled: true,
		LiteLLMURL:                   gateLiteLLM,
		ServedModels:                 nil,
		AccountCache:                 map[string]config.AccountCacheEntry{"glm-bedrock": unserved},
	}
	if got := selectAccount(cfgNil, "glm-lab", spec, nil); got != "glm-bedrock" {
		t.Errorf("unknown served set: expected no gating (glm-bedrock), got %q", got)
	}

	// (c) Account routes through a DIFFERENT litellm (external devops proxy) — its
	// served set is one we did not fetch, so we must not judge its model.
	foreign := unserved
	foreign.BaseURL = "https://litellm.app.devops.acme.example"
	cfgForeign := &config.Config{
		SpreadServedModelGateEnabled: true,
		LiteLLMURL:                   gateLiteLLM,
		ServedModels:                 map[string]bool{"claude-opus-4-8-oauth": true},
		AccountCache:                 map[string]config.AccountCacheEntry{"glm-bedrock": foreign},
	}
	if got := selectAccount(cfgForeign, "glm-lab", spec, nil); got != "glm-bedrock" {
		t.Errorf("foreign-proxy account: expected no gating (glm-bedrock), got %q", got)
	}
}

// TestAccountModelUnserved is the unit-level truth table for the gate predicate.
func TestAccountModelUnserved(t *testing.T) {
	served := map[string]bool{"claude-opus-4-8-oauth": true, "bedrock-glm-5": true}
	mk := func(model, baseURL string) config.AccountCacheEntry {
		return config.AccountCacheEntry{Model: model, BaseURL: baseURL}
	}
	cases := []struct {
		name    string
		acct    config.AccountCacheEntry
		served  map[string]bool
		litellm string
		want    bool
	}{
		{"unserved model, our proxy → gate", mk("bedrock-glm-9", gateLiteLLM), served, gateLiteLLM, true},
		{"served model, our proxy → keep", mk("claude-opus-4-8-oauth", gateLiteLLM), served, gateLiteLLM, false},
		{"unserved model, empty base_url uses our litellm → gate", mk("bedrock-glm-9", ""), served, gateLiteLLM, true},
		{"unserved model, foreign proxy → keep", mk("bedrock-glm-9", "https://other:4000"), served, gateLiteLLM, false},
		{"empty model → keep", mk("", gateLiteLLM), served, gateLiteLLM, false},
		{"nil served set → keep", mk("bedrock-glm-9", gateLiteLLM), nil, gateLiteLLM, false},
		{"empty served set → keep", mk("bedrock-glm-9", gateLiteLLM), map[string]bool{}, gateLiteLLM, false},
		{"case-insensitive base_url match → gate", mk("bedrock-glm-9", "HTTP://GARDENIO-LITELLM:4000"), served, gateLiteLLM, true},
		{"trailing-slash base_url match → gate", mk("bedrock-glm-9", gateLiteLLM+"/"), served, gateLiteLLM, true},
		{"empty base_url but no global litellm → keep", mk("bedrock-glm-9", ""), served, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountModelUnserved(tc.acct, tc.served, tc.litellm); got != tc.want {
				t.Errorf("accountModelUnserved = %v, want %v", got, tc.want)
			}
		})
	}
}
