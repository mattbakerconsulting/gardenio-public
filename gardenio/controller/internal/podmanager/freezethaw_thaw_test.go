package podmanager

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestThaw_Success(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	// Seed a snapshot as if Freeze had run.
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", BeadID: "gd-AGENT", Namespace: ns, PodName: "agentsim",
		Node: node, ImagePath: "/var/tmp/gb-ckpt/agentsim", BundlePath: "/var/tmp/gb-bundle/agentsim",
		SandboxCID: "sandboxcid222", AgentCID: "agentcid111", PodUID: "a-uid-123", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	rehydr := &recordingRehydrator{}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err != nil {
		t.Fatalf("Thaw: %v", err)
	}
	if res.PlaceholderPod != "agentsim-thaw-b" {
		t.Errorf("placeholder = %q", res.PlaceholderPod)
	}
	if res.RestoredPID != "37737" {
		t.Errorf("restored pid = %q", res.RestoredPID)
	}
	if res.RestoredNetns != "net:[4026532540]" || res.BNetns != res.RestoredNetns {
		t.Errorf("netns proof failed: restored=%q B=%q", res.RestoredNetns, res.BNetns)
	}
	if !res.Rehydrated {
		t.Error("rehydration should have fired")
	}
	// Placeholder Pod B was created, runsc-classed, node-pinned, sleep-overridden.
	podB, err := client.CoreV1().Pods(ns).Get(context.Background(), "agentsim-thaw-b", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Pod B not created: %v", err)
	}
	if podB.Spec.RuntimeClassName == nil || *podB.Spec.RuntimeClassName != RuntimeClassRunsc {
		t.Errorf("Pod B runtimeClass not runsc")
	}
	if podB.Spec.NodeName != node {
		t.Errorf("Pod B node = %q, want %q (node-local image)", podB.Spec.NodeName, node)
	}
	// Pod B KEEPS the run-to-completion init-clone container so the workspace
	// (repo) is re-hydrated before the graft (C6 strategy b, ka-0000007582); it
	// drops only always-on native sidecars. livePodA carries one init-clone.
	if len(podB.Spec.InitContainers) != 1 || podB.Spec.InitContainers[0].Name != "init-clone" {
		t.Errorf("Pod B should keep the init-clone init container for repo rehydration, got %+v", podB.Spec.InitContainers)
	}
	if got := podB.Spec.Containers[0].Command; len(got) == 0 || !strings.Contains(strings.Join(got, " "), "sleep") {
		t.Errorf("Pod B container command not a sleep placeholder: %v", got)
	}
	// Rehydration request carried the restored pid + placeholder pod + deadline.
	if len(rehydr.reqs) != 1 {
		t.Fatalf("rehydration calls = %d, want 1", len(rehydr.reqs))
	}
	if rehydr.reqs[0].RestoredPID != "37737" || rehydr.reqs[0].PodName != "agentsim-thaw-b" {
		t.Errorf("rehydration req = %+v", rehydr.reqs[0])
	}
	if rehydr.reqs[0].Deadline != DefaultRehydrateDeadline {
		t.Errorf("rehydration deadline = %v, want default", rehydr.reqs[0].Deadline)
	}
	// Thaw script drives the count-gated barrier + normalization under nsenter.
	script := exec.scripts[0]
	for _, want := range []string{"nsenter -t 1 -m -p -n", "--detach", "/var/run/", "/run/", "ROOT_RC", "lowerdir"} {
		if !strings.Contains(script, want) {
			t.Errorf("thaw script missing %q", want)
		}
	}
}

// TestThaw_RehydrationRequestCarriesIdentityHints asserts the C3 fields
// (ka-0000004017): the rehydration request must carry Pod B's kubelet UID and
// the snapshot's freeze wall-clock so the agent-side poison/freshness stages can
// reject a Pod-A-identity token (design C1/Stage 1).
func TestThaw_RehydrationRequestCarriesIdentityHints(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	frozenAt := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a-uid-123", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
		CreatedAt:   frozenAt,
	})
	rehydr := &recordingRehydrator{}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	if _, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"}); err != nil {
		t.Fatalf("Thaw: %v", err)
	}
	if len(rehydr.reqs) != 1 {
		t.Fatalf("rehydration calls = %d, want 1", len(rehydr.reqs))
	}
	req := rehydr.reqs[0]
	if req.PodUID != "b-uid-456" {
		t.Errorf("rehydration PodUID = %q, want Pod B's kubelet UID b-uid-456 (not Pod A's)", req.PodUID)
	}
	if !req.FrozenAt.Equal(frozenAt) {
		t.Errorf("rehydration FrozenAt = %v, want snapshot CreatedAt %v", req.FrozenAt, frozenAt)
	}
}

func TestThaw_NetnsMismatchFails(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	// Restored netns differs from Pod B's — the network identity did NOT come
	// from kubelet; this must fail loudly.
	stdout := strings.Replace(thawOKStdout, "FT_RESTORED_NETNS=net:[4026532540]", "FT_RESTORED_NETNS=net:[4026539999]", 1)
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err == nil || !strings.Contains(err.Error(), "netns") {
		t.Fatalf("want netns-mismatch error, got %v", err)
	}
}

// TestThaw_LoOnlyNetnsFailsClosed proves the fix-A netns-CONTENT assertion
// (ka-0000007713): a restore whose netns inode MATCHES Pod B's but whose netns
// holds only `lo` (no eth0, no default route) must fail loud at restore time, not
// 180s later on the rehydrate-ack network-unreachable timeout. This is the canary
// fr84 defect: the inode proof passed against an interface-less netns. The node
// script itself fail-closes (FT_ROOT_RC=128) on a lo-only netns; here we simulate
// the (shouldn't-happen) case where it reports verified rc=0 but the content
// sentinels say the netns is network-less, and assert the Go post-state check is
// defense-in-depth against it. Rehydration must NOT fire.
func TestThaw_LoOnlyNetnsFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(string) string
	}{
		{"no non-loopback interface", func(s string) string {
			return strings.Replace(s, "FT_NETNS_IFACES=1", "FT_NETNS_IFACES=0", 1)
		}},
		{"no default route", func(s string) string {
			return strings.Replace(s, "FT_NETNS_DEFROUTE=yes", "FT_NETNS_DEFROUTE=no", 1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns, node := "gvisor-poc", "node-1"
			client := fake.NewClientset()
			client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
			exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: tc.mutate(thawOKStdout)}}
			store := NewInMemorySnapshotStore()
			_ = store.Put(context.Background(), Snapshot{
				AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
				SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
				PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
			})
			rehydr := &recordingRehydrator{}
			m := newFreezeThawManager(t, client, exec, store, rehydr)

			_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
			if err == nil || !strings.Contains(err.Error(), "lo-only") {
				t.Fatalf("want lo-only netns fail-closed error, got %v", err)
			}
			if len(rehydr.reqs) != 0 {
				t.Error("rehydration must not fire when the restored netns is network-less")
			}
		})
	}
}

// TestThaw_FailureSurfacesNetnsGraftDiagnostics proves the ka-0000001790
// observability fix: when the thaw node script fail-closes a lo-only restore
// (FT_ROOT_RC=128 + exit 1), the controller error must surface the netns-graft
// sentinels emitted on STDOUT (CNI-confirmed / pinned / L3-reasserted / iface
// count / default route) — not just the truncated stderr tail — so the next
// canary localizes the failure to its layer without a node-side redrive.
func TestThaw_FailureSurfacesNetnsGraftDiagnostics(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	// Simulate the [6a] lo-only fail-closed: the script set FT_ROOT_RC=128 and
	// exited 1, reporting an eth0-but-no-route netns AFTER an L3 re-assert ran (so
	// the strip was deeper than pod-side L3 — the exact thing the diag disambiguates).
	stdout := thawOKStdout
	stdout = strings.Replace(stdout, "FT_ROOT_RC=0", "FT_ROOT_RC=128", 1)
	stdout = strings.Replace(stdout, "FT_NETNS_DEFROUTE=yes", "FT_NETNS_DEFROUTE=no", 1)
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout, Stderr: "FATAL: lo-only netns", ExitCode: 1}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	rehydr := &recordingRehydrator{}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err == nil {
		t.Fatal("want thaw failure, got nil")
	}
	// All three graft sentinels must appear in the error so the failure is
	// self-diagnosing controller-side.
	for _, want := range []string{
		"cni-confirmed=\"yes\"",
		"cni-pod-ip=\"10.30.136.3/32\"",
		"pinned=\"/var/run/netns/gb-thaw-bsandbox888\"",
		"l3-reasserted=\"yes\"",
		"self-ipam=\"yes\"",
		"default-route=\"no\"",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("thaw error missing netns-graft diagnostic %q; got: %v", want, err)
		}
	}
	if len(rehydr.reqs) != 0 {
		t.Error("rehydration must not fire on a fail-closed restore")
	}
}

// TestThaw_LegacySnapshotWithoutNetnsContentSentinels proves the assertion is
// backward-compatible: a snapshot restored by an older node script that does not
// emit FT_NETNS_IFACES / FT_NETNS_DEFROUTE is treated as not-asserted (the inode
// proof still gates it), NOT as a failure — so older in-flight snapshots still
// thaw. Drop both content sentinels from the OK stdout and expect success.
func TestThaw_LegacySnapshotWithoutNetnsContentSentinels(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	stdout := thawOKStdout
	stdout = strings.Replace(stdout, "FT_NETNS_IFACES=1\n", "", 1)
	stdout = strings.Replace(stdout, "FT_NETNS_DEFROUTE=yes\n", "", 1)
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	if _, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"}); err != nil {
		t.Fatalf("legacy snapshot without netns-content sentinels should still thaw, got %v", err)
	}
}

