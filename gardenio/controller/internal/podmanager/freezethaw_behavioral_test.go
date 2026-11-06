package podmanager

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// freezethaw_behavioral_test.go is the gd-QD7MlhBHmy behavioral/integration test
// for the dir-medium plural_fix freeze [3a] block (ka-0000000357, merged MR !4975).
//
// WHY THIS FILE EXISTS. The pre-existing freeze [3a] coverage
// (TestFreezeScript_DirMediumSkeletonAndStacking, freezethaw_overlay_test.go) is
// RENDER-STRING ONLY: it greps the GENERATED SCRIPT TEXT for the right lines. That
// can prove a line is present but CANNOT catch a logic-ORDER break, a broken
// python skeleton/stack computation, or a regression that re-hollows the relocated
// lower — exactly the cycle-14 sparse-lower failure mode that burned three wrong
// canaries before plural_fix landed. The one pre-existing behavioral test
// (TestThaw_DirMediumReconstructedLowerCarriesWorkspace) is THAW-side and
// HAND-COPIES a parallel materialization script, so a regression in the FREEZE
// template would not turn it red.
//
// WHAT THIS FILE DOES (and what is real vs stubbed — NO silent caps). It extracts
// the ACTUAL freeze [3a] + relocation block from the rendered freeze template
// (m.buildFreezeScript), so the bytes under test are the bytes that ship — and runs
// that real shell against a FIXTURE overlay tree (real dirs/files/symlinks on the
// test tmpfs) that models the cycle-14 catastrophically-sparse merged rootfs (agent
// top-layer file + dangling bin/lib64/sbin symlinks; usr/home/etc/var ABSENT). It
// then asserts the resulting lower STACK is COMPLETE: it carries the relocation cp,
// the stacked GC-safe image base layers (/usr/bin/bash et al.), the base-mountpoint
// ancestor + cwd skeleton dentries, and resolvable symlink targets — i.e. the
// plural_fix invariant. A future edit that re-introduces the sparse-lower bug (drops
// the [3a] stacking or skeleton, or reorders the cp/gate so the lower comes out
// hollow) turns these assertions RED.
//
//   REAL in this test:   the freeze [3a] shell (extracted verbatim from the rendered
//                        template), the relocation `cp -a --reflink=auto`, the
//                        image-lowerdir stacking, the [3a] python skeleton
//                        computation + mkdir, the .overlay-opts write, the
//                        .gvisor.filestore.* strip — all driven against real files.
//   STUBBED in this test: HMNT (the node-only `nsenter -t 1 -m` mount-namespace
//                        wrapper) is a passthrough — the only difference is WHICH
//                        mount namespace the filesystem ops target, irrelevant to the
//                        on-disk cp/mkdir/python logic [3a] performs. The runsc
//                        checkpoint/restore is NOT part of [3a] (which is pure
//                        filesystem work) and is unreachable in this sandbox; it is
//                        exercised by the Freeze/Thaw orchestration tests via
//                        fakeNodeExecutor. The overlay MOUNT + the restore-time
//                        9p/dentry walk is unreachable in-sandbox (needs root +
//                        runsc on a node), so the merged-view assertions EMULATE
//                        overlayfs lookup (a path resolves if it exists in any stack
//                        layer, lower-first) rather than mounting — explicitly an
//                        emulation, validated on-node only by the canary.

// extractFreeze3aBlock slices the real relocation + [3a] block out of the rendered
// freeze script: from the `LOWER="$BUNDLE/$dstn/lower"` relocation header through
// just before the `# resolve the OCI bundle dir` comment (the line after the
// relocation if/else `fi`). The block is the per-container body the freeze [3/8]
// loop runs; the markers are unique in the template. A template refactor that
// renames either boundary makes extraction fail LOUD (a clear RED), which is the
// intended safety property — the behavioral guard must never silently no-op.
func extractFreeze3aBlock(t *testing.T, script string) string {
	t.Helper()
	const startMarker = "  LOWER=\"$BUNDLE/$dstn/lower\"\n"
	const endMarker = "\n  # resolve the OCI bundle dir (runsc state \"bundle\" field"
	_, afterStart, ok := strings.Cut(script, startMarker)
	if !ok {
		t.Fatalf("could not locate freeze [3a] start marker %q in the rendered template — the block boundary moved; update this behavioral test (do NOT let it silently no-op)", strings.TrimSpace(startMarker))
	}
	// re-prepend the start marker so the executed block begins at the LOWER= line.
	rest := startMarker + afterStart
	block, _, ok := strings.Cut(rest, endMarker)
	if !ok {
		t.Fatalf("could not locate freeze [3a] end marker %q after the start — the block boundary moved; update this behavioral test", strings.TrimSpace(endMarker))
	}
	// Sanity: the extracted block must contain the load-bearing [3a] steps. If a
	// refactor strips them, fail here by name rather than letting a later assertion
	// produce a confusing diff.
	for _, must := range []string{
		`cp -a --reflink=auto "$1/." "$2/"`, // the relocation copy
		`if [ -n "$OVERLAY_FLAG" ]; then`,   // the dir-medium gate
		`printf 'lowerdir=%s%s\n' "$LOWER"`, // image-lowerdir stacking
		`HMNT mkdir -p "$LOWER/$rel"`,       // skeleton dentry floor
	} {
		if !strings.Contains(block, must) {
			t.Fatalf("extracted freeze [3a] block is missing load-bearing step %q — extraction window is wrong or the step was removed", must)
		}
	}
	return block
}

// runFreeze3a wraps the extracted block in the minimal prelude the freeze [3/8]
// loop establishes around each container body (HMNT/log/NOW_MS helpers + the
// per-container loop vars), then executes it against the fixture. It returns the
// combined stdout+stderr so the caller can assert the FT_* sentinels.
func runFreeze3a(t *testing.T, block, bundle, taskDir, cid, dstn, ovl, overlayFlag string) string {
	t.Helper()
	// PRELUDE faithfulness: HMNT is the node-only nsenter wrapper, stubbed to a
	// passthrough (see file header — the only difference is the mount namespace, not
	// the filesystem semantics [3a] exercises). log/NOW_MS are byte-identical to the
	// template's definitions. The six loop vars are what the real freeze [3/8] driver
	// has in scope when it enters this per-container body: $BUNDLE/$T are the
	// controller-owned safe paths, $cid/$dstn the current container, $ovl the live
	// captured overlay opts, $OVERLAY_FLAG the dir-medium gate.
	const prelude = `set -uo pipefail
HMNT() { "$@"; }
log() { echo "    $*" >&2; }
NOW_MS() { date +%s%3N; }
BUNDLE="$1"; T="$2"; cid="$3"; dstn="$4"; ovl="$5"; OVERLAY_FLAG="$6"
`
	cmd := exec.Command("bash", "-c", prelude+block, "freeze3a", bundle, taskDir, cid, dstn, ovl, overlayFlag)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("freeze [3a] block execution failed: %v\n%s", err, out)
	}
	return string(out)
}

