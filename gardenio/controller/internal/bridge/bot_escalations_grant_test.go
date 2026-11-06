package bridge

import "testing"

// TestGrantEscalationSummary pins the card line that names a grant's
// principal (gd-URd8GBIXvL): an approver must see who gets the taint grant
// without opening the Web UI.
func TestGrantEscalationSummary(t *testing.T) {
	cases := []struct {
		name, op, spec, want string
	}{
		{"agent principal", "grant", `{"taint_names":["untrusted-input"],"principal_kind":"agent","principal":"gd-abc"}`,
			"*Grants* `untrusted-input` *to* `agent:gd-abc`"},
		{"fleet principal", "grant", `{"taint_names":["a","b"],"principal_kind":"fleet","principal":"gardenio"}`,
			"*Grants* `a, b` *to* `fleet:gardenio`"},
		{"backticks and angle brackets neutralised", "grant", "{\"taint_names\":[\"x`y\"],\"principal_kind\":\"agent\",\"principal\":\"<!here>\"}",
			"*Grants* `x'y` *to* `agent:&lt;!here&gt;`"},
		{"not a grant", "helm_deploy", `{"principal_kind":"agent","principal":"gd-abc"}`, ""},
		{"no principal", "grant", `{"taint_names":["x"]}`, ""},
		{"bad spec", "grant", `not json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := grantEscalationSummary(tc.op, tc.spec); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}
