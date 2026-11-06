package main

// The read-only Claude-config Roles view at /claude-config (task ka-0000006351,
// plan ka-0000005440). It renders a role×knob matrix of the EFFECTIVE per-role
// Claude Code settings — ultracode / effort / workflows / model — each cell
// carrying a provenance chip (which merge layer set it: default / global /
// project / role / agent). The effective values and their provenance are
// computed by internal/claudeconfig, the SAME resolve+merge path that
// `gb setup claude` uses to write each agent pod's ~/.claude/settings.json, so
// the matrix can never drift from what actually runs.
//
// The headline feature is legibility of the "global effort pin blocks
// ultracode's xhigh" trap (plan §Gotcha): a role that flips ultracode:true but
// inherits a global/project CLAUDE_CODE_EFFORT_LEVEL pin does NOT reach xhigh.
// The matrix flags such cells with a ⚠ trap marker so operators see it at a
// glance instead of rediscovering it the hard way.
//
// Read-only by design: this is P1. The guided editor (P2) and agents-view effort
// badges (P3) are separate follow-up tasks on the same plan.
//
// Best-effort throughout (mirrors handleRolesIndex / handleAdvice): a daemon
// error degrades the page to an error banner rather than 500-ing; a role with no
// config layer still renders (defaults-only row). The role name is the only hard
// requirement and always comes from the config-bead role: labels.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"gardenio/controller/internal/claudeconfig"
	"gardenio/controller/internal/oidcauth"
)

// claudeConfigKnobCol is one column header in the matrix.
type claudeConfigKnobCol struct {
	Key   string // KnobUltracode / KnobEffort / KnobWorkflows / KnobModel
	Label string // display header
	Hint  string // tooltip explaining the knob + its source key
}

// knobAgentType is a viewer-only pseudo-knob: it is not a claude-settings JSON
// key but a field on the type:role bead (RoleInfo). It gets its own column so the
// matrix answers "what harness agent-type does this role default to?" alongside
// the merged claude-settings knobs. Sourced from the role bead, not the merge.
const knobAgentType = "agent_type"

// claudeConfigKnobCols is the fixed column set, in KnobOrder, followed by the
// role-bead-sourced agent_type column (knobAgentType).
var claudeConfigKnobCols = []claudeConfigKnobCol{
	{claudeconfig.KnobUltracode, "ultracode", "Whether ultracode (workflow mode) is on for this role. Source: settings.ultracode."},
	{claudeconfig.KnobEffort, "effort", "Effective CLAUDE_CODE_EFFORT_LEVEL. Under ultracode this reads 'xhigh' — unless a global/project effort pin blocks it (⚠ trap)."},
	{claudeconfig.KnobWorkflows, "workflows", "Whether CLAUDE_CODE_WORKFLOWS is enabled (the presence-driven workflow switch). Source: env.CLAUDE_CODE_WORKFLOWS."},
	{claudeconfig.KnobModel, "model", "Effective model override for this role. From a claude-settings bead, or the type:role bead's model field."},
	{knobAgentType, "agent_type", "Default harness agent/subagent type for this role, if pinned. Source: the type:role bead's agent_type field."},
}

// claudeConfigCell is one rendered matrix cell.
type claudeConfigCell struct {
	Value      string // effective value, or "" → rendered as an em-dash default
	Set        bool   // false → default/unset (em-dash)
	Provenance string // default | global | project | role | agent | config | override
	Target     string // provenance target (project/role/agent name), if any
	BeadID     string // source config bead id, if any
	Chip       string // provenance chip label (e.g. "role", "global · trap")
	Trap       bool   // effort cell blocked by a non-role effort pin under ultracode
}

// claudeConfigRow is one (project, role) row in the matrix. Project is "" when
// the role's config carries no project: label, so it resolves identically in
// every project.
type claudeConfigRow struct {
	Project    string
	Role       string
	Cells      []claudeConfigCell // one per claudeConfigKnobCols, same order
	LayerCount int                // # of claude-settings config-bead layers for this role
	HasTrap    bool               // any cell in this row is a trap
	DetailHref string
}

