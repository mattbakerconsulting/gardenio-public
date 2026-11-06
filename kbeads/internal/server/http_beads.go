package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// ListBeads request limits. An unbounded ListBeads call against a populated
// database returns hundreds of MB and inflates server heap (the JSON encoder
// buffers the full response). Cap requests defensively: callers that omit
// `limit` get a sane default; callers that ask for more than the cap are
// clamped. See investigation: kbeads/docs/oom-poststartup-listbeads-investigation.md
// (ka-0000006889).
const (
	defaultListBeadsLimit = 1000
	maxListBeadsLimit     = 10000
)

// clampListBeadsLimit defensively bounds a caller-supplied ListBeads limit so
// every list path shares the same OOM choke point (ka-0000006889). A limit <= 0
// (omitted, or a negative value smuggled through a JSON body) becomes def;
// anything above maxListBeadsLimit is capped. Route every hand-built
// model.BeadFilter through this rather than re-inlining the clamp — a
// hand-built filter that skips it is exactly how the graph-family handlers
// reintroduced the bypass (the sibling class of the gRPC fix, ka-0000007023).
func clampListBeadsLimit(limit, def int) int {
	if limit <= 0 {
		return def
	}
	if limit > maxListBeadsLimit {
		return maxListBeadsLimit
	}
	return limit
}

// handleCreateBead handles POST /v1/beads.
func (s *BeadsServer) handleCreateBead(w http.ResponseWriter, r *http.Request) {
	// Read body bytes so we can pass them to handleEscalation if needed.
	bodyBytes, ok := readBoundedBody(w, r)
	if !ok {
		return
	}
	var in createBeadInput
	if err := json.Unmarshal(bodyBytes, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if s.landDoorMessageCapped(w, r, in.Type) {
		return
	}

	// Accept idempotency_key as a query param as well as a body field, so a
	// caller can keep its create body byte-for-byte stable across re-issues
	// and only vary the key out-of-band. The body field wins when both are
	// present. See ka-0000002856.
	if in.IdempotencyKey == "" {
		if k := r.URL.Query().Get("idempotency_key"); k != "" {
			in.IdempotencyKey = k
		}
	}

	// Accept the dedup-ack hash (epic ka-0000004599) as a query param as well
	// as the body field, under the type-specific name saw_open_decisions /
	// saw_open_escalations, so a caller retrying after a 409 can keep the body
	// stable and vary only the ack token. The body field (saw_open_hash) wins.
	if in.SawOpenHash == "" {
		q := r.URL.Query()
		if v := q.Get("saw_open_decisions"); v != "" {
			in.SawOpenHash = v
		} else if v := q.Get("saw_open_escalations"); v != "" {
			in.SawOpenHash = v
		}
	}

	// Collect the non-fatal warnings createBead raises (the short_title gate,
	// gd-V5nGseRS5f, and the heraldry gate, gd-fn7uPujOqj) and return each as
	// an X-Beads-Warning header.
	ctx := withWarnings(r.Context())
	bead, err := s.createBead(ctx, in)
	if err != nil {
		var denial *AuthzDenial
		if errors.As(err, &denial) {
			if s.handleEscalation(w, r, denial, bodyBytes, "") {
				return
			}
			s.auditDenial(r, denial, "")
			writeAuthzDenial(w, denial)
			return
		}
		var refusal *operatorRefusalError
		if errors.As(err, &refusal) {
			writeOperatorRefusal(w, refusal.Source)
			return
		}
		var dedup *dedupAckError
		if errors.As(err, &dedup) {
			writeDedupAckRejection(w, dedup)
			return
		}
		var dup *duplicateOpenError
		if errors.As(err, &dup) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":       dup.Error(),
				"code":        duplicateOpenErrorCode,
				"bead_type":   dup.BeadType,
				"existing_id": dup.ExistingID,
			})
			return
		}
		if hc, ok := asHeraldryConflict(err); ok {
			writeHeraldryConflict(w, hc)
			return
		}
		var ie inputError
		if errors.As(err, &ie) {
			writeError(w, http.StatusBadRequest, ie.Error())
		} else {
			writeServerError(r.Context(), w, "handleCreateBead", err, "internal server error")
		}
		return
	}

	// writeJSON writes the headers on its first write, so the warnings must be
	// added before it.
	for _, msg := range warningsFromContext(ctx) {
		w.Header().Add("X-Beads-Warning", msg)
	}
	writeJSON(w, http.StatusCreated, bead)
}

// writeDedupAckRejection writes the structured HTTP 409 body for a dedup-ack
// gate rejection (epic ka-0000004599). The body carries the readable open
// list, the opaque acknowledgement hash, and a copy-pasteable retry hint so
// the kd CLI (T2) can render the list and compose the retry. The `code`
// field is stable (dedup_ack_required) for machine keying.
func writeDedupAckRejection(w http.ResponseWriter, e *dedupAckError) {
	writeJSON(w, http.StatusConflict, map[string]any{
		"error":      e.Error(),
		"code":       e.Code(),
		"bead_type":  e.BeadType,
		"project":    e.Project,
		"hash":       e.Hash,
		"open":       e.Open,
		"truncated":  e.Truncated,
		"total_open": e.TotalOpen,
		"retry_with": e.RetryWith,
	})
}

