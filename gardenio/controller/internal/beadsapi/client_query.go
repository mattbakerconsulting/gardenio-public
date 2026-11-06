package beadsapi

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// clientListAllLimit is the explicit page size the typed "list every active X"
// helpers (ListAgentBeads, ListTaskBeads, ListDecisionBeads, ListAccountBeads,
// …) send so they never silently ride the daemon's defaultListBeadsLimit
// (1000, kbeads http_beads.go). It is set to the daemon's maxListBeadsLimit so
// a single page returns the whole active set — those populations are
// status-filtered and well under it — and warnOnSilentTruncation below turns
// any breach into a loud WARN rather than a silent gap. Mirrors the
// collectAccounts fix (2f11eb5f1, ka-0000005987); class writeup in
// ka-0000003555 / ka-0000003547.
const clientListAllLimit = 10000

// warnOnSilentTruncation logs a loud WARN when a bead listing came back with
// fewer rows than the daemon's reported Total — i.e. the daemon silently
// capped the result set. This is the observability half of the fix for the
// invisible 1000-row default (ka-0000003547, class ka-0000003555): the daemon
// answers HTTP 200 and populates Total, but almost nothing in the codebase
// compares Total against len(Beads), so a caller that meant "give me
// everything" quietly receives at most defaultListBeadsLimit rows with no
// error. Making the mismatch loud is what lets the ~40 latent omitted-Limit
// call sites announce themselves the moment their population first overflows —
// instead of a correctness bug (a stale desired-pod set, a vanished account
// gauge, a re-sent JIRA comment) surfacing with no trace. Callers pass
// contextual attrs (types/statuses/labels) so the log names the offending
// query.
func warnOnSilentTruncation(returned, total int, attrs ...any) {
	if total <= returned {
		return
	}
	args := append([]any{"returned", returned, "total", total}, attrs...)
	slog.Warn("bead list silently truncated by daemon: returned fewer rows than match; caller sees an incomplete set (likely capped at the default list limit — set an explicit Limit or page)", args...)
}

