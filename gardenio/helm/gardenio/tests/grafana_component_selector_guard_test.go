package tests

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Fleet-wide guard that every `component="..."` selector on a shipped board
// names a component the chart actually renders (bug gd-tfR3nEQkZ6).
//
// WHY THIS EXISTS. gd-tfR3nEQkZ6 reported the webhook-liveness dark panels as
// dark because they pin on `component=`, "a label no scraped series carries".
// That report was itself a measurement error — it read the WRONG VictoriaMetrics
// instance (see the datasource note in
// grafana_webhook_liveness_dashboard_test.go) — but it exposed a real gap it
// named directly: the existing pin guards are string-containment asserts, and a
// string-containment assert cannot tell a correct selector from one that matches
// nothing. Both the reported bug AND its proposed `job="gardenio-jira-bridge"`
// fix would have sailed past those asserts.
//
// This closes that gap hermetically, without a live query. The `component` label
// is stamped by the chart's OWN vmagent, which relabels it from each pod's
// `app.kubernetes.io/component` label (templates/vmagent/configmap.yaml). So the
// set of values a dashboard may legally select on is exactly the set of
// `app.kubernetes.io/component` literals the chart's templates emit — both sides
// live in this repo, and a selector naming anything else can only ever match the
// empty set.
//
// Deliberately NOT covered: `component!=""` (a presence filter, not a value) and
// Grafana template variables like `${exporter_component}` (resolved at render
// time from a datasource query, not from chart templates).

// componentSelector matches a PromQL label matcher on `component` and captures
// the operator and the quoted value.
var componentSelector = regexp.MustCompile(`component\s*(=~|!~|=|!=)\s*"([^"]*)"`)

// componentLabelLiteral matches the `app.kubernetes.io/component:` label
// assignments the chart's templates emit.
var componentLabelLiteral = regexp.MustCompile(`app\.kubernetes\.io/component:\s*"?([A-Za-z0-9._-]+)"?`)

// regexMeta reports whether a `=~`/`!~` alternative is a pattern rather than a
// plain literal. Patterns are skipped: they are matched against the live label
// set, not enumerated here.
func regexMeta(s string) bool {
	return strings.ContainsAny(s, `.*+?()[]{}^$\`)
}

// chartComponentValues returns every literal `app.kubernetes.io/component` value
// the chart's templates assign. Values containing Helm actions ({{ ... }}) are
// skipped — they are not statically resolvable.
func chartComponentValues(t *testing.T) map[string]bool {
	t.Helper()
	root := filepath.Join(chartDir(t), "templates")
	out := map[string]bool{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.Contains(line, "{{") {
				continue
			}
			for _, m := range componentLabelLiteral.FindAllStringSubmatch(line, -1) {
				out[m[1]] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	// Anti-vacuous guard: a moved templates/ dir must fail loudly rather than
	// silently accept every selector.
	if len(out) < 40 {
		t.Fatalf("only %d app.kubernetes.io/component values found under %s — expected the full chart (>=40); did the directory move?", len(out), root)
	}
	return out
}

// dashboardExprs recursively collects every "expr" string in a dashboard's JSON.
// Recursion (rather than a fixed panels[].targets[] walk) is required: exprs
// also live in annotations[].list[] and in row-nested panels[].panels[].
func dashboardExprs(v interface{}, out *[]string) {
	switch t := v.(type) {
	case map[string]interface{}:
		for k, val := range t {
			if k == "expr" {
				if s, ok := val.(string); ok {
					*out = append(*out, s)
				}
			}
			dashboardExprs(val, out)
		}
	case []interface{}:
		for _, val := range t {
			dashboardExprs(val, out)
		}
	}
}

func TestDashboardComponentSelectorsNameRealChartComponents(t *testing.T) {
	t.Parallel()
	valid := chartComponentValues(t)

	dir := filepath.Join(chartDir(t), "dashboards")
	files, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		t.Fatalf("glob %s: %v", dir, err)
	}
	if len(files) < 20 {
		t.Fatalf("only %d dashboards matched %s/*.json — expected the full fleet (>=20); did the directory move?", len(files), dir)
	}

	checked := 0
	for _, path := range files {
		file := filepath.Base(path)
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		var doc interface{}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s is not valid JSON: %v", file, err)
		}
		var exprs []string
		dashboardExprs(doc, &exprs)

		t.Run(file, func(t *testing.T) {
			for _, expr := range exprs {
				for _, m := range componentSelector.FindAllStringSubmatch(expr, -1) {
					op, val := m[1], m[2]
					for _, alt := range strings.Split(val, "|") {
						alt = strings.TrimSpace(alt)
						// Presence filters and template variables are not values.
						if alt == "" || strings.Contains(alt, "$") {
							continue
						}
						if (op == "=~" || op == "!~") && regexMeta(alt) {
							continue
						}
						checked++
						if !valid[alt] {
							t.Errorf("component%s%q selects %q, which no chart template assigns as app.kubernetes.io/component — this matches the empty set and the panel renders dark.\n  expr: %s\n  known components: %s",
								op, val, alt, expr, strings.Join(sortedKeys(valid), ", "))
						}
					}
				}
			}
		})
	}

	// Anti-vacuous guard: if the selector regex ever stops matching, this test
	// would pass by checking nothing.
	if checked < 10 {
		t.Errorf("only %d component selectors were checked fleet-wide — expected >=10; the selector regex has likely gone stale", checked)
	}
}

func sortedKeys(m map[string]bool) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	if len(ks) > 20 {
		return append(ks[:20:20], fmt.Sprintf("... (%d more)", len(ks)-20))
	}
	return ks
}
