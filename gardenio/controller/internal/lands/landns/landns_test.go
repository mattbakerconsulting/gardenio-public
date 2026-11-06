package landns

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func testConfig() Config {
	return Config{ReleaseNamespace: "gardenio", ControllerServiceAccount: "gardenio-agents"}
}

func TestName(t *testing.T) {
	if ns, err := Name("acme"); err != nil || ns != "land-acme" {
		t.Fatalf("Name(acme) = %q, %v", ns, err)
	}
	for _, bad := range []string{"", "Acme", "a_b", strings.Repeat("x", 60)} {
		if _, err := Name(bad); err == nil {
			t.Errorf("Name(%q) = nil error, want one", bad)
		}
	}
}

func TestRenderTemplate(t *testing.T) {
	o, err := Render("acme", 3, testConfig())
	if err != nil {
		t.Fatal(err)
	}

	ns := o.Namespace
	if ns.Name != "land-acme" || ns.Labels[LabelLand] != "acme" || ns.Labels[LabelManagedBy] != ManagedBy {
		t.Errorf("namespace = %s %v", ns.Name, ns.Labels)
	}
	if ns.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Errorf("namespace PSA enforce = %q, want restricted", ns.Labels["pod-security.kubernetes.io/enforce"])
	}

	for _, m := range []metav1.ObjectMeta{o.ResourceQuota.ObjectMeta, o.LimitRange.ObjectMeta, o.NetworkPolicy.ObjectMeta, o.ServiceAccount.ObjectMeta, o.RoleBinding.ObjectMeta} {
		if m.Namespace != "land-acme" || m.Labels[LabelLand] != "acme" {
			t.Errorf("%s: namespace %q labels %v", m.Name, m.Namespace, m.Labels)
		}
	}

	// Quota: max_live_agents + 1 pods, that many agent pods' resources and
	// workspace claims, no Services, and capped Secrets and ConfigMaps.
	hard := o.ResourceQuota.Spec.Hard
	want := map[corev1.ResourceName]string{
		corev1.ResourcePods:                     "4",
		corev1.ResourceRequestsCPU:              "8",
		corev1.ResourceLimitsCPU:                "16",
		corev1.ResourceRequestsMemory:           "32Gi",
		corev1.ResourceLimitsMemory:             "64Gi",
		corev1.ResourceRequestsEphemeralStorage: "8Gi",
		corev1.ResourceLimitsEphemeralStorage:   "80Gi",
		corev1.ResourcePersistentVolumeClaims:   "4",
		corev1.ResourceRequestsStorage:          "40Gi",
		corev1.ResourceServices:                 "0",
		corev1.ResourceServicesLoadBalancers:    "0",
		corev1.ResourceServicesNodePorts:        "0",
		corev1.ResourceSecrets:                  "24",
		corev1.ResourceConfigMaps:               "12",
	}
	for k, v := range want {
		got := hard[k]
		if got.Cmp(resource.MustParse(v)) != 0 {
			t.Errorf("quota %s = %s, want %s", k, got.String(), v)
		}
	}
	if len(hard) != len(want) {
		t.Errorf("quota has %d entries, want %d: %v", len(hard), len(want), hard)
	}

	// LimitRange: the agent pod's defaults.
	lr := o.LimitRange.Spec.Limits
	if len(lr) != 1 || lr[0].Type != corev1.LimitTypeContainer {
		t.Fatalf("limit range = %+v", lr)
	}
	if got := lr[0].Default[corev1.ResourceMemory]; got.Cmp(PodMemoryLimit) != 0 {
		t.Errorf("default memory limit = %s", got.String())
	}
	if got := lr[0].DefaultRequest[corev1.ResourceCPU]; got.Cmp(PodCPURequest) != 0 {
		t.Errorf("default cpu request = %s", got.String())
	}

	// ServiceAccount: no token.
	if sa := o.ServiceAccount; sa.Name != "land-agent" || sa.AutomountServiceAccountToken == nil || *sa.AutomountServiceAccountToken {
		t.Errorf("service account = %s automount %v", sa.Name, sa.AutomountServiceAccountToken)
	}

	// RoleBinding: the controller to the one ClusterRole, nothing else.
	rb := o.RoleBinding
	if rb.RoleRef != (rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "gardenio-land-agent-manager"}) {
		t.Errorf("roleRef = %+v", rb.RoleRef)
	}
	if len(rb.Subjects) != 1 || rb.Subjects[0] != (rbacv1.Subject{Kind: "ServiceAccount", Name: "gardenio-agents", Namespace: "gardenio"}) {
		t.Errorf("subjects = %+v", rb.Subjects)
	}
}

