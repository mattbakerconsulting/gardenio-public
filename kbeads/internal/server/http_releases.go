package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
)

// The release routes (gardenio/docs/releases-design.md §3, plan gd-UZKZ9H25UD
// step 2). HTTP-only, like escalations and teams: no proto change. The record,
// its guards and the deployment write live in releases.go.
//
//   POST /v1/releases                    handleCreateRelease   kd release create
//   GET  /v1/releases                    handleListReleases    kd release list
//   GET  /v1/releases/{id}               handleGetRelease      kd release show
//   POST /v1/releases/{id}/deployments   handleDeployRelease   kd release deploy
//   POST /v1/releases/{id}/contents      handleAppendReleaseContents
//   POST /v1/releases/repair-deployments handleRepairReleaseDeployments
//                                                              kd release repair-deployments
//
// Writes need the release writer check (releaseWriterDenial); reads are
// scoped like any bead read.

// releaseCreateRequest is the body of POST /v1/releases. status and
// deployments are not accepted: the daemon derives the one and the deploy
// route writes the other. With no tag it opens the project's next release
// (releases_open.go), which takes project and contents only.
type releaseCreateRequest struct {
	Project          string          `json:"project"`
	Tag              string          `json:"tag"`
	SHA              string          `json:"sha,omitempty"`
	Previous         string          `json:"previous,omitempty"`
	PreviousTag      string          `json:"previous_tag,omitempty"`
	CutAt            string          `json:"cut_at,omitempty"`
	CutBy            string          `json:"cut_by,omitempty"`
	Source           string          `json:"source,omitempty"`
	BuiltAt          string          `json:"built_at,omitempty"`
	Images           json.RawMessage `json:"images,omitempty"`
	Contents         json.RawMessage `json:"contents,omitempty"`
	ContentsComplete *bool           `json:"contents_complete,omitempty"`
}

// writeReleaseError maps a release write error to its response.
func (s *BeadsServer) writeReleaseError(w http.ResponseWriter, r *http.Request, op string, err error) {
	var denial *AuthzDenial
	var ie inputError
	var conflict releaseConflict
	switch {
	case errors.As(err, &denial):
		s.auditDenial(r, denial, "")
		writeAuthzDenial(w, denial)
	case errors.As(err, &ie):
		writeError(w, http.StatusBadRequest, ie.Error())
	case errors.As(err, &conflict):
		writeError(w, http.StatusConflict, conflict.Error())
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "release not found")
	default:
		writeServerError(r.Context(), w, op, err, "internal server error")
	}
}

// handleCreateRelease handles POST /v1/releases: a release of project at tag,
// through createBead (applyReleaseGuard). A second create of the same
// (project, tag) returns the first release with 200; a new one is 201. With
// no tag it opens the project's next release; a second open one is a 409.
func (s *BeadsServer) handleCreateRelease(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if denial := s.releaseWriterDenial(ctx, "create"); denial != nil {
		s.writeReleaseError(w, r, "handleCreateRelease", denial)
		return
	}
	bodyBytes, ok := readBoundedBody(w, r)
	if !ok {
		return
	}
	var body releaseCreateRequest
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	body.Project, body.Tag = strings.TrimSpace(body.Project), strings.TrimSpace(body.Tag)
	if body.Project == "" {
		writeError(w, http.StatusBadRequest, "project is required")
		return
	}

	fields := map[string]any{"project": body.Project}
	title, shortTitle := body.Project+" next release", "next release"
	if body.Tag != "" {
		fields["tag"] = body.Tag
		title, shortTitle = body.Project+" "+body.Tag, "release "+body.Tag
	}
	for key, v := range map[string]string{
		"sha": body.SHA, "previous": body.Previous, "previous_tag": body.PreviousTag,
		"cut_at": body.CutAt, "cut_by": body.CutBy, "source": body.Source, "built_at": body.BuiltAt,
	} {
		if v = strings.TrimSpace(v); v != "" {
			fields[key] = v
		}
	}
	for key, raw := range map[string]json.RawMessage{"images": body.Images, "contents": body.Contents} {
		if len(raw) > 0 && string(raw) != "null" {
			fields[key] = raw
		}
	}
	if body.ContentsComplete != nil {
		fields["contents_complete"] = *body.ContentsComplete
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		writeServerError(ctx, w, "handleCreateRelease", err, "failed to encode fields")
		return
	}

	start := time.Now().UTC()
	bead, err := s.createBead(ctx, createBeadInput{
		Title:      title,
		ShortTitle: shortTitle,
		Type:       string(model.TypeRelease),
		Labels:     []string{"project:" + body.Project},
		Fields:     encoded,
	})
	if err != nil {
		s.writeReleaseError(w, r, "handleCreateRelease", err)
		return
	}
	code := http.StatusCreated
	if bead.CreatedAt.Before(start.Add(-time.Second)) {
		code = http.StatusOK
	}
	writeJSON(w, code, releaseViewOf(bead))
}

