# beads-viewer screenshot harness

Playwright suite that captures canonical beads-viewer pages at pinned
viewports. Intended for MR before/after visual review, not for CI gating.

Full docs: [`gardenio/docs/beads-viewer-dev.md`](../../docs/beads-viewer-dev.md)

## Quick start

```sh
# 1. Boot beads-viewer locally (see docs for the full recipe).
# 2. From the monorepo root:
make screenshot-beads-viewer
```

Screenshots land in `test-results/beads-viewer-screenshots/<viewport>/<page>.png`.

## Config knobs

| Env                  | Default                   |
| -------------------- | ------------------------- |
| `BEADS_VIEWER_URL`   | `http://localhost:8095`   |
| `SCREENSHOT_PROJECT` | `gardenio`                 |
| `SCREENSHOT_BEAD_ID` | `ka-0000001180`           |
| `SCREENSHOT_QUERY`   | `screenshot`              |

## `/threads` visual capture matrix (hermetic — no live daemon)

`tools/capture-threads.mjs` (+ `tools/threads-mock-daemon.mjs`,
`tools/threads-fixtures.mjs`) is a self-contained, **hermetic** capture of the
Phase-3 `/threads` surface — index, detail, and the bead-show "part of wish"
chip — graded against the Phase-3 visual rubric ka-0000007635 (gates V1–V12).
Task 3-3 (ka-0000004063).

Unlike the canonical harness above (which points at a real, port-forwarded
daemon and drifts with live data), this matrix boots a **mock beads daemon**
serving pinned fixtures — one wish in every lifecycle state (open multi-message,
closed/fulfilled, needs-triage, long-thread, empty) plus member beads for the
chip — so timestamps, ordering, and states are byte-stable and CI-reproducible
with no Postgres, no Keycloak, no port-forward.

```sh
# One command boots the mock daemon + the real beads-viewer binary + the capture:
gardenio/tests/beads-viewer-screenshots/tools/run-threads-capture.sh
# or via make from the repo root:
make screenshot-threads
```

The capture sweeps **SKINS × WIDTHS × cells**:

- **SKINS** = `baseline`, `dark-terminal`, **`dark`** (the contrast-trap probe —
  the thread templates carry no `data-skin="dark"` overrides, so capturing it
  surfaces rubric V4 as a tracked defect; see ka-0000004063's filed defect).
- **WIDTHS** = `480`, `768`, `1280` (mobile / tablet / desktop — V6/V7).
- **cells** = index `{empty,populated,error}`, detail
  `{empty,populated,fulfilled,needstriage,longthread,error}`, bead-show chip
  `{resolved,fallback,absent}`, plus a **live work-expand interaction** (clicks
  `.thread2-work > summary`).

Each cell is captured at the first viewport AND `fullPage`. PNGs land under
`test-results/threads/<theme>-<bp>/` named
`threads-<view>-<state>-<theme>-<bp>[-<interaction>].png`, with a
`manifest.json` index. Composer / optimistic-send cells (rubric V5, V10) are
**not** captured here — they land with task 2-5 / 3-1 and are marked
`N/A-until-2-5` in the grade, never fake-passed.

| Env                | Default                    | Meaning                                  |
| ------------------ | -------------------------- | ---------------------------------------- |
| `MOCK_DAEMON_PORT` | `18080`                    | Mock daemon port (NOT `8080` — avoids a real daemon) |
| `VIEWER_PORT`      | `8095`                     | beads-viewer listen port                 |
| `VIEWER_BIN`       | *(unset → `go run`)*       | Prebuilt viewer binary (faster than `go run`) |
| `OUT_DIR`          | `test-results/threads`     | Screenshot output dir (relative to harness) |

## `tools/shoot-one.js` — reusable single-page capture (per-route CDQR)

`tools/shoot-one.js` shoots **one** beads-viewer route at both canonical CDQR
viewports in a single invocation — desktop (`1440x900`) + narrow (`430x932`) —
so every per-route CDQR audit (gardener, and future single-page audits) can call
one flag-driven tool instead of hand-rolling a fresh playwright invocation each
pour. It supersedes the gardener-specific, positional-arg `capture-gardener.mjs`.

```sh
node tools/shoot-one.js \
  --url "$BEADS_VIEWER_URL/gardener/gd-XXXX" \
  --bearer "$BEADS_VIEWER_BEARER" \
  --out test-results/gardener \
  --name my-gardener
# writes test-results/gardener/{desktop,narrow}/my-gardener.png
```

Wait strategy: it RACES `networkidle` against a configurable readiness
`--selector` (default `.gardener-iter`) and captures on whichever resolves — so
it is correct both for quiescent pages (networkidle fires) and live-polling
SSE/htmx pages where the network is never idle (the selector fires).

| Flag                 | Default          | Meaning                                              |
| -------------------- | ---------------- | ---------------------------------------------------- |
| `--url`              | *(required)*     | Page URL to capture                                  |
| `--out`              | *(required)*     | Output base dir; PNGs land under `desktop/` + `narrow/` |
| `--name`             | *(required)*     | Basename (no `.png`) for each viewport shot          |
| `--bearer`           | `$BEADS_VIEWER_BEARER` | Authorization bearer token                     |
| `--selector`         | `.gardener-iter`  | Readiness selector; `--selector ''` opts out (networkidle-only) |
| `--selector-timeout` | `8000`           | ms to wait for `--selector` / networkidle            |
| `--settle`           | `1500`           | ms to settle in-flight htmx swaps before the shot    |
| `--full-page`        | *(off)*          | Capture the full scrollable page (auto-scrolls to fire `intersect once` lazy sections) |
| `--dump-html`        | *(off)*          | Write final rendered HTML to `<path>-<viewport>.html` |

## `tools/capture-navset.mjs` — before/after nav-consistency capture

`tools/capture-navset.mjs` boots ONE prebuilt `beads-viewer` binary
(`$VIEWER_BIN`) against a live daemon under dev-auth-bypass, captures the
nav/filter-strip pages at desktop (1280×800) + narrow (480×800) with animations
frozen, then tears the viewer down. Run it once per variant (`VARIANT=before`
then `VARIANT=after`) with two binaries built at different commits, pointed at
the **same** daemon, and diff the two output trees for a MR before/after visual.

It exists because the menu-bar consistency audit (ka-0000004068) could not
capture live screenshots — the prod viewer is OIDC-gated and a local render
needs Postgres. This closes that gap (F8, ka-0000005211): build the viewer at
the pre-fix and post-fix commits, boot each over the live (port-forwarded) prod
daemon with `BEADS_VIEWER_DEV_AUTH_BYPASS=1` + `_ACK_AGENT_POD=1`, and capture.

```sh
# 1. Port-forward the prod beads daemon (read-only), export its token:
kubectl -n gardenio-system port-forward svc/gardenio-beads 8080:8080 &
TOKEN=$(kubectl -n gardenio-system get secret kd-beads-token -o jsonpath='{.data.token}' | base64 -d)

# 2. Build the viewer at each commit:
go build -o /tmp/viewer-before ./gardenio/controller/cmd/beads-viewer/   # from a BEFORE worktree
go build -o /tmp/viewer-after  ./gardenio/controller/cmd/beads-viewer/   # from AFTER (main)

# 3. Capture each variant against the same daemon:
for v in before after; do
  VIEWER_BIN=/tmp/viewer-$v BEADS_HTTP_ADDR=http://localhost:8080 BEADS_DAEMON_TOKEN=$TOKEN \
  VARIANT=$v OUT_ROOT=test-results/navset VIEWER_PORT=8097 \
  CHROME_PATH=/ms-playwright/chromium-*/chrome-linux64/chrome \
  node tools/capture-navset.mjs
done
# PNGs land in test-results/navset/<variant>/<viewport>/<page>.png
```

| Env                 | Default                  | Meaning                                     |
| ------------------- | ------------------------ | ------------------------------------------- |
| `VIEWER_BIN`        | *(required)*             | Prebuilt beads-viewer binary to boot        |
| `BEADS_HTTP_ADDR`   | `http://localhost:8080`  | Daemon URL                                  |
| `BEADS_DAEMON_TOKEN`| *(unset)*                | Bearer for the daemon                       |
| `VARIANT`           | `after`                  | Output subdir (`before`/`after`)            |
| `OUT_ROOT`          | `test-results/navset`    | Output root                                 |
| `VIEWER_PORT`       | `8095`                   | beads-viewer listen port                    |
| `CHROME_PATH`       | *(bundled)*              | Chromium executable (agent pods pin `/ms-playwright/chromium-*`) |

Pages: `/`, `/agents`, `/triggers`, `/p/gardenio`, `/ratings`, `/plans`,
`/extern` — the second-row filter strips the audit inventoried. `/inbox` is
omitted: it now 308s to the per-user dashboard, which needs an OIDC identity
dev-auth-bypass does not provide.
