// Package plans is the plan cockpit's write service: the bodies of the
// /plans/{id}/... POST handlers, lifted out of cmd/beads-viewer so the old
// HTMX handlers and the Workbench panel call the same functions (spec
// 2026-10-19-lands-workbench-glass-design.md §2.5 step 4, task 5.8).
//
// Every verb is a function over the daemon client. None of them touch HTTP:
// a verb returns nil or an *Error whose Kind says what went wrong and whose
// Msg is the operator-facing copy. Each caller picks its own surface: the
// viewer re-renders an HTMX fragment or answers a plain error, and a JSON
// caller answers {"ok":false,"error":Msg}.
//
// The service owns no plan invariant. The daemon is the sole authority for
// plan_state transition legality, the gardener-enable gate, born-on-label
// creates and the atomic phase_order/step_order primitives (carve-out
// ka-0000004319; banner in cmd/beads-viewer/plans_lifecycle.go). The checks
// here are URL-scoping guards and UX pre-checks that choose the message; the
// daemon rejects an illegal write regardless.
package plans

import (
	"context"
	"errors"
	"log/slog"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planagent"
)

// Daemon is the slice of the beads daemon client the plan writes compose.
type Daemon interface {
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
	BatchGetBeads(ctx context.Context, ids []string) (map[string]*beadsapi.BeadDetail, error)
	GetReverseDependencies(ctx context.Context, id string) ([]beadsapi.Dependency, error)
	CreateBead(ctx context.Context, req beadsapi.CreateBeadRequest) (string, error)
	AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error
	UpdateBead(ctx context.Context, beadID string, req beadsapi.UpdateBeadRequest) error
	UpdateBeadFieldsTyped(ctx context.Context, beadID string, fields map[string]any) error
	AppendBeadArrayField(ctx context.Context, beadID, field, element string) error
	ReorderBeadArrayField(ctx context.Context, beadID, field string, expected, next []string) error
	CloseBeadAs(ctx context.Context, beadID, actor string, fields map[string]string) error
	AddComment(ctx context.Context, beadID, author, text string) error
	AddLabel(ctx context.Context, beadID, label string) error
	RemoveLabel(ctx context.Context, beadID, label string) error
}

// The real daemon client satisfies Daemon; if the interface drifts from it,
// this fails to compile.
var _ Daemon = (*beadsapi.Client)(nil)

// Gardeners is the plan-gardener machinery the lifecycle verbs reach. It still
// lives in the viewer (gardener_ops.go, plans_gardener*.go), so the service
// takes it as a seam rather than re-implementing it.
type Gardeners interface {
	// PlanGardener returns the gardener bound to the plan, or nil when none is.
	PlanGardener(ctx context.Context, planID string) *beadsapi.BeadDetail
	// SetGardenerEnabled enables or pauses a gardener. The error's text is
	// operator-facing.
	SetGardenerEnabled(ctx context.Context, gardener *beadsapi.BeadDetail, enable bool, email string) error
	// BindGardener binds a durable gardener to a newly marked plan. Best-effort:
	// it never fails the caller.
	BindGardener(ctx context.Context, epic *beadsapi.BeadDetail, email string)
}

// Assistant delivers an instruction to the plan's gardener: a nudge when it is
// live, a queued message plus wake when parked, a spawn when none is bound.
type Assistant interface {
	Prompt(ctx context.Context, epic *beadsapi.BeadDetail, actor, seed string) (planagent.DeliveryOutcome, string, error)
}

// Service runs the plan cockpit's write verbs.
type Service struct {
	Daemon    Daemon
	Gardeners Gardeners
	Assistant Assistant
	Logger    *slog.Logger
}

// Actor is the operator a write acts for.
type Actor struct {
	// Email is the operator's address as the auth layer reports it. It is the
	// created_by on new beads and the author of audit comments.
	Email string
	// Operator is the canonical operator id (oidc:<lowercased email>). Verbs
	// that create or link beads stamp it on the daemon ctx as X-Operated-By;
	// without it the kbeads audit resolver fail-closes with HTTP 400 (R2,
	// ka-0000005772). It is also the attribution on a plan-epic close.
	Operator string
}

// Kind classifies a write failure. Callers map it to their own surface.
type Kind int

const (
	// KindInternal is an unexpected failure (a plan read that failed for a
	// reason other than not-found).
	KindInternal Kind = iota
	// KindBadRequest is a malformed address: a missing id or a bad direction.
	// There is nothing to re-render.
	KindBadRequest
	// KindNotFound is a plan, step or doc that does not exist or is not part of
	// this plan. There is nothing to re-render.
	KindNotFound
	// KindInvalid is a submitted value the verb rejects (an empty title, a doc
	// with no text body). The caller re-renders with Msg inline.
	KindInvalid
	// KindTooLarge is a submitted value over its cap.
	KindTooLarge
	// KindConflict is contention or a missing prerequisite the operator can fix
	// and retry: repeated concurrent reorders, no gardener bound.
	KindConflict
	// KindRefused is a state pre-check that declined the write; nothing changed
	// and the current state is still worth showing.
	KindRefused
	// KindUpstream is a daemon write that failed.
	KindUpstream
)

// Error is a write failure with operator-facing copy.
type Error struct {
	Kind Kind
	// Msg is the operator-facing message.
	Msg string
	// Err is the underlying cause, when there is one.
	Err error
}

func (e *Error) Error() string {
	if e.Err != nil {
		return e.Msg + ": " + e.Err.Error()
	}
	return e.Msg
}

func (e *Error) Unwrap() error { return e.Err }

// KindOf returns err's Kind, or KindInternal for an error that is not an *Error.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return KindInternal
}

// Message returns err's operator-facing message, or a generic one for an error
// that is not an *Error.
func Message(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Msg
	}
	return "Internal Server Error"
}

func fail(kind Kind, msg string, cause error) *Error {
	return &Error{Kind: kind, Msg: msg, Err: cause}
}

var errPlanNotFound = &Error{Kind: KindNotFound, Msg: "Plan not found"}

// planEpic loads the plan epic. Any read failure is a not-found, as the step
// and edit verbs have always answered it.
func (s *Service) planEpic(ctx context.Context, planID string) (*beadsapi.BeadDetail, error) {
	epic, err := s.Daemon.GetBead(ctx, planID)
	if err != nil || epic == nil || epic.Type != "epic" {
		return nil, errPlanNotFound
	}
	return epic, nil
}

// planEpicStrict loads the plan epic for a lifecycle verb: a daemon 404 or a
// non-epic is a not-found, any other read failure is internal.
func (s *Service) planEpicStrict(ctx context.Context, planID string) (*beadsapi.BeadDetail, error) {
	epic, err := s.Daemon.GetBead(ctx, planID)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == 404 {
			return nil, errPlanNotFound
		}
		s.Logger.Error("plan lifecycle: get plan", "id", planID, "error", err)
		return nil, fail(KindInternal, "Internal Server Error", err)
	}
	if epic == nil || epic.Type != "epic" {
		return nil, errPlanNotFound
	}
	return epic, nil
}

// operatorCtx stamps the actor as X-Operated-By on the daemon ctx.
func operatorCtx(ctx context.Context, a Actor) context.Context {
	return beadsapi.WithOperator(ctx, a.Operator)
}
