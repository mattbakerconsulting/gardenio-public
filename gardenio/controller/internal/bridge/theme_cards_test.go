package bridge

import (
	"context"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"gardenio/controller/internal/beadsapi"
)

// countingProjectClient counts ListProjectBeads calls to prove themeFor's cache.
type countingProjectClient struct {
	*mockEscalationBeadClient
	projects map[string]beadsapi.ProjectInfo
	calls    atomic.Int32
}

func (c *countingProjectClient) ListProjectBeads(context.Context) (map[string]beadsapi.ProjectInfo, error) {
	c.calls.Add(1)
	return c.projects, nil
}

func TestThemeFor_CachesPerChannel(t *testing.T) {
	client := &countingProjectClient{
		mockEscalationBeadClient: newMockEscalationBeadClient(),
		projects: map[string]beadsapi.ProjectInfo{
			"proj": {Name: "proj", SlackChannels: []string{"C1"}, CardTheme: "business"},
		},
	}
	b := &Bot{logger: slog.Default(), daemon: client}
	for i := 0; i < 5; i++ {
		if got := b.themeFor(context.Background(), "C1"); got.Name != Business.Name {
			t.Fatalf("call %d: themeFor = %q, want business", i, got.Name)
		}
	}
	if n := client.calls.Load(); n != 1 {
		t.Errorf("ListProjectBeads called %d times for 5 resolutions, want 1 (cached)", n)
	}

	// An expired entry is re-resolved, picking up a card_theme edit.
	client.projects["proj"] = beadsapi.ProjectInfo{Name: "proj", SlackChannels: []string{"C1"}, CardTheme: "garden"}
	b.themes.put("C1", Business, time.Now().Add(-2*themeCacheTTL))
	if got := b.themeFor(context.Background(), "C1"); got.Name != Garden.Name {
		t.Errorf("after expiry themeFor = %q, want garden", got.Name)
	}
}

// TestAgentCards_GlyphGluedToLabel enforces the spec's accessibility rule for
// every lifecycle card builder under both themes: the glyph appears
// immediately before its verbal label in the same text run.
func TestAgentCards_GlyphGluedToLabel(t *testing.T) {
	for _, th := range []Theme{Garden, Business} {
		t.Run(th.Name, func(t *testing.T) {
			full := func(state string, pending int, blocked bool) string {
				blocks := buildAgentCardBlocks(th, "gardenio/crew/bot", "", pending, state, "", time.Time{},
					"", "", "", "", "", nil, "", blocked, triggerCardInfo{}, nil)
				return blocks[0].(*slack.SectionBlock).Text.Text
			}
			cases := []struct {
				got, state, label string
			}{
				{full("working", 0, false), LifecycleWorking, "working"},
				{full("spawning", 0, false), LifecycleSpawning, "starting"},
				{full("spawning", 0, true), LifecycleBlocked, "stuck: cluster capacity"},
				{full("done", 0, false), LifecycleDone, "done"},
				{full("failed", 0, false), LifecycleAborted, "failed"},
				{full("rate_limited", 0, false), LifecycleBlocked, "rate limited"},
				{full("working", 1, false), LifecycleNeedsDecision, "decision pending"},
			}
			for _, c := range cases {
				if want := th.Glyph(c.state) + " " + c.label; !strings.Contains(c.got, want) {
					t.Errorf("full card %q missing glued %q", c.got, want)
				}
			}

			compact := buildCompactWorkingCardBlocks(th, "gardenio/crew/bot", "", "working", "", "crew", "", nil)
			if text := compact[0].(*slack.SectionBlock).Text.Text; !strings.Contains(text, th.Glyph(LifecycleWorking)+" working") {
				t.Errorf("compact working card %q missing glued working glyph", text)
			}

			for state, c := range map[string]struct{ glyph, label string }{
				"done":         {LifecycleDone, "agent finished"},
				"failed":       {LifecycleAborted, "failed"},
				"rate_limited": {LifecycleBlocked, "rate limited"},
				"reaped":       {LifecycleReaped, "reaped"},
				"moved":        {LifecycleHandedOff, "moved"},
			} {
				blocks := buildCompactAgentCardBlocks(th, "gardenio/crew/bot", state, "", nil)
				text := blocks[0].(*slack.SectionBlock).Text.Text
				if want := th.Glyph(c.glyph) + " " + c.label; !strings.Contains(text, want) {
					t.Errorf("compact %s card %q missing glued %q", state, text, want)
				}
			}
		})
	}
}

func TestBuildWrapUpAgentCardBlocks_BusinessTheme(t *testing.T) {
	b := &Bot{}
	blocks := b.buildWrapUpAgentCardBlocks(Business, "gardenio/crew/bot", "done",
		`{"tldr":"Shipped it","accomplishments":"x","blockers":"waiting on key"}`, "", 0, nil)

	header := blocks[0].(*slack.HeaderBlock).Text.Text
	if want := Business.Glyph(LifecycleBlocked) + " blocked — Shipped it"; header != want {
		t.Errorf("header = %q, want %q", header, want)
	}
	meta := blocks[1].(*slack.ContextBlock).ContextElements.Elements[0].(*slack.TextBlockObject).Text
	if want := Business.Glyph(LifecycleBlocked) + " blocked · *bot*"; !strings.Contains(meta, want) {
		t.Errorf("meta = %q, want it to contain %q", meta, want)
	}
	// Business mode is plant-free: no Garden glyph may leak into the card.
	for _, g := range Garden.Lifecycle {
		if strings.Contains(header, g) || strings.Contains(meta, g) {
			t.Errorf("Business card leaked Garden glyph %q: header=%q meta=%q", g, header, meta)
		}
	}
}

