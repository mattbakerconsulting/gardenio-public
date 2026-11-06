package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/podmanager"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

var fleetCmd = &cobra.Command{
	Use:     "fleet",
	Short:   "Manage fleets (multi-namespace bead collections)",
	GroupID: "orchestration",
}

var fleetCreateCmd = &cobra.Command{
	Use:   "create <name>",
	Short: "Create a new fleet (namespace + RBAC + token + bead)",
	Long: `Create a new fleet by provisioning:

  1. K8s namespace with gardenio labels
  2. ServiceAccount + Role + RoleBinding for fleet agents
  3. Fleet bearer token stored as a K8s Secret
  4. Fleet bead in the parent fleet for tracking

The fleet token is registered with the beads daemon so queries from
agents in this fleet are automatically scoped.

Requires cluster-admin or fleet-manager ClusterRole.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		parentFleet, _ := cmd.Flags().GetString("parent")
		description, _ := cmd.Flags().GetString("description")
		maxAgents, _ := cmd.Flags().GetString("max-agents")
		dryRun, _ := cmd.Flags().GetBool("dry-run")

		namespace := "gardenio-" + name

		if dryRun {
			fmt.Printf("dry-run: would create fleet %q\n", name)
			fmt.Printf("  namespace:    %s\n", namespace)
			fmt.Printf("  parent fleet: %s\n", parentFleet)
			fmt.Printf("  description:  %s\n", description)
			return nil
		}

		ctx := context.Background()

		// Build K8s client.
		k8s, err := buildK8sClient()
		if err != nil {
			return fmt.Errorf("building K8s client: %w", err)
		}

		// Step 1: Create namespace.
		fmt.Printf("creating namespace %s...\n", namespace)
		if err := createFleetNamespace(ctx, k8s, namespace, name, parentFleet); err != nil {
			return fmt.Errorf("creating namespace: %w", err)
		}

		// Step 1b: Create ResourceQuota (caps pods/CPU/mem/PVCs/services/secrets).
		fmt.Printf("creating resource quota in %s...\n", namespace)
		if err := createFleetResourceQuota(ctx, k8s, namespace); err != nil {
			return fmt.Errorf("creating resource quota: %w", err)
		}

		// Step 1c: Create LimitRange (defense-in-depth resource cap).
		fmt.Printf("creating LimitRange in %s...\n", namespace)
		if err := createFleetLimitRange(ctx, k8s, namespace); err != nil {
			return fmt.Errorf("creating LimitRange: %w", err)
		}

		// Step 1d: Create baseline NetworkPolicies (default-deny + allow-DNS +
		// allow-egress to parent beads/NATS). Audit ka-0000005403 §F4.
		parentNS := parentNamespace()
		fmt.Printf("creating NetworkPolicies in %s (parent=%s)...\n", namespace, parentNS)
		if err := createFleetNetworkPolicies(ctx, k8s, namespace, parentNS); err != nil {
			return fmt.Errorf("creating network policies: %w", err)
		}

		// Step 2: Create RBAC (SA + Role + RoleBinding).
		saName := "gardenio-agent"
		fmt.Printf("creating RBAC in %s...\n", namespace)
		if err := createFleetRBAC(ctx, k8s, namespace, saName); err != nil {
			return fmt.Errorf("creating RBAC: %w", err)
		}

		// Step 3: Generate fleet token and store as Secret.
		token, err := generateToken()
		if err != nil {
			return fmt.Errorf("generating token: %w", err)
		}
		secretName := name + "-beads-token"
		fmt.Printf("creating token secret %s/%s...\n", namespace, secretName)
		storedToken, err := createFleetTokenSecret(ctx, k8s, namespace, secretName, token)
		if err != nil {
			return fmt.Errorf("creating token secret: %w", err)
		}

		// Step 4: Create fleet bead in parent fleet.
		fmt.Printf("creating fleet bead...\n")
		fields := map[string]string{
			"namespace":    namespace,
			"description":  description,
			"token_secret": secretName,
			"parent_fleet": parentFleet,
			"status":       "active",
		}
		if maxAgents != "" {
			fields["max_agents"] = maxAgents
		}
		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("marshalling fleet fields: %w", err)
		}
		beadID, err := daemon.CreateBead(ctx, beadsapi.CreateBeadRequest{
			Title:       name,
			Type:        "fleet",
			Description: description,
			CreatedBy:   actor,
			Labels:      []string{"fleet:" + name},
			Fields:      json.RawMessage(fieldsJSON),
		})
		if err != nil {
			return fmt.Errorf("creating fleet bead: %w", err)
		}

		fmt.Printf("\nfleet %q created successfully\n", name)
		fmt.Printf("  bead:      %s\n", beadID)
		fmt.Printf("  namespace: %s\n", namespace)
		fmt.Printf("  secret:    %s/%s\n", namespace, secretName)
		fmt.Printf("\nRegister the token with the beads daemon:\n")
		fmt.Printf("  Add to BEADS_FLEET_MAP: %s:%s\n", storedToken[:8]+"...", name)
		fmt.Printf("\nOr add to the daemon's fleet_tokens config:\n")
		fmt.Printf("  kd config set fleet_token:%s <token-from-secret>\n", name)

		return nil
	},
}

var fleetDeleteCmd = &cobra.Command{
	Use:   "delete <name>",
	Short: "Delete a fleet (namespace + resources + bead)",
	Long: `Delete a fleet by removing its K8s namespace (which cascades to all
resources within it) and closing the fleet bead. Use --force to skip
confirmation.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		force, _ := cmd.Flags().GetBool("force")

		if !force {
			fmt.Printf("This will delete namespace gardenio-%s and all resources within it.\n", name)
			fmt.Printf("Use --force to confirm.\n")
			return fmt.Errorf("deletion requires --force flag")
		}

		ctx := context.Background()
		namespace := "gardenio-" + name

		k8s, err := buildK8sClient()
		if err != nil {
			return fmt.Errorf("building K8s client: %w", err)
		}

		fmt.Printf("deleting namespace %s...\n", namespace)
		err = k8s.CoreV1().Namespaces().Delete(ctx, namespace, metav1.DeleteOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting namespace: %w", err)
		}

		// Close the fleet bead if one exists.
		result, listErr := daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
			Types:  []string{"fleet"},
			Labels: []string{"fleet:" + name},
			Limit:  1,
		})
		if listErr == nil && len(result.Beads) > 0 {
			fmt.Printf("closing fleet bead %s...\n", result.Beads[0].ID)
			if closeErr := daemon.CloseBead(ctx, result.Beads[0].ID, nil); closeErr != nil {
				fmt.Printf("  warning: could not close fleet bead: %v\n", closeErr)
			}
		}

		fmt.Printf("fleet %q deleted\n", name)
		return nil
	},
}

