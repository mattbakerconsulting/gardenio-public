package bridge

// Phase 4 (gd-anVGNChYkZ): slash-command and interactive replies resolve their
// glyphs through themeFor(channel). Each wired surface gets a Business-mode
// assertion here (plain bracket text, no Garden glyph) next to the Garden
// default; the per-surface legacy-shortcode expectations live in the surfaces'
// own tests.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"

	"github.com/slack-go/slack"
)

const (
	bizChannel    = "C-biz"
	gardenChannel = "C-garden"
)

// gardenGlyphs is every glyph Garden uses in the vocabularies the slash
// surfaces draw from, for the "Business output leaked a Garden glyph" check.
func gardenGlyphs() []string {
	var out []string
	for _, m := range []map[string]string{Garden.SemanticGlyphs, Garden.Lifecycle, Garden.BeadType} {
		for _, g := range m {
			out = append(out, g)
		}
	}
	return out
}

func assertNoGardenGlyph(t *testing.T, label, text string) {
	t.Helper()
	for _, g := range gardenGlyphs() {
		if strings.Contains(text, g) {
			t.Errorf("%s leaked Garden glyph %q:\n%s", label, g, text)
		}
	}
	if strings.Contains(text, ":") {
		for _, sc := range []string{":x:", ":white_check_mark:", ":warning:", ":pause_button:", ":arrow_forward:", ":rocket:", ":lock:", ":no_entry_sign:"} {
			if strings.Contains(text, sc) {
				t.Errorf("%s still carries legacy shortcode %q:\n%s", label, sc, text)
			}
		}
	}
}

// slashThemedBot returns a test bot whose bizChannel resolves to Business and
// gardenChannel to Garden, plus the captured ephemeral texts.
func slashThemedBot(t *testing.T) (*Bot, *[]string) {
	t.Helper()
	slackSrv, texts := captureEphemeral(t)
	t.Cleanup(slackSrv.Close)
	bot := newTestBot(newMockDaemon(), slackSrv)
	bot.themes.put(bizChannel, Business, time.Now())
	bot.themes.put(gardenChannel, Garden, time.Now())
	return bot, texts
}

func lastText(t *testing.T, texts *[]string) string {
	t.Helper()
	if len(*texts) == 0 {
		t.Fatal("expected an ephemeral message to be posted")
	}
	return (*texts)[len(*texts)-1]
}

