package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"

	"gardenio/controller/internal/beadsapi"
)

// slackPost is one chat.postMessage or chat.postEphemeral the bridge sent.
type slackPost struct {
	method, channel, threadTS, text string
}

// reasonSlack is a fake Slack API that records every post. A post to a
// channel in refuse answers not_in_channel.
type reasonSlack struct {
	mu     sync.Mutex
	posts  []slackPost
	refuse map[string]bool
}

func newReasonSlack(t *testing.T) (*reasonSlack, *httptest.Server) {
	t.Helper()
	rs := &reasonSlack{refuse: map[string]bool{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		method := strings.TrimPrefix(r.URL.Path, "/")
		if method == "chat.postMessage" || method == "chat.postEphemeral" {
			ch := r.FormValue("channel")
			rs.mu.Lock()
			refused := rs.refuse[ch]
			if !refused {
				rs.posts = append(rs.posts, slackPost{method: method, channel: ch, threadTS: r.FormValue("thread_ts"), text: r.FormValue("text")})
			}
			rs.mu.Unlock()
			if refused {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": "not_in_channel"})
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "ts": "1700.9999", "message_ts": "1700.9999", "channel": r.FormValue("channel")})
	}))
	t.Cleanup(srv.Close)
	return rs, srv
}

func (rs *reasonSlack) all() []slackPost {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return append([]slackPost(nil), rs.posts...)
}

// mention runs a top-level @gardenio mention of text in channel.
func mention(b *Bot, channel, ts, text string) {
	b.handleSlackMention(context.Background(), &slackevents.AppMentionEvent{
		Channel:   channel,
		User:      "U-ASKER",
		Text:      "<@U-BOT>" + text,
		TimeStamp: ts,
	})
}

func onlyPost(t *testing.T, rs *reasonSlack) slackPost {
	t.Helper()
	posts := rs.all()
	if len(posts) != 1 {
		t.Fatalf("want exactly one post, got %d: %+v", len(posts), posts)
	}
	return posts[0]
}

func TestMention_ConflictingChannel_NamesTheProjects(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("alpha", "C-SHARED")
	daemon.seedProjectWithChannel("beta", "C-SHARED")
	rs, srv := newReasonSlack(t)
	b := newTestBot(daemon, srv)
	b.botUserID = "U-BOT"

	mention(b, "C-SHARED", "1700.0001", " please look at the build")

	p := onlyPost(t, rs)
	if p.channel != "C-SHARED" || p.threadTS != "1700.0001" {
		t.Errorf("reason posted at %s/%s, want the mention's thread C-SHARED/1700.0001", p.channel, p.threadTS)
	}
	if !strings.Contains(p.text, "claimed by more than one project (alpha, beta)") {
		t.Errorf("reason = %q, want it to name both projects", p.text)
	}
	if strings.Contains(p.text, "no project is mapped") {
		t.Errorf("reason = %q, must not say no project is mapped", p.text)
	}
	if n := len(filterAgentBeads(daemon.beads)); n != 0 {
		t.Errorf("spawned %d agent beads, want none", n)
	}
}

func TestMention_DaemonError_SaysTryAgain(t *testing.T) {
	daemon := newMockDaemon()
	daemon.listProjectsErr = errors.New("connection refused")
	rs, srv := newReasonSlack(t)
	b := newTestBot(daemon, srv)
	b.botUserID = "U-BOT"

	mention(b, "C-ANY", "1700.0002", " please look at the build")

	p := onlyPost(t, rs)
	if p.threadTS != "1700.0002" {
		t.Errorf("reason threaded under %q, want 1700.0002", p.threadTS)
	}
	if !strings.Contains(p.text, "couldn't read the project list") || !strings.Contains(p.text, "Try again") {
		t.Errorf("reason = %q, want a daemon error and try again", p.text)
	}
	if strings.Contains(p.text, "no project is mapped") || strings.Contains(p.text, "connection refused") {
		t.Errorf("reason = %q, must not blame the mapping or leak the error", p.text)
	}
}

func TestMention_UnmappedChannel_StillSaysNoProject(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("alpha", "C-ALPHA")
	rs, srv := newReasonSlack(t)
	b := newTestBot(daemon, srv)
	b.botUserID = "U-BOT"

	mention(b, "C-OTHER", "1700.0003", " hello")

	if p := onlyPost(t, rs); !strings.Contains(p.text, "no project is mapped") {
		t.Errorf("reason = %q, want no project is mapped", p.text)
	}
}

