package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/types/known/timestamppb"
	"google.golang.org/protobuf/types/known/wrapperspb"
)

// GRPCClient implements BeadsClient using the gRPC transport.
type GRPCClient struct {
	conn       *grpc.ClientConn
	client     beadsv1.BeadsServiceClient
	fleet      string
	actor      string
	operatedBy string
}

// tokenCredentials implements grpc.PerRPCCredentials to inject a Bearer token.
type tokenCredentials struct {
	token string
}

func (t tokenCredentials) GetRequestMetadata(_ context.Context, _ ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + t.token}, nil
}

func (t tokenCredentials) RequireTransportSecurity() bool { return false }

// NewGRPCClient connects to the given gRPC address and returns a client.
// When token is non-empty, per-RPC credentials are attached.
func NewGRPCClient(addr, token string) (*GRPCClient, error) {
	opts := []grpc.DialOption{grpc.WithTransportCredentials(insecure.NewCredentials())}
	if token != "" {
		opts = append(opts, grpc.WithPerRPCCredentials(tokenCredentials{token: token}))
	}
	conn, err := grpc.NewClient(addr, opts...)
	if err != nil {
		return nil, fmt.Errorf("grpc dial: %w", err)
	}
	return &GRPCClient{
		conn:   conn,
		client: beadsv1.NewBeadsServiceClient(conn),
	}, nil
}

// SetFleet configures the fleet metadata sent with every gRPC call.
func (c *GRPCClient) SetFleet(fleet string) {
	c.fleet = fleet
}

// SetActor configures the x-actor metadata sent with every gRPC call. The
// server honors this only for primary-static-token callers — it is ignored
// for OIDC and fleet-scoped tokens, so sending it from all clients is safe.
func (c *GRPCClient) SetActor(actor string) {
	c.actor = actor
}

// SetOperatedBy configures the x-operated-by metadata sent with every gRPC
// call. Same trust-model as SetActor: server-side, only primary-static-token
// callers have the header honored; OIDC has it ignored.
func (c *GRPCClient) SetOperatedBy(v string) {
	c.operatedBy = v
}

// withFleet appends x-fleet, x-actor, and x-operated-by metadata to the context.
func (c *GRPCClient) withFleet(ctx context.Context) context.Context {
	kv := make([]string, 0, 6)
	if c.fleet != "" {
		kv = append(kv, "x-fleet", c.fleet)
	}
	if c.actor != "" {
		kv = append(kv, "x-actor", c.actor)
	}
	if c.operatedBy != "" {
		kv = append(kv, "x-operated-by", c.operatedBy)
	}
	if NoAutoParentFromContext(ctx) {
		kv = append(kv, "x-no-auto-parent", "1")
	}
	if ForceParentEdgeFromContext(ctx) {
		kv = append(kv, "x-force-parent-edge", "1")
	}
	if len(kv) == 0 {
		return ctx
	}
	return metadata.AppendToOutgoingContext(ctx, kv...)
}

func (c *GRPCClient) Close() error {
	return c.conn.Close()
}

// --- Bead CRUD ---

func (c *GRPCClient) CreateBead(ctx context.Context, req *CreateBeadRequest) (*model.Bead, error) {
	pbReq := &beadsv1.CreateBeadRequest{
		Title:          req.Title,
		Kind:           req.Kind,
		Type:           req.Type,
		Description:    req.Description,
		Notes:          req.Notes,
		Priority:       int32(req.Priority),
		Assignee:       req.Assignee,
		Owner:          req.Owner,
		Labels:         req.Labels,
		CreatedBy:      req.CreatedBy,
		Fields:         req.Fields,
		ParentBeadId:   req.ParentBeadID,
		IdempotencyKey: req.IdempotencyKey,
		ShortTitle:     req.ShortTitle,
		Heraldry:       req.Heraldry,
	}
	if req.DueAt != nil {
		pbReq.DueAt = timestamppb.New(*req.DueAt)
	}
	if req.DeferUntil != nil {
		pbReq.DeferUntil = timestamppb.New(*req.DeferUntil)
	}

	resp, err := c.client.CreateBead(c.withFleet(ctx), pbReq)
	if err != nil {
		return nil, err
	}
	return protoBeadToModel(resp.GetBead()), nil
}

func (c *GRPCClient) GetBead(ctx context.Context, id string) (*model.Bead, error) {
	resp, err := c.client.GetBead(c.withFleet(ctx), &beadsv1.GetBeadRequest{Id: id})
	if err != nil {
		return nil, err
	}
	return protoBeadToModel(resp.GetBead()), nil
}