func TestRenderNetworkPolicy(t *testing.T) {
	o, err := Render("acme", 3, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	spec := o.NetworkPolicy.Spec
	if len(spec.PodSelector.MatchLabels) != 0 || len(spec.PodSelector.MatchExpressions) != 0 {
		t.Errorf("policy selects %v, want every pod", spec.PodSelector)
	}
	if !slices.Equal(spec.PolicyTypes, []networkingv1.PolicyType{"Ingress", "Egress"}) {
		t.Errorf("policy types = %v", spec.PolicyTypes)
	}

	// Ingress: coopmux in the release namespace, to the coop port only.
	if len(spec.Ingress) != 1 {
		t.Fatalf("ingress rules = %d, want 1", len(spec.Ingress))
	}
	in := spec.Ingress[0]
	if len(in.From) != 1 || in.From[0].PodSelector.MatchLabels["app.kubernetes.io/component"] != "coopmux" ||
		in.From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "gardenio" || in.From[0].IPBlock != nil {
		t.Errorf("ingress from = %+v", in.From)
	}
	if len(in.Ports) != 1 || in.Ports[0].Port.IntValue() != CoopPort {
		t.Errorf("ingress ports = %+v", in.Ports)
	}

	// Egress: DNS, each service by namespace and labels, and the internet
	// minus everything internal.
	type dest struct {
		ns, component string
		ports         []int
	}
	var got []dest
	var block *networkingv1.IPBlock
	for _, r := range spec.Egress {
		for _, to := range r.To {
			if to.IPBlock != nil {
				if len(r.Ports) != 0 {
					t.Errorf("internet rule is port-limited: %+v", r.Ports)
				}
				block = to.IPBlock
				continue
			}
			if to.NamespaceSelector == nil || to.PodSelector == nil {
				t.Errorf("egress peer without both selectors: %+v", to)
				continue
			}
			d := dest{ns: to.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"],
				component: to.PodSelector.MatchLabels["app.kubernetes.io/component"] + to.PodSelector.MatchLabels["k8s-app"]}
			for _, p := range r.Ports {
				d.ports = append(d.ports, p.Port.IntValue())
			}
			got = append(got, d)
		}
	}
	wantDest := []dest{
		{"kube-system", "kube-dns", []int{53, 53}},
		{"gardenio", "beads", []int{8080, 9090}},
		{"gardenio", "litellm", []int{4000}},
		{"gardenio", "coopmux", []int{9800}},
		{"gardenio", "nats", []int{4222}},
		{"gardenio", "slack-bridge", []int{8091}},
	}
	if len(got) != len(wantDest) {
		t.Fatalf("egress peers = %+v, want %+v", got, wantDest)
	}
	for i := range got {
		if got[i].ns != wantDest[i].ns || got[i].component != wantDest[i].component || !slices.Equal(got[i].ports, wantDest[i].ports) {
			t.Errorf("egress peer %d = %+v, want %+v", i, got[i], wantDest[i])
		}
	}
	if block == nil || block.CIDR != "0.0.0.0/0" {
		t.Fatalf("internet egress = %+v", block)
	}
	for _, cidr := range []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "169.254.0.0/16"} {
		if !slices.Contains(block.Except, cidr) {
			t.Errorf("internet egress does not cut %s: %v", cidr, block.Except)
		}
	}
}

func TestRenderRefuses(t *testing.T) {
	if _, err := Render("acme", 0, testConfig()); err == nil {
		t.Error("max_live_agents 0 rendered")
	}
	if _, err := Render("acme", 3, Config{ReleaseNamespace: "gardenio"}); err == nil {
		t.Error("no controller ServiceAccount rendered")
	}
	cfg := testConfig()
	cfg.ExtraExceptCIDRs = []string{"not-a-cidr"}
	if _, err := Render("acme", 3, cfg); err == nil {
		t.Error("a bad CIDR rendered")
	}
	cfg = testConfig()
	cfg.Peers = []Peer{{Name: "open", Ports: []int32{80}}}
	if _, err := Render("acme", 3, cfg); err == nil {
		t.Error("a peer with no labels rendered (it would select every pod)")
	}
}

