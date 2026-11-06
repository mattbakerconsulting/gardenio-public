package podmanager

import (
	"context"
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

// fakeNodeExecutor returns a scripted result and records the script it was
// asked to run, so tests can assert both the post-state-assertion logic and the
// shape of the generated node script.
type fakeNodeExecutor struct {
	result   NodeExecResult
	err      error
	lastNode string
	scripts  []string
	// resultFn, if set, computes the result from the script (e.g. to branch
	// freeze vs thaw when the same executor serves both).
	resultFn func(node, script string) (NodeExecResult, error)
}

func (f *fakeNodeExecutor) Exec(_ context.Context, node, script string) (NodeExecResult, error) {
	f.lastNode = node
	f.scripts = append(f.scripts, script)
	if f.resultFn != nil {
		return f.resultFn(node, script)
	}
	return f.result, f.err
}

// recordingRehydrator records rehydration requests and returns a canned error.
type recordingRehydrator struct {
	reqs []RehydrationRequest
	err  error
}

func (r *recordingRehydrator) Trigger(_ context.Context, req RehydrationRequest) error {
	r.reqs = append(r.reqs, req)
	return r.err
}

// readyPodReactor mutates created pods to be Ready (and assigns a UID) so the
// fake clientset reports them Ready immediately — the fake tracker stores the
// object the reactor mutated in place.
func readyPodReactor(uid string) ktesting.ReactionFunc {
	return func(action ktesting.Action) (bool, runtime.Object, error) {
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
			Type:   corev1.PodReady,
			Status: corev1.ConditionTrue,
		})
		if pod.UID == "" {
			pod.UID = types.UID(uid)
		}
		return false, nil, nil // not handled — let the default tracker store the mutated pod
	}
}

// livePodA is a running multi-container agent pod on a node, used as the freeze
// target and the source of the placeholder Pod B template.
func livePodA(name, ns, node string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, UID: types.UID("a-uid-123"),
		},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name:    ContainerName,
				Image:   "agent:latest",
				Command: []string{"/entrypoint.sh"},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "work", MountPath: "/work"},
				},
			}},
			InitContainers: []corev1.Container{{Name: "init-clone", Image: "alpine/git"}},
			Volumes: []corev1.Volume{{
				Name:         "work",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// freezeOKStdout is a node-script stdout that passes all freeze post-state
// assertions.
const freezeOKStdout = `    resolving cids
FT_AGENT_CID=agentcid111
FT_SANDBOX_CID=sandboxcid222
FT_POD_UID=a-uid-123
FT_SUBC=agent|agentcid111
FT_CHECKPOINT_MS=3120
FT_CHECKPOINT_RC=0
FT_IMAGE_PRESENT=yes
FT_IMAGE_SIZE=8.4M
FT_CONTAINER_COUNT=2
FT_SANDBOX_STATE=running
FT_BUNDLE_OK=yes
FT_RELOCATION_MS=870
FT_IMAGE_OK=yes
FT_SPEC=sandbox/config.json|eyJvY2lWZXJzaW9uIjoiMS4wLjAiLCJtb3VudHMiOltdfQ==
FT_SPEC=agent/config.json|eyJvY2lWZXJzaW9uIjoiMS4wLjAiLCJtb3VudHMiOltdfQ==
FT_SPEC=sandbox/.overlay-opts|dXBwZXJkaXI9L3UsbG93ZXJkaXI9L2w=
FT_SPEC=agent/.overlay-opts|dXBwZXJkaXI9L3UsbG93ZXJkaXI9L2w=
`

// freezeOKStdoutSidecars passes freeze post-state assertions for a Docker+Postgres
// agent: container_count=4 (pause + agent + dind + postgres), a 3-entry FT_SUBC
// roster, and config/overlay spec files for every container subdir.
const freezeOKStdoutSidecars = `    resolving cids
FT_AGENT_CID=agentcid111
FT_SANDBOX_CID=sandboxcid222
FT_POD_UID=a-uid-123
FT_SUBC=agent|agentcid111
FT_SUBC=dind|dindcid333
FT_SUBC=postgres|pgcid444
FT_CHECKPOINT_RC=0
FT_IMAGE_PRESENT=yes
FT_IMAGE_SIZE=9.1M
FT_CONTAINER_COUNT=4
FT_SANDBOX_STATE=running
FT_BUNDLE_OK=yes
FT_IMAGE_OK=yes
FT_SPEC=sandbox/config.json|eyJvY2lWZXJzaW9uIjoiMS4wLjAiLCJtb3VudHMiOltdfQ==
FT_SPEC=sandbox/.overlay-opts|dXBwZXJkaXI9L3UsbG93ZXJkaXI9L2w=
FT_SPEC=agent/config.json|eyJvY2lWZXJzaW9uIjoiMS4wLjAiLCJtb3VudHMiOltdfQ==
FT_SPEC=agent/.overlay-opts|dXBwZXJkaXI9L3UsbG93ZXJkaXI9L2w=
FT_SPEC=dind/config.json|eyJvY2lWZXJzaW9uIjoiMS4wLjAiLCJtb3VudHMiOltdfQ==
FT_SPEC=dind/.overlay-opts|dXBwZXJkaXI9L3UsbG93ZXJkaXI9L2w=
FT_SPEC=postgres/config.json|eyJvY2lWZXJzaW9uIjoiMS4wLjAiLCJtb3VudHMiOltdfQ==
FT_SPEC=postgres/.overlay-opts|dXBwZXJkaXI9L3UsbG93ZXJkaXI9L2w=
`

// thawOKStdout passes all thaw post-state assertions: both restores rc=0, the
// restored sentry shares Pod B's netns inode.
const thawOKStdout = `    barrier restore
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
FT_THAW_SRC_PROVENANCE=ok
FT_CONTAINERS_RUNNING=yes
`

func newFreezeThawManager(t *testing.T, client *fake.Clientset, exec NodeExecutor, snaps SnapshotStore, rehydr RehydrationTrigger) *FreezeThawManager {
	t.Helper()
	m := NewFreezeThawManager(client, exec, snaps, rehydr, testLogger())
	// Tight timers so readiness polling does not slow tests.
	m.PodReadyTimeout = 2 * time.Second
	m.PodReadyPollInterval = time.Millisecond
	// The pre-thaw memory gate (ka-0000000088) adds a node exec before the
	// barrier restore; disable it for the shared test fixture so legacy tests
	// that index exec.scripts[0] as the barrier-restore script still work
	// unchanged. Gate-behavior tests explicitly clear MemGateDisabled.
	m.MemGateDisabled = true
	return m
}

func TestFreeze_Success(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodA("agentsim", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, exec, store, nil)

	snap, timing, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", BeadID: "gd-AGENT", PodName: "agentsim", Namespace: ns,
	})
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if snap.SandboxCID != "sandboxcid222" || snap.AgentCID != "agentcid111" {
		t.Errorf("cids: sandbox=%q agent=%q", snap.SandboxCID, snap.AgentCID)
	}
	// Per-step attribution parsed from the node FT_*_MS sentinels (task
	// ka-0000005147): the freeze histogram needs checkpoint vs. relocation broken
	// out independently. Persist is Go-side (a fast in-memory Put here, so just
	// assert it is non-negative, not a specific value).
	if timing.Checkpoint != 3120*time.Millisecond {
		t.Errorf("timing.Checkpoint = %v, want 3.12s (FT_CHECKPOINT_MS=3120)", timing.Checkpoint)
	}
	if timing.Relocation != 870*time.Millisecond {
		t.Errorf("timing.Relocation = %v, want 870ms (FT_RELOCATION_MS=870)", timing.Relocation)
	}
	if timing.Persist < 0 {
		t.Errorf("timing.Persist = %v, want >= 0", timing.Persist)
	}
	if snap.ContainerCount != 2 {
		t.Errorf("container count = %d, want 2", snap.ContainerCount)
	}
	if snap.Node != node {
		t.Errorf("node = %q, want %q", snap.Node, node)
	}
	if exec.lastNode != node {
		t.Errorf("exec ran on %q, want %q", exec.lastNode, node)
	}
	// Snapshot persisted under agent identity.
	got, err := store.Get(context.Background(), "agentsim")
	if err != nil {
		t.Fatalf("snapshot not persisted: %v", err)
	}
	if got.SandboxCID != "sandboxcid222" {
		t.Errorf("persisted sandbox cid = %q", got.SandboxCID)
	}
	// Pod template was captured with NodeName stripped (re-pinned at thaw).
	if got.PodTemplate.NodeName != "" {
		t.Errorf("template NodeName should be cleared, got %q", got.PodTemplate.NodeName)
	}
	if len(got.PodTemplate.Containers) != 1 {
		t.Errorf("template containers = %d, want 1", len(got.PodTemplate.Containers))
	}
	// The freeze script wraps runsc in a timeout and checkpoints --leave-running.
	script := exec.scripts[0]
	for _, want := range []string{"timeout", "checkpoint --leave-running", "--root=$ROOT"} {
		if !strings.Contains(script, want) {
			t.Errorf("freeze script missing %q", want)
		}
	}
}