func TestMention_Empty_OneLineOnce(t *testing.T) {
	daemon := newMockDaemon()
	rs, srv := newReasonSlack(t)
	b := newTestBot(daemon, srv)
	b.botUserID = "U-BOT"

	ev := &slackevents.AppMentionEvent{Channel: "C-ANY", User: "U-ASKER", Text: "<@U-BOT>  ", TimeStamp: "1700.0005", ThreadTimeStamp: "1700.0004"}
	b.handleSlackMention(context.Background(), ev)
	again := *ev
	again.TimeStamp = "1700.0006"
	b.handleSlackMention(context.Background(), &again)

	p := onlyPost(t, rs)
	if p.method != "chat.postEphemeral" {
		t.Errorf("empty mention answered with %s, want an ephemeral", p.method)
	}
	if p.threadTS != "1700.0004" {
		t.Errorf("ephemeral threaded under %q, want 1700.0004", p.threadTS)
	}
	if !strings.Contains(p.text, "no message") {
		t.Errorf("reason = %q, want it to say the mention had no message", p.text)
	}
	if n := len(filterAgentBeads(daemon.beads)); n != 0 {
		t.Errorf("spawned %d agent beads, want none", n)
	}
}

func TestRespawn_ConflictingChannel_NamesTheProjects(t *testing.T) {
	daemon := newMockDaemon()
	daemon.seedProjectWithChannel("alpha", "C-SHARED")
	daemon.seedProjectWithChannel("beta", "C-SHARED")
	rs, srv := newReasonSlack(t)
	b := newTestBot(daemon, srv)

	b.respawnThreadAgent(context.Background(), "C-SHARED", "1700.0007", "thread-1700-0007", "keep going", "U-ASKER")

	if p := onlyPost(t, rs); !strings.Contains(p.text, "Cannot resume agent — this channel is claimed by more than one project (alpha, beta)") {
		t.Errorf("reason = %q, want the conflicting projects", p.text)
	}
}

// seedCrewFromThread seeds a crew agent bead whose thread head records the
// Slack thread it was started from.
func seedCrewFromThread(daemon *mockDaemon, agent, threadChannel, threadTS string) {
	daemon.mu.Lock()
	defer daemon.mu.Unlock()
	daemon.beads["head-1"] = &beadsapi.BeadDetail{
		ID: "head-1", Type: "task", Status: "open",
		Fields: map[string]string{"slack_thread_channel": threadChannel, "slack_thread_ts": threadTS},
	}
	daemon.beads[agent] = &beadsapi.BeadDetail{
		ID: "bead-" + agent, Title: agent, Type: "agent", Status: "open",
		Labels: []string{"project:gardenio"},
		Fields: map[string]string{"agent": agent, "project": "gardenio", "role": "crew", "thread_id": "head-1"},
	}
}

func TestEnsureAgentCard_NotInChannel_TellsTheOriginThreadOnce(t *testing.T) {
	daemon := newMockDaemon()
	seedCrewFromThread(daemon, "crew-a", "C-THREAD", "1700.0010")
	rs, srv := newReasonSlack(t)
	rs.refuse["C-ROLE"] = true
	b := newTestBot(daemon, srv)

	for i := 0; i < 3; i++ {
		if _, err := b.ensureAgentCard(context.Background(), "crew-a", "C-ROLE"); err == nil {
			t.Fatal("ensureAgentCard into a channel the bot is not in: want an error")
		}
	}

	p := onlyPost(t, rs)
	if p.channel != "C-THREAD" || p.threadTS != "1700.0010" {
		t.Errorf("reason posted at %s/%s, want the origin thread C-THREAD/1700.0010", p.channel, p.threadTS)
	}
	if !strings.Contains(p.text, "crew-a") || !strings.Contains(p.text, "<#C-ROLE>") || !strings.Contains(p.text, "not a member") {
		t.Errorf("reason = %q, want the agent, the channel and not a member", p.text)
	}
}

func TestNoteCardRefused_Skips(t *testing.T) {
	notIn := fmt.Errorf("post agent card: %w", slack.SlackErrorResponse{Err: "not_in_channel"})
	cases := []struct {
		name    string
		channel string
		err     error
	}{
		{"origin thread is in the refused channel", "C-THREAD", notIn},
		{"another Slack error", "C-ROLE", slack.SlackErrorResponse{Err: "ratelimited"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			daemon := newMockDaemon()
			seedCrewFromThread(daemon, "crew-a", "C-THREAD", "1700.0010")
			rs, srv := newReasonSlack(t)
			b := newTestBot(daemon, srv)
			b.noteCardRefused(context.Background(), "crew-a", tc.channel, tc.err)
			if posts := rs.all(); len(posts) != 0 {
				t.Errorf("want no post, got %+v", posts)
			}
		})
	}
}

func TestChannelProjectMissReason(t *testing.T) {
	if got := (channelProjectMiss{}).reason(); got != "" {
		t.Errorf("plain miss reason = %q, want empty", got)
	}
	if got := (channelProjectMiss{err: errors.New("x")}).reason(); !strings.Contains(got, "Try again") {
		t.Errorf("daemon error reason = %q", got)
	}
	if got := (channelProjectMiss{conflicts: []string{"a", "b"}}).reason(); !strings.Contains(got, "(a, b)") {
		t.Errorf("conflict reason = %q", got)
	}
}
