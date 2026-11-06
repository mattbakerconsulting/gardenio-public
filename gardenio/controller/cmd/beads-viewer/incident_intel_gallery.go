package main

import (
	"encoding/json"
	"html/template"
	"net/http"
	"time"
)

// phase4GalleryWidget is one entry in the Phase 4 section of the gallery —
// a representative payload dispatched through the live renderer. Re-used
// for Phase 5+ since the wrapper shape is identical (slug, headline, html).
type phase4GalleryWidget struct {
	Slug     string
	Headline string
	HTML     template.HTML
}

// phase4GalleryFixtures returns one fixture per registered Phase 4 widget
// with payloads that exercise non-empty branches. Order matches
// registeredWidgets() (lexical) so the gallery has a stable layout.
func phase4GalleryFixtures() []struct {
	Slug     string
	Headline string
	Payload  string
} {
	return []struct {
		Slug     string
		Headline string
		Payload  string
	}{
		{"activity-log", "Activity log", `{"entries":[{"when":"2026-05-06T12:00:00Z","actor":"gardenio","kind":"acknowledged","text":"Auto-ack on alert"},{"when":"2026-05-06T12:05:00Z","actor":"matt","kind":"comment","text":"Investigating session token rotation"}]}`},
		{"affected-files", "Affected files", `{"files":[{"path":"gardenio/controller/cmd/beads-viewer/server.go","line":442,"reason":"new auth middleware introduced"},{"path":"gardenio/controller/internal/bridge/decisions.go","line":118,"reason":"call site"}]}`},
		{"ai-recommended-actions", "AI recommended actions", `{"actions":[{"headline":"Roll back !2185","detail":"Probable cause is the auth refactor","confidence":0.9,"hotkey":"r","open_mr":"https://gitlab.com/x/y/-/merge_requests/2185","button_label":"Open MR"},{"headline":"Check Mezmo for 401 spikes","confidence":"medium","open_mezmo":"app:beads-viewer level:error","button_label":"Open logs"}]}`},
		{"header-incident-summary", "Header / incident summary", `{"title":"study-web 502 spike — america-prod","severity":"P1","status":"open","started_at":"2026-05-06T11:50:00Z","incident_id":"ka-incident-001","primary_action_url":"https://gitlab.com/x/y/-/merge_requests/2185","primary_action_label":"!2185"}`},
		{"related-tickets", "Related tickets", `{"tickets":[{"id":"FSM-12345","kind":"jira","title":"TMF upload broken","state":"open"},{"id":"!2185","kind":"mr","title":"Fast-track auth refactor","url":"https://gitlab.com/x/-/merge_requests/2185","state":"merged"}]}`},
		{"reporter-info", "Reporter info", `{"name":"Perry Tate","email":"paulo@example.com","org":"Acme","role":"Site coordinator"}`},
		{"reproduction-steps", "Reproduction steps", `{"markdown":"1. Open the upload dialog\n2. Pick a 50 MB PDF\n3. Click **Upload** — see the spinner hang\n4. Refresh and try again — repeats"}`},
		{"service-health", "Service health", `{"services":[{"service":"study-web","state":"critical","detail":"5xx 12% (last 5m)"},{"service":"sponsor-api","state":"warn","detail":"latency p95 1.2s"},{"service":"site-api","state":"healthy","detail":"5xx 0%"}]}`},
		{"suggested-test-cases", "Suggested test cases", `{"cases":[{"title":"Verify login redirect on stale session","detail":"Token < 60s should re-auth","coverage":"missing"},{"title":"Verify upload of files > 25 MB","detail":"Regression seen in FSM-12345","coverage":"partial"}]}`},
		{"user-reported-issue-header", "User-reported issue header", `{"title":"Cannot upload TMF document","ticket_id":"FSM-12345","ticket_url":"https://acme.atlassian.net/browse/FSM-12345","severity":"P2","reported_at":"2026-05-05T10:00:00Z"}`},
		{"workaround", "Workaround", `{"headline":"Use the legacy upload path","markdown":"Append ` + "`?legacy=1`" + ` to the upload URL. Logged in users are auto-redirected back."}`},
	}
}

