package eventbus

import (
	"encoding/json"
	"time"
)

// EventType identifies an event flowing through the bus.
// Hook events map 1:1 to Claude Code hook events; other categories
// cover decisions, agents, mail, mutations, config, gates, and jacks.
type EventType string

const (
	// Claude Code hook events.
	EventSessionStart       EventType = "SessionStart"
	EventUserPromptSubmit   EventType = "UserPromptSubmit"
	EventPreToolUse         EventType = "PreToolUse"
	EventPostToolUse        EventType = "PostToolUse"
	EventPostToolUseFailure EventType = "PostToolUseFailure"
	EventStop               EventType = "Stop"
	EventPreCompact         EventType = "PreCompact"
	EventPostCompact        EventType = "PostCompact"
	EventSubagentStart      EventType = "SubagentStart"
	EventSubagentStop       EventType = "SubagentStop"
	EventNotification       EventType = "Notification"
	EventSessionEnd         EventType = "SessionEnd"

	// Advice CRUD events.
	EventAdviceCreated EventType = "advice.created"
	EventAdviceUpdated EventType = "advice.updated"
	EventAdviceDeleted EventType = "advice.deleted"

	// Decision events.
	EventDecisionCreated   EventType = "DecisionCreated"
	EventDecisionResponded EventType = "DecisionResponded"
	EventDecisionEscalated EventType = "DecisionEscalated"
	EventDecisionExpired   EventType = "DecisionExpired"

	// Agent lifecycle events.
	EventAgentStarted   EventType = "AgentStarted"
	EventAgentStopped   EventType = "AgentStopped"
	EventAgentCrashed   EventType = "AgentCrashed"
	EventAgentIdle      EventType = "AgentIdle"
	EventAgentHeartbeat EventType = "AgentHeartbeat"

	// Mail events.
	EventMailSent EventType = "MailSent"
	EventMailRead EventType = "MailRead"

	// Bead mutation events.
	EventMutationCreate  EventType = "MutationCreate"
	EventMutationUpdate  EventType = "MutationUpdate"
	EventMutationDelete  EventType = "MutationDelete"
	EventMutationComment EventType = "MutationComment"
	EventMutationStatus  EventType = "MutationStatus"

	// Config change events.
	EventConfigSet   EventType = "ConfigSet"
	EventConfigUnset EventType = "ConfigUnset"

	// Gate state change events.
	EventGateSatisfied EventType = "GateSatisfied"
	EventGateCleared   EventType = "GateCleared"

	// Jack lifecycle events.
	EventJackOn      EventType = "jack.on"
	EventJackOff     EventType = "jack.off"
	EventJackExpired EventType = "jack.expired"
	EventJackExtend  EventType = "jack.extend"

	// Session message events (from coop NATS forwarder).
	EventAssistantText EventType = "assistant_text"
	EventThinking      EventType = "thinking"
	EventUsage         EventType = "usage"
)

