package main

import (
	"net/url"
	"sort"
	"strings"
)

// This file holds the /plans index filter + visual-grouping machinery (Matt's
// ask: "filter plans by user, project, and status; some visual grouping would
// be nice"). It mirrors the proven /extern control-strip pattern (extern.go):
// a parsed filter state that is the single source of truth for chip URLs, plus
// a server-side grouping pass. The USER dimension is owned by the separate
// operator header chip (operator_chip.go) — this file preserves that scope
// across every status/project/group switch and adds group-by-user.

// planStatus* are the ?status= filter values. The default is "active" — the
// open/in_progress/blocked/deferred set the page has always shown — so the
// cold-load /plans view is unchanged. "closed" and "all" are the new reach:
// closed plans were previously invisible on the index.
const (
	planStatusActive     = "active" // open + in_progress + blocked + deferred (default)
	planStatusOpen       = "open"
	planStatusInProgress = "in_progress"
	planStatusBlocked    = "blocked"
	planStatusDeferred   = "deferred"
	planStatusClosed     = "closed"
	planStatusAll        = "all" // any status
	// planStatusStale is a DERIVED sweep filter, not a raw daemon status: an
	// ACTIVE plan (any non-terminal status) whose newest activity has crossed
	// planFreshnessStaleThreshold (7d) — the SAME per-card FreshnessStale signal
	// the freshness stamp already tints loud (plans.go iter-38). It is the
	// "no activity in N days" half of the close-out sweep (epic ka-0000007470,
	// task ka-0000004351) — a genuinely different pile from the zero-open-children
	// "Done?" lane MR !7208 shipped: a plan can idle for a month with every step
	// still open (nothing drained, no Done? badge), which is exactly the rot the
	// sweep is for. The daemon cannot filter on the folded lastActivityAt, so the
	// query scopes to the active set and handlePlans post-filters the enriched
	// rows on FreshnessStale.
	planStatusStale = "stale"
)

// planGroup* are the ?group= values. Empty means a single flat grid.
const (
	planGroupNone    = ""
	planGroupProject = "project"
	planGroupStatus  = "status"
	planGroupUser    = "user"
)

// planView* are the ?view= values (Matt's ask, thread ka-0000005903: "the
// plans index should look more like JiRA with lanes"). board — the default —
// renders one status lane per column, Jira-style; grid is the historical flat
// card grid (which is also where ?group= sectioning lives, so a legacy
// ?group= URL keeps rendering what it always did); tree is the zoomable forest
// (plans_tree.go, thread ka-0000005850), which renders each plan's ALREADY-TREE
// structure — epic → phases → steps — as a tree glyph you can zoom out to a
// whole-portfolio canopy.
const (
	planViewBoard = "board"
	planViewGrid  = "grid"
	planViewTree  = "tree"
)

// normalizePlanView canonicalizes a raw ?view= value, defaulting to board.
func normalizePlanView(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case planViewGrid, "list", "cards":
		return planViewGrid
	case planViewTree, "forest", "trees":
		return planViewTree
	default:
		return planViewBoard
	}
}

// planViewLaned reports whether a view renders the BOARD LANES (planBoardLanes)
// rather than a flat/grouped card list. Board does by definition; the forest
// sections its stands by the same lanes so the two views can never disagree
// about which plans exist or which column they belong in. Both therefore need
// the bounded recent-closed Done feed and neither honors ?group=.
func planViewLaned(view string) bool {
	return view == planViewBoard || view == planViewTree
}

// normalizePlanStatus canonicalizes a raw ?status= value to a recognized token,
// defaulting to "active" (the historical index scope). A few friendly aliases
// (in-progress, done) are folded in so a hand-typed URL still lands.
func normalizePlanStatus(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case planStatusOpen:
		return planStatusOpen
	case planStatusInProgress, "in-progress", "inprogress", "progress":
		return planStatusInProgress
	case planStatusBlocked:
		return planStatusBlocked
	case planStatusDeferred, "defer", "snoozed":
		return planStatusDeferred
	case planStatusClosed, "done", "complete", "completed":
		return planStatusClosed
	case planStatusStale, "idle", "rotting", "sweep":
		return planStatusStale
	case planStatusAll, "any":
		return planStatusAll
	default:
		return planStatusActive
	}
}

