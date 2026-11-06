// Package captioner writes a bird's caption with Haiku for a Bash call that has no description (gd-TBAYacR31x, step
// 3 of plan gd-jMl4oF2cJO; spec gardenio/docs/superpowers/specs/2026-10-28-bird-captions-design.md §8). It is fenced
// in on every side: an LRU keyed by the scrubbed command, a farm-wide rate cap, a concurrency cap, a short timeout,
// a daily ceiling, and a kill switch (FARM_CAPTION_HAIKU, off by default). Caption never waits on the model: it
// answers at once from the cache or not at all, and a caption Haiku writes later is handed to a callback, so the
// hub sends the call's start with its deterministic caption and the Haiku one as a `doing` update when it lands.
//
// Haiku is sent only the scrubbed command (PrepareCommand), never any other tool_input field, the cwd, the session,
// the agent or its task. Its reply is untrusted and passes the same scrubber.
package captioner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/time/rate"
)

// SystemPrompt is the fixed instruction (spec §8). The command is the only user content.
const SystemPrompt = "You write the caption for a shell command an AI coding agent is running, for a status display. " +
	"Reply with only the caption: an imperative phrase of at most eight words, no quotes, no trailing period, " +
	"no file paths, no secrets."

// LiteLLMTags attributes the spend (x-litellm-tags).
const LiteLLMTags = "component:beads-viewer,feature:bird-captions"

// A run of failures pauses the calls (gd-k0v6m9C43L: every call failed for days, counted and never logged, and each
// failure put the shared LiteLLM deployment into cooldown). breakFailures failed calls in a row (an error or a
// timeout, with no answer between) pause Haiku for breakPause: a Caption in the pause is skipped as "paused" and makes
// no call. The first failure after an answer and every logEvery after it are logged with the error, keys redacted.
const (
	breakFailures = 10
	breakPause    = 5 * time.Minute
	logEvery      = 50
)

// keyRe is a LiteLLM or Anthropic key as an error body might echo it.
var keyRe = regexp.MustCompile(`sk-[A-Za-z0-9_-]{8,}`)

// Config is the captioner's bounds and credentials (spec §8, Helm farm.captions.haiku.*).
type Config struct {
	Enabled       bool          // FARM_CAPTION_HAIKU=on; off (the default) is the kill switch
	BaseURL       string        // LITELLM_URL: LiteLLM's Anthropic-compatible base, /v1/messages is appended
	APIKey        string        // FARM_CAPTION_LITELLM_KEY: the LiteLLM virtual key; without it Haiku is off
	Model         string        // AGENT_DEFAULT_HAIKU_MODEL
	MaxTokens     int           // 32
	RatePerMinute int           // FARM_CAPTION_HAIKU_RATE_PER_MINUTE, 30
	Concurrency   int           // FARM_CAPTION_HAIKU_CONCURRENCY, 4
	Timeout       time.Duration // FARM_CAPTION_HAIKU_TIMEOUT, 3s, no retry
	DailyMax      int           // FARM_CAPTION_HAIKU_DAILY_MAX, 5000 calls a UTC day
	CacheSize     int           // FARM_CAPTION_HAIKU_CACHE_SIZE, 4096
	CacheTTL      time.Duration // 24h
}

// DefaultConfig is the spec's bounds, with Haiku off.
func DefaultConfig() Config {
	return Config{
		Model: "anthropic/claude-haiku-4-5", MaxTokens: 32, RatePerMinute: 30, Concurrency: 4,
		Timeout: 3 * time.Second, DailyMax: 5000, CacheSize: 4096, CacheTTL: 24 * time.Hour,
	}
}

