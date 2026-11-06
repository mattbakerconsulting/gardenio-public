package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/model"
)

// type=release: one tag of one project, the commit it names, the merged PRs
// it carries and where it runs (gardenio/docs/releases-design.md §3, plan
// gd-UZKZ9H25UD step 2).
//
// A release is a closed, data-kind bead parented to its project bead and keyed
// by (project, tag): createBead stamps idempotency_key "release:<project>:<tag>",
// so a second write of the same pair returns the first bead (the webhook, the
// cut script and the backfill can race) and migration 029's unique index
// backstops a concurrent insert. The tag must match the project's
// release_tag_pattern; a project without one has no releases.
//
// status is derived, never written by a client: releaseStatus over built_at
// and deployments. deployments is written only by deployRelease, which appends
// the row, closes the open row of the release that held the target and
// recomputes both statuses in one transaction, so exactly one release is open
// per (project, target), and by repairReleaseDeployments (releases_repair.go),
// which rebuilds a project's history from its rows.
//
// Writers: the release captain (an agent whose role names release-captain, on
// its own per-agent token), the operator and gardenio-admin (OIDC admin), and
// a machine principal whose grants cover the type. The same line
// release-role-guard.sh draws on BOAT_ROLE. Everyone who can read the project
// reads its releases.

// Release status words (releases-design.md §3.2). An open release
// (releases_open.go) reads releaseStatusOpen until its cut.
const (
	releaseStatusCut        = "cut"
	releaseStatusBuilt      = "built"
	releaseStatusLive       = "live"
	releaseStatusSuperseded = "superseded"
)

// releaseCaptainRole is the agent role that may write releases, the role the
// release captain trigger (gd-FgKpNn3A02) spawns.
const releaseCaptainRole = "release-captain"

// releaseVias are the ways a deployment row is reported (§3.1).
// escalation is a helm_deploy escalation closed with the rolled tag
// (recordEscalationDeployment).
var releaseVias = []string{"script", "contract", "github", "reconciler", "escalation"}

// githubEnvironmentKind is the kind of a target deployRelease declares for a
// GitHub environment the project did not list (§4.3).
const githubEnvironmentKind = "github-environment"

// withReleaseTarget returns a project bead's fields with t appended to its
// release_targets. The list is written back in the form it was read: a JSON
// string stays a string, so the project validator sees the shape it checks.
func withReleaseTarget(fields json.RawMessage, targets []releaseTarget, t releaseTarget) (json.RawMessage, error) {
	m := releaseFieldMap(fields)
	list, err := json.Marshal(append(slices.Clone(targets), t))
	if err != nil {
		return nil, err
	}
	if _, isString := m["release_targets"].(string); isString {
		m["release_targets"] = string(list)
	} else {
		m["release_targets"] = json.RawMessage(list)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encoding project fields: %w", err)
	}
	return out, nil
}

// releaseDeployment is one row of a release's deployments: this release
// started running on Target at Since. Until is set by the daemon when another
// release of the project takes the target; empty means it runs there now.
type releaseDeployment struct {
	Target   string `json:"target"`
	Kind     string `json:"kind,omitempty"`
	Since    string `json:"since"`
	Until    string `json:"until"`
	By       string `json:"by,omitempty"`
	Via      string `json:"via,omitempty"`
	Revision string `json:"revision,omitempty"`
	ImageTag string `json:"image_tag,omitempty"`
}

// releaseTimeAfter reports whether RFC 3339 time a is later than b. The daemon
// writes every since and until in UTC seconds, so a string that does not parse
// falls back to comparing the text.
func releaseTimeAfter(a, b string) bool {
	ta, errA := time.Parse(time.RFC3339, a)
	tb, errB := time.Parse(time.RFC3339, b)
	if errA != nil || errB != nil {
		return a > b
	}
	return ta.After(tb)
}

// compareReleaseSince orders deployment rows by since, oldest first.
func compareReleaseSince(a, b releaseDeployment) int {
	switch {
	case releaseTimeAfter(b.Since, a.Since):
		return -1
	case releaseTimeAfter(a.Since, b.Since):
		return 1
	}
	return 0
}

// releaseTarget is one row of a project's release_targets.
type releaseTarget struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// releaseSource is a project bead's release source (§4).
type releaseSource struct {
	TagPattern string
	Targets    []releaseTarget
	Branch     string
}

// releaseView is the API shape of one release: the record's fields, typed,
// plus the bead's identity. contents and images pass through as stored.
type releaseView struct {
	ID               string              `json:"id"`
	Title            string              `json:"title"`
	Project          string              `json:"project"`
	Tag              string              `json:"tag"`
	OpenedAt         string              `json:"opened_at,omitempty"`
	OpenedBy         string              `json:"opened_by,omitempty"`
	SHA              string              `json:"sha,omitempty"`
	Previous         string              `json:"previous,omitempty"`
	PreviousTag      string              `json:"previous_tag,omitempty"`
	CutAt            string              `json:"cut_at,omitempty"`
	CutBy            string              `json:"cut_by,omitempty"`
	Source           string              `json:"source,omitempty"`
	BuiltAt          string              `json:"built_at,omitempty"`
	Images           json.RawMessage     `json:"images,omitempty"`
	Contents         json.RawMessage     `json:"contents,omitempty"`
	ContentsComplete *bool               `json:"contents_complete,omitempty"`
	Deployments      []releaseDeployment `json:"deployments"`
	Status           string              `json:"status"`
	SkippedBy        string              `json:"skipped_by,omitempty"`  // derived: releaseSkippedBy
	GardenerID       string              `json:"gardener_id,omitempty"` // the open release's gardener trigger
	CreatedAt        time.Time           `json:"created_at"`
	UpdatedAt        time.Time           `json:"updated_at"`
}