// buildK8sClient creates a Kubernetes clientset from in-cluster config
// or the KUBECONFIG env var / default kubeconfig path.
func buildK8sClient() (*kubernetes.Clientset, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		// Fall back to kubeconfig.
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			home, _ := os.UserHomeDir()
			kubeconfig = home + "/.kube/config"
		}
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			return nil, fmt.Errorf("no K8s config: %w", err)
		}
	}
	return kubernetes.NewForConfig(config)
}

// psaProfile returns the Pod Security Admission profile to stamp on
// fleet namespaces. Defaults to "restricted"; operators can soften via
// the GARDENIO_PSA_PROFILE env var ("baseline" or "privileged") on the
// rare occasions a fleet needs to admit a workload that the agent pod
// securityContext does not already satisfy.
func psaProfile() string {
	switch v := os.Getenv("GARDENIO_PSA_PROFILE"); v {
	case "baseline", "privileged", "restricted":
		return v
	default:
		return "restricted"
	}
}

// fleetNamespaceLabels returns the labels stamped on every fleet
// namespace, including Pod Security Admission labels (defense-in-depth
// against a compromised SA admitting privileged or hostPath pods).
func fleetNamespaceLabels(fleetName, parentFleet string) map[string]string {
	profile := psaProfile()
	return map[string]string{
		"app.kubernetes.io/name":              "gardenio",
		"app.kubernetes.io/managed-by":        "gardenio-fleet",
		"gardenio.io/fleet":                    fleetName,
		"gardenio.io/parent-fleet":             parentFleet,
		"pod-security.kubernetes.io/enforce":  profile,
		"pod-security.kubernetes.io/audit":    profile,
		"pod-security.kubernetes.io/warn":     profile,
		"pod-security.kubernetes.io/enforce-version": "latest",
		"pod-security.kubernetes.io/audit-version":   "latest",
		"pod-security.kubernetes.io/warn-version":    "latest",
	}
}

