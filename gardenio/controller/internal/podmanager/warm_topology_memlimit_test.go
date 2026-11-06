package podmanager

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

// canary15 constants pin the EXACT shape bug ka-0000000413 (gVisor canary #15,
// 2026-07-04) surfaced: a 2.96GiB pages.img whose ROOT-sandbox restore peaked
// >8Gi = >2.71× the image and was cgroup-OOM-SIGKILLed against Pod B's INHERITED
// 8Gi role memory LIMIT — while the node had 14.4Gi free. These are the numbers
// the LIMIT fix (raise Pod B's cgroup memory.max) must cover.
const (
	canary15PagesBytes  int64 = 3_173_519_360 // 2.96 GiB pages.img
	canary15Containers  int   = 2             // dir-medium bare agent: pause + agent
	gvisorRoleMemLimit  int64 = 8 * 1024 * 1024 * 1024
	canary15ObservedOOM int64 = 8 * 1024 * 1024 * 1024 // root restore peak EXCEEDED this
)

// TestModeledRestorePeakLimit_CoversCanary15 proves the limit model sizes a cgroup
// ceiling that clears the observed canary #15 restore peak (>8Gi for a 2.96Gi
// pages.img, >2.71× the image) with headroom — the whole point of fix A.
func TestModeledRestorePeakLimit_CoversCanary15(t *testing.T) {
	got := modeledRestorePeakLimit(canary15PagesBytes, canary15Containers)

	// Documented model: pages×3.0 + 768Mi×2 + 512Mi.
	want := canary15PagesBytes*defaultRestorePeakLimitMultiplierPermille/1000 +
		defaultMemGateSentryOverhead*int64(canary15Containers) + defaultMemGateMargin
	if got != want {
		t.Errorf("modeledRestorePeakLimit(canary15) = %d, want %d", got, want)
	}
	// It must EXCEED the observed OOM ceiling (>8Gi) — otherwise the raised limit
	// would still cgroup-OOM the exact restore this fix exists to survive.
	if got <= canary15ObservedOOM {
		t.Errorf("modeled peak limit %d must exceed the observed >8Gi OOM ceiling %d", got, canary15ObservedOOM)
	}
	// And it must be fatter than the inherited 8Gi role limit (the binding cgroup
	// that OOMed on the create path).
	if got <= gvisorRoleMemLimit {
		t.Errorf("modeled peak limit %d must exceed the inherited role limit %d it replaces", got, gvisorRoleMemLimit)
	}
}

// TestRestorePeakLimit_ExceedsModeledNeed is the load-bearing INVARIANT for fix B:
// the limit model (fix A) is always >= the node-gate need model, so a Pod B minted
// with the raised limit always clears the pre-thaw mem-gate's own modeled need —
// fix A structurally satisfies fix B. If this ever flips, a create-path Pod B
// could be minted with a limit BELOW what the gate demands and the belt-and-braces
// create-path gate term would (correctly, but surprisingly) refuse it.
func TestRestorePeakLimit_ExceedsModeledNeed(t *testing.T) {
	var m FreezeThawManager // zero value → gate need uses production defaults
	for _, pages := range []int64{
		1, 256 * 1024 * 1024, 1_000_000_000, canary15PagesBytes, 3_221_225_472, 8_000_000_000,
	} {
		for _, cc := range []int{0, 1, 2, 3, 4, 8} {
			limit := modeledRestorePeakLimit(pages, cc)
			need := m.modeledRestoreNeed(pages, cc)
			if limit < need {
				t.Errorf("modeledRestorePeakLimit(%d,%d)=%d < modeledRestoreNeed=%d — fix A no longer satisfies fix B",
					pages, cc, limit, need)
			}
		}
	}
}

// TestModeledRestorePeakLimit_IndeterminatePages: pages<=0 (legacy snapshot /
// capture failure) yields 0 so callers impose NO limit off an unknown size.
func TestModeledRestorePeakLimit_IndeterminatePages(t *testing.T) {
	for _, pages := range []int64{0, -1, -1 << 40} {
		if got := modeledRestorePeakLimit(pages, 2); got != 0 {
			t.Errorf("modeledRestorePeakLimit(%d,2) = %d, want 0 (indeterminate)", pages, got)
		}
	}
}

