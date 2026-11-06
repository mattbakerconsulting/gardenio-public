package main

import (
	"errors"
	"html"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/coopmuxapi"
	"gardenio/controller/internal/glass"
	"gardenio/controller/internal/oidcauth"
)

// Phase 3 widens the agent terminal write surface from "send text" to the
// full operator vocabulary: named keys (Esc/Tab/Enter/arrows + control
// chords), a re-prompt nudge, and an answer-the-prompt respond. Each verb
// reuses the Phase 2 audit-log shape so a forensic trace stitches together
// across surfaces:
//
//	event=agent_terminal_write subverb=keys|input|nudge|respond
//
// Destructive shortcuts (Ctrl-C, Ctrl-D, Ctrl-Z, Esc) round-trip through
// a server-rendered confirmation fragment. The first POST returns the
// fragment (no upstream call); the operator then either Confirms (standard
// write line) or Cancels (audit line with result=ux_canceled, no upstream
// call). hx-confirm is not enough — the message has to show the bead id +
// session id so an operator pasting many tabs together knows which agent
// they are about to interrupt.

// maxAgentNudgeLen / maxAgentRespondTextLen / maxAgentKeysCount cap the
// shape of operator-supplied bodies. Same rationale as maxAgentInputLen:
// protect the audit log and the agent's input buffer from a runaway form.
const (
	maxAgentNudgeLen       = 4 * 1024 // the Glass caps its nudges the same (gardenhub glassNudgeMax)
	maxAgentRespondTextLen = 8 * 1024
	maxAgentKeysCount      = 32
	maxAgentKeyLen         = 32
)

// isDestructiveKey reports whether any key in keys requires confirmation:
// the Glass's list (Ctrl-C, Ctrl-D, Ctrl-Z, Esc), so the agent page and the
// Glass ask before the same keys. One destructive key in a multi-key burst
// poisons the whole batch — the operator confirms the whole batch or it goes
// nowhere.
func isDestructiveKey(keys []string) bool { return glass.IsDestructiveKey(keys) }

// parseAgentKeysCSV pulls a comma-separated key list from a form field
// (toolbar form value `keys=Esc,Tab,Enter,...`). Empty entries and
// over-long entries are rejected so a stray comma doesn't paper-cut the
// upstream call.
func parseAgentKeysCSV(raw string) ([]string, error) {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		k := strings.TrimSpace(p)
		if k == "" {
			continue
		}
		if len(k) > maxAgentKeyLen {
			return nil, errors.New("key name too long")
		}
		out = append(out, k)
	}
	if len(out) == 0 {
		return nil, errors.New("no keys")
	}
	if len(out) > maxAgentKeysCount {
		return nil, errors.New("too many keys")
	}
	return out, nil
}

// agentWriteContext is the shared preamble for every Phase 3 write
// handler: load the bead, verify it's an agent, pull its coopmux session id, and
// short-circuit on the coopmux disabled-mode. Returning the resolved
// session id keeps the per-handler bodies tight.
func (s *Server) agentWriteContext(w http.ResponseWriter, r *http.Request) (id, sessionID string, ok bool) {
	id = r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return "", "", false
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		var apiErr *beadsapi.APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			http.Error(w, "Agent not found", http.StatusNotFound)
			return "", "", false
		}
		s.logger.Error("getting agent bead for write", "id", id, "error", err)
		http.Error(w, "Failed to load agent", http.StatusInternalServerError)
		return "", "", false
	}
	if bead.Type != "agent" {
		http.Error(w, "Not an agent bead", http.StatusBadRequest)
		return "", "", false
	}
	sessionID = bead.MuxSessionID()
	if sessionID == "" {
		http.Error(w, "Agent has no live session", http.StatusConflict)
		return "", "", false
	}
	if s.coopmux == nil || s.coopmux.Disabled() {
		http.Error(w, "coopmux unavailable in this deployment", http.StatusServiceUnavailable)
		return "", "", false
	}
	return id, sessionID, true
}

// translateUpstreamWriteError maps coopmuxapi error categories onto the
// HTTP error codes the htmx layer expects. The audit-log lines for the
// upstream-not-found case live with the caller so each verb can stamp its
// own subverb tag.
func translateUpstreamWriteError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, coopmuxapi.ErrSessionNotFound):
		http.Error(w, "session not found upstream", http.StatusNotFound)
	case errors.Is(err, coopmuxapi.ErrDisabled):
		http.Error(w, "coopmux unavailable in this deployment", http.StatusServiceUnavailable)
	default:
		http.Error(w, "Failed to forward to agent", http.StatusBadGateway)
	}
}

