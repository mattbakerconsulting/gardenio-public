package main

import (
	"errors"
	"html"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/gardenhub"
	"gardenio/controller/internal/oidcauth"
)

// maxAgentInputLen caps the size of a single text-input write. PTY writes
// are unbounded by the protocol; this limit protects against a runaway form
// submission burning the audit log and the agent's input buffer at the same
// time. Operators who need to paste a large payload can chunk it.
const maxAgentInputLen = 8 * 1024

// handleAgentInput accepts a `text=...` form POST from the operator and
// forwards it to the agent's PTY via coopmuxapi.SendInput. RBAC is enforced
// at the route layer (RequireRole(RoleOperator)); this handler additionally
// audit-logs every successful write with the operator's email and a snippet
// of the forwarded text.
//
// Phase 2 scope: text only. No raw key codes, no nudge, no respond. Those
// are Phase 3 verbs that need their own gating and shape.
func (s *Server) handleAgentInput(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Agent not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting agent bead for input", "id", id, "error", err)
		http.Error(w, "Failed to load agent", http.StatusInternalServerError)
		return
	}
	if bead.Type != "agent" {
		http.Error(w, "Not an agent bead", http.StatusBadRequest)
		return
	}
	sessionID := bead.MuxSessionID()
	if sessionID == "" {
		http.Error(w, "Agent has no live session", http.StatusConflict)
		return
	}
	if s.coopmux == nil || s.coopmux.Disabled() {
		http.Error(w, "coopmux unavailable in this deployment", http.StatusServiceUnavailable)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	text := r.PostForm.Get("text")
	if text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}
	if len(text) > maxAgentInputLen {
		http.Error(w, "text exceeds maximum length", http.StatusRequestEntityTooLarge)
		return
	}

	email := oidcauth.EmailFromContext(r.Context())
	roles := oidcauth.RolesFromContext(r.Context())

	if err := s.coopmux.SendInput(r.Context(), sessionID, text); err != nil {
		switch {
		case errors.Is(err, coopmuxapi.ErrSessionNotFound):
			s.logger.Warn("agent_terminal_write: session not found",
				"agent", id, "session", sessionID, "operator", email)
			http.Error(w, "session not found upstream", http.StatusNotFound)
		case errors.Is(err, coopmuxapi.ErrDisabled):
			http.Error(w, "coopmux unavailable in this deployment", http.StatusServiceUnavailable)
		default:
			s.logger.Error("agent_terminal_write: coopmux SendInput failed",
				"agent", id, "session", sessionID, "operator", email, "error", err)
			http.Error(w, "Failed to forward input", http.StatusBadGateway)
		}
		return
	}

	// Audit trail. Keep snippet short — Slack-bridge and other readers can
	// fetch the full sequence from coopmux if a forensic trace is needed.
	s.logger.Info("agent_terminal_write",
		"event", "agent_terminal_write",
		"agent", id,
		"session", sessionID,
		"operator", email,
		"roles", roles,
		"len", len(text),
		"snippet", agentInputSnippet(text, 80))

	// Empty 204 — the form's caller (toolbar) clears its own draft on swap.
	w.WriteHeader(http.StatusNoContent)
}

// handleAgentControlClaim toggles the operator-gated "Take control" surface.
// It does not change any backend state — coopmux's PTY is always writable —
// but the toolbar needs a server-rendered fragment so RBAC denial flows
// through the same htmx swap target as the success path.
//
// Returns a small HTML fragment with the active toolbar. On role denial the
// RequireRole middleware returns 403 before this handler runs, and htmx
// surfaces the error inline.
func (s *Server) handleAgentControlClaim(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Agent not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting agent bead for control claim", "id", id, "error", err)
		http.Error(w, "Failed to load agent", http.StatusInternalServerError)
		return
	}
	if bead.Type != "agent" {
		http.Error(w, "Not an agent bead", http.StatusBadRequest)
		return
	}

	email := oidcauth.EmailFromContext(r.Context())
	s.logger.Info("agent_terminal_claim",
		"event", "agent_terminal_claim",
		"agent", id,
		"operator", email)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fragment := renderAgentControlToolbar(s.basePath, bead.ID, email)
	if _, err := w.Write([]byte(fragment)); err != nil {
		s.logger.Debug("write control claim fragment", "error", err)
	}
}

// renderAgentControlToolbar produces the htmx swap fragment shown after a
// successful "Take control" claim. It carries the input form pre-armed with
// hx-post to /agents/{id}/input plus a small "controlled by" stamp so the
// page makes the take-control state visible.
func renderAgentControlToolbar(basePath, beadID, email string) string {
	postURL := basePath + "/agents/" + beadID + "/input"
	var sb strings.Builder
	sb.WriteString(`<div id="agent-control" class="agent-control agent-control-active" data-controlled-by="`)
	sb.WriteString(html.EscapeString(email))
	sb.WriteString(`">`)
	sb.WriteString(`<form class="agent-input-form" hx-post="`)
	sb.WriteString(html.EscapeString(postURL))
	sb.WriteString(`" hx-swap="none">`)
	sb.WriteString(`<label for="agent-input-text" class="agent-input-label">Send text</label>`)
	sb.WriteString(`<textarea id="agent-input-text" class="empty-prompt" name="text" rows="2" maxlength="`)
	sb.WriteString(itoaSafe(maxAgentInputLen))
	sb.WriteString(`" placeholder="`)
	sb.WriteString(html.EscapeString(gardenhub.EmptyPrompt))
	sb.WriteString(`" required></textarea>`)
	sb.WriteString(`<button type="submit" class="agent-input-send">Send</button>`)
	sb.WriteString(`</form>`)
	if email != "" {
		sb.WriteString(`<p class="agent-control-stamp">controlled by <code>`)
		sb.WriteString(html.EscapeString(email))
		sb.WriteString(`</code></p>`)
	}
	sb.WriteString(`</div>`)
	return sb.String()
}

// itoaSafe is a tiny helper so we don't pull in strconv just for the maxlength
// attribute. The bound is a const so the value range is known at build time.
func itoaSafe(n int) string {
	if n <= 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// agentInputSnippet truncates a forwarded input string for the audit log so
// large pastes don't blow up structured logging output. Newlines are replaced
// with spaces; trailing characters past the limit are elided with "…".
func agentInputSnippet(text string, n int) string {
	t := strings.ReplaceAll(text, "\n", " ")
	t = strings.ReplaceAll(t, "\r", " ")
	if len(t) <= n {
		return t
	}
	return t[:n] + "…"
}
