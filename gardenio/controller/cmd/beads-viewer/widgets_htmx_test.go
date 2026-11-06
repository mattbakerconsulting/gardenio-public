package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// activityLogHTMXTemplate is the template_source the activity-log-htmx
// widget-type bead carries. It mirrors templates/partials/widgets_phase4.html
// `widget_activity_log` so a side-by-side render matches structure and the
// `<time datetime>` values the relative-time enhancer reads.
const activityLogHTMXTemplate = `<div class="ii-w-activity">
{{- $entries := index .Payload "entries" -}}
{{if $entries}}
  <ol class="ii-w-activity-list">
    {{range $entries}}
      <li class="ii-w-activity-row"{{if .kind}} data-kind="{{.kind}}"{{end}}>
        {{if .when}}<time class="ii-relative-time" datetime="{{.when}}">{{.when}}</time>{{end}}
        {{if .actor}}<span class="ii-w-activity-actor">{{.actor}}</span>{{end}}
        {{if .kind}}<span class="ii-pill" data-state="{{.kind}}">{{.kind}}</span>{{end}}
        {{if .text}}<span class="ii-w-activity-text">{{.text}}</span>{{end}}
      </li>
    {{end}}
  </ol>
{{else}}
  <p class="empty">No activity recorded.</p>
{{end}}
</div>`

// fakeHtmxResolver is the trimmed test double for the htmxResolver surface.
// It returns the configured beads regardless of query — every call site we
// exercise filters client-side by Fields["slug"], so a single shared list is
// sufficient.
type fakeHtmxResolver struct {
	beads       []*beadsapi.BeadDetail
	err         error
	callCount   int
	projects    map[string]beadsapi.ProjectInfo
	projectsErr error
}

// ListProjectBeads makes the fake an htmxTrustLookup; nil projects leave an
// unlabelled widget-type trusted.
func (f *fakeHtmxResolver) ListProjectBeads(context.Context) (map[string]beadsapi.ProjectInfo, error) {
	return f.projects, f.projectsErr
}

func (f *fakeHtmxResolver) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	f.callCount++
	if f.err != nil {
		return nil, f.err
	}
	// Defensive: only respond to widget-type queries so an accidental
	// reuse of this fake elsewhere is loud.
	wantsWidgetType := false
	for _, t := range q.Types {
		if t == "widget-type" {
			wantsWidgetType = true
		}
	}
	if !wantsWidgetType {
		return &beadsapi.ListBeadsResult{}, nil
	}
	return &beadsapi.ListBeadsResult{Beads: f.beads}, nil
}

func makeHtmxWidgetTypeBead(id, slug, src string, updated time.Time) *beadsapi.BeadDetail {
	return &beadsapi.BeadDetail{
		ID:   id,
		Type: "widget-type",
		Fields: map[string]string{
			"slug":            slug,
			"widget_version":  "1",
			"renderer_kind":   "htmx",
			"template_engine": "go-html-template-v1",
			"template_source": src,
		},
		UpdatedAt: updated,
	}
}

// TestCompileHtmxRenderer_RendersPayload exercises the happy path: a valid
// template_source compiles, the renderer decodes the instance JSON, and the
// rendered HTML carries the expected structural markers.
func TestCompileHtmxRenderer_RendersPayload(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-idvrgcqnb1", "activity-log-htmx",
		activityLogHTMXTemplate, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))

	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compileHtmxRenderer: %v", err)
	}

	payload := `{"entries":[
		{"when":"2026-05-01T10:00:00Z","actor":"alice","kind":"deploy","text":"v2026.122.1 to prod"},
		{"when":"2026-05-01T10:05:00Z","actor":"bob","kind":"alert","text":"latency spike"}
	]}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("renderer error: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		`<ol class="ii-w-activity-list">`,
		`datetime="2026-05-01T10:00:00Z"`,
		`datetime="2026-05-01T10:05:00Z"`,
		`data-kind="deploy"`,
		`data-state="alert"`,
		`alice`, `bob`,
		`v2026.122.1 to prod`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// TestCompileHtmxRenderer_EmptyEntries falls into the "No activity" arm.
func TestCompileHtmxRenderer_EmptyEntries(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-givk2halxi", "activity-log-htmx",
		activityLogHTMXTemplate, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{"entries":[]}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(html), "No activity recorded.") {
		t.Errorf("expected empty-state copy, got: %s", string(html))
	}
}

// TestCompileHtmxRenderer_BadTemplate surfaces a parse error.
func TestCompileHtmxRenderer_BadTemplate(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-9bkxzcy6vt", "broken",
		`{{ if .Payload.entries }}{{ end`, // unterminated action
		time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	if _, err := compileHtmxRenderer(bead); err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

// TestCompileHtmxRenderer_RejectsUnknownEngine refuses unsupported engines.
func TestCompileHtmxRenderer_RejectsUnknownEngine(t *testing.T) {
	resetHtmxRendererCache()
	bead := &beadsapi.BeadDetail{
		ID:   "gd-frkgtsphhv",
		Type: "widget-type",
		Fields: map[string]string{
			"slug":            "x",
			"renderer_kind":   "htmx",
			"template_engine": "jinja2",
			"template_source": "<div>x</div>",
		},
	}
	if _, err := compileHtmxRenderer(bead); err == nil {
		t.Fatal("expected unsupported-engine error, got nil")
	}
}

// TestLookupWidgetForReport_BuiltinWins ensures builtin renderers (registered
// via init() in widgets_phase4.go) trump any htmx fallback. activity-log is a
// builtin slug per Strategy A — port-then-soak: the htmx variant lives under
// activity-log-htmx, and the legacy path keeps rendering through Go.
func TestLookupWidgetForReport_BuiltinWins(t *testing.T) {
	resetHtmxRendererCache()
	// Even with a resolver that would happily return an htmx bead for
	// activity-log, the builtin registration wins.
	resolver := &fakeHtmxResolver{
		beads: []*beadsapi.BeadDetail{
			makeHtmxWidgetTypeBead("gd-hv6lt8lh0k", "activity-log",
				activityLogHTMXTemplate, time.Now()),
		},
	}
	if _, ok := lookupWidgetForReport(context.Background(), "activity-log", resolver); !ok {
		t.Fatal("expected builtin activity-log to resolve")
	}
	if resolver.callCount != 0 {
		t.Errorf("resolver consulted despite builtin hit: callCount=%d", resolver.callCount)
	}
}

// TestLookupWidgetForReport_HtmxFallback exercises the port path: a slug with
// no builtin (activity-log-htmx) resolves through the daemon to an htmx
// widget-type bead, compiles, and renders.
func TestLookupWidgetForReport_HtmxFallback(t *testing.T) {
	resetHtmxRendererCache()
	resolver := &fakeHtmxResolver{
		beads: []*beadsapi.BeadDetail{
			makeHtmxWidgetTypeBead("gd-o96fkxqcat", "activity-log-htmx",
				activityLogHTMXTemplate, time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)),
		},
	}
	r, ok := lookupWidgetForReport(context.Background(), "activity-log-htmx", resolver)
	if !ok {
		t.Fatal("expected htmx fallback to resolve activity-log-htmx")
	}
	html, err := r(`{"entries":[{"when":"2026-05-01T10:00:00Z","actor":"alice","kind":"deploy","text":"hi"}]}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(html), `datetime="2026-05-01T10:00:00Z"`) {
		t.Errorf("htmx render missing datetime; got: %s", string(html))
	}
}

// TestLookupWidgetForReport_NoMatch returns false when no builtin and no
// matching widget-type bead exist.
func TestLookupWidgetForReport_NoMatch(t *testing.T) {
	resetHtmxRendererCache()
	resolver := &fakeHtmxResolver{beads: nil}
	if _, ok := lookupWidgetForReport(context.Background(), "ghost-widget", resolver); ok {
		t.Fatal("expected ok=false for unknown slug")
	}
}

// TestLookupWidgetForReport_NilResolver — pre-Phase-6 callers (and unit tests
// without a daemon stub) pass nil; the function must short-circuit to the
// builtin lookup and never panic.
func TestLookupWidgetForReport_NilResolver(t *testing.T) {
	resetHtmxRendererCache()
	if _, ok := lookupWidgetForReport(context.Background(), "activity-log", nil); !ok {
		t.Fatal("expected builtin to resolve with nil resolver")
	}
	if _, ok := lookupWidgetForReport(context.Background(), "activity-log-htmx", nil); ok {
		t.Fatal("expected ok=false when resolver is nil and slug has no builtin")
	}
}

// TestLookupWidgetForReport_EmptySlug guards the trim-empty short-circuit.
func TestLookupWidgetForReport_EmptySlug(t *testing.T) {
	resetHtmxRendererCache()
	resolver := &fakeHtmxResolver{}
	if _, ok := lookupWidgetForReport(context.Background(), "   ", resolver); ok {
		t.Fatal("expected empty slug to return ok=false")
	}
	if resolver.callCount != 0 {
		t.Errorf("resolver consulted for empty slug: callCount=%d", resolver.callCount)
	}
}

// TestLookupWidgetForReport_ResolverError swallows daemon errors and returns
// ok=false — the rich-report falls through to the placeholder card rather
// than failing the page on a single bad widget.
func TestLookupWidgetForReport_ResolverError(t *testing.T) {
	resetHtmxRendererCache()
	resolver := &fakeHtmxResolver{err: errors.New("daemon down")}
	if _, ok := lookupWidgetForReport(context.Background(), "activity-log-htmx", resolver); ok {
		t.Fatal("expected ok=false on resolver error")
	}
}

// TestLookupWidgetForReport_FiltersByRendererKind ignores widget-type beads
// whose renderer_kind is not htmx — those are claimed by the builtin
// registry, not the dispatcher.
func TestLookupWidgetForReport_FiltersByRendererKind(t *testing.T) {
	resetHtmxRendererCache()
	resolver := &fakeHtmxResolver{
		beads: []*beadsapi.BeadDetail{
			{
				ID:   "gd-ubpi8fpdza",
				Type: "widget-type",
				Fields: map[string]string{
					"slug":          "only-builtin",
					"renderer_kind": "builtin",
				},
				UpdatedAt: time.Now(),
			},
		},
	}
	if _, ok := lookupWidgetForReport(context.Background(), "only-builtin", resolver); ok {
		t.Fatal("renderer_kind=builtin must not resolve through the htmx path")
	}
}

// TestCompileHtmxRenderer_CacheReusesCompile verifies the (bead.ID,
// bead.UpdatedAt) cache returns the same compiled renderer on repeat lookups.
func TestCompileHtmxRenderer_CacheReusesCompile(t *testing.T) {
	resetHtmxRendererCache()
	updated := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	bead := makeHtmxWidgetTypeBead("gd-7gkqtngwjk", "activity-log-htmx",
		activityLogHTMXTemplate, updated)

	r1, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile1: %v", err)
	}
	r2, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile2: %v", err)
	}
	// Both must render identically. We can't compare function pointers safely,
	// so assert byte-equal output on a fixed payload.
	out1, _ := r1(`{"entries":[{"when":"t","actor":"a","kind":"k","text":"x"}]}`, nil)
	out2, _ := r2(`{"entries":[{"when":"t","actor":"a","kind":"k","text":"x"}]}`, nil)
	if out1 != out2 {
		t.Errorf("cached renderer drifted:\n%s\n---\n%s", out1, out2)
	}
}

// TestCompileHtmxRenderer_CacheInvalidatesOnUpdate edits the same bead
// (template_source change + bumped UpdatedAt) and verifies the new template
// takes effect.
func TestCompileHtmxRenderer_CacheInvalidatesOnUpdate(t *testing.T) {
	resetHtmxRendererCache()
	id := "gd-9k9gmxfsvt"
	v1 := makeHtmxWidgetTypeBead(id, "activity-log-htmx",
		`<div data-v="1">v1</div>`,
		time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	r1, err := compileHtmxRenderer(v1)
	if err != nil {
		t.Fatalf("v1 compile: %v", err)
	}
	out1, _ := r1(`{}`, nil)
	if !strings.Contains(string(out1), `data-v="1"`) {
		t.Fatalf("v1 render unexpected: %s", out1)
	}

	v2 := makeHtmxWidgetTypeBead(id, "activity-log-htmx",
		`<div data-v="2">v2</div>`,
		time.Date(2026, 5, 1, 13, 0, 0, 0, time.UTC)) // newer
	r2, err := compileHtmxRenderer(v2)
	if err != nil {
		t.Fatalf("v2 compile: %v", err)
	}
	out2, _ := r2(`{}`, nil)
	if !strings.Contains(string(out2), `data-v="2"`) {
		t.Errorf("expected v2 to take effect; got: %s", out2)
	}
}

// TestActivityLogParity is the side-by-side fidelity check the port plan
// requires (research §6.1, ka-0000004584 acceptance). Same payload through
// the Go renderer and the htmx widget-type produces structurally-equivalent
// HTML: same number of <li> rows, the same datetime values (the
// relative-time enhancer reads `<time datetime>`), and the same actor / kind
// / text strings.
func TestActivityLogParity(t *testing.T) {
	resetHtmxRendererCache()
	payload := `{"entries":[
		{"when":"2026-05-01T10:00:00Z","actor":"alice","kind":"DEPLOY","text":"v2026.122.1 to prod"},
		{"when":"2026-05-01T10:05:00Z","actor":"bob","kind":"ALERT","text":"latency spike <500ms>"}
	]}`

	wctx := newTestWidgetCtx(t)
	goHTML, err := renderActivityLog(payload, wctx)
	if err != nil {
		t.Fatalf("Go renderer: %v", err)
	}

	htmxBead := makeHtmxWidgetTypeBead("gd-gcyhnkg6b1", "activity-log-htmx",
		activityLogHTMXTemplate, time.Now())
	htmxR, err := compileHtmxRenderer(htmxBead)
	if err != nil {
		t.Fatalf("htmx compile: %v", err)
	}
	// Lowercase the kind in the payload so the htmx template matches the Go
	// renderer's `strings.ToLower(kind)` normalization. The htmx template
	// intentionally does NOT lowercase — that normalization is a Go-renderer
	// concern that future templates can replicate via an `eq` / sprig fn.
	htmxPayload := strings.ReplaceAll(strings.ReplaceAll(payload,
		`"DEPLOY"`, `"deploy"`),
		`"ALERT"`, `"alert"`)
	htmxHTML, err := htmxR(htmxPayload, nil)
	if err != nil {
		t.Fatalf("htmx render: %v", err)
	}

	// Parity assertions: every datetime, actor, and text from the payload
	// must appear in BOTH outputs. The kind data-attributes (`data-kind`,
	// `data-state`) appear in both, lowercased.
	for _, want := range []string{
		`datetime="2026-05-01T10:00:00Z"`,
		`datetime="2026-05-01T10:05:00Z"`,
		`alice`, `bob`,
		`data-kind="deploy"`, `data-state="deploy"`,
		`data-kind="alert"`, `data-state="alert"`,
		`v2026.122.1 to prod`,
		// html/template auto-escapes `<500ms>` identically in both paths.
		`latency spike &lt;500ms&gt;`,
	} {
		if !strings.Contains(string(goHTML), want) {
			t.Errorf("Go renderer missing %q\n--- Go ---\n%s", want, goHTML)
		}
		if !strings.Contains(string(htmxHTML), want) {
			t.Errorf("htmx renderer missing %q\n--- htmx ---\n%s", want, htmxHTML)
		}
	}

	// Both renders must produce exactly two <li> rows.
	if got := strings.Count(string(goHTML), "<li "); got != 2 {
		t.Errorf("Go: <li count = %d, want 2", got)
	}
	if got := strings.Count(string(htmxHTML), "<li "); got != 2 {
		t.Errorf("htmx: <li count = %d, want 2", got)
	}
}

// relatedTicketsHTMXTemplate is the template_source the related-tickets-htmx
// widget-type bead carries. It mirrors templates/partials/widgets_phase4.html
// `widget_related_tickets` so a side-by-side render matches structure
// (data-kind, data-source-kind, data-state, ii-w-related-* classes).
//
// Two intentional gaps vs. the Go renderer (ka-0000005540 port doc):
//   - kind/state are emitted verbatim (the Go path ToLowers them server-side);
//     real instances should emit lowercase strings.
//   - the action button (OpenJiraAction / OpenMRAction) is not constructed —
//     those helpers live server-side. Renderer parity stops at the row chrome.
const relatedTicketsHTMXTemplate = `<div class="ii-w-related">
{{- $tickets := index .Payload "tickets" -}}
{{if $tickets}}
  <ul class="ii-w-related-list">
    {{range $tickets}}
      <li class="ii-w-related-row"{{if .kind}} data-kind="{{.kind}}"{{end}}>
        {{if .kind}}<span class="ii-source" data-source-kind="{{if eq .kind "jira"}}jira{{else if eq .kind "mr"}}gitlab{{else}}{{.kind}}{{end}}"><span class="ii-source-icon" aria-hidden="true"></span>{{.kind}}</span>{{end}}
        {{if .id}}<code class="ii-w-related-id">{{.id}}</code>{{end}}
        {{if .title}}<span class="ii-w-related-title">{{.title}}</span>{{end}}
        {{if .state}}<span class="ii-pill" data-state="{{.state}}">{{.state}}</span>{{end}}
      </li>
    {{end}}
  </ul>
{{else}}
  <p class="empty">No related tickets.</p>
{{end}}
</div>`

// TestCompileHtmxRenderer_RelatedTickets exercises the related-tickets-htmx
// happy path: flat tickets[] payload renders rows with all the data-*
// attributes the existing CSS targets.
func TestCompileHtmxRenderer_RelatedTickets(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-pkfzu5y8lu", "related-tickets-htmx",
		relatedTicketsHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compileHtmxRenderer: %v", err)
	}
	payload := `{"tickets":[
		{"id":"FSM-12345","kind":"jira","title":"TMF upload broken","state":"open"},
		{"id":"!2185","kind":"mr","title":"Fast-track merge","url":"https://gitlab.com/x/-/merge_requests/2185","state":"merged"}
	]}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("renderer error: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		`<ul class="ii-w-related-list">`,
		`data-kind="jira"`,
		`data-kind="mr"`,
		`data-source-kind="jira"`,
		`data-source-kind="gitlab"`,
		`<code class="ii-w-related-id">FSM-12345</code>`,
		`<code class="ii-w-related-id">!2185</code>`,
		`TMF upload broken`,
		`Fast-track merge`,
		`data-state="open"`,
		`data-state="merged"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// TestCompileHtmxRenderer_RelatedTicketsEmpty falls into the empty-state arm.
func TestCompileHtmxRenderer_RelatedTicketsEmpty(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-ayudw8nqzz", "related-tickets-htmx",
		relatedTicketsHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{"tickets":[]}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(html), "No related tickets.") {
		t.Errorf("expected empty-state copy, got: %s", string(html))
	}
}

