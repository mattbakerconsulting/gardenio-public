package server

import (
	"encoding/json"
	"errors"
	"testing"

	"gardenio/kbeads/internal/model"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// agentLikeTC mirrors the strict, closed-schema shape of builtinConfigs["type:agent"]:
// a fixed set of declared fields, one required, and AllowAdditionalFields=false
// so an undeclared key 400s — the exact property that produced the schema-drift
// outages (ka-0000003673). Used to drive the REAL validator so the reason
// classifier is tested against genuine validateFieldsImpl output, not
// hand-built ValidationErrors that could drift from the message text.
func agentLikeTC() *model.TypeConfig {
	return &model.TypeConfig{
		TypeName:              "agent",
		AllowAdditionalFields: false,
		Fields: []model.FieldDef{
			{Name: "role", Type: model.FieldTypeString, Required: true},
			{Name: "respawn_count", Type: model.FieldTypeInteger},
			{Name: "enabled", Type: model.FieldTypeBoolean},
		},
	}
}

func TestClassifyFieldRejection_Reasons(t *testing.T) {
	tc := agentLikeTC()

	cases := []struct {
		name   string
		fields string
		want   string
	}{
		{
			// The motivating outage: bridge sends a field the daemon's builtin
			// does not declare (`owner`). This is the schema-drift fingerprint.
			name:   "unknown field",
			fields: `{"role":"crew","owner":"matt"}`,
			want:   rejectReasonUnknownField,
		},
		{
			name:   "required field missing",
			fields: `{"respawn_count":1}`,
			want:   rejectReasonRequiredField,
		},
		{
			name:   "type mismatch",
			fields: `{"role":"crew","respawn_count":"three"}`,
			want:   rejectReasonTypeMismatch,
		},
		{
			name:   "malformed fields blob",
			fields: `["not","an","object"]`,
			want:   rejectReasonMalformed,
		},
	}

	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			err := model.ValidateTypeFields(json.RawMessage(tt.fields), tc)
			if err == nil {
				t.Fatalf("expected validation error for %q, got nil", tt.fields)
			}
			if got := classifyFieldRejection(err); got != tt.want {
				t.Fatalf("classifyFieldRejection(%q) = %q, want %q", tt.fields, got, tt.want)
			}
		})
	}
}

// A write can trip several field errors at once. unknown_field is the
// drift signal and must win over a co-occurring required/type error so the
// alert on reason="unknown_field" is never masked.
func TestClassifyFieldRejection_UnknownFieldWinsPrecedence(t *testing.T) {
	tc := agentLikeTC()
	// Missing required `role` AND an unknown `owner` AND a mistyped
	// `respawn_count` — all three in one write.
	err := model.ValidateTypeFields(json.RawMessage(`{"owner":"matt","respawn_count":"three"}`), tc)
	if err == nil {
		t.Fatal("expected multi-error validation failure, got nil")
	}
	if got := classifyFieldRejection(err); got != rejectReasonUnknownField {
		t.Fatalf("classifyFieldRejection = %q, want %q (unknown_field must win precedence)", got, rejectReasonUnknownField)
	}
}

func TestClassifyFieldRejection_NonValidationError(t *testing.T) {
	if got := classifyFieldRejection(errors.New("boom")); got != rejectReasonOther {
		t.Fatalf("classifyFieldRejection(non-validation) = %q, want %q", got, rejectReasonOther)
	}
	// A ValidationError with no field errors also degrades to "other".
	if got := classifyFieldRejection(&model.ValidationError{}); got != rejectReasonOther {
		t.Fatalf("classifyFieldRejection(empty ValidationError) = %q, want %q", got, rejectReasonOther)
	}
}

