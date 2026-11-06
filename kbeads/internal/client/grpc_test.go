package client

import (
	"encoding/json"
	"testing"
	"time"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// --- protoBeadToModel ---

func TestProtoBeadToModel_FullFields(t *testing.T) {
	now := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	closed := time.Date(2026, 1, 20, 15, 0, 0, 0, time.UTC)
	due := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	deferred := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	pb := &beadsv1.Bead{
		Id:          "bead-full",
		Slug:        "bd-full",
		Kind:        "issue",
		Type:        "task",
		Title:       "Full bead",
		Description: "A complete bead",
		Notes:       "Some notes",
		Status:      "open",
		Priority:    2,
		Assignee:    "alice",
		Owner:       "bob",
		CreatedBy:   "alice",
		Labels:      []string{"urgent", "backend"},
		CreatedAt:   timestamppb.New(now),
		UpdatedAt:   timestamppb.New(now),
		ClosedAt:    timestamppb.New(closed),
		DueAt:       timestamppb.New(due),
		DeferUntil:  timestamppb.New(deferred),
		Fields:      []byte(`{"custom": "value"}`),
		Dependencies: []*beadsv1.Dependency{
			{
				BeadId:      "bead-full",
				DependsOnId: "bead-dep",
				Type:        "blocks",
				CreatedAt:   timestamppb.New(now),
			},
		},
		Comments: []*beadsv1.Comment{
			{
				Id:        1,
				BeadId:    "bead-full",
				Author:    "alice",
				Text:      "Hello",
				CreatedAt: timestamppb.New(now),
			},
		},
	}

	b := protoBeadToModel(pb)

	if b.ID != "bead-full" {
		t.Errorf("ID = %q, want 'bead-full'", b.ID)
	}
	if b.Slug != "bd-full" {
		t.Errorf("Slug = %q, want 'bd-full'", b.Slug)
	}
	if b.Kind != model.KindIssue {
		t.Errorf("Kind = %q, want 'issue'", b.Kind)
	}
	if b.Type != model.TypeTask {
		t.Errorf("Type = %q, want 'task'", b.Type)
	}
	if b.Title != "Full bead" {
		t.Errorf("Title = %q, want 'Full bead'", b.Title)
	}
	if b.Description != "A complete bead" {
		t.Errorf("Description = %q, want 'A complete bead'", b.Description)
	}
	if b.Notes != "Some notes" {
		t.Errorf("Notes = %q, want 'Some notes'", b.Notes)
	}
	if b.Status != model.StatusOpen {
		t.Errorf("Status = %q, want 'open'", b.Status)
	}
	if b.Priority != 2 {
		t.Errorf("Priority = %d, want 2", b.Priority)
	}
	if b.Assignee != "alice" {
		t.Errorf("Assignee = %q, want 'alice'", b.Assignee)
	}
	if b.Owner != "bob" {
		t.Errorf("Owner = %q, want 'bob'", b.Owner)
	}
	if b.CreatedBy != "alice" {
		t.Errorf("CreatedBy = %q, want 'alice'", b.CreatedBy)
	}
	if len(b.Labels) != 2 {
		t.Fatalf("len(Labels) = %d, want 2", len(b.Labels))
	}
	if b.Labels[0] != "urgent" {
		t.Errorf("Labels[0] = %q, want 'urgent'", b.Labels[0])
	}

	if !b.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", b.CreatedAt, now)
	}
	if !b.UpdatedAt.Equal(now) {
		t.Errorf("UpdatedAt = %v, want %v", b.UpdatedAt, now)
	}
	if b.ClosedAt == nil {
		t.Fatal("ClosedAt is nil, want non-nil")
	}
	if !b.ClosedAt.Equal(closed) {
		t.Errorf("ClosedAt = %v, want %v", *b.ClosedAt, closed)
	}
	if b.DueAt == nil {
		t.Fatal("DueAt is nil, want non-nil")
	}
	if !b.DueAt.Equal(due) {
		t.Errorf("DueAt = %v, want %v", *b.DueAt, due)
	}
	if b.DeferUntil == nil {
		t.Fatal("DeferUntil is nil, want non-nil")
	}
	if !b.DeferUntil.Equal(deferred) {
		t.Errorf("DeferUntil = %v, want %v", *b.DeferUntil, deferred)
	}

	if b.Fields == nil {
		t.Fatal("Fields is nil, want non-nil")
	}
	var fields map[string]string
	if err := json.Unmarshal(b.Fields, &fields); err != nil {
		t.Fatalf("unmarshaling fields: %v", err)
	}
	if fields["custom"] != "value" {
		t.Errorf("fields[custom] = %q, want 'value'", fields["custom"])
	}

	if len(b.Dependencies) != 1 {
		t.Fatalf("len(Dependencies) = %d, want 1", len(b.Dependencies))
	}
	if b.Dependencies[0].DependsOnID != "bead-dep" {
		t.Errorf("Dependencies[0].DependsOnID = %q, want 'bead-dep'", b.Dependencies[0].DependsOnID)
	}

	if len(b.Comments) != 1 {
		t.Fatalf("len(Comments) = %d, want 1", len(b.Comments))
	}
	if b.Comments[0].Author != "alice" {
		t.Errorf("Comments[0].Author = %q, want 'alice'", b.Comments[0].Author)
	}
}

