package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"slices"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/oidcauth"
)

// /up is the GV phase-2 single-card focus surface. Read-only proto: data
// source is the viewer's top in_progress bead, falling back to the highest
// priority open bead globally when the viewer has no in_progress work.
// PRD §3.1 anatomy, §3.3 type-conditional zones, §4.3 footer actions.
// WebSocket live-updates and the inline chatbox are deferred to a follow-up
// bead.

// upDescLimit is the rune-budget for the description snippet rendered on
// the card. Wide enough for a short paragraph of context (PRD §3.1 "body")
// without pushing the suggested-action zone below the fold on a 1080p
// screen. Full markdown body remains reachable from the bead-show link.
const upDescLimit = 600

// upActivityHead is the cap on activity rows rendered in the "recent" rail.
// PRD §3.1 calls for ~3 events; we render slightly more so the rail still
// reads useful when half the events are tiny chip flips.
const upActivityHead = 5

// upMRSearchLimit caps the size of the opened-MR fetch we scan for
// per-bead linkage. A single page (100) easily covers steady-state load;
// the page exists for fresh authoring so we don't need exhaustive matching.
const upMRSearchLimit = 100

// upCardView is the per-card view-model rendered by templates/up.html.
type upCardView struct {
	// Header — operator-facing bead identity.
	ID               string
	Title            string
	TitleHTML        template.HTML
	Type             string
	Status           string
	Priority         int
	Glyph            string
	OperatorChipHTML template.HTML
	Project          string
	Ago              string
	UpdatedAtRFC     string

	// Body — markdown description (snipped) + per-type augments.
	DescriptionHTML template.HTML
	DescTruncated   bool
	BeadURL         string // /<id> for the deep link "Open bead" affordance

	// Dependencies + mentions — collapsed counts for the proto.
	BlockedByCount    int
	BlocksCount       int
	ParentID          string
	MentionedByCount  int
	MentionsCount     int

	// Activity rail (top-N events).
	RecentActivity []activityRow

	// Linked merge requests — opened MRs whose title or description names
	// this bead-id. Empty slice when GitLab isn't wired or none matched.
	LinkedMRs []upMRLink

	// Suggested-action zone (PRD §3.2.2 / §3.3). Read-only proto: just
	// renders the per-type default copy; no live action wiring yet.
	SuggestedAction upSuggestedAction

	// Footer + keymap (PRD §4). Footer drives the visible action bar; Keymap
	// JSON is the island up.js reads so the keyboard dispatch and the buttons
	// render from the same source and can never drift.
	Footer     upFooter
	KeymapJSON template.JS

	// DecisionOptions are the one-tap resolve chips for a decision bead (PRD
	// §3.3). Empty for every other type (and for decisions with no parsed
	// options — the primary then resolves via free-text response).
	DecisionOptions []decisionOption
	// IsDecision marks a decision card, whose body is its Context block
	// (decisionview.Context: the context field, else the description) in
	// place of the description (gd-PNtilz1bgs).
	IsDecision bool
	Context    decisionContextView
	// Evidence is the decision's screenshot strip, under its context
	// (gd-Pi7C83262N).
	Evidence template.HTML
}

// upSuggestedAction is the per-type placeholder zone. PRD §3.3 maps each
// bead type to a primary verb; the proto renders the copy and the help-
// text but does NOT wire the buttons to handlers yet — that lands with the
// WebSocket/HTMX phase.
type upSuggestedAction struct {
	Primary   string // e.g. "Bundle and close"
	Secondary string // e.g. "Skip"
	HelpText  string // one-liner under the primary action
}

// upMRLink is one merge-request row in the "Linked MRs" zone.
type upMRLink struct {
	IID         int
	Title       string
	WebURL      string
	State       string // opened / merged / closed
	Draft       bool
	MergeStatus string
}

// upRenderContext carries the lookup state needed to render a card. Built
// once per request so the data-source selection and the bead render share
// the same context.
type upRenderContext struct {
	ViewerEmail string
	Empty       bool   // true when no bead matched (render empty state)
	EmptyReason string // human-readable banner copy
	Bead        *beadsapi.BeadDetail
}

// handleUp renders the auto-picked card for the viewer.
//
// Selection (PRD §2.4 stack policy, simplified for the proto):
//
//  1. Top in_progress bead assigned to the viewer, sorted by priority.
//  2. Fallback: highest-priority open bead globally.
//  3. Empty-state when both queries return nothing.
func (s *Server) handleUp(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	rc := s.selectUpBead(r.Context(), email)
	s.renderUpCard(w, r, rc)
}

