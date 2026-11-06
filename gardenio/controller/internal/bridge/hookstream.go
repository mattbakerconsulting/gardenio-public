// Package bridge provides the hook event stream subscriber.
//
// HookStream subscribes to NATS hook events (published by gb hook relay in
// agent pods) and dispatches tool use events to the AgentStreamManager for
// live streaming status updates.
package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"

	"gardenio/controller/internal/natsauth"
)

// hookEvent mirrors the hookRelayEvent schema from cmd/gb/hook_relay.go.
type hookEvent struct {
	Agent string `json:"agent"`
	// AgentBeadID is the producer's agent bead id (cmd/gb/hook_relay.go).
	// Empty for off-cluster producers and for events published before the
	// producer started stamping it, so never assume it is set. Prefer it over
	// Agent whenever a bead lookup is wanted: Agent is a sanitised display
	// name and cannot be resolved back to a bead.
	AgentBeadID  string `json:"agent_bead_id,omitempty"`
	SessionID    string `json:"session_id"`
	Event        string `json:"event"`
	TS           string `json:"ts"`
	ToolName     string `json:"tool_name,omitempty"`
	ToolInput    any    `json:"tool_input,omitempty"`
	ToolResponse any    `json:"tool_response,omitempty"`
	SubagentID   string `json:"subagent_id,omitempty"`
	SubagentType string `json:"subagent_type,omitempty"`
	// ToolUseID/PromptID/PermissionMode/DurationMs/DurationSource mirror
	// cmd/gb/hook_relay.go's hookRelayEvent — see that file for the field
	// semantics (in particular, why DurationSource is never "derived" at the
	// producer).
	ToolUseID      string `json:"tool_use_id,omitempty"`
	PromptID       string `json:"prompt_id,omitempty"`
	PermissionMode string `json:"permission_mode,omitempty"`
	DurationMs     int64  `json:"duration_ms,omitempty"`
	DurationSource string `json:"duration_source,omitempty"`

	// Error events (PostToolUseFailure, StopFailure)
	Error     string `json:"error,omitempty"`
	ErrorType string `json:"error_type,omitempty"`

	// Notification events
	NotificationType string `json:"notification_type,omitempty"`

	// Task events
	TaskID      string `json:"task_id,omitempty"`
	TaskSubject string `json:"task_subject,omitempty"`

	// Stop/SubagentStop: last assistant message text
	LastAssistantMessage string `json:"last_assistant_message,omitempty"`

	// StopGateDecision is "block" or "allow" on the Stop events gb hook
	// stop-gate publishes, one per verdict (cmd/gb/hook_stopgate_publish.go,
	// gd-fSdUbOmf3g). A blocked Stop does not end the turn.
	StopGateDecision string `json:"stop_gate_decision,omitempty"`

	// CwdChanged events
	OldCWD string `json:"old_cwd,omitempty"`
	NewCWD string `json:"new_cwd,omitempty"`

	// FileChanged events
	FilePath   string `json:"file_path,omitempty"`
	ChangeType string `json:"change_type,omitempty"`

	// PermissionRequest events
	RequestID string `json:"request_id,omitempty"`

	// Session events
	Source string `json:"source,omitempty"`
	Reason string `json:"reason,omitempty"`

	// Compact events / Setup events
	Trigger string `json:"trigger,omitempty"`

	// Teammate events
	TeammateID   string `json:"teammate_id,omitempty"`
	TeammateType string `json:"teammate_type,omitempty"`

	// ConfigChange events
	ConfigSource string `json:"config_source,omitempty"`

	// InstructionsLoaded events
	InstructionFilePath string `json:"instruction_file_path,omitempty"`
	MemoryType          string `json:"memory_type,omitempty"`
	LoadReason          string `json:"load_reason,omitempty"`

	// Elicitation / ElicitationResult events
	MCPServerName string `json:"mcp_server_name,omitempty"`
	Message       string `json:"message,omitempty"`
	ElicitationID string `json:"elicitation_id,omitempty"`
	Action        string `json:"action,omitempty"`

	// WorktreeCreate events
	WorktreeName string `json:"worktree_name,omitempty"`

	// WorktreeRemove events
	WorktreePath string `json:"worktree_path,omitempty"`
}

