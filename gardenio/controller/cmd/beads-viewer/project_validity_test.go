package main

import (
	"strings"
	"testing"
)

// accountSet is a tiny helper to build the knownAccounts set the validator
// takes for dangling-reference resolution.
func accountSet(names ...string) map[string]struct{} {
	s := make(map[string]struct{}, len(names))
	for _, n := range names {
		s[n] = struct{}{}
	}
	return s
}

// findCheck returns the first check on the given field, or false.
func findCheck(r projectValidityReport, field string) (projectValidityCheck, bool) {
	for _, c := range r.Checks {
		if c.Field == field {
			return c, true
		}
	}
	return projectValidityCheck{}, false
}

func TestProjectValidityReport_Valid(t *testing.T) {
	// A fully-populated, well-formed config bead. Every JSON field parses into
	// its expected Go struct, every quantity is a valid K8s quantity, and the
	// account reference resolves. Expect a clean, green report.
	fields := map[string]string{
		"prefix":                "kd",
		"git_url":               "https://github.com/org/repo",
		"account":               "gardenio",
		"cpu_request":           "500m",
		"cpu_limit":             "2",
		"memory_request":        "512Mi",
		"memory_limit":          "2Gi",
		"secrets":               `[{"env":"FOO","secret":"my-secret","key":"foo"}]`,
		"env":                   `[{"name":"BAR","value":"baz"}]`,
		"repos":                 `[{"url":"https://github.com/org/lib","branch":"main","role":"reference"}]`,
		"resource_mapping":      `[{"env":"prod","cluster":"c","service":"s","db_instance":"db"}]`,
		"prewarmed_pool":        `{"enabled":true,"min_size":2,"max_size":5,"role":"thread","mode":"crew"}`,
		"channel_roles":         `{"C123":"reviewer"}`,
		"channel_modes":         `{"C123":"concierge"}`,
		"channel_runtime_class": `{"C123":"runsc"}`,
		"claude_config":         `{"claude-settings":{"theme":"dark"}}`,
		"env_json":              `{"KEY":"val"}`,
	}
	r := buildProjectValidityReport(fields, accountSet("gardenio", "other"))
	if !r.OK() {
		t.Fatalf("expected valid config to produce no findings, got %+v", r.Checks)
	}
	if r.Badge() != "green" {
		t.Errorf("badge: got %q, want green", r.Badge())
	}
	if r.ErrorCount() != 0 || r.WarningCount() != 0 {
		t.Errorf("counts: got %d errors / %d warnings, want 0/0", r.ErrorCount(), r.WarningCount())
	}
}

func TestProjectValidityReport_EmptyIsValid(t *testing.T) {
	// A minimal/empty config bead: optional fields absent. Nothing to flag.
	r := buildProjectValidityReport(map[string]string{"prefix": "kd"}, accountSet("gardenio"))
	if !r.OK() {
		t.Fatalf("expected empty config to be valid, got %+v", r.Checks)
	}
	if r.Badge() != "green" {
		t.Errorf("badge: got %q, want green", r.Badge())
	}
}

func TestProjectValidityReport_MalformedJSON(t *testing.T) {
	tests := []struct {
		name  string
		field string
		raw   string
	}{
		{"secrets not array", "secrets", `{"env":"FOO"}`},                 // object where []SecretEntry expected
		{"secrets truncated", "secrets", `[{"env":"FOO"`},                 // truncated JSON
		{"repos wrong type", "repos", `"not-an-array"`},                   // string where []RepoEntry expected
		{"prewarmed_pool array", "prewarmed_pool", `[1,2,3]`},             // array where struct expected
		{"resource_mapping garbage", "resource_mapping", `not json`},      // not JSON at all
		{"channel_roles array", "channel_roles", `["a","b"]`},             // array where map expected
		{"env_json wrong type", "env_json", `[1,2]`},                      // array where map[string]string expected
		{"env entry bad", "env", `[{"name":123}]`},                        // wrong scalar type inside entry
		{"secrets trailing data", "secrets", `[{"env":"FOO"}] extrajunk`}, // trailing garbage
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := buildProjectValidityReport(map[string]string{tc.field: tc.raw}, nil)
			c, ok := findCheck(r, tc.field)
			if !ok {
				t.Fatalf("expected a finding on field %q, got none (%+v)", tc.field, r.Checks)
			}
			if c.Severity != severityError {
				t.Errorf("severity: got %q, want error", c.Severity)
			}
			if !strings.Contains(c.Message, "malformed JSON") {
				t.Errorf("message: got %q, want it to mention malformed JSON", c.Message)
			}
			if r.Badge() != "red" {
				t.Errorf("badge: got %q, want red", r.Badge())
			}
		})
	}
}

