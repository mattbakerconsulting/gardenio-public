// Command group `gb grafana` — agent-facing affordances for the beads↔Grafana
// dashboard loop (epic ka-0000006776).
//
// `gb grafana render` closes the author→render→look→edit loop entirely inside
// an agent session: it renders the current state of a dashboard (authored as a
// type=dashboard bead, or addressed by raw Grafana uid) to a PNG on local disk
// so the agent can `Read` it (Claude decodes PNG natively), and optionally
// uploads the PNG to the viewer S3 bucket and attaches it to the dashboard
// bead. No human in the loop to eyeball Grafana.
//
// It drives Grafana's server-side image-renderer:
//
//	GET <base>/render/d/<uid>            → whole dashboard
//	GET <base>/render/d-solo/<uid>?panelId=<n>  → a single panel
//
// reached through the in-cluster Grafana Service (GRAFANA_URL — auto-injected
// into agent pods; in subpath mode this carries the /grafana subpath so the
// render hits the working base, bug ka-0000001440) or the public AVA-fronted
// base (GRAFANA_BASE_URL — the same URL beads-viewer embeds, also carrying the
// /grafana subpath). The image-renderer sidecar is feature-flagged in the chart
// (grafana.imageRenderer.enabled, bead ka-0000001982); when it is not deployed
// /render either 404s or — on some Grafana builds (e.g. 11.4.0) — returns HTTP
// 200 with a tiny "No image renderer available" placeholder PNG. This command
// detects both cases and fails loudly rather than writing a broken file.
//
// In-cluster Grafana runs the AVA auth-proxy with anonymous + the login form
// disabled, so an unauthenticated render 302-loops to /login. The controller
// injects GRAFANA_AUTH_PROXY_HEADER (the trusted header name) into agent pods;
// applyGrafanaAuth stamps it with the operator identity so the render passes
// the auth-proxy (bug ka-0000005842).
//
// Design: gardenio/docs/research/obs-w2-grafana-bridge.md,
// gardenio/docs/research/obs-w2c-render-dashboards-in-viewer.md.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gardenio/controller/internal/bridge"

	"github.com/spf13/cobra"
)

// defaultRenderTimeout bounds the whole render request. Grafana's renderer can
// be slow on cold panels, so this is generous compared to the daemon client.
const defaultRenderTimeout = 60 * time.Second

// renderHTTPClient is the production hook for the render GET; tests override it
// to avoid standing up an HTTP server.
var renderHTTPClient = func() *http.Client {
	return &http.Client{Timeout: defaultRenderTimeout}
}

// ── gb grafana ──────────────────────────────────────────────────────────

var grafanaCmd = &cobra.Command{
	Use:     "grafana",
	Short:   "Render and iterate on Grafana dashboards authored as beads",
	GroupID: "orchestration",
	Long: `Agent-facing affordances for the beads↔Grafana dashboard loop.

The grafana-bridge lets agents author dashboards as beads (type=dashboard). To
iterate on one, an agent needs to render its current state and SEE it. ` +
		"`gb grafana render`" + ` does exactly that: it writes a PNG locally so the
agent can Read it, closing the author→render→look→edit loop without a human.`,
}

// ── gb grafana render ─────────────────────────────────────────────────────