// TestThawScript_PinsNetnsAndAssertsContent proves the rendered thaw script
// carries both fix-B (netns pin) and fix-A (netns-content assertion) machinery
// (ka-0000007713): the pin bind-mounts Pod B's CNI netns to a controller-owned
// path so it survives the placeholder-graft sandbox delete + kubelet CNI DEL, and
// the content assertion enters the restored sentry's netns to require a
// non-loopback interface + default route before declaring restore verified.
func TestThawScript_PinsNetnsAndAssertsContent(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	if _, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"}); err != nil {
		t.Fatalf("Thaw: %v", err)
	}
	script := exec.scripts[0]
	// Fix B: pin Pod B's CNI netns by bind-mount before the teardown delete.
	for _, want := range []string{
		"mount --bind",     // pin the netns
		"PINNED_NETNS=",    // controller-owned pin path
		"FT_NETNS_PINNED=", // pin-result sentinel
	} {
		if !strings.Contains(script, want) {
			t.Errorf("thaw script missing fix-B netns-pin fragment %q", want)
		}
	}
	// Fix A: enter the restored sentry's netns and assert content before verified.
	for _, want := range []string{
		"/proc/$RPID/ns/net", // inspect the restored sentry's own netns
		"/proc/net/dev",      // non-loopback interface count
		"/proc/net/route",    // default route
		"FT_NETNS_IFACES=",
		"FT_NETNS_DEFROUTE=",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("thaw script missing fix-A netns-content fragment %q", want)
		}
	}
	// Fix C (ka-0000001156, RESEQUENCE): a CNI-ADD completion gate that polls Pod
	// B's live netns for a global IPv4 + default route (or a committed CNI result)
	// before pinning, so the pin captures a FULLY-configured routable netns rather
	// than the link-only (eth0-but-no-IP/route) netns canary xq3 observed.
	for _, want := range []string{
		"cni_add_complete",      // the IP+route confirmation helper
		"cni_result_cached",     // the /var/lib/cni/results fallback confirmation
		"/var/lib/cni/results/", // cached-result probe path
		"scope global",          // CNI IPAM assigned a global IPv4
		"FT_CNI_CONFIRMED=",     // confirmation sentinel
	} {
		if !strings.Contains(script, want) {
			t.Errorf("thaw script missing fix-C CNI-ADD-completion fragment %q", want)
		}
	}
	// The pin must happen BEFORE the teardown delete of Pod B's sandbox, or the
	// CNI netns is already gone by the time we bind-mount it.
	pinIdx := strings.Index(script, "mount --bind")
	delIdx := strings.Index(script, `RUNSC delete --force "$BSB"`)
	if pinIdx < 0 || delIdx < 0 || pinIdx > delIdx {
		t.Errorf("netns pin (idx=%d) must precede the Pod B sandbox teardown delete (idx=%d)", pinIdx, delIdx)
	}
	// Fix C ordering: the CNI-ADD completion gate must run BEFORE the pin (so the
	// bind-mount captures a fully-configured netns) and therefore also before the
	// teardown delete. The gate polls the kubelet-owned $BNETNS_PATH while it is
	// still the live CNI path — before the pin reassigns it to PINNED_NETNS.
	gateIdx := strings.Index(script, "FT_CNI_CONFIRMED=")
	if gateIdx < 0 || pinIdx < 0 || gateIdx > pinIdx {
		t.Errorf("CNI-ADD completion gate (idx=%d) must precede the netns pin (idx=%d)", gateIdx, pinIdx)
	}
	// Fix B (ka-0000001790): capture the confirmed L3 (IPv4 addrs + route table) at
	// the gate, then statically re-assert it into the pinned netns after the
	// force-delete's CNI DEL strips it — so the restore lands a routable netns.
	for _, want := range []string{
		"CAP_ADDRS",               // captured global IPv4 addrs
		"CAP_ROUTES",              // captured IPv4 route table
		"reassert_l3",             // the static re-apply helper
		"ip addr add",             // re-apply the addr
		"ip route add",            // re-apply the routes
		"FT_NETNS_L3_REASSERTED=", // re-assert-engaged sentinel
	} {
		if !strings.Contains(script, want) {
			t.Errorf("thaw script missing fix-B L3-reassert fragment %q", want)
		}
	}
	// The L3 must be CAPTURED while the CNI ADD output is still live — at the gate,
	// before the teardown delete fires the CNI DEL that strips it.
	capIdx := strings.Index(script, "CAP_ADDRS=$(HMNT")
	if capIdx < 0 || capIdx > delIdx {
		t.Errorf("L3 capture (idx=%d) must precede the Pod B sandbox teardown delete (idx=%d)", capIdx, delIdx)
	}
	// And the pre-restore re-assert must run AFTER the delete (re-applying what the
	// CNI DEL just stripped) but BEFORE the barrier restore reads the netns.
	reassertIdx := strings.Index(script, `reassert_l3 "$BNETNS_PATH"`)
	restoreIdx := strings.Index(script, "restore --image-path")
	if reassertIdx < 0 || reassertIdx < delIdx || (restoreIdx >= 0 && reassertIdx > restoreIdx) {
		t.Errorf("pre-restore L3 re-assert (idx=%d) must run after the teardown delete (idx=%d) and before the barrier restore (idx=%d)", reassertIdx, delIdx, restoreIdx)
	}
	// Fix C (ka-0000001099): the two-inode split — pin+capture target the SANDBOX
	// netns but the AWS VPC CNI installs the pod IP into the PAUSE netns — means
	// Fix B's sandbox-netns L3 capture no-ops (l3-reasserted=no). Fix C reads the
	// pod IP from the AUTHORITATIVE committed CNI result (matched by K8S_POD_UID, not
	// a cid/netns), synthesizes a link-scope gateway + default route, and applies it
	// into the RESTORED SENTRY'S OWN netns (/proc/<RPID>/ns/net) — the restore-target
	// inode — decoupling the fix from netns-inode resolution entirely.
	for _, want := range []string{
		"read_cni_pod_ip",       // authoritative committed-CNI-result IP reader
		"K8S_POD_UID",           // match Pod B's CNI result by pod UID, not a cid
		"/var/lib/cni/results/", // the committed CNI result dir
		"CNI_POD_IP",            // the pod IP captured from the CNI result
		"169.254.1.1",           // synthetic AWS VPC CNI link-scope gateway
		"self_ipam",             // the static self-IPAM helper
		"default via",           // default route via the synthetic gateway
		"FT_CNI_POD_IP=",        // pod-IP sentinel
		"FT_NETNS_SELF_IPAM=",   // self-IPAM-engaged sentinel
	} {
		if !strings.Contains(script, want) {
			t.Errorf("thaw script missing fix-C self-IPAM fragment %q", want)
		}
	}
	// Fix C must apply self-IPAM into the restored sentry's OWN netns
	// (/proc/$RPID/ns/net) AFTER the barrier restore (the netns only exists once the
	// sentry is restored) and the call target must be that proc-net path, not a
	// sandbox/pause cid or netns path.
	selfIpamCallIdx := strings.Index(script, `self_ipam "/proc/$RPID/ns/net"`)
	if selfIpamCallIdx < 0 {
		t.Error("fix-C self_ipam must be invoked against the restored sentry's OWN netns /proc/$RPID/ns/net")
	}
	if restoreIdx >= 0 && selfIpamCallIdx >= 0 && selfIpamCallIdx < restoreIdx {
		t.Errorf("self-IPAM (idx=%d) must run AFTER the barrier restore (idx=%d) — the restore-target netns does not exist until the sentry is restored", selfIpamCallIdx, restoreIdx)
	}
	// Fix D (ka-0000004088): gVisor's runsc restore SCRAPES the live netns into a fresh
	// sentry netstack DURING the restore RPC (setupNetwork runs before ContMgrRestore —
	// runsc/sandbox/sandbox.go) and discards the checkpoint's netstack. So the
	// load-bearing self_ipam must ALSO run PRE-restore, into the SAME pinned restore-
	// target netns ($BNETNS_PATH) the spec's network-namespace path points at, so the
	// scrape lands a routable netns in the sentry. The post-restore self_ipam alone only
	// fixes the host-kernel netns view (passes [6a]) but never reaches the already-built
	// sentry netstack — the rehydrate-ack frontier (coop NATS + gb DNS network-unreachable
	// for the full 180s deadline). The pre-restore self_ipam must run after the teardown
	// delete (so it re-applies what the CNI DEL stripped) and before the barrier restore.
	preSelfIpamIdx := strings.Index(script, `self_ipam "$BNETNS_PATH"`)
	if preSelfIpamIdx < 0 {
		t.Error("fix-D self_ipam must ALSO be invoked PRE-restore against the pinned restore-target netns $BNETNS_PATH — gVisor scrapes the netns into the sentry netstack during the restore RPC, so a post-restore-only apply never reaches the sentry")
	}
	if preSelfIpamIdx >= 0 && (preSelfIpamIdx < delIdx || (restoreIdx >= 0 && preSelfIpamIdx > restoreIdx)) {
		t.Errorf("fix-D pre-restore self-IPAM (idx=%d) must run after the teardown delete (idx=%d) and before the barrier restore (idx=%d)", preSelfIpamIdx, delIdx, restoreIdx)
	}
}

