package main

import (
	"context"
	"net/http"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/planagent"
	"gardenio/controller/internal/plans"
)

// The cockpit's POST handlers (plans.go, plan_edit.go, plan_phases.go,
// plans_lifecycle.go) parse the request, call internal/plans for the write and
// render the result. This file is the glue: the service bound to a request,
// and the mapping from a service error to the response.

// plansService returns the plans write service for one request. The gardener
// and assistant machinery still lives in the viewer, so it reaches the service
// through planSeams.
func (s *Server) plansService(r *http.Request) *plans.Service {
	seams := planSeams{s: s, r: r}
	return &plans.Service{
		Daemon:    s.daemon,
		Gardeners: seams,
		Assistant: seams,
		Logger:    s.logger,
	}
}

// planActor is the operator a cockpit write acts for.
func planActor(email string) plans.Actor {
	return plans.Actor{Email: email, Operator: canonicalOIDC(email)}
}

// planSeams adapts the Server's gardener and plan-assistant code to the plans
// service. r is held only because setGardenerEnabledCore takes a request; it
// reads nothing from it but the context, which each call replaces.
type planSeams struct {
	s *Server
	r *http.Request
}

func (p planSeams) PlanGardener(ctx context.Context, planID string) *beadsapi.BeadDetail {
	return p.s.planGardener(ctx, planID)
}

func (p planSeams) SetGardenerEnabled(ctx context.Context, gardener *beadsapi.BeadDetail, enable bool, email string) error {
	if opErr := p.s.setGardenerEnabledCore(p.r.WithContext(ctx), gardener, enable, email); opErr != nil {
		return opErr
	}
	return nil
}

func (p planSeams) BindGardener(ctx context.Context, epic *beadsapi.BeadDetail, email string) {
	p.s.bindPlanGardener(ctx, epic, email)
}

func (p planSeams) Prompt(ctx context.Context, epic *beadsapi.BeadDetail, actor, seed string) (planagent.DeliveryOutcome, string, error) {
	_, outcome, name, err := p.s.promptPlanAssistant(ctx, epic, actor, seed)
	return outcome, name, err
}

// planWriteStatus is the HTTP status for a plans service error.
func planWriteStatus(err error) int {
	switch plans.KindOf(err) {
	case plans.KindBadRequest, plans.KindInvalid:
		return http.StatusBadRequest
	case plans.KindNotFound:
		return http.StatusNotFound
	case plans.KindTooLarge:
		return http.StatusRequestEntityTooLarge
	case plans.KindConflict:
		return http.StatusConflict
	case plans.KindRefused:
		// Nothing changed; the re-rendered fragment carries the reason.
		return http.StatusOK
	case plans.KindUpstream:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// planActionError is a plans service error as a cockpit write's JSON answer
// (respondAction, action_json.go): its operator-facing message and its status,
// a refusal (nothing changed; the HTML fragment re-renders it at 200) a 409.
func planActionError(err error) error {
	if err == nil {
		return nil
	}
	status := planWriteStatus(err)
	if plans.KindOf(err) == plans.KindRefused {
		status = http.StatusConflict
	}
	return &actionError{Msg: plans.Message(err), Status: status}
}

// respondPlanAction answers a JSON caller of a cockpit write (the garden's plan
// cockpit, Lands WB-7: {"ok":true,"bead":<id>} or {"ok":false,"error":…}) and
// reports true; for any other caller it writes nothing and the handler renders
// its fragment as before.
func respondPlanAction(w http.ResponseWriter, r *http.Request, bead string, err error) bool {
	return respondAction(w, r, bead, planActionError(err))
}

// planWriteHTTPError answers the service errors that have no fragment to
// re-render (a malformed address, a missing plan, step or doc, a failed plan
// read) with a plain error, and reports whether it did. Any other error is the
// caller's to render inline.
func planWriteHTTPError(w http.ResponseWriter, err error) bool {
	switch plans.KindOf(err) {
	case plans.KindBadRequest, plans.KindNotFound, plans.KindInternal:
		http.Error(w, plans.Message(err), planWriteStatus(err))
		return true
	}
	return false
}
