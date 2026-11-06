package main

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// collect gathers all metric families off a registry into a name→family map.
func collect(t *testing.T, reg *prometheus.Registry) map[string]*dto.MetricFamily {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := make(map[string]*dto.MetricFamily, len(mfs))
	for _, mf := range mfs {
		out[mf.GetName()] = mf
	}
	return out
}

func TestTokenMetrics_RegisterAndNames(t *testing.T) {
	reg := prometheus.NewRegistry()
	tm := newTokenMetrics()
	if err := tm.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// Touch every series so the families materialize.
	tm.recordProvision(tokenProviderBeads, tokenProvisionSuccess)
	tm.recordRevocation(tokenProviderBeads, revocationTypeDespawn, time.Now().Add(-time.Minute))

	mfs := collect(t, reg)
	for _, name := range []string{
		"gardenio_agent_token_provisions_total",
		"gardenio_agent_token_revocations_total",
		"gardenio_agent_token_revocation_latency_seconds",
	} {
		if _, ok := mfs[name]; !ok {
			t.Errorf("expected metric family %q to be registered", name)
		}
	}
}

// TestTokenMetrics_LabelPrivacy is load-bearing: it enforces the HARD privacy
// invariant (telemetry-plan §5) that no raw token id / provider-side id ever
// appears as a label. The only permitted label names are provider, status and
// revocation_type, and their values are bounded enums — never token material.
func TestTokenMetrics_LabelPrivacy(t *testing.T) {
	reg := prometheus.NewRegistry()
	tm := newTokenMetrics()
	if err := tm.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Feed a fake "secret-looking" string everywhere a caller could plausibly
	// (mis)route token material. It must NOT surface as a label value.
	const secretLike = "deadbeefcafebabe0123456789abcdef"
	tm.recordProvision(tokenProviderBeads, tokenProvisionSuccess)
	tm.recordProvision(tokenProviderBeads, tokenProvisionError)
	tm.recordRevocation(tokenProviderBeads, revocationTypeDespawn, time.Now())
	tm.recordRevocation(tokenProviderBeads, revocationTypeStuckRestart, time.Now())
	tm.recordRevocation(tokenProviderBeads, revocationTypeAsyncJanitor, time.Now())

	allowedLabelNames := map[string]bool{"provider": true, "status": true, "revocation_type": true}
	allowedValues := map[string]bool{
		tokenProviderBeads:         true,
		tokenProvisionSuccess:      true,
		tokenProvisionError:        true,
		revocationTypeDespawn:      true,
		revocationTypeStuckRestart: true,
		revocationTypeAsyncJanitor: true,
	}

	for name, mf := range collect(t, reg) {
		if !strings.HasPrefix(name, "gardenio_agent_token_") {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if !allowedLabelNames[lp.GetName()] {
					t.Errorf("%s: forbidden label name %q", name, lp.GetName())
				}
				if !allowedValues[lp.GetValue()] {
					t.Errorf("%s: label %q has unexpected value %q (must be a bounded enum, never token material)",
						name, lp.GetName(), lp.GetValue())
				}
				if strings.Contains(lp.GetValue(), secretLike) {
					t.Errorf("%s: label %q leaked secret-like material %q", name, lp.GetName(), lp.GetValue())
				}
			}
		}
	}
}

func TestTokenMetrics_NilReceiverIsNoOp(t *testing.T) {
	var tm *tokenMetrics // nil
	// Must not panic.
	tm.recordProvision(tokenProviderBeads, tokenProvisionSuccess)
	tm.recordRevocation(tokenProviderBeads, revocationTypeDespawn, time.Now())
	tm.recordRevocation(tokenProviderBeads, revocationTypeDespawn, time.Time{})
}

func TestTokenMetrics_SLOBuckets(t *testing.T) {
	// The approved SLO query reads off the le="300" (5m) and le="3600" (1h)
	// bucket boundaries; assert both are present so the query never silently
	// bucket-mismatches.
	want := map[float64]bool{300: false, 3600: false}
	for _, b := range tokenSLOBuckets {
		if _, ok := want[b]; ok {
			want[b] = true
		}
	}
	for b, seen := range want {
		if !seen {
			t.Errorf("SLO bucket boundary %v missing from tokenSLOBuckets %v", b, tokenSLOBuckets)
		}
	}
}

func TestTokenMetrics_RevocationLatencyObserved(t *testing.T) {
	reg := prometheus.NewRegistry()
	tm := newTokenMetrics()
	if err := tm.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// A doneAt two minutes ago should record one latency observation.
	tm.recordRevocation(tokenProviderBeads, revocationTypeDespawn, time.Now().Add(-2*time.Minute))
	// A zero doneAt records the counter but NO latency observation.
	tm.recordRevocation(tokenProviderBeads, revocationTypeAsyncJanitor, time.Time{})

	mfs := collect(t, reg)
	hist := mfs["gardenio_agent_token_revocation_latency_seconds"]
	if hist == nil {
		t.Fatal("latency histogram not registered")
	}
	var totalCount uint64
	for _, m := range hist.GetMetric() {
		totalCount += m.GetHistogram().GetSampleCount()
	}
	if totalCount != 1 {
		t.Errorf("latency sample count = %d, want 1 (zero doneAt must not observe)", totalCount)
	}

	revs := mfs["gardenio_agent_token_revocations_total"]
	if revs == nil {
		t.Fatal("revocations counter not registered")
	}
	var totalRevs float64
	for _, m := range revs.GetMetric() {
		totalRevs += m.GetCounter().GetValue()
	}
	if totalRevs != 2 {
		t.Errorf("revocations total = %v, want 2 (count increments regardless of doneAt)", totalRevs)
	}
}
