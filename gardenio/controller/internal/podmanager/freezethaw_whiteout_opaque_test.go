package podmanager

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// freezethaw_whiteout_opaque_test.go is the WP1/WP6 fidelity guard (ka-0000001867).
//
// THE STRUCTURAL GAP IT CLOSES. sparseAgentFixture (freezethaw_behavioral_test.go) writes ONLY
// regular files, dirs and symlinks, and opts.resolves() models "exists in ANY layer" — NOT
// overlay MASKING. So the whole behavioral suite was structurally INCAPABLE of catching the two
// whiteout-safety regressions the reflink fix's WHITEOUT SAFETY note (freezethaw_node.go ~L1042)
// exists to prevent:
//
//   - a char-device 0:0 WHITEOUT (overlayfs's "this file is deleted in this layer" marker)
//     degraded by a per-file python copy into an EMPTY REGULAR FILE — silently resurrecting a
//     file the agent deleted at runtime; and
//   - a trusted.overlay.opaque XATTR (overlayfs's "this dir fully masks lower dirs" marker)
//     dropped by a copy that does not --preserve=all — silently un-masking a directory.
//
// The [3a] layer copy stays coreutils `cp -a --reflink=auto` (= -dR --preserve=all, run as root
// via nsenter so it has CAP_MKNOD + the trusted xattr namespace) EXACTLY so both survive. These
// tests make the suite able to FAIL if a future edit swaps that cp for a data-only walker: they
// run the real [3a] layer-copy shell against a fixture that carries a whiteout + an opaque dir
// and assert both round-trip intact, with a built-in negative control (a data-only copy DOES
// degrade them) so the assertion is non-vacuous.
//
// WHY A DIRECT FS ASSERTION, NOT AN FT_WHITEOUT_*/FT_OPAQUE_* NODE SENTINEL. The task allowed
// emitting node sentinels "optionally" to make the round-trip machine-checkable. We assert the
// round-trip DIRECTLY on the copied layer (char-device type + dev 0:0, xattr present + value)
// instead: that is a STRONGER, less-brittle check than parsing an echoed count — it inspects the
// actual on-disk artifact the restore-time overlay reads, and it needs no freeze-path shell
// change (zero behaviour-change risk on the release lane, per the guardrails).

// testFSSupportsMknodChardev reports whether the test fs + process can create a char-device 0:0
// (the overlay whiteout node). Needs CAP_MKNOD; skips cleanly on a CI runner without it.
func testFSSupportsMknodChardev(t *testing.T) bool {
	t.Helper()
	p := filepath.Join(t.TempDir(), "wh.probe")
	if err := syscall.Mknod(p, syscall.S_IFCHR|0o644, 0); err != nil {
		t.Logf("cannot mknod a char-device 0:0 on the test fs (need CAP_MKNOD): %v — skipping the whiteout round-trip", err)
		return false
	}
	return true
}

// opaqueXattr picks the xattr namespace the test can actually write on this fs. Production uses
// trusted.overlay.opaque (the real overlay marker), which needs CAP_SYS_ADMIN; an unprivileged
// CI/sandbox process cannot set it, so we fall back to a user.* xattr — same cp --preserve=all
// round-trip path, just a namespace the test is allowed to write. Returns ("", "" , false) when
// NEITHER can be set (skip the opaque half). Prefers the real trusted.overlay.opaque when
// available so a privileged runner exercises the exact production marker.
func opaqueXattr(t *testing.T) (name, value string, ok bool) {
	t.Helper()
	dir := t.TempDir()
	for _, cand := range []string{"trusted.overlay.opaque", "user.overlay.opaque.test"} {
		probe := filepath.Join(dir, "xa.probe.d")
		if mkErr := os.Mkdir(probe, 0o755); mkErr != nil {
			t.Fatalf("mkdir xattr probe: %v", mkErr)
		}
		if err := syscall.Setxattr(probe, cand, []byte("y"), 0); err == nil {
			return cand, "y", true
		}
		_ = os.RemoveAll(probe)
	}
	t.Logf("cannot set trusted.overlay.opaque OR a user.* xattr on the test fs — skipping the opaque-xattr round-trip")
	return "", "", false
}

