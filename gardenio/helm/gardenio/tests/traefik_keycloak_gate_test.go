package tests

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// helmTemplateFull renders the entire chart (no --show-only) with the given
// --set overrides and -f files. Returns the full multi-doc YAML output.
func helmTemplateFull(t *testing.T, valuesFiles []string, setFlags ...string) string {
	t.Helper()
	args := []string{"template", "test", chartDir(t)}
	for _, f := range valuesFiles {
		args = append(args, "-f", chartDir(t)+"/"+f)
	}
	for _, s := range setFlags {
		args = append(args, "--set", s)
	}
	cmd := exec.Command("helm", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

// kindLineRE matches manifest "kind: <X>" lines at the start of a line.
var kindLineRE = regexp.MustCompile(`(?m)^kind:\s+([A-Za-z]+)\s*$`)

func countKinds(out string, kinds ...string) map[string]int {
	want := make(map[string]struct{}, len(kinds))
	for _, k := range kinds {
		want[k] = struct{}{}
	}
	counts := make(map[string]int)
	for _, m := range kindLineRE.FindAllStringSubmatch(out, -1) {
		k := m[1]
		if _, ok := want[k]; ok {
			counts[k]++
		}
	}
	return counts
}

var (
	traefikKinds  = []string{"IngressRoute", "Middleware", "TLSOption"}
	keycloakKinds = []string{
		"KeycloakRealm",
		"KeycloakClient",
		"KeycloakRealmGroup",
		"KeycloakRealmRole",
		"KeycloakRealmIdentityProvider",
		"KeycloakAuthFlow",
		"ClusterKeycloak",
	}
)

// TestTraefikKeycloakGate_BothOff_NoCRDsEmitted is the customer-poc path:
// with traefik.enabled=false AND keycloak.enabled=false, the rendered
// manifest must contain ZERO IngressRoute / Middleware / TLSOption /
// Keycloak* objects. This is what unblocks helm install on clusters that
// don't preinstall the Traefik or EDP keycloak-operator CRDs.
func TestTraefikKeycloakGate_BothOff_NoCRDsEmitted(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, []string{"values-test.yaml"},
		"traefik.enabled=false",
		"keycloak.enabled=false",
	)
	all := append(append([]string{}, traefikKinds...), keycloakKinds...)
	counts := countKinds(out, all...)
	for _, k := range all {
		if counts[k] != 0 {
			t.Errorf("with traefik.enabled=false + keycloak.enabled=false, expected 0 %s objects, got %d", k, counts[k])
		}
	}
}

// TestTraefikKeycloakGate_TraefikOffOnly_NoTraefikCRDs proves the traefik
// toggle is independent: turning Traefik off alone strips every Traefik CRD
// even when keycloak.enabled is left at its default.
func TestTraefikKeycloakGate_TraefikOffOnly_NoTraefikCRDs(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, []string{"values-test.yaml"},
		"traefik.enabled=false",
	)
	counts := countKinds(out, traefikKinds...)
	for _, k := range traefikKinds {
		if counts[k] != 0 {
			t.Errorf("with traefik.enabled=false, expected 0 %s objects, got %d", k, counts[k])
		}
	}
}

// TestTraefikKeycloakGate_KeycloakOffOnly_NoKeycloakCRDs proves the
// keycloak toggle is independent: turning Keycloak off alone strips every
// Keycloak* CRD even when traefik.enabled is left at its default.
func TestTraefikKeycloakGate_KeycloakOffOnly_NoKeycloakCRDs(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, []string{"values-test.yaml"},
		"keycloak.enabled=false",
	)
	counts := countKinds(out, keycloakKinds...)
	for _, k := range keycloakKinds {
		if counts[k] != 0 {
			t.Errorf("with keycloak.enabled=false, expected 0 %s objects, got %d", k, counts[k])
		}
	}
}

// TestTraefikKeycloakGate_DefaultRender_StillEmitsCRDs is the
// no-regression guard for existing america-e2e-eks / e2e / prod
// consumers: with the default values (traefik.enabled=true,
// keycloak.enabled=true), the chart must still emit at least one
// IngressRoute, one Middleware, one TLSOption, and one Keycloak* CR.
// Uses default values.yaml (no test overlay) so TLSOption — which
// values-test.yaml disables via tls.enabled=false — is rendered.
func TestTraefikKeycloakGate_DefaultRender_StillEmitsCRDs(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil)

	traefikCounts := countKinds(out, traefikKinds...)
	for _, k := range traefikKinds {
		if traefikCounts[k] == 0 {
			t.Errorf("default render must emit at least one %s, got 0", k)
		}
	}

	keycloakCounts := countKinds(out, keycloakKinds...)
	totalKeycloak := 0
	for _, k := range keycloakKinds {
		totalKeycloak += keycloakCounts[k]
	}
	if totalKeycloak == 0 {
		t.Errorf("default render must emit at least one Keycloak* CR, got 0 total across %v", keycloakKinds)
	}
}

