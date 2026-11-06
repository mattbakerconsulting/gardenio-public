package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"

	"gardenio/kbeads/internal/eventbus"
	"gardenio/kbeads/internal/hooks"
	"gardenio/kbeads/internal/presence"
)

// hookExecuteAllowlistConfigKey is the config key that stores the set of
// triggers non-admin operators are permitted to invoke via
// POST /v1/hooks/execute. Value format: {"triggers": ["session-end", ...]}.
const hookExecuteAllowlistConfigKey = "hook-execute-allowlist"

// hookExecuteAllowlist is the JSON shape of the hook-execute-allowlist config.
type hookExecuteAllowlist struct {
	Triggers []string `json:"triggers"`
}

// isHookTriggerAllowlisted returns true when the given trigger is listed in
// the hook-execute-allowlist config. A missing or malformed config entry is
// treated as "no triggers allowlisted" (fail closed for non-admin callers).
func (s *BeadsServer) isHookTriggerAllowlisted(ctx context.Context, trigger string) bool {
	cfg, err := s.store.GetConfig(ctx, hookExecuteAllowlistConfigKey)
	if err != nil || cfg == nil || len(cfg.Value) == 0 {
		return false
	}
	var parsed hookExecuteAllowlist
	if err := json.Unmarshal(cfg.Value, &parsed); err != nil {
		return false
	}
	return slices.Contains(parsed.Triggers, trigger)
}

// hookEmitRequest is the JSON body for POST /v1/hooks/emit.
type hookEmitRequest struct {
	AgentBeadID     string `json:"agent_bead_id"`
	HookType        string `json:"hook_type"` // "Stop", "PreToolUse", etc.
	ClaudeSessionID string `json:"claude_session_id"`
	// SessionID is the harness-neutral session id (Claude session or Codex
	// thread id). Preferred over ClaudeSessionID, which is kept so older gb
	// binaries keep working.
	SessionID string `json:"session_id,omitempty"`
	// Harness is the agent CLI that produced the event ("claude" or "codex").
	Harness  string `json:"harness,omitempty"`
	CWD      string `json:"cwd"`
	Actor    string `json:"actor"`
	ToolName string `json:"tool_name,omitempty"` // tool name from Claude Code (e.g. "Bash", "Read")
}

// sessionID returns the neutral session_id, falling back to claude_session_id
// from callers that predate the harness-neutral field.
func (r hookEmitRequest) sessionID() string {
	if r.SessionID != "" {
		return r.SessionID
	}
	return r.ClaudeSessionID
}

