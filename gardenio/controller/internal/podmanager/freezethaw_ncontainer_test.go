package podmanager

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

// livePodAWithSidecars is a Docker+Postgres agent Pod A: agent container plus
// dind/postgres always-on native sidecars (init containers with restartPolicy:
// Always) and a run-to-completion init-clone.
func livePodAWithSidecars(name, ns, node string) *corev1.Pod {
	always := corev1.ContainerRestartPolicyAlways
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID("a-uid-123")},
		Spec: corev1.PodSpec{
			NodeName: node,
			Containers: []corev1.Container{{
				Name: ContainerName, Image: "agent:latest", Command: []string{"/entrypoint.sh"},
				VolumeMounts: []corev1.VolumeMount{{Name: "work", MountPath: "/work"}},
			}},
			InitContainers: []corev1.Container{
				{Name: InitCloneName, Image: "alpine/git"},
				{Name: DindContainerName, Image: DindImage, RestartPolicy: &always},
				{Name: PostgresContainerName, Image: PostgresImage, RestartPolicy: &always},
			},
			Volumes: []corev1.Volume{{Name: "work", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// TestFreeze_SidecarsPersistRoster verifies that freezing a Docker+Postgres agent
// (container_count=4) persists the full sub-container roster (agent + dind +
// postgres) on the snapshot, and that the count cross-check passes (ka-0000003561).
func TestFreeze_SidecarsPersistRoster(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset(livePodAWithSidecars("agentsim", ns, node))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: freezeOKStdoutSidecars}}
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, exec, store, nil)

	snap, _, err := m.Freeze(context.Background(), FreezeRequest{
		AgentName: "agentsim", BeadID: "gd-AGENT", PodName: "agentsim", Namespace: ns,
	})
	if err != nil {
		t.Fatalf("Freeze: %v", err)
	}
	if snap.ContainerCount != 4 {
		t.Errorf("container count = %d, want 4", snap.ContainerCount)
	}
	wantNames := []string{"agent", "dind", "postgres"}
	if len(snap.SubContainers) != len(wantNames) {
		t.Fatalf("roster = %+v, want %v", snap.SubContainers, wantNames)
	}
	for i, w := range wantNames {
		if snap.SubContainers[i].Name != w {
			t.Errorf("roster[%d].Name = %q, want %q", i, snap.SubContainers[i].Name, w)
		}
		if snap.SubContainers[i].CID == "" {
			t.Errorf("roster[%d] (%s) has empty cid", i, w)
		}
	}
	// All per-container spec files captured (sandbox + 3 sub-containers x 2 files).
	if len(snap.SpecFiles) != 8 {
		t.Errorf("spec files = %d, want 8 (4 containers x config+overlay)", len(snap.SpecFiles))
	}
}

// TestFreeze_CountRosterMismatchFailsClosed verifies the freeze count cross-check:
// a container_count that does not equal sandbox(1) + the captured sub-container
// roster is a shape mismatch and must refuse to persist (ka-0000003561).
func TestFreeze_CountRosterMismatchFailsClosed(t *testing.T) {
	ns := "gvisor-poc"
	client := fake.NewClientset(livePodA("agentsim", ns, "n1"))
	// count=4 but only the agent sub-container is rostered -> mismatch.
	stdout := "FT_SANDBOX_CID=sb\nFT_AGENT_CID=ag\nFT_SUBC=agent|ag\nFT_CHECKPOINT_RC=0\n" +
		"FT_IMAGE_PRESENT=yes\nFT_CONTAINER_COUNT=4\nFT_SANDBOX_STATE=running\nFT_BUNDLE_OK=yes\nFT_IMAGE_OK=yes\n" +
		"FT_SPEC=sandbox/config.json|eyJ4IjoxfQ==\nFT_SPEC=agent/config.json|eyJ4IjoxfQ==\n" +
		"FT_SPEC=sandbox/.overlay-opts|YQ==\nFT_SPEC=agent/.overlay-opts|YQ==\n"
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: stdout}}
	store := NewInMemorySnapshotStore()
	m := newFreezeThawManager(t, client, exec, store, nil)

	_, _, err := m.Freeze(context.Background(), FreezeRequest{AgentName: "agentsim", PodName: "agentsim", Namespace: ns})
	if err == nil || !strings.Contains(err.Error(), "roster/checkpoint shape mismatch") {
		t.Fatalf("want count/roster mismatch failure, got %v", err)
	}
	if _, gerr := store.Get(context.Background(), "agentsim"); gerr == nil {
		t.Fatal("snapshot persisted despite count/roster mismatch; must fail closed")
	}
}

