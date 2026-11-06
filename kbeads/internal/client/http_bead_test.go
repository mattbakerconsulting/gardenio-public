package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// --- CreateBead ---

func TestHTTPClient_CreateBead(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"id": "bead-abc",
			"slug": "bd-abc",
			"kind": "issue",
			"type": "task",
			"title": "Fix the widget",
			"description": "It is broken",
			"status": "open",
			"priority": 2,
			"assignee": "alice",
			"owner": "bob",
			"created_by": "alice",
			"created_at": "2026-01-15T10:00:00Z",
			"updated_at": "2026-01-15T10:00:00Z",
			"labels": ["urgent", "backend"]
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	due := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	req := &CreateBeadRequest{
		Title:       "Fix the widget",
		Kind:        "issue",
		Type:        "task",
		Description: "It is broken",
		Priority:    2,
		Assignee:    "alice",
		Owner:       "bob",
		Labels:      []string{"urgent", "backend"},
		CreatedBy:   "alice",
		DueAt:       &due,
	}

	bead, err := c.CreateBead(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateBead() error = %v", err)
	}

	// Verify request
	if h.method != http.MethodPost {
		t.Errorf("method = %q, want POST", h.method)
	}
	if h.path != "/v1/beads" {
		t.Errorf("path = %q, want /v1/beads", h.path)
	}
	if h.contentType != "application/json" {
		t.Errorf("content-type = %q, want application/json", h.contentType)
	}

	// Verify request body contains expected fields
	var reqBody map[string]interface{}
	if err := json.Unmarshal([]byte(h.body), &reqBody); err != nil {
		t.Fatalf("unmarshaling request body: %v", err)
	}
	if reqBody["title"] != "Fix the widget" {
		t.Errorf("request body title = %v, want 'Fix the widget'", reqBody["title"])
	}
	if reqBody["type"] != "task" {
		t.Errorf("request body type = %v, want 'task'", reqBody["type"])
	}
	if reqBody["assignee"] != "alice" {
		t.Errorf("request body assignee = %v, want 'alice'", reqBody["assignee"])
	}
	if reqBody["due_at"] == nil {
		t.Error("request body due_at is nil, want non-nil")
	}

	// Verify response parsing
	if bead.ID != "bead-abc" {
		t.Errorf("bead.ID = %q, want 'bead-abc'", bead.ID)
	}
	if bead.Slug != "bd-abc" {
		t.Errorf("bead.Slug = %q, want 'bd-abc'", bead.Slug)
	}
	if bead.Kind != model.KindIssue {
		t.Errorf("bead.Kind = %q, want 'issue'", bead.Kind)
	}
	if bead.Type != model.TypeTask {
		t.Errorf("bead.Type = %q, want 'task'", bead.Type)
	}
	if bead.Title != "Fix the widget" {
		t.Errorf("bead.Title = %q, want 'Fix the widget'", bead.Title)
	}
	if bead.Priority != 2 {
		t.Errorf("bead.Priority = %d, want 2", bead.Priority)
	}
	if bead.Assignee != "alice" {
		t.Errorf("bead.Assignee = %q, want 'alice'", bead.Assignee)
	}
	if len(bead.Labels) != 2 {
		t.Errorf("len(bead.Labels) = %d, want 2", len(bead.Labels))
	}
}

