package server

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// fakeStatsSource is a store stand-in that exposes a canned sql.DBStats.
type fakeStatsSource struct{ s sql.DBStats }

func (f fakeStatsSource) DBStats() sql.DBStats { return f.s }

func gather(t *testing.T, reg *prometheus.Registry) map[string]float64 {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	out := map[string]float64{}
	for _, mf := range mfs {
		for _, m := range mf.GetMetric() {
			var v float64
			switch {
			case m.GetGauge() != nil:
				v = m.GetGauge().GetValue()
			case m.GetCounter() != nil:
				v = m.GetCounter().GetValue()
			}
			out[mf.GetName()] = v
		}
	}
	return out
}

func TestDBPoolCollectorEmitsStats(t *testing.T) {
	src := fakeStatsSource{s: sql.DBStats{
		MaxOpenConnections: 25,
		OpenConnections:    7,
		InUse:              3,
		Idle:               4,
		WaitCount:          11,
		WaitDuration:       2 * time.Second,
		MaxIdleClosed:      42,
		MaxIdleTimeClosed:  1,
		MaxLifetimeClosed:  9,
	}}

	reg := prometheus.NewRegistry()
	registerDBPoolMetrics(reg, src)

	got := gather(t, reg)

	want := map[string]float64{
		"kbeads_db_pool_max_open_connections":        25,
		"kbeads_db_pool_open_connections":            7,
		"kbeads_db_pool_in_use_connections":          3,
		"kbeads_db_pool_idle_connections":            4,
		"kbeads_db_pool_wait_count_total":            11,
		"kbeads_db_pool_wait_duration_seconds_total": 2,
		"kbeads_db_pool_max_idle_closed_total":       42,
		"kbeads_db_pool_max_idle_time_closed_total":  1,
		"kbeads_db_pool_max_lifetime_closed_total":   9,
	}
	for name, wantV := range want {
		if gotV, ok := got[name]; !ok {
			t.Errorf("%s not emitted", name)
		} else if gotV != wantV {
			t.Errorf("%s = %v, want %v", name, gotV, wantV)
		}
	}
}

// TestDBPoolCollectorReflectsChurn asserts the max_idle_closed churn signal
// tracks the live pool state — the field a post-deploy check reads to confirm
// the warm-pool fix for bug ka-0000004428 stopped the cold-dial churn.
func TestDBPoolCollectorReflectsChurn(t *testing.T) {
	stats := sql.DBStats{MaxIdleClosed: 100}
	c := newDBPoolCollector(func() sql.DBStats { return stats })

	reg := prometheus.NewRegistry()
	reg.MustRegister(c)
	if v := gather(t, reg)["kbeads_db_pool_max_idle_closed_total"]; v != 100 {
		t.Fatalf("max_idle_closed = %v, want 100", v)
	}

	stats.MaxIdleClosed = 100 // warm pool: churn counter flat between scrapes
	if v := gather(t, reg)["kbeads_db_pool_max_idle_closed_total"]; v != 100 {
		t.Fatalf("max_idle_closed = %v, want flat 100", v)
	}
}

// TestRegisterDBPoolMetricsSkipsNonPoolStore ensures a store that does not
// expose DBStats (the sqlmock test stores) registers nothing rather than
// panicking, so /metrics stays valid without a live pool.
func TestRegisterDBPoolMetricsSkipsNonPoolStore(t *testing.T) {
	reg := prometheus.NewRegistry()
	registerDBPoolMetrics(reg, struct{}{}) // no DBStats method
	if mfs, _ := reg.Gather(); len(mfs) != 0 {
		var names []string
		for _, mf := range mfs {
			names = append(names, mf.GetName())
		}
		t.Fatalf("expected no pool metrics for a non-pool store, got: %s", strings.Join(names, ", "))
	}
}