// TestThaw_SidecarSnapshotProceeds verifies the C6 guard removal: a sidecar
// snapshot (container_count=4 WITH a roster) is no longer refused — Thaw drives
// the N-container barrier restore instead of falling back to cold start
// (ka-0000003561). The fake node executor returns the N-container OK stdout.
func TestThaw_SidecarSnapshotProceeds(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", BeadID: "gd-AGENT", Namespace: ns, PodName: "agentsim", Node: node,
		ImagePath: "/var/lib/gardenio/ckpt/agentsim", BundlePath: "/var/lib/gardenio/bundle/agentsim",
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 4,
		SubContainers: []SnapshotContainer{{Name: "agent", CID: "ag"}, {Name: "dind", CID: "d"}, {Name: "postgres", CID: "p"}},
		PodTemplate:   sanitizePodTemplate(livePodAWithSidecars("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	res, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err != nil {
		t.Fatalf("Thaw of a sidecar snapshot should proceed (N-container restore), got %v", err)
	}
	if res.RestoredPID == "" {
		t.Error("restore did not report a sentry pid")
	}
}

// TestThaw_MultiContainerNoRosterFallsBackToColdStart verifies the remaining
// fail-closed: a count>2 snapshot with NO roster (legacy/partial) cannot be driven
// by the N-restore loop, so Thaw refuses and the caller cold-starts (ka-0000003561).
func TestThaw_MultiContainerNoRosterFallsBackToColdStart(t *testing.T) {
	ns, node := "gvisor-poc", "node-1"
	client := fake.NewClientset()
	client.PrependReactor("create", "pods", readyPodReactor("b-uid-456"))
	exec := &fakeNodeExecutor{result: NodeExecResult{Stdout: thawOKStdout}}
	store := NewInMemorySnapshotStore()
	_ = store.Put(context.Background(), Snapshot{
		AgentName: "agentsim", Namespace: ns, PodName: "agentsim", Node: node,
		ImagePath: "/var/lib/gardenio/ckpt/agentsim", BundlePath: "/var/lib/gardenio/bundle/agentsim",
		SandboxCID: "sb", AgentCID: "ag", PodUID: "a", ContainerCount: 4, // no SubContainers
		PodTemplate: sanitizePodTemplate(livePodA("agentsim", ns, node).Spec),
	})
	m := newFreezeThawManager(t, client, exec, store, &recordingRehydrator{})

	_, err := m.Thaw(context.Background(), ThawRequest{AgentName: "agentsim"})
	if err == nil || !strings.Contains(err.Error(), "no sub-container roster") {
		t.Fatalf("want cold-start fallback for a rosterless multi-container snapshot, got %v", err)
	}
}

// TestThawScript_RestoresEverySubContainer is the ka-0000003561 thaw-script guard:
// for a sidecar roster the rendered script must drive one runsc restore per
// sub-container (agent + dind + postgres) against the matching bundle subdir, in
// addition to the background sandbox-root restore.
func TestThawScript_RestoresEverySubContainer(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 4, RunscTimeout: 90,
		SubContainers: []thawSubContainer{{Name: "agent", ACID: "a"}, {Name: "dind", ACID: "d"}, {Name: "postgres", ACID: "p"}},
		SpecFiles: []thawSpecFile{
			{Path: "sandbox/config.json", Base64: "eyJ4IjoxfQ=="},
			{Path: "agent/config.json", Base64: "eyJ4IjoxfQ=="},
		},
	})
	for _, want := range []string{
		`SUBNAMES="$(printf '%s\n' 'agent' 'dind' 'postgres' )"`, // roster materialized
		`for cn in $SUBNAMES; do`,                                // delete loop over roster
		`--bundle "$BUNDLE/$cn" --detach "$bc"`,                  // per-sub restore against name-keyed bundle
		`--bundle "$BUNDLE/sandbox" --detach "$BSB"`,             // background sandbox-root restore
	} {
		if !strings.Contains(tw, want) {
			t.Errorf("thaw script missing N-container restore step: %q", want)
		}
	}
	// The background sandbox restore must be launched before the per-sub restore
	// loop (the barrier ordering: root parks, subs satisfy the count).
	iRoot := strings.Index(tw, `--bundle "$BUNDLE/sandbox" --detach "$BSB"`)
	iSubLoop := strings.Index(tw, `--bundle "$BUNDLE/$cn" --detach "$bc"`)
	if iRoot < 0 || iSubLoop < 0 || iRoot > iSubLoop {
		t.Errorf("background sandbox restore must precede the per-sub restore loop (root=%d subloop=%d)", iRoot, iSubLoop)
	}
}

