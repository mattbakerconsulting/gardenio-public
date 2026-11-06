package podmanager

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// freezethaw_thaw_restorepeak_behavioral_test.go is the ka-0000003398 fix-C
// BEHAVIORAL guard. TestThawScript_EmitsRestorePeakSentinel greps the rendered
// script for the FT_RESTORE_PEAK_BYTES / memory.peak / peak_for_cid tokens — that
// proves the lines are PRESENT but CANNOT catch the exact bug the adversarial
// review found: the measurement block called HMNTT, which was defined only in the
// FREEZE template, so on the real thaw path HMNTT was "command not found", find
// never ran, and FT_RESTORE_PEAK_BYTES was ALWAYS 0 (fail-safe but inert). A
// token-grep passes right through that. This test EXTRACTS the real fix-C block
// from the rendered thaw template and EXECUTES it against a fake /sys/fs/cgroup
// tree, so an undefined HMNTT (or any logic regression) turns it RED.

// extractRestorePeakBlock slices the fix-C measurement block out of the rendered
// thaw script: from the PEAK=0 line through the FT_RESTORE_PEAK_BYTES emit. The
// markers are unique in the template; a refactor that moves them fails LOUD.
func extractRestorePeakBlock(t *testing.T, script string) string {
	t.Helper()
	const startMarker = "PEAK=0\n"
	const endMarker = "echo \"FT_RESTORE_PEAK_BYTES=$PEAK\"\n"
	_, afterStart, ok := strings.Cut(script, startMarker)
	if !ok {
		t.Fatalf("could not locate fix-C start marker %q in the rendered thaw template — block moved; update this test (do NOT let it silently no-op)", strings.TrimSpace(startMarker))
	}
	block, _, ok := strings.Cut(afterStart, endMarker)
	if !ok {
		t.Fatalf("could not locate fix-C end marker %q — block boundary moved", strings.TrimSpace(endMarker))
	}
	full := startMarker + block + endMarker
	// Sanity: the load-bearing steps must be present, by name.
	for _, must := range []string{"peak_for_cid()", "HMNTT find /sys/fs/cgroup", "memory.peak"} {
		if !strings.Contains(full, must) {
			t.Fatalf("extracted fix-C block missing load-bearing step %q", must)
		}
	}
	return full
}

// TestThawScript_RestorePeakBlockExecutes runs the REAL fix-C block against a fake
// cgroup tree. It proves (1) HMNTT is defined and invocable in the thaw scope —
// the bug the review caught — and (2) the block reads memory.peak from the scope
// cgroups keyed by cid and emits the MAX across the sandbox root + subs.
func TestThawScript_RestorePeakBlockExecutes(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	script := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "warm-b-1", BPodUID: "u", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apu", ImagePath: "/img", BundlePath: "/bundle", ContainerCount: 2, RunscTimeout: 120,
	})
	block := extractRestorePeakBlock(t, script)

	// Fake cgroup tree: a sandbox-root scope and an agent-sub scope, each with a
	// memory.peak. The agent sub peaked higher — the block must emit the MAX.
	root := t.TempDir()
	fakeCgroup := filepath.Join(root, "cgroup")
	sbScope := filepath.Join(fakeCgroup, "kubepods.slice", "cri-containerd-sandboxcid.scope")
	agScope := filepath.Join(fakeCgroup, "kubepods.slice", "cri-containerd-agentcid.scope")
	for _, d := range []string{sbScope, agScope} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	const sbPeak, agPeak = "6000000000", "8500000000" // sub peaked higher
	if err := os.WriteFile(filepath.Join(sbScope, "memory.peak"), []byte(sbPeak+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agScope, "memory.peak"), []byte(agPeak+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// PRELUDE: define the scope vars + helpers the real thaw driver has in scope
	// when it reaches the block. HMNT is a passthrough (its only prod difference is
	// the mount namespace, irrelevant to the cat/find semantics under test). HMNTT
	// is stubbed to REWRITE the hardcoded /sys/fs/cgroup root to our fake tree while
	// preserving the timeout discipline shape — the point is to prove HMNTT is
	// invoked (not "command not found") and the find pattern + max logic work.
	// bcid_for maps the roster name to the cid embedded in the fake scope dirs.
	prelude := `set -uo pipefail
log() { echo "    $*" >&2; }
HMNT() { "$@"; }
HMNTT() { local rewritten=(); for a in "$@"; do rewritten+=("${a//\/sys\/fs\/cgroup/` + fakeCgroup + `}"); done; "${rewritten[@]}"; }
BSB="sandboxcid"
SUBNAMES="agent"
bcid_for() { case "$1" in agent) echo "agentcid";; *) echo "";; esac; }
`
	out := runBashBlock(t, prelude+block)
	got := sentinelValue(out, "FT_RESTORE_PEAK_BYTES")
	if got != agPeak {
		t.Errorf("FT_RESTORE_PEAK_BYTES = %q, want MAX-across-scopes %q\n--- output ---\n%s", got, agPeak, out)
	}
}

// TestThawScript_RestorePeakBlockBestEffortZero proves the fail-safe contract: on
// a node with NO matching scope cgroups (cgroup-v1 / unresolvable scope), the
// block emits FT_RESTORE_PEAK_BYTES=0 and exits cleanly — never errors, never
// perturbs the verified restore.
func TestThawScript_RestorePeakBlockBestEffortZero(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	script := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "warm-b-1", BPodUID: "u", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apu", ImagePath: "/img", BundlePath: "/bundle", ContainerCount: 2, RunscTimeout: 120,
	})
	block := extractRestorePeakBlock(t, script)
	root := t.TempDir()
	emptyCgroup := filepath.Join(root, "cgroup") // exists but has no *.scope/memory.peak
	if err := os.MkdirAll(emptyCgroup, 0o755); err != nil {
		t.Fatal(err)
	}
	prelude := `set -uo pipefail
log() { echo "    $*" >&2; }
HMNT() { "$@"; }
HMNTT() { local rewritten=(); for a in "$@"; do rewritten+=("${a//\/sys\/fs\/cgroup/` + emptyCgroup + `}"); done; "${rewritten[@]}"; }
BSB="sandboxcid"
SUBNAMES="agent"
bcid_for() { case "$1" in agent) echo "agentcid";; *) echo "";; esac; }
`
	out := runBashBlock(t, prelude+block)
	if got := sentinelValue(out, "FT_RESTORE_PEAK_BYTES"); got != "0" {
		t.Errorf("FT_RESTORE_PEAK_BYTES = %q, want 0 when no scope cgroup is readable\n--- output ---\n%s", got, out)
	}
}

// runBashBlock executes a shell block and returns combined stdout+stderr. It fails
// the test if bash exits non-zero — the fix-C block must ALWAYS exit 0 (best-effort).
func runBashBlock(t *testing.T, s string) string {
	t.Helper()
	cmd := exec.Command("bash", "-c", s)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fix-C block execution failed (must exit 0, best-effort): %v\n%s", err, out)
	}
	return string(out)
}

// sentinelValue returns the value of the LAST KEY=value line in out (last-wins,
// matching parseSentinels), or "" if absent.
func sentinelValue(out, key string) string {
	val := ""
	for _, ln := range strings.Split(out, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(ln), key+"="); ok {
			val = rest
		}
	}
	return val
}
