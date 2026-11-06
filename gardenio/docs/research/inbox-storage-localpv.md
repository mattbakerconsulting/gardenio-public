# Storage Axis Research: Replacing EBS/EFS CSI with Local Storage for Gardenio-in-a-Box

**Task:** ka-0000005525 | **Epic:** Gardenio-in-a-Box (ka-0000001295) | **Scope:** storage axis only
**Operator:** Matthew Baker (matthew.baker@acme.example) | **Status:** research complete — implementation deferred to the follow-up beads in §8

---

## 1. TL;DR / Recommendation

Adopt **Rancher local-path-provisioner** (provisioner `rancher.io/local-path`) as the single-node storage backend, relying on the version k3s ships pre-wired as the default `local-path` StorageClass. It is the only candidate that preserves **dynamic provisioning**, which the controller's `podmanager.ensurePVC` and the `pre-upgrade-adopt-pvcs` hook hard-depend on. The entire conversion is achievable with **one new values overlay** (`values-inbox.yaml`) — no template edits are strictly required, because the EBS-specific objects (`StorageClass`, `VolumeSnapshotClass`) are already gated `create:false` by default and every PVC inherits `global.storageClass`. **Drop the VolumeSnapshotClass** (latent, unused, and unsupported by local-path — zero functional loss). For backup, since local-path cannot do CSI snapshots, the headline approach is: a single in-cluster **`pg_dumpall` CronJob** of postgres (the only crown-jewels store) writing to local disk, backed by an **out-of-band EC2-host EBS-snapshot** of the data volume as the whole-system DR floor (an ops action, not a runtime AWS dependency).

---

## 2. Why (the deciding factors)

1. **Dynamic provisioning is non-negotiable.** The controller's `podmanager.ensurePVC` (`gardenio/controller/internal/podmanager/manager.go:582`) **creates PVCs at runtime** for every persistent agent, with a configurable `StorageClassName`. These are never Helm-templated — the cluster must satisfy a freshly-created PVC on demand. The `pre-upgrade-adopt-pvcs` hook (`templates/pre-upgrade-adopt-pvcs.yaml`) also *creates-if-missing* PVCs at upgrade time. Both flows assume a provisioner behind the StorageClass. Only local-path provides this. Static `local` PVs and raw hostPath cannot — every runtime-generated agent PVC would wedge `Pending` forever, re-introducing exactly the gitlab-bridge wedge that ka-0000004481 was built to prevent.

2. **Single-node nullifies the usual reasons to prefer alternatives.** WaitForFirstConsumer binding is a no-op with one node; node-affinity/topology awareness (the structural advantage of static `local` PVs over a hostPath provisioner) is moot. No cross-node scheduling, no HA — by design, matching the in-box target.

3. **Security / PSA baseline.** The chart enforces **PSA baseline** on the main namespace, and baseline **forbids raw `hostPath`** in pod specs (`spec.volumes[*].hostPath` must be nil). This disqualifies Option B (raw hostPath) outright — it would fail admission unless the whole namespace is dropped to privileged, a platform-wide security regression. Crucially, workload pods referencing a `persistentVolumeClaim` are allowed under baseline; local-path's hostPath access lives only in its **helper pod, which runs in its own namespace** (`local-path-storage`), outside the baseline-enforced workload namespace. So Option A is PSA-clean.

4. **Operational simplicity for a scripted single-EC2 install.** k3s ships local-path as the default SC for free — the conversion collapses to flipping `global.storageClass` and `agents.agentStorageClass` to `local-path` via one overlay file. The chart's PVC topology runs unchanged.

---

## 3. Recommended choice

**Ranked:**
1. **(A) Rancher local-path-provisioner — RECOMMENDED.** Only option preserving dynamic provisioning.
2. **(C) Static `local` PersistentVolumes — fallback only.** No dynamic provisioning; breaks `ensurePVC` and the adopt hook without bolting on `sig-storage-local-static-provisioner` + a pre-mounted directory pool (capacity-bounded, clunky). Single-node moots its one advantage (topology awareness).
3. **(B) Raw hostPath in the chart — REJECT.** No dynamic provisioning *and* fails PSA-baseline admission in workload pods.