// ListBeadsQuery contains the full set of query parameters for listing beads.
type ListBeadsQuery struct {
	Types        []string // Filter by bead types (e.g., "decision", "mail")
	ExcludeTypes []string // Server-side type NOT IN (...). Lets count-only callers drop noisy types without pulling rows.
	Kinds        []string // Filter by bead kinds (e.g., "issue", "data", "config")
	Statuses     []string // Filter by statuses (e.g., "open", "closed")
	Labels       []string // Filter by labels (ANDed: a bead must carry ALL)
	// LabelsAny filters on beads carrying ANY of these labels (server-side
	// OR / any-of, forwarded as ?labels_any). Distinct from Labels which ANDs.
	// Mirrors kd list --label-any semantics (see kbeads model/filter.go:51).
	LabelsAny []string
	// ExcludeLabels drops beads carrying ANY of these labels (server-side
	// `label NOT IN (...)`). Used by the gardener-delivery trigger's cron
	// preflight to skip MR beads marked with an opt-out label.
	ExcludeLabels []string
	// ExclusiveProject narrows results to beads that belong EXCLUSIVELY to the
	// named project — those carrying NO `project:<other>` label (forwarded as
	// ?exclusive_project=, applied server-side in kbeads queries.go). A bead with
	// only `project:gardenio`, or no project label, matches; a dual-project row
	// (`project:gardenio` + `project:monorepo`) is dropped. This is the honest
	// server-side replacement for the client-side filterExclusiveProject pass:
	// because the predicate runs BEFORE LIMIT, `gb ready` / `gb news` no longer
	// over-fetch to backfill rows dropped after LIMIT, and the no-underfill
	// guarantee is exact (ka-0000003411). Empty = no filter.
	ExclusiveProject string
	Assignee         string // Filter by assignee
	// HeldBy filters escalations by held_by lane ref (or "@me"). It is served
	// by GET /v1/escalations, so ListBeadsFiltered delegates to
	// ListEscalationsHeldBy and Types must be exactly ["escalation"].
	HeldBy []string
	// Unassigned restricts results to beads with NO assignee, via the daemon's
	// `assignee IS NULL OR assignee = <empty>` predicate (forwarded as
	// ?unassigned=true). Surfaced as `gb ready --unassigned`. It is NOT
	// expressible as Assignee:"" — an empty scalar assignee reads as "no
	// assignee filter" and returns claimed beads too, so the work-seeking query
	// ("open, unblocked, unclaimed") previously had to pull the full page and
	// post-filter in jq (ka-0000007209).
	Unassigned bool
	// Assignees filters on assignee ∈ set via the daemon's
	// `assignee = ANY($::text[])` predicate (forwarded as ?assignee_in=csv;
	// OR-merged with the scalar Assignee daemon-side). The beads-viewer's
	// bidirectional inbox uses this to list `type=mail` addressed to any of
	// an operator's recipient handles in one query (design §4.2,
	// ka-0000007555). Empty = no filter.
	Assignees []string
	// CreatedBy filters on the actor who created the bead (forwarded as
	// ?created_by=, parsed in kbeads http_beads.go). This is the ownership
	// column for beads that are never self-assigned: `gb escalation create`
	// stamps no assignee at all, so an escalation's only durable "who filed
	// this" signal is created_by. Surfaced as `gb decision list --mine` /
	// `gb escalation list --mine` (ka-0000007853). Empty = no filter.
	CreatedBy string
	// Owner filters on the bead's Owner column (forwarded as ?owner=, parsed
	// in kbeads http_beads.go). Owner defaults to the creating operator's
	// canonical alias at create time but is independently editable
	// thereafter (kbeads auto_owner.go / owner_edit.go) — a plain string
	// column, not alias-resolved. Empty = no filter.
	Owner string
	// OperatedByAliases filters on canonical "kind:subject" alias strings
	// (e.g. "oidc:user@x", "slack:U123…"). The daemon resolves each alias to
	// its operators(id) FK via operator_aliases, so passing multiple aliases
	// for the same human surfaces cross-identity rows. The TEXT-keyed
	// operated_by column and operated_by_is_null filter were dropped in
	// daemon migration 024 (P6); use OperatedByID for direct FK equality.
	OperatedByAliases []string
	Search            string // Full-text search
	// Titles narrows to beads whose title is exactly one of these (forwarded as
	// repeated ?title=, kbeads BeadFilter.Titles). Unlike Search it is not
	// fuzzy, so no other bead can crowd an exact match off a short page.
	Titles            []string
	Sort              string // Sort field (e.g., "priority", "created_at")
	NoOpenDeps        bool   // Only return beads with no open/in_progress/deferred dependencies
	ShortTitleMissing bool   // Only return beads whose short_title is unset (gd-V5nGseRS5f)
	ExcludeScheduled  bool   // Hide scheduled-task beads (fields.schedule_id IS NOT NULL); see ka-0000006652
	Parent            string // Restrict to transitive subtree under the given bead-id (parent-child deps)
	// HeraldryProject lists the project's arms register: the beads bearing arms in it, whatever their arms (forwarded as
	// ?heraldry_project=, kbeads http_beads.go; heraldry spec R5). Empty = no filter.
	HeraldryProject string
	// Categories filters on the materialized beads.category column (intake /
	// intermediate / action / config). Inbox v1 §1.2.1 query shape.
	Categories []string
	// OperatedByID filters on the operators(id) FK. 0 means "no filter".
	// Used by /inbox handler to issue the single-FK-equality query.
	OperatedByID int64
	// UpdatedAfter filters to beads whose updated_at is strictly greater
	// than this timestamp. Zero value = no filter. Used by the bridge's
	// trigger:bead reconciler (ka-0000004769) to catch up after SSE outages.
	UpdatedAfter time.Time
	// UpdatedBefore filters to beads whose updated_at is strictly less than
	// this timestamp. Zero value = no filter. Forwarded as ?updated_before=
	// (parsed in kbeads http_beads.go, applied in queries.go). Combined with
	// CountOnly + a status filter it gives a cheap "stale in_progress" count
	// per project for the index health badge (ka-0000002729): set it to
	// now-24h to count beads no agent has touched in a day, the same staleness
	// heuristic projectSummary uses (project_summary.go).
	UpdatedBefore time.Time
	// CreatedAfter filters to beads whose created_at is at or after this
	// timestamp (inclusive at the store layer: >=). Zero value = no filter.
	// Forwarded as ?created_after= (parsed in kbeads http_beads.go since
	// ka-0000008195). The spend-logs sweep uses it to track exactly the agent
	// beads whose whole life fits inside its projection window.
	CreatedAfter time.Time
	// CreatedBefore filters to beads whose created_at is at or before this
	// timestamp (inclusive at the store layer: <=). Zero value = no filter.
	// Forwarded as ?created_before= to the microsecond, so a page cursor
	// (the Commons timeline's before=) does not round to the second.
	CreatedBefore time.Time
	// ClosedAfter filters to beads whose closed_at is strictly greater than
	// this timestamp (strict > at the store layer). Zero value = no filter.
	// Forwarded as ?closed_after= (parsed in kbeads http_beads.go, MR 5670).
	// The gardener D2 throughput scorer (bridge.ScoreGardenerD2) uses it to count
	// exactly the epic children that closed inside the scoring window, the same
	// closed_at window the meta-gardener rubric §D2 defines.
	ClosedAfter time.Time
	// Fields filters on custom-field equality (fields->>'key' = 'value').
	// Each entry forwards one daemon field_filter predicate as
	// ?field=key=value (parsed in kbeads http_beads.go, applied in
	// queries.go). Used by the Gardener Page to list a gardener's iterations
	// via Fields{"schedule_id": gardenerID} — backed by the partial B-tree
	// index from daemon migration 031.
	Fields map[string]string
	// FieldsIn filters on custom-field membership (fields->>'key' IN values),
	// forwarded as ?field_in=key=a,b,c (kbeads http_beads.go). The Commons
	// reads many channels' newest messages in one list with it (gd-OYKXtzN9kr).
	// A value must not hold a comma. A daemon older than field_in ignores it
	// and answers the list without it, so a caller must still check each row.
	FieldsIn map[string][]string
	// CountOnly switches the daemon to a count-only query (no sort, no
	// projections, no row bodies). Total is returned via Total; Beads is
	// always nil. Limit / Offset / Sort are ignored.
	CountOnly bool
	// NoTotal skips the daemon's full-set COUNT(*) for a full page: Total is
	// then -1 unless the page is short (exact). Set it on pages and polls that
	// never show a total (gd-g25jihMHFs).
	NoTotal bool
	// WithHasReport / WithBlockedOnDecision opt the matching projection
	// columns into the daemon's SELECT. Default false because each is a
	// non-trivial subquery — see kbeads model.BeadFilter docs and the
	// deps/open-decisions index migrations. Set them when the caller
	// actually reads BeadDetail.HasReport / BlockedOnDecision (e.g.
	// beads-viewer's badge rendering).
	WithHasReport         bool
	WithBlockedOnDecision bool
	// WithParkedOnDecision opts the parked_on_decision_bead_id projection into
	// the daemon's SELECT (ka-0000003797) — the text-mention sibling of
	// WithBlockedOnDecision. Set it when the caller reads BeadDetail.
	// ParkedOnDecision / ParkedOnDecisionBeadID (e.g. `gb ready` / `gb news`
	// rendering the PARKED indicator). Off by default: the EXISTS probe into
	// bead_references is index-backed but not free, so default callers do not
	// pay for it.
	WithParkedOnDecision bool
	// WithParent opts the immediate parent-child parent id (and its title, when
	// in the caller's project scope) into each returned row via the daemon's
	// with_parent projection (ka-0000006083). Like WithOpenBlockers it is a
	// batched per-page deps query on the daemon, NOT an N+1 kd show. Set it when
	// the caller reads BeadDetail.ParentID — e.g. `gb ready` so a dispatcher can
	// tell a leaf task from a container and group by epic. Empty ParentID means
	// orphan / not-opted-in / old daemon.
	WithParent bool
	// Omit / OmitFields send the daemon's opt-in list projection
	// (gd-zXdZq4Traf): Omit names whole body columns ("description",
	// "notes") and OmitFields dotted paths inside fields ("prompt"), so a
	// roster poll that never reads them stops downloading them. The rows come
	// back with those empty: never write a bead listed this way back whole. An
	// older daemon ignores both and returns full rows.
	Omit       []string
	OmitFields []string
	// FieldsOnly keeps only these top-level fields keys in each row
	// (gd-6PXLuR3juD); the daemon drops every other key and wins over
	// OmitFields. Set it with OnlyFields, which also fills Omit/OmitFields so
	// a daemon that predates fields_only still drops the bulk. Same rule:
	// never write a bead listed this way back whole.
	FieldsOnly []string
	Limit      int // Max results
	Offset     int // Pagination offset
}

