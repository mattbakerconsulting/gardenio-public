package podmanager

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// freezethaw_reflink_engagement_test.go is the WP4/WP8 behavioral witness (ka-0000001867).
//
// THE GAP IT CLOSES. FT_RELOC_REFLINK_HITS is a STRUCTURAL inference: the freeze script scores
// a HIT when the dest fs is CoW-capable (DSTCOW==1) AND the segment is on the same device
// (SEGDEV==DSTDEV), NOT by reading back the cp's per-file FICLONE result — the layer copy is
// coreutils `cp -a --reflink=auto` wrapped in `2>/dev/null; true`, so it swallows any per-file
// clone error and always returns success (freezethaw_node.go keyRelocReflinkHits). So HITS>0
// alone does not PROVE the bytes were shared; it proves the CONDITIONS for a reflink held. The
// behavioral test in freezethaw_behavioral_test.go can therefore only assert the counter
// scored a hit — it cannot observe the ioctl outcome.
//
// This test closes the "observe the actual FICLONE outcome" half of WP4/WP8: it runs the EXACT
// [3a] layer-copy shell (`cp -a --reflink=auto "$seg/." "$img/"`) against a real segment tree
// on the test fs and, when that fs is genuinely reflink-capable, inspects `filefrag`'s per-
// extent `shared` flag to confirm the copied file's data extents are SHARED with the source
// (a real reflink), NOT independently allocated (a full byte copy). The full-copy control
// (`--reflink=never` of the same bytes) MUST show NO shared extent — that non-vacuity check
// proves the discriminator actually distinguishes the two, so a green result is meaningful and
// not just "filefrag always says shared." On a non-CoW CI fs (ext4/tmpfs/9p) the whole test
// SKIPS cleanly — it is never an unconditional assertion.

// probeReflinkShared reports whether the test can (a) reflink at all on `dir`'s fs and (b) tell
// a reflinked file from a full copy via filefrag's shared-extent flag. It returns false (skip)
// if cp --reflink=always fails, filefrag is absent, or the discriminator does not cleanly
// separate a reflink from a --reflink=never full copy on this fs. This is stricter than
// testFSSupportsReflink: it validates the OBSERVATION MECHANISM, not just the capability.
func probeReflinkShared(t *testing.T) bool {
	t.Helper()
	if _, err := exec.LookPath("filefrag"); err != nil {
		t.Logf("filefrag not on PATH; skipping the shared-extent behavioral witness")
		return false
	}
	dir := t.TempDir()
	// A multi-block file so an extent exists to share (a tiny file may be inlined).
	data := make([]byte, 2<<20)
	for i := range data {
		data[i] = byte(i * 7)
	}
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, data, 0o644); err != nil {
		return false
	}
	rl := filepath.Join(dir, "rl.bin")
	if exec.Command("cp", "--reflink=always", src, rl).Run() != nil {
		t.Logf("cp --reflink=always failed on the test fs; skipping the shared-extent behavioral witness")
		return false
	}
	full := filepath.Join(dir, "full.bin")
	if err := exec.Command("cp", "--reflink=never", src, full).Run(); err != nil {
		return false
	}
	// The discriminator MUST separate the two: reflink shared, full copy not. If it does not
	// (some fs report shared spuriously, or filefrag lacks the flag), the witness is
	// meaningless here — skip rather than assert against a broken oracle.
	rlShared, _ := fileHasSharedExtent(t, rl)
	fullShared, _ := fileHasSharedExtent(t, full)
	if !rlShared || fullShared {
		t.Logf("filefrag shared-extent flag does not cleanly separate reflink(%v) from full-copy(%v) on this fs; skipping the behavioral witness", rlShared, fullShared)
		return false
	}
	return true
}

