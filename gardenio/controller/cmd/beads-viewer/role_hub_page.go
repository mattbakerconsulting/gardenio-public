package main

// Page + lazy-fragment HTTP handlers for the rich role hub at /role/<name>
// (epic ka-0000001026, task ka-0000004054, design doc §8.5). This is the
// frontend that sits on top of the backend assembler (role_hub.go,
// task ka-0000006170): it mirrors handleProject / handleGardenerDetail — a
// viewer-scoped read (plain HandleFunc, gated by the ambient auth middleware)
// that renders an eager Overview and lazy-loads the one expensive section.
//
//	GET /role/{role}         — the role hub page (eager overrides + config +
//	                           advice; agents panel is a lazy shell)
//	GET /b/role/{role}/agents — the lazy cross-project agents fragment
//
// The agents fragment lives under /b/ to dodge the net/http.ServeMux overlap
// rejection with /role/{role} (exactly as the project-hub fragments take a /b/
// prefix to avoid colliding with /p/{project}, project_hub_sections.go).
//
// Best-effort throughout (mirrors handleProject / the gardener page): a daemon
// error degrades a single panel — the eager assembler swallows its own errors
// (buildRoleHubEager), and the agents fragment renders an empty section with a
// retry affordance rather than a 500.

import (
	"net/http"
	"strings"

	"gardenio/controller/internal/advice"
)

// roleHubAgentsSlug is the DOM id + htmx swap target for the role hub's lazy
// cross-project agents section, shared by the page shell and the fragment so a
// reveal/retry round-trips to the same element.
const roleHubAgentsSlug = "role-hub-agents"

// handleRoleHub renders the role hub page at /role/<name>. It builds the eager
// half of the view-model (resolved overrides + role-scoped config + advice) on
// first paint and emits a lazy shell for the cross-project agents panel, which
// HTMX fetches from /b/role/{role}/agents on reveal (gardener-page eager-head +
// lazy-tail skeleton, gardener_page.go).
//
// Read → viewer-scoped: a plain HandleFunc gated by the ambient auth
// middleware, exactly like handleProject. The only hard input is the role name;
// a blank one is a 400 (the route never matches an empty segment, but the guard
// keeps the handler honest if called directly).
func (s *Server) handleRoleHub(w http.ResponseWriter, r *http.Request) {
	role := strings.TrimSpace(r.PathValue("role"))
	if role == "" {
		http.Error(w, "Missing role", http.StatusBadRequest)
		return
	}

	view := s.buildRoleHubEager(r.Context(), role, s.now())

	data := map[string]any{
		"Role":          view,
		"AgentsSlug":    roleHubAgentsSlug,
		"AgentsLazyURL": s.basePath + "/b/role/" + role + "/agents",
		// Reuse the advice-scope "Role: <name>" vocabulary (advice/matching.go)
		// so the hub header reads identically to the advice tab's role group.
		"ScopeHeader": advice.BuildScopeHeader("role", role),
	}
	s.render(w, r, "role_hub.html", data)
}

// handleRoleHubAgents renders the lazy cross-project agents fragment for the
// role hub: every active (open + in_progress) agent carrying this role across
// ALL projects, grouped by project. Reuses buildRoleHubAgents (the same
// label+field union the composed buildRoleHub uses) so the lazy panel and the
// composed view-model never drift.
//
// Best-effort: buildRoleHubAgents already logs-and-degrades each query to an
// empty contribution, so a daemon error surfaces as an empty section rather
// than a 500 — there is no error to propagate. An empty result renders the
// quiet empty-state.
func (s *Server) handleRoleHubAgents(w http.ResponseWriter, r *http.Request) {
	role := strings.TrimSpace(r.PathValue("role"))
	if role == "" {
		http.NotFound(w, r)
		return
	}

	groups, count := s.buildRoleHubAgents(r.Context(), role, s.now())

	data := map[string]any{
		"Slug":        roleHubAgentsSlug,
		"Role":        role,
		"AgentGroups": groups,
		"AgentCount":  count,
	}
	s.renderPartial(w, r, "templates/partials/role_hub_agents.html", "role_hub_agents", data)
}
