package grafanagrade

import (
	"reflect"
	"sort"
	"testing"
)

func sorted(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestMetricNames(t *testing.T) {
	cases := []struct {
		expr string
		want []string
	}{
		{"sum(gardenio_agents_deferred_total)", []string{"gardenio_agents_deferred_total"}},
		{"max(up{job=~\".*vpa-recommender.*\"})", []string{"up"}},
		// Label matchers name label keys and values, not metrics.
		{"sum(rate(a_total{pod=~\"crew-.*\", up=\"1\"}[5m]))", []string{"a_total"}},
		// A range selector's duration is not an identifier.
		{"sum(increase(b_total[15m])) > 0", []string{"b_total"}},
		// Grafana variables are substituted at render time.
		{"sum(c_total{ns=\"$namespace\"}) or vector($default)", []string{"c_total"}},
		// Aggregation keywords and modifiers are not metrics.
		{"sum by (project) (topk(5, d_total)) unless on (project) e_total",
			[]string{"d_total", "e_total"}},
		// Numeric literals and operators contribute nothing.
		{"clamp_min(sum(f_total), 1) / 60", []string{"f_total"}},
	}
	for _, tc := range cases {
		if got := sorted(metricNames(tc.expr)); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("metricNames(%q) = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestOrSites_FindsNestedSites(t *testing.T) {
	// A coalesce nested inside a ratio is exactly where a masked zero hides,
	// so a top-level-only split would miss the defect it was written for.
	expr := "(sum(a_total) or vector(0)) / clamp_min(sum(b_total), 1)"
	sites := orSites(expr)
	if len(sites) != 1 {
		t.Fatalf("want 1 or-site, got %d: %+v", len(sites), sites)
	}
	if !isConstantBranch(sites[0].RHS) {
		t.Errorf("RHS %q should read as a constant branch", sites[0].RHS)
	}

	// An `or` inside a string literal or a label matcher is not an operator.
	for _, expr := range []string{
		`sum(a_total{msg="deferred or blocked"})`,
		`sum(a_total{reason=~"or_thing"})`,
		`sum(corridor_total)`,
	} {
		if got := orSites(expr); len(got) != 0 {
			t.Errorf("orSites(%q) = %+v, want none", expr, got)
		}
	}
}

func TestIsConstantBranch(t *testing.T) {
	constant := []string{
		"vector(0)", " vector(0) ", "(vector(0))", "vector(1)", "vector(0.0)",
		"vector(-1)", "on() vector(0)", "on (project) vector(0)",
	}
	for _, s := range constant {
		if !isConstantBranch(s) {
			t.Errorf("isConstantBranch(%q) = false, want true", s)
		}
	}
	notConstant := []string{
		"vector(0) and (count(up) > 0)",
		"0 * (sum(increase(a_total[15m])) > 0)",
		"sum(b_total)",
		"",
	}
	for _, s := range notConstant {
		if isConstantBranch(s) {
			t.Errorf("isConstantBranch(%q) = true, want false", s)
		}
	}
}

func TestSplitTopLevel_RespectsDepthAndWordBoundaries(t *testing.T) {
	got := splitTopLevel("sum(a or b) or vector(0)", "or")
	want := []string{"sum(a or b) ", " vector(0)"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("splitTopLevel = %q, want %q", got, want)
	}
	// "corridor" and "order" must not be mistaken for the operator.
	if got := splitTopLevel("sum(corridor_total)", "or"); len(got) != 1 {
		t.Fatalf("word-boundary split failed: %q", got)
	}
}

func TestGateOperands(t *testing.T) {
	// Both the RHS of an `or` and the operands of a top-level `and` can carry
	// the liveness term, so both must be offered to the gate check.
	got := gateOperands("sum(a_total) or (vector(0) and (count(up) > 0))")
	if len(got) == 0 {
		t.Fatal("want at least the or-RHS")
	}
	found := false
	for _, g := range got {
		if metricNames(g)["up"] {
			found = true
		}
	}
	if !found {
		t.Fatalf("no operand carried the control metric: %q", got)
	}
}

func TestStripLiterals(t *testing.T) {
	// Blanking must preserve length so or-site offsets stay valid against the
	// original expression.
	in := `sum(a_total{pod=~"crew-.*"}[5m]) or vector(0)`
	out := stripLiterals(in)
	if len(out) != len(in) {
		t.Fatalf("stripLiterals changed length: %d != %d", len(out), len(in))
	}
	if metricNames(in)["crew"] {
		t.Fatal("a label-matcher value leaked into metric names")
	}
}