// handleIncidentIntelGallery renders the primitive gallery — a dev page that
// exercises each shared CSS+JS primitive in isolation, plus the Phase 3
// action descriptors. Daemon-free so it stays a stable demo target for
// downstream widget authors. See beads ka-0000007090 (Phase 1) and
// ka-0000005188 (Phase 3).
func (s *Server) handleIncidentIntelGallery(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	// Phase 3 action descriptors — pre-rendered as JSON so the template can
	// drop them straight into data-ii-action attributes. Each one exercises a
	// different dispatcher kind so a regression in any branch leaves a
	// missing fingerprint in the gallery test.
	data := map[string]any{
		"NowISO":          now.Format(time.RFC3339),
		"FiveMinAgoISO":   now.Add(-5 * time.Minute).Format(time.RFC3339),
		"TwoHoursAgoISO":  now.Add(-2 * time.Hour).Format(time.RFC3339),
		"ThreeDaysAgoISO": now.Add(-72 * time.Hour).Format(time.RFC3339),
		"ActionJira":      OpenJiraAction("", "FSM-12345").MustJSON(),
		"ActionMR":        OpenMRAction("!2185", "https://gitlab.com/example-org/gardenio/-/merge_requests/2185").MustJSON(),
		"ActionMezmo":     OpenMezmoAction("", `app:beads-viewer level:error`).MustJSON(),
		"ActionIDE":       OpenIDEAction("gardenio/controller/cmd/beads-viewer/server.go", 442).MustJSON(),
		"ActionCopy":      CopyToClipboardAction("Copy panic line", "panic: runtime error: index out of range").MustJSON(),
		"ActionDismiss":   DismissEvidenceAction("evidence-demo-1", "#ii-action-htmx-target").MustJSON(),
		"ActionAck":       AckIncidentAction("ka-demo-incident", "#ii-action-ack-target").MustJSON(),
		"RegisteredActionIDs": func() []string {
			if s.actions == nil {
				return nil
			}
			ids := s.actions.IDs()
			return ids
		}(),
		"Phase4Widgets": s.renderPhase4Gallery(),
		"Phase5Widgets": s.renderPhase5Gallery(),
		"Phase6Widgets": s.renderPhase6Gallery(),
		"Phase7Widgets": s.renderPhase7Gallery(),
		"Phase8Widgets": s.renderPhase8Gallery(),
		"Phase8LiveAllowlist": liveAllowlist(),
	}
	s.render(w, r, "incident_intel_gallery.html", data)
}

// renderPhase4Gallery dispatches each fixture through the live widget
// renderer registry so the gallery shows what the rich-report page actually
// produces. Render errors degrade to an empty slot rather than failing the
// whole gallery — a regression in one widget shouldn't take down the page.
func (s *Server) renderPhase4Gallery() []phase4GalleryWidget {
	return s.dispatchGalleryFixtures(phase4GalleryFixtures())
}

// renderPhase5Gallery does the same for Phase 5 — code-highlighted widgets.
// Kept as its own list so the gallery can group sections by phase and the
// widget-by-widget header reflects the synthesis-doc phase ordering.
func (s *Server) renderPhase5Gallery() []phase4GalleryWidget {
	return s.dispatchGalleryFixtures(phase5GalleryFixtures())
}

// renderPhase6Gallery — diff widget. One fixture; per-fixture-shape parity
// with the earlier phases keeps the gallery template generic.
func (s *Server) renderPhase6Gallery() []phase4GalleryWidget {
	return s.dispatchGalleryFixtures(phase6GalleryFixtures())
}

// renderPhase7Gallery — uPlot time-series widgets (#10 sparklines, #7 event
// timeline). Two fixtures share the same hover-sync GroupID so the gallery
// demonstrates cross-chart cursor coordination.
func (s *Server) renderPhase7Gallery() []phase4GalleryWidget {
	return s.dispatchGalleryFixtures(phase7GalleryFixtures())
}

// renderPhase8Gallery — live-update + cross-filter v2 demo. Reuses Phase 4 +
// Phase 7 widgets that participate in cross-filter (service-health,
// event-timeline) plus the always-live header so the gallery exercises the
// shared filter bus and the htmx polling target shape.
func (s *Server) renderPhase8Gallery() []phase4GalleryWidget {
	return s.dispatchGalleryFixtures(phase8GalleryFixtures())
}

