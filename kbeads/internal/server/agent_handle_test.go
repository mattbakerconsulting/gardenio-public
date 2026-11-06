package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

func TestSlugifyHandlePart(t *testing.T) {
	cases := []struct{ in, want string }{
		{"unknown", "unknown"},
		{"Slack", "slack"},
		{"synthetic-gardener:slack", "synthetic-gardener-slack"}, // prefix not stripped here
		{"ka-0000007129", "ka-0000007129"},
		{"  spaced  name  ", "spaced-name"},
		{"a__b--c", "a-b-c"},
		{"!!!", ""},
		{"", ""},
		{"UPPER_snake.Case", "upper-snake-case"},
	}
	for _, c := range cases {
		if got := slugifyHandlePart(c.in); got != c.want {
			t.Errorf("slugifyHandlePart(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSlugifyHandlePartCapsLength(t *testing.T) {
	got := slugifyHandlePart(strings.Repeat("a", 100))
	if len(got) > agentHandlePrefixMaxLen {
		t.Fatalf("slug len %d exceeds cap %d: %q", len(got), agentHandlePrefixMaxLen, got)
	}
}

func TestHandlePrefixFromGardenerID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"synthetic-gardener:unknown", "unknown"},
		{"synthetic-gardener:slack", "slack"},
		{"synthetic-gardener:", agentHandleFallbackPrefix},
		{"gd-PBPTLlRY3G", "gd-pbptllry3g"},
		{"", agentHandleFallbackPrefix},
		{"::::", agentHandleFallbackPrefix},
	}
	for _, c := range cases {
		if got := handlePrefixFromGardenerID(c.in); got != c.want {
			t.Errorf("handlePrefixFromGardenerID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatAgentHandle(t *testing.T) {
	cases := []struct {
		prefix string
		seq    int64
		want   string
	}{
		{"unknown", 1, "unknown-1"},
		{"slack", 42, "slack-42"},
		{"", 3, agentHandleFallbackPrefix + "-3"},
		{"!!!", 7, agentHandleFallbackPrefix + "-7"},
		{"Gardener Fun", 9, "gardener-fun-9"},
	}
	for _, c := range cases {
		if got := formatAgentHandle(c.prefix, c.seq); got != c.want {
			t.Errorf("formatAgentHandle(%q, %d) = %q, want %q", c.prefix, c.seq, got, c.want)
		}
	}
}

func agentBead(fields map[string]string) *model.Bead {
	raw, _ := json.Marshal(fields)
	return &model.Bead{Type: "agent", Fields: json.RawMessage(raw)}
}

func fieldVal(t *testing.T, b *model.Bead, key string) string {
	t.Helper()
	return parseFieldsToStringMap(b.Fields)[key]
}

func TestMaybeAllocateAgentHandle_AllocatesForGardener(t *testing.T) {
	s := &BeadsServer{store: &mockStore{}}
	b := agentBead(map[string]string{"gardener_id": "synthetic-gardener:unknown"})
	s.maybeAllocateAgentHandle(context.Background(), b)
	if got := fieldVal(t, b, agentHandleField); got != "unknown-1" {
		t.Fatalf("agent_handle = %q, want unknown-1", got)
	}
}

func TestMaybeAllocateAgentHandle_MonotonicPerGardener(t *testing.T) {
	s := &BeadsServer{store: &mockStore{}}
	ctx := context.Background()
	var handles []string
	for i := 0; i < 3; i++ {
		b := agentBead(map[string]string{"gardener_id": "synthetic-gardener:slack"})
		s.maybeAllocateAgentHandle(ctx, b)
		handles = append(handles, fieldVal(t, b, agentHandleField))
	}
	want := []string{"slack-1", "slack-2", "slack-3"}
	for i := range want {
		if handles[i] != want[i] {
			t.Fatalf("handles = %v, want %v", handles, want)
		}
	}
}

func TestMaybeAllocateAgentHandle_HonorsExplicitPrefix(t *testing.T) {
	s := &BeadsServer{store: &mockStore{}}
	b := agentBead(map[string]string{
		"gardener_id":         "synthetic-gardener:unknown",
		"agent_handle_prefix": "release-gardener",
	})
	s.maybeAllocateAgentHandle(context.Background(), b)
	if got := fieldVal(t, b, agentHandleField); got != "release-gardener-1" {
		t.Fatalf("agent_handle = %q, want release-gardener-1", got)
	}
}

func TestMaybeAllocateAgentHandle_SkipsWhenAlreadySet(t *testing.T) {
	s := &BeadsServer{store: &mockStore{}}
	b := agentBead(map[string]string{
		"gardener_id":  "synthetic-gardener:unknown",
		"agent_handle": "unknown-99",
	})
	s.maybeAllocateAgentHandle(context.Background(), b)
	if got := fieldVal(t, b, agentHandleField); got != "unknown-99" {
		t.Fatalf("agent_handle = %q, want preserved unknown-99", got)
	}
}

func TestMaybeAllocateAgentHandle_SkipsWithoutGardener(t *testing.T) {
	s := &BeadsServer{store: &mockStore{}}
	b := agentBead(map[string]string{"agent": "some-name"})
	s.maybeAllocateAgentHandle(context.Background(), b)
	if got := fieldVal(t, b, agentHandleField); got != "" {
		t.Fatalf("agent_handle = %q, want empty (no gardener)", got)
	}
}

func TestMaybeAllocateAgentHandle_SkipsNonAgentType(t *testing.T) {
	s := &BeadsServer{store: &mockStore{}}
	raw, _ := json.Marshal(map[string]string{"gardener_id": "synthetic-gardener:unknown"})
	b := &model.Bead{Type: "task", Fields: json.RawMessage(raw)}
	s.maybeAllocateAgentHandle(context.Background(), b)
	if got := fieldVal(t, b, agentHandleField); got != "" {
		t.Fatalf("agent_handle = %q, want empty (non-agent)", got)
	}
}
