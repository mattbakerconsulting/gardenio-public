package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestRecordWarmPlaceholderCycled(t *testing.T) {
	pm := NewPoolMetrics("e2e")
	reg := prometheus.NewRegistry()
	if err := pm.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}

	pm.RecordWarmPlaceholderCycled("node-a", "topo-1", 2)
	pm.RecordWarmPlaceholderCycled("node-a", "topo-1", 1)  // same series accumulates
	pm.RecordWarmPlaceholderCycled("node-b", "", 3)        // empty topo -> "legacy"
	pm.RecordWarmPlaceholderCycled("node-c", "topo-2", 0)  // no-op
	pm.RecordWarmPlaceholderCycled("node-c", "topo-2", -5) // no-op

	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	series := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "gardenio_warm_placeholders_cycled_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			v := m.GetCounter().GetValue()
			total += v
			if !hasLabel(m, "env", "e2e") {
				t.Errorf("series missing env=e2e label: %v", m.GetLabel())
			}
			var node, topo string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "node":
					node = l.GetValue()
				case "topology":
					topo = l.GetValue()
				}
			}
			series[node+"/"+topo] = v
		}
	}

	if total != 6 {
		t.Errorf("total cycled = %v, want 6 (2+1+3, the 0 and -5 dropped)", total)
	}
	if series["node-a/topo-1"] != 3 {
		t.Errorf("node-a/topo-1 = %v, want 3", series["node-a/topo-1"])
	}
	if series["node-b/legacy"] != 3 {
		t.Errorf("node-b/legacy = %v, want 3 (empty topology collapses to legacy)", series["node-b/legacy"])
	}
	if _, ok := series["node-c/topo-2"]; ok {
		t.Error("node-c/topo-2 should have no series (only no-op calls)")
	}
}

func TestPoolMetrics_NilReceiver(t *testing.T) {
	var pm *PoolMetrics
	// All methods must be safe on a nil receiver (pool disabled).
	if err := pm.Register(prometheus.NewRegistry()); err != nil {
		t.Errorf("nil Register: %v", err)
	}
	pm.RecordWarmPlaceholderCycled("n", "t", 1) // must not panic
	if pm.RecordWarmPlaceholderClaim("hit", "n", "t") {
		t.Error("nil RecordWarmPlaceholderClaim should return false")
	}
	if pm.RecordPrewarmedAssign("hit", "p", "r") {
		t.Error("nil RecordPrewarmedAssign should return false")
	}
	if pm.RecordPrewarmedAssignDuration(0.1, "hit", "p", "r", "kubernetes") {
		t.Error("nil RecordPrewarmedAssignDuration should return false")
	}
}

func TestRecordPrewarmedAssignDuration(t *testing.T) {
	pm := NewPoolMetrics("prod")
	reg := prometheus.NewRegistry()
	if err := pm.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Valid outcomes on both backends record.
	if !pm.RecordPrewarmedAssignDuration(0.05, PoolOutcomeHit, "gardenio", "crew", "lambda_microvm") {
		t.Error("hit/microvm should record")
	}
	if !pm.RecordPrewarmedAssignDuration(0.02, PoolOutcomeHit, "gardenio", "crew", "kubernetes") {
		t.Error("hit/k8s should record")
	}
	if !pm.RecordPrewarmedAssignDuration(0.3, PoolOutcomeEmpty, "gardenio", "crew", "lambda_microvm") {
		t.Error("empty should record")
	}
	// Empty labels collapse: project/role -> unknown, execution_target -> kubernetes.
	if !pm.RecordPrewarmedAssignDuration(0.01, PoolOutcomeError, "", "", "") {
		t.Error("error with empty labels should record")
	}
	// Out-of-enum outcome, negative duration are dropped.
	if pm.RecordPrewarmedAssignDuration(0.1, PoolOutcomeMiss, "gardenio", "crew", "kubernetes") {
		t.Error("miss is not an assign outcome; should be dropped")
	}
	if pm.RecordPrewarmedAssignDuration(0.1, "bogus", "gardenio", "crew", "kubernetes") {
		t.Error("bogus outcome should be dropped")
	}
	if pm.RecordPrewarmedAssignDuration(-1, PoolOutcomeHit, "gardenio", "crew", "kubernetes") {
		t.Error("negative duration should be dropped")
	}

	// The histogram's _count sub-series, keyed by the label tuple, is the
	// backend-split sample count (and the hit-rate numerator/denominator).
	counts := gatherHistogramCounts(t, reg, "gardenio_prewarmed_assign_duration_seconds",
		[]string{"outcome", "project", "role", "execution_target"})
	want := map[string]uint64{
		"hit/gardenio/crew/lambda_microvm":   1,
		"hit/gardenio/crew/kubernetes":       1,
		"empty/gardenio/crew/lambda_microvm": 1,
		"error/unknown/unknown/kubernetes":   1,
	}
	if len(counts) != len(want) {
		t.Errorf("series = %v, want %v", counts, want)
	}
	for k, v := range want {
		if counts[k] != v {
			t.Errorf("count[%s] = %d, want %d", k, counts[k], v)
		}
	}
}

