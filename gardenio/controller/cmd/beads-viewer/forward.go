package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// maxForwardNoteLen caps the operator-supplied forward note. The daemon does
// not enforce a length, but the UI input also caps via maxlength=500 so the
// server check is just defense-in-depth.
const maxForwardNoteLen = 500

// forwardFollowerView is one row in the "Followers" list rendered by the
// forward panel partial. Display fields fall back to "kind:subject" when the
// operators row has no canonical display string.
type forwardFollowerView struct {
	OperatorID int64
	Kind       string
	Subject    string
	Display    string
	Note       string
	// CanRemove is true when the current viewer is allowed to unforward this
	// row. v1 lets any signed-in operator unforward — the daemon also accepts
	// the call from any viewer, and the inbox surface can always silence the
	// row by closing the bead.
	CanRemove bool
}

// forwardOperatorOption is one suggestion offered by the picker's <datalist>.
// v1 leaves the option list empty (free-text email input); future iterations
// will populate this from a viewer-role operators-list endpoint.
type forwardOperatorOption struct {
	Email   string
	Display string
}

// forwardPanelView is the template view model for the forward panel partial.
type forwardPanelView struct {
	BeadID          string
	OperatorEmail   string
	Followers       []forwardFollowerView
	OperatorOptions []forwardOperatorOption
	PrefillEmail    string
	PrefillNote     string
	Error           string
}

// buildForwardPanelView assembles the view model. listErr being non-nil means
// the daemon's GET /v1/beads/{id}/followers errored — render the form anyway
// so the operator can still forward, but with an empty followers list.
func buildForwardPanelView(beadID, viewerEmail string, followers []*beadsapi.BeadFollower) forwardPanelView {
	v := forwardPanelView{
		BeadID:        beadID,
		OperatorEmail: viewerEmail,
	}
	for _, f := range followers {
		if f == nil {
			continue
		}
		display := f.OperatorDisplay
		if display == "" && f.OperatorSubject != "" {
			display = f.OperatorSubject
		}
		v.Followers = append(v.Followers, forwardFollowerView{
			OperatorID: f.OperatorID,
			Kind:       f.OperatorKind,
			Subject:    f.OperatorSubject,
			Display:    display,
			Note:       f.Note,
			CanRemove:  viewerEmail != "",
		})
	}
	return v
}

// resolveForwardTarget maps the user-supplied identifier (email or operator id)
// to a kbeads operators(id) FK by trying oidc-aliased lookups first, then
// numeric "operator-id:N" form. Returns 0 + a user-facing error message when
// no match exists; the form re-renders with that message inline.
func (s *Server) resolveForwardTarget(ctx context.Context, raw string) (int64, string) {
	id := strings.TrimSpace(raw)
	if id == "" {
		return 0, "Email is required."
	}
	// Operator IDs are entered as bare numbers or "operator-id:N"; aliases
	// are entered as plain emails (matched against kind=oidc subject).
	if rest, ok := strings.CutPrefix(id, "operator-id:"); ok {
		n, err := strconv.ParseInt(rest, 10, 64)
		if err != nil || n <= 0 {
			return 0, "Operator id must be a positive integer."
		}
		return n, ""
	}
	if n, err := strconv.ParseInt(id, 10, 64); err == nil && n > 0 {
		return n, ""
	}
	resp, err := s.daemon.LookupOperatorByAlias(ctx, "oidc", id)
	if err != nil {
		s.logger.Warn("forward: alias lookup", "subject", id, "error", err)
		return 0, "Failed to look up operator. Try again."
	}
	if resp == nil || !resp.Found || resp.OperatorID <= 0 {
		return 0, "No operator found for that email."
	}
	return resp.OperatorID, ""
}

// handleForwardPanel renders the forward panel partial as a standalone htmx
// fragment, swapping into #forward-panel on the bead-show page.
func (s *Server) handleForwardPanel(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	if _, err := s.daemon.GetBead(r.Context(), id); err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead for forward panel", "id", id, "error", err)
		http.Error(w, "Failed to load bead", http.StatusInternalServerError)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	followers, ferr := s.daemon.ListBeadFollowers(r.Context(), id)
	if ferr != nil {
		s.logger.Warn("listing followers", "id", id, "error", ferr)
		followers = nil
	}
	view := buildForwardPanelView(id, email, followers)
	s.renderForwardPanel(w, r, view, http.StatusOK)
}