func TestHTTPClient_CreateBead_MinimalFields(t *testing.T) {
	h := &testHandler{
		responseBody: `{"id": "bead-min", "title": "Minimal", "type": "task", "kind": "issue", "status": "open", "priority": 0, "created_at": "2026-01-15T10:00:00Z", "updated_at": "2026-01-15T10:00:00Z"}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	req := &CreateBeadRequest{
		Title: "Minimal",
		Type:  "task",
	}

	bead, err := c.CreateBead(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateBead() error = %v", err)
	}
	if bead.ID != "bead-min" {
		t.Errorf("bead.ID = %q, want 'bead-min'", bead.ID)
	}

	// Verify omitempty fields are absent from request body
	var reqBody map[string]interface{}
	if err := json.Unmarshal([]byte(h.body), &reqBody); err != nil {
		t.Fatalf("unmarshaling request body: %v", err)
	}
	if _, ok := reqBody["assignee"]; ok {
		t.Error("request body should not contain 'assignee' when empty")
	}
	if _, ok := reqBody["due_at"]; ok {
		t.Error("request body should not contain 'due_at' when nil")
	}
	if _, ok := reqBody["labels"]; ok {
		t.Error("request body should not contain 'labels' when nil")
	}
}

// --- GetBead ---

func TestHTTPClient_GetBead(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"id": "bead-123",
			"slug": "bd-123",
			"kind": "issue",
			"type": "bug",
			"title": "Something broke",
			"status": "open",
			"priority": 1,
			"created_at": "2026-01-10T08:00:00Z",
			"updated_at": "2026-01-11T09:30:00Z",
			"closed_at": "2026-01-12T12:00:00Z",
			"closed_by": "bob",
			"fields": {"severity": "high"}
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	bead, err := c.GetBead(context.Background(), "bead-123")
	if err != nil {
		t.Fatalf("GetBead() error = %v", err)
	}

	if h.method != http.MethodGet {
		t.Errorf("method = %q, want GET", h.method)
	}
	if h.path != "/v1/beads/bead-123" {
		t.Errorf("path = %q, want /v1/beads/bead-123", h.path)
	}
	if h.contentType != "" {
		t.Errorf("GET should not have Content-Type, got %q", h.contentType)
	}

	if bead.ID != "bead-123" {
		t.Errorf("bead.ID = %q, want 'bead-123'", bead.ID)
	}
	if bead.Type != model.TypeBug {
		t.Errorf("bead.Type = %q, want 'bug'", bead.Type)
	}
	if bead.ClosedAt == nil {
		t.Error("bead.ClosedAt is nil, want non-nil")
	}
	if bead.ClosedBy != "bob" {
		t.Errorf("bead.ClosedBy = %q, want 'bob'", bead.ClosedBy)
	}
	if bead.Fields == nil {
		t.Error("bead.Fields is nil, want non-nil")
	}
}

func TestHTTPClient_GetBead_URLEscaping(t *testing.T) {
	h := &testHandler{
		responseBody: `{"id": "bead/special", "title": "Test", "type": "task", "kind": "issue", "status": "open", "priority": 0, "created_at": "2026-01-15T10:00:00Z", "updated_at": "2026-01-15T10:00:00Z"}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	_, err := c.GetBead(context.Background(), "bead/special")
	if err != nil {
		t.Fatalf("GetBead() error = %v", err)
	}

	wantURI := "/v1/beads/bead%2Fspecial"
	if h.requestURI != wantURI {
		t.Errorf("requestURI = %q, want %q", h.requestURI, wantURI)
	}
}

// --- ListBeads ---

func TestHTTPClient_ListBeads(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"beads": [
				{"id": "b1", "title": "First", "type": "task", "kind": "issue", "status": "open", "priority": 1, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"},
				{"id": "b2", "title": "Second", "type": "bug", "kind": "issue", "status": "closed", "priority": 2, "created_at": "2026-01-02T00:00:00Z", "updated_at": "2026-01-02T00:00:00Z"}
			],
			"total": 42
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	prio := 1
	resp, err := c.ListBeads(context.Background(), &ListBeadsRequest{
		Status:   []string{"open", "in_progress"},
		Type:     []string{"task", "bug"},
		Kind:     []string{"issue"},
		Labels:   []string{"urgent", "backend"},
		Assignee: "alice",
		Search:   "widget",
		Sort:     "-priority",
		Priority: &prio,
		Limit:    10,
		Offset:   20,
	})
	if err != nil {
		t.Fatalf("ListBeads() error = %v", err)
	}

	if h.method != http.MethodGet {
		t.Errorf("method = %q, want GET", h.method)
	}
	if h.path != "/v1/beads" {
		t.Errorf("path = %q, want /v1/beads", h.path)
	}

	q := h.query
	for _, want := range []string{
		"status=open%2Cin_progress",
		"type=task%2Cbug",
		"kind=issue",
		"labels=urgent%2Cbackend",
		"assignee=alice",
		"search=widget",
		"sort=-priority",
		"priority=1",
		"limit=10",
		"offset=20",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q does not contain %q", q, want)
		}
	}

	if len(resp.Beads) != 2 {
		t.Fatalf("len(beads) = %d, want 2", len(resp.Beads))
	}
	if resp.Total != 42 {
		t.Errorf("total = %d, want 42", resp.Total)
	}
	if resp.Beads[0].ID != "b1" {
		t.Errorf("beads[0].ID = %q, want 'b1'", resp.Beads[0].ID)
	}
	if resp.Beads[1].Status != model.StatusClosed {
		t.Errorf("beads[1].Status = %q, want 'closed'", resp.Beads[1].Status)
	}
}

func TestHTTPClient_ListBeads_NoFilters(t *testing.T) {
	h := &testHandler{
		responseBody: `{"beads": [], "total": 0}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	resp, err := c.ListBeads(context.Background(), &ListBeadsRequest{})
	if err != nil {
		t.Fatalf("ListBeads() error = %v", err)
	}

	if h.query != "" {
		t.Errorf("query = %q, want empty", h.query)
	}
	if len(resp.Beads) != 0 {
		t.Errorf("len(beads) = %d, want 0", len(resp.Beads))
	}
	if resp.Total != 0 {
		t.Errorf("total = %d, want 0", resp.Total)
	}
}

func TestHTTPClient_ListBeads_EmptyResponse(t *testing.T) {
	h := &testHandler{
		responseBody: `{"beads": null, "total": 0}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	resp, err := c.ListBeads(context.Background(), &ListBeadsRequest{})
	if err != nil {
		t.Fatalf("ListBeads() error = %v", err)
	}

	if resp.Beads != nil {
		t.Errorf("beads = %v, want nil", resp.Beads)
	}
}

// TestHTTPClient_ListBeads_QueryParams pins how each optional ListBeadsRequest
// filter rides the wire. An unset filter emits nothing: that half is pinned by
// TestHTTPClient_ListBeads_NoFilters, whose empty request must send an empty
// query (a stray unassigned=true would hide every claimed bead; a stray
// with_open_blockers=true would put the blocker join on every default caller).
func TestHTTPClient_ListBeads_QueryParams(t *testing.T) {
	ts := func(s string) *time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parsing %q: %v", s, err)
		}
		return &v
	}
	for _, tc := range []struct {
		name string
		req  ListBeadsRequest
		want []string
	}{
		// ?parent= resolves server-side to the transitive parent-child subtree
		// (ka-0000000208).
		{"parent", ListBeadsRequest{Parent: "gd-Epic0000001"}, []string{"parent=gd-Epic0000001"}},
		{"field filters", ListBeadsRequest{FieldFilters: map[string]string{"jira_has_images": "true"}}, []string{"field=jira_has_images%3Dtrue"}},
		{"mentions", ListBeadsRequest{Mentions: "ka-0000004973"}, []string{"mentions=ka-0000004973"}},
		// The NOT-IN filters (ka-0000007103, ka-0000007843).
		{"exclude labels", ListBeadsRequest{ExcludeLabels: []string{"needs-intake", "release-regression"}}, []string{"exclude_labels=needs-intake%2Crelease-regression"}},
		{"exclude types", ListBeadsRequest{ExcludeTypes: []string{"message", "event"}}, []string{"exclude_type=message%2Cevent"}},
		// An empty ?assignee= means "no filter" server-side, hence its own param
		// (ka-0000007209).
		{"unassigned", ListBeadsRequest{Unassigned: true}, []string{"unassigned=true"}},
		{"has report true", ListBeadsRequest{HasReport: boolPtr(true)}, []string{"has_report=true"}},
		{"has report false", ListBeadsRequest{HasReport: boolPtr(false)}, []string{"has_report=false"}},
		{"blocked on decision true", ListBeadsRequest{BlockedOnDecision: boolPtr(true)}, []string{"blocked_on_decision=true"}},
		{"blocked on decision false", ListBeadsRequest{BlockedOnDecision: boolPtr(false)}, []string{"blocked_on_decision=false"}},
		{"with open blockers", ListBeadsRequest{WithOpenBlockers: true}, []string{"with_open_blockers=true"}},
		{"count only", ListBeadsRequest{CountOnly: true}, []string{"count_only=true"}},
		{"fleet", ListBeadsRequest{Fleet: "*"}, []string{"fleet=%2A"}},
		// RFC3339 time windows (ka-0000008195).
		{"time window", ListBeadsRequest{
			CreatedAfter:  ts("2026-07-01T00:00:00Z"),
			CreatedBefore: ts("2026-07-02T00:00:00Z"),
			UpdatedAfter:  ts("2026-07-03T00:00:00Z"),
			UpdatedBefore: ts("2026-07-04T00:00:00Z"),
			ClosedAfter:   ts("2026-07-05T00:00:00Z"),
			ClosedBefore:  ts("2026-07-06T00:00:00Z"),
		}, []string{
			"created_after=2026-07-01T00%3A00%3A00Z",
			"created_before=2026-07-02T00%3A00%3A00Z",
			"updated_after=2026-07-03T00%3A00%3A00Z",
			"updated_before=2026-07-04T00%3A00%3A00Z",
			"closed_after=2026-07-05T00%3A00%3A00Z",
			"closed_before=2026-07-06T00%3A00%3A00Z",
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &testHandler{responseBody: `{"beads": [], "total": 0}`}
			c, srv := newTestClient(h)
			defer srv.Close()

			if _, err := c.ListBeads(context.Background(), &tc.req); err != nil {
				t.Fatalf("ListBeads() error = %v", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(h.query, want) {
					t.Errorf("query %q does not contain %q", h.query, want)
				}
			}
		})
	}
}

func boolPtr(b bool) *bool { return &b }

func TestHTTPClient_TransferBead(t *testing.T) {
	h := &testHandler{
		responseBody: `{"id": "gd-t1", "title": "Transferred", "fleet": "beta", "status": "open", "kind": "issue", "type": "task", "created_at": "2026-01-15T10:00:00Z", "updated_at": "2026-01-16T14:00:00Z"}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	bead, err := c.TransferBead(context.Background(), "gd-t1", "beta")
	if err != nil {
		t.Fatalf("TransferBead() error = %v", err)
	}
	if bead.Fleet != "beta" {
		t.Errorf("expected fleet=beta, got %q", bead.Fleet)
	}
	if h.method != "POST" {
		t.Errorf("expected POST, got %s", h.method)
	}
	if !strings.Contains(h.path, "/v1/beads/gd-t1/transfer") {
		t.Errorf("expected transfer path, got %s", h.path)
	}
}

// --- UpdateBead ---

func TestHTTPClient_UpdateBead(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"id": "bead-upd",
			"slug": "bd-upd",
			"kind": "issue",
			"type": "task",
			"title": "Updated title",
			"description": "Updated desc",
			"status": "in_progress",
			"priority": 3,
			"assignee": "carol",
			"created_at": "2026-01-15T10:00:00Z",
			"updated_at": "2026-01-16T14:00:00Z"
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	title := "Updated title"
	desc := "Updated desc"
	status := "in_progress"
	prio := 3
	assignee := "carol"

	bead, err := c.UpdateBead(context.Background(), "bead-upd", &UpdateBeadRequest{
		Title:       &title,
		Description: &desc,
		Status:      &status,
		Priority:    &prio,
		Assignee:    &assignee,
	})
	if err != nil {
		t.Fatalf("UpdateBead() error = %v", err)
	}

	if h.method != http.MethodPatch {
		t.Errorf("method = %q, want PATCH", h.method)
	}
	if h.path != "/v1/beads/bead-upd" {
		t.Errorf("path = %q, want /v1/beads/bead-upd", h.path)
	}

	var reqBody map[string]interface{}
	if err := json.Unmarshal([]byte(h.body), &reqBody); err != nil {
		t.Fatalf("unmarshaling request body: %v", err)
	}
	if reqBody["title"] != "Updated title" {
		t.Errorf("request body title = %v, want 'Updated title'", reqBody["title"])
	}
	if _, ok := reqBody["notes"]; ok {
		t.Error("request body should not contain 'notes' when nil")
	}
	if _, ok := reqBody["owner"]; ok {
		t.Error("request body should not contain 'owner' when nil")
	}

	if bead.Title != "Updated title" {
		t.Errorf("bead.Title = %q, want 'Updated title'", bead.Title)
	}
	if bead.Status != model.StatusInProgress {
		t.Errorf("bead.Status = %q, want 'in_progress'", bead.Status)
	}
}

func TestHTTPClient_UpdateBead_OnlyNotes(t *testing.T) {
	h := &testHandler{
		responseBody: `{"id": "bead-n", "title": "X", "type": "task", "kind": "issue", "status": "open", "notes": "new notes", "priority": 0, "created_at": "2026-01-15T10:00:00Z", "updated_at": "2026-01-15T10:00:00Z"}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	notes := "new notes"
	bead, err := c.UpdateBead(context.Background(), "bead-n", &UpdateBeadRequest{
		Notes: &notes,
	})
	if err != nil {
		t.Fatalf("UpdateBead() error = %v", err)
	}

	var reqBody map[string]interface{}
	if err := json.Unmarshal([]byte(h.body), &reqBody); err != nil {
		t.Fatalf("unmarshaling request body: %v", err)
	}
	if reqBody["notes"] != "new notes" {
		t.Errorf("request body notes = %v, want 'new notes'", reqBody["notes"])
	}
	if _, ok := reqBody["title"]; ok {
		t.Error("request body should not contain 'title' when nil")
	}
	if bead.Notes != "new notes" {
		t.Errorf("bead.Notes = %q, want 'new notes'", bead.Notes)
	}
}

// --- CloseBead ---

func TestHTTPClient_CloseBead(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"id": "bead-cls",
			"title": "Closed bead",
			"type": "task",
			"kind": "issue",
			"status": "closed",
			"priority": 1,
			"closed_at": "2026-01-20T15:00:00Z",
			"closed_by": "alice",
			"created_at": "2026-01-15T10:00:00Z",
			"updated_at": "2026-01-20T15:00:00Z"
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	bead, err := c.CloseBead(context.Background(), "bead-cls", "alice")
	if err != nil {
		t.Fatalf("CloseBead() error = %v", err)
	}

	if h.method != http.MethodPost {
		t.Errorf("method = %q, want POST", h.method)
	}
	if h.path != "/v1/beads/bead-cls/close" {
		t.Errorf("path = %q, want /v1/beads/bead-cls/close", h.path)
	}

	var reqBody map[string]interface{}
	if err := json.Unmarshal([]byte(h.body), &reqBody); err != nil {
		t.Fatalf("unmarshaling request body: %v", err)
	}
	if reqBody["closed_by"] != "alice" {
		t.Errorf("request body closed_by = %v, want 'alice'", reqBody["closed_by"])
	}

	if bead.Status != model.StatusClosed {
		t.Errorf("bead.Status = %q, want 'closed'", bead.Status)
	}
	if bead.ClosedAt == nil {
		t.Error("bead.ClosedAt is nil, want non-nil")
	}
}

func TestHTTPClient_CloseBead_EmptyClosedBy(t *testing.T) {
	h := &testHandler{
		responseBody: `{"id": "bead-cls2", "title": "X", "type": "task", "kind": "issue", "status": "closed", "priority": 0, "created_at": "2026-01-15T10:00:00Z", "updated_at": "2026-01-15T10:00:00Z"}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	_, err := c.CloseBead(context.Background(), "bead-cls2", "")
	if err != nil {
		t.Fatalf("CloseBead() error = %v", err)
	}

	var reqBody map[string]interface{}
	if err := json.Unmarshal([]byte(h.body), &reqBody); err != nil {
		t.Fatalf("unmarshaling request body: %v", err)
	}
	if _, ok := reqBody["closed_by"]; ok {
		t.Error("request body should not contain 'closed_by' when empty")
	}
}

// --- DeleteBead ---

func TestHTTPClient_DeleteBead(t *testing.T) {
	h := &testHandler{
		statusCode: http.StatusNoContent,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	err := c.DeleteBead(context.Background(), "bead-del")
	if err != nil {
		t.Fatalf("DeleteBead() error = %v", err)
	}

	if h.method != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", h.method)
	}
	if h.path != "/v1/beads/bead-del" {
		t.Errorf("path = %q, want /v1/beads/bead-del", h.path)
	}
}
