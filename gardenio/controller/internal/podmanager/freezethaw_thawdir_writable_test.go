package podmanager

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// freezethaw_thawdir_writable_test.go — BEHAVIORAL regression guard for the [2c]
// thaw-state THAWDIR pre-create's fail-loud agent-writability assertion in
// freezethaw_node.go's thaw template (task ka-0000004248, robustness follow-up of
// MR !5003 / ka-0000001104, parent epic ka-0000000867).
//
// WHY THIS EXISTS — the gap it closes
// -----------------------------------
// The [2c] pre-create chowns/chmods the forced MountThawState source ($THAWDIR) so
// the restored coop/gb watchers (UID/GID 1000) can write their .done acks. Before
// this fix the chown/chmod ran with a bare `|| true`, so a SILENT failure (a
// kubelet-pre-provisioned root-owned DirectoryOrCreate dir, a read-only fs, an
// immutable bit) left the dir non-agent-writable. The restore then proceeded, the
// agent's ack EACCESed through the mount, and the both-consumer rehydrate gate
// fail-closed ~180s later as an OPAQUE timeout — re-tripping the very defect
// ka-0000000673/ka-0000001104 just fixed.
//
// The fix replaces `|| true` with a WARN-on-failure log AND a [2c-assert]
// fail-closed writability guard: it stats $THAWDIR in the host ns and, unless the
// agent (UID/GID 1000) can create a file there (owner/group/other write+search),
// emits FT_ROOT_RC=128 + exit 1 BEFORE the [3a] guard — diagnosing the silent
// chown/chmod failure by owner/group/mode instead of as a downstream gate timeout.
//
// A pure string-presence test (the sibling TestThawScript_PrecreatesThawStateDir)
// would still pass if a refactor weakened the case-condition or dropped the exit,
// so this test EXTRACTS the actual [2c] `if [ -n "$THAWDIR" ]` block out of the
// rendered template and RUNS it under bash against synthetic THAWDIR fixtures of
// varying mode, asserting the real fail-close-vs-pass-through behavior.

// extractThawdirPrecreateBlock slices the [2c] `if [ -n "$THAWDIR" ]; then ... fi`
// block out of the rendered thaw script by if/fi depth counting, anchored on the
// exact opener. A refactor that renames/removes the block makes extraction fail
// LOUD (a clear RED) rather than letting the guard silently no-op.
func extractThawdirPrecreateBlock(t *testing.T, script string) string {
	t.Helper()
	lines := strings.Split(script, "\n")

	ifIdx := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == `if [ -n "$THAWDIR" ]; then` {
			ifIdx = i
			break
		}
	}
	if ifIdx < 0 {
		t.Fatalf("[2c] THAWDIR pre-create block not found: no `if [ -n \"$THAWDIR\" ]; then` line in the rendered thaw template — the pre-create was renamed or removed (update this test to track it, or the block was wrongly deleted)")
	}

	// Find the matching `fi` by depth counting so the embedded `case`/heredoc and
	// any future nested conditional cannot truncate the extraction. The PYTWPERM
	// heredoc body contains no leading-`if `/`fi` shell lines, so it is depth-inert.
	fiIdx, depth := -1, 0
	for i := ifIdx; i < len(lines); i++ {
		tl := strings.TrimSpace(lines[i])
		if tl == "if" || strings.HasPrefix(tl, "if ") {
			depth++
		}
		if tl == "fi" {
			depth--
			if depth == 0 {
				fiIdx = i
				break
			}
		}
	}
	if fiIdx < 0 {
		t.Fatalf("[2c] THAWDIR pre-create block malformed: no matching `fi` for the opener at line %d", ifIdx+1)
	}

	block := strings.Join(lines[ifIdx:fiIdx+1], "\n")
	// Sanity: the extracted block must carry the fail-close path. If a divergent
	// edit kept the opener but dropped the assertion's FT_ROOT_RC=128 / exit, the
	// behavioral run below would catch it too, but assert here for a precise message.
	if !strings.Contains(block, "FT_ROOT_RC=128") {
		t.Fatalf("[2c] THAWDIR pre-create no longer emits FT_ROOT_RC=128 — its agent-writability fail-close was weakened/removed (the '|| true' silent-swallow regression). Extracted block:\n%s", block)
	}
	if !strings.Contains(block, "PYTWPERM") {
		t.Fatalf("[2c] THAWDIR pre-create no longer carries the PYTWPERM writability classifier — the assertion was removed. Extracted block:\n%s", block)
	}
	return block
}

