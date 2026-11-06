package bridge

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// themeFakeBeadClient wraps mockEscalationBeadClient (escalations_test.go) to
// override ListProjectBeads with per-test project data / error injection,
// without re-implementing the rest of the BeadClient interface.
type themeFakeBeadClient struct {
	*mockEscalationBeadClient
	projects map[string]beadsapi.ProjectInfo
	listErr  error
}

func (f *themeFakeBeadClient) ListProjectBeads(_ context.Context) (map[string]beadsapi.ProjectInfo, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.projects, nil
}

// TestThemeKeyParity guards against one theme gaining a key the other lacks,
// in any of its glyph/color/vocabulary maps. A themeFor caller that indexes the
// core maps by key (rather than switching per-theme) would panic/blank on the
// gap; a Semantic/Tool/Reaction gap would silently fall back to Garden's glyph
// in Business (gd-m8LxpeYIyE).
func TestThemeKeyParity(t *testing.T) {
	maps := func(th Theme) map[string]map[string]string {
		return map[string]map[string]string{
			"Lifecycle": th.Lifecycle,
			"BeadType":  th.BeadType,
			"Color":     th.Color,
			"Semantic":  th.SemanticGlyphs,
			"Tool":      th.ToolGlyphs,
			"Reaction":  th.ReactionNames,
		}
	}
	keys := func(m map[string]string) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		return out
	}
	business := maps(Business)
	for label, garden := range maps(Garden) {
		assertSameKeySet(t, label, keys(garden), keys(business[label]))
	}
}

func assertSameKeySet(t *testing.T, label string, a, b []string) {
	t.Helper()
	setA := make(map[string]bool, len(a))
	for _, k := range a {
		setA[k] = true
	}
	setB := make(map[string]bool, len(b))
	for _, k := range b {
		setB[k] = true
	}
	for k := range setA {
		if !setB[k] {
			t.Errorf("%s: Business is missing key %q present in Garden", label, k)
		}
	}
	for k := range setB {
		if !setA[k] {
			t.Errorf("%s: Garden is missing key %q present in Business", label, k)
		}
	}
}

func TestThemeByName(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want Theme
	}{
		{"empty defaults to Garden", "", Garden},
		{"garden", "garden", Garden},
		{"business", "business", Business},
		{"unknown value defaults to Garden", "bogus", Garden},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := themeByName(c.in)
			if got.Name != c.want.Name {
				t.Errorf("themeByName(%q) = %q, want %q", c.in, got.Name, c.want.Name)
			}
		})
	}
}

func TestBotThemeFor(t *testing.T) {
	logger := slog.Default()

	cases := []struct {
		name     string
		bot      *Bot
		channel  string
		wantName string
	}{
		{
			name:     "nil bot defaults to Garden",
			bot:      nil,
			channel:  "C1",
			wantName: Garden.Name,
		},
		{
			name:     "nil daemon defaults to Garden",
			bot:      &Bot{logger: logger},
			channel:  "C1",
			wantName: Garden.Name,
		},
		{
			name: "ListProjectBeads error defaults to Garden",
			bot: &Bot{
				logger: logger,
				daemon: &themeFakeBeadClient{
					mockEscalationBeadClient: newMockEscalationBeadClient(),
					listErr:                  errors.New("daemon unavailable"),
				},
			},
			channel:  "C1",
			wantName: Garden.Name,
		},
		{
			name: "no project claims the channel defaults to Garden",
			bot: &Bot{
				logger: logger,
				daemon: &themeFakeBeadClient{
					mockEscalationBeadClient: newMockEscalationBeadClient(),
					projects: map[string]beadsapi.ProjectInfo{
						"other": {Name: "other", SlackChannels: []string{"C-OTHER"}, CardTheme: "business"},
					},
				},
			},
			channel:  "C1",
			wantName: Garden.Name,
		},
		{
			name: "conflicting projects defaults to Garden",
			bot: &Bot{
				logger: logger,
				daemon: &themeFakeBeadClient{
					mockEscalationBeadClient: newMockEscalationBeadClient(),
					projects: map[string]beadsapi.ProjectInfo{
						"a": {Name: "a", SlackChannels: []string{"C1"}, CardTheme: "business"},
						"b": {Name: "b", SlackChannels: []string{"C1"}, CardTheme: "business"},
					},
				},
			},
			channel:  "C1",
			wantName: Garden.Name,
		},
		{
			name: "project with unset card_theme defaults to Garden",
			bot: &Bot{
				logger: logger,
				daemon: &themeFakeBeadClient{
					mockEscalationBeadClient: newMockEscalationBeadClient(),
					projects: map[string]beadsapi.ProjectInfo{
						"proj": {Name: "proj", SlackChannels: []string{"C1"}, CardTheme: ""},
					},
				},
			},
			channel:  "C1",
			wantName: Garden.Name,
		},
		{
			name: "project with card_theme=business resolves to Business",
			bot: &Bot{
				logger: logger,
				daemon: &themeFakeBeadClient{
					mockEscalationBeadClient: newMockEscalationBeadClient(),
					projects: map[string]beadsapi.ProjectInfo{
						"proj": {Name: "proj", SlackChannels: []string{"C1"}, CardTheme: "business"},
					},
				},
			},
			channel:  "C1",
			wantName: Business.Name,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.bot.themeFor(context.Background(), c.channel)
			if got.Name != c.wantName {
				t.Errorf("themeFor(%q) = %q, want %q", c.channel, got.Name, c.wantName)
			}
		})
	}
}