// TestRelatedTicketsParity is the side-by-side fidelity check for the
// related-tickets-htmx port — the second port after activity-log-htmx
// (ka-0000004584, MR !2334). Same payload through the Go renderer and the
// htmx widget-type produces structurally-equivalent HTML row chrome:
// data-kind, data-source-kind (with the jira→jira / mr→gitlab mapping),
// id, title, and lowercased state pill.
//
// Parity gap (intentional, port doc): the Go renderer wraps each row with
// an action <button> built from OpenJiraAction / OpenMRAction descriptors;
// the htmx template body has no access to those helpers, so it omits the
// button. The Go renderer also lowercases kind/state server-side; the htmx
// payload is pre-lowered to match (same approach as TestActivityLogParity).
func TestRelatedTicketsParity(t *testing.T) {
	resetHtmxRendererCache()
	payload := `{"tickets":[
		{"id":"FSM-12345","kind":"jira","title":"TMF upload broken","state":"OPEN"},
		{"id":"!2185","kind":"mr","title":"Fast-track merge <bug>","url":"https://gitlab.com/x/-/merge_requests/2185","state":"MERGED"}
	]}`

	wctx := newTestWidgetCtx(t)
	goHTML, err := renderRelatedTickets(payload, wctx)
	if err != nil {
		t.Fatalf("Go renderer: %v", err)
	}

	htmxBead := makeHtmxWidgetTypeBead("gd-9qdfalqvb3", "related-tickets-htmx",
		relatedTicketsHTMXTemplate, time.Now())
	htmxR, err := compileHtmxRenderer(htmxBead)
	if err != nil {
		t.Fatalf("htmx compile: %v", err)
	}
	// Mirror the Go renderer's strings.ToLower(kind/state) normalization in
	// the payload — same approach as TestActivityLogParity.
	htmxPayload := strings.NewReplacer(
		`"OPEN"`, `"open"`,
		`"MERGED"`, `"merged"`,
	).Replace(payload)
	htmxHTML, err := htmxR(htmxPayload, nil)
	if err != nil {
		t.Fatalf("htmx render: %v", err)
	}

	// Parity assertions: every row-chrome marker the existing CSS targets
	// must appear in BOTH outputs.
	for _, want := range []string{
		`data-kind="jira"`,
		`data-kind="mr"`,
		`data-source-kind="jira"`,
		`data-source-kind="gitlab"`,
		`FSM-12345`,
		`!2185`,
		`TMF upload broken`,
		// html/template auto-escapes `<bug>` identically in both paths.
		`Fast-track merge &lt;bug&gt;`,
		`data-state="open"`,
		`data-state="merged"`,
	} {
		if !strings.Contains(string(goHTML), want) {
			t.Errorf("Go renderer missing %q\n--- Go ---\n%s", want, goHTML)
		}
		if !strings.Contains(string(htmxHTML), want) {
			t.Errorf("htmx renderer missing %q\n--- htmx ---\n%s", want, htmxHTML)
		}
	}

	// Both renders must produce exactly two <li> rows.
	if got := strings.Count(string(goHTML), "<li "); got != 2 {
		t.Errorf("Go: <li count = %d, want 2", got)
	}
	if got := strings.Count(string(htmxHTML), "<li "); got != 2 {
		t.Errorf("htmx: <li count = %d, want 2", got)
	}

	// Documented parity gap: the Go renderer emits an Open / Open MR action
	// button per row; the htmx template does not. This assertion locks the
	// gap in place — change with eyes-open if a future sprig fn or Go-helper
	// makes button construction template-side feasible.
	if got := strings.Count(string(goHTML), `class="ii-action-btn"`); got != 2 {
		t.Errorf("Go: ii-action-btn count = %d, want 2 (Open + Open MR)", got)
	}
	if got := strings.Count(string(htmxHTML), `class="ii-action-btn"`); got != 0 {
		t.Errorf("htmx: ii-action-btn count = %d, want 0 (button is a parity gap)", got)
	}
}

// headerIncidentSummaryHTMXTemplate is the template_source the
// header-incident-summary-htmx widget-type bead carries. It mirrors
// templates/partials/widgets_phase4.html `widget_header_incident_summary` so
// a side-by-side render matches the header-card chrome (data-severity /
// data-status pills, title heading, started-time element).
//
// Two intentional gaps vs. the Go renderer (ka-0000005304 port doc):
//   - severity/status are emitted verbatim (the Go path ToLowers them
//     server-side); real instances should emit lowercase strings.
//   - the action buttons (Investigate / Acknowledge — built from
//     OpenMRAction / AckIncidentAction) are not constructed; those helpers
//     live server-side. Renderer parity stops at the header chrome.
const headerIncidentSummaryHTMXTemplate = `<div class="ii-w-header">
{{- $title := index .Payload "title" -}}
{{- $severity := index .Payload "severity" -}}
{{- $status := index .Payload "status" -}}
{{- $startedAt := index .Payload "started_at" -}}
  <div class="ii-w-header-top">
    {{if $severity}}<span class="ii-pill" data-severity="{{$severity}}">{{$severity}}</span>{{end}}
    {{if $title}}<h3 class="ii-w-title">{{$title}}</h3>{{end}}
    {{if $status}}<span class="ii-pill" data-status="{{$status}}">{{$status}}</span>{{end}}
  </div>
  {{if $startedAt}}
    <p class="ii-w-meta">
      Started <time class="ii-relative-time" datetime="{{$startedAt}}">{{$startedAt}}</time>
    </p>
  {{end}}
</div>`

