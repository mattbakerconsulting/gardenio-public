package main

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

func newTestWidgetCtxP6(t *testing.T) *widgetRenderCtx {
	t.Helper()
	tpl, err := template.New("").Funcs(template.FuncMap{
		"sparkConfig":    MarshalSparkConfig,
		"timelineConfig": MarshalTimelineConfig,
	}).ParseFS(getTestTemplateFS(),
		"templates/partials/widgets_phase4.html",
		"templates/partials/widgets_phase5.html",
		"templates/partials/widgets_phase6.html",
		"templates/partials/widgets_phase7.html")
	if err != nil {
		t.Fatalf("parse phase 4+5+6 partials: %v", err)
	}
	return &widgetRenderCtx{
		md:        goldmark.New(goldmark.WithExtensions(extension.GFM)),
		templates: tpl,
		hl:        newCodeHighlighter(),
	}
}

func TestRegisteredWidgets_Phase6SlugPresent(t *testing.T) {
	if !slices.Contains(registeredWidgets(), "fix-available-pending-branch") {
		t.Errorf("expected fix-available-pending-branch in registry; got %v", registeredWidgets())
	}
}

func TestParseUnifiedDiff_BasicAddDelCtx(t *testing.T) {
	src := `diff --git a/foo.go b/foo.go
--- a/foo.go
+++ b/foo.go
@@ -1,3 +1,4 @@ context line
 keep
-old line
+new line
+added`
	files := parseUnifiedDiff(src)
	if len(files) != 1 {
		t.Fatalf("want 1 file, got %d", len(files))
	}
	f := files[0]
	if f.OldPath != "foo.go" || f.NewPath != "foo.go" {
		t.Errorf("paths: %q -> %q", f.OldPath, f.NewPath)
	}
	if f.Language != "go" {
		t.Errorf("want go language, got %q", f.Language)
	}
	if f.Adds != 2 || f.Dels != 1 {
		t.Errorf("counts adds=%d dels=%d", f.Adds, f.Dels)
	}
	if len(f.Hunks) != 1 {
		t.Fatalf("want 1 hunk, got %d", len(f.Hunks))
	}
	h := f.Hunks[0]
	if h.OldStart != 1 || h.OldCount != 3 || h.NewStart != 1 || h.NewCount != 4 {
		t.Errorf("hunk header: -%d,%d +%d,%d", h.OldStart, h.OldCount, h.NewStart, h.NewCount)
	}
	if h.Context != "context line" {
		t.Errorf("hunk context: %q", h.Context)
	}
	// expect exactly: ctx, del, add, add (4 lines)
	if got := len(h.Lines); got != 4 {
		t.Fatalf("want 4 lines, got %d", got)
	}
	if h.Lines[0].Kind != "ctx" || h.Lines[1].Kind != "del" || h.Lines[2].Kind != "add" || h.Lines[3].Kind != "add" {
		t.Errorf("kinds: %+v", h.Lines)
	}
	if h.Lines[0].OldNum != 1 || h.Lines[0].NewNum != 1 {
		t.Errorf("ctx line nums: old=%d new=%d", h.Lines[0].OldNum, h.Lines[0].NewNum)
	}
	if h.Lines[1].OldNum != 2 || h.Lines[1].NewNum != 0 {
		t.Errorf("del line nums: old=%d new=%d", h.Lines[1].OldNum, h.Lines[1].NewNum)
	}
	if h.Lines[2].OldNum != 0 || h.Lines[2].NewNum != 2 {
		t.Errorf("add line nums: old=%d new=%d", h.Lines[2].OldNum, h.Lines[2].NewNum)
	}
}

