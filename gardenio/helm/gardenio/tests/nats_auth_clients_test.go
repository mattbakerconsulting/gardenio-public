package tests

import (
	"strings"
	"testing"
)

// These tests guard the client-side token wiring that makes flipping
// nats.auth.enabled + subjectIsolation safe (coordinated rollout
// ka-0000006513). Every infra-class NATS client must authenticate with the
// infra token once subject isolation is on; a component whose token env is
// missing connects anonymously and is REFUSED by the server — a silent
// event-plane outage the pods themselves survive (RetryOnFailedConnect), so
// only these render tests and a live E2E would catch it.

var subjectIsolationFlags = []string{
	"nats.enabled=true",
	"nats.auth.enabled=true",
	"nats.auth.subjectIsolation.enabled=true",
}

func renderWithIsolation(t *testing.T, showOnly string, extra ...string) string {
	t.Helper()
	return helmTemplate(t, showOnly, append(append([]string{}, subjectIsolationFlags...), extra...)...)
}

// TestNatsAuthBeadsDaemonGetsInfraToken: the beads daemon publishes every
// bead event and owns the JetStream streams — it must get BEADS_NATS_TOKEN
// from the infra creds Secret under subject isolation.
func TestNatsAuthBeadsDaemonGetsInfraToken(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/beads/deployment.yaml")
	if !strings.Contains(out, "BEADS_NATS_TOKEN") {
		t.Fatal("beads daemon must receive BEADS_NATS_TOKEN under nats.auth.subjectIsolation")
	}
	if !strings.Contains(out, "test-nats-infra-creds") {
		t.Error("BEADS_NATS_TOKEN must come from the -nats-infra-creds Secret")
	}
}

// TestNatsAuthBeadsDaemonNoTokenWithoutIsolation: without isolation the env
// must not be emitted (no phantom secretKeyRef onto a Secret that is not
// rendered).
func TestNatsAuthBeadsDaemonNoTokenWithoutIsolation(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml", "nats.enabled=true")
	if strings.Contains(out, "BEADS_NATS_TOKEN") {
		t.Error("BEADS_NATS_TOKEN must not be set when subjectIsolation is disabled")
	}
}

// TestNatsAuthCoopmuxUsesInfraToken: coopmux subscribes coop.events.> and the
// coop.mux.session.> relay — infra-class access. Its historical token source
// (the beads daemon bearer via gardenio.coopmux.authTokenSecretName) is NOT a
// NATS credential; under isolation it must be repointed at the infra Secret.
func TestNatsAuthCoopmuxUsesInfraToken(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/coopmux/deployment.yaml",
		"coopmux.enabled=true")
	// Extract just the COOP_MUX_NATS_TOKEN env entry (not the $(...) arg
	// reference) and check its source.
	idx := strings.Index(out, "name: COOP_MUX_NATS_TOKEN")
	if idx < 0 {
		t.Fatal("coopmux must receive COOP_MUX_NATS_TOKEN when NATS is enabled")
	}
	window := out[idx:min(idx+300, len(out))]
	if !strings.Contains(window, "test-nats-infra-creds") {
		t.Errorf("COOP_MUX_NATS_TOKEN must come from the -nats-infra-creds Secret under subject isolation, got:\n%s", window)
	}
}