// TestCompileHtmxRenderer_HeaderIncidentSummary exercises the
// header-incident-summary-htmx happy path: full payload renders the chrome
// with data-* attributes the existing CSS targets.
func TestCompileHtmxRenderer_HeaderIncidentSummary(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-fgr04tx7nc", "header-incident-summary-htmx",
		headerIncidentSummaryHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compileHtmxRenderer: %v", err)
	}
	payload := `{
		"title":"study-web 502 spike",
		"severity":"p1",
		"status":"open",
		"started_at":"2026-05-06T12:00:00Z",
		"incident_id":"gd-incident-001"
	}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("renderer error: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		`<div class="ii-w-header">`,
		`<div class="ii-w-header-top">`,
		`data-severity="p1"`,
		`data-status="open"`,
		`<h3 class="ii-w-title">study-web 502 spike</h3>`,
		`datetime="2026-05-06T12:00:00Z"`,
		`Started`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// TestCompileHtmxRenderer_HeaderIncidentSummaryEmpty falls into the
// minimum-payload arm where every optional field is missing — empty
// payload renders the chrome shell with no pills, title, or meta line.
func TestCompileHtmxRenderer_HeaderIncidentSummaryEmpty(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-u9lplsrfld", "header-incident-summary-htmx",
		headerIncidentSummaryHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, `<div class="ii-w-header">`) {
		t.Errorf("expected header shell, got: %s", got)
	}
	for _, banned := range []string{
		`data-severity`, `data-status`, `<h3`, `<time`, `Started`,
	} {
		if strings.Contains(got, banned) {
			t.Errorf("unexpected %q in empty render:\n%s", banned, got)
		}
	}
}

// TestHeaderIncidentSummaryParity is the side-by-side fidelity check for
// the header-incident-summary-htmx port — third port after activity-log-htmx
// (ka-0000004584, MR !2334) and related-tickets-htmx (ka-0000005540, MR !2346).
// Same payload through the Go renderer and the htmx widget-type produces
// structurally-equivalent header chrome: data-severity, data-status,
// h3 title, and the started_at <time datetime> element.
//
// Parity gap (intentional, port doc): the Go renderer emits Investigate /
// Acknowledge `<button class="ii-action-btn">` elements built from
// OpenMRAction / AckIncidentAction descriptors; the htmx template body has
// no access to those helpers, so it omits the buttons. The Go renderer also
// lowercases severity/status server-side; the htmx payload is pre-lowered
// to match (same approach as TestActivityLogParity / TestRelatedTicketsParity).
func TestHeaderIncidentSummaryParity(t *testing.T) {
	resetHtmxRendererCache()
	payload := `{
		"title":"study-web 502 spike <bug>",
		"severity":"P1",
		"status":"OPEN",
		"started_at":"2026-05-06T12:00:00Z",
		"incident_id":"gd-incident-001",
		"primary_action_url":"https://gitlab.com/x/-/merge_requests/2185",
		"primary_action_label":"Open MR"
	}`

	wctx := newTestWidgetCtx(t)
	goHTML, err := renderHeaderIncidentSummary(payload, wctx)
	if err != nil {
		t.Fatalf("Go renderer: %v", err)
	}

	htmxBead := makeHtmxWidgetTypeBead("gd-xku4msqkth", "header-incident-summary-htmx",
		headerIncidentSummaryHTMXTemplate, time.Now())
	htmxR, err := compileHtmxRenderer(htmxBead)
	if err != nil {
		t.Fatalf("htmx compile: %v", err)
	}
	// Mirror the Go renderer's strings.ToLower(severity/status) normalization
	// in the payload — same approach as TestRelatedTicketsParity.
	htmxPayload := strings.NewReplacer(
		`"P1"`, `"p1"`,
		`"OPEN"`, `"open"`,
	).Replace(payload)
	htmxHTML, err := htmxR(htmxPayload, nil)
	if err != nil {
		t.Fatalf("htmx render: %v", err)
	}

	// Parity assertions: every header-chrome marker the existing CSS targets
	// must appear in BOTH outputs.
	for _, want := range []string{
		`<div class="ii-w-header">`,
		`<div class="ii-w-header-top">`,
		`data-severity="p1"`,
		`data-status="open"`,
		// html/template auto-escapes `<bug>` identically in both paths.
		`study-web 502 spike &lt;bug&gt;`,
		`datetime="2026-05-06T12:00:00Z"`,
		`<time class="ii-relative-time"`,
	} {
		if !strings.Contains(string(goHTML), want) {
			t.Errorf("Go renderer missing %q\n--- Go ---\n%s", want, goHTML)
		}
		if !strings.Contains(string(htmxHTML), want) {
			t.Errorf("htmx renderer missing %q\n--- htmx ---\n%s", want, htmxHTML)
		}
	}

	// Both renders must produce exactly one <h3> title.
	if got := strings.Count(string(goHTML), "<h3"); got != 1 {
		t.Errorf("Go: <h3 count = %d, want 1", got)
	}
	if got := strings.Count(string(htmxHTML), "<h3"); got != 1 {
		t.Errorf("htmx: <h3 count = %d, want 1", got)
	}

	// Documented parity gap: the Go renderer emits Investigate + Acknowledge
	// action buttons (one for primary_action_url, one for incident_id); the
	// htmx template emits zero. This assertion locks the gap in place — change
	// with eyes-open if a future sprig fn or Go-helper makes button
	// construction template-side feasible.
	if got := strings.Count(string(goHTML), `class="ii-action-btn"`); got != 2 {
		t.Errorf("Go: ii-action-btn count = %d, want 2 (Investigate + Acknowledge)", got)
	}
	if got := strings.Count(string(htmxHTML), `class="ii-action-btn"`); got != 0 {
		t.Errorf("htmx: ii-action-btn count = %d, want 0 (buttons are a parity gap)", got)
	}
}

// reproductionStepsHTMXTemplate is the template_source the
// reproduction-steps-htmx widget-type bead carries. It mirrors
// templates/partials/widgets_phase4.html `widget_reproduction_steps` so a
// side-by-side render matches the markdown chrome (.ii-w-markdown
// .ii-w-repro container, numbered <ol> for the step list, and the empty-
// state paragraph copy).
//
// One intentional gap vs. the Go renderer (ka-0000007835 port doc):
//   - the `markdown` field is server-side only. The Go path runs goldmark
//     against widgetRenderCtx.md to render markdown into HTML; html/template
//     has no goldmark hook, so the htmx template only accepts the structured
//     steps[] shape. Producers should emit steps[] for instances that want
//     to render through the htmx path.
const reproductionStepsHTMXTemplate = `<div class="ii-w-markdown ii-w-repro">
{{- $steps := index .Payload "steps" -}}
{{if $steps}}
  <ol>
    {{range $steps}}<li>{{.}}</li>{{end}}
  </ol>
{{else}}
  <p class="empty">No reproduction steps recorded.</p>
{{end}}
</div>`

// TestCompileHtmxRenderer_ReproductionSteps exercises the
// reproduction-steps-htmx happy path: steps[] payload renders the chrome
// with the numbered list the existing CSS targets.
func TestCompileHtmxRenderer_ReproductionSteps(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-gfsqgyepdx", "reproduction-steps-htmx",
		reproductionStepsHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compileHtmxRenderer: %v", err)
	}
	payload := `{"steps":[
		"Open the upload dialog",
		"Pick a 50 MB PDF",
		"Click Upload — observe the spinner hang"
	]}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("renderer error: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		`<div class="ii-w-markdown ii-w-repro">`,
		`<ol>`,
		`<li>Open the upload dialog</li>`,
		`<li>Pick a 50 MB PDF</li>`,
		`<li>Click Upload — observe the spinner hang</li>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q\n--- got ---\n%s", want, got)
		}
	}
	if c := strings.Count(got, "<li>"); c != 3 {
		t.Errorf("<li> count = %d, want 3", c)
	}
}

// TestCompileHtmxRenderer_ReproductionStepsEmpty falls into the empty-
// state arm — empty steps array renders the placeholder copy and no <ol>.
func TestCompileHtmxRenderer_ReproductionStepsEmpty(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-ywc4ezmqdo", "reproduction-steps-htmx",
		reproductionStepsHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{"steps":[]}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, `<div class="ii-w-markdown ii-w-repro">`) {
		t.Errorf("expected chrome shell, got: %s", got)
	}
	if !strings.Contains(got, "No reproduction steps recorded.") {
		t.Errorf("expected empty-state copy, got: %s", got)
	}
	if strings.Contains(got, "<ol") {
		t.Errorf("unexpected <ol> in empty render:\n%s", got)
	}
}

// TestReproductionStepsParity is the side-by-side fidelity check for the
// reproduction-steps-htmx port — fourth port after activity-log-htmx
// (ka-0000004584, MR !2334), related-tickets-htmx (ka-0000005540,
// MR !2346), and header-incident-summary-htmx (ka-0000005304, MR !2347).
// Same steps[] payload through the Go renderer (which feeds the strings
// into goldmark as `1. step\n2. step\n…`) and the htmx widget-type produces
// structurally-equivalent reproduction-steps chrome: the .ii-w-markdown
// .ii-w-repro container, a numbered <ol>, and one <li> per step with the
// original step text.
//
// Parity gap (intentional, port doc): the Go renderer also accepts a
// `markdown` field and runs it through goldmark, so producers can ship
// rich markdown (bold, links, code, multi-paragraph). The htmx template
// has no goldmark hook and renders steps[] as plain <li>s only. The
// markdown wire-shape is therefore a Go-renderer-only affordance — see
// ka-0000007835 port doc for the two paths to close the gap if a future
// port needs richer markup.
func TestReproductionStepsParity(t *testing.T) {
	resetHtmxRendererCache()
	payload := `{"steps":[
		"Open the upload dialog",
		"Pick a 50 MB PDF",
		"Click Upload — observe the spinner hang"
	]}`

	wctx := newTestWidgetCtx(t)
	goHTML, err := renderReproductionSteps(payload, wctx)
	if err != nil {
		t.Fatalf("Go renderer: %v", err)
	}

	htmxBead := makeHtmxWidgetTypeBead("gd-twxu2im6pk", "reproduction-steps-htmx",
		reproductionStepsHTMXTemplate, time.Now())
	htmxR, err := compileHtmxRenderer(htmxBead)
	if err != nil {
		t.Fatalf("htmx compile: %v", err)
	}
	htmxHTML, err := htmxR(payload, nil)
	if err != nil {
		t.Fatalf("htmx render: %v", err)
	}

	// Parity assertions: every chrome marker the existing CSS targets must
	// appear in BOTH outputs. The Go renderer wraps the goldmark output in
	// `<div class="ii-w-markdown ii-w-repro">` (template chrome); goldmark
	// emits an `<ol>` with one `<li>` per numbered line.
	for _, want := range []string{
		`<div class="ii-w-markdown ii-w-repro">`,
		`<ol`,
		`Open the upload dialog`,
		`Pick a 50 MB PDF`,
		`Click Upload — observe the spinner hang`,
	} {
		if !strings.Contains(string(goHTML), want) {
			t.Errorf("Go renderer missing %q\n--- Go ---\n%s", want, goHTML)
		}
		if !strings.Contains(string(htmxHTML), want) {
			t.Errorf("htmx renderer missing %q\n--- htmx ---\n%s", want, htmxHTML)
		}
	}

	// Both renders must produce exactly three <li> entries.
	if got := strings.Count(string(goHTML), "<li>"); got != 3 {
		t.Errorf("Go: <li> count = %d, want 3", got)
	}
	if got := strings.Count(string(htmxHTML), "<li>"); got != 3 {
		t.Errorf("htmx: <li> count = %d, want 3", got)
	}

	// Documented parity gap: the Go renderer also accepts a `markdown`
	// payload field and renders it via goldmark. The htmx template has no
	// goldmark hook and the markdown field is silently ignored.
	mdPayload := `{"markdown":"1. Open the upload dialog\n2. Click **Upload**"}`
	goMD, err := renderReproductionSteps(mdPayload, wctx)
	if err != nil {
		t.Fatalf("Go renderer (markdown): %v", err)
	}
	htmxMD, err := htmxR(mdPayload, nil)
	if err != nil {
		t.Fatalf("htmx renderer (markdown): %v", err)
	}
	if !strings.Contains(string(goMD), "<strong>Upload</strong>") {
		t.Errorf("Go: expected <strong>Upload</strong> from goldmark:\n%s", goMD)
	}
	if strings.Contains(string(htmxMD), "<strong>") {
		t.Errorf("htmx: unexpected <strong> — markdown rendering is a parity gap:\n%s", htmxMD)
	}
	// And the empty-state copy fires when neither steps nor a recognized
	// markdown shape is present in the htmx path.
	if !strings.Contains(string(htmxMD), "No reproduction steps recorded.") {
		t.Errorf("htmx: expected empty-state copy when only markdown is supplied:\n%s", htmxMD)
	}
}

// reporterInfoHTMXTemplate is the template_source the reporter-info-htmx
// widget-type bead carries. It mirrors templates/partials/widgets_phase4.html
// `widget_reporter_info` so a side-by-side render matches the
// definition-list chrome (ii-w-reporter wrapper, field-list dl, dt/dd pairs
// for name/email/org/role).
//
// One intentional gap vs. the Go renderer (ka-0000004065 port doc):
//   - the Copy-email action button (built from CopyToClipboardAction)
//     is not constructed; the helper lives server-side. Renderer parity
//     stops at the dl chrome.
const reporterInfoHTMXTemplate = `<div class="ii-w-reporter">
{{- $name := index .Payload "name" -}}
{{- $email := index .Payload "email" -}}
{{- $org := index .Payload "org" -}}
{{- $role := index .Payload "role" -}}
  <dl class="field-list">
    {{if $name}}<dt>Name</dt><dd>{{$name}}</dd>{{end}}
    {{if $email}}<dt>Email</dt><dd><code>{{$email}}</code></dd>{{end}}
    {{if $org}}<dt>Org</dt><dd>{{$org}}</dd>{{end}}
    {{if $role}}<dt>Role</dt><dd>{{$role}}</dd>{{end}}
  </dl>
</div>`

// TestCompileHtmxRenderer_ReporterInfo exercises the reporter-info-htmx
// happy path: full payload renders the dl with all four dt/dd pairs.
func TestCompileHtmxRenderer_ReporterInfo(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-hw55lkk0pp", "reporter-info-htmx",
		reporterInfoHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compileHtmxRenderer: %v", err)
	}
	payload := `{
		"name":"Perry Tate",
		"email":"paulo@example.com",
		"org":"Acme",
		"role":"Site coordinator"
	}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("renderer error: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		`<div class="ii-w-reporter">`,
		`<dl class="field-list">`,
		`<dt>Name</dt><dd>Perry Tate</dd>`,
		`<dt>Email</dt><dd><code>paulo@example.com</code></dd>`,
		`<dt>Org</dt><dd>Acme</dd>`,
		`<dt>Role</dt><dd>Site coordinator</dd>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// TestCompileHtmxRenderer_ReporterInfoEmpty falls into the
// minimum-payload arm where every optional field is missing — empty
// payload renders the dl shell with zero dt/dd pairs.
func TestCompileHtmxRenderer_ReporterInfoEmpty(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-u1kz6okvag", "reporter-info-htmx",
		reporterInfoHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, `<div class="ii-w-reporter">`) {
		t.Errorf("expected reporter shell, got: %s", got)
	}
	if !strings.Contains(got, `<dl class="field-list">`) {
		t.Errorf("expected field-list dl, got: %s", got)
	}
	for _, banned := range []string{
		`<dt>Name`, `<dt>Email`, `<dt>Org`, `<dt>Role`, `<code>`,
	} {
		if strings.Contains(got, banned) {
			t.Errorf("unexpected %q in empty render:\n%s", banned, got)
		}
	}
}

// TestReporterInfoParity is the side-by-side fidelity check for the
// reporter-info-htmx port — fourth port after activity-log-htmx
// (ka-0000004584, MR !2334), related-tickets-htmx (ka-0000005540, MR !2346),
// and header-incident-summary-htmx (ka-0000005304, MR !2347).
// Same payload through the Go renderer and the htmx widget-type produces
// structurally-equivalent dl chrome: ii-w-reporter wrapper, field-list dl,
// dt/dd pairs with the email wrapped in <code>.
//
// Parity gap (intentional, port doc): the Go renderer emits a
// `<button class="ii-action-btn">Copy email</button>` built from
// CopyToClipboardAction; the htmx template body has no access to that
// helper, so it omits the button. This assertion locks the gap in place.
func TestReporterInfoParity(t *testing.T) {
	resetHtmxRendererCache()
	payload := `{
		"name":"Perry Tate <ops>",
		"email":"paulo@example.com",
		"org":"Acme",
		"role":"Site coordinator"
	}`

	wctx := newTestWidgetCtx(t)
	goHTML, err := renderReporterInfo(payload, wctx)
	if err != nil {
		t.Fatalf("Go renderer: %v", err)
	}

	htmxBead := makeHtmxWidgetTypeBead("gd-lbteeuqang", "reporter-info-htmx",
		reporterInfoHTMXTemplate, time.Now())
	htmxR, err := compileHtmxRenderer(htmxBead)
	if err != nil {
		t.Fatalf("htmx compile: %v", err)
	}
	htmxHTML, err := htmxR(payload, nil)
	if err != nil {
		t.Fatalf("htmx render: %v", err)
	}

	// Parity assertions: every dl-chrome marker the existing CSS targets
	// must appear in BOTH outputs.
	for _, want := range []string{
		`<div class="ii-w-reporter">`,
		`<dl class="field-list">`,
		// html/template auto-escapes `<ops>` identically in both paths.
		`<dt>Name</dt><dd>Perry Tate &lt;ops&gt;</dd>`,
		`<code>paulo@example.com</code>`,
		`<dt>Org</dt><dd>Acme</dd>`,
		`<dt>Role</dt><dd>Site coordinator</dd>`,
	} {
		if !strings.Contains(string(goHTML), want) {
			t.Errorf("Go renderer missing %q\n--- Go ---\n%s", want, goHTML)
		}
		if !strings.Contains(string(htmxHTML), want) {
			t.Errorf("htmx renderer missing %q\n--- htmx ---\n%s", want, htmxHTML)
		}
	}

	// Both renders must produce exactly four <dt> entries.
	if got := strings.Count(string(goHTML), "<dt>"); got != 4 {
		t.Errorf("Go: <dt count = %d, want 4", got)
	}
	if got := strings.Count(string(htmxHTML), "<dt>"); got != 4 {
		t.Errorf("htmx: <dt count = %d, want 4", got)
	}

	// Documented parity gap: the Go renderer emits a Copy-email action
	// button (one); the htmx template emits zero. This assertion locks the
	// gap in place — change with eyes-open if a future sprig fn or Go-helper
	// makes button construction template-side feasible.
	if got := strings.Count(string(goHTML), `class="ii-action-btn"`); got != 1 {
		t.Errorf("Go: ii-action-btn count = %d, want 1 (Copy email)", got)
	}
	if got := strings.Count(string(htmxHTML), `class="ii-action-btn"`); got != 0 {
		t.Errorf("htmx: ii-action-btn count = %d, want 0 (button is a parity gap)", got)
	}
}

// eventTimelineHTMXTemplate is the template_source the event-timeline-htmx
// widget-type bead carries. It mirrors templates/partials/widgets_phase7.html
// `widget_event_timeline` so a side-by-side render matches the static <ol>
// row chrome (data-source-kind, data-ii-crossfilter-keys/source-kind,
// <time datetime>, ii-w-event-timeline-* classes).
//
// Three intentional parity gaps vs. the Go renderer (ka-0000000243 port doc):
//   - uPlot canvas (`data-uplot-config`) is omitted — that JSON config is
//     produced server-side via MarshalTimelineConfig and has no template-
//     scope analogue. The htmx body emits the static <ol> fallback only.
//   - The lanes header is rendered only when the payload supplies a
//     pre-computed `lanes[]` array. The Go path derives lanes from sorted
//     events server-side; doing the same in a template would require sort
//     and group-by funcs that aren't in scope.
//   - Source-kind sanitization: the Go path collapses unknown kinds to
//     "code" via sanitizeSourceKind. The htmx template emits the kind
//     verbatim — producers should pre-sanitize to one of the eight Phase-1
//     source kinds.
const eventTimelineHTMXTemplate = `<div class="ii-w-event-timeline"{{with index .Payload "group_id"}} data-ii-ts-group="{{.}}"{{end}}>
{{- $title := index .Payload "title" -}}
{{- $subtitle := index .Payload "subtitle" -}}
{{- $badge := index .Payload "cross_filter_badge" -}}
{{- $events := index .Payload "events" -}}
{{- $lanes := index .Payload "lanes" -}}
{{- $groupID := index .Payload "group_id" -}}
  <header class="ii-w-event-timeline-head">
    {{if $title}}<h4 class="ii-w-event-timeline-title">{{$title}}</h4>{{end}}
    {{if $subtitle}}<p class="ii-w-event-timeline-subtitle meta">{{$subtitle}}</p>{{end}}
    {{if $badge}}<span class="ii-w-crossfilter-badge" title="Cross-filter is informational only — D4 v1">cross-filter: {{$badge}}</span>{{end}}
  </header>
  {{if $events}}
    {{if $lanes}}
    <div class="ii-w-event-timeline-lanes" data-ii-crossfilter-group="event-timeline-{{$groupID}}">
      {{range $lanes}}
        <span class="ii-source" data-source-kind="{{.kind}}"><span class="ii-source-icon" aria-hidden="true"></span>{{.label}}{{if .count}}<span class="ii-w-event-timeline-count">{{.count}}</span>{{end}}</span>
      {{end}}
    </div>
    {{end}}
    <ol class="ii-w-event-timeline-list">
      {{range $events}}
        <li class="ii-w-event-timeline-event" data-source-kind="{{.kind}}" data-ii-crossfilter-keys="source-kind" data-ii-crossfilter-source-kind="{{.kind}}">
          {{if .when}}<time class="ii-relative-time" datetime="{{.when}}"></time>{{end}}
          {{if .kind}}<span class="ii-source ii-w-event-timeline-source" data-source-kind="{{.kind}}"><span class="ii-source-icon" aria-hidden="true"></span>{{.kind}}</span>{{end}}
          {{if .title}}<span class="ii-w-event-timeline-event-title">{{.title}}</span>{{end}}
          {{if .detail}}<span class="ii-w-event-timeline-event-detail meta">— {{.detail}}</span>{{end}}
          {{if .url}}<a class="ii-w-event-timeline-event-link" href="{{.url}}" target="_blank" rel="noopener">open</a>{{end}}
        </li>
      {{end}}
    </ol>
  {{else}}
    <p class="empty">No timeline events.</p>
  {{end}}
</div>`

// TestCompileHtmxRenderer_EventTimeline exercises the event-timeline-htmx
// happy path: a flat events[] payload renders rows with the ii-relative-time
// hook, source-kind data attributes, and the title/detail/url chrome the
// existing CSS targets.
func TestCompileHtmxRenderer_EventTimeline(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-kmx7gauci7", "event-timeline-htmx",
		eventTimelineHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compileHtmxRenderer: %v", err)
	}
	payload := `{
		"title":"Event timeline",
		"group_id":"ii-ts-test",
		"events":[
			{"when":"2026-05-06T11:50:00Z","kind":"gitlab","title":"deploy !2185","url":"https://gitlab.com"},
			{"when":"2026-05-06T11:52:00Z","kind":"mezmo","title":"401 spike","detail":"1204 events"},
			{"when":"2026-05-06T11:55:00Z","kind":"jira","title":"FSM-12345 created"}
		]
	}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("renderer error: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		`data-ii-ts-group="ii-ts-test"`,
		`<h4 class="ii-w-event-timeline-title">Event timeline</h4>`,
		`<ol class="ii-w-event-timeline-list">`,
		`data-source-kind="gitlab"`,
		`data-source-kind="mezmo"`,
		`data-source-kind="jira"`,
		`data-ii-crossfilter-keys="source-kind"`,
		`data-ii-crossfilter-source-kind="gitlab"`,
		`<time class="ii-relative-time" datetime="2026-05-06T11:50:00Z"></time>`,
		`deploy !2185`,
		`401 spike`,
		`FSM-12345 created`,
		`— 1204 events`,
		`href="https://gitlab.com"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// TestCompileHtmxRenderer_EventTimelineEmpty falls into the empty-state arm.
func TestCompileHtmxRenderer_EventTimelineEmpty(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-lxtpyhhokz", "event-timeline-htmx",
		eventTimelineHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{"title":"none"}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(html), "No timeline events.") {
		t.Errorf("expected empty-state copy, got: %s", string(html))
	}
}

// TestCompileHtmxRenderer_EventTimelineLanes verifies the lanes header is
// rendered when the payload supplies a pre-computed lanes[] array. This is
// the optional opt-in for producers who want chip header parity with the Go
// renderer (which derives lanes server-side from sorted events).
func TestCompileHtmxRenderer_EventTimelineLanes(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-5obo51ro6w", "event-timeline-htmx",
		eventTimelineHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	payload := `{
		"group_id":"ii-ts-lanes",
		"events":[{"when":"2026-05-06T11:50:00Z","kind":"mezmo","title":"a"}],
		"lanes":[
			{"kind":"mezmo","label":"Logs (Mezmo)","count":1,"index":0},
			{"kind":"jira","label":"Tickets","count":2,"index":1}
		]
	}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		`data-ii-crossfilter-group="event-timeline-ii-ts-lanes"`,
		`Logs (Mezmo)`,
		`Tickets`,
		`<span class="ii-w-event-timeline-count">1</span>`,
		`<span class="ii-w-event-timeline-count">2</span>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("lanes header missing %q\n--- got ---\n%s", want, got)
		}
	}
}

// TestEventTimelineParity is the side-by-side fidelity check for the
// event-timeline-htmx port — the third port after activity-log-htmx
// (ka-0000004584, MR !2334) and related-tickets-htmx (ka-0000005540,
// MR !2346). Same payload through the Go renderer and the htmx widget-type
// produces structurally-equivalent HTML row chrome: data-source-kind,
// data-ii-crossfilter-* attrs, <time datetime> for the relative-time
// enhancer, and the title/detail/url chrome.
//
// Documented parity gaps (locked in by the trailing assertions):
//   - data-uplot-config / canvas div: only the Go path emits the uPlot
//     config blob (MarshalTimelineConfig). The htmx body has no analogue;
//     the static <ol> fallback is the parity surface.
//   - data-lane on each <li>: only the Go path computes lane indices from
//     first-seen kind order. The htmx body omits data-lane.
//   - sanitizeSourceKind: the Go path forces unknown kinds to "code". The
//     htmx body emits kind verbatim — payloads here use pre-sanitized kinds
//     to match.
func TestEventTimelineParity(t *testing.T) {
	resetHtmxRendererCache()
	payload := `{
		"title":"Event timeline",
		"group_id":"ii-ts-parity",
		"events":[
			{"when":"2026-05-06T11:50:00Z","kind":"gitlab","title":"deploy !2185","url":"https://gitlab.com"},
			{"when":"2026-05-06T11:52:00Z","kind":"mezmo","title":"401 spike <bug>","detail":"1204 events"}
		]
	}`

	wctx := newTestWidgetCtx(t)
	goR, ok := lookupWidget("event-timeline")
	if !ok {
		t.Fatal("event-timeline builtin not registered")
	}
	goHTML, err := goR(payload, wctx)
	if err != nil {
		t.Fatalf("Go renderer: %v", err)
	}

	htmxBead := makeHtmxWidgetTypeBead("gd-blsb0uaqit", "event-timeline-htmx",
		eventTimelineHTMXTemplate, time.Now())
	htmxR, err := compileHtmxRenderer(htmxBead)
	if err != nil {
		t.Fatalf("htmx compile: %v", err)
	}
	htmxHTML, err := htmxR(payload, nil)
	if err != nil {
		t.Fatalf("htmx render: %v", err)
	}

	// Parity assertions: every row-chrome marker the existing CSS / JS
	// targets must appear in BOTH outputs.
	for _, want := range []string{
		`data-ii-ts-group="ii-ts-parity"`,
		`<h4 class="ii-w-event-timeline-title">Event timeline</h4>`,
		`<ol class="ii-w-event-timeline-list">`,
		`data-source-kind="gitlab"`,
		`data-source-kind="mezmo"`,
		`data-ii-crossfilter-keys="source-kind"`,
		`data-ii-crossfilter-source-kind="gitlab"`,
		`data-ii-crossfilter-source-kind="mezmo"`,
		`datetime="2026-05-06T11:50:00Z"`,
		`datetime="2026-05-06T11:52:00Z"`,
		`deploy !2185`,
		// html/template auto-escapes `<bug>` identically in both paths.
		`401 spike &lt;bug&gt;`,
		`1204 events`,
		`href="https://gitlab.com"`,
	} {
		if !strings.Contains(string(goHTML), want) {
			t.Errorf("Go renderer missing %q\n--- Go ---\n%s", want, goHTML)
		}
		if !strings.Contains(string(htmxHTML), want) {
			t.Errorf("htmx renderer missing %q\n--- htmx ---\n%s", want, htmxHTML)
		}
	}

	// Both renders must produce exactly two <li> rows.
	if got := strings.Count(string(goHTML), "<li "); got != 2 {
		t.Errorf("Go: <li count = %d, want 2", got)
	}
	if got := strings.Count(string(htmxHTML), "<li "); got != 2 {
		t.Errorf("htmx: <li count = %d, want 2", got)
	}

	// Documented parity gap #1: only the Go path emits the uPlot canvas div
	// + data-uplot-config blob. MarshalTimelineConfig is server-side and has
	// no template-scope analogue — change with eyes-open if a future port
	// surfaces the helper as a template func.
	if !strings.Contains(string(goHTML), `data-uplot-kind="timeline"`) {
		t.Errorf("Go: expected data-uplot-kind=timeline, missing")
	}
	if strings.Contains(string(htmxHTML), `data-uplot-kind=`) {
		t.Errorf("htmx: unexpected data-uplot-kind (canvas is a parity gap)")
	}

	// Documented parity gap #2: only the Go path emits data-lane on each
	// <li>. Lane indices come from first-seen kind order computed
	// server-side; the htmx body omits the attribute.
	if got := strings.Count(string(goHTML), `data-lane="`); got < 2 {
		t.Errorf("Go: expected ≥2 data-lane attrs, got %d", got)
	}
	if got := strings.Count(string(htmxHTML), `data-lane="`); got != 0 {
		t.Errorf("htmx: data-lane count = %d, want 0 (lane is a parity gap)", got)
	}
}

// aiRecommendedActionsHTMXTemplate is the template_source the
// ai-recommended-actions-htmx widget-type bead carries. It mirrors
// templates/partials/widgets_phase4.html `widget_ai_recommended_actions`
// so a side-by-side render matches the actions-list chrome
// (.ii-w-actions-list container, <ol class="ii-w-actions-rows">, per-row
// .ii-w-action-row with data-confidence, the .ii-w-action-head row, and the
// optional .ii-w-action-detail paragraph).
//
// Two intentional gaps vs. the Go renderer (ka-0000000272 port doc):
//
//  1. Confidence normalisation. The Go path runs payload.confidence through
//     confidenceBand() — numbers >=0.8 → "high", >=0.5 → "medium", else
//     "low"; strings get trimmed/lower-cased and validated against
//     {high, medium, low}. The htmx template passes the field through
//     verbatim, so producers cutting an htmx widget-instance MUST emit
//     confidence as a pre-banded lower-case string ("high"/"medium"/"low").
//     A numeric score would render verbatim and violate the D2 rule that
//     numbers must not surface.
//  2. Action buttons. The Go renderer constructs `<button class="ii-action-btn">`
//     elements via OpenMRAction / OpenJiraAction / OpenMezmoAction
//     descriptors keyed off open_mr / open_jira / open_mezmo. The htmx
//     template body has no Go-side hook for button construction, so the
//     button-construction fields are silently ignored.
const aiRecommendedActionsHTMXTemplate = `<div class="ii-w-actions-list">
{{- $actions := index .Payload "actions" -}}
{{if $actions}}
  <ol class="ii-w-actions-rows">
    {{range $actions}}
      <li class="ii-w-action-row"{{if .confidence}} data-confidence="{{.confidence}}"{{end}}>
        <div class="ii-w-action-head">
          {{if .hotkey}}<kbd class="ii-key">{{.hotkey}}</kbd>{{end}}
          <span class="ii-w-action-headline">{{.headline}}</span>
          {{if .confidence}}<span class="ii-pill" data-confidence="{{.confidence}}">{{.confidence}} confidence</span>{{end}}
        </div>
        {{if .detail}}<p class="ii-w-action-detail">{{.detail}}</p>{{end}}
      </li>
    {{end}}
  </ol>
{{else}}
  <p class="empty">No recommended actions.</p>
{{end}}
</div>`

// TestCompileHtmxRenderer_AIRecommendedActions exercises the
// ai-recommended-actions-htmx happy path: actions[] payload renders the
// chrome with the data-confidence attribute the existing CSS targets.
func TestCompileHtmxRenderer_AIRecommendedActions(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-kmx7gauci7", "ai-recommended-actions-htmx",
		aiRecommendedActionsHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compileHtmxRenderer: %v", err)
	}
	payload := `{"actions":[
		{"headline":"Roll back !2185","detail":"Probable cause is the auth refactor","confidence":"high","hotkey":"r"},
		{"headline":"Check Mezmo errors","confidence":"medium"}
	]}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("renderer error: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		`<div class="ii-w-actions-list">`,
		`<ol class="ii-w-actions-rows">`,
		`data-confidence="high"`,
		`data-confidence="medium"`,
		`<kbd class="ii-key">r</kbd>`,
		`<span class="ii-w-action-headline">Roll back !2185</span>`,
		`high confidence`,
		`medium confidence`,
		`<p class="ii-w-action-detail">Probable cause is the auth refactor</p>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q\n--- got ---\n%s", want, got)
		}
	}
	if c := strings.Count(got, `class="ii-w-action-row"`); c != 2 {
		t.Errorf("ii-w-action-row count = %d, want 2", c)
	}
}

// TestCompileHtmxRenderer_AIRecommendedActionsEmpty falls into the empty-
// state arm — no actions[] renders the placeholder copy and no <ol>.
func TestCompileHtmxRenderer_AIRecommendedActionsEmpty(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-lxtpyhhokz", "ai-recommended-actions-htmx",
		aiRecommendedActionsHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{"actions":[]}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, `<div class="ii-w-actions-list">`) {
		t.Errorf("expected chrome shell, got: %s", got)
	}
	if !strings.Contains(got, "No recommended actions.") {
		t.Errorf("expected empty-state copy, got: %s", got)
	}
	if strings.Contains(got, "<ol") {
		t.Errorf("unexpected <ol> in empty render:\n%s", got)
	}
}