// sparseAgentFixture builds, on a fresh tmpdir, the inputs the freeze [3a] block
// reads, modeling the cycle-14 dir-medium failure mode. Returns the bundle root,
// task dir, cid and dstn, plus the absolute image-base + container-upper dirs the
// .overlay-opts (`$ovl`) names. The merged rootfs at $T/$cid/rootfs is deliberately
// CATASTROPHICALLY SPARSE (the forensic: only the agent top-layer file + dangling
// symlinks; the base image dirs ABSENT) so that the relocation cp ALONE would
// produce a hollow, un-walkable lower — making the [3a] completion the thing under
// test.
func sparseAgentFixture(t *testing.T) (bundle, taskDir, cid, dstn, imgBase, upper string) {
	t.Helper()
	root := t.TempDir()
	bundle = filepath.Join(root, "bundle")
	taskDir = filepath.Join(root, "task")
	cid = "agcid"
	dstn = "agent"

	// The early-captured agent OCI spec (freeze captures this BEFORE [3a]). It drives
	// the skeleton computation: file-bind LEAVES (resolv.conf/hosts/termination-log)
	// whose ancestors-only must be seeded, a deep /var/run mount whose ancestor chain
	// must be seeded (leaf NOT), the dedicated /work + /tmp single-component mounts
	// (no ancestors -> nothing seeded -> leaf-discipline negative control), and the
	// image-WORKDIR cwd whose FULL chain must be seeded.
	mustMkdirAll(t, filepath.Join(bundle, dstn))
	cfg := `{"process":{"cwd":"/home/agent/workspace","user":{"uid":1000,"gid":1000}},` +
		`"mounts":[` +
		`{"destination":"/etc/resolv.conf"},` +
		`{"destination":"/etc/hosts"},` +
		`{"destination":"/dev/termination-log"},` +
		`{"destination":"/var/run/secrets/kubernetes.io/serviceaccount"},` +
		`{"destination":"/work"},` +
		`{"destination":"/tmp"}` +
		`]}`
	mustWrite(t, filepath.Join(bundle, dstn, "config.json"), cfg)

	// SPARSE merged rootfs (cycle-14): the agent's own top-layer file + the three
	// dangling rootfs symlinks gVisor leaves (bin->usr/bin, lib64->usr/lib64,
	// sbin->usr/sbin), and a leftover gVisor filestore that [3a] must strip. NO
	// usr/home/etc/var/dev — those are exactly what a sparse merged cp drops.
	rootfs := filepath.Join(taskDir, cid, "rootfs")
	mustMkdirAll(t, rootfs)
	mustWrite(t, filepath.Join(rootfs, "entrypoint.sh"), "#!/bin/sh\n")
	mustSymlink(t, "usr/bin", filepath.Join(rootfs, "bin"))
	mustSymlink(t, "usr/lib64", filepath.Join(rootfs, "lib64"))
	mustSymlink(t, "usr/sbin", filepath.Join(rootfs, "sbin"))
	mustWrite(t, filepath.Join(rootfs, ".gvisor.filestore.deadbeef"), "stale\n")

	// The captured live overlay opts ($ovl) name the containerd image RO lowerdir +
	// the container upperdir. ON DISK those individual layer dirs are ALWAYS complete
	// regardless of the sparse merged view (verified on-node) — [3a] stacks them GC-
	// safely beneath the relocated lower so the merged restore view resolves the base
	// content. imgBase carries the real base-image dirs; upper carries a runtime file.
	imgBase = filepath.Join(root, "imgbase")
	upper = filepath.Join(root, "upper")
	for _, d := range []string{"usr/bin", "usr/lib", "usr/lib64", "usr/sbin", "etc"} {
		mustMkdirAll(t, filepath.Join(imgBase, d))
	}
	mustWrite(t, filepath.Join(imgBase, "usr/bin/bash"), "ELF\n")
	mustWrite(t, filepath.Join(imgBase, "usr/lib/libc.so.6"), "lib\n")
	mustWrite(t, filepath.Join(imgBase, "etc/passwd"), "root:x:0:0\n")
	// a stale filestore in the image layer too — [3a]'s cp-img must strip it.
	mustWrite(t, filepath.Join(imgBase, ".gvisor.filestore.cafe"), "stale\n")
	mustMkdirAll(t, filepath.Join(upper, "root"))
	mustWrite(t, filepath.Join(upper, "root/.bashrc"), "export PS1=\n")
	// The agent top-layer file lives in the container UPPERDIR — where a runtime-written
	// or top-layer rootfs file actually resides on a real overlay (every file in the
	// merged view originates in SOME constituent layer, by definition of overlay(upper,
	// lowers)). The pre-ka-0000007174 relocation copied the merged MOUNT so it did not
	// matter which layer a file "really" lived in; the reflink fix reconstructs the lower
	// from the real layers, so the fixture must place entrypoint.sh in its true layer.
	// It ALSO stays in the sparse merged rootfs above so the self-medium merged-cp path
	// (which copies $T/$cid/rootfs) still finds it — modelling a file present in both the
	// upper layer and the merged view, exactly as on a live overlay.
	mustWrite(t, filepath.Join(upper, "entrypoint.sh"), "#!/bin/sh\n")
	return
}

// overlayOpts is the parsed .overlay-opts: the ordered stack of lowerdir segments
// the freeze [3a] block wrote (lowerdir=<seg>:<seg>:...).
type overlayOpts struct{ stack []string }

func parseOverlayOpts(t *testing.T, bundle, dstn string) overlayOpts {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(bundle, dstn, ".overlay-opts"))
	if err != nil {
		t.Fatalf("read .overlay-opts: %v", err)
	}
	s := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(s, "lowerdir=") {
		t.Fatalf(".overlay-opts must start with lowerdir=, got %q", s)
	}
	return overlayOpts{stack: strings.Split(strings.TrimPrefix(s, "lowerdir="), ":")}
}

// resolves emulates an overlayfs lookup over the stack: rel resolves if it exists in
// ANY layer (lower-first), counting symlinks (Lstat) as present. This is the
// in-sandbox stand-in for the real `mount -t overlay` + restore-time 9p/dentry walk
// (unreachable without root + runsc on a node); the real merge is validated on-node
// by the canary. Returns the resolving layer, or "" if the path would ENOENT — the
// rc=128 failure mode.
func (o overlayOpts) resolves(rel string) string {
	for _, layer := range o.stack {
		if _, err := os.Lstat(filepath.Join(layer, rel)); err == nil {
			return layer
		}
	}
	return ""
}

