{{- /*
Lands SLOs (Track B spec §5.8, step 10.8, gd-rqtbyqCMZJ): the SLIs as
recording rules and a multi-window, multi-burn-rate alert pair per objective
(Google SRE workbook, ch. 5). Shared by the PrometheusRule CRD
(templates/lands-slo-rules.yaml) and the vmalert ConfigMap key
(templates/vmalert/configmap.yaml, lands-slo.yaml), the only rule path that
fires on a cluster without a VMAlert behind the CRDs.

Every objective in agents.metrics.prometheusRule.landsSLO.objectives records
gardenio:lands_slo_errors:ratio_rate<window>{slo="<key>"}, the fraction of
bad events in that window, for each window below, plus
gardenio:lands_slo:objective{slo} (the target, for dashboards and error-budget
queries). Three kinds of SLI:

  probe     the off-cluster availability prober (scripts/uptime-probe):
            failed probes / probes for one target, read from the status
            bucket's metrics file (vmagent.uptimeProbeScrape).
  latency   a histogram: events slower than thresholdSeconds / events.
            thresholdSeconds must be one of the histogram's bucket bounds.
  requests  beads-viewer's gardenio_user_request_latency_seconds: 5xx
            responses / responses for the endpoints matching `endpoints`.
            A surface that never answered a 5xx has no 5xx series at all,
            hence the `or vector(0)`: it reads 0, not nothing.

Alerts, burn rate B = error ratio / (1 - target):
  LandsSLOFastBurn (pageSeverity)    B > 14.4 over 1h and 5m, or B > 6 over 6h and 30m
  LandsSLOSlowBurn (ticketSeverity)  B > 3 over 1d and 2h,  or B > 1 over 3d and 6h
A 99.5% objective spends 2% of its 30-day budget in the 1h a fast burn needs.
*/ -}}

{{- define "gardenio.landsSLO.errorRatio" -}}
{{- $o := .o -}}{{- $w := .w -}}
{{- if eq $o.kind "probe" -}}
sum(increase(gardenio_uptime_probe_failures_total{target="{{ $o.probeTarget }}"}[{{ $w }}]))
/
sum(increase(gardenio_uptime_probe_total{target="{{ $o.probeTarget }}"}[{{ $w }}]))
{{- else if eq $o.kind "latency" -}}
{{- $sel := "" }}{{ with $o.selector }}{{ $sel = printf ",%s" . }}{{ end -}}
1 - (
  sum(increase({{ $o.metric }}_bucket{le=~"{{ $o.thresholdSeconds }}|{{ $o.thresholdSeconds }}\\.0"{{ $sel }}}[{{ $w }}]))
  /
  sum(increase({{ $o.metric }}_count{{ with $o.selector }}{ {{- . -}} }{{ end }}[{{ $w }}]))
)
{{- else if eq $o.kind "requests" -}}
(sum(increase(gardenio_user_request_latency_seconds_count{endpoint=~"{{ $o.endpoints }}",status_code=~"5.."}[{{ $w }}])) or vector(0))
/
sum(increase(gardenio_user_request_latency_seconds_count{endpoint=~"{{ $o.endpoints }}"}[{{ $w }}]))
{{- else -}}
{{- fail (printf "landsSLO.objectives.%s: kind must be probe, latency or requests, got %q" .name $o.kind) -}}
{{- end -}}
{{- end -}}

{{- define "gardenio.landsSLO.groups" -}}
{{- $cfg := .Values.agents.metrics.prometheusRule.landsSLO -}}
{{- $short := list "5m" "30m" "1h" "2h" "6h" -}}
{{- $long := list "1d" "3d" "30d" -}}
{{- range $name, $o := $cfg.objectives }}
{{- if $o.enabled }}
{{- if not (regexMatch "^[a-z][a-z0-9_]*$" $name) }}{{ fail (printf "landsSLO.objectives: key %q must be snake_case (it is the slo label)" $name) }}{{ end }}
{{- if or (le ($o.target | float64) 0.0) (ge ($o.target | float64) 1.0) }}{{ fail (printf "landsSLO.objectives.%s.target must be between 0 and 1, got %v" $name $o.target) }}{{ end }}
{{- if and (eq $o.kind "probe") (not $o.probeTarget) }}{{ fail (printf "landsSLO.objectives.%s: a probe objective needs probeTarget" $name) }}{{ end }}
{{- if and (eq $o.kind "latency") (or (not $o.metric) (not $o.thresholdSeconds)) }}{{ fail (printf "landsSLO.objectives.%s: a latency objective needs metric and thresholdSeconds" $name) }}{{ end }}
{{- if and (eq $o.kind "requests") (not $o.endpoints) }}{{ fail (printf "landsSLO.objectives.%s: a requests objective needs endpoints" $name) }}{{ end }}
{{- end }}
{{- end }}
- name: gardenio.lands_slo.sli
  interval: 1m
  rules:
    {{- range $name, $o := $cfg.objectives }}
    {{- if $o.enabled }}
    - record: gardenio:lands_slo:objective
      expr: vector({{ $o.target }})
      labels:
        slo: {{ $name }}
    {{- range $w := $short }}
    - record: gardenio:lands_slo_errors:ratio_rate{{ $w }}
      expr: |
        {{- include "gardenio.landsSLO.errorRatio" (dict "o" $o "w" $w "name" $name) | nindent 8 }}
      labels:
        slo: {{ $name }}
    {{- end }}
    {{- end }}
    {{- end }}