// handleUpBead renders /up/{id} — a deep-link entry to a specific bead.
// Same template, just bypasses the selection. Useful for testing the
// per-type renders without poking the daemon's state.
func (s *Server) handleUpBead(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	email := oidcauth.EmailFromContext(r.Context())
	rc := upRenderContext{ViewerEmail: email}
	if id == "" {
		rc.Empty = true
		rc.EmptyReason = "No bead ID."
		s.renderUpCard(w, r, rc)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead for /up", "id", id, "error", err)
		http.Error(w, "Failed to get bead", http.StatusInternalServerError)
		return
	}
	rc.Bead = bead
	s.renderUpCard(w, r, rc)
}

// upNeedsYouEscalation is the escalation /up leads with: the highest-priority,
// then oldest, open escalation that is the viewer's own (escalationOwnerRow)
// and held by one of their lanes, or unstamped and owner-audience for the
// operator lane. Nil when there is none, the viewer holds no lane, or the
// daemon errors (logged; /up falls through to beads).
func (s *Server) upNeedsYouEscalation(ctx context.Context, email string) *beadsapi.BeadDetail {
	lanes := s.viewerLaneRefs(ctx, email)
	if len(lanes) == 0 {
		return nil
	}
	escs, err := s.daemon.ListEscalations(ctx, "", escalationsListLimit)
	if err != nil {
		s.logger.Warn("listing escalations for /up", "email", email, "error", err)
		return nil
	}
	var best *beadsapi.BeadDetail
	for _, ed := range escs {
		b := ed.Issue
		if b == nil {
			continue
		}
		if mine, _ := escalationOwnerRow(b.Fields, lanes); !mine {
			continue
		}
		held := beadsapi.HeldByFromFields(b.Fields)
		if held == "" {
			held = beadsapi.LaneOperator // unstamped owner-audience: the gardenio owner's, as #756 lists it
		}
		if !slices.Contains(lanes, held) {
			continue
		}
		if best == nil || b.Priority < best.Priority || (b.Priority == best.Priority && b.CreatedAt.Before(best.CreatedAt)) {
			best = b
		}
	}
	return best
}

// selectUpBead implements the v1 data-source policy: prefer the viewer's
// top in_progress bead, fall back to the highest-priority open bead
// globally. Both queries are best-effort — daemon errors set EmptyReason
// so the operator at least sees what failed.
func (s *Server) selectUpBead(ctx context.Context, email string) upRenderContext {
	rc := upRenderContext{ViewerEmail: email}

	if email != "" {
		if b := s.upNeedsYouEscalation(ctx, email); b != nil {
			rc.Bead = b
			return rc
		}
		// canonicalOIDC turns the bare mixed-case email into the canonical
		// "oidc:<lowercased-email>" alias the daemon stores; a bare email never
		// matched the "kind:subject"-keyed alias filter (ka-0000002505).
		q := beadsapi.ListBeadsQuery{
			Statuses:          []string{"in_progress"},
			OperatedByAliases: []string{canonicalOIDC(email)},
			Sort:              "priority",
			Limit:             1,
			ExcludeScheduled:  true,
			// An escalation reaches /up only through the needs-you pick, so
			// one operated by the viewer but in hand never shows as theirs.
			ExcludeTypes: []string{"escalation"},
		}
		res, err := s.daemon.ListBeadsFiltered(ctx, q)
		if err != nil {
			s.logger.Warn("listing in_progress beads for /up", "email", email, "error", err)
		} else if res != nil && len(res.Beads) > 0 && res.Beads[0] != nil {
			rc.Bead = res.Beads[0]
			return rc
		}
	}

	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Statuses:         []string{"open"},
		Sort:             "priority",
		Limit:            1,
		ExcludeScheduled: true,
		// As above: escalations come only from the needs-you pick.
		ExcludeTypes: []string{"escalation"},
	})
	if err != nil {
		s.logger.Warn("listing open beads for /up", "error", err)
		rc.Empty = true
		rc.EmptyReason = "Couldn't reach the beads daemon."
		return rc
	}
	if res != nil && len(res.Beads) > 0 && res.Beads[0] != nil {
		rc.Bead = res.Beads[0]
		return rc
	}
	rc.Empty = true
	rc.EmptyReason = "No work in the queue."
	return rc
}

// renderUpCard turns the resolved context into the template view-model and
// hands off to the layout-driven page renderer.
func (s *Server) renderUpCard(w http.ResponseWriter, r *http.Request, rc upRenderContext) {
	data := map[string]any{
		"ViewerEmail": rc.ViewerEmail,
		"Empty":       rc.Empty || rc.Bead == nil,
		"EmptyReason": rc.EmptyReason,
	}
	if rc.Bead != nil {
		data["Card"] = s.buildUpCard(r.Context(), rc.Bead, rc.ViewerEmail)
	}
	s.render(w, r, "up.html", data)
}

