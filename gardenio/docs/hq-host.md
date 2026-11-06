# The Gas City HQ host (`gardenio-hq`)

One on-demand EC2 instance that runs the machine-wide `gc supervisor`, the
dogfood city and its tmux agent sessions. Phase 1 of the private plan
`hq/plans/hq-on-aws.md` (mirror `mattbakerconsulting/gascity-hq`): lift-and-shift
off the Vultr box, ziti controller stays on Vultr. Defined in
`terraform/modules/hq`, composed in `terraform/composition/gardenio`, driven
from the laptop by `scripts/hq`. Built 2026-09-17.

**Placement A.** Public subnet (us-east-1a), public IP, security group with
**zero ingress rules** and all egress. Nothing listens on the public address:
the supervisor binds `127.0.0.1:8372`, ziti and SSM are outbound dials. Access
is SSM Session Manager (`scripts/hq shell`) or the ziti overlay; there is no
SSH and no Elastic IP (the public IP changes on every stop/start, nothing
depends on it).

| item | value |
| --- | --- |
| instance | `i-0123456789abcdef0` (`m6a.large`, 2 vCPU / 8 GiB, on-demand) |
| root | gp3 40 GiB, encrypted, `delete_on_termination` (instance has `disable_api_termination`) |
| IMDS | v2 required, hop limit 1 |
| role | `gardenio-hq`: `AmazonSSMManagedInstanceCore` + read of SSM `/gardenio/hq/*` + `dolt/*` in the backup bucket |
| snapshots | DLM `policy-095a3e8b14a43c1c2`, daily 03:00 UTC, retain 7 (crash-consistent) |
| backups | `gc-dolt-backup.timer` daily 02:30 UTC + before every idle poweroff -> `/home/gc/backups/dolt/<db>` -> `s3://gardenio-hq-backups-123456789012/dolt/` (see below) |
| ziti | identity `gardenio-hq` (`gc-clients`,`gc-hosts`), `ziti-edge-tunnel run-host` root unit, `/var/lib/ziti` |
| user | `gc` (lingering systemd user session), `~/.local/bin/gc`, `~/.gc`, `~/.claude` |
| listening | `127.0.0.1:8372` (supervisor) and the stub resolver only; sshd is disabled (`ssh.socket`/`ssh.service`), SSM Session Manager is the shell |

## Idle-stop: how the host scales itself down

`hq-idle-stop.timer` (root, every 5 minutes, `/usr/local/sbin/hq-idle-stop`)
powers the host off when it has been idle for 30 consecutive minutes. Because
the instance has `instance_initiated_shutdown_behavior = "stop"`, an OS
`systemctl poweroff` is an EC2 **stop**: compute billing ends, the EBS root
persists with everything on it, and `scripts/hq start` boots it again in ~1
minute (same volume, new public IP).

The host is **active** (the idle counter resets) when any of these holds:

- a gc session is `state == "active"` or `running == true` in any running city
  (`gc session list --json` as `gc`, per city from `GET /v0/cities`);
- an SSM Session Manager shell is open (`ssm-session-worker` process; RunCommand
  invocations such as `scripts/hq run` do not count);
- a tmux client is attached for `gc` (someone is in a pane);
- `hq-bootstrap` is running;
- the hold file `/var/lib/hq/hold` exists (`scripts/hq hold` / `unhold`);
- the session query failed for a running city (`sessions_unknown`): a guess is
  never a reason to power off.

**tmux sessions.** Every agent session is a tmux session owned by the
supervisor. A running agent session (mid-turn or waiting on the model) shows
as active in `gc session list`, so poweroff never happens while one is live.
A *suspended* session has no tmux process and does not keep the host up;
it resumes when the supervisor next wakes it, which requires the host to be
started. Suspended/closed sessions survive the stop (their state is on the
root volume). Nothing that is not tracked by gc keeps the host up unless you
are attached to it or hold it.

Two more guards: nothing is stopped within 20 minutes of boot (`uptime`), and
the `check` mode never powers off. The decision inputs are logged every run
(`journalctl -t hq-idle-stop`, or `scripts/hq log`):

```text
hq-idle-stop: sessions=0, idle for 25m (limit 30m, uptime 47m)
hq-idle-stop: sessions=0, idle for 30m >= 30m, uptime 52m: powering off (EC2 stop)
```