// handleListBeads handles GET /v1/beads.
func (s *BeadsServer) handleListBeads(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	fleet := q.Get("fleet")
	if fleet == "" {
		// All tokens list across all fleets. `bead.fleet` is audit-attribution
		// only — the old per-token read-isolation gate was removed under epic
		// ka-0000000871 / ka-0000003285.
		fleet = model.FleetAll
	}
	filter := model.BeadFilter{
		Assignee:  q.Get("assignee"),
		Owner:     q.Get("owner"),
		CreatedBy: q.Get("created_by"),
		Search:    q.Get("search"),
		Sort:      q.Get("sort"),
		Fleet:     fleet,
		Mentions:  q.Get("mentions"),
	}
	// owner_in=<csv>: multi-valued owner filter, OR-merged with the scalar
	// ?owner daemon-side (mergeNonEmpty, queries.go) into a single
	// `owner = ANY($::text[])` predicate. Mirrors assignee_in above; the gRPC
	// path already wires the equivalent OwnerIn repeated field (list_filter.go).
	if v := q.Get("owner_in"); v != "" {
		filter.Owners = append(filter.Owners, strings.Split(v, ",")...)
	}
	// mentions_type narrows the mentions filter by bead_references.relationship_type.
	// Only meaningful when ?mentions= is also set. Three-state semantics:
	//   - missing key            → no filter
	//   - present, empty value   → untyped only (relationship_type IS NULL)
	//   - present, "<verb>"      → exact match
	if q.Has("mentions_type") {
		v := q.Get("mentions_type")
		filter.MentionsType = &v
	}
	// Legacy operated_by_in / operated_by_is_null TEXT filters were removed
	// in P6 (migration 024). Callers use operated_by_alias_in instead.
	// operated_by_alias_in: comma-separated `kind:subject` aliases, resolved
	// through operator_aliases to operated_by_id (P4 §5.3). Recovers legacy
	// Slack-driven beads under the canonical alias.
	if v := q.Get("operated_by_alias_in"); v != "" {
		filter.OperatedByAliases = strings.Split(v, ",")
	}
	// assignee_in=<csv>: multi-valued assignee filter (Phase 3b), OR-merged
	// with the scalar ?assignee daemon-side into a single
	// `assignee = ANY($::text[])` predicate. The beads-viewer bidirectional
	// inbox (design §4.2, ka-0000007555) issues
	// `type=mail&assignee_in=<recipient-handle-set>` to list mail addressed
	// to any of one operator's handles in one query. The gRPC path already
	// maps the equivalent AssigneeIn repeated field (list_filter.go); this
	// wires the HTTP transport to the same model field.
	if v := q.Get("assignee_in"); v != "" {
		filter.Assignees = append(filter.Assignees, strings.Split(v, ",")...)
	}
	// unassigned=true: match only beads with no assignee (`assignee IS NULL OR
	// assignee = <empty>`). Surfaced as `kd list/count --unassigned` and `gb ready
	// --unassigned`. It cannot be expressed as `?assignee=` — the empty scalar
	// is dropped as "no filter" — nor via the `?field=key=` absent syntax, since
	// assignee is a first-class column and not a JSONB custom field. Without it
	// the highest-frequency work-seeking query ("open, unblocked, unclaimed in
	// my lane") had to pull the whole open set and post-filter in jq, where one
	// wrong field name yields a silent false-zero (ka-0000007209).
	if q.Get("unassigned") == "true" {
		filter.Unassigned = true
	}
	// category=intake|intermediate|action|config (comma-separated for multi).
	// Powers the inbox v1 §1.2.1 query shape and chip-strip variants.
	if v := q.Get("category"); v != "" {
		filter.Categories = strings.Split(v, ",")
	}
	// operated_by_id=<int>: filter on the operators(id) FK column. Used by
	// /v1/beads consumers (e.g. beads-viewer /inbox handler) to issue the
	// single-FK-equality query that hits idx_beads_inbox_predicate.
	if v := q.Get("operated_by_id"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			filter.OperatedByID = &n
		}
	}

	if v := q.Get("status"); v != "" {
		for _, s := range strings.Split(v, ",") {
			filter.Status = append(filter.Status, model.Status(s))
		}
	}
	if v := q.Get("type"); v != "" {
		for _, t := range strings.Split(v, ",") {
			bt := model.BeadType(t)
			// Resolve deprecated aliases (template→formula) so the HTTP path
			// matches the gRPC buildListFilter behaviour — otherwise ?type=template
			// would silently match nothing and, once validated below, would even
			// 400 a value the create path accepts.
			if canonical, ok := model.TypeAliases[bt]; ok {
				bt = canonical
			}
			filter.Type = append(filter.Type, bt)
		}
	}
	// exclude_type=<csv>: server-side `type NOT IN (...)`. Beads-viewer
	// /active uses this for the queued-count tile to drop agent rows
	// without pulling them — fetching limit=500 just to client-side
	// post-filter was the dominant /v1/beads cost on gardenio-e2e (status=open
	// matches ~240k rows; the COUNT(*) OVER() forces a sort over all of
	// them and the response carries 740 KB of bead bodies the caller
	// throws away). With this filter + limit=1 the caller reads total
	// from the response instead.
	if v := q.Get("exclude_type"); v != "" {
		for _, t := range strings.Split(v, ",") {
			filter.ExcludeTypes = append(filter.ExcludeTypes, model.BeadType(t))
		}
	}
	// count_only=true: short-circuit ListBeads to a SELECT COUNT(*) without
	// the COUNT(*) OVER() + ORDER BY pattern. Even with limit=1 the default
	// path forces postgres to materialize and sort the entire matching set
	// (~2 s on status=open, gardenio-e2e). count-only callers (queued-count
	// tile, etc.) read `total` from the response and ignore `beads`.
	if q.Get("count_only") == "true" {
		filter.CountOnly = true
	}
	// no_total=true: skip the narrow COUNT(*) a full page pays for its total;
	// total is then -1 unless the page is short (model.BeadFilter.NoTotal).
	// For callers that page or poll but never show a total (gd-g25jihMHFs).
	if q.Get("no_total") == "true" {
		filter.NoTotal = true
	}
	if v := q.Get("kind"); v != "" {
		for _, k := range strings.Split(v, ",") {
			filter.Kind = append(filter.Kind, model.Kind(k))
		}
	}
	if v := q.Get("labels"); v != "" {
		filter.Labels = strings.Split(v, ",")
	}
	// labels_any=<csv>: user-facing any-of (OR) label filter — a bead matches
	// when it carries AT LEAST ONE of these labels. Routed into LabelsAnyUser
	// (its own AND-ed EXISTS clause), NOT the OIDC-scoping LabelsAny populated
	// below, so a user any-of narrows within project scope and can never widen
	// it (ka-0000003307). Surfaced as `kd list --label-any`.
	if v := q.Get("labels_any"); v != "" {
		filter.LabelsAnyUser = append(filter.LabelsAnyUser, strings.Split(v, ",")...)
	}
	// exclude_labels=<csv>: server-side `label NOT IN (...)`. The
	// gardener-delivery trigger's cron preflight uses this to drop MR beads
	// carrying an opt-out label so the gardener stays dormant when the only
	// open MRs are operator-flagged not-its-lane.
	if v := q.Get("exclude_labels"); v != "" {
		filter.ExcludeLabels = strings.Split(v, ",")
	}
	// exclusive_project=<name>: drop beads carrying a `project:<other>` label, so
	// LIMIT accounts for project narrowing (ka-0000003411). Replaces the
	// client-side filterExclusiveProject over-fetch in `gb ready` / `gb news`.
	if v := q.Get("exclusive_project"); v != "" {
		filter.ExclusiveProject = v
	}
	if v := q.Get("priority"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Priority = &n
		}
	}
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Limit = n
		}
	}
	filter.Limit = clampListBeadsLimit(filter.Limit, defaultListBeadsLimit)
	if v := q.Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Offset = n
		}
	}
	if q.Get("no_open_deps") == "true" {
		filter.NoOpenDeps = true
	}
	if q.Get("no_parent") == "true" {
		filter.NoParent = true
	}
	// title=<exact title>, repeatable: beads whose title is exactly one of the
	// values (model.BeadFilter.Titles; no full-text, no wildcards). It only
	// narrows. The lands-pause kill switch looks its gate up this way, so other
	// open gates that merely mention the title cannot crowd it out of a page
	// (gd-i74esUHCTg).
	if vs := q["title"]; len(vs) > 0 {
		filter.Titles = append(filter.Titles, vs...)
	}
	if q.Get("short_title_missing") == "true" {
		filter.ShortTitleMissing = true
	}
	// heraldry=<arms>: match on a bead's arms (setHeraldryFilter).
	if err := setHeraldryFilter(&filter, q.Get("heraldry")); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// heraldry_project=<project>: the beads bearing arms in that project's
	// register (beads.project, migration 057), whatever their arms; `kd
	// heraldry check` reads it to name collisions and look-alikes.
	if v := q.Get("heraldry_project"); v != "" {
		filter.HeraldryRegister = v
	}
	// `parent=<bead-id>` restricts results to the transitive subtree under
	// the given bead (descendants at any depth via parent-child deps).
	// Used by scheduler bead_query conditions — see plan §T2 in
	// gardenio/plans/epic-mention-looping-gardener.md.
	if v := q.Get("parent"); v != "" {
		filter.ParentSubtreeID = v
	}
	if q.Get("exclude_scheduled") == "true" {
		filter.ExcludeScheduled = true
	}
	// has_report=true|false (R1, ka-0000007923): filter by the per-row
	// has_report projection. Missing key = no filter; "true" / "false"
	// each map to a non-nil pointer.
	if q.Has("has_report") {
		v := q.Get("has_report") == "true"
		filter.HasReport = &v
	}
	// blocked_on_decision filter (ka-0000001065 / R2). Daemon computes the
	// predicate per-row; this param restricts results to one side of the
	// boolean. Anything other than "true"/"false" is ignored.
	if v := q.Get("blocked_on_decision"); v != "" {
		switch v {
		case "true":
			tr := true
			filter.BlockedOnDecision = &tr
		case "false":
			fa := false
			filter.BlockedOnDecision = &fa
		}
	}
	// parked_on_decision filter (ka-0000003797): text-mention sibling of the
	// above. Same three-state contract; anything but "true"/"false" is ignored.
	if v := q.Get("parked_on_decision"); v != "" {
		switch v {
		case "true":
			tr := true
			filter.ParkedOnDecision = &tr
		case "false":
			fa := false
			filter.ParkedOnDecision = &fa
		}
	}
	// updated_after=<RFC3339> filters to beads whose updated_at is strictly
	// greater than the given timestamp. Used by the slack-bridge persistent
	// event buffer (GAP-2, ka-0000004769) to reconcile bead mutations missed
	// during SSE outages: the bridge persists a high-water mark, polls this
	// endpoint on an interval, and replays anything newer through the local
	// trigger:bead pipeline. Unparseable values are ignored so a stale or
	// corrupt watermark never silently widens the result set.
	if v := q.Get("updated_after"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			filter.UpdatedAfter = &t
		}
	}
	// updated_before=<RFC3339> filters to beads whose updated_at is strictly
	// less than the given timestamp. The beads-viewer index health badge
	// (ka-0000002729) uses it with count_only + status=in_progress to get a
	// cheap per-project "stale in_progress" count (updated_before=now-24h),
	// the same staleness heuristic projectSummary applies. Unparseable values
	// are ignored so a malformed cutoff never silently widens the result set.
	if v := q.Get("updated_before"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			filter.UpdatedBefore = &t
		}
	}
	// created_after / created_before window on beads.created_at (inclusive at
	// the store layer: >= / <=). The filter fields predate this parsing — the
	// params were silently IGNORED until ka-0000008195, forcing offset-walks +
	// client-side date filtering for "created since X" queries. Unparseable
	// values are ignored, same as the updated_* params above.
	if v := q.Get("created_after"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			filter.CreatedAfter = &t
		}
	}
	if v := q.Get("created_before"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			filter.CreatedBefore = &t
		}
	}
	// closed_after / closed_before window on beads.closed_at (strict > / <,
	// mirroring updated_*). closed_at is nullable — never-closed beads never
	// match either bound. Powers "closed in the last 24h" queries
	// (kd list -s closed --closed-after 24h) without offset-walking.
	if v := q.Get("closed_after"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			filter.ClosedAfter = &t
		}
	}
	if v := q.Get("closed_before"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			filter.ClosedBefore = &t
		}
	}
	// with_has_report / with_blocked_on_decision: opt the per-row
	// projection columns into the SELECT without filtering. Both projections
	// are perf-load-bearing — see model.BeadFilter docs and the deps /
	// open-decisions index migrations. HTTP callers that need the badge data
	// (beads-viewer sequencer board, sequencer-report links) must pass the
	// flag; default callers (slack / gitlab bridges, scheduler roster, gb
	// ready, kd list without badge flags) get the cheap query.
	if q.Get("with_has_report") == "true" {
		filter.WithHasReport = true
	}
	if q.Get("with_blocked_on_decision") == "true" {
		filter.WithBlockedOnDecision = true
	}
	// with_parked_on_decision (ka-0000003797): opt the text-mention parked
	// projection into the SELECT without filtering. Same default-off discipline.
	if q.Get("with_parked_on_decision") == "true" {
		filter.WithParkedOnDecision = true
	}
	// with_open_blockers: denormalize each row's OPEN blocking edges
	// (id/status/type/title) onto the response, so a triage caller learns WHY
	// each bead is not ready in one call instead of one `kd show` per bead
	// (ka-0000000945). Costs one extra batched deps⋈beads query per page, so
	// like the projections above it is strictly opt-in.
	if q.Get("with_open_blockers") == "true" {
		filter.WithOpenBlockers = true
	}
	// with_parent: denormalize each row's immediate parent-child parent id (and
	// its title when in scope) onto the response, so a dispatcher reasoning over
	// `gb ready --json` can tell a leaf task under an epic from a floating one —
	// and group by epic — in one call instead of an N+1 `kd show` per row
	// (ka-0000006083). Costs one extra batched deps⋈beads query per page, so like
	// the projections above it is strictly opt-in.
	if q.Get("with_parent") == "true" {
		filter.WithParent = true
	}
	// omit=<csv of description,notes>: leave those free-text bodies out of the
	// rows (gd-zXdZq4Traf). For poll loops that re-list a roster every cycle
	// and never read the bodies — the trigger-engine's and the controller's
	// listings were ~77 GB/day of mostly description/notes. Strictly opt-in;
	// an unknown name is a 400 so a typo never reads as "omitted nothing".
	if v := q.Get("omit"); v != "" {
		for _, name := range strings.Split(v, ",") {
			switch strings.TrimSpace(name) {
			case "description":
				filter.OmitDescription = true
			case "notes":
				filter.OmitNotes = true
			case "":
			default:
				writeError(w, http.StatusBadRequest, fmt.Sprintf("omit: unknown column %q (allowed: description, notes)", name))
				return
			}
		}
	}
	// omit_fields=<csv of dotted paths>: strip those keys from each row's
	// fields in SQL (gd-zXdZq4Traf) — "prompt" is ~70% of an agent bead,
	// "gardener.run_log" half a gardener trigger. Same opt-in discipline as
	// omit; a malformed path is a 400 (the path is inlined into the SELECT).
	if v := q.Get("omit_fields"); v != "" {
		for _, path := range strings.Split(v, ",") {
			path = strings.TrimSpace(path)
			if path == "" {
				continue
			}
			if !model.ValidOmitFieldPath(path) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("omit_fields: invalid path %q (dotted segments of [A-Za-z0-9_-])", path))
				return
			}
			filter.OmitFields = append(filter.OmitFields, path)
		}
	}
	// fields_only=<csv of top-level keys>: keep only those keys of each row's
	// fields (gd-6PXLuR3juD), for walks that sum one or two fields over every
	// agent or session bead. Wins over omit_fields. Same opt-in discipline; a
	// malformed key is a 400 because it is inlined into the SELECT.
	if v := q.Get("fields_only"); v != "" {
		for _, key := range strings.Split(v, ",") {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			if !model.ValidFieldKey(key) {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("fields_only: invalid key %q (one segment of [A-Za-z0-9_-])", key))
				return
			}
			filter.FieldsOnly = append(filter.FieldsOnly, key)
		}
	}
	// Soft-delete visibility (admin-only). Non-admin requests silently
	// ignore these — the default filter already excludes soft-deleted beads.
	if hasMinimumRole(RolesFromContext(r.Context()), RoleAdmin) {
		if q.Get("include_deleted") == "true" {
			filter.IncludeDeleted = true
		}
		if q.Get("only_deleted") == "true" {
			filter.OnlyDeleted = true
		}
	}
	if ff := q["field"]; len(ff) > 0 {
		filter.Fields = make(map[string]string, len(ff))
		for _, f := range ff {
			if k, v, ok := strings.Cut(f, "="); ok {
				filter.Fields[k] = v
			}
		}
	}
	fieldsIn, err := model.ParseFieldsIn(q["field_in"])
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	filter.FieldsIn = fieldsIn

	// Enforce project scope for project-bound fleet tokens, OIDC read-side
	// project isolation, and the taint read gate (ADR docs/adr/taints.md
	// §5.1/§10.1: taintsOf(bead) ⊆ grantsOf(caller)) — the third, narrower
	// check a caller who passes both project gates must still satisfy.
	filter = s.scopeListFilter(r.Context(), filter)

	// Reject unknown ?type= filter values server-side (ka-0000008041) — the
	// same choke point the gRPC ListBeads RPC uses, so /v1/beads and its
	// consumers (beads-viewer, bridges, automation) fail loud on an unknown
	// type instead of returning a silent empty page indistinguishable from a
	// genuinely empty result (ka-0000006244).
	if err := s.validateFilterTypes(r.Context(), filter.Type); err != nil {
		var ie inputError
		if errors.As(err, &ie) {
			writeError(w, http.StatusBadRequest, ie.Error())
		} else {
			writeServerError(r.Context(), w, "handleListBeads", err, "failed to validate type filter")
		}
		return
	}

	beads, total, err := s.store.ListBeads(r.Context(), filter)
	if err != nil {
		writeServerError(r.Context(), w, "handleListBeads", err, "failed to list beads")
		return
	}
	// inherit:read for every bead on the page (`kd list`, `kd search`).
	if err := s.propagateReadTaintBeads(r.Context(), beadIDsOf(beads)); err != nil {
		writeServerError(r.Context(), w, "handleListBeads", err, "failed to propagate read taint")
		return
	}

	// Ensure beads is never null in JSON output.
	if beads == nil {
		beads = []*model.Bead{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"beads": beads,
		"total": total,
	})
}

