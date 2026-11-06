package main

import (
	"context"
	"strings"
	"testing"
)

// sealedGardenerStub is a timed gardener trigger gd-CAP1 for epic gd-EPIC1
// carrying a seal over its current content, behind a stub that reports the
// daemon's spawn digest and answers /v1/whoami with source.
func sealedGardenerStub(t *testing.T, source string) *gardenerStub {
	t.Helper()
	stub := newGardenerStub()
	stub.reportSpawnDigest = true
	stub.whoamiSource = source
	stub.addBead(makeEpic("gd-EPIC1", "Herders"))
	trig := makeGardenerTrigger("gd-CAP1", "gd-EPIC1", map[string]any{"cron": "7,37 * * * *"})
	trig["fields"].(map[string]any)["spawn_lineage"] = stub.stubSeal(trig)
	stub.addBead(trig)
	setupGardenerStub(t, stub)
	resetGardenerFlags(t)
	t.Cleanup(func() { resetGardenerFlags(t) })
	oldOperated := operatedBy
	operatedBy = "slack:U0OPERATOR1"
	t.Cleanup(func() { operatedBy = oldOperated })
	return stub
}

// TestGardenerCadence_SealedRefusesAgentToken is the gd-c4Xz4i95F7 regression:
// a plan gardener run (an agent token) changed the cron of its owner-sealed
// trigger, the seal went stale and every later fire 403'd until hard_stop. The
// command must refuse before writing and say how to escalate an edit-and-reseal.
func TestGardenerCadence_SealedRefusesAgentToken(t *testing.T) {
	stub := sealedGardenerStub(t, "token")
	if err := gardenerCadenceCmd.Flags().Set("cron", "25 */2 * * *"); err != nil {
		t.Fatal(err)
	}
	cmd := gardenerCadenceCmd
	cmd.SetContext(context.Background())
	cmd.SetOut(&strings.Builder{})

	err := cmd.RunE(cmd, []string{"gd-EPIC1"})
	if err == nil {
		t.Fatal("RunE succeeded on a sealed trigger from an agent token; want a refusal")
	}
	for _, want := range []string{"sealed to oidc:owner@example.com", "HTTP 403", "gb escalation create", `gb gardener cadence gd-CAP1 --cron "25 */2 * * *"`, "gb trigger reseal gd-CAP1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal missing %q:\n%s", want, err)
		}
	}
	if ops := stub.opsOf("gd-CAP1"); len(ops) != 0 {
		t.Errorf("refused edit still wrote %v", ops)
	}
}

// TestGardenerCadence_SealedRefusesWhenWhoamiFails fails closed: a caller the
// daemon cannot identify is not assumed to be an admin.
func TestGardenerCadence_SealedRefusesWhenWhoamiFails(t *testing.T) {
	stub := sealedGardenerStub(t, "")
	if err := gardenerCadenceCmd.Flags().Set("cron", "25 */2 * * *"); err != nil {
		t.Fatal(err)
	}
	cmd := gardenerCadenceCmd
	cmd.SetContext(context.Background())
	cmd.SetOut(&strings.Builder{})

	if err := cmd.RunE(cmd, []string{"gd-CAP1"}); err == nil || !strings.Contains(err.Error(), "could not tell who") {
		t.Fatalf("RunE err = %v, want a fail-closed refusal", err)
	}
	if ops := stub.opsOf("gd-CAP1"); len(ops) != 0 {
		t.Errorf("refused edit still wrote %v", ops)
	}
}

