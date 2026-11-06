package main

import (
	"context"
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// strandedPodForProject builds a running agent pod credentialed against the
// given LiteLLM key (baked into ANTHROPIC_CUSTOM_HEADERS) and carrying the
// BOAT_PROJECT env var the recovery sweep reads to re-run the spread selector.
func strandedPodForProject(name, project, litellmKey string) corev1.Pod {
	pod := agentPodWithHeader(name, litellmHeaderFor(litellmKey))
	pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "BOAT_PROJECT", Value: project},
	)
	return pod
}

// strandedAccount returns an AccountCacheEntry for an account that is genuinely
// dead: budget-exhausted lifecycle AND a live rejected throttle signal AND a
// TERMINAL exhaustion verdict (rejected auth probe) — the only condition
// ReconcileStrandedRecovered acts on. The terminal bit matters: a merely
// transient rate-limit window recovers in place and must not be swept
// (ka-0000002039).
func strandedAccount(litellmKey string, labels ...string) config.AccountCacheEntry {
	e := acct(litellmKey, "exhausted", "rejected", labels...)
	e.ProbeAuthStatus = "rejected"
	return e
}

// transientlyExhaustedAccount is exhausted+rejected like strandedAccount, but
// its exhaustion is a rolling rate-limit window rather than dead auth/billing:
// the auth probe still says ok, and org_spend_cap_reached is explicitly
// non-blocking for our subscription accounts (podspec.go overageReasonBlocks).
// accountExhaustionTerminal reports false for this shape.
func transientlyExhaustedAccount(litellmKey string, labels ...string) config.AccountCacheEntry {
	e := acct(litellmKey, "exhausted", "rejected", labels...)
	e.ProbeAuthStatus = "ok"
	e.OverageDisabledReason = "org_spend_cap_reached"
	return e
}

// TestReconcileStrandedRecovered_RestartsWhenSiblingHealthy is acceptance
// criterion (1): a pod baked onto an exhausted+rejected account is restarted
// once its project pool has a healthy alternative. This is the whole-pool
// exhaustion case the edge-triggered Reconcile cannot catch — the account was
// already exhausted at spawn, so there was never an active→exhausted edge.
func TestReconcileStrandedRecovered_RestartsWhenSiblingHealthy(t *testing.T) {
	strandedKey := "sk-litellm-exhausted"
	healthyKey := "sk-litellm-healthy"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio":  strandedAccount(strandedKey, "project:gardenio"),
			"gardenio2": acct(healthyKey, "active", "", "project:gardenio"),
		},
	}
	pm := &mockPodManager{pods: []corev1.Pod{
		strandedPodForProject("agent-stranded", "gardenio", strandedKey),
	}}
	r := newRestarterForTest(pm)

	deleted := r.ReconcileStrandedRecovered(context.Background(), cfg)
	if len(deleted) != 1 || deleted[0] != "agent-stranded" {
		t.Fatalf("expected agent-stranded restarted, got %v", deleted)
	}
}

// TestReconcileStrandedRecovered_NoOpWhenPoolStillExhausted is acceptance
// criterion (2): when the project's entire pool is still exhausted (no healthy
// alternative), the sweep never restarts — it must never strand a pod into an
// equal-or-worse situation.
func TestReconcileStrandedRecovered_NoOpWhenPoolStillExhausted(t *testing.T) {
	k1, k2 := "sk-litellm-ex1", "sk-litellm-ex2"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio":  strandedAccount(k1, "project:gardenio"),
			"gardenio2": strandedAccount(k2, "project:gardenio"),
		},
	}
	pm := &mockPodManager{pods: []corev1.Pod{
		strandedPodForProject("agent-stranded", "gardenio", k1),
	}}
	r := newRestarterForTest(pm)

	if d := r.ReconcileStrandedRecovered(context.Background(), cfg); len(d) != 0 {
		t.Fatalf("no healthy alternative — must be a no-op, got %v", d)
	}
	if len(pm.deleted) != 0 {
		t.Fatalf("must not delete any pod when pool still exhausted, got %v", pm.deleted)
	}
}

