package claudeconfig

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/advice"
	"gardenio/controller/internal/beadsapi"
)

// BeadLister is the interface for querying config beads. Satisfied by
// *beadsapi.Client (and by the viewer's beadAPI).
type BeadLister interface {
	ListBeadsFiltered(ctx context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error)
}

// Layer holds a matched config bead with its specificity for sorting. Unexported
// fields keep the merge inputs encapsulated; construct extra (inline) layers with
// NewLayer, and read provenance via the accessor methods below.
type Layer struct {
	labels      []string
	value       json.RawMessage
	specificity string    // sortable key from advice.GroupSortKey
	createdAt   time.Time // newer wins on tiebreak (applied last in merge)
	beadID      string    // final stable tiebreak when timestamps tie
	scope       string    // global | project | role | agent (from labels)
	target      string    // scope target value (project/role/agent name)
}

// NewLayer builds an extra (inline) config layer — e.g. a role/project bead's
// inline claude_config field — with an explicit specificity key. Used by callers
// that contribute layers outside the type=config bead resolution.
func NewLayer(value json.RawMessage, specificity string) Layer {
	return Layer{value: value, specificity: specificity}
}

// Value returns the layer's raw JSON config value.
func (l Layer) Value() json.RawMessage { return l.value }

// Scope reports the layer's targeting scope (global|project|role|agent), derived
// from its labels. Empty for inline layers built via NewLayer.
func (l Layer) Scope() string { return l.scope }

// Target reports the layer's scope target value (the project/role/agent name).
func (l Layer) Target() string { return l.target }

// BeadID reports the source config bead id, or "" for an inline layer.
func (l Layer) BeadID() string { return l.beadID }

// RoleInlineSpecificity returns the sort key for role bead inline config.
// Placed after role config beads (2:) but before project inline (2~:), keyed by
// role name so multi-role ordering is deterministic.
func RoleInlineSpecificity(roleName string) string {
	return "2^:role-inline:" + roleName
}

// ProjectInlineSpecificity is the sort key for project bead inline config —
// after role inline (2^:), before agent (3:).
const ProjectInlineSpecificity = "2~:project-inline"

// buildRoleIndex returns a map from role name to its index in the subscription
// list. Earlier roles (lower index) have higher precedence.
func buildRoleIndex(subscriptions []string) map[string]int {
	idx := make(map[string]int)
	i := 0
	for _, s := range subscriptions {
		if role, ok := strings.CutPrefix(s, "role:"); ok {
			if _, exists := idx[role]; !exists {
				idx[role] = i
				i++
			}
		}
	}
	return idx
}

// resolveSortedLayers matches config beads for a category against the agent
// subscriptions, appends any inline extra layers, and returns them sorted in
// merge order (lowest priority first). The merge and the viewer's provenance
// walk share this one ordering so they can never disagree.
func resolveSortedLayers(ctx context.Context, lister BeadLister, category string, subscriptions []string, extraLayers []Layer) ([]Layer, error) {
	result, err := lister.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "[config] warning: failed to list config beads: %v\n", err)
		return nil, fmt.Errorf("listing config beads: %w", err)
	}

	// Build role index for multi-role specificity sorting.
	roleIndex := buildRoleIndex(subscriptions)

	// Filter: matching title (category) + matching subscriptions.
	var matched []Layer
	for _, bead := range result.Beads {
		if bead.Title != category {
			continue
		}
		if !advice.MatchesSubscriptions(bead.Labels, subscriptions) {
			continue
		}

		// Config value is stored in the bead's Description as JSON.
		var raw json.RawMessage
		if err := json.Unmarshal([]byte(bead.Description), &raw); err != nil {
			fmt.Fprintf(os.Stderr, "[config] warning: invalid JSON in config bead %s: %v\n", bead.ID, err)
			continue
		}

		scope, target := advice.CategorizeScope(bead.Labels)
		sortKey := advice.GroupSortKey(scope, target)
		// For role-scoped beads with multi-role agents, use role index for
		// inter-role precedence. Earlier roles in BOAT_ROLE win (sort last,
		// since later items override earlier in merge).
		if scope == "role" && len(roleIndex) > 1 {
			idx, ok := roleIndex[target]
			if !ok {
				idx = 0 // unknown role = lowest precedence = sorted first
			} else {
				idx = 99 - idx // invert: first role (0) → 99, second (1) → 98
			}
			sortKey = fmt.Sprintf("2:%02d:%s", idx, target)
		}
		matched = append(matched, Layer{
			labels:      bead.Labels,
			value:       raw,
			specificity: sortKey,
			createdAt:   bead.CreatedAt,
			beadID:      bead.ID,
			scope:       scope,
			target:      target,
		})
	}

	// Append any extra layers (e.g. project bead inline claude_config).
	matched = append(matched, extraLayers...)

	// Sort by specificity: global (0:) < project (1:) < role (2:) < project-inline (2~:) < agent (3:).
	// Within the same specificity bucket, sort older beads first so newer beads
	// are applied LAST in the merge — meaning the most-recently-created bead at
	// a given specificity wins. This matches the intuition that the latest config
	// update reflects current intent, and (critically for E2E tests) makes a
	// freshly-created scenario bead deterministically beat any leaked or stale
	// bead at the same scope. Tiebreak on beadID for the rare case where two
	// beads share a CreatedAt timestamp.
	sort.Slice(matched, func(i, j int) bool {
		if matched[i].specificity != matched[j].specificity {
			return matched[i].specificity < matched[j].specificity
		}
		if !matched[i].createdAt.Equal(matched[j].createdAt) {
			return matched[i].createdAt.Before(matched[j].createdAt)
		}
		return matched[i].beadID < matched[j].beadID
	})

	return matched, nil
}

