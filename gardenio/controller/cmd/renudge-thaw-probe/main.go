// Command renudge-thaw-probe is a TEST-ONLY driver for the gvisor-renudge-thaw
// E2E module (test-gvisor-renudge-thaw.sh, task ka-0000001841). It exercises the
// REAL production re-nudge mid-THAW coordination code path — the per-agent
// single-flight in internal/podmanager.FreezeThawManager.Thaw and the
// FreezeThawManager.IsThawing signal — on the gvisor-poc substrate, beside a
// REAL runsc-restored agent alive in Pod B's netns.
//
// # Why this exists (the gap it closes)
//
// MR !4719 (impl ka-0000005830, design ka-0000005125) landed the re-nudge
// mid-THAW coordination: a user message arriving while a thaw of the target
// agent is IN FLIGHT must NOT race a half-restored agent or spawn a second Pod
// B. Unlike freeze, thaw has no live fallback Pod A, so the message is BUFFERED
// (bridge waitForThawAndNudge, keyed on the controller's IsThawing) until the
// rehydrated Pod B is live, then delivered exactly once — and a concurrent
// spawn/reconciler pass during the thaw must JOIN the in-flight thaw rather than
// drive a second `runsc restore` (DEDUP, the ka-0000005680 admit-fail class).
//
// That contract is asserted at the unit layer (freezethaw_singleflight_test.go,
// bot_mentions_thaw_buffer_test.go, thaw_status_endpoint_test.go) — but NOTHING
// drove the REAL manager single-flight + IsThawing against a marker-style
// concurrent-thaw round-trip on the ACTUAL substrate while a real runsc-restored
// agent is ALIVE in Pod B's netns. This probe is that missing piece: it builds
// the GENUINE FreezeThawManager (no re-implementation), seeds the snapshot whose
// PlaceholderPod is the live Pod B the shell harness just restored into, and
// runs the REAL Thaw single-flight + IsThawing. A regression in either fails
// THIS e2e rather than only the next prod canary. (The restore SCRIPT exec is
// intercepted by a recording NodeExecutor — Pod B is already restored by the
// shell, and the restore MECHANICS are proven by the freeze-thaw / rehydrate-ack
// siblings; this probe isolates the COORDINATION the 4-change MR added.)
//
// # What it does (modes)
//
//	-mode=dedup   Drive TWO concurrent Thaw calls for the SAME agent against the
//	              live Pod B: the first (owner) parks inside a gated restore exec;
//	              the second (joiner) must JOIN it (no second restore). Asserts:
//	                (a) IsThawing(agent)==true while the thaw is in flight — the
//	                    exact signal the bridge buffer keys on (a mid-thaw nudge
//	                    is held, not raced);
//	                (b) exactly ONE restore exec drove the wake (no duplicate Pod
//	                    B — the DEDUP invariant);
//	                (c) both callers returned the SAME Pod B with no error (the
//	                    joiner adopted the owner's result, exactly-once);
//	                (d) IsThawing(agent)==false once the thaw fully resolved (the
//	                    buffer releases → the nudge is delivered).
//
//	-mode=clears  Negative control: a SEQUENTIAL second wake (after the first
//	              thaw fully resolved) must run a FRESH restore — the single-flight
//	              entry is released on return, not cached. Asserts two sequential
//	              wakes ⇒ two restore execs (and IsThawing false between them), so
//	              the dedup of mode=dedup is genuine join-while-in-flight, not a
//	              stuck-forever "always one restore" that would wrongly suppress a
//	              legitimate later thaw.
//
// It is NOT part of any release Makefile / Dockerfile target — it is built from
// source on demand inside the E2E module (the same way thawepoch-probe is) and
// never ships in a product image.
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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"gardenio/controller/internal/podmanager"
)