// handleFindAgentByName handles GET /v1/agents-by-name/{name}.
//
// Resolves a type=agent bead by its fields.agent value, restricted to the
// optional ?status=open,in_progress,blocked,deferred filter (default: any
// status). The query hits the partial expression index idx_beads_fields_agent
// (migration 025) so it is O(log N) on a fully populated table. Used by the
// gardenio controller to avoid pulling the full active-agent list and
// filtering client-side (ka-0000005291).
func (s *BeadsServer) handleFindAgentByName(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	var statuses []model.Status
	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			statuses = append(statuses, model.Status(s))
		}
	}

	bead, err := s.store.FindAgentByName(r.Context(), name, statuses)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleFindAgentByName", err, "failed to find agent")
		return
	}
	if bead == nil {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}

	// Hydrate labels so the token-project / OIDC-project guards can match.
	labels, lerr := s.store.GetLabels(r.Context(), bead.ID)
	if lerr == nil {
		bead.Labels = labels
	}

	if !beadMatchesTokenProject(r.Context(), bead.Labels) {
		writeError(w, http.StatusForbidden, "token not authorized for this bead's project")
		return
	}
	if allowed := oidcAllowedProjects(r.Context(), s.store); allowed != nil {
		if !beadMatchesOIDCProjects(bead.Labels, allowed) {
			writeError(w, http.StatusForbidden, "not authorized to read beads in this project")
			return
		}
	}

	writeJSON(w, http.StatusOK, bead)
}

