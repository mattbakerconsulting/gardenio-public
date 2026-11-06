package metrics

import (
	"math"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestXCheckAgreement(t *testing.T) {
	tests := []struct {
		name string
		a, b float64
		want float64
	}{
		{"exact match", 100, 100, 1.0},
		{"both zero agree", 0, 0, 1.0},
		{"ten percent low", 90, 100, 0.9},
		{"ten percent low symmetric", 100, 90, 0.9},
		{"half", 50, 100, 0.5},
		{"total divergence one side zero", 0, 100, 0.0},
		{"negative clamped to zero", -50, 0, 1.0},
		{"negative input clamped before scoring", -10, 100, 0.0},
		{"sub-epsilon both sides treated as agreeing", 1e-12, 1e-12, 1.0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := XCheckAgreement(tt.a, tt.b)
			if math.Abs(got-tt.want) > 1e-9 {
				t.Fatalf("XCheckAgreement(%v,%v) = %v, want %v", tt.a, tt.b, got, tt.want)
			}
			if got < 0 || got > 1 {
				t.Fatalf("XCheckAgreement(%v,%v) = %v out of [0,1]", tt.a, tt.b, got)
			}
		})
	}
}

func TestXCheckSetPairPublishesThreeSeries(t *testing.T) {
	x := NewXCheckMetrics("test-env")
	reg := prometheus.NewRegistry()
	if err := x.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}

	x.SetPair("bill-conservation", 90, 100)

	if got := testutil.ToFloat64(x.ValueGauge().WithLabelValues("bill-conservation", XCheckSideA, "test-env")); got != 90 {
		t.Fatalf("side a = %v, want 90", got)
	}
	if got := testutil.ToFloat64(x.ValueGauge().WithLabelValues("bill-conservation", XCheckSideB, "test-env")); got != 100 {
		t.Fatalf("side b = %v, want 100", got)
	}
	if got := testutil.ToFloat64(x.AgreementGauge().WithLabelValues("bill-conservation", "test-env")); math.Abs(got-0.9) > 1e-9 {
		t.Fatalf("agreement = %v, want 0.9", got)
	}
}

func TestXCheckClearPairRemovesSeries(t *testing.T) {
	x := NewXCheckMetrics("test-env")
	x.SetPair("p", 1, 1)
	x.ClearPair("p")

	if n := testutil.CollectAndCount(x.ValueGauge()); n != 0 {
		t.Fatalf("value series after clear = %d, want 0", n)
	}
	if n := testutil.CollectAndCount(x.AgreementGauge()); n != 0 {
		t.Fatalf("agreement series after clear = %d, want 0", n)
	}
}

func TestXCheckNilReceiverSafe(t *testing.T) {
	var x *XCheckMetrics
	// None of these should panic on a nil receiver (metrics-less test runs).
	x.SetPair("p", 1, 2)
	x.ClearPair("p")
	if x.ValueGauge() != nil || x.AgreementGauge() != nil {
		t.Fatal("nil receiver accessors should return nil")
	}
}
