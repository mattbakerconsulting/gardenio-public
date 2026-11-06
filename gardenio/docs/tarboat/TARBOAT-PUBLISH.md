# Tarboat Publish — Manual Runbook + Release-Pipeline Automation Plan

> **Tracking bead:** ka-0000007039 · **Operator:** Matt Baker (oidc:matthew.baker@acme.example)
> **Date:** 2026-06-10
>
> **The ask (Matt Baker, Slack):** *"let's get a distributable private bundle into the
> groblegark gitlab; we will need to keep it up to date manually until we have the Gitlab
> gardenio release process generating it (TODO: plan that)."*
>
> The "distributable private bundle" is **tarboat** — the signed, air-gapped one-tarball
> install `gardenio-tarboat-<calver>.tar.zst` (see [`../tarboat-README.md`](../tarboat-README.md)).
> Today it is **built** (RWX `bundle.yml` → `scripts/tarboat/assemble-tarboat.sh`) but only as
> an **ephemeral RWX artifact** — nothing publishes it to a durable, distributable home.
> This doc is (1) the **manual publish runbook** for the interim and (2) the **plan** for
> wiring publish into the release pipeline so it happens automatically.

---

## 0. Where things stand today

| Piece | Status | Where |
|---|---|---|
| Image bundle (multi-GB OCI archive) | built **on-demand** at release | `.rwx/bundle.yml` `build-bundle`; GitLab CI job `bundle-airgap` (`when: manual`) |
| Tarball assembly | built **right after** the image bundle | `.rwx/bundle.yml` `build-tarboat` → `scripts/tarboat/assemble-tarboat.sh` |
| Tarball output | RWX artifact `gardenio-tarboat` (ephemeral) | `path: /tmp/tarboat/out` |
| **Publish to a durable home** | **MISSING** | — this doc |

Key facts that shape the plan:

- **`bundle-airgap` is `when: manual` + `allow_failure: true`.** The air-gap bundle is
  intentionally *not* part of every cloud release — an operator clicks it on a tag pipeline
  when an air-gap artifact is wanted. So the tarball only exists when someone asks for it.
- **`build-tarboat` runs inside the same RWX run as `build-bundle`** (`use: [code, build-bundle]`),
  so it reuses the already-built image archive — it does **not** rebuild images.
- **The tarball is multi-GB** (the image OCI archive dominates). This rules out committing it
  into a git repo or git-LFS as the primary channel. **GitLab Generic Package Registry** or a
  **GitLab Release asset** is the right shape. (The gardenio project already publishes generic
  packages today — e.g. `release-health` — so the mechanism is proven in this org.)

---

## 1. OPEN QUESTION — the destination (blocks the actual push)

`groblegark` resolves for the bot token as a **GitHub org** (mirrors: `groblegark/gardenio`,
`kbeads`, `coop`). I do **not** see a `groblegark` **namespace/group on GitLab** with the
gardenio bot token (it is scoped to `example-org/gardenio`). Before the first manual push
we need, from the operator:

1. **The exact GitLab destination** — project path under a `groblegark` group
   (e.g. `groblegark/gardenio-tarboat`?), and whether the bot account gets **Maintainer/Owner**
   there (Generic Package upload + Release-asset create both require an authenticated token with
   write access to that project).
2. **Distribution shape** — confirm **Generic Package Registry** (recommended) vs. **Release
   asset**. Recommendation below assumes Generic Package, with the Release tag carrying a link.

Everything below is written so that once (1)+(2) are answered, only the project path and token
need filling in.

---

## 2. Manual publish runbook (interim — "keep it up to date manually")

### 2.1 Build the tarball for a released calver

The tarball is built by an **RWX dispatch** of `gardenio-bundle` against the release tag. This is
exactly what the `bundle-airgap` GitLab CI job does; you can click that job on the tag pipeline,
or dispatch directly:

```sh
# Pick the calver you want to ship (must be a PROMOTED release tag — bare calver, never -rc).
CALVER=2026.161.20

# Dispatch the bundle workflow against that tag. This builds BOTH the image bundle
# (build-bundle) AND the tarball (build-tarboat) in one run.
rwx dispatch gardenio-bundle --ref "$CALVER" --param version="$CALVER"

# Watch it (RunID is printed on dispatch). build-tarboat is the long pole (~30-40m;
# multi-GB image archive). The tarball lands in the `gardenio-tarboat` artifact.
rwx results <RunID> -w
```

Download the `gardenio-tarboat` artifact from the run (RWX UI → run → artifacts, or
`rwx` artifact download if available). You want both files:

```
gardenio-tarboat-<calver>.tar.zst
gardenio-tarboat-<calver>.tar.zst.sha256
```

### 2.2 Verify before publishing

```sh
sha256sum -c "gardenio-tarboat-${CALVER}.tar.zst.sha256"
```

Do **not** publish a tarball whose sha256 does not verify.

### 2.3 Upload to the GitLab Generic Package Registry  *(fill in PROJECT once §1 is answered)*

```sh
# PROJECT = URL-encoded project path of the groblegark GitLab project, e.g.
#   groblegark%2Fgardenio-tarboat
PROJECT="<urlencoded-project-path>"
CALVER=2026.161.20
TOKEN="$GROBLEGARK_GITLAB_TOKEN"     # write-scoped token for the destination project

for f in "gardenio-tarboat-${CALVER}.tar.zst" "gardenio-tarboat-${CALVER}.tar.zst.sha256"; do
  curl --fail-with-body --header "PRIVATE-TOKEN: ${TOKEN}" \
    --upload-file "$f" \
    "https://gitlab.com/api/v4/projects/${PROJECT}/packages/generic/gardenio-tarboat/${CALVER}/${f}"
done
```

