// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

package podmanager

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
)

// freezethaw_thaw_token_ownership_test.go is the RED-BAR guard for bug ka-0000000189:
// the gVisor thaw SA/IRSA token-ownership EACCES. It pairs with
// freezethaw_thaw_token_source_test.go (the ka-0000008074 mount-SOURCE force) and is the
// READ-side mirror of freezethaw_thawdir_writable_test.go (the [2c-assert] WRITE-side
// agent-writability guard for the .done ack dir).
//
// THE BUG (source + node-confirmed, prod 2026.181.5; the NEW coop-only signature). The
// freeze warm-copy runs shutil.copytree AS ROOT and copy2/copystat preserve MODE bits but
// NEVER uid/gid, so the projected serviceaccount token's GROUP is RESET from agent (GID
// 1000) to root -> the warm backing token is root:root 0640. The thaw cred-overlay
// (ka-0000004956) open().write()s Pod B's live token BYTES into that file but never chowns
// it, so ownership stays root:root. The restored UID/GID-1000 coop then EACCESes the bound
// token read (mode 0640: agent is neither owner=root nor group=root -> falls to OTHER=---)
// -> coop's gate_token maps PermissionDenied to FreshnessError::Malformed -> the
// NodeRehydrationTrigger.waitForAcks 60s gate (DefaultRehydrateDeadline, NOT 180s —
// ka-0000005354) fail-closes 'failed consumers: coop'. This first becomes reachable on
// 181.5 because the forced-source + overlay (gd-GNzj6raaao) finally makes coop's token
// READ actually execute against the warm copy — the 8 prior canaries died earlier and
// masked this read-permission layer. The maintainers' own [2c] block already chowns the
// ack dir the agent WRITES through the gofer (1000:1000) for exactly this FSGroup-1000
// hazard; this fix is the missing READ-side mirror.
//
// THE FIX (two parts, both in freezethaw_node.go's thaw template):
//   PART 1 — the cred-overlay (`elif d in _SA_TOKEN_DIRS:` branch) chowns the warm token's
//            REAL backing file (os.path.realpath follows token -> ..data/token -> ..<ts>/
//            token) to 1000:1000 and OR-s in the 0640 read bits AFTER the byte-overlay, so
//            the restored agent reads it as owner.
//   PART 2 — a [3a-tok] fail-loud read-probe (mirroring [2c-assert]): stat each
//            _SA_TOKEN_DIRS backing token in the host ns and, unless UID/GID 1000 can read
//            it, emit FT_ROOT_RC=128 + exit BEFORE the restore — converting a would-be
//            OPAQUE coop-only 60s timeout into a named rc=128 diagnosable at the credential
//            layer, closing the read/write asymmetry.
//
// THE UID-1000 RUNNER CONSTRAINT (same as freezethaw_thawdir_writable_test.go). The test
// runner is UID/GID 1000 (the agent uid), so it CANNOT forge a root:root file, and a chown
// to 1000:1000 of a file it already owns is a no-op — the chown's effect is not directly
// observable. So, exactly as the [2c-assert] sibling drives writability via MODE bits, this
// suite drives the read-side via MODE bits: the OBSERVABLE behavioral delta of PART 1 is
// the chmod read-bit OR (a 0600 backing token becomes 0640) applied to the REALPATH-followed
// backing file (proving os.chown over the chain, not os.lchown on the symlink); and the
// PART 2 probe's readable-vs-unreadable verdict for UID/GID 1000 is IDENTICAL whether the
// unreadable state is prod's root:root-0640 or a runner-owned read-bit-stripped fixture.

// chownCallRe matches the PART-1 backing-token chown `_os.chown(_rp,1000,1000)` with ANY
// spacing around the arguments (gd-ZoGHy0kYNV). The original structural guard used a literal
// strings.Contains on the no-space spelling, so a reformatted-but-correct
// `_os.chown(_rp, 1000, 1000)` false-RED'd despite being semantically identical and
// prod-safe. The regex tightens the failure condition to "the chown is genuinely absent",
// not "the chown was reformatted".
var chownCallRe = regexp.MustCompile(`_os\.chown\(\s*_rp\s*,\s*1000\s*,\s*1000\s*\)`)

// TestThawScript_ChownMatcherIsWhitespaceTolerant is the gd-ZoGHy0kYNV meta-guard on the
// matcher itself: chownCallRe MUST accept every reformatted-but-equivalent spelling of the
// PART-1 chown (the brittle-literal class the old strings.Contains rejected) and MUST reject
// only a genuinely absent / wrong-target chown. Without this, a future "fix" could re-brittle
// the matcher (e.g. back to a no-space literal) and the regression would go unnoticed.
func TestThawScript_ChownMatcherIsWhitespaceTolerant(t *testing.T) {
	// EQUIVALENT spellings — all semantically `os.chown(realpath, 1000, 1000)`, all prod-safe;
	// every one MUST match (the old literal Contains only accepted the first).
	for _, ok := range []string{
		`_os.chown(_rp,1000,1000)`,       // canonical no-space (the literal the old guard used)
		`_os.chown(_rp, 1000, 1000)`,     // gofmt/black-style single spaces after commas
		`_os.chown(_rp,  1000,  1000)`,   // double spaces
		`_os.chown( _rp , 1000 , 1000 )`, // spaces inside parens + around args
		`_os.chown(_rp ,1000 ,1000)`,     // spaces before commas
	} {
		if !chownCallRe.MatchString(ok) {
			t.Errorf("chownCallRe must accept the reformatted-but-equivalent chown %q (whitespace-only difference is prod-safe, must NOT false-RED)", ok)
		}
	}
	// ABSENT / WRONG-TARGET spellings — the matcher MUST reject these (the only states the
	// structural guard exists to catch: a dropped chown, an lchown swap, or a wrong uid/gid).
	for _, bad := range []string{
		``, // chown removed entirely (the gd-ZoGHy0kYNV regression)
		`_os.chmod(_rp,_os.stat(_rp).st_mode|0o640)`, // only the chmod survives, chown dropped
		`_os.lchown(_rp,1000,1000)`,                  // lchown re-owns the symlink, leaves backing root:root
		`_os.chown(_rp,0,0)`,                         // chown to root:root — does NOT restore the agent grant
		`_os.chown(_rp,1000,1001)`,                   // wrong gid
		`_os.chown(_other,1000,1000)`,                // chown of a different var, not the realpath'd backing file
	} {
		if chownCallRe.MatchString(bad) {
			t.Errorf("chownCallRe must REJECT %q — it is an absent/wrong chown the structural guard exists to catch", bad)
		}
	}
}

