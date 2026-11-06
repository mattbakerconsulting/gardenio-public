# Info-density audit harness — agent-pod flow

Source of truth for how the **scheduled** info-density audit
(formula `ka-0000002694`, schedule `ka-0000002369`, epic `ka-0000005684`)
reaches `beads-viewer` from inside an agent pod.

> `.rwx/info-density-audit.yml` — the RWX workflow that ran this same Playwright
> harness from CI against `gardenio-rwx` (same Keycloak helper, same
> `BEADS_VIEWER_BEARER` plumbing, different orchestration shell) — was deleted
> along with the rest of `.rwx/` in the 2026-09-16 RWX decommission (rwx.com
> account/cluster no longer exist); the `make info-density-audit` target that
> dispatched it is gone too. This doc now describes the only surviving path.

## Why not the public host

Earlier iterations targeted the public ingress
`https://beads-viewer.gardenio.e2e.example.com`. The OIDC middleware in
front of beads-viewer requires a full forwardauth code-flow login for
browser sessions; agent pods cannot complete that flow. A directRoute /
bypass-header escape hatch was tried (ka-0000006025, ka-0000003883) and
correctly rendered in cluster, but the AWS ALB intercepts and 308-redirects
before traffic reaches Traefik — fixing that requires AWS-side IaC work
outside the gardenio repo. Operator decision `ka-0000005537` abandoned the
bypass approach.

The retool (ka-0000002478) routes around the ALB entirely: port-forward
straight to the ClusterIP and authenticate to beads-viewer's own OIDC
middleware via a Keycloak `client_credentials` bearer.

## The flow

The schedule `ka-0000002369` pours the formula with
`TARGET_URL=http://localhost:8095`. Each step bead's description bakes in
the prereq below; the warmup and capture steps both run it.

```bash
# 1. Namespace + helm release. POD_NAMESPACE is the downward-API value;
#    in the gardenio namespace it equals the release name.
export POD_NAMESPACE=${POD_NAMESPACE:-$(cat /var/run/secrets/kubernetes.io/serviceaccount/namespace)}
export KC_RELEASE_NAME=${KC_RELEASE_NAME:-$POD_NAMESPACE}

# 2. Mint a viewer-audience bearer via the client_credentials grant.
#    See advice ka-0000006483. The agent ServiceAccount already has
#    `get` on the operator-managed Secret named
#    keycloak-client-${KC_RELEASE_NAME}-client-gardenio-info-density-audit-secret.
# shellcheck disable=SC1091
source gardenio/tests/e2e/scripts/lib-keycloak-client-creds.sh
VIEWER_BEARER=$(mint_client_credentials_token)

# 3a. Bootstrap the captor's operator row. The audit JWT carries two
#     audiences (gardenio-beads-viewer + gardenio-beads-api) so it is also
#     valid against the kbeads daemon's OIDC middleware. POST to the
#     daemon's /v1/operators/me/aliases endpoint upserts the canonical
#     oidc:<email> operator + alias inside one transaction. Idempotent.
#     Without this, /inbox renders the unprovisioned-operator banner
#     (mode_a) and the audit captures an empty page (ka-0000004196).
kubectl -n "$POD_NAMESPACE" port-forward svc/gardenio-beads 8080:8080 \
  > /tmp/beads-daemon-pf.log 2>&1 &
trap 'kill %1 %2 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do
  curl -fsS -o /dev/null http://localhost:8080/healthz && break
  sleep 1
done
curl -fsS -X POST \
  -H "Authorization: Bearer $VIEWER_BEARER" \
  -H 'Content-Type: application/json' \
  --data '{"kind":"agent","subject":"gardenio-info-density-audit","source":"audit-bootstrap"}' \
  http://localhost:8080/v1/operators/me/aliases | jq .

# 3. Port-forward to the ClusterIP and wait for :8095 to bind.
kubectl -n "$POD_NAMESPACE" port-forward svc/gardenio-beads-viewer 8095:8095 \
  > /tmp/beads-viewer-pf.log 2>&1 &
for _ in $(seq 1 30); do
  curl -fsS -o /dev/null -H "Authorization: Bearer $VIEWER_BEARER" \
    http://localhost:8095/health && break
  sleep 1
done

# 4. Warm-up curl: every canonical page should be < 400.
for spec in 'index:/' 'project:/p/gardenio' 'bead-show:/ka-0000002478' \
            'search:/search?q=screenshot' 'agents:/agents' \
            'agents-filtered:/agents?project=gardenio' 'advice:/advice' \
            'health:/health' 'costs:/costs'; do
  slug=${spec%%:*}; path=${spec#*:}
  code=$(curl -s -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer $VIEWER_BEARER" \
    "http://localhost:8095${path}")
  printf '%-18s %-44s -> %s\n' "$slug" "$path" "$code"
done

# 5. Capture: the Playwright config picks BEADS_VIEWER_BEARER up
#    automatically and injects the Authorization header as an
#    extraHTTPHeader.
export BEADS_VIEWER_BEARER=$VIEWER_BEARER
BEADS_VIEWER_URL=http://localhost:8095 make screenshot-beads-viewer
```

