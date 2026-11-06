package main

import (
	"bytes"
	"context"
	"errors"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/gardenmd"
	"gardenio/controller/internal/oidcauth"
)

// maxDecisionResponseLen caps the optional free-text response posted with a
// resolve. Mirrors comment limits — long enough for a paragraph of context,
// short enough to keep the bead field skinny.
const maxDecisionResponseLen = 10_000

// maxDecisionDismissReasonLen caps the optional reason posted with a dismiss.
// Same ceiling as the resolve response — these fields render the same way in
// the bead detail view.
const maxDecisionDismissReasonLen = 10_000

// decisionsListLimit caps the number of open decisions pulled from the daemon.
// Open decisions are an attention queue; if it ever blows past this cap the
// truncation banner surfaces it loudly.
const decisionsListLimit = 200

// decisionRowView is one card on the /decisions page.
type decisionRowView struct {
	ID          string
	Title       string
	Project     string
	Prompt      string
	RequestedBy string
	Operator    template.HTML // resolved operator chip (operated_by → display)
	// OperatorCompact is the initials-only operator chip for the dense
	// decisions list row (ka-0000000013). Empty when the chip kind has no
	// compact treatment; the template falls back to Operator.
	OperatorCompact template.HTML
	Options         []decisionOption
	Ago             string
	UpdatedAtRFC    string
	// Origin is the agent / task / plan strip (Slack thread gd-AREKDbHx70):
	// who filed the decision (by name, linked to the agent bead), the task it
	// blocks (by title) and the owning plan — so the operator can see what a
	// decision is about without opening it.
	Origin requestOrigin
	// PromptHTML is the FULL prompt rendered as markdown and Context the
	// decision's context block (the context field, else the description),
	// for the inline "details" disclosure on the row, open by default
	// (gd-PNtilz1bgs); Prompt above stays the one-line snippet. HasDetails is
	// true when the disclosure has anything beyond the visible snippet to show.
	PromptHTML template.HTML
	Context    decisionContextView
	HasDetails bool
	// Evidence is the decision's screenshot strip, under the context
	// (decisionEvidenceOf, gd-Pi7C83262N).
	Evidence template.HTML
	// created and operatedByID are the bead's, for the Queues panel's
	// attention order and its "asked of you" (queue_garden.go).
	created      time.Time
	operatedByID int64
}

// handleDecisions renders /decisions — a list of open decision beads sorted
// by created_at descending (newest first). Mirrors iOS DecisionsTab.
//
// By default the list is scoped to the decisions the viewer owns (the
// operator the decision is addressed to, `operated_by_id`). Passing
// ?all=1 (the "Show all" toggle) drops the scope and renders the full
// pending queue. An optional ?project=<name> filter narrows by project
// label in either mode, and is surfaced as a clickable chip row built from
// the projects that actually have an open decision (ka-0000004140).
//
// When the viewer's operator identity can't be resolved (anonymous, a
// sentinel alias, an unprovisioned operator, or a resolver/daemon error)
// there is no "mine" set to show, so the handler falls back to the full
// queue and flips CanScope=false so the template hides the toggle.
func (s *Server) handleDecisions(w http.ResponseWriter, r *http.Request) {
	projectFilter := strings.TrimSpace(r.URL.Query().Get("project"))
	showAll := decisionsShowAllRequested(r)

	// Resolve the viewer's operator id so we can scope to "mine". A zero id
	// (no resolver, anonymous, sentinel, unprovisioned, or error) means we
	// cannot scope — the template hides the toggle and we show the full queue.
	viewerOpID := s.resolveDecisionsViewerOperator(r)
	canScope := viewerOpID != 0

	data := map[string]any{
		"ProjectFilter": projectFilter,
		"Decisions":     []decisionRowView{},
		"ProjectFacets": []decisionProjectFacet{},
		"ShowAll":       showAll || !canScope,
		"CanScope":      canScope,
		"GardenHref":    s.gardenQueuesBanner(r, gardenQueueTarget(gardenhub.QueueDecisions)),
	}

	// The project predicate is applied CLIENT-SIDE (below) rather than as a
	// `project:<x>` label filter on the query, because the chip row needs the
	// unfiltered set to count every project's queue depth — a server-filtered
	// fetch can only ever see the selected project. Mirrors /escalations, which
	// filters project in-handler over the same 200-row cap. The trade-off is
	// disclosed by the existing truncation banner: past decisionsListLimit open
	// decisions a filtered view shows only the newest 200 overall.
	res, err := s.daemon.ListBeadsFiltered(r.Context(), openDecisionsQuery(viewerOpID, showAll))
	if err != nil {
		s.logger.Error("listing decisions", "error", err)
		data["Error"] = "Failed to load decisions."
		s.render(w, r, "decisions.html", data)
		return
	}
	if canScope && !showAll {
		res = s.withAddressedLandsDecisions(r.Context(), res, oidcauth.EmailFromContext(r.Context()))
	}

	// Facets come from the FULL fetched set (pre project filter) so every chip
	// keeps its count while one project is selected.
	// Chip URLs carry the RAW ?all= state, not the effective one: when the
	// viewer can't scope at all, "show all" is already implicit, so stamping
	// all=1 onto every chip would be inert query noise.
	data["ProjectFacets"] = buildDecisionProjectFacets(res.Beads, projectFilter, showAll, s.basePath+"/decisions")

	visible := res.Beads
	if projectFilter != "" {
		visible = make([]*beadsapi.BeadDetail, 0, len(res.Beads))
		for _, b := range res.Beads {
			if b == nil || projectFromLabels(b.Labels) != projectFilter {
				continue
			}
			visible = append(visible, b)
		}
	}

	rows := s.decisionRows(r.Context(), visible)

	data["Decisions"] = rows
	data["Truncated"] = res.Total > len(res.Beads)
	data["Limit"] = decisionsListLimit
	r = s.prewarmAgentLinksFromBeads(r, visible)
	s.render(w, r, "decisions.html", data)
}

