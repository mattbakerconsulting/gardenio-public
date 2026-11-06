package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// viewsListLimit caps the number of saved views pulled from the daemon.
// Saved views are a per-user concept; even pooled across operators the
// realistic count is far under this cap. The truncation banner surfaces
// overflow if it ever happens.
const viewsListLimit = 200

// viewListLimit caps the number of beads rendered when showing a single
// saved view. Mirrors activeListLimit so a saved query can never accidentally
// blow the page up.
const viewListLimit = 500

// viewRowView is one row on the /views index page.
type viewRowView struct {
	ID        string
	Title     string
	Owner     string
	Source    string
	Summary   string
	Ago       string
	OwnedByMe bool
}

// savedViewSpec is the parsed filter specification stored on a view bead's
// fields. It maps cleanly onto beadsapi.ListBeadsQuery.
type savedViewSpec struct {
	Source   string   // "active" | "decisions" | "" (generic)
	Project  string   // project name, optional
	Types    []string // bead types
	Statuses []string // bead statuses
	Labels   []string // additional label filters
	Search   string   // ?q=
	Sort     string   // sort field
	Columns  []string // optional column-set names
}

// toQuery returns a ListBeadsQuery built from the spec. Defaults that the
// list pages apply (e.g. status=open for decisions) are folded in here so a
// view created from /decisions reproduces the same result set.
func (s savedViewSpec) toQuery() beadsapi.ListBeadsQuery {
	q := beadsapi.ListBeadsQuery{
		Types:    append([]string(nil), s.Types...),
		Statuses: append([]string(nil), s.Statuses...),
		Labels:   append([]string(nil), s.Labels...),
		Search:   s.Search,
		Sort:     s.Sort,
		Limit:    viewListLimit,
	}
	if s.Project != "" {
		q.Labels = append(q.Labels, "project:"+s.Project)
	}
	if len(q.Statuses) == 0 {
		switch s.Source {
		case "active":
			q.Statuses = []string{"in_progress"}
		case "decisions":
			q.Statuses = []string{"open"}
		}
	}
	if len(q.Types) == 0 && s.Source == "decisions" {
		q.Types = []string{"decision"}
	}
	if q.Sort == "" {
		switch s.Source {
		case "decisions":
			q.Sort = "-created_at"
		case "active":
			q.Sort = "priority"
		}
	}
	return q
}

// toBeadFields encodes the spec into bead fields (string-valued).
func (s savedViewSpec) toBeadFields() map[string]string {
	f := map[string]string{}
	if s.Source != "" {
		f["source"] = s.Source
	}
	if s.Project != "" {
		f["project"] = s.Project
	}
	if len(s.Types) > 0 {
		f["types"] = strings.Join(s.Types, ",")
	}
	if len(s.Statuses) > 0 {
		f["statuses"] = strings.Join(s.Statuses, ",")
	}
	if len(s.Labels) > 0 {
		f["labels_filter"] = strings.Join(s.Labels, ",")
	}
	if s.Search != "" {
		f["search"] = s.Search
	}
	if s.Sort != "" {
		f["sort"] = s.Sort
	}
	if len(s.Columns) > 0 {
		f["columns"] = strings.Join(s.Columns, ",")
	}
	return f
}