// TestThaw_RefusesMultiContainerSnapshot proves the C6 topology guard
// (ka-0000007582): a snapshot whose ContainerCount > 2 captured always-on
// sidecars the controller barrier restore cannot reconstruct (it fires
// sandbox+agent only). Thaw must refuse it so the caller falls back to cold
// start rather than hanging the restore at an unsatisfiable barrier. The node
// executor is given a panicking exec to prove Thaw bails BEFORE any node op.
func TestThaw_RefusesMultiContainerSnapshot(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	exec := &fakeNodeExecutor{err: context.Canceled} // must never be reached
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 4,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	rehydr := &recordingRehydrator{}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err == nil || !strings.Contains(err.Error(), "container_count=4") {
		t.Fatalf("want multi-container refusal, got %v", err)
	}
	if len(rehydr.reqs) != 0 {
		t.Error("rehydration must not fire when the snapshot is refused")
	}
	// Pod B must never be created for a refused snapshot.
	if pods, _ := client.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{}); len(pods.Items) != 0 {
		t.Errorf("no Pod B should be created for a refused snapshot, got %d pods", len(pods.Items))
	}
}

func TestThaw_SubContainerBarrierFails(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	// Sub-container restore failed (barrier not satisfied).
	stdout := strings.Replace(thawOKStdout, "FT_SUB_RC=0", "FT_SUB_RC=1", 1)
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout, Stderr: "reading mounts file: unexpected end of JSON input"}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	rehydr := &recordingRehydrator{}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err == nil || !strings.Contains(err.Error(), "sub-container restore rc=1") {
		t.Fatalf("want sub-container barrier error, got %v", err)
	}
	// The failure is surfaced as a typed *ThawBarrierError carrying the rcs, and
	// a sub-only failure (SubRC>0, RootRC==0) is the retryable /sys-walk class
	// (ka-0000005365 step 4).
	var be *ThawBarrierError
	if !errors.As(err, &be) {
		t.Fatalf("want a *ThawBarrierError, got %T: %v", err, err)
	}
	if be.RootRC != 0 || be.SubRC != 1 {
		t.Errorf("barrier error rcs: got RootRC=%d SubRC=%d, want RootRC=0 SubRC=1", be.RootRC, be.SubRC)
	}
	if !be.Retryable() {
		t.Error("a sub-only barrier failure (RootRC=0, SubRC!=0) must be Retryable")
	}
	if len(rehydr.reqs) != 0 {
		t.Error("rehydration must not fire on a failed restore")
	}
	// The create-path Pod B must be torn down on the failed restore, not leaked
	// (ka-0000005365): otherwise the reconciler's canonical-key collision between
	// "agentsim-thaw-b" and the cold-start "agentsim" can leave the Failed Pod B
	// undeleted.
	if pods, _ := client.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{}); len(pods.Items) != 0 {
		var names []string
		for _, p := range pods.Items {
			names = append(names, p.Name)
		}
		t.Errorf("create-path Pod B must be torn down after a failed restore, got surviving pods %v", names)
	}
}

// TestThaw_RootBarrierFailIsNotRetryable: a root-sandbox restore failure
// (RootRC!=0) is the deterministic netns/lo-only fail-closed path, NOT the
// transient /sys-walk class — it must surface as a non-retryable
// *ThawBarrierError, and the create-path Pod B is still torn down (ka-0000005365).
func TestThaw_RootBarrierFailIsNotRetryable(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	// Root sandbox restore failed (FT_ROOT_RC=128) — the lo-only/netns fail-closed
	// signature. The sub rc is irrelevant once root fails.
	stdout := strings.Replace(thawOKStdout, "FT_ROOT_RC=0", "FT_ROOT_RC=128", 1)
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout, Stderr: "lo-only netns"}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	var be *ThawBarrierError
	if !errors.As(err, &be) {
		t.Fatalf("want a *ThawBarrierError, got %T: %v", err, err)
	}
	if be.RootRC != 128 {
		t.Errorf("barrier error RootRC: got %d, want 128", be.RootRC)
	}
	if be.Retryable() {
		t.Error("a root-sandbox barrier failure (RootRC!=0) must NOT be Retryable")
	}
	if pods, _ := client.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{}); len(pods.Items) != 0 {
		t.Errorf("create-path Pod B must be torn down after a failed root restore, got %d surviving pods", len(pods.Items))
	}
}

// TestThaw_GraftBarrierFailDoesNotTearDownSuppliedPod: when the barrier restore
// fails on a SUPPLIED warm placeholder (req.PlaceholderPod set), Thaw must NOT
// delete it — the warm-B placeholder is owned by the caller
// (freezeThawLifecycle.DeletePromoted handles its teardown). Deleting it here
// would double-delete and race the caller's cleanup (ka-0000005365).
func TestThaw_GraftBarrierFailDoesNotTearDownSuppliedPod(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(readyPlaceholderPodB("warm-b-7", ns, node, "u7"))
	stdout := strings.Replace(thawOKStdout, "FT_SUB_RC=0", "FT_SUB_RC=1", 1)
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout, Stderr: "sub failed"}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim", PlaceholderPod: "warm-b-7"})
	if err == nil {
		t.Fatal("want a barrier error on the supplied placeholder")
	}
	// The supplied warm placeholder must SURVIVE — the caller owns its teardown.
	if _, gerr := client.CoreV1().Pods(ns).Get(context.Background(), "warm-b-7", metav1.GetOptions{}); gerr != nil {
		t.Errorf("supplied warm placeholder must NOT be torn down by Thaw (caller owns it), but it was deleted: %v", gerr)
	}
}

func TestThaw_SkipRehydration(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	rehydr := &recordingRehydrator{}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim", SkipRehydration: true})
	if err != nil {
		t.Fatalf("Thaw: %v", err)
	}
	if res.Rehydrated {
		t.Error("Rehydrated should be false when skipped")
	}
	if len(rehydr.reqs) != 0 {
		t.Error("rehydration must not fire when SkipRehydration is set")
	}
}

func TestThaw_RehydrationFailureReturnsResultAndError(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	rehydr := &recordingRehydrator{err: context.DeadlineExceeded}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err == nil || !strings.Contains(err.Error(), "rehydration failed") {
		t.Fatalf("want rehydration-failed error, got %v", err)
	}
	// The restore is verified; the result is still returned so the caller can
	// retry rehydration against the live Pod B.
	if res == nil || res.RestoredPID != "37737" {
		t.Errorf("result should be returned despite rehydration failure: %+v", res)
	}
	if res.Rehydrated {
		t.Error("Rehydrated should be false on failure")
	}
}

// thawThenReapExec serves the thaw script with thawOKStdout and the post-thaw
// reaper script with reapStdout, branching on the FT_IMAGE_REAPED signature the
// reaper template emits. It records the image path the reaper was asked to rm.
func thawThenReapExec(reapStdout string, reapErr error) *fakeNodeExecutor {
	f := &fakeNodeExecutor{}
	f.resultFn = func(_, script string) (NodeExecResult, error) {
		if strings.Contains(script, "FT_IMAGE_REAPED") {
			return NodeExecResult{Stdout: reapStdout}, reapErr
		}
		return NodeExecResult{Stdout: thawOKStdout}, nil
	}
	return f
}

// TestThaw_ReapsImageAfterVerifiedThaw is the ka-0000006968 guard: a successful
// thaw must reclaim the 3.0G checkpoint image (relocated outside any GC by
// ka-0000003702, so nothing else ever deletes it) exactly once the restore is
// verified, and report it via ThawResult.ImageReaped. The reaper rm must target
// the snapshot's ImagePath, host-namespaced, and must NOT touch the bundle
// (which backs the live Pod B).
func TestThaw_ReapsImageAfterVerifiedThaw(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := thawThenReapExec("FT_IMAGE_REAPED=yes\n", nil)
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		ImagePath:  "/var/lib/gardenio/ckpt/agentsim",
		BundlePath: "/var/lib/gardenio/bundle/agentsim",
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err != nil {
		t.Fatalf("Thaw: %v", err)
	}
	if !res.ImageReaped {
		t.Error("ImageReaped should be true after a verified thaw")
	}
	// Exactly two node execs: the barrier restore, then the reaper.
	if len(exec.scripts) != 2 {
		t.Fatalf("node execs = %d, want 2 (thaw + reap)", len(exec.scripts))
	}
	reap := exec.scripts[1]
	if !strings.Contains(reap, `HMNT rm -rf "$IMG"`) {
		t.Error("reaper must rm the image host-namespaced (HMNT)")
	}
	if !strings.Contains(reap, "IMG='/var/lib/gardenio/ckpt/agentsim'") {
		t.Error("reaper must target the snapshot's ImagePath")
	}
	// The bundle is the live Pod B's rootfs/work backing — the reaper must never
	// touch it.
	if strings.Contains(reap, "/var/lib/gardenio/bundle/agentsim") || strings.Contains(reap, "BUNDLE") {
		t.Error("reaper must NOT touch the bundle (it backs the live Pod B)")
	}
}

// TestThaw_ReapFailureDoesNotFailThaw verifies the reaper is best-effort: a
// failed reclaim (exec error or unconfirmed sentinel) leaves the image leaked
// and ImageReaped=false, but the already-succeeded thaw still returns success.
func TestThaw_ReapFailureDoesNotFailThaw(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	for _, tc := range []struct {
		name       string
		reapStdout string
		reapErr    error
	}{
		{"exec error", "", context.DeadlineExceeded},
		{"unconfirmed", "FT_IMAGE_REAPED=no\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := fake.NewClientset()
			client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
			exec := thawThenReapExec(tc.reapStdout, tc.reapErr)
			store := NewInMemorySnapshotStore()
			_ = store.Put(context.Background(), Snapshot{
				AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
				ImagePath:  "/var/lib/gardenio/ckpt/agentsim",
				SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
				PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
			})
			m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

			res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
			if err != nil {
				t.Fatalf("Thaw must succeed despite reap failure: %v", err)
			}
			if res.ImageReaped {
				t.Error("ImageReaped should be false when the reclaim did not land")
			}
		})
	}
}

