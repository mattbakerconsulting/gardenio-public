// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

package podmanager

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

// freezethaw_thaw_source_repoint_test.go is the ka-0000001104 / bug ka-0000000673
// RED-BAR guard for the thaw-state mount-SOURCE re-point.
//
// THE BUG (instrumented canary #7, prod 2026.180.27). On a warm-B graft the restored
// sandbox's /run/gardenio-thaw is the CHECKPOINTED 9p gofer mount re-pointed to Pod B's
// LIVE source — and on the warm path Pod B is a pool placeholder minted from a DIFFERENT
// agent's snapshot template, so its thaw-state hostPath sources from
// /var/lib/gardenio/thaw/<placeholder-template-agent>, NOT the NEW agent the controller
// writes the epoch marker for. The restored coop/gb watchers therefore read+ack into the
// PLACEHOLDER's stale thaw-state dir while the controller's NodeRehydrationTrigger writes
// the marker into + polls thawStateHostDir(<new agent>) — so the ack NEVER rendezvous and
// the rehydrate gate fail-closes both consumers every cycle.
//
// THE FIX. The thaw [3] PYAGENT mount-rewrite forces the agent bundle's MountThawState
// (/run/gardenio-thaw) mount SOURCE to the NEW agent's live hostPath (threaded in as the
// THAWDIR shell var = thawStateHostDir(req.AgentName)), OVERRIDING the bsrc[d] live-Pod-B
// branch that resolves the placeholder's dir. The node pre-creates+chowns THAWDIR so the
// forced source resolves (and the [3a] dangling-source guard passes) and is agent-writable.
//
//	REAL in the behavioral test: the ACTUAL PYAGENT block extracted verbatim from the
//	                             rendered thaw template, run by python3 against a warm-B
//	                             cross-agent fixture on the test tmpfs.
//	STUBBED: HMNT is irrelevant (the rewrite is pure python over a config.json on disk).

// extractPyagentBlock slices the real PYAGENT mount-rewrite body out of the rendered
// thaw template, between the `<<'PYAGENT'` heredoc open and its closing `PYAGENT` line.
// A refactor that renames either marker makes extraction fail LOUD (a clear RED) rather
// than letting the guard silently no-op.
func extractPyagentBlock(t *testing.T, script string) string {
	t.Helper()
	const startMarker = "<<'PYAGENT'\n"
	const endMarker = "\nPYAGENT\n"
	_, afterStart, ok := strings.Cut(script, startMarker)
	if !ok {
		t.Fatalf("could not locate PYAGENT heredoc open %q in the rendered template — the rewrite boundary moved; update this behavioral test (do NOT let it silently no-op)", strings.TrimSpace(startMarker))
	}
	block, _, ok := strings.Cut(afterStart, endMarker)
	if !ok {
		t.Fatalf("could not locate PYAGENT heredoc close %q after the open — the rewrite boundary moved; update this behavioral test", strings.TrimSpace(endMarker))
	}
	for _, must := range []string{
		`bsrc=`,       // the live-Pod-B destination->source map
		`gofer=`,      // the data-driven warm-copy manifest map
		`m["source"]`, // the actual source rewrite under test
	} {
		if !strings.Contains(block, must) {
			t.Fatalf("extracted PYAGENT block is missing load-bearing step %q — extraction window is wrong or the step was removed", must)
		}
	}
	return block
}

// runPyagent runs the extracted PYAGENT rewrite via python3 with the positional args the
// heredoc consumes. The signature must mirror the live `HMNT python3 - ... <<'PYAGENT'`
// invocation in freezethaw_node.go; if a new arg is appended there it must be appended
// here too (a too-short argv would IndexError and fail the test LOUD).
func runPyagent(t *testing.T, block string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not available: %v", err)
	}
	cmd := exec.Command("python3", append([]string{"-"}, args...)...)
	// Retarget the hardcoded agent uid/gid onto this runner; no-op at uid 1000.
	cmd.Stdin = strings.NewReader(rewriteAgentIDsForRunner(block))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("PYAGENT rewrite execution failed: %v\n%s", err, out)
	}
	return string(out)
}

