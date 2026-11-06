package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/bridge"
	"gardenio/controller/internal/conversation"
	"gardenio/controller/internal/oidcauth"
)

// quickSpawnSheetView is the data passed to the quick-spawn modal partial.
type quickSpawnSheetView struct {
	OperatorEmail  string
	Projects       []string
	Roles          []string
	DefaultRole    string
	Project        string // pre-selected project (e.g. when launched from /p/<project>)
	TaskID         string // optional pre-filled task id
	PromptTemplate string // pre-filled prompt textarea (from user spawn prefs)
}

// quickSpawnedAgent is one row in the result view — a single spawned agent
// bead with its display name. Used by both single- and multi-bead spawn flows.
type quickSpawnedAgent struct {
	BeadID    string
	AgentName string
}

// quickSpawnResultView is the data passed to the result partial after a
// successful POST /quick-spawn. Agents holds one entry for the legacy
// single-spawn path or N entries for per-bead spawn-from-selection.
type quickSpawnResultView struct {
	// BeadID/AgentName mirror Agents[0] for backwards-compatible templates and
	// tests that read the legacy single-agent shape.
	BeadID    string
	AgentName string
	Project   string
	Role      string
	Agents    []quickSpawnedAgent
	Error     string
	// TaskID is the first selected task bead id when present — lets the
	// inline error template preserve task context across a Try again click.
	TaskID string
}

// defaultQuickSpawnRoles is the role picker option list. Mirrors the iOS
// QuickSpawnSheet's implicit role set; kept short so the picker stays usable.
var defaultQuickSpawnRoles = []string{"crew", "gardener", "thread"}

// handleQuickSpawnInline renders the inline spawn-agent form (used by the
// "Try again" / "Spawn another" buttons on the inline result and as a
// standalone route for re-rendering the bottom-of-page form fragment).
func (s *Server) handleQuickSpawnInline(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	view := s.buildQuickSpawnSheetView(r, email)
	if wantsJSON(r) {
		writeQuickSpawnForm(w, view)
		return
	}
	s.renderPartial(w, r, "templates/partials/quick_spawn.html", "quick_spawn_inline", map[string]any{
		"Sheet": view,
	})
}

// quickSpawnForm is the JSON shape of the inline form (GET /quick-spawn-inline
// with ?format=json or Accept: application/json): what the in-garden bead
// panel's "Put an agent on this" fills its own form with (Lands WB-12). The
// role and the prompt are the viewer's spawn preferences, the prompt already
// rendered against the task; Models are the short model aliases, an empty
// choice keeping the role's own model.
type quickSpawnForm struct {
	Project string   `json:"project"`
	TaskID  string   `json:"task_id,omitempty"`
	Role    string   `json:"role"`
	Roles   []string `json:"roles"`
	Models  []string `json:"models"`
	Prompt  string   `json:"prompt,omitempty"`
}

func writeQuickSpawnForm(w http.ResponseWriter, v quickSpawnSheetView) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(quickSpawnForm{
		Project: v.Project, TaskID: v.TaskID, Role: v.DefaultRole, Roles: v.Roles, Models: bridge.ModelAliasNames(), Prompt: v.PromptTemplate,
	})
}

// handleQuickSpawnSheet renders the modal that the "New Agent" button opens.
// It is fetched via htmx and swapped into a host container in the page; the
// underlying form posts back to /quick-spawn.
func (s *Server) handleQuickSpawnSheet(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	view := s.buildQuickSpawnSheetView(r, email)
	s.renderPartial(w, r, "templates/partials/quick_spawn.html", "quick_spawn_sheet", map[string]any{
		"Sheet": view,
	})
}

