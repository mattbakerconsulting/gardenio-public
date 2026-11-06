package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// maxRatingRationaleLen caps the rationale field on a rating. Mirrors decision
// response length — long enough for a paragraph, short enough to keep the bead
// field skinny.
const maxRatingRationaleLen = 10_000

// ratingsListLimit caps the number of rating beads pulled from the daemon for
// /ratings. Truncation surfaces with a banner.
const ratingsListLimit = 200

// ratingDepType is the dependency-edge type wired from a rating bead to the
// rated bead. Walking incoming `rates` edges on a rated bead yields all of
// its ratings.
const ratingDepType = "rates"

// slackSourcePrefix matches the Slack-fold source variants on a rating bead's
// `source` field (slack-stop, slack-manual, slack-feedback-only, slack-backfill,
// and any -backfill-* infixes used by the migration tool).
const slackSourcePrefix = "slack"

// ratingAxes is the canonical set of optional sub-score axes. Order is
// rendering order in the widget and detail card.
var ratingAxes = []string{"correctness", "scope", "efficiency", "communication"}

// ratingAxis is one (name, score) sub-score on a rating.
type ratingAxis struct {
	Name  string
	Score int // 1..5
	Stars string
}

// ratingView is the per-type detail view model for a `rating` bead.
type ratingView struct {
	BeadID           string
	RatedBeadID      string
	RatedTitle       string
	Score            int
	Stars            string
	ShowStars        bool // false for feedback-only rows (score=0)
	AxisRows         []ratingAxis
	Tags             []string
	Rater            string
	Source           string // "web", "slack-stop", etc; empty == legacy/unset
	SourceLabel      string // pretty form for badges ("slack", "web", "slack-feedback-only")
	IsSlack          bool   // convenience: source has "slack" prefix
	ScoreNative      *int   // original-scale value (e.g. -1/0/+1 for slack); nil when unset
	ScoreNativeLabel string // pre-formatted "+1" / "0" / "−1"; empty when ScoreNative nil
	Feedback         string // free-text feedback snippet (modal capture)
	FeedbackHTML     template.HTML
	RationaleHTML    template.HTML
	CreatedAgo       string
	CreatedAt        time.Time

	// Detail-page sub-sections — only populated for the focused rating
	// detail page (server.go:injectTypeView), not the per-bead widget.
	Siblings    []ratingView      // other ratings on the same rated bead
	RaterRecent []ratingView      // this rater's other recent ratings
	References  []ratingReference // beads that reference this rating (non-`rates` edges + mentions)
}

// ratingReference is one inbound reference to a rating bead — either a
// non-`rates` dependency edge or a text mention. SourceField carries the
// origin ("description", "comment:42", or empty for dep-edges) so the
// template can show provenance.
type ratingReference struct {
	BeadID      string
	Title       string
	Type        string
	Status      string
	Kind        string // "dep" | "mention"
	SourceField string // "description" / "comment:<id>" for mentions; empty for deps
	DepType     string // dep edge type for "dep" kind
}

// ratingAggregate summarizes the rating distribution for one bead.
type ratingAggregate struct {
	Mean      float64
	MeanLabel string // "—" when empty, "4.2" otherwise
	Count     int
	Histogram [5]int // index 0 = 1★, index 4 = 5★

	// Native-scale aggregate over the slack −1/0/+1 dimension. NativeCount==0
	// when no row carried score_native; readers should suppress the line.
	NativeMean      float64
	NativeMeanLabel string // signed "+0.6" / "−0.4" / "0.0"
	NativeCount     int
}

// ratingWidgetView is the per-bead "rate this" widget partial view model.
type ratingWidgetView struct {
	BeadID         string
	BasePath       string
	MyRating       *ratingView
	Aggregate      ratingAggregate
	Recent         []ratingView // up to 5 most-recent ratings
	CanRate        bool
	RaterEmail     string
	ScoreDraft     int
	RationaleDraft string
	Error          string
}