func TestFreeze_ResolvesNodeFromPod(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "node-7"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	m := newFreezeThawManager(t, client, exec, NewInMemorySnapshotStore(), nil)

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", PodName: "agentsim", Namespace: ns, // Node omitted
	})
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if snap.Node != "node-7" {
		t.Errorf("node = %q, want node-7 (resolved from pod)", snap.Node)
	}
}

func TestFreeze_CheckpointNonZeroRC(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "n1"))
	stdout := "FT_SANDBOX_CID=sb\nFT_AGENT_CID=ag\nFT_CHECKPOINT_RC=1\nFT_IMAGE_PRESENT=no\n"
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout, Stderr: "runsc: boom"}}
	m := newFreezeThawManager(t, client, exec, NewInMemorySnapshotStore(), nil)

	_, _, err := m.Freeze(context.Background(), FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns})
	if err == nil || !strings.Contains(err.Error(), "checkpoint rc=1") {
		t.Fatalf("want checkpoint rc=1 error, got %v", err)
	}
}

func TestFreeze_SandboxNotRunningAfterLeaveRunning(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "n1"))
	stdout := "FT_SANDBOX_CID=sb\nFT_AGENT_CID=ag\nFT_CHECKPOINT_RC=0\nFT_IMAGE_PRESENT=yes\nFT_CONTAINER_COUNT=2\nFT_SANDBOX_STATE=stopped\n"
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout}}
	m := newFreezeThawManager(t, client, exec, NewInMemorySnapshotStore(), nil)

	_, _, err := m.Freeze(context.Background(), FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns})
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("want not-running error, got %v", err)
	}
}

func TestFreeze_IncompleteBundleFailsClosed(t *testing.T) {
	// The checkpoint image is fine, the sandbox is still running, count is baked
	// in — but the preserved OCI bundle is incomplete (FT_BUNDLE_OK=no). Freeze
	// MUST refuse to persist an un-thawable snapshot (the prod rc=128 defect).
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "n1"))
	stdout := "FT_SANDBOX_CID=sb\nFT_AGENT_CID=ag\nFT_CHECKPOINT_RC=0\n" +
		"FT_IMAGE_PRESENT=yes\nFT_CONTAINER_COUNT=2\nFT_SANDBOX_STATE=running\nFT_BUNDLE_OK=no\n"
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout, Stderr: "BUNDLE INCOMPLETE: sandbox/config.json missing"}}
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, exec, store, nil)

	_, _, err := m.Freeze(context.Background(), FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns})
	if err == nil || !strings.Contains(err.Error(), "incomplete OCI bundle") {
		t.Fatalf("want incomplete-bundle error, got %v", err)
	}
	// No snapshot may be persisted — the controller must cold-start cleanly.
	if _, gerr := store.Get(context.Background(), "agentsim"); gerr == nil {
		t.Fatal("snapshot persisted despite incomplete bundle; must fail closed")
	}
}

func TestFreeze_MissingBundleOKSentinelFailsClosed(t *testing.T) {
	// An old/garbled script that never emits FT_BUNDLE_OK must also fail closed:
	// absence is treated as not-OK (we never trust an unverified bundle).
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "n1"))
	stdout := "FT_SANDBOX_CID=sb\nFT_AGENT_CID=ag\nFT_CHECKPOINT_RC=0\n" +
		"FT_IMAGE_PRESENT=yes\nFT_CONTAINER_COUNT=2\nFT_SANDBOX_STATE=running\n" // no FT_BUNDLE_OK
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout}}
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, exec, store, nil)

	_, _, err := m.Freeze(context.Background(), FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns})
	if err == nil || !strings.Contains(err.Error(), "incomplete OCI bundle") {
		t.Fatalf("want incomplete-bundle error when sentinel absent, got %v", err)
	}
	if _, gerr := store.Get(context.Background(), "agentsim"); gerr == nil {
		t.Fatal("snapshot persisted despite missing FT_BUNDLE_OK; must fail closed")
	}
}

