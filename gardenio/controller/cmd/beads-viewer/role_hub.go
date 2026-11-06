package main

// Backend assembler for the rich role hub at /role/<name> (epic ka-0000001026,
// task ka-0000006170, design doc §8.5). It is the role-side mirror of
// buildProjectHub (project_hub.go): the project hub renders one project's config
// + its per-project role breakdown, while the role hub inverts that axis and
// renders one ROLE across ALL projects:
//
//	(a) active agents WITH this role across every project (the inverse of the
//	    project page's per-project role breakdown, agents.go:289),
//	(b) the resolved type:role overrides (image, runtime_class, account, model,
//	    secrets-by-NAME, env, claude_config, …) from ListRoleBeads, and
//	(c) the role-scoped config + advice beads (role:<name> labels), reusing the
//	    advice-scope "Role:" vocabulary (advice/matching.go).
//
// This file is BACKEND-ONLY by design: task ka-0000006170 delivers the
// view-model + assembler + its interface dependency; the GET /role/{name} route
// and the role_hub.html template land in the dependent P3 task (ka-0000004054).
// Keeping the assembler testable in isolation mirrors how buildProjectHub was
// landed ahead of its own hub shell.
//
// Best-effort degradation (mirrors buildProjectHub / the project-hub sections):
// a daemon error on any single sub-fetch hides only that panel — it never fails
// the whole assembly and the caller never 500s. A nil/zero sub-result simply
// renders an empty panel. The only hard requirement is the role name, which the
// caller always has from the URL path.

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// roleHubAgentsLimit caps the cross-project active-agent fetch backing the role
// hub's agents panel. The hub is a digest spanning every project, so the bound
// is higher than the per-project projectHubAgentsLimit (50) but still a safety
// cap, not an expected truncation point for realistic role fleets.
const roleHubAgentsLimit = 100

// roleHubConfigLimit caps the role-scoped config + advice fetches. Roles carry
// a handful of targeted config/advice beads; the cap is a guard, not a paging
// boundary.
const roleHubConfigLimit = 100

