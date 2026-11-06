package tests

import (
	"strings"
	"testing"
)

// Spawn-time VPA memory-stamp env wiring on the agent-controller deployment
// (task ka-0000004555, epic ka-0000005189; stamper podmanager/vpareco.go,
// ka-0000004606).
//
// The chart historically rendered the VPA *objects* (vpa-crew-rightsizing.yaml)
// but NEVER wired the controller env that makes the controller CONSUME their
// recommendations at spawn — so crew/gardener pods stayed at the flat 8Gi
// default even with mature recos. These tests pin the env plumbing:
//   - default OFF: none of the four VPA_* env vars render (zero blast radius),
//   - stamp.enabled=true: all four render with the configured values,
//   - the object-name template is emitted verbatim (a wrong/absent template
//     silently loses the savings — the stamper does a single Get with no
//     fallback), so it must be asserted, not just presence-checked.

const vpaStampTpl = "templates/controller/deployment.yaml"

// TestVPAStamp_DefaultOff: with vpaRightSizing.stamp unset, the controller must
// carry NONE of the VPA stamp env vars. This is the safe default — the chart
// ships the stamp off and an env overlay opts in.
func TestVPAStamp_DefaultOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, vpaStampTpl, "agents.enabled=true")
	for _, forbidden := range []string{
		"VPA_MEMORY_STAMP_ENABLED",
		"VPA_OBJECT_NAME_TEMPLATE",
		"VPA_MEMORY_FLOOR",
		"VPA_MEMORY_MAX",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%s must NOT render by default (stamp.enabled unset); got:\n%s", forbidden, out)
		}
	}
}

// TestVPAStamp_EnabledObjectsButStampOff: enabling the VPA *objects*
// (vpaRightSizing.enabled=true) must NOT by itself turn on the controller
// stamp — object collection and the spawn stamp are independently gated. This
// pins that a fleet collecting recommendations does not silently start stamping.
func TestVPAStamp_EnabledObjectsButStampOff(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, vpaStampTpl,
		"agents.enabled=true",
		"vpaRightSizing.enabled=true",
	)
	if strings.Contains(out, "VPA_MEMORY_STAMP_ENABLED") {
		t.Errorf("vpaRightSizing.enabled=true must NOT enable the stamp on its own; got:\n%s", out)
	}
}

// TestVPAStamp_RendersWhenEnabled: stamp.enabled=true renders all four env vars
// with the configured values. The object-name template value is asserted
// verbatim because the stamper resolves a single object name from it with no
// fallback — a wrong template silently leaves classes at 8Gi.
func TestVPAStamp_RendersWhenEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, vpaStampTpl,
		"agents.enabled=true",
		"vpaRightSizing.enabled=true",
		"vpaRightSizing.stamp.enabled=true",
		"vpaRightSizing.stamp.objectNameTemplate=gardenio-crew-default-{role}",
		"vpaRightSizing.stamp.memoryFloor=1500Mi",
		"vpaRightSizing.stamp.memoryMax=16Gi",
	)
	for _, want := range []string{
		"name: VPA_MEMORY_STAMP_ENABLED",
		"name: VPA_OBJECT_NAME_TEMPLATE",
		`value: "gardenio-crew-default-{role}"`,
		"name: VPA_MEMORY_FLOOR",
		`value: "1500Mi"`,
		"name: VPA_MEMORY_MAX",
		`value: "16Gi"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stamp.enabled=true must render %q; got:\n%s", want, out)
		}
	}
}

// TestVPAStamp_OmittedTemplateSkipsTemplateEnv: with the stamp on but no
// objectNameTemplate, VPA_OBJECT_NAME_TEMPLATE must be omitted (the controller
// falls back to its single-object default). The master flag still renders. This
// pins that the optional sub-values are each independently gated.
func TestVPAStamp_OmittedTemplateSkipsTemplateEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, vpaStampTpl,
		"agents.enabled=true",
		"vpaRightSizing.enabled=true",
		"vpaRightSizing.stamp.enabled=true",
		"vpaRightSizing.stamp.objectNameTemplate=",
		"vpaRightSizing.stamp.memoryFloor=",
		"vpaRightSizing.stamp.memoryMax=",
	)
	if !strings.Contains(out, "VPA_MEMORY_STAMP_ENABLED") {
		t.Fatalf("master flag must render when stamp.enabled=true; got:\n%s", out)
	}
	for _, forbidden := range []string{
		"VPA_OBJECT_NAME_TEMPLATE",
		"VPA_MEMORY_FLOOR",
		"VPA_MEMORY_MAX",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("%s must be omitted when its value is empty; got:\n%s", forbidden, out)
		}
	}
}