// claudeConfigView is the assembled /claude-config page model.
type claudeConfigView struct {
	Rows       []claudeConfigRow
	Cols       []claudeConfigKnobCol
	RoleCount  int // total roles discovered (before the query filter)
	TrapCount  int // rows carrying the effort trap (across ALL roles, not just matches)
	MatchCount int // rows surviving the query filter (len(Rows))
	Query      string
	Project    string // the effective project filter ("" = every project)
	Error      string

	// ScopeWarning is set when this viewer has NO project scope configured
	// (BOAT_PROJECT unset). Resolution then carries no project: subscription, so
	// MatchesSubscriptions (internal/advice/matching.go:218-220) rejects every
	// project-scoped config bead while pods in that project still receive it.
	// UnreflectedCount is how many such beads exist.
	ScopeWarning     string
	UnreflectedCount int
}

// handleClaudeConfig renders /claude-config — the role×knob effective-config
// matrix. Read-only; viewer-scoped. Accepts a `?q=` substring filter over role
// names + effective knob values, mirroring the /advice search box.
func (s *Server) handleClaudeConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := oidcauth.EmailFromContext(ctx)
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	project := s.viewerProject
	if r.URL.Query().Has("project") {
		project = strings.TrimSpace(r.URL.Query().Get("project"))
	}

	view := s.buildClaudeConfigMatrix(ctx, query, project)

	data := map[string]any{
		"View":        view,
		"ViewerEmail": email,
		"InboxCount":  s.inboxCountForViewer(ctx, email),
	}
	s.render(w, r, "claude_config.html", data)
}

// buildClaudeConfigMatrix discovers the per-(project, role) Claude config
// vocabulary and computes each scope's effective knobs. project is the
// effective filter: "" means every project contributes a row, resolved each
// against its own project (a single viewer-wide project cannot describe a
// multi-project daemon — the same role name resolves to DIFFERENT effective
// settings in different projects). Best-effort: a daemon error yields a view
// with only Error set. A non-empty query filters rows to those whose project,
// role name, or any effective knob value contains the (case-insensitive)
// substring.
func (s *Server) buildClaudeConfigMatrix(ctx context.Context, query, project string) claudeConfigView {
	view := claudeConfigView{Cols: claudeConfigKnobCols, Query: query, Project: project}

	scopes, err := claudeconfig.DiscoverRoleScopes(ctx, s.daemon, project)
	if err != nil {
		s.logger.Error("discovering claude-config role scopes", "error", err)
		view.Error = "Failed to load role config."
		return view
	}

	// Role-bead overlay: model + agent_type live on the type:role bead, not on a
	// claude-settings bead, so pull the role beads once and fold their fields
	// into each row (model as a fallback when no settings bead pins it,
	// agent_type as its own column). Best-effort: a listing error just yields no
	// overlay — the settings-derived cells still render.
	roleBeads, rbErr := s.listRoleConfigBeads(ctx)
	if rbErr != nil {
		s.logger.Warn("claude-config: listing role beads for overlay", "error", rbErr)
	}

	// Union the vocabulary: DiscoverRoleScopes returns scopes with an OPEN
	// claude-settings bead, but a role whose only Claude-relevant config is a
	// model/agent_type field on its type:role bead (e.g. crew, whose settings
	// bead may be closed) would otherwise be invisible. Role beads carry no
	// project, so add such a role as a project-agnostic ("") scope.
	seenRoles := make(map[string]struct{}, len(scopes))
	for _, sc := range scopes {
		seenRoles[sc.Role] = struct{}{}
	}
	for name, rb := range roleBeads {
		if _, ok := seenRoles[name]; ok {
			continue
		}
		if strings.TrimSpace(rb.fields["model"]) == "" && strings.TrimSpace(rb.fields["agent_type"]) == "" {
			continue
		}
		scopes = append(scopes, claudeconfig.RoleScope{Role: name})
		seenRoles[name] = struct{}{}
	}
	view.RoleCount = len(scopes)

	if project == "" {
		view.ScopeWarning = "Showing all projects — no default project is configured for this viewer (beadsViewer.project). Each row is resolved against its own project."
	}

	for _, scope := range scopes {
		// Resolve each scope against global + role: + its OWN project. A
		// project: label is REQUIRED to match (MatchesSubscriptions), so
		// resolving every row against one project (or against none) would
		// misreport all but one of them.
		subs := claudeconfig.BuildSubscriptions(scope.Role, scope.Project, "")
		ec, err := claudeconfig.ResolveEffectiveConfig(ctx, s.daemon, subs)
		if err != nil {
			// Degrade this row rather than failing the whole page.
			s.logger.Warn("resolving effective config for scope", "project", scope.Project, "role", scope.Role, "error", err)
			continue
		}
		var fields map[string]string
		if rb, ok := roleBeads[scope.Role]; ok {
			fields = rb.fields
		}
		row := s.buildClaudeConfigRow(scope, ec, fields)
		if row.HasTrap {
			// Count traps across the full vocabulary so the summary band and the
			// warning banner reflect the fleet, not just the current query.
			view.TrapCount++
		}
		if !claudeConfigRowMatches(row, query) {
			continue
		}
		view.Rows = append(view.Rows, row)
	}

	// Rows: trap rows first (they demand attention), then project, then role.
	sort.SliceStable(view.Rows, func(i, j int) bool {
		if view.Rows[i].HasTrap != view.Rows[j].HasTrap {
			return view.Rows[i].HasTrap
		}
		if view.Rows[i].Project != view.Rows[j].Project {
			return view.Rows[i].Project < view.Rows[j].Project
		}
		return view.Rows[i].Role < view.Rows[j].Role
	})
	view.MatchCount = len(view.Rows)
	return view
}

