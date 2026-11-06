// Command ftrestart-probe is a TEST-ONLY driver for the gvisor-controller-restart
// E2E module (test-gvisor-controller-restart.sh, task ka-0000002295). It exercises
// the REAL production controller-RESTART durability code paths — the durable
// internal/podmanager.ConfigMapSnapshotStore (ka-0000001694) and the
// FreezeThawManager drain primitives (InflightThawCount / DrainThaws, the detached
// WithoutCancel post-commit ctx — main.go:909, ka-0000001355 harden-thaw +
// ka-0000004088 detached teardown) — against the LIVE gvisor-poc API server.
//
// # Why this exists (the gap it closes)
//
// This is the CONTROLLER-PROCESS durability concern, distinct from its node-runsc
// E2E siblings (freeze-thaw, rehydrate-ack, renudge-thaw). The unique, never-proven
// gap is durability across a REAL controller bounce MID-CYCLE:
//
//   - ConfigMapSnapshotStore exists SOLELY so a freeze→thaw snapshot RECORD survives
//     a controller pod restart (deploy / rollout / OOM / eviction) between an agent's
//     freeze and its thaw (ka-0000001694). Without it, the in-memory store is
//     process-local: a restart loses the record, shouldThaw() returns false, and the
//     agent SILENTLY COLD-STARTS, discarding a perfectly restorable snapshot. Yet the
//     store is unit-tested ONLY with an in-process fake clientset — its durability
//     across a REAL process boundary against a LIVE API server is never proven.
//
//   - DrainThaws + the WithoutCancel-detached post-commit ctx (the controller's
//     graceful-shutdown drain at main.go:909) keep an in-flight thaw's detached
//     critical section running after the leader-election ctx is cancelled by a
//     SIGTERM / leader-loss, so a controller roll does not abandon an in-flight
//     restore and orphan Pod B. These are also unit-tested ONLY inline with a fake
//     client — never against a genuine ctx-cancel boundary the way a real roll cancels.
//
// A controller restart is a REAL exec boundary, so case (a) drives it across TWO
// separate OS PROCESSES: proc1 (the "pre-restart controller") persists records into
// the live API server then exits; proc2 (the "post-restart controller", a fresh
// store with zero in-memory state) reads them back. An in-process test cannot model
// the property the store exists to provide — surviving a process death.
//
// # What it does (modes)
//
//	-mode=freeze-persist   [proc1, case (a)] Persists, via the GENUINE
//	                        ConfigMapSnapshotStore against the LIVE API server, a
//	                        COMPLETE snapshot record (an in-flight freeze that DID
//	                        race to persistence before the restart). Prints the agent
//	                        name + ConfigMap name and EXITS — modeling the controller
//	                        dying mid-freeze right after the Put committed.
//
//	-mode=freeze-recover   [proc2, case (a)] A FRESH ConfigMapSnapshotStore +
//	                        FreezeThawManager (zero in-memory carry-over — the
//	                        post-restart process) reads the live API server. Asserts
//	                        the COMPLETE record SURVIVED the restart (HasSnapshot==true,
//	                        Get round-trips the agent identity, SnapshotNode/Topology
//	                        resolve) so the post-restart wake path thaws instead of
//	                        silently cold-starting (the ka-0000001694 symptom), AND that
//	                        a HALF-snapshot (an absent record — a freeze that the
//	                        restart interrupted BEFORE the Put committed) is NOT trusted
//	                        (HasSnapshot==false → a clean cold start, never a
//	                        half-restore from a partial record).
//
//	-mode=thaw-drain       [case (b)] Drives a REAL FreezeThawManager.Thaw whose
//	                        detached post-commit critical section is parked IN FLIGHT
//	                        (gated restore exec) at the moment the inbound (leader-
//	                        election) ctx is CANCELLED — exactly what a controller roll
//	                        does. Asserts: InflightThawCount>0 survives the parent
//	                        cancel (registerThaw holds the slot for the whole call);
//	                        the detached dctx is NOT cancelled by the parent cancel
//	                        (WithoutCancel — the restore runs to a real conclusion); and
//	                        DrainThaws drains to 0 within budget once released (Pod B is
//	                        not orphaned mid-restore). Then a budget-expiry NEGATIVE
//	                        CONTROL: with the section still parked, DrainThaws under a
//	                        SHORT budget returns >0 (it genuinely waits on real in-flight
//	                        work, not a no-op that always reports drained).
//
// It is NOT part of any release Makefile / Dockerfile target — it is built from
// source on demand inside the E2E module (the same idiom as thawepoch-probe and
// renudge-thaw-probe) and never ships in a product image.
//
// Output is a single line of `key=value` pairs on stdout (parsed by the shell
// module) plus human-readable detail on stderr. Exit code is 0 iff the asserted
// outcome held.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"gardenio/controller/internal/podmanager"
)

