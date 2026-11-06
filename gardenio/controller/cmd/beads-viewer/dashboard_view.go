package main

// dashboard_view.go — render Grafana dashboards in beads-viewer (W2c,
// task ka-0000001078; epic ka-0000006776). Builds on W2a (ka-0000000055),
// which models a Grafana dashboard as a type=dashboard bead with the JSON
// model stored in a type=doc bundle child labeled "grafana:dashboard-model".
//
// Two surfaces:
//
//  1. Bead view — injectTypeView adds a "dashboard" case that builds a
//     dashboardView from the bead's fields + the linked model doc. The
//     type_dashboard partial renders a live Grafana iframe (when GRAFANA_BASE_URL
//     is set), a metadata strip, and a JSON-model panel-list fallback.
//
//  2. Embeddable directive — a `:::grafana <uid> [panel=N] [height=H]` line in
//     any bead description / comment / bundle md doc expands (inside the
//     LinkifyHTML* post-pass) to the same sandboxed iframe.
//
// Auth path: the viewer and Grafana share one AVA-gated unified-host origin, so
// an <iframe src="<grafanaBaseURL>/d-solo/..."> is same-origin and reuses the
// viewer's AVA session — no render sidecar, no signed URL, no service-account
// token. See gardenio/docs/research/obs-w2c-render-dashboards-in-viewer.md.
//
// Security: every iframe src is built from the operator-set grafanaBaseURL plus
// a charset-validated uid and a numeric panel id — nothing author-supplied
// reaches the DOM unescaped, and the frame can only ever point at the one
// configured Grafana origin. goldmark stays WithUnsafe()-off; this directive is
// the only embedding path, mirroring how /image/ proxy URLs are the only
// embeddable image source.

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/grafanaembed"
)

// dashboardModelDocLabel marks the type=doc bead carrying a dashboard's JSON
// model. Must match the const used by grafana-bridge's DashboardStore
// (internal/bridge/grafana_dashboard_store.go) — the viewer reads what the
// bridge writes.
const dashboardModelDocLabel = "grafana:dashboard-model"

// dashboardUIDRe is the Grafana dashboard-uid charset: letters, digits, and
// `-._`. Used to validate a uid before it goes into an iframe URL so a
// hand-crafted directive can never inject path/query/script.
var dashboardUIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// dashboardView is the per-type view model for `dashboard` beads.
type dashboardView struct {
	UID     string
	Title   string
	Folder  string
	Version string
	Source  string // "beads" | "grafana"
	Hash    string // short (12-char) display form of dashboard_hash

	// EmbedURL is the same-origin /d/<uid> kiosk URL for the live iframe, or
	// "" when grafanaBaseURL is unset (degraded: no iframe, fallback only).
	EmbedURL template.URL
	// OpenURL is the full-page /d/<uid> deep link ("Open in Grafana ↗"), or "".
	OpenURL template.URL

	// ModelDocID is the type=doc child holding the JSON model ("" if none).
	ModelDocID string
	// Panels is the best-effort panel list parsed from the model, for the
	// no-Grafana fallback. Nil/empty → "model unavailable".
	Panels []dashboardModelPanel
	// ModelErr is a one-line reason the panel list is empty (parse/fetch).
	ModelErr string
}

// dashboardModelPanel is one row in the JSON-model panel-list fallback.
type dashboardModelPanel struct {
	ID    int
	Title string
	Type  string
}

// HasEmbed reports whether a live iframe should render.
func (v *dashboardView) HasEmbed() bool { return v != nil && v.EmbedURL != "" }