// createFleetNamespace creates a namespace for the fleet with labels.
func createFleetNamespace(ctx context.Context, k8s kubernetes.Interface, namespace, fleetName, parentFleet string) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   namespace,
			Labels: fleetNamespaceLabels(fleetName, parentFleet),
		},
	}
	_, err := k8s.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		fmt.Printf("  namespace %s already exists, skipping\n", namespace)
		return nil
	}
	return err
}

// fleetResourceQuotaHard is the default ResourceQuota.spec.hard applied
// to every fleet namespace. Mirrors the chart default in
// gardenio/helm/gardenio/values.yaml (resourceQuota.hard) — keep the two
// in sync. Sized to bound a compromised fleet without breaking a
// healthy multi-agent fleet.
//
// See gardenio/docs/audits/namespace-isolation-static.md §F1.
var fleetResourceQuotaHard = corev1.ResourceList{
	corev1.ResourcePods:                   resource.MustParse("200"),
	corev1.ResourceRequestsCPU:            resource.MustParse("200"),
	corev1.ResourceRequestsMemory:         resource.MustParse("1Ti"),
	corev1.ResourcePersistentVolumeClaims: resource.MustParse("200"),
	corev1.ResourceServices:               resource.MustParse("50"),
	corev1.ResourceSecrets:                resource.MustParse("200"),
}

// createFleetResourceQuota stamps a ResourceQuota on the fleet namespace
// to bound pods/CPU/memory/PVCs/services/secrets. Idempotent: a
// pre-existing quota of the same name is left in place so an operator
// can hand-tune a per-fleet quota without `gb fleet create` clobbering
// it on re-run.
func createFleetResourceQuota(ctx context.Context, k8s kubernetes.Interface, namespace string) error {
	quota := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gardenio-fleet-quota",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "gardenio",
				"app.kubernetes.io/managed-by": "gardenio-fleet",
			},
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: fleetResourceQuotaHard,
		},
	}
	_, err := k8s.CoreV1().ResourceQuotas(namespace).Create(ctx, quota, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		fmt.Printf("  resource quota in %s already exists, skipping\n", namespace)
		return nil
	}
	return err
}

// createFleetLimitRange creates a default LimitRange in the fleet
// namespace (audit ka-0000005403 §F2). The fleet `gardenio-agent` Role
// (see createFleetRBAC) grants full pod create rights with no resource
// caps of its own; without a namespace LimitRange, a pod admitted via
// that Role can run uncapped. Mirrors the podmanager defaults at
// gardenio/controller/internal/podmanager/manager.go:53-56 so the happy
// path for podmanager-built pods is unchanged.
func createFleetLimitRange(ctx context.Context, k8s kubernetes.Interface, namespace string) error {
	lr := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gardenio-defaults",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "gardenio",
				"app.kubernetes.io/managed-by": "gardenio-fleet",
			},
		},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type: corev1.LimitTypeContainer,
					Default: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(podmanager.DefaultCPULimit),
						corev1.ResourceMemory: resource.MustParse(podmanager.DefaultMemoryLimit),
					},
					DefaultRequest: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(podmanager.DefaultCPURequest),
						corev1.ResourceMemory: resource.MustParse(podmanager.DefaultMemoryRequest),
					},
					Max: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse(podmanager.DefaultCPULimit),
						corev1.ResourceMemory: resource.MustParse(podmanager.DefaultMemoryLimit),
					},
				},
			},
		},
	}
	_, err := k8s.CoreV1().LimitRanges(namespace).Create(ctx, lr, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		fmt.Printf("  LimitRange %s/gardenio-defaults already exists, skipping\n", namespace)
		return nil
	}
	return err
}


