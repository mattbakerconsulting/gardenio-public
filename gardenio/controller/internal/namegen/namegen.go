// Package namegen generates short, meaningful agent names and bead titles
// from long prompt text using Claude Haiku.
package namegen

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Generator produces agent names and bead titles from prompt text.
type Generator struct {
	apiKey  string
	baseURL string
	model   string
	client  *http.Client
}

// Result holds the generated name and title.
type Result struct {
	// Slug is a short, kebab-case agent name (e.g. "fix-auth-middleware").
	// Lowercase, hyphens only, no special chars. Max ~40 chars.
	Slug string

	// Title is a concise, human-readable summary (e.g. "Fix auth middleware nil pointer").
	// Max 120 chars.
	Title string
}

// Option configures a Generator.
type Option func(*Generator)

// WithBaseURL overrides the Anthropic API base URL.
func WithBaseURL(url string) Option {
	return func(g *Generator) { g.baseURL = url }
}

// WithModel overrides the model ID.
func WithModel(model string) Option {
	return func(g *Generator) { g.model = model }
}

// New creates a Generator. Returns nil if apiKey is empty (disabled).
func New(apiKey string, opts ...Option) *Generator {
	if apiKey == "" {
		return nil
	}
	g := &Generator{
		apiKey:  apiKey,
		baseURL: "https://api.anthropic.com",
		model:   "claude-haiku-4-5-20251001",
		client:  &http.Client{Timeout: 10 * time.Second},
	}
	for _, o := range opts {
		o(g)
	}
	return g
}

// anthropic request/response types (minimal subset).
type messagesReq struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	Messages  []message `json:"messages"`
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesResp struct {
	Content []contentBlock `json:"content"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

const prompt = `Given this task description, generate two things:
1. A short kebab-case slug (3-5 words, lowercase, hyphens, no special chars) suitable as a K8s pod name component. Max 40 chars.
2. A concise title (max 120 chars) summarizing the task.

Reply in EXACTLY this format (two lines, no extra text):
slug: <the-slug>
title: <the title>

Task description:
%s`

// Generate produces a slug and title from the given prompt text.
// Returns an error if the API call fails — callers should fall back to
// mechanical name generation.
func (g *Generator) Generate(ctx context.Context, text string) (*Result, error) {
	if g == nil {
		return nil, fmt.Errorf("namegen: generator is nil (disabled)")
	}

	// Truncate very long prompts to keep the Haiku call cheap.
	if len(text) > 2000 {
		text = text[:2000]
	}

	body, err := json.Marshal(messagesReq{
		Model:     g.model,
		MaxTokens: 100,
		Messages: []message{
			{Role: "user", Content: fmt.Sprintf(prompt, text)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("namegen: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", g.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("namegen: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", g.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")

	resp, err := g.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("namegen: API call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("namegen: API returned %d: %s", resp.StatusCode, string(respBody))
	}

	var apiResp messagesResp
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil, fmt.Errorf("namegen: decode response: %w", err)
	}

	if len(apiResp.Content) == 0 || apiResp.Content[0].Type != "text" {
		return nil, fmt.Errorf("namegen: unexpected response format")
	}

	return parseResponse(apiResp.Content[0].Text)
}

// parseResponse extracts slug and title from the model's two-line response.
func parseResponse(text string) (*Result, error) {
	var slug, title string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(line, "slug:"); ok {
			slug = strings.TrimSpace(v)
		} else if v, ok := strings.CutPrefix(line, "title:"); ok {
			title = strings.TrimSpace(v)
		}
	}

	if slug == "" {
		return nil, fmt.Errorf("namegen: no slug in response: %q", text)
	}

	// Sanitize the slug: lowercase, keep only [a-z0-9-], collapse hyphens.
	slug = sanitizeSlug(slug)
	if slug == "" {
		return nil, fmt.Errorf("namegen: empty slug after sanitization")
	}

	// Enforce max length.
	if len(slug) > 40 {
		slug = slug[:40]
		// Don't end on a hyphen.
		slug = strings.TrimRight(slug, "-")
	}

	if title == "" {
		title = slug // fallback
	}
	if len([]rune(title)) > 120 {
		title = string([]rune(title)[:120])
	}

	return &Result{Slug: slug, Title: title}, nil
}

// GenerateName is a convenience method that matches the bridge.NameGenerator
// interface. Returns (slug, title, error).
func (g *Generator) GenerateName(ctx context.Context, text string) (string, string, error) {
	r, err := g.Generate(ctx, text)
	if err != nil {
		return "", "", err
	}
	return r.Slug, r.Title, nil
}

// sanitizeSlug converts a string to a valid kebab-case slug.
func sanitizeSlug(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	lastHyphen := false
	for _, c := range s {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
			lastHyphen = false
		} else if !lastHyphen && b.Len() > 0 {
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	result := b.String()
	return strings.TrimRight(result, "-")
}
