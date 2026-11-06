# /costs CRISP Audit — iteration 2026-08-06-manual (first manual pour)

| | |
|---|---|
| Iteration | `2026-08-06-manual` |
| Molecule | ka-0000006569 (formula ka-0000007333) |
| Gardener | ka-0000008137 (**disabled** — this pour is the acceptance manual run) |
| Parent epic | ka-0000006115 |
| VIEWER_URL | http://localhost:8095 (port-forward) — **NOT reached** (see Access gap) |
| VM_URL | http://gardenio-vmsingle:8428 — reached (no auth) |
| BEADS_URL | http://localhost:8080 / daemon CLI — reached |
| Surfaces audited | source + reachable ground truth; **live render deferred** |
| Date | 2026-08-06 |

## Headline

**First manual pour of the /costs CRISP loop, driven by hand off the new (disabled) gardener
ka-0000008137.** The harness's required WRONG-WINDOW self-test **fired**, all five seed findings
re-verified **5/5 FIXED** at source level, and reachable ground truth (vmsingle, account beads) was
snapshotted. The live-page render (Playwright /costs) could **not** run: the
`gardenio-info-density-audit` Keycloak `client_credentials` secret is unprovisioned in every reachable
namespace, because the `ClusterKeycloak "gardenio-incluster"` the viewer clients reference is missing.
Filed as bug **ka-0000004206** (the re-enable blocker), parented to the epic.

No CRISP overall score is asserted this iteration — a partial (source-only) pass must not manufacture a
number the page's live render didn't earn. This is the honest-empty posture the rubric itself demands.

## Self-test (harness teeth)

✅ **FIRED.** The cross-check was fed a fabricated headline equal to the calendar-UTC-day GrandTotalUSD
while the account is subscription-billed, and classified it **WRONG-WINDOW**, not OK
(`crosscheck.json._selftest`). A harness that can only report OK is worthless; this one detects the
epic's founding defect class.

## Accuracy — reachable ground truth (vmsingle no-auth, account beads via daemon)

| Source | Value | Note |
|---|---|---|
| `sum(gardenio_agent_spend_logs_usd)` | 5639.08 | the live settled-bill source the explorer reads |
| `count(gardenio_agent_spend_logs_usd)` | 1154 series | |
| `count(gardenio_tag_cost_usd{dim="model"})` | 5 series | **By-model source is LIVE** (seed C/by-model) |
| `count(gardenio_account_cost_usd)` | 9 series | |
| account beads (daemon) | 20 | daily/5h/7d window fields populated on active accounts |

Displayed-vs-truth deltas require the rendered page and are deferred.

## Five seed findings — re-verification (source level, 5/5 FIXED)

| Seed | Dimension | Source citation | Verdict |
|---|---|---|---|
| headline subscription-window branch | I | costs.go:21-27, 267-273 | ✅ FIXED |
| trust floors in-lockstep w/ values.yaml (P3 retired) | R | costs_trust.go:44-85 | ✅ FIXED |
| e2e exclusion (PromQL + bead layer) | S | costs_rollup.go:119, costs_rate.go:38-106 | ✅ FIXED |
| By-model reads tag_cost{dim=model} (dead iframe removed) | C | costs.go:110-118, 509-517 | ✅ FIXED |
| explorer reads agent_spend_logs (dead counter gone) | C | costs_rollup.go:90 | ✅ FIXED |

The dead counter `gardenio_agent_spend_usd_total` appears in `costs*.go` **only in comments documenting
its removal** — it is not read by any live surface.

## Access gap (top finding — bug ka-0000004206)

The CRISP harness §Access recipe cannot complete from a gardenio crew pod:
`mint_client_credentials_token` fails to read the `gardenio-info-density-audit` secret, and every
beads-viewer KeycloakClient CRD in the reachable e2e namespaces reports
`ClusterKeycloak "gardenio-incluster" not found`. So the operator never writes the `client_credentials`
secret the info-density / CAMP / CRISP loops all depend on. **Fix options in the bug**: provision
`gardenio-incluster` (or repoint the chart realm at the live `acme-keycloak`), OR give the harness a
dev-bypass in-cluster access path. **Until then the gardener stays disabled** — a daily loop that can't
render the page would file false empty findings every run.

## Idea catalog

See `idea-catalog.md` — 5 honest cost cuts (5h-vs-7d headroom, subscription-vs-metered split,
cost-per-closed-bead, throttle-vs-utilisation timeline, estimate-vs-settled trend). None filed as work
this iteration (no live render to score against).

## Gardener readiness

- Trigger bead **ka-0000008137** created **disabled** (`enabled=false`), daily cron `23 9 * * *` UTC
  (offset from GRADE's `23 7`), slack routing `C0AHE7C0E1G`.
- `gb gardener status`: **has_backstop: true**, **self_block: ok** ("bounded + progress-aware") — strictly
  better than GRADE (which is self_block: weak), zero field divergences.
- Re-enable gate: operator go-ahead on epic ka-0000006115 **AND** access-gap bug ka-0000004206 resolved,
  then `kd update ka-0000008137 --field enabled=true`.
