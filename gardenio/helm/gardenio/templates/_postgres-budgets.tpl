{{- /*
Postgres efficiency budgets (gd-ylgk1jBhLi, epic gd-PkZRCb0BuB): recording
and alerting rules on the postgres_exporter sidecar's series
(templates/postgres/exporter-configmap.yaml), rendered as the vmalert
ConfigMap key postgres-budgets.yaml. Thresholds live in
vmalert.postgresBudgets.

  a PostgresQueryDominatesExecTime  one statement is > shareRatio of the hour's exec time
  b PostgresSlowFrequentQuery       mean > slowMeanSeconds over the hour with > slowMinCallsPerHour calls
  c PostgresLongActiveQuery         an active query older than longQuerySeconds
    PostgresQueryPileUp             more than pileUpBackends copies of one queryid running
  d PostgresTableGrowth             a table grew > growthRatio or > growthBytes in 24h
  e PostgresBeadsCacheHitLow        beads cache hit ratio < cacheHitRatio
  f PostgresCPUSaturatingNode       the postgres container uses > cpuNodeShare of its node

"Total exec time" is the sum over the exporter's top statements (topStatements,
default 50), which covers nearly all of it. Statements matching
excludeQueryRegex (pg_dump's COPY ... TO stdout) still count in the total but
never breach a/b themselves, and backends of excludeApps never breach c: a
backup is expected to be long and heavy. Alerts read the raw series rather
than the recorded ones, because vmalert reads a recorded series back from
vmsingle only after it is written. The daily report and task filer
(controller internal/pgbudget) apply the same thresholds per day.
*/ -}}

{{- /* . is extra label matchers ("" for none). */ -}}
{{- define "gardenio.postgresBudgets.execInc" -}}
sum by (datname, queryid, query) (increase(pg_top_statements_exec_seconds{component="postgres"{{ with . }},{{ . }}{{ end }}}[1h]))
{{- end -}}
{{- define "gardenio.postgresBudgets.callsInc" -}}
sum by (datname, queryid, query) (increase(pg_top_statements_calls{component="postgres"}[1h]))
{{- end -}}
{{- define "gardenio.postgresBudgets.tableGrowth" -}}
(sum by (datname, schemaname, relname) (pg_top_tables_total_bytes{component="postgres"}) - sum by (datname, schemaname, relname) (pg_top_tables_total_bytes{component="postgres"} offset 24h))
{{- end -}}