### Capability matrix

| Axis | (A) local-path-provisioner | (B) raw hostPath | (C) static `local` PV |
|---|---|---|---|
| **Dynamic provisioning (`ensurePVC` + adopt hook)** | **Yes** — PVCs bind automatically | No — no PVC objects at all; **breaks** | No (`no-provisioner`); runtime PVCs unmatched; **breaks** |
| **PSA baseline (workload ns)** | Pass — pods ref PVC; hostPath isolated in helper-pod's own ns | **Fail** — raw hostPath forbidden | Pass — pods ref PVC |
| **Access modes** | RWO (node-local); RWX only via `sharedFileSystemPath` (NFS) | RWO-ish by path | RWO |
| **Snapshots (CSI VolumeSnapshot)** | **No** (not a CSI driver) | No | No |
| **Volume expansion** | No (`allowVolumeExpansion` is a no-op even if true) | No | No |
| **Capacity enforcement** | None by default (size advisory); real quota needs XFS `prjquota` + `xfsprogs` helper | None | None |
| **reclaimPolicy** | `Delete` (k3s default) or `Retain` | n/a (dirs always persist, unmanaged) | `Retain` |
| **Default wiring** | k3s default SC `local-path`; kind `standard`; k0s needs OpenEBS ext | hand-authored per PVC | hand pre-created PVs + external provisioner |
| **Operational simplicity (single-EC2)** | **Best** — built-in on k3s | Worst — high edit surface + broken | Most moving parts |

**Security note:** pin local-path-provisioner **>= v0.0.36** (CVE-2025-62878 — HelperPod template validation), run it in its own namespace, and keep RBAC tight on the `local-path-config` ConfigMap.

---

## 4. Per-PVC impact table

| PVC | Access mode needed | Works on local-path? | Note |
|---|---|---|---|
| **postgres** (StatefulSet VCT) | RWO | **Yes** | CRITICAL — kbeads + controller data. The one MUST-backup store. |
| **nats** (StatefulSet VCT) | RWO | **Yes** | JetStream durable state; mostly re-derivable from postgres on restart. |
| **keycloak-runtime** (StatefulSet) | RWO | **Yes** | Realm *definition* is declarative EDP CRDs (config-as-code); only runtime users/sessions in DB. |
| **vmsingle** (StatefulSet VCT) | RWO | **Yes** | Metrics TSDB; disposable. |
| **grafana** (PVC) | RWO | **Yes** | Disposable if dashboards-as-code; PVC holds only SQLite prefs. |
| **coopmux** (PVC) | RWO | **Yes** | Agent credentials; re-syncable — controller re-pushes from account beads (in postgres) on startup. |
| **slack-bridge** (PVC 100Mi–1Gi) | RWO | **Yes** | SSE/dedup cursor; disposable. |
| **gitlab-bridge** (PVC) | RWO | **Yes** | Same; the adopt-hook wedge target (ka-0000004481) — works once `global.storageClass=local-path`. |
| **jira-bridge** (PVC) | RWO | **Yes** | Disposable. |
| **wl-bridge** (PVC) | RWO | **Yes** | Disposable. |
| **coverage** (PVC, `enabled:false`) | **RWM** | **NO** | RWM unsatisfiable on node-local local-path. **Keep disabled.** Only true blocker. |
| **agent-workspace** (dynamic, `ensurePVC`, 10Gi default) | RWO | **Yes** | Created at runtime; GC'd by `pvcgc` TTL. Requires `agents.agentStorageClass=local-path`. |

**Net:** every PVC works on local-path except `coverage` (RWM, default-off — keep off).

---

## 5. Snapshot & backup story on local disk

### Data-class classification

