# Release image signing + admission verification (cosign keyless)

**Task:** ka-0000007951 · **Audit:** ka-0000004158 (CI/CD supply chain) ·
**Rebased on GitHub Actions:** gd-oHQrFnsQ39 (decision gd-wQx4mvpZLk)

## What this closes

Without signing, nothing verifies provenance at admission. That is **attacker
path A6**: anyone with registry-write can push a malicious `controller` image (or
overwrite any calver tag) and the cluster runs it **unverified**.

Two halves close the gap:

1. **Sign.** The GitHub Actions `images.yml` workflow signs every release image it
   pushes on a calver tag with a cosign **keyless** signature (Fulcio/Rekor,
   public Sigstore), by digest.
2. **Verify.** A Kyverno `verifyImages` ClusterPolicy denies admission of any
   gardenio release image that is not signed by that workflow identity.
   Enforcement is **default-off** (`global.imageSigning.verify.enabled: false`).

**Status.** The sign step lands with gd-oHQrFnsQ39. The first signed images
arrive with the next release-captain cut; every image published before that cut
is unsigned, so do not enable the policy against a release older than it.

## How signing works

The `build` job in `.github/workflows/images.yml` runs once per image in the
matrix (`.github/images.json`). On a calver tag push (`github.ref_type == 'tag'`)
and only then, after `docker/build-push-action` pushes
`<registry>/gardenio/<image>:<calver>`, the job:

1. installs cosign with `sigstore/cosign-installer@v3` (cosign v2, pinned);
2. runs `cosign sign --yes <registry>/gardenio/<image>@<digest>`, using the digest
   the build step reports.

The job already holds `id-token: write` (it uses GitHub OIDC for ECR), so cosign
takes a GitHub OIDC token, Fulcio issues a short-lived certificate for it, and the
signature is recorded in Rekor. No key material exists anywhere. The Fulcio
certificate identity is:

```
issuer   https://token.actions.githubusercontent.com
subject  https://github.com/mattbakerconsulting/gardenio/.github/workflows/images.yml@refs/tags/<calver>
```

What this does and does not cover:

- **Signed by digest.** cosign v2 stores the signature as the tag
  `sha256-<digest>.sig` in the same ECR repository. It is keyed by the digest, so
  it holds for `:<calver>`, `:latest` and `:<sha>` alike: they all point at the
  bytes that were signed.
- **Tags only.** Pull requests never reach the sign step, and `images.yml` has no
  branch push trigger, so signing adds nothing to PR or `main` CI.
- **Not signed:** `cache/controller-base` (a build stage nothing deploys), the
  build caches under `cache/*`, and the pull-through mirror under `mirror/*`.
- **Fail-closed per image.** A sign failure fails that image's matrix leg, so a
  release run that reports success has signed every image it pushed.

## The verify policy (Kyverno `verifyImages`)

- **Chart template:** `gardenio/helm/gardenio/templates/kyverno/verify-image-signatures.yaml`,
  gated behind `global.imageSigning.verify.enabled` (**default false**).
- **Canonical standalone source:** `policy/kyverno/verify-image-signatures.yaml`
  (cluster-agnostic; `kubectl apply` it directly). It names the registry as
  `123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio`; substitute your
  account. Its structural test is `policy/kyverno/verify-image-signatures_test.sh`,
  run by `checks.yml`.
- **What it enforces:** any Pod running an image matching `<registry>/gardenio/*`
  must carry a keyless signature with
  `issuer=https://token.actions.githubusercontent.com` and a subject matching
  `^https://github[.]com/mattbakerconsulting/gardenio/[.]github/workflows/images[.]yml@refs/tags/20.*$`.
  A PR run, a branch run or any other workflow never matches. `mutateDigest`,
  `verifyDigest` and `required` all default `true` (tag→digest pinning,
  TOCTOU-safe). `failurePolicy: Fail`: it **fails closed** if Kyverno is
  unreachable.
- **Chart defaults.** The registry defaults to `global.imageRegistry` (override
  with `global.imageSigning.verify.imageRegistry`). The identity ships as
  `oidcIssuer` + `certificateIdentityRegExp`; a fork that publishes its own images
  points the regex at its repo, or pins one tag with `certificateIdentity`.
  Blanking both identities fails the render.