// buildUpCard composes the view-model for one bead. Best-effort lookups:
// any sub-fetch failure degrades that section to its empty state but
// keeps the rest of the card rendering.
func (s *Server) buildUpCard(ctx context.Context, bead *beadsapi.BeadDetail, viewerEmail string) upCardView {
	now := s.now()
	// Per-user-homepage M2 §6 (ka-0000007787): resolve slug before chip
	// render so the operator chip in the up-card links to /u/{slug}.
	s.populateOperatedBySlug(ctx, bead)
	chip := resolveOperatorChip(bead.OperatedBy, bead.OperatedByDisplay, bead.OperatedByEmail, bead.OperatedBySlug, bead.CreatedBy, s.basePath, viewerEmail)
	v := upCardView{
		ID:               bead.ID,
		Title:            bead.Title,
		TitleHTML:        LinkifyText(bead.Title, s.basePath),
		Type:             bead.Type,
		Status:           bead.Status,
		Priority:         bead.Priority,
		Glyph:            glyphForType(bead.Type),
		OperatorChipHTML: renderOperatorChip(chip),
		Project:          projectFromLabels(bead.Labels),
		Ago:              fmtTimeAgo(bead.UpdatedAt, now),
		BeadURL:          s.basePath + "/" + bead.ID,
	}
	if !bead.UpdatedAt.IsZero() {
		v.UpdatedAtRFC = bead.UpdatedAt.UTC().Format(time.RFC3339)
	}

	// Markdown body — snipped to upDescLimit runes. Full body lives on
	// the bead-show page (linked from the card header).
	if bead.Description != "" {
		raw := bead.Description
		if rs := []rune(raw); len(rs) > upDescLimit {
			raw = strings.TrimSpace(string(rs[:upDescLimit])) + "…"
			v.DescTruncated = true
		}
		var buf bytes.Buffer
		if err := s.md.Convert([]byte(raw), &buf); err != nil {
			s.logger.Warn("rendering /up markdown", "id", bead.ID, "error", err)
		} else {
			v.DescriptionHTML = LinkifyDescriptionHTML(buf.String(), s.basePath, nil)
		}
	}

	// Dependencies — collapsed counts. Daemon errors degrade to zero.
	if deps, err := s.daemon.GetDependencies(ctx, bead.ID); err == nil {
		for _, d := range deps {
			switch d.Type {
			case "blocks":
				v.BlockedByCount++
			case "parent-child":
				v.ParentID = d.DependsOnID
			}
		}
	} else {
		s.logger.Debug("up: getting dependencies", "id", bead.ID, "error", err)
	}
	if rev, err := s.daemon.GetReverseDependencies(ctx, bead.ID); err == nil {
		for _, d := range rev {
			if d.Type == "blocks" {
				v.BlocksCount++
			}
		}
	} else {
		s.logger.Debug("up: getting reverse deps", "id", bead.ID, "error", err)
	}

	// Text-mention counts (best-effort; old daemons may 404).
	if mb, err := s.daemon.GetMentionedBy(ctx, bead.ID, 50); err == nil {
		v.MentionedByCount = len(mb)
	}
	if m, err := s.daemon.GetMentions(ctx, bead.ID, 50); err == nil {
		v.MentionsCount = len(m)
	}

	// Activity rail — top-N most recent events.
	if events, err := s.daemon.GetEvents(ctx, bead.ID); err == nil {
		rows := buildActivityRows(events)
		if len(rows) > upActivityHead {
			rows = rows[:upActivityHead]
		}
		v.RecentActivity = rows
	} else {
		s.logger.Debug("up: getting events", "id", bead.ID, "error", err)
	}

	// Linked MRs — opened MRs whose title or description names this bead.
	v.LinkedMRs = s.findLinkedMRs(ctx, bead.ID)

	// Suggested-action zone — per-type default copy from PRD §3.3 / §4.3.
	v.SuggestedAction = suggestedActionForType(bead.Type)

	// Decision resolve chips (PRD §3.3): one-tap options parsed from the
	// decision's `options` field. Empty for non-decisions.
	if bead.Type == "decision" {
		v.IsDecision = true
		v.Context = s.decisionContextOf(bead)
		v.Evidence = s.decisionEvidenceOf(ctx, bead)
		v.DecisionOptions = parseDecisionOptions(bead.Fields["options"])
	}

	// Type-conditional footer + global keymap (PRD §4.2/§4.3). The footer
	// drives the visible bar; the same model is marshalled to KeymapJSON so
	// up.js dispatches keys from an island that can never drift from the
	// rendered buttons.
	v.Footer = buildUpFooter(bead.Type, len(v.DecisionOptions) > 0)
	if v.IsDecision && !v.Context.Present() {
		v.Footer = disarmUpDecisionEnter(v.Footer)
	}
	v.KeymapJSON = marshalUpKeymap(v.Footer)

	return v
}

