package grafanaembed

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The garden path: closed allowlists in, frame URLs out
// (gardenio/docs/garden-grafana-panels.md, "The URL" and "Allowlist"). A garden
// route hands Build a set name and the viewer's project / range / theme
// params. Build checks each against a closed set and looks the dashboard uid
// and panel ids up in the registry, so nothing author- or viewer-supplied
// reaches a frame URL except a project name shaped as a DNS label. Every route
// that returns a Build result must be routeAdminOnly: grafana.clonk.tv runs
// with defaultOrgRole Admin behind the /gardenio-admins edge gate.

// ErrInvalid wraps every rejection Build makes. A route answers it with a 400
// and never falls back to building a URL anyway.
var ErrInvalid = errors.New("grafanaembed: invalid input")

// Theme is the Grafana theme a frame renders in. It follows the surface the
// frame sits on, not the sky: the Workbench panel is the --box surface, dark
// by day and night, so the costs kind asks for ThemeDark in both skins.
type Theme string

const (
	ThemeDark  Theme = "dark"
	ThemeLight Theme = "light"
)

// DefaultTheme is the theme when the request leaves it out.
const DefaultTheme = ThemeDark

// ParseTheme accepts "light" or "dark"; "" is DefaultTheme.
func ParseTheme(s string) (Theme, error) {
	switch Theme(s) {
	case "":
		return DefaultTheme, nil
	case ThemeDark, ThemeLight:
		return Theme(s), nil
	}
	return "", fmt.Errorf("%w: theme %q (want light or dark)", ErrInvalid, s)
}

// Range is one time-range chip and the Grafana from/to it stands for.
type Range struct {
	Name string // the request param value and chip label
	From string
	To   string
}

// ranges is the closed set of chips. `today` is since 00:00 UTC (now/d with
// timezone=utc on the URL), not now-24h, so the chart's total matches the
// farm's today_usd coin (costWindows in gardenhub/farm_cost.go). 30d is the
// VictoriaMetrics retention, so its left edge may be thin.
var ranges = []Range{
	{Name: "today", From: "now/d", To: "now"},
	{Name: "7d", From: "now-7d", To: "now"},
	{Name: "30d", From: "now-30d", To: "now"},
}

// DefaultRange is the chip when the request leaves it out: early in the UTC
// day `today` is nearly empty, and a week shows a shape.
const DefaultRange = "7d"

// Timezone is passed on every garden URL so now/d rounds to UTC midnight even
// on a dashboard whose JSON does not set "timezone": "utc".
const Timezone = "utc"

// ParseRange accepts a chip name; "" is DefaultRange.
func ParseRange(s string) (Range, error) {
	if s == "" {
		s = DefaultRange
	}
	for _, r := range ranges {
		if r.Name == s {
			return r, nil
		}
	}
	return Range{}, fmt.Errorf("%w: range %q (want today, 7d or 30d)", ErrInvalid, s)
}

// projectRe is a DNS label: 1–63 of a-z, 0-9 and '-', a letter or digit at
// each end. Project names are DNS labels, so this is the whole charset a
// var-project value may carry.
var projectRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidProject reports whether s is shaped as a project name. It checks shape
// only: the route also checks the farm knows the project (a type:project bead).
func ValidProject(s string) bool { return projectRe.MatchString(s) }

// ValidAgent reports whether s is shaped as an agent name, which is a pod name:
// the same DNS label a project is. No regex metacharacter fits the charset, so
// a name, or names joined by '|', is a literal match in the board's
// `agent=~"$agent"` selectors.
func ValidAgent(s string) bool { return projectRe.MatchString(s) }

// AgentsMax caps the agents one per-agent request may name: a gardener's runs
// as its panel lists them (gardenhub's gardenerRunsMax).
const AgentsMax = 25

// allProjects is the value a fleet set sends for its dashboard's project
// variable. Only the server sets it; ValidProject rejects it from a request.
const allProjects = "$__all"

// CardHeight is the iframe height attribute (px) on a garden card: the no-CSS
// fallback under the card's 4:3, 260–360 px clamp. 360 is the height
// Playwright verified as scroll-free on /costs (costsEmbedHeight).
const CardHeight = 360

