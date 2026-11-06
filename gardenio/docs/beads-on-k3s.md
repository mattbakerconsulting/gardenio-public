# Shared beads (Dolt) on k3s, over ziti

> **The ziti path is opt-in only.** `beads.ziti` and the beads `ziti-host`
> apply only when the ziti overlay is on (`ziti.enabled=true` and
> `ziti.hostBeads.enabled=true`, `gardenio/helm/ziti`, off by default since
> gd-xNMLrsgajy). The Vultr controller it describes is kept running, unchanged,
> by owner decision 2026-10-13, and nothing on gardenio's default path dials
> it: see [`../helm/ziti/README.md`](../helm/ziti/README.md) "Where the ziti
> controller lives".

Built 2026-09-17 from `plans/shared-beads-on-k3s.md`. One `dolt sql-server`
for every Gas City city, in the gardenio k3s cluster, reachable only as
`beads.ziti:3306` through the OpenZiti fabric (controller on Vultr). Nothing
is exposed on the internet: no LoadBalancer, no ingress, no node port.

## What exists

| Where | Object | Notes |
| --- | --- | --- |
| k3s ns `beads` | StatefulSet `dolt` (1 replica), `dolthub/dolt:2.3.5` | `helm/beads/dolt-statefulset.yaml`; config in ConfigMap `dolt-config` |
| | Service `dolt` ClusterIP 3306 | `dolt.beads.svc.cluster.local:3306` |
| | PVC `dolt-data-dolt-0` 20 GiB, PVC `dolt-backups` 20 GiB | storage class `local-path`, both on node `ip-10-90-5-163` (the on-demand server) under `/var/lib/rancher/k3s/storage/` (see caveats) |
| | Secret `dolt-root` | password of `root@%`, from SSM `/gardenio/beads/dolt-root` |
| | Secret `ziti-identity` | enrolled identity `k3s-beads-host` (JSON, never on disk elsewhere) |
| | Deployment `ziti-host` | `openziti/ziti-edge-tunnel:1.19.1 run-host`, binds ziti service `beads` |
| | CronJob `dolt-backup` 02:45 UTC | `DOLT_BACKUP('sync-url')` per db → `/backups` → `s3://gardenio-hq-backups-123456789012/k3s-dolt/` |
| | Job template `dolt-restore-job.yaml` | S3 → `/backups/restore/<db>` → `DOLT_CLONE` on the server |
| ziti (Vultr) | configs `beads-host` (host.v1 → `dolt.beads.svc.cluster.local:3306`), `beads-intercept` (intercept.v1 `beads.ziti:3306`) | |
| | service `beads`; policies `beads-bind` (`#beads-hosts`), `beads-dial` (`#gc-clients`) | existing `all-endpoints-public-routers` / `all-routers-all-services` cover it |
| | identity `k3s-beads-host` (`beads-hosts`) | JWT was one-shot; re-mint on the controller to re-enroll |
| AWS | SSM SecureString `/gardenio/beads/dolt-root` | readable by the operator and the HQ instance role |
| | k3s node role: `s3:ListBucket`/`Put`/`Get`/`DeleteObject` under `k3s-dolt/*` | `terraform/modules/k3s` `backup_bucket_arn` |
| | bucket lifecycle `k3s-dolt-90d` | like the HQ host's `dolt/` prefix |
| | servers ASG: `on_demand_base_capacity = 1` | `terraform/modules/k3s` `server_on_demand_base`; nodes self-label `gardenio.io/lifecycle` |

Server config (ConfigMap): port 3306 on 0.0.0.0, `max_connections 512`,
`back_log 50`, `max_connections_timeout_millis 5000`,
`read_timeout_millis 120000`, `log_level warning`, Prometheus metrics off;
the init container also sets `dolt config --global --add metrics.disabled
true`. These are the Dolt load-lab numbers (`hq/dolt-load-report.md`).
Requests 250m/250Mi, limits 1 CPU/1Gi.

Version: Dolt 2.3.5 (lab: marginally cheaper than 2.1.7; the HQ host still
runs 2.1.7 as a client and `bd 1.3.0-rc.2` — both worked against 2.3.5 in
the validation below). Upstream contrib pins 2.1.7.

