# Rate-limit + abuse-logging review for public exposure (ka-0000001362)

Parent epic: ka-0000002083 (drop IP whitelist on beads-viewer; expose to
public internet).

This doc reviews the four areas the dispatch asked us to confirm before
we drop the office-VPN IP whitelist on `beads-viewer.gardenio`:

1. Per-IP behavior of the Traefik rate-limit middleware.
2. Mezmo log fidelity for auth failures and rate-limit rejections.
3. Whether tighter rate-limits on `/login` + Keycloak `authorize` are
   warranted.
4. Brute-force protection (BFP) on the `gardenio` Keycloak realm.

It also corrects a factual error in the plan-of-record at
`docs/beads-viewer-public-internet.md` §4.

## TL;DR

- The plan doc claims Traefik's rate-limit is "per-IP — `ipStrategy.depth: 1`
  on the X-Forwarded-For." **That is wrong.** The IP-whitelist Middleware
  sets `ipStrategy.depth`; the rate-limit Middleware does not. With no
  `sourceCriterion`, Traefik buckets by the connection peer (the AWS LB
  IP), so the chart's `50/100` is a global bucket per ALB node, not per
  client. This is the most important fix in this review.
- Mezmo log fidelity is partial. Traefik CLF lines have `clientip` and
  `verb`/`request`/`response` (status), but `User-Agent` and `Referer`
  arrive as `"-"`. oauth2-proxy logs are not visible in Mezmo for the
  `gardenio` namespace at all under any tag I could find.
- No tighter rate-limit exists on `/login` or the oauth2-proxy
  `/oauth2/start` route — both share the main 50/100 bucket. Worth
  adding a stricter tier (e.g. 5 avg / 10 burst) once per-IP bucketing
  is fixed.
- Keycloak realm chart (`KeycloakRealm` CRD via EDP operator) does not
  expose `bruteForce*` fields. The `gardenio` realm's BFP state has to
  be confirmed via Keycloak admin (out-of-band for this role) and
  enabled either by an operator follow-up or via a `KeycloakRealmComponent`
  that we don't currently render.

## Findings

### F1 — Traefik rate-limit is not per-IP today

`helm/gardenio/templates/beads-viewer/ingressroute.yaml:128-142`:

```yaml
spec:
  rateLimit:
    average: {{ .Values.beadsViewer.ingress.rateLimit.average }}
    burst:   {{ .Values.beadsViewer.ingress.rateLimit.burst }}
```

There is **no `sourceCriterion`**. Traefik's documented default for
`rateLimit` without `sourceCriterion` is to source-bucket by the
connection's `RemoteAddr`. In our cluster, the connection peer is the
AWS LB node — every client looks like the same source IP, so 50/100 is
a global bucket per LB node, not per client.

Compare to `ingressroute.yaml:111-127`, which is structurally what we
want:

```yaml
spec:
  ipWhiteList:
    sourceRange:
      {{- toYaml .Values.beadsViewer.ingress.ipWhitelist.sourceRange | nindent 6 }}
    ipStrategy:
      depth: {{ .Values.beadsViewer.ingress.ipWhitelist.depth | default 1 }}
```

The IP-whitelist Middleware *does* set `ipStrategy.depth: 1`, which is
why the office-VPN whitelist works against the X-Forwarded-For header.
The rate-limit needs the same treatment.