## Why `client_credentials`, not the password grant

`gardenio/tests/e2e/scripts/lib-gitlab-token.sh::mint_user_token` mints a
**user** transport bearer for E2E modules that need a real user identity (the
password-grant helper `lib-keycloak-token.sh` was retired in T6b,
ka-0000001463; the `mint_user_token` API is preserved). Agent
pods that just need "a token that beads-viewer will accept" should use
`client_credentials` instead: no user, no password, secret is auto-rotated
by the keycloak-operator and the agent ServiceAccount has narrow
`get secret` RBAC scoped to that one Secret
(`agents.agentServiceAccount.extraRules` in the chart). See advice
`ka-0000006483` for the full rationale.

## Where the bearer is honored

`beads-viewer`'s OIDC middleware (`oidcauth.go::extractToken`) accepts
either:

- An `Authorization: Bearer …` header (this path), or
- The browser cookie minted by the forwardauth code flow (public ingress).

Both check the JWT against the same Keycloak realm + audience
(`gardenio-beads-viewer`), so the bearer issued for the
`gardenio-info-density-audit` client is accepted on every protected route.

The same JWT is **also** valid against the kbeads daemon's OIDC middleware
(audience `gardenio-beads-api`) because the audit client carries two
`oidc-audience-mapper` protocolMappers (see
`deploy-values/gardenio.yaml`/`gardenio-rwx.yaml`). That second audience is
what lets the bootstrap step in §3a call
`POST /v1/operators/me/aliases` directly against the daemon.

## Capturing the OPERATOR surface (`gardenio-camp-audit`)

The default `gardenio-info-density-audit` client carries the **`gardenio-viewer`**
realm role. That is correct for read-only density auditing, but it means the
captured DOM is the **viewer** surface — every operator-gated control is
withheld. The only operator-conditional markup in `beads-viewer` today is the
`/gardener/{id}` lifecycle **control bar** (`{{if .IsOperator}}` →
`enable`/`disable`/`run-now`/`cancel-run`/`retire`/`edit-config`), gated by
`viewerCanOperate()`. So a viewer-scoped capture can NEVER observe it, and the
gardeners-page CAMP metric `control_bar_present` is structurally stuck at 0 even
when the bar ships and deploys (root cause of bug `ka-0000007161`; the
`operator`/`logout` strings the audit saw were CSS class names, not an operator
session).

To capture the operator surface, mint the bearer via the **`gardenio-camp-audit`**
client instead — a sibling of `gardenio-info-density-audit` that is identical
except it carries the **`gardenio-operator`** realm role (and the same
`gardenio-beads-viewer` + `gardenio-beads-api` audiences, so the §3a operator-row
bootstrap still works):

```bash
# Operator-scoped bearer for capturing /gardener/{id} with the control bar.
KC_CLIENT_ID=gardenio-camp-audit OPERATOR_BEARER=$(mint_client_credentials_token)
# …or, via the wrapper:
gardenio/tests/e2e/scripts/fetch-beads-viewer-prod.sh --operator \
  --screenshot --out gardener-detail.png /gardener/<gardener-id>
```

This is read-only in practice: the audit only GETs pages (plus the daemon-side
alias bootstrap); it never POSTs to the operator-gated mutation routes, so the
elevated role grants no write power it actually exercises. The gardeners-page
CAMP audit (formula `ka-0000000382`) MUST use this client for the
`/gardener/{id}` capture so `control_bar_present` reflects reality.

## Auditing a real GARDENER page — target-selection contract (ka-0000007587)

The gardener-page CDQR audit (formula `ka-0000005307`, epic `ka-0000001553`) reuses
this same flow, but it has an extra constraint the density audit does not: it must
render `/gardener/{id}` for a **specific gardener**, and that gardener must be
**resident in the daemon backing whichever viewer the harness can actually reach**.
That coupling is where every recent iteration has silently stalled. The environment
matrix (verified live 2026-08-28, prod-gardenio-eks):

