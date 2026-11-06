package tests

import (
	"strings"
	"testing"
)

// FREEZE_ARMED_GUARD_ENABLED (ka-0000006667 / MR !3817) is the pre-freeze
// watcher-liveness guard. MR !3817 wired the controller Go path but added no
// chart plumbing; bug ka-0000001058 adds gvisor.freezeThaw.armedGuardEnabled so
// the guard can be enabled via the values overlay. Default-off; only rendered
// inside the freezeThaw.enabled block.

func TestArmedGuard_RendersWhenEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"gvisor.enabled=true",
		"gvisor.namespace.name=gardenio-gvisor",
		"gvisor.freezeThaw.enabled=true",
		"gvisor.freezeThaw.armedGuardEnabled=true",
	)
	if !strings.Contains(out, "FREEZE_ARMED_GUARD_ENABLED") {
		t.Fatalf("FREEZE_ARMED_GUARD_ENABLED must render when armedGuardEnabled=true; got:\n%s", out)
	}
}

// The guard is gated INSIDE freezeThaw.enabled: with freeze/thaw off, the env
// must not render even if armedGuardEnabled is set (it is meaningless without a
// freeze path to interrupt).
func TestArmedGuard_GatedOnFreezeThawEnabled(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/controller/deployment.yaml",
		"agents.enabled=true",
		"gvisor.freezeThaw.enabled=false",
		"gvisor.freezeThaw.armedGuardEnabled=true",
	)
	if strings.Contains(out, "FREEZE_ARMED_GUARD_ENABLED") {
		t.Errorf("FREEZE_ARMED_GUARD_ENABLED must be gated inside freezeThaw.enabled; got:\n%s", out)
	}
}