// gatherHistogramCounts collects the sample count (_count) of one histogram
// family keyed by the given label values joined with "/". Fails if the env
// label is missing (every pool series must carry the cross-estate join key).
func gatherHistogramCounts(t *testing.T, reg *prometheus.Registry, name string, keyLabels []string) map[string]uint64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	counts := map[string]uint64{}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if !hasLabel(m, "env", "prod") {
				t.Errorf("series missing env=prod label: %v", m.GetLabel())
			}
			vals := map[string]string{}
			for _, l := range m.GetLabel() {
				vals[l.GetName()] = l.GetValue()
			}
			parts := make([]string, 0, len(keyLabels))
			for _, k := range keyLabels {
				parts = append(parts, vals[k])
			}
			counts[strings.Join(parts, "/")] = m.GetHistogram().GetSampleCount()
		}
	}
	return counts
}

func TestRecordWarmPlaceholderClaim(t *testing.T) {
	pm := NewPoolMetrics("prod")
	reg := prometheus.NewRegistry()
	if err := pm.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}

	if !pm.RecordWarmPlaceholderClaim(PoolOutcomeHit, "node-a", "topo-1") {
		t.Error("hit should record")
	}
	if !pm.RecordWarmPlaceholderClaim(PoolOutcomeMiss, "node-a", "topo-1") {
		t.Error("miss should record")
	}
	if !pm.RecordWarmPlaceholderClaim(PoolOutcomeError, "node-b", "") {
		t.Error("error should record (empty topology collapses to legacy)")
	}
	// Out-of-enum outcomes are dropped so cardinality stays bounded —
	// "empty" belongs to the prewarmed enum, not the claim enum.
	if pm.RecordWarmPlaceholderClaim("bogus", "node-a", "topo-1") {
		t.Error("bogus outcome should be dropped")
	}
	if pm.RecordWarmPlaceholderClaim(PoolOutcomeEmpty, "node-a", "topo-1") {
		t.Error("empty is not a claim outcome; should be dropped")
	}

	series := gatherSeries(t, reg, "gardenio_warm_placeholder_claims_total", []string{"outcome", "node", "topology"})
	want := map[string]float64{
		"hit/node-a/topo-1":   1,
		"miss/node-a/topo-1":  1,
		"error/node-b/legacy": 1,
	}
	if len(series) != len(want) {
		t.Errorf("series = %v, want %v", series, want)
	}
	for k, v := range want {
		if series[k] != v {
			t.Errorf("series[%s] = %v, want %v", k, series[k], v)
		}
	}
}

func TestRecordPrewarmedAssign(t *testing.T) {
	pm := NewPoolMetrics("prod")
	reg := prometheus.NewRegistry()
	if err := pm.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}

	if !pm.RecordPrewarmedAssign(PoolOutcomeHit, "gardenio", "crew") {
		t.Error("hit should record")
	}
	if !pm.RecordPrewarmedAssign(PoolOutcomeEmpty, "gardenio", "crew") {
		t.Error("empty should record")
	}
	if !pm.RecordPrewarmedAssign(PoolOutcomeError, "gardenio", "") {
		t.Error("error should record")
	}
	// "miss" belongs to the warm-B claim enum, not the assign enum.
	if pm.RecordPrewarmedAssign(PoolOutcomeMiss, "gardenio", "crew") {
		t.Error("miss is not an assign outcome; should be dropped")
	}
	if pm.RecordPrewarmedAssign("bogus", "gardenio", "crew") {
		t.Error("bogus outcome should be dropped")
	}

	series := gatherSeries(t, reg, "gardenio_prewarmed_assigns_total", []string{"outcome", "project", "role"})
	want := map[string]float64{
		"hit/gardenio/crew":   1,
		"empty/gardenio/crew": 1,
		"error/gardenio/":     1,
	}
	if len(series) != len(want) {
		t.Errorf("series = %v, want %v", series, want)
	}
	for k, v := range want {
		if series[k] != v {
			t.Errorf("series[%s] = %v, want %v", k, series[k], v)
		}
	}
}

// gatherSeries collects counter values for one metric family keyed by the
// given label values joined with "/". Fails the test when the env label is
// missing (every pool series must carry the cross-estate join key).
func gatherSeries(t *testing.T, reg *prometheus.Registry, name string, keyLabels []string) map[string]float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	series := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if !hasLabel(m, "env", "prod") {
				t.Errorf("series missing env=prod label: %v", m.GetLabel())
			}
			vals := map[string]string{}
			for _, l := range m.GetLabel() {
				vals[l.GetName()] = l.GetValue()
			}
			parts := make([]string, 0, len(keyLabels))
			for _, k := range keyLabels {
				parts = append(parts, vals[k])
			}
			series[strings.Join(parts, "/")] = m.GetCounter().GetValue()
		}
	}
	return series
}
