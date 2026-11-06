# beads-viewer — Public Internet Exposure Readiness

> **Audience:** Confluence — leadership / security stakeholders deciding whether to drop the office-VPN IP whitelist on `beads-viewer.gardenio.e2e.example.com`.
> **Source of truth:** [`gardenio/docs/research/beads-viewer-public-internet-readiness.md`](research/beads-viewer-public-internet-readiness.md) (research) and epic [ka-0000002083](https://beads.gardenio.e2e.example.com/beads/ka-0000002083) (plan-of-record).
> **Last updated:** 2026-05-03.

---

## Executive Summary

**beads-viewer is substantially ready to be exposed to the public internet, but is *not yet cleared* for cutover.** Five of the seven hardening pieces are landed; two remain, plus an explicit posture call from Matt on the mutation-endpoint surface.

| Verdict | What it means |
|---|---|
| **Green (landed)** | Login picker (T1), authorization source (T4), per-IP rate-limit + abuse logging (T5), GitLab IdP escalation, pod hardening, OIDC fail-closed behavior |
| **Yellow (work in progress)** | Chart-side fail-closed guard for ForwardAuth (T2); GitLab SSO end-to-end walkthrough on gardenio-rwx (T3) |
| **Red (decision required)** | Explicit Matt sign-off on the **mutation-endpoint posture** — the viewer is not read-only; 32 POST endpoints are exposed to any authenticated `@acme.{ai,com}` user |

**Recommendation.** Do not drop the IP whitelist until **T2 + T3 + Matt approval** are all green. Once those clear, cutover is a one-line values change with a 24-hour monitoring window.

---

## Decisions On Record (and Open)

This section enumerates every operator/Matt decision that has shaped or still gates this cutover. Each row links directly to the decision bead in beads-viewer; click through for the full prompt, the options that were on the table, and (where resolved) the choice and rationale.

| # | Decision | Bead | Status | Resolution |
|---|---|---|---|---|
| D1 | **Approve epic + scope** — file T1–T6 child tasks under a new public-internet epic and let crew start on T1 (login picker) | [ka-0000004594](https://beads.gardenio.e2e.example.com/beads/ka-0000004594) | ✅ Resolved 2026-04-25 (Matt) | **Option (a)** — approve direction; file T1-T6; crew starts T1 |
| D2 | **T4 — Authorization source** — gate `@acme.{ai,com}` via email-domain check (cheapest) vs layer in a `gardenio-viewer` realm role with backfill | [ka-0000002658](https://beads.gardenio.e2e.example.com/beads/ka-0000002658) | ✅ Resolved 2026-04-27 | **Option `t4-email`** — keep `emailDomains` gate; unblocks T2/T6 |
| D3 | **Mutation-surface posture (pre-cutover)** — keep mutations open to any signed-in `@acme.{ai,com}` user (Option A) vs gate the broader mutation surface behind `gardenio-operator` role (Option B); also reconcile the stale "read-only HTML renderer" docstring at `deploy-values/gardenio.yaml:858` | *not yet filed — recommend filing as `gb decision create` before T6* | ⏳ **Open — needs Matt** | See **Blocker 3** below for the two options laid out |
| D4 | **T6 — Final cutover approval** — flip the IP whitelist off in prod after T2 + T3 are green | *not yet filed — will file as part of T6 (ka-0000002062)* | ⏳ **Open — needs Matt** | See **Cutover Go / No-Go Checklist** below |

> All bead links above resolve in beads-viewer (`https://beads.gardenio.e2e.example.com/beads/<id>`). The viewer renders each decision with its full prompt, options, chosen option, and the requesting agent — that is the canonical record, not this doc.

---

## What "Public Internet Ready" Means Here

The viewer already lives behind:

- **AWS ALB** terminating TLS with an ACM cert.
- **Traefik** with security headers, strip-prefix, IP whitelist (the thing we want to drop), ForwardAuth (oauth2-proxy → Keycloak), and rate-limit middlewares.
- **Email-domain gate** (`@acme.{ai,com}`) enforced inside the application, not just the edge.
- **OIDC** with fail-closed provider discovery (5-attempt backoff, `/readyz` → not ready on permanent failure).

The cutover is a posture change, not a re-architecture: we already require SSO. The IP whitelist has been a defense-in-depth layer, not the primary gate.

---

## Defense-in-Depth Posture (Current)

| # | Layer | Status | Notes |
|---|---|---|---|
| 1 | **TLS at edge** | OK | ALB + ACM cert, HSTS via security-headers middleware (`stsSeconds=63072000`, `includeSubdomains`, `preload`) |
| 2 | **Auth chain** | OK | ALB → Traefik → IngressRoute → security-headers → strip-prefix → ip-whitelist (will drop) → ForwardAuth (oauth2-proxy → Keycloak) → rate-limit → service → in-app `oidcauth` middleware → email-domain check → handler |
| 3 | **OIDC fail-closed** | OK | `oidcauth.New` uses bounded exponential backoff; permanent discovery failure returns 503 and `/readyz` reports `not_ready`, removing the pod from the Service. No passthrough on failure. |
| 4 | **Email-domain gate** | OK | oauth2-proxy permits any email; `acme.{ai,com}` enforcement happens in-app via `oidcauth.AllowedEmailDomains`. Two-layer check; rejected users still produce abuse-log signal. |
| 5 | **CSRF** | Adequate-by-default | No app-layer CSRF token. Mitigations are environmental: `_gardenio_oauth2` cookie is `SameSite=Lax`, `Secure=true`; ForwardAuth blocks anonymous POSTs. **Residual risk:** any same-origin XSS or open-redirect would defeat SameSite. (See **Improvement #4**.) |
| 6 | **RBAC — operator gate** | Partial | Operator-role gate covers agent-control endpoints (`/agents/{id}/input`, `/control/claim`, `/keys`, `/keys/cancel`, `/nudge`, `/respond`). |
| 7 | **RBAC — broader mutation surface** | **Decision required** | Bead close/reopen/claim/release, edit description/title/owner/labels/priority/assignee, add comments, submit ratings, resolve/dismiss decisions, project bulk mutations, settings, advice create, view create/delete, quick-spawn — all **authenticated-only**. Any `@acme.{ai,com}` user can run them. |
| 8 | **Rate-limit** | OK | Per-IP, 50 avg / 100 burst, `sourceCriterion.ipStrategy.depth: 1` on X-Forwarded-For. Same limit on public-host, `/oauth2/*`, and `/login` routes. (Per-IP fix landed via T5.) |
| 9 | **Pod hardening** | OK | `runAsNonRoot`, non-zero UID/GID, `seccompProfile: RuntimeDefault`, `allowPrivilegeEscalation=false`, `readOnlyRootFilesystem=true`, `capabilities.drop=[ALL]`, `automountServiceAccountToken=false` |
| 10 | **Dev bypass safety** | OK | `BEADS_VIEWER_DEV_AUTH_BYPASS=1` refuses to start when a K8s SA token is mounted. Cannot accidentally ship a bypassed binary to prod. |
| 11 | **Logging fidelity** | Likely OK | Auth-failure paths log at debug level inside the app; T5 closed assuming Traefik + oauth2-proxy logs land in Mezmo with status / source-IP / path / UA. Worth a 2-minute Mezmo confirmation post-cutover. |

---

## What Is *Not* Done Yet (Cutover Blockers)

### Blocker 1 — T2: Make ForwardAuth fail-closed in the chart  ([ka-0000003476](https://beads.gardenio.e2e.example.com/beads/ka-0000003476))

The chart only adds the `forwardauth` middleware when `keycloak.forwardAuth.enabled` is true. An operator misconfig that sets `beadsViewer.ingress.enabled: true` with `keycloak.forwardAuth.enabled: false` would publish the viewer **unauthenticated**.

**Fix.** In `helm/gardenio/templates/beads-viewer/ingressroute.yaml`, refuse to render the public IngressRoute when `forwardAuth.enabled: false` (`{{ fail "..." }}`), or always include the forwardauth middleware reference (rendering will fail on missing CR if forwardAuth is disabled — the desired fail-closed mode).

**Why blocking.** This is a footgun. Today the IP whitelist hides the misconfig. The moment it comes off, the misconfig becomes a public unauthenticated viewer.

### Blocker 2 — T3: GitLab SSO end-to-end walkthrough  ([ka-0000002649](https://beads.gardenio.e2e.example.com/beads/ka-0000002649))

The escalation that blocked T3 (ka-0000003031 — GitLab OAuth app + AWS Secrets Manager secret) is closed and the Keycloak GitLab IdP CR is healthy. We have not actually walked a `@acme.example` GitLab account through SSO end-to-end on `gardenio-rwx` (which has no IP whitelist).

**Fix.** Walk a real `@acme.example` GitLab account through SSO on `gardenio-rwx`, capture screenshots/logs, exercise both picker buttons and each picker disable flag.

**Why blocking.** We cannot certify the public-internet auth chain works for the GitLab path without observing it work end-to-end on a non-whitelisted host.

### Blocker 3 — Matt approval on mutation-surface posture (Decision **D3**, not yet filed)

> **Decision bead:** to be filed via `gb decision create` before T6 cutover. Will be linked from this row in the **Decisions On Record** table once filed; treat the two options below as the canonical option set.

The viewer description in `deploy-values/gardenio.yaml:858` says "Deep-linkable read-only HTML renderer." **That is stale documentation.** The viewer exposes 32 mutating POST endpoints. The operator-role gate covers the agent-control subset; the rest is authenticated-user-only.

**This has been the policy since the office-VPN era.** Cutover does not change the *trust model* — `@acme.{ai,com}` SSO has always been the trust boundary. It does change the *blast radius*: from "on-VPN Acme staff" to "any `@acme.{ai,com}` mailbox owner". Two reasonable answers, both acceptable; pick one explicitly:

- **Option A — Stay as-is.** SSO is the trust boundary; mutations stay on for any signed-in user. Cheapest, aligns with the iOS app posture. Fix the stale docstring.
- **Option B — Layer in `gardenio-operator` role.** Gate the broader mutation surface (close/reopen/edit/comment/etc.) behind operator role, making the viewer truly read-only for default users. Tighter, but requires backfilling roles for the staff who actually use the viewer day-to-day.

**Why blocking.** Matt should make this call explicitly *before* cutover, so the posture change isn't discovered post-hoc by the first non-staff `@acme.{ai,com}` mailbox owner who opens a ticket.

---

## Recommended Pre-Cutover Improvements

Beyond the three cutover blockers above, the research identifies several improvements that would tighten posture. None block cutover individually; **#4 (XSS pass) is strongly recommended** before flipping the whitelist.

### Improvement #1 — Update the viewer docstring (cheap, do it)

`deploy-values/gardenio.yaml:858` says the viewer is "Deep-linkable read-only HTML renderer." Replace with accurate text that names the mutation surface.

### Improvement #2 — Confirm Mezmo carries Traefik + oauth2-proxy auth-failure events

T5's recommendation. 2-minute Mezmo query. Without this, abuse forensics post-cutover relies on assumed log fidelity rather than confirmed.

### Improvement #3 — Tighter rate-limit on `/login` and `/oauth2/start`

Login bombing defense. T5 left this as "recommend if cheap." Suggested values: 5 avg / 10 burst on those routes specifically, separate from the main 50/100 bucket. Bounded today by Keycloak's own brute-force protection; verify on the realm.

### Improvement #4 — Targeted XSS pass on user-supplied content rendering ⚠️

Go `html/template` is context-aware by default — the risk is anywhere a handler emits `template.HTML` or builds HTML by string concat. Quick scan:

```sh
grep -r "template.HTML\|RawHTML\|unsafe" gardenio/controller/cmd/beads-viewer
```

Focus the review on bead title, description, comments, advice body rendering. **Why this matters now:** SameSite=Lax cookies are the CSRF defense; a same-origin XSS would defeat them. Do this pass before public exposure.

### Improvement #5 — Decide and document mutation-surface policy

See **Blocker 3 — Matt approval** above. Whichever option is picked, document it next to the cutover record so a future on-call can reproduce the reasoning.

### Improvement #6 — Post-cutover monitoring window

24-hour active monitoring after cutover: Mezmo dashboard for auth-failure rate, rate-limit-rejection rate, viewer 5xx, `/readyz` flap rate. Pre-cutover snapshot of who's accessing the viewer (last 7d) so we have a baseline.

---

## Cutover Go / No-Go Checklist (Epic T6 — [ka-0000002062](https://beads.gardenio.e2e.example.com/beads/ka-0000002062))

| # | Item | Status |
|---|---|---|
| 1 | T1 — Login picker page in prod (both buttons enabled) | ✅ Done |
| 2 | T2 — ForwardAuth fail-closed in chart | ⏳ Open |
| 3 | T3 — GitLab SSO verified end-to-end on `gardenio-rwx` | ⏳ Open |
| 4 | T4 — Authorization source decision implemented (email-only) — **decision [ka-0000002658](https://beads.gardenio.e2e.example.com/beads/ka-0000002658)** | ✅ Done |
| 5 | T5 — Per-IP rate-limit + abuse logging confirmed | ✅ Done |
| 6 | Escalation [ka-0000003031](https://beads.gardenio.e2e.example.com/beads/ka-0000003031) — GitLab OAuth + AWS SM secret | ✅ Done |
| 7 | Matt explicit approval to flip the whitelist off — **decision D3 (mutation-surface posture) + D4 (final cutover)**, both still to be filed | ⏳ Open |
| 8 | Pre-cutover snapshot of viewer access (last 7d) | ⏳ Open |
| 9 | Post-cutover monitoring window (24h) | ⏳ Open |

**Three substantive boxes** before cutover is honest: T2, T3, and Matt's approval (with mutation-surface posture clarified at the same time). Items 8–9 are operational steps that bracket the actual flag flip.

---

## Out of Scope

- **Keycloak public exposure** (`auth.e2e.dev.acme.example`) — already public; separate posture review.
- **coopmux / agent terminal exposure** — beads-viewer proxies to coopmux for agent terminal screen/events; coopmux is its own hardening story.
- **iOS app** — separate auth flow, not affected by this cutover.
- **Mezmo / S3 / Grafana data flows** — covered in [`security-audit-observability.md`](research/security-audit-observability.md).

---

## Appendix — References

- **Research:** [`gardenio/docs/research/beads-viewer-public-internet-readiness.md`](research/beads-viewer-public-internet-readiness.md) (full assessment with code references)
- **Plan-of-record:** [`gardenio/docs/beads-viewer-public-internet.md`](beads-viewer-public-internet.md)
- **Rate-limit review:** [`gardenio/docs/research/beads-viewer-rate-limit-review.md`](research/beads-viewer-rate-limit-review.md)
- **Observability audit:** [`gardenio/docs/research/security-audit-observability.md`](research/security-audit-observability.md)
- **Epic:** [ka-0000002083](https://beads.gardenio.e2e.example.com/beads/ka-0000002083) — Drop IP whitelist on beads-viewer; expose to public internet
- **Cutover task:** [ka-0000002062](https://beads.gardenio.e2e.example.com/beads/ka-0000002062) — Prod cutover (T6)
- **Tracking bead for this doc:** [ka-0000006683](https://beads.gardenio.e2e.example.com/beads/ka-0000006683)
- **Decisions of record (resolved):** [ka-0000004594](https://beads.gardenio.e2e.example.com/beads/ka-0000004594) (epic approval), [ka-0000002658](https://beads.gardenio.e2e.example.com/beads/ka-0000002658) (T4 auth source)
- **Open decisions (to file before cutover):** D3 mutation-surface posture · D4 final cutover approval — both will appear in beads-viewer once filed; this doc + the cutover task (ka-0000002062) will be updated with their bead IDs