| Env | Viewer OIDC | Agent-mintable bearer? | Backing daemon contains real gardeners? |
|---|---|---|---|
| `gardenio-system` (prod) | **Entra / Azure-AD** for browsers (`login.microsoftonline.com`, role `gardenio-viewer`) **+ a second machine-token issuer**: the EKS OIDC issuer via the **k8sSA branch** (`beadsViewer.k8sSA.*`, off by default) | **Yes, via the k8sSA path** — an audience-pinned projected SA token (`kubectl create token … --audience gardenio-beads-viewer-audit`), NOT interactive Entra. See "Auditing the PROD viewer" below. Requires the branch enabled + audit operator row provisioned (epic ka-0000007405). | **Yes** — the live scheduler pours the real gardeners here |
| `gardenio-rwx` (CI twin) | Keycloak (`gardenio-rwx` realm) | **Yes** — `client_credentials` grant (this doc) | **No** — only E2E fixtures (`cer-gardener-*`, `runs_spawned=0`) |

Two independent facts make this a genuine wall, not a bearer/deploy bug:

1. **The prod viewer is Entra-gated for browsers** (human SSO). Its OIDC middleware
   takes exactly one `OIDC_ISSUER_URL`, so a Keycloak `client_credentials` bearer is
   rejected there even though the prod cluster *has* a `acme-keycloak`. The fix landed
   as a **second, machine-only issuer** — the viewer's `k8sSA` branch (code
   ka-0000006288, decision ka-0000001298 → `entra-audit-client`), which verifies an
   audience-pinned projected ServiceAccount token from the EKS OIDC issuer. See
   "Auditing the PROD `gardenio-system` viewer" below. Enabling it on prod
   (`beadsViewer.k8sSA.enabled=true`, `canonical-prod` terraform apply) + provisioning
   the audit operator row is operator scope (epic ka-0000007405, escalation
   ka-0000000886).
2. **Fixture gardeners 404 on the detail surface on purpose** (`excludeFixtureGardener`,
   bug `ka-0000008054`). So even though the rwx viewer is agent-auditable, its
   fixture gardeners return 404 on the very `/gardener/{id}` page under audit. Pointing
   the harness at rwx does **not** yield a real gardener page; it yields a 404 (or, if
   `BEADS_VIEWER_RENDER_FIXTURE_GARDENERS=true`, a synthetic page — not a real audit).

**Contract for the step-1 gardener picker:** choose the busiest **non-fixture**
gardener **from the same daemon the reachable viewer reads**, and fail loud if the
chosen id `GET /gardener/{id}`s to 404 there — do not fall through to capture/measure
against an absent or fixture gardener (that path reads deceptively green, see the
`/runs`-consistency note below). Picking the busiest gardener from the **prod** `kd`
daemon while auditing the **rwx** viewer is the exact target mismatch that produced
the recurring "blocked" iterations (`ka-0000004832`, `ka-0000004206`).

**Consistency fix shipped alongside this doc:** `GET /gardener/{id}/runs` now applies
the same existence/fixture/mirror 404 guard as `GET /gardener/{id}`
(`resolveLiveGardenerBead`). Previously the lazy `/runs` fragment returned `200` with
an empty body for a gardener that 404s on its parent page, so a monitoring/warm-up
probe on `/runs` alone read green for a nonexistent gardener — masking exactly this
target-selection failure.

**Architectural fork — RESOLVED.** The operator picked option (a) via decision
`ka-0000001298` → `entra-audit-client`, implemented as the **non-Entra machine-token
bearer path** (a second verified issuer on the viewer, not an Entra robot): the
`k8sSA` branch. The prod `gardenio-system` viewer becomes agent-auditable once that
branch is enabled + the audit operator row is provisioned — see the next section.

## Auditing the PROD `gardenio-system` viewer — the k8sSA bearer path

The prod viewer is Entra-gated for browsers, so the Keycloak `client_credentials`
bearer above is **rejected** there. To audit the real, rich gardeners where the live
scheduler pours them (`gardenio-system`), the viewer accepts a **second machine-only
issuer**: the cluster's EKS OIDC issuer, via its `k8sSA` branch (code ka-0000006288,
chart `beadsViewer.k8sSA.*`, decision ka-0000001298). An audit pod mints an
**audience-pinned projected ServiceAccount token** — no interactive Entra flow, no
client secret:

```bash
# Mint the viewer-audience SA token (helper: lib-k8s-sa-token.sh).
export POD_NAMESPACE=gardenio-system
# shellcheck disable=SC1091
source gardenio/tests/e2e/scripts/lib-k8s-sa-token.sh
VIEWER_BEARER=$(mint_viewer_sa_token)          # kubectl create token gardenio-audit \
                                               #   --audience gardenio-beads-viewer-audit --duration 15m

# Port-forward to the ClusterIP (same ALB-bypass as the density audit) and GET
# the real gardener page + its lazy /runs fragment.
kubectl -n gardenio-system port-forward svc/gardenio-beads-viewer 8095:8095 \
  > /tmp/beads-viewer-pf.log 2>&1 &
trap 'kill %1 2>/dev/null || true' EXIT
for _ in $(seq 1 30); do
  curl -fsS -o /dev/null -H "Authorization: Bearer $VIEWER_BEARER" \
    http://localhost:8095/health && break
  sleep 1
done
curl -fsS -H "Authorization: Bearer $VIEWER_BEARER" \
  http://localhost:8095/gardener/ka-0000007129       -o /dev/null -w '/gardener -> %{http_code}\n'
curl -fsS -H "Authorization: Bearer $VIEWER_BEARER" \
  http://localhost:8095/gardener/ka-0000007129/runs  -o /dev/null -w '/runs    -> %{http_code}\n'
```

