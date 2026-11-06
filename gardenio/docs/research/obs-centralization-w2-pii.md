# W2 — PII-stripping stage for the central observability backplane

**Bead:** [ka-0000001206](https://beads.gardenio.e2e.example.com/beads/ka-0000001206)
**Parent epic:** [ka-0000001783](https://beads.gardenio.e2e.example.com/beads/ka-0000001783) — Centralized observability backplane: metrics + traces in one stack
**Sibling workstreams:**
- W1 topology — [ka-0000001057](https://beads.gardenio.e2e.example.com/beads/ka-0000001057)
- W3 producer-side abstraction — [ka-0000008215](https://beads.gardenio.e2e.example.com/beads/ka-0000008215)
- W4 migration plan — pending W1 draft
**Related prior art:**
- [`security-audit-observability.md`](./security-audit-observability.md) — what we already know leaks
- [`otel-collector-trace-sampling.md`](./otel-collector-trace-sampling.md) — tail sampler design (sits *after* this stage)
- `canonical-prod-keycloak-trace-sampling.md` — layered-pipeline precedent (not in this repo, the canonical-prod research tree was excluded from the 2026-09-16 export)
- [`otel-centralize-acme-api-kit-proposal.md`](./otel-centralize-acme-api-kit-proposal.md) — shared `apikit.observability` proposal

**Status:** Phase-1 design. **No code lands from this doc.** Implementation gated on operator (@matthewbaker) sign-off.

---

## TL;DR

1. **Redaction MUST sit in front of the common collector** (operator hard constraint). This doc defines that boundary as a dedicated **`pii-gateway` OTEL pipeline** that producers point at — not the same collector that fans out to VictoriaTraces / VictoriaMetrics. The "common collector" downstream sees only post-redaction telemetry. Loss-of-control after that point is acceptable; loss-of-control before it is not.
2. **Two-tier defense, one mandatory tier.** The `pii-gateway` is mandatory and load-bearing. Producer-side scrubbing is **opt-in best-effort** for high-confidence classes (known DB column names, PHI-specific log fields) — it shrinks the gateway's attack surface but cannot be relied on for compliance. A producer with no scrubbing must still produce safe telemetry after the gateway.
3. **Six field classes** cover our domain: `phi-direct`, `pii-direct`, `pii-quasi`, `auth-secret`, `infra-secret`, `free-text`. Each has a single canonical action (`drop` | `hash` | `placeholder` | `truncate` | `pass`) and an explicit HIPAA disposition.
4. **Audit without leaking** uses a **commit-only sampler** at the gateway: 1-in-N redaction events are written to a write-only S3 bucket with KMS + Object Lock + bucket-owner-only-read, capturing `{trace_id, span_id, attribute_key, class, action, sha256(value), value_length, encrypted_value}` — and *never* the cleartext. Read requires break-glass IAM + a recorded justification.
5. **Catalog entries are flat YAML** keyed by a `match` selector (attribute key, span name, log body regex, resource attribute). One entry → one class → one action. The catalog itself is out of scope for this doc; the schema and the matcher precedence rules are in §6.

---

## 1. Pipeline placement

### 1.1 Operator constraint, restated

> "we should plan on having some mechanism for stripping PII out of traces as they pass into this common collector or whatever, so we don't have to worry so much about where the data is going."
> — @matthewbaker, MR !879

The literal reading is *the common collector receives only stripped data*. W1's topology design (ka-0000001057) is being authored to accommodate exactly that placement. This doc adopts that constraint as load-bearing: **the gateway is the trust boundary** between "producer territory" (where PHI/PII may legitimately exist in spans, metrics, log records, and resource attributes) and "backplane territory" (where it must not).

### 1.2 Three viable shapes

| Shape | Where the redaction processor runs | Producer change needed |
|---|---|---|
| **A. Central-only** | A dedicated `pii-gateway` OTEL collector deployment in front of the common collector | None (producers keep emitting raw) |
| **B. Sidecar-only** | OTEL collector sidecar / DaemonSet per producer cluster, redacts before WAN egress | Sidecar deployment per cluster |
| **C. Layered (recommended)** | `pii-gateway` (mandatory) **plus** optional producer-side scrubbing in `acme-api-kit` for high-confidence classes | Opt-in `apikit.observability` upgrade |

### 1.3 Trade-offs

**Shape A (central-only) — what we choose as the floor.**

Pros:
- One redaction config, one deploy target, one audit surface, one team-on-call.
- Cleanly enforces the operator constraint: nothing reaches the common collector unscrubbed.
- Producers stay dumb. Adding a new producer is `endpoint=pii-gateway`; no client library change.
- Works for K8s producers (`gardenio` controller, ACME controllers) and ECS producers (`site-api`, `study-web`, `sponsor-api`, `sponsor-connect`, `sysadmin-api`, `discovery-api`, `tsda-api`) identically — the gateway is over the wire, not in the runtime.

Cons:
- **Data-in-flight exposure**: raw PHI/PII traverses producer→gateway. If that hop is on the cluster-local VPC and TLS-terminated only at the gateway, we still have a one-hop unencrypted internal window on some legacy producers. → Mitigated by mTLS or TLS-with-pinning end-to-end (W1's auth pick).
- **Single point of failure for compliance**: a misconfigured gateway leaks everything from everyone. → Mitigated by §5 audit, §7 invariants, and the §8 staged rollout.
- **Gateway must scale to fan-in of all producers.** Sizing is a W1 concern; W2 just assumes it is sized correctly and notes back-pressure semantics in §4.4.

**Shape B (sidecar-only) — rejected as the only line of defense.**

Pros:
- Raw PHI never leaves the producer's pod / task.
- Easier per-tenant policy variance (e.g., `tsda-api` might have stricter classes than `study-web`).

Cons:
- **N deployments × N config drifts.** Same failure mode that produced this epic — the per-tenant ECS observability module fan-out we're trying to escape from.
- **Producers we don't control** (third-party telemetry sources we might add later — VictoriaMetrics scrape targets, vendor SDKs in `site-api`'s deps tree) cannot host a sidecar.
- Sidecars consume per-pod / per-task resources at the fleet scale.
- The audit surface fragments — every cluster needs its own redaction-event sink, or they all ship to the same one (re-creating shape A's audit surface anyway).

**Shape C (layered, recommended) — the actual proposal.**

The `pii-gateway` is shape A and is the only thing compliance leans on. Producers get an *optional* `apikit.observability.redact_known()` pre-step that:

- Strips fields whose class is **statically known at the producer** (e.g., `request.body.ssn`, `db.statement` parameter values for known PHI tables, the `Authorization` header).
- Replaces them in-place with the gateway's placeholder convention (§3) so the gateway sees `pii-direct` placeholders rather than raw cleartext.
- Is a defense-in-depth measure. **The gateway re-applies its rule set to the entire payload regardless** — the producer's scrub is never trusted to have done its job.

This composes well with `otel-centralize-acme-api-kit-proposal.md` (ka-0000006707): the kit owns the producer-side scrub. New services adopt it through the kit upgrade rather than per-service work.

**Why two tiers and not one (shape A only)?**

- Some classes have **type information at the producer that is lost at the gateway**. The producer knows `db.statement` parameter values came from a specific column; the gateway sees only the rendered string. For those, a producer-side rule is far more precise (and cheaper) than a regex at the gateway.
- Producer-side scrub reduces the volume of cleartext on the producer→gateway hop, narrowing the in-flight exposure window noted in shape A cons.
- We never *require* it — that would couple the design to producers we don't fully control. Compliance still depends only on the gateway.

### 1.4 The decision

**Adopt Shape C: layered redaction with the central `pii-gateway` as the load-bearing, mandatory tier.**

- Phase 1 implementation focus: stand up `pii-gateway`. Producers get pointed at it through W3's endpoint abstraction (one variable: `central_observability_endpoint`).
- Phase 2 implementation focus: add `apikit.observability.redact_known()` opt-in scrub.
- Greenfield producers can ship with no scrub on day one and still be compliant.

---

## 2. Field classes for our domain

These six classes are intended to be **exhaustive** for the producers in scope. Anything that doesn't match a class falls under `free-text` (the default-deny class) and goes through the placeholder rule.

The classes are defined on **logical fields**, not transport positions. A given logical class maps onto trace attributes, log records, metric labels, and resource attributes — see §3.4 for the cross-transport mapping.

### 2.1 `phi-direct` — Protected Health Information, identifying

**Definition:** any field that on its own re-identifies a patient/subject, or that pairs the subject identifier with a clinical fact.

**Examples by producer:**

| Producer | Field examples |
|---|---|
| `site-api` | `subject.mrn`, `subject.dob`, `subject.name.first`, `subject.name.last`, `subject.address.*`, `subject.phone`, `subject.email_personal`, `enrollment.diagnosis_code`, `consent.signature_text` |
| `study-web` (browser-emitted spans / RUM) | `query.subject_id` (URL param), `form.field.mrn`, `form.field.dob`, `event.target.value` for inputs in `/subjects/**` routes |
| `sponsor-api` | `request.body.patient_data.*`, `study.subject_identifier`, `adverse_event.narrative` (free-text PHI lives in `free-text` but tagged `phi-suspect` for stricter handling — see §3.3) |
| `sponsor-connect` | mostly mirrors `sponsor-api`; same set under namespace `connect.*` |
| `site-connect` | same as `site-api` under `connect.*` |
| `discovery-api` | `dataset.subject_id`, `dataset.row.*` for any row exported from a clinical table |
| `tsda-api` | `timeseries.subject_id`, `device.serial_number` (PHI under §164.514(b)) when paired with subject |
| `sysadmin-api` | `user.email` for site-staff users — not subject PHI, but PII (see `pii-direct`) |
| ACME internal | `task.patient_context` if ever set; `bead.field.subject_id` if a bead ever holds clinical context (currently rare but possible) |

**HIPAA disposition:** all `phi-direct` fields are §164.514 "identifiers" (one of the 18) or direct clinical facts. They must not exist downstream of the gateway in any form that allows re-identification, including hashed forms where the salt is recoverable. **Action: `drop` by default**, with explicit override to `hash` only for `subject_id`-shape values used for cardinality counts (see §3.1).

### 2.2 `pii-direct` — non-PHI personally identifying

**Definition:** identifies a natural person but is not protected under HIPAA (operator emails, staff names, IP addresses tied to identified users, etc.).

**Examples:**

| Producer | Field examples |
|---|---|
| All HTTP services | `http.request.header.x-forwarded-for`, `client.address`, `client.port`, `user.email`, `user.full_name`, `user.id` when identifiable |
| `gardenio` controller | `slack.user.email`, `slack.user.real_name`, `operator.email`, `agent.operated_by` when it carries a person email |
| `sysadmin-api` | `staff.email`, `staff.phone`, `audit_log.actor.email` |
| ACME internal | `bead.assignee.email`, `mr.author.email`, `gitlab.user.username` (only if username is a person's real-name handle) |

**HIPAA disposition:** not protected by HIPAA in the absence of clinical context, but covered by internal data-handling policy. **Action: `hash` by default** (keeps cardinality for "how many distinct users hit this endpoint" while preventing replay-to-cleartext); `drop` for fields where cardinality is uninteresting (e.g., `http.request.header.authorization` already covered by `auth-secret`).

### 2.3 `pii-quasi` — quasi-identifiers

**Definition:** fields that are not identifying alone but become identifying when combined (zip + DOB + sex is the textbook §164.514(b) example).

**Examples:**

| Producer | Field examples |
|---|---|
| `site-api` | `subject.zip5`, `subject.city`, `subject.state`, `subject.country`, `subject.sex`, `subject.age` (when below 89 it is allowed alone; we treat it as quasi because pairing is the risk) |
| `study-web` | `geo.zip`, `geo.city`, demographic form fields |
| `discovery-api` | `dataset.row.zip`, `dataset.row.city`, `dataset.row.state` |
| `tsda-api` | `device.location.zip` if the device is patient-co-located |

**HIPAA disposition:** the §164.514(b) "Safe Harbor" method requires removal of zip beyond first 3 digits, all dates more granular than year (for ages ≤89), and several others. **Action: `truncate`** with a per-field rule:
- `zip5` → `zip3` (drop last two digits unless 3-digit population <20,000, in which case → "00000")
- `subject.dob` → `subject.birth_year`
- `subject.age` → bucket `<18`, `18-64`, `65-89`, `90+`
- `geo.city` / `geo.state` → `drop` outright in span / log attributes; keep at resource-attribute level only if aggregated (see §3.4).

### 2.4 `auth-secret` — credentials and authn material

**Definition:** anything that, if leaked, lets the holder impersonate a user or service.

**Examples (universal across producers):**

- HTTP headers: `Authorization`, `Cookie`, `Set-Cookie`, `X-API-Key`, `X-CSRF-Token`, `Proxy-Authorization`
- URL components: `password=`, `api_key=`, `token=`, `code=` (OAuth code grants), `access_token=`, `refresh_token=`, `assertion=`, `client_secret=`
- Request/response body fields: any key matching `(?i)(password|secret|token|api[_-]?key|client[_-]?secret|signing[_-]?key|access[_-]?token|refresh[_-]?token|bearer|jwt|saml|assertion)` at any depth
- SQL parameter values where `db.statement` contains `crypt`, `password`, etc.
- TLS / mTLS material: `tls.client.certificate.*` chains (drop bodies, keep `tls.client.certificate.fingerprint.sha256` only)

**HIPAA disposition:** not directly PHI/PII, but a leak is a security incident and a likely PHI breach precursor. **Action: `drop` unconditionally.** Never hash (hash of a token is still useful to an attacker who can replay).

### 2.5 `infra-secret` — non-credential infrastructure secrets

**Definition:** internal cloud and account identifiers that we don't want public, distinct from `auth-secret` (which is a key) and `pii-direct` (which is a person).

**Examples:**

- `aws.account.id` — present per ECS task already (kit-injected). Operator preference: keep or drop in central backplane? Default: **`pass`** (we own those accounts, the data is internal). Override to `hash` if the central backplane is multi-tenant in future.
- `aws.ecs.task.arn` — pass; useful for ops.
- `cloud.account.id` — same as `aws.account.id`.
- `db.connection_string` — `drop` (contains password).
- `messaging.destination.name` when it encodes tenant — `pass` but classify as tenant identifier (`tenant-id` class added if needed; for now this is `pass`).
- `kubernetes.pod.uid` — pass; not sensitive but high-cardinality; metric-label inclusion is a sampling concern, not a redaction concern.

**HIPAA disposition:** outside HIPAA scope; included in the rule set to centralize the "what do we strip when shipping to a non-Pi backplane" decision. **Action: `pass`** by default; per-field override available.

### 2.6 `free-text` — bodies, narratives, unstructured strings

**Definition:** any attribute that may contain a string the producer didn't categorize. Default class for unknown attribute keys.

**Examples:**

- `http.request.body` (any service) — variable structure
- `log.body` — slog / structlog free-form messages
- `db.statement` — parameterized SQL, but parameter values themselves can be PHI
- `exception.message`, `exception.stacktrace` — exception bodies frequently include the offending value (e.g., `IntegrityError: duplicate key value violates unique constraint "subjects_mrn_uniq": Key (mrn)=(123-45-6789) already exists`)
- `messaging.message.body` — NATS / SNS payloads
- Browser-side `event.target.value`, `dom.snapshot`, screenshot data URIs

**HIPAA disposition:** the most dangerous category, because we cannot inspect it cheaply. **Action: `placeholder`** — replace the entire value with a fixed string `"<redacted:free-text>"` by default. Subclasses (`free-text` + `phi-suspect`) get the same treatment with a marker for audit. Override to `truncate` (first N chars after regex-scrub of known PHI shapes) only for fields the operator has explicitly whitelisted (e.g., `exception.type` is safe; `exception.message` is not).

### 2.7 Class summary

| Class | HIPAA-protected? | Default action | Override actions allowed |
|---|---|---|---|
| `phi-direct` | Yes | `drop` | `hash` (subject-id only, for cardinality) |
| `pii-direct` | No (internal policy) | `hash` | `drop`, `placeholder` |
| `pii-quasi` | Yes (per §164.514(b)) | `truncate` | `drop` |
| `auth-secret` | No (security) | `drop` | (none — drop is mandatory) |
| `infra-secret` | No | `pass` | `hash`, `drop` |
| `free-text` | Unknown / mixed | `placeholder` | `drop`, `truncate` (whitelisted fields only) |

---

## 3. Redaction rules

Four primitive actions. The gateway's OTEL processor pipeline implements exactly these — no class-specific code paths.

### 3.1 `hash`

`HMAC-SHA256(key=$REDACTION_KEY, msg=class || ":" || value)`, base64url, first 16 bytes (96 bits) printed.

- Key prefixed with class so the same string mapped under two classes never collides.
- `$REDACTION_KEY` rotated quarterly; rotation breaks downstream joinability across the rotation boundary, which is **intentional** — we do not want indefinite cardinality preservation.
- Key stored in AWS Secrets Manager, read by the gateway pod identity only; never exported.
- Output prefix: `"h_"` (e.g., `"h_3kQ2..."`) so audit consumers can tell at a glance it was hashed, not a raw value that happens to look hex.

**Used for:** `pii-direct` (default), `phi-direct` overrides where cardinality is needed (e.g., "distinct subjects seen by `study-web` per hour").

### 3.2 `drop`

Remove the attribute / log field entirely. The downstream collector sees no key, no value, no marker. Span / log record otherwise passes through.

**Used for:** `phi-direct` (default), `auth-secret` (mandatory).

### 3.3 `placeholder`

Replace value with a fixed string. Conventions:

- `"<redacted:phi>"`
- `"<redacted:pii>"`
- `"<redacted:auth>"`
- `"<redacted:free-text>"`
- `"<redacted:free-text:phi-suspect>"` for fields explicitly tagged as likely-to-contain PHI but already in `free-text` class

The attribute key remains; the value is replaced. The fixed string is short, distinct, and easy to grep for downstream when validating "did anything slip through?".

**Used for:** `free-text` (default), `pii-direct` overrides where we want to surface "yes a person was here" without keeping cardinality.

### 3.4 `truncate`

Class-specific truncation rules. Implemented as a small lookup table the gateway processor reads at boot:

```yaml
truncations:
  subject.zip5: { strategy: zip3, low_population_replacement: "00000" }
  geo.zip:      { strategy: zip3, low_population_replacement: "00000" }
  subject.dob:  { strategy: year_only }
  subject.age:  { strategy: age_bucket, buckets: ["<18","18-64","65-89","90+"] }
  geo.city:     { strategy: drop }
  geo.state:    { strategy: drop }
  free-text-whitelist:
    exception.type: { strategy: pass }
    db.system:      { strategy: pass }
```

**Used for:** `pii-quasi` (default), whitelisted `free-text` fields.

### 3.5 `pass`

No-op. Attribute traverses unchanged. Exists as a first-class action so the catalog can be exhaustively explicit — anything not classified is `free-text` placeholder, not pass.

**Used for:** `infra-secret` (default), explicitly-whitelisted free-text fields.

### 3.6 Cross-transport mapping

Each class applies identically across all four OTEL signal carriers:

| Carrier | Redaction surface |
|---|---|
| **Trace span attributes** | `span.attributes[key]`, `span.events[*].attributes[key]`, `span.links[*].attributes[key]`, `span.status.message` (treated as `free-text`) |
| **Trace span resource attributes** | `resource.attributes[key]` — applied once per export batch; same rules |
| **Log records** | `log.body` (free-text), `log.attributes[key]`, `log.resource.attributes[key]` |
| **Metric data points** | `datapoint.attributes[key]` — these become Prometheus labels and are the worst place to leak PHI because they explode cardinality |
| **Span name** | Patterns matching `*/<MRN>/*` rewritten via path-template extraction; raw span names with PHI shapes get the placeholder action on the name itself |

The OTEL collector's `transform` processor with the `OTTL` language is expressive enough to apply every action on every carrier — no per-carrier processor specialization needed.

### 3.7 What the gateway does NOT touch

Out of scope explicitly:
- **Log body parsing into structured fields.** If a service emits a JSON log line as `log.body = '{"mrn": "..."}'`, the gateway treats the whole body as free-text and placeholders it. Producers wanting field-level retention must emit log attributes, not stringified JSON bodies. This is a deliberate cliff that incentivizes structured emission.
- **Trace context propagation (`traceparent`, `tracestate`).** These have no PHI.
- **Span timing.** No redaction on `start_time_unix_nano` / `end_time_unix_nano`.
- **Metric values themselves.** Only labels. A counter named `subjects_enrolled_total` with no label PHI is fine.

---

## 4. The gateway

### 4.1 Shape

A dedicated OTEL collector deployment (initial target: devops EKS cluster, per W1), running the OTEL Collector contrib distribution with this pipeline:

```text
receivers:
  otlp:                       # HTTP + gRPC, mTLS termination
    protocols:
      grpc:
        endpoint: 0.0.0.0:4317
        tls: { ... }          # mTLS, client cert pinned via W1 auth design
      http:
        endpoint: 0.0.0.0:4318
        tls: { ... }

processors:
  memory_limiter: { ... }     # backpressure floor
  attributes/redact:          # the load-bearing processor
    actions:                  # generated from catalog.yaml (see §6)
      # ... rules ...
  transform/redact:           # OTTL for free-text + log body
    log_statements: [ ... ]
    trace_statements: [ ... ]
    metric_statements: [ ... ]
  audit/sample:               # see §5 — branches a copy to the audit pipeline
  batch:

exporters:
  otlp/common-collector:       # forwards to the W1 common collector
    endpoint: common-collector.observability.svc:4317
    tls: { ... }
  otlp/audit-sink:             # write-only S3 (via Firehose) — see §5
    endpoint: audit-firehose:4318

service:
  pipelines:
    traces:
      receivers:  [otlp]
      processors: [memory_limiter, attributes/redact, transform/redact, audit/sample, batch]
      exporters:  [otlp/common-collector]
    logs:    { ... same ... }
    metrics: { ... same ... }
    traces/audit:
      receivers:  [audit/sample]
      exporters:  [otlp/audit-sink]
```

### 4.2 Why the contrib OTEL collector and not custom code

- The `attributes` and `transform` processors are mature, performant, and operator-debuggable.
- OTTL is expressive enough for all six classes' actions, including conditional replacement and regex-driven matching.
- Replacing it with a custom redaction service costs us the ecosystem and gains us nothing — the rule expressiveness need is well within what's already shipped.
- We add **one** custom processor (`audit/sample`) for the redaction-event branch (§5). Everything else is stock.

### 4.3 Identity and trust

The gateway is the only client of the common collector that is allowed to write through the production hot path. Producer clients (site-api, study-web, sponsor-api, ACME internals, gardenio) write **only** to `pii-gateway.observability.<env>.<domain>`, never to the common collector directly.

Enforcement is on the network and IAM layer (W1's auth pick determines exactly how):
- If mTLS: only the gateway's client cert is trusted by the common collector.
- If SigV4 / IAM: only the gateway's IRSA / task role can call the common collector's OTLP endpoint.
- If bearer + secret manager: only the gateway has the secret.

Producers do not get common-collector credentials. This is what makes the constraint "redaction is in front of the common collector" enforceable rather than aspirational.

### 4.4 Back-pressure and failure modes

- **Gateway healthy, common collector down**: gateway buffers per OTLP exporter retry config, then drops. Producers see ack from gateway either way. Acceptable.
- **Gateway down**: producers fail OTLP export. **They must not fall back to direct common-collector export.** This is enforced because they don't have credentials to it (§4.3). They drop telemetry locally per their OTEL SDK's drop semantics. We accept telemetry loss over leakage.
- **Gateway up but redaction processor crashes**: the OTEL collector pipeline halts. Same outcome as gateway-down. We do NOT have a `passthrough on redaction failure` mode. **Fail-closed is mandatory for compliance.**

### 4.5 Configuration management

- `catalog.yaml` lives in this repo (or sibling, TBD) and is rendered into the OTEL collector config at chart-render time via Helm template.
- Catalog changes go through MR review with a `@security` reviewer-group requirement (configured in `.gitlab/CODEOWNERS`).
- Catalog rollout is a chart deploy; the collector reloads cleanly (it supports config reload signals).
- An **emergency disable** of a specific rule is a single catalog edit + chart deploy; we do not need a feature-flag system on top.

---

## 5. Audit story — "what did we strip?" without leaking

### 5.1 Requirement

The team needs to:

1. **Validate** the redaction rules are doing what we think (sample inspect).
2. **Detect new classes** of PII showing up in unexpected fields (e.g., a producer adding a new attribute key that lands in `free-text` placeholder when it should be `phi-direct` drop).
3. **Investigate** compliance incidents: did $value leak before $time?

Doing this naively defeats the whole exercise — a "what was stripped" audit log that contains the stripped values is just a second copy of the unscrubbed pipeline.

### 5.2 Design: commit-only sampler with field-level encryption

The `audit/sample` processor branches a sampled stream off after the redaction processors. For each redaction event:

```json
{
  "ts":                "2026-05-19T15:23:11.482Z",
  "trace_id":          "4bf92f3577b34da6a3ce929d0e0e4736",
  "span_id":           "00f067aa0ba902b7",
  "service_name":      "site-api",
  "resource.env":      "prod",
  "attribute_key":     "request.body.subject.mrn",
  "match_rule_id":     "phi-direct.subject.mrn",
  "class":             "phi-direct",
  "action":            "drop",
  "value_length":      11,
  "value_sha256":      "h_a1b2c3...",
  "value_encrypted":   "kms:envelope:<base64>"   // OPTIONAL, opt-in per class
}
```

Each redaction event carries:

- **What** (`attribute_key`, `match_rule_id`, `class`, `action`)
- **Where** (`trace_id`, `span_id`, `service_name`, resource env)
- **Length + HMAC fingerprint** (`value_length`, `value_sha256` — same HMAC as §3.1 with the audit-specific key so it doesn't replay against the production hash)
- **Encrypted value** (envelope-encrypted with a KMS key only the security team can decrypt, used **only for whitelisted classes** that need spot-check inspection — e.g., `free-text` placeholders we suspect may have been mis-classified)

### 5.3 Why this is safe

- **HMAC fingerprint** is non-reversible. Auditors can answer "did the same value appear twice?" or "is this the value we expect?" by feeding a candidate plaintext through the same HMAC and comparing. They cannot extract the plaintext from the fingerprint alone.
- **Encrypted value** is only present for classes where we explicitly need spot-inspection (e.g., `free-text` to detect new PHI patterns). Decryption requires:
  - IAM permission on the audit-KMS-key (granted to security IAM group only).
  - A logged justification in the IAM session (CloudTrail captures the `kms:Decrypt` event with the assumed-role session name we require to encode the ticket ID).
  - Quarterly access review.
- **`phi-direct` and `auth-secret` never get encrypted-value field populated.** Even break-glass-with-justification access cannot recover those because we never stored them in the audit stream.
- **`pii-direct` encrypted-value is opt-in**, defaults off.

### 5.4 Storage

- Audit-sink Firehose → S3 bucket `s3://obs-pii-audit-<env>/`.
- Bucket policy:
  - `BlockPublicAcls=true`, `RestrictPublicBuckets=true`, `IgnorePublicAcls=true`.
  - SSE-KMS with a dedicated `obs-pii-audit-cmk` (separate from production data CMK).
  - Object Lock in compliance mode, retention 90d minimum.
  - Versioning on.
  - Access logging to a separate `s3://obs-pii-audit-access-log-<env>/` bucket.
  - Bucket policy denies `s3:GetObject*` to all principals except a dedicated `obs-pii-audit-reader` role; that role has zero inline access and is only assumable via the break-glass IAM flow.
- Lifecycle: transition to Glacier Deep Archive at 30d, expire at 365d. Object Lock retention overrides expiration as needed for active investigations.

### 5.5 Sampling

- Default sample rate: **1% of redaction events** per `(service_name, class)` tuple, with a floor of "every distinct `attribute_key` first seen in a 24h window is captured at 100%". The floor is what lets §5.1.2 (detecting new fields) work.
- Sampling key: hash of `(trace_id, attribute_key)` so the same span's same field always either is or isn't sampled — supports reconstructing "what got stripped for this trace" deterministically.
- Sample rate adjustable per class. `auth-secret` defaults to **0%** sampling — we never store information about secret values, even encrypted, because the cost of the audit pipeline becoming a secondary leak path exceeds the value of auditing it.

### 5.6 What auditors can ask, and how

| Question | How it's answered |
|---|---|
| "Did `site-api` ever ship `subject.mrn` between $t1 and $t2?" | Query audit S3 for `attribute_key=subject.mrn`, `service_name=site-api`, in time window. Count of matching records = volume of attempts. Cleartext is never inspected; HMAC fingerprint can confirm "was THIS specific known-leaked mrn ever shipped?" by HMAC'ing the candidate and comparing. |
| "Are there any attribute keys that hit `free-text` placeholder more than 100k times/day from `study-web`?" | Aggregate query on audit S3 with no decrypt step. New high-volume keys are class-mismatch candidates — file follow-up to classify them. |
| "What was actually in `exception.message` from `sponsor-api` at $time? We suspect a misconfig leaked a row." | Break-glass: file ticket, assume `obs-pii-audit-reader` role with ticket ID in session name, decrypt the specific event's `value_encrypted` field. Recorded in CloudTrail. Decryption only works because `exception.message` is on the encryption whitelist; would not work for `subject.mrn`. |
| "Show me redactions that fired in trace $traceid" | Audit S3 query by `trace_id`, returns the list of attribute keys + classes + actions. Useful for "is this customer's trace stripping more than expected?" investigations without seeing the values. |

### 5.7 Where this audit story does NOT help

- **Cleartext that bypassed the gateway entirely** (e.g., a producer that ships logs to Mezmo directly via the DaemonSet path that this design does not touch — see [`security-audit-observability.md`](./security-audit-observability.md) §B for the existing PII surface there). The W4 migration plan needs to address that surface separately. We do not extend the W2 audit to cover non-OTEL pipelines.
- **Cleartext present *in producer memory* before OTEL emission.** Out of scope; the producer's own logging-handling discipline covers that.

---

## 6. Catalog entry schema

The catalog is a YAML document loaded by the gateway. Below is the schema each entry conforms to. **Drafting the catalog contents is explicitly out of scope** (parent epic ka-0000001783 calls this out); this section defines only the shape.

### 6.1 Entry shape

```yaml
# obs-pii-catalog.yaml
version: 1                            # schema version; bumped on incompatible changes
default_class: free-text              # action for unmatched attributes (see §2.6)
default_action: placeholder           # action when class matches but no override given

rules:
  - id: phi-direct.subject.mrn         # globally unique, kebab-cased, dotted by class
    description: "Subject MRN — HIPAA §164.514 identifier"
    class: phi-direct
    action: drop                       # one of: drop|hash|placeholder|truncate|pass
    match:
      # exactly one of: attribute_key, span_name, log_body_regex, resource_attribute
      attribute_key: "subject.mrn"
    applies_to:
      services: ["site-api", "site-connect", "discovery-api"]   # or "*" for all
      signals:  ["traces", "logs"]                              # subset of traces|logs|metrics
    audit:
      sample_rate: 0.01                # override default per-class rate
      encrypt_value: false             # never encrypt phi-direct in audit (§5.3)
    references:
      - "ka-0000001783"                # parent epic
      - "HIPAA §164.514"

  - id: pii-direct.user.email
    description: "Operator / staff email"
    class: pii-direct
    action: hash
    match:
      attribute_key: "user.email"
    applies_to:
      services: "*"
      signals: ["traces", "logs", "metrics"]
    audit:
      sample_rate: 0.01
      encrypt_value: false

  - id: pii-quasi.subject.zip
    description: "Subject ZIP — truncate to first 3 digits per Safe Harbor"
    class: pii-quasi
    action: truncate
    match:
      attribute_key_regex: "^(subject|patient)\\.zip5?$"
    truncate:
      strategy: zip3
      low_population_replacement: "00000"
    applies_to:
      services: ["site-api", "sponsor-api", "discovery-api", "tsda-api"]
      signals: ["traces", "logs"]

  - id: auth-secret.authorization-header
    description: "Authorization HTTP header"
    class: auth-secret
    action: drop
    match:
      attribute_key_regex: "^http\\.request\\.header\\.authorization$"
    applies_to:
      services: "*"
      signals: ["traces", "logs"]
    audit:
      sample_rate: 0.0                 # never sample (§5.5)
      encrypt_value: false

  - id: free-text.log-body-default
    description: "Default log body redaction"
    class: free-text
    action: placeholder
    match:
      log_body: true                   # special selector for log record body
    applies_to:
      services: "*"
      signals: ["logs"]
    audit:
      sample_rate: 0.001               # body sampling is expensive
      encrypt_value: true              # whitelisted for spot-inspection
```

### 6.2 Field reference

| Field | Type | Required | Notes |
|---|---|---|---|
| `id` | string | yes | Globally unique. Kebab-cased. Dotted by class for sortability. Match-rule deletion requires changing the id (no silent semantic shifts). |
| `description` | string | yes | Free-text. Cited in audit records. |
| `class` | enum | yes | One of the six classes from §2. |
| `action` | enum | yes | `drop` \| `hash` \| `placeholder` \| `truncate` \| `pass`. Must be allowed for the class (§2.7 override matrix). |
| `match` | object | yes | Exactly one selector: `attribute_key` (exact), `attribute_key_regex`, `span_name`, `span_name_regex`, `log_body` (boolean), `log_body_regex`, `resource_attribute` (exact), `resource_attribute_regex`. |
| `truncate` | object | only if `action: truncate` | `strategy` enum (`zip3`, `year_only`, `age_bucket`, `prefix_n`, `drop`), plus strategy-specific params. |
| `applies_to.services` | list[string] \| "*" | yes | OTEL `service.name` resource attribute values, or `"*"`. |
| `applies_to.signals` | list[enum] | yes | Subset of `traces`, `logs`, `metrics`. |
| `audit.sample_rate` | float (0-1) | no | Overrides class default. |
| `audit.encrypt_value` | bool | no | Defaults from class (§5.5). |
| `references` | list[string] | no | Bead IDs, HIPAA sections, or doc paths. |

### 6.3 Matcher precedence

When multiple rules could match the same attribute:

1. Exact `attribute_key` wins over `attribute_key_regex`.
2. Within regexes, longest-pattern wins (deterministic tiebreak: alpha order on `id`).
3. A rule with `applies_to.services: ["site-api"]` wins over `services: "*"` for `site-api` traffic.
4. A rule whose `signals` list more narrowly matches the current signal wins over a `signals: ["traces","logs","metrics"]` blanket rule.
5. If two rules still tie, the gateway **fails closed** at boot — refuses to start until the catalog is disambiguated. We do not pick winners by chance.

### 6.4 Validation

A pre-commit / CI validator (`cmd/gb/obs-catalog-validate` or equivalent — implementation in a follow-up; not this MR) checks:

- Every `id` is unique.
- Every `class` is one of six.
- Every `action` is allowed for the class.
- `match` has exactly one selector populated.
- Regexes compile.
- `applies_to.services` values exist in a known-service list (kit-maintained).
- No two rules tie under §6.3 precedence (the boot-time fail-closed has a static equivalent here).

### 6.5 What the catalog does NOT encode

- **Order of evaluation**: §6.3 precedence is deterministic; the catalog does not contain priority numbers. We resist the urge to add them.
- **Negative matches** ("if value matches X, do nothing"): the matcher is positive-only. Whitelisting happens at the level of "this attribute_key is class `infra-secret` with action `pass`", not as an exception inside a broader rule.
- **Per-tenant overrides**: not in v1. If a tenant needs stricter handling, file a class promotion request — the rule changes for everyone or no one. This keeps the gateway config single-tenant in shape regardless of how many producers feed it.

---

## 7. Invariants the design must preserve

The following are tested (W4 will own the test design):

1. **Common-collector ingress is gateway-only.** No producer credential reaches the common collector. (§4.3)
2. **Fail-closed.** Gateway redaction failure stops telemetry; does not pass through. (§4.4)
3. **Default-deny classification.** Unmatched attributes fall to `free-text` placeholder, never to `pass`. (§2.6, §6.1)
4. **No `pass` for `phi-direct` or `auth-secret`.** Catalog validator rejects (§6.2 override matrix).
5. **Audit stream does not store `phi-direct` or `auth-secret` cleartext, ever, even encrypted.** (§5.3)
6. **HMAC key separation.** Production HMAC key (§3.1) and audit HMAC key (§5.2) are distinct and rotated independently.
7. **Rule changes require security review.** Enforced by `.gitlab/CODEOWNERS` on the catalog path. (§4.5)
8. **Catalog precedence is deterministic.** Boot-time disambiguation prevents silent rule promotion. (§6.3)

---

## 8. Open questions for operator

These are decisions the operator needs to make before Phase-2 implementation begins. None block landing this design doc; they block landing the catalog and the gateway chart.

1. **HMAC key custody**: AWS Secrets Manager (current default for ACME secrets) vs. Vault vs. a dedicated KMS-only CMK with the gateway calling `GenerateMac` instead of holding the key. Default proposal: Secrets Manager + IRSA. Alternatives have trade-offs in rotation cost and audit visibility — flag for operator pick.
2. **Audit retention**: 90d minimum is proposed (§5.4). Operator/compliance may need 7-year retention for some PHI-suspect classes. If so, lifecycle policy stays in Glacier Deep Archive past 1y; storage cost is negligible.
3. **`infra-secret` default**: `pass` is proposed because the central backplane is Pi-internal today. If we ever multi-tenant the central backplane (a partner consuming our telemetry, however unlikely), `infra-secret` default flips to `hash`. Flag for explicit operator confirmation that we accept `pass` for now.
4. **Browser-side telemetry** (study-web RUM, click spans): the current design treats browser-emitted spans the same as server spans (gateway redacts everything regardless of origin). Operator should confirm this is the desired posture, vs. a stricter "browser spans land in a separate, harder-to-decrypt audit stream" mode.
5. **Class promotion process**: when a producer adds a new attribute, the catalog needs a new entry, but until that entry lands the attribute hits `free-text` placeholder. Acceptable lag? Or do we want a "block the rollout of new attribute keys until classified" CI check? Defer answer; mention it.

---

## 9. What this doc does not cover

- **The W1 collector topology itself** (deployment placement, sizing, TSDB/trace-store choice, auth pick). The gateway lives in front of whatever W1 decides; this design composes with all W1 options.
- **The W3 producer endpoint abstraction.** Producers point at `central_observability_endpoint` (the W3 variable name); whether that resolves to gateway or directly to common collector is *not* up to producers. The W3 implementation will resolve to the gateway.
- **Drafting the catalog contents.** Parent epic explicitly calls this out as a follow-up task. This doc defines schema and operating principles, not specific rules beyond illustrative examples.
- **Migration of existing producers.** That's W4. Producers can keep emitting raw to the gateway from day one; the gateway redacts. There is no "before" state where producers had to scrub themselves first.
- **Logs centralization.** Per the parent epic, logs are explicitly out of scope of this backplane work; the existing devops VictoriaLogs path stays. If we add OTEL log support to the backplane later, this design extends to it (§3.6 already covers log records — no rework needed).

---

## 10. Acceptance criteria for this design

This W2 design lands when:

1. ✅ Doc merged at `gardenio/docs/research/obs-centralization-w2-pii.md`.
2. ✅ Operator (@matthewbaker) acknowledges via comment, gb decision, or MR review approval. Phase-2 implementation does **not** start before that ack.
3. ✅ W1 topology design accepts the front-of-common-collector placement (already a stated W1 hard constraint).
4. ✅ Synthesis with W1 + W3 + W4 (parent epic completion) flags no contradictions.

---

Operator: @matthewbaker
