package tests

import (
	"strings"
	"testing"
)

// VPA crew right-sizing objects + in-place enforce machinery
// (epic ka-0000005189; scoping ka-0000001210; enforce/guards ka-0000003138).
//
// The template renders, behind .Values.vpaRightSizing.enabled (default false):
//   - one replicas:0 placeholder Deployment + one updateMode VPA per class, and
//   - WHEN .Values.vpaRightSizing.updateMode != "Off": per-class mid-run eviction
//     guards (grow-only evictionRequirements, minReplicas, a maxUnavailable:0 PDB).
//
// The HARD GATE for this task is that the default posture is recommendation-only:
// updateMode must default to "Off" and NO disruption-blocking object (PDB) or
// eviction guard may render unless an operator flips the mode value. These tests
// pin that safe default so a regression can't silently arm enforce.

const vpaCrewTpl = "templates/vpa-crew-rightsizing.yaml"

// TestVPACrew_DisabledByDefault: nothing renders unless enabled.
func TestVPACrew_DisabledByDefault(t *testing.T) {
	t.Parallel()
	out, err := helmTemplateAllowFail(t, vpaCrewTpl)
	if err == nil && strings.Contains(out, "kind:") {
		t.Errorf("vpa-crew-rightsizing must render nothing when vpaRightSizing.enabled=false, got:\n%s", out)
	}
}

// TestVPACrew_EnabledDefaultsToReco: enabled but no updateMode override → the
// recommendation-only posture. Every VPA is updateMode:Off and NO enforce guard
// (PDB / evictionRequirements / minReplicas) renders. This is the safe default
// the task ships — turning on enforce must be a deliberate values flip.
func TestVPACrew_EnabledDefaultsToReco(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, vpaCrewTpl, "vpaRightSizing.enabled=true")
	if !strings.Contains(out, "kind: VerticalPodAutoscaler") {
		t.Fatalf("VPAs must render when enabled, got:\n%s", out)
	}
	if !strings.Contains(out, `updateMode: "Off"`) {
		t.Errorf("default updateMode must be Off, got:\n%s", out)
	}
	// Assert on the rendered directive, not the bare substring — the template
	// comment legitimately names InPlaceOrRecreate when documenting the flip.
	if strings.Contains(out, `updateMode: "InPlaceOrRecreate"`) {
		t.Errorf("default render must NOT enable in-place enforce, got:\n%s", out)
	}
	for _, forbidden := range []string{
		"kind: PodDisruptionBudget", // no eviction-blocking object in reco-only
		"evictionRequirements",
		"changeRequirement",
		"minReplicas",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("reco-only (default) render must NOT contain %q, got:\n%s", forbidden, out)
		}
	}
}

// TestVPACrew_EnforceFlipRendersGuards: flipping updateMode to InPlaceOrRecreate
// arms enforce AND, in the same flip, renders all three mid-run eviction guards
// for every class. The guards must never lag the mode.
func TestVPACrew_EnforceFlipRendersGuards(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, vpaCrewTpl,
		"vpaRightSizing.enabled=true",
		"vpaRightSizing.updateMode=InPlaceOrRecreate",
	)
	if !strings.Contains(out, `updateMode: "InPlaceOrRecreate"`) {
		t.Fatalf("updateMode flip not applied, got:\n%s", out)
	}
	for _, want := range []string{
		// Guard 1: grow-only — updater may only act when the target is HIGHER
		// than the current request, so it can never shrink a running pod.
		"evictionRequirements",
		"changeRequirement: TargetHigherThanRequests",
		// Guard 2: minReplicas bounds updater aggressiveness.
		"minReplicas: 1",
		// Guard 3: maxUnavailable:0 PDB blocks the eviction API → no Recreate
		// fallback can delete a live bare crew pod mid-task.
		"kind: PodDisruptionBudget",
		"maxUnavailable: 0",
		"name: vpa-norecreate-default-crew",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("enforce render missing guard %q, got:\n%s", want, out)
		}
	}
	// The PDB must select the REAL bare crew pods by class labels (not the
	// 0-replica placeholder), or it protects nothing.
	if !strings.Contains(out, "gardenio.io/role: crew") {
		t.Errorf("PDB/VPA must carry the class role label, got:\n%s", out)
	}
}

// TestVPACrew_PDBSelectsRealCrewPods: the no-Recreate PDB's selector must match
// the class crew labels — the same labels the live bare pods carry — so the
// eviction API actually refuses to evict them. A project-class PDB must include
// the project label; a role-default PDB must select on role only.
func TestVPACrew_PDBSelectsRealCrewPods(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, vpaCrewTpl,
		"vpaRightSizing.enabled=true",
		"vpaRightSizing.updateMode=InPlaceOrRecreate",
	)
	// Project-specific class (monorepo crew) selector must carry both labels.
	mono := sectionFor(out, "name: vpa-norecreate-monorepo-crew")
	if mono == "" {
		t.Fatalf("missing monorepo-crew PDB, got:\n%s", out)
	}
	if !strings.Contains(mono, "gardenio.io/project: monorepo") ||
		!strings.Contains(mono, "gardenio.io/role: crew") {
		t.Errorf("project-class PDB selector must match {project,role}, got:\n%s", mono)
	}
}

// TestVPACrew_GrowOnlyToggle: guards.growOnly=false drops the evictionRequirements
// (allows down-resize) but leaves minReplicas + the PDB intact.
func TestVPACrew_GrowOnlyToggle(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, vpaCrewTpl,
		"vpaRightSizing.enabled=true",
		"vpaRightSizing.updateMode=InPlaceOrRecreate",
		"vpaRightSizing.guards.growOnly=false",
	)
	if strings.Contains(out, "changeRequirement") {
		t.Errorf("growOnly=false must drop evictionRequirements, got:\n%s", out)
	}
	if !strings.Contains(out, "kind: PodDisruptionBudget") {
		t.Errorf("PDB must survive growOnly=false, got:\n%s", out)
	}
}

// TestVPACrew_PDBToggle: pdb.enabled=false drops the PDB (allows Recreate
// fallback) but leaves the grow-only guard intact. Belt-and-suspenders independence.
func TestVPACrew_PDBToggle(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, vpaCrewTpl,
		"vpaRightSizing.enabled=true",
		"vpaRightSizing.updateMode=InPlaceOrRecreate",
		"vpaRightSizing.pdb.enabled=false",
	)
	if strings.Contains(out, "kind: PodDisruptionBudget") {
		t.Errorf("pdb.enabled=false must drop the PDB, got:\n%s", out)
	}
	if !strings.Contains(out, "changeRequirement: TargetHigherThanRequests") {
		t.Errorf("grow-only guard must survive pdb.enabled=false, got:\n%s", out)
	}
}

// TestVPACrew_MinReplicasOverride: the minReplicas guard is configurable.
func TestVPACrew_MinReplicasOverride(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, vpaCrewTpl,
		"vpaRightSizing.enabled=true",
		"vpaRightSizing.updateMode=InPlaceOrRecreate",
		"vpaRightSizing.guards.minReplicas=2",
	)
	if !strings.Contains(out, "minReplicas: 2") {
		t.Errorf("minReplicas override not applied, got:\n%s", out)
	}
}

// sectionFor returns the YAML document containing the marker line, bounded by the
// surrounding `---` separators. Used to assert on one object's selector in
// isolation (multiple PDBs render in the same stream).
func sectionFor(out, marker string) string {
	for d := range strings.SplitSeq(out, "\n---") {
		if strings.Contains(d, marker) {
			return d
		}
	}
	return ""
}