// TestAIRecommendedActionsParity is the side-by-side fidelity check for
// the ai-recommended-actions-htmx port — fifth port after activity-log-htmx
// (ka-0000004584, MR !2334), related-tickets-htmx (ka-0000005540,
// MR !2346), header-incident-summary-htmx (ka-0000005304, MR !2347), and
// reproduction-steps-htmx (ka-0000007835, MR !2350).
//
// Same actions[] payload through the Go renderer and the htmx widget-type
// produces structurally-equivalent action-list chrome: the
// .ii-w-actions-list container, the numbered <ol class="ii-w-actions-rows">,
// one .ii-w-action-row per action with data-confidence, the .ii-w-action-head
// row, and the .ii-w-action-detail paragraph.
//
// Parity gaps (intentional, port doc):
//
//  1. Confidence normalisation. The Go path normalises numeric scores via
//     confidenceBand (>=0.8 high, >=0.5 medium, otherwise low) and lower-
//     cases string bands. The htmx template renders the payload verbatim,
//     so the parity payload is pre-normalised to lowercase band strings.
//     This mirrors the TestRelatedTicketsParity / TestHeaderIncidentSummaryParity
//     pattern of pre-normalising the payload server-side for parity.
//  2. Action buttons. The Go renderer emits one ii-action-btn per action that
//     carries open_mr / open_jira / open_mezmo; the htmx template emits zero.
//     This assertion locks the gap — change with eyes-open if a future
//     sprig fn or Go-helper makes button construction template-side feasible.
func TestAIRecommendedActionsParity(t *testing.T) {
	resetHtmxRendererCache()

	// Go-renderer payload exercises confidenceBand by feeding numeric and
	// upper-case string confidences. The htmx payload is pre-normalised to
	// the bands the Go path produces.
	goPayload := `{
		"actions":[
			{"headline":"Roll back !2185","detail":"Probable cause is the auth refactor","confidence":0.9,"hotkey":"r","open_mr":"https://gitlab.com/x/y/-/merge_requests/2185","button_label":"Open MR"},
			{"headline":"Check Mezmo errors","confidence":"MEDIUM","open_mezmo":"app:beads-viewer level:error","button_label":"Open logs"}
		]
	}`
	htmxPayload := `{
		"actions":[
			{"headline":"Roll back !2185","detail":"Probable cause is the auth refactor","confidence":"high","hotkey":"r","open_mr":"https://gitlab.com/x/y/-/merge_requests/2185","button_label":"Open MR"},
			{"headline":"Check Mezmo errors","confidence":"medium","open_mezmo":"app:beads-viewer level:error","button_label":"Open logs"}
		]
	}`

	wctx := newTestWidgetCtx(t)
	goHTML, err := renderAIRecommendedActions(goPayload, wctx)
	if err != nil {
		t.Fatalf("Go renderer: %v", err)
	}

	htmxBead := makeHtmxWidgetTypeBead("gd-5obo51ro6w", "ai-recommended-actions-htmx",
		aiRecommendedActionsHTMXTemplate, time.Now())
	htmxR, err := compileHtmxRenderer(htmxBead)
	if err != nil {
		t.Fatalf("htmx compile: %v", err)
	}
	htmxHTML, err := htmxR(htmxPayload, nil)
	if err != nil {
		t.Fatalf("htmx render: %v", err)
	}

	// Parity assertions: every chrome marker the existing CSS targets must
	// appear in BOTH outputs.
	for _, want := range []string{
		`<div class="ii-w-actions-list">`,
		`<ol class="ii-w-actions-rows">`,
		`class="ii-w-action-row"`,
		`data-confidence="high"`,
		`data-confidence="medium"`,
		`<div class="ii-w-action-head">`,
		`Roll back !2185`,
		`Check Mezmo errors`,
		`high confidence`,
		`medium confidence`,
		`<kbd class="ii-key">r</kbd>`,
		`<p class="ii-w-action-detail">Probable cause is the auth refactor</p>`,
	} {
		if !strings.Contains(string(goHTML), want) {
			t.Errorf("Go renderer missing %q\n--- Go ---\n%s", want, goHTML)
		}
		if !strings.Contains(string(htmxHTML), want) {
			t.Errorf("htmx renderer missing %q\n--- htmx ---\n%s", want, htmxHTML)
		}
	}

	// Both renders must produce exactly two action rows.
	if got := strings.Count(string(goHTML), `class="ii-w-action-row"`); got != 2 {
		t.Errorf("Go: ii-w-action-row count = %d, want 2", got)
	}
	if got := strings.Count(string(htmxHTML), `class="ii-w-action-row"`); got != 2 {
		t.Errorf("htmx: ii-w-action-row count = %d, want 2", got)
	}

	// D2: numeric confidence scores must NOT leak into either rendered output.
	// (Go normalises 0.9 → "high"; htmx requires the producer to pre-band the
	// value, which the parity payload does.)
	if strings.Contains(string(goHTML), "0.9") {
		t.Errorf("D2 violation: numeric confidence 0.9 leaked into Go output:\n%s", goHTML)
	}
	if strings.Contains(string(htmxHTML), "0.9") {
		t.Errorf("D2 violation: numeric confidence 0.9 leaked into htmx output:\n%s", htmxHTML)
	}

	// Documented parity gap: the Go renderer emits one ii-action-btn per
	// action that carried open_mr / open_jira / open_mezmo (two here); the
	// htmx template emits zero. This assertion locks the gap in place.
	if got := strings.Count(string(goHTML), `class="ii-action-btn"`); got != 2 {
		t.Errorf("Go: ii-action-btn count = %d, want 2 (open_mr + open_mezmo)", got)
	}
	if got := strings.Count(string(htmxHTML), `class="ii-action-btn"`); got != 0 {
		t.Errorf("htmx: ii-action-btn count = %d, want 0 (buttons are a parity gap)", got)
	}
}

// fixAvailablePendingBranchHTMXTemplate is the template_source the
// fix-available-pending-branch-htmx widget-type bead carries. It mirrors
// templates/partials/widgets_phase6.html `widget_fix_available_pending_branch`
// so a side-by-side render matches the diff-file / hunk / line chrome the
// existing Phase 6 CSS targets (ii-w-pending-* wrapper, ii-w-diff-* table,
// data-language, ii-w-diff-{add,del,ctx} row classes).
//
// Five intentional parity gaps vs. the Go renderer (ka-0000005506 port doc):
//   - The Go renderer parses a raw unified `diff` string via
//     parseUnifiedDiff and chroma-highlights every line. The htmx template
//     has no parser or syntax-highlighter in scope; it requires producers
//     to ship pre-structured `files: [{path, language, adds, dels, hunks:
//     [{header, context, lines: [{kind, old_num, new_num, text}]}]}]`.
//     Diff content renders as plain (auto-escaped) text — no chroma spans.
//   - The Go renderer renders `summary_markdown` via goldmark
//     (`renderMarkdown`). The htmx template has no markdown helper; the
//     `summary_markdown` field is dropped on the floor. Producers who
//     want a summary can emit pre-rendered text into the `subtitle` field.
//   - The Go renderer emits a `<button class="ii-action-btn">Fast-track
//     merge</button>` and an `Open MR` button via FastTrackMergeAction /
//     OpenMRAction. The htmx body has no access to those helpers, so it
//     omits both buttons. The `<footer class="ii-w-pending-actions">` is
//     present but empty.
//   - The swap-target div (`<div id="ii-merge-result-..." class=
//     "ii-w-pending-result">`) is omitted — it pairs with the merge button
//     above; with no button to drive the swap, the placeholder serves no
//     purpose and would just clutter the DOM.
//   - CI status sanitization: the Go path collapses unknown statuses to
//     `"pending"` via normaliseCIStatus, and computes a 7-char
//     CommitShort. The htmx template emits `ci_status` and `commit_short`
//     verbatim — producers should pre-normalize and pre-truncate.
const fixAvailablePendingBranchHTMXTemplate = `<div class="ii-w-pending-branch">
{{- $title := index .Payload "title" -}}
{{- $branch := index .Payload "branch" -}}
{{- $baseBranch := index .Payload "base_branch" -}}
{{- $commitSHA := index .Payload "commit_sha" -}}
{{- $commitShort := index .Payload "commit_short" -}}
{{- $author := index .Payload "author" -}}
{{- $ciStatus := index .Payload "ci_status" -}}
{{- $adds := index .Payload "adds" -}}
{{- $dels := index .Payload "dels" -}}
{{- $files := index .Payload "files" -}}
  <header class="ii-w-pending-head">
    {{if $title}}<h4 class="ii-w-pending-title">{{$title}}</h4>{{end}}
    <div class="ii-w-pending-meta">
      {{if $branch}}<code class="ii-w-pending-branch-name">{{$branch}}</code>{{end}}
      {{if $baseBranch}}<span class="ii-w-pending-base">→ {{$baseBranch}}</span>{{end}}
      {{if $commitShort}}<code class="ii-w-pending-sha" title="{{$commitSHA}}">{{$commitShort}}</code>{{end}}
      {{if $author}}<span class="ii-w-pending-author">by {{$author}}</span>{{end}}
    </div>
    <div class="ii-w-pending-status">
      {{if $ciStatus}}<span class="ii-pill" data-state="{{$ciStatus}}">CI {{$ciStatus}}</span>{{end}}
      {{if $files}}<span class="ii-w-pending-count">{{len $files}} file{{if ne (len $files) 1}}s{{end}}</span>{{end}}
      {{if or $adds $dels}}
        <span class="ii-w-pending-adds">+{{$adds}}</span>
        <span class="ii-w-pending-dels">−{{$dels}}</span>
      {{end}}
    </div>
  </header>
  {{if $files}}
    <ol class="ii-w-pending-files">
      {{range $files}}
        <li class="ii-w-diff-file" data-language="{{.language}}">
          <header class="ii-w-diff-file-head">
            <code class="ii-w-diff-path">{{.path}}</code>
            <span class="ii-w-diff-counts">
              <span class="ii-w-diff-add-count">+{{.adds}}</span>
              <span class="ii-w-diff-del-count">−{{.dels}}</span>
            </span>
          </header>
          {{range .hunks}}
            <div class="ii-w-diff-hunk">
              <div class="ii-w-diff-hunk-head">
                <code class="ii-w-diff-hunk-header">{{.header}}</code>
                {{if .context}}<span class="ii-w-diff-hunk-ctx">{{.context}}</span>{{end}}
              </div>
              <table class="ii-w-diff-table">
                <tbody>
                  {{range .lines}}
                    <tr class="ii-w-diff-line ii-w-diff-{{.kind}}">
                      <td class="ii-w-diff-gutter ii-w-diff-gutter-old">{{if .old_num}}{{.old_num}}{{end}}</td>
                      <td class="ii-w-diff-gutter ii-w-diff-gutter-new">{{if .new_num}}{{.new_num}}{{end}}</td>
                      <td class="ii-w-diff-marker">{{if eq .kind "add"}}+{{else if eq .kind "del"}}−{{else}}&nbsp;{{end}}</td>
                      <td class="ii-w-diff-code">{{.text}}</td>
                    </tr>
                  {{end}}
                </tbody>
              </table>
            </div>
          {{end}}
        </li>
      {{end}}
    </ol>
  {{else}}
    <p class="empty">No diff available.</p>
  {{end}}
  <footer class="ii-w-pending-actions"></footer>
</div>`

// TestCompileHtmxRenderer_FixAvailablePendingBranch exercises the
// fix-available-pending-branch-htmx happy path: a structured files[] payload
// renders the wrapper, header chrome (branch / base / sha / author / CI
// pill), and per-file / per-hunk / per-line diff chrome with the data
// attributes the existing Phase 6 CSS targets.
func TestCompileHtmxRenderer_FixAvailablePendingBranch(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-5bvpl2fxu9", "fix-available-pending-branch-htmx",
		fixAvailablePendingBranchHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compileHtmxRenderer: %v", err)
	}
	payload := `{
		"title":"Fix available",
		"branch":"agent/fix",
		"base_branch":"main",
		"commit_sha":"9c2a3b7e1f4d8c0a5b6e9f2d4c1a3e5b7d9c0a2f",
		"commit_short":"9c2a3b7",
		"author":"gardenio",
		"ci_status":"passed",
		"adds":2,
		"dels":1,
		"files":[
			{
				"path":"foo.go",
				"language":"go",
				"adds":2,
				"dels":1,
				"hunks":[
					{
						"header":"@@ -1,3 +1,4 @@",
						"context":"context line",
						"lines":[
							{"kind":"ctx","old_num":1,"new_num":1,"text":" keep"},
							{"kind":"del","old_num":2,"new_num":0,"text":"-old"},
							{"kind":"add","old_num":0,"new_num":2,"text":"+new"},
							{"kind":"add","old_num":0,"new_num":3,"text":"+added"}
						]
					}
				]
			}
		]
	}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("renderer error: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		`<div class="ii-w-pending-branch">`,
		`<h4 class="ii-w-pending-title">Fix available</h4>`,
		`<code class="ii-w-pending-branch-name">agent/fix</code>`,
		`<span class="ii-w-pending-base">→ main</span>`,
		`<code class="ii-w-pending-sha" title="9c2a3b7e1f4d8c0a5b6e9f2d4c1a3e5b7d9c0a2f">9c2a3b7</code>`,
		`<span class="ii-w-pending-author">by gardenio</span>`,
		`<span class="ii-pill" data-state="passed">CI passed</span>`,
		`<span class="ii-w-pending-count">1 file</span>`,
		`<span class="ii-w-pending-adds">+2</span>`,
		`<span class="ii-w-pending-dels">−1</span>`,
		`<ol class="ii-w-pending-files">`,
		`<li class="ii-w-diff-file" data-language="go">`,
		`<code class="ii-w-diff-path">foo.go</code>`,
		`<table class="ii-w-diff-table">`,
		`<tr class="ii-w-diff-line ii-w-diff-ctx">`,
		`<tr class="ii-w-diff-line ii-w-diff-del">`,
		`<tr class="ii-w-diff-line ii-w-diff-add">`,
		// html/template auto-escapes `+` to `&#43;` in text contexts. The
		// Go renderer's template emits Header through the same escaper, so
		// both paths produce the encoded form — the parity test below
		// verifies that. Here we just look for the wrapping element.
		`<code class="ii-w-diff-hunk-header">`,
		`<span class="ii-w-diff-hunk-ctx">context line</span>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q\n--- got ---\n%s", want, got)
		}
	}
	// Singular vs. plural file count: 1 file payload above must NOT contain
	// the trailing "files".
	if strings.Contains(got, `1 files`) {
		t.Errorf("singular file count rendered with trailing s: %s", got)
	}
}

// TestCompileHtmxRenderer_FixAvailablePendingBranchEmpty falls into the
// empty-state arm — no files[] in the payload renders the "No diff
// available." copy and omits the count chrome.
func TestCompileHtmxRenderer_FixAvailablePendingBranchEmpty(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-ib2yrtze12", "fix-available-pending-branch-htmx",
		fixAvailablePendingBranchHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{"title":"Empty","ci_status":"running"}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, "No diff available.") {
		t.Errorf("expected empty-state copy, got: %s", got)
	}
	if !strings.Contains(got, `data-state="running"`) {
		t.Errorf("expected running pill, got: %s", got)
	}
	if strings.Contains(got, `<ol class="ii-w-pending-files">`) {
		t.Errorf("unexpected files list when payload has no files[]: %s", got)
	}
	if strings.Contains(got, `ii-w-pending-count`) {
		t.Errorf("unexpected count chrome with no files[]: %s", got)
	}
}

// TestCompileHtmxRenderer_FixAvailablePendingBranchPlural verifies the
// plural-file count path — multi-file payloads render "N files".
func TestCompileHtmxRenderer_FixAvailablePendingBranchPlural(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-1g9ya7zoeb", "fix-available-pending-branch-htmx",
		fixAvailablePendingBranchHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	payload := `{
		"files":[
			{"path":"a.go","language":"go","adds":1,"dels":0,"hunks":[]},
			{"path":"b.go","language":"go","adds":0,"dels":1,"hunks":[]}
		]
	}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, `<span class="ii-w-pending-count">2 files</span>`) {
		t.Errorf("expected plural count chrome, got: %s", got)
	}
}

// TestFixAvailablePendingBranchParity is the side-by-side fidelity check
// for the fix-available-pending-branch-htmx port — the sixth port after
// activity-log-htmx (ka-0000004584, MR !2334), related-tickets-htmx
// (ka-0000005540, MR !2346), header-incident-summary-htmx (ka-0000005304,
// MR !2347), reporter-info-htmx (ka-0000004065), and event-timeline-htmx
// (ka-0000000243).
//
// Unlike previous ports, this one uses TWO payloads — the Go renderer
// takes a raw unified `diff` string (parsed server-side) and the htmx
// template takes pre-structured `files[]`. Both payloads describe the
// same content, so the chrome assertions on the produced HTML hold for
// both paths. Where the chrome differs (chroma highlighting, Markdown
// summary, action buttons, swap-target div), the documented parity gap
// is locked in by trailing assertions.
func TestFixAvailablePendingBranchParity(t *testing.T) {
	resetHtmxRendererCache()
	goPayload := `{
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
	htmxPayload := `{
		"title":"Fix available",
		"branch":"agent/fix",
		"base_branch":"main",
		"commit_sha":"9c2a3b7e1f4d8c0a5b6e9f2d4c1a3e5b7d9c0a2f",
		"commit_short":"9c2a3b7",
		"author":"gardenio",
		"ci_status":"passed",
		"adds":1,
		"dels":1,
		"files":[
			{
				"path":"foo.go",
				"language":"go",
				"adds":1,
				"dels":1,
				"hunks":[
					{
						"header":"@@ -1,2 +1,2 @@",
						"context":"",
						"lines":[
							{"kind":"ctx","old_num":1,"new_num":1,"text":" keep"},
							{"kind":"del","old_num":2,"new_num":0,"text":"-old"},
							{"kind":"add","old_num":0,"new_num":2,"text":"+new"}
						]
					}
				]
			}
		]
	}`

	wctx := newTestWidgetCtx(t)
	goR, ok := lookupWidget("fix-available-pending-branch")
	if !ok {
		t.Fatal("fix-available-pending-branch builtin not registered")
	}
	goHTML, err := goR(goPayload, wctx)
	if err != nil {
		t.Fatalf("Go renderer: %v", err)
	}

	htmxBead := makeHtmxWidgetTypeBead("gd-oljerjrdk9", "fix-available-pending-branch-htmx",
		fixAvailablePendingBranchHTMXTemplate, time.Now())
	htmxR, err := compileHtmxRenderer(htmxBead)
	if err != nil {
		t.Fatalf("htmx compile: %v", err)
	}
	htmxHTML, err := htmxR(htmxPayload, nil)
	if err != nil {
		t.Fatalf("htmx render: %v", err)
	}

	// Parity assertions: every chrome marker the existing Phase 6 CSS / JS
	// targets must appear in BOTH outputs.
	for _, want := range []string{
		`<div class="ii-w-pending-branch">`,
		`<h4 class="ii-w-pending-title">Fix available</h4>`,
		`<code class="ii-w-pending-branch-name">agent/fix</code>`,
		`<span class="ii-w-pending-base">→ main</span>`,
		`<span class="ii-w-pending-author">by gardenio</span>`,
		`<span class="ii-pill" data-state="passed">CI passed</span>`,
		`<ol class="ii-w-pending-files">`,
		`<code class="ii-w-diff-path">foo.go</code>`,
		`<table class="ii-w-diff-table">`,
		`ii-w-diff-add`,
		`ii-w-diff-del`,
		`ii-w-diff-ctx`,
		// Both paths render the 7-char short SHA in the meta block.
		`9c2a3b7</code>`,
	} {
		if !strings.Contains(string(goHTML), want) {
			t.Errorf("Go renderer missing %q\n--- Go ---\n%s", want, goHTML)
		}
		if !strings.Contains(string(htmxHTML), want) {
			t.Errorf("htmx renderer missing %q\n--- htmx ---\n%s", want, htmxHTML)
		}
	}

	// Both renders must emit exactly one <li class="ii-w-diff-file"> entry.
	if got := strings.Count(string(goHTML), `class="ii-w-diff-file"`); got != 1 {
		t.Errorf("Go: ii-w-diff-file count = %d, want 1", got)
	}
	if got := strings.Count(string(htmxHTML), `class="ii-w-diff-file"`); got != 1 {
		t.Errorf("htmx: ii-w-diff-file count = %d, want 1", got)
	}

	// Documented parity gap #1: only the Go path emits the swap-target div
	// for fast-track-merge OOB swaps. The htmx body has no merge button to
	// drive the swap, so the placeholder is omitted.
	if !strings.Contains(string(goHTML), `id="ii-merge-result-2185-gd-incident-001"`) {
		t.Errorf("Go: expected ii-merge-result swap-target div, missing")
	}
	if strings.Contains(string(htmxHTML), `ii-w-pending-result`) {
		t.Errorf("htmx: unexpected ii-w-pending-result div (swap target is a parity gap)")
	}

	// Documented parity gap #2: only the Go path emits action buttons
	// (Fast-track merge + Open MR). The htmx body emits zero ii-action-btn.
	if got := strings.Count(string(goHTML), `class="ii-action-btn"`); got < 1 {
		t.Errorf("Go: expected ≥1 ii-action-btn, got %d", got)
	}
	if got := strings.Count(string(htmxHTML), `class="ii-action-btn"`); got != 0 {
		t.Errorf("htmx: ii-action-btn count = %d, want 0 (buttons are a parity gap)", got)
	}

	// Documented parity gap #3: only the Go path renders summary_markdown
	// (via goldmark). The htmx body drops the field entirely.
	if !strings.Contains(string(goHTML), `Stale-cookie fix`) {
		t.Errorf("Go: expected summary_markdown body, missing")
	}
	if strings.Contains(string(htmxHTML), `Stale-cookie fix`) {
		t.Errorf("htmx: unexpected summary_markdown body (markdown is a parity gap)")
	}

	// Documented parity gap #4: only the Go path emits chroma syntax
	// highlighting spans inside diff line code cells. The htmx body emits
	// the line text as plain auto-escaped text. We don't assert on the Go
	// side because chroma may emit different class shapes depending on
	// style — we just verify the htmx side is plain.
	if strings.Contains(string(htmxHTML), `<span class="line`) {
		t.Errorf("htmx: unexpected chroma <span class=\"line...\"> (highlighting is a parity gap)")
	}
}

