package surfacetest

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"

	"gardenio/controller/internal/conversation"
)

// SlackTwin is an in-process twin of the slackmock fixture
// (gardenio/tests/slackmock, its own module, so it cannot be imported here):
// the same form-or-JSON param parsing, {"ok":true,...} envelope and
// seconds.sequence ts, for the Web API methods the Slack surface and the moved
// handlers call. Keep the two in step; slackmock's main_test.go pins the same
// shapes.
//
// It records every call (method + params) so a test can pin a Slack payload
// golden: the recorded calls of a handler before and after it moves behind the
// Slack surface must be identical (design section 5.2).
type SlackTwin struct {
	mu        sync.Mutex
	calls     []TwinCall
	messages  map[string][]twinMsg // channel → messages
	reactions map[conversation.Ref]map[string]bool
	tsSeq     int64
	fileSeq   int
	scopes    string // X-OAuth-Scopes on every response; "" sends none
	// rejectBlocks names channels where a chat.postMessage with blocks is
	// refused with invalid_blocks, as Slack refuses a face it cannot render.
	rejectBlocks map[string]bool
}

// TwinCall is one recorded Web API call.
type TwinCall struct {
	Method string            `json:"method"`
	Params map[string]string `json:"params"`
}

// twinBotID is the bot_id of every message the twin holds: they were all
// posted with the bot token.
const twinBotID = "B00000000"

type twinMsg struct {
	Channel, Text, Blocks, TS, ThreadTS string
	Username                            string // a customized post's (chat:write.customize)
}

// SetScopes sets the X-OAuth-Scopes header the twin answers with, as Slack
// reports the token's scopes on every Web API response.
func (m *SlackTwin) SetScopes(scopes string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scopes = scopes
}

// RejectBlocks makes every chat.postMessage with blocks into channel fail
// with invalid_blocks (and a response_metadata message naming the block), so
// a test can drive a handler's plain-text fallback.
func (m *SlackTwin) RejectBlocks(channel string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rejectBlocks == nil {
		m.rejectBlocks = map[string]bool{}
	}
	m.rejectBlocks[channel] = true
}

// Usernames returns the username each message in channel was posted under,
// oldest first ("" for the bot's own name).
func (m *SlackTwin) Usernames(channel string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, msg := range m.messages[channel] {
		out = append(out, msg.Username)
	}
	return out
}

// historyMsg is msg as conversations.history and .replies return it.
func (msg twinMsg) historyMsg(threadTS string) map[string]any {
	out := map[string]any{"type": "message", "text": msg.Text, "ts": msg.TS, "bot_id": twinBotID}
	if threadTS != "" {
		out["thread_ts"] = threadTS
	}
	if msg.Username != "" {
		out["username"] = msg.Username
	}
	return out
}

// NewSlackTwin returns an empty twin. Its ts sequence is fixed, so recorded
// calls are stable enough to golden.
func NewSlackTwin() *SlackTwin {
	return &SlackTwin{
		messages:  map[string][]twinMsg{},
		reactions: map[conversation.Ref]map[string]bool{},
		tsSeq:     1_700_000_000,
	}
}

// Serve starts the twin on an httptest server closed at test end and returns
// the API URL to hand slack.OptionAPIURL (with the trailing slash).
func (m *SlackTwin) Serve(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(m.Handler())
	t.Cleanup(srv.Close)
	return srv.URL + "/api/"
}

// Calls returns the recorded calls, without the token param.
func (m *SlackTwin) Calls() []TwinCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]TwinCall, len(m.calls))
	for i, c := range m.calls {
		p := make(map[string]string, len(c.Params))
		for k, v := range c.Params {
			if k != "token" {
				p[k] = v
			}
		}
		out[i] = TwinCall{Method: c.Method, Params: p}
	}
	return out
}

// Reset clears recorded calls (messages stay).
func (m *SlackTwin) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = nil
}