// handleFindAgentsByNames handles GET /v1/agents-by-name:batch?names=a,b,c.
//
// Resolves a set of type=agent beads by their fields.agent values in a
// single round trip — the batched form of handleFindAgentByName. Returns
// one most-recently-updated bead per matching name; names with no match
// are silently omitted from the response. Optional ?status=... filter
// behaves the same as the per-name endpoint.
//
// Used by the beads-viewer list pages to prewarm the per-request
// agentResolver and collapse N per-row daemon round-trips into one
// (ka-0000000899).
func (s *BeadsServer) handleFindAgentsByNames(w http.ResponseWriter, r *http.Request) {
	namesParam := strings.TrimSpace(r.URL.Query().Get("names"))
	if namesParam == "" {
		writeError(w, http.StatusBadRequest, "names is required")
		return
	}

	raw := strings.Split(namesParam, ",")
	names := make([]string, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for _, n := range raw {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		names = append(names, n)
	}
	if len(names) == 0 {
		writeError(w, http.StatusBadRequest, "names is required")
		return
	}
	// Cap batch size so a single request can't pull arbitrarily large
	// result sets. Mirrors the /v1/batch/beads ceiling.
	const maxNames = 200
	if len(names) > maxNames {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("maximum %d names per batch request", maxNames))
		return
	}

	var statuses []model.Status
	if raw := strings.TrimSpace(r.URL.Query().Get("status")); raw != "" {
		for _, s := range strings.Split(raw, ",") {
			s = strings.TrimSpace(s)
			if s == "" {
				continue
			}
			statuses = append(statuses, model.Status(s))
		}
	}

	beads, err := s.store.FindAgentsByNames(r.Context(), names, statuses)
	if err != nil {
		writeServerError(r.Context(), w, "handleFindAgentsByNames", err, "failed to find agents")
		return
	}

	allowedOIDC := oidcAllowedProjects(r.Context(), s.store)

	// Hydrate labels so the token-project / OIDC-project guards can filter
	// the same way as the per-name endpoint — in one batched SELECT, not one
	// GetLabels per agent (the labels N+1, gd-cP9tODid8j). A failed batch
	// leaves every bead unlabelled, as a failed per-bead read used to.
	ids := make([]string, 0, len(beads))
	for _, bead := range beads {
		if bead != nil {
			ids = append(ids, bead.ID)
		}
	}
	labelsByID, lerr := s.store.GetLabelsForBeads(r.Context(), ids)

	out := make([]*model.Bead, 0, len(beads))
	for _, bead := range beads {
		if bead == nil {
			continue
		}
		if lerr == nil {
			bead.Labels = labelsByID[bead.ID]
		}
		if !beadMatchesTokenProject(r.Context(), bead.Labels) {
			continue
		}
		if allowedOIDC != nil && !beadMatchesOIDCProjects(bead.Labels, allowedOIDC) {
			continue
		}
		out = append(out, bead)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"agents": out,
	})
}

