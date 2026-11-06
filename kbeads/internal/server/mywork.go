package server

import (
	"context"
	"time"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MyWorkSummary aggregates the operator-centric "My Work" queues in a single
// fan-out so the viewer doesn't have to issue 4-5 List calls per refresh.
// Plan-of-record: beads3d/docs/my-work-operator-tab.md §Phase 3.

const (
	defaultStaleAgentMinutes = 30
	defaultMyWorkLimit       = 50
	maxMyWorkLimit           = 200
	recentClosedWindow       = 24 * time.Hour
)

// myWorkSummary is the transport-agnostic shape returned by the aggregator,
// shared between the gRPC handler and the HTTP / Connect-RPC handler.
type myWorkSummary struct {
	OperatorID            string
	StaleMinutes          int32
	DecisionsPending      []*model.Bead
	DecisionsPendingCount int
	MailUnread            []*model.Bead
	MailUnreadCount       int
	AssignedOpen          []*model.Bead
	AssignedOpenCount     int
	StaleAgents           []*model.Bead
	StaleAgentsCount      int
	RecentClosed          []*model.Bead
	RecentClosedCount     int
}

// computeMyWorkSummary issues the per-queue store queries and stitches the
// result. Cross-project read scope is NOT applied by the store layer — it is
// entirely filter-driven from the server (see read_authz_scope.go / the
// ka-0000008132 sweep). Every hand-built filter below is therefore routed
// through s.scopeListFilter so a project-bound fleet token or a restricted
// OIDC user cannot read out-of-scope bead content through this aggregator. The
// DecisionsPending queue is the sharpest edge: it carries no operator predicate
// (everyone's pending decisions), so without scoping it returns full decision
// beads from every project to any RoleViewer. A static/primary token and an
// unrestricted OIDC user are left untouched by scopeListFilter, preserving
// their existing fleet-wide view.
func (s *BeadsServer) computeMyWorkSummary(ctx context.Context, operatorID string, staleMinutes int32, limit int32) (*myWorkSummary, error) {
	if staleMinutes <= 0 {
		staleMinutes = defaultStaleAgentMinutes
	}
	cap := int(limit)
	if cap <= 0 {
		cap = defaultMyWorkLimit
	}
	if cap > maxMyWorkLimit {
		cap = maxMyWorkLimit
	}

	out := &myWorkSummary{
		OperatorID:   operatorID,
		StaleMinutes: staleMinutes,
	}

	// 1. Decisions waiting on the operator (everyone's pending decisions).
	decisions, decTotal, err := s.store.ListBeads(ctx, s.scopeListFilter(ctx, model.BeadFilter{
		Type:   []model.BeadType{model.TypeDecision},
		Status: []model.Status{model.StatusOpen, model.StatusInProgress},
		Sort:   "-created_at",
		Limit:  cap,
	}))
	if err != nil {
		return nil, err
	}
	out.DecisionsPending = decisions
	out.DecisionsPendingCount = decTotal

	// Without an operator id the remaining queues are not meaningful.
	if operatorID == "" {
		return s.propagateMyWorkReadTaint(ctx, out)
	}

	// 2. Mail to the operator (type=message, assignee=op, not closed).
	mail, mailTotal, err := s.store.ListBeads(ctx, s.scopeListFilter(ctx, model.BeadFilter{
		Type:     []model.BeadType{"message"},
		Status:   []model.Status{model.StatusOpen, model.StatusInProgress},
		Assignee: operatorID,
		Sort:     "-created_at",
		Limit:    cap,
	}))
	if err != nil {
		return nil, err
	}
	out.MailUnread = mail
	out.MailUnreadCount = mailTotal

	// 3. Crew agents the operator spawned (type=agent, created_by=op).
	crew, _, err := s.store.ListBeads(ctx, s.scopeListFilter(ctx, model.BeadFilter{
		Type:      []model.BeadType{"agent"},
		Status:    []model.Status{model.StatusOpen, model.StatusInProgress},
		CreatedBy: operatorID,
		Sort:      "-updated_at",
		Limit:     cap,
	}))
	if err != nil {
		return nil, err
	}
	threshold := time.Now().UTC().Add(-time.Duration(staleMinutes) * time.Minute)
	stale := make([]*model.Bead, 0, len(crew))
	for _, a := range crew {
		if a == nil || a.UpdatedAt.IsZero() {
			continue
		}
		if a.UpdatedAt.Before(threshold) {
			stale = append(stale, a)
		}
	}
	out.StaleAgents = stale
	out.StaleAgentsCount = len(stale)

	// 4. Beads assigned to the operator that are still open.
	assigned, assignedTotal, err := s.store.ListBeads(ctx, s.scopeListFilter(ctx, model.BeadFilter{
		Status:       []model.Status{model.StatusOpen, model.StatusInProgress, model.StatusBlocked},
		Assignee:     operatorID,
		ExcludeTypes: []model.BeadType{"agent", "message", model.TypeDecision},
		Sort:         "-updated_at",
		Limit:        cap,
	}))
	if err != nil {
		return nil, err
	}
	out.AssignedOpen = assigned
	out.AssignedOpenCount = assignedTotal

	// 5. Recently closed in the operator's lane (last 24h).
	since := time.Now().UTC().Add(-recentClosedWindow)
	recent, recentTotal, err := s.store.ListBeads(ctx, s.scopeListFilter(ctx, model.BeadFilter{
		Status:       []model.Status{model.StatusClosed},
		Assignee:     operatorID,
		ExcludeTypes: []model.BeadType{"agent", "message"},
		UpdatedAfter: &since,
		Sort:         "-updated_at",
		Limit:        cap,
	}))
	if err != nil {
		return nil, err
	}
	out.RecentClosed = recent
	out.RecentClosedCount = recentTotal

	return s.propagateMyWorkReadTaint(ctx, out)
}

// propagateMyWorkReadTaint is inherit:read for every bead in the summary
// (gd-WdnPW8TLPq): unread mail and pending decisions are other principals'
// text. Every return of a filled summary goes through it, the
// no-operator-id one included.
func (s *BeadsServer) propagateMyWorkReadTaint(ctx context.Context, out *myWorkSummary) (*myWorkSummary, error) {
	var read []string
	for _, q := range [][]*model.Bead{out.DecisionsPending, out.MailUnread, out.AssignedOpen, out.StaleAgents, out.RecentClosed} {
		read = append(read, beadIDsOf(q)...)
	}
	if err := s.propagateReadTaintBeads(ctx, read); err != nil {
		return nil, err
	}
	return out, nil
}

// MyWorkSummary is the gRPC handler.
func (s *BeadsServer) MyWorkSummary(ctx context.Context, req *beadsv1.MyWorkSummaryRequest) (*beadsv1.MyWorkSummaryResponse, error) {
	sum, err := s.computeMyWorkSummary(ctx, req.GetOperatorId(), req.GetStaleMinutes(), req.GetLimit())
	if err != nil {
		return nil, status.Errorf(codes.Internal, "my work summary: %v", err)
	}
	return myWorkSummaryToProto(sum), nil
}

func myWorkSummaryToProto(sum *myWorkSummary) *beadsv1.MyWorkSummaryResponse {
	if sum == nil {
		return &beadsv1.MyWorkSummaryResponse{}
	}
	return &beadsv1.MyWorkSummaryResponse{
		DecisionsPending:      beadsToProto(sum.DecisionsPending),
		DecisionsPendingCount: int32(sum.DecisionsPendingCount),
		MailUnread:            beadsToProto(sum.MailUnread),
		MailUnreadCount:       int32(sum.MailUnreadCount),
		AssignedOpen:          beadsToProto(sum.AssignedOpen),
		AssignedOpenCount:     int32(sum.AssignedOpenCount),
		StaleAgents:           beadsToProto(sum.StaleAgents),
		StaleAgentsCount:      int32(sum.StaleAgentsCount),
		RecentClosed:          beadsToProto(sum.RecentClosed),
		RecentClosedCount:     int32(sum.RecentClosedCount),
		OperatorId:            sum.OperatorID,
		StaleMinutes:          sum.StaleMinutes,
	}
}

func beadsToProto(in []*model.Bead) []*beadsv1.Bead {
	out := make([]*beadsv1.Bead, 0, len(in))
	for _, b := range in {
		out = append(out, beadToProto(b))
	}
	return out
}