// TestShapePlaceholderSpec_RaisesInheritedRoleLimit is the CREATE-PATH fix: a
// template carrying the gvisor role's 8Gi memory LIMIT (what buildPlaceholderPodB
// DeepCopies and, pre-fix, floored-requests-but-kept-limits) must have its app
// container LIMIT RAISED to the restore-peak model so the root restore does not
// cgroup-OOM against the inherited 8Gi (bug ka-0000000413). The REQUEST stays the
// pages fence (admission-safe), NOT the fat limit.
func TestShapePlaceholderSpec_RaisesInheritedRoleLimit(t *testing.T) {
	template := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "agent",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("2"),
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
				Limits: corev1.ResourceList{
					// The gvisor-poc ROLE limit the create path inherits (ka-0000003266).
					corev1.ResourceMemory: resource.MustParse("8Gi"),
				},
			},
		}},
	}
	spec := shapePlaceholderSpec(template, "node-a", canary15PagesBytes, canary15Containers)
	agentC := spec.Containers[0]

	wantLimit := modeledRestorePeakLimit(canary15PagesBytes, canary15Containers)
	gotLimit := agentC.Resources.Limits.Memory()
	if gotLimit.Value() != wantLimit {
		t.Errorf("app container memory LIMIT = %d, want raised model %d (was inherited 8Gi)", gotLimit.Value(), wantLimit)
	}
	// The raised limit must clear the restore peak the gate demands.
	if gotLimit.Value() < canary15ObservedOOM {
		t.Errorf("raised limit %d does not cover the observed >8Gi restore peak", gotLimit.Value())
	}
	// REQUEST stays the pages fence — NOT the fat limit — so the node-pinned
	// placeholder still admits alongside the live agent (ka-0000008044 invariant).
	wantReq := resource.NewQuantity(canary15PagesBytes, resource.BinarySI)
	if got := agentC.Resources.Requests.Memory(); got.Cmp(*wantReq) != 0 {
		t.Errorf("app container memory REQUEST = %s, want pages fence %s (must not follow the raised limit)", got, wantReq)
	}
	if got := agentC.Resources.Requests.Cpu(); got.Cmp(placeholderCPURequestFloor) != 0 {
		t.Errorf("CPU request = %s, want floor %s", got, placeholderCPURequestFloor.String())
	}
	// The template must NOT be mutated (deep-copied first).
	if got := template.Containers[0].Resources.Limits.Memory(); got.Cmp(resource.MustParse("8Gi")) != 0 {
		t.Errorf("template limit was mutated: got %s, want original 8Gi", got)
	}
}

// TestShapePlaceholderSpec_NeverShrinksFatLimit: a template whose limit already
// EXCEEDS the model (a 16Gi burst envelope) keeps it — raise-only, never shrink.
func TestShapePlaceholderSpec_NeverShrinksFatLimit(t *testing.T) {
	template := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "agent",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("16Gi")},
			},
		}},
	}
	// 16Gi > modeledRestorePeakLimit(3G,2) (~10.9Gi), so the limit is preserved.
	spec := shapePlaceholderSpec(template, "node-a", canary15PagesBytes, canary15Containers)
	if got := spec.Containers[0].Resources.Limits.Memory(); got.Cmp(resource.MustParse("16Gi")) != 0 {
		t.Errorf("fat limit = %s, want preserved 16Gi (raise-only must not shrink)", got)
	}
}

// TestShapePlaceholderSpec_NeverImposesLimitWhenUnbounded: a template with NO
// memory limit stays unbounded — an unbounded container has no cgroup memory.max
// ceiling to OOM against (only the node can OOM it, which the node mem-gate
// covers), so imposing one would only ADD a constraint.
func TestShapePlaceholderSpec_NeverImposesLimitWhenUnbounded(t *testing.T) {
	template := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "agent",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
				// No Limits at all.
			},
		}},
	}
	spec := shapePlaceholderSpec(template, "node-a", canary15PagesBytes, canary15Containers)
	if _, bounded := spec.Containers[0].Resources.Limits[corev1.ResourceMemory]; bounded {
		t.Errorf("unbounded template must stay unbounded, got limit %s", spec.Containers[0].Resources.Limits.Memory())
	}
}

// TestShapePlaceholderSpec_LegacyPagesZeroKeepsLimit: pages<=0 (legacy snapshot)
// is a no-op for the limit raise — the inherited limit is preserved exactly.
func TestShapePlaceholderSpec_LegacyPagesZeroKeepsLimit(t *testing.T) {
	template := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name: "agent",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
				Limits:   corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("8Gi")},
			},
		}},
	}
	spec := shapePlaceholderSpec(template, "node-a", 0, canary15Containers)
	if got := spec.Containers[0].Resources.Limits.Memory(); got.Cmp(resource.MustParse("8Gi")) != 0 {
		t.Errorf("legacy pages=0 limit = %s, want preserved 8Gi (raise is a no-op)", got)
	}
}

