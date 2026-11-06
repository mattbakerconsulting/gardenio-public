// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

package podmanager

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// freezethaw_thaw_token_danglingsource_test.go pins the fail-LOUD rc=128 path for the
// SA/IRSA token mount-source force end to end (gd-6Rk1pGuETU), and the keyed-but-bsrc-
// missing degraded sub-case (gd-YE5vVlnyuz). Both exercise the `elif d in _SA_TOKEN_DIRS:`
// forced branch (freezethaw_node.go) against warm-B fixtures and run the ACTUAL rendered
// shell, complementing the pure-rewrite guards in freezethaw_thaw_token_source_test.go.
//
// WHY THIS MATTERS. The forced branch REFUSES to fall through to the ephemeral bsrc[d]
// (Pod-B live dir that [4] deletes) or the A->B substring rewrite. When a token dir is
// NOT in the gofer manifest (warm-copy miss / older snapshot), the branch deliberately
// LEAVES the mount source as the frozen Pod-A value (GC'd at freeze), so the [3a]
// dangling-source guard fails LOUD by name (FT_ROOT_RC=128) rather than binding a trap
// that surfaces later as an opaque restored-sandbox ENOENT. gd-6Rk1pGuETU pins that the
// dangling source actually trips the guard (rc=128), not just that the rewrite left it.

// extractDanglingSourceGuardBlock slices the [3a] `for dstn in $SUBNAMES; do ... done`
// loop carrying the PYCHK dangling-source classifier out of the rendered thaw template,
// by anchoring on the PYCHK heredoc and walking out to the enclosing for/done. Mirrors
// extractTokenReadProbeBlock (the [3a-tok] extractor) so a refactor that renames/removes
// the guard fails LOUD (a clear RED) rather than letting the pin silently no-op.
func extractDanglingSourceGuardBlock(t *testing.T, script string) string {
	t.Helper()
	lines := strings.Split(script, "\n")
	pyIdx := -1
	for i, l := range lines {
		if strings.Contains(l, "PYCHK") {
			pyIdx = i
			break
		}
	}
	if pyIdx < 0 {
		t.Fatalf("[3a] dangling-source guard not found: no PYCHK heredoc in the rendered thaw template — the ka-0000004815 bind-source existence guard was renamed or removed")
	}
	forIdx := -1
	for i := pyIdx; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "for dstn in $SUBNAMES; do" {
			forIdx = i
			break
		}
	}
	if forIdx < 0 {
		t.Fatalf("[3a] guard malformed: no enclosing `for dstn in $SUBNAMES; do` above the PYCHK heredoc")
	}
	pyCloseIdx := -1
	for i := pyIdx + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "PYCHK" {
			pyCloseIdx = i
			break
		}
	}
	if pyCloseIdx < 0 {
		t.Fatalf("[3a] guard malformed: no PYCHK heredoc close")
	}
	doneIdx := -1
	for i := pyCloseIdx + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "done" {
			doneIdx = i
			break
		}
	}
	if doneIdx < 0 {
		t.Fatalf("[3a] guard malformed: no closing `done` after the PYCHK heredoc")
	}
	block := strings.Join(lines[forIdx:doneIdx+1], "\n")
	if !strings.Contains(block, "FT_ROOT_RC=128") {
		t.Fatalf("[3a] dangling-source guard no longer emits FT_ROOT_RC=128 — its fail-close was weakened/removed. Extracted block:\n%s", block)
	}
	return block
}

// runPyagentThenDanglingGuard runs the PYAGENT rewrite over the agent config, then the
// [3a] dangling-source guard over the rewritten config, under one bash harness with
// HMNT/log stubbed and SUBNAMES=agent. A trailing FT_ROOT_RC=0 marks the pass-through
// path (the guard did NOT fail-close). Returns the combined output + whether it exited
// non-zero (fail-closed).
func runPyagentThenDanglingGuard(t *testing.T, pyagentBlock, guardBlock string, args ...string) (out string, failedClosed bool) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not available: %v", err)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	// The PYAGENT block reads its args from stdin via `python3 - <args>`; run it as a
	// standalone python invocation first (exactly as runPyagent does), then feed the
	// rewritten config to the [3a] guard which reads $BUNDLE/$dstn/config.json.
	agentCfg := args[0]
	bundle := args[5]
	// Stage 1: run PYAGENT (rewrites agentCfg in place).
	_ = runPyagent(t, pyagentBlock, args...)
	// Stage 2: run the [3a] guard over the rewritten bundle.
	// log() echoes to stdout here (the live script routes it to the controller log)
	// so the guard's fail-loud diagnostic — which names the dangling destination<-source
	// via log, not echo — is observable in the captured output.
	harness := fmt.Sprintf(`set -uo pipefail
log() { echo "$@"; }
HMNT() { "$@"; }
BUNDLE=%s
SUBNAMES=agent
%s
echo "FT_ROOT_RC=0"
`, shellQuoteForTest(bundle), guardBlock)
	cmd := exec.Command("bash", "-c", harness)
	b, err := cmd.CombinedOutput()
	_ = agentCfg
	return string(b), err != nil
}

