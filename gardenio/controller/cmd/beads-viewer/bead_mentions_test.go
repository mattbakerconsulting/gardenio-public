package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

// TestBeadShow_RendersMentions verifies the bead detail page renders the
// "Mentions" and "Mentioned by" sections from the kbeads bead-references
// endpoints (epic ka-0000002334). When a daemon row has Title="" (dangling
// reference — typo, cross-fleet ID, or not-yet-created) the row still
// renders so users see the broken link.
func TestBeadShow_RendersMentions(t *testing.T) {
	f := newFake()
	f.mentions = map[string][]beadsapi.BeadReferenceSummary{
		"gd-aaa": {
			{ID: "gd-c3u76usvml", Title: "Some target bead", Type: "task", Status: "open", SourceField: "description"},
			{ID: "gd-zacqz1sgld", Title: "", Type: "", Status: "", SourceField: "notes"},
		},
	}
	f.mentionedBy = map[string][]beadsapi.BeadReferenceSummary{
		"gd-aaa": {
			{ID: "gd-qa1k0drtny", Title: "Citing bead", Type: "task", Status: "open", SourceField: "description"},
			{ID: "gd-d0hjrnsvzz", Title: "Comment-mention bead", Type: "task", Status: "open", SourceField: "comment:42"},
		},
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Section headers are present.
	for _, header := range []string{">Mentions<", ">Mentioned by<"} {
		if !strings.Contains(body, header) {
			t.Errorf("missing section header %q", header)
		}
	}

	// Forward mentions: target ID, title, source_field.
	for _, want := range []string{"gd-c3u76usvml", "Some target bead", `>description<`} {
		if !strings.Contains(body, want) {
			t.Errorf("Mentions section missing %q", want)
		}
	}

	// Dangling target still renders with an "(unknown)" placeholder so users
	// can spot typos and cross-fleet IDs.
	for _, want := range []string{"gd-zacqz1sgld", "(unknown)"} {
		if !strings.Contains(body, want) {
			t.Errorf("dangling-mention rendering missing %q", want)
		}
	}

	// Reverse mentions: per-comment provenance is preserved verbatim so
	// users can identify which comment a mention came from.
	for _, want := range []string{"gd-qa1k0drtny", "Citing bead", "gd-d0hjrnsvzz", "comment:42"} {
		if !strings.Contains(body, want) {
			t.Errorf("Mentioned-by section missing %q", want)
		}
	}
}

// TestBeadShow_NoMentions verifies the page renders without empty Mentions /
// Mentioned-by sections when there are no references — the headers should
// only appear when there is something to show.
func TestBeadShow_NoMentions(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, header := range []string{">Mentions<", ">Mentioned by<"} {
		if strings.Contains(body, header) {
			t.Errorf("unexpected section header %q on bead with no references", header)
		}
	}
}

// TestBeadShow_MentionsDaemonError verifies that errors from the
// mentioned-by / mentions daemon calls degrade gracefully — the page still
// renders 200, just without the affected section. Older daemons predate the
// bead-references epic and will return errors here.
func TestBeadShow_MentionsDaemonError(t *testing.T) {
	f := newFake()
	f.mentionedByErr = errors.New("daemon does not implement /v1/beads/{id}/mentioned-by")
	f.mentionsErr = errors.New("daemon does not implement /v1/beads/{id}/mentions")

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 even when mentions endpoints fail, got %d", w.Code)
	}
	body := w.Body.String()
	for _, header := range []string{">Mentions<", ">Mentioned by<"} {
		if strings.Contains(body, header) {
			t.Errorf("section %q rendered despite daemon error — should degrade silently", header)
		}
	}
}

// TestBeadShow_TypedMentionsRender verifies the bead detail page renders the
// typed-mention relationship_type next to each row (T7 / ka-0000005941). For
// a typed row the verb appears as a chip with an arrow ("parent → gd-X"); for
// an untyped row the existing source-field chip is preserved.
func TestBeadShow_TypedMentionsRender(t *testing.T) {
	parent := "parent-child"
	blocks := "blocks"
	f := newFake()
	f.mentions = map[string][]beadsapi.BeadReferenceSummary{
		"gd-aaa": {
			{ID: "gd-c3u76usvml", Title: "Some target", Type: "task", Status: "open", SourceField: "description", RelationshipType: &parent},
			{ID: "gd-17bscqb8mt", Title: "Plain mention", Type: "task", Status: "open", SourceField: "description"},
		},
	}
	f.mentionedBy = map[string][]beadsapi.BeadReferenceSummary{
		"gd-aaa": {
			{ID: "gd-qa1k0drtny", Title: "Blocker", Type: "task", Status: "open", SourceField: "description", RelationshipType: &blocks},
		},
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	// Typed forward mention: verb chip + arrow + ID + title + provenance suffix.
	for _, want := range []string{
		`dep-type-typed">parent-child<`,
		"&rarr;",
		"gd-c3u76usvml",
		"Some target",
		"(mentioned in description)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("typed Mentions row missing %q", want)
		}
	}

	// Untyped row keeps the historic source-field chip rendering.
	if !strings.Contains(body, "gd-17bscqb8mt") {
		t.Errorf("untyped row not rendered")
	}
	if strings.Contains(body, `dep-type-typed">description<`) {
		t.Errorf("untyped row should not be tagged as typed; body:\n%s", body)
	}

	// Typed reverse mention: verb chip + arrow on Mentioned-by row.
	for _, want := range []string{
		`dep-type-typed">blocks<`,
		"gd-qa1k0drtny",
		"Blocker",
		"(mentioned in description)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("typed Mentioned-by row missing %q", want)
		}
	}
}

// TestDepsPartial_IncludesMentions verifies the htmx /b/{id}/deps partial
// (which the page swaps in on dependency mutations) also surfaces mentions
// data — otherwise an htmx-driven dep update would erase the Mentions
// sections from the page until a full reload.
func TestDepsPartial_IncludesMentions(t *testing.T) {
	f := newFake()
	f.mentions = map[string][]beadsapi.BeadReferenceSummary{
		"gd-aaa": {{ID: "gd-c3u76usvml", Title: "Some target", Type: "task", Status: "open", SourceField: "description"}},
	}
	f.mentionedBy = map[string][]beadsapi.BeadReferenceSummary{
		"gd-aaa": {{ID: "gd-qa1k0drtny", Title: "Citing", Type: "task", Status: "open", SourceField: "description"}},
	}

	mux := mkServer(t, f)
	req := httptest.NewRequest("GET", "/b/gd-aaa/deps", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{">Mentions<", ">Mentioned by<", "gd-c3u76usvml", "gd-qa1k0drtny"} {
		if !strings.Contains(body, want) {
			t.Errorf("/b/{id}/deps partial missing %q\nbody: %s", want, body)
		}
	}
}
