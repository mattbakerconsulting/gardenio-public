package main

import (
	"context"
	"fmt"
	"maps"
	"testing"
	"time"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/subscriber"
)

// account_spread_spawn_e2e_test.go — end-to-end coverage for the spawn-time
// account picker (selectAccount) and its failover/redirect/role-partition
// hardening, driven through the REAL spawn path rather than the pure selector
// (task gd-sdah4Peq0x, epic ka-0000000958 — E2E coverage-gap sprint).
//
// WHY THIS EXISTS (the gap):
//
//   - selectAccount has thorough UNIT coverage (account_spread_test.go),
//     but every case calls the pure selector directly and inspects its returned
//     account NAME. Nothing drives the production seam: buildAgentPodSpec ->
//     applyCommonConfig -> applyAccountConfig baking the actual
//     ANTHROPIC_CUSTOM_HEADERS env, then reading the baked LiteLLM KEY back out
//     (podLiteLLMKey / liteLLMKeyFromHeader) to reconstruct the same baked-key
//     histogram the operator gets from `kubectl get pods -o json | jq`.
//   - No test threads the optimistic live-count bump the real spawn loop applies
//     (main.go: CreateAgentPod then cfg.BumpAccountLiveCount(spec.ResolvedAccount)),
//     so the burst-spread behavior — the whole point of the spreader — was never
//     exercised as a burst over the full stack.
//   - The account-auth E2E (tests/e2e/scripts/test-account-auth.sh scenario 6)
//     SKIPs without a live controller and only checks a config:auth field switch;
//     it never drives the spawn-time picker.
//
// These tests seed a project pool exactly as the task's repro describes (one
// out_of_credits, one org_spend_cap_reached, one keyless-with-base_url, one
// healthy), spawn a burst, and assert the baked-key histogram spreads
// least-loaded across the USABLE accounts while the keyless account is never
// baked. They then assert the fsm-triage Bedrock partition (exclusive, no spill,
// never offered to non-triage roles) and the primary-free failover contract
// (forward failover works, no switchback, recovery via the sweep) as a single
// episode driven through Handle + SweepStaleExhausted.
//
// CORRECTNESS NOTE on the overage accounts (ka-0000003769, ka-0000004929): the
// task's proposed assertion — "assert the first three are excluded but the
// spend-cap account is KEPT" — describes a STALE mental model. The
// operator-corrected, code-faithful behavior (overageReasonBlocks) is that BOTH
// out_of_credits AND org_spend_cap_reached are benign subscription-overage
// no-ops and stay in the pool; ONLY the keyless-with-base_url account (which
// would 401 every agent) is excluded. This e2e asserts that real behavior, in
// lockstep with the unit test TestSelectSpreadAccount_SkipsKeylessButKeepsSubscriptionOverage.

const e2eLiteLLM = "https://litellm.app.devops.acme.example"

// spawnPoolAccount builds a LiteLLM-proxied project-pool account for the spawn
// e2e: a base_url + virtual key so it is otherwise spawnable, with the given
// status / throttle / overage-reason. A distinct key per account lets the test
// reverse the baked ANTHROPIC_CUSTOM_HEADERS back to an account name, exactly as
// podLiteLLMKey does on a live pod.
func spawnPoolAccount(key, status, throttle, overageReason string) config.AccountCacheEntry {
	return config.AccountCacheEntry{
		Provider:              "anthropic",
		BaseURL:               e2eLiteLLM,
		Credential:            "secret:gardenio-oauth",
		LiteLLMKey:            key,
		Status:                status,
		ThrottleStatus:        throttle,
		OverageDisabledReason: overageReason,
		Labels:                []string{"project:gardenio"},
	}
}

// spawnBurst spawns n crew agents for `project` through the REAL spawn path
// (buildAgentPodSpec), threading the optimistic live-count bump exactly as
// main.go's spawn loop does, and returns the baked-key histogram keyed by
// account NAME (reversing each pod's baked LiteLLM key). A spec with no baked
// ANTHROPIC_CUSTOM_HEADERS (e.g. a blocked spawn or keyless account) is counted
// under "" so callers can assert it never happened.
func spawnBurst(t *testing.T, cfg *config.Config, project, role string, n int) map[string]int {
	t.Helper()
	// key -> account name reverse index, mirroring countLivePodsByAccount.
	keyToAccount := make(map[string]string, len(cfg.AccountCache))
	for name, acct := range cfg.AccountCache {
		if acct.LiteLLMKey != "" {
			keyToAccount[acct.LiteLLMKey] = name
		}
	}

	hist := make(map[string]int)
	for i := range n {
		ev := subscriber.Event{
			Project:   project,
			Role:      role,
			AgentName: fmt.Sprintf("%s-%d", role, i),
			Metadata:  map[string]string{},
		}
		spec := buildAgentPodSpec(cfg, ev)

		key := liteLLMKeyFromHeader(spec.Env["ANTHROPIC_CUSTOM_HEADERS"])
		name := keyToAccount[key] // "" when no header / unknown key
		hist[name]++

		// Optimistically bump the resolved account's live count so back-to-back
		// spawns in this window keep spreading (main.go post-CreateAgentPod).
		cfg.BumpAccountLiveCount(spec.ResolvedAccount)
	}
	return hist
}

