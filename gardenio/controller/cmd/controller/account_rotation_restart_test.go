package main

import (
	"context"
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// mockPodManager implements podmanager.Manager for testing.
type mockPodManager struct {
	pods    []corev1.Pod
	listErr error
	delErr  error
	deleted []string
}

func (m *mockPodManager) CreateAgentPod(_ context.Context, _ podmanager.AgentPodSpec) error {
	return nil
}

func (m *mockPodManager) DeleteAgentPod(_ context.Context, name, _ string) error {
	m.deleted = append(m.deleted, name)
	return m.delErr
}

func (m *mockPodManager) DeleteAllAgentPods(_ context.Context, _ string, _ bool) (int, error) {
	return 0, nil
}

func (m *mockPodManager) ListAgentPods(_ context.Context, _ string, _ map[string]string) ([]corev1.Pod, error) {
	return m.pods, m.listErr
}

func (m *mockPodManager) GetAgentPod(_ context.Context, _, _ string) (*corev1.Pod, error) {
	return nil, nil
}

func (m *mockPodManager) GetNode(_ context.Context, _ string) (*corev1.Node, error) {
	return nil, nil
}

// agentPodWithHeader builds a minimal running agent pod whose "agent"
// container carries the given ANTHROPIC_CUSTOM_HEADERS value.
func agentPodWithHeader(name, customHeaders string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "agent",
				Env: []corev1.EnvVar{
					{Name: "ANTHROPIC_CUSTOM_HEADERS", Value: customHeaders},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// mockSessionAccounts fakes the coopmux live-session-account lookup.
type mockSessionAccounts struct {
	accounts map[string]string
	err      error
}

func (m *mockSessionAccounts) ListSessionAccounts(_ context.Context) (map[string]string, error) {
	return m.accounts, m.err
}

func newRestarterForTest(pods podmanager.Manager) *accountRotationRestarter {
	return newAccountRotationRestarter(pods, "gardenio", nil, slog.Default())
}

func newRestarterWithSessions(pods podmanager.Manager, sessions liveSessionAccounts) *accountRotationRestarter {
	return newAccountRotationRestarter(pods, "gardenio", sessions, slog.Default())
}

// TestAccountRotationRestart_FirstPassSeedsOnly verifies that the very first
// Reconcile only seeds the status snapshot and never restarts pods — there is
// no prior observation to define a transition.
func TestAccountRotationRestart_FirstPassSeedsOnly(t *testing.T) {
	key := "sk-litellm-aaa"
	pm := &mockPodManager{pods: []corev1.Pod{agentPodWithHeader("agent-1", litellmHeaderFor(key))}}
	r := newRestarterForTest(pm)

	deleted := r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio": {Status: "exhausted", LiteLLMKey: key},
	})
	if len(deleted) != 0 {
		t.Fatalf("first pass should not restart any pods, got %v", deleted)
	}
	if len(pm.deleted) != 0 {
		t.Fatalf("first pass should not delete any pods, got %v", pm.deleted)
	}
}

// TestAccountRotationRestart_ExhaustedTransitionRestarts verifies the
// active→exhausted edge restarts the matching pod exactly once — for a TERMINAL
// exhaustion (here a hard probe auth rejection). A transient rate-limit
// exhaustion is covered separately (see the transient-skip tests below).
func TestAccountRotationRestart_ExhaustedTransitionRestarts(t *testing.T) {
	key := "sk-litellm-bbb"
	header := litellmHeaderFor(key)
	pm := &mockPodManager{pods: []corev1.Pod{
		agentPodWithHeader("agent-pinned", header),
		agentPodWithHeader("agent-other-account", litellmHeaderFor("sk-litellm-healthy")),
	}}
	r := newRestarterForTest(pm)

	// Seed: account healthy.
	r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio": {Status: "active", LiteLLMKey: key},
	})

	// Transition: account → exhausted, TERMINAL (probe auth rejected).
	deleted := r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio": {Status: "exhausted", LiteLLMKey: key, ProbeAuthStatus: "rejected"},
	})

	if len(deleted) != 1 || deleted[0] != "agent-pinned" {
		t.Fatalf("expected agent-pinned to be restarted, got %v", deleted)
	}
	// Pod on a different account's key must not be touched.
	for _, name := range pm.deleted {
		if name == "agent-other-account" {
			t.Fatalf("pod on healthy account must not be restarted")
		}
	}
}