func main() {
	var (
		mode       = flag.String("mode", "", "freeze-persist | freeze-recover | thaw-drain")
		agent      = flag.String("agent", "ftrestart-probe-agent", "agent name keying the snapshot/thaw")
		namespace  = flag.String("namespace", "gvisor-poc", "namespace the snapshot ConfigMap lives in (the controller ns)")
		node       = flag.String("node", "ftrestart-node-probe", "node stamped into the snapshot (Snapshot.Node)")
		kubeconfig = flag.String("kubeconfig", "", "path to a kubeconfig (empty = in-cluster)")
		timeout    = flag.Duration("timeout", 60*time.Second, "overall probe deadline")
		keep       = flag.Bool("keep", false, "leave the snapshot ConfigMap behind (debug); default deletes it on recover")
	)
	flag.Parse()

	if *mode == "" {
		fmt.Fprintln(os.Stderr, "ftrestart-probe: -mode is required (freeze-persist|freeze-recover|thaw-drain)")
		os.Exit(2)
	}

	switch *mode {
	case "freeze-persist":
		client := mustClient(*kubeconfig)
		runFreezePersist(client, *agent, *namespace, *node, *timeout)
	case "freeze-recover":
		client := mustClient(*kubeconfig)
		runFreezeRecover(client, *agent, *namespace, *node, *timeout, *keep)
	case "thaw-drain":
		// thaw-drain is process-local (the drain primitives are pure mutex/map state,
		// clientset-agnostic) — it uses a fake client + a gated NodeExecutor so it
		// never re-runs runsc. The LIVE Pod B liveness across the window is asserted
		// SEPARATELY by the shell harness, exactly as the renudge-thaw sibling does.
		runThawDrain(*agent, *namespace, *node, *timeout)
	default:
		fmt.Fprintf(os.Stderr, "ftrestart-probe: unknown mode %q (want freeze-persist|freeze-recover|thaw-drain)\n", *mode)
		os.Exit(2)
	}
}

// mustClient builds a kubernetes clientset against the live API server. The whole
// point of case (a) is durability against the LIVE API server, so there is no fake
// fallback — a failure here is fatal. Resolution order: an explicit -kubeconfig
// file; then in-cluster config (the probe-pod case); then the standard default
// loading rules (KUBECONFIG env + ~/.kube/config — the CI-runner case, where the
// probe runs in the job container, not a pod, exactly as kubectl does there).
func mustClient(kubeconfig string) kubernetes.Interface {
	var cfg *rest.Config
	var err error
	switch {
	case kubeconfig != "":
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
	default:
		cfg, err = rest.InClusterConfig()
		if err != nil {
			// Not in a pod — fall back to the default kubeconfig loading rules so the
			// probe authenticates the same way kubectl does in the e2e task container.
			cfg, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
				clientcmd.NewDefaultClientConfigLoadingRules(),
				&clientcmd.ConfigOverrides{},
			).ClientConfig()
		}
	}
	if err != nil {
		fmt.Printf("FTRESTART_PROBE mode=client ok=false error=%q\n", "build rest config: "+err.Error())
		fmt.Fprintf(os.Stderr, "  ✗ ftrestart-probe: build rest config: %v\n", err)
		os.Exit(1)
	}
	client, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		fmt.Printf("FTRESTART_PROBE mode=client ok=false error=%q\n", "new clientset: "+err.Error())
		fmt.Fprintf(os.Stderr, "  ✗ ftrestart-probe: new clientset: %v\n", err)
		os.Exit(1)
	}
	return client
}

