package server

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestWithDerivedAgentLabels(t *testing.T) {
	cases := []struct {
		name   string
		labels []string
		fields string
		want   []string
	}{
		{"no labels: both derived", nil, `{"project":"gardenio","role":"infra"}`,
			[]string{"project:gardenio", "role:infra"}},
		{"project label kept, role added", []string{"project:other"}, `{"project":"gardenio","role":"infra"}`,
			[]string{"project:other", "role:infra"}},
		{"existing labels not duplicated", []string{"project:gardenio", "role:infra"}, `{"project":"gardenio","role":"infra"}`,
			[]string{"project:gardenio", "role:infra"}},
		{"comma roles split and trimmed", nil, `{"project":"gardenio","role":"thread, crew"}`,
			[]string{"project:gardenio", "role:thread", "role:crew"}},
		{"empty fields add nothing", []string{"x"}, `{"project":"","role":" "}`, []string{"x"}},
		{"no fields blob", []string{"x"}, ``, []string{"x"}},
		{"unparseable fields", nil, `not json`, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := withDerivedAgentLabels(tc.labels, json.RawMessage(tc.fields))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("withDerivedAgentLabels(%v, %s) = %v, want %v", tc.labels, tc.fields, got, tc.want)
			}
		})
	}
}
