package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"
)

// mrLiveState is the authoritative GitLab verdict for a merge request, used to
// break the merge→close race (bug ka-0000004113) that the canonical
// type=merge-request bead cannot: that bead is a bridge PROJECTION and trails a
// synchronous `glab mr merge` by tens of seconds.
//
// Measured on the reported incident (gardenio MR !6746):
//
//	17:26:47  MR merged on GitLab (merged_at, authoritative)
//	17:27:12  `kd close` refused — canonical bead still said mr_state=opened
//	17:27:15  bridge projected mr_state=merged — 3s AFTER the refusal
//
// So the guard refused on 20-minute-stale `opened` evidence for an MR that had
// been merged 25s earlier.
type mrLiveState int

const (
	// mrLiveUnknown means we could not establish live state (no token, network
	// error, non-200, unparseable url/body). It is NOT evidence of anything and
	// must never flip a verdict on its own.
	mrLiveUnknown mrLiveState = iota
	// mrLiveMerged is a positive, authoritative "this MR is merged".
	mrLiveMerged
	// mrLiveOpen is a positive, authoritative "this MR is still open".
	mrLiveOpen
)

// mrLiveTimeout bounds the authoritative check. `kd close` is interactive, and
// this call sits between the agent and its close, so it must fail fast: a slow
// or unreachable GitLab degrades to mrLiveUnknown (the pre-existing
// projection-only behavior) rather than hanging the CLI.
const mrLiveTimeout = 5 * time.Second

// checkMRLiveState is swapped by tests. Production points at fetchMRLiveState.
var checkMRLiveState = fetchMRLiveState

// fetchMRLiveState asks GitLab directly whether mrURL is merged.
//
// It is deliberately best-effort and returns mrLiveUnknown for every failure
// mode, because its ONLY job is to supply positive evidence that can acquit an
// MR the projection still calls `opened`. It runs solely on the about-to-refuse
// path (see beadHasOpenMR), so the common close pays no network cost.
func fetchMRLiveState(ctx context.Context, mrURL string) mrLiveState {
	m := mrURLPattern.FindStringSubmatch(mrURL)
	if m == nil {
		return mrLiveUnknown
	}
	u, err := url.Parse(mrURL)
	if err != nil || u.Host == "" {
		return mrLiveUnknown
	}
	projectPath, iid := m[1], m[2]

	// GITLAB_TOKEN is the platform-wide spelling (agent pods, CI); GLAB_TOKEN is
	// the glab CLI's, present wherever an agent can already run `glab mr merge`
	// — precisely the caller who hits this race.
	token := os.Getenv("GITLAB_TOKEN")
	if token == "" {
		token = os.Getenv("GLAB_TOKEN")
	}
	if token == "" {
		return mrLiveUnknown // unauthenticated: cannot verify, so claim nothing
	}

	ctx, cancel := context.WithTimeout(ctx, mrLiveTimeout)
	defer cancel()

	endpoint := fmt.Sprintf("%s://%s/api/v4/projects/%s/merge_requests/%s",
		u.Scheme, u.Host, url.PathEscape(projectPath), iid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return mrLiveUnknown
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return mrLiveUnknown
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return mrLiveUnknown
	}

	var payload struct {
		State string `json:"state"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return mrLiveUnknown
	}
	switch payload.State {
	case "merged":
		return mrLiveMerged
	case "opened":
		return mrLiveOpen
	default:
		// "closed" / "locked" / anything new: not merged, but also not the
		// `opened` the guard exists to catch. Leave the projection's verdict
		// standing rather than inventing one.
		return mrLiveUnknown
	}
}