func (c *GRPCClient) ListBeads(ctx context.Context, req *ListBeadsRequest) (*ListBeadsResponse, error) {
	// ExcludeLabels (kd list --exclude-label) has no field on the proto
	// BeadFilter, so the gRPC transport cannot carry it. Fail loudly rather
	// than silently dropping the filter — a dropped negative-label filter
	// would return a WIDER set than asked for, the exact false-result footgun
	// this flag exists to kill (ka-0000007103). The default kd transport is
	// HTTP, where the filter is honored.
	if len(req.ExcludeLabels) > 0 {
		return nil, fmt.Errorf("--exclude-label is not supported over gRPC transport; use --transport=http (the default)")
	}
	// ExcludeTypes (kd list --exclude-type) has no field on the proto
	// ListBeadsRequest either. A silently dropped negative-type filter would
	// return a WIDER set than asked for — the same false-result footgun the
	// ExcludeLabels guard above kills (ka-0000007843). HTTP (the default)
	// honors it.
	if len(req.ExcludeTypes) > 0 {
		return nil, fmt.Errorf("--exclude-type is not supported over gRPC transport; use --transport=http (the default)")
	}
	// Parent (kd list --parent) has no field on the proto ListBeadsRequest,
	// so the gRPC transport cannot carry the parent-subtree filter. Fail
	// loudly rather than silently dropping it — a dropped parent filter
	// returns the WHOLE project instead of the epic's children, the same
	// false-result footgun the ExcludeLabels guard above exists to kill
	// (ka-0000000208). The default kd transport is HTTP, where it is honored.
	if req.Parent != "" {
		return nil, fmt.Errorf("--parent is not supported over gRPC transport; use --transport=http (the default)")
	}
	// NoParent (kd list --no-parent) has no field on the proto ListBeadsRequest
	// either. Dropping the orphan filter returns EVERY bead (parented ones
	// included) instead of just the parent-less ones — the triage-intake sweep
	// would then process the whole project as "orphans", the same widen-on-drop
	// footgun the guards above kill (ka-0000005267). HTTP (the default) honors it.
	if req.NoParent {
		return nil, fmt.Errorf("--no-parent is not supported over gRPC transport; use --transport=http (the default)")
	}
	// Unassigned (kd list/count --unassigned) has no field on the proto
	// ListBeadsRequest either. Dropping it returns every bead in scope —
	// CLAIMED ones included — instead of just the unclaimed slice, so a
	// work-seeking query would hand back beads another agent is already on: the
	// same widen-on-drop footgun the guards above kill (ka-0000007209). HTTP
	// (the default) honors it.
	if req.Unassigned {
		return nil, fmt.Errorf("--unassigned is not supported over gRPC transport; use --transport=http (the default)")
	}
	// The time-window filters (kd list --created-after / --updated-after /
	// --closed-after and the *-before variants) have no proto BeadFilter
	// fields either. A silently dropped window returns the full unwindowed
	// set — the exact false-result footgun the guards above kill
	// (ka-0000008195). HTTP (the default) honors them.
	if req.CreatedAfter != nil || req.CreatedBefore != nil ||
		req.UpdatedAfter != nil || req.UpdatedBefore != nil ||
		req.ClosedAfter != nil || req.ClosedBefore != nil {
		return nil, fmt.Errorf("time-window filters (--created-after/--updated-after/--closed-after and --*-before) are not supported over gRPC transport; use --transport=http (the default)")
	}
	// --with-blockers has no proto field. Dropping it is worse than dropping a
	// filter: the rows come back looking exactly right but with open_blockers
	// absent on every one, which reads as "nothing here is blocked" — a false
	// all-clear for the triage caller the flag exists to serve (ka-0000000945).
	// Fail loud instead. HTTP (the default) honors it.
	if req.WithOpenBlockers {
		return nil, fmt.Errorf("--with-blockers is not supported over gRPC transport; use --transport=http (the default)")
	}
	// The arms register read (kd heraldry check) has no proto field; a
	// dropped filter would list every bead as bearing arms.
	if req.HeraldryProject != "" {
		return nil, errors.New("reading the arms register is not supported over gRPC transport; use --transport=http (the default)")
	}
	pbReq := &beadsv1.ListBeadsRequest{
		Status:           req.Status,
		Type:             req.Type,
		Kind:             req.Kind,
		Labels:           req.Labels,
		Assignee:         req.Assignee,
		Search:           req.Search,
		Sort:             req.Sort,
		Limit:            int32(req.Limit),
		Offset:           int32(req.Offset),
		ExcludeScheduled: req.ExcludeScheduled,
	}
	pbReq.ShortTitleMissing = req.ShortTitleMissing
	pbReq.Heraldry = req.Heraldry
	// FieldFilters (kd list/count -f key=value) DOES have a proto field
	// (beads.proto field_filters = 11) and the server honors it
	// (internal/server/list_filter.go), but this client never populated it — so
	// every `-f` predicate was silently dropped over gRPC and the response came
	// back UNFILTERED: `kd count -f spawned_by=* --transport=grpc` returned the
	// total bead count, not the matching one. A dropped predicate returns a
	// WIDER set than asked for, the same false-result footgun the guards above
	// fail loudly on. Here the field is carryable, so carry it rather than
	// refuse (ka-0000004030).
	if len(req.FieldFilters) > 0 {
		pbReq.FieldFilters = req.FieldFilters
	}
	if len(req.LabelsAny) > 0 {
		pbReq.LabelsAny = req.LabelsAny
	}
	if req.Priority != nil {
		pbReq.Priority = wrapperspb.Int32(int32(*req.Priority))
	}
	// Typed-mentions filter (ka-0000001544). Mirrors the HTTP client at
	// http.go:178-184 — Mentions on its own is non-empty when used, and
	// MentionsType is a three-state pointer (nil = no filter, &"" = untyped
	// only, &"<verb>" = exact match).
	if req.Mentions != "" {
		m := req.Mentions
		pbReq.Mentions = &m
		if req.MentionsType != nil {
			v := *req.MentionsType
			pbReq.MentionsType = &v
		}
	}
	if req.HasReport != nil {
		v := *req.HasReport
		pbReq.HasReport = &v
	}

	resp, err := c.client.ListBeads(c.withFleet(ctx), pbReq)
	if err != nil {
		return nil, err
	}

	beads := make([]*model.Bead, 0, len(resp.GetBeads()))
	for _, pb := range resp.GetBeads() {
		beads = append(beads, protoBeadToModel(pb))
	}
	return &ListBeadsResponse{
		Beads: beads,
		Total: int(resp.GetTotal()),
	}, nil
}

