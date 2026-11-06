// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

package podmanager

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

// apierrorsForbidden models the serviceaccounts/token create RBAC denial (the
// default-off deploy without the token-mint grant) so the best-effort mint path is
// exercised.
func apierrorsForbidden() error {
	return apierrors.NewForbidden(
		schema.GroupResource{Resource: "serviceaccounts/token"}, "", nil)
}

// freezethaw_token_refresh_test.go is the offline red-bar suite for the post-thaw
// credential refresher (bug ka-0000000518). It has three layers:
//
//  1. RENDER GUARDS on buildTokenRefreshScript: the SCOPE guard (touches ONLY the
//     .gofer warm copies, never the sandbox/rootfs/other mounts) + the presence of
//     the atomic-overlay and 1000:1000 chown (the ownership-restore mirror).
//  2. A BEHAVIORAL test that runs the ACTUAL embedded PYTOKREFRESH block (via
//     python3) against an atomic-writer warm-copy fixture: it proves a fresh token
//     lands on the realpath-followed backing file AND that the backing file is
//     re-owned readable by uid/gid 1000. RED without the overlay+chown; GREEN with.
//  3. MANAGER tests of RefreshThawedTokens: enumeration (only CONSUMED snapshots
//     with a live thaw-placeholder pod are refreshed), the TokenRequest mint call,
//     and the default-off / best-effort behavior.

// tokRefreshRenderParams builds a representative params set for the render guards.
func tokRefreshRenderParams() tokenRefreshScriptParams {
	return tokenRefreshScriptParams{
		BundlePath:   "/var/lib/gardenio/bundle/agentX",
		SADest:       saTokenMountDest,
		IRSADest:     irsaTokenMountDest,
		SATokenB64:   base64.StdEncoding.EncodeToString([]byte("FRESH_SA_TOKEN")),
		IRSATokenB64: base64.StdEncoding.EncodeToString([]byte("FRESH_IRSA_TOKEN")),
	}
}

// TestTokenRefreshScript_ScopedToGoferWarmCopiesOnly is the SCOPE guard (fiq4 rubric
// ka-0000005354). The refresher runs on a LIVE thawed pod, so its blast radius MUST be
// confined to the two SA/IRSA token warm copies under $BUNDLE/.gofer/<n>. It must NOT
// reference the sandbox, the rootfs overlay, the [6c] sentry-egress block, the self-IPAM
// route, or drive any runsc verb.
func TestTokenRefreshScript_ScopedToGoferWarmCopiesOnly(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	script := m.buildTokenRefreshScript(tokRefreshRenderParams())

	// It MUST operate on the .gofer warm copies.
	if !strings.Contains(script, ".gofer") {
		t.Fatalf("token-refresh script must operate on the .gofer warm copies; got:\n%s", script)
	}
	// It MUST NOT touch anything outside that region. Check CODE (non-comment,
	// non-blank) lines only, so a word appearing in explanatory prose (e.g.
	// "the restored agent") does not false-trip — the guard is about what the
	// script DOES, not what it documents. These are the exact out-of-scope seams a
	// naive edit that reached for the rootfs / sandbox / netns / a runsc verb / the
	// [6c] egress block / another mount would trip.
	forbidden := []string{
		"runsc",     // no checkpoint/restore/delete verb — this is a live pod
		"rootfs",    // no rootfs overlay reconstruction
		"EGRESS",    // no [6c] sentry-egress block
		"self_ipam", // no self-IPAM route
		".claude",   // no ~/.claude warm trees
	}
	for raw := range strings.SplitSeq(script, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue // skip blank + python comment lines (prose, not behavior)
		}
		for _, f := range forbidden {
			if strings.Contains(line, f) {
				t.Errorf("token-refresh script CODE line references %q — out of scope (touch ONLY the SA/IRSA .gofer warm copies):\n  %s", f, line)
			}
		}
	}
}