// handleGetBead handles GET /v1/beads/{id}.
func (s *BeadsServer) handleGetBead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	bead, err := s.store.GetBead(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleGetBead", err, "failed to get bead")
		return
	}
	if bead == nil {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}

	// A bead outside the caller's projects answers as missing.
	if s.beadHiddenFromCaller(r.Context(), bead) {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}
	// A tenant reads no account bead, its own land's included (read_authz_tenant.go).
	if s.readHidesBead(r.Context(), bead) {
		writeError(w, http.StatusForbidden, errReadTypeDenied.Error())
		return
	}

	// Third read gate (ADR docs/adr/taints.md §5.1/§5.3): taintsOf(bead) ⊆
	// grantsOf(caller). Like the project-scope gates above, a taint denial
	// returns 404 so a caller lacking a grant cannot even learn the bead
	// exists (§5.3, §11 "denials do not leak existence"). The
	// gate state is resolved FIRST and the bead's own taint ids are fetched
	// only when it is not exempt, so a zero-config registry (or an exempt
	// caller) never issues a bead_taints query here (ADR §10.1).
	taintGate, err := s.resolveBeadTaintGateState(r.Context())
	if err != nil {
		writeServerError(r.Context(), w, "handleGetBead", err, "failed to resolve taint grants")
		return
	}
	if !taintGate.exempt {
		taintIDs, err := s.store.GetTaintsForBead(r.Context(), bead.ID)
		if err != nil {
			writeServerError(r.Context(), w, "handleGetBead", err, "failed to get bead taints")
			return
		}
		if !taintGate.allows(taintIDs) {
			recordTaintReadDenial(r.Context(), "read", string(bead.Type))
			writeError(w, http.StatusNotFound, "bead not found")
			return
		}
	}

	// inherit:read (ADR docs/adr/taints.md §4.2 row 3): now that the read is
	// authorized, a reading agent acquires beadID's taints onto its own
	// agent bead. taintGate.exempt already told us the registry is empty
	// (the only way it can be true for an agent caller — see
	// propagateReadTaint), so this is skipped on the same zero-config hot
	// path the gate check above short-circuits on.
	if !taintGate.exempt {
		if err := s.propagateReadTaint(r.Context(), bead.ID); err != nil {
			writeServerError(r.Context(), w, "handleGetBead", err, "failed to propagate read taint")
			return
		}
	}

	// Compute HasBundle (T1: agent-artifact bundles): true when this bead has
	// at least one type=bundle child via parent-child reference. Errors are
	// non-fatal — the lookup is informational and the index is small, but a
	// db hiccup must not break the GET. See plan
	// gardenio/plans/agent-artifact-bundles-multi-doc-viewer.md §3.5.
	hasBundle, err := s.store.HasBundleChild(r.Context(), bead.ID)
	if err != nil {
		hasBundle = false
	}

	detail := beadDetail{Bead: bead, HasBundle: hasBundle}
	if isAutonomousBeadType(bead.Type) {
		detail.SpawnLineageContent = spawnLineageContent(bead.Fields)
		detail.SpawnLineageDigest = spawnLineageDigest(bead.Fields)
	}
	writeJSON(w, http.StatusOK, detail)
}