func (c *GRPCClient) UpdateBead(ctx context.Context, id string, req *UpdateBeadRequest) (*model.Bead, error) {
	pbReq := &beadsv1.UpdateBeadRequest{Id: id}
	if req.Title != nil {
		pbReq.Title = req.Title
	}
	if req.Description != nil {
		pbReq.Description = req.Description
	}
	if req.Notes != nil {
		pbReq.Notes = req.Notes
	}
	if req.Status != nil {
		pbReq.Status = req.Status
	}
	if req.Priority != nil {
		p := int32(*req.Priority)
		pbReq.Priority = &p
	}
	if req.Assignee != nil {
		pbReq.Assignee = req.Assignee
	}
	if req.Owner != nil {
		pbReq.Owner = req.Owner
	}
	if req.DueAt != nil {
		pbReq.DueAt = timestamppb.New(*req.DueAt)
	}
	if req.DeferUntil != nil {
		pbReq.DeferUntil = timestamppb.New(*req.DeferUntil)
	}
	if req.Fields != nil {
		pbReq.Fields = req.Fields
	}
	if len(req.Labels) > 0 {
		pbReq.Labels = req.Labels
	}
	if req.ShortTitle != nil {
		pbReq.ShortTitle = req.ShortTitle
	}
	if req.OnlyIfShortTitleEmpty {
		v := true
		pbReq.OnlyIfShortTitleEmpty = &v
	}
	if req.Heraldry != nil {
		pbReq.Heraldry = req.Heraldry
	}

	resp, err := c.client.UpdateBead(c.withFleet(ctx), pbReq)
	if err != nil {
		return nil, err
	}
	return protoBeadToModel(resp.GetBead()), nil
}

func (c *GRPCClient) CloseBead(ctx context.Context, id string, closedBy string) (*model.Bead, error) {
	resp, err := c.client.CloseBead(c.withFleet(ctx), &beadsv1.CloseBeadRequest{
		Id:       id,
		ClosedBy: closedBy,
	})
	if err != nil {
		return nil, err
	}
	return protoBeadToModel(resp.GetBead()), nil
}

func (c *GRPCClient) DeleteBead(ctx context.Context, id string) error {
	_, err := c.client.DeleteBead(c.withFleet(ctx), &beadsv1.DeleteBeadRequest{Id: id})
	return err
}

// EditBeadSubField is not supported over gRPC — the atomic sub-field edit
// endpoint is HTTP-only (POST /v1/beads/{id}/fields/{field}/edit), mirroring the
// array-field primitives. The default kd transport is HTTP, so this stub only
// trips a caller that explicitly forced --transport=grpc.
func (c *GRPCClient) EditBeadSubField(_ context.Context, _, _ string, _ *EditSubFieldRequest) (*model.Bead, error) {
	return nil, fmt.Errorf("sub-field edit is not supported over gRPC transport; use --transport=http (the default)")
}

func (c *GRPCClient) RestoreBead(_ context.Context, _ string) (*model.Bead, error) {
	return nil, fmt.Errorf("restore is not supported over gRPC (use HTTP client)")
}

// --- Fleet ---

func (c *GRPCClient) TransferBead(_ context.Context, _, _ string) (*model.Bead, error) {
	return nil, fmt.Errorf("fleet transfer is not supported over gRPC (use HTTP client)")
}

// --- Dependencies ---

func (c *GRPCClient) AddDependency(ctx context.Context, req *AddDependencyRequest) (*model.Dependency, error) {
	// withFleet threads x-fleet/x-actor/x-operated-by plus the opt-in headers
	// (x-force-parent-edge for `kd dep add --force`, ka-0000005756) into the
	// outgoing metadata, matching the HTTP transport's header injection.
	resp, err := c.client.AddDependency(c.withFleet(ctx), &beadsv1.AddDependencyRequest{
		BeadId:      req.BeadID,
		DependsOnId: req.DependsOnID,
		Type:        req.Type,
		CreatedBy:   req.CreatedBy,
	})
	if err != nil {
		return nil, err
	}
	return protoDependencyToModel(resp.GetDependency()), nil
}

func (c *GRPCClient) RemoveDependency(ctx context.Context, beadID, dependsOnID, depType string) (bool, error) {
	resp, err := c.client.RemoveDependency(ctx, &beadsv1.RemoveDependencyRequest{
		BeadId:      beadID,
		DependsOnId: dependsOnID,
		Type:        depType,
	})
	if err != nil {
		return false, err
	}
	// An old daemon leaves the optional `removed` field unset. Absent must read
	// as true (assume-success) so a real removal is never reported as a no-op —
	// hence the explicit presence check rather than resp.GetRemoved(), whose
	// zero value would say "nothing matched" (ka-0000001966).
	if resp.Removed == nil {
		return true, nil
	}
	return resp.GetRemoved(), nil
}

func (c *GRPCClient) GetDependencies(ctx context.Context, beadID string) ([]*model.Dependency, error) {
	resp, err := c.client.GetDependencies(ctx, &beadsv1.GetDependenciesRequest{BeadId: beadID})
	if err != nil {
		return nil, err
	}
	deps := make([]*model.Dependency, 0, len(resp.GetDependencies()))
	for _, pb := range resp.GetDependencies() {
		deps = append(deps, protoDependencyToModel(pb))
	}
	return deps, nil
}

func (c *GRPCClient) GetReverseDependencies(_ context.Context, _ string) ([]*model.Dependency, error) {
	return nil, fmt.Errorf("GetReverseDependencies is not supported over gRPC transport; use --transport=http")
}

// --- References ---

// ListBeadsMentioning forwards to the gRPC ListMentionedBy RPC. The
// relationshipType pointer threads through to the proto's optional
// relationship_type field with three-state semantics (ka-0000001544):
// nil = no filter, &"" = untyped only, &"<verb>" = exact match.
func (c *GRPCClient) ListBeadsMentioning(ctx context.Context, beadID string, limit, offset int, relationshipType *string) (*ListReferencesResponse, error) {
	pbReq := &beadsv1.ListMentionedByRequest{
		BeadId: beadID,
		Limit:  int32(limit),
		Offset: int32(offset),
	}
	if relationshipType != nil {
		v := *relationshipType
		pbReq.RelationshipType = &v
	}
	resp, err := c.client.ListMentionedBy(ctx, pbReq)
	if err != nil {
		return nil, err
	}
	return &ListReferencesResponse{
		References: protoReferencesToModel(resp.GetReferences()),
		Total:      int(resp.GetTotal()),
		Limit:      int(resp.GetLimit()),
		Offset:     int(resp.GetOffset()),
	}, nil
}