// vmSparklinesHTMXTemplate is the template_source the vm-sparklines-htmx
// widget-type bead carries. It mirrors templates/partials/widgets_phase7.html
// `widget_vm_sparklines` so a side-by-side render matches the row chrome
// (data-state pills, ii-w-sparkline-* classes, latest/min/max readings line).
//
// Three intentional parity gaps vs. the Go renderer (ka-0000000715 port doc):
//   - uPlot canvas (`data-uplot-config`) is omitted — that JSON config is
//     produced server-side via MarshalSparkConfig and has no template-scope
//     analogue. The htmx body emits the static <ul> fallback only.
//   - latest/min/max derivation: the Go path computes these from
//     points[]/x[]/y[] inside renderVMSparklines. The htmx template requires
//     producers to ship them pre-computed; numbers are formatted via
//     {{printf "%.2f" ...}} for parity with the Go renderer chrome.
//   - State sanitization: the Go path lowercases the state via
//     strings.ToLower; the htmx template emits state verbatim. Producers
//     should pre-lower to one of healthy / warn / critical.
const vmSparklinesHTMXTemplate = `<div class="ii-w-sparklines"{{with index .Payload "group_id"}} data-ii-ts-group="{{.}}"{{end}}>
{{- $title := index .Payload "title" -}}
{{- $subtitle := index .Payload "subtitle" -}}
{{- $badge := index .Payload "cross_filter_badge" -}}
{{- $series := index .Payload "series" -}}
  <header class="ii-w-sparklines-head">
    {{if $title}}<h4 class="ii-w-sparklines-title">{{$title}}</h4>{{end}}
    {{if $subtitle}}<p class="ii-w-sparklines-subtitle meta">{{$subtitle}}</p>{{end}}
    {{if $badge}}<span class="ii-w-crossfilter-badge" title="Cross-filter is informational only — D4 v1">cross-filter: {{$badge}}</span>{{end}}
  </header>
  {{if $series}}
    <ul class="ii-w-sparklines-rows">
      {{range $series}}
        <li class="ii-w-sparkline-row">
          <div class="ii-w-sparkline-meta">
            {{if .service}}<span class="ii-w-sparkline-service">{{.service}}</span>{{end}}
            {{if .metric}}<span class="ii-w-sparkline-metric">{{.metric}}</span>{{end}}
            {{if .state}}<span class="ii-pill" data-state="{{.state}}">{{.state}}</span>{{end}}
          </div>
          {{if .latest}}<div class="ii-w-sparkline-readings">
            <span class="ii-w-sparkline-latest">latest <strong>{{printf "%.2f" .latest}}</strong>{{if .unit}} {{.unit}}{{end}}</span>
            {{if and .min .max}}<span class="ii-w-sparkline-range meta">min {{printf "%.2f" .min}} · max {{printf "%.2f" .max}}</span>{{end}}
          </div>{{end}}
        </li>
      {{end}}
    </ul>
  {{else}}
    <p class="empty">No metric series available.</p>
  {{end}}
</div>`

// TestCompileHtmxRenderer_VMSparklines exercises the vm-sparklines-htmx
// happy path: a flat series[] payload with pre-computed latest/min/max
// renders rows with the data-state pill, service/metric chrome, and the
// formatted readings line the existing CSS targets.
func TestCompileHtmxRenderer_VMSparklines(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-5bvpl2fxu9", "vm-sparklines-htmx",
		vmSparklinesHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compileHtmxRenderer: %v", err)
	}
	payload := `{
		"title":"VM sparklines",
		"group_id":"ii-ts-test",
		"cross_filter_badge":"informational",
		"series":[
			{"service":"study-web","metric":"p95","unit":"ms","state":"warn","latest":315.0,"min":220.0,"max":315.0},
			{"service":"sponsor-api","metric":"p95","unit":"ms","state":"healthy","latest":85.0,"min":80.0,"max":85.0}
		]
	}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("renderer error: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		`data-ii-ts-group="ii-ts-test"`,
		`<h4 class="ii-w-sparklines-title">VM sparklines</h4>`,
		`<ul class="ii-w-sparklines-rows">`,
		`ii-w-crossfilter-badge`,
		`cross-filter: informational`,
		`<span class="ii-w-sparkline-service">study-web</span>`,
		`<span class="ii-w-sparkline-service">sponsor-api</span>`,
		`<span class="ii-w-sparkline-metric">p95</span>`,
		`data-state="warn"`,
		`data-state="healthy"`,
		`<strong>315.00</strong>`,
		`<strong>85.00</strong>`,
		`min 220.00 · max 315.00`,
		`min 80.00 · max 85.00`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q\n--- got ---\n%s", want, got)
		}
	}
	if c := strings.Count(got, `<li class="ii-w-sparkline-row">`); c != 2 {
		t.Errorf("<li> count = %d, want 2", c)
	}
}

// TestCompileHtmxRenderer_VMSparklinesEmpty falls into the empty-state arm.
func TestCompileHtmxRenderer_VMSparklinesEmpty(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-ib2yrtze12", "vm-sparklines-htmx",
		vmSparklinesHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{"title":"empty"}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(html), "No metric series available.") {
		t.Errorf("expected empty-state copy, got: %s", string(html))
	}
}

// TestVMSparklinesParity is the side-by-side fidelity check for the
// vm-sparklines-htmx port — sixth port after activity-log-htmx
// (ka-0000004584, MR !2334), related-tickets-htmx (ka-0000005540, MR !2346),
// header-incident-summary-htmx (ka-0000005304, MR !2347),
// reproduction-steps-htmx (ka-0000007835), reporter-info-htmx
// (ka-0000004065), and event-timeline-htmx (ka-0000000243).
// Same payload through the Go renderer and the htmx widget-type produces
// structurally-equivalent row chrome: data-ii-ts-group on the wrapper,
// ii-w-sparklines-* classes, per-row data-state pill, ii-w-sparkline-service /
// metric chrome, and the formatted "latest <value> unit" + "min … · max …"
// readings strings.
//
// Documented parity gaps (locked in by the trailing assertions):
//   - data-uplot-config / canvas div: only the Go path emits the uPlot
//     config blob (MarshalSparkConfig). The htmx body has no analogue;
//     the static <ul> fallback is the parity surface.
//   - latest/min/max derivation: the Go path computes these from points[];
//     the htmx payload supplies them pre-computed alongside the points so
//     the same numeric chrome appears in both outputs.
//   - sanitizeSourceKind / strings.ToLower: the Go path lowercases state
//     server-side. The htmx body emits state verbatim — payloads here use
//     pre-lowered state strings to match.
func TestVMSparklinesParity(t *testing.T) {
	resetHtmxRendererCache()
	// Payload includes both points[] (so the Go renderer derives
	// latest/min/max) AND pre-computed latest/min/max (so the htmx template
	// can emit the readings line). The two computations produce identical
	// numbers for these inputs (315.00 latest, 220.00 min, 315.00 max for
	// study-web; 85.00 / 80.00 / 85.00 for sponsor-api).
	payload := `{
		"title":"VM sparklines",
		"group_id":"ii-ts-parity",
		"cross_filter_badge":"informational",
		"series":[
			{"service":"study-web","metric":"p95","unit":"ms","state":"warn",
			 "latest":315.0,"min":220.0,"max":315.0,
			 "points":[
				{"when":"2026-05-06T12:00:00Z","value":220.0},
				{"when":"2026-05-06T12:01:00Z","value":260.0},
				{"when":"2026-05-06T12:02:00Z","value":315.0}
			 ]},
			{"service":"sponsor-api","metric":"p95","unit":"ms","state":"healthy",
			 "latest":85.0,"min":80.0,"max":85.0,
			 "points":[
				{"when":"2026-05-06T12:00:00Z","value":80.0},
				{"when":"2026-05-06T12:01:00Z","value":85.0}
			 ]}
		]
	}`

	wctx := newTestWidgetCtx(t)
	goR, ok := lookupWidget("vm-sparklines")
	if !ok {
		t.Fatal("vm-sparklines builtin not registered")
	}
	goHTML, err := goR(payload, wctx)
	if err != nil {
		t.Fatalf("Go renderer: %v", err)
	}

	htmxBead := makeHtmxWidgetTypeBead("gd-1g9ya7zoeb", "vm-sparklines-htmx",
		vmSparklinesHTMXTemplate, time.Now())
	htmxR, err := compileHtmxRenderer(htmxBead)
	if err != nil {
		t.Fatalf("htmx compile: %v", err)
	}
	htmxHTML, err := htmxR(payload, nil)
	if err != nil {
		t.Fatalf("htmx render: %v", err)
	}

	// Parity assertions: every row-chrome marker the existing CSS targets
	// must appear in BOTH outputs.
	for _, want := range []string{
		`data-ii-ts-group="ii-ts-parity"`,
		`<h4 class="ii-w-sparklines-title">VM sparklines</h4>`,
		`<ul class="ii-w-sparklines-rows">`,
		`ii-w-crossfilter-badge`,
		`<span class="ii-w-sparkline-service">study-web</span>`,
		`<span class="ii-w-sparkline-service">sponsor-api</span>`,
		`<span class="ii-w-sparkline-metric">p95</span>`,
		`data-state="warn"`,
		`data-state="healthy"`,
		`<strong>315.00</strong>`,
		`<strong>85.00</strong>`,
		`min 220.00 · max 315.00`,
		`min 80.00 · max 85.00`,
	} {
		if !strings.Contains(string(goHTML), want) {
			t.Errorf("Go renderer missing %q\n--- Go ---\n%s", want, goHTML)
		}
		if !strings.Contains(string(htmxHTML), want) {
			t.Errorf("htmx renderer missing %q\n--- htmx ---\n%s", want, htmxHTML)
		}
	}

	// Both renders must produce exactly two <li> rows.
	if got := strings.Count(string(goHTML), "<li "); got != 2 {
		t.Errorf("Go: <li count = %d, want 2", got)
	}
	if got := strings.Count(string(htmxHTML), "<li "); got != 2 {
		t.Errorf("htmx: <li count = %d, want 2", got)
	}

	// Documented parity gap #1: only the Go path emits the uPlot canvas
	// + data-uplot-config blob. MarshalSparkConfig is server-side and has
	// no template-scope analogue — change with eyes-open if a future port
	// surfaces the helper as a template func.
	if !strings.Contains(string(goHTML), `data-uplot-kind="sparkline"`) {
		t.Errorf("Go: expected data-uplot-kind=sparkline, missing")
	}
	if strings.Contains(string(htmxHTML), `data-uplot-kind=`) {
		t.Errorf("htmx: unexpected data-uplot-kind (canvas is a parity gap)")
	}
	if !strings.Contains(string(goHTML), `data-uplot-config=`) {
		t.Errorf("Go: expected data-uplot-config blob, missing")
	}
	if strings.Contains(string(htmxHTML), `data-uplot-config=`) {
		t.Errorf("htmx: unexpected data-uplot-config (canvas is a parity gap)")
	}
}

// sourceCodeAnalysisHTMXTemplate is the template_source the
// source-code-analysis-htmx widget-type bead carries. It mirrors
// templates/partials/widgets_phase5.html `widget_source_code_analysis` so a
// side-by-side render matches the findings-list chrome (.ii-w-sca container,
// .ii-w-sca-title h4, <ol class="ii-w-sca-list">, per-row .ii-w-sca-finding
// with the .ii-w-sca-head row, the severity ii-pill with data-state, the
// .ii-w-sca-path code element, .ii-w-sca-fn in <code>, .ii-w-sca-headline,
// .ii-w-sca-detail, and the .ii-w-sca-code wrapper).
//
// First Phase-5 port — source-code-analysis is a code-highlighted widget
// (widgets_phase5.go) so the parity gap surface is wider than the Phase-4
// ports. Three intentional gaps vs. the Go renderer (ka-0000006619 port doc):
//
//  1. Chroma syntax highlighting. The Go path runs each finding's `code`
//     field through the shared codeHighlighter (chroma, github-dark, server-
//     side, cached). The htmx template body has no Go-side hook into chroma,
//     so the rendered <pre> carries plain escaped text rather than chroma's
//     token spans. The .ii-w-sca-code data-language wrapper is preserved.
//  2. Severity normalisation. The Go path lower-cases the severity string
//     before emitting `data-state="…"`. The htmx template renders the field
//     verbatim, so producers MUST emit severity as a pre-lower-cased string
//     for the Phase-1 state-pill CSS to match.
//  3. Jump-to-IDE buttons. The Go renderer constructs
//     `<button class="ii-action-btn">` elements via OpenIDEAction for every
//     finding with a `path`. The htmx template body has no Go-side hook for
//     button construction, so the optional jump-to-line action is silently
//     ignored. Same gap as ai-recommended-actions-htmx /
//     header-incident-summary-htmx.
const sourceCodeAnalysisHTMXTemplate = `<div class="ii-w-sca">
{{- $title := index .Payload "title" -}}
{{- $findings := index .Payload "findings" -}}
{{if $title}}<h4 class="ii-w-sca-title">{{$title}}</h4>{{end}}
{{if $findings}}
  <ol class="ii-w-sca-list">
    {{range $findings}}
      <li class="ii-w-sca-finding">
        <header class="ii-w-sca-head">
          {{if .severity}}<span class="ii-pill" data-state="{{.severity}}">{{.severity}}</span>{{end}}
          <code class="ii-w-sca-path">{{.path}}{{if .line}}:{{.line}}{{end}}</code>
          {{if .function}}<span class="ii-w-sca-fn">in <code>{{.function}}()</code></span>{{end}}
        </header>
        {{if .headline}}<p class="ii-w-sca-headline">{{.headline}}</p>{{end}}
        {{if .detail}}<p class="ii-w-sca-detail">{{.detail}}</p>{{end}}
        {{if .code}}<div class="ii-w-sca-code" data-language="{{.language}}"><pre>{{.code}}</pre></div>{{end}}
      </li>
    {{end}}
  </ol>
{{else}}
  <p class="empty">No source-code findings.</p>
{{end}}
</div>`

// TestCompileHtmxRenderer_SourceCodeAnalysis exercises the
// source-code-analysis-htmx happy path: a findings[] payload renders the
// chrome with the data-state severity attribute, .ii-w-sca-path code element,
// optional function span, and the .ii-w-sca-code wrapper that the existing
// CSS targets.
func TestCompileHtmxRenderer_SourceCodeAnalysis(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-5bvpl2fxu9", "source-code-analysis-htmx",
		sourceCodeAnalysisHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compileHtmxRenderer: %v", err)
	}
	payload := `{
		"title":"Top hot lines",
		"findings":[
			{"path":"controller/main.go","line":42,"function":"run","severity":"error",
			 "headline":"nil deref on missing config","detail":"Hit in 3/3 traces",
			 "language":"go","code":"if cfg.X == nil { panic(0) }"},
			{"path":"controller/auth.go","severity":"warn","headline":"Unbounded retry loop",
			 "language":"go","code":"for { try() }"}
		]
	}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("renderer error: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		`<div class="ii-w-sca">`,
		`<h4 class="ii-w-sca-title">Top hot lines</h4>`,
		`<ol class="ii-w-sca-list">`,
		`<li class="ii-w-sca-finding">`,
		`<header class="ii-w-sca-head">`,
		`data-state="error"`,
		`data-state="warn"`,
		`<code class="ii-w-sca-path">controller/main.go:42</code>`,
		`<code class="ii-w-sca-path">controller/auth.go</code>`,
		`<span class="ii-w-sca-fn">in <code>run()</code></span>`,
		`<p class="ii-w-sca-headline">nil deref on missing config</p>`,
		`<p class="ii-w-sca-detail">Hit in 3/3 traces</p>`,
		`<div class="ii-w-sca-code" data-language="go">`,
		`<pre>if cfg.X == nil { panic(0) }</pre>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q\n--- got ---\n%s", want, got)
		}
	}
	if c := strings.Count(got, `class="ii-w-sca-finding"`); c != 2 {
		t.Errorf("ii-w-sca-finding count = %d, want 2", c)
	}
	// The second finding has no `line` field — must not surface a stray ":".
	if strings.Contains(got, `controller/auth.go:0`) {
		t.Errorf("zero-line should be omitted, got: %s", got)
	}
}

// TestCompileHtmxRenderer_SourceCodeAnalysisEmpty falls into the empty-state
// arm — no findings[] renders the placeholder copy and no <ol>.
func TestCompileHtmxRenderer_SourceCodeAnalysisEmpty(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-ib2yrtze12", "source-code-analysis-htmx",
		sourceCodeAnalysisHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, `<div class="ii-w-sca">`) {
		t.Errorf("expected chrome shell, got: %s", got)
	}
	if !strings.Contains(got, "No source-code findings.") {
		t.Errorf("expected empty-state copy, got: %s", got)
	}
	if strings.Contains(got, "<ol") {
		t.Errorf("unexpected <ol> in empty render:\n%s", got)
	}
}

// TestSourceCodeAnalysisParity is the side-by-side fidelity check for the
// source-code-analysis-htmx port — first Phase-5 port (code-highlighted
// widget surface) after activity-log-htmx (ka-0000004584, MR !2334),
// related-tickets-htmx (ka-0000005540, MR !2346), header-incident-summary-
// htmx (ka-0000005304, MR !2347), reproduction-steps-htmx (ka-0000007835,
// MR !2350), reporter-info-htmx (ka-0000004065), event-timeline-htmx
// (ka-0000000243), ai-recommended-actions-htmx (ka-0000000272),
// fix-available-pending-branch-htmx (ka-0000005506), and vm-sparklines-htmx
// (ka-0000000715).
//
// Same findings[] payload through the Go renderer and the htmx widget-type
// produces structurally-equivalent findings-list chrome: the .ii-w-sca
// container, optional .ii-w-sca-title, <ol class="ii-w-sca-list">, one
// .ii-w-sca-finding per finding with the .ii-w-sca-head row, severity pill,
// .ii-w-sca-path code element, optional .ii-w-sca-fn span, .ii-w-sca-headline,
// .ii-w-sca-detail, and the .ii-w-sca-code wrapper.
//
// Parity gaps (intentional, port doc):
//
//  1. Chroma syntax highlighting. The Go path emits chroma token <span>s
//     inside .ii-w-sca-code; the htmx path emits a plain escaped <pre>.
//     The wrapper chrome (.ii-w-sca-code data-language=…) is preserved on
//     both paths.
//  2. Severity normalisation. The Go path lower-cases severity before
//     emitting `data-state`; the parity payload pre-lower-cases for the
//     htmx side, mirroring the TestRelatedTicketsParity /
//     TestHeaderIncidentSummaryParity pattern.
//  3. Jump-to-IDE buttons. The Go renderer emits one ii-action-btn per
//     finding with a path; the htmx template emits zero. This assertion
//     locks the gap — change with eyes-open if a future template-side
//     button-builder fn lands.
func TestSourceCodeAnalysisParity(t *testing.T) {
	resetHtmxRendererCache()

	// Go-renderer payload exercises severity normalisation by feeding an
	// upper-case "ERROR". The htmx payload pre-lower-cases.
	goPayload := `{
		"title":"Top hot lines",
		"findings":[
			{"path":"controller/main.go","line":42,"function":"run","severity":"ERROR",
			 "headline":"nil deref on missing config","detail":"Hit in 3/3 traces",
			 "language":"go","code":"if cfg.X == nil { panic(0) }"},
			{"path":"controller/auth.go","line":17,"severity":"warn",
			 "headline":"Unbounded retry loop","language":"go","code":"for { try() }"}
		]
	}`
	htmxPayload := `{
		"title":"Top hot lines",
		"findings":[
			{"path":"controller/main.go","line":42,"function":"run","severity":"error",
			 "headline":"nil deref on missing config","detail":"Hit in 3/3 traces",
			 "language":"go","code":"if cfg.X == nil { panic(0) }"},
			{"path":"controller/auth.go","line":17,"severity":"warn",
			 "headline":"Unbounded retry loop","language":"go","code":"for { try() }"}
		]
	}`

	wctx := newTestWidgetCtxP5(t)
	goHTML, err := renderSourceCodeAnalysis(goPayload, wctx)
	if err != nil {
		t.Fatalf("Go renderer: %v", err)
	}

	htmxBead := makeHtmxWidgetTypeBead("gd-1g9ya7zoeb", "source-code-analysis-htmx",
		sourceCodeAnalysisHTMXTemplate, time.Now())
	htmxR, err := compileHtmxRenderer(htmxBead)
	if err != nil {
		t.Fatalf("htmx compile: %v", err)
	}
	htmxHTML, err := htmxR(htmxPayload, nil)
	if err != nil {
		t.Fatalf("htmx render: %v", err)
	}

	// Parity assertions: every chrome marker the existing CSS targets must
	// appear in BOTH outputs.
	for _, want := range []string{
		`<div class="ii-w-sca">`,
		`<h4 class="ii-w-sca-title">Top hot lines</h4>`,
		`<ol class="ii-w-sca-list">`,
		`class="ii-w-sca-finding"`,
		`<header class="ii-w-sca-head">`,
		`data-state="error"`,
		`data-state="warn"`,
		`controller/main.go:42`,
		`controller/auth.go:17`,
		`<code>run()</code>`,
		`nil deref on missing config`,
		`Hit in 3/3 traces`,
		`Unbounded retry loop`,
		`data-language="go"`,
	} {
		if !strings.Contains(string(goHTML), want) {
			t.Errorf("Go renderer missing %q\n--- Go ---\n%s", want, goHTML)
		}
		if !strings.Contains(string(htmxHTML), want) {
			t.Errorf("htmx renderer missing %q\n--- htmx ---\n%s", want, htmxHTML)
		}
	}

	// Both renders must produce exactly two finding rows.
	if got := strings.Count(string(goHTML), `class="ii-w-sca-finding"`); got != 2 {
		t.Errorf("Go: ii-w-sca-finding count = %d, want 2", got)
	}
	if got := strings.Count(string(htmxHTML), `class="ii-w-sca-finding"`); got != 2 {
		t.Errorf("htmx: ii-w-sca-finding count = %d, want 2", got)
	}

	// Documented parity gap #1: chroma syntax highlighting. The Go path emits
	// chroma <span style="color:…"> token spans; the htmx path emits plain
	// escaped <pre>. Lock the gap so a regression that adds chroma here is
	// noisy.
	if !strings.Contains(string(goHTML), `<span style="color:`) {
		t.Errorf("Go: expected chroma token spans, missing")
	}
	if strings.Contains(string(htmxHTML), `<span style="color:`) {
		t.Errorf("htmx: unexpected chroma token spans (chroma is a parity gap)")
	}

	// Documented parity gap #3: jump-to-IDE buttons. The Go renderer emits
	// one ii-action-btn per finding that carried a `path` (two here); the
	// htmx template emits zero. This assertion locks the gap in place.
	if got := strings.Count(string(goHTML), `class="ii-action-btn"`); got != 2 {
		t.Errorf("Go: ii-action-btn count = %d, want 2", got)
	}
	if got := strings.Count(string(htmxHTML), `class="ii-action-btn"`); got != 0 {
		t.Errorf("htmx: ii-action-btn count = %d, want 0 (buttons are a parity gap)", got)
	}
}