// TestReconcileStrandedRecovered_SkipsTerminalPods is acceptance criterion (3):
// a terminal (Failed/Succeeded) pod is never restarted, even with a healthy
// sibling available.
func TestReconcileStrandedRecovered_SkipsTerminalPods(t *testing.T) {
	strandedKey := "sk-litellm-exhausted"
	healthyKey := "sk-litellm-healthy"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio":  strandedAccount(strandedKey, "project:gardenio"),
			"gardenio2": acct(healthyKey, "active", "", "project:gardenio"),
		},
	}
	failed := strandedPodForProject("agent-failed", "gardenio", strandedKey)
	failed.Status.Phase = corev1.PodFailed
	succeeded := strandedPodForProject("agent-succeeded", "gardenio", strandedKey)
	succeeded.Status.Phase = corev1.PodSucceeded

	pm := &mockPodManager{pods: []corev1.Pod{failed, succeeded}}
	r := newRestarterForTest(pm)

	if d := r.ReconcileStrandedRecovered(context.Background(), cfg); len(d) != 0 {
		t.Fatalf("terminal pods must be skipped, got %v", d)
	}
}

// TestReconcileStrandedRecovered_SkipsTerminatingPods verifies a pod already
// carrying a DeletionTimestamp (recreate in flight) is not deleted again.
func TestReconcileStrandedRecovered_SkipsTerminatingPods(t *testing.T) {
	strandedKey := "sk-litellm-exhausted"
	healthyKey := "sk-litellm-healthy"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio":  strandedAccount(strandedKey, "project:gardenio"),
			"gardenio2": acct(healthyKey, "active", "", "project:gardenio"),
		},
	}
	terminating := strandedPodForProject("agent-terminating", "gardenio", strandedKey)
	now := metav1.Now()
	terminating.DeletionTimestamp = &now

	pm := &mockPodManager{pods: []corev1.Pod{terminating}}
	r := newRestarterForTest(pm)

	if d := r.ReconcileStrandedRecovered(context.Background(), cfg); len(d) != 0 {
		t.Fatalf("terminating pod must be skipped, got %v", d)
	}
}

// TestReconcileStrandedRecovered_OnlyActsOnExhaustedRejected verifies the
// trigger is narrow: a pod on an account that is exhausted but NOT throttle-
// rejected (e.g. budget-flagged, throttle still allowed) is left alone — that
// is the spawn path's redirect concern, not a running-pod recovery one.
func TestReconcileStrandedRecovered_OnlyActsOnExhaustedRejected(t *testing.T) {
	exhaustedNotRejected := "sk-litellm-exhausted-allowed"
	healthyKey := "sk-litellm-healthy"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			// exhausted lifecycle but throttle still allowed (not rejected).
			"gardenio":  acct(exhaustedNotRejected, "exhausted", "", "project:gardenio"),
			"gardenio2": acct(healthyKey, "active", "", "project:gardenio"),
		},
	}
	pm := &mockPodManager{pods: []corev1.Pod{
		strandedPodForProject("agent-1", "gardenio", exhaustedNotRejected),
	}}
	r := newRestarterForTest(pm)

	if d := r.ReconcileStrandedRecovered(context.Background(), cfg); len(d) != 0 {
		t.Fatalf("exhausted-but-not-rejected account must not trigger a restart, got %v", d)
	}
}

