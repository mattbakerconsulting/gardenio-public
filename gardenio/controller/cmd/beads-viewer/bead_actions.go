package main

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// priorityMin / priorityMax bound the inline priority editor. kbeads stores
// priority as 0-5 (0 = highest, 5 = lowest); anything outside this range is
// rejected with an inline error so the operator can correct without losing
// the original value.
const (
	priorityMin = 0
	priorityMax = 5
)

// priorityOptions is the canonical option list rendered into the inline
// priority <select>. Pre-built so the template doesn't have to range over a
// computed slice on every render.
var priorityOptions = []int{0, 1, 2, 3, 4, 5}

// statusOptions bounds the inline status editor. The viewer only exposes the
// three canonical lifecycle states; anything else is rejected with an inline
// error so the operator can correct without losing the original value. kbeads
// is still authoritative for legality of any specific transition.
var statusOptions = []string{"open", "in_progress", "closed"}

// validStatus returns true if v is one of the canonical lifecycle states the
// inline editor exposes.
func validStatus(v string) bool {
	return slices.Contains(statusOptions, v)
}

// returnPartialActions and returnPartialRow select which fragment shape the
// action handlers re-render. Default is the bead-show toolbar; the active page
// passes ?return=row so the handler returns a <tr> swap target instead.
const (
	returnPartialActions = "actions"
	returnPartialRow     = "row"
	returnPartialProject = "project_row"
)

// beadActionsView is the template view model for the bead-show action toolbar
// partial. It encodes which buttons are visible for the current operator/bead
// state — Close vs Reopen, Claim vs Release — so the template stays declarative.
type beadActionsView struct {
	BeadID             string
	Status             string
	Assignee           string
	OperatorEmail      string
	CanClose           bool
	CanReopen          bool
	CanClaim           bool
	CanRelease         bool
	CanEditPriority    bool
	CanEditAssignee    bool
	CanEditDescription bool
	CanEditStatus      bool
	CanEditTitle       bool
	CanEditLabels      bool
	CanEditOwner       bool
	IsInboxWorthy      bool
	IsPinned           bool
	Priority           int
	PriorityOptions    []int
	StatusOptions      []string
	Error              string
	IsRowPartial       bool
	RowProject         string
	RowTitle           string
	RowShortTitle      string // shown in place of RowTitle when set (gd-V5nGseRS5f)
	RowType            string
	RowPriority        int
	ShowPriority       bool
	RowAgo             string
	OperatorChipHTML   template.HTML
	// OperatorChipCompactHTML is the initials-only operator chip rendered in
	// the dense /active and /views/{id} table rows (ka-0000000013). Empty for
	// rows whose chip kind has no compact treatment (scheduler / empty);
	// callers fall back to OperatorChipHTML when this is empty.
	OperatorChipCompactHTML template.HTML
}

// buildBeadActions computes which action buttons should be shown for the
// given bead and operator. Closed beads only get Reopen; open/in_progress
// beads get Close plus Claim or Release depending on assignee.
//
// pinnedSet is the viewer's set of pinned bead ids (from their
// profile_prefs.PinnedBeadIDs). The Pin/Unpin button reflects whether this
// bead is in that set — a nil set renders the button in the "Pin" state.
func buildBeadActions(bead *beadsapi.BeadDetail, email string, pinnedSet map[string]struct{}) beadActionsView {
	_, isPinned := pinnedSet[bead.ID]
	v := beadActionsView{
		BeadID:          bead.ID,
		Status:          bead.Status,
		Assignee:        bead.Assignee,
		OperatorEmail:   email,
		Priority:        bead.Priority,
		PriorityOptions: priorityOptions,
		StatusOptions:   statusOptions,
		IsInboxWorthy:   slices.Contains(bead.Labels, labelInboxWorthy),
		IsPinned:        isPinned,
	}
	if email == "" {
		// Anonymous viewers see no action buttons. The OIDC middleware
		// should already block this path in production, but the toolbar
		// still renders — just empty.
		return v
	}
	// Authenticated operators can edit priority on any bead, regardless of
	// open/closed/assignee — same gate as the comment box.
	v.CanEditPriority = true
	v.CanEditAssignee = true
	v.CanEditDescription = true
	v.CanEditStatus = true
	v.CanEditTitle = true
	v.CanEditLabels = true
	v.CanEditOwner = true
	if bead.Status == "closed" {
		v.CanReopen = true
		return v
	}
	// An escalation is resolved through its dedicated approve/reject verbs
	// (the bridge-SPA ReviewURL), not the generic close button: since
	// ka-0000005516 the daemon diverts a generic close of an open escalation
	// with 409, so offering the button here would only surface a failed close.
	v.CanClose = bead.Type != "escalation"
	if bead.Assignee == email {
		v.CanRelease = true
	} else {
		v.CanClaim = true
	}
	return v
}