// TestTokenRefreshScript_CarriesOverlayAndChown asserts the rendered script carries the
// two load-bearing operations — the atomic byte-overlay (temp file + os.rename following
// the realpath) and the 1000:1000 chown that restores the agent's read grant (the
// ka-0000000189 ownership-restore mirror). Without either, the refresh would leave a
// truncated or unreadable token.
func TestTokenRefreshScript_CarriesOverlayAndChown(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	script := m.buildTokenRefreshScript(tokRefreshRenderParams())
	for _, must := range []string{
		"os.rename(tmp,rp)",          // atomic overlay: rename over the realpath'd backing file
		"os.path.realpath",           // follow the atomic-writer token -> ..data/token chain
		"os.chown(rp,1000,1000)",     // restore the agent (uid/gid 1000) read grant
		"..data",                     // overlay BOTH token and ..data/token
		"manifest.json",              // resolve warm subdir from the .gofer manifest
		"FT_TOKREFRESH_DONE=yes",     // completion sentinel
	} {
		if !strings.Contains(script, must) {
			t.Errorf("token-refresh script missing load-bearing step %q; got:\n%s", must, script)
		}
	}
}

// extractTokRefreshBlock pulls the PYTOKREFRESH heredoc body out of the rendered script
// so a behavioral test can run the ACTUAL embedded python (not a hand-copied paraphrase),
// mirroring extractPyagentBlock. A moved boundary fails loudly rather than silently
// no-opping.
func extractTokRefreshBlock(t *testing.T, script string) string {
	t.Helper()
	const startMarker = "<<'PYTOKREFRESH'\n"
	const endMarker = "\nPYTOKREFRESH\n"
	_, afterStart, ok := strings.Cut(script, startMarker)
	if !ok {
		t.Fatalf("could not locate PYTOKREFRESH heredoc open in the rendered template — boundary moved; update this test")
	}
	block, _, ok := strings.Cut(afterStart, endMarker)
	if !ok {
		t.Fatalf("could not locate PYTOKREFRESH heredoc close — boundary moved; update this test")
	}
	return block
}

