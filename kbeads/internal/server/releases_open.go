package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
)

// The open release: a project's next release before its cut (plan
// gd-lgN4U7X5Tb, step gd-La3evUVEbJ). It is a release bead with no tag yet,
// kept open, keyed "release:<project>/next" so a project has at most one, and
// stamped opened_at and opened_by. Merged PRs board it through POST
// /v1/releases/{id}/contents (appendReleaseContents). The cut writes tag, sha,
// cut_at, cut_by and source onto it once, in one PATCH (checkReleaseUpdate):
// the bead closes, its key becomes "release:<project>:<tag>" and its status is
// derived like any cut release's, which frees the project's next key for the
// following open release.

// releaseStatusOpen is the status of a release that has no tag yet.
const releaseStatusOpen = "open"

// releaseCutFields are written once onto an open release, by its cut.
var releaseCutFields = []string{"tag", "sha", "cut_at", "cut_by", "source"}

// releaseNextKey is the key of a project's open release.
func releaseNextKey(project string) string {
	return "release:" + project + "/next"
}

// releaseIsOpen reports whether a release's fields name no tag: an open
// release, not cut yet.
func releaseIsOpen(m map[string]any) bool {
	return releaseString(m, "tag") == ""
}

// applyOpenReleaseGuard completes a create with no tag as the project's open
// release (applyReleaseGuard has checked project and its label). It refuses a
// second open release of the project (409 naming it) and every field the cut,
// a build or a deployment writes, stamps opened_at, opened_by and status, and
// leaves the bead open.
func (s *BeadsServer) applyOpenReleaseGuard(ctx context.Context, bead *model.Bead, m map[string]any, project string, projBead *model.Bead) error {
	existing, err := s.lookupByIdempotencyKey(ctx, []string{"project:" + project}, "", releaseNextKey(project))
	if err != nil {
		return err
	}
	if existing != nil {
		return releaseConflict(fmt.Sprintf("release: project %q already has an open release, %s; merged PRs board it, and its cut opens the next", project, existing.ID))
	}
	for _, key := range slices.Concat(releaseCutFields[1:], []string{"status", "deployments", "built_at", "images", "previous", "previous_tag", "contents_complete"}) {
		if v, ok := m[key]; ok && v != nil {
			return inputError(fmt.Sprintf("release: an open release has no %s; create it with project only, or create a cut release with a tag", key))
		}
	}
	if err := validateReleaseRows(m, "contents"); err != nil {
		return err
	}
	m["opened_at"] = time.Now().UTC().Format(time.RFC3339)
	m["opened_by"] = releaseActor(ctx)
	m["status"] = releaseStatusOpen
	m[idempotencyKeyField] = releaseNextKey(project)
	fields, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding release fields: %w", err)
	}
	bead.Fields = fields
	bead.Status = model.StatusOpen
	bead.ClosedAt = nil
	return nil
}

// releaseCut is what checkReleaseUpdate decided about a PATCH of a release:
// tag is the tag a cut wrote onto an open release, "" when the PATCH cuts
// nothing. contents is the open release's contents as the cut read them
// (JSON), and patchContents whether the PATCH itself writes contents; both
// feed checkReleaseCutRace.
type releaseCut struct {
	tag           string
	project       string
	contents      string
	patchContents bool
}

