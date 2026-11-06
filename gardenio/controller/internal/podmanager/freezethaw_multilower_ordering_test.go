package podmanager

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// freezethaw_multilower_ordering_test.go is the WP2 guard (ka-0000001867).
//
// THE STRUCTURAL GAP IT CLOSES. Every existing [3a] behavioral test uses a SINGLE image
// lowerdir (sparseAgentFixture's $ovl is "upperdir=<upper>,lowerdir=<imgBase>"). A real agent
// image has MANY containerd layers, and overlay precedence is ORDER-SENSITIVE: the container
// upperdir wins over the image lowerdirs, and among lowerdirs the earlier-declared layer wins
// over the later one. An ordering-inversion regression — an upper whiteout landing BELOW the
// lower it is meant to delete from, or two image lowerdirs stacked in the wrong order — would
// still "resolve" every path (so the single-lower suite stays green) yet serve the WRONG file
// (or fail to mask a deleted one) at restore. The [3a-stack] loop preserves segments in a
// specific order (upper → img.0, then each lowerdir in declared order → img.1, img.2, …) and
// writes them into .overlay-opts as lowerdir=$LOWER:img.0:img.1:… so that overlay precedence
// on the reconstructed stack matches the live merged view. WP2 pins that ordering behaviorally
// with a MULTI-lowerdir fixture and a MASKING-aware resolver, so an inversion turns this RED.

// maskAwareResolve walks an ordered overlay stack (top layer first, matching
// overlayOpts.stack) and returns the layer that WINS for rel under real overlay semantics:
//   - the FIRST layer (highest precedence) that carries a regular entry at rel WINS (returns
//     that layer, masked=false);
//   - if the first layer that carries rel carries it as a char-device 0:0 WHITEOUT, the path is
//     DELETED in the merged view (returns "", masked=true) — lower layers are masked;
//   - if an intermediate layer carries an OPAQUE dir (opaqueXattrName present), lookups for
//     entries UNDER that dir stop there (the dir fully masks lower dirs).
//
// It is the masking-aware sibling of overlayOpts.resolves (which models only "exists in any
// layer" and is blind to whiteouts/opaque markers — the exact WP2 blind spot).
func maskAwareResolve(stack []string, rel string, opaqueXattrName string) (layer string, masked bool) {
	for _, l := range stack {
		p := filepath.Join(l, rel)
		var st syscall.Stat_t
		if err := syscall.Lstat(p, &st); err != nil {
			// Not in this layer — check whether an ancestor dir in THIS layer is opaque, which
			// would mask the entry from all lower layers even though the entry itself is absent
			// here. Walk rel's ancestors in this layer looking for an opaque marker.
			if dir := opaqueAncestor(l, rel, opaqueXattrName); dir != "" {
				return "", true
			}
			continue
		}
		// Present in this layer. A char-device 0:0 is a whiteout: the path is deleted.
		if st.Mode&syscall.S_IFMT == syscall.S_IFCHR && st.Rdev == 0 {
			return "", true
		}
		return l, false
	}
	return "", false
}

// opaqueAncestor returns the highest ancestor dir of rel within layer that carries the opaque
// xattr, or "" if none. A hit means rel is masked from lower layers by that opaque dir.
func opaqueAncestor(layer, rel, opaqueXattrName string) string {
	if opaqueXattrName == "" {
		return ""
	}
	parts := strings.Split(rel, "/")
	acc := ""
	for i := 0; i < len(parts)-1; i++ { // ancestors only (exclude the leaf itself)
		acc = filepath.Join(acc, parts[i])
		buf := make([]byte, 8)
		if _, err := syscall.Getxattr(filepath.Join(layer, acc), opaqueXattrName, buf); err == nil {
			return acc
		}
	}
	return ""
}