// TestAccountRotationRestart_NoRestartWhileStaysExhausted verifies the churn
// guard: once restarted on a TERMINAL transition, subsequent reconciles while
// the account stays exhausted do not restart again.
func TestAccountRotationRestart_NoRestartWhileStaysExhausted(t *testing.T) {
	key := "sk-litellm-ccc"
	pm := &mockPodManager{pods: []corev1.Pod{agentPodWithHeader("agent-1", litellmHeaderFor(key))}}
	r := newRestarterForTest(pm)

	r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio": {Status: "active", LiteLLMKey: key},
	})
	if d := r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio": {Status: "exhausted", LiteLLMKey: key, ProbeAuthStatus: "rejected"},
	}); len(d) != 1 {
		t.Fatalf("transition should restart once, got %v", d)
	}
	// Stays exhausted — must not restart again.
	if d := r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio": {Status: "exhausted", LiteLLMKey: key, ProbeAuthStatus: "rejected"},
	}); len(d) != 0 {
		t.Fatalf("steady exhausted state should not restart, got %v", d)
	}
	if len(pm.deleted) != 1 {
		t.Fatalf("expected exactly one delete across cycles, got %v", pm.deleted)
	}
}

// TestAccountRotationRestart_HealthyAccountNoRestart verifies that an account
// staying healthy (active across cycles) never triggers a restart.
func TestAccountRotationRestart_HealthyAccountNoRestart(t *testing.T) {
	key := "sk-litellm-ddd"
	pm := &mockPodManager{pods: []corev1.Pod{agentPodWithHeader("agent-1", litellmHeaderFor(key))}}
	r := newRestarterForTest(pm)

	for i := range 3 {
		if d := r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{
			"gardenio": {Status: "active", LiteLLMKey: key},
		}); len(d) != 0 {
			t.Fatalf("healthy account should never restart, cycle %d got %v", i, d)
		}
	}
	if len(pm.deleted) != 0 {
		t.Fatalf("healthy account should never delete pods, got %v", pm.deleted)
	}
}

// TestAccountRotationRestart_RecoveryThenReExhaustRestartsAgain verifies that
// after an exhausted→active recovery, a fresh active→exhausted TERMINAL
// transition restarts again (the edge fires each time, not just once ever).
func TestAccountRotationRestart_RecoveryThenReExhaustRestartsAgain(t *testing.T) {
	key := "sk-litellm-eee"
	header := litellmHeaderFor(key)
	pm := &mockPodManager{pods: []corev1.Pod{agentPodWithHeader("agent-1", header)}}
	r := newRestarterForTest(pm)

	term := config.AccountCacheEntry{Status: "exhausted", LiteLLMKey: key, ProbeAuthStatus: "rejected"}
	r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{"gardenio": {Status: "active", LiteLLMKey: key}})
	r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{"gardenio": term})
	r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{"gardenio": {Status: "active", LiteLLMKey: key}})
	d := r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{"gardenio": term})

	if len(d) != 1 {
		t.Fatalf("re-exhaustion after recovery should restart again, got %v", d)
	}
	if len(pm.deleted) != 2 {
		t.Fatalf("expected two restarts across two transitions, got %v", pm.deleted)
	}
}

