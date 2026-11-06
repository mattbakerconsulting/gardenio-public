# LOL Release-Pipeline Port — Code-Level Parameterization

**Bead:** ka-0000000040 · **Parent epic:** ka-0000006835
**Decision of record:** [lol-cibilling-target-decision.md](./lol-cibilling-target-decision.md)
(operator-resolved `rwx-org` = dedicated Live Oak Labs RWX org on OSS credits,
**minimal port** — `.rwx/*.yml` stay nearly as-is).

This document covers the **code-level track**: the registry/token
parameterization that can land without any provisioning. The
**provisioning track** (LOL RWX org + token, OSS-credit application, and the
BYO-cluster deploy/E2E rework) is operator-gated and tracked separately.

## What changed

| File | Change |
|---|---|
| `.rwx/helm.yml` | `helm-login` derives the registry host from a new `CHART_REGISTRY` vault var; `helm-publish` pushes/echoes via `oci://${CHART_REGISTRY}` instead of the hardcoded GitLab OCI path. |
| `.rwx/promote.yml` | `promote-helm-chart` uses the same `CHART_REGISTRY` var for login, push, and the post-publish verification pull. |
| `scripts/release.sh` | New optional `GARDENIO_RWX_TOKEN` env var: when set, it is exported as `RWX_ACCESS_TOKEN` for the preflight + mirror-gate `rwx run` dispatches, so those runs bill a LOL-owned RWX org. |

No image-build code changed: `docker.yml` and `promote.yml`'s `promote-images`
task **already** honor `IMAGE_REGISTRY` / `REGISTRY_USER` / `REGISTRY_TOKEN`
(with `${:-default}` fallbacks and a GHCR `GHCR_TOKEN` / `GITHUB_USER`
fallback). RWX resolves an unset secret to an empty string, so adding a new
optional secret reference is non-breaking for the existing GitLab pipeline.

## The `CHART_REGISTRY` variable

- **Shape:** bare `host/path`, **no** `oci://` prefix — identical convention to
  `IMAGE_REGISTRY`. The host for `helm registry login` is derived with
  `${CHART_REGISTRY%%/*}`; the push target is `oci://${CHART_REGISTRY}`.
- **Default (unchanged behaviour):**
  `<registry>/gardenio/charts`
- **LOL target (recommended):** `ghcr.io/liveoaklabs/gardenio/charts`

Set it as an RWX vault secret named `CHART_REGISTRY`. Leave it unset and the
pipeline behaves exactly as before.

## Secrets surface an external / LOL operator must provide

For a self-releasing public build the RWX vault needs:

| Secret | Purpose | LOL value |
|---|---|---|
| `RWX_ACCESS_TOKEN` (or `GARDENIO_RWX_TOKEN` for `release.sh`) | CI compute / dispatch auth | LOL RWX org token |
| `IMAGE_REGISTRY` | image push target | `ghcr.io/liveoaklabs/gardenio` |
| `CHART_REGISTRY` | helm chart push target | `ghcr.io/liveoaklabs/gardenio/charts` |
| `REGISTRY_USER` / `REGISTRY_TOKEN` | registry auth (images + charts) | GitHub user + a GHCR-scoped PAT (`write:packages`) |

GHCR accepts the same credentials for images and OCI charts, so one
`REGISTRY_USER`/`REGISTRY_TOKEN` pair covers both `IMAGE_REGISTRY` and
`CHART_REGISTRY` when both point at `ghcr.io/liveoaklabs/...`.

## Still gated on provisioning (NOT in this change)

1. **LOL RWX org + `RWX_ACCESS_TOKEN` + `gardenio-lol` vault** + OSS-credit
   application. Needs the operator answer: *does LOL already have a GitHub org
   and/or an existing RWX org?*
2. **`mirror.yml`** — already `IMAGE_REGISTRY`-parameterized (`${IMAGE_REGISTRY:-…}/mirror`).
   A full **drop** is coupled to the deploy path (the mirror exists to avoid
   Docker Hub 429s at in-cluster deploy time), so it is deferred to the
   deploy/E2E rework rather than removed here.
3. **deploy / E2E / smoke** (`deploy*.yml`, `e2e.yml`, `smoke-rwx.yml`) — deep
   ACME-EKS coupling; the public-friendly BYO-cluster path is the deepest item
   and is handled on the provisioning track.

## Verifying the parameterization

```sh
# Default path (GitLab) — unchanged:
rwx run .rwx/helm.yml --init commit-sha=$(git rev-parse HEAD) --target helm-lint --wait

# LOL path — set the vault vars CHART_REGISTRY=ghcr.io/liveoaklabs/gardenio/charts,
# IMAGE_REGISTRY=ghcr.io/liveoaklabs/gardenio, REGISTRY_USER/REGISTRY_TOKEN to a
# GHCR PAT, then dispatch the tag pipeline against the LOL RWX org.
```
