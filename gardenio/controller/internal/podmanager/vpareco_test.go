package podmanager

import (
	"context"
	"log/slog"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
)

func fakeDynClient(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{
			verticalPodAutoscalerGVR: "VerticalPodAutoscalerList",
		},
		objects...,
	)
}

// vpaObject builds an unstructured VPA carrying a single container
// recommendation. An empty memTarget produces an object with no recommendation
// block, modeling a freshly-created VPA the recommender hasn't filled in yet.
func vpaObject(name, namespace, containerName, memTarget string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetUnstructuredContent(map[string]any{
		"apiVersion": "autoscaling.k8s.io/v1",
		"kind":       "VerticalPodAutoscaler",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
	})
	if memTarget != "" {
		_ = unstructured.SetNestedSlice(obj.Object, []any{
			map[string]any{
				"containerName": containerName,
				"target": map[string]any{
					"memory": memTarget,
				},
			},
		}, "status", "recommendation", "containerRecommendations")
	}
	return obj
}

func crewSpec() AgentPodSpec {
	return AgentPodSpec{
		Project:   "gardenio",
		Role:      "crew",
		Mode:      "crew",
		Namespace: "gardenio",
	}
}

func TestVPAStamper_Disabled_NoLookup(t *testing.T) {
	client := fakeDynClient(vpaObject(defaultVPAObjectName, "gardenio", ContainerName, "2Gi"))
	s := NewVPAMemoryStamper(client, VPAMemoryStamperConfig{Enabled: false}, testLogger())
	if s.Enabled() {
		t.Fatal("stamper should be disabled")
	}
	if _, ok := s.Recommend(context.Background(), crewSpec(), nil); ok {
		t.Fatal("disabled stamper must not return a recommendation")
	}
}

func TestVPAStamper_NilClient_Disabled(t *testing.T) {
	s := NewVPAMemoryStamper(nil, VPAMemoryStamperConfig{Enabled: true}, testLogger())
	if s.Enabled() {
		t.Fatal("nil client must yield a disabled stamper")
	}
}

func TestVPAStamper_RecommendationStampedAndClampedToFloor(t *testing.T) {
	// Recommendation (800Mi) below the 1500Mi floor must clamp UP to the floor —
	// never under-provision.
	client := fakeDynClient(vpaObject(defaultVPAObjectName, "gardenio", ContainerName, "800Mi"))
	s := NewVPAMemoryStamper(client, VPAMemoryStamperConfig{Enabled: true}, testLogger())

	got, ok := s.Recommend(context.Background(), crewSpec(), nil)
	if !ok {
		t.Fatal("expected a recommendation")
	}
	want := resource.MustParse(DefaultVPAMemoryFloor)
	if got.Cmp(want) != 0 {
		t.Fatalf("expected clamp to floor %s, got %s", want.String(), got.String())
	}
}

func TestVPAStamper_RecommendationPassthroughWithinBand(t *testing.T) {
	client := fakeDynClient(vpaObject(defaultVPAObjectName, "gardenio", ContainerName, "3Gi"))
	s := NewVPAMemoryStamper(client, VPAMemoryStamperConfig{Enabled: true}, testLogger())

	got, ok := s.Recommend(context.Background(), crewSpec(), nil)
	if !ok {
		t.Fatal("expected a recommendation")
	}
	if got.Cmp(resource.MustParse("3Gi")) != 0 {
		t.Fatalf("expected 3Gi passthrough, got %s", got.String())
	}
}

func TestVPAStamper_RecommendationClampedToMax(t *testing.T) {
	client := fakeDynClient(vpaObject(defaultVPAObjectName, "gardenio", ContainerName, "64Gi"))
	s := NewVPAMemoryStamper(client, VPAMemoryStamperConfig{Enabled: true, Max: "16Gi"}, testLogger())

	got, ok := s.Recommend(context.Background(), crewSpec(), nil)
	if !ok {
		t.Fatal("expected a recommendation")
	}
	if got.Cmp(resource.MustParse("16Gi")) != 0 {
		t.Fatalf("expected clamp to max 16Gi, got %s", got.String())
	}
}

