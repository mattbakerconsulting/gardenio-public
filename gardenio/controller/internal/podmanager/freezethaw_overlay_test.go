package podmanager

import (
	"os/exec"
	"strings"
	"testing"
)

// overlay-dir-medium controller half (ka-0000007239). These tests pin the
// snapshot-self-describes-its-medium contract: freeze probes the live runsc
// rootfs-overlay medium; thaw replays the SAME --overlay2 to every `runsc
// restore`; and a default/self medium leaves the restore lines byte-identical to
// the pre-fix script (zero blast radius on self-medium nodes — the upper rides
// inside checkpoint.img, proven on-node by substrate revalidation ka-0000007329).

// bashParses runs `bash -n` (syntax-only, no execution) over a rendered node
// script and fails the test if bash rejects it. The freeze/thaw scripts are
// shell text built from a Go template, so a stray quote or unbalanced construct
// would only surface at node-exec time without this guard.
func bashParses(t *testing.T, label, script string) {
	t.Helper()
	cmd := exec.Command("bash", "-n")
	cmd.Stdin = strings.NewReader(script)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: rendered script failed `bash -n`: %v\n%s", label, err, out)
	}
}

func TestOverlayRestoreFlag(t *testing.T) {
	cases := []struct {
		name   string
		medium string
		want   string
	}{
		{"empty defaults to no flag", "", ""},
		{"explicit self is the default — no flag", "root:self", ""},
		{"bare self alias — no flag", "self", ""},
		{"dir medium renders the full flag", "root:dir=/var/lib/gardenio/overlay", "--overlay2=root:dir=/var/lib/gardenio/overlay"},
		{"whitespace is trimmed", "  root:dir=/var/lib/gardenio/overlay  ", "--overlay2=root:dir=/var/lib/gardenio/overlay"},
		{"sized dir medium", "root:dir=/var/lib/gardenio/overlay,size=2g", "--overlay2=root:dir=/var/lib/gardenio/overlay,size=2g"},
		// shell-unsafe values fall back to "" (default behaviour) rather than inject.
		{"space injection rejected", "root:dir=/x ; rm -rf /", ""},
		{"backtick injection rejected", "root:dir=/x`whoami`", ""},
		{"dollar injection rejected", "root:dir=$HOME", ""},
		{"quote injection rejected", `root:dir="/x"`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := overlayRestoreFlag(tc.medium); got != tc.want {
				t.Errorf("overlayRestoreFlag(%q) = %q, want %q", tc.medium, got, tc.want)
			}
		})
	}
}

// TestThawScript_DefaultMediumByteIdenticalToPreFix verifies that a snapshot with
// NO overlay medium (the self/default case, i.e. every existing snapshot and every
// non-dir node) renders restore lines with NO --overlay2 flag — the OVERLAY_FLAG
// shell var expands empty, so `runsc restore $OVERLAY_FLAG ...` is exactly the
// pre-fix `runsc restore ...`. This is the zero-blast-radius guarantee.
func TestThawScript_DefaultMediumByteIdenticalToPreFix(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	base := thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
	}
	tw := m.buildThawScript(base)
	if !strings.Contains(tw, `OVERLAY_FLAG=''`) {
		t.Error("default-medium thaw script must render an EMPTY OVERLAY_FLAG (no --overlay2)")
	}
	// The rendered OVERLAY_FLAG var must carry no medium (it expands empty), so a
	// self-medium restore is byte-identical to the pre-fix script. (The template's
	// explanatory comment legitimately mentions --overlay2, so check the rendered
	// assignment, not a bare substring.)
	if strings.Contains(tw, `OVERLAY_FLAG='--overlay2=`) {
		t.Error("default-medium thaw script must NOT render a populated --overlay2 flag")
	}
	// The restore invocations still reference the var (so a dir-medium snapshot can
	// inject it) — pin that the wiring is present. $OVERLAY_FLAG must sit in the
	// runsc GLOBAL position (before `restore`), since --overlay2 is a runsc global
	// flag — placing it after the subcommand makes runsc reject it (ka-0000000621).
	for _, want := range []string{
		`RUNSC $OVERLAY_FLAG restore --image-path="$IMG" --bundle "$BUNDLE/sandbox"`,
		`RUNSC $OVERLAY_FLAG restore --image-path="$IMG" --bundle "$BUNDLE/$cn"`,
	} {
		if !strings.Contains(tw, want) {
			t.Errorf("thaw restore line missing OVERLAY_FLAG wiring: %q", want)
		}
	}
	bashParses(t, "thaw(default-medium)", tw)
}

// TestThawScript_DirMediumPassesOverlayFlag verifies a dir-medium snapshot renders
// the --overlay2=root:dir=... flag into OVERLAY_FLAG so EVERY runsc restore (the
// background sandbox root AND each sub) replays the checkpoint's medium. A root:dir
// checkpoint can only restore under root:dir (substrate revalidation ka-0000007329).
func TestThawScript_DirMediumPassesOverlayFlag(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 3, RunscTimeout: 90,
		RestoreOverlayFlag: overlayRestoreFlag("root:dir=/var/lib/gardenio/overlay"),
		SubContainers:      []thawSubContainer{{Name: "agent", ACID: "a"}, {Name: "dind", ACID: "d"}},
	})
	if !strings.Contains(tw, `OVERLAY_FLAG='--overlay2=root:dir=/var/lib/gardenio/overlay'`) {
		t.Error("dir-medium thaw script must render OVERLAY_FLAG with the --overlay2=root:dir flag")
	}
	// Both restore sites must reference $OVERLAY_FLAG in the runsc GLOBAL position
	// (before `restore`), sandbox root + per-sub loop. --overlay2 is a runsc global
	// flag; the subcommand position is rejected by runsc (ka-0000000621).
	if strings.Count(tw, "RUNSC $OVERLAY_FLAG restore") < 2 {
		t.Errorf("both the sandbox-root and per-sub restore must replay OVERLAY_FLAG in global position; got %d sites", strings.Count(tw, "RUNSC $OVERLAY_FLAG restore"))
	}
	// Guard against regression to the subcommand position: --overlay2 (via
	// $OVERLAY_FLAG) must never follow `restore`.
	if strings.Contains(tw, "RUNSC restore $OVERLAY_FLAG") {
		t.Error("OVERLAY_FLAG must precede `restore` (runsc global flag), not follow it")
	}
	bashParses(t, "thaw(dir-medium)", tw)
}