// TestSpawnSpread_BurstBakedKeyHistogram_ExcludesKeylessKeepsOverage is the core
// spawn-path e2e: seed the task's exact pool and spawn a 12-agent burst, then
// assert the baked-key histogram spreads evenly across the three USABLE accounts
// (healthy + out_of_credits + org_spend_cap_reached) and NEVER bakes the keyless
// account's key. This drives applyAccountConfig end-to-end and reads back the
// literal ANTHROPIC_CUSTOM_HEADERS the pod would carry.
func TestSpawnSpread_BurstBakedKeyHistogram_ExcludesKeylessKeepsOverage(t *testing.T) {
	cfg := &config.Config{
		LiteLLMURL:   e2eLiteLLM,
		ProjectCache: map[string]config.ProjectCacheEntry{"gardenio": {Account: "acct-healthy"}},
		AccountCache: map[string]config.AccountCacheEntry{
			// Keyless LiteLLM account (base_url, no key) — would 401. EXCLUDED.
			"acct-keyless": {Provider: "anthropic", BaseURL: e2eLiteLLM, LiteLLMKey: "", Status: "", ThrottleStatus: "allowed", Labels: []string{"project:gardenio"}},
			// out_of_credits — subscription overage off, still bills the subscription. KEPT.
			"acct-no-credits": spawnPoolAccount("sk-nocredit", "active", "allowed", "out_of_credits"),
			// org_spend_cap_reached — likewise a subscription no-op. KEPT.
			"acct-spend-cap": spawnPoolAccount("sk-spendcap", "active", "allowed", "org_spend_cap_reached"),
			// Fully healthy. KEPT.
			"acct-healthy": spawnPoolAccount("sk-healthy", "active", "allowed", ""),
		},
	}

	const burst = 12
	hist := spawnBurst(t, cfg, "gardenio", "crew", burst)

	// The keyless account's key must never be baked into any pod.
	if hist["acct-keyless"] != 0 {
		t.Errorf("keyless account must never be baked, got %d pods on it", hist["acct-keyless"])
	}
	// Every spawn must have baked SOME usable account's key (no empty/synthetic-env pods).
	if hist[""] != 0 {
		t.Errorf("every burst spawn should bake a usable account key; %d pods baked no ANTHROPIC_CUSTOM_HEADERS", hist[""])
	}

	usable := []string{"acct-healthy", "acct-no-credits", "acct-spend-cap"}
	total := 0
	for _, name := range usable {
		if hist[name] == 0 {
			t.Errorf("usable account %q took zero pods — burst did not spread onto it (hist=%v)", name, hist)
		}
		total += hist[name]
	}
	if total != burst {
		t.Errorf("usable accounts took %d pods, want the whole burst of %d (hist=%v)", total, burst, hist)
	}
	// Even spread: 12 pods / 3 usable accounts == 4 each (least-loaded rotation).
	for _, name := range usable {
		if hist[name] != burst/len(usable) {
			t.Errorf("expected even spread of %d per usable account, got %d on %q (hist=%v)",
				burst/len(usable), hist[name], name, hist)
		}
	}
}

// TestSpawnSpread_BurstNeverPilesOnPrimary is the anti-regression for the
// original defect selectAccount fixes (ka-0000004861, ka-0000003255): every
// pod used to bake the SAME cached primary key, re-saturating one account's RPM
// while siblings sat idle. Drive a burst and prove no single account absorbs more
// than a fair share + 1 — i.e. the load genuinely spreads instead of piling on
// the ProjectCache primary.
func TestSpawnSpread_BurstNeverPilesOnPrimary(t *testing.T) {
	cfg := &config.Config{
		LiteLLMURL:   e2eLiteLLM,
		ProjectCache: map[string]config.ProjectCacheEntry{"gardenio": {Account: "acct-a"}}, // primary
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-a": spawnPoolAccount("sk-a", "active", "allowed", ""),
			"acct-b": spawnPoolAccount("sk-b", "active", "allowed", ""),
			"acct-c": spawnPoolAccount("sk-c", "active", "allowed", ""),
		},
	}

	const burst = 30
	hist := spawnBurst(t, cfg, "gardenio", "crew", burst)

	fairShare := burst / 3
	for name, n := range hist {
		if name == "" {
			t.Fatalf("a spawn baked no account key (hist=%v)", hist)
		}
		if n > fairShare+1 {
			t.Errorf("account %q absorbed %d pods (> fair share %d+1) — load piled up instead of spreading (hist=%v)",
				name, n, fairShare, hist)
		}
	}
	// Sanity: the primary did not swallow the burst.
	if hist["acct-a"] >= burst {
		t.Errorf("primary acct-a took the whole burst (%d) — no spread happened", hist["acct-a"])
	}
}