// scopeWarning reports the project-scope blind spot, or "" when a project is
// configured or nothing is being hidden. Best-effort: a daemon error yields no
// warning rather than failing the page.
func (s *Server) scopeWarning(ctx context.Context) (string, int) {
	if s.viewerProject != "" {
		return "", 0
	}
	n, err := claudeconfig.CountProjectScopedSettings(ctx, s.daemon)
	if err != nil {
		s.logger.Warn("claude-config: counting project-scoped settings", "error", err)
		return "", 0
	}
	if n == 0 {
		return "", 0
	}
	noun := "beads"
	if n == 1 {
		noun = "bead"
	}
	return fmt.Sprintf(
		"No project scope is configured for this viewer (BOAT_PROJECT is unset), so %d project-scoped claude-settings %s below. Pods DO receive them. Set beadsViewer.project in the chart.",
		n, noun), n
}

// claudeConfigRowMatches reports whether a row satisfies the case-insensitive
// substring query. An empty query matches everything. The project cell, role
// name, and every effective (set) cell value participate, so an operator can
// search "xhigh", "opus", "gardenio", or "agent_type"-style values
// interchangeably.
func claudeConfigRowMatches(row claudeConfigRow, query string) bool {
	if query == "" {
		return true
	}
	q := strings.ToLower(query)
	if strings.Contains(strings.ToLower(row.Project), q) {
		return true
	}
	if strings.Contains(strings.ToLower(row.Role), q) {
		return true
	}
	for _, cell := range row.Cells {
		if cell.Set && strings.Contains(strings.ToLower(cell.Value), q) {
			return true
		}
	}
	return false
}

