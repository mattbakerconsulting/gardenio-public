package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/ciwait"
)

// The CI-wait poll fallback (plan gardenio/plans/wake-on-ci-completion.md
// §2.5, task gd-rntlUTWMtI). GitHub does not retry a failed delivery, and a
// run whose deliveries arrived while the bridge was down (or before the App
// webhook pointed here at all) is in no ledger. So the sweep also lists the
// sha's runs from the Actions API and records them into the SAME ledger, which
// the SAME ciwait.Evaluate then decides: there is no second resolution path.
//
// Two polls, both from apply:
//   - the periodic poll: once a waiter has been seen for ciWaitPollGrace, every
//     ciWaitPollInterval. It is what resolves a wait whose deliveries were lost.
//   - the confirm poll: when Evaluate says resolved but no poll has looked at
//     the sha since the ledger last changed. It catches a run the webhook never
//     reported, and a workflow_run-chained workflow requested just after the
//     poll that saw its parent complete, before a wait resolves without it. A
//     failed confirm poll does not hold the resolution back.
//
// The token is the bridge's installation token (githubapp.BridgeTokenPermissions
// carries actions:read), so this needs no crew PAT (plan Q3).

const (
	defaultCIWaitPollInterval = 5 * time.Minute
	// defaultCIWaitPollGrace is how long a waiter must be known before the
	// first periodic poll: the webhook normally settles a short CI run first.
	defaultCIWaitPollGrace = 2 * time.Minute
	// ciWaitRunsPerPage and ciWaitMaxRunPages bound one listing. A sha has a
	// handful of workflows; 300 runs is past anything real.
	ciWaitRunsPerPage = 100
	ciWaitMaxRunPages = 3
)

// ciRunLister lists the workflow runs GitHub has for one commit.
type ciRunLister interface {
	WorkflowRuns(ctx context.Context, repo, sha string) ([]ciwait.Run, error)
}

// pollDueLocked reports whether e's sha is due a periodic poll. h.mu must be
// held.
func (h *ciWaitHandler) pollDueLocked(e *ciWaitEntry, now time.Time) bool {
	if h.runs == nil || now.Sub(e.waited) < h.pollGrace {
		return false
	}
	return e.polled.IsZero() || now.Sub(e.polled) >= h.pollInterval
}

// unconfirmedLocked reports whether the sha's ledger changed since its last
// poll (or was never polled). h.mu must be held.
func (h *ciWaitHandler) unconfirmedLocked(key string) bool {
	e := h.ledgers[key]
	return h.runs != nil && e != nil && !e.polled.After(e.ledger.LastChange)
}

// poll lists the runs for waitKey ("OWNER/REPO@sha") and records them into the
// ledger at key. The poll time is both e.polled and the time each changed run
// is recorded at, so a poll that changed the ledger is not its own
// confirmation. A failed poll still counts as a poll for the interval, so an
// API outage costs one request per waiter per interval.
func (h *ciWaitHandler) poll(ctx context.Context, waitKey, key string) error {
	h.mu.Lock()
	at := h.now()
	h.entryLocked(key, at).polled = at
	h.mu.Unlock()

	repo, sha, _ := strings.Cut(waitKey, "@")
	if _, err := beadsapi.CIWaitKey(repo, sha); err != nil {
		return err
	}
	runs, err := h.runs.WorkflowRuns(ctx, repo, strings.ToLower(sha))
	if err != nil {
		h.logger.Warn("ci wait: listing workflow runs failed", "key", waitKey, "error", err)
		return err
	}

	h.mu.Lock()
	defer h.mu.Unlock()
	e := h.entryLocked(key, h.now())
	changed := 0
	for _, r := range runs {
		if e.ledger.Record(r, at) {
			changed++
		}
	}
	h.logger.Debug("ci wait: polled workflow runs", "key", waitKey, "runs", len(runs), "changed", changed)
	return nil
}

// WorkflowRuns lists the workflow runs on sha in repo
// (GET /repos/{repo}/actions/runs?head_sha=). Each run is its latest attempt,
// the same shape a workflow_run delivery carries.
func (c *githubCommenter) WorkflowRuns(ctx context.Context, repo, sha string) ([]ciwait.Run, error) {
	var out []ciwait.Run
	for page := 1; page <= ciWaitMaxRunPages; page++ {
		var resp struct {
			TotalCount int             `json:"total_count"`
			Runs       []ciWorkflowRun `json:"workflow_runs"`
		}
		path := fmt.Sprintf("/repos/%s/actions/runs?head_sha=%s&exclude_pull_requests=true&per_page=%d&page=%d",
			repo, url.QueryEscape(sha), ciWaitRunsPerPage, page)
		if err := c.do(ctx, repo, http.MethodGet, path, nil, http.StatusOK, &resp); err != nil {
			return nil, err
		}
		for _, wr := range resp.Runs {
			if !strings.EqualFold(wr.HeadSHA, sha) {
				continue
			}
			out = append(out, ciwait.Run{ID: wr.ID, Name: wr.Name, Attempt: wr.RunAttempt, Status: wr.Status, Conclusion: wr.Conclusion})
		}
		if len(resp.Runs) < ciWaitRunsPerPage || page*ciWaitRunsPerPage >= resp.TotalCount {
			break
		}
	}
	return out, nil
}