// ---------------------------------------------------------------------------
// PART 1 — the cred-overlay chown/chmod (behavioral, via the real PYAGENT block)
// ---------------------------------------------------------------------------

// writeWarmAtomicToken builds, at warm, the kubelet atomic-writer projected-token layout
// (token -> ..data/token -> ..<ts>/token) with the given bytes, and sets the REAL backing
// file (..<ts>/token) to backingMode. This is exactly the on-node warm-copy shape the
// PART-1 overlay rewrites: the overlay writes BYTES through the `token` symlink and chowns/
// chmods the realpath-followed backing file.
func writeWarmAtomicToken(t *testing.T, warm, tok string, backingMode os.FileMode) (backing string) {
	t.Helper()
	ts := "..2026_06_30_07_30_00.123456789"
	if err := os.MkdirAll(filepath.Join(warm, ts), 0o755); err != nil {
		t.Fatalf("mkdir atomic-writer ts dir: %v", err)
	}
	backing = filepath.Join(warm, ts, "token")
	if err := os.WriteFile(backing, []byte(tok), 0o644); err != nil {
		t.Fatalf("write backing token: %v", err)
	}
	if err := os.Symlink(ts, filepath.Join(warm, "..data")); err != nil {
		t.Fatalf("symlink ..data: %v", err)
	}
	if err := os.Symlink(filepath.Join("..data", "token"), filepath.Join(warm, "token")); err != nil {
		t.Fatalf("symlink token: %v", err)
	}
	// Set the backing mode LAST (the WriteFile above ignores umask-masked bits).
	if err := os.Chmod(backing, backingMode); err != nil {
		t.Fatalf("chmod backing token: %v", err)
	}
	return backing
}

// ownershipFixture builds a warm-B graft fixture whose token dirs ARE in the gofer manifest
// (keyed), so the `elif d in _SA_TOKEN_DIRS:` branch takes the warm-copy bind + cred-overlay
// path. Each warm copy is the atomic-writer layout with a read-bit-STRIPPED (0600) backing
// token, modelling the on-node root:root warm copy whose only loss is the agent read grant.
// Returns the agent config the PYAGENT block rewrites, the bundle, Pod B's live bcfg, and
// the two backing-token paths the overlay must chown/chmod.
func ownershipFixture(t *testing.T) (agentCfg, bundle, bcfgPath, backingEKS, backingSA string) {
	t.Helper()
	root := t.TempDir()
	bundle = filepath.Join(root, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, ".gofer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Pod B's LIVE projected dirs (the fresh cred-overlay byte source). Plain token files.
	mkLive := func(p, tok string) string {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "token"), []byte(tok), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	bEKS := mkLive(filepath.Join(root, "podB", "aws-iam-token", "serviceaccount"), "POD_B_IRSA")
	bSA := mkLive(filepath.Join(root, "podB", "kube-api-access", "serviceaccount"), "POD_B_SA")

	// Warm copies (controller-owned; survive [4]) in the atomic-writer layout, backing
	// token mode 0600 = owner rw, NO group/other read -> models the dropped agent grant.
	warmEKS := filepath.Join(bundle, ".gofer", "0")
	warmSA := filepath.Join(bundle, ".gofer", "1")
	backingEKS = writeWarmAtomicToken(t, warmEKS, "WARM_POD_A_IRSA", 0o600)
	backingSA = writeWarmAtomicToken(t, warmSA, "WARM_POD_A_SA", 0o600)
	manifest := map[string]string{
		tokenDestEKS: "0", "/var" + tokenDestEKS: "0",
		tokenDestSA: "1", "/var" + tokenDestSA: "1",
	}
	mb, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(bundle, ".gofer", "manifest.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := map[string]any{"annotations": map[string]string{}, "mounts": []map[string]string{
		{"destination": "/var/run/secrets/eks.amazonaws.com/serviceaccount", "source": "/podA/frozen/eks"},
		{"destination": "/var/run/secrets/kubernetes.io/serviceaccount", "source": "/podA/frozen/sa"},
	}}
	cfgBytes, _ := json.Marshal(cfg)
	agentCfg = filepath.Join(bundle, "agent", "config.json")
	if err := os.WriteFile(agentCfg, cfgBytes, 0o644); err != nil {
		t.Fatal(err)
	}

	bcfg := map[string]any{"mounts": []map[string]string{
		{"destination": "/var/run/secrets/eks.amazonaws.com/serviceaccount", "source": bEKS},
		{"destination": "/var/run/secrets/kubernetes.io/serviceaccount", "source": bSA},
	}}
	bb, _ := json.Marshal(bcfg)
	bcfgPath = filepath.Join(root, "bcfg.json")
	if err := os.WriteFile(bcfgPath, bb, 0o644); err != nil {
		t.Fatal(err)
	}
	return agentCfg, bundle, bcfgPath, backingEKS, backingSA
}

