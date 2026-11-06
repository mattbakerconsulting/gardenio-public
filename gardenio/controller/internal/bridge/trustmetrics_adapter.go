package bridge

import (
	"context"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/trustmetrics"
)

// This file adapts the existing dark GitLabClient and beadsapi.Client to the
// narrow trustmetrics source interfaces (GitLabSource / BeadSource). It is the
// only place the R5 metrics pipeline (ka-0000000493) touches bridge/beadsapi
// types — the trustmetrics package itself stays dependency-free and unit-tested
// with fakes.

// TrustMetricsGitLabSource adapts *GitLabClient to trustmetrics.GitLabSource.
// It applies the "which MRs are trickle (dark→upstream delivery) MRs" policy:
// every MR in the dark group, updated within the window, targeting the
// integration/main branch. The group scan is the same one GitLabPoller uses.
type TrustMetricsGitLabSource struct {
	gitlab  *GitLabClient
	groupID int
	// targetBranches restricts to MRs delivering to these branches (the trickle
	// destination). Empty = no branch filter (every group MR is considered).
	targetBranches map[string]struct{}
}

// NewTrustMetricsGitLabSource builds the adapter. targetBranches is the set of
// branches a trickle MR targets (e.g. "main"); pass nil/empty for no filter.
func NewTrustMetricsGitLabSource(gitlab *GitLabClient, groupID int, targetBranches []string) *TrustMetricsGitLabSource {
	set := map[string]struct{}{}
	for _, b := range targetBranches {
		if b != "" {
			set[b] = struct{}{}
		}
	}
	return &TrustMetricsGitLabSource{gitlab: gitlab, groupID: groupID, targetBranches: set}
}

// ListTrickleMRs lists the dark group's MRs updated after `since`, filtered to
// the trickle target branches, projected to trustmetrics.MRMeta.
func (s *TrustMetricsGitLabSource) ListTrickleMRs(ctx context.Context, since time.Time) ([]trustmetrics.MRMeta, error) {
	mrs, err := s.gitlab.ListGroupMergeRequests(ctx, s.groupID, "all", since)
	if err != nil {
		return nil, err
	}
	out := make([]trustmetrics.MRMeta, 0, len(mrs))
	for _, mr := range mrs {
		if len(s.targetBranches) > 0 {
			if _, ok := s.targetBranches[mr.TargetBranch]; !ok {
				continue
			}
		}
		out = append(out, trustmetrics.MRMeta{
			ProjectPath: projectPathFromWebURL(mr.WebURL),
			IID:         mr.IID,
			State:       mr.State,
			WebURL:      mr.WebURL,
			OpenedAt:    parseGitLabTime(mr.CreatedAt),
			MergedAt:    parseGitLabTimePtr(mr.MergedAt),
			Description: mr.Description,
		})
	}
	return out, nil
}

// DiffStats fetches per-MR diff stats.
func (s *TrustMetricsGitLabSource) DiffStats(ctx context.Context, projectPath string, iid int) (trustmetrics.DiffStats, error) {
	ds, err := s.gitlab.GetMergeRequestDiffStats(ctx, projectPath, iid)
	if err != nil {
		return trustmetrics.DiffStats{}, err
	}
	return trustmetrics.DiffStats{
		Additions:     ds.Additions,
		Deletions:     ds.Deletions,
		FilesChanged:  ds.FilesChanged,
		CodeDiffChars: ds.CodeDiffChars,
	}, nil
}

// Approvals fetches per-MR approval state.
func (s *TrustMetricsGitLabSource) Approvals(ctx context.Context, projectPath string, iid int) (trustmetrics.Approvals, error) {
	ap, err := s.gitlab.ListMergeRequestApprovals(ctx, projectPath, iid)
	if err != nil {
		return trustmetrics.Approvals{}, err
	}
	return trustmetrics.Approvals{Approved: ap.Approved, ApprovedBy: ap.ApprovedBy}, nil
}

