// Package landns is a tenant land's Kubernetes namespace (Lands Track B spec
// gardenio/docs/superpowers/specs/2026-10-19-lands-track-b-design.md §5.2,
// B-D9, step 10.2): one template the provisioner applies when it opens a land
// and deletes when gb lands close takes the land apart.
//
// The template is, for land <name>:
//
//   - Namespace land-<name>, labelled gardenio.io/land=<name> and PodSecurity
//     restricted (tenant pods carry no thaw-state hostPath, see the
//     controller's land fence);
//   - ResourceQuota: pods = max_live_agents + 1, and CPU, memory,
//     ephemeral storage and one workspace claim for that many agent pods;
//     no Services; and a cap on Secrets and ConfigMaps, so nothing that
//     writes in the namespace can fill etcd or claim storage past the land's
//     share;
//   - LimitRange with the agent pod's defaults, so a container that names no
//     resources still fits the quota;
//   - NetworkPolicy land-fence: default deny both ways; egress to cluster DNS
//     and the release namespace's services an agent needs (kbeads, LiteLLM,
//     coopmux, NATS, the bridge's land listener), and to 0.0.0.0/0 except
//     the cluster CIDRs, RFC 1918 and the metadata IP; ingress only from
//     coopmux to the coop port;
//   - ServiceAccount land-agent with no RBAC and
//     automountServiceAccountToken false;
//   - RoleBinding gardenio-land-agent-manager, which gives the controller's
//     ServiceAccount that one ClusterRole in this namespace only.
//
// Every object carries gardenio.io/land and app.kubernetes.io/managed-by
// gardenio-controller. Apply refuses to take over a namespace that is not
// labelled for the land, and Delete refuses to delete one, so a land name can
// never reach a namespace the provisioner did not make.
package landns

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
)

const (
	// Prefix is the land namespace prefix: land <name> runs in land-<name>.
	Prefix = "land-"

	// LabelLand names the land a namespace and its objects belong to.
	LabelLand = "gardenio.io/land"
	// LabelManagedBy marks what the controller made.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedBy is LabelManagedBy's value.
	ManagedBy = "gardenio-controller"

	// ServiceAccount is the tenant pods' ServiceAccount: no RBAC, no token.
	ServiceAccount = "land-agent"
	// ClusterRole is the one ClusterRole the controller is bound to in a land
	// namespace (the chart renders it, templates/controller/lands-namespace-rbac.yaml).
	ClusterRole = "gardenio-land-agent-manager"
	// RuntimeClassEnv names the RuntimeClass tenant pods run under
	// (landNamespaces.runtimeClassName). Unset, they carry no
	// runtimeClassName and run on the node's default runtime (runc): the
	// owner dropped gVisor from the Lands gate on 2026-11-01
	// (gd-iyZom5u11A). RuntimeClassGVisor is the value that puts them back
	// under gVisor once nodes provide it.
	RuntimeClassEnv    = "LANDS_RUNTIME_CLASS"
	RuntimeClassGVisor = "runsc"

	// Object names inside the namespace.
	QuotaName       = "land-quota"
	LimitRangeName  = "land-limits"
	PolicyName      = "land-fence"
	RoleBindingName = ClusterRole

	// CoopPort is the agent's coop HTTP port, the one port coopmux reaches.
	CoopPort = 8080
	// BridgeLandPort is the slack-bridge's land listener, which serves only
	// the per-agent-token routes gb say and gb thread use; the bridge's main
	// port (8090) serves its whole mux and is never a tenant peer
	// (gd-pXik57wZA9). The chart's slack-bridge renders it.
	BridgeLandPort = 8091
)

// Per-pod figures the quota multiplies by max_live_agents + 1. They match the
// agent pod's defaults (internal/podmanager: 2/4 CPU, 8/16Gi memory).
var (
	PodCPURequest       = resource.MustParse("2")
	PodCPULimit         = resource.MustParse("4")
	PodMemoryRequest    = resource.MustParse("8Gi")
	PodMemoryLimit      = resource.MustParse("16Gi")
	PodEphemeralRequest = resource.MustParse("2Gi")
	PodEphemeralLimit   = resource.MustParse("20Gi")
	// PodWorkspaceStorage is one agent's workspace claim (podmanager's
	// default PVC size).
	PodWorkspaceStorage = resource.MustParse("10Gi")
)

// Object counts the quota allows: a fixed allowance for the land's own
// objects (kube-root-ca.crt, the release Secrets copied into the namespace)
// plus a few per agent pod (its per-agent token and credential copies).
const (
	SecretsBase      = 8
	SecretsPerPod    = 4
	ConfigMapsBase   = 4
	ConfigMapsPerPod = 2
)

