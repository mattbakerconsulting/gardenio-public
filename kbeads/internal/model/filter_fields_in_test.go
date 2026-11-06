package model

import (
	"reflect"
	"testing"
)

// ParseFieldsIn splits on commas, adds up a repeated key, and refuses a key or
// value left empty (gd-OYKXtzN9kr).
func TestParseFieldsIn(t *testing.T) {
	got, err := ParseFieldsIn([]string{"channel_id=c1, c2", "channel_id=c3", "thread_id=t1"})
	want := map[string][]string{"channel_id": {"c1", "c2", "c3"}, "thread_id": {"t1"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("ParseFieldsIn = %v, %v; want %v", got, err, want)
	}
	if got, err := ParseFieldsIn(nil); got != nil || err != nil {
		t.Errorf("ParseFieldsIn(nil) = %v, %v; want nil, nil", got, err)
	}
	for _, bad := range []string{"channel_id", "channel_id=", "channel_id=,", "=c1"} {
		if _, err := ParseFieldsIn([]string{bad}); err == nil {
			t.Errorf("ParseFieldsIn(%q): no error", bad)
		}
	}
}