// completeSnapshot builds a COMPLETE, internally-consistent snapshot record — the
// shape the freeze finalize path Puts after the checkpoint + bundle relocation
// committed (freezethaw.go:1373). A bare-agent topology (ContainerCount=2, empty
// sub-container roster) keeps the serialized payload comfortably under the ConfigMap
// 1MiB limit and is the canonical thawable shape HasSnapshot/SnapshotNode key on.
func completeSnapshot(agent, ns, node string) podmanager.Snapshot {
	return podmanager.Snapshot{
		AgentName:      agent,
		BeadID:         "ka-0000002295",
		Namespace:      ns,
		PodName:        agent + "-poda",
		Node:           node,
		ImagePath:      "/var/lib/gardenio/ckpt/" + agent,
		BundlePath:     "/var/lib/gardenio/bundle/" + agent,
		SandboxCID:     "sandboxcid-ftrestart",
		AgentCID:       "agentcid-ftrestart",
		PodUID:         "a-uid-ftrestart",
		ContainerCount: 2,
		CreatedAt:      time.Now().UTC(),
	}
}

// runFreezePersist (proc1, case (a)) persists a COMPLETE snapshot via the GENUINE
// durable ConfigMapSnapshotStore against the LIVE API server, then exits — modeling
// a controller that died mid-freeze right after its snapshot Put committed. The
// record now lives in a ConfigMap in the API server, independent of this process.
func runFreezePersist(client kubernetes.Interface, agent, ns, node string, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	store := podmanager.NewConfigMapSnapshotStore(client, ns)
	snap := completeSnapshot(agent, ns, node)
	if err := store.Put(ctx, snap); err != nil {
		fail("freeze-persist", "Put complete snapshot into live API server: "+err.Error())
	}
	// Read-back inside the SAME process is necessary but not sufficient (it does not
	// cross a restart) — assert it so a Put that silently no-ops is caught here rather
	// than masquerading as a "lost on restart" failure in proc2.
	if got, err := store.Get(ctx, agent); err != nil || got.AgentName != agent || got.Node != node {
		fail("freeze-persist", fmt.Sprintf("same-process read-back failed (err=%v got.agent=%q got.node=%q)", err, got.AgentName, got.Node))
	}

	fmt.Printf("FTRESTART_PROBE mode=freeze-persist ok=true agent=%q namespace=%q node=%q persisted=true\n", agent, ns, node)
	fmt.Fprintf(os.Stderr, "  ✓ freeze-persist: complete snapshot for %q committed to the live API server (ConfigMap in ns %q); process now EXITS (the mid-freeze restart boundary)\n", agent, ns)
	os.Exit(0)
}

