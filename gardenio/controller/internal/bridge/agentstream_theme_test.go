package bridge

import (
	"context"
	"strings"
	"testing"
	"time"
)

// themedStatusSetter is a ThreadStatusSetter that also resolves a fixed theme,
// the way *Bot does in production, so the stream manager's themeFor picks it up.
type themedStatusSetter struct {
	mockThreadStatusSetter
	theme Theme
}

func (s *themedStatusSetter) themeFor(context.Context, string) Theme { return s.theme }

// TestBuildStopSummary_Business pins the Business rendering of every glyph the
// stop summary resolves through the theme: bracket outcome tags, the [REPORT]
// tag and the terse tool marks, with no Garden glyph or legacy shortcode left.
func TestBuildStopSummary_Business(t *testing.T) {
	counts := map[string]int{"Bash": 4, "Read": 3, "Mystery": 1}
	for _, c := range []struct{ outcome, want string }{
		{"done", "[OK] Agent `w` — done"},
		{"failed", "[FAIL] Agent `w` — failed"},
		{"stopped", "[STOP] Agent `w` — stopped"},
	} {
		t.Run(c.outcome, func(t *testing.T) {
			got := buildStopSummary(Business, "w", c.outcome, 8, counts, time.Minute)
			for _, want := range []string{c.want, "[REPORT] 8 tool calls", "$ Bash (4)", "≡ Read (3)", "· Mystery (1)"} {
				if !strings.Contains(got, want) {
					t.Errorf("Business summary missing %q: %q", want, got)
				}
			}
			if strings.Contains(strings.ReplaceAll(got, "`", ""), ":") {
				t.Errorf("legacy shortcode leaked: %q", got)
			}
			for _, g := range Garden.SemanticGlyphs {
				if strings.Contains(got, g) {
					t.Errorf("Garden semantic glyph %q leaked into Business summary: %q", g, got)
				}
			}
			for _, g := range Garden.ToolGlyphs {
				if strings.Contains(got, g) {
					t.Errorf("Garden tool glyph %q leaked into Business summary: %q", g, got)
				}
			}
		})
	}
}

// TestBuildStopSummary_GardenToolDefault covers the Garden fallback for a tool
// without its own glyph.
func TestBuildStopSummary_GardenToolDefault(t *testing.T) {
	got := buildStopSummary(Garden, "w", "done", 1, map[string]int{"Mystery": 1}, time.Second)
	if !strings.Contains(got, "🔧 Mystery (1)") {
		t.Errorf("summary = %q, want default tool glyph glued to the name", got)
	}
}

// TestStopSession_ThemesSummary proves StopSession resolves the theme for the
// stream's channel when building the final task_update summary.
func TestStopSession_ThemesSummary(t *testing.T) {
	for _, c := range []struct {
		name  string
		theme Theme
		want  string
	}{
		{"garden", Garden, "🌼 Agent `worker-1` — done"},
		{"business", Business, "[OK] Agent `worker-1` — done"},
	} {
		t.Run(c.name, func(t *testing.T) {
			client := &mockStreamClient{}
			mgr := NewAgentStreamManager(client, testLogger())
			mgr.SetThreadStatusSetter(&themedStatusSetter{theme: c.theme})
			ctx := context.Background()
			mgr.StartSession(ctx, "worker-1", "C1", "ts-1", "U1")
			mgr.HandleToolEvent("worker-1", "Read", "PreToolUse", "")
			mgr.StopSession(ctx, "worker-1", "done")

			var found bool
			for _, call := range client.getCalls() {
				if strings.Contains(call.chunksJSON, c.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("no appended chunk contained %q; calls: %+v", c.want, client.getCalls())
			}
		})
	}
}

// TestHandleSessionStart_ResumeConfirmationThemed pins the themed resumed
// line: the glyph is glued to the "Resumed in" label in both themes.
func TestHandleSessionStart_ResumeConfirmationThemed(t *testing.T) {
	for _, c := range []struct {
		name  string
		theme Theme
		want  string
	}{
		{"garden", Garden, "🌅 Resumed in "},
		{"business", Business, "[RESUMED] Resumed in "},
	} {
		t.Run(c.name, func(t *testing.T) {
			mgr := NewAgentStreamManager(&mockStreamClient{}, testLogger())
			mgr.SetThreadStatusSetter(&themedStatusSetter{theme: c.theme})
			poster := &mockThreadPoster{}
			mgr.SetThreadMessagePoster(poster)
			ctx := context.Background()

			mgr.MarkColdResume("resume-agent", time.Now().Add(-3*time.Second), "thread", "runc", "C-THREAD", "ts-root")
			mgr.StartSession(ctx, "resume-agent", "C-THREAD", "ts-root", "U1")
			mgr.HandleSessionStart(ctx, "resume-agent", "resume")

			posts := poster.all()
			if len(posts) != 1 || !strings.HasPrefix(posts[0].text, c.want) {
				t.Fatalf("resume posts = %+v, want one starting with %q", posts, c.want)
			}
		})
	}
}