func TestApplyCreatesAndIsIdempotent(t *testing.T) {
	ctx := context.Background()
	c := fake.NewSimpleClientset()
	a := &Applier{Client: c, Config: testConfig()}
	for i := range 2 {
		ns, err := a.Apply(ctx, "acme", 3)
		if err != nil || ns != "land-acme" {
			t.Fatalf("apply %d = %q, %v", i, ns, err)
		}
	}
	if _, err := c.CoreV1().Namespaces().Get(ctx, "land-acme", metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.NetworkingV1().NetworkPolicies("land-acme").Get(ctx, PolicyName, metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.RbacV1().RoleBindings("land-acme").Get(ctx, RoleBindingName, metav1.GetOptions{}); err != nil {
		t.Fatal(err)
	}
	sa, err := c.CoreV1().ServiceAccounts("land-acme").Get(ctx, ServiceAccount, metav1.GetOptions{})
	if err != nil || *sa.AutomountServiceAccountToken {
		t.Fatalf("service account %v, %v", sa, err)
	}

	// A raised max_live_agents re-sizes the quota on the next apply.
	if _, err := a.Apply(ctx, "acme", 5); err != nil {
		t.Fatal(err)
	}
	rq, _ := c.CoreV1().ResourceQuotas("land-acme").Get(ctx, QuotaName, metav1.GetOptions{})
	if got := rq.Spec.Hard[corev1.ResourcePods]; got.Value() != 6 {
		t.Errorf("pods quota after re-apply = %s, want 6", got.String())
	}
}

func TestApplyRestoresTamperedPolicy(t *testing.T) {
	ctx := context.Background()
	c := fake.NewSimpleClientset()
	a := &Applier{Client: c, Config: testConfig()}
	if _, err := a.Apply(ctx, "acme", 3); err != nil {
		t.Fatal(err)
	}
	np, _ := c.NetworkingV1().NetworkPolicies("land-acme").Get(ctx, PolicyName, metav1.GetOptions{})
	np.Spec.Egress = []networkingv1.NetworkPolicyEgressRule{{}} // allow all
	if _, err := c.NetworkingV1().NetworkPolicies("land-acme").Update(ctx, np, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Apply(ctx, "acme", 3); err != nil {
		t.Fatal(err)
	}
	np, _ = c.NetworkingV1().NetworkPolicies("land-acme").Get(ctx, PolicyName, metav1.GetOptions{})
	if len(np.Spec.Egress) < 2 || len(np.Spec.Egress[0].To) == 0 {
		t.Errorf("tampered policy was not restored: %+v", np.Spec.Egress)
	}
}

func TestApplyAndDeleteRefuseAForeignNamespace(t *testing.T) {
	ctx := context.Background()
	c := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "land-acme"}})
	a := &Applier{Client: c, Config: testConfig()}
	if _, err := a.Apply(ctx, "acme", 3); !errors.Is(err, ErrNotOurs) {
		t.Errorf("apply on an unlabelled namespace = %v, want ErrNotOurs", err)
	}
	if err := a.Delete(ctx, "acme"); !errors.Is(err, ErrNotOurs) {
		t.Errorf("delete of an unlabelled namespace = %v, want ErrNotOurs", err)
	}
	if _, err := c.CoreV1().Namespaces().Get(ctx, "land-acme", metav1.GetOptions{}); err != nil {
		t.Errorf("foreign namespace was deleted: %v", err)
	}

	// Another land's label is foreign too.
	c = fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "land-acme",
		Labels: map[string]string{LabelLand: "other", LabelManagedBy: ManagedBy}}})
	a = &Applier{Client: c, Config: testConfig()}
	if _, err := a.Apply(ctx, "acme", 3); !errors.Is(err, ErrNotOurs) {
		t.Errorf("apply on another land's namespace = %v, want ErrNotOurs", err)
	}
}

func TestApplyRefusesAForeignObjectInTheNamespace(t *testing.T) {
	ctx := context.Background()
	c := fake.NewSimpleClientset(&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: RoleBindingName, Namespace: "land-acme"},
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"}})
	a := &Applier{Client: c, Config: testConfig()}
	if _, err := a.Apply(ctx, "acme", 3); !errors.Is(err, ErrNotOurs) {
		t.Errorf("apply over a foreign RoleBinding = %v, want ErrNotOurs", err)
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	c := fake.NewSimpleClientset()
	a := &Applier{Client: c, Config: testConfig()}
	if err := a.Delete(ctx, "acme"); err != nil {
		t.Fatalf("delete of a missing namespace = %v", err)
	}
	if _, err := a.Apply(ctx, "acme", 3); err != nil {
		t.Fatal(err)
	}
	if err := a.Delete(ctx, "acme"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CoreV1().Namespaces().Get(ctx, "land-acme", metav1.GetOptions{}); err == nil {
		t.Error("namespace still there after delete")
	}
}
