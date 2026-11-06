package server

import (
	"context"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

// authzDenialsAcrossPrincipals is a collector reporting one value: the sum of
// gardenio_authz_denials_total over every principal_kind for the given action,
// reason and bead_type. Tests that measure a per-principal delta on one
// (action, reason, bead_type) read it through testutil.ToFloat64 as they read
// a single series before principal_kind existed.
func authzDenialsAcrossPrincipals(action, reason, beadType string) prometheus.Collector {
	return denialSum{action: action, reason: reason, beadType: beadType}
}

type denialSum struct{ action, reason, beadType string }

var denialSumDesc = prometheus.NewDesc("authz_denials_across_principals", "test sum", nil, nil)

func (d denialSum) Describe(ch chan<- *prometheus.Desc) { ch <- denialSumDesc }

func (d denialSum) Collect(ch chan<- prometheus.Metric) {
	metrics := make(chan prometheus.Metric, 64)
	go func() {
		authzDenialsTotal.Collect(metrics)
		close(metrics)
	}()
	var total float64
	for m := range metrics {
		var pb dto.Metric
		if err := m.Write(&pb); err != nil {
			continue
		}
		if metricLabel(&pb, "action") == d.action && metricLabel(&pb, "reason") == d.reason && metricLabel(&pb, "bead_type") == d.beadType {
			total += pb.GetCounter().GetValue()
		}
	}
	ch <- prometheus.MustNewConstMetric(denialSumDesc, prometheus.CounterValue, total)
}

func TestDeriveAuthzDenialReason(t *testing.T) {
	cases := []struct {
		name string
		d    *AuthzDenial
		want string
	}{
		{
			name: "nil denial -> denied",
			d:    nil,
			want: reasonDenied,
		},
		{
			name: "min_role branch -> insufficient_role",
			d:    &AuthzDenial{Action: "create", BeadType: "task", RequiredRole: "admin"},
			want: reasonInsufficientRole,
		},
		{
			name: "project scope branch -> project_mismatch",
			d:    &AuthzDenial{Action: "create", BeadType: "task", RequiredScope: "gardenio", Escalatable: true},
			want: reasonProjectMismatch,
		},
		{
			name: "token project mismatch message -> project_mismatch",
			d:    &AuthzDenial{Action: "create", BeadType: "task", Message: "token not authorized for this bead's project"},
			want: reasonProjectMismatch,
		},
		{
			name: "ownership/escalatable branch -> escalation_required",
			d:    &AuthzDenial{Action: "update", BeadType: "task", Escalatable: true, Message: "ownership denied"},
			want: reasonEscalationRequired,
		},
		{
			name: "token scope (no role/scope/escalatable) -> denied",
			d:    &AuthzDenial{Action: "close", BeadType: "task", Message: "token scope denied: operation close not permitted"},
			want: reasonDenied,
		},
	}

	allowed := map[string]struct{}{
		reasonInsufficientRole:   {},
		reasonProjectMismatch:    {},
		reasonEscalationRequired: {},
		reasonDenied:             {},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := deriveAuthzDenialReason(tc.d)
			if got != tc.want {
				t.Fatalf("deriveAuthzDenialReason = %q, want %q", got, tc.want)
			}
			if _, ok := allowed[got]; !ok {
				t.Fatalf("derived reason %q is outside the bounded enum", got)
			}
		})
	}
}

func TestAuthzBeadTypeLabelCap(t *testing.T) {
	// Allowlisted types pass through verbatim.
	for _, bt := range []string{"task", "epic", "agent", "audit", "decision", "escalation"} {
		if got := authzBeadTypeLabel(bt); got != bt {
			t.Errorf("authzBeadTypeLabel(%q) = %q, want passthrough", bt, got)
		}
	}

	// Unknown / user-defined types collapse to "custom", capping cardinality.
	for _, bt := range []string{"", "wibble", "my-custom-type", "workflow", "message"} {
		if got := authzBeadTypeLabel(bt); got != "custom" {
			t.Errorf("authzBeadTypeLabel(%q) = %q, want \"custom\"", bt, got)
		}
	}
}

