package main

import (
	"context"
	"fmt"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// agentSpawnBadgeView is the spawn-provenance badge rendered on the agent
// detail card (type_agent.html), next to the existing "spawned by" chip
// (gd-jbY5UiWlN4). Label is already the full display text; Class selects the
// CSS modifier (agent-spawn-badge-<Class> in layout.html). An empty Label
// suppresses the badge entirely — it is a problem signal (or a confirmation
// of an approval), never decoration on an ordinary spawn.
type agentSpawnBadgeView struct {
	Label string
	Class string // "approved" | "policy" | "unreviewed"
}

// buildAgentSpawnBadge derives the three-way spawn badge for an agent bead,
// mirroring the `kd show` CLI badge (kbeads/cmd/kd/agent_spawn_badge.go) so
// both surfaces render the same three cases distinctly:
//
//   - spawn_approval_mode=auto   → "policy-approved"
//   - spawn_approval_mode=human  → "human-approved" (+ spawn_approved_by when set)
//   - neither field, but the agent's role is privileged (per a type:role bead's
//     "privileged" field) → red "unreviewed"
//
// Every other case (non-privileged role, or a role that can't be resolved)
// returns a zero view. The role-privilege lookup is best-effort: a daemon
// error is treated as "not privileged" so a transient lookup failure never
// blocks the page.
func (s *Server) buildAgentSpawnBadge(ctx context.Context, bead *beadsapi.BeadDetail) agentSpawnBadgeView {
	if bead == nil || bead.Type != "agent" || len(bead.Fields) == 0 {
		return agentSpawnBadgeView{}
	}

	switch bead.Fields["spawn_approval_mode"] {
	case "auto":
		return agentSpawnBadgeView{Label: "policy-approved", Class: "policy"}
	case "human":
		if by := bead.Fields["spawn_approved_by"]; by != "" {
			return agentSpawnBadgeView{Label: fmt.Sprintf("human-approved (%s)", by), Class: "approved"}
		}
		return agentSpawnBadgeView{Label: "human-approved", Class: "approved"}
	}

	role := bead.Fields["role"]
	if role == "" {
		return agentSpawnBadgeView{}
	}
	roles, err := s.daemon.ListRoleBeads(ctx)
	if err != nil {
		return agentSpawnBadgeView{}
	}
	// Any element of a comma-joined role counts, the same split the daemon's
	// spawn gate and the controller's pod spec use (gd-Yhbc5wdjwj).
	privileged := false
	for _, r := range strings.Split(role, ",") {
		if info, ok := roles[strings.TrimSpace(r)]; ok && info.Privileged {
			privileged = true
			break
		}
	}
	if !privileged {
		return agentSpawnBadgeView{}
	}
	return agentSpawnBadgeView{
		Label: "unreviewed — privileged role, no spawn approval on record",
		Class: "unreviewed",
	}
}