// ListBeadReferencesFrom forwards to the gRPC ListMentions RPC. See
// ListBeadsMentioning for the relationshipType three-state semantics.
func (c *GRPCClient) ListBeadReferencesFrom(ctx context.Context, beadID string, limit, offset int, relationshipType *string) (*ListReferencesResponse, error) {
	pbReq := &beadsv1.ListMentionsRequest{
		BeadId: beadID,
		Limit:  int32(limit),
		Offset: int32(offset),
	}
	if relationshipType != nil {
		v := *relationshipType
		pbReq.RelationshipType = &v
	}
	resp, err := c.client.ListMentions(ctx, pbReq)
	if err != nil {
		return nil, err
	}
	return &ListReferencesResponse{
		References: protoReferencesToModel(resp.GetReferences()),
		Total:      int(resp.GetTotal()),
		Limit:      int(resp.GetLimit()),
		Offset:     int(resp.GetOffset()),
	}, nil
}

func protoReferencesToModel(rows []*beadsv1.BeadReferenceSummary) []*model.BeadReferenceSummary {
	out := make([]*model.BeadReferenceSummary, 0, len(rows))
	for _, pb := range rows {
		if pb == nil {
			continue
		}
		out = append(out, &model.BeadReferenceSummary{
			ID:          pb.GetId(),
			Title:       pb.GetTitle(),
			Type:        pb.GetType(),
			Status:      pb.GetStatus(),
			SourceField: pb.GetSourceField(),
		})
	}
	return out
}

// --- Labels ---

func (c *GRPCClient) AddLabel(ctx context.Context, beadID, label string) (*model.Bead, error) {
	resp, err := c.client.AddLabel(ctx, &beadsv1.AddLabelRequest{
		BeadId: beadID,
		Label:  label,
	})
	if err != nil {
		return nil, err
	}
	return protoBeadToModel(resp.GetBead()), nil
}

func (c *GRPCClient) RemoveLabel(ctx context.Context, beadID, label string) (bool, error) {
	resp, err := c.client.RemoveLabel(ctx, &beadsv1.RemoveLabelRequest{
		BeadId: beadID,
		Label:  label,
	})
	if err != nil {
		return false, err
	}
	// An old daemon leaves the optional `removed` field unset. Absent must read
	// as true (assume-success) so a real removal is never reported as a no-op —
	// hence the explicit presence check rather than resp.GetRemoved(), whose
	// zero value would say "nothing matched" (ka-0000000424).
	if resp.Removed == nil {
		return true, nil
	}
	return resp.GetRemoved(), nil
}

func (c *GRPCClient) GetLabels(ctx context.Context, beadID string) ([]string, error) {
	resp, err := c.client.GetLabels(ctx, &beadsv1.GetLabelsRequest{BeadId: beadID})
	if err != nil {
		return nil, err
	}
	return resp.GetLabels(), nil
}

// --- Comments ---

func (c *GRPCClient) AddComment(ctx context.Context, beadID, author, text string) (*model.Comment, error) {
	resp, err := c.client.AddComment(ctx, &beadsv1.AddCommentRequest{
		BeadId: beadID,
		Author: author,
		Text:   text,
	})
	if err != nil {
		return nil, err
	}
	return protoCommentToModel(resp.GetComment()), nil
}

func (c *GRPCClient) GetComments(ctx context.Context, beadID string) ([]*model.Comment, error) {
	resp, err := c.client.GetComments(ctx, &beadsv1.GetCommentsRequest{BeadId: beadID})
	if err != nil {
		return nil, err
	}
	comments := make([]*model.Comment, 0, len(resp.GetComments()))
	for _, pb := range resp.GetComments() {
		comments = append(comments, protoCommentToModel(pb))
	}
	return comments, nil
}

func (c *GRPCClient) DeleteComment(ctx context.Context, beadID string, commentID int64) (bool, error) {
	resp, err := c.client.DeleteComment(ctx, &beadsv1.DeleteCommentRequest{
		BeadId:    beadID,
		CommentId: commentID,
	})
	if err != nil {
		return false, err
	}
	return resp.GetDeleted(), nil
}

func (c *GRPCClient) TrimComments(ctx context.Context, beadID string, keepLast int, authorFilter string) ([]int64, error) {
	resp, err := c.client.TrimComments(ctx, &beadsv1.TrimCommentsRequest{
		BeadId:   beadID,
		KeepLast: int32(keepLast),
		Author:   authorFilter,
	})
	if err != nil {
		return nil, err
	}
	return resp.GetDeletedIds(), nil
}

// --- Events ---

func (c *GRPCClient) GetEvents(ctx context.Context, beadID string) ([]*model.Event, error) {
	resp, err := c.client.GetEvents(ctx, &beadsv1.GetEventsRequest{BeadId: beadID})
	if err != nil {
		return nil, err
	}
	events := make([]*model.Event, 0, len(resp.GetEvents()))
	for _, pb := range resp.GetEvents() {
		events = append(events, protoEventToModel(pb))
	}
	return events, nil
}

// ListGlobalEvents is not supported over gRPC — the global events feed is
// HTTP-only (GET /v1/events), mirroring GetMutations below. The default kd
// transport is HTTP, and `kd events backfill` requires HTTP, so practical
// callers are unaffected. Adding gRPC support requires a proto-level change.
func (c *GRPCClient) ListGlobalEvents(_ context.Context, _ *ListGlobalEventsRequest) (*ListGlobalEventsResponse, error) {
	return nil, errors.New("ListGlobalEvents: gRPC transport not supported; use HTTP")
}

// --- Mutation history ---

// GetMutations is not supported over gRPC in this MR — mutation history is
// HTTP-only. The default kd transport is HTTP, so practical callers are
// unaffected. Adding gRPC support requires a proto-level change scheduled
// for a follow-up MR.
func (c *GRPCClient) GetMutations(_ context.Context, _ string) ([]*model.MutationRow, error) {
	return nil, errors.New("GetMutations: gRPC transport not supported; use HTTP")
}