// statMode returns the file mode bits (perm only) of path, following symlinks.
func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// TestThawScript_TokenOverlayRestoresGroupReadOnBackingFile is the ka-0000000189 PART-1
// RED-BAR behavioral guard. It runs the ACTUAL PYAGENT cred-overlay block against a warm-B
// fixture whose backing token is 0600 (owner rw, NO group read — the on-node root:root
// warm copy's effect, modelled at uid 1000 via the stripped read bits). The overlay must:
//   - chown/chmod the REALPATH-FOLLOWED backing file (..<ts>/token), proving it used
//     os.chown (follows the symlink chain) not os.lchown (which would re-own the symlink
//     and leave the backing root:root unreadable), and
//   - leave the backing token readable by GID 1000 (group-read bit present, 0640).
//
// PRE-FIX (no chown/chmod lines) the backing token stays 0600 -> group-read absent -> RED.
func TestThawScript_TokenOverlayRestoresGroupReadOnBackingFile(t *testing.T) {
	block := tokenPyagentBlock(t)
	agentCfg, bundle, bcfgPath, backingEKS, backingSA := ownershipFixture(t)
	runPyagent(t, block, warmBPyagentArgs(agentCfg, bundle, bcfgPath, thawStateHostDir("agentY"))...)

	for _, tc := range []struct {
		name, backing, wantTok string
	}{
		{"eks", backingEKS, "POD_B_IRSA"},
		{"sa", backingSA, "POD_B_SA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The cred-overlay wrote Pod B's bytes THROUGH the token symlink into the
			// backing file — confirm we are asserting on the file coop actually reads.
			gotTok, err := os.ReadFile(backing(t, bundle, tc.name))
			if err != nil {
				t.Fatalf("backing token does not resolve through the atomic-writer chain: %v", err)
			}
			if string(gotTok) != tc.wantTok {
				t.Fatalf("backing token bytes = %q, want Pod-B %q (the overlay must write the live token through the symlink chain)", gotTok, tc.wantTok)
			}
			// PART 1: the realpath-followed backing file must now carry the group-read bit.
			// PRE-FIX it stays 0600 (no group read) -> the restored gid-1000 coop EACCESes.
			// PART 1's observable delta at uid 1000 (the chown to 1000:1000 is a no-op for
			// a file the runner already owns): the chmod must OR-in the group-read bit on
			// the realpath-followed backing file. PRE-FIX it stays 0600 -> group-read
			// absent -> the restored gid-1000 coop EACCESes the bound read.
			mode := statMode(t, tc.backing)
			if mode&0o040 == 0 {
				t.Fatalf("backing token %s mode = %04o after the cred-overlay; the group-read bit (0040) is ABSENT, so the restored UID/GID-1000 coop EACCESes the bound token read -> FreshnessError::Malformed -> 'failed consumers: coop' (ka-0000000189). PART 1 must chown 1000:1000 + OR-in 0640 the REALPATH-followed backing file.", tc.backing, mode)
			}
		})
	}
}

// backing resolves the warm copy's backing token (via the live `token` symlink) for the
// given dest name, so the test reads exactly what the restored coop reads.
func backing(t *testing.T, bundle, name string) string {
	t.Helper()
	n := "0"
	if name == "sa" {
		n = "1"
	}
	return filepath.Join(bundle, ".gofer", n, "token")
}