// TestNatsAuthCoopmuxOwnSecretWithoutIsolation: no isolation -> coopmux sends
// a NATS token only from nats.auth.clientTokenSecret, never the beads bearer or
// the agent-facing coopmux token (gd-5QYwXtV7IA). Unset sends none, and the
// --nats-token arg is dropped with it (an undefined $(VAR) would be passed
// literally).
func TestNatsAuthCoopmuxOwnSecretWithoutIsolation(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/coopmux/deployment.yaml",
		"nats.enabled=true", "coopmux.enabled=true", "beads.tokenSecret=kd-beads-token", "coopmux.coopmuxTokenSecret=coopmux-agent-token")
	if strings.Contains(out, "COOP_MUX_NATS_TOKEN") {
		t.Errorf("coopmux must not send a NATS token when none is configured, got:\n%s", out)
	}
	out = helmTemplate(t, "templates/coopmux/deployment.yaml",
		"nats.enabled=true", "coopmux.enabled=true", "beads.tokenSecret=kd-beads-token", "coopmux.coopmuxTokenSecret=coopmux-agent-token",
		"nats.auth.clientTokenSecret=nats-client-token")
	idx := strings.Index(out, "name: COOP_MUX_NATS_TOKEN")
	if idx < 0 {
		t.Fatal("coopmux must receive COOP_MUX_NATS_TOKEN when nats.auth.clientTokenSecret is set")
	}
	window := out[idx:min(idx+300, len(out))]
	if !strings.Contains(window, "nats-client-token") || strings.Contains(window, "kd-beads-token") || strings.Contains(window, "coopmux-agent-token") {
		t.Errorf("COOP_MUX_NATS_TOKEN must come from nats-client-token only, got:\n%s", window)
	}
}

// TestNatsAuthTriggerEngineUsesInfraToken: startSchedulerNATSListeners
// subscribes decision_resolved/bead_closed/mail_sent — infra-class access.
// Its historical token source (the beads daemon bearer via
// gardenio.beads.tokenSecretName) is NOT a NATS credential; under isolation
// it must be repointed at the infra Secret, same as coopmux (gd-BBbXQBZTX0).
func TestNatsAuthTriggerEngineUsesInfraToken(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/trigger-engine/deployment.yaml",
		"triggerEngine.enabled=true")
	idx := strings.Index(out, "name: NATS_TOKEN")
	if idx < 0 {
		t.Fatal("trigger-engine must receive NATS_TOKEN when NATS is enabled")
	}
	window := out[idx:min(idx+300, len(out))]
	if !strings.Contains(window, "test-nats-infra-creds") {
		t.Errorf("trigger-engine NATS_TOKEN must come from the -nats-infra-creds Secret under subject isolation, got:\n%s", window)
	}
}

// TestNatsAuthTriggerEngineOwnSecretWithoutIsolation: no isolation ->
// trigger-engine sends a NATS token only from nats.auth.clientTokenSecret,
// never the beads bearer (gd-5QYwXtV7IA).
func TestNatsAuthTriggerEngineOwnSecretWithoutIsolation(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/trigger-engine/deployment.yaml",
		"nats.enabled=true", "triggerEngine.enabled=true", "beads.tokenSecret=kd-beads-token")
	if strings.Contains(out, "name: NATS_TOKEN") {
		t.Errorf("trigger-engine must not send a NATS token when none is configured, got:\n%s", out)
	}
	if !strings.Contains(out, `value: "nats://`) {
		t.Error("trigger-engine must receive a plain NATS_URL value")
	}
	out = helmTemplate(t, "templates/trigger-engine/deployment.yaml",
		"nats.enabled=true", "triggerEngine.enabled=true", "beads.tokenSecret=kd-beads-token",
		"nats.auth.clientTokenSecret=nats-client-token")
	idx := strings.Index(out, "name: NATS_TOKEN")
	if idx < 0 {
		t.Fatal("trigger-engine must receive NATS_TOKEN when nats.auth.clientTokenSecret is set")
	}
	window := out[idx:min(idx+300, len(out))]
	if !strings.Contains(window, "nats-client-token") || strings.Contains(window, "kd-beads-token") {
		t.Errorf("NATS_TOKEN must come from nats-client-token only, got:\n%s", window)
	}
}

// TestNatsAuthSlackBridgeGetsInfraToken: the hookstream subscribes hooks.> and
// coop.events.> — infra-class read. agents.natsTokenSecret (when set) keeps
// precedence as the explicit operator override.
func TestNatsAuthSlackBridgeGetsInfraToken(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true")
	idx := strings.Index(out, "COOP_NATS_TOKEN")
	if idx < 0 {
		t.Fatal("slack-bridge must receive COOP_NATS_TOKEN under nats.auth.subjectIsolation")
	}
	window := out[idx:min(idx+300, len(out))]
	if !strings.Contains(window, "test-nats-infra-creds") {
		t.Errorf("slack-bridge COOP_NATS_TOKEN must come from the -nats-infra-creds Secret, got:\n%s", window)
	}
}