// onlyFieldsFallbackOmit are the heavy fields keys an OnlyFields read also
// asks the daemon to omit, for a daemon that has omit_fields but not
// fields_only. Measured 2026-10-23/24: prompt ~60% and wrapup ~8% of an agent
// row, slowest_calls ~45% of a session row, gardener and captain most of a
// gardener trigger. A key a bead does not carry is a no-op.
var onlyFieldsFallbackOmit = []string{"prompt", "wrapup", "slowest_calls", "gardener", "captain"}

// OnlyFields returns q narrowed for a walk that reads only the given fields
// keys plus the bead's columns and labels (gd-6PXLuR3juD): description and
// notes are omitted, and fields carries just keys. On a daemon without
// fields_only the rows still lose onlyFieldsFallbackOmit; on one older than
// omit they come back whole. Either way the listed keys are present.
func (q ListBeadsQuery) OnlyFields(keys ...string) ListBeadsQuery {
	q.Omit = []string{"description", "notes"}
	q.OmitFields = nil
	for _, k := range onlyFieldsFallbackOmit {
		if !slices.Contains(keys, k) {
			q.OmitFields = append(q.OmitFields, k)
		}
	}
	q.FieldsOnly = slices.Clone(keys)
	return q
}

// noFieldsKey is a fields key no bead carries. fields_only with only this key
// makes the daemon return every row with fields = {}; a bare fields_only= is
// ignored, so "no keys at all" has to be spelled as one absent key.
const noFieldsKey = "_none"

// NoFields is OnlyFields for a walk that reads only the bead's columns and
// labels (gd-6PXLuR3juD): fields comes back empty. On a daemon without
// fields_only the rows still lose onlyFieldsFallbackOmit.
func (q ListBeadsQuery) NoFields() ListBeadsQuery {
	return q.OnlyFields(noFieldsKey)
}

// ListBeadsResult is the response from a filtered bead listing.
type ListBeadsResult struct {
	Beads []*BeadDetail
	Total int
}

// MayHaveMore reports whether the match set may extend past the rows
// returned: the daemon counted more than it sent, or a NoTotal page came back
// full and uncounted (Total -1). It is the truncation test for a NoTotal
// query, which never pays the full-set COUNT (gd-I2zntgOsCq).
func (r *ListBeadsResult) MayHaveMore() bool {
	return r != nil && (r.Total < 0 || r.Total > len(r.Beads))
}