// ratingRowView is one row on the /ratings list page.
type ratingRowView struct {
	BeadID           string
	RatedBeadID      string
	RatedTitle       string
	Score            int
	Stars            string // pre-rendered "★★★☆☆" for the score
	ShowStars        bool   // false when score=0 (feedback-only rows)
	Rater            string
	Source           string // "web", "slack-stop", etc; empty for legacy rows
	IsSlack          bool   // source has "slack" prefix
	ScoreNative      *int   // nil when unset
	ScoreNativeLabel string // pre-formatted "+1" / "0" / "−1"; empty when nil
	Feedback         string // slack feedback snippet
	Snippet          string
	Ago              string
}

// renderStars returns a 5-character "★★★☆☆"-style label for an integer score
// in [0,5]. Out-of-range scores degrade to all-empty stars.
func renderStars(score int) string {
	if score < 0 {
		score = 0
	}
	if score > 5 {
		score = 5
	}
	return strings.Repeat("★", score) + strings.Repeat("☆", 5-score)
}

// isSlackSource returns true when source is one of the slack-* variants
// (slack-stop, slack-manual, slack-feedback-only, slack-backfill[-*]).
func isSlackSource(source string) bool {
	return strings.HasPrefix(source, slackSourcePrefix)
}

// formatNativeScore renders a signed −1/0/+1 native score with a typographic
// minus sign for negative values and an explicit plus for positive ones, so
// "−1" / "0" / "+1" reads cleanly inline next to stars.
func formatNativeScore(n int) string {
	switch {
	case n > 0:
		return "+" + strconv.Itoa(n)
	case n < 0:
		return "−" + strconv.Itoa(-n)
	default:
		return "0"
	}
}

// formatNativeMean renders a signed mean over the native scale (e.g. "+0.6").
func formatNativeMean(mean float64) string {
	s := strconv.FormatFloat(mean, 'f', 1, 64)
	switch {
	case mean > 0:
		return "+" + s
	case mean < 0:
		// strconv prints a leading "-" already; swap for typographic minus.
		return "−" + strings.TrimPrefix(s, "-")
	default:
		return s
	}
}

// readScoreNative extracts the optional score_native field from a rating
// bead, returning (nil, "") when the field is unset or unparseable.
func readScoreNative(fields map[string]string) (*int, string) {
	raw, ok := fields["score_native"]
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, ""
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return nil, ""
	}
	return &n, formatNativeScore(n)
}