// relevantHookEvents is the set of hook event types we dispatch to the stream manager.
var relevantHookEvents = map[string]bool{
	"PreToolUse":         true,
	"PostToolUse":        true,
	"PostToolUseFailure": true,
	"Stop":               true,
	"StopFailure":        true,
	"SubagentStart":      true,
	"SubagentStop":       true,
	"Notification":       true,
	"TaskCreated":        true,
	"TaskCompleted":      true,
	"CwdChanged":         true,
	"FileChanged":        true,
	"PermissionRequest":  true,
	"SessionStart":       true,
	"SessionEnd":         true,
	"PreCompact":         true,
	"PostCompact":        true,
	"TeammateIdle":       true,
	"ConfigChange":       true,
	"InstructionsLoaded": true,
	"Elicitation":        true,
	"ElicitationResult":  true,
	"WorktreeCreate":     true,
	"WorktreeRemove":     true,
	"Setup":              true,
}

// HookStreamConfig holds configuration for the hook event subscriber.
type HookStreamConfig struct {
	NatsURL   string
	NatsToken string
	// NatsUser: when set, NatsToken is sent as this user's PASSWORD —
	// multi-user server auth only supports user/password entries
	// (ka-0000000002).
	NatsUser      string
	StreamManager *AgentStreamManager
	Logger        *slog.Logger
}

// messageEvent is the payload from coop's NATS message forwarder.
type messageEvent struct {
	EventType  string `json:"event_type"`
	SessionID  string `json:"session_id"`
	Agent      string `json:"agent"`
	Text       string `json:"text,omitempty"`
	FullLength int    `json:"full_length,omitempty"`
	State      string `json:"state,omitempty"`
	Cause      string `json:"cause,omitempty"`
	// Usage fields
	InputTokens  uint64 `json:"input_tokens,omitempty"`
	OutputTokens uint64 `json:"output_tokens,omitempty"`
	CacheRead    uint64 `json:"cache_read,omitempty"`
	CacheWrite   uint64 `json:"cache_write,omitempty"`
	DurationMs   uint64 `json:"duration_ms,omitempty"`
	// Model is the model id LiteLLM/Claude Code actually SERVED this turn
	// (coop reads it from message.model). ExpectedModel is the model the
	// controller baked into the pod (ANTHROPIC_MODEL/CLAUDE_MODEL), stamped
	// into coop's session metadata. Comparing the two flags a silent
	// mid-conversation model switch (fallback) — ka-0000003127.
	Model         string `json:"model,omitempty"`
	ExpectedModel string `json:"expected_model,omitempty"`
	// Refusal fields (event_type == "refusal", ka-0000004364 → ka-0000006122).
	// coop emits a classified `refusal` event when a turn's Anthropic stop_reason
	// is "refusal". StopReason is the literal token ("refusal"). RequestedModel is
	// the model the session asked for (coop's ANTHROPIC_MODEL) — distinct from
	// ExpectedModel (used by the usage path); on the refusal event coop carries
	// the requested model under `requested_model`, and Model is the served model
	// (`model`), so a refusal answered by a silent fallback is visible.
	StopReason     string `json:"stop_reason,omitempty"`
	RequestedModel string `json:"requested_model,omitempty"`
	// Session identity carried in coop metadata; used to attribute a durable
	// model-fallback / refusal record and route its Slack flag. Project/AgentBeadID
	// come from BOAT_PROJECT/BOAT_AGENT_BEAD_ID.
	Project     string `json:"project,omitempty"`
	AgentBeadID string `json:"agent_bead_id,omitempty"`
	// K8s metadata for agent identification
	K8s *k8sMeta `json:"k8s,omitempty"`
}

type k8sMeta struct {
	PodName string `json:"pod_name,omitempty"`
}

