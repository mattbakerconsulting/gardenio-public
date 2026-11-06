package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFieldKeyCaseErrors(t *testing.T) {
	defs := []FieldDef{{Name: "author", Type: FieldTypeString}, {Name: "land_kind", Type: FieldTypeString}}
	for _, c := range []struct {
		name   string
		fields string
		prior  string
		want   []string // keys refused
	}{
		{"exact keys", `{"author":"a","land_kind":"tenant","x":1}`, "", nil},
		{"author/Author", `{"author":"a","Author":"b"}`, "", []string{"Author", "author"}},
		{"Author alone", `{"Author":"b"}`, "", []string{"Author"}},
		{"Land_kind alone", `{"Land_kind":"operator"}`, "", []string{"Land_kind"}},
		{"undeclared pair", `{"note":"a","NOTE":"b"}`, "", []string{"NOTE", "note"}},
		{"three-way", `{"note":"a","Note":"b","NOTE":"c"}`, "", []string{"NOTE", "Note", "note"}},
		{"Kelvin sign folds to k", `{"kind":"a","Kind":"b"}`, "", []string{"kind", "Kind"}},
		{"long s folds to s", `{"ſource":"a","source":"b"}`, "", []string{"source", "ſource"}},
		{"stored pair unchanged", `{"note":"a","Note":"b","x":2}`, `{"note":"a","Note":"b","x":1}`, nil},
		{"stored pair, one side changed", `{"note":"a","Note":"c"}`, `{"note":"a","Note":"b"}`, []string{"Note", "note"}},
		{"new variant of a stored key", `{"note":"a","Note":"b"}`, `{"note":"a"}`, []string{"Note", "note"}},
		{"stored declared variant unchanged", `{"Author":"b"}`, `{"Author":"b"}`, nil},
		{"stored declared variant changed", `{"Author":"c"}`, `{"Author":"b"}`, []string{"Author"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var m, prior map[string]any
			if err := json.Unmarshal([]byte(c.fields), &m); err != nil {
				t.Fatal(err)
			}
			if c.prior != "" {
				if err := json.Unmarshal([]byte(c.prior), &prior); err != nil {
					t.Fatal(err)
				}
			}
			var got []string
			for _, fe := range FieldKeyCaseErrors(m, defs, prior) {
				got = append(got, fe.Field)
			}
			if strings.Join(got, ",") != strings.Join(c.want, ",") {
				t.Errorf("refused %v, want %v", got, c.want)
			}
		})
	}
}

// The create-time choke point reports a declared field's case variant once,
// as a case error rather than an unknown field.
func TestValidateTypeFields_CaseVariant(t *testing.T) {
	tc := &TypeConfig{TypeName: "message", Fields: []FieldDef{{Name: "author", Type: FieldTypeString}}}
	err := ValidateTypeFields(json.RawMessage(`{"Author":"x"}`), tc)
	ve, ok := err.(*ValidationError)
	if !ok || len(ve.Errors) != 1 || !strings.Contains(ve.Errors[0].Message, `case-folds to the declared field "author"`) {
		t.Fatalf("err = %v, want one case-variant error", err)
	}
	open := &TypeConfig{TypeName: "open", AllowAdditionalFields: true}
	if err := ValidateTypeFields(json.RawMessage(`{"a":1,"A":2}`), open); err == nil {
		t.Fatal("open schema accepted a case-variant pair")
	}
	if err := ValidateTypeFieldsForUpdate(json.RawMessage(`{"a":1,"A":2,"b":3}`), open, map[string]any{"a": 1.0, "A": 2.0}); err != nil {
		t.Fatalf("stored pair untouched: %v", err)
	}
}