func TestFreezeScript_CapturesConfigAndAssertsBundle(t *testing.T) {
	// The generated freeze script must (B) explicitly resolve+copy config.json
	// from the runsc-state bundle dir for both containers, (A) assert bundle
	// completeness and emit FT_BUNDLE_OK, and NOT swallow cp stderr to /dev/null.
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "n1"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	m := newFreezeThawManager(t, client, exec, NewInMemorySnapshotStore(), nil)

	if _, _, err := m.Freeze(context.Background(), FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns}); err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	script := exec.scripts[0]
	for _, want := range []string{
		"bundle_dir()", // resolves OCI bundle dir from runsc state
		`HMNT cp -a "$cfgsrc" "$BUNDLE/$dstn/config.json"`, // explicit host-ns config.json copy
		"FT_BUNDLE_OK=", // emits the completeness sentinel
		`HMNT test -s "$BUNDLE/$dstn/config.json"`, // host-ns assert: non-empty config.json
		`HMNT test -s "$BUNDLE/$dstn/.overlay-opts"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("freeze script missing %q", want)
		}
	}
	// stderr must no longer be swallowed on the bundle-payload copy loop.
	if strings.Contains(script, `cp -a "$fpath" "$BUNDLE/$dstn/" 2>/dev/null`) {
		t.Error("freeze script still swallows cp stderr (2>/dev/null) on bundle copy")
	}
}

// TestFreezeScript_BundleOpsRunInHostMountNS is the ka-0000002536 regression
// guard. The PROD rc=128 was a MOUNT-NAMESPACE ASYMMETRY: the script body runs in
// the probe pod ns (NodeExecutor execs `chroot /host bash -c`), but the checkpoint
// image is written — and thaw reads the OCI bundle — through `nsenter -t 1 -m`
// (HMNT). Plain `cp`/`mkdir`/`test` wrote the bundle + FT_BUNDLE_OK-asserted it in
// the throwaway probe ns, so thaw's host-ns runsc FetchSpec-ENOENTed. Every
// $BUNDLE filesystem op MUST be host-namespaced (HMNT). Asserting the FT_BUNDLE_OK
// guard runs host-namespaced is what the script-text-only tests missed.
func TestFreezeScript_BundleOpsRunInHostMountNS(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	script := m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90})

	// The bundle dir creation, overlay-opts write, config copy, payload copy, the
	// completeness guard, and the warm-/work copy must all be wrapped in HMNT so
	// they land on the SAME host /var/tmp/gb-bundle the image + thaw use.
	for _, want := range []string{
		`HMNT rm -rf "$BUNDLE" && HMNT mkdir -p "$BUNDLE"`, // bundle root in host ns
		`HMNT mkdir -p "$BUNDLE/$dstn/rootfs"`,             // per-container dir in host ns
		`HMNT tee "$BUNDLE/$dstn/.overlay-opts"`,           // overlay-opts written host ns
		`HMNT test -f "$cand/config.json"`,                 // source probed in host ns
		`HMNT cp -a "$cfgsrc" "$BUNDLE/$dstn/config.json"`, // config copy in host ns
		`HMNT test -s "$BUNDLE/$dstn/config.json"`,         // guard asserts in host ns
		`HMNT test -s "$BUNDLE/$dstn/.overlay-opts"`,
		`HMNT rm -rf "$BUNDLE/.work"`,                           // warm /work rm host ns
		`HMNT cp -aL --reflink=auto "$WORKSRC" "$BUNDLE/.work"`, // warm /work copy host ns, CoW payload (ka-0000005425), rc captured for FT_WORK_CP_RC (ka-0000000276)
		`HMNT rm -rf "$BUNDLE/.tmp"`,                            // preserved /tmp gofer tree rm host ns (ka-0000001085)
		`HMNT cp -aL --reflink=auto "$TMPSRC" "$BUNDLE/.tmp"`,   // preserved /tmp gofer tree copy host ns, CoW payload (ka-0000005425), rc captured for FT_TMP_CP_RC (ka-0000000276)
	} {
		if !strings.Contains(script, want) {
			t.Errorf("freeze bundle op not host-namespaced: missing %q", want)
		}
	}

	// The OLD probe-ns forms (no HMNT prefix) are exactly the prod defect — they
	// must be gone. Anchoring on the line start (\n) avoids matching the HMNT-
	// wrapped variants that legitimately contain the same suffix.
	for _, banned := range []string{
		"\nrm -rf \"$BUNDLE\" && mkdir -p \"$BUNDLE\"\n", // unwrapped bundle root
		"\n  mkdir -p \"$BUNDLE/$dstn/rootfs\"\n",        // unwrapped per-container dir
		"\n  echo \"$ovl\" > \"$BUNDLE/$dstn/.overlay-opts\"\n",
		"\n  [ -s \"$BUNDLE/$dstn/config.json\" ]",   // unwrapped guard
		"\n  [ -s \"$BUNDLE/$dstn/.overlay-opts\" ]", // unwrapped guard
	} {
		if strings.Contains(script, banned) {
			t.Errorf("freeze script still runs a bundle op in the probe mount ns (prod rc=128 defect): %q", banned)
		}
	}
}

// TestFreezeScript_PayloadCopyReasonDenoise is the ka-0000006933 render + shape
// guard. It asserts the freeze script (a) still renders without a text/template
// panic after embedding the ftClassifyCopyRC classifier (S4 — a stray backtick or
// {{ would have blown up template.Must at init, so reaching this assertion at all
// proves the render), (b) emits the de-noise reason sentinels FT_WORK_CP_REASON /
// FT_TMP_CP_REASON on their OWN lines (M6 — a truncated stream must not bleed a
// reason into another sentinel), (c) short-circuits to reason=clean on rc==0 (M7 —
// no probe cost on a healthy freeze), (d) invokes ftClassifyCopyRC on the non-zero
// path, and (e) still emits the raw FT_WORK_CP_RC / FT_TMP_CP_RC unchanged (M8 —
// old-Go/new-node back-compat: old controllers key off the rc and must be
// unaffected), and preserves the cp -aL --reflink=auto invocation verbatim (I2 —
// bytes copied unchanged).
func TestFreezeScript_PayloadCopyReasonDenoise(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	script := m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90})

	for _, want := range []string{
		"ftClassifyCopyRC() {",                                          // the classifier function is defined
		`echo "FT_WORK_CP_REASON=$WORK_CP_REASON"`,                      // /work reason emitted on its own line (M6)
		`echo "FT_TMP_CP_REASON=$TMP_CP_REASON"`,                        // /tmp reason emitted on its own line (M6)
		`WORK_CP_REASON=$(ftClassifyCopyRC "$WORKSRC" "$BUNDLE/.work")`, // classify the non-zero /work rc
		`TMP_CP_REASON=$(ftClassifyCopyRC "$TMPSRC" "$BUNDLE/.tmp")`,    // classify the non-zero /tmp rc
		`if [ "$WORK_CP_RC" -eq 0 ]; then WORK_CP_REASON=clean;`,        // rc==0 short-circuit (M7)
		`if [ "$TMP_CP_RC" -eq 0 ]; then TMP_CP_REASON=clean;`,          // rc==0 short-circuit (M7)
		`echo "FT_WORK_CP_RC=$WORK_CP_RC"`,                              // raw rc still emitted (M8)
		`echo "FT_TMP_CP_RC=$TMP_CP_RC"`,                                // raw rc still emitted (M8)
		`HMNT cp -aL --reflink=auto "$WORKSRC" "$BUNDLE/.work"`,         // bytes-copied unchanged (I2)
		`HMNT cp -aL --reflink=auto "$TMPSRC" "$BUNDLE/.tmp"`,           // bytes-copied unchanged (I2)
		"timeout -s KILL 15s python3",                                   // classifier is time-bounded (M7)
	} {
		if !strings.Contains(script, want) {
			t.Errorf("freeze script payload-denoise shape: missing %q", want)
		}
	}
	// The classifier verdict whitelist must fail unrecognized tokens to copyfail
	// (M6), never silently pass a truncated reason through as complete.
	if !strings.Contains(script, "*) echo \"copyfail\" ;;") {
		t.Error("freeze script classifier must whitelist reason tokens, defaulting unknown to copyfail (M6)")
	}
}

// TestFreezeScript_DataDrivenGoferPreserve asserts the freeze script sweeps and
// preserves ALL runtime-populated 9p gofer EmptyDir/subPath mounts (not just the
// dedicated /work + /tmp), recording them in a manifest thaw repoints from
// (ka-0000006427, decision ka-0000004662: fix_race_too = fix direction B). This is
// what carries /home/agent/.claude (and any future such mount) across C/R.
func TestFreezeScript_DataDrivenGoferPreserve(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	script := m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90})
	for _, want := range []string{
		`HMNT rm -rf "$BUNDLE/.gofer" && HMNT mkdir -p "$BUNDLE/.gofer"`, // gofer-preserve dir in host ns
		`HMNT python3 - "$BUNDLE/agent/config.json" "$BUNDLE"`,           // sweep reads the agent spec host ns
		`/var/lib/kubelet/pods/[^/]+/(volumes|volume-subpaths)/`,         // kubelet EmptyDir/subPath family
		`if d in ("/work","/tmp"):`,                                      // dedicated cases excluded from sweep
		`shutil.copytree(s,dst`,                                          // warm-copy each mount tree
		`.gofer","manifest.json"`,                                        // manifest dest->n written
		`FT_GOFER_PRESERVED=`,                                            // sentinel for observability
	} {
		if !strings.Contains(script, want) {
			t.Errorf("freeze data-driven gofer preserve missing %q", want)
		}
	}
}

