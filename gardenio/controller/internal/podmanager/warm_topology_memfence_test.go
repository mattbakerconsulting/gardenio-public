package podmanager

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// TestShapePlaceholderSpec_MultiSidecarFenceRightSized is the ka-0000005097
// fix: on a multi-sidecar pod (DinD + Postgres), the pages fence must be carried
// ONCE — on the app container — while every always-on sidecar floors to 256Mi.
// pages.img is a single sandbox-wide checkpoint mapped once for the whole sentry,
// so pagesBytes is the pod's total restore RAM need, not a per-container quantity;
// the scheduler sums all container requests into one pod-level reservation.
// Fencing pagesBytes on N+1 containers would reserve (1+N)×pagesBytes and push a
// heavy-tier placeholder past what the subject freeze released — a same-node-thaw
// admission MISS. This test asserts the total memory request is pagesBytes +
// N×256Mi (NOT (1+N)×pagesBytes).
func TestShapePlaceholderSpec_MultiSidecarFenceRightSized(t *testing.T) {
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
			{Name: "dind", RestartPolicy: &always, Resources: *agentRes.DeepCopy()},
			{Name: "postgres", RestartPolicy: &always, Resources: *agentRes.DeepCopy()},
		},
	}

	const pagesBytes int64 = 3_221_225_472 // 3.0 GiB — canary #12 subject size
	spec := shapePlaceholderSpec(template, "node-a", pagesBytes, 2)

	// App container: carries the FULL pages fence.
	wantPages := resource.NewQuantity(pagesBytes, resource.BinarySI)
	if got := spec.Containers[0].Resources.Requests.Memory(); got.Cmp(*wantPages) != 0 {
		t.Errorf("app container memory request = %s, want pages fence %s", got, wantPages.String())
	}

	// Each always-on sidecar floors to 256Mi — NOT pagesBytes.
	var dindC, pgC, cloneC *corev1.Container
	for i := range spec.InitContainers {
		switch spec.InitContainers[i].Name {
		case "dind":
			dindC = &spec.InitContainers[i]
		case "postgres":
			pgC = &spec.InitContainers[i]
		case InitCloneName:
			cloneC = &spec.InitContainers[i]
		}
	}
	if dindC == nil || pgC == nil || cloneC == nil {
		t.Fatalf("expected dind + postgres sidecars and init-clone, got %+v", spec.InitContainers)
	}
	for _, sc := range []*corev1.Container{dindC, pgC} {
		if got := sc.Resources.Requests.Memory(); got.Cmp(placeholderMemRequestFloor) != 0 {
			t.Errorf("sidecar %s memory request = %s, want floor %s (must NOT carry pages fence)",
				sc.Name, got, placeholderMemRequestFloor.String())
		}
		// CPU still floors, limits still preserved (ka-0000008044 invariant).
		if got := sc.Resources.Requests.Cpu(); got.Cmp(placeholderCPURequestFloor) != 0 {
			t.Errorf("sidecar %s CPU request = %s, want floor %s", sc.Name, got, placeholderCPURequestFloor.String())
		}
		if got := sc.Resources.Limits.Memory(); got.Cmp(resource.MustParse("16Gi")) != 0 {
			t.Errorf("sidecar %s memory limit = %s, want preserved 16Gi", sc.Name, got)
		}
	}

	// init-clone (run-to-completion) is never floored.
	if got := cloneC.Resources.Requests.Memory(); got.Cmp(resource.MustParse("128Mi")) != 0 {
		t.Errorf("init-clone memory request = %s, want preserved 128Mi", got)
	}

	// The load-bearing assertion: the POD's total memory request is
	// pagesBytes (app) + 256Mi (dind) + 256Mi (postgres) + 128Mi (init-clone),
	// NOT 3×pagesBytes. Sum the container requests the scheduler will see.
	total := resource.NewQuantity(0, resource.BinarySI)
	total.Add(*spec.Containers[0].Resources.Requests.Memory())
	total.Add(*dindC.Resources.Requests.Memory())
	total.Add(*pgC.Resources.Requests.Memory())
	total.Add(*cloneC.Resources.Requests.Memory())

	want := resource.NewQuantity(pagesBytes, resource.BinarySI)
	want.Add(placeholderMemRequestFloor) // dind
	want.Add(placeholderMemRequestFloor) // postgres
	want.Add(resource.MustParse("128Mi"))
	if total.Cmp(*want) != 0 {
		t.Errorf("pod total memory request = %s, want %s (pages once + sidecar floors, NOT (1+N)×pages)",
			total.String(), want.String())
	}

	// And prove the over-fence bug is gone: total must be strictly LESS than the
	// (1+N)×pagesBytes an every-container fence would have produced.
	overFence := resource.NewQuantity(3*pagesBytes, resource.BinarySI)
	if total.Cmp(*overFence) >= 0 {
		t.Errorf("pod total memory request %s >= (1+N)×pages %s — the multi-sidecar over-fence regressed",
			total.String(), overFence.String())
	}
}