// DefaultExceptCIDRs are the destinations the internet egress rule never
// reaches: RFC 1918 (which holds k3s's default pod and service CIDRs,
// 10.42.0.0/16 and 10.43.0.0/16), carrier-grade NAT, and the link-local range
// with the metadata IP 169.254.169.254.
var DefaultExceptCIDRs = []string{"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16"}

// Peer is one in-cluster destination a tenant pod may reach: the pods with
// Labels in Namespace (the release namespace when empty), on Ports (TCP
// unless UDP is set).
type Peer struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace,omitempty"`
	Labels    map[string]string `json:"labels"`
	Ports     []int32           `json:"ports"`
	UDP       bool              `json:"udp,omitempty"`
}

// DefaultPeers are the release namespace's services an agent talks to, by the
// chart's component labels and default ports.
func DefaultPeers() []Peer {
	c := func(name string, ports ...int32) Peer {
		return Peer{Name: name, Labels: map[string]string{"app.kubernetes.io/component": name}, Ports: ports}
	}
	return []Peer{
		c("beads", 8080, 9090),
		c("litellm", 4000),
		c("coopmux", 9800),
		c("nats", 4222),
		c("slack-bridge", BridgeLandPort),
	}
}

// Config is what every land namespace shares.
type Config struct {
	// ReleaseNamespace is where the controller and the services in Peers run.
	ReleaseNamespace string
	// ControllerServiceAccount is the ServiceAccount the RoleBinding names.
	ControllerServiceAccount string
	// Peers are the in-cluster egress destinations; nil means DefaultPeers.
	Peers []Peer
	// DNSNamespace and DNSLabels select cluster DNS (kube-system,
	// k8s-app=kube-dns when empty).
	DNSNamespace string
	DNSLabels    map[string]string
	// ExceptCIDRs are cut from the internet egress rule; nil means
	// DefaultExceptCIDRs. ExtraExceptCIDRs are cut as well (the cluster's own
	// pod and service CIDRs when they sit outside RFC 1918).
	ExceptCIDRs      []string
	ExtraExceptCIDRs []string
	// CoopmuxLabels select coopmux, the one ingress source.
	CoopmuxLabels map[string]string
}

// Validate reports a Config that cannot render a land namespace.
func (c Config) Validate() error {
	var missing []string
	if c.ReleaseNamespace == "" {
		missing = append(missing, "the release namespace")
	}
	if c.ControllerServiceAccount == "" {
		missing = append(missing, "the controller ServiceAccount")
	}
	if len(missing) > 0 {
		return fmt.Errorf("land namespaces need %s", strings.Join(missing, " and "))
	}
	for _, cidr := range append(c.exceptCIDRs(), c.ExtraExceptCIDRs...) {
		if _, _, err := net.ParseCIDR(cidr); err != nil {
			return fmt.Errorf("land namespace egress: %w", err)
		}
	}
	for _, p := range c.peers() {
		if len(p.Labels) == 0 || len(p.Ports) == 0 {
			return fmt.Errorf("land namespace egress peer %q needs labels and ports", p.Name)
		}
	}
	return nil
}

func (c Config) peers() []Peer {
	if c.Peers == nil {
		return DefaultPeers()
	}
	return c.Peers
}

func (c Config) exceptCIDRs() []string {
	if c.ExceptCIDRs == nil {
		return DefaultExceptCIDRs
	}
	return c.ExceptCIDRs
}

// Name is land's namespace, or an error when land-<land> is not a namespace
// name.
func Name(land string) (string, error) {
	ns := Prefix + land
	if land == "" {
		return "", errors.New("a land namespace needs a land name")
	}
	if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
		return "", fmt.Errorf("land %q cannot be a namespace: %s", land, strings.Join(errs, "; "))
	}
	return ns, nil
}

// Objects is one land's rendered template.
type Objects struct {
	Namespace      *corev1.Namespace
	ResourceQuota  *corev1.ResourceQuota
	LimitRange     *corev1.LimitRange
	NetworkPolicy  *networkingv1.NetworkPolicy
	ServiceAccount *corev1.ServiceAccount
	RoleBinding    *rbacv1.RoleBinding
}