// ResolveConfigBeads queries config beads matching the given category and agent
// subscriptions, sorts by label specificity, and merges according to the
// category's merge strategy.
//
// Returns the merged config, the number of layers found, and an error.
// Returns (nil, 0, nil) if the daemon responded but no matching beads exist.
// Returns (nil, 0, err) when the daemon listing itself failed (e.g. unreachable) —
// callers that must fail closed on daemon-unreachable should check the error.
func ResolveConfigBeads(ctx context.Context, lister BeadLister, category string, subscriptions []string, extraLayers ...Layer) (map[string]any, int, error) {
	cat := LookupCategory(category)
	if cat == nil {
		return nil, 0, nil
	}

	matched, err := resolveSortedLayers(ctx, lister, category, subscriptions, extraLayers)
	if err != nil {
		return nil, 0, err
	}
	if len(matched) == 0 {
		return nil, 0, nil
	}

	// Merge layers in specificity order.
	layers := make([]json.RawMessage, len(matched))
	for i, m := range matched {
		layers[i] = m.value
	}

	return MergeLayers(cat.Strategy, layers), len(matched), nil
}

// ResolveConfigLayers is like ResolveConfigBeads but returns the sorted,
// merge-ordered layers themselves (lowest priority first) instead of the merged
// result. The beads-viewer uses it to attribute each effective knob value to the
// layer that set it (provenance), which the collapsed merged map cannot show.
// Returns (nil, err) on a daemon listing failure; (nil, nil) when no layers match.
func ResolveConfigLayers(ctx context.Context, lister BeadLister, category string, subscriptions []string, extraLayers ...Layer) ([]Layer, error) {
	if LookupCategory(category) == nil {
		return nil, nil
	}
	return resolveSortedLayers(ctx, lister, category, subscriptions, extraLayers)
}

// listSettingsRoleScopes returns the distinct (project, role) pairs carried by
// open claude-settings config beads. A bead with role: labels and no project: label
// contributes {"" , role}; a bead with both contributes one pair per project
// label. Deduplicated and sorted by project then role.
func listSettingsRoleScopes(ctx context.Context, lister BeadLister, project string) ([]RoleScope, error) {
	result, err := lister.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		return nil, fmt.Errorf("listing config beads: %w", err)
	}
	seen := make(map[RoleScope]struct{})
	for _, bead := range result.Beads {
		if bead.Title != "claude-settings" {
			continue
		}
		if !projectCompatible(bead.Labels, project) {
			continue
		}
		var roles, projects []string
		for _, l := range bead.Labels {
			clean := advice.StripGroupPrefix(l)
			if r, ok := strings.CutPrefix(clean, "role:"); ok && r != "" {
				roles = append(roles, r)
			}
			if p, ok := strings.CutPrefix(clean, "project:"); ok && p != "" {
				projects = append(projects, p)
			}
		}
		if len(roles) == 0 {
			continue
		}
		if len(projects) == 0 {
			projects = []string{""}
		}
		for _, p := range projects {
			for _, r := range roles {
				seen[RoleScope{Project: p, Role: r}] = struct{}{}
			}
		}
	}
	out := make([]RoleScope, 0, len(seen))
	for rs := range seen {
		out = append(out, rs)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Role < out[j].Role
	})
	return out, nil
}

// CountProjectScopedSettings returns how many OPEN claude-settings config beads
// carry at least one project: label. The beads-viewer uses it to say out loud
// how many beads its own (project-less) resolution is failing to reflect:
// MatchesSubscriptions hard-requires every project: label to be subscribed
// (internal/advice/matching.go:218-220), so a viewer with no project scope shows
// none of them even though pods in that project do receive them.
func CountProjectScopedSettings(ctx context.Context, lister BeadLister) (int, error) {
	result, err := lister.ListBeadsFiltered(ctx, beadsapi.ListBeadsQuery{
		Types:    []string{"config"},
		Statuses: []string{"open"},
		Limit:    500,
	})
	if err != nil {
		return 0, fmt.Errorf("listing config beads: %w", err)
	}
	n := 0
	for _, bead := range result.Beads {
		if bead.Title != "claude-settings" {
			continue
		}
		for _, l := range bead.Labels {
			if strings.HasPrefix(advice.StripGroupPrefix(l), "project:") {
				n++
				break
			}
		}
	}
	return n, nil
}

// projectCompatible reports whether a bead's project: labels are compatible with
// the target project. A bead with no project: label is always compatible. A
// project of "" means "no project filtering" (every bead is compatible) — that
// is this helper's only divergence from MatchesSubscriptions, and it is
// deliberate: callers use it for project-AGNOSTIC listing.
//
// Otherwise it mirrors MatchesSubscriptions (internal/advice/matching.go:218-220)
// exactly: EVERY project: label must match, not merely one of them.
func projectCompatible(labels []string, project string) bool {
	if project == "" {
		return true
	}
	for _, l := range labels {
		clean := advice.StripGroupPrefix(l)
		if p, ok := strings.CutPrefix(clean, "project:"); ok && p != project {
			return false
		}
	}
	return true
}
