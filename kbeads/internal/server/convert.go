package server

import (
	"database/sql"
	"errors"
	"time"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// beadToProto converts a model.Bead to a proto Bead message.
func beadToProto(b *model.Bead) *beadsv1.Bead {
	if b == nil {
		return nil
	}

	pb := &beadsv1.Bead{
		Id:                b.ID,
		Slug:              b.Slug,
		ShortTitle:        b.ShortTitle,
		Heraldry:          b.Heraldry,
		Kind:              string(b.Kind),
		Type:              string(b.Type),
		Title:             b.Title,
		Description:       b.Description,
		Notes:             b.Notes,
		Status:            string(b.Status),
		Priority:          int32(b.Priority),
		Assignee:          b.Assignee,
		Owner:             b.Owner,
		CreatedAt:         timestamppb.New(b.CreatedAt),
		CreatedBy:         b.CreatedBy,
		UpdatedAt:         timestamppb.New(b.UpdatedAt),
		Fields:            []byte(b.Fields),
		Labels:            b.Labels,
		OperatedBy:        b.OperatedBy,
		OperatedByDisplay: b.OperatedByDisplay,
		OperatedByEmail:   b.OperatedByEmail,
		HasReport:         b.HasReport,
		RelevanceScore:    b.RelevanceScore,
	}

	if b.ClosedAt != nil {
		pb.ClosedAt = timestamppb.New(*b.ClosedAt)
	}
	if b.DueAt != nil {
		pb.DueAt = timestamppb.New(*b.DueAt)
	}
	if b.DeferUntil != nil {
		pb.DeferUntil = timestamppb.New(*b.DeferUntil)
	}

	for _, d := range b.Dependencies {
		pb.Dependencies = append(pb.Dependencies, dependencyToProto(d))
	}
	for _, c := range b.Comments {
		pb.Comments = append(pb.Comments, commentToProto(c))
	}

	return pb
}

// dependencyToProto converts a model.Dependency to a proto Dependency message.
func dependencyToProto(d *model.Dependency) *beadsv1.Dependency {
	if d == nil {
		return nil
	}
	return &beadsv1.Dependency{
		BeadId:      d.BeadID,
		DependsOnId: d.DependsOnID,
		Type:        string(d.Type),
		CreatedAt:   timestamppb.New(d.CreatedAt),
		CreatedBy:   d.CreatedBy,
		Metadata:    d.Metadata,
		OperatedBy:  d.OperatedBy,
	}
}

// beadReferenceSummaryToProto converts a model.BeadReferenceSummary to its
// proto twin. See kbeads/docs/design-bead-references.md § 5a.
func beadReferenceSummaryToProto(r *model.BeadReferenceSummary) *beadsv1.BeadReferenceSummary {
	if r == nil {
		return nil
	}
	return &beadsv1.BeadReferenceSummary{
		Id:          r.ID,
		Title:       r.Title,
		Type:        r.Type,
		Status:      r.Status,
		SourceField: r.SourceField,
	}
}

// commentToProto converts a model.Comment to a proto Comment message.
func commentToProto(c *model.Comment) *beadsv1.Comment {
	if c == nil {
		return nil
	}
	return &beadsv1.Comment{
		Id:                c.ID,
		BeadId:            c.BeadID,
		Author:            c.Author,
		Text:              c.Text,
		CreatedAt:         timestamppb.New(c.CreatedAt),
		OperatedBy:        c.OperatedBy,
		OperatedByDisplay: c.OperatedByDisplay,
		OperatedByEmail:   c.OperatedByEmail,
	}
}

// eventToProto converts a model.Event to a proto Event message.
func eventToProto(e *model.Event) *beadsv1.Event {
	if e == nil {
		return nil
	}
	return &beadsv1.Event{
		Id:         e.ID,
		Topic:      e.Topic,
		BeadId:     e.BeadID,
		Actor:      e.Actor,
		Payload:    []byte(e.Payload),
		CreatedAt:  timestamppb.New(e.CreatedAt),
		OperatedBy: e.OperatedBy,
	}
}

// configToProto converts a model.Config to a proto Config message.
func configToProto(c *model.Config) *beadsv1.Config {
	if c == nil {
		return nil
	}
	return &beadsv1.Config{
		Key:       c.Key,
		Value:     []byte(c.Value),
		CreatedAt: timestamppb.New(c.CreatedAt),
		UpdatedAt: timestamppb.New(c.UpdatedAt),
	}
}

// protoTimestamp converts an optional proto Timestamp to a *time.Time.
// Returns nil when the input is nil.
func protoTimestamp(ts *timestamppb.Timestamp) *time.Time {
	if ts == nil {
		return nil
	}
	t := ts.AsTime()
	return &t
}

// storeError maps store-layer errors to appropriate gRPC status codes.
func storeError(err error, entity string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return status.Errorf(codes.NotFound, "%s not found", entity)
	}
	// Check for a well-known "not found" sentinel, if any.
	var nfErr interface{ NotFound() bool }
	if errors.As(err, &nfErr) && nfErr.NotFound() {
		return status.Errorf(codes.NotFound, "%s not found", entity)
	}
	return status.Errorf(codes.Internal, "failed to get %s: %v", entity, err)
}