// nodeLayerCopy runs the EXACT [3a-stack] layer-copy shell (freezethaw_node.go ~L1099):
// cp -a --reflink=auto "$seg/." "$img/" with the .gvisor.filestore.* strip + the 2>/dev/null;true
// guard. This is the copy whose whiteout/opaque fidelity WP1/WP6 pins.
func nodeLayerCopy(t *testing.T, seg, img string) {
	t.Helper()
	if err := os.MkdirAll(img, 0o755); err != nil {
		t.Fatal(err)
	}
	const cp = `cp -a --reflink=auto "$1/." "$2/" 2>/dev/null && rm -f "$2"/.gvisor.filestore.* 2>/dev/null; true`
	if out, err := exec.Command("bash", "-c", cp, "cp-img", seg, img).CombinedOutput(); err != nil {
		t.Fatalf("[3a] layer cp failed: %v\n%s", err, out)
	}
}

// isChardevZero reports whether path is a character-special device with rdev 0 (the overlay
// whiteout node, dev 0:0) — the property a degrading copy would destroy (an empty regular file
// instead). detail describes what the path actually is, for a legible failure message.
func isChardevZero(t *testing.T, path string) (isWhiteout bool, detail string) {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		return false, "lstat error: " + err.Error()
	}
	switch st.Mode & syscall.S_IFMT {
	case syscall.S_IFCHR:
		return st.Rdev == 0, fmt.Sprintf("char-device (rdev=%d, want 0)", st.Rdev)
	case syscall.S_IFREG:
		return false, fmt.Sprintf("REGULAR FILE (%d bytes) — whiteout DEGRADED!", st.Size)
	default:
		return false, fmt.Sprintf("mode %#o (not a char-device)", st.Mode)
	}
}

