# In-box object store runbook

Operator runbook for the optional **in-box object store** — an in-cluster,
S3-compatible store that replaces AWS S3 for coop session persistence
(transcripts, recordings, event logs) and the beads-viewer `/image` proxy
when gardenio is deployed self-contained ("in a box"), with no AWS account.

- **Epic:** ka-0000004955 (implementation), parent mega-epic ka-0000001295.
- **Research / decision:** `gardenio/docs/research/inbox-replace-s3-object-store.md`
  (decision ka-0000008110 resolved "file-children").
- **Status:** OFF BY DEFAULT. The S3-endpoint plumbing is merged and
  object-store-agnostic; the server image (MinIO vs SeaweedFS) is gated on a
  legal/license decision (see [Server image & license](#server-image--license)).

---

## 1. What it is

Default gardenio persists coop session artifacts to AWS S3. The in-box overlay
points the same code at an **in-cluster** S3-compatible endpoint instead, so a
self-hosted gardenio needs no AWS account or external object store.

Only **5 S3 verbs** are used across coop + controller (PutObject, GetObject,
HeadObject, HeadBucket, ListObjectsV2), so any S3-compatible store works. The
recommendation is a **single-node, single-drive** server backed by a
`local-path` PVC — no HA, no erasure coding. The session archive is
**sacrificial** (see [DR posture](#5-dr-posture-sacrificial-vs-postgres)).

### The path-style wrinkle

S3-compatible stores require **path-style** addressing
(`http://endpoint/bucket/key`) rather than AWS virtual-hosted style
(`http://bucket.endpoint/key`). The Rust AWS SDK **cannot** enable path-style
from env/config — it must be set in code (`force_path_style(true)`,
aws-sdk-rust #1230, #390). That is the one non-trivial code change; it is
merged in both coop (Rust) and the controller (Go).

---

## 2. Architecture

```
            COOP_S3_ENDPOINT (+ path-style)
                     │
   ┌─────────────────┼──────────────────┐
   │                 │                  │
 coop            controller         beads-viewer
 (Rust)            (Go gb)           (/image proxy)
   │                 │                  │
   └─────────────────┴──────────────────┘
                     │
            in-cluster object store
          (MinIO or SeaweedFS, 1 node)
                     │
              local-path PVC
```

Consumers and their endpoint hooks:

| Component | File | Hook |
|---|---|---|
| coop | `coop/crates/cli/src/s3.rs` | `build_aws_client` / `reload_credentials` apply `endpoint_url` + `force_path_style(true)` |
| coop config | `coop/crates/cli/src/config.rs` | `COOP_S3_ENDPOINT` (`s3_endpoint`) |
| controller `gb` | `gardenio/controller/cmd/gb/audit.go` | `newS3Client` / `s3EndpointOpts` (`BaseEndpoint` + `UsePathStyle`) |
| controller `gb image` | `gardenio/controller/cmd/gb/image.go` | via `newS3Client` |
| beads-viewer | `gardenio/controller/cmd/beads-viewer/image_proxy.go` | `imageProxyS3EndpointOpts` |
| controller config | `gardenio/controller/internal/config/config.go` | `CoopS3Endpoint` (`COOP_S3_ENDPOINT`) |
| agent pods | `gardenio/controller/cmd/controller/podspec.go` | injects `COOP_S3_ENDPOINT` into the pod env |

The endpoint is plumbed through Helm via `.Values.agents.s3.endpoint` into the
controller, beads-viewer, and (via the controller's podspec) agent pods.

---

## 3. Enabling it (values overlay)

The in-box overlay is **minimal and additive**. Set the endpoint and the
bucket; the credentials surface as `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`
from an in-box k8s secret (secrets axis ka-0000004337).

```yaml
# values-inbox.yaml (excerpt — the in-box overlay already sets these)
agents:
  s3:
    bucket: "gardenio-sessions"
    prefix: "coop/sessions"
    region: "us-east-1"               # cosmetic for MinIO; SDK signs with it
    endpoint: "http://gardenio-minio:9000"   # "<release>-minio" Service DNS

objectStore:
  enabled: true            # off by default; the in-box overlay flips this on
  bucket: "gardenio-sessions"          # MUST match agents.s3.bucket
  existingSecret: ""        # empty → chart default "<release>-minio-credentials"
  agentCredentials:
    existingSecret: ""      # empty → "<release>-minio-agent-credentials"
  persistence:
    size: 20Gi              # local-path PVC size (hard cap; ILM keeps it bounded)
    storageClass: "local-path"
  ilmExpiryDays: 30         # <=0 disables the expiry rule
```

> Root credentials live in the `<release>-minio-credentials` Secret (keys
> `accesskey`/`secretkey`), seeded create-if-missing by `scripts/deploy-byo.sh`
> via `openssl rand`. The chart wires them into MinIO, the bootstrap Job and the
> controller and beads-viewer Deployments as
> `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`. **Agent pods never get them**
> (gd-urSiLLwPIz): they get the least-privilege agent user below. For dev/test
> only, set `objectStore.rootPassword` to render an inline Secret instead.

### Agent credentials (least privilege)

Every agent pod, including crews running untrusted repo code, mounts the Secret
named by the controller's `COOP_S3_CREDENTIALS_SECRET`. That is
`<release>-minio-agent-credentials` (override:
`objectStore.agentCredentials.existingSecret`), keys `accesskey`/`secretkey`,
seeded create-if-missing by `scripts/deploy-byo.sh`.

On every install/upgrade the bootstrap Job (§4) turns it into a MinIO user with
the policy `objectStore.agentCredentials.policyName` (default
`gardenio-coop-agent`, rendered by `gardenio.minio.agentPolicy`):

| Allowed | On |
|---|---|
| `s3:ListBucket` | `<bucket>`, only for prefixes under `<agents.s3.prefix>/` |
| `s3:GetObject`, `s3:PutObject` (+ multipart abort/list parts) | `<bucket>/<agents.s3.prefix>/*` |
| `s3:GetBucketLocation` | `<bucket>` |

Everything else is denied: deletes, the bucket root and any other prefix, every
other bucket (Commons attachments included), and the admin API. coop's startup
`HeadBucket` is denied too; coop only logs `s3: bucket HEAD failed (may still
work)` and carries on, so that warning is expected.

The prefix is shared by all agents, so one agent can still read or overwrite
another agent's session objects (but not delete them). That is deliberate for
now: an agent legitimately restores an earlier agent's prefix when it adopts a
canonical `resume_s3_key`, and `gb audit`, `gb session death-report` and
`gb salvage restore --agent-bead` read other agents' sessions. Per-agent
scoping needs per-agent credentials; see the follow-up on gd-urSiLLwPIz.

Guards: the chart refuses to render when the agent Secret is the root Secret,
the bootstrap Job fails (rolling back an `--atomic` upgrade) when the agent
Secret is missing or carries the root access key, and the controller refuses
to mount any Secret whose name contains `minio-credentials` into an agent pod,
whether through `COOP_S3_CREDENTIALS_SECRET` or a project/role bead's
`secrets[]`.

**First rollout on an existing install.** Create the user *before* the
`helm upgrade`, or agent pods that start between the controller restart and the
post-upgrade bootstrap Job get credentials MinIO does not know yet and
cold-start without resuming. No secret value ever goes on a command line here
(local or in the pod): they travel through files and stdin only.

```sh
NS=gardenio R=gardenio
umask 077; T=$(mktemp -d)
# 1. Seed the agent Secret, create-if-missing, adoptable by Helm.
if ! kubectl -n $NS get secret $R-minio-agent-credentials >/dev/null 2>&1; then
  printf gardenio-coop-agent > "$T/accesskey"
  openssl rand -hex 24 | tr -d '\n' > "$T/secretkey"
  kubectl -n $NS create secret generic $R-minio-agent-credentials \
    --from-file=accesskey="$T/accesskey" --from-file=secretkey="$T/secretkey"
fi
kubectl -n $NS label secret $R-minio-agent-credentials app.kubernetes.io/managed-by=Helm --overwrite
kubectl -n $NS annotate secret $R-minio-agent-credentials \
  meta.helm.sh/release-name=$R meta.helm.sh/release-namespace=$NS --overwrite
# 2. Render the policy from the chart so it matches what the Job applies:
helm template $R gardenio/helm/gardenio -f <your-values.yaml> \
  -s templates/minio/bootstrap-job.yaml \
  | yq '.spec.template.spec.containers[0].command[2]' \
  | sed -n "/<<'POLICY'/,/^POLICY\$/p" | sed '1d;$d' > "$T/agent-policy.json"
jq . "$T/agent-policy.json" >/dev/null   # must be valid JSON
# 3. Create the policy and user inside the MinIO pod (its image has sh, cat and
#    mc but no grep). stdin carries: access key, secret key, then the policy.
POD=$(kubectl -n $NS get pod -l app.kubernetes.io/component=minio -o name | head -1)
{ kubectl -n $NS get secret $R-minio-agent-credentials -o jsonpath='{.data.accesskey}' | base64 -d; echo
  kubectl -n $NS get secret $R-minio-agent-credentials -o jsonpath='{.data.secretkey}' | base64 -d; echo
  cat "$T/agent-policy.json"
} | kubectl -n $NS exec -i "$POD" -- sh -c '
    set -e; export HOME=/tmp
    IFS= read -r AK; IFS= read -r SK; cat > /tmp/agent-policy.json
    export MC_HOST_local="http://$MINIO_ROOT_USER:$MINIO_ROOT_PASSWORD@127.0.0.1:9000"
    mc admin policy create local gardenio-coop-agent /tmp/agent-policy.json
    printf "%s\n" "$SK" | mc admin user add local "$AK"
    mc admin policy attach local gardenio-coop-agent --user "$AK" || true
    mc admin user info local "$AK"'
rm -rf "$T"
```

The last command must print `PolicyName: gardenio-coop-agent`. (`mc admin
user add` reads the secret key from stdin when it is not given; `printf` is a
shell builtin, so the key is in no process's argv.)

### Beads daemon credentials (read-only, opt-in)

The beads daemon's land-door evidence route (`GET
/v1/lands/{land}/decisions/{decision}/evidence/{image}`, kbeads
`landmap.Pictures`) serves the pictures `gb image upload` writes under
`<agents.s3.prefix>/images/`. With `objectStore.beadsCredentials.enabled: true`
(gd-k3GmLJKW8R) the bootstrap Job turns the Secret
`<release>-minio-beads-credentials` (override:
`objectStore.beadsCredentials.existingSecret`, keys `accesskey`/`secretkey`)
into a MinIO user with the policy `objectStore.beadsCredentials.policyName`
(default `gardenio-beads-evidence`, rendered by `gardenio.minio.beadsPolicy`):
`s3:GetObject` on `<bucket>/<agents.s3.prefix>/images/*` and nothing else (no
list, no put or delete, no other prefix or bucket, no admin API). The beads
Deployment reads the Secret as `AWS_ACCESS_KEY_ID`/`AWS_SECRET_ACCESS_KEY`
next to its `COOP_S3_*` env. Without credentials the daemon logs `decision
evidence pictures off` at startup and lists no evidence (#2040).

The Secret is not seeded by `deploy-byo.sh`: seed it as the agent one above
(access key `gardenio-beads-evidence`, `openssl rand -hex 24` secret, via
files, then the Helm ownership label and annotations), and on an existing
install create the user before the `helm upgrade` with the same `kubectl exec`
recipe, reading the policy from the `BEADSPOLICY` heredoc instead and using
`gardenio-beads-evidence` as the policy name. The chart refuses to render when
the beads Secret is the root or the agent Secret.

**Rotating the root keys.** Do it only after every agent pod started before the
switch is gone, because those pods still mount the root Secret and kubelet
would hand them the new value too:

```sh
# 1. No pod may still mount the root Secret as a volume (only agent pods ever
#    did; the platform Deployments read it via env). Expect no output; delete
#    any listed agent pod (it respawns with the agent Secret) and re-check:
kubectl -n $NS get pods -o json | jq -r --arg s "$R-minio-credentials" '.items[]
  | select(any(.spec.volumes[]?; .secret.secretName == $s)) | .metadata.name'
# 2. New root password; keep the access key so nothing else needs re-keying.
#    The value goes through a 0600 patch file, never argv:
umask 077; T=$(mktemp -d)
printf '{"stringData":{"secretkey":"%s"}}' "$(openssl rand -hex 24)" > "$T/patch.json"
kubectl -n $NS patch secret $R-minio-credentials --type merge --patch-file "$T/patch.json"
rm -rf "$T"
# 3. Restart every root-key consumer, MinIO first (it reads the root pair at
#    boot). The controller Deployment is <release>-agents:
kubectl -n $NS rollout restart deploy/$R-minio && kubectl -n $NS rollout status deploy/$R-minio
kubectl -n $NS rollout restart deploy/$R-agents deploy/$R-beads-viewer
```

Users and policies survive a root rotation (current MinIO releases do not
encrypt IAM data with the root credentials; checked on gd-urSiLLwPIz by
rotating a local server and re-using the agent user), so the agent user keeps
working. To rotate
the agent key, change `secretkey` in `<release>-minio-agent-credentials` and
re-run the bootstrap Job (`helm upgrade`): `mc admin user add` applies the new
key, and kubelet refreshes the file in running agent pods within about a minute.

> When the endpoint is unset (the default and all AWS-backed deployments),
> every consumer falls back to AWS S3 with virtual-hosted addressing —
> behaviour is unchanged. The overlay is the only thing that turns the in-box
> store on.

---

## 4. Bootstrap & ILM tuning

The object-store server template (gated, see §6) ships with:

- **Bucket-bootstrap Job** — a one-shot `mc mb` (make-bucket) that creates the
  session bucket idempotently after the server is ready. Re-running is a no-op.
- **ILM expiry rule** — an `mc ilm` lifecycle rule that expires objects under
  the session prefix after a TTL, so the sacrificial archive does not grow
  unbounded on a single local drive.

**Tuning the expiry TTL:** the default keeps recent sessions long enough for
resume + audit but bounds disk use. Raise it if you need a longer audit window
and have the disk; lower it on a tight local drive. The rule targets the
session prefix only — it does not touch any other bucket contents.

The bootstrap Job applies a whole-bucket expiry rule of `objectStore.ilmExpiryDays`
days (it removes any prior rule first, so the rule set stays at exactly one
expiry policy across upgrades). Set `ilmExpiryDays: 0` to skip the rule entirely
(bucket creation still runs). Tune by hand against a running server:

```sh
# inspect the configured lifecycle rule
mc ilm ls <alias>/<bucket>
# example: expire session objects after 30 days
mc ilm rule add <alias>/<bucket> --expire-days 30
```

---

## 5. DR posture: sacrificial vs postgres

**The session object store is sacrificial. Postgres (beads) is not.**

- **Postgres** holds the bead graph — the source of record. Back it up; treat
  data loss as an incident.
- **The object store** holds session transcripts/recordings — convenience
  artifacts for resume and after-the-fact inspection. On a single-node,
  single-drive, local-path PVC there is **no redundancy**: a node/disk loss
  loses the archive. That is an accepted trade-off for the in-box profile.

Implications:

- Losing the object store loses session-replay convenience, **not** any beads
  state. Agents recover from beads + git, not from S3.
- Do not put anything in this store whose loss would be an incident.
- If an operator needs durable session archives, point `agents.s3.endpoint` at
  a real/durable S3 (or a replicated store) instead — the plumbing is
  identical; only the backing store changes.

---

## 6. Server image & license

The S3-endpoint plumbing is object-store-agnostic. The **server image**
choice is a separate, gated decision:

- **MinIO** — the reference single-node S3-compatible server. Its server is
  **AGPL-v3**. We ship it OFF BY DEFAULT, in-cluster, with the operator
  self-hosting (not offered as a network service to third parties).
- **SeaweedFS** — Apache-2.0 fallback. Same plumbing, no AGPL exposure.

The AGPL-vs-Apache call was an operator/legal decision (ka-0000004293), **resolved
in favour of MinIO**: ship it OFF by default, in-cluster, operator-self-hosted
(not offered as a network service to third parties). The chart's
`templates/minio/` (Deployment + Service + PVC + Secret + bootstrap Job) is now
shipped and gated on `objectStore.enabled`. Switching to SeaweedFS would change
only the server template + the `objectStore` image values, not the endpoint
plumbing.

### Terraform note

`gardenio/terraform/modules/s3-sessions/` remains AWS-POC-only. In the in-box
runtime path it simply goes unused — no edits required; the in-cluster store
replaces it.

---

## 7. Quick verification

```sh
# 1. endpoint is injected into a consumer
kubectl -n gardenio exec deploy/gardenio-controller -- printenv COOP_S3_ENDPOINT

# 2. bucket exists (from a pod with mc configured, or the bootstrap Job log)
mc ls <alias>/

# 3. a fresh session uploads transcripts
mc ls <alias>/gardenio-sessions/coop/sessions/

# 4. beads-viewer /image proxy resolves against the in-box store
#    (open a bead with an uploaded image in the viewer; it should render,
#     not fall through to an access-denied link card)
```
