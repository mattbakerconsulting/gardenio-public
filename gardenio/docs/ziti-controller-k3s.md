# The OpenZiti controller: from Vultr quickstart to k3s

> **Opt-in only.** This applies only when the ziti overlay is on
> (`ziti.enabled=true`, `gardenio/helm/ziti`, off by default since
> gd-xNMLrsgajy). The Vultr controller it describes is kept running, unchanged,
> by owner decision 2026-10-13, and nothing on gardenio's default path dials
> it: see [`../helm/ziti/README.md`](../helm/ziti/README.md) "Where the ziti
> controller lives".

Written 2026-09-19, when `gardenio/helm/ziti/controller.yaml` (now `templates/controller.yaml` of the opt-in ziti chart, off by default; see `gardenio/helm/ziti/README.md`) was added.
Companion to `ziti-routers.md` (which covers the routers already running
this way) and `plans/ziti-access.md` (the original, pre-quickstart plan for
self-hosting the fabric).

## What this replaces

The live controller today is one process on a Vultr box, started as:

```
ziti edge quickstart --home /home/claude/ziti --ctrl-address 45-77-68-134.nip.io \
  --ctrl-port 8440 --router-address ... -u admin -p <PASSWORD IN ARGV>
```

`ziti edge quickstart` is a demo/dev harness: it stands up a controller and a
router in a single process from CLI flags. Two problems with running it as
the fleet's production fabric:

- **The admin password is a CLI argument** — visible to anything that can
  read the process table on that box (`ps auxww`, `/proc/<pid>/cmdline`).
- **Controller and router are not separable.** Every other router in this
  fleet is its own deployment (`router-aws-1`, `ziti-routers.md`); the
  Vultr box conflates the two, which is also documented as the fleet's
  single point of failure.

## What replaces it

`gardenio/helm/ziti/templates/controller.yaml` runs the official `openziti/ziti-controller`
image (not the community Helm chart — same "plain YAML, house style" call
`gardenio/helm/gardenio-ziti/README.md` already made) in its documented
bootstrap mode: `ZITI_BOOTSTRAP_CONFIG=true` generates the controller's config
from environment variables instead of a `-p` flag, and the admin password
comes in over `ZITI_PWD`, sourced from a Kubernetes Secret. An env var is
still readable via `/proc/<pid>/environ` by the same audience that can already
read a mounted Secret in that pod — this is not "the password is now
inaccessible", it's "the password is no longer visible to `ps`/anything
scanning argv on a shared box", which is the actual finding (title of
gd-1hMSYnoW1d).

This is a **fresh, independent ziti network** — new PKI, new admin identity —
running **alongside** the Vultr controller, not clustered with it and not a
migration-in-place. Nothing points at it yet:

- None of the eight enrolled identities (`ziti-access-broker-integration.md`,
  `helm/ziti/README.md`'s "nine ziti services" table) have been
  re-enrolled against it. That is Phase 2 of `gd-Vn8uWTbQ9S` and touches all
  eight, per Bite 1 in `01-why-and-what-bites.md`.
- No router is attached to it yet. `router-aws-1` still points at the Vultr
  controller (`ZITI_CTRL_ADVERTISED_ADDRESS: 45-77-68-134.nip.io` in its
  manifest); which router(s) end up on the new controller is the separate
  router-topology decision (`gd-aOJJVZtJCV`).
- The Vultr controller keeps running, untouched, until this one is proven —
  Bite 2's mitigation is "two controllers briefly, not a flag day."

## What is deliberately not here yet

Two follow-on tasks against the same epic (`gd-i5eu4YiVVq`) build on top of
this deployment rather than being folded into it, so three crews were not
scaffolding the same not-yet-real manifests in parallel:

- **Durable storage** (`gd-fY6ipET226`, landed on the SECOND attempt — see
  "Durable storage: applied, reverted, fixed" below). This k3s cluster has no
  EBS CSI driver, only the built-in `local-path` provisioner, and a
  `local-path` PVC does not actually survive node replacement even with
  node-class pinning: the PV it binds gets a nodeAffinity for whichever node
  it first lands on, and when that node is later replaced (instance refresh,
  AZ event) the PVC stays Bound to a PV naming a node that no longer exists —
  the pod is then Pending forever, with no in-pod way to un-stick it.
  So `controller.yaml` holds no PVC. `data` is an `emptyDir` (node-local,
  gone on every pod restart by design, which is what sidesteps the stuck-PVC
  deadlock — a fresh emptyDir always binds). Durability instead comes from
  two sidecars alongside the `controller` container plus an initContainer:
  `ziti-snapshot` calls `ziti edge db snapshot` over `localhost:8440` every
  15 minutes (the same in-pod loopback trick as the exec command above,
  no cert-SAN workaround needed); `s3-backup` tars, encrypts, and uploads
  the whole `ZITI_HOME` to S3 on the same cadence, reusing the k3s node
  role's existing IAM grant (`terraform/modules/k3s`
  `extra_backup_prefixes = ["ziti-ctrl"]`, commit `bd49105`, so no
  terraform change was needed for this file) — see
  `gardenio/docs/ziti-controller-backup.md`; and `restore-if-empty` restores
  a PINNED backup (never "latest" — see below) and promotes its newest
  consistent snapshot member whenever `ZITI_HOME` is empty, which after this
  change is every pod (re)start. RPO is one ~15-minute backup interval on
  every restart, not zero — the deliberate trade for a design with no
  stuck-PVC failure mode and no EBS-CSI/terraform dependency. Node-class
  pinning (`gardenio.io/lifecycle=on-demand`, the same label the beads Dolt
  StatefulSet uses — see `gardenio/helm/beads/dolt-statefulset.yaml` and
  `beads-on-k3s.md`) is kept, but is no longer load-bearing for
  correctness — it now only avoids the RPO-window churn of routine spot
  reclaims.

  ### Durable storage: applied, reverted, fixed

  The first version of this design (PR #7, commit `c616795`) was applied to
  the live cluster and reverted (commit `0f158ff`) the same day. Worth
  recording so the next durability change here doesn't repeat it:

  1. **The blocking defect**: `s3-backup` and `restore-if-empty` ran on
     plain `amazon/aws-cli:2.36.48` (the image `helm/beads/dolt-backup-cronjob.yaml`
     already uses). That image has no `tar`, `gzip` or `openssl`. `s3-backup`
     crash-looped `"tar: command not found"` and never took a single backup;
     `restore-if-empty` would have failed identically the first time it had
     something to restore. Fixed by a purpose-built image,
     `gardenio/images/ziti-ctrl-backup` (this repo's own CI builds and
     pushes it), which `yum install`s the three tools at image-BUILD time —
     deliberately not at pod-boot time, which would put a package mirror on
     the controller's critical restart path.
  2. **Wrong S3 prefix**: the sidecars used `k3s-dolt/ziti-ctrl`; the
     terraform grant and `gardenio/scripts/ziti-ctrl-backup` both use
     `ziti-ctrl`. On its own this sent every restore down the
     no-backup-found path and would have minted a fresh PKI silently.
  3. **Restore integrity wasn't actually enforced**: `restore-if-empty` took
     whichever object sorted last under the prefix and checked it against a
     `.sha256` object fetched from that SAME prefix. This cluster has no
     IRSA — the node role (which grants `PutObject`/`DeleteObject` on this
     prefix) is inherited by every pod that can reach IMDS, so whoever could
     write the backup could write its digest too. The check proved nothing
     against the actual threat model.

  All three are fixed in the current file. Fix 3 in particular changed the
  restore contract: `restore-if-empty` now only restores a PINNED backup
  (`RESTORE_S3_KEY` + `RESTORE_S3_VERSION_ID` + `RESTORE_CIPHERTEXT_SHA256`,
  set as Deployment env vars — git, not S3, is what the node role cannot
  write), verifies the ciphertext's sha256 against the pin BEFORE decrypting,
  and fails closed (pod never comes up) if nothing is pinned and
  `ALLOW_FRESH_PKI` isn't explicitly `"true"`. See the comment block at the
  top of `controller.yaml` for the exact mechanics and the operator steps to
  advance the pin.
- **Public reachability** (`gd-mClQbiOtPk`). The Service here is
  `ClusterIP`-only — no Ingress, no `hostNetwork`, no hook into the
  `k3s-dns` Route53 pattern (`modules/k3s/templates/node.sh.tftpl:23-64`)
  that `ziti.clonk.tv` and `k3s.clonk.tv` already use. The advertised
  address is still set to the real, decided name (`ziti-ctrl.clonk.tv`,
  `gd-zU3VrMUTpo`) so the bootstrap PKI's cert SAN is already correct and
  never needs regenerating once a public listener exists.

## Deploying

Namespace `ziti` already exists (created by `router-aws-1.yaml`). Two secrets
must exist before the controller's first boot: the admin password (that boot
creates the default admin user) and the backup passphrase (`restore-if-empty`
and `s3-backup` both fail closed without it):

```sh
kubectl -n ziti create secret generic ziti-controller-admin \
  --from-literal=password="$(openssl rand -hex 24)"

kubectl -n ziti create secret generic ziti-ctrl-backup-passphrase \
  --from-literal=passphrase="$(openssl rand -base64 32)"

# ziti is an opt-in chart, off by default (gardenio/helm/ziti/README.md):
helm upgrade --install gardenio-ziti gardenio/helm/ziti -n default \
  --set ziti.enabled=true,ziti.routerAws.enabled=false,ziti.routerPublic.enabled=false,ziti.corednsSplitHorizon.enabled=false,ziti.hostGardenio.enabled=false
```