// fleetBaselineNetworkPolicies returns the baseline NetworkPolicies stamped
// into every fleet namespace at creation time (audit ka-0000005403 §F4):
//
//   - default-deny-ingress / default-deny-egress: no pod in the fleet ns
//     receives or originates traffic unless an additional policy allows it.
//   - allow-dns: UDP/TCP 53 to kube-system/kube-dns.
//   - allow-egress-to-parent-beads: TCP 8080 to the parent gardenio ns
//     beads pods.
//   - allow-egress-to-parent-nats: TCP 4222 to the parent gardenio ns
//     NATS pods.
//
// The 21 chart-rendered NetworkPolicies in
// gardenio/helm/gardenio/templates/networkpolicies/networkpolicies.yaml all
// live in .Release.Namespace and do NOT apply to fleet namespaces. Without
// this baseline, pod-to-pod within a fleet ns and to anywhere else in the
// cluster is wide open.
func fleetBaselineNetworkPolicies(namespace, parentNS string) []*networkingv1.NetworkPolicy {
	udp := corev1.ProtocolUDP
	tcp := corev1.ProtocolTCP
	dns53UDP := intstr.FromInt(53)
	beadsHTTP := intstr.FromInt(8080)
	natsClient := intstr.FromInt(4222)
	labels := map[string]string{
		"app.kubernetes.io/name":       "gardenio",
		"app.kubernetes.io/managed-by": "gardenio-fleet",
	}

	return []*networkingv1.NetworkPolicy{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "default-deny-ingress",
				Namespace: namespace,
				Labels:    labels,
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "default-deny-egress",
				Namespace: namespace,
				Labels:    labels,
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "allow-dns",
				Namespace: namespace,
				Labels:    labels,
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"kubernetes.io/metadata.name": "kube-system",
							},
						},
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"k8s-app": "kube-dns"},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &udp, Port: &dns53UDP},
						{Protocol: &tcp, Port: &dns53UDP},
					},
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "allow-egress-to-parent-beads",
				Namespace: namespace,
				Labels:    labels,
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"kubernetes.io/metadata.name": parentNS,
							},
						},
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"app.kubernetes.io/component": "beads",
							},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &tcp, Port: &beadsHTTP},
					},
				}},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "allow-egress-to-parent-nats",
				Namespace: namespace,
				Labels:    labels,
			},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
				Egress: []networkingv1.NetworkPolicyEgressRule{{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"kubernetes.io/metadata.name": parentNS,
							},
						},
						PodSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{
								"app.kubernetes.io/component": "nats",
							},
						},
					}},
					Ports: []networkingv1.NetworkPolicyPort{
						{Protocol: &tcp, Port: &natsClient},
					},
				}},
			},
		},
	}
}

// createFleetNetworkPolicies stamps the baseline NetworkPolicy set into the
// fleet namespace. Idempotent: AlreadyExists is swallowed per policy so an
// operator can safely re-run `gb fleet create` and a hand-tuned policy of
// the same name is not clobbered.
func createFleetNetworkPolicies(ctx context.Context, k8s kubernetes.Interface, namespace, parentNS string) error {
	for _, np := range fleetBaselineNetworkPolicies(namespace, parentNS) {
		_, err := k8s.NetworkingV1().NetworkPolicies(namespace).Create(ctx, np, metav1.CreateOptions{})
		if apierrors.IsAlreadyExists(err) {
			fmt.Printf("  NetworkPolicy %s/%s already exists, skipping\n", namespace, np.Name)
			continue
		}
		if err != nil {
			return fmt.Errorf("creating NetworkPolicy %s: %w", np.Name, err)
		}
	}
	return nil
}

// parentNamespace returns the namespace where the parent gardenio release
// (beads, NATS, controller) is installed. Used as the namespaceSelector
// target for the fleet's allow-egress-to-parent-{beads,nats} policies.
//
// Resolution order:
//  1. BOAT_PARENT_NAMESPACE (explicit override, takes priority)
//  2. POD_NAMESPACE (set by downward API when running inside the controller pod)
//  3. "gardenio" (default release namespace)
func parentNamespace() string {
	return envOr("BOAT_PARENT_NAMESPACE", envOr("POD_NAMESPACE", "gardenio"))
}

