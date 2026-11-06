package main

import (
	"net/http"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestForwardauth403Reason_BoundedMapping(t *testing.T) {
	cases := []struct {
		detail string
		want   string
	}{
		{auditDetailDenyNotInGroup, reasonNotInGroup},
		{auditDetailDenyEmailDomain, reasonEmailDomain},
		{auditDetailDenyEntraForbidden, reasonEntraForbidden},
		// Details that are 403-adjacent in the audit vocabulary but never a 403
		// still map to the bounded catch-all rather than leaking as a label.
		{auditDetailDenyInvalidPAT, reasonOther},
		{auditDetailDenyMissingToken, reasonOther},
		{"deny:some-future-reason", reasonOther},
		{"", reasonOther},
	}
	for _, tc := range cases {
		if got := forwardauth403Reason(tc.detail); got != tc.want {
			t.Errorf("forwardauth403Reason(%q)=%q, want %q", tc.detail, got, tc.want)
		}
	}
}

func TestForwardauth403RouteTemplate_BoundedAllowlist(t *testing.T) {
	cases := []struct {
		routeKey string
		want     string
	}{
		{auditEndpointAuthCheck, "auth_check"},
		{auditEndpointForwardauth, "forwardauth"},
		{auditEndpointEntraForwardauth, "entra_forwardauth"},
		{"kbeads", "kbeads"},
		{"coopmux", "coopmux"},
		{"nats", "nats"},
		// Anything off the allowlist — an unexpected upstream, a raw path — must
		// collapse to "other" so the label cannot grow unbounded.
		{"some-new-upstream", routeTemplateOther},
		{"/kd/v1/beads/gd-abc123", routeTemplateOther},
		{"", routeTemplateOther},
	}
	for _, tc := range cases {
		if got := forwardauth403RouteTemplate(tc.routeKey); got != tc.want {
			t.Errorf("forwardauth403RouteTemplate(%q)=%q, want %q", tc.routeKey, got, tc.want)
		}
	}

	// The allowlist must stay under the <=15 ceiling the telemetry plan sets.
	// Probe the known keys plus a spread of unknowns and assert the produced
	// set of label values never exceeds the cap.
	seen := map[string]struct{}{}
	for _, k := range []string{
		auditEndpointAuthCheck, auditEndpointForwardauth, auditEndpointEntraForwardauth,
		"kbeads", "coopmux", "nats",
		"x1", "x2", "x3", "x4", "x5", "/raw/url", "another",
	} {
		seen[forwardauth403RouteTemplate(k)] = struct{}{}
	}
	if len(seen) > 15 {
		t.Fatalf("route_template produced %d distinct values, want <=15", len(seen))
	}
}

func TestForwardauth403Counter_EmitsBoundedLabelsOnProxy403(t *testing.T) {
	base, _, fx := newProxyTestServerWithCapture(t, "test-hmac-key")
	defer fx.close()

	// A valid PAT for a principal that is NOT in the gardenio group → 403.
	req, _ := http.NewRequest("GET", base+"/kd/v1/beads", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer notin-group-pat")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status=%d, want 403", resp.StatusCode)
	}

	// The counter must have incremented for the bounded (reason, route_template)
	// pair — reason=not_in_group derived from the deny detail, route_template=
	// kbeads derived from the matched upstream (never the raw URL).
	if got := testutil.ToFloat64(fx.s.metrics.forwardauth403.WithLabelValues(reasonNotInGroup, "kbeads")); got != 1 {
		t.Errorf("forwardauth_403_total{reason=not_in_group,route_template=kbeads}=%v, want 1", got)
	}

	// Total across all label sets is exactly 1 (no stray emit on other labels).
	if n := testutil.CollectAndCount(fx.s.metrics.forwardauth403); n != 1 {
		t.Errorf("distinct label series=%d, want 1", n)
	}
}

// TestForwardauth403Counter_NoOperatorLabel is the guardrail for the plan's
// "No operator label" contract: the metric must carry EXACTLY reason and
// route_template, never a per-operator/per-email dimension.
func TestForwardauth403Counter_NoOperatorLabel(t *testing.T) {
	base, _, fx := newProxyTestServerWithCapture(t, "test-hmac-key")
	defer fx.close()

	req, _ := http.NewRequest("GET", base+"/kd/v1/beads", strings.NewReader(""))
	req.Header.Set("Authorization", "Bearer notin-group-pat")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	fams, err := fx.s.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, fam := range fams {
		if fam.GetName() != forwardauth403MetricName {
			continue
		}
		found = true
		for _, mtr := range fam.GetMetric() {
			names := map[string]struct{}{}
			for _, lp := range mtr.GetLabel() {
				names[lp.GetName()] = struct{}{}
			}
			if len(names) != 2 {
				t.Errorf("metric has %d labels %v, want exactly 2 (reason, route_template)", len(names), names)
			}
			if _, ok := names["reason"]; !ok {
				t.Error("missing reason label")
			}
			if _, ok := names["route_template"]; !ok {
				t.Error("missing route_template label")
			}
			for _, forbidden := range []string{"operator", "operator_id", "email", "email_addr", "user", "actor"} {
				if _, ok := names[forbidden]; ok {
					t.Errorf("forbidden operator-attributing label %q present on %s", forbidden, forwardauth403MetricName)
				}
			}
		}
	}
	if !found {
		t.Fatalf("metric %s not registered", forwardauth403MetricName)
	}
}
