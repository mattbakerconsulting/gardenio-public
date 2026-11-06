package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// The releases JSON (plan gd-UZKZ9H25UD step 6; gardenio/docs/releases-design.md
// §3.5): a project's releases for the Release page (step 8), which reads these
// routes and does not register them.
//
//	GET /api/releases?project=<p>[&limit=N&cursor=C]   {"items":[Release...], "next_cursor":"..."}
//	GET /api/releases/{id}[?project=<p>]                one Release; {id} is the bead id, or the tag with ?project=
//
// Both read the daemon's GET /v1/releases[/{id}] on the request context, so the
// daemon authenticates the signed-in user and narrows the read to their
// projects (both kbeads routes are classFiltered); the routes are routeScoped.
// A project the caller cannot read, or that does not exist, lists no items.
//
// The daemon's releaseView is reshaped, not passed through: every empty
// optional field is left out (never null), contents and deployments are always
// arrays, times are RFC 3339 UTC whole seconds (an unparseable one is dropped),
// a contents row with no PR number is dropped, and each row carries the titles
// of its step and plan beads (and the plan's status) from one batch read. A
// failed title read leaves the titles out; it never fails the request. The
// single read also names the release that follows this one (next, next_tag):
// the release of the same project whose previous_tag is this tag.

const (
	// releasesDefaultLimit / releasesMaxLimit bound ?limit= on the list.
	releasesDefaultLimit = 50
	releasesMaxLimit     = 200
	// releasesScanMax is the most releases one daemon read asks for (the
	// daemon's own list cap). The daemon has no offset, so a page at offset o
	// reads o+limit+1 and slices; a cursor past this is the end of the list.
	releasesScanMax = 10000
	// releasesNextScan is how many of the project's newest releases the single
	// read scans for the one that follows it. A release further back than
	// this answers without next.
	releasesNextScan = 1000
)

// releaseJSON is one release as the viewer serves it.
type releaseJSON struct {
	ID          string                  `json:"id"`
	Tag         string                  `json:"tag"`
	Status      string                  `json:"status"`
	SHA         string                  `json:"sha,omitempty"`
	CutAt       string                  `json:"cut_at,omitempty"`
	Contents    []releaseContentJSON    `json:"contents"`
	Deployments []releaseDeploymentJSON `json:"deployments"`
	Project     string                  `json:"project"`
	CutBy       string                  `json:"cut_by,omitempty"`
	Previous    string                  `json:"previous,omitempty"`
	PreviousTag string                  `json:"previous_tag,omitempty"`
	BuiltAt     string                  `json:"built_at,omitempty"`
	// Next and NextTag are the release that follows this one; only the single
	// read fills them, and the newest release has none.
	Next    string `json:"next,omitempty"`
	NextTag string `json:"next_tag,omitempty"`
}

// releaseContentJSON is one merged PR the release carries.
type releaseContentJSON struct {
	PR         int    `json:"pr"`
	Title      string `json:"title"`
	URL        string `json:"url"`
	Step       string `json:"step,omitempty"`
	Plan       string `json:"plan,omitempty"`
	MergedAt   string `json:"merged_at,omitempty"`
	Task       string `json:"task,omitempty"`
	MRBead     string `json:"mr_bead,omitempty"`
	Train      int    `json:"train,omitempty"`
	StepTitle  string `json:"step_title,omitempty"`
	PlanTitle  string `json:"plan_title,omitempty"`
	PlanStatus string `json:"plan_status,omitempty"`
}

// releaseDeploymentJSON is one place the release ran or runs; Until is empty
// while it runs there.
type releaseDeploymentJSON struct {
	Target   string `json:"target"`
	Kind     string `json:"kind,omitempty"`
	Since    string `json:"since,omitempty"`
	Until    string `json:"until,omitempty"`
	By       string `json:"by,omitempty"`
	Revision string `json:"revision,omitempty"`
	Via      string `json:"via,omitempty"`
	ImageTag string `json:"image_tag,omitempty"`
}

func (s *Server) registerReleasesAPIRoutes(mux routeRegistrar) {
	mux.HandleFunc("GET /api/releases", s.handleReleasesList)
	mux.HandleFunc("GET /api/releases/{id}", s.handleReleaseGet)
}

func (s *Server) handleReleasesList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	project := strings.TrimSpace(q.Get("project"))
	if project == "" {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "project is required"})
		return
	}
	limit := releasesDefaultLimit
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > releasesMaxLimit {
			writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "limit must be 1-" + strconv.Itoa(releasesMaxLimit)})
			return
		}
		limit = n
	}
	offset, ok := decodeReleasesCursor(q.Get("cursor"))
	if !ok {
		writeJSONStatus(w, http.StatusBadRequest, map[string]string{"error": "bad cursor"})
		return
	}

	out := struct {
		Items      []releaseJSON `json:"items"`
		NextCursor string        `json:"next_cursor,omitempty"`
	}{Items: []releaseJSON{}}
	if offset < releasesScanMax {
		// One more than the page for the open release, which is listed first
		// and left out here.
		rels, err := s.daemon.ListReleases(r.Context(), project, "", min(offset+limit+2, releasesScanMax))
		if err != nil {
			s.logger.Warn("releases list", "project", project, "error", err)
			writeJSONStatus(w, http.StatusBadGateway, map[string]string{"error": "listing releases: " + err.Error()})
			return
		}
		rels = beadsapi.CutReleases(rels)
		end := min(offset+limit, len(rels))
		for _, rel := range rels[min(offset, end):end] {
			out.Items = append(out.Items, releaseJSONOf(rel))
		}
		if len(rels) > offset+limit {
			out.NextCursor = encodeReleasesCursor(offset + limit)
		}
	}
	s.fillReleaseTitles(r.Context(), out.Items)
	w.Header().Set("Cache-Control", "no-store")
	writeJSONStatus(w, http.StatusOK, out)
}

