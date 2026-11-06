package tests

import (
	"strings"
	"testing"
)

// --- gvisor.namespace.secretMirrors: extraction-shape selection ---
//
// A mirror entry renders either dataFrom.extract (default, for JSON-object SM
// values) or a single-key `data` entry (when secretKey is set, for plain-string
// SM values). Getting this wrong wedges the runsc canary: dataFrom.extract on a
// plain string yields zero keys, the target Secret never materializes, and the
// pod defers forever on "secret not yet synced" (bugs gd-QEnm4gPcNO /
// ka-0000005152).

const gvisorMirrorBase = "gvisor.enabled=true,gvisor.namespace.name=gardenio-gvisor,externalSecrets.enabled=true,externalSecrets.secretStoreName=aws-secretsmanager"

func TestGvisorSecretMirror_DefaultUsesDataFromExtract(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/gvisor-namespace.yaml",
		gvisorMirrorBase,
		"gvisor.namespace.secretMirrors[0].name=gardenio-claude-oauth-gardenio3",
		"gvisor.namespace.secretMirrors[0].remoteRef=gardenio/gardenio-claude-oauth-gardenio3",
	)
	block := mirrorBlock(t, out, "gardenio-claude-oauth-gardenio3")
	if !strings.Contains(block, "dataFrom:") || !strings.Contains(block, "extract:") {
		t.Errorf("JSON-valued mirror should use dataFrom.extract, got:\n%s", block)
	}
	if strings.Contains(block, "secretKey:") {
		t.Errorf("default mirror must not emit a data.secretKey entry, got:\n%s", block)
	}
}

func TestGvisorSecretMirror_SecretKeyUsesSingleKeyData(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/gvisor-namespace.yaml",
		gvisorMirrorBase,
		"gvisor.namespace.secretMirrors[0].name=kd-beads-token",
		"gvisor.namespace.secretMirrors[0].remoteRef=gardenio/kd-beads-token",
		"gvisor.namespace.secretMirrors[0].secretKey=token",
	)
	block := mirrorBlock(t, out, "kd-beads-token")
	if strings.Contains(block, "dataFrom:") {
		t.Errorf("plain-string mirror (secretKey set) must NOT use dataFrom.extract, got:\n%s", block)
	}
	for _, want := range []string{"data:", "secretKey: \"token\"", "key: \"gardenio/kd-beads-token\""} {
		if !strings.Contains(block, want) {
			t.Errorf("expected secretKey mirror to contain %q, got:\n%s", want, block)
		}
	}
}

func TestGvisorSecretMirror_SecretKeyWithProperty(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/gvisor-namespace.yaml",
		gvisorMirrorBase,
		"gvisor.namespace.secretMirrors[0].name=some-secret",
		"gvisor.namespace.secretMirrors[0].remoteRef=gardenio/some-secret",
		"gvisor.namespace.secretMirrors[0].secretKey=token",
		"gvisor.namespace.secretMirrors[0].property=api_token",
	)
	block := mirrorBlock(t, out, "some-secret")
	if !strings.Contains(block, "property: \"api_token\"") {
		t.Errorf("expected property to render under remoteRef, got:\n%s", block)
	}
}

// mirrorBlock returns the ExternalSecret YAML document whose metadata.name
// matches the given target name. Fails the test if no such document is found.
func mirrorBlock(t *testing.T, rendered, name string) string {
	t.Helper()
	for _, doc := range strings.Split(rendered, "\n---") {
		if strings.Contains(doc, "kind: ExternalSecret") &&
			strings.Contains(doc, "name: "+name+"\n") {
			return doc
		}
	}
	t.Fatalf("no ExternalSecret document found for name %q in:\n%s", name, rendered)
	return ""
}