// TestAccountRotationRestart_TransientExhaustionDoesNotRestart is the PRIMARY
// regression test for ka-0000002085: an account that transitions to exhausted
// on a transient 5h/7d rate-limit window (no terminal auth/billing signal) must
// NOT have its pods deleted — the window resets on its own and the in-session
// coop switch can recover the pod in place. Deleting here is what destroyed
// recoverable work (one pod at 272k ctx) during the incident.
func TestAccountRotationRestart_TransientExhaustionDoesNotRestart(t *testing.T) {
	key := "sk-litellm-transient"
	pm := &mockPodManager{pods: []corev1.Pod{agentPodWithHeader("agent-stuck", litellmHeaderFor(key))}}
	r := newRestarterForTest(pm)

	// Seed: account healthy.
	r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio": {Status: "active", LiteLLMKey: key},
	})

	// Transition to exhausted driven purely by the rolling rate-limit window:
	// throttle rejected, but auth is live (probe_auth_status="ok") and billing
	// is fine. This is transient — it self-heals on window reset.
	deleted := r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio": {
			Status: "exhausted", LiteLLMKey: key,
			ThrottleStatus: "rejected", ProbeAuthStatus: "ok",
		},
	})
	if len(deleted) != 0 {
		t.Fatalf("transient rate-limit exhaustion must NOT delete pods, got %v", deleted)
	}
	if len(pm.deleted) != 0 {
		t.Fatalf("transient rate-limit exhaustion must NOT delete pods, got %v", pm.deleted)
	}
}

// TestAccountRotationRestart_BareExhaustionIsTransient pins that a plain
// Status="exhausted" with NO terminal signal at all (no probe_auth_status, no
// blocking overage) is treated as transient — the common case, since
// reconcileLegacyStatusFields flips Status="exhausted" on the rolling
// rate-limit window (ka-0000002085).
func TestAccountRotationRestart_BareExhaustionIsTransient(t *testing.T) {
	key := "sk-litellm-bare"
	pm := &mockPodManager{pods: []corev1.Pod{agentPodWithHeader("agent-1", litellmHeaderFor(key))}}
	r := newRestarterForTest(pm)

	r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio": {Status: "active", LiteLLMKey: key},
	})
	deleted := r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio": {Status: "exhausted", LiteLLMKey: key},
	})
	if len(deleted) != 0 {
		t.Fatalf("bare exhaustion (no terminal signal) must be treated transient, got %v", deleted)
	}
}

// TestAccountRotationRestart_TerminalBillingRestarts verifies the OTHER terminal
// branch: an account exhausted with a genuinely-blocking overage reason (dead
// billing that no window reset will revive) DOES get its pods restarted.
func TestAccountRotationRestart_TerminalBillingRestarts(t *testing.T) {
	key := "sk-litellm-billing"
	pm := &mockPodManager{pods: []corev1.Pod{agentPodWithHeader("agent-billing", litellmHeaderFor(key))}}
	r := newRestarterForTest(pm)

	r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio": {Status: "active", LiteLLMKey: key},
	})
	// "quota_exceeded" on a METERED account is a blocking overage reason —
	// there the overage path IS the billing path, so billing is dead → terminal.
	// (On a subscription account the same string is benign; see
	// TestAccountExhaustionTerminal.)
	deleted := r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio": {
			Status: "exhausted", LiteLLMKey: key,
			BillingType:           "metered",
			OverageDisabledReason: "quota_exceeded",
		},
	})
	if len(deleted) != 1 || deleted[0] != "agent-billing" {
		t.Fatalf("terminal billing exhaustion should restart the pod, got %v", deleted)
	}
}

// TestAccountExhaustionTerminal unit-tests the classifier directly across the
// transient/terminal boundary (ka-0000002085).
func TestAccountExhaustionTerminal(t *testing.T) {
	tests := []struct {
		name string
		acct config.AccountCacheEntry
		want bool
	}{
		{"bare rate-limit exhaustion is transient", config.AccountCacheEntry{Status: "exhausted"}, false},
		{"throttle rejected + auth ok is transient", config.AccountCacheEntry{Status: "exhausted", ThrottleStatus: "rejected", ProbeAuthStatus: "ok"}, false},
		{"benign subscription overage is transient", config.AccountCacheEntry{Status: "exhausted", OverageDisabledReason: "out_of_credits"}, false},
		{"org spend cap overage is transient", config.AccountCacheEntry{Status: "exhausted", OverageDisabledReason: "org_spend_cap_reached"}, false},
		{"probe auth rejected is terminal", config.AccountCacheEntry{Status: "exhausted", ProbeAuthStatus: "rejected"}, true},
		{"blocking overage reason is terminal on metered", config.AccountCacheEntry{Status: "exhausted", BillingType: "metered", OverageDisabledReason: "account_suspended"}, true},
		// Subscription: no overage reason is terminal — the plan quota is billed by
		// the subscription, so the pod's credential is still live and a window
		// reset revives it (2026-08-28 org_level_disabled outage).
		{"same reason on subscription is transient", config.AccountCacheEntry{Status: "exhausted", OverageDisabledReason: "account_suspended"}, false},
		{"org_level_disabled is transient", config.AccountCacheEntry{Status: "exhausted", OverageDisabledReason: "org_level_disabled"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := accountExhaustionTerminal(tc.acct); got != tc.want {
				t.Errorf("accountExhaustionTerminal(%+v) = %v, want %v", tc.acct, got, tc.want)
			}
		})
	}
}