// fileHasSharedExtent parses `filefrag -v <path>` and reports whether ANY extent carries the
// `shared` flag — the kernel's signal that the file's data blocks are shared with another
// inode (a reflink / CoW clone), as opposed to independently allocated (a full copy). Returns
// the raw filefrag output too for failure diagnostics.
func fileHasSharedExtent(t *testing.T, path string) (bool, string) {
	t.Helper()
	out, err := exec.Command("filefrag", "-v", path).CombinedOutput()
	if err != nil {
		t.Fatalf("filefrag -v %s failed: %v\n%s", path, err, out)
	}
	for ln := range strings.SplitSeq(string(out), "\n") {
		// The flags column is the last field of each extent row; `shared` appears there for a
		// CoW-shared extent. Skip the header row (it literally contains "flags:") and match on
		// the whole word so a path or a header substring cannot false-hit.
		if strings.Contains(ln, "flags:") {
			continue // "ext: logical_offset: ... flags:" column header
		}
		toks := strings.FieldsFunc(ln, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
		if slices.Contains(toks, "shared") {
			return true, string(out)
		}
	}
	return false, string(out)
}

// TestFreeze_DirMedium3aReflinkSharesExtents is the behavioral proof (WP4/WP8) that the [3a]
// layer copy REALLY reflinks — it observes the shared-extent outcome, not the structural
// counter. It runs the exact node cp shape against a real segment and asserts the copied
// file's extents are shared with the source. SKIPS on a non-CoW fs (the counter-and-
// testability discipline: never an unconditional assertion the CI fs cannot honour).
func TestFreeze_DirMedium3aReflinkSharesExtents(t *testing.T) {
	if !probeReflinkShared(t) {
		t.Skip("test fs is not reflink-capable / observable; the shared-extent witness cannot run here")
	}

	root := t.TempDir()
	// A realistic base-image layer segment: a multi-block regular file (an .so the merged
	// view backfills) is what actually reflinks — dirs/symlinks carry no data extents.
	seg := filepath.Join(root, "seg")
	if err := os.MkdirAll(filepath.Join(seg, "usr/lib"), 0o755); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(seg, "usr/lib/libbig.so")
	data := make([]byte, 4<<20) // 4 MiB → several fs blocks, guaranteed extents
	for i := range data {
		data[i] = byte(i * 13)
	}
	if err := os.WriteFile(big, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// THE EXACT [3a-stack] cp shape (freezethaw_node.go ~L1099): cp -a --reflink=auto "$1/." "$2/".
	img := filepath.Join(root, "img.0")
	if err := os.MkdirAll(img, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-c", `cp -a --reflink=auto "$1/." "$2/"`, "cp-img", seg, img).CombinedOutput(); err != nil {
		t.Fatalf("[3a] layer cp failed: %v\n%s", err, out)
	}

	// THE WITNESS: the copied file's data extents must be SHARED with the source (a real
	// FICLONE), proving the reflink genuinely engaged — the thing HITS>0 can only infer.
	dstBig := filepath.Join(img, "usr/lib/libbig.so")
	shared, ff := fileHasSharedExtent(t, dstBig)
	if !shared {
		t.Errorf("the [3a] cp -a --reflink=auto must SHARE extents on a reflink-capable fs (real FICLONE), but filefrag shows no shared extent — the copy fell back to a full byte copy despite a CoW-capable dest (the exact silent-fallback the structural HITS counter cannot see):\n%s", ff)
	}

	// NON-VACUITY: a --reflink=never copy of the identical bytes must NOT share extents, so a
	// green result above reflects a real reflink and not a filefrag oracle that always reports
	// shared. (probeReflinkShared already checked this on a throwaway file; re-assert on the
	// actual segment content so the guard travels with this test.)
	full := filepath.Join(root, "img.full")
	if err := os.MkdirAll(full, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("bash", "-c", `cp -a --reflink=never "$1/." "$2/"`, "cp-full", seg, full).CombinedOutput(); err != nil {
		t.Fatalf("full-copy control cp failed: %v\n%s", err, out)
	}
	if fullShared, ffFull := fileHasSharedExtent(t, filepath.Join(full, "usr/lib/libbig.so")); fullShared {
		t.Errorf("non-vacuity: a --reflink=never full copy must NOT share extents (else the shared-extent witness proves nothing); filefrag:\n%s", ffFull)
	}
}