// TestFreezeScript_ConsistentSnapshotPauseWindow is the ka-0000004656 regression
// guard (decision ka-0000003069: fix_tmp_consistency / fix dir A). The live-Claude
// thaw rc=128 root cause was that the 9p-gofer warm copies (/work, /tmp, .gofer)
// were taken AFTER a bare `runsc checkpoint --leave-running` had already RESUMED the
// sentry, so a churning agent mutated the gofer trees between the checkpoint's VFS
// capture and the cp — the checkpoint referenced a transient dentry the copy no
// longer saw, ENOENT'ing the thaw 9p walk. The fix brackets BOTH the gofer copies
// AND the checkpoint inside one `runsc pause`..`runsc resume` window so the copy and
// the dumped VFS are byte-identical. This test pins the ordering so a future edit
// cannot silently slide a warm copy back outside the window.
func TestFreezeScript_ConsistentSnapshotPauseWindow(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	script := m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90})

	// All the load-bearing markers must be present.
	idxPause := strings.Index(script, `RUNSC pause "$SB"; PRC=$?`)
	idxWork := strings.Index(script, `HMNT cp -aL --reflink=auto "$WORKSRC" "$BUNDLE/.work"`)
	idxTmp := strings.Index(script, `HMNT cp -aL --reflink=auto "$TMPSRC" "$BUNDLE/.tmp"`)
	idxGofer := strings.Index(script, `HMNT python3 - "$BUNDLE/agent/config.json" "$BUNDLE" <<'PYGOFER'`)
	idxCkpt := strings.Index(script, `RUNSC checkpoint --leave-running --image-path="$IMG" "$SB"`)
	idxResume := strings.Index(script, "\nresume_sb\ntrap - EXIT INT TERM")
	for name, idx := range map[string]int{
		"runsc pause": idxPause, "/work copy": idxWork, "/tmp copy": idxTmp,
		"gofer sweep": idxGofer, "checkpoint": idxCkpt, "resume_sb": idxResume,
	} {
		if idx < 0 {
			t.Fatalf("freeze script missing consistent-snapshot marker: %s", name)
		}
	}

	// THE ORDERING INVARIANT: pause < {all three gofer copies} < checkpoint < resume.
	// If any warm copy moved after the checkpoint (or after resume), the TOCTOU is
	// back. If the checkpoint moved after resume, the dump is of a running guest.
	if !(idxPause < idxWork && idxPause < idxTmp && idxPause < idxGofer) {
		t.Error("gofer warm copies must be taken AFTER `runsc pause` (inside the quiescent window)")
	}
	if !(idxWork < idxCkpt && idxTmp < idxCkpt && idxGofer < idxCkpt) {
		t.Error("gofer warm copies must be taken BEFORE the checkpoint (so the copy matches the dumped VFS)")
	}
	if !(idxCkpt < idxResume) {
		t.Error("checkpoint must run BEFORE the sentry is resumed (else --leave-running's guest is live during the dump's consistency window)")
	}

	// The pause window must be exit-safe: a trap resumes the sentry on ANY exit path
	// (checkpoint failure / aborted exec) so a paused sandbox is never left hung.
	if !strings.Contains(script, `trap 'resume_sb' EXIT INT TERM`) {
		t.Error("freeze must install a resume trap so an aborted/failed freeze never leaves the sentry externally paused")
	}
	// FT_PAUSE_RC is the observability sentinel for the window.
	if !strings.Contains(script, `echo "FT_PAUSE_RC=$PRC"`) {
		t.Error("freeze must emit FT_PAUSE_RC for the consistent-snapshot pause")
	}

	// There must be exactly ONE of each warm copy — the OLD post-resume copies were
	// deleted, not duplicated. A second occurrence means the trailing blocks were
	// left behind (the copy would then run post-resume and re-introduce the race).
	if strings.Count(script, `HMNT cp -aL --reflink=auto "$TMPSRC" "$BUNDLE/.tmp"`) != 1 {
		t.Error("the /tmp warm copy must appear exactly once (inside the pause window); a duplicate means the post-resume copy was not removed")
	}
	if strings.Count(script, `print("FT_GOFER_PRESERVED=`) != 1 {
		t.Error("the data-driven gofer sweep must appear exactly once (inside the pause window)")
	}
}

func TestFreeze_NonZeroScriptExit(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "n1"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: "FT_CHECKPOINT_RC=127", Stderr: "no agent sub-container", ExitCode: 1}}
	m := newFreezeThawManager(t, client, exec, NewInMemorySnapshotStore(), nil)

	_, _, err := m.Freeze(context.Background(), FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns})
	if err == nil || !strings.Contains(err.Error(), "exited 1") {
		t.Fatalf("want script-exit error, got %v", err)
	}
}

