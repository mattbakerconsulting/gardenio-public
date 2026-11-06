# S3 session resume — operator runbook

**Bead:** ka-0000004448 (G10)
**Research:** `docs/research/s3-resume-e2e.md` (archived — excluded from this export)

## What this is

When an agent pod restarts, prior conversation state is rehydrated from a
combination of the agent's workspace PVC and an S3 archive at
`s3://<COOP_S3_BUCKET>/<COOP_S3_PREFIX>/<agent-bead-id>/<spawn-uuid>/`.
This runbook covers three operator scenarios: confirming a resume actually
happened, removing a stuck/poisoned S3 prefix, and triaging the case where
the session bead reports `resumed=true` but the agent appears to have
forgotten everything.

The full contract — what is checkpointed, who reads what on startup, and
the load-bearing invariants — is in `docs/research/s3-resume-e2e.md`
(archived — excluded from this export). Read §2 ("The resume contract") and
§2.3 ("Guarantees by scenario") before making any non-trivial change to a
stuck agent.

## Key facts

- **PVC is canonical for the Claude conversation transcript.** S3 carries
  a copy for observability + audit. If the PVC is gone, the S3
  `transcripts/` chunks are **not** automatically replayed back into
  Claude — see research §2.2.
- **`resumed=true` on the session bead means observable resume input was
  proven to exist** at registration time: either a local resume log of
  ≥ 256 bytes (`localResumeLogLoadable`) or a non-empty S3 prefix
  (`s3ResumeSessionExists` — 5s `ListObjectsV2`, `MaxKeys=1`). It does
  **not** by itself prove that Claude rehydrated a non-empty conversation.
  See research §2.4 invariants 3 and 4 and §3.5 "Resume after PVC GC"
  for the canonical "blank but resumed=true" trap.
- The S3 prefix is keyed by **agent bead ID**, not pod / coop session
  UUID. Concurrent or sequential pod spawns for the same agent share the
  bead-scoped prefix and are distinguished by the per-spawn UUID
  underneath.

## 1. Verify a session resumed correctly

Use this when an operator needs to confirm that a freshly-restarted agent
actually picked up where the previous spawn left off.

### 1.1 Check the session bead

```bash
# Resolve the agent bead id (e.g. from the Slack thread or `gb ready`).
AGENT=ka-0000001713

# Find the most recent open session bead for that agent.
kd list --type=session --status=open | grep "$AGENT"

# Inspect the resumed flag and session log path.
kd show <session-bead-id> --json | jq '.fields | {resumed, session_log, started_at}'
```

What to expect:

| Field | Fresh start | Resumed (PVC) | Resumed (S3 only) |
|---|---|---|---|
| `resumed` | `false` | `true` | `true` |
| `session_log` | `""` | local path (`/home/agent/.claude/projects/.../<uuid>.jsonl`) | `s3:<session-id>` |

If `resumed=false` on a pod restart, the agent will be starting cold. That
is correct behavior when the PVC was lost AND no S3 archive exists for
that bead — but it's also what you see when the PVC walk failed to find
a candidate transcript. Continue to §1.2.

### 1.2 Inspect the S3 archive

Requires `COOP_S3_BUCKET` and AWS credentials with read access.

```bash
# Bucket / prefix come from the controller env. On the agent pod:
kubectl exec -n <ns> <agent-pod> -- env | grep '^COOP_S3_'

# Or from the controller deployment:
kubectl get deploy -n <ns> <release>-controller -o yaml \
  | yq '.spec.template.spec.containers[0].env[] | select(.name == "COOP_S3_BUCKET" or .name == "COOP_S3_PREFIX" or .name == "COOP_S3_REGION")'

export COOP_S3_BUCKET=...
export COOP_S3_PREFIX=coop/sessions   # default; override only if your env sets one

# List spawns under the agent's bead-scoped prefix.
aws s3 ls "s3://${COOP_S3_BUCKET}/${COOP_S3_PREFIX}/${AGENT}/"
```

You should see one sub-prefix per spawn (each a coop session UUID),
e.g.:

```
                           PRE 0f6b2c4a-…/
                           PRE 91eaa9f8-…/
                           PRE c3d2…/
```

For each spawn, the latest one (by `meta.json.started_at`) is what
`pick_latest_spawn` would resume from. Inspect:

```bash
SPAWN=0f6b2c4a-...
aws s3 ls "s3://${COOP_S3_BUCKET}/${COOP_S3_PREFIX}/${AGENT}/${SPAWN}/"
aws s3 cp "s3://${COOP_S3_BUCKET}/${COOP_S3_PREFIX}/${AGENT}/${SPAWN}/meta.json" -
```

A healthy spawn has at minimum `meta.json` and `session.jsonl`; a session
that emitted Claude turns will also have `transcripts/0.jsonl` (and
higher-numbered chunks).

### 1.3 Use `gb audit` for a higher-level view

`gb audit` is the read-only consumer of the same archive (`gardenio/controller/cmd/gb/audit.go`).
On any host with the same `COOP_S3_*` env and AWS credentials:

```bash
gb audit list --since 24h --agent <agent-name>
gb audit fetch <session-id>
```

