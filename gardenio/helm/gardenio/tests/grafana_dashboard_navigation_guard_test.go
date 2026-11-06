package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fleet-wide navigation + deploy-annotation guard (bug gd-fN1YKBjedJ).
//
// GRADE measured Fleet Depth at 6.65/10 against a bar of 8.0: 20 of 26 boards
// had ZERO panel links and ZERO dashboard links, so every spike dead-ended
// (agent deaths climbing on fleet-lifecycle had no path to involuntary-deaths),
// and there were ZERO custom annotation queries fleet-wide, so a step change
// could never be tied to a deploy without leaving Grafana.
//
// These asserts are structural and apply to EVERY shipped board, so a new
// dashboard cannot land back in the dead-end state:
//   - carries the "gardenio" tag (the tag the fleet dropdown selects on),
//   - carries the "All gardenio boards" tag dropdown link,
//   - carries at least one explicit sibling link, pointing at a uid that
//     actually exists in the fleet,
//   - spells every internal href "/grafana/d/..." (Grafana is served from the
//     /grafana subpath; a bare "/d/..." only works via a 301 redirect hop),
//   - carries the shared "gardenio rollout" annotation, enabled, whose window is
//     [$__interval] and NOT a fixed window.
//
// The [$__interval] assert is the load-bearing one and is not cosmetic. Grafana
// evaluates an annotation query at its own step (~range/100, floored by "step"),
// which at a 24h range is ~14m. A fixed [5m] window therefore leaves most
// events BETWEEN evaluation points and silently drops them: measured against
// live VictoriaMetrics on 2026-09-02, [5m] rendered 2 of 11 real rollouts over
// 24h while [$__interval] rendered all 11.
const (
	fleetTag           = "gardenio"
	fleetDropdownTitle = "All gardenio boards"
	rolloutAnnName     = "gardenio rollout"
	dashboardHrefBase  = "/grafana/d/"
)

type navDashboard struct {
	UID   string   `json:"uid"`
	Title string   `json:"title"`
	Tags  []string `json:"tags"`
	Links []struct {
		Title      string   `json:"title"`
		Type       string   `json:"type"`
		URL        string   `json:"url"`
		Tags       []string `json:"tags"`
		AsDropdown bool     `json:"asDropdown"`
	} `json:"links"`
	Annotations struct {
		List []struct {
			Name    string `json:"name"`
			Enable  bool   `json:"enable"`
			Expr    string `json:"expr"`
			BuiltIn int    `json:"builtIn"`
		} `json:"list"`
	} `json:"annotations"`
}

// shippedDashboards parses every dashboard the grafana-bridge image bakes in —
// the top-level dashboards/*.json glob, which deliberately excludes
// dashboards/_parked/. Keyed by file name.
func shippedDashboards(t *testing.T) map[string]navDashboard {
	t.Helper()
	dir := filepath.Join(chartDir(t), "dashboards")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	// Anti-vacuous guard: a moved/renamed dashboards/ directory must fail this
	// test loudly rather than silently pass zero boards.
	if len(files) < 20 {
		t.Fatalf("only %d dashboards matched %s/*.json — expected the full fleet (>=20); did the directory move?", len(files), dir)
	}
	out := make(map[string]navDashboard, len(files))
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var d navDashboard
		if err := json.Unmarshal(raw, &d); err != nil {
			t.Fatalf("%s is not valid JSON: %v", filepath.Base(path), err)
		}
		if d.UID == "" {
			t.Fatalf("%s has no uid — links to it cannot be built", filepath.Base(path))
		}
		out[filepath.Base(path)] = d
	}
	return out
}

func TestEveryDashboardIsReachableFromEveryOther(t *testing.T) {
	t.Parallel()
	boards := shippedDashboards(t)

	uids := make(map[string]string, len(boards))
	for file, d := range boards {
		uids[d.UID] = file
	}

	for file, d := range boards {
		t.Run(file, func(t *testing.T) {
			// The fleet dropdown is tag-based, so a board without the tag is
			// invisible from every other board's dropdown.
			hasTag := false
			for _, tag := range d.Tags {
				if tag == fleetTag {
					hasTag = true
					break
				}
			}
			if !hasTag {
				t.Errorf("%s (%s) is missing the %q tag, so it never appears in the fleet dropdown; tags=%v", file, d.UID, fleetTag, d.Tags)
			}

			var dropdown, siblings int
			for _, l := range d.Links {
				switch l.Type {
				case "dashboards":
					hasFleetTag := false
					for _, tag := range l.Tags {
						if tag == fleetTag {
							hasFleetTag = true
						}
					}
					if l.Title == fleetDropdownTitle && hasFleetTag && l.AsDropdown {
						dropdown++
					}
				case "link":
					if !strings.HasPrefix(l.URL, dashboardHrefBase) {
						// External links are fine; only guard internal ones.
						if strings.HasPrefix(l.URL, "/d/") {
							t.Errorf("%s link %q url %q must start with %q (Grafana serves from the /grafana subpath)", file, l.Title, l.URL, dashboardHrefBase)
						}
						continue
					}
					siblings++
					target := strings.TrimPrefix(l.URL, dashboardHrefBase)
					if i := strings.IndexAny(target, "/?"); i >= 0 {
						target = target[:i]
					}
					if _, ok := uids[target]; !ok {
						t.Errorf("%s link %q points at uid %q, which no shipped dashboard declares — dead link", file, l.Title, target)
					}
				}
			}
			if dropdown == 0 {
				t.Errorf("%s has no %q tag dropdown link — an operator on this board cannot reach the rest of the fleet", file, fleetDropdownTitle)
			}
			if siblings == 0 {
				t.Errorf("%s has no explicit sibling dashboard link — a spike on this board dead-ends (bug gd-fN1YKBjedJ)", file)
			}
		})
	}
}

func TestEveryDashboardCarriesTheRolloutAnnotation(t *testing.T) {
	t.Parallel()
	for file, d := range shippedDashboards(t) {
		t.Run(file, func(t *testing.T) {
			var found bool
			for _, a := range d.Annotations.List {
				if a.Name != rolloutAnnName {
					continue
				}
				found = true
				if !a.Enable {
					t.Errorf("%s annotation %q is disabled — deploys stay invisible on this board", file, rolloutAnnName)
				}
				if !strings.Contains(a.Expr, "[$__interval]") {
					t.Errorf("%s annotation %q must use a [$__interval] range-vector window, got expr %q; a fixed window is narrower than Grafana's annotation step at wide ranges and silently drops most rollouts", file, rolloutAnnName, a.Expr)
				}
				if !strings.Contains(a.Expr, "process_start_time_seconds") {
					t.Errorf("%s annotation %q lost its process_start_time_seconds signature: %q", file, rolloutAnnName, a.Expr)
				}
			}
			if !found {
				names := make([]string, 0, len(d.Annotations.List))
				for _, a := range d.Annotations.List {
					names = append(names, a.Name)
				}
				t.Errorf("%s has no %q annotation — a step change on this board cannot be tied to a deploy (bug gd-fN1YKBjedJ); annotations present: %v", file, rolloutAnnName, names)
			}
		})
	}
}
