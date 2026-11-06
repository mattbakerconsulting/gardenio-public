package main

// gb hook relay — Publish Claude Code, Codex and Gemini hook events to the HOOK_EVENTS NATS stream.
//
// Reads the hook input JSON from stdin, transforms it to the hook event schema
// (see docs/hook-event-schema.md), and publishes to the HOOK_EVENTS NATS
// stream on subject hooks.<agent>.<event>.
//
// Publishing strategy (in order of preference):
//   1. HTTP POST to kbeads daemon ($BEADS_HTTP_ADDR/v1/hooks/publish)
//   2. Direct NATS publish ($BEADS_NATS_URL) with short timeout
//
// Env vars:
//   BEADS_HTTP_ADDR     — kbeads daemon HTTP address (preferred publisher)
//   BEADS_NATS_URL      — NATS server URL, fallback (e.g., nats://gardenio-nats:4222)
//   HOSTNAME            — Agent name for the subject hierarchy
//   BOAT_AGENT          — Agent name override (set by controller)
//   KD_AGENT_ID         — Agent bead id, stamped as agent_bead_id (preferred)
//   BOAT_AGENT_BEAD_ID  — Agent bead id fallback, same meaning
//   BOAT_PROJECT        — gardenio project, stamped as project (gd-sWwPnwkZtw)
//   BOAT_ROLE           — agent role (crew/gardener/...), stamped as role
//
// Note on consumers: the original subscriber was beads3d (the "doots"
// visualisation), deleted in f1fd0c5f. Today the only non-test subscriber to
// hooks.> is bridge.HookStream, built solely by cmd/slack-bridge, which is
// disabled on the live cluster. These events are durably stored and read by
// nobody in production; see gardenio/plans/claude-session-subagent-tracking.md.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/toollatency"

	"github.com/nats-io/nats.go"
	"github.com/spf13/cobra"
)

var hookRelayCmd = &cobra.Command{
	Use:   "relay",
	Short: "Publish hook events to the HOOK_EVENTS NATS stream",
	Long: `Reads Claude Code hook input JSON from stdin and publishes it
to the HOOK_EVENTS NATS JetStream stream.

Subject format: hooks.<agent>.<event>
Example: hooks.worker-1.PreToolUse

Events published: PreToolUse, PostToolUse, PostToolUseFailure, Stop,
StopFailure, SubagentStart, SubagentStop, SessionStart, SessionEnd,
PreCompact, PostCompact, Notification, TeammateIdle, TaskCreated,
TaskCompleted, CwdChanged, FileChanged, PermissionRequest,
ConfigChange, InstructionsLoaded, Elicitation, ElicitationResult,
WorktreeCreate, WorktreeRemove, Setup.

Publishing strategy (in order of preference):
  1. HTTP POST to kbeads daemon (BEADS_HTTP_ADDR/v1/hooks/publish)
  2. Direct NATS publish (BEADS_NATS_URL) with optimized connection`,
	RunE: func(cmd *cobra.Command, args []string) error {
		rec := newHarnessRecorder()
		err := runHookRelay(rec)
		rec.finish("relay")
		return err
	},
}

func init() {
	hookCmd.AddCommand(hookRelayCmd)
}

