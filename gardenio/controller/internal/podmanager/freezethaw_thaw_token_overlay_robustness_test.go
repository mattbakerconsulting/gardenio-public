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

// freezethaw_thaw_token_overlay_robustness_test.go carries the RED-BAR guards for the
// three MR !4985 pre-merge robustness follow-ups (ka-0000005326) on the SA/IRSA token
// cred-overlay (`elif d in _SA_TOKEN_DIRS:` branch, freezethaw_node.go). All three
// degrade fail-closed today (coop rejects a truncated/empty token as
// FreshnessError::Malformed -> the canary fails cleanly), so these harden the overlay
// against wasting an operator-gated canary cycle rather than fixing a live break:
//
//   nit 1 ATOMIC TOKEN WRITE — the overlay writes Pod-B's live token bytes to a temp
//         file on the backing file's own dir, fsyncs, then os.renames over it, so a
//         write interrupted mid-flush (ENOSPC/EIO) never leaves a TRUNCATED JWT.
//   nit 2 EMPTY-TOKEN GUARD — if realpath(bsrc[d]/token) is momentarily zero-length
//         (kubelet atomic-writer mid ..data-swap), the overlay must NOT truncate the
//         previously-valid warm token to b"".
//   nit 3 CompleteRestoreOptions ASSUMPTION PIN — doc-only; asserted as a render-string
//         guard that the pin comment is present so a future runsc bump triggers a review.
//
// These run the ACTUAL PYAGENT block extracted from the rendered thaw template against a
// warm-B keyed fixture, exactly like freezethaw_thaw_token_ownership_test.go.

// keyedAtomicTokenFixture builds a warm-B graft fixture whose two _SA_TOKEN_DIRS are in
// the gofer manifest (keyed) and whose warm copies use the kubelet atomic-writer layout
// (token -> ..data/token -> ..<ts>/token) seeded with warmTok. Pod B's LIVE projected
// dirs carry liveTok (may be empty to model a mid-rotation read). Returns the agent
// config the PYAGENT block rewrites, the bundle, and Pod B's live bcfg path.
func keyedAtomicTokenFixture(t *testing.T, warmTok, liveTok string) (agentCfg, bundle, bcfgPath string) {
	t.Helper()
	root := t.TempDir()
	bundle = filepath.Join(root, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, ".gofer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bundle, "agent"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Warm copies in the atomic-writer layout (0640 backing = post-chown readable state).
	writeWarmAtomicToken(t, filepath.Join(bundle, ".gofer", "0"), warmTok, 0o640)
	writeWarmAtomicToken(t, filepath.Join(bundle, ".gofer", "1"), warmTok, 0o640)
	manifest := map[string]string{
		tokenDestEKS: "0", "/var" + tokenDestEKS: "0",
		tokenDestSA: "1", "/var" + tokenDestSA: "1",
	}
	mb, _ := json.Marshal(manifest)
	if err := os.WriteFile(filepath.Join(bundle, ".gofer", "manifest.json"), mb, 0o644); err != nil {
		t.Fatal(err)
	}

	// Pod B's LIVE projected dirs (the cred-overlay byte source). liveTok may be "".
	mkLive := func(p, tok string) string {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(p, "token"), []byte(tok), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	bEKS := mkLive(filepath.Join(root, "podB", "aws-iam-token", "serviceaccount"), liveTok)
	bSA := mkLive(filepath.Join(root, "podB", "kube-api-access", "serviceaccount"), liveTok)

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
	return agentCfg, bundle, bcfgPath
}

// TestThawScript_TokenOverlayAtomicWriteServesFullBytes is the ka-0000005326 nit-1
// behavioral guard: the atomic overlay must serve Pod B's FULL live token bytes through
// the atomic-writer symlink chain (token -> ..data/token -> ..<ts>/token) — never a
// partial write — and must NOT leave a stray temp file in the backing dir (the mkstemp
// temp must be renamed away, not orphaned). A truncated JWT is what coop rejects as
// FreshnessError::Malformed; the temp-then-rename makes the overlay all-or-nothing.
func TestThawScript_TokenOverlayAtomicWriteServesFullBytes(t *testing.T) {
	block := tokenPyagentBlock(t)
	// A long, distinctive live token so a partial write is unmistakable.
	const liveTok = "POD_B_LIVE_JWT_eyJhbGciOiJSUzI1NiIsImtpZCI6IkFUT01JQ19XUklURV9URVNUIn0.PAYLOAD.SIGNATURE"
	agentCfg, bundle, bcfgPath := keyedAtomicTokenFixture(t, "WARM_POD_A_STALE", liveTok)
	runPyagent(t, block, warmBPyagentArgs(agentCfg, bundle, bcfgPath, thawStateHostDir("agentY"))...)

	for _, n := range []string{"0", "1"} {
		warm := filepath.Join(bundle, ".gofer", n)
		// The `token` symlink must resolve to the FULL Pod-B bytes (overlay applied end-to-end).
		got, err := os.ReadFile(filepath.Join(warm, "token"))
		if err != nil {
			t.Fatalf(".gofer/%s token does not resolve through the atomic-writer chain: %v", n, err)
		}
		if string(got) != liveTok {
			t.Fatalf(".gofer/%s served token = %q, want the FULL Pod-B token %q (atomic overlay must be all-or-nothing, never a partial write)", n, got, liveTok)
		}
		// ..data/token must resolve to the same full bytes.
		dd, err := os.ReadFile(filepath.Join(warm, "..data", "token"))
		if err != nil {
			t.Fatalf(".gofer/%s ..data/token does not resolve: %v", n, err)
		}
		if string(dd) != liveTok {
			t.Fatalf(".gofer/%s ..data/token = %q, want %q", n, dd, liveTok)
		}
		// No orphaned mkstemp temp files (prefix .tok.) in the backing ..<ts>/ dir.
		backingDir := filepath.Dir(mustRealpath(t, filepath.Join(warm, "token")))
		ents, err := os.ReadDir(backingDir)
		if err != nil {
			t.Fatalf("read backing dir %s: %v", backingDir, err)
		}
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".tok.") {
				t.Fatalf("atomic overlay left a stray temp file %q in %s — the mkstemp temp must be renamed onto the backing file, not orphaned", e.Name(), backingDir)
			}
		}
	}
}

