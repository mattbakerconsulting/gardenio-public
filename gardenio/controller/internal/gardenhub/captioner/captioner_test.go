package captioner

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeHaiku is a Messages API that answers reply, after delay, and keeps every request.
type fakeHaiku struct {
	mu      sync.Mutex
	reply   string
	status  int
	delay   time.Duration
	hold    chan struct{} // when set, every request waits for it to close
	bodies  [][]byte
	headers []http.Header
}

func (f *fakeHaiku) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	f.bodies = append(f.bodies, body)
	f.headers = append(f.headers, r.Header.Clone())
	reply, status, delay, hold := f.reply, f.status, f.delay, f.hold
	f.mu.Unlock()
	if hold != nil {
		<-hold
	}
	select {
	case <-time.After(delay):
	case <-r.Context().Done():
		return
	}
	if status != 0 && status != http.StatusOK {
		http.Error(w, "upstream", status)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "message", "role": "assistant",
		"content": []map[string]string{{"type": "text", "text": reply}},
		"usage":   map[string]int{"input_tokens": 290, "output_tokens": 6},
	})
}

func (f *fakeHaiku) requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.bodies)
}

// clock is a settable time.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTest(t *testing.T, f *fakeHaiku, mod func(*Config), opts ...Option) (*Captioner, *clock) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cfg := DefaultConfig()
	cfg.Enabled, cfg.BaseURL, cfg.APIKey = true, srv.URL, "sk-test-virtual-key"
	if mod != nil {
		mod(&cfg)
	}
	clk := &clock{t: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	return New(cfg, append([]Option{WithClock(clk.now)}, opts...)...), clk
}

// lateRec collects late captions.
type lateRec struct {
	mu   sync.Mutex
	caps []string
}

func (l *lateRec) fn(c string) { l.mu.Lock(); l.caps = append(l.caps, c); l.mu.Unlock() }
func (l *lateRec) got() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.caps...)
}

func counted(k *Captioner, src string) int {
	return int(testutil.ToFloat64(k.metrics.total.WithLabelValues(src)))
}

func skippedN(k *Captioner, why string) int {
	return int(testutil.ToFloat64(k.metrics.skipped.WithLabelValues(why)))
}

func TestOffByDefault(t *testing.T) {
	env := map[string]string{"LITELLM_URL": "http://litellm:4000", "FARM_CAPTION_LITELLM_KEY": "sk-x"}
	cfg := ConfigFromEnv(func(k string) string { return env[k] })
	if cfg.Enabled {
		t.Fatal("FARM_CAPTION_HAIKU unset must leave Haiku off")
	}
	if New(cfg).Enabled() {
		t.Fatal("Enabled with the kill switch off")
	}
	env["FARM_CAPTION_HAIKU"] = "on"
	if !New(ConfigFromEnv(func(k string) string { return env[k] })).Enabled() {
		t.Fatal("FARM_CAPTION_HAIKU=on with a URL and a key must enable Haiku")
	}
	delete(env, "FARM_CAPTION_LITELLM_KEY")
	if New(ConfigFromEnv(func(k string) string { return env[k] })).Enabled() {
		t.Fatal("no key: Haiku must be off")
	}

	f := &fakeHaiku{reply: "Never sent"}
	k, _ := newTest(t, f, func(c *Config) { c.Enabled = false })
	if c, src, pending := k.Caption("ls -la", nil); c != "" || src != SrcNone || pending {
		t.Fatalf("off: Caption = %q %q %v", c, src, pending)
	}
	if f.requests() != 0 || skippedN(k, SkipOff) != 1 || counted(k, SrcNone) != 1 {
		t.Fatalf("off: requests=%d skipped(off)=%d none=%d", f.requests(), skippedN(k, SkipOff), counted(k, SrcNone))
	}
}