// writeWarmAtomicTokenMode builds the kubelet atomic-writer layout at dir (token ->
// ..data/token -> ..<ts>/token) with the given backing bytes + mode, and returns the
// backing file path. Mode 0600 models the on-node root:root warm copy's dropped agent
// grant at uid 1000 (owner rw, no group read).
func writeWarmAtomicTokenMode(t *testing.T, dir, tok string, mode os.FileMode) string {
	t.Helper()
	ts := filepath.Join(dir, "..2026_01_01")
	if err := os.MkdirAll(ts, 0o755); err != nil {
		t.Fatal(err)
	}
	backing := filepath.Join(ts, "token")
	if err := os.WriteFile(backing, []byte(tok), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(backing, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("..2026_01_01", filepath.Join(dir, "..data")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..data", "token"), filepath.Join(dir, "token")); err != nil {
		t.Fatal(err)
	}
	return backing
}

// tokRefreshFixture builds a bundle with a .gofer manifest mapping the two token
// destinations to warm subdirs 0 (SA) and 1 (IRSA) in the atomic-writer layout, backing
// tokens carrying the STALE (pre-refresh) bytes. Returns the bundle + backing paths.
func tokRefreshFixture(t *testing.T, mode os.FileMode) (bundle, backingSA, backingIRSA string) {
	t.Helper()
	root := t.TempDir()
	bundle = filepath.Join(root, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, ".gofer"), 0o755); err != nil {
		t.Fatal(err)
	}
	backingSA = writeWarmAtomicTokenMode(t, filepath.Join(bundle, ".gofer", "0"), "STALE_SA", mode)
	backingIRSA = writeWarmAtomicTokenMode(t, filepath.Join(bundle, ".gofer", "1"), "STALE_IRSA", mode)
	manifest := map[string]string{
		saTokenMountDest:            "0",
		"/var" + saTokenMountDest:   "0",
		irsaTokenMountDest:          "1",
		"/var" + irsaTokenMountDest: "1",
	}
	mb, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(bundle, ".gofer", "manifest.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}
	return bundle, backingSA, backingIRSA
}

// TestTokenRefreshScript_OverlaysFreshTokenAndRestoresReadGrant is the behavioral RED-BAR
// guard. It runs the ACTUAL embedded PYTOKREFRESH block against an atomic-writer warm-copy
// fixture whose backing token is stale + mode 0600 (no group read — the dropped agent
// grant). After the refresh:
//   - the realpath-followed backing file must carry the FRESH bytes (proving os.rename
//     landed on the backing file through the token -> ..data/token chain, not the symlink),
//   - the backing file must be readable by gid 1000 (group-read bit present, 0640) —
//     proving the chown/chmod ran.
//
// Pre-fix (no overlay or no chown) the backing token stays STALE / 0600 -> RED.
func TestTokenRefreshScript_OverlaysFreshTokenAndRestoresReadGrant(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not available: %v", err)
	}
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractTokRefreshBlock(t, m.buildTokenRefreshScript(tokRefreshRenderParams()))
	bundle, backingSA, backingIRSA := tokRefreshFixture(t, 0o600)

	// argv order mirrors the live PYTOKREFRESH invocation:
	//   "$BUNDLE" '{{.SADest}}' '{{.IRSADest}}' '{{.SATokenB64}}' '{{.IRSATokenB64}}'
	freshSA := "FRESH_SA_v2"
	freshIRSA := "FRESH_IRSA_v2"
	args := []string{
		bundle,
		saTokenMountDest,
		irsaTokenMountDest,
		base64.StdEncoding.EncodeToString([]byte(freshSA)),
		base64.StdEncoding.EncodeToString([]byte(freshIRSA)),
	}
	cmd := exec.Command("python3", append([]string{"-"}, args...)...)
	cmd.Stdin = strings.NewReader(rewriteAgentIDsForRunner(block))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("PYTOKREFRESH execution failed: %v\n%s", err, out)
	}

	for _, tc := range []struct {
		name, backing, want string
	}{
		{"SA", backingSA, freshSA},
		{"IRSA", backingIRSA, freshIRSA},
	} {
		got, rerr := os.ReadFile(tc.backing)
		if rerr != nil {
			t.Fatalf("%s: read backing token %s: %v", tc.name, tc.backing, rerr)
		}
		if string(got) != tc.want {
			t.Errorf("%s: backing token = %q, want fresh %q — the atomic overlay did not land on the realpath-followed backing file", tc.name, got, tc.want)
		}
		// The chown to 1000:1000 is a no-op at uid 1000 (the test runner), but the
		// chmod OR-in of 0640 is observable: mode 0600 -> 0640 (group-read restored).
		fi, serr := os.Stat(tc.backing)
		if serr != nil {
			t.Fatalf("%s: stat backing token: %v", tc.name, serr)
		}
		if fi.Mode().Perm()&0o040 == 0 {
			t.Errorf("%s: backing token mode %o lacks the group-read bit — the chmod OR-in of 0640 (the read-grant restore) did not run", tc.name, fi.Mode().Perm())
		}
	}
}

// --- Manager-level enumeration + mint tests --------------------------------

// thawPlaceholderPod builds a live thaw-placeholder Pod B (create-path naming
// "<podName>-thaw-b"), carrying the SA the mint targets and pinned to node.
func thawPlaceholderPod(podName, ns, node, sa string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName + ThawPlaceholderSuffix,
			Namespace: ns,
			UID:       "podb-uid-1",
			Labels: map[string]string{
				LabelApp:             LabelAppValue,
				LabelAgent:           "agentX",
				LabelThawPlaceholder: "true",
			},
		},
		Spec: corev1.PodSpec{NodeName: node, ServiceAccountName: sa},
		// Post-thaw Pod B reports phase=Failed by design (placeholder PID replaced
		// by the restored sentry) — the refresher must still enumerate it.
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}
}