// handleRatings renders /ratings — a list of rating beads sorted by created_at
// descending. Optional ?project=, ?type=, ?rater=, ?source= filters narrow
// the result. ?source=slack matches any "slack-*" provenance variant.
func (s *Server) handleRatings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	projectFilter := strings.TrimSpace(q.Get("project"))
	typeFilter := strings.TrimSpace(q.Get("type"))
	raterFilter := strings.TrimSpace(q.Get("rater"))
	sourceFilter := strings.TrimSpace(q.Get("source"))

	data := map[string]any{
		"ProjectFilter": projectFilter,
		"TypeFilter":    typeFilter,
		"RaterFilter":   raterFilter,
		"SourceFilter":  sourceFilter,
		"Ratings":       []ratingRowView{},
		"Limit":         ratingsListLimit,
		"BasePath":      s.basePath,
		// Source-filter strip renders through the canonical filter_strip partial
		// (ka-0000006647 / audit F7) — the bespoke `.rating-source-tabs` idiom is
		// retired so /ratings' source toggles read as the same component as every
		// other page-level filter strip.
		"SourceChips": buildRatingSourceChips(s.basePath, projectFilter, sourceFilter),
	}

	listQ := beadsapi.ListBeadsQuery{
		Types: []string{"rating"},
		Sort:  "-created_at",
		Limit: ratingsListLimit,
	}
	if projectFilter != "" {
		listQ.Labels = []string{"project:" + projectFilter}
	}
	res, err := s.daemon.ListBeadsFiltered(r.Context(), listQ)
	if err != nil {
		s.logger.Error("listing ratings", "error", err)
		data["Error"] = "Failed to load ratings."
		s.render(w, r, "ratings.html", data)
		return
	}

	// Pre-collect the distinct rated_bead_ids of the rows that survive the
	// rater/source filters, then hydrate them in ONE BatchGetBeads call.
	// Resolving the rated-bead title/type per row with an uncached GetBead
	// turned /ratings into a 200-RTT chain (ratingsListLimit rows) — the same
	// per-row N+1 fixed on /gardeners in MR !3442 (perf audit ka-0000002433).
	// A handful of beads typically draw many ratings, so the batch both
	// removes the serial chain and dedups repeated rated ids.
	ratedIDs := make([]string, 0, len(res.Beads))
	seenRated := make(map[string]struct{}, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		if raterFilter != "" && b.Fields["rater"] != raterFilter {
			continue
		}
		if !matchesSourceFilter(b.Fields["source"], sourceFilter) {
			continue
		}
		rated := b.Fields["rated_bead_id"]
		if rated == "" {
			continue
		}
		if _, dup := seenRated[rated]; dup {
			continue
		}
		seenRated[rated] = struct{}{}
		ratedIDs = append(ratedIDs, rated)
	}
	ratedByID := map[string]*beadsapi.BeadDetail{}
	if len(ratedIDs) > 0 {
		hydrated, err := s.daemon.BatchGetBeads(r.Context(), ratedIDs)
		if err != nil {
			// Degrade gracefully — rows render without rated title/type, and
			// a ?type= filter then drops rows it cannot classify (same
			// conservative behavior as a per-row lookup miss).
			s.logger.Warn("ratings: batch get rated beads", "error", err)
		} else {
			ratedByID = hydrated
		}
	}

	now := time.Now()
	rows := make([]ratingRowView, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		rated := b.Fields["rated_bead_id"]
		rater := b.Fields["rater"]
		source := b.Fields["source"]
		if raterFilter != "" && rater != raterFilter {
			continue
		}
		if !matchesSourceFilter(source, sourceFilter) {
			continue
		}
		ratedTitle := ""
		ratedType := ""
		if rated != "" {
			if rb := ratedByID[rated]; rb != nil {
				ratedTitle = rb.Title
				ratedType = rb.Type
			}
		}
		if typeFilter != "" && ratedType != typeFilter {
			continue
		}
		score, _ := strconv.Atoi(b.Fields["score"])
		nativePtr, nativeLabel := readScoreNative(b.Fields)
		feedback := b.Fields["feedback"]
		desc := b.Description
		if desc == "" {
			desc = feedback
		}
		rows = append(rows, ratingRowView{
			BeadID:           b.ID,
			RatedBeadID:      rated,
			RatedTitle:       ratedTitle,
			Score:            score,
			Stars:            renderStars(score),
			ShowStars:        score > 0,
			Rater:            rater,
			Source:           source,
			IsSlack:          isSlackSource(source),
			ScoreNative:      nativePtr,
			ScoreNativeLabel: nativeLabel,
			Feedback:         feedback,
			Snippet:          snippet(desc, 240),
			Ago:              fmtTimeAgo(b.CreatedAt, now),
		})
	}

	data["Ratings"] = rows
	data["Truncated"] = res.Total > len(res.Beads)
	s.render(w, r, "ratings.html", data)
}

// buildRatingSourceChips builds the /ratings source-filter chips (All / Web /
// Slack) as canonical filterChips for the shared filter_strip partial
// (ka-0000006647 / audit F7). Each chip's Href preserves the active ?project=
// scope so switching source never drops the project filter, matching the
// pre-consolidation `.rating-source-tabs` links.
func buildRatingSourceChips(basePath, projectFilter, sourceFilter string) []filterChip {
	href := func(source string) string {
		v := url.Values{}
		if source != "" {
			v.Set("source", source)
		}
		if projectFilter != "" {
			v.Set("project", projectFilter)
		}
		u := basePath + "/ratings"
		if enc := v.Encode(); enc != "" {
			u += "?" + enc
		}
		return u
	}
	return []filterChip{
		{Label: "All", Value: "", Href: href(""), Active: sourceFilter == ""},
		{Label: "Web", Value: "web", Href: href("web"), Active: sourceFilter == "web"},
		{Label: "Slack", Value: "slack", Href: href("slack"), Active: sourceFilter == "slack"},
	}
}

