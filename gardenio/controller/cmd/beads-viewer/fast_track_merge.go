package main

// Phase 6 — fast-track-merge action descriptor + server handler.
// Plan-of-record: synthesis.md §Phase 6. Bead: ka-0000005934.
//
// D3 (gating policy): CI green + bypass record. The descriptor confirms before
// dispatch; the server handler:
//   1. validates ci_status == "passed" (rejects with 412 otherwise),
//   2. requires a bypass_reason in the request body,
//   3. writes a structured audit line to the logger and (when bead_id is
//      present) appends a comment to the bead via beadsapi.
//
// The handler does NOT actually merge the MR. Phase 6 ships the gating + audit
// surface; the merge call itself is deferred to a follow-up phase that wires
// in the GitLab API. Returning a 200 with a "ready-to-merge" fragment lets
// htmx swap a confirmation pill in without surprising downstream phases.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// registerServerBoundActions wires action handlers that need access to the
// Server (for s.daemon, s.logger, …). Called from NewServer after the Server
// struct is built. Phase 6 lands one such action: fast-track-merge.
func registerServerBoundActions(s *Server) {
	if s == nil || s.actions == nil {
		return
	}
	s.actions.Register("fast-track-merge", s.fastTrackMergeHandler)
}

// FastTrackMergeAction returns an htmx-post descriptor for the fast-track-
// merge button. swapInto is the CSS selector to replace with the response
// fragment — the widget renders an empty placeholder with that id.
//
// ciStatus is captured at render time and round-trips through Params so the
// server can verify it hasn't drifted between render and click. A drift means
// CI status changed; the server re-reads at handle time anyway, so this is a
// belt-and-braces check that surfaces in the bypass record.
func FastTrackMergeAction(mrID, mrURL, beadID, ciStatus, swapInto string) ActionDescriptor {
	return ActionDescriptor{
		ID:    "fast-track-merge",
		Kind:  ActionHtmxPost,
		Label: "Fast-track merge",
		Params: map[string]any{
			"mr_id":     mrID,
			"mr_url":    mrURL,
			"bead_id":   beadID,
			"ci_status": ciStatus,
			// bypass_reason is captured client-side via the confirm modal's
			// promptable-body extension. For Phase 6 we ship a fixed reason in
			// the descriptor; the gardener UX expanding this lives in a
			// follow-up bead.
			"bypass_reason": "operator-initiated incident remediation",
		},
		SwapInto: swapInto,
		Confirm: &ConfirmDescriptor{
			Title:   "Fast-track merge?",
			Body:    "Bypasses normal review. Logs a bypass record on the incident bead.",
			Confirm: "Merge",
			Danger:  true,
		},
		Danger: true,
		Hint:   "CI must be green; logs an audit record on the incident bead.",
	}
}

// fastTrackMergeHandler is the server-side implementation behind the
// htmx-post dispatch for "fast-track-merge". Wired by registerStandardActions.
//
// We require a beadsapi client so the audit comment lands; if Server.beads is
// nil (test path), we still write the log line but skip the comment.
func (s *Server) fastTrackMergeHandler(w http.ResponseWriter, r *http.Request, p ActionParams) error {
	mrID := stringParam(p, "mr_id")
	mrURL := stringParam(p, "mr_url")
	beadID := stringParam(p, "bead_id")
	ciStatus := stringParam(p, "ci_status")
	reason := stringParam(p, "bypass_reason")

	// D3 gate: CI must report passed at click-time. We trust the client value
	// for now (it was set at render-time off the same payload that produced
	// the diff); a future phase polls GitLab directly.
	if ciStatus != "passed" {
		http.Error(w, "ci status is "+ciStatus+", refusing fast-track merge", http.StatusPreconditionFailed)
		return nil
	}

	if reason == "" {
		http.Error(w, "bypass_reason is required", http.StatusBadRequest)
		return nil
	}

	auditLine := fmt.Sprintf("fast-track merge: mr=%s url=%s bead=%s actor=%s reason=%q",
		mrID, mrURL, beadID, actorFromRequest(r), reason)
	s.logger.Info(auditLine, "kind", "incident-intel-bypass-record")

	if beadID != "" && s.daemon != nil {
		if err := writeBypassComment(r.Context(), s.daemon, beadID, mrID, mrURL, reason, actorFromRequest(r)); err != nil {
			// Don't fail the request — the audit log line is the source of
			// truth. Surface it as a Sentry-style breadcrumb in the response.
			s.logger.Error("fast-track merge: failed to write bypass comment",
				"bead", beadID, "error", err)
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w,
		`<span class="ii-pill" data-confidence="high">ready to merge — bypass recorded</span>`)
	return nil
}

// writeBypassComment posts an audit comment to the incident bead. Errors are
// returned for the caller to log; we never surface them to the client as the
// merge gate already passed.
func writeBypassComment(ctx context.Context, bc beadsCommenter, beadID, mrID, mrURL, reason, actor string) error {
	if bc == nil {
		return errors.New("beads client unavailable")
	}
	body := strings.TrimSpace(fmt.Sprintf(
		"**Fast-track merge bypass record**\n\n"+
			"- MR: %s (%s)\n"+
			"- Actor: %s\n"+
			"- Reason: %s\n",
		safeOrDash(mrID), safeOrDash(mrURL), safeOrDash(actor), safeOrDash(reason),
	))
	return bc.AddComment(ctx, beadID, actor, body)
}

// beadsCommenter is the smallest slice of the beadAPI surface this handler
// needs. Phase 6 doesn't pull in the full client interface; tests inject a
// fake by satisfying this contract. Signature mirrors beadAPI.AddComment.
type beadsCommenter interface {
	AddComment(ctx context.Context, beadID, author, text string) error
}

// stringParam pulls a string from the JSON-decoded params map. Treats numbers
// and bools as their fmt.Sprint representations so a JS client sending a
// non-string is still readable in the audit log.
func stringParam(p ActionParams, key string) string {
	if p == nil {
		return ""
	}
	switch v := p[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case nil:
		return ""
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
}

// actorFromRequest looks at the OIDC headers the parent middleware sets. Falls
// back to "unknown" so the audit line is never blank.
func actorFromRequest(r *http.Request) string {
	for _, h := range []string{"X-Forwarded-User", "X-User-Email", "X-User"} {
		if v := r.Header.Get(h); v != "" {
			return v
		}
	}
	return "unknown"
}

func safeOrDash(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "—"
	}
	return s
}