// buildQuickSpawnSheetView resolves the operator's project list, role default,
// and prompt-template defaults. Shared between the modal sheet route and the
// inline form pre-rendered on bead-show pages — the latter lets operators
// spawn an agent without leaving the bead.
func (s *Server) buildQuickSpawnSheetView(r *http.Request, email string) quickSpawnSheetView {
	view := quickSpawnSheetView{
		OperatorEmail: email,
		Roles:         defaultQuickSpawnRoles,
		Project:       strings.TrimSpace(r.URL.Query().Get("project")),
		TaskID:        strings.TrimSpace(r.URL.Query().Get("task")),
	}

	// Apply user spawn preferences as defaults. Explicit URL-query overrides
	// stay authoritative; prefs sit between query params and the static
	// gardenio/crew fallbacks below.
	prefs := s.lookupUserPreferences(r.Context(), email)
	if view.Project == "" {
		view.Project = strings.TrimSpace(prefs["default_spawn_project"])
	}
	view.DefaultRole = strings.TrimSpace(prefs["default_spawn_role"])
	view.PromptTemplate = prefs["default_spawn_prompt_template"]

	if view.PromptTemplate != "" && view.TaskID != "" &&
		(strings.Contains(view.PromptTemplate, "{{title}}") || strings.Contains(view.PromptTemplate, "{{id}}")) {
		if first, err := s.daemon.GetBead(r.Context(), view.TaskID); err == nil && first != nil {
			view.PromptTemplate = renderPromptTemplate(view.PromptTemplate, first)
		} else if err != nil {
			s.logger.Warn("loading task bead for prompt template render", "task", view.TaskID, "error", err)
		}
	}

	projects, err := s.daemon.ListProjectBeads(r.Context())
	if err != nil {
		s.logger.Warn("listing projects for quick-spawn", "error", err)
	}
	names := make([]string, 0, len(projects)+1)
	for name := range projects {
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if !slices.Contains(names, "gardenio") {
		names = append([]string{"gardenio"}, names...)
	}
	view.Projects = names
	if view.Project == "" {
		view.Project = "gardenio"
	}
	if view.DefaultRole == "" {
		view.DefaultRole = "crew"
	}
	return view
}

// handleQuickSpawn validates the form, generates an agent name, creates the
// agent bead(s), and applies project: + role: labels and `assigned`
// dependencies to selected task beads.
//
// Multi-bead spawn-from-selection: the form may carry repeated `task_ids[]`
// values plus an optional `spawn_per_selection` mode (single|per-bead).
//
//   - "single" (default): one agent bead, one `assigned` dep per selected
//     task.
//   - "per-bead": N agent beads, each with one `assigned` dep.
//
// When project / role / prompt / spawn_per_selection are missing on the form,
// values are filled from the operator's user-bead preferences
// (default_spawn_project, default_spawn_role, default_spawn_prompt_template,
// spawn_per_selection). The prompt template is rendered with simple
// {{title}} / {{id}} substitution against the first selected bead.
func (s *Server) handleQuickSpawn(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	if email == "" {
		http.Error(w, "Unauthenticated", http.StatusUnauthorized)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form", http.StatusBadRequest)
		return
	}
	project := strings.TrimSpace(r.PostForm.Get("project"))
	role := strings.TrimSpace(r.PostForm.Get("role"))
	prompt := strings.TrimSpace(r.PostForm.Get("prompt"))
	rawModel := strings.TrimSpace(r.PostForm.Get("model"))
	spawnMode := strings.TrimSpace(r.PostForm.Get("spawn_per_selection"))
	// display=inline picks the inline-result template so the per-bead form at
	// the bottom of bead-show can swap its response in place instead of
	// rendering a modal sheet. Defaults to the modal sheet for backwards
	// compatibility with the nav "+ New Agent" button.
	inline := r.PostForm.Get("display") == "inline"

	taskIDs := collectTaskIDs(r.PostForm["task_ids[]"], r.PostForm.Get("task_id"))

	// Fall back to user preferences for any missing field. Best-effort: if the
	// user bead lookup fails, we keep the missing-field error behavior below.
	if project == "" || role == "" || prompt == "" || spawnMode == "" {
		if prefs := s.lookupUserPreferences(r.Context(), email); prefs != nil {
			if project == "" {
				project = strings.TrimSpace(prefs["default_spawn_project"])
			}
			if role == "" {
				role = strings.TrimSpace(prefs["default_spawn_role"])
			}
			if prompt == "" {
				prompt = strings.TrimSpace(prefs["default_spawn_prompt_template"])
			}
			if spawnMode == "" {
				spawnMode = strings.TrimSpace(prefs["spawn_per_selection"])
			}
		}
	}
	if spawnMode == "" {
		spawnMode = "single"
	}

	// Stamp the operator on the daemon ctx so write calls (CreateBead,
	// AddLabel, AddDependency) carry X-Operated-By. Without this the kbeads
	// audit resolver fail-closes with HTTP 400 "operator could not be
	// resolved" (R2 enforcement, ka-0000005772) because beads-viewer talks
	// to the daemon with a static service token that has no OIDC subject.
	ctx := beadsapi.WithOperator(r.Context(), canonicalOIDC(email))

	// Render the prompt template against the first selected bead, when one
	// exists. Substitution is intentionally simple: {{title}} / {{id}}.
	if prompt != "" && len(taskIDs) > 0 && (strings.Contains(prompt, "{{title}}") || strings.Contains(prompt, "{{id}}")) {
		if first, err := s.daemon.GetBead(ctx, taskIDs[0]); err == nil && first != nil {
			prompt = renderPromptTemplate(prompt, first)
		} else if err != nil {
			s.logger.Warn("loading first selected bead for prompt template", "task", taskIDs[0], "error", err)
		}
	}

	if project == "" {
		s.renderQuickSpawnError(w, r, "Project is required.", http.StatusBadRequest, inline)
		return
	}
	if prompt == "" {
		s.renderQuickSpawnError(w, r, "Prompt is required — describe what the agent should do.", http.StatusBadRequest, inline)
		return
	}
	if role == "" {
		role = "crew"
	}
	// An optional model alias (opus, sonnet, …, or a full allowlisted id), the
	// same vocabulary as a Slack --model override; empty keeps the role's model.
	model := ""
	if rawModel != "" {
		m, ok := bridge.NormalizeModel(rawModel)
		if !ok {
			s.renderQuickSpawnError(w, r, "Model must be one of "+strings.Join(bridge.ModelAliasNames(), ", ")+".", http.StatusBadRequest, inline)
			return
		}
		model = m
	}
	// Spawn rights (Lands Q8c, gd-fPXoeHDQS3): the land's owner and members may
	// start agents in it, guests may not. An unrestricted caller is the operator.
	if !viewerScopeFromContext(r.Context()).may(project, conversation.ActionSpawn) {
		s.renderQuickSpawnError(w, r, "Only the owner and members of "+project+" may start agents there.", http.StatusForbidden, inline)
		return
	}

	// Decide spawn count. "per-bead" with N tasks → N agents; otherwise a
	// single agent gets every selected task as an `assigned` dep.
	perBead := spawnMode == "per-bead" && len(taskIDs) > 0

	var spawned []quickSpawnedAgent
	if perBead {
		for _, tid := range taskIDs {
			id, name, err := s.spawnOne(ctx, email, project, role, model, prompt, []string{tid})
			if err != nil {
				s.logger.Error("creating agent bead (per-bead)", "task", tid, "error", err)
				s.renderQuickSpawnError(w, r, "Failed to spawn agent.", http.StatusBadGateway, inline)
				return
			}
			spawned = append(spawned, quickSpawnedAgent{BeadID: id, AgentName: name})
		}
	} else {
		id, name, err := s.spawnOne(ctx, email, project, role, model, prompt, taskIDs)
		if err != nil {
			s.logger.Error("creating agent bead", "error", err)
			s.renderQuickSpawnError(w, r, "Failed to spawn agent.", http.StatusBadGateway, inline)
			return
		}
		spawned = append(spawned, quickSpawnedAgent{BeadID: id, AgentName: name})
	}

	result := quickSpawnResultView{
		Project: project,
		Role:    role,
		Agents:  spawned,
	}
	if len(taskIDs) > 0 {
		result.TaskID = taskIDs[0]
	}
	if len(spawned) > 0 {
		result.BeadID = spawned[0].BeadID
		result.AgentName = spawned[0].AgentName
	}
	if wantsActionJSON(r) {
		writeActionJSON(w, actionResponse{OK: true, Bead: result.BeadID, Agent: result.AgentName})
		return
	}
	tmpl := "quick_spawn_result"
	if inline {
		tmpl = "quick_spawn_inline_result"
	}
	s.renderPartial(w, r, "templates/partials/quick_spawn.html", tmpl, map[string]any{
		"Result": result,
	})
}

// spawnOne creates a single agent bead, applies project:/role: labels, and
// adds one `assigned` dependency per task ID. A non-empty model is the agent's
// model override (the controller pins it, podspec applyAgentModelOverride).
// Returns the bead ID and the generated agent name.
func (s *Server) spawnOne(ctx context.Context, email, project, role, model, prompt string, taskIDs []string) (string, string, error) {
	agentName, err := generateAgentName(project, prompt)
	if err != nil {
		return "", "", fmt.Errorf("generating agent name: %w", err)
	}

	fields := map[string]string{
		"agent":   agentName,
		"mode":    "crew",
		"role":    role,
		"project": project,
		"prompt":  prompt,
	}
	if model != "" {
		fields["model"] = model
	}
	// Preserve the legacy single-task field for downstream consumers that read
	// task_id directly off the agent bead. With multiple tasks, the dep edges
	// are the source of truth.
	if len(taskIDs) == 1 {
		fields["task_id"] = taskIDs[0]
	}
	fieldsJSON, err := json.Marshal(fields)
	if err != nil {
		return "", "", fmt.Errorf("marshalling agent fields: %w", err)
	}

	description := prompt
	switch len(taskIDs) {
	case 0:
		// no task assignment; description is just the prompt
	case 1:
		description = "Assigned to task: " + taskIDs[0] + "\n\n" + prompt
	default:
		description = "Assigned to tasks: " + strings.Join(taskIDs, ", ") + "\n\n" + prompt
	}

	req := beadsapi.CreateBeadRequest{
		Title:       agentName,
		Type:        "agent",
		Description: description,
		CreatedBy:   email,
		Fields:      fieldsJSON,
	}
	id, err := s.daemon.CreateBead(ctx, req)
	if err != nil {
		return "", "", fmt.Errorf("creating bead: %w", err)
	}

	// Best-effort labels and dependencies — log and continue. The agent bead
	// itself is the load-bearing artifact.
	if err := s.daemon.AddLabel(ctx, id, "project:"+project); err != nil {
		s.logger.Warn("adding project label", "agent", agentName, "bead", id, "error", err)
	}
	for rr := range strings.SplitSeq(role, ",") {
		rr = strings.TrimSpace(rr)
		if rr == "" {
			continue
		}
		if err := s.daemon.AddLabel(ctx, id, "role:"+rr); err != nil {
			s.logger.Warn("adding role label", "agent", agentName, "bead", id, "role", rr, "error", err)
		}
	}
	for _, tid := range taskIDs {
		if err := s.daemon.AddDependency(ctx, id, tid, "assigned", agentName); err != nil {
			s.logger.Warn("adding task dependency", "agent", agentName, "bead", id, "task", tid, "error", err)
		}
	}
	return id, agentName, nil
}

// collectTaskIDs merges legacy `task_id` and repeated `task_ids[]` form values
// into a single deduplicated, trimmed slice while preserving caller order.
func collectTaskIDs(multi []string, single string) []string {
	out := make([]string, 0, len(multi)+1)
	seen := map[string]struct{}{}
	add := func(raw string) {
		t := strings.TrimSpace(raw)
		if t == "" {
			return
		}
		if _, dup := seen[t]; dup {
			return
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	for _, t := range multi {
		add(t)
	}
	add(single)
	return out
}

// renderPromptTemplate substitutes {{title}} / {{id}} on the prompt template
// against a single bead. Substitution is literal — no nested templating, no
// expression syntax.
func renderPromptTemplate(tpl string, b *beadsapi.BeadDetail) string {
	r := strings.NewReplacer(
		"{{title}}", b.Title,
		"{{id}}", b.ID,
	)
	return r.Replace(tpl)
}

// lookupUserPreferences resolves the operator's user bead by email and
// returns its `preferences` map. Returns nil on any lookup failure — callers
// must tolerate a missing or unparseable preferences blob.
func (s *Server) lookupUserPreferences(ctx context.Context, email string) map[string]string {
	if email == "" {
		return nil
	}
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"user"},
		Kinds:    []string{"config"},
		Statuses: []string{"open", "in_progress"},
		Limit:    500,
	})
	if err != nil {
		s.logger.Warn("listing user beads for prefs", "email", email, "error", err)
		return nil
	}
	if res == nil {
		return nil
	}
	var bead *beadsapi.BeadDetail
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		if b.Fields["email"] == email {
			bead = b
			break
		}
	}
	if bead == nil {
		return nil
	}
	raw := bead.Fields["preferences"]
	if raw == "" || raw == "null" {
		return nil
	}
	var prefs map[string]string
	if err := json.Unmarshal([]byte(raw), &prefs); err != nil {
		s.logger.Warn("parsing user preferences", "email", email, "error", err)
		return nil
	}
	return prefs
}

