package namegen

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseResponse(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantSlug string
		wantTitle string
		wantErr  bool
	}{
		{
			name:      "normal response",
			input:     "slug: fix-auth-middleware\ntitle: Fix auth middleware nil pointer dereference",
			wantSlug:  "fix-auth-middleware",
			wantTitle: "Fix auth middleware nil pointer dereference",
		},
		{
			name:      "extra whitespace",
			input:     "  slug:  fix-login-bug  \n  title:  Fix the login bug  ",
			wantSlug:  "fix-login-bug",
			wantTitle: "Fix the login bug",
		},
		{
			name:      "slug with special chars gets sanitized",
			input:     "slug: Fix_Auth.Middleware!\ntitle: Fix auth",
			wantSlug:  "fix-auth-middleware",
			wantTitle: "Fix auth",
		},
		{
			name:    "missing slug",
			input:   "title: Something",
			wantErr: true,
		},
		{
			name:      "missing title falls back to slug",
			input:     "slug: do-the-thing",
			wantSlug:  "do-the-thing",
			wantTitle: "do-the-thing",
		},
		{
			name:      "very long slug gets truncated",
			input:     "slug: this-is-a-very-long-slug-that-exceeds-the-forty-character-limit-by-quite-a-lot\ntitle: Short",
			wantSlug:  "this-is-a-very-long-slug-that-exceeds-th",
			wantTitle: "Short",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result, err := parseResponse(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Slug != tc.wantSlug {
				t.Errorf("slug = %q, want %q", result.Slug, tc.wantSlug)
			}
			if result.Title != tc.wantTitle {
				t.Errorf("title = %q, want %q", result.Title, tc.wantTitle)
			}
		})
	}
}

func TestSanitizeSlug(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"fix-auth", "fix-auth"},
		{"Fix Auth!", "fix-auth"},
		{"hello_world.test", "hello-world-test"},
		{"---leading---", "leading"},
		{"trailing---", "trailing"},
		{"a  b  c", "a-b-c"},
		{"UPPER-CASE", "upper-case"},
		{"", ""},
	}
	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			got := sanitizeSlug(tc.input)
			if got != tc.want {
				t.Errorf("sanitizeSlug(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestNew_EmptyKey(t *testing.T) {
	g := New("")
	if g != nil {
		t.Error("expected nil generator for empty API key")
	}
}

func TestGenerate_NilGenerator(t *testing.T) {
	var g *Generator
	_, err := g.Generate(context.Background(), "test")
	if err == nil {
		t.Error("expected error for nil generator")
	}
}

func TestGenerate_MockServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request headers.
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("missing or wrong api key")
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("missing anthropic-version header")
		}

		// Verify request body.
		var req messagesReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Model != "claude-haiku-4-5-20251001" {
			t.Errorf("model = %q, want haiku", req.Model)
		}

		// Return mock response.
		resp := messagesResp{
			Content: []contentBlock{
				{Type: "text", Text: "slug: fix-spawn-name-limit\ntitle: Fix spawn command 500-char name limit"},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	g := New("test-key", WithBaseURL(srv.URL))
	result, err := g.Generate(context.Background(), "When we use/spawn there is an annoying thing that happens which is you can't make a prompt that's too long because it complains about 500 characters")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Slug != "fix-spawn-name-limit" {
		t.Errorf("slug = %q, want fix-spawn-name-limit", result.Slug)
	}
	if result.Title != "Fix spawn command 500-char name limit" {
		t.Errorf("title = %q", result.Title)
	}
}

func TestGenerate_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"invalid key"}`))
	}))
	defer srv.Close()

	g := New("bad-key", WithBaseURL(srv.URL))
	_, err := g.Generate(context.Background(), "test")
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
}

func TestGenerate_TruncatesLongInput(t *testing.T) {
	var receivedLen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req messagesReq
		json.NewDecoder(r.Body).Decode(&req)
		if len(req.Messages) > 0 {
			receivedLen = len(req.Messages[0].Content)
		}
		resp := messagesResp{
			Content: []contentBlock{
				{Type: "text", Text: "slug: test-slug\ntitle: Test"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	g := New("test-key", WithBaseURL(srv.URL))
	longText := make([]byte, 5000)
	for i := range longText {
		longText[i] = 'a'
	}
	g.Generate(context.Background(), string(longText))

	// The prompt template adds overhead, but the task text should be truncated to 2000.
	// The total message includes the prompt template wrapping the truncated text.
	if receivedLen > 2500 {
		t.Errorf("expected truncated input, got content length %d", receivedLen)
	}
}