// TestFreeze_DirMedium3aProducesCompleteLower is the core behavioral guard. It runs
// the REAL freeze [3a] block (extracted from the rendered template) against the
// cycle-14 sparse fixture under the DIR medium and asserts the resulting lower STACK
// is COMPLETE — the plural_fix invariant: "the relocated dir-medium lower carries
// /usr /bin /etc /lib /home/agent/workspace + cwd + resolvable symlink targets, not
// a sparse upper-delta." A regression that re-hollows the lower turns this RED.
func TestFreeze_DirMedium3aProducesCompleteLower(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractFreeze3aBlock(t, m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90}))
	bundle, taskDir, cid, dstn, imgBase, upper := sparseAgentFixture(t)
	ovl := "upperdir=" + upper + ",lowerdir=" + imgBase

	// PRECONDITION (negative control built in): the raw sparse source — what the
	// relocation cp ALONE would copy — is genuinely missing the base content + the
	// cwd. If this ever stops holding, the fixture no longer models the bug and the
	// completeness assertions below would pass vacuously.
	for _, rel := range []string{"usr", "usr/bin/bash", "home/agent/workspace", "etc/passwd"} {
		if _, err := os.Lstat(filepath.Join(taskDir, cid, "rootfs", rel)); !os.IsNotExist(err) {
			t.Fatalf("precondition: sparse source must NOT carry %q before [3a] (err=%v) — fixture no longer models the cycle-14 sparse lower", rel, err)
		}
	}

	out := runFreeze3a(t, block, bundle, taskDir, cid, dstn, ovl, "root:dir=/var/lib/gardenio/overlay")

	opts := parseOverlayOpts(t, bundle, dstn)
	lower := filepath.Join(bundle, dstn, "lower")

	// (1) .overlay-opts must stack the image base layers BENEATH the relocated lower:
	// lowerdir=$LOWER:<img.0>:<img.1>..., $LOWER first (authoritative on top).
	if len(opts.stack) < 2 {
		t.Fatalf("dir-medium .overlay-opts must stack image lowerdirs beneath $LOWER; got stack=%v", opts.stack)
	}
	if opts.stack[0] != lower {
		t.Errorf(".overlay-opts must lead with the relocated lower %q; got %q", lower, opts.stack[0])
	}
	for i, seg := range opts.stack[1:] {
		if base := filepath.Base(seg); !strings.HasPrefix(base, "img.") {
			t.Errorf("stacked segment %d must be a GC-safe img.* dir; got %q", i, seg)
		}
		if fi, err := os.Stat(seg); err != nil || !fi.IsDir() {
			t.Errorf("stacked image lowerdir %q must exist as a directory: %v", seg, err)
		}
	}

	// (2) MERGED-VIEW COMPLETENESS — the heart of the invariant. Every path the
	// restore-time 9p/dentry walk needs must resolve through the stack. A sparse
	// lower (the bug) would ENOENT the base content and the cwd -> rc=128.
	wantResolves := map[string]string{
		"entrypoint.sh":        "the relocation cp (agent top layer)",
		"usr/bin/bash":         "the stacked image base layer (execve target)",
		"usr/lib/libc.so.6":    "the stacked image base layer (ld.so dependency)",
		"etc/passwd":           "the stacked image base layer",
		"root/.bashrc":         "the stacked container upper",
		"home/agent/workspace": "the [3a] cwd skeleton (image WORKDIR dentry)",
		"usr":                  "the [3a] symlink-target skeleton / image base",
		"usr/bin":              "the [3a] symlink-target skeleton / image base (bin-> target)",
		"usr/lib64":            "the [3a] symlink-target skeleton / image base (lib64-> target)",
		"usr/sbin":             "the [3a] symlink-target skeleton / image base (sbin-> target)",
	}
	for rel, why := range wantResolves {
		if layer := opts.resolves(rel); layer == "" {
			t.Errorf("merged lower stack must resolve %q (%s) — a sparse lower would ENOENT this and rc=128 the restore 9p walk", rel, why)
		}
	}

	// (3) The dangling rootfs symlinks must be PRESERVED in the lower by the cp (so
	// they still point at targets that now resolve through the stack).
	for link, target := range map[string]string{"bin": "usr/bin", "lib64": "usr/lib64", "sbin": "usr/sbin"} {
		got, err := os.Readlink(filepath.Join(lower, link))
		if err != nil {
			t.Errorf("relocated lower must preserve the %q rootfs symlink: %v", link, err)
			continue
		}
		if got != target {
			t.Errorf("symlink %q = %q, want %q", link, got, target)
		}
	}

	// (4) The [3a] skeleton dentry floor must carry the base mount-DESTINATION
	// ANCESTOR chains + the cwd FULL chain + the hardcoded symlink targets, AS REAL
	// DIRECTORIES on the lower (the 9p walk stats the dentry type). The SA-token mount
	// destination /var/run/secrets/kubernetes.io/serviceaccount is NORMALIZED
	// /var/run->/run (ka-0000005421) before its ancestor chain is seeded — so its real
	// ancestors land at run/secrets/kubernetes.io, where the restored mount actually
	// binds (mirroring the thaw [3] mount-destination normalization), NOT at var/run/*.
	for _, rel := range []string{
		"home", "home/agent", "home/agent/workspace", // cwd full chain
		"etc",                                             // ancestor of /etc/resolv.conf + /etc/hosts
		"dev",                                             // ancestor of /dev/termination-log
		"var",                                             // /var/run symlink parent (real dir)
		"run", "run/secrets", "run/secrets/kubernetes.io", // NORMALIZED SA mount ancestors
		"usr", "usr/bin", "usr/lib", "usr/lib64", "usr/sbin", // symlink targets
	} {
		fi, err := os.Lstat(filepath.Join(lower, rel))
		if err != nil {
			t.Errorf("[3a] skeleton must seed %q into the lower: %v", rel, err)
			continue
		}
		if !fi.IsDir() {
			t.Errorf("[3a] skeleton dentry %q must be a directory (gVisor restore stats the dentry type)", rel)
		}
	}

	// (4b) /var/run must be recreated as a SYMLINK -> /run (the usrmerge topology),
	// NOT a real directory (ka-0000005421). A real var/run dir SHADOWS the base-image
	// /var/run->/run symlink, so coop's hardcoded /var/run/secrets/.../token read
	// (rehydrate.rs:53 SA + :415 EKS-webhook IRSA) ENOENTs against the empty shadow
	// dir while the normalized token mount binds at /run/secrets — the canary #10
	// rehydrate fail-close. Restoring the symlink makes /var/run/secrets and
	// /run/secrets the same path again, exactly as on a cold-start pod.
	varRun := filepath.Join(lower, "var", "run")
	fi, err := os.Lstat(varRun)
	if err != nil {
		t.Errorf("[3a] must recreate %q in the lower: %v", varRun, err)
	} else if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("[3a] var/run must be a SYMLINK -> /run (usrmerge topology), not a real %v — a real dir shadows the base-image symlink and ENOENTs coop's /var/run/secrets token read (ka-0000005421)", fi.Mode())
	} else if target, lerr := os.Readlink(varRun); lerr != nil || target != "/run" {
		t.Errorf("[3a] var/run symlink target = %q (err=%v), want \"/run\"", target, lerr)
	}

	// (5) LEAF DISCIPLINE (critical, the [3a] comment's load-bearing rule): a mount
	// DESTINATION leaf is the mountpoint, created by the mount machinery at restore.
	// Pre-creating a FILE-bind leaf as a DIRECTORY is a type mismatch that kills the
	// gofer. So these leaves must be ABSENT from the lower (their ancestors ARE
	// seeded, asserted above). The SA-token leaf is asserted at its NORMALIZED
	// /run/secrets/... path (ka-0000005421) — the var/run/* form is gone (var/run is a
	// symlink). /work and /tmp are single-component mounts with no ancestors, so they
	// must be absent too.
	for _, leaf := range []string{
		"etc/resolv.conf", "etc/hosts", "dev/termination-log",
		"run/secrets/kubernetes.io/serviceaccount",
		"work", "tmp",
	} {
		if _, err := os.Lstat(filepath.Join(lower, leaf)); !os.IsNotExist(err) {
			t.Errorf("LEAF DISCIPLINE: mount-destination leaf %q must NOT be pre-created in the lower (the mount creates it with the correct type at restore; a dir here is a type mismatch that kills the gofer) — err=%v", leaf, err)
		}
	}

	// (6) The .gvisor.filestore.* must be STRIPPED from both the relocated lower and
	// every stacked image layer (runsc's overlay guard rejects a leftover filestore).
	for _, layer := range opts.stack {
		entries, err := os.ReadDir(layer)
		if err != nil {
			t.Errorf("read stack layer %q: %v", layer, err)
			continue
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".gvisor.filestore.") {
				t.Errorf("stack layer %q must have its %q stripped by [3a]", layer, e.Name())
			}
		}
	}

	// (7) FORENSIC SENTINELS the controller parses for its post-state assertions must
	// be emitted, with the stacked-count and skeleton-count attributing the work.
	for _, want := range []string{
		"FT_STACKED_LOWERS_agent=2", // both image layers (upper + base) stacked
		"FT_SKELETON_DIRS_agent=",   // skeleton dentry count emitted
		"FT_RELOC_FSTYPE_agent=",    // reflink-capability signal
		"FT_RELOC_IMG_MS_agent=",    // image-stack timing
		"FT_VARRUN_SYMLINK_agent=1", // var/run recreated as a symlink->/run (ka-0000005421)
		// relocation reflink-engagement counters (ka-0000007174): the smoking-gun signal
		// FT_RELOC_FSTYPE (dest-only) could not give. Presence + accounting asserted in (8).
		"FT_RELOC_REFLINK_HITS_agent=",
		"FT_RELOC_REFLINK_FALLBACKS_agent=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("freeze [3a] must emit forensic sentinel %q\n--- output ---\n%s", want, out)
		}
	}

	// (8) REFLINK-ENGAGEMENT COUNTER ACCOUNTING (ka-0000007174). The counters mirror the
	// .gofer sweep's FT_GOFER_REFLINK_HITS/FALLBACKS but are per-SEGMENT (the layer copy
	// stays cp -a for whiteout fidelity, so a per-file ioctl tally is impossible). The
	// load-bearing, node-fs-INDEPENDENT invariant: every stacked segment is accounted for
	// as either a reflink HIT (same-device + CoW-capable dest fs) or a FALLBACK (full byte
	// copy) — so hits+fallbacks == FT_STACKED_LOWERS. This proves the counter is wired to
	// the stack loop and not stuck at 0/0, regardless of whether the test fs supports
	// FICLONE. The hits-vs-fallbacks SPLIT is fs-dependent (asserted probe-gated below).
	fields := parseSentinels(out)
	hits, okH := intField(fields, "FT_RELOC_REFLINK_HITS_agent")
	falls, okF := intField(fields, "FT_RELOC_REFLINK_FALLBACKS_agent")
	stacked, okS := intField(fields, "FT_STACKED_LOWERS_agent")
	if !okH || !okF || !okS {
		t.Fatalf("reflink counters + stacked count must all parse (hits ok=%v falls ok=%v stacked ok=%v):\n%s", okH, okF, okS, out)
	}
	if stacked <= 0 {
		t.Fatalf("fixture must stack >0 segments to exercise the reflink counter; got FT_STACKED_LOWERS_agent=%d", stacked)
	}
	if hits+falls != stacked {
		t.Errorf("reflink counters must account for every stacked segment: hits=%d + fallbacks=%d = %d, want FT_STACKED_LOWERS_agent=%d\n%s",
			hits, falls, hits+falls, stacked, out)
	}
	// The hits==stacked / fallbacks==0 guard is only sound on a CoW-capable fs; the sandbox
	// tmpfs/overlay may not support FICLONE, so gate it on a real `cp --reflink=always`
	// capability probe and skip cleanly otherwise (never an unconditional assertion — the
	// counter-and-testability kill-shot).
	//
	// HONESTY CAVEAT (WP4/WP8, ka-0000001867): this asserts only that the STRUCTURAL counter
	// scored every same-device segment as a HIT — it does NOT prove the cp's FICLONE ioctl
	// actually shared extents (the counter keys off DSTCOW && same-st_dev, and the layer cp
	// swallows per-file errors + always returns true; see freezethaw_node.go keyRelocReflinkHits).
	// hits>0 is NECESSARY-not-SUFFICIENT. The behavioral proof that a reflink genuinely engaged
	// (observing the real shared-extent outcome) lives in
	// TestFreeze_DirMedium3aReflinkSharesExtents, which is why this sub-assertion is careful not
	// to over-claim in its failure message.
	if testFSSupportsReflink(t) {
		if hits != stacked || falls != 0 {
			t.Errorf("on a reflink-capable test fs the structural reflink counter must score every same-device segment a HIT: hits=%d fallbacks=%d, want hits=%d fallbacks=0 (structural inference, NOT observed FICLONE — see TestFreeze_DirMedium3aReflinkSharesExtents for the behavioral witness)\n%s", hits, falls, stacked, out)
		}
	} else {
		t.Logf("test fs does not support cp --reflink=always; skipping the hits==stacked/fallbacks==0 sub-assertion (counter accounting hits+fallbacks==%d already verified)", stacked)
	}
}