// handleListReleases handles GET /v1/releases?project=&status=&limit=: the
// caller's readable releases, newest cut first.
func (s *BeadsServer) handleListReleases(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := model.BeadFilter{
		Type:  []model.BeadType{model.TypeRelease},
		Limit: maxListBeadsLimit,
	}
	if p := strings.TrimSpace(q.Get("project")); p != "" {
		filter.Labels = []string{"project:" + p}
	}
	limit := 50
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	limit = clampListBeadsLimit(limit, 50)
	var statuses []string
	if v := strings.TrimSpace(q.Get("status")); v != "" {
		statuses = strings.Split(v, ",")
	}
	filter = s.scopeListFilter(r.Context(), filter)

	beads, _, err := s.store.ListBeads(r.Context(), filter)
	if err != nil {
		writeServerError(r.Context(), w, "handleListReleases", err, "failed to list releases")
		return
	}
	sortReleasesNewestFirst(beads)
	skippedBy := releaseSkippedBy(beads)
	out := make([]releaseView, 0, min(len(beads), limit))
	for _, b := range beads {
		v := releaseViewOf(b)
		v.SkippedBy = skippedBy[b.ID]
		if len(statuses) > 0 && !containsTrimmed(statuses, v.Status) {
			continue
		}
		out = append(out, v)
		if len(out) == limit {
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"releases": out})
}

// containsTrimmed reports whether want is one of the comma-split values.
func containsTrimmed(values []string, want string) bool {
	for _, v := range values {
		if strings.TrimSpace(v) == want {
			return true
		}
	}
	return false
}

// handleGetRelease handles GET /v1/releases/{id}. {id} is the release bead's
// id, or its tag when ?project= names the project (resolveRelease).
func (s *BeadsServer) handleGetRelease(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	bead, err := s.resolveRelease(r, id)
	if err != nil {
		writeServerError(r.Context(), w, "handleGetRelease", err, "failed to get release")
		return
	}
	if bead == nil {
		writeError(w, http.StatusNotFound, "release not found")
		return
	}
	if !s.authorizeBeadRead(w, r, bead, "release not found") {
		return
	}
	writeJSON(w, http.StatusOK, s.releaseViewWithSkip(r.Context(), bead))
}

