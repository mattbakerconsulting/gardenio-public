package bridge

import (
	"strings"
	"testing"
)

func TestRouter_Resolve_Default(t *testing.T) {
	r := NewRouter(RouterConfig{DefaultChannel: "C-default"})
	result := r.Resolve("gardenio/crew/test-bot")
	if result.ChannelID != "C-default" {
		t.Errorf("got %q, want C-default", result.ChannelID)
	}
	if !result.IsDefault {
		t.Error("expected IsDefault=true")
	}
}

func TestRouter_Resolve_ExactPattern(t *testing.T) {
	r := NewRouter(RouterConfig{
		DefaultChannel: "C-default",
		Channels: map[string]string{
			"gardenio/crew/test-bot": "C-exact",
		},
	})
	result := r.Resolve("gardenio/crew/test-bot")
	if result.ChannelID != "C-exact" {
		t.Errorf("got %q, want C-exact", result.ChannelID)
	}
}

func TestRouter_Resolve_WildcardPattern(t *testing.T) {
	r := NewRouter(RouterConfig{
		DefaultChannel: "C-default",
		Channels: map[string]string{
			"gardenio/crew/*": "C-crew",
		},
	})
	result := r.Resolve("gardenio/crew/test-bot")
	if result.ChannelID != "C-crew" {
		t.Errorf("got %q, want C-crew", result.ChannelID)
	}
}

func TestRouter_Resolve_OverrideTakesPrecedence(t *testing.T) {
	r := NewRouter(RouterConfig{
		DefaultChannel: "C-default",
		Channels: map[string]string{
			"gardenio/crew/*": "C-crew",
		},
		Overrides: map[string]string{
			"gardenio/crew/test-bot": "C-breakout",
		},
	})
	result := r.Resolve("gardenio/crew/test-bot")
	if result.ChannelID != "C-breakout" {
		t.Errorf("got %q, want C-breakout (override)", result.ChannelID)
	}
	if result.MatchedBy != "(override)" {
		t.Errorf("got matchedBy %q, want (override)", result.MatchedBy)
	}
}

func TestRouter_Resolve_SpecificityOrdering(t *testing.T) {
	r := NewRouter(RouterConfig{
		DefaultChannel: "C-default",
		Channels: map[string]string{
			"*/*":             "C-catchall",
			"gardenio/*":      "C-gardenio",
			"gardenio/crew/*": "C-crew",
		},
	})

	// 3-segment agent should match the most specific 3-segment pattern.
	result := r.Resolve("gardenio/crew/bot")
	if result.ChannelID != "C-crew" {
		t.Errorf("got %q, want C-crew (3-segment match)", result.ChannelID)
	}

	// 2-segment agent should match "gardenio/*" over "*/*".
	result = r.Resolve("gardenio/standalone")
	if result.ChannelID != "C-gardenio" {
		t.Errorf("got %q, want C-gardenio (2-segment match)", result.ChannelID)
	}

	// Unmatched segment count falls to default.
	result = r.Resolve("other/project/agent/extra")
	if result.ChannelID != "C-default" {
		t.Errorf("got %q, want C-default (no match)", result.ChannelID)
	}
}

func TestRouter_OverrideCRUD(t *testing.T) {
	r := NewRouter(RouterConfig{DefaultChannel: "C-default"})

	if r.HasOverride("agent-a") {
		t.Error("expected no override initially")
	}

	r.AddOverride("agent-a", "C-breakout")
	if !r.HasOverride("agent-a") {
		t.Error("expected override after add")
	}

	result := r.Resolve("agent-a")
	if result.ChannelID != "C-breakout" {
		t.Errorf("got %q, want C-breakout", result.ChannelID)
	}

	if got := r.GetAgentByChannel("C-breakout"); got != "agent-a" {
		t.Errorf("reverse lookup got %q, want agent-a", got)
	}

	r.RemoveOverride("agent-a")
	if r.HasOverride("agent-a") {
		t.Error("expected no override after remove")
	}
}

func TestMatchPattern(t *testing.T) {
	tests := []struct {
		pattern string
		agent   string
		want    bool
	}{
		{"gardenio/crew/*", "gardenio/crew/bot", true},
		{"gardenio/crew/*", "gardenio/crew/other", true},
		{"gardenio/crew/*", "beads/crew/bot", false},
		{"*/crew/*", "gardenio/crew/bot", true},
		{"*/crew/*", "beads/crew/bot", true},
		{"*/*", "gardenio/crew", true},
		{"*/*", "gardenio/crew/bot", false}, // wrong segment count
		{"gardenio/crew/bot", "gardenio/crew/bot", true},
		{"gardenio/crew/bot", "gardenio/crew/other", false},
	}
	for _, tt := range tests {
		got := matchPattern(strings.Split(tt.pattern, "/"), strings.Split(tt.agent, "/"))
		if got != tt.want {
			t.Errorf("matchPattern(%q, %q) = %v, want %v", tt.pattern, tt.agent, got, tt.want)
		}
	}
}

func TestRouter_ResolveIdentity(t *testing.T) {
	r := NewRouter(RouterConfig{
		DefaultChannel: "C-default",
		Channels:       map[string]string{"gardenio/crew/*": "C-crew", "solo": "C-solo"},
		Overrides: map[string]string{
			"gardenio/crew/hq": "C-hq",
			"broken-out":       "C-break",
		},
	})
	cases := []struct {
		project, role, name string
		wantCh, wantBy      string
	}{
		{"gardenio", "crew", "test-bot", "C-crew", "gardenio/crew/*"},
		{"gardenio", "crew", "hq", "C-hq", "(override)"},
		{"gardenio", "crew", "broken-out", "C-break", "(override)"},
		{"gardenio", "captain", "test-bot", "C-default", "(default)"},
		// Project or role unknown: the short name alone.
		{"", "crew", "test-bot", "C-default", "(default)"},
		{"gardenio", "", "solo", "C-solo", "solo"},
		{"", "", "broken-out", "C-break", "(override)"},
		// A name that is already a path is not prefixed again.
		{"gardenio", "crew", "gardenio/crew/hq", "C-hq", "(override)"},
	}
	for _, tc := range cases {
		got := r.ResolveIdentity(tc.project, tc.role, tc.name)
		if got.ChannelID != tc.wantCh || got.MatchedBy != tc.wantBy {
			t.Errorf("ResolveIdentity(%q, %q, %q) = %s by %s, want %s by %s",
				tc.project, tc.role, tc.name, got.ChannelID, got.MatchedBy, tc.wantCh, tc.wantBy)
		}
	}
}
