package main

// The roles discovery index at /roles (epic ka-0000001026, task ka-0000007118,
// design doc §8.5). It is the role-side mirror of the project list on the home
// page (handleIndex / index.html): one card per role in the authoritative role
// vocabulary, each showing the live agent count carrying that role across all
// projects, a config-validity badge for its type:role bead, and a link out to
// the /role/{name} hub (task ka-0000004054).
//
// "Authoritative role vocabulary" is the union of:
//
//	(a) every type:role config bead title (ListRoleBeads — the registry), and
//	(b) every distinct role carried by a live agent bead (role field/label),
//
// so the index surfaces BOTH configured roles with no live agents AND live
// roles with no config bead (the latter flagged "no role bead" — the same
// soft-referential signal ValidateAgentRoles emits, design doc §8.5). This
// makes the index a discovery surface for drift, not just a registry dump.
//
// Best-effort throughout (mirrors handleIndex): the role-registry fetch and the
// agent/account fetches run independently; a daemon error on any one degrades
// its contribution (an empty registry, zero counts, or skipped validity) rather
// than failing the page. The role name is the only hard requirement and it
// always comes from the bead title / agent field.

import (
	"context"
	"net/http"
	"sort"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/oidcauth"
)

// rolesIndexAgentLimit caps the live-agent fetch backing the per-role agent
// counts. The index spans every project, so the bound matches the /agents
// active list (agentsActiveLimit) — a safety cap, not an expected truncation
// point for a realistic fleet.
const rolesIndexAgentLimit = 500

// roleIndexCard is one row on the /roles index: a role name, its live
// cross-project agent count, whether a type:role config bead backs it, and the
// config-validity badge for that bead.
type roleIndexCard struct {
	// Name is the role name (type:role title or an agent's role value).
	Name string
	// ConfigBeadID is the type:role bead backing this role, or "" for a
	// label/field-only role with no config bead (drift). When empty the card
	// renders a "no role bead" hint, mirroring ValidateAgentRoles.
	ConfigBeadID string
	// AgentCount is the number of live (open + in_progress) agents carrying
	// this role across all projects, deduped across the role field + role:label
	// union. Unengaged prewarmed-pool members are excluded (isUnengagedPrewarmedAgent).
	AgentCount int
	// Validity is the config-validity health report for the role's config bead
	// (role_validity.go). Zero report (green) for a label-only role.
	Validity roleValidityReport
	// HasConfigBead reports whether a type:role bead backs this role. Template
	// convenience for ConfigBeadID != "".
	HasConfigBead bool
}

// rolesIndexView is the assembled view-model for /roles.
type rolesIndexView struct {
	Cards []roleIndexCard
	// TotalRoles is len(Cards) — the headline count.
	TotalRoles int
	// ConfiguredRoles is the number of cards backed by a type:role bead.
	ConfiguredRoles int
	// UnconfiguredRoles is the number of cards with live agents but no config
	// bead (drift) — TotalRoles - ConfiguredRoles.
	UnconfiguredRoles int
	// Error is a page-level error string when the role registry could not be
	// loaded at all (the only hard dependency). Per-panel degradation does not
	// set this.
	Error string
}

// handleRolesIndex renders the /roles discovery index. Viewer-scoped read
// (plain HandleFunc gated by the ambient auth middleware), mirroring
// handleIndex.
func (s *Server) handleRolesIndex(w http.ResponseWriter, r *http.Request) {
	email := oidcauth.EmailFromContext(r.Context())
	ctx := r.Context()

	view := s.buildRolesIndex(ctx)

	data := map[string]any{
		"Roles":       view,
		"ViewerEmail": email,
		"InboxCount":  s.inboxCountForViewer(ctx, email),
	}
	s.render(w, r, "roles.html", data)
}

