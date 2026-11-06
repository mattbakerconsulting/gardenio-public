package tests

import (
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Lands gate 3 (gd-VtHRlD6gvB): the k3s gVisor installer DaemonSet
// (templates/gvisor-k3s-installer.yaml) and the runsc RuntimeClass scheduling
// it drives (templates/gvisor.yaml). The EKS/Karpenter path must not move.

const k3sInstallerTemplate = "templates/gvisor-k3s-installer.yaml"

func k3sInstallerArgs(extra ...string) []string {
	return append([]string{
		"gvisor.enabled=true",
		"gvisor.k3sInstaller.enabled=true",
	}, extra...)
}

// k3sDocs splits a helm render into objects keyed by kind.
func k3sDocs(t *testing.T, out string) map[string]map[string]any {
	t.Helper()
	docs := map[string]map[string]any{}
	for doc := range strings.SplitSeq(out, "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var o map[string]any
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("unmarshal rendered doc: %v\n%s", err, doc)
		}
		if kind, _ := o["kind"].(string); kind != "" {
			docs[kind] = o
		}
	}
	return docs
}

// k3sDig walks nested maps (string keys) and list indexes (int keys).
func k3sDig(t *testing.T, o any, path ...any) any {
	t.Helper()
	cur := o
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := cur.(map[string]any)
			if !ok {
				t.Fatalf("k3sDig %v: %v is not a map", path, cur)
			}
			cur = m[k]
		case int:
			l, ok := cur.([]any)
			if !ok || k >= len(l) {
				t.Fatalf("k3sDig %v: %v is not a list with index %d", path, cur, k)
			}
			cur = l[k]
		}
	}
	return cur
}

func TestGvisorK3sInstaller_DefaultOff(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, k3sInstallerTemplate)
	rendersNothing(t, out, err)
}

func TestGvisorK3sInstaller_OffWhenOnlyGvisorEnabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, k3sInstallerTemplate, "gvisor.enabled=true")
	rendersNothing(t, out, err)
}

func TestGvisorK3sInstaller_RequiresGvisorEnabled(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, k3sInstallerTemplate, "gvisor.k3sInstaller.enabled=true")
	if err == nil || !strings.Contains(out, "requires gvisor.enabled=true") {
		t.Fatalf("installer without gvisor.enabled must fail the render; err=%v\n%s", err, out)
	}
}

func TestGvisorK3sInstaller_ExcludesKarpenterNodePool(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, k3sInstallerTemplate,
		k3sInstallerArgs(
			"gvisor.nodePool.create=true",
			"gvisor.nodePool.nodeClass.role=node-role",
			"gvisor.nodePool.nodeClass.subnetSelectorTerms[0].tags.k=v",
			"gvisor.nodePool.nodeClass.securityGroupSelectorTerms[0].tags.k=v",
		)...)
	if err == nil || !strings.Contains(out, "are exclusive") {
		t.Fatalf("installer with nodePool.create must fail the render; err=%v\n%s", err, out)
	}
}

func TestGvisorK3sInstaller_RejectsBadSha(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, k3sInstallerTemplate,
		k3sInstallerArgs("gvisor.k3sInstaller.sha512=deadbeef")...)
	if err == nil || !strings.Contains(out, "128-hex sha512") {
		t.Fatalf("a short sha512 must fail the render; err=%v\n%s", err, out)
	}
}

