package server

import (
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/store"
)

// defaultOperatorsLimit mirrors the store's own limit<=0 fallback
// (listOperatorsWithSlugs in internal/store/postgres/operators.go). Passing it
// as the clampListBeadsLimit default keeps the omitted-limit behavior identical
// (0 -> 500) while capping any positive value at maxListBeadsLimit.
const defaultOperatorsLimit = 500

// maxOperatorStatsRecentWindowHours caps the caller-supplied
// recent_window_hours on GET /v1/operators/stats. The store binds this value
// into `now() - ($3 || ' hours')::interval`; an astronomically large value
// both defeats the age bound (admitting every historical bead into the
// chip fan-out — the OOM lever) AND can overflow Postgres's interval range and
// 500 rather than 400. 100 years is far past any real "recently created"
// window while staying comfortably inside the interval type. 0 (omitted) is
// left untouched so the store applies its own default window.
const maxOperatorStatsRecentWindowHours = 24 * 365 * 100

// LookupOperatorByAliasResponse is the response body for
// GET /v1/operators/by-alias?kind=X&subject=Y. Returns the operator id and
// canonical operators(kind) for an existing alias. Found=false means no
// alias row exists for (kind, subject) — caller should treat the OIDC
// subject as unprovisioned.
type LookupOperatorByAliasResponse struct {
	OperatorID   int64  `json:"operator_id"`
	OperatorKind string `json:"operator_kind"`
	Found        bool   `json:"found"`
}

// LookupOperatorBySlugResponse is the response body for
// GET /v1/operators/by-slug/{slug}. Resolves a current OR retired slug
// through operator_slug_aliases. Found=false means no alias exists for
// the slug.
type LookupOperatorBySlugResponse struct {
	OperatorID int64 `json:"operator_id"`
	Found      bool  `json:"found"`
}

// GetOperatorSlugResponse is the response body for
// GET /v1/operators/{id}/slug. Returns the operator's current canonical
// slug. Found=false means the operator id is unknown or has no slug.
type GetOperatorSlugResponse struct {
	Slug  string `json:"slug"`
	Found bool   `json:"found"`
}

// OperatorDirectoryEntry mirrors store.OperatorDirectoryEntry in JSON form
// for the GET /v1/operators directory feed (M2 §4 /people, ka-0000000340).
type OperatorDirectoryEntry struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Display string `json:"display"`
	Email   string `json:"email"`
	Slug    string `json:"slug"`
}

// ListOperatorsResponse is the response body for GET /v1/operators.
type ListOperatorsResponse struct {
	Operators []OperatorDirectoryEntry `json:"operators"`
}

// humanOperatorKinds is the set of operator kinds the /people directory treats
// as (candidate) humans — the SAME set the viewer's filterHumanOperators keeps
// (gardenio beads-viewer people.go peopleHumanKinds). Only rows of these kinds
// are subject to the bot-marker screen below; sentinel kinds (system, fleet,
// gitlab, jira, agent, email) are NOT screened here because /fleet renders them
// on purpose and they legitimately carry @noreply.kbeads.local sentinel emails.
var humanOperatorKinds = map[string]struct{}{
	"oidc":  {},
	"slack": {},
}