// marshalUpKeymap serializes the footer's primary + secondaries + global
// keymap into the JSON island up.js reads. Marshalling can't realistically
// fail on this fixed shape; on the off chance it does we emit an empty object
// so up.js degrades to "buttons render, keyboard is inert" rather than
// injecting broken script.
func marshalUpKeymap(f upFooter) template.JS {
	payload := struct {
		Primary     *upAction  `json:"primary"`
		Secondaries []upAction `json:"secondaries"`
		Global      []upAction `json:"global"`
		ReadOnly    bool       `json:"readOnly"`
	}{
		Primary:     f.Primary,
		Secondaries: f.Secondaries,
		Global:      f.Global,
		ReadOnly:    f.ReadOnly,
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return template.JS("{}")
	}
	return template.JS(b)
}

// findLinkedMRs scans the opened MRs for a substring match on bead-id.
// Returns nil when GitLab isn't wired. Best-effort: a fetch error logs
// and returns an empty slice so the rest of the card still renders.
func (s *Server) findLinkedMRs(ctx context.Context, beadID string) []upMRLink {
	if s.mrLister == nil || s.mrProjectPath == "" {
		return nil
	}
	opts := bridge.MRListOptions{State: "opened", PerPage: upMRSearchLimit, Search: beadID}
	mrs, err := s.mrLister.ListMergeRequests(ctx, s.mrProjectPath, opts)
	if err != nil {
		s.logger.Debug("up: listing MRs", "bead", beadID, "error", err)
		return nil
	}
	out := make([]upMRLink, 0, len(mrs))
	for _, mr := range mrs {
		if !mentionsBead(mr.Title, mr.Description, mr.SourceBranch, beadID) {
			continue
		}
		out = append(out, upMRLink{
			IID:         mr.IID,
			Title:       mr.Title,
			WebURL:      mr.WebURL,
			State:       mr.State,
			Draft:       mr.Draft,
			MergeStatus: mr.MergeStatus,
		})
	}
	return out
}

// mentionsBead is a substring case-insensitive check across an MR's text
// fields. The GitLab `search` parameter does a coarse match (and the API
// occasionally returns hits where the term appears only in commit
// messages), so we re-verify locally.
func mentionsBead(title, description, branch, beadID string) bool {
	if beadID == "" {
		return false
	}
	needle := strings.ToLower(beadID)
	for _, hay := range []string{title, description, branch} {
		if strings.Contains(strings.ToLower(hay), needle) {
			return true
		}
	}
	return false
}

// suggestedActionForType is the PRD §3.3 / §4.3 default-action table.
// Read-only proto: the strings render in the suggested-action zone but
// the buttons are placeholders — wiring lands with the action handler.
func suggestedActionForType(t string) upSuggestedAction {
	switch t {
	case "task":
		return upSuggestedAction{Primary: "Bundle and close", Secondary: "Skip", HelpText: "Wrap work, attach report, close."}
	case "bug":
		return upSuggestedAction{Primary: "Reproduce and close", Secondary: "Skip", HelpText: "Confirm fix; attach repro evidence."}
	case "epic":
		return upSuggestedAction{Primary: "Pick child", Secondary: "Skip", HelpText: "Drop into the next blocked-on-decision child."}
	case "agent":
		return upSuggestedAction{Primary: "Nudge", Secondary: "Skip", HelpText: "Send a message; or open the live terminal."}
	case "decision":
		return upSuggestedAction{Primary: "Resolve", Secondary: "Defer", HelpText: "Pick an option or dismiss with reason."}
	case "escalation":
		return upSuggestedAction{Primary: "Acknowledge", Secondary: "Skip", HelpText: "Take ownership; resolve in the linked surface."}
	case "molecule", "formula":
		return upSuggestedAction{Primary: "Apply", Secondary: "Skip", HelpText: "Run the next pending step."}
	case "schedule", "trigger":
		return upSuggestedAction{Primary: "Toggle enabled", Secondary: "Skip", HelpText: "Pause or resume this trigger."}
	case "message", "squawk", "doc", "bundle":
		return upSuggestedAction{Primary: "", Secondary: "Skip", HelpText: "Read-only — no primary action."}
	default:
		return upSuggestedAction{Primary: "Close", Secondary: "Skip", HelpText: "Mark done or move on."}
	}
}