// warmBPyagentArgs builds the positional argv for the PYAGENT block from a fixture map.
// It is the SINGLE place the test encodes the live invocation's arg ORDER, so a re-order
// in freezethaw_node.go is caught here (the assertions below would shift meaning) rather
// than silently passing.
func warmBPyagentArgs(agentCfg, bundle, bcfg, thawDir string) []string {
	// Live order (freezethaw_node.go):
	//   "$BUNDLE/$dstn/config.json" "$ASB" "$BSB" "$APOD" "$BPODUID" "$BUNDLE" "$BCFG" "$THAWDIR" "<MountThawState>"
	return []string{
		agentCfg,   // p
		"ASANDBOX", // asb
		"BSANDBOX", // bsb
		"APODUID",  // apod
		"BPODUID",  // bpod
		bundle,     // bundle
		bcfg,       // bcfg (Pod B live config)
		thawDir,    // thawdir (NEW agent's live hostPath)
		MountThawState,
	}
}

// TestThawScript_RepointsThawStateSourceToNewAgent is the ka-0000000673 RED-BAR
// behavioral guard. It runs the ACTUAL PYAGENT rewrite against a warm-B cross-agent
// fixture where Pod B's LIVE thaw-state source is the PLACEHOLDER template agent's dir
// (agentX) and asserts the rewritten /run/gardenio-thaw source is instead the NEW agent's
// dir (THAWDIR == thawStateHostDir(agentY)). On the unfixed rewrite the bsrc[d] branch
// wins and the source stays agentX -> RED. With the THAWDIR force it is agentY -> GREEN.
func TestThawScript_RepointsThawStateSourceToNewAgent(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractPyagentBlock(t, m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"),
	}))

	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "agent"), 0o755); err != nil {
		t.Fatalf("mkdir bundle/agent: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, ".gofer"), 0o755); err != nil {
		t.Fatalf("mkdir bundle/.gofer: %v", err)
	}

	// The CHECKPOINT agent config (frozen agent Y): /run/gardenio-thaw sources from Y's
	// frozen hostPath. The restore re-walks the mountpoint, so the SOURCE is what matters.
	placeholderThawDir := thawStateHostDir("agentX") // the warm placeholder's template agent
	newAgentThawDir := thawStateHostDir("agentY")    // the agent the controller writes the marker for
	cfg := map[string]any{
		"annotations": map[string]string{"io.kubernetes.cri.sandbox-id": "ASANDBOX"},
		"mounts": []map[string]string{
			{"destination": MountThawState, "source": placeholderThawDir, "type": "bind"},
			{"destination": "/work", "source": "/var/lib/kubelet/pods/APODUID/volumes/kubernetes.io~empty-dir/work", "type": "bind"},
		},
	}
	cfgBytes, _ := json.Marshal(cfg)
	agentCfg := filepath.Join(bundle, "agent", "config.json")
	if err := os.WriteFile(agentCfg, cfgBytes, 0o644); err != nil {
		t.Fatalf("write agent config: %v", err)
	}

	// Pod B LIVE config: a warm placeholder built from agentX's template, so ITS
	// /run/gardenio-thaw sources from agentX's dir — the exact divergence the fix closes.
	bcfg := map[string]any{"mounts": []map[string]string{
		{"destination": MountThawState, "source": placeholderThawDir},
		{"destination": "/work", "source": "/var/lib/kubelet/pods/BPODUID/volumes/kubernetes.io~empty-dir/work"},
	}}
	bcfgBytes, _ := json.Marshal(bcfg)
	bcfgPath := filepath.Join(root, "bcfg.json")
	if err := os.WriteFile(bcfgPath, bcfgBytes, 0o644); err != nil {
		t.Fatalf("write bcfg: %v", err)
	}

	out := runPyagent(t, block, warmBPyagentArgs(agentCfg, bundle, bcfgPath, newAgentThawDir)...)

	got := readMountSource(t, agentCfg, MountThawState)
	if got == placeholderThawDir {
		t.Fatalf("thaw-state source was NOT re-pointed to the new agent: still the warm placeholder's dir %q.\n"+
			"The restored coop/gb watchers would read+ack into the PLACEHOLDER's stale thaw-state while the controller\n"+
			"polls thawStateHostDir(<new agent>)=%q — both-consumer fail-close (ka-0000000673).\nsweep output:\n%s",
			placeholderThawDir, newAgentThawDir, out)
	}
	if got != newAgentThawDir {
		t.Fatalf("thaw-state source = %q, want the NEW agent's live hostPath %q (THAWDIR force)", got, newAgentThawDir)
	}
}

