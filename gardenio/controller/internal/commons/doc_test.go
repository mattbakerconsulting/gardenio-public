package commons

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/commons/commonstest"
)

func TestDocMarkdownKeptByMessage(t *testing.T) {
	text := "## Shipped\n\n| Budget | Before | Now |\n|---|---|---|\n| cold | 470 KB | 138 KB |\n\nWaits on cl-AbCdEfGhIj"
	names := beadNames{"cl-AbCdEfGhIj": "Release the chart"}
	first := docMarkdown("m1/done", "cm-x", text, "/base", names)
	for _, want := range []string{`<div class="cm-x md">`, "<h5>Shipped</h5>", "<table>", "<td>138 KB</td>", "Release the chart"} {
		if !strings.Contains(string(first), want) {
			t.Fatalf("missing %q in\n%s", want, first)
		}
	}
	// Kept: the same text renders to the same entry, without goldmark.
	docMemo.Lock()
	e := docMemo.m["m1/done"]
	e.html += "<!--kept-->"
	docMemo.m["m1/done"] = e
	docMemo.Unlock()
	if got := docMarkdown("m1/done", "cm-x", text, "/base", names); !strings.HasSuffix(string(got), "<!--kept-->") {
		t.Fatalf("rendered again:\n%s", got)
	}
	// A viewer who reads the bead under another title, or not at all, never gets the first viewer's.
	for _, other := range []beadNames{{"cl-AbCdEfGhIj": "Another title"}, nil} {
		got := string(docMarkdown("m1/done", "cm-x", text, "/base", other))
		if strings.Contains(got, "kept") || strings.Contains(got, "Release the chart") {
			t.Fatalf("names %v got the cached render:\n%s", other, got)
		}
	}
	// Edited text, or another base, renders again.
	if got := docMarkdown("m1/done", "cm-x", text+" now", "/base", names); strings.Contains(string(got), "kept") {
		t.Fatal("edited text served from the memo")
	}
	if got := docMarkdown("m1/done", "cm-x", text, "", names); strings.Contains(string(got), `href="/base`) {
		t.Fatal("another base served from the memo")
	}
}

func TestBulletsAndDocShape(t *testing.T) {
	for in, want := range map[string]string{
		"one thing":           "one thing",
		"a; b; c":             "- a\n- b\n- c",
		"- a; b\n- c":         "- a; b\n- c",
		"**bold**; and plain": "**bold**; and plain",
	} {
		if got := bullets(in); got != want {
			t.Errorf("bullets(%q) = %q, want %q", in, got, want)
		}
	}
	for text, want := range map[string]bool{
		"Done, PR #12 is green": false, "## Step 3\nbody": true, "- one\n- two": true, "| a | b |\n|---|---|": true,
		"1. first": true, "a **strong** word": true, "2 * 3 = 6": false,
	} {
		if got := (row{M: Message{Author: "agent:x", Text: text}}).IsDoc(); got != want {
			t.Errorf("IsDoc(%q) = %v, want %v", text, got, want)
		}
	}
	if (row{M: Message{Author: "oidc:ann@example.com", Text: "## mine"}}).IsDoc() {
		t.Error("a person's message rendered as a document")
	}
	for raw, want := range map[string]wrapPR{
		"https://github.com/o/r/pull/1654":           {"https://github.com/o/r/pull/1654", "1654"},
		"https://gitlab.com/o/r/-/merge_requests/9/": {"https://gitlab.com/o/r/-/merge_requests/9/", "9"},
		"https://example.com/review":                 {"https://example.com/review", ""},
		"javascript:alert(1)":                        {},
		"github.com/o/r/pull/3":                      {},
	} {
		if got, _ := wrapPRFrom(raw); got != want {
			t.Errorf("wrapPRFrom(%q) = %+v, want %+v", raw, got, want)
		}
	}
}

func TestThreadAnswersNotModified(t *testing.T) {
	h := commonsServer(t, commonstest.TwoLands(t0))
	ask := func(tag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", "/garden/c/claimlayer/general/t/cl-wish1", nil)
		r.Header.Set("X-Test-As", "claimlayer")
		if tag != "" {
			r.Header.Set("If-None-Match", tag)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	first := ask("")
	tag := first.Header().Get("ETag")
	if first.Code != http.StatusOK || tag == "" || first.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("first open: %d %q %v", first.Code, tag, first.Header())
	}
	if again := ask(tag); again.Code != http.StatusNotModified || again.Body.Len() != 0 {
		t.Fatalf("open again: %d, %d bytes", again.Code, again.Body.Len())
	}
	if other := ask(`"stale"`); other.Code != http.StatusOK || other.Body.String() != first.Body.String() {
		t.Fatalf("a stale tag: %d", other.Code)
	}
}
