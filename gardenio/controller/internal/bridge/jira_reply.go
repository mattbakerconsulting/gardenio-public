// Package bridge — agent-initiated Jira reply endpoint.
//
// JiraReplyAPI serves POST /api/jira/reply, the jira-bridge analog of the
// slack-bridge's /api/slack/threads/reply (feature F2, plan ka-0000007401).
// A Jira-bound agent's `gb jira reply` posts a comment on the ticket the agent
// was spawned from, giving the same conversational back-channel the Slack
// thread reply provides.
//
// Security invariants (F5 spine — do not weaken):
//
//   - Bound-ticket scoping: the comment target is resolved from the acting
//     agent's OWN jira_key binding (agent bead field, else notes). A request
//     naming a different ticket is rejected — the endpoint can only ever
//     comment on the agent's own bound ticket, so "target == bound" holds by
//     construction (same contract as JiraSquawkForwarder).
//   - Hard project gate: WritePolicyStore.AllowProject is checked FIRST and is
//     mode-independent (never softened by shadow mode). FSM is not a writable
//     project in any toggle state (epic ka-0000002067), so an FSM-bound reply
//     is dropped even when the policy runs in shadow mode.
//   - Role + visibility: the full AllowComment verdict (project + role +
//     visibility) then applies. Internal notes are the default; a public
//     (customer-facing) reply is opt-in AND requires the role to carry
//     AllowPublicComment — which no role grants by default.
//   - A nil WritePolicyStore denies everything (fail closed).
package bridge

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// jiraReplyMaxLen is the maximum length for a reply comment body, matching the
// squawk-forward cap so both outbound comment paths truncate identically.
const jiraReplyMaxLen = 4000

// JiraReplyJiraClient is the subset of JiraClient used by JiraReplyAPI: the
// squawk-forwarder's visibility-aware comment method plus the source-linked
// variant that anchors a reply to the @gardenio mention comment it answers
// (G4b quote-link fallback, ka-0000002864) and its markdown-rendering twin
// (H1, ka-0000004233) the reply path posts through.
type JiraReplyJiraClient interface {
	JiraSquawkJiraClient
	AddCommentWithVisibilityAndSourceLink(ctx context.Context, key, text string, vis CommentVisibility, sourceCommentID string) error
	AddMarkdownCommentWithVisibilityAndSourceLink(ctx context.Context, key, text string, vis CommentVisibility, sourceCommentID string) error
}

// JiraReplyAPI serves the agent-facing Jira reply endpoint. It reuses the
// squawk-forwarder client subsets: the beads daemon for agent-bead resolution
// and the visibility-aware Jira comment method.
type JiraReplyAPI struct {
	daemon      JiraSquawkClient
	jira        JiraReplyJiraClient
	history     JiraCommentHistoryClient
	issues      JiraIssueReader
	writePolicy *WritePolicyStore
	agentToken  string
	logger      *slog.Logger
	// dedup collapses identical reply comments posted within its window so a
	// transient-error retry (classifyReplyPostError tells agents to retry) or a
	// lost-200 re-invocation never posts a duplicate on a possibly
	// customer-facing ticket (ka-0000007243, Slack threadReplyDeduper parity).
	dedup *jiraReplyDeduper
}

// JiraReplyAPIConfig holds configuration for the reply endpoint.
type JiraReplyAPIConfig struct {
	Daemon JiraSquawkClient
	Jira   JiraReplyJiraClient
	// History serves GET /api/jira/thread reads of the agent's bound ticket
	// (feature G1, epic ka-0000007401). Nil disables thread reads (the
	// endpoint answers 501) without affecting replies.
	History JiraCommentHistoryClient
	// Issues serves GET /api/jira/issue reads of the agent's bound ticket body
	// (feature G6, epic ka-0000007401) — the ticket-body analog of History.
	// Nil disables issue reads (the endpoint answers 501) without affecting
	// replies or thread reads.
	Issues JiraIssueReader
	Logger *slog.Logger
	// WritePolicy is the shared config-driven write policy (ka-0000007252).
	// The production jira-bridge threads the SAME store into every outbound
	// path so the FSM block and role tiers stay atomic. nil denies all writes.
	WritePolicy *WritePolicyStore
	// AgentToken, when non-empty, requires `Authorization: Bearer <token>` on
	// every reply request (mirrors the slack-bridge's SLACK_BRIDGE_AGENT_TOKEN
	// agent-endpoint auth). Empty preserves local-dev behavior (no auth).
	AgentToken string
}