// TestFreezeScript_PreservesRootfsLowerdirGCSafe is the ka-0000000655 fix-B
// regression guard. The agent rootfs overlay's source dirs (image lowerdirs +
// writable upperdir) live in containerd's per-container snapshot tree, inside the
// pod-lifecycle GC zone; Pod A's post-freeze teardown reaps them, so at thaw
// `mount -t overlay` ENOENTs ("special device overlay does not exist") on the
// vanished source -> rootfs left unmounted -> sub rc=128. Freeze must collapse
// the LIVE merged rootfs into a self-contained RO lowerdir under the
// controller-owned safe path ($BUNDLE/<c>/lower, outside pod GC) and rewrite
// .overlay-opts to name THAT — host-namespaced (HMNT) per ka-0000002536.
func TestFreezeScript_PreservesRootfsLowerdirGCSafe(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	script := m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90})

	for _, want := range []string{
		`LOWER="$BUNDLE/$dstn/lower"`,                     // preserved lower under the safe path
		`HMNT bash -c 'cp -a --reflink=auto "$1/." "$2/"`, // host-ns CoW-or-copy of the live merged rootfs (C3 ka-0000002239)
		`cp-lower "$T/$cid/rootfs" "$LOWER"`,              // source is the LIVE merged rootfs mountpoint
		`printf 'lowerdir=%s\n' "$LOWER"`,                 // .overlay-opts rewritten to the preserved lower
		`HMNT test -n "$(HMNT ls -A "$LOWER"`,             // assert the copy is non-empty before trusting it
	} {
		if !strings.Contains(script, want) {
			t.Errorf("freeze script missing rootfs-preservation op %q", want)
		}
	}
	// The preserved lower must be written via the host-ns tee (same mount ns thaw
	// reads), not a probe-ns redirect.
	if !strings.Contains(script, `printf 'lowerdir=%s\n' "$LOWER" | HMNT tee "$BUNDLE/$dstn/.overlay-opts"`) {
		t.Error("freeze must write the preserved-lower .overlay-opts host-namespaced (HMNT tee)")
	}
}

// TestFreezeScript_RelocationUsesReflinkAuto pins the C3 freeze-speed lever
// (ka-0000002239): the dominant relocation copy (C1-attributed ~88% of freeze
// latency) must use cp --reflink=auto so a CoW-capable node fs clones the merged
// rootfs in O(metadata) instead of a full byte copy, while degrading safely to a
// plain copy elsewhere. The effectiveness probe (FT_RELOC_REFLINK_<dstn>) must be
// emitted so a substrate run can prove whether CoW actually engaged.
func TestFreezeScript_RelocationUsesReflinkAuto(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	script := m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90})

	if !strings.Contains(script, `cp -a --reflink=auto "$1/." "$2/"`) {
		t.Error("relocation copy must use cp -a --reflink=auto (C3 ka-0000002239 CoW lever)")
	}
	// A bare `cp -a "$1/."` (without --reflink) in the relocation op would mean the
	// optimization regressed back to a full byte copy.
	if strings.Contains(script, `cp -a "$1/." "$2/"`) {
		t.Error("relocation copy still contains a non-reflink `cp -a \"$1/.\"` — C3 lever regressed")
	}
	if !strings.Contains(script, `FT_RELOC_FSTYPE_$dstn=`) {
		t.Error("freeze must emit FT_RELOC_FSTYPE_<dstn> so a substrate run can tell whether the node fs is reflink-capable")
	}
}

// TestFreezeScript_WorkspacePayloadUsesReflink pins the ka-0000005425 workspace-
// payload CoW lever: the /work + /tmp warm copies must pass cp --reflink=auto (the
// rootfs relocation already reflinks; the payload did not, so a repo-heavy freeze —
// ~170s single-clone on the same codebase — could hit the ~240s FreezeHardDeadline
// cap and cold-start despite being thawable). The .gofer sweep, which MUST stay a
// copytree to carry the atomic-writer SA-token chain, gets its CoW via a reflink-
// aware copy_function on the copytree call. --reflink=auto / the ioctl-then-copy2
// fallback are STRICTLY no-worse on non-CoW nodes, so this is a safe headroom win.
func TestFreezeScript_WorkspacePayloadUsesReflink(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	script := m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90})

	// /work + /tmp cp copies must carry --reflink=auto (kept -aL so attrs + the
	// top-level source-symlink deref are unchanged; only the data blocks CoW).
	for _, want := range []string{
		`HMNT cp -aL --reflink=auto "$WORKSRC" "$BUNDLE/.work"`,
		`HMNT cp -aL --reflink=auto "$TMPSRC" "$BUNDLE/.tmp"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("workspace warm copy must use cp --reflink=auto (ka-0000005425 CoW payload lever): missing %q", want)
		}
	}
	// The pre-fix non-reflink forms must be gone — their presence means the lever
	// regressed back to a full byte copy billed against the FreezeHardDeadline cap.
	for _, banned := range []string{
		`HMNT cp -aL "$WORKSRC" "$BUNDLE/.work"`,
		`HMNT cp -aL "$TMPSRC" "$BUNDLE/.tmp"`,
	} {
		if strings.Contains(script, banned) {
			t.Errorf("workspace warm copy still uses a non-reflink `cp -aL` — ka-0000005425 CoW lever regressed: %q", banned)
		}
	}
	// Reflink-engagement observability (ka-0000006167, this task's minor finding). The
	// rootfs relocation emits FT_RELOC_FSTYPE_<dstn>; the workspace-payload copies must
	// emit analogous FT_ sentinels so the C3 re-baseline can tell whether CoW engaged for
	// /work, /tmp, and .gofer. FT_WORK_FSTYPE/FT_TMP_FSTYPE report the copy destination's
	// fs type (reflink-capability); the two FT_GOFER_REFLINK_* counters report the
	// per-file CoW hit/fallback tally from the .gofer copy_function. All additive and
	// diagnostic-only (parsed as FT_ key=value); no existing sentinel is renamed.
	for _, want := range []string{
		`echo "FT_WORK_FSTYPE=`,              // /work copy-destination fs type
		`echo "FT_TMP_FSTYPE=`,               // /tmp copy-destination fs type
		`print("FT_GOFER_REFLINK_HITS=`,      // .gofer per-file FICLONE hits
		`print("FT_GOFER_REFLINK_FALLBACKS=`, // .gofer per-file copy2 fallbacks
	} {
		if !strings.Contains(script, want) {
			t.Errorf("freeze must emit workspace-payload reflink-engagement sentinel (ka-0000006167): missing %q", want)
		}
	}
	// Each fstype sentinel must appear exactly once (guarding against an accidental
	// duplicate the way the pause-window test guards the /tmp copy).
	for _, one := range []string{`echo "FT_WORK_FSTYPE=`, `echo "FT_TMP_FSTYPE=`} {
		if strings.Count(script, one) != 1 {
			t.Errorf("workspace-payload fstype sentinel must appear exactly once: %q (count=%d)", one, strings.Count(script, one))
		}
	}

	// The .gofer sweep must reflink its regular-file data via a copy_function on the
	// copytree (copytree stays so the atomic-writer token symlink chain round-trips).
	block := extractPygoferBlock(t, script)
	// The CoW-aware copier + its ioctl/fallback must be DEFINED in the sweep. These are
	// unique to the definition (a comment cannot supply a def/ioctl/copy2 body), so a
	// whole-block Contains is safe for them.
	for _, want := range []string{
		`FICLONE`,       // the reflink ioctl
		`shutil.copy2(`, // no-worse fallback on any non-CoW / error path
	} {
		if !strings.Contains(block, want) {
			t.Errorf("PYGOFER sweep must define a reflink copy_function with a copy2 fallback (ka-0000005425): missing %q", want)
		}
	}
	// THE LOAD-BEARING ASSERTION (gd-mmp6kwyFt8 review fix). The prior test did a
	// whole-block Contains for `copy_function=_reflink_copy2`, but that exact substring
	// also appears in the CODE COMMENT above the copytree call (freezethaw_node.go), so
	// deleting ONLY the kwarg from the actual copytree() call still passed — the .gofer
	// reflink lever was UNPINNED (mutation-verified). Isolate the real `shutil.copytree(`
	// CALL line (the same helper TestFreezeScript_AtomicWriterAwareCopy uses for its
	// symlinks= arg) and assert the kwarg is wired ON THE CALL. Removing the kwarg from
	// the call now FAILS this test even though the comment is untouched.
	copyLine := pygoferCopytreeCallLine(block)
	if copyLine == "" {
		t.Fatal("PYGOFER sweep has no shutil.copytree( call line")
	}
	if !strings.Contains(copyLine, "copy_function=_reflink_copy2") {
		t.Errorf("the copytree CALL must pass copy_function=_reflink_copy2 so the .gofer sweep CoW-clones regular-file data (ka-0000005425); a comment mentioning it does not count: %q", copyLine)
	}
}