// UndoMutation is not supported over gRPC in this MR — mutation history is
// HTTP-only. See GetMutations above for context.
func (c *GRPCClient) UndoMutation(_ context.Context, _ int64) (*UndoMutationResponse, error) {
	return nil, errors.New("UndoMutation: gRPC transport not supported; use HTTP")
}

// --- Taints (docs/adr/taints.md §7) ---
//
// Taints get no gRPC RPCs at all (ADR §13 OQ2); the default kd transport is
// HTTP, so practical callers are unaffected.

func (c *GRPCClient) GrantTaint(_ context.Context, _ string, _ *GrantTaintRequest) (*model.TaintGrant, error) {
	return nil, errors.New("GrantTaint: gRPC transport not supported; use HTTP")
}

func (c *GRPCClient) RevokeTaint(_ context.Context, _, _ string) (bool, error) {
	return false, errors.New("RevokeTaint: gRPC transport not supported; use HTTP")
}

func (c *GRPCClient) ApplyTaint(_ context.Context, _, _ string) (*model.BeadTaint, error) {
	return nil, errors.New("ApplyTaint: gRPC transport not supported; use HTTP")
}

func (c *GRPCClient) GetBeadTaints(_ context.Context, _ string) ([]*model.BeadTaint, error) {
	return nil, errors.New("GetBeadTaints: gRPC transport not supported; use HTTP")
}

func (c *GRPCClient) RequestTaintGrant(_ context.Context, _ string, _ *RequestTaintGrantRequest) (*EscalationCreated, error) {
	return nil, errors.New("RequestTaintGrant: gRPC transport not supported; use HTTP")
}

// --- Config ---

func (c *GRPCClient) SetConfig(ctx context.Context, key string, value json.RawMessage) (*model.Config, error) {
	resp, err := c.client.SetConfig(ctx, &beadsv1.SetConfigRequest{
		Key:   key,
		Value: value,
	})
	if err != nil {
		return nil, err
	}
	return protoConfigToModel(resp.GetConfig()), nil
}

func (c *GRPCClient) GetConfig(ctx context.Context, key string) (*model.Config, error) {
	resp, err := c.client.GetConfig(ctx, &beadsv1.GetConfigRequest{Key: key})
	if err != nil {
		return nil, err
	}
	return protoConfigToModel(resp.GetConfig()), nil
}

func (c *GRPCClient) ListConfigs(ctx context.Context, namespace string) ([]*model.Config, error) {
	resp, err := c.client.ListConfigs(ctx, &beadsv1.ListConfigsRequest{Namespace: namespace})
	if err != nil {
		return nil, err
	}
	configs := make([]*model.Config, 0, len(resp.GetConfigs()))
	for _, pb := range resp.GetConfigs() {
		configs = append(configs, protoConfigToModel(pb))
	}
	return configs, nil
}

func (c *GRPCClient) DeleteConfig(ctx context.Context, key string) error {
	_, err := c.client.DeleteConfig(ctx, &beadsv1.DeleteConfigRequest{Key: key})
	return err
}

// --- Hooks ---

// EmitHook is not implemented over gRPC (no proto definition exists).
// Returns an error directing callers to use the HTTP transport.
func (c *GRPCClient) EmitHook(_ context.Context, _ *EmitHookRequest) (*EmitHookResponse, error) {
	return nil, fmt.Errorf("EmitHook is not supported over gRPC transport; use --transport=http")
}

// --- Roster ---

func (c *GRPCClient) GetAgentRoster(_ context.Context, _ int) (*AgentRosterResponse, error) {
	return nil, fmt.Errorf("GetAgentRoster is not supported over gRPC transport; use --transport=http")
}

// --- Operators ---

func (c *GRPCClient) LookupOperatorByAlias(_ context.Context, _, _ string) (*LookupOperatorByAliasResponse, error) {
	return nil, fmt.Errorf("LookupOperatorByAlias is not supported over gRPC transport; use --transport=http (the default)")
}

// --- Gates ---

func (c *GRPCClient) ListGates(_ context.Context, _ string) ([]model.GateRow, error) {
	return nil, fmt.Errorf("not supported over gRPC")
}

func (c *GRPCClient) SatisfyGate(_ context.Context, _, _ string) error {
	return fmt.Errorf("not supported over gRPC")
}

func (c *GRPCClient) ClearGate(_ context.Context, _, _ string) error {
	return fmt.Errorf("not supported over gRPC")
}

// --- Batch ---

// BatchGetBeads fetches multiple beads by ID. The gRPC transport implements
// this as concurrent individual GetBead calls since there is no batch RPC.
func (c *GRPCClient) BatchGetBeads(ctx context.Context, ids []string) ([]BatchGetResult, error) {
	results := make([]BatchGetResult, len(ids))
	var wg sync.WaitGroup
	for i, id := range ids {
		results[i].ID = id
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			bead, err := c.GetBead(ctx, id)
			if err != nil {
				results[i].Error = err.Error()
			} else {
				results[i].Bead = bead
			}
		}(i, id)
	}
	wg.Wait()
	return results, nil
}

// --- Semantic search ---

// FindSimilar is not implemented on the gRPC client yet. The semantic-search
// RPC lives in a separate task (ka-0000005236); until that lands, callers
// should use the HTTP client for semantic queries.
func (c *GRPCClient) FindSimilar(_ context.Context, _ string, _ int, _ float64) (*FindSimilarResponse, error) {
	return nil, fmt.Errorf("FindSimilar: not implemented on gRPC client; use HTTP")
}

// SemanticSearch is not implemented on the gRPC client yet. The semantic-search
// gRPC RPC lives in a separate task; until that lands, callers should use the
// HTTP client for semantic queries.
func (c *GRPCClient) SemanticSearch(_ context.Context, _ *SemanticSearchRequest) (*SemanticSearchResponse, error) {
	return nil, fmt.Errorf("SemanticSearch: not implemented on gRPC client; use HTTP")
}

// --- Escalations ---
//
// The escalation surface (GET/POST /v1/escalations/…) has no gRPC counterpart —
// there is no escalation service in the proto. All five verbs are HTTP-only;
// the gRPC client returns an unsupported error so `kd escalation` under
// --transport=grpc fails loud rather than silently no-op'ing (ka-0000007156).