func TestProtoBeadToModel_NilTimestamps(t *testing.T) {
	pb := &beadsv1.Bead{
		Id:       "bead-nil-ts",
		Title:    "No timestamps",
		Kind:     "issue",
		Type:     "task",
		Status:   "open",
		Priority: 0,
		// All timestamp fields are nil
	}

	b := protoBeadToModel(pb)

	if b.CreatedAt != (time.Time{}) {
		t.Errorf("CreatedAt = %v, want zero time", b.CreatedAt)
	}
	if b.UpdatedAt != (time.Time{}) {
		t.Errorf("UpdatedAt = %v, want zero time", b.UpdatedAt)
	}
	if b.ClosedAt != nil {
		t.Errorf("ClosedAt = %v, want nil", b.ClosedAt)
	}
	if b.DueAt != nil {
		t.Errorf("DueAt = %v, want nil", b.DueAt)
	}
	if b.DeferUntil != nil {
		t.Errorf("DeferUntil = %v, want nil", b.DeferUntil)
	}
}

func TestProtoBeadToModel_EmptyFields(t *testing.T) {
	pb := &beadsv1.Bead{
		Id:     "bead-empty",
		Title:  "Empty fields",
		Kind:   "issue",
		Type:   "task",
		Status: "open",
		Fields: nil, // explicitly nil
	}

	b := protoBeadToModel(pb)
	if b.Fields != nil {
		t.Errorf("Fields = %v, want nil", b.Fields)
	}
}

func TestProtoBeadToModel_Nil(t *testing.T) {
	b := protoBeadToModel(nil)
	if b != nil {
		t.Errorf("protoBeadToModel(nil) = %v, want nil", b)
	}
}

func TestProtoBeadToModel_NoDependenciesOrComments(t *testing.T) {
	pb := &beadsv1.Bead{
		Id:     "bead-bare",
		Title:  "Bare",
		Kind:   "issue",
		Type:   "task",
		Status: "open",
	}

	b := protoBeadToModel(pb)
	if b.Dependencies != nil {
		t.Errorf("Dependencies = %v, want nil", b.Dependencies)
	}
	if b.Comments != nil {
		t.Errorf("Comments = %v, want nil", b.Comments)
	}
}

// --- protoDependencyToModel ---

func TestProtoDependencyToModel(t *testing.T) {
	now := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	pb := &beadsv1.Dependency{
		BeadId:      "bead-a",
		DependsOnId: "bead-b",
		Type:        "blocks",
		CreatedBy:   "alice",
		CreatedAt:   timestamppb.New(now),
	}

	d := protoDependencyToModel(pb)

	if d.BeadID != "bead-a" {
		t.Errorf("BeadID = %q, want 'bead-a'", d.BeadID)
	}
	if d.DependsOnID != "bead-b" {
		t.Errorf("DependsOnID = %q, want 'bead-b'", d.DependsOnID)
	}
	if d.Type != model.DepBlocks {
		t.Errorf("Type = %q, want 'blocks'", d.Type)
	}
	if d.CreatedBy != "alice" {
		t.Errorf("CreatedBy = %q, want 'alice'", d.CreatedBy)
	}
	if !d.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", d.CreatedAt, now)
	}
}

func TestProtoDependencyToModel_Nil(t *testing.T) {
	d := protoDependencyToModel(nil)
	if d != nil {
		t.Errorf("protoDependencyToModel(nil) = %v, want nil", d)
	}
}

func TestProtoDependencyToModel_NilTimestamp(t *testing.T) {
	pb := &beadsv1.Dependency{
		BeadId:      "bead-a",
		DependsOnId: "bead-b",
		Type:        "related",
	}

	d := protoDependencyToModel(pb)
	if d.CreatedAt != (time.Time{}) {
		t.Errorf("CreatedAt = %v, want zero time", d.CreatedAt)
	}
}

// --- protoCommentToModel ---

func TestProtoCommentToModel(t *testing.T) {
	now := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	pb := &beadsv1.Comment{
		Id:        42,
		BeadId:    "bead-cmt",
		Author:    "alice",
		Text:      "Nice work!",
		CreatedAt: timestamppb.New(now),
	}

	c := protoCommentToModel(pb)

	if c.ID != 42 {
		t.Errorf("ID = %d, want 42", c.ID)
	}
	if c.BeadID != "bead-cmt" {
		t.Errorf("BeadID = %q, want 'bead-cmt'", c.BeadID)
	}
	if c.Author != "alice" {
		t.Errorf("Author = %q, want 'alice'", c.Author)
	}
	if c.Text != "Nice work!" {
		t.Errorf("Text = %q, want 'Nice work!'", c.Text)
	}
	if !c.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", c.CreatedAt, now)
	}
}

