package tests

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// keycloak.runtime.tls's in-cluster clients (gd-lYPZlslOM7, scope item 6 of
// gd-XRsosVFAau): with the runtime serving https, every in-cluster Keycloak
// client the chart renders dials the https port and trusts the runtime's CA,
// and the http port admits only Traefik and the EDP operator.

const (
	kcClientsHTTPSBase = "https://gardenio-keycloak-runtime.gardenio.svc:8443/auth/realms/gardenio"
	kcClientsCAFile    = "/etc/gardenio/keycloak-runtime-ca/ca.crt"
	kcClientsTLSSecret = "gardenio-keycloak-runtime-tls"
)

// kcInClusterHTTP is any plain-http URL naming the keycloak-runtime Service.
var kcInClusterHTTP = regexp.MustCompile(`http://[a-z0-9.-]*keycloak-runtime[a-z0-9.-]*:8080`)

// renderKCClients renders the whole chart as release gardenio in namespace
// gardenio (the k3s release) and returns its documents, parsed.
func renderKCClients(t *testing.T, args ...string) []map[string]any {
	t.Helper()
	full := append([]string{"template", "gardenio", chartDir(t), "--namespace", "gardenio"}, args...)
	out, err := exec.Command("helm", full...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	var docs []map[string]any
	for _, raw := range strings.Split(string(out), "\n---") {
		var d map[string]any
		if err := yaml.Unmarshal([]byte(raw), &d); err != nil {
			t.Fatalf("parsing a rendered document: %v\n%s", err, raw)
		}
		if d != nil {
			docs = append(docs, d)
		}
	}
	return docs
}

func kcDocName(d map[string]any) string {
	md, _ := d["metadata"].(map[string]any)
	name, _ := md["name"].(string)
	return d["kind"].(string) + "/" + name
}

func kcFindDoc(t *testing.T, docs []map[string]any, kind, name string) map[string]any {
	t.Helper()
	for _, d := range docs {
		if kcDocName(d) == kind+"/"+name {
			return d
		}
	}
	t.Fatalf("%s/%s not rendered", kind, name)
	return nil
}

// kcPodSpec returns the pod spec of a Deployment, Job or CronJob.
func kcPodSpec(d map[string]any) map[string]any {
	spec, _ := d["spec"].(map[string]any)
	if jt, ok := spec["jobTemplate"].(map[string]any); ok {
		spec, _ = jt["spec"].(map[string]any)
	}
	tpl, _ := spec["template"].(map[string]any)
	ps, _ := tpl["spec"].(map[string]any)
	return ps
}

func kcList(m map[string]any, key string) []map[string]any {
	raw, _ := m[key].([]any)
	var out []map[string]any
	for _, r := range raw {
		if e, ok := r.(map[string]any); ok {
			out = append(out, e)
		}
	}
	return out
}

func kcEnv(c map[string]any, name string) (string, bool) {
	for _, e := range kcList(c, "env") {
		if e["name"] == name {
			v, _ := e["value"].(string)
			return v, true
		}
	}
	return "", false
}

// kcCAVolume checks the pod mounts ca.crt alone from the keycloak-runtime TLS
// Secret (never tls.key) at the path the client's CA env names.
func kcCAVolume(t *testing.T, who string, ps, c map[string]any) {
	t.Helper()
	var vol map[string]any
	for _, v := range kcList(ps, "volumes") {
		if v["name"] == "keycloak-runtime-ca" {
			vol = v
		}
	}
	if vol == nil {
		t.Errorf("%s: no keycloak-runtime-ca volume", who)
		return
	}
	sec, _ := vol["secret"].(map[string]any)
	if sec["secretName"] != kcClientsTLSSecret {
		t.Errorf("%s: keycloak-runtime-ca secretName = %v, want %s", who, sec["secretName"], kcClientsTLSSecret)
	}
	items := kcList(sec, "items")
	if len(items) != 1 || items[0]["key"] != "ca.crt" {
		t.Errorf("%s: keycloak-runtime-ca must project ca.crt only, got %v", who, items)
	}
	mounted := false
	for _, m := range kcList(c, "volumeMounts") {
		if m["name"] == "keycloak-runtime-ca" && m["mountPath"] == filepath.Dir(kcClientsCAFile) {
			mounted = true
		}
	}
	if !mounted {
		t.Errorf("%s: keycloak-runtime-ca not mounted at %s", who, filepath.Dir(kcClientsCAFile))
	}
}

// The acceptance check the step asked for: in the k3s render, no in-cluster
// consumer still names http://<keycloak-runtime>:8080. The EDP operator's
// ClusterKeycloak is the exception only while keycloak.runtime.clusterKeycloak.tls
// is off (gd-PhU6jPtSxa): it reads its CA from its own namespace, where the
// first ConfigMap is an admin roll (templates/keycloak-runtime/cluster-keycloak-ca.yaml).
func TestKeycloakClientsTLS_NoInClusterHTTPConsumer(t *testing.T) {
	t.Parallel()
	for _, operatorTLS := range []bool{false, true} {
		docs := renderKCClients(t, "-f", filepath.Join(chartDir(t), k3sOverlay),
			"--set", fmt.Sprintf("keycloak.runtime.clusterKeycloak.tls=%v", operatorTLS))
		for _, d := range docs {
			if d["kind"] == "ClusterKeycloak" && !operatorTLS {
				continue
			}
			raw, err := yaml.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			if m := kcInClusterHTTP.FindString(string(raw)); m != "" {
				t.Errorf("clusterKeycloak.tls=%v: %s still dials the keycloak-runtime over plain http: %s", operatorTLS, kcDocName(d), m)
			}
		}
		ck := kcFindDoc(t, docs, "ClusterKeycloak", "gardenio-keycloak-runtime")
		spec := ck["spec"].(map[string]any)
		u := spec["url"].(string)
		if !operatorTLS {
			if !kcInClusterHTTP.MatchString(u) {
				t.Errorf("ClusterKeycloak url = %q with clusterKeycloak.tls off, want the http port", u)
			}
			if _, ok := spec["caCert"]; ok {
				t.Error("ClusterKeycloak renders caCert with clusterKeycloak.tls off")
			}
			continue
		}
		if want := "https://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8443/auth"; u != want {
			t.Errorf("ClusterKeycloak url = %q, want %q", u, want)
		}
		ref, _ := spec["caCert"].(map[string]any)["configMapKeyRef"].(map[string]any)
		if ref["name"] != kcOperatorCAConfigMap || ref["key"] != "ca.crt" {
			t.Errorf("ClusterKeycloak caCert = %v, want configMapKeyRef %s/ca.crt", spec["caCert"], kcOperatorCAConfigMap)
		}
	}
}

const kcOperatorCAConfigMap = "gardenio-keycloak-runtime-ca-bundle"

// kcConfigMaps returns the rendered ConfigMaps named name, keyed by namespace.
func kcConfigMaps(docs []map[string]any, name string) map[string]map[string]any {
	out := map[string]map[string]any{}
	for _, d := range docs {
		if kcDocName(d) != "ConfigMap/"+name {
			continue
		}
		ns, _ := d["metadata"].(map[string]any)["namespace"].(string)
		out[ns] = d
	}
	return out
}

// The CA the operator trusts lands in ITS namespace (operators), never empty:
// helm template has no lookup, so with no caCert value nothing is rendered,
// and caCert stands in for the leaf Secret's ca.crt.
func TestKeycloakClientsTLS_OperatorCAConfigMap(t *testing.T) {
	t.Parallel()
	k3s := filepath.Join(chartDir(t), k3sOverlay)
	if cms := kcConfigMaps(renderKCClients(t, "-f", k3s, "--set", "keycloak.runtime.clusterKeycloak.tls=false"), kcOperatorCAConfigMap); len(cms) != 0 {
		t.Errorf("CA ConfigMap rendered with clusterKeycloak.tls off: %v", cms)
	}
	if cms := kcConfigMaps(renderKCClients(t, "-f", k3s, "--set", "keycloak.runtime.clusterKeycloak.tls=true"), kcOperatorCAConfigMap); len(cms) != 0 {
		t.Errorf("CA ConfigMap rendered with no CA to put in it (lookup is empty under helm template): %v", cms)
	}

	pem := "-----BEGIN CERTIFICATE-----\nMIIBtest\n-----END CERTIFICATE-----"
	cms := kcConfigMaps(renderKCClients(t, "-f", k3s, "--set", "keycloak.runtime.clusterKeycloak.tls=true",
		"--set-string", "keycloak.runtime.clusterKeycloak.caCert="+pem), kcOperatorCAConfigMap)
	cm, ok := cms["operators"]
	if len(cms) != 1 || !ok {
		t.Fatalf("want the CA ConfigMap in operators only, got namespaces %v", cms)
	}
	data, _ := cm["data"].(map[string]any)
	if got := strings.TrimSpace(data["ca.crt"].(string)); got != pem {
		t.Errorf("ca.crt = %q, want %q", got, pem)
	}
	ann, _ := cm["metadata"].(map[string]any)["annotations"].(map[string]any)
	if ann["helm.sh/resource-policy"] != "keep" {
		t.Errorf("CA ConfigMap annotations = %v, want helm.sh/resource-policy: keep (the deployer cannot re-create it)", ann)
	}
}

// clusterKeycloak.tls without the runtime serving https is a config error.
func TestKeycloakClientsTLS_OperatorTLSNeedsRuntimeTLS(t *testing.T) {
	t.Parallel()
	args := []string{"template", "gardenio", chartDir(t), "--namespace", "gardenio", "-f", filepath.Join(chartDir(t), k3sOverlay),
		"--set", "keycloak.runtime.clusterKeycloak.tls=true", "--set", "keycloak.runtime.tls.enabled=false"}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err == nil || !strings.Contains(string(out), "clusterKeycloak.tls needs keycloak.runtime.tls.enabled") {
		t.Errorf("want a render failure naming keycloak.runtime.tls.enabled, got err=%v\n%s", err, out)
	}
}

// Every machine-principal client and the beads daemon's discovery dial the
// https port and trust the CA, in the k3s render.
func TestKeycloakClientsTLS_ConsumersTrustTheCA(t *testing.T) {
	t.Parallel()
	docs := renderKCClients(t, "-f", filepath.Join(chartDir(t), k3sOverlay))
	tokenURL := kcClientsHTTPSBase + "/protocol/openid-connect/token"
	cases := []struct {
		kind, name, urlEnv, want, caEnv string
	}{
		{"Deployment", "gardenio-agents", "BEADS_OIDC_TOKEN_URL", tokenURL, "BEADS_OIDC_CA_FILE"},
		{"Deployment", "gardenio-slack-bridge", "BEADS_OIDC_TOKEN_URL", tokenURL, "BEADS_OIDC_CA_FILE"},
		{"Deployment", "gardenio-grafana-bridge", "BEADS_OIDC_TOKEN_URL", tokenURL, "BEADS_OIDC_CA_FILE"},
		{"Deployment", "gardenio-beads-viewer", "BEADS_OIDC_TOKEN_URL", tokenURL, "BEADS_OIDC_CA_FILE"},
		{"Deployment", "gardenio-trigger-engine", "BEADS_OIDC_TOKEN_URL", tokenURL, "BEADS_OIDC_CA_FILE"},
		{"Deployment", "gardenio-coopmux", "BEADS_OIDC_TOKEN_URL", tokenURL, "BEADS_OIDC_CA_FILE"},
		{"CronJob", "gardenio-release-deployer", "KD_OIDC_TOKEN_URL", tokenURL, "KD_OIDC_CA_FILE"},
		{"Deployment", "gardenio-beads", "BEADS_OIDC_DISCOVERY_URL", kcClientsHTTPSBase, "BEADS_OIDC_CA_FILE"},
	}
	for _, tc := range cases {
		d := kcFindDoc(t, docs, tc.kind, tc.name)
		ps := kcPodSpec(d)
		found := false
		for _, c := range kcList(ps, "containers") {
			u, ok := kcEnv(c, tc.urlEnv)
			if !ok {
				continue
			}
			found = true
			if u != tc.want {
				t.Errorf("%s %s = %q, want %q", tc.name, tc.urlEnv, u, tc.want)
			}
			if ca, _ := kcEnv(c, tc.caEnv); ca != kcClientsCAFile {
				t.Errorf("%s %s = %q, want %s", tc.name, tc.caEnv, ca, kcClientsCAFile)
			}
			kcCAVolume(t, tc.name, ps, c)
		}
		if !found {
			t.Errorf("%s renders no %s (is its machine principal / OIDC still on in the k3s overlay?)", tc.name, tc.urlEnv)
		}
	}
}

// inClusterClients=false is the rollback lever: the clients go back to the
// http port and mount no CA, while the Lands provisioner keeps https.
func TestKeycloakClientsTLS_OptOutRestoresHTTP(t *testing.T) {
	t.Parallel()
	docs := renderKCClients(t, "-f", filepath.Join(chartDir(t), k3sOverlay), "--set", "keycloak.runtime.tls.inClusterClients=false")
	d := kcFindDoc(t, docs, "Deployment", "gardenio-slack-bridge")
	c := kcList(kcPodSpec(d), "containers")[0]
	want := "http://gardenio-keycloak-runtime.gardenio.svc:8080/auth/realms/gardenio/protocol/openid-connect/token"
	if u, _ := kcEnv(c, "BEADS_OIDC_TOKEN_URL"); u != want {
		t.Errorf("slack-bridge BEADS_OIDC_TOKEN_URL = %q, want %q", u, want)
	}
	if _, ok := kcEnv(c, "BEADS_OIDC_CA_FILE"); ok {
		t.Error("slack-bridge renders BEADS_OIDC_CA_FILE with inClusterClients=false")
	}
	job := kcFindDoc(t, docs, "Job", "gardenio-keycloak-runtime-first-admin")
	jc := kcList(kcPodSpec(job), "containers")[0]
	if s, _ := kcEnv(jc, "KC_SERVER"); !strings.HasPrefix(s, "http://") || !strings.Contains(s, ":8080/") {
		t.Errorf("first-admin KC_SERVER = %q, want the http port", s)
	}
	ctl := kcFindDoc(t, docs, "Deployment", "gardenio-agents")
	for _, cc := range kcList(kcPodSpec(ctl), "containers") {
		if u, ok := kcEnv(cc, "LANDS_KEYCLOAK_URL"); ok && !strings.HasPrefix(u, "https://") {
			t.Errorf("LANDS_KEYCLOAK_URL = %q: the provisioner must stay on https", u)
		}
	}
}

// The first-admin kcadm Job dials https and loads cert-manager's
// truststore.p12 (kcadm reads no PEM truststore); the leaf Certificate asks
// cert-manager for it.
func TestKeycloakClientsTLS_FirstAdminTruststore(t *testing.T) {
	t.Parallel()
	docs := renderKCClients(t, "-f", filepath.Join(chartDir(t), k3sOverlay))
	job := kcFindDoc(t, docs, "Job", "gardenio-keycloak-runtime-first-admin")
	ps := kcPodSpec(job)
	c := kcList(ps, "containers")[0]
	if s, _ := kcEnv(c, "KC_SERVER"); s != "https://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8443/auth" {
		t.Errorf("first-admin KC_SERVER = %q", s)
	}
	if s, _ := kcEnv(c, "TRUSTSTORE_FILE"); s != "/etc/keycloak-runtime-ca/truststore.p12" {
		t.Errorf("first-admin TRUSTSTORE_FILE = %q", s)
	}
	cmd, _ := yaml.Marshal(c["command"])
	if !strings.Contains(string(cmd), `kcadm config truststore --trustpass "$TRUSTSTORE_PASSWORD" "$TRUSTSTORE_FILE"`) {
		t.Error("first-admin script does not configure the kcadm truststore")
	}
	for _, v := range kcList(ps, "volumes") {
		if v["name"] != "keycloak-runtime-ca" {
			continue
		}
		sec := v["secret"].(map[string]any)
		items := kcList(sec, "items")
		if sec["secretName"] != kcClientsTLSSecret || len(items) != 1 || items[0]["key"] != "truststore.p12" {
			t.Errorf("first-admin CA volume must project truststore.p12 only from %s, got %v", kcClientsTLSSecret, sec)
		}
		if opt, _ := sec["optional"].(bool); opt {
			t.Error("first-admin CA volume must not be optional (the Job waits for cert-manager)")
		}
	}
	leaf := kcFindDoc(t, docs, "Certificate", kcClientsTLSSecret)
	ks, _ := leaf["spec"].(map[string]any)["keystores"].(map[string]any)
	p12, _ := ks["pkcs12"].(map[string]any)
	ref, _ := p12["passwordSecretRef"].(map[string]any)
	if p12["create"] != true || ref["name"] != "gardenio-keycloak-runtime-truststore-password" {
		t.Errorf("leaf Certificate keystores = %v, want pkcs12.create with the truststore password Secret", ks)
	}
	kcFindDoc(t, docs, "Secret", "gardenio-keycloak-runtime-truststore-password")
}

// kcIngressRules returns the keycloak-runtime ingress rules as port -> peers
// (each peer's podSelector component label, beads selector labels or
// "namespaceSelector").
func kcIngressRules(t *testing.T, docs []map[string]any) map[float64][]string {
	t.Helper()
	np := kcFindDoc(t, docs, "NetworkPolicy", "gardenio-keycloak-runtime-ingress")
	rules := map[float64][]string{}
	for _, r := range kcList(np["spec"].(map[string]any), "ingress") {
		from := kcList(r, "from")
		if len(from) == 0 {
			t.Fatalf("keycloak-runtime ingress has a rule with no from (admits every source): %v", r)
		}
		for _, p := range kcList(r, "ports") {
			port, _ := p["port"].(float64)
			for _, f := range from {
				peer := "namespaceSelector"
				if ps, ok := f["podSelector"].(map[string]any); ok {
					ml, _ := ps["matchLabels"].(map[string]any)
					peer, _ = ml["app.kubernetes.io/component"].(string)
				}
				rules[port] = append(rules[port], peer)
			}
		}
	}
	return rules
}

func kcHas(peers []string, want string) bool {
	for _, p := range peers {
		if p == want {
			return true
		}
	}
	return false
}

// kcMachineHTTPSPeers are the components that reach the https port for a
// machine-principal token fetch in the k3s render (gd-PhU6jPtSxa); agents is
// the controller. github-bridge needs kcGithubBridgeOn: the overlay does not
// enable the bridge itself.
var kcMachineHTTPSPeers = []string{"agents", "slack-bridge", "grafana-bridge", "beads-viewer", "trigger-engine", "coopmux", "github-bridge", "release-deployer"}

const kcGithubBridgeOn = "githubBridge.enabled=true"

// The NetworkPolicy follows the clients: with them on https, the beads
// daemon, the first-admin Job, the controller and the other machine-principal
// consumers reach 8443 and the http port keeps only Traefik and the EDP
// operator (until clusterKeycloak.tls moves the operator to 8443 too).
func TestKeycloakClientsTLS_NetworkPolicyPeers(t *testing.T) {
	t.Parallel()
	np := "networkPolicies.enabled=true,networkPolicies.keycloakOperator.namespaceSelector.name=operators"
	rules := kcIngressRules(t, renderKCClients(t, "-f", filepath.Join(chartDir(t), k3sOverlay), "--set", np,
		"--set", "keycloak.runtime.clusterKeycloak.tls=false", "--set", kcGithubBridgeOn))
	for _, peer := range append([]string{"beads", "keycloak-runtime-first-admin"}, kcMachineHTTPSPeers...) {
		if !kcHas(rules[8443], peer) {
			t.Errorf("8443 does not admit %s: %v", peer, rules[8443])
		}
	}
	for _, peer := range []string{"forwardauth", "beads", "keycloak-runtime-first-admin", "agents"} {
		if kcHas(rules[8080], peer) {
			t.Errorf("http 8080 still admits %s: %v", peer, rules[8080])
		}
	}
	if !kcHas(rules[8080], "namespaceSelector") {
		t.Errorf("http 8080 lost the EDP operator (its ClusterKeycloak is still http): %v", rules[8080])
	}
	if kcHas(rules[8443], "namespaceSelector") {
		t.Errorf("8443 admits the EDP operator with clusterKeycloak.tls off: %v", rules[8443])
	}

	// clusterKeycloak.tls: the operator moves to 8443 and the http port no
	// longer admits it.
	rules = kcIngressRules(t, renderKCClients(t, "-f", filepath.Join(chartDir(t), k3sOverlay), "--set", np,
		"--set", "keycloak.runtime.clusterKeycloak.tls=true"))
	if kcHas(rules[8080], "namespaceSelector") {
		t.Errorf("clusterKeycloak.tls: http 8080 still admits the EDP operator: %v", rules[8080])
	}
	if !kcHas(rules[8443], "namespaceSelector") {
		t.Errorf("clusterKeycloak.tls: 8443 does not admit the EDP operator: %v", rules[8443])
	}

	rules = kcIngressRules(t, renderKCClients(t, "-f", filepath.Join(chartDir(t), k3sOverlay), "--set", np,
		"--set", "keycloak.runtime.tls.inClusterClients=false", "--set", "keycloak.runtime.clusterKeycloak.tls=false"))
	for _, peer := range []string{"forwardauth", "beads", "keycloak-runtime-first-admin", "namespaceSelector"} {
		if !kcHas(rules[8080], peer) {
			t.Errorf("inClusterClients=false: 8080 does not admit %s: %v", peer, rules[8080])
		}
	}
}

// Ingress alone is not enough under default-deny egress: each consumer of the
// https port also needs an egress rule to keycloak-runtime:8443 in a policy
// that selects its pod (gd-PhU6jPtSxa).
func TestKeycloakClientsTLS_MachineConsumersEgress(t *testing.T) {
	t.Parallel()
	docs := renderKCClients(t, "-f", filepath.Join(chartDir(t), k3sOverlay),
		"--set", "networkPolicies.enabled=true,networkPolicies.defaultDenyEgress=true", "--set", kcGithubBridgeOn)
	reaches := map[string]bool{}
	for _, d := range docs {
		if d["kind"] != "NetworkPolicy" {
			continue
		}
		spec, _ := d["spec"].(map[string]any)
		ps, _ := spec["podSelector"].(map[string]any)
		ml, _ := ps["matchLabels"].(map[string]any)
		comp, _ := ml["app.kubernetes.io/component"].(string)
		if comp == "" {
			continue
		}
		for _, r := range kcList(spec, "egress") {
			toKC := false
			for _, to := range kcList(r, "to") {
				tps, _ := to["podSelector"].(map[string]any)
				tml, _ := tps["matchLabels"].(map[string]any)
				if tml["app.kubernetes.io/component"] == "keycloak-runtime" {
					toKC = true
				}
			}
			for _, p := range kcList(r, "ports") {
				if toKC && p["port"] == float64(8443) {
					reaches[comp] = true
				}
			}
		}
	}
	for _, comp := range kcMachineHTTPSPeers {
		if !reaches[comp] {
			t.Errorf("%s has no egress rule to keycloak-runtime:8443", comp)
		}
	}
}