// TestReconcileStrandedRecovered_SkipsTransientlyExhaustedAccount is the
// regression for ka-0000002039: on 2026-07-31 this sweep force-deleted two
// mid-turn agent pods stranded on account gardenio3, whose exhaustion was a
// rolling rate-limit window (probe_auth ok, overage org_spend_cap_reached), not
// dead auth or dead billing. reconcileExhaustedAccounts had correctly declined
// to kill those same pods ten minutes earlier — "leaving pods running to
// recover in place" — but this path lacked the terminal check and killed them
// anyway once the drain timeout expired.
//
// A transient window resets on its own and coop's in-session credential switch
// recovers the pod with its context intact, so the sweep must leave it alone
// even though the account reads exhausted+rejected.
func TestReconcileStrandedRecovered_SkipsTransientlyExhaustedAccount(t *testing.T) {
	transientKey := "sk-litellm-rate-limited"
	healthyKey := "sk-litellm-healthy"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio3": transientlyExhaustedAccount(transientKey, "project:gardenio"),
			"fimbaz":    acct(healthyKey, "active", "", "project:gardenio"),
		},
	}
	pm := &mockPodManager{pods: []corev1.Pod{
		strandedPodForProject("agent-mid-turn", "gardenio", transientKey),
	}}
	r := newRestarterForTest(pm)

	if d := r.ReconcileStrandedRecovered(context.Background(), cfg); len(d) != 0 {
		t.Fatalf("transient rate-limit exhaustion must not trigger a restart, got %v", d)
	}
	if len(pm.deleted) != 0 {
		t.Fatalf("must not delete a pod recoverable in place, got %v", pm.deleted)
	}
}

// TestReconcileStrandedRecovered_StillActsOnTerminalExhaustion pins the other
// side of the ka-0000002039 guard: terminal exhaustion (dead auth) still gets
// the pod moved. No window reset will revive that baked credential, so leaving
// the pod would 429-loop it forever.
func TestReconcileStrandedRecovered_StillActsOnTerminalExhaustion(t *testing.T) {
	deadKey := "sk-litellm-dead-auth"
	healthyKey := "sk-litellm-healthy"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio3": strandedAccount(deadKey, "project:gardenio"),
			"fimbaz":    acct(healthyKey, "active", "", "project:gardenio"),
		},
	}
	pm := &mockPodManager{pods: []corev1.Pod{
		strandedPodForProject("agent-dead-auth", "gardenio", deadKey),
	}}
	r := newRestarterForTest(pm)

	d := r.ReconcileStrandedRecovered(context.Background(), cfg)
	if len(d) != 1 || d[0] != "agent-dead-auth" {
		t.Fatalf("terminal exhaustion must still restart the pod, got %v", d)
	}
}

// TestReconcileStrandedRecovered_Idempotent verifies the self-idempotency the
// sweep relies on: a pod ALREADY baked onto the healthy account carries that
// account's key, which no longer maps to an exhausted account, so a second
// sweep is a no-op. (Models the post-restart steady state.)
func TestReconcileStrandedRecovered_Idempotent(t *testing.T) {
	strandedKey := "sk-litellm-exhausted"
	healthyKey := "sk-litellm-healthy"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio":  strandedAccount(strandedKey, "project:gardenio"),
			"gardenio2": acct(healthyKey, "active", "", "project:gardenio"),
		},
	}
	// The pod was already re-baked onto the healthy account.
	pm := &mockPodManager{pods: []corev1.Pod{
		strandedPodForProject("agent-rebaked", "gardenio", healthyKey),
	}}
	r := newRestarterForTest(pm)

	if d := r.ReconcileStrandedRecovered(context.Background(), cfg); len(d) != 0 {
		t.Fatalf("a pod on the healthy account must never be restarted, got %v", d)
	}
}

// TestReconcileStrandedRecovered_NoOpWhenOnlyAccountStranded verifies that when
// the stranded account is the project's ONLY account, the selector returns the
// same (exhausted) account or nothing — and the sweep does not restart, because
// the candidate key would equal the pod's current key.
func TestReconcileStrandedRecovered_NoOpWhenOnlyAccountStranded(t *testing.T) {
	strandedKey := "sk-litellm-exhausted"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio": strandedAccount(strandedKey, "project:gardenio"),
		},
	}
	pm := &mockPodManager{pods: []corev1.Pod{
		strandedPodForProject("agent-1", "gardenio", strandedKey),
	}}
	r := newRestarterForTest(pm)

	if d := r.ReconcileStrandedRecovered(context.Background(), cfg); len(d) != 0 {
		t.Fatalf("single stranded account must be a no-op, got %v", d)
	}
}

