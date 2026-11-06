package main

import (
	"context"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// requestOrigin is the "where did this come from" strip shared by decisions
// and escalations (Slack thread gd-AREKDbHx70): the agent that filed it, the
// task it blocks, and the plan that task belongs to. Each link carries a
// resolved title so the operator reads "for Rename gardener → gardener" instead
// of a bare gd-xxxx id, and the agent reads by NAME (its bead title) instead
// of the bead id `gb escalation create` stamps into requesting_agent.
//
// Every lookup is best-effort: a batch-fetch miss leaves the id-only link so
// the anchor still resolves to /{id}, and a transport error degrades the whole
// strip to id-only links rather than failing the page.
type requestOrigin struct {
	// Agent is the requesting agent bead. Title is the agent NAME (the agent
	// bead's title). Nil when the bead carries no agent bead id.
	Agent *originLink
	// AgentName is the display name when there is no resolvable agent bead
	// (legacy escalations stamped a bare name; decisions carry requested_by).
	// Empty when Agent is set and already names the agent.
	AgentName string
	// Task is the requesting task (fields.requesting_task).
	Task *originLink
	// Plan is the owning plan (fields.requesting_plan); links to /plans/{id}.
	Plan *originLink
}

// originLink is one bead reference in the origin strip. Title is best-effort
// and empty on a lookup miss; the template falls back to the id.
type originLink struct {
	ID    string
	Title string
}

// Label is the text the template renders for the link: the title when it was
// resolved, else the bare id.
func (l *originLink) Label() string {
	if l == nil {
		return ""
	}
	if l.Title != "" {
		return l.Title
	}
	return l.ID
}

// Any reports whether the strip has anything to render.
func (o requestOrigin) Any() bool {
	return o.Agent != nil || o.AgentName != "" || o.Task != nil || o.Plan != nil
}

// WithoutAgent returns the strip minus the agent, for surfaces that already
// render the agent in their own "requested by" slot and only want the task +
// plan links appended.
func (o requestOrigin) WithoutAgent() requestOrigin {
	return requestOrigin{Task: o.Task, Plan: o.Plan}
}

// originRefs pulls the raw origin references off a decision or escalation
// bead. The two types spell the agent differently:
//
//   - decision:   requesting_agent_bead_id = agent BEAD id,
//     requested_by / requesting_agent = agent NAME (gb decision create).
//   - escalation: requesting_agent = agent BEAD id (gb escalation create reads
//     BOAT_AGENT_BEAD_ID), but older rows carry a bare agent name.
//
// A value that matches the bead-id shape is treated as a bead id; anything
// else is a display name.
func originRefs(b *beadsapi.BeadDetail) (agentID, agentName, taskID, planID string) {
	if b == nil {
		return "", "", "", ""
	}
	f := b.Fields
	taskID = strings.TrimSpace(f["requesting_task"])
	planID = strings.TrimSpace(f["requesting_plan"])

	agentID = strings.TrimSpace(f["requesting_agent_bead_id"])
	ra := strings.TrimSpace(f["requesting_agent"])
	if agentID == "" && isBareBeadID(ra) {
		agentID = ra
		ra = ""
	}
	agentName = firstNonEmptyStr(strings.TrimSpace(f["requested_by"]), ra)
	if agentName == "" && b.Type == "decision" {
		agentName = strings.TrimSpace(b.CreatedBy)
	}
	return agentID, agentName, taskID, planID
}

// isBareBeadID reports whether s is exactly one bead id (no anchor, no
// surrounding text).
func isBareBeadID(s string) bool {
	if s == "" {
		return false
	}
	m := beadIDRe.FindString(s)
	return m == s && !strings.Contains(s, "#")
}

// resolveRequestOrigins builds the origin strip for every bead in one
// BatchGetBeads round-trip (list pages render up to 200 rows; a per-row
// GetBead would be an N+1 against the daemon). The result is keyed by the
// source bead's id. Beads with no origin references get a zero requestOrigin.
func (s *Server) resolveRequestOrigins(ctx context.Context, beads []*beadsapi.BeadDetail) map[string]requestOrigin {
	out := make(map[string]requestOrigin, len(beads))
	if len(beads) == 0 {
		return out
	}
	type refs struct{ agentID, agentName, taskID, planID string }
	byBead := make(map[string]refs, len(beads))
	want := make(map[string]struct{})
	for _, b := range beads {
		if b == nil {
			continue
		}
		r := refs{}
		r.agentID, r.agentName, r.taskID, r.planID = originRefs(b)
		byBead[b.ID] = r
		for _, id := range []string{r.agentID, r.taskID, r.planID} {
			if id != "" {
				want[id] = struct{}{}
			}
		}
	}

	var resolved map[string]*beadsapi.BeadDetail
	if len(want) > 0 && s != nil && s.daemon != nil {
		ids := make([]string, 0, len(want))
		for id := range want {
			ids = append(ids, id)
		}
		if got, err := s.daemon.BatchGetBeads(ctx, ids); err == nil {
			resolved = got
		} else if s.logger != nil {
			s.logger.Warn("request origin: batch fetch failed; rendering id-only links", "error", err, "ids", len(ids))
		}
	}

	link := func(id string) *originLink {
		if id == "" {
			return nil
		}
		l := &originLink{ID: id}
		if b := resolved[id]; b != nil {
			l.Title = strings.TrimSpace(b.Title)
		}
		return l
	}

	for _, b := range beads {
		if b == nil {
			continue
		}
		r := byBead[b.ID]
		o := requestOrigin{
			Agent: link(r.agentID),
			Task:  link(r.taskID),
			Plan:  link(r.planID),
		}
		// The agent bead's title IS the agent name. When the batch fetch missed
		// (or there is no bead id at all) fall back to the stamped name so the
		// strip never shows a bare bead id where a name was available.
		if o.Agent != nil && o.Agent.Title == "" && r.agentName != "" {
			o.Agent.Title = r.agentName
		}
		if o.Agent == nil {
			o.AgentName = r.agentName
		}
		out[b.ID] = o
	}
	return out
}

// resolveRequestOrigin is the single-bead form of resolveRequestOrigins, for
// the per-bead card renders (/{id}, the approve/reject swap payload).
func (s *Server) resolveRequestOrigin(ctx context.Context, b *beadsapi.BeadDetail) requestOrigin {
	if b == nil {
		return requestOrigin{}
	}
	return s.resolveRequestOrigins(ctx, []*beadsapi.BeadDetail{b})[b.ID]
}