var grafanaRenderCmd = &cobra.Command{
	Use:   "render <dashboard-bead-id | grafana-uid>",
	Short: "Render a dashboard (or single panel) to a local PNG",
	Long: `Render a Grafana dashboard to a PNG on local disk and print its path.

The argument is either a dashboard bead id (type=dashboard, whose dashboard_uid
field names the Grafana dashboard) or a raw Grafana dashboard uid. Pass --uid to
force raw-uid interpretation.

By default the whole dashboard is rendered; --panel <id> renders a single panel
(Grafana's d-solo route). --from/--to set the time range (Grafana relative or
absolute, e.g. now-6h / now), --var k=v sets a dashboard template variable
(repeatable), and --width/--height/--theme/--tz tune the image.

The PNG is written to --out (default: a temp file) so you can Read it. With
--upload the PNG is also pushed to the viewer S3 bucket; combined with --bead it
is attached to the dashboard bead as a doc (same path as 'gb image upload').

Examples:
  gb grafana render ka-Dash1234
  gb grafana render my-grafana-uid --uid --panel 4 --from now-24h
  gb grafana render ka-Dash1234 --var datasource=vmsingle --theme light
  gb grafana render ka-Dash1234 --upload --bead ka-Dash1234`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		target := args[0]
		forceUID, _ := cmd.Flags().GetBool("uid")
		panel, _ := cmd.Flags().GetString("panel")
		from, _ := cmd.Flags().GetString("from")
		to, _ := cmd.Flags().GetString("to")
		vars, _ := cmd.Flags().GetStringArray("var")
		width, _ := cmd.Flags().GetInt("width")
		height, _ := cmd.Flags().GetInt("height")
		theme, _ := cmd.Flags().GetString("theme")
		tz, _ := cmd.Flags().GetString("tz")
		outPath, _ := cmd.Flags().GetString("out")
		baseFlag, _ := cmd.Flags().GetString("grafana-url")
		upload, _ := cmd.Flags().GetBool("upload")
		beadFlag, _ := cmd.Flags().GetString("bead")

		base := grafanaRenderBaseURL(baseFlag)
		if base == "" {
			return fmt.Errorf("no Grafana base URL: set GRAFANA_URL (in-cluster) or GRAFANA_BASE_URL (public), or pass --grafana-url")
		}

		// Resolve the argument to a Grafana uid. A bead id is dereferenced to
		// its dashboard_uid field; a raw uid passes through.
		uid, dashboardBead, err := resolveDashboardUID(cmd.Context(), target, forceUID)
		if err != nil {
			return err
		}
		// --bead defaults to the dashboard bead we resolved through, so
		// `--upload --bead` "just works" when rendering by bead id.
		if beadFlag == "" {
			beadFlag = dashboardBead
		}

		opts := renderOptions{
			panel:  strings.TrimSpace(panel),
			from:   strings.TrimSpace(from),
			to:     strings.TrimSpace(to),
			vars:   vars,
			width:  width,
			height: height,
			theme:  strings.TrimSpace(theme),
			tz:     strings.TrimSpace(tz),
		}
		renderURL, err := buildRenderURL(base, uid, opts)
		if err != nil {
			return err
		}

		png, err := fetchRender(cmd.Context(), renderURL, opts.width, opts.height)
		if err != nil {
			return err
		}

		if outPath == "" {
			outPath = defaultRenderOutPath(uid, opts.panel)
		}
		if err := os.WriteFile(outPath, png, 0o644); err != nil {
			return fmt.Errorf("writing PNG to %s: %w", outPath, err)
		}

		var uploadURL string
		if upload {
			uploadURL, err = uploadRenderedPNG(cmd.Context(), outPath, png, beadFlag)
			if err != nil {
				return fmt.Errorf("uploading rendered PNG: %w", err)
			}
		}

		if jsonOutput {
			out := map[string]any{
				"path":  outPath,
				"uid":   uid,
				"bytes": len(png),
				"url":   renderURL,
			}
			if dashboardBead != "" {
				out["bead"] = dashboardBead
			}
			if uploadURL != "" {
				out["upload_url"] = uploadURL
			}
			printJSON(out)
			return nil
		}

		fmt.Println(outPath)
		fmt.Fprintf(os.Stderr, "Rendered %d bytes from %s\n", len(png), renderURL)
		if uploadURL != "" {
			fmt.Fprintf(os.Stderr, "Uploaded: %s\n", uploadURL)
		}
		return nil
	},
}

// renderOptions captures the tunable render parameters.
type renderOptions struct {
	panel  string   // panel id → d-solo single-panel route when non-empty
	from   string   // Grafana time range start (e.g. now-6h)
	to     string   // Grafana time range end (e.g. now)
	vars   []string // template variables as "key=value" → var-key=value
	width  int
	height int
	theme  string // light | dark
	tz     string // IANA timezone (e.g. UTC, America/New_York)
}

// grafanaRenderBaseURL resolves the Grafana root to render against. An explicit
// flag wins; then GRAFANA_URL (the in-cluster Service, no AVA hop), then
// GRAFANA_BASE_URL (the public AVA-fronted base beads-viewer embeds). The
// trailing slash is trimmed so callers can append "/render/...".
func grafanaRenderBaseURL(flag string) string {
	for _, v := range []string{flag, os.Getenv("GRAFANA_URL"), os.Getenv("GRAFANA_BASE_URL")} {
		if s := strings.TrimRight(strings.TrimSpace(v), "/"); s != "" {
			return s
		}
	}
	return ""
}

