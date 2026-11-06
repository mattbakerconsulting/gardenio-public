# Moving Keycloak from ECS to In-Cluster: Assessment

**Tracking bead:** ka-0000004896 (parent of ka-0000007266 — the prior
"GitLab-direct" assessment that prompted this follow-up).

**Question (Matt, 2026-05-04):** Rather than dropping Keycloak in favor
of GitLab IdP directly (ka-0000007266), can we move the Keycloak server
from ECS into the EKS cluster? Assess difficulty.

## TL;DR

**Smart, mostly de-risked, scope depends on one fact we don't yet know:
whether the ECS Keycloak serves only gardenio or also other ACME apps.**

What's already easy because the chart is the way it is:
- The realm, all 11 clients, roles, groups, and the GitLab IdP federation
  are already declarative as `v1.edp.epam.com/v1` CRDs in
  `gardenio/helm/gardenio/templates/keycloak/*.yaml`. The operator just
  needs a `ClusterKeycloak` to point at — ECS or in-cluster, it doesn't
  care.
- The Keycloak operator is already deployed in EKS (it's what reconciles
  the realm CRDs against ECS today). No new operator needed.

What changes:
- The Keycloak server itself moves into a pod (StatefulSet via the
  Bitnami chart, **or** a `Keycloak` CR if we adopt the upstream
  `k8s.keycloak.org/v2alpha1` operator alongside the existing epam
  realm-management operator).
- A backing Postgres database (today: whatever ECS uses; tomorrow: RDS
  or CNPG, see §3.2).
- TLS + DNS for `auth.<host>` shifts from ALB-in-front-of-ECS to
  Traefik-in-front-of-pod.

Estimated effort for the gardenio-only variant (§4 Option A.3): **5–8
dev-days** + an outage-window cutover. For the "decommission ECS
entirely" variant (§4 Option A.2), effort scales with however many
non-gardenio consumers the ECS Keycloak still has.

This is **substantially cheaper than the GitLab-direct path**
(ka-0000007266 estimated 12–18 dev-days) because none of the kbeads
authorization rework, service-account-rework, or session-revocation
rework is needed — Keycloak stays Keycloak.

---

## 1. What we're actually moving

| Layer | Today (ECS) | After move (in-cluster) |
|---|---|---|
| Keycloak server (Java) | ECS task, fronted by ALB | Pod / StatefulSet behind Traefik |
| Postgres backing | (presumed) shared RDS, "acme-keycloak" DB | RDS (same or new), or CNPG, or shared with beads RDS — see §3.2 |
| `ClusterKeycloak` CR named `acme-keycloak` | already in EKS, **points at ECS** via admin URL/credentials | rewritten to point at the in-cluster Keycloak service — same CR shape |
| Realm + 11 clients + roles + groups + GitLab IdP CRs | already in EKS, gardenio namespace | unchanged |
| ForwardAuth (oauth2-proxy) | EKS, points at `auth.e2e.dev.acme.example` | unchanged if we keep the host; trivial value-only change otherwise |
| `auth.e2e.dev.acme.example` DNS → ALB → ECS | Route53 → ALB → ECS | Route53 → Traefik (or new ALB) → pod |
| TLS cert for `auth.e2e.dev.acme.example` | ACM cert on ALB | cert-manager-issued cert in cluster (same pattern as every other gardenio ingress) |

**Important:** the EKS-side **operator** and **realm/client/role CRDs**
are not what's on ECS. They're already in-cluster. Only the Keycloak
*runtime* is external. That's why the move is narrower than the prior
"remove Keycloak" research suggested when read at a glance.

## 2. The unknown that scopes the work

`auth.e2e.dev.acme.example` is the ACME-wide Keycloak hostname. The
`ClusterKeycloak` is named `acme-keycloak` (not `gardenio-keycloak`).
The realm hosted there is named `gardenio`, but **the server itself is
likely shared with other ACME services** — kbeads/INFRASTRUCTURE.md
files describe a single ECS Keycloak in the broader acme/platform
estate.

Consequence — three flavors of "in-cluster":

- **A.1.** Move ECS Keycloak in-cluster, **keep `auth.e2e.dev.acme.example`**,
  **keep all realms** (gardenio + whatever else). Zero user-visible
  change. Decommissions ECS. Scope balloons if other realms have their
  own constraints.
- **A.2.** Move ECS Keycloak in-cluster, **keep `auth.e2e.dev.acme.example`**,
  but **scope to gardenio realm only** by negotiating non-gardenio
  realms off ECS first or in parallel. Cleaner, slower.
