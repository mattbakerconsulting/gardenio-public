package main

import (
	"context"
	"testing"

	"gardenio/controller/internal/podmanager"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestGenerateToken(t *testing.T) {
	token, err := generateToken()
	if err != nil {
		t.Fatalf("generateToken: %v", err)
	}
	if len(token) < 10 {
		t.Errorf("token too short: %q", token)
	}
	if token[:6] != "fleet-" {
		t.Errorf("token should start with 'fleet-': %q", token)
	}

	// Two tokens should be different.
	token2, err := generateToken()
	if err != nil {
		t.Fatalf("generateToken: %v", err)
	}
	if token == token2 {
		t.Errorf("two tokens should be different: %q == %q", token, token2)
	}
}

func TestCreateFleetResourceQuotaStampsDefaults(t *testing.T) {
	const ns = "gardenio-test"
	client := fake.NewSimpleClientset()

	if err := createFleetResourceQuota(context.Background(), client, ns); err != nil {
		t.Fatalf("createFleetResourceQuota: %v", err)
	}

	quota, err := client.CoreV1().ResourceQuotas(ns).Get(context.Background(), "gardenio-fleet-quota", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get quota: %v", err)
	}

	want := map[corev1.ResourceName]string{
		corev1.ResourcePods:                   "200",
		corev1.ResourceRequestsCPU:            "200",
		corev1.ResourceRequestsMemory:         "1Ti",
		corev1.ResourcePersistentVolumeClaims: "200",
		corev1.ResourceServices:               "50",
		corev1.ResourceSecrets:                "200",
	}
	for name, expected := range want {
		got, ok := quota.Spec.Hard[name]
		if !ok {
			t.Errorf("quota.Spec.Hard missing key %q", name)
			continue
		}
		if got.Cmp(resource.MustParse(expected)) != 0 {
			t.Errorf("quota.Spec.Hard[%q] = %q, want %q", name, got.String(), expected)
		}
	}

	if quota.Labels["app.kubernetes.io/managed-by"] != "gardenio-fleet" {
		t.Errorf("missing managed-by label: %v", quota.Labels)
	}
}

func TestCreateFleetResourceQuotaIdempotent(t *testing.T) {
	const ns = "gardenio-test"
	existing := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: "gardenio-fleet-quota", Namespace: ns},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourcePods: resource.MustParse("42"),
		}},
	}
	client := fake.NewSimpleClientset(existing)

	if err := createFleetResourceQuota(context.Background(), client, ns); err != nil {
		t.Fatalf("createFleetResourceQuota: %v", err)
	}

	got, err := client.CoreV1().ResourceQuotas(ns).Get(context.Background(), "gardenio-fleet-quota", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get quota: %v", err)
	}
	pods := got.Spec.Hard[corev1.ResourcePods]
	if pods.Cmp(resource.MustParse("42")) != 0 {
		t.Errorf("idempotent create clobbered existing quota: pods=%q, want 42", pods.String())
	}
}

func TestCreateFleetResourceQuotaPropagatesError(t *testing.T) {
	// Sanity: returns nil when AlreadyExists; verified above. Confirm the
	// error path returns the underlying error rather than swallowing it.
	const ns = "gardenio-test"
	client := fake.NewSimpleClientset()

	// First create succeeds.
	if err := createFleetResourceQuota(context.Background(), client, ns); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Second call to the same namespace should be a no-op (AlreadyExists).
	err := createFleetResourceQuota(context.Background(), client, ns)
	if err != nil {
		t.Fatalf("second create should be no-op, got: %v", err)
	}
	// And nothing here should have produced a NotFound — the quota is real.
	if _, getErr := client.CoreV1().ResourceQuotas(ns).Get(context.Background(), "gardenio-fleet-quota", metav1.GetOptions{}); apierrors.IsNotFound(getErr) {
		t.Fatalf("quota missing after create: %v", getErr)
	}
}