// buildDashboardView assembles the dashboard view model from the bead fields
// and (best-effort) the linked JSON-model doc. grafanaBase is the viewer's
// configured Grafana base URL (Server.grafanaBaseURL); when empty, the live
// embed is suppressed and only metadata + the panel-list fallback render.
//
// The model-doc fetch is best-effort: a daemon error or missing/unparseable
// model degrades to "model unavailable" and never fails the page. daemon may
// be nil (tests that only exercise the metadata/URL path).
func buildDashboardView(ctx context.Context, bead *beadsapi.BeadDetail, daemon dashboardModelReader, grafanaBase string) *dashboardView {
	if bead == nil {
		return nil
	}
	f := bead.Fields
	title := firstNonEmpty(f, "title")
	if title == "" {
		title = bead.Title
	}
	v := &dashboardView{
		UID:     strings.TrimSpace(f["dashboard_uid"]),
		Title:   strings.TrimSpace(title),
		Folder:  strings.TrimSpace(f["dashboard_folder"]),
		Version: strings.TrimSpace(f["dashboard_version"]),
		Source:  strings.TrimSpace(f["dashboard_source"]),
		Hash:    shortHash(f["dashboard_hash"]),
	}

	if v.UID != "" && dashboardUIDRe.MatchString(v.UID) && grafanaBase != "" {
		base := strings.TrimRight(grafanaBase, "/")
		// /d/<uid> in kiosk mode = full dashboard, chrome-free, for the iframe.
		v.EmbedURL = template.URL(base + "/d/" + v.UID + "?kiosk") //nolint:gosec // base is operator-set env; uid is charset-validated.
		v.OpenURL = template.URL(base + "/d/" + v.UID)             //nolint:gosec // same.
	}

	// Best-effort model-doc fetch + panel extraction.
	if daemon != nil && bead.ID != "" {
		docID, model, err := fetchDashboardModel(ctx, daemon, bead.ID)
		switch {
		case err != nil:
			v.ModelErr = "model fetch failed"
		case docID == "":
			v.ModelErr = "no model attached"
		default:
			v.ModelDocID = docID
			panels, perr := extractDashboardPanels(model)
			if perr != nil {
				v.ModelErr = "model unparseable"
			} else {
				v.Panels = panels
			}
		}
	}
	return v
}

// dashboardModelReader is the slice of beadAPI buildDashboardView needs to find
// and read a dashboard's model doc. *beadsapi.Client and the viewer's beadAPI
// both satisfy it; tests supply a fake.
type dashboardModelReader interface {
	GetReverseDependencies(ctx context.Context, id string) ([]beadsapi.Dependency, error)
	GetBead(ctx context.Context, id string) (*beadsapi.BeadDetail, error)
}

// fetchDashboardModel finds the type=doc child labeled grafana:dashboard-model
// linked parent-child under dashboardID and returns (docID, modelBytes).
// Returns ("", nil, nil) when no model doc exists. Mirrors the bridge's
// DashboardStore.ModelDoc traversal so the viewer reads exactly what the bridge
// writes (the model lives in the doc's "content" field).
func fetchDashboardModel(ctx context.Context, daemon dashboardModelReader, dashboardID string) (docID string, model []byte, err error) {
	deps, err := daemon.GetReverseDependencies(ctx, dashboardID)
	if err != nil {
		return "", nil, fmt.Errorf("dashboard view: deps of %s: %w", dashboardID, err)
	}
	for _, d := range deps {
		if d.Type != "parent-child" {
			continue
		}
		child, gerr := daemon.GetBead(ctx, d.BeadID)
		if gerr != nil || child == nil || child.Type != "doc" {
			continue
		}
		if !hasDashboardModelLabel(child.Labels) {
			continue
		}
		return child.ID, []byte(child.Fields["content"]), nil
	}
	return "", nil, nil
}

func hasDashboardModelLabel(labels []string) bool {
	return slices.Contains(labels, dashboardModelDocLabel)
}

