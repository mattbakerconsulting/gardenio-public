package main

import (
	"context"
	"testing"
	"time"
)

// ── escalation pingback tests (ka-0000004385) ────────────────────────────
//
// Pingback stamps the requested_at/count/by/source handshake fields the
// bridge's Escalations watcher keys off, plus an audit comment. Reuses the
// reflagDaemonStub from the reflag suites.

// resetEscalationPingbackFlags resets the pingback command's flag state
// across tests; cobra commands are global singletons in this package.
func resetEscalationPingbackFlags(t *testing.T) {
	t.Helper()
	escalationPingbackCmd.ResetFlags()
	escalationPingbackCmd.Flags().String("reason", "", "reason for the pingback (recorded on the bead and shown in the Slack re-post)")
	escalationPingbackCmd.Flags().Bool("force", false, "override the 1h pingback rate limit")
}

// pingbackFieldsFromPatches extracts the merged fields map from the stub's
// recorded PATCH bodies.
func pingbackFieldsFromPatches(patches []map[string]any) map[string]any {
	merged := map[string]any{}
	for _, p := range patches {
		if fields, ok := p["fields"].(map[string]any); ok {
			for k, v := range fields {
				merged[k] = v
			}
		}
	}
	return merged
}

func TestEscalationPingback_StampsHandshakeFields(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagEscalation("gd-p1", 2, nil, nil),
	}
	setupReflagDaemon(t, stub)
	resetEscalationPingbackFlags(t)

	cmd := escalationPingbackCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-p1"}); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}

	fields := pingbackFieldsFromPatches(stub.patches)
	if fields["pingback_requested_at"] == nil || fields["pingback_requested_at"] == "" {
		t.Error("pingback_requested_at was not stamped")
	}
	switch v := fields["pingback_count"].(type) {
	case string:
		if v != "1" {
			t.Errorf("pingback_count = %q, want \"1\"", v)
		}
	case float64:
		if v != 1 {
			t.Errorf("pingback_count = %v, want 1", v)
		}
	default:
		t.Errorf("pingback_count missing or wrong type: %#v", fields["pingback_count"])
	}
	if fields["pingback_source"] != "manual" {
		t.Errorf("pingback_source = %v, want manual", fields["pingback_source"])
	}
	if len(stub.comments) != 1 {
		t.Errorf("got %d audit comments, want 1", len(stub.comments))
	}
	// Pingback must never touch priority.
	for _, p := range stub.patches {
		if _, ok := p["priority"]; ok {
			t.Errorf("pingback sent a priority patch: %v", p["priority"])
		}
	}
}

func TestEscalationPingback_RateLimited(t *testing.T) {
	recent := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	stub := &reflagDaemonStub{
		bead: makeReflagEscalation("gd-p2", 2, map[string]any{
			"pingback_requested_at": recent,
			"pingback_count":        "3",
		}, nil),
	}
	setupReflagDaemon(t, stub)
	resetEscalationPingbackFlags(t)

	cmd := escalationPingbackCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(cmd, []string{"gd-p2"})
	if err == nil {
		t.Fatal("expected rate-limit error for pingback 10m after the last; got nil")
	}
	if len(stub.patches) != 0 {
		t.Errorf("rate-limited pingback must not patch the bead; got %#v", stub.patches)
	}
}

func TestEscalationPingback_ForceOverridesRateLimit(t *testing.T) {
	recent := time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339)
	stub := &reflagDaemonStub{
		bead: makeReflagEscalation("gd-p3", 2, map[string]any{
			"pingback_requested_at": recent,
			"pingback_count":        "3",
		}, nil),
	}
	setupReflagDaemon(t, stub)
	resetEscalationPingbackFlags(t)

	cmd := escalationPingbackCmd
	cmd.SetContext(context.Background())
	if err := cmd.Flags().Set("force", "true"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.RunE(cmd, []string{"gd-p3"}); err != nil {
		t.Fatalf("RunE with --force returned error: %v", err)
	}

	fields := pingbackFieldsFromPatches(stub.patches)
	if fields["pingback_count"] != "4" && fields["pingback_count"] != float64(4) {
		t.Errorf("pingback_count = %v, want 4 (3 + 1)", fields["pingback_count"])
	}
}