// NewJiraReplyAPI creates the reply endpoint handler.
func NewJiraReplyAPI(cfg JiraReplyAPIConfig) *JiraReplyAPI {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &JiraReplyAPI{
		daemon:      cfg.Daemon,
		jira:        cfg.Jira,
		history:     cfg.History,
		issues:      cfg.Issues,
		writePolicy: cfg.WritePolicy,
		agentToken:  cfg.AgentToken,
		logger:      logger,
		dedup:       newJiraReplyDeduper(jiraReplyDedupWindow),
	}
}

// RegisterRoutes registers the agent-facing endpoints on the given mux.
func (a *JiraReplyAPI) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/api/jira/reply", a.handlePostReply)
	mux.HandleFunc("/api/jira/thread", a.handleGetThread)
	mux.HandleFunc("/api/jira/issue", a.handleGetIssue)
}

// authorized checks the request's bearer token against the configured agent
// token. An empty configured token means no auth (local dev).
func (a *JiraReplyAPI) authorized(r *http.Request) bool {
	if a.agentToken == "" {
		return true
	}
	got := ""
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		got = strings.TrimSpace(tok)
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(a.agentToken)) == 1
}

// jiraReplyRequest is the request body for POST /api/jira/reply.
type jiraReplyRequest struct {
	// Agent is the acting agent's name; the bridge resolves its agent bead for
	// the jira_key binding, role, and operator attribution.
	Agent string `json:"agent"`
	// JiraKey optionally names the target ticket. It must match the agent's
	// bound ticket; empty means "the bound ticket".
	JiraKey string `json:"jira_key"`
	Text    string `json:"text"`
	// Visibility is "internal" (default) or "public".
	Visibility string `json:"visibility"`
}

// jiraReplyResponse is the response body for POST /api/jira/reply.
type jiraReplyResponse struct {
	OK      bool   `json:"ok"`
	JiraKey string `json:"jira_key,omitempty"`
	Error   string `json:"error,omitempty"`
}

