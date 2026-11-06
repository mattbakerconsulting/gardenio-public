package tests

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// The k3s API audit-log shipper (task gd-ZrPUxj8wyU, follow-up to gd-HXRD9yizAE;
// decision gd-bNVBdrmIo5). Operator review of PR #397 (must-fix D): the
// PSA-privileged namespace and the hostPath DaemonSet must NOT be in the gardenio
// Helm release, because the release deployer would then need DaemonSet rights in
// a privileged namespace (root on the control plane) and namespace creation. They
// are operator-applied manifests in helm/k3s-audit-shipper/, like helm/gardenio-
// ziti/. These tests pin both halves: nothing of it in the chart, and the shape
// of the operator-applied manifests.

const auditDir = "../../k3s-audit-shipper/"

func loadDocs(t *testing.T, file string) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(auditDir + file)
	if err != nil {
		t.Fatal(err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	var docs []map[string]any
	for {
		var d map[string]any
		if err := dec.Decode(&d); err != nil {
			break
		}
		if d != nil {
			docs = append(docs, d)
		}
	}
	return docs
}

func get(m any, path ...string) any {
	cur := m
	for _, p := range path {
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = mm[p]
	}
	return cur
}

func kindOf(docs []map[string]any, kind string) map[string]any {
	for _, d := range docs {
		if d["kind"] == kind {
			return d
		}
	}
	return nil
}

// The chart must render nothing of the shipper under any values: no namespace,
// no DaemonSet, no hostPath into the audit dir, and no value to turn it on.
func TestK3sAuditShipper_NotInChart(t *testing.T) {
	t.Parallel()
	for _, flags := range [][]string{
		nil,
		{"vlsingle.enabled=true", "networkPolicies.enabled=true", "k3sAuditShipper.enabled=true"},
	} {
		out := helmTemplate(t, "", flags...)
		for _, needle := range []string{"k3s-audit-shipper", "gardenio-audit", "K3S_AUDIT_", "/var/log/k3s-audit"} {
			if strings.Contains(out, needle) {
				t.Errorf("%q rendered by the chart (flags %v); the shipper is operator-applied (helm/k3s-audit-shipper)", needle, flags)
			}
		}
	}
	vals, err := os.ReadFile("../values.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(vals), "k3sAuditShipper:") {
		t.Error("values.yaml must not carry a k3sAuditShipper block: the release path must not be able to enable it")
	}
}

func TestK3sAuditShipper_Namespace_PSAPrivilegedAndOperatorManaged(t *testing.T) {
	t.Parallel()
	ns := kindOf(loadDocs(t, "namespace.yaml"), "Namespace")
	if ns == nil {
		t.Fatal("namespace.yaml has no Namespace")
	}
	if get(ns, "metadata", "name") != "gardenio-audit" {
		t.Errorf("namespace name = %v", get(ns, "metadata", "name"))
	}
	if get(ns, "metadata", "labels", "pod-security.kubernetes.io/enforce") != "privileged" {
		t.Error("namespace must enforce PSA privileged (hostPath is forbidden by baseline)")
	}
	if get(ns, "metadata", "labels", "app.kubernetes.io/managed-by") == "Helm" {
		t.Error("namespace must not be Helm-managed")
	}
}

func shipperDaemonSet(t *testing.T) map[string]any {
	t.Helper()
	ds := kindOf(loadDocs(t, "shipper.yaml"), "DaemonSet")
	if ds == nil {
		t.Fatal("shipper.yaml has no DaemonSet")
	}
	return ds
}

func TestK3sAuditShipper_DaemonSet_PinnedToControlPlane(t *testing.T) {
	t.Parallel()
	raw, _ := os.ReadFile(auditDir + "shipper.yaml")
	out := string(raw)
	for _, want := range []string{
		"key: node-role.kubernetes.io/control-plane",
		"operator: Exists",
		"effect: NoSchedule",
		"requiredDuringSchedulingIgnoredDuringExecution",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("shipper.yaml missing %q", want)
		}
	}
	if ns := get(shipperDaemonSet(t), "metadata", "namespace"); ns != "gardenio-audit" {
		t.Errorf("DaemonSet namespace = %v", ns)
	}
}

func TestK3sAuditShipper_DaemonSet_HostPathContract(t *testing.T) {
	t.Parallel()
	spec := get(shipperDaemonSet(t), "spec", "template", "spec")
	vols, _ := get(spec, "volumes").([]any)
	paths := map[string]map[string]any{}
	for _, v := range vols {
		hp, _ := get(v, "hostPath").(map[string]any)
		paths[get(v, "name").(string)] = hp
	}
	// The audit DIRECTORY (so rotated backups are visible), and it must pre-exist.
	if a := paths["audit"]; a["path"] != "/var/log/k3s-audit" || a["type"] != "Directory" {
		t.Errorf("audit volume = %v, want hostPath /var/log/k3s-audit type Directory", a)
	}
	if s := paths["state"]; s["path"] != "/var/lib/k3s-audit-shipper" || s["type"] != "DirectoryOrCreate" {
		t.Errorf("state volume = %v", s)
	}
	if len(paths) != 2 {
		t.Errorf("unexpected extra volumes: %v", paths)
	}
	c, _ := get(spec, "containers").([]any)
	mounts, _ := get(c[0], "volumeMounts").([]any)
	for _, m := range mounts {
		if get(m, "name") == "audit" && get(m, "readOnly") != true {
			t.Error("audit mount must be readOnly")
		}
	}
}

