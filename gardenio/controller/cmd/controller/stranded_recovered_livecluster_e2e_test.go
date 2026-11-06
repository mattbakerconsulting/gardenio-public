//go:build e2e

// stranded_recovered_livecluster_e2e_test.go — LIVE-CLUSTER e2e for the
// level-triggered ReconcileStrandedRecovered sweep (bead gd-DMvno7uS3J, split
// from ka-0000001464; parent E2E coverage-gap epic ka-0000000958).
//
// ── The gap this closes ───────────────────────────────────────────────────────
//   ReconcileStrandedRecovered (cmd/controller/account_rotation_restart.go) is the
//   level-triggered counterpart to the edge-triggered accountRotationRestarter.
//   Reconcile. It recycles a running pod baked onto a STILL-exhausted account once
//   that pod's PROJECT POOL regains a healthy alternative — the whole-pool
//   exhaustion case Reconcile can never catch (the account was already exhausted
//   at spawn, so there was never an active→exhausted edge). Its decision line is
//   "restarting agent pod stranded on exhausted account; project pool has
//   recovered".
//
//   That sweep had ZERO e2e in EITHER suite: a HEAD grep for "stranded on
//   exhausted" / "project pool has recovered" returns 0 hits across
//   gardenio/tests/e2e/scripts + tests/beads-viewer-e2e. Its two sibling sweeps
//   (stale-vkey + keyless-gained-key) were backfilled in
//   test-credential-rotation.sh Scenario 5 (ka-0000001464), which explicitly
//   sanctioned splitting StrandedRecovered to this follow-up "if it needs a whole
//   project-pool-recovery scenario" — it does (see next paragraph).
//
//   Unit coverage exists (account_rotation_recovery_test.go), but every case
//   drives a FAKE mockPodManager: nothing exercised the REAL K8s list→decide→
//   delete chain against a live apiserver, which is the exact seam the sibling
//   exhaustion leg proved matters.
//
// ── Why this leg is a heavier fixture than its two siblings ───────────────────
//   The stale-vkey + keyless legs decide on the pod's OWN baked env vs a single
//   account key (one husk + one fixture account each). StrandedRecovered instead
//   calls the REAL selectAccount(cfg, project, spec, liveCounts) over the
//   whole AccountCache under ProjectCacheMu, so a NON-VACUOUS positive leg needs a
//   genuine project pool:
//     - a husk baked onto account X where X is status=exhausted AND
//       throttle_status=rejected AND TERMINAL (probe_auth_status=rejected — the
//       transient-window skip guard at account_rotation_restart.go spares a
//       non-terminal X, ka-0000002085), and
//     - a DIFFERENT healthy account Y labeled project:<same> whose litellm_key
//       differs, so selectAccount returns Y (not "" and not X) — else the
//       sweep is a correct no-op and the leg is vacuous.
//   The negative control removes Y so the pool is still fully exhausted:
//   selectAccount returns "" (or re-picks X), and the sweep MUST NOT delete.
//
// ── Why a from-source LIVE-CLUSTER go:build e2e test, not a shell log-poll ────
//   The sweep runs every 5 MINUTES (strandedRecoveryInterval), and after a coopmux
//   blip it can sit out up to strandedRecoveryBlindSweeps additional ticks — so a
//   shell test that plants a husk in the e2e namespace and polls the DEPLOYED
//   controller's decision log would need a ~15-minute budget and would still be at
//   the mercy of whether the deployed image even carries the sweep (it landed
//   2026-06-29 in db432b759; a lagging staging controller may predate a later
//   guard). Instead — exactly as the bead's landing note directs ("Prove via the
//   same in-cluster harness method (real K8sManager, isolated scratch ns) since
//   the deployed staging controller may lag") — this test constructs the REAL
//   podmanager.K8sManager against the live apiserver, plants husk pods in an
//   ISOLATED SCRATCH NAMESPACE, and calls ReconcileStrandedRecovered directly. It
//   is deterministic (one call, no 5-min wait), version-independent (drives the
//   SOURCE the release ships, not whatever image staging runs), and fully isolated
//   (the deployed controller only ever sweeps its OWN namespace, never the scratch
//   ns — which also means the deployed stale-vkey sweep can't confound our husks).
//
//   The account "cache" the sweep reads is cfg.AccountCache, an in-memory map — so
//   no beads daemon, coopmux, or LiteLLM is required. We wire the restarter with
//   sessions=nil (skips the blind-sweep defer entirely — the `a.sessions != nil`
//   guard is false, so the sweep never sits out a tick), drain=nil (every
//   delete is immediate — no coop drain to gate on), and beads=nil (no restart-
//   reason stamp). The ONLY live dependency is the K8s API on the scratch ns.
//
// ── What this proves (each leg on its own uniquely-named husk) ────────────────
//   Positive: a real Pending husk baked onto the exhausted+rejected+terminal
//             account, in a project whose pool has a healthy keyed sibling, is
//             DELETED from the apiserver by the real sweep — and the sweep's
//             returned pod list names exactly that husk.
//   Negative: with the healthy sibling REMOVED (pool still fully exhausted), the
//             identically-baked husk is LEFT in place — the sweep returns empty
//             and the pod still exists. Without this control a sweep that deleted
//             every pod would pass the positive leg vacuously.
//   Foreign : a co-resident husk baked onto a healthy account's key (not
//             stranded) is never deleted, in the SAME call that deletes the
//             stranded one — proving the sweep is targeted, not a blanket kill.
//
// ── Anti-vacuity (mutation-verified while authoring) ──────────────────────────
//   - Gut ReconcileStrandedRecovered to `return nil` → the positive/foreign legs
//     red (the stranded husk survives, so the delete + returned-name assertions
//     fail).
//   - Drop the "candidate == acctName || candKey == key" no-op guard → the
//     NEGATIVE control leg reds (the sweep would delete a husk with no healthy
//     alternative).
//
// ── Skip-clean contract ──────────────────────────────────────────────────────
//   SKIPs CLEANLY (t.Skip, never a fail) when no cluster is reachable — a missing
//   substrate must never be read as a pass. When a cluster IS reachable every leg
//   is a HARD assertion. The scratch namespace is always torn down.
//
// Run: (cd gardenio/controller && go test -tags e2e ./cmd/controller/ \
//         -run TestReconcileStrandedRecoveredE2E -count=1 -timeout 180s)
// Wrapped into the e2e shard by
// gardenio/tests/e2e/scripts/test-stranded-recovered-sweep.sh.