// TestReconcileStaleVKeys_RestartsRotatedOutPod verifies that a pod whose baked
// LiteLLM key is no longer any account's current key is restarted, while a pod
// carrying a still-valid key is left alone (ka-0000003704).
func TestReconcileStaleVKeys_RestartsRotatedOutPod(t *testing.T) {
	staleKey := "sk-litellm-OLD"
	freshKey := "sk-litellm-NEW"
	pm := &mockPodManager{pods: []corev1.Pod{
		agentPodWithHeader("agent-stale", litellmHeaderFor(staleKey)),
		agentPodWithHeader("agent-fresh", litellmHeaderFor(freshKey)),
	}}
	r := newRestarterForTest(pm)

	// Account now carries only the fresh key (the old one was rotated out).
	deleted := r.ReconcileStaleVKeys(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio": {Status: "active", LiteLLMKey: freshKey},
	})

	if len(deleted) != 1 || deleted[0] != "agent-stale" {
		t.Fatalf("expected only agent-stale restarted, got %v", deleted)
	}
}

// TestReconcileStaleVKeys_EmptySnapshotIsNoOp verifies the fleet-safety guard:
// an account cache with no valid keys (empty/partial refresh) must NOT restart
// any pod, even though every pod's key technically fails set membership.
func TestReconcileStaleVKeys_EmptySnapshotIsNoOp(t *testing.T) {
	pm := &mockPodManager{pods: []corev1.Pod{
		agentPodWithHeader("agent-1", litellmHeaderFor("sk-litellm-x")),
		agentPodWithHeader("agent-2", litellmHeaderFor("sk-litellm-y")),
	}}
	r := newRestarterForTest(pm)

	// No keys at all.
	if d := r.ReconcileStaleVKeys(context.Background(), map[string]config.AccountCacheEntry{}); len(d) != 0 {
		t.Fatalf("empty cache must be a no-op, got %v", d)
	}
	// Accounts present but none carry a LiteLLM key.
	if d := r.ReconcileStaleVKeys(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio": {Status: "active"},
	}); len(d) != 0 {
		t.Fatalf("cache with no valid keys must be a no-op, got %v", d)
	}
	if len(pm.deleted) != 0 {
		t.Fatalf("guard must prevent all deletes, got %v", pm.deleted)
	}
}

// TestReconcileStaleVKeys_SkipsNoKeyAndTerminatingPods verifies pods that don't
// authenticate via LiteLLM (no baked key) and pods already Terminating are not
// restarted.
func TestReconcileStaleVKeys_SkipsNoKeyAndTerminatingPods(t *testing.T) {
	freshKey := "sk-litellm-NEW"

	noKeyPod := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-nokey"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	terminating := agentPodWithHeader("agent-terminating", litellmHeaderFor("sk-litellm-OLD"))
	now := metav1.Now()
	terminating.DeletionTimestamp = &now

	pm := &mockPodManager{pods: []corev1.Pod{noKeyPod, terminating}}
	r := newRestarterForTest(pm)

	deleted := r.ReconcileStaleVKeys(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio": {Status: "active", LiteLLMKey: freshKey},
	})
	if len(deleted) != 0 {
		t.Fatalf("no-key and terminating pods must be skipped, got %v", deleted)
	}
}