// TestThaw_SkipRehydrationDoesNotReap guards that the SkipRehydration caller —
// which drives its own post-restore lifecycle and may retry the thaw from the
// snapshot — keeps the image intact (reaping it would strand the snapshot
// un-thawable). No reaper exec must fire.
func TestThaw_SkipRehydrationDoesNotReap(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := thawThenReapExec("FT_IMAGE_REAPED=yes\n", nil)
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		ImagePath:  "/var/lib/gardenio/ckpt/agentsim",
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim", SkipRehydration: true})
	if err != nil {
		t.Fatalf("Thaw: %v", err)
	}
	if res.ImageReaped {
		t.Error("ImageReaped must be false when SkipRehydration is set")
	}
	if len(exec.scripts) != 1 {
		t.Errorf("node execs = %d, want 1 (thaw only; no reaper)", len(exec.scripts))
	}
}

// TestThaw_RehydrationFailureDoesNotReap guards the partial-success path: when
// rehydration fail-closes, the caller (main.go) tears down Pod B and cold-starts
// from the still-intact snapshot — so the image must survive. Thaw must NOT reap
// it on the rehydration-failure return.
func TestThaw_RehydrationFailureDoesNotReap(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := thawThenReapExec("FT_IMAGE_REAPED=yes\n", nil)
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		ImagePath:  "/var/lib/gardenio/ckpt/agentsim",
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	rehydr := &recordingRehydrator{err: context.DeadlineExceeded}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err == nil || !strings.Contains(err.Error(), "rehydration failed") {
		t.Fatalf("want rehydration-failed error, got %v", err)
	}
	if res != nil && res.ImageReaped {
		t.Error("image must NOT be reaped when rehydration failed (snapshot needed for cold-start retry)")
	}
	if len(exec.scripts) != 1 {
		t.Errorf("node execs = %d, want 1 (thaw only; reaper must not fire on rehydration failure)", len(exec.scripts))
	}
}

func TestThaw_MissingSnapshot(t *testing.T) {
	client := fake.NewClientset()
	m := newFreezeThawManager(t, client, &fakeNodeExecutor{}, NewInMemorySnapshotStore(), nil)
	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "ghost"})
	if err == nil || !strings.Contains(err.Error(), "load snapshot") {
		t.Fatalf("want load-snapshot error, got %v", err)
	}
}

func TestThaw_CustomPlaceholderName(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim", PlaceholderName: "my-b"})
	if err != nil {
		t.Fatalf("Thaw: %v", err)
	}
	if res.PlaceholderPod != "my-b" {
		t.Errorf("placeholder = %q, want my-b", res.PlaceholderPod)
	}
	if _, err := client.CoreV1().Pods(ns).Get(context.Background(), "my-b", metav1.GetOptions{}); err != nil {
		t.Errorf("custom-named Pod B not created: %v", err)
	}
}

// readyPlaceholderPodB returns a Ready warm-B pool placeholder pod, pre-created
// on `node` as if poolmanager had already provisioned it ahead of the thaw.
func readyPlaceholderPodB(name, ns, node, uid string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid)},
		Spec:       corev1.PodSpec{NodeName: node},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
}

// TestThaw_GraftOntoPreReadyPlaceholder is the C1 hot path
// (sub-second-resume-integration.md): a Ready warm-B pool pod is supplied via
// ThawRequest.PlaceholderPod, and Thaw must graft the checkpoint onto it
// WITHOUT creating a new pod or waiting for Ready — the dominant multi-second
// kubelet pod-admission cost is skipped.
func TestThaw_GraftOntoPreReadyPlaceholder(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(readyPlaceholderPodB("warm-b-7", ns, node, "warm-uid-7"))
	// Fail loudly if Thaw tries to CREATE a pod on the graft path.
	client.PrependReactor("create", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, errCreateForbiddenOnGraft
	})
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a-uid-123", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	rehydr := &recordingRehydrator{}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim", PlaceholderPod: "warm-b-7"})
	if err != nil {
		t.Fatalf("Thaw graft: %v", err)
	}
	if res.PlaceholderPod != "warm-b-7" {
		t.Errorf("placeholder = %q, want warm-b-7", res.PlaceholderPod)
	}
	// B's UID came from the supplied pod, not a freshly-created one.
	if res.BPodUID != "warm-uid-7" {
		t.Errorf("BPodUID = %q, want warm-uid-7 (resolved from supplied pod)", res.BPodUID)
	}
	// The barrier-restore script still ran against the grafted pod.
	if len(exec.scripts) != 1 {
		t.Fatalf("node exec calls = %d, want 1 (barrier restore on graft)", len(exec.scripts))
	}
	if !strings.Contains(exec.scripts[0], "warm-b-7") {
		t.Errorf("thaw script did not target the grafted placeholder warm-b-7")
	}
	// Rehydration carried the supplied pod's UID, not Pod A's.
	if len(rehydr.reqs) != 1 || rehydr.reqs[0].PodUID != "warm-uid-7" {
		t.Errorf("rehydration req = %+v, want PodUID warm-uid-7", rehydr.reqs)
	}
	if rehydr.reqs[0].PodName != "warm-b-7" {
		t.Errorf("rehydration PodName = %q, want warm-b-7", rehydr.reqs[0].PodName)
	}
}

var errCreateForbiddenOnGraft = errCreateForbidden{}

type errCreateForbidden struct{}

func (errCreateForbidden) Error() string {
	return "Thaw should not CREATE a pod when PlaceholderPod is set (graft hot path)"
}

// TestThaw_GraftMissingPlaceholder: a supplied PlaceholderPod that does not
// exist must fail loudly (the pool handed out a stale/absent name) rather than
// silently falling back to create.
func TestThaw_GraftMissingPlaceholder(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim", PlaceholderPod: "ghost-b"})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("want not-found error for missing placeholder, got %v", err)
	}
	if len(exec.scripts) != 0 {
		t.Errorf("barrier restore should not run against a missing placeholder")
	}
}

// TestThaw_GraftNotReadyPlaceholder: a supplied PlaceholderPod that exists but
// is not Ready must be rejected — the pool's contract is to hand out only Ready
// pods, and grafting onto a not-yet-admitted pod reintroduces the very race C1
// removes.
func TestThaw_GraftNotReadyPlaceholder(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	notReady := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "warm-b-9", Namespace: ns, UID: types.UID("u9")},
		Spec:       corev1.PodSpec{NodeName: node},
		Status:     corev1.PodStatus{Phase: corev1.PodPending},
	}
	client := fake.NewClientset(notReady)
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim", PlaceholderPod: "warm-b-9"})
	if err == nil || !strings.Contains(err.Error(), "not Ready") {
		t.Fatalf("want not-Ready rejection, got %v", err)
	}
	if len(exec.scripts) != 0 {
		t.Errorf("barrier restore should not run against a not-Ready placeholder")
	}
}