// consumedSnap builds a CONSUMED snapshot (the post-thaw record that survives and
// carries the warm-copy BundlePath + Node) keyed by the canonical pod name.
func consumedSnap(agent, podName, ns, node, bundle string) Snapshot {
	return Snapshot{
		AgentName: agent, PodName: podName, Namespace: ns, Node: node,
		BundlePath: bundle, Consumed: true,
	}
}

// mintReactor registers a CreateToken reactor that echoes the requested audience so a
// test can tell the SA (default) mint from the IRSA (sts) mint, and counts calls.
func mintReactor(client *fake.Clientset, calls *int) {
	client.PrependReactor("create", "serviceaccounts/token", func(action ktesting.Action) (bool, runtime.Object, error) {
		*calls++
		ca := action.(ktesting.CreateAction)
		tr := ca.GetObject().(*authenticationv1.TokenRequest)
		aud := "default"
		if len(tr.Spec.Audiences) > 0 {
			aud = tr.Spec.Audiences[0]
		}
		return true, &authenticationv1.TokenRequest{
			Status: authenticationv1.TokenRequestStatus{Token: "MINTED:" + aud},
		}, nil
	})
}

// TestRefreshThawedTokens_MintsAndOverlaysForLiveThawedAgent is the enumeration happy
// path: a CONSUMED snapshot joined to a live thaw-placeholder pod is refreshed — both
// tokens are minted (SA default audience + IRSA sts audience) and the node script runs on
// the pod's node with the fresh bytes.
func TestRefreshThawedTokens_MintsAndOverlaysForLiveThawedAgent(t *testing.T) {
	ns, node := "gardenio", "node-7"
	pod := thawPlaceholderPod("m-p-r-agentX", ns, node, "gardenio-agent")
	client := fake.NewClientset(pod)
	var mintCalls int
	mintReactor(client, &mintCalls)
	nodeExec := &fakeNodeExecutor{result: NodeExecResult{
		Stdout: "FT_TOKREFRESH_SA=ok\nFT_TOKREFRESH_IRSA=ok\nFT_TOKREFRESH_DONE=yes\n",
	}}
	snaps := NewInMemorySnapshotStore()
	_ = snaps.Put(context.Background(), consumedSnap("agentX", "m-p-r-agentX", ns, node, "/var/lib/gardenio/bundle/agentX"))
	m := NewFreezeThawManager(client, nodeExec, snaps, nil, testLogger())

	n := m.RefreshThawedTokens(context.Background(), []string{ns})
	if n != 1 {
		t.Fatalf("RefreshThawedTokens refreshed %d agents, want 1", n)
	}
	if mintCalls != 2 {
		t.Errorf("minted %d tokens, want 2 (SA + IRSA)", mintCalls)
	}
	if nodeExec.lastNode != node {
		t.Errorf("node exec ran on %q, want the pod's node %q", nodeExec.lastNode, node)
	}
	if len(nodeExec.scripts) != 1 {
		t.Fatalf("expected exactly one refresh node exec, got %d", len(nodeExec.scripts))
	}
	// The fresh minted bytes must ride into the script (base64 of "MINTED:<aud>").
	script := nodeExec.scripts[0]
	wantSA := base64.StdEncoding.EncodeToString([]byte("MINTED:default"))
	wantIRSA := base64.StdEncoding.EncodeToString([]byte("MINTED:" + awsSTSAudience))
	if !strings.Contains(script, wantSA) {
		t.Errorf("refresh script missing the minted SA token bytes")
	}
	if !strings.Contains(script, wantIRSA) {
		t.Errorf("refresh script missing the minted IRSA token bytes")
	}
}