// parseViewSpec decodes a saved-view bead's fields into a spec.
func parseViewSpec(fields map[string]string) savedViewSpec {
	splitCSV := func(v string) []string {
		v = strings.TrimSpace(v)
		if v == "" {
			return nil
		}
		parts := strings.Split(v, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	return savedViewSpec{
		Source:   fields["source"],
		Project:  fields["project"],
		Types:    splitCSV(fields["types"]),
		Statuses: splitCSV(fields["statuses"]),
		Labels:   splitCSV(fields["labels_filter"]),
		Search:   fields["search"],
		Sort:     fields["sort"],
		Columns:  splitCSV(fields["columns"]),
	}
}

// summarize describes a spec in a short, human-readable string for list rows.
func (s savedViewSpec) summarize() string {
	parts := make([]string, 0, 5)
	if len(s.Types) > 0 {
		parts = append(parts, "type="+strings.Join(s.Types, "/"))
	}
	if len(s.Statuses) > 0 {
		parts = append(parts, "status="+strings.Join(s.Statuses, "/"))
	}
	if s.Project != "" {
		parts = append(parts, "project="+s.Project)
	}
	if len(s.Labels) > 0 {
		parts = append(parts, "labels="+strings.Join(s.Labels, "/"))
	}
	if s.Search != "" {
		parts = append(parts, "q="+s.Search)
	}
	if s.Sort != "" {
		parts = append(parts, "sort="+s.Sort)
	}
	return strings.Join(parts, " · ")
}

// viewOwnerLabel encodes ownership on a view bead via a label so the index
// page can filter without an extra bead-by-bead lookup.
func viewOwnerLabel(email string) string {
	return "view:owner:" + email
}

// viewOwnerFromBead pulls the owner email out of a view bead. Falls back to
// CreatedBy so beads created from older code (or via kd CLI) still attribute
// correctly.
func viewOwnerFromBead(b *beadsapi.BeadDetail) string {
	for _, l := range b.Labels {
		if strings.HasPrefix(l, "view:owner:") {
			return strings.TrimPrefix(l, "view:owner:")
		}
	}
	return b.CreatedBy
}

// handleViewsList renders /views — saved view beads owned by the current
// operator, plus a short summary of the filter each one captures.
func (s *Server) handleViewsList(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	data := map[string]any{
		"OperatorEmail": email,
		"Views":         []viewRowView{},
		"Created":       r.URL.Query().Get("created"),
		"Deleted":       r.URL.Query().Get("deleted") == "1",
	}

	q := beadsapi.ListBeadsQuery{
		Types:    []string{"view"},
		Statuses: []string{"open"},
		Sort:     "-created_at",
		Limit:    viewsListLimit,
	}
	res, err := s.daemon.ListBeadsFiltered(r.Context(), q)
	if err != nil {
		s.logger.Error("listing saved views", "error", err)
		data["Error"] = "Failed to load saved views."
		s.render(w, r, "views_index.html", data)
		return
	}

	now := time.Now()
	rows := make([]viewRowView, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		owner := viewOwnerFromBead(b)
		spec := parseViewSpec(b.Fields)
		rows = append(rows, viewRowView{
			ID:        b.ID,
			Title:     b.Title,
			Owner:     owner,
			Source:    spec.Source,
			Summary:   spec.summarize(),
			Ago:       fmtTimeAgo(b.CreatedAt, now),
			OwnedByMe: email != "" && owner == email,
		})
	}
	// Stable sort: my views first, then everyone else's. Within each group,
	// preserve the daemon's -created_at order (newest first).
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].OwnedByMe != rows[j].OwnedByMe {
			return rows[i].OwnedByMe
		}
		return false
	})

	data["Views"] = rows
	data["Truncated"] = res.Total > len(res.Beads)
	data["Limit"] = viewsListLimit
	s.render(w, r, "views_index.html", data)
}

// handleViewShow renders /views/{id} — re-runs the saved query and renders
// the matching beads as an active-style list.
func (s *Server) handleViewShow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing view id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "View not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting view bead", "id", id, "error", err)
		http.Error(w, "Failed to load view", http.StatusInternalServerError)
		return
	}
	if bead.Type != "view" {
		http.Error(w, "Bead is not a saved view", http.StatusBadRequest)
		return
	}

	spec := parseViewSpec(bead.Fields)
	owner := viewOwnerFromBead(bead)
	data := map[string]any{
		"View": map[string]any{
			"ID":        bead.ID,
			"Title":     bead.Title,
			"Owner":     owner,
			"Source":    spec.Source,
			"Summary":   spec.summarize(),
			"OwnedByMe": email != "" && owner == email,
		},
		"OperatorEmail": email,
		"Beads":         []activeBeadView{},
	}

	// Resolve the viewer's pinned set before the saved-view query so the
	// extra user-bead lookup doesn't reorder the daemon's last-query state
	// the view query result depends on.
	pinnedSet := s.viewerPinnedSet(r.Context(), email)

	res, err := s.daemon.ListBeadsFiltered(r.Context(), spec.toQuery())
	if err != nil {
		s.logger.Error("running saved view query", "view_id", id, "error", err)
		data["Error"] = "Failed to run saved view query."
		s.render(w, r, "view_show.html", data)
		return
	}

	// Per-user-homepage M2 §6 (ka-0000007787): populate operator slugs
	// before chip rendering so each row links to /u/{slug}.
	s.populateOperatedBySlugs(r.Context(), res.Beads)

	now := time.Now()
	rows := make([]activeBeadView, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		project := projectFromLabels(b.Labels)
		if project == "" {
			project = "untagged"
		}
		row := buildBeadActions(b, email, pinnedSet)
		row.IsRowPartial = true
		row.RowProject = project
		row.RowTitle = b.Title
		row.RowShortTitle = b.ShortTitle
		row.RowType = b.Type
		row.RowPriority = b.Priority
		row.ShowPriority = b.Priority <= 1
		row.RowAgo = fmtTimeAgo(b.UpdatedAt, now)
		rowChip := resolveOperatorChip(b.OperatedBy, b.OperatedByDisplay, b.OperatedByEmail, b.OperatedBySlug, b.CreatedBy, s.basePath, email)
		row.OperatorChipHTML = renderOperatorChip(rowChip)
		row.OperatorChipCompactHTML = renderOperatorChipCompact(rowChip)
		rows = append(rows, activeBeadView{
			ID:        b.ID,
			Title:     b.Title,
			Type:      b.Type,
			Priority:  b.Priority,
			Assignee:  b.Assignee,
			Project:   project,
			UpdatedAt: b.UpdatedAt,
			Ago:       fmtTimeAgo(b.UpdatedAt, now),
			Row:       row,
		})
	}
	data["Beads"] = rows
	data["Truncated"] = res.Total > len(res.Beads)
	data["Limit"] = viewListLimit
	s.render(w, r, "view_show.html", data)
}