// releaseStatus is the status a release's fields imply (§3.2): open while it
// has no tag, live while any deployment row is open, superseded when it has
// rows and all are closed, built when built_at is set, else cut.
func releaseStatus(tag, builtAt string, deps []releaseDeployment) string {
	if strings.TrimSpace(tag) == "" {
		return releaseStatusOpen
	}
	if len(deps) > 0 {
		for _, d := range deps {
			if d.Until == "" {
				return releaseStatusLive
			}
		}
		return releaseStatusSuperseded
	}
	if strings.TrimSpace(builtAt) != "" {
		return releaseStatusBuilt
	}
	return releaseStatusCut
}

// releaseSkippedBy maps each cut or built release in rels to the release that
// went out first: the oldest newer release of its project that has run
// (live or superseded). That release was skipped (releases-design.md §3.2,
// gd-UbcDWJtcOA). Its status stays cut or built, a function of its own record;
// skipped is read across the project, like the viewer's /releases list. rels
// is newest first (sortReleasesNewestFirst) and may span projects.
func releaseSkippedBy(rels []*model.Bead) map[string]string {
	out := map[string]string{}
	wentOut := map[string]string{} // project -> the oldest newer release that ran
	for _, b := range rels {
		v := releaseViewOf(b)
		switch v.Status {
		case releaseStatusLive, releaseStatusSuperseded:
			wentOut[v.Project] = v.ID
		case releaseStatusCut, releaseStatusBuilt:
			if id := wentOut[v.Project]; id != "" {
				out[v.ID] = id
			}
		}
	}
	return out
}

// releaseViewWithSkip is releaseViewOf with SkippedBy filled from the
// project's releases. A failed lookup leaves SkippedBy empty.
func (s *BeadsServer) releaseViewWithSkip(ctx context.Context, b *model.Bead) releaseView {
	v := releaseViewOf(b)
	if (v.Status != releaseStatusCut && v.Status != releaseStatusBuilt) || v.Project == "" {
		return v
	}
	rels, err := s.projectReleases(ctx, v.Project)
	if err != nil {
		return v
	}
	v.SkippedBy = releaseSkippedBy(rels)[v.ID]
	return v
}

// releaseIdempotencyKey is the key that makes (project, tag) unique.
func releaseIdempotencyKey(project, tag string) string {
	return "release:" + project + ":" + tag
}

// releaseKeyFromFields returns the trimmed project and tag of a release's
// fields, "" for either when absent or not a string.
func releaseKeyFromFields(fields json.RawMessage) (project, tag string) {
	m := parseFieldsToStringMap(fields)
	return strings.TrimSpace(m["project"]), strings.TrimSpace(m["tag"])
}

// releaseFieldMap decodes a release's fields into a generic map (nil-safe).
func releaseFieldMap(fields json.RawMessage) map[string]any {
	m := map[string]any{}
	if len(fields) > 0 {
		_ = json.Unmarshal(fields, &m)
	}
	if m == nil {
		m = map[string]any{}
	}
	return m
}

// releaseString reads a string field from a decoded fields map.
func releaseString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return strings.TrimSpace(s)
}

// parseReleaseDeployments decodes the stored deployments list. An absent or
// null value is no rows.
func parseReleaseDeployments(v any) ([]releaseDeployment, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if s, ok := v.(string); ok {
		// A json field written as a string holds the JSON text.
		raw = []byte(s)
	}
	var deps []releaseDeployment
	if err := json.Unmarshal(raw, &deps); err != nil {
		return nil, fmt.Errorf("deployments is not a list of deployment rows: %w", err)
	}
	return deps, nil
}

// releaseViewOf projects a release bead into its API shape.
func releaseViewOf(b *model.Bead) releaseView {
	m := releaseFieldMap(b.Fields)
	deps, _ := parseReleaseDeployments(m["deployments"])
	if deps == nil {
		deps = []releaseDeployment{}
	}
	v := releaseView{
		ID:          b.ID,
		Title:       b.Title,
		Project:     releaseString(m, "project"),
		Tag:         releaseString(m, "tag"),
		OpenedAt:    releaseString(m, "opened_at"),
		OpenedBy:    releaseString(m, "opened_by"),
		SHA:         releaseString(m, "sha"),
		Previous:    releaseString(m, "previous"),
		PreviousTag: releaseString(m, "previous_tag"),
		CutAt:       releaseString(m, "cut_at"),
		CutBy:       releaseString(m, "cut_by"),
		Source:      releaseString(m, "source"),
		BuiltAt:     releaseString(m, "built_at"),
		Deployments: deps,
		Status:      releaseString(m, "status"),
		GardenerID:  releaseString(m, "gardener_id"),
		CreatedAt:   b.CreatedAt,
		UpdatedAt:   b.UpdatedAt,
	}
	if v.Status == "" {
		v.Status = releaseStatus(v.Tag, v.BuiltAt, deps)
	}
	if raw, ok := m["images"]; ok && raw != nil {
		v.Images, _ = json.Marshal(raw)
	}
	if raw, ok := m["contents"]; ok && raw != nil {
		v.Contents, _ = json.Marshal(raw)
	}
	if c, ok := m["contents_complete"].(bool); ok {
		v.ContentsComplete = &c
	}
	return v
}