// handleCloseBeadAction closes a bead, attributing the close to the
// authenticated operator. On success, re-renders the action toolbar partial
// (or the active-row partial when ?return=row). An optional form field
// `reason` is recorded the way `kd close --reason` records it: a comment by
// the operator, posted before the close (the in-garden bead panel's "Close
// with a reason", Lands WB-4).
func (s *Server) handleCloseBeadAction(w http.ResponseWriter, r *http.Request) {
	reason := strings.TrimSpace(r.PostFormValue("reason"))
	if len(reason) > maxCommentLen {
		http.Error(w, "The reason is too long (max 10000 characters).", http.StatusRequestEntityTooLarge)
		return
	}
	s.runBeadAction(w, r, func(ctx context.Context, id, email string) error {
		if reason != "" {
			if err := s.daemon.AddComment(beadsapi.WithActor(ctx, email), id, email, reason); err != nil {
				return err
			}
		}
		return s.daemon.CloseBeadAs(ctx, id, email, nil)
	}, "Failed to close bead.")
}

// handleReopenBead flips a closed bead back to status=open.
func (s *Server) handleReopenBead(w http.ResponseWriter, r *http.Request) {
	s.runBeadAction(w, r, func(ctx context.Context, id, email string) error {
		open := "open"
		return s.daemon.UpdateBead(ctx, id, beadsapi.UpdateBeadRequest{Status: &open})
	}, "Failed to reopen bead.")
}

// handleClaimBead assigns the bead to the operator and flips status to
// in_progress (matches `kd claim` semantics).
func (s *Server) handleClaimBead(w http.ResponseWriter, r *http.Request) {
	s.runBeadAction(w, r, func(ctx context.Context, id, email string) error {
		ip := "in_progress"
		return s.daemon.UpdateBead(ctx, id, beadsapi.UpdateBeadRequest{
			Assignee: &email,
			Status:   &ip,
		})
	}, "Failed to claim bead.")
}

// handleReleaseBead clears the assignee on a bead the operator currently owns.
// Status is left as-is so a release on an in_progress bead doesn't silently
// flip it to open behind the user's back.
func (s *Server) handleReleaseBead(w http.ResponseWriter, r *http.Request) {
	s.runBeadAction(w, r, func(ctx context.Context, id, email string) error {
		empty := ""
		return s.daemon.UpdateBead(ctx, id, beadsapi.UpdateBeadRequest{Assignee: &empty})
	}, "Failed to release bead.")
}

// handleDeferBead parks a bead as deferred (matches `kd defer` without
// --until): the /garden chore tour's Defer (gd-S2Cn2dTsP0). The inline status
// editor keeps its three lifecycle states; this is the one way in to the fourth.
func (s *Server) handleDeferBead(w http.ResponseWriter, r *http.Request) {
	s.runBeadAction(w, r, func(ctx context.Context, id, email string) error {
		deferred := "deferred"
		return s.daemon.UpdateBead(ctx, id, beadsapi.UpdateBeadRequest{Status: &deferred})
	}, "Failed to defer bead.")
}