// runFreezeRecover (proc2, case (a)) is the POST-RESTART controller: a FRESH
// ConfigMapSnapshotStore + FreezeThawManager with ZERO in-memory carry-over. It
// asserts (1) the COMPLETE record persisted by proc1 SURVIVED the restart and is
// trusted (HasSnapshot true, identity round-trips, node/topology resolve) — so the
// wake path thaws, not silently cold-starts; and (2) a HALF-snapshot (an ABSENT
// record — a freeze the restart cut off before the Put) is NOT trusted (HasSnapshot
// false → a clean cold start, never a half-restore from a partial record).
func runFreezeRecover(client kubernetes.Interface, agent, ns, node string, timeout time.Duration, keep bool) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// FRESH store + manager — the post-restart process has none of proc1's heap.
	store := podmanager.NewConfigMapSnapshotStore(client, ns)
	mgr := podmanager.NewFreezeThawManager(client, nil, store, nil,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))

	// (1) The COMPLETE record proc1 persisted must survive the restart and be trusted.
	hasComplete := mgr.HasSnapshot(ctx, agent)

	got, getErr := store.Get(ctx, agent)
	identityOK := getErr == nil && got.AgentName == agent && got.Node == node &&
		got.BeadID == "ka-0000002295" && got.ImagePath == "/var/lib/gardenio/ckpt/"+agent

	gotNode, nodeOK := mgr.SnapshotNode(ctx, agent)
	nodeResolved := nodeOK && gotNode == node
	_, topoOK := mgr.SnapshotTopology(ctx, agent)

	// (2) A HALF-snapshot = a freeze the restart interrupted BEFORE its Put committed,
	// so NO record exists for that agent. HasSnapshot must report it ABSENT (a store
	// miss → false), so the post-restart wake path cold-starts cleanly rather than
	// trusting a partial/half record. Use a never-persisted agent name as the
	// half-snapshot identity.
	halfAgent := agent + "-half"
	halfTrusted := mgr.HasSnapshot(ctx, halfAgent)
	_, halfNodeOK := mgr.SnapshotNode(ctx, halfAgent)

	if !keep {
		// Clean up the record we asserted on (best-effort; a leaked CM is harmless
		// and the shell harness also force-deletes by name).
		_ = store.Delete(ctx, agent)
	}

	line := fmt.Sprintf("FTRESTART_PROBE mode=freeze-recover has_complete=%t identity_ok=%t node_resolved=%t topo_ok=%t half_trusted=%t half_node_ok=%t",
		hasComplete, identityOK, nodeResolved, topoOK, halfTrusted, halfNodeOK)
	fmt.Println(line)

	if !hasComplete {
		fail("freeze-recover", "HasSnapshot==false for the COMPLETE record after the restart — the snapshot did NOT survive the controller bounce; the post-restart wake would SILENTLY COLD-START, discarding a restorable snapshot (the exact ka-0000001694 symptom ConfigMapSnapshotStore exists to prevent)")
	}
	if !identityOK {
		fail("freeze-recover", fmt.Sprintf("the recovered record did not round-trip the agent identity across the restart (err=%v agent=%q node=%q bead=%q image=%q)", getErr, got.AgentName, got.Node, got.BeadID, got.ImagePath))
	}
	if !nodeResolved {
		fail("freeze-recover", fmt.Sprintf("SnapshotNode did not resolve the persisted node after the restart (got=%q ok=%t want=%q) — the node-local thaw could not claim a co-resident placeholder", gotNode, nodeOK, node))
	}
	if !topoOK {
		fail("freeze-recover", "SnapshotTopology did not resolve after the restart — the warm-B pool could not enumerate the bucket to thaw into")
	}
	if halfTrusted {
		fail("freeze-recover", "HasSnapshot==true for a HALF-snapshot (a freeze interrupted before its Put committed) — the post-restart wake would TRUST a record that never fully persisted and attempt a half-restore instead of a clean cold start")
	}
	if halfNodeOK {
		fail("freeze-recover", "SnapshotNode resolved for the absent half-snapshot — a partial record leaked a node the wake path would pin a placeholder to")
	}

	fmt.Fprintf(os.Stderr, "  ✓ freeze-recover: a FRESH post-restart store recovered the COMPLETE record from the live API server (HasSnapshot true, identity+node+topology round-trip) → wake THAWS not cold-starts; a half-snapshot is ABSENT → clean cold start (not trusted). ConfigMapSnapshotStore durability across a real process boundary PROVEN.\n")
	os.Exit(0)
}

// --- case (b): thaw-drain --------------------------------------------------

// thawOKStdout is a barrier-restore stdout that passes every Thaw post-state
// assertion (netns inode match, rc=0, all containers running). Mirrors the node
// script's success output; the restore MECHANICS are proven by the freeze-thaw /
// rehydrate-ack siblings, so here we only need a clean success so the genuine drain
// COORDINATION (InflightThawCount / DrainThaws / detached-ctx) runs to completion.
const thawOKStdout = `    barrier restore
FT_B_SANDBOX_CID=bsandbox888
FT_B_AGENT_CID=bagent999
FT_B_POD_UID=b-uid-ftrestart
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
FT_NETNS_IFACES=1
FT_NETNS_DEFROUTE=yes
FT_CONTAINERS_RUNNING=yes
`

const restoreScriptSig = "nsenter -t 1 -m -p -n"

// gatedDrainExecutor is the genuine NodeExecutor seam: it intercepts the restore
// script exec so the probe never re-runs runsc. It PARKS the restore in flight on
// `release` AND records the ctx the detached dctx handed it — so the probe can
// assert that the detached post-commit ctx is NOT cancelled when the inbound
// (leader-election) parent ctx is cancelled (the WithoutCancel property). It also
// signals `started` once the restore is reached, so the probe knows the thaw has
// committed Pod B and entered the detached critical section.
type gatedDrainExecutor struct {
	mu          sync.Mutex
	started     chan struct{}
	release     chan struct{}
	startedOnce bool
	execCtx     context.Context
}