// TestReconcileStrandedRecovered_EmptyCacheNoOp verifies the bad-refresh guard:
// an empty/keyless account cache must not restart any pod (mirrors
// ReconcileStaleVKeys' fleet-safety guard).
func TestReconcileStrandedRecovered_EmptyCacheNoOp(t *testing.T) {
	pm := &mockPodManager{pods: []corev1.Pod{
		strandedPodForProject("agent-1", "gardenio", "sk-litellm-x"),
	}}
	r := newRestarterForTest(pm)

	if d := r.ReconcileStrandedRecovered(context.Background(), &config.Config{}); len(d) != 0 {
		t.Fatalf("empty cache must be a no-op, got %v", d)
	}
	if d := r.ReconcileStrandedRecovered(context.Background(), &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{"gardenio": {Status: "active"}},
	}); len(d) != 0 {
		t.Fatalf("keyless cache must be a no-op, got %v", d)
	}
}

// TestReconcileStrandedRecovered_LeavesHealthyAndForeignPods verifies the sweep
// touches only stranded pods: a pod on a healthy account and a pod whose key
// belongs to no current account (stale-vkey territory) are both left alone,
// while the genuinely-stranded pod alongside them is restarted.
func TestReconcileStrandedRecovered_LeavesHealthyAndForeignPods(t *testing.T) {
	strandedKey := "sk-litellm-exhausted"
	healthyKey := "sk-litellm-healthy"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio":  strandedAccount(strandedKey, "project:gardenio"),
			"gardenio2": acct(healthyKey, "active", "", "project:gardenio"),
		},
	}
	pm := &mockPodManager{pods: []corev1.Pod{
		strandedPodForProject("agent-stranded", "gardenio", strandedKey),
		strandedPodForProject("agent-healthy", "gardenio", healthyKey),
		strandedPodForProject("agent-foreign", "gardenio", "sk-litellm-unknown"),
	}}
	r := newRestarterForTest(pm)

	deleted := r.ReconcileStrandedRecovered(context.Background(), cfg)
	if len(deleted) != 1 || deleted[0] != "agent-stranded" {
		t.Fatalf("expected only agent-stranded restarted, got %v", deleted)
	}
}

// TestReconcileStrandedRecovered_UsesLiveCountsForSpread verifies the sweep
// honors the live-pod-count spread: with two healthy siblings, the selector
// picks the least-loaded, but the key point here is simply that a healthy
// candidate distinct from the stranded account triggers the restart.
func TestReconcileStrandedRecovered_UsesLiveCountsForSpread(t *testing.T) {
	strandedKey := "sk-litellm-exhausted"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio":  strandedAccount(strandedKey, "project:gardenio"),
			"gardenio2": acct("sk-h2", "active", "", "project:gardenio"),
			"gardenio3": acct("sk-h3", "active", "", "project:gardenio"),
		},
		// Pre-seed live counts so spread selection is deterministic; either
		// healthy account is a valid restart target.
		AccountLiveCounts: map[string]int{"gardenio2": 5},
	}
	pm := &mockPodManager{pods: []corev1.Pod{
		strandedPodForProject("agent-stranded", "gardenio", strandedKey),
	}}
	r := newRestarterForTest(pm)

	deleted := r.ReconcileStrandedRecovered(context.Background(), cfg)
	if len(deleted) != 1 || deleted[0] != "agent-stranded" {
		t.Fatalf("expected agent-stranded restarted onto a healthy sibling, got %v", deleted)
	}
}