// hookRelayEvent is the NATS message payload for hook events.
type hookRelayEvent struct {
	Agent string `json:"agent"`
	// AgentBeadID is the agent bead this event came from (e.g. "gd-2bZJkQ8VZR").
	//
	// Agent is only a subject-safe display name derived from BOAT_AGENT or the
	// pod hostname, so it cannot be joined back to a bead: names repeat across
	// pods and are lossy after sanitizeSubject. AgentBeadID is the stable key
	// that lets a consumer attach a hook event to the agent and session beads
	// that already exist. Omitted when the process has no agent context (a
	// developer laptop, an unparented one-off), so consumers must tolerate "".
	AgentBeadID string `json:"agent_bead_id,omitempty"`
	// Project is the gardenio project this agent is working in (BOAT_PROJECT),
	// one of the P2 tool-latency consumer's bounded metric labels
	// (gd-sWwPnwkZtw) — small and fixed, unlike Agent/AgentBeadID.
	Project string `json:"project,omitempty"`
	// Role is the agent's functional role (BOAT_ROLE, e.g. "crew"/"gardener").
	Role string `json:"role,omitempty"`
	// SessionID is the harness-neutral session id: the Claude session id, or
	// the Codex session (thread) id. Consumers key on this regardless of Harness.
	SessionID string `json:"session_id"`
	// Harness is the agent CLI that fired the hook: "claude", "codex" or "gemini".
	Harness string `json:"harness"`
	Event   string `json:"event"`
	TS      string `json:"ts"`
	CWD     string `json:"cwd,omitempty"`
	// IsSidechain marks a tool call issued from a subagent/sidechain context
	// rather than the main conversation thread (input key "is_sidechain").
	IsSidechain bool `json:"is_sidechain,omitempty"`

	// Tool events
	ToolName     string `json:"tool_name,omitempty"`
	ToolInput    any    `json:"tool_input,omitempty"`
	ToolResponse any    `json:"tool_response,omitempty"`
	// ToolResponseExcerpt is what a PostToolUse response loses past the
	// 1024-byte cut that still says why the call went wrong: the lines after
	// the cut that read like an error or a refusal, and the last line
	// (gd-YaYOmNpbnW). gh prints "Resource not accessible by personal access
	// token" last and kubectl prints its Forbidden lines between its tables,
	// so the cut hid both from gb session sweep. Set only when tool_response
	// was cut; each line RedactText'd, at most toolResponseExcerptMax bytes.
	ToolResponseExcerpt string `json:"tool_response_excerpt,omitempty"`
	// ToolDescription and ToolFilePath are copied out of tool_input on
	// PreToolUse before truncateAny cuts it to 1024 bytes (bird captions spec
	// 6.2, gd-gCHKhIROvB): tool_input marshals with its keys sorted, so a Bash
	// "description" (after "command") and a Write "file_path" (after
	// "content") are the first lost. beads-viewer makes a call's caption from
	// them (internal/gardenhub/caption.go). Each is RedactText'd and at most
	// toolCaptionFieldMax bytes; both are already inside tool_input, so they
	// add no new secret surface.
	ToolDescription string `json:"tool_description,omitempty"` // Bash/Monitor/Agent/Task "description"
	ToolFilePath    string `json:"tool_file_path,omitempty"`   // Read/Edit/Write/MultiEdit/Notebook* "file_path"
	// ToolUseID is Claude's own pairing key for a tool call, unique within a
	// session and stable across its PreToolUse/PostToolUse(Failure) events.
	// Stamped on PreToolUse too (not just Post) so a consumer can pair the two
	// without guessing from tool name + arrival order — the same FIFO-by-name
	// mis-pairing fixed on the coop side in gd-Kl42VWWRd6.
	ToolUseID string `json:"tool_use_id,omitempty"`
	// PromptID identifies the assistant turn that issued this tool call.
	PromptID string `json:"prompt_id,omitempty"`
	// PermissionMode is the session's permission mode at call time (e.g.
	// "default", "acceptEdits", "bypassPermissions", "plan").
	PermissionMode string `json:"permission_mode,omitempty"`
	// DurationMs is Claude Code's own measurement of tool execution time, on
	// PostToolUse. It is CC-version dependent, so it is optional: a consumer
	// that needs a duration when this is absent must derive one itself (e.g.
	// from Pre/Post arrival timestamps) and is responsible for marking that
	// value "derived" in its own data model — DurationSource below only ever
	// says "claude", never "derived", because this relay has no cross-process
	// state to compute a derived value.
	DurationMs int64 `json:"duration_ms,omitempty"`
	// DurationSource is "claude" when DurationMs came from Claude Code's own
	// payload, and omitted otherwise (including when DurationMs is absent).
	DurationSource string `json:"duration_source,omitempty"`
	// OutputBytes is the size of a PostToolUse response's text before the
	// 1024-byte cut (every string in tool_response, a shell call's stdout and
	// stderr): the Commons jobs record's output size (gd-qlk31NBV6o).
	OutputBytes int `json:"output_bytes,omitempty"`

	// Session events
	Source string `json:"source,omitempty"`
	Reason string `json:"reason,omitempty"`

	// Subagent events
	SubagentID   string `json:"subagent_id,omitempty"`
	SubagentType string `json:"subagent_type,omitempty"`

	// Teammate events
	TeammateID   string `json:"teammate_id,omitempty"`
	TeammateType string `json:"teammate_type,omitempty"`

	// Task events
	TaskID      string `json:"task_id,omitempty"`
	TaskSubject string `json:"task_subject,omitempty"`

	// Compact events / Setup events
	Trigger string `json:"trigger,omitempty"`

	// Error events (PostToolUseFailure, StopFailure)
	Error     string `json:"error,omitempty"`
	ErrorType string `json:"error_type,omitempty"`

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

	// Notification events
	NotificationType string `json:"notification_type,omitempty"`

	// Stop/SubagentStop: last assistant message text
	LastAssistantMessage string `json:"last_assistant_message,omitempty"`
	// StopGateDecision is the stop-gate's verdict on this Stop: "block" or
	// "allow" (gd-fSdUbOmf3g). Set only on the Stop events gb hook stop-gate
	// publishes (hook_stopgate_publish.go), whose Reason says why.
	StopGateDecision string `json:"stop_gate_decision,omitempty"`

	// CwdChanged events
	OldCWD string `json:"old_cwd,omitempty"`
	NewCWD string `json:"new_cwd,omitempty"`

	// FileChanged events
	FilePath   string `json:"file_path,omitempty"`
	ChangeType string `json:"change_type,omitempty"`

	// PermissionRequest events
	RequestID string `json:"request_id,omitempty"`
}

