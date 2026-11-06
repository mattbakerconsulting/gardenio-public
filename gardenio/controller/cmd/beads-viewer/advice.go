package main

import (
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// adviceListLimit caps the number of advice beads pulled from the daemon.
// Advice is bounded by hand (operators write it, not bots), so a single fetch
// of up to 1000 covers any realistic deployment.
const adviceListLimit = 1000

// scopeKind enumerates the advice-targeting buckets that drive the sidebar
// and the ?scope= query parameter. Mirrors iOS AdviceTab.ScopeNode.
type scopeKind int

const (
	scopeAll scopeKind = iota
	scopeGlobal
	scopeAdvanced
	scopeProject
	scopeRole
	scopeAgent
)

// parsedScope is the resolved value of the ?scope= query parameter.
type parsedScope struct {
	kind  scopeKind
	value string // empty for all/global/advanced; bucket name for project/role/agent
}

// parseScopeParam decodes ?scope= into a parsedScope. Unknown or empty input
// falls back to scopeAll (the default sidebar selection).
func parseScopeParam(q string) parsedScope {
	q = strings.TrimSpace(q)
	switch q {
	case "", "all":
		return parsedScope{kind: scopeAll}
	case "global":
		return parsedScope{kind: scopeGlobal}
	case "advanced":
		return parsedScope{kind: scopeAdvanced}
	}
	if v, ok := strings.CutPrefix(q, "project:"); ok && v != "" {
		return parsedScope{kind: scopeProject, value: v}
	}
	if v, ok := strings.CutPrefix(q, "role:"); ok && v != "" {
		return parsedScope{kind: scopeRole, value: v}
	}
	if v, ok := strings.CutPrefix(q, "agent:"); ok && v != "" {
		return parsedScope{kind: scopeAgent, value: v}
	}
	return parsedScope{kind: scopeAll}
}

// adviceClassification is the per-bead categorization used both for sidebar
// counts and for filtering.
type adviceClassification struct {
	kind     scopeKind // scopeGlobal | scopeProject | scopeRole | scopeAgent | scopeAdvanced
	projects []string  // project: labels (deduped, original order)
	roles    []string
	agents   []string
	advanced bool // true when a gN: prefix or multi-target combo forced advanced
}

// classifyAdvice walks a bead's labels and produces a classification suitable
// for sidebar discovery and scope filtering. Matches the controller's
// matching.go semantics: any gN: prefix → advanced; multiple non-global
// targeting labels → advanced; otherwise the single targeting label wins.
func classifyAdvice(labels []string) adviceClassification {
	var c adviceClassification
	hasGlobal := false
	for _, l := range labels {
		if hasGroupPrefix(l) {
			c.advanced = true
		}
		switch {
		case l == "global":
			hasGlobal = true
		case strings.HasPrefix(l, "project:"):
			c.projects = appendUnique(c.projects, l[len("project:"):])
		case strings.HasPrefix(l, "role:"):
			c.roles = appendUnique(c.roles, l[len("role:"):])
		case strings.HasPrefix(l, "agent:"):
			c.agents = appendUnique(c.agents, l[len("agent:"):])
		}
	}
	if c.advanced {
		c.kind = scopeAdvanced
		return c
	}
	// Same precedence as AdviceScope.fromLabels: a single non-global targeting
	// label dominates global; multiple targeting labels → advanced.
	targets := len(c.projects) + len(c.roles) + len(c.agents)
	switch {
	case targets == 0 && hasGlobal:
		c.kind = scopeGlobal
	case targets == 0:
		// No targeting labels at all → treat as global (kd advice add default).
		c.kind = scopeGlobal
	case targets == 1:
		switch {
		case len(c.projects) == 1:
			c.kind = scopeProject
		case len(c.agents) == 1:
			c.kind = scopeAgent
		case len(c.roles) == 1 && hasGlobal:
			// role: has no pre-filter; combined with global the effective
			// audience is everyone — surface as global.
			c.kind = scopeGlobal
		case len(c.roles) == 1:
			c.kind = scopeRole
		}
	default:
		// Multiple targeting labels: treat the project/agent pre-filter as the
		// effective scope; a role + global combo is still global.
		// (See AdviceDraft.swift lines 96-103 for the equivalent logic.)
		switch {
		case len(c.projects) >= 1:
			c.kind = scopeProject
		case len(c.agents) >= 1:
			c.kind = scopeAgent
		case len(c.roles) >= 1 && hasGlobal:
			c.kind = scopeGlobal
		default:
			c.advanced = true
			c.kind = scopeAdvanced
		}
	}
	return c
}

// hasGroupPrefix reports whether a label starts with a `gN:` grouping prefix
// like "g0:", "g1:", … . Used to detect advanced advice.
func hasGroupPrefix(label string) bool {
	rest, ok := strings.CutPrefix(label, "g")
	if !ok {
		return false
	}
	colon := strings.IndexByte(rest, ':')
	if colon < 1 {
		return false
	}
	for i := range colon {
		if rest[i] < '0' || rest[i] > '9' {
			return false
		}
	}
	return true
}

func appendUnique(xs []string, v string) []string {
	if slices.Contains(xs, v) {
		return xs
	}
	return append(xs, v)
}

// scopeMatches reports whether a bead's classification falls within the
// selected sidebar scope. scopeAll matches everything.
func scopeMatches(c adviceClassification, scope parsedScope) bool {
	switch scope.kind {
	case scopeAll:
		return true
	case scopeGlobal:
		return c.kind == scopeGlobal
	case scopeAdvanced:
		return c.kind == scopeAdvanced
	case scopeProject:
		return slices.Contains(c.projects, scope.value)
	case scopeRole:
		return slices.Contains(c.roles, scope.value)
	case scopeAgent:
		return slices.Contains(c.agents, scope.value)
	}
	return false
}

// statusOpenSortKey returns 0 for open/in_progress beads and 1 for everything
// else, so closed advice sinks to the bottom of the list.
func statusOpenSortKey(status string) int {
	switch status {
	case "open", "in_progress", "":
		return 0
	default:
		return 1
	}
}

// adviceRowView is one row on the /advice page.
type adviceRowView struct {
	ID           string
	Title        string
	DescPreview  string
	Project      string
	Priority     int
	Status       string
	IsClosed     bool
	HookTrigger  string          // populated when the bead carries a hook trigger
	Scope        scopeChipView   // primary chip — drives sort tiering
	Chips        []scopeChipView // all scope chips (project + role + agent) — always rendered
	CreatedBy    string          // author of the advice, surfaced in the row meta
	Ago          string
	UpdatedAtRFC string
}

// scopeChipView labels the row's scope chip on the All view. Kind drives the
// CSS class (chip-scope-global / chip-scope-project / …).
type scopeChipView struct {
	Kind  string // global, project, role, agent, advanced
	Label string // text shown in the chip (e.g. "project:gardenio", "global")
}

// sidebarNode is one selectable scope row in the sidebar. Active is set on the
// node matching the current ?scope= query parameter.
type sidebarNode struct {
	Kind   string // all, global, advanced, project, role, agent
	Label  string
	Value  string // ?scope= value used in the URL
	Href   string // pre-built relative URL (preserves ?q=)
	Count  int
	Active bool
}

// adviceSidebar groups discovered scope nodes for rendering.
type adviceSidebar struct {
	All      sidebarNode
	Global   sidebarNode
	Advanced sidebarNode // omitted (zero count) when no advanced advice exists
	Projects []sidebarNode
	Roles    []sidebarNode
	Agents   []sidebarNode
}

// adviceStats is the aggregate header strip rendered above the advice list.
// Counts are scope-wide (independent of the current ?scope= filter) and
// surface the per-bucket breakdown without forcing the operator to scan the
// sidebar. Open-only counts so the strip tracks actionable advice; Closed
// is a separate signal for archived volume.
type adviceStats struct {
	Total    int // open advice across all scopes
	Global   int
	Projects int // count of distinct projects with open advice
	Roles    int // count of distinct roles with open advice
	Agents   int // count of distinct agents with open advice
	Advanced int
	Closed   int // closed advice (any scope) — visible but archived
	Shown    int // rows after scope + search filter
}

// handleAdvice renders /advice — a list of advice beads with sidebar scope
// browser and search. Mirrors iOS AdviceTab. Read-only — no editor.
//
// Query params:
//
//	?scope=  one of all|global|advanced|project:<n>|role:<n>|agent:<n> (default: all)
//	?q=      free-text search across title/description/id/labels
func (s *Server) handleAdvice(w http.ResponseWriter, r *http.Request) {
	scope := parseScopeParam(r.URL.Query().Get("scope"))
	query := strings.TrimSpace(r.URL.Query().Get("q"))

	data := map[string]any{
		"Scope":     scopeQueryString(scope),
		"Query":     query,
		"Title":     scopeTitle(scope),
		"Sidebar":   adviceSidebar{},
		"Rows":      []adviceRowView{},
		"Truncated": false,
		"Limit":     adviceListLimit,
	}

	res, err := s.daemon.ListBeadsFiltered(r.Context(), beadsapi.ListBeadsQuery{
		Types: []string{"advice"},
		Sort:  "created_at",
		Limit: adviceListLimit,
	})
	if err != nil {
		s.logger.Error("listing advice", "error", err)
		data["Error"] = "Failed to load advice."
		s.render(w, r, "advice.html", data)
		return
	}

	now := time.Now()
	type pair struct {
		bead  *beadsapi.BeadDetail
		class adviceClassification
	}
	all := make([]pair, 0, len(res.Beads))
	projectSet := map[string]struct{}{}
	roleSet := map[string]struct{}{}
	agentSet := map[string]struct{}{}
	advancedCount := 0
	globalOpenCount := 0
	allOpenCount := 0
	closedCount := 0
	projectOpenCount := map[string]int{}
	roleOpenCount := map[string]int{}
	agentOpenCount := map[string]int{}
	advancedOpenCount := 0

	for _, b := range res.Beads {
		if b == nil {
			continue
		}
		c := classifyAdvice(b.Labels)
		all = append(all, pair{b, c})
		isOpen := statusOpenSortKey(b.Status) == 0
		if isOpen {
			allOpenCount++
		} else {
			closedCount++
		}
		for _, name := range c.projects {
			projectSet[name] = struct{}{}
			if isOpen {
				projectOpenCount[name]++
			}
		}
		for _, name := range c.roles {
			roleSet[name] = struct{}{}
			if isOpen {
				roleOpenCount[name]++
			}
		}
		for _, name := range c.agents {
			agentSet[name] = struct{}{}
			if isOpen {
				agentOpenCount[name]++
			}
		}
		if c.kind == scopeGlobal && isOpen {
			globalOpenCount++
		}
		if c.kind == scopeAdvanced {
			advancedCount++
			if isOpen {
				advancedOpenCount++
			}
		}
	}

	// Filter by scope, then by search query. The scope filter shapes which
	// beads contribute to the visible list — counts above stay scope-wide so
	// the sidebar reflects the full advice population.
	rows := make([]adviceRowView, 0, len(all))
	for _, p := range all {
		if !scopeMatches(p.class, scope) {
			continue
		}
		if query != "" && !matchAdviceSearch(p.bead, query) {
			continue
		}
		rows = append(rows, buildAdviceRow(p.bead, p.class, scope, now))
	}

	sort.SliceStable(rows, func(i, j int) bool {
		// Find the underlying classifications by id — rebuild compact keys.
		ki := statusOpenSortKey(rows[i].Status)
		kj := statusOpenSortKey(rows[j].Status)
		if ki != kj {
			return ki < kj
		}
		// Use the row's own scope chip kind to derive the delivery key.
		dki := scopeDeliveryKeyFromName(rows[i].Scope.Kind)
		dkj := scopeDeliveryKeyFromName(rows[j].Scope.Kind)
		if dki != dkj {
			return dki < dkj
		}
		if rows[i].Priority != rows[j].Priority {
			return rows[i].Priority < rows[j].Priority
		}
		// CreatedAt desc — fall through using ID as a stable tiebreaker.
		return rows[i].ID > rows[j].ID
	})

	// Build sidebar. Counts use OPEN advice only so the sidebar shows
	// actionable buckets (closed advice still appears in the list itself).
	hrefFor := func(scopeValue string) string {
		return adviceHref(s.basePath, scopeValue, query)
	}
	sidebar := adviceSidebar{
		All: sidebarNode{
			Kind: "all", Label: "All Advice", Value: "",
			Href:  hrefFor(""),
			Count: allOpenCount, Active: scope.kind == scopeAll,
		},
		Global: sidebarNode{
			Kind: "global", Label: "Global", Value: "global",
			Href:  hrefFor("global"),
			Count: globalOpenCount, Active: scope.kind == scopeGlobal,
		},
	}
	if advancedCount > 0 {
		sidebar.Advanced = sidebarNode{
			Kind: "advanced", Label: "Advanced targeting", Value: "advanced",
			Href:  hrefFor("advanced"),
			Count: advancedOpenCount, Active: scope.kind == scopeAdvanced,
		}
	}
	sidebar.Projects = sidebarNodes("project", projectSet, projectOpenCount, scope, scopeProject, hrefFor)
	sidebar.Roles = sidebarNodes("role", roleSet, roleOpenCount, scope, scopeRole, hrefFor)
	sidebar.Agents = sidebarNodes("agent", agentSet, agentOpenCount, scope, scopeAgent, hrefFor)

	data["Sidebar"] = sidebar
	data["Rows"] = rows
	data["Stats"] = adviceStats{
		Total:    allOpenCount,
		Global:   globalOpenCount,
		Projects: countOpenBuckets(projectOpenCount),
		Roles:    countOpenBuckets(roleOpenCount),
		Agents:   countOpenBuckets(agentOpenCount),
		Advanced: advancedOpenCount,
		Closed:   closedCount,
		Shown:    len(rows),
	}
	data["Truncated"] = res.Total > len(res.Beads)
	s.render(w, r, "advice.html", data)
}

// countOpenBuckets returns the number of buckets with at least one open
// advice — drives the "Projects (N)" / "Roles (N)" / "Agents (N)" tallies in
// the header strip without leaking per-bucket counts (those live in the
// sidebar).
func countOpenBuckets(m map[string]int) int {
	n := 0
	for _, v := range m {
		if v > 0 {
			n++
		}
	}
	return n
}

// matchAdviceSearch implements iOS-parity search: case-insensitive substring
// match across title, description, id, and labels (joined).
func matchAdviceSearch(b *beadsapi.BeadDetail, q string) bool {
	q = strings.ToLower(q)
	if strings.Contains(strings.ToLower(b.Title), q) {
		return true
	}
	if strings.Contains(strings.ToLower(b.Description), q) {
		return true
	}
	if strings.Contains(strings.ToLower(b.ID), q) {
		return true
	}
	if strings.Contains(strings.ToLower(strings.Join(b.Labels, " ")), q) {
		return true
	}
	return false
}

func buildAdviceRow(b *beadsapi.BeadDetail, c adviceClassification, scope parsedScope, now time.Time) adviceRowView {
	_ = scope // scope no longer gates chip visibility — chips always render.
	row := adviceRowView{
		ID:           b.ID,
		Title:        b.Title,
		DescPreview:  snippet(b.Description, 240),
		Project:      projectFromLabels(b.Labels),
		Priority:     b.Priority,
		Status:       b.Status,
		IsClosed:     statusOpenSortKey(b.Status) != 0,
		HookTrigger:  b.Fields["hook_trigger"],
		Scope:        buildScopeChip(c),
		Chips:        buildScopeChips(c),
		CreatedBy:    b.CreatedBy,
		Ago:          fmtTimeAgo(b.UpdatedAt, now),
		UpdatedAtRFC: rfcTime(b.UpdatedAt),
	}
	return row
}

// buildScopeChips emits one chip per scope dimension on the bead so the row
// surfaces the full targeting set (e.g. `project:gardenio` + `role:gardener`)
// instead of only the primary scope. Advanced beads short-circuit to a single
// "advanced" chip — their multi-label semantics are intentionally opaque.
func buildScopeChips(c adviceClassification) []scopeChipView {
	if c.kind == scopeAdvanced {
		return []scopeChipView{{Kind: "advanced", Label: "advanced"}}
	}
	chips := make([]scopeChipView, 0, 1+len(c.projects)+len(c.roles)+len(c.agents))
	if c.kind == scopeGlobal {
		chips = append(chips, scopeChipView{Kind: "global", Label: "global"})
	}
	for _, p := range c.projects {
		chips = append(chips, scopeChipView{Kind: "project", Label: "project:" + p})
	}
	for _, r := range c.roles {
		chips = append(chips, scopeChipView{Kind: "role", Label: "role:" + r})
	}
	for _, a := range c.agents {
		chips = append(chips, scopeChipView{Kind: "agent", Label: "agent:" + a})
	}
	if len(chips) == 0 {
		chips = append(chips, scopeChipView{Kind: "global", Label: "global"})
	}
	return chips
}

func buildScopeChip(c adviceClassification) scopeChipView {
	switch c.kind {
	case scopeGlobal:
		return scopeChipView{Kind: "global", Label: "global"}
	case scopeProject:
		v := ""
		if len(c.projects) > 0 {
			v = c.projects[0]
		}
		return scopeChipView{Kind: "project", Label: "project:" + v}
	case scopeRole:
		v := ""
		if len(c.roles) > 0 {
			v = c.roles[0]
		}
		return scopeChipView{Kind: "role", Label: "role:" + v}
	case scopeAgent:
		v := ""
		if len(c.agents) > 0 {
			v = c.agents[0]
		}
		return scopeChipView{Kind: "agent", Label: "agent:" + v}
	case scopeAdvanced:
		return scopeChipView{Kind: "advanced", Label: "advanced"}
	}
	return scopeChipView{Kind: "global", Label: "global"}
}

func scopeDeliveryKeyFromName(name string) int {
	switch name {
	case "global":
		return 0
	case "project":
		return 1
	case "role":
		return 2
	case "agent":
		return 3
	case "advanced":
		return 9
	}
	return 5
}

// scopeQueryString turns a parsedScope back into the canonical ?scope= form
// (empty for the default "all", to keep URLs short).
func scopeQueryString(s parsedScope) string {
	switch s.kind {
	case scopeAll:
		return ""
	case scopeGlobal:
		return "global"
	case scopeAdvanced:
		return "advanced"
	case scopeProject:
		return "project:" + s.value
	case scopeRole:
		return "role:" + s.value
	case scopeAgent:
		return "agent:" + s.value
	}
	return ""
}

func scopeTitle(s parsedScope) string {
	switch s.kind {
	case scopeAll:
		return "All Advice"
	case scopeGlobal:
		return "Global"
	case scopeAdvanced:
		return "Advanced targeting"
	case scopeProject:
		return "Project: " + s.value
	case scopeRole:
		return "Role: " + s.value
	case scopeAgent:
		return s.value
	}
	return "Advice"
}

// sidebarNodes builds the list of nodes for one bucket (projects, roles, agents),
// alphabetically sorted by name, marking the currently-active node.
func sidebarNodes(prefix string, set map[string]struct{}, openCount map[string]int, scope parsedScope, want scopeKind, hrefFor func(string) string) []sidebarNode {
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		return strings.ToLower(names[i]) < strings.ToLower(names[j])
	})
	out := make([]sidebarNode, 0, len(names))
	for _, n := range names {
		value := prefix + ":" + n
		out = append(out, sidebarNode{
			Kind:   prefix,
			Label:  n,
			Value:  value,
			Href:   hrefFor(value),
			Count:  openCount[n],
			Active: scope.kind == want && scope.value == n,
		})
	}
	return out
}

// adviceHref builds a /advice URL for the given scope and search query.
// scopeValue == "" omits the ?scope= param (canonical "all" form).
func adviceHref(basePath, scopeValue, query string) string {
	var b strings.Builder
	b.WriteString(basePath)
	b.WriteString("/advice")
	first := true
	add := func(k, v string) {
		if first {
			b.WriteByte('?')
			first = false
		} else {
			b.WriteByte('&')
		}
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(queryEscape(v))
	}
	if scopeValue != "" {
		add("scope", scopeValue)
	}
	if query != "" {
		add("q", query)
	}
	return b.String()
}

// queryEscape is a minimal URL query escaper. Avoids pulling net/url for the
// only call site here — values come from short, safe label strings.
func queryEscape(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == ' ':
			b.WriteByte('+')
		case (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == ':':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0x0f])
		}
	}
	return b.String()
}
