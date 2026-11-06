package tests

import (
	"strings"
	"testing"
)

// ANTHROPIC_DEFAULT_HAIKU_MODEL wiring on the agent-controller deployment
// (ka-0000005599). The Claude Code harness `haiku` alias otherwise resolves —
// via its bundled model catalog — to the DATED snapshot id
// "claude-haiku-4-5-20251001" the fleet's LiteLLM virtual-key ACL rejects (401
// "Tried to access claude-haiku-4-5-20251001"), so a haiku crew is dead on
// arrival (and the harness's small-model ops like WebFetch 401 silently). The
// controller injects AGENT_DEFAULT_HAIKU_MODEL, which it forwards as
// ANTHROPIC_DEFAULT_HAIKU_MODEL onto agent pods; the harness reads that env
// FIRST when resolving the alias.
//
// The env var is emitted UNCONDITIONALLY (quoted) — not truthy-guarded — so an
// explicit empty override reaches the controller as a SET-but-empty value that
// os.LookupEnv distinguishes from unset, disabling the pin. These tests pin
// both the default value and the empty escape hatch.

const haikuModelTpl = "templates/controller/deployment.yaml"

// TestDefaultHaikuModel_DefaultRenders: with no override, the controller carries
// AGENT_DEFAULT_HAIKU_MODEL set to the ACL-allowed rolling pass-through model
// group LiteLLM serves. The value is asserted verbatim: the whole point is that
// it must NOT be the dated "claude-haiku-4-5-20251001" snapshot (the key ACL is
// checked against the literal request model name before model_group_alias
// resolution).
func TestDefaultHaikuModel_DefaultRenders(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, haikuModelTpl, "agents.enabled=true")
	for _, want := range []string{
		"name: AGENT_DEFAULT_HAIKU_MODEL",
		`value: "anthropic/claude-haiku-4-5"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("default render must contain %q; got:\n%s", want, out)
		}
	}
	// The dated snapshot must NOT be what the chart pins — that is the value the
	// ACL rejects, and the reason the pin exists at all.
	if strings.Contains(out, `value: "claude-haiku-4-5-20251001"`) {
		t.Errorf("default render must NOT pin the dated snapshot (ACL rejects it); got:\n%s", out)
	}
}

// TestDefaultHaikuModel_EmptyEscapeHatch: an explicit empty override must still
// render the env var (as value: "") so the controller sees it SET-but-empty and
// disables the pin. A truthy if-guard would drop the key entirely, and the
// controller's baked default would silently re-apply — this pins the escape
// hatch against that regression.
func TestDefaultHaikuModel_EmptyEscapeHatch(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, haikuModelTpl,
		"agents.enabled=true",
		"agents.defaultHaikuModel=",
	)
	if !strings.Contains(out, "name: AGENT_DEFAULT_HAIKU_MODEL") {
		t.Fatalf("empty override must still render the env key (set-but-empty); got:\n%s", out)
	}
	if !strings.Contains(out, `value: ""`) {
		t.Errorf("empty override must render value: \"\"; got:\n%s", out)
	}
}

// TestDefaultHaikuModel_Override: a non-empty override is rendered verbatim.
func TestDefaultHaikuModel_Override(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, haikuModelTpl,
		"agents.enabled=true",
		"agents.defaultHaikuModel=anthropic/claude-haiku-4-5-custom",
	)
	if !strings.Contains(out, `value: "anthropic/claude-haiku-4-5-custom"`) {
		t.Errorf("override must render verbatim; got:\n%s", out)
	}
}