func TestProjectValidityReport_UnknownKeysTolerated(t *testing.T) {
	// The controller uses lax json.Unmarshal and tolerates unknown keys, so a
	// valid-shape value with an extra key MUST NOT be flagged — flagging it
	// would red a config that actually works. This guards against the validator
	// drifting stricter than the controller it mirrors.
	fields := map[string]string{
		"secrets": `[{"env":"FOO","secret":"s","key":"k","extra":"tolerated"}]`,
		"repos":   `[{"url":"u","branch":"b","unknown_field":true}]`,
	}
	r := buildProjectValidityReport(fields, nil)
	if !r.OK() {
		t.Fatalf("unknown keys should be tolerated, got findings: %+v", r.Checks)
	}
}

func TestProjectValidityReport_BadQuantity(t *testing.T) {
	tests := []struct {
		name  string
		field string
		raw   string
	}{
		{"cpu_request garbage", "cpu_request", "lots"},
		{"cpu_limit bad suffix", "cpu_limit", "2x"},
		{"memory_request empty unit", "memory_request", "512MiB"}, // MiB is not a valid K8s suffix (Mi is)
		{"memory_limit nonsense", "memory_limit", "two-gigs"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := buildProjectValidityReport(map[string]string{tc.field: tc.raw}, nil)
			c, ok := findCheck(r, tc.field)
			if !ok {
				t.Fatalf("expected a finding on field %q for value %q, got none (%+v)", tc.field, tc.raw, r.Checks)
			}
			if c.Severity != severityError {
				t.Errorf("severity: got %q, want error", c.Severity)
			}
			if !strings.Contains(c.Message, "not a valid Kubernetes quantity") {
				t.Errorf("message: got %q, want quantity error", c.Message)
			}
			if r.Badge() != "red" {
				t.Errorf("badge: got %q, want red", r.Badge())
			}
		})
	}
}

func TestProjectValidityReport_GoodQuantities(t *testing.T) {
	// Valid K8s quantity strings must not be flagged.
	for _, q := range []string{"500m", "2", "2000m", "512Mi", "2Gi", "1.5", "100M"} {
		fields := map[string]string{"cpu_limit": q}
		r := buildProjectValidityReport(fields, nil)
		if !r.OK() {
			t.Errorf("quantity %q should be valid, got findings: %+v", q, r.Checks)
		}
	}
}

func TestProjectValidityReport_DanglingReference(t *testing.T) {
	// account references a name not present in the known-accounts set.
	fields := map[string]string{"account": "ghost-account"}
	r := buildProjectValidityReport(fields, accountSet("gardenio", "claude-max"))
	c, ok := findCheck(r, "account")
	if !ok {
		t.Fatalf("expected a dangling-reference finding on account, got none (%+v)", r.Checks)
	}
	if c.Severity != severityError {
		t.Errorf("severity: got %q, want error", c.Severity)
	}
	if !strings.Contains(c.Message, "ghost-account") || !strings.Contains(c.Message, "does not resolve") {
		t.Errorf("message: got %q, want it to name the dangling account", c.Message)
	}
	if r.Badge() != "red" {
		t.Errorf("badge: got %q, want red", r.Badge())
	}
}

func TestProjectValidityReport_ResolvedReference(t *testing.T) {
	// account resolves cleanly — no finding.
	fields := map[string]string{"account": "gardenio"}
	r := buildProjectValidityReport(fields, accountSet("gardenio"))
	if !r.OK() {
		t.Fatalf("resolved account should produce no findings, got %+v", r.Checks)
	}
}

func TestProjectValidityReport_NilAccountsSkipsReferenceCheck(t *testing.T) {
	// When the account list could not be fetched (nil), the reference check is
	// skipped entirely — we don't flag what we can't verify. Other checks still
	// run, so a malformed JSON field is still caught.
	fields := map[string]string{
		"account": "anything",
		"secrets": `garbage`,
	}
	r := buildProjectValidityReport(fields, nil)
	if _, ok := findCheck(r, "account"); ok {
		t.Errorf("account reference check must be skipped when knownAccounts is nil")
	}
	if _, ok := findCheck(r, "secrets"); !ok {
		t.Errorf("other checks must still run when knownAccounts is nil")
	}
}

