package tests

import (
	"strings"
	"testing"
)

// ANTHROPIC_DEFAULT_FABLE_MODEL wiring on the agent-controller deployment
// (ka-0000003175). The Claude Code harness `fable` alias otherwise resolves —
// via its bundled model catalog — to a bare "claude-sonnet-5" id the fleet's
// LiteLLM virtual-key ACL rejects (401 "Tried to access claude-sonnet-5"), so a
// fable crew is dead on arrival. The controller injects
// AGENT_DEFAULT_FABLE_MODEL, which it forwards as ANTHROPIC_DEFAULT_FABLE_MODEL
// onto agent pods; the harness reads that env FIRST when resolving the alias.
//
// The env var is emitted UNCONDITIONALLY (quoted) — not truthy-guarded — so an
// explicit empty override reaches the controller as a SET-but-empty value that
// os.LookupEnv distinguishes from unset, disabling the pin. These tests pin
// both the default value and the empty escape hatch.

const fableModelTpl = "templates/controller/deployment.yaml"

// TestDefaultFableModel_DefaultRenders: with no override, the controller carries
// AGENT_DEFAULT_FABLE_MODEL set to the ACL-allowed "-oauth" model group LiteLLM
// serves. The value is asserted verbatim: the whole point is that it must NOT be
// a bare "claude-fable-5-1" (the key ACL is checked against the literal request
// model name before model_group_alias resolution).
func TestDefaultFableModel_DefaultRenders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, fableModelTpl, "agents.enabled=true")
	for _, want := range []string{
		"name: AGENT_DEFAULT_FABLE_MODEL",
		`value: "claude-fable-5-1-oauth"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("default render must contain %q; got:\n%s", want, out)
		}
	}
}

// TestDefaultFableModel_EmptyEscapeHatch: an explicit empty override must still
// render the env var (as value: "") so the controller sees it SET-but-empty and
// disables the pin. A truthy if-guard would drop the key entirely, and the
// controller's baked default would silently re-apply — this pins the escape
// hatch against that regression.
func TestDefaultFableModel_EmptyEscapeHatch(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, fableModelTpl,
		"agents.enabled=true",
		"agents.defaultFableModel=",
	)
	if !strings.Contains(out, "name: AGENT_DEFAULT_FABLE_MODEL") {
		t.Fatalf("empty override must still render the env key (set-but-empty); got:\n%s", out)
	}
	if !strings.Contains(out, `value: ""`) {
		t.Errorf("empty override must render value: \"\"; got:\n%s", out)
	}
}

// TestDefaultFableModel_Override: a non-empty override is rendered verbatim.
func TestDefaultFableModel_Override(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, fableModelTpl,
		"agents.enabled=true",
		"agents.defaultFableModel=claude-fable-5-custom-oauth",
	)
	if !strings.Contains(out, `value: "claude-fable-5-custom-oauth"`) {
		t.Errorf("override must render verbatim; got:\n%s", out)
	}
}
