package landmap

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"gardenio/kbeads/internal/model"
	"gardenio/lands/contract/landsv1"
)

// Releases (releases plan gd-UZKZ9H25UD step 6, gardenio/docs/releases-design.md
// §3.5): a land's releases, read through the daemon's release routes, and the
// one tenant write, reporting where one runs. A release is recorded from the
// land's tags (github-bridge) and never written by a tenant; the daemon admits
// the deploy only for the land's owner (releaseDeployDenial) and stamps it
// via=contract.

// releaseRecord is what the map reads off the daemon's release routes (the
// server's releaseView). Rows are kept raw and read one by one, so one
// malformed row drops that row, not the release.
type releaseRecord struct {
	ID          string            `json:"id"`
	Project     string            `json:"project"`
	Tag         string            `json:"tag"`
	SHA         string            `json:"sha"`
	CutAt       string            `json:"cut_at"`
	Status      string            `json:"status"`
	Contents    []json.RawMessage `json:"contents"`
	Deployments []json.RawMessage `json:"deployments"`
}

// releaseItemRecord is one contents row the map reads.
type releaseItemRecord struct {
	PR    int    `json:"pr"`
	Title string `json:"title"`
	URL   string `json:"url"`
	Step  string `json:"step"`
	Plan  string `json:"plan"`
}

// deploymentRecord is one deployments row the map reads.
type deploymentRecord struct {
	Target   string `json:"target"`
	Kind     string `json:"kind"`
	Since    string `json:"since"`
	Until    string `json:"until"`
	By       string `json:"by"`
	Revision string `json:"revision"`
}

type releaseListRecord struct {
	Releases []releaseRecord `json:"releases"`
}

// The contract's bounds on a Release (openapi.yaml).
const (
	maxReleaseItems       = 1000
	maxReleaseDeployments = 500
	maxReleaseTag         = 64
	maxTargetName         = 64
	maxReleaseText        = 200
	maxReleaseTitle       = 500
	maxReleaseURL         = 2048
)

var releaseSHA = regexp.MustCompile(`^[0-9a-f]{7,64}$`)

var releaseStatuses = map[string]landsv1.ReleaseStatus{
	"cut": landsv1.Cut, "built": landsv1.Built, "live": landsv1.Live, "superseded": landsv1.Superseded,
}

// releaseTime reads an RFC 3339 time as a contract Timestamp; ok is false for
// an empty or unreadable one.
func releaseTime(s string) (landsv1.Timestamp, bool) {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(s))
	if err != nil {
		return landsv1.Timestamp{}, false
	}
	return stamp(t), true
}

// releaseText is s trimmed, or nil when it is empty or longer than n.
func releaseText(s string, n int) *string {
	s = strings.TrimSpace(s)
	if s == "" || utf8.RuneCountInString(s) > n {
		return nil
	}
	return &s
}

// releaseRef is a step or plan id the land may be shown: one with the
// release's own prefix (scrubProse's rule), else none.
func releaseRef(own, id string) *landsv1.Id {
	id = strings.TrimSpace(id)
	prefix, _, _ := strings.Cut(own, "-")
	p, _, ok := strings.Cut(id, "-")
	if !ok || p != prefix || len(id) > 64 {
		return nil
	}
	return &id
}