// Render renders land's template for a land that runs at most maxLiveAgents
// agents at once.
func Render(land string, maxLiveAgents int, cfg Config) (*Objects, error) {
	ns, err := Name(land)
	if err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if maxLiveAgents < 1 {
		return nil, fmt.Errorf("land %q: max_live_agents must be at least 1, not %d", land, maxLiveAgents)
	}
	labels := func() map[string]string {
		return map[string]string{LabelLand: land, LabelManagedBy: ManagedBy}
	}
	meta := func(name string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels()}
	}

	nsLabels := labels()
	for _, mode := range []string{"enforce", "audit", "warn"} {
		nsLabels["pod-security.kubernetes.io/"+mode] = "restricted"
	}
	pods := int64(maxLiveAgents + 1)
	times := func(q resource.Quantity) resource.Quantity {
		return *resource.NewQuantity(q.Value()*pods, q.Format)
	}
	timesCPU := func(q resource.Quantity) resource.Quantity {
		return *resource.NewMilliQuantity(q.MilliValue()*pods, q.Format)
	}
	count := func(n int64) resource.Quantity { return *resource.NewQuantity(n, resource.DecimalSI) }

	return &Objects{
		Namespace: &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: nsLabels}},
		ResourceQuota: &corev1.ResourceQuota{
			ObjectMeta: meta(QuotaName),
			Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
				corev1.ResourcePods:                     count(pods),
				corev1.ResourceRequestsCPU:              timesCPU(PodCPURequest),
				corev1.ResourceLimitsCPU:                timesCPU(PodCPULimit),
				corev1.ResourceRequestsMemory:           times(PodMemoryRequest),
				corev1.ResourceLimitsMemory:             times(PodMemoryLimit),
				corev1.ResourceRequestsEphemeralStorage: times(PodEphemeralRequest),
				corev1.ResourceLimitsEphemeralStorage:   times(PodEphemeralLimit),
				corev1.ResourcePersistentVolumeClaims:   count(pods),
				corev1.ResourceRequestsStorage:          times(PodWorkspaceStorage),
				corev1.ResourceServices:                 count(0),
				corev1.ResourceServicesLoadBalancers:    count(0),
				corev1.ResourceServicesNodePorts:        count(0),
				corev1.ResourceSecrets:                  count(SecretsBase + SecretsPerPod*pods),
				corev1.ResourceConfigMaps:               count(ConfigMapsBase + ConfigMapsPerPod*pods),
			}},
		},
		LimitRange: &corev1.LimitRange{
			ObjectMeta: meta(LimitRangeName),
			Spec: corev1.LimitRangeSpec{Limits: []corev1.LimitRangeItem{{
				Type: corev1.LimitTypeContainer,
				DefaultRequest: corev1.ResourceList{
					corev1.ResourceCPU: PodCPURequest, corev1.ResourceMemory: PodMemoryRequest,
					corev1.ResourceEphemeralStorage: PodEphemeralRequest,
				},
				Default: corev1.ResourceList{
					corev1.ResourceCPU: PodCPULimit, corev1.ResourceMemory: PodMemoryLimit,
					corev1.ResourceEphemeralStorage: PodEphemeralLimit,
				},
			}}},
		},
		NetworkPolicy:  networkPolicy(meta(PolicyName), cfg),
		ServiceAccount: &corev1.ServiceAccount{ObjectMeta: meta(ServiceAccount), AutomountServiceAccountToken: ptr(false)},
		RoleBinding: &rbacv1.RoleBinding{
			ObjectMeta: meta(RoleBindingName),
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: ClusterRole},
			Subjects: []rbacv1.Subject{{
				Kind: rbacv1.ServiceAccountKind, Name: cfg.ControllerServiceAccount, Namespace: cfg.ReleaseNamespace,
			}},
		},
	}, nil
}