// TestRefreshThawedTokens_SkipsNonConsumedAndUnmatched proves the enumeration filter: an
// agent awaiting thaw (non-consumed snapshot) and a placeholder pod with no snapshot are
// both skipped — only a CONSUMED-snapshot + live-pod join is refreshed.
func TestRefreshThawedTokens_SkipsNonConsumedAndUnmatched(t *testing.T) {
	ns, node := "gardenio", "n1"
	// Pod with a snapshot that is NOT consumed (awaiting thaw) — skip.
	awaiting := thawPlaceholderPod("m-p-r-await", ns, node, "sa")
	awaiting.Labels[LabelAgent] = "await"
	// Placeholder pod with NO snapshot at all — skip.
	orphan := thawPlaceholderPod("m-p-r-orphan", ns, node, "sa")
	orphan.Labels[LabelAgent] = "orphan"
	client := fake.NewClientset(awaiting, orphan)
	var mintCalls int
	mintReactor(client, &mintCalls)
	nodeExec := &fakeNodeExecutor{result: NodeExecResult{Stdout: "FT_TOKREFRESH_DONE=yes\n"}}
	snaps := NewInMemorySnapshotStore()
	awaitSnap := consumedSnap("await", "m-p-r-await", ns, node, "/b/await")
	awaitSnap.Consumed = false // awaiting thaw
	_ = snaps.Put(context.Background(), awaitSnap)
	m := NewFreezeThawManager(client, nodeExec, snaps, nil, testLogger())

	n := m.RefreshThawedTokens(context.Background(), []string{ns})
	if n != 0 {
		t.Errorf("refreshed %d agents, want 0 (non-consumed + orphan must be skipped)", n)
	}
	if mintCalls != 0 {
		t.Errorf("minted %d tokens, want 0 — no eligible thawed agent", mintCalls)
	}
	if len(nodeExec.scripts) != 0 {
		t.Errorf("ran %d node execs, want 0", len(nodeExec.scripts))
	}
}

// TestRefreshThawedTokens_BothMintsFailIsBestEffort proves a total mint failure (e.g.
// missing serviceaccounts/token RBAC) is non-fatal: no node exec runs and the agent keeps
// its prior token (refreshed count 0), the loop does not error.
func TestRefreshThawedTokens_BothMintsFailIsBestEffort(t *testing.T) {
	ns, node := "gardenio", "n1"
	pod := thawPlaceholderPod("m-p-r-agentX", ns, node, "gardenio-agent")
	client := fake.NewClientset(pod)
	client.PrependReactor("create", "serviceaccounts/token", func(action ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrorsForbidden()
	})
	nodeExec := &fakeNodeExecutor{}
	snaps := NewInMemorySnapshotStore()
	_ = snaps.Put(context.Background(), consumedSnap("agentX", "m-p-r-agentX", ns, node, "/b/agentX"))
	m := NewFreezeThawManager(client, nodeExec, snaps, nil, testLogger())

	n := m.RefreshThawedTokens(context.Background(), []string{ns})
	if n != 0 {
		t.Errorf("refreshed %d, want 0 when both mints fail", n)
	}
	if len(nodeExec.scripts) != 0 {
		t.Errorf("ran %d node execs, want 0 when there is nothing to write", len(nodeExec.scripts))
	}
}

// TestCanonicalThawPodName covers both remap branches (create-path suffix-trim + warm-B
// canonical annotation).
func TestCanonicalThawPodName(t *testing.T) {
	create := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "m-p-r-agentX" + ThawPlaceholderSuffix}}
	if got := canonicalThawPodName(create); got != "m-p-r-agentX" {
		t.Errorf("create-path canonical = %q, want %q", got, "m-p-r-agentX")
	}
	warm := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:        "warm-b-node7-abcde",
		Annotations: map[string]string{AnnotationCanonicalPod: "m-p-r-agentY"},
	}}
	if got := canonicalThawPodName(warm); got != "m-p-r-agentY" {
		t.Errorf("warm-B canonical = %q, want %q", got, "m-p-r-agentY")
	}
}
