// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 Matt Baker <fimbaz@gmail.com>

package podmanager

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// freezethaw_satoken_test.go is the ka-0000008074 behavioral + render guard for the
// projected SA/IRSA token warm-copy.
//
// THE BUG (prod 2026.180.23, canary ka-0000001355). On a runsc thaw, coop's
// CredentialReminter fail-closed because BOTH projected token files were ENOENT in
// the restored sandbox:
//
//	aws_token: malformed JWT: read /var/run/secrets/eks.amazonaws.com/serviceaccount/token: No such file or directory
//	sa_token:  malformed JWT: read /var/run/secrets/kubernetes.io/serviceaccount/token: No such file or directory
//
// ROOT CAUSE (empirically reproduced). The two projected token mounts
// (aws-iam-token @ .../eks.amazonaws.com/serviceaccount, kube-api-access @
// .../kubernetes.io/serviceaccount) are DIRECTORY mounts under the kubelet pod dir,
// so the data-driven PYGOFER sweep (freezethaw_node.go, KUBELET_RE) DOES select them
// and DOES warm-copy them — but with `shutil.copytree(..., symlinks=False,
// ignore_dangling_symlinks=True)`. Those dirs use the kubelet ATOMIC-WRITER layout:
//
//	token   -> ..data/token        (relative symlink)
//	..data  -> ..<timestamp>/       (relative symlink)
//	..<timestamp>/token             (the real bytes)
//
// CPython's copytree, for a symlink with symlinks=False + ignore_dangling_symlinks,
// does `linkto = os.readlink(name); if not os.path.exists(linkto) and ignore: continue`.
// os.path.exists resolves the RAW RELATIVE target ("..data/token", "..<ts>") against
// the PROCESS CWD — not the link's own directory — so BOTH atomic-writer symlinks read
// as false-positive "dangling" and are silently skipped. The warm copy ends up with
// only ..<ts>/token and NO top-level `token`. At thaw coop re-reads <mount>/token ->
// ENOENT -> the observed "malformed JWT: ... No such file or directory".
//
// (The .gofer overlay added by ka-0000004956 in .24 only INCIDENTALLY re-creates the
// dropped top-level token, and only when Pod B's live bundle config resolves; when the
// "WARN no live Pod B bundle config" fallback fires, bsrc is empty, the overlay is
// skipped, and the ENOENT is still live. This guard pins the upstream copy so the file
// is structurally present regardless of the overlay.)
//
// THE FIX. The freeze sweep detects the kubelet atomic-writer signature
// (os.path.islink(s/"..data")) and copies THOSE dirs with symlinks=True, preserving the
// token -> ..data/token chain verbatim so it resolves on read. Plain runtime trees
// (~/.claude etc.) keep symlinks=False — no ka-0000006427 regression.
//
//	REAL in the behavioral test: the ACTUAL PYGOFER sweep extracted verbatim from the
//	                             rendered freeze template, run by python3 against a real
//	                             atomic-writer fixture on the test tmpfs.
//	STUBBED: HMNT (the nsenter mount-ns wrapper) is a passthrough — irrelevant to the
//	         copytree symlink semantics under test. The kubelet pod prefix the sweep's
//	         KUBELET_RE matches is created on the test tmpfs under a fixture root.