// Lookup returns what Slack shows at ref, for RunContract.
func (m *SlackTwin) Lookup(ref conversation.Ref) (Shown, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, msg := range m.messages[ref.Channel] {
		if msg.TS != ref.TS {
			continue
		}
		s := Shown{Ref: ref, Text: msg.Text, ThreadTS: msg.ThreadTS, Reactions: map[string]bool{}}
		if s.ThreadTS == msg.TS {
			s.ThreadTS = ""
		}
		for k, v := range m.reactions[ref] {
			s.Reactions[k] = v
		}
		return s, true
	}
	return Shown{}, false
}

// Messages returns the messages in a channel, oldest first, as (ts, text,
// blocks JSON) triples.
func (m *SlackTwin) Messages(channel string) [][3]string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out [][3]string
	for _, msg := range m.messages[channel] {
		out = append(out, [3]string{msg.TS, msg.Text, msg.Blocks})
	}
	return out
}

// Channels returns every channel that holds at least one message, sorted, so
// a test can catch a post that landed somewhere it never thought to look.
func (m *SlackTwin) Channels() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for ch, msgs := range m.messages {
		if len(msgs) > 0 {
			out = append(out, ch)
		}
	}
	sort.Strings(out)
	return out
}

func (m *SlackTwin) nextTS() string {
	m.tsSeq++
	return fmt.Sprintf("%d.%06d", m.tsSeq, m.tsSeq%1000000)
}