// errSkipEvent signals that the event should be silently skipped.
var errSkipEvent = fmt.Errorf("skip event")

// runHookRelay does the relay. rec (nil-safe) collects gardenio's own timings;
// the caller finishes it, which hands them to a detached recorder (see
// hook_harness_metrics.go) so nothing here waits on the metrics push.
func runHookRelay(rec *harnessRecorder) error {
	natsURL := os.Getenv("BEADS_NATS_URL")
	if daemon == nil && natsURL == "" {
		return nil // No publish target configured — silently skip.
	}
	// A land door serves no hook route and a tenant's laptop has no fleet
	// bus (gd-2iqSr9ANOH): nothing to relay to.
	if landDoor() != "" {
		return nil
	}

	// Read hook input from stdin.
	data, err := io.ReadAll(os.Stdin)
	if err != nil || len(data) == 0 {
		return nil // No input — nothing to relay.
	}

	var input map[string]any
	if err := json.Unmarshal(data, &input); err != nil {
		return nil // Invalid JSON — silently skip.
	}

	agentName := resolveAgentName()
	agentBeadID := resolveAgentBeadID()
	evt, subject, err := buildRelayEvent(input, agentName, agentBeadID)
	if err != nil {
		return nil // Unknown or empty event — silently skip.
	}

	payload, err := json.Marshal(evt)
	if err != nil {
		return nil
	}

	// Only tool events count toward the harness tax; see harnessRecorder.arm.
	switch evt.Event {
	case "PreToolUse", "PostToolUse", "PostToolUseFailure":
		rec.arm()
	}

	// Try HTTP POST to kbeads daemon first (preferred: no per-event NATS connection).
	// Timed as its own "bus_publish" phase (distinct from the whole "relay"
	// command) so a slow daemon/NATS round-trip is distinguishable from
	// gardenio's own JSON decode/transform overhead above.
	if daemon != nil {
		publishStart := time.Now()
		pubErr := daemon.PublishHookEvent(context.Background(), beadsapi.PublishHookEventRequest{
			Subject: subject,
			Payload: payload,
		})
		if pubErr == nil {
			rec.observe("bus_publish", time.Since(publishStart))
			return nil
		}
		// Fall through to NATS on HTTP failure.
		fmt.Fprintf(os.Stderr, "gb hook relay: HTTP publish failed, trying NATS\n")
	}

	// Fallback: direct NATS publish.
	if natsURL != "" {
		publishStart := time.Now()
		err := publishToNATS(natsURL, subject, payload)
		rec.observe("bus_publish", time.Since(publishStart))
		return err
	}

	return nil
}