// resolveDashboardUID turns the CLI argument into a Grafana uid. When forceUID
// is false it first tries to dereference the argument as a dashboard bead: if
// the bead exists and is type=dashboard, its dashboard_uid field is returned
// (along with the bead id, so callers can default --bead). A bead that exists
// but is not a dashboard is a hard error. If the lookup fails (e.g. the arg is
// not a bead id), the argument is treated as a raw Grafana uid.
func resolveDashboardUID(ctx context.Context, arg string, forceUID bool) (uid, beadID string, err error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return "", "", fmt.Errorf("empty dashboard id/uid")
	}
	if forceUID || daemon == nil {
		return arg, "", nil
	}
	bead, gerr := daemon.GetBead(ctx, arg)
	if gerr != nil || bead == nil {
		// Not a resolvable bead — treat as a raw Grafana uid.
		return arg, "", nil
	}
	if bead.Type != "dashboard" {
		return "", "", fmt.Errorf("bead %s is type=%s, not a dashboard; pass --uid to render a raw Grafana uid", arg, bead.Type)
	}
	u := strings.TrimSpace(bead.Fields["dashboard_uid"])
	if u == "" {
		return "", "", fmt.Errorf("dashboard bead %s has no dashboard_uid field yet (not synced to Grafana?)", arg)
	}
	return u, bead.ID, nil
}

// buildRenderURL assembles the Grafana image-renderer URL. With a panel it uses
// the d-solo single-panel route; otherwise the whole-dashboard /render/d route.
// Width/height/theme/tz/time-range and template vars become query parameters.
func buildRenderURL(base, uid string, opts renderOptions) (string, error) {
	uid = strings.TrimSpace(uid)
	if uid == "" {
		return "", fmt.Errorf("empty Grafana uid")
	}
	// Guard against an arg that smuggles a path/query — the uid is a single
	// opaque path segment.
	if strings.ContainsAny(uid, "/?#") {
		return "", fmt.Errorf("invalid Grafana uid %q (must be a single path segment)", uid)
	}

	base = strings.TrimRight(base, "/")

	var path string
	if opts.panel != "" {
		path = "/render/d-solo/" + url.PathEscape(uid)
	} else {
		path = "/render/d/" + url.PathEscape(uid)
	}

	q := url.Values{}
	if opts.panel != "" {
		q.Set("panelId", opts.panel)
	}
	if opts.from != "" {
		q.Set("from", opts.from)
	}
	if opts.to != "" {
		q.Set("to", opts.to)
	}
	if opts.width > 0 {
		q.Set("width", strconv.Itoa(opts.width))
	}
	if opts.height > 0 {
		q.Set("height", strconv.Itoa(opts.height))
	}
	if opts.theme != "" {
		q.Set("theme", opts.theme)
	}
	if opts.tz != "" {
		q.Set("tz", opts.tz)
	}
	for _, kv := range opts.vars {
		k, v, ok := strings.Cut(kv, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return "", fmt.Errorf("invalid --var %q (want key=value)", kv)
		}
		q.Add("var-"+k, v)
	}

	full := base + path
	if encoded := q.Encode(); encoded != "" {
		full += "?" + encoded
	}
	return full, nil
}

// fetchRender GETs the render URL, attaching auth (GRAFANA_TOKEN Bearer when
// set, else the auth-proxy identity header), and returns the PNG bytes. It maps the
// common "renderer not deployed" 404 to an actionable error, and — because some
// Grafana builds (e.g. 11.4.0) return HTTP 200 with a tiny "No image renderer
// available" placeholder card instead of 404 — also detects that placeholder on
// a 200 response so a broken image is never written and reported as success.
// reqW/reqH are the requested image dimensions; a render that ignores them is a
// strong signal the placeholder card was returned.
func fetchRender(ctx context.Context, renderURL string, reqW, reqH int) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, defaultRenderTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, renderURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building render request: %w", err)
	}
	req.Header.Set("Accept", "image/png")
	applyGrafanaAuth(req)

	resp, err := renderHTTPClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("grafana render request: %w", err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("grafana render returned 404 — the image-renderer sidecar may not be deployed (chart flag, bead ka-0000005807), or the uid is wrong: %s", snippetBody(body))
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("grafana render: status %d: %s", resp.StatusCode, snippetBody(body))
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("grafana render: empty response body")
	}
	if reason := missingRendererReason(body, reqW, reqH); reason != "" {
		return nil, fmt.Errorf("grafana render returned a placeholder image, not a real render (%s) — the Grafana Image Renderer plugin is not installed/available. Install the image-renderer plugin or deploy the renderer sidecar (chart flag, bead ka-0000005807)", reason)
	}
	return body, nil
}

