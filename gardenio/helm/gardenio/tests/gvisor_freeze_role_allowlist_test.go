package tests

import (
	"strings"
	"testing"
)

// WS3 freeze/thaw canary role allowlist (task ka-0000000138, epic ka-0000004429).
// The controller env FREEZE_THAW_ROLE_ALLOWLIST narrows freeze-eligibility to a
// canary set of roles so freeze/thaw can be widened one role at a time on a
// substrate where freezeThaw.enabled is already globally on. Rendering rules:
//   - default (empty roleAllowlist): env ABSENT → no role filter → pre-WS3
//     behaviour verbatim (this is what makes merging the value a no-op).
//   - non-empty: env present, comma-joined.
//   - only meaningful inside freezeThaw.enabled — nothing renders when the
//     lifecycle flag is off.

const freezeAllowlistBase = "agents.enabled=true,gvisor.enabled=true,gvisor.namespace.name=gardenio-gvisor,gvisor.freezeThaw.enabled=true"

func TestGvisorFreezeRoleAllowlist_DefaultAbsent(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml", freezeAllowlistBase)
	if strings.Contains(out, "FREEZE_THAW_ROLE_ALLOWLIST") {
		t.Error("FREEZE_THAW_ROLE_ALLOWLIST must be ABSENT when roleAllowlist is empty (no role filter — pre-WS3 behaviour)")
	}
	// Sanity: the lifecycle flag itself did render, so an absent allowlist is a
	// real "no filter", not a mis-rendered block.
	if !strings.Contains(out, "FREEZE_THAW_ENABLED") {
		t.Fatal("FREEZE_THAW_ENABLED should render when freezeThaw.enabled=true")
	}
}

func TestGvisorFreezeRoleAllowlist_SetRendersJoined(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		freezeAllowlistBase,
		"gvisor.freezeThaw.roleAllowlist[0]=gvisor-poc",
		"gvisor.freezeThaw.roleAllowlist[1]=plan-gardener",
	)
	if !strings.Contains(out, "FREEZE_THAW_ROLE_ALLOWLIST") {
		t.Fatal("FREEZE_THAW_ROLE_ALLOWLIST should render when roleAllowlist is set")
	}
	if !strings.Contains(out, "gvisor-poc,plan-gardener") {
		t.Errorf("allowlist should render comma-joined 'gvisor-poc,plan-gardener', got:\n%s", out)
	}
}

func TestGvisorFreezeRoleAllowlist_AbsentWhenLifecycleDisabled(t *testing.T) {
	t.Parallel()
	// roleAllowlist set but freezeThaw.enabled=false — the whole freeze env
	// block is gated off, so the allowlist env must not leak out.
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"gvisor.enabled=true",
		"gvisor.namespace.name=gardenio-gvisor",
		"gvisor.freezeThaw.enabled=false",
		"gvisor.freezeThaw.roleAllowlist[0]=plan-gardener",
	)
	if strings.Contains(out, "FREEZE_THAW_ROLE_ALLOWLIST") {
		t.Error("FREEZE_THAW_ROLE_ALLOWLIST must not render when freezeThaw.enabled=false")
	}
}
