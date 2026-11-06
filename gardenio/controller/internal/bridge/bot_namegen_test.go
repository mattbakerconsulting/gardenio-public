package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestTruncateTitle(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		maxLen int
		want   string
	}{
		{
			name:   "short text unchanged",
			input:  "fix the login bug",
			maxLen: 500,
			want:   "fix the login bug",
		},
		{
			name:   "exactly at limit",
			input:  strings.Repeat("a", 500),
			maxLen: 500,
			want:   strings.Repeat("a", 500),
		},
		{
			name:   "truncates at word boundary",
			input:  "fix the login bug " + strings.Repeat("x", 500),
			maxLen: 50,
			want:   "fix the login bug " + strings.Repeat("x", 29) + "...",
		},
		{
			name:   "single long word",
			input:  strings.Repeat("a", 600),
			maxLen: 500,
			want:   strings.Repeat("a", 497) + "...",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateTitle(tc.input, tc.maxLen)
			if len([]rune(got)) > tc.maxLen {
				t.Errorf("result length %d exceeds max %d", len([]rune(got)), tc.maxLen)
			}
			if got != tc.want {
				if len(got) > 60 {
					t.Errorf("truncateTitle() len=%d, want len=%d", len(got), len(tc.want))
				} else {
					t.Errorf("truncateTitle() = %q, want %q", got, tc.want)
				}
			}
		})
	}
}

// mockNameGen is a test implementation of NameGenerator.
type mockNameGen struct {
	slug  string
	title string
	err   error
}

func (m *mockNameGen) GenerateName(_ context.Context, _ string) (string, string, error) {
	return m.slug, m.title, m.err
}

func TestGenerateAgentNameLLM_WithNameGen(t *testing.T) {
	bot := &Bot{
		nameGen: &mockNameGen{slug: "fix-auth-middleware", title: "Fix auth middleware nil pointer"},
		logger:  slog.Default(),
	}

	name, title := bot.generateAgentNameLLM(context.Background(), "gardenio", "fix the authentication middleware because it has a nil pointer dereference when the session token is expired")
	if !strings.HasPrefix(name, "gardenio-fix-auth-middleware-") {
		t.Errorf("name = %q, want prefix 'gardenio-fix-auth-middleware-'", name)
	}
	if title != "Fix auth middleware nil pointer" {
		t.Errorf("title = %q, want 'Fix auth middleware nil pointer'", title)
	}
}

func TestGenerateAgentNameLLM_NoProject(t *testing.T) {
	bot := &Bot{
		nameGen: &mockNameGen{slug: "fix-auth", title: "Fix auth"},
		logger:  slog.Default(),
	}

	name, _ := bot.generateAgentNameLLM(context.Background(), "", "fix auth")
	if !strings.HasPrefix(name, "fix-auth-") {
		t.Errorf("name = %q, want prefix 'fix-auth-'", name)
	}
}

func TestGenerateAgentNameLLM_FallbackOnError(t *testing.T) {
	bot := &Bot{
		nameGen: &mockNameGen{err: fmt.Errorf("API error")},
		logger:  slog.Default(),
	}

	name, title := bot.generateAgentNameLLM(context.Background(), "gardenio", "fix the login bug")
	if !strings.HasPrefix(name, "gardenio-fix-the-login-") {
		t.Errorf("name = %q, want mechanical fallback prefix 'gardenio-fix-the-login-'", name)
	}
	if title != "" {
		t.Errorf("title should be empty on fallback, got %q", title)
	}
}

func TestGenerateAgentNameLLM_NilNameGen(t *testing.T) {
	bot := &Bot{
		nameGen: nil,
		logger:  slog.Default(),
	}

	name, title := bot.generateAgentNameLLM(context.Background(), "gardenio", "fix the login bug")
	if !strings.HasPrefix(name, "gardenio-fix-the-login-") {
		t.Errorf("name = %q, want mechanical fallback prefix 'gardenio-fix-the-login-'", name)
	}
	if title != "" {
		t.Errorf("title should be empty without nameGen, got %q", title)
	}
}
