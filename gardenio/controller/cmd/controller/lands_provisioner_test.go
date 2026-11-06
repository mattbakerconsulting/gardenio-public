package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"gardenio/controller/internal/beadsapi"
)

func landsProvisionerEnv(t *testing.T, over map[string]string) (func(string) string, func(string) ([]byte, error)) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	env := map[string]string{
		"LANDS_PROVISION_ENABLED":           "true",
		"LANDS_KEYCLOAK_URL":                "https://auth.example.com",
		"LANDS_KEYCLOAK_CLIENT_SECRET_PATH": "/secrets/kc",
		"LANDS_KEYCLOAK_RESERVED_GROUPS":    "project-ops, gardenio-extra",
		"LANDS_ACCOUNT_TEMPLATE":            "lands-upstream",
		"LANDS_GITHUB_APP_ID":               "12345",
		"LANDS_GITHUB_APP_PRIVATE_KEY_PATH": "/secrets/app.pem",
	}
	for k, v := range over {
		env[k] = v
	}
	files := map[string][]byte{"/secrets/kc": []byte("s3cret\n"), "/secrets/app.pem": keyPEM}
	return func(k string) string { return env[k] }, func(p string) ([]byte, error) {
		if b, ok := files[p]; ok {
			return b, nil
		}
		return nil, os.ErrNotExist
	}
}

func TestLandsProvisioner_OffByDefault(t *testing.T) {
	getenv, readFile := landsProvisionerEnv(t, map[string]string{"LANDS_PROVISION_ENABLED": ""})
	p, err := newLandsProvisioner(context.Background(), &beadsapi.Client{}, "http://litellm", "sk-master", getenv, readFile, quietLogger())
	if p != nil || err != nil {
		t.Fatalf("off: %v, %v", p, err)
	}
	if slices.Contains(controllerMachineRolesFor(getenv), landsProvisionerMachineRole) {
		t.Fatal("the provisioner's role is expected while it is off")
	}
}

func TestLandsProvisioner_FullyConfigured(t *testing.T) {
	getenv, readFile := landsProvisionerEnv(t, nil)
	p, err := newLandsProvisioner(context.Background(), &beadsapi.Client{}, "http://litellm", "sk-master", getenv, readFile, quietLogger())
	if err != nil || p == nil {
		t.Fatalf("configured: %v, %v", p, err)
	}
	if p.AccountTemplate != "lands-upstream" || p.Keys == nil || p.Groups == nil || p.Repos == nil {
		t.Fatalf("provisioner = %+v", p)
	}
	if roles := controllerMachineRolesFor(getenv); !slices.Contains(roles, landsProvisionerMachineRole) || len(roles) != len(controllerMachineRoles)+1 {
		t.Fatalf("roles = %v", roles)
	}
}

// LANDS_RUNTIME_CLASS reaches the provisioner's namespace step; unset, it
// stamps no runtime class (gd-iyZom5u11A).
func TestLandsProvisioner_RuntimeClass(t *testing.T) {
	for env, want := range map[string]string{"": "", "runsc": "runsc", " runsc ": "runsc"} {
		getenv, readFile := landsProvisionerEnv(t, map[string]string{"LANDS_RUNTIME_CLASS": env})
		p, err := newLandsProvisioner(context.Background(), &beadsapi.Client{}, "http://litellm", "sk-master", getenv, readFile, quietLogger())
		if err != nil || p == nil || p.RuntimeClass != want {
			t.Errorf("LANDS_RUNTIME_CLASS=%q: %+v, %v; want runtime class %q", env, p, err, want)
		}
	}
}

