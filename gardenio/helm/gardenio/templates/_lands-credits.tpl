{{- /*
Tenant land credit series (plan gd-gh5J0vVbJP step 3.2, gd-DxFIK7tips):
vmalert recording rules that turn a land's USD bill into credits, so the
tenant charts (3.3, GET /garden/charts/costs-land) read only credit series and
no tenant panel ever carries USD. Rendered as the vmalert ConfigMap key
lands-credits.yaml (templates/vmalert/configmap.yaml). Runbook:
gardenio/docs/e8-recording-rules-runbook.md, "Land credit series".

Inputs:
  gardenio_account_cost_usd{account="lands-<land>"}  the land account's
      lifetime LiteLLM bill (the purse's total_cost_usd), a gauge.
  gardenio_agent_spend_logs_usd{account="lands-<land>"}  per-agent lifetime
      USD from the spend-logs sweep (Reset+Set, ~48 h lookback slice).
  gardenio_lands_credit_rate{land}  credits per US$1, the llm_multiplier of
      the rate in force (controller, internal/metrics/lands_credits.go).
  gardenio_lands_credits_granted{land}  the sum of the land's grants.

Credits = USD x the rate in force, as the purse computes them
(internal/lands/purse): a rate change reprices the whole history, never only
what came after it. So every window takes the USD increase first and
multiplies by the rate now; the rate is never inside a window. Only lands with
a rate series (tenant lands, a rate in force) get a credit series: the
`* on (land)` join drops the rest.

Neither engine's increase() is used: it extrapolates in Prometheus and not in
VictoriaMetrics, so the same rule would read differently in promtool and in
production. A trailing window is the gauge now less its lowest value in the
window (it only climbs, so that is its first), which reads each raw sample
once. The window is 30s longer than its name (1h30s), so with samples on the
minute the hour-ago sample is inside it under either range convention
(Prometheus 2.x includes a range's start; 3.x and VictoriaMetrics do not). A
re-minted key resets the gauge to 0; the window then counts only the new key,
low, never negative.

"today" is since 00:00 UTC, as the garden's today chip is. A rule cannot take
a window that depends on the clock, so "today" is a sum of 5-minute
increments over an aligned subquery, each weighted by the UTC day it fell in:
with d the day number of an increment's step and d0 today's, the sum of
increment x (d - d0 + 1) over a window under a day long counts today's
increments once and yesterday's not at all. d0 is a constant of each
evaluation, so that is W - (d0 - 1) x S, with W the day-weighted sum and S the
plain sum, and each is an ordinary sum_over_time. An increment's step is
00:05 for 00:00-00:05, so a step's day is the day of (step - 1s): the 00:00
step belongs to yesterday. The window is 1439m, which always holds today's
first step and never the day before yesterday's last, under either range
convention. An increment is clamped at 0 (the step with a reset counts
nothing) and bridges a scrape gap of up to 30 minutes.

Rules in one group are not chained: vmalert reads a recorded series back from
vmsingle after a remote write, so a rule built on another would lag it by an
evaluation. Each expression is built from the inputs.
*/ -}}

{{- /* Every land account's USD gauge. */ -}}
{{- define "gardenio.landsCredits.usdSelector" -}}
gardenio_account_cost_usd{account=~"lands-.+"}
{{- end -}}

{{- /* account → land */ -}}
{{- define "gardenio.landsCredits.toLand" -}}
max by (land) (label_replace({{ . }}, "land", "$1", "account", "lands-(.+)"))
{{- end -}}

{{- define "gardenio.landsCredits.rate" -}}
max by (land) (gardenio_lands_credit_rate)
{{- end -}}

{{- /* One 5-minute increment of a land account's USD, by account. */ -}}
{{- define "gardenio.landsCredits.increment" -}}
{{- $sel := include "gardenio.landsCredits.usdSelector" . -}}
clamp_min(max by (account) (last_over_time({{ $sel }}[5m])) - max by (account) (last_over_time({{ $sel }}[30m] offset 5m)), 0)
{{- end -}}