- **A.3.** Stand up a **second, gardenio-only in-cluster Keycloak** at
  `auth.gardenio.e2e.example.com`. Migrate the gardenio realm to it.
  Leave ECS Keycloak alone for other consumers. Highest user-impact
  (issuer URL changes — see §3.4) but smallest blast radius for
  non-gardenio services.

**Recommend A.1 if gardenio is the only consumer; A.3 if not, with A.2
as the long-term path.** The decision branches on the answer to:
*who else uses `auth.e2e.dev.acme.example`?* Operator question — see §6.

## 3. Per-area assessment

### 3.1 Keycloak runtime in EKS

Two reasonable shapes:

**(a) Bitnami `keycloak` chart as a tenant-system sibling.**
StatefulSet with persistence, Service, ServiceMonitor. Traefik
IngressRoute on top. Operator maturity: high. Already used by other
Acme tenants (see `tenant-system` chart references in
`canonical-prod-tenant-system-chart.md`). epam operator continues to
manage realm config — Bitnami is just the runtime.

**(b) Upstream `k8s.keycloak.org/v2alpha1` Keycloak operator.**
Manages a `Keycloak` CR + `KeycloakRealmImport` CRs natively. Would
duplicate work the epam operator already does today (it's *also* a
Keycloak operator, but the EDP fork). Not recommended unless we want
to retire the epam operator altogether — and that's a separate epic.

**Pick (a).** ~1–2 days to author the chart values + helm-deploy + verify
operator can reach the in-cluster service.

### 3.2 Postgres backing

Three options, decreasing migration cost:

| Option | Notes |
|---|---|
| **Reuse existing RDS, point new Keycloak at the same DB** | Zero data migration. Cutover = stop ECS task, start in-cluster pod with same `JDBC_URL`. **Best for A.1**. Requires confirming the RDS instance is reachable from EKS (likely is — same VPC). |
| **New small RDS for in-cluster Keycloak (db.t4g.small ≈ \$15/mo)** | Snapshot existing → restore into new instance. Independent backup window. Best for A.3 (clean separation). Already designed in `canonical-prod-keycloak-placement.md` D-KC-5. |
| **CNPG (in-cluster Postgres)** | Avoids RDS entirely. Adds CNPG operator dependency. Cheapest. Backup story is CNPG-managed. Aligned with the "swap-out point" in the canonical-prod tenant-system design (`canonical-prod-tenant-system-chart.md`). |

Recommend **option 1 for A.1** (lowest cutover risk), **option 2 for
A.3** (clean separation matches the new realm). Option 3 is fine but
adds a moving part that won't pay for itself in this scope.

### 3.3 TLS + DNS

If we keep `auth.e2e.dev.acme.example` (A.1 / A.2):
- cert-manager issues a Let's Encrypt cert for the host. Already the
  pattern for every other `*.e2e.dev.acme.example` gardenio ingress.
- DNS flip: Route53 record for `auth.e2e.dev.acme.example` repointed from
  ECS-fronting ALB → Traefik IngressRoute (or new ALB if we want to
  keep an ALB in front for parity).
- Cutover window is the DNS TTL + a few seconds for pod readiness.

If we use a new host (A.3):
- New cert-manager cert. New Route53 record. No conflict with old
  host. Lowest-risk DNS posture.
- iOS app issuer URL change (see §3.4) becomes load-bearing.

### 3.4 Client-side blast radius

| Surface | A.1 (same host) | A.3 (new host) |
|---|---|---|
| Web SPAs (beads-viewer, beads3d, coopmux, landing, escalation, roles-server) | none — issuer URL unchanged | values-only change in `gardenio.yaml` (`issuerURL`, redirect URIs) |
| ForwardAuth (oauth2-proxy) | none | values-only |
| iOS app (`gardenio-ios` client, custom-scheme redirect) | none | **iOS update required** — issuer URL is in the OIDC client config inside the app. Same problem the canonical-prod doc D-KC-3 frames. |
| Service-account clients (`gardenio-beads-api`, `gardenio-admin-api`) | none | values-only (issuer + clientSecret regen) |
| GitLab OAuth app for IdP federation | none — redirect URI is `auth.e2e.dev.acme.example/auth/realms/gardenio/broker/gitlab/endpoint` (unchanged) | **new GitLab OAuth app** with new redirect URI |
| `gardenio-rwx` realm reuse (`realm.create: false`, shared with prod) | none — same realm | rwx either follows prod to new host (cutover sequencing), or stays on ECS until ready |

**A.1 is significantly less invasive.** No iOS update, no new GitLab
OAuth app, no cookie-domain reshuffling. The trade-off is that you
must take *all* current ECS Keycloak realms with you, which depends on
the open question.

### 3.5 What stays the same (this is the biggest argument)

Because the realm/client/role/group/IdP CRDs already live in EKS and
are GitOps-managed via the chart:

- All 11 OIDC clients: re-rendered identically. No code change.
- Role-based authz in kbeads (`write_policy.min_role` at ~14 admin
  bead types): unchanged. The token shape stays exactly the same —
  same `realm_access.roles` claim, same audience, same JWKS endpoint.
  The painful kbeads/role-resolver work that the GitLab-direct path
  required (ka-0000007266 §2.1) is **not** needed here.
- Service-account clients (`gardenio-beads-api`, `gardenio-admin-api`):
  unchanged. The Keycloak admin REST API still exists; the
  client-credentials grant still works. Session revocation on
  offboarding stays operational. (GitLab-direct's "wait one refresh
  window" trade-off goes away.)
- Federation point preserved: GitLab IdP keeps working, and the
  `corporate` IdP slot for Google/Azure stays available — the
  Phase-2 federation plan from `escalation-auth-recommendation.md`
  is not foreclosed.
- CLI auth path (`BEADS_OIDC_ISSUER_URLS`, `gb login`): unchanged
  whether we pick A.1 or A.3.

## 4. Difficulty rating per workstream

Effort assumes one engineer; lower bound = happy path, upper bound =
one nontrivial cutover hiccup.

| Workstream | A.1 (same host, all consumers) | A.3 (new host, gardenio only) |
|---|---|---|
| Bitnami Keycloak chart values + StatefulSet up in EKS | 1–2d | 1–2d |
| Postgres decision + provisioning | 0.5d (reuse) | 1–2d (snapshot/restore to new RDS) |
| cert-manager + DNS for the auth host | 0.5d (rewire) | 0.5d (new cert/record) |
| `ClusterKeycloak` CR repointed at in-cluster service | 0.5d | 0.5d |
| Smoke against `gardenio-rwx` (issuer URL switch test) | 1d | 1d |
| iOS app update for issuer URL | n/a | 1–2d + App Store release cycle |
| New GitLab OAuth app registration + secret rotation | n/a | 0.5d |
| Production cutover (DNS flip + watch) | 0.5d + outage window | 1d (parallel run, then DNS flip per app) |
| ECS task decommission | 0.5d (after stability window) | n/a — leave ECS alone |
| Documentation + runbook updates (`docs/secret-rotation.md`, ingress runbooks, INFRASTRUCTURE.md) | 0.5d | 0.5d |
| **Total (gardenio-only effort)** | **~5–7 dev-days** | **~7–10 dev-days** + iOS release window |

Add 0–N days to A.1 for any non-gardenio consumer that has its own
cutover requirements (each is essentially "test their app against the
new server, watch their logs"). If those consumers are in the same
operator's hands (devops team), parallelize.

## 5. What's load-bearing and would break loudly

1. **Postgres reachability from EKS to wherever the existing data
   lives.** If the RDS backing the ECS Keycloak is in a different
   subnet/SG that EKS pods can't reach, A.1's "reuse RDS" path needs
   a SG rule change first. Verify before cutover.
2. **Admin credentials.** The `ClusterKeycloak` CR carries the admin
   username/password (or admin client credentials) the operator uses.
   Rotating these mid-cutover is fine, but the new admin must be
   pre-provisioned in the realm before flipping the CR.
3. **`gardenio-beads-api` service-account secret.** Already per-env;
   regenerated by the operator on realm reconcile. The gardenio
   controller picks it up from `gardenio-beads-api-secret` (or
   equivalent). Confirm the controller does a fresh fetch, not a
   cached read.
4. **ForwardAuth cookie / state secret rotation timing.** If the host
   stays the same and cookies are domain-scoped, in-flight sessions
   can survive a Keycloak runtime swap if the Postgres data is the
   same (same client secrets, same JWKS keys). If we rotate JWKS as
   part of the move, every session is invalidated. **Recommend not
   rotating JWKS on the cutover** — preserve the realm export's keys.
5. **Other consumers' behavior under outage.** During the cutover
   window (DNS TTL + pod warmup), `auth.e2e.dev.acme.example` is briefly
   unavailable for *anyone* who relies on it. Schedule for a low-
   traffic window; pre-warm Keycloak (`/health/ready` against the
   in-cluster pod) before flipping DNS.

## 6. Open questions for the operator

1. **Who else uses `auth.e2e.dev.acme.example`?** Just gardenio, or other
   acme/platform services? The answer picks A.1 (gardenio-only or
   shared with one or two known consumers — fold them in) vs A.3
   (many consumers — leave ECS for them, gardenio gets its own).
2. **What backs the ECS Keycloak today — RDS, RDS-shared-with-other-services,
   or local-storage?** Determines §3.2 path. (Plausibly an RDS
   instance in the same `america-e2e` VPC; needs confirmation.)
3. **Do we need to preserve user state (manually-added local users
   without a GitLab IdP link)?** A.1 with shared RDS preserves
   everything trivially. A.3 needs the realm-export-and-import dance
   from `canonical-prod-keycloak-placement.md` Cutover §, including
   the lazy-vs-bulk-user-migration choice (D-KC-2).
4. **Is the iOS app in active use?** Same question ka-0000007266 §7
   asked. A.1 sidesteps it; A.3 requires an iOS issuer-URL update.
5. **Is `gardenio-rwx` part of this move or stays on ECS during a
   transition?** Same question as canonical-prod D-KC-4. RC's
   `realm.create: false` couples it to whichever Keycloak the prod
   `gardenio` realm lives in.

## 7. Recommended plan (if we proceed)

If the answers to §6 favor A.1 (single consumer or small known set,
RDS reusable, want to decommission ECS):

**Phase 1 — Stand up in parallel.**
1. Bitnami `keycloak` chart values committed under
   `gardenio/helm/gardenio/templates/keycloak-runtime/` (or a tenant-system
   sibling chart, depending on operator preference) — guarded behind
   `keycloak.runtime.enabled: false` by default.
2. Provision/verify Postgres reachability. Decide reuse vs new (§3.2).
3. cert-manager cert for `auth.e2e.dev.acme.example`-equivalent test host
   (e.g. `auth.test.gardenio.e2e.example.com`). Wire IngressRoute.
4. Stand up against `gardenio-rwx`. Flip its `ClusterKeycloak` CR to
   point at the new in-cluster Keycloak. Run the existing E2E suite.
5. **Hold-point: human review + `gb decision` before flipping prod.**

**Phase 2 — Flip prod and decommission ECS.**
6. Pre-flight: DNS TTL lowered to 60s, in-cluster Keycloak pod warm.
7. Cutover: flip the `ClusterKeycloak` CR, then flip the Route53
   record. Watch ForwardAuth + Keycloak logs for 5–15 min.
8. Soak window (24–72h) with ECS task still alive but not serving
   traffic — fast rollback.
9. Decommission ECS task + ALB + (optionally) the old Postgres if a
   new one was provisioned.
10. Update INFRASTRUCTURE.md, secret-rotation.md, ingress runbooks.

If A.3 is required (many consumers on ECS, can't move them all):

Follow the cutover choreography from
`canonical-prod-keycloak-placement.md` §"Cutover choreography (Option A)"
with the additional twist that we're not moving accounts/clusters —
we're standing up a second Keycloak in the same cluster with a
different host. That doc's user-migration guidance (D-KC-2) and iOS
bridge guidance (D-KC-3) apply directly.

## 8. Comparison to ka-0000007266 (GitLab-direct)

The prior research asked: *can we drop Keycloak entirely and use
GitLab IdP at the ingress?* That's a 12–18 dev-day project because
removing Keycloak means rewriting kbeads role-resolution, replacing
two service-account clients, and giving up federation.

This proposal asks: *can we keep Keycloak but move the runtime?* It's
a 5–10 dev-day project because nothing about token shape, role
claims, service-account model, or federation changes. The only thing
that changes is **where the Java process runs**.

If the original motivation for ka-0000007266 was "Keycloak is too
much to operate," the in-cluster move addresses about half of that
("don't have to operate ECS as well as EKS") without giving up the
identity-broker capabilities that a v2 federation story (Slack SSO,
Google/Azure SSO) will eventually want.

## 9. Pointers

- Existing chart Keycloak templates: `gardenio/helm/gardenio/templates/keycloak/*.yaml`.
- Per-env Keycloak posture: `deploy-values/gardenio.yaml` lines 422–695,
  `deploy-values/gardenio-rwx.yaml` lines 538–610.
- Prior in-cluster Keycloak design (canonical-prod context, much of
  the analysis transfers verbatim): `gardenio/docs/research/canonical-prod-keycloak-placement.md`.
- Prior "remove Keycloak entirely" research (the alternative to this
  one): `gardenio/docs/research/keycloak-removal-gitlab-direct.md` (on
  branch `research/ka-0000007266-keycloak-removal`, MR !2094).
- Tenant-system chart bundling discussion (the existing
  `bundle_keycloak` toggle that was designed for exactly this shape):
  `docs/research/canonical-prod-tenant-system-chart.md`.
- Stateful-services / Postgres backing patterns: `docs/research/canonical-prod-stateful-services.md`.
- Infrastructure topology (the file that confirmed Keycloak is on
  ECS): `kbeads/.claude/skills/ci-perf/resources/INFRASTRUCTURE.md`.
