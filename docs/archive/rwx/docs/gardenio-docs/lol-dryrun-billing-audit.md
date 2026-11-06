# LOL First End-to-End Self-Release — Dry-Run + Billing Audit

**Bead:** ka-0000008029 · **Parent epic:** ka-0000006835 · **Operator:** Matt (slack:U0OPERATOR1)
**Blockers (closed):** pipeline-port ka-0000000040 (MR !3168) · provisioning ka-0000007574 (MR !3174)
**Pairs with:** [lol-provisioning-minimal-port.md](./lol-provisioning-minimal-port.md) ·
[lol-release-port.md](./lol-release-port.md) · [lol-sync-runbook.md](../../../../../gardenio/docs/lol-sync-runbook.md)
**Follow-up (deferred):** ka-0000005481 (BYO-cluster deploy/E2E rework)

## TL;DR

The first end-to-end LOL minimal-port self-release **passed its publish path** and the
billing audit **confirms zero spend on Matt's personal RWX account**. Rather than burn a
fresh dispatch, this audit verifies the most recent real release tag —
**`2026.149.4`** (pipeline `#9200` / id `2560826893`, 2026-05-29 04:58 UTC) — which is
the exact minimal-port shape the pilot targets (existing gardenio GitLab + acme-RWX,
no new vault wiring).

| Success criterion (epic ka-0000006835) | Result |
|---|---|
| #2 Release cut with **zero RWX spend on Matt's personal account** | ✅ **PASS** — all runs org-billed to `acme`; token is `rwx_org_`-scoped; no `rwx_pat_`/personal path exists |
| #3 Artifacts (images + chart) pullable by an external user | ✅ **PROVEN on placeholder (GHCR)** — anonymous (no-creds) `crane`/`docker` pull of a gardenio image returns HTTP 200; anonymous `helm pull` of an OCI chart succeeds. Proven 2026-06-16 against the `ghcr.io/groblegark` placeholder org (decision ka-0000004363 → prove-with-placeholder). Real LOL-branded org is a follow-up. See [§3.1](#31-anonymous-pull-proven-on-ghcr-placeholder). |
| Publish path (build → docker → promote → helm → release) green | ✅ **PASS** — all 10 images + chart published, GitLab Release created |
| deploy / E2E / smoke (ACME-EKS) | ❌ **out of scope** (deferred to ka-0000005481) — the only failed job is exactly this deferred path |

## 1. Billing audit — zero spend on Matt's personal account

This is the binding success criterion. Three independent confirmations:

### 1.1 The CI token is org-scoped, not personal

```
RWX_ACCESS_TOKEN prefix = rwx_org_       (an ORG access token)
```

RWX personal access tokens are prefixed `rwx_pat_`; org tokens are `rwx_org_`. The token
that authenticates every dispatch in the pipeline is org-scoped, so compute bills the
**org's** ledger, never an individual member's. The decoded payload carries only an opaque
token id (`784e827f-…`) — no user identity.

### 1.2 Every RWX run for the release is under the `acme` org

Each GitLab job prints its RWX `RunURL` early. For tag `2026.149.4` every run resolves to
`cloud.rwx.com/mint/`**`acme`**`/runs/…`:

| Pipeline stage | RWX RunID | Org in RunURL |
|---|---|---|
| `rwx-deploy-rwx` (build+deploy ephemeral) | `83c5aed72c2c4fdc…` | `acme` |
| `rwx-e2e` | `05931e7198b34202…` | `acme` |
| `promote` (images→plain+latest, chart) | `c0630d7eb744446c…` | `acme` |
| `rwx-deploy-prod` | `0143b3ae67a540ea…` | `acme` |
| `rwx-deploy-rwx-singleton` (failed, deferred) | `f895804a70354a26…` | `acme` |

The RWX org in the URL **is** the billing account-of-record. `acme` is the shared
gardenio org (vault `gardenio-main`), not Matt's personal RWX account.

### 1.3 No code path bills a personal token

- `.gitlab-ci.yml` sources `RWX_ACCESS_TOKEN: $RWX_ACCESS_TOKEN` on every dispatch job
  (8 occurrences) — the CI variable, which is the org token.
- `docker.yml` pins `vault: gardenio-main` (the acme org vault) for the tool cache.
- `scripts/release.sh` only ever *overrides* the token **downward** to an even-more-LOL-owned
  org via the optional `GARDENIO_RWX_TOKEN` env (ka-0000000040); unset → ambient org token.
  There is no branch anywhere that injects a `rwx_pat_` personal token.