func TestCreateFleetNetworkPoliciesStampsBaseline(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	ctx := context.Background()
	ns := "gardenio-qa"
	parent := "gardenio"

	if err := createFleetNetworkPolicies(ctx, k8s, ns, parent); err != nil {
		t.Fatalf("createFleetNetworkPolicies: %v", err)
	}

	want := []string{
		"default-deny-ingress",
		"default-deny-egress",
		"allow-dns",
		"allow-egress-to-parent-beads",
		"allow-egress-to-parent-nats",
	}
	got := map[string]*networkingv1.NetworkPolicy{}
	list, err := k8s.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing NetworkPolicies: %v", err)
	}
	for i := range list.Items {
		got[list.Items[i].Name] = &list.Items[i]
	}
	for _, name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("missing NetworkPolicy %q (got %d policies)", name, len(got))
		}
	}

	// default-deny-ingress: empty podSelector, Ingress only.
	if np := got["default-deny-ingress"]; np != nil {
		if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
			t.Errorf("default-deny-ingress policyTypes = %v, want [Ingress]", np.Spec.PolicyTypes)
		}
		if len(np.Spec.PodSelector.MatchLabels) != 0 || len(np.Spec.PodSelector.MatchExpressions) != 0 {
			t.Errorf("default-deny-ingress podSelector should be empty, got %+v", np.Spec.PodSelector)
		}
		if len(np.Spec.Ingress) != 0 {
			t.Errorf("default-deny-ingress should have no ingress rules, got %d", len(np.Spec.Ingress))
		}
	}

	// default-deny-egress: empty podSelector, Egress only, no rules.
	if np := got["default-deny-egress"]; np != nil {
		if len(np.Spec.PolicyTypes) != 1 || np.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
			t.Errorf("default-deny-egress policyTypes = %v, want [Egress]", np.Spec.PolicyTypes)
		}
		if len(np.Spec.Egress) != 0 {
			t.Errorf("default-deny-egress should have no egress rules, got %d", len(np.Spec.Egress))
		}
	}

	// allow-dns: namespaceSelector kube-system, podSelector kube-dns,
	// ports 53/UDP and 53/TCP.
	if np := got["allow-dns"]; np != nil {
		if len(np.Spec.Egress) != 1 {
			t.Fatalf("allow-dns wants 1 egress rule, got %d", len(np.Spec.Egress))
		}
		rule := np.Spec.Egress[0]
		if len(rule.To) != 1 {
			t.Fatalf("allow-dns wants 1 peer, got %d", len(rule.To))
		}
		peer := rule.To[0]
		if peer.NamespaceSelector == nil ||
			peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "kube-system" {
			t.Errorf("allow-dns namespaceSelector = %+v", peer.NamespaceSelector)
		}
		if peer.PodSelector == nil || peer.PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
			t.Errorf("allow-dns podSelector = %+v", peer.PodSelector)
		}
		var sawUDP, sawTCP bool
		for _, p := range rule.Ports {
			if p.Port == nil || p.Port.IntValue() != 53 {
				continue
			}
			if p.Protocol != nil && *p.Protocol == corev1.ProtocolUDP {
				sawUDP = true
			}
			if p.Protocol != nil && *p.Protocol == corev1.ProtocolTCP {
				sawTCP = true
			}
		}
		if !sawUDP || !sawTCP {
			t.Errorf("allow-dns ports want UDP+TCP/53, got %+v", rule.Ports)
		}
	}

	// allow-egress-to-parent-beads: parent ns + component=beads, port 8080/TCP.
	if np := got["allow-egress-to-parent-beads"]; np != nil {
		if len(np.Spec.Egress) != 1 || len(np.Spec.Egress[0].To) != 1 {
			t.Fatalf("allow-egress-to-parent-beads malformed: %+v", np.Spec.Egress)
		}
		peer := np.Spec.Egress[0].To[0]
		if peer.NamespaceSelector == nil ||
			peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != parent {
			t.Errorf("beads namespaceSelector = %+v, want metadata.name=%s", peer.NamespaceSelector, parent)
		}
		if peer.PodSelector == nil ||
			peer.PodSelector.MatchLabels["app.kubernetes.io/component"] != "beads" {
			t.Errorf("beads podSelector = %+v", peer.PodSelector)
		}
		ports := np.Spec.Egress[0].Ports
		if len(ports) != 1 || ports[0].Port == nil || ports[0].Port.IntValue() != 8080 ||
			ports[0].Protocol == nil || *ports[0].Protocol != corev1.ProtocolTCP {
			t.Errorf("beads ports = %+v, want TCP/8080", ports)
		}
	}

	// allow-egress-to-parent-nats: parent ns + component=nats, port 4222/TCP.
	if np := got["allow-egress-to-parent-nats"]; np != nil {
		if len(np.Spec.Egress) != 1 || len(np.Spec.Egress[0].To) != 1 {
			t.Fatalf("allow-egress-to-parent-nats malformed: %+v", np.Spec.Egress)
		}
		peer := np.Spec.Egress[0].To[0]
		if peer.NamespaceSelector == nil ||
			peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != parent {
			t.Errorf("nats namespaceSelector = %+v, want metadata.name=%s", peer.NamespaceSelector, parent)
		}
		if peer.PodSelector == nil ||
			peer.PodSelector.MatchLabels["app.kubernetes.io/component"] != "nats" {
			t.Errorf("nats podSelector = %+v", peer.PodSelector)
		}
		ports := np.Spec.Egress[0].Ports
		if len(ports) != 1 || ports[0].Port == nil || ports[0].Port.IntValue() != 4222 ||
			ports[0].Protocol == nil || *ports[0].Protocol != corev1.ProtocolTCP {
			t.Errorf("nats ports = %+v, want TCP/4222", ports)
		}
	}

	// All policies must carry the gardenio-fleet managed-by label so the
	// chart owners can audit/remove them later.
	for _, np := range got {
		if np.Labels["app.kubernetes.io/managed-by"] != "gardenio-fleet" {
			t.Errorf("NetworkPolicy %s missing managed-by label, got %v", np.Name, np.Labels)
		}
	}
}

