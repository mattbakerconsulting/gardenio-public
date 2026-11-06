package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/freehtmx"
)

// Viewer-render half of gd-a2s0GZExxr: a widget-type from a tenant land never
// renders as trusted HTML, and what a template renders cannot run script.

const tenantWidgetTemplate = `<div class="w-tenant">tenant<script>alert(document.cookie)</script></div>`

func widgetTrustProjects() map[string]beadsapi.ProjectInfo {
	return map[string]beadsapi.ProjectInfo{
		"gardenio": {Name: "gardenio"},
		"land-a":   {Name: "land-a", LandKind: "tenant"},
	}
}

func labelledWidgetType(id, slug, src, project string, updated time.Time) *beadsapi.BeadDetail {
	b := makeHtmxWidgetTypeBead(id, slug, src, updated)
	if project != "" {
		b.Labels = []string{"project:" + project}
	}
	return b
}

// A tenant's newer widget-type under a fleet slug does not shadow the fleet's.
func TestLookupWidgetForReport_TenantWidgetTypeDoesNotShadowFleet(t *testing.T) {
	resetHtmxRendererCache()
	resolver := &fakeHtmxResolver{
		projects: widgetTrustProjects(),
		beads: []*beadsapi.BeadDetail{
			labelledWidgetType("gd-fleetwt001", "shared-htmx", `<div class="w-fleet">fleet</div>`, "gardenio",
				time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)),
			labelledWidgetType("gd-tenantwt01", "shared-htmx", tenantWidgetTemplate, "land-a",
				time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)),
		},
	}
	r, ok := lookupWidgetForReport(context.Background(), "shared-htmx", resolver)
	if !ok {
		t.Fatal("expected the fleet widget-type to resolve")
	}
	html, err := r(`{}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(string(html), "fleet") || strings.Contains(string(html), "tenant") {
		t.Errorf("rendered the wrong widget-type: %s", html)
	}
}

func TestLookupWidgetForReport_RefusesUntrustedWidgetType(t *testing.T) {
	for _, tc := range []struct {
		name     string
		resolver htmxResolver
	}{
		{"tenant land", &fakeHtmxResolver{projects: widgetTrustProjects(), beads: []*beadsapi.BeadDetail{
			labelledWidgetType("gd-tenantwt02", "x-htmx", tenantWidgetTemplate, "land-a", time.Now()),
		}}},
		{"unregistered project", &fakeHtmxResolver{projects: widgetTrustProjects(), beads: []*beadsapi.BeadDetail{
			labelledWidgetType("gd-tenantwt03", "x-htmx", tenantWidgetTemplate, "land-gone", time.Now()),
		}}},
		{"project lookup fails", &fakeHtmxResolver{projectsErr: errors.New("daemon down"), beads: []*beadsapi.BeadDetail{
			labelledWidgetType("gd-tenantwt04", "x-htmx", tenantWidgetTemplate, "", time.Now()),
		}}},
		{"resolver without a project registry", listOnlyResolver{beads: []*beadsapi.BeadDetail{
			labelledWidgetType("gd-tenantwt05", "x-htmx", tenantWidgetTemplate, "", time.Now()),
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetHtmxRendererCache()
			if _, ok := lookupWidgetForReport(context.Background(), "x-htmx", tc.resolver); ok {
				t.Fatal("an untrusted widget-type resolved")
			}
		})
	}
}

type listOnlyResolver struct{ beads []*beadsapi.BeadDetail }

func (l listOnlyResolver) ListBeadsFiltered(context.Context, beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	return &beadsapi.ListBeadsResult{Beads: l.beads}, nil
}

// Even a trusted template's literal script and handlers do not survive render.
func TestCompileHtmxRenderer_TemplateScriptIsInert(t *testing.T) {
	resetHtmxRendererCache()
	bead := makeHtmxWidgetTypeBead("gd-inertwt001", "inert-htmx",
		`<div class="ii-w-x" data-kind="k"><script>alert(1)</script><button onclick="alert(2)" hx-post="/admin/x" hx-trigger="load">go</button><a href="javascript:alert(3)">l</a></div>`,
		time.Now())
	r, err := compileHtmxRenderer(bead)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	html, err := r(`{}`, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := string(html)
	for _, banned := range []string{"<script", "alert(", "onclick", "hx-post", "hx-trigger", "javascript:"} {
		if strings.Contains(got, banned) {
			t.Errorf("rendered output keeps %q: %s", banned, got)
		}
	}
	for _, want := range []string{`class="ii-w-x"`, `data-kind="k"`, `<button>go</button>`} {
		if !strings.Contains(got, want) {
			t.Errorf("rendered output lost %q: %s", want, got)
		}
	}
}

func TestInertWidgetHTML(t *testing.T) {
	in := `<div class="a" data-kind="k" style="color:red">` +
		`<a href="https://example.com/x">ok</a><a href="/b/gd-1">rel</a><a href="java	script:alert(1)">tab</a>` +
		`<a href="data:text/html,x">data</a><img src="x" onerror="alert(1)">` +
		`<span data-hx-get="/admin/y" hx-on:click="alert(1)">s</span>` +
		`<iframe srcdoc="<script>alert(1)</script>"></iframe><form action="/admin/z"><button formaction="/admin/z">f</button></form>` +
		`<svg onload="alert(1)"><circle/></svg><base href="https://evil.example/"><style>body{}</style>` +
		`<p>a &lt; b &amp; &#39;c&#39;</p></div>`
	got := inertWidgetHTML(in)
	for _, banned := range []string{"script", "alert", "onerror", "hx-", "<iframe", "srcdoc", "<form", "formaction",
		"<svg", "<base", "<style", `href="data:`, "evil.example"} {
		if strings.Contains(got, banned) {
			t.Errorf("output keeps %q: %s", banned, got)
		}
	}
	for _, want := range []string{`class="a"`, `data-kind="k"`, `style="color:red"`, `href="https://example.com/x"`,
		`href="/b/gd-1"`, `<a>tab</a>`, `<span>s</span>`, `<p>a &lt; b &amp; &#39;c&#39;</p>`} {
		if !strings.Contains(got, want) {
			t.Errorf("output lost %q: %s", want, got)
		}
	}
}

