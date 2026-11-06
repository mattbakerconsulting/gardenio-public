package model

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// fieldMessage returns the Message of the first FieldError for the named field,
// failing the test if there is no such error.
func fieldMessage(t *testing.T, err error, field string) string {
	t.Helper()
	for _, fe := range fieldErrors(t, err) {
		if fe.Field == field {
			return fe.Message
		}
	}
	t.Fatalf("no error for field %q in %v", field, err)
	return ""
}

func TestValidateFields_EmptyDefsEmptyFields(t *testing.T) {
	if err := ValidateTypeFields(nil, &TypeConfig{Fields: nil}); err != nil {
		t.Fatalf("expected nil, got %v", err)
	}
}

func TestValidateFields_RequiredMissing(t *testing.T) {
	defs := []FieldDef{{Name: "severity", Type: FieldTypeString, Required: true}}
	err := ValidateTypeFields(nil, &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for missing required field")
	}
	errs := fieldErrors(t, err)
	if !hasFieldError(errs, "severity") {
		t.Error("expected error on 'severity'")
	}
}

func TestValidateFields_RequiredNullValue(t *testing.T) {
	defs := []FieldDef{{Name: "severity", Type: FieldTypeString, Required: true}}
	err := ValidateTypeFields(json.RawMessage(`{"severity":null}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for null required field")
	}
	errs := fieldErrors(t, err)
	if !hasFieldError(errs, "severity") {
		t.Error("expected error on 'severity'")
	}
}

func TestValidateFields_OptionalMissing(t *testing.T) {
	defs := []FieldDef{{Name: "notes", Type: FieldTypeString}}
	if err := ValidateTypeFields(json.RawMessage(`{}`), &TypeConfig{Fields: defs}); err != nil {
		t.Fatalf("optional missing field should pass, got %v", err)
	}
}

func TestValidateFields_UnknownField(t *testing.T) {
	defs := []FieldDef{{Name: "severity", Type: FieldTypeString}}
	err := ValidateTypeFields(json.RawMessage(`{"severity":"high","bogus":123}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
	errs := fieldErrors(t, err)
	if !hasFieldError(errs, "bogus") {
		t.Error("expected error on 'bogus'")
	}
}

// TestValidateFields_UnknownFieldSuggestsNearMiss is the core regression for
// ka-0000002423: the field is slack_channel, the author typed
// slack_thread_channel, and the rejection must name the intended field. This
// exact drift (a dropped "thread_" token, edit distance 7) sent a real agent
// down a dead end — it concluded the capability didn't exist and filed a
// redundant feature (ka-0000003647). One near-miss name would have fixed it.
func TestValidateFields_UnknownFieldSuggestsNearMiss(t *testing.T) {
	defs := []FieldDef{
		{Name: "slack_channel", Type: FieldTypeString},
		{Name: "slack_user_id", Type: FieldTypeString},
		{Name: "prompt", Type: FieldTypeString},
	}
	err := ValidateTypeFields(json.RawMessage(`{"slack_thread_channel":"C123"}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for unknown field slack_thread_channel")
	}
	msg := fieldMessage(t, err, "slack_thread_channel")
	if !strings.HasPrefix(msg, "unknown field") {
		t.Errorf("message must keep the stable %q prefix, got %q", "unknown field", msg)
	}
	if !strings.Contains(msg, "slack_channel") {
		t.Errorf("expected did-you-mean to name slack_channel, got %q", msg)
	}
	// A did-you-mean is exclusive with the far-miss field list; don't dump both.
	if strings.Contains(msg, "valid fields:") {
		t.Errorf("near-miss message should not also list valid fields, got %q", msg)
	}
}

// TestValidateFields_UnknownFieldSuggestsTypo covers the ordinary-typo path
// (shares no whole token, but close by edit distance): sevrity → severity.
func TestValidateFields_UnknownFieldSuggestsTypo(t *testing.T) {
	defs := []FieldDef{
		{Name: "severity", Type: FieldTypeString},
		{Name: "component", Type: FieldTypeString},
	}
	err := ValidateTypeFields(json.RawMessage(`{"sevrity":"high"}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for unknown field sevrity")
	}
	msg := fieldMessage(t, err, "sevrity")
	if !strings.Contains(msg, `"severity"`) {
		t.Errorf("expected did-you-mean severity, got %q", msg)
	}
}

// TestValidateFields_UnknownFieldFarMissListsValidFields covers the far-miss
// path: a name close to nothing gets no bogus suggestion — instead the message
// lists the valid field names so the author can pick the right one.
func TestValidateFields_UnknownFieldFarMissListsValidFields(t *testing.T) {
	defs := []FieldDef{
		{Name: "severity", Type: FieldTypeString},
		{Name: "component", Type: FieldTypeString},
		{Name: "assignee", Type: FieldTypeString},
	}
	err := ValidateTypeFields(json.RawMessage(`{"totally_bogus_zzz":"x"}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for unknown field totally_bogus_zzz")
	}
	msg := fieldMessage(t, err, "totally_bogus_zzz")
	if strings.Contains(msg, "did you mean") {
		t.Errorf("far-miss must not fabricate a suggestion, got %q", msg)
	}
	if !strings.Contains(msg, "valid fields:") {
		t.Errorf("expected far-miss to list valid fields, got %q", msg)
	}
	for _, name := range []string{"assignee", "component", "severity"} {
		if !strings.Contains(msg, name) {
			t.Errorf("valid-fields list missing %q, got %q", name, msg)
		}
	}
	// Sorted order: assignee < component < severity.
	if i, j := strings.Index(msg, "assignee"), strings.Index(msg, "severity"); i > j {
		t.Errorf("valid fields should be sorted, got %q", msg)
	}
}

// TestValidateFields_UnknownFieldFarMissCapsList caps the far-miss list at 10
// names plus a "+N more" so a wide type (trigger has 89 fields) doesn't dump a
// wall of text into one error line.
func TestValidateFields_UnknownFieldFarMissCapsList(t *testing.T) {
	defs := make([]FieldDef, 0, 25)
	for i := 0; i < 25; i++ {
		defs = append(defs, FieldDef{Name: fmt.Sprintf("field_%02d", i), Type: FieldTypeString})
	}
	err := ValidateTypeFields(json.RawMessage(`{"zzz_no_match_at_all":"x"}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error")
	}
	msg := fieldMessage(t, err, "zzz_no_match_at_all")
	if !strings.Contains(msg, "+15 more") {
		t.Errorf("expected '+15 more' suffix for 25 fields capped at 10, got %q", msg)
	}
	// Exactly 10 names before the suffix.
	list := strings.TrimPrefix(msg, "unknown field; valid fields: ")
	list = strings.SplitN(list, " (+", 2)[0]
	if got := len(strings.Split(list, ", ")); got != 10 {
		t.Errorf("expected 10 sampled names, got %d (%q)", got, list)
	}
}

// TestValidateFields_UnknownFieldNoDefsStaysBare: a closed schema that declares
// no fields at all has nothing to suggest or list, so the message stays the
// bare "unknown field".
func TestValidateFields_UnknownFieldNoDefsStaysBare(t *testing.T) {
	tc := &TypeConfig{Kind: KindConfig} // no Fields
	err := ValidateTypeFields(json.RawMessage(`{"anything":"x"}`), tc)
	if err == nil {
		t.Fatal("expected error for unknown field against empty schema")
	}
	msg := fieldMessage(t, err, "anything")
	if msg != "unknown field" {
		t.Errorf("empty-schema message should stay bare %q, got %q", "unknown field", msg)
	}
}

// TestValidateTypeFields_FarMissRoutesToKdFields is the core regression for
// ka-0000000365: when a type-aware validation rejects a far-miss unknown field,
// the message must point the author at `kd fields <type>` — the introspection
// command that lists every valid field — instead of dumping a truncated
// "+N more" sample that leaves them guessing at the rest. The type name comes
// from TypeConfig.TypeName, which resolveTypeConfig stamps in-memory.
func TestValidateTypeFields_FarMissRoutesToKdFields(t *testing.T) {
	// A wide schema so the old behaviour would have truncated the list.
	defs := make([]FieldDef, 0, 20)
	for i := 0; i < 20; i++ {
		defs = append(defs, FieldDef{Name: fmt.Sprintf("field_%02d", i), Type: FieldTypeString})
	}
	tc := &TypeConfig{TypeName: "task", Kind: KindIssue, Fields: defs}

	err := ValidateTypeFields(json.RawMessage(`{"zzz_no_match_at_all":"x"}`), tc)
	if err == nil {
		t.Fatal("expected error for far-miss unknown field")
	}
	msg := fieldMessage(t, err, "zzz_no_match_at_all")
	if !strings.HasPrefix(msg, "unknown field") {
		t.Errorf("message must keep the stable %q prefix, got %q", "unknown field", msg)
	}
	if !strings.Contains(msg, "kd fields task") {
		t.Errorf("far-miss message should route to `kd fields task`, got %q", msg)
	}
	// The whole point of the fix: no truncation and no inline field dump.
	if strings.Contains(msg, "more)") || strings.Contains(msg, "valid fields:") {
		t.Errorf("far-miss message should not dump a truncated field list, got %q", msg)
	}
}

// TestValidateTypeFieldsForUpdate_FarMissRoutesToKdFields mirrors the create
// path for the update path — a far-miss unknown field on `kd update -f` must
// also route to `kd fields <type>` (the acceptance criterion names update).
func TestValidateTypeFieldsForUpdate_FarMissRoutesToKdFields(t *testing.T) {
	tc := &TypeConfig{
		TypeName: "task",
		Kind:     KindIssue,
		Fields:   []FieldDef{{Name: "schedule_id", Type: FieldTypeString}},
	}
	err := ValidateTypeFieldsForUpdate(json.RawMessage(`{"zzz_no_match_at_all":"x"}`), tc, nil)
	if err == nil {
		t.Fatal("expected error for far-miss unknown field on update")
	}
	msg := fieldMessage(t, err, "zzz_no_match_at_all")
	if !strings.Contains(msg, "kd fields task") {
		t.Errorf("update far-miss message should route to `kd fields task`, got %q", msg)
	}
}

// TestValidateTypeFields_FarMissNoTypeNameFallsBackToSample: when the type name
// is unknown (e.g. a hand-built TypeConfig with no TypeName), the far-miss
// branch still falls back to the capped inline sample rather than emitting a
// bogus `kd fields ` with an empty type.
func TestValidateTypeFields_FarMissNoTypeNameFallsBackToSample(t *testing.T) {
	tc := &TypeConfig{
		Kind:   KindIssue,
		Fields: []FieldDef{{Name: "severity", Type: FieldTypeString}, {Name: "component", Type: FieldTypeString}},
	}
	err := ValidateTypeFields(json.RawMessage(`{"zzz_no_match_at_all":"x"}`), tc)
	if err == nil {
		t.Fatal("expected error for far-miss unknown field")
	}
	msg := fieldMessage(t, err, "zzz_no_match_at_all")
	if strings.Contains(msg, "kd fields") {
		t.Errorf("no type name should not emit a gd-fields pointer, got %q", msg)
	}
	if !strings.Contains(msg, "valid fields:") {
		t.Errorf("no-type-name far-miss should fall back to inline sample, got %q", msg)
	}
}

// TestValidateTypeFields_NearMissStillSuggestsWithTypeName: the did-you-mean
// path is unchanged by the type-name plumbing — a near miss still names the
// intended field and does NOT append the gd-fields pointer.
func TestValidateTypeFields_NearMissStillSuggestsWithTypeName(t *testing.T) {
	tc := &TypeConfig{
		TypeName: "trigger",
		Kind:     KindData,
		Fields:   []FieldDef{{Name: "slack_channel", Type: FieldTypeString}, {Name: "prompt", Type: FieldTypeString}},
	}
	err := ValidateTypeFields(json.RawMessage(`{"slack_thread_channel":"C123"}`), tc)
	if err == nil {
		t.Fatal("expected error for near-miss unknown field")
	}
	msg := fieldMessage(t, err, "slack_thread_channel")
	if !strings.Contains(msg, "slack_channel") {
		t.Errorf("near-miss should still name slack_channel, got %q", msg)
	}
	if strings.Contains(msg, "kd fields") {
		t.Errorf("near-miss should not append the gd-fields pointer, got %q", msg)
	}
}

// TestValidateTypeFields_TypeMismatchRoutesToKdFields is the core regression for
// ka-0000004431: a KNOWN field given a value of the WRONG TYPE (the dominant
// "guess the --field value type, eat a 400, then read the schema" papercut) must
// route the author to `kd fields <type>` — symmetric with the unknown-field-name
// case — while keeping the bare "must be a ..." shape hint the caller relies on.
func TestValidateTypeFields_TypeMismatchRoutesToKdFields(t *testing.T) {
	tc := &TypeConfig{
		TypeName: "task",
		Kind:     KindIssue,
		Fields:   []FieldDef{{Name: "gitlab_mr_iid", Type: FieldTypeString}},
	}
	// An integer-looking value against a string-typed field — exactly the shape
	// that 400'd with a bare "must be a string" and no route to the schema.
	err := ValidateTypeFields(json.RawMessage(`{"gitlab_mr_iid":2387}`), tc)
	if err == nil {
		t.Fatal("expected error for wrong-typed value on a known field")
	}
	msg := fieldMessage(t, err, "gitlab_mr_iid")
	if !strings.Contains(msg, "must be a string") {
		t.Errorf("type-mismatch should keep the bare shape hint, got %q", msg)
	}
	if !strings.Contains(msg, "kd fields task") {
		t.Errorf("type-mismatch should route to `kd fields task`, got %q", msg)
	}
}

// TestValidateTypeFieldsForUpdate_TypeMismatchRoutesToKdFields mirrors the
// create path for the update path — a CHANGED value of the wrong type on
// `kd update -f` must also route to `kd fields <type>`. (An UNCHANGED legacy
// wrong-typed value is grandfathered elsewhere and never reaches this branch.)
func TestValidateTypeFieldsForUpdate_TypeMismatchRoutesToKdFields(t *testing.T) {
	tc := &TypeConfig{
		TypeName: "trigger",
		Kind:     KindData,
		Fields:   []FieldDef{{Name: "enabled", Type: FieldTypeBoolean}},
	}
	// prior has no `enabled`, so the incoming value is CHANGED and is validated.
	err := ValidateTypeFieldsForUpdate(json.RawMessage(`{"enabled":"true"}`), tc, map[string]any{})
	if err == nil {
		t.Fatal("expected error for wrong-typed value on update")
	}
	msg := fieldMessage(t, err, "enabled")
	if !strings.Contains(msg, "must be a boolean") {
		t.Errorf("type-mismatch should keep the bare shape hint, got %q", msg)
	}
	if !strings.Contains(msg, "kd fields trigger") {
		t.Errorf("update type-mismatch should route to `kd fields trigger`, got %q", msg)
	}
}

// TestValidateTypeFields_TypeMismatchNoTypeNameStaysBare: when the type name is
// unknown (a hand-built TypeConfig, or a nil config), the type-mismatch
// message stays bare — a bogus `kd fields ` pointer with an empty type would
// be worse than none.
func TestValidateTypeFields_TypeMismatchNoTypeNameStaysBare(t *testing.T) {
	defs := []FieldDef{{Name: "count", Type: FieldTypeInteger}}
	err := ValidateTypeFields(json.RawMessage(`{"count":"nope"}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for wrong-typed value")
	}
	msg := fieldMessage(t, err, "count")
	if !strings.Contains(msg, "must be an integer") {
		t.Errorf("type-mismatch should keep the bare shape hint, got %q", msg)
	}
	if strings.Contains(msg, "kd fields") {
		t.Errorf("no type name should not emit a gd-fields pointer, got %q", msg)
	}
}

// TestClosestFieldName_ThresholdAndTieBreak white-box-tests the two fragile
// tuning knobs directly: the 0.6 score floor and the first-declared tie-break.
func TestClosestFieldName_ThresholdAndTieBreak(t *testing.T) {
	// Boundary: "ab_cd" vs "ab_ef" share 1 of 3 tokens → Jaccard 1/3 ≈ 0.33,
	// and edit similarity 3/5 = 0.6 exactly. closestFieldName requires strictly
	// greater than 0.6, so an exact-0.6 candidate must NOT be suggested.
	if got := closestFieldName("ab_cd", []string{"ab_ef"}); got != "" {
		t.Errorf("exact-0.6 score must not suggest (strict > threshold), got %q", got)
	}
	// Just over: "sevrity" vs "severity" ≈ 0.875 clears the floor.
	if got := closestFieldName("sevrity", []string{"severity"}); got != "severity" {
		t.Errorf("clearly-above-threshold typo should suggest severity, got %q", got)
	}

	// Tie-break: two candidates equidistant from the key. "severi" is one edit
	// from both "severit" and "severiX"-style names of equal length, but keep it
	// simple with a genuine tie: "abcx" vs "abcy" and key "abcz" each have edit
	// distance 1 (similarity 0.75) and no shared '_'-token, so both score 0.75.
	// closestFieldName uses strict `>`, so the FIRST-declared equal-scoring
	// candidate wins and a later tie never displaces it.
	got := closestFieldName("abcz", []string{"abcx", "abcy"})
	if got != "abcx" {
		t.Errorf("on equal scores the first-declared candidate should win, got %q", got)
	}
}

func TestValidateFields_StringType(t *testing.T) {
	defs := []FieldDef{{Name: "name", Type: FieldTypeString}}
	if err := ValidateTypeFields(json.RawMessage(`{"name":"hello"}`), &TypeConfig{Fields: defs}); err != nil {
		t.Fatalf("expected pass for string, got %v", err)
	}
	err := ValidateTypeFields(json.RawMessage(`{"name":123}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for non-string")
	}
}

// TestValidateFields_StringTypeCoercionHint pins the actionable hint appended
// when a bare JSON scalar (number/bool) is fed to a string-typed field: the CLI
// auto-detects `2387` as a JSON number before any server round-trip, so the
// author needs to know to nest-quote it as `field="2387"`. The hint names that
// fix inline (ka-0000001910).
func TestValidateFields_StringTypeCoercionHint(t *testing.T) {
	defs := []FieldDef{{Name: "gitlab_mr_iid", Type: FieldTypeString}}

	// Integer-looking value: the motivating case (gitlab_mr_iid=2387).
	msg := fieldMessage(t, ValidateTypeFields(json.RawMessage(`{"gitlab_mr_iid":2387}`), &TypeConfig{Fields: defs}), "gitlab_mr_iid")
	if !strings.HasPrefix(msg, "must be a string") {
		t.Errorf("hint must keep the 'must be a string' prefix, got %q", msg)
	}
	if !strings.Contains(msg, `gitlab_mr_iid="2387"`) {
		t.Errorf("expected the field-named quoted example %q in %q", `gitlab_mr_iid="2387"`, msg)
	}
	if !strings.Contains(msg, "wrap the value in quotes") {
		t.Errorf("expected the wrap-in-quotes hint in %q", msg)
	}

	// Float value renders without a spurious ".0".
	msg = fieldMessage(t, ValidateTypeFields(json.RawMessage(`{"gitlab_mr_iid":3.5}`), &TypeConfig{Fields: defs}), "gitlab_mr_iid")
	if !strings.Contains(msg, `"3.5"`) {
		t.Errorf("expected float example %q in %q", `"3.5"`, msg)
	}

	// Boolean value.
	msg = fieldMessage(t, ValidateTypeFields(json.RawMessage(`{"gitlab_mr_iid":true}`), &TypeConfig{Fields: defs}), "gitlab_mr_iid")
	if !strings.Contains(msg, `"true"`) || !strings.Contains(msg, "got a boolean") {
		t.Errorf("expected boolean example %q in %q", `"true"`, msg)
	}

	// Object/array/null get no scalar hint — the bare message stands.
	for _, body := range []string{`{"gitlab_mr_iid":{"a":1}}`, `{"gitlab_mr_iid":[1,2]}`} {
		msg = fieldMessage(t, ValidateTypeFields(json.RawMessage(body), &TypeConfig{Fields: defs}), "gitlab_mr_iid")
		if msg != "must be a string" {
			t.Errorf("expected bare message for %s, got %q", body, msg)
		}
	}
}

func TestValidateFields_IntegerType(t *testing.T) {
	defs := []FieldDef{{Name: "count", Type: FieldTypeInteger}}
	if err := ValidateTypeFields(json.RawMessage(`{"count":42}`), &TypeConfig{Fields: defs}); err != nil {
		t.Fatalf("expected pass for integer, got %v", err)
	}
	err := ValidateTypeFields(json.RawMessage(`{"count":3.14}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for float in integer field")
	}
	err = ValidateTypeFields(json.RawMessage(`{"count":"five"}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for string in integer field")
	}
}

func TestValidateFields_IntegerMinMax(t *testing.T) {
	one, five := 1, 5
	defs := []FieldDef{{Name: "score", Type: FieldTypeInteger, Min: &one, Max: &five}}

	for _, ok := range []int{1, 3, 5} {
		body := fmt.Appendf(nil, `{"score":%d}`, ok)
		if err := ValidateTypeFields(body, &TypeConfig{Fields: defs}); err != nil {
			t.Errorf("score=%d should be valid, got %v", ok, err)
		}
	}
	for _, bad := range []int{0, -1, 6, 100} {
		body := fmt.Appendf(nil, `{"score":%d}`, bad)
		if err := ValidateTypeFields(body, &TypeConfig{Fields: defs}); err == nil {
			t.Errorf("score=%d should fail min/max", bad)
		}
	}
}

func TestValidateFields_FloatType(t *testing.T) {
	defs := []FieldDef{{Name: "weight", Type: FieldTypeFloat}}
	if err := ValidateTypeFields(json.RawMessage(`{"weight":3.14}`), &TypeConfig{Fields: defs}); err != nil {
		t.Fatalf("expected pass for float, got %v", err)
	}
	if err := ValidateTypeFields(json.RawMessage(`{"weight":42}`), &TypeConfig{Fields: defs}); err != nil {
		t.Fatalf("expected pass for integer in float field, got %v", err)
	}
	err := ValidateTypeFields(json.RawMessage(`{"weight":"heavy"}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for string in float field")
	}
}

func TestValidateFields_EnumType(t *testing.T) {
	defs := []FieldDef{{Name: "priority", Type: FieldTypeEnum, Values: []string{"low", "medium", "high"}}}
	if err := ValidateTypeFields(json.RawMessage(`{"priority":"high"}`), &TypeConfig{Fields: defs}); err != nil {
		t.Fatalf("expected pass for valid enum, got %v", err)
	}
	err := ValidateTypeFields(json.RawMessage(`{"priority":"critical"}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for invalid enum value")
	}
	err = ValidateTypeFields(json.RawMessage(`{"priority":1}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for non-string enum")
	}
}

// TestValidateFields_TriStateEnum exercises the tri-state-bool enum shape used
// by type:project's session_resume field — {"true","false","auto"} — matching
// the controller parseTriBool path. The string literals "true"/"false" must be
// accepted as enum members (not coerced to JSON booleans), JSON booleans must be
// rejected (enum is a string type), and an out-of-set string must be rejected.
func TestValidateFields_TriStateEnum(t *testing.T) {
	defs := []FieldDef{{Name: "session_resume", Type: FieldTypeEnum, Values: []string{"true", "false", "auto"}}}
	for _, v := range []string{"true", "false", "auto"} {
		if err := ValidateTypeFields(json.RawMessage(`{"session_resume":"`+v+`"}`), &TypeConfig{Fields: defs}); err != nil {
			t.Errorf("expected pass for session_resume=%q, got %v", v, err)
		}
	}
	if err := ValidateTypeFields(json.RawMessage(`{"session_resume":"yes"}`), &TypeConfig{Fields: defs}); err == nil {
		t.Error("expected error for out-of-set session_resume value")
	}
	// A JSON boolean (not the string "true") must be rejected — the field is a
	// string enum, so the daemon should not silently accept a typed boolean.
	if err := ValidateTypeFields(json.RawMessage(`{"session_resume":true}`), &TypeConfig{Fields: defs}); err == nil {
		t.Error("expected error for JSON-boolean in string enum field")
	}
}

func TestValidateFields_StringArrayType(t *testing.T) {
	defs := []FieldDef{{Name: "tags", Type: FieldTypeStrings}}
	if err := ValidateTypeFields(json.RawMessage(`{"tags":["a","b"]}`), &TypeConfig{Fields: defs}); err != nil {
		t.Fatalf("expected pass for string array, got %v", err)
	}
	if err := ValidateTypeFields(json.RawMessage(`{"tags":[]}`), &TypeConfig{Fields: defs}); err != nil {
		t.Fatalf("expected pass for empty array, got %v", err)
	}
	err := ValidateTypeFields(json.RawMessage(`{"tags":[1,2]}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for non-string elements")
	}
	err = ValidateTypeFields(json.RawMessage(`{"tags":"single"}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for non-array")
	}
}

func TestValidateFields_EnumArrayType(t *testing.T) {
	defs := []FieldDef{{Name: "roles", Type: FieldTypeEnums, Values: []string{"admin", "user", "viewer"}}}
	if err := ValidateTypeFields(json.RawMessage(`{"roles":["admin","user"]}`), &TypeConfig{Fields: defs}); err != nil {
		t.Fatalf("expected pass for valid enum array, got %v", err)
	}
	err := ValidateTypeFields(json.RawMessage(`{"roles":["admin","superuser"]}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for invalid enum array value")
	}
}

func TestValidateFields_BooleanType(t *testing.T) {
	defs := []FieldDef{{Name: "active", Type: FieldTypeBoolean}}
	if err := ValidateTypeFields(json.RawMessage(`{"active":true}`), &TypeConfig{Fields: defs}); err != nil {
		t.Fatalf("expected pass for boolean true, got %v", err)
	}
	if err := ValidateTypeFields(json.RawMessage(`{"active":false}`), &TypeConfig{Fields: defs}); err != nil {
		t.Fatalf("expected pass for boolean false, got %v", err)
	}
	err := ValidateTypeFields(json.RawMessage(`{"active":"yes"}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for string in boolean field")
	}
	err = ValidateTypeFields(json.RawMessage(`{"active":1}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for number in boolean field")
	}
}

func TestValidateFields_TimestampType(t *testing.T) {
	defs := []FieldDef{{Name: "due", Type: FieldTypeTimestamp}}
	if err := ValidateTypeFields(json.RawMessage(`{"due":"2024-01-15T10:30:00Z"}`), &TypeConfig{Fields: defs}); err != nil {
		t.Fatalf("expected pass for valid RFC3339 timestamp, got %v", err)
	}
	err := ValidateTypeFields(json.RawMessage(`{"due":"not-a-timestamp"}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for invalid timestamp string")
	}
	err = ValidateTypeFields(json.RawMessage(`{"due":1234567890}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for number in timestamp field")
	}
}

func TestValidateFields_JSONType(t *testing.T) {
	defs := []FieldDef{{Name: "extra", Type: FieldTypeJSON}}
	if err := ValidateTypeFields(json.RawMessage(`{"extra":{"nested":"value"}}`), &TypeConfig{Fields: defs}); err != nil {
		t.Fatalf("expected pass for JSON object, got %v", err)
	}
	if err := ValidateTypeFields(json.RawMessage(`{"extra":42}`), &TypeConfig{Fields: defs}); err != nil {
		t.Fatalf("expected pass for JSON number, got %v", err)
	}
	if err := ValidateTypeFields(json.RawMessage(`{"extra":"string"}`), &TypeConfig{Fields: defs}); err != nil {
		t.Fatalf("expected pass for JSON string, got %v", err)
	}
}

func TestValidateFields_InvalidJSON(t *testing.T) {
	defs := []FieldDef{{Name: "x", Type: FieldTypeString}}
	err := ValidateTypeFields(json.RawMessage(`{not json}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestValidateFields_MultipleErrors(t *testing.T) {
	defs := []FieldDef{
		{Name: "name", Type: FieldTypeString, Required: true},
		{Name: "count", Type: FieldTypeInteger, Required: true},
	}
	err := ValidateTypeFields(json.RawMessage(`{}`), &TypeConfig{Fields: defs})
	if err == nil {
		t.Fatal("expected errors")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	if len(ve.Errors) != 2 {
		t.Fatalf("expected 2 errors, got %d", len(ve.Errors))
	}
}

func TestValidateTypeFields_NilTypeConfig(t *testing.T) {
	if err := ValidateTypeFields(nil, nil); err != nil {
		t.Fatalf("expected nil error for empty input + nil tc, got %v", err)
	}
	err := ValidateTypeFields(json.RawMessage(`{"k":"v"}`), nil)
	if err == nil {
		t.Fatal("expected error: nil tc behaves as closed schema with no fields")
	}
}

func TestValidateTypeFields_ClosedSchemaRejectsUnknown(t *testing.T) {
	tc := &TypeConfig{
		Kind:   KindConfig,
		Fields: []FieldDef{{Name: "value", Type: FieldTypeJSON}},
	}
	err := ValidateTypeFields(json.RawMessage(`{"COOP_QUEUED_WARN_AGE":"30s"}`), tc)
	if err == nil {
		t.Fatal("expected error for unknown field in closed schema")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	found := false
	for _, fe := range ve.Errors {
		// The message now grows a did-you-mean or valid-fields hint, so match
		// on the stable "unknown field" prefix rather than exact equality.
		if fe.Field == "COOP_QUEUED_WARN_AGE" && strings.HasPrefix(fe.Message, "unknown field") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected unknown-field error for COOP_QUEUED_WARN_AGE, got %v", ve.Errors)
	}
}

func TestValidateTypeFields_OpenSchemaAcceptsArbitraryKeys(t *testing.T) {
	tc := &TypeConfig{
		Kind:                  KindConfig,
		Fields:                []FieldDef{{Name: "value", Type: FieldTypeJSON}},
		AllowAdditionalFields: true,
	}
	// Mirrors the tunables hot-reload contract: env-var-name keys directly in Fields.
	body := json.RawMessage(`{"COOP_QUEUED_WARN_AGE":"30s","value":{"nested":"ok"},"OTHER_TUNABLE":42}`)
	if err := ValidateTypeFields(body, tc); err != nil {
		t.Fatalf("expected open-schema to accept arbitrary keys, got %v", err)
	}
}

func TestValidateTypeFieldsForUpdate_GrandfathersPreExistingUnknownKey(t *testing.T) {
	// Repro of ka-0000007952: a field (schedule_mirror_key) that was already
	// stored on the bead is not in the type config, but the update — which
	// merges-and-replaces all fields — must not be rejected just because
	// the prior write put an unknown key on the bead.
	tc := &TypeConfig{
		Kind: KindConfig,
		Fields: []FieldDef{
			{Name: "stop_requested", Type: FieldTypeString},
		},
	}
	body := json.RawMessage(`{"stop_requested":"true","schedule_mirror_key":"0:0:2026-05-25T01:43"}`)

	// Without grandfathering, this fails.
	if err := ValidateTypeFields(body, tc); err == nil {
		t.Fatal("expected unknown-field rejection without grandfather list (sanity check)")
	}

	// With schedule_mirror_key already present in the prior values, this passes.
	prior := map[string]any{"schedule_mirror_key": "0:0:2026-05-25T01:43"}
	if err := ValidateTypeFieldsForUpdate(body, tc, prior); err != nil {
		t.Fatalf("expected grandfathered key to pass, got %v", err)
	}
}

func TestValidateTypeFieldsForUpdate_StillRejectsNewlyAddedUnknown(t *testing.T) {
	// Grandfathering only covers keys that were already present. A truly new
	// unknown key in the update payload must still be rejected so typo-driven
	// field-name drift is still caught.
	tc := &TypeConfig{
		Kind: KindConfig,
		Fields: []FieldDef{
			{Name: "stop_requested", Type: FieldTypeString},
		},
	}
	body := json.RawMessage(`{"stop_requested":"true","schedule_mirror_key":"x","brand_new_key":"y"}`)

	prior := map[string]any{"schedule_mirror_key": "x"}
	err := ValidateTypeFieldsForUpdate(body, tc, prior)
	if err == nil {
		t.Fatal("expected error for newly-added unknown key brand_new_key")
	}
	ve, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("expected *ValidationError, got %T", err)
	}
	gotNew, gotGrandfathered := false, false
	for _, fe := range ve.Errors {
		if fe.Field == "brand_new_key" {
			gotNew = true
		}
		if fe.Field == "schedule_mirror_key" {
			gotGrandfathered = true
		}
	}
	if !gotNew {
		t.Errorf("expected error on brand_new_key, got %v", ve.Errors)
	}
	if gotGrandfathered {
		t.Errorf("schedule_mirror_key should be grandfathered, got error: %v", ve.Errors)
	}
}

func TestValidateTypeFieldsForUpdate_StillValidatesKnownFieldTypes(t *testing.T) {
	// Grandfathering must not weaken type validation on known fields.
	tc := &TypeConfig{
		Kind: KindConfig,
		Fields: []FieldDef{
			{Name: "yield_count", Type: FieldTypeInteger},
		},
	}
	// yield_count is a NEWLY-CHANGED value (prior had no yield_count), so even
	// on the update path the bad type is still rejected.
	body := json.RawMessage(`{"yield_count":"not-an-int","schedule_mirror_key":"x"}`)
	prior := map[string]any{"schedule_mirror_key": "x"}
	if err := ValidateTypeFieldsForUpdate(body, tc, prior); err == nil {
		t.Fatal("expected type error on yield_count even with grandfather list")
	}
}

func TestValidateTypeFieldsForUpdate_NilPriorBehavesLikeStrict(t *testing.T) {
	tc := &TypeConfig{
		Kind: KindConfig,
		Fields: []FieldDef{
			{Name: "stop_requested", Type: FieldTypeString},
		},
	}
	body := json.RawMessage(`{"stop_requested":"true","schedule_mirror_key":"x"}`)
	if err := ValidateTypeFieldsForUpdate(body, tc, nil); err == nil {
		t.Fatal("expected unknown-field rejection with nil prior values")
	}
}

// TestValidateTypeFieldsForUpdate_GrandfathersUnchangedWrongTypedValue is the
// direct repro of the ka-0000007257 wedge: a DEFINED field carries a legacy
// value in the wrong JSON type (a boolean where the registry says string). A
// PATCH that touches ONLY another field must not re-fail on the untouched bad
// value; a PATCH that CHANGES the bad field to a good string must succeed; a
// PATCH that changes it to another bad type must still be rejected.
func TestValidateTypeFieldsForUpdate_GrandfathersUnchangedWrongTypedValue(t *testing.T) {
	tc := &TypeConfig{
		Kind: KindConfig,
		Fields: []FieldDef{
			{Name: "enabled", Type: FieldTypeBoolean},
			{Name: "gardener_stuck_escalated", Type: FieldTypeString},
		},
	}
	// The stored state: gardener_stuck_escalated is a BOOLEAN false (wrong type),
	// enabled is true. This is exactly the two wedged prod trigger beads.
	prior := map[string]any{"gardener_stuck_escalated": false, "enabled": true}

	// A PATCH that only flips enabled=false re-merges the untouched boolean
	// gardener_stuck_escalated. Under the old strict path this 400'd; now the
	// unchanged bad value is grandfathered so the patch succeeds.
	patchOtherField := json.RawMessage(`{"gardener_stuck_escalated":false,"enabled":false}`)
	if err := ValidateTypeFieldsForUpdate(patchOtherField, tc, prior); err != nil {
		t.Fatalf("expected unchanged wrong-typed value to be grandfathered, got %v", err)
	}

	// The data-repair PATCH overwrites the boolean with the registry-correct
	// empty string — a CHANGED value with the right type, so it must pass.
	patchRepair := json.RawMessage(`{"gardener_stuck_escalated":"","enabled":true}`)
	if err := ValidateTypeFieldsForUpdate(patchRepair, tc, prior); err != nil {
		t.Fatalf("expected string repair value to pass, got %v", err)
	}

	// Changing the field to ANOTHER wrong type (a number) is a CHANGED value and
	// must still be rejected — grandfathering only covers the unchanged value.
	patchStillBad := json.RawMessage(`{"gardener_stuck_escalated":42,"enabled":true}`)
	if err := ValidateTypeFieldsForUpdate(patchStillBad, tc, prior); err == nil {
		t.Fatal("expected a CHANGED wrong-typed value to still be rejected")
	}
}

func TestValidateTypeFields_OpenSchemaStillValidatesKnownTypes(t *testing.T) {
	// Even when AllowAdditionalFields is true, declared fields must still
	// pass type checks — open schema relaxes the unknown-key rejection
	// only, not type validation on listed fields.
	tc := &TypeConfig{
		Kind:                  KindConfig,
		Fields:                []FieldDef{{Name: "value", Type: FieldTypeInteger}},
		AllowAdditionalFields: true,
	}
	if err := ValidateTypeFields(json.RawMessage(`{"value":"not-an-int","FOO":"bar"}`), tc); err == nil {
		t.Fatal("expected type error on declared field even with open schema")
	}
}

// A strict type that never declared idempotency_key still accepts the key the
// daemon stores on a keyed create, as a string only; other unknown keys are
// still refused.
func TestValidateFields_IdempotencyKeyOnEveryType(t *testing.T) {
	tc := &TypeConfig{TypeName: "project", Fields: []FieldDef{{Name: "prefix", Type: FieldTypeString}}}
	if err := ValidateTypeFields(json.RawMessage(`{"prefix":"lands","idempotency_key":"lands-provision:gd-x:project"}`), tc); err != nil {
		t.Fatalf("keyed create on a strict type: %v", err)
	}
	if err := ValidateTypeFields(json.RawMessage(`{"idempotency_key":7}`), tc); err == nil {
		t.Fatal("a non-string idempotency_key passed")
	}
	if err := ValidateTypeFields(json.RawMessage(`{"bogus":"x"}`), tc); err == nil {
		t.Fatal("an unknown field passed")
	}
	if len(tc.Fields) != 1 {
		t.Fatalf("the type's own field list grew to %d", len(tc.Fields))
	}
}
