package bridge

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestNotifyNewEscalation_StampsOperatorVisibility asserts that posting an
// escalation card stamps the operator-visibility fields (slack_channel,
// slack_ts, notified_at) onto the escalation bead so bearer-token agents can
// read "was the operator notified, where, when" via kd show / gb escalation
// show (ka-0000003667). The Slack ref is otherwise trapped in the in-memory
// message map and the OIDC-only web API.
func TestNotifyNewEscalation_StampsOperatorVisibility(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["gd-esc1"] = &beadsapi.BeadDetail{
		ID:     "gd-esc1",
		Title:  "Rotate prod DB credentials",
		Type:   "escalation",
		Fields: map[string]string{"operation_type": "credential_rotation"},
	}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	bead := BeadEvent{
		ID:     "gd-esc1",
		Type:   "escalation",
		Title:  "Rotate prod DB credentials",
		Fields: map[string]string{"operation_type": "credential_rotation"},
	}
	if err := bot.NotifyNewEscalation(context.Background(), bead); err != nil {
		t.Fatalf("NotifyNewEscalation: %v", err)
	}

	got := daemon.beads["gd-esc1"].Fields
	// The fake Slack server returns message_ts "1234.5678", so ts is non-empty
	// and the stamp fires. slack_ts being set proves the non-sensitive branch
	// ran (it also writes slack_channel from the Slack response — empty here
	// because the fake server returns no channel, populated in production).
	if got["slack_ts"] != "1234.5678" {
		t.Errorf("slack_ts = %q, want %q", got["slack_ts"], "1234.5678")
	}
	if _, ok := got["slack_channel"]; !ok {
		t.Error("slack_channel key was not stamped onto the escalation bead")
	}
	if got["notified_at"] == "" {
		t.Error("notified_at was not stamped onto the escalation bead")
	}
}

// TestNotifyNewEscalation_SensitiveOmitsChannel asserts that for
// sensitive/secret escalations only notified_at is stamped — the raw
// slack_channel/slack_ts are omitted so a private escalation's operator
// channel is not exposed to any bearer-token agent that can read the bead.
func TestNotifyNewEscalation_SensitiveOmitsChannel(t *testing.T) {
	daemon := newMockDaemon()
	daemon.beads["gd-esc2"] = &beadsapi.BeadDetail{
		ID:     "gd-esc2",
		Title:  "Provision Vault secret",
		Type:   "escalation",
		Fields: map[string]string{"operation_type": "vault_provisioning", "sensitivity": "secret"},
	}
	slackSrv := newFakeSlackServer(t)
	defer slackSrv.Close()
	bot := newTestBot(daemon, slackSrv)

	bead := BeadEvent{
		ID:     "gd-esc2",
		Type:   "escalation",
		Title:  "Provision Vault secret",
		Fields: map[string]string{"operation_type": "vault_provisioning", "sensitivity": "secret"},
	}
	if err := bot.NotifyNewEscalation(context.Background(), bead); err != nil {
		t.Fatalf("NotifyNewEscalation: %v", err)
	}

	got := daemon.beads["gd-esc2"].Fields
	if got["notified_at"] == "" {
		t.Error("notified_at should be stamped even for secret escalations")
	}
	if got["slack_channel"] != "" {
		t.Errorf("slack_channel should be omitted for secret escalations, got %q", got["slack_channel"])
	}
	if got["slack_ts"] != "" {
		t.Errorf("slack_ts should be omitted for secret escalations, got %q", got["slack_ts"])
	}
}