// TestThawScript_BareAgentDefaultsToAgentRoster verifies that with NO SubContainers
// (the legacy bare-agent shape) the rendered thaw script defaults SUBNAMES to the
// single "agent" sub-container, preserving the proven 2-container behavior.
func TestThawScript_BareAgentDefaultsToAgentRoster(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
	})
	if !strings.Contains(tw, `SUBNAMES="agent"`) {
		t.Error("bare-agent thaw script must default SUBNAMES to \"agent\"")
	}
}

// TestThawScript_AssertsCWDDentryAfterRemount verifies the thaw overlay
// reconstruction asserts the container's checkpointed process.cwd dentry is
// EXPOSED by the freshly-reconstructed rootfs overlay before any runsc restore
// (ka-0000008217). The prod 2026.160.12 rc=128 was a vfs.CompleteRestore 9p-walk
// ENOENT on home/agent/workspace — the agent's CWD, an image WORKDIR served by
// the rootfs gofer (not a kubelet EmptyDir, so the [3] gofer-preserve sweep can
// never cover it). The fix-C guard only asserts the lowerdir directory exists;
// this guard asserts the CWD subpath actually survives the collapse-and-remount,
// converting the opaque rc=128 into a by-name FATAL that bisects thaw-side
// overlay-reconstruction vs Pod-B rootfs-wiring.
func TestThawScript_AssertsCWDDentryAfterRemount(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	tw := m.buildThawScript(thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
	})
	for _, want := range []string{
		`.get("process",{}).get("cwd","")`,             // reads cwd from the captured spec
		`HMNT test -e "$BUNDLE/$dstn/rootfs/${cwd#/}"`, // asserts it resolves in the reconstructed rootfs (host ns)
		`MISSING the container CWD dentry`,             // by-name FATAL diagnosis
	} {
		if !strings.Contains(tw, want) {
			t.Errorf("thaw script missing post-mount CWD-dentry assertion: %q", want)
		}
	}
	// The CWD assertion must live INSIDE remount_rootfs, AFTER the overlay mount
	// (so it inspects the reconstructed view) and BEFORE the [3] config rewrite /
	// [5] runsc restore — i.e. it gates the same fail path as the fix-C lowerdir
	// guard.
	iMount := strings.Index(tw, `HMNT mount -t overlay overlay "$BUNDLE/$dstn/rootfs"`)
	iCWD := strings.Index(tw, `MISSING the container CWD dentry`)
	iRestore := strings.Index(tw, `--bundle "$BUNDLE/$cn" --detach "$bc"`)
	if iMount < 0 || iCWD < 0 || iRestore < 0 || !(iMount < iCWD && iCWD < iRestore) {
		t.Errorf("CWD-dentry assertion must sit after the overlay mount and before runsc restore (mount=%d cwd=%d restore=%d)", iMount, iCWD, iRestore)
	}
}