// beadDetail is the GET /v1/beads/{id} response shape: a bead embedded
// alongside derived flags computed at request time. HasBundle is set when
// the bead has at least one parent-child reference from a type=bundle bead.
type beadDetail struct {
	*model.Bead
	HasBundle bool `json:"has_bundle"`
	// SpawnLineageContent and SpawnLineageDigest are set on a trigger/schedule:
	// the spawn-relevant content a seal would cover and its sha256, so `gb
	// trigger reseal` can show the admin what they adopt and pin the digest on
	// the write (spawn_lineage_seal.go).
	SpawnLineageContent map[string]any `json:"spawn_lineage_content,omitempty"`
	SpawnLineageDigest  string         `json:"spawn_lineage_digest,omitempty"`
}

// handleDeleteBead handles DELETE /v1/beads/{id}.
func (s *BeadsServer) handleDeleteBead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	// Fetch the bead so we can run project-scope and write-policy authz against
	// its actual type and labels. §C2 (ka-0000006767): DELETE previously only
	// consulted TokenProjectFromContext and skipped write_policy + fleet-token
	// type/op scope enforcement entirely. authorizeWriteForBead also runs the
	// ownership gate (ka-0000001016).
	existing, err := s.store.GetBead(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "bead not found")
		} else {
			writeServerError(r.Context(), w, "handleDeleteBead", err, "failed to fetch bead")
		}
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}
	// Each refusal below answers a bead the caller cannot read as missing.
	if !beadMatchesTokenProject(r.Context(), existing.Labels) {
		if !s.refuseHidden(w, r, existing) {
			writeError(w, http.StatusForbidden, "token not authorized for this bead's project")
		}
		return
	}
	if denial := s.authorizeWriteForBead(r.Context(), "delete", existing); denial != nil {
		if s.beadHiddenFromCaller(r.Context(), existing) {
			s.auditDenial(r, denial, id)
			writeError(w, http.StatusNotFound, "bead not found")
			return
		}
		// DELETE has no request body; synthesize a minimal operation_spec
		// so the escalation bead satisfies schema validation.
		spec := []byte(`{"id":"` + id + `"}`)
		if s.handleEscalation(w, r, denial, spec, id) {
			return
		}
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return
	}
	if denial := commonsMessageLockDenial(r.Context(), "delete", "delete it", existing); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return
	}
	if denial := s.mailControlCloseDenial(r.Context(), "delete", existing, nil); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return
	}

	if err := s.store.DeleteBead(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "bead not found")
			return
		}
		writeServerError(r.Context(), w, "handleDeleteBead", err, "failed to delete bead")
		return
	}

	s.recordAndPublish(r.Context(), events.TopicBeadDeleted, id, "", events.BeadDeleted{BeadID: id, Fleet: FleetFromContext(r.Context())})
	s.recordDelete(r.Context(), id, actorFromContext(r.Context()))

	w.WriteHeader(http.StatusNoContent)
}

// handleRestoreBead handles POST /v1/beads/{id}/restore. Clears deleted_at on
// a soft-deleted bead, returning the restored row. Admin-only mirror of
// handleDeleteBead.
func (s *BeadsServer) handleRestoreBead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	bead, err := s.store.RestoreBead(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "bead not found or not soft-deleted")
			return
		}
		writeServerError(r.Context(), w, "handleRestoreBead", err, "failed to restore bead")
		return
	}
	if !beadMatchesTokenProject(r.Context(), bead.Labels) {
		writeError(w, http.StatusForbidden, "token not authorized for this bead's project")
		return
	}

	s.recordAndPublish(r.Context(), events.TopicBeadUpdated, id, "", events.BeadUpdated{Bead: bead, Changes: map[string]any{"deleted_at": nil}})
	s.recordRestore(r.Context(), id, actorFromContext(r.Context()))

	writeJSON(w, http.StatusOK, bead)
}

// handleUpdateBead handles PATCH /v1/beads/{id}.
func (s *BeadsServer) handleUpdateBead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	bodyBytes, ok := readBoundedBody(w, r)
	if !ok {
		return
	}
	var in updateBeadInput
	if err := json.Unmarshal(bodyBytes, &in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// For HTTP/JSON, DueAt/DeferUntil/Labels presence is inferred from non-nil/non-empty.
	if in.DueAt != nil {
		in.dueAtSet = true
	}
	if in.DeferUntil != nil {
		in.deferUntilSet = true
	}
	if in.Labels != nil {
		in.labelsSet = true
	}

	// Collect the non-fatal warnings updateBead raises (the arms near-duplicate,
	// heraldry.go) and return each as an X-Beads-Warning header.
	ctx := withWarnings(r.Context())
	if parseForceCloseHeader(r.Header.Get(forceCloseHeader)) {
		ctx = withForceClose(ctx)
	}
	bead, err := s.updateBead(ctx, id, in)
	if err != nil {
		var denial *AuthzDenial
		if errors.As(err, &denial) {
			if denial.hidden {
				s.auditDenial(r, denial, id)
				writeError(w, http.StatusNotFound, "bead not found")
				return
			}
			if s.handleEscalation(w, r, denial, bodyBytes, id) {
				return
			}
			s.auditDenial(r, denial, id)
			writeAuthzDenial(w, denial)
			return
		}
		var refusal *operatorRefusalError
		if errors.As(err, &refusal) {
			writeOperatorRefusal(w, refusal.Source)
			return
		}
		var divert *escalationDivertError
		if errors.As(err, &divert) {
			writeError(w, http.StatusConflict, divert.Error())
			return
		}
		var openPR *openPRCloseError
		if errors.As(err, &openPR) {
			writeError(w, http.StatusConflict, openPR.Error())
			return
		}
		var held *lockHeldError
		var relConflict releaseConflict
		if errors.As(err, &held) || errors.As(err, &relConflict) {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		var stale *spawnLineageDigestMismatchError
		if errors.As(err, &stale) {
			writeError(w, http.StatusConflict, stale.Error())
			return
		}
		if hc, ok := asHeraldryConflict(err); ok {
			writeHeraldryConflict(w, hc)
			return
		}
		var ie inputError
		if errors.As(err, &ie) {
			writeError(w, http.StatusBadRequest, ie.Error())
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "bead not found")
			return
		}
		writeServerError(r.Context(), w, "handleUpdateBead", err, "internal server error")
		return
	}

	for _, msg := range warningsFromContext(ctx) {
		w.Header().Add("X-Beads-Warning", msg)
	}
	writeJSON(w, http.StatusOK, bead)
}

