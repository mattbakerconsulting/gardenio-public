// slackmock_loop_e2e_test.go — the slackmock VALIDATION LOOP, closed end-to-end
// through the REAL production Slack client the bridge builds from SLACK_API_URL.
//
// ── The gap this closes (task gd-RhOuCeCYUC, epic gd-L1HXcTGzDV §C1) ─────────
//   slackmock (gardenio/tests/slackmock) is the outbound-fixture that lets E2E
//   exercise the bridge's Slack posting without flooding real channels, and the
//   bridge already honours SLACK_API_URL to point its Slack client at that mock
//   (buildWebhookSlackClient / buildBotConfig APIURL → slackapi.OptionAPIURL,
//   logged by shouldLogSlackAPIOverride). But NOTHING closed the loop:
//     • SLACK_API_URL is set in NO ci/e2e/helm flow — `grep -rn SLACK_API_URL
//       gardenio/tests gardenio/helm .github` returns 0 non-test hits — so the
//       override that redirects prod posting to the fixture was never once
//       exercised against a running mock end-to-end.
//     • buildWebhookSlackClient had ONLY a nil/non-nil unit assertion
//       (TestBuildWebhookSlackClient); no test ever POSTED through the client it
//       returns and read the message back off the fixture. A regression that
//       dropped OptionAPIURL (client posts to real slack.com), or a fixture
//       protocol drift (form-encoding, ok:true envelope, ts shape), stays green
//       under the unit test while silently breaking every mock-backed module.
//   This file is that loop: it starts the REAL slackmock HTTP surface in-process
//   and drives the REAL client from buildWebhookSlackClient(&config{slackAPIURL:
//   mock}) — the exact constructor main() uses — through postMessage → update →
//   reaction → history read-back, asserting BOTH that the client honoured the
//   override (the fixture, not slack.com, saw the calls) AND that the fixture's
//   recorded state round-trips.
//
// ── Why in-process, not the standalone binary ────────────────────────────────
//   slackmock is its own go module (`module slackmock`, NOT in go.work), so it
//   cannot be imported here. This file re-mounts the SAME Slack Web API surface
//   the binary serves (auth.test / chat.postMessage / chat.update / reactions.add
//   / conversations.history + the /mock query endpoints) on an httptest server,
//   from a self-contained recorder — a faithful, dependency-free twin of
//   slackmock/main.go's handlers. The sibling bash module
//   (test-slackmock-loop.sh) covers the ACTUAL compiled binary + lib-slackmock.sh
//   query helpers, so the compiled-artifact path is not left unproven; this Go
//   leg proves the PRODUCTION-CLIENT half of the loop with zero external deps, so
//   it is a hermetic HARD gate that is green today.
//
// Hermetic: no pods, no cluster, no network egress, no shared-daemon writes.

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/slack-go/slack"
)

// ── A faithful, self-contained twin of gardenio/tests/slackmock/main.go ───────
// Only the surface these legs drive is implemented; the shapes (form-encoded
// params, {"ok":true,...} envelope, seconds.sequence ts, newest-first history)
// mirror the real fixture so a drift on either side is caught here.

type loopMockCall struct {
	Method string
	Params map[string]string
}

type loopMockMsg struct {
	Channel  string
	Text     string
	Blocks   string
	TS       string
	ThreadTS string
	BotID    string
}

type loopMock struct {
	mu       sync.Mutex
	calls    []loopMockCall
	messages map[string][]loopMockMsg
	tsSeq    int64
}

func newLoopMock() *loopMock {
	return &loopMock{messages: make(map[string][]loopMockMsg), tsSeq: 1_700_000_000}
}

func (m *loopMock) nextTS() string {
	m.tsSeq++
	return strconv.FormatInt(m.tsSeq, 10) + "." + strconv.Itoa(int(m.tsSeq%1_000_000))
}

func (m *loopMock) record(method string, p map[string]string) {
	m.calls = append(m.calls, loopMockCall{Method: method, Params: p})
}

// parseParams mirrors slackmock's parseParams: form values first (slack-go's
// default is form-encoded POST), JSON body as fallback.
func loopParseParams(r *http.Request) map[string]string {
	p := make(map[string]string)
	if err := r.ParseForm(); err == nil {
		for k, vs := range r.Form {
			if len(vs) > 0 {
				p[k] = vs[0]
			}
		}
	}
	if len(p) == 0 || (len(p) == 1 && p["token"] != "") {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			for k, v := range body {
				if s, ok := v.(string); ok {
					p[k] = s
				} else {
					b, _ := json.Marshal(v)
					p[k] = string(b)
				}
			}
		}
	}
	return p
}

func loopJSONOK(w http.ResponseWriter, extra map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"ok": true}
	for k, v := range extra {
		resp[k] = v
	}
	_ = json.NewEncoder(w).Encode(resp)
}