// TestThawScript_CreatePathThawStateSourceUnchanged proves zero blast radius on the
// create-then-wait path: there Pod B is minted from the SAME agent's snapshot, so its
// live thaw-state source ALREADY equals THAWDIR. The forced re-point must resolve to the
// SAME value the bsrc[d] branch would have — i.e. the create path is byte-equivalent.
func TestThawScript_CreatePathThawStateSourceUnchanged(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractPyagentBlock(t, m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"),
	}))

	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "agent"), 0o755); err != nil {
		t.Fatalf("mkdir bundle/agent: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, ".gofer"), 0o755); err != nil {
		t.Fatalf("mkdir bundle/.gofer: %v", err)
	}
	sameAgentThawDir := thawStateHostDir("agentY")
	cfg := map[string]any{"annotations": map[string]string{}, "mounts": []map[string]string{
		{"destination": MountThawState, "source": sameAgentThawDir, "type": "bind"},
	}}
	cfgBytes, _ := json.Marshal(cfg)
	agentCfg := filepath.Join(bundle, "agent", "config.json")
	if err := os.WriteFile(agentCfg, cfgBytes, 0o644); err != nil {
		t.Fatalf("write agent config: %v", err)
	}
	// Create-path Pod B: same-agent template, so live source already == THAWDIR.
	bcfg := map[string]any{"mounts": []map[string]string{
		{"destination": MountThawState, "source": sameAgentThawDir},
	}}
	bcfgBytes, _ := json.Marshal(bcfg)
	bcfgPath := filepath.Join(root, "bcfg.json")
	if err := os.WriteFile(bcfgPath, bcfgBytes, 0o644); err != nil {
		t.Fatalf("write bcfg: %v", err)
	}

	runPyagent(t, block, warmBPyagentArgs(agentCfg, bundle, bcfgPath, sameAgentThawDir)...)

	if got := readMountSource(t, agentCfg, MountThawState); got != sameAgentThawDir {
		t.Fatalf("create-path thaw-state source = %q, want %q (the force must be a no-op when Pod B is the same agent)", got, sameAgentThawDir)
	}
}