// dispatchGalleryFixtures is the shared dispatch loop both phase galleries
// use. Render errors degrade to an empty slot rather than failing the whole
// gallery — a regression in one widget shouldn't take down the page.
func (s *Server) dispatchGalleryFixtures(fixtures []struct {
	Slug     string
	Headline string
	Payload  string
}) []phase4GalleryWidget {
	wctx := s.newWidgetRenderCtx()
	out := make([]phase4GalleryWidget, 0, len(fixtures))
	for _, f := range fixtures {
		entry := phase4GalleryWidget{Slug: f.Slug, Headline: f.Headline}
		if r, ok := lookupWidget(f.Slug); ok {
			if html, err := r(f.Payload, wctx); err == nil {
				entry.HTML = html
			}
		}
		out = append(out, entry)
	}
	return out
}

// phase5GalleryFixtures returns one fixture per Phase 5 widget. Same shape
// as phase4GalleryFixtures so dispatchGalleryFixtures can share a loop.
func phase5GalleryFixtures() []struct {
	Slug     string
	Headline string
	Payload  string
} {
	return []struct {
		Slug     string
		Headline string
		Payload  string
	}{
		{"rca-evidence-cards", "RCA evidence cards", `{"cards":[
			{"id":"ev-1","headline":"Auth middleware drops sessions on stale cookie","detail":"401 spike correlates with !2185 deploy at 11:50 UTC","source_kind":"mezmo","source_label":"mezmo: auth-svc","confidence":0.92,"language":"go","code":"func (m *Auth) Validate(ctx context.Context, c *Cookie) error {\n\tif c == nil { return ErrUnauth }\n\treturn nil // FIXME: missing TTL check\n}","open_mezmo":"app:auth-svc level:error","drilldown_markdown":"### Detail\n\n- First seen 11:50:14 UTC\n- 1,204 events / 5 min\n- Affected sessions: ~430"},
			{"id":"ev-2","headline":"Latency spike on /v1/sessions","detail":"p95 jumped 220ms → 1.4s","source_kind":"victoria-metrics","source_label":"VM: study-web p95","confidence":"medium","language":"promql","code":"histogram_quantile(0.95, rate(http_request_duration_seconds_bucket{service=\"study-web\"}[5m]))","open_mr":"https://gitlab.com/example-org/gardenio/-/merge_requests/2185"}
		]}`},
		{"source-code-analysis", "Source-code analysis", `{
			"title":"Top hot lines (last 1h)",
			"findings":[
				{"path":"controller/cmd/beads-viewer/server.go","line":442,"function":"NewServer","severity":"error","headline":"highlighter init runs even when chroma deps fail to load","detail":"NewServer keeps booting if newCodeHighlighter() panics — wrap in recover or hard-fail.","language":"go","code":"hl := highlighting.NewHighlighting(\n  highlighting.WithStyle(\"github-dark\"),\n)"},
				{"path":"controller/internal/bridge/decisions.go","line":118,"function":"acceptDecision","severity":"warn","headline":"unbounded retries on slack 5xx","detail":"backoff loop has no ceiling, can wedge the watcher.","language":"go","code":"for {\n  if err := postSlack(d); err == nil { break }\n  time.Sleep(2 * time.Second) // <- no ceiling\n}"}
			]
		}`},
	}
}