func (s *Server) handleReleaseGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	rel, err := s.daemon.GetRelease(r.Context(), id, project)
	var apiErr *beadsapi.APIError
	switch {
	case errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.StatusCode == http.StatusForbidden):
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "release not found"})
		return
	case err != nil:
		s.logger.Warn("release read", "id", id, "project", project, "error", err)
		writeJSONStatus(w, http.StatusBadGateway, map[string]string{"error": "reading release: " + err.Error()})
		return
	case rel == nil || (project != "" && rel.Project != project):
		// The daemon falls back to the bead id when no tag matches, so an id
		// of another project's release is not this project's.
		writeJSONStatus(w, http.StatusNotFound, map[string]string{"error": "release not found"})
		return
	}

	out := releaseJSONOf(*rel)
	if rel.Tag != "" {
		if next, err := s.daemon.ListReleases(r.Context(), rel.Project, "", releasesNextScan); err != nil {
			s.logger.Warn("release next", "id", rel.ID, "error", err)
		} else {
			// Newest first: the last match is the nearest successor.
			for _, n := range next {
				if n.PreviousTag == rel.Tag && n.ID != rel.ID {
					out.Next, out.NextTag = n.ID, n.Tag
				}
			}
		}
	}
	items := []releaseJSON{out}
	s.fillReleaseTitles(r.Context(), items)
	w.Header().Set("Cache-Control", "no-store")
	writeJSONStatus(w, http.StatusOK, items[0])
}

// releaseJSONOf reshapes a daemon release (see the file comment).
func releaseJSONOf(rel beadsapi.Release) releaseJSON {
	out := releaseJSON{
		ID:          rel.ID,
		Tag:         rel.Tag,
		Status:      rel.Status,
		SHA:         rel.SHA,
		CutAt:       releaseTime(rel.CutAt),
		Contents:    []releaseContentJSON{},
		Deployments: []releaseDeploymentJSON{},
		Project:     rel.Project,
		CutBy:       rel.CutBy,
		Previous:    rel.Previous,
		PreviousTag: rel.PreviousTag,
		BuiltAt:     releaseTime(rel.BuiltAt),
	}
	// Rows are decoded one by one so a malformed row loses only itself.
	var rows []json.RawMessage
	_ = json.Unmarshal(rel.Contents, &rows)
	for _, raw := range rows {
		var row releaseContentRow
		if json.Unmarshal(raw, &row) != nil || row.PR <= 0 {
			continue
		}
		out.Contents = append(out.Contents, releaseContentJSON{
			PR:       row.PR,
			Title:    row.Title,
			URL:      row.URL,
			Step:     row.Step,
			Plan:     row.Plan,
			MergedAt: releaseTime(row.MergedAt),
			Task:     row.Task,
			MRBead:   row.MRBead,
			Train:    max(row.Train, 0),
		})
	}
	for _, d := range rel.Deployments {
		out.Deployments = append(out.Deployments, releaseDeploymentJSON{
			Target:   d.Target,
			Kind:     d.Kind,
			Since:    releaseTime(d.Since),
			Until:    releaseTime(d.Until),
			By:       d.By,
			Revision: d.Revision,
			Via:      d.Via,
			ImageTag: d.ImageTag,
		})
	}
	return out
}

// fillReleaseTitles sets each contents row's step_title, plan_title and
// plan_status from one batch read of the distinct step and plan beads across
// items. A failed read, or a bead the read does not return, leaves them out.
func (s *Server) fillReleaseTitles(ctx context.Context, items []releaseJSON) {
	seen := map[string]bool{}
	var ids []string
	for _, it := range items {
		for _, c := range it.Contents {
			for _, id := range []string{c.Step, c.Plan} {
				if id != "" && !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
			}
		}
	}
	if len(ids) == 0 {
		return
	}
	beads, err := s.daemon.BatchGetBeads(ctx, ids)
	if err != nil {
		s.logger.Warn("release titles", "beads", len(ids), "error", err)
		return
	}
	for i := range items {
		for j := range items[i].Contents {
			c := &items[i].Contents[j]
			if b := beads[c.Step]; c.Step != "" && b != nil {
				c.StepTitle = b.Title
			}
			if b := beads[c.Plan]; c.Plan != "" && b != nil {
				c.PlanTitle, c.PlanStatus = b.Title, b.Status
			}
		}
	}
}

// releaseTime is s as RFC 3339 UTC whole seconds, "" when it does not parse.
func releaseTime(s string) string {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	if err != nil {
		return ""
	}
	return t.UTC().Truncate(time.Second).Format(time.RFC3339)
}

// encodeReleasesCursor / decodeReleasesCursor are the list's opaque cursor:
// an offset into the newest-first list. An empty cursor is offset 0.
func encodeReleasesCursor(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("o" + strconv.Itoa(offset)))
}

func decodeReleasesCursor(c string) (int, bool) {
	if c == "" {
		return 0, true
	}
	raw, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimPrefix(string(raw), "o"))
	if err != nil || n < 0 || !strings.HasPrefix(string(raw), "o") {
		return 0, false
	}
	return n, true
}