func TestVPAStamper_ClampedToMemLimit(t *testing.T) {
	// A high (within-band) recommendation must never exceed the pod's mem limit.
	client := fakeDynClient(vpaObject(defaultVPAObjectName, "gardenio", ContainerName, "10Gi"))
	s := NewVPAMemoryStamper(client, VPAMemoryStamperConfig{Enabled: true, Max: "16Gi"}, testLogger())

	limit := resource.MustParse("4Gi")
	got, ok := s.Recommend(context.Background(), crewSpec(), &limit)
	if !ok {
		t.Fatal("expected a recommendation")
	}
	if got.Cmp(limit) != 0 {
		t.Fatalf("expected clamp to mem limit 4Gi, got %s", got.String())
	}
}

func TestVPAStamper_NoVPAObject_NoStamp(t *testing.T) {
	client := fakeDynClient() // no objects
	s := NewVPAMemoryStamper(client, VPAMemoryStamperConfig{Enabled: true}, testLogger())
	if _, ok := s.Recommend(context.Background(), crewSpec(), nil); ok {
		t.Fatal("missing VPA object must not produce a recommendation")
	}
}

func TestVPAStamper_EmptyRecommendation_NoStamp(t *testing.T) {
	// VPA exists but recommender hasn't produced a recommendation yet.
	client := fakeDynClient(vpaObject(defaultVPAObjectName, "gardenio", ContainerName, ""))
	s := NewVPAMemoryStamper(client, VPAMemoryStamperConfig{Enabled: true}, testLogger())
	if _, ok := s.Recommend(context.Background(), crewSpec(), nil); ok {
		t.Fatal("empty recommendation must not produce a stamp")
	}
}

func TestVPAStamper_WrongContainer_NoStamp(t *testing.T) {
	client := fakeDynClient(vpaObject(defaultVPAObjectName, "gardenio", "sidecar", "2Gi"))
	s := NewVPAMemoryStamper(client, VPAMemoryStamperConfig{Enabled: true}, testLogger())
	if _, ok := s.Recommend(context.Background(), crewSpec(), nil); ok {
		t.Fatal("recommendation for a non-agent container must not match")
	}
}

func TestVPAStamper_MissingClassKey_NoStamp(t *testing.T) {
	client := fakeDynClient(vpaObject(defaultVPAObjectName, "gardenio", ContainerName, "2Gi"))
	s := NewVPAMemoryStamper(client, VPAMemoryStamperConfig{Enabled: true}, testLogger())
	spec := crewSpec()
	spec.Role = "" // no class key
	if _, ok := s.Recommend(context.Background(), spec, nil); ok {
		t.Fatal("missing role must not match a per-class recommendation")
	}
}

func TestVPAStamper_ObjectNameTemplate(t *testing.T) {
	// Per-class object naming: template resolves to gardenio-gardenio-crew.
	client := fakeDynClient(vpaObject("gardenio-gardenio-crew", "gardenio", ContainerName, "2Gi"))
	s := NewVPAMemoryStamper(client, VPAMemoryStamperConfig{
		Enabled:            true,
		ObjectNameTemplate: "gardenio-{project}-{role}",
	}, testLogger())

	got, ok := s.Recommend(context.Background(), crewSpec(), nil)
	if !ok {
		t.Fatal("expected a recommendation via templated object name")
	}
	if got.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Fatalf("expected 2Gi, got %s", got.String())
	}
}

func TestVPAStamper_InvalidQuantityConfig_FallsBackToDefaults(t *testing.T) {
	s := NewVPAMemoryStamper(fakeDynClient(), VPAMemoryStamperConfig{
		Enabled: true,
		Floor:   "not-a-quantity",
		Max:     "",
	}, testLogger())
	if s.floor.Cmp(resource.MustParse(DefaultVPAMemoryFloor)) != 0 {
		t.Fatalf("invalid floor should fall back to default, got %s", s.floor.String())
	}
	if s.max.Cmp(resource.MustParse(DefaultVPAMemoryMax)) != 0 {
		t.Fatalf("empty max should fall back to default, got %s", s.max.String())
	}
}