func (g *gatedDrainExecutor) Exec(ctx context.Context, _ string, script string) (podmanager.NodeExecResult, error) {
	if strings.Contains(script, restoreScriptSig) {
		g.mu.Lock()
		g.execCtx = ctx
		if !g.startedOnce {
			g.startedOnce = true
			close(g.started)
		}
		g.mu.Unlock()
		<-g.release // hold the restore in flight (the detached post-commit section parked)
	}
	return podmanager.NodeExecResult{Stdout: thawOKStdout}, nil
}

// detachedCtxErr returns the error of the ctx the restore exec ran under (nil = not
// cancelled). It is the dctx the thaw built via context.WithoutCancel(inbound) —
// asserting it stays nil after the parent cancel proves the detach.
func (g *gatedDrainExecutor) detachedCtxErr() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.execCtx == nil {
		return nil
	}
	return g.execCtx.Err()
}

func seedPodBClient(podB, bUID, ns, node string) kubernetes.Interface {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podB, Namespace: ns, UID: types.UID(bUID)},
		Spec:       corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	return fake.NewClientset(pod)
}

// runThawDrain (case (b)) drives a REAL Thaw whose detached post-commit critical
// section is parked IN FLIGHT when the inbound ctx is cancelled (a controller roll),
// and asserts the harden-thaw drain contract: the slot survives the cancel, the
// detached ctx is NOT cancelled, and DrainThaws drains within budget. A short-budget
// drain against the still-parked section is the negative control.
func runThawDrain(agent, ns, node string, timeout time.Duration) {
	podB := agent + "-thaw-b"
	bUID := "b-uid-ftrestart"
	client := seedPodBClient(podB, bUID, ns, node)
	exec := &gatedDrainExecutor{started: make(chan struct{}), release: make(chan struct{})}

	store := podmanager.NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), podmanager.Snapshot{
		AgentName: agent, BeadID: "ka-0000002295", Namespace: ns, PodName: "",
		Node: node, ImagePath: "/var/lib/gardenio/ckpt/" + agent, BundlePath: "/var/lib/gardenio/bundle/" + agent,
		SandboxCID: "sandboxcid-ftrestart", AgentCID: "agentcid-ftrestart", PodUID: "a-uid-ftrestart", ContainerCount: 2,
	})
	mgr := podmanager.NewFreezeThawManager(client, exec, store, nil,
		slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	mgr.PodReadyTimeout = 5 * time.Second
	mgr.PodReadyPollInterval = time.Millisecond

	// PlaceholderPod set ⇒ Thaw grafts onto the live Pod B and skips Create/waitReady
	// — the warm-graft hot path. SkipRehydration scopes the probe to the drain
	// coordination (no rehydration hand-off / image reap to drive on the node).
	req := podmanager.ThawRequest{AgentName: agent, PlaceholderPod: podB, SkipRehydration: true}

	// The inbound ctx models the controller's leader-election ctx. We cancel it
	// MID-THAW (the restore parked in flight) — exactly what a SIGTERM / leader-loss
	// does — and assert the detached post-commit section keeps running.
	inbound, cancelInbound := context.WithCancel(context.Background())

	type thawOut struct {
		res *podmanager.ThawResult
		err error
	}
	out := make(chan thawOut, 1)
	go func() {
		res, err := mgr.Thaw(inbound, req)
		out <- thawOut{res, err}
	}()

	// Wait until the thaw is provably inside the detached restore exec (Pod B
	// committed, post-commit critical section entered).
	select {
	case <-exec.started:
	case <-time.After(timeout):
		fail("thaw-drain", "thaw never reached the restore exec (Pod B graft pre-restore failed?)")
	}

	// (a) A thaw is in flight — the slot the drain waits on.
	inflightBefore := mgr.InflightThawCount()

	// Cancel the inbound (leader-election) ctx — the controller-roll moment. The
	// detached post-commit section (running under context.WithoutCancel) must keep
	// going; InflightThawCount must stay >0.
	cancelInbound()
	time.Sleep(50 * time.Millisecond) // let the cancel propagate if it were going to

	inflightAfterCancel := mgr.InflightThawCount()
	// (b) The detached restore ctx must NOT be cancelled by the parent cancel — the
	// WithoutCancel property. (nil err = still live.)
	detachedErr := exec.detachedCtxErr()

	// (c) Negative control: with the section STILL parked, a SHORT-budget DrainThaws
	// must return >0 — it genuinely waits on real in-flight work, not a no-op that
	// always reports drained.
	shortDrainCtx, shortCancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	shortRemaining := mgr.DrainThaws(shortDrainCtx, 50*time.Millisecond)
	shortCancel()

	// Now release the parked restore so the detached section completes, and assert the
	// full-budget drain reaches 0 (Pod B not orphaned mid-restore).
	close(exec.release)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), timeout)
	fullRemaining := mgr.DrainThaws(drainCtx, 25*time.Millisecond)
	drainCancel()

	// The thaw goroutine must return cleanly (the detached section ran to conclusion
	// despite the parent cancel).
	var r thawOut
	select {
	case r = <-out:
	case <-time.After(timeout):
		fail("thaw-drain", "thaw did not return after the parked restore was released — the detached section wedged")
	}

	inflightAfterDrain := mgr.InflightThawCount()

	line := fmt.Sprintf("FTRESTART_PROBE mode=thaw-drain inflight_before=%d inflight_after_cancel=%d detached_ctx_cancelled=%t short_drain_remaining=%d full_drain_remaining=%d inflight_after_drain=%d thaw_err=%q",
		inflightBefore, inflightAfterCancel, detachedErr != nil, shortRemaining, fullRemaining, inflightAfterDrain, errStr(r.err))
	fmt.Println(line)

	if inflightBefore < 1 {
		fail("thaw-drain", "InflightThawCount==0 while a thaw was provably parked in the restore exec — the drain would not wait for the in-flight restore")
	}
	if inflightAfterCancel < 1 {
		fail("thaw-drain", "InflightThawCount dropped to 0 after the inbound ctx was cancelled — the parent cancel abandoned the in-flight thaw (the detached slot did not survive a controller roll); DrainThaws would exit early and the process would exit mid-restore, orphaning Pod B")
	}
	if detachedErr != nil {
		fail("thaw-drain", "the detached post-commit ctx WAS cancelled by the parent cancel ("+detachedErr.Error()+") — context.WithoutCancel did not decouple the restore; a controller roll would cancel the in-flight restore at ~6.8s (the ka-0000004088 confound)")
	}
	if shortRemaining < 1 {
		fail("thaw-drain", "short-budget DrainThaws returned 0 while the restore was still parked — DrainThaws is a no-op that always reports drained, not a genuine wait on in-flight work")
	}
	if fullRemaining != 0 {
		fail("thaw-drain", fmt.Sprintf("full-budget DrainThaws returned %d (want 0) — the in-flight thaw did not drain within budget after release; a controller roll would SIGKILL it mid-restore", fullRemaining))
	}
	if inflightAfterDrain != 0 {
		fail("thaw-drain", fmt.Sprintf("InflightThawCount==%d after the drain returned 0 — the slot leaked", inflightAfterDrain))
	}
	if r.err != nil {
		fail("thaw-drain", "the thaw returned an error despite the detached section running to conclusion: "+r.err.Error())
	}

	fmt.Fprintf(os.Stderr, "  ✓ thaw-drain: thaw in flight (count=%d) survived the inbound-ctx cancel (count=%d, detached ctx NOT cancelled); short-budget drain genuinely waited (remaining=%d); full drain reached 0 after release → Pod B not orphaned across a controller roll mid-thaw. DrainThaws + WithoutCancel detach PROVEN.\n",
		inflightBefore, inflightAfterCancel, shortRemaining)
	os.Exit(0)
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func fail(mode, msg string) {
	fmt.Printf("FTRESTART_PROBE mode=%s ok=false error=%q\n", mode, msg)
	fmt.Fprintf(os.Stderr, "  ✗ %s: %s\n", mode, msg)
	os.Exit(1)
}