func TestProjectValidityReport_EmptyAccountsTreatsAllAsDangling(t *testing.T) {
	// A non-nil but empty set means "we fetched accounts and there are none" —
	// any reference is therefore dangling. (Distinct from nil = "couldn't
	// fetch".)
	fields := map[string]string{"account": "gardenio"}
	r := buildProjectValidityReport(fields, accountSet())
	if _, ok := findCheck(r, "account"); !ok {
		t.Errorf("empty (non-nil) account set should flag any reference as dangling")
	}
}

func TestProjectValidityReport_MultipleFindingsSorted(t *testing.T) {
	// Several problems at once: deterministic ordering by field, then message.
	fields := map[string]string{
		"account":      "ghost",
		"cpu_limit":    "bad",
		"secrets":      `garbage`,
		"memory_limit": "alsobad",
	}
	r := buildProjectValidityReport(fields, accountSet("real"))
	if got := len(r.Checks); got != 4 {
		t.Fatalf("expected 4 findings, got %d (%+v)", got, r.Checks)
	}
	// Verify the slice is sorted by field ascending.
	for i := 1; i < len(r.Checks); i++ {
		if r.Checks[i-1].Field > r.Checks[i].Field {
			t.Errorf("findings not sorted by field: %q before %q", r.Checks[i-1].Field, r.Checks[i].Field)
		}
	}
	if r.ErrorCount() != 4 {
		t.Errorf("ErrorCount: got %d, want 4", r.ErrorCount())
	}
	if r.Badge() != "red" {
		t.Errorf("badge: got %q, want red", r.Badge())
	}
}

func TestProjectValidityReport_WarningOnlyYieldsYellow(t *testing.T) {
	// Synthetic: a warning-severity finding with no errors yields a yellow
	// badge. (No builder path emits warnings today, so we exercise the Badge
	// classification directly to lock the green/yellow/red contract.)
	r := projectValidityReport{Checks: []projectValidityCheck{
		{Field: "x", Severity: severityWarning, Message: "heads up"},
	}}
	if r.Badge() != "yellow" {
		t.Errorf("badge: got %q, want yellow", r.Badge())
	}
	if r.ErrorCount() != 0 {
		t.Errorf("ErrorCount: got %d, want 0", r.ErrorCount())
	}
	if r.WarningCount() != 1 {
		t.Errorf("WarningCount: got %d, want 1", r.WarningCount())
	}
	if r.OK() {
		t.Errorf("OK: got true, want false (a warning is still a finding)")
	}
}

// TestProjectValidityReport_Budget pins the budget_usd / budget_period checks
// (decision gd-PtNuTyjQoN): blank is "no budget", a set budget must be a finite
// amount above $0, and the period must be month or 7d.
func TestProjectValidityReport_Budget(t *testing.T) {
	tests := []struct {
		name      string
		fields    map[string]string
		wantField string // "" = no finding
		wantMsg   string
	}{
		{"no budget", map[string]string{}, "", ""},
		{"whole dollars", map[string]string{"budget_usd": "250"}, "", ""},
		{"cents, 7d", map[string]string{"budget_usd": "99.50", "budget_period": "7d"}, "", ""},
		{"month", map[string]string{"budget_usd": "1000", "budget_period": "month"}, "", ""},
		{"zero", map[string]string{"budget_usd": "0"}, "budget_usd", "greater than $0"},
		{"zero decimal", map[string]string{"budget_usd": "0.00"}, "budget_usd", "greater than $0"},
		{"negative", map[string]string{"budget_usd": "-5"}, "budget_usd", "greater than $0"},
		{"dollar sign", map[string]string{"budget_usd": "$250"}, "budget_usd", "not a dollar amount"},
		{"words", map[string]string{"budget_usd": "lots"}, "budget_usd", "not a dollar amount"},
		{"NaN", map[string]string{"budget_usd": "NaN"}, "budget_usd", "not a dollar amount"},
		{"Inf", map[string]string{"budget_usd": "Inf"}, "budget_usd", "not a dollar amount"},
		{"bad period", map[string]string{"budget_usd": "250", "budget_period": "week"}, "budget_period", "not a valid budget period"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := buildProjectValidityReport(tc.fields, nil)
			if tc.wantField == "" {
				if !r.OK() {
					t.Fatalf("expected no findings, got %+v", r.Checks)
				}
				return
			}
			c, ok := findCheck(r, tc.wantField)
			if !ok {
				t.Fatalf("expected a %s finding, got %+v", tc.wantField, r.Checks)
			}
			if c.Severity != severityError {
				t.Errorf("severity: got %q, want error", c.Severity)
			}
			if !strings.Contains(c.Message, tc.wantMsg) {
				t.Errorf("message %q does not contain %q", c.Message, tc.wantMsg)
			}
		})
	}
}