func TestSlashReplies_ThemedPerChannel(t *testing.T) {
	cases := []struct {
		name     string
		run      func(b *Bot, ch string)
		bizWant  string
		gardWant string
	}{
		{
			name: "bouncer not configured",
			run: func(b *Bot, ch string) {
				b.handleBouncerCommand(context.Background(), slack.SlashCommand{ChannelID: ch, UserID: "U1", Text: "list"})
			},
			bizWant:  "[FAIL] Bouncer",
			gardWant: Garden.Semantic(SemFail) + " Bouncer",
		},
		{
			name: "bouncer usage",
			run: func(b *Bot, ch string) {
				b.bouncer = newTestBouncer([]string{"10.0.0.1/32"})
				b.handleBouncerCommand(context.Background(), slack.SlashCommand{ChannelID: ch, UserID: "U1", Text: ""})
			},
			bizWant:  "[INFO] Usage:",
			gardWant: Garden.Semantic(SemInfo) + " Usage:",
		},
		{
			name: "bouncer list",
			run: func(b *Bot, ch string) {
				b.bouncer = newTestBouncer([]string{"10.0.0.1/32"})
				b.handleBouncerCommand(context.Background(), slack.SlashCommand{ChannelID: ch, UserID: "U1", Text: "list"})
			},
			bizWant:  "[INFO] *IP Whitelist*",
			gardWant: Garden.Semantic(SemInfo) + " *IP Whitelist*",
		},
		{
			name: "bouncer add",
			run: func(b *Bot, ch string) {
				b.bouncer = newTestBouncer([]string{"10.0.0.1/32"})
				b.handleBouncerCommand(context.Background(), slack.SlashCommand{ChannelID: ch, UserID: "U1", Text: "add 10.0.0.2"})
			},
			bizWant:  "[OK] Added",
			gardWant: Garden.Semantic(SemOK) + " Added",
		},
		{
			name: "trigger pause",
			run: func(b *Bot, ch string) {
				b.handleTriggerCommand(context.Background(), slack.SlashCommand{ChannelID: ch, UserID: "U1", Text: "off"})
			},
			bizWant:  "[PAUSED] Triggers paused.",
			gardWant: Garden.Semantic(SemPaused) + " Triggers paused.",
		},
		{
			name: "trigger resume when running",
			run: func(b *Bot, ch string) {
				b.handleTriggerCommand(context.Background(), slack.SlashCommand{ChannelID: ch, UserID: "U1", Text: "on"})
			},
			bizWant:  "[RESUMED] Triggers are already running.",
			gardWant: Garden.Semantic(SemResumed) + " Triggers are already running.",
		},
		{
			name: "schedule pause",
			run: func(b *Bot, ch string) {
				b.handleScheduleCommand(context.Background(), slack.SlashCommand{ChannelID: ch, UserID: "U1", Text: "off"})
			},
			bizWant:  "[PAUSED] Time-based activity paused",
			gardWant: Garden.Semantic(SemPaused) + " Time-based activity paused",
		},
		{
			name: "schedule unknown project",
			run: func(b *Bot, ch string) {
				b.handleScheduleCommand(context.Background(), slack.SlashCommand{ChannelID: ch, UserID: "U1", Text: "off nope"})
			},
			bizWant:  "[FAIL]",
			gardWant: Garden.Semantic(SemFail),
		},
		{
			name: "formula usage",
			run: func(b *Bot, ch string) {
				b.handleFormulaCommand(context.Background(), slack.SlashCommand{ChannelID: ch, UserID: "U1", Text: "show"})
			},
			bizWant:  "[FAIL] Usage:",
			gardWant: Garden.Semantic(SemFail) + " Usage:",
		},
		{
			name: "formula help",
			run: func(b *Bot, ch string) {
				b.handleFormulaCommand(context.Background(), slack.SlashCommand{ChannelID: ch, UserID: "U1", Text: "help"})
			},
			bizWant:  "[NOTE] */formula*",
			gardWant: Garden.Semantic(SemNote) + " */formula*",
		},
		{
			name: "autodestruct unauthorized",
			run: func(b *Bot, ch string) {
				b.handleAutoDestructCommand(context.Background(), slack.SlashCommand{ChannelID: ch, UserID: "U999"})
			},
			bizWant:  "[DENIED] You are not authorized",
			gardWant: Garden.Semantic(SemDenied) + " You are not authorized",
		},
		{
			name: "kill usage",
			run: func(b *Bot, ch string) {
				b.handleKillCommand(context.Background(), slack.SlashCommand{ChannelID: ch, UserID: "U1", Text: ""})
			},
			bizWant:  "[FAIL] Usage:",
			gardWant: Garden.Semantic(SemFail) + " Usage:",
		},
		{
			name: "decisions none pending",
			run: func(b *Bot, ch string) {
				b.handleDecisionsCommand(context.Background(), slack.SlashCommand{ChannelID: ch, UserID: "U1"})
			},
			bizWant:  "[OK] No pending decisions",
			gardWant: Garden.Semantic(SemOK) + " No pending decisions",
		},
		{
			name: "roster none",
			run: func(b *Bot, ch string) {
				b.handleRosterCommand(context.Background(), slack.SlashCommand{ChannelID: ch, UserID: "U1"})
			},
			bizWant:  "[AGENT] No active agents",
			gardWant: Garden.BeadType["agent"] + " No active agents",
		},
		{
			name: "clear threads without state",
			run: func(b *Bot, ch string) {
				b.handleClearThreadsCommand(context.Background(), slack.SlashCommand{ChannelID: ch, UserID: "U1"})
			},
			bizWant:  "[FAIL] State manager not available",
			gardWant: Garden.Semantic(SemFail) + " State manager not available",
		},
	}
	t.Setenv("AUTODESTRUCT_ALLOWED_USERS", "")
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			b, texts := slashThemedBot(t)
			c.run(b, bizChannel)
			biz := lastText(t, texts)
			if !strings.Contains(biz, c.bizWant) {
				t.Errorf("Business: want %q in:\n%s", c.bizWant, biz)
			}
			assertNoGardenGlyph(t, "Business reply", biz)

			b, texts = slashThemedBot(t)
			c.run(b, gardenChannel)
			if g := lastText(t, texts); !strings.Contains(g, c.gardWant) {
				t.Errorf("Garden: want %q in:\n%s", c.gardWant, g)
			}
		})
	}
}