// ConfigFromEnv is DefaultConfig overridden by the environment. A value that does not parse keeps the default.
func ConfigFromEnv(getenv func(string) string) Config {
	c := DefaultConfig()
	switch strings.ToLower(strings.TrimSpace(getenv("FARM_CAPTION_HAIKU"))) {
	case "on", "true", "1", "yes":
		c.Enabled = true
	}
	c.BaseURL = strings.TrimRight(strings.TrimSpace(getenv("LITELLM_URL")), "/")
	c.APIKey = strings.TrimSpace(getenv("FARM_CAPTION_LITELLM_KEY"))
	if m := strings.TrimSpace(getenv("AGENT_DEFAULT_HAIKU_MODEL")); m != "" {
		c.Model = m
	}
	envInt := func(name string, dst *int) {
		if n, err := strconv.Atoi(strings.TrimSpace(getenv(name))); err == nil && n > 0 {
			*dst = n
		}
	}
	envInt("FARM_CAPTION_HAIKU_RATE_PER_MINUTE", &c.RatePerMinute)
	envInt("FARM_CAPTION_HAIKU_CONCURRENCY", &c.Concurrency)
	envInt("FARM_CAPTION_HAIKU_DAILY_MAX", &c.DailyMax)
	envInt("FARM_CAPTION_HAIKU_CACHE_SIZE", &c.CacheSize)
	if d, err := time.ParseDuration(strings.TrimSpace(getenv("FARM_CAPTION_HAIKU_TIMEOUT"))); err == nil && d > 0 {
		c.Timeout = d
	}
	return c
}

// Captioner asks Haiku for captions within its bounds. Safe for concurrent use.
type Captioner struct {
	cfg     Config
	on      bool
	client  *http.Client
	now     func() time.Time
	scrub   func(string) string
	metrics *Metrics
	sem     chan struct{}
	logger  *slog.Logger

	mu          sync.Mutex
	failures    int       // failed calls since the last answer (the breaker)
	pausedUntil time.Time // Haiku is not called before this (set after breakFailures)
	cache       *lru
	limiter     *rate.Limiter
	day         string // the UTC day dayCalls counts
	dayCalls    int
	inflight    map[string][]func(string) // by cache key: the callbacks waiting on one call
	calls       sync.WaitGroup            // calls out (a test waits on them)
}

// Option configures a Captioner.
type Option func(*Captioner)

// WithHTTPClient sets the client (a test's fake server).
func WithHTTPClient(c *http.Client) Option { return func(k *Captioner) { k.client = c } }

// WithLogger sets where a failed call is logged (sampled: the first of a run and every logEvery). Default slog.Default.
func WithLogger(l *slog.Logger) Option { return func(k *Captioner) { k.logger = l } }

// WithScrub sets the scrubber Haiku's reply passes (the hub's scrubCaption); the default is Scrub.
func WithScrub(f func(string) string) Option { return func(k *Captioner) { k.scrub = f } }

// New builds a Captioner. Haiku is on only when cfg.Enabled and both a base URL and a key are set; otherwise every
// Caption answers from nothing (src none, skipped off), which is today's behaviour.
func New(cfg Config, opts ...Option) *Captioner {
	d := DefaultConfig()
	if cfg.MaxTokens <= 0 {
		cfg.MaxTokens = d.MaxTokens
	}
	if cfg.RatePerMinute <= 0 {
		cfg.RatePerMinute = d.RatePerMinute
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = d.Concurrency
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = d.Timeout
	}
	if cfg.DailyMax <= 0 {
		cfg.DailyMax = d.DailyMax
	}
	if cfg.CacheSize <= 0 {
		cfg.CacheSize = d.CacheSize
	}
	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = d.CacheTTL
	}
	if cfg.Model == "" {
		cfg.Model = d.Model
	}
	k := &Captioner{
		cfg:      cfg,
		on:       cfg.Enabled && cfg.BaseURL != "" && cfg.APIKey != "",
		client:   &http.Client{},
		now:      time.Now,
		scrub:    Scrub,
		logger:   slog.Default(),
		metrics:  newMetrics(),
		sem:      make(chan struct{}, cfg.Concurrency),
		cache:    newLRU(cfg.CacheSize, cfg.CacheTTL),
		limiter:  rate.NewLimiter(rate.Every(time.Minute/time.Duration(cfg.RatePerMinute)), cfg.RatePerMinute),
		inflight: map[string][]func(string){},
	}
	for _, o := range opts {
		o(k)
	}
	return k
}

