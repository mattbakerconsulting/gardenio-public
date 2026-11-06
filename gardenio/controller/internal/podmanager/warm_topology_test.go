package podmanager

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// agentLikeTemplate builds a pod template that resembles a real agent pod: an
// agent container mounting the workspace + /work, an init-clone container for
// project, and optionally a dind sidecar (native sidecar = restartPolicy Always).
func agentLikeTemplate(project, gitURL string, withDind bool) corev1.PodSpec {
	always := corev1.ContainerRestartPolicyAlways
	spec := corev1.PodSpec{
		Containers: []corev1.Container{
			{
				Name: "agent",
				VolumeMounts: []corev1.VolumeMount{
					{Name: VolumeWorkspace, MountPath: MountWorkspace},
					{Name: VolumeWork, MountPath: WarmPlaceholderWorkPath},
				},
			},
		},
		InitContainers: []corev1.Container{
			{
				Name:    InitCloneName,
				Command: []string{"/bin/sh", "-c", "set -e\ngit clone -b main " + gitURL + " " + MountWorkspace + "/" + project + "/work\ngit config user.name agent-x\ngit config user.email agent-x@gardenio\n"},
				VolumeMounts: []corev1.VolumeMount{
					{Name: VolumeWorkspace, MountPath: MountWorkspace},
				},
			},
		},
	}
	if withDind {
		spec.InitContainers = append(spec.InitContainers, corev1.Container{
			Name:          "dind",
			RestartPolicy: &always,
			VolumeMounts:  []corev1.VolumeMount{{Name: "dind-storage", MountPath: "/var/lib/docker"}},
		})
	}
	return spec
}

func TestSnapshotTopologyKey_StableAndShapeSensitive(t *testing.T) {
	base := Snapshot{PodTemplate: agentLikeTemplate("gardenio", "https://gitlab.com/x/gardenio", false)}

	// Deterministic: same template → same key. Compute into distinct vars so
	// staticcheck doesn't flag a self-comparison (SA4000).
	k1, k2 := SnapshotTopologyKey(base), SnapshotTopologyKey(base)
	if k1 != k2 {
		t.Fatal("topology key must be deterministic")
	}

	// Agent-identity differences (git user.name/email) must NOT change the bucket:
	// same project+repo agents share a placeholder pool.
	other := Snapshot{PodTemplate: agentLikeTemplate("gardenio", "https://gitlab.com/x/gardenio", false)}
	other.PodTemplate.InitContainers[0].Command = []string{"/bin/sh", "-c",
		"set -e\ngit clone -b main https://gitlab.com/x/gardenio " + MountWorkspace + "/gardenio/work\ngit config user.name DIFFERENT\ngit config user.email different@gardenio\n"}
	if SnapshotTopologyKey(base) != SnapshotTopologyKey(other) {
		t.Errorf("agent-identity-only differences must share a topology bucket")
	}

	// A different repo → different bucket (gap 2: must not pre-clone the wrong repo).
	diffRepo := Snapshot{PodTemplate: agentLikeTemplate("other", "https://gitlab.com/x/other", false)}
	if SnapshotTopologyKey(base) == SnapshotTopologyKey(diffRepo) {
		t.Errorf("different repo must not share a topology bucket")
	}

	// A different mount shape (added sidecar) → different bucket (gap 1).
	withSidecar := Snapshot{PodTemplate: agentLikeTemplate("gardenio", "https://gitlab.com/x/gardenio", true)}
	if SnapshotTopologyKey(base) == SnapshotTopologyKey(withSidecar) {
		t.Errorf("sidecar presence must change the topology bucket")
	}
}