// TestSpawnSpread_FsmTriageBedrockPartition proves the role partition through the
// spawn path (ka-0000005223, commit b348fea9): an fsm-triage agent is pinned to
// its EXCLUSIVE Bedrock account and never spills onto the general pool — even
// when Bedrock is exhausted — while ordinary roles never select Bedrock, only the
// general project pool.
func TestSpawnSpread_FsmTriageBedrockPartition(t *testing.T) {
	// Bedrock is exclusive to fsm-triage AND exhausted — the "never spills even
	// when exhausted" case. Keyed so we can detect it in the histogram.
	bedrock := config.AccountCacheEntry{
		Provider:        "anthropic",
		BaseURL:         e2eLiteLLM,
		Credential:      "secret:bedrock-oauth",
		LiteLLMKey:      "sk-bedrock",
		Model:           "bedrock-claude",
		Status:          "exhausted",
		ExclusiveToRole: "fsm-triage",
		Labels:          []string{"project:gardenio"},
	}
	cfg := &config.Config{
		LiteLLMURL:   e2eLiteLLM,
		ProjectCache: map[string]config.ProjectCacheEntry{"gardenio": {Account: "acct-general-1"}},
		RoleCache:    map[string]config.RoleCacheEntry{"fsm-triage": {Account: "litellm-bedrock"}},
		AccountCache: map[string]config.AccountCacheEntry{
			"litellm-bedrock": bedrock,
			"acct-general-1":  spawnPoolAccount("sk-g1", "active", "allowed", ""),
			"acct-general-2":  spawnPoolAccount("sk-g2", "active", "allowed", ""),
		},
	}

	// (1) fsm-triage burst: every agent stays pinned to the exhausted exclusive
	// Bedrock account — no spill onto the healthy general pool.
	triageHist := spawnBurst(t, cfg, "gardenio", "fsm-triage", 6)
	if triageHist["litellm-bedrock"] != 6 {
		t.Errorf("fsm-triage must pin ALL pods to its exclusive Bedrock account, got %v", triageHist)
	}
	if triageHist["acct-general-1"]+triageHist["acct-general-2"] != 0 {
		t.Errorf("exhausted exclusive Bedrock must NOT spill fsm-triage onto the general pool, got %v", triageHist)
	}

	// (2) ordinary-role burst on a FRESH cfg (reset live counts): never lands on
	// Bedrock, only the general pool.
	cfg.SetAccountLiveCounts(map[string]int{})
	crewHist := spawnBurst(t, cfg, "gardenio", "crew", 8)
	if crewHist["litellm-bedrock"] != 0 {
		t.Errorf("non-triage role must NEVER select the exclusive Bedrock account, got %v", crewHist)
	}
	if crewHist["acct-general-1"]+crewHist["acct-general-2"] != 8 {
		t.Errorf("crew burst should spread entirely across the general pool, got %v", crewHist)
	}
	// And it spreads evenly across the two general accounts.
	if crewHist["acct-general-1"] != 4 || crewHist["acct-general-2"] != 4 {
		t.Errorf("crew burst should spread 4/4 across the general pool, got %v", crewHist)
	}
}

// TestSpawnSpread_LeastLoadedPrefersFreshWindowAccount ties the baked-key
// histogram to the capacity+reset bonus (ka-0000001522): given two healthy
// accounts where one has a fresh 5h window about to reset (idle, use-it-or-lose-
// it) and the other is mid-window, an initial burst should favor the fresh
// account through the real spawn path until the bonus is spent.
func TestSpawnSpread_LeastLoadedPrefersFreshWindowAccount(t *testing.T) {
	// acct-fresh: idle window, resets in ~30m -> high use-it-or-lose-it bonus.
	fresh := spawnPoolAccount("sk-fresh", "active", "allowed", "")
	fresh.Utilization5h = "0.05"
	fresh.RepresentativeClaim = "five_hour"
	fresh.ThrottleReset5hAt = time.Now().UTC().Add(30 * time.Minute).Format(time.RFC3339)

	// acct-mid: half-used, resets far out -> only the small baseline bonus.
	mid := spawnPoolAccount("sk-mid", "active", "allowed", "")
	mid.Utilization5h = "0.50"
	mid.RepresentativeClaim = "five_hour"
	mid.ThrottleReset5hAt = time.Now().UTC().Add(5 * time.Hour).Format(time.RFC3339)

	cfg := &config.Config{
		LiteLLMURL:   e2eLiteLLM,
		ProjectCache: map[string]config.ProjectCacheEntry{"gardenio": {Account: "acct-mid"}},
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-fresh": fresh,
			"acct-mid":   mid,
		},
	}

	// First two spawns should both land on the fresh account (its bonus makes it
	// the least-loaded pick for the first couple of pods before load catches up).
	hist := spawnBurst(t, cfg, "gardenio", "crew", 2)
	if hist["acct-fresh"] < hist["acct-mid"] {
		t.Errorf("fresh-window account should attract the initial burst (use-it-or-lose-it), got %v", hist)
	}
	if hist["acct-fresh"] == 0 {
		t.Errorf("fresh-window account took zero pods despite its reset-proximity bonus, got %v", hist)
	}
}