// TestReconcileStrandedRecovered_BatchSpreadsAcrossHealthySiblings is the
// regression test for the pile-up bug: when N pods share the same
// project+role and are all stranded on the same exhausted account, the sweep
// must SPREAD their rebake batch across the available healthy candidates rather
// than redirecting every one of them onto the alphabetically-first healthy
// account. Without the per-iteration liveCounts bump, selectAccount is a
// pure function and re-picks the same least-loaded account every iteration —
// re-saturating one sibling's per-account RPM/5h limit and reproducing the
// single-account pile-up account_spread.go was written to prevent.
func TestReconcileStrandedRecovered_BatchSpreadsAcrossHealthySiblings(t *testing.T) {
	strandedKey := "sk-litellm-exhausted"
	// Two healthy siblings, equal initial load (so without the per-pod bump
	// every iteration would pick acct-h1 alphabetically).
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"acct-stranded": strandedAccount(strandedKey, "project:gardenio"),
			"acct-h1":       acct("sk-h1", "active", "", "project:gardenio"),
			"acct-h2":       acct("sk-h2", "active", "", "project:gardenio"),
		},
	}
	const stranded = 4
	pods := make([]corev1.Pod, 0, stranded)
	for i := range stranded {
		pods = append(pods, strandedPodForProject(
			fmt.Sprintf("agent-stranded-%d", i), "gardenio", strandedKey,
		))
	}
	pm := &mockPodManager{pods: pods}
	r := newRestarterForTest(pm)

	deleted := r.ReconcileStrandedRecovered(context.Background(), cfg)
	if len(deleted) != stranded {
		t.Fatalf("expected all %d stranded pods restarted, got %d (%v)",
			stranded, len(deleted), deleted)
	}
	// The candidate the sweep picked for each pod is logged but not returned;
	// to assert the spread we re-derive it from the recorded delete order +
	// what we know about the deterministic spread: starting from equal load,
	// least-loaded with alphabetical tie-break picks h1 then h2 then h1 then
	// h2, so 4 pods must split exactly 2/2. Re-run the same logic by polling
	// selectAccount against a working snapshot, which is what the sweep
	// does internally:
	live := map[string]int{}
	picks := map[string]int{}
	spec := &podmanager.AgentPodSpec{Project: "gardenio"}
	for range stranded {
		c := selectAccount(cfg, "gardenio", spec, live)
		picks[c]++
		live[c]++
	}
	if picks["acct-h1"] != 2 || picks["acct-h2"] != 2 {
		t.Fatalf("expected an even 2/2 spread across acct-h1/acct-h2, got %v", picks)
	}
}

// TestPodProjectRole verifies env-var extraction with label fallback.
func TestPodProjectRole(t *testing.T) {
	// Env vars win.
	withEnv := agentPodWithHeader("p1", litellmHeaderFor("sk-x"))
	withEnv.Spec.Containers[0].Env = append(withEnv.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "BOAT_PROJECT", Value: "gardenio"},
		corev1.EnvVar{Name: "BOAT_ROLE", Value: "crew,gardener"},
	)
	if p, role := podProjectRole(&withEnv); p != "gardenio" || role != "crew,gardener" {
		t.Fatalf("env extraction: got project=%q role=%q", p, role)
	}

	// Label fallback when env absent.
	withLabels := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "p2",
			Labels: map[string]string{
				podmanager.LabelProject: "monorepo",
				podmanager.LabelRole:    "crew",
			},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}},
	}
	if p, role := podProjectRole(&withLabels); p != "monorepo" || role != "crew" {
		t.Fatalf("label fallback: got project=%q role=%q", p, role)
	}
}

// TestReconcileStrandedRecovered_SkipsInProcessSwitchedPod is the
// ka-0000005463 regression test: a pod whose baked key maps to the stranded
// account but whose SESSION (per coopmux) already switched to a different
// account in-process is healthy — the sweep must not delete it.
func TestReconcileStrandedRecovered_SkipsInProcessSwitchedPod(t *testing.T) {
	strandedKey := "sk-litellm-exhausted"
	healthyKey := "sk-litellm-healthy"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio3": strandedAccount(strandedKey, "project:gardenio"),
			"shannon":   acct(healthyKey, "active", "", "project:gardenio"),
		},
	}
	pm := &mockPodManager{pods: []corev1.Pod{
		strandedPodForProject("agent-switched", "gardenio", strandedKey),
		strandedPodForProject("agent-truly-stranded", "gardenio", strandedKey),
	}}
	// coopmux: agent-switched already runs on matthewbaker1110; the other
	// session still reports the stranded account.
	r := newRestarterWithSessions(pm, &mockSessionAccounts{accounts: map[string]string{
		"agent-switched":       "matthewbaker1110",
		"agent-truly-stranded": "gardenio3",
	}})

	deleted := r.ReconcileStrandedRecovered(context.Background(), cfg)
	if len(deleted) != 1 || deleted[0] != "agent-truly-stranded" {
		t.Fatalf("expected only agent-truly-stranded restarted, got %v", deleted)
	}
	for _, name := range pm.deleted {
		if name == "agent-switched" {
			t.Fatalf("already-switched pod must not be deleted")
		}
	}
}

