package gardenhub

import (
	"encoding/json"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// Relevance says which snapshot an upstream bead event can change. The daemon
// already puts the whole bead on beads.bead.* events (kbeads/internal/events:
// BeadCreated, BeadUpdated, BeadClosed), so the type and status are free to
// read. Measured 2026-10-21: of 284 mutations in ten minutes none was a farm
// work type (114 agent, 78 trigger, 43 message, ...), yet each one re-read
// 876 open work beads (3.44 MB) in each viewer replica. Plan
// docs/superpowers/plans/2026-10-21-aws-cost-cuts.md Task B2.
type Relevance struct {
	Hub    bool // Hub.fetch: in_progress beads of any type, open decisions, escalations and agents
	Farm   bool // Farm.readRows: the farmWorkTypes rows
	Roster bool // the farm's live-agent roster and gardeners (type agent)
}

// relevantAll is a (re)connect, or an event we cannot read: anything may have
// changed. It is the old behaviour, so failing open costs bytes, never
// freshness.
var relevantAll = Relevance{Hub: true, Farm: true, Roster: true}

// hubTypes change Hub.fetch whatever their status: it lists open decisions and
// escalations, and a report feeds the in_progress rows' has_report.
var hubTypes = map[string]bool{"decision": true, "escalation": true, "report": true}

// farmTypeSet is the work the farm draws plus the produce it lists in each
// plan's bin (a bundle or report written or closed changes a bin; a merge
// request's state reaches the farm as a field change on its step) and the
// evidence a closed step's harvest reads (farm_harvest.go).
var farmTypeSet = func() map[string]bool {
	m := make(map[string]bool, len(farmWorkTypes)+len(produceTypes)+1)
	for _, t := range farmWorkTypes {
		m[t] = true
	}
	for _, t := range produceTypes {
		m[t] = true
	}
	m["evidence"] = true
	return m
}()

type eventPayload struct {
	Bead *struct {
		Type   string `json:"type"`
		Status string `json:"status"`
	} `json:"bead"`
	// Changes carries the column transitions and, overlaid at the top level,
	// the per-key Fields transitions (kbeads perFieldChanges).
	Changes map[string]json.RawMessage `json:"changes"`
}

// panelOnlyTopics are the families eventTopics adds for the open bead panels
// (live.go): a comment, a link or a label moves a panel, and no snapshot reads
// them.
var panelOnlyTopics = []string{"beads.comment.", "beads.dependency.", "beads.label."}

func panelOnly(topic string) bool {
	for _, p := range panelOnlyTopics {
		if strings.HasPrefix(topic, p) {
			return true
		}
	}
	return false
}

func classify(ev beadsapi.SSEEvent) Relevance {
	if panelOnly(ev.Event) {
		return Relevance{}
	}
	var p eventPayload
	if len(ev.Data) == 0 || json.Unmarshal(ev.Data, &p) != nil || p.Bead == nil || p.Bead.Type == "" {
		return relevantAll
	}
	t, s := p.Bead.Type, p.Bead.Status
	_, statusChanged := p.Changes["status"]
	created := strings.HasSuffix(ev.Event, ".created")
	left := strings.HasSuffix(ev.Event, ".closed") || strings.HasSuffix(ev.Event, ".deleted") ||
		strings.HasSuffix(ev.Event, ".transferred")
	// The hub draws open agents with their agent_state. A new agent or a state
	// change is news; the controller's 60 s usage writes are not.
	_, agentState := p.Changes["agent_state"]
	agentNews := t == "agent" && (created || agentState)
	return Relevance{
		Hub:    hubTypes[t] || s == "in_progress" || statusChanged || left || agentNews,
		Farm:   farmTypeSet[t],
		Roster: t == "agent",
	}
}