// Enabled reports whether Haiku may be called at all.
func (k *Captioner) Enabled() bool { return k != nil && k.on }

// Metrics is the collector to register.
func (k *Captioner) Metrics() *Metrics { return k.metrics }

// Count counts a caption the hub made itself (a deterministic source).
func (k *Captioner) Count(src string) { k.metrics.count(src) }

// Uncaptioned counts a call start the hub made no caption for, by tool: which tools leave the gap (gd-Ty2eNoZipX). The
// hub bounds tool to the names it knows.
func (k *Captioner) Uncaptioned(tool string) { k.metrics.uncaptioned(tool) }

// Key is the cache key of a prepared command: its SHA-256.
func Key(prepared string) string {
	sum := sha256.Sum256([]byte(prepared))
	return hex.EncodeToString(sum[:])
}

// Caption is the caption of a Bash command with no description, without waiting on the model. A cached caption comes
// back at once (src cache). Otherwise, if every bound allows a call, one is started and Caption returns ("", "",
// true): late is called on another goroutine with the caption if Haiku writes a usable one, and is not called
// otherwise. Anything else returns ("", none, false). The Captioner counts the outcome of every Caption once, in
// gardenio_farm_caption_total.
func (k *Captioner) Caption(command string, late func(caption string)) (caption, src string, pending bool) {
	prepared := PrepareCommand(command)
	if prepared == "" {
		k.metrics.count(SrcNone)
		return "", SrcNone, false
	}
	key := Key(prepared)
	now := k.now()

	k.mu.Lock()
	if c, ok := k.cache.get(key, now); ok {
		k.mu.Unlock()
		if c == "" {
			k.metrics.count(SrcNone) // a reply that was dropped: not asked again
			return "", SrcNone, false
		}
		k.metrics.count(SrcCache)
		return c, SrcCache, false
	}
	if !k.on {
		k.mu.Unlock()
		k.skipped(SkipOff)
		return "", SrcNone, false
	}
	if now.Before(k.pausedUntil) {
		k.mu.Unlock()
		k.skipped(SkipPaused)
		return "", SrcNone, false
	}
	if waiters, ok := k.inflight[key]; ok {
		k.inflight[key] = append(waiters, joined(k.metrics, late)) // the same command is out: wait on its answer
		k.mu.Unlock()
		return "", "", true
	}
	select {
	case k.sem <- struct{}{}:
	default:
		k.mu.Unlock()
		k.skipped(SkipConcurrency)
		return "", SrcNone, false
	}
	why := ""
	if day := now.UTC().Format(time.DateOnly); day != k.day {
		k.day, k.dayCalls = day, 0
	}
	switch {
	case k.dayCalls >= k.cfg.DailyMax:
		why = SkipDaily
	case !k.limiter.AllowN(now, 1):
		why = SkipRate
	}
	if why != "" {
		<-k.sem
		k.mu.Unlock()
		k.skipped(why)
		return "", SrcNone, false
	}
	k.dayCalls++
	k.inflight[key] = []func(string){first(k.metrics, late)}
	k.calls.Add(1)
	k.mu.Unlock()

	go k.ask(key, prepared)
	return "", "", true
}

// first is the callback of the Caption that made the call: it counts haiku or none.
func first(m *Metrics, late func(string)) func(string) {
	return func(c string) {
		if c == "" {
			m.count(SrcNone)
			return
		}
		m.count(SrcHaiku)
		if late != nil {
			late(c)
		}
	}
}

// joined is the callback of a Caption that waited on another's call: an answer it did not pay for counts as cache.
func joined(m *Metrics, late func(string)) func(string) {
	return func(c string) {
		if c == "" {
			m.count(SrcNone)
			return
		}
		m.count(SrcCache)
		if late != nil {
			late(c)
		}
	}
}