// TestSnapshotTopologyKey_DNSShapeSensitive guards ka-0000004024: a checkpoint
// must only be grafted onto a placeholder whose DNS resolution shape matches the
// frozen agent's, because the restore repoints the restored sandbox's
// /etc/resolv.conf onto the CLAIMED placeholder's own kubelet-generated file. A
// differing DNS search domain / policy / nameserver set therefore MUST land the
// snapshot in a different bucket; an agent-identity-equivalent DNS shape must not.
func TestSnapshotTopologyKey_DNSShapeSensitive(t *testing.T) {
	base := Snapshot{PodTemplate: agentLikeTemplate("gardenio", "https://gitlab.com/x/gardenio", false)}

	// A DNSConfig search domain (the cross-namespace ka-0000001058 append) must
	// change the bucket: its resolv.conf is materially different.
	withSearch := Snapshot{PodTemplate: agentLikeTemplate("gardenio", "https://gitlab.com/x/gardenio", false)}
	withSearch.PodTemplate.DNSPolicy = corev1.DNSClusterFirst
	withSearch.PodTemplate.DNSConfig = &corev1.PodDNSConfig{Searches: []string{"gardenio-release.svc.cluster.local"}}
	if SnapshotTopologyKey(base) == SnapshotTopologyKey(withSearch) {
		t.Errorf("a DNSConfig search domain must change the topology bucket (restore inherits the claimed member's resolv.conf)")
	}

	// A DIFFERENT search domain must not collide with the first.
	withOtherSearch := Snapshot{PodTemplate: agentLikeTemplate("gardenio", "https://gitlab.com/x/gardenio", false)}
	withOtherSearch.PodTemplate.DNSPolicy = corev1.DNSClusterFirst
	withOtherSearch.PodTemplate.DNSConfig = &corev1.PodDNSConfig{Searches: []string{"other-release.svc.cluster.local"}}
	if SnapshotTopologyKey(withSearch) == SnapshotTopologyKey(withOtherSearch) {
		t.Errorf("distinct DNS search domains must occupy distinct topology buckets")
	}

	// Same DNS shape (identical policy + searches) must share a bucket — the key
	// fences on shape, not on pointer identity.
	sameShape := Snapshot{PodTemplate: agentLikeTemplate("gardenio", "https://gitlab.com/x/gardenio", false)}
	sameShape.PodTemplate.DNSPolicy = corev1.DNSClusterFirst
	sameShape.PodTemplate.DNSConfig = &corev1.PodDNSConfig{Searches: []string{"gardenio-release.svc.cluster.local"}}
	if SnapshotTopologyKey(withSearch) != SnapshotTopologyKey(sameShape) {
		t.Errorf("identical DNS shapes must share a topology bucket")
	}

	// DNSPolicy alone (no DNSConfig) must still register — resolv.conf differs
	// between Default and ClusterFirst.
	policyOnly := Snapshot{PodTemplate: agentLikeTemplate("gardenio", "https://gitlab.com/x/gardenio", false)}
	policyOnly.PodTemplate.DNSPolicy = corev1.DNSDefault
	if SnapshotTopologyKey(base) == SnapshotTopologyKey(policyOnly) {
		t.Errorf("a differing DNSPolicy must change the topology bucket")
	}
}