// --- Fix B (ka-0000000301): spec-file capture + thaw re-materialize ----------

// TestFreeze_CapturesSpecFilesIntoSnapshot verifies that a successful freeze
// persists the four OCI spec files (sandbox/agent x config.json/.overlay-opts)
// into Snapshot.SpecFiles, decoded from the base64 sentinels. This is what makes
// thaw independent of the host bundle dir surviving Pod A teardown.
func TestFreeze_CapturesSpecFilesIntoSnapshot(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "n1"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, exec, store, nil)

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns})
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	for _, p := range []string{"sandbox/config.json", "agent/config.json", "sandbox/.overlay-opts", "agent/.overlay-opts"} {
		if len(snap.SpecFiles[p]) == 0 {
			t.Errorf("snapshot missing captured spec file %q", p)
		}
	}
	// config.json decodes to the real JSON bytes (not the base64).
	if got := string(snap.SpecFiles["sandbox/config.json"]); !strings.Contains(got, "ociVersion") {
		t.Errorf("sandbox config.json not decoded: %q", got)
	}
	// The persisted snapshot carries them too.
	stored, _ := store.Get(context.Background(), "agentsim")
	if len(stored.SpecFiles) != 4 {
		t.Errorf("persisted snapshot SpecFiles = %d, want 4", len(stored.SpecFiles))
	}
}

// TestFreeze_MissingSpecSentinelFailsClosed verifies that if the freeze script
// did not emit a spec sentinel (e.g. base64 capture failed on the node), freeze
// refuses to persist the snapshot — a snapshot missing a spec file is as
// un-thawable as one with a deleted bundle, so it must cold-start cleanly.
func TestFreeze_MissingSpecSentinelFailsClosed(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "n1"))
	// FT_BUNDLE_OK=yes + FT_IMAGE_OK=yes but the agent overlay spec file is absent.
	stdout := "FT_SANDBOX_CID=sb\nFT_AGENT_CID=ag\nFT_SUBC=agent|ag\nFT_CHECKPOINT_RC=0\n" +
		"FT_IMAGE_PRESENT=yes\nFT_CONTAINER_COUNT=2\nFT_SANDBOX_STATE=running\nFT_BUNDLE_OK=yes\nFT_IMAGE_OK=yes\n" +
		"FT_SPEC=sandbox/config.json|eyJ4IjoxfQ==\nFT_SPEC=agent/config.json|eyJ4IjoxfQ==\nFT_SPEC=sandbox/.overlay-opts|YQ==\n" // no agent/.overlay-opts
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout}}
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, exec, store, nil)

	_, _, err := m.Freeze(context.Background(), FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns})
	if err == nil || !strings.Contains(err.Error(), "missing agent/.overlay-opts") {
		t.Fatalf("want bundle-completeness failure when a spec file is absent, got %v", err)
	}
	if _, gerr := store.Get(context.Background(), "agentsim"); gerr == nil {
		t.Fatal("snapshot persisted despite missing spec sentinel; must fail closed")
	}
}

// --- Fix ka-0000003702: checkpoint-image relocation + fail-closed image guard --

// TestFreeze_MissingImageOKSentinelFailsClosed verifies the image-relocation
// guard fails closed when the freeze script never emits FT_IMAGE_OK (absence is
// treated as not-OK, exactly like FT_BUNDLE_OK). The 3.0G checkpoint image is the
// sibling deletion vector to the bundle; a snapshot whose image was not proven
// safely relocated is un-thawable, so no snapshot may be persisted.
func TestFreeze_MissingImageOKSentinelFailsClosed(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "n1"))
	// Bundle is complete + spec files present, but FT_IMAGE_OK is absent.
	stdout := "FT_SANDBOX_CID=sb\nFT_AGENT_CID=ag\nFT_SUBC=agent|ag\nFT_CHECKPOINT_RC=0\n" +
		"FT_IMAGE_PRESENT=yes\nFT_CONTAINER_COUNT=2\nFT_SANDBOX_STATE=running\nFT_BUNDLE_OK=yes\n" +
		"FT_SPEC=sandbox/config.json|eyJ4IjoxfQ==\nFT_SPEC=agent/config.json|eyJ4IjoxfQ==\n" +
		"FT_SPEC=sandbox/.overlay-opts|YQ==\nFT_SPEC=agent/.overlay-opts|YQ==\n" // no FT_IMAGE_OK
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout}}
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, exec, store, nil)

	_, _, err := m.Freeze(context.Background(), FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns})
	if err == nil || !strings.Contains(err.Error(), "not safely relocated") {
		t.Fatalf("want image-relocation failure when FT_IMAGE_OK absent, got %v", err)
	}
	if _, gerr := store.Get(context.Background(), "agentsim"); gerr == nil {
		t.Fatal("snapshot persisted despite missing FT_IMAGE_OK; must fail closed")
	}
}

// TestFreeze_ImageNotRelocatedFailsClosed verifies the guard fails closed when
// the freeze script positively reports FT_IMAGE_OK=no (the image was absent or
// empty at the controller-owned safe path after copy). Pod A must NOT be torn
// down behind an image thaw could never restore from.
func TestFreeze_ImageNotRelocatedFailsClosed(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "n1"))
	stdout := "FT_SANDBOX_CID=sb\nFT_AGENT_CID=ag\nFT_CHECKPOINT_RC=0\n" +
		"FT_IMAGE_PRESENT=yes\nFT_CONTAINER_COUNT=2\nFT_SANDBOX_STATE=running\nFT_BUNDLE_OK=yes\n" +
		"FT_SPEC_SANDBOX_CONFIG=eyJ4IjoxfQ==\nFT_SPEC_AGENT_CONFIG=eyJ4IjoxfQ==\n" +
		"FT_SPEC_SANDBOX_OVL=YQ==\nFT_SPEC_AGENT_OVL=YQ==\nFT_IMAGE_OK=no\n"
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout, Stderr: "IMAGE MISSING: checkpoint.img absent"}}
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, exec, store, nil)

	_, _, err := m.Freeze(context.Background(), FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns})
	if err == nil || !strings.Contains(err.Error(), "not safely relocated") {
		t.Fatalf("want image-relocation failure on FT_IMAGE_OK=no, got %v", err)
	}
	if _, gerr := store.Get(context.Background(), "agentsim"); gerr == nil {
		t.Fatal("snapshot persisted despite FT_IMAGE_OK=no; must fail closed")
	}
}

