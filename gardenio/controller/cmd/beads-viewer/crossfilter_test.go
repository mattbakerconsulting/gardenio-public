package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParseCrossFilter_BasicAndRoundTrip(t *testing.T) {
	cases := []struct {
		raw  string
		want string // canonical String() form
	}{
		{"", ""},
		{"   ", ""},
		{"service:study-web", "service:study-web"},
		{"service:study-web,severity:p1", "service:study-web,severity:p1"},
		// duplicate values dedup
		{"service:study-web,service:study-web", "service:study-web"},
		// unsorted -> sorted by key, then by value
		{"severity:p1,service:b,service:a", "service:a,service:b,severity:p1"},
		// malformed pairs dropped
		{"service:study-web,bogus,severity:", "service:study-web"},
		// percent-encoded round trip
		{"service%3Astudy-web%2Cseverity%3Ap1", "service:study-web,severity:p1"},
	}
	for _, tc := range cases {
		f := parseCrossFilter(tc.raw)
		got := f.String()
		if got != tc.want {
			t.Errorf("parseCrossFilter(%q).String() = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestCrossFilter_MatchesAny(t *testing.T) {
	f := parseCrossFilter("service:study-web,service:sponsor-api,source-kind:mezmo")
	// Empty filter always matches.
	var empty crossFilter
	if !empty.matchesAny("service", "anything") {
		t.Error("empty filter should always match")
	}
	// Absent key always matches.
	if !f.matchesAny("severity", "p1") {
		t.Error("filter without 'severity' key should match")
	}
	// Present key matches one of the listed values.
	if !f.matchesAny("service", "study-web") {
		t.Error("study-web should match")
	}
	if !f.matchesAny("service", "Study-Web") {
		t.Error("matchesAny should be case-insensitive")
	}
	if f.matchesAny("service", "site-api") {
		t.Error("site-api should NOT match")
	}
	// Multiple candidates: any-match.
	if !f.matchesAny("service", "site-api", "sponsor-api") {
		t.Error("at least one candidate should match")
	}
	// Whitespace trimmed.
	if !f.matchesAny("service", "  study-web  ") {
		t.Error("matchesAny should trim whitespace")
	}
}

func TestApplyServerCrossFilter_TimelineDropsNonMatching(t *testing.T) {
	payload := `{"events":[
		{"when":"2026-05-06T12:00:00Z","kind":"mezmo","title":"a"},
		{"when":"2026-05-06T12:01:00Z","kind":"jira","title":"b"},
		{"when":"2026-05-06T12:02:00Z","kind":"gitlab","title":"c"},
		{"when":"2026-05-06T12:03:00Z","kind":"mezmo","title":"d"}
	]}`
	out := applyServerCrossFilter(payload, "event-timeline", "source-kind:mezmo")
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	events, _ := got["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("want 2 mezmo events, got %d in:\n%s", len(events), out)
	}
}

func TestApplyServerCrossFilter_TimelineMultiKey(t *testing.T) {
	payload := `{"events":[
		{"when":"2026-05-06T12:00:00Z","kind":"mezmo","service":"study-web","title":"a"},
		{"when":"2026-05-06T12:01:00Z","kind":"mezmo","service":"sponsor-api","title":"b"},
		{"when":"2026-05-06T12:02:00Z","kind":"jira","service":"study-web","title":"c"}
	]}`
	out := applyServerCrossFilter(payload, "event-timeline", "source-kind:mezmo,service:study-web")
	if !strings.Contains(out, `"title":"a"`) {
		t.Errorf("expected event a, got: %s", out)
	}
	if strings.Contains(out, `"title":"b"`) || strings.Contains(out, `"title":"c"`) {
		t.Errorf("expected b/c filtered out, got: %s", out)
	}
}

func TestApplyServerCrossFilter_NonTimelineUnchanged(t *testing.T) {
	payload := `{"services":[{"service":"study-web"}]}`
	out := applyServerCrossFilter(payload, "service-health", "service:sponsor-api")
	if out != payload {
		t.Errorf("non-timeline slugs should pass through unchanged; got: %s", out)
	}
}

func TestApplyServerCrossFilter_EmptyFilterPasses(t *testing.T) {
	payload := `{"events":[{"when":"2026-05-06T12:00:00Z","kind":"mezmo"}]}`
	out := applyServerCrossFilter(payload, "event-timeline", "")
	if out != payload {
		t.Errorf("empty filter should pass payload unchanged")
	}
}

func TestApplyServerCrossFilter_MalformedPayloadPasses(t *testing.T) {
	payload := `not json`
	out := applyServerCrossFilter(payload, "event-timeline", "source-kind:mezmo")
	if out != payload {
		t.Errorf("malformed payload should pass through unchanged")
	}
}

func TestFilterTimelinePayload_KeepsTopLevelMetadata(t *testing.T) {
	payload := `{"title":"Tl","group_id":"g1","events":[
		{"when":"2026-05-06T12:00:00Z","kind":"mezmo","title":"a"},
		{"when":"2026-05-06T12:01:00Z","kind":"jira","title":"b"}
	]}`
	out := filterTimelinePayload(payload, parseCrossFilter("source-kind:mezmo"))
	if !strings.Contains(out, `"title":"Tl"`) {
		t.Errorf("expected title preserved: %s", out)
	}
	if !strings.Contains(out, `"group_id":"g1"`) {
		t.Errorf("expected group_id preserved: %s", out)
	}
}