// handleViewCreate creates a saved view bead from a form post. The form is
// expected from the "Save this view" dialog on /search; it collects a name
// plus the current query and filter values.
func (s *Server) handleViewCreate(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.PostForm.Get("name"))
	if name == "" {
		http.Error(w, "View name is required.", http.StatusBadRequest)
		return
	}
	if len(name) > 200 {
		http.Error(w, "View name is too long (max 200 characters).", http.StatusBadRequest)
		return
	}

	splitForm := func(key string) []string {
		// Accept either CSV in a single field or repeated form values.
		out := []string{}
		for _, v := range r.PostForm[key] {
			for _, p := range strings.Split(v, ",") {
				p = strings.TrimSpace(p)
				if p != "" {
					out = append(out, p)
				}
			}
		}
		return out
	}

	spec := savedViewSpec{
		Source:   strings.TrimSpace(r.PostForm.Get("source")),
		Project:  strings.TrimSpace(r.PostForm.Get("project")),
		Types:    splitForm("types"),
		Statuses: splitForm("statuses"),
		Labels:   splitForm("labels_filter"),
		Search:   strings.TrimSpace(r.PostForm.Get("search")),
		Sort:     strings.TrimSpace(r.PostForm.Get("sort")),
		Columns:  splitForm("columns"),
	}

	fieldsJSON, err := json.Marshal(spec.toBeadFields())
	if err != nil {
		s.logger.Error("marshalling view fields", "error", err)
		http.Error(w, "Failed to encode view.", http.StatusInternalServerError)
		return
	}

	req := beadsapi.CreateBeadRequest{
		Title:     name,
		Type:      "view",
		Kind:      "config",
		CreatedBy: email,
		Labels:    []string{viewOwnerLabel(email)},
		Fields:    fieldsJSON,
	}
	id, err := s.daemon.CreateBead(r.Context(), req)
	if err != nil {
		s.logger.Error("creating view bead", "error", err)
		http.Error(w, "Failed to save view.", http.StatusBadGateway)
		return
	}

	target := s.basePath + "/views/" + url.PathEscape(id)
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// handleViewDelete deletes a saved view bead. Owners may delete their own
// views; the form is POST (rather than DELETE) so plain HTML forms can
// trigger it.
func (s *Server) handleViewDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing view id", http.StatusBadRequest)
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
			http.Error(w, "View not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting view for delete", "id", id, "error", err)
		http.Error(w, "Failed to load view", http.StatusInternalServerError)
		return
	}
	if bead.Type != "view" {
		http.Error(w, "Bead is not a saved view", http.StatusBadRequest)
		return
	}
	if owner := viewOwnerFromBead(bead); owner != "" && owner != email {
		http.Error(w, "You can only delete views you own.", http.StatusForbidden)
		return
	}
	if err := s.daemon.DeleteBead(r.Context(), id); err != nil {
		s.logger.Error("deleting view bead", "id", id, "error", err)
		http.Error(w, "Failed to delete view.", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, s.basePath+"/views?deleted=1", http.StatusSeeOther)
}