// ListBeadsFiltered queries the daemon with full query parameters.
func (c *Client) ListBeadsFiltered(ctx context.Context, q ListBeadsQuery) (*ListBeadsResult, error) {
	if len(q.HeldBy) > 0 {
		if len(q.Types) != 1 || q.Types[0] != "escalation" {
			return nil, fmt.Errorf("ListBeadsQuery.HeldBy is only valid with Types=[escalation]")
		}
		beads, err := c.ListEscalationsHeldBy(ctx, q.HeldBy, q.Statuses, q.Limit)
		if err != nil {
			return nil, err
		}
		return &ListBeadsResult{Beads: beads, Total: len(beads)}, nil
	}
	params := url.Values{}
	if len(q.Types) > 0 {
		params.Set("type", strings.Join(q.Types, ","))
	}
	if len(q.ExcludeTypes) > 0 {
		params.Set("exclude_type", strings.Join(q.ExcludeTypes, ","))
	}
	if len(q.Kinds) > 0 {
		params.Set("kind", strings.Join(q.Kinds, ","))
	}
	if len(q.Statuses) > 0 {
		params.Set("status", strings.Join(q.Statuses, ","))
	}
	if len(q.Labels) > 0 {
		params.Set("labels", strings.Join(q.Labels, ","))
	}
	if len(q.LabelsAny) > 0 {
		params.Set("labels_any", strings.Join(q.LabelsAny, ","))
	}
	if len(q.ExcludeLabels) > 0 {
		params.Set("exclude_labels", strings.Join(q.ExcludeLabels, ","))
	}
	if q.ExclusiveProject != "" {
		params.Set("exclusive_project", q.ExclusiveProject)
	}
	for _, t := range q.Titles {
		params.Add("title", t)
	}
	if q.Assignee != "" {
		params.Set("assignee", q.Assignee)
	}
	if q.Unassigned {
		params.Set("unassigned", "true")
	}
	if len(q.Assignees) > 0 {
		params.Set("assignee_in", strings.Join(q.Assignees, ","))
	}
	if q.CreatedBy != "" {
		params.Set("created_by", q.CreatedBy)
	}
	if q.Owner != "" {
		params.Set("owner", q.Owner)
	}
	if len(q.OperatedByAliases) > 0 {
		params.Set("operated_by_alias_in", strings.Join(q.OperatedByAliases, ","))
	}
	if q.Search != "" {
		params.Set("search", q.Search)
	}
	if q.Sort != "" {
		params.Set("sort", q.Sort)
	}
	if q.NoOpenDeps {
		params.Set("no_open_deps", "true")
	}
	if q.ShortTitleMissing {
		params.Set("short_title_missing", "true")
	}
	if q.ExcludeScheduled {
		params.Set("exclude_scheduled", "true")
	}
	if q.Parent != "" {
		params.Set("parent", q.Parent)
	}
	if q.HeraldryProject != "" {
		params.Set("heraldry_project", q.HeraldryProject)
	}
	if len(q.Categories) > 0 {
		params.Set("category", strings.Join(q.Categories, ","))
	}
	if q.OperatedByID != 0 {
		params.Set("operated_by_id", strconv.FormatInt(q.OperatedByID, 10))
	}
	if !q.UpdatedAfter.IsZero() {
		params.Set("updated_after", q.UpdatedAfter.UTC().Format(time.RFC3339))
	}
	if !q.UpdatedBefore.IsZero() {
		params.Set("updated_before", q.UpdatedBefore.UTC().Format(time.RFC3339))
	}
	if !q.CreatedAfter.IsZero() {
		params.Set("created_after", q.CreatedAfter.UTC().Format(time.RFC3339))
	}
	if !q.CreatedBefore.IsZero() {
		params.Set("created_before", q.CreatedBefore.UTC().Format(time.RFC3339Nano))
	}
	if !q.ClosedAfter.IsZero() {
		params.Set("closed_after", q.ClosedAfter.UTC().Format(time.RFC3339))
	}
	for k, v := range q.Fields {
		// Add (not Set) so multiple field predicates compose; the daemon
		// splits each field= value on the first '=' (http_beads.go).
		params.Add("field", k+"="+v)
	}
	for k, vs := range q.FieldsIn {
		if len(vs) > 0 {
			params.Add("field_in", k+"="+strings.Join(vs, ","))
		}
	}
	if q.CountOnly {
		params.Set("count_only", "true")
	}
	if q.NoTotal {
		params.Set("no_total", "true")
	}
	if q.WithHasReport {
		params.Set("with_has_report", "true")
	}
	if q.WithBlockedOnDecision {
		params.Set("with_blocked_on_decision", "true")
	}
	if q.WithParkedOnDecision {
		params.Set("with_parked_on_decision", "true")
	}
	if q.WithParent {
		params.Set("with_parent", "true")
	}
	if len(q.Omit) > 0 {
		params.Set("omit", strings.Join(q.Omit, ","))
	}
	if len(q.OmitFields) > 0 {
		params.Set("omit_fields", strings.Join(q.OmitFields, ","))
	}
	if len(q.FieldsOnly) > 0 {
		params.Set("fields_only", strings.Join(q.FieldsOnly, ","))
	}
	if q.Limit > 0 {
		params.Set("limit", strconv.Itoa(q.Limit))
	}
	if q.Offset > 0 {
		params.Set("offset", strconv.Itoa(q.Offset))
	}

	path := "/v1/beads"
	if len(params) > 0 {
		path += "?" + params.Encode()
	}

	var resp listBeadsResponse
	if err := c.doJSON(ctx, "GET", path, nil, &resp); err != nil {
		return nil, fmt.Errorf("listing beads: %w", err)
	}

	// Loudly surface a silent daemon truncation, but ONLY when the caller
	// omitted Limit — i.e. their intent was "give me everything" and the
	// daemon quietly capped them at the default. A caller that set an explicit
	// Limit (a paged walk, an existence-check Limit:1, a bounded preview) is
	// deliberately bounded and its Total > len is expected, not a bug; warning
	// there would be pure noise. CountOnly returns Total with a nil Beads by
	// design, so it can never be "truncated". See warnOnSilentTruncation.
	if q.Limit <= 0 && !q.CountOnly {
		warnOnSilentTruncation(len(resp.Beads), resp.Total,
			"types", q.Types, "statuses", q.Statuses, "labels", q.Labels, "kinds", q.Kinds)
	}

	beads := make([]*BeadDetail, 0, len(resp.Beads))
	for _, b := range resp.Beads {
		beads = append(beads, b.toDetail())
	}
	return &ListBeadsResult{Beads: beads, Total: resp.Total}, nil
}