// extractPygoferBlock slices the real PYGOFER sweep body out of the rendered freeze
// template, between the `<<'PYGOFER'` heredoc open and its closing `PYGOFER` line.
// Both markers are unique in the template; a refactor that renames either makes
// extraction fail LOUD (a clear RED) rather than letting the guard silently no-op.
func extractPygoferBlock(t *testing.T, script string) string {
	t.Helper()
	const startMarker = "<<'PYGOFER'\n"
	const endMarker = "\nPYGOFER\n"
	_, afterStart, ok := strings.Cut(script, startMarker)
	if !ok {
		t.Fatalf("could not locate PYGOFER heredoc open %q in the rendered template — the sweep boundary moved; update this behavioral test (do NOT let it silently no-op)", strings.TrimSpace(startMarker))
	}
	block, _, ok := strings.Cut(afterStart, endMarker)
	if !ok {
		t.Fatalf("could not locate PYGOFER heredoc close %q after the open — the sweep boundary moved; update this behavioral test", strings.TrimSpace(endMarker))
	}
	// Sanity: the extracted block must carry the load-bearing sweep steps. If a
	// refactor strips them, fail here by name rather than producing a confusing diff.
	for _, must := range []string{
		`KUBELET_RE=re.compile`, // the mount selector
		`shutil.copytree(`,      // the warm-copy call under test
		`manifest.json`,         // dest->n manifest write
	} {
		if !strings.Contains(block, must) {
			t.Fatalf("extracted PYGOFER block is missing load-bearing step %q — extraction window is wrong or the step was removed", must)
		}
	}
	return block
}

// pygoferCopytreeCallLine returns the trimmed `shutil.copytree(` CALL line from the
// extracted sweep block, ISOLATING it from the explanatory comments that mention the
// same call/kwargs in prose. Asserting a copytree kwarg (symlinks=_atomic,
// copy_function=_reflink_copy2, …) against the whole block is vacuous — a comment
// carrying the substring makes a strings.Contains pass even when the kwarg was deleted
// from the real call (gd-mmp6kwyFt8 mutation). Both the atomic-writer guard and the
// reflink guard need exactly this isolation, so it lives here once. Fails LOUD (empty
// return -> caller Fatals) rather than silently no-opping if the call line is gone.
func pygoferCopytreeCallLine(block string) string {
	for ln := range strings.SplitSeq(block, "\n") {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "shutil.copytree(") {
			return trimmed
		}
	}
	return ""
}

// writeAtomicWriterTokenDir builds, at dir, the kubelet atomic-writer projected-token
// layout (token -> ..data/token -> ..<ts>/token) with the given token bytes. This is
// exactly how the kubelet materializes a projected serviceAccountToken volume on disk.
func writeAtomicWriterTokenDir(t *testing.T, dir, tokenBytes string) {
	t.Helper()
	ts := "..2026_06_29_19_46_00.123456789"
	if err := os.MkdirAll(filepath.Join(dir, ts), 0o755); err != nil {
		t.Fatalf("mkdir atomic-writer ts dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, ts, "token"), []byte(tokenBytes), 0o644); err != nil {
		t.Fatalf("write backing token: %v", err)
	}
	// ..data -> ..<ts>  (relative)
	if err := os.Symlink(ts, filepath.Join(dir, "..data")); err != nil {
		t.Fatalf("symlink ..data: %v", err)
	}
	// token -> ..data/token  (relative)
	if err := os.Symlink(filepath.Join("..data", "token"), filepath.Join(dir, "token")); err != nil {
		t.Fatalf("symlink token: %v", err)
	}
}

// runPygofer wraps the extracted sweep in the minimal prelude the freeze script
// establishes around it (HMNT passthrough + log) and runs it via python3 with the two
// positional args the heredoc consumes ($BUNDLE/agent/config.json, $BUNDLE). Returns
// combined stdout+stderr.
func runPygofer(t *testing.T, block, agentCfg, bundle string) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skipf("python3 not available: %v", err)
	}
	cmd := exec.Command("python3", "-", agentCfg, bundle)
	cmd.Stdin = strings.NewReader(block)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("PYGOFER sweep execution failed: %v\n%s", err, out)
	}
	return string(out)
}