// resolveRelease finds the release {id} names: by tag when ?project= is set
// and the project has a release with that tag, else by bead id. nil when
// neither is a release.
func (s *BeadsServer) resolveRelease(r *http.Request, id string) (*model.Bead, error) {
	ctx := r.Context()
	if project := strings.TrimSpace(r.URL.Query().Get("project")); project != "" {
		b, err := s.lookupByIdempotencyKey(ctx, []string{"project:" + project}, "", releaseIdempotencyKey(project, id))
		if err != nil {
			return nil, err
		}
		if b != nil && b.Type == model.TypeRelease {
			return b, nil
		}
	}
	b, err := s.store.GetBead(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if b == nil || b.Type != model.TypeRelease {
		return nil, nil
	}
	return b, nil
}

// handleDeployRelease handles POST /v1/releases/{id}/deployments: the release
// ({id} or, with ?project=, its tag) runs on body.target from body.since
// (deployRelease). A release writer may report any release. A scoped
// principal is no release writer; it may report only a release of a land it
// owns (releaseDeployDenial), the Lands contract's reportDeployment, and its
// row is always via=contract.
func (s *BeadsServer) handleDeployRelease(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	writerDenial := s.releaseWriterDenial(ctx, releaseDeployAction)
	tenant := writerDenial != nil && isScopedPrincipal(ctx)
	if writerDenial != nil && !tenant {
		s.writeReleaseError(w, r, "handleDeployRelease", writerDenial)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	bodyBytes, ok := readBoundedBody(w, r)
	if !ok {
		return
	}
	var body releaseDeployRequest
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	rel, err := s.resolveRelease(r, id)
	if err != nil {
		writeServerError(ctx, w, "handleDeployRelease", err, "failed to get release")
		return
	}
	if rel == nil {
		writeError(w, http.StatusNotFound, "release not found")
		return
	}
	if tenant {
		// A release the caller cannot read answers as one that is absent.
		if !s.authorizeBeadRead(w, r, rel, "release not found") {
			return
		}
		if denial := releaseDeployDenial(ctx, rel); denial != nil {
			s.writeReleaseError(w, r, "handleDeployRelease", denial)
			return
		}
		// The contract body is {target, revision?, by?}: the server stamps
		// via and since, and kind and image_tag are not the tenant's to say.
		body.Via = "contract"
		body.Since = ""
		body.Kind = ""
		body.ImageTag = ""
	}
	bead, err := s.deployRelease(ctx, rel.ID, body)
	if err != nil {
		s.writeReleaseError(w, r, "handleDeployRelease", err)
		return
	}
	writeJSON(w, http.StatusOK, releaseViewOf(bead))
}

// handleAppendReleaseContents handles POST /v1/releases/{id}/contents with
// body {"contents": [row, ...]}: appends the rows to the open release ({id},
// or with ?project= its tag) that it does not already carry
// (appendReleaseContents). Release writers only, github-bridge's release
// recorder among them (its update grant on type release). The answer is the
// release, with X-Release-Contents-Added the number of rows written.
func (s *BeadsServer) handleAppendReleaseContents(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if denial := s.releaseWriterDenial(ctx, "update"); denial != nil {
		s.writeReleaseError(w, r, "handleAppendReleaseContents", denial)
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}
	bodyBytes, ok := readBoundedBody(w, r)
	if !ok {
		return
	}
	var body releaseContentsRequest
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	rel, err := s.resolveRelease(r, id)
	if err != nil {
		writeServerError(ctx, w, "handleAppendReleaseContents", err, "failed to get release")
		return
	}
	if rel == nil {
		writeError(w, http.StatusNotFound, "release not found")
		return
	}
	bead, added, err := s.appendReleaseContents(ctx, rel.ID, body.Contents)
	if err != nil {
		s.writeReleaseError(w, r, "handleAppendReleaseContents", err)
		return
	}
	if added > 0 {
		s.noteBoardedWork(ctx, bead)
	}
	w.Header().Set("X-Release-Contents-Added", strconv.Itoa(added))
	writeJSON(w, http.StatusOK, releaseViewOf(bead))
}

// releaseRepairRequest is the body of POST /v1/releases/repair-deployments.
type releaseRepairRequest struct {
	Project string `json:"project"`
	DryRun  bool   `json:"dry_run,omitempty"`
}

// handleRepairReleaseDeployments handles POST /v1/releases/repair-deployments:
// rebuild a project's deployment history from its rows
// (repairReleaseDeployments). Release writers only, a dry run included: the
// answer lists every row of every release that changes.
func (s *BeadsServer) handleRepairReleaseDeployments(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if denial := s.releaseWriterDenial(ctx, releaseDeployAction); denial != nil {
		s.writeReleaseError(w, r, "handleRepairReleaseDeployments", denial)
		return
	}
	bodyBytes, ok := readBoundedBody(w, r)
	if !ok {
		return
	}
	var body releaseRepairRequest
	if err := json.Unmarshal(bodyBytes, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	project := strings.TrimSpace(body.Project)
	if project == "" {
		writeError(w, http.StatusBadRequest, "project is required")
		return
	}
	res, err := s.repairReleaseDeployments(ctx, project, body.DryRun)
	if err != nil {
		s.writeReleaseError(w, r, "handleRepairReleaseDeployments", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
