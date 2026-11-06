package tests

// Live-namespace e2e lane (gd-WBz2qt9ADZ): the operator-applied pool manifest
// gardenio/tests/e2e/lane/rbac/namespace-pool.yaml against the chart rendered
// with the lane overlay gardenio/tests/e2e/lane/values-e2e.yaml.
//
// Operator review gd-LXD71HHC8n asked for two things this file proves without
// a cluster:
//
//  1. The runner's Role enumerates core resources instead of "*", so it holds
//     NO verb on resourcequotas or limitranges (the operator's ceiling) and
//     nothing on namespaces. tenantAllows is a `kubectl auth can-i` over the
//     manifest's rules.
//  2. Both pool Namespaces carry pod-security.kubernetes.io/enforce=baseline
//     (warn/audit restricted), and every pod template the overlay renders
//     passes baseline, so the label cannot break the install.
//
// Plus the superset invariant the manifest header states: RBAC refuses to let
// a subject grant what it does not hold, so every rule in every Role the
// overlay renders must be covered by the tenant Role, and every rendered kind
// must be one the tenant can create.
//
// Review gd-h3F6SjVL53 added a third: the overlay's beads-viewer must be
// reachable by its own probes (no agent-pod ack env, bind-all LISTEN_ADDR),
// or `helm --wait` in lane.sh times out on every run.
//
// The operator hold on gd-LvbhX557UD added a fourth: the tenant's
// rbac.authorization.k8s.io rule must name its verbs. verbs ["*"] silently
// includes `escalate` and `bind`, with which the runner could write a Role
// granting update/delete on resourcequotas and limitranges in its own
// namespace, bind it to itself, and lift the ceiling while every direct can-i
// above still answered no. TestE2ELane_TenantRoleCannotEscalateOrBind is that
// can-i; the live positive control (create such a Role as the runner, expect
// Forbidden) is lane.sh assert-tenant.

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

const (
	laneDirFromChart   = "../../tests/e2e/lane"
	lanePoolNamespaces = 2
	laneRunnerSA       = "gardenio-e2e-runner"
	laneRunnerSANS     = "arc-runners"
	laneTenantRole     = "gardenio-e2e-tenant"
)

type laneRule struct {
	APIGroups []string `json:"apiGroups"`
	Resources []string `json:"resources"`
	Verbs     []string `json:"verbs"`
}

type laneSubject struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

type laneRoleRef struct {
	APIGroup string `json:"apiGroup"`
	Kind     string `json:"kind"`
	Name     string `json:"name"`
}

type laneObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name        string            `json:"name"`
		Namespace   string            `json:"namespace"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Rules    []laneRule     `json:"rules"`
	Subjects []laneSubject  `json:"subjects"`
	RoleRef  laneRoleRef    `json:"roleRef"`
	Spec     map[string]any `json:"spec"`
}

func laneDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(chartDir(t), laneDirFromChart)
}

func laneParseDocs(t *testing.T, src string) []laneObject {
	t.Helper()
	var objs []laneObject
	for _, doc := range strings.Split(src, "\n---") {
		doc = strings.TrimSpace(doc)
		if doc == "" || strings.HasPrefix(doc, "---") && strings.TrimSpace(strings.TrimPrefix(doc, "---")) == "" {
			continue
		}
		var o laneObject
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("parse document: %v\n%s", err, doc)
		}
		if o.Kind == "" {
			continue // comment-only leading document
		}
		objs = append(objs, o)
	}
	return objs
}

func lanePoolManifest(t *testing.T) []laneObject {
	t.Helper()
	p := filepath.Join(laneDir(t), "rbac", "namespace-pool.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return laneParseDocs(t, string(b))
}

// laneRender renders the chart exactly as lane.sh install does: release name
// == namespace name, the lane overlay, no cluster.
func laneRender(t *testing.T) []laneObject {
	t.Helper()
	const ns = "gardenio-e2e-0"
	cmd := exec.Command("helm", "template", ns, chartDir(t), "--namespace", ns,
		"-f", filepath.Join(laneDir(t), "values-e2e.yaml"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template with values-e2e.yaml failed: %v\n%s", err, out)
	}
	return laneParseDocs(t, string(out))
}

// tenantAllows is `kubectl auth can-i <verb> <resource>` evaluated over a
// Role's rules the way the RBAC authorizer does: a rule matches when its
// apiGroups, resources and verbs each contain the request or "*".
// Subresources match "<res>/<sub>" or "*/<sub>".
func tenantAllows(rules []laneRule, group, resource, verb string) bool {
	for _, r := range rules {
		if !laneHas(r.APIGroups, group) || !laneHas(r.Verbs, verb) {
			continue
		}
		if laneHas(r.Resources, resource) {
			return true
		}
		if i := strings.Index(resource, "/"); i > 0 && laneHas(r.Resources, "*"+resource[i:]) {
			return true
		}
	}
	return false
}

func laneHas(list []string, want string) bool {
	for _, s := range list {
		if s == "*" || s == want {
			return true
		}
	}
	return false
}

// laneKindResource maps a rendered kind to the (group, resource) a subject
// must hold to create it. Anything the overlay renders that is not here fails
// the superset test, which is the point: a new kind in the overlay is a new
// grant the operator has to see.
var laneKindResource = map[string][2]string{
	"ConfigMap":               {"", "configmaps"},
	"Secret":                  {"", "secrets"},
	"Service":                 {"", "services"},
	"ServiceAccount":          {"", "serviceaccounts"},
	"PersistentVolumeClaim":   {"", "persistentvolumeclaims"},
	"Pod":                     {"", "pods"},
	"Deployment":              {"apps", "deployments"},
	"StatefulSet":             {"apps", "statefulsets"},
	"DaemonSet":               {"apps", "daemonsets"},
	"Job":                     {"batch", "jobs"},
	"CronJob":                 {"batch", "cronjobs"},
	"PodDisruptionBudget":     {"policy", "poddisruptionbudgets"},
	"HorizontalPodAutoscaler": {"autoscaling", "horizontalpodautoscalers"},
	"Lease":                   {"coordination.k8s.io", "leases"},
	"NetworkPolicy":           {"networking.k8s.io", "networkpolicies"},
	"Ingress":                 {"networking.k8s.io", "ingresses"},
	"Role":                    {"rbac.authorization.k8s.io", "roles"},
	"RoleBinding":             {"rbac.authorization.k8s.io", "rolebindings"},
	"ExternalSecret":          {"external-secrets.io", "externalsecrets"},
	// Deliberately absent: ResourceQuota, LimitRange (operator ceiling, see
	// TestE2ELane_OverlayRendersNoQuotaOrLimitRange) and every cluster-scoped
	// kind.
}

func laneTenantRoles(t *testing.T, objs []laneObject) map[string]laneObject {
	t.Helper()
	roles := map[string]laneObject{}
	for _, o := range objs {
		if o.Kind == "Role" && o.Metadata.Name == laneTenantRole {
			roles[o.Metadata.Namespace] = o
		}
	}
	if len(roles) != lanePoolNamespaces {
		t.Fatalf("want %d tenant Roles (one per pool namespace), got %d", lanePoolNamespaces, len(roles))
	}
	return roles
}

func TestE2ELane_PoolManifestIsNamespacedOnly(t *testing.T) {
	t.Parallel()
	objs := lanePoolManifest(t)
	allowed := map[string]bool{"Namespace": true, "ServiceAccount": true, "Role": true,
		"RoleBinding": true, "ResourceQuota": true, "LimitRange": true}
	var namespaces []string
	for _, o := range objs {
		if !allowed[o.Kind] {
			t.Errorf("%s/%s: kind %s must not be in the pool manifest (no ClusterRole, ClusterRoleBinding or Secret)", o.Metadata.Namespace, o.Metadata.Name, o.Kind)
		}
		switch o.Kind {
		case "Namespace":
			namespaces = append(namespaces, o.Metadata.Name)
		case "ServiceAccount":
			if o.Metadata.Name != laneRunnerSA || o.Metadata.Namespace != laneRunnerSANS {
				t.Errorf("unexpected ServiceAccount %s/%s; only %s/%s belongs here", o.Metadata.Namespace, o.Metadata.Name, laneRunnerSANS, laneRunnerSA)
			}
		case "RoleBinding":
			for _, s := range o.Subjects {
				if s.Kind != "ServiceAccount" || s.Name != laneRunnerSA || s.Namespace != laneRunnerSANS {
					t.Errorf("RoleBinding %s/%s binds %+v; only the runner SA may be a subject (no groups, no users)", o.Metadata.Namespace, o.Metadata.Name, s)
				}
			}
		}
	}
	if len(namespaces) != lanePoolNamespaces {
		t.Fatalf("want %d pool namespaces, got %v", lanePoolNamespaces, namespaces)
	}
	// Every pool namespace gets the same four operator objects.
	for _, ns := range namespaces {
		got := map[string]int{}
		for _, o := range objs {
			if o.Metadata.Namespace == ns {
				got[o.Kind]++
			}
		}
		for _, k := range []string{"Role", "RoleBinding", "ResourceQuota", "LimitRange"} {
			if got[k] != 1 {
				t.Errorf("namespace %s: want exactly one %s, got %d", ns, k, got[k])
			}
		}
	}
}

func TestE2ELane_PoolNamespacesEnforcePSSBaseline(t *testing.T) {
	t.Parallel()
	want := map[string]string{
		"pod-security.kubernetes.io/enforce": "baseline",
		"pod-security.kubernetes.io/warn":    "restricted",
		"pod-security.kubernetes.io/audit":   "restricted",
		"gardenio.io/e2e-pool":               "true",
	}
	for _, o := range lanePoolManifest(t) {
		if o.Kind != "Namespace" {
			continue
		}
		for k, v := range want {
			if o.Metadata.Labels[k] != v {
				t.Errorf("Namespace %s: label %s=%q, want %q", o.Metadata.Name, k, o.Metadata.Labels[k], v)
			}
		}
	}
}

// TestE2ELane_TenantRoleCannotTouchCeiling is the offline `kubectl auth can-i`.
// The runner must not be able to read, raise or delete the operator's
// ResourceQuota/LimitRange, relabel the Namespace, or reach anything
// cluster-scoped -- for ANY verb, since a wildcard verb list is what the
// manifest grants elsewhere.
func TestE2ELane_TenantRoleCannotTouchCeiling(t *testing.T) {
	t.Parallel()
	verbs := []string{"get", "list", "watch", "create", "update", "patch", "delete", "deletecollection", "escalate", "bind", "impersonate"}
	denied := [][2]string{
		{"", "resourcequotas"}, {"", "limitranges"}, {"", "namespaces"}, {"", "nodes"},
		{"", "persistentvolumes"}, {"", "pods/eviction"}, {"", "bindings"},
		{"rbac.authorization.k8s.io", "clusterroles"}, {"rbac.authorization.k8s.io", "clusterrolebindings"},
		{"admissionregistration.k8s.io", "validatingadmissionpolicies"},
		{"scheduling.k8s.io", "priorityclasses"}, {"storage.k8s.io", "storageclasses"},
		{"node.k8s.io", "runtimeclasses"}, {"apiextensions.k8s.io", "customresourcedefinitions"},
	}
	for ns, role := range laneTenantRoles(t, lanePoolManifest(t)) {
		for _, r := range role.Rules {
			if laneHas(r.APIGroups, "*") {
				t.Errorf("%s: a rule grants apiGroups [*]; enumerate groups", ns)
			}
			for _, g := range r.APIGroups {
				if g == "" && laneHas(r.Resources, "*") {
					t.Errorf("%s: the core group rule grants resources [*], which includes resourcequotas and limitranges; enumerate them", ns)
				}
			}
		}
		for _, d := range denied {
			for _, v := range verbs {
				if tenantAllows(role.Rules, d[0], d[1], v) {
					t.Errorf("%s: can-i %s %s.%q answered yes; the tenant must hold no verb on it", ns, v, d[1], d[0])
				}
			}
		}
	}
}

// TestE2ELane_TenantRoleCannotEscalateOrBind: the hole behind the direct
// verbs. With `escalate` on roles the runner can create a Role whose rules it
// does not hold; with `bind` on roles/clusterroles it can reference one in a
// RoleBinding. Either lifts the ceiling that every can-i above says is
// closed. Named verbs only in the rbac group, never "*" (which contains both),
// and the offline can-i for escalate/bind must answer no on roles,
// rolebindings and clusterroles. The live counterpart is the positive
// control in lane.sh assert-tenant.
func TestE2ELane_TenantRoleCannotEscalateOrBind(t *testing.T) {
	t.Parallel()
	const rbac = "rbac.authorization.k8s.io"
	for ns, role := range laneTenantRoles(t, lanePoolManifest(t)) {
		for _, r := range role.Rules {
			if !laneHas(r.APIGroups, rbac) {
				continue
			}
			for _, v := range r.Verbs {
				switch v {
				case "*":
					t.Errorf("%s: the %s rule grants verbs [*], which includes escalate and bind; name the verbs", ns, rbac)
				case "escalate", "bind":
					t.Errorf("%s: the %s rule grants %q; the runner must not be able to grant what the tenant withholds", ns, rbac, v)
				}
			}
		}
		for _, res := range []string{"roles", "rolebindings", "clusterroles", "clusterrolebindings"} {
			for _, v := range []string{"escalate", "bind"} {
				if tenantAllows(role.Rules, rbac, res, v) {
					t.Errorf("%s: can-i %s %s.%s answered yes; the ceiling is decorative while this holds", ns, v, res, rbac)
				}
			}
		}
		// helm install still has to create the controller's and the adopt-PVCs
		// Job's Role and RoleBinding with the named verbs.
		for _, res := range []string{"roles", "rolebindings"} {
			for _, v := range []string{"get", "list", "watch", "create", "update", "patch", "delete"} {
				if !tenantAllows(role.Rules, rbac, res, v) {
					t.Errorf("%s: can-i %s %s.%s answered no; helm install needs it", ns, v, res, rbac)
				}
			}
		}
	}
}

// TestE2ELane_TenantRoleCoversTheOverlay: RBAC's escalation check means helm
// install fails unless the runner holds every rule of every Role it creates,
// and the obvious unless it can create every kind the overlay renders.
func TestE2ELane_TenantRoleCoversTheOverlay(t *testing.T) {
	t.Parallel()
	roles := laneTenantRoles(t, lanePoolManifest(t))
	rendered := laneRender(t)
	if len(rendered) == 0 {
		t.Fatal("overlay rendered nothing")
	}
	for ns, tenant := range roles {
		for _, o := range rendered {
			gr, ok := laneKindResource[o.Kind]
			if !ok {
				t.Errorf("%s: overlay renders %s %s, which is not in laneKindResource; add the (group, resource) and make sure the tenant Role grants it", ns, o.Kind, o.Metadata.Name)
				continue
			}
			for _, v := range []string{"get", "list", "create", "update", "patch", "delete"} {
				if !tenantAllows(tenant.Rules, gr[0], gr[1], v) {
					t.Errorf("%s: helm needs %s on %s.%q for %s %s; the tenant Role does not grant it", ns, v, gr[1], gr[0], o.Kind, o.Metadata.Name)
				}
			}
			// Without `bind`, a RoleBinding may only reference a Role whose
			// every rule the runner holds. A rendered RoleBinding that points at
			// a ClusterRole (admin, edit, anything built in) would be refused at
			// install time unless the tenant covered all of it, which it does not
			// (admin reads resourcequotas). Keep the overlay on namespaced Roles.
			if o.Kind == "RoleBinding" && o.RoleRef.Kind != "Role" {
				t.Errorf("%s: rendered RoleBinding %s references %s %s; without bind on clusterroles the tenant cannot create it unless it holds every rule of that ClusterRole", ns, o.Metadata.Name, o.RoleRef.Kind, o.RoleRef.Name)
			}
			if o.Kind != "Role" {
				continue
			}
			for _, rule := range o.Rules {
				for _, g := range rule.APIGroups {
					for _, res := range rule.Resources {
						for _, v := range rule.Verbs {
							if !tenantAllows(tenant.Rules, g, res, v) {
								t.Errorf("%s: rendered Role %s grants %s %s.%q, which the tenant does not hold; RBAC would refuse the install", ns, o.Metadata.Name, v, res, g)
							}
						}
					}
				}
			}
		}
	}
}

func TestE2ELane_OverlayRendersNoQuotaOrLimitRange(t *testing.T) {
	t.Parallel()
	for _, o := range laneRender(t) {
		switch o.Kind {
		case "ResourceQuota", "LimitRange":
			t.Errorf("overlay renders %s %s; the operator's copy in rbac/namespace-pool.yaml is the only one and the runner holds no verb on the kind (set %s.enabled=false in values-e2e.yaml)", o.Kind, o.Metadata.Name, strings.ToLower(o.Kind[:1])+o.Kind[1:])
		}
	}
}

// ── Pod Security "baseline", evaluated statically over rendered templates ──

var (
	pssBaselineCapabilities = map[string]bool{
		"AUDIT_WRITE": true, "CHOWN": true, "DAC_OVERRIDE": true, "FOWNER": true, "FSETID": true,
		"KILL": true, "MKNOD": true, "NET_BIND_SERVICE": true, "SETFCAP": true, "SETGID": true,
		"SETPCAP": true, "SETUID": true, "SYS_CHROOT": true,
	}
	pssBaselineSysctls = map[string]bool{
		"kernel.shm_rmid_forced": true, "net.ipv4.ip_local_port_range": true, "net.ipv4.ip_unprivileged_port_start": true,
		"net.ipv4.tcp_syncookies": true, "net.ipv4.ping_group_range": true, "net.ipv4.ip_local_reserved_ports": true,
		"net.ipv4.tcp_keepalive_time": true, "net.ipv4.tcp_fin_timeout": true, "net.ipv4.tcp_keepalive_intvl": true,
		"net.ipv4.tcp_keepalive_probes": true, "net.ipv4.tcp_rmem": true, "net.ipv4.tcp_wmem": true,
	}
	pssBaselineSELinuxTypes = map[string]bool{"": true, "container_t": true, "container_init_t": true, "container_kvm_t": true, "container_engine_t": true}
)

func laneMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func laneList(v any) []any {
	l, _ := v.([]any)
	return l
}

// lanePodSpec returns the pod spec (and its template annotations) for every
// kind that carries one; nil for kinds that do not.
func lanePodSpec(o laneObject) (map[string]any, map[string]string) {
	var tmpl map[string]any
	switch o.Kind {
	case "Pod":
		return o.Spec, o.Metadata.Annotations
	case "Deployment", "StatefulSet", "DaemonSet", "Job", "ReplicaSet":
		tmpl = laneMap(o.Spec["template"])
	case "CronJob":
		tmpl = laneMap(laneMap(laneMap(o.Spec["jobTemplate"])["spec"])["template"])
	default:
		return nil, nil
	}
	ann := map[string]string{}
	for k, v := range laneMap(laneMap(tmpl["metadata"])["annotations"]) {
		if s, ok := v.(string); ok {
			ann[k] = s
		}
	}
	return laneMap(tmpl["spec"]), ann
}

// pssBaselineViolations lists every field of a pod spec the Pod Security
// Standards "baseline" profile rejects (the enforce label on the pool
// namespaces). Written against the upstream profile table; restricted is a
// superset and is only warn/audit in the pool.
func pssBaselineViolations(spec map[string]any, annotations map[string]string) []string {
	var out []string
	for _, f := range []string{"hostNetwork", "hostPID", "hostIPC"} {
		if b, _ := spec[f].(bool); b {
			out = append(out, f+"=true")
		}
	}
	for _, v := range laneList(spec["volumes"]) {
		vm := laneMap(v)
		if _, ok := vm["hostPath"]; ok {
			out = append(out, "volume "+laneStr(vm["name"])+" is a hostPath")
		}
	}
	psc := laneMap(spec["securityContext"])
	for _, s := range laneList(psc["sysctls"]) {
		if name := laneStr(laneMap(s)["name"]); !pssBaselineSysctls[name] {
			out = append(out, "unsafe sysctl "+name)
		}
	}
	out = append(out, pssSecurityContextViolations("pod", psc)...)
	for k, v := range annotations {
		if strings.HasPrefix(k, "container.apparmor.security.beta.kubernetes.io/") && v != "runtime/default" && !strings.HasPrefix(v, "localhost/") {
			out = append(out, "apparmor annotation "+k+"="+v)
		}
	}
	for _, field := range []string{"initContainers", "containers", "ephemeralContainers"} {
		for _, c := range laneList(spec[field]) {
			cm := laneMap(c)
			name := field + "/" + laneStr(cm["name"])
			sc := laneMap(cm["securityContext"])
			if b, _ := sc["privileged"].(bool); b {
				out = append(out, name+": privileged")
			}
			for _, cap := range laneList(laneMap(sc["capabilities"])["add"]) {
				if !pssBaselineCapabilities[laneStr(cap)] {
					out = append(out, name+": adds capability "+laneStr(cap))
				}
			}
			if pm := laneStr(sc["procMount"]); pm != "" && pm != "Default" {
				out = append(out, name+": procMount="+pm)
			}
			for _, p := range laneList(cm["ports"]) {
				if hp, ok := laneMap(p)["hostPort"]; ok && hp != nil && hp != float64(0) {
					out = append(out, name+": hostPort set")
				}
			}
			out = append(out, pssSecurityContextViolations(name, sc)...)
		}
	}
	return out
}

func pssSecurityContextViolations(where string, sc map[string]any) []string {
	var out []string
	if t := laneStr(laneMap(sc["seccompProfile"])["type"]); t == "Unconfined" {
		out = append(out, where+": seccompProfile Unconfined")
	}
	if t := laneStr(laneMap(sc["appArmorProfile"])["type"]); t == "Unconfined" {
		out = append(out, where+": appArmorProfile Unconfined")
	}
	if se := laneMap(sc["seLinuxOptions"]); se != nil {
		if !pssBaselineSELinuxTypes[laneStr(se["type"])] || laneStr(se["user"]) != "" || laneStr(se["role"]) != "" {
			out = append(out, where+": seLinuxOptions outside baseline")
		}
	}
	if hp, _ := laneMap(sc["windowsOptions"])["hostProcess"].(bool); hp {
		out = append(out, where+": windows hostProcess")
	}
	return out
}

func laneStr(v any) string {
	s, _ := v.(string)
	return s
}

func TestE2ELane_OverlayPodsPassPSSBaseline(t *testing.T) {
	t.Parallel()
	checked := 0
	for _, o := range laneRender(t) {
		spec, ann := lanePodSpec(o)
		if spec == nil {
			continue
		}
		checked++
		for _, v := range pssBaselineViolations(spec, ann) {
			t.Errorf("%s %s would be rejected by pod-security enforce=baseline: %s", o.Kind, o.Metadata.Name, v)
		}
	}
	if checked < 5 {
		t.Fatalf("only %d pod templates found in the overlay render; the stack did not render", checked)
	}
}

// The checker itself must see a violation, or the test above is vacuous.
func TestE2ELane_PSSBaselineCheckerRejectsPrivilegedAndHostPath(t *testing.T) {
	t.Parallel()
	spec := map[string]any{
		"hostNetwork": true,
		"volumes":     []any{map[string]any{"name": "sock", "hostPath": map[string]any{"path": "/var/run/docker.sock"}}},
		"containers": []any{
			map[string]any{"name": "dind", "securityContext": map[string]any{"privileged": true}},
			map[string]any{"name": "tun", "securityContext": map[string]any{"capabilities": map[string]any{"add": []any{"NET_ADMIN", "CHOWN"}}}},
			map[string]any{"name": "ok", "securityContext": map[string]any{"capabilities": map[string]any{"drop": []any{"ALL"}}, "seccompProfile": map[string]any{"type": "RuntimeDefault"}}},
		},
	}
	got := pssBaselineViolations(spec, nil)
	sort.Strings(got)
	want := []string{
		"containers/dind: privileged",
		"containers/tun: adds capability NET_ADMIN",
		"hostNetwork=true",
		"volume sock is a hostPath",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("violations = %q, want %q", got, want)
	}
}

// --- beads-viewer reachability under devAuthBypass (review gd-h3F6SjVL53) ---
//
// The lane runs the viewer with beadsViewer.devAuthBypass (no Keycloak in the
// pool). cmd/beads-viewer/main.go forces LISTEN_ADDR to loopback whenever
// BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD is set, so a viewer carrying that
// env binds 127.0.0.1 and its httpGet probes (pod IP) fail forever; `helm
// upgrade --install --wait` in lane.sh then times out on every run. The ack is
// not needed: the chart's viewer pod sets automountServiceAccountToken: false,
// so dev_auth.go's SA-token prod guard never trips. laneViewerRoutable is the
// offline check; the test below runs it over the rendered overlay, and the
// self-test proves it rejects the two ways to break it.

