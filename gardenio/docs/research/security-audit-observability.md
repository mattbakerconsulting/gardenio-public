# Audit — Observability data security audit (ka-0000001349)

**Date:** 2026-04-29
**Task:** [ka-0000001349](https://beads.gardenio.e2e.example.com/beads/ka-0000001349) —
"Audit: Observability data — Mezmo logs, S3 session uploads, transcript PII,
Grafana, hippo".
**Scope:** Data shipped from this monorepo to observability sinks. Specifically:

- **Mezmo** — what credentials this chart provisions, what the agent ships (or
  doesn't ship) to Mezmo, and how the read/write paths split.
- **S3 sessions bucket** (`s3://gardenio-sessions/coop/sessions/<id>/`) — every
  artifact `coop` uploads, and the bucket-policy posture the chart assumes.
- **Grafana** (`gardenio/helm/gardenio/templates/grafana/**`) + dashboards
  (`gardenio/helm/gardenio/dashboards/`) and the VictoriaMetrics datasource
  it auto-wires.
- **Hippo memories** (`hippo/`, `.hippo/`) — does any production code path still
  ship them anywhere?

**Out of scope.** Metrics-pipeline transport (covered by
`docs/research/canonical-prod-observability.md`),
controller-side OWASP findings (covered by
`docs/audits/AUDIT-2026-04-27-controller-go.md`),
the coop S3 client itself (covered by
`docs/audits/AUDIT-2026-04-27-coop-rust.md`
§5–6) — none of the three are in this repo; the research and audits trees were
excluded from the 2026-09-16 export. This audit reads code only; no live AWS,
S3, Mezmo, or Grafana state was inspected. Anything that requires a live look
is flagged **[operator to confirm]**.

---

## TL;DR

1. **The chart ships no log-shipper.** `mezmo-credentials-external-secret.yaml`
   provisions a **read** key (`MEZMO_SERVICE_KEY`) for the Mezmo MCP tools used
   by agent pods to *query* Mezmo. The **ingest** path (LogDNA / Mezmo
   DaemonSet) lives in the acme `tenant-system` chart, not here. Greenfield
   consumers of `gardenio` get neither.
2. **Container stdout is the de-facto log surface.** Controller + slack-bridge
   write JSON-formatted slog to stdout; whatever cluster log pipe is bolted on
   downstream (CloudWatch, Mezmo, Loki) carries those lines verbatim. There is
   **no field-level redaction** at log emission. Findings R1–R4 enumerate the
   PII / secret-shape leakage paths.
3. **`s3://gardenio-sessions/coop/sessions/<id>/` is the high-blast-radius
   sink.** Per session, coop writes `meta.json`, `transcripts/N.jsonl`,
   `recording/<a>-<b>.jsonl`, `session.jsonl`, `state_events.jsonl`, and
   `hook_events.jsonl`. Recording snapshots run through a regex redactor;
   transcripts and the session log do **not**. Bucket-policy controls
   (block-public, TLS-only, SSE-KMS, versioning, access logging, object lock)
   are **not** asserted by this chart and live in the consumer's TF — the
   audit cannot verify them from code (Findings B1–B6).
4. **Grafana ships an `admin` / inline-string default password** when neither
   `adminPasswordSecret` nor `adminPassword` is set. There is no built-in
   OIDC; the chart relies on the cluster's keycloak ForwardAuth middleware,
   which is *off* unless `keycloak.enabled && keycloak.forwardAuth.enabled`
   are true. Default values gate Grafana behind ingress only — exposure is a
   call-site decision, not a chart guarantee (Finding G1–G3).
5. **Hippo is gone.** No production code path in this repo writes to a
   `hippo/` or `.hippo/` directory. The two `.gitignore` lines are vestigial.
   The only remaining reference in code is a defensive filter in
   `cmd/gb/prime_advice.go` that drops legacy `source:hippo-global` mirror
   beads still living in the beads database. The audit's hippo question is
   moot for this repo.

Net classification: **the most sensitive observability artifact this codebase
produces is the per-session S3 archive.** A second-tier surface is the
controller / slack-bridge stdout stream, which carries Slack user emails
plaintext. A third-tier surface is the agents-side Mezmo MCP query traffic
(read-only, but the queries themselves can carry beads identifiers and search
phrases that an operator might consider sensitive).

---

## 1. Data-flow diagram

```mermaid
flowchart LR
  subgraph PodAgent[Agent pod]
    AGT[agent / claude]
    COOP[coop sidecar]
    GB[gb CLI / prime]
    AGT -- stdout/stderr --> KCONT
    GB -- stdout --> KCONT
    AGT -- "tmux PTY" --> COOP
    COOP -- "screen + state events" --> COOP_REC[record.rs]
    COOP -- "session log + transcripts" --> COOP_TR[transcript.rs]
  end
  COOP_REC --> COOP_S3
  COOP_TR --> COOP_S3
  COOP -- "hook_events.jsonl, state_events.jsonl" --> COOP_S3
  COOP_S3{{S3 sessions subscriber<br/>s3.rs}}
  COOP_S3 -- "PutObject (no SSE-KMS asserted)" --> S3[(s3://gardenio-sessions/<br/>coop/sessions/&lt;id&gt;/)]

  subgraph PodController[Controller pod]
    CTRL[controller bin]
    CTRL -- "JSON slog to stdout" --> KCONT
    CTRL -- "/metrics :9092" --> VMSCRAPE
  end

  subgraph PodSlackBridge[slack-bridge pod]
    SBR[slack-bridge bin]
    SBR -- "JSON slog to stdout" --> KCONT
    SBR -- "/metrics" --> VMSCRAPE
  end

  KCONT[kubelet container stdout sink]
  KCONT -.-> CWLogs[CloudWatch /aws/eks/.../containers<br/>OR LogDNA→Mezmo<br/>OR custom]
  VMSCRAPE[vmagent scrape]
  VMSCRAPE --> VMSINGLE[(vmsingle TSDB)]
  VMSINGLE --> GRAF[Grafana<br/>admin/inline pwd<br/>OR keycloak ForwardAuth]
  GRAF --> GRAFD[(grafana PVC<br/>SQLite session/users)]

  AGT -- "MCP HTTP/JSON" --> MEZMO_API
  MEZMO_API[(Mezmo SaaS API<br/>read/query)]
  AGT -.->|Bearer ${MEZMO_SERVICE_KEY}| MEZMO_API

  classDef sink fill:#fde4d8,stroke:#b45000,color:#000;
  class S3,CWLogs,VMSINGLE,GRAFD,MEZMO_API sink;
```

Edges in this repo's code:

| # | Producer | Sink | Wire format | Code path |
|---|---|---|---|---|
| E1 | `controller` bin (stdout) | container log → cluster log pipe | JSON slog | `gardenio/controller/cmd/controller/main.go:1348` (`slog.NewJSONHandler(os.Stdout)`) |
| E2 | `slack-bridge` bin (stdout) | container log → cluster log pipe | JSON slog | `gardenio/controller/cmd/slack-bridge/main.go:663` |
| E3 | `coop` sidecar (S3 subscriber) | `s3://gardenio-sessions/coop/sessions/<id>/{meta.json,transcripts/N.jsonl,recording/<a>-<b>.jsonl,session.jsonl,state_events.jsonl,hook_events.jsonl}` | mixed JSON / JSONL | `coop/crates/cli/src/s3.rs:371` (`spawn_subscriber`) |
| E4 | `agent` (Claude Code via MCP) | Mezmo SaaS query API | HTTP JSON | `.mcp.json` → `mcp.mezmo.com`, `Authorization: Bearer ${MEZMO_SERVICE_KEY}` (`gardenio/controller/cmd/gb/setup_test.go:1284`) |
| E5 | `controller` `/metrics` | `vmagent` → `vmsingle` | Prometheus exposition | `gardenio/helm/gardenio/templates/controller/deployment.yaml:25` (`prometheus.io/scrape:"true"`) |
| E6 | Grafana | `vmsingle` (read-back) | Prometheus query | `gardenio/helm/gardenio/templates/grafana/configmap.yaml:13-18` |

---

## 2. Inventory — fields shipped to each sink

### 2.1 Controller / slack-bridge stdout (E1, E2)

The controller and slack-bridge use `slog.NewJSONHandler(os.Stdout, ...)`
with no custom replacers, so every key/value pair passed into a log call
appears as a top-level JSON field on the line. Hot fields (sampled):

| Field family | Example call sites | Sensitivity |
|---|---|---|
| `bead_id`, `agent`, `agent_id`, `session_id`, `id` | bridge/squawk.go:83-86, slack.go:164,288,398 | Internal IDs. Low. |
| `email` | `bridge/api_escalations.go:309,390` (`logger.Info("...", "email", email)`); `bridge/bot_mentions.go:1176` (`"email", user.Email`) | **Plaintext Slack user / Keycloak email.** PII (medium-confidentiality at best). |
| `error` | every `logger.Error` site | Often safe; can leak DB rows or Slack API payloads on the way out. |
| `last_id`, `restored last event ID from state` | bridge/sse.go:79 | Beads SSE cursor — internal opaque. Low. |
| `text_length` | bridge/squawk.go:86 | Length only — squawk text body is **not** logged. Good. |
| `addr`, `host_addr`, `image`, `digest` | controller main.go:260,279,387 | Infra; low. |

**Findings:**

- **R1 (Medium) — Slack/Keycloak emails are written to stdout in plaintext.**
  Two distinct call sites in `bridge/api_escalations.go` and one in
  `bot_mentions.go`. There is no slog `ReplaceAttr` redactor, no leveled gate
  ("log emails only at debug"), and no scrubbing in container/log-shipper
  config in this repo. Once the cluster log pipe forwards to CloudWatch /
  Mezmo / Loki, those emails persist for the full retention window of the
  receiving sink. If the receiving sink is shared (multi-tenant log SaaS),
  this becomes a cross-tenant data-residency question.

- **R2 (Low) — `error` strings can include token suffixes from Slack API
  responses.** Bridge code mostly logs `error` directly from upstream HTTP
  call sites; Slack and Keycloak occasionally include the request URL or a
  parameter snippet in their error messages. There is no specific call site
  in this repo that demonstrates a leaked secret; the risk is structural
  ("we trust the error string"). Worth a one-time `gosec`-style sweep, but
  not a hard finding.

### 2.2 Coop S3 subscriber (E3)

Per session, `coop/crates/cli/src/s3.rs:371` (`spawn_subscriber`) writes the
following objects under `s3://<bucket>/<prefix>/<session-id>/` (default
prefix `coop/sessions`):

| Object key | Producer | Format | What it contains | Redaction at write? |
|---|---|---|---|---|
| `meta.json` | `s3.rs:259 upload_meta` | pretty JSON | `SessionMeta`: `session_id`, `agent_type`, `started_at`, `ended_at`, `exit_code`, `exit_status`, `labels[]`, `agent_name`, `project`, `role`, `bead_id`, `transcript_count` (`s3.rs:94-117`). | n/a — structural metadata only. |
| `transcripts/<N>.jsonl` | `transcript.rs:120-153` (snapshot of session log on each compaction) | JSONL | Full Claude session log slice — every user prompt, every assistant turn, every tool input/output, including any text the agent typed at any time. | **No.** |
| `session.jsonl` | `s3.rs:553 upload_session_log` (periodic + post-transcript) | JSONL | The current Claude `session.jsonl` (transcript head before next compaction). Same content shape as transcripts. | **No.** |
| `recording/<first_seq>-<last_seq>.jsonl` | `s3.rs:578 flush_recording_buffer` | JSONL | Batched `RecordingEntry { ts, seq, kind, detail, screen: ScreenSnapshot }` — full screen contents at each state transition / hook event. | **Yes** — `record.rs:62 redact_snapshot` runs `(?i)((?:api[_-]?key|password|token|secret|bearer)\s*[:=]\s*)\S+` across the screen lines and ANSI buffer when `COOP_RECORD_REDACTION` is not set to a falsy value. |
| `state_events.jsonl` | `s3.rs:564 upload_event_logs` | JSONL | Driver state-machine transitions (`AgentState` enum: `Booting`, `Idle`, `Working`, `Stopped`, …). | n/a — no free text. |
| `hook_events.jsonl` | `s3.rs:564 upload_event_logs` | JSONL | Hook payloads: `SessionStart`, `Stop`, `PreToolUse`, `PostToolUse`, including the prime/system-reminder text and tool args. | **No.** Same lack of redaction as transcripts. |

**Trust-boundary observations:**

- **B1 (High) — Transcripts and the session log are uploaded raw.** Anything
  the agent saw or wrote, including Slack messages copy/pasted into a
  spawn prompt, ticket bodies pasted by users, and `~/.aws/credentials` if
  someone ever `cat`'s one — lands in S3 in plaintext. The recording
  redactor (Finding R3 below) does not run on transcripts. This is the
  primary classification anchor for this audit: **transcripts must be
  treated as "may contain secrets and customer data"** and protected
  bucket-side (KMS, IAM, retention).

- **B2 (Medium) — Recording redaction is regex-only and screen-only.** The
  regex (`record.rs:35`, `(?i)((?:api[_-]?key|password|token|secret|bearer)\s*[:=]\s*)\S+`)
  catches `key=value` and `key: value` shapes for a small keyword set.
  It does **not** catch:
  - Slack tokens (`xoxb-…`, `xoxp-…`, `xapp-…`).
  - GitLab tokens (`glpat-…`).
  - GitHub fine-grained tokens (`github_pat_…`).
  - AWS access keys (`AKIA…`) and secret keys (40-char base64).
  - JWTs (any `eyJ…` triple).
  - Bearer values that aren't preceded by a literal `bearer` keyword on the
    same line (e.g. an `Authorization` header rendered by `curl -v` shows
    `Authorization: Bearer …` — caught — but a token printed standalone in
    a tool's output is not).
  - Any of the above when the agent renders them inside an ANSI escape that
    breaks the line.
  Even within the screen, this is a best-effort posture, not a guarantee.
  And the env-var escape hatch `COOP_RECORD_REDACTION=off` disables it
  entirely. **R3** (Medium): broaden the regex to cover the providers
  this repo's `secrets[]` list calls out as in-scope (Slack, GitLab,
  GitHub, AWS, JWT, Bearer-anywhere) and add unit-test coverage for each.

- **B3 (High) — `hook_events.jsonl` carries the prime/system-reminder
  context plaintext.** `gb prime` output (advice beads, role rules,
  project instructions, agent identity) is written into the start-hook
  event by Claude Code's hook protocol, and coop persists those events to
  S3. Some prime advice items are operator-confidential (e.g. "the operator
  password rotates monthly to slack channel X"). There is no redaction
  here at all. Mitigation is upstream: keep secrets out of advice/prime
  content — but the audit should call this out so that operators
  classifying the bucket know to.

- **B4 (Info) — `meta.json` is not sensitive.** Structural metadata,
  agent name + project + role + bead ID. Useful for forensics.

**Bucket-policy controls (cannot be asserted from this repo's code):**

The `gardenio-sessions` bucket itself is provisioned **outside this
repo** — by the consumer's terraform / IaC. The chart only consumes
the bucket name through `agents.s3.bucket` (`values.yaml:565`) and
expects IRSA on the agent ServiceAccount. Five posture items the audit
cannot resolve from this codebase, plus one that it can:

| # | Control | Status from code | Action |
|---|---|---|---|
| B5a | Block Public Access | **[operator to confirm]** | Verify on bucket. |
| B5b | Bucket policy enforces TLS (`aws:SecureTransport=true` deny) | **[operator to confirm]** | Verify on bucket. |
| B5c | Default encryption SSE-KMS (CMK, not aws/s3) | **Not asserted by this code.** `coop/crates/cli/src/s3.rs:191,203` calls `put_object().bucket(...).key(...).body(...)` with **no `server_side_encryption(...)` or `ssekms_key_id(...)`**. Whether objects end up encrypted depends entirely on the bucket's default-encryption configuration, which this repo does not own. | Verify SSE-KMS default; consider explicitly setting `ServerSideEncryption=aws:kms` in `coop`'s `put_object` calls so a misconfigured bucket cannot store plaintext. |
| B5d | Versioning + MFA-delete | **[operator to confirm]** | Verify on bucket. |
| B5e | Server access logging to a separate logging bucket | **[operator to confirm]** | Verify on bucket. |
| B5f | Object Lock (compliance/governance) | **[operator to confirm]** | Verify on bucket. Coop's flow is overwrite-friendly (`session.jsonl` is overwritten on every upload, `meta.json` is rewritten on shutdown), so compliance-mode lock is incompatible without redesign — flag this when Object Lock is being scoped. |

- **B6 (Medium) — IRSA scoping.** `values.yaml:558-565` documents that the
  agent ServiceAccount needs `eks.amazonaws.com/role-arn` annotations.
  This repo does not pin the IAM policy. The minimal policy needed by
  `coop/crates/cli/src/s3.rs` is `s3:PutObject` + `s3:GetObject` +
  `s3:ListBucket` (with `prefix` condition on `coop/sessions/`) +
  `s3:HeadBucket` (the connectivity check at `s3.rs:166`). **[operator to
  confirm]** the IRSA policy is no broader than that and is scoped to this
  bucket only — and that no human IAM user has read access without an
  audit log entry going somewhere reviewable.

### 2.3 Mezmo (E4)

There are **two distinct Mezmo paths**, only one of which is provisioned by
this chart.

| Path | Direction | Credential | Where it lives in this repo | Where it actually runs |
|---|---|---|---|---|
| **MCP read** (`mcp__mezmo__analyze_logs_*`, etc.) | agent pod → Mezmo SaaS | `MEZMO_SERVICE_KEY` (write-only-from-Mezmo's-perspective is wrong — this is a *read/query* key) | `helm/gardenio/templates/mezmo-credentials-external-secret.yaml` provisions the K8s Secret from Secrets Manager via External Secrets. Agent pod reads `${MEZMO_SERVICE_KEY}` and uses it as the `Authorization: Bearer ...` for `https://mcp.mezmo.com/mcp` (per `cmd/gb/setup_test.go:1235`, `:1280`). | Yes — every agent pod with `agents.mezmoServiceKeyExternalSecret.enabled=true`. |
| **Log ingest** (LogDNA agent → Mezmo) | every node → Mezmo SaaS | LogDNA ingest token (separate, NOT in this repo) | n/a — **not provisioned by this chart.** | The acme `tenant-system` chart (out of tree). Greenfield consumers don't get it. |

**Findings:**

- **M1 (Info) — `MEZMO_SERVICE_KEY` is shared across every agent in the
  install.** From `gardenio/docs/research/per-agent-tokens-providers.md:124`:
  "Shared log-ingestion key — write-only, not attributed in any UI."
  (That doc's "write-only" wording is technically wrong; this is a read
  key. The classification — no human-facing identity to corrupt — is still
  correct.) Per-agent attribution is correctly deferred.

- **M2 (Low) — Mezmo MCP query strings are not redacted.** The agent
  passes its query (`level:error app:foo`, sometimes with a literal
  bead ID or a phrase the agent picked from a Slack message) to Mezmo as
  the `query` parameter. Mezmo logs the request on its end. If an agent
  ever pastes a customer email into a query — possible during incident
  triage — that email lands in Mezmo's API logs. Mitigation: agent-side
  guidance ("don't paste PII into log queries") and trust Mezmo's
  retention.

- **M3 (Info) — There is no Mezmo log ingest in this chart.** The
  audit's "what does the controller / bridge ship to Mezmo" question is
  answered by code: **nothing directly.** Container stdout goes wherever
  the cluster's log pipe sends it. In acme-rwx today, that is a
  LogDNA DaemonSet shipping to Mezmo (per the canonical-prod
  observability doc). In greenfield consumers per the canonical-prod
  module plan, that will default to fluent-bit → CloudWatch.

### 2.4 Grafana / VictoriaMetrics (E5, E6)

`gardenio/helm/gardenio/templates/grafana/`:

| File | Purpose | Notable defaults |
|---|---|---|
| `deployment.yaml` | Grafana 11.4.0 + optional `k8s-sidecar:1.28.0` for dashboard ConfigMap discovery (`grafana_dashboard:"1"` label). RootFS read-only, drops all caps. | `runAsNonRoot:true`, `runAsUser:65534`, `seccompProfile.RuntimeDefault`. Strong baseline. |
| `configmap.yaml` | Datasource provisioning: a single `VictoriaMetrics` Prometheus-shaped datasource pointing to in-chart `vmsingle:8428`. Editable: false. | Datasource is `editable: false`. Sidecar dashboard provider runs from `/tmp/dashboards`. |
| `ingressroute.yaml` | Traefik IngressRoute on `websecure`. Optional middlewares: `security-headers`, `stripprefix`, `ipwhitelist`, `forwardauth` (keycloak), `ratelimit`. | **Auth is opt-in.** No middleware = the chart serves Grafana on the public ingress with whatever auth Grafana itself provides. |
| `pvc.yaml` | Persistence (default off). | Grafana's SQLite session/users live here when enabled. |
| `rbac.yaml` | ServiceAccount + Role for the dashboard sidecar to `list/watch` ConfigMaps in this namespace. | Minimal. |
| `service.yaml` | ClusterIP service. | Standard. |

Dashboards on disk (`gardenio/helm/gardenio/dashboards/`):

| Dashboard | Source | Data classification |
|---|---|---|
| `cost-overview.json` | bundled in repo | Aggregate `gardenio_*` metrics (cost USD, fleet stats). No PII; metric labels include project name, agent name, role. |
| `kbeads-embedding.json` | bundled in repo | Embedding completeness counters from kbeads. No PII. |

**Findings:**

- **G1 (High when default-enabled in production) — Grafana admin password
  defaults are weak.** `values.yaml:1196-1202`:
  ```yaml
  adminUser: admin
  adminPassword: ""
  adminPasswordSecret: ""
  ```
  When neither `adminPasswordSecret` nor `adminPassword` is set, the
  Deployment's env block emits **no** `GF_SECURITY_ADMIN_PASSWORD`
  (`deployment.yaml:48-57`), so Grafana boots with its built-in fallback
  password (`admin`). The Helm chart documents this (`adminPassword`
  comment: "For production, use adminPasswordSecret instead.") but does
  not enforce it. Greenfield consumers who flip `grafana.enabled=true`
  with no overrides get `admin/admin` at the ingress. **Recommendation:**
  fail-fast in the chart if `adminPasswordSecret == ""` AND
  `adminPassword == ""` AND `enabled` is true (Helm `required` template
  function), or auto-generate via lookup-or-`randAlphaNum`.

- **G2 (High when ingress is enabled without auth) — There is no Grafana
  OIDC.** The chart does not configure Grafana's `auth.generic_oauth`
  block. Auth is bolted on **outside Grafana** via the Traefik
  ForwardAuth middleware that proxies to keycloak's userinfo endpoint
  (`ingressroute.yaml:34-36`, conditional on
  `keycloak.enabled && keycloak.forwardAuth.enabled`). Two consequences:
  1. If a deployment ships `grafana.enabled=true` and
     `grafana.ingress.enabled=true` but does **not** also enable
     keycloak forwardAuth, the ingress-served Grafana is "behind"
     nothing but Grafana's own admin/admin login. (See G1.)
  2. ForwardAuth establishes *who* is at the ingress, but Grafana
     itself sees them as anonymous (or as `admin` if they log in with
     the password). There is no per-user Grafana role mapping driven
     by Keycloak. Multi-user audit is "Grafana audit log says admin
     did X" regardless of who proxied through.
  **Recommendation:** wire Grafana `auth.generic_oauth` against keycloak
  (or document this gap explicitly so operators don't assume SSO).

- **G3 (Medium) — Datasource has no auth config.** `configmap.yaml:13-17`
  provisions VictoriaMetrics as `access: proxy` with no `basicAuth*`
  fields. Implication: vmsingle is treated as cluster-internal and
  authless. **[operator to confirm]** there is a NetworkPolicy that
  prevents tenant pods in the same cluster from reaching `vmsingle:8428`
  directly. (The chart includes `networkpolicies/` — out of scope for this
  audit.)

- **G4 (Low) — Provisioning logs leak in default mode.** `logFilters: ""`
  means Grafana logs ~840 provisioning loops per hour at info. Not a
  security issue, but downstream log volume / billing concern when log
  pipeline is metered.

### 2.5 Hippo (`hippo/`, `.hippo/`)

- **H1 (Info) — Hippo has been removed from production code.**
  - `hippo/`, `.hippo/` are listed in `.gitignore` (lines 3, 41) and do
    not exist on disk.
  - The only references in the controller code base are:
    - `gardenio/controller/cmd/gb/prime_advice.go:74-76,256-271` — a
      defensive filter that drops legacy `source:hippo-global` mirror
      beads still living in the beads database from before the hippo
      subsystem was removed. This is *clean-up* code, not data flow.
    - `kbeads/docs/design-embedding-search.md` — a single mention in a
      design doc, no live code.
  - The `beads3d/src/main.js:921` line — "Memory satellites — orbiting
    particles showing hippo memory state (ka-0000000124)" — is a
    visualization comment for a feature that was never wired.
  - **No production code path writes to `hippo/`, no production code
    path reads from it, and no code in this repo dehydrates anything
    "hippo" to Postgres.** The audit's hippo question is answered:
    not applicable.
  - **Recommendation:** delete the two `.gitignore` entries and the dead
    `prime_advice.go` filter as separate cleanup tasks. Track at the
    "tech-debt" priority — no security exposure today.

---

## 3. Retention / classification table

The classifications here are this auditor's recommendation given the field
inventories above. Retention numbers are **not** asserted by code — they
are upstream / bucket-side decisions that need operator confirmation.

| Surface | Classification | What it can contain | Default retention (in this repo) | Recommended bucket-side controls |
|---|---|---|---|---|
| Controller / slack-bridge stdout | **Internal-confidential** | Slack/Keycloak emails, bead IDs, agent IDs, infrastructure addresses, rare token suffixes in error strings. | None set in this repo — depends on cluster log shipper sink. | Sink with TLS, access-logged, retention ≤90d unless a regulatory reason demands more. |
| `s3://gardenio-sessions/coop/sessions/<id>/transcripts/*.jsonl` | **Highly confidential — may contain secrets and customer data** | Full Claude session: every prompt, tool input/output, pasted text. | None set in this repo. | SSE-KMS (CMK), Block Public Access, TLS-only via bucket policy, versioning, server-access logging, IRSA-scoped IAM (no human reads without audit), lifecycle expiration ≤90d unless a regulatory reason demands more. |
| `…/coop/sessions/<id>/session.jsonl` | Same as transcripts | Same content as transcripts (the live cut). | Same. | Same. |
| `…/coop/sessions/<id>/recording/*.jsonl` | **Confidential — secret-shape redaction applied** | Screen snapshots at state transitions; regex-redacted for `api_key=`, `password=`, `token=`, `secret=`, `bearer …`. | Same. | Same as transcripts; the redactor is best-effort, not exhaustive (B2). |
| `…/coop/sessions/<id>/hook_events.jsonl` | **Confidential** | `gb prime` content (advice + role rules), tool args, hook payloads. | Same. | Same as transcripts. |
| `…/coop/sessions/<id>/state_events.jsonl` | Internal | Driver state-machine transitions only. | Same. | Same as transcripts (cheaper to ship to coldline storage). |
| `…/coop/sessions/<id>/meta.json` | Internal | Session-level metadata (project, role, bead ID, exit info). | Same. | Same as transcripts. |
| Mezmo MCP query path (E4) | Internal | Read-side queries from agents. | Mezmo SaaS retention applies. | Operator to verify Mezmo data-residency suits org policy. |
| Mezmo ingest path | n/a | **Not provisioned by this chart.** | n/a | Out of scope. |
| VictoriaMetrics (`vmsingle`) | Internal-low | Aggregate `gardenio_*` metrics; labels include project / agent / role. | `retentionPeriod: 90d` (`values.yaml:1126`). | NetworkPolicy isolating `vmsingle:8428`; no public ingress (chart already does not expose it). |
| Grafana PVC | Internal | Grafana SQLite (users, sessions, dashboard versions). | n/a (PVC). | Encrypt the underlying volume; rotate admin password (G1). |
| `hippo/`, `.hippo/` | n/a | **Not in use.** | n/a | n/a — flag for removal (H1). |

---

## 4. Redaction findings

Consolidated from the section-2 sub-findings, ranked by severity.

| ID | Severity | Surface | Finding |
|---|---|---|---|
| **B1** | High | S3 transcripts + session log + hook_events | No redaction. Full agent context — including pasted text, tool I/O, prime content — lands in S3 plaintext. Treat as "may contain secrets and customer data"; rely on bucket-side controls (B5a–B5f). |
| **G1** | High when greenfield-default | Grafana | Admin password defaults to `admin`/empty when neither `adminPasswordSecret` nor `adminPassword` is set. Add a Helm `required` gate or auto-generate. |
| **G2** | High when ingress-without-auth | Grafana | No `auth.generic_oauth`. Keycloak is via Traefik ForwardAuth only and is opt-in. Document explicitly or wire native OIDC. |
| **B3** | High | S3 hook_events.jsonl | `gb prime` content (advice/role rules) is captured plaintext. Mitigate upstream by keeping secrets out of advice — but classify the bucket accordingly. |
| **R1** | Medium | controller/slack-bridge stdout | Slack/Keycloak emails logged plaintext at info level. Add a `slog.HandlerOptions.ReplaceAttr` redactor that masks the local-part of any field whose name is `email`, or move email logging to debug. |
| **B2** | Medium | S3 recording redaction | Regex covers only `api_key`, `password`, `token`, `secret`, `bearer`. Extend to Slack/GitLab/GitHub/AWS/JWT shapes; add unit tests. |
| **G3** | Medium | Grafana datasource | No auth on `vmsingle:8428`; relies on cluster NetworkPolicy. Verify policy. |
| **B5c** | Medium | S3 uploads | `coop` does not set `ServerSideEncryption=aws:kms` on `put_object`. Bucket default encryption is the only line of defense. Set explicitly. |
| **R2** | Low | controller/slack-bridge stdout | `error` strings from upstream APIs may carry token suffixes. Sweep with a `gosec`-like rule for `error` arg leakage. |
| **M2** | Low | Mezmo MCP queries | Free-text queries land in Mezmo API logs. Agent-side guidance only. |
| **G4** | Low | Grafana | Default `logFilters` empty causes ~840/hr provisioning info logs. Set `provisioning.dashboard:warn`. |
| **H1** | Info | Hippo (`hippo/`, `.hippo/`) | Subsystem removed; only vestigial `.gitignore` entries and a defensive bead filter remain. Schedule cleanup. |
| **M1** | Info | Mezmo MCP key | `MEZMO_SERVICE_KEY` is install-shared; per-agent attribution correctly deferred. |
| **M3** | Info | Mezmo log ingest | This chart does not provision an ingest agent. The audit's "what does the controller ship to Mezmo" question is "nothing directly". |
| **B4** | Info | S3 meta.json | Structural metadata only; useful for forensics. |
| **B5a–B5f** | n/a | S3 bucket | Block-public, TLS-only, SSE-KMS, versioning, access logging, object lock all live in the consumer's TF. **[operator to confirm]** for any prod deploy. |
| **B6** | n/a | IRSA | IAM policy not pinned in this repo. **[operator to confirm]** scope to `s3://gardenio-sessions/coop/sessions/*` and `HeadBucket`/`Get`/`Put`/`List` only. |

---

## 5. Recommendations / next-step beads

This audit is research only. Concrete fixes should be filed as separate bug
beads under `project:gardenio`, `security` labels, before any operator commits
to a fix:

1. **Code-fixable** (one bead each):
   - **B5c** — Set `ServerSideEncryption=aws:kms` explicitly in `coop/crates/cli/src/s3.rs` `put_object` calls.
   - **B2** — Extend `record.rs:35` redaction regex to cover Slack/GitLab/GitHub/AWS/JWT shapes; add unit tests.
   - **R1** — Add `slog.HandlerOptions.ReplaceAttr` to mask `email` field local-part in controller/slack-bridge.
   - **G1** — Helm `required` gate on Grafana admin password (or auto-generate with `randAlphaNum`).
   - **G2** — Wire Grafana `auth.generic_oauth` against keycloak, or document the gap.
   - **G4** — Default `grafana.logFilters: "provisioning.dashboard:warn"`.
   - **H1** — Delete `.gitignore` `hippo/` entries and `prime_advice.go` legacy filter.
2. **Operator-confirm** (one bead, but mostly an Observability epic checklist):
   - **B5a–B5f, B6** — `gardenio-sessions` bucket policy + IRSA scoping. Validate against the canonical-prod-aws-account / canonical-prod-stateful-services research docs.
3. **Out of scope here, file separately if pursued:**
   - Add Slack-tenant / customer-data classification to bead lifecycle so downstream consumers (kbeads viewer, beads3d) carry forward the same data-class.

---

## Appendix A — Files inspected

- `gardenio/helm/gardenio/templates/mezmo-credentials-external-secret.yaml`
- `gardenio/helm/gardenio/templates/grafana/{configmap,deployment,ingressroute,pvc,rbac,service}.yaml`
- `gardenio/helm/gardenio/templates/controller/grafana-dashboard-configmap.yaml`
- `gardenio/helm/gardenio/dashboards/{cost-overview,kbeads-embedding}.json`
- `gardenio/helm/gardenio/values.yaml` (agents, grafana, vmsingle, vmagent, agents.s3 blocks)
- `gardenio/controller/cmd/controller/main.go` (slog setup; logger init)
- `gardenio/controller/cmd/slack-bridge/main.go` (slog setup)
- `gardenio/controller/internal/bridge/{squawk,sse,slack,api_escalations,bot_mentions}.go` (logger call sites for emails / IDs)
- `gardenio/controller/cmd/gb/{prime,prime_advice,setup_config,setup_test}.go` (prime size report log emit; legacy hippo filter; MEZMO_SERVICE_KEY MCP wiring)
- `gardenio/controller/cmd/controller/podspec.go` (S3 env wiring; MEZMO_SERVICE_KEY env wiring at ~595 via project-bead `secrets[]`)
- `gardenio/controller/internal/config/config.go` (`CoopS3Bucket`/`Prefix`/`Region`)
- `coop/crates/cli/src/s3.rs` (subscriber, key construction, no SSE-KMS)
- `coop/crates/cli/src/record.rs` (redaction regex + `COOP_RECORD_REDACTION` escape hatch)
- `coop/crates/cli/src/transcript.rs` (no redaction)
- `coop/crates/cli/src/config.rs` (S3 env knobs)
- `docs/research/canonical-prod-observability.md` (prior-art on the
  ingest-vs-MCP split and the canonical-prod default of fluent-bit→CloudWatch)
- `docs/audits/AUDIT-2026-04-27-{controller-go,coop-rust}.md` (overlap with
  controller-side and coop-side findings)
- `gardenio/docs/research/per-agent-tokens-providers.md` (`MEZMO_SERVICE_KEY`
  classification)

## Appendix B — What this audit did NOT do

- Did not run `aws s3api get-bucket-policy --bucket gardenio-sessions` or
  any other live AWS read — no creds, intentional (per role rules).
- Did not query Mezmo to confirm retention or access patterns.
- Did not log into Grafana to verify ForwardAuth at any specific
  cluster.
- Did not enumerate IRSA roles attached to the agent ServiceAccount.
- Did not assess transit-encryption on `vmsingle` ↔ Grafana (in-cluster,
  default plaintext HTTP — accepted by `access: proxy` mode).
- Did not test the redaction regex against live captured data.