`gb audit list` filters by `--agent`, `--project`, `--role`, `--session-id`
and surfaces metadata from `meta.json`. `gb audit fetch` downloads the
spawn artifacts to a local directory for inspection.

### 1.4 Cross-check against the agent log

The controller logs `[gb agent start] session bead registered: <id>` on
every restart. Look for the resume decision around the same line:

```bash
kubectl logs -n <ns> <agent-pod> -c agent | grep -E 'session bead registered|resume|COOP_S3'
```

If you see `session bead registered` but no S3 / resume log lines, the
restart was a fresh start — confirmed by the same `resumed=false` you
read from the bead.

## 2. Delete a stuck S3 prefix

Use this when an S3 archive is corrupting the resume picker — common
cases: a prior spawn wrote a meta with a future `started_at`, an orphan
upload from a never-finished spawn keeps confusing diagnostics, or a test
fixture left a stale archive that's poisoning fresh agents that reuse the
bead ID. See research §G3 / §G12
for the underlying issues; there is no automated cleanup today.

### 2.1 Stop the agent first

If the agent is still running, deletes mid-flight will race the live
uploader and produce a half-deleted prefix that's worse than the
original. Kill the agent first:

```bash
# Polite stop: closes session bead and triggers despawn.
gb agent kill <agent-bead-id>

# Confirm no agent pods remain for that bead.
kubectl get pods -n <ns> -l boat.agent=<agent-bead-id>
```

For a stuck pod that won't drain, force-delete:

```bash
kubectl delete pod -n <ns> <agent-pod> --grace-period=0 --force
```

### 2.2 Identify the prefix

```bash
export COOP_S3_BUCKET=...
export COOP_S3_PREFIX=coop/sessions
AGENT=ka-0000001713

# Whole bead (every spawn ever):
TARGET="s3://${COOP_S3_BUCKET}/${COOP_S3_PREFIX}/${AGENT}/"

# A single bad spawn within an otherwise-healthy bead:
SPAWN=<coop-session-uuid>
TARGET="s3://${COOP_S3_BUCKET}/${COOP_S3_PREFIX}/${AGENT}/${SPAWN}/"

# Print before deleting — sanity-check the scope.
aws s3 ls "$TARGET" --recursive | head -30
aws s3 ls "$TARGET" --recursive --summarize | tail -5   # total objects + bytes
```

### 2.3 Snapshot before deleting

This is irreversible. If the prefix is interesting (e.g. you're deleting
to recover a hung agent but the operator may want forensics), copy it
out first:

```bash
aws s3 cp "$TARGET" "/tmp/s3-resume-snapshot-${AGENT}-$(date +%s)/" --recursive
```

### 2.4 Delete

```bash
aws s3 rm "$TARGET" --recursive
```

If versioning is enabled on the bucket, `rm --recursive` only removes
current versions. List delete-markers and old versions explicitly with
`aws s3api list-object-versions ... --prefix "$AGENT/"` and remove them
via `delete-objects` if you need the prefix truly gone.

### 2.5 Clear the agent bead's `last_session_id`

If `last_session_id` on the agent bead points at the spawn you just
deleted, the next pod will set `COOP_S3_RESUME_SESSION` to a non-existent
ID. Today coop tolerates this (returns `None` from `pick_restore_key` and
starts fresh), but it's tidier to clear it:

```bash
kd update <agent-bead-id> --unset-field last_session_id
```

### 2.6 Restart

```bash
gb spawn --task <task-id> --role <role>
# or, if the agent bead still exists and is in a respawn-eligible state:
gb agent respawn <agent-bead-id>
```

Verify per §1.

## 3. `resumed=true` but the conversation appears blank

This is the ka-0000005950 / ka-0000004404 class of bug in disguise. The
session bead is honest — it ran the ground-truth checks
(`session_resume_check.go`) — but the resume input it found does not
contain what the operator expected. The resume runbook focuses on the
three real ways this can happen.

### 3.1 Triage tree

```text
resumed=true on the session bead, conversation looks blank in Slack / Claude
│
├─ session_log starts with "s3:..."  (S3-only resume path)
│   ├─ S3 archive existed but PVC was gone  → see §3.2 (PVC GC)
│   └─ S3 archive belonged to a stale fixture / wrong bead → see §3.4
│
├─ session_log is a local path under ~/.claude/projects/
│   ├─ Local jsonl is ≥ 256 bytes but only contains init events  → see §3.3
│   └─ Local jsonl IS the right transcript but the UI shows blank  → see §3.5
│
└─ session_log is empty but resumed=true
    └─ Bug — file new bead, attach `kubectl logs` of the agent pod
       and the session bead JSON. resumed=true requires a non-empty
       session_log per the contract.
```

### 3.2 PVC was GC'd; S3 alone is not enough to rehydrate Claude

This is research §3.5, the
most common confusion. Symptoms:

- Session bead has `resumed=true` and `session_log` starts with `s3:`.
- S3 prefix is populated; `aws s3 ls` shows recent transcripts.
- The agent's actual `~/.claude/projects/` is empty or has only stub
  files.