// decisionRows builds the /decisions rows for visible, in order (nil beads
// skipped): the operator chip, the prompt snippet and options, the origin
// strip and the inline details. The Queues panel in /garden reads the same
// rows (queue_garden.go).
func (s *Server) decisionRows(ctx context.Context, visible []*beadsapi.BeadDetail) []decisionRowView {
	// Resolve the operator homepage slugs so each row's operator chip can emit
	// /u/{slug} links (mirrors /active and the other list surfaces).
	s.populateOperatedBySlugs(ctx, visible)
	viewerEmail := oidcauth.EmailFromContext(ctx)
	origins := s.resolveRequestOrigins(ctx, visible)
	evidence := s.decisionEvidenceAll(ctx, visible)

	now := time.Now()
	rows := make([]decisionRowView, 0, len(visible))
	for _, b := range visible {
		if b == nil {
			continue
		}
		decisionChip := resolveOperatorChip(b.OperatedBy, b.OperatedByDisplay, b.OperatedByEmail, b.OperatedBySlug, b.CreatedBy, s.basePath, viewerEmail)
		row := decisionRowView{
			ID:              b.ID,
			Title:           b.Title,
			Project:         projectFromLabels(b.Labels),
			Prompt:          snippet(b.Fields["prompt"], 240),
			RequestedBy:     b.Fields["requested_by"],
			Operator:        renderOperatorChip(decisionChip),
			OperatorCompact: renderOperatorChipCompact(decisionChip),
			Options:         parseDecisionOptions(b.Fields["options"]),
			Ago:             fmtTimeAgo(b.CreatedAt, now),
			UpdatedAtRFC:    rfcTime(b.UpdatedAt),
			Origin:          origins[b.ID],
			Evidence:        evidence[b.ID],
			created:         b.CreatedAt,
		}
		if b.OperatedByID != nil {
			row.operatedByID = *b.OperatedByID
		}
		if row.RequestedBy == "" {
			row.RequestedBy = b.CreatedBy
		}
		// Inline details: the full prompt when the snippet clipped it, plus
		// the context block (what the agent tried / why it is blocked). Both
		// render through the shared decision markdown path.
		prompt := strings.TrimSpace(b.Fields["prompt"])
		if prompt != "" && len(prompt) > len(row.Prompt) {
			row.PromptHTML = s.decisionMarkdown(prompt)
		}
		row.Context = s.decisionContextOf(b)
		row.HasDetails = row.PromptHTML != "" || row.Context.Present()
		rows = append(rows, row)
	}
	return rows
}