// The demo land and its pass length (gd-c6LzZkk0qy) reach the provisioner;
// a bad value keeps it off.
func TestLandsProvisioner_DemoLand(t *testing.T) {
	getenv, readFile := landsProvisionerEnv(t, map[string]string{"LANDS_DEMO_LAND": "demo", "LANDS_DEMO_GUEST_TTL": "48h"})
	p, err := newLandsProvisioner(context.Background(), &beadsapi.Client{}, "http://litellm", "sk-master", getenv, readFile, quietLogger())
	if err != nil || p == nil || p.DemoLand != "demo" || p.DemoGuestTTL != 48*time.Hour {
		t.Fatalf("demo: %+v, %v", p, err)
	}
	for _, over := range []map[string]string{
		{"LANDS_DEMO_LAND": "Demo!"},
		{"LANDS_DEMO_LAND": "claimlayer"},
		{"LANDS_DEMO_LAND": "demo", "LANDS_DEMO_GUEST_TTL": "5m"},
		{"LANDS_DEMO_LAND": "demo", "LANDS_DEMO_GUEST_TTL": "a day"},
	} {
		getenv, readFile := landsProvisionerEnv(t, over)
		if _, err := newLandsProvisioner(context.Background(), &beadsapi.Client{}, "http://litellm", "sk-master", getenv, readFile, quietLogger()); err == nil {
			t.Errorf("%v accepted", over)
		}
	}
}

// The gatehouse (step 9.12) reaches the provisioner; a bad name, or the demo
// land's, keeps it off.
func TestLandsProvisioner_Gatehouse(t *testing.T) {
	getenv, readFile := landsProvisionerEnv(t, map[string]string{"LANDS_GATEHOUSE_LAND": "gatehouse", "LANDS_DEMO_LAND": "demo"})
	p, err := newLandsProvisioner(context.Background(), &beadsapi.Client{}, "http://litellm", "sk-master", getenv, readFile, quietLogger())
	if err != nil || p == nil || p.Gatehouse != "gatehouse" {
		t.Fatalf("gatehouse: %+v, %v", p, err)
	}
	for _, over := range []map[string]string{
		{"LANDS_GATEHOUSE_LAND": "Gate house"},
		{"LANDS_GATEHOUSE_LAND": "claimlayer"},
		{"LANDS_GATEHOUSE_LAND": "demo", "LANDS_DEMO_LAND": "demo"},
	} {
		getenv, readFile := landsProvisionerEnv(t, over)
		if _, err := newLandsProvisioner(context.Background(), &beadsapi.Client{}, "http://litellm", "sk-master", getenv, readFile, quietLogger()); err == nil {
			t.Errorf("%v accepted", over)
		}
	}
}

func TestLandsProvisioner_OnButIncompleteNamesWhatIsMissing(t *testing.T) {
	getenv, readFile := landsProvisionerEnv(t, map[string]string{
		"LANDS_KEYCLOAK_URL": "", "LANDS_ACCOUNT_TEMPLATE": "",
		"LANDS_GITHUB_APP_ID": "", "LANDS_GITHUB_APP_PRIVATE_KEY_PATH": "",
	})
	p, err := newLandsProvisioner(context.Background(), &beadsapi.Client{}, "", "", getenv, readFile, quietLogger())
	if p != nil || err == nil {
		t.Fatalf("incomplete: %v, %v", p, err)
	}
	for _, want := range []string{"LANDS_KEYCLOAK_URL", "LANDS_ACCOUNT_TEMPLATE", "LITELLM_URL", "LANDS_GITHUB_APP_ID"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}

	getenv, readFile = landsProvisionerEnv(t, map[string]string{"LANDS_KEYCLOAK_URL": "http://auth.example.com"})
	if _, err := newLandsProvisioner(context.Background(), &beadsapi.Client{}, "http://litellm", "sk", getenv, readFile, quietLogger()); err == nil {
		t.Fatal("a plain-http Keycloak URL must be refused")
	}
	getenv, readFile = landsProvisionerEnv(t, map[string]string{"LANDS_KEYCLOAK_CLIENT_SECRET_PATH": "/nope"})
	if _, err := newLandsProvisioner(context.Background(), &beadsapi.Client{}, "http://litellm", "sk", getenv, readFile, quietLogger()); err == nil {
		t.Fatal("an unreadable client secret must be refused")
	}
}