// handleAddForward resolves the email + adds the follower, then re-renders the
// panel. Validation errors render inline with the operator-supplied values
// preserved so a typo is one keystroke away from being fixed.
func (s *Server) handleAddForward(w http.ResponseWriter, r *http.Request) {
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
	target := strings.TrimSpace(r.PostForm.Get("email"))
	note := strings.TrimSpace(r.PostForm.Get("note"))
	if len(note) > maxForwardNoteLen {
		note = note[:maxForwardNoteLen]
	}

	if _, err := s.daemon.GetBead(r.Context(), id); err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead for forward", "id", id, "error", err)
		http.Error(w, "Failed to load bead", http.StatusInternalServerError)
		return
	}

	opID, errMsg := s.resolveForwardTarget(r.Context(), target)
	if errMsg != "" {
		s.renderForwardPanelWithError(w, r, id, email, errMsg, target, note, http.StatusBadRequest)
		return
	}

	if err := s.daemon.AddBeadFollower(r.Context(), id, opID, note); err != nil {
		s.logger.Error("add follower", "bead", id, "operator", opID, "actor", email, "error", err)
		s.renderForwardPanelWithError(w, r, id, email, "Failed to forward bead.", target, note, http.StatusBadGateway)
		return
	}

	followers, ferr := s.daemon.ListBeadFollowers(r.Context(), id)
	if ferr != nil {
		s.logger.Warn("listing followers after add", "id", id, "error", ferr)
		followers = nil
	}
	view := buildForwardPanelView(id, email, followers)
	s.renderForwardPanel(w, r, view, http.StatusOK)
}

// handleRemoveForward unforwards a single (bead, operator) pair. Idempotent on
// the daemon side; the panel re-renders the now-shorter list.
func (s *Server) handleRemoveForward(w http.ResponseWriter, r *http.Request) {
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
	raw := strings.TrimSpace(r.PostForm.Get("operator_id"))
	opID, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || opID <= 0 {
		s.renderForwardPanelWithError(w, r, id, email, "Invalid operator id.", "", "", http.StatusBadRequest)
		return
	}
	if err := s.daemon.RemoveBeadFollower(r.Context(), id, opID); err != nil {
		s.logger.Error("remove follower", "bead", id, "operator", opID, "actor", email, "error", err)
		s.renderForwardPanelWithError(w, r, id, email, "Failed to unforward bead.", "", "", http.StatusBadGateway)
		return
	}
	followers, ferr := s.daemon.ListBeadFollowers(r.Context(), id)
	if ferr != nil {
		s.logger.Warn("listing followers after remove", "id", id, "error", ferr)
		followers = nil
	}
	view := buildForwardPanelView(id, email, followers)
	s.renderForwardPanel(w, r, view, http.StatusOK)
}

// renderForwardPanelWithError re-fetches the followers list and renders the
// panel with the given inline error + prefilled form values.
func (s *Server) renderForwardPanelWithError(w http.ResponseWriter, r *http.Request, id, email, errMsg, prefillEmail, prefillNote string, status int) {
	if respondAction(w, r, id, actionFailure(errMsg, status)) {
		return
	}
	followers, ferr := s.daemon.ListBeadFollowers(r.Context(), id)
	if ferr != nil {
		s.logger.Warn("listing followers for error render", "id", id, "error", ferr)
		followers = nil
	}
	view := buildForwardPanelView(id, email, followers)
	view.Error = errMsg
	view.PrefillEmail = prefillEmail
	view.PrefillNote = prefillNote
	s.renderForwardPanel(w, r, view, status)
}

// renderForwardPanel writes the partial to w with the given status. The r
// argument lets us reuse the agentLink-binding helper so the partial executes
// with the same per-request resolver as page renders.
func (s *Server) renderForwardPanel(w http.ResponseWriter, r *http.Request, view forwardPanelView, status int) {
	tpl, ok := s.partials["templates/partials/forward_panel.html"]
	if !ok {
		s.logger.Error("forward_panel partial not registered")
		http.Error(w, "Template missing", http.StatusInternalServerError)
		return
	}
	// Clone before executing: the parsed partial is shared across Server
	// instances (loadTemplateAssets), and html/template forbids Clone after a
	// direct execute — one bare ExecuteTemplate here would poison every later
	// renderPartial of the shared set.
	bound, err := s.bindAgentLink(tpl, r)
	if err != nil {
		s.logger.Error("cloning forward_panel", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	data := map[string]any{
		"BasePath": s.basePath,
		"Forward":  view,
	}
	if err := bound.ExecuteTemplate(w, "forward_panel", data); err != nil {
		s.logger.Error("rendering forward_panel", "error", err)
	}
}