// createFleetRBAC creates a ServiceAccount, Role, and RoleBinding in the
// fleet namespace. The Role grants pod/secret/configmap management needed
// for agent operations.
func createFleetRBAC(ctx context.Context, k8s kubernetes.Interface, namespace, saName string) error {
	// ServiceAccount
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "gardenio",
				"app.kubernetes.io/managed-by": "gardenio-fleet",
			},
		},
	}
	if _, err := k8s.CoreV1().ServiceAccounts(namespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating service account: %w", err)
		}
	}

	// Role — minimal verbs needed by the in-namespace agent pod SA.
	// Tightened in F5 (gardenio/docs/audits/namespace-isolation-static.md):
	// only the verbs actually consumed by gardenio/controller/internal/podmanager
	// + secret reads for credential materialisation. Mirrors the mainline
	// agents Role at gardenio/helm/gardenio/templates/controller/rbac.yaml.
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gardenio-agent",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "gardenio",
				"app.kubernetes.io/managed-by": "gardenio-fleet",
			},
		},
		Rules: []rbacv1.PolicyRule{
			// Pod lifecycle: podmanager (create/get/delete), statusreporter+pvcgc (list).
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "list", "create", "delete"},
			},
			// Secret reads only: credential materialisation. Write verbs
			// (list/create/update/patch/delete) intentionally absent — a
			// compromised agent must not be able to tamper with peer
			// agents' mounted credentials.
			{
				APIGroups: []string{""},
				Resources: []string{"secrets"},
				Verbs:     []string{"get"},
			},
			// PVC lifecycle: podmanager (create/get), pvcgc (list/delete).
			{
				APIGroups: []string{""},
				Resources: []string{"persistentvolumeclaims"},
				Verbs:     []string{"get", "list", "create", "delete"},
			},
		},
	}
	if _, err := k8s.RbacV1().Roles(namespace).Create(ctx, role, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating role: %w", err)
		}
	}

	// RoleBinding
	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "gardenio-agent",
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "gardenio",
				"app.kubernetes.io/managed-by": "gardenio-fleet",
			},
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      saName,
				Namespace: namespace,
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     "gardenio-agent",
		},
	}
	if _, err := k8s.RbacV1().RoleBindings(namespace).Create(ctx, rb, metav1.CreateOptions{}); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating rolebinding: %w", err)
		}
	}

	return nil
}

// generateToken creates a cryptographically random hex token for fleet auth.
func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "fleet-" + hex.EncodeToString(b), nil
}

// createFleetTokenSecret stores the fleet bearer token as a K8s Secret.
// Returns the token that is actually stored (which may differ from the input
// token if the secret already exists from a previous partial run).
func createFleetTokenSecret(ctx context.Context, k8s *kubernetes.Clientset, namespace, secretName, token string) (string, error) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       "gardenio",
				"app.kubernetes.io/managed-by": "gardenio-fleet",
				"gardenio.io/secret-type":       "fleet-token",
			},
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"token": token,
		},
	}
	_, err := k8s.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		fmt.Printf("  secret %s already exists, reading existing token\n", secretName)
		existing, getErr := k8s.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
		if getErr != nil {
			return "", fmt.Errorf("reading existing secret: %w", getErr)
		}
		if t, ok := existing.Data["token"]; ok {
			return string(t), nil
		}
		return token, nil
	}
	return token, err
}

func init() {
	fleetCreateCmd.Flags().String("parent", defaultParentFleet(), "parent fleet name")
	fleetCreateCmd.Flags().String("description", "", "human-readable fleet description")
	fleetCreateCmd.Flags().String("max-agents", "", "maximum number of agents in this fleet")
	fleetCreateCmd.Flags().Bool("dry-run", false, "show what would be created without doing it")

	fleetDeleteCmd.Flags().Bool("force", false, "confirm destructive deletion")

	fleetCmd.AddCommand(fleetCreateCmd)
	fleetCmd.AddCommand(fleetDeleteCmd)
}

// defaultParentFleet returns the current fleet name from env. Defaults to "gardenio".
func defaultParentFleet() string {
	return envOr("BOAT_FLEET", "gardenio")
}
