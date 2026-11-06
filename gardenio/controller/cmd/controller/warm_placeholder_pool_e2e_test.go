package main

// End-to-end coverage for the controller-minted warm-B placeholder pool feeding
// the ClaimWarmPlaceholderForThaw graft-on-thaw wake path (task gd-cP6p4hxPqx,
// epic ka-0000000958 — E2E coverage-gap sprint).
//
// WHY THIS EXISTS (the gap): the warm-B pool has strong UNIT coverage, but split
// across two seams that never meet:
//
//   - poolmanager/*_test.go proves Reconcile MINTS topology-bucketed placeholders
//     from snapshot templates, and proves ClaimWarmPlaceholderForThaw promotes a
//     topology-MATCHING placeholder (and misses on a mismatch) — but it hand-seeds
//     the placeholder and calls the pool method directly.
//   - cmd/controller/freezethaw_lifecycle_test.go drives the ft.thaw() wake seam,
//     but PRE-SEEDS a readyWarmPlaceholder pod and only exercises the FAILURE
//     graft (failingNodeExec) and the miss/no-pool fallbacks.
//
// No test wires the PRODUCTION seam end-to-end: enable the pool exactly as
// main.go does (WARM_PLACEHOLDER_POOL_ENABLED via config → NewPlaceholderPool →
// SetSnapshotProvider(ft.snapshotProvider())), let the CONTROLLER POOL mint the
// placeholder from a real snapshot template via Reconcile, then freeze→thaw an
// agent and assert ClaimWarmPlaceholderForThaw grafts onto the co-resident,
// topology-MATCHING minted placeholder with an atomic claim+promote (canonical
// name stamp), and FALLS BACK to create-then-restore on a topology miss.
//
// This is the standing regression guard the resume-latency e2e (test-resume-
// latency.sh) does NOT provide: that harness uses MANUAL make_placeholder
// kubectl-apply pods and never sets WARM_PLACEHOLDER_POOL_ENABLED, so the
// controller-minted pool + graft-on-thaw path had zero coverage of its own.

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"gardenio/controller/internal/config"
	gardeniometrics "gardenio/controller/internal/metrics"
	"gardenio/controller/internal/podmanager"
	"gardenio/controller/internal/poolmanager"
)

// warmPoolThawOKStdout is a node-script stdout that passes every thaw post-state
// assertion (both restores rc=0, the restored sentry shares Pod B's netns
// inode). It mirrors podmanager's package-private thawOKStdout fixture; we
// redeclare it here because this test lives in package main and drives the graft
// through a self-supplied NodeExecutor.
const warmPoolThawOKStdout = `    barrier restore
FT_B_SANDBOX_CID=bsandbox888
FT_B_AGENT_CID=bagent999
FT_B_POD_UID=b-uid-456
FT_B_NETNS_PATH=/var/run/netns/cni-bbbbbbbb-2222-3333-4444-555555555555
FT_B_NETNS=net:[4026532540]
FT_NETNS_PINNED=/var/run/netns/gb-thaw-bsandbox888
FT_CNI_CONFIRMED=yes
FT_CNI_POD_IP=10.30.136.3/32
FT_SUB_RC=0
FT_ROOT_RC=0
FT_RESTORED_PID=37737
FT_RESTORED_NETNS=net:[4026532540]
FT_NETNS_L3_REASSERTED=yes
FT_NETNS_SELF_IPAM=yes
FT_NETNS_PRERESTORE_ROUTABLE=yes
FT_NETNS_IFACES=1
FT_NETNS_DEFROUTE=yes
FT_SENTRY_DEFROUTE=yes
FT_SENTRY_NONLO=1
FT_SENTRY_EGRESS=ok
FT_CONTAINERS_RUNNING=yes
`

// scriptedNodeExec returns a fixed stdout/exit for every node exec and records
// how many times it was invoked. The warm-graft success path runs exactly one
// node exec (the barrier restore); the memory gate is disabled on the manager so
// no extra exec is interposed.
type scriptedNodeExec struct {
	stdout string
	exit   int
	calls  int
}

func (s *scriptedNodeExec) Exec(context.Context, string, string) (podmanager.NodeExecResult, error) {
	s.calls++
	return podmanager.NodeExecResult{Stdout: s.stdout, ExitCode: s.exit}, nil
}

// e2eClaimRecorder captures RecordWarmPlaceholderClaim outcomes so the test can
// assert the hit/miss the metric records — the same signal
// warm_placeholder_claims_total exposes in prod.
type e2eClaimRecorder struct {
	outcomes []string // "outcome/node/topology"
}

