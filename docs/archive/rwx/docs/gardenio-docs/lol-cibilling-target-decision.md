# LOL CI/Billing Target Selection — Decision Artifact

**Bead:** ka-0000003382 · **Parent epic:** ka-0000006835 · **Blocks:** ka-0000000040 (release-pipeline port)
**Operator:** Matt (slack:U0OPERATOR1) · **Status:** awaiting operator approval

## Problem

The Live Oak Labs (LOL) public gardenio repo must build, test, and cut its own
releases **without billing Matt's personal RWX budget**. Today every CI run
(build/lint/test, docker image builds, deploy, E2E, helm push, mirror gate)
dispatches RWX workflows that meter against the RWX account behind
`RWX_ACCESS_TOKEN` / vault `gardenio-main`. We must pick the
**account-of-record** for three surfaces:

1. **CI compute** — who runs (and pays for) the build/test/release jobs.
2. **Container registry** — where the 13 service images land.
3. **Helm chart push target** — where the OCI chart lands.

## What the current pipeline actually depends on (survey)

| Surface | Current target | Parameterized? | Port cost |
|---|---|---|---|
| **CI compute / orchestration** | GitLab CI (`.gitlab-ci.yml`, 1490 lines) dispatches RWX via `rwx dispatch`+`rwx results --wait`. RWX is the executor. Auth: `RWX_ACCESS_TOKEN`, vault `gardenio-main`. | n/a — this is the thing to move | **High** — the budget source |
| **Image build+push** | `docker.yml` crane-pushes 13 services (+cover variants) to `$IMAGE_REGISTRY` (vault `IMAGE_REGISTRY`/`REGISTRY_USER`/`REGISTRY_TOKEN`), default `<registry>/gardenio`, GHCR fallback. `promote.yml` same var. | **Yes** ✅ | **Low** — set vault vars + creds |
| **Helm chart push** | `helm.yml` L317-323 hardcodes `oci://<registry>/gardenio/charts`. | **No** ❌ | **Low** — add `CHART_REGISTRY` var |
| **Mirror gate** | `mirror.yml` crane-copies upstream→`registry.gitlab.com/.../mirror/`. Dispatched by `release.sh`. | partial (`IMAGE_REGISTRY`) | **Low** — likely drop for LOL |
| **Deploy + E2E + smoke** | `deploy.yml`/`deploy-rwx.yml`/`e2e.yml`/`smoke-rwx.yml`: `aws eks update-kubeconfig` against `E2E_EKS_CLUSTER` (us-east-1), ephemeral `gardenio-rwx-*` namespaces, keycloak/forwardauth/litellm/slack secrets. | no — deeply ACME-coupled | **High** — owned by ka-0000000040 (BYO-cluster path) |
| **release.sh** | calver + git, then `rwx run ci.yml` (preflight) + `rwx run mirror.yml`. Pushes to GitLab origin + GitHub mirror → triggers pipeline. | n/a | **Medium** — re-point preflight runner |

**Key finding:** image push is *already* registry-agnostic. The expensive
coupling is (a) the RWX **compute budget** and (b) the **deploy/E2E** EKS
dependency. (b) is explicitly the next bead (ka-0000000040); this artifact
decides (a) + the registry/chart hosts.

**Grounding facts (researched 2026-05-29):**
- **GitHub Actions** is *free and unlimited* on standard runners for **public**
  repos; **GHCR** package storage + bandwidth is free for public packages
  (images *and* Helm OCI charts). Larger runners are always billed.
- **RWX** bills per-second ($0.00014/s, $50 free credit) against the
  token's org. RWX offers **free compute credits to OSS orgs** on request —
  but that's an approval, not an automatic guarantee, and the meter runs by
  default. Billing follows the access-token's org.

---

## Decision 1 — CI compute (account-of-record)

### Option A — Dedicated RWX org for Live Oak Labs *(minimal-port)*
Stand up an `rwx-cloud/liveoaklabs` org, issue its own `RWX_ACCESS_TOKEN` +
`gardenio-lol` vault, request OSS free credits.

- **Pros:** Zero pipeline rewrite — `.rwx/*.yml` already work; just swap the
  token+vault. Keeps RWX's caching/parallelism/DAG that the build is tuned for.
  Per-second billing is cheap; OSS credits likely cover it.
- **Cons:** Still a *paid* platform — if OSS credits lapse or aren't granted,
  LOL's own RWX account is billed (better than Matt's, but not provably $0).
  External contributors can't run RWX on their fork without org access.
  Adds an RWX-org provisioning + OSS-credit-application step.

### Option B — GitHub Actions (public repo) *(provably-zero-bill)* ⭐ RECOMMENDED
Port build/lint/test + image/chart publish to GHA workflows in the public LOL
repo. Standard runners → **$0** for public repos.

- **Pros:** **Provably zero cost** for a public repo — satisfies success
  criterion #2 (billing audit shows $0) by construction, no credit
  application. Fork-friendly: any contributor's PR runs CI free on their fork.
  Native GHCR integration (`GITHUB_TOKEN` pushes images+charts, no extra
  secrets). No third-party billing relationship for LOL to own.
- **Cons:** **Highest port effort** — translate `ci.yml`+`docker.yml`+`helm.yml`
  trigger/DAG/cache semantics to GHA (the `rwx:migrate-from-gha` skill is the
  *reverse* direction; this is GHA-authoring). Lose RWX content-based caching
  (GHA cache is coarser → slower builds). Standard runners are smaller than
  the tuned RWX agents — the multi-arch crane builds may need tuning.

### Option C — Hybrid: GHA control plane + RWX execution
GHA triggers on public-repo events, dispatches RWX for heavy build/test.