// TestThawScript_DirMediumMaterializesCWDOnLower is the ka-0000004935 regression
// guard (supersedes the earlier "skip the pre-restore check" guard): the prod
// gardenio-gvisor thaw rc=128 (crew pk8 canary ka-0000005316, re-proven on
// 2026.179.13) is a GOFER LOWER re-walk ENOENT inside vfs.CompleteRestore —
// 'failed to walk "home/agent/workspace" of type 040000 in mount "agent:/"'.
//
// Adversarial review of the deployed runsc (release-20260525.0) source corrected
// the prior premise: 'runsc restore --overlay2=root:dir' does NOT only rebuild the
// upper and ignore the lower. gofer.(*filesystem).CompleteRestore re-walks EVERY
// non-synthetic dentry cached at checkpoint against the LIVE gofer LOWER, top-down.
// The agent's process.cwd (home/agent/workspace, an image WORKDIR served by the
// rootfs gofer) is one such dentry, and the freeze relocation produced a lower
// MISSING /home — so the re-walk ENOENTs (sub rc=128). Merely SKIPPING the
// pre-restore fail-close (the ka-0000000357 fix) was necessary-not-sufficient: it
// removed a false fail-close but never made the lower walkable.
//
// THE FIX (medium-gated divergence):
//   - dir medium (OVERLAY_FLAG non-empty): the script must MATERIALIZE the empty
//     CWD directory chain on the reconstructed lower (mkdir -p + chown) when it is
//     missing, so the restore gofer re-walk resolves; the content overlays from the
//     rebuilt upper (checkpoint pages). gVisor adopts the fresh inode (no QID
//     revalidation) and applies no size/mtime/type checks to a directory, so an
//     empty same-type dir suffices. It must NOT fail-close pre-restore.
//   - self/default medium (OVERLAY_FLAG empty): the original pre-restore
//     fail-closing assertion is RETAINED verbatim (the preserved lower carries the
//     CWD there, ka-0000006205; zero blast radius on self nodes).
func TestThawScript_DirMediumMaterializesCWDOnLower(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	base := thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		SubContainers: []thawSubContainer{{Name: "agent", ACID: "a"}},
	}

	// dir-medium: the pre-restore branch must MATERIALIZE the cwd dir, NOT fail-close.
	dirParams := base
	dirParams.RestoreOverlayFlag = overlayRestoreFlag("root:dir=/var/lib/gardenio/overlay")
	dir := m.buildThawScript(dirParams)
	if !strings.Contains(dir, `if [ -n "$OVERLAY_FLAG" ]; then`) {
		t.Error("dir-medium thaw script must gate the CWD handling on the overlay medium ($OVERLAY_FLAG)")
	}
	// The dir-medium branch must mkdir the cwd chain on the reconstructed lower and
	// chown it to the checkpointed owner, by name, when missing.
	for _, want := range []string{
		`HMNT mkdir -p "$BUNDLE/$dstn/rootfs/${cwd#/}"`,        // re-create the empty cwd chain on the lower
		`HMNT chown "$cwduid" "$BUNDLE/$dstn/rootfs/${cwd#/}"`, // owned by the checkpointed cwd owner
		`MATERIALIZED $dstn CWD dir`,                           // by-name log
	} {
		if !strings.Contains(dir, want) {
			t.Errorf("dir-medium thaw must materialize the CWD dir on the reconstructed lower: %q", want)
		}
	}
	// RC-SAFETY (the catastrophic-if-wrong case): the dir-medium branch must NOT
	// fail-close pre-restore. remount_rootfs's rc IS the if/elif/fi's rc, and the
	// caller fail-closes on it. The dir branch must end with a bare ':' rc-0 pin and
	// must NOT contain a `return 1` (only the self-medium elif and the lowerdir guard
	// fail-close). Assert the dir branch carries no pre-restore `return 1`: the only
	// `return 1` sites are the lowerdir-missing guard and the self-medium elif, both
	// outside the dir-medium `if`.
	iDirIf := strings.Index(dir, `if [ -n "$OVERLAY_FLAG" ]; then`)
	iElif := strings.Index(dir, `elif [ -n "$cwd" ] && [ "$cwd" != "/" ]; then`)
	if iDirIf < 0 || iElif < 0 || !(iDirIf < iElif) {
		t.Fatalf("dir-medium `if` must precede the self-medium `elif` (if=%d elif=%d)", iDirIf, iElif)
	}
	dirBranch := dir[iDirIf:iElif]
	if strings.Contains(dirBranch, "return 1") {
		t.Error("dir-medium pre-restore branch must NOT fail-close (no `return 1`); it materializes the cwd dir best-effort")
	}
	// The fail-closing dentry test (`if ! HMNT test -e .../${cwd#/}` -> return 1) must
	// be reachable ONLY under the self-medium elif, AFTER the dir-medium if.
	iFailTest := strings.Index(dir, `if ! HMNT test -e "$BUNDLE/$dstn/rootfs/${cwd#/}"; then`)
	if iFailTest < 0 || !(iElif < iFailTest) {
		t.Errorf("the fail-closing CWD `if ! test -e ... return 1` must live only in the self-medium elif branch (elif=%d failtest=%d)", iElif, iFailTest)
	}
	if strings.Count(dir, "RUNSC $OVERLAY_FLAG restore") < 2 {
		t.Errorf("dir-medium thaw must still replay --overlay2 to every restore so the upper is rebuilt from checkpoint.img; got %d sites", strings.Count(dir, "RUNSC $OVERLAY_FLAG restore"))
	}
	bashParses(t, "thaw(dir-medium-cwd-materialize)", dir)

	// The cwd-materialization mkdir lives INSIDE the dir-medium `if [ -n
	// "$OVERLAY_FLAG" ]` branch (runtime-gated), so it is reachable ONLY when the
	// medium is dir — proven by its position in dirBranch above. On a self-medium node
	// OVERLAY_FLAG renders EMPTY, so that branch is never taken at runtime and the
	// self-medium elif (fail-close) runs instead. (The bash body text is identical
	// across renders; the divergence is the OVERLAY_FLAG value + runtime gating.)
	selfParams := base // RestoreOverlayFlag == "" => self/default medium
	self := m.buildThawScript(selfParams)
	if !strings.Contains(self, `OVERLAY_FLAG=''`) {
		t.Error("self-medium render must set OVERLAY_FLAG='' so the dir-medium materialization branch is not taken at runtime")
	}
	for _, want := range []string{
		`if ! HMNT test -e "$BUNDLE/$dstn/rootfs/${cwd#/}"; then`, // the fail-closing dentry test
		`MISSING the container CWD dentry`,                        // by-name FATAL
		`return 1`,                                                // fail-closed
	} {
		if !strings.Contains(self, want) {
			t.Errorf("self-medium thaw script must RETAIN the pre-restore CWD-dentry assertion: %q", want)
		}
	}
	bashParses(t, "thaw(self-medium-cwd-retained)", self)
}

