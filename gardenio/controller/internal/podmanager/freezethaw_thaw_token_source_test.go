// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

package podmanager

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// freezethaw_thaw_token_source_test.go is the ka-0000008074 / gd-GNzj6raaao RED-BAR
// guard for the projected SA/IRSA token mount-SOURCE force — the SAME fix class as the
// a61f450fb / ka-0000000673 thaw-state source re-point (freezethaw_thaw_source_repoint_test.go).
//
// THE BUG (instrumented canary #7, prod 2026.180.27; FreshnessError::Missing). The two
// projected serviceaccount token mounts (aws-iam-token @ /run/secrets/eks.amazonaws.com/
// serviceaccount, kube-api-access @ /run/secrets/kubernetes.io/serviceaccount) are
// DIRECTORY mounts under the kubelet pod dir. The merged ka-0000008074 warm-copy +
// ka-0000004956 overlay pin them to the controller-owned warm copy ($BUNDLE/.gofer/<n>)
// — but ONLY on the `elif d in gofer:` branch, i.e. only when the freeze sweep actually
// keyed them. On ANY path where a token dir is ABSENT from the gofer manifest (a
// best-effort copytree skip at freeze, or a snapshot frozen by an older controller before
// the sweep landed), the thaw rewrite fell THROUGH to `elif d in bsrc and bsrc[d]:` and
// bound the token mount SOURCE to Pod B's LIVE projected dir under
// /var/lib/kubelet/pods/<podB>/.../kubernetes.io~projected/...
//
// That live source RESOLVES at the [3a] dangling-source guard (Pod B is alive until [4]),
// so the guard passes — but [4] FORCE-DELETES Pod B BEFORE the [5] count-gated restore
// (freezethaw_node.go: "[4] tear down B's live original containers"). The kubelet then
// reclaims Pod B's projected volumes and the bound source dir VANISHES, so the restored
// sandbox sees the serviceaccount DIRECTORY itself ENOENT -> coop's CredentialReminter
// fails closed with FreshnessError::Missing ("token file not found: .../serviceaccount/
// token"), NOT PodAIdentity. Exactly the canary #7 forensic.
//
// THE FIX. A dedicated `elif d in _SA_TOKEN_DIRS:` forced-source branch, HIGHEST
// precedence after the thaw-state force, that binds the token mounts ONLY to the stable
// controller-owned warm copy (which survives [4]) and applies the Pod-B cred-overlay —
// and REFUSES to fall through to the ephemeral bsrc[d] or the A->B substring rewrite.
// When the warm copy is absent, the source is left as the (GC'd) frozen Pod-A value so
// the [3a] guard fails LOUD by name rather than silently binding the [4]-teardown trap.
//
//	REAL in the behavioral tests: the ACTUAL PYAGENT block extracted verbatim from the
//	                              rendered thaw template, run by python3 against a warm-B
//	                              cross-pod token fixture on the test tmpfs.

// tokenFixture builds a warm-B cross-pod token fixture under root and returns the paths
// the assertions need. It writes:
//   - the CHECKPOINT agent config (frozen Pod A) with the two token mounts (raw /var/run
//     dests, sources = Pod A's projected dirs),
//   - Pod A's projected token dirs (the frozen, GC-at-freeze sources),
//   - Pod B's LIVE projected token dirs (the ephemeral bsrc sources [4] deletes),
//   - the Pod B live bundle config (bcfg) naming Pod B's sources by destination,
//   - optionally the warm copies in $BUNDLE/.gofer/<n> + manifest (keyed=true).
type tokenFixture struct {
	agentCfg string // the checkpoint agent config.json the PYAGENT block rewrites
	bundle   string // $BUNDLE (holds .gofer warm copies + manifest)
	bcfgPath string // Pod B live bundle config ("" => the empty-bcfg fallback)
	bEKS     string // Pod B's LIVE eks projected dir (ephemeral; [4] deletes it)
	bSA      string // Pod B's LIVE sa projected dir (ephemeral; [4] deletes it)
	aEKS     string // Pod A's FROZEN eks projected dir (GC'd at freeze)
	aSA      string // Pod A's FROZEN sa projected dir (GC'd at freeze)
}