// openDecisionsQuery is the /decisions query: open decisions, newest first,
// capped at decisionsListLimit. Default scope: only the viewer's own
// decisions (operated_by_id). The ?all=1 toggle, or an unresolvable viewer
// (opID 0), drops the operator predicate.
func openDecisionsQuery(opID int64, all bool) beadsapi.ListBeadsQuery {
	q := beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"open"},
		Sort:     "-created_at",
		Limit:    decisionsListLimit,
	}
	if opID != 0 && !all {
		q.OperatedByID = opID
	}
	return q
}

// withAddressedLandsDecisions adds to a viewer-scoped ("mine") result the
// open Lands approval decisions that name the viewer as their approver. The
// Gate files those as its machine principal (lands_apply.go fileDecision), so
// the bead's operator is the machine and the OperatedByID scope matched no
// one: the approver never saw the application in their own queue. The
// approver is named in fields.operated_by as oidc:<email>. Best effort: a
// daemon error leaves the scoped result as it was.
func (s *Server) withAddressedLandsDecisions(ctx context.Context, res *beadsapi.ListBeadsResult, email string) *beadsapi.ListBeadsResult {
	alias := "oidc:" + strings.ToLower(strings.TrimSpace(email))
	if res == nil || alias == "oidc:" {
		return res
	}
	extra, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"decision"},
		Statuses: []string{"open"},
		Labels:   []string{landsIntakeLabel},
		Sort:     "-created_at",
		Limit:    decisionsListLimit,
	})
	if err != nil {
		s.logger.Warn("decisions: listing lands approval decisions", "error", err)
		return res
	}
	seen := make(map[string]bool, len(res.Beads))
	for _, b := range res.Beads {
		if b != nil {
			seen[b.ID] = true
		}
	}
	out := append([]*beadsapi.BeadDetail(nil), res.Beads...)
	added := 0
	for _, b := range extra.Beads {
		if b == nil || seen[b.ID] || !strings.EqualFold(strings.TrimSpace(b.Fields["operated_by"]), alias) {
			continue
		}
		seen[b.ID] = true
		out = append(out, b)
		added++
	}
	if added == 0 {
		return res
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return &beadsapi.ListBeadsResult{Beads: out, Total: res.Total + added}
}

// decisionsShowAllRequested reports whether the request opted out of the
// default viewer-owned scope via the ?all toggle. Any truthy-ish value
// (`1`, `true`, `yes`, or a bare `?all`) counts.
func decisionsShowAllRequested(r *http.Request) bool {
	v := strings.TrimSpace(r.URL.Query().Get("all"))
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on", "":
		// "" only counts when the key is actually present (a bare ?all).
		return r.URL.Query().Has("all")
	default:
		return false
	}
}

// resolveDecisionsViewerOperator returns the viewer's operators(id) FK for
// scoping the decisions list to "mine", or 0 when no scope is possible. It
// reuses the inbox operator-resolution primitive (resolver + sentinel-safety),
// collapsing every "cannot scope" outcome — nil resolver, anonymous viewer,
// sentinel alias, unprovisioned operator, transport error — to 0 so the caller
// has a single fall-back-to-all branch.
func (s *Server) resolveDecisionsViewerOperator(r *http.Request) int64 {
	email := oidcauth.EmailFromContext(r.Context())
	id, _, err := s.resolveInboxViewerOperator(r.Context(), email)
	if err != nil {
		s.logger.Warn("decisions: resolving viewer operator", "error", err)
		return 0
	}
	return id
}

