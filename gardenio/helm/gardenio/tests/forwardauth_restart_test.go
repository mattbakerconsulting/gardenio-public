package tests

import (
	"regexp"
	"strings"
	"testing"
)

// gd-NrzwHXUxV0: a calver release with no template edit must not roll
// oauth2-proxy. The forwardauth Deployment hashes the templates ConfigMap into
// checksum/error-template; that ConfigMap used to carry helm.sh/chart, so every
// release rolled the pod and it came back with an empty JWKS cache. When the same
// release also restarted Keycloak, the first bearer verification fetched keys
// through the edge mid-boot, got a 502 and answered 401 to a valid `kd login`
// token. Same shape as the Keycloak theme fix (keycloak_theme_test.go, #544).

var forwardAuthRestartFlags = []string{
	"keycloak.enabled=true",
	"keycloak.forwardAuth.enabled=true",
	"keycloak.forwardAuth.oidcIssuerUrl=https://auth.example.test/auth/realms/gardenio",
}

var errorTemplateChecksumRE = regexp.MustCompile(`checksum/error-template: ([0-9a-f]{64})\n`)

func errorTemplateChecksumIn(t *testing.T, dep string) string {
	t.Helper()
	m := errorTemplateChecksumRE.FindStringSubmatch(dep)
	if m == nil {
		t.Fatalf("checksum/error-template annotation missing or not a sha256:\n%s", dep)
	}
	return m[1]
}

func TestForwardAuth_TemplatesConfigMapHasNoChartVersionLabel(t *testing.T) {
	t.Parallel()
	cm := helmTemplate(t, "templates/forwardauth/error-template-configmap.yaml", forwardAuthRestartFlags...)
	if strings.Contains(cm, "helm.sh/chart:") {
		t.Errorf("templates ConfigMap must not carry helm.sh/chart (it is hashed into the pod template):\n%s", cm)
	}
	for _, want := range []string{"app.kubernetes.io/managed-by: Helm", "app.kubernetes.io/component: forwardauth", "app.kubernetes.io/instance: "} {
		if !strings.Contains(cm, want) {
			t.Errorf("templates ConfigMap lost label %q:\n%s", want, cm)
		}
	}
}

// The Deployment, which does carry helm.sh/chart, is the positive control: the
// same two-version comparison must see it differ, so the version bump is known
// to reach the render, while checksum/error-template stays the same.
func TestForwardAuth_ChecksumIgnoresChartVersion(t *testing.T) {
	t.Parallel()
	v1 := helmTemplatePackaged(t, "2026.100.1", forwardAuthRestartFlags...)
	v2 := helmTemplatePackaged(t, "2026.200.9", forwardAuthRestartFlags...)

	depV1 := renderedDoc(t, v1, "Deployment", "test-forwardauth")
	depV2 := renderedDoc(t, v2, "Deployment", "test-forwardauth")
	if depV1 == depV2 || !strings.Contains(depV1, "gardenio-2026.100.1") || !strings.Contains(depV2, "gardenio-2026.200.9") {
		t.Fatal("control: the Deployment carries helm.sh/chart and must differ between chart versions; the version bump is not reaching the render")
	}
	if c1, c2 := errorTemplateChecksumIn(t, depV1), errorTemplateChecksumIn(t, depV2); c1 != c2 {
		t.Errorf("checksum/error-template changed with the chart version (%s -> %s): a calver release would roll oauth2-proxy", c1, c2)
	}
	cmV1 := renderedDoc(t, v1, "ConfigMap", "test-forwardauth-templates")
	cmV2 := renderedDoc(t, v2, "ConfigMap", "test-forwardauth-templates")
	if cmV1 != cmV2 {
		t.Errorf("templates ConfigMap differs between chart versions:\n--- v1\n%s\n--- v2\n%s", cmV1, cmV2)
	}
}
