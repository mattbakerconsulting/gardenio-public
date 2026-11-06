package tests

import "testing"

// gd-UBeJ3kVicQ: tenant support (Lands Track B §6.1). slackBridge.landsSupport
// names who a tenant's #support post DMs and where the text-free pointer goes
// when it cannot; both are unset by default (beads are still filed).

func TestLandsSupport_DefaultRendersNoEnv(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml", "slackBridge.enabled=true")
	for _, key := range []string{"LANDS_SUPPORT_OPERATOR", "LANDS_SUPPORT_POINTER_CHANNEL"} {
		if v, ok := envValueAfter(out, key); ok {
			t.Errorf("%s=%q rendered with landsSupport unset", key, v)
		}
	}
}

func TestLandsSupport_RendersOperatorAndPointerChannel(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/slack-bridge/deployment.yaml", "slackBridge.enabled=true",
		"slackBridge.landsSupport.operator=owner@example.com",
		"slackBridge.landsSupport.pointerChannel=C0123456789")
	for key, want := range map[string]string{
		"LANDS_SUPPORT_OPERATOR":        "owner@example.com",
		"LANDS_SUPPORT_POINTER_CHANNEL": "C0123456789",
	} {
		if v, ok := envValueAfter(out, key); !ok || v != want {
			t.Errorf("%s = %q (set %v), want %q", key, v, ok, want)
		}
	}
}