// testFSSupportsReflink probes whether the test tempdir's filesystem supports
// copy-on-write reflinks (cp --reflink=always). Used to gate the "reflink actually
// engaged" sub-assertions so they skip cleanly on a tmpfs/ext4 CI runner while still
// firing on an xfs(reflink=1)/btrfs runner — the same capability the freeze script's
// FT_RELOC_REFLINK counters key off on-node.
func testFSSupportsReflink(t *testing.T) bool {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "rlprobe.src")
	dst := filepath.Join(dir, "rlprobe.dst")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		return false
	}
	return exec.Command("cp", "--reflink=always", src, dst).Run() == nil
}

// TestFreeze_SelfMediumSkips3a_ZeroBlastRadius proves both (a) the zero-blast-radius
// guarantee — under the self/default medium the [3a] gate is skipped, so the block's
// ONLY effect is the relocation cp (no stacking, no skeleton, .overlay-opts =
// lowerdir=$LOWER) — and (b) the negative control: given the SAME sparse fixture the
// dir-medium path completes (TestFreeze_DirMedium3aProducesCompleteLower) while the
// self path leaves the lower sparse. That attributes the dir-medium completeness to
// [3a], not to the fixture, so the core test cannot pass vacuously.
//
// NOTE on realism: on a REAL self-medium node the live merged rootfs is NOT sparse
// (the self filestore is self-contained), so the cp produces a complete lower there.
// This test feeds the self path the same artificially-sparse input ONLY to isolate
// "[3a] did nothing extra" — it asserts the BLOCK BEHAVIOR (gate skipped), not the
// realism of a self-medium lower.
func TestFreeze_SelfMediumSkips3a_ZeroBlastRadius(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractFreeze3aBlock(t, m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90}))
	bundle, taskDir, cid, dstn, imgBase, upper := sparseAgentFixture(t)
	ovl := "upperdir=" + upper + ",lowerdir=" + imgBase

	// Empty OVERLAY_FLAG == self/default medium -> the [3a] `if [ -n "$OVERLAY_FLAG" ]`
	// gate is false at runtime, so the block runs only the relocation cp.
	out := runFreeze3a(t, block, bundle, taskDir, cid, dstn, ovl, "")

	opts := parseOverlayOpts(t, bundle, dstn)
	lower := filepath.Join(bundle, dstn, "lower")

	// (a) zero blast radius: .overlay-opts is exactly lowerdir=$LOWER — no stacking.
	if len(opts.stack) != 1 || opts.stack[0] != lower {
		t.Errorf("self-medium .overlay-opts must be exactly lowerdir=%q (no [3a] stacking); got %v", lower, opts.stack)
	}
	// No GC-safe image layers preserved.
	if matches, _ := filepath.Glob(filepath.Join(bundle, dstn, "img.*")); len(matches) != 0 {
		t.Errorf("self-medium must NOT preserve image lowerdirs; found %v", matches)
	}
	// No [3a] forensic sentinels under self medium (the gated block never runs). The two
	// FT_RELOC_REFLINK_HITS/FALLBACKS counters (ka-0000007174) are in the notWant list
	// because they are emitted ONLY inside the `if [ -n "$OVERLAY_FLAG" ]` dir-medium
	// branch (freezethaw_node.go [3a-stack], ~L1123-1124); the self/default-medium `else`
	// branch emits only FT_RELOC_FSTYPE. Asserting their ABSENCE here guards against a
	// future regression that leaks the per-segment reflink tally onto the zero-blast-radius
	// self path (gd-aCi03D9tZ3 W4).
	for _, notWant := range []string{
		"FT_STACKED_LOWERS_", "FT_SKELETON_DIRS_", "FT_RELOC_IMG_MS_", "FT_VARRUN_SYMLINK_",
		"FT_RELOC_REFLINK_HITS_", "FT_RELOC_REFLINK_FALLBACKS_",
	} {
		if strings.Contains(out, notWant) {
			t.Errorf("self-medium must NOT emit the dir-medium sentinel %q (gate skipped)\n--- output ---\n%s", notWant, out)
		}
	}

	// (b) negative control: the self-medium lower is the sparse cp — given this sparse
	// fixture it ENOENTs the base content + cwd. (Same fixture the dir-medium test
	// proved COMPLETE.) This is what guarantees the dir-medium completeness is due to
	// [3a]: drop [3a] and the dir-medium lower would look like this sparse one.
	for _, rel := range []string{"usr/bin/bash", "home/agent/workspace", "etc/passwd", "usr"} {
		if _, err := os.Lstat(filepath.Join(lower, rel)); !os.IsNotExist(err) {
			t.Errorf("self-medium (cp-only) lower must remain sparse for %q (err=%v) — if it is complete here, [3a] is not what completes the dir-medium lower", rel, err)
		}
	}
	// The relocation cp itself still happened (the lower exists and carries the agent
	// top-layer file + preserved symlinks) — self medium preserves the lower, it just
	// does not [3a]-complete it.
	if _, err := os.Lstat(filepath.Join(lower, "entrypoint.sh")); err != nil {
		t.Errorf("self-medium must still relocate the merged rootfs into the lower (entrypoint.sh missing): %v", err)
	}
}