// normalizePlanGroup canonicalizes a raw ?group= value, defaulting to none.
func normalizePlanGroup(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case planGroupProject:
		return planGroupProject
	case planGroupStatus, "state":
		return planGroupStatus
	case planGroupUser, "owner", "operator":
		return planGroupUser
	default:
		return planGroupNone
	}
}

// resolvePlanView picks the effective view for one render: an explicit ?view=
// wins; otherwise a ?group= URL implies the grid (sectioning is a grid-only
// feature, and every pre-board grouped URL must keep rendering its sections);
// otherwise the board is the default.
func resolvePlanView(rawView string, hasViewKey bool, groupKey string) string {
	if hasViewKey {
		return normalizePlanView(rawView)
	}
	if groupKey != planGroupNone {
		return planViewGrid
	}
	return planViewBoard
}

// planStatusEnum maps a canonical ?status= value to the daemon status set that
// scopes the index query. "all" → nil (no status filter). "active" → the shared
// activeEnumStatuses (active_statuses.go). Any single-status value scopes to
// exactly that enum.
func planStatusEnum(status string) []string {
	switch status {
	case planStatusAll:
		return nil
	case planStatusOpen:
		return []string{"open"}
	case planStatusInProgress:
		return []string{"in_progress"}
	case planStatusBlocked:
		return []string{"blocked"}
	case planStatusDeferred:
		return []string{"deferred"}
	case planStatusClosed:
		return []string{"closed"}
	case planStatusStale:
		// Stale is a DERIVED sweep over ACTIVE plans (planFreshnessStale gates on
		// non-terminal status, so a closed/deferred plan is never stale). Scope the
		// daemon query to the same active set the default view uses; handlePlans
		// then post-filters the enriched rows on FreshnessStale — the daemon has no
		// column for the folded lastActivityAt to filter on server-side.
		return activeEnumStatuses
	default: // planStatusActive
		return activeEnumStatuses
	}
}

// planStatusLabel humanizes a bead lifecycle status for a group heading /
// chip label ("in_progress" → "In progress").
func planStatusLabel(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "in_progress":
		return "In progress"
	case "open":
		return "Open"
	case "blocked":
		return "Blocked"
	case "closed":
		return "Closed"
	case "deferred":
		return "Deferred"
	case planStatusStale:
		return "Stale"
	case "":
		return "(no status)"
	default:
		return status
	}
}

// planControl is a single filter/group chip in the control strip: a label, the
// URL that applies it (preserving the other dimensions), and whether it is the
// currently-active choice. Mirrors externControl.
type planControl struct {
	Label  string
	URL    string
	Active bool
	// Count is an optional badge after the label. /plans control chips carry no
	// count today, so it stays zero (the shared filter_row partial hides a
	// zero badge), but the field must EXIST for the partial to render: html/template
	// errors on an absent field even under a `{{if .Count}}` guard, and both
	// planControl and externControl feed the same partial (ka-0000006647 / F7).
	Count int
}

// planControls bundles the chip rows the /plans template renders below the
// user-scope header chip.
type planControls struct {
	Views    []planControl
	Statuses []planControl
	Projects []planControl
	Groups   []planControl
}