func (m *loopMock) mux() *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/auth.test", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.record("auth.test", loopParseParams(r))
		loopJSONOK(w, map[string]any{
			"url": "https://slackmock.test/", "team": "E2E Test Team",
			"user": "slackmock-bot", "team_id": "T00000000",
			"user_id": "U00000000", "bot_id": "B00000000",
		})
	})

	mux.HandleFunc("/api/chat.postMessage", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		p := loopParseParams(r)
		m.record("chat.postMessage", p)
		ts := m.nextTS()
		msg := loopMockMsg{
			Channel: p["channel"], Text: p["text"], Blocks: p["blocks"],
			TS: ts, ThreadTS: p["thread_ts"], BotID: "B00000000",
		}
		m.messages[msg.Channel] = append(m.messages[msg.Channel], msg)
		loopJSONOK(w, map[string]any{
			"channel": msg.Channel, "ts": ts, "message_ts": ts,
			"message": map[string]any{"text": msg.Text, "ts": ts, "thread_ts": msg.ThreadTS, "bot_id": msg.BotID},
		})
	})

	mux.HandleFunc("/api/chat.update", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		p := loopParseParams(r)
		m.record("chat.update", p)
		ch, ts := p["channel"], p["ts"]
		for i, msg := range m.messages[ch] {
			if msg.TS == ts {
				if t, ok := p["text"]; ok {
					m.messages[ch][i].Text = t
				}
				if b, ok := p["blocks"]; ok {
					m.messages[ch][i].Blocks = b
				}
				break
			}
		}
		loopJSONOK(w, map[string]any{"channel": ch, "ts": ts})
	})

	mux.HandleFunc("/api/reactions.add", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.record("reactions.add", loopParseParams(r))
		loopJSONOK(w, nil)
	})

	mux.HandleFunc("/api/conversations.history", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		p := loopParseParams(r)
		m.record("conversations.history", p)
		ch := p["channel"]
		var filtered []loopMockMsg
		for _, msg := range m.messages[ch] {
			if msg.ThreadTS != "" && msg.ThreadTS != msg.TS {
				continue // top-level only, like the real fixture
			}
			filtered = append(filtered, msg)
		}
		sort.Slice(filtered, func(i, j int) bool { return filtered[i].TS > filtered[j].TS })
		respMsgs := make([]map[string]any, len(filtered))
		for i, msg := range filtered {
			mm := map[string]any{"type": "message", "text": msg.Text, "ts": msg.TS, "bot_id": msg.BotID}
			if msg.Blocks != "" {
				var blocks []any
				if json.Unmarshal([]byte(msg.Blocks), &blocks) == nil {
					mm["blocks"] = blocks
				}
			}
			respMsgs[i] = mm
		}
		loopJSONOK(w, map[string]any{"messages": respMsgs, "has_more": false, "response_metadata": map[string]any{}})
	})

	// /mock query surface — the assertion side of lib-slackmock.sh.
	mux.HandleFunc("/mock/messages", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		ch := r.URL.Query().Get("channel")
		search := r.URL.Query().Get("search")
		var out []loopMockMsg
		for c, msgs := range m.messages {
			if ch != "" && c != ch {
				continue
			}
			for _, msg := range msgs {
				if search != "" && !strings.Contains(msg.Text, search) && !strings.Contains(msg.Blocks, search) {
					continue
				}
				out = append(out, msg)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"total": len(out)})
	})

	mux.HandleFunc("/mock/calls", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		method := r.URL.Query().Get("method")
		n := 0
		for _, c := range m.calls {
			if method == "" || c.Method == method {
				n++
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"total": n})
	})

	return mux
}

func (m *loopMock) callCount(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.calls {
		if c.Method == method {
			n++
		}
	}
	return n
}

func (m *loopMock) lastParams(method string) map[string]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.calls) - 1; i >= 0; i-- {
		if m.calls[i].Method == method {
			return m.calls[i].Params
		}
	}
	return nil
}

