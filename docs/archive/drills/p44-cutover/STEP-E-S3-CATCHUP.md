# P4.4 Step-E preflight — S3 sessions catch-up before the COOP flip

> Companion to `step-e-repoint.sh` (the consumer re-point) and `STEP-D-GATE.md`
> (the DB cutover). Origin: task **ka-0000003458** (P4.2(b)), decision
> **ka-0000000392** = `sync`. This doc + `s3-sessions-catchup.sh` bake the
> "final replication catch-up" the migration runbook places at T-day
> (runbook edge O8) into a runnable, idempotent preflight.

## What P4.2(b) already did (the bulk copy)

Decision **ka-0000000392** approved a **one-time additive** copy of the coop
session store from the source bucket to the dest bucket, replacing live S3
replication entirely:

- **source** `s3://gardenio-sessions` (acct **909**) — read via the pod **IRSA**
  (`gardenio-agents-s3`; source objects are **SSE-S3**, so no KMS grant needed to
  read them).
- **dest** `s3://gardenio-sessions-prod` (acct **133**) — write via the
  **customer-poc AKIA** (user `gardenio`, advice **ka-0000007102**), which holds
  `kms:GenerateDataKey` on the dest CMK `48c85718` (alias
  `prod-gardenio-s3-sessions`, granted by acme-helm-chart MR !4875).
- **additive only** (`rclone copy`, never `sync`/`--delete*`): zero writes to
  909, and the dest's pre-existing `p44-cutover/` DB-transfer objects are
  preserved.
- volume at bulk-copy time: **~485k objects / ~34.6 GiB**.

## Why a catch-up is still needed

There is **no ongoing replication**. Until `COOP_S3_BUCKET` is re-pointed at the
dest bucket (the **agents** phase of `step-e-repoint.sh`), every live agent keeps
writing new session objects (transcripts, recordings, `meta.json`,
`state_events.jsonl`, `hook_events.jsonl`, `stdout-tail.txt`) to the **source**
bucket. Anything written after the bulk copy is missing from the dest until a
catch-up pass copies it. So the catch-up is the **last read-only-to-909
preflight before the flip**.

## The KMS-ETag gotcha (why a naive `aws s3 sync`/`rclone copy` fails)

The dest bucket **default-encrypts SSE-KMS**. For a KMS-encrypted object, S3
returns an **opaque ETag that is NOT the plaintext MD5**. A checksum-verifying
copier (rclone by default) compares the **source MD5** against the **dest
KMS-ETag** and false-flags **every** object `corrupted on transfer`, then
deletes its own upload and retries — net-zero progress. (Observed live on the
first P4.2(b) run: 0 objects landed until the run was aborted.)

**Fix, baked into `s3-sessions-catchup.sh`:** `--ignore-checksum` disables
rclone's broken client-side re-check. Integrity is **not** weakened — S3 still
validates the `Content-MD5` header on every PUT server-side (rejects real
corruption as `BadDigest`). Proven byte-identical afterward by downloading both
sides with aws-cli (dest object KMS-decrypted on GET) and `cmp`.

## When to run it

1. **T-minus (optional warm pass):** run once a few hours before the cut to
   shrink the final delta. Additive + idempotent, so it is always safe.
2. **REQUIRED — final pass at the flip:** run **immediately before** the
   `agents` phase of `step-e-repoint.sh` re-points `COOP_S3_BUCKET`. Because new
   writes only fully stop once the source agent fleet is frozen (step-E freezes
   actuators as part of step D and the fleet bring-up is the last micro-flip),
   the final catch-up should run **after the source fleet is quiesced** and
   **before** the dest fleet comes up on the new bucket, so the copied set is
   complete and stable.

## How to run

From the cutover driver pod (source cluster, acct 909, so the ambient IRSA is
the source-read cred):

```sh
# dest write cred = customer-poc AKIA (k8s secret infra-gardener-canonical-creds)
export DEST_AWS_ACCESS_KEY_ID="$(kubectl get secret infra-gardener-canonical-creds \
    -o jsonpath='{.data.AWS_ACCESS_KEY_ID}' | base64 -d)"
export DEST_AWS_SECRET_ACCESS_KEY="$(kubectl get secret infra-gardener-canonical-creds \
    -o jsonpath='{.data.AWS_SECRET_ACCESS_KEY}' | base64 -d)"

# dry-run first to see the pending delta (no writes)
DRY_RUN=1 gardenio/scripts/p44-cutover/s3-sessions-catchup.sh

# real additive catch-up + a 3-object byte-verify
gardenio/scripts/p44-cutover/s3-sessions-catchup.sh
```

`rclone` must be on `PATH` (or set `RCLONE_BIN`). The script builds its own
ephemeral rclone config (src=IRSA `env_auth`, dst=explicit AKIA), so no
persistent rclone config is required.

## What it guarantees / does not

- **Guarantees:** every source object present at run time exists in the dest,
  byte-identical (sampled); the dest keeps everything it already had; the 909
  account is never written.
- **Does NOT** stop source writers, flip `COOP_S3_BUCKET`, or delete anything.
  Those remain explicit, separately-gated step-E actions. This script only makes
  the dest object-complete so the flip is safe.

## Rollback

None needed — the script is additive and read-only against 909. If the flip is
aborted, the dest simply carries a superset of the source; nothing to undo. The
legacy source bucket is retained 90 days (runbook), so a re-run can always
re-close any gap.
