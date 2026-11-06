package main

import (
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// taintPresets are the preset scrub-policy starting points the "New taint"
// form offers (docs/adr/taints.md §10.2). Mirrors kbeads'
// model.PresetProduction / PresetInternal / PresetScratch — hardcoded here
// since the viewer module cannot import the daemon's internal/model package.
var taintPresets = []struct {
	Value string
	Label string
}{
	{Value: "production", Label: "Production (strict — gate, approvers required)"},
	{Value: "internal", Label: "Internal (moderate — gate, auto-approve capped)"},
	{Value: "scratch", Label: "Scratch (permissive — trace-only)"},
}

// taintRowView is one row of the /taints registry (docs/adr/taints.md §9.1,
// task gd-QXSHroFXwB).
type taintRowView struct {
	Name              string
	OwnerTeam         string
	Description       string
	BeadCountDisplay  string // "—" when the viewer holds no grant for this taint
	OpenScrubRequests int
	LastScrubDisplay  string
	Retired           bool
}

// handleTaintsRegistry renders GET /taints — the taint registry every
// viewer sees (docs/adr/taints.md §9.1). Bead count, open scrub requests
// and last-scrub are computed by the daemon fresh on every call under the
// caller's own read scope, so this handler does no caching of its own.
func (s *Server) handleTaintsRegistry(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Rows":     []taintRowView{},
		"CanAdmin": viewerCanAdmin(r),
	}

	taints, err := s.daemon.ListTaints(r.Context())
	if err != nil {
		s.logger.Error("listing taints", "error", err)
		data["Error"] = "Failed to load the taints registry."
		s.render(w, r, "taints.html", data)
		return
	}

	rows := make([]taintRowView, 0, len(taints))
	for _, t := range taints {
		rows = append(rows, taintRowView{
			Name:              t.Name,
			OwnerTeam:         t.OwnerTeam,
			Description:       t.Description,
			BeadCountDisplay:  beadCountDisplay(t.BeadCount),
			OpenScrubRequests: t.OpenScrubRequests,
			LastScrubDisplay:  lastScrubDisplay(t.LastScrubAt),
			Retired:           t.RetiredAt != nil,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	data["Rows"] = rows

	s.render(w, r, "taints.html", data)
}

// beadCountDisplay renders a taint's bead count, or the dash the registry
// acceptance criteria requires when the viewer holds no grant for that
// taint (nil, distinguishing "denied" from a genuine zero — see
// kbeads' taintBeadCount).
func beadCountDisplay(n *int) string {
	if n == nil {
		return "—"
	}
	return strconv.Itoa(*n)
}

// lastScrubDisplay renders a taint's last-approved-scrub timestamp as a
// relative "ago" string, or "never" when no scrub has ever been approved
// under the viewer's own read scope.
func lastScrubDisplay(t *time.Time) string {
	if t == nil {
		return "never"
	}
	return fmtTimeAgo(*t, time.Now())
}

// createTaintSheetView is the data passed to the "New taint" modal partial.
type createTaintSheetView struct {
	Presets []struct {
		Value string
		Label string
	}
}

// createTaintResultView is the data passed to the result partial after a
// POST /taints. On error, Error is populated and the form is re-rendered as
// the result so the admin can fix and retry.
type createTaintResultView struct {
	Name  string
	Error string
}

// handleCreateTaintSheet renders the modal that the "New taint" button
// opens. Admin-only (route-gated); fetched via htmx and swapped into the
// shared modal host, mirroring /advice/new.
func (s *Server) handleCreateTaintSheet(w http.ResponseWriter, r *http.Request) {
	s.renderPartial(w, r, "templates/partials/create_taint.html", "create_taint_sheet", map[string]any{
		"Sheet": createTaintSheetView{Presets: taintPresets},
	})
}

// handleCreateTaintSubmit handles POST /taints — the admin "New taint"
// action (docs/adr/taints.md §9.1, task gd-QXSHroFXwB acceptance criteria:
// "'New taint' is visible only to admins and drives p1-taint-crud-api").
// Admin-only (route-gated). The daemon's POST /v1/taints requires direct
// OIDC RoleAdmin auth; s.daemon.CreateTaint always authenticates as the
// calling human via the per-request user bearer operatorMiddleware already
// stamps onto the request context, so a caller reached through an edge with
// no forwardable bearer (e.g. AVA) fails closed with a clear message rather
// than a bare daemon 403.
func (s *Server) handleCreateTaintSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}

	name := strings.TrimSpace(r.PostForm.Get("name"))
	ownerTeam := strings.TrimSpace(r.PostForm.Get("owner_team"))
	preset := strings.TrimSpace(r.PostForm.Get("preset"))

	if name == "" {
		s.renderCreateTaintError(w, r, "Name is required.", http.StatusBadRequest)
		return
	}
	if ownerTeam == "" {
		s.renderCreateTaintError(w, r, "Owner team is required.", http.StatusBadRequest)
		return
	}

	req := beadsapi.CreateTaintRequest{
		Name:      name,
		OwnerTeam: ownerTeam,
		Preset:    preset,
	}
	created, err := s.daemon.CreateTaint(r.Context(), req)
	if err != nil {
		email := oidcauth.EmailFromContext(r.Context())
		if errors.Is(err, beadsapi.ErrTaintCreateRequiresUserBearer) {
			s.logger.Warn("create taint: no forwardable user bearer", "operator", email)
			s.renderCreateTaintError(w, r, "Your session has no forwardable identity token — taint creation requires signing in directly (not available through this access path).", http.StatusForbidden)
			return
		}
		s.logger.Error("creating taint", "error", err, "name", name)
		s.renderCreateTaintError(w, r, "Failed to create taint: "+err.Error(), http.StatusBadGateway)
		return
	}

	dest := s.basePath + "/taints"
	w.Header().Set("HX-Redirect", dest)
	w.WriteHeader(http.StatusOK)
	s.renderPartial(w, r, "templates/partials/create_taint.html", "create_taint_result", map[string]any{
		"Result": createTaintResultView{Name: created.Name},
	})
}

// renderCreateTaintError re-renders the modal with an inline error so the
// admin can correct the form and resubmit without losing context.
func (s *Server) renderCreateTaintError(w http.ResponseWriter, r *http.Request, msg string, status int) {
	w.WriteHeader(status)
	s.renderPartial(w, r, "templates/partials/create_taint.html", "create_taint_result", map[string]any{
		"Result": createTaintResultView{Error: msg},
	})
}