// TestDefaultWarmPlaceholderResources_LimitCoversRestorePeak is the LEGACY WARM-B
// POOL fix: BuildWarmPlaceholderPod's default 512Mi limit (bug ka-0000000413:
// "A 3.0G restore in a 512Mi cgroup OOMs harder") must now be sized to the
// restore-peak model at the nominal ~3G checkpoint, while the REQUEST stays at the
// small 256Mi floor (limits do not reserve node capacity).
func TestDefaultWarmPlaceholderResources_LimitCoversRestorePeak(t *testing.T) {
	res := defaultWarmPlaceholderResources()

	wantLimit := modeledRestorePeakLimit(warmPlaceholderNominalPagesBytes, freezeBaselineContainerCount)
	gotLimit := res.Limits.Memory()
	if gotLimit.Value() != wantLimit {
		t.Errorf("warm placeholder default memory LIMIT = %d, want restore-peak model %d", gotLimit.Value(), wantLimit)
	}
	// Regression guard: the old flat 512Mi limit must be gone.
	if gotLimit.Cmp(resource.MustParse("512Mi")) <= 0 {
		t.Errorf("warm placeholder limit %s must exceed the old 512Mi that cgroup-OOMed a 3G restore", gotLimit)
	}
	// It must cover a nominal ~3G restore peak (fits the fleet-representative case).
	if gotLimit.Value() < 3*1024*1024*1024 {
		t.Errorf("warm placeholder limit %d must cover a nominal 3G restore", gotLimit.Value())
	}
	// REQUEST unchanged at the 256Mi floor — a fat limit costs nothing standing.
	if got := res.Requests.Memory(); got.Cmp(resource.MustParse("256Mi")) != 0 {
		t.Errorf("warm placeholder memory REQUEST = %s, want unchanged 256Mi floor", got)
	}
	if got := res.Requests.Cpu(); got.Cmp(resource.MustParse("100m")) != 0 {
		t.Errorf("warm placeholder CPU REQUEST = %s, want 100m", got)
	}
}

// TestBuildWarmPlaceholderPod_LimitCoversRestorePeak: the end-to-end bare builder
// stamps the restore-peak-covering limit onto the pod (not just the resource
// helper).
func TestBuildWarmPlaceholderPod_LimitCoversRestorePeak(t *testing.T) {
	pod := BuildWarmPlaceholderPod(WarmPlaceholderSpec{
		Name: "warm-b-node-a-1", Namespace: "gvisor", Node: "node-a", Image: "agent:latest",
	})
	lim := pod.Spec.Containers[0].Resources.Limits.Memory()
	if lim.Cmp(resource.MustParse("512Mi")) <= 0 {
		t.Errorf("bare warm placeholder limit %s must exceed the old 512Mi", lim)
	}
	if lim.Value() < 3*1024*1024*1024 {
		t.Errorf("bare warm placeholder limit %d must cover a nominal 3G restore", lim.Value())
	}
}

// TestBindingContainerMemLimit exercises the tightest-bounded-limit reader the
// warm-B graft gate uses: MIN across app containers, unbounded contributes no
// ceiling, none-bounded returns (0,false).
func TestBindingContainerMemLimit(t *testing.T) {
	q := func(s string) corev1.ResourceList {
		return corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(s)}
	}
	t.Run("single bounded", func(t *testing.T) {
		spec := corev1.PodSpec{Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{Limits: q("512Mi")}}}}
		got, ok := bindingContainerMemLimit(spec)
		if !ok || got != 512*1024*1024 {
			t.Errorf("got (%d,%v), want (536870912,true)", got, ok)
		}
	})
	t.Run("min of several", func(t *testing.T) {
		spec := corev1.PodSpec{Containers: []corev1.Container{
			{Resources: corev1.ResourceRequirements{Limits: q("8Gi")}},
			{Resources: corev1.ResourceRequirements{Limits: q("512Mi")}},
			{Resources: corev1.ResourceRequirements{Limits: q("16Gi")}},
		}}
		got, ok := bindingContainerMemLimit(spec)
		if !ok || got != 512*1024*1024 {
			t.Errorf("got (%d,%v), want the 512Mi tightest limit", got, ok)
		}
	})
	t.Run("unbounded contributes no ceiling", func(t *testing.T) {
		// A limitless container next to a bounded one must NOT mask the bounded
		// one — the OOM fires against the bounded container's own scope cgroup.
		spec := corev1.PodSpec{Containers: []corev1.Container{
			{Resources: corev1.ResourceRequirements{}}, // unbounded
			{Resources: corev1.ResourceRequirements{Limits: q("512Mi")}},
		}}
		got, ok := bindingContainerMemLimit(spec)
		if !ok || got != 512*1024*1024 {
			t.Errorf("got (%d,%v), want the 512Mi bounded limit despite the unbounded sibling", got, ok)
		}
	})
	t.Run("none bounded", func(t *testing.T) {
		spec := corev1.PodSpec{Containers: []corev1.Container{{Resources: corev1.ResourceRequirements{}}}}
		if got, ok := bindingContainerMemLimit(spec); ok {
			t.Errorf("got (%d,true), want (_,false) when no container is bounded", got)
		}
	})
}

// TestPeakOverPagesPermille pins the fix-C ratio helper.
func TestPeakOverPagesPermille(t *testing.T) {
	if got := peakOverPagesPermille(8_000_000_000, 3_000_000_000); got != 2666 {
		t.Errorf("peakOverPagesPermille(8e9,3e9) = %d, want 2666", got)
	}
	for _, tc := range []struct{ peak, pages int64 }{{0, 3e9}, {8e9, 0}, {-1, 3e9}} {
		if got := peakOverPagesPermille(tc.peak, tc.pages); got != 0 {
			t.Errorf("peakOverPagesPermille(%d,%d) = %d, want 0", tc.peak, tc.pages, got)
		}
	}
}