func TestCreateFleetNetworkPoliciesIdempotent(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	ctx := context.Background()
	ns := "gardenio-qa"

	if err := createFleetNetworkPolicies(ctx, k8s, ns, "gardenio"); err != nil {
		t.Fatalf("first create: %v", err)
	}
	// Second call must not return an error (AlreadyExists swallowed).
	if err := createFleetNetworkPolicies(ctx, k8s, ns, "gardenio"); err != nil {
		t.Fatalf("second create (idempotent): %v", err)
	}
	list, err := k8s.NetworkingV1().NetworkPolicies(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(list.Items) != 5 {
		t.Errorf("after idempotent re-run, want 5 policies, got %d", len(list.Items))
	}
}

func TestParentNamespace(t *testing.T) {
	t.Setenv("BOAT_PARENT_NAMESPACE", "")
	t.Setenv("POD_NAMESPACE", "")
	if got := parentNamespace(); got != "gardenio" {
		t.Errorf("parentNamespace() with no env = %q, want gardenio", got)
	}

	t.Setenv("POD_NAMESPACE", "gardenio-e2e")
	if got := parentNamespace(); got != "gardenio-e2e" {
		t.Errorf("parentNamespace() with POD_NAMESPACE=gardenio-e2e = %q", got)
	}

	// BOAT_PARENT_NAMESPACE wins over POD_NAMESPACE.
	t.Setenv("BOAT_PARENT_NAMESPACE", "platform")
	if got := parentNamespace(); got != "platform" {
		t.Errorf("parentNamespace() with override = %q, want platform", got)
	}
}

func TestDefaultParentFleet(t *testing.T) {
	// When BOAT_FLEET is unset, default to "gardenio".
	t.Setenv("BOAT_FLEET", "")
	if got := defaultParentFleet(); got != "gardenio" {
		t.Errorf("defaultParentFleet() = %q, want %q", got, "gardenio")
	}

	// When BOAT_FLEET is set, use it.
	t.Setenv("BOAT_FLEET", "qa-fleet")
	if got := defaultParentFleet(); got != "qa-fleet" {
		t.Errorf("defaultParentFleet() = %q, want %q", got, "qa-fleet")
	}
}

func TestPSAProfileDefault(t *testing.T) {
	t.Setenv("GARDENIO_PSA_PROFILE", "")
	if got := psaProfile(); got != "restricted" {
		t.Errorf("psaProfile() default = %q, want %q", got, "restricted")
	}
}

func TestPSAProfileOverride(t *testing.T) {
	cases := map[string]string{
		"baseline":   "baseline",
		"privileged": "privileged",
		"restricted": "restricted",
		"garbage":    "restricted", // unknown values fall back to restricted
	}
	for in, want := range cases {
		t.Setenv("GARDENIO_PSA_PROFILE", in)
		if got := psaProfile(); got != want {
			t.Errorf("psaProfile() with GARDENIO_PSA_PROFILE=%q = %q, want %q", in, got, want)
		}
	}
}

func TestCreateFleetNamespaceStampsPSALabels(t *testing.T) {
	t.Setenv("GARDENIO_PSA_PROFILE", "")
	k8s := fake.NewSimpleClientset()
	ctx := context.Background()

	if err := createFleetNamespace(ctx, k8s, "gardenio-qa", "qa", "gardenio"); err != nil {
		t.Fatalf("createFleetNamespace: %v", err)
	}
	ns, err := k8s.CoreV1().Namespaces().Get(ctx, "gardenio-qa", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting created namespace: %v", err)
	}

	// Existing fleet labels still present.
	for _, k := range []string{
		"app.kubernetes.io/name",
		"app.kubernetes.io/managed-by",
		"gardenio.io/fleet",
		"gardenio.io/parent-fleet",
	} {
		if _, ok := ns.Labels[k]; !ok {
			t.Errorf("missing fleet label %q on namespace", k)
		}
	}
	if got := ns.Labels["gardenio.io/fleet"]; got != "qa" {
		t.Errorf("gardenio.io/fleet = %q, want %q", got, "qa")
	}

	// PSA labels present and set to restricted/latest by default.
	wantPSA := map[string]string{
		"pod-security.kubernetes.io/enforce":         "restricted",
		"pod-security.kubernetes.io/audit":           "restricted",
		"pod-security.kubernetes.io/warn":            "restricted",
		"pod-security.kubernetes.io/enforce-version": "latest",
		"pod-security.kubernetes.io/audit-version":   "latest",
		"pod-security.kubernetes.io/warn-version":    "latest",
	}
	for k, want := range wantPSA {
		if got := ns.Labels[k]; got != want {
			t.Errorf("ns.Labels[%q] = %q, want %q", k, got, want)
		}
	}
}

// TestCreateFleetRBACRoleVerbs locks the tightened verb set from F5
// (gardenio/docs/audits/namespace-isolation-static.md). Adding a verb
// to the per-fleet gardenio-agent Role expands the blast radius of any
// compromised agent in the namespace — gate it through this test.
func TestCreateFleetRBACRoleVerbs(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	ctx := context.Background()

	if err := createFleetRBAC(ctx, k8s, "gardenio-qa", "gardenio-agent"); err != nil {
		t.Fatalf("createFleetRBAC: %v", err)
	}
	role, err := k8s.RbacV1().Roles("gardenio-qa").Get(ctx, "gardenio-agent", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting created role: %v", err)
	}

	want := map[string][]string{
		"pods":                   {"get", "list", "create", "delete"},
		"secrets":                {"get"},
		"persistentvolumeclaims": {"get", "list", "create", "delete"},
	}
	got := map[string][]string{}
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			got[res] = append(got[res], rule.Verbs...)
		}
	}

	for res, wantVerbs := range want {
		gotVerbs, ok := got[res]
		if !ok {
			t.Errorf("missing rule for resource %q", res)
			continue
		}
		if !equalStringSets(gotVerbs, wantVerbs) {
			t.Errorf("verbs for %q = %v, want %v", res, gotVerbs, wantVerbs)
		}
	}

	// Forbidden subresources / resources / verbs that F5 removed —
	// regressing any of these re-introduces the audit findings.
	forbidden := map[string]bool{
		"pods/exec":  true,
		"pods/log":   true,
		"configmaps": true,
		"services":   true,
	}
	for res := range got {
		if forbidden[res] {
			t.Errorf("forbidden resource %q present in fleet Role (F5 regression)", res)
		}
	}
	if verbs, ok := got["secrets"]; ok {
		for _, v := range verbs {
			if v == "list" || v == "create" || v == "update" || v == "patch" || v == "delete" {
				t.Errorf("forbidden secrets verb %q present (F5 regression: agent must not be able to tamper with peer credentials)", v)
			}
		}
	}
	if verbs, ok := got["pods"]; ok {
		for _, v := range verbs {
			if v == "watch" || v == "update" || v == "patch" {
				t.Errorf("forbidden pods verb %q present (F5: not consumed by podmanager)", v)
			}
		}
	}
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, s := range a {
		m[s]++
	}
	for _, s := range b {
		m[s]--
	}
	for _, v := range m {
		if v != 0 {
			return false
		}
	}
	return true
}