package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"gardenio/controller/internal/config"
	"gardenio/controller/internal/podmanager"
)

// strandedE2EProject is the project the husk pods carry in BOAT_PROJECT and the
// fixture accounts are labeled for. It is deliberately fictitious so the
// in-memory AccountCache we build never overlaps a real project's pool.
const strandedE2EProject = "gardenio-stranded-e2e"

// strandedE2ENeverSchedules is a nodeSelector no node satisfies, so every husk
// sits Pending — no node, no image pull, no model spend. The sweep decides on
// pod SPEC (env), readable while Pending, so it fires regardless of scheduling.
var strandedE2ENeverSchedules = map[string]string{"gardenio-stranded-e2e/never-schedules": "true"}

// buildLiveClusterClient builds a clientset the same way the controller's
// buildK8sConfig / ftrestart-probe do: in-cluster config first (the agent-pod /
// live-controller case), then the default kubeconfig loading rules (KUBECONFIG
// env + ~/.kube/config — the CI-runner case, where the job has written
// ~/.kube/config). Returns (nil, reason) when no
// cluster is reachable so the caller can t.Skip rather than fail.
func buildLiveClusterClient() (kubernetes.Interface, string) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			clientcmd.NewDefaultClientConfigLoadingRules(),
			&clientcmd.ConfigOverrides{},
		).ClientConfig()
		if err != nil {
			return nil, "no in-cluster config and no usable kubeconfig: " + err.Error()
		}
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, "building clientset: " + err.Error()
	}
	return client, ""
}