// extractDashboardPanels parses a Grafana dashboard JSON model and returns its
// panels (id, title, type) for the fallback list. Grafana nests panels under
// row panels (type=="row" with a `panels` array) in older schemas; we flatten
// one level so row children are included. Best-effort: returns an error only
// when the model is not a JSON object.
func extractDashboardPanels(model []byte) ([]dashboardModelPanel, error) {
	if len(model) == 0 {
		return nil, fmt.Errorf("empty model")
	}
	var doc struct {
		Panels []struct {
			ID     int    `json:"id"`
			Title  string `json:"title"`
			Type   string `json:"type"`
			Panels []struct {
				ID    int    `json:"id"`
				Title string `json:"title"`
				Type  string `json:"type"`
			} `json:"panels"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(model, &doc); err != nil {
		return nil, fmt.Errorf("dashboard model is not valid JSON: %w", err)
	}
	var out []dashboardModelPanel
	for _, p := range doc.Panels {
		if p.Type == "row" {
			// A row groups panels; include the row label then its children.
			if p.Title != "" {
				out = append(out, dashboardModelPanel{ID: p.ID, Title: p.Title, Type: "row"})
			}
			for _, c := range p.Panels {
				out = append(out, dashboardModelPanel{ID: c.ID, Title: c.Title, Type: c.Type})
			}
			continue
		}
		out = append(out, dashboardModelPanel{ID: p.ID, Title: p.Title, Type: p.Type})
	}
	return out, nil
}

// shortHash trims a sha256 hex string to its first 12 chars for display.
func shortHash(h string) string {
	h = strings.TrimSpace(h)
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// --- Embeddable directive -------------------------------------------------

// grafanaDirectiveRe matches a standalone embed directive, optionally wrapped
// in a <p> by goldmark. Captures: uid, then the remaining option string
// (panel=, height=, var-<name>=). The directive must occupy its own
// line/paragraph.
//
//	:::grafana <uid>
//	:::grafana <uid> panel=3
//	:::grafana <uid> panel=3 height=240
//	:::grafana <uid> panel=3 var-agent=gardenio-foo-123
//
// The opening tag pattern `<p(?:\s[^>]*)?>` tolerates a `<p>` carrying
// attributes — e.g. the `data-block-id="blk-N"` the block-id transformer
// (crit-critique T1) now stamps on every paragraph — so a directive on its own
// paragraph still matches.
//
// Option keys are lowercase identifiers (the `var-<name>` template-var form is
// allowed via the `[a-z][a-z0-9_-]*` key pattern); option values allow `.`/`-`
// so dashboard-variable values such as agent names (`gardenio-foo-123`) and
// project slugs pass through. A value containing a char outside this set (e.g.
// `/`, `:`, a space) fails the whole-directive match, so it degrades to plain
// text rather than emitting a malformed embed.
var grafanaDirectiveRe = regexp.MustCompile(`(?m)^(?:<p(?:\s[^>]*)?>)?\s*:::grafana\s+([A-Za-z0-9._-]{1,64})((?:\s+[a-z][a-z0-9_-]*=[A-Za-z0-9._-]+)*)\s*(?:</p>)?\s*$`)

// grafanaEmbedBaseURL resolves the Grafana base URL for the directive
// post-pass from the GRAFANA_BASE_URL env var — the same source
// Server.grafanaBaseURL reads. Empty (the default in envs without Grafana)
// means directives degrade to a link card. Reading env here mirrors how the
// linkify package resolves jiraBaseURL(), so every linkified surface
// (descriptions, comments, bundle md docs) gets embeds without threading the
// config through every call site.
func grafanaEmbedBaseURL() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("GRAFANA_BASE_URL")), "/")
}

// embedGrafanaDirectives rewrites every :::grafana directive in the rendered
// HTML to a sandboxed iframe (or a quiet link card when grafanaBase is unset).
// It runs as a post-pass inside LinkifyHTMLWithOpts so every linkified surface
// — bead descriptions, comments, bundle md docs — gets embeds for free.
//
// When grafanaBase is empty the directive degrades to a link-card-style note
// ("📊 Grafana dashboard <uid>") rather than an iframe pointed nowhere.
func embedGrafanaDirectives(htmlIn, grafanaBase string) string {
	if !strings.Contains(htmlIn, ":::grafana") {
		return htmlIn
	}
	base := strings.TrimRight(grafanaBase, "/")
	expand := func(s string) string {
		return grafanaDirectiveRe.ReplaceAllStringFunc(s, func(m string) string {
			sub := grafanaDirectiveRe.FindStringSubmatch(m)
			if sub == nil {
				return m
			}
			uid := sub[1]
			opts := parseGrafanaEmbedOpts(sub[2])
			return grafanaembed.HTML(base, uid, opts)
		})
	}
	// A code block's text is verbatim author text: a `:::grafana` line inside
	// one must NOT be expanded into an embed. In a mermaid diagram it would
	// corrupt the diagram and litter the no-JS fallback (ka-0000005977); in a
	// fenced block it is quoted, often someone else's, text (a Gate
	// applicant's purpose, gd-ZwnwonBxGS). Expand only the regions OUTSIDE
	// any <pre>, leaving code and diagram source intact.
	var b strings.Builder
	last := 0
	for _, loc := range preBlockRe.FindAllStringIndex(htmlIn, -1) {
		b.WriteString(expand(htmlIn[last:loc[0]]))
		b.WriteString(htmlIn[loc[0]:loc[1]])
		last = loc[1]
	}
	b.WriteString(expand(htmlIn[last:]))
	return b.String()
}

// parseGrafanaEmbedOpts extracts panel=, height=, and var-<name>= from the
// option tail. Unknown keys are ignored; invalid values fall back to defaults.
// var-<name>= tokens are collected in order into Vars; a `var-` token with an
// empty name (just `var-=x`) is ignored.
func parseGrafanaEmbedOpts(tail string) grafanaembed.Opts {
	var o grafanaembed.Opts
	for tok := range strings.FieldsSeq(tail) {
		k, val, ok := strings.Cut(tok, "=")
		if !ok {
			continue
		}
		switch {
		case k == "panel":
			if _, err := strconv.Atoi(val); err == nil {
				o.PanelID = val
			}
		case k == "height":
			o.Height = val
		case strings.HasPrefix(k, "var-"):
			if name := k[len("var-"):]; name != "" {
				o.Vars = append(o.Vars, grafanaembed.Var{Name: name, Val: val})
			}
		}
	}
	return o
}