const (
	laneViewerContainer = "beads-viewer"
	laneViewerBypassEnv = "BEADS_VIEWER_DEV_AUTH_BYPASS"
	laneViewerAckEnv    = "BEADS_VIEWER_DEV_AUTH_BYPASS_ACK_AGENT_POD"
)

// laneViewerRoutable returns every reason the viewer container could not be
// reached by its own httpGet probes: the ack env (forces loopback), a
// LISTEN_ADDR host that is not bind-all, a probe port that is not the listen
// port, or the bypass set while the SA token is mounted (dev_auth.go refuses
// to start, which is a CrashLoop instead of a probe failure but the same
// `--wait` timeout).
func laneViewerRoutable(c map[string]any, automountSAToken bool) []string {
	var problems []string
	env := map[string]string{}
	for _, e := range laneList(c["env"]) {
		m := laneMap(e)
		name, _ := m["name"].(string)
		val, _ := m["value"].(string)
		env[name] = val
	}
	if _, ok := env[laneViewerAckEnv]; ok {
		problems = append(problems, laneViewerAckEnv+" is set: main.go forces LISTEN_ADDR to 127.0.0.1 and the pod-IP probes can never pass")
	}
	listen, ok := env["LISTEN_ADDR"]
	if !ok {
		listen = ":8095" // the binary's default
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		problems = append(problems, "LISTEN_ADDR "+listen+" does not parse as host:port")
	} else if host != "" && host != "0.0.0.0" && host != "::" {
		problems = append(problems, "LISTEN_ADDR "+listen+" binds a specific host; probes hit the pod IP")
	}
	if _, ok := env[laneViewerBypassEnv]; ok && automountSAToken {
		problems = append(problems, laneViewerBypassEnv+" with the SA token mounted: dev_auth.go refuses to start without the ack, and the ack forces loopback")
	}
	ports := map[string]string{}
	for _, p := range laneList(c["ports"]) {
		m := laneMap(p)
		name, _ := m["name"].(string)
		ports[name] = laneNumber(m["containerPort"])
	}
	for _, probe := range []string{"startupProbe", "livenessProbe", "readinessProbe"} {
		get := laneMap(laneMap(c[probe])["httpGet"])
		if get == nil {
			continue
		}
		pp := laneNumber(get["port"])
		if s, ok := get["port"].(string); ok {
			if n, named := ports[s]; named {
				pp = n
			}
		}
		if pp != port {
			problems = append(problems, probe+" httpGet port "+pp+" is not the LISTEN_ADDR port "+port)
		}
	}
	return problems
}