func (r *e2eClaimRecorder) RecordWarmPlaceholderClaim(outcome, node, topology string) bool {
	r.outcomes = append(r.outcomes, outcome+"/"+node+"/"+topology)
	return true
}

// gvisorProbePod builds a Ready gVisor probe pod on node so the pool's node
// discovery finds a warmable node (poolmanager keys buckets by (node,topology)
// and only fills nodes that carry a Ready probe).
func gvisorProbePod(name, ns, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels: map[string]string{
				poolmanager.DefaultProbeSelectorKey: poolmanager.DefaultProbeSelectorValue,
			},
		},
		Spec: corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
}

// warmSnapshotTemplate builds a distinguishable snapshot PodTemplate whose
// topology key is stable and non-empty (a named agent container with a specific
// workspace mount + an init-clone), so Reconcile mints a per-topology bucket
// (not the legacy blind bare-/work fallback) and the wake path's topology lookup
// resolves to the same key.
func warmSnapshotTemplate(mountPath string) corev1.PodSpec {
	return corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:         podmanager.ContainerName,
			Image:        "agent:latest",
			VolumeMounts: []corev1.VolumeMount{{Name: "workspace", MountPath: mountPath}},
		}},
		InitContainers: []corev1.Container{{Name: "init-clone", Image: "alpine/git"}},
	}
}

// newWarmPoolE2E wires the freeze/thaw lifecycle + warm-B pool EXACTLY as main.go
// does when WARM_PLACEHOLDER_POOL_ENABLED is set: it builds the pool from the
// same config fields, points its SnapshotProvider at the manager, and shares one
// fake client + namespace. A snapshot for agentName on node is seeded into the
// manager's store so both WarmBuckets (the pool fill source) and SnapshotTopology
// (the wake-path lookup) resolve from the same record — the production topology
// contract.
//
// Returns the lifecycle, the pool, and the claim recorder the pool writes
// hit/miss outcomes to. The caller supplies (and directly observes) the node
// executor.
func newWarmPoolE2E(t *testing.T, client *fake.Clientset, ns, agentName, node string, tmpl corev1.PodSpec, exec podmanager.NodeExecutor) (*freezeThawLifecycle, *poolmanager.PlaceholderPool, *e2eClaimRecorder) {
	t.Helper()

	// Seed the durable snapshot record as if Freeze had already run on `node`.
	store := podmanager.NewInMemorySnapshotStore()
	if err := store.Put(context.Background(), podmanager.Snapshot{
		AgentName: agentName, Namespace: ns, PodName: agentName, Node: node,
		ImagePath: "/var/tmp/ckpt/" + agentName, BundlePath: "/var/tmp/bundle/" + agentName,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a-uid", ContainerCount: 2,
		PodTemplate: tmpl,
	}); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	// Build the manager by hand (nil rehydrator → the graft skips the rehydration
	// hand-off, so a scripted-OK node exec yields a fully successful graft) and
	// tighten timers so the create-path fallback fails fast instead of waiting the
	// multi-minute production Ready budget.
	mgr := podmanager.NewFreezeThawManager(client, exec, store, nil, discardLogger())
	mgr.PodReadyTimeout = 250 * time.Millisecond
	mgr.CreateRestorePodReadyTimeout = 250 * time.Millisecond
	mgr.PodReadyPollInterval = time.Millisecond
	// Disable the pre-thaw memory gate so the graft's node-exec count reflects the
	// barrier restore only (ka-0000000088 adds an exec otherwise).
	mgr.MemGateDisabled = true
	ft := &freezeThawLifecycle{mgr: mgr, logger: discardLogger()}

	// Build the pool from the SAME config the controller reads, so the test
	// genuinely exercises the WARM_PLACEHOLDER_POOL_ENABLED wiring path: main.go
	// only constructs the pool when cfg.WarmPlaceholderPoolEnabled, sizing it from
	// cfg.WarmPlaceholderPoolMinSize.
	cfg := &config.Config{
		WarmPlaceholderPoolEnabled: true,
		WarmPlaceholderPoolMinSize: 1,
	}
	if !cfg.WarmPlaceholderPoolEnabled || cfg.WarmPlaceholderPoolMinSize < 1 {
		t.Fatal("test misconfigured: pool must be enabled with a positive MinSize")
	}
	rec := &e2eClaimRecorder{}
	pool := poolmanager.NewPlaceholderPool(client, poolmanager.PlaceholderPoolConfig{
		Namespace:     ns,
		Image:         "agent:latest",
		MinSize:       cfg.WarmPlaceholderPoolMinSize,
		ClaimRecorder: rec,
	}, discardLogger())
	if pool == nil {
		t.Fatal("NewPlaceholderPool returned nil for an enabled config")
	}
	// Topology/repo-aware fills, driven from the manager's snapshots — the
	// production wiring (main.go: pool.SetSnapshotProvider(ft.snapshotProvider())).
	pool.SetSnapshotProvider(ft.snapshotProvider())
	ft.setPlaceholderPool(pool)

	return ft, pool, rec
}

