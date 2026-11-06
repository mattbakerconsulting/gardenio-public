package tests

import (
	"os/exec"
	"strings"
	"testing"
)

// gd-ZbqAlzulHK: a browser-facing Keycloak frontend URL, with in-cluster
// callers kept on the Service URL via backchannel-dynamic.

func kcRuntimeSTS(t *testing.T, extra ...string) (string, error) {
	t.Helper()
	args := []string{
		"template", "test", chartDir(t), "--namespace", "gardenio",
		"--show-only", "templates/keycloak-runtime/statefulset.yaml",
		"--set", "postgres.enabled=true",
		"--set", "keycloak.enabled=true",
		"--set", "keycloak.runtime.enabled=true",
		"--set", "keycloak.runtime.db.autoWire=true",
		"--set", "keycloak.runtime.host=kc.ns.svc.cluster.local",
	}
	for _, s := range extra {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	return string(out), err
}

func kcEnvValue(t *testing.T, out, name string) (string, bool) {
	t.Helper()
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) == "- name: "+name && i+1 < len(lines) {
			v := strings.TrimSpace(lines[i+1])
			return strings.Trim(strings.TrimPrefix(v, "value: "), `"`), true
		}
	}
	return "", false
}

func TestKeycloakHostnameURL_DefaultUnchanged(t *testing.T) {
	t.Parallel()
	out, err := kcRuntimeSTS(t)
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if v, _ := kcEnvValue(t, out, "KC_HOSTNAME"); v != "kc.ns.svc.cluster.local" {
		t.Fatalf("KC_HOSTNAME = %q, want runtime.host", v)
	}
	if _, ok := kcEnvValue(t, out, "KC_HOSTNAME_BACKCHANNEL_DYNAMIC"); ok {
		t.Fatal("KC_HOSTNAME_BACKCHANNEL_DYNAMIC rendered without being asked for")
	}
}

func TestKeycloakHostnameURL_Set(t *testing.T) {
	t.Parallel()
	out, err := kcRuntimeSTS(t,
		"keycloak.runtime.hostnameUrl=https://auth.example.com/auth",
		"keycloak.runtime.hostnameBackchannelDynamic=true")
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	if v, _ := kcEnvValue(t, out, "KC_HOSTNAME"); v != "https://auth.example.com/auth" {
		t.Fatalf("KC_HOSTNAME = %q", v)
	}
	if v, _ := kcEnvValue(t, out, "KC_HOSTNAME_BACKCHANNEL_DYNAMIC"); v != "true" {
		t.Fatalf("KC_HOSTNAME_BACKCHANNEL_DYNAMIC = %q", v)
	}
}

func TestKeycloakHostnameURL_BackchannelNeedsURL(t *testing.T) {
	t.Parallel()
	out, err := kcRuntimeSTS(t, "keycloak.runtime.hostnameBackchannelDynamic=true")
	if err == nil || !strings.Contains(out, "needs keycloak.runtime.hostnameUrl") {
		t.Fatalf("want a render failure, got err=%v\n%s", err, out)
	}
}