// runThawdirPrecreate executes the extracted [2c] block under bash with HMNT and
// log stubbed, $THAWDIR set to the fixture dir, and a trailing FT_ROOT_RC=0 marker
// for the pass-through path. HMNT drops the host-ns-enter prefix and runs its argv
// directly against the local fixture.
//
// simulateChownChmodFail models the DEFECT this fix guards: in production [2c]
// runs as root (HMNT nsenter -t 1 -m), so chown/chmod normally succeed; but on a
// read-only fs / immutable bit / kubelet-pre-provisioned root-owned dir they fail
// SILENTLY, leaving the dir in its non-agent-writable mode. The unprivileged test
// runner OWNS its fixture dirs, so a real chmod would always succeed and widen
// them — masking the defect. When simulateChownChmodFail is true the HMNT stub
// makes chown/chmod return non-zero WITHOUT acting (faithfully preserving the
// fixture's provisioned mode), so the [2c-assert] classifier sees the real
// non-writable state. When false it models the healthy path (chown/chmod run).
func runThawdirPrecreate(t *testing.T, block, thawdir string, simulateChownChmodFail bool) (out string, failedClosed bool) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not available: %v", err)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	hmnt := `HMNT() { "$@"; }` // healthy path: run argv directly (chown/chmod succeed)
	if simulateChownChmodFail {
		// Defect path: chown/chmod are silently denied (RO fs / immutable / cross-ns),
		// every other op (mkdir, python3) still runs against the fixture.
		hmnt = `HMNT() { case "$1" in chown|chmod) return 1 ;; *) "$@" ;; esac; }`
	}
	harness := fmt.Sprintf(`set -uo pipefail
log() { :; }            # stub the thaw template's logger
%s
THAWDIR=%s
%s
echo "FT_ROOT_RC=0"     # fall-through: the [2c-assert] writability guard did NOT fail-close
`, hmnt, shellQuoteForTest(thawdir), rewriteAgentIDsForRunner(block))

	cmd := exec.Command("bash", "-c", harness)
	b, err := cmd.CombinedOutput()
	return string(b), err != nil
}