// listAllPagesPageSize is the per-page size ListAllBeadsFiltered requests. Set
// to the daemon's maxListBeadsLimit (kbeads http_beads.go) so small populations
// come back in a single round-trip while the offset loop still protects larger
// ones. Requesting more than the daemon max is pointless — it clamps silently.
const listAllPagesPageSize = clientListAllLimit

// listAllPagesMaxPages caps the exhaustive walk so a daemon that ignores offset
// (or a population that grows without bound) cannot spin forever. 1000 pages ×
// 10000 = 10M beads — far beyond any real matched population, but finite.
const listAllPagesMaxPages = 1000

// ListAllBeadsFiltered walks the daemon's paged list endpoint, following offset
// until a short page signals exhaustion, and returns EVERY matching bead. It is
// the growth-proof replacement for a single capped ListBeadsFiltered call
// whenever the caller means "the whole matching set" AND that set — or a
// pre-filter superset the daemon caps FIRST — can exceed a fixed limit.
//
// The bug this fixes (ka-0000001936, class ka-0000003555): a caller that fetches
// Limit:500 of a broad population and then applies the REAL selector client-side
// truncates the WRONG population. `gb mr list` fetched 500 open/in_progress
// issue beads and applied `mr_url != ""` in filterMRBeads afterwards — so the
// cap was spent on the ~93% of beads carrying no mr_url and silently dropped
// ~45% of the MR-bearing ones. Pushing the selector server-side (Fields) AND
// paging here makes the cap irrelevant: the walk exhausts the matched set no
// matter how large it grows. Same family as ka-0000005054 (that fix widened
// Types but left the Limit half of the same hazard in place).
//
// Contract: the caller MUST leave q.Limit and q.Offset zero — the walk owns
// paging, and a caller-set Limit would re-cap every page and make the loop
// misread exhaustion. Results are deduplicated by bead ID, and the walk pins a
// stable created_at sort (created_at is immutable per bead), so a concurrent
// insert/close that shifts a page boundary mid-walk can at worst produce a
// duplicate — which the dedup drops — rather than a double-count. Total is the
// size of the match as of the last page (offset + rows), or -1 if the walk hit
// the page cap.
func (c *Client) ListAllBeadsFiltered(ctx context.Context, q ListBeadsQuery) (*ListBeadsResult, error) {
	if q.Limit != 0 || q.Offset != 0 {
		return nil, fmt.Errorf("ListAllBeadsFiltered owns paging: caller must leave Limit/Offset zero (got Limit=%d Offset=%d)", q.Limit, q.Offset)
	}
	// A stable, immutable sort key keeps the page sequence from skipping or
	// repeating rows as the underlying set shifts. created_at (ascending) also
	// appends new rows past the current offset window, so a concurrent insert
	// never shifts an already-read page. Only override an unset sort.
	if q.Sort == "" {
		q.Sort = "created_at"
	}
	q.Limit = listAllPagesPageSize
	// Only the last, short page's total is returned, and a short page's total
	// is exact for free (offset + rows), so no page needs the daemon's
	// full-set COUNT(*) (gd-g25jihMHFs).
	q.NoTotal = true

	seen := make(map[string]struct{})
	var out []*BeadDetail
	var total int
	for page := range listAllPagesMaxPages {
		q.Offset = page * listAllPagesPageSize
		res, err := c.ListBeadsFiltered(ctx, q)
		if err != nil {
			return nil, err
		}
		total = res.Total
		for _, b := range res.Beads {
			if _, dup := seen[b.ID]; dup {
				continue
			}
			seen[b.ID] = struct{}{}
			out = append(out, b)
		}
		if len(res.Beads) < listAllPagesPageSize {
			// Short page → the daemon returned every remaining match. An empty
			// page past offset 0 is not counted under NoTotal (-1); the match
			// set then ends exactly at this offset.
			if total < 0 {
				total = q.Offset + len(res.Beads)
			}
			return &ListBeadsResult{Beads: out, Total: total}, nil
		}
	}
	// Ran the page cap without ever seeing a short page: the population exceeds
	// listAllPagesMaxPages*listAllPagesPageSize (or the daemon is ignoring
	// offset). Never truncate SILENTLY — that is the exact defect this helper
	// exists to kill — so make the incompleteness loud.
	slog.Warn("ListAllBeadsFiltered hit the max-pages cap without exhausting the match set; result may be incomplete",
		"maxPages", listAllPagesMaxPages, "pageSize", listAllPagesPageSize, "returned", len(out), "total", total,
		"types", q.Types, "statuses", q.Statuses, "labels", q.Labels)
	return &ListBeadsResult{Beads: out, Total: total}, nil
}