## Connecting

Any identity with role `gc-clients` (matthew-laptop, matthew-laptop-win,
matthew-phone, gardenio-hq) can dial. From a tunneler in intercept mode:

```sh
PW=$(aws ssm get-parameter --profile gardenio --region us-east-1 \
      --name /gardenio/beads/dolt-root --with-decryption --query Parameter.Value --output text)
mysql -h beads.ziti -P 3306 -u root -p"$PW" -e 'show databases'
# or, with the dolt CLI:
DOLT_CLI_PASSWORD="$PW" dolt --host beads.ziti --port 3306 --user root --no-tls sql -q 'show databases'
```

Inside the cluster: `dolt.beads.svc.cluster.local:3306`, same credentials
(Secret `dolt-root`).

How `gc` finds the password (`internal/doltauth/auth.go`, in order):
`GC_DOLT_PASSWORD` env → `<city>/.beads/.env` line
`BEADS_DOLT_PASSWORD=…` → ambient `BEADS_DOLT_PASSWORD` →
`~/.config/beads/credentials` section `[host:port]` with `password=`.
There is no `--password` flag on `gc beads city use-external`; the
city-local `.beads/.env` (mode 0600) is the durable choice and is what the
validation used.

## Backups

- Daily 02:45 UTC, CronJob `dolt-backup`. Per database:
  `USE db; CALL DOLT_BACKUP('sync-url', 'file:///backups/<db>')` against the
  server (online, includes the working set), then `aws s3 sync --delete
  /backups s3://gardenio-hq-backups-123456789012/k3s-dolt/`. Marker
  `/backups/.last-dolt-sync` holds the time and the database list.
- Manual run: `kubectl -n beads create job --from=cronjob/dolt-backup backup-now`
  then `kubectl -n beads logs job/backup-now --all-containers`.
- Check: `aws s3 ls --recursive s3://gardenio-hq-backups-123456789012/k3s-dolt/`.
- The bucket is versioned (non-current versions kept 30 days); `k3s-dolt/`
  objects expire after 90 days and are re-uploaded by the next sync.
- The EBS root volume of the node is *not* snapshotted; the S3 copy is the
  backup. A local-path volume dies with its node.

## Restore / import a database

Works for both "bring a city's store to the server" (cutover) and
"recover after losing the volume":

```sh
export KUBECONFIG=~/.kube/gardenio.yaml AWS_PROFILE=gardenio AWS_REGION=us-east-1
B=s3://gardenio-hq-backups-123456789012
DB=<database name on the server>
# 1. put a Dolt backup (the directory `dolt backup`/DOLT_BACKUP writes:
#    manifest + *.darc + LOCK) under k3s-dolt/restore/$DB/, e.g. from the HQ
#    host's daily backup:
aws s3 sync $B/dolt/hq/ $B/k3s-dolt/restore/$DB/
# 2. run the restore job (refuses if $DB already exists; DROP DATABASE first if you mean it)
cd gardenio/helm/beads
sed -e "s/DBNAME/$DB/g" -e "s/JOBSUFFIX/${DB//_/-}/g" dolt-restore-job.yaml | kubectl apply -f -
kubectl -n beads wait --for=condition=complete job/dolt-restore-${DB//_/-} --timeout=300s
kubectl -n beads logs job/dolt-restore-${DB//_/-} --all-containers
```

Full-volume loss: re-create the StatefulSet (the init container re-creates
`root@%`), then run the restore job once per database listed in the last
`.last-dolt-sync` (the S3 prefix `k3s-dolt/<db>/` is itself a valid
restore source: `aws s3 sync $B/k3s-dolt/$DB/ $B/k3s-dolt/restore/$DB/`).

## Cutover runbook for the live city (mayor)