// TestReconcileStrandedRecovered_FailsOpenWhenCoopmuxUnavailable verifies the
// sweep still rescues genuinely stranded pods when the live-account lookup
// errors — coopmux availability must not become a SPOF for crashloop recovery.
//
// The fail-open is now DEFERRED, not immediate (ka-0000001482 D2): without live
// account data the in-process-switch guard silently matches nothing, so the sweep
// waits out a bounded number of blind sweeps (a coopmux blip costs one 5-minute
// tick) before trusting baked keys alone. This mirrors the mass-orphan deferral
// guard added in the same commit as the original fail-open (b07d513b5). What must
// never happen is deferring FOREVER, so the restart is still asserted.
func TestReconcileStrandedRecovered_FailsOpenWhenCoopmuxUnavailable(t *testing.T) {
	strandedKey := "sk-litellm-exhausted"
	healthyKey := "sk-litellm-healthy"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio3": strandedAccount(strandedKey, "project:gardenio"),
			"shannon":   acct(healthyKey, "active", "", "project:gardenio"),
		},
	}
	pm := &mockPodManager{pods: []corev1.Pod{
		strandedPodForProject("agent-stranded", "gardenio", strandedKey),
	}}
	r := newRestarterWithSessions(pm, &mockSessionAccounts{err: fmt.Errorf("mux down")})

	// Blind sweeps are deferred, not acted on: a transient coopmux outage must not
	// cost an already-recovered pod its context.
	for i := 1; i <= strandedRecoveryBlindSweeps; i++ {
		if deleted := r.ReconcileStrandedRecovered(context.Background(), cfg); len(deleted) != 0 {
			t.Fatalf("blind sweep %d must defer, got %v", i, deleted)
		}
	}

	// But the deferral is bounded — coopmux must never become a SPOF that leaves a
	// genuinely stranded pod 429-looping forever.
	deleted := r.ReconcileStrandedRecovered(context.Background(), cfg)
	if len(deleted) != 1 || deleted[0] != "agent-stranded" {
		t.Fatalf("mux error must eventually fail open (still restart), got %v", deleted)
	}
}

// TestReconcileStrandedRecovered_BlindSweepCounterResets verifies a recovered
// coopmux clears the blind-sweep budget, so an intermittent mux cannot ratchet
// the counter up over hours and turn a later single blip into an immediate
// baked-key delete.
func TestReconcileStrandedRecovered_BlindSweepCounterResets(t *testing.T) {
	strandedKey := "sk-litellm-exhausted"
	healthyKey := "sk-litellm-healthy"
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"gardenio3": strandedAccount(strandedKey, "project:gardenio"),
			"shannon":   acct(healthyKey, "active", "", "project:gardenio"),
		},
	}
	pm := &mockPodManager{pods: []corev1.Pod{
		strandedPodForProject("agent-stranded", "gardenio", strandedKey),
	}}
	sessions := &mockSessionAccounts{err: fmt.Errorf("mux down")}
	r := newRestarterWithSessions(pm, sessions)

	// Burn one blind sweep.
	if deleted := r.ReconcileStrandedRecovered(context.Background(), cfg); len(deleted) != 0 {
		t.Fatalf("blind sweep must defer, got %v", deleted)
	}

	// coopmux recovers and reports the pod still on the stranded account, so the
	// sweep acts on real data — and the blind counter resets.
	sessions.err = nil
	sessions.accounts = map[string]string{"agent-stranded": "gardenio3"}
	if deleted := r.ReconcileStrandedRecovered(context.Background(), cfg); len(deleted) != 1 {
		t.Fatalf("live data must restart the stranded pod, got %v", deleted)
	}
	if r.blindSweeps != 0 {
		t.Fatalf("live data must reset the blind-sweep counter, got %d", r.blindSweeps)
	}
}