// placeholderMaxBytes is an upper bound on the size of Grafana's "No image
// renderer available" placeholder PNG. The real card observed in prod is ~9KB;
// a full dashboard render (any non-trivial width/height) is far larger. We use
// this only to corroborate the dimension heuristic, never as the sole signal.
const placeholderMaxBytes = 20 << 10

// missingRendererReason inspects a 200-OK render body and returns a non-empty
// reason string when it looks like the renderer-missing placeholder rather than
// a genuine dashboard render. It returns "" when the body looks like a real
// render. Detection uses two robust, version-independent signals:
//
//  1. Not a PNG at all — the renderer-missing path on some builds returns HTML
//     or JSON; a body lacking the PNG magic was never a successful render.
//  2. A PNG whose intrinsic dimensions ignore the requested width/height while
//     the body is suspiciously small. The placeholder card is a fixed-size
//     image that does not honour the &width=&height= query params, so a tiny
//     PNG that is materially smaller than requested is the placeholder.
func missingRendererReason(body []byte, reqW, reqH int) string {
	if !bytes.HasPrefix(body, pngMagic) {
		return fmt.Sprintf("response is not a PNG (%d bytes, starts %q)", len(body), snippetBody(body[:min(len(body), 16)]))
	}
	w, h, ok := pngDimensions(body)
	if !ok {
		// PNG magic but no parseable IHDR — be conservative and accept it
		// rather than risk a false positive on an unusual-but-valid encoding.
		return ""
	}
	// A genuine whole-dashboard / panel render honours the requested geometry.
	// The placeholder is a small fixed card that ignores it. Flag only when the
	// PNG is both materially smaller than requested AND the body is tiny, so a
	// caller who intentionally asks for a small image is not falsely tripped.
	if reqW > 0 && reqH > 0 && w < reqW && h < reqH && len(body) <= placeholderMaxBytes {
		return fmt.Sprintf("%dx%d PNG for a %dx%d request, %d bytes", w, h, reqW, reqH, len(body))
	}
	return ""
}

// pngMagic is the 8-byte PNG file signature.
var pngMagic = []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

// pngDimensions reads width/height from a PNG's IHDR chunk without decoding the
// pixels. The IHDR chunk always immediately follows the signature: 4-byte
// length, "IHDR", then width and height as big-endian uint32s. Returns ok=false
// for anything that does not match this layout.
func pngDimensions(b []byte) (w, h int, ok bool) {
	// 8 (sig) + 4 (len) + 4 ("IHDR") + 4 (w) + 4 (h) = 24 bytes minimum.
	if len(b) < 24 || !bytes.Equal(b[12:16], []byte("IHDR")) {
		return 0, 0, false
	}
	w = int(binary.BigEndian.Uint32(b[16:20]))
	h = int(binary.BigEndian.Uint32(b[20:24]))
	if w == 0 || h == 0 {
		return 0, 0, false
	}
	return w, h, true
}

// applyGrafanaAuth attaches credentials to a render request, in precedence:
//
//  1. GRAFANA_TOKEN service-account token → Authorization: Bearer (wins; the
//     public AVA path uses this).
//  2. The in-cluster auth-proxy identity header. Grafana in gardenio runs with
//     GF_AUTH_PROXY_ENABLED + GF_AUTH_ANONYMOUS_ENABLED=false, so an
//     unauthenticated request 302-loops to /login. The controller injects
//     GRAFANA_AUTH_PROXY_HEADER (the header name Grafana trusts, e.g.
//     X-Auth-Request-Email) into agent pods; we stamp it with the operator
//     identity (grafanaAuthProxyIdentity) so the render authenticates as the
//     signed-in user — the same path a raw curl with the header takes.
//
// With neither set the request goes out bare — fine for an in-cluster Grafana with
// anonymous access. See bug ka-0000005842 §2.
func applyGrafanaAuth(req *http.Request) {
	if tok := strings.TrimSpace(os.Getenv("GRAFANA_TOKEN")); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
		return
	}
	if header := strings.TrimSpace(os.Getenv("GRAFANA_AUTH_PROXY_HEADER")); header != "" {
		if id := grafanaAuthProxyIdentity(); id != "" {
			req.Header.Set(header, id)
		}
	}
}

