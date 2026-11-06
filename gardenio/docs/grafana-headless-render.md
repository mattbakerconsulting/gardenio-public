# Grafana headless render — populated dashboard screenshots from an agent pod

How an agent (a GRADE-audit gardener, a thread agent grabbing a demo, any
headless pod) captures **populated** Grafana panel PNGs — real pixels with live
data, not blank frames and not "refused to connect".

Verified end-to-end from a gardenio agent pod against **prod** `gardenio-grafana`
on 2026-06-17 (task `ka-0000004768`, Slack thread `kd` Matt Baker: *"we need
those populated and we need to document how to do it in appropriate formulas
(any grafana related formulas) — and gardeners for that matter"*).

## TL;DR

```bash
# Subpath mode (ka-0000001440): Grafana serves everything under /grafana, so the
# in-cluster base carries the subpath. This is exactly what the GRAFANA_URL env
# already injected into your pod holds — prefer it over hand-building the base.
GRAF="${GRAFANA_URL:-http://gardenio-grafana.gardenio.svc.cluster.local:3000/grafana}"
H="X-Auth-Request-Email: someone@acme.example"               # stamp the auth-proxy header yourself

# whole-dashboard PNG (now works — full-page render fixed in ka-0000001440):
curl -fsS -H "$H" -o dash.png \
  "$GRAF/render/d/<uid>/x?width=1200&height=800&theme=dark&from=now-24h&to=now"

# or one populated panel:
curl -fsS -H "$H" -o panel.png \
  "$GRAF/render/d-solo/<uid>/x?panelId=<N>&width=700&height=350&theme=dark&from=now-24h&to=now"
```

The full-page `/render/d/<uid>` route now returns a real PNG (it used to break
under our `/grafana` subpath — see Gotchas). Per-panel `/render/d-solo` still
works and tiling solo panels remains useful for custom montages.

## Why the previous screenshots were blank — the actual mechanism

Grafana here has **no login of its own**. It runs in header auth-proxy mode
(`GF_AUTH_PROXY_ENABLED=true`, helm `grafana.auth.proxy.enabled`). The AVA edge
(`forwardAuth`) stamps `X-Auth-Request-Email` on every request that reaches the
public `/grafana` host, and Grafana trusts that header as the logged-in user
(`GF_AUTH_PROXY_HEADER_PROPERTY=username`, `GF_AUTH_PROXY_AUTO_SIGN_UP=true`).
Grafana accepts the header from any source inside
`GF_AUTH_PROXY_WHITELIST=10.0.0.0/8,172.16.0.0/12,192.168.0.0/16`.

So:

| Path | What happens |
|---|---|
| Browser → public `/grafana` host | edge stamps `X-Auth-Request-Email` → Grafana renders authenticated. **Works for humans.** |
| Agent pod → port-forward + Keycloak `client_credentials` bearer | bypasses the edge → **no `X-Auth-Request-Email` header** → Grafana renders nothing. **This is why headless screenshots came back blank.** |
| Agent pod → ClusterIP, **stamping the header itself** | pod IP is in `10.0.0.0/8` (whitelisted) → Grafana trusts the header → renders authenticated. **Works for agents.** ✓ |

The blank panels were never an architecture problem and never needed a new AVA
endpoint or kbeads ingress. The headless tooling simply skipped the layer that
does the auth. Stamp the header yourself and you are authenticated.

Proof (from a pod, IP `10.30.139.1`):

```bash
curl -s -o /dev/null -w "%{http_code}\n" "$GRAF/api/user"                 # 401  (no header)
curl -s -H "X-Auth-Request-Email: x@acme.example" "$GRAF/api/user"        # 200  {"login":"x@acme.example","authLabels":["Auth Proxy"],...}
```

## Recipe

```bash
# Subpath mode: include /grafana (or just use $GRAFANA_URL, already injected).
GRAF="${GRAFANA_URL:-http://gardenio-grafana.gardenio.svc.cluster.local:3000/grafana}"
VM=http://gardenio-vmsingle:8428                              # ground truth (GRADE accuracy)
H="X-Auth-Request-Email: matt.baker@acme.example"           # any email; auto-signed-up as a Viewer

# 1. enumerate dashboards
curl -fsS -H "$H" "$GRAF/api/search?type=dash-db"            # uid + title list

# 2. get the model (for panel queries / GRADE cross-check) — panel IDs may be null
#    in the model JSON for ConfigMap-provisioned dashboards; Grafana assigns
#    sequential IDs at load time, so render by sweeping panelId=1..N (below).
curl -fsS -H "$H" "$GRAF/api/dashboards/uid/<uid>"

# 3. render each panel solo — the populated path
curl -fsS -H "$H" -o p<N>.png \
  "$GRAF/render/d-solo/<uid>/x?panelId=<N>&width=700&height=350&theme=dark&from=now-24h&to=now"
```

### Sweeping panel IDs

ConfigMap-provisioned dashboards (bridge-synced dashboard beads, and the
in-repo `gardenio/observability` dashboards) usually have `"id": null` in the
model JSON. Grafana assigns IDs `1..N` at load. Just sweep a range and keep the
ones that render real content:

```bash
for pid in $(seq 1 30); do
  sz=$(curl -s -H "$H" -o p$pid.png -w '%{size_download}' \
    "$GRAF/render/d-solo/<uid>/x?panelId=$pid&width=700&height=350&theme=dark&from=now-24h&to=now")
  echo "panelId=$pid size=$sz"
done
```

A **"Panel not found"** placeholder comes back at a small, constant size
(~4.4 KB in dark theme) — discard those; real panels are larger and vary. (The
constant-size value is theme/version-specific; the point is the placeholders all
share one small size while real panels differ — filter on that, not a hardcoded
number.)

### Tiling into a whole-dashboard image (ffmpeg, already in the agent image)

```bash
# pad each panel to a uniform tile, then hstack rows + vstack the grid
for f in p*.png; do
  ffmpeg -y -loglevel error -i "$f" \
    -vf "scale=700:350:force_original_aspect_ratio=decrease,pad=700:350:(ow-iw)/2:(oh-ih)/2:color=0x0e1117" "pad_$f"
done
# hstack 3 per row → row1.png row2.png ...; then vstack the rows → montage.png
```

Upload with `gb image upload montage.png` (whitelisted S3 bucket, prints a URL).

## Gotchas

- **Full-page render `/render/d/<uid>` — FIXED (bug ka-0000001440).** It used to
  return Grafana's "failed to load application files" page (rendered to PNG)
  because `GF_SERVER_ROOT_URL=.../grafana` carried a subpath while Grafana ran
  with `serve_from_sub_path=false` and the renderer callback pointed at the
  Service root — so the renderer's headless browser loaded the SPA with
  `<base href>=/grafana/` but fetched `/grafana/public/build/*.js` from a pod
  serving assets at root → every asset 404'd. The chart now runs **true subpath
  mode**: `GF_SERVER_SERVE_FROM_SUB_PATH=true`, the unified-host ingress no
  longer strips `/grafana`, and the in-cluster `GF_RENDERING_CALLBACK_URL` +
  agent `GRAFANA_URL` both carry `/grafana` — all four agree, so full-page
  renders return real PNGs. (Per-panel `/render/d-solo/<uid>` still works too,
  and tiling remains useful for custom montages.)
- **Full-board render `/render/d/<uid>` used to 500 / cascade under load — FIXED
  (bug ka-0000006919).** Even after the subpath fix above, the whole-board route
  returned HTTP 500 (or hung to timeout) on tall, multi-panel pages, and the
  single `grafana-image-renderer` instance cascaded 500s under any concurrency
  with a ~60-75s recovery, while a single `d-solo` panel still rendered fine.
  Three headless-Chromium-in-a-container failure modes, each fixed by one helm
  knob under `grafana.imageRenderer`: (1) K8s mounts `/dev/shm` at only 64Mi,
  which a tall page exhausts → tab crash → 500 — fixed by
  `--disable-dev-shm-usage` in `RENDERING_ARGS` (Chromium shm goes to the
  disk-backed `/tmp` emptyDir); (2) the single-browser `default` mode collides
  on one shared page under concurrency — fixed by `RENDERING_MODE=clustered`
  with an isolated context per render behind a bounded queue
  (`maxConcurrentRenders`); (3) the 512Mi sidecar OOM-killed on a tall render
  (the kill+relaunch WAS the ~60-75s "recovery") — fixed by bumping the memory
  limit to 1Gi. All three default-on when `grafana.imageRenderer.enabled=true`.
  Concurrent full-board renders now queue and succeed instead of cascading.
- **`admin:admin` basic-auth also works in-cluster** for the API and renders,
  but it is break-glass: prefer the auth-proxy header so you render as a real
  user and exercise the same path humans use.
- **Always target the ClusterIP**, never the public `/grafana` ingress, from a
  pod — the public ALB 308-redirects/AVA-gates non-browser traffic.
- The render itself takes ~5–10 s per panel (headless Chromium in the
  image-renderer sidecar). Budget for it; render in the background.

## Live config reference (prod, 2026-06-17)

`gardenio-grafana` Deployment env, all set by the helm chart
(`gardenio/helm/gardenio/templates/grafana/deployment.yaml`):

```
GF_SERVER_ROOT_URL=https://gardenio.e2e.example.com/grafana
GF_SERVER_SERVE_FROM_SUB_PATH=true        # true subpath mode (ka-0000001440)
GF_AUTH_PROXY_ENABLED=true
GF_AUTH_PROXY_HEADER_NAME=X-Auth-Request-Email
GF_AUTH_PROXY_HEADER_PROPERTY=username
GF_AUTH_PROXY_AUTO_SIGN_UP=true
GF_AUTH_PROXY_WHITELIST=10.0.0.0/8,172.16.0.0/12,192.168.0.0/16
GF_SECURITY_ALLOW_EMBEDDING=true          # the iframe-in-beads-viewer fix (ka-0000003842)
GF_RENDERING_SERVER_URL=http://127.0.0.1:8081/render        # image-renderer sidecar
GF_RENDERING_CALLBACK_URL=http://gardenio-grafana:3000/grafana/  # carries /grafana in subpath mode
```

In-cluster callers therefore reach Grafana at `http://gardenio-grafana:3000/grafana`
(NOT the bare root) — this is what the agent `GRAFANA_URL` env now carries, so
`gb grafana render` and the renderer callback both hit the working base.

The image-renderer is the 3rd container in the `gardenio-grafana` pod
(`grafana-image-renderer`); `grafana.imageRenderer.enabled` gates it.

## See also

- `gardenio/docs/grafana-grade-audit-harness.md` — the GRADE audit loop; its
  "Access" section points here for the verified render path.
- `gardenio/controller/cmd/beads-viewer/dashboard_view.go` — the same-origin
  iframe embed (`:::grafana <uid>`), which is the *human* render path in
  beads-viewer (uses the AVA session cookie, not a stamped header).
