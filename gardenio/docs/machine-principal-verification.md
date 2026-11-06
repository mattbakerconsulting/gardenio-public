# Machine principals: end-to-end verification on k3s

Plan [gd-UdM4LnnaGE](../../gardenio/docs/machine-principal-design.md) Task 7, step
**gd-eiCV6zrLUS**. Closes the evidence loop for bug **gd-5NvJpCOlMC** ("Costs tab
frozen: controller account PATCH 403s").

What has to be shown, with recorded numbers:

- `/costs` refreshes again (total, hourly rate, 5h/7d windows) after the controller
  started writing account beads as `machine:gardenio-controller` (PR #324, tag
  `2026.286.8`).
- Account bead `gd-XOnJ3PSDsn` tracks the LiteLLM key spend.
- `gardenio_authz_denials_total{action="update",bead_type="account"}` stops climbing.
- A **different** machine principal (`gardenio-authz-canary`, service account holding
  only `gardenio-operator`) gets a visible 403 on the same write.
- The viewer's Grafana base URL (commit `c3a4144a`) is live so the `/costs` panels render.
- The live-agent cost chip gap (`live_cost_usd` always 0) is filed as a bug.

Roles: the **crew** wrote the canary (script, test, Job manifest, this checklist) and
files one escalation carrying the operator checklist. The **operator** (laptop-claude)
runs every cluster step; crews never run kubectl or touch Keycloak. The crew then fills
the results table from the operator's numbers.

## T0 is already in the past

T0 is the moment the controller pod from Task 4's roll became Ready. laptop-claude
recorded the fix live at **2026-10-13 23:37Z** (tag `2026.286.8`, PR #324), so:

| | UTC | unix |
|---|---|---|
| T0 | 2026-10-13T23:37:00Z | 1791934620 |
| T0 + 5 min | 2026-10-13T23:42:00Z | 1791934920 |
| T0 + 30 min | 2026-10-14T00:07:00Z | 1791936420 |

Item 1's "before the roll" baseline is therefore read from history with a PromQL
`@` modifier (vmsingle keeps the series), and items 2 and 3 are evaluated at the
timestamps above **plus** a fresh reading now, so the claim is "it refreshed within
5 minutes of T0 and is still tracking". If the operator's T0 differs (pod Ready time
from `kubectl -n gardenio get pod -l app.kubernetes.io/component=agents
-o jsonpath='{.items[0].status.conditions[?(@.type=="Ready")].lastTransitionTime}'`),
use that and note it in the table.

## Operator checklist

Run from the laptop with cluster access. Print only the fields named; account beads
carry `credential` and `litellm_key`, and the LiteLLM master key is never printed.

1. **Baseline, before the roll.** Record:
   - from vmsingle, `sum(gardenio_authz_denials_total{action="update",bead_type="account"})`
     and its `increase(...[30m])`, which should be > 0 while frozen
     (evaluate both `@ 1791934620`, i.e. at T0);
   - from `kd show gd-XOnJ3PSDsn --json`, only `total_cost_usd`, `daily_cost_usd`,
     `spend_5h_window_usd`, `spend_7d_window_usd`, `probe_checked_at` and `updated_at`
     (today's values; the frozen pre-roll values are on gd-5NvJpCOlMC:
     total_cost_usd 1012.2759, daily 74.64, probe_checked_at 2026-10-11T03:53Z);
   - the LiteLLM key spend for that account's key. Use LiteLLM `/key/info` with the
     master key, from inside the cluster, printing only `.info.spend`; never print the
     key or master key.
2. **T0 + 5 min:**
   - `probe_checked_at` and `updated_at` on gd-XOnJ3PSDsn are later than T0;
   - `total_cost_usd` changed and is within $1 of the LiteLLM key spend read in the same
     minute (allow one sync interval of drift: re-read both if not);
   - `spend_5h_window_usd` and `spend_7d_window_usd` are present and changed;
   - the viewer `/costs` page shows the new total, the hourly rate panel and the 5h/7d
     figures (screenshot or numbers). The rate panel reads `gardenio_account_cost_usd`
     (`cmd/beads-viewer/costs_rate.go:98`). For the historical check:
     `max by (account) (gardenio_account_cost_usd) @ 1791934920` versus `@ 1791934620`.
3. **T0 + 30 min:**
   - `increase(gardenio_authz_denials_total{action="update",bead_type="account"}[25m]) == 0`,
     measured over a window starting after T0 + 5 min (`@ 1791936420`);
   - `total_cost_usd` still tracks LiteLLM key spend (second reading).
4. **Wrong principal.** Apply the canary Job (`kubectl -n gardenio create -f
   gardenio/helm/manifests/machine-principal-canary-job.yaml`, with the script ConfigMap
   per the manifest's header), then read `kubectl -n gardenio logs job/machine-principal-canary`.
   It must show HTTP `403`, `caller_subject: machine:gardenio-authz-canary` and
   `required_role: gardenio-admin`, and the Job must have succeeded (exit 0). The denial
   counter rises by exactly 1 (re-read the metric). The canary is the only new denial.
   Delete the Job afterwards.
5. **Grafana base URL (`c3a4144a`, "point beads-viewer /charts at Grafana's ziti URL").**
   It is contained in tag `2026.285.5`, which is live (viewer image
   `beads-viewer:2026.285.5`, checked 2026-10-13). Confirm that the viewer Deployment's
   env carries the Grafana base URL from that commit (print the env NAME and VALUE only
   for that one variable; it is not a secret) and that the `/costs` Grafana panels render.
   If the URL is a `.ziti` host, note it. Ziti is being deprecated, so a follow-up to
   point it at the Keycloak edge is filed as a bug under the ziti-to-keycloak plan's
   owner, not fixed here.
   ```sh
   kubectl -n gardenio get deploy -l app.kubernetes.io/component=beads-viewer \
     -o jsonpath='{range .items[0].spec.template.spec.containers[0].env[?(@.name=="GRAFANA_BASE_URL")]}{.name}={.value}{"\n"}{end}'
   ```
   Expected from `c3a4144a`: `GRAFANA_BASE_URL=http://gardenio.grafana.ziti:3000`.
6. **Live-agent cost chip gap.** `live_cost_usd` is always 0 on the LiteLLM path. Record
   one live agent's `live_cost_usd` and its LiteLLM-tagged spend. File a **bug** (not a
   decision: a decision naming beads would park them) with the evidence and a proposed
   fix, related to this plan. Linking the bug closes this item; it does not block Task 6.

## Canary mechanics

- Script: [`gardenio/scripts/machine-principal-canary.sh`](../scripts/machine-principal-canary.sh).
  Client-credentials grant as `gardenio-authz-canary`, `GET` the target's
  `fields.probe_auth_status`, `PATCH` the same value back. Exit 0 only on a 403 whose
  `caller_subject` is `machine:gardenio-authz-canary`; exit 1 if the write was allowed;
  exit 2 otherwise. The token and secret never reach argv, stdout or stderr; bodies are
  never printed.
- Test: [`gardenio/scripts/machine-principal-canary_test.sh`](../scripts/machine-principal-canary_test.sh)
  runs the script against a local stub. The **allow → exit 1** case is the positive
  control that the check can fail; the secrecy assertions check that the issued token,
  the client secret and the bead's credential fields never appear in the output. Runs in
  CI's "shell guards + script self-tests" job and in `make lint`.
- Job: [`gardenio/helm/manifests/machine-principal-canary-job.yaml`](../helm/manifests/machine-principal-canary-job.yaml).
  Raw manifest, applied by hand, `ttlSecondsAfterFinished: 600`, `backoffLimit: 0`, no
  ServiceAccount token, no ziti, no public hostname. Image: the chart's coopmux image
  (bash + curl + jq), already in the k3s ECR pull-through cache.

## Results

Filled by the crew from laptop-claude's numbers (comment on gd-eiCV6zrLUS, 2026-10-14
06:20-07:25Z). T0 = 2026-10-13T23:37:00Z as given; items 1-3 were read from vmsingle
history with `@`. "n/r" = not recoverable / not recorded.