func TestRestartConditionLine_ThemeMarker(t *testing.T) {
	w, _ := parseWrapUp(`{"restart_when":"when gd-X merges"}`)
	for _, th := range []Theme{Garden, Business} {
		want := th.Glyph(LifecycleRestarted) + " *Restart:* Wakes when gd-X merges"
		if got := restartConditionLine(th, w, nil); got != want {
			t.Errorf("%s restartConditionLine = %q, want %q", th.Name, got, want)
		}
	}
}

func TestBuildThreadStatusText_Themes(t *testing.T) {
	for _, th := range []Theme{Garden, Business} {
		got := buildThreadStatusText(th, "reading files", "Read", "", 1, nil, time.Time{})
		if want := th.Glyph(LifecycleWorking) + " reading files... (1 tools) · last: Read"; got != want {
			t.Errorf("%s status = %q, want %q", th.Name, got, want)
		}
		if got := buildThreadStatusText(th, "", "", "", 0, nil, time.Time{}); got != "" {
			t.Errorf("%s: no activity/tools should yield empty status, got %q", th.Name, got)
		}
	}
}

func TestBuildUnfurlAttachment_Themes(t *testing.T) {
	b := &Bot{beadsViewerURL: "https://prod.gardenio.acme.example/beads"}
	link := "https://prod.gardenio.acme.example/beads/gd-x"

	cases := []struct {
		name string
		th   Theme
		bead *beadsapi.BeadDetail
		want []string
	}{
		{"garden plan", Garden,
			&beadsapi.BeadDetail{ID: "gd-x", Type: "epic", Status: "open", Labels: []string{"plan"}},
			[]string{"🗺️ `plan`", "🫘 open"}},
		{"business epic", Business,
			&beadsapi.BeadDetail{ID: "gd-x", Type: "epic", Status: "in_progress"},
			[]string{"[EPIC]", "◐ in progress"}},
		{"business plan", Business,
			&beadsapi.BeadDetail{ID: "gd-x", Type: "epic", Status: "closed", Labels: []string{"plan"}},
			[]string{"[PLAN]", "● closed"}},
		{"business decision", Business,
			&beadsapi.BeadDetail{ID: "gd-x", Type: "decision", Status: "blocked"},
			[]string{"[DECISION]", "■ blocked"}},
		{"business unthemed type gets a bracket tag", Business,
			&beadsapi.BeadDetail{ID: "gd-x", Type: "bundle", Status: "open"},
			[]string{"[BUNDLE]", "○ open"}},
		{"garden bundle takes its Garden glyph", Garden,
			&beadsapi.BeadDetail{ID: "gd-x", Type: "bundle", Status: "open"},
			[]string{"🪺 `bundle`"}},
		{"garden unthemed type takes the fallback glyph", Garden,
			&beadsapi.BeadDetail{ID: "gd-x", Type: "rating", Status: "open"},
			[]string{"🪹 `rating`"}},
		{"unknown status", Business,
			&beadsapi.BeadDetail{ID: "gd-x", Type: "task", Status: "weird"},
			[]string{"? weird"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			text := renderBlocksText(b.buildUnfurlAttachment(c.th, c.bead, link))
			for _, want := range c.want {
				if !strings.Contains(text, want) {
					t.Errorf("card missing %q\n---\n%s", want, text)
				}
			}
		})
	}

	// Business cards carry no emoji for the themed types.
	text := renderBlocksText(b.buildUnfurlAttachment(Business,
		&beadsapi.BeadDetail{ID: "gd-x", Type: "task", Status: "open"}, link))
	for _, g := range Garden.BeadType {
		if strings.Contains(text, g) {
			t.Errorf("Business unfurl leaked Garden glyph %q: %s", g, text)
		}
	}
}

// TestBuildUnfurls_UsesChannelTheme proves the unfurl path resolves the theme
// from the channel's project rather than defaulting to Garden.
func TestBuildUnfurls_UsesChannelTheme(t *testing.T) {
	daemon := &fakeBatchDaemon{
		beads: map[string]*beadsapi.BeadDetail{
			"gd-abc": {ID: "gd-abc", Title: "T", Type: "task", Status: "open"},
		},
		projects: map[string]beadsapi.ProjectInfo{
			"proj": {Name: "proj", SlackChannels: []string{"CBIZ"}, CardTheme: "business"},
		},
	}
	b := &Bot{logger: slog.Default(), daemon: daemon, beadsViewerURL: "https://v.example/beads"}
	const link = "https://v.example/beads/gd-abc"
	for channel, want := range map[string]string{"CBIZ": "[TASK]", "COTHER": "🌱 `task`"} {
		unfurls := b.buildUnfurls(context.Background(), &slackevents.LinkSharedEvent{
			Channel: channel,
			Links:   []slackevents.SharedLinks{{Domain: "v.example", URL: link}},
		})
		att, ok := unfurls[link]
		if !ok {
			t.Fatalf("%s: no unfurl built", channel)
		}
		if text := renderBlocksText(att); !strings.Contains(text, want) {
			t.Errorf("%s: unfurl missing %q\n---\n%s", channel, want, text)
		}
	}
}