// closeRefusedBeforeBody runs handleCloseBead's gates that need no request
// body and reports whether one refused (the response is then written). Each
// refusal answers a bead the caller cannot read as missing (refuseHidden).
func (s *BeadsServer) closeRefusedBeforeBody(w http.ResponseWriter, r *http.Request, id string, existing *model.Bead) bool {
	if !beadMatchesTokenProject(r.Context(), existing.Labels) {
		if !s.refuseHidden(w, r, existing) {
			writeError(w, http.StatusForbidden, "token not authorized for this bead's project")
		}
		return true
	}
	// A scoped principal is denied on type and project before the escalation
	// redirect below, so the redirect cannot confirm that an out-of-scope bead
	// is an open escalation.
	if denial := scopedWriteDenial(r.Context(), "close", existing.Type, existing.Labels); denial != nil {
		s.auditDenial(r, denial, id)
		if !s.refuseHidden(w, r, existing) {
			writeAuthzDenial(w, denial)
		}
		return true
	}
	// Escalation resolve-surface guard (ka-0000005516): divert a generic close
	// of an OPEN escalation to its dedicated approve/reject/result verbs so
	// operation_spec replay and close-on-escalation wrapper release stay
	// correct. Runs before the ownership gate so the caller gets an actionable
	// redirect instead of a confusing 403. See escalationBlocksGenericClose.
	if escalationBlocksGenericClose(existing) {
		if !s.refuseHidden(w, r, existing) {
			writeError(w, http.StatusConflict, fmt.Sprintf(escalationResolveHint, id))
		}
		return true
	}
	return false
}

// handleCloseBead handles POST /v1/beads/{id}/close.
// Accepts optional JSON body with "closed_by" and any extra fields to merge
// into the bead's fields before closing (e.g., "chosen", "rationale" for decisions).
func (s *BeadsServer) handleCloseBead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	// Check write policy before closing.
	existing, err := s.store.GetBead(r.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "bead not found")
		} else {
			writeServerError(r.Context(), w, "handleCloseBead", err, "failed to fetch bead")
		}
		return
	}
	if existing == nil {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}
	if s.closeRefusedBeforeBody(w, r, id, existing) {
		return
	}
	// Read the bounded body once — it may be needed for escalation replay and
	// for merging extra fields into the bead.
	r.Body = http.MaxBytesReader(w, r.Body, beadsMaxBodyBytes)
	bodyBytes, readErr := io.ReadAll(r.Body)
	if readErr != nil {
		var maxErr *http.MaxBytesError
		if errors.As(readErr, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	if denial := s.authorizeWriteForBead(r.Context(), "close", existing); denial != nil {
		if s.beadHiddenFromCaller(r.Context(), existing) {
			s.auditDenial(r, denial, id)
			writeError(w, http.StatusNotFound, "bead not found")
			return
		}
		if s.handleEscalation(w, r, denial, bodyBytes, id) {
			return
		}
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return
	}
	// Only its writer closes a person's Commons message (gd-74wnmpQgyT).
	if denial := commonsMessageLockDenial(r.Context(), "close", "close it", existing); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return
	}

	// Idempotent close: if the bead is already closed, return the existing
	// bead without merging new fields or re-stamping closed_at/closed_by.
	// This prevents racing callers (e.g. a dying agent pod's cleanup path)
	// from overwriting fields the agent set at clean close — see ka-0000000961.
	if existing.Status == model.StatusClosed {
		writeJSON(w, http.StatusOK, existing)
		return
	}
	// Open-PR close guard (gd-lIKFejVISw): a task or bug whose PR is still
	// open stays open; `kd close --force` from an operator overrides.
	ctx := r.Context()
	if parseForceCloseHeader(r.Header.Get(forceCloseHeader)) {
		ctx = withForceClose(ctx)
	}
	if refusal := s.openPRCloseRefusal(ctx, existing); refusal != nil {
		writeError(w, http.StatusConflict, refusal.Error())
		return
	}
	if err := openReleaseCloseError(existing, nil); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}

	// Decode body as generic map to capture extra fields. Body is optional;
	// ignore decode errors to preserve prior behavior.
	var body map[string]any
	if len(bodyBytes) > 0 {
		_ = json.Unmarshal(bodyBytes, &body)
	}

	// SECURITY (AUDIT §C3): ignore client-supplied closed_by — derive actor
	// from the authenticated identity. We still strip closed_by from the
	// body so stale clients don't leak it into the bead's extra fields.
	closedBy := actorFromContext(r.Context())

	// Merge extra fields (anything except closed_by) into the bead before closing.
	extraFields := make(map[string]any)
	for k, v := range body {
		if k != "closed_by" {
			extraFields[k] = v
		}
	}
	// A runner may not stamp runtime_id through the close body either: the
	// stream gate trusts it (runnerRuntimeIDDenial). Refused before the close,
	// since a refused mergeBeadFields below would still close the bead.
	if denial := runnerRuntimeIDFieldsDenial(r.Context(), "close", extraFields, existing); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return
	}
	// control: mail closes only with a control_acted stamp unless the caller
	// may send it (mail_control.go).
	if denial := s.mailControlCloseDenial(r.Context(), "close", existing, extraFields); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return
	}
	// The extra fields are written by the same primitive the field endpoints
	// use, so they get the same identity-field gate (a user bead's close body
	// must not smuggle in email_aliases).
	if denial := s.authorizeUserBeadFieldWrite(r.Context(), "close", existing, mapKeys(extraFields)...); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return
	}
	if denial := s.authorizeAgentBeadFieldWrite(r.Context(), "close", existing, mapKeys(extraFields)...); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return
	}
	if err := s.fieldEndpointKeyError(r.Context(), existing, mapKeys(extraFields)...); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Nor a message's origin or author that an update would refuse
	// (commons_origin.go, message_author.go): the gates read the raw body, so
	// a key given twice or in another letter case is refused too.
	if len(extraFields) > 0 {
		for _, gate := range []func(context.Context, *model.Bead, json.RawMessage) *AuthzDenial{commonsOriginPatchDenial, messageAuthorPatchDenial, func(_ context.Context, b *model.Bead, p json.RawMessage) *AuthzDenial {
			return messageGitHubAuthorPatchDenial("close", b, p)
		}} {
			if denial := gate(r.Context(), existing, bodyBytes); denial != nil {
				denial.Action = "close"
				s.auditDenial(r, denial, id)
				writeAuthzDenial(w, denial)
				return
			}
		}
	}
	// Nor an escalation's approval terms or frozen fields (audience,
	// sensitivity, credential_requirements). escalationBlocksGenericClose
	// already diverts every open escalation above, so this holds the line if
	// that divert ever loosens (#756 send-back).
	for _, check := range []func(context.Context, *model.Bead, ...string) error{checkEscalationFieldWrite, checkEscalationFieldEndpointWrite, checkEscalationFrozenFieldEndpoint, checkLandStewardApprovesFieldEndpoint} {
		if err := check(r.Context(), existing, mapKeys(extraFields)...); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	// A decision's filer may withdraw it with a bare close, but a caller that
	// may not resolve decisions cannot ride an answer in on the close body
	// (gd-Du6wHclx4g).
	if denial := denyDecisionStampedKeyWrite("close", existing, mapKeys(extraFields)); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return
	}
	if denial := s.authorizeDecisionAnswerFields(r.Context(), "close", existing, mapKeys(extraFields)...); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return
	}
	if writesDecisionAnswer(existing, mapKeys(extraFields)) {
		maps.Copy(extraFields, s.decisionResolverStamp(r.Context()))
	}
	// A ledger record's fields never change (lands_ledger.go), so a close
	// body cannot carry any.
	if len(extraFields) > 0 && s.ledgerFieldEndpointDenied(w, r, "close", existing) {
		return
	}
	if len(extraFields) > 0 {
		if err := s.mergeBeadFields(r.Context(), id, extraFields); err != nil {
			// Non-fatal — log and continue with close.
			slog.Warn("failed to merge close fields", "bead", id, "error", err)
		}
	}
	// A close by an impersonated session names the admin behind it
	// (impersonation.go). Stamped before the close, so a failure leaves the
	// bead open.
	if err := s.stampImpersonatedByOn(r.Context(), id); err != nil {
		writeServerError(r.Context(), w, "handleCloseBead", err, "failed to stamp impersonated_by")
		return
	}

	bead, err := s.store.CloseBead(r.Context(), id, closedBy)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleCloseBead", err, "failed to close bead")
		return
	}
	if bead == nil {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}

	// Taint propagation on write (ADR docs/adr/taints.md §4.2 row 4).
	if err := s.propagateWriteTaint(r.Context(), bead.ID, closedBy); err != nil {
		writeServerError(r.Context(), w, "handleCloseBead", err, "failed to propagate write taint")
		return
	}

	s.recordAndPublish(r.Context(), events.TopicBeadClosed, bead.ID, closedBy, events.BeadClosed{
		Bead:     bead,
		ClosedBy: closedBy,
	})
	s.recordMutation(r.Context(), bead.ID, closedBy, "status", string(existing.Status), string(bead.Status), model.MutationTypeUpdate)
	s.recordCloseClient(r.Context(), bead.ID, closedBy)

	// Auto-close cascade (ka-0000001587): drain a completed parent epic /
	// release a close-on-escalation wrapper. Best-effort; never blocks this close.
	s.afterBeadClosed(r.Context(), bead)

	writeJSON(w, http.StatusOK, bead)
}