// ── Leg 1: the production client honours SLACK_API_URL → the fixture, not
// slack.com, receives the posts; and the full outbound round-trip round-trips.
//
// This is the loop the task asks for: it drives buildWebhookSlackClient — the
// EXACT constructor main() uses (main.go: buildWebhookSlackClient(cfg) in
// webhook mode; buildBotConfig's APIURL in socket mode share the same
// slackapi.OptionAPIURL wiring in helpers.go) — so a regression that stops
// threading SLACK_API_URL into the client reddens here.
func TestSlackmockLoopE2E_ProductionClientHonoursOverride(t *testing.T) {
	mock := newLoopMock()
	srv := httptest.NewServer(mock.mux())
	defer srv.Close()

	// The production constructor, with the override main() reads from env.
	client := buildWebhookSlackClient(&config{slackBotToken: "xoxb-test", slackAPIURL: srv.URL + "/api/"})
	if client == nil {
		t.Fatal("buildWebhookSlackClient returned nil for token+url — cannot drive the loop")
	}

	const channel = "C0LOOPTEST"
	const marker = "slackmock-loop probe gd-RhOuCeCYUC"

	// auth.test — proves the client is talking to the fixture at all.
	authResp, err := client.AuthTest()
	if err != nil {
		t.Fatalf("AuthTest through the override failed (client not pointed at fixture?): %v", err)
	}
	if authResp.Team != "E2E Test Team" {
		t.Fatalf("AuthTest team = %q, want the fixture's %q — the client reached the WRONG endpoint", authResp.Team, "E2E Test Team")
	}

	// postMessage — the outbound path every card/squawk/thread reply uses.
	_, ts, err := client.PostMessage(channel, slack.MsgOptionText(marker, false))
	if err != nil {
		t.Fatalf("PostMessage through the override failed: %v", err)
	}
	if ts == "" {
		t.Fatal("PostMessage returned empty ts — fixture response shape drift")
	}
	if got := mock.callCount("chat.postMessage"); got != 1 {
		t.Fatalf("fixture recorded %d chat.postMessage calls, want 1 — the client did NOT post to the fixture", got)
	}
	if p := mock.lastParams("chat.postMessage"); p["channel"] != channel || p["text"] != marker {
		t.Fatalf("fixture recorded chat.postMessage params channel=%q text=%q, want channel=%q text=%q",
			p["channel"], p["text"], channel, marker)
	}

	// chat.update — the card-lifecycle path (working → done rewrites).
	const updated = marker + " (updated)"
	if _, _, _, err := client.UpdateMessage(channel, ts, slack.MsgOptionText(updated, false)); err != nil {
		t.Fatalf("UpdateMessage through the override failed: %v", err)
	}
	if got := mock.callCount("chat.update"); got != 1 {
		t.Fatalf("fixture recorded %d chat.update calls, want 1", got)
	}

	// reactions.add — the lifecycle-emoji path.
	if err := client.AddReaction("white_check_mark", slack.NewRefToMessage(channel, ts)); err != nil {
		t.Fatalf("AddReaction through the override failed: %v", err)
	}
	if got := mock.callCount("reactions.add"); got != 1 {
		t.Fatalf("fixture recorded %d reactions.add calls, want 1", got)
	}

	// conversations.history — the read-back path the bash assertions use; must
	// reflect the UPDATE, not the original text.
	hist, err := client.GetConversationHistory(&slack.GetConversationHistoryParameters{ChannelID: channel, Limit: 10})
	if err != nil {
		t.Fatalf("GetConversationHistory through the override failed: %v", err)
	}
	if len(hist.Messages) != 1 {
		t.Fatalf("history returned %d messages, want 1", len(hist.Messages))
	}
	if hist.Messages[0].Text != updated {
		t.Fatalf("history text = %q, want the updated %q — chat.update did not round-trip through the fixture",
			hist.Messages[0].Text, updated)
	}
}

// ── Leg 2: anti-vacuity control. Without the override, the SAME constructor
// yields a client pointed at real slack.com — so it must NOT reach the fixture.
// This proves Leg 1's success is caused by the override, not by the fixture
// answering every possible endpoint. We assert the wiring (no override → the
// fixture records nothing) WITHOUT egressing: an empty-URL client that we never
// call cannot touch the mock, and a non-empty-URL client that we DO call must.
func TestSlackmockLoopE2E_NoOverrideDoesNotReachFixture(t *testing.T) {
	mock := newLoopMock()
	srv := httptest.NewServer(mock.mux())
	defer srv.Close()

	// No slackAPIURL: the production client is built against real slack.com.
	noOverride := buildWebhookSlackClient(&config{slackBotToken: "xoxb-test"})
	if noOverride == nil {
		t.Fatal("buildWebhookSlackClient returned nil for token-only")
	}
	// shouldLogSlackAPIOverride is the production predicate main() uses to decide
	// whether posting is mock-directed. It must be false here and true with the
	// override — this is the observable that gates the whole loop.
	if shouldLogSlackAPIOverride(&config{slackBotToken: "xoxb-test"}) {
		t.Fatal("shouldLogSlackAPIOverride true with NO slackAPIURL — override misdetected; loop would silently hit real Slack")
	}
	if !shouldLogSlackAPIOverride(&config{slackBotToken: "xoxb-test", slackAPIURL: srv.URL + "/api/"}) {
		t.Fatal("shouldLogSlackAPIOverride false WITH slackAPIURL — override undetected; the mock redirect is dead")
	}

	// The fixture has seen nothing: no client was pointed at it yet.
	if got := mock.callCount("chat.postMessage"); got != 0 {
		t.Fatalf("fixture recorded %d calls before any override client posted — non-hermetic", got)
	}

	// Now an override client DOES reach it — the positive half of the control.
	override := buildWebhookSlackClient(&config{slackBotToken: "xoxb-test", slackAPIURL: srv.URL + "/api/"})
	if _, _, err := override.PostMessage("C0CTRL", slack.MsgOptionText("control", false)); err != nil {
		t.Fatalf("override client PostMessage failed: %v", err)
	}
	if got := mock.callCount("chat.postMessage"); got != 1 {
		t.Fatalf("override client posted but fixture recorded %d calls, want 1", got)
	}
}