// buildRelayEvent transforms raw hook input into a relay event and NATS subject.
// Returns errSkipEvent for unknown or empty event names.
//
// agentName and agentBeadID are passed in rather than read from the environment
// so this stays a pure function of its inputs and remains testable.
func buildRelayEvent(input map[string]any, agentName, agentBeadID string) (*hookRelayEvent, string, error) {
	eventName, _ := input["hook_event_name"].(string)
	if eventName == "" {
		return nil, "", errSkipEvent
	}
	harnessName := relayHarness(input)
	if harnessName == beadsapi.HarnessGemini {
		var ok bool
		if eventName, input, ok = normalizeGeminiHook(eventName, input); !ok {
			return nil, "", errSkipEvent
		}
	}

	sessionID, _ := input["session_id"].(string)
	cwd, _ := input["cwd"].(string)

	isSidechain, _ := input["is_sidechain"].(bool)

	evt := &hookRelayEvent{
		Agent:       agentName,
		AgentBeadID: agentBeadID,
		Project:     os.Getenv("BOAT_PROJECT"),
		Role:        os.Getenv("BOAT_ROLE"),
		SessionID:   sessionID,
		Harness:     harnessName,
		Event:       eventName,
		TS:          time.Now().UTC().Format(time.RFC3339Nano),
		CWD:         cwd,
		IsSidechain: isSidechain,
	}

	// Codex names the model turn "turn_id"; it plays the role of Claude's
	// prompt_id (the assistant turn that issued the call), so tool events fall
	// back to it.
	turnID, _ := input["turn_id"].(string)

	// Extract event-specific fields.
	switch eventName {
	case "PreToolUse":
		evt.ToolName, _ = input["tool_name"].(string)
		evt.ToolDescription, evt.ToolFilePath = toolCaptionFields(evt.ToolName, input["tool_input"])
		evt.ToolInput = truncateAny(redactToolInput(input["tool_input"]), 1024)
		evt.ToolUseID, _ = input["tool_use_id"].(string)
		evt.PromptID, _ = input["prompt_id"].(string)
		evt.PermissionMode, _ = input["permission_mode"].(string)
	case "PostToolUse":
		evt.ToolName, _ = input["tool_name"].(string)
		evt.ToolInput = truncateAny(redactToolInput(input["tool_input"]), 1024)
		evt.ToolResponse = truncateAny(input["tool_response"], 1024)
		evt.ToolResponseExcerpt = toolResponseExcerpt(evt.ToolName, input["tool_response"], evt.ToolResponse)
		evt.OutputBytes = responseBytes(input["tool_response"])
		evt.ToolUseID, _ = input["tool_use_id"].(string)
		evt.PromptID, _ = input["prompt_id"].(string)
		evt.PermissionMode, _ = input["permission_mode"].(string)
		if ms, ok := numberToInt64(input["duration_ms"]); ok {
			evt.DurationMs = ms
			evt.DurationSource = "claude"
		}
	case "PostToolUseFailure":
		evt.ToolName, _ = input["tool_name"].(string)
		evt.ToolInput = truncateAny(redactToolInput(input["tool_input"]), 1024)
		evt.Error, _ = input["error"].(string)
		evt.ErrorType, _ = input["error_type"].(string)
		evt.ToolUseID, _ = input["tool_use_id"].(string)
		evt.PromptID, _ = input["prompt_id"].(string)
		evt.PermissionMode, _ = input["permission_mode"].(string)
	case "SessionStart":
		evt.Source, _ = input["source"].(string)
	case "SessionEnd":
		evt.Reason, _ = input["reason"].(string)
	case "SubagentStart":
		evt.SubagentID, _ = input["agent_id"].(string)
		evt.SubagentType, _ = input["agent_type"].(string)
	case "SubagentStop":
		evt.SubagentID, _ = input["agent_id"].(string)
		evt.SubagentType, _ = input["agent_type"].(string)
		evt.LastAssistantMessage = truncateString(input["last_assistant_message"], 512)
	case "PreCompact", "PostCompact":
		evt.Trigger, _ = input["trigger"].(string)
	case "TeammateIdle":
		evt.TeammateID, _ = input["teammate_id"].(string)
		evt.TeammateType, _ = input["teammate_type"].(string)
	case "TaskCreated":
		evt.TaskID, _ = input["task_id"].(string)
		evt.TaskSubject, _ = input["task_subject"].(string)
	case "TaskCompleted":
		evt.TaskID, _ = input["task_id"].(string)
		evt.TaskSubject, _ = input["task_subject"].(string)
	case "Notification":
		evt.NotificationType, _ = input["notification_type"].(string)
	case "Stop":
		evt.LastAssistantMessage = truncateString(input["last_assistant_message"], 512)
	case "StopFailure":
		evt.Error, _ = input["error"].(string)
	case "CwdChanged":
		evt.OldCWD, _ = input["old_cwd"].(string)
		evt.NewCWD, _ = input["new_cwd"].(string)
	case "FileChanged":
		evt.FilePath, _ = input["file_path"].(string)
		evt.ChangeType, _ = input["change_type"].(string)
	case "PermissionRequest":
		evt.ToolName, _ = input["tool_name"].(string)
		evt.RequestID, _ = input["request_id"].(string)
	case "ConfigChange":
		evt.ConfigSource, _ = input["source"].(string)
		evt.FilePath, _ = input["file_path"].(string)
	case "InstructionsLoaded":
		evt.InstructionFilePath, _ = input["file_path"].(string)
		evt.MemoryType, _ = input["memory_type"].(string)
		evt.LoadReason, _ = input["load_reason"].(string)
	case "Elicitation":
		evt.MCPServerName, _ = input["mcp_server_name"].(string)
		evt.Message = truncateString(input["message"], 512)
		evt.ElicitationID, _ = input["elicitation_id"].(string)
	case "ElicitationResult":
		evt.MCPServerName, _ = input["mcp_server_name"].(string)
		evt.ElicitationID, _ = input["elicitation_id"].(string)
		evt.Action, _ = input["action"].(string)
	case "WorktreeCreate":
		evt.WorktreeName, _ = input["name"].(string)
	case "WorktreeRemove":
		evt.WorktreePath, _ = input["worktree_path"].(string)
	case "Setup":
		evt.Trigger, _ = input["trigger"].(string)
	default:
		return nil, "", errSkipEvent
	}

	if evt.PromptID == "" && turnID != "" {
		switch eventName {
		case "PreToolUse", "PostToolUse", "PostToolUseFailure":
			evt.PromptID = turnID
		}
	}

	subject := fmt.Sprintf("hooks.%s.%s", sanitizeSubject(agentName), eventName)
	return evt, subject, nil
}