// strandedE2EHuskPod returns a Pending husk pod spec carrying the exact env the
// sweep container-walks: the baked LiteLLM key in ANTHROPIC_CUSTOM_HEADERS (via
// litellmHeaderFor, the same helper applyAccountConfig uses) and BOAT_PROJECT so
// podProjectRole resolves the pool. It carries the ListAgentPods selector labels
// (app.kubernetes.io/name=gardenio + gardenio.io/app=gardenio) but DELIBERATELY
// OMITS gardenio.io/agent, so even if this ever ran in a controller-managed
// namespace the reconciler's orphan reaper (which only touches
// gardenio.io/agent-labeled pods) would leave it alone. In a scratch ns there is
// no controller at all, so it is purely a safety belt.
func strandedE2EHuskPod(name, litellmKey string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				podmanager.LabelApp:         podmanager.LabelAppValue,
				podmanager.LabelGardenioApp: podmanager.LabelAppValue,
				"gardenio.io/e2e-husk":      "stranded-recovered",
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			// Unschedulable — carries the spec the sweep reads, never runs.
			NodeSelector: strandedE2ENeverSchedules,
			Containers: []corev1.Container{{
				Name:    "agent",
				Image:   "public.ecr.aws/docker/library/busybox:1.36",
				Command: []string{"sh", "-c", "sleep 3600"},
				Env: []corev1.EnvVar{
					{Name: "BOAT_PROJECT", Value: strandedE2EProject},
					{Name: "ANTHROPIC_CUSTOM_HEADERS", Value: litellmHeaderFor(litellmKey)},
				},
			}},
		},
	}
}

// strandedE2EAccount returns an AccountCacheEntry for a genuinely-dead account:
// budget-exhausted lifecycle AND a live rejected throttle signal AND a TERMINAL
// verdict (rejected auth probe) — the only shape ReconcileStrandedRecovered acts
// on. Mirrors account_rotation_recovery_test.go's strandedAccount helper.
func strandedE2EAccount(litellmKey string, labels ...string) config.AccountCacheEntry {
	e := acct(litellmKey, "exhausted", "rejected", labels...)
	e.ProbeAuthStatus = "rejected"
	return e
}

// strandedE2EFixture creates a scratch namespace, plants the given husk pods, and
// returns a live K8sManager plus a teardown that deletes the namespace. It skips
// the whole test cleanly when no cluster is reachable.
func strandedE2EFixture(t *testing.T, ns string, husks ...*corev1.Pod) (*podmanager.K8sManager, string, func()) {
	t.Helper()
	client, skipReason := buildLiveClusterClient()
	if client == nil {
		t.Skipf("SKIP stranded-recovered live-cluster e2e (no cluster reachable): %s", skipReason)
	}
	ctx := context.Background()

	// Fresh scratch namespace. privileged PSA so the busybox husk is admissible
	// on a restricted-default cluster.
	_, err := client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
			Labels: map[string]string{
				"pod-security.kubernetes.io/enforce": "privileged",
				"gardenio.io/e2e-scratch":            "stranded-recovered",
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && !apierrors.IsAlreadyExists(err) {
		// A cluster that refuses namespace creation is a genuine substrate gap,
		// not a product regression — skip clean rather than red the shard.
		t.Skipf("SKIP stranded-recovered live-cluster e2e (cannot create scratch namespace %s): %v", ns, err)
	}

	teardown := func() {
		_ = client.CoreV1().Namespaces().Delete(context.Background(), ns, metav1.DeleteOptions{})
	}

	for _, h := range husks {
		h.Namespace = ns
		if _, err := client.CoreV1().Pods(ns).Create(ctx, h, metav1.CreateOptions{}); err != nil {
			teardown()
			t.Fatalf("planting husk pod %s/%s: %v", ns, h.Name, err)
		}
	}

	return podmanager.New(client, silentLogger()), ns, teardown
}

// waitHuskGone polls until the husk pod is absent (the sweep's delete has
// propagated) or the deadline elapses. Returns true if gone.
func waitHuskGone(ctx context.Context, pm *podmanager.K8sManager, ns, name string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		_, err := pm.GetAgentPod(ctx, name, ns)
		if apierrors.IsNotFound(err) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// huskPresent reports whether the husk pod still exists (used by the negative
// control, which asserts the sweep left it alone).
func huskPresent(ctx context.Context, pm *podmanager.K8sManager, ns, name string) bool {
	_, err := pm.GetAgentPod(ctx, name, ns)
	return err == nil
}

// TestReconcileStrandedRecoveredE2E_RestartsWhenSiblingHealthy is the POSITIVE +
// FOREIGN leg: against a REAL apiserver, a husk baked onto the exhausted+rejected
// terminal account — in a project whose pool has a healthy keyed sibling — is
// deleted by the real sweep, while a co-resident husk on the healthy account's
// own key is left in place. This is the whole-pool-exhaustion recovery the
// edge-triggered Reconcile cannot catch.
func TestReconcileStrandedRecoveredE2E_RestartsWhenSiblingHealthy(t *testing.T) {
	// One stamp per run: a still-Terminating ns from a prior run can't collide,
	// and concurrent shard binaries land in separate scratch namespaces.
	stamp := strandedE2EStamp()
	ns := "gardenio-e2e-stranded-pos-" + stamp
	strandedKey := "sk-litellm-stranded-" + stamp
	healthyKey := "sk-litellm-healthy-" + stamp

	strandedHusk := "cred-stranded-pos-" + stamp
	foreignHusk := "cred-stranded-foreign-" + stamp

	pm, _, teardown := strandedE2EFixture(t, ns,
		strandedE2EHuskPod(strandedHusk, strandedKey),
		// A pod already on the HEALTHY account — must never be swept.
		strandedE2EHuskPod(foreignHusk, healthyKey),
	)
	defer teardown()

	// In-memory account cache: X stranded (terminal), Y healthy — both labeled
	// for the same project, so selectAccount(project) returns Y for the
	// stranded pod. Account NAMES sort so the healthy account is a real,
	// distinct candidate (its key differs from the stranded key).
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"stranded-x": strandedE2EAccount(strandedKey, "project:"+strandedE2EProject),
			"healthy-y":  acct(healthyKey, "active", "", "project:"+strandedE2EProject),
		},
	}

	// Real restarter over the scratch ns. sessions=nil skips the blind-sweep
	// defer (the `a.sessions != nil` guard is false); drain/beads left nil so
	// deletes are immediate and unstamped.
	r := newAccountRotationRestarter(pm, ns, nil, silentLogger())

	deleted := r.ReconcileStrandedRecovered(context.Background(), cfg)

	// The sweep must have named EXACTLY the stranded husk.
	if len(deleted) != 1 || deleted[0] != strandedHusk {
		t.Fatalf("expected sweep to restart exactly %q, got %v", strandedHusk, deleted)
	}

	// …and the delete must have propagated on the real apiserver.
	ctx := context.Background()
	if !waitHuskGone(ctx, pm, ns, strandedHusk, 30*time.Second) {
		t.Fatalf("stranded husk %q still present after sweep — delete did not propagate", strandedHusk)
	}

	// The foreign (healthy-account) husk must be untouched.
	if !huskPresent(ctx, pm, ns, foreignHusk) {
		t.Fatalf("foreign husk %q on a healthy account was deleted — sweep is over-broad", foreignHusk)
	}
}