// TestThawScript_TokenOverlayChownIsScopedToTokenDirs is the ka-0000000189 PART-1 SCOPE
// gate: the chown/chmod must touch ONLY the two _SA_TOKEN_DIRS warm copies, never the
// ka-0000006427 ~/.claude warm trees (which take the generic `elif d in gofer:` branch and
// must stay agent-owned-AS-COPIED, untouched). A ~/.claude warm file seeded at 0600 must
// remain 0600 after the run, while a token warm copy at 0600 becomes group-readable.
func TestThawScript_TokenOverlayChownIsScopedToTokenDirs(t *testing.T) {
	block := tokenPyagentBlock(t)
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, ".gofer", "9"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A token warm copy (keyed at n=0) and a NON-token ~/.claude warm tree (keyed at n=9).
	warmTok := filepath.Join(bundle, ".gofer", "0")
	backingTok := writeWarmAtomicToken(t, warmTok, "WARM_IRSA", 0o600)
	// ~/.claude warm tree: a plain runtime file at 0600 (NOT a token mount).
	claudeFile := filepath.Join(bundle, ".gofer", "9", "settings.json")
	if err := os.WriteFile(claudeFile, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := map[string]string{
		tokenDestEKS: "0", "/var" + tokenDestEKS: "0",
		"/home/agent/.claude": "9", "/var/home/agent/.claude": "9",
	}
	mb, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(bundle, ".gofer", "manifest.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}
	bEKS := filepath.Join(root, "podB", "eks", "serviceaccount")
	if err := os.MkdirAll(bEKS, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bEKS, "token"), []byte("POD_B_IRSA"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"annotations": map[string]string{}, "mounts": []map[string]string{
		{"destination": "/var/run/secrets/eks.amazonaws.com/serviceaccount", "source": "/podA/frozen/eks"},
		{"destination": "/home/agent/.claude", "source": "/podA/frozen/claude"},
	}}
	cfgBytes, _ := json.Marshal(cfg)
	agentCfg := filepath.Join(bundle, "agent", "config.json")
	if err := os.WriteFile(agentCfg, cfgBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	bcfg := map[string]any{"mounts": []map[string]string{
		{"destination": "/var/run/secrets/eks.amazonaws.com/serviceaccount", "source": bEKS},
		{"destination": "/home/agent/.claude", "source": "/podB/live/claude"},
	}}
	bb, _ := json.Marshal(bcfg)
	bcfgPath := filepath.Join(root, "bcfg.json")
	if err := os.WriteFile(bcfgPath, bb, 0o644); err != nil {
		t.Fatal(err)
	}

	runPyagent(t, block, warmBPyagentArgs(agentCfg, bundle, bcfgPath, thawStateHostDir("agentY"))...)

	// The token warm copy got the group-read bit (PART 1 fired for the _SA_TOKEN_DIRS dest).
	if m := statMode(t, backingTok); m&0o040 == 0 {
		t.Fatalf("token backing %s mode = %04o; PART 1 must restore group-read on the token warm copy", backingTok, m)
	}
	// The ~/.claude warm file is UNTOUCHED — the chown/chmod must not widen non-token trees.
	if m := statMode(t, claudeFile); m != 0o600 {
		t.Fatalf("~/.claude warm file %s mode = %04o, want unchanged 0600 — the PART-1 chown/chmod escaped the _SA_TOKEN_DIRS scope and widened a non-token warm tree (ka-0000006427 regression)", claudeFile, m)
	}
}

// ---------------------------------------------------------------------------
// PART 2 — the [3a-tok] fail-loud read-probe (behavioral, via the real shell block)
// ---------------------------------------------------------------------------

// extractTokenReadProbeBlock slices the [3a-tok] `for dstn in $SUBNAMES; do ... done` loop
// that carries the PYTOKRD classifier out of the rendered thaw template, by anchoring on the
// PYTOKRD heredoc and walking out to the enclosing for/done. A refactor that renames/removes
// the probe makes extraction fail LOUD (a clear RED) rather than letting the guard no-op.
func extractTokenReadProbeBlock(t *testing.T, script string) string {
	t.Helper()
	lines := strings.Split(script, "\n")
	pyIdx := -1
	for i, l := range lines {
		if strings.Contains(l, "PYTOKRD") {
			pyIdx = i
			break
		}
	}
	if pyIdx < 0 {
		t.Fatalf("[3a-tok] read-probe not found: no PYTOKRD heredoc in the rendered thaw template — the SA/IRSA token readability guard (ka-0000000189) was renamed or removed")
	}
	// Walk UP to the enclosing `for dstn in $SUBNAMES; do`.
	forIdx := -1
	for i := pyIdx; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) == "for dstn in $SUBNAMES; do" {
			forIdx = i
			break
		}
	}
	if forIdx < 0 {
		t.Fatalf("[3a-tok] read-probe malformed: no enclosing `for dstn in $SUBNAMES; do` above the PYTOKRD heredoc")
	}
	// Walk DOWN to the matching `done` AFTER the second PYTOKRD marker (the heredoc close).
	pyCloseIdx := -1
	for i := pyIdx + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "PYTOKRD" {
			pyCloseIdx = i
			break
		}
	}
	if pyCloseIdx < 0 {
		t.Fatalf("[3a-tok] read-probe malformed: no PYTOKRD heredoc close")
	}
	doneIdx := -1
	for i := pyCloseIdx + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "done" {
			doneIdx = i
			break
		}
	}
	if doneIdx < 0 {
		t.Fatalf("[3a-tok] read-probe malformed: no closing `done` after the PYTOKRD heredoc")
	}
	block := strings.Join(lines[forIdx:doneIdx+1], "\n")
	if !strings.Contains(block, "FT_ROOT_RC=128") {
		t.Fatalf("[3a-tok] read-probe no longer emits FT_ROOT_RC=128 — its fail-close was weakened/removed. Extracted block:\n%s", block)
	}
	return block
}

// runTokenReadProbe executes the extracted [3a-tok] block under bash with HMNT/log stubbed,
// SUBNAMES=agent, and the fixture agent config at $BUNDLE/agent/config.json. A trailing
// FT_ROOT_RC=0 marks the pass-through path (the probe did NOT fail-close).
func runTokenReadProbe(t *testing.T, block, bundle string) (out string, failedClosed bool) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not available: %v", err)
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}
	harness := fmt.Sprintf(`set -uo pipefail
log() { :; }
HMNT() { "$@"; }
BUNDLE=%s
SUBNAMES=agent
%s
echo "FT_ROOT_RC=0"
`, shellQuoteForTest(bundle), rewriteAgentIDsForRunner(block))
	cmd := exec.Command("bash", "-c", harness)
	b, err := cmd.CombinedOutput()
	return string(b), err != nil
}

// probeFixture writes an agent config at $BUNDLE/agent/config.json whose two _SA_TOKEN_DIRS
// mounts point at warm token dirs in the atomic-writer layout, each backing token set to
// backingMode. The mount destinations are the normalized /run form (the PYAGENT rewrite has
// already run by the time [3a-tok] fires in the live script).
func probeFixture(t *testing.T, backingMode os.FileMode) string {
	t.Helper()
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	warmEKS := filepath.Join(bundle, ".gofer", "0")
	warmSA := filepath.Join(bundle, ".gofer", "1")
	writeWarmAtomicToken(t, warmEKS, "IRSA", backingMode)
	writeWarmAtomicToken(t, warmSA, "SA", backingMode)
	cfg := map[string]any{"annotations": map[string]string{}, "mounts": []map[string]string{
		{"destination": tokenDestEKS, "source": warmEKS},
		{"destination": tokenDestSA, "source": warmSA},
	}}
	cfgBytes, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(bundle, "agent", "config.json"), cfgBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	return bundle
}

