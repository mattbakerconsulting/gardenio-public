package main

// Deployment recording (plan gd-UZKZ9H25UD step 4b, releases-design.md §4.3,
// "How your project's deploys are recorded"): a project whose CD creates
// GitHub Deployments gets a deployment row on its release for every
// `deployment_status` of state success, via=github, by=github:<creator>, at
// revision <deployment id>. failure, error, inactive and the in-between states
// write nothing.
//
// The release is the one the deployment's ref names as a tag, else the one
// recorded at its sha. The target is the deployment's environment; one the
// project does not declare is declared by the daemon as a github-environment
// target (deployRelease, via=github), so the bridge never writes the project
// bead. The write goes through the release recorder's machine principal, whose
// role grants deploy on type=release.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// deploymentShaReleasesRead is how many of a project's newest releases are
// searched for the deployment's sha when its ref is not a recorded tag.
const deploymentShaReleasesRead = 50

// releaseDeployer writes a deployment row (the machine principal's client).
type releaseDeployer interface {
	DeployRelease(ctx context.Context, ref, project string, req beadsapi.DeployReleaseRequest) (*beadsapi.Release, error)
}

var _ releaseDeployer = (*beadsapi.Client)(nil)

// deploymentStatus is the part of a deployment_status payload the recorder
// reads.
type deploymentStatus struct {
	State     string `json:"state"`
	CreatedAt string `json:"created_at"`
}

type deploymentOf struct {
	ID          int64  `json:"id"`
	Ref         string `json:"ref"`
	SHA         string `json:"sha"`
	Environment string `json:"environment"`
	Creator     struct {
		Login string `json:"login"`
	} `json:"creator"`
}

// parseDeploymentStatus reads a deployment_status payload; ok is false for
// another event, a payload that does not decode, or one with no environment.
func parseDeploymentStatus(ev Event) (deploymentStatus, deploymentOf, bool) {
	var p struct {
		DeploymentStatus deploymentStatus `json:"deployment_status"`
		Deployment       deploymentOf     `json:"deployment"`
	}
	if ev.Type != "deployment_status" || json.Unmarshal(ev.Payload, &p) != nil {
		return deploymentStatus{}, deploymentOf{}, false
	}
	p.Deployment.Environment = strings.TrimSpace(p.Deployment.Environment)
	return p.DeploymentStatus, p.Deployment, p.Deployment.Environment != ""
}

// handleDeploymentStatus records a successful GitHub Deployment on the
// release it names, for each release-enabled project whose repo it is.
func (h *releaseRecorder) handleDeploymentStatus(ctx context.Context, ev Event) error {
	if h.deploys == nil || ev.Repo == "" {
		return nil
	}
	st, dep, ok := parseDeploymentStatus(ev)
	if !ok || st.State != "success" {
		return nil
	}
	projects, err := h.releaseProjectsList(ctx)
	if err != nil {
		return err
	}
	for _, p := range projects {
		if !strings.EqualFold(p.repo, ev.Repo) {
			continue
		}
		rel, err := h.deployedRelease(ctx, p, dep)
		if err != nil {
			return fmt.Errorf("deployment %d of %s: %w", dep.ID, p.name, err)
		}
		if rel == nil {
			h.logger.Info("deployment: no recorded release for the deployment's ref or sha",
				"project", p.name, "deployment", dep.ID, "ref", dep.Ref, "sha", dep.SHA, "environment", dep.Environment)
			continue
		}
		req := beadsapi.DeployReleaseRequest{
			Target:   dep.Environment,
			Since:    st.CreatedAt,
			Via:      "github",
			Revision: strconv.FormatInt(dep.ID, 10),
		}
		if dep.Creator.Login != "" {
			req.By = "github:" + dep.Creator.Login
		}
		out, err := h.deploys.DeployRelease(ctx, rel.ID, "", req)
		if err != nil {
			return fmt.Errorf("recording deployment %d of %s %s: %w", dep.ID, p.name, rel.Tag, err)
		}
		h.logger.Info("deployment recorded",
			"project", p.name, "tag", rel.Tag, "release", rel.ID, "target", dep.Environment,
			"deployment", dep.ID, "by", req.By, "status", out.Status)
	}
	return nil
}

// deployedRelease is the release a deployment runs: the one its ref names as
// a tag (recorded first when the tag matches the pattern and the tag webhook
// has not landed yet), else the newest one recorded at its sha. nil when
// neither is a release of p.
func (h *releaseRecorder) deployedRelease(ctx context.Context, p releaseProject, dep deploymentOf) (*beadsapi.Release, error) {
	if tag := strings.TrimPrefix(dep.Ref, "refs/tags/"); tag != "" && p.pattern.MatchString(tag) {
		rel, err := h.daemon.GetRelease(ctx, tag, p.name)
		if isNotFound(err) {
			if err := h.record(ctx, p, tag); err != nil {
				return nil, fmt.Errorf("recording release %s: %w", tag, err)
			}
			rel, err = h.daemon.GetRelease(ctx, tag, p.name)
		}
		if err != nil {
			return nil, err
		}
		return rel, nil
	}
	if dep.SHA == "" {
		return nil, nil
	}
	rels, err := h.projects.ListReleases(ctx, p.name, "", deploymentShaReleasesRead)
	if err != nil {
		return nil, err
	}
	for i := range rels {
		if strings.EqualFold(rels[i].SHA, dep.SHA) {
			return &rels[i], nil
		}
	}
	return nil, nil
}

func isNotFound(err error) bool {
	var apiErr *beadsapi.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}