func TestBuildWarmPlaceholderFromSnapshot_MatchesShapeAndTopology(t *testing.T) {
	snap := Snapshot{
		AgentName:   "agent-x",
		Namespace:   "gvisor",
		Node:        "node-a",
		PodTemplate: agentLikeTemplate("gardenio", "https://gitlab.com/x/gardenio", true),
	}
	pod := BuildWarmPlaceholderFromSnapshot("warm-b-x", snap, "")

	if pod.Spec.NodeName != "node-a" {
		t.Errorf("expected node pin from snapshot, got %q", pod.Spec.NodeName)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != RuntimeClassRunsc {
		t.Error("expected runsc runtime class")
	}
	if pod.Labels[LabelWarmTopology] != SnapshotTopologyKey(snap) {
		t.Errorf("topology label = %q, want %q", pod.Labels[LabelWarmTopology], SnapshotTopologyKey(snap))
	}
	if pod.Labels[LabelWarmPlaceholder] != WarmPlaceholderReady {
		t.Errorf("expected ready label")
	}
	if _, ok := pod.Labels[LabelAgent]; ok {
		t.Error("warm placeholder must NOT carry the agent label")
	}

	// The agent container's mount destinations must be carried verbatim so a
	// graft passes runsc mount validation (gap 1) — workspace AND /work present.
	var agentC *corev1.Container
	for i := range pod.Spec.Containers {
		if pod.Spec.Containers[i].Name == "agent" {
			agentC = &pod.Spec.Containers[i]
		}
	}
	if agentC == nil {
		t.Fatal("expected an agent container in the placeholder")
	}
	dsts := map[string]bool{}
	for _, m := range agentC.VolumeMounts {
		dsts[m.MountPath] = true
	}
	if !dsts[MountWorkspace] || !dsts[WarmPlaceholderWorkPath] {
		t.Errorf("agent mount destinations not carried from snapshot: %v", dsts)
	}
	// The app container command must be the sleep shell, not the real agent body.
	if len(agentC.Command) == 0 || agentC.Command[0] != "/bin/sh" {
		t.Errorf("expected sleep shell, got %v", agentC.Command)
	}

	// init-clone (repo rehydration, gap 2) must be KEPT unchanged so the repo is
	// cloned before the graft.
	var foundInitClone, foundDindSidecar bool
	for _, c := range pod.Spec.InitContainers {
		if c.Name == InitCloneName {
			foundInitClone = true
			// init-clone command must be UNCHANGED (still clones), not sleep-shelled.
			if len(c.Command) == 0 || c.Command[len(c.Command)-1] == placeholderSleepCommand[len(placeholderSleepCommand)-1] {
				t.Errorf("init-clone command must be preserved, got %v", c.Command)
			}
		}
		if c.Name == "dind" {
			foundDindSidecar = true
			// sidecar must be sleep-shelled (kubelet provisions the slot; restore grafts).
			if len(c.Command) == 0 || c.Command[len(c.Command)-1] != placeholderSleepCommand[len(placeholderSleepCommand)-1] {
				t.Errorf("dind sidecar must be sleep-shelled, got %v", c.Command)
			}
		}
	}
	if !foundInitClone {
		t.Error("init-clone must be kept on the warm placeholder (repo rehydration)")
	}
	if !foundDindSidecar {
		t.Error("always-on dind sidecar must be kept on the warm placeholder (N-container restore)")
	}
}

func TestWarmBuckets_DedupsByNodeAndTopology(t *testing.T) {
	store := NewInMemorySnapshotStore()
	ctx := context.Background()
	// Two same-shape agents on the same node → one bucket.
	tmpl := agentLikeTemplate("gardenio", "https://gitlab.com/x/gardenio", false)
	_ = store.Put(ctx, Snapshot{AgentName: "a1", Node: "node-a", PodTemplate: tmpl})
	_ = store.Put(ctx, Snapshot{AgentName: "a2", Node: "node-a", PodTemplate: tmpl})
	// Same shape, different node → distinct bucket.
	_ = store.Put(ctx, Snapshot{AgentName: "a3", Node: "node-b", PodTemplate: tmpl})
	// Different shape, same node → distinct bucket.
	_ = store.Put(ctx, Snapshot{AgentName: "a4", Node: "node-a", PodTemplate: agentLikeTemplate("other", "https://gitlab.com/x/other", true)})
	// No node → skipped.
	_ = store.Put(ctx, Snapshot{AgentName: "a5", Node: "", PodTemplate: tmpl})

	mgr := NewFreezeThawManager(nil, nil, store, nil, testLogger())
	buckets, err := mgr.WarmBuckets(ctx)
	if err != nil {
		t.Fatalf("WarmBuckets: %v", err)
	}
	if len(buckets) != 3 {
		t.Fatalf("expected 3 distinct (node,topology) buckets, got %d: %+v", len(buckets), buckets)
	}
	// Sanity: every bucket carries a representative snapshot with a node.
	for _, b := range buckets {
		if b.Node == "" || b.Topology == "" || b.Snapshot.AgentName == "" {
			t.Errorf("malformed bucket: %+v", b)
		}
	}
}

func TestSnapshotTopology_AccessorMatchesKey(t *testing.T) {
	store := NewInMemorySnapshotStore()
	ctx := context.Background()
	snap := Snapshot{AgentName: "a1", Node: "node-a", PodTemplate: agentLikeTemplate("gardenio", "https://gitlab.com/x/gardenio", false)}
	_ = store.Put(ctx, snap)
	mgr := NewFreezeThawManager(nil, nil, store, nil, testLogger())

	topo, ok := mgr.SnapshotTopology(ctx, "a1")
	if !ok {
		t.Fatal("expected a topology for a seeded snapshot")
	}
	if topo != SnapshotTopologyKey(snap) {
		t.Errorf("accessor topology %q != key %q", topo, SnapshotTopologyKey(snap))
	}
	if _, ok := mgr.SnapshotTopology(ctx, "missing"); ok {
		t.Error("expected miss for an unknown agent")
	}
}

// TestShapePlaceholderSpec_FloorsRequestsKeepsLimits is the ka-0000008044 fix:
// a sleep-shelled placeholder built from a snapshot whose app container / sidecar
// inherit the agent's full 2-CPU/8Gi REQUEST must be relaxed to the admission
// floor (so it fits on a node already running the live agent) while keeping the
// template's LIMIT (so the grafted agent retains its real burst ceiling).
func TestShapePlaceholderSpec_FloorsRequestsKeepsLimits(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways
	agentRes := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("8Gi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("4"),
			corev1.ResourceMemory: resource.MustParse("16Gi"),
		},
	}
	template := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:      "agent",
			Resources: *agentRes.DeepCopy(),
			VolumeMounts: []corev1.VolumeMount{
				{Name: VolumeWorkspace, MountPath: MountWorkspace},
				{Name: VolumeWork, MountPath: WarmPlaceholderWorkPath},
			},
		}},
		InitContainers: []corev1.Container{
			{
				Name:    InitCloneName,
				Command: []string{"/bin/sh", "-c", "git clone ...\n"},
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
			},
			{
				Name:          "dind",
				RestartPolicy: &always,
				Resources:     *agentRes.DeepCopy(),
			},
		},
	}

	// pagesBytes=0 = "no snapshot pages captured" → the memory request must
	// COLLAPSE to the 256Mi floor exactly as pre-fix behavior. This preserves
	// the original assertion in ka-0000008044 semantics (a legacy snapshot with
	// no PagesBytes never regresses).
	spec := shapePlaceholderSpec(template, "node-a", 0, 2)

	// App container: request floored, limit preserved.
	agentC := spec.Containers[0]
	if got := agentC.Resources.Requests.Cpu(); got.Cmp(placeholderCPURequestFloor) != 0 {
		t.Errorf("agent CPU request = %s, want floor %s", got, placeholderCPURequestFloor.String())
	}
	if got := agentC.Resources.Requests.Memory(); got.Cmp(placeholderMemRequestFloor) != 0 {
		t.Errorf("agent memory request = %s, want floor %s", got, placeholderMemRequestFloor.String())
	}
	if got := agentC.Resources.Limits.Cpu(); got.Cmp(resource.MustParse("4")) != 0 {
		t.Errorf("agent CPU limit = %s, want preserved 4", got)
	}
	if got := agentC.Resources.Limits.Memory(); got.Cmp(resource.MustParse("16Gi")) != 0 {
		t.Errorf("agent memory limit = %s, want preserved 16Gi", got)
	}

	// Find the dind sidecar and init-clone in the reordered init list.
	var dindC, cloneC *corev1.Container
	for i := range spec.InitContainers {
		switch spec.InitContainers[i].Name {
		case "dind":
			dindC = &spec.InitContainers[i]
		case InitCloneName:
			cloneC = &spec.InitContainers[i]
		}
	}
	if dindC == nil || cloneC == nil {
		t.Fatalf("expected both dind sidecar and init-clone, got %+v", spec.InitContainers)
	}

	// Sidecar: CPU request floored, limit absent in template stays absent.
	if got := dindC.Resources.Requests.Cpu(); got.Cmp(placeholderCPURequestFloor) != 0 {
		t.Errorf("dind CPU request = %s, want floor %s", got, placeholderCPURequestFloor.String())
	}
	// Sidecar memory on the pagesBytes=0 path collapses to the 256Mi floor — the
	// sidecar does NOT carry the pages fence (ka-0000005097); it is floored off
	// pagesBytes=0 regardless of the snapshot's pages size (asserted with pages>0
	// in TestShapePlaceholderSpec_MultiSidecarFenceRightSized). Guards the
	// pagesBytes=0 branch of the sidecar right-size against a revert.
	if got := dindC.Resources.Requests.Memory(); got.Cmp(placeholderMemRequestFloor) != 0 {
		t.Errorf("dind memory request = %s, want floor %s (sidecar must not carry pages fence)", got, placeholderMemRequestFloor.String())
	}

	// init-clone does real work (kept command) and already requests below the
	// floor — it must be left untouched.
	if got := cloneC.Resources.Requests.Cpu(); got.Cmp(resource.MustParse("100m")) != 0 {
		t.Errorf("init-clone CPU request = %s, want preserved 100m", got)
	}

	// The original template must NOT be mutated (deep-copied first).
	if got := template.Containers[0].Resources.Requests.Cpu(); got.Cmp(resource.MustParse("2")) != 0 {
		t.Errorf("template was mutated: agent CPU request = %s, want original 2", got)
	}
}

// TestShapePlaceholderSpec_KeepsSubFloorRequests verifies a template that already
// requests below the floor is left as-is (the floor only lowers, never raises).
func TestShapePlaceholderSpec_KeepsSubFloorRequests(t *testing.T) {
	template := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "agent",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("50m"),
				},
			},
		}},
	}
	spec := shapePlaceholderSpec(template, "node-a", 0, 2)
	if got := spec.Containers[0].Resources.Requests.Cpu(); got.Cmp(resource.MustParse("50m")) != 0 {
		t.Errorf("sub-floor CPU request = %s, want preserved 50m", got)
	}
}
