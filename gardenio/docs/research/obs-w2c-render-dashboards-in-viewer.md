# W2c — render Grafana dashboards in beads-viewer

> Phase 1 implementation doc for task [[ka-0000001078]] (workstream W2c, epic [[ka-0000006776]]).
> Builds directly on W2a ([[ka-0000000055]], merged MR !3964) which models a Grafana
> dashboard as a `type=dashboard` bead + a `type=doc` bundle child holding the JSON model.

## 1. Goal

W2a made a dashboard *editable as a bead*. W2c makes it *viewable in beads-viewer*:

1. **Bead view** — when viewing a `dashboard`-type bead, render the dashboard inline
   (panels), not just its metadata fields.
2. **Embeddable** — a markdown affordance so any bead description or bundle doc can inline
   a whole dashboard or a single panel, the way `gb image upload` embeds a screenshot —
   but live and dashboard-aware.

Operator ask (@matthewbaker, Slack 2026-06-09):
> "make sure we are planning to render dashboards in beads viewer in the bead view and in
> an embeddable way in other beads and bundles."

## 2. The auth path — same-origin iframe, no renderer sidecar

The task description floated two render paths: (a) live Grafana `d-solo` iframe behind AVA,
or (b) a server-side snapshot PNG via Grafana's `/render` image-renderer, and recommended
(b) as the default.

**We pick (a), the live `d-solo` iframe, as the default — and it is simpler, not harder,
in this deployment.** The reasoning:

- **The image-renderer is not deployed.** Grafana's `/render` endpoint requires the
  `grafana-image-renderer` plugin / sidecar, which the gardenio chart does not ship
  (`grafana.*` in `values.yaml` has a dashboard sidecar but no renderer). Path (b) would
  mean a new sidecar, a new headless-chromium image, and an internal render-auth hop —
  strictly more infra than the chart has today.
- **The viewer and Grafana already share one AVA-gated origin.** Under the unified host
  (`unifiedHost.paths.grafana = "/grafana"`, `grafana.auth.proxy.enabled = true`), Grafana
  is fronted by the same `gardenio-edge-auth` forwardAuth that fronts beads-viewer. The
  viewer already knows that public base URL: `Server.grafanaBaseURL` (env
  `GRAFANA_BASE_URL`), used today for the "Open in Grafana ↗" pill on `/charts`
  (task ka-0000006706). An `<iframe src="<grafanaBaseURL>/d-solo/...">` is **same-origin**
  with the viewer page, so the browser carries the AVA session cookie automatically —
  no second login, no signed render URL, no server-held service-account token.
- **Snapshot-PNG can be added later as an enhancement** for static contexts (a bead
  exported to a non-AVA reader). It is explicitly out of scope for this slice; the field
  shape below leaves room for it (`dashboard_snapshot_key`).

### 2.1 Iframe embedding prerequisites (Grafana side)

For a `d-solo` iframe to load, Grafana must allow being framed by its own origin and
permit anonymous-less embedding within the AVA session:

- `GF_SECURITY_ALLOW_EMBEDDING=true` — without it Grafana sets `X-Frame-Options: deny`.
- The viewer page and Grafana share the unified host, so `SAMEORIGIN` framing is
  sufficient; we do **not** need `cookie_samesite=none`.

These are **not** flipped in this MR (no chart change is required to land the viewer code —
the iframe simply shows Grafana's own "refused to connect" until embedding is enabled).
A follow-up chart task ([[ka-0000001078]] → child) wires `grafana.allowEmbedding` so the
flag rides the same `auth.proxy.enabled` opt-in. The viewer degrades cleanly when Grafana
is unreachable or framing is denied (see §5).

## 3. Bead view (`type=dashboard`)

`injectTypeView` (server.go) gains a `case "dashboard"` that builds a `dashboardView`
(type_views.go) from the bead's fields:

| Field | Used for |
|---|---|
| `dashboard_uid` | the `d-solo`/`/d/` URL path segment — the live embed target |
| `title` | iframe title + heading |
| `dashboard_folder` | shown as provenance |
| `dashboard_version` | shown as provenance |
| `dashboard_source` | `beads` \| `grafana` provenance badge |
| `dashboard_hash` | shown (short) as the sync token |

The `type_dashboard` partial renders, in order:

1. A **live embed** — `<iframe>` at `<grafanaBaseURL>/d/<uid>?kiosk&theme=...` (whole
   dashboard) when `grafanaBaseURL` is set, else an explanatory empty state.
2. A **metadata strip** (`<dl>`) — uid / folder / version / source / hash, plus an
   "Open in Grafana ↗" deep link and an "Edit JSON model" link to the model doc bead.
3. The **JSON-model fallback** is *always* available behind a `<details>`: a panel-list
   summary parsed from the model doc (panel titles + types), so even with no Grafana the
   bead conveys what the dashboard contains. This is the static-context answer.

The model doc is fetched the same way W2a's `DashboardStore.ModelDoc` finds it: the
`type=doc` child linked `doc → dashboard (parent-child)`, labeled
`grafana:dashboard-model`. The viewer reuses `GetReverseDependencies` + a `GetBead` on the
matching child, reading the JSON from the doc's description (where bundle docs store their
body). Panel extraction is best-effort and read-only — a parse failure degrades to "model
unavailable", never an error page.