// HookStream subscribes to NATS hook events and dispatches to the stream manager.
type HookStream struct {
	cfg    HookStreamConfig
	conn   *nats.Conn
	sub    *nats.Subscription
	msgSub *nats.Subscription
	// lastEventNano is the unix-nano timestamp of the most recent NATS message
	// consumed (any subject). Read via LastEventAt(); surfaced on /healthz so
	// the platform health page can detect a stalled bridge.
	lastEventNano atomic.Int64
}

// NewHookStream creates a hook event subscriber.
// Returns nil if NatsURL is empty (NATS not configured).
func NewHookStream(cfg HookStreamConfig) *HookStream {
	if cfg.NatsURL == "" {
		cfg.Logger.Info("hookstream: NATS URL not configured, hook streaming disabled")
		return nil
	}
	if cfg.StreamManager == nil {
		cfg.Logger.Warn("hookstream: no stream manager provided, hook streaming disabled")
		return nil
	}
	return &HookStream{cfg: cfg}
}

// Start connects to NATS and subscribes to hook events.
// The subscription runs asynchronously — this method returns immediately.
func (h *HookStream) Start() error {
	opts := []nats.Option{
		nats.Name("slack-bridge-hookstream"),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(2 * nats.DefaultReconnectWait),
		nats.DisconnectErrHandler(func(_ *nats.Conn, err error) {
			if err != nil {
				h.cfg.Logger.Warn("hookstream: NATS disconnected", "error", err)
			}
		}),
		nats.ReconnectHandler(func(_ *nats.Conn) {
			h.cfg.Logger.Info("hookstream: NATS reconnected")
		}),
	}

	opts = append(opts, natsauth.Options(h.cfg.NatsUser, h.cfg.NatsToken)...)
	opts = append(opts, natsauth.StartupRetry()...)

	conn, err := nats.Connect(h.cfg.NatsURL, opts...)
	if err != nil {
		return err
	}
	h.conn = conn

	// Subscribe to all hook events: hooks.<agent>.<event>
	sub, err := conn.Subscribe("hooks.>", h.handleMessage)
	if err != nil {
		conn.Close()
		return err
	}
	h.sub = sub

	// Subscribe to coop message forwarding events: coop.events.messages.*
	msgSub, err := conn.Subscribe("coop.events.messages.>", h.handleMessageForward)
	if err != nil {
		h.cfg.Logger.Warn("hookstream: failed to subscribe to message events", "error", err)
		// Non-fatal — hook events still work without message forwarding.
	} else {
		h.msgSub = msgSub
	}

	// Flush so the subscription interest reaches the server before Start()
	// returns. Subscribe() only queues the SUB protocol on the client's write
	// buffer; without this round-trip the server may not yet have registered
	// interest when a publisher fires immediately after Start(). Core NATS is
	// fire-and-forget, so any message that beats interest propagation is
	// dropped and the handler never runs. A callers-publish-right-after-Start
	// pattern (e.g. the usage-contract fixture) can lose the first event under
	// load — the interest-propagation race behind ka-0000003735.
	if !conn.IsConnected() {
		// NATS is not up yet: the subscriptions are buffered and sent once
		// the retrying connect succeeds, and there is nothing to flush.
		h.cfg.Logger.Warn("hookstream: NATS not reachable yet — subscribing once it is", "url", h.cfg.NatsURL)
		return nil
	}
	if err := conn.Flush(); err != nil {
		conn.Close()
		return fmt.Errorf("flush subscriptions: %w", err)
	}

	h.cfg.Logger.Info("hookstream: subscribed to NATS hook events",
		"url", h.cfg.NatsURL, "subject", "hooks.>")
	return nil
}

// Close unsubscribes and closes the NATS connection.
func (h *HookStream) Close() {
	if h.sub != nil {
		_ = h.sub.Unsubscribe()
	}
	if h.msgSub != nil {
		_ = h.msgSub.Unsubscribe()
	}
	if h.conn != nil {
		h.conn.Close()
	}
}