// LANDS_KEYCLOAK_CA_FILE makes the admin client trust a private CA, as the
// in-cluster keycloak-runtime Service's (gd-XRsosVFAau); a CA file that is
// missing or holds no certificate keeps the provisioner off.
func TestLandsKeycloakHTTPClient_TrustsCAFile(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	readFile := func(p string) ([]byte, error) {
		switch p {
		case "/ca/ca.crt":
			return caPEM, nil
		case "/ca/empty":
			return []byte("not a cert\n"), nil
		}
		return nil, os.ErrNotExist
	}

	if hc, err := landsKeycloakHTTPClient("", readFile); hc != nil || err != nil {
		t.Fatalf("no CA file: %v, %v; want the default client", hc, err)
	}
	hc, err := landsKeycloakHTTPClient("/ca/ca.crt", readFile)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := hc.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET with the CA trusted: %v", err)
	}
	resp.Body.Close()
	if _, err := (&http.Client{}).Get(srv.URL); err == nil {
		t.Fatal("the test server's CA must not be trusted by default")
	}
	for _, p := range []string{"/ca/empty", "/ca/missing"} {
		if _, err := landsKeycloakHTTPClient(p, readFile); err == nil {
			t.Errorf("CA file %s: want error", p)
		}
	}

	getenv, readProv := landsProvisionerEnv(t, map[string]string{"LANDS_KEYCLOAK_CA_FILE": "/ca/missing"})
	if _, err := newLandsProvisioner(context.Background(), &beadsapi.Client{}, "http://litellm", "sk", getenv, readProv, quietLogger()); err == nil {
		t.Fatal("an unreadable Keycloak CA file must keep the provisioner off")
	}
}

func TestLandNamespaces_FromEnv(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	k8s := fake.NewSimpleClientset()

	if a, err := newLandNamespaces(k8s, "gardenio", getenv); a != nil || err != nil {
		t.Fatalf("off: %v, %v", a, err)
	}

	env["LANDS_NAMESPACES_ENABLED"] = "true"
	if _, err := newLandNamespaces(k8s, "gardenio", getenv); err == nil || !strings.Contains(err.Error(), "ServiceAccount") {
		t.Fatalf("on without the controller ServiceAccount: %v", err)
	}

	env["LANDS_CONTROLLER_SERVICE_ACCOUNT"] = "gardenio-agents"
	env["LANDS_NAMESPACE_EXTRA_EXCEPT_CIDRS"] = "fd00::/8, 203.0.113.0/24"
	a, err := newLandNamespaces(k8s, "gardenio", getenv)
	if err != nil || a == nil {
		t.Fatalf("on: %v, %v", a, err)
	}
	if a.Config.ReleaseNamespace != "gardenio" || a.Config.ControllerServiceAccount != "gardenio-agents" ||
		!slices.Equal(a.Config.ExtraExceptCIDRs, []string{"fd00::/8", "203.0.113.0/24"}) {
		t.Fatalf("config = %+v", a.Config)
	}

	env["LANDS_NAMESPACE_EGRESS_PEERS"] = `[{"name":"litellm","namespace":"llm","labels":{"app":"litellm"},"ports":[4000]}]`
	a, err = newLandNamespaces(k8s, "gardenio", getenv)
	if err != nil || len(a.Config.Peers) != 1 || a.Config.Peers[0].Namespace != "llm" {
		t.Fatalf("peers: %+v, %v", a, err)
	}
	env["LANDS_NAMESPACE_EGRESS_PEERS"] = `[{"name":"open","ports":[80]}]`
	if _, err := newLandNamespaces(k8s, "gardenio", getenv); err == nil {
		t.Fatal("a peer with no labels must be refused")
	}
	env["LANDS_NAMESPACE_EGRESS_PEERS"] = `{`
	if _, err := newLandNamespaces(k8s, "gardenio", getenv); err == nil {
		t.Fatal("bad JSON must be refused")
	}
}

func TestLandScopes_FromEnv(t *testing.T) {
	env := map[string]string{}
	getenv := func(k string) string { return env[k] }
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())
	if a := newLandScopes(dyn, "gardenio", getenv); a != nil {
		t.Fatalf("unset: %+v", a)
	}
	env["LANDS_KEYCLOAK_REALM_RESOURCE"] = "gardenio-realm"
	if a := newLandScopes(nil, "gardenio", getenv); a != nil {
		t.Fatalf("no client: %+v", a)
	}
	a := newLandScopes(dyn, "gardenio", getenv)
	if a == nil || a.Namespace != "gardenio" || a.Realm != "gardenio-realm" {
		t.Fatalf("set: %+v", a)
	}
}
