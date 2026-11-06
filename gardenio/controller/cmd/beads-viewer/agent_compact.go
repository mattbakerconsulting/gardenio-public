package main

import (
	"html"
	"net/http"
	"strings"

	"gardenio/controller/internal/oidcauth"
)

// compactCommand is the Claude Code slash command that triggers an
// (auto)compaction of the running session's context window. We deliver it
// through the nudge path rather than the raw input path because nudge types
// the text and then reliably submits Enter (with retry) — a bare PTY write of
// "/compact" would leave the command sitting unsent in the prompt.
const compactCommand = "/compact"

// handleAgentCompact sends the Claude Code "/compact" slash command to a live
// agent session, initiating a context-window compaction. Compaction is
// disruptive (it rewrites the session transcript), so a click first swaps in
// an inline confirmation fragment; the confirmed POST (confirmed=1) is what
// actually forwards the command to coopmux.
//
// RBAC is enforced at the route layer (operatorOnly). Every confirmed compact
// is audit-logged with the operator's email, mirroring handleAgentKeys /
// handleAgentNudge.
func (s *Server) handleAgentCompact(w http.ResponseWriter, r *http.Request) {
	id, sessionID, ok := s.agentWriteContext(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}

	email := oidcauth.EmailFromContext(r.Context())
	roles := oidcauth.RolesFromContext(r.Context())

	// First click: show the confirmation fragment instead of compacting.
	if r.PostForm.Get("confirmed") != "1" {
		s.logger.Info("agent_terminal_write",
			"event", "agent_terminal_write",
			"subverb", "compact",
			"agent", id,
			"session", sessionID,
			"operator", email,
			"roles", roles,
			"result", "confirm_required")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fragment := renderAgentCompactConfirm(s.basePath, id, sessionID)
		if _, werr := w.Write([]byte(fragment)); werr != nil {
			s.logger.Debug("write compact-confirm fragment", "error", werr)
		}
		return
	}

	// Confirmed: deliver "/compact" via the nudge path (types + Enter).
	if err := s.coopmux.Nudge(r.Context(), sessionID, compactCommand); err != nil {
		s.logger.Error("agent_terminal_write: coopmux compact (nudge) failed",
			"agent", id, "session", sessionID, "operator", email, "error", err)
		translateUpstreamWriteError(w, err)
		return
	}

	s.logger.Info("agent_terminal_write",
		"event", "agent_terminal_write",
		"subverb", "compact",
		"agent", id,
		"session", sessionID,
		"operator", email,
		"roles", roles,
		"result", "ok")

	// htmx swaps the confirm fragment out for an empty slot on success.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// renderAgentCompactConfirm builds the inline confirmation fragment shown when
// an operator clicks Compact. It swaps in over the #agent-compact-confirm slot.
// Confirm POSTs back to /compact with confirmed=1; Cancel clears the slot.
func renderAgentCompactConfirm(basePath, beadID, sessionID string) string {
	postURL := basePath + "/agents/" + beadID + "/compact"

	var sb strings.Builder
	sb.WriteString(`<div id="agent-compact-confirm" class="agent-keys-confirm" role="alertdialog" aria-live="assertive">`)
	sb.WriteString(`<p class="agent-keys-confirm-msg">`)
	sb.WriteString(`Compact the context window of agent <code>`)
	sb.WriteString(html.EscapeString(beadID))
	sb.WriteString(`</code> session <code>`)
	sb.WriteString(html.EscapeString(sessionID))
	sb.WriteString(`</code>? This sends <strong>/compact</strong> to Claude Code and rewrites the session transcript.`)
	sb.WriteString(`</p>`)
	sb.WriteString(`<form class="agent-keys-confirm-actions" hx-post="`)
	sb.WriteString(html.EscapeString(postURL))
	sb.WriteString(`" hx-target="#agent-compact-confirm" hx-swap="outerHTML">`)
	sb.WriteString(`<input type="hidden" name="confirmed" value="1">`)
	sb.WriteString(`<button type="submit" class="agent-keys-confirm-yes">Confirm compact</button>`)
	sb.WriteString(`</form>`)
	sb.WriteString(`<form class="agent-keys-confirm-cancel-form" hx-post="`)
	sb.WriteString(html.EscapeString(basePath + "/agents/" + beadID + "/compact/cancel"))
	sb.WriteString(`" hx-target="#agent-compact-confirm" hx-swap="outerHTML">`)
	sb.WriteString(`<button type="submit" class="agent-keys-confirm-no">Cancel</button>`)
	sb.WriteString(`</form>`)
	sb.WriteString(`</div>`)
	return sb.String()
}

// handleAgentCompactCancel records an operator's cancel of a compact
// confirmation. No upstream call; the audit line pairs with the
// confirm_required line so a forensic trace can match every prompt with an
// outcome. The empty #agent-compact-confirm body removes the fragment.
func (s *Server) handleAgentCompactCancel(w http.ResponseWriter, r *http.Request) {
	id, sessionID, ok := s.agentWriteContext(w, r)
	if !ok {
		return
	}
	email := oidcauth.EmailFromContext(r.Context())
	roles := oidcauth.RolesFromContext(r.Context())
	s.logger.Info("agent_terminal_write",
		"event", "agent_terminal_write",
		"subverb", "compact",
		"agent", id,
		"session", sessionID,
		"operator", email,
		"roles", roles,
		"result", "ux_canceled")

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("HX-Reswap", "outerHTML")
	if _, err := w.Write([]byte(`<div id="agent-compact-confirm"></div>`)); err != nil {
		s.logger.Debug("write compact-cancel fragment", "error", err)
	}
}