// TestFreeze_UsesControllerOwnedSafePaths asserts the persisted snapshot records
// the controller-owned safe paths under /var/lib/gardenio (outside the /var/tmp
// pod-lifecycle GC zone) for BOTH the image and the bundle, and that the freeze
// script is parameterized with those exact paths (not the literal "$IMG"/"$BUNDLE"
// placeholders the call site previously left in place — a path mismatch that
// guaranteed a thaw-time ENOENT independent of GC).
func TestFreeze_UsesControllerOwnedSafePaths(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "n1"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdout}}
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, exec, store, nil)

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns})
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if snap.ImagePath != "/var/lib/gardenio/ckpt/agentsim" {
		t.Errorf("ImagePath = %q, want controller-owned /var/lib/gardenio/ckpt/agentsim", snap.ImagePath)
	}
	if snap.BundlePath != "/var/lib/gardenio/bundle/agentsim" {
		t.Errorf("BundlePath = %q, want controller-owned /var/lib/gardenio/bundle/agentsim", snap.BundlePath)
	}
	if strings.Contains(snap.ImagePath, "/var/tmp") || strings.Contains(snap.BundlePath, "/var/tmp") {
		t.Errorf("paths must NOT be under /var/tmp (pod-lifecycle GC zone): image=%q bundle=%q", snap.ImagePath, snap.BundlePath)
	}
	// The freeze script must carry the concrete safe paths, not the placeholders.
	script := exec.scripts[0]
	if !strings.Contains(script, "IMG='/var/lib/gardenio/ckpt/agentsim'") {
		t.Error("freeze script not parameterized with the controller-owned image path")
	}
	if strings.Contains(script, "IMG='$IMG'") {
		t.Error("freeze script still renders the literal $IMG placeholder (path mismatch with thaw)")
	}
}

// TestFreezeScript_ImageOpsRunInHostMountNS guards the ka-0000002536 mount-ns
// invariant extended to the checkpoint image (ka-0000003702): runsc writes the
// image via nsenter -t 1, so the image dir setup + probes + the FT_IMAGE_OK
// re-verify must ALL be host-namespaced (both HMNT and the Fix-A HMNTT wrapper
// nsenter into PID 1's mount ns), not run in the throwaway probe ns where they
// would address a different filesystem.
//
// Fix A (bug ka-0000004931) split the host-ns helpers: the READ-ONLY image
// probes (test -f/-s, du, stat, strings) are now HMNTT — same host mount ns, but
// KILL-signal timeout-bounded so a wedged probe fails fast into the fail-closed
// markers instead of hanging+truncating the sentinel stream. The MUTATING dir
// setup (rm/mkdir) stays on plain HMNT (unbounded — bounding a mutating op risks
// a partial-copy un-thawable snapshot). This test asserts BOTH the mutating ops
// are HMNT and the read-only probes are the bounded HMNTT, and that every one
// still enters the host ns.
func TestFreezeScript_ImageOpsRunInHostMountNS(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	fs := m.buildFreezeScript(freezeScriptParams{
		PodName: "mypod", RunscTimeout: 120,
		ImagePath: "/var/lib/gardenio/ckpt/ag", BundlePath: "/var/lib/gardenio/bundle/ag",
	})
	for _, want := range []string{
		`HMNT rm -rf "$IMG" && HMNT mkdir -p "$IMG"`, // MUTATING image dir setup: host-ns, unbounded (HMNT)
		`HMNTT test -f "$IMG/checkpoint.img"`,        // READ-ONLY presence probe: host-ns, bounded (HMNTT, Fix A)
		`HMNTT test -s "$IMG/checkpoint.img"`,        // READ-ONLY re-verify (FT_IMAGE_OK): host-ns, bounded (HMNTT, Fix A)
		`HMNTT du -sh "$IMG"`,                        // READ-ONLY size probe: bounded (HMNTT, Fix A)
		`HMNTT stat -c %s "$IMG/pages.img"`,          // READ-ONLY pages stat: bounded (HMNTT, Fix A)
		`HMNTT bash -c 'strings "$1/checkpoint.img"`, // READ-ONLY container_count strings: bounded (HMNTT, Fix A)
		"FT_IMAGE_OK=", // emits the relocation sentinel
	} {
		if !strings.Contains(fs, want) {
			t.Errorf("freeze script missing host-ns image op %q", want)
		}
	}
	// HMNTT must be a host-ns helper bounded by the SAME RUNSC_TIMEOUT discipline
	// as RUNSC (Fix A). It must nsenter into PID 1's mount ns AND wrap the op in a
	// KILL-signal timeout, or a wedged probe would still hang the freeze exec.
	if !strings.Contains(fs, `HMNTT() { nsenter -t 1 -m -- timeout -s KILL`) {
		t.Error("HMNTT must be a host-ns (nsenter -t 1 -m) op bounded by a KILL-signal timeout (Fix A)")
	}
	if !strings.Contains(fs, `timeout -s KILL -k 5s "${RUNSC_TIMEOUT}"`) {
		t.Error("HMNTT must bound the probe with the same RUNSC_TIMEOUT budget RUNSC uses")
	}
	// The pre-relocation probe-ns ops must be gone.
	if strings.Contains(fs, `rm -rf "$IMG" && mkdir -p "$IMG"`) {
		t.Error("freeze script still does probe-ns image dir setup (must be HMNT)")
	}
	if strings.Contains(fs, `if [ -f "$IMG/checkpoint.img" ];`) {
		t.Error("freeze script still does probe-ns image presence check (must be HMNTT)")
	}
	// REGRESSION GUARD (Fix A scope): the read-only probes must NOT be plain HMNT
	// (that is the unbounded pre-fix shape that hangs+truncates on a wedge). If a
	// future edit reverts HMNTT->HMNT on the checkpoint-image probes, catch it here.
	for _, mustNot := range []string{
		`HMNT test -f "$IMG/checkpoint.img"`,
		`HMNT test -s "$IMG/checkpoint.img"`,
	} {
		if strings.Contains(fs, mustNot) {
			t.Errorf("read-only image probe reverted to unbounded HMNT (%q) — must be the bounded HMNTT (Fix A, ka-0000004931)", mustNot)
		}
	}
}