// Panel is one registry entry: a panel on its set's dashboard and the words its
// card carries while the frame loads (or when Grafana is not set up).
type Panel struct {
	ID    int // Grafana panelId on the set's dashboard
	Title string
	Scope string // the line under the title: units, window, source
	Note  string // optional
	// LandOnly panels read a tenant land's own account; Build drops them
	// unless Query.Land says the project is one.
	LandOnly bool
}

// Set is a named, closed group of panels on one dashboard.
type Set struct {
	Name string
	UID  string // dashboard uid
	// PerProject sets take a project and are scoped to it; the others are
	// fleet-wide and refuse one.
	PerProject bool
	// ProjectVar says the dashboard has a `project` template variable. A
	// per-project set sends var-project=<project>; a fleet set sends $__all.
	ProjectVar bool
	// PerAgent sets take one or more agents (Query.Agents) and are scoped to
	// them: the board's `agent` textbox gets the name, or the names joined by
	// '|'. They take no project.
	PerAgent bool
	Panels   []Panel
}

// sets is the registry. Changing an entry means changing this table and the
// dashboard JSON in one PR: catalog-generated dashboards renumber panels when
// the catalog is reordered (costs.go's "PANEL IDS ARE POSITIONAL"), so panel
// ids here only ever point at hand-written, explicitly numbered dashboards or
// at the live /costs dashboard as it stands.
//
// costs-fleet and costs-project both embed gardenio-garden-costs, whose ids
// grafana_garden_costs_dashboard_test.go pins (garden-grafana-panels.md, "After
// 1.4: gardenio-garden-costs"). The board has a `project` variable, so the
// fleet set sends var-project=$__all and var-account stays at its All default.
// Panels 9 and 12 read a tenant land's lands-<project> account and are LandOnly.
var sets = []Set{
	{
		Name:       "costs-fleet",
		UID:        "gardenio-garden-costs",
		ProjectVar: true,
		Panels: []Panel{
			{ID: 1, Title: "Spend (window)", Scope: "USD · the chip's window · LiteLLM bill, all projects"},
			{ID: 2, Title: "Fleet spend rate", Scope: "USD/hour · 24h + 1h rolling · LiteLLM bill"},
			{ID: 3, Title: "Spend by project over time", Scope: "USD per trailing hour · by project · stacked"},
			{ID: 4, Title: "Spend by model", Scope: "USD · the chip's window · by model, fleet-wide"},
			{ID: 5, Title: "Spend by account", Scope: "USD · the chip's window · by credential account"},
		},
	},
	{
		Name:       "costs-project",
		UID:        "gardenio-garden-costs",
		PerProject: true,
		ProjectVar: true,
		Panels: []Panel{
			{ID: 1, Title: "Spend (window)", Scope: "USD · the chip's window · LiteLLM bill for this project"},
			{ID: 10, Title: "Project spend rate", Scope: "USD/hour · 24h + 1h rolling · this project's tagged spend"},
			{ID: 6, Title: "Spend by agent", Scope: "USD · top 10 agents · the sweep's ~48 h lookback", Note: "The range chips do not move this figure."},
			{ID: 7, Title: "Spend by role", Scope: "USD · by role · the sweep's ~48 h lookback", Note: "The range chips do not move this figure."},
			{ID: 8, Title: "Spend by model (project)", Scope: "USD · by model · the sweep's ~48 h lookback", Note: "The range chips do not move this figure."},
			{ID: 9, Title: "Land purse spend, USD", Scope: "USD · the chip's window and to date · the land's own account", Note: "Dollars at cost, not the tenant's credits.", LandOnly: true},
			{ID: 12, Title: "Land spend rate", Scope: "USD per trailing hour · the land's account · $2/hour alert floor", LandOnly: true},
		},
	},
	// agent and gardener embed gardenio-garden-agents (step 4.2, gd-g7Ms8Ddfmj),
	// whose ids grafana_garden_agents_dashboard_test.go pins: one agent on its
	// bead's face, and a gardener loop's runs on its panel.
	{
		Name:     "agent",
		UID:      "gardenio-garden-agents",
		PerAgent: true,
		Panels: []Panel{
			{ID: 1, Title: "Cost over time", Scope: "USD · its real LiteLLM bill so far · the sweep's ~48 h lookback"},
			{ID: 2, Title: "Context size", Scope: "tokens in its context window · per session"},
			{ID: 3, Title: "Turns", Scope: "turns taken · per session, cumulative"},
			{ID: 4, Title: "Tokens", Scope: "tokens per 5 minutes · by type"},
		},
	},
	{
		Name:     "gardener",
		UID:      "gardenio-garden-agents",
		PerAgent: true,
		Panels: []Panel{
			{ID: 5, Title: "Spend by run", Scope: "USD · each run's real bill · the sweep's ~48 h lookback", Note: "Runs older than the lookback are not here; the range chips do not move this figure."},
			{ID: 1, Title: "Cost over time", Scope: "USD · each run's real bill so far · top 10 runs"},
			{ID: 2, Title: "Context size", Scope: "tokens in each run's context window · per session"},
			{ID: 3, Title: "Turns", Scope: "turns taken by each run · per session, cumulative"},
			{ID: 4, Title: "Tokens", Scope: "tokens per 5 minutes · by type, over its runs"},
		},
	},
	// health-land embeds gardenio-garden-health, whose ids
	// grafana_garden_health_dashboard_test.go pins (garden-grafana-panels.md,
	// "After 4.1: gardenio-garden-health"): the Lands SLOs, fleet-wide, on every
	// project, and the land's own Lands API use and latency SLIs, cut by
	// var-project, on a tenant land only. A land is the project of its name.
	{
		Name:       "health-land",
		UID:        "gardenio-garden-health",
		PerProject: true,
		ProjectVar: true,
		Panels: []Panel{
			{ID: 1, Title: "SLO burn rate", Scope: "× budget · 1 h error ratio over (1 − objective) · page at 14.4 · every Lands SLO"},
			{ID: 2, Title: "Error budget left", Scope: "share of each SLO's 30-day budget · the last 30 days", Note: "The range chips do not move this figure."},
			{ID: 3, Title: "SLO error ratio, 5 minutes", Scope: "bad share of events · trailing 5 min · every Lands SLO"},
			{ID: 4, Title: "Uptime probes now", Scope: "last result per target · the off-cluster prober, every minute"},
			{ID: 5, Title: "Probe availability", Scope: "share of probes passed · the chip's window · 99.5% objective"},
			{ID: 7, Title: "Land API requests by caller", Scope: "answers/second · member, agent, operator · admitted calls", LandOnly: true},
			{ID: 9, Title: "Member calls, last 7 days", Scope: "Lands API answers to the land's members · the v1-beta weekly-use read", Note: "Always the trailing 7 days.", LandOnly: true},
			{ID: 8, Title: "Land API problems by code", Scope: "problem answers/second · by problem code", LandOnly: true},
			{ID: 10, Title: "Land API calls by op", Scope: "answers · the chip's window · by contract operation", LandOnly: true},
			{ID: 12, Title: "Commons event lag", Scope: "seconds · p50 and p95 · write to broadcast · 5 s objective", LandOnly: true},
			{ID: 13, Title: "Agent spawn to working", Scope: "seconds · p50 and p95 over a trailing day · 300 s objective", LandOnly: true},
		},
	},
}

