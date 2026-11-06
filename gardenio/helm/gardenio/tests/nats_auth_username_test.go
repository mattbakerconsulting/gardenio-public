package tests

import (
	"strings"
	"testing"
)

// These tests guard the USERNAME half of the subject-isolation client
// contract (ka-0000000002). The server's multi-user authorization only
// matches user/password entries, so every client must send a username
// alongside the delivered secret — a component with the token env but no
// username env connects with bare-token auth and is REFUSED, the same silent
// event-plane outage the token tests guard against.

// envWindow returns the rendered text following the first occurrence of
// needle, for asserting on an env entry's immediate context.
func envWindow(t *testing.T, out, needle string) string {
	t.Helper()
	idx := strings.Index(out, needle)
	if idx < 0 {
		t.Fatalf("rendered output missing %q", needle)
	}
	return out[idx:min(idx+200, len(out))]
}

func TestNatsUserBeadsDaemon(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/beads/deployment.yaml")
	w := envWindow(t, out, "name: BEADS_NATS_USER")
	if !strings.Contains(w, "value: infra") {
		t.Errorf("beads daemon must authenticate as the infra user, got:\n%s", w)
	}
}

func TestNatsUserBeadsDaemonAbsentWithoutIsolation(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/beads/deployment.yaml", "nats.enabled=true")
	if strings.Contains(out, "BEADS_NATS_USER") {
		t.Error("BEADS_NATS_USER must not be set when subjectIsolation is disabled")
	}
}

func TestNatsUserController(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/controller/deployment.yaml", "agents.enabled=true")
	w := envWindow(t, out, "name: NATS_USER")
	if !strings.Contains(w, "value: infra") {
		t.Errorf("controller must authenticate as the infra user, got:\n%s", w)
	}
}

func TestNatsUserCoopmux(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/coopmux/deployment.yaml",
		"coopmux.enabled=true", "coopmux.natsRelay.enabled=true")
	w := envWindow(t, out, "name: COOP_MUX_NATS_USER")
	if !strings.Contains(w, "value: infra") {
		t.Errorf("coopmux must authenticate as the infra user, got:\n%s", w)
	}
	w = envWindow(t, out, "name: COOP_MUX_NATS_RELAY_USER")
	if !strings.Contains(w, "value: infra") {
		t.Errorf("coopmux relay must authenticate as the infra user, got:\n%s", w)
	}
}

func TestNatsUserCoopmuxAbsentWithoutIsolation(t *testing.T) {
	t.Parallel()
	out := helmTemplate(t, "templates/coopmux/deployment.yaml",
		"nats.enabled=true", "coopmux.enabled=true")
	if strings.Contains(out, "COOP_MUX_NATS_USER") {
		t.Error("COOP_MUX_NATS_USER must not be set when subjectIsolation is disabled " +
			"(the historical bearer token is not a NATS user password)")
	}
}

func TestNatsUserSlackBridge(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/slack-bridge/deployment.yaml",
		"slackBridge.enabled=true")
	w := envWindow(t, out, "name: COOP_NATS_USER")
	if !strings.Contains(w, "value: infra") {
		t.Errorf("slack-bridge must authenticate as the infra user, got:\n%s", w)
	}
}

func TestNatsUserBeadsViewer(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/beads-viewer/deployment.yaml",
		"beadsViewer.enabled=true")
	w := envWindow(t, out, "name: COOP_NATS_USER")
	if !strings.Contains(w, "value: infra") {
		t.Errorf("beads-viewer must authenticate as the infra user, got:\n%s", w)
	}
}

func TestNatsUserBackupCronjob(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/backup/nats-cronjob.yaml",
		"backup.nats.enabled=true")
	w := envWindow(t, out, "name: NATS_USER")
	if !strings.Contains(w, "value: infra") {
		t.Errorf("backup cronjob must authenticate as the infra user, got:\n%s", w)
	}
	// The script must translate user+secret into --user/--password flags for
	// the nats CLI, never --token (which cannot match a users[] entry).
	if !strings.Contains(out, "--user ${NATS_USER} --password ${NATS_TOKEN}") {
		t.Error("backup script must pass --user/--password to the nats CLI under subject isolation")
	}
}

// TestNatsUserStreamInit: the controller's nats-stream-init init container
// runs the nats CLI (stream admin — infra-class). It historically ran with NO
// credentials at all, which wedges the controller rollout the moment server
// auth turns on. The nats CLI reads NATS_USER/NATS_PASSWORD from env.
func TestNatsUserStreamInit(t *testing.T) {
	t.Parallel()
	out := renderWithIsolation(t, "templates/controller/deployment.yaml", "agents.enabled=true")
	idx := strings.Index(out, "nats-stream-init")
	if idx < 0 {
		t.Fatal("controller must render the nats-stream-init init container")
	}
	initSection := out[idx:]
	if end := strings.Index(initSection, "containers:"); end > 0 {
		initSection = initSection[:end]
	}
	if !strings.Contains(initSection, "NATS_USER") {
		t.Error("nats-stream-init must receive NATS_USER under subject isolation")
	}
	if !strings.Contains(initSection, "NATS_PASSWORD") {
		t.Error("nats-stream-init must receive NATS_PASSWORD under subject isolation")
	}
	if !strings.Contains(initSection, "test-nats-infra-creds") {
		t.Error("nats-stream-init credentials must come from the -nats-infra-creds Secret")
	}
}