// TestReconcileStaleVKeys_AllKeysValidNoOp verifies that when every pod carries
// a key still present on some account, nothing is restarted.
func TestReconcileStaleVKeys_AllKeysValidNoOp(t *testing.T) {
	k1, k2 := "sk-litellm-a", "sk-litellm-b"
	pm := &mockPodManager{pods: []corev1.Pod{
		agentPodWithHeader("agent-1", litellmHeaderFor(k1)),
		agentPodWithHeader("agent-2", litellmHeaderFor(k2)),
	}}
	r := newRestarterForTest(pm)

	d := r.ReconcileStaleVKeys(context.Background(), map[string]config.AccountCacheEntry{
		"acct-1": {Status: "active", LiteLLMKey: k1},
		"acct-2": {Status: "active", LiteLLMKey: k2},
	})
	if len(d) != 0 {
		t.Fatalf("all valid keys should be a no-op, got %v", d)
	}
}

// TestAccountRotationRestart_SkipsInProcessSwitchedPod verifies the terminal
// exhaustion edge does not delete a pod whose session (per coopmux) already
// switched to a different account in-process (ka-0000005463).
func TestAccountRotationRestart_SkipsInProcessSwitchedPod(t *testing.T) {
	key := "sk-litellm-dead"
	pm := &mockPodManager{pods: []corev1.Pod{
		agentPodWithHeader("agent-switched", litellmHeaderFor(key)),
		agentPodWithHeader("agent-stuck", litellmHeaderFor(key)),
	}}
	r := newRestarterWithSessions(pm, &mockSessionAccounts{accounts: map[string]string{
		"agent-switched": "shannon",
		"agent-stuck":    "gardenio3",
	}})

	r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio3": {Status: "active", LiteLLMKey: key},
	})
	deleted := r.Reconcile(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio3": {Status: "exhausted", LiteLLMKey: key, ProbeAuthStatus: "rejected"},
	})

	if len(deleted) != 1 || deleted[0] != "agent-stuck" {
		t.Fatalf("expected only agent-stuck restarted, got %v", deleted)
	}
}

// TestReconcileStaleVKeys_SkipsInProcessSwitchedPod verifies a pod whose baked
// key was rotated out but whose session already switched to a DIFFERENT
// account (live != BOAT_ACCOUNT) is left alone, while a session still on its
// birth account with a rotated key (the true ka-0000003704 crashloop) is
// restarted.
func TestReconcileStaleVKeys_SkipsInProcessSwitchedPod(t *testing.T) {
	staleKey := "sk-litellm-OLD"
	freshKey := "sk-litellm-NEW"
	withBirth := func(name, account string) corev1.Pod {
		pod := agentPodWithHeader(name, litellmHeaderFor(staleKey))
		pod.Spec.Containers[0].Env = append(pod.Spec.Containers[0].Env,
			corev1.EnvVar{Name: "BOAT_ACCOUNT", Value: account},
		)
		return pod
	}
	pm := &mockPodManager{pods: []corev1.Pod{
		withBirth("agent-switched", "gardenio3"),
		withBirth("agent-crashlooping", "gardenio3"),
	}}
	r := newRestarterWithSessions(pm, &mockSessionAccounts{accounts: map[string]string{
		"agent-switched":     "shannon",   // moved off gardenio3 in-process
		"agent-crashlooping": "gardenio3", // still on birth account with dead key
	}})

	deleted := r.ReconcileStaleVKeys(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio3": {Status: "active", LiteLLMKey: freshKey},
	})

	if len(deleted) != 1 || deleted[0] != "agent-crashlooping" {
		t.Fatalf("expected only agent-crashlooping restarted, got %v", deleted)
	}
}

