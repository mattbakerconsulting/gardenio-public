# Edge-auth v2 auth-telemetry contract

**Task:** ka-0000006312 — Edge-auth 403 counter (conditional) + document v2 Keycloak/deny-reason contract
**Epic:** ka-0000002046 — Customer-1 security-event telemetry
**Plan of record:** [`customer-1-telemetry-plan.md`](customer-1-telemetry-plan.md)
**Related:** [`forwardauth-403-force-logout.md`](forwardauth-403-force-logout.md) (the landing `/403` handler whose existence made the v1 counter unconditional)

This doc records (1) what v1 shipped for edge-auth denial telemetry, and (2) the
contract for the two v2 items that are **explicitly deferred as genuinely-new
infra** — each must be **re-gated** (re-scoped, re-approved, re-benchmarked)
when it is picked up, because each stands up a component that does not exist
today. It is the "contract documented only; no emit added" deliverable the
acceptance criteria call for on the v2 items, and the design reference for the
v1 counter.

---

## v1 (shipped this task): `gardenio_forwardauth_403_total`

The landing `/403` force-logout handler exists (Traefik errors middleware +
nginx `/403` page, `forwardauth-403-force-logout*.md`), so per the acceptance
criteria the v1 counter is **emitted**, not deferred.

| | |
|---|---|
| **Metric** | `gardenio_forwardauth_403_total` |
| **Type** | counter |
| **Labels** | `reason`, `route_template` — **no operator label** |
| **Emitted by** | `gardenio/controller/cmd/edge-auth/` (`metrics.go`, `record403`) |
| **Exposed on** | `/metrics`, internal metrics listener `METRICS_LISTEN_ADDR` (`:9092`), never the public auth port |
| **Scraped via** | `prometheus.io/scrape` pod annotations + the `metrics` Service port |

### Where it emits

edge-auth is the only in-process, Go-instrumentable forwardauth component that
returns HTTP 403. oauth2-proxy and the nginx `/403` page are third-party
binaries with no Go emit site, so the counter is recorded at every 403 exit the
shim itself owns:

| handler (file) | route key → `route_template` | deny detail → `reason` |
|---|---|---|
| `handleAuthCheck` (server.go) | `auth_check` | `deny:not-in-gardenio` → `not_in_group` |
| `forwardauthPAT` (forwardauth.go) | `forwardauth` | `deny:not-in-gardenio` → `not_in_group` |
| `handleEntraForwardauth` (entra_forwardauth.go) | `entra_forwardauth` | `deny:entra-forbidden` → `entra_forbidden` |
| `handleProxy` (proxy.go), Entra arm | upstream name (`kbeads`/`coopmux`/`nats`) | `deny:entra-forbidden` → `entra_forbidden` |
| `handleProxy` (proxy.go), PAT arm | upstream name | `deny:not-in-gardenio` → `not_in_group` |
| `handleProxy` (proxy.go), email-domain gate | upstream name | `deny:email-domain` → `email_domain` |

### Bounded-label contract

Both labels are **capped-cardinality by construction** — the whole point of the
plan's "allowlisted `route_template` (≤15), unknown → `other`; no operator
label" rule (`customer-1-telemetry-plan.md`, §label-hygiene). A 403 is
attacker-triggerable, so an unbounded label here would be a
denial-of-wallet/DoS cardinality bomb, and an operator/email label would be
both unbounded and PII.

- **`route_template`** — `forwardauth403RouteTemplate` allowlists
  `auth_check`, `forwardauth`, `entra_forwardauth`, `kbeads`, `coopmux`,
  `nats` (6 values); anything else → `other`. A raw request URL never reaches
  the label.
- **`reason`** — `forwardauth403Reason` maps the coarse in-process deny details
  to `not_in_group`, `email_domain`, `entra_forbidden`; anything else →
  `other`. This is the ceiling of reason granularity available **in-process**.
  Finer reasons require v2 (below).

---

## v2 item (a): oauth2-proxy `X-Oauth2-Deny-Reason` header — deny-reason granularity

**Status: deferred. Contract documented only. No emit added. Re-gate on pickup.**

### Why it's genuinely-new infra

v1's `reason` label is limited to the deny outcomes edge-auth can name from its
own decision logic. The web front door's finer-grained denials
(oauth2-proxy → Keycloak) — expired session vs. missing group claim vs. failed
`--email-domain` vs. upstream-provider error — happen **inside oauth2-proxy**, a
third-party binary the shim does not instrument. Getting that granularity into
the counter is not a code tweak; it requires a **new contract on the response
oauth2-proxy emits**, plus a code path in edge-auth (or the `/403` handler) to
read it.