// LookupSet returns the registry set with this name.
func LookupSet(name string) (Set, bool) { return lookup(sets, name) }

func lookup(reg []Set, name string) (Set, bool) {
	for _, s := range reg {
		if s.Name == name {
			return s, true
		}
	}
	return Set{}, false
}

// Query is a garden request for one set, as the route received it.
type Query struct {
	Set     string
	Project string // "" for a fleet set
	Range   string // "" is DefaultRange
	Theme   string // "" is DefaultTheme
	// Agents are a per-agent set's agents, each shaped by ValidAgent. The route
	// takes one from the request (agent) or reads them from the farm (a
	// gardener's runs); a fleet or project set refuses them.
	Agents []string
	// Land says the project is a tenant land (its land_kind is not operator).
	// The route sets it from the farm, never from a request param. It adds the
	// set's LandOnly panels and is refused on a fleet set.
	Land bool
}

// Card is one panel as the route returns it. Src and OpenURL are what the
// garden uses (it builds the iframe with createElement/setAttribute, never
// innerHTML). EmbedHTML is the same markup the viewer surfaces emit for these
// inputs, kept for the golden; the garden does not inject it. With no Grafana
// base URL, Src and OpenURL are "" and EmbedHTML is the Unconfigured card.
type Card struct {
	ID        int    `json:"id"`
	Title     string `json:"title"`
	Scope     string `json:"scope"`
	Note      string `json:"note"`
	Src       string `json:"src"`
	OpenURL   string `json:"open_url"`
	EmbedHTML string `json:"embed_html"`
}