// matchesSourceFilter applies the ?source= post-list filter. Empty filter
// matches every row; "slack" matches any slack-* variant; otherwise exact.
// Unset row source defaults to "web" semantically — beads-viewer-authored
// rows pre-dating the source field still appear under ?source=web.
func matchesSourceFilter(rowSource, filter string) bool {
	if filter == "" {
		return true
	}
	if filter == "slack" {
		return isSlackSource(rowSource)
	}
	if filter == "web" {
		return rowSource == "" || rowSource == "web"
	}
	return rowSource == filter
}

// handleRatingWidget renders the per-bead rating widget partial.
func (s *Server) handleRatingWidget(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		if beadMissing(err) {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead for rating widget", "id", id, "error", err)
		http.Error(w, "Failed to load bead", http.StatusInternalServerError)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	view := s.buildRatingWidget(r.Context(), bead, email, 0, "", "")
	s.renderPartial(w, r, "templates/partials/rating_widget.html", "rating_widget", map[string]any{
		"Widget": view,
	})
}

// handleSubmitRating creates-or-updates the calling user's rating for the bead
// at /b/{id}/rating. Identity is server-set from the OIDC email claim — the
// form must not be allowed to override it.
func (s *Server) handleSubmitRating(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		// Defense-in-depth: the OIDC middleware should already block this.
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}

	scoreRaw := strings.TrimSpace(r.PostForm.Get("score"))
	score, err := strconv.Atoi(scoreRaw)
	if err != nil || score < 1 || score > 5 {
		s.renderRatingError(w, r, id, email, score, r.PostForm.Get("rationale"), "Pick a score between 1 and 5.", http.StatusBadRequest)
		return
	}
	rationale := strings.TrimSpace(r.PostForm.Get("rationale"))
	if len(rationale) > maxRatingRationaleLen {
		s.renderRatingError(w, r, id, email, score, rationale, "Rationale is too long (max 10000 characters).", http.StatusRequestEntityTooLarge)
		return
	}
	axes := parseAxesForm(r.PostForm)
	tags := parseRatingTags(r.PostForm.Get("tags"))

	rated, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead for rating submit", "id", id, "error", err)
		s.renderRatingError(w, r, id, email, score, rationale, "Failed to load bead.", http.StatusBadGateway)
		return
	}
	if rated.Type == "rating" {
		s.renderRatingError(w, r, id, email, score, rationale, "Cannot rate a rating bead.", http.StatusBadRequest)
		s.logger.Info("rating.reject", "bead", id, "rater", email, "reason", "rate_of_rating")
		return
	}
	if isSelfRating(rated, email) {
		s.renderRatingError(w, r, id, email, score, rationale, "Cannot rate your own work.", http.StatusForbidden)
		s.logger.Info("rating.reject", "bead", id, "rater", email, "reason", "self_rate")
		return
	}

	existing, lookupErr := s.findExistingRating(r.Context(), id, email)
	if lookupErr != nil {
		s.logger.Error("looking up existing rating", "bead", id, "rater", email, "error", lookupErr)
		s.renderRatingError(w, r, id, email, score, rationale, "Failed to look up existing rating.", http.StatusBadGateway)
		return
	}

	fieldsMap := buildRatingFieldsMap(id, email, score, axes, tags)

	title := buildRatingTitle(rated, score)
	isUpdate := existing != nil

	if isUpdate {
		desc := rationale
		req := beadsapi.UpdateBeadRequest{
			Title:       &title,
			Description: &desc,
		}
		if err := s.daemon.UpdateBead(r.Context(), existing.ID, req); err != nil {
			s.logger.Error("updating rating", "bead", existing.ID, "rater", email, "error", err)
			s.renderRatingError(w, r, id, email, score, rationale, "Failed to update rating.", http.StatusBadGateway)
			return
		}
		if err := s.daemon.UpdateBeadFieldsTyped(r.Context(), existing.ID, fieldsMap); err != nil {
			s.logger.Error("updating rating fields", "bead", existing.ID, "rater", email, "error", err)
			s.renderRatingError(w, r, id, email, score, rationale, "Failed to update rating.", http.StatusBadGateway)
			return
		}
	} else {
		fieldsJSON, err := json.Marshal(fieldsMap)
		if err != nil {
			s.logger.Error("encoding rating fields", "bead", id, "error", err)
			s.renderRatingError(w, r, id, email, score, rationale, "Failed to encode rating.", http.StatusInternalServerError)
			return
		}
		project := projectFromLabels(rated.Labels)
		var labels []string
		if project != "" {
			labels = []string{"project:" + project}
		}
		req := beadsapi.CreateBeadRequest{
			Title:       title,
			Type:        "rating",
			Description: rationale,
			Labels:      labels,
			CreatedBy:   email,
			Fields:      fieldsJSON,
		}
		newID, err := s.daemon.CreateBead(r.Context(), req)
		if err != nil {
			s.logger.Error("creating rating", "bead", id, "rater", email, "error", err)
			s.renderRatingError(w, r, id, email, score, rationale, "Failed to create rating.", http.StatusBadGateway)
			return
		}
		if err := s.daemon.AddDependency(r.Context(), newID, id, ratingDepType, email); err != nil {
			s.logger.Error("adding rating dependency", "rating", newID, "rated", id, "error", err)
			// Soft-fail: the rating exists; the missing edge means it won't
			// show up in the rated-bead's incoming list, which is repairable
			// with a re-rate or a daemon-side reconcile. Surface a warning so
			// the rater sees something happened, but render the partial in
			// success-state so they don't double-submit.
		}
	}

	s.logger.Info("rating.submit", "bead", id, "rater", email, "score", score, "axes_count", len(axes), "update", isUpdate)

	// Re-fetch and render the widget so the freshly-submitted rating shows
	// up in the aggregate and "your rating" sections.
	updated, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		updated = rated
	}
	view := s.buildRatingWidget(r.Context(), updated, email, 0, "", "")
	s.renderPartial(w, r, "templates/partials/rating_widget.html", "rating_widget", map[string]any{
		"Widget": view,
	})
}