// readyOnCreateReactor makes every pod the pool CREATES reach Running+Ready
// immediately (kubelet admission is what the warm pool front-loads off the hot
// path; here we simulate it so the minted placeholder is a valid graft target).
func readyOnCreateReactor(client *fake.Clientset) {
	client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		ca, ok := action.(ktesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		pod, ok := ca.GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = append(pod.Status.Conditions, corev1.PodCondition{
			Type: corev1.PodReady, Status: corev1.ConditionTrue,
		})
		if pod.UID == "" {
			pod.UID = types.UID("uid-" + pod.Name)
		}
		return false, nil, nil // let the tracker store the mutated pod
	})
}

// mintedWarmPlaceholders lists the Ready warm placeholders the pool minted.
func mintedWarmPlaceholders(t *testing.T, client *fake.Clientset, ns string) []corev1.Pod {
	t.Helper()
	pods, err := client.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{
		LabelSelector: podmanager.LabelWarmPlaceholder,
	})
	if err != nil {
		t.Fatalf("listing minted placeholders: %v", err)
	}
	return pods.Items
}

// TestWarmPool_E2E_TopologyMatchGraftsOntoMintedPlaceholder is the headline
// end-to-end: the CONTROLLER POOL mints a topology-bucketed placeholder from the
// snapshot template (Reconcile), then a thaw for a topology-MATCHING co-resident
// agent claims+promotes THAT minted pod atomically and grafts the checkpoint onto
// it — no create-path Pod B, a hit recorded, and the promoted pod carries the
// live-agent identity (agent label, warm-placeholder label dropped, canonical +
// bead-id + do-not-disrupt annotations stamped).
func TestWarmPool_E2E_TopologyMatchGraftsOntoMintedPlaceholder(t *testing.T) {
	ns, agent, node := "gvisor", "agentsim", "node-a"
	client := fake.NewClientset(gvisorProbePod("probe-a", ns, node))
	readyOnCreateReactor(client)

	tmpl := warmSnapshotTemplate("/home/agent/bot")
	exec := &scriptedNodeExec{stdout: warmPoolThawOKStdout, exit: 0}
	ft, pool, rec := newWarmPoolE2E(t, client, ns, agent, node, tmpl, exec)

	// --- Mint: the controller pool builds a topology placeholder from the snap ---
	if err := pool.Reconcile(context.Background()); err != nil {
		t.Fatalf("pool Reconcile (mint): %v", err)
	}
	minted := mintedWarmPlaceholders(t, client, ns)
	if len(minted) != 1 {
		t.Fatalf("expected exactly 1 minted warm placeholder, got %d", len(minted))
	}
	mintedName := minted[0].Name
	wantTopo := podmanager.PodTemplateTopologyKey(tmpl)
	if got := minted[0].Labels[podmanager.LabelWarmTopology]; got != wantTopo {
		t.Fatalf("minted placeholder topology = %q, want %q (built from snapshot template, not legacy)", got, wantTopo)
	}
	if minted[0].Spec.NodeName != node {
		t.Fatalf("minted placeholder node = %q, want %q (node-local to the snapshot)", minted[0].Spec.NodeName, node)
	}
	// It must present the snapshot's workspace mount, not a bare /work — the whole
	// point of a topology-aware (graft-compatible) placeholder.
	var hasWorkspaceMount bool
	for _, c := range minted[0].Spec.Containers {
		for _, m := range c.VolumeMounts {
			if m.MountPath == "/home/agent/bot" {
				hasWorkspaceMount = true
			}
		}
	}
	if !hasWorkspaceMount {
		t.Errorf("minted placeholder did not inherit the snapshot workspace mount (topology fill broken)")
	}

	// --- Thaw: the wake path grafts onto the minted, co-resident placeholder ---
	spec := podmanager.AgentPodSpec{
		AgentName: agent, Project: "gardenio", Mode: "crew", Role: "crew",
		RuntimeClassName: podmanager.RuntimeClassRunsc,
	}
	res, resumePath, err := ft.thaw(context.Background(), spec, "gd-bead")
	if err != nil {
		t.Fatalf("warm graft thaw must succeed with an OK node exec, got err=%v", err)
	}
	if resumePath != gardeniometrics.ResumePathThawWarm {
		t.Errorf("topology-match graft must report the thaw-warm path, got %q", resumePath)
	}
	if res == nil || res.PlaceholderPod != mintedName {
		t.Fatalf("thaw must graft onto the minted placeholder %q, got result=%+v", mintedName, res)
	}
	// The warm graft must NOT mint a create-path Pod B ("<snap.PodName>-thaw-b"):
	// grafting onto the pre-Ready placeholder is the whole latency win. A create
	// fallback would have created that pod and run a second, separate restore.
	if _, gerr := client.CoreV1().Pods(ns).Get(context.Background(), agent+"-thaw-b", metav1.GetOptions{}); gerr == nil {
		t.Errorf("warm graft must NOT create a create-path Pod B %q — it grafts onto the minted placeholder", agent+"-thaw-b")
	}
	// At least the barrier restore ran on the node (a successful thaw also runs
	// the post-restore image reap, so calls is >=1, not exactly 1).
	if exec.calls < 1 {
		t.Errorf("warm graft must run the barrier restore on the node, got %d node execs", exec.calls)
	}

	// --- The minted pod was promoted IN PLACE to the live agent pod ---
	promoted, gerr := client.CoreV1().Pods(ns).Get(context.Background(), mintedName, metav1.GetOptions{})
	if gerr != nil {
		t.Fatalf("promoted pod %q must still exist as the live agent pod: %v", mintedName, gerr)
	}
	if _, stillWarm := promoted.Labels[podmanager.LabelWarmPlaceholder]; stillWarm {
		t.Errorf("promoted pod must DROP the warm-placeholder label (else the pool reaper deletes it mid-thaw), still present: %q",
			promoted.Labels[podmanager.LabelWarmPlaceholder])
	}
	if promoted.Labels[podmanager.LabelAgent] != agent {
		t.Errorf("promoted pod agent label = %q, want %q", promoted.Labels[podmanager.LabelAgent], agent)
	}
	if promoted.Labels[podmanager.LabelThawPlaceholder] != "true" {
		t.Errorf("promoted pod must carry thaw-placeholder=true so the reconciler exempts it from drift recreation")
	}
	// Canonical-name stamp: the pool pod keeps its warm-b-* name, so the canonical
	// agent pod name is recorded in the annotation for the reconciler remap.
	if got, want := promoted.Annotations[podmanager.AnnotationCanonicalPod], spec.PodName(); got != want {
		t.Errorf("promoted pod canonical-pod annotation = %q, want %q (the reconciler keys desired state by this)", got, want)
	}
	if promoted.Annotations[podmanager.AnnotationBeadID] != "gd-bead" {
		t.Errorf("promoted pod bead-id annotation = %q, want gd-bead", promoted.Annotations[podmanager.AnnotationBeadID])
	}
	if promoted.Annotations[podmanager.AnnotationKarpenterDoNotDisrupt] != "true" {
		t.Errorf("promoted pod must be Karpenter do-not-disrupt so consolidation cannot evict the live agent")
	}

	// --- The claim metric recorded a HIT for this (node, topology) ---
	wantHit := "hit/" + node + "/" + wantTopo
	if len(rec.outcomes) != 1 || rec.outcomes[0] != wantHit {
		t.Errorf("claim recorder outcomes = %v, want exactly [%q]", rec.outcomes, wantHit)
	}
}

