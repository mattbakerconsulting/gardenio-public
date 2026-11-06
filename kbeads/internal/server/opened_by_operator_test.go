package server

import (
	"context"
	"encoding/json"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// seedOpenedThread seeds a wish head the way the slack bridge mints one
// (getOrCreateThreadBead): the thread label, the opener's Slack id, and
// operated_by resolved from it.
func seedOpenedThread(t *testing.T, ms *mockStore, wishID, operatedBy string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"slack_opener_user_id": "U0OPENER", "operated_by": operatedBy})
	ms.beads[wishID] = &model.Bead{
		ID: wishID, Kind: model.KindIssue, Type: "task",
		Title: "wish head " + wishID, Status: model.StatusOpen, Fields: raw,
	}
	ms.labels[wishID] = []string{threadHeadLabel}
}

func openedByOf(t *testing.T, ms *mockStore, beadID string) string {
	t.Helper()
	b := ms.beads[beadID]
	if b == nil {
		t.Fatalf("bead %s not found in store", beadID)
	}
	v, _, _ := exactFieldString(b.Fields, openedByOperatorField)
	return v
}

// A thread agent's kd create inherits thread_id, and the task gets the head's
// resolved opener as opened_by_operator.
func TestCreateBead_OpenedByOperator_StampedFromThreadHead(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const agentName = "thread-1780289972-opener"
	const wishID = "gd-wish-OPEN"
	seedWishAgentBead(t, ms, agentName, wishID)
	seedOpenedThread(t, ms, wishID, "oidc:alice@example.com")

	id := createBeadAsAgent(t, h, agentName, map[string]any{"title": "derivative", "type": "task"}, nil)
	if got := openedByOf(t, ms, id); got != "oidc:alice@example.com" {
		t.Fatalf("opened_by_operator = %q, want oidc:alice@example.com", got)
	}
	if got := threadIDOf(t, ms, id); got != wishID {
		t.Fatalf("thread_id = %q, want %q", got, wishID)
	}
}

// An opener the bridge could not resolve to an email (slack:U… sentinel) is
// not an operator a runner can be owned by: leave the field unset.
func TestCreateBead_OpenedByOperator_UnresolvedOpenerSkipped(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const agentName = "thread-1780289972-unresolved"
	const wishID = "gd-wish-UNRS"
	seedWishAgentBead(t, ms, agentName, wishID)
	seedOpenedThread(t, ms, wishID, "slack:U0OPENER")

	id := createBeadAsAgent(t, h, agentName, map[string]any{"title": "derivative", "type": "task"}, nil)
	if got := openedByOf(t, ms, id); got != "" {
		t.Fatalf("opened_by_operator = %q, want unset", got)
	}
}

// A thread_id that points at a bead which is not a wish head borrows nothing.
func TestCreateBead_OpenedByOperator_NonHeadSkipped(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})

	seedOpenedThread(t, ms, "gd-not-a-head", "oidc:alice@example.com")
	ms.labels["gd-not-a-head"] = nil

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "bob@example.com"})
	bead, err := s.createBead(ctx, createBeadInput{
		Title: "points at a non-head", Type: "task",
		Fields: json.RawMessage(`{"thread_id":"gd-not-a-head"}`),
	})
	if err != nil {
		t.Fatalf("createBead: %v", err)
	}
	if got := openedByOf(t, ms, bead.ID); got != "" {
		t.Fatalf("opened_by_operator = %q, want unset", got)
	}
}

// The stamp keys off thread_id, not the caller: a bridge or composer that sets
// thread_id itself gets it too. A caller-supplied value is kept.
func TestCreateBead_OpenedByOperator_ExplicitThreadAndValue(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	const wishID = "gd-wish-EXPL"
	seedOpenedThread(t, ms, wishID, "oidc:alice@example.com")

	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: "alice@example.com"})
	bead, err := s.createBead(ctx, createBeadInput{
		Title: "composer derivative", Type: "task",
		Fields: json.RawMessage(`{"thread_id":"` + wishID + `"}`),
	})
	if err != nil {
		t.Fatalf("createBead: %v", err)
	}
	if got := openedByOf(t, ms, bead.ID); got != "oidc:alice@example.com" {
		t.Fatalf("opened_by_operator = %q, want oidc:alice@example.com", got)
	}

	kept, err := s.createBead(ctx, createBeadInput{
		Title: "already says", Type: "task",
		Fields: json.RawMessage(`{"thread_id":"` + wishID + `","opened_by_operator":"oidc:carol@example.com"}`),
	})
	if err != nil {
		t.Fatalf("createBead: %v", err)
	}
	if got := openedByOf(t, ms, kept.ID); got != "oidc:carol@example.com" {
		t.Fatalf("opened_by_operator = %q, want the caller's oidc:carol@example.com", got)
	}
}

// Only the leaf-work types that declare the field get it, even when the
// thread_id copy lands on another type.
func TestCreateBead_OpenedByOperator_UndeclaredTypeSkipped(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &events.NoopPublisher{})
	h := xactorAgentHandler(s, "primary-token", nil)

	const agentName = "thread-1780289972-epic"
	const wishID = "gd-wish-EPIC"
	seedWishAgentBead(t, ms, agentName, wishID)
	seedOpenedThread(t, ms, wishID, "oidc:alice@example.com")

	id := createBeadAsAgent(t, h, agentName, map[string]any{"title": "an epic", "type": "epic"}, nil)
	if got := threadIDOf(t, ms, id); got != wishID {
		t.Fatalf("precondition: thread_id = %q, want %q", got, wishID)
	}
	if got := openedByOf(t, ms, id); got != "" {
		t.Fatalf("opened_by_operator = %q on an epic, want unset", got)
	}
}
