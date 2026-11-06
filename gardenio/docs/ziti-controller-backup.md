# Backing up the OpenZiti controller, and restoring it

> **Opt-in only.** This applies only when the ziti overlay is on
> (`ziti.enabled=true`, `gardenio/helm/ziti`, off by default since
> gd-xNMLrsgajy). The Vultr controller it describes is kept running, unchanged,
> by owner decision 2026-10-13, and nothing on gardenio's default path dials
> it: see [`../helm/ziti/README.md`](../helm/ziti/README.md) "Where the ziti
> controller lives".

Written 2026-09-19 for `gd-rn7innedGW`, Phase 0 of the move off the Vultr
quickstart box (epic `gd-Vn8uWTbQ9S`). Companion to `ziti-routers.md`, which
describes the controller this protects, and to the `hq-host.md` backup section,
whose conventions this follows.

**Do this before the migration, not after.** The controller's PKI holds the
root and intermediate CA private keys that signed all eight enrolled
identities. Lose it and there is no recovery short of re-enrolling every
identity by hand — the laptops, the phone, `gardenio-hq`, and the two
in-cluster host identities that the overlay to k3s depends on.

## What the controller's state actually is

The box runs `ziti edge quickstart --home /home/claude/ziti`, which in 2.0.x is
an HA/raft controller plus a router in one process. Under the home:

| Path | What it is | Losing it costs |
| --- | --- | --- |
| `pki/root-ca/keys/root-ca.key` | the root CA **private key** | every identity re-enrols |
| `pki/intermediate-ca-*/keys/` | intermediate CA key + the server/client keys it issued | the controller cannot serve its own API |
| `instance-*/raft/ctrl-ha.db` | the bolt state machine — identities, services, policies, configs | the overlay's entire configuration |
| `instance-*/raft/raft.db`, `raft/snapshots/` | the raft log and its snapshots | nothing on its own; meaningful only with the DB above |
| `instance-*/ctrl.yaml` | controller config, **with absolute paths** (see gotchas) | rebuildable, but fiddly |
| `instance-*/router-*.{yaml,cert,key,cas}` | the co-located router's enrolment | that router re-enrols |
| `instance-*/db/` | the non-HA bolt path. **Empty on a raft controller** | nothing |

About 4 MB in total. Size has never been the problem — the bundle
(`gd-8brYjTBHIu`) measured 3.6 MB on the live box. Node-locality and the CA
keys are the problem.

## The script

`gardenio/scripts/ziti-ctrl-backup`:

```
ziti-ctrl-backup snapshot [--home DIR] [--out FILE] [--offline]   # on the box
ziti-ctrl-backup pull [--host vultr] [--s3 URI] [--keep DIR]      # from the laptop
ziti-ctrl-backup verify ARCHIVE
ziti-ctrl-backup restore ARCHIVE DEST
```

**It pulls, it does not push.** The Vultr box is not in AWS and holds no AWS
credential. Giving it one — an IAM user access key on disk, forever — is a
worse trade than running the backup from the laptop, where the operator's
credential already lives. It is also the box whose process table already leaks
the ziti admin password in `argv`; it should be gaining fewer secrets, not
more. `pull` ships the script to the box over `ssh` (`bash -s`), so there is no
second copy on the box to drift.

**Consistency.** The live `ctrl-ha.db` is a bolt file being written to; a plain
`tar` of it can tear. So `snapshot` first calls `ziti edge db snapshot`, which
asks the controller to write a consistent copy alongside as
`raft/ctrl-ha.db-<YYYYMMDD>-<HHMMSS>`. That member — never the live file — is
what `restore` promotes. `--offline` is the other option: with the controller
stopped, the whole `raft/` set is coherent as taken. For a 4 MB home the
offline window is seconds, and it is the cleaner choice for the one
pre-migration backup.

**`--offline` verifies quiescence, not a process name.** The original guard
was `pgrep -f 'ziti (edge quickstart|controller run)'`, which only recognizes
the two launch forms its author knew about — a controller started any other
way (a systemd unit, a container ENTRYPOINT, e.g. the in-cluster
`openziti/ziti-controller` image) was invisible to it, and "no pgrep match"
would read as "stopped" and silently tear a live backup. `--offline` instead
checks whether anything has the raft data files open (`lsof +D`, or `fuser`
if `lsof` is absent) and **fails closed** — refuses — if neither tool is
available to check. Online mode (the default) is unaffected.

