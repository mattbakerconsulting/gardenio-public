package server

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

func TestRecordAuthzLegacyFallthroughEmitsCounter(t *testing.T) {
	// Use an isolated registry so we observe only this metric.
	reg := prometheus.NewRegistry()
	authzLegacyFallthroughTotal.Reset()
	if err := reg.Register(authzLegacyFallthroughTotal); err != nil {
		t.Fatalf("register: %v", err)
	}

	recordAuthzLegacyFallthrough("update", "task")
	recordAuthzLegacyFallthrough("close", "task")
	recordAuthzLegacyFallthrough("update", "task")
	// Unknown/user-defined type must be capped to "custom".
	recordAuthzLegacyFallthrough("delete", "totally-custom")

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}

	var found *dto.MetricFamily
	for _, mf := range mfs {
		if mf.GetName() == "gardenio_authz_legacy_fallthrough_total" {
			found = mf
			break
		}
	}
	if found == nil {
		t.Fatal("gardenio_authz_legacy_fallthrough_total not registered/emitted")
	}

	got := map[string]float64{}
	for _, m := range found.GetMetric() {
		labels := map[string]string{}
		for _, lp := range m.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		key := strings.Join([]string{labels["action"], labels["bead_type"]}, "|")
		got[key] = m.GetCounter().GetValue()
	}

	if v := got["update|task"]; v != 2 {
		t.Errorf("update|task = %v, want 2", v)
	}
	if v := got["close|task"]; v != 1 {
		t.Errorf("close|task = %v, want 1", v)
	}
	// Overflow type must have been capped to "custom".
	if v := got["delete|custom"]; v != 1 {
		t.Errorf("delete|custom = %v, want 1 (bead_type cap)", v)
	}
	if _, ok := got["delete|totally-custom"]; ok {
		t.Error("uncapped bead_type leaked into the counter labels")
	}
}