// parseReleaseSource reads a project bead's release source.
func parseReleaseSource(fields json.RawMessage) (releaseSource, error) {
	m := releaseFieldMap(fields)
	src := releaseSource{
		TagPattern: releaseString(m, "release_tag_pattern"),
		Branch:     releaseString(m, "release_branch"),
	}
	if v, ok := m["release_targets"]; ok && v != nil {
		raw, err := json.Marshal(v)
		if err != nil {
			return src, err
		}
		if s, ok := v.(string); ok {
			raw = []byte(s)
		}
		if err := json.Unmarshal(raw, &src.Targets); err != nil {
			return src, fmt.Errorf("release_targets is not a list of {name, kind}: %w", err)
		}
	}
	return src, nil
}

// releaseTargetsShapeError checks a project's release_targets value: a JSON
// list of {name, kind} with a non-empty, unique name each. "" when valid.
func releaseTargetsShapeError(raw string) string {
	var targets []releaseTarget
	if err := json.Unmarshal([]byte(raw), &targets); err != nil {
		return fmt.Sprintf("release_targets must be a JSON list of {\"name\", \"kind\"}: %v", err)
	}
	seen := map[string]bool{}
	for i, t := range targets {
		name := strings.TrimSpace(t.Name)
		if name == "" {
			return fmt.Sprintf("release_targets[%d] has no name", i)
		}
		if seen[name] {
			return fmt.Sprintf("release_targets names %q twice", name)
		}
		seen[name] = true
	}
	return ""
}

// releaseTagPatternError checks a project's release_tag_pattern compiles as a
// Go regexp. "" when valid or empty.
func releaseTagPatternError(pattern string) string {
	if strings.TrimSpace(pattern) == "" {
		return ""
	}
	if _, err := regexp.Compile(pattern); err != nil {
		return fmt.Sprintf("release_tag_pattern is not a valid Go regexp: %v", err)
	}
	return ""
}

// releaseProject resolves a release's project bead and its release source. It
// is an inputError when the project has no bead, no release_tag_pattern or an
// unusable one: a project without a release source has no releases.
func (s *BeadsServer) releaseProject(ctx context.Context, project string) (*model.Bead, releaseSource, error) {
	if project == "" {
		return nil, releaseSource{}, inputError("release: project is required")
	}
	p, err := s.store.FindProjectByName(ctx, project)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && p == nil) {
		return nil, releaseSource{}, inputError(fmt.Sprintf("release: project %q has no project bead", project))
	}
	if err != nil {
		return nil, releaseSource{}, fmt.Errorf("looking up project %q: %w", project, err)
	}
	src, err := parseReleaseSource(p.Fields)
	if err != nil {
		return nil, releaseSource{}, inputError(fmt.Sprintf("release: project %q: %v", project, err))
	}
	if src.TagPattern == "" {
		return nil, releaseSource{}, inputError(fmt.Sprintf("release: project %q has no release_tag_pattern, so it has no releases", project))
	}
	return p, src, nil
}

// releaseDeployAction is the release writer check's action for recording a
// deployment (POST /v1/releases/{id}/deployments, a helm_deploy escalation's
// close). It is its own action, not update, so a machine role granted only
// deploy/release (gardenio-machine-release-deploy) records where a release
// runs and cannot PATCH the release's sha, images or contents.
const releaseDeployAction = "deploy"

// releaseWriterDenial is the release writer check: nil when the caller may
// write a release or a deployment, else the 403. It admits the daemon's own
// writes, an auth-disabled daemon, an OIDC gardenio-admin (the operator), a
// machine principal whose grants cover the action on type release, and an
// agent on its own per-agent token whose role names release-captain. The
// shared static token never qualifies: every pod holds it and its X-Actor is a
// claim.
func (s *BeadsServer) releaseWriterDenial(ctx context.Context, action string) *AuthzDenial {
	if isMachineWrite(ctx) {
		return nil
	}
	method := authMethodFromContext(ctx)
	switch {
	case method == authMethodNone:
		return nil
	case roleBearing(method) && hasMinimumRole(RolesFromContext(ctx), RoleAdmin):
		return nil
	case method == authMethodMachine && machineGrantsWrite(ctx, action, model.TypeRelease):
		return nil
	}
	if agent, err := s.callerAgentBead(ctx); err == nil && agent != nil &&
		slices.Contains(parseRoleList(agentRole(agent)), releaseCaptainRole) {
		return nil
	}
	d := &AuthzDenial{
		Action:       action,
		BeadType:     string(model.TypeRelease),
		RequiredRole: RoleAdmin,
		Message:      "insufficient permissions: only the release captain, the operator and gardenio-admin may write a release or a deployment",
	}
	if method == authMethodMachine {
		d.CallerSubject = AuthIdentityFromContext(ctx).Subject
	}
	s.populateAdvisoryText(ctx, d, "Releases are written by the release captain, the operator and gardenio-admin.")
	return d
}

