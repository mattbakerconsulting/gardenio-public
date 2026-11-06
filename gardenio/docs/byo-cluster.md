# BYO-Cluster Deploy / Smoke — running gardenio with no ACME credentials

**Bead:** ka-0000005481 (deploy+smoke) · ka-0000003910 (E2E gate) ·
**Parent:** LOL public-release port (ka-0000006835) ·
**Minimal-port context:** [lol-provisioning-minimal-port.md](../../docs/archive/rwx/docs/gardenio-docs/lol-provisioning-minimal-port.md) (archived) §3 ·
**Operator:** Matt (slack:U0OPERATOR1)

This is the public, reproducible path: deploy gardenio to **your own** Kubernetes
cluster, with **no** AWS EKS, **no** Keycloak, **no** ExternalSecrets / AWS
Secrets Manager, **no** Slack, **no** LiteLLM, and **no** ACME-owned DNS/TLS.

The release pipeline's `.rwx/deploy-rwx.yml` + `.rwx/e2e.yml` are deliberately
**not** reworked — they remain the ACME-EKS staging/E2E path (incident-hardened,
load-bearing for the live release). They are scoped OUT of the public path, and
this BYO track runs alongside them. See §"Why a parallel track" below.

---

## TL;DR

```sh
# 1. Point kubectl at YOUR cluster (we never call `aws eks update-kubeconfig`):
export KUBECONFIG=/path/to/your/kubeconfig        # or: --context <ctx>

# 2. Deploy the self-contained profile + run the smoke check:
scripts/deploy-byo.sh --namespace gardenio --release gardenio

# 3. Use it:
kubectl -n gardenio port-forward svc/gardenio-beads 8080:8080
export BEADS_HTTP_ADDR=http://localhost:8080
```

`deploy-byo.sh` pre-creates the three core secrets (random values via
`openssl rand`) if they don't already exist, lints + installs the chart with
`deploy-values/gardenio-byo.yaml`, waits for rollout, then runs `smoke-byo.sh`.

---

## What's in the BYO profile (`deploy-values/gardenio-byo.yaml`)

The Helm chart is already toggle-driven — every ACME subsystem has an `enabled`
flag. The BYO profile just sets the ACME-coupled ones OFF and turns on an
in-cluster Postgres so there is no external dependency at all.

| Subsystem | ACME staging | BYO profile | Why |
|---|---|---|---|
| Postgres | external RDS / AWS | **in-cluster** (`postgres.enabled=true`) | no AWS RDS |
| Secrets | ExternalSecrets ← AWS Secrets Manager | **operator-created K8s Secrets** (`externalSecrets.enabled=false`) | no AWS SM / ESO |
| Auth | Keycloak realm + forwardauth/oauth2-proxy | **off** (`keycloak.enabled=false`, `keycloak.forwardAuth.enabled=false`) | no Keycloak operator / ACME realm |
| Ingress / TLS | Traefik IngressRoutes + cert-manager DNS-01 (Route53) | **off** — ClusterIP + `kubectl port-forward` (`tls.enabled=false`, `traefik.enabled=false`, per-component `ingress.enabled=false`) | no ACME DNS, no Route53 |
| LLM | LiteLLM → Bedrock | **off** (`litellm.enabled=false`, `beads.embedding.enabled=false`, `agents.litellmURL=""`) | no ACME LiteLLM |
| Slack | slack-bridge ← gastown creds | **off** (`slackBridge.enabled=false`) | no ACME Slack app |
| Agents | controller + coopmux | **off by default** (`agents.enabled=false`) | needs an LLM credential; opt-in |
| IRSA | `eks.amazonaws.com/role-arn` SA annotations | **none** — plain in-cluster RBAC | non-EKS clusters |
| Image registry | ACME GitLab registry | `global.imageRegistry` (override with `--image-registry`) | mirror to your own registry if needed |

Resulting stack: **beads daemon + Postgres + NATS** (+ bootstrap Jobs). Verified
with `helm template` / `helm lint` to render **zero** ACME couplings (no
`*.acme.example`, no IRSA ARNs, no `ExternalSecret`/`KeycloakRealm`/oauth2-proxy/
cert-manager objects).

### Core secrets you must provide

Because `externalSecrets.enabled=false`, the chart does **not** self-generate the
core secrets. `deploy-byo.sh` creates them for you if absent, but to control the
values (e.g. share the beads token with a `kd` client) pre-create them in your
namespace `<NS>`:

```sh
kubectl -n <NS> create secret generic <RELEASE>-postgres-password \
    --from-literal=password="$(openssl rand -hex 24)"
kubectl -n <NS> create secret generic kd-beads-token \
    --from-literal=token="$(openssl rand -hex 24)"
kubectl -n <NS> create secret generic coop-credential-key \
    --from-literal=key="$(openssl rand -hex 32)"
```

The beads daemon enforces bearer-token auth at the daemon itself
(`BEADS_AUTH_TOKEN` ← `kd-beads-token`), so disabling forwardauth does **not**
leave the API unauthenticated — clients pass `Authorization: Bearer <token>`.

### Private GitLab repo clone token (agents only)

If you run agents (`agents.enabled=true`) whose project bead `git_url` points at
a **private** GitLab repo, the agent `init-clone` needs a credential or it dies
with `fatal: could not read Username for https://gitlab.com`. There are **two
halves** — both are required (codified after bug ka-0000006337):

1. **The Secret.** `deploy-byo.sh` seeds `<RELEASE>-gitlab-token` (key `token`)
   create-if-missing from a deploy-time `GITLAB_TOKEN` env; absent that env it
   writes a placeholder so the wiring is provable up to the clone. To control
   the value yourself:

   ```sh
   kubectl -n <NS> create secret generic <RELEASE>-gitlab-token \
       --from-literal=token="<glpat- token with read_repository>"
   # or: GITLAB_TOKEN=glpat-… scripts/deploy-byo.sh …
   ```

2. **The project-bead reference.** Per the controller `podspec.go` prefix guard,
   `init-clone` mounts the GitLab cred **only** when the project bead declares a
   secret named `<project>-*` exposing env `GITLAB_TOKEN`. The default release
   name `gardenio` makes the seeded Secret `gardenio-gitlab-token`, which already
   carries the required prefix. Add the entry to the project bead's `secrets`
   JSON (project beads live in the beads DB, **not** in helm — this step is not
   automated by `deploy-byo.sh`):

   ```json
   {"env": "GITLAB_TOKEN", "key": "token", "secret": "<RELEASE>-gitlab-token"}
   ```

   The controller then writes `https://oauth2:$GITLAB_TOKEN@gitlab.com` into the
   agent's `.git-credentials` and the clone authenticates.

---

## The smoke check (`scripts/smoke-byo.sh`)

The full E2E suite assumes the ACME auth/litellm/slack stack and is gated OUT of
the public path. `smoke-byo.sh` is its ACME-free replacement — it verifies the
core stack an external user actually deployed:

1. `beads` Deployment + `postgres`/`nats` StatefulSets reach Ready.
2. `GET /v1/health` answers 200 over a port-forward.
3. A bead CRUD round-trip (create → read → close) succeeds using the
   `kd-beads-token` bearer.

Run standalone against an already-deployed release:

```sh
RELEASE=gardenio NAMESPACE=gardenio scripts/smoke-byo.sh
# health-only (skip the CRUD round-trip):
SKIP_CRUD=1 scripts/smoke-byo.sh
```

Note: most E2E test modules already self-skip when their ACME secret is absent
(`test-slack-*.sh` exit 0 when `SLACK_BOT_TOKEN` is unset;
`test-litellm-cost-tracking.sh` / `test-usage-tracking.sh` skip when
`LITELLM_URL`/`LITELLM_MASTER_KEY` are unset; `lib-gitlab-token.sh` fails open
to SKIP when no transport bearer is resolvable). So the ACME E2E suite degrades gracefully on a bare
cluster — but `smoke-byo.sh` is the supported, self-contained gate.

### Why `smoke-byo.sh` *is* the E2E for the BYO path

The bead acceptance asks for "deploy + E2E + smoke" on a BYO cluster. For an
external user with no ACME creds, `smoke-byo.sh` **is** the end-to-end
verification — it exercises the full deployed stack the way E2E does, scoped to
what a ACME-free deploy actually contains:

- **Workload health** (E2E `stack-health`): every Deployment/StatefulSet the
  profile rendered reaches Ready.
- **API liveness** (E2E `mail-ready` / health probes): `GET /v1/health` answers
  200 through the daemon's own bearer-token auth.
- **Functional round-trip** (E2E `beads-crud`): create → read → close a bead
  over the authenticated HTTP API.

The remaining ACME E2E modules (`tls-certs`, `agent-crew-lifecycle`, the
keycloak/litellm/slack suites) test subsystems the BYO profile turns OFF, so
there is nothing for them to assert on a bare cluster — running them would only
test ACME infra the external user doesn't have. `smoke-byo.sh` is therefore the
**complete** functional gate for the deployed BYO surface, not a reduced subset
of it.