// laneNumber renders a YAML scalar (int, float64 or string) as a string.
func laneNumber(v any) string {
	switch n := v.(type) {
	case int:
		return strconv.Itoa(n)
	case int64:
		return strconv.FormatInt(n, 10)
	case float64:
		return strconv.FormatInt(int64(n), 10)
	case string:
		return n
	}
	return ""
}

func laneViewerContainers(t *testing.T, objs []laneObject) (map[string]any, bool) {
	t.Helper()
	var found map[string]any
	var automount bool
	for _, o := range objs {
		if o.Kind != "Deployment" {
			continue
		}
		spec, _ := lanePodSpec(o)
		for _, c := range laneList(spec["containers"]) {
			m := laneMap(c)
			if m["name"] != laneViewerContainer {
				continue
			}
			if found != nil {
				t.Fatalf("overlay renders more than one %s container", laneViewerContainer)
			}
			found = m
			automount, _ = spec["automountServiceAccountToken"].(bool)
			if _, set := spec["automountServiceAccountToken"]; !set {
				automount = true // the K8s default mounts the token
			}
		}
	}
	if found == nil {
		t.Fatalf("overlay renders no Deployment with a %s container; values-e2e.yaml must keep beadsViewer.enabled", laneViewerContainer)
	}
	return found, automount
}