func TestRoleFamily(t *testing.T) {
	cases := map[string]string{
		"":                  "",
		"crew":              "",
		"thread":            "",
		"fsm-triage":        "",
		"gardener":          "", // bare gardener has its own object
		"cost-gardener":     "gardener",
		"infra-gardener":    "gardener",
		"gardener-delivery": "gardener",
		"release-gardener":  "gardener",
	}
	for role, want := range cases {
		if got := roleFamily(role); got != want {
			t.Errorf("roleFamily(%q) = %q, want %q", role, got, want)
		}
	}
}

func TestVPAStamper_GardenerFamilyFallback(t *testing.T) {
	// A granular gardener role (cost-gardener) has NO dedicated VPA object, but the
	// bare gardener object exists → the stamper falls back to the gardener family
	// bucket and stamps its recommendation, instead of leaving the pod at 8Gi.
	client := fakeDynClient(vpaObject("gardenio-crew-default-gardener", "gardenio", ContainerName, "2Gi"))
	s := NewVPAMemoryStamper(client, VPAMemoryStamperConfig{
		Enabled:            true,
		ObjectNameTemplate: "gardenio-crew-default-{role}",
	}, testLogger())

	spec := crewSpec()
	spec.Role = "cost-gardener"
	got, ok := s.Recommend(context.Background(), spec, nil)
	if !ok {
		t.Fatal("expected gardener-family fallback to produce a recommendation")
	}
	if got.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Fatalf("expected 2Gi from gardener family bucket, got %s", got.String())
	}
}

func TestVPAStamper_GardenerFamilyFallback_NoFamilyObject_NoStamp(t *testing.T) {
	// Neither the exact role object NOR the gardener family object exists → no
	// stamp (pod keeps its safe default; never under-provisions).
	client := fakeDynClient() // no objects
	s := NewVPAMemoryStamper(client, VPAMemoryStamperConfig{
		Enabled:            true,
		ObjectNameTemplate: "gardenio-crew-default-{role}",
	}, testLogger())

	spec := crewSpec()
	spec.Role = "infra-gardener"
	if _, ok := s.Recommend(context.Background(), spec, nil); ok {
		t.Fatal("missing exact and family object must not produce a stamp")
	}
}

func TestVPAStamper_NonGardenerRole_NoFamilyFallback(t *testing.T) {
	// A non-gardener granular role whose exact object is missing must NOT fall
	// back to the gardener bucket — only gardener-family roles do.
	client := fakeDynClient(vpaObject("gardenio-crew-default-gardener", "gardenio", ContainerName, "2Gi"))
	s := NewVPAMemoryStamper(client, VPAMemoryStamperConfig{
		Enabled:            true,
		ObjectNameTemplate: "gardenio-crew-default-{role}",
	}, testLogger())

	spec := crewSpec()
	spec.Role = "some-other-role"
	if _, ok := s.Recommend(context.Background(), spec, nil); ok {
		t.Fatal("non-gardener role must not fall back to the gardener bucket")
	}
}

func TestVPAStamper_BareGardener_NoFallbackNeeded(t *testing.T) {
	// Bare gardener matches its own object directly — no family retry.
	client := fakeDynClient(vpaObject("gardenio-crew-default-gardener", "gardenio", ContainerName, "2Gi"))
	s := NewVPAMemoryStamper(client, VPAMemoryStamperConfig{
		Enabled:            true,
		ObjectNameTemplate: "gardenio-crew-default-{role}",
	}, testLogger())

	spec := crewSpec()
	spec.Role = "gardener"
	got, ok := s.Recommend(context.Background(), spec, nil)
	if !ok {
		t.Fatal("expected bare gardener to match its own object")
	}
	if got.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Fatalf("expected 2Gi, got %s", got.String())
	}
}

func TestObjectName(t *testing.T) {
	s := &VPAMemoryStamper{}
	if got := s.objectName("gardenio", "crew"); got != defaultVPAObjectName {
		t.Fatalf("empty template should yield default, got %s", got)
	}
	s.objectNameTemplate = "vpa-{project}-{role}"
	if got := s.objectName("gardenio", "crew"); got != "vpa-gardenio-crew" {
		t.Fatalf("unexpected templated name: %s", got)
	}
}