// TestThaw_GraftCrossNodePlaceholder: the checkpoint image is node-local, so a
// placeholder on a different node than the snapshot cannot serve the graft and
// must be rejected before any node exec.
func TestThaw_GraftCrossNodePlaceholder(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset(readyPlaceholderPodB("warm-b-x", ns, "node-2", "ux"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: "node-1",
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, "node-1").Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim", PlaceholderPod: "warm-b-x"})
	if err == nil || !strings.Contains(err.Error(), "node-local") {
		t.Fatalf("want cross-node rejection, got %v", err)
	}
	if len(exec.scripts) != 0 {
		t.Errorf("barrier restore should not run for a cross-node placeholder")
	}
}

func TestParseSentinels(t *testing.T) {
	in := "noise\nFT_A=1\n  FT_B = hello world \nnot_a_sentinel=x\nFT_A=2\nFT_EMPTY=\n"
	got := parseSentinels(in)
	if got["FT_A"] != "2" {
		t.Errorf("FT_A = %q, want 2 (last wins)", got["FT_A"])
	}
	if got["FT_B"] != "hello world" {
		t.Errorf("FT_B = %q, want trimmed value", got["FT_B"])
	}
	if _, ok := got["not_a_sentinel"]; ok {
		t.Error("non-FT line should be ignored")
	}
	if v, ok := got["FT_EMPTY"]; !ok || v != "" {
		t.Errorf("FT_EMPTY should be present and empty, got %q ok=%v", v, ok)
	}
}

func TestIntField(t *testing.T) {
	fields := map[string]string{"n": "42", "bad": "x", "empty": ""}
	if n, ok := intField(fields, "n"); !ok || n != 42 {
		t.Errorf("n = %d ok=%v", n, ok)
	}
	if _, ok := intField(fields, "bad"); ok {
		t.Error("non-numeric should not parse")
	}
	if _, ok := intField(fields, "empty"); ok {
		t.Error("empty should not parse")
	}
	if _, ok := intField(fields, "absent"); ok {
		t.Error("absent should not parse")
	}
}

func TestSanitizeIdent(t *testing.T) {
	cases := map[string]string{
		"gardenio-crew-abc": "gardenio-crew-abc",
		"agent/with:slash":  "agent_with_slash",
		"":                  "agent",
		"a.b_c-1":           "a_b_c-1",
	}
	for in, want := range cases {
		if got := sanitizeIdent(in); got != want {
			t.Errorf("sanitizeIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInMemorySnapshotStore(t *testing.T) {
	s := NewInMemorySnapshotStore()
	if _, err := s.Get(context.Background(), "x"); err == nil {
		t.Error("Get on empty store should error")
	}
	if err := s.Put(context.Background(), Snapshot{}); err == nil {
		t.Error("Put with empty agent name should error")
	}
	if err := s.Put(context.Background(), Snapshot{AgentName: "x", SandboxCID: "cid"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(context.Background(), "x")
	if err != nil || got.SandboxCID != "cid" {
		t.Errorf("Get = %+v err=%v", got, err)
	}
}

func TestHasSnapshot(t *testing.T) {
	store := NewInMemorySnapshotStore()
	m := NewFreezeThawManager(nil, nil, store, nil, testLogger())

	if m.HasSnapshot(context.Background(), "") {
		t.Error("HasSnapshot(\"\") should be false")
	}
	if m.HasSnapshot(context.Background(), "absent") {
		t.Error("HasSnapshot for an unknown agent should be false")
	}
	if err := store.Put(context.Background(), Snapshot{AgentName: "present", SandboxCID: "cid"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !m.HasSnapshot(context.Background(), "present") {
		t.Error("HasSnapshot for a stored agent should be true")
	}
}

// TestThawScript_BundleOpsRunInHostMountNS is the thaw-side ka-0000002536
// regression guard. After freeze writes the OCI bundle in the HOST mount ns
// (HMNT), thaw must READ/REWRITE/overlay-prep that same host-ns bundle — the
// $OVER upper/work dirs, the .overlay-opts read, and both config.json rewrites.
// Running them in the probe ns (chroot /host) would prep + rewrite a different
// filesystem than the one the HMNT overlay mount and runsc restore consume,
// reintroducing the rc=128 FetchSpec defect.
func TestThawScript_BundleOpsRunInHostMountNS(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
	})
	for _, want := range []string{
		`HMNT rm -rf "$OVER"`,                          // overlay scratch root in host ns
		`HMNT mkdir -p "$up" "$wk"`,                    // fresh upper/work in host ns
		`HMNT cat "$BUNDLE/$dstn/.overlay-opts"`,       // opts read in host ns
		`HMNT python3 - "$BUNDLE/$dstn/config.json"`,   // per-sub-container rewrite in host ns
		`HMNT python3 - "$BUNDLE/sandbox/config.json"`, // sandbox rewrite in host ns
	} {
		if !strings.Contains(tw, want) {
			t.Errorf("thaw bundle op not host-namespaced: missing %q", want)
		}
	}
	// The OLD probe-ns forms (no HMNT prefix) are the prod defect — must be gone.
	for _, banned := range []string{
		"\nrm -rf \"$OVER\"\n",                        // unwrapped overlay scratch
		"\n  mkdir -p \"$up\" \"$wk\"\n",              // unwrapped upper/work
		"\npython3 - \"$BUNDLE/$dstn/config.json\"",   // unwrapped per-sub rewrite
		"\npython3 - \"$BUNDLE/sandbox/config.json\"", // unwrapped sandbox rewrite
		"$(cat \"$BUNDLE/$dstn/.overlay-opts\"",       // unwrapped opts read
	} {
		if strings.Contains(tw, banned) {
			t.Errorf("thaw script still runs a bundle op in the probe mount ns (prod rc=128 defect): %q", banned)
		}
	}
}

// TestThawScript_FailsLoudOnMissingLowerdirOrMount is the ka-0000000655 fix-C
// regression guard. The PROD defect was a SILENT overlay mount failure: a GC'd
// agent lowerdir made `mount -t overlay` ENOENT ("special device overlay does not
// exist"), but the rc was swallowed by the pipe to sed, so the rootfs was left
// unmounted and the failure surfaced 3 steps later as the OPAQUE runsc
// "reading mounts file: unexpected end of JSON input" sub rc=128. Thaw must
// instead (1) assert each lowerdir exists in the host ns BEFORE mounting, (2)
// capture the mount rc directly (not behind a pipe), and (3) fail LOUD with
// FT_ROOT_RC=128 + exit on either, naming the real cause.
func TestThawScript_FailsLoudOnMissingLowerdirOrMount(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
	})
	for _, want := range []string{
		`for d in $lowers; do`,    // iterate the (possibly :-joined) lowerdir set
		`HMNT test -d "$d"`,       // assert each lowerdir exists in the host ns
		`2> "$OVER/$dstn.mnterr"`, // capture mount stderr to a file, not a masking pipe
		`echo "FT_ROOT_RC=128"`,   // fail-loud sentinel the Go side keys on
	} {
		if !strings.Contains(tw, want) {
			t.Errorf("thaw script missing fail-loud overlay guard %q", want)
		}
	}
	// The OLD silent form piped mount straight into sed, masking the mount rc with
	// sed's exit status — that is the prod defect and must be gone.
	if strings.Contains(tw, `-o "lowerdir=$lowers,upperdir=$up,workdir=$wk,index=off" 2>&1 | sed`) {
		t.Error("thaw still pipes mount -t overlay into sed (masks the mount rc — prod rc=128 defect)")
	}
	// The lowerdir assertion + mount must run BEFORE the barrier restore, so a
	// missing source fails by name rather than as an empty-mounts JSON error.
	iGuard := strings.Index(tw, `HMNT test -d "$d"`)
	iRestore := strings.Index(tw, `RUNSC $OVERLAY_FLAG restore --image-path="$IMG" --bundle "$BUNDLE/sandbox"`)
	if iGuard < 0 || iRestore < 0 || iGuard > iRestore {
		t.Errorf("lowerdir guard must precede the barrier restore (guard=%d restore=%d)", iGuard, iRestore)
	}
}

// TestThawScript_RepointsNetnsAndSandboxState is the ka-0000004146 regression
// guard for the two thaw rc=128 vectors: (a) the root sandbox restore opening
// Pod A's torn-down CNI netns, and (b) the agent SUB restore loading Pod A's
// GC'd sandbox .state because its io.kubernetes.cri.sandbox-id annotation still
// named sandbox A. The thaw script must (1) resolve Pod B's netns fail-closed
// (bail FT_ROOT_RC=128 if it cannot), (2) express B's netns as a net:[inode]
// for the proof, (3) rewrite CRI annotations A->B in BOTH bundles, and
// (4) repoint the sandbox netns unconditionally on the resolved path.
func TestThawScript_RepointsNetnsAndSandboxState(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
	})
	for _, want := range []string{
		// fix-a: fail-closed netns resolution + inode emission for the proof.
		`BNETNS_PATH=$(read_netns_path "$cand")`,
		`echo "FT_ROOT_RC=128"`, // bail when B netns cannot be resolved / repointed
		`BNETNS_INODE=$(HMNT stat -L -c 'net:[%i]' "$BNETNS_PATH"`,
		`echo "FT_B_NETNS=$BNETNS_INODE"`,
		`echo "FT_B_NETNS_PATH=$BNETNS_PATH"`,
		// fix-a: sandbox netns repointed on the resolved PATH, not the inode.
		`HMNT python3 - "$BUNDLE/sandbox/config.json" "$ASB" "$BSB" "$APOD" "$BPODUID" "$BNETNS_PATH"`,
		// fix-b: CRI annotations rewritten A->B in both bundles so the sub joins
		// Pod B's sandbox (whose .state the root restore wrote), not A's GC'd one.
		`ann=c.get("annotations",{})`,
	} {
		if !strings.Contains(tw, want) {
			t.Errorf("thaw script missing ka-0000004146 netns/sandbox-state fix: %q", want)
		}
	}
	// The sandbox netns repoint must NOT be gated on a possibly-empty var the way
	// the prior code silently skipped it (the "and bnetns" short-circuit). After
	// the fix the repoint records that it happened and bails if there was no netns.
	if !strings.Contains(tw, "repointed=True") || !strings.Contains(tw, "if not repointed:") {
		t.Errorf("thaw sandbox netns repoint is not fail-closed (missing repointed guard)")
	}
}

// TestThawScript_RepointsBindSourcesByDestination is the ka-0000004815 regression
// guard. After the ka-0000000655 overlay fix, the agent SUB restore still rc=128'd
// with the OPAQUE "reading mounts file: unexpected end of JSON input" (root=0,
// sub=128). That error is runsc's INTERNAL --mounts-fd pipe closing EMPTY because
// the sub gofer dies in ResolveMounts walking a DANGLING bind-mount source. The
// prior code repointed kubelet-GC'd bind sources A->B by UID SUBSTRING only, which
// silently fails for sources with a per-pod-random component beyond the uid
// (/dev/termination-log, kube-api-access-<rand>). The fix repoints each sub mount
// SOURCE to Pod B's LIVE source by DESTINATION, then asserts every source resolves
// in the host ns BEFORE any runsc restore — failing loud by name instead of as the
// opaque empty-mounts error.
func TestThawScript_RepointsBindSourcesByDestination(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
	})
	for _, want := range []string{
		// resolve Pod B's LIVE bundle config so we can read its fresh sources.
		`BDIR=$(RUNSC state "$bc"`,
		`for cand in "$BDIR/config.json" "$T/$bc/config.json"; do`,
		// pass Pod B's live config to the per-sub rewrite as the 7th arg.
		`HMNT python3 - "$BUNDLE/$dstn/config.json" "$ASB" "$BSB" "$APOD" "$BPODUID" "$BUNDLE" "$BCFG"`,
		// build destination->source from Pod B and prefer it for the repoint.
		`bsrc[d]=m.get("source","")`,
		`elif d in bsrc and bsrc[d]:`,
		// /work still rides the preserved warm copy, not B's empty emptyDir.
		`m["source"]=bundle+"/.work"`,
		// /tmp rides the preserved gofer tree (ka-0000001085) carrying claude-1000,
		// not B's fresh empty /tmp EmptyDir whose 9p walk would ENOENT -> sub rc=128.
		`elif d=="/tmp":`,
		`m["source"]=bundle+"/.tmp"`,
		// data-driven gofer manifest (ka-0000006427): any OTHER preserved
		// runtime-populated 9p mount (e.g. /home/agent/.claude) repoints to its
		// warm copy. MUST precede the bsrc[d] branch — B's live source is the
		// fresh empty dir whose 9p walk would ENOENT. The warm dir is bound to
		// _warm then set as the source (ka-0000004956 refactor: the SA-token
		// overlay reads _warm), so the repoint is the two lines below.
		`_raw=json.load(open(_os.path.join(bundle,".gofer","manifest.json")))`,
		`elif d in gofer:`,
		`_warm=bundle+"/.gofer/"+gofer[d]`,
		`m["source"]=_warm`,
		// [3a] fail-closed: every absolute source must exist in the host ns.
		`if s.startswith("/") and not os.path.exists(s):`,
		`dangling bind-mount source`,
		`echo "FT_ROOT_RC=128"`,
		// on sub-restore failure, dump the gofer/boot log that names the dying walk.
		`for lf in "$BUNDLE/$cn/log.json" "$T/$bc/log.json"; do`,
	} {
		if !strings.Contains(tw, want) {
			t.Errorf("thaw script missing ka-0000004815 bind-source repoint/guard: %q", want)
		}
	}
	// The dangling-source guard must run BEFORE the barrier restore, so a missing
	// source fails by name rather than as the opaque empty-mounts JSON error.
	iGuard := strings.Index(tw, `dangling bind-mount source`)
	iRestore := strings.Index(tw, `RUNSC $OVERLAY_FLAG restore --image-path="$IMG" --bundle "$BUNDLE/sandbox"`)
	if iGuard < 0 || iRestore < 0 || iGuard > iRestore {
		t.Errorf("bind-source guard must precede the barrier restore (guard=%d restore=%d)", iGuard, iRestore)
	}
}

