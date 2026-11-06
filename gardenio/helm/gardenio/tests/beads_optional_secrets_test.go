package tests

import (
	"strings"
	"testing"
)

// Regression for ka-0000002852: the gardenio-next-beads pod wedged in
// CreateContainerConfigError ("secret bridge-fleet-tokens not found") because
// the BEADS_FLEET_MAP and BEADS_EMBED_API_KEY env vars referenced Secrets that
// were never provisioned in that namespace (gardenio-next runs no bridges, and
// its embedding key had not synced). Marking both secretKeyRefs optional lets a
// missing Secret degrade gracefully instead of blocking container creation.
// Prod still loads the values because the Secrets exist there.

// blockAfter returns the slice of out starting at the first line that contains
// marker, so Contains checks below are scoped to a single env var's secretKeyRef
// and don't accidentally match a sibling env var's "optional: true".
func blockAfter(out, marker string) string {
	i := strings.Index(out, marker)
	if i < 0 {
		return ""
	}
	rest := out[i:]
	// Scope to the next ~12 lines, which comfortably covers a valueFrom block.
	lines := strings.SplitN(rest, "\n", 13)
	return strings.Join(lines, "\n")
}

func TestBeadsFleetMapSecretRefIsOptional(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml")
	block := blockAfter(out, "BEADS_FLEET_MAP")
	if block == "" {
		t.Fatal("expected BEADS_FLEET_MAP env var in rendered beads deployment")
	}
	if !strings.Contains(block, "optional: true") {
		t.Errorf("BEADS_FLEET_MAP secretKeyRef must be optional so a missing "+
			"bridge-token Secret does not wedge the pod (ka-0000002852); got:\n%s", block)
	}
}

// Regression for gd-ShWLbKKzGs: the privileged-fleet ownership bypass
// (ka-0000001946) only takes effect if the daemon is told which fleets are
// privileged. The live gitlab-bridge token loads from the bridge-tokens Secret
// via BEADS_FLEET_MAP (no per-token privileged field), so without
// BEADS_PRIVILEGED_FLEETS the bridge 403s on every cross-owner produces dep +
// mr_url back-fill. Assert the env var renders and defaults to gitlab-bridge.
func TestBeadsPrivilegedFleetsRenders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml")
	block := blockAfter(out, "BEADS_PRIVILEGED_FLEETS")
	if block == "" {
		t.Fatal("expected BEADS_PRIVILEGED_FLEETS env var in rendered beads deployment")
	}
	if !strings.Contains(block, "gitlab-bridge") {
		t.Errorf("BEADS_PRIVILEGED_FLEETS must default to gitlab-bridge so the "+
			"bridge gets the ownership bypass (gd-ShWLbKKzGs); got:\n%s", block)
	}
}

func TestBeadsEmbedApiKeySecretRefIsOptional(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml",
		"beads.embedding.enabled=true",
		"beads.embedding.apiKeySecret.name=litellm-gardenio-key",
	)
	block := blockAfter(out, "BEADS_EMBED_API_KEY")
	if block == "" {
		t.Fatal("expected BEADS_EMBED_API_KEY env var when embedding is enabled")
	}
	if !strings.Contains(block, "optional: true") {
		t.Errorf("BEADS_EMBED_API_KEY secretKeyRef must be optional so a missing/"+
			"unsynced embedding key Secret does not wedge the pod (ka-0000002852); got:\n%s", block)
	}
}