func TestGvisorK3sInstaller_DaemonSet(t *testing.T) {
	t.Parallel()
	docs := k3sDocs(t, helmTemplate(t, k3sInstallerTemplate, k3sInstallerArgs()...))
	ds, ok := docs["DaemonSet"]
	if !ok {
		t.Fatal("installer on must render the DaemonSet")
	}
	if ns := k3sDig(t, ds, "metadata", "namespace"); ns != "kube-system" {
		t.Errorf("DaemonSet namespace = %v, want kube-system (release ns enforces PSA baseline)", ns)
	}
	spec := k3sDig(t, ds, "spec", "template", "spec")
	if k3sDig(t, spec, "hostPID") != true {
		t.Error("installer must run hostPID to nsenter into PID 1")
	}
	if k3sDig(t, spec, "automountServiceAccountToken") != false {
		t.Error("installer needs no API token")
	}
	if sel := k3sDig(t, spec, "nodeSelector", "gardenio.io/role"); sel != "platform" {
		t.Errorf("nodeSelector gardenio.io/role = %v, want platform", sel)
	}
	// Only the platform pool's taint: nothing that would admit it to a server.
	tols := k3sDig(t, spec, "tolerations").([]any)
	tol := tols[0].(map[string]any)
	if len(tols) != 1 || tol["key"] != "gardenio.agent" || tol["effect"] != "NoSchedule" {
		t.Errorf("installer must tolerate only gardenio.agent:NoSchedule, got %v", tols)
	}

	// Never on a k3s server: control-plane nodes are excluded outright.
	exprs := k3sDig(t, spec, "affinity", "nodeAffinity",
		"requiredDuringSchedulingIgnoredDuringExecution", "nodeSelectorTerms", 0, "matchExpressions").([]any)
	var excludesCP bool
	for _, e := range exprs {
		m := e.(map[string]any)
		if m["key"] == "node-role.kubernetes.io/control-plane" && m["operator"] == "DoesNotExist" {
			excludesCP = true
		}
	}
	if !excludesCP {
		t.Errorf("installer must require node-role.kubernetes.io/control-plane DoesNotExist, got %v", exprs)
	}

	c := k3sDig(t, spec, "containers", 0)
	if k3sDig(t, c, "securityContext", "privileged") != true {
		t.Error("installer container must be privileged")
	}
	env := map[string]string{}
	for _, e := range k3sDig(t, c, "env").([]any) {
		m := e.(map[string]any)
		env[m["name"].(string)], _ = m["value"].(string)
	}
	if env["GVISOR_VERSION"] != "20261026.0" {
		t.Errorf("GVISOR_VERSION = %q, want the pinned 20261026.0", env["GVISOR_VERSION"])
	}
	if len(env["GVISOR_SHA512"]) != 128 {
		t.Errorf("GVISOR_SHA512 = %q, want a 128-hex sha512", env["GVISOR_SHA512"])
	}
	wantURL := "https://storage.googleapis.com/gvisor/releases/release/20261026.0/x86_64/gvisor.tar.bz2"
	if env["GVISOR_URL"] != wantURL {
		t.Errorf("GVISOR_URL = %q, want %q", env["GVISOR_URL"], wantURL)
	}
	args := k3sDig(t, c, "args", 0).(string)
	if !strings.Contains(args, "nsenter -t 1 -m -u -i -n") {
		t.Errorf("installer must run the script in the host namespaces, got:\n%s", args)
	}
	probe := strings.Join(toStrings(k3sDig(t, c, "readinessProbe", "exec", "command").([]any)), " ")
	if !strings.Contains(probe, "/proc/1/root/usr/local/bin/runsc") {
		t.Errorf("readiness must check the host's runsc, got %q", probe)
	}
}

func TestGvisorK3sInstaller_ScriptConfigMap(t *testing.T) {
	t.Parallel()
	docs := k3sDocs(t, helmTemplate(t, k3sInstallerTemplate, k3sInstallerArgs()...))
	script, _ := k3sDig(t, docs["ConfigMap"], "data", "install.sh").(string)
	for _, want := range []string{
		"/var/lib/rancher/k3s/agent/etc/containerd/config-v3.toml.d/runsc.toml",
		"[plugins.'io.containerd.cri.v1.runtime'.containerd.runtimes.runsc]",
		`runtime_type = \"io.containerd.runsc.v1\"`,
		"systemctl restart k3s-agent.service",
		"systemctl is-active --quiet k3s.service",
		"sha512 mismatch",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("install.sh missing %q", want)
		}
	}
	// The script is the pod template's checksum, so a script change rolls it.
	ds := docs["DaemonSet"]
	if sum, _ := k3sDig(t, ds, "spec", "template", "metadata", "annotations", "checksum/install-script").(string); len(sum) != 64 {
		t.Errorf("checksum/install-script = %q, want a sha256", sum)
	}
}

func TestGvisorK3sInstaller_NamespaceOverride(t *testing.T) {
	t.Parallel()
	docs := k3sDocs(t, helmTemplate(t, k3sInstallerTemplate,
		k3sInstallerArgs("gvisor.k3sInstaller.namespace=gardenio-node-setup")...))
	for _, kind := range []string{"DaemonSet", "ConfigMap"} {
		if ns := k3sDig(t, docs[kind], "metadata", "namespace"); ns != "gardenio-node-setup" {
			t.Errorf("%s namespace = %v, want the override", kind, ns)
		}
	}
}

// On k3s the RuntimeClass pins runsc pods to the installer's nodes.
func TestGvisorK3sInstaller_RuntimeClassScheduling(t *testing.T) {
	t.Parallel()
	rc := k3sDocs(t, helmTemplate(t, "templates/gvisor.yaml", k3sInstallerArgs()...))["RuntimeClass"]
	if rc == nil {
		t.Fatal("runtimeClass.create defaults true: the RuntimeClass must render")
	}
	if h := k3sDig(t, rc, "handler"); h != "runsc" {
		t.Errorf("handler = %v, want runsc", h)
	}
	sel := k3sDig(t, rc, "scheduling", "nodeSelector").(map[string]any)
	if len(sel) != 1 || sel["gardenio.io/role"] != "platform" {
		t.Errorf("RuntimeClass nodeSelector = %v, want only gardenio.io/role=platform", sel)
	}
	tols := k3sDig(t, rc, "scheduling", "tolerations").([]any)
	if len(tols) != 1 || tols[0].(map[string]any)["key"] != "gardenio.agent" {
		t.Errorf("RuntimeClass tolerations = %v, want only gardenio.agent", tols)
	}
}