| Class | Components | Mechanism |
|---|---|---|
| **MUST-backup** | **postgres** (kbeads beads DB + controller state) | In-cluster `pg_dumpall` CronJob → local backup PVC/hostPath |
| **nice-to-have** | nats JetStream; keycloak runtime users; coopmux creds | nats: `nats account backup` CronJob. keycloak users + coopmux: **already covered** — keycloak runtime users live in postgres (caught by `pg_dumpall`); coopmux creds re-sync from account beads in postgres on startup. |
| **disposable** | vmsingle, grafana (dashboards-as-code), 4 bridges (SSE cursors), agent-workspace (TTL-GC'd), coverage | No backup; rebuild from chart + postgres on restore. |

### Recommended mechanisms

- **postgres → `pg_dumpall` CronJob (the only MUST):** mirror the existing CronJob pattern (`templates/token-rotator/cronjob.yaml`, `templates/rwx-reaper/cronjob.yaml`). Run `pg_dumpall -h gardenio-postgres -U postgres | gzip > /backup/pg-$(date +%F-%H%M).sql.gz`; use `pg_dumpall` (not `pg_dump`) to capture **all** DBs (beads/controller *and* keycloak) plus roles in one shot. Write to a backup dir that lives on the EBS data volume so the host snapshot also captures it. Prune with `find /backup -mtime +N -delete`. Online + transactionally consistent — no quiescing. **Restore:** fresh postgres volume, `gunzip -c dump.sql.gz | psql -U postgres`; controller re-syncs coopmux creds, Keycloak operator reconciles realm from CRDs, bridges rebuild cursors.
- **nats (optional) → `nats account backup`:** use natscli, *not* `cp -r` of a live JetStream dir (corrupts mid-write). Restore fails over an existing same-named stream — runbook must "delete stream, then restore." Include only if zero event-loss across restore is a goal.
- **keycloak:** **no separate job** — `kc.sh export` requires the server stopped (hostile to an always-on box), and `pg_dumpall` already captures the runtime users/sessions.

### Verdict on the VolumeSnapshotClass: **DROP it (gate off) for in-box**

Definitive across all dossiers: local-path-provisioner is **not a CSI driver**, so a `VolumeSnapshot` against it fails (`snapshotting non-CSI volumes is not supported`); `templates/volumesnapshotclass.yaml` hard-codes `driver: ebs.csi.aws.com` which is meaningless without the EBS CSI driver; and **nothing in the chart or controller creates or consumes any VolumeSnapshot CR** (gVisor freeze/thaw uses node-local `runsc` hostPath checkpoints, not CSI snapshots). It already defaults `create:false` and is additionally CRD-gated. **Action:** keep the template in-repo (prod AWS-POC may still want it) but set `volumeSnapshotClass.create: false` explicitly in the in-box overlay and document why. Zero functional impact in-box.

### DR floor: EC2-host EBS snapshot

The box runs on EC2, so the underlying EBS volume under the k3s data dir (`/var/lib/rancher/k3s/storage`) remains snapshottable at the **AWS layer**, independent of the cluster's storage class. An **off-cluster** cron (`aws ec2 create-snapshot --volume-id ...`) or an AWS Backup plan captures *every* PVC at once, crash-consistent, including the dynamic agent PVCs. This is an **ops/control-plane action, not a runtime call from the workload**, so it does **not** reintroduce an AWS runtime dependency — it satisfies "no AWS deps at runtime." Cadence: hourly/daily EBS snapshot as the floor; the `pg_dumpall` artifact is the clean, portable, app-consistent restore on top (and survives a future move to a non-AWS host).

---

## 6. Chart & controller change surface

All paths absolute under `/home/agent/bot/gardenio/work`. **Net: one new overlay file is the only required change.**

1. **`gardenio/helm/gardenio/values.yaml` (line ~657) — base `global.storageClass: "gp2"` STAYS.** It is the EKS/AWS default. Do not edit the base; the in-box value is supplied by the overlay. Optionally add a comment pointing at the overlay.

2. **`gardenio/helm/gardenio/templates/storageclass.yaml` — no edit required.** `storageClass.create` defaults `false` (values ~line 967), so it renders nothing. Rely on k3s's built-in default `local-path` SC. *Optional hardening only:* parameterize `provisioner: {{ .Values.storageClass.provisioner | default "ebs.csi.aws.com" }}` and wrap the EBS-only `parameters`/`type`/`allowVolumeExpansion` in `{{- if eq (...) "ebs.csi.aws.com" }}` — preferred to skip on a single node.

3. **`gardenio/helm/gardenio/values.yaml` (lines ~966–977) — add `storageClass.provisioner: "ebs.csi.aws.com"` ONLY if item 2's optional parameterization is taken.** Otherwise no change.

4. **`gardenio/helm/gardenio/templates/volumesnapshotclass.yaml` — no edit.** Already `create:false` (values ~line 985) + CRD-gated on `snapshot.storage.k8s.io/v1`. Explicitly set `volumeSnapshotClass.create: false` in the overlay for unambiguous intent.

5. **`gardenio/helm/gardenio/values.yaml` (line 1589) — set `agents.agentStorageClass: "local-path"` in the overlay.** Wired to `AGENT_STORAGE_CLASS` at `templates/controller/deployment.yaml:270-272` → `config.AgentStorageClass` (`config.go:738`) → `podmanager.ensurePVC` (`manager.go:582`). Empty would fall back to the cluster-default SC (which *is* `local-path` on k3s, `manager.go:613-619`), but **be explicit**. Note: a project bead's `storage_class` label can override per-project — in-box deployments must not set it to an AWS SC name.

6. **`gardenio/helm/gardenio/templates/pre-upgrade-adopt-pvcs.yaml` — no edit; compatible.** Computes SC as `component.persistence.storageClass | default $defaultSC` where `$defaultSC = .Values.global.storageClass`; with `local-path` it Just Works. Hardcodes `accessModes: ReadWriteOnce` (local-path-compatible). WaitForFirstConsumer Pending-until-scheduled behavior is identical to today's EBS (same binding mode), not new breakage.

7. **`gardenio/helm/gardenio/templates/coverage.yaml` — no edit; keep disabled.** Declares `accessModes: [ReadWriteMany]`; local-path is RWO-only → would wedge Pending. Gated by `global.coverage.enabled` (default `false`). Set it explicitly `false` in the overlay and document. If in-box coverage is ever needed, a follow-up must switch it to `emptyDir` — out of scope.

8. **NEW: `gardenio/helm/gardenio/values-inbox.yaml`** — the single required change. Apply with `helm install/upgrade ... -f values.yaml -f values-inbox.yaml`:
   ```yaml
   global:
     storageClass: "local-path"     # k3s built-in default SC
     coverage:
       enabled: false               # RWM unbindable on local-path
   storageClass:
     create: false                  # do not create the EBS SC in-box
   volumeSnapshotClass:
     create: false                  # local-path cannot honor CSI snapshots
   agents:
     agentStorageClass: "local-path"
   backup:                          # (paired with item 9 below)
     postgres:
       enabled: true
       schedule: "17 */6 * * *"
       retentionDays: 7
       pvcSize: 5Gi
     nats:
       enabled: false               # nice-to-have; flip on for zero event-loss
   ```
   All per-component `persistence.storageClass: ""` entries (verified at values lines 1962, 2090, 2545, 2630, 2837, 3146, 3921, 3973, 4049, 4921 — postgres, nats, keycloak, vmsingle, grafana, bridges) inherit `global.storageClass` via `| default .Values.global.storageClass`, so the overlay propagates `local-path` everywhere with no per-component edits.

9. **NEW: `gardenio/helm/gardenio/templates/backup/postgres-cronjob.yaml`** — mirror `token-rotator/cronjob.yaml`: a CronJob running the postgres-client image, mounting a backup PVC, executing `pg_dumpall | gzip` + retention prune, gated on `backup.postgres.enabled`. Optional sibling `nats-backup-cronjob.yaml` gated on `backup.nats.enabled`.

10. **Optional: `global.inBox: false` marker** in the global block (`values.yaml` ~line 611), set `true` in the overlay — informational for this task, but lets later epic axes (secrets, ingress, DB) branch on one documented flag.

11. **`gardenio/controller/internal/podmanager/manager.go` / `config/config.go` — no code edits.** Behavior (empty SC → cluster default) is in-box-safe and already driven by `agents.agentStorageClass`.

12. **`gardenio/terraform/` (AWS-POC) — out of scope.** The EBS CSI EKS addon + IRSA is simply unused once EKS is removed; in-box host provisioning (k3s install) is a separate infra axis.

---

## 7. Risks & gotchas

- **coverage RWM incompatibility (only true blocker).** `templates/coverage.yaml` requires `ReadWriteMany`; local-path is RWO-only and the PVC will never bind. Default-off, so benign — but enabling coverage in-box wedges it `Pending`. Keep `global.coverage.enabled=false`; document.
- **WaitForFirstConsumer + adopt-hook timing.** A PVC the adopt hook creates before its consumer pod exists stays `Pending` until the pod schedules. This is **identical to today's EBS behavior** (same binding mode), not new breakage; on single-node the pod schedules locally and binding completes.
- **Capacity quota is advisory.** local-path does **not enforce `requests.storage`** by default — a runaway agent workspace, unbounded vmsingle TSDB, or NATS stream can fill the host disk and starve postgres (data-loss risk, shared-fate on one disk). The 10Gi agent default and project `storage_class` labels become advisory. Mitigate with either (a) XFS filesystem mounted `prjquota` + a custom helper image carrying `xfsprogs` for real per-PVC quotas, or (b) a generously-sized host disk + disk-usage monitoring.
- **No volume expansion.** local-path cannot honor `allowVolumeExpansion` — editing `requests.storage` silently no-ops. Any "grow postgres/nats PVC" runbook step must instead recreate the PV. Document recreate-to-resize.
- **No HA / node-replacement = total data loss.** WFC pins every PV to the single node's disk; replacing the EC2 instance loses all state unless the `pg_dumpall` artifact or EBS snapshot exists. This matches the single-node target but makes the backup story (§5) mandatory, not optional, for any data that matters.
- **Uninstall data retention.** k3s local-path defaults to `reclaimPolicy: Delete`, which removes the on-disk directory when the PVC is deleted (and on `helm uninstall`). For critical stores (postgres, nats, keycloak), set `reclaimPolicy: Retain` so data survives — mirroring the intent of the chart's existing `helm.sh/resource-policy: keep` on the cluster-scoped SC. (This requires either a custom local-path SC with Retain or per-PV patching, since the k3s default SC is Delete — note as a decision point for the implementation bead.)

---

## 8. Follow-up tasks to file

- **chart: add `values-inbox.yaml` overlay** (`global.storageClass=local-path`, `agents.agentStorageClass=local-path`, `storageClass.create=false`, `volumeSnapshotClass.create=false`, `global.coverage.enabled=false`) — the one required change.
- **chart: parameterize `storageclass.yaml` provisioner** + gate EBS-only `parameters`/`allowVolumeExpansion` (optional hardening, only if Helm should own the in-box SC).
- **chart: gate VolumeSnapshotClass off for in-box** + document why (local-path is non-CSI).
- **chart: add `pg_dumpall` backup CronJob** (`templates/backup/postgres-cronjob.yaml`, mirror token-rotator) → local backup PVC, retention prune.
- **chart: add optional NATS `nats account backup` CronJob** (gated `backup.nats.enabled=false`).
- **chart/decision: choose `reclaimPolicy: Retain` for critical stores** (custom local-path SC vs per-PV patch) so data survives uninstall.
- **ops: EC2-host EBS-snapshot cron / AWS Backup plan** for the data volume as the DR floor (off-cluster, not a runtime dep).
- **docs: restore runbook** in `gardenio/docs/` — whole-box EBS-snapshot restore + app-consistent `pg_dumpall` restore + NATS delete-then-restore caveat.
- **chart (optional): add `global.inBox` profile marker** for future epic axes to branch on.
- **follow-up (out of scope): coverage `emptyDir` fallback** if in-box coverage is ever required (RWM unsupportable on single-node local-path).