func main() {
	var (
		mode    = flag.String("mode", "dedup", "dedup | clears")
		agent   = flag.String("agent", "renudge-probe-agent", "agent name keying the thaw single-flight")
		podB    = flag.String("pod-b", "", "name of the live (Ready) placeholder Pod B to graft onto")
		bUID    = flag.String("pod-b-uid", "b-uid-probe", "Pod B kubelet UID (stamped onto the seeded fake pod)")
		ns      = flag.String("namespace", "gvisor-poc", "namespace the Pod B lives in")
		node    = flag.String("node", "node-probe", "node the snapshot image is local to (must match Pod B's nodeName)")
		timeout = flag.Duration("timeout", 30*time.Second, "overall probe deadline")
	)
	flag.Parse()

	if *podB == "" {
		fmt.Fprintln(os.Stderr, "renudge-thaw-probe: -pod-b is required (the live placeholder Pod B name)")
		os.Exit(2)
	}

	switch *mode {
	case "dedup":
		runDedup(*agent, *podB, *bUID, *ns, *node, *timeout)
	case "clears":
		runClears(*agent, *podB, *bUID, *ns, *node, *timeout)
	default:
		fmt.Fprintf(os.Stderr, "renudge-thaw-probe: unknown mode %q (want dedup|clears)\n", *mode)
		os.Exit(2)
	}
}