// --- Scenario C: failover works, switchback is abolished ----------------------

// TestFailoverEpisode_SwitchbackAbolished drives the whole episode through the
// PUBLIC entry points (Handle then SweepStaleExhausted) to prove the primary-free
// contract end-to-end (ka-0000008240): forward failover still works, there is no
// switchback that moves the project back onto its former account, and the parked
// account rejoins the EQUAL spread pool only when the sweep clears its exhausted
// status. It seeds a genuine at-budget rejection (throttle_status=rejected,
// utilization_7d>=0.99) so the failover HARD-exhausts the primary and the
// sweep-recovery path is exercised. (A 429 with utilization HEADROOM — the
// RPM/spend-cap transient of ka-0000003255 Bug 2 — now deprioritizes instead of
// exhausting; that path is covered by TestAuthFailover_DeprioritizesOn429WithHeadroom.)
func TestFailoverEpisode_SwitchbackAbolished(t *testing.T) {
	now := time.Now().UTC().Format(time.RFC3339)
	store := newMockStore(
		account("gd-p", "primary", "gardenio", map[string]any{
			"provider": "anthropic", "fallback": "backup", "status": "active",
			// Genuinely at rolling budget: rejected + 7d utilization maxed.
			"throttle_status": "rejected", "utilization_7d": "1.00", "probe_checked_at": now,
		}),
		account("gd-b", "backup", "gardenio", map[string]any{
			"provider": "anthropic", "status": "active", "throttle_status": "allowed",
		}),
	)
	srv := mockDaemonServer(t, store)
	defer srv.Close()

	f := newAuthFailover(newClient(t, srv), nil, testFailoverLogger(), 5*time.Minute, 24*time.Hour)

	// (1) RPM 429 -> forward failover onto the backup. Handle still works.
	if err := f.Handle(context.Background(), "gardenio", "gd-agent1"); err != nil {
		t.Fatalf("Handle (failover): %v", err)
	}
	st, ok := f.runtime.Get("gardenio")
	if !ok || !st.inFailover() || st.Current != "backup" {
		t.Fatalf("expected failover onto backup, got %+v (ok=%v)", st, ok)
	}
	if store.get("gd-p").Fields["status"] != "exhausted" {
		t.Fatalf("primary should be marked exhausted after failover, got %v", store.get("gd-p").Fields["status"])
	}

	// (2) The parked account's probe stays fresh and allowed — the exact
	// conditions that used to force a switchback back onto it. Run the sweep,
	// which is now the sole recovery path.
	store.mustUpdate(t, "gd-p", map[string]any{
		"throttle_status": "allowed", "probe_checked_at": time.Now().UTC().Format(time.RFC3339),
	})

	f.SweepStaleExhausted(context.Background())

	// (3) No switchback: the project's runtime entry stays on its backup — the
	// sweep never moves a project back onto its former account.
	if st, ok := f.runtime.Get("gardenio"); !ok || !st.inFailover() || st.Current != "backup" {
		t.Errorf("no switchback: expected project still on backup, got %+v (ok=%v)", st, ok)
	}
	// (4) The former account rejoins the EQUAL spread pool: the sweep clears its
	// exhausted status back to active (a peer in the pool, not a privileged home).
	if got := store.get("gd-p").Fields["status"]; got != "active" {
		t.Errorf("sweep should return the recovered account to the pool: status = %v, want active", got)
	}
}

// mustUpdate is a tiny test helper: patch fields on a bead in the mock store,
// failing the test on error. Mirrors how the daemon's typed-field update lands.
func (s *mockStore) mustUpdate(t *testing.T, id string, fields map[string]any) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.beads[id]
	if !ok {
		t.Fatalf("mustUpdate: bead %q not found in mock store", id)
	}
	if b.Fields == nil {
		b.Fields = map[string]any{}
	}
	maps.Copy(b.Fields, fields)
}