// handlePostReply handles POST /api/jira/reply.
func (a *JiraReplyAPI) handlePostReply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJiraReplyError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if !a.authorized(r) {
		writeJiraReplyError(w, http.StatusUnauthorized, "missing or invalid bearer token")
		return
	}

	var req jiraReplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJiraReplyError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	req.Agent = strings.TrimSpace(req.Agent)
	req.Text = strings.TrimSpace(req.Text)
	if req.Agent == "" || req.Text == "" {
		writeJiraReplyError(w, http.StatusBadRequest, "agent and text are required")
		return
	}

	vis := CommentVisibility(strings.TrimSpace(req.Visibility))
	if vis == "" {
		vis = CommentInternal
	}
	if !knownCommentVisibilities[vis] {
		writeJiraReplyError(w, http.StatusBadRequest, "visibility must be \"internal\" or \"public\"")
		return
	}

	agentBead, err := a.daemon.FindAgentBead(r.Context(), req.Agent)
	if err != nil {
		a.logger.Warn("jira-reply: agent bead not found", "agent", req.Agent, "error", err)
		writeJiraReplyError(w, http.StatusNotFound, "agent not found")
		return
	}

	jiraKey, status, msg := a.resolveReplyTarget(r.Context(), agentBead, req)
	if status != 0 {
		recordJiraReply(jiraReplyBlocked, vis)
		writeJiraReplyError(w, status, msg)
		return
	}

	text := req.Text
	if len(text) > jiraReplyMaxLen {
		text = text[:jiraReplyMaxLen-3] + "..."
	}

	// Format like the squawk forwarder, with the same operator attribution
	// trailer ("Acted by <operator>") appended AFTER truncation so it is never
	// clipped (ka-0000006003).
	body := "[gardenio agent] " + req.Agent + ":\n\n" + text
	operatedBy := agentBead.OperatedBy
	if operatedBy == "" {
		operatedBy = agentBead.Fields["operated_by"]
	}
	body = withAttribution(body, operatorAttributionLine(operatedBy, agentBead.OperatedByDisplay))

	// Quote-link fallback (G4b, ka-0000002864): true Jira comment threading is
	// impossible (no parentId on comment create — G4 finding, ka-0000001458),
	// so anchor the reply to the @gardenio mention comment that spawned the
	// agent (jira_comment_id, stamped by F3 on mention-spawn/respawn) with a
	// prepended focusedCommentId link paragraph. Agents without the field
	// (poller-spawned, manual binding) post the flat comment unchanged.
	//
	// The markdown variant (H1, ka-0000004233) typesets the agent's markdown
	// as rich ADF; unmappable constructs degrade inside the client to the
	// plain-text comment this path posted before H1.
	sourceCommentID := agentBead.Fields["jira_comment_id"]

	// Dedup guard (ka-0000007243, Slack threadReplyDeduper parity): reserve the
	// (agent, jira_key, visibility, body) slot BEFORE posting so a transient-
	// error retry or a lost-200 re-invocation never posts a DUPLICATE comment
	// on a possibly customer-facing ticket. Key on the fully-assembled `body`
	// (attribution + quote-link already folded in) so two replies that render
	// identically collapse and two that differ never do. A collapsed retry
	// reports success WITHOUT posting — the same idempotent contract the Slack
	// reply endpoint returns.
	dedupKey, dup := a.dedup.begin(req.Agent, jiraKey, vis, body)
	if dup {
		a.logger.Info("jira-reply: collapsed duplicate reply",
			"agent", req.Agent, "jira_key", jiraKey, "visibility", vis)
		// An idempotent dedup-collapse is a user-visible "reply landed" (the
		// original comment is already on the ticket) — count it as posted so
		// the reply-volume series matches what the operator sees.
		recordJiraReply(jiraReplyPosted, vis)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jiraReplyResponse{OK: true, JiraKey: jiraKey})
		return
	}

	if err := a.jira.AddMarkdownCommentWithVisibilityAndSourceLink(r.Context(), jiraKey, body, vis, sourceCommentID); err != nil {
		// Release the reservation so a legitimate retry of this reply — which
		// classifyReplyPostError explicitly invites on a transient failure — is
		// not swallowed by a slot that never produced a Jira comment.
		a.dedup.abort(dedupKey)
		status, msg := classifyReplyPostError(err)
		recordJiraReply(jiraReplyError, vis)
		a.logger.Error("jira-reply: failed to post JIRA comment",
			"agent", req.Agent, "jira_key", jiraKey, "visibility", vis, "status", status, "error", err)
		writeJiraReplyError(w, status, msg)
		return
	}
	a.dedup.commit(dedupKey)

	recordJiraReply(jiraReplyPosted, vis)
	a.logger.Info("jira-reply: posted JIRA comment",
		"agent", req.Agent, "jira_key", jiraKey, "visibility", vis, "text_length", len(text))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jiraReplyResponse{OK: true, JiraKey: jiraKey})
}