// TestThemeVocabularyDistinct checks every glyph is non-empty and unique within
// its map (a reused glyph reads as a duplicate label), and that Garden's
// Semantic/Tool glyphs don't collide with its Lifecycle/BeadType glyphs.
func TestThemeVocabularyDistinct(t *testing.T) {
	for _, th := range []Theme{Garden, Business} {
		for name, m := range map[string]map[string]string{
			"Semantic": th.SemanticGlyphs,
			"Tool":     th.ToolGlyphs,
			"Reaction": th.ReactionNames,
		} {
			seen := map[string]string{}
			for k, v := range m {
				if v == "" {
					t.Errorf("%s.%s[%q] is empty", th.Name, name, k)
				}
				if prev, dup := seen[v]; dup {
					t.Errorf("%s.%s: glyph %q used by both %q and %q", th.Name, name, v, prev, k)
				}
				seen[v] = k
			}
		}
	}
	taken := map[string]bool{}
	for _, m := range []map[string]string{Garden.Lifecycle, Garden.BeadType} {
		for _, v := range m {
			taken[v] = true
		}
	}
	for _, m := range []map[string]string{Garden.SemanticGlyphs, Garden.ToolGlyphs} {
		for k, v := range m {
			if taken[v] {
				t.Errorf("Garden glyph %q (%q) collides with a Lifecycle/BeadType glyph", v, k)
			}
		}
	}
}

// TestThemeVocabularyRules pins the spec's per-theme rules: Garden reactions
// are plant emoji, Business Semantic values are plain bracket text and its Tool
// glyphs contain no emoji, and Reaction names are bare stock names.
func TestThemeVocabularyRules(t *testing.T) {
	for _, th := range []Theme{Garden, Business} {
		for k, n := range th.ReactionNames {
			if strings.ContainsAny(n, ": ") || n != strings.ToLower(n) {
				t.Errorf("%s reaction %q = %q: want a bare lowercase stock emoji name", th.Name, k, n)
			}
		}
	}
	for k, v := range Business.SemanticGlyphs {
		if !strings.HasPrefix(v, "[") || !strings.HasSuffix(v, "]") {
			t.Errorf("Business Semantic[%q] = %q: want a bracket tag", k, v)
		}
	}
	for k, v := range Business.ToolGlyphs {
		for _, r := range v {
			if r > 0x2BFF {
				t.Errorf("Business Tool[%q] = %q contains non-geometric rune %U", k, v, r)
			}
		}
	}
	if got, want := Garden.Reaction(ReactionSeen), "seedling"; got != want {
		t.Errorf("Garden seen reaction = %q, want %q", got, want)
	}
	if got, want := Business.Reaction(ReactionDone), "white_check_mark"; got != want {
		t.Errorf("Business done reaction = %q, want %q", got, want)
	}
	if got := Garden.BeadType["task"]; got != "🌱" {
		t.Errorf("Task glyph = %q, want 🌱 (kept per spec)", got)
	}
}

func TestThemeVocabularyAccessors(t *testing.T) {
	if got := Garden.Semantic(SemOK); got != "🌼" {
		t.Errorf("Garden.Semantic(ok) = %q", got)
	}
	if got := Business.Semantic(SemFail); got != "[FAIL]" {
		t.Errorf("Business.Semantic(fail) = %q", got)
	}
	if got := Garden.Semantic("no_such_key"); got != "" {
		t.Errorf("unknown semantic key = %q, want empty", got)
	}
	if got := Garden.Tool("Bash"); got != "⚙️" {
		t.Errorf("Garden.Tool(Bash) = %q", got)
	}
	if got := Business.Tool("Bash"); got != "$" {
		t.Errorf("Business.Tool(Bash) = %q", got)
	}
	// Unknown tools take the theme's own default, not Garden's.
	if got := Garden.Tool("MysteryTool"); got != Garden.ToolGlyphs[ToolDefault] {
		t.Errorf("Garden unknown tool = %q", got)
	}
	if got := Business.Tool("MysteryTool"); got != Business.ToolGlyphs[ToolDefault] {
		t.Errorf("Business unknown tool = %q", got)
	}
	if got := Garden.Reaction("no_such_state"); got != "" {
		t.Errorf("unknown reaction = %q, want empty", got)
	}
	// A theme with empty maps falls back to Garden (a third theme mid-build).
	var bare Theme
	if got := bare.Semantic(SemLink); got != Garden.SemanticGlyphs[SemLink] {
		t.Errorf("bare.Semantic(link) = %q, want Garden's", got)
	}
	if got := bare.Tool("Read"); got != Garden.ToolGlyphs[ToolDefault] {
		t.Errorf("bare.Tool(Read) = %q, want Garden default", got)
	}
	if got := bare.Reaction(ReactionFailed); got != Garden.ReactionNames[ReactionFailed] {
		t.Errorf("bare.Reaction(failed) = %q, want Garden's", got)
	}
}