func TestAutoDestructConfirm_ThemedWarning(t *testing.T) {
	var mu sync.Mutex
	var blocks string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		blocks = r.FormValue("blocks")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1.2"})
	}))
	defer srv.Close()
	bot := newTestBot(newMockDaemon(), srv)
	bot.themes.put(bizChannel, Business, time.Now())

	bot.autoDestructConfirm(context.Background(), slack.SlashCommand{ChannelID: bizChannel, UserID: "U1"})
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(blocks, "[WARN] *Autodestruct*") {
		t.Errorf("Business confirm block missing [WARN] label: %s", blocks)
	}
	assertNoGardenGlyph(t, "autodestruct confirm", blocks)
}

func TestRestartThreadAgent_ThemedFailure(t *testing.T) {
	b, texts := slashThemedBot(t)
	callback := slack.InteractionCallback{
		User:    slack.User{ID: "U1"},
		Channel: slack.Channel{GroupConversation: slack.GroupConversation{Conversation: slack.Conversation{ID: bizChannel}}},
	}
	b.handleRestartThreadAgent(context.Background(), "some-agent", callback)
	if got := lastText(t, texts); !strings.Contains(got, "[FAIL] Restart is only available in threads.") {
		t.Errorf("Business restart failure = %q", got)
	}
}

func TestRosterAgentStateBadge(t *testing.T) {
	cases := []struct{ state, garden, business string }{
		{"working", Garden.Glyph(LifecycleWorking) + " working", Business.Glyph(LifecycleWorking) + " working"},
		{"spawning", Garden.Glyph(LifecycleSpawning) + " spawning", Business.Glyph(LifecycleSpawning) + " spawning"},
		{"done", Garden.Glyph(LifecycleDone) + " done", Business.Glyph(LifecycleDone) + " done"},
		{"failed", Garden.Glyph(LifecycleAborted) + " failed", Business.Glyph(LifecycleAborted) + " failed"},
		{"rate_limited", Garden.Semantic(SemWarn) + " rate limited", "[WARN] rate limited"},
		{"idle", Garden.Semantic(SemIdle) + " idle", "[IDLE] idle"},
		{"", Garden.Semantic(SemIdle) + " idle", "[IDLE] idle"},
	}
	for _, c := range cases {
		if got := agentStateBadge(Garden, c.state); got != c.garden {
			t.Errorf("Garden badge(%q) = %q, want %q", c.state, got, c.garden)
		}
		if got := agentStateBadge(Business, c.state); got != c.business {
			t.Errorf("Business badge(%q) = %q, want %q", c.state, got, c.business)
		}
	}
}