// UpdateBeadRequest contains mutable fields for updating a bead.
type UpdateBeadRequest struct {
	Title      *string `json:"title,omitempty"`
	ShortTitle *string `json:"short_title,omitempty"`
	// Heraldry sets the bead's arms in any input form the daemon accepts;
	// "" or "none" removes them. Arms given without a colour keep the bead's.
	Heraldry    *string   `json:"heraldry,omitempty"`
	Description *string   `json:"description,omitempty"`
	Assignee    *string   `json:"assignee,omitempty"`
	Owner       *string   `json:"owner,omitempty"`
	Status      *string   `json:"status,omitempty"`
	Notes       *string   `json:"notes,omitempty"`
	Priority    *int      `json:"priority,omitempty"`
	Labels      *[]string `json:"labels,omitempty"` // nil = leave alone, non-nil = replace whole set (empty slice clears)
	// Fields is an optional partial fields patch sent in the SAME PATCH body as
	// the other columns, so a status change and a field patch land atomically in
	// one daemon write (one transaction, one event). The daemon applies additive
	// RFC-7386 merge semantics: a key with a non-nil value sets it, and a key
	// mapped to nil DELETES it (the only way to return a field to "absent", which
	// for an enum field like agent_state is the only legal "cleared" value — the
	// empty string "" is NOT in the enum and 400s). map[string]any (not
	// map[string]string) precisely so a nil value can express that delete.
	Fields map[string]any `json:"-"`
	// OnlyIfShortTitleEmpty makes the daemon apply ShortTitle only when the
	// bead has none yet, so a fill (the groomer, a create-time default) never
	// overwrites an authored short title. It qualifies ShortTitle and is sent
	// only alongside it.
	OnlyIfShortTitleEmpty bool `json:"-"`
}

// UpdateBead updates mutable fields on a bead.
func (c *Client) UpdateBead(ctx context.Context, beadID string, req UpdateBeadRequest) error {
	body := make(map[string]any)
	if req.Title != nil {
		body["title"] = *req.Title
	}
	if req.ShortTitle != nil {
		body["short_title"] = *req.ShortTitle
		if req.OnlyIfShortTitleEmpty {
			body["only_if_short_title_empty"] = true
		}
	}
	if req.Heraldry != nil {
		body["heraldry"] = *req.Heraldry
	}
	if req.Description != nil {
		body["description"] = *req.Description
	}
	if req.Assignee != nil {
		body["assignee"] = *req.Assignee
	}
	if req.Owner != nil {
		body["owner"] = *req.Owner
	}
	if req.Status != nil {
		body["status"] = *req.Status
	}
	if req.Notes != nil {
		body["notes"] = *req.Notes
	}
	if req.Priority != nil {
		body["priority"] = *req.Priority
	}
	if req.Labels != nil {
		// Non-nil slice replaces the whole label set, even when empty (clears all labels).
		body["labels"] = *req.Labels
	}
	if len(req.Fields) > 0 {
		// Send fields in the SAME PATCH body so a status change and a field patch
		// land atomically in one daemon write (one transaction, one event). The
		// server applies additive RFC-7386 merge semantics on the `fields` object:
		// only the keys sent here change (untouched fields are preserved), and a
		// key mapped to nil is DELETED. This is what lets the plan-gardener wake
		// reopen a closed gardener AND clear its terminal agent_state in a single
		// event (ka-0000001583): agent_state is an enum with no "" member, so the
		// clear MUST be a delete (nil), not an empty string.
		body["fields"] = req.Fields
	}
	if len(body) == 0 {
		return nil
	}
	if err := c.doJSON(ctx, "PATCH", "/v1/beads/"+url.PathEscape(beadID), body, nil); err != nil {
		return fmt.Errorf("updating bead %s: %w", beadID, err)
	}
	return nil
}

// DeleteBead deletes a bead by ID.
func (c *Client) DeleteBead(ctx context.Context, beadID string) error {
	if err := c.doJSON(ctx, "DELETE", "/v1/beads/"+url.PathEscape(beadID), nil, nil); err != nil {
		return fmt.Errorf("deleting bead %s: %w", beadID, err)
	}
	return nil
}