// thawOKStdout is a barrier-restore stdout that passes every Thaw post-state
// assertion (netns inode match, rc=0, all containers running). It mirrors the
// node script's success output; the restore MECHANICS are proven by the
// freeze-thaw / rehydrate-ack siblings, so here we only need a clean success so
// the genuine Thaw COORDINATION (single-flight + IsThawing) runs to completion.
const thawOKStdout = `    barrier restore
FT_B_SANDBOX_CID=bsandbox888
FT_B_AGENT_CID=bagent999
FT_B_POD_UID=b-uid-probe
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

// gatedThawExecutor is the genuine NodeExecutor seam: it intercepts the restore
// script exec so the probe never re-runs runsc (Pod B is already restored by the
// shell). The FIRST restore parks on `release` so a concurrent Thaw for the same
// agent overlaps the in-flight one; it counts restore execs so the single-flight
// invariant ("exactly one restore per agent per concurrent wake") is assertable.
// gateFirst=false (mode=clears) lets every restore return immediately.
type gatedThawExecutor struct {
	mu           sync.Mutex
	restoreCount int
	scripts      []string
	started      chan struct{}
	release      chan struct{}
	startedOnce  bool
	gateFirst    bool
}

func (g *gatedThawExecutor) Exec(_ context.Context, _ string, script string) (podmanager.NodeExecResult, error) {
	if strings.Contains(script, restoreScriptSig) {
		g.mu.Lock()
		g.restoreCount++
		g.scripts = append(g.scripts, script)
		first := g.restoreCount == 1
		if !g.startedOnce {
			g.startedOnce = true
			close(g.started)
		}
		g.mu.Unlock()
		if first && g.gateFirst {
			<-g.release // hold the first restore in flight until the test releases it
		}
	}
	return podmanager.NodeExecResult{Stdout: thawOKStdout}, nil
}

func (g *gatedThawExecutor) restores() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.restoreCount
}

// seedPodBClient builds a fake clientset whose Pod B is Running + Ready on the
// target node — the same shape the genuine warm-graft Thaw path Gets and
// validates (phase not terminal, podIsReady, NodeName==snap.Node). The live Pod
// B's actual liveness on the substrate is asserted SEPARATELY by the shell
// harness (kubectl phase=Running), exactly as test-gvisor-rehydrate-ack.sh does;
// the in-manager single-flight / IsThawing state this probe asserts is
// clientset-agnostic (pure mutex/map), so a fake Pod B object is faithful.
func seedPodBClient(podB, bUID, ns, node string) *fake.Clientset {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: podB, Namespace: ns, UID: types.UID(bUID)},
		Spec:       corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	client := fake.NewClientset(pod)
	// Keep any (defensive) create path Ready too, in case a future Thaw refactor
	// re-creates rather than grafts — the probe should still exercise one restore.
	client.PrependReactor("create", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		ca, ok := action.(ktesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		p, ok := ca.GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		p.Status.Phase = corev1.PodRunning
		p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.PodReady, Status: corev1.ConditionTrue})
		if p.UID == "" {
			p.UID = types.UID(bUID)
		}
		return false, nil, nil
	})
	return client
}

func newManager(client *fake.Clientset, exec podmanager.NodeExecutor, agent, podB, ns, node string) (*podmanager.FreezeThawManager, podmanager.ThawRequest) {
	store := podmanager.NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), podmanager.Snapshot{
		AgentName: agent, BeadID: "ka-RENUDGE-PROBE", Namespace: ns, PodName: "",
		Node: node, ImagePath: "/var/tmp/gb-ckpt/" + agent, BundlePath: "/var/tmp/gb-bundle/" + agent,
		SandboxCID: "sandboxcid222", AgentCID: "agentcid111", PodUID: "a-uid-123", ContainerCount: 2,
	})
	m := podmanager.NewFreezeThawManager(client, exec, store, nil, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})))
	m.PodReadyTimeout = 5 * time.Second
	m.PodReadyPollInterval = time.Millisecond
	// PlaceholderPod set ⇒ Thaw grafts onto the live Pod B (Get + Ready + node
	// match) and SKIPS Create/waitPodReady — the real warm-graft hot path. The
	// snapshot's empty PodName makes the Pod-A teardown barrier a no-op.
	// SkipRehydration keeps the probe scoped to the single-flight + IsThawing
	// coordination (no rehydration hand-off / image reap to drive on the node).
	req := podmanager.ThawRequest{AgentName: agent, PlaceholderPod: podB, SkipRehydration: true}
	return m, req
}

type thawOut struct {
	res *podmanager.ThawResult
	err error
}

// runDedup drives the headline invariant: a re-nudge / duplicate spawn landing
// mid-thaw must JOIN the in-flight thaw (one Pod B, IsThawing true), and the
// thaw releases cleanly (IsThawing false) so the buffered nudge is delivered.
func runDedup(agent, podB, bUID, ns, node string, timeout time.Duration) {
	client := seedPodBClient(podB, bUID, ns, node)
	exec := &gatedThawExecutor{started: make(chan struct{}), release: make(chan struct{}), gateFirst: true}
	m, req := newManager(client, exec, agent, podB, ns, node)

	out1 := make(chan thawOut, 1)
	out2 := make(chan thawOut, 1)

	// Owner thaw — parks inside the gated restore exec.
	go func() {
		res, err := m.Thaw(context.Background(), req)
		out1 <- thawOut{res, err}
	}()

	// Wait until the owner is provably inside the restore, THEN assert the
	// buffer signal and fire the concurrent (joiner) thaw.
	select {
	case <-exec.started:
	case <-time.After(timeout):
		fail("dedup", "owner thaw never reached the restore exec (Pod B graft pre-restore failed?)")
	}

	// (a) IsThawing must be TRUE while the thaw is in flight — the signal the
	// controller's GET /api/v1/thaw/status returns and the bridge buffers on.
	inflightThawing := m.IsThawing(agent)

	go func() {
		res, err := m.Thaw(context.Background(), req)
		out2 <- thawOut{res, err}
	}()

	// Give the joiner a moment to attach to the in-flight entry, then release.
	time.Sleep(80 * time.Millisecond)
	close(exec.release)

	var r1, r2 thawOut
	select {
	case r1 = <-out1:
	case <-time.After(timeout):
		fail("dedup", "owner thaw did not return")
	}
	select {
	case r2 = <-out2:
	case <-time.After(timeout):
		fail("dedup", "joined thaw did not return")
	}

	restores := exec.restores()
	// (d) IsThawing must clear once the thaw fully resolved (refcount drained) —
	// the buffer's release condition. Poll briefly: the owner's registerThaw
	// release fires on return, which may land microseconds after the channel read.
	clearedThawing := !waitIsThawing(m, agent, false, time.Second)

	samePodB := r1.res != nil && r2.res != nil && r1.res.PlaceholderPod == r2.res.PlaceholderPod

	line := fmt.Sprintf("THAW_RENUDGE_PROBE mode=dedup inflight_thawing=%t restores=%d owner_err=%q joiner_err=%q same_podb=%t cleared_thawing=%t podb=%q",
		inflightThawing, restores, errStr(r1.err), errStr(r2.err), samePodB, clearedThawing, placeholder(r1.res))
	fmt.Println(line)

	if !inflightThawing {
		fail("dedup", "IsThawing was FALSE while a thaw was in flight — the bridge buffer would NOT hold the mid-thaw nudge (it would race a half-restored agent)")
	}
	if r1.err != nil {
		fail("dedup", "owner thaw failed: "+r1.err.Error())
	}
	if r2.err != nil {
		fail("dedup", "joined thaw failed: "+r2.err.Error())
	}
	if restores != 1 {
		fail("dedup", fmt.Sprintf("restore execs = %d, want 1 — a 2nd concurrent thaw started a 2nd `runsc restore` (duplicate Pod B, the ka-0000005680 admit-fail class). Single-flight DEDUP broken.", restores))
	}
	if !samePodB {
		fail("dedup", fmt.Sprintf("joiner got a different Pod B (owner=%q joiner=%q) — the join did not adopt the owner's result, so the nudge would not land exactly-once on the one rehydrated agent", placeholder(r1.res), placeholder(r2.res)))
	}
	if !clearedThawing {
		fail("dedup", "IsThawing stayed TRUE after both thaws returned — the buffer would never release and the nudge would hit the deadline instead of being delivered")
	}

	fmt.Fprintf(os.Stderr, "  ✓ dedup: IsThawing true in-flight → buffered; 1 restore (no dup Pod B); joiner adopted Pod B %q; IsThawing cleared → nudge delivered exactly-once\n", placeholder(r1.res))
	os.Exit(0)
}

// runClears is the negative control: a SEQUENTIAL second wake must run a FRESH
// restore (the single-flight entry released on return, not cached). Proves the
// dedup join is "join WHILE in flight", not a degenerate "always one restore"
// that would wrongly suppress a legitimate later thaw of the same agent.
func runClears(agent, podB, bUID, ns, node string, timeout time.Duration) {
	client := seedPodBClient(podB, bUID, ns, node)
	exec := &gatedThawExecutor{started: make(chan struct{}), release: make(chan struct{}), gateFirst: false}
	m, req := newManager(client, exec, agent, podB, ns, node)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if _, err := m.Thaw(ctx, req); err != nil {
		fail("clears", "first sequential thaw failed: "+err.Error())
	}
	// Between wakes IsThawing must be false (entry released).
	betweenThawing := m.IsThawing(agent)
	if _, err := m.Thaw(ctx, req); err != nil {
		fail("clears", "second sequential thaw failed: "+err.Error())
	}
	restores := exec.restores()

	line := fmt.Sprintf("THAW_RENUDGE_PROBE mode=clears restores=%d between_thawing=%t",
		restores, betweenThawing)
	fmt.Println(line)

	if betweenThawing {
		fail("clears", "IsThawing was TRUE between two sequential wakes — the single-flight entry / refcount did not clear on return")
	}
	if restores != 2 {
		fail("clears", fmt.Sprintf("restore execs = %d, want 2 — each sequential wake must run its OWN restore; a cached single-flight result would wrongly suppress the later legitimate thaw", restores))
	}
	fmt.Fprintf(os.Stderr, "  ✓ clears: two sequential wakes ⇒ two restores; IsThawing cleared between them (single-flight is join-while-in-flight, not stuck-forever)\n")
	os.Exit(0)
}

// waitIsThawing polls IsThawing until it equals `want` or the budget expires;
// returns the final observed value.
func waitIsThawing(m *podmanager.FreezeThawManager, agent string, want bool, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for {
		got := m.IsThawing(agent)
		if got == want || time.Now().After(deadline) {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func placeholder(r *podmanager.ThawResult) string {
	if r == nil {
		return ""
	}
	return r.PlaceholderPod
}

func errStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func fail(mode, msg string) {
	fmt.Printf("THAW_RENUDGE_PROBE mode=%s ok=false error=%q\n", mode, msg)
	fmt.Fprintf(os.Stderr, "  ✗ %s: %s\n", mode, msg)
	os.Exit(1)
}