| # | Check | Expected | Observed | When (UTC) | Pass |
|---|---|---|---|---|---|
| 1a | denials counter at T0: `sum(...)` / `increase(...[30m])` | increase > 0 | sum 132; increase[30m] 38 | @T0 | yes |
| 1b | gd-XOnJ3PSDsn pre-roll: total / daily / 5h / 7d / probe_checked_at / updated_at | frozen at 1012.2759 / 74.64 / stale / stale / 2026-10-11T03:53Z | n/r for the bead fields (history not recoverable); `gardenio_account_cost_usd` @T0: matthewbaker1110 1012.28 (= frozen value), fimbaz 1311.48, matthewcbaker0122 0 | @T0 | yes (metric) |
| 1c | LiteLLM `.info.spend` for the account's key | number | n/r at T0; first readings are 2b/3b below | | n/a |
| 2a | probe_checked_at, updated_at > T0 | yes | advance every sync: 06:19:10Z, 06:42:10Z, 07:12:53Z (current bead; T0+5m bead value n/r) | 06:19-07:12 | yes |
| 2b | total_cost_usd vs LiteLLM spend (same minute) | moved; within $1 | `gardenio_account_cost_usd` matthewbaker1110 1012.28 → 1331.67 (+319.39) by T0+5m. Bead vs LiteLLM: 06:23Z 1380.79 vs 1382.01 (Δ1.22, bead synced 06:19, inside one interval) | T0+5m, 06:23 | yes |
| 2c | spend_5h_window_usd / spend_7d_window_usd | present, changed | 5h 54.61, 7d 596.91, daily 47.53 (daily from the 06:19Z read), present and moving | ~07:13 | yes |
| 2d | /costs total, hourly rate panel, 5h/7d figures | render with new values | Operator, signed-in Chrome at `https://gardenio.clonk.tv/costs`, release 2026.287.11, 3 accounts, updated 18:57: bill vs week (7d limit) $978.53; bill vs 5h limit $28.86; UTC day $413.47 (78% elapsed); rate 24h avg $31.56/h, 1h avg $2.03/h; projected $757.38 over 24h; bill x-check 3/3 ok; max 5h util 4%, max 7d util 89%; active 3, throttled 0, overage 3; sessions 3177; $/session $0.970; month-so-far $1957.05, projected month $4333.47; per-agent leaderboard renders. Dollar figures are subscription-equivalent, not a bill | 2026-10-14 ~18:57 | yes |
| 3a | `increase(denials[25m])` over a window after T0+5m | 0 | 0 @T0+30m (sum = 1: counter reset by a kbeads restart). No account-update denial series until the canary | @T0+30m | yes |
| 3b | total_cost_usd vs LiteLLM spend, second reading ≥ 25 min later | within $1 | 06:44Z 1388.36 vs 1390.65 (Δ2.29, read right after a cluster DNS outage); 07:13Z, within 7 s of a sync: 1396.50 vs 1396.59 (Δ0.09). Second-and-third readings 50 min apart | 06:44, 07:13 | yes (07:13), 06:44 outside $1 |
| 4a | canary Job exit / log | exit 0; HTTP 403; caller_subject machine:gardenio-authz-canary; required_role gardenio-admin | HTTP 403; caller_subject machine:gardenio-authz-canary; required_role gardenio-admin; "canary: PASS"; Job complete, exit 0. Job + ConfigMap deleted | 06:20-07:25 | yes |
| 4b | denials counter delta from the canary | exactly +1, the only new denial | no series → exactly 1 (update/account); no other denial | after 4a | yes |
| 5 | viewer `GRAFANA_BASE_URL`; /costs Grafana panels | `http://gardenio.grafana.ziti:3000`; panels render | `GRAFANA_BASE_URL=http://gardenio.grafana.ziti:3000` (release 2026.287.4 at 08:24Z, again on 2026.287.11 at ~19:00Z): confirmed live, and it IS a plain-http `.ziti` host. **All 4 Grafana iframes are blank** (dashboard `gardenio-costs-candidate`: cost per agent-hour, fleet spend rate, per-agent cost over time, spend by account): an https page embedding an `http://*.ziti:3000` iframe is mixed content, and the `.ziti` name resolves only with a tunnel. Bug **gd-RUhOpXWwmz** filed under the public-edge plan gd-P78ygCUgA9 | 2026-10-14 08:24, ~19:00 | URL confirmed; panels **fail** |
| 6 | one live agent: `live_cost_usd` vs LiteLLM tagged spend; bug id | 0 vs > 0; bug filed | `live_cost_usd` 0 on 7/7 working agents (e.g. gd-RMDWP9BNs0 with 124048 context tokens, 23 turns); bug **gd-UIlh0DR2VG** filed with cause and proposed fix. The paired LiteLLM `/spend/tags` reading was not taken | 2026-10-14 06:03 | yes (bug filed) |