// checkOpenReleaseCut handles a PATCH of an open release (prior has no tag).
// Without a tag the cut's other fields are refused: they are written with it.
// With one it is the cut: the tag must match the project's
// release_tag_pattern and be no other release's, cut_at, cut_by and source
// are filled when absent, previous is linked, and the key moves to the tag's.
// The caller derives status and closes the bead.
func (s *BeadsServer) checkOpenReleaseCut(ctx context.Context, selfID string, m, pm map[string]any) (releaseCut, error) {
	project := releaseString(m, "project")
	tag := releaseString(m, "tag")
	if tag == "" {
		for _, key := range releaseCutFields[1:] {
			if releaseString(m, key) != releaseString(pm, key) {
				return releaseCut{}, inputError(fmt.Sprintf("release: %s is written with the tag, by the cut of an open release", key))
			}
		}
		return releaseCut{}, nil
	}
	_, src, err := s.releaseProject(ctx, project)
	if err != nil {
		return releaseCut{}, err
	}
	re, err := regexp.Compile(src.TagPattern)
	if err != nil {
		return releaseCut{}, inputError(fmt.Sprintf("release: project %q: %s", project, releaseTagPatternError(src.TagPattern)))
	}
	if !re.MatchString(tag) {
		return releaseCut{}, inputError(fmt.Sprintf("release: tag %q does not match project %q's release_tag_pattern %q", tag, project, src.TagPattern))
	}
	if err := s.releaseTagFree(ctx, project, tag, selfID); err != nil {
		return releaseCut{}, err
	}
	m["tag"] = tag
	if releaseString(m, "cut_at") == "" {
		m["cut_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	if releaseString(m, "cut_by") == "" {
		m["cut_by"] = releaseActor(ctx)
	}
	if releaseString(m, "source") == "" {
		m["source"] = "manual"
	}
	m[idempotencyKeyField] = releaseIdempotencyKey(project, tag)
	contents, _ := json.Marshal(pm["contents"])
	return releaseCut{tag: tag, project: project, contents: string(contents)}, nil
}

// releaseTagFree is the releaseConflict when a release other than selfID
// already holds project's tag.
func (s *BeadsServer) releaseTagFree(ctx context.Context, project, tag, selfID string) error {
	other, err := s.lookupByIdempotencyKey(ctx, []string{"project:" + project}, "", releaseIdempotencyKey(project, tag))
	if err != nil {
		return err
	}
	if other != nil && other.ID != selfID {
		return releaseConflict(fmt.Sprintf("release: %s %s is already recorded as %s", project, tag, other.ID))
	}
	return nil
}

// checkCutFieldsFixed refuses a change to the cut's fields on a release that
// was opened and has been cut: they were written once, by the cut. A release
// created cut (no opened_at) keeps the older rule, where only tag is fixed.
func checkCutFieldsFixed(m, pm map[string]any) error {
	if releaseString(pm, "opened_at") == "" {
		return nil
	}
	for _, key := range releaseCutFields {
		if releaseString(m, key) != releaseString(pm, key) {
			return inputError("release: " + key + " was written by the cut and cannot change")
		}
	}
	return nil
}

// checkReleaseCutRace re-checks a cut under releaseWriteMu, which the caller
// holds until the write is stored: the release is still open, no other
// release took the tag, and no merged PR's row is lost. When contents were
// appended since checkReleaseUpdate read the release (appendReleaseContents),
// a cut that writes contents is a 409, so its writer re-reads and cuts again,
// and a cut that writes none takes the stored contents onto bead.Fields.
// stored is the release as now stored when it took them, else nil.
func (s *BeadsServer) checkReleaseCutRace(ctx context.Context, bead *model.Bead, cut releaseCut) (stored json.RawMessage, err error) {
	cur, err := s.store.GetBead(ctx, bead.ID)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, sql.ErrNoRows
	}
	sm := releaseFieldMap(cur.Fields)
	if have := releaseString(sm, "tag"); have != "" {
		return nil, releaseConflict(fmt.Sprintf("release: %s was cut as %s while this write was in flight", bead.ID, have))
	}
	if err := s.releaseTagFree(ctx, cut.project, cut.tag, bead.ID); err != nil {
		return nil, err
	}
	if have, _ := json.Marshal(sm["contents"]); string(have) == cut.contents {
		return nil, nil
	}
	if cut.patchContents {
		return nil, releaseConflict(fmt.Sprintf("release: %s's contents changed while this cut was in flight; re-read the release and cut again", bead.ID))
	}
	m := releaseFieldMap(bead.Fields)
	if c, ok := sm["contents"]; ok {
		m["contents"] = c
	} else {
		delete(m, "contents")
	}
	fields, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encoding release %s fields: %w", bead.ID, err)
	}
	bead.Fields = fields
	return cur.Fields, nil
}