// buildClaudeConfigRow projects an EffectiveConfig (plus the role bead's fields,
// which may be nil) into a matrix row for the given (project, role) scope.
func (s *Server) buildClaudeConfigRow(scope claudeconfig.RoleScope, ec *claudeconfig.EffectiveConfig, roleFields map[string]string) claudeConfigRow {
	href := s.basePath + "/claude-config/" + url.PathEscape(scope.Role)
	// Only carry ?project= when the detail page's own default (s.viewerProject)
	// would resolve the WRONG project for this row — i.e. the row's scope
	// differs from what handleClaudeConfigDetail falls back to with no query
	// param. Appending it unconditionally (scope.Project != "") made every row
	// in the common single-project deployment carry a redundant query string,
	// breaking any link built on the plain "/claude-config/{role}" path.
	if scope.Project != s.viewerProject {
		href += "?project=" + url.QueryEscape(scope.Project)
	}
	row := claudeConfigRow{
		Project:    scope.Project,
		Role:       scope.Role,
		LayerCount: ec.LayerCount,
		DetailHref: href,
	}
	for _, col := range claudeConfigKnobCols {
		cell := s.buildClaudeConfigCell(col, ec, roleFields)
		if cell.Trap {
			row.HasTrap = true
		}
		row.Cells = append(row.Cells, cell)
	}
	return row
}

// buildClaudeConfigCell resolves one column into a rendered cell. The
// settings-derived knobs come straight from ec.Knobs; the agent_type column and
// the model fallback are sourced from the role bead's fields.
func (s *Server) buildClaudeConfigCell(col claudeConfigKnobCol, ec *claudeconfig.EffectiveConfig, roleFields map[string]string) claudeConfigCell {
	// agent_type is a role-bead field, not a claude-settings knob.
	if col.Key == knobAgentType {
		if v := strings.TrimSpace(roleFields["agent_type"]); v != "" {
			kv := claudeconfig.KnobValue{Value: v, Set: true, Provenance: "role"}
			return claudeConfigCell{
				Value:      v,
				Set:        true,
				Provenance: "role",
				Chip:       provenanceChip(kv, false),
			}
		}
		return claudeConfigCell{} // unset → em-dash
	}

	kv := ec.Knobs[col.Key]

	// model fallback: when no claude-settings bead pins model, the role bead's
	// `model` field is the real effective override (applied in the account/role
	// precedence chain — RoleInfo.Model). Surface it so the column isn't blank
	// for the common case where model lives on the type:role bead.
	if col.Key == claudeconfig.KnobModel && !kv.Set {
		if v := strings.TrimSpace(roleFields["model"]); v != "" {
			kv = claudeconfig.KnobValue{Value: v, Set: true, Provenance: "role"}
		}
	}

	trap := col.Key == claudeconfig.KnobEffort && ec.EffortTrap
	return claudeConfigCell{
		Value:      kv.Value,
		Set:        kv.Set,
		Provenance: kv.Provenance,
		Target:     kv.ProvenanceTarget,
		BeadID:     kv.BeadID,
		Chip:       provenanceChip(kv, trap),
		Trap:       trap,
	}
}

// provenanceChip builds the small chip label shown under a cell value. Empty
// when the cell is an unset default (the em-dash carries the meaning already).
func provenanceChip(kv claudeconfig.KnobValue, trap bool) string {
	if !kv.Set {
		return ""
	}
	label := kv.Provenance
	if kv.ProvenanceTarget != "" && (kv.Provenance == "project" || kv.Provenance == "role" || kv.Provenance == "agent") {
		// Keep the chip compact: the row already names the role, so a role
		// target is redundant; show a target only when it differs from the row
		// (project/agent).
		if kv.Provenance != "role" {
			label = kv.Provenance + ":" + kv.ProvenanceTarget
		}
	}
	if trap {
		label += " · ⚠ blocks xhigh"
	}
	return label
}

// ── Role detail (/claude-config/{role}) ─────────────────────────────────────