func (c *GRPCClient) GetEscalation(_ context.Context, _ string) (*EscalationDetail, error) {
	return nil, fmt.Errorf("escalations are not supported over gRPC transport; use --transport=http (the default)")
}

func (c *GRPCClient) ListEscalations(_ context.Context, _ ListEscalationsOptions) ([]EscalationDetail, error) {
	return nil, fmt.Errorf("escalations are not supported over gRPC transport; use --transport=http (the default)")
}

func (c *GRPCClient) ApproveEscalation(_ context.Context, _ string, _ ApproveEscalationRequest) error {
	return fmt.Errorf("escalations are not supported over gRPC transport; use --transport=http (the default)")
}

func (c *GRPCClient) RejectEscalation(_ context.Context, _ string, _ RejectEscalationRequest) error {
	return fmt.Errorf("escalations are not supported over gRPC transport; use --transport=http (the default)")
}

func (c *GRPCClient) ClaimEscalation(_ context.Context, _ string) (*model.Bead, error) {
	return nil, fmt.Errorf("escalations are not supported over gRPC transport; use --transport=http (the default)")
}

func (c *GRPCClient) HandUpEscalation(_ context.Context, _ string, _ HandUpEscalationRequest) (*model.Bead, error) {
	return nil, fmt.Errorf("escalations are not supported over gRPC transport; use --transport=http (the default)")
}

func (c *GRPCClient) CaptureEscalationResult(_ context.Context, _ string, _ CaptureEscalationResultRequest) error {
	return fmt.Errorf("escalations are not supported over gRPC transport; use --transport=http (the default)")
}

func (c *GRPCClient) ApproveScrubRequest(_ context.Context, _ string, _ ApproveScrubRequestRequest) (*model.ScrubAuditArtifact, error) {
	return nil, fmt.Errorf("scrub-requests are not supported over gRPC transport; use --transport=http (the default)")
}

func (c *GRPCClient) RejectScrubRequest(_ context.Context, _ string, _ RejectScrubRequestRequest) error {
	return fmt.Errorf("scrub-requests are not supported over gRPC transport; use --transport=http (the default)")
}

// --- Taints ---
//
// The taints surface (GET/POST /v1/taints…) has no gRPC counterpart — there
// is no taint service in the proto (ADR docs/adr/taints.md §7). All three
// verbs are HTTP-only; the gRPC client returns an unsupported error so
// `kd taint` under --transport=grpc fails loud rather than silently
// no-op'ing.

func (c *GRPCClient) CreateTaint(_ context.Context, _ *CreateTaintRequest) (*TaintView, error) {
	return nil, fmt.Errorf("taints are not supported over gRPC transport; use --transport=http (the default)")
}

func (c *GRPCClient) ListTaints(_ context.Context) (*ListTaintsResponse, error) {
	return nil, fmt.Errorf("taints are not supported over gRPC transport; use --transport=http (the default)")
}

func (c *GRPCClient) GetTaint(_ context.Context, _ string) (*TaintView, error) {
	return nil, fmt.Errorf("taints are not supported over gRPC transport; use --transport=http (the default)")
}

func (c *GRPCClient) CreateScrubRequest(_ context.Context, _ *CreateScrubRequestRequest) (*ScrubRequestView, error) {
	return nil, fmt.Errorf("scrub-requests are not supported over gRPC transport; use --transport=http (the default)")
}

// UpsertEmbedding is not implemented on the gRPC client — `kd embed sync`
// runs over HTTP.
func (c *GRPCClient) UpsertEmbedding(_ context.Context, _ *UpsertEmbeddingRequest) error {
	return fmt.Errorf("UpsertEmbedding: not implemented on gRPC client; use HTTP")
}

// GetEmbeddingHashes is not implemented on the gRPC client — `kd embed sync`
// runs over HTTP.
func (c *GRPCClient) GetEmbeddingHashes(_ context.Context, _ []string) (map[string]string, error) {
	return nil, fmt.Errorf("GetEmbeddingHashes: not implemented on gRPC client; use HTTP")
}

// SyncBatchEmbeddings is not implemented on the gRPC client — `kd embed sync`
// runs over HTTP.
func (c *GRPCClient) SyncBatchEmbeddings(_ context.Context, _ *SyncBatchEmbeddingsRequest) (*SyncBatchEmbeddingsResponse, error) {
	return nil, fmt.Errorf("SyncBatchEmbeddings: not implemented on gRPC client; use HTTP")
}

// MigrateEmbeddings is not implemented on the gRPC client — `kd embed migrate`
// runs over HTTP.
func (c *GRPCClient) MigrateEmbeddings(_ context.Context, _ *MigrateEmbeddingsRequest) error {
	return fmt.Errorf("MigrateEmbeddings: not implemented on gRPC client; use HTTP")
}

// ReindexEmbeddings is not implemented on the gRPC client — `kd embed migrate`
// runs over HTTP.
func (c *GRPCClient) ReindexEmbeddings(_ context.Context) error {
	return fmt.Errorf("ReindexEmbeddings: not implemented on gRPC client; use HTTP")
}

// ReextractReferences is not implemented on the gRPC client —
// `kd admin reextract-references` runs over HTTP.
func (c *GRPCClient) ReextractReferences(_ context.Context, _ *ReextractReferencesRequest) (*ReextractReferencesResponse, error) {
	return nil, fmt.Errorf("ReextractReferences: not implemented on gRPC client; use HTTP")
}

// BackfillSearchTSV is not implemented on the gRPC client —
// `kd admin backfill-search-tsv` runs over HTTP.
func (c *GRPCClient) BackfillSearchTSV(_ context.Context, _ *BackfillSearchTSVRequest) (*BackfillSearchTSVResponse, error) {
	return nil, fmt.Errorf("BackfillSearchTSV: not implemented on gRPC client; use HTTP")
}

