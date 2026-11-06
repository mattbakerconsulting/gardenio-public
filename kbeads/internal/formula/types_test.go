package formula

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestParseRange(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantStart int
		wantEnd   int
		wantErr   bool
	}{
		{"simple", "1..3", 1, 3, false},
		{"single", "5..5", 5, 5, false},
		{"with spaces", " 2 .. 4 ", 2, 4, false},
		{"negative start", "-2..2", -2, 2, false},
		{"missing separator", "13", 0, 0, true},
		{"non-numeric start", "a..3", 0, 0, true},
		{"non-numeric end", "1..z", 0, 0, true},
		{"reversed", "5..1", 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, err := ParseRange(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseRange(%q) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if start != tt.wantStart || end != tt.wantEnd {
				t.Errorf("ParseRange(%q) = (%d, %d), want (%d, %d)", tt.in, start, end, tt.wantStart, tt.wantEnd)
			}
		})
	}
}

func TestVarDefListUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    VarDefList
		wantErr bool
	}{
		{
			"native array",
			`[{"name":"a","required":true}]`,
			VarDefList{{Name: "a", Required: true}},
			false,
		},
		{
			"legacy string-encoded array",
			`"[{\"name\":\"b\"}]"`,
			VarDefList{{Name: "b"}},
			false,
		},
		{"empty string", `""`, nil, false},
		{"null", `null`, nil, false},
		{"string null", `"null"`, nil, false},
		{"malformed array", `[{`, nil, true},
		{"string but not array payload", `"not json"`, nil, true},
		{"neither array nor string", `123`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var v VarDefList
			err := json.Unmarshal([]byte(tt.in), &v)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Unmarshal(%s) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(v, tt.want) {
				t.Errorf("Unmarshal(%s) = %+v, want %+v", tt.in, v, tt.want)
			}
		})
	}
}

// A double-encoded VarDefList should marshal back to a native array so that
// re-writing a tolerated bead normalizes it.
func TestVarDefListNormalizesOnRemarshal(t *testing.T) {
	var v VarDefList
	if err := json.Unmarshal([]byte(`"[{\"name\":\"a\"}]"`), &v); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(out) != `[{"name":"a"}]` {
		t.Errorf("re-marshal = %s, want native array", out)
	}
}

func TestStepListUnmarshalJSON(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    StepList
		wantErr bool
	}{
		{
			"native array",
			`[{"id":"s1","title":"T"}]`,
			StepList{{ID: "s1", Title: "T"}},
			false,
		},
		{
			"legacy string-encoded array",
			`"[{\"id\":\"s2\"}]"`,
			StepList{{ID: "s2"}},
			false,
		},
		{"empty string", `""`, nil, false},
		{"null", `null`, nil, false},
		{"string null", `"null"`, nil, false},
		{"malformed array", `[{`, nil, true},
		{"string but not array payload", `"nope"`, nil, true},
		{"neither array nor string", `42`, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var s StepList
			err := json.Unmarshal([]byte(tt.in), &s)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Unmarshal(%s) err = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(s, tt.want) {
				t.Errorf("Unmarshal(%s) = %+v, want %+v", tt.in, s, tt.want)
			}
		})
	}
}

// Fields should parse using the tolerant list types for both vars and steps,
// including the legacy double-encoded string forms.
func TestFieldsParsesTolerantForms(t *testing.T) {
	raw := `{
		"formula_type": "workflow",
		"vars": "[{\"name\":\"env\",\"required\":true}]",
		"steps": "[{\"id\":\"s1\",\"title\":\"Build {{env}}\"}]"
	}`
	var f Fields
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		t.Fatalf("unmarshal Fields: %v", err)
	}
	if len(f.Vars) != 1 || f.Vars[0].Name != "env" || !f.Vars[0].Required {
		t.Errorf("Vars = %+v, want one required env var", f.Vars)
	}
	if len(f.Steps) != 1 || f.Steps[0].ID != "s1" {
		t.Errorf("Steps = %+v, want one step s1", f.Steps)
	}
}