// grafanaAuthProxyIdentity resolves the value to stamp on the auth-proxy header.
// An explicit GRAFANA_USER override wins; otherwise it derives the operator's
// email from the standard attribution chain (BEADS_OPERATED_BY / credstore),
// stripping the "oidc:" / "slack:" scheme prefix since Grafana's auth-proxy
// header carries a bare identity (an email under the default
// X-Auth-Request-Email). Returns "" when no identity is resolvable.
func grafanaAuthProxyIdentity() string {
	if u := strings.TrimSpace(os.Getenv("GRAFANA_USER")); u != "" {
		return u
	}
	ob := strings.TrimSpace(defaultOperatedBy())
	if ob == "" {
		return ""
	}
	// "oidc:matthew.baker@acme.example" → "matthew.baker@acme.example".
	if _, rest, ok := strings.Cut(ob, ":"); ok {
		return rest
	}
	return ob
}

// defaultRenderOutPath builds a temp-file path for the PNG. The panel id (when
// present) is folded into the name so per-panel renders don't clobber each
// other. We avoid a timestamp so re-rendering the same target overwrites in
// place — the agent's iterate loop wants the latest, not a pile of files.
func defaultRenderOutPath(uid, panel string) string {
	name := "grafana-" + sanitizeBasename(uid)
	if panel != "" {
		name += "-panel" + sanitizeBasename(panel)
	}
	name += ".png"
	return filepath.Join(os.TempDir(), name)
}

// uploadRenderedPNG pushes the PNG to the viewer S3 bucket (same layout as
// `gb image upload`) and, when beadID is set, scopes the upload under that
// bead. Returns the public URL.
func uploadRenderedPNG(ctx context.Context, path string, data []byte, beadID string) (string, error) {
	bucket := s3ImagesBucket()
	if bucket == "" {
		return "", fmt.Errorf("COOP_S3_BUCKET is not set; cannot --upload")
	}
	scope := imageScope(beadID, "", time.Now().UTC())
	key := imageKey(s3Prefix(), scope, filepath.Base(path), data)
	region := os.Getenv("COOP_S3_REGION")
	publicURL := imageURL(bucket, region, key)

	client, err := newS3Uploader(ctx)
	if err != nil {
		return "", err
	}
	putCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if _, err := client.PutObject(putCtx, putImageInput(bucket, key, data, "image/png")); err != nil {
		return "", fmt.Errorf("uploading to s3://%s/%s: %w", bucket, key, err)
	}
	return publicURL, nil
}

// ── gb grafana put ─────────────────────────────────────────────────────────