func (s *Server) renderQuickSpawnError(w http.ResponseWriter, r *http.Request, msg string, status int, inline bool) {
	if respondAction(w, r, "", actionFailure(msg, status)) {
		return
	}
	w.WriteHeader(status)
	tmpl := "quick_spawn_result"
	if inline {
		tmpl = "quick_spawn_inline_result"
	}
	s.renderPartial(w, r, "templates/partials/quick_spawn.html", tmpl, map[string]any{
		"Result": quickSpawnResultView{Error: msg},
	})
}

// generateAgentName builds an agent identifier in the iOS spawnFreeAgent shape:
// "{project}-{slug-prefix-30}-{4-hex-suffix}". The 4-hex suffix prevents
// collisions when the slug prefix is identical across spawns.
func generateAgentName(project, prompt string) (string, error) {
	slug := slugify(prompt)
	if len(slug) > 30 {
		slug = slug[:30]
		slug = strings.TrimRight(slug, "-")
	}
	if slug == "" {
		slug = "agent"
	}
	var buf [2]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("reading random bytes: %w", err)
	}
	return project + "-" + slug + "-" + hex.EncodeToString(buf[:]), nil
}

// slugify reduces s to lowercase ASCII letters/digits joined by single dashes.
// Any other rune becomes a separator; runs of separators collapse.
func slugify(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	prevDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := b.String()
	return strings.Trim(out, "-")
}