Or via the wrapper (`--auth sa` is the default of `fetch-beads-viewer-prod.sh`):

```bash
gardenio/tests/e2e/scripts/fetch-beads-viewer-prod.sh /gardener/ka-0000007129   # DOM
gardenio/tests/e2e/scripts/fetch-beads-viewer-prod.sh --screenshot \
  --out gardener.png /gardener/ka-0000007129
```

**Why this token, not the daemon bootstrap:** the SA token carries a single audience
(`gardenio-beads-viewer-audit`) and is viewer-only — it does **not** double as a kbeads
bearer, so the §3a `POST /v1/operators/me/aliases` bootstrap does not apply. Instead
the mapped identity (`system:serviceaccount:gardenio-system:gardenio-audit` →
`gardenio-system.gardenio-audit@acme.example`) is provisioned once, out of band, via the
daemon admin endpoint `POST /v1/admin/operators/link-aliases` (escalation
ka-0000000886). Without that row, authenticated pages render the unprovisioned-operator
banner even though auth succeeds.

**Contract / negative controls** (enforced by the viewer's verifier, mirrored in the
helper): the audience MUST be `gardenio-beads-viewer-audit` — never the apiserver
audience (`https://kubernetes.default.svc`) or the IRSA audience (`sts.amazonaws.com`);
the subject MUST be on the viewer's `allowedSubjects` allow-list (no wildcard); and the
token is fully verified (JWKS + issuer + audience + expiry + signature), never a trusted
header. `mint_viewer_sa_token` refuses to mint a forbidden audience up front.

**Prereqs (operator scope, epic ka-0000007405):** this path is live only once
`beadsViewer.k8sSA.enabled=true` is applied to the prod release (a `canonical-prod`
`terraform apply -target=module.gardenio`, W1 ka-0000005868), the dedicated
`gardenio-audit` ServiceAccount + token RBAC exists (W2 ka-0000002445), and the operator
row is provisioned (W3 escalation ka-0000000886). Until then the mint mechanics work
(the token has the correct `aud`/`sub`/`iss`/`exp`) but the live viewer still 401s
because the branch is disabled — verify the deploy actually rendered `K8S_SA_*` env
before concluding a bearer bug:

```bash
kubectl -n gardenio-system get deploy gardenio-beads-viewer -o json \
  | jq -r '.spec.template.spec.containers[].env[]? | select(.name|startswith("K8S_SA")) | .name'
```

**rwx/e2e fallback:** the `gardenio-rwx` CI-twin viewer is Keycloak-gated, not
Entra-gated, so it uses the `client_credentials` path (`--auth keycloak`, this doc's
main flow). The scheduled density audit and `.rwx/info-density-audit.yml` are
unchanged — they target rwx and keep Keycloak.

## References

- Decision: `ka-0000005537` — operator chose `abandon-directroute`.
- Retool task: `ka-0000002478`.
- Bearer advice: `ka-0000006483` —
  `gardenio/tests/e2e/scripts/lib-keycloak-client-creds.sh`.
- PROD k8sSA bearer helper: `gardenio/tests/e2e/scripts/lib-k8s-sa-token.sh`
  (`mint_viewer_sa_token`); viewer code `internal/oidcauth/k8ssa.go` (ka-0000006288);
  chart `beadsViewer.k8sSA.*`; decision ka-0000001298; plan ka-0000004227 (W4
  ka-0000004324); enablement epic ka-0000007405 / escalation ka-0000000886.
- RWX twin: `.rwx/info-density-audit.yml` + `make info-density-audit`.
- Playwright config (`extraHTTPHeaders` wiring):
  `gardenio/tests/beads-viewer-screenshots/playwright.config.js`.
- Canonical pages list: discovered at runtime from
  `gardenio/tests/beads-viewer-screenshots/tests/canonical-pages.spec.js`.
- Audit epic: `ka-0000005684`.
- Schedule: `ka-0000002369` (cron `37 */6 * * *`, `TARGET_URL=http://localhost:8095`).
- Formula: `ka-0000002694`.