// TestFreezeScript_WarmCopiesProjectedTokenResolvably is the ka-0000008074 RED-BAR
// behavioral guard. It runs the ACTUAL PYGOFER sweep against a fixture whose agent
// mounts include the two projected SA/IRSA token dirs in the kubelet atomic-writer
// layout, and asserts the warm copy's top-level `token` RESOLVES (and reads the right
// bytes). On the unfixed sweep (symlinks=False) the top-level token symlink is dropped
// -> os.path.exists(warm/token) is False -> RED. With the atomic-writer-aware fix it
// resolves -> GREEN.
func TestFreezeScript_WarmCopiesProjectedTokenResolvably(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractPygoferBlock(t, m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90}))

	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	// The `rm -rf "$BUNDLE/.gofer" && mkdir -p "$BUNDLE/.gofer"` line precedes the
	// PYGOFER heredoc (so it is NOT in the extracted block); the sweep writes its
	// manifest into $BUNDLE/.gofer. Create both the .gofer manifest dir and the
	// agent dir that holds config.json.
	if err := os.MkdirAll(filepath.Join(bundle, ".gofer"), 0o755); err != nil {
		t.Fatalf("mkdir bundle/.gofer: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "agent"), 0o755); err != nil {
		t.Fatalf("mkdir bundle/agent: %v", err)
	}
	// The sweep's KUBELET_RE matches sources that literally start with
	// /var/lib/kubelet/pods/<uid>/(volumes|volume-subpaths)/. We must create the
	// fixture under that REAL prefix (a unique per-test pod uid, cleaned up) so the
	// shipped selector picks it. If /var/lib/kubelet is not writable (some CI
	// sandboxes), skip rather than silently testing nothing.
	//
	// The pod uid MUST be unique ACROSS PROCESSES (gd-jcfyq28IO0): the earlier
	// "ft-satoken-test-"+filepath.Base(root) suffix was always "001" (t.TempDir's
	// first per-test dir), so two concurrent `go test` invocations — a multi-agent
	// pre-merge review, parallel CI shards — shared one literal global path and
	// raced: symlink EEXIST, or one process's Cleanup RemoveAll'd the fixture out
	// from under the other's sweep (empty manifest). os.MkdirTemp mints a fresh
	// suffix atomically, so concurrent processes can never collide.
	if err := os.MkdirAll("/var/lib/kubelet/pods", 0o755); err != nil {
		t.Skipf("cannot create /var/lib/kubelet/pods (need write under /var/lib/kubelet): %v", err)
	}
	kubeletPod, err := os.MkdirTemp("/var/lib/kubelet/pods", "ft-satoken-test-")
	if err != nil {
		t.Skipf("cannot create kubelet pod dir under /var/lib/kubelet/pods: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(kubeletPod) })
	podRoot := filepath.Join(kubeletPod, "volumes", "kubernetes.io~projected")
	eksDir := filepath.Join(podRoot, "aws-iam-token", "serviceaccount")
	saDir := filepath.Join(podRoot, "kube-api-access", "serviceaccount")
	writeAtomicWriterTokenDir(t, eksDir, "HDR.POD_A_IRSA.SIG")
	writeAtomicWriterTokenDir(t, saDir, "HDR.POD_A_SA.SIG")

	// A plain (non-atomic-writer) runtime tree to prove the fix does NOT regress the
	// ka-0000006427 sweep for ordinary EmptyDir mounts.
	claudeSrc := filepath.Join(podRoot, "claude-state")
	if err := os.MkdirAll(filepath.Join(claudeSrc, "projects"), 0o755); err != nil {
		t.Fatalf("mkdir claude: %v", err)
	}
	if err := os.WriteFile(filepath.Join(claudeSrc, "projects", "session.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write claude session: %v", err)
	}

	cfg := map[string]any{"mounts": []map[string]string{
		{"destination": "/var/run/secrets/eks.amazonaws.com/serviceaccount", "source": eksDir},
		{"destination": "/var/run/secrets/kubernetes.io/serviceaccount", "source": saDir},
		{"destination": "/home/agent/.claude", "source": claudeSrc},
	}}
	cfgBytes, _ := json.Marshal(cfg)
	agentCfg := filepath.Join(bundle, "agent", "config.json")
	if err := os.WriteFile(agentCfg, cfgBytes, 0o644); err != nil {
		t.Fatalf("write agent config: %v", err)
	}

	// The sweep does `rm -rf "$BUNDLE/.gofer" && mkdir -p` itself; here it reads the
	// manifest under $BUNDLE/.gofer. Run from a CWD WITHOUT a "..data" entry so the
	// copytree dangling false-positive (the actual prod failure mode) is reproduced.
	out := runPygofer(t, block, agentCfg, bundle)

	manifestPath := filepath.Join(bundle, ".gofer", "manifest.json")
	mb, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read manifest: %v\nsweep output:\n%s", err, out)
	}
	var manifest map[string]string
	if err := json.Unmarshal(mb, &manifest); err != nil {
		t.Fatalf("parse manifest: %v", err)
	}

	// Each token mount AND the claude tree must be in the manifest (all are dir mounts
	// under the kubelet prefix). The token files in their warm copies must RESOLVE.
	for _, tc := range []struct {
		dest string
		want string
	}{
		{"/var/run/secrets/eks.amazonaws.com/serviceaccount", "HDR.POD_A_IRSA.SIG"},
		{"/var/run/secrets/kubernetes.io/serviceaccount", "HDR.POD_A_SA.SIG"},
	} {
		n, ok := manifest[tc.dest]
		if !ok {
			t.Fatalf("manifest missing token mount %q (it must be swept): %v", tc.dest, manifest)
		}
		warmToken := filepath.Join(bundle, ".gofer", n, "token")
		// THE LOAD-BEARING ASSERTION: the top-level `token` coop re-reads must resolve.
		// Unfixed (symlinks=False) drops it -> ReadFile ENOENT -> RED.
		got, err := os.ReadFile(warmToken)
		if err != nil {
			t.Errorf("warm copy %s token does not resolve (the prod ENOENT): %v", tc.dest, err)
			continue
		}
		if string(got) != tc.want {
			t.Errorf("warm copy %s token bytes = %q, want %q", tc.dest, got, tc.want)
		}
	}

	// Regression guard: the plain ~/.claude tree's runtime subtree must still ride the
	// warm copy (the fix must not have switched the whole sweep to symlinks=True or
	// otherwise broken the ka-0000006427 carry).
	if n, ok := manifest["/home/agent/.claude"]; ok {
		sess := filepath.Join(bundle, ".gofer", n, "projects", "session.json")
		if _, err := os.Stat(sess); err != nil {
			t.Errorf("plain runtime tree (~/.claude) lost its subtree in the warm copy: %v", err)
		}
	} else {
		t.Errorf("plain runtime tree (~/.claude) must still be swept: %v", manifest)
	}
}

// TestFreezeScript_AtomicWriterAwareCopy is the render-string guard pinning the
// atomic-writer detection in the rendered freeze template, so a future refactor cannot
// silently drop the symlinks=True branch for the token dirs while still rendering a
// copytree call.
func TestFreezeScript_AtomicWriterAwareCopy(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractPygoferBlock(t, m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90}))
	for _, want := range []string{
		// the atomic-writer signature probe: a kubelet projected-token dir has a
		// "..data" symlink; plain EmptyDir trees do not.
		`"..data"`,
		`os.path.islink`,
	} {
		if !strings.Contains(block, want) {
			t.Errorf("PYGOFER sweep missing atomic-writer-aware copy marker %q", want)
		}
	}
	// Isolate the actual copytree CALL line (not the explanatory comments, which may
	// mention symlinks=False/True in prose). The call's symlinks= arg must be the
	// computed atomic-writer probe, not a hardcoded False that drops the token chain.
	copyLine := pygoferCopytreeCallLine(block)
	if copyLine == "" {
		t.Fatal("PYGOFER sweep has no shutil.copytree( call line")
	}
	if strings.Contains(copyLine, "symlinks=False") {
		t.Errorf("the copytree call still hardcodes symlinks=False — atomic-writer token symlinks (token -> ..data/token) are silently dropped, reproducing the prod ENOENT (ka-0000008074): %q", copyLine)
	}
	if !strings.Contains(copyLine, "symlinks=_atomic") {
		t.Errorf("the copytree call must pass the computed atomic-writer probe (symlinks=_atomic): %q", copyLine)
	}
}