// planFilterState captures the parsed non-user filter dimensions for one /plans
// render (status, project, group). It is the single source of truth for
// building control-chip URLs that preserve the OTHER dimensions — including the
// user scope (operated_by), which is owned by the separate operator header chip
// but must survive every status/project/group switch. Mirrors externState.
type planFilterState struct {
	status  string
	project string
	group   string
	// view is the RESOLVED board/grid/tree choice (resolvePlanView). Chip URLs
	// carry it explicitly whenever it differs from the board default, so a
	// filter switch never silently flips the layout back.
	view string
	// zoom is the forest zoom level (normalizePlanZoom). Inert on board/grid,
	// but carried through every chip URL like the other dimensions so switching
	// project/status inside the forest never resets how far out you were zoomed.
	zoom string
	// operatedByRaw is the raw ?operated_by= value; operatedByKey records
	// whether the key was present at all. This distinguishes the explicit
	// "?operated_by=" show-all opt-out (key present, value empty — must be
	// preserved) from the absent-key default-to-mine (nothing to preserve).
	operatedByRaw string
	operatedByKey bool
	base          string // chip base path (s.basePath + "/plans")
}

// params renders the non-default filter dimensions as a query map, then applies
// override (an empty override value deletes that key). Defaults (status=active,
// project="", group="") are omitted so URLs stay clean; operated_by is carried
// verbatim whenever its key was present so the user scope never silently resets.
func (st planFilterState) params(override map[string]string) map[string]string {
	m := map[string]string{}
	if st.status != "" && st.status != planStatusActive {
		m["status"] = st.status
	}
	if st.project != "" {
		m["project"] = st.project
	}
	if st.group != planGroupNone {
		m["group"] = st.group
	}
	if st.view != "" && st.view != planViewBoard {
		m["view"] = st.view
	}
	// zoom is a FOREST-ONLY dimension: emit it only on the tree view, and only
	// when it differs from the grove default, so a board/grid URL never grows an
	// inert ?zoom= key.
	if st.view == planViewTree && st.zoom != "" && st.zoom != planZoomGrove {
		m["zoom"] = st.zoom
	}
	if st.operatedByKey {
		// Preserve the user scope verbatim — including the empty show-all form.
		m["operated_by"] = st.operatedByRaw
	}
	for k, v := range override {
		if v == "" {
			delete(m, k)
		} else {
			m[k] = v
		}
	}
	return m
}

// chipURL builds the chip URL for the current state with override applied. An
// empty-valued key (e.g. operated_by= for show-all) is preserved in the query
// so the scope survives the switch.
func (st planFilterState) chipURL(override map[string]string) string {
	params := st.params(override)
	if len(params) == 0 {
		return st.base
	}
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	return st.base + "?" + q.Encode()
}