// TestThawScript_ThawStateForceIsDestinationScoped proves the THAWDIR force only touches
// the /run/gardenio-thaw mount — a sidecar/agent mount at any OTHER destination must keep
// the live-Pod-B / warm-copy source resolution it had before. Guards against a blanket
// "rewrite every source to THAWDIR" regression.
func TestThawScript_ThawStateForceIsDestinationScoped(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractPyagentBlock(t, m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"),
	}))

	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "agent"), 0o755); err != nil {
		t.Fatalf("mkdir bundle/agent: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, ".gofer"), 0o755); err != nil {
		t.Fatalf("mkdir bundle/.gofer: %v", err)
	}
	newAgentThawDir := thawStateHostDir("agentY")
	const podBWorkSrc = "/var/lib/kubelet/pods/BPODUID/volumes/kubernetes.io~empty-dir/work"
	cfg := map[string]any{"annotations": map[string]string{}, "mounts": []map[string]string{
		{"destination": MountThawState, "source": thawStateHostDir("agentX"), "type": "bind"},
		{"destination": "/work", "source": "/var/lib/kubelet/pods/APODUID/volumes/kubernetes.io~empty-dir/work", "type": "bind"},
	}}
	cfgBytes, _ := json.Marshal(cfg)
	agentCfg := filepath.Join(bundle, "agent", "config.json")
	if err := os.WriteFile(agentCfg, cfgBytes, 0o644); err != nil {
		t.Fatalf("write agent config: %v", err)
	}
	bcfg := map[string]any{"mounts": []map[string]string{
		{"destination": MountThawState, "source": thawStateHostDir("agentX")},
		{"destination": "/work", "source": podBWorkSrc},
	}}
	bcfgBytes, _ := json.Marshal(bcfg)
	bcfgPath := filepath.Join(root, "bcfg.json")
	if err := os.WriteFile(bcfgPath, bcfgBytes, 0o644); err != nil {
		t.Fatalf("write bcfg: %v", err)
	}

	runPyagent(t, block, warmBPyagentArgs(agentCfg, bundle, bcfgPath, newAgentThawDir)...)

	// /work must keep its dedicated warm-copy source ($BUNDLE/.work), NOT become THAWDIR.
	work := readMountSource(t, agentCfg, "/work")
	if work == newAgentThawDir {
		t.Fatalf("/work source was clobbered to THAWDIR %q — the force must be scoped to MountThawState only", newAgentThawDir)
	}
	if !strings.HasSuffix(work, "/.work") {
		t.Fatalf("/work source = %q, want the dedicated warm-copy $BUNDLE/.work (the force must not perturb other mounts)", work)
	}
	// And the thaw-state mount IS re-pointed.
	if got := readMountSource(t, agentCfg, MountThawState); got != newAgentThawDir {
		t.Fatalf("thaw-state source = %q, want %q", got, newAgentThawDir)
	}
}

// readMountSource reads a config.json and returns the source for the given destination.
func readMountSource(t *testing.T, cfgPath, dest string) string {
	t.Helper()
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read %s: %v", cfgPath, err)
	}
	var c struct {
		Mounts []struct {
			Destination string `json:"destination"`
			Source      string `json:"source"`
		} `json:"mounts"`
	}
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("parse %s: %v", cfgPath, err)
	}
	for _, m := range c.Mounts {
		if m.Destination == dest {
			return m.Source
		}
	}
	t.Fatalf("destination %q not found in %s", dest, cfgPath)
	return ""
}

// TestThawScript_PrecreatesThawStateDir is the render-string guard for the node-side
// pre-create of THAWDIR. The forced source MUST exist + be agent-writable at restore
// time, or the [3a] dangling-source guard fail-closes (or the agent cannot write its
// ack). The script must mkdir THAWDIR and chown it to the agent UID/GID.
func TestThawScript_PrecreatesThawStateDir(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"),
	})
	for _, want := range []string{
		`THAWDIR='` + thawStateHostDir("agentY") + `'`, // rendered shell var
		`HMNT mkdir -p "$THAWDIR"`,                     // pre-create on the node
		`chown 1000:1000 "$THAWDIR"`,                   // agent-writable for the ack
	} {
		if !strings.Contains(tw, want) {
			t.Errorf("thaw script missing THAWDIR pre-create step: %q", want)
		}
	}
	// The pre-create must land BEFORE the [3a] dangling-source guard so the forced
	// source resolves when the guard runs.
	iMkdir := strings.Index(tw, `HMNT mkdir -p "$THAWDIR"`)
	iGuard := strings.Index(tw, "dangling bind-mount source(s)")
	if iMkdir < 0 || iGuard < 0 || iMkdir > iGuard {
		t.Errorf("THAWDIR pre-create must precede the [3a] dangling-source guard (mkdir=%d guard=%d)", iMkdir, iGuard)
	}
}

