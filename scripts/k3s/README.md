# Gardenio in a Box — k3s pinning & durability (Phase 0.2)

Single-node k3s hardening for the in-box build (epic [ka-0000007690], task
ka-0000002781). Source: `gardenio/docs/research/inbox-k8s-distro-choice.md`
§"Single-EC2 install sketch" / (c) restart durability / §"Backup / DR".

These scripts are invoked from `scripts/bootstrap-ec2.sh` (stage 2) but can be
run standalone on any single-node k3s box.

| Script | Role |
|---|---|
| `write-k3s-config.sh` | Generate `/etc/rancher/k3s/config.yaml` — pins `node-ip`/`advertise-address` to the current private IPv4 (re-derived from IMDS each boot) and builds the `tls-san` list (current IP + stable EIP/DNS + loopback). Declares the single-node datastore intent (SQLite/kine). |
| `install-k3s-durability.sh` | Install the systemd units: `k3s-config-refresh.service` (ordered `Before=k3s.service`, re-pins the IP every boot) and `k3s-datastore-backup.timer`. Copies helpers to `/usr/local/lib/gardenio/`. |
| `k3s-datastore-backup.sh` | Online `sqlite3 .backup` of the kine datastore + node token, with local retention and optional S3 off-box copy. |

## The two durability problems solved

### 1. EC2 stop/start changes the private IP → apiserver cert breaks

A stop/start re-leases the private IPv4. If the apiserver cert was signed for
the old IP and `node-ip`/kubeconfig point there, the apiserver comes back with a
cert that no longer matches a reachable address and `kubectl`/agents fail TLS.

Fix: `k3s-config-refresh.service` runs `write-k3s-config.sh` on every boot,
**before** `k3s.service`, re-pinning `node-ip`/`advertise-address` to the
current IP and rewriting `tls-san`. Pass a **stable** SAN (an Elastic IP or a
DNS name) via `--tls-san` so external kubeconfigs keep working across the
IP change:

```sh
sudo scripts/k3s/install-k3s-durability.sh --tls-san my-box.example.com --tls-san 52.1.2.3
```

### 2. Backup/DR on a SQLite/kine single node