// LastEventAt returns the timestamp of the most recent NATS message consumed
// (hook or coop message-forward), or the zero time if none has been seen.
func (h *HookStream) LastEventAt() time.Time {
	n := h.lastEventNano.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

// handleMessage processes a single NATS hook event message.
func (h *HookStream) handleMessage(msg *nats.Msg) {
	h.lastEventNano.Store(time.Now().UnixNano())
	var evt hookEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		return // silently skip malformed events
	}

	if !relevantHookEvents[evt.Event] {
		return
	}

	// Extract agent name from subject (hooks.<agent>.<event>) or payload.
	agent := evt.Agent
	if agent == "" {
		agent = agentFromSubject(msg.Subject)
	}
	if agent == "" {
		return
	}

	ctx := context.Background()

	label := extractToolLabel(evt.ToolName, evt.ToolInput)

	switch evt.Event {
	case "PreToolUse":
		h.cfg.StreamManager.HandleToolEvent(agent, evt.ToolName, evt.Event, label)
	case "PostToolUse":
		h.cfg.StreamManager.HandleToolEvent(agent, evt.ToolName, evt.Event, label)
		// Parse Bash output for git operations and test results.
		if evt.ToolName == "Bash" {
			if summary := parseBashOutput(evt.ToolResponse); summary != "" {
				h.cfg.StreamManager.HandleGitEvent(ctx, agent, summary)
			}
		}
	case "PostToolUseFailure":
		h.cfg.StreamManager.HandleToolError(ctx, agent, evt.ToolName, evt.Error)
	case "SubagentStart":
		h.cfg.StreamManager.HandleToolEvent(agent, "Agent", "PreToolUse", evt.SubagentType)
		h.cfg.StreamManager.HandleSubagentStart(ctx, agent, evt.SubagentID, evt.SubagentType)
	case "SubagentStop":
		h.cfg.StreamManager.HandleToolEvent(agent, "Agent", "PostToolUse", evt.SubagentType)
		h.cfg.StreamManager.HandleSubagentStop(ctx, agent, evt.SubagentID, evt.SubagentType, evt.LastAssistantMessage)
	case "Stop":
		// The stop-gate blocked this Stop and the agent keeps working, so
		// the turn is not complete (gd-fSdUbOmf3g).
		if evt.StopGateDecision == "block" {
			return
		}
		h.cfg.StreamManager.HandleStopEvent(ctx, agent, evt.LastAssistantMessage)
	case "StopFailure":
		h.cfg.StreamManager.HandleToolError(ctx, agent, "Stop", evt.Error)
	case "Notification":
		h.cfg.StreamManager.HandleNotification(ctx, agent, evt.NotificationType)
	case "TaskCreated":
		h.cfg.StreamManager.HandleTaskCreated(ctx, agent, evt.TaskID, evt.TaskSubject)
	case "TaskCompleted":
		h.cfg.StreamManager.HandleTaskCompleted(ctx, agent, evt.TaskID, evt.TaskSubject)
	case "CwdChanged":
		h.cfg.StreamManager.HandleCwdChanged(ctx, agent, evt.OldCWD, evt.NewCWD)
	case "FileChanged":
		h.cfg.StreamManager.HandleFileChanged(ctx, agent, evt.FilePath, evt.ChangeType)
	case "PermissionRequest":
		h.cfg.StreamManager.HandlePermissionRequest(ctx, agent, evt.ToolName, evt.RequestID)
	case "SessionStart":
		h.cfg.StreamManager.HandleSessionStart(ctx, agent, evt.Source)
	case "SessionEnd":
		h.cfg.StreamManager.HandleSessionEnd(ctx, agent, evt.Reason)
	case "PreCompact":
		h.cfg.StreamManager.HandleCompactEvent(ctx, agent, "PreCompact", evt.Trigger)
	case "PostCompact":
		h.cfg.StreamManager.HandleCompactEvent(ctx, agent, "PostCompact", evt.Trigger)
	case "TeammateIdle":
		h.cfg.StreamManager.HandleTeammateIdle(ctx, agent, evt.TeammateID, evt.TeammateType)
	case "ConfigChange":
		h.cfg.StreamManager.HandleConfigChange(ctx, agent, evt.ConfigSource, evt.FilePath)
	case "InstructionsLoaded":
		h.cfg.StreamManager.HandleInstructionsLoaded(ctx, agent, evt.InstructionFilePath, evt.LoadReason)
	case "Elicitation":
		h.cfg.StreamManager.HandleElicitation(ctx, agent, evt.MCPServerName, evt.Message)
	case "ElicitationResult":
		h.cfg.StreamManager.HandleElicitationResult(ctx, agent, evt.MCPServerName, evt.Action)
	case "WorktreeCreate":
		h.cfg.StreamManager.HandleWorktreeCreate(ctx, agent, evt.WorktreeName)
	case "WorktreeRemove":
		h.cfg.StreamManager.HandleWorktreeRemove(ctx, agent, evt.WorktreePath)
	case "Setup":
		h.cfg.StreamManager.HandleSetup(ctx, agent, evt.Trigger)
	}
}