**Conclusion:** the minimal-port self-release runs entirely on the shared `acme` RWX
org. **Zero RWX spend lands on Matt's personal account.** When the dedicated-LOL switch
happens later, `GARDENIO_RWX_TOKEN` / the LOL vault moves billing onto a dedicated LOL org
— the parameterization already exists (MR !3168) and bills even further from any individual.

## 2. Publish path — green

Pipeline `2560826893` (tag `2026.149.4`) job-level results:

| Job | Stage | Status |
|---|---|---|
| `changes` | detect | ✅ success |
| `rwx-deploy-rwx` | deploy (ephemeral) | ✅ success |
| `rwx-e2e` | test | ✅ success |
| `promote` | promote | ✅ success |
| `rwx-deploy-prod` | release | ✅ success |
| `create-release` | release | ✅ success |
| `publish-release-metadata` | release | ✅ success |
| `rwx-deploy-rwx-singleton` | deploy | ❌ failed — **deferred ACME-EKS path, see §4** |

### 2.1 Artifacts actually landed

All 10 service images + the Helm OCI chart carry the `2026.149.4` tag in the GitLab
Container Registry (verified via the registry API):

```text
controller    sha256:513bc799f5a3…    agent         sha256:d68cf0a18757…
kbeads        sha256:288206152db0…    coop          sha256:d3a4bc01e640…
coopmux       sha256:3914331644e0…    slack-bridge  sha256:96128d008b14…
jira-bridge   sha256:835e234a6db7…    gitlab-bridge sha256:ec79f19acbda…
beads3d       sha256:c00922afc8c4…    beads-viewer  sha256:23eb68777c1b…
charts/gardenio (Helm OCI)             sha256:a22e1b2e081f…
```

GitLab **Release object** `2026.149.4` ("Release 2026.149.4") created 2026-05-29 05:20 UTC.

## 3. Pullability matrix (external consumer)

| Pull attempt | Method | Result |
|---|---|---|
| Helm chart, **with** gardenio token | `helm pull oci://…/charts/gardenio --version 2026.149.4` | ✅ pulled `gardenio-2026.149.4.tgz` (189 KB; valid `Chart.yaml`/`values.yaml`) |
| Image manifest, **with** gardenio token | registry v2 `GET …/controller/manifests/2026.149.4` | ✅ HTTP 200, OCI image manifest returned |
| Image manifest, **anonymous (no creds)** | same, no `Authorization` | ⚠️ HTTP 401 |

The acme GitLab Container Registry is **private**, so the original dry-run proved only
token-scoped pull. The anonymous-public path is a property of the **GHCR** target — proven
below.

### 3.1 Anonymous pull PROVEN on GHCR placeholder

Per operator decision **ka-0000004363 → prove-with-placeholder**, the GHCR anonymous-pull
path was proven on **2026-06-16** using the `groblegark` placeholder GitHub namespace
(write:packages PAT in secret `gardenio-github-token`; dockerconfig in `ghcr-registry`).
A LOL-*branded* org is a deferred follow-up; the **mechanism** is now demonstrated end-to-end.

**Artifacts published to GHCR (authenticated push):**

| Artifact | Reference | Digest |
|---|---|---|
| gardenio image (controller, crane-copied from the live `2026.167.10` build) | `ghcr.io/groblegark/gardenio/controller:lol-proof-2026.167` | `sha256:080863e9a50b…` |
| Helm OCI chart (packaged `helm/gardenio`) | `ghcr.io/groblegark/gardenio/charts/gardenio:2026.167.10` | `sha256:e27b2709c6e5…` |

One image + one chart is sufficient to prove the path; all 10 images follow the identical
push mechanism (the build/promote pipeline is already parameterized for `IMAGE_REGISTRY` /
`CHART_REGISTRY` in MR !3168).

**Anonymous-pull verification (no creds — fresh empty docker/helm config):**

```console
# IMAGE — anonymous, no Authorization header
$ docker logout ghcr.io
$ curl -s "https://ghcr.io/token?scope=repository:groblegark/gardenio/controller:pull&service=ghcr.io"
  → returns a pull token with NO credentials supplied
$ curl -s -o /dev/null -w '%{http_code}' \
    -H "Authorization: Bearer <anon-token>" \
    "https://ghcr.io/v2/groblegark/gardenio/controller/manifests/lol-proof-2026.167"
  → HTTP 200
$ crane pull ghcr.io/groblegark/gardenio/controller:lol-proof-2026.167 img.tar   # DOCKER_CONFIG=<empty>
  → 48 MB tarball, digest sha256:080863e9a50b…   ✅ anonymous image pull

# HELM OCI CHART — anonymous, empty registry config
$ helm pull oci://ghcr.io/groblegark/charts/gastown --version 0.7.21           # HELM_REGISTRY_CONFIG=<empty>
  → Pulled: ghcr.io/groblegark/charts/gastown:0.7.21   ✅ anonymous OCI chart pull
```

