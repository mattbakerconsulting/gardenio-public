# gardenio-next Config Mirror — Mechanism Decision

Decision record for Epic 4 task [`ka-0000006436`](#). Picks the transport that
moves config from gardenio to gardenio-next on an ongoing basis.

Companion to [`gardenio-next-mirror-deny-list.md`](./gardenio-next-mirror-deny-list.md)
(the *what* to mirror) and advice [`ka-0000007027`](#) (the durable cross-session
reminder).

---

## 1. Problem

Epic 4 (`ka-0000004441`) needs a mechanism that keeps gardenio-next's config a
faithful, filtered copy of gardenio's config, with these acceptance criteria:

- `kd list --type=config` in gardenio-next returns the same custom types, prefixes,
  role contexts, and decision templates as gardenio.
- The mirror runs without manual intervention after cutover.
- A change to gardenio config propagates within the chosen cadence.
- Per-env keys (deny-list §2) are filtered; per-type partial-mirror rules (§3) are
  applied; failures are fail-closed with a structured warning.

Scope is **config only** (deny-list §1 recap). Live issues, comments, audit events,
and gate state are out.

---

## 2. Options considered

| Option | Transport | How filter happens | Pros | Cons |
|---|---|---|---|---|
| **A. K8s CronJob + `gb config dump \| filter \| gb config load`** | HTTP API (kbeads `/v1/configs*` + SSE optional) | In a stand-alone filter binary between `dump` and `load` | Uses existing `gb config dump`/`gb config load` (implemented, tested, used for backups today). Allow-list / deny-list / field-stripper live in one Go binary that is trivially auditable. Idempotent; re-runnable; fails closed. Pod-level isolation; no cross-env Postgres or NATS surface. Works across separate clusters or same-cluster/different-namespace. | Polling-based, so propagation lag ≤ cadence (hourly by default). Requires one K8s Secret carrying a gardenio-next admin token for the CronJob pod. |
| **B. Postgres logical replication** | Postgres publication + subscription from gardenio's RDS to gardenio-next's Postgres | Impractical at the row level: kbeads stores labels as JSON and config beads are interleaved with all other beads in the same tables. `WHERE` clause filtering on publications doesn't understand JSON label shapes. | Low-latency when it works. | Huge blast radius — a mis-scoped publication silently copies **live issues, audit events, comments, gate state** into gardenio-next, which violates the Epic 4 scope. Per-env fields can't be stripped at the row level (the destructive deny-list patterns in §2.1–2.8 of the deny-list doc live inside JSON payloads). Requires network reachability to RDS from gardenio-next's Postgres and admin privileges on both sides. Operational complexity (slot management, replay state, schema-migration coordination) greatly exceeds the value. |
| **C. One-shot seed (Helm post-install Job only)** | `gb config dump \| filter \| gb config load` at chart install/upgrade | Same filter binary as A | Simplest. | Fails Epic 4 acceptance (no ongoing propagation). Drift sets in immediately; every new custom type or decision template landing in gardenio requires a chart redeploy of gardenio-next. Not acceptable as the sole mechanism. |
| **D. NATS-driven live sync** | kbeads already publishes config mutations on the NATS JetStream `config` subject; gardenio-next subscribes and applies filtered events. | In the subscriber | Real-time; minimal drift window. | Requires cross-env JetStream reachability (currently each env's NATS is namespace-local). Subscriber has to survive reconnect / replay correctly, handle out-of-order events, and still round-trip through the same filter. Higher operational complexity than A for a propagation-lag win that does not matter for config (which changes on the order of days, not minutes). |

---

## 3. Recommendation — Option A (CronJob, hourly) + bootstrap seed Job

**Decision**: `K8s CronJob` running **hourly** that runs the same binary the Helm
chart's post-install `Job` runs on first deploy. Both paths call:

```text
gardenio-beads /v1/configs*  →  gb-mirror filter  →  gardenio-next-beads /v1/configs*
```

Concretely:

1. **Bootstrap Job** (Helm `post-install,post-upgrade` hook on gardenio-next).
   Runs `gb-mirror sync --source=<gardenio-beads-url> --dest=<gardenio-next-beads-url>
   --deny-list=<path-or-embedded> --dry-run=false --full=true`. Populates gardenio-next
   from empty. Idempotent.

2. **Hourly CronJob** (`schedule: "23 * * * *"` — off-the-hour to avoid thundering
   herds with unrelated hourly jobs; offset is arbitrary). Same binary, same flags,
   `--full=false` (incremental by modified-since). Runs in the **gardenio-next**
   namespace so that the admin credential it holds is scoped to gardenio-next only.

3. **Credentials**:
   - On gardenio: a `Viewer`-role API token (read-only; can `GET /v1/configs*`,
     `GET /v1/beads*`). Stored as a K8s Secret in the gardenio-next namespace
     (value is safe to hold cross-env because it is read-only and scoped).
   - On gardenio-next: the mirror uses the pod's in-cluster `Admin` token.
   - Both are mounted into the CronJob pod.

4. **Filter binary** (`gb-mirror`) — new small Go binary in
   `gardenio/controller/cmd/gb-mirror/`. Responsibilities:
   - Apply allow-list (deny-list doc §4): reject anything whose `kind` or `type`
     label isn't in the allow-list.
   - Apply deny-list patterns (§2) over labels, description, and `fields` JSON;
     drop matches; emit a structured warning line per drop so ops can audit drift.
   - Apply per-type partial-mirror strippers (§3) for `type:account` and
     `type:config` title=`auth` / title=`claude-mcp`.
   - Fail closed: unrecognized type or schema → deny.
   - Load the deny-list from the doc file (committed alongside the binary), NOT
     baked into the binary — per §5 rule 5 of the deny-list doc.
   - Emits metrics (count copied, count dropped-by-allowlist, count dropped-by-denylist,
     count partially-stripped).

5. **Scope of the API calls**:
   - `gb config dump` already returns `{configs, config_beads, advice}`. It covers
     the KV table *and* the config-kind beads. That is the right shape.
   - Config-kind beads outside the allow-list (e.g. `type:account` runtime rows)
     are already surfaced by `dump` and are filtered/stripped by the filter step.
     No new API is required.

6. **Observability**:
   - CronJob success/failure is a standard K8s event; the gardenio controller's
     existing alerting covers it.
   - Drop-counter breaches (e.g. >N drops per run) surface as a `bug`-type bead
     auto-opened by the mirror binary, so a human can audit whether the deny-list
     needs adjustment.

7. **Rollback**: a bad filter run is undone by running the binary again once the
   filter bug is fixed — the gardenio-next config store is fully derivable from
   gardenio's. We never lose data in gardenio-next that isn't also in gardenio.

---

## 4. Why not the alternatives (short form)

- **B (Postgres logical replication)** loses the scope fight at row 1 — it can't
  selectively copy config-kind beads without copying issue-kind beads, and it can't
  strip per-env JSON fields. It also multiplies the blast radius by orders of
  magnitude vs option A.
- **C (one-shot seed)** violates the "propagates within cadence" acceptance
  criterion and would require a chart redeploy every time a new decision template
  or custom type lands. We keep C *as the bootstrap* — it is step 1 of option A.
- **D (NATS live)** is a real-time optimization for a problem that does not have a
  real-time requirement. Config churn in gardenio is hours-to-days, not sub-minute.
  We can adopt D later as a drop-in replacement once A is proven.

---

## 5. Cadence

**Hourly** (the CronJob schedule `"23 * * * *"`). Rationale:

- Gardenio config changes (new custom types, prefixes, role contexts, decision
  templates) land via MR and merge-to-main; the natural cadence is MR-merge rate,
  which is on the order of days. Hourly is conservative and keeps drift ≤ 1h.
- Hourly keeps K8s scheduling cheap (one pod per hour) and leaves enough headroom
  that a pod running 5 minutes on a large dump does not overlap with the next run.
- If a specific change is urgent, an operator can trigger the mirror manually with
  `kubectl create job --from=cronjob/gb-mirror gb-mirror-<tag> -n gardenio-next`.
  That handle means we don't need to tighten the schedule to accommodate rare
  impatience.

Alternatives we considered: every 15m (unjustified load), on-deploy only (drift),
live via NATS (option D — defer).

---

## 6. Non-obvious forks that still need a human

The default recommendation above answers the headline question in this bead's
title (CronJob vs logical repl vs one-shot). These three sub-questions are where
reasonable humans could override — and each will be raised as a `gb decision` if
the implementation task wants to revisit:

1. **Cadence**. Hourly is the default. An operator may want 15m or on-merge-of-
   gardenio-main (via a CI webhook that fires a one-off Job). Default: hourly.
2. **Mirror direction & token custody**. Default: CronJob runs in the
   gardenio-next namespace, holds a read-only token for gardenio and uses the
   in-cluster admin for gardenio-next. Alternative: CronJob runs in gardenio and
   pushes to gardenio-next with an admin token for gardenio-next. The former scopes
   the admin blast radius to gardenio-next; the latter avoids storing a gardenio
   token in a different namespace. Default wins on blast radius.
3. **Deny-list location**. Default: committed to the repo at
   `gardenio/docs/gardenio-next-mirror-deny-list.md`, baked into the mirror image at
   build time, loaded as file inside the container. Alternative: store the deny-list
   as a config-kind bead in gardenio (self-hosting). Default wins on simplicity and
   on the bootstrap paradox (you need the deny-list to *start* mirroring).

---

## 7. Implementation outline (for follow-up tasks)

Split into three tasks that can be picked up once this decision is recorded:

- **Implementation-1: `gb-mirror` binary skeleton** — Go binary under
  `gardenio/controller/cmd/gb-mirror/`. Reads `gb config dump` JSON, applies
  allow-list + deny-list + field-stripper, writes via `gb config load` semantics
  (or direct `PUT /v1/configs*` + bead create). Unit tests exercise every deny-list
  rule in §2.1–2.10 and every partial-mirror rule in §3.
- **Implementation-2: Helm chart wiring** — adds the CronJob manifest (in
  `gardenio/helm/gardenio/`) gated on a values flag `mirror.enabled`; adds the
  post-install Job; wires the image (reused from the existing agent image, so no
  new build). Values overlay in `acme-helm-chart/values/gardenio-next.yaml` sets
  `mirror.enabled: true` and provides the source/dest URLs + token secret refs.
- **Implementation-3: Observability + drift handling** — drop-counter threshold
  auto-opens a bug bead via the kbeads HTTP API; metrics exposed on a Prometheus
  endpoint if the ambient platform scrapes it.

Each task should reference this doc and the deny-list doc, and both docs should
be updated together when categories change (per deny-list §6).

---

## 8. Source-of-truth pointers

- Deny-list doc: [`gardenio/docs/gardenio-next-mirror-deny-list.md`](./gardenio-next-mirror-deny-list.md)
- Advice: `ka-0000007027` (*gardenio-next mirror: config-only, with per-env deny-list*)
- Existing dump/load primitives: `gb config dump`, `gb config load` in
  `gardenio/controller/cmd/gb/cmd_config_*.go` (used for daemon backups today)
- HTTP API: `GET/PUT/DELETE /v1/configs/{key}` + `GET /v1/configs` (see
  `kbeads/internal/server/http.go`); config-kind beads exposed via standard
  `/v1/beads*` endpoints.
- Epic: `ka-0000004441` (gardenio-next Epic 4); root: `ka-0000005356`.
- Predecessor tasks: `ka-0000000108` (scope), `ka-0000005103` (deny-list, MR !1252).