// TestFreezeScript_GoferReflinkCountersEmit is the ka-0000006167 BEHAVIORAL guard for
// the reflink-engagement observability. It runs the ACTUAL PYGOFER sweep against a
// fixture with regular-file payload and asserts the sweep prints parseable
// FT_GOFER_REFLINK_HITS / FT_GOFER_REFLINK_FALLBACKS counters that ACCOUNT FOR the
// regular files copied (hits+fallbacks == the regular-file count). The render-string
// pin in TestFreezeScript_WorkspacePayloadUsesReflink proves the sentinels are emitted;
// this proves the counter LOGIC is wired to the copy_function and actually tallies —
// so the C3 re-baseline (ka-0000006167) reads a real number, not a hardcoded 0.
//
// On the gVisor node fs (XFS reflink=1) hits>0; on the test tmpfs FICLONE returns
// EOPNOTSUPP so every file takes the copy2 fallback and fallbacks>0. Either way the
// SUM must equal the regular files swept — that invariant holds regardless of node fs,
// which is exactly what makes it a portable assertion.
func TestFreezeScript_GoferReflinkCountersEmit(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractPygoferBlock(t, m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90}))

	root := t.TempDir()
	bundle := filepath.Join(root, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, ".gofer"), 0o755); err != nil {
		t.Fatalf("mkdir bundle/.gofer: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "agent"), 0o755); err != nil {
		t.Fatalf("mkdir bundle/agent: %v", err)
	}
	if err := os.MkdirAll("/var/lib/kubelet/pods", 0o755); err != nil {
		t.Skipf("cannot create /var/lib/kubelet/pods (need write under /var/lib/kubelet): %v", err)
	}
	kubeletPod, err := os.MkdirTemp("/var/lib/kubelet/pods", "ft-reflink-test-")
	if err != nil {
		t.Skipf("cannot create kubelet pod dir under /var/lib/kubelet/pods: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(kubeletPod) })

	// A plain EmptyDir tree with a known number of REGULAR FILES (no symlinks — those
	// take the _reflink_copy2 early-raise and count as fallbacks, which would muddy the
	// exact-sum invariant). Three files across two dirs.
	src := filepath.Join(kubeletPod, "volumes", "kubernetes.io~empty-dir", "state")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatalf("mkdir src: %v", err)
	}
	const wantRegularFiles = 3
	for _, f := range []string{"a.txt", "b.txt", "sub/c.txt"} {
		if err := os.WriteFile(filepath.Join(src, f), []byte("payload-"+f), 0o644); err != nil {
			t.Fatalf("write %s: %v", f, err)
		}
	}

	cfg := map[string]any{"mounts": []map[string]string{
		{"destination": "/home/agent/.claude", "source": src},
	}}
	cfgBytes, _ := json.Marshal(cfg)
	agentCfg := filepath.Join(bundle, "agent", "config.json")
	if err := os.WriteFile(agentCfg, cfgBytes, 0o644); err != nil {
		t.Fatalf("write agent config: %v", err)
	}

	out := runPygofer(t, block, agentCfg, bundle)

	fields := parseSentinels(out)
	hits, okH := intField(fields, "FT_GOFER_REFLINK_HITS")
	falls, okF := intField(fields, "FT_GOFER_REFLINK_FALLBACKS")
	if !okH {
		t.Fatalf("sweep did not emit a parseable FT_GOFER_REFLINK_HITS sentinel (ka-0000006167):\n%s", out)
	}
	if !okF {
		t.Fatalf("sweep did not emit a parseable FT_GOFER_REFLINK_FALLBACKS sentinel (ka-0000006167):\n%s", out)
	}
	// The load-bearing invariant: every regular file swept is accounted for as either a
	// CoW hit or a full-copy fallback, so the counters are wired to the copy_function and
	// not stuck at 0. Node-fs-independent (hits vs fallbacks split varies; the SUM does
	// not). A hardcoded 0/0 (counter logic dropped) fails here.
	if hits+falls != wantRegularFiles {
		t.Errorf("reflink counters must account for every regular file: hits=%d + fallbacks=%d = %d, want %d\n%s",
			hits, falls, hits+falls, wantRegularFiles, out)
	}
}