The manifest as committed has `ALLOW_FRESH_PKI: "true"` on `restore-if-empty`
for exactly this first-ever boot (no backup exists to restore yet). After the
pod is Ready and the first `s3-backup` upload has landed (~2-15 minutes,
`kubectl -n ziti logs deploy/ziti-controller -c s3-backup`), pin that backup
as the restore point and flip `ALLOW_FRESH_PKI` back to `"false"` — see the
"Restore is pinned, not latest" comment block at the top of `controller.yaml`
for the three env vars to set and why a *future* restart must not silently
fall back to a fresh PKI.

Then, from a pod/shell that can reach the cluster network (the `ziti-ctrl.clonk.tv`
name resolves nowhere yet — see above):

```sh
kubectl -n ziti get pods -w                       # wait for Ready
kubectl -n ziti exec deploy/ziti-controller -- ziti agent stats
```

Logging in over the CLI needs a `--tls-server-name`/hosts-file workaround
until the public listener exists, because the cert SAN is
`ziti-ctrl.clonk.tv` but the only reachable name today is the Service's
ClusterIP or its in-cluster DNS name:

```sh
kubectl -n ziti exec -it deploy/ziti-controller -- \
  ziti edge login localhost:8440 -u admin -p "$(kubectl -n ziti get secret ziti-controller-admin -o jsonpath='{.data.password}' | base64 -d)" -y
```

(`-y` skips the interactive "trust this cert" prompt — acceptable here since
this is the controller's own freshly-bootstrapped self-signed CA, checked
from inside its own pod.)

## Gotchas

- **`ZITI_PWD` is read once, at first-boot database bootstrap.** After that,
  the config/PKI/database already exist and `ZITI_BOOTSTRAP_*` steps no-op
  (same "bootstrap is once-only by design" property `ziti-routers.md`
  documents for the router's enrollment). Rotating the admin password later
  is a `ziti update authenticator` call, not a Secret edit + restart.
- **This image always runs "clustered", even at one replica** — the upstream
  README's own wording. `ZITI_BOOTSTRAP_CLUSTER` defaults `true` and was left
  at that default; it only needs to be `false` when *joining* an existing
  cluster, which this is not.
- **No IP-SAN workaround needed here**, unlike the router
  (`ziti-routers.md`'s `ZITI_ROUTER_IP_OVERRIDE` gotcha) — the advertised
  address is a real DNS name (`ziti-ctrl.clonk.tv`) from the start, precisely
  because the address was decided (`gd-zU3VrMUTpo`) before this manifest was
  written rather than after.
- **Image version is pinned to `2.0.4`**, matching the fleet's existing ziti
  version (the Vultr controller and `router-aws-1` are both `v2.0.4`). This
  new controller isn't required to match the *old* one's version since the
  two aren't clustered, but whatever version it runs becomes the pin every
  future router attached to it must match (`ziti-routers.md`'s "keep the
  router image at the controller's version" rule) — 2.0.4 keeps that number
  from moving twice.

## Status

The controller itself has been live on the node-pinned-emptyDir-less shape
before (applied via PR #5/#6, `gd-1hMSYnoW1d`). The durable-storage change in
this file (`gd-fY6ipET226`) is on its SECOND attempt — the first was applied,
found broken, and reverted same-day (see "Durable storage: applied, reverted,
fixed" above); this version fixes the blocking defect and folds in every
review finding from that round. It is **written and pushed but not yet
applied to the live cluster** — this crew pod carries no AWS/kubectl
credentials to do so or to verify it live (same constraint noted on
`gd-hJzAV4kTTh`/`gd-rn7innedGW`/the first attempt).

Needs an operator (or a session with cluster credentials) to:

1. Confirm `gardenio/images/ziti-ctrl-backup` has built and pushed via this
   repo's `images` GitHub Actions workflow (triggers automatically once this
   change lands on `main`).
2. Run the "Deploying" steps above.
3. Confirm the pod goes Ready with 3/3 containers and `ziti agent stats`
   reports healthy.
4. After ~15-20 minutes, confirm a `s3-backup` upload landed under
   `s3://gardenio-hq-backups-123456789012/ziti-ctrl/` and pin it (see
   "Deploying" above), flipping `ALLOW_FRESH_PKI` back to `"false"`.
5. Ideally, force a pod delete/reschedule and confirm `restore-if-empty`'s
   logs show a real restore against the pin, not the fresh-PKI branch.
6. Report back on `gd-fY6ipET226` so the task can close.
