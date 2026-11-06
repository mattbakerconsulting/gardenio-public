package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// --- AddDependency ---

func TestHTTPClient_AddDependency(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"bead_id": "bead-a",
			"depends_on_id": "bead-b",
			"type": "blocks",
			"created_by": "alice",
			"created_at": "2026-01-15T10:00:00Z"
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	dep, err := c.AddDependency(context.Background(), &AddDependencyRequest{
		BeadID:      "bead-a",
		DependsOnID: "bead-b",
		Type:        "blocks",
		CreatedBy:   "alice",
	})
	if err != nil {
		t.Fatalf("AddDependency() error = %v", err)
	}

	if h.method != http.MethodPost {
		t.Errorf("method = %q, want POST", h.method)
	}
	if h.path != "/v1/beads/bead-a/dependencies" {
		t.Errorf("path = %q, want /v1/beads/bead-a/dependencies", h.path)
	}

	var reqBody map[string]interface{}
	if err := json.Unmarshal([]byte(h.body), &reqBody); err != nil {
		t.Fatalf("unmarshaling request body: %v", err)
	}
	if reqBody["depends_on_id"] != "bead-b" {
		t.Errorf("request body depends_on_id = %v, want 'bead-b'", reqBody["depends_on_id"])
	}
	if reqBody["type"] != "blocks" {
		t.Errorf("request body type = %v, want 'blocks'", reqBody["type"])
	}

	if dep.BeadID != "bead-a" {
		t.Errorf("dep.BeadID = %q, want 'bead-a'", dep.BeadID)
	}
	if dep.DependsOnID != "bead-b" {
		t.Errorf("dep.DependsOnID = %q, want 'bead-b'", dep.DependsOnID)
	}
	if dep.Type != model.DepBlocks {
		t.Errorf("dep.Type = %q, want 'blocks'", dep.Type)
	}
}

// --- RemoveDependency ---

func TestHTTPClient_RemoveDependency(t *testing.T) {
	h := &testHandler{
		statusCode: http.StatusNoContent,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	// statusCode 204 with no body is the OLD daemon's success response, which
	// must still read as removed=true (assume-success on an absent signal).
	removed, err := c.RemoveDependency(context.Background(), "bead-a", "bead-b", "blocks")
	if err != nil {
		t.Fatalf("RemoveDependency() error = %v", err)
	}
	if !removed {
		t.Error("removed = false on a bodiless 204; want true (assume-success)")
	}

	if h.method != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", h.method)
	}
	if h.path != "/v1/beads/bead-a/dependencies" {
		t.Errorf("path = %q, want /v1/beads/bead-a/dependencies", h.path)
	}

	q := h.query
	if !strings.Contains(q, "depends_on_id=bead-b") {
		t.Errorf("query %q missing depends_on_id=bead-b", q)
	}
	if !strings.Contains(q, "type=blocks") {
		t.Errorf("query %q missing type=blocks", q)
	}
}