var grafanaPutCmd = &cobra.Command{
	Use:   "put <file.json>",
	Short: "Author a dashboard as a bead from a Grafana JSON model file",
	Long: `Author a Grafana dashboard as a bead in one shot from a JSON model file.

This is the authoring half of the dashboards-as-beads loop — the mirror of
'gb grafana render --bead'. It reads a Grafana dashboard JSON model from the
given file and creates everything 'gb grafana render --bead' (and the
grafana-bridge) expect:

  • a type=dashboard bead carrying the small metadata fields
    (dashboard_uid / dashboard_source=beads / dashboard_version) and the
    grafana:dashboard label;
  • a type=doc model bead carrying the JSON model in its content field,
    linked parent-child under the dashboard bead and labeled
    grafana:dashboard-model — exactly what grafana_dashboard_store.PutModel
    writes, so the model round-trips through ModelDoc/render.

The dashboard's uid is read from the JSON ("uid" key) — it is required, since
it is how the bead is matched to a Grafana dashboard. Re-running with the same
uid is idempotent: the existing dashboard bead is reused and its model doc
content is replaced in place (no duplicate beads), so 'put' doubles as an
"update the model" command.

--project sets the project:<p> label (default: $KD_PROJECT / $BOAT_PROJECT).
--title overrides the dashboard bead title (default: the model's "title").

Examples:
  gb grafana put my-dashboard.json
  gb grafana put my-dashboard.json --project gardenio --title "Cost Overview"
  gb grafana render --bead $(gb grafana put my-dashboard.json --json | jq -r .bead) --upload`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if daemon == nil {
			return fmt.Errorf("no beads daemon configured; set BEADS_HTTP_ADDR or pass --http-url")
		}

		path := args[0]
		model, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading dashboard JSON %s: %w", path, err)
		}

		meta, err := bridge.ParseDashboardMeta(model)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if strings.TrimSpace(meta.UID) == "" {
			return fmt.Errorf("dashboard JSON %s has no \"uid\" — a uid is required to author a dashboard bead", path)
		}

		project, _ := cmd.Flags().GetString("project")
		if project == "" {
			project = defaultProject()
		}
		if title, _ := cmd.Flags().GetString("title"); strings.TrimSpace(title) != "" {
			meta.Title = strings.TrimSpace(title)
		}

		store := bridge.NewDashboardStore(daemon, defaultActor())

		// Idempotent on uid: reuse an existing dashboard bead so a re-put just
		// refreshes the model, matching backfill semantics.
		ctx := cmd.Context()
		beadID, err := store.FindByUID(ctx, meta.UID)
		if err != nil {
			return fmt.Errorf("looking up dashboard by uid %q: %w", meta.UID, err)
		}
		created := beadID == ""
		if created {
			desc := "Grafana dashboard authored as a bead via 'gb grafana put'. uid=" + meta.UID
			beadID, err = store.CreateDashboardBead(ctx, meta, project, desc)
			if err != nil {
				return fmt.Errorf("creating dashboard bead: %w", err)
			}
		}

		// PutModel creates (or replaces) the type=doc model bead, links it
		// parent-child under the dashboard bead, and applies the
		// grafana:dashboard-model label — the exact shape render --bead reads.
		docID, err := store.PutModel(ctx, beadID, model)
		if err != nil {
			return fmt.Errorf("storing dashboard model: %w", err)
		}

		if jsonOutput {
			printJSON(map[string]any{
				"bead":      beadID,
				"model_doc": docID,
				"uid":       meta.UID,
				"title":     meta.Title,
				"created":   created,
			})
			return nil
		}

		fmt.Println(beadID)
		verb := "Updated"
		if created {
			verb = "Created"
		}
		fmt.Fprintf(os.Stderr, "%s dashboard bead %s (uid=%s), model doc %s\n", verb, beadID, meta.UID, docID)
		return nil
	},
}

// snippetBody trims a render error body for inclusion in a message.
func snippetBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func init() {
	grafanaCmd.AddCommand(grafanaRenderCmd)
	grafanaCmd.AddCommand(grafanaPutCmd)

	grafanaPutCmd.Flags().String("project", "", "project label for the dashboard bead (default: $KD_PROJECT / $BOAT_PROJECT)")
	grafanaPutCmd.Flags().String("title", "", "dashboard bead title (default: the model's \"title\" field)")

	grafanaRenderCmd.Flags().Bool("uid", false, "treat the argument as a raw Grafana uid, not a bead id")
	grafanaRenderCmd.Flags().String("panel", "", "render a single panel by id (d-solo route) instead of the whole dashboard")
	grafanaRenderCmd.Flags().String("from", "", "time range start (Grafana relative/absolute, e.g. now-6h)")
	grafanaRenderCmd.Flags().String("to", "", "time range end (e.g. now)")
	grafanaRenderCmd.Flags().StringArray("var", nil, "dashboard template variable as key=value (repeatable)")
	grafanaRenderCmd.Flags().Int("width", 1000, "image width in px")
	grafanaRenderCmd.Flags().Int("height", 500, "image height in px")
	grafanaRenderCmd.Flags().String("theme", "", "theme: light or dark (default: Grafana default)")
	grafanaRenderCmd.Flags().String("tz", "", "timezone (IANA, e.g. UTC, America/New_York)")
	grafanaRenderCmd.Flags().String("out", "", "output PNG path (default: a temp file)")
	grafanaRenderCmd.Flags().String("grafana-url", "", "override Grafana base URL (default: GRAFANA_URL or GRAFANA_BASE_URL)")
	grafanaRenderCmd.Flags().Bool("upload", false, "also upload the PNG to the viewer S3 bucket")
	grafanaRenderCmd.Flags().String("bead", "", "scope/attach the upload under this bead id (default: the rendered dashboard bead)")
}