// roleHubView is the assembled view-model for the role hub at /role/<name>.
// Every panel is independent and best-effort: an empty slice / Found==false is
// a valid rendered state, never an error.
type roleHubView struct {
	// Name is the role name (the URL path value), always populated even when no
	// type:role bead, agents, or scoped config exist for it.
	Name string

	// Found reports whether a type:role bead carries this title in the role
	// registry (ListRoleBeads). False for a label-only or builtin role that has
	// no config bead seeded — the hub still renders agents + scoped config, and
	// the dependent route can surface a "no role bead" hint. Mirrors the
	// project hub's ConfigBeadID=="" (label-only project) degradation.
	Found bool

	// Info is the resolved type:role overrides for this role, straight from
	// ListRoleBeads (which already applies the deterministic duplicate-title
	// guard). Zero-valued when Found is false. The flattened Overrides/Secrets/
	// Env*/ClaudeConfigKeys/AccountWhitelist fields below are derived from this
	// for template convenience; Info is retained so the dependent template can
	// reach any field without a re-projection.
	Info beadsapi.RoleInfo

	// Overrides is the set of scalar pod-override fields that are actually
	// configured on the role bead, as {Label,Value} rows. Unset scalars are
	// omitted so the panel renders only the real overrides (no wall of empty
	// rows). Bool flags appear only when true (a parsed false is
	// indistinguishable from unset in RoleInfo); SessionResume, a tri-state
	// *bool, appears whenever it is explicitly set.
	Overrides []roleOverrideRow

	// Secrets lists the per-role secret references BY NAME ONLY (env var name,
	// K8s Secret name, key) — never a secret value, which RoleInfo does not
	// carry in the first place. Empty when the role declares no secret
	// overrides.
	Secrets []beadsapi.SecretEntry

	// EnvVars lists the per-role plain env overrides (name/value pairs from the
	// role bead's `env` field). Empty when none are declared.
	EnvVars []beadsapi.EnvEntry

	// EnvOverrideKeys is the sorted key set of the role's env_json map. Keys
	// only — values may be environment-specific and the hub is a config digest,
	// not a secret dumper. Empty when env_json is unset.
	EnvOverrideKeys []string

	// ClaudeConfigKeys is the sorted key set of the role's inline claude_config
	// overrides. Keys only, for the same reason as EnvOverrideKeys. Empty when
	// claude_config is unset.
	ClaudeConfigKeys []string

	// AccountWhitelist is the role's account allow-list (intersected with the
	// project pool at spawn). Empty when the role imposes no restriction.
	AccountWhitelist []string

	// EnabledSkills and DisabledSkills are the per-role skill toggle (Adam
	// feedback #1, epic ka-0000004989). EnabledSkills is an allow-list: when
	// non-empty, ONLY the named skills are linked into the workspace (empty =
	// every discovered skill enabled). DisabledSkills is a deny-list: named
	// skills are never linked (deny wins). Both name the LINKED skill name
	// Claude Code discovers (primary skills bare, sibling-repo skills prefixed
	// "{repo}-"). Empty when the role sets no toggle.
	EnabledSkills  []string
	DisabledSkills []string

	// EnabledAgents and DisabledAgents are the per-role sub-agent toggle
	// (ka-0000006908) — the "agents" subdir analogue of the skill toggle with
	// identical allow/deny semantics and the same linked-name matching. Empty
	// when the role sets no toggle.
	EnabledAgents  []string
	DisabledAgents []string

	// AgentGroups holds the active (open + in_progress) agents carrying this
	// role, grouped by project and sorted by project name. Empty when no live
	// agent currently runs this role. Unengaged prewarmed-pool members are
	// dropped exactly as /agents drops them (isUnengagedPrewarmedAgent).
	AgentGroups []roleHubProjectGroup

	// AgentCount is the total agent rows across all groups — the headline count
	// for the agents panel.
	AgentCount int

	// Config is the role-scoped config beads (kind=config carrying a
	// role:<name> label): claude-settings/hooks/instructions/nudge-prompts, etc.
	// Empty when the role has no scoped config.
	Config []roleHubConfigRow

	// Advice is the role-scoped advice beads (type=advice carrying a role:<name>
	// label). Empty when the role has no scoped advice. Reuses the same compact
	// row shape as Config.
	Advice []roleHubConfigRow
}

// roleOverrideRow is one configured scalar pod-override on the role bead,
// rendered as a label/value pair in the resolved-overrides panel.
type roleOverrideRow struct {
	Label string
	Value string
}

// roleHubProjectGroup is one project section of the role hub's agents panel:
// the live agents carrying this role that run in the named project. Reuses the
// compact projectHubAgentView row (project_hub_sections.go) so the role hub and
// the project hub render an identical agent card.
type roleHubProjectGroup struct {
	Project string
	Agents  []projectHubAgentView
}

// roleHubConfigRow is one role-scoped config or advice bead in the hub's
// config/advice panels. A thin link-out row: identity, type/status, and "ago".
type roleHubConfigRow struct {
	ID     string
	Title  string
	Type   string
	Status string
	Ago    string
}

// buildRoleHubEager assembles the eager half of the role hub: the resolved
// type:role overrides and the role-scoped config + advice beads, but NOT the
// cross-project agent list. The agents panel is the one expensive, fan-out
// section (two queries across every project), so the page renders this half on
// first byte and lazy-loads agents via handleRoleHubAgents (gardener-page
// eager-head + lazy-tail skeleton).
//
// Best-effort throughout: a daemon error on any sub-fetch hides only that panel
// and never fails the assembly; the only hard input is the role name (the URL
// path value). A blank role returns an empty view without issuing any call.
func (s *Server) buildRoleHubEager(ctx context.Context, role string, now time.Time) *roleHubView {
	role = strings.TrimSpace(role)
	view := &roleHubView{Name: role}
	if role == "" {
		return view
	}

	// (b) Resolved overrides (best-effort). ListRoleBeads already resolves the
	// duplicate-title guard, so a hit is the authoritative RoleInfo. A miss
	// (label-only/builtin role) or a daemon error leaves Found=false and the
	// overrides panel empty — agents + scoped config still render.
	if roles, err := s.daemon.ListRoleBeads(ctx); err != nil {
		s.logger.Warn("role hub: listing role beads", "role", role, "error", err)
	} else if info, ok := roles[role]; ok {
		view.Found = true
		view.Info = info
		populateRoleOverrides(view, info)
	}

	// (c) Role-scoped config + advice beads (best-effort). Both keyed by the
	// role:<name> label — the same targeting label advice/config delivery
	// matches (advice/matching.go). A daemon error on either drops only that
	// list.
	view.Config = s.listRoleScopedBeads(ctx, role, roleHubConfigQuery{kind: "config"}, now)
	view.Advice = s.listRoleScopedBeads(ctx, role, roleHubConfigQuery{adviceType: true}, now)

	return view
}

