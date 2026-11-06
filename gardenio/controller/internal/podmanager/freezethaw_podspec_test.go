package podmanager

import (
	"log/slog"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// These tests cover the C3 podspec wiring (ka-0000004017): the per-agent
// thaw-state hostPath volume/mount and the COOP_THAW_STATE_DIR env are added
// ONLY to runsc-pinned pods so the in-pod gb+coop thaw watchers run in prod —
// and never leak a hostPath onto non-gVisor pods.

func TestThawState_WiredOnlyForRunsc(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.RuntimeClassName = RuntimeClassRunsc

	// Volume present, sourced from the per-agent host dir.
	vols := m.buildVolumes(spec)
	var tv *string
	for i := range vols {
		if vols[i].Name == VolumeThawState {
			if vols[i].VolumeSource.HostPath == nil {
				t.Fatal("thaw-state volume must be a hostPath")
			}
			p := vols[i].VolumeSource.HostPath.Path
			tv = &p
		}
	}
	if tv == nil {
		t.Fatal("runsc pod missing thaw-state volume")
	}
	if want := thawStateHostDir(spec.AgentName); *tv != want {
		t.Errorf("thaw-state hostPath = %q, want %q", *tv, want)
	}

	// Mount present at the in-pod marker dir, read-write (agent writes acks).
	mounts := m.buildVolumeMounts(spec)
	foundMount := false
	for _, mt := range mounts {
		if mt.Name == VolumeThawState {
			foundMount = true
			if mt.MountPath != MountThawState {
				t.Errorf("thaw-state mountPath = %q, want %q", mt.MountPath, MountThawState)
			}
			if mt.ReadOnly {
				t.Error("thaw-state mount must be read-write so the agent can write acks")
			}
		}
	}
	if !foundMount {
		t.Error("runsc pod missing thaw-state mount")
	}

	// Env points the watchers at the mount.
	env := m.buildEnvVars(spec)
	got := ""
	for _, e := range env {
		if e.Name == EnvThawStateDir {
			got = e.Value
		}
	}
	if got != MountThawState {
		t.Errorf("%s = %q, want %q", EnvThawStateDir, got, MountThawState)
	}
}

func TestThawState_AbsentForNonRunsc(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec() // no RuntimeClassName

	for _, v := range m.buildVolumes(spec) {
		if v.Name == VolumeThawState {
			t.Error("non-runsc pod must not carry a thaw-state hostPath volume")
		}
	}
	for _, mt := range m.buildVolumeMounts(spec) {
		if mt.Name == VolumeThawState {
			t.Error("non-runsc pod must not mount thaw-state")
		}
	}
	for _, e := range m.buildEnvVars(spec) {
		if e.Name == EnvThawStateDir {
			t.Errorf("non-runsc pod must not set %s", EnvThawStateDir)
		}
	}
}

func TestThawStateHostDir_SanitizesAgentName(t *testing.T) {
	cases := map[string]string{
		"gardenio-phase-4-implement-zzc": ThawStateHostBase + "/gardenio-phase-4-implement-zzc",
		"weird/../name":                  ThawStateHostBase + "/weird_.._name",
		"":                               ThawStateHostBase + "/agent",
		"a b$c":                          ThawStateHostBase + "/a_b_c",
	}
	for in, want := range cases {
		if got := thawStateHostDir(in); got != want {
			t.Errorf("thawStateHostDir(%q) = %q, want %q", in, got, want)
		}
	}
	// No path traversal escapes the base dir: slashes are stripped, so the
	// agent segment can never introduce a new path component (dots alone, with
	// no separator, cannot traverse).
	got := thawStateHostDir("x/../../etc")
	seg := strings.TrimPrefix(got, ThawStateHostBase+"/")
	if strings.Contains(seg, "/") {
		t.Errorf("sanitized agent segment introduced a path separator: %q", got)
	}
}

// TestThawState_PlaceholderPodBInheritsVolume proves Pod B carries the thaw
// state volume the marker writer addresses — it inherits it via the snapshot's
// PodTemplate deep-copy (the template is captured from a live runsc Pod A that
// already had the volume wired). Without it, the controller would write the
// marker to a node dir the thawed pod never mounts.
func TestThawState_PlaceholderPodBInheritsVolume(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.RuntimeClassName = RuntimeClassRunsc

	// Simulate the live Pod A spec: build the pod the manager would create, then
	// sanitize it into a snapshot template, then mint Pod B from it.
	podA := m.buildPod(spec)
	snap := Snapshot{
		AgentName: spec.AgentName, Namespace: spec.Namespace, PodName: spec.PodName(),
		Node: "node-1", PodTemplate: sanitizePodTemplate(podA.Spec),
	}
	ft := &FreezeThawManager{}
	podB := ft.buildPlaceholderPodB(snap, "agentsim-thaw-b")

	found := false
	for _, v := range podB.Spec.Volumes {
		if v.Name == VolumeThawState && v.VolumeSource.HostPath != nil {
			found = true
		}
	}
	if !found {
		t.Error("placeholder Pod B did not inherit the thaw-state hostPath volume from the template")
	}
}

// TestPlaceholderPodB_KeepsInitCloneForRepoRehydrate proves C6 strategy (b):
// Pod B retains the run-to-completion init-clone container so the workspace
// (the repo) is re-hydrated before the graft. The gVisor checkpoint carries the
// agent process RAM + its rootfs overlay but NOT the workspace volume contents,
// so without init-clone a restored agent wakes into an empty /home/agent/bot.
func TestPlaceholderPodB_KeepsInitCloneForRepoRehydrate(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.RuntimeClassName = RuntimeClassRunsc
	spec.GitURL = "https://github.com/acme/platform.git" // makes buildPod add init-clone

	podA := m.buildPod(spec)
	// Sanity: the live Pod A actually has an init-clone init container.
	var haveInitClone bool
	for _, ic := range podA.Spec.InitContainers {
		if ic.Name == InitCloneName {
			haveInitClone = true
		}
	}
	if !haveInitClone {
		t.Fatalf("test setup: live Pod A missing init-clone (init containers: %+v)", podA.Spec.InitContainers)
	}

	snap := Snapshot{
		AgentName: spec.AgentName, Namespace: spec.Namespace, PodName: spec.PodName(),
		Node: "node-1", ContainerCount: 2, PodTemplate: sanitizePodTemplate(podA.Spec),
	}
	ft := &FreezeThawManager{logger: slog.Default()}
	podB := ft.buildPlaceholderPodB(snap, "test-thaw-b")

	var keptInitClone bool
	for _, ic := range podB.Spec.InitContainers {
		if ic.Name == InitCloneName {
			keptInitClone = true
		}
	}
	if !keptInitClone {
		t.Errorf("Pod B must keep init-clone for repo rehydration, got init containers %+v", podB.Spec.InitContainers)
	}
}

// TestPlaceholderPodB_KeepsAlwaysOnSidecars proves Pod B now KEEPS native
// sidecars (DinD/Postgres — init containers with restartPolicy: Always) so the
// kubelet provisions their container slots for the N-container barrier restore
// (ka-0000003561), while still keeping the plain init-clone container. The kept
// sidecars are sleep-overridden so their live body never races the graft.
func TestPlaceholderPodB_KeepsAlwaysOnSidecars(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.RuntimeClassName = RuntimeClassRunsc
	spec.GitURL = "https://github.com/acme/platform.git"
	spec.Docker = true
	spec.Postgres = true

	podA := m.buildPod(spec)
	snap := Snapshot{
		AgentName: spec.AgentName, Namespace: spec.Namespace, PodName: spec.PodName(),
		Node: "node-1", ContainerCount: 4, PodTemplate: sanitizePodTemplate(podA.Spec),
		SubContainers: []SnapshotContainer{{Name: "agent", CID: "a"}, {Name: DindContainerName, CID: "d"}, {Name: PostgresContainerName, CID: "p"}},
	}
	ft := &FreezeThawManager{logger: slog.Default()}
	podB := ft.buildPlaceholderPodB(snap, "test-thaw-b")

	var haveDind, havePostgres, keptInitClone bool
	wantCmd := strings.Join([]string{"/bin/sh", "-c", "mkdir -p /work/repo 2>/dev/null; exec sleep 86400"}, " ")
	for _, ic := range podB.Spec.InitContainers {
		switch ic.Name {
		case DindContainerName:
			haveDind = true
		case PostgresContainerName:
			havePostgres = true
		case InitCloneName:
			keptInitClone = true
			continue // init-clone keeps its real command
		}
		if ic.Name == DindContainerName || ic.Name == PostgresContainerName {
			// kept sidecars must be sleep-overridden so they don't race the graft
			if got := strings.Join(ic.Command, " "); got != wantCmd {
				t.Errorf("kept sidecar %q command = %q, want sleep placeholder %q", ic.Name, got, wantCmd)
			}
			if ic.RestartPolicy == nil || *ic.RestartPolicy != corev1.ContainerRestartPolicyAlways {
				t.Errorf("kept sidecar %q must retain restartPolicy: Always so the kubelet runs it as a native sidecar slot", ic.Name)
			}
		}
	}
	if !haveDind || !havePostgres {
		t.Errorf("Pod B must KEEP always-on sidecars for the N-container barrier restore (dind=%v postgres=%v)", haveDind, havePostgres)
	}
	if !keptInitClone {
		t.Error("Pod B dropped init-clone; it must be kept for repo rehydration")
	}
}

// TestPartitionInitContainers splits run-to-completion init containers from
// always-on native sidecars by restartPolicy.
func TestPartitionInitContainers(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	inits := []corev1.Container{
		{Name: "init-clone"},
		{Name: "dind", RestartPolicy: &always},
		{Name: "postgres", RestartPolicy: &always},
	}
	keep, sidecars := partitionInitContainers(inits)
	if len(keep) != 1 || keep[0].Name != "init-clone" {
		t.Errorf("keep = %+v, want [init-clone]", keep)
	}
	if len(sidecars) != 2 {
		t.Errorf("sidecars = %+v, want 2 (dind, postgres)", sidecars)
	}
}

// A tenant land pod (Lands step 10.2) runs under runsc with NoThawState: no
// hostPath, no root init container, no thaw env, so it is admissible under
// the land namespace's PodSecurity restricted.
func TestThawState_NoThawStateKeepsRunscPodOffHostPath(t *testing.T) {
	m := newTestManager()
	spec := minimalSpec()
	spec.RuntimeClassName = RuntimeClassRunsc
	spec.NoThawState = true

	pod := m.buildPod(spec)
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != RuntimeClassRunsc {
		t.Fatalf("runtime class = %v, want runsc", pod.Spec.RuntimeClassName)
	}
	for _, v := range pod.Spec.Volumes {
		if v.HostPath != nil {
			t.Errorf("volume %s is a hostPath", v.Name)
		}
	}
	for _, c := range append(append([]corev1.Container(nil), pod.Spec.InitContainers...), pod.Spec.Containers...) {
		if c.Name == ThawStatePermsInitName {
			t.Error("thaw-state-perms init container present")
		}
		for _, mt := range c.VolumeMounts {
			if mt.Name == VolumeThawState {
				t.Errorf("%s mounts thaw-state", c.Name)
			}
		}
		for _, e := range c.Env {
			if e.Name == EnvThawStateDir {
				t.Errorf("%s has %s", c.Name, EnvThawStateDir)
			}
		}
	}
}