// TestFreeze_DirMedium3aSkeletonMatchesSpecData verifies the [3a] skeleton is
// DATA-DRIVEN from the captured spec (not a hardcoded set): adding a new mount
// destination to config.json must extend the seeded ancestor chain. This catches a
// regression that hardcodes the skeleton or drops the spec read, which a render
// grep cannot. It asserts the FT_SKELETON_DIRS count tracks the spec.
func TestFreeze_DirMedium3aSkeletonMatchesSpecData(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractFreeze3aBlock(t, m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90}))
	bundle, taskDir, cid, dstn, imgBase, upper := sparseAgentFixture(t)
	ovl := "upperdir=" + upper + ",lowerdir=" + imgBase

	// Rewrite the spec to add a NEW deep mount destination not in the base fixture.
	// Its ancestor chain (opt, opt/extra) must appear in the lower; its leaf
	// (opt/extra/deep) must NOT (leaf discipline).
	cfg := `{"process":{"cwd":"/home/agent/workspace","user":{"uid":1000,"gid":1000}},` +
		`"mounts":[{"destination":"/opt/extra/deep/mountpoint"}]}`
	mustWrite(t, filepath.Join(bundle, dstn, "config.json"), cfg)

	runFreeze3a(t, block, bundle, taskDir, cid, dstn, ovl, "root:dir=/var/lib/gardenio/overlay")
	lower := filepath.Join(bundle, dstn, "lower")

	// Ancestors of the new mount destination must be seeded (data-driven), leaf must not.
	for _, rel := range []string{"opt", "opt/extra", "opt/extra/deep"} {
		if fi, err := os.Lstat(filepath.Join(lower, rel)); err != nil || !fi.IsDir() {
			t.Errorf("data-driven skeleton must seed mount-dest ancestor %q from the spec: err=%v", rel, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(lower, "opt/extra/deep/mountpoint")); !os.IsNotExist(err) {
		t.Errorf("LEAF DISCIPLINE: the mount-destination leaf opt/extra/deep/mountpoint must NOT be seeded (err=%v)", err)
	}
	// The cwd chain + the hardcoded symlink targets are seeded regardless of the spec.
	for _, rel := range []string{"home/agent/workspace", "usr", "usr/bin", "usr/lib", "usr/lib64", "usr/sbin"} {
		if fi, err := os.Lstat(filepath.Join(lower, rel)); err != nil || !fi.IsDir() {
			t.Errorf("skeleton must always seed %q (cwd chain / symlink target): err=%v", rel, err)
		}
	}
}

// TestFreeze_DirMedium3aVarRunResolvesToRunSecrets is the ka-0000005421 regression
// guard: it runs the REAL freeze [3a] block against the canary-#10 fixture (the
// projected SA + IRSA token mounts, raw /var/run/* in the early-captured spec) and
// proves the reconstructed lower has the cold-start usrmerge topology — /var/run is a
// SYMLINK -> /run, and a coop-shaped /var/run/secrets/.../token read reaches the
// /run/secrets mount landing through it.
//
// THE BUG (canary #10, prod 2026.181.12): the controller normalizes the token mount
// DESTINATIONS /var/run/secrets/* -> /run/secrets/* at restore, but the [3a] skeleton
// previously mkdir-p'd the RAW var/run/secrets/* ancestor chain as a REAL directory,
// which SHADOWED the base-image /var/run->/run usrmerge symlink. coop reads
// /var/run/secrets/.../token (rehydrate.rs:53 SA hardcoded + :415 EKS-webhook IRSA env)
// and ENOENTed against the empty shadow dir -> rehydrate fail-close. The fix normalizes
// the seeded ancestor chain to /run/secrets (where the mount binds) and recreates
// var/run as a symlink -> /run, so /var/run/secrets and /run/secrets are the same path
// again — exactly the cold-start FS topology. This extends the ka-0000003421 thaw-state
// /run-canonicalization to the SA/IRSA token mounts.
func TestFreeze_DirMedium3aVarRunResolvesToRunSecrets(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractFreeze3aBlock(t, m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90}))
	bundle, taskDir, cid, dstn, imgBase, upper := sparseAgentFixture(t)
	ovl := "upperdir=" + upper + ",lowerdir=" + imgBase

	// Canary-#10 spec: BOTH projected serviceaccount token mounts, in the RAW /var/run
	// form the freeze captures BEFORE the thaw [3] /var/run->/run rewrite runs. The cwd
	// keeps the [3a] block exercising its full path.
	cfg := `{"process":{"cwd":"/home/agent/workspace","user":{"uid":1000,"gid":1000}},` +
		`"mounts":[` +
		`{"destination":"/var/run/secrets/kubernetes.io/serviceaccount"},` +
		`{"destination":"/var/run/secrets/eks.amazonaws.com/serviceaccount"}` +
		`]}`
	mustWrite(t, filepath.Join(bundle, dstn, "config.json"), cfg)

	out := runFreeze3a(t, block, bundle, taskDir, cid, dstn, ovl, "root:dir=/var/lib/gardenio/overlay")
	lower := filepath.Join(bundle, dstn, "lower")

	// (1) var/run is a SYMLINK -> /run (not a real dir that shadows the usrmerge link).
	varRun := filepath.Join(lower, "var", "run")
	fi, err := os.Lstat(varRun)
	if err != nil {
		t.Fatalf("[3a] must recreate var/run in the reconstructed lower: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("var/run must be a SYMLINK -> /run, got a real %v — this is the canary-#10 shadow dir that ENOENTs coop's /var/run/secrets token read (ka-0000005421)", fi.Mode())
	}
	if target, lerr := os.Readlink(varRun); lerr != nil || target != "/run" {
		t.Fatalf("var/run symlink target = %q (err=%v), want \"/run\"", target, lerr)
	}

	// (2) The NORMALIZED token-mount ancestors are seeded at /run/secrets/* as REAL
	// dirs (where the restored mount actually binds) — NOT at the raw var/run/secrets/*.
	for _, rel := range []string{
		"run", "run/secrets",
		"run/secrets/kubernetes.io", "run/secrets/eks.amazonaws.com",
	} {
		if fi, err := os.Lstat(filepath.Join(lower, rel)); err != nil || !fi.IsDir() {
			t.Errorf("normalized token-mount ancestor %q must be a real dir in the lower (the /run/secrets mount landing): err=%v", rel, err)
		}
	}

	// (3) THE END-TO-END INVARIANT: a coop-shaped read of /var/run/secrets/<sa>/token
	// must reach the /run/secrets mount landing through the var/run symlink, byte-for-
	// byte. chrootResolve models how the restored sandbox resolves the path WITHIN its
	// own rootfs (an absolute /run symlink target re-roots at the lower, not the host
	// /) — the in-sandbox stand-in for the runsc 9p path walk, validated on-node by the
	// canary.
	//
	// This block discriminates BOTH halves of the fix on its own (it does not lean on
	// sections 1+2): we create ONLY the mount LEAF + token with a NON-recursive os.Mkdir
	// (modeling the mount machinery, which creates only the mountpoint at restore — leaf
	// discipline), so the SCRIPT must have already seeded the /run/secrets/<sa-parent>
	// ancestor chain for the leaf mkdir to succeed. If the normalization half regressed
	// (the script seeded the raw var/run/secrets/* ancestors instead of /run/secrets/*),
	// the /run/secrets/<sa-parent> ancestor is ABSENT -> the non-recursive Mkdir ENOENTs
	// -> RED. If the symlink half regressed, chrootResolve cannot re-root through a real
	// var/run -> RED. So neither half can silently pass here.
	for _, tc := range []struct {
		sa, token string
	}{
		{"kubernetes.io/serviceaccount", "HDR.SA.SIG"},
		{"eks.amazonaws.com/serviceaccount", "HDR.IRSA.SIG"},
	} {
		// The mount LEAF (the normalized destination) and its script-seeded ancestor.
		leaf := filepath.Join(lower, "run", "secrets", tc.sa)
		ancestor := filepath.Dir(leaf)
		// The ancestor MUST already be a script-seeded real dir (the normalization half).
		if fi, err := os.Lstat(ancestor); err != nil || !fi.IsDir() {
			t.Fatalf("the /run/secrets mount ancestor %q must be seeded as a real dir by the NORMALIZED skeleton before the mount lands its leaf (err=%v) — a dropped /var/run->/run normalization would seed var/run/secrets/* instead and this ancestor would be absent (ka-0000005421)", ancestor, err)
		}
		// Model the mount machinery: create ONLY the leaf (non-recursive — it fails if the
		// script did not seed the ancestor) + surface the token the 9p mount carries.
		if err := os.Mkdir(leaf, 0o755); err != nil {
			t.Fatalf("mount leaf %q could not be created onto the script-seeded ancestor: %v", leaf, err)
		}
		if err := os.WriteFile(filepath.Join(leaf, "token"), []byte(tc.token), 0o644); err != nil {
			t.Fatalf("write mounted token %q: %v", tc.sa, err)
		}

		// The exact /var/run-rooted path coop reads (rehydrate.rs:53 SA + :415 IRSA).
		resolved := chrootResolve(t, lower, "/var/run/secrets/"+tc.sa+"/token")
		got, rerr := os.ReadFile(resolved)
		if rerr != nil {
			t.Errorf("coop read /var/run/secrets/%s/token did NOT resolve through the var/run->/run symlink to the /run/secrets mount (resolved=%q): %v\n"+
				"This is the canary-#10 ENOENT: a real var/run shadow dir blocks the read while the normalized token mount binds at /run/secrets (ka-0000005421)", tc.sa, resolved, rerr)
			continue
		}
		if string(got) != tc.token {
			t.Errorf("coop read /var/run/secrets/%s/token = %q, want %q (the bytes the /run/secrets mount surfaces)", tc.sa, got, tc.token)
		}
	}

	// (4) The forensic sentinel attributing the symlink recreation must be emitted.
	if !strings.Contains(out, "FT_VARRUN_SYMLINK_agent=1") {
		t.Errorf("freeze [3a] must emit FT_VARRUN_SYMLINK_agent=1\n--- output ---\n%s", out)
	}
}