// keylessPodOnAccount builds a running agent pod with NO ANTHROPIC_CUSTOM_HEADERS
// (keyless spawn) pinned to the given BOAT_ACCOUNT — the shape of a pod spawned
// before its account was edited to add a litellm_key (ka-0000004556).
func keylessPodOnAccount(name, account string) corev1.Pod {
	return corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "agent",
				Env: []corev1.EnvVar{
					{Name: "BOAT_ACCOUNT", Value: account},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// TestReconcileKeylessGainedKey_RestartsKeylessPodOnNowKeyedAccount verifies the
// core case: a pod spawned keyless against an account that has since gained a
// litellm_key is restarted so the recreate re-bakes the key (ka-0000004556).
func TestReconcileKeylessGainedKey_RestartsKeylessPodOnNowKeyedAccount(t *testing.T) {
	pm := &mockPodManager{pods: []corev1.Pod{
		keylessPodOnAccount("agent-keyless", "gardenio3"),
	}}
	r := newRestarterForTest(pm)

	deleted := r.ReconcileKeylessGainedKey(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio3": {Status: "active", LiteLLMKey: "sk-litellm-NEW"},
	})
	if len(deleted) != 1 || deleted[0] != "agent-keyless" {
		t.Fatalf("expected agent-keyless restarted, got %v", deleted)
	}
}

// TestReconcileKeylessGainedKey_NoRestartWhenAccountStillKeyless verifies the
// no-false-restart guard: a keyless pod on an account that is STILL keyless
// (direct-provider auth, no litellm_key) is left running.
func TestReconcileKeylessGainedKey_NoRestartWhenAccountStillKeyless(t *testing.T) {
	pm := &mockPodManager{pods: []corev1.Pod{
		keylessPodOnAccount("agent-keyless", "direct-acct"),
	}}
	r := newRestarterForTest(pm)

	// Some OTHER account has a key (so the empty-snapshot guard passes), but the
	// pod's own account does not — no drift, no restart.
	deleted := r.ReconcileKeylessGainedKey(context.Background(), map[string]config.AccountCacheEntry{
		"direct-acct": {Status: "active"},
		"other-acct":  {Status: "active", LiteLLMKey: "sk-litellm-other"},
	})
	if len(deleted) != 0 {
		t.Fatalf("keyless pod on a still-keyless account must not restart, got %v", deleted)
	}
}

// TestReconcileKeylessGainedKey_SkipsKeyedPods verifies a pod that already
// carries a baked key is not touched — that drift is ReconcileStaleVKeys' job,
// and a keyed pod on a keyed account is simply healthy.
func TestReconcileKeylessGainedKey_SkipsKeyedPods(t *testing.T) {
	key := "sk-litellm-live"
	keyed := agentPodWithHeader("agent-keyed", litellmHeaderFor(key))
	keyed.Spec.Containers[0].Env = append(keyed.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "BOAT_ACCOUNT", Value: "gardenio3"},
	)
	pm := &mockPodManager{pods: []corev1.Pod{keyed}}
	r := newRestarterForTest(pm)

	deleted := r.ReconcileKeylessGainedKey(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio3": {Status: "active", LiteLLMKey: key},
	})
	if len(deleted) != 0 {
		t.Fatalf("keyed pod must be skipped, got %v", deleted)
	}
}

// TestReconcileKeylessGainedKey_SkipsNoAccountTerminalAndTerminating verifies
// that a keyless pod without BOAT_ACCOUNT (birth account unknown), a
// Failed/Succeeded pod, and an already-Terminating pod are all left alone.
func TestReconcileKeylessGainedKey_SkipsNoAccountTerminalAndTerminating(t *testing.T) {
	noAccount := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-noaccount"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "agent"}}},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	succeeded := keylessPodOnAccount("agent-succeeded", "gardenio3")
	succeeded.Status.Phase = corev1.PodSucceeded
	terminating := keylessPodOnAccount("agent-terminating", "gardenio3")
	now := metav1.Now()
	terminating.DeletionTimestamp = &now

	pm := &mockPodManager{pods: []corev1.Pod{noAccount, succeeded, terminating}}
	r := newRestarterForTest(pm)

	deleted := r.ReconcileKeylessGainedKey(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio3": {Status: "active", LiteLLMKey: "sk-litellm-NEW"},
	})
	if len(deleted) != 0 {
		t.Fatalf("no-account, terminal, and terminating pods must be skipped, got %v", deleted)
	}
}