// Result is a whole set built for one request: the normalised inputs, the
// whole-dashboard Open URL ("" when unconfigured) and one Card per panel.
type Result struct {
	Set              string   `json:"set"`
	Project          string   `json:"project"`
	Range            string   `json:"range"`
	Theme            string   `json:"theme"`
	Agents           []string `json:"agents,omitempty"`
	OpenDashboardURL string   `json:"open_dashboard_url"`
	Panels           []Card   `json:"panels"`
}

// Build validates q against the closed sets and builds every panel in its set
// against base (beadsViewer.grafanaBaseURL; "" degrades every card). Any input
// outside the sets returns an error wrapping ErrInvalid and no Result.
func Build(base string, q Query) (Result, error) { return build(sets, base, q) }

func build(reg []Set, base string, q Query) (Result, error) {
	set, ok := lookup(reg, q.Set)
	if !ok {
		return Result{}, fmt.Errorf("%w: set %q", ErrInvalid, q.Set)
	}
	rng, err := ParseRange(q.Range)
	if err != nil {
		return Result{}, err
	}
	theme, err := ParseTheme(q.Theme)
	if err != nil {
		return Result{}, err
	}
	var vars []Var
	switch {
	case set.PerProject && !ValidProject(q.Project):
		return Result{}, fmt.Errorf("%w: project %q for set %s", ErrInvalid, q.Project, set.Name)
	case !set.PerProject && q.Project != "":
		return Result{}, fmt.Errorf("%w: set %s is fleet-wide and takes no project", ErrInvalid, set.Name)
	case !set.PerProject && q.Land:
		return Result{}, fmt.Errorf("%w: set %s is fleet-wide and takes no land", ErrInvalid, set.Name)
	case !set.PerAgent && len(q.Agents) > 0:
		return Result{}, fmt.Errorf("%w: set %s takes no agent", ErrInvalid, set.Name)
	case set.PerAgent:
		if len(q.Agents) == 0 || len(q.Agents) > AgentsMax {
			return Result{}, fmt.Errorf("%w: set %s takes 1 to %d agents, got %d", ErrInvalid, set.Name, AgentsMax, len(q.Agents))
		}
		for _, a := range q.Agents {
			if !ValidAgent(a) {
				return Result{}, fmt.Errorf("%w: agent %q for set %s", ErrInvalid, a, set.Name)
			}
		}
		vars = []Var{{Name: "agent", Val: strings.Join(q.Agents, "|")}}
	case set.ProjectVar && set.PerProject:
		vars = []Var{{Name: "project", Val: q.Project}}
	case set.ProjectVar:
		vars = []Var{{Name: "project", Val: allProjects}}
	}

	base = strings.TrimRight(base, "/")
	opts := Opts{
		Height:   strconv.Itoa(CardHeight),
		Vars:     vars,
		Theme:    string(theme),
		From:     rng.From,
		To:       rng.To,
		Timezone: Timezone,
	}
	res := Result{
		Set:     set.Name,
		Project: q.Project,
		Range:   rng.Name,
		Theme:   string(theme),
		Agents:  q.Agents,
		Panels:  make([]Card, 0, len(set.Panels)),
	}
	_, res.OpenDashboardURL = URLs(base, set.UID, opts)
	for _, p := range set.Panels {
		if p.LandOnly && !q.Land {
			continue
		}
		po := opts
		po.PanelID = strconv.Itoa(p.ID)
		src, openURL := URLs(base, set.UID, po)
		res.Panels = append(res.Panels, Card{
			ID:        p.ID,
			Title:     p.Title,
			Scope:     p.Scope,
			Note:      p.Note,
			Src:       src,
			OpenURL:   openURL,
			EmbedHTML: HTML(base, set.UID, po),
		})
	}
	return res, nil
}