// TestFreeze_DirMediumStackEmptyFallsBackToMergedCp is the ka-0000007174 KS-1 guard.
// The reflink fix makes the dir-medium primary path reconstruct the lower from the real
// bare-xfs layers and DROP the merged-mount cp — but ONLY when the layer stack is
// non-empty. If $ovl is unparseable or every layer dir was GC'd (the ka-0000000655 race
// this whole feature exists for), the stack is empty and a naive skeleton-only $LOWER
// would be HOLLOW -> runsc restore ENOENTs every dentry -> rc=128, re-landing the
// cycle-14 regression. This test drives the dir medium with an $ovl whose segments do
// NOT exist on disk, and asserts the script FALLS BACK to the pre-fix merged-mount cp so
// the reconstructed lower is complete (not hollow): FT_STACKED_LOWERS_agent=0,
// .overlay-opts = lowerdir=$LOWER (single segment), and $LOWER carries the merged-cp
// content. This is the assertion that would have caught a one-line "just delete the
// merged cp" implementation.
func TestFreeze_DirMediumStackEmptyFallsBackToMergedCp(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractFreeze3aBlock(t, m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90}))
	bundle, taskDir, cid, dstn, _, _ := sparseAgentFixture(t)
	// $ovl names layer dirs that do NOT exist -> the [3a-stack] loop preserves zero
	// segments -> STACK empty -> the merged-cp fallback must fire (KS-1).
	ovl := "upperdir=/nonexistent/upper,lowerdir=/nonexistent/lower"

	out := runFreeze3a(t, block, bundle, taskDir, cid, dstn, ovl, "root:dir=/var/lib/gardenio/overlay")

	// Zero segments stacked.
	if !strings.Contains(out, "FT_STACKED_LOWERS_agent=0") {
		t.Errorf("an unparseable/GC'd $ovl must stack ZERO segments (FT_STACKED_LOWERS_agent=0)\n--- output ---\n%s", out)
	}
	// No GC-safe image layers preserved (none existed to preserve).
	if matches, _ := filepath.Glob(filepath.Join(bundle, dstn, "img.*")); len(matches) != 0 {
		t.Errorf("STACK-empty fallback must NOT create img.* dirs; found %v", matches)
	}
	// .overlay-opts is the single relocated lower — the merged cp, NOT a stacked list.
	opts := parseOverlayOpts(t, bundle, dstn)
	lower := filepath.Join(bundle, dstn, "lower")
	if len(opts.stack) != 1 || opts.stack[0] != lower {
		t.Errorf("STACK-empty fallback .overlay-opts must be exactly lowerdir=%q (the merged cp); got %v", lower, opts.stack)
	}
	// The load-bearing KS-1 invariant: $LOWER is the FULL merged cp (not a hollow
	// skeleton) — the merged rootfs content landed. entrypoint.sh + the usrmerge symlinks
	// are what the sparse fixture's merged rootfs carries.
	if _, err := os.Lstat(filepath.Join(lower, "entrypoint.sh")); err != nil {
		t.Errorf("KS-1: STACK-empty fallback must merged-cp the rootfs into $LOWER (entrypoint.sh missing = hollow lower = the cycle-14 rc=128 regression): %v", err)
	}
	for link := range map[string]string{"bin": "", "lib64": "", "sbin": ""} {
		if _, err := os.Readlink(filepath.Join(lower, link)); err != nil {
			t.Errorf("KS-1: STACK-empty fallback merged cp must preserve the %q rootfs symlink in $LOWER: %v", link, err)
		}
	}
	// The reflink counters are still emitted (0/0 here — no segments to reflink), so the
	// Go post-state parse never mistakes the fallback for an older node script.
	for _, want := range []string{"FT_RELOC_REFLINK_HITS_agent=0", "FT_RELOC_REFLINK_FALLBACKS_agent=0"} {
		if !strings.Contains(out, want) {
			t.Errorf("STACK-empty fallback must still emit %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestFreeze_DirMediumSkeletonDoesNotShadowStackContent is the ka-0000007174 type-conflict
// guard. Design B makes the skeleton-only $LOWER the TOP overlay layer; the reflinked
// image layers sit beneath. Overlay serves the TOPMOST layer at any path, so if the
// skeleton seeded a DIRECTORY at a path where a stacked layer carries a regular FILE, the
// overlay would serve the dir and the file would be unreachable (ENOTDIR at the 9p walk).
// The skeleton only ever mkdir's genuine-directory paths (mount-dest ancestors, the cwd
// chain, the usrmerge symlink TARGETS usr/usr/bin/...) so this cannot happen by
// construction — this test PROVES it by asserting no skeleton dir in $LOWER collides with
// a non-directory at the same rel path in any stacked img.* layer.
func TestFreeze_DirMediumSkeletonDoesNotShadowStackContent(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractFreeze3aBlock(t, m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90}))
	bundle, taskDir, cid, dstn, imgBase, upper := sparseAgentFixture(t)
	ovl := "upperdir=" + upper + ",lowerdir=" + imgBase
	// The fixture's imgBase already carries usr/bin/bash (a regular FILE) beneath usr/bin
	// (a dir) — exactly the DIR-OVER-FILE shadow hazard: the skeleton seeds usr + usr/bin
	// as DIRS (correct, they ARE dirs) but must never seed usr/bin/bash as a dir over the
	// file.
	//
	// W5 (gd-pnDwHr919y): also model the INVERSE direction — a cp-tlsym seeded top-level
	// SYMLINK in $LOWER vs a stacked real DIRECTORY. Give imgBase the usrmerge top-level
	// symlinks (bin->usr/bin, lib64->usr/lib64, sbin->usr/sbin) a real usrmerge base image
	// ships, matching the SAME symlinks the merged rootfs carries (fixture L162-164). This
	// is the topology under which cp-tlsym's [ -h ]-guarded copy runs: it seeds those
	// symlinks into the TOP layer $LOWER while the stacked base carries them too — so the
	// inverse-shadow assertion below has real stacked state to inspect and PROVES the
	// symlink-over-symlink case does not mask a stacked directory. (Local to this test to
	// keep the shared fixture — and the other five tests using it — untouched.)
	mustSymlink(t, "usr/bin", filepath.Join(imgBase, "bin"))
	mustSymlink(t, "usr/lib64", filepath.Join(imgBase, "lib64"))
	mustSymlink(t, "usr/sbin", filepath.Join(imgBase, "sbin"))

	runFreeze3a(t, block, bundle, taskDir, cid, dstn, ovl, "root:dir=/var/lib/gardenio/overlay")
	lower := filepath.Join(bundle, dstn, "lower")
	opts := parseOverlayOpts(t, bundle, dstn)

	// Walk the skeleton $LOWER; for every entry that is a DIRECTORY, assert no stacked
	// img.* layer has a non-directory at the same rel path (a dir-over-file shadow).
	stackLayers := opts.stack[1:] // segments beneath $LOWER (the img.* layers)
	_ = filepath.WalkDir(lower, func(p string, d os.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(lower, p)
		if rerr != nil || rel == "." {
			return nil
		}
		for _, layer := range stackLayers {
			if fi, lerr := os.Lstat(filepath.Join(layer, rel)); lerr == nil && !fi.IsDir() {
				t.Errorf("SHADOW HAZARD: skeleton dir %q in $LOWER shadows a non-directory at the same path in stacked layer %q — overlay would serve the dir and hide the file (ENOTDIR at the 9p walk)", rel, layer)
			}
		}
		return nil
	})
	// And the positive: the base-layer file resolves through the stack (not shadowed).
	if layer := opts.resolves("usr/bin/bash"); layer == "" {
		t.Errorf("usr/bin/bash must resolve through the stack (the skeleton usr/usr/bin dirs merge, they do not shadow the file)")
	}

	// W5 (gd-pnDwHr919y): the INVERSE shadow direction. cp-tlsym (freezethaw_node.go [3a]
	// ~L1132) copies the merged rootfs's top-level usrmerge SYMLINKS (bin->usr/bin,
	// lib64->usr/lib64, sbin->usr/sbin) into $LOWER — the TOP overlay layer. A symlink in
	// the top layer at a path where a STACKED layer carries a real DIRECTORY would make
	// overlay serve the symlink and HIDE the directory beneath (a symlink-over-dir shadow,
	// the mirror of the dir-over-file case above). This cannot happen because cp-tlsym is
	// `[ -h ]`-guarded (it only copies entries that ARE symlinks on the merged mount) and a
	// usrmerge base image carries the SAME symlinks (not real dirs) in its stacked layers —
	// symlink-over-symlink, both resolving to usr/*, no masking. Assert it: for every SYMLINK
	// the walk finds in $LOWER, no stacked img.* layer carries a real directory at the same
	// rel path. The walk visits 4 seeded symlinks, not just the 3 cp-tlsym usrmerge links
	// (bin->usr/bin, lib64->usr/lib64, sbin->usr/sbin): [3a-varrun] (freezethaw_node.go
	// ~L1233) also recreates var/run as a usrmerge symlink -> /run, which WalkDir visits
	// nested under var/, so seededTLSyms counts 4. That 4th (var/run) is a valid bonus
	// inverse-shadow check. (os.Lstat().IsDir() is false for a stacked symlink, so the safe
	// symlink-over-symlink case correctly does not fire — only a stacked real dir does.)
	seededTLSyms := 0
	_ = filepath.WalkDir(lower, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		rel, rerr := filepath.Rel(lower, p)
		if rerr != nil || rel == "." {
			return nil
		}
		seededTLSyms++
		for _, layer := range stackLayers {
			if fi, lerr := os.Lstat(filepath.Join(layer, rel)); lerr == nil && fi.IsDir() {
				t.Errorf("INVERSE SHADOW HAZARD: cp-tlsym symlink %q in $LOWER shadows a real DIRECTORY at the same path in stacked layer %q — overlay would serve the top-layer symlink and hide the stacked directory (ENOTDIR/wrong-type at the 9p walk) (gd-pnDwHr919y)", rel, layer)
			}
		}
		return nil
	})

	// Two-sided non-vacuity: cp-tlsym must have actually seeded the top-level usrmerge
	// symlinks into $LOWER (else the walk above inspects nothing), AND the stacked base must
	// carry those SAME entries as symlinks (not real dirs) — the usrmerge topology that makes
	// symlink-over-symlink safe. Without both, the inverse-shadow assertion could pass
	// vacuously (no top-layer symlink to check, or no stacked entry underneath it).
	if seededTLSyms == 0 {
		t.Fatal("expected cp-tlsym to seed >=1 top-level symlink into $LOWER; the walk found none — the inverse-shadow assertion would be vacuous")
	}
	for _, link := range []string{"bin", "lib64", "sbin"} {
		fi, err := os.Lstat(filepath.Join(lower, link))
		if err != nil {
			t.Errorf("cp-tlsym must seed top-level %q into $LOWER as a symlink, but $LOWER/%s is absent: %v — the merged-rootfs usrmerge link the restore 9p-walk expects on the top layer (ka-0000007174 KS-2)", link, link, err)
		} else if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("cp-tlsym seeded top-level %q into $LOWER as a %v, want a symlink — the merged-rootfs usrmerge link the restore 9p-walk expects on the top layer (ka-0000007174 KS-2)", link, fi.Mode())
		}
		found := false
		for _, layer := range stackLayers {
			lfi, lerr := os.Lstat(filepath.Join(layer, link))
			if lerr != nil {
				continue
			}
			found = true
			// A stacked real dir here IS the hazard — the walk above already reports it; this
			// makes the intent explicit at the fixture level.
			if lfi.IsDir() {
				t.Errorf("stacked layer %q carries top-level %q as a real directory — cp-tlsym's top-layer symlink would shadow it; a usrmerge base must carry it as a symlink", layer, link)
			}
		}
		if !found {
			t.Errorf("expected a stacked img.* layer to carry top-level %q (as a usrmerge symlink) so the inverse-shadow check clears REAL stacked state, not an absent path", link)
		}
	}
}

