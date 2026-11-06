# Object-Store Axis Research: Replacing S3 with an In-Box Store for Gardenio-in-a-Box

**Task:** ka-0000004744 | **Epic:** Gardenio-in-a-Box (ka-0000001295) | **Scope:** object-store axis only
**Operator:** Matthew Baker (matthew.baker@acme.example) | **Status:** research complete — implementation deferred to the follow-up beads in §8

---

## 1. TL;DR / Recommendation

Run **MinIO as a single-node, single-drive Deployment in-cluster**, backed by the same `local-path` PVC the storage axis (ka-0000005525) already standardizes on, and point coop + the controller at it via an endpoint override. MinIO is the only candidate that keeps the **existing S3 code path completely intact** — coop and the controller speak a tiny, well-worn slice of the S3 API (just `PutObject`, `GetObject`, `HeadObject`, `HeadBucket`, `ListObjectsV2`), all of which MinIO implements faithfully.

**But the headline finding is an integration gap, not a compatibility one.** Neither coop (Rust) nor the controller (Go) currently exposes an **S3 endpoint or path-style override**. Both build their client from the AWS default chain (`aws_config::defaults(...)` / `config.LoadDefaultConfig`) with only `BUCKET`/`PREFIX`/`REGION` plumbed. MinIO needs a custom `endpoint_url` **and** `force_path_style=true`, and — critically — **the Rust SDK cannot enable path-style from an environment variable; it must be set in code** ([aws-sdk-rust #1230](https://github.com/awslabs/aws-sdk-rust/issues/1230)). So the real cost of this axis is **~15 lines of client-construction code in two repos plus a chart Deployment**, not a rewrite. There is no S3-feature coop relies on (no multipart, versioning, KMS-SSE, copy, or delete API calls) that MinIO lacks.

**Reject "local-path PV only" (drop S3 entirely):** it sounds simplest but is the most invasive — it would force a rewrite of coop's `S3Storage` impl, the controller's `gb audit`/image-proxy/`gb image upload` S3 clients, the beads-viewer image proxy, and the S3-restore resume path, replacing a stable persistence contract with a brand-new filesystem one. **Reject SeaweedFS:** more capable at scale but solves problems (distributed volumes, erasure coding, scale-out) we don't have on one box, while adding a second storage system alongside the `local-path` PV that already exists.

---

## 2. What actually uses S3 today (the surface we must preserve)

S3 is used by **two languages, four consumers**, all against one bucket (`COOP_S3_BUCKET`, prod `gardenio-sessions`, prefix `coop/sessions`):

| Consumer | Lang | File | Purpose | S3 verbs |
|---|---|---|---|---|
| **coop session persistence** | Rust | `coop/crates/cli/src/s3.rs` | Real-time upload of transcripts, recordings, session log, event logs, workflow journals, meta.json; S3-only resume restore | `PutObject`, `GetObject`, `HeadObject`, `HeadBucket`, `ListObjectsV2` |
| **`gb audit` / session fetch** | Go | `gardenio/controller/cmd/gb/audit.go` | List + download archived sessions for audit/replay | `GetObject`, `ListObjectsV2` |
| **`gb image upload`** | Go | `gardenio/controller/cmd/gb/image.go` | Push agent-attached images to `<prefix>/images/` | `PutObject` |
| **beads-viewer image proxy** | Go | `gardenio/controller/cmd/beads-viewer/image_proxy.go` | Stream `<prefix>/images/*` objects to the UI | `GetObject` |

**Complete verb inventory (grepped across both repos):**

- Rust (coop): `put_object` ×2, `get_object` ×3, `head_object` ×1, `head_bucket` ×1, `list_objects_v2` ×5.
- Go (controller): `PutObject` ×2, `GetObject` ×13, `ListObjectsV2` ×1.

That is the **entire** S3 dependency. Notably **absent**: no multipart upload, no `CopyObject`, no `DeleteObject`, no versioning/`GetBucketVersioning`, no object-lock, no tagging, no bucket-policy or ACL API calls at runtime. The `ListObjectsV2` calls use exactly three features — `prefix`, `delimiter("/")` (for "directory" enumeration in `list_sessions`), and `continuation_token` pagination — all first-class in MinIO.

**Terraform today** (`gardenio/terraform/modules/s3-sessions`) layers on KMS-SSE, versioning, public-access-block, ownership controls, and lifecycle rules (expire-noncurrent, abort-multipart). **None of these are exercised by the application code** — they are bucket-level hardening enforced by AWS, not API calls coop makes. In-box, encryption-at-rest becomes a disk-level concern (LUKS/EBS volume encryption on the host) and lifecycle/versioning are dropped (or emulated by a MinIO ILM rule if ever needed). This whole terraform module simply disappears in the in-box profile — it is AWS-POC infra, out of the in-box runtime path.

---

## 3. The integration gap (the real work, and why it's small)

### 3.1 coop (Rust) — `coop/crates/cli/src/s3.rs:362`

```rust
async fn build_aws_client(region: Option<&str>) -> aws_sdk_s3::Client {
    let mut config_loader = aws_config::defaults(aws_config::BehaviorVersion::latest());
    if let Some(region) = region {
        config_loader = config_loader.region(aws_config::Region::new(region.to_owned()));
    }
    let sdk_config = config_loader.load().await;
    aws_sdk_s3::Client::new(&sdk_config)   // <-- no endpoint, no path-style
}
```

`config.rs` plumbs only `COOP_S3_BUCKET` / `COOP_S3_PREFIX` / `COOP_S3_REGION`. To talk to MinIO we must:

1. Add a `COOP_S3_ENDPOINT` (and a `COOP_S3_FORCE_PATH_STYLE` / implied-by-endpoint) knob to `config.rs`.
2. In `build_aws_client`, when an endpoint is set, build via `aws_sdk_s3::config::Builder::from(&sdk_config).endpoint_url(ep).force_path_style(true).build()` and `Client::from_conf(...)`.

**Why path-style must be code, not env:** the AWS default chain *can* read `AWS_ENDPOINT_URL_S3` from the environment, but the Rust SDK **ignores the `s3.addressing_style` profile/section** and has **no env var for `force_path_style`** — it must be set on the config builder in code ([aws-sdk-rust #1230](https://github.com/awslabs/aws-sdk-rust/issues/1230), [AWS SDK for Rust endpoints docs](https://docs.aws.amazon.com/sdk-for-rust/latest/dg/endpoints.html)). MinIO single-node serves buckets path-style (`http://minio:9000/<bucket>/<key>`); without `force_path_style` the SDK issues virtual-hosted requests (`http://<bucket>.minio:9000/...`) that fail DNS. So a code change is unavoidable — there is no env-only path. (This same call site already rebuilds the client on credential thaw via `reload_credentials`, so the endpoint/path-style config must be threaded through both `build_aws_client` and that rebuild — one shared helper.)

**Credentials:** IRSA/web-identity disappears in-box. MinIO uses a static access-key/secret-key pair. coop's default chain will pick these up from `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` env vars (sourced from an in-box k8s secret — see the secrets axis, ka-0000004337) with **no code change** beyond ensuring the env vars are set; the credential-thaw machinery degrades to a no-op (static creds never expire).

### 3.2 controller (Go) — `gb audit.go`, `image_proxy.go`, `image.go`, `config.go:769`

Identical shape: `config.go` reads only `COOP_S3_BUCKET/PREFIX/REGION`; the S3 clients are built from `config.LoadDefaultConfig(...)` with no `BaseEndpoint`. The Go SDK v2 fix is one option on each client constructor:

```go
s3.NewFromConfig(awsCfg, func(o *s3.Options) {
    if ep := os.Getenv("COOP_S3_ENDPOINT"); ep != "" {
        o.BaseEndpoint = aws.String(ep)
        o.UsePathStyle = true
    }
})
```

The Go SDK v2 **does** honor `AWS_ENDPOINT_URL` / `AWS_ENDPOINT_URL_S3` for the endpoint, but — same as Rust — **path-style is not env-settable** and must be `o.UsePathStyle = true` in code. Three client constructors to touch (`audit.go`, `image_proxy.go`, `image.go`); the cleanest move is one shared `newS3Client(ctx)` helper they all call.

### 3.3 Net code delta

- coop: 1 new config field + ~8 lines in `build_aws_client` (threaded into the thaw rebuild).
- controller: 1 config field + 1 shared client helper (~10 lines) called from 3 sites.
- chart: 1 MinIO Deployment+Service+PVC+Secret (or a values-gated subchart), plus `COOP_S3_ENDPOINT` wired into the controller, beads-viewer, and agent pod specs (`podspec.go:616` already sets the other `COOP_S3_*` vars — add one more).

No data-model change, no API-contract change, no test rewrite. The `S3Storage` trait and its in-memory mock are untouched, so coop's existing `s3_tests.rs` suite still covers the logic.

---

## 4. Candidate comparison

| Axis | (A) MinIO single-node — **RECOMMENDED** | (B) local-path PV only (drop S3) | (C) SeaweedFS |
|---|---|---|---|
| **Keeps existing S3 code path** | **Yes** — endpoint+path-style override only | **No** — rewrite coop `S3Storage` + 3 Go clients + viewer proxy + resume path | Yes (S3 gateway) |
| **S3 API coverage for our 5 verbs** | **Full + battle-tested** | n/a (removed) | Full, but via a gateway layer |
| **Net code change** | ~15 lines, 2 repos | Large multi-file rewrite, 2 repos | ~15 lines (same override) but new infra |
| **New runtime components** | 1 (minio) | **0** | 2+ (master + volume, or `weed server`) |
| **Durability on one disk** | Single drive, no erasure (matches single-box) | Same disk, same fate | Can erasure-code, but pointless on 1 disk |
| **Capacity** | Bounded by the host PVC; advisory | Bounded by host PVC | Bounded by host PVC |
| **Operational complexity** | Low — one well-known container | Lowest infra, **highest code risk** | Highest — extra moving parts for scale we lack |
| **Memory footprint** | ~100–200 MB idle | 0 | master+volume processes, heavier |
| **Right-sized for single-EC2** | **Yes** | Tempting but code-risky | **Over-engineered** |
| **License** | AGPL-v3 (server) — see §7 | n/a | Apache-2.0 |

### Why not (B) "local-path only / drop S3"

It is the **most** invasive option despite sounding like the least. coop's persistence is built around an S3 key-space (`<prefix>/<session-key>/transcripts/N.jsonl`, `recording/`, `workflow/`, `meta.json`) with `ListObjectsV2`-driven enumeration (`list_sessions`, `list_spawns`, `list_transcripts`, `list_recording_chunks`, `list_workflow_files`) and a `download → resume` restore path. Replacing S3 with a filesystem means:
- Rewriting all six `S3Storage` list/get/put methods against `std::fs` + `walkdir`, re-implementing prefix/delimiter semantics by hand.
- Rewriting `gb audit`'s list/fetch, the image proxy's object streaming, and `gb image upload` against the filesystem.
- Re-designing the **resume contract**: today an agent restarts on a fresh pod and pulls prior state from S3 by bead ID. A local PVC is pod-/node-local; making it survive a pod restart means the PVC must be re-attached to the new pod — which is exactly what `local-path` does *for a StatefulSet*, but coop agents are dynamically-created pods, not a stable StatefulSet. The S3 indirection is what currently decouples "where the agent ran" from "where its history lives." Dropping it re-introduces that coupling.

This trades a stable, tested persistence contract for a new one with broad blast radius. Not worth it to save one container.

### Why not (C) SeaweedFS

SeaweedFS shines at **scale-out**: many volumes across many disks/nodes, erasure coding, billions of small files. On a single EC2 with one data disk, none of those advantages apply — you would run the master+volume servers (or `weed server` all-in-one) plus its S3 gateway, a strictly heavier footprint than MinIO, to land on the same single-disk durability. The S3 gateway is also a thinner-tested compat layer than MinIO's native S3 surface. More parts, same outcome.

---

## 5. Durability & capacity on one disk (shared-fate reality)

This axis inherits the storage axis's constraints (ka-0000005525) verbatim, because MinIO's data lives on a `local-path` PVC on the single host disk:

- **No redundancy.** Single drive, no erasure coding (and enabling MinIO erasure on one disk is pointless). Disk loss = session-archive loss. This is **acceptable**: session transcripts/recordings are **audit/replay artifacts, not crown jewels**. The crown jewel is postgres (kbeads + controller state), which has its own `pg_dumpall` backup story. A lost session archive degrades audit history; it does not break the running platform.
- **Shared fate with postgres.** MinIO, postgres, NATS, and agent workspaces all sit on the same host disk. A runaway session-recording stream (coop flushes every state transition + every 1 s by default — `s3_upload_interval_ms: 1000`) competing for the same disk as postgres is the real risk. **Mitigation:** size MinIO's PVC with a hard cap and apply a MinIO ILM expiry rule (e.g. expire objects > N days) so the session archive is self-pruning — replacing the AWS lifecycle rule the terraform module used to provide. Pair with disk-usage monitoring (the storage axis already calls for this).
- **Backup floor.** Same as the storage axis: an out-of-band EC2-host EBS snapshot of the data volume is the whole-box DR floor. Session archives are explicitly **lower priority** than postgres for restore — document that they may be sacrificed.
- **Capacity.** Bounded by the host disk. A box sized for the platform (the EC2-sizing axis, ka-0000003889) should budget a session-archive allowance; ILM expiry keeps it bounded. There is no "infinite S3" anymore — capacity planning becomes mandatory, which the ILM rule + PVC cap handle.

---

## 6. Recommended MinIO deployment shape

- **Mode:** single-node single-drive (SNSD), `MINIO_VOLUMES=/data`, one `local-path` PVC (RWO, fits single-node).
- **Form:** a values-gated `templates/minio/` Deployment+Service+PVC+Secret in the chart (or the upstream MinIO subchart pinned), gated on `inBox.objectStore.enabled` / off by default so the AWS-POC path is untouched. Mirrors how the storage axis adds in-box-only objects behind a flag.
- **Bucket bootstrap:** an init step (`mc mb local/gardenio-sessions --ignore-existing`) via a small Job or the MinIO entrypoint, since coop calls `head_bucket` on connect and expects the bucket to exist (it warns-but-continues on HEAD failure, but uploads need the bucket present).
- **Credentials:** root user/password from an in-box k8s secret (secrets axis), surfaced to coop/controller as `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY` + `COOP_S3_ENDPOINT=http://minio.<ns>.svc:9000`.
- **PSA:** MinIO runs fine under **PSA baseline/restricted** (non-root, no privileged, no hostPath — it uses a PVC), so it is admission-clean in the workload namespace, unlike raw hostPath.
- **Encryption-at-rest:** rely on host-disk/volume encryption (LUKS or the EC2 EBS volume's encryption) rather than MinIO KMS — KMS-SSE was an AWS-bucket feature, not an app requirement.
- **No TLS internally:** MinIO reachable only in-cluster over ClusterIP; HTTP is acceptable on the pod network (the public-exposure axis ka-0000003100 governs the edge, and MinIO is never edge-exposed).

---

## 7. Risks & gotchas

- **`force_path_style` is mandatory and code-only (both SDKs).** The single most likely failure mode: someone sets `COOP_S3_ENDPOINT` via env, assumes it "just works," and gets virtual-host DNS failures because path-style was never toggled. It **cannot** be enabled by env var in either the Rust or Go SDK — must be code. This is the crux of the whole axis. ([aws-sdk-rust #1230](https://github.com/awslabs/aws-sdk-rust/issues/1230))
- **MinIO server license is AGPL-v3.** Self-hosting for internal use is fine, but flag it for legal awareness if the box is ever distributed to third parties. (The `mc` client and Go SDK are Apache-2.0; only the server binary is AGPL.) If AGPL is a hard blocker, the fallback is SeaweedFS (Apache-2.0) at the cost of §4(C)'s extra complexity — note as a decision point.
- **Bucket must pre-exist.** coop's `head_bucket` on connect only warns on failure, but `put_object` into a missing bucket errors. The bootstrap Job is not optional.
- **Endpoint must reach the thaw rebuild path.** coop's `reload_credentials` rebuilds the client on credential thaw (`s3.rs:385`). The endpoint+path-style config must be applied in **both** the initial build and the rebuild, or post-thaw uploads silently revert to AWS-default addressing. One shared helper avoids the drift.
- **ILM expiry replaces AWS lifecycle.** Without the terraform lifecycle rules, nothing prunes old session archives. A MinIO ILM rule (or a tiny `mc ilm` bootstrap) is needed to keep the single disk from filling — otherwise capacity creep is a slow-motion outage for postgres on the shared disk.
- **`gb image upload` + viewer proxy share the endpoint.** All four consumers must point at the same MinIO; a partial cutover (coop on MinIO, viewer still on AWS-default) would break image serving. Wire `COOP_S3_ENDPOINT` into every pod spec that sets `COOP_S3_BUCKET` (controller, beads-viewer, agents).
- **Region is cosmetic but required.** MinIO ignores region, but the SDK signs with one; keep `COOP_S3_REGION=us-east-1` (or any value) set so signing is deterministic.

---

## 8. Follow-up tasks to file

- **coop: add `COOP_S3_ENDPOINT` (+ path-style) override** — `config.rs` field + `build_aws_client`/`reload_credentials` shared helper using `force_path_style(true)`. *(blocks implementation)*
- **controller: add S3 endpoint override** — shared `newS3Client` helper with `BaseEndpoint`+`UsePathStyle`, called from `audit.go`, `image_proxy.go`, `image.go`; add `CoopS3Endpoint` to `config.go`.
- **chart: add gated `templates/minio/` (Deployment+Service+PVC+Secret)** on a `local-path` PVC, off by default; plus bucket-bootstrap Job (`mc mb`) and an ILM expiry rule.
- **chart: wire `COOP_S3_ENDPOINT` into controller, beads-viewer, and agent pod specs** (`podspec.go:616` adds one more `COOP_S3_*` var) + the in-box values overlay (`values-inbox.yaml`, shared with the storage axis).
- **secrets axis dependency (ka-0000004337):** provision the MinIO root creds as the in-box k8s secret surfaced as `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`.
- **docs: in-box object-store runbook** — bootstrap, ILM tuning, capacity/backup posture, "session archive is sacrificial vs postgres" DR note.
- **decision (legal): confirm MinIO AGPL-v3 server license is acceptable** for the in-box distribution model; SeaweedFS (Apache-2.0) is the documented fallback.
- **terraform: mark `modules/s3-sessions` AWS-POC-only / out of in-box runtime path** (no edits; it simply goes unused in-box).

---

## 9. References

- [AWS SDK for Rust — Configuring client endpoints](https://docs.aws.amazon.com/sdk-for-rust/latest/dg/endpoints.html)
- [aws-sdk-rust #1230 — `s3.addressing_style` not parsed from config (path-style must be code)](https://github.com/awslabs/aws-sdk-rust/issues/1230)
- [aws-sdk-rust #390 — How to enable path style for S3 services](https://github.com/awslabs/aws-sdk-rust/issues/390)
- Sibling storage axis report: `gardenio/docs/research/inbox-storage-localpv.md` (ka-0000005525)
- Code: `coop/crates/cli/src/s3.rs`, `coop/crates/cli/src/config.rs:225`, `gardenio/controller/cmd/gb/audit.go`, `gardenio/controller/cmd/beads-viewer/image_proxy.go`, `gardenio/controller/cmd/gb/image.go`, `gardenio/controller/internal/config/config.go:769`, `gardenio/controller/cmd/controller/podspec.go:616`, `gardenio/terraform/modules/s3-sessions/`