// handleMessageForward processes a NATS message from coop's message forwarder.
// Subject pattern: coop.events.messages.<event_type>
func (h *HookStream) handleMessageForward(msg *nats.Msg) {
	h.lastEventNano.Store(time.Now().UnixNano())
	var evt messageEvent
	if err := json.Unmarshal(msg.Data, &evt); err != nil {
		return
	}

	// Resolve agent name: prefer k8s pod_name (which maps to agent bead name),
	// fall back to "agent" metadata field from coop.
	agent := ""
	if evt.K8s != nil && evt.K8s.PodName != "" {
		agent = evt.K8s.PodName
	}
	if agent == "" {
		agent = evt.Agent
	}
	if agent == "" {
		return
	}

	h.cfg.StreamManager.HandleMessageEvent(agent, &evt)
}

// extractToolLabel extracts a short human-readable label from tool_input.
// For Bash: prefers "description", falls back to first line of "command" (truncated).
// For Read/Edit/Write: uses the file basename.
// For Grep/Glob: uses the search pattern.
// For WebFetch/WebSearch: uses the URL or query.
// For LSP: uses the operation.
// For Skill/SendMessage/NotebookEdit: uses the primary field.
// For MCP tools: tries common fields (query, url, pattern, operation, prompt).
// Falls back to generic heuristics for unknown tools.
// Returns "" if no meaningful label can be extracted.
func extractToolLabel(toolName string, toolInput any) string {
	m, ok := toolInput.(map[string]any)
	if !ok {
		return ""
	}

	switch toolName {
	case "Bash":
		// Prefer the description field (human-readable summary).
		if desc, ok := m["description"].(string); ok && desc != "" {
			return truncate(desc, 50)
		}
		// Fall back to the first line of the command.
		if cmd, ok := m["command"].(string); ok && cmd != "" {
			line := strings.SplitN(cmd, "\n", 2)[0]
			// Strip cd prefixes for readability.
			if idx := strings.Index(line, " && "); idx >= 0 && strings.HasPrefix(line, "cd ") {
				line = line[idx+4:]
			}
			return truncate(line, 50)
		}
	case "Read":
		if fp, ok := m["file_path"].(string); ok && fp != "" {
			parts := strings.Split(fp, "/")
			return parts[len(parts)-1]
		}
	case "Edit", "Write":
		if fp, ok := m["file_path"].(string); ok && fp != "" {
			parts := strings.Split(fp, "/")
			return parts[len(parts)-1]
		}
	case "Grep":
		if pat, ok := m["pattern"].(string); ok && pat != "" {
			return truncate(pat, 30)
		}
	case "Glob":
		if pat, ok := m["pattern"].(string); ok && pat != "" {
			return truncate(pat, 30)
		}
	case "Agent":
		if desc, ok := m["description"].(string); ok && desc != "" {
			return truncate(desc, 40)
		}
	case "WebFetch":
		if url, ok := m["url"].(string); ok && url != "" {
			return truncate(url, 50)
		}
	case "WebSearch":
		if q, ok := m["query"].(string); ok && q != "" {
			return truncate(q, 40)
		}
	case "LSP":
		if op, ok := m["operation"].(string); ok && op != "" {
			if fp, ok := m["filePath"].(string); ok && fp != "" {
				parts := strings.Split(fp, "/")
				return truncate(op+" "+parts[len(parts)-1], 40)
			}
			return truncate(op, 30)
		}
	case "NotebookEdit":
		if np, ok := m["notebook_path"].(string); ok && np != "" {
			parts := strings.Split(np, "/")
			return parts[len(parts)-1]
		}
	case "Skill":
		if s, ok := m["skill"].(string); ok && s != "" {
			return truncate(s, 30)
		}
	case "SendMessage":
		if to, ok := m["to"].(string); ok && to != "" {
			return truncate("to: "+to, 30)
		}
	default:
		// MCP tools (mcp__*) and other unknown tools: try common fields.
		return extractGenericLabel(m)
	}
	return ""
}

