package main

// Server-side syntax highlighter for Phase 5 widgets (#4 RCA evidence,
// #13 source-code analysis) and any future widget that surfaces code
// outside a markdown fence. Plan-of-record: synthesis.md §Phase 5.
// Bead: ka-0000004643.
//
// Why a dedicated path (we already have chroma via goldmark): widget payloads
// arrive as `{ "code": "...", "language": "go" }` — not as fenced markdown.
// Routing through goldmark would require synthesizing a fence, parsing it
// back out, and discarding the wrapping <p>/<pre> shell. Calling chroma
// directly is shorter, faster, and lets us put a cache in front of it.
//
// Cache keying: (language, sha256(code)). Bead-level keying would force a
// cache thrash whenever a single byte of the report changes; the snippet
// itself is the stable identity. github-dark / inline-styles match the
// goldmark config in server.go so embedded fences and Phase 5 cards render
// identically.

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2"
	chromahtml "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/lexers"
	"github.com/alecthomas/chroma/v2/styles"
	htmlpkg "html"
	"html/template"
)

// codeHighlighter renders code snippets as Chroma-highlighted HTML, cached on
// (language, code-hash). Safe for concurrent use.
type codeHighlighter struct {
	style     *chroma.Style
	formatter *chromahtml.Formatter

	mu    sync.RWMutex
	cache map[string]template.HTML
	// hits/misses are tracked so tests can assert the cache is doing work.
	hits   int
	misses int
}

// newCodeHighlighter wires up github-dark + inline styles (no separate
// stylesheet required). The same combo the goldmark instance in server.go
// uses, so a fenced block and a Phase 5 card render to visually-identical
// HTML.
func newCodeHighlighter() *codeHighlighter {
	style := styles.Get("github-dark")
	if style == nil {
		style = styles.Fallback
	}
	return &codeHighlighter{
		style:     style,
		formatter: chromahtml.New(chromahtml.WithClasses(false)),
		cache:     map[string]template.HTML{},
	}
}

// highlight returns Chroma-rendered HTML for code in the given language.
// Empty code returns "". Unknown languages fall back to the analyser's best
// guess; if that also fails, we drop to the plain-text lexer so the snippet
// still renders (just without colors).
//
// Errors degrade to escaped plain-text inside <pre> rather than panic — a
// broken highlight should never tank the surrounding widget render.
func (h *codeHighlighter) highlight(language, code string) template.HTML {
	if code == "" {
		return ""
	}
	key := h.cacheKey(language, code)
	h.mu.RLock()
	if got, ok := h.cache[key]; ok {
		h.mu.RUnlock()
		h.bumpHits()
		return got
	}
	h.mu.RUnlock()

	out := h.render(language, code)

	h.mu.Lock()
	h.cache[key] = out
	h.misses++
	h.mu.Unlock()
	return out
}

// render does the actual chroma call. Split out so highlight() can keep
// its locking simple.
func (h *codeHighlighter) render(language, code string) template.HTML {
	lexer := lexers.Get(strings.ToLower(strings.TrimSpace(language)))
	if lexer == nil {
		lexer = lexers.Analyse(code)
	}
	if lexer == nil {
		lexer = lexers.Fallback
	}
	lexer = chroma.Coalesce(lexer)

	iter, err := lexer.Tokenise(nil, code)
	if err != nil {
		return plainTextPre(code)
	}
	var buf strings.Builder
	if err := h.formatter.Format(&buf, h.style, iter); err != nil {
		return plainTextPre(code)
	}
	return template.HTML(buf.String())
}

func (h *codeHighlighter) cacheKey(language, code string) string {
	sum := sha256.Sum256([]byte(code))
	return strings.ToLower(strings.TrimSpace(language)) + ":" + hex.EncodeToString(sum[:])
}

func (h *codeHighlighter) bumpHits() {
	h.mu.Lock()
	h.hits++
	h.mu.Unlock()
}

// plainTextPre renders code as escaped plain text inside <pre>. Used as the
// degraded path when chroma fails for any reason.
func plainTextPre(code string) template.HTML {
	return template.HTML(`<pre>` + htmlpkg.EscapeString(code) + `</pre>`)
}