func TestE2ELane_ViewerIsRoutableWithDevAuthBypass(t *testing.T) {
	t.Parallel()
	c, automount := laneViewerContainers(t, laneRender(t))
	for _, p := range laneViewerRoutable(c, automount) {
		t.Errorf("lane viewer unroutable: %s (values-e2e.yaml must set devAuthBypass only; see review gd-h3F6SjVL53)", p)
	}
	bypass := false
	for _, e := range laneList(c["env"]) {
		if laneMap(e)["name"] == laneViewerBypassEnv {
			bypass = true
		}
	}
	if !bypass {
		t.Errorf("lane viewer renders without %s; the pool has no Keycloak, so OIDC would make the viewer refuse to start", laneViewerBypassEnv)
	}
}

// Rejecting the ack env and a loopback LISTEN_ADDR is the whole point; prove
// the checker sees both and passes the clean shape.
func TestE2ELane_ViewerRoutableCheckerRejectsLoopback(t *testing.T) {
	t.Parallel()
	clean := func() map[string]any {
		return map[string]any{
			"env": []any{
				map[string]any{"name": "LISTEN_ADDR", "value": ":8095"},
				map[string]any{"name": laneViewerBypassEnv, "value": "1"},
			},
			"ports":          []any{map[string]any{"name": "http", "containerPort": 8095}},
			"startupProbe":   map[string]any{"httpGet": map[string]any{"path": "/healthz", "port": "http"}},
			"readinessProbe": map[string]any{"httpGet": map[string]any{"path": "/readyz", "port": "http"}},
		}
	}
	if p := laneViewerRoutable(clean(), false); len(p) != 0 {
		t.Fatalf("clean viewer shape must pass, got %v", p)
	}
	withAck := clean()
	withAck["env"] = append(laneList(withAck["env"]), map[string]any{"name": laneViewerAckEnv, "value": "1"})
	if p := laneViewerRoutable(withAck, false); len(p) == 0 {
		t.Error("checker must reject " + laneViewerAckEnv)
	}
	loopback := clean()
	loopback["env"] = []any{map[string]any{"name": "LISTEN_ADDR", "value": "127.0.0.1:8095"}}
	if p := laneViewerRoutable(loopback, false); len(p) == 0 {
		t.Error("checker must reject a loopback LISTEN_ADDR")
	}
	if p := laneViewerRoutable(clean(), true); len(p) == 0 {
		t.Error("checker must reject the bypass with the SA token mounted")
	}
	wrongPort := clean()
	wrongPort["readinessProbe"] = map[string]any{"httpGet": map[string]any{"path": "/readyz", "port": 9095}}
	if p := laneViewerRoutable(wrongPort, false); len(p) == 0 {
		t.Error("checker must reject a probe on a port the viewer does not listen on")
	}
}