// affectedFilesHTMXTemplate is the template_source the affected-files-htmx
// widget-type bead carries. It mirrors templates/partials/widgets_phase4.html
// `widget_affected_files` so a side-by-side render matches the file-list
// chrome the existing CSS targets (ii-w-files wrapper, ii-w-files-list ul,
// per-row ii-w-file-row li with code path, optional reason).
//
// One intentional gap vs. the Go renderer (ka-0000005289 port doc):
//   - the Open-in-IDE action button (built from OpenIDEAction) is not
//     constructed; the helper lives server-side. Renderer parity stops at
//     the file-list chrome.
//
// Note on .line: Go templates treat 0 as falsy, so {{if .line}} naturally
// suppresses the `:line` suffix when a producer ships line=0 or omits the
// field entirely. JSON numbers arrive as float64; html/template formats
// integer-valued floats without a decimal point (e.g. 442 not 442.0).
const affectedFilesHTMXTemplate = `<div class="ii-w-files">
{{- $files := index .Payload "files" -}}
{{if $files}}
  <ul class="ii-w-files-list">
    {{range $files}}
      <li class="ii-w-file-row">
        <code class="ii-w-file-path">{{.path}}{{if .line}}:{{.line}}{{end}}</code>
        {{if .reason}}<span class="ii-w-file-reason">{{.reason}}</span>{{end}}
      </li>
    {{end}}
  </ul>
{{else}}
  <p class="empty">No affected files.</p>
{{end}}
</div>`

// TestCompileHtmxRenderer_AffectedFiles exercises the affected-files-htmx
// happy path: files[] payload renders the chrome the existing CSS targets.
func TestCompileHtmxRenderer_AffectedFiles(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-5bvpl2fxu9", "affected-files-htmx",
		affectedFilesHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compileHtmxRenderer: %v", err)
	}
	payload := `{"files":[
		{"path":"gardenio/controller/cmd/beads-viewer/server.go","line":442,"reason":"new auth middleware introduced"},
		{"path":"gardenio/controller/internal/bridge/decisions.go","line":118,"reason":"call site"}
	]}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("renderer error: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		`<div class="ii-w-files">`,
		`<ul class="ii-w-files-list">`,
		`<li class="ii-w-file-row">`,
		`<code class="ii-w-file-path">gardenio/controller/cmd/beads-viewer/server.go:442</code>`,
		`<code class="ii-w-file-path">gardenio/controller/internal/bridge/decisions.go:118</code>`,
		`<span class="ii-w-file-reason">new auth middleware introduced</span>`,
		`<span class="ii-w-file-reason">call site</span>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q\n--- got ---\n%s", want, got)
		}
	}
	if c := strings.Count(got, `class="ii-w-file-row"`); c != 2 {
		t.Errorf("ii-w-file-row count = %d, want 2", c)
	}
}

// TestCompileHtmxRenderer_AffectedFilesEmpty falls into the empty-state
// arm — empty files[] renders the placeholder copy and no <ul>.
func TestCompileHtmxRenderer_AffectedFilesEmpty(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-ib2yrtze12", "affected-files-htmx",
		affectedFilesHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{"files":[]}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, `<div class="ii-w-files">`) {
		t.Errorf("expected chrome shell, got: %s", got)
	}
	if !strings.Contains(got, "No affected files.") {
		t.Errorf("expected empty-state copy, got: %s", got)
	}
	if strings.Contains(got, "<ul") {
		t.Errorf("unexpected <ul> in empty render:\n%s", got)
	}
}

// TestCompileHtmxRenderer_AffectedFilesLineless verifies that rows where
// line is missing or zero suppress the `:line` suffix on the <code>
// path — mirrors the Go renderer's `{{if gt .Line 0}}:{{.Line}}{{end}}`
// behaviour through Go-template-truthy semantics on float64.
func TestCompileHtmxRenderer_AffectedFilesLineless(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-1g9ya7zoeb", "affected-files-htmx",
		affectedFilesHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	payload := `{"files":[
		{"path":"README.md","reason":"docs only"},
		{"path":"main.go","line":0,"reason":"unused"}
	]}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, `<code class="ii-w-file-path">README.md</code>`) {
		t.Errorf("expected unsuffixed path for missing line, got: %s", got)
	}
	if !strings.Contains(got, `<code class="ii-w-file-path">main.go</code>`) {
		t.Errorf("expected unsuffixed path for line=0, got: %s", got)
	}
	if strings.Contains(got, ":0</code>") {
		t.Errorf("unexpected ':0' suffix on lineless row:\n%s", got)
	}
}

// TestAffectedFilesParity is the side-by-side fidelity check for the
// affected-files-htmx port — seventh port after activity-log-htmx
// (ka-0000004584, MR !2334), related-tickets-htmx (ka-0000005540, MR !2346),
// header-incident-summary-htmx (ka-0000005304, MR !2347),
// reproduction-steps-htmx (ka-0000007835, MR !2350), reporter-info-htmx
// (ka-0000004065), ai-recommended-actions-htmx (ka-0000000272),
// event-timeline-htmx (ka-0000000243), fix-available-pending-branch-htmx
// (ka-0000005506), and vm-sparklines-htmx (ka-0000000715).
//
// Same files[] payload through the Go renderer and the htmx widget-type
// produces structurally-equivalent file-list chrome: the .ii-w-files
// container, the <ul class="ii-w-files-list"> shell, and one
// .ii-w-file-row li per file with the path wrapped in <code> and
// (when present) the reason wrapped in <span class="ii-w-file-reason">.
//
// Parity gap (intentional, port doc): the Go renderer emits a
// `<button class="ii-action-btn">Open in IDE</button>` per file built
// from OpenIDEAction(path, line); the htmx template body has no access
// to that helper, so it omits the button. This assertion locks the gap
// in place — change with eyes-open if a future sprig fn or Go-helper
// makes button construction template-side feasible.
func TestAffectedFilesParity(t *testing.T) {
	resetHtmxRendererCache()
	payload := `{"files":[
		{"path":"gardenio/controller/cmd/beads-viewer/server.go","line":442,"reason":"new auth middleware <introduced>"},
		{"path":"gardenio/controller/internal/bridge/decisions.go","line":118,"reason":"call site"}
	]}`

	wctx := newTestWidgetCtx(t)
	goHTML, err := renderAffectedFiles(payload, wctx)
	if err != nil {
		t.Fatalf("Go renderer: %v", err)
	}

	htmxBead := makeHtmxWidgetTypeBead("gd-oljerjrdk9", "affected-files-htmx",
		affectedFilesHTMXTemplate, time.Now())
	htmxR, err := compileHtmxRenderer(htmxBead)
	if err != nil {
		t.Fatalf("htmx compile: %v", err)
	}
	htmxHTML, err := htmxR(payload, nil)
	if err != nil {
		t.Fatalf("htmx render: %v", err)
	}

	// Parity assertions: every chrome marker the existing CSS targets
	// must appear in BOTH outputs.
	for _, want := range []string{
		`<div class="ii-w-files">`,
		`<ul class="ii-w-files-list">`,
		`class="ii-w-file-row"`,
		`<code class="ii-w-file-path">gardenio/controller/cmd/beads-viewer/server.go:442</code>`,
		`<code class="ii-w-file-path">gardenio/controller/internal/bridge/decisions.go:118</code>`,
		// html/template auto-escapes `<introduced>` identically in both paths.
		`new auth middleware &lt;introduced&gt;`,
		`<span class="ii-w-file-reason">call site</span>`,
	} {
		if !strings.Contains(string(goHTML), want) {
			t.Errorf("Go renderer missing %q\n--- Go ---\n%s", want, goHTML)
		}
		if !strings.Contains(string(htmxHTML), want) {
			t.Errorf("htmx renderer missing %q\n--- htmx ---\n%s", want, htmxHTML)
		}
	}

	// Both renders must produce exactly two file rows.
	if got := strings.Count(string(goHTML), `class="ii-w-file-row"`); got != 2 {
		t.Errorf("Go: ii-w-file-row count = %d, want 2", got)
	}
	if got := strings.Count(string(htmxHTML), `class="ii-w-file-row"`); got != 2 {
		t.Errorf("htmx: ii-w-file-row count = %d, want 2", got)
	}

	// Documented parity gap: the Go renderer emits one ii-action-btn per
	// file (Open in IDE — two here); the htmx template emits zero. This
	// assertion locks the gap in place — change with eyes-open if a future
	// sprig fn or Go-helper makes button construction template-side feasible.
	if got := strings.Count(string(goHTML), `class="ii-action-btn"`); got != 2 {
		t.Errorf("Go: ii-action-btn count = %d, want 2 (Open in IDE × 2)", got)
	}
	if got := strings.Count(string(htmxHTML), `class="ii-action-btn"`); got != 0 {
		t.Errorf("htmx: ii-action-btn count = %d, want 0 (button is a parity gap)", got)
	}
}

// rcaEvidenceCardsHTMXTemplate is the template_source the
// rca-evidence-cards-htmx widget-type bead carries. It mirrors
// templates/partials/widgets_phase5.html `widget_rca_evidence_cards` so a
// side-by-side render matches the cards-list chrome (.ii-w-rca container,
// <ol class="ii-w-rca-list">, per-card .ii-w-rca-card with optional
// data-confidence, the .ii-w-rca-card-head row carrying the .ii-source chip
// (data-source-kind / .ii-source-label), the .ii-w-rca-headline h4, the
// .ii-pill confidence chip, the .ii-w-rca-detail paragraph, and the
// .ii-w-rca-code wrapper. The .ii-w-rca-actions div is preserved as the
// CSS hook for action buttons even when the htmx body emits zero buttons).
//
// Tenth pre-defined widget on the free-htmx substrate, second Phase-5 port
// (after source-code-analysis-htmx ka-0000006619). Five intentional gaps
// vs. the Go renderer (ka-0000004029 port doc):
//
//  1. Chroma syntax highlighting. The Go path runs each card's `code`
//     field through the shared codeHighlighter (chroma, github-dark, server-
//     side, cached). The htmx template body has no Go-side hook into chroma,
//     so the rendered <pre> carries plain escaped text rather than chroma's
//     token spans. The .ii-w-rca-code data-language wrapper is preserved.
//     Same gap as source-code-analysis-htmx.
//  2. Confidence normalisation. The Go path runs payload.confidence through
//     confidenceBand() — numbers >=0.8 → "high", >=0.5 → "medium", else
//     "low"; strings get trimmed/lower-cased and validated against
//     {high, medium, low}. The htmx template renders the field verbatim,
//     so producers MUST pre-band confidence to a lower-case string.
//     Numeric scores would render verbatim and violate D2 (numbers must
//     not surface). Same gap as ai-recommended-actions-htmx.
//  3. Source-kind normalisation. The Go path lower-cases payload.source_kind
//     before emitting `data-source-kind="…"`. The htmx template renders
//     the field verbatim, so producers MUST emit source_kind as a
//     pre-lower-cased string for the Phase-1 source-chip CSS palette to
//     match.
//  4. Action buttons. The Go renderer constructs
//     `<button class="ii-action-btn">` elements via OpenMezmoAction /
//     OpenMRAction / OpenIDEAction / OpenJiraAction descriptors keyed off
//     open_mezmo / open_mr / ide_path / open_jira. The htmx template body
//     has no Go-side hook for button construction, so the four button-
//     construction fields are silently ignored. The .ii-w-rca-actions
//     wrapper is present but empty. Same gap as ai-recommended-actions-htmx
//     / header-incident-summary-htmx / source-code-analysis-htmx.
//  5. Drill-down drawer. The Go renderer pre-renders payload.drilldown_markdown
//     via goldmark and emits a `<button class="ii-w-rca-drill">` plus a
//     `<template>` carrying the rendered HTML. The htmx template has no
//     goldmark helper in scope and no template-side button construction,
//     so the drawer button + template pair is omitted entirely. Producers
//     who want a drawer should keep using the rca-evidence-cards Go slug.
const rcaEvidenceCardsHTMXTemplate = `<div class="ii-w-rca">
{{- $cards := index .Payload "cards" -}}
{{if $cards}}
  <ol class="ii-w-rca-list">
    {{range $cards}}
      <li class="ii-w-rca-card" {{if .confidence}}data-confidence="{{.confidence}}"{{end}}>
        <header class="ii-w-rca-card-head">
          {{if .source_kind}}
            <span class="ii-source" data-source-kind="{{.source_kind}}">
              <span class="ii-source-icon" aria-hidden="true"></span>
              <span class="ii-source-label">{{if .source_label}}{{.source_label}}{{else}}{{.source_kind}}{{end}}</span>
            </span>
          {{end}}
          {{if .headline}}<h4 class="ii-w-rca-headline">{{.headline}}</h4>{{end}}
          {{if .confidence}}<span class="ii-pill" data-confidence="{{.confidence}}">{{.confidence}} confidence</span>{{end}}
        </header>
        {{if .detail}}<p class="ii-w-rca-detail">{{.detail}}</p>{{end}}
        {{if .code}}<div class="ii-w-rca-code" data-language="{{.language}}"><pre>{{.code}}</pre></div>{{end}}
        <div class="ii-w-rca-actions"></div>
      </li>
    {{end}}
  </ol>
{{else}}
  <p class="empty">No RCA evidence collected.</p>
{{end}}
</div>`

// TestCompileHtmxRenderer_RCAEvidenceCards exercises the
// rca-evidence-cards-htmx happy path: a cards[] payload renders the chrome
// with the source chip, headline, confidence pill, detail paragraph, code
// wrapper, and the empty .ii-w-rca-actions hook the existing CSS targets.
func TestCompileHtmxRenderer_RCAEvidenceCards(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-oljerjrdk9", "rca-evidence-cards-htmx",
		rcaEvidenceCardsHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compileHtmxRenderer: %v", err)
	}
	payload := `{"cards":[
		{"id":"ev-1","headline":"Auth middleware drops sessions",
		 "detail":"401 spike correlates with !2185 deploy",
		 "source_kind":"mezmo","source_label":"mezmo: auth-svc",
		 "confidence":"high","language":"go",
		 "code":"func auth() { panic(0) }"},
		{"id":"ev-2","headline":"Retry storm in fetcher",
		 "source_kind":"victoria-metrics","confidence":"medium"}
	]}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("renderer error: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		`<div class="ii-w-rca">`,
		`<ol class="ii-w-rca-list">`,
		`<li class="ii-w-rca-card" data-confidence="high">`,
		`<li class="ii-w-rca-card" data-confidence="medium">`,
		`<header class="ii-w-rca-card-head">`,
		`<span class="ii-source" data-source-kind="mezmo">`,
		`<span class="ii-source" data-source-kind="victoria-metrics">`,
		`<span class="ii-source-label">mezmo: auth-svc</span>`,
		`<span class="ii-source-label">victoria-metrics</span>`,
		`<h4 class="ii-w-rca-headline">Auth middleware drops sessions</h4>`,
		`<h4 class="ii-w-rca-headline">Retry storm in fetcher</h4>`,
		`<span class="ii-pill" data-confidence="high">high confidence</span>`,
		`<span class="ii-pill" data-confidence="medium">medium confidence</span>`,
		`<p class="ii-w-rca-detail">401 spike correlates with !2185 deploy</p>`,
		`<div class="ii-w-rca-code" data-language="go">`,
		`<pre>func auth() { panic(0) }</pre>`,
		`<div class="ii-w-rca-actions"></div>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q\n--- got ---\n%s", want, got)
		}
	}
	if c := strings.Count(got, `class="ii-w-rca-card"`); c != 2 {
		t.Errorf("ii-w-rca-card count = %d, want 2", c)
	}
}

// TestCompileHtmxRenderer_RCAEvidenceCardsEmpty falls into the empty-state
// arm — no cards[] renders the placeholder copy and no <ol>.
func TestCompileHtmxRenderer_RCAEvidenceCardsEmpty(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-smmurvulii", "rca-evidence-cards-htmx",
		rcaEvidenceCardsHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, `<div class="ii-w-rca">`) {
		t.Errorf("expected chrome shell, got: %s", got)
	}
	if !strings.Contains(got, "No RCA evidence collected.") {
		t.Errorf("expected empty-state copy, got: %s", got)
	}
	if strings.Contains(got, "<ol") {
		t.Errorf("unexpected <ol> in empty render:\n%s", got)
	}
}

// TestRCAEvidenceCardsParity is the side-by-side fidelity check for the
// rca-evidence-cards-htmx port — second Phase-5 port (code-highlighted
// widget surface) after source-code-analysis-htmx (ka-0000006619). Mirrors
// the reference ports activity-log-htmx (ka-0000004584, MR !2334),
// related-tickets-htmx (ka-0000005540, MR !2346), header-incident-summary-
// htmx (ka-0000005304, MR !2347), reproduction-steps-htmx (ka-0000007835,
// MR !2350), reporter-info-htmx (ka-0000004065), event-timeline-htmx
// (ka-0000000243), ai-recommended-actions-htmx (ka-0000000272),
// fix-available-pending-branch-htmx (ka-0000005506), and vm-sparklines-htmx
// (ka-0000000715).
//
// Same cards[] payload through the Go renderer and the htmx widget-type
// produces structurally-equivalent chrome: the .ii-w-rca container,
// <ol class="ii-w-rca-list">, one .ii-w-rca-card per card with optional
// data-confidence, the .ii-w-rca-card-head row, the .ii-source chip, the
// .ii-w-rca-headline h4, the .ii-pill confidence chip, the .ii-w-rca-detail
// paragraph, and the .ii-w-rca-code wrapper.
//
// Parity gaps (intentional, port doc):
//
//  1. Chroma syntax highlighting. The Go path emits chroma token <span>s
//     inside .ii-w-rca-code; the htmx path emits a plain escaped <pre>.
//     The wrapper chrome (.ii-w-rca-code data-language=…) is preserved on
//     both paths.
//  2. Confidence normalisation. The Go path normalises numeric scores via
//     confidenceBand (>=0.8 high, >=0.5 medium, otherwise low); the parity
//     payload pre-bands the htmx side, mirroring the
//     TestAIRecommendedActionsParity pattern.
//  3. Source-kind normalisation. The Go path lower-cases source_kind; the
//     parity payload pre-lower-cases the htmx side.
//  4. Action buttons. The Go renderer emits one ii-action-btn per card with
//     an open_mezmo / open_mr / ide_path / open_jira field; the htmx
//     template emits zero. This assertion locks the gap — change with
//     eyes-open if a future template-side button-builder fn lands.
//  5. Drill-down drawer. The Go renderer emits a .ii-w-rca-drill button
//     plus a <template> with goldmark-rendered drilldown_markdown; the
//     htmx template emits zero of either. Locked here.
func TestRCAEvidenceCardsParity(t *testing.T) {
	resetHtmxRendererCache()

	// Go-renderer payload exercises confidenceBand (numeric 0.92 → "high")
	// and source-kind lowercasing ("MEZMO" → "mezmo"). The htmx payload
	// pre-bands and pre-lower-cases.
	goPayload := `{"cards":[
		{"id":"ev-1","headline":"Auth middleware drops sessions",
		 "detail":"401 spike correlates with !2185 deploy",
		 "source_kind":"MEZMO","source_label":"mezmo: auth-svc",
		 "confidence":0.92,"language":"go",
		 "code":"func auth() { panic(0) }",
		 "open_mezmo":"app:auth-svc level:error"},
		{"id":"ev-2","headline":"Retry storm in fetcher",
		 "source_kind":"victoria-metrics","confidence":"medium",
		 "open_mr":"https://gitlab.com/x/y/-/merge_requests/2185"}
	]}`
	htmxPayload := `{"cards":[
		{"id":"ev-1","headline":"Auth middleware drops sessions",
		 "detail":"401 spike correlates with !2185 deploy",
		 "source_kind":"mezmo","source_label":"mezmo: auth-svc",
		 "confidence":"high","language":"go",
		 "code":"func auth() { panic(0) }",
		 "open_mezmo":"app:auth-svc level:error"},
		{"id":"ev-2","headline":"Retry storm in fetcher",
		 "source_kind":"victoria-metrics","confidence":"medium",
		 "open_mr":"https://gitlab.com/x/y/-/merge_requests/2185"}
	]}`

	wctx := newTestWidgetCtxP5(t)
	goHTML, err := renderRCAEvidenceCards(goPayload, wctx)
	if err != nil {
		t.Fatalf("Go renderer: %v", err)
	}

	htmxBead := makeHtmxWidgetTypeBead("gd-efgditfylk", "rca-evidence-cards-htmx",
		rcaEvidenceCardsHTMXTemplate, time.Now())
	htmxR, err := compileHtmxRenderer(htmxBead)
	if err != nil {
		t.Fatalf("htmx compile: %v", err)
	}
	htmxHTML, err := htmxR(htmxPayload, nil)
	if err != nil {
		t.Fatalf("htmx render: %v", err)
	}

	// Parity assertions: every chrome marker the existing CSS targets must
	// appear in BOTH outputs.
	for _, want := range []string{
		`<div class="ii-w-rca">`,
		`<ol class="ii-w-rca-list">`,
		`class="ii-w-rca-card"`,
		`data-confidence="high"`,
		`data-confidence="medium"`,
		`data-source-kind="mezmo"`,
		`data-source-kind="victoria-metrics"`,
		`<header class="ii-w-rca-card-head">`,
		`Auth middleware drops sessions`,
		`Retry storm in fetcher`,
		`high confidence`,
		`medium confidence`,
		`401 spike correlates with !2185 deploy`,
		`data-language="go"`,
	} {
		if !strings.Contains(string(goHTML), want) {
			t.Errorf("Go renderer missing %q\n--- Go ---\n%s", want, goHTML)
		}
		if !strings.Contains(string(htmxHTML), want) {
			t.Errorf("htmx renderer missing %q\n--- htmx ---\n%s", want, htmxHTML)
		}
	}

	// Both renders must produce exactly two cards.
	if got := strings.Count(string(goHTML), `class="ii-w-rca-card"`); got != 2 {
		t.Errorf("Go: ii-w-rca-card count = %d, want 2", got)
	}
	if got := strings.Count(string(htmxHTML), `class="ii-w-rca-card"`); got != 2 {
		t.Errorf("htmx: ii-w-rca-card count = %d, want 2", got)
	}

	// D2: numeric confidence scores must NOT leak into either rendered output.
	// (Go normalises 0.92 → "high"; htmx requires the producer to pre-band
	// the value, which the parity payload does.)
	if strings.Contains(string(goHTML), "0.92") {
		t.Errorf("D2 violation: numeric confidence 0.92 leaked into Go output:\n%s", goHTML)
	}
	if strings.Contains(string(htmxHTML), "0.92") {
		t.Errorf("D2 violation: numeric confidence 0.92 leaked into htmx output:\n%s", htmxHTML)
	}

	// Documented parity gap #1: chroma syntax highlighting. The Go path emits
	// chroma <span style="color:…"> token spans; the htmx path emits plain
	// escaped <pre>. Lock the gap so a regression that adds chroma here is
	// noisy.
	if !strings.Contains(string(goHTML), `<span style="color:`) {
		t.Errorf("Go: expected chroma token spans, missing")
	}
	if strings.Contains(string(htmxHTML), `<span style="color:`) {
		t.Errorf("htmx: unexpected chroma token spans (chroma is a parity gap)")
	}

	// Documented parity gap #4: action buttons. The Go renderer emits one
	// ii-action-btn per card with an open_* / ide_path field (two here);
	// the htmx template emits zero. This assertion locks the gap in place.
	if got := strings.Count(string(goHTML), `class="ii-action-btn"`); got != 2 {
		t.Errorf("Go: ii-action-btn count = %d, want 2 (open_mezmo + open_mr)", got)
	}
	if got := strings.Count(string(htmxHTML), `class="ii-action-btn"`); got != 0 {
		t.Errorf("htmx: ii-action-btn count = %d, want 0 (buttons are a parity gap)", got)
	}

	// Documented parity gap #5: drill-down drawer is omitted on the htmx
	// path. The Go path doesn't emit ii-w-rca-drill here either (no
	// drilldown_markdown in the parity payload), so this assertion just
	// locks the htmx-side absence.
	if strings.Contains(string(htmxHTML), `ii-w-rca-drill`) {
		t.Errorf("htmx: unexpected ii-w-rca-drill (drawer is a parity gap)")
	}
}

