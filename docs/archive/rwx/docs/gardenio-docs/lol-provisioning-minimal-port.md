# LOL Release-Pipeline Port — Provisioning / Wiring (Minimal-Port Pilot)

**Bead:** ka-0000007574 · **Parent epic:** ka-0000006835 · **Operator:** Matt (slack:U0OPERATOR1)
**Decision of record:** [lol-cibilling-target-decision.md](./lol-cibilling-target-decision.md) ·
**Code-level track:** [lol-release-port.md](./lol-release-port.md) (landed in MR !3168)
**Downstream dry-run:** ka-0000008029 · **BYO-cluster follow-up:** ka-0000005481

## Operator decision that set the targets (binding)

Decision **ka-0000006193**, resolved 2026-05-29 by Matt:

> *"we are doing gitlab not github and a token has been provided there. for rwx we
> can target acme org same as gardenio right this instant and then switch later."*

This **supersedes** the GHA+GHCR recommendation in `lol-cibilling-target-decision.md`
for the pilot. We take the **minimal-port** path: reuse the existing gardenio GitLab +
acme-RWX infrastructure for the first dry-run, and defer the dedicated-LOL-org +
OSS-credits + GHCR migration to a follow-up.

| Surface | Pilot target (minimal-port) | Deferred (dedicated-LOL) |
|---|---|---|
| VCS / repo | `gitlab.com/example-org/gardenio` (proj `12345678`) | a public LOL GitLab project |
| CI compute | RWX **acme** org, vault `gardenio-main`, existing `RWX_ACCESS_TOKEN` | dedicated LOL RWX org + OSS credits |
| Image registry | `<registry>/gardenio` | `ghcr.io/liveoaklabs/gardenio` |
| Chart registry | `<registry>/gardenio/charts` | `ghcr.io/liveoaklabs/gardenio/charts` |

## 1. The provided GitLab token — identity & scope (confirmed)

The "token already provided on the GitLab side" is the **gardenio group bot PAT**, not a
new credential:

- **Identity:** user `group_14176856_bot_259bb18fe9d9d91bf5bbb2a4670f7eaa` (id `34915288`,
  display name `gardenio`) — the group access token for **example-org** (group
  `14176856`).
- **Available to agents as:** env `GITLAB_TOKEN` / `GLAB_TOKEN` (this pod), and k8s secret
  `gardenio-gitlab-token` in the `gardenio` namespace.
- **PAT id `21599570`**, expires **2027-03-27**, active.
- **Scopes:** `api, read_api, read_repository, write_repository, read_registry,
  write_registry, read_virtual_registry, write_virtual_registry`.

**Scope assessment — sufficient for the pilot:**

| Pipeline need | Required scope | Present? |
|---|---|---|
| push 13 service images (`docker.yml`, `promote.yml`) | `write_registry` | ✅ |
| push Helm OCI chart (`helm.yml`, `promote.yml`) | `write_registry` | ✅ (GitLab OCI charts live under the same Container Registry) |
| push tags / create release (`release.sh`, `.gitlab-ci.yml`) | `write_repository` + `api` | ✅ |
| read CI/CD project variables | Maintainer role (not a PAT scope) | ❌ — read returns `403`; not needed, see §2 |

One token covers image push **and** OCI chart push — GitLab serves both from the project
Container Registry (`<registry>/gardenio/...`, incl.
`charts/gardenio`).

## 2. Vault-var wiring — **nothing new to set for the pilot**

The headline result of the minimal-port decision: the code defaults from MR !3168 already
**equal** the chosen targets, so the pilot is the **status-quo gardenio pipeline** with no
vault changes.

| Var (MR !3168) | Code default | Pilot target | Action |
|---|---|---|---|
| `IMAGE_REGISTRY` | `<registry>/gardenio` | same | **none — leave unset** |
| `CHART_REGISTRY` | `<registry>/gardenio/charts` | same | **none — leave unset** |
| `REGISTRY_USER` / `REGISTRY_TOKEN` | already in vault `gardenio-main` | same | **none — already present** |
| `RWX_ACCESS_TOKEN` | vault `gardenio-main` (acme org) | same | **none — already present** |

`gardenio-main` already carries the `REGISTRY_USER`/`REGISTRY_TOKEN` pair the **live
production** pipeline uses to push to this exact registry today, so image + chart publish
are already authorized. RWX resolves any unset secret to an empty string, and every
consumer has a `${VAR:-<gitlab-default>}` fallback (`docker.yml:593`, `helm.yml:84`,
`promote.yml:192`, `mirror.yml:54`) — so leaving the new vars unset is the correct,
non-breaking configuration for the pilot.