// extractGenericLabel tries common field names to extract a label from tool input.
// Used as a fallback for MCP tools and other unrecognized tools.
func extractGenericLabel(m map[string]any) string {
	// Try fields in order of informativeness.
	for _, key := range []string{"description", "query", "url", "pattern", "prompt", "operation", "skill", "command", "file_path", "path", "name"} {
		if v, ok := m[key].(string); ok && v != "" {
			return truncate(v, 40)
		}
	}
	return ""
}

// truncate is defined in jira.go — reused here for label truncation.

// agentFromSubject extracts the agent name from a NATS subject like "hooks.worker-1.PreToolUse".
func agentFromSubject(subject string) string {
	parts := strings.SplitN(subject, ".", 3)
	if len(parts) < 3 {
		return ""
	}
	return parts[1]
}

// --- Bash output parsing ---

// Regex patterns for detecting git operations and test results in Bash output.
var (
	gitCommitRe  = regexp.MustCompile(`(?m)^\[[\w/.-]+ ([0-9a-f]{7,12})\]\s+(.+)$`)
	gitPushRe    = regexp.MustCompile(`(?m)([\w.-]+)\s+->\s+([\w/.-]+)`)
	gitPushToRe  = regexp.MustCompile(`(?m)To\s+(https?://\S+|git@\S+)`)
	testPassedRe = regexp.MustCompile(`(?m)^(ok\s+\S+|PASS\b|Tests?:.*\bpassed\b|All \d+ tests? passed)`)
	testFailedRe = regexp.MustCompile(`(?m)^(FAIL\s+\S+|Tests?:.*\bfailed\b|FAILED)`)
)

// parseBashOutput examines the tool response from a Bash PostToolUse event and
// returns a summary string if a git commit, git push, or test result is detected.
// Returns "" if no recognizable pattern is found.
func parseBashOutput(toolResponse any) string {
	output := toolResponseString(toolResponse)
	if output == "" {
		return ""
	}

	var parts []string

	// Detect git commit.
	if m := gitCommitRe.FindStringSubmatch(output); len(m) >= 3 {
		parts = append(parts, fmt.Sprintf("Committed %s: %s", m[1], truncateText(m[2], 60)))
	}

	// Detect git push.
	if gitPushToRe.MatchString(output) {
		if m := gitPushRe.FindStringSubmatch(output); len(m) >= 3 {
			parts = append(parts, fmt.Sprintf("Pushed %s → %s", m[1], m[2]))
		} else {
			parts = append(parts, "Pushed to remote")
		}
	}

	// Detect test results.
	if testFailedRe.MatchString(output) {
		if m := testFailedRe.FindStringSubmatch(output); len(m) >= 2 {
			parts = append(parts, truncateText(m[1], 80))
		}
	} else if testPassedRe.MatchString(output) {
		if m := testPassedRe.FindStringSubmatch(output); len(m) >= 2 {
			parts = append(parts, truncateText(m[1], 80))
		}
	}

	return strings.Join(parts, "; ")
}

// toolResponseString extracts a string from the ToolResponse field, which may
// be a string, a map with a "stdout" key, or another JSON type.
func toolResponseString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case map[string]any:
		if s, ok := t["stdout"].(string); ok {
			return s
		}
		if s, ok := t["output"].(string); ok {
			return s
		}
	}
	return ""
}