func TestConfigFromEnvBounds(t *testing.T) {
	env := map[string]string{
		"FARM_CAPTION_HAIKU_RATE_PER_MINUTE": "10", "FARM_CAPTION_HAIKU_CONCURRENCY": "2",
		"FARM_CAPTION_HAIKU_TIMEOUT": "1500ms", "FARM_CAPTION_HAIKU_DAILY_MAX": "nope",
		"AGENT_DEFAULT_HAIKU_MODEL": "anthropic/claude-haiku-4-5-custom", "LITELLM_URL": "http://litellm:4000/",
	}
	c := ConfigFromEnv(func(k string) string { return env[k] })
	if c.RatePerMinute != 10 || c.Concurrency != 2 || c.Timeout != 1500*time.Millisecond || c.DailyMax != 5000 ||
		c.CacheSize != 4096 || c.CacheTTL != 24*time.Hour || c.MaxTokens != 32 ||
		c.Model != "anthropic/claude-haiku-4-5-custom" || c.BaseURL != "http://litellm:4000" {
		t.Fatalf("ConfigFromEnv = %+v", c)
	}
	if d := DefaultConfig(); d.Model != "anthropic/claude-haiku-4-5" || d.RatePerMinute != 30 || d.Concurrency != 4 ||
		d.Timeout != 3*time.Second || d.Enabled {
		t.Fatalf("DefaultConfig = %+v", d)
	}
}

func TestLateCaptionThenCache(t *testing.T) {
	f := &fakeHaiku{reply: "Show recent commits"}
	k, _ := newTest(t, f, nil)
	var late lateRec
	c, src, pending := k.Caption("cd /home/agent/bot/gardenio/work && git log --oneline -3", late.fn)
	if c != "" || src != "" || !pending {
		t.Fatalf("first Caption = %q %q %v, want a pending call and no caption yet", c, src, pending)
	}
	k.calls.Wait()
	if got := late.got(); len(got) != 1 || got[0] != "Show recent commits" {
		t.Fatalf("late = %q", got)
	}
	// The same command after the cd is stripped is the same key: free.
	c, src, pending = k.Caption("git log --oneline -3", late.fn)
	if c != "Show recent commits" || src != SrcCache || pending {
		t.Fatalf("second Caption = %q %q %v, want the cached caption", c, src, pending)
	}
	if f.requests() != 1 || counted(k, SrcHaiku) != 1 || counted(k, SrcCache) != 1 {
		t.Fatalf("requests=%d haiku=%d cache=%d", f.requests(), counted(k, SrcHaiku), counted(k, SrcCache))
	}
	if n := testutil.CollectAndCount(k.metrics, "gardenio_farm_caption_haiku_seconds"); n != 1 {
		t.Fatalf("latency histogram series = %d", n)
	}
}

// TestPromptSendsOnlyTheScrubbedCommand: Haiku is sent the scrubbed command and the fixed prompt, nothing else of
// the tool_input, the cwd, the session or the agent.
func TestPromptSendsOnlyTheScrubbedCommand(t *testing.T) {
	f := &fakeHaiku{reply: "Call the API"}
	k, _ := newTest(t, f, nil)
	toolInput := json.RawMessage(`{"command":"cd /home/agent/bot/gardenio/work && S=/home/agent/.claude-tmp/claude-1000/abc/scratchpad; ` +
		`curl -s -H \"Authorization: Bearer xoxb-1234567890-secret\" https://api.example.com/v1/users/me?email=a@b.co > $S/out.json",` +
		`"description":"","timeout":120000,"run_in_background":false,"cwd":"/home/agent/private-cwd",` +
		`"session_id":"sess-6b1f0c2e","agent":"gardenio-planted-agent","api_key":"sk-ant-planted0123456789"}`)
	k.Caption(CommandOf(toolInput), nil)
	k.calls.Wait()
	if f.requests() != 1 {
		t.Fatalf("requests = %d", f.requests())
	}
	body := f.bodies[0]
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for key := range req {
		keys = append(keys, key)
	}
	if len(req) != 5 || req["model"] == nil || req["max_tokens"] == nil || req["temperature"] == nil ||
		req["system"] == nil || req["messages"] == nil {
		t.Fatalf("request keys = %v, want exactly model, max_tokens, temperature, system, messages", keys)
	}
	var msgs []message
	_ = json.Unmarshal(req["messages"], &msgs)
	want := `curl -s -H "Authorization: Bearer [REDACTED] api.example.com > $S/out.json`
	if len(msgs) != 1 || msgs[0].Role != "user" || msgs[0].Content != want {
		t.Fatalf("messages = %+v\nwant one user message %q", msgs, want)
	}
	var system string
	_ = json.Unmarshal(req["system"], &system)
	if system != SystemPrompt {
		t.Fatalf("system = %q", system)
	}
	for _, planted := range []string{"xoxb-", "private-cwd", "sess-6b1f0c2e", "planted", "/home/agent", "scratchpad",
		"a@b.co", "/v1/users", "120000", "run_in_background", "description"} {
		if strings.Contains(string(body), planted) {
			t.Errorf("request body carries %q:\n%s", planted, body)
		}
	}
	h := f.headers[0]
	if h.Get("Authorization") != "Bearer sk-test-virtual-key" || h.Get("x-litellm-tags") != LiteLLMTags ||
		h.Get("anthropic-version") == "" {
		t.Fatalf("headers = %v", h)
	}
}