// The preview fragment refuses a tenant widget-type and carries the strict CSP.
func TestHTMXWidgetPreview_TenantNotRenderedStrictCSP(t *testing.T) {
	fake := newFake()
	fake.projectsResp = widgetTrustProjects()
	fake.listResp = &beadsapi.ListBeadsResult{Beads: []*beadsapi.BeadDetail{
		labelledWidgetType("gd-tenantwt06", "tenant-htmx", tenantWidgetTemplate, "land-a", time.Now()),
		labelledWidgetType("gd-fleetwt002", "fleet-htmx", `<div class="w-fleet">fleet<script>alert(1)</script></div>`, "gardenio", time.Now()),
	}}
	srv := NewServer(fake, testLogger(), "", "http://test", "", nil)
	mux := http.NewServeMux()
	srv.RegisterRoutes(mux)

	get := func(slug string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/htmx/widget-types/"+slug+"/preview", nil))
		return w
	}

	w := get("tenant-htmx")
	if w.Code != http.StatusOK {
		t.Fatalf("tenant preview: HTTP %d: %s", w.Code, w.Body)
	}
	if got := w.Header().Get("Content-Security-Policy"); got != freehtmx.StrictCSP {
		t.Errorf("preview CSP = %q, want freehtmx.StrictCSP", got)
	}
	body := w.Body.String()
	if strings.Contains(body, "w-tenant") || strings.Contains(body, "alert(") {
		t.Errorf("tenant template rendered: %s", body)
	}
	if !strings.Contains(body, "not from an operator land") {
		t.Errorf("tenant preview does not say why it is not rendered: %s", body)
	}

	body = get("fleet-htmx").Body.String()
	if !strings.Contains(body, `class="w-fleet"`) || strings.Contains(body, "alert(") {
		t.Errorf("fleet preview: want rendered and inert, got: %s", body)
	}
}
