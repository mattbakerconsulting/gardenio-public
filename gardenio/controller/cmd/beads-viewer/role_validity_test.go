package main

import "testing"

// TestBuildRoleValidityReport_Clean: a fully valid role bead yields no findings
// (green badge).
func TestBuildRoleValidityReport_Clean(t *testing.T) {
	fields := map[string]string{
		"image":         "registry/agent:crew",
		"secrets":       `[{"name":"FOO","secret":"k8s-foo","key":"foo"}]`,
		"env":           `[{"name":"BAR","value":"baz"}]`,
		"env_json":      `{"X":"1"}`,
		"claude_config": `{"model":"opus"}`,
		"cpu_limit":     "2",
		"memory_limit":  "4Gi",
	}
	r := buildRoleValidityReport(fields, nil)
	if !r.OK() {
		t.Errorf("expected clean report, got %+v", r.Checks)
	}
	if r.Badge() != "green" {
		t.Errorf("Badge = %q, want green", r.Badge())
	}
}

// TestBuildRoleValidityReport_MalformedJSON: a JSON field that doesn't decode
// into its Go type is flagged as an error (silently dropped by the controller).
func TestBuildRoleValidityReport_MalformedJSON(t *testing.T) {
	cases := []struct{ field, raw string }{
		{"secrets", `{"name":"FOO"}`},      // object, want array
		{"env", `"not-an-array"`},          // string, want array
		{"env_json", `["a","b"]`},          // array, want map
		{"claude_config", `[1,2,3]`},       // array, want map
	}
	for _, tc := range cases {
		r := buildRoleValidityReport(map[string]string{tc.field: tc.raw}, nil)
		if r.ErrorCount() != 1 {
			t.Errorf("%s=%q: ErrorCount = %d, want 1\nchecks=%+v", tc.field, tc.raw, r.ErrorCount(), r.Checks)
			continue
		}
		if r.Checks[0].Field != tc.field {
			t.Errorf("%s: finding Field = %q, want %q", tc.field, r.Checks[0].Field, tc.field)
		}
		if r.Badge() != "red" {
			t.Errorf("%s: Badge = %q, want red", tc.field, r.Badge())
		}
	}
}

// TestBuildRoleValidityReport_BadQuantity: an invalid K8s resource quantity is
// flagged.
func TestBuildRoleValidityReport_BadQuantity(t *testing.T) {
	r := buildRoleValidityReport(map[string]string{"cpu_limit": "two-cores"}, nil)
	if r.ErrorCount() != 1 || r.Checks[0].Field != "cpu_limit" {
		t.Errorf("expected one cpu_limit error, got %+v", r.Checks)
	}
}

// TestBuildRoleValidityReport_AccountRefs: a dangling account / account_whitelist
// reference is flagged when the known-accounts set is provided; skipped when nil.
func TestBuildRoleValidityReport_AccountRefs(t *testing.T) {
	fields := map[string]string{
		"account":           "ghost",
		"account_whitelist": `["live-one","ghost-two"]`,
	}
	known := map[string]struct{}{"live-one": {}}

	// With the known set: account "ghost" + whitelist "ghost-two" both dangle.
	r := buildRoleValidityReport(fields, known)
	if r.ErrorCount() != 2 {
		t.Errorf("ErrorCount = %d, want 2 (ghost account + ghost-two whitelist)\nchecks=%+v", r.ErrorCount(), r.Checks)
	}

	// Nil known set: account checks skipped entirely.
	r2 := buildRoleValidityReport(fields, nil)
	if !r2.OK() {
		t.Errorf("expected no findings when knownAccounts is nil, got %+v", r2.Checks)
	}
}

// TestBuildRoleValidityReport_Deterministic: findings are ordered by field then
// message, regardless of input map iteration order.
func TestBuildRoleValidityReport_Deterministic(t *testing.T) {
	fields := map[string]string{
		"secrets":      `{}`,
		"cpu_limit":    "bad",
		"memory_limit": "bad",
	}
	r := buildRoleValidityReport(fields, nil)
	for i := 1; i < len(r.Checks); i++ {
		if r.Checks[i-1].Field > r.Checks[i].Field {
			t.Errorf("findings not field-sorted: %+v", r.Checks)
			break
		}
	}
}

// TestParseRoleAccountList covers both the JSON-array and comma-separated
// shapes the account_whitelist field accepts.
func TestParseRoleAccountList(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{`["a","b"]`, []string{"a", "b"}},
		{"a, b ,c", []string{"a", "b", "c"}},
		{"", nil},
		{`[" x "]`, []string{"x"}},
		{`[bad json`, []string{"[bad json"}}, // malformed JSON → comma-split fallback
	}
	for _, tc := range cases {
		got := parseRoleAccountList(tc.raw)
		if len(got) != len(tc.want) {
			t.Errorf("parseRoleAccountList(%q) = %v, want %v", tc.raw, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("parseRoleAccountList(%q)[%d] = %q, want %q", tc.raw, i, got[i], tc.want[i])
			}
		}
	}
}
