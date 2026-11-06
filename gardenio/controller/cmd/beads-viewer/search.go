package main

import (
	"errors"
	"html/template"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// searchResultView is a template-friendly view of one search result.
type searchResultView struct {
	ID               string
	Title            string
	ShortTitle       string // shown in place of Title when set (gd-V5nGseRS5f)
	Heraldry         string // the bead's arms, canonical; "" when none (arms.go)
	Type             string
	Status           string
	Priority         int    // 0-5; rendered as chip-priority-N (0 = highest)
	Score            string // formatted similarity score (empty for lexical results)
	Snippet          string // truncated description
	Age              string // relative age (e.g. "5m", "3d") of UpdatedAt
	UpdatedAt        time.Time
	Labels           []string // raw bead labels — used for project facets
	OperatorChipHTML template.HTML
	// OperatorChipCompactHTML is the initials-only variant rendered in the
	// `search-as-list` table (ka-0000000305 / ka-0000004740). Empty for
	// rows where the underlying chip kind has no compact treatment
	// (scheduler / empty / self / suppressed).
	OperatorChipCompactHTML template.HTML
}

// facetCount is one row of the per-type / per-status breakdown rendered in
// the results header. Sorted descending by Count for compact, scannable
// "12 task, 5 bug, …"-style summaries.
type facetCount struct {
	Key   string
	Count int
}

// computeFacets returns descending-by-count breakdowns over Type and Status.
// Keys with empty values are skipped so the summary stays signal-dense.
func computeFacets(views []searchResultView) (types, statuses []facetCount) {
	tcount := map[string]int{}
	scount := map[string]int{}
	for _, v := range views {
		if v.Type != "" {
			tcount[v.Type]++
		}
		if v.Status != "" {
			scount[v.Status]++
		}
	}
	types = sortFacets(tcount)
	statuses = sortFacets(scount)
	return
}

// computeProjectFacets returns descending-by-count counts of distinct
// `project:<name>` labels seen across the views. Each bead contributes at
// most one count per project (duplicates within a bead are deduped). The
// `project:` prefix is stripped from the facet Key for compact rendering.
func computeProjectFacets(views []searchResultView) []facetCount {
	counts := map[string]int{}
	for _, v := range views {
		seen := map[string]bool{}
		for _, l := range v.Labels {
			if !strings.HasPrefix(l, "project:") {
				continue
			}
			name := strings.TrimPrefix(l, "project:")
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			counts[name]++
		}
	}
	return sortFacets(counts)
}

// ageBuckets defines the buckets used by computeAgeBucketFacets in display
// order. Each bucket counts views whose UpdatedAt falls in (prev, max].
// The final "older" bucket has max == 0 meaning unbounded.
var ageBuckets = []struct {
	key string
	max time.Duration
}{
	{"<1h", time.Hour},
	{"today", 24 * time.Hour},
	{"this week", 7 * 24 * time.Hour},
	{"this month", 30 * 24 * time.Hour},
	{"older", 0},
}

// computeAgeBucketFacets returns counts of view UpdatedAt grouped into
// fixed buckets relative to now. Buckets with zero counts are omitted.
// Output order is fixed (newest → oldest) — not sorted by count — so users
// see a stable timeline regardless of which buckets are populated.
func computeAgeBucketFacets(views []searchResultView, now time.Time) []facetCount {
	bucketCounts := make([]int, len(ageBuckets))
	for _, v := range views {
		if v.UpdatedAt.IsZero() {
			continue
		}
		d := now.Sub(v.UpdatedAt)
		if d < 0 {
			d = 0
		}
		for i, b := range ageBuckets {
			if b.max == 0 || d < b.max {
				bucketCounts[i]++
				break
			}
		}
	}
	out := make([]facetCount, 0, len(ageBuckets))
	for i, b := range ageBuckets {
		if bucketCounts[i] > 0 {
			out = append(out, facetCount{Key: b.key, Count: bucketCounts[i]})
		}
	}
	return out
}

func sortFacets(m map[string]int) []facetCount {
	out := make([]facetCount, 0, len(m))
	for k, c := range m {
		out = append(out, facetCount{Key: k, Count: c})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// computeAgeSummary returns formatted oldest/newest/median ages over the
// UpdatedAt of the views, relative to now. Skips views with a zero time.
// Returns empty strings when there is no data.
func computeAgeSummary(views []searchResultView, now time.Time) (oldest, newest, median string) {
	times := make([]time.Time, 0, len(views))
	for _, v := range views {
		if !v.UpdatedAt.IsZero() {
			times = append(times, v.UpdatedAt)
		}
	}
	if len(times) == 0 {
		return "", "", ""
	}
	sort.Slice(times, func(i, j int) bool { return times[i].Before(times[j]) })
	oldest = fmtTimeAgo(times[0], now)
	newest = fmtTimeAgo(times[len(times)-1], now)
	median = fmtTimeAgo(times[len(times)/2], now)
	return
}

// highlightQuery returns text with case-insensitive substring matches of query
// wrapped in <strong>. Whitespace-separated query tokens are highlighted
// independently so multi-word queries (e.g. "auth bug") bold each token.
// Empty query returns the escaped text unchanged.
func highlightQuery(text, query string) template.HTML {
	q := strings.TrimSpace(query)
	if q == "" || text == "" {
		return template.HTML(template.HTMLEscapeString(text))
	}
	tokens := strings.Fields(q)
	if len(tokens) == 0 {
		return template.HTML(template.HTMLEscapeString(text))
	}
	lower := strings.ToLower(text)
	// Collect non-overlapping match ranges by scanning each token.
	type span struct{ start, end int }
	var spans []span
	for _, tok := range tokens {
		tl := strings.ToLower(tok)
		if tl == "" {
			continue
		}
		off := 0
		for off < len(lower) {
			i := strings.Index(lower[off:], tl)
			if i < 0 {
				break
			}
			s := off + i
			e := s + len(tl)
			spans = append(spans, span{start: s, end: e})
			off = e
		}
	}
	if len(spans) == 0 {
		return template.HTML(template.HTMLEscapeString(text))
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start < spans[j].start })
	// Merge overlapping ranges so we never wrap <strong> inside another <strong>.
	merged := spans[:1]
	for _, s := range spans[1:] {
		last := &merged[len(merged)-1]
		if s.start <= last.end {
			if s.end > last.end {
				last.end = s.end
			}
		} else {
			merged = append(merged, s)
		}
	}
	var b strings.Builder
	cursor := 0
	for _, s := range merged {
		b.WriteString(template.HTMLEscapeString(text[cursor:s.start]))
		b.WriteString("<strong class=\"search-hl\">")
		b.WriteString(template.HTMLEscapeString(text[s.start:s.end]))
		b.WriteString("</strong>")
		cursor = s.end
	}
	b.WriteString(template.HTMLEscapeString(text[cursor:]))
	return template.HTML(b.String())
}

// snippet returns the first N runes of s with a trailing ellipsis when truncated.
// Newlines are collapsed to single spaces so the table row renders cleanly.
func snippet(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", " "), "\n", " "))
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return strings.TrimSpace(string(runes[:n])) + "…"
}

// cookieSearchDensity persists the /search row-density preference. Reuses
// the agents allowedFeedDensities allow-list ("", "list", "compact"). Default
// is "compact" so the audit's signal-per-pixel target is hit out of the box;
// operators who prefer the snippet-included list view can toggle to "list".
const cookieSearchDensity = "search_density"

const defaultSearchDensity = "compact"

// searchDensity returns the active row-density preference for /search.
// Cookie/query round-trip is identical to the /agents toggle (ka-0000005947).
func (s *Server) searchDensity(w http.ResponseWriter, r *http.Request) string {
	return readUIPreference(w, r, cookieSearchDensity, "density", defaultSearchDensity, allowedFeedDensities)
}

// handleSearch serves GET /search. Query params:
//
//	q        — query text
//	status   — comma-separated statuses to filter by
//	type     — comma-separated bead types to filter by
//	labels   — comma-separated labels to filter by
//	density  — "compact" | "list" (UI preference; cookie-persisted)
//
// When q is empty the page renders the form only. Otherwise the handler calls
// POST /v1/search/semantic and degrades gracefully on ANY semantic failure
// (503 embedder-disabled, 500 embed-call-failed, or transient error) to
// lexical GET /v1/beads?search=q (returning the same view-model shape).
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	statusFilter := parseCommaSeparated(r.URL.Query().Get("status"))
	typeFilter := parseCommaSeparated(r.URL.Query().Get("type"))
	labelsFilter := parseCommaSeparated(r.URL.Query().Get("labels"))
	operatedByRaw := strings.TrimSpace(r.URL.Query().Get("operated_by"))
	operatedBySlug := strings.TrimSpace(r.URL.Query().Get("operated_by_slug"))

	email := oidcauth.EmailFromContext(r.Context())
	urlHasOperatedBy := r.URL.Query().Has("operated_by")

	// §9.5 telemetry — /search is the chip's secondary "All beads" target.
	// Logging viewer + filter presence here lets the offline aggregator
	// compute /u/:slug link clicks vs. /search?operated_by_slug=… fallback.
	// One emit per request (covers every render branch below).
	emitTelemetry(s.logger, telemetryEventSearchView,
		"viewer_email", trimEmail(email),
		"has_query", query != "",
		"has_operated_by_slug", operatedBySlug != "",
		"has_operated_by", urlHasOperatedBy,
		"operated_by_slug", operatedBySlug,
		"referer_path", refererPath(r),
	)
	chipExtra := r.URL.Query()
	chipExtra.Del("operated_by")
	// /search is default-on per the approved decision (ka-0000007227): when the
	// `?operated_by=` key is absent and the viewer is an authenticated OIDC
	// user, scope results to their own work. An explicit `?operated_by=` (any
	// value, including empty for "show all") opts out and is echoed back.
	operatedByRaw = resolveDefaultOperatedBy(r, operatedByRaw)
	operatedBy, isMe := resolveOperatedByParam(operatedByRaw, email)
	operatedByIsNull := operatedByRaw == "null" || strings.EqualFold(operatedByRaw, "(none)")

	data := map[string]any{
		"Query":               query,
		"StatusFilter":        strings.Join(statusFilter, ","),
		"TypeFilter":          strings.Join(typeFilter, ","),
		"LabelsFilter":        strings.Join(labelsFilter, ","),
		"OperatedByFilter":    operatedByRaw,
		"OperatedByMe":        isMe,
		"OperatedByActive":    operatedByRaw != "",
		"OperatedByURLHasKey": urlHasOperatedBy,
		"EffectiveOperatedBy": operatedBy,
		"OperatedByIsUnowned": operatedByIsNull,
		"OperatedByDisplay":   "",
		"ChipBasePath":        s.basePath + "/search",
		"ChipExtraQuery":      chipExtra.Encode(),
		"ViewerEmail":         email,
		// /search is a default-on surface per V-R2 §5.3 rule 3. Banner gates
		// (OIDC + non-Matt + non-dismissed + V-R4 sunset flag/backstop) are
		// computed in shouldRenderMigrationBanner.
		"ShowMigrationBanner": s.shouldRenderMigrationBanner(r),
		"GardenHref":          s.gardenBanner(r, retireSearch, gardenSearchTarget),
		"Results":             []searchResultView{},
		"Mode":                "", // "semantic", "lexical", or "" when no query
		"SearchDensity":       s.searchDensity(w, r),
	}

	if query == "" && !data["OperatedByActive"].(bool) {
		s.render(w, r, "search.html", data)
		return
	}

	// Try semantic first.
	semReq := beadsapi.SemanticSearchRequest{
		Query:            query,
		Status:           statusFilter,
		Type:             typeFilter,
		Labels:           labelsFilter,
		OperatedBy:       operatedBy,
		OperatedByIsNull: operatedByIsNull,
		Limit:            20,
	}
	if query != "" {
		semResp, err := s.daemon.SemanticSearch(r.Context(), semReq)
		if err == nil {
			data["Mode"] = "semantic"
			data["Model"] = semResp.Model
			// Per-user-homepage M2 §6 (ka-0000007787): populate slugs on the
			// embedded beads before chip rendering so the search rows link
			// to /u/{slug} instead of /search?operated_by=….
			for _, sr := range semResp.Results {
				if sr != nil && sr.Bead != nil {
					s.populateOperatedBySlug(r.Context(), sr.Bead)
				}
			}
			views := semanticResultViews(semResp.Results, s.basePath, email)
			data["Results"] = views
			decorateResultsMeta(data, views)
			s.render(w, r, "search.html", data)
			return
		}

		// Semantic search is a strict ENHANCEMENT over lexical: any semantic
		// failure should quietly degrade to the always-available lexical path
		// rather than showing a hard "Search failed" error (ka-0000000389).
		// The daemon distinguishes two 5xx shapes:
		//   - 503 → embedder disabled (BEADS_EMBED_BACKEND=noop / no embedder
		//     configured). The expected e2e/dev posture.
		//   - 500 → embedder IS configured but its call failed — e.g. the
		//     in-cluster LiteLLM proxy 401s because BEADS_EMBED_API_KEY is empty
		//     (the new-prod missed-a-spot bug). Previously this skipped the
		//     fallback and rendered a red error even though lexical was healthy.
		// Both — and any other transient semantic error — fall back to lexical.
		isEmbedderDisabled := semanticDisabled(err)
		s.logger.Warn("semantic search failed; falling back to lexical",
			"query", query, "error", err, "embedder_disabled", isEmbedderDisabled)
		data["Mode"] = "lexical"
		data["Degraded"] = true
		if isEmbedderDisabled {
			data["DegradedReason"] = "Semantic search is disabled on the kbeads daemon (BEADS_EMBED_BACKEND=noop). Falling back to lexical full-text search."
		} else {
			data["DegradedReason"] = "Semantic search is temporarily unavailable. Falling back to lexical full-text search."
		}
		s.lexicalSearch(w, r, query, statusFilter, typeFilter, labelsFilter, operatedBy, operatedByIsNull, data)
		return
	}

	// Operator-only filter (no q): lexical-only path.
	data["Mode"] = "lexical"
	s.lexicalSearch(w, r, query, statusFilter, typeFilter, labelsFilter, operatedBy, operatedByIsNull, data)
}

// semanticDisabled reports whether a semantic search failed because the
// daemon has no embedder (503, BEADS_EMBED_BACKEND=noop), not because its call
// failed: the two fallbacks to lexical say different things.
func semanticDisabled(err error) bool {
	var apiErr *beadsapi.APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusServiceUnavailable
}

func (s *Server) lexicalSearch(w http.ResponseWriter, r *http.Request, query string, statusFilter, typeFilter, labelsFilter []string, operatedBy string, operatedByIsNull bool, data map[string]any) {
	q := beadsapi.ListBeadsQuery{
		Search:   query,
		Statuses: statusFilter,
		Types:    typeFilter,
		Labels:   labelsFilter,
		Limit:    20,
	}
	// Daemon migration 024 dropped the operated_by TEXT column; the
	// IS-NULL "unowned" filter has no successor (see ka-0000003586). The
	// caller still tracks operatedByIsNull for chip rendering, but it no
	// longer feeds into the daemon query.
	_ = operatedByIsNull
	if operatedBy != "" {
		q.OperatedByAliases = []string{operatedBy}
	}
	res, err := s.daemon.ListBeadsFiltered(r.Context(), q)
	if err != nil {
		s.logger.Error("lexical search failed", "query", query, "error", err)
		// Both semantic AND lexical failed — no healthy path left, so surface a
		// hard error. Set Mode="error" so the template's error-banner branch
		// (which gates on Mode) actually renders the message; leaving Mode as
		// "lexical" here silently rendered an empty results table. (ka-0000000389)
		data["Mode"] = "error"
		data["Degraded"] = false
		data["ErrorMessage"] = "Lexical search fallback failed: " + err.Error()
		s.render(w, r, "search.html", data)
		return
	}
	email, _ := data["ViewerEmail"].(string)
	// Per-user-homepage M2 §6 (ka-0000007787): populate slugs before
	// chip rendering so the search rows link to /u/{slug}.
	s.populateOperatedBySlugs(r.Context(), res.Beads)
	views := lexicalResultViews(res.Beads, s.basePath, email)
	data["Results"] = views
	decorateResultsMeta(data, views)
	s.render(w, r, "search.html", data)
}

// decorateResultsMeta populates results-derived template fields used to pack
// more signal into the results header: the oldest/newest/median age summary
// and per-type / per-status facet counts (ka-0000007172).
// The Operator column is always rendered (ka-0000003067: populate with the
// CreatedBy agent fallback rather than hiding when sparse).
func decorateResultsMeta(data map[string]any, views []searchResultView) {
	oldest, newest, median := computeAgeSummary(views, time.Now())
	data["AgeOldest"] = oldest
	data["AgeNewest"] = newest
	data["AgeMedian"] = median
	data["HasAgeSummary"] = oldest != ""
	types, statuses := computeFacets(views)
	data["TypeFacets"] = types
	data["StatusFacets"] = statuses
	projects := computeProjectFacets(views)
	data["ProjectFacets"] = projects
	ageBuckets := computeAgeBucketFacets(views, time.Now())
	data["AgeBucketFacets"] = ageBuckets
	data["HasFacets"] = len(types) > 0 || len(statuses) > 0 || len(projects) > 0 || len(ageBuckets) > 0
}

func semanticResultViews(results []*beadsapi.SemanticSearchResult, basePath, viewerEmail string) []searchResultView {
	now := time.Now()
	views := make([]searchResultView, 0, len(results))
	for _, r := range results {
		if r == nil || r.Bead == nil {
			continue
		}
		b := r.Bead
		chip := resolveOperatorChip(b.OperatedBy, b.OperatedByDisplay, b.OperatedByEmail, b.OperatedBySlug, b.CreatedBy, basePath, viewerEmail)
		views = append(views, searchResultView{
			ID:                      b.ID,
			Title:                   b.Title,
			ShortTitle:              b.ShortTitle,
			Heraldry:                bearerArms(b),
			Type:                    b.Type,
			Status:                  b.Status,
			Priority:                b.Priority,
			Score:                   strconv.FormatFloat(r.Score, 'f', 3, 64),
			Snippet:                 snippet(b.Description, 180),
			Age:                     fmtTimeAgo(b.UpdatedAt, now),
			UpdatedAt:               b.UpdatedAt,
			Labels:                  b.Labels,
			OperatorChipHTML:        renderOperatorChip(chip),
			OperatorChipCompactHTML: renderOperatorChipCompact(chip),
		})
	}
	return views
}

func lexicalResultViews(beads []*beadsapi.BeadDetail, basePath, viewerEmail string) []searchResultView {
	now := time.Now()
	views := make([]searchResultView, 0, len(beads))
	for _, b := range beads {
		if b == nil {
			continue
		}
		chip := resolveOperatorChip(b.OperatedBy, b.OperatedByDisplay, b.OperatedByEmail, b.OperatedBySlug, b.CreatedBy, basePath, viewerEmail)
		views = append(views, searchResultView{
			ID:                      b.ID,
			Title:                   b.Title,
			ShortTitle:              b.ShortTitle,
			Heraldry:                bearerArms(b),
			Type:                    b.Type,
			Status:                  b.Status,
			Priority:                b.Priority,
			Snippet:                 snippet(b.Description, 180),
			Age:                     fmtTimeAgo(b.UpdatedAt, now),
			UpdatedAt:               b.UpdatedAt,
			Labels:                  b.Labels,
			OperatorChipHTML:        renderOperatorChip(chip),
			OperatorChipCompactHTML: renderOperatorChipCompact(chip),
		})
	}
	return views
}
