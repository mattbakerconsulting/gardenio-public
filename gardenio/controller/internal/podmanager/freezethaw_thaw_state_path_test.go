package podmanager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gardenio/controller/internal/thawepoch"
)

// These tests are the ka-0000003421 regression guard: the gVisor freeze->thaw
// rehydrate handshake fail-closed because the in-pod gb+coop watchers polled a
// thaw-state path the restored mount never occupied.
//
// The mechanism: COOP_THAW_STATE_DIR is set to MountThawState on the live Pod A
// and FROZEN into the checkpoint as part of the process env. The restore barrier
// re-lands every checkpointed mount whose DESTINATION began with /var/run/ at the
// corresponding /run/ path (freezethaw_node.go "normalize ... mount DESTINATIONS
// /var/run/* -> /run/*"). Pre-freeze that divergence was invisible because Pod A's
// rootfs carried /var/run -> /run as a SYMLINK, so a /var/run-rooted env resolved
// to the same inode as the /run mount. But the RESTORED sandbox materializes
// /var/run as a REAL directory (no symlink), so a /var/run-rooted env pointed the
// watchers at an empty /var/run/gardenio-thaw while the controller's epoch marker
// sat at the real mount /run/gardenio-thaw -> neither consumer acked -> the C3
// gate hit its deadline and fail-closed. Canonicalizing MountThawState on /run
// removes the symlink dependency.

// normalizeRestoreMountDest mirrors the mount-DESTINATION normalization the
// restore barrier applies on the node (freezethaw_node.go, the per-sub-container
// config.json rewrite: `if d.startswith("/var/run/"): m["destination"]="/run/"+
// d[len("/var/run/"):]`). It is kept test-local — the production normalization is
// embedded Python inside the thaw script — so the invariant below is checked
// against the SAME rule the live restore uses, not a hand-waved approximation.
func normalizeRestoreMountDest(dest string) string {
	const varRun = "/var/run/"
	if strings.HasPrefix(dest, varRun) {
		return "/run/" + dest[len(varRun):]
	}
	return dest
}

// TestThawState_MountPathSurvivesRestoreNormalization proves the core invariant
// the fix establishes: the path the watchers poll (the frozen
// COOP_THAW_STATE_DIR == MountThawState) MUST equal the path the restored mount
// re-lands on (normalizeRestoreMountDest(MountThawState)). If MountThawState is
// rooted on /var/run the normalization shifts the landing to /run and the two
// diverge — exactly the ka-0000003421 fail-close. Reverting the fix
// (MountThawState back to /var/run/gardenio-thaw) makes this test fail (the
// built-in mutation check).
func TestThawState_MountPathSurvivesRestoreNormalization(t *testing.T) {
	landed := normalizeRestoreMountDest(MountThawState)
	if landed != MountThawState {
		t.Fatalf("MountThawState %q is NOT invariant under the restore /var/run->/run "+
			"destination normalization (the restored mount re-lands at %q). The frozen "+
			"COOP_THAW_STATE_DIR=%q would then point the in-pod gb+coop watchers at a path "+
			"the restored mount never occupies, fail-closing the C3 rehydrate gate "+
			"(ka-0000003421). Pin MountThawState on the canonical /run path.",
			MountThawState, landed, MountThawState)
	}
	// Belt and suspenders: the invariant is only meaningful if the path is the
	// canonical /run mount the 9p restore surfaces — not some other prefix that is
	// trivially invariant. A /var/run-rooted value can never have this prefix.
	if !strings.HasPrefix(MountThawState, "/run/") {
		t.Errorf("MountThawState = %q, want a canonical /run/-rooted path so it survives "+
			"the restore mount-destination normalization intact", MountThawState)
	}
}

