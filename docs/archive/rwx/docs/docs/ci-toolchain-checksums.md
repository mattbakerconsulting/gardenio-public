# CI toolchain checksums

SHA256 digests for the pinned binaries the CI pipeline downloads at runtime
and executes. This pairs the *version pin* (already present at each call site)
with a *byte pin*, so a compromised upstream release or a MITM'd download is
rejected before it runs inside a CI job holding `RWX_ACCESS_TOKEN` (the org
RWX master key), registry, and AWS credentials.

Threat model: **A6 — supply chain** (audit `ka-0000004158`, finding **F3**
`ka-0000001204`). Attacker who can serve altered bytes for a pinned release URL
gains code execution as the CI job. Version-only pinning does not stop this:
the tag can be re-pointed, or the bytes swapped in transit; only a content hash
does.

This is the same pattern the agent image already uses
(`gardenio/images/agent/install/checksums.env` + `sha256sum -c`), extended to the
CI-runtime tool downloads that live in the pipeline YAML rather than the image.

## Pinned digests

### RWX CLI (`rwx-linux-x86_64-static`)

Highest blast radius: every job that installs the RWX CLI can dispatch any RWX
workflow (including `deploy-prod` / `teardown`) and read every run log.

| Version  | SHA256                                                             |
|----------|-------------------------------------------------------------------|
| v3.13.2  | `ef4afcad5ed9a6e7d24190050753f35cf1a6d609f07b949a1caa83074c16507e` |
| v3.20.0  | `a99c0ade62489a8db7e2db76e8901784cf631432449827e6021a5f3858eaa4a6` |

Call sites (all verify with `sha256sum -c` against the digest above):

- `.gitlab-ci.yml` — `.rwx-cli-setup` anchor (v3.13.2)
- `.gitlab-ci.yml` — `rwx-list` E2E-run lookup binary (v3.20.0); a fetch **or**
  checksum failure degrades the coverage job to unit-only rather than executing
  an unverified binary.
- `.rwx/e2e.yml`, `.rwx/coverage.yml`, `.rwx/coverage-patch.yml`,
  `.rwx/coverage-merge.yml`, `.rwx/release.yml` (all v3.13.2)

## Updating a digest

When you bump a pinned version, update the byte pin in the **same** MR:

```sh
VER=v3.13.2
curl -fsSL "https://github.com/rwx-cloud/rwx/releases/download/${VER}/rwx-linux-x86_64-static" -o /tmp/rwx
sha256sum /tmp/rwx     # copy the digest into every call site + the table above
```

RWX does not publish a `SHA256SUMS` asset next to its releases, so this is
trust-on-first-use (TOFU): the digest is captured once from the release and
frozen. Re-download from a second network path if you want extra assurance —
the digest is stable across fetches. Keep the table above and every inline
`RWX_SHA256=` / `RWX_LIST_SHA256=` in sync (grep `rwx-linux-x86_64-static`).

## Not covered here (residual, tracked separately)

Some CI downloads are **unversioned moving targets** — they resolve `HEAD` /
`main` upstream, so a static content hash would break on every upstream commit
and cannot be pinned as-is:

- `helm/scripts/get-helm-3 | bash` (`.rwx/helm.yml`, `.rwx/deploy*.yml`,
  `.rwx/promote.yml`, `.rwx/teardown-rwx.yml`, `.rwx/ci.yml`)
- `golangci/golangci-lint .../HEAD/install.sh | sh` (`.rwx/ci.yml`)
- `sh.rustup.rs | sh` (`.rwx/ci.yml`, `.rwx/docker.yml`)

Hardening those requires first pinning the installer script to a tagged commit
(so it *has* a stable hash), which is a larger change with its own upgrade
cadence. Tracked as residual on F3 (`ka-0000001204`) / the CI/CD hardening
backlog `ka-0000005893`.