### CI gate that never hard-fails on missing ACME creds (`.rwx/deploy-byo.yml`)

`.rwx/deploy-byo.yml` is the portable, ACME-free CI counterpart to the
load-bearing `.rwx/deploy-rwx.yml` + `.rwx/e2e.yml`. Because an RWX runner has
no Kubernetes cluster, it cannot run a *real* deploy + smoke; instead it
statically validates the BYO artifacts an external user will run:

1. `helm lint` + `helm template` the BYO profile, then **assert the render
   contains zero ACME couplings** (no `*.acme.example`, IRSA role-arn,
   `ExternalSecret`/`KeycloakRealm`/oauth2-proxy/cert-manager/`IngressRoute`).
2. `shellcheck` `deploy-byo.sh` + `smoke-byo.sh`.

It references **no** ACME secret (the clone token is optional and resolves to
`""` for a public repo), so a public LOL RWX org with none of the
`E2E_AWS_*` / keycloak / litellm vault vars runs it green end-to-end:

```sh
rwx run .rwx/deploy-byo.yml --wait                          # local, no push
rwx dispatch gardenio-deploy-byo --param commit-sha=$(git rev-parse HEAD)
```

This is safe to add because the ACME `deploy-rwx.yml` / `e2e.yml` /
`smoke-rwx.yml` workflows are **dispatch-only** (no `github:`/`gitlab:` push or
PR trigger) — they fire only from the gardenio GitLab release/scheduled
pipelines, never from a public push — and the public push/PR pipeline
(`.rwx/ci.yml`) references zero ACME deploy/E2E secrets. A fork's pipeline thus
cannot hard-fail on a missing `E2E_AWS_*` cred: there is no public-triggered
path that consumes one.

---

## Turning subsystems back on

The BYO profile is a floor, not a ceiling. To re-enable a subsystem, override the
matching toggle (the chart already supports each):

- **Agents + LLM:** `--set agents.enabled=true --set coopmux.enabled=true`, set
  `agents.litellmURL` to your LiteLLM/OpenAI-compatible endpoint (or configure a
  credential account bead), and provide the `coop-credential-key` secret.
- **External identity:** `--set keycloak.forwardAuth.enabled=true` and point
  `keycloak.forwardAuth.oidcIssuerUrl` / `clientId` / `cookieDomain` at YOUR IdP
  (Keycloak, Cognito, Auth0, GitHub OAuth via oauth2-proxy). Pre-register the
  client with redirect URI `https://<cookieDomain>/oauth2/callback`.
- **Public ingress + TLS:** set each component's `ingress.enabled=true` + `host`,
  `traefik.enabled=true`, and either `tls.enabled=true` with `tls.domainName` +
  a cert-manager DNS provider, or `tls.existingSecretName` for a pre-issued cert.
- **Slack:** create `slack-bot-credentials` (bot token), set
  `slackBridge.enabled=true` + `slackBridge.slack.channel`.

---

## Why a parallel track (not editing deploy-rwx.yml / e2e.yml)

`.rwx/deploy-rwx.yml` and `.rwx/e2e.yml` are dense with ACME incident history
(singleton helm-lock races, Keycloak CRD orphan cleanup, ephemeral-namespace
finalizer recovery, Service-drift reconciliation). Threading optional-kubeconfig
and optional-secret branches through them would risk the **live** release
pipeline for the benefit of an external user who can't run RWX-against-ACME
anyway. The BYO artifacts here — a values profile + two portable shell scripts an
external user runs against their own `kubectl` — are the reproducible deliverable.
The thin `.rwx/deploy-byo.yml` gate (see §"CI gate that never hard-fails…" above)
validates those artifacts in a public RWX org without touching either ACME
workflow or any ACME secret.

---

## Files

| Path | What |
|---|---|
| `deploy-values/gardenio-byo.yaml` | self-contained, ACME-free Helm values profile |
| `scripts/deploy-byo.sh` | kubeconfig-based deploy (no `aws eks`); pre-creates secrets, installs, smokes |
| `scripts/smoke-byo.sh` | minimal ACME-free health + CRUD smoke check (the BYO E2E) |
| `.rwx/deploy-byo.yml` | portable, ACME-free CI gate: helm lint + template-asserts-ACME-free + shellcheck (no ACME secret) |
| `gardenio/docs/byo-cluster.md` | this doc |