func TestK3sAuditShipper_DaemonSet_LeastPrivilege(t *testing.T) {
	t.Parallel()
	spec := get(shipperDaemonSet(t), "spec", "template", "spec")
	if get(spec, "automountServiceAccountToken") != false {
		t.Error("must not mount a ServiceAccount token (it never calls the Kubernetes API)")
	}
	for _, bad := range []string{"hostNetwork", "hostPID", "hostIPC"} {
		if _, ok := spec.(map[string]any)[bad]; ok {
			t.Errorf("pod must not set %s", bad)
		}
	}
	if get(spec, "securityContext", "runAsUser") != 0 { // the audit file is 0600 root
		t.Errorf("runAsUser = %v", get(spec, "securityContext", "runAsUser"))
	}
	c, _ := get(spec, "containers").([]any)
	sc := get(c[0], "securityContext")
	if get(sc, "allowPrivilegeEscalation") != false || get(sc, "readOnlyRootFilesystem") != true || get(sc, "privileged") == true {
		t.Errorf("container securityContext = %v", sc)
	}
	if drop, _ := get(sc, "capabilities", "drop").([]any); len(drop) != 1 || drop[0] != "ALL" {
		t.Errorf("capabilities.drop = %v", drop)
	}
}

// C: the audit log is written to its own VictoriaLogs tenant, never tenant 0.
func TestK3sAuditShipper_DaemonSet_OwnTenantAndCrossNamespaceSink(t *testing.T) {
	t.Parallel()
	spec := get(shipperDaemonSet(t), "spec", "template", "spec")
	c, _ := get(spec, "containers").([]any)
	env := map[string]string{}
	envs, _ := get(c[0], "env").([]any)
	for _, e := range envs {
		if v, ok := get(e, "value").(string); ok {
			env[get(e, "name").(string)] = v
		}
	}
	if acct := env["K3S_AUDIT_VICTORIALOGS_ACCOUNT_ID"]; acct == "" || acct == "0" {
		t.Errorf("K3S_AUDIT_VICTORIALOGS_ACCOUNT_ID = %q: the audit log needs its own, non-default tenant", acct)
	}
	// The shipper is not in the release namespace: the bare Service name would not resolve.
	if ep := env["K3S_AUDIT_VICTORIALOGS_ENDPOINT"]; ep != "http://gardenio-victoria-logs.gardenio.svc:9428" {
		t.Errorf("sink endpoint = %q", ep)
	}
	if env["K3S_AUDIT_LOG_PATH"] != "/var/log/k3s-audit/audit.log" || env["K3S_AUDIT_STATE_PATH"] != "/var/lib/k3s-audit-shipper/state.json" {
		t.Errorf("paths = %q / %q", env["K3S_AUDIT_LOG_PATH"], env["K3S_AUDIT_STATE_PATH"])
	}
}

func TestK3sAuditShipper_EgressPolicy_NoInternet(t *testing.T) {
	t.Parallel()
	np := kindOf(loadDocs(t, "networkpolicy.yaml"), "NetworkPolicy")
	if np == nil {
		t.Fatal("networkpolicy.yaml has no NetworkPolicy")
	}
	raw, _ := os.ReadFile(auditDir + "networkpolicy.yaml")
	if strings.Contains(string(raw), "0.0.0.0/0") || strings.Contains(string(raw), "ipBlock") {
		t.Error("egress must be DNS + vlsingle only")
	}
	if !strings.Contains(string(raw), "port: 9428") {
		t.Error("egress must allow vlsingle :9428")
	}
}

// The ecr pull-secret refresher keeps `ecr` fresh in gardenio-audit, and skips the
// namespace (rather than failing every other namespace's refresh) until it exists.
func TestK3sAuditShipper_ECRRefreshCoversAuditNamespace(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../gardenio-ziti/ecr-token-refresh.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	if !strings.Contains(doc, "namespace: gardenio-audit") || !strings.Contains(doc, "gardenio-e2e-1 gardenio-audit; do") {
		t.Error("ecr-token-refresh.yaml must refresh the ecr secret in gardenio-audit")
	}
	if !strings.Contains(doc, `"$NS" = "gardenio-audit"`) {
		t.Error("a missing gardenio-audit namespace must be skipped, not fail the job")
	}
}

// kubectl parses each "---" document on its own, so a YAML anchor defined in one
// document and aliased in another fails at apply time ("unknown anchor"), while
// yaml.v3's Decoder carries anchors across documents and the tests above pass.
// These manifests are applied by hand, so keep them free of anchors and aliases.
func TestK3sAuditShipper_ManifestsHaveNoYAMLAnchors(t *testing.T) {
	t.Parallel()
	anchor := regexp.MustCompile(`(?m):\s+[&*][A-Za-z_][A-Za-z0-9_-]*\s*$`)
	for _, f := range []string{"namespace.yaml", "shipper.yaml", "networkpolicy.yaml"} {
		raw, err := os.ReadFile(auditDir + f)
		if err != nil {
			t.Fatal(err)
		}
		if m := anchor.Find(raw); m != nil {
			t.Errorf("%s uses a YAML anchor/alias %q; kubectl apply rejects aliases across documents", f, m)
		}
	}
}