> ⚠ **Correction to the research doc.** The install-sketch step 7 says to enable
> `k3s etcd-snapshot save` "with the SQLite datastore". That is **wrong** —
> `k3s etcd-snapshot` and the `etcd-snapshot-schedule-cron` server flag only
> work with the **embedded etcd** datastore. On a single node running
> SQLite/kine (the recommended datastore, step 8), `k3s etcd-snapshot save`
> fails with *"etcd database not found"* (k3s-io/k3s#8960). The correct
> datastore backup is an online `sqlite3 .backup` of the kine DB — what
> `k3s-datastore-backup.sh` does, scheduled by `k3s-datastore-backup.timer`
> (default 00:00/12:00, matching k3s's own etcd-snapshot default cadence).

The datastore backup captures the kine `state.db` **and** the node `token`
(which encrypts datastore secrets — a DB backup is useless without it).

### What the datastore backup does NOT cover

local-path PV **contents** under `/var/lib/rancher/k3s/storage` (postgres, NATS
bytes) are **not** in the datastore — the datastore records PV *objects*, not
their data. Back those up separately:

- **EBS snapshot** of the root/data volume (simplest on EC2), or
- **filesystem snapshot** / `tar` of `/var/lib/rancher/k3s/storage`, or
- **Velero** with a restic/node-agent for app-consistent PV backups.

Phase 1 (ka-0000007539, esp. ka-0000008198) adds an in-cluster `pg_dumpall`
CronJob for logical postgres backups on top of this host-level DR.

## Restore

Each backup archive embeds a `RESTORE.md`. In short, on a clean k3s node:

```sh
sudo systemctl stop k3s
sudo tar -xzf k3s-kine-<ts>.tar.gz -C /tmp/restore
sudo cp /tmp/restore/token    /var/lib/rancher/k3s/server/token
sudo cp /tmp/restore/state.db /var/lib/rancher/k3s/server/db/state.db
sudo rm -f /var/lib/rancher/k3s/server/db/state.db-wal /var/lib/rancher/k3s/server/db/state.db-shm
sudo systemctl start k3s
```

[ka-0000007690]: https://beads-viewer.e2e.example.com/bead/ka-0000007690

## kubectl and break-glass over SSM (no public 6443)

kubectl reaches the API through an AWS SSM port-forward (owner decision
gd-PlIleJE1J2): AWS IAM is the edge, and there is no public 6443 once
`admin_cidrs = []`. The same path is the break-glass route: it works with the
edge, Keycloak and ziti all down, because it needs only AWS IAM and the node's
SSM agent (`AmazonSSMManagedInstanceCore`).

Needs the `session-manager-plugin` on the machine, and an AWS profile
`gardenio` allowed `ssm:StartSession` on the server instances with document
`AWS-StartPortForwardingSession`; for the edge relay below, also
`ssm:StartSession` on the ziti edge instance with document
`AWS-StartPortForwardingSessionToRemoteHost`.

```sh
scripts/k3s/kubectl-ssm.sh start    # idempotent; returns once 127.0.0.1:16443 answers
kubectl --context gardenio-ssm get nodes
scripts/k3s/kubectl-ssm.sh status   # exit 0 live, 1 down
scripts/k3s/kubectl-ssm.sh stop     # kills only the recorded PID's process group
```

`start` finds a running instance tagged `k3s:role=server`, starts
`aws ssm start-session ... AWS-StartPortForwardingSession
--parameters portNumber=6443,localPortNumber=16443` detached (its own session,
PID in `~/.local/state/gardenio/kubectl-ssm.pid`, log beside it), and writes a
`gardenio-ssm` context into `~/.kube/config` derived from `~/.kube/gardenio.yaml`
with `server: https://127.0.0.1:16443` and `tls-server-name: k3s.clonk.tv`.
It never prints certificate or key data. It needs no prompt and no TTY, so an
agent can run `start`, and a later separate command can run plain
`kubectl --context gardenio-ssm ...`; the session outlives the command that
started it. Knobs (port, context, paths) are listed in the script header.

`GARDENIO_SSM_VIA` picks the path: `server` (the forward above), `edge`, or
`auto` (the default: server first, then edge if 127.0.0.1:16443 does not answer
within `GARDENIO_SSM_WAIT_SECS`). `edge` opens the session on the running
instance tagged `Name=gardenio-k3s-ziti-edge` with
`AWS-StartPortForwardingSessionToRemoteHost
--parameters host=<server private IP>,portNumber=6443,...`. The edge keeps a
public address and the IGW route and shares the `nodes` SG with the servers, so
this path survives a NAT outage that takes the servers' own SSM agents offline.
The written context is the same either way.
The gardenio kubeconfig authenticates with a client certificate, so nothing
secret is passed as an argument. If the source kubeconfig used a bearer token
instead, `start` would hand it to `kubectl config set-credentials --token`,
which is visible to `ps` for the instant it runs.

Self-test: `bash scripts/k3s/kubectl-ssm_test.sh` (stubs `aws`; uses the real
`kubectl` for the kubeconfig cases and says SKIP when it is absent).

### Break-glass: shell on the node

When even the forward cannot be used (or the API itself is down), skip kubectl
entirely and go onto the server:

```sh
aws ssm start-session --target <server instance id> --profile gardenio --region us-east-1
sudo k3s kubectl get nodes        # on the node
```

Find the id with `aws ec2 describe-instances --profile gardenio --region us-east-1
--filters Name=tag:k3s:role,Values=server Name=instance-state-name,Values=running
--query 'Reservations[].Instances[].InstanceId' --output text`.
This does not depend on the edge, Keycloak or ziti.