// TestThawScript_EmptyThawStateDirIsInert proves a legacy snapshot / unset ThawStateDir
// (empty THAWDIR) does NOT force any re-point — the rewrite falls through to the existing
// bsrc[d] behavior, so an older controller path carries zero blast radius.
func TestThawScript_EmptyThawStateDirIsInert(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractPyagentBlock(t, m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		// ThawStateDir intentionally empty (legacy snapshot / older controller).
	}))

	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "agent"), 0o755); err != nil {
		t.Fatalf("mkdir bundle/agent: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, ".gofer"), 0o755); err != nil {
		t.Fatalf("mkdir bundle/.gofer: %v", err)
	}
	placeholderThawDir := thawStateHostDir("agentX")
	cfg := map[string]any{"annotations": map[string]string{}, "mounts": []map[string]string{
		{"destination": MountThawState, "source": placeholderThawDir, "type": "bind"},
	}}
	cfgBytes, _ := json.Marshal(cfg)
	agentCfg := filepath.Join(bundle, "agent", "config.json")
	if err := os.WriteFile(agentCfg, cfgBytes, 0o644); err != nil {
		t.Fatalf("write agent config: %v", err)
	}
	bcfg := map[string]any{"mounts": []map[string]string{
		{"destination": MountThawState, "source": placeholderThawDir},
	}}
	bcfgBytes, _ := json.Marshal(bcfg)
	bcfgPath := filepath.Join(root, "bcfg.json")
	if err := os.WriteFile(bcfgPath, bcfgBytes, 0o644); err != nil {
		t.Fatalf("write bcfg: %v", err)
	}
	// Empty THAWDIR arg (the live script renders THAWDIR='').
	runPyagent(t, block, warmBPyagentArgs(agentCfg, bundle, bcfgPath, "")...)

	// With no force, the bsrc[d] branch is unchanged: source stays the live Pod B value.
	if got := readMountSource(t, agentCfg, MountThawState); got != placeholderThawDir {
		t.Fatalf("empty THAWDIR must NOT alter the rewrite (got source %q, want unchanged bsrc[d] %q)", got, placeholderThawDir)
	}
}

