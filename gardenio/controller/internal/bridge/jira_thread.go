// Package bridge — agent-initiated Jira thread read endpoint.
//
// GET /api/jira/thread is the jira-bridge analog of the slack-bridge's
// GET /api/slack/threads (feature G1, epic ka-0000007401). A Jira-bound
// agent's `gb jira thread` fetches the recent conversation on the ticket the
// agent was spawned from, so it can re-read the room MID-SESSION — the F4
// spawn-prompt history is a one-shot snapshot and goes stale as the ticket
// conversation continues.
//
// Security invariants (mirrors /api/jira/reply — do not weaken):
//
//   - Bound-ticket scoping: the read target is resolved from the acting
//     agent's OWN jira_key binding (agent bead field, else notes). There is
//     no way to name an arbitrary ticket — an agent can only ever read the
//     ticket it is bound to, so pods never need raw JIRA_* credentials.
//   - Bearer auth: the same JIRA_BRIDGE_AGENT_TOKEN gate as the reply
//     endpoint applies (empty token = local dev, unauthenticated).
//   - This is a READ of a ticket whose content the agent already received at
//     spawn time (F4 history), so the outbound WritePolicyStore (which gates
//     writes) is not consulted; nothing here can post to Jira.
package bridge

import (
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// jiraThreadDefaultLimit is how many recent comments a thread read returns
// when the caller does not ask for a specific limit.
const jiraThreadDefaultLimit = 20

// jiraThreadMaxLimit caps a caller-supplied limit so one request cannot ask
// Jira for an unbounded page.
const jiraThreadMaxLimit = 100

// jiraThreadComment is one comment in a thread-read response, flattened to
// plain text (ADF or Jira Server string bodies both normalize through
// jiraCommentBodyToText).
type jiraThreadComment struct {
	ID      string `json:"id"`
	Author  string `json:"author"`
	Created string `json:"created"`
	Text    string `json:"text"`
}

// jiraThreadResponse is the response body for GET /api/jira/thread. Comments
// are ordered oldest first, ready for context injection.
type jiraThreadResponse struct {
	OK       bool                `json:"ok"`
	JiraKey  string              `json:"jira_key,omitempty"`
	Comments []jiraThreadComment `json:"comments,omitempty"`
	Error    string              `json:"error,omitempty"`
}

// handleGetThread handles GET /api/jira/thread?agent=<name>[&limit=<n>].
func (a *JiraReplyAPI) handleGetThread(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJiraThreadError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if !a.authorized(r) {
		writeJiraThreadError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}

	if a.history == nil {
		writeJiraThreadError(w, http.StatusNotImplemented, "thread reads are not configured on this bridge")
		return
	}

	agent := strings.TrimSpace(r.URL.Query().Get("agent"))
	if agent == "" {
		writeJiraThreadError(w, http.StatusBadRequest, "agent is required")
		return
	}

	limit := jiraThreadDefaultLimit
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			writeJiraThreadError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		limit = min(n, jiraThreadMaxLimit)
	}

	agentBead, err := a.daemon.FindAgentBead(r.Context(), agent)
	if err != nil {
		a.logger.Warn("jira-thread: agent bead not found", "agent", agent, "error", err)
		writeJiraThreadError(w, http.StatusNotFound, "agent not found")
		return
	}

	// Bound-ticket scoping: same binding resolution as the reply endpoint.
	jiraKey := agentBead.Fields["jira_key"]
	if jiraKey == "" {
		if notes := beadsapi.ParseNotes(agentBead.Notes); notes != nil {
			jiraKey = notes["jira_key"]
		}
	}
	if jiraKey == "" {
		writeJiraThreadError(w, http.StatusForbidden, "agent has no jira_key binding")
		return
	}

	comments, err := a.history.GetComments(r.Context(), jiraKey, limit)
	if err != nil {
		a.logger.Error("jira-thread: failed to fetch comments",
			"agent", agent, "jira_key", jiraKey, "error", err)
		writeJiraThreadError(w, http.StatusBadGateway, "failed to fetch comments")
		return
	}

	// GetComments returns newest first; flip to oldest first for reading.
	out := make([]jiraThreadComment, 0, len(comments))
	for _, c := range slices.Backward(comments) {
		text := strings.TrimSpace(jiraCommentBodyToText(c.Body))
		if text == "" {
			continue
		}
		author := c.Author.DisplayName
		if author == "" {
			author = "unknown"
		}
		out = append(out, jiraThreadComment{
			ID:      c.ID,
			Author:  author,
			Created: c.Created,
			Text:    text,
		})
	}

	a.logger.Info("jira-thread: served thread read",
		"agent", agent, "jira_key", jiraKey, "comments", len(out))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jiraThreadResponse{OK: true, JiraKey: jiraKey, Comments: out})
}

// writeJiraThreadError writes a JSON error response with the given status.
func writeJiraThreadError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(jiraThreadResponse{OK: false, Error: msg})
}