// handleAgentKeys forwards a CSV `keys` form to coopmux's /input/keys
// endpoint. Destructive keys (Ctrl-C, Ctrl-D, Ctrl-Z, Esc) short-circuit
// to an inline confirmation fragment unless the operator already confirmed
// (form field `confirmed=1`). Confirmed batches are written as-is and
// emit the standard audit line.
func (s *Server) handleAgentKeys(w http.ResponseWriter, r *http.Request) {
	id, sessionID, ok := s.agentWriteContext(w, r)
	if !ok {
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	keys, err := parseAgentKeysCSV(r.PostForm.Get("keys"))
	if err != nil {
		http.Error(w, "keys is required", http.StatusBadRequest)
		return
	}
	confirmed := r.PostForm.Get("confirmed") == "1"

	email := oidcauth.EmailFromContext(r.Context())
	roles := oidcauth.RolesFromContext(r.Context())

	if isDestructiveKey(keys) && !confirmed {
		s.logger.Info("agent_terminal_write",
			"event", "agent_terminal_write",
			"subverb", "keys",
			"agent", id,
			"session", sessionID,
			"operator", email,
			"roles", roles,
			"keys", strings.Join(keys, ","),
			"result", "confirm_required")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fragment := renderAgentConfirmDestructive(s.basePath, id, sessionID, keys)
		if _, werr := w.Write([]byte(fragment)); werr != nil {
			s.logger.Debug("write destructive-confirm fragment", "error", werr)
		}
		return
	}

	if err := s.coopmux.SendKeys(r.Context(), sessionID, keys); err != nil {
		s.logger.Error("agent_terminal_write: coopmux SendKeys failed",
			"agent", id, "session", sessionID, "operator", email, "keys", keys, "error", err)
		translateUpstreamWriteError(w, err)
		return
	}

	s.logger.Info("agent_terminal_write",
		"event", "agent_terminal_write",
		"subverb", "keys",
		"agent", id,
		"session", sessionID,
		"operator", email,
		"roles", roles,
		"keys", strings.Join(keys, ","),
		"confirmed", confirmed,
		"result", "ok")

	// On confirmed destructive batches, htmx swaps the confirm fragment
	// out for an empty toolbar — a 200 with empty body lets the swap
	// succeed cleanly. On normal toolbar clicks the request was hx-swap=none
	// so the response body is ignored.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// handleAgentKeysCancel records the operator's cancel of a destructive
// confirmation. No upstream call. The audit line carries result=ux_canceled
// so a forensic trace can pair every confirm-required line with either an
// ok or a ux_canceled line.
func (s *Server) handleAgentKeysCancel(w http.ResponseWriter, r *http.Request) {
	id, sessionID, ok := s.agentWriteContext(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	keys, _ := parseAgentKeysCSV(r.PostForm.Get("keys"))

	email := oidcauth.EmailFromContext(r.Context())
	roles := oidcauth.RolesFromContext(r.Context())

	s.logger.Info("agent_terminal_write",
		"event", "agent_terminal_write",
		"subverb", "keys",
		"agent", id,
		"session", sessionID,
		"operator", email,
		"roles", roles,
		"keys", strings.Join(keys, ","),
		"result", "ux_canceled")

	// htmx swap target is the same #agent-keys-confirm slot; an empty body
	// removes the confirmation fragment from the page.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
}

// handleAgentNudge takes a `message` form and nudges the agent the Glass's
// way (Lands workbench-glass GL-7, step 6.5): through the bridge's NudgeAgent
// rather than coopmux's PTY nudge, written into the agent's Commons thread,
// and audited as agent_terminal_write subverb=nudge surface=glass. It is the
// garden's handler (gardenhub GlassNudge), so the two cannot disagree; a
// viewer with no Glass wired answers 503. Empty messages are refused:
// operators are expected to type a hint, not punch the button blindly.
func (s *Server) handleAgentNudge(w http.ResponseWriter, r *http.Request) {
	if s.garden == nil || s.garden.Glass == nil || s.garden.Glass.Nudge == nil {
		http.Error(w, "nudges are not enabled in this deployment", http.StatusServiceUnavailable)
		return
	}
	r.SetPathValue("agent", r.PathValue("id"))
	s.garden.GlassNudge(w, r)
}

// handleAgentRespond forwards an `accept` / `text` / `option` form to
// coopmux's /agent/respond. At least one of the three has to be set; the
// server does NOT cross-check that the answer matches the agent's current
// prompt shape — that's coopmux's job. Form field shapes:
//
//	accept=yes|no   -> Accept = &true | &false
//	text=...        -> Text   = &"..."
//	option=N        -> Option = &N (signed)
//
// If multiple are set we forward all of them and let coopmux decide. The
// modal in type_agent.html only enables one path at a time so this is
// belt-and-suspenders.
func (s *Server) handleAgentRespond(w http.ResponseWriter, r *http.Request) {
	id, sessionID, ok := s.agentWriteContext(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}

	var req coopmuxapi.RespondRequest
	if v := strings.TrimSpace(r.PostForm.Get("accept")); v != "" {
		switch strings.ToLower(v) {
		case "yes", "true", "1", "accept":
			t := true
			req.Accept = &t
		case "no", "false", "0", "reject", "decline":
			f := false
			req.Accept = &f
		default:
			http.Error(w, "accept must be yes or no", http.StatusBadRequest)
			return
		}
	}
	if v := r.PostForm.Get("text"); v != "" {
		if len(v) > maxAgentRespondTextLen {
			http.Error(w, "text exceeds maximum length", http.StatusRequestEntityTooLarge)
			return
		}
		text := v
		req.Text = &text
	}
	if v := strings.TrimSpace(r.PostForm.Get("option")); v != "" {
		n, err := atoiSigned(v)
		if err != nil {
			http.Error(w, "option must be an integer", http.StatusBadRequest)
			return
		}
		req.Option = &n
	}
	if req.Accept == nil && req.Text == nil && req.Option == nil {
		http.Error(w, "respond requires accept, text, or option", http.StatusBadRequest)
		return
	}

	email := oidcauth.EmailFromContext(r.Context())
	roles := oidcauth.RolesFromContext(r.Context())

	if err := s.coopmux.Respond(r.Context(), sessionID, req); err != nil {
		s.logger.Error("agent_terminal_write: coopmux Respond failed",
			"agent", id, "session", sessionID, "operator", email, "error", err)
		translateUpstreamWriteError(w, err)
		return
	}

	textLen := 0
	if req.Text != nil {
		textLen = len(*req.Text)
	}
	s.logger.Info("agent_terminal_write",
		"event", "agent_terminal_write",
		"subverb", "respond",
		"agent", id,
		"session", sessionID,
		"operator", email,
		"roles", roles,
		"has_accept", req.Accept != nil,
		"has_text", req.Text != nil,
		"text_len", textLen,
		"has_option", req.Option != nil,
		"result", "ok")

	w.WriteHeader(http.StatusNoContent)
}

// renderAgentConfirmDestructive builds the inline confirmation fragment
// shown when an operator clicks a destructive key. The fragment is a
// server-rendered <div id="agent-keys-confirm"> that swaps in over the
// keys toolbar's own confirm slot. Cancel POSTs to the cancel endpoint
// (audit line, no upstream); Confirm POSTs back to /keys with
// confirmed=1.
func renderAgentConfirmDestructive(basePath, beadID, sessionID string, keys []string) string {
	postURL := basePath + "/agents/" + beadID + "/keys"
	cancelURL := basePath + "/agents/" + beadID + "/keys/cancel"
	keysCSV := strings.Join(keys, ",")

	var sb strings.Builder
	sb.WriteString(`<div id="agent-keys-confirm" class="agent-keys-confirm" role="alertdialog" aria-live="assertive">`)
	sb.WriteString(`<p class="agent-keys-confirm-msg">`)
	sb.WriteString(`Send <strong>`)
	sb.WriteString(html.EscapeString(keysCSV))
	sb.WriteString(`</strong> to agent <code>`)
	sb.WriteString(html.EscapeString(beadID))
	sb.WriteString(`</code> session <code>`)
	sb.WriteString(html.EscapeString(sessionID))
	sb.WriteString(`</code>? This may interrupt the running process.`)
	sb.WriteString(`</p>`)
	sb.WriteString(`<form class="agent-keys-confirm-actions" hx-post="`)
	sb.WriteString(html.EscapeString(postURL))
	sb.WriteString(`" hx-target="#agent-keys-confirm" hx-swap="outerHTML">`)
	sb.WriteString(`<input type="hidden" name="keys" value="`)
	sb.WriteString(html.EscapeString(keysCSV))
	sb.WriteString(`">`)
	sb.WriteString(`<input type="hidden" name="confirmed" value="1">`)
	sb.WriteString(`<button type="submit" class="agent-keys-confirm-yes">Confirm send</button>`)
	sb.WriteString(`</form>`)
	sb.WriteString(`<form class="agent-keys-confirm-cancel-form" hx-post="`)
	sb.WriteString(html.EscapeString(cancelURL))
	sb.WriteString(`" hx-target="#agent-keys-confirm" hx-swap="outerHTML">`)
	sb.WriteString(`<input type="hidden" name="keys" value="`)
	sb.WriteString(html.EscapeString(keysCSV))
	sb.WriteString(`">`)
	sb.WriteString(`<button type="submit" class="agent-keys-confirm-no">Cancel</button>`)
	sb.WriteString(`</form>`)
	sb.WriteString(`</div>`)
	return sb.String()
}

// atoiSigned parses a signed integer string. We don't pull strconv just for
// this — the option range is small enough that hand-parsing is fine and
// matches the itoaSafe pattern in agent_input.go.
func atoiSigned(s string) (int, error) {
	if s == "" {
		return 0, errors.New("empty")
	}
	neg := false
	i := 0
	if s[0] == '-' {
		neg = true
		i = 1
	} else if s[0] == '+' {
		i = 1
	}
	if i == len(s) {
		return 0, errors.New("no digits")
	}
	n := 0
	for ; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, errors.New("not a digit")
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		n = -n
	}
	return n, nil
}