// --- container resizePolicy + CreateAgentPod integration ---

// memReq returns the agent container's memory request from a built pod.
func agentMemRequest(t *testing.T, pod *corev1.Pod) resource.Quantity {
	t.Helper()
	for _, c := range pod.Spec.Containers {
		if c.Name == ContainerName {
			return c.Resources.Requests[corev1.ResourceMemory]
		}
	}
	t.Fatalf("no %q container in pod", ContainerName)
	return resource.Quantity{}
}

func TestBuildContainer_HasMemoryResizePolicy(t *testing.T) {
	m := newTestManager()
	c := m.buildContainer(minimalSpec())
	var found bool
	for _, rp := range c.ResizePolicy {
		if rp.ResourceName == corev1.ResourceMemory {
			found = true
			if rp.RestartPolicy != corev1.NotRequired {
				t.Fatalf("memory resize policy = %q, want NotRequired", rp.RestartPolicy)
			}
		}
	}
	if !found {
		t.Fatal("agent container missing a memory resizePolicy")
	}
}

func TestCreateAgentPod_VPAStampDisabled_KeepsDefaultRequest(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, slog.Default())
	// No stamper set → request stays at the 8Gi default.
	spec := minimalSpec()
	spec.Namespace = "default"

	if err := m.CreateAgentPod(context.Background(), spec); err != nil {
		t.Fatalf("CreateAgentPod: %v", err)
	}
	pod, err := client.CoreV1().Pods("default").Get(context.Background(), spec.PodName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	got := agentMemRequest(t, pod)
	if got.Cmp(resource.MustParse(DefaultMemoryRequest)) != 0 {
		t.Fatalf("memory request = %s, want default %s", got.String(), DefaultMemoryRequest)
	}
}

func TestCreateAgentPod_VPAStampApplied(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, slog.Default())
	m.VPAStamper = NewVPAMemoryStamper(
		fakeDynClient(vpaObject(defaultVPAObjectName, "default", ContainerName, "2Gi")),
		VPAMemoryStamperConfig{Enabled: true}, testLogger(),
	)
	spec := minimalSpec()
	spec.Namespace = "default"
	spec.Role = "crew"

	if err := m.CreateAgentPod(context.Background(), spec); err != nil {
		t.Fatalf("CreateAgentPod: %v", err)
	}
	pod, err := client.CoreV1().Pods("default").Get(context.Background(), spec.PodName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	got := agentMemRequest(t, pod)
	if got.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Fatalf("memory request = %s, want stamped 2Gi", got.String())
	}
	// The default memory LIMIT must be preserved (untouched by the stamp).
	var limit resource.Quantity
	for _, c := range pod.Spec.Containers {
		if c.Name == ContainerName {
			limit = c.Resources.Limits[corev1.ResourceMemory]
		}
	}
	if limit.Cmp(resource.MustParse(DefaultMemoryLimit)) != 0 {
		t.Fatalf("memory limit = %s, want preserved %s", limit.String(), DefaultMemoryLimit)
	}
}

func TestCreateAgentPod_VPAStampNoRec_KeepsDefault(t *testing.T) {
	client := fake.NewClientset()
	m := New(client, slog.Default())
	// Stamper enabled but the VPA object carries no recommendation yet → the
	// default request must survive (no under-provisioning).
	m.VPAStamper = NewVPAMemoryStamper(
		fakeDynClient(vpaObject(defaultVPAObjectName, "default", ContainerName, "")),
		VPAMemoryStamperConfig{Enabled: true}, testLogger(),
	)
	spec := minimalSpec()
	spec.Namespace = "default"
	spec.Role = "crew"

	if err := m.CreateAgentPod(context.Background(), spec); err != nil {
		t.Fatalf("CreateAgentPod: %v", err)
	}
	pod, err := client.CoreV1().Pods("default").Get(context.Background(), spec.PodName(), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get pod: %v", err)
	}
	got := agentMemRequest(t, pod)
	if got.Cmp(resource.MustParse(DefaultMemoryRequest)) != 0 {
		t.Fatalf("memory request = %s, want default %s (no rec yet)", got.String(), DefaultMemoryRequest)
	}
}