// geminiHookEvents maps Gemini CLI hook event names to the canonical (Claude
// spelling) events every relay consumer keys on. Events with no counterpart
// (BeforeModel, AfterModel, BeforeToolSelection, ...) are skipped. BeforeAgent
// maps to UserPromptSubmit, which the relay does not publish for any harness.
var geminiHookEvents = map[string]string{
	"SessionStart": "SessionStart",
	"SessionEnd":   "SessionEnd",
	"BeforeAgent":  "UserPromptSubmit",
	"AfterAgent":   "Stop",
	"BeforeTool":   "PreToolUse",
	"AfterTool":    "PostToolUse",
	"Notification": "Notification",
	"PreCompress":  "PreCompact",
}

// normalizeGeminiHook rewrites a Gemini hook payload into the canonical shape
// buildRelayEvent reads. It returns a copy; ok is false for an event with no
// canonical counterpart. Gemini differs from Claude in two ways that matter
// here: AfterAgent carries the model's reply as prompt_response (Claude's Stop
// has last_assistant_message), and tool events have no tool_use_id, so the
// session id plus tool name is all a consumer can pair on.
func normalizeGeminiHook(event string, input map[string]any) (string, map[string]any, bool) {
	canonical, ok := geminiHookEvents[event]
	if !ok {
		return "", nil, false
	}
	out := make(map[string]any, len(input)+1)
	for k, v := range input {
		out[k] = v
	}
	out["hook_event_name"] = canonical
	if canonical == "Stop" {
		if _, has := out["last_assistant_message"]; !has {
			if resp, ok := out["prompt_response"].(string); ok {
				out["last_assistant_message"] = resp
			}
		}
	}
	return canonical, out, true
}