// renderRatingError re-renders the widget partial with an inline error,
// preserving the operator's draft so a transient failure doesn't lose work.
func (s *Server) renderRatingError(w http.ResponseWriter, r *http.Request, id, email string, score int, rationale, msg string, status int) {
	if respondAction(w, r, id, actionFailure(msg, status)) {
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		http.Error(w, msg, status)
		return
	}
	view := s.buildRatingWidget(r.Context(), bead, email, score, rationale, msg)
	w.WriteHeader(status)
	s.renderPartial(w, r, "templates/partials/rating_widget.html", "rating_widget", map[string]any{
		"Widget": view,
	})
}

// buildRatingWidget assembles the per-bead widget view: existing-rating lookup,
// aggregate, recent samples, and authorization gates.
func (s *Server) buildRatingWidget(ctx context.Context, bead *beadsapi.BeadDetail, email string, scoreDraft int, rationaleDraft, errMsg string) ratingWidgetView {
	view := ratingWidgetView{
		BeadID:         bead.ID,
		BasePath:       s.basePath,
		RaterEmail:     email,
		ScoreDraft:     scoreDraft,
		RationaleDraft: rationaleDraft,
		Error:          errMsg,
	}
	view.CanRate = email != "" && bead.Type != "rating" && !isSelfRating(bead, email)

	ratings, err := s.collectRatings(ctx, bead.ID)
	if err != nil {
		s.logger.Warn("collecting ratings", "bead", bead.ID, "error", err)
		return view
	}
	view.Aggregate = aggregateRatings(ratings)
	now := time.Now()
	views := make([]ratingView, 0, len(ratings))
	for _, rb := range ratings {
		rv := buildRatingView(rb, "", s.renderBeadMarkdown, now)
		if rv == nil {
			continue
		}
		if email != "" && rb.Fields["rater"] == email {
			my := *rv
			view.MyRating = &my
		}
		views = append(views, *rv)
	}
	sort.SliceStable(views, func(i, j int) bool {
		return views[i].CreatedAt.After(views[j].CreatedAt)
	})
	if len(views) > 5 {
		views = views[:5]
	}
	view.Recent = views
	return view
}