- Claude UI / Slack thread shows a fresh "Hi, what would you like me to
  do?" with no memory of prior turns.

**This is working as designed today.** The S3 archive is observability,
not a Claude-resume input. coop's own bookkeeping rehydrates from S3 (so
the spawn UUID stays consistent, recordings continue numbering, etc.),
but Claude itself starts cold because no local jsonl exists to pass to
`claude --resume`.

What the operator can do:

1. Confirm via `kubectl exec <pod> -- ls ~/.claude/projects/` — empty or
   nearly so.
2. Inspect `kubectl describe pod` for the workspace PVC; if it's a
   freshly-bound PVC (not the one the prior spawn used), the previous
   PVC was reclaimed.
3. There is no in-product recovery: the conversation transcript on the
   prior PVC is gone. Communicate this to the requester. The S3
   `transcripts/` chunks are human-readable JSONL and can be downloaded
   for forensics, but cannot be fed back into a live Claude session.
4. File the operational miss — was the PVC supposed to be retained?
   Check `helm/gardenio/values.yaml` workspace storage settings and the
   PVC's `reclaimPolicy`.

If a "PVC-gone S3-only" rehydration path is desirable as a feature,
that's the open question raised in
research §7 question 3 — file
under the umbrella epic ka-0000003664.

### 3.3 Local jsonl ≥ 256 bytes but content-empty

`localResumeLogLoadable` thresholds at 256 bytes — see
research §2.4 invariants 1 and 3.
A jsonl with only init / metadata lines can clear that and still carry
zero user turns.

How to confirm:

```bash
kubectl exec -n <ns> <agent-pod> -- \
  cat "$(kubectl exec -n <ns> <agent-pod> -- find /home/agent/.claude/projects -name '*.jsonl' -size +256c | head -1)" \
  | head -20
```

If you see only `type:summary` / `type:meta` / system events and no
`type:user` or `type:assistant` lines, the resume was technically
correct but there was nothing to actually resume. Treat as fresh start.
A future fix (research §G — bump the threshold or count user turns
before claiming `resumed`) is captured in the gap list; cite §2.4 inv. 3
when filing.

### 3.4 Wrong S3 archive — fixture / test bead collision

Symptoms: a freshly-spawned agent (e.g. for a new task) reports
`resumed=true` from a `s3:` log, but the conversation it "remembers"
belongs to a completely different prior task.

This is research §G9 / §G12:
the bead ID was reused (a recycled fixture seed, a manually-recreated
agent under the same `KD_AGENT_ID`, or a cross-shard ID collision) and
the old archive is still in S3 under that bead.

Fix:

1. Confirm via `aws s3 ls s3://${BUCKET}/${PREFIX}/${BEAD}/` and inspect
   any spawn's `meta.json` — `started_at` and `agent_name` will reveal
   the prior identity.
2. Delete the stale prefix per §2 (snapshot first).
3. If this is a recurring fixture issue, file a bug under the umbrella
   epic referencing G9 — convention is not enforcement.

### 3.5 Local jsonl is correct but the UI shows blank

Rare. The transcript file exists, has user/assistant turns, and is large
enough — yet Slack or `coop peek` shows blank.

Verify in order:

```bash
# 1. Is Claude actually running?
kubectl exec -n <ns> <agent-pod> -- pgrep -fa claude

# 2. Is coop binding to the right session?
kubectl exec -n <ns> <agent-pod> -- coop status

# 3. Did stale-PID cleanup misfire? Look for an active PID file pointing
#    at a process that DOES exist.
kubectl exec -n <ns> <agent-pod> -- ls -la /home/agent/.claude/sessions/

# 4. Pull the agent log for the resume decision.
kubectl logs -n <ns> <agent-pod> -c agent | grep -E 'findResumeSession|--resume|stale'
```

If `findResumeSession` rejected the candidate (e.g. ≥ 3 stale neighbors
in the past hour — research §2.4 inv. 7),
the log will show the reject reason. Resolve by clearing the stale
files:

```bash
kubectl exec -n <ns> <agent-pod> -- \
  find /home/agent/.claude/projects -name '*.jsonl.stale' -mmin -60 -delete
```

Then `gb agent respawn <bead>` and re-verify per §1.

## See also

- `docs/research/s3-resume-e2e.md` (archived — excluded from this export)
  — the resume contract, sequence diagrams, gap list (G1–G14).
- [`gardenio/docs/user-guide/11-e2e-testing.md`](../user-guide/11-e2e-testing.md)
  — E2E tests covering session resume:
  `test-session-resume.sh`, `test-session-archival.sh`,
  `test-thread-respawn-resume.sh`, `test-cred-rotation-resume.sh`.
- `gardenio/controller/cmd/gb/session_resume_check.go` — the ground-truth
  check that powers `resumed=true/false`.
- `gardenio/controller/cmd/gb/init_agent_loop.go` — restart loop, env-var
  pinning (`COOP_S3_SESSION_KEY`).
- `coop/crates/cli/src/s3.rs` — coop's S3 reader / writer; resolution
  rules in `resolve_session_key`, `compose_session_key`,
  `pick_restore_key`.