// phase6GalleryFixtures returns one fixture for the diff widget. Sample diff
// is small enough to read inline in the gallery but exercises adds, dels,
// context, and language detection (Go).
func phase6GalleryFixtures() []struct {
	Slug     string
	Headline string
	Payload  string
} {
	sampleDiff := `diff --git a/controller/cmd/beads-viewer/server.go b/controller/cmd/beads-viewer/server.go
--- a/controller/cmd/beads-viewer/server.go
+++ b/controller/cmd/beads-viewer/server.go
@@ -440,6 +440,7 @@ func NewServer(daemon beadAPI, ...) *Server {
 	widgetTemplates := template.Must(template.New("").Funcs(funcs).ParseFS(templateFS,
 		"templates/partials/widgets_phase4.html",
 		"templates/partials/widgets_phase5.html",
+		"templates/partials/widgets_phase6.html",
 	))
 	s := &Server{
 		daemon:           daemon,
@@ -460,6 +461,7 @@ func NewServer(...) *Server {
 		actions:          actions,
 		widgetTemplates:  widgetTemplates,
 	}
-	registerStandardActions(s.actions)
+	registerStandardActions(s.actions)
+	registerServerBoundActions(s)
 	return s
 }`
	payload := map[string]any{
		"title":         "Fix available: drop stale auth cookies on session rotate",
		"branch":        "agent/auth-fix-stale-cookie",
		"base_branch":   "main",
		"commit_sha":    "9c2a3b7e1f4d8c0a5b6e9f2d4c1a3e5b7d9c0a2f",
		"author":        "gardenio",
		"mr_id":         "!2185",
		"mr_url":        "https://gitlab.com/example-org/gardenio/-/merge_requests/2185",
		"bead_id":       "ka-incident-001",
		"ci_status":     "passed",
		"ci_detail_url": "https://gitlab.com/example-org/gardenio/-/pipelines/12345",
		"summary_markdown": "Drops the stale-cookie path on session rotate. Wires the Phase 6 widget into the renderer registry; deepest test exercises the empty-diff fall-through.",
		"diff": sampleDiff,
	}
	b, _ := json.Marshal(payload)
	return []struct {
		Slug     string
		Headline string
		Payload  string
	}{
		{"fix-available-pending-branch", "Fix available in pending branch", string(b)},
	}
}

// phase7GalleryFixtures returns one fixture per Phase 7 widget. Both share
// `group_id: "ii-ts-incident-001"` so the gallery exercises hover-sync across
// the sparklines and the event timeline.
func phase7GalleryFixtures() []struct {
	Slug     string
	Headline string
	Payload  string
} {
	now := time.Now().UTC().Truncate(time.Minute)
	groupID := "ii-ts-incident-001"

	mkSeries := func(svc, metric, unit, state string, base, jitter float64) map[string]any {
		points := make([]map[string]any, 0, 30)
		for i := range 30 {
			t := now.Add(time.Duration(-29+i) * time.Minute)
			val := base + jitter*float64((i*7)%13)/10.0
			if i > 22 && metric == "p95" && svc == "study-web" {
				val *= 1.6
			}
			if i > 24 && metric == "5xx" && svc == "study-web" {
				val += 0.04 * float64(i-24)
			}
			points = append(points, map[string]any{
				"when":  t.Format(time.RFC3339),
				"value": val,
			})
		}
		return map[string]any{
			"service": svc, "metric": metric, "unit": unit, "state": state,
			"points": points,
		}
	}

	sparkPayload := map[string]any{
		"title":              "VictoriaMetrics — last 30 min",
		"subtitle":           "Hover any chart to sync the timeline below",
		"group_id":           groupID,
		"cross_filter_badge": "synced via hover (informational only · D4 v1)",
		"series": []any{
			mkSeries("study-web", "p95", "ms", "warn", 220, 0.6),
			mkSeries("study-web", "5xx", "%", "critical", 0.01, 0.05),
			mkSeries("sponsor-api", "p95", "ms", "healthy", 80, 0.2),
			mkSeries("site-api", "p95", "ms", "healthy", 95, 0.15),
		},
	}
	sparkB, _ := json.Marshal(sparkPayload)

	type ev = map[string]any
	timelinePayload := map[string]any{
		"title":              "Cross-source event timeline",
		"subtitle":           "Logs, traces, deploys, alerts. Markers per kind.",
		"group_id":           groupID,
		"cross_filter_badge": "synced via hover (informational only · D4 v1)",
		"events": []any{
			ev{"when": now.Add(-28 * time.Minute).Format(time.RFC3339), "kind": "gitlab", "title": "deploy !2185 to america-prod", "detail": "auth refactor", "url": "https://gitlab.com/x/-/merge_requests/2185"},
			ev{"when": now.Add(-22 * time.Minute).Format(time.RFC3339), "kind": "victoria-metrics", "title": "p95 study-web breached 350ms"},
			ev{"when": now.Add(-21 * time.Minute).Format(time.RFC3339), "kind": "mezmo", "title": "401 spike auth-svc", "detail": "1,204 events / 5 min"},
			ev{"when": now.Add(-18 * time.Minute).Format(time.RFC3339), "kind": "sentry", "title": "panic: stale cookie validate", "url": "https://sentry.io/issues/x"},
			ev{"when": now.Add(-15 * time.Minute).Format(time.RFC3339), "kind": "jira", "title": "FSM-12345 created", "url": "https://acme.atlassian.net/browse/FSM-12345"},
			ev{"when": now.Add(-12 * time.Minute).Format(time.RFC3339), "kind": "victoria-traces", "title": "/v1/sessions trace > 1.4s"},
			ev{"when": now.Add(-9 * time.Minute).Format(time.RFC3339), "kind": "slack", "title": "page from oncall channel"},
			ev{"when": now.Add(-3 * time.Minute).Format(time.RFC3339), "kind": "gitlab", "title": "fix branch agent/auth-fix-stale-cookie"},
		},
	}
	tlB, _ := json.Marshal(timelinePayload)

	return []struct {
		Slug     string
		Headline string
		Payload  string
	}{
		{"vm-sparklines", "VM sparklines (#10)", string(sparkB)},
		{"event-timeline", "Synchronized event timeline (#7)", string(tlB)},
	}
}