// recordBeadWriteRejection must increment the counter under the classified
// reason and the allowlisted bead_type, and must never fire for a nil error
// (a normal, accepted write).
func TestRecordBeadWriteRejection_Counter(t *testing.T) {
	tc := agentLikeTC()

	// Baseline for the label pair we assert against.
	before := testutil.ToFloat64(beadsWritesRejectedTotal.WithLabelValues(rejectReasonUnknownField, "agent"))

	driftErr := model.ValidateTypeFields(json.RawMessage(`{"role":"crew","owner":"matt"}`), tc)
	recordBeadWriteRejection("agent", driftErr)

	after := testutil.ToFloat64(beadsWritesRejectedTotal.WithLabelValues(rejectReasonUnknownField, "agent"))
	if after-before != 1 {
		t.Fatalf("unknown_field/agent counter delta = %v, want 1", after-before)
	}

	// A nil error (accepted write) is a no-op — no false alarm on normal ops.
	nilBase := testutil.ToFloat64(beadsWritesRejectedTotal.WithLabelValues(rejectReasonOther, "agent"))
	recordBeadWriteRejection("agent", nil)
	if got := testutil.ToFloat64(beadsWritesRejectedTotal.WithLabelValues(rejectReasonOther, "agent")); got != nilBase {
		t.Fatalf("nil-error write incremented the counter (%v -> %v); accepted writes must not alarm", nilBase, got)
	}
}

// A user-defined bead type outside the hard allowlist collapses to "custom" so
// label cardinality cannot blow up.
// End-to-end wiring proof: a real createBead of a strict-schema type (epic,
// which does not allow_additional_fields) carrying an undeclared field must be
// refused AND must bump the counter under unknown_field/epic. This is the guard
// that the recordBeadWriteRejection call is actually reached on the create path
// — a helper-only test would pass even if the wiring were deleted.
func TestCreateBead_UnknownField_IncrementsCounter(t *testing.T) {
	s, _, _ := newTestServer()
	before := testutil.ToFloat64(beadsWritesRejectedTotal.WithLabelValues(rejectReasonUnknownField, "epic"))

	_, err := s.createBead(ctxAsTestSubject(), createBeadInput{
		Title:  "epic with a drifted field",
		Type:   "epic",
		Fields: json.RawMessage(`{"totally_undeclared_field":"x"}`),
	})
	if err == nil {
		t.Fatal("expected createBead to reject an unknown field on strict type=epic")
	}

	after := testutil.ToFloat64(beadsWritesRejectedTotal.WithLabelValues(rejectReasonUnknownField, "epic"))
	if after-before != 1 {
		t.Fatalf("createBead unknown-field counter delta = %v, want 1 (increment not wired into create path)", after-before)
	}
}

// A normal, schema-valid create must NOT touch the counter — the alert must not
// false-alarm on healthy writes.
func TestCreateBead_ValidWrite_DoesNotIncrementCounter(t *testing.T) {
	s, _, _ := newTestServer()
	// Sum across the reasons a healthy epic create could plausibly trip.
	sum := func() float64 {
		total := 0.0
		for _, r := range []string{rejectReasonUnknownField, rejectReasonRequiredField, rejectReasonTypeMismatch, rejectReasonMalformed, rejectReasonOther} {
			total += testutil.ToFloat64(beadsWritesRejectedTotal.WithLabelValues(r, "epic"))
		}
		return total
	}
	before := sum()
	if _, err := s.createBead(ctxAsTestSubject(), createBeadInput{Title: "healthy epic", Type: "epic"}); err != nil {
		t.Fatalf("createBead(valid epic) error: %v", err)
	}
	if after := sum(); after != before {
		t.Fatalf("valid create changed the rejection counter (%v -> %v); healthy writes must not alarm", before, after)
	}
}

func TestRecordBeadWriteRejection_BeadTypeAllowlist(t *testing.T) {
	tc := &model.TypeConfig{
		TypeName:              "my-weird-type",
		AllowAdditionalFields: false,
		Fields:                []model.FieldDef{{Name: "a", Type: model.FieldTypeString}},
	}
	before := testutil.ToFloat64(beadsWritesRejectedTotal.WithLabelValues(rejectReasonUnknownField, "custom"))
	err := model.ValidateTypeFields(json.RawMessage(`{"b":"x"}`), tc)
	recordBeadWriteRejection("my-weird-type", err)
	after := testutil.ToFloat64(beadsWritesRejectedTotal.WithLabelValues(rejectReasonUnknownField, "custom"))
	if after-before != 1 {
		t.Fatalf("custom bead_type counter delta = %v, want 1", after-before)
	}
}
