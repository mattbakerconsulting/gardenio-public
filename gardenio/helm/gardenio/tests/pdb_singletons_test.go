package tests

import (
	"strings"
	"testing"
)

// Persistent gardenio-system spine singletons (nats, postgres, keycloak-runtime,
// vmsingle, vtsingle) must get a PodDisruptionBudget at EVERY replica count —
// unlike the replica-gated per-component PDBs (alertmanager/coopmux/litellm),
// these are single-replica PVC-backed StatefulSets whose eviction during a
// managed-nodegroup node roll stranded nats-0 Pending on the EKS 1.31->1.32
// hop-2 roll (ka-0000001453). The budget must be maxUnavailable (never
// minAvailable), or a 1-replica PDB becomes maxUnavailable:0 and wedges the
// roll with PodEvictionFailure. These tests lock that contract.

// singletonPDBCase pairs a chart component with the PDB resource name the
// consolidated template is expected to emit for it.
type singletonPDBCase struct {
	name    string // sub-test / PDB metadata.name suffix
	enable  []string
	pdbName string
}

func TestPlatformPDB_SingletonsRenderAtOneReplica(t *testing.T) {
	t.Parallel()
	cases := []singletonPDBCase{
		{"nats", []string{"nats.enabled=true"}, "test-nats"},
		{"postgres", []string{"postgres.enabled=true"}, "test-postgres"},
		{"keycloak-runtime", []string{"keycloak.enabled=true", "keycloak.runtime.enabled=true"}, "test-keycloak-runtime"},
		{"vmsingle", []string{"vmsingle.enabled=true"}, "test-vmsingle"},
		{"vtsingle", []string{"vtsingle.enabled=true"}, "test-victoria-traces"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := helmTemplate(t, "templates/pdb-singletons.yaml", c.enable...)
			if !strings.Contains(out, "kind: PodDisruptionBudget") {
				t.Fatalf("%s: expected a PDB at 1 replica, got:\n%s", c.name, out)
			}
			if !strings.Contains(out, "name: "+c.pdbName) {
				t.Errorf("%s: expected PDB name %q, got:\n%s", c.name, c.pdbName, out)
			}
			// Must be maxUnavailable (drain-safe on a singleton), never
			// minAvailable (would wedge a managed-nodegroup roll).
			if !strings.Contains(out, "maxUnavailable: 1") {
				t.Errorf("%s: expected maxUnavailable: 1, got:\n%s", c.name, out)
			}
			if strings.Contains(out, "minAvailable") {
				t.Errorf("%s: PDB must NOT use minAvailable on a singleton, got:\n%s", c.name, out)
			}
		})
	}
}

// The keycloak-runtime PDB is gated on the runtime sub-toggle, not just
// keycloak.enabled — the ClusterKeycloak-only deployment has no in-box pod to
// protect.
func TestPlatformPDB_KeycloakRuntimeGatedOnRuntime(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/pdb-singletons.yaml",
		"keycloak.enabled=true",
		"keycloak.runtime.enabled=false",
		// something else on so the file is not entirely empty
		"nats.enabled=true",
	)
	if err == nil && strings.Contains(out, "keycloak-runtime") {
		t.Errorf("keycloak-runtime PDB must NOT render when runtime disabled, got:\n%s", out)
	}
}

// platformPdb.enabled=false is a hard opt-out for the whole set.
func TestPlatformPDB_OptOut(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/pdb-singletons.yaml",
		"nats.enabled=true",
		"postgres.enabled=true",
		"platformPdb.enabled=false",
	)
	if err == nil && strings.Contains(out, "kind: PodDisruptionBudget") {
		t.Errorf("platformPdb.enabled=false must render no PDBs, got:\n%s", out)
	}
}

// A disabled component emits no PDB even when platformPdb is on.
func TestPlatformPDB_DisabledComponentSkipped(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, "templates/pdb-singletons.yaml",
		"nats.enabled=true",
		"postgres.enabled=false",
	)
	if err != nil {
		return // nothing rendered at all is also acceptable
	}
	if strings.Contains(out, "test-postgres") {
		t.Errorf("disabled postgres must not get a PDB, got:\n%s", out)
	}
	if !strings.Contains(out, "test-nats") {
		t.Errorf("enabled nats should still get its PDB, got:\n%s", out)
	}
}

// maxUnavailable is operator-tunable via the top-level knob.
func TestPlatformPDB_MaxUnavailableTunable(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/pdb-singletons.yaml",
		"nats.enabled=true",
		"platformPdb.maxUnavailable=2",
	)
	if !strings.Contains(out, "maxUnavailable: 2") {
		t.Errorf("expected maxUnavailable: 2 override, got:\n%s", out)
	}
}