func TestReplyPassesTheScrubber(t *testing.T) {
	for _, c := range []struct{ reply, want string }{
		{"Read /home/agent/bot/gardenio/work/gardenio/controller/internal/gardenhub/web/birds.js", "Read web/birds.js"},
		{`"Fetch https://example.com/x"`, "Fetch example.com"},
		{"Two\nlines", ""},
		{"   ", ""},
		{strings.Repeat("word ", 20), ""}, // 100 runes: dropped, not cut
	} {
		f := &fakeHaiku{reply: c.reply}
		k, _ := newTest(t, f, nil)
		var late lateRec
		k.Caption("make x", late.fn)
		k.calls.Wait()
		got := late.got()
		switch {
		case c.want == "" && len(got) != 0:
			t.Errorf("reply %q: late = %q, want none", c.reply, got)
		case c.want != "" && (len(got) != 1 || got[0] != c.want):
			t.Errorf("reply %q: late = %q, want %q", c.reply, got, c.want)
		}
		if c.want == "" {
			// A dropped reply is cached as nothing: the same command is not asked again.
			if cap, src, pending := k.Caption("make x", late.fn); cap != "" || src != SrcNone || pending || f.requests() != 1 {
				t.Errorf("reply %q: again = %q %q %v, requests %d", c.reply, cap, src, pending, f.requests())
			}
			if counted(k, SrcNone) != 2 {
				t.Errorf("reply %q: none = %d, want 2", c.reply, counted(k, SrcNone))
			}
		}
	}
}

func TestWithScrub(t *testing.T) {
	f := &fakeHaiku{reply: "Show gd-AbCdEfGhIj"}
	k, _ := newTest(t, f, nil, WithScrub(func(s string) string { return strings.ReplaceAll(s, "gd-AbCdEfGhIj", "a bead") }))
	var late lateRec
	k.Caption("kd show gd-AbCdEfGhIj", late.fn)
	k.calls.Wait()
	if got := late.got(); len(got) != 1 || got[0] != "Show a bead" {
		t.Fatalf("late = %q", got)
	}
}

func TestRateCap(t *testing.T) {
	f := &fakeHaiku{reply: "Do it"}
	k, clk := newTest(t, f, func(c *Config) { c.RatePerMinute = 2 })
	for i, cmd := range []string{"a1", "a2", "a3"} {
		_, src, pending := k.Caption(cmd, nil)
		k.calls.Wait()
		if want := i < 2; pending != want {
			t.Fatalf("call %d: pending = %v (src %q)", i, pending, src)
		}
	}
	if skippedN(k, SkipRate) != 1 {
		t.Fatalf("skipped(rate) = %d", skippedN(k, SkipRate))
	}
	clk.add(30 * time.Second) // a token a 30 s at 2 a minute
	if _, _, pending := k.Caption("a4", nil); !pending {
		t.Fatal("a token should have come back")
	}
	k.calls.Wait()
	if f.requests() != 3 {
		t.Fatalf("requests = %d", f.requests())
	}
}

func TestConcurrencyCapAndJoin(t *testing.T) {
	hold := make(chan struct{})
	f := &fakeHaiku{reply: "Run the build", hold: hold}
	k, _ := newTest(t, f, func(c *Config) { c.Concurrency = 1 })
	var a, b, c lateRec
	if _, _, pending := k.Caption("make build", a.fn); !pending {
		t.Fatal("first call should go out")
	}
	// The same command joins the call out; another one is over the cap and never queued.
	if _, _, pending := k.Caption("make build", b.fn); !pending {
		t.Fatal("the same command should wait on the call out")
	}
	if cap, src, pending := k.Caption("make test", c.fn); cap != "" || src != SrcNone || pending {
		t.Fatalf("over the cap: %q %q %v", cap, src, pending)
	}
	close(hold)
	k.calls.Wait()
	if f.requests() != 1 || skippedN(k, SkipConcurrency) != 1 {
		t.Fatalf("requests=%d skipped(concurrency)=%d", f.requests(), skippedN(k, SkipConcurrency))
	}
	if len(a.got()) != 1 || len(b.got()) != 1 || len(c.got()) != 0 {
		t.Fatalf("late: a=%q b=%q c=%q", a.got(), b.got(), c.got())
	}
	if counted(k, SrcHaiku) != 1 || counted(k, SrcCache) != 1 || counted(k, SrcNone) != 1 {
		t.Fatalf("haiku=%d cache=%d none=%d", counted(k, SrcHaiku), counted(k, SrcCache), counted(k, SrcNone))
	}
}