// botOperatorMarkerRe matches an OBJECTIVE non-human marker in a human-kind
// operator's identifier strings (subject / email). It is an EXACT mirror of the
// Users-CAMP harness BOT_ROW_MARKER_RE (gardenio
// tests/beads-viewer-screenshots/tools/measure-users-camp.py, bug ka-0000001715)
// so the page render, this feed's ground-truth (row_vs_truth_delta), and the
// harness's independent bot_rows_rendered assertion all reconcile at zero once
// this screen is in place. Keep the two patterns in lockstep — a marker one side
// screens but the other does not re-opens the "delta stays 0 while bots render"
// blind spot (ka-0000004034).
//
// The markers are definitional, not heuristic:
//   - @noreply. / .noreply. — a no-reply email domain (slack-U…@noreply.kbeads.local
//     on a pre-resync slack row, group_…@noreply.gitlab.com bot service accounts,
//     …@users.noreply.github.com). No human logs in as one.
//   - @example.(com|org|net) — RFC 2606 reserved fixture domains (e2e-fixture@…).
//   - _bot_ — a GitLab bot service-account token (group_<n>_bot_<hash>).
//
// DELIBERATELY NOT screened: a service account on a REAL domain
// (info-density-audit@acme.example, gardenio@acme.example). No objective address
// pattern separates those from a human; calling them junk is a judge curation
// call, and widening the markers would false-positive real employees and never
// reconcile with the harness (its BOT_ROW_MARKER_RE makes the same exclusion).
// The (?i) flag mirrors the harness's re.I: email domains are case-insensitive
// (RFC 1035 §2.3.3), so an address like Foo@Noreply.Gitlab.com is the same bot
// domain and must screen identically on both sides.
var botOperatorMarkerRe = regexp.MustCompile(
	`(?i)@noreply\.` + // …@noreply.gitlab.com, slack-U…@noreply.kbeads.local
		`|\.noreply\.` + // …@users.noreply.github.com (noreply as a sub-label)
		`|@example\.(?:com|org|net)\b` + // RFC 2606 fixture domains (e2e-fixture@example.com)
		`|_bot_`, // GitLab bot service accounts (group_<n>_bot_<hash>)
)

// isBotOperatorRow reports whether a human-KIND directory row is actually a
// bot/service/fixture identity that must not surface as a person. Only rows of
// a human kind are screened (see humanOperatorKinds) — a sentinel kind is out
// of scope and returns false regardless of its email. For an in-scope row it
// screens BOTH identifier fields: an oidc row carries the address in Subject
// (and usually Email); a slack row whose OIDC resync has not run yet carries
// the synthetic slack-<U…>@noreply.kbeads.local sentinel in Email while Subject
// is the bare U-id, so screening both catches either shape.
func isBotOperatorRow(row store.OperatorDirectoryEntry) bool {
	if _, ok := humanOperatorKinds[row.Kind]; !ok {
		return false
	}
	return botOperatorMarkerRe.MatchString(row.Subject) ||
		botOperatorMarkerRe.MatchString(row.Email)
}

// serviceOperatorSubjects is an explicit, auditable deny-list of known
// service/CI identities that authenticate on a REAL corporate domain and so
// carry NO objective non-human address marker — botOperatorMarkerRe provably
// cannot reach them (see its "DELIBERATELY NOT screened" note). They are all
// kind='oidc' with an ordinary email-shaped subject and would otherwise render
// on /people as colleagues, diluting the "every human, no junk" claim the
// directory exists to make (ka-0000004673). Each entry is a SPECIFIC provisioned
// identity, not a pattern, so the screen cannot false-positive a real employee
// on the same domain: jordan.lake@acme.example (a distinct human) is untouched;
// only joel.approver@ (the SCIF approver@ approval fixture) is listed.
//
// Why an explicit list and not a wider regex: the marker approach is exhausted
// here — no address pattern separates gardenio2@acme.example from a real employee
// without also catching employees (ka-0000004034 scoped this class out for
// exactly that reason). Why not an operators.is_service schema column: it is the
// right long-term shape, but these rows are auto-created via OIDC login /
// alias-resync with no provisioning step to SET the flag, and kbeads forbids
// eager full-table backfills on the prod operators table (kbeads/CLAUDE.md
// migration policy) — so a bare column would ship defaulted-false everywhere and
// not fix the bug. The deny-list lands the fix now; the column can subsume it
// later once a provisioning + backfill path exists.
//
// Keys are lowercased; the lookup lowercases the row's subject/email first.
// Email domains are case-insensitive (RFC 1035 §2.3.3) and these local-parts are
// canonical, so a case fold cannot admit an entry it should not.
var serviceOperatorSubjects = map[string]struct{}{
	"gardenio@acme.example":           {}, // agent fleet's own identity / browse-only Jira account
	"gardenio2@acme.example":          {}, // second Jira API-token identity
	"info-density-audit@acme.example": {}, // info-density-audit OIDC claim (RWX-era audit job)
	"joel.approver@acme.example":      {}, // CI approval fixture (SCIF approver@ convention)
}