**Fix (cheap, included in this MR's plan):** add

```yaml
spec:
  rateLimit:
    average: …
    burst:   …
    sourceCriterion:
      ipStrategy:
        depth: {{ … | default 1 }}
```

…to every rate-limit Middleware template:

- `templates/beads-viewer/ingressroute.yaml`
- `templates/beads3d/ingressroute.yaml`
- `templates/gitlab-bridge/ingressroute.yaml`
- `templates/grafana/ingressroute.yaml`
- `templates/jira-bridge/ingressroute.yaml`
- `templates/landing/ingressroute.yaml`
- `templates/roles-server/ingressroute.yaml`
- `templates/slack-bridge/ingressroute.yaml`
- `templates/trigger-engine/ingressroute.yaml`
- `templates/beads/ingress.yaml` (main route + `ciRoute`)
- `templates/coopmux/ingress.yaml`

This bug is invisible inside the IP whitelist (everyone is on the
office VPN; rate-limit hits are nil), but becomes a real problem the
moment the whitelist drops.

### F2 — Plan doc factually wrong in §4

`docs/beads-viewer-public-internet.md:80-82` says rate-limit is per-IP
with `ipStrategy.depth: 1`. It isn't. Patch §4 to point at this
findings doc and the F1 fix.

### F3 — Mezmo log fidelity is partial

What Mezmo currently shows for `_app:traefik` access logs over the past
hour from the cluster-wide tenant-system traefik:

| Field        | Captured? | Notes |
|---           |---        |---    |
| `clientip`   | yes       | Looks correct under tenant-system traefik |
| `verb`       | yes       | GET/POST etc |
| `request`    | yes       | full path |
| `response`   | yes       | status code (e.g. 200, 401, 404, 429) |
| `bytes`      | yes       | response size |
| `User-Agent` | **no**    | shows as `"-"` in CLF |
| `Referer`    | **no**    | shows as `"-"` in CLF |
| `X-Forwarded-For` chain | **no** | not exposed |

`User-Agent` is the single most useful field for distinguishing
abuse/scanner traffic from legitimate users; without it, post-incident
forensics is much weaker.

Traefik supports JSON access logs with selectable header capture:

```yaml
accessLog:
  format: json
  fields:
    headers:
      defaultMode: drop
      names:
        User-Agent: keep
        Referer: keep
        X-Forwarded-For: keep
```

Where this lives is operator-domain (the tenant-system traefik chart),
not gardenio — but we should ask for it.

Real scanner traffic confirmed in current logs (e.g. `GET /.env` from
`45.38.78.250` returning 404), which proves (a) public scanners reach
the cluster and (b) we'd benefit from UA capture today.

### F4 — gardenio-namespace ingress traffic not searchable in Mezmo

I could not find any beads-viewer / oauth2-proxy traffic in Mezmo via
common tag predicates:

- `app:traefik beads-viewer` → 0 hits
- `app:traefik gardenio` → 0 hits
- `"beads-viewer.gardenio"` → 0 hits
- `"oauth2-proxy"` → 0 hits

Either:

- the gardenio-namespace traefik instance is not shipping logs (only
  cluster-wide tenant-system traefik is), or
- the tag `_app` for that traffic isn't `traefik` and I'm searching the
  wrong attribute, or
- oauth2-proxy stdout isn't being picked up by the LogDNA agent for
  this namespace.

Needs operator confirmation. We can't recommend "rely on Mezmo for
abuse forensics" until this is resolved.

### F5 — No `/login` / `/oauth2/start` rate-limit tier

Today both pre-auth paths (`/login` for the picker, `/oauth2/start` for
oauth2-proxy) inherit the same 50/100 bucket as the rest of the route.
A targeted brute-forcer doesn't need 50 req/s — 5/10 is plenty for
real users on those paths.

Once F1 is fixed (per-IP bucketing actually working), it's cheap to add
a second Middleware:

```yaml
- name: {{ $fullname }}-loginratelimit
spec:
  rateLimit:
    average: 5
    burst:   10
    sourceCriterion:
      ipStrategy:
        depth: 1
```

…and apply it on the `/login` and `/oauth2/start` rules in addition to
the main `-ratelimit`. This MR proposes adding the values knob
(`ingress.loginRateLimit.{enabled,average,burst}`) but **does not**
wire the second middleware in by default — that's the next PR after
the per-IP fix is verified in staging.

### F6 — Keycloak realm: BFP not configured in chart

`helm/gardenio/templates/keycloak/realm.yaml` (the EDP `KeycloakRealm`
CRD wrapper) renders these blocks:

- `realmName`, `keycloakRef`, `displayName`
- `login.{userRegistration, emailAsUsername, loginWithEmail, verifyEmail, …}`
- `themes`, `sessions`, `tokenSettings`, `passwordPolicy`, `defaultGroups`

Notably absent: anything BFP-shaped. `values.yaml` likewise has no
`bruteForce*` keys under `keycloak.realm`.

The EDP `KeycloakRealm` CRD does not expose BFP fields directly.
Two options to enable BFP for the `gardenio` realm:

1. **Operator-applied**: have an operator set BFP in the Keycloak admin
   UI (or via admin REST API) once. Realm config drift after that is
   a possibility, but BFP changes infrequently.
2. **Render via component CRD**: extend `realm.yaml` (or add a sibling
   template) to render whatever the EDP operator provides for realm
   policy / authentication-flow config. Needs operator support
   verification — this is a follow-up, not in this MR.

### F7 — Keycloak X-Forwarded-For trust required for BFP to work

Keycloak BFP buckets attempts by `ipAddress`. Mezmo `KEYCLOAK_AUDIT`
events for `auth.e2e.dev.acme.example` show
`ipAddress=10.35.231.223` (in-cluster IP), which means Keycloak is not
trusting the X-Forwarded-For header from the LB → Keycloak hop. With
that configuration, BFP would group all attackers under a small set of
proxy IPs and either (a) lock everyone out at once or (b) be effectively
unenforceable depending on the failure threshold.

Setting `proxy=edge` (or its equivalent) on the Keycloak deployment so
that Keycloak honors `X-Forwarded-For` from the trusted LB chain is a
prerequisite for BFP to actually be useful. Out-of-repo for this chart
(Keycloak lives in tenant-system), but on the operator checklist for
the `auth.e2e.dev.acme.example` deployment.

## Recommendations

| # | Recommendation | Owner | Where |
|---|----------------|-------|-------|
| R1 | Add `sourceCriterion.ipStrategy.depth: 1` to all gardenio-chart rate-limit Middlewares (single change, all routes) | this MR | `helm/gardenio/templates/**/ingressroute.yaml`, `templates/beads/ingress.yaml`, `templates/coopmux/ingress.yaml` |
| R2 | Patch `docs/beads-viewer-public-internet.md` §4 to point at this doc and remove the wrong claim | this MR | `docs/beads-viewer-public-internet.md` |
| R3 | Add a `loginRateLimit` values knob and the second Middleware spec for `/login` + `/oauth2/start` (5 avg / 10 burst). Wire in **after** R1 verified in staging | next MR | new template + values |
| R4 | Switch tenant-system traefik to JSON access logs with `User-Agent`, `Referer`, `X-Forwarded-For` capture | operator | tenant-system traefik chart |
| R5 | Confirm gardenio-ns oauth2-proxy + traefik logs are shipping to Mezmo and discoverable by tag (`_app` / `app` / `namespace`) | operator | LogDNA agent config |
| R6 | Enable BFP on the `gardenio` Keycloak realm (e.g. permanentLockout=false, maxLoginFailures=10, waitIncrementSeconds=60, quickLoginCheckMilliSeconds=1000, minimumQuickLoginWaitSeconds=60, maxFailureWaitSeconds=900, failureResetTimeSeconds=900) | operator | Keycloak admin / future CRD |
| R7 | Set Keycloak `proxy=edge` (or equivalent) on `auth.e2e.dev.acme.example` so XFF is honored — prerequisite for R6 to be useful | operator | tenant-system Keycloak |

## Decisions to escalate

The items in **R4–R7** require operator action or cluster credentials
this role does not hold. The companion `gb decision` on this bead
breaks out the operator-only checklist; this MR ships the in-chart
fixes (R1, R2) only.

## References

- Bead: ka-0000001362 (this task)
- Parent: ka-0000002083 (drop IP whitelist; expose to public internet)
- Plan-of-record: `gardenio/docs/beads-viewer-public-internet.md`
- Traefik rate-limit docs: `sourceCriterion`, `ipStrategy.depth`
- Keycloak BFP: `realm` settings under "Brute Force Detection"