// SetStewardDelegation is not implemented on the gRPC client —
// `kd admin backfill-land-steward-approves` runs over HTTP.
func (c *GRPCClient) SetStewardDelegation(_ context.Context, _ string, _ []string) error {
	return fmt.Errorf("SetStewardDelegation: not implemented on gRPC client; use HTTP")
}

// BackfillCategory is not implemented on the gRPC client —
// `kd admin backfill-category` runs over HTTP.
func (c *GRPCClient) BackfillCategory(_ context.Context, _ *BackfillCategoryRequest) (*BackfillCategoryResponse, error) {
	return nil, fmt.Errorf("BackfillCategory: not implemented on gRPC client; use HTTP")
}

// BackfillProjectLabels is not implemented on the gRPC client —
// `kd admin backfill-project-labels` runs over HTTP.
func (c *GRPCClient) BackfillProjectLabels(_ context.Context, _ *BackfillProjectLabelsRequest) (*BackfillProjectLabelsResponse, error) {
	return nil, fmt.Errorf("BackfillProjectLabels: not implemented on gRPC client; use HTTP")
}

// BackfillOperators is not implemented on the gRPC client —
// `kd admin backfill-operators` runs over HTTP.
func (c *GRPCClient) BackfillOperators(_ context.Context, _ *BackfillOperatorsRequest) (*BackfillOperatorsResponse, error) {
	return nil, fmt.Errorf("BackfillOperators: not implemented on gRPC client; use HTTP")
}

// BackfillOperatedByLegacy is not implemented on the gRPC client —
// `kd admin backfill-operated-by-legacy` runs over HTTP.
func (c *GRPCClient) BackfillOperatedByLegacy(_ context.Context, _ *BackfillOperatedByLegacyRequest) (*BackfillOperatedByLegacyResponse, error) {
	return nil, fmt.Errorf("BackfillOperatedByLegacy: not implemented on gRPC client; use HTTP")
}

// BackfillSlackUserID is not implemented on the gRPC client —
// `kd admin backfill-slack-user-id` runs over HTTP.
func (c *GRPCClient) BackfillSlackUserID(_ context.Context, _ *BackfillSlackUserIDRequest) (*BackfillSlackUserIDResponse, error) {
	return nil, fmt.Errorf("BackfillSlackUserID: not implemented on gRPC client; use HTTP")
}

// BackfillOperatorSlugs is not implemented on the gRPC client —
// `kd admin backfill-operator-slugs` runs over HTTP.
func (c *GRPCClient) BackfillOperatorSlugs(_ context.Context, _ *BackfillOperatorSlugsRequest) (*BackfillOperatorSlugsResponse, error) {
	return nil, fmt.Errorf("BackfillOperatorSlugs: not implemented on gRPC client; use HTTP")
}

// ForgetOperator is not implemented on the gRPC client —
// `kd admin forget-operator` runs over HTTP.
func (c *GRPCClient) ForgetOperator(_ context.Context, _ int64, _ *ForgetOperatorRequest) (*ForgetOperatorResponse, error) {
	return nil, fmt.Errorf("ForgetOperator: not implemented on gRPC client; use HTTP")
}

// BackfillOwner is not implemented on the gRPC client —
// `kd admin backfill-owner` runs over HTTP.
func (c *GRPCClient) BackfillOwner(_ context.Context, _ *BackfillOwnerRequest) (*BackfillOwnerResponse, error) {
	return nil, fmt.Errorf("BackfillOwner: not implemented on gRPC client; use HTTP")
}

// BackfillEmptyOwner is not implemented on the gRPC client —
// `kd admin backfill-empty-owner` runs over HTTP.
func (c *GRPCClient) BackfillEmptyOwner(_ context.Context, _ *BackfillEmptyOwnerRequest) (*BackfillEmptyOwnerResponse, error) {
	return nil, fmt.Errorf("BackfillEmptyOwner: not implemented on gRPC client; use HTTP")
}

// BackfillOperatedByAgents is not implemented on the gRPC client —
// `kd admin backfill-operated-by-agents` runs over HTTP.
func (c *GRPCClient) BackfillOperatedByAgents(_ context.Context, _ *BackfillOperatedByAgentsRequest) (*BackfillOperatedByAgentsResponse, error) {
	return nil, fmt.Errorf("BackfillOperatedByAgents: not implemented on gRPC client; use HTTP")
}

// BackfillParentChildDeps is not implemented on the gRPC client —
// `kd admin backfill-parent-child-deps` runs over HTTP.
func (c *GRPCClient) BackfillParentChildDeps(_ context.Context, _ *BackfillParentChildDepsRequest) (*BackfillParentChildDepsResponse, error) {
	return nil, fmt.Errorf("BackfillParentChildDeps: not implemented on gRPC client; use HTTP")
}

// RepairReversedParentChildDeps is not implemented on the gRPC client —
// `kd admin repair-reversed-parent-child-deps` runs over HTTP.
func (c *GRPCClient) RepairReversedParentChildDeps(_ context.Context, _ *RepairReversedParentChildDepsRequest) (*RepairReversedParentChildDepsResponse, error) {
	return nil, fmt.Errorf("RepairReversedParentChildDeps: not implemented on gRPC client; use HTTP")
}

// BackfillTriggeredByDeps is not implemented on the gRPC client —
// `kd admin backfill-triggered-by-deps` runs over HTTP.
func (c *GRPCClient) BackfillTriggeredByDeps(_ context.Context, _ *BackfillTriggeredByDepsRequest) (*BackfillTriggeredByDepsResponse, error) {
	return nil, fmt.Errorf("BackfillTriggeredByDeps: not implemented on gRPC client; use HTTP")
}

// AuditBlocksCycles is not implemented on the gRPC client —
// `kd admin audit-blocks-cycles` runs over HTTP.
func (c *GRPCClient) AuditBlocksCycles(_ context.Context) (*AuditBlocksCyclesResponse, error) {
	return nil, fmt.Errorf("AuditBlocksCycles: not implemented on gRPC client; use HTTP")
}