// populateRoleOverrides flattens the resolved RoleInfo into the view's
// template-friendly panels: a {Label,Value} list of the configured scalar
// overrides (unset scalars omitted), and the structured secret/env/claude/
// whitelist lists. Secrets are carried BY NAME ONLY — RoleInfo never holds a
// secret value.
func populateRoleOverrides(view *roleHubView, info beadsapi.RoleInfo) {
	add := func(label, value string) {
		if strings.TrimSpace(value) != "" {
			view.Overrides = append(view.Overrides, roleOverrideRow{Label: label, Value: value})
		}
	}
	add("Image", info.Image)
	add("Runtime class", info.RuntimeClassName)
	add("Storage class", info.StorageClass)
	add("Service account", info.ServiceAccount)
	add("Account", info.Account)
	add("Model", info.Model)
	add("Harness", info.Harness)
	add("CPU request", info.CPURequest)
	add("CPU limit", info.CPULimit)
	add("Memory request", info.MemoryRequest)
	add("Memory limit", info.MemoryLimit)
	// Bool flags appear only when true: a parsed false is indistinguishable
	// from "unset" in RoleInfo (ListRoleBeads reads `field == "true"`), so a
	// false row would assert an override that may not exist.
	if info.DockerEnabled {
		add("Docker", "enabled")
	}
	if info.PostgresEnabled {
		add("Postgres", "enabled")
	}
	if info.NeedsK8sAPI {
		add("K8s API token", "mounted")
	}
	// SessionResume is a genuine tri-state (*bool): nil = inherit, so it only
	// surfaces when the role explicitly set it either way.
	if info.SessionResume != nil {
		if *info.SessionResume {
			add("Session resume", "true")
		} else {
			add("Session resume", "false")
		}
	}

	view.Secrets = info.Secrets
	view.EnvVars = info.EnvVars
	view.AccountWhitelist = info.AccountWhitelist
	view.EnabledSkills = info.EnabledSkills
	view.DisabledSkills = info.DisabledSkills
	view.EnabledAgents = info.EnabledAgents
	view.DisabledAgents = info.DisabledAgents
	view.EnvOverrideKeys = sortedMapKeys(info.EnvOverrides)
	view.ClaudeConfigKeys = sortedRawMessageKeys(info.ClaudeConfig)
}

