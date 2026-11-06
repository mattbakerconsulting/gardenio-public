package tests

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Fleet-wide datasource-binding gate (bug gd-rwCcRV3G2f, epic ka-0000008066).
//
// 177 targets across 11 of the 26 chart-shipped boards carried no `datasource`
// of their own. Grafana resolves such a target against the PANEL's datasource,
// and only falls through to the org default when the panel has none either — so
// the fleet worked, but only by accident of every panel happening to be bound.
// Two edits break that silently, with no error anywhere:
//
//   - a panel switched to "-- Mixed --" (the moment a second datasource joins a
//     board, which is exactly what the VictoriaLogs boards did), at which point
//     every unbound target on it drops to the org default, and
//   - a panel-level `datasource` dropped in a hand edit, which sends its targets
//     to the org default with the panel still rendering *something*.
//
// A query pointed at the wrong datasource does not error — it returns empty, and
// an empty panel is indistinguishable from a healthy quiet one. That is the same
// display-lie class the grounded-zero sweep (ka-0000006531) was opened for, so
// it gets the same treatment: bind it everywhere, then pin it fleet-wide.
//
// The per-board tests DO assert target-level binding, but only on the boards
// that have such a test — 15 of 26 boards had no coverage at all. These asserts
// are structural and apply to EVERY shipped board, including any added later,
// so no allowlist/baseline is needed: the tree is clean as of this commit.
//
// Guarded here, for every top-level dashboards/*.json (the glob the
// grafana-bridge image bakes in; dashboards/_parked/ is deliberately excluded):
//
//  1. every query-bearing panel declares a datasource,
//  2. every target on it declares its own, non-empty datasource,
//  3. a target's datasource matches its panel's, unless the panel is mixed
//     (a mismatch under a non-mixed panel is dead config: Grafana ignores the
//     target binding and runs the panel's, so the two disagree on paper), and
//  4. every ${DS_*} reference names a datasource template variable that the
//     board actually defines (an unresolved one reads as a literal uid and
//     matches nothing).
//
// Rows and text panels are exempt: they carry no queries.

// dsRef is the shape a Grafana datasource reference takes on a panel, a target,
// an annotation or a template variable.
type dsRef struct {
	Type string `json:"type"`
	UID  string `json:"uid"`
}

func (d *dsRef) empty() bool { return d == nil || strings.TrimSpace(d.UID) == "" }

func (d *dsRef) String() string {
	if d == nil {
		return "<none>"
	}
	return fmt.Sprintf("{type:%q uid:%q}", d.Type, d.UID)
}

// mixed reports whether the panel delegates datasource choice to its targets.
func (d *dsRef) mixed() bool {
	return d != nil && (strings.Contains(strings.ToLower(d.UID), "mixed") ||
		strings.EqualFold(d.Type, "datasource"))
}

type dsPanel struct {
	Type       string    `json:"type"`
	Title      string    `json:"title"`
	Datasource *dsRef    `json:"datasource"`
	Targets    []dsTgt   `json:"targets"`
	Panels     []dsPanel `json:"panels"` // nested panels of a collapsed row
}

type dsTgt struct {
	RefID      string `json:"refId"`
	Datasource *dsRef `json:"datasource"`
}

type dsBoard struct {
	UID         string    `json:"uid"`
	Title       string    `json:"title"`
	Panels      []dsPanel `json:"panels"`
	Annotations struct {
		List []struct {
			Name       string `json:"name"`
			BuiltIn    int    `json:"builtIn"`
			Datasource *dsRef `json:"datasource"`
		} `json:"list"`
	} `json:"annotations"`
	Templating struct {
		List []struct {
			Name       string `json:"name"`
			Type       string `json:"type"`
			Datasource *dsRef `json:"datasource"`
		} `json:"list"`
	} `json:"templating"`
}

// dsShippedBoards parses every board the grafana-bridge image bakes in, keyed by
// file name. Mirrors shippedDashboards' anti-vacuous guard: a moved or renamed
// dashboards/ directory must fail loudly rather than silently pass zero boards.
func dsShippedBoards(t *testing.T) map[string]dsBoard {
	t.Helper()
	dir := filepath.Join(chartDir(t), "dashboards")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	if len(files) < 20 {
		t.Fatalf("only %d dashboards matched %s/*.json — expected the full fleet (>=20); did the directory move?", len(files), dir)
	}
	out := make(map[string]dsBoard, len(files))
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		var b dsBoard
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatalf("%s is not valid JSON: %v", filepath.Base(path), err)
		}
		out[filepath.Base(path)] = b
	}
	return out
}