func TestEscalationPingback_AllowedAfterInterval(t *testing.T) {
	old := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)
	stub := &reflagDaemonStub{
		bead: makeReflagEscalation("gd-p4", 2, map[string]any{
			"pingback_requested_at": old,
			"pingback_count":        "1",
		}, nil),
	}
	setupReflagDaemon(t, stub)
	resetEscalationPingbackFlags(t)

	cmd := escalationPingbackCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-p4"}); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}
	fields := pingbackFieldsFromPatches(stub.patches)
	if fields["pingback_count"] != "2" && fields["pingback_count"] != float64(2) {
		t.Errorf("pingback_count = %v, want 2", fields["pingback_count"])
	}
}

func TestEscalationPingback_RejectsClosedEscalation(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagEscalation("gd-p5", 2, nil, nil),
	}
	stub.bead["status"] = "closed"
	setupReflagDaemon(t, stub)
	resetEscalationPingbackFlags(t)

	cmd := escalationPingbackCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-p5"}); err == nil {
		t.Fatal("expected error pinging closed escalation; got nil")
	}
}

func TestEscalationPingback_RejectsNonEscalationBead(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagBead("gd-p6", 2, nil, nil),
	}
	setupReflagDaemon(t, stub)
	resetEscalationPingbackFlags(t)

	cmd := escalationPingbackCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-p6"}); err == nil {
		t.Fatal("expected error pinging non-escalation bead; got nil")
	}
	if len(stub.patches) != 0 {
		t.Errorf("no patches expected on type rejection; got %#v", stub.patches)
	}
}

// TestEscalationReflag_StampsPingbackRequest locks the runReflag change:
// an escalation reflag doubles as a re-surface request, so the bridge's
// pingback path re-posts the card (the "escalated" label alone only
// triggers a repost for decision beads).
func TestEscalationReflag_StampsPingbackRequest(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagEscalation("gd-p7", 3, map[string]any{"pingback_count": "2"}, nil),
	}
	setupReflagDaemon(t, stub)
	resetEscalationReflagFlags(t)

	cmd := escalationReflagCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-p7"}); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}

	fields := pingbackFieldsFromPatches(stub.patches)
	if fields["pingback_requested_at"] == nil || fields["pingback_requested_at"] == "" {
		t.Error("escalation reflag did not stamp pingback_requested_at")
	}
	if fields["pingback_source"] != "reflag" {
		t.Errorf("pingback_source = %v, want reflag", fields["pingback_source"])
	}
	if fields["pingback_count"] != "3" && fields["pingback_count"] != float64(3) {
		t.Errorf("pingback_count = %v, want 3 (2 + 1)", fields["pingback_count"])
	}
}

// TestDecisionReflag_DoesNotStampPingback locks the type gate on the
// runReflag pingback stamp: decision reflags must NOT grow pingback fields.
func TestDecisionReflag_DoesNotStampPingback(t *testing.T) {
	stub := &reflagDaemonStub{
		bead: makeReflagBead("gd-p8", 3, nil, nil),
	}
	setupReflagDaemon(t, stub)
	resetReflagFlags(t)

	cmd := decisionReflagCmd
	cmd.SetContext(context.Background())
	if err := cmd.RunE(cmd, []string{"gd-p8"}); err != nil {
		t.Fatalf("RunE returned error: %v", err)
	}
	fields := pingbackFieldsFromPatches(stub.patches)
	if _, ok := fields["pingback_requested_at"]; ok {
		t.Error("decision reflag must not stamp pingback_requested_at")
	}
}
