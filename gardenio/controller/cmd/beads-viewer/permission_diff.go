package main

import (
	"context"
	"net/http"
	"strings"

	"gardenio/controller/internal/beadsapi"
)

// agentSpawnEscalationID returns the escalation an agent was replayed from,
// or "" for an ordinary spawn. stampSpawnProvenance (kbeads
// http_escalations.go) writes the approving escalation's ID into spawned_by
// together with spawn_approval_mode; without the mode, spawned_by is just the
// spawning agent's name.
func agentSpawnEscalationID(bead *beadsapi.BeadDetail) string {
	if bead == nil || bead.Type != "agent" || bead.Fields["spawn_approval_mode"] == "" {
		return ""
	}
	return bead.Fields["spawned_by"]
}

// agentPermissionDiffView is the agent page's live-vs-approved ServiceAccount
// permission diff (gd-SyEYkqe601, design doc gd-sGW5Y1B8sg §8 point 9). A
// zero view (Available false) renders an empty, non-polling section.
type agentPermissionDiffView struct {
	Available      bool
	SnapshotAt     string
	ServiceAccount string
	Widened        bool
	Unknown        bool
	Namespaces     []beadsapi.NamespacePermissionDiff
}

// buildAgentPermissionDiffView fetches the diff for bead's originating
// escalation. A daemon error degrades to a zero view rather than failing the
// fragment, like buildAgentSpawnBadge.
func (s *Server) buildAgentPermissionDiffView(ctx context.Context, bead *beadsapi.BeadDetail) agentPermissionDiffView {
	escalationID := agentSpawnEscalationID(bead)
	if escalationID == "" {
		return agentPermissionDiffView{}
	}
	diff, ok, err := s.daemon.GetEscalationPermissionDiff(ctx, escalationID)
	if err != nil {
		s.logger.Warn("getting spawn permission diff", "agent", bead.ID, "escalation", escalationID, "error", err)
		return agentPermissionDiffView{}
	}
	if !ok || diff == nil {
		return agentPermissionDiffView{}
	}
	return agentPermissionDiffView{
		Available:      true,
		SnapshotAt:     diff.SnapshotAt,
		ServiceAccount: diff.ServiceAccount,
		Widened:        diff.Widened,
		Unknown:        diff.Unknown,
		Namespaces:     diff.Namespaces,
	}
}

// permissionDiffRuleLabel renders a diff row's rule the way
// `kubectl auth can-i --list` names it: "pods", "jobs.batch",
// "secrets [app-token]", or a non-resource URL.
func permissionDiffRuleLabel(r beadsapi.PermissionDiffRow) string {
	if r.NonResourceURL != "" {
		return r.NonResourceURL
	}
	label := r.Resource
	if r.APIGroup != "" {
		label += "." + r.APIGroup
	}
	if len(r.ResourceNames) > 0 {
		label += " [" + strings.Join(r.ResourceNames, " ") + "]"
	}
	return label
}

// handleAgentPermissionDiff serves the fragment agent_permission_diff.html
// loads and polls. Read-authz first, like handleBeadActivity: GetBead
// surfaces the daemon's 403/404 before any diff is fetched.
func (s *Server) handleAgentPermissionDiff(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "Missing bead id", http.StatusBadRequest)
		return
	}
	bead, err := s.daemon.GetBead(r.Context(), id)
	if err != nil {
		if beadMissing(err) {
			http.Error(w, "Bead not found", http.StatusNotFound)
			return
		}
		s.logger.Error("getting bead for permission diff", "id", id, "error", err)
		http.Error(w, "Failed to get bead", http.StatusInternalServerError)
		return
	}
	s.renderPartial(w, r, "templates/partials/agent_permission_diff.html", "agent_permission_diff", map[string]any{
		"Bead":           bead,
		"PermissionDiff": s.buildAgentPermissionDiffView(r.Context(), bead),
	})
}