// geminiOnlyHookEvents are hook_event_name values the Gemini CLI emits that no
// other supported harness does, used to sniff the harness when
// GARDENIO_HARNESS is absent. (PreToolUse/PostToolUse/Stop are Claude/Codex.)
var geminiOnlyHookEvents = map[string]bool{
	"BeforeAgent": true, "AfterAgent": true, "BeforeTool": true, "AfterTool": true,
	"PreCompress": true, "BeforeModel": true, "AfterModel": true, "BeforeToolSelection": true,
}

// relayHarness names the agent CLI that produced a hook payload. The pod's
// GARDENIO_HARNESS (set by the controller) is authoritative; without it the
// payload is sniffed: Gemini's event names (BeforeTool, AfterAgent, ...) exist
// in no other harness and its transcript_path lives under ~/.gemini/, only
// Codex stamps turn_id on hook input, and its transcript_path is a
// rollout-*.jsonl file.
func relayHarness(input map[string]any) string {
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(envGardenioHarness))); v {
	case beadsapi.HarnessCodex, beadsapi.HarnessClaude, beadsapi.HarnessGemini:
		return v
	}
	if name, _ := input["hook_event_name"].(string); geminiOnlyHookEvents[name] {
		return beadsapi.HarnessGemini
	}
	// SessionStart/SessionEnd/Notification share their names with Claude's; the
	// session record under ~/.gemini/tmp/<slug>/chats/ tells them apart.
	tp, _ := input["transcript_path"].(string)
	if strings.Contains(filepath.ToSlash(tp), "/.gemini/") {
		return beadsapi.HarnessGemini
	}
	if _, ok := input["turn_id"]; ok {
		return beadsapi.HarnessCodex
	}
	if strings.HasPrefix(filepath.Base(tp), "rollout-") {
		return beadsapi.HarnessCodex
	}
	return beadsapi.HarnessClaude
}

// resolveAgentName extracts the agent name from environment.
func resolveAgentName() string {
	// BOAT_AGENT is set by the controller.
	if name := os.Getenv("BOAT_AGENT"); name != "" {
		return name
	}
	// Fall back to HOSTNAME, stripping the pod prefix.
	hostname, _ := os.Hostname()
	// Pod names are like "crew-gardenio-crew-worker-1" — extract "worker-1".
	parts := strings.Split(hostname, "-")
	if len(parts) >= 2 {
		return strings.Join(parts[len(parts)-2:], "-")
	}
	return hostname
}

// resolveAgentBeadID returns the agent bead id for this process, or "" when
// there is no agent context (a developer laptop, an unparented one-off run).
//
// KD_AGENT_ID and BOAT_AGENT_BEAD_ID carry the same value; both are checked in
// this order to match every other gb call site (agent_start_k8s.go,
// session_metrics_push.go, salvage_metrics_push.go, init_stdout_tail.go).
func resolveAgentBeadID() string {
	return envOr("KD_AGENT_ID", os.Getenv("BOAT_AGENT_BEAD_ID"))
}

// sanitizeSubject replaces characters invalid in NATS subjects.
func sanitizeSubject(s string) string {
	s = strings.ReplaceAll(s, " ", "_")
	s = strings.ReplaceAll(s, ".", "_")
	return s
}

