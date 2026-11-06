# Gardenio in a Box — EC2 bootstrap (Phase 0)

Scripted single-EC2 bootstrap for the "Gardenio in a Box" build (epic
[ka-0000007690]; synthesis `gardenio/docs/gardenio-in-a-box-synthesis.md` §4).
A fresh EC2 instance boots into a single-node **k3s** cluster with the gardenio
Helm chart installed via the BYO deploy path.

Source research:
- `gardenio/docs/research/inbox-ec2-sizing-bootstrap.md` (sizing + §5 bootstrap path)
- `gardenio/docs/research/inbox-k8s-distro-choice.md` (k3s + install sketch)

## Pieces

| File | Role |
|---|---|
| `../bootstrap-ec2.sh` | Runs **on** the instance (as root). OS prep → k3s → optional gVisor → optional host hardening → image import → `deploy-byo.sh`. Each stage idempotent. |
| `../k3s/install-host-hardening.sh` | Phase 3 P3.5 host-layer hardening (`--harden`): nftables default-drop to 22/80/443, SSH key-only + fail2ban (nft backend), IMDSv2 hop-limit=1. Runs **after** k3s so it only ADDS a table and never flushes k3s's. |
| `gardenio-inbox-user-data.yaml.tmpl` | `#cloud-config` user-data template. Stages-or-clones the repo, then invokes `bootstrap-ec2.sh`. A tarball-staged tree under `/opt/gardenio/tarboat/repo` (no `.git`) is linked to `/opt/gardenio/repo` and the private clone is skipped (Tarboat T1/G1.2). |
| `../render-user-data.sh` | Substitutes the `@@PLACEHOLDER@@` tokens, emitting a launch-ready user-data file. |

## Sizing scenarios

Workload is **RAM-bound** — every concurrent agent reserves a hard 16 GiB, so
use memory-optimized `r7i`. Run `scripts/bootstrap-ec2.sh --print-scenarios`.

| Scenario | Agents | Instance | vCPU / RAM | gp3 root |
|---|---|---|---|---|
| A | 1–2 | `r7i.2xlarge` | 8 / 64 GiB | 150 GiB |
| B | 4–6 | `r7i.4xlarge` | 16 / 128 GiB | 250 GiB |
| C | 8–12 | `r7i.8xlarge` | 32 / 256 GiB | 400 GiB |

## Launch

`gardenio` is **private**, so cloud-init needs the repo subset on the box (runbook §A.0 item 5).
Three options: **(a) Tarboat tarball** stages the tree under `/opt/gardenio/tarboat/repo` (no
`.git`) — cloud-init links it to `/opt/gardenio/repo` and **skips the clone entirely** (the true
air-gap default, no token, no GitLab access); **(b) AMI-bake / pre-stage** a checkout at
`/opt/gardenio/repo`; or **(c)** pass a read-only clone token via `GARDENIO_REPO_TOKEN` /
`--repo-token` (internet-connected clone — not air-gap). The clone short-circuits on the
sentinel `scripts/deploy-byo.sh`, so (a) and (b) need no credential. Override the staged
location with `--offline-assets DIR`.

```sh
# 1. Render user-data for scenario A (demo/eval), gVisor off.
#    Supply a read-only deploy-token/PAT for the private repo (omit only if the
#    repo is already staged on the box):
GARDENIO_REPO_TOKEN=glpat-xxxx scripts/render-user-data.sh --scenario A --repo-ref main > /tmp/user-data.yaml

# 2. Launch the instance with the scenario's instance type + gp3 root size:
aws ec2 run-instances --instance-type r7i.2xlarge \
  --block-device-mappings 'DeviceName=/dev/xvda,Ebs={VolumeSize=150,VolumeType=gp3}' \
  --user-data file:///tmp/user-data.yaml \
  --image-id <al2023-or-ubuntu-ami> --key-name <key> ...
```

Bootstrap progress lands in `/var/log/cloud-init-output.log` and
`/var/log/gardenio-bootstrap.log` on the instance.

## Run by hand (already-launched instance)

```sh
sudo scripts/bootstrap-ec2.sh --scenario A --repo-dir /path/to/gardenio
# add --gvisor to install the runsc sandbox runtime (Phase 0.3).
# add --harden to apply the Phase 3 host firewall + SSH + IMDSv2 hardening
#   (P3.5). Pair with --operator-user <name> (the single SSH account; its
#   authorized_keys must be injected at LAUNCH — never bake a shared key).
#   Render path: render-user-data.sh --harden 1 --operator-user <name>.
#   NOTE: --harden writes an SSH key-only, single-user drop-in — confirm the
#   operator account + its public key exist BEFORE enabling, or you lock
#   yourself out. Also set MetadataOptions HttpTokens=required +
#   HttpPutResponseHopLimit=1 at run-instances (IMDSv2 belt-and-suspenders).
```

## Caveats

- The default values overlay (`deploy-values/gardenio-byo.yaml`) runs
  keycloak / forwardauth / TLS **off** and exposes ClusterIP only. That meets
  Phase 0's goal ("it installs and runs") but is **not** public-internet-safe.
  Point `--values` at the public-safe overlay once Phase 3 (ka-0000005904) lands.
- **Single box = no HA.** Snapshot EBS / keep an AMI + user-data rebuild path.

[ka-0000007690]: https://beads-viewer.e2e.example.com/bead/ka-0000007690