// resolveReplyTarget resolves and gates the comment target for a reply. It
// returns the bound jira key on success; otherwise a non-zero HTTP status and
// error message. Every rejection here fails closed.
func (a *JiraReplyAPI) resolveReplyTarget(_ context.Context, agentBead *beadsapi.BeadDetail, req jiraReplyRequest) (string, int, string) {
	// Bound-ticket scoping: the target comes from the agent's own binding.
	jiraKey := agentBead.Fields["jira_key"]
	if jiraKey == "" {
		if notes := beadsapi.ParseNotes(agentBead.Notes); notes != nil {
			jiraKey = notes["jira_key"]
		}
	}
	if jiraKey == "" {
		return "", http.StatusForbidden, "agent has no jira_key binding"
	}
	if req.JiraKey != "" && !strings.EqualFold(strings.TrimSpace(req.JiraKey), jiraKey) {
		a.logger.Warn("jira-reply: blocked reply to non-bound ticket",
			"agent", req.Agent, "requested", req.JiraKey, "bound", jiraKey)
		return "", http.StatusForbidden, "jira_key does not match the agent's bound ticket"
	}

	// Hard project pre-gate — mode-independent, exactly like the squawk
	// forwarder. AllowReplyProject never applies shadow-mode softening, so an
	// FSM/unknown project (absent from the policy) is rejected here even when the
	// store runs in shadow mode. It uses the SPAWN predicate (projectSpawnable,
	// SyncToggle ignored) rather than the write predicate: a bound-ticket
	// internal reply is not an FSM/outbox write, so it must not be coupled to the
	// outbox toggle — the same false coupling AllowSpawn removed for the spawn
	// path (ka-0000001940, extended to reply in ka-0000007628). GBFSM is therefore
	// reply-eligible in both toggle states; FSM/unknown stay denied. A nil store
	// denies everything.
	if a.writePolicy == nil || !a.writePolicy.AllowReplyProject(jiraKey) {
		a.logger.Warn("jira-reply: blocked reply on non-replyable project",
			"agent", req.Agent, "jira_key", jiraKey,
			"project_key", jiraProjectKey(jiraKey))
		return "", http.StatusForbidden, "project is not replyable"
	}

	// Full reply-policy verdict: project + role + visibility. AllowReplyComment
	// mirrors AllowComment but likewise relaxes ONLY the project axis to the
	// spawn predicate (so the toggle-off GBFSM outbox re-check in decide() cannot
	// independently re-block the reply); role gating and the AllowPublicComment
	// ceiling are unchanged, so public replies still require a role grant no
	// shipped role holds — internal notes are the ceiling for autonomous agents.
	role := agentBead.Fields["role"]
	vis := CommentVisibility(strings.TrimSpace(req.Visibility))
	if vis == "" {
		vis = CommentInternal
	}
	if !a.writePolicy.AllowReplyComment(jiraKey, role, vis) {
		a.logger.Warn("jira-reply: blocked comment by write policy",
			"agent", req.Agent, "jira_key", jiraKey, "role", role, "visibility", vis)
		return "", http.StatusForbidden, "comment not permitted by write policy"
	}
	return jiraKey, 0, ""
}

// classifyReplyPostError maps a failed comment POST to the HTTP status the
// reply endpoint should return, distinguishing a CONTENT rejection from an
// upstream OUTAGE.
//
// Before this, every post failure became a blanket 502 (ka-0000000661). That
// conflated two very different failures: Jira returning 4xx because the ADF
// body itself was invalid (a deterministic, content-dependent failure the
// caller fixes by re-posting plain text) vs. Jira being genuinely unreachable
// or 5xxing (a transient outage worth a retry). A 502 reads as "the bridge is
// down", which pushes agents onto the direct-API fallback (advice
// ka-0000001317) instead of simply downgrading to plain text.
//
// So: a Jira 4xx (except 429, which is real throttling worth a 502-style retry)
// becomes 422 Unprocessable Entity with an actionable message — the render was
// rejected, retry without markdown. Everything else (Jira 5xx, 429, transport
// errors, non-Jira errors) stays 502.
func classifyReplyPostError(err error) (int, string) {
	if apiErr, ok := asJiraAPIError(err); ok {
		if apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 && apiErr.StatusCode != http.StatusTooManyRequests {
			return http.StatusUnprocessableEntity,
				"JIRA rejected the comment body (likely unrenderable markdown); retry as plain text"
		}
	}
	return http.StatusBadGateway, "failed to post comment"
}

// writeJiraReplyError writes a JSON error response with the given status.
func writeJiraReplyError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(jiraReplyResponse{OK: false, Error: msg})
}