This makes the tarball downloadable at:

```
https://gitlab.com/api/v4/projects/${PROJECT}/packages/generic/gardenio-tarboat/${CALVER}/gardenio-tarboat-${CALVER}.tar.zst
```

### 2.4 (Optional) tie it to a GitLab Release for human discoverability

If the destination project also cuts Releases, attach the package as a release link so friends
have one page to land on:

```sh
glab api --method POST "projects/${PROJECT}/releases" \
  -f tag_name="${CALVER}" \
  -f name="Tarboat ${CALVER}" \
  -f description="Air-gapped one-tarball gardenio distribution. Verify with the .sha256 sidecar; see repo/gardenio/docs/tarboat/TARBOAT-SETUP.md." \
  -f "assets[links][][name]=gardenio-tarboat-${CALVER}.tar.zst" \
  -f "assets[links][][url]=https://gitlab.com/api/v4/projects/${PROJECT}/packages/generic/gardenio-tarboat/${CALVER}/gardenio-tarboat-${CALVER}.tar.zst"
```

### 2.5 Record provenance

Comment the published URL + sha256 on the release/tracking bead so there is an audit trail of
which calver is live for friends.

---

## 3. Automation plan (the "TODO: plan that")

Goal: when a release is cut, the tarball is built **and published** to the groblegark GitLab
destination with no manual step — while preserving the current "air-gap bundle is opt-in, never
gates prod" property.

### 3.1 Design constraints (do not regress)

- **Must not gate prod promotion/release.** Today `bundle-airgap` is `allow_failure: true` and
  `when: manual`. Publishing is heavier still (network egress to a second GitLab) — it must stay
  off the prod-deploy critical path.
- **Must bundle PROMOTED bare-calver images, never `-rc`.** `bundle.yml` is already gated
  `needs: promote`. The publish step inherits that.
- **Secrets:** the destination write token must live in an RWX vault secret (or GitLab CI
  variable), not in the repo. Referencing an undefined secret fails the whole RWX run, so the
  publish task must be guarded to no-op when the token is absent (same pattern as the optional
  GPG signing knob already in `build-tarboat`).

### 3.2 Recommended shape — a `publish-tarboat` task in `bundle.yml`

Add a task gated after `build-tarboat` that uploads to the destination Generic Package Registry:

```yaml
  - key: publish-tarboat
    use: [build-tarboat]
    if: ${{ init.version != '' }}          # only on a real release dispatch
    run: |
      set -e
      [ -n "${GROBLEGARK_GITLAB_TOKEN:-}" ] || { echo "no publish token — skipping"; exit 0; }
      CALVER="${VERSION}"
      OUT="${TARBOAT_OUT}"
      for f in "gardenio-tarboat-${CALVER}.tar.zst" "gardenio-tarboat-${CALVER}.tar.zst.sha256"; do
        curl --fail-with-body --header "PRIVATE-TOKEN: ${GROBLEGARK_GITLAB_TOKEN}" \
          --upload-file "${OUT}/${f}" \
          "https://gitlab.com/api/v4/projects/${GROBLEGARK_PROJECT}/packages/generic/gardenio-tarboat/${CALVER}/${f}"
      done
    env:
      TARBOAT_OUT: ${{ tasks.build-tarboat.artifacts.gardenio-tarboat }}
      VERSION: ${{ init.version }}
      GROBLEGARK_PROJECT: <urlencoded-project-path>
      # GROBLEGARK_GITLAB_TOKEN: ${{ secrets.GROBLEGARK_GITLAB_TOKEN }}  # add when vault secret exists
```

The token-absent guard means: until the vault secret is wired, the task is a harmless no-op and
the manual runbook (§2) stays the path of record — exactly matching Matt's "manual until the
pipeline does it."

### 3.3 Trigger policy — two options for the operator to pick

- **Option A (opt-in, lowest risk):** keep `bundle-airgap` `when: manual`. The operator clicks it
  on the tag pipeline when they want a friend-shippable tarball; `publish-tarboat` then runs
  inside that same dispatch. Zero change to default release behavior. **Recommended to start.**
- **Option B (every release):** flip `bundle-airgap` to `when: on_success` for tag pipelines
  (still `allow_failure: true`, still `needs: promote`). Every release auto-builds + publishes a
  tarball. Costs ~30-40m of RWX time + multi-GB egress per release. Choose only once the path is
  proven and we want every calver shippable.

### 3.4 Open items the automation depends on

1. Destination project path + a **write-scoped token** stored as an RWX vault secret
   (`GROBLEGARK_GITLAB_TOKEN`) — same blocker as §1.
2. Decide A vs. B (§3.3).
3. GPG signing (wired ON by default, ka-0000008173): `build-tarboat` references
   `TARBOAT_GPG_KEYID` + `TARBOAT_SIGNING_KEY` and imports the private key before assembly, so a
   detached `.asc` is emitted automatically **once the two `gardenio-main` vault secrets are
   provisioned** (a GPG keypair; publish the public key so friends can `gpg --verify`). Until
   then both secrets resolve to empty and the tarball ships on the sha256 + MANIFEST + SHA256SUMS
   floor with no hard-fail.
4. Retention: Generic Package Registry keeps every version; decide whether to prune old calvers
   or keep all (storage on the destination project).

---

## 4. Immediate next actions

- [ ] **Operator:** answer §1 (destination project path + bot access) and §3.3 (A vs. B).
- [ ] **Agent:** once §1 is answered, run §2 against the latest promoted release (`2026.161.20`)
      to do the **first manual publish** and prove the path end-to-end.
- [ ] **Agent:** land §3.2 `publish-tarboat` (token-guarded no-op until the secret exists) so the
      pipeline is ready the moment the vault secret is added.