// TestThawState_RestoredSandboxInodeRendezvous reconstructs the restored-sandbox
// filesystem divergence from the forensics and proves the watcher poll path and
// the controller marker-write path resolve to the SAME inode even when /var/run is
// a REAL directory rather than a symlink to /run.
//
// Simulated restored-sandbox layout (the on-node forensic finding):
//
//	<root>/run/gardenio-thaw/        - where the per-agent thaw-state 9p hostPath
//	                                  mount RE-LANDS after the restore
//	                                  /var/run->/run destination normalization; the
//	                                  controller-written epoch marker is visible
//	                                  here through the mount (its node-side source
//	                                  is ThawStateHostBase, unaffected by this).
//	<root>/var/run/                 - a REAL directory created at restore time, NOT
//	                                  a symlink to /run (the pre-freeze symlink is
//	                                  gone in the restored rootfs).
//
// The in-pod watcher polls $COOP_THAW_STATE_DIR (== MountThawState) rooted in the
// restored sandbox. With the fix (canonical /run) the poll lands on the marker;
// with the old /var/run value it ENOENTs against the empty real /var/run dir.
func TestThawState_RestoredSandboxInodeRendezvous(t *testing.T) {
	root := t.TempDir()

	// The mount re-lands at the restore-normalized destination; the marker the
	// controller wrote (node-side via ThawStateHostBase) shows through there.
	mountLanding := filepath.Join(root, normalizeRestoreMountDest(MountThawState))
	if err := os.MkdirAll(mountLanding, 0o775); err != nil {
		t.Fatalf("mkdir restored mount landing %q: %v", mountLanding, err)
	}
	markerPath := filepath.Join(mountLanding, thawepoch.MarkerName)
	if err := os.WriteFile(markerPath, []byte(`{"epoch":1}`), 0o644); err != nil {
		t.Fatalf("write controller marker: %v", err)
	}

	// The restore materializes /var/run as a REAL directory, NOT a symlink to /run.
	realVarRun := filepath.Join(root, "var", "run")
	if err := os.MkdirAll(realVarRun, 0o755); err != nil {
		t.Fatalf("mkdir real /var/run: %v", err)
	}
	// Guard the premise: if /var/run were (still) a symlink to /run, the test would
	// prove nothing — it would be the benign pre-freeze topology, not the restored
	// divergence we are guarding against.
	fi, err := os.Lstat(realVarRun)
	if err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("test premise broken: restored /var/run must be a REAL dir (got mode=%v err=%v)", fi.Mode(), err)
	}

	// The watcher polls COOP_THAW_STATE_DIR == MountThawState, rooted in the
	// restored sandbox — the exact path gb/coop os.Stat/read for the epoch marker.
	watcherPollPath := filepath.Join(root, MountThawState, thawepoch.MarkerName)
	gotInfo, err := os.Stat(watcherPollPath)
	if err != nil {
		t.Fatalf("watcher poll path %q did not resolve to the marker the controller wrote at %q: %v\n"+
			"This is the ka-0000003421 divergence: COOP_THAW_STATE_DIR=%q points at a path the restored "+
			"mount never occupies because /var/run is a real dir, not a symlink to /run.",
			watcherPollPath, markerPath, MountThawState, err)
	}

	// Prove it is the SAME inode, not a coincidentally same-named file.
	wantInfo, err := os.Stat(markerPath)
	if err != nil {
		t.Fatalf("stat controller marker %q: %v", markerPath, err)
	}
	if !os.SameFile(wantInfo, gotInfo) {
		t.Errorf("watcher poll path and controller marker are different inodes:\n  marker=%q\n  poll  =%q\n"+
			"the rehydrate handshake can only release when both address the same bytes", markerPath, watcherPollPath)
	}
}

// TestThawState_CanonicalPathIsAgentWritableAtArmTime proves requirement (2): the
// chosen path is made agent-UID-writable at watcher-arm time. The root
// thaw-state-perms init container (buildThawStatePermsInit) mounts the volume at
// MountThawState and chowns it to the agent UID/GID before the watchers arm, and
// it runs as a runsc-only root init container. With the fix this now establishes
// writability on the canonical /run/gardenio-thaw — the same path the env points
// at post-restore — so the watchers can write their .armed sentinels and acks
// beside the controller's marker. (Ordering vs init-clone is covered by
// TestBuildPod_ThawStatePermsInit_Runsc.)
func TestThawState_CanonicalPathIsAgentWritableAtArmTime(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.RuntimeClassName = RuntimeClassRunsc

	ic := m.buildThawStatePermsInit(spec)
	if ic == nil {
		t.Fatal("runsc pod must carry the thaw-state-perms init container so the watcher path is agent-writable at arm time")
	}

	// Mounts the thaw-state volume at the canonical path the watchers poll.
	var mounted bool
	for _, vm := range ic.VolumeMounts {
		if vm.Name == VolumeThawState && vm.MountPath == MountThawState {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("thaw-state-perms init must mount %q at MountThawState=%q, got %+v",
			VolumeThawState, MountThawState, ic.VolumeMounts)
	}

	// chowns the canonical path to the agent UID/GID (writability for the watchers).
	cmd := ""
	if len(ic.Command) > 0 {
		cmd = ic.Command[len(ic.Command)-1]
	}
	if !strings.Contains(cmd, MountThawState) || !strings.Contains(cmd, "1000:1000") {
		t.Errorf("thaw-state-perms command = %q, want chown 1000:1000 on canonical %q", cmd, MountThawState)
	}
	if !strings.HasPrefix(MountThawState, "/run/") {
		t.Errorf("writability is established on %q, but the canonical post-restore path is /run-rooted; "+
			"a /var/run mount would be chowned at a path the restored env does not point at", MountThawState)
	}
}