// TestWarmPool_E2E_TopologyMissFallsBackToCreatePath is the mismatch control: the
// pool minted a placeholder for topology T1, but the thawing agent's snapshot has
// a DIFFERENT mount shape (T2). The wake path must NOT graft onto the incompatible
// placeholder (runsc mount validation would reject the checkpoint); it records a
// MISS, leaves the minted placeholder Ready+unclaimed, and falls back to
// create-then-restore (thaw-fresh) — minting its own -thaw-b Pod B rather than
// grafting onto the co-resident-but-incompatible warm placeholder.
func TestWarmPool_E2E_TopologyMissFallsBackToCreatePath(t *testing.T) {
	ns, agent, node := "gvisor", "agentsim", "node-a"
	client := fake.NewClientset(gvisorProbePod("probe-a", ns, node))
	readyOnCreateReactor(client)
	// Watch for the create-path Pod B ("<snap.PodName>-thaw-b"): its creation is
	// the proof the wake path fell back to create-then-restore rather than grafting
	// onto the incompatible warm placeholder.
	createdPodB := new(bool)
	client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		if pod, ok := action.(ktesting.CreateAction).GetObject().(*corev1.Pod); ok && pod.Name == agent+"-thaw-b" {
			*createdPodB = true
		}
		return false, nil, nil
	})

	// The pool fills from the agent's OWN snapshot template (T2, "/srv/work"); we
	// then OVERWRITE the minted placeholder's topology label to a foreign T1 so the
	// wake-path lookup (which reads the live snapshot's T2 key) cannot match it —
	// modeling a placeholder minted for a different agent's shape co-resident on
	// the node.
	agentTmpl := warmSnapshotTemplate("/srv/work")
	exec := &scriptedNodeExec{stdout: warmPoolThawOKStdout, exit: 0}
	ft, pool, rec := newWarmPoolE2E(t, client, ns, agent, node, agentTmpl, exec)

	if err := pool.Reconcile(context.Background()); err != nil {
		t.Fatalf("pool Reconcile (mint): %v", err)
	}
	minted := mintedWarmPlaceholders(t, client, ns)
	if len(minted) != 1 {
		t.Fatalf("expected 1 minted placeholder, got %d", len(minted))
	}
	mintedName := minted[0].Name
	foreignTopo := podmanager.PodTemplateTopologyKey(warmSnapshotTemplate("/home/agent/bot"))
	agentTopo := podmanager.PodTemplateTopologyKey(agentTmpl)
	if foreignTopo == agentTopo {
		t.Fatal("test setup broken: foreign and agent topologies must differ")
	}
	// Relabel the minted pod to the foreign bucket so the co-resident placeholder
	// is a genuine topology MISS for this agent's thaw.
	minted[0].Labels[podmanager.LabelWarmTopology] = foreignTopo
	if _, err := client.CoreV1().Pods(ns).Update(context.Background(), &minted[0], metav1.UpdateOptions{}); err != nil {
		t.Fatalf("relabel minted placeholder to foreign topology: %v", err)
	}

	spec := podmanager.AgentPodSpec{
		AgentName: agent, Project: "gardenio", Mode: "crew", Role: "crew",
		RuntimeClassName: podmanager.RuntimeClassRunsc,
	}
	res, resumePath, err := ft.thaw(context.Background(), spec, "gd-bead")
	if err != nil {
		t.Fatalf("create-path fallback must succeed (Pod B reaches Ready, node exec OK), got err=%v", err)
	}
	if resumePath != gardeniometrics.ResumePathThawFresh {
		t.Errorf("topology miss must report the thaw-fresh (create-path) fallback, got %q", resumePath)
	}
	// The fallback minted its OWN create-path Pod B and restored onto that.
	if !*createdPodB {
		t.Errorf("topology miss must fall back to create-then-restore (Pod B %q not created)", agent+"-thaw-b")
	}
	if res == nil || res.PlaceholderPod != agent+"-thaw-b" {
		t.Fatalf("create-path thaw result must name the create-path Pod B %q, got %+v", agent+"-thaw-b", res)
	}
	// The incompatible warm placeholder must be untouched — still Ready, never
	// claimed or promoted (grafting the checkpoint onto it would fail runsc mount
	// validation, ka-0000002219).
	got, gerr := client.CoreV1().Pods(ns).Get(context.Background(), mintedName, metav1.GetOptions{})
	if gerr != nil {
		t.Fatalf("foreign-topology placeholder must remain: %v", gerr)
	}
	if got.Labels[podmanager.LabelWarmPlaceholder] != podmanager.WarmPlaceholderReady {
		t.Errorf("topology-miss placeholder must stay Ready (never claimed), label=%q", got.Labels[podmanager.LabelWarmPlaceholder])
	}
	if _, promotedAgent := got.Labels[podmanager.LabelAgent]; promotedAgent {
		t.Errorf("topology-miss placeholder must NOT be promoted to an agent pod, agent label=%q", got.Labels[podmanager.LabelAgent])
	}
	// The claim metric recorded a MISS for the agent's (node, topology) — the only
	// path into the create-then-restore fallback.
	wantMiss := "miss/" + node + "/" + agentTopo
	if len(rec.outcomes) != 1 || rec.outcomes[0] != wantMiss {
		t.Errorf("claim recorder outcomes = %v, want exactly [%q] (a topology miss)", rec.outcomes, wantMiss)
	}
}