// TestThawScript_AssertsImagePresentBeforeRestore guards the loud pre-restore
// image-deletion assertion (ka-0000003702). Thaw must probe the checkpoint image
// host-namespaced BEFORE any runsc restore and bail with FT_ROOT_RC=128 +
// FT_IMAGE_OK=no on a reaped image, so the failure surfaces as an unmistakable
// image-deletion diagnosis instead of an opaque runsc "opening state file" error.
func TestThawScript_AssertsImagePresentBeforeRestore(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/var/lib/gardenio/ckpt/ag", BundlePath: "/var/lib/gardenio/bundle/ag",
		ContainerCount: 2, RunscTimeout: 90,
	})
	if !strings.Contains(tw, `HMNT test -s "$IMG/checkpoint.img"`) {
		t.Error("thaw script missing host-ns pre-restore image assertion")
	}
	if !strings.Contains(tw, "FT_IMAGE_OK=yes") || !strings.Contains(tw, "FT_IMAGE_OK=no") {
		t.Error("thaw script must emit FT_IMAGE_OK for both present/absent cases")
	}
	// The assertion must come BEFORE any runsc restore.
	iAssert := strings.Index(tw, `HMNT test -s "$IMG/checkpoint.img"`)
	iRestore := strings.Index(tw, "restore --image-path")
	if iAssert < 0 || iRestore < 0 || iAssert > iRestore {
		t.Errorf("image assertion must precede restore (assert=%d restore=%d)", iAssert, iRestore)
	}
}

// TestThaw_ImageDeletedFailsLoud verifies the Go post-state check turns the
// thaw script's FT_IMAGE_OK=no (image reaped between freeze and thaw) into a
// distinct image-deletion error rather than the opaque root-restore rc=128.
func TestThaw_ImageDeletedFailsLoud(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	// The script bailed early at the pre-restore image assertion.
	stdout := "FT_IMAGE_OK=no\nFT_ROOT_RC=128\n"
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout, Stderr: "checkpoint IMAGE absent at thaw", ExitCode: 1}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		ImagePath:  "/var/lib/gardenio/ckpt/agentsim",
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	rehydr := &recordingRehydrator{}
	m := newFreezeThawManager(t, client, exec, store, rehydr)

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err == nil || !strings.Contains(err.Error(), "image absent at restore") {
		t.Fatalf("want loud image-deletion error, got %v", err)
	}
	if len(rehydr.reqs) != 0 {
		t.Error("rehydration must not fire when the image was deleted")
	}
}

// TestThawScript_RematerializesSpecFiles is the fix-B regression guard: the thaw
// script must, BEFORE any runsc restore, re-create the bundle dir tree and
// rewrite the captured config.json/.overlay-opts in the host ns (HMNT), so a
// GC'd host bundle (the prod deletion vector) no longer FetchSpec-ENOENTs (rc=128).
func TestThawScript_RematerializesSpecFiles(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		SpecFiles: []thawSpecFile{
			{Path: "sandbox/config.json", Base64: "eyJ4IjoxfQ=="},
			{Path: "agent/config.json", Base64: "eyJ4IjoxfQ=="},
			{Path: "sandbox/.overlay-opts", Base64: "YQ=="},
			{Path: "agent/.overlay-opts", Base64: "YQ=="},
		},
	})
	for _, want := range []string{
		`for dstn in $DSTNS; do HMNT mkdir -p "$BUNDLE/$dstn/rootfs"; done`, // dir tree incl rootfs mountpoint per container
		`base64 -d | HMNT tee "$BUNDLE/$1"`,                                 // host-ns write of decoded bytes
		`write_spec 'sandbox/config.json' 'eyJ4IjoxfQ=='`,                   // each captured file emitted
		`write_spec 'agent/.overlay-opts' 'YQ=='`,
	} {
		if !strings.Contains(tw, want) {
			t.Errorf("thaw script missing spec re-materialize step: %q", want)
		}
	}
	// The re-materialize must come BEFORE the overlay-opts read and the restore.
	iMat := strings.Index(tw, "write_spec 'sandbox/config.json'")
	iOpts := strings.Index(tw, `HMNT cat "$BUNDLE/$dstn/.overlay-opts"`)
	iRestore := strings.Index(tw, "restore --image-path")
	if iMat < 0 || iOpts < 0 || iRestore < 0 || !(iMat < iOpts && iMat < iRestore) {
		t.Errorf("spec re-materialize must precede overlay read + restore (mat=%d opts=%d restore=%d)", iMat, iOpts, iRestore)
	}
}

// TestReapImageScript_HostNamespacedImageOnly guards the ka-0000006968 reaper
// script: it must rm the checkpoint image dir host-namespaced (HMNT, the
// ka-0000002536 mount-ns invariant runsc wrote the image under), target the
// concrete ImagePath, emit FT_IMAGE_REAPED for both outcomes, and NEVER touch the
// bundle (which backs the live Pod B).
func TestReapImageScript_HostNamespacedImageOnly(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	rs := m.buildReapImageScript(reapImageScriptParams{ImagePath: "/var/lib/gardenio/ckpt/ag"})
	for _, want := range []string{
		`IMG='/var/lib/gardenio/ckpt/ag'`, // concrete image path
		`HMNT() { nsenter -t 1 -m -- "$@"; }`,
		`HMNT rm -rf "$IMG"`,  // host-ns rm of the image dir
		"FT_IMAGE_REAPED=yes", // confirmed-reclaim sentinel
		"FT_IMAGE_REAPED=no",  // best-effort failure sentinel
	} {
		if !strings.Contains(rs, want) {
			t.Errorf("reap-image script missing %q", want)
		}
	}
	// The bundle is the live Pod B's backing — the reaper must never reference it.
	if strings.Contains(rs, "BUNDLE") || strings.Contains(rs, "/bundle/") {
		t.Error("reap-image script must NOT touch the bundle (it backs the live Pod B)")
	}
}

// TestEncodeDecodeSpecFiles round-trips the spec-file encode/decode helpers via
// the generic FT_SPEC "<path>|<base64>" wire format (ka-0000003561).
func TestEncodeDecodeSpecFiles(t *testing.T) {
	orig := map[string][]byte{
		"sandbox/config.json":   []byte(`{"ociVersion":"1.0.0"}`),
		"agent/config.json":     []byte(`{"process":{}}`),
		"sandbox/.overlay-opts": []byte("upperdir=/u,lowerdir=/l"),
		"agent/.overlay-opts":   []byte("lowerdir=/l2"),
		"dind/config.json":      []byte(`{"process":{"dind":true}}`),
		"dind/.overlay-opts":    []byte("lowerdir=/l3"),
	}
	enc := encodeThawSpecFiles(orig)
	if len(enc) != len(orig) {
		t.Fatalf("encode = %d entries, want %d", len(enc), len(orig))
	}
	// Re-decode through the generic FT_SPEC line format the freeze script emits.
	var lines []string
	for _, e := range enc {
		lines = append(lines, e.Path+"|"+e.Base64)
	}
	dec, err := decodeSpecFiles(lines)
	if err != nil {
		t.Fatalf("decodeSpecFiles: %v", err)
	}
	for p, want := range orig {
		if string(dec[p]) != string(want) {
			t.Errorf("round-trip %q = %q, want %q", p, dec[p], want)
		}
	}
}