// TestThawScript_UnkeyedTokenDanglingSourceFailsLoud is the gd-6Rk1pGuETU end-to-end pin.
// On a warm-B graft where a token dir is NOT in the gofer manifest (warm-copy miss / older
// snapshot) AND its frozen Pod-A source does NOT exist on disk (GC'd at freeze), the forced
// _SA_TOKEN_DIRS branch must LEAVE the mount source as that dangling frozen value (no
// ephemeral bsrc[d] bind, no A->B substring rewrite) — and the [3a] dangling-source guard
// must then fail LOUD with FT_ROOT_RC=128 by name, rather than the trap surfacing later as
// an opaque restored-sandbox ENOENT / empty-mounts sub rc=128.
func TestThawScript_UnkeyedTokenDanglingSourceFailsLoud(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"), ThawStateDest: MountThawState,
	})
	pyagent := extractPyagentBlock(t, tw)
	guard := extractDanglingSourceGuardBlock(t, tw)

	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, ".gofer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	// EMPTY gofer manifest -> the token dirs are UNKEYED (no warm copy to bind).
	if err := os.WriteFile(filepath.Join(bundle, ".gofer", "manifest.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The checkpoint agent config: the two token mounts source from FROZEN Pod-A dirs
	// that DO NOT EXIST on disk (GC'd at freeze) — the dangling-source condition.
	const frozenEKS = "/podA/frozen/eks/serviceaccount" // never created
	const frozenSA = "/podA/frozen/sa/serviceaccount"   // never created
	cfg := map[string]any{"annotations": map[string]string{}, "mounts": []map[string]string{
		{"destination": "/var/run/secrets/eks.amazonaws.com/serviceaccount", "source": frozenEKS},
		{"destination": "/var/run/secrets/kubernetes.io/serviceaccount", "source": frozenSA},
	}}
	cfgBytes, _ := json.Marshal(cfg)
	agentCfg := filepath.Join(bundle, "agent", "config.json")
	if err := os.WriteFile(agentCfg, cfgBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	// No Pod-B live config (bcfg=""), so bsrc does not resolve either — the branch has
	// nothing to fall through to and MUST leave the dangling frozen source.
	out, failedClosed := runPyagentThenDanglingGuard(t, pyagent, guard,
		warmBPyagentArgs(agentCfg, bundle, "" /* no bcfg */, thawStateHostDir("agentY"))...)

	// First, the rewrite must have LEFT the dangling frozen source (not bound anything).
	if got := readMountSource(t, agentCfg, tokenDestEKS); got != frozenEKS {
		t.Fatalf("unkeyed token EKS source = %q, want the LEFT dangling frozen Pod-A source %q (the forced branch must NOT bind bsrc[d] or substring-rewrite when unkeyed)", got, frozenEKS)
	}
	// Then the [3a] guard must fail LOUD on that dangling source.
	if !failedClosed {
		t.Fatalf("[3a] guard did NOT fail-close on a dangling unkeyed token source — the fail-loud rc=128 path is broken; the trap would surface later as an opaque restored-sandbox ENOENT. Output:\n%s", out)
	}
	if !strings.Contains(out, "FT_ROOT_RC=128") {
		t.Fatalf("dangling token source must emit FT_ROOT_RC=128 by name (gd-6Rk1pGuETU / ka-0000004815), got:\n%s", out)
	}
	if strings.Contains(out, "FT_ROOT_RC=0") {
		t.Fatalf("[3a] guard reached the pass-through marker despite a dangling source — it did not fail-close:\n%s", out)
	}
	// The diagnostic must name the token destination<-source so a redrive localizes it.
	if !strings.Contains(out, tokenDestEKS) && !strings.Contains(out, frozenEKS) {
		t.Errorf("[3a] fatal diagnostic should name the dangling token destination/source; got:\n%s", out)
	}
}

// TestThawScript_KeyedTokenResolvedSourcePassesGuard is the complementary GREEN direction
// for gd-6Rk1pGuETU: when the token dirs ARE keyed (warm copy present, source resolves),
// the [3a] guard must PASS (FT_ROOT_RC=0, no fail-close) — proving the rc=128 in the
// dangling case is caused specifically by the missing source, not an unconditional trip.
func TestThawScript_KeyedTokenResolvedSourcePassesGuard(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"), ThawStateDest: MountThawState,
	})
	pyagent := extractPyagentBlock(t, tw)
	guard := extractDanglingSourceGuardBlock(t, tw)

	// Keyed + bsrc-resolving fixture: the token dirs bind to the warm copy under
	// $BUNDLE/.gofer, which exists on disk -> [3a] resolves.
	fx := newTokenFixture(t, true /*keyed*/, true /*bsrcResolves*/)
	out, failedClosed := runPyagentThenDanglingGuard(t, pyagent, guard,
		warmBPyagentArgs(fx.agentCfg, fx.bundle, fx.bcfgPath, thawStateHostDir("agentY"))...)

	if failedClosed || strings.Contains(out, "FT_ROOT_RC=128") {
		t.Fatalf("[3a] guard fail-closed on a KEYED token whose warm-copy source resolves — the guard must only trip on a genuinely dangling source. Output:\n%s", out)
	}
	if !strings.Contains(out, "FT_ROOT_RC=0") {
		t.Fatalf("keyed token did not reach the [3a] pass-through marker FT_ROOT_RC=0:\n%s", out)
	}
	// The bound source must be the warm copy (exists on disk), not the frozen dangling value.
	if got := readMountSource(t, fx.agentCfg, tokenDestEKS); !strings.HasPrefix(got, filepath.Join(fx.bundle, ".gofer")+"/") {
		t.Fatalf("keyed token EKS source = %q, want the warm copy under %s/.gofer/", got, fx.bundle)
	}
}