// TestThawScript_TokenReadProbePassesWhenReadable is the ka-0000000189 PART-2 happy path:
// a warm backing token readable by GID 1000 (0640, owned 1000:1000 by the runner) must
// pass the [3a-tok] probe through — no FT_ROOT_RC=128 — exactly as the post-chown state.
func TestThawScript_TokenReadProbePassesWhenReadable(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractTokenReadProbeBlock(t, m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"), ThawStateDest: MountThawState,
	}))
	bundle := probeFixture(t, 0o640) // group-readable, owned 1000:1000 (post-chown state)
	out, failedClosed := runTokenReadProbe(t, block, bundle)
	if failedClosed {
		t.Errorf("a group-readable (0640) token must NOT fail-close the [3a-tok] probe, but it did:\n%s", out)
	}
	if strings.Contains(out, "FT_ROOT_RC=128") {
		t.Errorf("group-readable token emitted the fail-close sentinel FT_ROOT_RC=128:\n%s", out)
	}
	if !strings.Contains(out, "FT_ROOT_RC=0") {
		t.Errorf("group-readable token did not reach the pass-through marker FT_ROOT_RC=0:\n%s", out)
	}
}

// TestThawScript_TokenReadProbeFailsLoudWhenUnreadable is the ka-0000000189 PART-2 core
// RED-BAR: a warm backing token UNREADABLE by UID/GID 1000 (mode 0000 — the runner-ownable
// model of prod's root:root-0640, whose readability verdict for the agent is identical:
// none of owner/group/other read bits apply) must make the [3a-tok] probe emit
// FT_ROOT_RC=128 + exit. Without the probe (pre-fix) this state passes silently and
// manifests downstream as the OPAQUE coop-only 60s rehydrate timeout.
func TestThawScript_TokenReadProbeFailsLoudWhenUnreadable(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractTokenReadProbeBlock(t, m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"), ThawStateDest: MountThawState,
	}))
	bundle := probeFixture(t, 0o000) // no read bit for owner/group/other -> EACCES for uid 1000
	out, failedClosed := runTokenReadProbe(t, block, bundle)
	if !failedClosed {
		t.Errorf("an UNREADABLE token must fail-close the [3a-tok] probe (exit non-zero) — the read/write asymmetry the guard closes. Output:\n%s", out)
	}
	if !strings.Contains(out, "FT_ROOT_RC=128") {
		t.Errorf("unreadable token must emit FT_ROOT_RC=128, got:\n%s", out)
	}
	if strings.Contains(out, "FT_ROOT_RC=0") {
		t.Errorf("unreadable token reached the pass-through marker — the guard did not fail-close:\n%s", out)
	}
}

// TestThawScript_TokenReadProbeIsTokenDestScoped proves the [3a-tok] probe fires ONLY on the
// two _SA_TOKEN_DIRS destinations: an UNREADABLE (0000) warm file mounted at a NON-token
// destination must NOT fail-close the probe (the probe must skip it), so the guard never
// trips on the ka-0000006427 ~/.claude warm trees.
func TestThawScript_TokenReadProbeIsTokenDestScoped(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractTokenReadProbeBlock(t, m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"), ThawStateDest: MountThawState,
	}))
	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A NON-token warm tree with an unreadable backing file mounted at ~/.claude.
	warmClaude := filepath.Join(bundle, ".gofer", "9")
	writeWarmAtomicToken(t, warmClaude, "CLAUDE", 0o000)
	cfg := map[string]any{"annotations": map[string]string{}, "mounts": []map[string]string{
		{"destination": "/home/agent/.claude", "source": warmClaude},
	}}
	cfgBytes, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(bundle, "agent", "config.json"), cfgBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	out, failedClosed := runTokenReadProbe(t, block, bundle)
	if failedClosed {
		t.Errorf("the [3a-tok] probe fail-closed on a NON-token (~/.claude) destination — it must be scoped to the two _SA_TOKEN_DIRS only:\n%s", out)
	}
	if strings.Contains(out, "FT_ROOT_RC=128") {
		t.Errorf("the [3a-tok] probe emitted FT_ROOT_RC=128 for a non-token destination — scope leak:\n%s", out)
	}
}

// TestThawScript_TokenReadProbePrecedesRestore is the render-order guard: the [3a-tok]
// read-probe (PYTOKRD) must sit AFTER the [3a] dangling-source guard (so the token mount
// source already resolves) and BEFORE the [5] count-gated restore (so the agent never reads
// an unreadable token). A re-order silently reintroduces the read/write asymmetry.
func TestThawScript_TokenReadProbePrecedesRestore(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	full := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"), ThawStateDest: MountThawState,
	})
	iDangling := strings.Index(full, "dangling bind-mount source(s)")
	iProbe := strings.Index(full, "PYTOKRD")
	iRestore := strings.Index(full, `restore --image-path="$IMG" --bundle "$BUNDLE/sandbox"`)
	if iDangling < 0 || iProbe < 0 || iRestore < 0 {
		t.Fatalf("missing an ordering marker (dangling=%d probe=%d restore=%d)", iDangling, iProbe, iRestore)
	}
	if !(iDangling < iProbe && iProbe < iRestore) {
		t.Fatalf("the [3a-tok] read-probe must sit AFTER the [3a] dangling-source guard and BEFORE the [5] restore (dangling=%d probe=%d restore=%d)", iDangling, iProbe, iRestore)
	}
}

// ---------------------------------------------------------------------------
// Structural guards on the cred-overlay chown (catch a silent revert the behavioral
// tests cannot localize, mirroring freezethaw_thaw_token_source_test.go's order guard).
// ---------------------------------------------------------------------------