// controls builds the status / project / group chip rows for the current state.
// projects is the sorted project list offered as project-filter chips (from
// planProjectOptions); an "All" chip is always prepended.
func (st planFilterState) controls(projects []string) planControls {
	c := planControls{}

	// Board ↔ grid ↔ tree layout toggle. The Board chip URL clears BOTH view
	// (board is the default, so the key is omitted) and group (sectioning is
	// grid-only — carrying it over would immediately resolve back to grid); the
	// Grid chip preserves any active grouping. The Tree chip (the zoomable
	// forest, plans_tree.go) also clears group: the forest sections by BOARD
	// LANE, so a carried ?group= would be inert state that silently flips the
	// view back to grid the moment the operator switches to Board.
	c.Views = []planControl{
		// Board is active for the canonical token AND for the empty zero value:
		// board is the default, so a state built without an explicit view (every
		// pre-view caller, and the params() form that omits the default key) must
		// still light the Board chip rather than leaving the row with no active
		// item.
		{Label: "Board", URL: st.chipURL(map[string]string{"view": "", "group": ""}), Active: st.view == planViewBoard || st.view == ""},
		{Label: "Grid", URL: st.chipURL(map[string]string{"view": planViewGrid}), Active: st.view == planViewGrid},
		{Label: "Tree", URL: st.chipURL(map[string]string{"view": planViewTree, "group": ""}), Active: st.view == planViewTree},
	}

	c.Statuses = []planControl{
		{Label: "Active", URL: st.chipURL(map[string]string{"status": ""}), Active: st.status == planStatusActive},
		{Label: "Open", URL: st.chipURL(map[string]string{"status": planStatusOpen}), Active: st.status == planStatusOpen},
		{Label: "In progress", URL: st.chipURL(map[string]string{"status": planStatusInProgress}), Active: st.status == planStatusInProgress},
		{Label: "Blocked", URL: st.chipURL(map[string]string{"status": planStatusBlocked}), Active: st.status == planStatusBlocked},
		{Label: "Deferred", URL: st.chipURL(map[string]string{"status": planStatusDeferred}), Active: st.status == planStatusDeferred},
		{Label: "Stale", URL: st.chipURL(map[string]string{"status": planStatusStale}), Active: st.status == planStatusStale},
		{Label: "Closed", URL: st.chipURL(map[string]string{"status": planStatusClosed}), Active: st.status == planStatusClosed},
		{Label: "All", URL: st.chipURL(map[string]string{"status": planStatusAll}), Active: st.status == planStatusAll},
	}

	c.Projects = []planControl{
		{Label: "All", URL: st.chipURL(map[string]string{"project": ""}), Active: st.project == ""},
	}
	for _, p := range projects {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		c.Projects = append(c.Projects, planControl{
			Label:  p,
			URL:    st.chipURL(map[string]string{"project": p}),
			Active: st.project == p,
		})
	}

	c.Groups = []planControl{
		{Label: "None", URL: st.chipURL(map[string]string{"group": ""}), Active: st.group == planGroupNone},
		{Label: "Project", URL: st.chipURL(map[string]string{"group": planGroupProject}), Active: st.group == planGroupProject},
		{Label: "Status", URL: st.chipURL(map[string]string{"group": planGroupStatus}), Active: st.group == planGroupStatus},
		{Label: "User", URL: st.chipURL(map[string]string{"group": planGroupUser}), Active: st.group == planGroupUser},
	}
	return c
}

// isFiltered reports whether any non-default filter/group dimension is active —
// drives the "× clear" affordance (the user scope is cleared by its own header
// chip, so it is intentionally excluded here).
func (st planFilterState) isFiltered() bool {
	return (st.status != "" && st.status != planStatusActive) ||
		st.project != "" ||
		st.group != planGroupNone
}

// clearURL drops status/project/group but preserves the user scope — the "×
// clear" target. Mirrors extern's clear, which keeps its primary lens.
func (st planFilterState) clearURL() string {
	return st.chipURL(map[string]string{"status": "", "project": "", "group": ""})
}

// planGroup is one section when the index is grouped (?group=). Rows preserve
// the already-applied (index query) order; groups are ordered largest-first
// then alphabetically by label — matching externGroup.
type planGroup struct {
	// Key is the stable, lowercased grouping key (used for anchors / CSS ids).
	Key string
	// Label is the human-facing section heading.
	Label string
	// Rows are the plan cards in this group, in list order.
	Rows []planRowView
}

// groupPlanRows sections the flat plan-row list by the given group key. Returns
// nil for none/unrecognized (the caller then renders one flat grid).
func groupPlanRows(rows []planRowView, groupKey string) []planGroup {
	keyFn := planGroupKeyFunc(groupKey)
	if keyFn == nil {
		return nil
	}
	byKey := map[string]*planGroup{}
	var order []string
	for _, row := range rows {
		key, label := keyFn(row)
		g, ok := byKey[key]
		if !ok {
			g = &planGroup{Key: key, Label: label}
			byKey[key] = g
			order = append(order, key)
		} else if planLabelRicherThan(label, g.Label, key) {
			// First-writer-wins would pin a group heading to whatever the
			// first row projected — a problem only for group-by-user, where
			// one plan may lack the operator display name a sibling carries
			// (per-bead attribution enrichment). Upgrade the heading to the
			// richest (real display) label any row in the bucket offers. A
			// no-op for the pure-function project/status projectors.
			g.Label = label
		}
		g.Rows = append(g.Rows, row)
	}
	groups := make([]planGroup, 0, len(order))
	for _, key := range order {
		groups = append(groups, *byKey[key])
	}
	sort.SliceStable(groups, func(i, j int) bool {
		if len(groups[i].Rows) != len(groups[j].Rows) {
			return len(groups[i].Rows) > len(groups[j].Rows)
		}
		return groups[i].Label < groups[j].Label
	})
	return groups
}