// TestShapePlaceholderSpec_MemFenceHoldsPagesBytes: ka-0000000088 / fix B —
// a placeholder built from a snapshot with a real PagesBytes must set the
// sleeping app container's memory REQUEST to that byte count, so the
// scheduler fences the freed subject RAM against backfill until the same-node
// graft lands. CPU still floors to 100m — the request-lowering that made the
// warm-B pod admissible alongside the live agent is preserved.
func TestShapePlaceholderSpec_MemFenceHoldsPagesBytes(t *testing.T) {
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
		}},
	}
	const pagesBytes int64 = 3_221_225_472 // 3.0 GiB — canary #12 subject size
	spec := shapePlaceholderSpec(template, "node-a", pagesBytes, 2)
	agentC := spec.Containers[0]

	// CPU request still floors — the ka-0000008044 invariant is preserved.
	if got := agentC.Resources.Requests.Cpu(); got.Cmp(placeholderCPURequestFloor) != 0 {
		t.Errorf("CPU request = %s, want floor %s", got, placeholderCPURequestFloor.String())
	}
	// Memory request equals pagesBytes → placeholder fences the freed RAM.
	wantMem := resource.NewQuantity(pagesBytes, resource.BinarySI)
	if got := agentC.Resources.Requests.Memory(); got.Cmp(*wantMem) != 0 {
		t.Errorf("memory request = %s, want %s (pages fence)", got, wantMem.String())
	}
	// Limits preserved — the woken agent retains its 16Gi burst envelope.
	if got := agentC.Resources.Limits.Memory(); got.Cmp(resource.MustParse("16Gi")) != 0 {
		t.Errorf("memory limit = %s, want preserved 16Gi", got)
	}
}

// TestShapePlaceholderSpec_MemFenceCollapsesToFloor: PagesBytes=0 (legacy
// snapshot / capture failure) MUST fall back to the 256Mi floor exactly as
// pre-ka-0000000088 behavior — nothing regresses on snapshots that never
// captured the pages size.
func TestShapePlaceholderSpec_MemFenceCollapsesToFloor(t *testing.T) {
	template := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "agent",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		}},
	}
	spec := shapePlaceholderSpec(template, "node-a", 0, 2)
	if got := spec.Containers[0].Resources.Requests.Memory(); got.Cmp(placeholderMemRequestFloor) != 0 {
		t.Errorf("legacy PagesBytes=0 memory request = %s, want floor %s", got, placeholderMemRequestFloor.String())
	}
}

// TestShapePlaceholderSpec_MemFenceHonorsFloorForSmallPages: a snapshot with a
// (hypothetical) small pages.img must not DROP the request below the 256Mi
// floor. The floor stays the lower bound; pagesBytes only raises it.
func TestShapePlaceholderSpec_MemFenceHonorsFloorForSmallPages(t *testing.T) {
	template := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "agent",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		}},
	}
	// 100 MiB pages — below the 256Mi floor.
	spec := shapePlaceholderSpec(template, "node-a", 100*1024*1024, 2)
	if got := spec.Containers[0].Resources.Requests.Memory(); got.Cmp(placeholderMemRequestFloor) != 0 {
		t.Errorf("sub-floor pages memory request = %s, want floor %s", got, placeholderMemRequestFloor.String())
	}
}

// TestPlaceholderMemRequestFor: unit-level check of the request arithmetic
// (independent of shapePlaceholderSpec so a regression in the helper shows up
// with a small, fast test).
func TestPlaceholderMemRequestFor(t *testing.T) {
	cases := []struct {
		name       string
		pagesBytes int64
		want       resource.Quantity
	}{
		{"zero collapses to floor", 0, placeholderMemRequestFloor},
		{"negative collapses to floor", -1, placeholderMemRequestFloor},
		{"sub-floor collapses to floor", 100 * 1024 * 1024, placeholderMemRequestFloor},
		{"exactly floor", int64(placeholderMemRequestFloor.Value()), placeholderMemRequestFloor},
		{"3 GiB pages", 3 * 1024 * 1024 * 1024, *resource.NewQuantity(3*1024*1024*1024, resource.BinarySI)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := placeholderMemRequestFor(tc.pagesBytes)
			if got.Cmp(tc.want) != 0 {
				t.Errorf("placeholderMemRequestFor(%d) = %s, want %s", tc.pagesBytes, got.String(), tc.want.String())
			}
		})
	}
}