Tuning: `HQ_IDLE_MINUTES` / `HQ_MIN_UPTIME_MINUTES` as `Environment=` in
`/etc/systemd/system/hq-idle-stop.service` (drop-in), then `daemon-reload`.

## Cost

us-east-1 on-demand, m6a.large $0.0864/h, gp3 $0.08/GiB-mo, snapshots
$0.05/GiB-mo incremental. No NAT, no EIP, no ELB.

| scenario | compute | EBS 40 GiB | snapshots | total $/mo |
| --- | --- | --- | --- | --- |
| always on (730 h) | 63.07 | 3.20 | ~1 | **~67** |
| idle-stop, 8 h/day weekdays (~174 h) | 15.03 | 3.20 | ~1 | **~19** |
| idle-stop, 4 h/day every day (~122 h) | 10.54 | 3.20 | ~1 | **~15** |
| stopped all month | 0 | 3.20 | ~1 | **~4** |

Stopped means EBS only: ~$3.20/mo for the 40 GiB root plus the incremental
snapshots (a few hundred MB after the first). Resizing changes only the
compute line: c6a.large $0.0765/h, m6a.xlarge $0.1728/h, m6a.2xlarge $0.3456/h.
The account budget (`monthly_budget_usd`) was raised 100 -> 150 for the
always-on case.

## Operating it

All from the laptop, profile `gardenio`, region us-east-1; the script finds
the instance by tag `Name=gardenio-hq`.

```sh
gardenio/scripts/hq status        # state, type, IPs, idle-stop inputs
gardenio/scripts/hq start         # aws ec2 start-instances + wait for SSM
gardenio/scripts/hq stop          # refuses while sessions are active; --force
gardenio/scripts/hq resize m6a.xlarge   # stop, modify-instance-attribute, start
gardenio/scripts/hq run 'journalctl -u ziti-edge-tunnel -n 5'   # SSM RunCommand
gardenio/scripts/hq shell         # SSM Session Manager (counts as activity)
gardenio/scripts/hq hold          # keep it up regardless (unhold to release)
gardenio/scripts/hq backup-status # /var/lib/hq/last-backup + last gc-dolt-backup journal lines
```

Without the script: `aws ec2 start-instances --instance-ids i-0123456789abcdef0 --profile gardenio --region us-east-1`.

**Resizing.** Two equivalent paths; either way the host stops and starts, and
active sessions are lost, so check `hq status` first.

1. `terraform apply -var hq_instance_type=m6a.xlarge` (terraform performs the
   stop/modify/start itself), then persist the value in `terraform.tfvars`.
2. `scripts/hq resize m6a.xlarge`, then set `hq_instance_type` in
   `composition/gardenio` so the next apply does not resize it back.

Larger classes when needed: m6a.xlarge (4/16) for 6-10 concurrent sessions,
r6a.large (2/16) if memory not CPU is the limit. Stay on-demand: a spot
interruption kills every tmux session mid-turn.

## Backups and restore

Three layers, from cheapest to most useful:

| layer | what | consistency | where |
| --- | --- | --- | --- |
| EBS snapshot | DLM daily 03:00 UTC, retain 7 | crash-consistent (whole root volume) | EC2 snapshots |
| local `dolt backup` | `gc-dolt-backup` daily 02:30 UTC, 10 min after boot, and before every idle poweroff | consistent (`CALL DOLT_BACKUP('sync')` through the running server) | `/home/gc/backups/dolt/<db>` on the root volume |
| S3 mirror | same run, `aws s3 sync --delete` of that directory | same | `s3://gardenio-hq-backups-123456789012/dolt/<db>/` |

`gc-dolt-backup` (`/usr/local/bin`, installed by `hq-bootstrap backup`) reads
the managed server's port from `<city>/.gc/runtime/packs/dolt/dolt-state.json`,
lists databases (`show databases` minus `information_schema`/`mysql`), and
for each one adds a backup named `hq-local` -> `file:///home/gc/backups/dolt/<db>`
if missing, then syncs it. Backups are incremental (new chunk files + a new
`manifest`), so a sync of an unchanged database is a few seconds and uploads
nothing. A `flock` serialises overlapping runs (timer, idle-stop, by hand).
The last line of every run goes to `/var/lib/hq/last-backup`:

```text
2026-09-17T22:05:56Z ok dbs=hq bytes=24025968 s3=s3://gardenio-hq-backups-123456789012/dolt/
```

(`<utc> ok|fail dbs=<list> bytes=<local dir> <detail or error>`). The unit
exits non-zero on any failure and the failure line is also logged at
`err` priority (`journalctl -t gc-dolt-backup`). Read it from the laptop
with `scripts/hq backup-status`.

**The doctor is blind to this.** `gc doctor`'s `bd-backup-freshness` check
passes vacuously: gc's own `[maintenance.dolt]` backup wiring was never
connected upstream, so nothing gc knows about ever writes a backup and the
check has nothing to be stale about. `/var/lib/hq/last-backup` is the real
signal; a timestamp older than a day while the host has been up, or a `fail`,
is the alarm.

**Bucket.** `gardenio-hq-backups-123456789012` (module `hq`): SSE-S3,
versioning on, all public access blocked, lifecycle: noncurrent versions
expire after 30 days, objects under `dolt/` expire after 90 days, incomplete
multipart uploads aborted after 7 days. The instance role may `ListBucket`
and `Put/Get/DeleteObject` + `AbortMultipartUpload` under `dolt/*` only
(`DeleteObject` is what `sync --delete` needs; on a versioned bucket it only
adds a delete marker, so a bad sync is undoable for 30 days). Caveat of the
90-day rule: Dolt chunk files that never change are expired too, and the next
sync re-uploads them; between the expiry and that sync (normally the next
02:30, longer if the host is stopped) the S3 copy is missing files while the
noncurrent version is still restorable via `aws s3api list-object-versions`.
The local copy and the EBS snapshots are unaffected.

Cost at ~50 MB/day of new chunk data: ~4.5 GB steady state under `dolt/`
plus up to a month of noncurrent versions, S3 Standard $0.023/GB-month,
so roughly $0.10-0.15/month; requests are a handful per day. Restores are
the only egress ($0.09/GB out of AWS; free onto the host).

### Restore

Verified 2026-09-17 on the host, as `gc`: `dolt clone` from the local backup
and from a fresh `aws s3 sync` of the bucket into `/tmp` both yield 23,563
chunks and `select count(*) from issues` = 176, matching the live server.
The full procedure below is written from that and has **not** been run
against the live city.

Do not restore over the live city directory; build a new one and switch.

```sh
# 0. From the laptop: keep the host up, then get a root shell.
gardenio/scripts/hq hold
gardenio/scripts/hq shell

# 1. Stop the supervisor (it owns the city's dolt sql-server and tmux sessions).
sudo -u gc -H XDG_RUNTIME_DIR=/run/user/$(id -u gc) systemctl --user stop gascity-supervisor.service
pgrep -af 'dolt sql-server'          # must print nothing; kill -TERM any leftover

# 2. As gc: clone the backup into a NEW city dir. Local copy first; the S3
#    mirror is the same bytes if the local one is gone (new host from a
#    snapshot, or the root volume lost).
sudo -u gc -H bash -l
export AWS_DEFAULT_REGION=us-east-1
NEW=/home/gc/gascity-restore
mkdir -p "$NEW/.beads/dolt"
dolt clone file:///home/gc/backups/dolt/hq "$NEW/.beads/dolt/hq"
#   or: aws s3 sync s3://gardenio-hq-backups-123456789012/dolt/ /tmp/s3-mirror --only-show-errors
#       dolt clone file:///tmp/s3-mirror/hq "$NEW/.beads/dolt/hq"
#   for a point in time: aws s3api list-object-versions --bucket ... --prefix dolt/hq/manifest
#       and copy that manifest version (plus the chunk files it names) into the mirror first.
(cd "$NEW/.beads/dolt/hq" && dolt remote remove origin && dolt sql -q 'select count(*) from issues')

# 3. Bring the rest of the city over: everything except the dolt data.
rsync -a --exclude '.beads/dolt' --exclude '.gc/runtime' /home/gc/gascity-test/ "$NEW/"

# 4. Point at the new dir and start. Rename so nothing else needs to change:
mv /home/gc/gascity-test /home/gc/gascity-test.pre-restore.$(date -u +%Y%m%dT%H%M%SZ)
mv "$NEW" /home/gc/gascity-test
exit   # back to root
sudo -u gc -H XDG_RUNTIME_DIR=/run/user/$(id -u gc) systemctl --user start gascity-supervisor.service
```