// TestTraefikKeycloakGate_DefaultValuesYaml_HasToggles is a smoke test that
// the documented toggles live at the expected paths in values.yaml so the
// customer-poc overlay (and operator docs) can rely on them.
func TestTraefikKeycloakGate_DefaultValuesYaml_HasToggles(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil, "traefik.enabled=true", "keycloak.enabled=true")
	if !strings.Contains(out, "kind: IngressRoute") {
		t.Errorf("default render with explicit traefik.enabled=true must emit IngressRoute objects")
	}
}

// TestTraefikIngressRoutesGate_BoxProfile_EmitsRoutesWithWorkloadOff is the
// Gardenio-in-a-Box regression guard for ka-0000001357. The box runs the
// k3s-BUNDLED Traefik (kube-system), so it correctly sets traefik.enabled=false
// (don't deploy a SECOND Traefik) while the bundled Traefik STILL needs the
// chart's IngressRoute/Middleware/TLSOption CRs. The inner decoupling flag
// traefik.ingressRoutes.enabled=true must emit those CRs even with the legacy
// workload flag off. Before the split this rendered ZERO routes and the LE
// perHost cert never bound.
func TestTraefikIngressRoutesGate_BoxProfile_EmitsRoutesWithWorkloadOff(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, []string{"values-test.yaml"},
		"traefik.enabled=false",
		"traefik.ingressRoutes.enabled=true",
	)
	counts := countKinds(out, traefikKinds...)
	// values-test.yaml sets tls.enabled=false so TLSOption is intentionally
	// absent; the routing CRs (IngressRoute + Middleware) are what matter here.
	if counts["IngressRoute"] == 0 {
		t.Errorf("with traefik.enabled=false + traefik.ingressRoutes.enabled=true, expected >0 IngressRoute objects (bundled-Traefik box profile), got 0")
	}
	if counts["Middleware"] == 0 {
		t.Errorf("with traefik.enabled=false + traefik.ingressRoutes.enabled=true, expected >0 Middleware objects, got 0")
	}
}

// TestTraefikIngressRoutesGate_InnerFalseOverridesWorkloadOn proves the inner
// flag WINS when explicitly set: traefik.ingressRoutes.enabled=false suppresses
// every Traefik CR even though the legacy traefik.enabled=true would otherwise
// emit them.
func TestTraefikIngressRoutesGate_InnerFalseOverridesWorkloadOn(t *testing.T) {
	t.Parallel()
	out := helmTemplateFull(t, nil,
		"traefik.enabled=true",
		"traefik.ingressRoutes.enabled=false",
	)
	counts := countKinds(out, traefikKinds...)
	for _, k := range traefikKinds {
		if counts[k] != 0 {
			t.Errorf("with traefik.ingressRoutes.enabled=false, expected 0 %s objects (inner flag overrides traefik.enabled=true), got %d", k, counts[k])
		}
	}
}

// TestTraefikIngressRoutesGate_InnerUnsetFallsBackToWorkloadFlag locks in the
// backward-compatible fallback: when traefik.ingressRoutes.enabled is left
// null, CR emission follows the legacy traefik.enabled. This is what keeps the
// customer-poc POC (traefik.enabled=false, inner unset) emitting ZERO CRs.
func TestTraefikIngressRoutesGate_InnerUnsetFallsBackToWorkloadFlag(t *testing.T) {
	t.Parallel()
	// traefik.enabled=false, inner unset → zero CRs (customer-poc path).
	off := helmTemplateFull(t, []string{"values-test.yaml"}, "traefik.enabled=false")
	for _, k := range traefikKinds {
		if c := countKinds(off, traefikKinds...)[k]; c != 0 {
			t.Errorf("traefik.enabled=false with inner unset must emit 0 %s (fallback), got %d", k, c)
		}
	}
	// traefik.enabled=true, inner unset → routes still emit (default path).
	on := helmTemplateFull(t, nil, "traefik.enabled=true")
	if countKinds(on, "IngressRoute")["IngressRoute"] == 0 {
		t.Errorf("traefik.enabled=true with inner unset must still emit IngressRoute objects (fallback), got 0")
	}
}