// twinParams mirrors slackmock's parseParams: form values first (slack-go's
// default is form-encoded POST), JSON body as the fallback (views.open).
func twinParams(r *http.Request) map[string]string {
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

func twinOK(w http.ResponseWriter, extra map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"ok": true}
	for k, v := range extra {
		resp[k] = v
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// Handler serves the twin's Web API under /api/.
func (m *SlackTwin) Handler() http.Handler {
	mux := http.NewServeMux()
	handle := func(method string, fn func(p map[string]string) map[string]any) {
		mux.HandleFunc("/api/"+method, func(w http.ResponseWriter, r *http.Request) {
			m.mu.Lock()
			defer m.mu.Unlock()
			p := twinParams(r)
			m.calls = append(m.calls, TwinCall{Method: method, Params: p})
			if m.scopes != "" {
				w.Header().Set("X-OAuth-Scopes", m.scopes)
			}
			twinOK(w, fn(p))
		})
	}

	handle("auth.test", func(map[string]string) map[string]any {
		return map[string]any{
			"url": "https://slackmock.test/", "team": "E2E Test Team", "user": "slackmock-bot",
			"team_id": "T00000000", "user_id": "U00000000", "bot_id": twinBotID,
		}
	})
	handle("chat.postMessage", func(p map[string]string) map[string]any {
		if m.rejectBlocks[p["channel"]] && p["blocks"] != "" {
			return map[string]any{"ok": false, "error": "invalid_blocks",
				"response_metadata": map[string]any{"messages": []string{"[ERROR] failed to match all allowed schemas [json-pointer:/blocks/0/text]"}}}
		}
		ts := m.nextTS()
		msg := twinMsg{Channel: p["channel"], Text: p["text"], Blocks: p["blocks"], TS: ts, ThreadTS: p["thread_ts"],
			Username: p["username"]}
		m.messages[msg.Channel] = append(m.messages[msg.Channel], msg)
		return map[string]any{
			"channel": msg.Channel, "ts": ts, "message_ts": ts,
			"message": map[string]any{"text": msg.Text, "ts": ts, "thread_ts": msg.ThreadTS, "bot_id": twinBotID},
		}
	})
	handle("chat.update", func(p map[string]string) map[string]any {
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
		return map[string]any{"channel": ch, "ts": ts}
	})
	handle("chat.delete", func(p map[string]string) map[string]any {
		ch, ts := p["channel"], p["ts"]
		msgs := m.messages[ch]
		for i, msg := range msgs {
			if msg.TS == ts {
				m.messages[ch] = append(msgs[:i], msgs[i+1:]...)
				break
			}
		}
		return map[string]any{"channel": ch, "ts": ts}
	})
	handle("chat.postEphemeral", func(map[string]string) map[string]any {
		return map[string]any{"message_ts": m.nextTS()}
	})
	react := func(on bool) func(p map[string]string) map[string]any {
		return func(p map[string]string) map[string]any {
			ref := conversation.Ref{Surface: conversation.SurfaceSlack, Channel: p["channel"], TS: p["timestamp"]}
			if m.reactions[ref] == nil {
				m.reactions[ref] = map[string]bool{}
			}
			if on {
				m.reactions[ref][p["name"]] = true
			} else {
				delete(m.reactions[ref], p["name"])
			}
			return nil
		}
	}
	handle("reactions.add", react(true))
	handle("reactions.remove", react(false))
	handle("conversations.open", func(p map[string]string) map[string]any {
		users := strings.Split(p["users"], ",")
		sort.Strings(users)
		return map[string]any{"channel": map[string]any{"id": "D" + strings.Join(users, "")}}
	})
	handle("conversations.replies", func(p map[string]string) map[string]any {
		var replies []map[string]any
		for _, msg := range m.messages[p["channel"]] {
			if msg.ThreadTS == p["ts"] || msg.TS == p["ts"] {
				replies = append(replies, msg.historyMsg(p["ts"]))
			}
		}
		return map[string]any{"messages": replies, "has_more": false}
	})
	// Top-level messages newer than oldest, newest first, as slackmock's.
	// Every message here was posted by the bot.
	handle("conversations.history", func(p map[string]string) map[string]any {
		var out []map[string]any
		msgs := m.messages[p["channel"]]
		for i := len(msgs) - 1; i >= 0; i-- {
			msg := msgs[i]
			if (msg.ThreadTS != "" && msg.ThreadTS != msg.TS) || (p["oldest"] != "" && msg.TS <= p["oldest"]) {
				continue
			}
			out = append(out, msg.historyMsg(""))
		}
		return map[string]any{"messages": out, "has_more": false, "response_metadata": map[string]any{}}
	})
	handle("users.info", func(p map[string]string) map[string]any {
		return map[string]any{"user": map[string]any{
			"id": p["user"], "name": "mockuser", "real_name": "Mock User",
			"profile": map[string]any{"display_name": "Mock User", "real_name": "Mock User",
				"image_72": "https://slackmock.test/avatar/" + p["user"] + "_72.png"},
		}}
	})
	handle("views.open", func(map[string]string) map[string]any {
		return map[string]any{"view": map[string]any{"id": "V00000001", "type": "modal"}}
	})

	// The files.upload v2 flow: getUploadURLExternal hands out an upload URL
	// on the twin itself, the client posts the bytes there (recorded as an
	// "upload" call), and completeUploadExternal shares the files.
	mux.HandleFunc("/api/files.getUploadURLExternal", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		p := twinParams(r)
		m.calls = append(m.calls, TwinCall{Method: "files.getUploadURLExternal", Params: p})
		m.fileSeq++
		id := fmt.Sprintf("F%08d", m.fileSeq)
		twinOK(w, map[string]any{"upload_url": "http://" + r.Host + "/upload/" + id, "file_id": id})
	})
	mux.HandleFunc("/upload/", func(w http.ResponseWriter, r *http.Request) {
		p := map[string]string{"file_id": strings.TrimPrefix(r.URL.Path, "/upload/")}
		if f, h, err := r.FormFile("file"); err == nil {
			data, _ := io.ReadAll(f)
			_ = f.Close()
			p["filename"], p["content"] = h.Filename, string(data)
		}
		m.mu.Lock()
		m.calls = append(m.calls, TwinCall{Method: "upload", Params: p})
		m.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	handle("files.completeUploadExternal", func(p map[string]string) map[string]any {
		var files []map[string]any
		_ = json.Unmarshal([]byte(p["files"]), &files)
		return map[string]any{"files": files}
	})
	return mux
}
