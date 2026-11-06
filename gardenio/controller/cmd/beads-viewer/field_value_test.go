package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHumanizeFieldKey(t *testing.T) {
	tests := []struct{ in, want string }{
		{"operation_spec", "Operation spec"},
		{"mr_url", "MR URL"},
		{"requesting_agent", "Requesting agent"},
		{"mr_iid", "MR IID"},
		{"gitlab_mr_merge", "Gitlab MR merge"},
		{"result", "Result"},
		{"k8s_sa", "K8s SA"},
		{"tldr", "TLDR"},
		{"approved-by", "Approved by"},
		{"", ""},
	}
	for _, tc := range tests {
		if got := humanizeFieldKey(tc.in); got != tc.want {
			t.Errorf("humanizeFieldKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestBareFieldURL(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"https://gitlab.com/a/b/-/merge_requests/1274", true},
		{"http://example.com", true},
		{"see https://gitlab.com/x", false}, // whitespace → markdown path
		{"ftp://example.com/x", false},
		{"not a url", false},
		{"https://", false},
	}
	for _, tc := range tests {
		if _, ok := bareFieldURL(tc.in); ok != tc.want {
			t.Errorf("bareFieldURL(%q) ok = %v, want %v", tc.in, ok, tc.want)
		}
	}
}

// TestRenderFieldValue_BareURLIsClickable is the direct regression for the
// operator complaint that started this work: "there's no way to click on the
// mr link".
func TestRenderFieldValue_BareURLIsClickable(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	got := string(renderFieldValue(nil, "https://gitlab.com/Acme/x/-/merge_requests/1274", "", now))
	if !strings.Contains(got, `href="https://gitlab.com/Acme/x/-/merge_requests/1274"`) {
		t.Errorf("bare URL field did not render an anchor: %s", got)
	}
	if !strings.Contains(got, `rel="noopener noreferrer"`) {
		t.Errorf("external anchor missing rel safety attrs: %s", got)
	}
}

// TestRenderFieldValue_JSONExpandsWithClickableNestedURL covers the real
// failure shape: the mr_url was NESTED inside the operation_spec JSON blob, so
// even GFM autolinking of the raw <pre> dump would not have helped.
func TestRenderFieldValue_JSONExpandsWithClickableNestedURL(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	spec := `{"mr_url":"https://gitlab.com/Acme/Platform/site-deployment/-/merge_requests/1274","mr_iid":1274,"squash":true,"project":"Acme/Platform/site-deployment"}`
	got := string(renderFieldValue(nil, spec, "", now))
	if !strings.Contains(got, `<dl class="field-json">`) {
		t.Errorf("JSON field did not expand to a key/value list: %s", got)
	}
	if !strings.Contains(got, `href="https://gitlab.com/Acme/Platform/site-deployment/-/merge_requests/1274"`) {
		t.Errorf("nested mr_url is not clickable: %s", got)
	}
	if !strings.Contains(got, "MR URL") {
		t.Errorf("nested key not humanized: %s", got)
	}
	if !strings.Contains(got, "<code>1274</code>") {
		t.Errorf("numeric leaf lost: %s", got)
	}
	if !strings.Contains(got, "<code>true</code>") {
		t.Errorf("bool leaf lost: %s", got)
	}
}

func TestRenderFieldValue_JSONArrayAndEmpties(t *testing.T) {
	now := time.Now()
	got := string(renderFieldValue(nil, `["a","b"]`, "", now))
	if !strings.Contains(got, `<ul class="field-json-list">`) {
		t.Errorf("JSON array did not render a list: %s", got)
	}
	if e := string(renderFieldValue(nil, "   ", "", now)); !strings.Contains(e, "empty") {
		t.Errorf("blank field should render the empty marker, got %s", e)
	}
}

// TestRenderFieldValue_NotJSON guards the false-positive direction: a scalar
// that merely starts with a brace, or has trailing garbage, must not be
// swallowed by the JSON path.
func TestRenderFieldValue_NotJSON(t *testing.T) {
	now := time.Now()
	for _, in := range []string{`{not json`, `{"a":1} trailing`, `1274`, `"quoted"`} {
		got := string(renderFieldValue(nil, in, "", now))
		if strings.Contains(got, "field-json") {
			t.Errorf("renderFieldValue(%q) took the JSON path: %s", in, got)
		}
	}
}

func TestRenderFieldValue_Timestamp(t *testing.T) {
	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	got := string(renderFieldValue(nil, "2026-08-27T10:00:00Z", "", now))
	if !strings.Contains(got, `class="field-time"`) {
		t.Errorf("RFC3339 field did not humanize: %s", got)
	}
	if !strings.Contains(got, "2026-08-27 10:00:00 UTC") {
		t.Errorf("exact stamp missing from title: %s", got)
	}
}

func TestRenderFieldValue_LinkifiesBeadIDs(t *testing.T) {
	got := string(renderFieldValue(nil, "blocked on gd-2HYAp93i4I", "", time.Now()))
	if !strings.Contains(got, "gd-2HYAp93i4I") || !strings.Contains(got, "<a ") {
		t.Errorf("bead id was not linkified: %s", got)
	}
}

func TestRenderFieldValue_EscapesHTML(t *testing.T) {
	got := string(renderFieldValue(nil, `<script>alert(1)</script>`, "", time.Now()))
	if strings.Contains(got, "<script>") {
		t.Errorf("field value did not escape raw HTML: %s", got)
	}
}

func TestUnwrapSoleParagraph(t *testing.T) {
	if got := unwrapSoleParagraph("<p>hello</p>"); got != "hello" {
		t.Errorf("unwrapSoleParagraph single = %q", got)
	}
	// The viewer's goldmark stamps data-block-id on every block, so the
	// attribute-bearing form is the one that actually shows up in production —
	// missing it leaves every scalar field wrapped in a margin-bearing <p>,
	// which reads as a broken two-row grid cell.
	if got := unwrapSoleParagraph(`<p data-block-id="blk-1">hello</p>`); got != "hello" {
		t.Errorf("unwrapSoleParagraph with attrs = %q", got)
	}
	if got := unwrapSoleParagraph(`<p data-block-id="blk-1">a</p>` + "\n" + `<p data-block-id="blk-2">b</p>`); !strings.Contains(got, "<p ") {
		t.Errorf("multi-paragraph with attrs was unwrapped: %q", got)
	}
	multi := "<p>a</p>\n<p>b</p>"
	if got := unwrapSoleParagraph(multi); got != multi {
		t.Errorf("unwrapSoleParagraph stripped a multi-paragraph value: %q", got)
	}
	if got := unwrapSoleParagraph("<ul><li>a</li></ul>"); got != "<ul><li>a</li></ul>" {
		t.Errorf("unwrapSoleParagraph touched a non-paragraph block: %q", got)
	}
}

func TestFieldJSONDepthCap(t *testing.T) {
	deep := `{"a":{"b":{"c":{"d":{"e":{"f":1}}}}}}`
	got := string(renderFieldValue(nil, deep, "", time.Now()))
	if !strings.Contains(got, "field-json-raw") {
		t.Errorf("deep nesting did not fall back to a <pre>: %s", got)
	}
}

// TestBeadShow_GenericFieldsRenderRich locks in the operator-visible half of
// ka-0000006884 on the GENERIC dump (the path every non-typed bead takes):
// the <dt> shows a humanized label while keeping the exact snake_case key in
// its title attribute, and the value is rendered inline rather than dumped in
// a <pre>. The beads-viewer E2E asserts the same contract in a real browser
// (tests/type_detail_more.spec.js, "type_generic (fallback)") — this test is
// the fast local guard so a template regression does not have to wait for CI.
func TestBeadShow_GenericFieldsRenderRich(t *testing.T) {
	mux := mkServer(t, newFake())
	req := httptest.NewRequest("GET", "/gd-aaa", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	body := w.Body.String()

	if !strings.Contains(body, `<dl class="field-list field-list-rich">`) {
		t.Errorf("generic Fields dump should use the rich field list\nbody: %s", body)
	}
	// The raw key survives as the title attribute (still greppable) while the
	// visible label is humanized.
	for _, want := range []string{
		`<dt title="acceptance_criteria">Acceptance criteria</dt>`,
		`<dt title="design">Design</dt>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing humanized field row %q\nbody: %s", want, body)
		}
	}
	// The old raw dump wrapped every value in <pre>; a one-line scalar must now
	// render inline in its <dd>.
	if strings.Contains(body, "<dd><pre>use a graph</pre></dd>") {
		t.Errorf("field value still rendered as a raw <pre> dump\nbody: %s", body)
	}
	if !strings.Contains(body, "<dd>use a graph</dd>") {
		t.Errorf("expected the field value inline in its <dd>\nbody: %s", body)
	}
}