func TestDailyCeiling(t *testing.T) {
	f := &fakeHaiku{reply: "Do it"}
	k, clk := newTest(t, f, func(c *Config) { c.DailyMax = 2 })
	for _, cmd := range []string{"d1", "d2", "d3"} {
		k.Caption(cmd, nil)
		k.calls.Wait()
	}
	if f.requests() != 2 || skippedN(k, SkipDaily) != 1 {
		t.Fatalf("requests=%d skipped(daily)=%d", f.requests(), skippedN(k, SkipDaily))
	}
	clk.add(12 * time.Hour) // the next UTC day
	if _, _, pending := k.Caption("d4", nil); !pending {
		t.Fatal("a new UTC day should reset the ceiling")
	}
	k.calls.Wait()
}

func TestTimeoutAndError(t *testing.T) {
	f := &fakeHaiku{reply: "Too late", delay: time.Second}
	k, _ := newTest(t, f, func(c *Config) { c.Timeout = 50 * time.Millisecond })
	var late lateRec
	start := time.Now()
	if _, _, pending := k.Caption("sleep 10", late.fn); !pending {
		t.Fatal("call should go out")
	}
	if time.Since(start) > 20*time.Millisecond {
		t.Fatal("Caption waited on the model")
	}
	k.calls.Wait()
	if len(late.got()) != 0 || skippedN(k, SkipTimeout) != 1 || counted(k, SrcNone) != 1 {
		t.Fatalf("late=%q skipped(timeout)=%d none=%d", late.got(), skippedN(k, SkipTimeout), counted(k, SrcNone))
	}
	// A timeout is not cached: the next one asks again.
	if _, _, pending := k.Caption("sleep 10", late.fn); !pending {
		t.Fatal("a timed-out command should be asked again")
	}
	k.calls.Wait()

	e := &fakeHaiku{status: http.StatusInternalServerError}
	k2, _ := newTest(t, e, nil)
	k2.Caption("ls", late.fn)
	k2.calls.Wait()
	if skippedN(k2, SkipError) != 1 || len(late.got()) != 0 {
		t.Fatalf("skipped(error)=%d late=%q", skippedN(k2, SkipError), late.got())
	}
}

