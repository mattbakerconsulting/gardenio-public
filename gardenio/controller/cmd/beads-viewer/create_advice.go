package main

import (
	"net/http"
	"slices"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// createAdviceSheetView is the data passed to the create-advice modal partial.
type createAdviceSheetView struct {
	OperatorEmail   string
	Projects        []string
	Roles           []string
	DefaultScope    string // global | project | role | agent
	DefaultValue    string // pre-filled scope value (e.g. project name)
	DefaultPriority int
}

// createAdviceResultView is the data passed to the result partial after a
// successful POST /advice. On error, Error is populated and the form is
// re-rendered as the result so the operator can fix and retry.
type createAdviceResultView struct {
	BeadID string
	Title  string
	Error  string
}

// defaultAdviceRoles mirrors the role picker options used elsewhere in the
// viewer (quick-spawn). These are presented as suggestions; the input is a
// free-text value so an operator can target an arbitrary role.
var defaultAdviceRoles = []string{"crew", "gardener", "thread"}

// adviceScopeKinds enumerates the scope kinds offered by the form. The
// values match the prefix used in the resulting label (global → "global";
// project/role/agent → "<kind>:<value>").
var adviceScopeKinds = []string{"global", "project", "role", "agent"}

// handleCreateAdviceSheet renders the modal that the "New Advice" button
// opens. Fetched via htmx and swapped into the shared modal host.
func (s *Server) handleCreateAdviceSheet(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}

	view := createAdviceSheetView{
		OperatorEmail:   email,
		Roles:           defaultAdviceRoles,
		DefaultScope:    strings.TrimSpace(r.URL.Query().Get("scope")),
		DefaultValue:    strings.TrimSpace(r.URL.Query().Get("value")),
		DefaultPriority: 2,
	}
	if !slices.Contains(adviceScopeKinds, view.DefaultScope) {
		view.DefaultScope = "global"
	}

	projects, err := s.daemon.ListProjectBeads(r.Context())
	if err != nil {
		s.logger.Warn("listing projects for create-advice", "error", err)
	}
	names := make([]string, 0, len(projects)+1)
	for name := range projects {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if !slices.Contains(names, "gardenio") {
		names = append([]string{"gardenio"}, names...)
	}
	view.Projects = names

	s.renderPartial(w, r, "templates/partials/create_advice.html", "create_advice_sheet", map[string]any{
		"Sheet": view,
	})
}

// handleCreateAdvice validates the form, creates an advice bead, and
// htmx-redirects the browser to the new bead's page on success. On
// validation/server error the form is re-rendered with the error.
func (s *Server) handleCreateAdvice(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}

	title := strings.TrimSpace(r.PostForm.Get("title"))
	description := strings.TrimSpace(r.PostForm.Get("description"))
	priorityStr := strings.TrimSpace(r.PostForm.Get("priority"))
	scopeKind := strings.TrimSpace(r.PostForm.Get("scope"))
	scopeValue := strings.TrimSpace(r.PostForm.Get("scope_value"))

	// kd advice add semantics: title falls back to description when blank.
	if title == "" {
		title = description
	}
	if description == "" {
		s.renderCreateAdviceError(w, r, "Advice text is required.", http.StatusBadRequest)
		return
	}

	priority := 2
	switch priorityStr {
	case "0":
		priority = 0
	case "1":
		priority = 1
	case "3":
		priority = 3
	case "4":
		priority = 4
	}

	if !slices.Contains(adviceScopeKinds, scopeKind) {
		scopeKind = "global"
	}
	labels, err := buildAdviceLabels(scopeKind, scopeValue)
	if err != nil {
		s.renderCreateAdviceError(w, r, err.Error(), http.StatusBadRequest)
		return
	}

	req := beadsapi.CreateBeadRequest{
		Title:       title,
		Type:        "advice",
		Kind:        "advice",
		Description: description,
		Labels:      labels,
		Priority:    priority,
		CreatedBy:   email,
	}
	id, err := s.daemon.CreateBead(r.Context(), req)
	if err != nil {
		s.logger.Error("creating advice bead", "error", err, "labels", labels)
		s.renderCreateAdviceError(w, r, "Failed to create advice: "+err.Error(), http.StatusBadGateway)
		return
	}

	// Redirect the htmx client to the new bead's page. Using HX-Redirect
	// (rather than swapping a success partial) matches the iOS flow where
	// AdviceEditorView dismisses to the bead detail view on save.
	dest := s.basePath + "/" + id
	w.Header().Set("HX-Redirect", dest)
	w.WriteHeader(http.StatusOK)
	s.renderPartial(w, r, "templates/partials/create_advice.html", "create_advice_result", map[string]any{
		"Result": createAdviceResultView{BeadID: id, Title: title},
	})
}

// renderCreateAdviceError re-renders the modal with an inline error so the
// operator can correct the form and resubmit without losing context.
func (s *Server) renderCreateAdviceError(w http.ResponseWriter, r *http.Request, msg string, status int) {
	w.WriteHeader(status)
	s.renderPartial(w, r, "templates/partials/create_advice.html", "create_advice_result", map[string]any{
		"Result": createAdviceResultView{Error: msg},
	})
}

// buildAdviceLabels turns a scope kind + value into the label list the
// daemon validates. Returns an error when a non-global scope is missing
// its required value.
func buildAdviceLabels(kind, value string) ([]string, error) {
	switch kind {
	case "", "global":
		return []string{"global"}, nil
	case "project", "role", "agent":
		if value == "" {
			return nil, &createAdviceErr{msg: "Scope value is required for " + kind + " scope."}
		}
		return []string{kind + ":" + value}, nil
	}
	return nil, &createAdviceErr{msg: "Unknown scope: " + kind}
}

type createAdviceErr struct{ msg string }

func (e *createAdviceErr) Error() string { return e.msg }