func TestHTTPClient_RemoveDependency_NoType(t *testing.T) {
	h := &testHandler{
		statusCode: http.StatusNoContent,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	if _, err := c.RemoveDependency(context.Background(), "bead-a", "bead-b", ""); err != nil {
		t.Fatalf("RemoveDependency() error = %v", err)
	}

	q := h.query
	if !strings.Contains(q, "depends_on_id=bead-b") {
		t.Errorf("query %q missing depends_on_id=bead-b", q)
	}
	if strings.Contains(q, "type=") {
		t.Errorf("query %q should not contain type= when depType is empty", q)
	}
}

// --- GetDependencies ---

func TestHTTPClient_GetDependencies(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"dependencies": [
				{"bead_id": "bead-a", "depends_on_id": "bead-b", "type": "blocks", "created_at": "2026-01-15T10:00:00Z"},
				{"bead_id": "bead-a", "depends_on_id": "bead-c", "type": "related", "created_at": "2026-01-16T10:00:00Z"}
			]
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	deps, err := c.GetDependencies(context.Background(), "bead-a")
	if err != nil {
		t.Fatalf("GetDependencies() error = %v", err)
	}

	if h.method != http.MethodGet {
		t.Errorf("method = %q, want GET", h.method)
	}
	if h.path != "/v1/beads/bead-a/dependencies" {
		t.Errorf("path = %q, want /v1/beads/bead-a/dependencies", h.path)
	}

	if len(deps) != 2 {
		t.Fatalf("len(deps) = %d, want 2", len(deps))
	}
	if deps[0].DependsOnID != "bead-b" {
		t.Errorf("deps[0].DependsOnID = %q, want 'bead-b'", deps[0].DependsOnID)
	}
	if deps[1].Type != model.DepRelated {
		t.Errorf("deps[1].Type = %q, want 'related'", deps[1].Type)
	}
}

func TestHTTPClient_GetDependencies_Empty(t *testing.T) {
	h := &testHandler{
		responseBody: `{"dependencies": []}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	deps, err := c.GetDependencies(context.Background(), "bead-lonely")
	if err != nil {
		t.Fatalf("GetDependencies() error = %v", err)
	}

	if len(deps) != 0 {
		t.Errorf("len(deps) = %d, want 0", len(deps))
	}
}

// --- AddLabel ---

func TestHTTPClient_AddLabel(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"id": "bead-lbl",
			"title": "Labeled",
			"type": "task",
			"kind": "issue",
			"status": "open",
			"priority": 1,
			"labels": ["urgent", "backend"],
			"created_at": "2026-01-15T10:00:00Z",
			"updated_at": "2026-01-15T10:00:00Z"
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	bead, err := c.AddLabel(context.Background(), "bead-lbl", "backend")
	if err != nil {
		t.Fatalf("AddLabel() error = %v", err)
	}

	if h.method != http.MethodPost {
		t.Errorf("method = %q, want POST", h.method)
	}
	if h.path != "/v1/beads/bead-lbl/labels" {
		t.Errorf("path = %q, want /v1/beads/bead-lbl/labels", h.path)
	}

	var reqBody map[string]interface{}
	if err := json.Unmarshal([]byte(h.body), &reqBody); err != nil {
		t.Fatalf("unmarshaling request body: %v", err)
	}
	if reqBody["label"] != "backend" {
		t.Errorf("request body label = %v, want 'backend'", reqBody["label"])
	}

	if len(bead.Labels) != 2 {
		t.Errorf("len(bead.Labels) = %d, want 2", len(bead.Labels))
	}
}

// --- RemoveLabel ---

func TestHTTPClient_RemoveLabel(t *testing.T) {
	h := &testHandler{
		statusCode: http.StatusNoContent,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	// statusCode 204 with no body is the OLD daemon's success response, which
	// must still read as removed=true (assume-success on an absent signal).
	removed, err := c.RemoveLabel(context.Background(), "bead-lbl", "backend")
	if err != nil {
		t.Fatalf("RemoveLabel() error = %v", err)
	}
	if !removed {
		t.Error("removed = false on a bodiless 204; want true (assume-success)")
	}

	if h.method != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", h.method)
	}
	if h.path != "/v1/beads/bead-lbl/labels/backend" {
		t.Errorf("path = %q, want /v1/beads/bead-lbl/labels/backend", h.path)
	}
}

func TestHTTPClient_RemoveLabel_SpecialChars(t *testing.T) {
	h := &testHandler{
		statusCode: http.StatusNoContent,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	if _, err := c.RemoveLabel(context.Background(), "bead-lbl", "team/backend"); err != nil {
		t.Fatalf("RemoveLabel() error = %v", err)
	}

	wantURI := "/v1/beads/bead-lbl/labels/team%2Fbackend"
	if h.requestURI != wantURI {
		t.Errorf("requestURI = %q, want %q", h.requestURI, wantURI)
	}
}

// --- GetLabels ---

func TestHTTPClient_GetLabels(t *testing.T) {
	h := &testHandler{
		responseBody: `{"labels": ["urgent", "backend", "P0"]}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	labels, err := c.GetLabels(context.Background(), "bead-lbl")
	if err != nil {
		t.Fatalf("GetLabels() error = %v", err)
	}

	if h.method != http.MethodGet {
		t.Errorf("method = %q, want GET", h.method)
	}
	if h.path != "/v1/beads/bead-lbl/labels" {
		t.Errorf("path = %q, want /v1/beads/bead-lbl/labels", h.path)
	}

	if len(labels) != 3 {
		t.Fatalf("len(labels) = %d, want 3", len(labels))
	}
	if labels[0] != "urgent" {
		t.Errorf("labels[0] = %q, want 'urgent'", labels[0])
	}
	if labels[2] != "P0" {
		t.Errorf("labels[2] = %q, want 'P0'", labels[2])
	}
}

func TestHTTPClient_GetLabels_Empty(t *testing.T) {
	h := &testHandler{
		responseBody: `{"labels": []}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	labels, err := c.GetLabels(context.Background(), "bead-no-labels")
	if err != nil {
		t.Fatalf("GetLabels() error = %v", err)
	}
	if len(labels) != 0 {
		t.Errorf("len(labels) = %d, want 0", len(labels))
	}
}

// --- AddComment ---

func TestHTTPClient_AddComment(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"id": 42,
			"bead_id": "bead-cmt",
			"author": "alice",
			"text": "This looks good!",
			"created_at": "2026-01-15T10:00:00Z"
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	comment, err := c.AddComment(context.Background(), "bead-cmt", "alice", "This looks good!")
	if err != nil {
		t.Fatalf("AddComment() error = %v", err)
	}

	if h.method != http.MethodPost {
		t.Errorf("method = %q, want POST", h.method)
	}
	if h.path != "/v1/beads/bead-cmt/comments" {
		t.Errorf("path = %q, want /v1/beads/bead-cmt/comments", h.path)
	}

	var reqBody map[string]interface{}
	if err := json.Unmarshal([]byte(h.body), &reqBody); err != nil {
		t.Fatalf("unmarshaling request body: %v", err)
	}
	if reqBody["author"] != "alice" {
		t.Errorf("request body author = %v, want 'alice'", reqBody["author"])
	}
	if reqBody["text"] != "This looks good!" {
		t.Errorf("request body text = %v, want 'This looks good!'", reqBody["text"])
	}

	if comment.ID != 42 {
		t.Errorf("comment.ID = %d, want 42", comment.ID)
	}
	if comment.BeadID != "bead-cmt" {
		t.Errorf("comment.BeadID = %q, want 'bead-cmt'", comment.BeadID)
	}
	if comment.Author != "alice" {
		t.Errorf("comment.Author = %q, want 'alice'", comment.Author)
	}
	if comment.Text != "This looks good!" {
		t.Errorf("comment.Text = %q, want 'This looks good!'", comment.Text)
	}
}

// --- GetComments ---

func TestHTTPClient_GetComments(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"comments": [
				{"id": 1, "bead_id": "bead-cmt", "author": "alice", "text": "First", "created_at": "2026-01-15T10:00:00Z"},
				{"id": 2, "bead_id": "bead-cmt", "author": "bob", "text": "Second", "created_at": "2026-01-15T11:00:00Z"}
			]
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	comments, err := c.GetComments(context.Background(), "bead-cmt")
	if err != nil {
		t.Fatalf("GetComments() error = %v", err)
	}

	if h.method != http.MethodGet {
		t.Errorf("method = %q, want GET", h.method)
	}
	if h.path != "/v1/beads/bead-cmt/comments" {
		t.Errorf("path = %q, want /v1/beads/bead-cmt/comments", h.path)
	}

	if len(comments) != 2 {
		t.Fatalf("len(comments) = %d, want 2", len(comments))
	}
	if comments[0].Author != "alice" {
		t.Errorf("comments[0].Author = %q, want 'alice'", comments[0].Author)
	}
	if comments[1].Text != "Second" {
		t.Errorf("comments[1].Text = %q, want 'Second'", comments[1].Text)
	}
}

func TestHTTPClient_GetComments_Empty(t *testing.T) {
	h := &testHandler{
		responseBody: `{"comments": []}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	comments, err := c.GetComments(context.Background(), "bead-no-cmt")
	if err != nil {
		t.Fatalf("GetComments() error = %v", err)
	}
	if len(comments) != 0 {
		t.Errorf("len(comments) = %d, want 0", len(comments))
	}
}

// --- GetEvents ---

func TestHTTPClient_GetEvents(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"events": [
				{"id": 1, "topic": "bead.created", "bead_id": "bead-evt", "actor": "alice", "payload": {"title": "New"}, "created_at": "2026-01-15T10:00:00Z"},
				{"id": 2, "topic": "bead.updated", "bead_id": "bead-evt", "actor": "bob", "payload": {"status": "closed"}, "created_at": "2026-01-16T10:00:00Z"}
			]
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	events, err := c.GetEvents(context.Background(), "bead-evt")
	if err != nil {
		t.Fatalf("GetEvents() error = %v", err)
	}

	if h.method != http.MethodGet {
		t.Errorf("method = %q, want GET", h.method)
	}
	if h.path != "/v1/beads/bead-evt/events" {
		t.Errorf("path = %q, want /v1/beads/bead-evt/events", h.path)
	}

	if len(events) != 2 {
		t.Fatalf("len(events) = %d, want 2", len(events))
	}
	if events[0].Topic != "bead.created" {
		t.Errorf("events[0].Topic = %q, want 'bead.created'", events[0].Topic)
	}
	if events[0].Actor != "alice" {
		t.Errorf("events[0].Actor = %q, want 'alice'", events[0].Actor)
	}
	if events[0].Payload == nil {
		t.Error("events[0].Payload is nil, want non-nil")
	}
	if events[1].ID != 2 {
		t.Errorf("events[1].ID = %d, want 2", events[1].ID)
	}
}

func TestHTTPClient_GetEvents_Empty(t *testing.T) {
	h := &testHandler{
		responseBody: `{"events": []}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	events, err := c.GetEvents(context.Background(), "bead-no-evt")
	if err != nil {
		t.Fatalf("GetEvents() error = %v", err)
	}
	if len(events) != 0 {
		t.Errorf("len(events) = %d, want 0", len(events))
	}
}
