package gardenhub

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"gardenio/controller/internal/grafanaembed"
)

// The garden's Grafana charts (plan gd-gh5J0vVbJP step 1.2, gd-j0IzQNgwkB;
// gardenio/docs/garden-grafana-panels.md, "Route shape"): GET
// /garden/charts/{set}?project=&range=&theme= answers one registry set's panel
// cards as JSON, built by grafanaembed.Build from closed allowlists only. The
// garden builds each iframe from a card's src with createElement/setAttribute
// and never injects embed_html, which stays for the cross-surface golden.
//
// The route is routeAdminOnly in the viewer (route_classes.go), as /costs*,
// /grafana* and /charts* are: grafana.clonk.tv runs with defaultOrgRole Admin
// behind the /gardenio-admins edge gate, and costs-project lets an admin pick
// any project. A tenant-scoped set is a new route, not a reclassification.

// chartsPanel serves GET /garden/charts/{set}. An unknown set is a 404 and any
// other input outside its closed set a 400, never a fallback URL. With no
// Grafana base URL every card degrades (no src, the Unconfigured note) and the
// answer is still a 200, so the garden shows the cards' words. A project set
// for a tenant land (its farm cost row carries a purse, farm_cost.go) adds the
// land-only panels; that comes from the farm's project bead, never a param.
//
// The per-agent sets (step 4.2, gd-g7Ms8Ddfmj) name their agents two ways: the
// agent set takes one, agent=<name>, from the agent face, which the builder
// checks is shaped as a name; the gardener set takes gardener=<id> and reads
// that gardener loop's runs on the request's bearer, as its panel does, so the
// names come from the daemon, never from the request. A gardener with no run
// yet answers no cards.
func (h *Handler) chartsPanel(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("set")
	set, ok := grafanaembed.LookupSet(name)
	if !ok {
		http.Error(w, "no such chart set", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	project := q.Get("project")
	land := false
	if set.PerProject && grafanaembed.ValidProject(project) && h.Farm != nil {
		if known, listed := h.Farm.KnowsProject(project); listed && !known {
			http.Error(w, "unknown project", http.StatusBadRequest)
			return
		}
		land = h.Farm.IsTenantLand(project)
	}
	var agents []string
	switch set.Name {
	case "agent":
		agents = []string{q.Get("agent")}
	case "gardener":
		var status int
		if agents, status = h.gardenerAgents(r.Context(), q.Get("gardener")); status != http.StatusOK {
			http.Error(w, http.StatusText(status), status)
			return
		}
		if len(agents) == 0 {
			h.noCharts(w, set.Name, q.Get("range"), q.Get("theme"))
			return
		}
	}
	res, err := grafanaembed.Build(h.GrafanaBaseURL, grafanaembed.Query{
		Set:     set.Name,
		Project: project,
		Range:   q.Get("range"),
		Theme:   q.Get("theme"),
		Agents:  agents,
		Land:    land,
	})
	if errors.Is(err, grafanaembed.ErrInvalid) {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, "chart set did not build", http.StatusInternalServerError)
		return
	}
	writeDoc(w, "charts", res)
}

// gardenerAgents reads the agents a gardener loop's runs were, newest first, as its panel lists them (up to
// gardenerRunsMax, a spawn that never ran left out, each once), and a status: 400 for a bad id, 404 for one the viewer
// may not see, 502 when the daemon did not answer or its runs did not read (Partial "runs"), 503 with no gardener reader.
func (h *Handler) gardenerAgents(ctx context.Context, id string) ([]string, int) {
	id = strings.TrimSpace(id)
	if id == "" || len(id) > 200 {
		return nil, http.StatusBadRequest
	}
	if h.Gardeners == nil {
		return nil, http.StatusServiceUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, gardenerTimeout)
	defer cancel()
	doc, err := h.Gardeners.Gardener(ctx, id)
	var ae *beadsapi.APIError
	switch {
	case err == nil && doc == nil, errors.As(err, &ae) && (ae.StatusCode == http.StatusNotFound || ae.StatusCode == http.StatusForbidden):
		return nil, http.StatusNotFound
	case err != nil, slices.Contains(doc.Partial, "runs"): // a failed runs read is not a gardener that never ran
		return nil, http.StatusBadGateway
	}
	seen, out := map[string]bool{}, []string{}
	for _, run := range doc.Runs {
		if run.DidNotRun || run.Name == "" || seen[run.Name] || !grafanaembed.ValidAgent(run.Name) {
			continue
		}
		seen[run.Name] = true
		if out = append(out, run.Name); len(out) == min(gardenerRunsMax, grafanaembed.AgentsMax) {
			break
		}
	}
	return out, http.StatusOK
}

// noCharts answers a set with nothing to chart yet (a gardener with no run): its normalised inputs and no cards, or a
// 400 for a range or theme outside the chips, as Build would.
func (h *Handler) noCharts(w http.ResponseWriter, set, rng, theme string) {
	r, err := grafanaembed.ParseRange(rng)
	if err == nil {
		var th grafanaembed.Theme
		if th, err = grafanaembed.ParseTheme(theme); err == nil {
			theme = string(th)
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeDoc(w, "charts", grafanaembed.Result{Set: set, Range: r.Name, Theme: theme, Panels: []grafanaembed.Card{}})
}