Preconditions done 2026-09-17: the HQ host's tunnel runs in `run` mode
(intercepts `beads.ziti`, still hosts `gc-dashboard`); the HQ instance role
can read `/gardenio/beads/dolt-root`; bd on HQ is 1.3.0-rc.2 and the store
is already at schema v66. **Precondition NOT met:** the HQ→k3s ziti path is
200–400 ms per round trip (see the validation section); put an edge router
in AWS first, then re-measure `bd list` from HQ (target < 1 s) before
starting this runbook.

1. **Pick the database name.** Every gc-managed city pins
   `dolt_database: "hq"` in `.beads/metadata.json`; on a shared server the
   name must be unique per city. Use `gascity_test` for the live city.
2. **Quiesce.** Stop the supervisor (session-preserving stop), confirm no
   `gc`/`bd` process is running against the city.
3. **Fresh backup.** `sudo /usr/local/bin/gc-dolt-backup` (writes
   `~gc/backups/dolt/hq` → `s3://…/dolt/hq/`); note the status file
   `/var/lib/hq/last-backup`.
4. **Import into the server** (from the laptop):
   `DB=gascity_test`, `aws s3 sync $B/dolt/hq/ $B/k3s-dolt/restore/$DB/`,
   then the restore job above. Confirm with
   `mysql -h beads.ziti -u root -p… -e "use gascity_test; select count(*) from issues; select * from metadata"`
   that the row count matches `gc bd list --json | jq length` on HQ and
   `_project_id` equals `.beads/metadata.json` `project_id`.
5. **Credentials on HQ** (as gc, in the city):
   `printf 'BEADS_DOLT_PASSWORD=%s\n' "$(aws ssm get-parameter --name /gardenio/beads/dolt-root --with-decryption --query Parameter.Value --output text)" > .beads/.env; chmod 600 .beads/.env`.
6. **Rename the pinned database:** edit `.beads/metadata.json`
   `"dolt_database": "gascity_test"` (keep a copy of the file).
7. **Switch:** `gc beads city use-external --host beads.ziti --port 3306 --user root --dry-run`,
   read the diff, then without `--dry-run`. It validates the endpoint
   (ping, `issues` table, `_project_id` match), writes `[dolt] host/port` to
   `city.toml`, updates `.beads/config.yaml`, and stops the managed dolt pack.
8. **Verify:** `gc bd list`, `gc bd show <id>`, `gc doctor`, then start the
   supervisor and load the dashboard (`gc.dashboard.ziti:8372`). Watch
   `gc dolt health` and the patrol for a few minutes. Every `bd` op opens a
   new connection, and over ziti that is the expensive part: from the
   laptop (home → Vultr router → k3s) a connect+auth is ~500 ms and each
   further query ~80 ms; a bare TCP connect is ~140 ms. Measure from HQ
   before judging (its path is AWS → Vultr → AWS).
9. **Rollback** (until step 10): `gc beads city use-managed`, restore
   `.beads/metadata.json`, start the supervisor. The local store is untouched
   by steps 4–7.
10. **After a week:** `DROP DATABASE` nothing — instead archive the local
    `.beads/dolt/hq` (`tar` to `~gc/backups/`) and remove it; the daily
    `gc-dolt-backup` on HQ will then have nothing local to back up, so
    disable its timer or leave it (it exits cleanly on an empty list — verify).

## Caveats found while building

- **`gc init` starts the managed dolt pack even with `--no-start`** (a
  `dolt sql-server --config <city>/.gc/runtime/packs/dolt/dolt-config.yaml`
  appeared for the scratch city). `use-external` stops it; if you abandon a
  scratch city, kill it by hand.
- **Database name collision:** every managed city's store is named `hq`.
  One server, many cities ⇒ rename per city at import time (step 1/6).
- **`DOLT_BACKUP` needs a current database:** `CALL DOLT_BACKUP(...)`
  without `USE <db>` fails with `empty database name` (both CLI and the
  CronJob use `USE`).
