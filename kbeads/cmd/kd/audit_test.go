package main

import (
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

func TestAuditBead_TableOutput(t *testing.T) {
	mc := newMockClient()
	mc.GetMutationsResult = []*model.MutationRow{
		{ID: 1, BeadID: "gd-x", Actor: "alice", Field: "*", OldValue: "", NewValue: "<created>", MutationType: "create", CreatedAt: time.Date(2026, 4, 27, 10, 0, 0, 0, time.UTC)},
		{ID: 2, BeadID: "gd-x", Actor: "bob", Field: "title", OldValue: "Old", NewValue: "New", MutationType: "update", CreatedAt: time.Date(2026, 4, 27, 11, 0, 0, 0, time.UTC)},
	}

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	out := captureStdout(t, func() {
		if err := auditBeadCmd.RunE(auditBeadCmd, []string{"gd-x"}); err != nil {
			t.Fatalf("auditBeadCmd: %v", err)
		}
	})
	for _, want := range []string{"alice", "create", "bob", "update", "title", "Old", "New"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}

func TestAuditBead_Empty(t *testing.T) {
	mc := newMockClient()
	mc.GetMutationsResult = nil

	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	out := captureStdout(t, func() {
		if err := auditBeadCmd.RunE(auditBeadCmd, []string{"gd-empty"}); err != nil {
			t.Fatalf("auditBeadCmd: %v", err)
		}
	})
	if !strings.Contains(out, "No mutations recorded.") {
		t.Errorf("expected empty notice, got:\n%s", out)
	}
}

func TestAuditBead_RoundTrip(t *testing.T) {
	mc := newMockClient()
	// Simulate a sequence of mutations stored by the server.
	mc.GetMutationsResult = []*model.MutationRow{
		{BeadID: "gd-rt", Actor: "alice", Field: "*", NewValue: "<created>", MutationType: "create"},
		{BeadID: "gd-rt", Actor: "alice", Field: "title", OldValue: "A", NewValue: "B", MutationType: "update"},
		{BeadID: "gd-rt", Actor: "alice", Field: "*", OldValue: "<existed>", MutationType: "delete"},
	}
	origClient := beadsClient
	beadsClient = mc
	defer func() { beadsClient = origClient }()

	out := captureStdout(t, func() {
		if err := auditBeadCmd.RunE(auditBeadCmd, []string{"gd-rt"}); err != nil {
			t.Fatalf("auditBeadCmd: %v", err)
		}
	})
	// All three mutation_type strings must appear in the chronological listing.
	for _, want := range []string{"create", "update", "delete"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected output to contain %q, got:\n%s", want, out)
		}
	}
}
