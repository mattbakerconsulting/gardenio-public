package beadsapi

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// LookupOperatorByAliasResponse mirrors server.LookupOperatorByAliasResponse.
// Found=false means no alias row exists for the given (kind, subject) pair.
type LookupOperatorByAliasResponse struct {
	OperatorID   int64  `json:"operator_id"`
	OperatorKind string `json:"operator_kind"`
	Found        bool   `json:"found"`
}

// LookupOperatorByAlias returns the operator id and canonical operators(kind)
// for an existing alias, or Found=false when no alias exists for
// (kind, subject). Drives the beads-viewer's OIDC-subject → operator-id
// resolver (inbox-v1 G2). Viewer-role.
func (c *Client) LookupOperatorByAlias(ctx context.Context, kind, subject string) (*LookupOperatorByAliasResponse, error) {
	q := url.Values{}
	q.Set("kind", kind)
	q.Set("subject", subject)
	path := "/v1/operators/by-alias?" + q.Encode()
	var resp LookupOperatorByAliasResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("lookup operator by alias %s/%s: %w", kind, subject, err)
	}
	return &resp, nil
}

// LookupOperatorBySlugResponse mirrors server.LookupOperatorBySlugResponse.
// Found=false means no current OR retired alias row matches the slug.
type LookupOperatorBySlugResponse struct {
	OperatorID int64 `json:"operator_id"`
	Found      bool  `json:"found"`
}

// LookupOperatorBySlug resolves a slug (current or retired) to its
// operator id. Drives the /u/{slug} per-user homepage route
// (ka-0000000022 / epic ka-0000006114).
func (c *Client) LookupOperatorBySlug(ctx context.Context, slug string) (*LookupOperatorBySlugResponse, error) {
	path := "/v1/operator-slugs/" + url.PathEscape(slug)
	var resp LookupOperatorBySlugResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("lookup operator by slug %q: %w", slug, err)
	}
	return &resp, nil
}

// OperatorDirectoryEntry mirrors server.OperatorDirectoryEntry.
type OperatorDirectoryEntry struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Display string `json:"display"`
	Email   string `json:"email"`
	Slug    string `json:"slug"`
}

// ListOperatorsResponse mirrors server.ListOperatorsResponse.
type ListOperatorsResponse struct {
	Operators []OperatorDirectoryEntry `json:"operators"`
}

// ListOperators returns the public operator directory — every operator with a
// canonical slug, ordered by slug. Drives the /people page (M2 §4,
// ka-0000000340). Limit bounds the result; 0 means default (500 server-side).
func (c *Client) ListOperators(ctx context.Context, limit int) (*ListOperatorsResponse, error) {
	path := "/v1/operators"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	var resp ListOperatorsResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("list operators: %w", err)
	}
	return &resp, nil
}

// OperatorStatEntry mirrors server.OperatorStatEntry — one operator's real
// aggregate. LastActive is an RFC3339 string; the zero time
// ("0001-01-01T00:00:00Z") means the operator has no attributed beads.
//
// The per-operator counts (InProgress … GardenersActive) are additive fields
// from the §4.1 superset GROUP BY (ka-0000002003); they ride the same single
// daemon query as OpenBeads, so /people stays at 3 bounded queries total.
type OperatorStatEntry struct {
	OperatorID       int64  `json:"operator_id"`
	OpenBeads        int    `json:"open_beads"`
	InProgress       int    `json:"in_progress"`
	Created7d        int    `json:"created_7d"`
	Closed7d         int    `json:"closed_7d"`
	MRsOpen          int    `json:"mrs_open"`
	MRsMerged7d      int    `json:"mrs_merged_7d"`
	DecisionsPending int    `json:"decisions_pending"`
	AgentsActive     int    `json:"agents_active"`
	GardenersActive  int    `json:"gardeners_active"`
	LastActive       string `json:"last_active"`
}

// OperatorRecentBeadEntry mirrors server.OperatorRecentBeadEntry — one
// "recently created" chip keyed by created_by_operator_id.
type OperatorRecentBeadEntry struct {
	OperatorID int64  `json:"operator_id"`
	BeadID     string `json:"bead_id"`
	Title      string `json:"title"`
	CreatedAt  string `json:"created_at"`
}

// OperatorStatsResponse mirrors server.OperatorStatsResponse.
type OperatorStatsResponse struct {
	Stats  []OperatorStatEntry       `json:"stats"`
	Recent []OperatorRecentBeadEntry `json:"recent"`
}

// OperatorStats returns real per-operator aggregates for the /people
// directory (ka-0000001390): open/in_progress count + last-active recency
// computed via a GROUP BY operated_by_id over the whole beads table, plus a
// bounded, age-limited per-operator "recently created" chip list keyed on
// operated_by_id. This replaces the viewer's old 5000-bead in-memory sample
// whose counts were sampling artifacts.
//
// recentPerOperator caps the chips per operator; recentLimit is a hard
// ceiling on total chip rows; recentWindowHours bounds how far back a chip may
// have been created. All 0 mean "use the daemon defaults" (5 per operator,
// 5000 total, 7 days). The open-count aggregate is a true full-table GROUP BY
// and is not subject to any cap.
//
// LastActive counts HUMAN-authored bead types only — agent/session/trigger
// scaffolding heartbeats on its own and would otherwise report a person as
// active when they did nothing (ka-0000001991).
func (c *Client) OperatorStats(ctx context.Context, recentPerOperator, recentLimit, recentWindowHours int) (*OperatorStatsResponse, error) {
	q := url.Values{}
	if recentPerOperator > 0 {
		q.Set("recent_per_operator", strconv.Itoa(recentPerOperator))
	}
	if recentLimit > 0 {
		q.Set("recent_limit", strconv.Itoa(recentLimit))
	}
	if recentWindowHours > 0 {
		q.Set("recent_window_hours", strconv.Itoa(recentWindowHours))
	}
	path := "/v1/operators/stats"
	if enc := q.Encode(); enc != "" {
		path += "?" + enc
	}
	var resp OperatorStatsResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("operator stats: %w", err)
	}
	return &resp, nil
}

// GetOperatorSlugResponse mirrors server.GetOperatorSlugResponse.
type GetOperatorSlugResponse struct {
	Slug  string `json:"slug"`
	Found bool   `json:"found"`
}

// GetOperatorSlug returns the operator's current canonical slug. Used by
// /me to compute the 308 redirect target. Found=false means the operator
// has no slug — callers can fall through to `op-<id>`.
func (c *Client) GetOperatorSlug(ctx context.Context, operatorID int64) (*GetOperatorSlugResponse, error) {
	path := "/v1/operator-slugs/by-id/" + strconv.FormatInt(operatorID, 10)
	var resp GetOperatorSlugResponse
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, fmt.Errorf("get operator slug %d: %w", operatorID, err)
	}
	return &resp, nil
}