// TestReconcileStrandedRecoveredE2E_NoOpWhenPoolStillExhausted is the NEGATIVE
// control: with NO healthy sibling in the pool (selectAccount returns "" or
// re-picks the stranded account), the identically-baked husk is LEFT in place.
// Without this leg, a sweep that deleted every matched pod would pass the
// positive leg vacuously.
func TestReconcileStrandedRecoveredE2E_NoOpWhenPoolStillExhausted(t *testing.T) {
	stamp := strandedE2EStamp()
	ns := "gardenio-e2e-stranded-neg-" + stamp
	strandedKey := "sk-litellm-stranded-" + stamp
	strandedHusk := "cred-stranded-neg-" + stamp

	pm, _, teardown := strandedE2EFixture(t, ns,
		strandedE2EHuskPod(strandedHusk, strandedKey),
	)
	defer teardown()

	// Only the stranded account in the pool — no healthy alternative.
	cfg := &config.Config{
		AccountCache: map[string]config.AccountCacheEntry{
			"stranded-x": strandedE2EAccount(strandedKey, "project:"+strandedE2EProject),
		},
	}

	r := newAccountRotationRestarter(pm, ns, nil, silentLogger())

	deleted := r.ReconcileStrandedRecovered(context.Background(), cfg)
	if len(deleted) != 0 {
		t.Fatalf("pool still fully exhausted — sweep must be a no-op, got %v", deleted)
	}

	// Give any errant async delete a beat to surface, then assert the husk lives.
	ctx := context.Background()
	time.Sleep(2 * time.Second)
	if !huskPresent(ctx, pm, ns, strandedHusk) {
		t.Fatalf("stranded husk %q was deleted with no healthy alternative — sweep never strands a worse situation", strandedHusk)
	}
}

// strandedE2EStamp returns a short per-process suffix so husk pod / account names
// are unique across concurrent test binaries. Uses the wall clock (fine for a
// live-cluster e2e; these tests are not the pure-function unit suite).
func strandedE2EStamp() string {
	return fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
}
