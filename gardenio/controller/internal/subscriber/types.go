// Package subscriber watches for agent lifecycle events and emits them on a
// channel. The controller's main loop reads these events and translates them
// to K8s pod operations.
package subscriber

import (
	"context"
	"strings"
)

// EventType identifies the kind of beads lifecycle event.
type EventType string

const (
	// AgentSpawn means a new agent needs a pod (agent bead created).
	AgentSpawn EventType = "agent_spawn"

	// AgentDone means an agent completed its work (bot done, bead closed).
	AgentDone EventType = "agent_done"

	// AgentStuck means an agent is unresponsive (escalation).
	AgentStuck EventType = "agent_stuck"

	// AgentKill means an agent should be terminated (lifecycle shutdown).
	AgentKill EventType = "agent_kill"

	// AgentUpdate means agent bead metadata was changed (e.g., sidecar profile).
	AgentUpdate EventType = "agent_update"

	// AgentStop means an agent should be gracefully stopped (agent_state=stopping).
	AgentStop EventType = "agent_stop"

	// ConfigUpdated means a config bead was updated, potentially requiring
	// rolling restart of agent pods for the affected project.
	ConfigUpdated EventType = "config_updated"

	// BudgetExceeded means an agent hit its budget limit and may need
	// automatic failover to a fallback account.
	BudgetExceeded EventType = "budget_exceeded"

	// AccountUpdated means an account bead's fields changed (e.g., credential
	// rotation). The controller should push the updated credential to coopmux.
	AccountUpdated EventType = "account_updated"

	// SessionDone means a session bead closed (one coop restart-loop
	// iteration finished). Metrics-only: unlike the agent-bead events above,
	// this never drives a pod operation — it exists to increment
	// gardenio_session_exit_code_total from the session bead's exit_code
	// field (gd-5MTDWjwNjm). Fired once per coop run, far more often than
	// AgentDone (once per agent lifetime).
	SessionDone EventType = "session_done"
)

// Event represents a beads lifecycle event that requires a pod operation.
type Event struct {
	Type      EventType
	Project   string
	Mode      string
	Role      string // functional role from role bead (e.g., "devops", "qa")
	AgentName string
	BeadID    string            // The bead that triggered this event
	Metadata  map[string]string // Additional context from beads
}

// Watcher subscribes to BD Daemon lifecycle events and emits them on a channel.
type Watcher interface {
	// Start begins watching for beads events. Blocks until ctx is canceled.
	Start(ctx context.Context) error

	// Events returns a read-only channel of lifecycle events.
	Events() <-chan Event
}

// beadEventPayload is the event structure published by the beads daemon.
// The daemon publishes to subjects like beads.bead.created, beads.bead.updated, etc.
type beadEventPayload struct {
	Bead    beadData       `json:"bead"`
	Changes map[string]any `json:"changes,omitempty"`
}

// beadData mirrors the bead fields from the daemon's event payload.
type beadData struct {
	ID         string            `json:"id"`
	Title      string            `json:"title"`
	Type       string            `json:"type"`
	Status     string            `json:"status"`
	Labels     []string          `json:"labels"`
	Fields     map[string]string `json:"fields"`
	AgentState string            `json:"agent_state"`
	Assignee   string            `json:"assignee"`
	CreatedBy  string            `json:"created_by"`
}

// projectFromLabels extracts the project name from a label list.
// Returns the first "project:X" label's value, or empty string.
func projectFromLabels(labels []string) string {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "project:"); ok {
			return v
		}
	}
	return ""
}

// roleFromLabels extracts the role name from a label list. Returns the first
// "role:X" label's value, or empty string. A config bead's role: label scopes
// which agent role the config governs; a config with no role: label governs
// every role in the project.
func roleFromLabels(labels []string) string {
	for _, l := range labels {
		if v, ok := strings.CutPrefix(l, "role:"); ok {
			return v
		}
	}
	return ""
}

// subjectAction extracts the action from a NATS-style dotted subject.
// e.g., "beads.bead.created" -> "created"
func subjectAction(subject string) string {
	parts := strings.Split(subject, ".")
	if len(parts) > 0 {
		return parts[len(parts)-1]
	}
	return ""
}