// TestThawScript_NormalizesGoferManifestVarRunKeys is the ka-0000007745 regression
// guard. After the overlay2-flag fix (ka-0000000621) made root restore rc=0, the
// agent SUB still rc=128'd with the empty --mounts-fd "reading mounts file:
// unexpected end of JSON input". Root cause: the freeze sweep (PYGOFER) keys the
// warm-copy manifest by the RAW OCI destination (e.g. the EKS projected SA token at
// /var/run/secrets/eks.amazonaws.com/serviceaccount), but the thaw repoint loop
// normalizes the mount destination d to /run/... BEFORE the `d in gofer` test — so
// a /var/run manifest key never matched, the warm copy was skipped, and the loop
// fell through to Pod B's fresh-empty source whose deep 9p walk ENOENTs. The fix
// normalizes the manifest keys the same /var/run<->/run way and keys BOTH forms, so
// the lookup hits regardless of which form was recorded at freeze.
func TestThawScript_NormalizesGoferManifestVarRunKeys(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
	})
	for _, want := range []string{
		// the raw manifest is loaded into _raw, not directly into the lookup map.
		`_raw=json.load(open(_os.path.join(bundle,".gofer","manifest.json")))`,
		// every raw key is kept AND its /var/run<->/run sibling is added, so the
		// normalized destination d below always finds its warm copy.
		`for _k,_v in _raw.items():`,
		`gofer[_k]=_v`,
		`if _k.startswith("/var/run/"): gofer["/run/"+_k[len("/var/run/"):]]=_v`,
		`elif _k.startswith("/run/"):   gofer["/var/run/"+_k[len("/run/"):]]=_v`,
	} {
		if !strings.Contains(tw, want) {
			t.Errorf("thaw script missing ka-0000007745 /var/run manifest-key normalization: %q", want)
		}
	}
	// The both-forms keying MUST run BEFORE the per-mount repoint loop that does the
	// `d in gofer` test — otherwise the normalized destination still misses.
	iNorm := strings.Index(tw, `for _k,_v in _raw.items():`)
	iLookup := strings.Index(tw, `elif d in gofer:`)
	if iNorm < 0 || iLookup < 0 || iNorm > iLookup {
		t.Errorf("gofer manifest-key normalization must precede the `d in gofer` repoint (norm=%d lookup=%d)", iNorm, iLookup)
	}
}

// TestThawScript_OverlaysPodBSATokenBytesOverWarmCopy is the ka-0000004956
// red-bar guard for the coop credential-gate PodAIdentity fail-close. The
// ka-0000006427 gofer sweep warm-copies the projected SA/IRSA token DIRS into
// $BUNDLE/.gofer/<n>, and the thaw repoint serves that warm copy as the mount
// source so the restore 9p walk finds the runtime subtree (no rc=128). But the
// warm token file carries the FROZEN Pod-A token whose kubernetes.io.pod.uid is
// Pod A's, and coop's freshness_gate HARD-rejects a mismatched pod_uid with
// FreshnessError::PodAIdentity at Sequence stage A — BEFORE the NATS/netstack
// reconnect stage — so coop is a "failed consumer" even when restore rc=0. The
// fix overlays Pod B's LIVE token bytes (bsrc[d]/token, realpath-resolved) onto
// the warm copy file coop re-reads, for the TWO SA token dirs ONLY (a blanket
// overlay would clobber the /home/agent/.claude warm trees -> ka-0000006427).
// We keep the warm DIR as the mount source, so this is purely a byte overlay.
//
// ka-0000008074 moved this warm-copy bind + overlay INTO the dedicated
// `elif d in _SA_TOKEN_DIRS:` forced-source branch (so it sits ABOVE the generic
// `elif d in gofer:` / `elif d in bsrc:` chain and the token dirs can never fall
// through to the ephemeral bsrc[d]). The overlay logic + bytes are unchanged; the
// guard that previously read `if d in _SA_TOKEN_DIRS and d in bsrc and bsrc[d]:`
// is now the inner `if d in bsrc and bsrc[d]:` of that branch (the allowlist is the
// branch condition itself). This guard pins the overlay steps + their ordering.
func TestThawScript_OverlaysPodBSATokenBytesOverWarmCopy(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
	})
	for _, want := range []string{
		// the allowlist is scoped to EXACTLY the two SA token destinations
		// (normalized /run/... form, since d is normalized before the lookup).
		`_SA_TOKEN_DIRS={"/run/secrets/eks.amazonaws.com/serviceaccount","/run/secrets/kubernetes.io/serviceaccount"}`,
		// the dedicated forced-source branch the token dirs now take (highest
		// precedence after the thaw-state force; above d-in-gofer / d-in-bsrc).
		`elif d in _SA_TOKEN_DIRS:`,
		// the warm dir REMAINS the mount source (no rc=128); only the bytes overlay.
		`_warm=bundle+"/.gofer/"+gofer[d]`,
		`m["source"]=_warm`,
		// inside the forced branch, the overlay is guarded by a live Pod-B source.
		`if d in bsrc and bsrc[d]:`,
		// Pod B's live token, realpath-resolved through the ..data/token layout.
		`_live=_os.path.realpath(_os.path.join(bsrc[d],"token"))`,
		`if _os.path.isfile(_live):`,
		`_tok=open(_live,"rb").read()`,
		// EMPTY-TOKEN GUARD (ka-0000005326 nit 2): a zero-length live read must NOT
		// truncate the warm token, so the overlay writes are guarded by `if _tok:`.
		`if _tok:`,
		// ATOMIC TOKEN WRITE (ka-0000005326 nit 1): the byte-overlay is all-or-nothing
		// via a temp-file + os.rename, not a direct truncating open().write().
		`_atomic_overlay(_os.path.join(_warm,"token"),_tok)`,
		// the kubelet atomic-writer ..data/token sibling is overlaid too, when real
		// (also gated on _tok so an empty read never truncates it).
		`_dd=_os.path.join(_warm,"..data","token")`,
		`if _tok and _os.path.isfile(_dd): _atomic_overlay(_dd,_tok)`,
		// best-effort/idempotent: a copy miss must NOT fail the freeze/thaw.
		`except Exception:`,
	} {
		if !strings.Contains(tw, want) {
			t.Errorf("thaw script missing ka-0000004956 SA-token overlay step: %q", want)
		}
	}
	// The overlay MUST come AFTER the SA-token forced-source warm-copy assignment (it
	// reads _warm) and BEFORE the PYAGENT json.dump that serializes the rewritten spec.
	// The _SA_TOKEN_DIRS forced branch must precede the generic `elif d in gofer:`.
	iForced := strings.Index(tw, `elif d in _SA_TOKEN_DIRS:`)
	iWarm := strings.Index(tw, `_warm=bundle+"/.gofer/"+gofer[d]`)
	iOverlay := strings.Index(tw, `_live=_os.path.realpath(_os.path.join(bsrc[d],"token"))`)
	iGoferBranch := strings.Index(tw, `elif d in gofer:`)
	iDump := strings.Index(tw, `json.dump(c,open(p,"w"))`)
	if iForced < 0 || iWarm < 0 || iOverlay < 0 || iDump < 0 || !(iForced < iWarm && iWarm < iOverlay && iOverlay < iDump) {
		t.Errorf("SA-token overlay must be ordered forced-branch < warm-source < overlay < json.dump (forced=%d warm=%d overlay=%d dump=%d)", iForced, iWarm, iOverlay, iDump)
	}
	if iGoferBranch < 0 || iForced > iGoferBranch {
		t.Errorf("the _SA_TOKEN_DIRS forced branch must precede the generic `elif d in gofer:` branch (forced=%d gofer=%d) so token dirs take the forced source, not the generic warm-copy fall-through", iForced, iGoferBranch)
	}
}