// handleResolveDecision resolves an open decision bead from the viewer detail
// page. The form posts the chosen option_id and an optional free-text
// response; on success we re-render the type_decision partial so htmx swaps
// the card to its resolved state.
//
// Errors render inline on the same partial (preserving the operator's draft
// response) and ship a 4xx/5xx status so htmx can surface a non-OK signal to
// the user even though the swap target stays the decision card.
func (s *Server) handleResolveDecision(w http.ResponseWriter, r *http.Request) {
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
	optionID := strings.TrimSpace(r.PostForm.Get("option_id"))
	responseText := strings.TrimSpace(r.PostForm.Get("response_text"))

	if optionID == "" && responseText == "" {
		s.renderDecisionError(w, r, id, email, "", "", "Pick an option or write a response.", http.StatusBadRequest)
		return
	}
	if len(responseText) > maxDecisionResponseLen {
		s.renderDecisionError(w, r, id, email, optionID, responseText, "Response is too long (max 10000 characters).", http.StatusRequestEntityTooLarge)
		return
	}

	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead for resolve", "id", id, "error", err)
		http.Error(w, "Failed to load bead", http.StatusInternalServerError)
		return
	}
	if bead.Type != "decision" {
		http.Error(w, "Bead is not a decision", http.StatusBadRequest)
		return
	}
	if bead.Status != "open" {
		// Already-resolved race: re-render the partial (which now reads as
		// resolved) and surface a friendly inline message.
		s.renderDecisionError(w, r, id, email, optionID, responseText, "Decision is already resolved.", http.StatusConflict)
		return
	}

	if err := s.daemon.ResolveDecision(r.Context(), id, beadsapi.ResolveDecisionRequest{
		SelectedOption: optionID,
		ResponseText:   responseText,
		RespondedBy:    email,
	}); err != nil {
		s.logger.Error("resolving decision", "id", id, "author", email, "error", err)
		s.renderDecisionError(w, r, id, email, optionID, responseText, "Failed to resolve decision.", http.StatusBadGateway)
		return
	}

	// Re-fetch so the resolved fields the daemon merged in (`chosen`,
	// `response_text`, `responded_by`, `responded_at`) flow through to the
	// view model and the swap renders the resolved banner.
	updated, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.logger.Warn("re-fetching bead after resolve", "id", id, "error", err)
		updated = bead
	}
	s.renderDecisionPartial(w, r, updated, email, "", "", http.StatusOK)
}

// handleDismissDecision cancels an open decision bead from the viewer detail
// page or the /decisions list. The form posts an optional free-text reason; on
// success the decision is closed (status=closed, fields.canceled=true) and we
// re-render the type_decision partial so htmx swaps the card to its resolved
// (canceled) state.
//
// Errors render inline on the same partial (preserving the operator's draft
// reason) and ship a 4xx/5xx status so htmx can surface a non-OK signal.
func (s *Server) handleDismissDecision(w http.ResponseWriter, r *http.Request) {
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
	reason := strings.TrimSpace(r.PostForm.Get("reason"))
	if len(reason) > maxDecisionDismissReasonLen {
		s.renderDecisionError(w, r, id, email, "", reason, "Reason is too long (max 10000 characters).", http.StatusRequestEntityTooLarge)
		return
	}

	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead for dismiss", "id", id, "error", err)
		http.Error(w, "Failed to load bead", http.StatusInternalServerError)
		return
	}
	if bead.Type != "decision" {
		http.Error(w, "Bead is not a decision", http.StatusBadRequest)
		return
	}
	if bead.Status != "open" {
		// Already-resolved race: surface a friendly inline message.
		s.renderDecisionError(w, r, id, email, "", reason, "Decision is already resolved.", http.StatusConflict)
		return
	}

	if err := s.daemon.CancelDecision(r.Context(), id, reason, email); err != nil {
		s.logger.Error("dismissing decision", "id", id, "actor", email, "error", err)
		s.renderDecisionError(w, r, id, email, "", reason, "Failed to dismiss decision.", http.StatusBadGateway)
		return
	}

	// Re-fetch so the canceled fields the daemon merged in (`canceled`,
	// `chosen=dismissed`, `cancel_reason`, `canceled_by`) flow through to the
	// view model and the swap renders the resolved banner.
	updated, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		s.logger.Warn("re-fetching bead after dismiss", "id", id, "error", err)
		updated = bead
	}
	s.renderDecisionPartial(w, r, updated, email, "", "", http.StatusOK)
}