func TestHandleRosterCommand_ThemedRows(t *testing.T) {
	daemon := newMockDaemon()
	daemon.mu.Lock()
	daemon.beads["agent-1"] = &beadsapi.BeadDetail{
		ID: "agent-1", Title: "bot-1", Type: "agent", Status: "open",
		Fields: map[string]string{"agent": "bot-1", "agent_state": "working", "project": "gardenio", "mode": "crew", "role": "crew"},
	}
	daemon.mu.Unlock()

	var mu sync.Mutex
	var blocks string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		blocks = r.FormValue("blocks")
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "message_ts": "1.2"})
	}))
	defer srv.Close()
	bot := newTestBot(daemon, srv)
	bot.themes.put(bizChannel, Business, time.Now())
	bot.handleRosterCommand(context.Background(), slack.SlashCommand{ChannelID: bizChannel, UserID: "U1"})

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(blocks, "bot-1") {
		t.Fatalf("seeded agent missing from roster: %s", blocks)
	}
	if !strings.Contains(blocks, "[AGENT] *Agent Roster*") {
		t.Errorf("Business roster header missing [AGENT] label: %s", blocks)
	}
	assertNoGardenGlyph(t, "roster", blocks)
}

func TestUnreleasedCommand_ThemedNoProvider(t *testing.T) {
	b, texts := slashThemedBot(t)
	b.handleUnreleasedCommand(context.Background(), slack.SlashCommand{ChannelID: bizChannel, UserID: "U1"})
	if got := lastText(t, texts); !strings.HasPrefix(got, "[FAIL] No git provider configured") {
		t.Errorf("Business unreleased failure = %q", got)
	}
}

func TestSpawnVectorLine_Themed(t *testing.T) {
	fields := map[string]string{"spawn_source": "schedule", "schedule_title": "nightly triage"}
	line := formatSpawnVectorLine(fields)
	if line == "" {
		t.Fatal("expected a classified spawn-vector line")
	}
	garden := themeSpawnVectorLine(Garden, line)
	business := themeSpawnVectorLine(Business, line)
	if !strings.HasPrefix(garden, Garden.Semantic(SemSchedule)+" ") {
		t.Errorf("Garden spawn vector = %q", garden)
	}
	if !strings.HasPrefix(business, "[SCHEDULE] ") {
		t.Errorf("Business spawn vector = %q", business)
	}
	// Every vector maps to a themed glyph in both themes; the label survives.
	for _, icon := range []string{spawnIconWebhook, spawnIconSlack, spawnIconJira, spawnIconGitlab, spawnIconCron, spawnIconGardener, spawnIconManual, spawnIconFallback} {
		for _, th := range []Theme{Garden, Business} {
			got := themeSpawnVectorLine(th, icon+" label text")
			if !strings.HasSuffix(got, " label text") || strings.HasPrefix(got, icon) {
				t.Errorf("%s/%s: %q not themed", th.Name, icon, got)
			}
		}
	}
	assertNoGardenGlyph(t, "Business spawn vectors", themeSpawnVectorLine(Business, spawnIconGardener+" x"))
	if got := themeSpawnVectorLine(Business, "no-icon-here"); got != "no-icon-here" {
		t.Errorf("unrecognised line must pass through, got %q", got)
	}
}

func TestClassifyMessageSubtype_ThemedPrefixes(t *testing.T) {
	for _, th := range []Theme{Garden, Business} {
		squawk := th.Semantic(SemNote) + " agent: making progress"
		if got := classifyBotMessage(squawk, slack.Msg{Text: squawk}); got != SubtypeSquawk {
			t.Errorf("%s squawk = %q, want %q", th.Name, got, SubtypeSquawk)
		}
		closed := th.Semantic(SemOK) + " Closed task: *x*"
		if got := classifyBotMessage(closed, slack.Msg{Text: closed}); got != SubtypeBeadMirror {
			t.Errorf("%s closed mirror = %q, want %q", th.Name, got, SubtypeBeadMirror)
		}
	}
	if got := classifyBotMessage("[OK] something else", slack.Msg{Text: "[OK] something else"}); got != "" {
		t.Errorf("non-mirror themed text must not classify, got %q", got)
	}
}