// TestThawScript_DirMediumPostRestoreWorkspaceLiveness verifies gardener steer
// req-2 (ka-0000000357) HARDENED to NON-EMPTINESS by ka-0000004935. Under the dir
// medium the pre-restore lower-only fail-close is removed and the cwd dir is
// MATERIALIZED on the lower (so restore can re-walk it). That makes the cwd path
// ALWAYS EXIST post-restore — so the post-restore liveness guard must check
// NON-EMPTINESS (entry count > 0), not mere existence: existence is provided by the
// lower-side mkdir, but the CONTENT can only come from the rebuilt upper (checkpoint
// pages). A populated cwd => the upper round-tripped; an EMPTY cwd => real data loss
// => fail-closed FT_ROOT_RC=128 by name (last-wins over the earlier FT_ROOT_RC=0) so
// the controller cold-starts instead of resuming a workspace-less agent.
//
//   - dir medium: RUNSC exec the restored agent, count entries under its checkpointed
//     cwd INSIDE the restored sentry; >0 => yes, ==0 => no (fail-close), non-integer
//     (flaky exec) => unknown (let the restore stand). Runs AFTER restore
//     (FT_RESTORED_PID known); the 128 last-wins over the restore-success rc.
//   - self medium: NO such post-restore exec (the pre-restore host-lower check
//     already proved the path); zero blast radius.
func TestThawScript_DirMediumPostRestoreWorkspaceLiveness(t *testing.T) {
	m := NewFreezeThawManager(nil, nil, nil, nil, testLogger())
	base := thawScriptParams{
		PlaceholderPod: "podb", BPodUID: "buid", ASandboxCID: "asb", AAgentCID: "aag",
		APodUID: "apod", ImagePath: "/img", BundlePath: "/bndl", ContainerCount: 2, RunscTimeout: 90,
		SubContainers: []thawSubContainer{{Name: "agent", ACID: "a"}},
	}

	dirParams := base
	dirParams.RestoreOverlayFlag = overlayRestoreFlag("root:dir=/var/lib/gardenio/overlay")
	dir := m.buildThawScript(dirParams)
	for _, want := range []string{
		`ACNT=$(RUNSC exec "$ABC" sh -c 'ls -A1 "$1" 2>/dev/null | wc -l' ls-cwd "$ACWD" 2>/dev/null | tr -dc '0-9')`, // COUNT entries inside the restored sentry ($ACWD passed as a positional arg — single-quote-safe)
		`FT_DIRMEDIUM_CWD_LIVE=yes`,     // present and non-empty
		`FT_DIRMEDIUM_CWD_LIVE=no`,      // present but empty: real loss
		`FT_DIRMEDIUM_CWD_LIVE=unknown`, // inconclusive (flaky exec)
		`ABC=$(bcid_for agent)`,         // resolve the restored agent cid
		`present but EMPTY (0 entries)`, // by-name FATAL on real loss
	} {
		if !strings.Contains(dir, want) {
			t.Errorf("dir-medium thaw must carry the post-restore NON-EMPTY workspace-liveness guard: %q", want)
		}
	}
	// The guard MUST be non-emptiness, NOT a bare existence test — otherwise the
	// ka-0000004935 lower-side mkdir (which guarantees the dir exists) would defeat it
	// and silently mask real data loss. Pin out the old `test -e` existence form.
	if strings.Contains(dir, `RUNSC exec "$ABC" test -e "$ACWD"`) {
		t.Error("post-restore liveness must check NON-EMPTINESS (entry count), not `test -e` existence — the lower-side cwd mkdir guarantees existence, so `test -e` would mask real upper-content loss")
	}
	// rc DISAMBIGUATION: fail-closed ONLY on a real empty dir (count==0); a flaky exec
	// that yields no integer must NOT fail-close (warn + let the restore stand), else a
	// transient exec hiccup converts a good thaw into a needless cold-start.
	if !strings.Contains(dir, `if [ -z "$ACNT" ]; then`) {
		t.Error("liveness guard must treat a missing count (flaky exec) as inconclusive, not a failure")
	}
	if !strings.Contains(dir, `elif [ "$ACNT" -gt 0 ]; then`) {
		t.Error("liveness guard must record LIVE=yes only when the entry count is > 0")
	}
	// The inconclusive (no-count) branch must NOT emit FT_ROOT_RC=128/exit; only the
	// count==0 (else) branch fail-closes. Scope the search to the liveness block (from
	// the LIVE=unknown marker onward) so the global netns-guard FT_ROOT_RC=128 sites
	// earlier in the script do not confound the ordering.
	iUnknown := strings.Index(dir, `FT_DIRMEDIUM_CWD_LIVE=unknown`)
	if iUnknown < 0 {
		t.Fatal("post-restore liveness inconclusive branch (LIVE=unknown) not rendered")
	}
	iNo := strings.Index(dir, `FT_DIRMEDIUM_CWD_LIVE=no`)
	iExitInLiveness := strings.Index(dir[iUnknown:], `echo "FT_ROOT_RC=128"`)
	if iNo < 0 || iExitInLiveness < 0 || !(iUnknown < iNo) {
		t.Error("the inconclusive (no-count) branch must precede the count==0 LIVE=no fail-close; only count==0 emits FT_ROOT_RC=128")
	}
	// The guard must fail-closed on an empty dir: the FATAL is followed by
	// FT_ROOT_RC=128 + exit 1 (the long diagnostic message sits between them, so
	// scan the whole tail, not a fixed window).
	iGuard := strings.Index(dir, `present but EMPTY (0 entries)`)
	if iGuard < 0 {
		t.Fatal("post-restore liveness FATAL not rendered")
	}
	after := dir[iGuard:]
	i128 := strings.Index(after, `echo "FT_ROOT_RC=128"`)
	iExitAfter := strings.Index(after, "exit 1")
	if i128 < 0 || iExitAfter < 0 || !(i128 < iExitAfter) {
		t.Error("post-restore liveness guard must emit FT_ROOT_RC=128 then exit 1 on an empty cwd (fail-closed to cold-start)")
	}
	// The whole guard is runtime-gated by `if [ -n "$OVERLAY_FLAG" ]; then` so it
	// is INERT on self-medium nodes (OVERLAY_FLAG empty) — the exec never runs.
	iGate := strings.Index(dir, `if [ -n "$OVERLAY_FLAG" ]; then
  ACWD=`)
	iLive := strings.Index(dir, `ACNT=$(RUNSC exec "$ABC"`)
	if iGate < 0 || iLive < 0 || !(iGate < iLive) {
		t.Errorf("post-restore liveness exec must sit inside the `if [ -n \"$OVERLAY_FLAG\" ]` runtime gate (gate=%d live=%d)", iGate, iLive)
	}
	// Ordering: the liveness exec must come AFTER FT_RESTORED_PID is set (restore
	// done) and after the netns-content assertion's FT_ROOT_RC sites — so a 128
	// here last-wins over the FT_ROOT_RC=$ROOTRC emitted on restore success.
	iPID := strings.Index(dir, "FT_RESTORED_PID=")
	if iPID < 0 || iLive < 0 || !(iPID < iLive) {
		t.Errorf("post-restore liveness must run after FT_RESTORED_PID is known (pid=%d live=%d)", iPID, iLive)
	}
	iRootRC := strings.LastIndex(dir[:iLive], "FT_ROOT_RC=$ROOTRC")
	if iRootRC < 0 {
		t.Error("the restore-success FT_ROOT_RC=$ROOTRC must precede the post-restore liveness guard (so a later 128 last-wins)")
	}
	bashParses(t, "thaw(dir-medium-postrestore-liveness)", dir)

	// self-medium (zero blast radius): the post-restore liveness block is runtime-
	// gated by `if [ -n "$OVERLAY_FLAG" ]` and self renders OVERLAY_FLAG='' , so the
	// exec never runs. The bash body text is identical across renders; the divergence
	// is the OVERLAY_FLAG value + runtime gating, so we assert the empty flag rather
	// than absence of the (always-present) block text.
	self := m.buildThawScript(base)
	if !strings.Contains(self, `OVERLAY_FLAG=''`) {
		t.Error("self-medium render must set OVERLAY_FLAG='' so the dir-medium post-restore liveness exec is not taken at runtime")
	}
}