func networkPolicy(meta metav1.ObjectMeta, cfg Config) *networkingv1.NetworkPolicy {
	tcp, udp := corev1.ProtocolTCP, corev1.ProtocolUDP
	port := func(p int32, proto corev1.Protocol) networkingv1.NetworkPolicyPort {
		v := intstr.FromInt32(p)
		return networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &v}
	}
	inNamespace := func(ns string, labels map[string]string) networkingv1.NetworkPolicyPeer {
		return networkingv1.NetworkPolicyPeer{
			NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": ns}},
			PodSelector:       &metav1.LabelSelector{MatchLabels: labels},
		}
	}

	dnsNS, dnsLabels := cfg.DNSNamespace, cfg.DNSLabels
	if dnsNS == "" {
		dnsNS = "kube-system"
	}
	if len(dnsLabels) == 0 {
		dnsLabels = map[string]string{"k8s-app": "kube-dns"}
	}
	egress := []networkingv1.NetworkPolicyEgressRule{{
		To:    []networkingv1.NetworkPolicyPeer{inNamespace(dnsNS, dnsLabels)},
		Ports: []networkingv1.NetworkPolicyPort{port(53, udp), port(53, tcp)},
	}}
	for _, p := range cfg.peers() {
		ns := p.Namespace
		if ns == "" {
			ns = cfg.ReleaseNamespace
		}
		proto := tcp
		if p.UDP {
			proto = udp
		}
		rule := networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{inNamespace(ns, p.Labels)}}
		for _, n := range p.Ports {
			rule.Ports = append(rule.Ports, port(n, proto))
		}
		egress = append(egress, rule)
	}
	egress = append(egress, networkingv1.NetworkPolicyEgressRule{To: []networkingv1.NetworkPolicyPeer{{
		IPBlock: &networkingv1.IPBlock{
			CIDR:   "0.0.0.0/0",
			Except: append(append([]string(nil), cfg.exceptCIDRs()...), cfg.ExtraExceptCIDRs...),
		},
	}}})

	coopmux := cfg.CoopmuxLabels
	if len(coopmux) == 0 {
		coopmux = map[string]string{"app.kubernetes.io/component": "coopmux"}
	}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: meta,
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From:  []networkingv1.NetworkPolicyPeer{inNamespace(cfg.ReleaseNamespace, coopmux)},
				Ports: []networkingv1.NetworkPolicyPort{port(CoopPort, tcp)},
			}},
			Egress: egress,
		},
	}
}

// Applier applies and deletes land namespaces with the controller's
// Kubernetes client.
type Applier struct {
	Client kubernetes.Interface
	Config Config
}

// ErrNotOurs is returned for a namespace of the land's name that the
// controller did not make for that land.
var ErrNotOurs = errors.New("namespace is not this land's")

// Apply creates land's namespace and objects, or brings existing ones back to
// the template. It returns the namespace name. It is safe to re-run.
func (a *Applier) Apply(ctx context.Context, land string, maxLiveAgents int) (string, error) {
	objs, err := Render(land, maxLiveAgents, a.Config)
	if err != nil {
		return "", err
	}
	ns := objs.Namespace.Name
	c := a.Client

	existing, err := c.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
		if _, err := c.CoreV1().Namespaces().Create(ctx, objs.Namespace, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			return "", fmt.Errorf("creating namespace %s: %w", ns, err)
		}
	case err != nil:
		return "", fmt.Errorf("reading namespace %s: %w", ns, err)
	default:
		if err := owned(existing.ObjectMeta, land); err != nil {
			return "", fmt.Errorf("namespace %s: %w", ns, err)
		}
		if existing.DeletionTimestamp != nil {
			return "", fmt.Errorf("namespace %s is still being deleted; the next pass tries again", ns)
		}
		if !hasLabels(existing.Labels, objs.Namespace.Labels) {
			existing.Labels = merge(existing.Labels, objs.Namespace.Labels)
			if _, err := c.CoreV1().Namespaces().Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
				return "", fmt.Errorf("labelling namespace %s: %w", ns, err)
			}
		}
	}

	// The fence goes up before anything that could run in the namespace.
	np := c.NetworkingV1().NetworkPolicies(ns)
	if err := upsert(ctx, "NetworkPolicy", objs.NetworkPolicy.Name,
		func() error { _, err := np.Create(ctx, objs.NetworkPolicy, metav1.CreateOptions{}); return err },
		func() (metav1.Object, error) { return np.Get(ctx, objs.NetworkPolicy.Name, metav1.GetOptions{}) },
		func(cur metav1.Object) error {
			o := cur.(*networkingv1.NetworkPolicy)
			o.Labels, o.Spec = merge(o.Labels, objs.NetworkPolicy.Labels), objs.NetworkPolicy.Spec
			_, err := np.Update(ctx, o, metav1.UpdateOptions{})
			return err
		}, land); err != nil {
		return "", err
	}
	rq := c.CoreV1().ResourceQuotas(ns)
	if err := upsert(ctx, "ResourceQuota", objs.ResourceQuota.Name,
		func() error { _, err := rq.Create(ctx, objs.ResourceQuota, metav1.CreateOptions{}); return err },
		func() (metav1.Object, error) { return rq.Get(ctx, objs.ResourceQuota.Name, metav1.GetOptions{}) },
		func(cur metav1.Object) error {
			o := cur.(*corev1.ResourceQuota)
			o.Labels, o.Spec = merge(o.Labels, objs.ResourceQuota.Labels), objs.ResourceQuota.Spec
			_, err := rq.Update(ctx, o, metav1.UpdateOptions{})
			return err
		}, land); err != nil {
		return "", err
	}
	lr := c.CoreV1().LimitRanges(ns)
	if err := upsert(ctx, "LimitRange", objs.LimitRange.Name,
		func() error { _, err := lr.Create(ctx, objs.LimitRange, metav1.CreateOptions{}); return err },
		func() (metav1.Object, error) { return lr.Get(ctx, objs.LimitRange.Name, metav1.GetOptions{}) },
		func(cur metav1.Object) error {
			o := cur.(*corev1.LimitRange)
			o.Labels, o.Spec = merge(o.Labels, objs.LimitRange.Labels), objs.LimitRange.Spec
			_, err := lr.Update(ctx, o, metav1.UpdateOptions{})
			return err
		}, land); err != nil {
		return "", err
	}
	sa := c.CoreV1().ServiceAccounts(ns)
	if err := upsert(ctx, "ServiceAccount", objs.ServiceAccount.Name,
		func() error { _, err := sa.Create(ctx, objs.ServiceAccount, metav1.CreateOptions{}); return err },
		func() (metav1.Object, error) { return sa.Get(ctx, objs.ServiceAccount.Name, metav1.GetOptions{}) },
		func(cur metav1.Object) error {
			o := cur.(*corev1.ServiceAccount)
			o.Labels, o.AutomountServiceAccountToken = merge(o.Labels, objs.ServiceAccount.Labels), ptr(false)
			_, err := sa.Update(ctx, o, metav1.UpdateOptions{})
			return err
		}, land); err != nil {
		return "", err
	}
	rb := c.RbacV1().RoleBindings(ns)
	if err := upsert(ctx, "RoleBinding", objs.RoleBinding.Name,
		func() error { _, err := rb.Create(ctx, objs.RoleBinding, metav1.CreateOptions{}); return err },
		func() (metav1.Object, error) { return rb.Get(ctx, objs.RoleBinding.Name, metav1.GetOptions{}) },
		func(cur metav1.Object) error {
			o := cur.(*rbacv1.RoleBinding)
			if o.RoleRef != objs.RoleBinding.RoleRef {
				// roleRef is immutable: replace the binding.
				if err := rb.Delete(ctx, o.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
					return err
				}
				_, err := rb.Create(ctx, objs.RoleBinding, metav1.CreateOptions{})
				return err
			}
			o.Labels, o.Subjects = merge(o.Labels, objs.RoleBinding.Labels), objs.RoleBinding.Subjects
			_, err := rb.Update(ctx, o, metav1.UpdateOptions{})
			return err
		}, land); err != nil {
		return "", err
	}
	return ns, nil
}

