package main

import (
	"fmt"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// maxBulkBeads caps the per-request bead set the bulk endpoints will operate
// on. Same defense-in-depth motivation as the single-bead editors — keeps a
// runaway form post from fanning out into hundreds of daemon RPCs.
const maxBulkBeads = 200

// handleProjectBulkStatus updates the status of every bead listed in the
// `bead_id` form values. Iterates over the existing UpdateBead RPC — there is
// no batch RPC on the daemon side. On success, re-renders the bulk action bar
// (cleared) plus an OOB row partial for each updated bead so htmx swaps the
// rows in place.
func (s *Server) handleProjectBulkStatus(w http.ResponseWriter, r *http.Request) {
	project, ids, status, _, _, ok := s.parseBulkRequest(w, r, true)
	if !ok {
		return
	}
	if !validStatus(status) {
		s.renderBulkBar(w, r, project, "Status must be one of open, in_progress, or closed.", nil, http.StatusBadRequest)
		return
	}
	updated := make([]string, 0, len(ids))
	var firstErr string
	for _, id := range ids {
		st := status
		if err := s.daemon.UpdateBead(r.Context(), id, beadsapi.UpdateBeadRequest{Status: &st}); err != nil {
			s.logger.Error("bulk status update", "id", id, "actor", oidcauth.EmailFromContext(r.Context()), "status", status, "error", err)
			if firstErr == "" {
				firstErr = fmt.Sprintf("Failed to update %s.", id)
			}
			continue
		}
		updated = append(updated, id)
	}
	httpStatus := http.StatusOK
	if firstErr != "" && len(updated) == 0 {
		httpStatus = http.StatusBadGateway
	}
	s.renderBulkBar(w, r, project, firstErr, updated, httpStatus)
}

// handleProjectBulkAssignee sets (or clears) the assignee field on every
// bead in `bead_id`. Empty `assignee` is a deliberate clear — same semantics
// as the per-row popover in assignee_edit.go. Iterates over the existing
// UpdateBead RPC; no batch RPC on the daemon side. Same OOB-row response
// shape as the status / labels endpoints.
func (s *Server) handleProjectBulkAssignee(w http.ResponseWriter, r *http.Request) {
	project, ids, _, _, _, ok := s.parseBulkRequest(w, r, false)
	if !ok {
		return
	}
	raw := r.PostForm.Get("assignee")
	if len(raw) > maxAssigneeLen {
		s.renderBulkBar(w, r, project, "Assignee input is too long.", nil, http.StatusRequestEntityTooLarge)
		return
	}
	assignee := strings.TrimSpace(raw)
	updated := make([]string, 0, len(ids))
	var firstErr string
	for _, id := range ids {
		a := assignee
		if err := s.daemon.UpdateBead(r.Context(), id, beadsapi.UpdateBeadRequest{Assignee: &a}); err != nil {
			s.logger.Error("bulk assignee update", "id", id, "actor", oidcauth.EmailFromContext(r.Context()), "assignee", assignee, "error", err)
			if firstErr == "" {
				firstErr = fmt.Sprintf("Failed to update %s.", id)
			}
			continue
		}
		updated = append(updated, id)
	}
	httpStatus := http.StatusOK
	if firstErr != "" && len(updated) == 0 {
		httpStatus = http.StatusBadGateway
	}
	s.renderBulkBar(w, r, project, firstErr, updated, httpStatus)
}

// handleProjectBulkLabels adds and/or removes a single label across every
// bead listed in `bead_id`. Accepts at most one label per direction per
// request — keeps the action bar simple and matches the typical "tag a batch"
// flow. The form must supply at least one of add_label / remove_label.
func (s *Server) handleProjectBulkLabels(w http.ResponseWriter, r *http.Request) {
	project, ids, _, addLabel, removeLabel, ok := s.parseBulkRequest(w, r, false)
	if !ok {
		return
	}
	addLabel = strings.TrimSpace(addLabel)
	removeLabel = strings.TrimSpace(removeLabel)
	if addLabel == "" && removeLabel == "" {
		s.renderBulkBar(w, r, project, "Provide a label to add or remove.", nil, http.StatusBadRequest)
		return
	}
	updated := make([]string, 0, len(ids))
	var firstErr string
	for _, id := range ids {
		failed := false
		if addLabel != "" {
			if err := s.daemon.AddLabel(r.Context(), id, addLabel); err != nil {
				s.logger.Error("bulk add label", "id", id, "actor", oidcauth.EmailFromContext(r.Context()), "label", addLabel, "error", err)
				if firstErr == "" {
					firstErr = fmt.Sprintf("Failed to add label on %s.", id)
				}
				failed = true
			}
		}
		if !failed && removeLabel != "" {
			if err := s.daemon.RemoveLabel(r.Context(), id, removeLabel); err != nil {
				s.logger.Error("bulk remove label", "id", id, "actor", oidcauth.EmailFromContext(r.Context()), "label", removeLabel, "error", err)
				if firstErr == "" {
					firstErr = fmt.Sprintf("Failed to remove label on %s.", id)
				}
				failed = true
			}
		}
		if !failed {
			updated = append(updated, id)
		}
	}
	httpStatus := http.StatusOK
	if firstErr != "" && len(updated) == 0 {
		httpStatus = http.StatusBadGateway
	}
	s.renderBulkBar(w, r, project, firstErr, updated, httpStatus)
}

// parseBulkRequest enforces the shared preconditions for both bulk endpoints:
// project path value, OIDC auth, parsed form, ≥1 bead_id within the cap.
// Returns project, ids, status, addLabel, removeLabel, ok. When ok is false
// the response has already been written.
func (s *Server) parseBulkRequest(w http.ResponseWriter, r *http.Request, requireStatus bool) (string, []string, string, string, string, bool) {
	project := r.PathValue("project")
	if project == "" {
		http.Error(w, "Missing project", http.StatusBadRequest)
		return "", nil, "", "", "", false
	}
	if oidcauth.EmailFromContext(r.Context()) == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return "", nil, "", "", "", false
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return "", nil, "", "", "", false
	}
	raw := r.PostForm["bead_id"]
	ids := dedupeNonEmpty(raw)
	if len(ids) == 0 {
		s.renderBulkBar(w, r, project, "Select at least one bead.", nil, http.StatusBadRequest)
		return "", nil, "", "", "", false
	}
	if len(ids) > maxBulkBeads {
		s.renderBulkBar(w, r, project, fmt.Sprintf("Too many beads (max %d).", maxBulkBeads), nil, http.StatusRequestEntityTooLarge)
		return "", nil, "", "", "", false
	}
	status := strings.TrimSpace(r.PostForm.Get("status"))
	if requireStatus && status == "" {
		s.renderBulkBar(w, r, project, "Status is required.", nil, http.StatusBadRequest)
		return "", nil, "", "", "", false
	}
	return project, ids, status, r.PostForm.Get("add_label"), r.PostForm.Get("remove_label"), true
}