### Contract

- **Producer:** oauth2-proxy is configured to stamp a response header
  `X-Oauth2-Deny-Reason: <slug>` on its 401/403 auth-subrequest responses (and
  the nginx `/403` page preserves it). oauth2-proxy does not emit this natively
  today — it requires either an upstream build/config change or a small
  Lua/nginx shim in the `/403` path. **This is the genuinely-new infra.**
- **Value set:** a **bounded, allowlisted** enum, e.g.
  `session_expired`, `not_in_group`, `email_domain`, `provider_error`,
  `csrf`, `other`. The allowlist MUST be pinned at pickup and any unknown slug
  folded to `other` — same cardinality discipline as v1.
- **Consumer:** edge-auth's `handleForwardauth`/cookie path (or the `/403`
  handler) reads the header and passes the slug through `forwardauth403Reason`,
  which gains the new cases. `route_template` is unchanged.
- **Backward compatibility:** absence of the header → today's coarse `reason`.
  The header only *refines* an existing label value; it never adds a label, so
  existing dashboards keep working.

### Re-gating checklist (before implementing)

1. Confirm the oauth2-proxy version/deploy in each env can emit the header
   (upstream capability vs. nginx/Lua shim) — this is the load-bearing new infra.
2. Pin the bounded slug allowlist; verify it stays ≤ the `reason` cardinality budget.
3. Verify no slug carries user/PII substrings (no email, no username).
4. Add emit + test; update this doc's v1 table with the new `reason` values.

---

## v2 item (b): Keycloak event-listener sidecar → `gardenio_auth_attempts_total`

**Status: deferred. Contract documented only. No emit added. Re-gate on pickup.**

### Why it's genuinely-new infra

Login success/failure and token-issuance events live **inside Keycloak**, which
emits them only to its internal event bus — nothing in gardenio observes them
today. Surfacing them as metrics requires standing up a **new Keycloak SPI
event-listener sidecar** (a Java provider deployed into the Keycloak pod) that
subscribes to the event bus and exports Prometheus series. That sidecar does
not exist; this is the largest of the deferred items.

### Contract

| | |
|---|---|
| **Metric** | `gardenio_auth_attempts_total` |
| **Type** | counter |
| **Labels** | `event`, `provider`, `outcome` — **no operator/user label** |
| **`event`** | bounded: `login`, `logout`, `token_refresh`, `token_issue`, `register` |
| **`provider`** | bounded: the IdP/broker alias (`keycloak`, `entra`, `gitlab`, …), allowlisted; unknown → `other` |
| **`outcome`** | bounded: `success`, `failure` |
| **Producer** | new Keycloak SPI event-listener sidecar (Java provider) exporting `/metrics` |
| **Consumer** | scraped in-cluster like every other gardenio `/metrics` endpoint |

Cardinality is `|event| × |provider| × |outcome|` — a fixed, small product.
Keycloak event objects carry `userId`/`ipAddress`/`sessionId`; the sidecar MUST
drop them at the export boundary. Per-user auth analytics, if ever needed, are a
separate audit-log stream (kbeads OCSF), never a metric label.

### Re-gating checklist (before implementing)

1. Scope + approve the SPI sidecar build/deploy (Java provider, image, chart
   wiring, Keycloak version compatibility) — this is real new infra, not a flag.
2. Pin the `event`/`provider` allowlists and the `outcome` enum.
3. Enforce the drop-PII-at-export boundary; test that no `userId`/`ip` leaks.
4. Decide whether the box (in-box Keycloak) and cloud both get the sidecar.
5. Add scrape wiring + alerts; document the metric in the telemetry plan.

---

## Summary

| item | v1 status | infra | label contract |
|---|---|---|---|
| `gardenio_forwardauth_403_total` | **shipped (emit)** | none new — in-process edge-auth | `reason`, `route_template` bounded; no operator label |
| `X-Oauth2-Deny-Reason` deny granularity | **deferred (contract only)** | oauth2-proxy header emit (new) | refines `reason` only; still bounded |
| `gardenio_auth_attempts_total` (Keycloak) | **deferred (contract only)** | Keycloak event-listener sidecar (new) | `event`, `provider`, `outcome` bounded; no user label |

Both v2 items are gated on standing up infrastructure that does not exist today
and MUST be re-gated (re-scoped and re-approved) at pickup, not treated as a
follow-up code change.