// phase8GalleryFixtures returns three widgets that participate in cross-filter
// v2 — service-health (filterable by service), event-timeline (filterable by
// source-kind + service, server-side), and header-incident-summary (one of the
// four live-update widgets). The gallery wires a chip-strip above this section
// that drives the cross-filter bus, so changing chips updates both widgets in
// place.
func phase8GalleryFixtures() []struct {
	Slug     string
	Headline string
	Payload  string
} {
	now := time.Now().UTC().Truncate(time.Minute)
	groupID := "ii-ts-phase8-demo"

	header := map[string]any{
		"title":                "study-web 502 spike — america-prod (live demo)",
		"severity":             "P1",
		"status":               "open",
		"started_at":           now.Add(-30 * time.Minute).Format(time.RFC3339),
		"incident_id":          "ka-incident-phase8",
		"primary_action_url":   "https://gitlab.com/example-org/gardenio/-/merge_requests/2191",
		"primary_action_label": "!2191",
	}
	headerB, _ := json.Marshal(header)

	svcHealth := map[string]any{
		"services": []any{
			map[string]any{"service": "study-web", "state": "critical", "detail": "5xx 12% (last 5m)"},
			map[string]any{"service": "sponsor-api", "state": "warn", "detail": "latency p95 1.2s"},
			map[string]any{"service": "site-api", "state": "healthy", "detail": "5xx 0%"},
			map[string]any{"service": "auth-svc", "state": "warn", "detail": "401 spike"},
		},
	}
	svcB, _ := json.Marshal(svcHealth)

	type ev = map[string]any
	timelinePayload := map[string]any{
		"title":              "Cross-filtered event timeline",
		"subtitle":           "Add a chip above to filter; non-matching rows fade out and the next 10s poll re-fetches a server-filtered payload.",
		"group_id":           groupID,
		"cross_filter_badge": "live · cross-filter v2",
		"events": []any{
			ev{"when": now.Add(-26 * time.Minute).Format(time.RFC3339), "kind": "gitlab", "title": "deploy !2191 to america-prod", "url": "https://gitlab.com/x/-/merge_requests/2191"},
			ev{"when": now.Add(-22 * time.Minute).Format(time.RFC3339), "kind": "victoria-metrics", "title": "p95 study-web breached 350ms", "service": "study-web"},
			ev{"when": now.Add(-21 * time.Minute).Format(time.RFC3339), "kind": "mezmo", "title": "401 spike auth-svc", "service": "auth-svc"},
			ev{"when": now.Add(-15 * time.Minute).Format(time.RFC3339), "kind": "jira", "title": "FSM-12345 created"},
			ev{"when": now.Add(-9 * time.Minute).Format(time.RFC3339), "kind": "slack", "title": "page from oncall channel"},
			ev{"when": now.Add(-3 * time.Minute).Format(time.RFC3339), "kind": "gitlab", "title": "fix branch agent/auth-fix-stale-cookie"},
		},
	}
	tlB, _ := json.Marshal(timelinePayload)

	return []struct {
		Slug     string
		Headline string
		Payload  string
	}{
		{"header-incident-summary", "Header — live (5s poll)", string(headerB)},
		{"service-health", "Service health — cross-filter by service", string(svcB)},
		{"event-timeline", "Event timeline — server-side cross-filter (10s poll)", string(tlB)},
	}
}