// Event represents a single event flowing through the bus.
type Event struct {
	Type           EventType       `json:"hook_event_name"`
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path"`
	CWD            string          `json:"cwd"`
	PermissionMode string          `json:"permission_mode"`
	Raw            json.RawMessage `json:"-"`

	// Hook-specific fields, populated based on Type.
	ToolName     string                 `json:"tool_name,omitempty"`
	ToolInput    map[string]interface{} `json:"tool_input,omitempty"`
	ToolResponse map[string]interface{} `json:"tool_response,omitempty"`
	Prompt       string                 `json:"prompt,omitempty"`
	Source       string                 `json:"source,omitempty"`
	Model        string                 `json:"model,omitempty"`
	AgentID      string                 `json:"agent_id,omitempty"`
	AgentType    string                 `json:"agent_type,omitempty"`
	Error        string                 `json:"error,omitempty"`

	// Actor is the agent name that emitted this event (e.g., "bright-hog").
	// Set by the emit handler from the authenticated request.
	Actor string `json:"actor,omitempty"`

	// PublishedAt is set by the bus when publishing to JetStream.
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

// Result aggregates handler responses for an event.
type Result struct {
	Block    bool     `json:"block,omitempty"`
	Reason   string   `json:"reason,omitempty"`
	Inject   []string `json:"inject,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

// Event type classification methods.

func (t EventType) IsDecisionEvent() bool {
	switch t {
	case EventDecisionCreated, EventDecisionResponded,
		EventDecisionEscalated, EventDecisionExpired:
		return true
	}
	return false
}

func (t EventType) IsAgentEvent() bool {
	switch t {
	case EventAgentStarted, EventAgentStopped,
		EventAgentCrashed, EventAgentIdle,
		EventAgentHeartbeat:
		return true
	}
	return false
}

func (t EventType) IsMailEvent() bool {
	switch t {
	case EventMailSent, EventMailRead:
		return true
	}
	return false
}

func (t EventType) IsMutationEvent() bool {
	switch t {
	case EventMutationCreate, EventMutationUpdate,
		EventMutationDelete, EventMutationComment,
		EventMutationStatus:
		return true
	}
	return false
}

func (t EventType) IsGateEvent() bool {
	switch t {
	case EventGateSatisfied, EventGateCleared:
		return true
	}
	return false
}

func (t EventType) IsJackEvent() bool {
	switch t {
	case EventJackOn, EventJackOff, EventJackExpired, EventJackExtend:
		return true
	}
	return false
}

func (t EventType) IsMessageEvent() bool {
	switch t {
	case EventAssistantText, EventThinking, EventUsage:
		return true
	}
	return false
}

func (t EventType) IsConfigEvent() bool {
	switch t {
	case EventConfigSet, EventConfigUnset:
		return true
	}
	return false
}

// MutationEventPayload carries data for bead mutation events.
type MutationEventPayload struct {
	Type       string   `json:"type"`
	IssueID    string   `json:"issue_id"`
	Title      string   `json:"title,omitempty"`
	Assignee   string   `json:"assignee,omitempty"`
	Actor      string   `json:"actor,omitempty"`
	Timestamp  string   `json:"timestamp"`
	OldStatus  string   `json:"old_status,omitempty"`
	NewStatus  string   `json:"new_status,omitempty"`
	IssueType  string   `json:"issue_type,omitempty"`
	Labels     []string `json:"labels,omitempty"`
	AgentState string   `json:"agent_state,omitempty"`
}

// DecisionEventPayload carries data for decision events.
type DecisionEventPayload struct {
	DecisionID  string `json:"decision_id"`
	Question    string `json:"question"`
	Urgency     string `json:"urgency,omitempty"`
	RequestedBy string `json:"requested_by,omitempty"`
	Options     int    `json:"option_count"`
	ChosenIndex int    `json:"chosen_index,omitempty"`
	ChosenLabel string `json:"chosen_label,omitempty"`
	ResolvedBy  string `json:"resolved_by,omitempty"`
	Rationale   string `json:"rationale,omitempty"`

	// EscalatedByRole carries the role (NOT operator_id) of the actor that
	// escalated the decision. Populated on EventDecisionEscalated; labels the
	// gardenio_escalations_total metric so override analysis stays identity-free
	// (customer-1 telemetry plan §4). Operator identity is recovered from the
	// decision bead / metric exemplar, never from a live-series label.
	EscalatedByRole string `json:"escalated_by_role,omitempty"`

	// Outcome carries the escalation outcome (requested/approved/rejected/
	// expired). Populated on EventDecisionEscalated. Empty is treated as
	// "requested" — the escalation was raised but not yet resolved.
	Outcome string `json:"outcome,omitempty"`
}

// GateEventPayload carries data for gate state change events.
type GateEventPayload struct {
	GateID    string `json:"gate_id"`
	Agent     string `json:"agent"`
	Mechanism string `json:"mechanism,omitempty"`
	Actor     string `json:"actor,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Timestamp string `json:"timestamp"`
}

// JackEventPayload carries data for jack lifecycle events.
type JackEventPayload struct {
	JackID    string `json:"jack_id"`
	Target    string `json:"target"`
	Agent     string `json:"agent"`
	Reason    string `json:"reason,omitempty"`
	TTL       string `json:"ttl,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Timestamp string `json:"timestamp"`
}

// AgentEventPayload carries data for agent lifecycle events.
type AgentEventPayload struct {
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name,omitempty"`
	Role      string `json:"role,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
	Uptime    int64  `json:"uptime_sec,omitempty"`
}

// MailEventPayload carries data for mail events.
type MailEventPayload struct {
	MessageID string `json:"message_id,omitempty"`
	From      string `json:"from"`
	To        string `json:"to"`
	Subject   string `json:"subject"`
	SentAt    string `json:"sent_at,omitempty"`
}

// ConfigEventPayload carries data for config change events.
type ConfigEventPayload struct {
	Key   string `json:"key,omitempty"`
	Value string `json:"value,omitempty"`
	Actor string `json:"actor,omitempty"`
}