// TestThaw_RendersThawDirFromRequestAgent is the end-to-end WIRING guard. The 5
// render/behavioral tests above hand-construct thawScriptParams with ThawStateDir
// set directly, so they never exercise the freezethaw_thaw.go call site that POPULATES
// ThawStateDir from thawStateHostDir(req.AgentName). This test drives the real
// Thaw() path (warm-B graft onto a Ready placeholder) and asserts the captured
// thaw script renders THAWDIR keyed to the REQUEST agent — catching a regression
// that drops the wiring, mis-keys it, or hardcodes a literal (any of which would
// silently reintroduce bug ka-0000000673 in prod while the render tests stay green).
func TestThaw_RendersThawDirFromRequestAgent(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	const agent = "agent-wire-check"
	// A Ready warm placeholder Pod B already on the node (warm-B graft hot path):
	// Thaw SKIPS Create+waitReady and grafts straight onto it, then drives the
	// barrier-restore node exec whose script we capture.
	placeholder := livePodA("warm-b-node-1-xyz", ns, node)
	placeholder.UID = types.UID("b-uid-wire")
	placeholder.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	client := fake.NewClientset(placeholder)
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: agent, Namespace: ns, PodName: agent, Node: node,
		ImagePath:  "/var/lib/gardenio/ckpt/" + agent,
		BundlePath: "/var/lib/gardenio/bundle/" + agent,
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 2,
		PodTemplate: sanitizePodTemplate(livePodA(agent, ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	// SkipRehydration so the (real-node-only) marker write/poll does not run under
	// the fake executor; the barrier-restore script is rendered + captured first.
	_, err := m.Thaw(context.Background(), ThawRequest{
		AgentName: agent, PlaceholderPod: placeholder.Name, SkipRehydration: true,
	})
	if err != nil {
		t.Fatalf("Thaw: %v", err)
	}
	// Find the barrier-restore script among the captured execs.
	var thawScript string
	for _, s := range exec.scripts {
		if strings.Contains(s, "<<'PYAGENT'") {
			thawScript = s
			break
		}
	}
	if thawScript == "" {
		t.Fatalf("no thaw barrier-restore script captured (execs=%d)", len(exec.scripts))
	}
	wantTHAWDIR := "THAWDIR='" + thawStateHostDir(agent) + "'"
	if !strings.Contains(thawScript, wantTHAWDIR) {
		t.Fatalf("thaw script did not render THAWDIR keyed to the request agent.\n  want substring: %s\n"+
			"This is the freezethaw_thaw.go wiring (ThawStateDir: thawStateHostDir(req.AgentName)); a mutation that drops or\n"+
			"mis-keys it silently reintroduces bug ka-0000000673 (the forced source would not match the controller's poll dir).", wantTHAWDIR)
	}
	// And the empty placeholder THAWDIR='' (the inert form) must NOT be what rendered.
	if strings.Contains(thawScript, "THAWDIR=''") {
		t.Fatal("thaw script rendered an EMPTY THAWDIR — the wiring fell through to the inert path, so the source re-point never fires (bug ka-0000000673 live)")
	}
}

// TestThawScript_PygoferArgOrderMatchesPyagentInvocation guards the arg-order
// contract between the live PYAGENT shell invocation and the embedded python's
// positional unpack. extractPyagentBlock slices only the python BODY, so a re-order
// of the `HMNT python3 - ... <<'PYAGENT'` SHELL line (which would silently disable
// the fix — thawdir would receive the destination and d==thawdest never match) is
// NOT caught by the behavioral tests. Pin the shell-line order here: $THAWDIR must
// be the 8th positional and $THAWDEST the 9th, matching `sys.argv[1:10]` unpack
// order (p,asb,bsb,apod,bpod,bundle,bcfg,thawdir,thawdest).
func TestThawScript_PygoferArgOrderMatchesPyagentInvocation(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"), ThawStateDest: MountThawState,
	})
	// The exact shell invocation line: the positional args, in order, terminated by
	// the heredoc open. $THAWDIR then $THAWDEST, last before <<'PYAGENT'.
	const wantInvocation = `HMNT python3 - "$BUNDLE/$dstn/config.json" "$ASB" "$BSB" "$APOD" "$BPODUID" "$BUNDLE" "$BCFG" "$THAWDIR" "$THAWDEST" <<'PYAGENT'`
	if !strings.Contains(tw, wantInvocation) {
		t.Fatalf("PYAGENT shell invocation arg order drifted from the python unpack (thawdir 8th, thawdest 9th).\n  want line: %s\n"+
			"A re-order here silently disables the thaw-state force (thawdir would receive the destination, d==thawdest never matches) — the behavioral tests cannot catch it because they re-encode argv by hand.", wantInvocation)
	}
	// The python unpack must consume exactly those 9 positionals in the same order.
	const wantUnpack = `p,asb,bsb,apod,bpod,bundle,bcfg,thawdir,thawdest=sys.argv[1:10]`
	if !strings.Contains(tw, wantUnpack) {
		t.Fatalf("PYAGENT python unpack drifted; want %q", wantUnpack)
	}
}

// TestThawScript_PrecreateChmodsGroupWritable pins the [2c] chmod 0775 on $THAWDIR.
// The group-write bit is load-bearing: the pod's FSGroup is AgentGID and the agent
// writes its .done ack through the mount, so a dir created without group-write (or a
// dropped chmod) would EACCES the ack and re-fail-close the gate. Asserted separately
// from TestThawScript_PrecreatesThawStateDir (which covers mkdir/chown/ordering).
func TestThawScript_PrecreateChmodsGroupWritable(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"), ThawStateDest: MountThawState,
	})
	if !strings.Contains(tw, `chmod 0775 "$THAWDIR"`) {
		t.Errorf(`thaw [2c] must chmod 0775 "$THAWDIR" — the FSGroup=AgentGID agent writes its .done ack through the mount, so the dir must be group-writable or the ack EACCESes and the gate re-fail-closes (bug ka-0000000673)`)
	}
}
