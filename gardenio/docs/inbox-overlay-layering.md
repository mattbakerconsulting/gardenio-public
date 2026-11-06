# Gardenio-in-a-Box: the two overlay files and their layering order

The Gardenio-in-a-Box profile is **not one file**. It is split across two values
overlays with complementary, non-overlapping scopes. To get the full box profile
an operator (or `scripts/deploy-byo.sh`) layers **both** on top of the base
values, in this order:

```sh
helm upgrade --install gardenio gardenio/helm/gardenio \
  -f gardenio/helm/gardenio/values.yaml \          # 1. base chart defaults
  -f gardenio/helm/gardenio/values-inbox.yaml \     # 2. storage + secrets + air-gap
  -f deploy-values/gardenio-inbox.yaml             # 3. edge hardening + in-box Keycloak
```

Helm merges later `-f` files over earlier ones (deep-merge for maps), so the
order above is the canonical one: **base → storage/secrets → edge**.

> **Why two files?** The synthesis doc (`gardenio-in-a-box-synthesis.md` §3)
> sketched a single `values-inbox.yaml`, but the work landed in two phases that
> each grew their own overlay: Phase 1 (storage + secrets) created the in-chart
> `gardenio/helm/gardenio/values-inbox.yaml`; the Phase 3 edge work + the in-box
> Keycloak bring-up landed in `deploy-values/gardenio-inbox.yaml` (which is also
> what the chart render tests reference). Reconciling these (ka-0000007248) chose
> to **document the layering explicitly + add a combined render test** rather
> than merge the files, because the chart tests, `deploy-byo.sh`, and the restore
> runbook all already reference the existing paths — a merge would churn all of
> them for no functional gain. The combined render is regression-guarded by
> `tests/inbox_overlay_layering_test.go`.

## What each file owns

| File | Phase / scope | Key flips |
|---|---|---|
| `gardenio/helm/gardenio/values-inbox.yaml` | Phase 1 (ka-0000007539) + P4.3 air-gap | `global.inBox`, `global.airgap`, `global.storageClass=local-path`, `storageClass.create=false`, `volumeSnapshotClass.create=false`, `global.coverage.enabled=false`, `agents.agentStorageClass`, `agents.agentPullPolicy=IfNotPresent`, `externalSecrets.enabled=false`, `coopmux.credentialKey.*` (fail-fast, no plaintext) |
| `deploy-values/gardenio-inbox.yaml` | Phase 3 edge (P3.1/P3.3/P3.4/P3.6) + in-box Keycloak | `keycloak.runtime.enabled=true` + realm bind, `keycloak.forwardAuth.provider=oidc`, `edgeAuth.ava.trustSigner=""` / `patDisabled=true` / `cookieAuth`, `*.avaIngress.enabled=false`, `landing.public=false`, `edge.inFlightReq`/`buffering`/`crowdsec`, `networkPolicies.enabled=true` + `defaultDenyEgress=true`, TLS perHost sketch (commented) |

## Overlapping keys (intentional, and they agree)

A handful of keys appear in **both** files. This is deliberate: each file is
independently coherent for the half of the box it describes, and the shared
values are byte-identical so the deep-merge is a no-op rather than a conflict.

| Key | `values-inbox.yaml` | `deploy-values/gardenio-inbox.yaml` | Merged result |
|---|---|---|---|
| `postgres.enabled` | `true` | `true` | `true` |
| `postgres.persistence.storageClass` | `local-path-retain` | `local-path-retain` | `local-path-retain` |
| `keycloak.runtime.persistence.storageClass` | `local-path-retain` | `local-path-retain` | `local-path-retain` |
| `storageClassRetain.create` | `true` | `true` | `true` |
| `storageClassRetain.name` | `local-path-retain` | `local-path-retain` (+ `provisioner`/`reclaimPolicy`) | union — Retain SC fully specified |
| `backup.postgres.*` | enabled, `17 */6 * * *`, 7d | enabled, `17 */6 * * *`, 7d | identical |

The storage-half file omits `storageClassRetain.provisioner`/`reclaimPolicy`
(it relies on the chart defaults), and the edge-half file spells them out; the
union is the fully-specified Retain StorageClass. The render test asserts the
Retain SC and the pg_dumpall CronJob both survive the layering and that no
`ExternalSecret` / AVA carve-out route slips through.

## If you only apply ONE file

- **Only `values-inbox.yaml`**: you get local-path storage, in-box secrets, and
  air-gap pulls — but the edge is still the cloud profile (AVA trust signer,
  GitLab-PAT fallback, no NetworkPolicy, landing public). **Not** public-safe.
- **Only `deploy-values/gardenio-inbox.yaml`**: you get the fail-closed edge +
  in-box Keycloak — but storage still inherits the base AWS/EBS defaults and
  `externalSecrets`/IRSA are not pinned off, so the chart may try to render
  `ExternalSecret` CRs that have no backing store on a stock k3s box.

You need **both**. The render test in `tests/inbox_overlay_layering_test.go` is
the machine-checked statement of that requirement.

## Where this layering is wired

- `scripts/deploy-byo.sh` — `--values` (base profile) + repeatable `--extra-values`
  layer additional overlays in order (helm precedence: later wins).
- `scripts/bootstrap-ec2.sh` — delegates to `deploy-byo.sh`, adding overlays
  (e.g. the gVisor overlay) via `--extra-values`.
- `gardenio/helm/gardenio/tests/*_test.go` — the edge tests reference
  `../../../deploy-values/gardenio-inbox.yaml`; `inbox_overlay_layering_test.go`
  references **both** files together.
