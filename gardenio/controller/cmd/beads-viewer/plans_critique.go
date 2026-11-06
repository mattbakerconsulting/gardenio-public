package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// Line-precise inline critique on plan bundles (epic ka-0000002316). Critique
// comments are ordinary comments-as-beads on the plan's STEP bead — no kbeads
// schema change, no new bead type (operator decision ka-0000004596: the feature
// is additive/non-destructive and ships ON BY DEFAULT, no env-var flag).
//
// The write path mirrors handleAddComment (server.go): 401 when unauthenticated
// (defense-in-depth behind operatorOnly), non-empty + length validation, then
// s.daemon.AddComment on the step bead. AddComment returns only error, so both
// handlers re-GetComments to re-render the flat thread fragment.
//
// The redundant W3C-style anchor (block_id + exact quote + 32-char prefix/suffix)
// rides in the flat text column as a defined HTML-comment prefix — there is no
// metadata column on the Comment struct. encodeCritiqueComment stamps it;
// decodeCritiqueComment peels it back off so the rendered body shows only the
// user's markdown while the anchor surfaces as data-* attributes for the gutter
// UI (T4) and the 3-tier resolver (T6).

// critiqueAnchor is the redundant hybrid anchor persisted with each critique
// comment. All fields are optional: an all-empty anchor is a plan/step-level
// comment (no line anchor) and is stored as plain text with no prefix.
type critiqueAnchor struct {
	// BlockID is the data-block-id stamped on the rendered block node (T1).
	BlockID string `json:"block_id,omitempty"`
	// Quote is the exact selected text (W3C TextQuoteSelector "exact").
	Quote string `json:"quote,omitempty"`
	// Prefix is up to 32 chars of context immediately before the quote.
	Prefix string `json:"prefix,omitempty"`
	// Suffix is up to 32 chars of context immediately after the quote.
	Suffix string `json:"suffix,omitempty"`
}

func (a critiqueAnchor) isEmpty() bool {
	return a.BlockID == "" && a.Quote == "" && a.Prefix == "" && a.Suffix == ""
}

// The anchor is serialized as `<!--critique:{json}-->\n` and prepended to the
// comment text. encoding/json escapes `<`, `>`, and `&` to \uXXXX by default,
// so a quote containing the literal `-->` can never produce a false close
// delimiter — the codec round-trips arbitrary user text.
const (
	critiqueAnchorOpen  = "<!--critique:"
	critiqueAnchorClose = "-->"
)

// encodeCritiqueComment prepends the anchor prefix to the user's markdown body.
// An empty anchor returns the body unchanged so unanchored comments stay clean.
func encodeCritiqueComment(anchor critiqueAnchor, body string) string {
	if anchor.isEmpty() {
		return body
	}
	j, err := json.Marshal(anchor)
	if err != nil {
		// A flat all-string struct cannot actually fail to marshal; stay
		// defensive and never block the comment on anchor serialization.
		return body
	}
	return critiqueAnchorOpen + string(j) + critiqueAnchorClose + "\n" + body
}

// decodeCritiqueComment splits a stored critique comment into its anchor and
// the user-facing markdown body. Text with no anchor prefix — or a malformed
// one — returns a zero anchor and the original text verbatim, so nothing is
// ever lost or silently mis-rendered.
func decodeCritiqueComment(text string) (critiqueAnchor, string) {
	if !strings.HasPrefix(text, critiqueAnchorOpen) {
		return critiqueAnchor{}, text
	}
	rest := text[len(critiqueAnchorOpen):]
	payload, after, found := strings.Cut(rest, critiqueAnchorClose)
	if !found {
		return critiqueAnchor{}, text
	}
	body := strings.TrimPrefix(after, "\n")
	var a critiqueAnchor
	if err := json.Unmarshal([]byte(payload), &a); err != nil {
		return critiqueAnchor{}, text
	}
	return a, body
}

// critiqueCommentView embeds the shared commentView (linkified body, operator
// chip, timestamps — reused verbatim) and adds the decoded anchor so the
// template can stamp data-block-id / data-quote for the gutter UI.
type critiqueCommentView struct {
	commentView
	HasAnchor bool
	BlockID   string
	Quote     string
	Prefix    string
	Suffix    string
}