// Notes fetches per-MR notes.
func (s *TrustMetricsGitLabSource) Notes(ctx context.Context, projectPath string, iid int) ([]trustmetrics.Note, error) {
	notes, err := s.gitlab.ListMRNotes(ctx, projectPath, iid)
	if err != nil {
		return nil, err
	}
	out := make([]trustmetrics.Note, 0, len(notes))
	for _, n := range notes {
		out = append(out, trustmetrics.Note{Author: n.Author.Username, System: n.System, Body: n.Body})
	}
	return out, nil
}

// TrustMetricsBeadSource adapts *beadsapi.Client to trustmetrics.BeadSource. It
// resolves an MR web URL to its type=merge-request bead, then walks the
// producing peel-request bead(s) for the peel-request timestamp, the
// merge-time trust survey score, and the escaped-defect marker.
type TrustMetricsBeadSource struct {
	beads *beadsapi.Client
	// trustScoreField is the bead field the merge-time micro-survey (#3, sibling
	// work) writes the 1-5 confidence score into. Default "trust_score".
	trustScoreField string
	// escapedDefectField marks an MR/peel bead whose delivered change was later
	// reverted / caused an incident (#1/#4 balancing). Default "escaped_defect".
	escapedDefectField string
}

// NewTrustMetricsBeadSource builds the bead adapter with default field names.
func NewTrustMetricsBeadSource(beads *beadsapi.Client) *TrustMetricsBeadSource {
	return &TrustMetricsBeadSource{
		beads:              beads,
		trustScoreField:    "trust_score",
		escapedDefectField: "escaped_defect",
	}
}

// PeelInfoForMR resolves the beads-side facts for an MR by web URL.
func (s *TrustMetricsBeadSource) PeelInfoForMR(ctx context.Context, mrWebURL string) (trustmetrics.PeelInfo, bool, error) {
	mrBead, err := s.beads.MRBeadByURL(ctx, mrWebURL)
	if err != nil {
		return trustmetrics.PeelInfo{}, false, err
	}
	if mrBead == nil {
		return trustmetrics.PeelInfo{}, false, nil
	}

	info := trustmetrics.PeelInfo{
		TrustScore:    atoiSafe(mrBead.Fields[s.trustScoreField]),
		EscapedDefect: isTruthy(mrBead.Fields[s.escapedDefectField]),
	}

	producers, err := s.beads.ProducingTaskBeadsForMR(ctx, mrBead.ID)
	if err != nil {
		// The MR bead exists; a producing-bead walk failure still lets the
		// GitLab-side metrics through. Report the MR as found with no peel link.
		return info, true, nil
	}
	for _, p := range producers {
		if p == nil {
			continue
		}
		info.HasProvenanceBeadLink = true
		// Earliest producing-bead creation time is the peel request (#10).
		if t := p.CreatedAt.UTC(); !t.IsZero() {
			if info.PeelRequestedAt.IsZero() || t.Before(info.PeelRequestedAt) {
				info.PeelRequestedAt = t
			}
		}
		// A producing bead may also carry the survey score / defect marker.
		if info.TrustScore == 0 {
			info.TrustScore = atoiSafe(p.Fields[s.trustScoreField])
		}
		if !info.EscapedDefect {
			info.EscapedDefect = isTruthy(p.Fields[s.escapedDefectField])
		}
	}
	return info, true, nil
}

// --- small parse helpers ---

func parseGitLabTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func parseGitLabTimePtr(s *string) time.Time {
	if s == nil {
		return time.Time{}
	}
	return parseGitLabTime(*s)
}

func atoiSafe(s string) int {
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return n
}

func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "y":
		return true
	default:
		return false
	}
}

// projectPathFromWebURL extracts the "group/subgroup/project" path from a GitLab
// MR web URL of the form https://host/group/project/-/merge_requests/123.
func projectPathFromWebURL(webURL string) string {
	prefix, _, ok := strings.Cut(webURL, "/-/merge_requests/")
	if !ok {
		return ""
	}
	// Strip scheme://host/ to leave group/.../project.
	if _, rest, found := strings.Cut(prefix, "://"); found {
		if _, path, ok := strings.Cut(rest, "/"); ok {
			return path
		}
	}
	return strings.TrimPrefix(prefix, "/")
}
