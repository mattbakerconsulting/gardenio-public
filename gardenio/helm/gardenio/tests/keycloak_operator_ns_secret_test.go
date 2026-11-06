package tests

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// ka-0000005282: the EDP keycloak-operator resolves a ClusterKeycloak's
// .spec.secret in the OPERATOR's namespace, not in the CR's namespace. The
// chart's admin Secret lands in .Release.Namespace, so without an identical
// copy in the operator namespace the operator can never authenticate —
// ClusterKeycloak stays connected:false, the realm/clients are never
// reconciled, and oauth2-proxy CrashLoopBackOffs on OIDC discovery 404
// ("Realm does not exist"). These tests pin the copy + its determinism.

// inboxRenderNS renders the whole chart with the in-box Keycloak runtime on and
// a PINNED release namespace, so assertions on .Release.Namespace are
// deterministic regardless of the ambient kube context (ka-0000003495).
func inboxRenderNS(t *testing.T, namespace string, extra ...string) string {
	t.Helper()
	args := []string{"template", "test", chartDir(t), "--namespace", namespace,
		"--set", "postgres.enabled=true",
		"--set", "keycloak.enabled=true",
		"--set", "keycloak.runtime.enabled=true",
		"--set", "keycloak.runtime.db.autoWire=true",
		"--set", "keycloak.runtime.host=auth.example.com",
		"--set", "keycloak.realm.name=myrealm",
	}
	for _, s := range extra {
		args = append(args, "--set", s)
	}
	out, err := exec.Command("helm", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("helm template failed: %v\n%s", err, out)
	}
	return string(out)
}

// runtimeAdminSecretDocs returns every rendered gardenio-keycloak-runtime-admin
// Secret document, keyed by namespace.
func runtimeAdminSecretDocs(t *testing.T, out string) map[string]string {
	t.Helper()
	docs := map[string]string{}
	nsRE := regexp.MustCompile(`(?m)^\s*namespace:\s*"?([A-Za-z0-9-]+)"?\s*$`)
	for _, d := range strings.Split(out, "\n---") {
		if !strings.Contains(d, "kind: Secret") {
			continue
		}
		if !strings.Contains(d, `name: "gardenio-keycloak-runtime-admin"`) {
			continue
		}
		m := nsRE.FindStringSubmatch(d)
		if m == nil {
			t.Fatalf("admin Secret doc has no namespace:\n%s", d)
		}
		docs[m[1]] = d
	}
	return docs
}

func adminSecretPassword(doc string) string {
	m := regexp.MustCompile(`(?m)^\s*password:\s*"?([A-Za-z0-9+/=]+)"?\s*$`).FindStringSubmatch(doc)
	if m == nil {
		return ""
	}
	return m[1]
}

//  1. With operatorNamespace set, a SECOND admin Secret is rendered into that
//     namespace, and both copies carry the SAME password (critical on first
//     install where the lookup helper returns nil and would otherwise mint two
//     different randAlphaNum values).
func TestKeycloakOperatorNS_AdminSecretCopied(t *testing.T) {
	t.Parallel()
	out := inboxRenderNS(t, "gardenio",
		"keycloak.runtime.clusterKeycloak.operatorNamespace=operators",
	)
	docs := runtimeAdminSecretDocs(t, out)
	if _, ok := docs["gardenio"]; !ok {
		t.Fatalf("expected an admin Secret in the release namespace gardenio; got namespaces %v", adminKeys(docs))
	}
	opDoc, ok := docs["operators"]
	if !ok {
		t.Fatalf("expected an admin Secret copy in ns/operators; got namespaces %v", adminKeys(docs))
	}
	relPW := adminSecretPassword(docs["gardenio"])
	opPW := adminSecretPassword(opDoc)
	if relPW == "" || opPW == "" {
		t.Fatalf("both admin Secret copies must carry a password (rel=%q op=%q)", relPW, opPW)
	}
	if relPW != opPW {
		t.Errorf("operator-namespace admin Secret password must MATCH the release-namespace copy; got rel=%q op=%q", relPW, opPW)
	}
	// The copy must keep its value across helm uninstall (resource-policy keep).
	if !strings.Contains(opDoc, "helm.sh/resource-policy") {
		t.Error("operator-namespace admin Secret must carry helm.sh/resource-policy: keep")
	}
}

//  2. Default (operatorNamespace empty) renders NO second copy — the cloud path
//     binds the realm to the external acme-keycloak whose secret already lives in
//     the operators namespace, so the chart must not create a stray Secret there.
func TestKeycloakOperatorNS_NoCopyByDefault(t *testing.T) {
	t.Parallel()
	out := inboxRenderNS(t, "gardenio")
	docs := runtimeAdminSecretDocs(t, out)
	if len(docs) != 1 {
		t.Errorf("without operatorNamespace exactly one admin Secret should render; got namespaces %v", adminKeys(docs))
	}
	if _, ok := docs["operators"]; ok {
		t.Error("no admin Secret should be rendered into ns/operators when operatorNamespace is unset")
	}
}

//  3. operatorNamespace equal to the release namespace must NOT duplicate the
//     Secret (they would collide on the same name+namespace).
func TestKeycloakOperatorNS_SameNamespaceNoDuplicate(t *testing.T) {
	t.Parallel()
	out := inboxRenderNS(t, "gardenio",
		"keycloak.runtime.clusterKeycloak.operatorNamespace=gardenio",
	)
	docs := runtimeAdminSecretDocs(t, out)
	if len(docs) != 1 {
		t.Errorf("operatorNamespace == release namespace must render exactly one admin Secret; got %v", adminKeys(docs))
	}
}

func adminKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
