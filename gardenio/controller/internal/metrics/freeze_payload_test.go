package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// TestRecordFreezePayloadIncomplete_EnumGuards asserts the {reason,runtime} enum
// discipline (ka-0000006933, M5): a valid reason+runtime records (returns true);
// an out-of-enum reason or runtime is DROPPED (returns false, no series created),
// mirroring RecordFreezeLatency's cardinality guard. An empty role collapses to
// "unknown".
func TestRecordFreezePayloadIncomplete_EnumGuards(t *testing.T) {
	fm := NewFreezeMetrics("e2e")
	reg := prometheus.NewRegistry()
	if err := fm.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}

	cases := []struct {
		name         string
		reason, role string
		runtime      string
		want         bool
	}{
		{"valid work_copy", FreezePayloadReasonWorkCopy, "crew", FreezeRuntimeRunsc, true},
		{"valid tmp_copy", FreezePayloadReasonTmpCopy, "gardener", FreezeRuntimeRunsc, true},
		{"valid gofer", FreezePayloadReasonGofer, "crew", FreezeRuntimeRunsc, true},
		{"valid gofer_malformed", FreezePayloadReasonGoferMalformed, "crew", FreezeRuntimeRunsc, true},
		{"empty role -> unknown, still recorded", FreezePayloadReasonWorkCopy, "", FreezeRuntimeRunsc, true},
		{"symlink is NOT a valid incomplete reason", "symlink", "crew", FreezeRuntimeRunsc, false},
		{"bad reason dropped", "work-copy", "crew", FreezeRuntimeRunsc, false},
		{"empty reason dropped", "", "crew", FreezeRuntimeRunsc, false},
		{"bad runtime dropped", FreezePayloadReasonWorkCopy, "crew", "kata", false},
	}
	for _, c := range cases {
		if got := fm.RecordFreezePayloadIncomplete(c.reason, c.role, c.runtime); got != c.want {
			t.Errorf("%s: RecordFreezePayloadIncomplete=%v want %v", c.name, got, c.want)
		}
	}

	// Exactly the 5 valid calls above must have landed as counter increments; the 4
	// invalid ones must not have created any series.
	var total float64
	for _, mf := range reg2Gather(t, reg) {
		if mf.GetName() != "gardenio_freeze_payload_incomplete_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			total += m.GetCounter().GetValue()
			if !hasLabel(m, "env", "e2e") {
				t.Errorf("series missing env=e2e label: %v", m.GetLabel())
			}
		}
	}
	if total != 5 {
		t.Errorf("expected 5 valid increments, got %v", total)
	}
	// "symlink" must never appear as a reason on the incomplete counter — that is
	// the whole point of the two-counter split (a naive rate()>0 alert must never
	// fire on benign noise).
	for _, mf := range reg2Gather(t, reg) {
		if mf.GetName() != "gardenio_freeze_payload_incomplete_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			if hasLabel(m, "reason", "symlink") {
				t.Error("payload_incomplete must never carry reason=symlink (benign noise leaked into the alert metric)")
			}
		}
	}
}

// TestRecordFreezePayloadSymlinkSkip_EnumGuards asserts the {leg,runtime} enum
// discipline for the benign-noise counter (ka-0000006933, M5).
func TestRecordFreezePayloadSymlinkSkip_EnumGuards(t *testing.T) {
	fm := NewFreezeMetrics("e2e")
	reg := prometheus.NewRegistry()
	if err := fm.Register(reg); err != nil {
		t.Fatalf("register: %v", err)
	}

	cases := []struct {
		name      string
		leg, role string
		runtime   string
		want      bool
	}{
		{"valid work", FreezePayloadLegWork, "crew", FreezeRuntimeRunsc, true},
		{"valid tmp", FreezePayloadLegTmp, "crew", FreezeRuntimeRunsc, true},
		{"empty role -> unknown", FreezePayloadLegWork, "", FreezeRuntimeRunsc, true},
		{"bad leg dropped", "gofer", "crew", FreezeRuntimeRunsc, false},
		{"empty leg dropped", "", "crew", FreezeRuntimeRunsc, false},
		{"bad runtime dropped", FreezePayloadLegWork, "crew", "kata", false},
	}
	for _, c := range cases {
		if got := fm.RecordFreezePayloadSymlinkSkip(c.leg, c.role, c.runtime); got != c.want {
			t.Errorf("%s: RecordFreezePayloadSymlinkSkip=%v want %v", c.name, got, c.want)
		}
	}

	var total float64
	for _, mf := range reg2Gather(t, reg) {
		if mf.GetName() != "gardenio_freeze_payload_symlink_skip_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			total += m.GetCounter().GetValue()
		}
	}
	if total != 3 {
		t.Errorf("expected 3 valid increments, got %v", total)
	}
}

// TestFreezePayloadEnumValidators guards the closed reason/leg sets directly.
func TestFreezePayloadEnumValidators(t *testing.T) {
	for _, r := range []string{FreezePayloadReasonWorkCopy, FreezePayloadReasonTmpCopy, FreezePayloadReasonGofer, FreezePayloadReasonGoferMalformed} {
		if !freezePayloadReasonValid(r) {
			t.Errorf("reason %q must be valid", r)
		}
	}
	for _, r := range []string{"", "symlink", "work-copy", "WorkCopy", "clean", "copyfail"} {
		if freezePayloadReasonValid(r) {
			t.Errorf("reason %q must be invalid", r)
		}
	}
	for _, l := range []string{FreezePayloadLegWork, FreezePayloadLegTmp} {
		if !freezePayloadLegValid(l) {
			t.Errorf("leg %q must be valid", l)
		}
	}
	for _, l := range []string{"", "gofer", "Work", "work_copy"} {
		if freezePayloadLegValid(l) {
			t.Errorf("leg %q must be invalid", l)
		}
	}
}

// TestFreezePayload_NilReceiver verifies both new record methods are safe no-ops
// on a nil metric, so the controller can call them unconditionally without a
// panic (invariant I1 protection — a metric panic must never break the freeze).
func TestFreezePayload_NilReceiver(t *testing.T) {
	var fm *FreezeMetrics
	if fm.RecordFreezePayloadIncomplete(FreezePayloadReasonWorkCopy, "crew", FreezeRuntimeRunsc) {
		t.Error("nil receiver RecordFreezePayloadIncomplete=true, want false")
	}
	if fm.RecordFreezePayloadSymlinkSkip(FreezePayloadLegWork, "crew", FreezeRuntimeRunsc) {
		t.Error("nil receiver RecordFreezePayloadSymlinkSkip=true, want false")
	}
}

// reg2Gather is a tiny gather-or-fatal helper (mirrors the inline gather in the
// sibling metric tests).
func reg2Gather(t *testing.T, reg *prometheus.Registry) []*dto.MetricFamily {
	t.Helper()
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	return mfs
}
