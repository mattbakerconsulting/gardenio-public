package server

import (
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestClassifyOIDCInitError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, "none"},
		{"timeout", errors.New("dial tcp 10.43.6.66:443: i/o timeout"), "timeout"},
		{"deadline exceeded", errors.New("context deadline exceeded"), "timeout"},
		{"connection refused", errors.New("dial tcp 10.43.6.66:8080: connect: connection refused"), "connection_refused"},
		{"dns", errors.New("dial tcp: lookup gardenio-keycloak-runtime: no such host"), "dns"},
		{"tls", errors.New("x509: certificate signed by unknown authority"), "tls"},
		{"other", errors.New("oidc: issuer did not match the issuer returned by provider"), "other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyOIDCInitError(tt.err); got != tt.want {
				t.Errorf("classifyOIDCInitError(%v) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// TestRecordOIDCVerifierHealth verifies the gauge/error-series pair the
// gd-qZs1IbMhzQ persistent-warning fix relies on: a failure sets the health
// gauge to 0 and stamps exactly one bounded-reason error series, a later
// success flips health back to 1 and clears the error series entirely (not
// just zeroes it — Reset removes the series so it stops appearing in
// /metrics once recovered).
func TestRecordOIDCVerifierHealth(t *testing.T) {
	reg := prometheus.NewRegistry()
	registerOIDCVerifierMetrics(reg)
	t.Cleanup(func() {
		beadsOIDCVerifierHealthy.Set(0)
		beadsOIDCVerifierInitErrors.Reset()
	})

	RecordOIDCVerifierHealth(1, errors.New("dial tcp 10.43.6.66:443: i/o timeout"))
	if got := testutil.ToFloat64(beadsOIDCVerifierHealthy); got != 0 {
		t.Errorf("healthy = %v, want 0 after failed init", got)
	}
	if got := testutil.ToFloat64(beadsOIDCVerifierInitErrors.WithLabelValues("timeout", "1")); got != 1 {
		t.Errorf("init_error{reason=timeout,issuer_count=1} = %v, want 1", got)
	}

	RecordOIDCVerifierHealth(1, nil)
	if got := testutil.ToFloat64(beadsOIDCVerifierHealthy); got != 1 {
		t.Errorf("healthy = %v, want 1 after successful init", got)
	}
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == "beads_oidc_verifier_init_error" && len(mf.GetMetric()) != 0 {
			t.Errorf("init_error series still present after success: %v", mf.GetMetric())
		}
	}
}