// handleUpdateStatus changes the bead status via the inline editor in the
// bead-actions toolbar. Form value `status` must be one of the canonical
// lifecycle states (open / in_progress / closed); anything else re-renders
// the partial with an inline error and the original value preserved. kbeads
// remains authoritative for transition legality.
func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}

	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead for status update", "id", id, "error", err)
		http.Error(w, "Failed to load bead", http.StatusInternalServerError)
		return
	}

	status := strings.TrimSpace(r.PostForm.Get("status"))
	if !validStatus(status) {
		s.renderBeadActionsPartial(w, r, bead, email, "Status must be one of open, in_progress, or closed.", http.StatusBadRequest)
		return
	}

	if err := s.daemon.UpdateBead(r.Context(), id, beadsapi.UpdateBeadRequest{Status: &status}); err != nil {
		s.logger.Error("updating status", "id", id, "actor", email, "status", status, "error", err)
		msg, code := daemonWriteFailure(err, "Failed to update status.")
		s.renderBeadActionsPartial(w, r, bead, email, msg, code)
		return
	}

	updated, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.logger.Warn("re-fetching bead after status update", "id", id, "error", err)
		updated = bead
		updated.Status = status
	}
	s.renderBeadActionsPartial(w, r, updated, email, "", http.StatusOK)
}

// runBeadAction is the shared backbone of the four POST handlers. It loads the
// bead, runs the per-action mutation, re-fetches, and renders the appropriate
// partial. Errors render inline on the same partial with a 4xx/5xx status.
func (s *Server) runBeadAction(w http.ResponseWriter, r *http.Request, do func(ctx context.Context, id, email string) error, failMsg string) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}

	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead for action", "id", id, "error", err)
		http.Error(w, "Failed to load bead", http.StatusInternalServerError)
		return
	}

	if err := do(r.Context(), id, email); err != nil {
		s.logger.Error("bead action failed", "id", id, "actor", email, "error", err)
		msg, status := daemonWriteFailure(err, failMsg)
		s.renderBeadActionsPartial(w, r, bead, email, msg, status)
		return
	}

	updated, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.logger.Warn("re-fetching bead after action", "id", id, "error", err)
		updated = bead
	}
	s.renderBeadActionsPartial(w, r, updated, email, "", http.StatusOK)
}

// renderBeadActionsPartial picks between the bead-show toolbar partial and the
// active-row partial based on the ?return= query param.
func (s *Server) renderBeadActionsPartial(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, email, errMsg string, status int) {
	if respondAction(w, r, bead.ID, actionFailure(errMsg, status)) {
		return
	}
	view := buildBeadActions(bead, email, s.viewerPinnedSet(r.Context(), email))
	view.Error = errMsg

	if status != http.StatusOK {
		writeInlineStatus(w, status)
	}

	// Per-user-homepage M2 §6 (ka-0000007787): populate the slug so any
	// chip the partial renders points at /u/{slug}.
	s.populateOperatedBySlug(r.Context(), bead)

	switch r.URL.Query().Get("return") {
	case returnPartialRow:
		view.IsRowPartial = true
		project := projectFromLabels(bead.Labels)
		if project == "" {
			project = "untagged"
		}
		view.RowProject = project
		view.RowTitle = bead.Title
		view.RowShortTitle = bead.ShortTitle
		view.RowType = bead.Type
		view.RowPriority = bead.Priority
		view.ShowPriority = bead.Priority <= 1
		view.RowAgo = formatAgo(bead.UpdatedAt)
		rowChip := resolveOperatorChip(bead.OperatedBy, bead.OperatedByDisplay, bead.OperatedByEmail, bead.OperatedBySlug, bead.CreatedBy, s.basePath, email)
		view.OperatorChipHTML = renderOperatorChip(rowChip)
		view.OperatorChipCompactHTML = renderOperatorChipCompact(rowChip)
		s.renderPartial(w, r, "templates/partials/active_row.html", "active_row", map[string]any{
			"Row": view,
		})
	case returnPartialProject:
		project := projectFromLabels(bead.Labels)
		if project == "" {
			project = "untagged"
		}
		row := projectViewFromBead(bead, email, project, s.basePath)
		s.renderPartial(w, r, "templates/partials/project_row.html", "project_row", map[string]any{
			"Row":      row,
			"BasePath": s.basePath,
			"OOB":      false,
		})
	default:
		s.renderPartial(w, r, "templates/partials/bead_actions.html", "bead_actions", map[string]any{
			"Actions": view,
		})
	}
}