{{- define "gardenio.postgresBudgets.groups" -}}
{{- $b := .Values.vmalert.postgresBudgets -}}
{{- $ga := .Values.vmalert.gapAlerts -}}
{{- $dashBase := $ga.grafanaBaseURL | trimSuffix "/" -}}
{{- $runbook := printf "%s#postgres-efficiency-budgets" $ga.runbookBaseURL -}}
{{- $keep := printf "query!~%q" $b.excludeQueryRegex -}}
{{- $exec := include "gardenio.postgresBudgets.execInc" "" -}}
{{- $execKept := include "gardenio.postgresBudgets.execInc" $keep -}}
{{- $calls := include "gardenio.postgresBudgets.callsInc" . -}}
{{- $growth := include "gardenio.postgresBudgets.tableGrowth" . -}}
{{- $total := `sum(increase(pg_top_statements_exec_seconds{component="postgres"}[1h]))` -}}
- name: gardenio.postgres-budgets
  interval: {{ .Values.vmalert.evaluationInterval | quote }}
  rules:
    # For dashboards and ad hoc reads; the alerts below do not chain on them.
    - record: gardenio:pg_statement_exec_seconds:increase1h
      expr: {{ $exec }}
    - record: gardenio:pg_statement_calls:increase1h
      expr: {{ $calls }}
    - record: gardenio:pg_table_growth_bytes:delta24h
      expr: {{ $growth }}
    - record: gardenio:pg_node_cpu_share:ratio5m
      expr: sum(rate(pg_cpu_container_usage_seconds{component="postgres"}[5m])) / sum(rate(pg_cpu_node_seconds{component="postgres"}[5m]))
    # a. The total floor keeps an idle database, where any one statement is
    # most of a few seconds, quiet.
    - alert: PostgresQueryDominatesExecTime
      expr: |
        ({{ $execKept }} / on () group_left {{ $total }} > {{ $b.shareRatio }})
        and on () ({{ $total }} > {{ $b.shareMinTotalSeconds }})
      for: {{ $b.for | quote }}
      labels:
        severity: {{ $b.severity | quote }}
        component: postgres
        subsystem: postgres-budget
        budget: query-share
      annotations:
        summary: "one statement is {{`{{ $value | humanizePercentage }}`}} of postgres exec time over 1h ({{`{{ $labels.datname }}`}})"
        description: |
          queryid {{`{{ $labels.queryid }}`}} in {{`{{ $labels.datname }}`}} used more than
          {{ mulf $b.shareRatio 100 }}% of the last hour's statement execution time:
          {{`{{ $labels.query }}`}}
          Make the query cheaper or call it less; the daily efficiency report
          files a task when this holds two days running.
        runbook_url: {{ $runbook | quote }}
        {{- if $dashBase }}
        dashboard: "{{ $dashBase }}/d/gardenio-postgres-efficiency/postgres-efficiency?var-datname={{`{{ $labels.datname }}`}}&from=now-6h&to=now"
        {{- end }}
    # b. Mean over the hour, not pg_stat_statements' lifetime mean, so a fixed
    # query clears within the hour.
    - alert: PostgresSlowFrequentQuery
      expr: |
        ({{ $execKept }} / {{ $calls }} > {{ $b.slowMeanSeconds }})
        and {{ $calls }} > {{ $b.slowMinCallsPerHour }}
      for: {{ $b.for | quote }}
      labels:
        severity: {{ $b.severity | quote }}
        component: postgres
        subsystem: postgres-budget
        budget: slow-query
      annotations:
        summary: "a statement averages {{`{{ $value | humanizeDuration }}`}} over 1h with over {{ $b.slowMinCallsPerHour }} calls ({{`{{ $labels.datname }}`}})"
        description: |
          queryid {{`{{ $labels.queryid }}`}} in {{`{{ $labels.datname }}`}} averaged more
          than {{ $b.slowMeanSeconds }}s per call over the last hour, and ran more than
          {{ $b.slowMinCallsPerHour }} times: {{`{{ $labels.query }}`}}
        runbook_url: {{ $runbook | quote }}
        {{- if $dashBase }}
        dashboard: "{{ $dashBase }}/d/gardenio-postgres-efficiency/postgres-efficiency?var-datname={{`{{ $labels.datname }}`}}&from=now-6h&to=now"
        {{- end }}
    # c. max_query_age_seconds is non-zero only for state=active.
    - alert: PostgresLongActiveQuery
      expr: |
        max by (datname, application_name) (pg_activity_max_query_age_seconds{component="postgres",state="active",application_name!~{{ $b.excludeApps | quote }}})
          > {{ $b.longQuerySeconds }}
      for: {{ $b.longQueryFor | quote }}
      labels:
        severity: {{ $b.severity | quote }}
        component: postgres
        subsystem: postgres-budget
        budget: long-query
      annotations:
        summary: "{{`{{ $labels.application_name }}`}} has a query active for {{`{{ $value | humanizeDuration }}`}} in {{`{{ $labels.datname }}`}}"
        description: |
          A {{`{{ $labels.application_name }}`}} backend in {{`{{ $labels.datname }}`}} has run
          one statement for more than {{ $b.longQuerySeconds }}s. Long queries hold
          connections and snapshots; a client timeout shorter than the query
          leaves it running server-side. pg_active_statements_max_age_seconds
          names the queryid.
        runbook_url: {{ $runbook | quote }}
        {{- if $dashBase }}
        dashboard: "{{ $dashBase }}/d/gardenio-postgres-efficiency/postgres-efficiency?var-datname={{`{{ $labels.datname }}`}}&from=now-1h&to=now"
        {{- end }}
    - alert: PostgresQueryPileUp
      expr: |
        sum by (datname, queryid) (pg_active_statements_backends{component="postgres",application_name!~{{ $b.excludeApps | quote }}})
          > {{ $b.pileUpBackends }}
      for: {{ $b.pileUpFor | quote }}
      labels:
        severity: {{ $b.severity | quote }}
        component: postgres
        subsystem: postgres-budget
        budget: pile-up
      annotations:
        summary: "{{`{{ $value }}`}} copies of queryid {{`{{ $labels.queryid }}`}} are running at once in {{`{{ $labels.datname }}`}}"
        description: |
          More than {{ $b.pileUpBackends }} backends are running the same statement
          (queryid {{`{{ $labels.queryid }}`}}). Callers are retrying or polling
          faster than it completes. pg_active_statements_backends by
          application_name names the caller.
        runbook_url: {{ $runbook | quote }}
        {{- if $dashBase }}
        dashboard: "{{ $dashBase }}/d/gardenio-postgres-efficiency/postgres-efficiency?var-datname={{`{{ $labels.datname }}`}}&from=now-1h&to=now"
        {{- end }}
    # d. A table new to the exporter's top tables has no sample a day back, so
    # it is not judged until it has one. growthMinBytes keeps a small table's
    # percentage swings quiet.
    - alert: PostgresTableGrowth
      expr: |
        {{ $growth }} > {{ $b.growthBytes | int64 }}
        or
        (
          {{ $growth }}
            > {{ $b.growthRatio }} * sum by (datname, schemaname, relname) (pg_top_tables_total_bytes{component="postgres"} offset 24h)
          and sum by (datname, schemaname, relname) (pg_top_tables_total_bytes{component="postgres"}) > {{ $b.growthMinBytes | int64 }}
        )
      for: {{ $b.growthFor | quote }}
      labels:
        severity: {{ $b.severity | quote }}
        component: postgres
        subsystem: postgres-budget
        budget: table-growth
      annotations:
        summary: "{{`{{ $labels.datname }}`}}.{{`{{ $labels.relname }}`}} grew {{`{{ $value | humanize1024 }}`}}B in 24h"
        description: |
          {{`{{ $labels.datname }}`}}.{{`{{ $labels.schemaname }}`}}.{{`{{ $labels.relname }}`}} grew
          more than {{ mulf $b.growthRatio 100 }}% or {{ div ($b.growthBytes | int64) 1048576 }} MiB in the last
          24h (tables, indexes and TOAST). Look for a write path that stores more
          than it needs, or rows nothing prunes.
        runbook_url: {{ $runbook | quote }}
        {{- if $dashBase }}
        dashboard: "{{ $dashBase }}/d/gardenio-postgres-efficiency/postgres-efficiency?var-datname={{`{{ $labels.datname }}`}}&from=now-7d&to=now"
        {{- end }}
    # e.
    - alert: PostgresBeadsCacheHitLow
      expr: |
        sum(rate(pg_stat_database_blks_hit{component="postgres",datname={{ $b.cacheHitDatabase | quote }}}[5m]))
          / (sum(rate(pg_stat_database_blks_hit{component="postgres",datname={{ $b.cacheHitDatabase | quote }}}[5m])) + sum(rate(pg_stat_database_blks_read{component="postgres",datname={{ $b.cacheHitDatabase | quote }}}[5m])))
          < {{ $b.cacheHitRatio }}
      for: {{ $b.cacheHitFor | quote }}
      labels:
        severity: {{ $b.severity | quote }}
        component: postgres
        subsystem: postgres-budget
        budget: cache-hit
        datname: {{ $b.cacheHitDatabase | quote }}
      annotations:
        summary: "{{ $b.cacheHitDatabase }} cache hit ratio is {{`{{ $value | humanizePercentage }}`}}"
        description: |
          Under {{ mulf $b.cacheHitRatio 100 }}% of {{ $b.cacheHitDatabase }}'s block reads were served
          from shared_buffers for {{ $b.cacheHitFor }}. Usually a scan over a table
          bigger than the cache; the efficiency board's blocks-read panel names
          the statement.
        runbook_url: {{ $runbook | quote }}
        {{- if $dashBase }}
        dashboard: "{{ $dashBase }}/d/gardenio-postgres-efficiency/postgres-efficiency?var-datname={{ $b.cacheHitDatabase }}&from=now-6h&to=now"
        {{- end }}
    # f. pg_cpu_node_seconds counts every CPU of the node, so the ratio is the
    # share of the whole node.
    - alert: PostgresCPUSaturatingNode
      expr: |
        sum(rate(pg_cpu_container_usage_seconds{component="postgres"}[5m])) / sum(rate(pg_cpu_node_seconds{component="postgres"}[5m]))
          > {{ $b.cpuNodeShare }}
      for: {{ $b.cpuFor | quote }}
      labels:
        severity: {{ $b.severity | quote }}
        component: postgres
        subsystem: postgres-budget
        budget: cpu
      annotations:
        summary: "postgres is using {{`{{ $value | humanizePercentage }}`}} of its node's CPU"
        description: |
          The postgres container has used more than {{ mulf $b.cpuNodeShare 100 }}% of its node's
          CPU for {{ $b.cpuFor }}. Every query on the box slows down. Find the
          statements with the most exec time; the fix is cheaper queries, not a
          bigger node.
        runbook_url: {{ $runbook | quote }}
        {{- if $dashBase }}
        dashboard: "{{ $dashBase }}/d/gardenio-postgres-efficiency/postgres-efficiency?from=now-6h&to=now"
        {{- end }}
{{- end -}}