// releaseDeployDenial is the tenant's deploy check (releases-design.md §3.3):
// a scoped principal may report where a release runs only when the release
// names exactly one project and the caller owns that land (actionReleaseDeploy
// is the owner tier's alone). nil when it may.
func releaseDeployDenial(ctx context.Context, rel *model.Bead) *AuthzDenial {
	if projects := projectLabelValues(rel.Labels); len(projects) != 1 {
		return &AuthzDenial{
			Action:   actionReleaseDeploy,
			BeadType: string(model.TypeRelease),
			Message:  "insufficient permissions: a release must name one land for its owner to report a deployment",
		}
	}
	return scopedTierDenial(ctx, actionReleaseDeploy, model.TypeRelease, rel.Labels)
}

// validateReleaseTime refuses a non-RFC 3339 timestamp field.
func validateReleaseTime(m map[string]any, key string) error {
	v := releaseString(m, key)
	if v == "" {
		return nil
	}
	if _, err := time.Parse(time.RFC3339, v); err != nil {
		return inputError(fmt.Sprintf("release: %s %q is not an RFC 3339 time", key, v))
	}
	return nil
}

// validateReleaseRows refuses a contents or images value that is not a JSON
// list of objects.
func validateReleaseRows(m map[string]any, key string) error {
	v, ok := m[key]
	if !ok || v == nil {
		return nil
	}
	raw, _ := json.Marshal(v)
	if s, isString := v.(string); isString {
		raw = []byte(s)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		return inputError(fmt.Sprintf("release: %s must be a JSON list of objects", key))
	}
	return nil
}

// newestReleaseBefore returns the release of project with the latest cut_at
// before cutAt (any when cutAt is empty), skipping skipID. nil when none.
func (s *BeadsServer) newestReleaseBefore(ctx context.Context, project, cutAt, skipID string) (*model.Bead, error) {
	rels, err := s.projectReleases(ctx, project)
	if err != nil {
		return nil, err
	}
	for _, r := range rels { // newest first
		if r.ID == skipID || releaseIsOpen(releaseFieldMap(r.Fields)) {
			continue
		}
		if cutAt != "" && releaseString(releaseFieldMap(r.Fields), "cut_at") >= cutAt {
			continue
		}
		return r, nil
	}
	return nil, nil
}

// projectReleases lists every release bead of project, the open one first,
// then newest cut first.
func (s *BeadsServer) projectReleases(ctx context.Context, project string) ([]*model.Bead, error) {
	beads, _, err := s.store.ListBeads(ctx, model.BeadFilter{
		Type:   []model.BeadType{model.TypeRelease},
		Labels: []string{"project:" + project},
		Limit:  maxListBeadsLimit,
	})
	if err != nil {
		return nil, fmt.Errorf("listing releases of %s: %w", project, err)
	}
	sortReleasesNewestFirst(beads)
	return beads, nil
}

// sortReleasesNewestFirst puts an open release first, then orders by cut_at,
// then created_at, newest first. RFC 3339 UTC strings sort as times.
func sortReleasesNewestFirst(beads []*model.Bead) {
	sort.SliceStable(beads, func(i, j int) bool {
		mi, mj := releaseFieldMap(beads[i].Fields), releaseFieldMap(beads[j].Fields)
		if oi, oj := releaseIsOpen(mi), releaseIsOpen(mj); oi != oj {
			return oi
		}
		ci, cj := releaseString(mi, "cut_at"), releaseString(mj, "cut_at")
		if ci != cj {
			return ci > cj
		}
		return beads[i].CreatedAt.After(beads[j].CreatedAt)
	})
}

// releaseIdempotencyKeyForCreate is the key createBead stamps on a release
// create: the tag's, or the project's next key for a create with no tag (an
// open release). "" when the fields carry no project (the guard then refuses
// the create).
func releaseIdempotencyKeyForCreate(fields json.RawMessage) string {
	project, tag := releaseKeyFromFields(fields)
	switch {
	case project == "":
		return ""
	case tag == "":
		return releaseNextKey(project)
	}
	return releaseIdempotencyKey(project, tag)
}