func (k *Captioner) skipped(why string) {
	k.metrics.skip(why)
	k.metrics.count(SrcNone)
}

// ask makes the one call for key and hands its caption to every waiter.
func (k *Captioner) ask(key, prepared string) {
	defer k.calls.Done()
	defer func() { <-k.sem }()
	ctx, cancel := context.WithTimeout(context.Background(), k.cfg.Timeout)
	start := time.Now()
	reply, usage, err := k.call(ctx, prepared)
	k.metrics.seconds.Observe(time.Since(start).Seconds())
	k.metrics.tokens(usage)
	cancel()

	caption, cache := "", false
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		k.metrics.skip(SkipTimeout)
	case err != nil:
		k.metrics.skip(SkipError)
	default:
		caption, cache = k.usable(reply), true
	}

	k.mu.Lock()
	if cache {
		k.cache.put(key, caption, k.now())
		k.failures = 0
	} else {
		k.failedLocked(err)
	}
	waiters := k.inflight[key]
	delete(k.inflight, key)
	k.mu.Unlock()
	for _, w := range waiters {
		w(caption)
	}
}

// failedLocked counts a call that got no answer. The first of a run and every logEvery after it are logged, and from
// breakFailures on the calls pause for breakPause (again on each failure after a pause, so a dead deployment is asked
// once per pause until it answers).
func (k *Captioner) failedLocked(err error) {
	k.failures++
	msg := "no reply"
	if err != nil {
		msg = keyRe.ReplaceAllString(err.Error(), "sk-…")
	}
	if k.failures == 1 || k.failures%logEvery == 0 {
		k.logger.Warn("farm captions: Haiku call failed", "err", msg, "failures", k.failures, "model", k.cfg.Model)
	}
	if k.failures >= breakFailures {
		k.pausedUntil = k.now().Add(breakPause)
		k.logger.Warn("farm captions: Haiku paused after a run of failures", "failures", k.failures, "pause", breakPause.String())
	}
}

// usable is Haiku's reply as a caption, or "" when it must be dropped: empty, more than one line, or over
// MaxCaptionRunes after scrubbing.
func (k *Captioner) usable(reply string) string {
	reply = strings.TrimSpace(reply)
	if reply == "" || strings.ContainsAny(reply, "\n\r") {
		return ""
	}
	c := k.scrub(reply)
	if c == "" || utf8.RuneCountInString(c) > MaxCaptionRunes {
		return ""
	}
	return c
}

// The Messages API, the subset sent and read (internal/namegen's shape).
type messagesReq struct {
	Model       string    `json:"model"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature"`
	System      string    `json:"system"`
	Messages    []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResp struct {
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage Usage `json:"usage"`
}

// Usage is the tokens a reply says it cost: the spend gardenio_farm_caption_haiku_tokens_total adds up.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// RequestBody is the exact body sent for a prepared command.
func (k *Captioner) RequestBody(prepared string) ([]byte, error) {
	return json.Marshal(messagesReq{
		Model: k.cfg.Model, MaxTokens: k.cfg.MaxTokens, Temperature: 0, System: SystemPrompt,
		Messages: []message{{Role: "user", Content: prepared}},
	})
}

// call asks for one caption. Its usage is what a 200 reply says it cost, even when the reply has no text.
func (k *Captioner) call(ctx context.Context, prepared string) (string, Usage, error) {
	body, err := k.RequestBody(prepared)
	if err != nil {
		return "", Usage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, k.cfg.BaseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", Usage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+k.cfg.APIKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("x-litellm-tags", LiteLLMTags)
	resp, err := k.client.Do(req)
	if err != nil {
		return "", Usage{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return "", Usage{}, fmt.Errorf("captioner: %d: %s", resp.StatusCode, b)
	}
	var r messagesResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&r); err != nil {
		return "", Usage{}, err
	}
	if len(r.Content) == 0 || r.Content[0].Type != "text" {
		return "", r.Usage, errors.New("captioner: no text in the reply")
	}
	return r.Content[0].Text, r.Usage, nil
}