// sortedMapKeys returns the keys of a string-valued map in ascending order, or
// nil for an empty/nil map (so the template can `{{if}}` on it).
func sortedMapKeys(m map[string]string) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// sortedRawMessageKeys returns the keys of a json.RawMessage-valued map (the
// shape of RoleInfo.ClaudeConfig) in ascending order, or nil for an empty/nil
// map. Keys only — the hub surfaces WHICH claude_config layers a role overrides,
// not their (possibly env-specific) values.
func sortedRawMessageKeys(m map[string]json.RawMessage) []string {
	if len(m) == 0 {
		return nil
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// buildRoleHubAgents lists the active agents carrying the role across every
// project, grouped by project. It unions a role:<name> label query with a
// role-field query (deduped by bead ID) so an agent that names its role only as
// a label OR only as a field is still captured, then projects each through the
// shared agentRowFromBeadCtx so the role hub and /agents agree on identity and
// state. Best-effort: a daemon error on either query is logged and that query
// contributes nothing — the other still populates the panel.
func (s *Server) buildRoleHubAgents(ctx context.Context, role string, now time.Time) ([]roleHubProjectGroup, int) {
	seen := map[string]bool{}
	byProject := map[string][]projectHubAgentView{}
	total := 0

	collect := func(q beadsapi.ListBeadsQuery) {
		res, err := s.daemon.ListBeadsFiltered(ctx, q)
		if err != nil {
			s.logger.Warn("role hub agents: listing", "role", role, "error", err)
			return
		}
		if res == nil {
			return
		}
		for _, b := range res.Beads {
			if b == nil || seen[b.ID] || isUnengagedPrewarmedAgent(b) {
				continue
			}
			seen[b.ID] = true
			project := agentProjectName(b)
			full := s.agentRowFromBeadCtx(ctx, b, project, now)
			byProject[project] = append(byProject[project], projectHubAgentView{
				ID:         full.ID,
				Name:       full.Name,
				Role:       full.Role,
				AgentState: full.AgentState,
				Status:     full.Status,
				TaskID:     full.TaskID,
				TaskTitle:  full.TaskTitle,
				Ago:        full.Ago,
			})
			total++
		}
	}

	// Label-scoped query: agents tagged role:<name>.
	collect(beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress"},
		Labels:   []string{"role:" + role},
		Sort:     "-updated_at",
		Limit:    roleHubAgentsLimit,
	})
	// Field-scoped query: agents whose role FIELD equals the role but that may
	// lack the label (BOAT_ROLE-only agents).
	collect(beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress"},
		Fields:   map[string]string{"role": role},
		Sort:     "-updated_at",
		Limit:    roleHubAgentsLimit,
	})

	groups := make([]roleHubProjectGroup, 0, len(byProject))
	for project, agents := range byProject {
		// Within a project, newest-activity-first is already the query sort, but
		// the two-query union can interleave; re-sort by name for a stable,
		// deterministic render (the hub is a digest, not an activity feed).
		sort.SliceStable(agents, func(i, j int) bool {
			return strings.ToLower(agents[i].Name) < strings.ToLower(agents[j].Name)
		})
		groups = append(groups, roleHubProjectGroup{Project: project, Agents: agents})
	}
	sort.SliceStable(groups, func(i, j int) bool {
		return groups[i].Project < groups[j].Project
	})
	return groups, total
}

// roleHubConfigQuery selects which role-scoped bead family listRoleScopedBeads
// fetches: a kind (e.g. "config") for the config panel, or adviceType for the
// advice panel (type=advice). Exactly one should be set.
type roleHubConfigQuery struct {
	kind       string
	adviceType bool
}

// listRoleScopedBeads fetches the role-scoped config OR advice beads (those
// carrying a role:<name> label) and projects them into compact link-out rows.
// Best-effort: a daemon error logs and yields an empty slice so the panel
// renders empty rather than failing the hub.
func (s *Server) listRoleScopedBeads(ctx context.Context, role string, sel roleHubConfigQuery, now time.Time) []roleHubConfigRow {
	q := beadsapi.ListBeadsQuery{
		Labels: []string{"role:" + role},
		// Only ACTIVE config/advice belongs on the "what governs this role" panels:
		// closing a config or advice bead is the operator's lever to retire it, and
		// gb prime already stops delivering closed beads — so a closed bead here
		// would misleadingly read as "still in effect". Mirrors the active-set
		// filter buildRoleHubAgents applies to the agent panels in this same file,
		// and the shared activeEnumStatuses used by /charts + /plans (ka-0000005651).
		Statuses: activeEnumStatuses,
		Sort:     "-updated_at",
		Limit:    roleHubConfigLimit,
	}
	if sel.adviceType {
		q.Types = []string{"advice"}
	}
	if sel.kind != "" {
		q.Kinds = []string{sel.kind}
	}

	res, err := s.daemon.ListBeadsFiltered(ctx, q)
	if err != nil {
		s.logger.Warn("role hub scoped beads: listing", "role", role,
			"advice", sel.adviceType, "kind", sel.kind, "error", err)
		return nil
	}
	if res == nil {
		return nil
	}
	rows := make([]roleHubConfigRow, 0, len(res.Beads))
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		rows = append(rows, roleHubConfigRow{
			ID:     b.ID,
			Title:  b.Title,
			Type:   b.Type,
			Status: b.Status,
			Ago:    fmtTimeAgo(b.UpdatedAt, now),
		})
	}
	return rows
}