// applyReleaseGuard validates and completes a release on create (createBead
// holds releaseWriteMu). It refuses a project with no release source, a tag
// the project's pattern does not match, a client-written status or
// deployments, a previous that is not a release of the same project, and a
// parent other than the project bead. It fills previous (from previous_tag,
// else the newest earlier release of the project), cut_at, cut_by, source and
// status, stamps the project label, parents the release to its project bead
// and creates it closed: a release is a record, not work. A create with no
// tag is the project's open release (applyOpenReleaseGuard).
func (s *BeadsServer) applyReleaseGuard(ctx context.Context, bead *model.Bead, in *createBeadInput) error {
	m := releaseFieldMap(bead.Fields)
	project, tag := releaseString(m, "project"), releaseString(m, "tag")
	if project == "" {
		return inputError("release: project is required")
	}
	m["project"] = project
	if tag != "" {
		m["tag"] = tag
	} else {
		delete(m, "tag")
	}

	if lp := projectFromLabels(bead.Labels); lp != "" && lp != "unknown" && lp != project {
		return inputError(fmt.Sprintf("release: label project:%s does not match fields.project %q", lp, project))
	}
	if !hasProjectLabel(bead.Labels) {
		bead.Labels = append(bead.Labels, "project:"+project)
	}

	projBead, src, err := s.releaseProject(ctx, project)
	if err != nil {
		return err
	}
	if in.ParentBeadID == "" {
		in.ParentBeadID = projBead.ID
	} else if in.ParentBeadID != projBead.ID {
		return inputError(fmt.Sprintf("release: a release is parented to its project bead %s, not %s", projBead.ID, in.ParentBeadID))
	}
	if tag == "" {
		return s.applyOpenReleaseGuard(ctx, bead, m, project, projBead)
	}
	re, err := regexp.Compile(src.TagPattern)
	if err != nil {
		return inputError(fmt.Sprintf("release: project %q: %s", project, releaseTagPatternError(src.TagPattern)))
	}
	if !re.MatchString(tag) {
		return inputError(fmt.Sprintf("release: tag %q does not match project %q's release_tag_pattern %q", tag, project, src.TagPattern))
	}

	if _, ok := m["status"]; ok {
		return inputError("release: status is derived by the daemon (open, cut, built, live, superseded); do not write it")
	}
	if deps, err := parseReleaseDeployments(m["deployments"]); err != nil || len(deps) > 0 {
		return inputError("release: deployments are written with POST /v1/releases/{id}/deployments (kd release deploy), not on create")
	}
	delete(m, "deployments")
	for _, key := range []string{"cut_at", "built_at"} {
		if err := validateReleaseTime(m, key); err != nil {
			return err
		}
	}
	for _, key := range []string{"contents", "images"} {
		if err := validateReleaseRows(m, key); err != nil {
			return err
		}
	}

	if releaseString(m, "cut_at") == "" {
		m["cut_at"] = time.Now().UTC().Format(time.RFC3339)
	}
	if releaseString(m, "cut_by") == "" {
		m["cut_by"] = releaseActor(ctx)
	}
	if releaseString(m, "source") == "" {
		m["source"] = "manual"
	}

	if err := s.resolveReleasePrevious(ctx, m, project, ""); err != nil {
		return err
	}

	m["status"] = releaseStatus(tag, releaseString(m, "built_at"), nil)
	fields, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding release fields: %w", err)
	}
	bead.Fields = fields

	if bead.Status != model.StatusClosed {
		now := time.Now().UTC()
		bead.Status = model.StatusClosed
		bead.ClosedAt = &now
	}
	return nil
}

// resolveReleasePrevious validates and fills previous / previous_tag in m. A
// set previous must be a release of the same project (its tag fills
// previous_tag). Otherwise previous_tag names the release to link, and with
// neither the newest release of the project cut before this one is linked. A
// previous_tag with no release yet stays unlinked: the backfill may write the
// older release later.
func (s *BeadsServer) resolveReleasePrevious(ctx context.Context, m map[string]any, project, selfID string) error {
	prevID, prevTag := releaseString(m, "previous"), releaseString(m, "previous_tag")
	switch {
	case prevID != "":
		if prevID == selfID {
			return inputError("release: previous cannot be the release itself")
		}
		prev, err := s.store.GetBead(ctx, prevID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && prev == nil) {
			return inputError(fmt.Sprintf("release: previous %s does not exist", prevID))
		}
		if err != nil {
			return fmt.Errorf("looking up previous release %s: %w", prevID, err)
		}
		pm := releaseFieldMap(prev.Fields)
		if prev.Type != model.TypeRelease || releaseString(pm, "project") != project {
			return inputError(fmt.Sprintf("release: previous %s is not a release of project %q", prevID, project))
		}
		if prevTag != "" && prevTag != releaseString(pm, "tag") {
			return inputError(fmt.Sprintf("release: previous_tag %q is not the tag of previous %s (%q)", prevTag, prevID, releaseString(pm, "tag")))
		}
		m["previous_tag"] = releaseString(pm, "tag")
	case prevTag != "":
		prev, err := s.lookupByIdempotencyKey(ctx, []string{"project:" + project}, "", releaseIdempotencyKey(project, prevTag))
		if err != nil {
			return err
		}
		if prev != nil && prev.Type == model.TypeRelease && prev.ID != selfID {
			m["previous"] = prev.ID
		}
	default:
		prev, err := s.newestReleaseBefore(ctx, project, releaseString(m, "cut_at"), selfID)
		if err != nil {
			return err
		}
		if prev != nil {
			m["previous"] = prev.ID
			m["previous_tag"] = releaseString(releaseFieldMap(prev.Fields), "tag")
		}
	}
	return nil
}