// dedupeNonEmpty trims whitespace, drops empty values, and dedupes preserving
// input order.
func dedupeNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// renderBulkBar writes the cleared bulk action bar plus OOB row partials for
// every successfully-updated bead. The bar is the htmx target (#project-bulk-bar);
// each row partial OOB-swaps into its own #project-row-{id}.
//
// errMsg is shown inline above the bar; pass "" to clear.
func (s *Server) renderBulkBar(w http.ResponseWriter, r *http.Request, project, errMsg string, updatedIDs []string, status int) {
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Render the action bar inline to avoid a dedicated partial — it shares
	// the same DOM shape as the original (id="project-bulk-bar"), so htmx
	// outerHTML swaps drop it back to the cleared state.
	fmt.Fprintf(w, `<div id="project-bulk-bar" class="project-bulk-bar" data-project=%q hidden>`, project)
	fmt.Fprintf(w, `<span class="project-bulk-count" data-bulk-count>0 selected</span>`)
	fmt.Fprintf(w,
		`<form class="project-bulk-form project-bulk-status" hx-post="%s/p/%s/bulk/status" hx-include=".project-row-select:checked" hx-target="#project-bulk-bar" hx-swap="outerHTML" hx-disabled-elt="find select, find button">`,
		s.basePath, project)
	fmt.Fprint(w, `<label>Status <select name="status" aria-label="Bulk status"><option value="open">open</option><option value="in_progress">in_progress</option><option value="closed">closed</option></select></label>`)
	fmt.Fprint(w, `<button type="submit" class="bead-action">Apply</button></form>`)
	fmt.Fprintf(w,
		`<form class="project-bulk-form project-bulk-labels" hx-post="%s/p/%s/bulk/labels" hx-include=".project-row-select:checked" hx-target="#project-bulk-bar" hx-swap="outerHTML" hx-disabled-elt="find input, find button">`,
		s.basePath, project)
	fmt.Fprint(w, `<label>Add label <input type="text" name="add_label" autocomplete="off" aria-label="Bulk add label"></label>`)
	fmt.Fprint(w, `<label>Remove label <input type="text" name="remove_label" autocomplete="off" aria-label="Bulk remove label"></label>`)
	fmt.Fprint(w, `<button type="submit" class="bead-action">Apply</button></form>`)
	fmt.Fprintf(w,
		`<form class="project-bulk-form project-bulk-assignee" hx-post="%s/p/%s/bulk/assignee" hx-include=".project-row-select:checked" hx-target="#project-bulk-bar" hx-swap="outerHTML" hx-disabled-elt="find input, find button">`,
		s.basePath, project)
	fmt.Fprint(w, `<label>Assignee <input type="text" name="assignee" autocomplete="off" aria-label="Bulk assignee"></label>`)
	fmt.Fprint(w, `<button type="submit" class="bead-action">Apply</button></form>`)
	fmt.Fprint(w, `<button type="button" class="bead-action project-bulk-clear" data-bulk-clear>Clear</button>`)
	if errMsg != "" {
		fmt.Fprintf(w, `<span class="bead-actions-error" role="alert">%s</span>`, errMsg)
	}
	fmt.Fprint(w, `</div>`)

	// OOB row partials for every successfully-updated bead. Re-fetch each
	// bead from the daemon so the row reflects the post-mutation state
	// (status pill, label-derived chips, etc.). Failures here are logged
	// and silently dropped — the operator can refresh to recover.
	tmpl, ok := s.partials["templates/partials/project_row.html"]
	if !ok {
		s.logger.Error("project_row partial not registered")
		return
	}
	// Clone (via bindAgentLink) before executing: the parsed partial is shared
	// across Server instances (loadTemplateAssets), and html/template forbids
	// Clone after a direct execute.
	bound, err := s.bindAgentLink(tmpl, r)
	if err != nil {
		s.logger.Error("project_row bind", "error", err)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	for _, id := range updatedIDs {
		bead, err := s.daemon.GetBead(r.Context(), id)
		if err != nil {
			s.logger.Warn("re-fetching bead for OOB row", "id", id, "error", err)
			continue
		}
		// Per-user-homepage M2 §6 (ka-0000007787): populate slug so the
		// OOB-swapped row links to /u/{slug}.
		s.populateOperatedBySlug(r.Context(), bead)
		row := projectViewFromBead(bead, email, project, s.basePath)
		data := map[string]any{
			"Row":      row,
			"BasePath": s.basePath,
			"OOB":      true,
		}
		if err := bound.ExecuteTemplate(w, "project_row", data); err != nil {
			s.logger.Error("project_row OOB render", "id", id, "error", err)
		}
	}
}

// projectViewFromBead constructs the per-row view model from a BeadDetail —
// shared between handleProject's initial render and the bulk endpoints' OOB
// re-render so a row updated via bulk action looks identical to one rendered
// from a fresh page load.
func projectViewFromBead(b *beadsapi.BeadDetail, email, project, basePath string) projectBeadView {
	chip := resolveOperatorChip(b.OperatedBy, b.OperatedByDisplay, b.OperatedByEmail, b.OperatedBySlug, b.CreatedBy, basePath, email)
	return projectBeadView{
		ID:                      b.ID,
		Title:                   b.Title,
		ShortTitle:              b.ShortTitle,
		Heraldry:                bearerArms(b),
		Type:                    b.Type,
		Status:                  b.Status,
		Priority:                b.Priority,
		ShowPriority:            b.Priority <= 1,
		Assignee:                b.Assignee,
		DescPreview:             snippet(b.Description, 120),
		Ago:                     formatAgo(b.UpdatedAt),
		UpdatedAtRFC:            rfcTime(b.UpdatedAt),
		OperatorEmail:           email,
		CanClose:                email != "" && b.Status != "closed",
		Project:                 project,
		IsRichReport:            b.Type == "rich-report",
		HasReport:               b.HasReport,
		BlockedOnDecision:       b.BlockedOnDecision,
		BlockedOnDecisionBeadID: b.BlockedOnDecisionBeadID,
		OperatorChipCompactHTML: renderOperatorChipCompact(chip),
	}
}