func TestBuildScripts_AreSafeTemplates(t *testing.T) {
	// The script builders must not panic and must interpolate the params.
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	fz := m.buildFreezeScript(freezeScriptParams{PodName: "mypod", RunscTimeout: 90})
	if !strings.Contains(fz, "SBNAME='mypod'") || !strings.Contains(fz, "RUNSC_TIMEOUT=90") {
		t.Errorf("freeze script did not interpolate params")
	}
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
	})
	for _, want := range []string{"BNAME='podb'", "ASB='asb'", "CCOUNT=2", "BUNDLE='/bndl'"} {
		if !strings.Contains(tw, want) {
			t.Errorf("thaw script missing interpolated %q", want)
		}
	}
}

// --- ka-0000005680: teardown-order barrier + Pod B failure diagnostics ------

// failedPodReactor mutates created pods into a terminal Failed phase with a
// container-level terminated status, reproducing the prod 2026.157.15 signature
// where the placeholder Pod B dies at the pod level (~2s) BEFORE the runsc
// restore is ever attempted.
func failedPodReactor(uid, termReason, termMessage string, exitCode int32) ktesting.ReactionFunc {
	return func(action ktesting.Action) (bool, runtime.Object, error) {
		ca, ok := action.(ktesting.CreateAction)
		if !ok {
			return false, nil, nil
		}
		pod, ok := ca.GetObject().(*corev1.Pod)
		if !ok {
			return false, nil, nil
		}
		pod.Status.Phase = corev1.PodFailed
		pod.Status.Reason = "PodFailed"
		pod.Status.Message = "pod admit-failed on node"
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name: ContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				Reason: termReason, Message: termMessage, ExitCode: exitCode,
			}},
		}}
		if pod.UID == "" {
			pod.UID = types.UID(uid)
		}
		return false, nil, nil
	}
}

// TestThaw_PodAStillPresentBlocksPodBCreation is the ka-0000005680 teardown-order
// invariant: if Pod A is still listed in the API (not yet fully torn down after
// the ka-0000003702 deferred teardown), Thaw must NOT create Pod B on the same
// node and must fail with a distinct "Pod A not fully torn down" error rather
// than racing into the "Pod B Failed before Ready" mode.
func TestThaw_PodAStillPresentBlocksPodBCreation(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	// Pod A is STILL present in the API (deferred teardown not yet complete).
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	m.PodReadyTimeout = 50 * time.Millisecond // Pod A never disappears here; bound the barrier wait.

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err == nil || !strings.Contains(err.Error(), "not fully torn down") {
		t.Fatalf("want teardown-barrier error, got %v", err)
	}
	// Pod B must NOT have been created while Pod A was still present.
	if _, gerr := client.CoreV1().Pods(ns).Get(context.Background(), "agentsim-thaw-b", metav1.GetOptions{}); gerr == nil {
		t.Error("Pod B was created before Pod A was torn down — barrier did not hold")
	}
	// The node script (restore) must never have run.
	if len(exec.scripts) != 0 {
		t.Errorf("node restore ran despite teardown barrier: %d scripts", len(exec.scripts))
	}
}

// TestThaw_PodAGoneProceedsToPodB confirms the barrier is a no-op on the normal
// path: with Pod A already gone (the lifecycle deleted it), Thaw proceeds to
// create Pod B and restore as usual.
func TestThaw_PodAGoneProceedsToPodB(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	// No Pod A in the API — teardown already complete.
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err != nil {
		t.Fatalf("Thaw should succeed when Pod A is gone: %v", err)
	}
	if res.PlaceholderPod != "agentsim-thaw-b" {
		t.Errorf("placeholder = %q", res.PlaceholderPod)
	}
}

// TestWaitPodGone_ReleasesWhenPodADeleted exercises the barrier directly: it
// blocks while Pod A is present and returns as soon as it is deleted.
func TestWaitPodGone_ReleasesWhenPodADeleted(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "node-1"))
	m := newFreezeThawManager(t, client, &fakeNodeExecutor{}, NewInMemorySnapshotStore(), &recordingRehydrator{})

	// Delete Pod A shortly after the barrier starts polling.
	go func() {
		time.Sleep(10 * time.Millisecond)
		_ = client.CoreV1().Pods(ns).Delete(context.Background(), "agentsim", metav1.DeleteOptions{})
	}()

	if err := m.waitPodGone(context.Background(), ns, "agentsim"); err != nil {
		t.Fatalf("waitPodGone should release once Pod A is deleted: %v", err)
	}
}

// TestWaitPodGone_EmptyNameIsNoOp confirms an absent Pod A name returns
// immediately (first-thaw path).
func TestWaitPodGone_EmptyNameIsNoOp(t *testing.T) {
	client := fake.NewClientset()
	m := newFreezeThawManager(t, client, &fakeNodeExecutor{}, NewInMemorySnapshotStore(), &recordingRehydrator{})
	if err := m.waitPodGone(context.Background(), "gvisor-poc", ""); err != nil {
		t.Fatalf("empty name should be a no-op, got %v", err)
	}
}

// TestThaw_PodBFailedBeforeReadyLogsDiagnostics is the ka-0000005680 diagnostic
// enrichment: when Pod B enters terminal phase Failed before Ready, the
// controller must capture and log Pod B's container-level terminated cause and
// recent events on the failure branch (so the next re-drive self-diagnoses
// without cluster-admin), then surface the not-Ready error.
func TestThaw_PodBFailedBeforeReadyLogsDiagnostics(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods",
		failedPodReactor("b-uid-456", "StartError", "failed to reserve sandbox name: name is reserved", 128))
	// Seed a Pod B event the diagnostics path should surface.
	_, _ = client.CoreV1().Events(ns).Create(context.Background(), &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "agentsim-thaw-b.evt1", Namespace: ns},
		InvolvedObject: corev1.ObjectReference{
			Kind: "Pod", Name: "agentsim-thaw-b", Namespace: ns, FieldPath: "spec.containers{agent}",
		},
		Type: "Warning", Reason: "FailedCreatePodSandBox",
		Message: "Failed to create pod sandbox: hostPath in use", Count: 1,
	}, metav1.CreateOptions{})

	var logbuf strings.Builder
	logger := slog.New(slog.NewTextHandler(&logbuf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := NewFreezeThawManager(client, exec, store, &recordingRehydrator{}, logger)
	m.PodReadyTimeout = 200 * time.Millisecond
	m.PodReadyPollInterval = time.Millisecond
	// Bypass the pre-thaw memory gate (ka-0000000088) so this test's assertion
	// "the barrier-restore script never ran" (len(exec.scripts)==0) is not
	// contaminated by the mem-gate probe exec, which fires BEFORE Pod B create.
	m.MemGateDisabled = true

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err == nil || !strings.Contains(err.Error(), "not Ready") {
		t.Fatalf("want not-Ready error, got %v", err)
	}
	logs := logbuf.String()
	for _, want := range []string{
		"failed before Ready",
		"StartError",
		"name is reserved",
		"exitCode=128",
		"FailedCreatePodSandBox",
		"hostPath in use",
	} {
		if !strings.Contains(logs, want) {
			t.Errorf("diagnostics log missing %q\n--- logs ---\n%s", want, logs)
		}
	}
	// The node restore must never have run — Pod B died upstream of it.
	if len(exec.scripts) != 0 {
		t.Errorf("node restore ran despite Pod B terminal failure: %d scripts", len(exec.scripts))
	}
}

func TestSnapshotNode(t *testing.T) {
	client := fake.NewClientset()
	exec := &fakeNodeExecutor{}
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	// No snapshot → miss.
	if _, ok := m.SnapshotNode(context.Background(), "agentsim"); ok {
		t.Error("SnapshotNode must miss when no snapshot exists")
	}
	// Empty agent name → miss.
	if _, ok := m.SnapshotNode(context.Background(), ""); ok {
		t.Error("SnapshotNode must miss on empty agent name")
	}
	// Snapshot with a node → hit.
	_ = store.Put(context.Background(), Snapshot{AgentName: "agentsim", Node: "node-7"})
	node, ok := m.SnapshotNode(context.Background(), "agentsim")
	if !ok || node != "node-7" {
		t.Errorf("SnapshotNode = (%q, %v), want (node-7, true)", node, ok)
	}
	// Snapshot with empty node → miss (a thaw cannot pin a warm placeholder).
	_ = store.Put(context.Background(), Snapshot{AgentName: "nonode", Node: ""})
	if _, ok := m.SnapshotNode(context.Background(), "nonode"); ok {
		t.Error("SnapshotNode must miss when the snapshot carries no node")
	}
}

// thawObservingRehydrator records whether IsThawing(agentName) was true at the
// moment the rehydration hand-off ran — proving the in-flight-thaw slot is held
// through the rehydration window (ka-0000002058).
type thawObservingRehydrator struct {
	mgr             *FreezeThawManager
	agentName       string
	thawingDuringRH bool
	err             error
}

func (r *thawObservingRehydrator) Trigger(_ context.Context, _ RehydrationRequest) error {
	r.thawingDuringRH = r.mgr.IsThawing(r.agentName)
	return r.err
}

// TestThaw_IsThawingHeldThroughRehydration asserts the registry the reconciler's
// terminal-pod reaper consults (IsThawing) is true for the entire Thaw call —
// crucially during the rehydration hand-off, the window in which Pod B flips to
// phase=Failed — and is cleared once Thaw returns (ka-0000002058, exempt_reaper).
func TestThaw_IsThawingHeldThroughRehydration(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", BeadID: "gd-AGENT", Namespace: ns, PodName: "agentsim",
		Node: node, ImagePath: "/var/tmp/gb-ckpt/agentsim", BundlePath: "/var/tmp/gb-bundle/agentsim",
		SandboxCID: "sandboxcid222", AgentCID: "agentcid111", PodUID: "a-uid-123", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, nil)
	rehydr := &thawObservingRehydrator{mgr: m, agentName: "agentsim"}
	m.rehydr = rehydr

	// Not thawing before the call.
	if m.IsThawing("agentsim") {
		t.Fatal("IsThawing must be false before Thaw is called")
	}

	if _, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"}); err != nil {
		t.Fatalf("Thaw: %v", err)
	}

	if !rehydr.thawingDuringRH {
		t.Error("IsThawing must be true during the rehydration hand-off (the Pod B phase=Failed window)")
	}
	// Slot released after the call so an abandoned Pod B can be reaped later.
	if m.IsThawing("agentsim") {
		t.Error("IsThawing must be false after Thaw returns")
	}
}