// isServiceOperatorRow reports whether a human-KIND directory row is a known
// service/CI identity on the explicit serviceOperatorSubjects deny-list. Like
// isBotOperatorRow it only screens human-kind rows (a sentinel kind returns
// false regardless of subject) and checks BOTH identifier fields, so a row whose
// service address lives in Subject OR Email is caught.
func isServiceOperatorRow(row store.OperatorDirectoryEntry) bool {
	if _, ok := humanOperatorKinds[row.Kind]; !ok {
		return false
	}
	if _, ok := serviceOperatorSubjects[strings.ToLower(strings.TrimSpace(row.Subject))]; ok {
		return true
	}
	_, ok := serviceOperatorSubjects[strings.ToLower(strings.TrimSpace(row.Email))]
	return ok
}

// isNonHumanOperatorRow unions the two objective humans-only screens applied to
// the /v1/operators feed: the marker regex (isBotOperatorRow — noreply/fixture
// domains, _bot_ tokens) and the explicit service-subject deny-list
// (isServiceOperatorRow — provisioned CI/service identities on real domains the
// regex cannot reach, ka-0000004673). A human directory needs BOTH: the marker
// screen catches junk by address shape, the deny-list catches the marker-
// invisible residue that shape leaves behind. Both are anchored to
// humanOperatorKinds, so a sentinel kind is never screened here.
func isNonHumanOperatorRow(row store.OperatorDirectoryEntry) bool {
	return isBotOperatorRow(row) || isServiceOperatorRow(row)
}

// handleListOperators handles GET /v1/operators?limit=N.
// Viewer-role: returns operators that have a canonical slug, ordered by slug
// ascending, for the public /people directory (M2 §4, ka-0000000340). Only
// slugged operators are exposed — sentinel/system rows without a slug are
// excluded by the underlying store query, and human-kind rows that are not
// actually people (isNonHumanOperatorRow) are screened here: the marker regex
// catches bot/fixture identities by address shape (ka-0000004034), and the
// explicit deny-list catches provisioned service/CI identities on real domains
// the regex cannot reach (ka-0000004673). Screening at the feed — not just in
// the viewer — keeps /people and its harness ground-truth query consistent, so
// row_vs_truth_delta stays 0 while the directory is humans-only.
func (s *BeadsServer) handleListOperators(w http.ResponseWriter, r *http.Request) {
	limit := 0
	if q := r.URL.Query().Get("limit"); q != "" {
		n, err := strconv.Atoi(q)
		if err != nil || n < 0 {
			http.Error(w, "invalid limit", http.StatusBadRequest)
			return
		}
		limit = n
	}
	// Bound the caller-supplied limit through the shared OOM choke point
	// (ka-0000006889): the same sibling class as the gRPC ListBeads
	// (ka-0000003195), graph-family (ka-0000007023), and audit/decisions/
	// escalations (ka-0000001252) bypasses. handleListOperators is
	// RoleViewer-gated and powers the public /people directory, so an
	// unbounded LIMIT $1 let a low-privilege caller drive the shared daemon
	// toward OOM. limit==0 keeps the store's own 500 default (clamp def=500).
	limit = clampListBeadsLimit(limit, defaultOperatorsLimit)
	rows, err := s.store.ListOperatorsWithSlugs(r.Context(), limit)
	if err != nil {
		writeServerError(r.Context(), w, "handleListOperators", err, "failed to list operators")
		return
	}
	out := ListOperatorsResponse{Operators: make([]OperatorDirectoryEntry, 0, len(rows))}
	for _, row := range rows {
		if isNonHumanOperatorRow(row) {
			continue
		}
		out.Operators = append(out.Operators, OperatorDirectoryEntry{
			ID:      row.ID,
			Kind:    row.Kind,
			Subject: row.Subject,
			Display: row.Display,
			Email:   row.Email,
			Slug:    row.Slug,
		})
	}
	writeJSON(w, http.StatusOK, &out)
}

// OperatorStatEntry is one operator's real aggregate in the
// GET /v1/operators/stats response (ka-0000001390). LastActive is RFC3339;
// the zero time means "no activity" (operator has no attributed beads).
//
// The per-operator counts (InProgress … GardenersActive) are additive fields
// from the §4.1 superset GROUP BY (ka-0000002003); they come from the same
// single query as OpenBeads, so /people stays at 3 bounded queries total.
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