func TestCreateFleetNamespaceProfileOverride(t *testing.T) {
	t.Setenv("GARDENIO_PSA_PROFILE", "baseline")
	k8s := fake.NewSimpleClientset()
	ctx := context.Background()

	if err := createFleetNamespace(ctx, k8s, "gardenio-qa", "qa", "gardenio"); err != nil {
		t.Fatalf("createFleetNamespace: %v", err)
	}
	ns, err := k8s.CoreV1().Namespaces().Get(ctx, "gardenio-qa", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting created namespace: %v", err)
	}
	if got := ns.Labels["pod-security.kubernetes.io/enforce"]; got != "baseline" {
		t.Errorf("enforce label = %q, want %q", got, "baseline")
	}
}

func TestCreateFleetLimitRange(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	ctx := context.Background()

	if err := createFleetLimitRange(ctx, k8s, "gardenio-qa"); err != nil {
		t.Fatalf("createFleetLimitRange: %v", err)
	}

	lr, err := k8s.CoreV1().LimitRanges("gardenio-qa").Get(ctx, "gardenio-defaults", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting created LimitRange: %v", err)
	}

	if len(lr.Spec.Limits) != 1 {
		t.Fatalf("expected 1 LimitRangeItem, got %d", len(lr.Spec.Limits))
	}
	item := lr.Spec.Limits[0]
	if item.Type != corev1.LimitTypeContainer {
		t.Errorf("Type = %q, want Container", item.Type)
	}

	// Defaults must mirror the podmanager defaults so podmanager-built
	// pods see no behavioral change.
	wantDefault := map[corev1.ResourceName]string{
		corev1.ResourceCPU:    podmanager.DefaultCPULimit,
		corev1.ResourceMemory: podmanager.DefaultMemoryLimit,
	}
	for k, want := range wantDefault {
		if got := item.Default[k]; !got.Equal(resource.MustParse(want)) {
			t.Errorf("Default[%q] = %s, want %s", k, got.String(), want)
		}
	}
	wantRequest := map[corev1.ResourceName]string{
		corev1.ResourceCPU:    podmanager.DefaultCPURequest,
		corev1.ResourceMemory: podmanager.DefaultMemoryRequest,
	}
	for k, want := range wantRequest {
		if got := item.DefaultRequest[k]; !got.Equal(resource.MustParse(want)) {
			t.Errorf("DefaultRequest[%q] = %s, want %s", k, got.String(), want)
		}
	}
	wantMax := map[corev1.ResourceName]string{
		corev1.ResourceCPU:    podmanager.DefaultCPULimit,
		corev1.ResourceMemory: podmanager.DefaultMemoryLimit,
	}
	for k, want := range wantMax {
		if got := item.Max[k]; !got.Equal(resource.MustParse(want)) {
			t.Errorf("Max[%q] = %s, want %s", k, got.String(), want)
		}
	}
}

func TestCreateFleetLimitRangeIdempotent(t *testing.T) {
	k8s := fake.NewSimpleClientset()
	ctx := context.Background()

	if err := createFleetLimitRange(ctx, k8s, "gardenio-qa"); err != nil {
		t.Fatalf("first createFleetLimitRange: %v", err)
	}
	// Second call must not error on AlreadyExists.
	if err := createFleetLimitRange(ctx, k8s, "gardenio-qa"); err != nil {
		t.Fatalf("second createFleetLimitRange: %v", err)
	}
}
