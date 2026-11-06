package main

import (
	"strings"
	"testing"
)

const anchorSectionDoc = `# Plan

intro prose

## C1 Shape C

shape overview

### C1.1 — TTL field

The TTL field carries the expiry.
It spans two lines.

### C1.2 — Refresh endpoint

refresh body

## 6.3 Retention

retention body

## Done

trailing`

func TestAnchorSection_FindsSubsectionByLabel(t *testing.T) {
	// `c1-1` should yield the C1.1 heading + its body, stopping at the next
	// same-or-shallower heading (### C1.2).
	heading, body, ok := anchorSection(anchorSectionDoc, "c1-1")
	if !ok {
		t.Fatal("expected to find c1-1 section")
	}
	if heading != "C1.1 — TTL field" {
		t.Errorf("heading = %q", heading)
	}
	if !strings.Contains(body, "The TTL field carries the expiry.") || !strings.Contains(body, "It spans two lines.") {
		t.Errorf("body missing section text: %q", body)
	}
	if strings.Contains(body, "refresh body") {
		t.Errorf("body leaked into next section: %q", body)
	}
}

func TestAnchorSection_LabelAsWritten(t *testing.T) {
	// The anchor may be supplied as the raw label `C1.1`; it slugifies to the
	// same target.
	h1, b1, ok1 := anchorSection(anchorSectionDoc, "C1.1")
	h2, b2, ok2 := anchorSection(anchorSectionDoc, "c1-1")
	if !ok1 || !ok2 || h1 != h2 || b1 != b2 {
		t.Errorf("raw label and slug disagree: (%v,%q) vs (%v,%q)", ok1, h1, ok2, h2)
	}
}

func TestAnchorSection_NumericADRHeading(t *testing.T) {
	heading, body, ok := anchorSection(anchorSectionDoc, "6-3")
	if !ok {
		t.Fatal("expected to find 6-3 section")
	}
	if heading != "6.3 Retention" {
		t.Errorf("heading = %q", heading)
	}
	if !strings.Contains(body, "retention body") || strings.Contains(body, "trailing") {
		t.Errorf("body wrong: %q", body)
	}
}

func TestAnchorSection_ParentStopsAtShallowerHeading(t *testing.T) {
	// `c1` is the H2 "## C1 Shape C"; its section includes the nested H3
	// subsections and stops at the next H2 (## 6.3).
	heading, body, ok := anchorSection(anchorSectionDoc, "c1")
	if !ok {
		t.Fatal("expected to find c1 section")
	}
	if heading != "C1 Shape C" {
		t.Errorf("heading = %q", heading)
	}
	if !strings.Contains(body, "shape overview") || !strings.Contains(body, "C1.1 — TTL field") {
		t.Errorf("parent section should include children: %q", body)
	}
	if strings.Contains(body, "retention body") {
		t.Errorf("parent section leaked past next H2: %q", body)
	}
}

func TestAnchorSection_LastSectionRunsToEnd(t *testing.T) {
	heading, body, ok := anchorSection(anchorSectionDoc, "done")
	if !ok {
		t.Fatal("expected to find done section")
	}
	if heading != "Done" || !strings.Contains(body, "trailing") {
		t.Errorf("heading=%q body=%q", heading, body)
	}
}

func TestAnchorSection_NoMatchFallsThrough(t *testing.T) {
	if _, _, ok := anchorSection(anchorSectionDoc, "nope"); ok {
		t.Error("expected no match for absent anchor")
	}
	if _, _, ok := anchorSection(anchorSectionDoc, ""); ok {
		t.Error("expected no match for empty anchor")
	}
}

func TestAnchorSection_IgnoresHeadingInCodeFence(t *testing.T) {
	doc := "## Real\n\n```\n# C1.1 not a heading\n```\n\n### C1.1 — actual\n\nbody"
	heading, body, ok := anchorSection(doc, "c1-1")
	if !ok {
		t.Fatal("expected to find the real C1.1 heading")
	}
	if heading != "C1.1 — actual" {
		t.Errorf("matched fenced comment instead of heading: %q", heading)
	}
	if !strings.Contains(body, "body") {
		t.Errorf("body = %q", body)
	}
}

func TestAnchorSection_CrossMarkerFenceDoesNotMisToggle(t *testing.T) {
	// Regression (review finding): a ~~~ line inside a backtick fence must NOT
	// close the fence, so a heading-looking line within it is not treated as a
	// real heading and the section boundary is preserved.
	doc := "## C1 Target\n\n```\nsome code\n~~~\n## Fake Inside fence\nmore code\n```\n\nreal prose body\n\n## C2 next\n\nx"
	heading, body, ok := anchorSection(doc, "c1")
	if !ok {
		t.Fatal("expected to find c1 section")
	}
	if heading != "C1 Target" {
		t.Errorf("heading = %q", heading)
	}
	if !strings.Contains(body, "real prose body") {
		t.Errorf("real body lost to mis-toggled fence: %q", body)
	}
	if strings.Contains(body, "C2 next") {
		t.Errorf("section leaked past next H2: %q", body)
	}
}

func TestAnchorSection_TildeFence(t *testing.T) {
	doc := "## C1 Target\n\n~~~\n## not a heading\n~~~\n\nbody\n\n## C2\n\nx"
	heading, body, ok := anchorSection(doc, "c1")
	if !ok || heading != "C1 Target" {
		t.Fatalf("ok=%v heading=%q", ok, heading)
	}
	if !strings.Contains(body, "body") || strings.Contains(body, "C2") {
		t.Errorf("body = %q", body)
	}
}

func TestAnchorSection_ClosedATXTrailingHash(t *testing.T) {
	// "## C1 Title ##" (closed ATX) → title "C1 Title"; "## C1 Notes on C#"
	// (content-terminal #) keeps the trailing #.
	doc := "## C1 Title ##\n\nbody"
	heading, _, ok := anchorSection(doc, "c1")
	if !ok || heading != "C1 Title" {
		t.Errorf("closed-ATX: ok=%v heading=%q (want \"C1 Title\")", ok, heading)
	}

	doc2 := "## C3 Notes on C#\n\nbody"
	heading2, _, ok2 := anchorSection(doc2, "c3")
	if !ok2 || heading2 != "C3 Notes on C#" {
		t.Errorf("content-terminal #: ok=%v heading=%q (want \"C3 Notes on C#\")", ok2, heading2)
	}
}

func TestAnchorSection_CRLF(t *testing.T) {
	doc := "## C1 Target\r\n\r\nbody line\r\n\r\n## C2\r\n\r\nx"
	heading, body, ok := anchorSection(doc, "c1")
	if !ok || heading != "C1 Target" {
		t.Fatalf("ok=%v heading=%q", ok, heading)
	}
	if !strings.Contains(body, "body line") || strings.Contains(body, "C2") {
		t.Errorf("body = %q", body)
	}
}

func TestAnchorSection_EmptyBody(t *testing.T) {
	// A heading immediately followed by a same-level heading yields an empty
	// body but still reports ok=true with the heading.
	doc := "## C1 Target\n## C2 next\n\nx"
	heading, body, ok := anchorSection(doc, "c1")
	if !ok || heading != "C1 Target" {
		t.Fatalf("ok=%v heading=%q", ok, heading)
	}
	if body != "" {
		t.Errorf("expected empty body, got %q", body)
	}
}
