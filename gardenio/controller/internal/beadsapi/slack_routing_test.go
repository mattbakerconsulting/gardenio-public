package beadsapi

import "testing"

func TestCopySlackRoutingFields(t *testing.T) {
	src := map[string]string{"project": "gardenio", "role": "crew"}
	for i, k := range SlackRoutingFields {
		src[k] = "v" + string(rune('0'+i))
	}
	dst := map[string]string{}
	CopySlackRoutingFields(dst, src)
	if len(dst) != len(SlackRoutingFields) {
		t.Errorf("copied %d fields, want %d (routing fields only): %v", len(dst), len(SlackRoutingFields), dst)
	}
	for _, k := range SlackRoutingFields {
		if dst[k] != src[k] {
			t.Errorf("%s = %q, want %q", k, dst[k], src[k])
		}
	}

	empty := map[string]string{}
	CopySlackRoutingFields(empty, map[string]string{"project": "gardenio", "slack_thread_ts": ""})
	if len(empty) != 0 {
		t.Errorf("no non-empty routing fields must copy none, got %v", empty)
	}
}