// TestReconcileKeylessGainedKey_EmptySnapshotIsNoOp verifies the fleet-safety
// guard: an account cache with no keyed accounts (a wiped/failed refresh) never
// restarts a keyless pod, even if the pod's account name is present-but-blank.
func TestReconcileKeylessGainedKey_EmptySnapshotIsNoOp(t *testing.T) {
	pm := &mockPodManager{pods: []corev1.Pod{
		keylessPodOnAccount("agent-keyless", "gardenio3"),
	}}
	r := newRestarterForTest(pm)

	if d := r.ReconcileKeylessGainedKey(context.Background(), map[string]config.AccountCacheEntry{}); len(d) != 0 {
		t.Fatalf("empty snapshot must be a no-op, got %v", d)
	}
	if d := r.ReconcileKeylessGainedKey(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio3": {Status: "active"}, // present but keyless
	}); len(d) != 0 {
		t.Fatalf("keyless-only snapshot must be a no-op, got %v", d)
	}
	if len(pm.deleted) != 0 {
		t.Fatalf("no pods should be deleted, got %v", pm.deleted)
	}
}

// TestReconcileKeylessGainedKey_SkipsInProcessSwitchedPod verifies a keyless pod
// whose session already switched to a DIFFERENT account in-process (coopmux live
// account != BOAT_ACCOUNT) is left alone — its live credentials came from the
// broker and the baked keyless env is inert — while a pod still on its birth
// account is restarted (ka-0000005463 guard).
func TestReconcileKeylessGainedKey_SkipsInProcessSwitchedPod(t *testing.T) {
	pm := &mockPodManager{pods: []corev1.Pod{
		keylessPodOnAccount("agent-switched", "gardenio3"),
		keylessPodOnAccount("agent-stuck", "gardenio3"),
	}}
	r := newRestarterWithSessions(pm, &mockSessionAccounts{accounts: map[string]string{
		"agent-switched": "shannon",   // moved off gardenio3 in-process
		"agent-stuck":    "gardenio3", // still on birth account, keyless
	}})

	deleted := r.ReconcileKeylessGainedKey(context.Background(), map[string]config.AccountCacheEntry{
		"gardenio3": {Status: "active", LiteLLMKey: "sk-litellm-NEW"},
	})
	if len(deleted) != 1 || deleted[0] != "agent-stuck" {
		t.Fatalf("expected only agent-stuck restarted, got %v", deleted)
	}
}

// TestReconcileKeylessGainedKey_Idempotent verifies the sweep is self-idempotent:
// once the account is keyed and the keyless pod is recreated with a baked key, a
// subsequent sweep (pod now carries the key) is a no-op — no churn.
func TestReconcileKeylessGainedKey_Idempotent(t *testing.T) {
	accounts := map[string]config.AccountCacheEntry{
		"gardenio3": {Status: "active", LiteLLMKey: "sk-litellm-NEW"},
	}
	// First sweep: keyless pod restarted.
	pm := &mockPodManager{pods: []corev1.Pod{
		keylessPodOnAccount("agent-keyless", "gardenio3"),
	}}
	r := newRestarterForTest(pm)
	if d := r.ReconcileKeylessGainedKey(context.Background(), accounts); len(d) != 1 {
		t.Fatalf("first sweep should restart the keyless pod, got %v", d)
	}

	// Recreate: the reconciler respawns it with the account's key baked in.
	rebaked := agentPodWithHeader("agent-keyless", litellmHeaderFor("sk-litellm-NEW"))
	rebaked.Spec.Containers[0].Env = append(rebaked.Spec.Containers[0].Env,
		corev1.EnvVar{Name: "BOAT_ACCOUNT", Value: "gardenio3"},
	)
	pm2 := &mockPodManager{pods: []corev1.Pod{rebaked}}
	r2 := newRestarterForTest(pm2)
	if d := r2.ReconcileKeylessGainedKey(context.Background(), accounts); len(d) != 0 {
		t.Fatalf("re-baked pod carries the key — second sweep must be a no-op, got %v", d)
	}
}