func TestRecordAuthzDenialEmitsCounter(t *testing.T) {
	// Use an isolated registry so we observe only this metric.
	reg := prometheus.NewRegistry()
	authzDenialsTotal.Reset()
	if err := reg.Register(authzDenialsTotal); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx := asOIDC(context.Background(), "someone@example.com")
	recordAuthzDenial(ctx, &AuthzDenial{Action: "create", BeadType: "task", RequiredRole: "admin"})
	recordAuthzDenial(ctx, &AuthzDenial{Action: "create", BeadType: "totally-custom", RequiredRole: "admin"})
	recordAuthzDenial(ctx, nil) // must be a no-op, not a panic.

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	var found *dto.MetricFamily
	for _, mf := range mfs {
		if mf.GetName() == "gardenio_authz_denials_total" {
			found = mf
			break
		}
	}
	if found == nil {
		t.Fatal("gardenio_authz_denials_total not registered/emitted")
	}

	got := map[string]float64{}
	for _, m := range found.GetMetric() {
		labels := map[string]string{}
		for _, lp := range m.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		key := strings.Join([]string{labels["action"], labels["reason"], labels["bead_type"], labels["principal_kind"]}, "|")
		got[key] = m.GetCounter().GetValue()
	}

	if v := got["create|insufficient_role|task|human"]; v != 1 {
		t.Errorf("create|insufficient_role|task|human = %v, want 1 (got %v)", v, got)
	}
	// Overflow type must have been capped to "custom".
	if v := got["create|insufficient_role|custom|human"]; v != 1 {
		t.Errorf("create|insufficient_role|custom|human = %v, want 1 (bead_type cap)", v)
	}
	if _, ok := got["create|insufficient_role|totally-custom|human"]; ok {
		t.Error("uncapped bead_type leaked into the counter labels")
	}
}

// The principal_kind label names the kind of caller refused, and a machine
// principal by its registered client, so a denial loop like gd-Hk70sS1XuS's
// (machine:gardenio-controller stamping plan epics) is attributable from the
// metric alone. Unknown identity types collapse to "other".
func TestAuthzPrincipalKind(t *testing.T) {
	agentCtx := WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "gardenio-crew-1"}), "agent-gd-abc")
	bridgeCtx := WithFleet(WithAuthIdentity(context.Background(), AuthIdentity{Type: "fleet", Subject: "bridge:slack"}), "slack-bridge")
	cases := []struct {
		name string
		ctx  context.Context
		want string
	}{
		{"machine names its client", machineCtx("gardenio-controller", "gardenio-operator"), "machine:gardenio-controller"},
		{"machine without a subject", WithAuthIdentity(context.Background(), AuthIdentity{Type: "machine"}), "machine"},
		{"oidc person", asOIDC(context.Background(), "someone@example.com"), "human"},
		{"per-agent token", agentCtx, "agent"},
		{"bridge fleet token", bridgeCtx, "fleet"},
		{"static token", WithAuthIdentity(context.Background(), AuthIdentity{Type: "static"}), "static"},
		{"no identity", context.Background(), "anonymous"},
		{"unknown type", WithAuthIdentity(context.Background(), AuthIdentity{Type: "new-kind", Subject: "x"}), principalKindOther},
	}
	for _, c := range cases {
		if got := authzPrincipalKind(c.ctx); got != c.want {
			t.Errorf("%s: authzPrincipalKind = %q, want %q", c.name, got, c.want)
		}
	}
}

// recordAuthzDenial carries principal_kind onto the series.
func TestRecordAuthzDenial_PrincipalKindLabel(t *testing.T) {
	authzDenialsTotal.Reset()
	d := &AuthzDenial{Action: "update", BeadType: "epic", Escalatable: true}
	recordAuthzDenial(machineCtx("gardenio-controller", "gardenio-operator"), d)
	got := testutil.ToFloat64(authzDenialsTotal.WithLabelValues("update", reasonEscalationRequired, "epic", "machine:gardenio-controller"))
	if got != 1 {
		t.Fatalf("denials{update,escalation_required,epic,machine:gardenio-controller} = %v, want 1", got)
	}
}