func TestProtoCommentToModel_Nil(t *testing.T) {
	c := protoCommentToModel(nil)
	if c != nil {
		t.Errorf("protoCommentToModel(nil) = %v, want nil", c)
	}
}

func TestProtoCommentToModel_NilTimestamp(t *testing.T) {
	pb := &beadsv1.Comment{
		Id:     1,
		BeadId: "bead-cmt",
		Author: "bob",
		Text:   "Hello",
	}

	c := protoCommentToModel(pb)
	if c.CreatedAt != (time.Time{}) {
		t.Errorf("CreatedAt = %v, want zero time", c.CreatedAt)
	}
}

// --- protoEventToModel ---

func TestProtoEventToModel(t *testing.T) {
	now := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	pb := &beadsv1.Event{
		Id:        99,
		Topic:     "bead.created",
		BeadId:    "bead-evt",
		Actor:     "alice",
		Payload:   []byte(`{"title": "New bead"}`),
		CreatedAt: timestamppb.New(now),
	}

	e := protoEventToModel(pb)

	if e.ID != 99 {
		t.Errorf("ID = %d, want 99", e.ID)
	}
	if e.Topic != "bead.created" {
		t.Errorf("Topic = %q, want 'bead.created'", e.Topic)
	}
	if e.BeadID != "bead-evt" {
		t.Errorf("BeadID = %q, want 'bead-evt'", e.BeadID)
	}
	if e.Actor != "alice" {
		t.Errorf("Actor = %q, want 'alice'", e.Actor)
	}
	if e.Payload == nil {
		t.Fatal("Payload is nil, want non-nil")
	}
	var payload map[string]string
	if err := json.Unmarshal(e.Payload, &payload); err != nil {
		t.Fatalf("unmarshaling payload: %v", err)
	}
	if payload["title"] != "New bead" {
		t.Errorf("payload[title] = %q, want 'New bead'", payload["title"])
	}
	if !e.CreatedAt.Equal(now) {
		t.Errorf("CreatedAt = %v, want %v", e.CreatedAt, now)
	}
}

func TestProtoEventToModel_Nil(t *testing.T) {
	e := protoEventToModel(nil)
	if e != nil {
		t.Errorf("protoEventToModel(nil) = %v, want nil", e)
	}
}

func TestProtoEventToModel_NilTimestamp(t *testing.T) {
	pb := &beadsv1.Event{
		Id:    1,
		Topic: "bead.updated",
	}

	e := protoEventToModel(pb)
	if e.CreatedAt != (time.Time{}) {
		t.Errorf("CreatedAt = %v, want zero time", e.CreatedAt)
	}
}

func TestProtoEventToModel_EmptyPayload(t *testing.T) {
	pb := &beadsv1.Event{
		Id:      1,
		Topic:   "bead.deleted",
		BeadId:  "bead-x",
		Payload: nil,
	}

	e := protoEventToModel(pb)
	if e.Payload != nil {
		t.Errorf("Payload = %v, want nil", e.Payload)
	}
}

// --- protoConfigToModel ---

func TestProtoConfigToModel(t *testing.T) {
	created := time.Date(2026, 1, 15, 10, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 1, 16, 14, 0, 0, 0, time.UTC)
	pb := &beadsv1.Config{
		Key:       "view:inbox",
		Value:     []byte(`{"columns": ["title"]}`),
		CreatedAt: timestamppb.New(created),
		UpdatedAt: timestamppb.New(updated),
	}

	c := protoConfigToModel(pb)

	if c.Key != "view:inbox" {
		t.Errorf("Key = %q, want 'view:inbox'", c.Key)
	}
	if c.Value == nil {
		t.Fatal("Value is nil, want non-nil")
	}
	if !c.CreatedAt.Equal(created) {
		t.Errorf("CreatedAt = %v, want %v", c.CreatedAt, created)
	}
	if !c.UpdatedAt.Equal(updated) {
		t.Errorf("UpdatedAt = %v, want %v", c.UpdatedAt, updated)
	}
}

func TestProtoConfigToModel_Nil(t *testing.T) {
	c := protoConfigToModel(nil)
	if c != nil {
		t.Errorf("protoConfigToModel(nil) = %v, want nil", c)
	}
}

func TestProtoConfigToModel_NilTimestamps(t *testing.T) {
	pb := &beadsv1.Config{
		Key:   "test:key",
		Value: []byte(`"hello"`),
	}

	c := protoConfigToModel(pb)
	if c.CreatedAt != (time.Time{}) {
		t.Errorf("CreatedAt = %v, want zero time", c.CreatedAt)
	}
	if c.UpdatedAt != (time.Time{}) {
		t.Errorf("UpdatedAt = %v, want zero time", c.UpdatedAt)
	}
}

// --- Interface compliance ---

func TestGRPCClient_ImplementsBeadsClient(t *testing.T) {
	var _ BeadsClient = (*GRPCClient)(nil)
}