- **Pros:** Keeps RWX's fast cached builds; GHA gives the free public trigger
  surface and fork ergonomics.
- **Cons:** **Worst of both for the billing goal** — the heavy compute still
  meters on RWX, so it does NOT provably hit $0; you keep the RWX billing
  relationship AND take on GHA-authoring effort. Two systems to maintain.
  Only makes sense if RWX build speed is non-negotiable.

**Recommendation: Option B (GitHub Actions, public repo).** The stated
mission is a *self-releasing public repo with zero spend on Matt's account*,
audited to $0 (criterion #2) and pullable by externals with no ACME creds
(criterion #3). Only B delivers $0 by construction rather than by credit-grant,
and it's the most contributor-friendly. The cost is real porting effort, but
that effort lands in ka-0000000040 regardless (deploy/E2E must be reworked for
a public env anyway). **Fallback: Option A** if the GHA port proves too costly
for the pilot timeline — a dedicated LOL RWX org with OSS credits still moves
the bill off Matt's personal account, accepting the credit-grant dependency.

---

## Decision 2 — Container registry

### Option A — GHCR (`ghcr.io/liveoaklabs/gardenio/*`) ⭐ RECOMMENDED
- **Pros:** Free for public packages (images). Zero extra auth under GHA
  (`GITHUB_TOKEN`). `docker.yml` already supports it via `IMAGE_REGISTRY` +
  GHCR fallback — near-zero port. Externals pull anonymously, no ACME creds
  (criterion #3). Co-located with the public repo.
- **Cons:** "Currently free" wording in GitHub's billing docs — small risk
  GitHub meters GHCR bandwidth later. Mitigation: pulls via GHA stay free;
  keep a Plan-B mirror documented if a public K8s cluster pulls heavily.

### Option B — RWX-hosted registry
- **Cons:** Ties registry to RWX billing/availability; less standard for public
  consumption; no clear advantage over GHCR for a public repo. Reject.

### Option C — Other LOL-owned (Docker Hub OSS org, self-hosted, ECR public)
- **Docker Hub** OSS org: free public pulls but rate-limited; extra account.
  **ECR Public**: free public pulls (generous egress) but adds an AWS billing
  relationship + creds. Both viable fallbacks; neither beats GHCR's
  zero-extra-auth co-location for a GitHub-hosted public repo.

**Recommendation: Option A (GHCR).** Aligns with the GHA compute choice, free
for public, already supported by `docker.yml`. **Fallback: ECR Public** if
GHCR bandwidth metering becomes a concern at scale.

---

## Decision 3 — Helm chart push target

### Option A — GHCR OCI (`oci://ghcr.io/liveoaklabs/gardenio/charts`) ⭐ RECOMMENDED
- **Pros:** Helm 3 pushes OCI charts to GHCR natively; free for public; same
  registry/creds as images. Port = replace the hardcoded `oci://registry.gitlab.com/...`
  in `helm.yml` L317-323 with a `CHART_REGISTRY` var (small, isolated edit).
- **Cons:** Same "currently free" caveat as Decision 2.

### Option B — GitHub Pages `index.yaml` (classic Helm HTTP repo)
- **Pros:** Most familiar `helm repo add` UX for external users; trivially free
  on Pages.
- **Cons:** Diverges from the current OCI-push shape (bigger rewrite); needs an
  index-maintenance step. Nice-to-have, not for the pilot.

### Option C — Other OCI (Docker Hub / ECR Public OCI)
- Same tradeoffs as Decision 2-C; keep as fallback.

**Recommendation: Option A (GHCR OCI).** One registry for images + charts,
free public, smallest diff to `helm.yml`. Optionally publish a Pages
`index.yaml` later for friendlier `helm repo add` UX (file as follow-up).

---

## Consolidated recommendation

| Surface | Pick | Fallback |
|---|---|---|
| CI compute | **GitHub Actions (public repo)** | Dedicated LOL RWX org + OSS credits |
| Container registry | **GHCR public** (`ghcr.io/liveoaklabs/...`) | ECR Public |
| Helm chart push | **GHCR OCI** (`oci://ghcr.io/liveoaklabs/.../charts`) | GitHub Pages `index.yaml` |

**Why this set:** it is the only combination that makes success criterion #2
($0 on Matt's account, billing-audited) true *by construction* rather than by a
revocable OSS-credit grant, while keeping criteria #3 (externally pullable, no
ACME creds) and the existing build→docker→helm→release shape intact. The image
layer is already portable; the real work — and the real risk — is the GHA
authoring of `ci.yml`/`docker.yml`/`helm.yml` and the deploy/E2E rework, both of
which are scoped to the downstream port bead **ka-0000000040**.

## Scope handed to ka-0000000040 (the port) once this is approved
1. Author GHA workflows for build/lint/test + multi-arch image build → GHCR + chart push → GHCR OCI (or migrate `.rwx/*.yml` semantics).
2. Parameterize the hardcoded chart registry in `helm.yml` (and drop/repoint `mirror.yml`).
3. Rework deploy/E2E/smoke for a public-friendly **BYO-cluster** path (the deep EKS coupling).
4. Rewrite `release.sh` preflight to call GHA (or `gh workflow run`) instead of `rwx run`.
5. Document the secrets surface an external operator must provide.
6. First self-release dry-run + billing audit confirming $0 on Matt's RWX account.

## Open question for the operator
Does Live Oak Labs already have (a) a GitHub org for the public repo, and
(b) any existing RWX org? The answer decides whether Option B is a clean
greenfield GHA setup or whether an existing RWX org makes Option A's
"minimal-port" path attractive enough to accept the credit-grant dependency.