// TestThawScript_TokenOverlayChownIsInForcedBranch proves the chown/chmod lives INSIDE the
// `elif d in _SA_TOKEN_DIRS:` branch (between the token branch and the `elif d=="/work":`
// branch) — never in a generic branch that would re-own non-token warm trees.
func TestThawScript_TokenOverlayChownIsInForcedBranch(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"), ThawStateDest: MountThawState,
	})
	iToken := strings.Index(tw, `elif d in _SA_TOKEN_DIRS:`)
	iWork := strings.Index(tw, `elif d=="/work":`)
	if iToken < 0 || iWork < 0 {
		t.Fatalf("missing a branch marker (token=%d work=%d)", iToken, iWork)
	}
	tokenBranch := tw[iToken:iWork]
	// Whitespace-tolerant match (gd-ZoGHy0kYNV): a reformatted-but-correct
	// `_os.chown(_rp, 1000, 1000)` (WITH spaces — e.g. after a reflow of the embedded
	// python) is semantically identical and prod-safe, so it must NOT false-RED. A literal
	// strings.Contains("_os.chown(_rp,1000,1000)") did exactly that. The regex still FAILS
	// when the chown is genuinely ABSENT (the only state this guard exists to catch), but
	// accepts any spacing around the arguments.
	if !chownCallRe.MatchString(tokenBranch) {
		t.Fatalf("the _SA_TOKEN_DIRS forced branch must chown the warm backing token to 1000:1000 after the cred-overlay (ka-0000000189 PART 1); no `_os.chown(_rp,1000,1000)` (any spacing) found in:\n%s", tokenBranch)
	}
	// os.lchown would re-own the symlink, leaving the backing file root:root unreadable —
	// the fix MUST follow the chain with os.chown over realpath, never lchown.
	if strings.Contains(tokenBranch, "lchown") {
		t.Fatalf("the _SA_TOKEN_DIRS chown must use os.chown over the realpath-followed backing file, NOT os.lchown (which re-owns the symlink and leaves the backing token root:root unreadable):\n%s", tokenBranch)
	}
	if !strings.Contains(tokenBranch, "realpath") {
		t.Fatalf("the _SA_TOKEN_DIRS chown must follow the atomic-writer chain via os.path.realpath to the backing file:\n%s", tokenBranch)
	}
}

// ---------------------------------------------------------------------------
// PART 1 — PRIVILEGED behavioral coverage that OBSERVES the chown effect (gd-ZoGHy0kYNV).
//
// The unprivileged behavioral tests above can only assert the chmod read-bit DELTA, because
// the UID/GID-1000 runner already owns its fixtures, so a chown to 1000:1000 is an
// unobservable no-op there: removing ONLY the `_os.chown(_rp,1000,1000)` line leaves them
// GREEN (verified — only the structural grep catches it). In PROD the warm token is
// root:root 0640, so the |0o640 chmod is a NO-OP and the chown is the SOLE load-bearing op.
// These tests reconstruct exactly that prod precondition — a root:root backing token the
// runner does NOT own — which REQUIRES root (only root can forge a root-owned file and
// seteuid to 1000). They are cleanly SKIPPED when not root (the UID/GID-1000 CI agent lane),
// and EXECUTE wherever the runner is root (the RWX `test-gardenio` task was), so a
// future chown-drop red-bars at the BEHAVIORAL layer there, not just via the grep.
// ---------------------------------------------------------------------------

// requireRootForChownObservation skips a test that needs to forge a root-owned fixture and
// drop to uid 1000 — only constructible as root. The skip reason is explicit so an
// unprivileged local run (or the uid-1000 agent lane) reads as "gated", not "passing".
func requireRootForChownObservation(t *testing.T) {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skipf("needs root: the chown(1000:1000) effect on a prod-shaped root:root backing token is only OBSERVABLE when the runner is NOT uid 1000 (euid=%d). Run it as root to exercise it.", os.Geteuid())
	}
}

// canUID1000Read forks `setpriv --reuid 1000 --regid 1000 --clear-groups cat <path>` (or a
// python seteuid fallback) and reports whether a real UID/GID-1000 process can read path.
// This is the end-to-end read the restored agent performs — it exercises BOTH the file's
// read bits AND traversal x on every parent dir, exactly as coop's gate_token does.
func canUID1000Read(t *testing.T, path string) (ok bool, detail string) {
	t.Helper()
	if p, err := exec.LookPath("setpriv"); err == nil {
		cmd := exec.Command(p, "--reuid", "1000", "--regid", "1000", "--clear-groups", "cat", path)
		out, err := cmd.CombinedOutput()
		return err == nil, fmt.Sprintf("setpriv cat: err=%v out=%q", err, strings.TrimSpace(string(out)))
	}
	// Fallback: a python child that drops euid/egid to 1000 then open()s the file.
	py := `import os,sys
try:
    os.setgroups([1000]); os.setegid(1000); os.seteuid(1000)
    open(sys.argv[1],"rb").read(); print("OK")
except Exception as e:
    print("DENIED:%s"%e); sys.exit(1)`
	cmd := exec.Command("python3", "-c", py, path)
	out, err := cmd.CombinedOutput()
	return err == nil, fmt.Sprintf("python seteuid read: err=%v out=%q", err, strings.TrimSpace(string(out)))
}

