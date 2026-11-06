package tests

import (
	"strings"
	"testing"
)

// The gardenio-box-useradmin-svc client secret (epic ka-0000001297, task
// ka-0000001845) reaches agent pods via ExternalSecret only — the box Keycloak
// is a separate server with no KeycloakClient CRD path, so its confidential
// client secret must flow AWS SM -> ExternalSecret -> K8s Secret. These tests
// pin:
//   - the ExternalSecret stays DORMANT unless externalSecrets.enabled AND a
//     boxUserAdminSvcSecret.remoteRef is set (default-off, like every other
//     opt-in ExternalSecret in this chart),
//   - when on, it emits the SAME Secret name + `clientSecret` key the agent
//     helper (lib-keycloak-box-useradmin.sh) and the extraRules grant read, so
//     a drift that renames the target or key would fail here instead of
//     silently breaking the agent's kubectl read.

func boxUserAdminESOCount(out string) int {
	// Count ExternalSecret manifests carrying the box-useradmin-svc component
	// label. Each rendered doc is separated by a `---` marker.
	n := 0
	for doc := range strings.SplitSeq(out, "\n---") {
		if strings.Contains(doc, "kind: ExternalSecret") &&
			strings.Contains(doc, "app.kubernetes.io/component: box-useradmin-svc") {
			n++
		}
	}
	return n
}

func TestBoxUserAdminESO_DormantByDefault(t *testing.T) {
	t.Parallel()
	// externalSecrets enabled but no remoteRef: nothing renders.
	out := helmTemplate(t, "",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secrets-manager",
	)
	if got := boxUserAdminESOCount(out); got != 0 {
		t.Fatalf("box-useradmin ExternalSecret must NOT render without a remoteRef, got %d", got)
	}
}

func TestBoxUserAdminESO_DormantWhenExternalSecretsDisabled(t *testing.T) {
	t.Parallel()
	// remoteRef set but externalSecrets OFF: still nothing (the whole family is
	// gated on externalSecrets.enabled).
	out := helmTemplate(t, "",
		"externalSecrets.enabled=false",
		"externalSecrets.boxUserAdminSvcSecret.remoteRef=gardenio/prod/box-useradmin-svc-client-secret",
	)
	if got := boxUserAdminESOCount(out); got != 0 {
		t.Fatalf("box-useradmin ExternalSecret must NOT render when externalSecrets.enabled=false, got %d", got)
	}
}

func TestBoxUserAdminESO_RendersWhenEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secrets-manager",
		"externalSecrets.boxUserAdminSvcSecret.remoteRef=gardenio/prod/box-useradmin-svc-client-secret",
	)
	if got := boxUserAdminESOCount(out); got != 1 {
		t.Fatalf("expected exactly 1 box-useradmin ExternalSecret, got %d\n%s", got, out)
	}
	// Pin the target Secret name + key the agent helper and extraRules read.
	// A drift on either would silently break the agent's read.
	for _, want := range []string{
		"name: gardenio-box-useradmin-svc",
		"clientSecret: '{{ .clientSecret }}'",
		"secretKey: clientSecret",
		`key: "gardenio/prod/box-useradmin-svc-client-secret"`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("rendered box-useradmin ExternalSecret missing %q\n%s", want, out)
		}
	}
	// Guard the IRSA-scope invariant: the effective remoteRef key must live under
	// gardenio/prod/*, never gardenio/box/* (the ESO IRSA is GetSecretValue-scoped
	// to gardenio/prod/* — a gardenio/box/* key AccessDenies; see the runbook).
	// Scope the check to the rendered `key:` line so an explanatory comment
	// mentioning gardenio/box/* does not false-positive.
	if strings.Contains(out, `key: "gardenio/box/`) {
		t.Fatalf("box-useradmin remoteRef must not use the gardenio/box/* prefix (ESO IRSA is scoped to gardenio/prod/*)\n%s", out)
	}
}

func TestBoxUserAdminESO_CustomSecretName(t *testing.T) {
	t.Parallel()
	// The target Secret name is overridable; the clientSecret key is fixed.
	out := helmTemplate(t, "",
		"externalSecrets.enabled=true",
		"externalSecrets.secretStoreName=aws-secrets-manager",
		"externalSecrets.boxUserAdminSvcSecret.remoteRef=gardenio/prod/box-useradmin-svc-client-secret",
		"externalSecrets.boxUserAdminSvcSecret.secretName=custom-box-secret",
	)
	if !strings.Contains(out, "name: custom-box-secret") {
		t.Fatalf("expected overridden target Secret name custom-box-secret\n%s", out)
	}
}
