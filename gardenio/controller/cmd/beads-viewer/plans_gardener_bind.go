package main

// plans_gardener_bind.go — auto-bind the DURABLE looping plan-gardener when a
// plan comes into existence (task ka-0000002863). Two seams call it:
//
//   - POST /plans (handleCreatePlan): right after the one-shot author
//     assistant is spawned, the durable gardener loop is bound too. The plan
//     was just minted with plan_state=draft (the 3a born-on write), so the
//     gardener is born PAUSED (enabled=false) — the scheduler cannot fire an
//     unapproved plan; "Approve & build" (handlePlanBuild →
//     setGardenerEnabledCore) is what enables it.
//   - hand-mark (handlePlanMark, add direction): an epic promoted to a plan
//     by the one-click mark gets its gardener too. A legacy epic with no
//     plan_state is UNGATED, so its gardener is born enabled — the historical
//     behaviour, matching the gb-plan-gardener backfill.
//
// The actual gardener-spec builder is internal/plangardener.Bind — the ONE
// shared core `gb plan gardener` also wraps — so the viewer emits gardener
// beads byte-identical to the CLI's, including the idempotent-adopt (a
// re-mark / double-submit / retry never mints a duplicate: an existing
// gardener, enabled OR paused, is adopted).
//
// BEST-EFFORT by design, exactly like spawnPlanAuthorAssistant: the plan is
// already write-through (D5) — persisted and discoverable — so a bind
// failure must NOT fail the create/mark. Failures are logged (Warn — visible,
// never silent) and the request still succeeds; `gb plan gardener <id>` is the
// out-of-page recovery path.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
	"gardenio/controller/internal/plangardener"
)

// bindPlanGardener binds the durable plan-gardener to the given plan epic via
// the shared plangardener core. Best-effort: errors are logged at Warn and
// swallowed (see file header). The ctx must carry the operator stamp
// (beadsapi.WithOperator) — the kbeads audit resolver fail-closes the
// CreateBead otherwise, which here degrades to a logged Warn, not a request
// failure.
func (s *Server) bindPlanGardener(ctx context.Context, epic *beadsapi.BeadDetail, email string) {
	if epic == nil {
		return
	}
	res, err := plangardener.Bind(ctx, s.daemon, epic, plangardener.BindSpec{
		Operator:  canonicalOIDC(email),
		CreatedBy: email,
	})
	if err != nil {
		s.logger.Warn("plan gardener: bind", "plan", epic.ID, "actor", email, "error", err)
		return
	}
	if res.Adopted {
		s.logger.Info("plan gardener: adopted existing",
			"plan", epic.ID, "gardener", res.ID, "spawn_role", res.SpawnRole)
		return
	}
	s.logger.Info("plan gardener: bound",
		"plan", epic.ID, "gardener", res.ID, "enabled", res.Enabled,
		"cron", res.Cron, "actor", email)
}

// handlePlanGardenerEnable is the one-click "Enable gardener" button on the
// /plans/{id} cockpit (Matt's ask, thread ka-0000004696): a plan with NO bound
// gardener renders a button that binds one in place, replacing the copy-paste
// `gb plan gardener <id>` CLI hint. Operator-gated at the route layer.
//
// It calls the SAME shared plangardener.Bind core the CLI and the auto-bind
// paths use (via bindPlanGardenerErr — NOT the best-effort bindPlanGardener,
// because a button click must surface a failure to the operator instead of
// swallowing it), so a button-bound gardener is byte-identical to a CLI-bound
// one. Idempotent-adopt makes a double-click safe: a gardener already bound
// (e.g. via the CLI between page load and click) is adopted, not duplicated,
// and the re-render then shows the pause/resume + Wake levers.
//
// A newly-bound gardener is born per EnabledOnBind: a draft/ready plan's gardener
// is PAUSED behind the R6 approve→build gate ("Approve & build" enables it),
// while a legacy/ungated plan's gardener is born enabled — so this button
// changes NOTHING about the enable gate, it only removes the CLI detour to
// bind the loop in the first place.
func (s *Server) handlePlanGardenerEnable(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	ctx := r.Context()
	epic, err := s.daemon.GetBead(ctx, id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Plan not found", http.StatusNotFound)
			return
		}
		s.logger.Error("plan gardener enable: get plan", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if epic == nil || epic.Type != "epic" {
		http.Error(w, "Plan not found", http.StatusNotFound)
		return
	}
	if bindErr := s.bindPlanGardenerErr(ctx, epic, email); bindErr != "" {
		// Bind refused (e.g. the epic lost its 'plan' label, or a project-less
		// plan) — surface it inline in the card rather than a bare HTTP error, so
		// the operator sees WHY nothing bound without losing the cockpit.
		s.renderPlanGardenerErr(w, r, id, bindErr, http.StatusOK)
		return
	}
	// Success (fresh bind OR idempotent-adopt): re-render the card, which now
	// resolves the bound gardener and swaps the CLI hint for the pause/resume +
	// Wake levers (and, on a gated plan, the "Approve & build" affordance).
	s.renderPlanGardenerErr(w, r, id, "", http.StatusOK)
}

// bindPlanGardenerErr binds the durable plan-gardener via the shared plangardener
// core and returns a human-facing error string ("" on success). It is the
// SURFACED sibling of bindPlanGardener: the auto-bind seams (create / hand-mark)
// swallow bind failures because the plan is already write-through and the CLI
// is the recovery path, but the explicit "Enable gardener" button must tell the
// operator when a bind is refused. Both go through the ONE plangardener.Bind, so
// the gardener bead they mint is identical.
func (s *Server) bindPlanGardenerErr(ctx context.Context, epic *beadsapi.BeadDetail, email string) string {
	if epic == nil {
		return "Plan not found."
	}
	res, err := plangardener.Bind(ctx, s.daemon, epic, plangardener.BindSpec{
		Operator:  canonicalOIDC(email),
		CreatedBy: email,
	})
	if err != nil {
		s.logger.Warn("plan gardener: enable-button bind", "plan", epic.ID, "actor", email, "error", err)
		return err.Error()
	}
	if res.Adopted {
		s.logger.Info("plan gardener: enable-button adopted existing",
			"plan", epic.ID, "gardener", res.ID, "spawn_role", res.SpawnRole)
		return ""
	}
	s.logger.Info("plan gardener: enable-button bound",
		"plan", epic.ID, "gardener", res.ID, "enabled", res.Enabled,
		"cron", res.Cron, "actor", email)
	return ""
}