// OperatorRecentBeadEntry is one "recently created" chip in the
// GET /v1/operators/stats response, keyed by created_by_operator_id.
type OperatorRecentBeadEntry struct {
	OperatorID int64  `json:"operator_id"`
	BeadID     string `json:"bead_id"`
	Title      string `json:"title"`
	CreatedAt  string `json:"created_at"`
}

// OperatorStatsResponse is the response body for GET /v1/operators/stats.
type OperatorStatsResponse struct {
	Stats  []OperatorStatEntry       `json:"stats"`
	Recent []OperatorRecentBeadEntry `json:"recent"`
}

// handleOperatorStats handles
// GET /v1/operators/stats?recent_per_operator=N&recent_limit=M&recent_window_hours=H.
// Viewer-role: returns real per-operator aggregates (open/in_progress count +
// human-authored last-active recency via GROUP BY operated_by_id over the whole
// beads table) plus a bounded, age-limited per-operator "recently created" chip
// list keyed on operated_by_id. Drives the beads-viewer /people directory stats
// (ka-0000001390), replacing the old 5000-bead in-memory sample whose counts
// were sampling artifacts against a 245K+ open-bead fleet. The counts are
// alias-resolved: operated_by_id is the canonical operator the write-path
// bound through operator_aliases, so every linked identity folds into one row.
func (s *BeadsServer) handleOperatorStats(w http.ResponseWriter, r *http.Request) {
	parse := func(key string) (int, bool) {
		q := r.URL.Query().Get(key)
		if q == "" {
			return 0, true
		}
		n, err := strconv.Atoi(q)
		if err != nil || n < 0 {
			return 0, false
		}
		return n, true
	}
	recentPerOperator, ok := parse("recent_per_operator")
	if !ok {
		http.Error(w, "invalid recent_per_operator", http.StatusBadRequest)
		return
	}
	recentLimit, ok := parse("recent_limit")
	if !ok {
		http.Error(w, "invalid recent_limit", http.StatusBadRequest)
		return
	}
	// recent_window_hours bounds the age of a "recently created" chip
	// (ka-0000001991). Omitted / 0 falls back to the store default; the
	// /people directory passes an explicit window so a months-old bead never
	// renders as current work.
	recentWindowHours, ok := parse("recent_window_hours")
	if !ok {
		http.Error(w, "invalid recent_window_hours", http.StatusBadRequest)
		return
	}
	// Bound all three caller-supplied params through the shared OOM choke
	// point (ka-0000006889), mirroring the sibling handleListOperators clamp
	// above. handleOperatorStats is RoleViewer-gated and powers the same
	// public /people directory, but its params flowed straight into the store
	// with only an n<0 reject: recent_limit binds to the chip query's outer
	// LIMIT $2 (the total-rows ceiling that sizes the returned slice),
	// recent_per_operator to `rn <= $1` (the per-operator cap), and
	// recent_window_hours to the age interval. A low-privilege caller passing
	// huge values (e.g. recent_limit=1e9, recent_window_hours=8760000) erased
	// every bound and made the store materialize the whole beads table into
	// memory — the same OOM class already clamped on every sibling list
	// endpoint (gRPC ListBeads ka-0000003195, graph ka-0000007023,
	// audit/decisions/escalations ka-0000001252, operators list above). 0
	// (omitted) is preserved for each so the store keeps applying its own
	// defaults (defaultOperatorStatsRecent* in operators_stats.go).
	recentLimit = clampListBeadsLimit(recentLimit, 0)
	recentPerOperator = clampListBeadsLimit(recentPerOperator, 0)
	if recentWindowHours > maxOperatorStatsRecentWindowHours {
		recentWindowHours = maxOperatorStatsRecentWindowHours
	}
	stats, err := s.store.OperatorDirectoryStats(r.Context(), recentPerOperator, recentLimit, recentWindowHours)
	if err != nil {
		writeServerError(r.Context(), w, "handleOperatorStats", err, "failed to compute operator stats")
		return
	}
	out := OperatorStatsResponse{
		Stats:  make([]OperatorStatEntry, 0, len(stats.Stats)),
		Recent: make([]OperatorRecentBeadEntry, 0, len(stats.Recent)),
	}
	for _, st := range stats.Stats {
		out.Stats = append(out.Stats, OperatorStatEntry{
			OperatorID:       st.OperatorID,
			OpenBeads:        st.OpenBeads,
			InProgress:       st.InProgress,
			Created7d:        st.Created7d,
			Closed7d:         st.Closed7d,
			MRsOpen:          st.MRsOpen,
			MRsMerged7d:      st.MRsMerged7d,
			DecisionsPending: st.DecisionsPending,
			AgentsActive:     st.AgentsActive,
			GardenersActive:  st.GardenersActive,
			LastActive:       st.LastActive.UTC().Format(time.RFC3339),
		})
	}
	for _, rb := range stats.Recent {
		out.Recent = append(out.Recent, OperatorRecentBeadEntry{
			OperatorID: rb.OperatorID,
			BeadID:     rb.BeadID,
			Title:      rb.Title,
			CreatedAt:  rb.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	writeJSON(w, http.StatusOK, &out)
}

// handleLookupOperatorByAlias handles GET /v1/operators/by-alias.
// Viewer-role: drives the gardenio beads-viewer's OIDC-subject → operator-id
// resolver (inbox-v1 G2). Read-only PK probe + JOIN, response carries the
// parent operators(kind) so the caller can sentinel-filter
// (kind ∈ {system,fleet}) without a second round-trip.
func (s *BeadsServer) handleLookupOperatorByAlias(w http.ResponseWriter, r *http.Request) {
	kind := r.URL.Query().Get("kind")
	subject := r.URL.Query().Get("subject")
	if kind == "" || subject == "" {
		http.Error(w, "kind and subject are required", http.StatusBadRequest)
		return
	}
	subject = canonicalAliasSubject(kind, subject)
	id, opKind, err := s.store.LookupOperatorByAlias(r.Context(), kind, subject)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusOK, &LookupOperatorByAliasResponse{Found: false})
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleLookupOperatorByAlias", err, "failed to lookup operator alias")
		return
	}
	writeJSON(w, http.StatusOK, &LookupOperatorByAliasResponse{
		OperatorID:   id,
		OperatorKind: opKind,
		Found:        true,
	})
}

// handleLookupOperatorBySlug handles GET /v1/operators/by-slug/{slug}.
// Viewer-role: drives the beads-viewer's /u/{slug} per-user homepage route
// (ka-0000000022 / epic ka-0000006114). Resolves current AND retired slugs
// through operator_slug_aliases — old links keep working after a rename.
func (s *BeadsServer) handleLookupOperatorBySlug(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimSpace(r.PathValue("slug"))
	if slug == "" {
		http.Error(w, "slug is required", http.StatusBadRequest)
		return
	}
	id, err := s.store.GetOperatorBySlug(r.Context(), slug)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusOK, &LookupOperatorBySlugResponse{Found: false})
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleLookupOperatorBySlug", err, "failed to lookup operator by slug")
		return
	}
	writeJSON(w, http.StatusOK, &LookupOperatorBySlugResponse{
		OperatorID: id,
		Found:      true,
	})
}

// handleGetOperatorSlug handles GET /v1/operators/{id}/slug.
// Viewer-role: powers /me by looking up the viewer's current canonical
// slug for the 308 redirect target. Returns Found=false (not 404) when
// the operator has no slug so callers can fall through to op-<id>.
func (s *BeadsServer) handleGetOperatorSlug(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	operatorID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || operatorID <= 0 {
		http.Error(w, "invalid operator id", http.StatusBadRequest)
		return
	}
	slug, err := s.store.GetOperatorSlug(r.Context(), operatorID)
	if errors.Is(err, sql.ErrNoRows) {
		writeJSON(w, http.StatusOK, &GetOperatorSlugResponse{Found: false})
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleGetOperatorSlug", err, "failed to get operator slug")
		return
	}
	writeJSON(w, http.StatusOK, &GetOperatorSlugResponse{
		Slug:  slug,
		Found: true,
	})
}