// TestIsThawing_NilAndEmpty guards the cheap defensive paths.
func TestIsThawing_NilAndEmpty(t *testing.T) {
	var nilMgr *FreezeThawManager
	if nilMgr.IsThawing("agentsim") {
		t.Error("IsThawing on a nil manager must be false")
	}
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	if m.IsThawing("") {
		t.Error("IsThawing on an empty agent name must be false")
	}
}

// TestRehydrateDeadlineEffective asserts the exported accessor used by the
// lifecycle startup log reports the EFFECTIVE deadline: the field when set,
// else DefaultRehydrateDeadline. A raw 0 field must surface as the default,
// not 0 (bug ka-0000007916 — the misleading "rehydrateDeadline:0" log line).
func TestRehydrateDeadlineEffective(t *testing.T) {
	unset := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	if got := unset.RehydrateDeadlineEffective(); got != DefaultRehydrateDeadline {
		t.Errorf("effective deadline with unset field = %v, want %v", got, DefaultRehydrateDeadline)
	}

	set := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	set.RehydrateDeadline = 180 * time.Second
	if got := set.RehydrateDeadlineEffective(); got != 180*time.Second {
		t.Errorf("effective deadline with set field = %v, want 180s", got)
	}
}

// TestCreateRestorePodReadyTimeoutEffective asserts the exported accessor reports
// the EFFECTIVE create-then-restore Pod B Ready-wait budget: the field when set,
// else DefaultCreateRestorePodReadyTimeout (ka-0000004449). The default must be
// strictly larger than DefaultPodReadyTimeout — the whole point of the seam is to
// give the cold-clone-bearing create path more room than the no-clone waits.
func TestCreateRestorePodReadyTimeoutEffective(t *testing.T) {
	unset := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	if got := unset.CreateRestorePodReadyTimeoutEffective(); got != DefaultCreateRestorePodReadyTimeout {
		t.Errorf("effective budget with unset field = %v, want %v", got, DefaultCreateRestorePodReadyTimeout)
	}
	if DefaultCreateRestorePodReadyTimeout <= DefaultPodReadyTimeout {
		t.Errorf("DefaultCreateRestorePodReadyTimeout (%v) must exceed DefaultPodReadyTimeout (%v) — the create-path clone budget has to be more generous than the no-clone waits",
			DefaultCreateRestorePodReadyTimeout, DefaultPodReadyTimeout)
	}

	set := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	set.CreateRestorePodReadyTimeout = 900 * time.Second
	if got := set.CreateRestorePodReadyTimeoutEffective(); got != 900*time.Second {
		t.Errorf("effective budget with set field = %v, want 900s", got)
	}
}

// slowInitClonePodReactor models a FRESH create-then-restore Pod B whose cold
// init-clone re-hydrates the workspace volume INSIDE the readiness window
// (ka-0000004449): a Get for podName reports the pod not-Ready until readyAfter
// has elapsed since the first observation, then Ready (with uid). Gets for any
// other pod fall through to the default tracker (so an absent Pod A stays
// NotFound, keeping waitPodGone a no-op). All client calls in Thaw run on the
// test goroutine, so the closure state needs no locking.
func slowInitClonePodReactor(podName, uid string, readyAfter time.Duration) ktesting.ReactionFunc {
	var firstSeen time.Time
	return func(action ktesting.Action) (bool, runtime.Object, error) {
		ga, ok := action.(ktesting.GetAction)
		if !ok || ga.GetName() != podName {
			return false, nil, nil // not our Pod B / not a Get — default handling
		}
		if firstSeen.IsZero() {
			firstSeen = time.Now()
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: podName, Namespace: ga.GetNamespace(), UID: types.UID(uid)},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}
		if time.Since(firstSeen) >= readyAfter {
			pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
		}
		return true, pod, nil
	}
}

// TestThaw_CreateRestorePodB_SlowInitCloneUsesCloneBudget is the ka-0000004449
// regression guard: a create-then-restore Pod B whose cold init-clone takes
// LONGER than podReadyTimeout (the no-clone budget) must still reach the runsc
// restore, because the create path waits on the larger clone-aware
// createRestorePodReadyTimeout. Before the fix the create path used
// podReadyTimeout (180s on prod); a repo-heavy ~2m50s clone blew it and the thaw
// cold-started before the restore ever ran. Here PodReadyTimeout is set far
// SHORTER than the modeled clone time precisely so a regression that reverts the
// create path to podReadyTimeout fails this test.
func TestThaw_CreateRestorePodB_SlowInitCloneUsesCloneBudget(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	bName := "agentsim-thaw-b"
	client := fake.NewClientset() // no Pod A — teardown already complete
	// Pod B Create succeeds (default tracker); a get reactor models the slow
	// clone: not-Ready until 150ms in, then Ready.
	client.PrependReactor("get", "pods", slowInitClonePodReactor(bName, "b-uid-456", 150*time.Millisecond))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	// The clone (150ms) outlasts the no-clone budget but is well within the
	// clone-aware budget. If the create path regressed to podReadyTimeout, the
	// 40ms here would time the wait out before the clone finished.
	m.PodReadyTimeout = 40 * time.Millisecond
	m.CreateRestorePodReadyTimeout = 10 * time.Second
	m.PodReadyPollInterval = 2 * time.Millisecond

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err != nil {
		t.Fatalf("create-then-restore thaw should wait out the slow init-clone and restore, got: %v", err)
	}
	if res.PlaceholderPod != bName {
		t.Errorf("placeholder = %q, want %q", res.PlaceholderPod, bName)
	}
	// The whole point: the runsc restore actually ran (it never did on the prod
	// timeout — the thaw cold-started before reaching the node exec).
	if len(exec.scripts) != 1 {
		t.Errorf("node restore should have run exactly once, got %d script(s)", len(exec.scripts))
	}
}

// TestThaw_CreateRestorePodB_BoundedByCloneBudget is the complementary direction:
// the create path is bounded by createRestorePodReadyTimeout SPECIFICALLY, not by
// podReadyTimeout. PodReadyTimeout is set LARGE and createRestorePodReadyTimeout
// SMALL while the clone never finishes within either; the thaw must fail on the
// small budget (proving that is the one consulted) without ever reaching the
// restore, and tear the create-path Pod B down (no leak). A regression that read
// podReadyTimeout here would instead wait the full large budget.
//
// The Pod B DELETION assertion is the regression guard for ka-0000006072: the
// create-path teardown defer is now registered BEFORE waitPodReady, so a Ready-wait
// TIMEOUT (this test) tears Pod B down instead of leaking it to the reaper. Before
// that reorder the defer was registered only after waitPodReady returned Ready, so
// this timeout path returned upstream of it and left Pod B pending — a leak MR !4981
// widened to <=600s. Asserting the Delete fired makes the "no leak" claim real.
func TestThaw_CreateRestorePodB_BoundedByCloneBudget(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	bName := "agentsim-thaw-b"
	client := fake.NewClientset()
	// Clone never becomes Ready within the test's budget window (5s >> the 60ms
	// create budget), so the wait must hit its deadline.
	client.PrependReactor("get", "pods", slowInitClonePodReactor(bName, "b-uid-456", 5*time.Second))
	// Record the create-path Pod B teardown Delete so we can assert no leak.
	var podBDeleted bool
	client.PrependReactor("delete", "pods", func(action ktesting.Action) (bool, runtime.Object, error) {
		if da, ok := action.(ktesting.DeleteAction); ok && da.GetName() == bName {
			podBDeleted = true
		}
		return false, nil, nil // fall through to the default tracker delete
	})
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})
	// podReadyTimeout is LARGE; if the create path (wrongly) consulted it, the
	// clone would finish (5s < 10s) and the thaw would succeed. It must instead
	// time out on the SMALL clone budget.
	m.PodReadyTimeout = 10 * time.Second
	m.CreateRestorePodReadyTimeout = 60 * time.Millisecond
	m.PodReadyPollInterval = 2 * time.Millisecond

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err == nil || !strings.Contains(err.Error(), "not Ready") {
		t.Fatalf("want create-path not-Ready timeout, got %v", err)
	}
	// The error must report the create budget (60ms), not podReadyTimeout (10s) —
	// the exact "which deadline fired" signal.
	if !strings.Contains(err.Error(), "60ms") {
		t.Errorf("timeout error should cite the create-restore budget (60ms), got %q", err.Error())
	}
	// The restore must never have run — Pod B never reached Ready.
	if len(exec.scripts) != 0 {
		t.Errorf("node restore ran despite the Ready-wait timeout: %d script(s)", len(exec.scripts))
	}
	// ka-0000006072: the pending create-path Pod B must be torn down on the Ready-wait
	// timeout — the defer now fires because it is registered BEFORE waitPodReady. A
	// regression that moves it back after waitPodReady leaks Pod B here (Delete never
	// fires) and this assertion goes RED.
	if !podBDeleted {
		t.Errorf("create-path Pod B %q was NOT torn down after the Ready-wait timeout — it leaked to the reaper (ka-0000006072: the teardown defer must be registered BEFORE waitPodReady)", bName)
	}
}