// renderDecisionError re-renders the decision partial with an inline error
// banner, preserving the operator's draft response so a transient failure
// (network, validation) doesn't lose what they were writing.
func (s *Server) renderDecisionError(w http.ResponseWriter, r *http.Request, id, email, optionID, draft, msg string, status int) {
	if respondAction(w, r, id, actionFailure(msg, status)) {
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		// If we can't even load the bead, fall back to a plain error response
		// so htmx leaves the existing DOM (and form contents) alone.
		http.Error(w, msg, status)
		return
	}
	_ = optionID // option_id is not echoed on the form — the error is on the textarea side
	s.renderDecisionPartial(w, r, bead, email, draft, msg, status)
}

// decisionMarkdown renders decision prompt/response text as bead-id-linkified
// HTML through the shared goldmark + Linkify pipeline. It is the single
// markdown path for both the bead-detail decision card (renderDecisionPartial)
// and the inline plan-cockpit decisions (planDecisions, task ka-0000003157) so
// the two renders never diverge. It is the hard-wrap goldmark (mdInline), so a
// single newline is a line break, as /garden's gardenmd renders the same text
// in the Mailbox and the Commons (gd-gKXlbPRw8i).
func (s *Server) decisionMarkdown(text string) template.HTML {
	var buf bytes.Buffer
	if err := s.mdInline.Convert([]byte(text), &buf); err != nil {
		return LinkifyText(text, s.basePath)
	}
	return LinkifyHTML(decisionPictures(buf.String(), s.basePath), s.basePath)
}

var (
	imgTagRe  = regexp.MustCompile(`<img\b[^>]*>`)
	imgAttrRe = regexp.MustCompile(`\s(src|alt)="([^"]*)"`)
)

// decisionPictures keeps a picture in decision text only when it loads from
// this site: through the viewer's /image proxy (what gb image upload prints,
// gardenmd.PicSrc, the /garden renderer's rule) or a relative path. Any other
// picture becomes a link to it, "[image: alt]", so a decision never fetches
// from a host its writer chose: a Gate applicant's tracking pixel would tell
// them the operator's IP and when they looked (gd-ZwnwonBxGS).
func decisionPictures(h, base string) string {
	if !strings.Contains(h, "<img") {
		return h
	}
	return imgTagRe.ReplaceAllStringFunc(h, func(tag string) string {
		attr := map[string]string{}
		for _, m := range imgAttrRe.FindAllStringSubmatch(tag, -1) {
			attr[m[1]] = m[2]
		}
		src := html.UnescapeString(attr["src"])
		if p := gardenmd.PicSrc(base, src); p != "" {
			return strings.Replace(tag, ` src="`+attr["src"]+`"`, ` src="`+template.HTMLEscapeString(p)+`" loading="lazy" referrerpolicy="no-referrer"`, 1)
		}
		if relativeURL(src) {
			return tag
		}
		label := "[image: " + attr["alt"] + "]"
		if src == "" {
			return label
		}
		return `<a href="` + attr["src"] + `" target="_blank" rel="noopener noreferrer">` + label + `</a>`
	})
}

// relativeURL is true for a path on this site: no scheme, no host, and
// nothing a browser would read as one ("//host", or "/\host").
func relativeURL(u string) bool {
	if u == "" || strings.HasPrefix(u, "//") || strings.ContainsAny(u, "\\\t\n\r") {
		return false
	}
	x, err := url.Parse(u)
	return err == nil && x.Scheme == "" && x.Host == ""
}

// renderDecisionPartial renders templates/partials/type_decision.html as a
// standalone fragment so htmx hx-swap=outerHTML can replace the decision card.
func (s *Server) renderDecisionPartial(w http.ResponseWriter, r *http.Request, bead *beadsapi.BeadDetail, email, draft, errMsg string, status int) {
	dv := buildDecisionView(bead, s.decisionMarkdown)
	if dv != nil {
		dv.CanResolve = !dv.Resolved && email != ""
		dv.ResponseDraft = draft
		dv.Error = errMsg
		dv.Origin = s.resolveRequestOrigin(r.Context(), bead)
		dv.DetailURL = s.basePath + "/decisions/" + url.PathEscape(bead.ID)
		dv.Evidence = s.decisionEvidenceOf(r.Context(), bead)
	}
	if status != http.StatusOK {
		w.WriteHeader(status)
	}
	s.renderPartial(w, r, "templates/partials/type_decision.html", "type_decision", map[string]any{
		"Decision": dv,
		"Bead":     bead,
	})
	_ = r
}