// workaroundHTMXTemplate is the template_source the workaround-htmx
// widget-type bead carries. It mirrors templates/partials/widgets_phase4.html
// `widget_workaround` so a side-by-side render matches the markdown chrome
// (.ii-w-markdown .ii-w-workaround container, the optional headline
// paragraph, and the empty-state copy).
//
// One intentional gap vs. the Go renderer (ka-0000001237 port doc):
//   - the `markdown` field is server-side only. The Go path runs goldmark
//     against widgetRenderCtx.md to render markdown into HTML; html/template
//     has no goldmark hook, so the htmx template wraps the raw `markdown`
//     string in a single <p> with the value escaped verbatim. Same gap as
//     reproduction-steps-htmx.
const workaroundHTMXTemplate = `<div class="ii-w-markdown ii-w-workaround">
{{- $headline := index .Payload "headline" -}}
{{- $markdown := index .Payload "markdown" -}}
{{if $headline}}<p class="ii-w-workaround-headline"><strong>{{$headline}}</strong></p>{{end}}
{{if $markdown}}<p>{{$markdown}}</p>{{else}}<p class="empty">No workaround documented.</p>{{end}}
</div>`

// TestCompileHtmxRenderer_Workaround exercises the workaround-htmx happy
// path: headline + markdown payload renders the chrome with the headline
// paragraph and a body <p> carrying the escaped markdown text.
func TestCompileHtmxRenderer_Workaround(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-xock0yo1tx", "workaround-htmx",
		workaroundHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compileHtmxRenderer: %v", err)
	}
	payload := `{
		"headline":"Use the legacy upload path",
		"markdown":"Append ?legacy=1 to the upload URL."
	}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("renderer error: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		`<div class="ii-w-markdown ii-w-workaround">`,
		`<p class="ii-w-workaround-headline"><strong>Use the legacy upload path</strong></p>`,
		`Append ?legacy=1 to the upload URL.`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q\n--- got ---\n%s", want, got)
		}
	}
	if strings.Contains(got, "No workaround documented.") {
		t.Errorf("unexpected empty-state copy in populated render:\n%s", got)
	}
}

// TestCompileHtmxRenderer_WorkaroundEmpty falls into the empty-state arm —
// missing markdown renders the placeholder copy, no headline paragraph
// when headline is also missing.
func TestCompileHtmxRenderer_WorkaroundEmpty(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-hpjwvxyl2f", "workaround-htmx",
		workaroundHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, `<div class="ii-w-markdown ii-w-workaround">`) {
		t.Errorf("expected chrome shell, got: %s", got)
	}
	if !strings.Contains(got, "No workaround documented.") {
		t.Errorf("expected empty-state copy, got: %s", got)
	}
	if strings.Contains(got, "ii-w-workaround-headline") {
		t.Errorf("unexpected headline in empty render:\n%s", got)
	}
}

// TestCompileHtmxRenderer_WorkaroundHeadlineOnly covers the "headline but
// no markdown" arm — headline paragraph renders and the empty-state copy
// fires for the body.
func TestCompileHtmxRenderer_WorkaroundHeadlineOnly(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-nkng3ovbwb", "workaround-htmx",
		workaroundHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{"headline":"Use the legacy upload path"}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, `<strong>Use the legacy upload path</strong>`) {
		t.Errorf("expected headline, got: %s", got)
	}
	if !strings.Contains(got, "No workaround documented.") {
		t.Errorf("expected empty-state copy when markdown missing:\n%s", got)
	}
}

// TestWorkaroundParity is the side-by-side fidelity check for the
// workaround-htmx port — eleventh free-htmx port after activity-log
// (ka-0000004584, MR !2334), related-tickets (ka-0000005540, MR !2346),
// header-incident-summary (ka-0000005304, MR !2347), reproduction-steps
// (ka-0000007835), reporter-info (ka-0000004065), event-timeline
// (ka-0000000243), ai-recommended-actions (ka-0000000272),
// fix-available-pending-branch (ka-0000005506), vm-sparklines
// (ka-0000000715), and source-code-analysis (ka-0000006619). Same
// payload through both renderers must produce structurally-equivalent
// chrome: the .ii-w-markdown .ii-w-workaround container, the optional
// headline paragraph, and the empty-state copy when nothing is supplied.
//
// Parity gap (intentional, port doc): the Go renderer feeds `markdown`
// through goldmark, so producers can ship rich markup (bold, links,
// code, multi-paragraph). The htmx template has no goldmark hook and
// emits the raw `markdown` string verbatim inside a single <p>. The
// markdown rendering is therefore a Go-renderer-only affordance — see
// ka-0000001237 port doc for the two paths to close the gap if a
// future port needs richer markup.
func TestWorkaroundParity(t *testing.T) {
	resetHtmxRendererCache()
	payload := `{
		"headline":"Use the legacy upload path",
		"markdown":"Append ?legacy=1 to the upload URL."
	}`

	wctx := newTestWidgetCtx(t)
	goHTML, err := renderWorkaround(payload, wctx)
	if err != nil {
		t.Fatalf("Go renderer: %v", err)
	}

	htmxBead := makeHtmxWidgetTypeBead("gd-h73hluuzo5", "workaround-htmx",
		workaroundHTMXTemplate, time.Now())
	htmxR, err := compileHtmxRenderer(htmxBead)
	if err != nil {
		t.Fatalf("htmx compile: %v", err)
	}
	htmxHTML, err := htmxR(payload, nil)
	if err != nil {
		t.Fatalf("htmx render: %v", err)
	}

	// Parity assertions: every chrome marker the existing CSS targets must
	// appear in BOTH outputs.
	for _, want := range []string{
		`<div class="ii-w-markdown ii-w-workaround">`,
		`ii-w-workaround-headline`,
		`<strong>Use the legacy upload path</strong>`,
		`Append ?legacy=1 to the upload URL.`,
	} {
		if !strings.Contains(string(goHTML), want) {
			t.Errorf("Go renderer missing %q\n--- Go ---\n%s", want, goHTML)
		}
		if !strings.Contains(string(htmxHTML), want) {
			t.Errorf("htmx renderer missing %q\n--- htmx ---\n%s", want, htmxHTML)
		}
	}

	// Documented parity gap: the Go renderer runs `markdown` through
	// goldmark, so backtick-delimited code becomes <code>…</code>. The
	// htmx template has no goldmark hook and emits the raw text verbatim.
	mdPayload := `{"headline":"Use the legacy upload path","markdown":"Append ` + "`?legacy=1`" + ` to the upload URL."}`
	goMD, err := renderWorkaround(mdPayload, wctx)
	if err != nil {
		t.Fatalf("Go renderer (markdown): %v", err)
	}
	htmxMD, err := htmxR(mdPayload, nil)
	if err != nil {
		t.Fatalf("htmx renderer (markdown): %v", err)
	}
	if !strings.Contains(string(goMD), "<code>?legacy=1</code>") {
		t.Errorf("Go: expected <code>?legacy=1</code> from goldmark:\n%s", goMD)
	}
	if strings.Contains(string(htmxMD), "<code>?legacy=1</code>") {
		t.Errorf("htmx: unexpected <code> — markdown rendering is a parity gap:\n%s", htmxMD)
	}

	// Empty payload: both renders fall into the empty-state arm.
	goEmpty, err := renderWorkaround(`{}`, wctx)
	if err != nil {
		t.Fatalf("Go renderer (empty): %v", err)
	}
	htmxEmpty, err := htmxR(`{}`, nil)
	if err != nil {
		t.Fatalf("htmx renderer (empty): %v", err)
	}
	if !strings.Contains(string(goEmpty), "No workaround documented.") {
		t.Errorf("Go: expected empty-state copy:\n%s", goEmpty)
	}
	if !strings.Contains(string(htmxEmpty), "No workaround documented.") {
		t.Errorf("htmx: expected empty-state copy:\n%s", htmxEmpty)
	}
}

// serviceHealthHTMXTemplate is the template_source the service-health-htmx
// widget-type bead carries. It mirrors templates/partials/widgets_phase4.html
// `widget_service_health` so a side-by-side render matches the cross-filter
// table chrome (data-ii-crossfilter-group on the wrapper, data-ii-crossfilter-
// keys / data-ii-crossfilter-service on each row, data-state pill).
//
// One intentional gap vs. the Go renderer (ka-0000006096 port doc):
//   - state is emitted verbatim (the Go path ToLowers it server-side); real
//     instances should emit lowercase strings to match the [data-state] CSS
//     targets.
const serviceHealthHTMXTemplate = `<div class="ii-w-services" data-ii-crossfilter-group="service-health">
{{- $services := index .Payload "services" -}}
{{if $services}}
  <table class="ii-w-table">
    <thead><tr><th>Service</th><th>State</th><th>Detail</th></tr></thead>
    <tbody>
      {{range $services}}
        <tr data-ii-crossfilter-keys="service" data-ii-crossfilter-service="{{.service}}">
          <td><code>{{.service}}</code></td>
          <td>{{if .state}}<span class="ii-pill" data-state="{{.state}}">{{.state}}</span>{{else}}—{{end}}</td>
          <td>{{.detail}}</td>
        </tr>
      {{end}}
    </tbody>
  </table>
{{else}}
  <p class="empty">No service rows.</p>
{{end}}
</div>`

// TestCompileHtmxRenderer_ServiceHealth exercises the service-health-htmx
// happy path: a flat services[] payload renders rows with the data-ii-
// crossfilter-* attributes and the data-state pill the existing CSS targets.
func TestCompileHtmxRenderer_ServiceHealth(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-2nxyrqgnbs", "service-health-htmx",
		serviceHealthHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compileHtmxRenderer: %v", err)
	}
	payload := `{"services":[
		{"service":"study-web","state":"critical","detail":"5xx 12% (last 5m)"},
		{"service":"sponsor-api","state":"warn","detail":"latency p95 1.2s"},
		{"service":"site-api","state":"healthy","detail":"5xx 0%"}
	]}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("renderer error: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		`data-ii-crossfilter-group="service-health"`,
		`<table class="ii-w-table">`,
		`data-ii-crossfilter-keys="service"`,
		`data-ii-crossfilter-service="study-web"`,
		`data-ii-crossfilter-service="sponsor-api"`,
		`data-ii-crossfilter-service="site-api"`,
		`<code>study-web</code>`,
		`<code>sponsor-api</code>`,
		`<code>site-api</code>`,
		`data-state="critical"`,
		`data-state="warn"`,
		`data-state="healthy"`,
		`5xx 12% (last 5m)`,
		`latency p95 1.2s`,
		`5xx 0%`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q\n--- got ---\n%s", want, got)
		}
	}
	if c := strings.Count(got, "<tr "); c != 3 {
		t.Errorf("<tr count = %d, want 3", c)
	}
}

// TestCompileHtmxRenderer_ServiceHealthEmpty falls into the empty-state arm.
func TestCompileHtmxRenderer_ServiceHealthEmpty(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-ai0svfjcb3", "service-health-htmx",
		serviceHealthHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{"services":[]}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(html), "No service rows.") {
		t.Errorf("expected empty-state copy, got: %s", string(html))
	}
}

// TestServiceHealthParity is the side-by-side fidelity check for the
// service-health-htmx port — tenth port after activity-log-htmx
// (ka-0000004584, MR !2334), related-tickets-htmx (ka-0000005540, MR !2346),
// header-incident-summary-htmx (ka-0000005304, MR !2347),
// reproduction-steps-htmx (ka-0000007835), reporter-info-htmx
// (ka-0000004065), event-timeline-htmx (ka-0000000243),
// ai-recommended-actions-htmx (ka-0000000272),
// fix-available-pending-branch-htmx (ka-0000005506), and vm-sparklines-htmx
// (ka-0000000715). Same payload through the Go renderer and the htmx
// widget-type produces structurally-equivalent table chrome:
// data-ii-crossfilter-group on the wrapper, data-ii-crossfilter-keys /
// data-ii-crossfilter-service per row, <code> service name, data-state pill,
// and detail cell.
//
// Documented parity gap (locked in by the trailing assertions):
//   - strings.ToLower on state — the Go path lowercases the state
//     server-side. The htmx body emits state verbatim — the payload here
//     uses pre-lowered state strings to match (same approach as
//     TestRelatedTicketsParity / TestVMSparklinesParity).
func TestServiceHealthParity(t *testing.T) {
	resetHtmxRendererCache()
	// The Go renderer ToLowers state via strings.ToLower; the htmx template
	// emits state verbatim. Mirror the Go normalization in the htmx payload
	// (same approach as TestRelatedTicketsParity).
	payload := `{"services":[
		{"service":"study-web","state":"CRITICAL","detail":"5xx 12% <bug>"},
		{"service":"sponsor-api","state":"WARN","detail":"latency p95 1.2s"},
		{"service":"site-api","state":"HEALTHY","detail":"5xx 0%"}
	]}`

	wctx := newTestWidgetCtx(t)
	goHTML, err := renderServiceHealth(payload, wctx)
	if err != nil {
		t.Fatalf("Go renderer: %v", err)
	}

	htmxBead := makeHtmxWidgetTypeBead("gd-33pbgephbq", "service-health-htmx",
		serviceHealthHTMXTemplate, time.Now())
	htmxR, err := compileHtmxRenderer(htmxBead)
	if err != nil {
		t.Fatalf("htmx compile: %v", err)
	}
	htmxPayload := strings.NewReplacer(
		`"CRITICAL"`, `"critical"`,
		`"WARN"`, `"warn"`,
		`"HEALTHY"`, `"healthy"`,
	).Replace(payload)
	htmxHTML, err := htmxR(htmxPayload, nil)
	if err != nil {
		t.Fatalf("htmx render: %v", err)
	}

	// Parity assertions: every row-chrome marker the existing CSS targets
	// must appear in BOTH outputs.
	for _, want := range []string{
		`data-ii-crossfilter-group="service-health"`,
		`<table class="ii-w-table">`,
		`data-ii-crossfilter-keys="service"`,
		`data-ii-crossfilter-service="study-web"`,
		`data-ii-crossfilter-service="sponsor-api"`,
		`data-ii-crossfilter-service="site-api"`,
		`<code>study-web</code>`,
		`<code>sponsor-api</code>`,
		`<code>site-api</code>`,
		`data-state="critical"`,
		`data-state="warn"`,
		`data-state="healthy"`,
		// html/template auto-escapes `<bug>` identically in both paths.
		`5xx 12% &lt;bug&gt;`,
		`latency p95 1.2s`,
		`5xx 0%`,
	} {
		if !strings.Contains(string(goHTML), want) {
			t.Errorf("Go renderer missing %q\n--- Go ---\n%s", want, goHTML)
		}
		if !strings.Contains(string(htmxHTML), want) {
			t.Errorf("htmx renderer missing %q\n--- htmx ---\n%s", want, htmxHTML)
		}
	}

	// Both renders must produce exactly three <tr> rows in the body.
	if got := strings.Count(string(goHTML), `data-ii-crossfilter-keys="service"`); got != 3 {
		t.Errorf("Go: row count = %d, want 3", got)
	}
	if got := strings.Count(string(htmxHTML), `data-ii-crossfilter-keys="service"`); got != 3 {
		t.Errorf("htmx: row count = %d, want 3", got)
	}
}