**When the dedicated-LOL switch happens (deferred):** set the four vars on the LOL RWX
vault to the GHCR/`ghcr.io/liveoaklabs/...` values per the table in
[lol-release-port.md](./lol-release-port.md#secrets-surface-an-external--lol-operator-must-provide)
and export `GARDENIO_RWX_TOKEN` for `release.sh`. No code change is needed at that point —
the parameterization already exists.

## 3. Deploy / E2E / smoke — scoped OUT of the pilot dry-run

`deploy.yml`, `deploy-rwx.yml`, `e2e.yml`, and `smoke-rwx.yml` are **deeply ACME-EKS
coupled** and are NOT reworked here. Confirmed couplings:

- `aws eks update-kubeconfig --name $E2E_EKS_CLUSTER --region us-east-1` with
  `E2E_AWS_ACCESS_KEY_ID` / `E2E_AWS_SECRET_ACCESS_KEY` (`deploy.yml:97-104`).
- Ephemeral `gardenio-rwx-*` namespaces; keycloak CRDs (`KeycloakRealm/Client/Group/RealmRole`);
  `gardenio-forwardauth-secrets`; litellm/slack secrets.
- E2E/smoke suites assume the `gardenio-rwx` singleton + ACME keycloak/forwardauth/litellm
  stack is live.

A public, reproducible-by-an-external-user path needs a **BYO-cluster** rework
(parameterize the cluster target; make keycloak/forwardauth/litellm optional or ship a
self-contained values profile; no-op the E2E/smoke gates when ACME secrets are absent).
That is the deepest item in the port and is tracked as a dedicated follow-up:
**ka-0000005481**. It is **not** required for the build→push→promote→helm-publish dry-run.

### What the ka-0000008029 dry-run WILL / WON'T cover

| Stage | In dry-run? | Notes |
|---|---|---|
| `ci.yml` build + lint + test (preflight) | ✅ | runs on acme RWX |
| `docker.yml` build + push 13 `-rc` images | ✅ | → `<registry>/gardenio` |
| `promote.yml` promote `-rc` → plain + `latest` | ✅ | same registry |
| `helm.yml` package + push OCI chart | ✅ | → `…/gardenio/charts` |
| `.gitlab-ci.yml` create-release | ✅ | GitLab Release on gardenio |
| `mirror.yml` Docker Hub mirror gate | ✅ (only if mirror.yml changed) | already `IMAGE_REGISTRY`-parameterized |
| `deploy-rwx` → staging EKS | ❌ deferred | ka-0000005481 |
| `e2e.yml` full E2E | ❌ deferred | ka-0000005481 |
| `deploy-prod` | ❌ deferred | ka-0000005481 |
| **external-pullability check** (pull image + chart with **no ACME creds**) | ⚠️ partial | GitLab registry/charts are **not anonymous-public** — see §3.1 |

#### 3.1 Caveat: GitLab registry is not anonymous-public

Success criterion #3 for the LOL pilot is *"images + helm chart pullable by an external
user with no ACME creds."* The acme GitLab Container Registry is **private** — an
anonymous pull will `401`. For the minimal-port pilot this means:

- The dry-run **proves the publish path is green** (images + chart land at the targets and
  are pullable **with the gardenio token**).
- It does **not** prove anonymous external pull. True anonymous-public pull is a property
  of the **dedicated-LOL / GHCR** target (GHCR public packages are anonymous-pullable),
  and lands with the deferred GHCR migration.

The dry-run (ka-0000008029) should record this explicitly in its billing/audit writeup:
publish-path green on acme-GitLab; anonymous-public pull deferred to the GHCR switch.
**Done** — see [lol-dryrun-billing-audit.md](./lol-dryrun-billing-audit.md).

## 4. Sync runbook

Keeping the public LOL repo in step with internal gardenio is handled by the **sanitize
pipeline** — see [lol-sync-runbook.md](../../../../../gardenio/docs/lol-sync-runbook.md).

## Summary — what this bead changed

- **Confirmed** the provided GitLab token (gardenio group bot PAT) has the registry +
  repository + OCI-chart scopes the pilot needs; no new token required.
- **Determined** the concrete GitLab + acme-RWX targets are exactly the existing
  pipeline defaults; **no new vault wiring** is required for the pilot.
- **Scoped** the ACME-EKS-coupled deploy/E2E/smoke rework as an explicit follow-up
  (ka-0000005481) and documented precisely what the dry-run will/won't cover, including
  the anonymous-pull caveat — so the dry-run is not blocked on the deepest item.
- **Wrote** the public-repo sync runbook ([lol-sync-runbook.md](../../../../../gardenio/docs/lol-sync-runbook.md)).