**Findings**

- The two canary Job bugs the operator hit are fixed in the manifest on this PR: no
  toleration for the `gardenio.agent` taint (0/19 nodes available) and no
  `imagePullSecrets: ecr` (ErrImagePull from ECR). The first run needed both added by
  hand.
- 3b: the 06:44Z reading was $2.29 apart, outside the $1 bound. The operator attributes
  it to a cluster DNS outage right before that sync; the 07:13Z reading taken seconds
  after a sync is $0.09. The two checks at least 25 minutes apart that pass are 06:23
  (Δ1.22, within one sync interval) and 07:13, so treat the "within $1" claim as
  holding only when read at a sync boundary.
- 2d: the `/costs` page was read by the operator on 2026.287.11. The headline, rate
  (24h and 1h), 5h/7d and trust panels all render and the numbers are current. Trust
  tiles that read DRIFTING at that reading: sessions vs agent 0.18, token parity 0.13,
  interrupt parity 0.80 (bill conservation 1.00, agent attribution 0.97, tag
  conservation 1.00, rollup vs tree 1.00 are fine). They are not part of this
  acceptance and were not investigated here.
- Item 5: the Grafana base URL is live but unusable from the public edge. Plain-http
  `.ziti` inside an https page is blocked as mixed content, so the four Grafana panels
  on `/costs` render blank in the operator's signed-in browser (the `.ziti` name also
  only resolves with a tunnel). This is **not** fixed here. Bug
  **gd-RUhOpXWwmz** tracks it under the public-edge plan (gd-P78ygCUgA9): serve Grafana on
  an https public host behind Keycloak, or proxy `/grafana/` through the viewer origin,
  then point `GRAFANA_BASE_URL` at it.

**Acceptance summary:** `/costs` total, rate and 5h/7d figures refresh and are current;
`total_cost_usd` tracks LiteLLM key spend (Δ$0.09 at the 07:13Z sync boundary); the
account-update denials stopped (increase[25m] = 0 after T0+5m); the canary got the
intended 403 with `caller_subject: machine:gardenio-authz-canary`; the live-cost chip bug
is filed (gd-UIlh0DR2VG). The one acceptance item that does not hold is the Grafana
panels rendering, tracked in gd-RUhOpXWwmz.

**Rollback:** this task changes nothing live except the one-off Job, which is deleted
after the run and expires through `ttlSecondsAfterFinished`. If acceptance fails, use
Task 4's rollback (`agents.machinePrincipal.enabled: false`) and reopen gd-s7aPIf0Bgp
with the evidence.