// TestNatsAuthSlackBridgeOverridePrecedence: an explicit agents.natsTokenSecret
// wins over the subject-isolation default.
func TestNatsAuthSlackBridgeOverridePrecedence(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true", "agents.natsTokenSecret=my-explicit-secret")
	idx := strings.Index(out, "COOP_NATS_TOKEN")
	if idx < 0 {
		t.Fatal("slack-bridge must receive COOP_NATS_TOKEN")
	}
	window := out[idx:min(idx+300, len(out))]
	if !strings.Contains(window, "my-explicit-secret") {
		t.Errorf("explicit agents.natsTokenSecret must take precedence, got:\n%s", window)
	}
}

// TestNatsAuthBeadsViewerGetsInfraToken: telepathy hub subscribes
// coop.events.messages.*.thinking (cross-agent read) — infra token.
func TestNatsAuthBeadsViewerGetsInfraToken(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true")
	idx := strings.Index(out, "COOP_NATS_TOKEN")
	if idx < 0 {
		t.Fatal("beads-viewer must receive COOP_NATS_TOKEN under nats.auth.subjectIsolation")
	}
	window := out[idx:min(idx+300, len(out))]
	if !strings.Contains(window, "test-nats-infra-creds") {
		t.Errorf("beads-viewer COOP_NATS_TOKEN must come from the -nats-infra-creds Secret, got:\n%s", window)
	}
}

// TestNatsAuthBackupCronjobGetsInfraToken: the JetStream account backup needs
// full access; under isolation the legacy values-inline nats.auth.token path
// is empty, so it must source the infra Secret instead.
func TestNatsAuthBackupCronjobGetsInfraToken(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/backup/nats-cronjob.yaml",
		"backup.nats.enabled=true")
	if !strings.Contains(out, "NATS_TOKEN") {
		t.Fatal("nats backup cronjob must receive NATS_TOKEN under nats.auth.subjectIsolation")
	}
	if !strings.Contains(out, "test-nats-infra-creds") {
		t.Error("backup NATS_TOKEN must come from the -nats-infra-creds Secret")
	}
}

// TestNatsAuthControllerWiring (pre-existing behavior, kept as a guard): the
// controller gets the infra token as NATS_TOKEN and points agent pods at the
// restricted agent-creds Secret.
func TestNatsAuthControllerWiring(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/controller/deployment.yaml",
		"agents.enabled=true")
	if !strings.Contains(out, "NATS_AGENT_USER_SECRET") {
		t.Error("controller must receive NATS_AGENT_USER_SECRET (restricted token for agent pods)")
	}
	if !strings.Contains(out, "test-nats-agent-creds") {
		t.Error("NATS_AGENT_USER_SECRET must name the -nats-agent-creds Secret")
	}
	if !strings.Contains(out, "test-nats-infra-creds") {
		t.Error("controller NATS_TOKEN must come from the -nats-infra-creds Secret")
	}
}

// TestNatsAuthAgentUserAllowsHookRelay: gb hook relay (agent pods) falls back
// to publishing hooks.<agent>.<event> straight to NATS when the daemon HTTP
// publish fails. Without the publish allow the fallback is silently refused.
func TestNatsAuthAgentUserAllowsHookRelay(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/nats/statefulset.yaml")
	if !strings.Contains(out, `"hooks.>"`) {
		t.Error("agent user must be allowed to publish hooks.> (gb hook relay NATS fallback)")
	}
	// Must be publish-only: hooks.> must not appear twice (once per allow list).
	if strings.Count(out, `"hooks.>"`) != 1 {
		t.Error("hooks.> must appear only in the publish allow, never subscribe")
	}
}
