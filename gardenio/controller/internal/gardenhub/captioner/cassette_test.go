package captioner

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gardenio/controller/internal/claudemore"
)

// The cassettes (internal/claudemore's format: a canonical, redacted request keyed by its hash, and the response to
// serve) pin the captioner's wire: a change to the prompt, the model, the bounds sent or the command scrubbing
// changes a request hash, and replay fails until the cassettes are written again.
//
//	go test ./internal/gardenhub/captioner/ -run TestCassettes -update
//
// writes them from cassetteCases' replies. With CAPTION_CASSETTE_LIVE_URL and CAPTION_CASSETTE_LIVE_KEY set (a
// LiteLLM base and a virtual key), -update records live Haiku instead and prints its captions.
var update = flag.Bool("update", false, "write testdata/cassettes")

const cassetteDir = "testdata/cassettes"

// cassetteCases are description-less Bash commands of the shapes the spec's sample found (§2.3): cd-led, a scratch
// variable, git, kd, a pipeline, one with a planted secret.
var cassetteCases = []struct {
	name, command, reply, want string
}{
	{"cd-git-log", "cd /home/agent/bot/gardenio/work && git log --oneline -5 origin/main",
		"Show recent commits on main", "Show recent commits on main"},
	{"scratch-var", "S=/home/agent/.claude-tmp/claude-1000/0f3e/scratchpad; grep -c PreToolUse $S/hook_events.jsonl",
		"Count PreToolUse events", "Count PreToolUse events"},
	{"kd-show", "kd show gd-gCHKhIROvB | sed -n 1,12p",
		"Show the first lines of a bead", "Show the first lines of a bead"},
	{"pipeline", "git diff --stat origin/main...HEAD | tail -3",
		"Summarize diff against main", "Summarize diff against main"},
	{"planted-secret", "curl -s -H 'Authorization: Bearer sk-ant-fake-key-planted0123' https://litellm.internal/key/info",
		"Check the LiteLLM key info", "Check the LiteLLM key info"},
	{"reply-with-path", "wc -l /home/agent/bot/gardenio/work/gardenio/controller/internal/gardenhub/activity.go",
		"Count lines in /home/agent/bot/gardenio/work/gardenio/controller/internal/gardenhub/activity.go",
		"Count lines in gardenhub/activity.go"},
}

// cassetteHeaders are the request headers that key a cassette (claudemore's allowlist reads anthropic-version).
func cassetteHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k := range h {
		out[strings.ToLower(k)] = h.Get(k)
	}
	return out
}

func loadCassettes(t *testing.T) map[string]claudemore.Cassette {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(cassetteDir, "*.json"))
	out := map[string]claudemore.Cassette{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		c, err := claudemore.UnmarshalCassette(b)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		out[c.RequestHash] = c
	}
	return out
}