// AddDependency adds a dependency between beads.
func (c *Client) AddDependency(ctx context.Context, beadID, dependsOnID, depType, createdBy string) error {
	body := map[string]string{
		"depends_on_id": dependsOnID,
		"type":          depType,
		"created_by":    createdBy,
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/dependencies"
	if err := c.doJSON(ctx, "POST", path, body, nil); err != nil {
		return fmt.Errorf("adding dependency on %s: %w", beadID, err)
	}
	return nil
}

// RemoveDependency deletes a single dependency edge (beadID depends on
// dependsOnID with the given depType) via DELETE
// /v1/beads/{id}/dependencies?depends_on_id=...&type=.... An empty depType
// removes the edge regardless of type. Mirrors AddDependency; used by
// `gb bundle remove` to detach a doc from its bundle parent (ka-0000004867).
func (c *Client) RemoveDependency(ctx context.Context, beadID, dependsOnID, depType string) error {
	path := "/v1/beads/" + url.PathEscape(beadID) + "/dependencies" +
		"?depends_on_id=" + url.QueryEscape(dependsOnID)
	if depType != "" {
		path += "&type=" + url.QueryEscape(depType)
	}
	if err := c.doJSON(ctx, "DELETE", path, nil, nil); err != nil {
		return fmt.Errorf("removing dependency %s -> %s: %w", beadID, dependsOnID, err)
	}
	return nil
}

// GetDependencies lists dependencies for a bead.
func (c *Client) GetDependencies(ctx context.Context, beadID string) ([]Dependency, error) {
	var resp struct {
		Dependencies []Dependency `json:"dependencies"`
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/dependencies"
	if err := c.doJSON(ctx, "GET", path, nil, &resp); err != nil {
		return nil, fmt.Errorf("getting dependencies for %s: %w", beadID, err)
	}
	return resp.Dependencies, nil
}

// GetReverseDependencies lists beads that depend on this bead. The returned
// Dependency rows have BeadID set to the dependent bead's ID and DependsOnID
// set to the requested bead's ID — i.e. each row reads "BeadID depends on us".
func (c *Client) GetReverseDependencies(ctx context.Context, beadID string) ([]Dependency, error) {
	var resp struct {
		Dependencies []Dependency `json:"dependencies"`
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/dependents"
	if err := c.doJSON(ctx, "GET", path, nil, &resp); err != nil {
		return nil, fmt.Errorf("getting dependents for %s: %w", beadID, err)
	}
	return resp.Dependencies, nil
}

// Dependency represents a bead dependency.
type Dependency struct {
	BeadID      string `json:"bead_id"`
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
	// BeadType is the type of the DEPENDENT bead (BeadID) — the child on a
	// reverse (parent → children) walk. The daemon populates it only on the
	// batch reverse-dependency response, so a caller enumerating a parent's
	// children can filter by child type before hydrating each child's full
	// record. Empty on every other path (forward deps, single-bead deps) and
	// against an older daemon that predates the field — callers must treat ""
	// as "unknown" and fall back to hydration (ka-0000003113).
	BeadType string `json:"bead_type,omitempty"`
}

// BatchGetDependenciesMaxIDs is the number of IDs this client puts in a single
// GET /v1/batch/dependencies request. BatchGetDependencies shards larger
// requests into multiple round-trips below this cap.
//
// Deliberately defined AS BatchGetBeadsMaxIDs rather than as its own literal:
// the daemon ties the two together (batchGetDependenciesMaxIDs =
// batchGetMaxIDs), and the pre-raise code kept two independent 50s that a
// future edit could silently desynchronise. Both surfaces also share one
// runtime ratchet (batch_cap.go), so they cannot drift at runtime either.
const BatchGetDependenciesMaxIDs = BatchGetBeadsMaxIDs

// batchDepResult mirrors one per-ID slot in the daemon's
// /v1/batch/dependencies response. Every requested ID lands here, with an
// empty (non-nil) Deps slice when the daemon found no edges.
type batchDepResult struct {
	ID   string       `json:"id"`
	Deps []Dependency `json:"deps"`
}

// batchDepsResponse is the daemon's batch-fetch envelope for dependencies.
type batchDepsResponse struct {
	Results []batchDepResult `json:"results"`
}

// BatchGetDependencies fetches forward (direction="forward") or reverse
// (direction="reverse") dependencies for multiple beads in one (or a few)
// round-trips via GET /v1/batch/dependencies?ids=...&direction=....
//
// Direction semantics match the per-bead endpoints:
//   - "forward" → /v1/beads/{id}/dependencies — rows where bead_id = id
//     (this bead depends on X). Each returned row reads "id depends on
//     DependsOnID".
//   - "reverse" → /v1/beads/{id}/dependents — rows where depends_on_id = id
//     (X depends on this bead). Each returned row reads "BeadID depends on
//     id".
//
// The returned map is keyed by the requested ID. Every input ID is present
// in the map — IDs the daemon found no edges for land with a nil slice
// (callers should treat nil and empty as equivalent). Requests larger than
// BatchGetDependenciesMaxIDs are sharded; a per-shard failure aborts the
// whole call.
//
// Used by the beads-viewer constellation expandLevel batched fetch to
// collapse N per-parent GetDependencies round-trips into one (see
// ka-0000008009).
func (c *Client) BatchGetDependencies(ctx context.Context, ids []string, direction string) (map[string][]Dependency, error) {
	out := make(map[string][]Dependency, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	if direction == "" {
		direction = "forward"
	}
	switch direction {
	case "forward", "reverse":
	default:
		return nil, fmt.Errorf("batch deps: invalid direction %q", direction)
	}
	if err := c.withBatchCapRetry(func(perRequest int) error {
		// Fresh accumulator per attempt — see BatchGetBeads.
		out = make(map[string][]Dependency, len(ids))
		var mu sync.Mutex
		return c.forEachShard(ctx, ids, perRequest, func(ctx context.Context, shard []string) error {
			q := url.Values{}
			q.Set("ids", strings.Join(shard, ","))
			q.Set("direction", direction)
			var resp batchDepsResponse
			if err := c.doJSON(ctx, "GET", "/v1/batch/dependencies?"+q.Encode(), nil, &resp); err != nil {
				return fmt.Errorf("batch-getting dependencies: %w", err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, r := range resp.Results {
				if len(r.Deps) == 0 {
					out[r.ID] = nil
					continue
				}
				out[r.ID] = r.Deps
			}
			return nil
		})
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// BatchChildrenByStatusMaxParents is the maximum number of parent IDs the
// daemon accepts in a single GET /v1/batch/children-by-status request.
// BatchGetChildrenByStatus shards larger requests below this cap. Mirrors the
// server-side childrenStatusHistogramMaxParents constant (kbeads
// internal/server/http_batch.go).
const BatchChildrenByStatusMaxParents = 100

// childrenByStatusResponse is the daemon's envelope for
// GET /v1/batch/children-by-status: a per-parent histogram of parent-child
// child statuses (e.g. {"ka-AAA": {"open": 3, "closed": 7}}). Parents with no
// parent-child children echo back with an empty (non-nil) map.
type childrenByStatusResponse struct {
	Histograms map[string]map[string]int `json:"histograms"`
}

// BatchGetChildrenByStatus fetches, for each parent bead ID, a histogram of its
// direct parent-child children grouped by status, via
// GET /v1/batch/children-by-status?parents=ka-AAA,ka-BBB.
//
// The returned map is keyed by the requested parent ID; every input ID is
// present (the daemon echoes each requested parent, with an empty map for a
// parent that has no parent-child children — callers treat a nil/empty bucket
// as "no children"). Requests larger than BatchChildrenByStatusMaxParents are
// sharded; a per-shard failure aborts the whole call.
//
// Used by the orphan sweep to detect tree-roots (a parentless bead with at
// least one OPEN child is a legitimate root, not an orphan — ka-0000003548).
func (c *Client) BatchGetChildrenByStatus(ctx context.Context, parentIDs []string) (map[string]map[string]int, error) {
	out := make(map[string]map[string]int, len(parentIDs))
	if len(parentIDs) == 0 {
		return out, nil
	}
	var mu sync.Mutex
	if err := c.forEachShard(ctx, parentIDs, BatchChildrenByStatusMaxParents, func(ctx context.Context, shard []string) error {
		q := url.Values{}
		q.Set("parents", strings.Join(shard, ","))
		var resp childrenByStatusResponse
		if err := c.doJSON(ctx, "GET", "/v1/batch/children-by-status?"+q.Encode(), nil, &resp); err != nil {
			return fmt.Errorf("batch-getting children by status: %w", err)
		}
		mu.Lock()
		defer mu.Unlock()
		maps.Copy(out, resp.Histograms)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// BeadReferenceSummary mirrors the kbeads model returned by the bead-id
// reverse-lookup endpoints. SourceField encodes provenance — "title",
// "description", "notes", or "comment:<id>". For forward-direction lookups
// (mentions emitted by a bead) Title/Type/Status may be empty when the
// target ID is dangling (typo, cross-fleet, or not-yet-created). See
// kbeads/docs/design-bead-references.md § 5a.
type BeadReferenceSummary struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Type        string `json:"type"`
	Status      string `json:"status"`
	SourceField string `json:"source_field"`
	// RelationshipType carries the typed-mention verb the author declared
	// in prose (e.g. `parent(ka-X)`). nil means the row is an untyped
	// mention — the historic default. Typed values mirror
	// model.DependencyType strings: "parent-child", "blocks", "related",
	// "duplicates", "supersedes". See
	// kbeads/docs/research-typed-mention-syntax.md § 4.2.
	RelationshipType *string `json:"relationship_type"`
}

// GetMentionedBy returns up to limit beads whose text references beadID
// (reverse lookup). limit ≤ 0 falls back to the daemon default. The daemon
// caps limit at 500. See kbeads/docs/design-bead-references.md § 5a.
func (c *Client) GetMentionedBy(ctx context.Context, beadID string, limit int) ([]BeadReferenceSummary, error) {
	var resp struct {
		References []BeadReferenceSummary `json:"references"`
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/mentioned-by"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	if err := c.doJSON(ctx, "GET", path, nil, &resp); err != nil {
		return nil, fmt.Errorf("getting mentioned-by for %s: %w", beadID, err)
	}
	return resp.References, nil
}

// GetMentions returns the ka-id references emitted by beadID's text fields
// (forward lookup). limit ≤ 0 falls back to the daemon default. Dangling
// targets (typos, cross-fleet IDs) appear with empty Title/Type/Status.
// See kbeads/docs/design-bead-references.md § 5a.
func (c *Client) GetMentions(ctx context.Context, beadID string, limit int) ([]BeadReferenceSummary, error) {
	var resp struct {
		References []BeadReferenceSummary `json:"references"`
	}
	path := "/v1/beads/" + url.PathEscape(beadID) + "/mentions"
	if limit > 0 {
		path += "?limit=" + strconv.Itoa(limit)
	}
	if err := c.doJSON(ctx, "GET", path, nil, &resp); err != nil {
		return nil, fmt.Errorf("getting mentions for %s: %w", beadID, err)
	}
	return resp.References, nil
}

// Health checks the daemon health endpoint.
func (c *Client) Health(ctx context.Context) error {
	if err := c.doJSON(ctx, "GET", "/v1/health", nil, nil); err != nil {
		return fmt.Errorf("health check failed: %w", err)
	}
	return nil
}
