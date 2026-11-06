package server

import (
	"context"
	"testing"

	"gardenio/kbeads/internal/model"
)

// ctxAsTestSubject returns context.Background() wrapped with an OIDC AuthIdentity
// matching defaultTestSubject. Tests that drive s.createBead directly need this
// so the post-R2 fail-closed operator check in beads.go does not 400 the call.
func ctxAsTestSubject() context.Context {
	return WithAuthIdentity(context.Background(), AuthIdentity{
		Type: "oidc", Subject: defaultTestSubject,
	})
}

// TestCreateBead_DefaultStatusOpen pins existing behavior: omitting Status
// in the request defaults to "open" and leaves ClosedAt nil.
func TestCreateBead_DefaultStatusOpen(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title: "default open", Type: "task",
	})
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}
	stored := ms.beads[bead.ID]
	if stored == nil {
		t.Fatalf("bead %s not persisted", bead.ID)
	}
	if stored.Status != model.StatusOpen {
		t.Errorf("Status = %q; want %q", stored.Status, model.StatusOpen)
	}
	if stored.ClosedAt != nil {
		t.Errorf("ClosedAt = %v; want nil", stored.ClosedAt)
	}
}

// TestCreateBead_StatusClosed_StampsClosedAt covers the audit-event ledger
// path from ka-0000004100. Append-only records (type=event, label=
// auto:trigger-audit) are born closed so they never surface in ready/news
// queries and don't inflate the open-bead count. The data invariant
// (status=closed ⇔ closed_at IS NOT NULL) must hold on create.
func TestCreateBead_StatusClosed_StampsClosedAt(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "audit",
		Type:   "event",
		Status: "closed",
		Labels: []string{"auto:trigger-audit"},
	})
	if err != nil {
		t.Fatalf("createBead error: %v", err)
	}
	stored := ms.beads[bead.ID]
	if stored == nil {
		t.Fatalf("bead %s not persisted", bead.ID)
	}
	if stored.Status != model.StatusClosed {
		t.Errorf("Status = %q; want %q", stored.Status, model.StatusClosed)
	}
	if stored.ClosedAt == nil {
		t.Error("ClosedAt is nil; want stamped to CreatedAt")
	} else if !stored.ClosedAt.Equal(stored.CreatedAt) {
		t.Errorf("ClosedAt (%v) should equal CreatedAt (%v) for born-closed bead",
			stored.ClosedAt, stored.CreatedAt)
	}
}

// TestCreateBead_StatusPartial covers the first-class 'partial' status
// (ka-0000007305 §2.3). 'partial' is terminal-for-progression but NOT a clean
// completion, so the write path must accept it AND leave ClosedAt nil (only
// 'closed' stamps closed_at).
func TestCreateBead_StatusPartial(t *testing.T) {
	s, ms, _ := newTestServer()
	bead, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "fan-out step, some failures",
		Type:   "task",
		Status: "partial",
	})
	if err != nil {
		t.Fatalf("createBead with status=partial error: %v", err)
	}
	stored := ms.beads[bead.ID]
	if stored == nil {
		t.Fatalf("bead %s not persisted", bead.ID)
	}
	if stored.Status != model.StatusPartial {
		t.Errorf("Status = %q; want %q", stored.Status, model.StatusPartial)
	}
	if stored.ClosedAt != nil {
		t.Errorf("ClosedAt = %v; want nil — partial must not stamp closed_at", stored.ClosedAt)
	}
}

// TestCreateBead_InvalidStatus rejects garbage status values up-front so
// callers can't smuggle in junk via JSON.
func TestCreateBead_InvalidStatus(t *testing.T) {
	s, _, _ := newTestServer()
	_, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title: "bogus", Type: "task", Status: "bogus",
	})
	if err == nil {
		t.Fatal("expected inputError for invalid status, got nil")
	}
}