// Delete deletes land's namespace, and with it everything in it. A namespace
// that is gone, or already being deleted, is done.
func (a *Applier) Delete(ctx context.Context, land string) error {
	ns, err := Name(land)
	if err != nil {
		return err
	}
	existing, err := a.Client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading namespace %s: %w", ns, err)
	}
	if err := owned(existing.ObjectMeta, land); err != nil {
		return fmt.Errorf("namespace %s: %w; it is not deleted", ns, err)
	}
	if existing.DeletionTimestamp != nil {
		return nil
	}
	if err := a.Client.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting namespace %s: %w", ns, err)
	}
	return nil
}

// owned reports whether an object is the controller's, made for land.
func owned(m metav1.ObjectMeta, land string) error {
	if m.Labels[LabelLand] != land || m.Labels[LabelManagedBy] != ManagedBy {
		return fmt.Errorf("%w (labels %s=%q, %s=%q)", ErrNotOurs, LabelLand, m.Labels[LabelLand], LabelManagedBy, m.Labels[LabelManagedBy])
	}
	return nil
}

// upsert creates an object, or updates the existing one after checking it is
// the land's.
func upsert(ctx context.Context, kind, name string, create func() error, get func() (metav1.Object, error), update func(metav1.Object) error, land string) error {
	err := create()
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating %s %s: %w", kind, name, err)
	}
	cur, err := get()
	if err != nil {
		return fmt.Errorf("reading %s %s: %w", kind, name, err)
	}
	if lbl := cur.GetLabels(); lbl[LabelLand] != land || lbl[LabelManagedBy] != ManagedBy {
		return fmt.Errorf("%s %s: %w", kind, name, ErrNotOurs)
	}
	if err := update(cur); err != nil {
		return fmt.Errorf("updating %s %s: %w", kind, name, err)
	}
	return nil
}

func hasLabels(have, want map[string]string) bool {
	for k, v := range want {
		if have[k] != v {
			return false
		}
	}
	return true
}

func merge(have, want map[string]string) map[string]string {
	out := make(map[string]string, len(have)+len(want))
	for k, v := range have {
		out[k] = v
	}
	for k, v := range want {
		out[k] = v
	}
	return out
}

func ptr[T any](v T) *T { return &v }