// checkReleaseUpdate guards a generic PATCH of a release (updateBead, after
// the merge): project and tag never change, except that the cut writes the tag
// onto an open release once (checkOpenReleaseCut); status and deployments are
// the daemon's (deployRelease); a changed previous must be a release of the
// same project; times stay RFC 3339. It returns the fields with status
// recomputed, and the cut when the PATCH cut an open release, which the
// caller closes.
func (s *BeadsServer) checkReleaseUpdate(ctx context.Context, bead *model.Bead, prior json.RawMessage) (json.RawMessage, releaseCut, error) {
	m, pm := releaseFieldMap(bead.Fields), releaseFieldMap(prior)
	var cut releaseCut
	if releaseIsOpen(pm) {
		if releaseString(m, "tag") == "" {
			delete(m, "tag")
		}
		for _, key := range []string{"opened_at", "opened_by"} {
			if releaseString(m, key) != releaseString(pm, key) {
				return nil, cut, inputError("release: " + key + " is written by the daemon when the release opens")
			}
		}
	} else if err := checkCutFieldsFixed(m, pm); err != nil {
		return nil, cut, err
	}
	for _, key := range []string{"project", "tag", "deployments", "status", "idempotency_key"} {
		if releaseIsOpen(pm) && (key == "tag" || key == "idempotency_key") {
			continue // the cut writes both (checkOpenReleaseCut)
		}
		before, _ := json.Marshal(pm[key])
		after, _ := json.Marshal(m[key])
		if string(before) != string(after) {
			if key == "status" || key == "deployments" {
				return nil, cut, inputError("release: " + key + " is written by the daemon; record a deployment with kd release deploy")
			}
			return nil, cut, inputError("release: " + key + " cannot change after create")
		}
	}
	if releaseIsOpen(pm) {
		if releaseString(m, idempotencyKeyField) != releaseString(pm, idempotencyKeyField) {
			return nil, cut, inputError("release: idempotency_key cannot change after create")
		}
		var err error
		if cut, err = s.checkOpenReleaseCut(ctx, bead.ID, m, pm); err != nil {
			return nil, cut, err
		}
	}
	for _, key := range []string{"cut_at", "built_at"} {
		if err := validateReleaseTime(m, key); err != nil {
			return nil, cut, err
		}
	}
	for _, key := range []string{"contents", "images"} {
		if err := validateReleaseRows(m, key); err != nil {
			return nil, cut, err
		}
	}
	if cut.tag != "" && releaseString(m, "previous") == "" && releaseString(m, "previous_tag") == "" {
		// The cut links the newest release cut before it, as a create does.
		if err := s.resolveReleasePrevious(ctx, m, releaseString(m, "project"), bead.ID); err != nil {
			return nil, cut, err
		}
	} else if releaseString(m, "previous") != releaseString(pm, "previous") || releaseString(m, "previous_tag") != releaseString(pm, "previous_tag") {
		if releaseString(m, "previous") == releaseString(pm, "previous") {
			// Only previous_tag moved: relink from it.
			delete(m, "previous")
		}
		if err := s.resolveReleasePrevious(ctx, m, releaseString(m, "project"), bead.ID); err != nil {
			return nil, cut, err
		}
	}
	deps, err := parseReleaseDeployments(m["deployments"])
	if err != nil {
		return nil, cut, inputError("release: " + err.Error())
	}
	m["status"] = releaseStatus(releaseString(m, "tag"), releaseString(m, "built_at"), deps)
	out, err := json.Marshal(m)
	if err != nil {
		return nil, cut, fmt.Errorf("encoding release fields: %w", err)
	}
	return out, cut, nil
}

// releaseActor names the caller for cut_by and a deployment's by: the agent
// name on a per-agent token, else the authenticated subject.
func releaseActor(ctx context.Context) string {
	if name, ok := agentNameFromContext(ctx); ok {
		return name
	}
	if subj := actorFromContext(ctx); subj != "" {
		return subj
	}
	return "unknown"
}

// releaseDeployRequest is the body of POST /v1/releases/{id}/deployments.
type releaseDeployRequest struct {
	Target   string `json:"target"`
	Kind     string `json:"kind,omitempty"`
	Since    string `json:"since,omitempty"`
	By       string `json:"by,omitempty"`
	Via      string `json:"via,omitempty"`
	Revision string `json:"revision,omitempty"`
	ImageTag string `json:"image_tag,omitempty"`
}

// deployRelease records a deployment (writeDeployment) and, when it wrote a
// row, closes the awaits_release work the release carries
// (closeShippedWork). The close runs after releaseWriteMu is released.
func (s *BeadsServer) deployRelease(ctx context.Context, id string, req releaseDeployRequest) (*model.Bead, error) {
	rel, wrote, err := s.writeDeployment(ctx, id, req)
	if err != nil {
		return nil, err
	}
	if wrote {
		s.closeShippedWork(ctx, rel, strings.TrimSpace(req.Target))
	}
	return rel, nil
}