// collectRatings walks incoming `rates` edges on the rated bead and fetches
// the rating beads. Returns an empty slice on success when there are no
// ratings yet.
func (s *Server) collectRatings(ctx context.Context, ratedBeadID string) ([]*beadsapi.BeadDetail, error) {
	revDeps, err := s.daemon.GetReverseDependencies(ctx, ratedBeadID)
	if err != nil {
		return nil, err
	}
	out := make([]*beadsapi.BeadDetail, 0, len(revDeps))
	for _, dep := range revDeps {
		if dep.Type != ratingDepType {
			continue
		}
		rb, err := s.daemon.GetBead(ctx, dep.BeadID)
		if err != nil {
			s.logger.Debug("fetching rating bead", "id", dep.BeadID, "error", err)
			continue
		}
		if rb == nil || rb.Type != "rating" {
			continue
		}
		out = append(out, rb)
	}
	return out, nil
}

// findExistingRating returns the calling rater's existing rating bead for the
// given rated bead, or nil when there is none. Uses the reverse-deps walk
// instead of a daemon-side FieldEquals query so this works against the
// existing kbeads HTTP API (no T4 dependency).
func (s *Server) findExistingRating(ctx context.Context, ratedBeadID, email string) (*beadsapi.BeadDetail, error) {
	ratings, err := s.collectRatings(ctx, ratedBeadID)
	if err != nil {
		return nil, err
	}
	for _, rb := range ratings {
		if rb.Fields["rater"] == email {
			return rb, nil
		}
	}
	return nil, nil
}

// renderBeadMarkdown converts rating-rationale markdown to HTML with the same
// goldmark + linkify pipeline used elsewhere on the page.
func (s *Server) renderBeadMarkdown(text string) template.HTML {
	if text == "" {
		return ""
	}
	var buf bytes.Buffer
	if err := s.md.Convert([]byte(text), &buf); err != nil {
		return LinkifyText(text, s.basePath)
	}
	return LinkifyHTML(buf.String(), s.basePath)
}

// buildRatingView is the pure view-model builder for a rating bead.
// Rated-bead title is supplied by the caller (one extra GetBead per row, bounded
// by ratingsListLimit) — pass empty when not available so the partial falls
// back to the rated-bead id.
func buildRatingView(bead *beadsapi.BeadDetail, ratedTitle string, renderMarkdown func(string) template.HTML, now time.Time) *ratingView {
	if bead == nil {
		return nil
	}
	score, _ := strconv.Atoi(bead.Fields["score"])
	source := bead.Fields["source"]
	nativePtr, nativeLabel := readScoreNative(bead.Fields)
	feedback := bead.Fields["feedback"]
	v := &ratingView{
		BeadID:           bead.ID,
		RatedBeadID:      bead.Fields["rated_bead_id"],
		RatedTitle:       ratedTitle,
		Score:            score,
		Stars:            renderStars(score),
		ShowStars:        score > 0,
		Rater:            bead.Fields["rater"],
		Source:           source,
		SourceLabel:      ratingSourceLabel(source),
		IsSlack:          isSlackSource(source),
		ScoreNative:      nativePtr,
		ScoreNativeLabel: nativeLabel,
		Feedback:         feedback,
		Tags:             parseRatingTags(bead.Fields["tags"]),
		AxisRows:         parseAxesField(bead.Fields["axes"]),
		CreatedAgo:       fmtTimeAgo(bead.CreatedAt, now),
		CreatedAt:        bead.CreatedAt,
	}
	if bead.Description != "" && renderMarkdown != nil {
		v.RationaleHTML = renderMarkdown(bead.Description)
	}
	if feedback != "" && renderMarkdown != nil {
		v.FeedbackHTML = renderMarkdown(feedback)
	}
	return v
}

// ratingSourceLabel returns a human-friendly label for the rating's source,
// suitable for a badge. Empty source falls back to "web" (the historical
// beads-viewer-authored default — rows pre-dating the source field).
func ratingSourceLabel(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return "web"
	}
	return source
}

// parseAxesForm extracts axes from a POST form, accepting `axes_<name>` keys.
func parseAxesForm(form interface{ Get(string) string }) []ratingAxis {
	out := make([]ratingAxis, 0, len(ratingAxes))
	for _, name := range ratingAxes {
		raw := strings.TrimSpace(form.Get("axes_" + name))
		if raw == "" {
			continue
		}
		v, err := strconv.Atoi(raw)
		if err != nil || v < 1 || v > 5 {
			continue
		}
		out = append(out, ratingAxis{Name: name, Score: v, Stars: renderStars(v)})
	}
	return out
}