// userReportedIssueHeaderHTMXTemplate is the template_source the
// user-reported-issue-header-htmx widget-type bead carries. It mirrors
// templates/partials/widgets_phase4.html `widget_user_reported_issue_header`
// — same chrome (.ii-w-user-header > .ii-w-header-top + .ii-w-meta), same
// optional severity pill / title / ticket link / reported time.
//
// Two intentional gaps vs. the Go renderer (ka-0000000993 port doc):
//   - the "Open ticket" action button is server-side only. The Go path
//     builds it via actionButtonJSON / OpenJiraAction; html/template has
//     no template-only equivalent (same gap as header-incident-summary-htmx
//     and related-tickets-htmx).
//   - severity normalization is server-side only. The Go path runs
//     strings.ToLower(severity); html/template has no `lower` func
//     registered, so producers must pre-lowercase severity in the payload
//     (same gap as header-incident-summary-htmx).
const userReportedIssueHeaderHTMXTemplate = `<div class="ii-w-user-header">
{{- $title := index .Payload "title" -}}
{{- $ticketID := index .Payload "ticket_id" -}}
{{- $ticketURL := index .Payload "ticket_url" -}}
{{- $severity := index .Payload "severity" -}}
{{- $reportedAt := index .Payload "reported_at" -}}
  <div class="ii-w-header-top">
    {{if $severity}}<span class="ii-pill" data-severity="{{$severity}}">{{$severity}}</span>{{end}}
    {{if $title}}<h3 class="ii-w-title">{{$title}}</h3>{{end}}
  </div>
  <p class="ii-w-meta">
    {{if $ticketID}}
      Ticket
      {{if $ticketURL}}<a href="{{$ticketURL}}" target="_blank" rel="noopener"><code>{{$ticketID}}</code></a>
      {{else}}<code>{{$ticketID}}</code>{{end}}
    {{end}}
    {{if $reportedAt}}
      · reported <time class="ii-relative-time" datetime="{{$reportedAt}}">{{$reportedAt}}</time>
    {{end}}
  </p>
</div>`

// TestCompileHtmxRenderer_UserReportedIssueHeader exercises the
// user-reported-issue-header-htmx happy path: full payload renders the
// chrome with the severity pill, title h3, ticket anchor, and reported
// <time> element.
func TestCompileHtmxRenderer_UserReportedIssueHeader(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-2nxyrqgnbs", "user-reported-issue-header-htmx",
		userReportedIssueHeaderHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compileHtmxRenderer: %v", err)
	}
	payload := `{
		"title":"Cannot upload TMF document",
		"ticket_id":"FSM-12345",
		"ticket_url":"https://acme.atlassian.net/browse/FSM-12345",
		"severity":"p2",
		"reported_at":"2026-05-05T10:00:00Z"
	}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("renderer error: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		`<div class="ii-w-user-header">`,
		`<div class="ii-w-header-top">`,
		`data-severity="p2"`,
		`<h3 class="ii-w-title">Cannot upload TMF document</h3>`,
		`<a href="https://acme.atlassian.net/browse/FSM-12345"`,
		`target="_blank"`,
		`rel="noopener"`,
		`<code>FSM-12345</code>`,
		`datetime="2026-05-05T10:00:00Z"`,
		`reported`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q\n--- got ---\n%s", want, got)
		}
	}
	if strings.Contains(got, "Open ticket") {
		t.Errorf("unexpected action button in htmx render (parity gap):\n%s", got)
	}
}

// TestCompileHtmxRenderer_UserReportedIssueHeaderEmpty falls into the
// empty-payload arm — the chrome shell renders, no pill, no title, no
// ticket text, no reported time. The .ii-w-meta paragraph still renders
// (matches the Go template's unconditional <p class="ii-w-meta">).
func TestCompileHtmxRenderer_UserReportedIssueHeaderEmpty(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-ai0svfjcb3", "user-reported-issue-header-htmx",
		userReportedIssueHeaderHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, `<div class="ii-w-user-header">`) {
		t.Errorf("expected user-header shell, got: %s", got)
	}
	if !strings.Contains(got, `<p class="ii-w-meta">`) {
		t.Errorf("expected unconditional .ii-w-meta paragraph, got: %s", got)
	}
	for _, banned := range []string{
		`data-severity`, `<h3`, `<code`, `<time`, `<a href`,
	} {
		if strings.Contains(got, banned) {
			t.Errorf("unexpected %q in empty render:\n%s", banned, got)
		}
	}
}

// TestCompileHtmxRenderer_UserReportedIssueHeaderNoTicketURL covers the
// ticket-id-without-url arm: <code>FSM-…</code> renders bare, no anchor.
func TestCompileHtmxRenderer_UserReportedIssueHeaderNoTicketURL(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-33pbgephbq", "user-reported-issue-header-htmx",
		userReportedIssueHeaderHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{"ticket_id":"FSM-99"}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, `<code>FSM-99</code>`) {
		t.Errorf("expected bare <code>FSM-99</code>, got: %s", got)
	}
	if strings.Contains(got, `<a href`) {
		t.Errorf("unexpected anchor when ticket_url is missing:\n%s", got)
	}
}

// TestUserReportedIssueHeaderParity is the side-by-side fidelity check
// for the user-reported-issue-header-htmx port — twelfth free-htmx port
// after activity-log (ka-0000004584, MR !2334), related-tickets
// (ka-0000005540, MR !2346), header-incident-summary (ka-0000005304,
// MR !2347), reproduction-steps (ka-0000007835), reporter-info
// (ka-0000004065), event-timeline (ka-0000000243), ai-recommended-actions
// (ka-0000000272), fix-available-pending-branch (ka-0000005506),
// vm-sparklines (ka-0000000715), source-code-analysis (ka-0000006619),
// and workaround (ka-0000001237). Same payload through both renderers
// must produce structurally-equivalent chrome: the .ii-w-user-header
// container, the .ii-w-header-top with severity pill + title h3, the
// .ii-w-meta paragraph with the ticket anchor and reported <time>.
//
// Parity gaps (intentional, port doc):
//  1. The Go renderer emits an "Open ticket" action button when ticket_id
//     is set (built via actionButtonJSON + OpenJiraAction); the htmx
//     template body has no access to those helpers and omits the button.
//     Same gap as header-incident-summary-htmx and related-tickets-htmx.
//  2. The Go renderer lowercases severity server-side via
//     strings.ToLower; the htmx payload is pre-lowered to match (same
//     approach as TestHeaderIncidentSummaryParity).
func TestUserReportedIssueHeaderParity(t *testing.T) {
	resetHtmxRendererCache()
	payload := `{
		"title":"Cannot upload TMF document <50MB>",
		"ticket_id":"FSM-12345",
		"ticket_url":"https://acme.atlassian.net/browse/FSM-12345",
		"severity":"P2",
		"reported_at":"2026-05-05T10:00:00Z"
	}`

	wctx := newTestWidgetCtx(t)
	goHTML, err := renderUserReportedIssueHeader(payload, wctx)
	if err != nil {
		t.Fatalf("Go renderer: %v", err)
	}

	htmxBead := makeHtmxWidgetTypeBead("gd-4jlbussyk9", "user-reported-issue-header-htmx",
		userReportedIssueHeaderHTMXTemplate, time.Now())
	htmxR, err := compileHtmxRenderer(htmxBead)
	if err != nil {
		t.Fatalf("htmx compile: %v", err)
	}
	// Mirror the Go renderer's strings.ToLower(severity) normalization in
	// the payload — same approach as TestHeaderIncidentSummaryParity.
	htmxPayload := strings.NewReplacer(`"P2"`, `"p2"`).Replace(payload)
	htmxHTML, err := htmxR(htmxPayload, nil)
	if err != nil {
		t.Fatalf("htmx render: %v", err)
	}

	// Parity assertions: every chrome marker the existing CSS targets
	// must appear in BOTH outputs.
	for _, want := range []string{
		`<div class="ii-w-user-header">`,
		`<div class="ii-w-header-top">`,
		`data-severity="p2"`,
		// html/template auto-escapes `<50MB>` identically in both paths.
		`Cannot upload TMF document &lt;50MB&gt;`,
		`<code>FSM-12345</code>`,
		`href="https://acme.atlassian.net/browse/FSM-12345"`,
		`target="_blank"`,
		`rel="noopener"`,
		`datetime="2026-05-05T10:00:00Z"`,
		`<time class="ii-relative-time"`,
	} {
		if !strings.Contains(string(goHTML), want) {
			t.Errorf("Go renderer missing %q\n--- Go ---\n%s", want, goHTML)
		}
		if !strings.Contains(string(htmxHTML), want) {
			t.Errorf("htmx renderer missing %q\n--- htmx ---\n%s", want, htmxHTML)
		}
	}

	// Documented parity gap: the Go renderer emits an "Open ticket"
	// action button when ticket_id is set. The htmx template has no
	// access to actionButtonJSON / OpenJiraAction and omits the button.
	if !strings.Contains(string(goHTML), "Open ticket") {
		t.Errorf("Go: expected 'Open ticket' action button:\n%s", goHTML)
	}
	if strings.Contains(string(htmxHTML), "Open ticket") {
		t.Errorf("htmx: unexpected 'Open ticket' action button (parity gap):\n%s", htmxHTML)
	}

	// Empty payload: both renders fall into the empty-state arm — chrome
	// shell, no pill, no title, no ticket, no reported time.
	goEmpty, err := renderUserReportedIssueHeader(`{}`, wctx)
	if err != nil {
		t.Fatalf("Go renderer (empty): %v", err)
	}
	htmxEmpty, err := htmxR(`{}`, nil)
	if err != nil {
		t.Fatalf("htmx renderer (empty): %v", err)
	}
	for _, want := range []string{
		`<div class="ii-w-user-header">`,
		`<p class="ii-w-meta">`,
	} {
		if !strings.Contains(string(goEmpty), want) {
			t.Errorf("Go (empty) missing %q:\n%s", want, goEmpty)
		}
		if !strings.Contains(string(htmxEmpty), want) {
			t.Errorf("htmx (empty) missing %q:\n%s", want, htmxEmpty)
		}
	}
	for _, banned := range []string{`data-severity`, `<h3`, `<code`, `<time`} {
		if strings.Contains(string(goEmpty), banned) {
			t.Errorf("Go (empty) unexpected %q:\n%s", banned, goEmpty)
		}
		if strings.Contains(string(htmxEmpty), banned) {
			t.Errorf("htmx (empty) unexpected %q:\n%s", banned, htmxEmpty)
		}
	}
}

// suggestedTestCasesHTMXTemplate is the template_source the
// suggested-test-cases-htmx widget-type bead carries. It mirrors
// templates/partials/widgets_phase4.html `widget_suggested_test_cases` so a
// side-by-side render matches the test-list chrome (.ii-w-tests container,
// <ul class="ii-w-tests-list">, per-row .ii-w-test-row with the disabled
// checkbox label, optional coverage pill, and optional detail paragraph).
//
// No parity gaps vs. the Go renderer (ka-0000001809 port doc): the Go path
// at widgets_phase4.go::renderSuggestedTestCases does no normalisation, no
// markdown rendering, and no button construction. Both paths render the
// payload verbatim through html/template auto-escaping.
const suggestedTestCasesHTMXTemplate = `<div class="ii-w-tests">
{{- $cases := index .Payload "cases" -}}
{{if $cases}}
  <ul class="ii-w-tests-list">
    {{range $cases}}
      <li class="ii-w-test-row">
        <label class="ii-w-test-label">
          <input type="checkbox" disabled />
          <span class="ii-w-test-title">{{.title}}</span>
          {{if .coverage}}<span class="ii-pill" data-state="{{.coverage}}">{{.coverage}}</span>{{end}}
        </label>
        {{if .detail}}<p class="ii-w-test-detail">{{.detail}}</p>{{end}}
      </li>
    {{end}}
  </ul>
{{else}}
  <p class="empty">No test cases suggested.</p>
{{end}}
</div>`

// TestCompileHtmxRenderer_SuggestedTestCases exercises the
// suggested-test-cases-htmx happy path: cases[] payload renders the chrome
// with the disabled-checkbox row, coverage pill, and detail paragraph the
// existing CSS targets.
func TestCompileHtmxRenderer_SuggestedTestCases(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-o8sqsvukhq", "suggested-test-cases-htmx",
		suggestedTestCasesHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))

	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compileHtmxRenderer: %v", err)
	}
	payload := `{"cases":[
		{"title":"Upload a 50 MB PDF","detail":"Asserts the spinner clears within 30 s.","coverage":"regression"},
		{"title":"Upload a malformed file","coverage":"edge"}
	]}`
	html, err := r(payload, nil)
	if err != nil {
		t.Fatalf("renderer error: %v", err)
	}
	got := string(html)
	for _, want := range []string{
		`<div class="ii-w-tests">`,
		`<ul class="ii-w-tests-list">`,
		`<input type="checkbox" disabled`,
		`<span class="ii-w-test-title">Upload a 50 MB PDF</span>`,
		`<span class="ii-w-test-title">Upload a malformed file</span>`,
		`data-state="regression"`,
		`data-state="edge"`,
		`<p class="ii-w-test-detail">Asserts the spinner clears within 30 s.</p>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered HTML missing %q\n--- got ---\n%s", want, got)
		}
	}
	if c := strings.Count(got, `class="ii-w-test-row"`); c != 2 {
		t.Errorf("ii-w-test-row count = %d, want 2", c)
	}
}

// TestCompileHtmxRenderer_SuggestedTestCasesEmpty falls into the empty-state
// arm — no cases[] renders the placeholder copy and no <ul>.
func TestCompileHtmxRenderer_SuggestedTestCasesEmpty(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-nocry9cwnp", "suggested-test-cases-htmx",
		suggestedTestCasesHTMXTemplate, time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{"cases":[]}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	if !strings.Contains(got, `<div class="ii-w-tests">`) {
		t.Errorf("expected chrome shell, got: %s", got)
	}
	if !strings.Contains(got, "No test cases suggested.") {
		t.Errorf("expected empty-state copy, got: %s", got)
	}
	if strings.Contains(got, "<ul") {
		t.Errorf("unexpected <ul> in empty render:\n%s", got)
	}
}

// TestSuggestedTestCasesParity is the side-by-side fidelity check for the
// suggested-test-cases-htmx port — twelfth free-htmx port after activity-log
// (ka-0000004584, MR !2334), related-tickets (ka-0000005540, MR !2346),
// header-incident-summary (ka-0000005304, MR !2347), reproduction-steps
// (ka-0000007835, MR !2350), ai-recommended-actions (ka-0000000272,
// MR !2351), and the rest of the Phase 4–7 backlog (ka-0000005157).
//
// Same cases[] payload through the Go renderer and the htmx widget-type
// produces structurally-equivalent test-list chrome: the .ii-w-tests
// container, the <ul class="ii-w-tests-list">, one .ii-w-test-row per
// case with the disabled-checkbox label, the .ii-w-test-title span, the
// optional coverage pill (data-state), and the optional .ii-w-test-detail
// paragraph.
//
// No parity gaps. The Go renderer at
// widgets_phase4.go::renderSuggestedTestCases does no normalisation, no
// markdown rendering, and no button construction — it just lifts
// title/detail/coverage off each case object and feeds them into the
// template. Both renders are bit-for-bit equivalent modulo the field-
// access shape (`.Title` Go struct vs `.title` map index).
func TestSuggestedTestCasesParity(t *testing.T) {
	resetHtmxRendererCache()
	payload := `{"cases":[
		{"title":"Upload a 50 MB PDF","detail":"Asserts the spinner clears within 30 s.","coverage":"regression"},
		{"title":"Upload a malformed file","coverage":"edge"}
	]}`

	wctx := newTestWidgetCtx(t)
	goHTML, err := renderSuggestedTestCases(payload, wctx)
	if err != nil {
		t.Fatalf("Go renderer: %v", err)
	}

	htmxBead := makeHtmxWidgetTypeBead("gd-gf8hrr7j1k", "suggested-test-cases-htmx",
		suggestedTestCasesHTMXTemplate, time.Now())
	htmxR, err := compileHtmxRenderer(htmxBead)
	if err != nil {
		t.Fatalf("htmx compile: %v", err)
	}
	htmxHTML, err := htmxR(payload, nil)
	if err != nil {
		t.Fatalf("htmx render: %v", err)
	}

	// Parity assertions: every chrome marker the existing CSS targets must
	// appear in BOTH outputs.
	for _, want := range []string{
		`<div class="ii-w-tests">`,
		`<ul class="ii-w-tests-list">`,
		`class="ii-w-test-row"`,
		`<input type="checkbox" disabled`,
		`<span class="ii-w-test-title">Upload a 50 MB PDF</span>`,
		`<span class="ii-w-test-title">Upload a malformed file</span>`,
		`data-state="regression"`,
		`data-state="edge"`,
		`<p class="ii-w-test-detail">Asserts the spinner clears within 30 s.</p>`,
	} {
		if !strings.Contains(string(goHTML), want) {
			t.Errorf("Go renderer missing %q\n--- Go ---\n%s", want, goHTML)
		}
		if !strings.Contains(string(htmxHTML), want) {
			t.Errorf("htmx renderer missing %q\n--- htmx ---\n%s", want, htmxHTML)
		}
	}

	// Both renders must produce exactly two test rows.
	if got := strings.Count(string(goHTML), `class="ii-w-test-row"`); got != 2 {
		t.Errorf("Go: ii-w-test-row count = %d, want 2", got)
	}
	if got := strings.Count(string(htmxHTML), `class="ii-w-test-row"`); got != 2 {
		t.Errorf("htmx: ii-w-test-row count = %d, want 2", got)
	}

	// Empty payload: both renders fall into the empty-state arm.
	goEmpty, err := renderSuggestedTestCases(`{}`, wctx)
	if err != nil {
		t.Fatalf("Go renderer (empty): %v", err)
	}
	htmxEmpty, err := htmxR(`{}`, nil)
	if err != nil {
		t.Fatalf("htmx renderer (empty): %v", err)
	}
	if !strings.Contains(string(goEmpty), "No test cases suggested.") {
		t.Errorf("Go: expected empty-state copy:\n%s", goEmpty)
	}
	if !strings.Contains(string(htmxEmpty), "No test cases suggested.") {
		t.Errorf("htmx: expected empty-state copy:\n%s", htmxEmpty)
	}
}