// Without the installer the EKS/Karpenter scheduling (gvisor=enabled) is kept.
func TestGvisorK3sInstaller_EKSRuntimeClassUntouched(t *testing.T) {
	t.Parallel()
	rc := k3sDocs(t, helmTemplate(t, "templates/gvisor.yaml", "gvisor.enabled=true"))["RuntimeClass"]
	if sel := k3sDig(t, rc, "scheduling", "nodeSelector", "gvisor"); sel != "enabled" {
		t.Errorf("EKS RuntimeClass nodeSelector gvisor = %v, want enabled", sel)
	}
	tol := k3sDig(t, rc, "scheduling", "tolerations", 0).(map[string]any)
	if tol["key"] != "gvisor" || tol["value"] != "enabled" {
		t.Errorf("EKS RuntimeClass toleration = %v, want gvisor=enabled", tol)
	}
}

func toStrings(l []any) []string {
	out := make([]string, 0, len(l))
	for _, v := range l {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

// The k3s release-deployer rolls deploy-values every 15 min, so once the
// installer is on it must be able to re-apply the RuntimeClass (by name) and
// the kube-system DaemonSet + ConfigMap (by name, no create/delete: installing
// or removing a node-root DaemonSet stays an admin roll). The foreign-namespace
// half is TestReleaseDeployer_ForeignNamespaceObjectsAreGranted/gvisorK3sInstaller.
func TestGvisorK3sInstaller_ReleaseDeployerGrants(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, releaseDeployerRepoRootValues(),
		"releaseDeployer.enabled=true", "gvisor.enabled=true", "gvisor.k3sInstaller.enabled=true")
	var clusterRole, ksRole string
	for doc := range strings.SplitSeq(out, "\n---") {
		if !strings.Contains(doc, "app.kubernetes.io/component: release-deployer") {
			continue
		}
		k := kindLineRE.FindStringSubmatch(doc)
		switch {
		case k == nil:
		case k[1] == "ClusterRole":
			clusterRole = doc
		case k[1] == "Role" && strings.Contains(doc, `namespace: "kube-system"`):
			ksRole = doc
		}
	}
	var named bool
	for _, b := range clusterRoleRuleBlocks(clusterRole) {
		if strings.Contains(b, `"runtimeclasses"`) && strings.Contains(b, `resourceNames: ["runsc"]`) &&
			strings.Contains(b, `"patch"`) && !strings.Contains(b, `"delete"`) {
			named = true
		}
	}
	if !named {
		t.Errorf("release-deployer ClusterRole must grant get/update/patch (no delete) on runtimeclasses named runsc; got:\n%s", clusterRole)
	}
	if ksRole == "" {
		t.Fatal("release-deployer needs a Role in kube-system for the installer objects")
	}
	for _, b := range clusterRoleRuleBlocks(ksRole) {
		if (strings.Contains(b, `"daemonsets"`) || strings.Contains(b, `"configmaps"`)) &&
			(strings.Contains(b, `"create"`) || strings.Contains(b, `"delete"`) || !strings.Contains(b, `"gvisor-k3s-installer"`)) {
			t.Errorf("kube-system grant must be get/update/patch on gvisor-k3s-installer only; got:\n%s", b)
		}
	}
}

// Off (today's deploy-values): no kube-system Role, no runtimeclasses rule.
// Only the release-deployer's own objects are checked: the infra-reader
// ClusterRole reads runtimeclasses, which is not a deployer grant.
func TestGvisorK3sInstaller_ReleaseDeployerGrantsAbsentWhenOff(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, releaseDeployerRepoRootValues(), "releaseDeployer.enabled=true")
	var deployerDocs int
	for doc := range strings.SplitSeq(out, "\n---") {
		if !strings.Contains(doc, "app.kubernetes.io/component: release-deployer") {
			continue
		}
		deployerDocs++
		if strings.Contains(doc, `"runtimeclasses"`) || strings.Contains(doc, `"gvisor-k3s-installer"`) {
			t.Errorf("with the installer off the release-deployer must not gain any gVisor grant; got:\n%s", doc)
		}
	}
	if deployerDocs == 0 {
		t.Fatal("no release-deployer objects in the render; this check controls nothing")
	}
}