Verify: as `gc`, `cd /home/gc/gascity-test && gc doctor`; the managed
server is back when `.gc/runtime/packs/dolt/dolt-state.json` says
`running: true` (note the port may change), then
`DOLT_CLI_PASSWORD= dolt --host 127.0.0.1 --port <port> --user root --no-tls --use-db hq sql -q 'select count(*) from issues'`
should match step 2, `bd list` should show the issues, and
`sudo /usr/local/bin/gc-dolt-backup && cat /var/lib/hq/last-backup` should
say `ok` (the clone has no `hq-local` backup entry; the script re-adds it).
Then `gardenio/scripts/hq unhold`. The `.pre-restore.*` directory can be
deleted once the city has run for a day.

Restoring on a rebuilt host is the same from step 2 with the S3 path, after
`hq-bootstrap all` and a copy of the city's non-dolt files from the last
EBS snapshot (or from git, if the city dir is under version control).

## What is on the host and how it got there

`terraform/modules/hq/files/hq-bootstrap.sh` is delivered by cloud-init
user-data (gzip'd) and run once as `hq-bootstrap all`; it lives at
`/usr/local/sbin/hq-bootstrap` and every step is idempotent, so re-run one
with `scripts/hq run 'hq-bootstrap <step>'`. Pins are in `/etc/hq/pins.env`
(from the module's variables). Log: `/var/log/hq-bootstrap.log`. The host
copy is what user-data delivered at first boot; later edits to the repo file
reach it only when you re-deliver it (a rebuild, or paste the changed step
through `scripts/hq run`).

| step | what |
| --- | --- |
| base | tmux git curl jq unzip libicu-dev build-essential; awscli v2; user `gc` + `loginctl enable-linger gc` |
| go | Go `1.26.6` at `/usr/local/go` (upstream `go.mod`: `go 1.26.6`) |
| node | Node 22 from NodeSource (upstream `setup-gascity-ubuntu` default) |
| gascity | `~gc/src/gascity` = `gastownhall/gascity` at **`d807c8e0dd6d071a79e821be5da73097dbf4ff96`**; dolt `2.1.7` and bd `v1.3.0-rc.2` via upstream's pinned-SHA installers (`.github/scripts/install-{dolt,bd}-archive.sh`, the versions `fork-verify.yml` pins); `go build ./cmd/gc` -> `~gc/.local/bin/gc`. `gc version` prints `dev` (no ldflags); the commit is in `~gc/.local/share/gc-build.txt` |
| claude | Claude Code native install as `gc` (`https://claude.ai/install.sh`) |
| ziti | `ziti-edge-tunnel` v1.18.7 at `/usr/local/bin`; `ziti-edge-tunnel.service` (`run-host -I /var/lib/ziti`, stays inactive until an identity exists) |
| idle-stop | the timer above (calls `gc-dolt-backup` before `systemctl poweroff`) |
| backup | `/usr/local/bin/gc-dolt-backup` + `gc-dolt-backup.{service,timer}` (root units; the Dolt and S3 work runs as `gc`) |
| ziti-enroll | reads SSM `/gardenio/hq/ziti-enroll-jwt` (one-time JWT minted on Vultr: `ziti edge create identity gardenio-hq -a gc-clients,gc-hosts`) and enrols to `/var/lib/ziti/gardenio-hq.json` |
| claude-creds | reads SSM `/gardenio/hq/claude-credentials` into `~gc/.claude/.credentials.json` (0600) |
| supervisor | writes `~gc/.gc/supervisor.toml` (`allowed_hosts = ["gc.dashboard.ziti", ...]`), then `gc supervisor install` -> user unit `gascity-supervisor.service`, `127.0.0.1:8372`, enabled, `WantedBy=default.target` + linger = comes back on every boot |

The two SSM SecureStrings are staged from the laptop
(`aws ssm put-parameter --type SecureString --value file://...`) and deleted
after the host consumes them; nothing secret is in terraform state or the
repo. This host builds gc from source once (`go build`, ~minutes) so its
binary matches a known upstream commit; it is not a CI runner and runs no
builds otherwise.

`gc` here is built from `d807c8e0d`; the Vultr city's `gc version` prints
1.4.1 (996 commits behind). Both run bd `1.3.0-rc.2`: the Vultr city was
upgraded from 1.2.2 on 2026-09-17 and its store migrated with it.

### Cutover rollback lever (degraded, and why)

The Vultr city was the one-week rollback for this cutover (through
2026-09-24), held at bd 1.2.2 / dolt schema v53 so that rolling back was
one step: restart the unit there. That is no longer true. Opening a 1.2.2
store with bd 1.3.0-rc.2 migrates it irreversibly (schema v53 -> v66) and
1.2.2 then refuses it — *"database is at v66, binary knows up to v53 (13
migrations ahead)"*. Rollback is therefore now **two** steps, restore then
run the old binary:

```sh
# on vultr, as claude
rm -rf ~/gascity-test/.beads
cp -a "$(cat ~/backups/LATEST_BEADS_BACKUP)" ~/gascity-test/.beads
cd ~/gascity-test && bd-1.2.2 dolt start && bd-1.2.2 list --limit 0
```

Both halves are kept permanently and must not be garbage-collected:
`~/backups/gascity-test-beads-20260917-225522` (36M, `.local_version`
1.2.2) and the preserved binary `~/.local/bin/bd-1.2.2`.

**Restore drill, 2026-09-17 — the lever is verified, not assumed.** The
backup was copied to a scratch path and opened with `bd-1.2.2`: it reports
`.local_version` 1.2.2, the read exits 0, and it returns the same 9 issues
with the same sorted-unique id-inventory hash recorded before the
migration, `3cf17008ab63888042fb39d0727915c5ba26855fb7b54337de3a34703780bc27`.
The drill ran entirely on a copy; the live city was never opened.

Re-run the drill if the backup is ever moved or regenerated. An unverified
backup is not a rollback lever — which is the same failure this host already
has in `bd-backup-freshness` passing vacuously over an empty set.

## Ziti

**Cutover is done** (2026-09-17): `gc-dashboard` terminates here, not on
Vultr. `gc-dashboard-host` is `{"address":"127.0.0.1","port":8372}`, the
`gc-dashboard-bind` policy binds `#gc-hosts`, and `gardenio-hq` is the only
identity carrying that attribute — so no other host may bind the service. The
`intercept.v1` (`gc.dashboard.ziti:8372`) was left as is, so laptop and phone
identities needed no change. The tunnel runs in `run-host` mode (no TUN, no
resolver changes); switch the unit to `run` when the host must dial services
such as `beads.ziti`.

Verified end to end: HQ answers 200 on `127.0.0.1:8372` including with
`Host: gc.dashboard.ziti:8372`, and `curl http://gc.dashboard.ziti:8372/`
from an enrolled laptop returns 200. That it is *this* host and not Vultr is
confirmed by Vultr having nothing listening on 8372 with its
`gascity-supervisor.service` inactive.

> Circuit setup through the overlay takes **~9 s** on a cold dial. A probe
> with a timeout under that (e.g. `curl --max-time 10`) reports a false
> failure — `HTTP 000` — and looks identical to the dashboard being down.
> Allow >= 20 s before concluding anything is broken.

Health: `scripts/hq run 'journalctl -u ziti-edge-tunnel -n 5'` should show
`router router-instance-1 connected`. The one `ERROR ... DNS ip pool exhausted
(0 IPs)` line at start-up is expected in `run-host` mode (no TUN, no DNS range).

## Rebuilding or retiring

- Snapshot first: `aws ec2 create-snapshot` on the root volume, or wait for
  the 03:00 DLM run.
- Rebuild in place: the module ignores AMI and user-data drift on purpose.
  `terraform apply -replace=module.hq.aws_instance.hq` after setting
  `disable_api_termination = false` (apply that alone first).
- Retire: `terraform destroy -target=module.hq` (same termination-protection
  dance), keep the last snapshot. Delete `/gardenio/hq/*` SSM parameters if
  any were left behind and the ziti identity on the controller.
