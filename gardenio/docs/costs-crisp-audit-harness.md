# /costs CRISP audit — methodology + harness

Source of truth for the **accuracy-first** `/costs` dashboard audit loop:
epic [`ka-0000006115`](https://gitlab.com/example-org/gardenio/-/issues)
(*"/costs page — CRISP accuracy audit + subscription-correct headline"*),
formula `ka-0000007333` (definition checked in at
[`costs-crisp-audit-formula.json`](costs-crisp-audit-formula.json)),
continual gardener `ka-0000006654` (ships **disabled** until operator go-ahead —
same posture the GRADE gardener shipped with).

Driven by Slack thread `ka-0000005642` (Matt Baker): *"a rubric, graphical based
gardener and correctness based formula based kind of thing like we've done in the
past … make the /costs page a surface an operator can read and believe."* The
fleet is **almost entirely Claude subscription accounts** — not the metered-API
estate the page was originally written for — so "believe" means the number an
operator reads has to mean what they think it means.

## Lineage — GRADE, adapted for a hand-written page

This loop is the [Grafana GRADE](grafana-grade-audit-harness.md) harness
(epic `ka-0000008066`, formula `ka-0000003015`, 13 clean daily runs, fleet mean
6.87 → 8.0) pointed at a different surface. GRADE audits **Grafana dashboards**
rendered from VictoriaMetrics; CRISP audits the **hand-written Go/HTML `/costs`
page** (`gardenio/controller/cmd/beads-viewer/costs*.go` + `templates/costs.html`),
which no existing loop covers.

Two things change because the surface changed:

| | GRADE (Grafana) | CRISP (/costs) |
|---|---|---|
| **Render** | Grafana server-side `d-solo` PNG, in-cluster, header-auth | Playwright against the beads-viewer ClusterIP behind a Keycloak `client_credentials` bearer (§Access) |
| **Ground truth** | one source — VictoriaMetrics | **three** — vmsingle gauges, the account beads, and LiteLLM (§Ground truth) |
| **Displayed value** | Grafana panel value | scraped from the rendered DOM / `/costs/rollup` partials |

The dimension neither the maps-beauty nor the DVFC molecule ever fully closed is
**accuracy** — proving the displayed number equals reality. On `/costs` that is
the heaviest dimension by design, because every one of the five operator-reported
defects (§Seed) was a number that *looked* authoritative and was wrong.

## The CRISP rubric (0-10 each; bar for "good" = 8)

Deliberately mapped 1:1 onto the five defects, so the loop that scores the page
also regression-guards the fixes that closed them.

- **C — Correct (accuracy).** *The heaviest dimension.* Every displayed number
  replayed against its **own** source of truth (the account-bead field, the
  vmsingle gauge, or LiteLLM `/spend/logs`+`/spend/tags`) and shown to match.
  Covers defects 2 + 4 + the P0 1000×-low counter.
- **R — Reconciled (drift honesty).** Every trust-strip pair judged against the
  **same floor the C5 vmalert uses** — not a stale hardcoded copy. Each
  divergence is either closed or documented as *structural* with a measured
  steady-state band. A red badge must mean something is actually wrong. Covers
  defect 2.
- **I — Intelligible (does the number mean what an operator thinks).** The
  headline's window == the window the *subscription* resets on, and the label
  says which. A subscription fleet's headline must not silently be a calendar-day
  sum on a clock unrelated to the quota. Covers defect 1.
- **S — Sanitized (no synthetic pollution).** No e2e / fixture / test accounts in
  any list, ranking, or dropdown — **bead-side *and* PromQL-side**, since the
  gauge series outlive the bead. Covers defect 3.
- **P — Presentation.** Tufte signal-per-pixel, dark-theme craft, both viewports,
  honest empty states (never a silent "No data" that reads as `$0`).

Overall = mean of the five. Bar for "good" is **8** — `/costs` is the operator's
primary money surface.

## Access — the hard part (do NOT assume GRADE's recipe transfers)

GRADE runs **entirely in-cluster by ClusterIP with no auth** — Grafana trusts a
stamped `X-Auth-Request-Email` header from a whitelisted pod IP. **beads-viewer
is different**, and this is the single most important thing to get right in step
1: the public ingress (`https://beads-viewer.gardenio.e2e.example.com`) sits
behind AVA `forwardAuth`, an OIDC code-flow that agent pods cannot complete, and
the AWS ALB 308-redirects before traffic even reaches Traefik (decision
`ka-0000005537` abandoned the bypass-header approach). **Never target the public
host, and per project rule never target production/staging `acme.net` — deploy-MR
envs on the e2e cluster only.**

The working recipe — lifted verbatim from the proven beads-viewer audits
(`info-density-audit-harness.md`, formula `ka-0000002694`; the `/gardeners` CAMP
loop `ka-0000000382`) — is: **port-forward straight to the ClusterIP and
authenticate to beads-viewer's own OIDC middleware with a Keycloak
`client_credentials` bearer.** The bearer is dual-audience
(`gardenio-beads-viewer` + `gardenio-beads-api`), so the *same* token is accepted
by the beads daemon, which is what lets us read bead-field ground truth and
bootstrap the operator row through one credential.

```bash
# 1. Namespace + release name (downward-API; equals the release name in-namespace).
export POD_NAMESPACE=${POD_NAMESPACE:-$(cat /var/run/secrets/kubernetes.io/serviceaccount/namespace)}
export KC_RELEASE_NAME=${KC_RELEASE_NAME:-$POD_NAMESPACE}

# 2. Mint a viewer-audience bearer via client_credentials (advice ka-0000006483).
#    The agent ServiceAccount already has `get` on the operator-managed Secret
#    keycloak-client-${KC_RELEASE_NAME}-client-gardenio-info-density-audit-secret.
# shellcheck disable=SC1091
source gardenio/tests/e2e/scripts/lib-keycloak-client-creds.sh
VIEWER_BEARER=$(mint_client_credentials_token)

# 3. Port-forward the DAEMON and bootstrap the captor's operator row — the SAME
#    bearer works here (gardenio-beads-api audience). Without this /costs (like
#    /inbox) renders the unprovisioned-operator banner and captures an empty page
#    (ka-0000004196).
kubectl -n "$POD_NAMESPACE" port-forward svc/gardenio-beads 8080:8080 \
  > /tmp/beads-daemon-pf.log 2>&1 &
trap 'kill %1 %2 %3 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do curl -fsS -o /dev/null http://localhost:8080/healthz && break; sleep 1; done
curl -fsS -X POST -H "Authorization: Bearer $VIEWER_BEARER" -H 'Content-Type: application/json' \
  --data '{"kind":"agent","subject":"gardenio-costs-crisp-audit","source":"audit-bootstrap"}' \
  http://localhost:8080/v1/operators/me/aliases | jq .

# 4. Port-forward the VIEWER and wait for :8095 to bind.
kubectl -n "$POD_NAMESPACE" port-forward svc/gardenio-beads-viewer 8095:8095 \
  > /tmp/beads-viewer-pf.log 2>&1 &
for _ in $(seq 1 30); do
  curl -fsS -o /dev/null -H "Authorization: Bearer $VIEWER_BEARER" http://localhost:8095/health && break
  sleep 1
done

# 5. vmsingle — ground truth for the PromQL-backed surfaces. In-cluster ClusterIP,
#    NO auth (same as GRADE). No port-forward needed from an agent pod.
VM_URL=${VM_URL:-http://${GARDENIO_VMSINGLE_SERVICE_HOST:-gardenio-vmsingle}:8428}
curl -fsS "$VM_URL/api/v1/query?query=up" | jq .status   # "success"

# 6. Warm-up: /costs and its rollup endpoint must both be < 400.
for path in /costs '/costs/rollup?dim=agent&window=24h'; do
  code=$(curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $VIEWER_BEARER" \
    "http://localhost:8095${path}")
  printf '%-40s -> %s\n' "$path" "$code"
done

# 7. Capture: the Playwright config picks BEADS_VIEWER_BEARER up automatically and
#    injects it as an Authorization extraHTTPHeader.
export BEADS_VIEWER_BEARER=$VIEWER_BEARER
BEADS_VIEWER_URL=http://localhost:8095 make screenshot-beads-viewer
```

`/costs` is **not** operator-gated (the info-density warmup list already hits it
with the viewer-role bearer), so the default `gardenio-info-density-audit` client
is sufficient — no need for the elevated `gardenio-camp-audit` client. The bearer
is read-only in practice: the audit only GETs pages plus the one daemon-side alias
bootstrap.

## Ground truth — three sources, one per surface class

`/costs` mixes bead-sourced and PromQL-sourced surfaces, and both ultimately
derive from LiteLLM. The cross-check replays each displayed number against **its
own** source, not a convenient proxy:

| Source | Reached via | Surfaces it grounds |
|---|---|---|
| **vmsingle gauges** (`$VM_URL/api/v1/query`) | in-cluster ClusterIP, no auth | trust strip (`gardenio_xcheck_agreement`), cost explorer rollup (`gardenio_agent_spend_logs_usd`, `gardenio_account_cost_usd`, `gardenio_tag_cost_usd`), the 24h/1h rate line, the By-model rollup (`gardenio_tag_cost_usd{dim="model"}`) |
| **account beads** (`$BEADS_URL/v1/beads`, `type=account`) | daemon port-forward, dual-audience bearer | headline subscription bill (`spend_5h_window_usd`, `spend_7d_window_usd`), per-account cards, Top spenders, Hottest, Throttled, By-provider |
| **LiteLLM** (`/spend/logs`, `/spend/tags`, `/key/info`) | *the ultimate source* — see note | what the two above are *derived from*; the measure-twice anchor for C |

**On LiteLLM access (measure twice — advice ka-0000005924).** The bead fields and
the gauges are both re-published LiteLLM values (`spend_sync.go`, `spendlogs_sweep.go`,
`tag_spend_sync`). The honest, load-cheap second source is to read those series
back rather than re-hit LiteLLM — which would double the API load *and* skip the
emit→scrape path the number travels. **This is exactly what the trust strip already
is**: `gardenio_xcheck_agreement{pair=…}` is a *standing* reconciliation of
LiteLLM-vs-gauge, published by the C2 cross-check sweep
([`cost-xcheck-audit-sweep.md`](cost-xcheck-audit-sweep.md)). So CRISP's R
dimension reads the same agreement series an operator sees, and CRISP's C dimension
treats the gauges as authoritative for displayed-vs-gauge equality while trusting
the sweep to keep gauge-vs-LiteLLM honest. **Direct `/spend/logs` replay is
optional** and only attempted in step 1 if `LITELLM_URL` + a key are present in the
pod; if absent, the harness degrades to gauge-replay and says so — it never
fabricates a LiteLLM number.

## The surfaces to audit (enumerated)

Verified against `costs.go` / `costs_rollup.go` / `costs_trust.go` /
`templates/costs.html` at the epic baseline. Step 1 re-derives this list from the
live page; it is reproduced here so a fresh iteration knows what "complete" means.

| # | Surface | Go builder | Data source | Dimension it stresses |
|---|---|---|---|---|
| A | Headline / summary card | `buildCostsSummary` (`costs.go:754`) | bead `spend_5h/7d_window_usd` + rolling-rate `gardenio_account_cost_usd` (`costs_rate.go:82`) | **I** + C |
| B | Trust strip | `buildCostsTrust` (`costs_trust.go:293`) | `gardenio_xcheck_agreement` (`costs_trust.go:35`) | **R** |
| C | Cost explorer rollup + dropdowns | `handleCostsRollup` (`costs_rollup.go:893`) | real-bill gauges, dim-dispatched (`resolveRollupSource` `costs_rollup.go:443`) | **C** + S |
| D | Grafana panels ("Spend over time") | `renderCostsChartPanels` (`costs.go:171`) | in-cluster Grafana solo panels 1–3, uid `gardenio-costs-candidate` (`costs.go:47,117`) | C + P |
| E | By provider | `buildCostsProviderRollups` (`costs.go:883`) | account bead `provider` + cost fields | C |
| F | By model | `buildCostsModelRollups` (`costs.go:1029`) | `gardenio_tag_cost_usd{dim="model"}` (`costs.go:993`) | **C** |
| G | Hottest utilization | `buildCostsHottestUtilization` (`costs.go:1116`) | account bead `utilization_5h/7d`, `throttle_reset_*_at` | I + P |
| H | Top spenders today | `buildCostsTopSpenders` (`costs.go:951`) | account bead `daily_cost_usd` | S + P |
| I | Throttled now | `buildCostsThrottled` (`costs.go:1157`) | account bead `throttle_status`, reset fields | I + P |
| J | Per-account cards | `buildCostAccountView` (`costs.go:615`) | account-bead LiteLLM fields (`costs.go:674`) | **S** + C |
| K | Empty-state scaffold | `costsScaffoldNames` (`costs.go:201`) | static placeholders | **P** |

Explorer dropdowns cover dim ∈ {agent, role, project, account, model} × window;
each dim dispatches a different real-bill metric, so all five are captured.

## The cross-check (heart of the loop)

For **every** displayed number: replay it against its own source over the
**same window** the surface renders, and reconcile *displayed* vs *truth*.
Objective, no judgement yet. Classify each mismatch:

- **`WRONG-WINDOW`** — *the defect class this epic is built on.* The number is a
  correct sum over the **wrong time window** (e.g. a calendar-UTC-day total shown
  as the headline for a subscription account whose quota resets on a rolling 5h/7d
  clock). This is the class the injected-defect self-test (below) exercises.
- **`AGGREGATION-DRIFT`** — the query runs but misleads: a `sum()` that
  double-counts, a group-by on the wrong/optional label that collapses buckets, a
  rate over the wrong window.
- **`UNIT/SCALE`** — value correct but rendered at the wrong magnitude/format
  (the classic 1e15; percentages 0..1 vs 0..100).
- **`STALE/FROZEN`** — series/field exists but its last sample is old; the surface
  paints a stale point as if live. *Includes reading a dead counter* — the P0
  1000×-low `gardenio_agent_spend_usd_total` was this class (11 live series holding
  0.09% of the bill).
- **`SYNTHETIC-POLLUTION`** — an e2e / fixture / test account appears in a list,
  ranking, or dropdown, bead-side *or* PromQL-side.
- **`MISSING-SERIES`** — the expr references a metric/label absent from vmsingle.
  Distinguish a *naming bug* (exists under a different name — fix it) from an
  *honest no-data condition* (the surface must say so, not imply `$0`).
- **`OK`** — displayed == truth within tolerance and the query is honest.

Emit `docs/costs-crisp/<ITERATION>/crosscheck.json` — one row per surface/number:
`{surface, displayed, source, truth, delta, window, verdict}`.

### Prove the harness has teeth (required self-test)

An audit that can only report `OK` is worthless. Every iteration MUST demonstrate
the cross-check catches a **deliberately-injected wrong-window number** before it
trusts a clean result:

> Feed the cross-check a fabricated "displayed" headline equal to the
> calendar-UTC-day `GrandTotalUSD` while the account is subscription-billed
> (so its true headline is the 5h/7d window bill). Assert the cross-check
> classifies it **`WRONG-WINDOW`**, not `OK`. Record the assertion in
> `crosscheck.json` under a `_selftest` key. If the self-test does not fire,
> the run is void — the harness is blind and its `OK`s mean nothing.

This is the same discipline GRADE's seed drifts enforce: a known-bad input the
scorer must flag, run every iteration.

## Seed the rubric — this epic's five verified findings (iteration-0 baseline)

The five operator-reported defects, each verified in code against the epic
baseline `2a64638e3` **with file:line citations**, and each now **FIXED** by a
landed MR. They are the first iteration's baseline: the loop's first run measures
against this known state and must show every one still holds — exactly how GRADE
tracks its seed drifts (`ka-0000002809`, `ka-0000004193`) as "confirmed FIXED" in
every later report. A regression on any row is a P0.

| CRISP | Class | Defect (baseline `2a64638e3`) | Fix bead → MR | Baseline status |
|---|---|---|---|---|
| **I** | `WRONG-WINDOW` | Headline `GrandTotalUSD` = Σ bead `daily_cost_usd` (`costs.go:613-617`, `costs.html:31-34`), bucketed 00:00 UTC (`spend_sync.go:101-103`) — no subscription branch anywhere (`billing_type` had zero hits). | `ka-0000003343` → !6182 | **FIXED** — subscription headline (`PrimaryBillUSD` from `spend_5h/7d_window_usd`, `billing_type` branch `costs.go:26-29,687`). Guard against regression: `billing_type` present, calendar-day demoted to a sub-line. |
| **R** | `AGGREGATION-DRIFT` | Trust strip hardcoded `costsTrustDriftFloor = 0.95` (`costs_trust.go:59`) while `values.yaml:5856` had retuned `settled-vs-bill: 0.5` (`c30892e46`) — the strip painted red on a floor the platform had abandoned. | `ka-0000007295` → !6177 | **FIXED** — per-pair floors `costsTrustPairFloors()` (`costs_trust.go:79-85`: settled-vs-bill `0.5`, bill-conservation `0.85`) + lockstep test `TestCostsTrustFloorsMatchValuesYAML`. |
| **S** | `SYNTHETIC-POLLUTION` | Zero test-account filtering on the whole path — bead query never set the available `ExcludeLabels` (`costs.go:431-436`), PromQL unfiltered (`costs_rollup.go:481-489`), producer emitted a gauge per e2e account (`metrics.go:467-471`). | `ka-0000005167` → !6186 | **FIXED** — bead-side `IsE2EAccountBead` (`costs.go:576`) + PromQL-side `costsRollupE2EAccountExclusion` (`costs_rollup.go:147`). Live: by-account 40 groups (24 e2e) → 16 (0 e2e). |
| **C** | `AGGREGATION-DRIFT` | Per-model rollup grouped by the account bead's **optional, uncorrected** `model` field (`costs.go:483`, schema `config.go:731` — not required) → one `"unknown"` bucket. Grafana panel read `gardenio_agent_spend_usd_total` whose `model` label came from the account default not the effective model (`main.go:1041-1052`). | `ka-0000006501` → !6188 | **FIXED** — By-model now groups by `gardenio_tag_cost_usd{dim="model"}` (`costs.go:993`), the corrected effective-model tag. |
| **C** | `STALE/FROZEN` | *(+1, discovered mid-epic — the highest-impact drift.)* "Settled cost" surfaces read the dead `gardenio_agent_spend_usd_total` counter — $36 across 11 series vs a $39,964 bill (~1000× low) (`costs.go:144-152`, `costs_rollup.go:68`). | `ka-0000002699` → !6192 | **FIXED** — explorer + fallbacks repointed onto `gardenio_agent_spend_logs_usd` (`costs_rollup.go:90`); dead-counter panel-5 embed removed. |

Defect 5 in the epic (*"just a lot of cleanup … make a plan"*) is this epic + this
loop; it has no single file:line and is not a cross-checkable number, so it is
tracked as the whole rubric rather than a table row. Two live siblings remain in
flight and are **expected findings**, not regressions, until they close:
`ka-0000000543` (settled-vs-bill genuinely widened — a real drift, distinct from
the display-floor bug) and `ka-0000007296` (P7 estimate-vs-settled NO-DATA since
07-20).

## Judge → file → report (mirrors GRADE steps 4-6)

- **Judge CRISP** — score C/R/I/S/P 0-10 off the objective `crosscheck.json` plus
  the captured PNGs (you MUST look at both viewports before scoring P). Write
  `verdicts.json` with `{score, verdict, observations, issues[], recommendations[]}`
  per dimension. C is drawn straight from the cross-check; the bar is 8.
- **Ideate + file** — one bug bead per dimension scoring `< 8` and one per distinct
  accuracy-drift row, parented to `ka-0000006115`, labelled `area:observability`
  + `kind:crisp` + the dimension. De-dupe against still-open `kind:crisp` beads —
  comment a new data point rather than refiling. (Unlike GRADE, the "ideate" half
  is lighter: `/costs` is a fixed page, so new-view ideas are a short catalogue of
  missing honest cost cuts, not the headline output.)
- **Report** — land `docs/costs-crisp/<ITERATION>/REPORT.md` + `captures/` +
  `crosscheck.json` + `verdicts.json` on `main` via an MR so iterations diff, and
  post the scorecard one-liner + filed bead IDs + MR link as a comment on the
  epic. Header table, headline verdict + overall mean with delta vs the prior
  iteration, the CRISP scorecard, the cross-check verdict-class counts with the
  worst rows called out (the operator's priority), and per-dimension prose.

## Running it

```bash
# One-shot manual pour (fill VIEWER_URL/BEADS_URL from the port-forwards in §Access):
kd formula apply ka-0000007333 \
  --var ITERATION=$(date -u +%Y-%m-%dT%H%M) \
  --var PARENT_EPIC=ka-0000006115 \
  --var VIEWER_URL=http://localhost:8095 \
  --var BEADS_URL=http://localhost:8080 \
  --var VM_URL=http://gardenio-vmsingle:8428

# Or flip the continual gardener on for the daily cadence (operator go-ahead only):
#   enable ka-0000006654  (cron suggested 23 9 * * * UTC — offset from GRADE's 23 7)
```

The continual gardener (`ka-0000006654`) ships **`enabled=false`** and is flipped
only on Matt's explicit go-ahead — the same posture GRADE shipped with.
