package config

import (
	"reflect"
	"testing"
)

func TestParseKeyValuePairs(t *testing.T) {
	cases := map[string]map[string]string{
		"":                                      nil,
		"gardenio-slack-link=gardenio-operator": {"gardenio-slack-link": "gardenio-operator"},
		" a=b , c=d ,=x, y=, nopair ":           {"a": "b", "c": "d"},
		"a=b,a=c":                               {"a": "c"},
	}
	for in, want := range cases {
		if got := parseKeyValuePairs(in); !reflect.DeepEqual(got, want) {
			t.Errorf("parseKeyValuePairs(%q) = %v, want %v", in, got, want)
		}
	}
}