// truncateString extracts a string from v (which may be any type) and
// truncates to maxLen characters. Returns "" if v is not a string.
func truncateString(v any, maxLen int) string {
	s, ok := v.(string)
	if !ok || s == "" {
		return ""
	}
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// truncateAny JSON-encodes the value and truncates to maxBytes,
// returning nil if the value is nil or empty.
func truncateAny(v any, maxBytes int) any {
	if v == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	if len(data) <= maxBytes {
		return v
	}
	// Truncate: re-parse the first maxBytes as a string summary.
	return string(data[:maxBytes]) + "..."
}

const (
	// toolResponseExcerptMax is the most bytes of tool_response_excerpt, and
	// toolResponseExcerptLineMax the most of any one line in it.
	toolResponseExcerptMax     = 1024
	toolResponseExcerptLineMax = 320
)

// toolResponseErrorLine is a response line worth keeping past the cut: an
// error, a refusal or a failed exit. "HTTP 4xx" is gh's form; a bare 401 is
// not matched, so an access log's status column is not kept.
var toolResponseErrorLine = regexp.MustCompile(`(?i)\b(?:error|fatal|forbidden|denied|refused|unauthori[sz]ed|cannot|can't|failed)\b|not accessible|permission|\bHTTP 4\d\d\b|\bexit (?:code|status) [1-9]`)

// toolResponseExcerptSkip are the tools whose response is file or page
// content: its "error" lines are source text, not the call failing, and a
// failed read arrives as PostToolUseFailure with its whole error.
var toolResponseExcerptSkip = map[string]bool{
	"Read": true, "Grep": true, "Glob": true, "NotebookRead": true, "WebFetch": true, "WebSearch": true,
	"Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true,
	"read_file": true, "write_file": true, "replace": true,
}

// toolResponseExcerpt is tool_response_excerpt for a response resp that the
// relay cut to head: the lines of resp's text that match
// toolResponseErrorLine, then its last non-empty line, skipping any line head
// already carries, in their order in resp and joined by newlines. The last
// line is kept first when the budget is short. It is "" when head is not a
// cut (truncateAny returned the value itself).
func toolResponseExcerpt(tool string, resp, head any) string {
	cut, ok := head.(string)
	if !ok || toolResponseExcerptSkip[tool] {
		return ""
	}
	var text []string
	collectStrings(resp, &text)
	lines := strings.Split(strings.Join(text, "\n"), "\n")
	seen := map[string]bool{}
	// keep reports whether line i is new: not blank, not already in the
	// head (compared JSON-escaped, the way the head carries it) and not a
	// repeat of a line already chosen.
	keep := func(i int) (string, bool) {
		l := strings.TrimRight(lines[i], " \t\r")
		if strings.TrimSpace(l) == "" || seen[l] {
			return "", false
		}
		if enc, err := json.Marshal(l); err == nil && strings.Contains(cut, string(enc[1:len(enc)-1])) {
			return "", false
		}
		seen[l] = true
		return l, true
	}
	clip := func(l string) string {
		l, _ = truncateUTF8(toollatency.RedactText(l), toolResponseExcerptLineMax)
		return l
	}
	picked := map[int]string{}
	budget := toolResponseExcerptMax
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(lines[last]) == "" {
		last--
	}
	if last >= 0 {
		if l, ok := keep(last); ok {
			picked[last] = clip(l)
			budget -= len(picked[last]) + 1
		}
	}
	for i := 0; i < last; i++ {
		if !toolResponseErrorLine.MatchString(lines[i]) {
			continue
		}
		l, ok := keep(i)
		if !ok {
			continue
		}
		l = clip(l)
		if len(l)+1 > budget {
			break
		}
		picked[i] = l
		budget -= len(l) + 1
	}
	idx := make([]int, 0, len(picked))
	for i := range picked {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	out := make([]string, len(idx))
	for j, i := range idx {
		out[j] = picked[i]
	}
	return strings.Join(out, "\n")
}

// collectStrings appends v's string values to out, map keys in sorted order
// (json.Marshal's order, so Bash's stderr comes before its stdout as in the
// cut head).
// responseBytes is the bytes of every string in a tool response.
func responseBytes(v any) int {
	var text []string
	collectStrings(v, &text)
	n := 0
	for _, s := range text {
		n += len(s)
	}
	return n
}

func collectStrings(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		*out = append(*out, t)
	case []any:
		for _, x := range t {
			collectStrings(x, out)
		}
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			collectStrings(t[k], out)
		}
	}
}

// toolCaptionFieldMax is the most bytes of tool_description or tool_file_path.
const toolCaptionFieldMax = 256