# The long windows change slowly; evaluating them every 5m keeps the 30d
# increase() cheap.
- name: gardenio.lands_slo.sli_long
  interval: 5m
  rules:
    {{- range $name, $o := $cfg.objectives }}
    {{- if $o.enabled }}
    {{- range $w := $long }}
    - record: gardenio:lands_slo_errors:ratio_rate{{ $w }}
      expr: |
        {{- include "gardenio.landsSLO.errorRatio" (dict "o" $o "w" $w "name" $name) | nindent 8 }}
      labels:
        slo: {{ $name }}
    {{- end }}
    {{- end }}
    {{- end }}
- name: gardenio.lands_slo.alerts
  interval: 1m
  rules:
    {{- range $name, $o := $cfg.objectives }}
    {{- if $o.enabled }}
    {{- $budget := printf "(1 - %v)" $o.target }}
    {{- $sel := printf "{slo=\"%s\"}" $name }}
    - alert: LandsSLOFastBurn
      expr: |
        (
          gardenio:lands_slo_errors:ratio_rate1h{{ $sel }} > (14.4 * {{ $budget }})
          and
          gardenio:lands_slo_errors:ratio_rate5m{{ $sel }} > (14.4 * {{ $budget }})
        )
        or
        (
          gardenio:lands_slo_errors:ratio_rate6h{{ $sel }} > (6 * {{ $budget }})
          and
          gardenio:lands_slo_errors:ratio_rate30m{{ $sel }} > (6 * {{ $budget }})
        )
      for: {{ $cfg.fastBurnFor | default "2m" | quote }}
      labels:
        severity: {{ $o.pageSeverity | default $cfg.pageSeverity | quote }}
        component: gardenio
        subsystem: lands-slo
        slo: {{ $name }}
        {{- with $cfg.labels }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      annotations:
        summary: "SLO {{ $name }} is burning its error budget fast ({{ $o.description }})"
        description: |
          {{ $name }} ({{ $o.description }}, objective {{ $o.target }}) is spending its
          30-day error budget at more than 14.4x over the last hour (or 6x over 6 hours):
          {{`{{ $value | humanizePercentage }}`}} of events bad in the window that fired.
          At this rate the month's budget is gone in about two days.
          {{- if eq $o.kind "probe" }}
          Check the off-cluster status page and gb lands status; the prober's
          last detail per target is in status.json.
          {{- end }}
          Query: gardenio:lands_slo_errors:ratio_rate1h{slo="{{ $name }}"}
        {{- with $cfg.runbookURL }}
        runbook_url: {{ printf "%s#%s" . (lower $name) | quote }}
        {{- end }}
        {{- with $cfg.annotations }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
    - alert: LandsSLOSlowBurn
      expr: |
        (
          gardenio:lands_slo_errors:ratio_rate1d{{ $sel }} > (3 * {{ $budget }})
          and
          gardenio:lands_slo_errors:ratio_rate2h{{ $sel }} > (3 * {{ $budget }})
        )
        or
        (
          gardenio:lands_slo_errors:ratio_rate3d{{ $sel }} > (1 * {{ $budget }})
          and
          gardenio:lands_slo_errors:ratio_rate6h{{ $sel }} > (1 * {{ $budget }})
        )
      for: {{ $cfg.slowBurnFor | default "15m" | quote }}
      labels:
        severity: {{ $o.ticketSeverity | default $cfg.ticketSeverity | quote }}
        component: gardenio
        subsystem: lands-slo
        slo: {{ $name }}
        {{- with $cfg.labels }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      annotations:
        summary: "SLO {{ $name }} is burning its error budget ({{ $o.description }})"
        description: |
          {{ $name }} ({{ $o.description }}, objective {{ $o.target }}) is spending its
          30-day error budget faster than it earns it: more than 3x over a day, or
          more than 1x over three days. {{`{{ $value | humanizePercentage }}`}} of
          events bad in the window that fired. Not urgent; find the cause this week.
          Query: gardenio:lands_slo_errors:ratio_rate1d{slo="{{ $name }}"}
        {{- with $cfg.runbookURL }}
        runbook_url: {{ printf "%s#%s" . (lower $name) | quote }}
        {{- end }}
        {{- with $cfg.annotations }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
    {{- end }}
    {{- end }}
    {{- if and $cfg.probeStale.enabled $.Values.vmagent.uptimeProbeScrape.enabled }}
    # The probe SLIs are only as fresh as the status bucket's metrics file.
    # A prober that stopped, or a scrape that cannot reach the bucket, leaves
    # them flat, and a flat counter burns nothing: this says so.
    - alert: LandsUptimeProbeStale
      expr: |
        (time() - max(gardenio_uptime_probe_last_run_timestamp_seconds)) > {{ $cfg.probeStale.seconds }}
        or
        absent(gardenio_uptime_probe_last_run_timestamp_seconds)
      for: {{ $cfg.probeStale.for | default "10m" | quote }}
      labels:
        severity: {{ $cfg.probeStale.severity | default $cfg.ticketSeverity | quote }}
        component: gardenio
        subsystem: lands-slo
        {{- with $cfg.labels }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      annotations:
        summary: "The off-cluster availability prober has not reported for over {{ $cfg.probeStale.seconds }}s"
        description: |
          gardenio_uptime_probe_last_run_timestamp_seconds, scraped from the status
          bucket's metrics file, is older than {{ $cfg.probeStale.seconds }}s or missing.
          Either the gardenio-uptime-probe Lambda stopped (its heartbeat alarm
          emails too) or vmagent cannot reach vmagent.uptimeProbeScrape.url. Until
          it is back, the probe SLOs cannot burn and the status page may be stale.
        {{- with $cfg.annotations }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
    {{- end }}
{{- end -}}