// writeDeployment records that release id runs on req.Target from req.Since.
// Under releaseWriteMu and in one transaction it closes the open row on that
// target of every other release of the project (a rollback is a deployment of
// an older release, so the newer one is closed the same way), closes this
// release's own open row there when the revision or image changed, appends the
// new row and recomputes every touched release's status. A helm revision is
// one roll on a target: a report of a revision the release already records
// there changes nothing, and one another release records is a releaseConflict
// (409). Without a revision, a repeat of the open row (same target and image
// tag), or of a closed row with the same since, changes nothing. A row older
// than the newest on the target is history: it goes in closed, and what runs
// there now is untouched. The caller has passed releaseWriterDenial. wrote is
// false when nothing changed.
func (s *BeadsServer) writeDeployment(ctx context.Context, id string, req releaseDeployRequest) (*model.Bead, bool, error) {
	s.releaseWriteMu.Lock()
	defer s.releaseWriteMu.Unlock()

	rel, err := s.store.GetBead(ctx, id)
	if err != nil {
		return nil, false, err
	}
	if rel == nil || rel.Type != model.TypeRelease {
		return nil, false, sql.ErrNoRows
	}
	if !beadMatchesTokenProject(ctx, rel.Labels) {
		return nil, false, &AuthzDenial{Action: releaseDeployAction, BeadType: string(model.TypeRelease), Message: "token not authorized for this release's project"}
	}
	relFields := releaseFieldMap(rel.Fields)
	project, tag := releaseString(relFields, "project"), releaseString(relFields, "tag")
	if tag == "" {
		return nil, false, releaseConflict(fmt.Sprintf("release deploy: %s is %s's open release, not cut yet; deploy a cut release", rel.ID, project))
	}
	projBead, src, err := s.releaseProject(ctx, project)
	if err != nil {
		return nil, false, err
	}

	target := strings.TrimSpace(req.Target)
	if target == "" {
		return nil, false, inputError("release deploy: target is required")
	}
	var declared *releaseTarget
	for i := range src.Targets {
		if strings.TrimSpace(src.Targets[i].Name) == target {
			declared = &src.Targets[i]
			break
		}
	}
	// A GitHub Deployment to an environment the project does not declare
	// declares it (§4.3), so a deploy the project's own CD made is never lost.
	// Every other way in names a target someone chose, so a typo stays a 400.
	var newTarget *releaseTarget
	if declared == nil && strings.TrimSpace(req.Via) == "github" {
		newTarget = &releaseTarget{Name: target, Kind: githubEnvironmentKind}
		declared = newTarget
	}
	if declared == nil {
		names := make([]string, 0, len(src.Targets))
		for _, t := range src.Targets {
			names = append(names, t.Name)
		}
		return nil, false, inputError(fmt.Sprintf("release deploy: project %q has no release target %q (release_targets: %s)", project, target, strings.Join(names, ", ")))
	}
	row := releaseDeployment{
		Target:   target,
		Kind:     strings.TrimSpace(req.Kind),
		Since:    strings.TrimSpace(req.Since),
		By:       strings.TrimSpace(req.By),
		Via:      strings.TrimSpace(req.Via),
		Revision: strings.TrimSpace(req.Revision),
		ImageTag: strings.TrimSpace(req.ImageTag),
	}
	if row.Kind == "" {
		row.Kind = declared.Kind
	}
	if row.Since == "" {
		row.Since = time.Now().UTC().Format(time.RFC3339)
	} else if t, err := time.Parse(time.RFC3339, row.Since); err != nil {
		return nil, false, inputError(fmt.Sprintf("release deploy: since %q is not an RFC 3339 time", row.Since))
	} else {
		row.Since = t.UTC().Format(time.RFC3339)
	}
	if row.By == "" {
		row.By = releaseActor(ctx)
	}
	if row.Via == "" {
		row.Via = "script"
	}
	if !slices.Contains(releaseVias, row.Via) {
		return nil, false, inputError(fmt.Sprintf("release deploy: via %q is not one of %s", row.Via, strings.Join(releaseVias, ", ")))
	}
	if row.ImageTag == "" {
		row.ImageTag = tag
	}

	ownDeps, err := parseReleaseDeployments(relFields["deployments"])
	if err != nil {
		return nil, false, inputError("release deploy: " + err.Error())
	}
	if releaseDeployRepeat(ownDeps, row) {
		return rel, false, nil
	}

	others, err := s.projectReleases(ctx, project)
	if err != nil {
		return nil, false, err
	}
	if err := releaseRevisionTaken(rel.ID, project, others, row); err != nil {
		return nil, false, err
	}
	// A backdated row (the backfill replaying old deploys) slots into the
	// target's history: it is closed at the since of the earliest row that
	// started after it, and the row it follows is closed at its since, so the
	// release that runs there now keeps its open row.
	next := ""
	noteNext := func(deps []releaseDeployment) {
		for _, d := range deps {
			if d.Target == target && releaseTimeAfter(d.Since, row.Since) && (next == "" || releaseTimeAfter(next, d.Since)) {
				next = d.Since
			}
		}
	}
	noteNext(ownDeps)
	for _, other := range others {
		if other.ID == rel.ID {
			continue
		}
		if deps, err := parseReleaseDeployments(releaseFieldMap(other.Fields)["deployments"]); err == nil {
			noteNext(deps)
		}
	}
	row.Until = next
	// closeBefore closes, at row.Since, a row on target that started no later
	// than the new row and was still running at its since.
	closeBefore := func(deps []releaseDeployment) bool {
		changed := false
		for i := range deps {
			d := &deps[i]
			if d.Target != target || releaseTimeAfter(d.Since, row.Since) {
				continue
			}
			if d.Until == "" || releaseTimeAfter(d.Until, row.Since) {
				d.Until = row.Since
				changed = true
			}
		}
		return changed
	}
	var writes []releaseWrite
	setDeployments := func(b *model.Bead, m map[string]any, deps []releaseDeployment) error {
		old := string(b.Fields)
		m["deployments"] = deps
		m["status"] = releaseStatus(releaseString(m, "tag"), releaseString(m, "built_at"), deps)
		fields, err := json.Marshal(m)
		if err != nil {
			return fmt.Errorf("encoding release %s fields: %w", b.ID, err)
		}
		b.Fields = fields
		b.UpdatedAt = time.Now().UTC()
		writes = append(writes, releaseWrite{bead: b, old: old})
		return nil
	}
	for _, other := range others {
		if other.ID == rel.ID {
			continue
		}
		om := releaseFieldMap(other.Fields)
		deps, err := parseReleaseDeployments(om["deployments"])
		if err != nil {
			continue // a malformed neighbour is left alone, never rewritten
		}
		if closeBefore(deps) {
			if err := setDeployments(other, om, deps); err != nil {
				return nil, false, err
			}
		}
	}
	closeBefore(ownDeps)
	ownDeps = append(ownDeps, row)
	slices.SortStableFunc(ownDeps, compareReleaseSince)
	if err := setDeployments(rel, relFields, ownDeps); err != nil {
		return nil, false, err
	}
	if newTarget != nil {
		old := string(projBead.Fields)
		fields, err := withReleaseTarget(projBead.Fields, src.Targets, *newTarget)
		if err != nil {
			return nil, false, err
		}
		projBead.Fields = fields
		projBead.UpdatedAt = time.Now().UTC()
		writes = append(writes, releaseWrite{bead: projBead, old: old})
	}

	if err := s.commitReleaseWrites(ctx, writes); err != nil {
		return nil, false, err
	}
	return rel, true, nil
}