// planLabelRicherThan reports whether candidate is a strictly better group
// heading than current for a bucket keyed on key. Labels rank in three tiers
// (planLabelRank): the id fallback (empty or the raw key) < an email fallback <
// a real display name. A strictly higher tier upgrades the heading, so a real
// name beats an email beats an id — while two labels in the SAME tier never
// fight (the first stable one keeps it, e.g. two distinct display names, or an
// email heading that a sibling's identical email cannot displace).
//
// This is what lets a group-by-user heading recover a display name that only a
// later sibling row carries: the first row may project just the operator email
// (display-name enrichment missing), and the email is a middle tier that a real
// name still outranks — the pre-tier rule only upgraded from the bare id, so an
// email-labeled heading was pinned even when a real name was available.
func planLabelRicherThan(candidate, current, key string) bool {
	return planLabelRank(candidate, key) > planLabelRank(current, key)
}

// planLabelRank scores a group heading's richness for a bucket keyed on key:
//
//	0 — id fallback: empty, or the raw key itself (planUserGroupKey's last
//	    resort when no display name or email is known). Case-insensitive against
//	    key because planUserGroupKey lowercases the key while labels keep case.
//	1 — email fallback: an address (contains "@") standing in for a missing
//	    display name. The key check runs first, so a key that is itself an email
//	    (operated_by absent, email promoted to the key) still ranks 0, not 1.
//	2 — a real display name: a human label carrying more than the id or email.
func planLabelRank(label, key string) int {
	label = strings.TrimSpace(label)
	if label == "" || strings.EqualFold(label, key) {
		return 0
	}
	if strings.Contains(label, "@") {
		return 1
	}
	return 2
}

// planGroupKeyFunc maps a group key to a row→(key,label) projector, or nil for
// none/unrecognized.
func planGroupKeyFunc(groupKey string) func(planRowView) (string, string) {
	switch groupKey {
	case planGroupProject:
		return func(r planRowView) (string, string) {
			if strings.TrimSpace(r.Project) == "" {
				return "~none", "(no project)"
			}
			return r.Project, r.Project
		}
	case planGroupStatus:
		return func(r planRowView) (string, string) {
			s := strings.TrimSpace(r.Status)
			if s == "" {
				return "~none", "(no status)"
			}
			return strings.ToLower(s), planStatusLabel(s)
		}
	case planGroupUser:
		return planUserGroupKey
	}
	return nil
}

// planUserGroupKey derives a stable key + human label for the group-by-user
// section from a plan row's operated-by attribution, falling back through
// display name → email → raw operated_by id → creating agent identity, and to
// an "(unassigned)" bucket when none is set.
func planUserGroupKey(r planRowView) (string, string) {
	key := strings.TrimSpace(r.OperatedBy)
	if key == "" {
		key = strings.TrimSpace(r.OperatedByEmail)
	}
	label := strings.TrimSpace(r.OperatedByDisplay)
	if label == "" {
		label = strings.TrimSpace(r.OperatedByEmail)
	}
	if key == "" {
		// No operator attribution — fall back to the creating agent identity.
		key = strings.TrimSpace(r.CreatedBy)
		if label == "" {
			label = key
		}
	}
	if key == "" {
		return "~none", "(unassigned)"
	}
	if label == "" {
		label = key
	}
	return strings.ToLower(key), label
}
