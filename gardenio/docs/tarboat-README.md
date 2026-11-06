# Gardenio Tarboat

**A self-contained, air-gapped Gardenio distribution — one tarball, one EC2, no internet.**

Tarboat ("gardenio in a tar") packs everything a single Gardenio box needs into one
signed `gardenio-tarboat-<calver>.tar.zst`. Untar it on a fresh Linux EC2 instance
and bring up a complete Gardenio install with **no public-internet fetch** and **no
private-repo clone**. This is the artifact we ship to friends.

> Built by `.rwx/bundle.yml` (`build-tarboat` task) → `scripts/tarboat/assemble-tarboat.sh`.
> Operator: Matt Baker. Plan: bead ka-0000003281. Gap analysis: `tarboat-gap-analysis.md`.

---

## What you need

- A fresh single EC2 instance (an `r7i.2xlarge` or larger is a comfortable
  starting point — Gardenio runs k3s + a full controller/agent stack + an in-box
  registry + object store).
- Root (`sudo`) on the box.
- The `gardenio-tarboat-<calver>.tar.zst` file and its `.sha256` sidecar.

That's it. No GitLab access, no Docker Hub pulls, no `helm repo add`, no
`get.k3s.io`. Everything is in the tarball.

## What's inside

| Path | What |
|---|---|
| `bin/` | `k3s`, `k3s-install.sh`, `helm`, `crane`, `zot` — pinned, checksummed |
| `manifests/` | `cert-manager.yaml`, `servicemonitor-crd.yaml` (pinned versions) |
| `charts/` | `keycloak-operator-<ver>.tgz` (helm-pulled, installs from local path) |
| `images/` | every runtime container image as one OCI archive (reused from the release image bundle) |
| `k3s-airgap-images-<arch>.tar.zst` | k3s system images, so k3s itself is offline too |
| `repo/` | the gardenio tree (`scripts/` + `gardenio/helm/gardenio/` + `deploy-values/` + install docs) — no clone needed |
| `LICENSES/` | component + bundled-image licenses, incl. the MinIO **AGPL-v3** `NOTICE` |
| `MANIFEST` | every file + its sha256 + provenance (calver, commit, build run) |
| `SHA256SUMS` | top-level checksum — verify with `sha256sum -c SHA256SUMS` |
| `README.md` | this page |
| `SETUP-PROMPT.txt` | the single on-box-agent setup prompt (filled in by Tarboat T2) |

## Install (true air-gap)

```sh
# 1. Verify what you received.
sha256sum -c gardenio-tarboat-<calver>.tar.zst.sha256

# 2. Unpack.
zstd -d gardenio-tarboat-<calver>.tar.zst
tar -xf gardenio-tarboat-<calver>.tar
cd gardenio-tarboat-<calver>

# 3. Verify the inner tree, then install.
sha256sum -c SHA256SUMS
sudo repo/scripts/bootstrap-ec2.sh \
  --scenario A --harden \
  --airgap --airgap-strict \
  --offline-assets "$PWD" \
  --bundle-dir "$PWD/images" \
  --repo-dir "$PWD/repo"
```

- `--offline-assets "$PWD"` points the installer at the staged `bin/`,
  `manifests/`, and `charts/` — it consumes the staged copies and never reaches
  the public internet.
- `--airgap-strict` makes a missing staged asset a **hard fail** instead of a
  silent network fallback — this is what makes the air-gap real.
- `--bundle-dir "$PWD/images"` seeds the in-box registry from the bundled images.
- `--repo-dir "$PWD/repo"` uses the carried gardenio tree, so there is no clone.

## Integrity & signing

- `MANIFEST` lists every file with its sha256 and the build provenance.
- `SHA256SUMS` is the top-level integrity floor (`sha256sum -c`).
- The artifact ships with a `.sha256` sidecar.
- When the release is signed, a detached `gardenio-tarboat-<calver>.tar.zst.asc`
  is published alongside it; verify with `gpg --verify`. If no `.asc` is present,
  the sha256 + `MANIFEST` + `SHA256SUMS` are the integrity floor.

## Licensing

This tarball is **redistributable** and bundles third-party software. The
notable obligation is **MinIO (AGPL-v3)** — the in-box object store. See
`LICENSES/NOTICE` and `LICENSES/AGPL-3.0.txt`. SeaweedFS is the documented
fallback if AGPL is unacceptable for your distribution.

## Getting help

This is an early "for friends" distribution. The full architecture and runbooks
live under `repo/gardenio/docs/` — start with `gardenio-in-a-box-install.md`.