// releaseDeployRepeat reports whether row, a deploy report for a release,
// repeats a row the release already records (ownDeps) on its target. A helm
// revision is one roll on its target: a late report of it (an event's time is
// when someone reported the roll, not when it happened) changes nothing,
// wherever its row now sits in history. Without a revision, a repeat of the
// open row with the same image tag, or of a closed row with the same since,
// changes nothing.
func releaseDeployRepeat(ownDeps []releaseDeployment, row releaseDeployment) bool {
	for _, d := range ownDeps {
		if d.Target != row.Target {
			continue
		}
		if row.Revision != "" && d.Revision == row.Revision {
			return true
		}
		if d.Revision == row.Revision && d.ImageTag == row.ImageTag && (d.Until == "" || d.Since == row.Since) {
			return true
		}
	}
	return false
}

// releaseRevisionTaken is the releaseConflict for row when a release of
// project other than relID already records row's helm revision on its target,
// since a revision cannot carry two tags; nil otherwise and for a row with no
// revision. A release whose deployments do not parse is skipped.
func releaseRevisionTaken(relID, project string, others []*model.Bead, row releaseDeployment) error {
	if row.Revision == "" {
		return nil
	}
	for _, other := range others {
		if other.ID == relID {
			continue
		}
		om := releaseFieldMap(other.Fields)
		deps, err := parseReleaseDeployments(om["deployments"])
		if err != nil {
			continue
		}
		for _, d := range deps {
			if d.Target == row.Target && d.Revision == row.Revision {
				return releaseConflict(fmt.Sprintf("release deploy: revision %s on %s is already recorded for %s %s; a revision is one roll of one tag",
					row.Revision, row.Target, project, releaseString(om, "tag")))
			}
		}
	}
	return nil
}

// helmDeployOperation is the escalation operation_type a crew files to ask for
// a build to ship (`gb escalation create --operation-type helm_deploy`).
const helmDeployOperation = "helm_deploy"

// recordEscalationDeployment records the deployment a helm_deploy escalation
// rolled when it is closed success with the rolled tag in its result:
//
//	{"tag": "2026.304.1", "revision": "431", "target": "prod", "project": "gardenio"}
//
// tag (or image_tag) is required; revision is the Helm revision; target
// defaults to prod and project to the escalation's project:<name> label, else
// gardenio. The row is the one release-deploy.sh writes, by the executor, via
// escalation. It is best effort: the close has already happened, so a result
// with no tag records nothing and a refusal is returned for the response, never
// an error. The caller must hold the release writer check, as on the deploy
// route.
func (s *BeadsServer) recordEscalationDeployment(ctx context.Context, esc *model.Bead, result any, executor string) map[string]any {
	if esc == nil || releaseString(releaseFieldMap(esc.Fields), "operation_type") != helmDeployOperation {
		return nil
	}
	m, ok := result.(map[string]any)
	if !ok {
		return nil
	}
	tag := releaseString(m, "tag")
	if tag == "" {
		tag = releaseString(m, "image_tag")
	}
	if tag == "" {
		return nil
	}
	project := releaseString(m, "project")
	if project == "" {
		for _, l := range esc.Labels {
			if p, ok := strings.CutPrefix(l, "project:"); ok && p != "" {
				project = p
				break
			}
		}
	}
	if project == "" {
		project = "gardenio"
	}
	target := releaseString(m, "target")
	if target == "" {
		target = "prod"
	}
	revision := releaseString(m, "revision")
	if n, ok := m["revision"].(float64); ok && revision == "" {
		revision = strconv.FormatFloat(n, 'f', -1, 64)
	}
	out := map[string]any{"project": project, "tag": tag, "target": target}
	fail := func(msg string) map[string]any {
		slog.Info("helm_deploy escalation: deployment not recorded", "escalation", esc.ID, "project", project, "tag", tag, "reason", msg)
		out["error"] = msg
		return out
	}
	if denial := s.releaseWriterDenial(ctx, releaseDeployAction); denial != nil {
		return fail(denial.Message)
	}
	rel, err := s.lookupByIdempotencyKey(ctx, []string{"project:" + project}, "", releaseIdempotencyKey(project, tag))
	if err != nil {
		return fail(err.Error())
	}
	if rel == nil || rel.Type != model.TypeRelease {
		return fail(fmt.Sprintf("no release %s %s is recorded", project, tag))
	}
	by := strings.TrimSpace(executor)
	if by == "" {
		by = releaseActor(ctx)
	}
	if _, err := s.deployRelease(ctx, rel.ID, releaseDeployRequest{
		Target: target, By: by, Via: "escalation", Revision: revision, ImageTag: tag,
	}); err != nil {
		return fail(err.Error())
	}
	out["release"] = rel.ID
	out["revision"] = revision
	return out
}