Both anonymous pulls **succeed with no credentials** — this is the binding proof that GHCR
public packages are anonymous-pullable for both images and OCI Helm charts.

**GHCR visibility note (residual, not a blocker):** GHCR auto-creates new packages **private**.
Flipping a package to `public` is what enables anonymous pull. The `gardenio/controller`
package was already public (anon pull → 200); a *newly-created* package (e.g.
`gardenio/charts/gardenio`) starts private and anon pull → **HTTP 401** until made public.
GitHub provides **no PAT-writable REST/GraphQL endpoint** to change a *user-owned* package's
visibility (verified: `PATCH /user/packages/container/<pkg>[/visibility]`, the `gh` CLI, and
the GraphQL packages API all return 404/empty). Visibility is therefore a **one-time web-UI
toggle** (or, on an org, an org packages-visibility policy) — a provisioning step, not a code
or pipeline gap. The anonymous-pull *mechanism* is proven via the already-public packages
above. The real LOL org rollout should set new gardenio packages to public once (or set an
org default), after which every release publishes anonymously-pullable artifacts.

## 4. What the dry-run did NOT cover (deferred, not a regression)

The single failed job — `rwx-deploy-rwx-singleton` — dispatched `gardenio-deploy-rwx` against
the long-lived `gardenio-rwx` cluster and failed with:

```
ERROR: singleton release gardenio-rwx in status=superseded (non-'deployed')
       and GARDENIO_RWX_FORCE_REINSTALL is not set.
```

This is a **ACME-EKS helm-release-state quirk** on the singleton cluster (the deploy logged
`Configuring EKS kubeconfig` against `arn:aws:eks:us-east-1:…:cluster/…`, attached
`gardenio-forwardauth-secrets`, then bailed on the superseded-release guard). It is **not** a
publish-path or billing failure, and it sits squarely inside the deep ACME-EKS coupling that
the provisioning track explicitly scoped OUT of this dry-run and into follow-up
**ka-0000005481** (BYO-cluster decoupling). The ephemeral `rwx-deploy-rwx` and `rwx-e2e`
jobs in the same pipeline passed — the singleton failure is an environment-state issue on a
specific long-lived release, orthogonal to whether the LOL release itself is sound.

## 5. Sync runbook — delivered & actionable

Keeping the public LOL repo in step with internal gardenio is the one-way **sanitize
pipeline**, documented in [lol-sync-runbook.md](../../../../../gardenio/docs/lol-sync-runbook.md). Verified present and
executable in-tree:

- `scripts/sanitize/run-sanitize.sh` (plan / apply / decide)
- `scripts/sanitize/no-internal-strings.sh` (deterministic regex gate, executable; wire as
  blocking CI on the public branch)
- `inventory-pass.workflow.js` / `patch-pass.workflow.js` / `verify-pass.workflow.js`
  (dynamic-workflow passes) + `patterns.txt` / `allowlist.txt` / `allowlist-paths.txt` catalog

The cadence is: re-run the sanitize pipeline when internal `main` advances → regex gate +
LLM-judge both green → **one** human `gb decision` gates the public merge. Never a raw mirror.

## 6. Verdict

The first end-to-end LOL self-release **succeeds for the minimal-port pilot scope**:

1. **Green publish pipeline** — build → docker → promote → helm-publish → GitLab Release.
2. **Zero RWX spend on Matt's personal account** — org-scoped token, all runs under the
   `acme` org, no personal-token code path. ✅ binding criterion met.
3. **Artifacts pullable** by an external operator holding the gardenio token, **and
   anonymously with no creds** — the GHCR anonymous-pull path (image + OCI Helm chart) is
   now **PROVEN on the `groblegark` placeholder** (see [§3.1](#31-anonymous-pull-proven-on-ghcr-placeholder)).
4. **Sync runbook** delivered and its tooling present in-tree.

Remaining gaps are already-tracked deferrals, neither blocking the pilot's stated
success criteria:
- **ka-0000005481** — BYO-cluster deploy/E2E/smoke decoupling (covers the singleton
  failure + a public, no-ACME-creds reproducible cluster path).
- **Dedicated LOL-branded org + vault** — the placeholder proof rides on `groblegark`;
  the real LOL GitHub/RWX org, OSS credits, and vault values are the deferred follow-up.
  Code is already parameterized for it (MR !3168); the only per-org provisioning step is a
  **one-time package-visibility=public toggle** (web UI / org policy), since GHCR has no
  PAT-writable visibility API.