// TestThawScript_TokenOverlayEmptyTokenGuardPreservesWarmCopy is the ka-0000005326 nit-2
// behavioral guard: when Pod B's live token read is momentarily ZERO-LENGTH (the kubelet
// atomic-writer mid ..data-swap), the overlay must NOT truncate the previously-valid warm
// token to b"" — it must skip the write and let the warm copy stand. PRE-FIX (unconditional
// open().write(_tok)) the warm backing token becomes empty -> coop rejects it as
// FreshnessError::Malformed -> a wasted operator-gated canary cycle.
func TestThawScript_TokenOverlayEmptyTokenGuardPreservesWarmCopy(t *testing.T) {
	block := tokenPyagentBlock(t)
	const warmTok = "WARM_POD_A_STILL_VALID_JWT"
	agentCfg, bundle, bcfgPath := keyedAtomicTokenFixture(t, warmTok, "" /* live token empty */)
	runPyagent(t, block, warmBPyagentArgs(agentCfg, bundle, bcfgPath, thawStateHostDir("agentY"))...)

	for _, n := range []string{"0", "1"} {
		warm := filepath.Join(bundle, ".gofer", n)
		got, err := os.ReadFile(filepath.Join(warm, "token"))
		if err != nil {
			t.Fatalf(".gofer/%s token does not resolve: %v", n, err)
		}
		if len(got) == 0 {
			t.Fatalf(".gofer/%s warm token was TRUNCATED to empty by an empty Pod-B live-token read — the empty-token guard must skip the overlay and preserve the previously-valid warm copy (ka-0000005326 nit 2); coop would reject the empty token as FreshnessError::Malformed", n)
		}
		if string(got) != warmTok {
			t.Fatalf(".gofer/%s token = %q, want the preserved warm copy %q (an empty live read must not overwrite it)", n, got, warmTok)
		}
	}
}

// TestThawScript_TokenOverlayPinsCompleteRestoreOptionsAssumption is the ka-0000005326
// nit-3 doc guard: the cred-overlay branch must carry the comment pinning the byte-overlay's
// restore-safety to the deployed runsc's EMPTY CompleteRestoreOptions (no size/mtime
// re-validation). A future runsc bump that flips ValidateFileSizes on would silently
// reintroduce rc=128; the pin makes the bump trigger a re-review. Guarding the comment's
// presence keeps the assumption from being lost in a refactor.
func TestThawScript_TokenOverlayPinsCompleteRestoreOptionsAssumption(t *testing.T) {
	block := tokenPyagentBlock(t)
	for _, want := range []string{
		"CompleteRestoreOptions", // names the runsc struct the safety rides on
		"release-20260525.0",     // pins the exact deployed release
		"ValidateFileSizes",      // the specific flag that would break the overlay if flipped
	} {
		if !strings.Contains(block, want) {
			t.Errorf("cred-overlay branch is missing the CompleteRestoreOptions assumption pin substring %q — a runsc bump that turns on size validation would silently reintroduce rc=128 (ka-0000005326 nit 3)", want)
		}
	}
}

// mustRealpath resolves symlinks in p or fails the test.
func mustRealpath(t *testing.T, p string) string {
	t.Helper()
	rp, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatalf("resolve symlinks for %s: %v", p, err)
	}
	return rp
}