func TestNormaliseCIStatus(t *testing.T) {
	cases := map[string]string{
		"passed":      "passed",
		"SUCCESS":     "passed",
		"failed":      "failed",
		"FAIL":        "failed",
		"running":     "running",
		"queued":      "pending",
		"":            "",
		"weird-state": "pending",
	}
	for in, want := range cases {
		if got := normaliseCIStatus(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

func TestRenderFixAvailablePendingBranch_Full(t *testing.T) {
	ctx := newTestWidgetCtxP6(t)
	r, ok := lookupWidget("fix-available-pending-branch")
	if !ok {
		t.Fatal("widget not registered")
	}
	payload := `{
		"title":"Fix available",
		"branch":"agent/fix",
		"base_branch":"main",
		"commit_sha":"9c2a3b7e1f4d8c0a5b6e9f2d4c1a3e5b7d9c0a2f",
		"author":"gardenio",
		"mr_id":"!2185",
		"mr_url":"https://gitlab.com/x/-/merge_requests/2185",
		"bead_id":"gd-incident-001",
		"ci_status":"passed",
		"summary_markdown":"Stale-cookie fix",
		"diff":"diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1,2 +1,2 @@\n keep\n-old\n+new\n"
	}`
	html, err := r(payload, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(html)
	for _, want := range []string{
		"Fix available",
		"agent/fix",
		"9c2a3b7", // short SHA
		"gardenio",
		`data-state="passed"`,
		"foo.go",
		"ii-w-diff-add",
		"ii-w-diff-del",
		`id="ii-merge-result-2185-gd-incident-001"`,
		"fast-track-merge",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
}

func TestRenderFixAvailablePendingBranch_EmptyDiff(t *testing.T) {
	ctx := newTestWidgetCtxP6(t)
	r, _ := lookupWidget("fix-available-pending-branch")
	html, err := r(`{"title":"Empty","ci_status":"running"}`, ctx)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(html)
	if !strings.Contains(s, "No diff available") {
		t.Errorf("want empty-state fallback; got:\n%s", s)
	}
	if !strings.Contains(s, `data-state="running"`) {
		t.Errorf("want running pill; got:\n%s", s)
	}
}

func TestFastTrackMergeAction_Descriptor(t *testing.T) {
	d := FastTrackMergeAction("!2185", "https://example/mr", "gd-1", "passed", "#target")
	if d.ID != "fast-track-merge" || d.Kind != ActionHtmxPost {
		t.Errorf("descriptor shape: %+v", d)
	}
	if d.Confirm == nil || !d.Confirm.Danger {
		t.Errorf("confirm should be danger; got %+v", d.Confirm)
	}
	for _, k := range []string{"mr_id", "mr_url", "bead_id", "ci_status", "bypass_reason"} {
		if _, ok := d.Params[k]; !ok {
			t.Errorf("params missing %q: %+v", k, d.Params)
		}
	}
}

// fakeBeadsCommenter records AddComment calls so the bypass-record handler
// test can assert without a real daemon.
type fakeBeadsCommenter struct {
	calls []struct{ id, author, body string }
	err   error
}

func (f *fakeBeadsCommenter) AddComment(_ context.Context, id, author, body string) error {
	f.calls = append(f.calls, struct{ id, author, body string }{id, author, body})
	return f.err
}

func TestFastTrackMergeHandler_GateBlocksOnFailedCI(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/actions/fast-track-merge", nil)
	err := s.fastTrackMergeHandler(w, r, ActionParams{
		"ci_status":     "failed",
		"bypass_reason": "x",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if w.Code != http.StatusPreconditionFailed {
		t.Errorf("want 412, got %d", w.Code)
	}
}

func TestFastTrackMergeHandler_RequiresBypassReason(t *testing.T) {
	s := &Server{}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/actions/fast-track-merge", nil)
	err := s.fastTrackMergeHandler(w, r, ActionParams{
		"ci_status":     "passed",
		"bypass_reason": "",
	})
	if err != nil {
		t.Fatalf("handler err: %v", err)
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("want 400, got %d", w.Code)
	}
}

func TestWriteBypassComment_RoundTrip(t *testing.T) {
	fc := &fakeBeadsCommenter{}
	err := writeBypassComment(context.Background(), fc, "gd-x", "!2185",
		"https://example/mr", "operator-initiated incident remediation", "matt@example.com")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(fc.calls) != 1 {
		t.Fatalf("want 1 call, got %d", len(fc.calls))
	}
	c := fc.calls[0]
	if c.id != "gd-x" || c.author != "matt@example.com" {
		t.Errorf("call shape: %+v", c)
	}
	for _, want := range []string{"Fast-track merge bypass record", "!2185", "matt@example.com", "operator-initiated"} {
		if !strings.Contains(c.body, want) {
			t.Errorf("body missing %q: %s", want, c.body)
		}
	}
}

func TestWriteBypassComment_NilCommenter(t *testing.T) {
	err := writeBypassComment(context.Background(), nil, "x", "y", "z", "r", "a")
	if err == nil {
		t.Errorf("want error on nil commenter")
	}
}

func TestWriteBypassComment_PropagatesErr(t *testing.T) {
	want := errors.New("boom")
	fc := &fakeBeadsCommenter{err: want}
	got := writeBypassComment(context.Background(), fc, "x", "y", "z", "r", "a")
	if got != want {
		t.Errorf("err propagation: got %v want %v", got, want)
	}
}

func TestActorFromRequest_Headers(t *testing.T) {
	r := httptest.NewRequest("POST", "/x", nil)
	r.Header.Set("X-User-Email", "alice@example.com")
	if got := actorFromRequest(r); got != "alice@example.com" {
		t.Errorf("got %q", got)
	}
	r2 := httptest.NewRequest("POST", "/x", nil)
	if got := actorFromRequest(r2); got != "unknown" {
		t.Errorf("default actor: got %q", got)
	}
}

func TestSwapTargetIDFor_DOMSafe(t *testing.T) {
	got := swapTargetIDFor("!2185", "gd-incident-001")
	if got != "ii-merge-result-2185-gd-incident-001" {
		t.Errorf("slug: %q", got)
	}
	if swapTargetIDFor("", "") != "ii-merge-result" {
		t.Errorf("empty fallback: %q", swapTargetIDFor("", ""))
	}
}

func TestStripChromaWrap_HandlesPreCode(t *testing.T) {
	in := `<pre tabindex="0" style="..."><code>hello <span>world</span></code></pre>`
	out := stripChromaWrap(in)
	if out != "hello <span>world</span>" {
		t.Errorf("got %q", out)
	}
}

func TestRegisterServerBoundActions_RegistersFastTrackMerge(t *testing.T) {
	s := &Server{actions: NewActionRegistry()}
	registerServerBoundActions(s)
	ids := s.actions.IDs()
	if !slices.Contains(ids, "fast-track-merge") {
		t.Errorf("fast-track-merge not registered; got %v", ids)
	}
}
