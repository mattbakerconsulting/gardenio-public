package bridge

import (
	"context"
	"log/slog"
	"testing"
)

// TestUserDirectory_SSEUpsertMakesUserImmediatelyVisible exercises the fix
// for ka-0000001175: a freshly-created user bead must be resolvable via
// LookupByEmail immediately, without waiting for the 5-minute periodic
// Refresh. The escalation approve flow depends on this — otherwise the
// e2e test's "create user, sleep 5s, approve" path returns 403.
func TestUserDirectory_SSEUpsertMakesUserImmediatelyVisible(t *testing.T) {
	daemon := newMockDaemon()
	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()

	if err := dir.Refresh(ctx); err != nil {
		t.Fatalf("Refresh failed: %v", err)
	}
	if got := dir.LookupByEmail(ctx, "new@example.com"); got != nil {
		t.Fatal("expected LookupByEmail to miss before SSE event")
	}

	evt := []byte(`{"bead":{
		"id":"gd-NEW",
		"type":"user",
		"title":"New User",
		"status":"open",
		"fields":{
			"email":"new@example.com",
			"slack_id":"U-NEW",
			"display_name":"New User",
			"capabilities":"[\"approve:escalation\"]"
		}
	}}`)
	dir.handleSSEUpsert(ctx, evt)

	got := dir.LookupByEmail(ctx, "new@example.com")
	if got == nil {
		t.Fatal("expected LookupByEmail to resolve after SSE create event")
	}
	if got.BeadID != "gd-NEW" {
		t.Errorf("bead_id: got %q, want gd-NEW", got.BeadID)
	}
	if got.SlackID != "U-NEW" {
		t.Errorf("slack_id: got %q, want U-NEW", got.SlackID)
	}
	if !got.HasCapability("approve:escalation") {
		t.Errorf("expected approve:escalation capability, got %v", got.Capabilities)
	}

	if dir.LookupBySlackID(ctx, "U-NEW") == nil {
		t.Error("expected LookupBySlackID to resolve after SSE create event")
	}
	if dir.LookupByBeadID(ctx, "gd-NEW") == nil {
		t.Error("expected LookupByBeadID to resolve after SSE create event")
	}
}

// TestUserDirectory_SSEUpsertReplacesExistingEntry verifies that an update
// event for an existing user rewrites the lookup maps so stale email/slack
// values don't linger.
func TestUserDirectory_SSEUpsertReplacesExistingEntry(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Alice", map[string]string{
		"email":    "alice@old.example.com",
		"slack_id": "U-ALICE",
	})
	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()
	if err := dir.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	evt := []byte(`{"bead":{
		"id":"user-Alice",
		"type":"user",
		"title":"Alice",
		"status":"open",
		"fields":{
			"email":"alice@new.example.com",
			"slack_id":"U-ALICE"
		}
	}}`)
	dir.handleSSEUpsert(ctx, evt)

	if got := dir.LookupByEmail(ctx, "alice@new.example.com"); got == nil {
		t.Error("expected new email to resolve")
	}
	if got := dir.LookupByEmail(ctx, "alice@old.example.com"); got != nil {
		t.Error("expected old email to be removed from index")
	}
}

// TestUserDirectory_SSECloseRemovesUser verifies a closed user bead is
// dropped from the directory.
func TestUserDirectory_SSECloseRemovesUser(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedUser("Bob", map[string]string{
		"email":    "bob@example.com",
		"slack_id": "U-BOB",
	})
	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()
	if err := dir.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if dir.LookupByEmail(ctx, "bob@example.com") == nil {
		t.Fatal("precondition: Bob should be in the directory")
	}

	evt := []byte(`{"bead":{"id":"user-Bob","type":"user","title":"Bob","status":"closed","fields":{"email":"bob@example.com","slack_id":"U-BOB"}}}`)
	dir.handleSSEClosed(ctx, evt)

	if got := dir.LookupByEmail(ctx, "bob@example.com"); got != nil {
		t.Error("expected Bob to be removed after close event")
	}
	if got := dir.LookupBySlackID(ctx, "U-BOB"); got != nil {
		t.Error("expected Bob's Slack ID to be removed after close event")
	}
}

// TestUserDirectory_SSEIgnoresNonUserBeads guards against the directory
// reacting to unrelated bead lifecycle events.
func TestUserDirectory_SSEIgnoresNonUserBeads(t *testing.T) {
	daemon := newMockDaemon()
	dir := NewUserDirectory(daemon, slog.Default())
	ctx := context.Background()
	if err := dir.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	evt := []byte(`{"bead":{"id":"gd-X","type":"task","title":"Some task","status":"open","fields":{"email":"x@example.com"}}}`)
	dir.handleSSEUpsert(ctx, evt)

	if got := dir.LookupByEmail(ctx, "x@example.com"); got != nil {
		t.Error("non-user bead should not be indexed by email")
	}
	if dir.Count() != 0 {
		t.Errorf("expected empty directory, got count=%d", dir.Count())
	}
}
