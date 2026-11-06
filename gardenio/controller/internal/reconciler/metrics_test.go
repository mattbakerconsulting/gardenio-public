package reconciler

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestDeferredMetrics_RegisterSucceeds(t *testing.T) {
	m := NewDeferredMetrics()
	reg := prometheus.NewRegistry()
	if err := m.Register(reg); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Registering the same metrics twice must fail (Prometheus detects
	// duplicate collectors) — this validates DeferredTotal was registered.
	if err := m.Register(reg); err == nil {
		t.Fatal("expected duplicate-register error, got nil")
	}
}

func TestDeferredMetrics_RegisterReturnsErrorOnConflict(t *testing.T) {
	m := NewDeferredMetrics()

	// Create a registry that already contains a conflicting collector name
	// so DeferredTotal registration fails on the first Register call.
	reg := prometheus.NewRegistry()
	conflict := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: metricsNamespace,
		Name:      "agents_deferred_total",
		Help:      "conflict",
	}, []string{"project", "reason"})
	if err := reg.Register(conflict); err != nil {
		t.Fatalf("seed register: %v", err)
	}

	if err := m.Register(reg); err == nil {
		t.Fatal("expected conflict error from DeferredTotal register")
	}
}

func TestDeferredMetrics_RegisterWaitSecondsConflict(t *testing.T) {
	m := NewDeferredMetrics()
	reg := prometheus.NewRegistry()

	// Seed only the wait-seconds collector to force the second branch to fail.
	conflict := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: metricsNamespace,
		Name:      "agent_deferred_wait_seconds",
		Help:      "conflict",
	}, []string{"project"})
	if err := reg.Register(conflict); err != nil {
		t.Fatalf("seed register: %v", err)
	}

	if err := m.Register(reg); err == nil {
		t.Fatal("expected conflict error from DeferredWaitSeconds register")
	}
}