- **Storage is `local-path`, not EBS CSI.** The cluster has no EBS CSI driver
  (only `rancher.io/local-path`), so the PVC is a directory on one node's
  root volume; the pod is pinned by `gardenio.io/lifecycle=on-demand` (and,
  after first bind, by the PV's own node affinity). If that node is replaced
  the volume is gone and the pod stays Pending until the PV/PVC are deleted
  and the store is restored from S3. Trade-off accepted: an EBS CSI driver
  needs the controller add-on plus IAM and would still be AZ-bound; the
  on-demand server is the durable one and S3 is the backup of record.
- **Changing `on_demand_base_capacity` on a live ASG is not gentle.** The
  ASG launched the on-demand instance and 10 s later terminated a spot
  server with no spot notice, so `k3s-spot-watch` never drained it; with my
  own graceful removal of a second server in flight, etcd lost quorum.
  Recovery: `k3s server --cluster-reset` on the survivor, then on each other
  server `systemctl stop k3s; rm -rf /var/lib/rancher/k3s/server/db;
  systemctl start k3s` **one at a time, after the previous one is Ready**
  (wiping a node that has just been promoted to voter loses quorum again).
  Then `kubectl delete node` the stale objects and `k3s-dns add` on the
  rejoined nodes. Next time: drain and delete one server yourself, terminate
  it via `aws autoscaling terminate-instance-in-auto-scaling-group
  --no-should-decrement-desired-capacity`, and only then change the policy.
- **`openziti/ziti-edge-tunnel` image contract:** its entrypoint ignores
  `-i`; it wants `ZITI_IDENTITY_BASENAME` and a writable `/ziti-edge-tunnel`
  directory (identity is re-written on cert renewal). The Deployment copies
  the Secret into an emptyDir; renewals survive restarts of the container
  but not of the pod. If the identity ever stops authenticating, re-mint
  the JWT on the controller and recreate the Secret.
- **HQ tunnel mode:** `run-host` cannot intercept, so the HQ unit was
  switched to `ziti-edge-tunnel run -I /var/lib/ziti` (tun device, DNS
  `100.64.0.2` on `ziti0` with routing domain `~ziti`, default-route off).
  `gc-dashboard` stayed hosted (laptop got HTTP 200 within a minute) and
  `beads.ziti`/`gc.dashboard.ziti` resolve on the host. The previous unit is
  kept at `/etc/systemd/system/ziti-edge-tunnel.service.run-host.bak`.
- Client `dolt 2.1.7` / `bd 1.3.0-rc.2` against server 2.3.5 worked; keep
  the server ≥ client.
- **The HQ host (m6a.large, 8 GiB) is not a safe place for a long
  validation.** During this work its console log showed the OOM killer
  taking a 6.7 GiB `golangci-lint` (uid gc, ~22:20 UTC) from the live
  city's workers, and from 22:52 UTC the host stopped executing SSM
  RunCommand and Session Manager commands for over ten minutes at ~70 %
  CPU while the agent kept pinging. Anything that needs the host answers
  only between the city's lint/test runs; give the city a memory budget or
  a bigger host before the cutover, and run the cutover with the
  supervisor stopped.

## Divergences from upstream `gascity/contrib/k8s`

Candidate contribution back rather than a fork; upstream refs are
`contrib/k8s/<file>:<line>` at gc `d807c8e0d`.

| Upstream | Ours | Why |
| --- | --- | --- |
| `namespace.yaml:4` namespace `gc` | `beads` | this cluster hosts only the store, not agent pods |
| `dolt-statefulset.yaml:29,59` `dolthub/dolt:2.1.7` | `2.3.5` | load lab; bd 1.3.0-rc.2 works against it |
| `dolt-statefulset.yaml:72,76` flags `--port=3307 --max-connections=500` | ConfigMap `config.yaml`, port 3306, 512 conns, timeouts, `back_log`, metrics off | timeouts/back_log are config-file only; 3306 matches the ziti intercept |
| `dolt-statefulset.yaml:49-50` `root@%` with empty password | init container aligns `root@%` with Secret `dolt-root` on every start (`DOLT_ROOT_HOST=% DOLT_ROOT_PASSWORD` on first boot, `ALTER USER` after) | ziti gates the port, but a shared server still gets a password |
| (none) | `dolt config --global --add metrics.disabled true` in init | CLI phone-home off |
| `dolt-statefulset.yaml:101-105` 500m/1Gi – 2/4Gi | 250m/250Mi – 1/1Gi | one city on t3.small servers |
| `dolt-statefulset.yaml:109-116` PVC 20Gi, default class | 20Gi `local-path` + nodeAffinity `gardenio.io/lifecycle=on-demand` + second PVC `dolt-backups` at `/backups` | no EBS CSI here; backup staging shared with the CronJob |
| `dolt-service.yaml:15` port 3307 | 3306 | as above |
| (none) | CronJob `dolt-backup`, Job `dolt-restore`, Deployment `ziti-host` | ours |

Kept as upstream: object names (`dolt`, `dolt-data`), labels
(`app: dolt`, `app.kubernetes.io/part-of: gas-city`), non-root 10001 with
`readOnlyRootFilesystem`, `HOME=/tmp` emptyDir, TCP probes, the
init-user temporary-server pattern.

## Validation transcript (throwaway city, HQ host, 2026-09-17)

All as user `gc` on the HQ host via SSM (`scripts/hq run` / the base64 |
`sudo -u gc -H bash -l` pattern), gc `dev` (d807c8e0d), bd 1.3.0-rc.2,
dolt CLI 2.1.7. The scratch city is left in place at
`/home/gc/scratch/beads-ext-test` (never registered with the supervisor).

```
$ cd ~/scratch && gc init beads-ext-test --template minimal --default-provider claude \
    --providers claude --no-start --skip-provider-readiness --yes
[1/8] Creating runtime scaffold … [7/8] Skipping supervisor startup
Initialized city "beads-ext-test" with default provider "claude".
# .beads/metadata.json: {"backend":"dolt","dolt_database":"hq","dolt_mode":"server",
#   "project_id":"63a5887e-3239-4386-ad34-d3c7e4c54845"}   <- every managed city is "hq"
# a dolt pack was started anyway: dolt sql-server --config …/beads-ext-test/.gc/runtime/packs/dolt/dolt-config.yaml (port 33135)

# migrate the store: online backup -> S3 -> restore job -> DOLT_CLONE
$ DOLT_CLI_PASSWORD='' dolt --host 127.0.0.1 --port 33135 --user root --no-tls sql \
    -q "USE hq; CALL DOLT_BACKUP('sync-url','file:///home/gc/scratch/backup/beads_ext_test')"
$ aws s3 sync ~/scratch/backup/beads_ext_test s3://gardenio-hq-backups-123456789012/dolt/migrate/beads_ext_test/
(laptop) $ aws s3 sync $B/dolt/migrate/beads_ext_test/ $B/k3s-dolt/restore/beads_ext_test/
(laptop) $ DB=beads_ext_test; sed -e "s/DBNAME/$DB/g" -e "s/JOBSUFFIX/${DB//_/-}/g" \
             helm/beads/dolt-restore-job.yaml | kubectl apply -f -
job log: CALL DOLT_CLONE('file:///backups/restore/beads_ext_test','beads_ext_test') -> status 0;
         tables 19; metadata _project_id 63a5887e-3239-4386-ad34-d3c7e4c54845

# credentials + database name, then the switch
$ printf 'BEADS_DOLT_PASSWORD=%s\n' "$(aws ssm get-parameter --name /gardenio/beads/dolt-root \
    --with-decryption --query Parameter.Value --output text)" > .beads/.env   # umask 077
$ python3 -c '…json: metadata["dolt_database"]="beads_ext_test"…'
$ DOLT_CLI_PASSWORD=… dolt --host beads.ziti --port 3306 --user root --no-tls sql -q "show databases"
| beads_ext_test | information_schema | mysql |                      (1.4 s)
$ gc beads city use-external --host beads.ziti --port 3306 --user root --dry-run
WOULD UPDATE: city endpoint
  city: managed_city status=verified -> city_canonical beads.ziti:3306 user=root status=verified
  file: .beads/config.yaml
  file: city.toml
$ gc beads city use-external --host beads.ziti --port 3306 --user root
UPDATED: city endpoint
  state: city_canonical beads.ziti:3306 user=root status=verified
  inherited rigs updated: 0
# .beads/config.yaml now: gc.endpoint_origin: city_canonical, dolt.host: beads.ziti, dolt.port: 3306, dolt.user: root
# city.toml now: [dolt] host = "beads.ziti" / port = 3306 ; the scratch dolt pack was stopped

$ gc bd create "beads-on-k3s validation bead" --type task -p 2      # 44 s (first op)
✓ Created issue: bet-1bz — beads-on-k3s validation bead
$ gc bd list                                                        # 8 s
○ bet-1bz P2 beads-on-k3s validation bead
Total: 1 issues (1 open, 0 in progress)
$ gc bd show bet-1bz                                                # 22 s
gc bd: answering from the city store
○ bet-1bz · beads-on-k3s validation bead   [P2 · OPEN]

(laptop) $ mysql -h beads.ziti -P 3306 -u root -p… \
    -e "select id,title,status,priority,created_at from beads_ext_test.issues"
bet-1bz  beads-on-k3s validation bead  open  2  2026-09-17 23:07:13

$ gc doctor
… ✓ dolt-server — reachable on beads.ziti:3306
  ✗ bead-store-preflight — bead store unreachable — skipped 16 store checks …: timed out after 4.987s (caller deadline)
  ✗ order-firing-current — order history lookup timed out after 15s (advisory)
  ⚠ agent-sessions / orphan-sessions — no tmux server (city never started; expected)
  ⚠ fork-rate — 103 forks/s — the per-command data-plane fork storm (advisory)
75 passed, 3 warnings, 2 failed, 1 advisory                          # 46 s

# where the time goes (HQ -> beads.ziti):
tcp connect            201 / 398 / 298 ms
dolt CLI select 1     1003 / 898 / 1098 ms
bd list (direct)      8198 / 8303 ms
gc bd list            8699 / 7898 ms
```

Backup after the bead: manual `dolt-backup` job → `k3s-dolt/beads_ext_test/`
gained a new chunk file and manifest; `.last-dolt-sync` =
`2026-09-17T23:07:48Z beads_ext_test`. Pod restart test: deleting `dolt-0`
brought it back with the data and `root@%` re-aligned.

**Result: functionally verified. The latency precondition is now MET.**

Originally this section read "operationally not ready": every bd operation
opens fresh connections and does tens of sequential round trips, and with
only the Vultr edge router on the fabric each of those crossed the Atlantic
twice, making `bd list` 8 s and failing `gc doctor`'s 5 s store preflight —
even though HQ and the cluster are both in us-east-1.

Option 1 below was built (`router-aws-1`, see `ziti-routers.md`). Measured
from the HQ host afterwards, same scratch city, same store:

| | before (Vultr router only) | after (`router-aws-1`) |
|---|---|---|
| `bd list` | 8 s | **0.62 s** |
| circuit setup (fresh TCP connect) | 200–400 ms | 136–174 ms |

`bd list` is a 13x improvement and comfortably inside the <1 s bar the
cutover runbook sets. Note the two rows measure different things: the
`bd list` figure is the one that matters, because bd reuses the circuit
across its round trips, while a cold `socket.create_connection` pays full
circuit establishment every time — which still involves the controller on
Vultr. Anything that opens one connection and immediately closes it will
still look slow; that is expected and is not the cutover blocker.

The options as originally written, kept for the record:

1. **An edge router inside AWS** so the HQ↔k3s path stays in-region. **Done.**
2. Or run the HQ city's gc/bd with a longer store deadline and accept
   ~8 s per op (not acceptable for the patrol at ~2 ops/s).
3. Or move the HQ city's workload into the cluster (the contrib/k8s
   controller path), which the plan's later phases already contemplate.

## Cost

| Item | $/month |
| --- | --- |
| One server on-demand instead of spot (t3a.small $0.0188/h vs ~$0.0084/h spot) | +7.6 |
| PVCs (on the node root volume) | 0 |
| S3 `k3s-dolt/` (~1 MB now, versioned) | ~0.01 |
| Pods (dolt, tunneler, jobs) on existing nodes | 0 |
