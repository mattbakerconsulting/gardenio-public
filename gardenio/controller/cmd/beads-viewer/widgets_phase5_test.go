package main

import (
	"html/template"
	"slices"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

// newTestWidgetCtxP5 mirrors newTestWidgetCtx but additionally parses the
// Phase 5 partial and wires up a code highlighter — Phase 5 widgets need
// both. Kept separate so Phase 4 tests stay focused on the plain-HTML
// surface.
func newTestWidgetCtxP5(t *testing.T) *widgetRenderCtx {
	t.Helper()
	tpl, err := template.New("").ParseFS(getTestTemplateFS(),
		"templates/partials/widgets_phase4.html",
		"templates/partials/widgets_phase5.html")
	if err != nil {
		t.Fatalf("parse phase 4+5 partials: %v", err)
	}
	return &widgetRenderCtx{
		md:        goldmark.New(goldmark.WithExtensions(extension.GFM)),
		templates: tpl,
		hl:        newCodeHighlighter(),
	}
}

func TestRegisteredWidgets_Phase5SlugsPresent(t *testing.T) {
	got := registeredWidgets()
	for _, slug := range []string{"rca-evidence-cards", "source-code-analysis"} {
		if !slices.Contains(got, slug) {
			t.Errorf("expected widget %q in registry, got %v", slug, got)
		}
	}
}

func TestRenderRCAEvidenceCards_FullCard(t *testing.T) {
	ctx := newTestWidgetCtxP5(t)
	r, ok := lookupWidget("rca-evidence-cards")
	if !ok {
		t.Fatal("rca-evidence-cards not registered")
	}
	payload := `{"cards":[{
		"id":"ev-1",
		"headline":"Auth middleware drops sessions",
		"detail":"401 spike correlates with !2185 deploy",
		"source_kind":"mezmo",
		"source_label":"mezmo: auth-svc",
		"confidence":0.92,
		"language":"go",
		"code":"func auth() {\n\tpanic(\"boom\")\n}",
		"open_mezmo":"app:auth-svc level:error",
		"drilldown_markdown":"### Detail\n\nFirst seen 11:50 UTC."
	}]}`
	html, err := r(payload, ctx)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := string(html)

	// Source-kind chip rendered with Phase 1 selectors.
	if !strings.Contains(body, `data-source-kind="mezmo"`) {
		t.Errorf("missing source-kind=mezmo chip; body=%s", body)
	}
	// Confidence band only — D2. Number must NOT leak.
	if !strings.Contains(body, `data-confidence="high"`) {
		t.Errorf("expected confidence=high pill; body=%s", body)
	}
	if strings.Contains(body, "0.92") {
		t.Errorf("raw confidence number leaked into widget; body=%s", body)
	}
	// Chroma highlight signals (matches the existing code-fence test in
	// code_highlight_test.go).
	if !strings.Contains(body, "background-color:") {
		t.Errorf("expected chroma <pre> background-color inline style; body=%s", body)
	}
	if !strings.Contains(body, `<span style="color:`) {
		t.Errorf("expected chroma token spans; body=%s", body)
	}
	// Mezmo action button.
	if !strings.Contains(body, "Open in Mezmo") {
		t.Errorf("missing open-mezmo action; body=%s", body)
	}
	// Drill-down drawer scaffolding.
	if !strings.Contains(body, "ii-w-rca-drill") {
		t.Errorf("missing drilldown button; body=%s", body)
	}
	if !strings.Contains(body, "rca-evidence-0-ev-1") {
		t.Errorf("missing scoped drawer key; body=%s", body)
	}
	if !strings.Contains(body, "<template") || !strings.Contains(body, "First seen 11:50 UTC.") {
		t.Errorf("missing pre-rendered drawer body in <template>; body=%s", body)
	}
}

func TestRenderRCAEvidenceCards_EmptyAndAltActions(t *testing.T) {
	ctx := newTestWidgetCtxP5(t)
	r, _ := lookupWidget("rca-evidence-cards")

	t.Run("empty payload renders empty state", func(t *testing.T) {
		html, err := r(`{}`, ctx)
		if err != nil {
			t.Fatalf("render error: %v", err)
		}
		if !strings.Contains(string(html), "No RCA evidence collected") {
			t.Errorf("missing empty state; got %s", html)
		}
	})

	t.Run("ide_path falls back to OpenIDEAction", func(t *testing.T) {
		payload := `{"cards":[{"id":"ev-2","headline":"file ref","ide_path":"a/b.go","ide_line":42}]}`
		html, _ := r(payload, ctx)
		if !strings.Contains(string(html), "Open in IDE") {
			t.Errorf("expected IDE deeplink button; got %s", html)
		}
	})
}

func TestRenderSourceCodeAnalysis_FullPayload(t *testing.T) {
	ctx := newTestWidgetCtxP5(t)
	r, ok := lookupWidget("source-code-analysis")
	if !ok {
		t.Fatal("source-code-analysis not registered")
	}
	payload := `{
		"title":"Top hot lines",
		"findings":[
			{"path":"controller/main.go","line":42,"function":"run","severity":"error",
			 "headline":"nil deref on missing config","detail":"Hit in 3/3 traces",
			 "language":"go","code":"if cfg.X == nil { panic(0) }"}
		]
	}`
	html, err := r(payload, ctx)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	body := string(html)
	if !strings.Contains(body, "Top hot lines") {
		t.Errorf("missing title; body=%s", body)
	}
	if !strings.Contains(body, "controller/main.go:42") {
		t.Errorf("missing path:line; body=%s", body)
	}
	if !strings.Contains(body, `data-state="error"`) {
		t.Errorf("missing severity pill; body=%s", body)
	}
	if !strings.Contains(body, "background-color:") {
		t.Errorf("expected chroma highlight; body=%s", body)
	}
	if !strings.Contains(body, "Open in IDE") {
		t.Errorf("missing jump-to-line action; body=%s", body)
	}
}

func TestRenderSourceCodeAnalysis_EmptyPayload(t *testing.T) {
	ctx := newTestWidgetCtxP5(t)
	r, _ := lookupWidget("source-code-analysis")
	html, err := r(`{}`, ctx)
	if err != nil {
		t.Fatalf("render error: %v", err)
	}
	if !strings.Contains(string(html), "No source-code findings") {
		t.Errorf("missing empty state; got %s", html)
	}
}

func TestCodeHighlighter_CacheHits(t *testing.T) {
	hl := newCodeHighlighter()
	first := hl.highlight("go", "package main")
	second := hl.highlight("go", "package main")
	if first != second {
		t.Errorf("cached render differs from original")
	}
	hits, misses, size := hl.stats()
	if hits != 1 {
		t.Errorf("expected 1 cache hit, got %d", hits)
	}
	if misses != 1 {
		t.Errorf("expected 1 miss (initial render), got %d", misses)
	}
	if size != 1 {
		t.Errorf("expected cache size 1, got %d", size)
	}
}

func TestCodeHighlighter_UnknownLanguageFallsBack(t *testing.T) {
	hl := newCodeHighlighter()
	got := hl.highlight("not-a-real-lang", "the quick brown fox")
	if got == "" {
		t.Errorf("expected non-empty fallback render for unknown language")
	}
}

func TestCodeHighlighter_EmptyCodeReturnsEmpty(t *testing.T) {
	hl := newCodeHighlighter()
	if got := hl.highlight("go", ""); got != "" {
		t.Errorf("empty code should render empty, got %q", got)
	}
}

func TestRenderHighlight_NilContextDoesNotPanic(t *testing.T) {
	// Plain-HTML fallback path: a Phase 5 widget rendered through a context
	// that lacks an hl (older Phase 4-only test setup) must still produce
	// safe HTML rather than panic.
	got := renderHighlight(nil, "go", "x := 1")
	if !strings.Contains(string(got), "x := 1") {
		t.Errorf("expected plain-text pre fallback, got %q", got)
	}
}