// critiqueCommentViews decodes each comment's anchor, renders the cleaned body
// through the shared linkifyComments pipeline, and zips the two back together.
func (s *Server) critiqueCommentViews(comments []beadsapi.Comment, since time.Time, viewerEmail string) []critiqueCommentView {
	if len(comments) == 0 {
		return nil
	}
	anchors := make([]critiqueAnchor, len(comments))
	cleaned := make([]beadsapi.Comment, len(comments))
	for i, c := range comments {
		a, body := decodeCritiqueComment(c.Text)
		anchors[i] = a
		cleaned[i] = c
		cleaned[i].Text = body
	}
	base := linkifyComments(s.mdInline, cleaned, s.basePath, since, viewerEmail)
	out := make([]critiqueCommentView, len(base))
	for i := range base {
		a := anchors[i]
		out[i] = critiqueCommentView{
			commentView: base[i],
			HasAnchor:   !a.isEmpty(),
			BlockID:     a.BlockID,
			Quote:       a.Quote,
			Prefix:      a.Prefix,
			Suffix:      a.Suffix,
		}
	}
	return out
}

// handlePlanCritiqueComments renders the flat critique thread for one plan step.
// GET /plans/{id}/comments?step={beadID} — viewer-scoped (any authenticated
// viewer reads it). Comments live on the STEP bead via existing comment
// semantics, so this is a thin read + anchor-decode over s.daemon.GetComments.
func (s *Server) handlePlanCritiqueComments(w http.ResponseWriter, r *http.Request) {
	planID := strings.TrimSpace(r.PathValue("id"))
	stepID := strings.TrimSpace(r.URL.Query().Get("step"))
	if planID == "" || stepID == "" {
		http.Error(w, "Missing plan or step id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	ctx := r.Context()
	if email != "" {
		ctx = beadsapi.WithOperator(ctx, canonicalOIDC(email))
	}
	comments, err := s.daemon.GetComments(ctx, stepID)
	if err != nil {
		s.logger.Warn("plan critique: get comments", "step", stepID, "error", err)
		comments = nil
	}
	latest := latestCommentTime(comments)
	since := parseSinceParam(r.URL.Query().Get("since"))
	// No-op short-circuit mirrors handleBeadComments: the client's cursor
	// matches the server-side latest, so there is nothing to swap. The
	// live-refresh lane (T5) reads the same data-comments-latest cursor this
	// write emits. 204 No Content (NOT 304) so an htmx outerHTML swap of the
	// empty body can never erase #plan-critique-thread — the same footgun that
	// wiped the bead comments section in ka-0000006459.
	if !latest.IsZero() && !since.IsZero() && latest.Equal(since) {
		w.Header().Set("X-Comments-Latest", commentsLatestAttr(latest))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.writeCritiqueThread(w, r, planID, stepID, email, comments, since)
}

// handleAddPlanCritiqueComment posts a critique comment on the plan step bead
// and re-renders the thread fragment. POST /plans/{id}/comments?step={beadID}.
// Operator-gated at the route layer (operatorOnly); re-checks EmailFromContext
// defense-in-depth and re-stamps the operator on the daemon ctx so the kbeads
// audit resolver does not fail-closed (mirrors handlePlanAddStep, R2
// ka-0000005772).
func (s *Server) handleAddPlanCritiqueComment(w http.ResponseWriter, r *http.Request) {
	planID := strings.TrimSpace(r.PathValue("id"))
	stepID := strings.TrimSpace(r.URL.Query().Get("step"))
	if planID == "" || stepID == "" {
		http.Error(w, "Missing plan or step id", http.StatusBadRequest)
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		// Defense-in-depth: operatorOnly should already block this, but a
		// future middleware reorder must not let anonymous writes through.
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	ctx := beadsapi.WithOperator(r.Context(), canonicalOIDC(email))
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	text := strings.TrimSpace(r.PostForm.Get("text"))
	if text == "" {
		http.Error(w, "Comment cannot be empty.", http.StatusBadRequest)
		return
	}
	if len(text) > maxCommentLen {
		http.Error(w, "Comment is too long (max 10000 characters).", http.StatusRequestEntityTooLarge)
		return
	}
	anchor := critiqueAnchor{
		BlockID: strings.TrimSpace(r.PostForm.Get("block_id")),
		Quote:   r.PostForm.Get("quote"),
		Prefix:  r.PostForm.Get("prefix"),
		Suffix:  r.PostForm.Get("suffix"),
	}
	stored := encodeCritiqueComment(anchor, text)
	if err := s.daemon.AddComment(ctx, stepID, email, stored); err != nil {
		s.logger.Error("plan critique: add comment", "step", stepID, "author", email, "error", err)
		http.Error(w, "Failed to post comment.", http.StatusBadGateway)
		return
	}
	// AddComment returns only error, so re-fetch to re-render the fragment with
	// the freshly-posted comment (mirror handleAddComment -> handleBeadComments).
	comments, err := s.daemon.GetComments(ctx, stepID)
	if err != nil {
		s.logger.Warn("plan critique: get comments after add", "step", stepID, "error", err)
		comments = nil
	}
	s.writeCritiqueThread(w, r, planID, stepID, email, comments, time.Time{})
}

// writeCritiqueThread renders the plan_critique_thread fragment for the given
// step's comments. Shared by the GET and POST handlers.
func (s *Server) writeCritiqueThread(w http.ResponseWriter, r *http.Request, planID, stepID, email string, comments []beadsapi.Comment, since time.Time) {
	latestAttr := commentsLatestAttr(latestCommentTime(comments))
	w.Header().Set("X-Comments-Latest", latestAttr)
	s.renderPartial(w, r, "templates/partials/plan_critique_thread.html", "plan_critique_thread", map[string]any{
		"PlanID":            planID,
		"StepID":            stepID,
		"Comments":          s.critiqueCommentViews(comments, since, email),
		"CommentsLatestRFC": latestAttr,
		"AuthorEmail":       email,
		"CanComment":        email != "",
	})
}

// renderPlanCritiqueFragment renders the plan_critique_thread partial to a
// string for the SSE payload (T5, ka-0000007360). Mirrors renderPlanStepsFragment's
// buffer-render idiom (bindAgentLink + ExecuteTemplate to a bytes.Buffer);
// returns "" on any error so the caller skips the frame rather than emitting a
// broken one. This is the live-refresh sibling of writeCritiqueThread: the plan
// SSE stream calls it when a `beads.comment.added` event lands on a step in the
// plan's interest set, and plan_events.js swaps the freshly-rendered thread into
// an open critique overlay bound to that same step.
//
// epicID IS the plan id (the epic) — it feeds the fragment's data-plan-id so
// the swapped thread keeps a correct hx-post target if re-processed. The
// comments themselves hang off the STEP bead, not the epic. Rendered with a
// zero `since` so no comment is spuriously flagged IsNew — the outerHTML swap is
// itself the visual signal, exactly as the POST re-render (writeCritiqueThread,
// time.Time{}).
func (s *Server) renderPlanCritiqueFragment(ctx context.Context, r *http.Request, epicID, stepID string, isOperator bool) string {
	tmpl, ok := s.partials["templates/partials/plan_critique_thread.html"]
	if !ok {
		s.logger.Error("plan events: plan_critique_thread partial not found")
		return ""
	}
	// The viewer's own email drives the mine-vs-not-mine operator-chip skin on
	// each comment (critiqueCommentViews) and mirrors the connect-time
	// isOperator verdict; a read-only viewer still gets the thread, just without
	// the compose affordance (which lives in the static shell, not this fragment).
	email := oidcauth.EmailFromContext(r.Context())
	comments, err := s.daemon.GetComments(ctx, stepID)
	if err != nil {
		s.logger.Warn("plan events: get critique comments", "step", stepID, "error", err)
		return ""
	}
	data := map[string]any{
		"PlanID":            epicID,
		"StepID":            stepID,
		"Comments":          s.critiqueCommentViews(comments, time.Time{}, email),
		"CommentsLatestRFC": commentsLatestAttr(latestCommentTime(comments)),
		"AuthorEmail":       email,
		"CanComment":        isOperator && email != "",
		"BasePath":          s.basePath,
	}
	bound, err := s.bindAgentLink(tmpl, r)
	if err != nil {
		s.logger.Error("plan events: bind critique template", "error", err)
		return ""
	}
	var buf bytes.Buffer
	if err := bound.ExecuteTemplate(&buf, "plan_critique_thread", data); err != nil {
		s.logger.Error("plan events: render plan_critique_thread", "step", stepID, "error", err)
		return ""
	}
	return buf.String()
}