// A run of failures is logged once (keys redacted) and, from breakFailures on, pauses the calls for breakPause: a
// Caption in the pause is skipped as paused and reaches the server not at all; after it one call goes, and a failure
// pauses again while an answer clears the run (gd-k0v6m9C43L).
func TestFailuresLogAndPause(t *testing.T) {
	f := &fakeHaiku{status: http.StatusUnauthorized}
	var logs bytes.Buffer
	k, clk := newTest(t, f, nil, WithLogger(slog.New(slog.NewTextHandler(&logs, nil))))
	cmd := func(i int) string { return "ls " + strconv.Itoa(i) }
	for i := 0; i < breakFailures; i++ {
		if _, _, pending := k.Caption(cmd(i), nil); !pending {
			t.Fatalf("call %d should go out", i)
		}
		k.calls.Wait()
	}
	if n := f.requests(); n != breakFailures {
		t.Fatalf("%d requests, want %d", n, breakFailures)
	}
	if _, src, pending := k.Caption("ls paused", nil); pending || src != SrcNone || skippedN(k, SkipPaused) != 1 || f.requests() != breakFailures {
		t.Fatalf("paused: pending=%v src=%q skipped(paused)=%d requests=%d", pending, src, skippedN(k, SkipPaused), f.requests())
	}
	if got := strings.Count(logs.String(), "Haiku call failed"); got != 1 || !strings.Contains(logs.String(), "401") || strings.Contains(logs.String(), "sk-test") {
		t.Fatalf("one failure logged, with its status and no key: %d\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "Haiku paused") {
		t.Fatalf("the pause is logged:\n%s", logs.String())
	}
	// After the pause one call goes; a failure pauses again.
	clk.add(breakPause + time.Second)
	if _, _, pending := k.Caption("ls again", nil); !pending {
		t.Fatal("the call after the pause should go out")
	}
	k.calls.Wait()
	if _, _, pending := k.Caption("ls again 2", nil); pending || skippedN(k, SkipPaused) != 2 {
		t.Fatalf("paused again: pending=%v skipped(paused)=%d", pending, skippedN(k, SkipPaused))
	}
	// An answer clears the run: the next failures start a new one, logged afresh.
	clk.add(breakPause + time.Second)
	f.mu.Lock()
	f.status = 0
	f.reply = "List the files"
	f.mu.Unlock()
	k.Caption("ls ok", nil)
	k.calls.Wait()
	f.mu.Lock()
	f.status = http.StatusBadGateway
	f.mu.Unlock()
	k.Caption("ls fail", nil)
	k.calls.Wait()
	if _, _, pending := k.Caption("ls fail 2", nil); !pending || strings.Count(logs.String(), "Haiku call failed") != 2 {
		t.Fatalf("a new run after an answer: pending=%v\n%s", pending, logs.String())
	}
	k.calls.Wait()
}

func TestCacheTTLAndSize(t *testing.T) {
	f := &fakeHaiku{reply: "List files"}
	k, clk := newTest(t, f, func(c *Config) { c.CacheSize = 2 })
	for _, cmd := range []string{"ls a", "ls b", "ls c"} {
		k.Caption(cmd, nil)
		k.calls.Wait()
	}
	if n := k.cache.len(); n != 2 {
		t.Fatalf("cache holds %d, want its size 2", n)
	}
	if _, src, _ := k.Caption("ls c", nil); src != SrcCache {
		t.Fatalf("newest entry: src %q", src)
	}
	if _, _, pending := k.Caption("ls a", nil); !pending {
		t.Fatal("the oldest entry should have been evicted")
	}
	k.calls.Wait()
	clk.add(24 * time.Hour)
	if _, _, pending := k.Caption("ls c", nil); !pending {
		t.Fatal("an entry 24 h old should have expired")
	}
	k.calls.Wait()
}

func TestEmptyCommand(t *testing.T) {
	f := &fakeHaiku{reply: "x"}
	k, _ := newTest(t, f, nil)
	if _, src, pending := k.Caption("cd /tmp && ", nil); src != SrcNone || pending || f.requests() != 0 {
		t.Fatalf("src %q pending %v requests %d", src, pending, f.requests())
	}
}

func TestCountBounded(t *testing.T) {
	k := New(DefaultConfig())
	k.Count(SrcDescription)
	k.Count("made-up")
	if counted(k, SrcDescription) != 1 || counted(k, SrcNone) != 1 {
		t.Fatalf("description=%d none=%d", counted(k, SrcDescription), counted(k, SrcNone))
	}
	if n := testutil.CollectAndCount(k.metrics, "gardenio_farm_caption_total"); n != len(sources) {
		t.Fatalf("caption_total series = %d, want %d (every src, bounded)", n, len(sources))
	}
}

// TestTokensCountTheSpend: each answered call adds its reply's usage, so the dashboard can price the captions; a call
// that never answered adds nothing.
func TestTokensCountTheSpend(t *testing.T) {
	f := &fakeHaiku{reply: "Show recent commits"}
	k, _ := newTest(t, f, nil)
	k.Caption("git log --oneline -3", nil)
	k.Caption("git status --short", nil)
	k.calls.Wait()
	tokens := func(typ string) int { return int(testutil.ToFloat64(k.metrics.tokenN.WithLabelValues(typ))) }
	if tokens("input") != 580 || tokens("output") != 12 {
		t.Fatalf("tokens input=%d output=%d, want 580 and 12", tokens("input"), tokens("output"))
	}
	f.mu.Lock()
	f.status = http.StatusBadGateway
	f.mu.Unlock()
	k.Caption("git diff --stat", nil)
	k.calls.Wait()
	if tokens("input") != 580 {
		t.Fatalf("an upstream error added tokens: input=%d", tokens("input"))
	}
	if n := testutil.CollectAndCount(k.metrics, "gardenio_farm_caption_haiku_tokens_total"); n != 2 {
		t.Fatalf("tokens series = %d, want input and output", n)
	}
}