// parseAxesField parses the JSON `axes` field on a rating bead.
func parseAxesField(raw string) []ratingAxis {
	if raw == "" {
		return nil
	}
	var m map[string]int
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return nil
	}
	out := make([]ratingAxis, 0, len(m))
	for _, name := range ratingAxes {
		if v, ok := m[name]; ok && v >= 1 && v <= 5 {
			out = append(out, ratingAxis{Name: name, Score: v, Stars: renderStars(v)})
		}
	}
	return out
}

// parseRatingTags accepts either a JSON array (the typed kbeads tags storage
// for `string[]`) or a comma-separated fallback string. Whitespace-only entries
// are dropped.
func parseRatingTags(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	if strings.HasPrefix(raw, "[") {
		var arr []string
		if err := json.Unmarshal([]byte(raw), &arr); err == nil {
			out := make([]string, 0, len(arr))
			for _, p := range arr {
				if p = strings.TrimSpace(p); p != "" {
					out = append(out, p)
				}
			}
			return out
		}
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// buildRatingFieldsMap encodes the rating's structured fields with the JSON
// types the type:rating schema requires: integer score (1..5), JSON-object
// axes, and string-array tags. Returned as a map[string]any so the same value
// drives both CreateBead (marshalled to JSON) and UpdateBeadFieldsTyped (typed
// PATCH on re-rate).
func buildRatingFieldsMap(ratedBeadID, rater string, score int, axes []ratingAxis, tags []string) map[string]any {
	m := map[string]any{
		"rated_bead_id": ratedBeadID,
		"rater":         rater,
		"score":         score,
	}
	if len(axes) > 0 {
		axesMap := make(map[string]int, len(axes))
		for _, a := range axes {
			axesMap[a.Name] = a.Score
		}
		m["axes"] = axesMap
	}
	if len(tags) > 0 {
		m["tags"] = append([]string(nil), tags...)
	}
	return m
}

// buildRatingTitle returns the canonical title for a new or updated rating.
func buildRatingTitle(rated *beadsapi.BeadDetail, score int) string {
	hint := rated.ID
	if t := strings.TrimSpace(rated.Title); t != "" {
		if len(t) > 60 {
			t = t[:60] + "…"
		}
		hint = t
	}
	return fmt.Sprintf("rating: %s · %d★", hint, score)
}

// isSelfRating returns true when the rater email matches the assignee or the
// agent-run actor of the rated bead. Protects against self-rating without
// requiring a separate identity service.
func isSelfRating(bead *beadsapi.BeadDetail, email string) bool {
	if email == "" || bead == nil {
		return false
	}
	if bead.Assignee != "" && strings.EqualFold(bead.Assignee, email) {
		return true
	}
	if a := bead.Fields["actor"]; a != "" && strings.EqualFold(a, email) {
		return true
	}
	return false
}

// aggregateRatings computes mean / count / histogram across rating beads.
// Star (1..5) and native (-1/0/+1) scales are tallied separately so a row
// with score=0 + score_native=0 (slack feedback-only) contributes to the
// native aggregate without skewing the star histogram.
func aggregateRatings(ratings []*beadsapi.BeadDetail) ratingAggregate {
	agg := ratingAggregate{MeanLabel: "—"}
	var sum int
	var nativeSum int
	for _, rb := range ratings {
		s, err := strconv.Atoi(rb.Fields["score"])
		if err == nil && s >= 1 && s <= 5 {
			agg.Histogram[s-1]++
			sum += s
			agg.Count++
		}
		if nativePtr, _ := readScoreNative(rb.Fields); nativePtr != nil {
			nativeSum += *nativePtr
			agg.NativeCount++
		}
	}
	if agg.Count > 0 {
		agg.Mean = float64(sum) / float64(agg.Count)
		agg.MeanLabel = strconv.FormatFloat(agg.Mean, 'f', 1, 64)
	}
	if agg.NativeCount > 0 {
		agg.NativeMean = float64(nativeSum) / float64(agg.NativeCount)
		agg.NativeMeanLabel = formatNativeMean(agg.NativeMean)
	}
	return agg
}
