package main

import (
	"reflect"
	"sort"
	"testing"

	"gardenio/controller/internal/config"
)

// account_pool_validator_test.go — tests for the cyclic-fallback-chain detector
// and stranded-project-pool finder (ka-0000002560, criteria 1 & 3).

// sortCycles canonicalizes the set of detected cycles (each already
// member-sorted) by sorting the outer slice on the joined membership, so
// assertions do not depend on map iteration order.
func sortCycles(cycles [][]string) [][]string {
	out := append([][]string(nil), cycles...)
	sort.Slice(out, func(i, j int) bool {
		return joinForSort(out[i]) < joinForSort(out[j])
	})
	return out
}

func joinForSort(s []string) string {
	var b string
	for _, x := range s {
		b += x + "\x00"
	}
	return b
}

func TestDetectFallbackCycles_MonorepoCircularChain(t *testing.T) {
	// The 2026-06-29 incident topology: gardenio2 -> gardenio3 ->
	// matthewbaker-acme -> gardenio2 (matthewbaker-acme.fallback=gardenio2).
	accounts := map[string]config.AccountCacheEntry{
		"gardenio2":              {Fallback: "gardenio3"},
		"gardenio3":              {Fallback: "matthewbaker-acme"},
		"matthewbaker-acme": {Fallback: "gardenio2"},
	}
	got := sortCycles(detectFallbackCycles(accounts))
	want := [][]string{{"gardenio2", "gardenio3", "matthewbaker-acme"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("detectFallbackCycles = %v, want %v", got, want)
	}
}

func TestDetectFallbackCycles_SelfLoop(t *testing.T) {
	accounts := map[string]config.AccountCacheEntry{
		"a": {Fallback: "a"},
	}
	got := sortCycles(detectFallbackCycles(accounts))
	want := [][]string{{"a"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("self-loop: got %v, want %v", got, want)
	}
}

func TestDetectFallbackCycles_NoCycleTerminatingChain(t *testing.T) {
	// A healthy terminating chain a -> b -> c (c has no fallback) has no cycle.
	accounts := map[string]config.AccountCacheEntry{
		"a": {Fallback: "b"},
		"b": {Fallback: "c"},
		"c": {Fallback: ""},
	}
	if got := detectFallbackCycles(accounts); len(got) != 0 {
		t.Errorf("terminating chain should have no cycles, got %v", got)
	}
}

func TestDetectFallbackCycles_DanglingPointerNoCycle(t *testing.T) {
	// A fallback to an unknown account terminates the walk — not a cycle.
	accounts := map[string]config.AccountCacheEntry{
		"a": {Fallback: "nonexistent"},
	}
	if got := detectFallbackCycles(accounts); len(got) != 0 {
		t.Errorf("dangling pointer should not be a cycle, got %v", got)
	}
}

func TestDetectFallbackCycles_TailIntoCycleReportedOnce(t *testing.T) {
	// A tree tail (t1 -> t2) feeding into a 2-cycle (a <-> b). Only the cycle
	// {a,b} is reported, and exactly once regardless of start order.
	accounts := map[string]config.AccountCacheEntry{
		"t1": {Fallback: "t2"},
		"t2": {Fallback: "a"},
		"a":  {Fallback: "b"},
		"b":  {Fallback: "a"},
	}
	got := sortCycles(detectFallbackCycles(accounts))
	want := [][]string{{"a", "b"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tail-into-cycle: got %v, want %v", got, want)
	}
}

func TestDetectFallbackCycles_TwoIndependentCycles(t *testing.T) {
	accounts := map[string]config.AccountCacheEntry{
		"a": {Fallback: "b"},
		"b": {Fallback: "a"},
		"x": {Fallback: "y"},
		"y": {Fallback: "x"},
	}
	got := sortCycles(detectFallbackCycles(accounts))
	want := [][]string{{"a", "b"}, {"x", "y"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("two cycles: got %v, want %v", got, want)
	}
}

func TestDetectFallbackCycles_NoFallbacks(t *testing.T) {
	accounts := map[string]config.AccountCacheEntry{
		"a": {}, "b": {},
	}
	if got := detectFallbackCycles(accounts); len(got) != 0 {
		t.Errorf("no fallbacks → no cycles, got %v", got)
	}
}

// --- findStrandedProjectPools ---

func TestFindStrandedProjectPools_AllDisabled(t *testing.T) {
	accounts := map[string]config.AccountCacheEntry{
		"a": {Status: "disabled", Labels: []string{"project:p"}},
		"b": {Status: "disabled", Labels: []string{"project:p"}},
	}
	got := findStrandedProjectPools(accounts)
	if got["p"] != 2 {
		t.Errorf("expected project p stranded with 2 accounts, got %v", got)
	}
}

func TestFindStrandedProjectPools_ExhaustedIsNotStranded(t *testing.T) {
	// A merely capacity-exhausted pool RECOVERS on window reset — it is NOT a
	// config-stranded pool, so the validator must not flag it (the spawn-time
	// escape hatch handles the transient case instead).
	accounts := map[string]config.AccountCacheEntry{
		"a": {Status: "exhausted", LiteLLMKey: "k", Labels: []string{"project:p"}},
	}
	if got := findStrandedProjectPools(accounts); len(got) != 0 {
		t.Errorf("exhausted-but-recoverable pool must not be stranded, got %v", got)
	}
}

func TestFindStrandedProjectPools_OneHealthyRescues(t *testing.T) {
	accounts := map[string]config.AccountCacheEntry{
		"dead":    {Status: "disabled", Labels: []string{"project:p"}},
		"healthy": {Status: "active", LiteLLMKey: "k", Labels: []string{"project:p"}},
	}
	if got := findStrandedProjectPools(accounts); len(got) != 0 {
		t.Errorf("a pool with one healthy account is not stranded, got %v", got)
	}
}

func TestFindStrandedProjectPools_KeylessAndBlockingOverage(t *testing.T) {
	accounts := map[string]config.AccountCacheEntry{
		// keyless LiteLLM (401s every agent) — not recoverable on its own.
		"keyless": {BaseURL: "u", LiteLLMKey: "", Labels: []string{"project:p"}},
		// genuinely-blocking billing reason on a METERED account — not self-healing.
		"blocked": {BillingType: "metered", OverageDisabledReason: "payment_method_required", Labels: []string{"project:p"}},
	}
	got := findStrandedProjectPools(accounts)
	if got["p"] != 2 {
		t.Errorf("expected project p stranded with 2 unfit accounts, got %v", got)
	}
}

func TestFindStrandedProjectPools_BenignOverageNotStranded(t *testing.T) {
	// out_of_credits / org_spend_cap_reached are benign on subscription tokens
	// (ka-0000004929) — such an account is healthy-or-recoverable, so its pool is
	// NOT stranded.
	accounts := map[string]config.AccountCacheEntry{
		"a": {OverageDisabledReason: "out_of_credits", LiteLLMKey: "k", Labels: []string{"project:p"}},
	}
	if got := findStrandedProjectPools(accounts); len(got) != 0 {
		t.Errorf("benign-overage account must not strand its pool, got %v", got)
	}
}

func TestAccountHealthyOrRecoverable(t *testing.T) {
	tests := []struct {
		name string
		acct config.AccountCacheEntry
		want bool
	}{
		{"active", config.AccountCacheEntry{Status: "active", LiteLLMKey: "k"}, true},
		{"exhausted recovers", config.AccountCacheEntry{Status: "exhausted", LiteLLMKey: "k"}, true},
		{"disabled", config.AccountCacheEntry{Status: "disabled"}, false},
		{"keyless litellm", config.AccountCacheEntry{BaseURL: "u", LiteLLMKey: ""}, false},
		{"blocking overage on metered", config.AccountCacheEntry{BillingType: "metered", OverageDisabledReason: "payment_method_required"}, false},
		{"benign overage", config.AccountCacheEntry{OverageDisabledReason: "out_of_credits", LiteLLMKey: "k"}, true},
		// Subscription accounts are never billing-stranded by an overage reason,
		// whatever the reason string says (2026-08-28 org_level_disabled outage).
		{"unknown overage on subscription", config.AccountCacheEntry{OverageDisabledReason: "payment_method_required", LiteLLMKey: "k"}, true},
		{"org_level_disabled on subscription", config.AccountCacheEntry{OverageDisabledReason: "org_level_disabled", LiteLLMKey: "k"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountHealthyOrRecoverable(tc.acct); got != tc.want {
				t.Errorf("accountHealthyOrRecoverable(%+v) = %v, want %v", tc.acct, got, tc.want)
			}
		})
	}
}

// TestValidateAccountPools_Smoke exercises the top-level validator with a mixed
// cache (one cycle + one stranded pool) to ensure it runs without panicking and
// is a no-op on empty input. Log output is verified by the unit tests above; this
// just guards the wiring.
func TestValidateAccountPools_Smoke(t *testing.T) {
	validateAccountPools(discardLogger(), nil) // empty → no-op, must not panic
	validateAccountPools(discardLogger(), map[string]config.AccountCacheEntry{
		"a":    {Fallback: "b", Labels: []string{"project:cyc"}},
		"b":    {Fallback: "a", Labels: []string{"project:cyc"}},
		"dead": {Status: "disabled", Labels: []string{"project:stranded"}},
	})
}
