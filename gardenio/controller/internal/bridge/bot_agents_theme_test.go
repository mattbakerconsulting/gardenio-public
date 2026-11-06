package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/slack-go/slack"
)

// Phase 4 (gd-wKyW84WezG): the leftover shortcodes in bot_agents.go resolve
// through themeFor(channel). These tests pin one Business-channel assertion per
// wired surface, plus the Garden default, by seeding the bot's theme cache.

// themedPostCapture is a Slack stub that records every chat.postMessage's text
// and blocks payload.
type themedPostCapture struct {
	mu    sync.Mutex
	posts []string // text + blocks, concatenated
}

func (c *themedPostCapture) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/chat.postMessage" {
			_ = r.ParseForm()
			c.mu.Lock()
			c.posts = append(c.posts, r.FormValue("text")+"\n"+r.FormValue("blocks"))
			c.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "channel": "C123", "ts": "1234.5678"})
	}))
}

func (c *themedPostCapture) all() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.Join(c.posts, "\n---\n")
}

// themedBot builds a test bot whose channel C123 resolves to th.
func themedBot(t *testing.T, th Theme, daemon *mockDaemon, srv *httptest.Server) *Bot {
	t.Helper()
	bot := newTestBot(daemon, srv)
	bot.channel = "C123"
	bot.themes.put("C123", th, time.Now())
	return bot
}

func TestNotifyAgentSpawn_FlatSpawnGlyphIsThemed(t *testing.T) {
	for _, tc := range []struct {
		th   Theme
		want string
	}{
		{Garden, Garden.Semantic(SemSpawn) + " *Agent spawned: "},
		{Business, "[SPAWN] *Agent spawned: "},
	} {
		daemon := newMockDaemon()
		cap := &themedPostCapture{}
		srv := cap.server()
		bot := themedBot(t, tc.th, daemon, srv)
		bot.NotifyAgentSpawn(context.Background(), BeadEvent{
			ID: "agent-1", Type: "agent", Title: "my-agent", Assignee: "my-agent",
			Fields: map[string]string{"agent": "my-agent", "role": "crew"},
		})
		srv.Close()
		got := cap.all()
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: spawn post missing %q, got %q", tc.th.Name, tc.want, got)
		}
		if strings.Contains(got, ":rocket:") {
			t.Errorf("%s: spawn post leaked :rocket:, got %q", tc.th.Name, got)
		}
	}
}

func TestNotifyAgentSpawn_ScheduleGlyphIsThemed(t *testing.T) {
	for _, tc := range []struct {
		th   Theme
		want string
	}{
		{Garden, Garden.Semantic(SemSchedule) + " Scheduled task *Daily Release Notes* started"},
		{Business, "[SCHEDULE] Scheduled task *Daily Release Notes* started"},
	} {
		daemon := newMockDaemon()
		cap := &themedPostCapture{}
		srv := cap.server()
		bot := themedBot(t, tc.th, daemon, srv)
		bot.NotifyAgentSpawn(context.Background(), BeadEvent{
			ID: "sched-agent-1", Type: "agent", Title: "sched-1234", Assignee: "sched-1234",
			Fields: map[string]string{
				"agent": "sched-1234", "schedule_id": "gd-sched-1",
				"schedule_title": "Daily Release Notes", "schedule_cron": "0 9 * * 1-5",
			},
		})
		srv.Close()
		got := cap.all()
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: schedule start missing %q, got %q", tc.th.Name, tc.want, got)
		}
		if strings.Contains(got, ":clock1:") {
			t.Errorf("%s: schedule start leaked :clock1:, got %q", tc.th.Name, got)
		}
	}
}

func TestNotifyAgentState_ScheduledOutcomeGlyphsAreThemed(t *testing.T) {
	for _, tc := range []struct {
		th    Theme
		state string
		want  string
	}{
		{Garden, "done", Garden.Semantic(SemOK) + " Scheduled task *Nightly* completed"},
		{Business, "done", "[OK] Scheduled task *Nightly* completed"},
		{Garden, "failed", Garden.Semantic(SemFail) + " Scheduled task *Nightly* failed"},
		{Business, "failed", "[FAIL] Scheduled task *Nightly* failed"},
	} {
		daemon := newMockDaemon()
		cap := &themedPostCapture{}
		srv := cap.server()
		bot := themedBot(t, tc.th, daemon, srv)
		bot.agentState["sched-x"] = "working"
		bot.agentScheduleTitle["sched-x"] = "Nightly"
		bot.agentSpawnedAt["sched-x"] = time.Now().Add(-time.Minute)
		bot.NotifyAgentState(context.Background(), BeadEvent{
			Assignee: "sched-x",
			Fields: map[string]string{
				"agent_state": tc.state, "agent": "sched-x",
				"total_cost_usd": "1.25", "total_input_tokens": "1000",
			},
		})
		srv.Close()
		got := cap.all()
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s/%s: missing %q, got %q", tc.th.Name, tc.state, tc.want, got)
		}
		costTag := tc.th.Semantic(SemCost) + " $1.25"
		if !strings.Contains(got, costTag) {
			t.Errorf("%s/%s: cost line missing %q, got %q", tc.th.Name, tc.state, costTag, got)
		}
		for _, legacy := range []string{":white_check_mark:", ":x:", ":moneybag:"} {
			if strings.Contains(got, legacy) {
				t.Errorf("%s/%s: leaked %s, got %q", tc.th.Name, tc.state, legacy, got)
			}
		}
	}
}