{{- /* Credits spent in a trailing window: dict "ctx" . "w" "7d30s" */ -}}
{{- define "gardenio.landsCredits.window" -}}
{{- $sel := include "gardenio.landsCredits.usdSelector" .ctx -}}
{{- include "gardenio.landsCredits.toLand" (printf "max by (account) (%s) - min by (account) (min_over_time(%s[%s]))" $sel $sel .w) }}
* on (land)
{{ include "gardenio.landsCredits.rate" .ctx }}
{{- end -}}

{{- define "gardenio.landsCredits.spent" -}}
{{- include "gardenio.landsCredits.toLand" (printf "max by (account) (%s)" (include "gardenio.landsCredits.usdSelector" .)) }}
* on (land)
{{ include "gardenio.landsCredits.rate" . }}
{{- end -}}

{{- define "gardenio.landsCredits.groups" -}}
{{- $inc := include "gardenio.landsCredits.increment" . -}}
{{- $day := "(((time() - 1) - (time() - 1) % 86400) / 86400)" -}}
{{- $today := "((time() - time() % 86400) / 86400)" -}}
- name: gardenio.lands_credits
  interval: {{ .Values.vmalert.evaluationInterval | quote }}
  rules:
    # Credits spent to date: USD to date x the rate. The purse's spent_credits
    # before in-flight (GET /api/lands/{land}/purse).
    - record: gardenio:land_spend_credits:total
      expr: |
        {{- include "gardenio.landsCredits.spent" . | nindent 8 }}
    # Credits granted: the land's ledger, as the purse sums it.
    - record: gardenio:land_credits:granted
      expr: |
        max by (land) (gardenio_lands_credits_granted)
    # Credits remaining = granted - spent. The purse's remaining_credits also
    # takes off in-flight agent cost, which only it can see; this is the
    # settled remaining, so it can read a little above the land header.
    - record: gardenio:land_credits:remaining
      expr: |
        max by (land) (gardenio_lands_credits_granted)
        -
        (
          {{- include "gardenio.landsCredits.spent" . | nindent 10 }}
        )
    # Burn rate, credits per hour: the trailing hour, and the trailing day's
    # hourly mean (the run-out projection on the burn chart).
    - record: gardenio:land_spend_credits:rate1h
      expr: |
        {{- include "gardenio.landsCredits.window" (dict "ctx" . "w" "1h30s") | nindent 8 }}
    - record: gardenio:land_spend_credits:rate1d
      expr: |
        (
          {{- include "gardenio.landsCredits.window" (dict "ctx" . "w" "1d30s") | nindent 10 }}
        ) / 24
    # Credits per agent, from the spend-logs gauge: an instant read of the
    # sweep's ~48 h lookback slice (increase() over this Reset+Set gauge is
    # garbage), keyed by the land the agent bills to.
    - record: gardenio:land_spend_credits:by_agent
      expr: |
        sum by (land, agent) (label_replace(max by (agent, project, role, model, account) (gardenio_agent_spend_logs_usd{account=~"lands-.+"}), "land", "$1", "account", "lands-(.+)"))
        * on (land) group_left
        {{ include "gardenio.landsCredits.rate" . }}
# "today" and the long windows move no faster than the 5-minute spend sync;
# every 5m keeps today's 288-step subquery cheap.
- name: gardenio.lands_credits.long
  interval: 5m
  rules:
    # Credits spent since 00:00 UTC (see the day weighting above).
    - record: gardenio:land_spend_credits:today
      expr: |
        {{- $w := printf "sum_over_time((%s * %s)[1439m:5m])" $inc $day }}
        {{- $s := printf "sum_over_time((%s)[1439m:5m])" $inc }}
        {{- include "gardenio.landsCredits.toLand" (printf "clamp_min(%s - (%s - 1) * %s, 0)" $w $today $s) | nindent 8 }}
        * on (land)
        {{ include "gardenio.landsCredits.rate" . }}
    - record: gardenio:land_spend_credits:increase7d
      expr: |
        {{- include "gardenio.landsCredits.window" (dict "ctx" . "w" "7d30s") | nindent 8 }}
    - record: gardenio:land_spend_credits:increase30d
      expr: |
        {{- include "gardenio.landsCredits.window" (dict "ctx" . "w" "30d30s") | nindent 8 }}
{{- end -}}