// queryPanels flattens a board's panel tree, dropping the two types that carry
// no queries (rows and text). Nested panels live under a collapsed row.
func queryPanels(panels []dsPanel) []dsPanel {
	var out []dsPanel
	for _, p := range panels {
		if len(p.Panels) > 0 {
			out = append(out, queryPanels(p.Panels)...)
		}
		switch p.Type {
		case "row", "text":
			continue
		}
		out = append(out, p)
	}
	return out
}

func TestEveryDashboardTargetDeclaresItsDatasource(t *testing.T) {
	t.Parallel()
	boards := dsShippedBoards(t)

	// Anti-vacuous: the sweep must actually see targets. A shape change that made
	// queryPanels return nothing would otherwise pass silently.
	var seen int
	for file, b := range boards {
		t.Run(file, func(t *testing.T) {
			for _, p := range queryPanels(b.Panels) {
				if len(p.Targets) == 0 {
					continue
				}
				// (1) The panel is the fallback for anything a target does not
				// bind, so it must itself be bound.
				if p.Datasource.empty() {
					t.Errorf("%s panel %q (%s) declares no datasource, so its targets resolve against Grafana's org default", file, p.Title, p.Type)
				}
				for i, tg := range p.Targets {
					id := tg.RefID
					if id == "" {
						id = fmt.Sprintf("#%d", i)
					}
					// (2) Bind every target explicitly.
					if tg.Datasource.empty() {
						t.Errorf("%s panel %q target %s declares no datasource — it silently inherits the panel's (%s) today and Grafana's org default the moment that panel goes mixed or loses its binding",
							file, p.Title, id, p.Datasource)
						continue
					}
					// (3) Under a non-mixed panel, a differing target binding is
					// dead config: Grafana runs the panel's datasource anyway.
					if !p.Datasource.mixed() && !p.Datasource.empty() &&
						(tg.Datasource.UID != p.Datasource.UID || !strings.EqualFold(tg.Datasource.Type, p.Datasource.Type)) {
						t.Errorf("%s panel %q target %s binds %s but the panel binds %s and is not mixed — Grafana runs the panel's, so this target's binding is dead config",
							file, p.Title, id, tg.Datasource, p.Datasource)
					}
					seen++
				}
			}
		})
	}
	if seen < 300 {
		t.Errorf("only %d targets were inspected across %d boards — expected the whole fleet (>=300); did the panel/target shape change?", seen, len(boards))
	}
}

// TestEveryDashboardBindsAnnotationsAndQueryVariables extends the same rule to
// the two other query surfaces on a board. Both are green fleet-wide today; the
// gate keeps a new board from reintroducing an unbound one, which fails the same
// silent way — an annotation or variable query pointed at the org default just
// comes back empty.
func TestEveryDashboardBindsAnnotationsAndQueryVariables(t *testing.T) {
	t.Parallel()
	for file, b := range dsShippedBoards(t) {
		t.Run(file, func(t *testing.T) {
			for _, a := range b.Annotations.List {
				// builtIn=1 is Grafana's own "Annotations & Alerts" entry.
				if a.BuiltIn == 1 {
					continue
				}
				if a.Datasource.empty() {
					t.Errorf("%s annotation %q declares no datasource", file, a.Name)
				}
			}
			for _, v := range b.Templating.List {
				if v.Type != "query" {
					continue
				}
				if v.Datasource.empty() {
					t.Errorf("%s template variable %q is a query variable with no datasource", file, v.Name)
				}
			}
		})
	}
}

var dsVarRef = regexp.MustCompile(`\$\{(DS_[A-Za-z0-9_]+)\}`)

// TestEveryDatasourceVariableReferenceResolves catches the other half of the
// binding bug: a target CAN carry a datasource and still query nothing, if its
// uid is a ${DS_*} placeholder the board never defines. Grafana does not
// substitute an undefined variable — the literal string becomes the uid and
// matches no datasource, so the panel renders empty with no error.
func TestEveryDatasourceVariableReferenceResolves(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(chartDir(t), "dashboards")
	for file, b := range dsShippedBoards(t) {
		t.Run(file, func(t *testing.T) {
			defined := make(map[string]bool, len(b.Templating.List))
			for _, v := range b.Templating.List {
				defined[v.Name] = true
			}
			raw, err := os.ReadFile(filepath.Join(dir, file))
			if err != nil {
				t.Fatalf("read %s: %v", file, err)
			}
			for _, m := range dsVarRef.FindAllStringSubmatch(string(raw), -1) {
				if !defined[m[1]] {
					t.Errorf("%s references datasource variable ${%s} but defines no such template variable — the placeholder is used verbatim as a uid and matches nothing", file, m[1])
				}
			}
		})
	}
}