// openReleaseCloseError refuses closing an open release other than by its
// cut: a PATCH or close that does not write a tag. nil for any other bead and
// for a patch whose fields carry a tag.
func openReleaseCloseError(bead *model.Bead, patch json.RawMessage) error {
	if bead == nil || bead.Type != model.TypeRelease || bead.Status == model.StatusClosed {
		return nil
	}
	if !releaseIsOpen(releaseFieldMap(bead.Fields)) || releaseString(releaseFieldMap(patch), "tag") != "" {
		return nil
	}
	return releaseConflict(fmt.Sprintf("release: %s is the project's open release; it closes when its cut writes the tag", bead.ID))
}

// releaseContentsRequest is the body of POST /v1/releases/{id}/contents.
type releaseContentsRequest struct {
	Contents json.RawMessage `json:"contents"`
}

// releaseRowKey is the identity of a contents row: its merge commit and its PR
// number. A merge train's PRs share a merge_sha and differ in pr.
func releaseRowKey(row map[string]any) (sha, pr string) {
	sha = releaseString(row, "merge_sha")
	switch v := row["pr"].(type) {
	case float64:
		pr = strconv.FormatFloat(v, 'f', -1, 64)
	case string:
		pr = strings.TrimSpace(v)
	}
	return sha, pr
}

// appendReleaseContents appends rows to an open release's contents under
// releaseWriteMu. A row whose (merge_sha, pr) the release already carries, or
// that repeats an earlier row of the same request, changes nothing, so a
// redelivered merge boards once. Each row is a JSON object naming merge_sha or
// pr. A cut release is a 409: its contents are its git range, and a merged PR
// boards the next one. The caller has passed releaseWriterDenial. added is the
// number of rows written.
func (s *BeadsServer) appendReleaseContents(ctx context.Context, id string, raw json.RawMessage) (*model.Bead, int, error) {
	if err := validateReleaseRows(map[string]any{"contents": raw}, "contents"); err != nil {
		return nil, 0, err
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil || len(rows) == 0 {
		return nil, 0, inputError("release contents: contents must be a non-empty JSON list of objects")
	}
	for i, row := range rows {
		if row == nil {
			return nil, 0, inputError(fmt.Sprintf("release contents: contents[%d] is not an object", i))
		}
		if sha, pr := releaseRowKey(row); sha == "" && pr == "" {
			return nil, 0, inputError(fmt.Sprintf("release contents: contents[%d] names neither merge_sha nor pr", i))
		}
	}

	s.releaseWriteMu.Lock()
	defer s.releaseWriteMu.Unlock()

	rel, err := s.store.GetBead(ctx, id)
	if err != nil {
		return nil, 0, err
	}
	if rel == nil || rel.Type != model.TypeRelease {
		return nil, 0, sql.ErrNoRows
	}
	if !beadMatchesTokenProject(ctx, rel.Labels) {
		return nil, 0, &AuthzDenial{Action: "update", BeadType: string(model.TypeRelease), Message: "token not authorized for this release's project"}
	}
	m := releaseFieldMap(rel.Fields)
	if !releaseIsOpen(m) {
		return nil, 0, releaseConflict(fmt.Sprintf("release contents: %s is cut (%s); a merged PR boards the project's open release", rel.ID, releaseString(m, "tag")))
	}
	if _, ok := m["contents"]; ok && m["contents"] != nil {
		if err := validateReleaseRows(m, "contents"); err != nil {
			return nil, 0, err
		}
	}
	have := releaseContentsRows(m["contents"])
	seen := map[[2]string]bool{}
	for _, row := range have {
		sha, pr := releaseRowKey(row)
		seen[[2]string{sha, pr}] = true
	}
	added := 0
	for _, row := range rows {
		sha, pr := releaseRowKey(row)
		if seen[[2]string{sha, pr}] {
			continue
		}
		seen[[2]string{sha, pr}] = true
		have = append(have, row)
		added++
	}
	if added == 0 {
		return rel, 0, nil
	}
	old := string(rel.Fields)
	m["contents"] = have
	fields, err := json.Marshal(m)
	if err != nil {
		return nil, 0, fmt.Errorf("encoding release %s fields: %w", rel.ID, err)
	}
	rel.Fields = fields
	rel.UpdatedAt = time.Now().UTC()
	if err := s.commitReleaseWrites(ctx, []releaseWrite{{bead: rel, old: old}}); err != nil {
		return nil, 0, err
	}
	return rel, added, nil
}