const (
	tokenDestEKS = "/run/secrets/eks.amazonaws.com/serviceaccount"
	tokenDestSA  = "/run/secrets/kubernetes.io/serviceaccount"
)

func newTokenFixture(t *testing.T, keyed, bsrcResolves bool) tokenFixture {
	t.Helper()
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, ".gofer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}

	mk := func(p, tok string) string {
		t.Helper()
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "token"), []byte(tok), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Pod A's frozen projected dirs (the checkpoint sources; GC'd at freeze on the node).
	aEKS := mk(filepath.Join(root, "podA", "aws-iam-token", "serviceaccount"), "POD_A_IRSA")
	aSA := mk(filepath.Join(root, "podA", "kube-api-access", "serviceaccount"), "POD_A_SA")
	// Pod B's LIVE projected dirs (the ephemeral bsrc sources [4] force-deletes).
	bEKS := mk(filepath.Join(root, "podB", "aws-iam-token", "serviceaccount"), "POD_B_IRSA")
	bSA := mk(filepath.Join(root, "podB", "kube-api-access", "serviceaccount"), "POD_B_SA")

	// Optional warm copies (controller-owned; survive [4]) + manifest. The warm copy
	// is written in the REAL kubelet atomic-writer symlink layout (token -> ..data/token
	// -> ..<ts>/token) that the on-node freeze copytree preserves and the cred-overlay
	// writes THROUGH (gd-Ozsn5mtLG7) — not a flat token file — so the keyed happy path
	// exercises the overlay's realpath-follow + the ..data/token sibling write, exactly
	// as prod does. 0640 backing = the post-chown readable state.
	manifest := map[string]string{}
	if keyed {
		for i, dst := range []string{tokenDestEKS, tokenDestSA} {
			n := string(rune('0' + i))
			warm := filepath.Join(bundle, ".gofer", n)
			tok := "WARM_POD_A_IRSA"
			if i == 1 {
				tok = "WARM_POD_A_SA"
			}
			writeWarmAtomicToken(t, warm, tok, 0o640)
			manifest[dst] = n        // /run form
			manifest["/var"+dst] = n // /var/run sibling (dual-key, ka-0000007745)
		}
	}
	mb, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(bundle, ".gofer", "manifest.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}

	// The checkpoint agent config: raw /var/run dests, sources = Pod A's frozen dirs.
	cfg := map[string]any{"annotations": map[string]string{}, "mounts": []map[string]string{
		{"destination": "/var/run/secrets/eks.amazonaws.com/serviceaccount", "source": aEKS},
		{"destination": "/var/run/secrets/kubernetes.io/serviceaccount", "source": aSA},
	}}
	cfgBytes, _ := json.Marshal(cfg)
	agentCfg := filepath.Join(bundle, "agent", "config.json")
	if err := os.WriteFile(agentCfg, cfgBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	bcfgPath := ""
	if bsrcResolves {
		bcfg := map[string]any{"mounts": []map[string]string{
			{"destination": "/var/run/secrets/eks.amazonaws.com/serviceaccount", "source": bEKS},
			{"destination": "/var/run/secrets/kubernetes.io/serviceaccount", "source": bSA},
		}}
		bb, _ := json.Marshal(bcfg)
		bcfgPath = filepath.Join(root, "bcfg.json")
		if err := os.WriteFile(bcfgPath, bb, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return tokenFixture{
		agentCfg: agentCfg, bundle: bundle, bcfgPath: bcfgPath,
		bEKS: bEKS, bSA: bSA, aEKS: aEKS, aSA: aSA,
	}
}

func tokenPyagentBlock(t *testing.T) string {
	t.Helper()
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	return extractPyagentBlock(t, m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"), ThawStateDest: MountThawState,
	}))
}

// TestThawScript_TokenSourceNeverBindsEphemeralPodBDir is the ka-0000008074 RED-BAR
// behavioral guard. On a warm-B graft where the token dirs are NOT in the gofer manifest
// (warm-copy miss / older snapshot) but Pod B's live config resolves, the PRE-FIX rewrite
// fell through to `elif d in bsrc and bsrc[d]:` and bound the token mount SOURCE to Pod
// B's LIVE projected dir — which [4] force-deletes before the [5] restore, so the restored
// sandbox sees the serviceaccount DIRECTORY ENOENT (FreshnessError::Missing, canary #7).
// The forced `elif d in _SA_TOKEN_DIRS:` branch refuses that fall-through.
func TestThawScript_TokenSourceNeverBindsEphemeralPodBDir(t *testing.T) {
	block := tokenPyagentBlock(t)
	fx := newTokenFixture(t, false /*keyed*/, true /*bsrcResolves*/)
	runPyagent(t, block, warmBPyagentArgs(fx.agentCfg, fx.bundle, fx.bcfgPath, thawStateHostDir("agentY"))...)

	for _, tc := range []struct {
		dest, ephemeral string
	}{
		{tokenDestEKS, fx.bEKS},
		{tokenDestSA, fx.bSA},
	} {
		got := readMountSource(t, fx.agentCfg, tc.dest)
		if got == tc.ephemeral {
			t.Fatalf("token mount %q SOURCE was bound to Pod B's LIVE projected dir %q.\n"+
				"That source resolves at [3a] (Pod B alive) but [4] force-deletes Pod B BEFORE the [5] restore,\n"+
				"so the kubelet reclaims it and the restored sandbox sees the serviceaccount DIRECTORY ENOENT ->\n"+
				"coop FreshnessError::Missing 'token file not found' (canary #7, ka-0000008074). The forced\n"+
				"_SA_TOKEN_DIRS branch must refuse the ephemeral bsrc[d] fall-through.", tc.dest, got)
		}
	}
}

// TestThawScript_TokenSourceForcedToWarmCopyOnWarmBGraft is the keyed happy-path guard:
// when the token dirs ARE in the gofer manifest, the forced branch binds them to the
// stable controller-owned warm copy ($BUNDLE/.gofer/<n>, surviving [4]) AND overlays Pod
// B's LIVE token bytes so coop's freshness_gate takes the pod_uid==Pod-B arm. The source
// must be the warm copy (NOT the ephemeral Pod-B dir), and the served token must read the
// Pod-B bytes (NOT the frozen Pod-A bytes -> avoid PodAIdentity).
func TestThawScript_TokenSourceForcedToWarmCopyOnWarmBGraft(t *testing.T) {
	block := tokenPyagentBlock(t)
	fx := newTokenFixture(t, true /*keyed*/, true /*bsrcResolves*/)
	runPyagent(t, block, warmBPyagentArgs(fx.agentCfg, fx.bundle, fx.bcfgPath, thawStateHostDir("agentY"))...)

	for _, tc := range []struct {
		dest, ephemeral, wantTok string
	}{
		{tokenDestEKS, fx.bEKS, "POD_B_IRSA"},
		{tokenDestSA, fx.bSA, "POD_B_SA"},
	} {
		got := readMountSource(t, fx.agentCfg, tc.dest)
		// Must be the stable warm copy under $BUNDLE/.gofer, NOT the ephemeral Pod-B dir.
		if got == tc.ephemeral {
			t.Fatalf("token %q bound to ephemeral Pod-B dir %q instead of the stable warm copy", tc.dest, got)
		}
		if !strings.HasPrefix(got, filepath.Join(fx.bundle, ".gofer")+"/") {
			t.Fatalf("token %q source = %q, want the controller-owned warm copy under %s/.gofer/", tc.dest, got, fx.bundle)
		}
		// The served token must carry Pod B's bytes (overlay applied), NOT frozen Pod A.
		// The warm copy is the kubelet atomic-writer layout (token -> ..data/token ->
		// ..<ts>/token, gd-Ozsn5mtLG7), so reading `token` follows the symlink chain to
		// the backing file exactly as the restored coop does.
		tb, err := os.ReadFile(filepath.Join(got, "token"))
		if err != nil {
			t.Fatalf("warm copy token for %q does not resolve (the restored-sandbox ENOENT): %v", tc.dest, err)
		}
		if string(tb) != tc.wantTok {
			t.Fatalf("token %q served bytes = %q, want Pod-B %q (cred-overlay must write Pod-B's live token so coop's gate takes pod_uid==Pod-B, not PodAIdentity)", tc.dest, tb, tc.wantTok)
		}
		// The overlay must write THROUGH the whole atomic-writer layout: the ..data/token
		// sibling AND the resolved ..<ts>/token backing file both carry the Pod-B bytes.
		// A regression that wrote only the top-level `token` (turning it into a plain file
		// or leaving ..data/token stale) would be invisible to a flat-file fixture but is
		// what the on-node kubelet layout actually exposes to coop.
		dd, err := os.ReadFile(filepath.Join(got, "..data", "token"))
		if err != nil {
			t.Fatalf("token %q ..data/token sibling does not resolve — the warm copy is not the atomic-writer layout the overlay writes through: %v", tc.dest, err)
		}
		if string(dd) != tc.wantTok {
			t.Fatalf("token %q ..data/token = %q, want Pod-B %q (the overlay must write the ..data/token sibling too)", tc.dest, dd, tc.wantTok)
		}
		backing, err := filepath.EvalSymlinks(filepath.Join(got, "token"))
		if err != nil {
			t.Fatalf("token %q backing file (..<ts>/token) does not resolve through the symlink chain: %v", tc.dest, err)
		}
		if !strings.Contains(backing, "/..") {
			t.Fatalf("token %q resolved to %q; expected the kubelet atomic-writer backing ..<ts>/token (the fixture must model the real symlink layout, not a flat file)", tc.dest, backing)
		}
		bb, err := os.ReadFile(backing)
		if err != nil {
			t.Fatalf("token %q backing file %q unreadable: %v", tc.dest, backing, err)
		}
		if string(bb) != tc.wantTok {
			t.Fatalf("token %q backing file %q = %q, want Pod-B %q (the overlay must land the bytes on the realpath-followed backing file coop reads)", tc.dest, backing, bb, tc.wantTok)
		}
	}
}

// TestThawScript_TokenKeyedButBsrcMissingDegradesToPodAIdentity is the gd-YE5vVlnyuz
// guard for the keyed-but-bsrc-missing DEGRADED sub-case. When a token dir IS in the
// gofer manifest (warm copy present) but Pod B's LIVE source does NOT resolve (no live
// bcfg for it — an older-snapshot / pre-Ready-graft / lost-live-config path), the forced
// _SA_TOKEN_DIRS branch binds the mount to the stable warm copy (correct — survives [4],
// so restore is safe) but the `if d in bsrc and bsrc[d]:` cred-overlay guard is FALSE, so
// NO Pod-B bytes are written. The warm copy then serves the FROZEN Pod-A token.
//
// This must degrade HONESTLY, not silent-pass: the served token is genuinely the Pod-A
// one (its kubernetes.io.pod.uid is Pod A's), so the restored coop's freshness_gate
// correctly takes the FreshnessError::PodAIdentity arm and fails the thaw CLOSED — rather
// than the code masking the missing overlay and serving something that looks fresh. The
// test pins: (1) the source is the warm copy (NOT the ephemeral Pod-B dir, NOT a dangling
// frozen source), and (2) the served token is the Pod-A/warm bytes, NOT Pod-B's — i.e. the
// overlay did NOT silently fabricate a Pod-B identity when it had no live source to read.
func TestThawScript_TokenKeyedButBsrcMissingDegradesToPodAIdentity(t *testing.T) {
	block := tokenPyagentBlock(t)
	// keyed=true (warm copy present), bsrcResolves=false (no live Pod-B bcfg -> bsrc empty).
	fx := newTokenFixture(t, true /*keyed*/, false /*bsrcResolves*/)
	runPyagent(t, block, warmBPyagentArgs(fx.agentCfg, fx.bundle, fx.bcfgPath, thawStateHostDir("agentY"))...)

	for _, tc := range []struct {
		dest, ephemeral, warmTok, podBTok string
	}{
		{tokenDestEKS, fx.bEKS, "WARM_POD_A_IRSA", "POD_B_IRSA"},
		{tokenDestSA, fx.bSA, "WARM_POD_A_SA", "POD_B_SA"},
	} {
		got := readMountSource(t, fx.agentCfg, tc.dest)
		// (1) Source must be the stable warm copy — NOT the ephemeral Pod-B dir [4] deletes,
		// NOT a dangling frozen Pod-A source. Binding the warm copy keeps the restore safe
		// (it survives [4]); the identity mismatch is coop's job to reject, not the mount's.
		if got == tc.ephemeral {
			t.Fatalf("keyed-but-bsrc-missing token %q bound to the ephemeral Pod-B dir %q — [4] deletes it -> restored-sandbox ENOENT; the forced branch must still bind the stable warm copy even with no live source to overlay", tc.dest, got)
		}
		if !strings.HasPrefix(got, filepath.Join(fx.bundle, ".gofer")+"/") {
			t.Fatalf("keyed-but-bsrc-missing token %q source = %q, want the controller-owned warm copy under %s/.gofer/", tc.dest, got, fx.bundle)
		}
		// (2) The served token must be the FROZEN Pod-A/warm bytes — the overlay had no live
		// bsrc to read, so it must NOT have fabricated or served Pod-B bytes. This is the
		// "PodAIdentity, not silent pass" contract: coop will see Pod-A's pod_uid and fail
		// closed with FreshnessError::PodAIdentity, which is the CORRECT degraded behavior.
		tb, err := os.ReadFile(filepath.Join(got, "token"))
		if err != nil {
			t.Fatalf("warm copy token for %q does not resolve: %v", tc.dest, err)
		}
		if string(tb) == tc.podBTok {
			t.Fatalf("keyed-but-bsrc-missing token %q served Pod-B bytes %q with NO live bsrc to overlay from — a silent pass that fabricates a fresh identity. It must serve the frozen Pod-A/warm token so coop honestly hits FreshnessError::PodAIdentity", tc.dest, tc.podBTok)
		}
		if string(tb) != tc.warmTok {
			t.Fatalf("keyed-but-bsrc-missing token %q served bytes = %q, want the frozen warm Pod-A token %q (no overlay applied when bsrc is missing)", tc.dest, tb, tc.warmTok)
		}
	}
}

// TestThawScript_TokenForcedBranchIsDestinationScoped proves the forced branch touches
// ONLY the two _SA_TOKEN_DIRS — a non-token mount that happens to be in the gofer manifest
// keeps its generic warm-copy resolution, and a /work mount keeps its dedicated source.
func TestThawScript_TokenForcedBranchIsDestinationScoped(t *testing.T) {
	block := tokenPyagentBlock(t)
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, ".gofer", "9"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundle, ".gofer", "9", "session.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// gofer keys a NON-token runtime tree (~/.claude) only.
	manifest := map[string]string{"/home/agent/.claude": "9", "/var/home/agent/.claude": "9"}
	mb, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(bundle, ".gofer", "manifest.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}
	const podBWork = "/var/lib/kubelet/pods/BPODUID/volumes/kubernetes.io~empty-dir/work"
	cfg := map[string]any{"annotations": map[string]string{}, "mounts": []map[string]string{
		{"destination": "/home/agent/.claude", "source": "/some/frozen/claude"},
		{"destination": "/work", "source": "/var/lib/kubelet/pods/APODUID/volumes/kubernetes.io~empty-dir/work"},
	}}
	cfgBytes, _ := json.Marshal(cfg)
	agentCfg := filepath.Join(bundle, "agent", "config.json")
	if err := os.WriteFile(agentCfg, cfgBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	bcfg := map[string]any{"mounts": []map[string]string{
		{"destination": "/home/agent/.claude", "source": "/podB/live/claude"},
		{"destination": "/work", "source": podBWork},
	}}
	bb, _ := json.Marshal(bcfg)
	bcfgPath := filepath.Join(root, "bcfg.json")
	if err := os.WriteFile(bcfgPath, bb, 0o644); err != nil {
		t.Fatal(err)
	}
	runPyagent(t, block, warmBPyagentArgs(agentCfg, bundle, bcfgPath, thawStateHostDir("agentY"))...)

	// ~/.claude is a gofer-keyed NON-token tree -> generic warm-copy branch, source ends
	// up the $BUNDLE/.gofer/9 warm copy (not the forced-token path, not bsrc).
	claude := readMountSource(t, agentCfg, "/home/agent/.claude")
	if claude != filepath.Join(bundle, ".gofer", "9") {
		t.Fatalf("/home/agent/.claude source = %q, want generic warm copy %s/.gofer/9 (the token force must not perturb other gofer mounts)", claude, bundle)
	}
	// /work keeps its dedicated warm copy.
	work := readMountSource(t, agentCfg, "/work")
	if !strings.HasSuffix(work, "/.work") {
		t.Fatalf("/work source = %q, want the dedicated warm copy $BUNDLE/.work", work)
	}
}

// TestThawScript_TokenForcedBranchPrecedesGenericBranches is the render-order guard: the
// `elif d in _SA_TOKEN_DIRS:` branch must sit BETWEEN the thaw-state force and the generic
// `elif d=="/work":` / `elif d in gofer:` / `elif d in bsrc:` branches, so the token dirs
// take the forced source and never the generic bsrc[d] fall-through. A re-order silently
// reintroduces the ka-0000008074 ephemeral-bind regression, which the behavioral tests
// (run on a fixture) cannot localize to ordering.
func TestThawScript_TokenForcedBranchPrecedesGenericBranches(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"), ThawStateDest: MountThawState,
	})
	iThaw := strings.Index(tw, `if thawdir and d==thawdest:`)
	iToken := strings.Index(tw, `elif d in _SA_TOKEN_DIRS:`)
	iWork := strings.Index(tw, `elif d=="/work":`)
	iGofer := strings.Index(tw, `elif d in gofer:`)
	iBsrc := strings.Index(tw, `elif d in bsrc and bsrc[d]:`)
	if iThaw < 0 || iToken < 0 || iWork < 0 || iGofer < 0 || iBsrc < 0 {
		t.Fatalf("missing a branch marker (thaw=%d token=%d work=%d gofer=%d bsrc=%d)", iThaw, iToken, iWork, iGofer, iBsrc)
	}
	if !(iThaw < iToken && iToken < iWork && iWork < iGofer && iGofer < iBsrc) {
		t.Fatalf("branch order wrong: the _SA_TOKEN_DIRS force must sit after the thaw-state force and BEFORE the generic /work/gofer/bsrc branches (thaw=%d token=%d work=%d gofer=%d bsrc=%d)", iThaw, iToken, iWork, iGofer, iBsrc)
	}
	// The forced branch must NOT bind tokens to the live Pod-B source: no `m["source"]=bsrc[d]`
	// inside the _SA_TOKEN_DIRS branch (it may overlay bsrc[d]/token bytes, but never bind it
	// as the mount source).
	tokenBranch := tw[iToken:iWork]
	if strings.Contains(tokenBranch, `m["source"]=bsrc[d]`) {
		t.Fatalf("the _SA_TOKEN_DIRS forced branch must NOT bind the mount source to the ephemeral bsrc[d] (Pod-B live dir [4] deletes); it may only overlay bsrc[d]/token BYTES onto the warm copy")
	}
}