// replayServer serves each request the cassette its hash names, and 599 for one with none.
func replayServer(t *testing.T, cs map[string]claudemore.Cassette) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_, _, hash, err := claudemore.CanonicalizeRequest(claudemore.RequestIdentity{
			Method: r.Method, Path: r.URL.Path, Headers: cassetteHeaders(r.Header), Body: body,
		})
		c, ok := cs[hash]
		if err != nil || !ok {
			t.Errorf("no cassette for request %s (re-record with -update):\n%s", hash, body)
			w.WriteHeader(599)
			return
		}
		for k, v := range c.Response.Headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(c.Response.StatusCode)
		_, _ = w.Write(c.Response.Body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// recordServer answers each case's reply (or proxies to live Haiku) and writes the round trip as a cassette.
func recordServer(t *testing.T, replies map[string]string, names map[string]string) *httptest.Server {
	liveURL, liveKey := os.Getenv("CAPTION_CASSETTE_LIVE_URL"), os.Getenv("CAPTION_CASSETTE_LIVE_KEY")
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req messagesReq
		_ = json.Unmarshal(body, &req)
		prepared := req.Messages[0].Content
		status, respBody, respHeaders := http.StatusOK, []byte(nil), map[string]string{"content-type": "application/json"}
		if liveURL != "" {
			up, _ := http.NewRequest(http.MethodPost, strings.TrimRight(liveURL, "/")+"/v1/messages", bytes.NewReader(body))
			for _, h := range []string{"Content-Type", "anthropic-version", "x-litellm-tags"} {
				up.Header.Set(h, r.Header.Get(h))
			}
			up.Header.Set("Authorization", "Bearer "+liveKey)
			resp, err := http.DefaultClient.Do(up)
			if err != nil {
				t.Fatalf("live: %v", err)
			}
			respBody, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
			status = resp.StatusCode
			fmt.Printf("live %s: %s\n", names[prepared], respBody)
		} else {
			respBody, _ = json.Marshal(map[string]any{
				"id": "msg_cassette", "type": "message", "role": "assistant", "model": req.Model,
				"content":     []map[string]string{{"type": "text", "text": replies[prepared]}},
				"stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 80, "output_tokens": 8},
			})
		}
		creq, model, hash, err := claudemore.CanonicalizeRequest(claudemore.RequestIdentity{
			Method: r.Method, Path: r.URL.Path, Headers: cassetteHeaders(r.Header), Body: body,
		})
		if err != nil {
			t.Fatal(err)
		}
		c := claudemore.NewCassette(creq, model, hash, claudemore.CassetteResponse{
			StatusCode: status, Headers: respHeaders, Body: claudemore.RedactBytes(respBody),
		}, time.Date(2026, 10, 28, 0, 0, 0, 0, time.UTC).UnixMilli())
		out, err := c.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		var pretty bytes.Buffer
		_ = json.Indent(&pretty, out, "", "  ")
		mu.Lock()
		err = os.WriteFile(filepath.Join(cassetteDir, names[prepared]+".json"), append(pretty.Bytes(), '\n'), 0o644)
		mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write(respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestCassettes(t *testing.T) {
	var srv *httptest.Server
	if *update {
		if err := os.MkdirAll(cassetteDir, 0o755); err != nil {
			t.Fatal(err)
		}
		replies, names := map[string]string{}, map[string]string{}
		for _, c := range cassetteCases {
			replies[PrepareCommand(c.command)] = c.reply
			names[PrepareCommand(c.command)] = c.name
		}
		srv = recordServer(t, replies, names)
	} else {
		srv = replayServer(t, loadCassettes(t))
	}
	cfg := DefaultConfig()
	cfg.Enabled, cfg.BaseURL, cfg.APIKey = true, srv.URL, "sk-cassette-key"
	k := New(cfg)
	for _, c := range cassetteCases {
		var late lateRec
		if _, _, pending := k.Caption(c.command, late.fn); !pending {
			t.Fatalf("%s: no call went out", c.name)
		}
		k.calls.Wait()
		if *update && os.Getenv("CAPTION_CASSETTE_LIVE_URL") != "" {
			continue // live captions vary: they are printed, not asserted
		}
		if got := late.got(); len(got) != 1 || got[0] != c.want {
			t.Errorf("%s: caption %q, want %q", c.name, got, c.want)
		}
	}
	if *update {
		return
	}
	if n := counted(k, SrcHaiku); n != len(cassetteCases) {
		t.Errorf("haiku = %d, want %d", n, len(cassetteCases))
	}
}

// TestCassettesHoldOnlyTheScrubbedCommand reads the recorded requests themselves: each carries the fixed prompt and
// the scrubbed command as its one message, and no planted path, secret or credential header.
func TestCassettesHoldOnlyTheScrubbedCommand(t *testing.T) {
	byName := map[string]string{}
	for _, c := range cassetteCases {
		byName[c.name] = PrepareCommand(c.command)
	}
	files, _ := filepath.Glob(filepath.Join(cassetteDir, "*.json"))
	if len(files) != len(cassetteCases) {
		t.Fatalf("%d cassettes, want %d (re-record with -update)", len(files), len(cassetteCases))
	}
	for _, f := range files {
		raw, _ := os.ReadFile(f)
		c, err := claudemore.UnmarshalCassette(raw)
		if err != nil {
			t.Fatal(err)
		}
		var req messagesReq
		if err := json.Unmarshal(c.Request.Body, &req); err != nil {
			t.Fatal(err)
		}
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		if len(req.Messages) != 1 || req.Messages[0].Content != byName[name] || req.System != SystemPrompt ||
			req.MaxTokens != 32 || req.Temperature != 0 || req.Model != "anthropic/claude-haiku-4-5" {
			t.Errorf("%s: request %+v", name, req)
		}
		for _, planted := range []string{"/home/agent", "scratchpad", "planted0123", "sk-cassette-key", "Bearer sk-"} {
			if bytes.Contains(raw, []byte(planted)) {
				t.Errorf("%s carries %q", name, planted)
			}
		}
		for h := range c.Request.Headers {
			if h == "authorization" || strings.HasPrefix(h, "x-litellm") {
				t.Errorf("%s stores header %q", name, h)
			}
		}
	}
}