// TestThawScript_TokenOverlayChownDropsToUID1000_Privileged is the gd-ZoGHy0kYNV behavioral
// red-bar for PART 1's chown. As ROOT it builds the prod precondition the unprivileged tests
// cannot — a warm backing token owned root:root 0640 (the copytree-after-freeze state where
// the chmod is a no-op and the chown is the only thing that restores the agent's grant) —
// runs the ACTUAL PYAGENT cred-overlay, and asserts BOTH: (a) the backing file's owner is now
// uid 1000 (the chown's DIRECT, otherwise-unobservable effect), and (b) a real UID/GID-1000
// process can now read it end-to-end. Delete ONLY the `_os.chown(_rp,1000,1000)` line and the
// token stays root:root 0640 -> uid-1000 read EACCESes -> this test RED-bars (whereas the
// older chmod-delta tests stay GREEN). That is the regression the task requires us to bite.
func TestThawScript_TokenOverlayChownDropsToUID1000_Privileged(t *testing.T) {
	requireRootForChownObservation(t)
	block := tokenPyagentBlock(t)
	agentCfg, bundle, bcfgPath, backingEKS, backingSA := ownershipFixture(t)
	// Re-forge each warm backing token into the EXACT prod state: owned root:root, mode 0640
	// (group-read already present, so the chmod is a NO-OP and the chown is load-bearing).
	for _, b := range []string{backingEKS, backingSA} {
		if err := os.Chmod(b, 0o640); err != nil {
			t.Fatalf("chmod backing 0640: %v", err)
		}
		if err := os.Chown(b, 0, 0); err != nil {
			t.Fatalf("chown backing root:root (needs root): %v", err)
		}
		// Parent ..<ts>/ dir 0755 root:root (the kubelet/copytree warm-copy dir state) so the
		// ONLY remaining obstacle for uid 1000 is the FILE's ownership — isolating the chown.
		if err := os.Chown(filepath.Dir(b), 0, 0); err != nil {
			t.Fatalf("chown backing dir root:root: %v", err)
		}
		if err := os.Chmod(filepath.Dir(b), 0o755); err != nil {
			t.Fatalf("chmod backing dir 0755: %v", err)
		}
		// The t.TempDir() ancestors are 0700 root-owned, which would block a uid-1000 process
		// from traversing TO the token regardless of the token's own perms — confounding the
		// chown observation. Make every ancestor under /tmp traversable so the FILE's ownership
		// (the thing PART 1's chown fixes) is the SOLE remaining obstacle for uid 1000.
		chmodAncestorsTraversable(t, filepath.Dir(b))
	}
	// Pre-condition sanity: BEFORE the overlay a uid-1000 process must NOT be able to read the
	// root:root-0640 token (proves the fixture actually models the prod EACCES we fix — and
	// that the ancestor-traversal prep above did NOT accidentally make it readable).
	if ok, detail := canUID1000Read(t, backingEKS); ok {
		t.Fatalf("fixture invalid: uid-1000 could already read the root:root-0640 backing token before the overlay ran (%s) — the chown effect would be unobservable", detail)
	}

	runPyagent(t, block, warmBPyagentArgs(agentCfg, bundle, bcfgPath, thawStateHostDir("agentY"))...)

	for _, tc := range []struct{ name, backing, wantTok string }{
		{"eks", backingEKS, "POD_B_IRSA"},
		{"sa", backingSA, "POD_B_SA"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// (a) DIRECT chown effect: the backing file is now owned by uid 1000. PRE-FIX
			// (chown line removed) it stays root (uid 0) -> RED here.
			var st syscall.Stat_t
			if err := syscall.Stat(tc.backing, &st); err != nil {
				t.Fatalf("stat backing %s: %v", tc.backing, err)
			}
			if st.Uid != 1000 {
				t.Fatalf("backing token %s is owned uid=%d after the cred-overlay, want 1000. PART 1's os.chown(_rp,1000,1000) did NOT run (or was dropped): in prod the warm token is root:root 0640, the chmod is a no-op, and the chown is the SOLE op that restores the agent's read grant — without it the restored UID/GID-1000 coop EACCESes the bound read -> FreshnessError::Malformed -> 'failed consumers: coop' (ka-0000000189).", tc.backing, st.Uid)
			}
			// (b) END-TO-END: a real uid/gid-1000 process can now read the token bytes.
			ok, detail := canUID1000Read(t, tc.backing)
			if !ok {
				t.Fatalf("after the cred-overlay a UID/GID-1000 process STILL cannot read backing token %s (%s) — the chown-drop did not take, so the restored agent EACCESes the bound token read (ka-0000000189).", tc.backing, detail)
			}
			// The bytes it reads are Pod B's live token (overlay wrote them through the chain).
			got, err := os.ReadFile(backing(t, bundle, tc.name))
			if err != nil {
				t.Fatalf("backing token does not resolve through the atomic-writer chain: %v", err)
			}
			if string(got) != tc.wantTok {
				t.Fatalf("backing token bytes = %q, want Pod-B %q", got, tc.wantTok)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// PART 2 — PRIVILEGED behavioral coverage for the [3a-tok] DIR-TRAVERSAL assertion
// (ka-0000000537). The probe runs as ROOT in the host ns, where root traverses ANY dir
// regardless of its x-bit — so a dir-x gap for uid 1000 is INVISIBLE to a file-mode-only
// verdict and would present as the SAME opaque coop timeout as an unreadable file. To OBSERVE
// the traversal assertion biting we must (a) forge an intermediate ..<ts>/ dir that uid 1000
// cannot traverse but whose backing FILE is read-bit-set, and (b) run the probe as root so it
// actually reaches the file (an unprivileged probe EACCESes at os.stat and can't distinguish
// the dir-x gap from a file-read gap). Both require root -> cleanly skipped otherwise.
// ---------------------------------------------------------------------------

// TestThawScript_TokenReadProbeFailsLoudWhenDirUntraversable_Privileged is the ka-0000000537
// behavioral red-bar. As ROOT it builds a warm token whose backing FILE is world-readable
// (0644) — so the file-read verdict alone PASSES — but whose intermediate ..<ts>/ dir is
// 0700 root:root, denying directory traversal (search/x) to uid/gid 1000. The restored agent
// would EACCES the dir WALK (coop maps it to FreshnessError::Malformed identically to a
// file-read EACCES — the invisible necessary-but-insufficient shape). With the new traversal
// assertion the [3a-tok] probe emits FT_ROOT_RC=128 HERE; WITHOUT it (file-mode-only) this
// state passes silently and manifests as the opaque coop 60s timeout.
func TestThawScript_TokenReadProbeFailsLoudWhenDirUntraversable_Privileged(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skipf("needs root: the probe runs as root in the host ns (HMNT), where root traverses any dir regardless of x-bit, so observing the uid-1000 dir-traversal gap requires forging a root-owned non-x dir AND running the probe as root (euid=%d). Run it as root to exercise it.", os.Geteuid())
	}
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractTokenReadProbeBlock(t, m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		ThawStateDir: thawStateHostDir("agentY"), ThawStateDest: MountThawState,
	}))
	// buildProbeFixture seeds a fresh fixture whose backing token FILE is world-readable
	// (0644) — so the FILE-read verdict alone always passes — owns both the ts dir and the
	// backing file root:root, sets the ts dir to tsMode, and makes the t.TempDir() ancestors
	// uid-1000-traversable so the ts dir's OWN mode is the only traversal variable.
	buildProbeFixture := func(t *testing.T, tsMode os.FileMode) string {
		t.Helper()
		bundle := probeFixture(t, 0o644)
		for _, n := range []string{"0", "1"} {
			tsDir := filepath.Dir(mustRealBacking(t, filepath.Join(bundle, ".gofer", n)))
			if err := os.Chown(tsDir, 0, 0); err != nil {
				t.Fatalf("chown ts dir root:root: %v", err)
			}
			if err := os.Chmod(tsDir, tsMode); err != nil {
				t.Fatalf("chmod ts dir %04o: %v", tsMode, err)
			}
			chmodAncestorsTraversable(t, filepath.Dir(tsDir)) // ancestors above the ts dir
		}
		return bundle
	}

	// NEGATIVE CONTROL: a 0755 root:root ts dir IS traversable by uid 1000 (other-x set), and
	// the backing file is readable, so the probe MUST pass through (no fail-close). This pins
	// the difference between this and the red-bar to the ts-dir x-bit ALONE — proving the
	// fail-close below is caused by the traversal gap, not by some unrelated fixture defect.
	t.Run("traversable ts dir (0755) passes through", func(t *testing.T) {
		bundle := buildProbeFixture(t, 0o755)
		out, failedClosed := runTokenReadProbe(t, block, bundle)
		if failedClosed || strings.Contains(out, "FT_ROOT_RC=128") {
			t.Errorf("a TRAVERSABLE 0755 ts dir over a readable backing file must NOT fail-close the [3a-tok] probe (the negative control isolating the x-bit). Output:\n%s", out)
		}
		if !strings.Contains(out, "FT_ROOT_RC=0") {
			t.Errorf("traversable ts dir did not reach the pass-through marker FT_ROOT_RC=0:\n%s", out)
		}
	})

	// RED-BAR (ka-0000000537): the SAME fixture but the ts dir is 0700 root:root -> uid 1000
	// cannot traverse it (owner-class first-match: not owner, so the absent other-x denies the
	// walk), even though the backing FILE is world-readable. The restored agent would EACCES
	// the dir WALK (coop maps it to FreshnessError::Malformed identically to a file-read EACCES
	// — the invisible necessary-but-insufficient shape). WITH the new traversal assertion the
	// probe fail-closes FT_ROOT_RC=128; WITHOUT it (file-mode-only) this passes silently and
	// manifests as the opaque coop 60s timeout. So deleting the traversal block flips this RED.
	t.Run("untraversable ts dir (0700) fails loud", func(t *testing.T) {
		bundle := buildProbeFixture(t, 0o700)
		out, failedClosed := runTokenReadProbe(t, block, bundle)
		if !failedClosed {
			t.Errorf("an UN-TRAVERSABLE 0700 root:root ts dir over a readable backing file must fail-close the [3a-tok] probe — the restored uid-1000 agent EACCESes the dir walk (coop FreshnessError::Malformed, the same opaque timeout as an unreadable file). Output:\n%s", out)
		}
		if !strings.Contains(out, "FT_ROOT_RC=128") {
			t.Errorf("un-traversable ts dir must emit FT_ROOT_RC=128, got:\n%s", out)
		}
	})
}

// mustRealBacking resolves warm/token through the atomic-writer symlink chain to the backing
// file, so a test can address the ..<ts>/ dir the realpath passes through.
func mustRealBacking(t *testing.T, warm string) string {
	t.Helper()
	real, err := filepath.EvalSymlinks(filepath.Join(warm, "token"))
	if err != nil {
		t.Fatalf("resolve backing token under %s: %v", warm, err)
	}
	return real
}

// chmodAncestorsTraversable OR-s in the other-search bit (0o001) on every ancestor of leaf up
// to (and including) os.TempDir(), so a UID/GID-1000 process can traverse the t.TempDir()
// fixture path to reach the token. Go's t.TempDir() roots at a 0700 process-owned dir, which
// would EACCES a uid-1000 walk regardless of the token's own perms — confounding the
// privileged behavioral observations. This only widens TRAVERSAL (x) on ANCESTOR dirs above
// the fixture's own token dirs; it never touches the token file or the ..<ts>/ dir under test,
// so it cannot mask the very ownership/x-bit the tests assert on. Root-only (needs to widen
// root-owned ancestors); the privileged tests already gate on euid==0.
func chmodAncestorsTraversable(t *testing.T, leaf string) {
	t.Helper()
	tmp := filepath.Clean(os.TempDir())
	p := filepath.Clean(leaf)
	for {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat ancestor %s: %v", p, err)
		}
		if err := os.Chmod(p, fi.Mode().Perm()|0o001); err != nil {
			t.Fatalf("chmod ancestor +o-x %s: %v", p, err)
		}
		if p == tmp || p == "/" {
			break
		}
		parent := filepath.Dir(p)
		if parent == p { // reached the root without hitting tmp; stop defensively
			break
		}
		p = parent
	}
}