- **Scope:** only OUR release repos. Third-party images (postgres, nats, kyverno
  itself) are **not** signed by our CI and are deliberately not matched. The E2E
  namespaces (`gardenio-rwx*`, `gardenio-e2e*`, which run unsigned `-cover`
  variants) are excluded, mirroring `disallow-cover-images`.
- **The `mirror/*` and `cache/*` skips (load-bearing).** Kyverno image globs treat
  `*` as matching `/`, so the default `imageReferences` (`<registry>/*`) *also*
  catches the pull-through mirror repo `<registry>/mirror/*` (postgres, nats,
  vmagent, victoria-\*, oauth2-proxy, …), third-party upstreams our CI never
  signs, and the build caches under `<registry>/cache/*`. The chart therefore
  defaults `skipImageReferences` to both, so an Enforce rollout does **not** deny
  every mirrored dependency (ka-0000008191). A `namespace` exclude cannot cover
  this: the mirror pods run in `gardenio-system` alongside the signed release
  pods, so the exemption is by *repo*, not namespace.

## Enabling verification (operator follow-up)

The policy is default-off: it is a Kyverno CRD, and an unconditional render fails
`helm upgrade` on a cluster without Kyverno. Enabling is a deliberate operator
action, in this order:

1. **Keep signatures alive in ECR.** The `gardenio/*` lifecycle policy
   (`gardenio/terraform/composition/gardenio/main.tf`) keeps the 15 newest tags
   starting `20` and expires everything else after 14 days. The
   `sha256-<digest>.sig` tags are in "everything else", so a release's signature
   disappears two weeks after the cut and Enforce would then deny that release.
   Add a lifecycle rule that keeps `sha256-` tags as long as their calver images
   before enabling anything.
2. **Install Kyverno.** Nothing installs it today: the EKS-era `kyverno`
   platform addon went with the deleted `terraform/modules/platform` (PR #1681).
   Kyverno needs ECR read access to fetch the `.sig` tags: either a
   dockerconfigjson Secret for the ECR registry, or Kyverno's
   ECR credential helper with an identity that may read `gardenio/*`.
3. **Roll out the policy, Audit first, then Enforce.** Set
   `global.imageSigning.verify.enabled: true` with
   `validationFailureAction: Audit` in `deploy-values/gardenio-k3s.yaml`, plus
   `certificateIdentity` (or `certificateIdentityRegExp`) naming the CI identity
   that signs the release images; the chart refuses to render without one. Apply it
   with the chart, confirm the PolicyReports are clean against the running fleet, then
   flip it to `Enforce` and apply again.

Do the Enforce flip only after a signed release is the one running; otherwise
Enforce denies the current deployment. Signing and verification are independent
knobs so signing can ship before enforcement turns on.

## Verifying a signature by hand

```bash
cosign verify \
  123456789012.dkr.ecr.us-east-1.amazonaws.com/gardenio/controller:<calver> \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --certificate-identity-regexp '^https://github\.com/mattbakerconsulting/gardenio/\.github/workflows/images\.yml@refs/tags/20.*$'
```

Substitute the registry's account id, and log in to ECR first
(`aws ecr get-login-password | docker login ...`). Exit 0 means signed by the
release workflow; a non-zero exit with `no signatures found` means the image was
not signed (published before signing landed, or not by a calver tag run).

## Related defenses

- `disallow-cover-images` ClusterPolicy denies coverage-instrumented `-cover`
  images outside E2E namespaces (`policy/kyverno/disallow-cover-images.yaml`).
- `restrict-agent-registries` ClusterPolicy denies agent pods pulling images
  from outside the gardenio registry allow-list.
- The `gardenio-ci-ecr-release` role (`gardenio/terraform/composition/gardenio/ci_ecr.tf`)
  is the only CI identity that may push `gardenio/*`, and only a run on a calver
  tag or on `main` can assume it.
- `gb verify-prod-images` is an operator drill asserting no `-cover` image
  reached the prod deploy surface.