// CloseTriggerAuditBeads is not implemented on the gRPC client —
// `kd admin close-trigger-audit-beads` runs over HTTP.
func (c *GRPCClient) CloseTriggerAuditBeads(_ context.Context, _ *CloseTriggerAuditBeadsRequest) (*CloseTriggerAuditBeadsResponse, error) {
	return nil, fmt.Errorf("CloseTriggerAuditBeads: not implemented on gRPC client; use HTTP")
}

// --- Health ---

func (c *GRPCClient) Health(ctx context.Context) (string, error) {
	resp, err := c.client.Health(ctx, &beadsv1.HealthRequest{})
	if err != nil {
		return "", err
	}
	return resp.GetStatus(), nil
}

// --- proto to model conversions ---

func protoBeadToModel(pb *beadsv1.Bead) *model.Bead {
	if pb == nil {
		return nil
	}
	b := &model.Bead{
		ID:          pb.GetId(),
		Slug:        pb.GetSlug(),
		Kind:        model.Kind(pb.GetKind()),
		Type:        model.BeadType(pb.GetType()),
		Title:       pb.GetTitle(),
		ShortTitle:  pb.GetShortTitle(),
		Heraldry:    pb.GetHeraldry(),
		Description: pb.GetDescription(),
		Notes:       pb.GetNotes(),
		Status:      model.Status(pb.GetStatus()),
		Priority:    int(pb.GetPriority()),
		Assignee:    pb.GetAssignee(),
		Owner:       pb.GetOwner(),
		CreatedBy:   pb.GetCreatedBy(),
		Labels:      pb.GetLabels(),
		// RelevanceScore rides the ListBeads search projection (ka-0000003832).
		// 0 when no text search is active.
		RelevanceScore: pb.GetRelevanceScore(),
	}
	if pb.GetCreatedAt() != nil {
		b.CreatedAt = pb.GetCreatedAt().AsTime()
	}
	if pb.GetUpdatedAt() != nil {
		b.UpdatedAt = pb.GetUpdatedAt().AsTime()
	}
	if pb.GetClosedAt() != nil {
		t := pb.GetClosedAt().AsTime()
		b.ClosedAt = &t
	}
	if pb.GetDueAt() != nil {
		t := pb.GetDueAt().AsTime()
		b.DueAt = &t
	}
	if pb.GetDeferUntil() != nil {
		t := pb.GetDeferUntil().AsTime()
		b.DeferUntil = &t
	}
	if len(pb.GetFields()) > 0 {
		b.Fields = json.RawMessage(pb.GetFields())
	}

	for _, d := range pb.GetDependencies() {
		b.Dependencies = append(b.Dependencies, protoDependencyToModel(d))
	}
	for _, c := range pb.GetComments() {
		b.Comments = append(b.Comments, protoCommentToModel(c))
	}

	return b
}

func protoDependencyToModel(pb *beadsv1.Dependency) *model.Dependency {
	if pb == nil {
		return nil
	}
	d := &model.Dependency{
		BeadID:      pb.GetBeadId(),
		DependsOnID: pb.GetDependsOnId(),
		Type:        model.DependencyType(pb.GetType()),
		CreatedBy:   pb.GetCreatedBy(),
	}
	if pb.GetCreatedAt() != nil {
		d.CreatedAt = pb.GetCreatedAt().AsTime()
	}
	return d
}

func protoCommentToModel(pb *beadsv1.Comment) *model.Comment {
	if pb == nil {
		return nil
	}
	c := &model.Comment{
		ID:     int64(pb.GetId()),
		BeadID: pb.GetBeadId(),
		Author: pb.GetAuthor(),
		Text:   pb.GetText(),
	}
	if pb.GetCreatedAt() != nil {
		c.CreatedAt = pb.GetCreatedAt().AsTime()
	}
	return c
}

func protoEventToModel(pb *beadsv1.Event) *model.Event {
	if pb == nil {
		return nil
	}
	e := &model.Event{
		ID:      int64(pb.GetId()),
		Topic:   pb.GetTopic(),
		BeadID:  pb.GetBeadId(),
		Actor:   pb.GetActor(),
		Payload: json.RawMessage(pb.GetPayload()),
	}
	if pb.GetCreatedAt() != nil {
		e.CreatedAt = pb.GetCreatedAt().AsTime()
	}
	return e
}

func protoConfigToModel(pb *beadsv1.Config) *model.Config {
	if pb == nil {
		return nil
	}
	c := &model.Config{
		Key:   pb.GetKey(),
		Value: json.RawMessage(pb.GetValue()),
	}
	if pb.GetCreatedAt() != nil {
		c.CreatedAt = pb.GetCreatedAt().AsTime()
	}
	if pb.GetUpdatedAt() != nil {
		c.UpdatedAt = pb.GetUpdatedAt().AsTime()
	}
	return c
}

// Releases are HTTP-only (no release service in the proto).

func (c *GRPCClient) CreateRelease(_ context.Context, _ CreateReleaseRequest) (*Release, error) {
	return nil, fmt.Errorf("releases are not supported over gRPC transport; use --transport=http (the default)")
}

func (c *GRPCClient) GetRelease(_ context.Context, _, _ string) (*Release, error) {
	return nil, fmt.Errorf("releases are not supported over gRPC transport; use --transport=http (the default)")
}

func (c *GRPCClient) ListReleases(_ context.Context, _, _ string, _ int) ([]Release, error) {
	return nil, fmt.Errorf("releases are not supported over gRPC transport; use --transport=http (the default)")
}

func (c *GRPCClient) DeployRelease(_ context.Context, _, _ string, _ DeployReleaseRequest) (*Release, error) {
	return nil, fmt.Errorf("releases are not supported over gRPC transport; use --transport=http (the default)")
}

func (c *GRPCClient) RepairReleaseDeployments(_ context.Context, _ string, _ bool) (*RepairDeploymentsResult, error) {
	return nil, fmt.Errorf("releases are not supported over gRPC transport; use --transport=http (the default)")
}