// TestFreeze_DirMedium3aMultiLowerOrdering runs the REAL [3a] block against a fixture with a
// container upper + TWO ordered image lowerdirs, and asserts the reconstructed stack preserves
// overlay precedence: the upper wins over both lowers, the earlier lowerdir wins over the later,
// and an upper WHITEOUT masks a file that exists in a lower. An ordering inversion (upper below
// its lower, or the two lowers swapped) turns the masking / precedence assertions RED.
func TestFreeze_DirMedium3aMultiLowerOrdering(t *testing.T) {
	if !testFSSupportsMknodChardev(t) {
		t.Skip("test fs/process cannot create a char-device 0:0 whiteout (needed to model overlay masking)")
	}
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	block := extractFreeze3aBlock(t, m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90}))

	// Reuse the shared sparse fixture for the bundle/spec/rootfs scaffolding, then OVERRIDE $ovl
	// with a MULTI-lowerdir opts string naming a container upper + two ordered image lowerdirs,
	// each a real on-disk layer dir. This is the multi-layer topology the single-lower suite
	// never exercised.
	bundle, taskDir, cid, dstn, _, _ := sparseAgentFixture(t)
	root := t.TempDir()
	upper := filepath.Join(root, "upper")   // container upperdir  -> img.0 (highest precedence)
	low0 := filepath.Join(root, "imglow.0") // earlier image layer -> img.1
	low1 := filepath.Join(root, "imglow.1") // later image layer   -> img.2 (lowest precedence)
	for _, d := range []string{upper, low0, low1} {
		if err := os.MkdirAll(filepath.Join(d, "usr/bin"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// PRECEDENCE probe file: /usr/bin/tool exists in ALL THREE layers with a DIFFERENT marker so
	// the winning layer is identifiable. Overlay serves the highest-precedence copy: upper.
	mustWrite(t, filepath.Join(upper, "usr/bin/tool"), "UPPER\n")
	mustWrite(t, filepath.Join(low0, "usr/bin/tool"), "LOW0\n")
	mustWrite(t, filepath.Join(low1, "usr/bin/tool"), "LOW1\n")

	// LOWERDIR-ORDER probe: /usr/bin/libonly exists only in the two image lowers (NOT upper),
	// with different markers. Overlay serves the EARLIER lowerdir: low0.
	mustWrite(t, filepath.Join(low0, "usr/bin/libonly"), "LOW0\n")
	mustWrite(t, filepath.Join(low1, "usr/bin/libonly"), "LOW1\n")

	// WHITEOUT-ORDER probe (the load-bearing one): /usr/bin/deleted exists in BOTH image lowers,
	// and the container upper carries a char-device 0:0 WHITEOUT for it. Correct overlay: the
	// upper's whiteout (highest precedence) MASKS the file → deleted in the merged view. If the
	// upper landed BELOW the lowers (the inversion this test guards), the lower's real file would
	// win and the deleted file would RESURRECT.
	mustWrite(t, filepath.Join(low0, "usr/bin/deleted"), "LOW0\n")
	mustWrite(t, filepath.Join(low1, "usr/bin/deleted"), "LOW1\n")
	if err := syscall.Mknod(filepath.Join(upper, "usr/bin/deleted"), syscall.S_IFCHR|0o644, 0); err != nil {
		t.Fatalf("mknod upper whiteout: %v", err)
	}

	ovl := "upperdir=" + upper + ",lowerdir=" + low0 + ":" + low1
	out := runFreeze3a(t, block, bundle, taskDir, cid, dstn, ovl, "root:dir=/var/lib/gardenio/overlay")

	opts := parseOverlayOpts(t, bundle, dstn)
	lower := filepath.Join(bundle, dstn, "lower")

	// (1) The reconstructed stack must be: $LOWER (skeleton top), then img.0=upper, img.1=low0,
	// img.2=low1 — three stacked segments beneath $LOWER, in that order. FT_STACKED_LOWERS=3.
	if !strings.Contains(out, "FT_STACKED_LOWERS_agent=3") {
		t.Errorf("multi-lowerdir fixture must stack 3 segments (upper + 2 image lowers); want FT_STACKED_LOWERS_agent=3\n%s", out)
	}
	if len(opts.stack) != 4 || opts.stack[0] != lower {
		t.Fatalf(".overlay-opts must be lowerdir=$LOWER:img.0:img.1:img.2 (4 segments, $LOWER first); got %v", opts.stack)
	}
	// img.0/img.1/img.2 must be GC-safe copies (not the live layer paths) in the right order.
	for i, seg := range opts.stack[1:] {
		if base, want := filepath.Base(seg), fmt.Sprintf("img.%d", i); base != want {
			t.Errorf("stack segment %d must be the GC-safe %s copy; got %q", i+1, want, seg)
		}
	}

	// The ordered stack we assert precedence against IS opts.stack (top-first: $LOWER, then the
	// img.* copies in declared order). $LOWER is a skeleton (no /usr/bin/tool content), so it
	// merges without masking these probes.
	stack := opts.stack

	// (2) PRECEDENCE: /usr/bin/tool must resolve to the UPPER copy (img.0), not a lower one.
	if layer, masked := maskAwareResolve(stack, "usr/bin/tool", ""); masked || layer == "" {
		t.Errorf("usr/bin/tool must resolve (masked=%v layer=%q) — the upper copy should win", masked, layer)
	} else if got, _ := os.ReadFile(filepath.Join(layer, "usr/bin/tool")); strings.TrimSpace(string(got)) != "UPPER" {
		t.Errorf("usr/bin/tool resolved to %q content=%q, want the UPPER copy (overlay precedence: upper > lowers)", layer, strings.TrimSpace(string(got)))
	}

	// (3) LOWERDIR ORDER: /usr/bin/libonly (only in the two lowers) must resolve to the EARLIER
	// lowerdir (low0 → img.1), not the later (low1 → img.2).
	if layer, masked := maskAwareResolve(stack, "usr/bin/libonly", ""); masked || layer == "" {
		t.Errorf("usr/bin/libonly must resolve (masked=%v layer=%q)", masked, layer)
	} else if got, _ := os.ReadFile(filepath.Join(layer, "usr/bin/libonly")); strings.TrimSpace(string(got)) != "LOW0" {
		t.Errorf("usr/bin/libonly resolved to %q content=%q, want LOW0 (earlier lowerdir wins over later)", layer, strings.TrimSpace(string(got)))
	}

	// (4) WHITEOUT MASKING (the load-bearing assertion): /usr/bin/deleted must be MASKED — the
	// upper's char-device 0:0 whiteout, being highest precedence, deletes it from the merged
	// view. An ordering inversion that put the upper below the lowers would serve the lower's
	// real file here (resurrecting a deleted file) and this would FAIL.
	if layer, masked := maskAwareResolve(stack, "usr/bin/deleted", ""); !masked {
		t.Errorf("usr/bin/deleted must be MASKED by the upper whiteout (resolved layer=%q) — the upper's char-device 0:0 must win over the lowers that carry the real file; a resolving path here means an ordering inversion resurrected a runtime-deleted file (ka-0000001867 WP2)", layer)
	}

	// NON-VACUITY: prove the masking assertion actually depends on ordering. Re-run the resolver
	// against a DELIBERATELY INVERTED stack (lowers above the upper whiteout) and confirm it does
	// NOT mask — i.e. the inversion the real test guards against is one this resolver can detect.
	// This guards the guard: if maskAwareResolve masked regardless of order, assertion (4) would
	// be vacuous.
	inverted := []string{stack[0], stack[2], stack[3], stack[1]} // $LOWER, img.1(low0), img.2(low1), img.0(upper)
	if _, masked := maskAwareResolve(inverted, "usr/bin/deleted", ""); masked {
		t.Errorf("NON-VACUITY BROKEN: maskAwareResolve masked usr/bin/deleted even with the upper whiteout stacked BELOW the lowers — it is order-insensitive, so the WP2 masking assertion proves nothing")
	}
}