## 4. Embeddable directive (other beads & bundles)

A markdown directive usable in any bead description or bundle doc:

```
:::grafana <uid>                  # whole dashboard
:::grafana <uid> panel=<panelId>  # single panel (d-solo)
:::grafana <uid> panel=3 height=240
```

It expands, at render time, to the same sandboxed iframe used by the bead view. This rides
the existing description/bundle markdown pipeline rather than inventing a new one:

- A goldmark-independent **post-pass** over the rendered HTML (`embedGrafanaDirectives`)
  recognises a fenced/standalone directive line and replaces it with the iframe HTML. It
  runs inside `LinkifyHTML*` so every surface that linkifies (bead descriptions, comments,
  bundle `md` renderer) gets embeds for free, and only when `grafanaBaseURL` is configured
  (otherwise the directive renders as a quiet "dashboard <uid>" link card — never a broken
  iframe).
- **Panel embeds** use `/d-solo/<uid>?panelId=<n>` (Grafana's single-panel solo route);
  whole-dashboard embeds use `/d/<uid>?kiosk`.

### 4.1 Why a directive and not raw HTML

Bead descriptions and bundle md docs are goldmark-rendered with `html.WithUnsafe()` **off**
(see bundle_renderers.go security note) — authors cannot inject raw `<iframe>`. The
directive is the *only* way to embed, and it only ever produces a URL we construct from a
validated uid + numeric panel id pointing at the one configured `grafanaBaseURL` origin.
An author can never aim the iframe at an arbitrary host. This mirrors how `/image/` proxy
URLs are the only embeddable image source.

## 5. Degraded modes (every one renders, none errors)

- **No `GRAFANA_BASE_URL`** (env without Grafana): no iframe; bead view shows metadata +
  JSON-model fallback; directive renders a link card. The viewer behaves identically to
  today for non-dashboard beads.
- **Grafana up but embedding denied** (`ALLOW_EMBEDDING` not yet flipped): the iframe loads
  Grafana's framing-refusal — visible, self-explanatory, and the "Open in Grafana ↗" link
  beside it always works. No viewer change needed once the chart flag lands.
- **Model doc missing / unparseable**: panel-list fallback shows "model unavailable"; the
  live embed (which only needs the uid from a bead field) is unaffected.
- **Malformed uid / panel id**: the directive validator rejects it and falls back to the
  link card; the uid charset is `[A-Za-z0-9._-]` (Grafana uid charset), panel id is digits.

## 6. Security

- **No new XSS surface.** The directive is the sole embedding path; goldmark stays
  `WithUnsafe()` off. The iframe `src` is built from `grafanaBaseURL` (operator-set env) +
  a charset-validated uid + a numeric panel id; nothing author-supplied reaches the DOM
  unescaped.
- **Same-origin by construction.** The iframe points only at the one configured Grafana
  base; an author cannot frame an external site.
- **AVA-gated.** Because the embed is same-origin under the unified host, Grafana enforces
  its own AVA forwardAuth + org-role on the framed request — a viewer with no Grafana
  access sees Grafana's own auth response inside the frame, not leaked panels.

## 7. Files

- `gardenio/controller/cmd/beads-viewer/dashboard_view.go` — `dashboardView` model, model-doc
  fetch + panel extraction, iframe-URL builder, directive expander.
- `gardenio/controller/cmd/beads-viewer/dashboard_view_test.go` — table tests for URL
  building, uid/panel validation, directive expansion, degraded modes.
- `gardenio/controller/cmd/beads-viewer/templates/partials/type_dashboard.html` — bead-view
  partial.
- wiring: `injectTypeView` (server.go) `case "dashboard"`; `bead_show.html` dispatch row;
  `LinkifyHTMLWithOpts` post-pass hook (linkify.go).

## 8. Related

- W2a impl [[ka-0000000055]] (MR !3964) · epic [[ka-0000006776]] · gate [[ka-0000004494]]
- Grafana-on-unified-host epic [[ka-0000007895]] · "Open in Grafana" task ka-0000006706
- Embeddable-artifacts precedent: `/image/` proxy (image_proxy.go), bundle renderers.
- Image-embed advice [[ka-0000002145]] (S3 reuse — relevant only if snapshot-PNG is added).