// handleTransferBead handles POST /v1/beads/{id}/transfer. Moves a bead and
// its related rows to a different fleet.
//
// Only an OIDC gardenio-admin session may transfer (gd-i6GZX5jzHM), checked
// here in the handler as well as by the route's requireRole(RoleAdmin):
// requireRole still passes a verified v1 edge envelope, and no static, fleet,
// machine or land caller has a transfer call site (kd fleet transfer is a
// human command), so none gets a grant. A daemon with no auth configured
// (authMethodNone, dev/test) stays open, as on every admin route. The bead's
// write_policy and ownership gate then run like any other update.
func (s *BeadsServer) handleTransferBead(w http.ResponseWriter, r *http.Request) {
	if !callerMayTransferBead(r.Context()) {
		writeError(w, http.StatusForbidden, "insufficient permissions: transferring a bead requires "+RoleAdmin+" (OIDC)")
		return
	}
	id := r.PathValue("id")
	if id == "" {
		writeError(w, http.StatusBadRequest, "id is required")
		return
	}

	var body struct {
		ToFleet string `json:"to_fleet"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if body.ToFleet == "" {
		writeError(w, http.StatusBadRequest, "to_fleet is required")
		return
	}

	existing, err := s.store.GetBead(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && existing == nil) {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleTransferBead", err, "failed to fetch bead")
		return
	}
	if denial := s.authorizeWriteForBead(r.Context(), "update", existing); denial != nil {
		s.auditDenial(r, denial, id)
		writeAuthzDenial(w, denial)
		return
	}

	bead, fromFleet, err := s.store.TransferBead(r.Context(), id, body.ToFleet)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "bead not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleTransferBead", err, "failed to transfer bead")
		return
	}

	s.recordAndPublish(r.Context(), events.TopicBeadTransferred, id, "", events.BeadTransferred{
		Bead:      bead,
		FromFleet: fromFleet,
		ToFleet:   body.ToFleet,
	})

	writeJSON(w, http.StatusOK, bead)
}

// callerMayTransferBead reports whether ctx may call POST
// /v1/beads/{id}/transfer: an OIDC gardenio-admin session, or a daemon with
// no auth configured.
func callerMayTransferBead(ctx context.Context) bool {
	return authMethodFromContext(ctx) == authMethodNone || isOIDCAdminSession(ctx)
}