// toolCaptionFields is the description and file path of a tool call's input,
// for the hub's caption, read from the whole tool_input before it is cut. Gemini
// names its shell and file tools run_shell_command, read_file, write_file and
// replace; Codex's shell call has no description.
func toolCaptionFields(tool string, in any) (description, filePath string) {
	m, _ := in.(map[string]any)
	field := func(key string) string {
		s, _ := m[key].(string)
		s, _ = truncateUTF8(toollatency.RedactText(strings.TrimSpace(s)), toolCaptionFieldMax)
		return s
	}
	switch tool {
	case "Bash", "Monitor", "Agent", "Task", "run_shell_command":
		description = field("description")
	case "Read", "Edit", "Write", "MultiEdit", "NotebookRead", "NotebookEdit", "read_file", "write_file", "replace":
		if filePath = field("file_path"); filePath == "" {
			filePath = field("notebook_path")
		}
	}
	return description, filePath
}

// redactToolInput scrubs secret-shaped values out of a tool_input payload
// (plan §P2 "redaction is a precondition", gd-sWwPnwkZtw) before it ever
// leaves the agent pod — the FIRST of two independent redaction layers, the
// second being the HOOK_EVENTS consumer scrubbing again on write
// (internal/toollatency.RedactJSON). v is nil for a tool with no input (or a
// malformed payload); returned unchanged in that case.
func redactToolInput(v any) any {
	if v == nil {
		return nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return v
	}
	redacted := toollatency.RedactJSON(data)
	var out any
	if err := json.Unmarshal(redacted, &out); err != nil {
		return v
	}
	return out
}

// numberToInt64 extracts an integer from v, which comes from encoding/json's
// map[string]any decoding and so is a float64 (or, for a caller that used
// json.Decoder.UseNumber, a json.Number). Returns false for anything else,
// including a missing key (nil).
func numberToInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}

// readNATSToken resolves the NATS auth token, preferring the tmpfs-backed
// file at $COOP_NATS_TOKEN_FILE (W5, ka-0000006585) over the legacy
// $COOP_NATS_TOKEN env-var. File contents are trimmed of trailing whitespace
// so values produced by `echo`/heredocs work. A failed file read silently
// falls back to the env-var path; the caller decides whether an empty
// token is acceptable.
func readNATSToken() string {
	if path := os.Getenv("COOP_NATS_TOKEN_FILE"); path != "" {
		if b, err := os.ReadFile(path); err == nil {
			if tok := strings.TrimRight(string(b), "\r\n\t "); tok != "" {
				return tok
			}
		}
	}
	return os.Getenv("COOP_NATS_TOKEN")
}

// publishToNATS connects to NATS, publishes the message, and disconnects.
// Uses optimized settings: no reconnect, short timeouts.
func publishToNATS(natsURL, subject string, payload []byte) error {
	opts := []nats.Option{
		nats.Name("gb-hook-relay"),
		nats.Timeout(1 * time.Second),
		nats.NoReconnect(),
	}

	// Use NATS token if available. Prefer the tmpfs-backed file
	// ($COOP_NATS_TOKEN_FILE, W5 / ka-0000006585) over the env-var so
	// the token is not visible in /proc/self/environ. Fall back to the
	// legacy COOP_NATS_TOKEN env-var for compatibility with --local mode
	// and external operators. With $COOP_NATS_USER (stamped by the
	// controller under nats.auth.subjectIsolation) the token is sent as
	// that user's PASSWORD — multi-user server auth only matches
	// user/password entries (ka-0000000002).
	if token := readNATSToken(); token != "" {
		if user := os.Getenv("COOP_NATS_USER"); user != "" {
			opts = append(opts, nats.UserInfo(user, token))
		} else {
			opts = append(opts, nats.Token(token))
		}
	}

	nc, err := nats.Connect(natsURL, opts...)
	if err != nil {
		return fmt.Errorf("nats connect: %w", err)
	}
	defer nc.Close()

	if err := nc.Publish(subject, payload); err != nil {
		return fmt.Errorf("nats publish: %w", err)
	}

	// FlushTimeout avoids blocking indefinitely on slow connections.
	return nc.FlushTimeout(500 * time.Millisecond)
}
