package tests

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The Lands provisioner's Keycloak URL (gd-XRsosVFAau, which removed the
// in-cluster http exception of decision gd-F060vJkxoM): it defaults to the
// keycloak-runtime Service over in-cluster TLS, the controller trusts the
// runtime's CA, and the render accepts https only, as keycloakadmin does.

const (
	landsKCHTTPSURL = "https://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8443/auth"
	// landsProvNoURL is landsProvOn without keycloak.url.
	landsProvNoURL = "agents.enabled=true,agents.lands.provisioner.enabled=true," +
		"agents.lands.provisioner.githubApp.appId=4242," +
		"agents.lands.provisioner.accountTemplate=gd-template"
	landsKCRuntimeTLS = "keycloak.runtime.enabled=true,keycloak.runtime.tls.enabled=true"
)

// renderLandsKC renders as release gardenio in namespace gardenio, the k3s
// release.
func renderLandsKC(t *testing.T, showOnly string, wantErr bool, args ...string) string {
	t.Helper()
	full := append([]string{"template", "gardenio", chartDir(t), "--namespace", "gardenio", "--show-only", showOnly}, args...)
	out, err := exec.Command("helm", full...).CombinedOutput()
	if wantErr && err == nil {
		t.Fatalf("expected helm template to FAIL, but it succeeded; got:\n%s", out)
	}
	if !wantErr && err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

func landsKCEnv(t *testing.T, out, name string) (string, bool) {
	t.Helper()
	key := "- name: " + name + "\n"
	i := strings.Index(out, key)
	if i < 0 {
		return "", false
	}
	line := strings.SplitN(out[i+len(key):], "\n", 2)[0]
	return strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "value:")), `"`), true
}

func landsKCURLEnv(t *testing.T, out string) string {
	t.Helper()
	v, ok := landsKCEnv(t, out, "LANDS_KEYCLOAK_URL")
	if !ok {
		t.Fatalf("LANDS_KEYCLOAK_URL not rendered:\n%s", out)
	}
	return v
}