func TestPostThreadStateReply_OutcomeGlyphIsThemed(t *testing.T) {
	for _, tc := range []struct {
		th    Theme
		state string
		want  string
	}{
		{Garden, "done", Garden.Semantic(SemOK) + " "},
		{Business, "done", "[OK] "},
		{Garden, "failed", Garden.Semantic(SemFail) + " "},
		{Business, "failed", "[FAIL] "},
	} {
		daemon := newMockDaemon()
		cap := &themedPostCapture{}
		srv := cap.server()
		bot := themedBot(t, tc.th, daemon, srv)
		bot.postThreadStateReply(context.Background(), "my-agent", tc.state, BeadEvent{
			Fields: map[string]string{"total_cost_usd": "2.00", "total_input_tokens": "10"},
		}, "C123", "1111.2222")
		srv.Close()
		got := cap.all()
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s/%s: reply should start with %q, got %q", tc.th.Name, tc.state, tc.want, got)
		}
		if want := tc.th.Semantic(SemCost) + " $2.00"; !strings.Contains(got, want) {
			t.Errorf("%s/%s: reply missing cost %q, got %q", tc.th.Name, tc.state, want, got)
		}
	}
}

func TestReconcileThreadCardTerminal_PingGlyphIsThemed(t *testing.T) {
	for _, tc := range []struct {
		th    Theme
		state string
		want  string
	}{
		{Garden, "done", Garden.Semantic(SemOK) + " "},
		{Business, "done", "[OK] "},
		{Business, "failed", "[FAIL] "},
	} {
		daemon := newMockDaemon()
		cap := &themedPostCapture{}
		srv := cap.server()
		bot := themedBot(t, tc.th, daemon, srv)
		// A wrap-up with a PR always pings (threadCardPingWanted).
		bot.reconcileThreadCardTerminal(context.Background(), "my-agent", tc.state, BeadEvent{
			Fields: map[string]string{"wrapup": `{"pull_requests":["https://example.com/pr/1"]}`},
		}, "C123", "1111.2222")
		srv.Close()
		got := cap.all()
		if !strings.Contains(got, tc.want) || !strings.Contains(got, "see card above") {
			t.Errorf("%s/%s: ping missing %q glued to its label, got %q", tc.th.Name, tc.state, tc.want, got)
		}
	}
}

func TestBuildWrapUpAgentCardBlocks_BlockersHandoffGlyphsAreThemed(t *testing.T) {
	wrapup := `{"tldr":"x","blockers":"API key pending","handoff_notes":"Check PR #42"}`
	b := &Bot{}
	for _, tc := range []struct {
		th                     Theme
		wantBlock, wantHandoff string
	}{
		{Garden, Garden.Semantic(SemBlockedBy) + " *Blockers:* API key pending", Garden.Semantic(SemHandoff) + " *Handoff:* Check PR #42"},
		{Business, "[BLOCKED BY] *Blockers:* API key pending", "[HANDOFF] *Handoff:* Check PR #42"},
	} {
		text := blocksToText(b.buildWrapUpAgentCardBlocks(tc.th, "test-bot", "done", wrapup, "", 0, nil))
		for _, want := range []string{tc.wantBlock, tc.wantHandoff} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: wrap-up card missing %q, got %q", tc.th.Name, want, text)
			}
		}
		for _, legacy := range []string{":no_entry:", ":handshake:"} {
			if strings.Contains(text, legacy) {
				t.Errorf("%s: wrap-up card leaked %s", tc.th.Name, legacy)
			}
		}
	}
}

func TestBuildAgentCardBlocks_TaskCostAndOwnershipGlyphsAreThemed(t *testing.T) {
	for _, tc := range []struct {
		th   Theme
		want []string
	}{
		{Garden, []string{Garden.Tool(ToolDefault) + " Fix the thing", Garden.Semantic(SemCost) + " $1.23", Garden.Semantic(SemTrigger) + " trigger: gd-T1", Garden.Semantic(SemAssignee) + " Owner: "}},
		{Business, []string{"· Fix the thing", "[COST] $1.23", "[TRIGGER] trigger: gd-T1", "[ASSIGNEE] Owner: "}},
	} {
		blocks := buildAgentCardBlocks(tc.th, "gardenio/crew/test-bot", "", 0, "working", "Fix the thing", time.Time{},
			"", "", "crew", "$1.23", "", nil, "", false,
			triggerCardInfo{ID: "gd-T1", Owner: "oidc:a@b.example"}, nil)
		var text string
		for _, blk := range blocks {
			if cb, ok := blk.(*slack.ContextBlock); ok {
				for _, e := range cb.ContextElements.Elements {
					if to, ok := e.(*slack.TextBlockObject); ok {
						text += to.Text + "\n"
					}
				}
			}
		}
		for _, want := range tc.want {
			if !strings.Contains(text, want) {
				t.Errorf("%s: agent card context missing %q, got %q", tc.th.Name, want, text)
			}
		}
		for _, legacy := range []string{":wrench:", ":moneybag:", ":zap:", ":compass:"} {
			if strings.Contains(text, legacy) {
				t.Errorf("%s: agent card leaked %s", tc.th.Name, legacy)
			}
		}
	}
}