// TestThaw_DirMediumReconstructedLowerCarriesWorkspace is the ka-0000004935
// behavioral regression test the task requires: it asserts that the dir-medium
// reconstruction makes the relocated lower CARRY home/agent/workspace even when the
// freeze relocation produced a lower MISSING /home entirely (the exact prod
// forensic). It executes the SAME shell the thaw script runs in remount_rootfs's
// dir-medium branch (read process.cwd + process.user from config.json; if the cwd
// is missing from the reconstructed rootfs, mkdir -p the chain and chown it to the
// checkpointed owner) against a temp "reconstructed lower" that, like the failing
// bundle, has proc/run/usr but NO /home — and verifies home/agent/workspace then
// exists as a directory. This is the on-disk proof that 'runsc restore's gofer
// re-walk of mount "agent:/" will resolve home/agent/workspace instead of ENOENTing
// rc=128. (The string-level wiring is pinned by
// TestThawScript_DirMediumMaterializesCWDOnLower; this proves the shell semantics.)
//
// SCOPE CAVEAT: this test mkdirs into a PLAIN temp directory — it establishes NO host
// overlay mount. It therefore proves the materialize SHELL SEMANTICS (read cwd/user,
// mkdir -p the missing chain, chown, leave it empty), NOT the upper-vs-lower physical
// placement. In production the mkdir targets the live overlay mountpoint
// ($BUNDLE/$dstn/rootfs), so the write lands in the host-overlay upper
// ($OVER/$dstn/upper) and surfaces at the merged view the restore gofer serves; that
// layer-placement is not (and need not be) exercised here.
func TestThaw_DirMediumReconstructedLowerCarriesWorkspace(t *testing.T) {
	root := t.TempDir()
	rootfs := filepath.Join(root, "rootfs")
	// Mirror the failing dir-medium relocated lower: proc/run/usr present, /home ABSENT.
	for _, d := range []string{"proc", "run", "usr/local"} {
		if err := os.MkdirAll(filepath.Join(rootfs, d), 0o755); err != nil {
			t.Fatalf("seed lower %s: %v", d, err)
		}
	}
	// The agent OCI config: process.cwd is the image WORKDIR; user is the agent uid.
	cfg := filepath.Join(root, "config.json")
	if err := os.WriteFile(cfg, []byte(`{"process":{"cwd":"/home/agent/workspace","user":{"uid":1000,"gid":1000}}}`), 0o644); err != nil {
		t.Fatalf("write config.json: %v", err)
	}
	// Pre-condition: the workspace path is genuinely missing from the reconstruction.
	if _, err := os.Stat(filepath.Join(rootfs, "home/agent/workspace")); !os.IsNotExist(err) {
		t.Fatalf("precondition: home/agent/workspace must be absent before materialization (err=%v)", err)
	}

	// The materialization shell the thaw script runs (HMNT stripped — same commands).
	// cwd = process.cwd; only mkdir when missing; chown to the checkpointed owner.
	script := `set -uo pipefail
ROOTFS="$1"; CFG="$2"
cwd=$(python3 -c 'import json,sys
try: print(json.load(open(sys.argv[1])).get("process",{}).get("cwd",""))
except Exception: print("")' "$CFG")
[ -n "$cwd" ] && [ "$cwd" != "/" ] || exit 0
if [ -e "$ROOTFS/${cwd#/}" ]; then exit 0; fi
cwduid=$(python3 -c 'import json,sys
try:
    u=json.load(open(sys.argv[1])).get("process",{}).get("user",{})
    print("%d:%d"%(int(u.get("uid",1000)),int(u.get("gid",1000))))
except Exception: print("1000:1000")' "$CFG")
[ -z "$cwduid" ] && cwduid="1000:1000"
mkdir -p "$ROOTFS/${cwd#/}" || exit 1
# chown best-effort (the test runs unprivileged; ownership is not what we assert).
chown "$cwduid" "$ROOTFS/${cwd#/}" 2>/dev/null || true
:`
	cmd := exec.Command("bash", "-c", script, "materialize", rootfs, cfg)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("materialization shell failed: %v\n%s", err, out)
	}

	// Post-condition: the full chain now exists as directories on the lower — so the
	// restore gofer re-walk of home/agent/workspace in mount "agent:/" resolves.
	for _, p := range []string{"home", "home/agent", "home/agent/workspace"} {
		fi, err := os.Stat(filepath.Join(rootfs, p))
		if err != nil {
			t.Errorf("reconstructed lower must carry %q after materialization: %v", p, err)
			continue
		}
		if !fi.IsDir() {
			t.Errorf("%q must be a directory (gVisor restore stats the dentry type)", p)
		}
	}
	// And it must be EMPTY (content rides the upper / checkpoint pages, not the lower).
	entries, err := os.ReadDir(filepath.Join(rootfs, "home/agent/workspace"))
	if err != nil {
		t.Fatalf("read materialized workspace: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("materialized workspace must be EMPTY (content overlays from the upper); got %d entries", len(entries))
	}
}