// toRelease projects one release of land. ok is false for a record that is
// not the land's or lacks what every Release carries.
func toRelease(land string, rec releaseRecord) (landsv1.Release, bool) {
	tag := strings.TrimSpace(rec.Tag)
	if rec.Project != land || rec.ID == "" || len(rec.ID) > 64 || tag == "" || len(tag) > maxReleaseTag {
		return landsv1.Release{}, false
	}
	status, ok := releaseStatuses[rec.Status]
	if !ok {
		status = landsv1.Cut
	}
	out := landsv1.Release{
		Id: rec.ID, Tag: tag, Status: status,
		Contents: []landsv1.ReleaseItem{}, Deployments: []landsv1.Deployment{},
	}
	if sha := strings.TrimSpace(rec.SHA); releaseSHA.MatchString(sha) {
		out.Sha = &sha
	}
	if at, ok := releaseTime(rec.CutAt); ok {
		out.CutAt = &at
	}
	for _, raw := range rec.Contents {
		if len(out.Contents) == maxReleaseItems {
			break
		}
		if it, ok := toReleaseItem(rec.ID, raw); ok {
			out.Contents = append(out.Contents, it)
		}
	}
	for _, raw := range rec.Deployments {
		if d, ok := toDeployment(raw); ok {
			out.Deployments = append(out.Deployments, d)
		}
	}
	if n := len(out.Deployments); n > maxReleaseDeployments {
		// The newest rows say where it runs; keep those.
		out.Deployments = out.Deployments[n-maxReleaseDeployments:]
	}
	return out, true
}

// toReleaseItem projects one contents row: a merged pull request with its
// number, a title and a web address. A row with no pull request (a direct
// commit) is not one.
func toReleaseItem(own string, raw json.RawMessage) (landsv1.ReleaseItem, bool) {
	var row releaseItemRecord
	if json.Unmarshal(raw, &row) != nil || row.PR < 1 {
		return landsv1.ReleaseItem{}, false
	}
	u, err := url.Parse(strings.TrimSpace(row.URL))
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || len(u.String()) > maxReleaseURL {
		return landsv1.ReleaseItem{}, false
	}
	title := []rune(strings.TrimSpace(scrubProse(own, row.Title)))
	if len(title) == 0 {
		title = []rune("#" + strconv.Itoa(row.PR))
	}
	if len(title) > maxReleaseTitle {
		title = title[:maxReleaseTitle]
	}
	return landsv1.ReleaseItem{
		Pr: row.PR, Title: string(title), Url: u.String(),
		Step: releaseRef(own, row.Step), Plan: releaseRef(own, row.Plan),
	}, true
}

// toDeployment projects one deployments row. by is a display name, never
// the principal.
func toDeployment(raw json.RawMessage) (landsv1.Deployment, bool) {
	var row deploymentRecord
	if json.Unmarshal(raw, &row) != nil {
		return landsv1.Deployment{}, false
	}
	target := releaseText(row.Target, maxTargetName)
	since, ok := releaseTime(row.Since)
	if target == nil || !ok {
		return landsv1.Deployment{}, false
	}
	d := landsv1.Deployment{
		Target: *target, Since: since,
		Kind:     releaseText(row.Kind, maxTargetName),
		By:       releaseText(displayName(row.By), maxReleaseText),
		Revision: releaseText(row.Revision, maxReleaseText),
	}
	if until, ok := releaseTime(row.Until); ok {
		d.Until = &until
	}
	return d, true
}

// getRelease reads the land's release named by ref, its tag or its id, and
// answers not_found for one the caller cannot read or another land's.
func (m *Map) getRelease(w http.ResponseWriter, r *http.Request, op, land, ref string) (landsv1.Release, bool) {
	var rec releaseRecord
	if err := m.d.call(r, rGetRelease, []string{ref}, url.Values{"project": {land}}, nil, &rec); err != nil {
		var ue *upstreamError
		if errors.As(err, &ue) && (ue.status == http.StatusForbidden || ue.status == http.StatusNotFound) {
			problem(w, landsv1.NotFound, "")
			return landsv1.Release{}, false
		}
		fail(w, r, op, err)
		return landsv1.Release{}, false
	}
	rel, ok := toRelease(land, rec)
	if !ok {
		problem(w, landsv1.NotFound, "")
	}
	return rel, ok
}