func TestEscalationNotifications_ThemedBusiness(t *testing.T) {
	var mu sync.Mutex
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		posted = append(posted, r.FormValue("text")+"\n"+r.FormValue("blocks"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C-biz", "ts": "1.2", "message_ts": "1.2"})
	}))
	defer srv.Close()
	bot := newTestBot(newMockDaemon(), srv)
	bot.channel = bizChannel
	bot.escalationQueueChannel = bizChannel
	bot.themes.put(bizChannel, Business, time.Now())

	ev := BeadEvent{ID: "esc-1", Title: "Deploy", Priority: 1, Fields: map[string]string{
		"operation_type": "deploy", "result_status": "success",
	}}
	if err := bot.NotifyEscalationQueueAlert(context.Background(), ev); err != nil {
		t.Fatalf("queue alert: %v", err)
	}
	if err := bot.PostStaleEscalationDigest(context.Background(), []StaleEscalationDigestEntry{{ID: "esc-1", Title: "Deploy", Priority: 1}}, 0); err != nil {
		t.Fatalf("digest: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(posted, "\n")
	for _, want := range []string{"[ESCALATION] *Stale Escalation", "[ESCALATION] *Stale Escalations Digest*"} {
		if !strings.Contains(joined, want) {
			t.Errorf("Business escalation post missing %q:\n%s", want, joined)
		}
	}
	assertNoGardenGlyph(t, "escalation posts", joined)
}

func TestAccountNotifications_ThemedBusiness(t *testing.T) {
	var mu sync.Mutex
	var posted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		posted = append(posted, r.FormValue("text")+"\n"+r.FormValue("blocks"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C-biz", "ts": "1.2", "message_ts": "1.2"})
	}))
	defer srv.Close()
	bot := newTestBot(newMockDaemon(), srv)
	bot.channel = bizChannel
	bot.themes.put(bizChannel, Business, time.Now())

	evt := RotationEvent{FromAccount: "acct-a", ToAccount: "acct-b", Project: "gardenio"}
	if err := bot.NotifyAccountThrottled(context.Background(), evt); err != nil {
		t.Fatalf("throttled: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(posted, "\n")
	if !strings.Contains(joined, "[WARN] Account throttled: acct-a") {
		t.Errorf("Business throttled post missing [WARN] label:\n%s", joined)
	}
	assertNoGardenGlyph(t, "account throttled", joined)
	if strings.Contains(joined, "🚨") {
		t.Errorf("legacy 🚨 must be themed away:\n%s", joined)
	}
}

func TestDashboardRender_ThemedBusiness(t *testing.T) {
	d := &Dashboard{cfg: DashboardConfig{}}
	agents := []beadsapi.AgentBead{
		{ID: "a1", AgentName: "w1", AgentState: "working", Project: "p", Mode: "crew", Role: "crew", Metadata: map[string]string{}},
		{ID: "a2", AgentName: "s1", AgentState: "spawning", Project: "p", Metadata: map[string]string{}},
		{ID: "a3", AgentName: "i1", AgentState: "idle", Project: "p", Metadata: map[string]string{}},
		{ID: "a4", AgentName: "d1", AgentState: "failed", Project: "p", Metadata: map[string]string{}},
	}
	decisions := []*beadsapi.BeadDetail{{ID: "dec-1", Title: "Pick", Fields: map[string]string{}, Labels: []string{"escalated"}}}
	blocks, _ := d.renderBlocks(Business, agents, decisions, accountCostData{DailyUSD: 1})
	text := blocksToText(blocks)

	if !strings.Contains(text, "[REPORT] "+dashboardMarker) {
		t.Errorf("Business dashboard header missing themed marker:\n%s", text)
	}
	for _, want := range []string{
		Business.Glyph(LifecycleWorking) + " 1 working",
		"[IDLE] 1 idle",
		"[STOP] 1 stopped",
		Business.Glyph(LifecycleSpawning) + " 1 starting",
		Business.Glyph(LifecycleNeedsDecision) + " 1 pending decisions",
		"[ESCALATION]",
		"[COST]",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("Business dashboard missing %q:\n%s", want, text)
		}
	}
	assertNoGardenGlyph(t, "dashboard", text)
	if strings.Contains(text, ":factory:") {
		t.Errorf("legacy factory shortcode must be themed away:\n%s", text)
	}
}
