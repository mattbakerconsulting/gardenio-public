package bridge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// This file adds the read-only GitLab MR endpoints the R5 Trusted-Throughput
// metrics pipeline (ka-0000000493, source bundle ka-0000008221 §3.4) needs but
// the base GitLabClient did not yet expose: per-MR diff stats (lines
// added/removed, files changed) and the MR approval state. Both are pure GETs
// against the dark GitLab MR API — they never write upstream and honor the
// read-only-mirror invariant (ka-0000003234).

// GitLabMRDiffStats is the diff substance of one merge request: total lines
// added/removed and files changed. Diff size (#6) and the verbosity ratio (#7)
// in the R5 metric table are computed from these numbers.
type GitLabMRDiffStats struct {
	// Additions / Deletions are summed across every file in the MR diff.
	Additions int
	Deletions int
	// FilesChanged is the number of distinct file entries in the diff.
	FilesChanged int
	// CodeDiffChars is the byte length of all diff hunk text concatenated. It
	// is the denominator of the verbosity ratio (#7): description_chars /
	// (1 + code_diff_chars). Counting raw diff chars (not just LOC) keeps the
	// ratio robust to one-token-per-line vs dense-line changes.
	CodeDiffChars int
}

// LinesChanged is additions + deletions — the "lines_added+lines_removed"
// quantity the diff-size metric (#6) is a percentile of.
func (s GitLabMRDiffStats) LinesChanged() int { return s.Additions + s.Deletions }

// mrChangesResponse is the subset of GET .../merge_requests/:iid/changes we
// consume. GitLab returns the full MR object plus a `changes` array; we only
// read the per-file diff text and rename/new/delete flags.
type mrChangesResponse struct {
	Changes []struct {
		Diff        string `json:"diff"`
		NewFile     bool   `json:"new_file"`
		RenamedFile bool   `json:"renamed_file"`
		DeletedFile bool   `json:"deleted_file"`
	} `json:"changes"`
}

// GetMergeRequestDiffStats fetches the per-file diff for an MR and reduces it to
// added/removed line counts, files changed, and total diff-char size.
// GET /projects/:id/merge_requests/:iid/changes
//
// GitLab does not return aggregate line counts on this endpoint, so we parse
// the unified-diff hunk text: a line beginning with "+" (not "+++") is an
// addition, "-" (not "---") a deletion. This mirrors how GitLab's own UI
// computes the +N/-M badge.
func (c *GitLabClient) GetMergeRequestDiffStats(ctx context.Context, projectPath string, mrIID int) (*GitLabMRDiffStats, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/changes", encoded, mrIID)
	var resp mrChangesResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("GitLab get MR changes %s!%d: %w", projectPath, mrIID, err)
	}

	stats := &GitLabMRDiffStats{}
	for _, ch := range resp.Changes {
		stats.FilesChanged++
		stats.CodeDiffChars += len(ch.Diff)
		add, del := countDiffLines(ch.Diff)
		stats.Additions += add
		stats.Deletions += del
	}
	return stats, nil
}

// countDiffLines counts added/removed lines in a single unified-diff hunk body.
// File-header lines ("+++ b/foo", "--- a/foo") and hunk headers ("@@ ...") are
// excluded so they don't inflate the count.
func countDiffLines(diff string) (added, removed int) {
	for line := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			// File header — not a content line.
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return added, removed
}

// GitLabMRApprovals is the approval state of a merge request, used by the
// rework-cycles (#4) and trust (#3 balancing "approval-without-comment") views.
type GitLabMRApprovals struct {
	// Approved is true when the MR has met its required approvals.
	Approved bool
	// ApprovedBy is the set of usernames that have approved.
	ApprovedBy []string
	// ApprovalsRequired / ApprovalsLeft mirror the GitLab fields when present.
	ApprovalsRequired int
	ApprovalsLeft     int
}

// mrApprovalsResponse is the subset of GET .../merge_requests/:iid/approvals we
// consume.
type mrApprovalsResponse struct {
	Approved          bool `json:"approved"`
	ApprovalsRequired int  `json:"approvals_required"`
	ApprovalsLeft     int  `json:"approvals_left"`
	ApprovedBy        []struct {
		User GitLabUser `json:"user"`
	} `json:"approved_by"`
}

// ListMergeRequestApprovals fetches the approval state of an MR.
// GET /projects/:id/merge_requests/:iid/approvals
//
// The /approvals endpoint is a GitLab Premium feature; on a Community-Edition
// dark instance it returns 404. That is not an error for metrics purposes — it
// just means "no approval gate configured", so we return a zero-value
// (un-approved, zero-required) struct and let the caller fall back to the
// approved/thumbs-up note heuristic. Any other failure is surfaced.
func (c *GitLabClient) ListMergeRequestApprovals(ctx context.Context, projectPath string, mrIID int) (*GitLabMRApprovals, error) {
	encoded := url.PathEscape(projectPath)
	path := fmt.Sprintf("/api/v4/projects/%s/merge_requests/%d/approvals", encoded, mrIID)
	var resp mrApprovalsResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		if isNotFound(err) {
			return &GitLabMRApprovals{}, nil
		}
		return nil, fmt.Errorf("GitLab get MR approvals %s!%d: %w", projectPath, mrIID, err)
	}

	out := &GitLabMRApprovals{
		Approved:          resp.Approved,
		ApprovalsRequired: resp.ApprovalsRequired,
		ApprovalsLeft:     resp.ApprovalsLeft,
	}
	for _, a := range resp.ApprovedBy {
		if a.User.Username != "" {
			out.ApprovedBy = append(out.ApprovedBy, a.User.Username)
		}
	}
	return out, nil
}

// isNotFound reports whether err is a GitLab 404 surfaced by doJSON, which
// formats status as "returned 404:". Used to treat the Premium-only /approvals
// endpoint as "no approval gate" on a CE instance rather than a hard error.
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "returned 404")
}