// --- small fixture helpers (fail-loud wrappers) ---

func mustMkdirAll(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
}

func mustWrite(t *testing.T, p, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatalf("mkdir parent of %s: %v", p, err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink %s -> %s: %v", link, target, err)
	}
}

// chrootResolve resolves an absolute in-sandbox path (e.g. /var/run/secrets/.../token)
// against a rootfs dir, following symlinks the way a chroot does: an ABSOLUTE symlink
// target (like /var/run -> /run) re-roots at `rootfs`, not the host /. It is the
// in-sandbox stand-in for the runsc 9p path walk (the real walk is unreachable without
// runsc on a node — validated on-node by the canary) and lets the ka-0000005421 test
// prove a /var/run-rooted coop read reaches the /run-rooted mount landing. Returns the
// host filesystem path the in-sandbox path resolves to; bounded to MaxSymlinkHops so a
// symlink cycle fails the test loud rather than hanging.
func chrootResolve(t *testing.T, rootfs, sandboxPath string) string {
	t.Helper()
	const maxHops = 40
	cur := strings.TrimPrefix(sandboxPath, "/")
	for hop := 0; ; hop++ {
		if hop > maxHops {
			t.Fatalf("chrootResolve %q: exceeded %d symlink hops (cycle?)", sandboxPath, maxHops)
		}
		parts := strings.Split(cur, "/")
		// Walk component by component so an intermediate symlink (var/run) re-roots the
		// remainder of the walk, exactly like the kernel/9p resolver.
		acc := ""
		redirected := false
		for i, part := range parts {
			if part == "" {
				continue
			}
			next := filepath.Join(acc, part)
			fi, err := os.Lstat(filepath.Join(rootfs, next))
			if err != nil {
				// ENOENT (or other) at this component — return the best-effort host path
				// so the caller's ReadFile produces the real, attributable error.
				return filepath.Join(rootfs, cur)
			}
			if fi.Mode()&os.ModeSymlink != 0 {
				target, lerr := os.Readlink(filepath.Join(rootfs, next))
				if lerr != nil {
					t.Fatalf("chrootResolve readlink %q: %v", next, lerr)
				}
				rest := strings.Join(parts[i+1:], "/")
				if abs, ok := strings.CutPrefix(target, "/"); ok {
					// ABSOLUTE target re-roots at the sandbox root (the chroot semantics).
					cur = abs
				} else {
					// RELATIVE target resolves against the symlink's parent dir.
					cur = filepath.Join(acc, target)
				}
				if rest != "" {
					cur = filepath.Join(cur, rest)
				}
				redirected = true
				break
			}
			acc = next
		}
		if !redirected {
			return filepath.Join(rootfs, cur)
		}
	}
}