// TestGardenerCadence_StaleSealIsNotGuarded: a seal that no longer covers the
// content is already void, so the edit cannot break it and goes through
// (gd-c4Xz4i95F7).
func TestGardenerCadence_StaleSealIsNotGuarded(t *testing.T) {
	stub := sealedGardenerStub(t, "token")
	stub.beads["gd-CAP1"]["fields"].(map[string]any)["prompt"] = "rewritten after the seal"
	if err := gardenerCadenceCmd.Flags().Set("cron", "25 */2 * * *"); err != nil {
		t.Fatal(err)
	}
	cmd := gardenerCadenceCmd
	cmd.SetContext(context.Background())
	cmd.SetOut(&strings.Builder{})

	if err := cmd.RunE(cmd, []string{"gd-CAP1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if got := lastFieldsPatch(t, stub, "gd-CAP1")["cron"]; got != "25 */2 * * *" {
		t.Errorf("cron patch = %v, want the new cadence", got)
	}
}

// TestGardenerCadence_SealedAdminKeepsSeal: an OIDC admin's write is let
// through, and the daemon's re-stamp is read back as a kept seal.
func TestGardenerCadence_SealedAdminKeepsSeal(t *testing.T) {
	stub := sealedGardenerStub(t, "auth")
	stub.restampSealOnPatch = true
	if err := gardenerCadenceCmd.Flags().Set("cron", "25 */2 * * *"); err != nil {
		t.Fatal(err)
	}
	cmd := gardenerCadenceCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-CAP1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if !strings.Contains(buf.String(), "cadence set to 25 */2 * * *") {
		t.Errorf("output = %q, want the set confirmation", buf.String())
	}
}

// TestGardenerCadence_SealedNonAdminOIDCReportsLostSeal: an OIDC session that
// is not a gardenio-admin gets past the guard, but the daemon does not re-stamp,
// so the command must say the trigger is now unsealed and how to fix it.
func TestGardenerCadence_SealedNonAdminOIDCReportsLostSeal(t *testing.T) {
	stub := sealedGardenerStub(t, "auth")
	if err := gardenerCadenceCmd.Flags().Set("cron", "25 */2 * * *"); err != nil {
		t.Fatal(err)
	}
	cmd := gardenerCadenceCmd
	cmd.SetContext(context.Background())
	cmd.SetOut(&strings.Builder{})

	err := cmd.RunE(cmd, []string{"gd-CAP1"})
	if err == nil || !strings.Contains(err.Error(), "no longer sealed") || !strings.Contains(err.Error(), "gb trigger reseal gd-CAP1") {
		t.Fatalf("RunE err = %v, want a lost-seal report naming the reseal", err)
	}
	if got := lastFieldsPatch(t, stub, "gd-CAP1")["cron"]; got != "25 */2 * * *" {
		t.Errorf("cron patch = %v, want the write to have landed", got)
	}
}

// TestGardenerMode_SealedRefusesAgentFlip: a mode flip rewrites cron and
// triggers too, so it is guarded the same way.
func TestGardenerMode_SealedRefusesAgentFlip(t *testing.T) {
	stub := sealedGardenerStub(t, "token")
	if err := gardenerModeCmd.Flags().Set("event", "true"); err != nil {
		t.Fatal(err)
	}
	cmd := gardenerModeCmd
	cmd.SetContext(context.Background())
	cmd.SetOut(&strings.Builder{})

	err := cmd.RunE(cmd, []string{"gd-CAP1"})
	if err == nil || !strings.Contains(err.Error(), "gb gardener mode gd-CAP1 --event") {
		t.Fatalf("RunE err = %v, want a refusal naming the flip", err)
	}
	if ops := stub.opsOf("gd-CAP1"); len(ops) != 0 {
		t.Errorf("refused flip still wrote %v", ops)
	}
}

// TestGardenerMode_SealedNoOpFlipIsNotGuarded: asking for the mode the gardener
// is already in writes nothing, so it is not refused (gd-c4Xz4i95F7).
func TestGardenerMode_SealedNoOpFlipIsNotGuarded(t *testing.T) {
	sealedGardenerStub(t, "token")
	if err := gardenerModeCmd.Flags().Set("timed", "true"); err != nil {
		t.Fatal(err)
	}
	cmd := gardenerModeCmd
	cmd.SetContext(context.Background())
	var buf strings.Builder
	cmd.SetOut(&buf)

	if err := cmd.RunE(cmd, []string{"gd-CAP1"}); err != nil {
		t.Fatalf("RunE: %v", err)
	}
	if !strings.Contains(buf.String(), "already in timed mode") {
		t.Errorf("output = %q, want the no-op report", buf.String())
	}
}
