package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TestBeadShow_DescriptionCodeFenceHighlighted verifies that fenced code
// blocks in bead descriptions render with chroma syntax highlighting (inline
// styles, no separate stylesheet) — the parity gap with iOS / GitHub / Slack
// the gd-wuxkludgz9 enrichment is meant to close.
func TestBeadShow_DescriptionCodeFenceHighlighted(t *testing.T) {
	f := newFake()
	f.beads["gd-hl-d"] = &beadsapi.BeadDetail{
		ID:          "gd-hl-d",
		Title:       "highlighted desc",
		Type:        "task",
		Status:      "open",
		Description: "Look:\n\n```go\nfunc f() {}\n```\n",
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-hl-d", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	// chroma emits <pre> with an inline background-color style, and tokens
	// inside as <span style="color:..."> spans. Either signal proves the
	// highlighter ran.
	if !strings.Contains(body, "background-color:") {
		t.Errorf("expected chroma <pre> background-color inline style, body=%s", body)
	}
	if !strings.Contains(body, `<span style="color:`) {
		t.Errorf("expected chroma token <span style=\"color:...\"> in description, body=%s", body)
	}
	// Raw fence markers must not leak through.
	if strings.Contains(body, "```go") {
		t.Errorf("raw ```go fence leaked through unrendered, body=%s", body)
	}
}

// TestBeadShow_CommentCodeFenceHighlighted asserts the same for comment
// bodies, since both render paths share the goldmark instance.
func TestBeadShow_CommentCodeFenceHighlighted(t *testing.T) {
	f := newFake()
	f.beads["gd-hl-c"] = &beadsapi.BeadDetail{
		ID:     "gd-hl-c",
		Title:  "highlighted comment",
		Type:   "task",
		Status: "open",
	}
	f.comments["gd-hl-c"] = []beadsapi.Comment{{
		Author:    "agent",
		Text:      "Patch:\n\n```go\nfunc f() {}\n```\n",
		CreatedAt: time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC),
	}}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-hl-c", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `<span style="color:`) {
		t.Errorf("expected chroma token span in comment render, body=%s", body)
	}
}

// TestBeadShow_InlineCodeUnchanged guards against the highlighter accidentally
// styling inline `code` (single backticks). Inline code should remain a bare
// <code> with no inline style attribute.
func TestBeadShow_InlineCodeUnchanged(t *testing.T) {
	f := newFake()
	f.beads["gd-hl-i"] = &beadsapi.BeadDetail{
		ID:          "gd-hl-i",
		Title:       "inline code",
		Type:        "task",
		Status:      "open",
		Description: "See `foo.go` for details.",
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-hl-i", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, "<code>foo.go</code>") {
		t.Errorf("expected unstyled inline <code>foo.go</code>, body=%s", body)
	}
}

// TestBeadShow_CodeFenceBeadIDNotLinkified guards LinkifyHTML behavior on
// highlighted code fences: a bead ID inside a Go code block must not be
// rewritten into an <a href> link. With chroma's Go lexer the identifier
// "gd-foo" tokenizes as separate spans (kd / - / foo), which prevents the
// LinkifyHTML regex from matching the bare `gd-foo` literal.
func TestBeadShow_CodeFenceBeadIDNotLinkified(t *testing.T) {
	f := newFake()
	f.beads["gd-hl-l"] = &beadsapi.BeadDetail{
		ID:          "gd-hl-l",
		Title:       "no linkify in code",
		Type:        "task",
		Status:      "open",
		Description: "ref outside: gd-other\n\n```go\ngd-foo\nfunc f() {}\n```\n",
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-hl-l", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	// The reference outside the fence must still be linkified — sanity check
	// for the linkifier. Description goes through the rich linkifier, so we
	// substring-match on href + visible text rather than the exact plain form.
	if !strings.Contains(body, `href="/gd-other"`) || !strings.Contains(body, `>gd-other</a>`) {
		t.Errorf("expected gd-other linkified outside code fence, body=%s", body)
	}
	// Inside the code fence, gd-foo must NOT be wrapped in an anchor — neither
	// the plain nor the rich form. Check the href substring (which prefixes
	// both forms) so this catches either.
	if strings.Contains(body, `href="/gd-foo"`) {
		t.Errorf("gd-foo inside code fence was linkified, body=%s", body)
	}
}

// TestBeadShow_PreservesInlineCodeKbeadID confirms that bead IDs in inline
// `code` are still safe — single-backtick code is rendered as <code> without
// chroma; if an ID slipped in there it would be linkified, which is the same
// existing behavior pre-highlighting (and matches what users expect when they
// reference an ID with surrounding markup).
func TestBeadShow_LinkifyOutsideFencesUnaffected(t *testing.T) {
	f := newFake()
	f.beads["gd-hl-o"] = &beadsapi.BeadDetail{
		ID:          "gd-hl-o",
		Title:       "linkify still works",
		Type:        "task",
		Status:      "open",
		Description: "see gd-aaa for context.\n\n```go\nfunc f() {}\n```",
	}
	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-hl-o", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	body := w.Body.String()
	if !strings.Contains(body, `href="/gd-aaa"`) || !strings.Contains(body, `>gd-aaa</a>`) {
		t.Errorf("expected gd-aaa linkified outside code fence, body=%s", body)
	}
	if !strings.Contains(body, "background-color:") {
		t.Errorf("expected highlighter ran on the fenced block, body=%s", body)
	}
}

var _ = http.StatusOK // keep imports stable when individual asserts shift
