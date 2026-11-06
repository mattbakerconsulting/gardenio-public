# One beads database for everyone, on k3s, over ziti

Status: **built 2026-09-17** — Dolt 2.3.5 server in k3s namespace `beads`,
reachable as `beads.ziti:3306`, validated end to end with a throwaway city
on the HQ host (`gc beads city use-external`); the live city cutover is the
mayor's step. Operator doc: `docs/beads-on-k3s.md`. Manifests:
`helm/beads/`. Originally proposed 2026-09-17 (user: "get everything hooked
up to one beads database, ideally hosted in k3s and accessible via ziti, so
we can track work"). Owner: laptop-claude for k3s + ziti, the mayor for the
city cutover.

## What exists today

- Gas City stores beads in **Dolt**: each managed city runs its own
  `dolt sql-server` from a "dolt pack" (`.gc/runtime/packs/dolt/dolt-config.yaml`,
  loopback port, `data_dir <city>/.beads/dolt`). On Vultr, `gascity-test`
  has a 32 MB store, prefix `gt`, `bd 1.2.2`, `dolt 2.1.7` (2.3.5 current).
- `gc` already has the notion of a **canonical external Dolt endpoint**
  (`contract.DoltConnectionTarget`, `GC_DOLT_HOST/PORT/USER/PASSWORD`,
  `cmd/gc/bd_env.go`), documented in `docs/reference/internal/beads-topology.md`,
  `docs/runbooks/managed-city-endpoints.md` and
  `engdocs/design/beads-dolt-contract-redesign.md`. Step 0 below is to confirm
  from those docs that a managed city can be pointed at a server it does not
  own, with `dolt.auto-start: false` (already the case in
  `gascity-test/.beads/config.yaml`).
- The ziti fabric is on Vultr (`ziti-fabric` memory; `gc.dashboard.ziti` is
  the working template for a hosted service).
- gardenio's own `kbeads` (Postgres, kd/gd ids) is a *different* beads
  implementation. This plan is about Gas City's `bd`/Dolt, which is where the
  agents now live. What to do with kbeads is an open question at the end.

## Design

1. **Dolt server on k3s.** Namespace `beads`. A one-replica StatefulSet running
   `dolthub/dolt-sql-server` (pin the version `bd 1.2.2` supports; upgrade the
   Vultr city's 2.1.7 at cutover so both sides match), 20 GiB gp3 PVC, the
   same listener tuning the dolt pack uses (`read_timeout_millis 15000`,
   `max_connections 256`) because bd opens a short-lived connection per
   operation. Admin user/password in a k8s Secret, sourced from SSM
   `/gardenio/beads/dolt-admin`. Requests ~250 Mi / 0.25 CPU; fits the
   existing `t3.small` servers, no CI node needed.
2. **Spot and durability.** The PVC is EBS, so it is AZ-bound: `nodeAffinity`
   to that AZ. A spot reclaim there means the pod is down until the replacement
   server joins (measured 2m16s); bd clients wait `GC_DOLT_WAIT_TIMEOUT`.
   Nightly CronJob `dolt push` to an S3 remote (`s3://gardenio-beads-backup`)
   plus the daily EBS snapshot the cluster already takes. Restore = clone the
   remote.
3. **Reachable only over ziti.** No Service of type LoadBalancer, no ingress.
   A `ziti-edge-tunnel` Deployment in host mode with identity `k3s-beads-host`
   (role attr `beads-hosts`) binds ziti service `beads` with
   `host.v1 → dolt.beads.svc.cluster.local:3306` and
   `intercept.v1 → beads.ziti:3306`. Policies: `beads-bind` for
   `#beads-hosts`, `beads-dial` for `#gc-clients`. The identities already
   enrolled (laptop WSL, laptop Windows, phone) get access for free; the Vultr
   box needs a dialing tunneler too — either switch `router-instance-1`'s
   tunnel binding from `host` to `tproxy` or run `ziti-edge-tunnel` there with
   an identity `vultr-hq`. That is the mayor's box; coordinate by mail.
4. **Cut the city over.** Migrate `gascity-test`'s store into the server
   (`dolt backup` → `dolt clone` into the PVC, or `dolt push` from Vultr to the
   S3 remote and pull), set `GC_DOLT_HOST=beads.ziti GC_DOLT_PORT=3306` plus
   user/password for the city, restart the supervisor (the mayor knows the
   session-preserving restart), verify `gc bd list` and the dashboard still
   work, then stop the local dolt pack. Keep the Vultr copy read-only for a
   week before deleting it.
5. **One database vs one server.** Start with one Dolt *server*, one database
   per city (`gascity_test`, later `gascity_hq` …) so prefixes cannot collide
   and each city's `bd` sees only its own. If cross-city tracking needs one
   *database*, verify first that `bd` tolerates multiple issue prefixes in a
   single store; otherwise cross-city views belong in the dashboard, not the
   store.

## Cost

| Item | Estimate |
|---|---|
| 20 GiB gp3 PVC | ~$1.60/mo |
| Pods (dolt, tunneler, backup cron) on existing nodes | $0 |
| S3 backup, ~100 MB | ~$0.01/mo |

## Steps

0. **Answered 2026-09-17 (assessment map, `hq/assessment-map.md` §4):** a
   managed city can use a Dolt server it does not own:
   `gc beads city use-external --host <h> --port <p>` (and
   `gc rig set-endpoint --external`), with a verification contract (project
   identity must match, `issues` table must exist, `--adopt-unverified` to
   skip). Also learned: `[maintenance.dolt]` is observe-only, `gc doctor`
   strips off-box remotes, and the recovery commands the Accepted design
   names (`gc beads endpoint repair`, `city recover`, `resume --op`) are not
   implemented — so the S3 push CronJob in step 1 is the only backup and is
   not optional.
1. Terraform: S3 backup bucket, SSM secret. Helm/manifests under
   `gardenio/helm/beads/`: StatefulSet, Service, Secret, CronJob.
2. Ziti: create identity `k3s-beads-host` and service/policies on the Vultr
   controller; deploy the tunneler in k3s with the enrolled identity as a
   Secret. Test from the laptop: `mysql -h beads.ziti -P 3306`.
3. Vultr dial path (mayor): tproxy on the router or a tunneler identity.
   Test from Vultr the same way.
4. Migrate and cut over `gascity-test` (mayor drives, laptop verifies from
   outside).
5. New cities (`gascity-hq` work) are created pointing at `beads.ziti` from
   day one.

## Open questions

- **gardenio kbeads.** The controller's `TestRealCatalogSnapshotFreshness`
  and the agent image assume the kbeads daemon. Either gardenio's controller
  learns to read Gas City beads, or kbeads stays a separate store for the
  gardenio harness only. Decide once the Gas City control plane on k3s is
  planned; do not run two "one databases".
- Dolt replication (a read replica in a second AZ) would remove the
  spot-reclaim outage; skip until it actually hurts.
- Multi-tenant auth: one Dolt user per city vs one shared admin. Start
  shared; ziti already gates who can reach the port.
