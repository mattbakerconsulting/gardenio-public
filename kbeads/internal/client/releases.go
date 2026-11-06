package client

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
// POST/GET /v1/releases, GET /v1/releases/{id}, POST
// /v1/releases/{id}/deployments, POST /v1/releases/repair-deployments. HTTP-only; the gRPC transport returns an
// unsupported error. The writes are refused (403) unless the caller is the
// release captain, the operator or gardenio-admin.

// Release is one release of a project, as the daemon returns it.
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
	SkippedBy        string              `json:"skipped_by,omitempty"` // never ran: this release went out first
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

// CreateRelease records a release (POST /v1/releases). A second create of the
// same (project, tag) returns the existing release.
func (c *HTTPClient) CreateRelease(ctx context.Context, req CreateReleaseRequest) (*Release, error) {
	var r Release
	if err := c.doJSON(ctx, http.MethodPost, "/v1/releases", req, &r); err != nil {
		return nil, fmt.Errorf("creating release %s %s: %w", req.Project, req.Tag, err)
	}
	return &r, nil
}

// GetRelease reads one release (GET /v1/releases/{ref}). ref is the release
// bead id, or its tag when project is set.
func (c *HTTPClient) GetRelease(ctx context.Context, ref, project string) (*Release, error) {
	var r Release
	if err := c.doJSON(ctx, http.MethodGet, releasePath(ref, project, ""), nil, &r); err != nil {
		return nil, fmt.Errorf("getting release %s: %w", ref, err)
	}
	return &r, nil
}

// ListReleases lists releases newest cut first (GET /v1/releases). Empty
// project lists every project the caller can read; status is a
// comma-separated filter; limit <= 0 defers to the daemon (50).
func (c *HTTPClient) ListReleases(ctx context.Context, project, status string, limit int) ([]Release, error) {
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

// DeployRelease records that a release runs on req.Target (POST
// /v1/releases/{ref}/deployments). ref is the release bead id, or its tag
// when project is set.
func (c *HTTPClient) DeployRelease(ctx context.Context, ref, project string, req DeployReleaseRequest) (*Release, error) {
	var r Release
	if err := c.doJSON(ctx, http.MethodPost, releasePath(ref, project, "/deployments"), req, &r); err != nil {
		return nil, fmt.Errorf("deploying release %s to %s: %w", ref, req.Target, err)
	}
	return &r, nil
}

// ReleaseRepairChange is one release a deployment repair rewrote (or would,
// on a dry run): its rows and status before and after, and the duplicate rows
// it dropped.
type ReleaseRepairChange struct {
	ID           string              `json:"id"`
	Tag          string              `json:"tag"`
	StatusBefore string              `json:"status_before"`
	Status       string              `json:"status"`
	Removed      []ReleaseDeployment `json:"removed,omitempty"`
	Before       []ReleaseDeployment `json:"before"`
	Deployments  []ReleaseDeployment `json:"deployments"`
}

// RepairDeploymentsResult is the answer of POST
// /v1/releases/repair-deployments. Conflicts names each revision recorded on
// two releases, which the repair leaves alone.
type RepairDeploymentsResult struct {
	Project   string                `json:"project"`
	DryRun    bool                  `json:"dry_run"`
	Releases  []ReleaseRepairChange `json:"releases"`
	Conflicts []string              `json:"conflicts,omitempty"`
}

// RepairReleaseDeployments rebuilds a project's deployment history (POST
// /v1/releases/repair-deployments): one row per (target, revision) of a
// release, each target's rows chained in since order. dryRun writes nothing.
func (c *HTTPClient) RepairReleaseDeployments(ctx context.Context, project string, dryRun bool) (*RepairDeploymentsResult, error) {
	var r RepairDeploymentsResult
	body := map[string]any{"project": project, "dry_run": dryRun}
	if err := c.doJSON(ctx, http.MethodPost, "/v1/releases/repair-deployments", body, &r); err != nil {
		return nil, fmt.Errorf("repairing %s deployments: %w", project, err)
	}
	return &r, nil
}