// TestFreezeScript_ProbesOverlayMedium verifies the freeze script probes the live
// runsc rootfs-overlay medium off the sandbox boot args and emits the
// FT_OVERLAY_MEDIUM sentinel the controller persists into the snapshot (plan Step
// 4-A: the snapshot self-describes its medium).
func TestFreezeScript_ProbesOverlayMedium(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	fs := m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90})
	for _, want := range []string{
		`OVERLAY_MEDIUM=$(nsenter -t 1 -m -p --`,   // host-ns probe of the live sandbox args
		`sed -n "s/^--overlay2=//p"`,               // extract the medium from --overlay2=
		`echo "FT_OVERLAY_MEDIUM=$OVERLAY_MEDIUM"`, // emit the self-describe sentinel
	} {
		if !strings.Contains(fs, want) {
			t.Errorf("freeze script missing overlay-medium probe step: %q", want)
		}
	}
	bashParses(t, "freeze", fs)
}

// TestFreezeScript_DerivesOverlayFlagGate verifies the freeze script derives a single
// OVERLAY_FLAG dir-medium gate from the probed medium (ka-0000000357 plural_fix), so
// the [3a] skeleton + lowerdir-stacking blocks run ONLY under the dir medium. The
// self/root:self cases must collapse to an EMPTY gate (zero blast radius).
func TestFreezeScript_DerivesOverlayFlagGate(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	fs := m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90})
	for _, want := range []string{
		`case "$OVERLAY_MEDIUM" in`,             // gate derivation off the probed medium
		`''|root:self|self) OVERLAY_FLAG='' ;;`, // self/default collapse to empty gate
		`*)                 OVERLAY_FLAG="$OVERLAY_MEDIUM" ;;`,
	} {
		if !strings.Contains(fs, want) {
			t.Errorf("freeze script missing OVERLAY_FLAG gate derivation: %q", want)
		}
	}
	bashParses(t, "freeze", fs)
}

// TestFreezeScript_DirMediumSkeletonAndStacking verifies the freeze cp-success branch
// carries the plural_fix [3a] edits (ka-0000000357): the full-skeleton mkdir (dentry
// floor for the 9p-walk) AND the image-lowerdir-stacking (real base content so the
// rootfs is not hollow), BOTH gated on the dir medium, plus their forensic sentinels.
func TestFreezeScript_DirMediumSkeletonAndStacking(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	fs := m.buildFreezeScript(freezeScriptParams{PodName: "p", RunscTimeout: 90})
	for _, want := range []string{
		`if [ -n "$OVERLAY_FLAG" ]; then`,                                           // dir-medium gate on the [3a] block
		`IMGDST="$BUNDLE/$dstn/img.$i"`,                                             // GC-safe per-segment image preserve
		`cp -a --reflink=auto "$1/." "$2/"`,                                         // same reflink shape as the relocation
		`printf 'lowerdir=%s%s\n' "$LOWER" "$STACK"`,                                // stack images BENEATH $LOWER
		`echo "FT_STACKED_LOWERS_$dstn=$i"`,                                         // stacking forensic
		`for s in ("usr","usr/bin","usr/lib","usr/lib64","usr/sbin"): paths.add(s)`, // symlink targets
		`HMNT mkdir -p "$LOWER/$rel"`,                                               // skeleton dentry floor into $LOWER
		`echo "FT_SKELETON_DIRS_$dstn=$n"`,                                          // skeleton forensic
		`echo "FT_RELOC_REFLINK_HITS_$dstn=$RHITS"`,                                 // reflink-engagement counter (ka-0000007174)
		`echo "FT_RELOC_REFLINK_FALLBACKS_$dstn=$RFALL"`,                            // reflink-engagement counter (ka-0000007174)
	} {
		if !strings.Contains(fs, want) {
			t.Errorf("freeze script missing plural_fix [3a] step: %q", want)
		}
	}
	// The skeleton/stacking must live INSIDE the cp-success branch (after the
	// lowerdir=$LOWER write), not in the cp-failure fallback — assert the gate appears
	// after the FT_RELOC_FSTYPE signal and before the `else` fallback.
	stackIdx := strings.Index(fs, `echo "FT_STACKED_LOWERS_$dstn=$i"`)
	fstypeIdx := strings.Index(fs, `echo "FT_RELOC_FSTYPE_$dstn=`)
	fallbackIdx := strings.Index(fs, `fell back to live opts`)
	if !(fstypeIdx >= 0 && stackIdx > fstypeIdx && (fallbackIdx < 0 || stackIdx < fallbackIdx)) {
		t.Errorf("plural_fix [3a] must sit in the cp-success branch (after FT_RELOC_FSTYPE, before the cp-failure fallback); fstype=%d stack=%d fallback=%d", fstypeIdx, stackIdx, fallbackIdx)
	}
	bashParses(t, "freeze", fs)
}