// shellQuoteForTest single-quotes a path for the bash harness. Test fixtures are
// t.TempDir() paths (no single quotes), so simple wrapping suffices.
func shellQuoteForTest(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// TestThawScript_ThawdirPrecreateFailsLoudOnNonWritable is the core ka-0000004248
// behavioral regression: it runs the LIVE extracted [2c] block over THAWDIR
// fixtures of varying mode and asserts the fail-close-vs-pass-through contract.
// The test runner is UID/GID 1000 (the agent UID), so a 0775 dir it creates is
// owner+group-writable -> OK; a 0555 dir is non-writable -> the silent-chown-
// failure case the guard must catch -> FT_ROOT_RC=128 + exit.
func TestThawScript_ThawdirPrecreateFailsLoudOnNonWritable(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractThawdirPrecreateBlock(t, m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"),
	}))

	t.Run("agent-writable dir passes through", func(t *testing.T) {
		// A 0775 dir the UID/GID-1000 runner owns: owner+group write+search -> OK.
		// Mirrors the healthy [2c] path where chown 1000:1000 + chmod 0775 took.
		dir := filepath.Join(t.TempDir(), "thaw")
		if err := os.Mkdir(dir, 0o775); err != nil {
			t.Fatalf("mkdir writable fixture: %v", err)
		}
		// Healthy path: chown/chmod run (as they do as root in prod) -> dir is
		// agent-writable -> pass through.
		out, failedClosed := runThawdirPrecreate(t, block, dir, false)
		if failedClosed {
			t.Errorf("agent-writable THAWDIR must NOT fail-close, but it did:\n%s", out)
		}
		if strings.Contains(out, "FT_ROOT_RC=128") {
			t.Errorf("agent-writable THAWDIR emitted the fail-close sentinel FT_ROOT_RC=128:\n%s", out)
		}
		if !strings.Contains(out, "FT_ROOT_RC=0") {
			t.Errorf("agent-writable THAWDIR did not reach the pass-through marker FT_ROOT_RC=0:\n%s", out)
		}
	})

	t.Run("non-group-writable root-style dir fails loud", func(t *testing.T) {
		// A 0555 dir: no write bit for owner/group/other -> the agent (UID/GID 1000)
		// cannot create its .done ack. This is exactly the silently-failed-chown
		// state (e.g. a kubelet-pre-provisioned root-owned DirectoryOrCreate dir).
		// simulateChownChmodFail=true keeps the in-block chown/chmod from widening
		// the dir (as a RO fs / immutable bit / cross-ns denial would), so the
		// [2c-assert] guard sees the real non-writable state and must fail-close.
		dir := filepath.Join(t.TempDir(), "thaw")
		if err := os.Mkdir(dir, 0o555); err != nil {
			t.Fatalf("mkdir non-writable fixture: %v", err)
		}
		// Belt-and-suspenders: force the mode back down in case umask/mkdir widened it.
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatalf("chmod non-writable fixture: %v", err)
		}
		out, failedClosed := runThawdirPrecreate(t, block, dir, true)
		if !failedClosed {
			t.Errorf("non-writable THAWDIR must fail-close (exit non-zero) — the '|| true' silent-swallow regression. Output:\n%s", out)
		}
		if !strings.Contains(out, "FT_ROOT_RC=128") {
			t.Errorf("non-writable THAWDIR must emit FT_ROOT_RC=128, got:\n%s", out)
		}
		if strings.Contains(out, "FT_ROOT_RC=0") {
			t.Errorf("non-writable THAWDIR reached the pass-through marker — the guard did not fail-close:\n%s", out)
		}
	})

	t.Run("missing dir fails loud", func(t *testing.T) {
		// If even the mkdir -p could not materialize the dir (the HMNT mkdir stderr
		// is swallowed), the classifier stats a MISSING dir -> not OK -> fail-close.
		// Simulate by pointing THAWDIR at a path under a read-only parent so the
		// in-block mkdir -p cannot create it.
		parent := filepath.Join(t.TempDir(), "ro")
		if err := os.Mkdir(parent, 0o555); err != nil {
			t.Fatalf("mkdir read-only parent: %v", err)
		}
		if err := os.Chmod(parent, 0o555); err != nil {
			t.Fatalf("chmod read-only parent: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(parent, 0o755) }) // let t.TempDir cleanup remove it
		dir := filepath.Join(parent, "thaw")
		// chown/chmod would fail here too (the dir does not exist), so either stub
		// path is faithful; use the healthy one to prove the MISSING branch alone
		// (not a stubbed chown failure) drives the fail-close.
		out, failedClosed := runThawdirPrecreate(t, block, dir, false)
		if !failedClosed {
			t.Errorf("a THAWDIR that could not be created must fail-close, but it did not:\n%s", out)
		}
		if !strings.Contains(out, "FT_ROOT_RC=128") {
			t.Errorf("uncreatable THAWDIR must emit FT_ROOT_RC=128, got:\n%s", out)
		}
	})
}

// TestThawScript_ThawdirChownChmodNoLongerSwallowed is the precise string-level
// guard that the chown/chmod `|| true` silent-swallow is gone. The pre-fix block
// ended both lines with `|| true`; the fix routes them to a WARN log and adds the
// [2c-assert] fail-close. This catches a partial revert that restores `|| true`
// even if the assertion is somehow still present.
func TestThawScript_ThawdirChownChmodNoLongerSwallowed(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractThawdirPrecreateBlock(t, m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"),
	}))

	for _, banned := range []string{
		`HMNT chown 1000:1000 "$THAWDIR" 2>/dev/null || true`,
		`HMNT chmod 0775 "$THAWDIR" 2>/dev/null || true`,
	} {
		if strings.Contains(block, banned) {
			t.Errorf("[2c] chown/chmod still silently swallows failure with %q — the ka-0000004248 fail-loud hardening was reverted", banned)
		}
	}
	// The chown/chmod failures must be logged (WARN), not dropped.
	for _, want := range []string{
		`HMNT chown 1000:1000 "$THAWDIR" 2>/dev/null || log "WARN`,
		`HMNT chmod 0775 "$THAWDIR" 2>/dev/null || log "WARN`,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("[2c] chown/chmod failure is not logged loudly; missing %q", want)
		}
	}

	// The writability assertion must precede the [3a] dangling-source guard in the
	// full rendered script so a non-writable forced source fails by name, not as a
	// downstream empty-mounts / gate-timeout error.
	full := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"),
	})
	iAssert := strings.Index(full, "PYTWPERM")
	iGuard := strings.Index(full, "dangling bind-mount source(s)")
	if iAssert < 0 || iGuard < 0 || iAssert > iGuard {
		t.Errorf("[2c-assert] writability guard must precede the [3a] dangling-source guard (assert=%d guard=%d)", iAssert, iGuard)
	}
}