// hookEmitResponse is the JSON response from POST /v1/hooks/emit.
type hookEmitResponse struct {
	Block    bool     `json:"block,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	Inject   string   `json:"inject,omitempty"`
}

// handleHookEmit handles POST /v1/hooks/emit.
// It evaluates gate state and soft auto-checks, returning block/warn/inject signals.
func (s *BeadsServer) handleHookEmit(w http.ResponseWriter, r *http.Request) {
	var req hookEmitRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if !s.tenantAgentHookAllowed(r.Context(), &req) {
		writeError(w, http.StatusNotFound, "agent not found")
		return
	}

	// Record presence for agent roster tracking.
	if s.Presence != nil && req.Actor != "" {
		s.Presence.RecordHookEvent(presence.HookEvent{
			Actor:     req.Actor,
			HookType:  req.HookType,
			ToolName:  req.ToolName,
			SessionID: req.sessionID(),
			CWD:       req.CWD,
		})
	}

	ctx := r.Context()
	var resp hookEmitResponse

	// If no agent_bead_id, there are no gates to check — allow.
	if req.AgentBeadID == "" {
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Ownership gate: only the agent itself, its creator/owner, or an admin
	// may emit hooks against this agent bead. Non-existent agent beads fall
	// through so existing 404-like behaviour elsewhere is preserved.
	if agentBead, err := s.store.GetBead(ctx, req.AgentBeadID); err == nil && agentBead != nil {
		if !beadMatchesTokenProject(ctx, agentBead.Labels) {
			writeError(w, http.StatusForbidden, "token not authorized for this agent bead's project")
			return
		}
		if denial := s.authorizeWriteForBead(ctx, "hook-emit", agentBead); denial != nil {
			s.auditDenial(r, denial, req.AgentBeadID)
			writeAuthzDenial(w, denial)
			return
		}
	}

	// Evaluate strict gates for Stop hook.
	if req.HookType == "Stop" {
		// Upsert the decision gate for this agent (creates pending row if not exists).
		if err := s.store.UpsertGate(ctx, req.AgentBeadID, "decision"); err != nil {
			slog.Warn("hookEmit: failed to upsert decision gate", "agent", req.AgentBeadID, "err", err)
		}

		satisfied, err := s.store.IsGateSatisfied(ctx, req.AgentBeadID, "decision")
		if err != nil {
			slog.Warn("hookEmit: failed to check decision gate", "agent", req.AgentBeadID, "err", err)
		}
		if !satisfied {
			resp.Block = true
			resp.Reason = "decision: decision point offered before session end"
			writeJSON(w, http.StatusOK, resp)
			return
		}

		// Gate is satisfied — verify it was satisfied via 'gb yield' (not a manual mark).
		// gb yield sets gate_satisfied_by=yield; gb gate mark --force sets gate_satisfied_by=operator
		// (or the legacy value manual-force for backward compatibility). An empty or unrecognized
		// value means the gate was bypassed without going through the proper yield flow, which
		// breaks the Slack bridge.
		agentBead, beadErr := s.store.GetBead(ctx, req.AgentBeadID)
		var satisfiedBy string
		if beadErr == nil && agentBead != nil && len(agentBead.Fields) > 0 {
			var fields map[string]any
			if json.Unmarshal(agentBead.Fields, &fields) == nil {
				satisfiedBy, _ = fields["gate_satisfied_by"].(string)
			}
		}
		if satisfiedBy != "yield" && satisfiedBy != "operator" && satisfiedBy != "manual-force" && satisfiedBy != "ready" {
			resp.Block = true
			resp.Reason = "decision: gate was not satisfied via 'gb yield' — create a decision with 'gb decision create' then call 'gb yield'"
			writeJSON(w, http.StatusOK, resp)
			return
		}

		// Consume the gate (reset to pending) so the next Stop blocks again.
		// Skip consume for operator overrides and ready-based satisfaction —
		// these persist across stop attempts so thread agents, operator-cleared
		// gates, and persistent agents that checked gb ready don't loop.
		if satisfiedBy != "operator" && satisfiedBy != "manual-force" && satisfiedBy != "ready" {
			if err := s.store.ClearGate(ctx, req.AgentBeadID, "decision"); err != nil {
				slog.Warn("hookEmit: failed to clear decision gate after consume", "agent", req.AgentBeadID, "err", err)
			}
			// Clear gate_satisfied_by field so it doesn't carry over to the next session.
			if err := s.mergeBeadFields(ctx, req.AgentBeadID, map[string]any{"gate_satisfied_by": nil}); err != nil {
				slog.Warn("hookEmit: failed to clear gate_satisfied_by field", "agent", req.AgentBeadID, "err", err)
			}
		}
	}

	// Soft gate AutoChecks — always warn, never block.
	if warning := hooks.CheckCommitPush(req.CWD); warning != "" {
		resp.Warnings = append(resp.Warnings, warning)
	}

	// TODO: bead-update soft check — requires checking KD_HOOK_BEAD env var from the
	// client side. Skip server-side check for now; the CLI can handle this in future.

	// Run existing advice hook logic for session-end trigger on Stop hook.
	if req.HookType == "Stop" {
		agentID := req.AgentBeadID
		if req.Actor != "" {
			agentID = req.Actor
		}
		hookResp := s.hooksHandler.HandleSessionEvent(ctx, hooks.SessionEvent{
			AgentID: agentID,
			Trigger: hooks.TriggerSessionEnd,
			CWD:     req.CWD,
		})
		if hookResp.Block && !resp.Block {
			resp.Block = true
			resp.Reason = hookResp.Reason
		}
		resp.Warnings = append(resp.Warnings, hookResp.Warnings...)
	}

	// Dispatch hook event through the bus for JetStream publishing and handler chain.
	if s.bus != nil {
		event := &eventbus.Event{
			Type:      eventbus.EventType(req.HookType),
			SessionID: req.sessionID(),
			CWD:       req.CWD,
			Actor:     req.Actor,
			ToolName:  req.ToolName,
		}
		if busResult, err := s.bus.Dispatch(ctx, event); err != nil {
			slog.Warn("hookEmit: bus dispatch error", "hook_type", req.HookType, "err", err)
		} else if busResult.Block && !resp.Block {
			resp.Block = busResult.Block
			resp.Reason = busResult.Reason
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

// hookPublishRequest is the JSON body for POST /v1/hooks/publish.
type hookPublishRequest struct {
	Subject string          `json:"subject"`
	Payload json.RawMessage `json:"payload"`
}

// handleHookPublish handles POST /v1/hooks/publish.
// It publishes a pre-built hook event payload to the NATS HOOK_EVENTS stream
// on the given subject. This allows the hook relay to publish events through
// the daemon's persistent NATS connection instead of opening a new connection
// per event.
func (s *BeadsServer) handleHookPublish(w http.ResponseWriter, r *http.Request) {
	var req hookPublishRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Subject == "" {
		writeError(w, http.StatusBadRequest, "subject is required")
		return
	}
	if len(req.Payload) == 0 {
		writeError(w, http.StatusBadRequest, "payload is required")
		return
	}

	if s.bus == nil {
		writeError(w, http.StatusServiceUnavailable, "event bus not configured")
		return
	}

	s.bus.PublishRaw(req.Subject, req.Payload)
	w.WriteHeader(http.StatusNoContent)
}

// executeHooksRequest is the JSON body for POST /v1/hooks/execute.
type executeHooksRequest struct {
	AgentID string `json:"agent_id"`
	Trigger string `json:"trigger"`
	CWD     string `json:"cwd,omitempty"`
}

// handleExecuteHooks handles POST /v1/hooks/execute.
// Agents call this to evaluate advice hooks for a given lifecycle trigger.
//
// Because advice hook commands can spawn K8s workloads and run arbitrary
// shell, this endpoint is locked down beyond the RoleOperator outer gate:
// OIDC callers must either hold RoleAdmin or invoke a trigger listed in the
// hook-execute-allowlist config. Static-token (service-to-service) callers
// retain full access since they represent the daemon's own identity.
// Every attempt — allowed or denied — is recorded to the audit log.
func (s *BeadsServer) handleExecuteHooks(w http.ResponseWriter, r *http.Request) {
	var req executeHooksRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.AgentID == "" {
		writeError(w, http.StatusBadRequest, "agent_id is required")
		return
	}
	if req.Trigger == "" {
		writeError(w, http.StatusBadRequest, "trigger is required")
		return
	}

	ctx := r.Context()
	id := AuthIdentityFromContext(ctx)
	auditBase := auditEntry{
		Actor:        id.Subject,
		Action:       "hook-execute",
		Resource:     req.Trigger,
		ResourceType: "hook",
		AuthMethod:   id.Type,
		SourceIP:     clientIP(r, s.trustedProxies),
	}

	if !s.authorizeHookExecute(ctx, req.Trigger) {
		denied := auditBase
		denied.Outcome = "deny"
		denied.Reason = "hook-execute requires admin role or allowlisted trigger"
		s.recordAuditEntry(ctx, denied)
		writeError(w, http.StatusForbidden, "hook-execute requires admin role or allowlisted trigger")
		return
	}

	allowed := auditBase
	allowed.Outcome = "allow"
	allowed.Reason = s.hookExecuteAuthzReason(ctx, req.Trigger)
	s.recordAuditEntry(ctx, allowed)

	resp := s.hooksHandler.HandleSessionEvent(ctx, hooks.SessionEvent{
		AgentID: req.AgentID,
		Trigger: req.Trigger,
		CWD:     req.CWD,
	})

	writeJSON(w, http.StatusOK, resp)
}

// authorizeHookExecute returns true when the caller is permitted to invoke
// POST /v1/hooks/execute for the given trigger. Static / fleet / unauthenticated
// callers pass through (mirrors requireRole semantics — static tokens are
// service-to-service and already fully trusted). OIDC callers must either
// hold RoleAdmin or the trigger must appear in the hook-execute-allowlist
// config. Callers that reach this function have already cleared RoleOperator
// at the mux layer, so we do not re-check that.
func (s *BeadsServer) authorizeHookExecute(ctx context.Context, trigger string) bool {
	if !roleBearing(authMethodFromContext(ctx)) {
		return true
	}
	if hasMinimumRole(RolesFromContext(ctx), RoleAdmin) {
		return true
	}
	return s.isHookTriggerAllowlisted(ctx, trigger)
}

// hookExecuteAuthzReason returns a short label describing why an allowed
// hook-execute call was permitted. Used only for audit log reason fields so
// operators can distinguish admin calls from allowlist-granted ones.
func (s *BeadsServer) hookExecuteAuthzReason(ctx context.Context, trigger string) string {
	if !roleBearing(authMethodFromContext(ctx)) {
		return "service token"
	}
	if hasMinimumRole(RolesFromContext(ctx), RoleAdmin) {
		return "admin role"
	}
	if s.isHookTriggerAllowlisted(ctx, trigger) {
		return "allowlisted trigger"
	}
	return ""
}

// tenantAgentHookAllowed confines a tenant land's agent, which scopedRouteGate
// lets reach POST /v1/hooks/emit (tenantAgentRoutes), to its own agent bead
// and its own name: it reports false for another agent's bead, the same
// whether that bead exists or not, and overwrites the body's actor with the
// token's, so presence cannot be recorded for anyone else (gd-c1DOWNYy7W).
// Every other caller is unchanged.
func (s *BeadsServer) tenantAgentHookAllowed(ctx context.Context, req *hookEmitRequest) bool {
	own, ok := agentBeadIDFromContext(ctx)
	if !ok || !s.callerIsTenant(ctx) {
		return true
	}
	if req.AgentBeadID != "" && req.AgentBeadID != own {
		return false
	}
	if name, ok := agentNameFromContext(ctx); ok {
		req.Actor = name
	}
	return true
}