// The http exception is gone from both checks: neither keycloakadmin nor the
// chart names an http host any more.
func TestLandsKeycloakURL_NoHTTPException(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(filepath.Join(chartDir(t), "..", "..", "controller", "internal", "keycloakadmin", "keycloakadmin.go"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(src), "InClusterKeycloakHost") {
		t.Error("keycloakadmin.go still declares InClusterKeycloakHost")
	}
	tpl, err := os.ReadFile(filepath.Join(chartDir(t), "templates", "_helpers.tpl"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tpl), "adminInClusterHost") {
		t.Error("_helpers.tpl still defines gardenio.keycloak.adminInClusterHost")
	}
}

func TestLandsKeycloakURL_DefaultsToInClusterHTTPS(t *testing.T) {
	t.Parallel()
	out := renderLandsKC(t, "templates/controller/deployment.yaml", false,
		"--set", landsProvNoURL, "--set", landsKCRuntimeTLS)
	if got := landsKCURLEnv(t, out); got != landsKCHTTPSURL {
		t.Errorf("LANDS_KEYCLOAK_URL = %q, want %q", got, landsKCHTTPSURL)
	}
	if got, _ := landsKCEnv(t, out, "LANDS_KEYCLOAK_CA_FILE"); got != "/etc/gardenio/keycloak-ca/ca.crt" {
		t.Errorf("LANDS_KEYCLOAK_CA_FILE = %q, want /etc/gardenio/keycloak-ca/ca.crt", got)
	}
}

// The k3s overlay derives the in-cluster https URL with no keycloak.url typed
// in, and in both namespaces the chart tests and the cluster render it in.
func TestLandsKeycloakURL_ProdOverlayDefault(t *testing.T) {
	t.Parallel()
	overlay := filepath.Join(chartDir(t), "..", "..", "..", "deploy-values", "gardenio-k3s.yaml")
	out := renderLandsKC(t, "templates/controller/deployment.yaml", false, "-f", overlay)
	if got := landsKCURLEnv(t, out); got != landsKCHTTPSURL {
		t.Errorf("LANDS_KEYCLOAK_URL = %q, want %q", got, landsKCHTTPSURL)
	}
	if _, ok := landsKCEnv(t, out, "LANDS_KEYCLOAK_CA_FILE"); !ok {
		t.Error("the k3s overlay renders no LANDS_KEYCLOAK_CA_FILE")
	}
	out2, err := exec.Command("helm", "template", "test", chartDir(t), "--values", overlay,
		"--show-only", "templates/controller/deployment.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("release test, namespace default: %v\n%s", err, out2)
	}
}

func TestLandsKeycloakURL_Accepted(t *testing.T) {
	t.Parallel()
	for _, u := range []string{
		"https://auth.example.com/auth",
		"https://gardenio-keycloak-runtime.gardenio.svc:8443/auth",
		landsKCHTTPSURL,
	} {
		out := renderLandsKC(t, "templates/controller/deployment.yaml", false,
			"--set", landsProvNoURL, "--set", "agents.lands.provisioner.keycloak.url="+u)
		if got := landsKCURLEnv(t, out); got != u {
			t.Errorf("LANDS_KEYCLOAK_URL = %q, want %q", got, u)
		}
	}
}

func TestLandsKeycloakURL_Refused(t *testing.T) {
	t.Parallel()
	for _, u := range []string{
		"http://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8080/auth",
		"http://gardenio-keycloak-runtime.gardenio.svc.cluster.local:8443/auth",
		"http://keycloak:8080",
		"http://auth.example.com/auth",
		"https://u:p@auth.example.com/auth",
		"https://auth.example.com/auth?x=1",
		"auth.example.com",
	} {
		out := renderLandsKC(t, "templates/controller/deployment.yaml", true,
			"--set", landsProvNoURL, "--set", "agents.lands.provisioner.keycloak.url="+u)
		if !strings.Contains(out, "must be an https URL (keycloakadmin refuses anything else)") {
			t.Errorf("url %q: want the https-only error, got:\n%s", u, out)
		}
	}
}

// The runtime without tls has no https URL to default to: the render asks
// for keycloak.url instead of falling back to http.
func TestLandsKeycloakURL_RuntimeWithoutTLSNoURLFails(t *testing.T) {
	t.Parallel()
	for _, set := range []string{"keycloak.runtime.enabled=false", "keycloak.runtime.enabled=true"} {
		out := renderLandsKC(t, "templates/controller/deployment.yaml", true, "--set", landsProvNoURL, "--set", set)
		if !strings.Contains(out, "agents.lands.provisioner.keycloak.url is required") {
			t.Errorf("%s: want the required error, got:\n%s", set, out)
		}
	}
}

// The controller mounts ca.crt alone from the runtime's TLS Secret, never
// tls.key, and only while the runtime serves tls.
func TestLandsKeycloakURL_ControllerMountsCAOnly(t *testing.T) {
	t.Parallel()
	out := renderLandsKC(t, "templates/controller/deployment.yaml", false,
		"--set", landsProvNoURL, "--set", landsKCRuntimeTLS)
	i := strings.Index(out, "- name: lands-keycloak-ca\n          secret:")
	if i < 0 {
		t.Fatalf("no lands-keycloak-ca volume:\n%s", out)
	}
	vol := out[i:]
	vol = vol[:strings.Index(vol, "path: ca.crt")+len("path: ca.crt")]
	for _, want := range []string{`secretName: "gardenio-keycloak-runtime-tls"`, "- key: ca.crt"} {
		if !strings.Contains(vol, want) {
			t.Errorf("lands-keycloak-ca volume lacks %q:\n%s", want, vol)
		}
	}
	if strings.Contains(vol, "tls.key") {
		t.Errorf("the controller must not get tls.key:\n%s", vol)
	}

	off := renderLandsKC(t, "templates/controller/deployment.yaml", false,
		"--set", landsProvNoURL, "--set", "agents.lands.provisioner.keycloak.url=https://auth.example.com/auth")
	if strings.Contains(off, "lands-keycloak-ca") || strings.Contains(off, "LANDS_KEYCLOAK_CA_FILE") {
		t.Error("no runtime tls: the controller still gets the Keycloak CA")
	}
}

// keycloak.runtime.tls renders the namespaced CA chain and a leaf for every
// Service name, Keycloak serves https from it with the management interface
// kept on http, and the Service exposes the https port.
func TestKeycloakRuntimeTLS_Renders(t *testing.T) {
	t.Parallel()
	tls := renderLandsKC(t, "templates/keycloak-runtime/tls.yaml", false, "--set", landsKCRuntimeTLS)
	for _, want := range []string{
		"kind: Issuer", "selfSigned: {}", "isCA: true", `secretName: "gardenio-keycloak-runtime-ca"`,
		`secretName: "gardenio-keycloak-runtime-tls"`,
		`- "gardenio-keycloak-runtime"`,
		`- "gardenio-keycloak-runtime.gardenio"`,
		`- "gardenio-keycloak-runtime.gardenio.svc"`,
		`- "gardenio-keycloak-runtime.gardenio.svc.cluster.local"`,
	} {
		if !strings.Contains(tls, want) {
			t.Errorf("tls.yaml lacks %q:\n%s", want, tls)
		}
	}
	if strings.Contains(tls, "kind: ClusterIssuer") {
		t.Error("tls.yaml must stay namespaced (the release deployer has no ClusterIssuer write)")
	}

	sts := renderLandsKC(t, "templates/keycloak-runtime/statefulset.yaml", false, "--set", landsKCRuntimeTLS)
	for name, want := range map[string]string{
		"KC_HTTPS_PORT":                 "8443",
		"KC_HTTPS_CERTIFICATE_FILE":     "/opt/keycloak/conf/tls/tls.crt",
		"KC_HTTPS_CERTIFICATE_KEY_FILE": "/opt/keycloak/conf/tls/tls.key",
		"KC_HTTP_MANAGEMENT_SCHEME":     "http",
		"KC_HTTP_ENABLED":               "true",
	} {
		if got, _ := landsKCEnv(t, sts, name); got != want {
			t.Errorf("statefulset %s = %q, want %q", name, got, want)
		}
	}
	if !strings.Contains(sts, "- name: https\n              containerPort: 8443") {
		t.Errorf("statefulset has no https containerPort:\n%s", sts)
	}

	svc := renderLandsKC(t, "templates/keycloak-runtime/service.yaml", false, "--set", landsKCRuntimeTLS)
	if !strings.Contains(svc, "- port: 8443\n      targetPort: https") {
		t.Errorf("service has no https port:\n%s", svc)
	}

	for _, f := range []string{"templates/keycloak-runtime/statefulset.yaml", "templates/keycloak-runtime/service.yaml"} {
		if out := renderLandsKC(t, f, false, "--set", "keycloak.runtime.enabled=true"); strings.Contains(out, "8443") {
			t.Errorf("%s: tls off still renders the https port", f)
		}
	}
	out, _ := exec.Command("helm", "template", "gardenio", chartDir(t), "--set", "keycloak.runtime.enabled=true",
		"--show-only", "templates/keycloak-runtime/tls.yaml").CombinedOutput()
	if strings.Contains(string(out), "kind: Certificate") {
		t.Error("tls off still renders the keycloak-runtime Certificates")
	}
}

// Under networkPolicies, the controller may reach keycloak-runtime's https
// port and keycloak-runtime admits the controller there, only while the
// provisioner is on; the controller is no longer a peer on the http port.
func TestLandsKeycloakURL_NetworkPolicyPeers(t *testing.T) {
	t.Parallel()
	base := "networkPolicies.enabled=true,networkPolicies.defaultDenyEgress=true," + landsKCRuntimeTLS + "," + landsProvNoURL
	for _, on := range []bool{true, false} {
		set := base
		if !on {
			set += ",agents.lands.provisioner.enabled=false"
		}
		egress := renderLandsKC(t, "templates/networkpolicies/default-deny-egress.yaml", false, "--set", set)
		ctrl := egress[strings.Index(egress, "name: gardenio-controller-egress"):]
		ctrl = ctrl[:strings.Index(ctrl, "\n---")]
		peer := "app.kubernetes.io/component: keycloak-runtime\n      ports:\n        - protocol: TCP\n          port: 8443"
		if got := strings.Contains(ctrl, peer); got != on {
			t.Errorf("provisioner %v: controller egress has a keycloak-runtime :8443 peer = %v", on, got)
		}
		ingress := renderLandsKC(t, "templates/networkpolicies/networkpolicies.yaml", false, "--set", set)
		kc := ingress[strings.Index(ingress, "name: gardenio-keycloak-runtime-ingress"):]
		if i := strings.Index(kc, "\n---"); i >= 0 {
			kc = kc[:i]
		}
		admits := "app.kubernetes.io/component: agents\n      ports:\n        - protocol: TCP\n          port: 8443"
		if got := strings.Contains(kc, admits); got != on {
			t.Errorf("provisioner %v: keycloak-runtime ingress admits the controller on 8443 = %v", on, got)
		}
		if n := strings.Count(kc, "app.kubernetes.io/component: agents"); on && n != 1 {
			t.Errorf("provisioner on: the controller is a keycloak-runtime ingress peer %d times, want once (https only)", n)
		}
	}
}