func (m *Map) ListReleases(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, params landsv1.ListReleasesParams) {
	if !m.admit(w, r, land) {
		return
	}
	off, n, ok := pageParams(w, params.Cursor, params.Limit)
	if !ok {
		return
	}
	// The daemon pages by limit alone: read through the end of this page and
	// one more, to know whether there is a next.
	q := url.Values{"project": {land}, "limit": {strconv.Itoa(off + n + 1)}}
	var pg releaseListRecord
	if err := m.d.call(r, rListReleases, nil, q, nil, &pg); err != nil {
		fail(w, r, "listReleases", err)
		return
	}
	all := make([]landsv1.Release, 0, len(pg.Releases))
	for _, rec := range pg.Releases {
		if rel, ok := toRelease(land, rec); ok {
			all = append(all, rel)
		}
	}
	items, next := pageOf(all, off, n)
	writeJSON(w, http.StatusOK, landsv1.ReleasePage{Items: items, NextCursor: next})
}

func (m *Map) GetRelease(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, release landsv1.ReleasePath) {
	if !m.admit(w, r, land) {
		return
	}
	if rel, ok := m.getRelease(w, r, "getRelease", land, release); ok {
		writeJSON(w, http.StatusOK, rel)
	}
}

// ReportDeployment reports that the release runs on one of the land's
// targets. The release is read first, so another land's answers not_found
// whatever the deploy route would say; the daemon then judges the caller
// (the land's owner only, forbidden for anyone else) and the target (one the
// land does not declare is its 400, conflict here).
func (m *Map) ReportDeployment(w http.ResponseWriter, r *http.Request, land landsv1.LandPath, release landsv1.ReleasePath, _ landsv1.ReportDeploymentParams) {
	const op = "reportDeployment"
	if !m.writable(w, r, op, land) {
		return
	}
	var in landsv1.DeploymentReport
	if !decodeBody(w, r, &in) {
		return
	}
	if !textOK(w, "target", in.Target, 1, maxTargetName) {
		return
	}
	body := map[string]string{"target": strings.TrimSpace(in.Target), "via": "contract"}
	if in.Revision != nil {
		if !textOK(w, "revision", *in.Revision, 1, maxReleaseText) {
			return
		}
		body["revision"] = strings.TrimSpace(*in.Revision)
	}
	if in.By != nil {
		if !textOK(w, "by", *in.By, 1, maxReleaseText) {
			return
		}
		body["by"] = strings.TrimSpace(*in.By)
	}
	rel, ok := m.getRelease(w, r, op, land, release)
	if !ok {
		return
	}
	var rec releaseRecord
	if err := m.d.call(r, rDeployRelease, []string{rel.Id}, nil, body, &rec); err != nil {
		var ue *upstreamError
		if errors.As(err, &ue) && ue.status == http.StatusBadRequest {
			// The map sent a well-formed row; what the daemon refuses is the
			// target, which the land does not declare.
			id := newCorrelationID()
			slog.Info("landmap: deployment refused", "op", op, "status", ue.status, "correlation_id", id, "body", string(ue.body))
			writeProblem(w, landsv1.Conflict, id, "")
			return
		}
		fail(w, r, op, err)
		return
	}
	out, ok := toRelease(land, rec)
	if !ok {
		fail(w, r, op, errors.New("deploy answered a release that is not the land's"))
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// releaseEvent reads a changed release for the feed: release.cut when it is
// recorded, release.deployed when an edit leaves it with deployments (a new
// row, or its open row closed by a newer release). ok is false for any other
// change.
func (m *Map) releaseEvent(r *http.Request, land string, b *beadRecord, kind ChangeKind) (landsv1.EventType, any, bool) {
	var typ landsv1.EventType
	switch kind {
	case ChangeCreated:
		typ = landsv1.ReleaseCut
	case ChangeUpdated:
		typ = landsv1.ReleaseDeployed
	default:
		return "", nil, false
	}
	var rec releaseRecord
	if err := m.d.call(r, rGetRelease, []string{b.ID}, nil, nil, &rec); err != nil {
		return "", nil, false
	}
	rel, ok := toRelease(land, rec)
	if !ok || (typ == landsv1.ReleaseDeployed && len(rel.Deployments) == 0) {
		return "", nil, false
	}
	return typ, rel, true
}

// releaseTypes is the one bead type the release verbs touch.
var releaseTypes = []model.BeadType{model.TypeRelease}