**Logical export, alongside the file copy.** Every online `snapshot` also
runs `ziti edge list <identities|services|configs|service-policies|
edge-router-policies> -j` and adds the JSON under `export/` in the tar. The
file-level `raft/` copy can in principle be torn even with the consistency
step above; the logical export is a live read through the controller's own
API and is what is actually restorable at 3am. It only runs online (it needs
a logged-in `ziti` session against a live controller) — an `--offline`
archive has no `export/`, and `verify` says so rather than assuming. A
per-entity export failure is a warning, not a hard failure: the backup still
has the file-level copy either way. `restore` carries `export/` into the new
home (`DEST/export/`) for manual reference if a rebuild from bolt state is
ever needed.

**Encryption.** The archive is a CA private key. `pull` refuses to upload
without a passphrase from the SSM SecureString `/gardenio/ziti/backup-passphrase`
and encrypts with `openssl enc -aes-256-cbc -pbkdf2 -iter 600000` before the
upload. Plaintext lives only in a 0700 `mktemp -d` on the puller, removed on
exit. The sha256 of the *plaintext* is uploaded beside the ciphertext so a
restore can prove it decrypted to the bytes that were verified.

**Where it lands.** `s3://gardenio-hq-backups-123456789012/ziti-ctrl/` — the
same bucket as the Dolt backups (`hq-host.md`): SSE-S3, versioning on, all
public access blocked. Two properties that matter here and were checked against
`terraform/modules/hq/main.tf`:

- The prefix expiry rules are `dolt/` and `k3s-dolt/` only, so nothing expires
  a controller backup at 90 days. Only the bucket-wide noncurrent-version rule
  (30 days) applies.
- The HQ instance role is scoped to `dolt/*`. It cannot read `ziti-ctrl/*`.
  A compromise of the HQ host does not hand over the overlay's CA.

## Restore mechanism

**The promote-and-drop-the-log approach below (used through 2026-09-19,
including the "Verified restore" drill that used to head this section) was
wrong, and its "Verified" label was checked against the wrong signal**
(`gd-3AhYZKV7yH`, fixed here and, for the in-cluster path, in
`gardenio/helm/ziti/templates/controller.yaml` commit `00ec9d0`, then at `gardenio/helm/ziti/controller.yaml`). Promoting the newest
`ctrl-ha.db-<ts>` snapshot member over the live `ctrl-ha.db` restores the FSM
(edge) database, but raft **membership** lives in `raft.db`, not in the FSM
bolt file, and the old restore dropped `raft.db` along with the rest of
`raft/`. The restored controller boots with zero known peers, logs "no known
peers, aborting election", and never elects a leader — every model write then
returns `CLUSTER_NO_LEADER`, even though a plain `edge list` against the
stale pre-restore state still works. That is exactly why the 2026-09-19 drill
below "passed": it checked `ziti edge list identities` (a read against
already-loaded state) and identity count/PKI survival, and never checked
`ziti agent cluster list` or attempted a write. Proven false on ziti 2.0.4,
2026-09-23.

**The fix**: stage the newest `ctrl-ha.db-<ts>` snapshot member OUTSIDE the
raft directory (`instance-*/import/ctrl-import.db`, a name that is not
`ctrl-ha.db-*`) and leave `raft/` completely empty. Restore also writes a
top-level `db: <path to staged file>` line into `ctrl.yaml`. This makes
**restoring a two-start operation**:

1. First start: with `raft/` empty and `db:` set, ziti 2.0.4 runs
   `RaftRestoreFromBoltDb`, imports the staged bolt file, bootstraps a fresh
   single-voter raft cluster, then **exits once** ("database restore requires
   controller restart. exiting..."). This is expected, not a crash.
2. Second start: `raft.db` now exists, so ziti ignores the leftover `db:`
   line (no code change needed) and comes up as leader on the imported state.

### Live run, 2026-09-19 — the real controller

Escalation `gd-YSm8n9hhED`, run from the laptop (which has `ssh vultr` and the
`gardenio` AWS profile). This is the live data; the drill below is kept for the
restore path it exercised.

```
$ aws --profile gardenio --region us-east-1 ssm put-parameter \
    --name /gardenio/ziti/backup-passphrase --type SecureString --no-overwrite
Version: 1                       # minted once; never echoed

$ gardenio/scripts/ziti-ctrl-backup pull --host vultr --keep <scratch>
snapshotting on vultr
asking the controller for a consistent db snapshot
snapshot member: instance-1/raft/ctrl-ha.db-20260919-201713
wrote ziti-ctrl-20260919T201713Z.tar.gz (524K),
  sha256 6da1cdb1cfbff2dc0ecd915ada56a5c66214332717ed7841d4c8112a8a2decea
pulled 524K, sha256 6da1cdb1cfbff2dc0ecd915ada56a5c66214332717ed7841d4c8112a8a2decea
uploaded s3://gardenio-hq-backups-123456789012/ziti-ctrl/ziti-ctrl-20260919T201712Z.tar.gz.enc

$ ziti-ctrl-backup verify <archive>      # run ON the box: it has the ziti binary
ok: ctrl-ha.db-20260919-201713 -- edge database version: 44, 8 identities
```

**8 identities**, as expected for the live fabric (the drill had 7). The
sha256 matched on both sides of the transfer, so the archive is intact.

The plaintext copy was shredded after verification — it holds the root and
intermediate CA PRIVATE KEYS. What remains in S3 is the encrypted object only.

Two defects surfaced on this first live run and are fixed:
- `pull` had no way to pass `ZITI_BIN` through to the remote, and a
  NON-INTERACTIVE ssh does not source the profile that puts `~/.local/bin` on
  PATH — so the box's ziti was invisible and `snapshot` died telling the
  operator to "set ZITI_BIN", advice that could not actually be followed.
  `pull` now forwards `ZITI_REMOTE_BIN` (absolute path on the box), kept
  SEPARATE from the local `ZITI_BIN` that `verify` uses, because one variable
  for both means fixing the remote silently degrades the local verify to
  structure-only.
- the EXIT trap used `$tmp` under `set -u`, so it aborted with
  "tmp: unbound variable" on top of the real error and obscured it. Now
  `${tmp:-}`.
- the structure-only verify path now says PARTIAL loudly. It had read as a
  pass ("ok: structure only"), which is exactly the wrong impression for a
  backup you have not actually read back.

### Drill, 2026-09-23 — real two-start restore, real ziti 2.0.4

Not against the live Vultr box — the crew pod has neither `ssh vultr` nor an
AWS credential — but against a real `ziti-linux-amd64-2.0.4` binary (downloaded
directly from GitHub releases) driving a real HA/raft quickstart controller of
the same shape, seeded with a couple of identities and the default policies.
This exercises exactly the code path `cmd_restore` now takes, checked against
the acceptance criteria in `gd-E1Y1hDmuks`: `ziti agent cluster list` shows a
voter and a leader, a write (create then delete an edge router) actually
persists, and `ziti edge login` works.

```
$ ziti-ctrl-backup snapshot --home .../src --out backup.tar.gz
asking the controller for a consistent db snapshot
snapshot member: instance-1/raft/ctrl-ha.db-20260923-214938
logical export: identities 4/4, edge-router-policies 2/2
wrote backup.tar.gz (88K), sha256 469a98d57262ef27b75f45cd88aa6602bdf726c5dcc074298243e5aac0cd24d6

# controller stopped
$ ziti-ctrl-backup restore backup.tar.gz .../restored
ok: ctrl-ha.db-20260923-214938 -- edge database version: 44, 4 identities
staging ctrl-ha.db-20260923-214938 for import; raft starts empty (two-start restore)
rewriting absolute paths: .../src -> .../restored
wrote 'db: .../restored/instance-1/import/ctrl-import.db' to .../restored/instance-1/ctrl.yaml -- FIRST start only, ziti drops it once raft.db exists
restored to .../restored -- THIS IS A TWO-START RESTORE: ...

$ ziti controller run .../restored/instance-1/ctrl.yaml     # 1st start
... "all servers running"
{"msg":"database restore requires controller restart. exiting...","time":"2026-09-23T21:50:03.209Z"}
$ echo $?
0

$ ziti controller run .../restored/instance-1/ctrl.yaml &   # 2nd start
{"msg":"initial configuration","servers":"[[{Suffrage:Voter ID:instance-1 Address:tls:localhost:18500}]]", ...}
{"msg":"entering leader state", ...}

$ ziti agent cluster list
╭────────────┬─────────────────────┬───────┬────────┬─────────┬───────────┬───────────╮
│ ID         │ ADDRESS             │ VOTER │ LEADER │ VERSION │ CONNECTED │ PREFERRED │
├────────────┼─────────────────────┼───────┼────────┼─────────┼───────────┼───────────┤
│ instance-1 │ tls:localhost:18500 │ true  │ true   │ v2.0.4  │ true      │ false     │
╰────────────┴─────────────────────┴───────┴────────┴─────────┴───────────┴───────────╯

$ ziti edge login localhost:18500 -u admin -p ... -y
Saving identity 'default' to ~/.config/ziti/ziti-cli.json
$ ziti edge list identities
results: 1-4 of 4     # acceptance-id-1, acceptance-id-2, admin, router-instance-1 -- all survived

$ ziti edge create edge-router acceptance-router-11   # 11th attempt; see catch-up window below
New edge router acceptance-router-11 created with id: YjYz7Fxru
$ ziti edge list edge-routers 'id="YjYz7Fxru"'
results: 1-1 of 1     # persisted

$ ziti edge delete edge-router acceptance-router-11
delete of edge-router with id YjYz7Fxru: OK
$ ziti edge list edge-routers
results: 1-1 of 1     # only router-instance-1 left -- create-then-delete round-tripped
```

Also checked, same as the old drill: `sha256sum pki/root-ca/keys/root-ca.key`
byte-identical before and after, and login succeeds against a certificate the
restored PKI signed.

**Why a two-start restart is right, and what the old fix got wrong.** A
single raft log whose applied index is ahead of the state machine is the
exact failure the old approach produced (leaderless raft) by dropping
membership instead of rebuilding it. `RaftRestoreFromBoltDb` is ziti's own
built-in mechanism for this: import the bolt state, bootstrap a *fresh*
single-voter log around it, then force a restart so the running process
picks up the new log rather than continuing on stale in-memory raft state.
Proven above: `ziti agent cluster list` shows a real leader, not just a
process that stayed up.

**Gotcha: a short post-restore write catch-up window.** The imported bolt
file carries over the *last-applied-index* watermark from the original
cluster's history. The freshly bootstrapped raft log starts counting from 1
again, so the first several commands submitted against the restored
controller can land at log indices at or below that old watermark — the FSM
logs `"msg":"skipping replay of command"` and returns a real-looking 201 with
a generated ID, but the write **never persists** (immediate 404 on GET,
absent from subsequent lists). This is bounded and self-resolving: once
enough commands have been submitted to advance the new log's index past the
old watermark, every write persists normally and permanently (confirmed
above and by a 40-iteration follow-up loop with zero further skips after the
catch-up point). In this drill the watermark took **10 skipped creates**
before the 11th one (`acceptance-router-11`) persisted; the exact count
depends on how much raft history the original cluster had accumulated before
backup. **Practical implication: after a restore, don't trust a bare 201 —
verify a test write with a follow-up `ziti edge list` (or GET on its ID) and
retry if it did not take**, same guidance `cmd_restore`'s own trailing log
output now gives.

## Restoring for real

Do not restore over a live home. Build a new one and switch — the same rule as
the HQ city restore.

```sh
# 0. Get the archive and decrypt it, in a 0700 scratch dir.
umask 077; mkdir -p ~/ziti-restore && cd ~/ziti-restore
aws --profile gardenio s3 cp s3://gardenio-hq-backups-123456789012/ziti-ctrl/ziti-ctrl-<ts>.tar.gz.enc .
aws --profile gardenio ssm get-parameter --name /gardenio/ziti/backup-passphrase \
  --with-decryption --query Parameter.Value --output text \
  | openssl enc -d -aes-256-cbc -pbkdf2 -iter 600000 \
      -in ziti-ctrl-<ts>.tar.gz.enc -out ziti-ctrl-<ts>.tar.gz -pass stdin
# the .sha256 beside the object is the plaintext's; check it.
sha256sum -c <(aws --profile gardenio s3 cp s3://.../ziti-ctrl/ziti-ctrl-<ts>.tar.gz.enc.sha256 -)

# 1. Structure + bolt read-back before trusting it.
gardenio/scripts/ziti-ctrl-backup verify ziti-ctrl-<ts>.tar.gz

# 2. Restore into a NEW home, then stop the old controller and switch.
gardenio/scripts/ziti-ctrl-backup restore ziti-ctrl-<ts>.tar.gz /home/claude/ziti.restored
#   on the box: stop the quickstart process first, then
#   mv /home/claude/ziti /home/claude/ziti.pre-restore.$(date -u +%Y%m%dT%H%M%SZ)
#   mv /home/claude/ziti.restored /home/claude/ziti
#   -- restoring to the SAME path is what keeps ctrl.yaml's absolute paths honest.

# 3. THIS IS A TWO-START RESTORE if the archive had a snapshot member to
#    import (cmd_restore says so; look for "wrote 'db: ...' to ctrl.yaml").
ziti controller run /home/claude/ziti/instance-1/ctrl.yaml
#   1st start: imports the staged bolt file, bootstraps a single-voter raft
#   cluster, then EXITS ONCE ("database restore requires controller
#   restart") -- this is expected, restart it:
ziti controller run /home/claude/ziti/instance-1/ctrl.yaml &
#   2nd start: comes up as leader on the imported state.

# 4. Check leadership, not just that the process stayed up.
ziti agent cluster list                                # expect 1 voter + a leader
ziti edge login <ctrl-address>:8440 -u admin && ziti edge list identities   # expect 8
ziti edge list edge-routers   # router-instance-1 and router-aws-1 ONLINE

# 5. Verify a WRITE actually persists before trusting the restore -- the
#    first few writes after a restore can silently no-op (real 201, no
#    persistence) while raft's new log catches up past the imported
#    snapshot's old last-applied-index watermark; see "Gotcha" above.
#    Retry a create + list a handful of times if the first one doesn't stick.
ziti edge create edge-router restore-check && ziti edge list edge-routers 'name="restore-check"'
ziti edge delete edge-router restore-check
```

A restored controller keeps the same CA, so **enrolled identities do not have
to be re-enrolled** — that is the entire point of backing up `pki/`. What an
identity still pins is the *address* (`ztAPI`), which is bite 1 of the
migration and a separate problem from this one.

## In-cluster backups (the k3s controller)

Everything above is the Vultr-box mechanism: operator-driven, over ssh, into
`s3://gardenio-hq-backups-123456789012/ziti-ctrl/`. The in-cluster controller
(`gardenio/helm/ziti/templates/controller.yaml`, `gd-fY6ipET226`) has its own,
self-contained version of the same idea, running as sidecars in the
controller's own pod rather than a separate script someone has to remember to
run. This is on its SECOND attempt — the first (PR #7, commit `c616795`) was
applied live and reverted the same day (commit `0f158ff`); what's described
below is the fixed version. See `docs/ziti-controller-k3s.md`'s "Durable
storage: applied, reverted, fixed" for the full incident writeup.

- `ziti-snapshot` calls `ziti edge db snapshot` every 15 minutes over
  `localhost:8440` (same pod, same network namespace).
- `s3-backup` tars the whole `ZITI_HOME`, encrypts it with the same
  `openssl enc -aes-256-cbc -pbkdf2 -iter 600000` scheme used above, and
  uploads it every 15 minutes to the **SAME** prefix the Vultr-box `pull`
  path uses, `s3://gardenio-hq-backups-123456789012/ziti-ctrl/` (the
  reverted first attempt used a different, wrong prefix nested under
  `k3s-dolt/` — fixed). Reachable via the k3s node role's
  `extra_backup_prefixes = ["ziti-ctrl"]` IAM grant (`terraform/modules/k3s`,
  commit `bd49105`), so no terraform change was needed for this file. Uses
  the same object-naming convention as `ziti-ctrl-backup pull` too:
  `<name>.tar.gz.enc` for the ciphertext, `<name>.tar.gz.enc.sha256` for the
  plaintext's digest (the `.enc` stays in the digest object's name even
  though its *content* is the plaintext sum — the first attempt got this
  pairing wrong on both sides and fixed it here). Runs on a purpose-built
  image, `gardenio/images/ziti-ctrl-backup` — the first attempt's
  `amazon/aws-cli:2.36.48` has no `tar`/`gzip`/`openssl` and never took a
  working backup.
- `restore-if-empty` (an initContainer, same image) restores whenever
  `ZITI_HOME` is empty — which, because the controller's `data` volume is an
  `emptyDir` (not a PVC — see `docs/ziti-controller-k3s.md` for why), is
  every pod restart, not just a node replacement. **It restores a PINNED
  backup, never "latest".** This cluster has no IRSA: the same node role
  that lets `s3-backup` write this prefix is inherited by every pod that can
  reach IMDS, so a `latest`-wins restore (the first attempt's actual
  behavior) would let any pod on the cluster choose what PKI the controller
  boots into next. Instead `restore-if-empty` requires three Deployment env
  vars — `RESTORE_S3_KEY`, `RESTORE_S3_VERSION_ID`,
  `RESTORE_CIPHERTEXT_SHA256` — pinning an exact, previously-reviewed S3
  object version, verifies the downloaded ciphertext's sha256 against the
  pin BEFORE decrypting, and stages the restored archive's newest
  `ctrl-ha.db-<ts>` member for import the same way `ziti-ctrl-backup restore`
  does: outside `raft/` as `import/ctrl-import.db`, with `raft/` left empty
  and a `db:` line written into the config so the controller's first start
  runs `RaftRestoreFromBoltDb` and bootstraps a fresh single-voter raft
  cluster (commit `00ec9d0`, fixing the same leaderless-raft bug documented
  above for `ziti-ctrl-backup`). Missing pin + `ALLOW_FRESH_PKI` not
  explicitly `"true"` ⇒ the initContainer exits non-zero and the pod stays
  Pending rather than quietly booting a new PKI.
  **Runbook: advancing the pin.** `s3-backup` logs the key, version ID and
  ciphertext sha256 of every object it uploads
  (`kubectl -n ziti logs deploy/ziti-controller -c s3-backup`). Copy those
  three values into the commented-out `RESTORE_*` env vars at the top of
  `controller.yaml`, set `ALLOW_FRESH_PKI` to `"false"`, and
  `kubectl apply`. Do this periodically in normal operation (weekly is
  plenty) — not only after an incident — so a future node-replacement
  restore doesn't fall back to a stale pin. A restart with no pin at all
  (the state right after this file first lands) is expected to stay Pending
  until the first pin is set; that is the fail-closed design working as
  intended, not a bug.
- The encryption passphrase lives in its own Secret,
  `ziti-ctrl-backup-passphrase` — a different value from this doc's
  `/gardenio/ziti/backup-passphrase` SSM parameter, which in-cluster pods
  cannot reach (no OIDC/IRSA on this cluster).

The `ziti-ctrl-backup verify`/`restore` tooling above works unmodified
against these archives — same tar layout, same encryption, same object
naming. `ziti-ctrl-backup` itself has no `pull` for this prefix (nothing
here runs the Vultr box's ssh-based flow); restoring one of these objects by
hand for inspection is `aws s3api get-object --version-id ...` + the same
`openssl enc -d` + `verify` steps `pull` performs internally, same as the
"Restoring for real" section above.

## Gotchas learned the hard way

- **`ctrl.yaml` hard-codes the absolute home path** — `dataDir` and every
  `cert`/`key`/`ca` entry, eleven of them. Untar somewhere else and the
  controller reads the *old* home's PKI, or fails. `restore` rewrites them,
  but the safest live restore is back to the same path. It is also why an
  in-cluster controller should mount its home at the path its config was
  generated for.
- **`ziti edge db snapshot` accumulates.** Each call leaves another
  `ctrl-ha.db-<ts>` next to the live DB and nothing prunes them. On a 4 MB
  database that is slow-motion disk fill. `snapshot` keeps the three newest
  and deletes the rest.
- **...and it is rate limited.** A second call within about a minute returns
  `429 RATE_LIMITED` and writes nothing. A scheduled backup never notices; a
  retry loop, or two people backing up at once, would. `snapshot` fails loudly
  rather than tarring a stale snapshot and calling it fresh.
- **`ziti ops db explore` needs a TTY** — it panics with
  `no such device or address` under a pipe. The scriptable offline inspectors
  are `ziti ops db get-db-version <file>` and `ziti ops db du <file>`; `verify`
  counts identities from the latter (`^/ziti/identities/<id>: `) without
  starting anything.
- **`instance-*/db/` is empty on a raft controller** and backing it up proves
  nothing. The data is in `raft/ctrl-ha.db`. A backup that grabbed `db/` and
  called it done would restore an empty overlay.
- **quickstart leaves the CA private keys mode 0644.** Verified on a fresh
  2.0.4 quickstart: `pki/root-ca/keys/root-ca.key` is world-readable. The
  archive inherits those modes, which is one more reason it is encrypted at
  rest — and a hardening item for the box itself, separate from this task.
