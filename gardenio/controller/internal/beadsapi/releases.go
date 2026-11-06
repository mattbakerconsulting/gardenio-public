package beadsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Releases (gardenio/docs/releases-design.md §3, plan gd-UZKZ9H25UD step 2):
// the daemon's /v1/releases routes. A release is one tag of one project, the
// merged PRs it carries and where it runs; its Status (cut, built, live,
// superseded) is derived by the daemon. The farm document, the truck and the
// Release page read releases through these calls. Writes are refused (403)
// unless the caller is the release captain, the operator or gardenio-admin.

// Release status words. An open release has no tag yet: it is the project's
// next release, which merged PRs board before its cut.
const (
	ReleaseStatusOpen       = "open"
	ReleaseStatusCut        = "cut"
	ReleaseStatusBuilt      = "built"
	ReleaseStatusLive       = "live"
	ReleaseStatusSuperseded = "superseded"
)

// Release is one release of a project.
type Release struct {
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
	Deployments      []ReleaseDeployment `json:"deployments"`
	Status           string              `json:"status"`
	GardenerID       string              `json:"gardener_id,omitempty"`
	CreatedAt        time.Time           `json:"created_at"`
	UpdatedAt        time.Time           `json:"updated_at"`
}

// ReleaseDeployment is one row of a release's deployments. Until is empty
// while the release runs on Target.
type ReleaseDeployment struct {
	Target   string `json:"target"`
	Kind     string `json:"kind,omitempty"`
	Since    string `json:"since"`
	Until    string `json:"until"`
	By       string `json:"by,omitempty"`
	Via      string `json:"via,omitempty"`
	Revision string `json:"revision,omitempty"`
	ImageTag string `json:"image_tag,omitempty"`
}

// CutReleases is rels without the open release (no tag yet), in order: the
// releases a reader of cut, built and live releases shows. The open release's
// own surfaces are plan gd-lgN4U7X5Tb's later steps.
func CutReleases(rels []Release) []Release {
	out := rels[:0:0]
	for _, r := range rels {
		if r.Tag != "" {
			out = append(out, r)
		}
	}
	return out
}

// RunsOn returns the targets the release runs on now.
func (r Release) RunsOn() []string {
	var out []string
	for _, d := range r.Deployments {
		if d.Until == "" {
			out = append(out, d.Target)
		}
	}
	return out
}

// CreateReleaseRequest is the body of POST /v1/releases.
type CreateReleaseRequest struct {
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

// DeployReleaseRequest is the body of POST /v1/releases/{id}/deployments.
type DeployReleaseRequest struct {
	Target   string `json:"target"`
	Kind     string `json:"kind,omitempty"`
	Since    string `json:"since,omitempty"`
	By       string `json:"by,omitempty"`
	Via      string `json:"via,omitempty"`
	Revision string `json:"revision,omitempty"`
	ImageTag string `json:"image_tag,omitempty"`
}

// releasePath is /v1/releases/{ref}[suffix], with ?project= when project is
// set so ref may be a tag.
func releasePath(ref, project, suffix string) string {
	path := "/v1/releases/" + url.PathEscape(ref) + suffix
	if project != "" {
		path += "?project=" + url.QueryEscape(project)
	}
	return path
}

// ListReleases lists releases newest cut first. Empty project lists every
// project the caller can read; status is a comma-separated filter; limit <= 0
// defers to the daemon (50).
func (c *Client) ListReleases(ctx context.Context, project, status string, limit int) ([]Release, error) {
	q := url.Values{}
	if project != "" {
		q.Set("project", project)
	}
	if status != "" {
		q.Set("status", status)
	}
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	path := "/v1/releases"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var resp struct {
		Releases []Release `json:"releases"`
	}
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("listing releases: %w", err)
	}
	return resp.Releases, nil
}

// GetRelease reads one release: ref is its bead id, or its tag when project
// is set.
func (c *Client) GetRelease(ctx context.Context, ref, project string) (*Release, error) {
	var r Release
	if err := c.doJSON(ctx, http.MethodGet, releasePath(ref, project, ""), nil, &r); err != nil {
		return nil, fmt.Errorf("getting release %s: %w", ref, err)
	}
	return &r, nil
}

// CreateRelease records a release; a second create of the same (project,
// tag) returns the existing one.
func (c *Client) CreateRelease(ctx context.Context, req CreateReleaseRequest) (*Release, error) {
	var r Release
	if err := c.doJSON(ctx, http.MethodPost, "/v1/releases", req, &r); err != nil {
		return nil, fmt.Errorf("creating release %s %s: %w", req.Project, req.Tag, err)
	}
	return &r, nil
}

// DeployRelease records that a release runs on req.Target; ref is its bead
// id, or its tag when project is set.
func (c *Client) DeployRelease(ctx context.Context, ref, project string, req DeployReleaseRequest) (*Release, error) {
	var r Release
	if err := c.doJSON(ctx, http.MethodPost, releasePath(ref, project, "/deployments"), req, &r); err != nil {
		return nil, fmt.Errorf("deploying release %s to %s: %w", ref, req.Target, err)
	}
	return &r, nil
}

// AppendReleaseContents appends rows to the open release ref (its bead id)
// through POST /v1/releases/{id}/contents: a merged PR boarding the next
// release. Rows the release already carries (by merge_sha and pr) are left
// as they are, so a second append of the same rows writes nothing; a cut
// release answers 409.
func (c *Client) AppendReleaseContents(ctx context.Context, ref string, rows any) (*Release, error) {
	var r Release
	body := map[string]any{"contents": rows}
	if err := c.doJSON(ctx, http.MethodPost, releasePath(ref, "", "/contents"), body, &r); err != nil {
		return nil, fmt.Errorf("appending to release %s: %w", ref, err)
	}
	return &r, nil
}
