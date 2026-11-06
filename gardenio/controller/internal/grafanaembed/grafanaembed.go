// Package grafanaembed builds the sandboxed Grafana iframe every gardenio
// surface embeds a dashboard or a single panel with: the beads viewer's /costs
// cards, the /charts hub heroes and the `:::grafana` directive, and the /garden
// costs charts (gardenio/docs/garden-grafana-panels.md, plan gd-gh5J0vVbJP).
// It was lifted out of cmd/beads-viewer (gd-4sF22T1are) so internal/gardenhub
// can build the same markup; testdata/grafana_embed.golden in the viewer pins
// that the viewer surfaces kept their bytes.
//
// Two layers:
//
//   - HTML / URLs take a base, a uid and Opts. The viewer's callers already
//     validate uid (dashboardUIDRe or the directive regex) and panel id.
//   - Build (garden.go) takes only a set name, a project, a range and a theme,
//     each checked against a closed set, and looks the uid and panel ids up in
//     the registry. That is the only path a garden route may use.
package grafanaembed

import (
	"fmt"
	"html"
	"net/url"
	"strconv"
	"strings"
)

// DefaultHeight / PanelHeight are the iframe heights (px) for a whole dashboard
// vs a single panel when Opts.Height is unset; MaxHeight clamps Opts.Height.
const (
	DefaultHeight = 420
	PanelHeight   = 260
	MaxHeight     = 1200
)

// Var is one Grafana dashboard template-variable assignment, emitted as
// `&var-<Name>=<urlencoded Val>`. Callers keep them in an ordered slice (not a
// map) so the emitted URL query order is deterministic across renders.
type Var struct {
	Name string // template var name (the part after `var-`)
	Val  string // raw value; URL-encoded by the builder
}

// Opts holds one embed's options.
type Opts struct {
	PanelID string // numeric, "" for whole-dashboard
	Height  string // pixel int, defaults applied by builder
	// Vars are template-variable assignments passed through to Grafana as
	// `var-<name>=<value>` URL params. Enables an embed to be scoped to one
	// entity (e.g. var-agent=<name>) instead of fleet-wide.
	Vars []Var
	// Theme, From, To and Timezone become `&theme=`, `&from=`, `&to=` and
	// `&timezone=` on both URLs, ahead of the vars. Empty leaves the param out,
	// which is what the viewer surfaces do, so their URLs are unchanged.
	Theme    string
	From     string
	To       string
	Timezone string
}

// query renders the theme/time params then the vars, each as `&k=v`.
func (o Opts) query() string {
	var b strings.Builder
	for _, kv := range [...][2]string{
		{"theme", o.Theme},
		{"from", o.From},
		{"to", o.To},
		{"timezone", o.Timezone},
	} {
		if kv[1] != "" {
			b.WriteString("&" + kv[0] + "=" + url.QueryEscape(kv[1]))
		}
	}
	for _, v := range o.Vars {
		b.WriteString("&var-")
		b.WriteString(v.Name)
		b.WriteByte('=')
		b.WriteString(url.QueryEscape(v.Val))
	}
	return b.String()
}

// URLs returns the iframe src and the Open↗ URL for one embed. A panel embed is
// the solo render `<base>/d-solo/<uid>?panelId=N&kiosk...` with the pop-out on
// `<base>/d/<uid>?viewPanel=N...`; a whole-dashboard embed is
// `<base>/d/<uid>?kiosk...`. The same query tail goes on both, so the pop-out
// lands on the same filtered view. Both are "" when base is empty.
func URLs(base, uid string, o Opts) (src, openURL string) {
	if base == "" {
		return "", ""
	}
	q := o.query()
	if o.PanelID != "" {
		return base + "/d-solo/" + uid + "?panelId=" + o.PanelID + "&kiosk" + q,
			base + "/d/" + uid + "?viewPanel=" + o.PanelID + q
	}
	openURL = base + "/d/" + uid
	if q != "" {
		// Whole-dashboard open URL has no query yet; start one.
		openURL += "?" + strings.TrimPrefix(q, "&")
	}
	return base + "/d/" + uid + "?kiosk" + q, openURL
}

// Unconfigured is the quiet, escaped note an embed degrades to when no Grafana
// base URL is configured, instead of an iframe pointed nowhere.
func Unconfigured(uid string) string {
	return `<div class="grafana-embed grafana-embed--unconfigured">📊 Grafana dashboard <code>` +
		html.EscapeString(uid) + `</code> <span class="grafana-embed-note">(Grafana not configured in this environment)</span></div>`
}

// HTML builds the iframe (or the Unconfigured card when base is empty) for one
// embed. uid must already be charset-validated by the caller; height is
// clamped to MaxHeight and every attribute value is HTML-escaped.
func HTML(base, uid string, o Opts) string {
	if base == "" {
		return Unconfigured(uid)
	}
	src, openURL := URLs(base, uid, o)

	height := DefaultHeight
	if o.PanelID != "" {
		height = PanelHeight
	}
	if o.Height != "" {
		if h, err := strconv.Atoi(o.Height); err == nil && h > 0 {
			height = min(h, MaxHeight)
		}
	}

	// src/openURL are built from operator-set base + validated uid/panel; we
	// still HTML-escape them as attribute values for defense in depth.
	esrc := html.EscapeString(src)
	eopen := html.EscapeString(openURL)
	euid := html.EscapeString(uid)
	label := "Grafana dashboard"
	if o.PanelID != "" {
		label = "Grafana panel " + html.EscapeString(o.PanelID)
	}
	return fmt.Sprintf(
		`<div class="grafana-embed">`+
			`<div class="grafana-embed-bar"><span class="grafana-embed-label">%s <code>%s</code></span>`+
			`<a class="grafana-embed-open" href="%s" target="_blank" rel="noopener">Open ↗</a></div>`+
			`<iframe class="grafana-embed-frame" src="%s" height="%d" loading="lazy" scrolling="no" `+
			`sandbox="allow-scripts allow-same-origin allow-popups" `+
			`referrerpolicy="same-origin" title="%s %s"></iframe></div>`,
		label, euid, eopen, esrc, height, label, euid,
	)
}