// buildRolesIndex assembles the /roles view-model. The role registry is the
// only hard dependency: if it fails to load AND the agent list also fails, the
// view carries a page-level Error. Otherwise every panel degrades independently.
func (s *Server) buildRolesIndex(ctx context.Context) rolesIndexView {
	var view rolesIndexView

	// (a) The role registry: type:role config beads keyed by title, carrying
	// the bead ID (for the hub link) and the raw fields (for the validity
	// report). This is the single registry source — the index doesn't need the
	// resolved RoleInfo from ListRoleBeads, only id+fields, so one fetch backs
	// both the vocabulary and the validity badge. A daemon error leaves an
	// empty registry but is not yet fatal — live agent roles can still populate
	// the index (drift discovery).
	configBeads, configErr := s.listRoleConfigBeads(ctx)
	if configErr != nil {
		s.logger.Warn("roles index: listing role config beads", "error", configErr)
	}

	// (b) Live agents grouped by role, deduped across the field + label union.
	agentCounts, agentsErr := s.countAgentsByRole(ctx)
	if agentsErr != nil {
		s.logger.Warn("roles index: counting agents by role", "error", agentsErr)
	}

	// Hard-failure guard: if BOTH the registry and the agent list failed, the
	// page has nothing to render — surface the error rather than a blank index.
	if configErr != nil && agentsErr != nil {
		view.Error = "Failed to load roles."
		return view
	}

	// (c) Known accounts for the validity account-reference check. Best-effort:
	// a nil set skips the account checks (matching buildProjectValidity's eager
	// path), so a fetch error simply yields fewer findings, never a failure.
	knownAccounts := s.knownAccountNames(ctx)

	// Union the role vocabulary: every registry title + every live agent role.
	names := map[string]struct{}{}
	for name := range configBeads {
		names[name] = struct{}{}
	}
	for name := range agentCounts {
		names[name] = struct{}{}
	}

	cards := make([]roleIndexCard, 0, len(names))
	for name := range names {
		card := roleIndexCard{
			Name:       name,
			AgentCount: agentCounts[name],
		}
		if cb, ok := configBeads[name]; ok {
			card.ConfigBeadID = cb.id
			card.HasConfigBead = true
			card.Validity = buildRoleValidityReport(cb.fields, knownAccounts)
		}
		if card.HasConfigBead {
			view.ConfiguredRoles++
		} else {
			view.UnconfiguredRoles++
		}
		cards = append(cards, card)
	}

	// Sort: configured roles before drift, then by agent count desc, then name
	// asc — surfaces the live, healthy roles first and parks the
	// no-config-bead drift at the end where an operator can spot it.
	sort.SliceStable(cards, func(i, j int) bool {
		if cards[i].HasConfigBead != cards[j].HasConfigBead {
			return cards[i].HasConfigBead // configured first
		}
		if cards[i].AgentCount != cards[j].AgentCount {
			return cards[i].AgentCount > cards[j].AgentCount
		}
		return cards[i].Name < cards[j].Name
	})

	view.Cards = cards
	view.TotalRoles = len(cards)
	return view
}

// roleConfigBead is the {id, raw fields} of one type:role config bead, used to
// recover the bead ID (which ListRoleBeads discards) and the raw fields the
// validity report re-parses.
type roleConfigBead struct {
	id     string
	fields map[string]string
}

// listRoleConfigBeads fetches the type:role config beads and returns them keyed
// by title, applying the same deterministic duplicate-title guard as
// ListRoleBeads (lowest bead ID wins) so the index agrees with the registry on
// which bead owns a duplicated title. Returns the daemon error so the caller can
// fold it into the both-failed page-level guard; a non-nil error comes with a
// nil map.
func (s *Server) listRoleConfigBeads(ctx context.Context) (map[string]roleConfigBead, error) {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"role"},
		Statuses: []string{"open", "in_progress"},
		Limit:    rolesIndexAgentLimit,
	})
	if err != nil {
		return nil, err
	}
	out := map[string]roleConfigBead{}
	if res == nil {
		return out, nil
	}
	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		name := strings.TrimSpace(b.Title)
		if name == "" {
			continue
		}
		// Duplicate-title guard: keep the lowest bead ID, mirroring
		// ListRoleBeads so the index card links to the same authoritative bead
		// the registry resolved.
		if prior, dup := out[name]; dup && prior.id <= b.ID {
			continue
		}
		out[name] = roleConfigBead{id: b.ID, fields: b.Fields}
	}
	return out, nil
}

// countAgentsByRole counts live (open + in_progress) agents per role across all
// projects, deduped by bead ID across the role field. Roles come from the agent
// bead's role field (the same source agents.go uses for its role chips,
// b.Fields["role"]); the role:<name> label is a derived mirror of that field at
// spawn time, so the field is the authoritative per-agent role. Unengaged
// prewarmed-pool members are excluded exactly as /agents and the role hub
// exclude them.
func (s *Server) countAgentsByRole(ctx context.Context) (map[string]int, error) {
	res, err := s.daemon.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"agent"},
		Statuses: []string{"open", "in_progress"},
		Sort:     "-updated_at",
		Limit:    rolesIndexAgentLimit,
	})
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	if res == nil {
		return counts, nil
	}
	for _, b := range res.Beads {
		if b == nil || isUnengagedPrewarmedAgent(b) {
			continue
		}
		role := strings.TrimSpace(b.Fields["role"])
		if role == "" {
			continue // untyped agents don't belong to any role vocabulary entry
		}
		counts[role]++
	}
	return counts, nil
}

// knownAccountNames returns the set of live type:account bead names for the
// validity account-reference check, or nil if it could not be loaded (which
// makes buildRoleValidityReport skip the account checks). Accounts are
// project-scoped (ListAccountBeadsByProject), so the index unions accounts
// across every known project to build the global name set a role's account /
// account_whitelist can legitimately reference.
func (s *Server) knownAccountNames(ctx context.Context) map[string]struct{} {
	projects, err := s.daemon.ListProjectBeads(ctx)
	if err != nil {
		s.logger.Warn("roles index: listing projects for account check", "error", err)
		return nil
	}
	known := map[string]struct{}{}
	for project := range projects {
		if project == "" {
			continue
		}
		accts, err := s.daemon.ListAccountBeadsByProject(ctx, project)
		if err != nil {
			s.logger.Warn("roles index: listing accounts", "project", project, "error", err)
			continue
		}
		for _, a := range accts {
			if a.Name != "" {
				known[a.Name] = struct{}{}
			}
		}
	}
	// An empty-but-non-nil set is meaningful: it means "we successfully looked
	// and found no accounts", so every account reference is dangling. Only nil
	// (total failure) skips the check. If no projects existed, return the empty
	// set rather than nil so the distinction holds.
	return known
}