// claudeConfigDetailView is the /claude-config/{role} page model.
type claudeConfigDetailView struct {
	Role          string
	Project       string
	Subscriptions []string
	Knobs         []claudeConfigKnobDetail // effective high-signal knobs, ordered
	LayerCount    int
	EffortTrap    bool
	SettingsJSON  string // pretty-printed effective settings.json
	Error         string
	NotFound      bool

	// ScopeWarning is set when this viewer has NO project scope configured
	// (BOAT_PROJECT unset). Resolution then carries no project: subscription, so
	// MatchesSubscriptions (internal/advice/matching.go:218-220) rejects every
	// project-scoped config bead while pods in that project still receive it.
	// UnreflectedCount is how many such beads exist.
	ScopeWarning     string
	UnreflectedCount int
}

// claudeConfigKnobDetail is a knob's effective value + provenance for the detail
// view (a fuller version of the matrix cell).
type claudeConfigKnobDetail struct {
	Key        string
	Label      string
	Value      string
	Set        bool
	Provenance string
	Target     string
	BeadID     string
	Trap       bool
	Hint       string
}

// handleClaudeConfigDetail renders /claude-config/{role} — the full resolved
// settings.json a pod with this role would receive, plus per-knob provenance.
func (s *Server) handleClaudeConfigDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	email := oidcauth.EmailFromContext(ctx)
	role := strings.TrimSpace(r.PathValue("role"))
	project := s.viewerProject
	if r.URL.Query().Has("project") {
		project = strings.TrimSpace(r.URL.Query().Get("project"))
	}

	view := s.buildClaudeConfigDetail(ctx, role, project)

	data := map[string]any{
		"View":        view,
		"ViewerEmail": email,
		"InboxCount":  s.inboxCountForViewer(ctx, email),
	}
	s.render(w, r, "claude_config_detail.html", data)
}

// buildClaudeConfigDetail computes the effective settings for one (project,
// role) scope.
func (s *Server) buildClaudeConfigDetail(ctx context.Context, role, project string) claudeConfigDetailView {
	view := claudeConfigDetailView{Role: role, Project: project}
	if role == "" {
		view.NotFound = true
		return view
	}
	subs := claudeconfig.BuildSubscriptions(role, project, "")
	view.Subscriptions = subs

	ec, err := claudeconfig.ResolveEffectiveConfig(ctx, s.daemon, subs)
	if err != nil {
		s.logger.Error("resolving effective config detail", "role", role, "error", err)
		view.Error = "Failed to load role config."
		return view
	}

	// Role-bead overlay for the model fallback + the agent_type column, mirroring
	// the matrix. Best-effort: a listing error leaves the settings-derived knobs.
	var roleFields map[string]string
	if roleBeads, rbErr := s.listRoleConfigBeads(ctx); rbErr != nil {
		s.logger.Warn("claude-config detail: listing role beads for overlay", "role", role, "error", rbErr)
	} else if rb, ok := roleBeads[role]; ok {
		roleFields = rb.fields
	}

	view.LayerCount = ec.LayerCount
	view.EffortTrap = ec.EffortTrap
	view.ScopeWarning, view.UnreflectedCount = s.scopeWarning(ctx)
	for _, col := range claudeConfigKnobCols {
		cell := s.buildClaudeConfigCell(col, ec, roleFields)
		view.Knobs = append(view.Knobs, claudeConfigKnobDetail{
			Key:        col.Key,
			Label:      col.Label,
			Value:      cell.Value,
			Set:        cell.Set,
			Provenance: cell.Provenance,
			Target:     cell.Target,
			BeadID:     cell.BeadID,
			Trap:       cell.Trap,
			Hint:       col.Hint,
		})
	}
	view.SettingsJSON = prettySettingsJSON(ec.Settings)
	return view
}

// prettySettingsJSON marshals the effective settings map to indented JSON for
// the detail view's raw-settings pane. Returns a placeholder on the (practically
// impossible) marshal failure rather than an empty pane.
func prettySettingsJSON(settings map[string]any) string {
	b, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "{}"
	}
	return string(b)
}