// TestFreeze_DirMedium3aPreservesWhiteoutCharDev is the WP1 guard: a char-device 0:0 whiteout in
// a container layer MUST survive the [3a] `cp -a --reflink=auto` into the stacked img.* layer as
// a char-device 0:0, NOT degrade to an empty regular file. Non-vacuity: a data-only copy of the
// SAME source degrades it (proving the fixture models a real whiteout and the assertion bites).
func TestFreeze_DirMedium3aPreservesWhiteoutCharDev(t *testing.T) {
	if !testFSSupportsMknodChardev(t) {
		t.Skip("test fs/process cannot create a char-device 0:0 whiteout")
	}
	root := t.TempDir()
	seg := filepath.Join(root, "seg")
	if err := os.MkdirAll(filepath.Join(seg, "usr/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The overlay whiteout: char-device 0:0 at a path the layer "deletes" from the layers below.
	wh := filepath.Join(seg, "usr/bin/removed-tool")
	if err := syscall.Mknod(wh, syscall.S_IFCHR|0o644, 0); err != nil {
		t.Fatalf("mknod whiteout: %v", err)
	}
	// A regular file alongside it (the copy must carry both — whiteout AND real content).
	if err := os.WriteFile(filepath.Join(seg, "usr/bin/kept-tool"), []byte("ELF\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	img := filepath.Join(root, "img.0")
	nodeLayerCopy(t, seg, img)

	// WP1 assertion: the whiteout round-trips as a char-device 0:0.
	if ok, detail := isChardevZero(t, filepath.Join(img, "usr/bin/removed-tool")); !ok {
		t.Errorf("[3a] cp -a must preserve the char-device 0:0 whiteout so the overlay still masks the deleted file at restore; got %s — a degraded whiteout silently RESURRECTS a runtime-deleted file (ka-0000001867 WP1)", detail)
	}
	// The real file must also be there (the copy is not just preserving the whiteout).
	if _, err := os.Lstat(filepath.Join(img, "usr/bin/kept-tool")); err != nil {
		t.Errorf("[3a] cp must also carry the regular file alongside the whiteout: %v", err)
	}

	// NON-VACUITY: model the EXACT degradation the freezethaw_node.go WHITEOUT SAFETY note
	// warns about — "a shutil.copy2/copytree would open a 0:0 whiteout as a regular file and
	// materialize an EMPTY regular file, silently DESTROYING the deletion marker." A python
	// walker that shutil.copy2's each entry does precisely this: it raises on the char-device
	// (No such device or address) and falls back to an empty regular file. That degraded copy
	// must NOT reproduce the char-device 0:0 — proving the fixture carries a genuine whiteout
	// and the WP1 assertion above actually distinguishes a faithful cp -a from a data-only
	// walker (which is why the [3a] copy is cp -a and NOT a python copytree).
	degraded := filepath.Join(root, "img.degraded")
	const walker = `import os,shutil,sys
src,dst=sys.argv[1],sys.argv[2]
for r,dirs,files in os.walk(src):
    rel=os.path.relpath(r,src)
    for d in dirs: os.makedirs(os.path.join(dst,rel,d),exist_ok=True)
    for f in files:
        s=os.path.join(r,f); t=os.path.join(dst,rel,f)
        os.makedirs(os.path.dirname(t),exist_ok=True)
        try: shutil.copy2(s,t)            # opens the 0:0 char-device for read -> OSError
        except OSError: open(t,"w").close()  # the documented bug: an EMPTY regular file`
	if err := os.MkdirAll(degraded, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("python3", "-c", walker, seg, degraded).CombinedOutput(); err != nil {
		t.Fatalf("degrading walker failed to run: %v\n%s", err, out)
	}
	if ok, detail := isChardevZero(t, filepath.Join(degraded, "usr/bin/removed-tool")); ok {
		t.Errorf("NON-VACUITY BROKEN: the data-only shutil walker reproduced the char-device 0:0 whiteout (%s) — the fixture/assertion does not actually distinguish a faithful cp -a from a degrading walker, so the WP1 guard proves nothing", detail)
	}
}

// TestFreeze_DirMedium3aPreservesOpaqueXattr is the WP6 guard: an overlay opaque-dir xattr MUST
// survive the [3a] `cp -a --reflink=auto` (= --preserve=all). A copy that drops the xattr
// silently un-masks a directory the layer meant to fully shadow. Non-vacuity: a
// --no-preserve=all copy drops it.
func TestFreeze_DirMedium3aPreservesOpaqueXattr(t *testing.T) {
	name, value, ok := opaqueXattr(t)
	if !ok {
		t.Skip("cannot set an overlay-opaque-style xattr on the test fs")
	}
	root := t.TempDir()
	seg := filepath.Join(root, "seg")
	od := filepath.Join(seg, "etc/fully-masked")
	if err := os.MkdirAll(od, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Setxattr(od, name, []byte(value), 0); err != nil {
		t.Fatalf("set opaque xattr %q: %v", name, err)
	}

	img := filepath.Join(root, "img.0")
	nodeLayerCopy(t, seg, img)

	// WP6 assertion: the opaque xattr round-trips with its value intact.
	got := make([]byte, 64)
	n, err := syscall.Getxattr(filepath.Join(img, "etc/fully-masked"), name, got)
	if err != nil {
		t.Errorf("[3a] cp -a (--preserve=all) must preserve the overlay opaque xattr %q so the dir stays masked at restore; got err=%v — a dropped opaque xattr silently UN-MASKS a directory (ka-0000001867 WP6)", name, err)
	} else if string(got[:n]) != value {
		t.Errorf("opaque xattr %q value = %q, want %q", name, got[:n], value)
	}

	// NON-VACUITY: a --no-preserve=all copy of the SAME dir drops the xattr, proving the
	// assertion above actually depends on cp -a's --preserve=all (not on the fs preserving
	// xattrs regardless of the copy mode).
	degraded := filepath.Join(root, "img.degraded")
	if err := os.MkdirAll(degraded, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-c", `cp -R --no-preserve=all "$1/." "$2/" 2>/dev/null; true`, "degrade", seg, degraded).CombinedOutput(); err != nil {
		t.Fatalf("degraded copy failed: %v\n%s", err, out)
	}
	if _, err := syscall.Getxattr(filepath.Join(degraded, "etc/fully-masked"), name, make([]byte, 64)); err == nil {
		t.Errorf("NON-VACUITY BROKEN: a --no-preserve=all copy kept the opaque xattr %q — the WP6 assertion does not actually depend on cp -a's --preserve=all, so it proves nothing", name)
	}
}
